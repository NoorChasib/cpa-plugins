package canon

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// errNoHead is PieceRoles' answer to a nil head.
var errNoHead = errors.New("canon: PieceRoles needs a head function (the archive_mcp marker and reminder rules read piece content)")

// Role is a piece's role: the server's piece.kind.
type Role string

// Piece roles.
const (
	RoleSystem      Role = "system"
	RoleToolSchema  Role = "tool_schema"
	RoleParams      Role = "params"
	RoleReminder    Role = "reminder"
	RoleArchiveMCP  Role = "archive_mcp"
	RoleThinking    Role = "thinking"
	RoleToolInput   Role = "tool_input"
	RoleToolResult  Role = "tool_result"
	RoleFailureBody Role = "failure_body"
	RoleText        Role = "text"
)

// IdxPolicy is the server's piece.idx_policy.
type IdxPolicy string

// Index policies.
const (
	IdxNone     IdxPolicy = "none"
	IdxFull     IdxPolicy = "full"
	IdxHeadTail IdxPolicy = "headtail"
)

// IdxPolicy returns the index policy of r: none for system, tool_schema,
// params, reminder and archive_mcp; full for thinking, tool_input and text; headtail for tool_result
// and failure_body.
func (r Role) IdxPolicy() IdxPolicy {
	switch r {
	case RoleThinking, RoleToolInput, RoleText:
		return IdxFull
	case RoleToolResult, RoleFailureBody:
		return IdxHeadTail
	}
	return IdxNone
}

// Archive MCP result detection.
const (
	// ArchiveMCPMarker starts the text of every archive MCP tool result and tool error.
	ArchiveMCPMarker = "[chat-archive-mcp/v1]"
	// ArchiveMCPMarkerWindow: the marker counts when it appears in a payload's first this many bytes.
	ArchiveMCPMarkerWindow = 512
	// DefaultArchiveToolPrefix is the default of ARCHIVE_MCP_TOOL_PREFIXES.
	DefaultArchiveToolPrefix = "mcp__chat-archive__"
	// ReminderPrefix marks a reminder text payload in a user message.
	ReminderPrefix = "<system-reminder>"
)

// PieceRoles assigns a Role to every json and text piece hash one envelope references;
// attachments get no role. kind and body are Envelope.Kind and Envelope.Body.
// toolPrefixes are the archive MCP tool-name prefixes (ARCHIVE_MCP_TOOL_PREFIXES, default
// DefaultArchiveToolPrefix): a tool-result payload paired in the same envelope with a tool whose name
// starts with one of them is archive_mcp. head is required (nil is an error, never a silent skip of
// the marker and reminder rules, which would index the archive's own MCP results): it returns at least
// the first ArchiveMCPMarkerWindow bytes of every json and text piece the envelope references (all of
// it when shorter), from the commit batch or the store; ok=false means the piece is unknown, and only
// then are the two content rules skipped for it. The server calls this at ingest; first sight of a hash
// wins. Returns an error wrapping wire.ErrSchema when body does not decode for kind.
//
// Item payloads get their role from the block type of their frame and the role of their message, per
// Body.Format (import.record: claude-code uses the Claude rules, codex the Responses rules). A payload
// of a system or developer message that the block rules would index as text is system: chat and
// Responses keep those messages as ordinary items, and a Claude Code api-request-blob record carries
// its system prompt this way. The payload of a Responses item of type additional_tools (Codex sends
// tool schemas this way, in requests and in rollouts) is tool_schema. An import.record path item
// (frame {"path": pointer}) whose pointer starts with /attachment/systemPrompt/, /shape/system/ or
// /payload/base_instructions/ is system, and one under /attachment/tools/ or /shape/tools/ is
// tool_schema: prompt_snapshot attachments and api-request-shape records move each system prompt and
// tool schema entry into its own piece, and a Codex session_meta moves its long base instructions
// (the Codex system prompt) into one. ImportRecord.Extra is params; ImportFile.Content is tool_result
// for kind tool_result_file, else text. ImportFile.Content may be an attachment, which the body alone
// cannot tell: the server applies roles only to the json and text pieces it stores.
func PieceRoles(kind wire.Kind, body json.RawMessage, toolPrefixes []string, head func(wire.Hash) ([]byte, bool)) (map[wire.Hash]Role, error) {
	if head == nil {
		return nil, errNoHead
	}
	r := roler{roles: make(map[wire.Hash]Role), prefixes: toolPrefixes, head: head}
	switch kind {
	case wire.KindExchangeBegin, wire.KindExchangeResponse:
		var v struct {
			Body *roleBody `json:"body"`
		}
		if err := decodeRoleBody(kind, body, &v); err != nil {
			return nil, err
		}
		if v.Body != nil {
			r.set(v.Body.Skeleton, RoleParams)
			r.set(v.Body.Tools, RoleToolSchema)
			for _, it := range v.Body.System {
				r.set(it.P, RoleSystem)
			}
			r.items(v.Body.Format, v.Body.Items, false)
		}
	case wire.KindImportRecord:
		var v struct {
			Client string     `json:"client"`
			Items  []roleItem `json:"items"`
			Extra  *roleHash  `json:"extra"`
		}
		if err := decodeRoleBody(kind, body, &v); err != nil {
			return nil, err
		}
		format := ""
		switch v.Client {
		case "claude-code":
			format = FormatClaude
		case "codex":
			format = FormatOpenAIResponses
		}
		r.items(format, v.Items, true)
		r.set(v.Extra, RoleParams)
	case wire.KindImportFile:
		var v struct {
			Kind    string    `json:"kind"`
			Content *roleHash `json:"content"`
		}
		if err := decodeRoleBody(kind, body, &v); err != nil {
			return nil, err
		}
		role := RoleText
		if v.Kind == "tool_result_file" {
			role = RoleToolResult
		}
		r.set(v.Content, role)
	case wire.KindUsage:
		var v struct {
			FailureBody *roleHash `json:"failure_body"`
		}
		if err := decodeRoleBody(kind, body, &v); err != nil {
			return nil, err
		}
		r.set(v.FailureBody, RoleFailureBody)
	case wire.KindExchangeAttempt, wire.KindExchangeDone:
		var v struct{}
		if err := decodeRoleBody(kind, body, &v); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w: unknown kind %q", wire.ErrSchema, kind)
	}
	return r.roles, nil
}

// roleHash is a piece hash in an envelope body: exactly 64 lowercase hex characters. PieceRoles reads
// only the fields it needs, so it does not depend on the full wire decoder.
type roleHash wire.Hash

func (h *roleHash) UnmarshalText(b []byte) error {
	if len(b) != 64 || bytes.ContainsFunc(b, func(r rune) bool { return !('0' <= r && r <= '9' || 'a' <= r && r <= 'f') }) {
		return fmt.Errorf("invalid piece hash")
	}
	_, err := hex.Decode(h[:], b)
	return err
}

type roleItem struct {
	M json.RawMessage `json:"m"`
	F json.RawMessage `json:"f"`
	P *roleHash       `json:"p"`
}

type roleBody struct {
	Format   string     `json:"format"`
	Skeleton *roleHash  `json:"skeleton"`
	Tools    *roleHash  `json:"tools"`
	System   []roleItem `json:"system"`
	Items    []roleItem `json:"items"`
}

func decodeRoleBody(kind wire.Kind, body json.RawMessage, v any) error {
	if t := bytes.TrimLeft(body, " \t\r\n"); len(t) == 0 || t[0] != '{' {
		return fmt.Errorf("%w: %s body must be a JSON object", wire.ErrSchema, kind)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%w: %s body: %v", wire.ErrSchema, kind, err)
	}
	return nil
}

type roler struct {
	roles    map[wire.Hash]Role
	prefixes []string
	head     func(wire.Hash) ([]byte, bool)
}

// set records role for h unless h already has one (first sight wins).
func (r *roler) set(h *roleHash, role Role) {
	if h == nil {
		return
	}
	if _, ok := r.roles[wire.Hash(*h)]; !ok {
		r.roles[wire.Hash(*h)] = role
	}
}

// frameInfo is what the role rules read from an item's M or F frame.
type frameInfo struct {
	typ, role, id, name, callID, toolUseID, toolCallID, fnName string
	path                                                       string // an import.record path item's pointer
	thought                                                    bool
	function                                                   bool
}

// recordPathRoles are the import.record path item pointer prefixes whose pieces are system prompts
// or tool schemas.
var recordPathRoles = []struct {
	prefix string
	role   Role
}{
	{"/attachment/systemPrompt/", RoleSystem},
	{"/shape/system/", RoleSystem},
	{"/payload/base_instructions/", RoleSystem},
	{"/attachment/tools/", RoleToolSchema},
	{"/shape/tools/", RoleToolSchema},
}

// pathRole returns the role a path item's pointer gives its piece, if any.
func pathRole(ptr string) (Role, bool) {
	for _, p := range recordPathRoles {
		if strings.HasPrefix(ptr, p.prefix) {
			return p.role, true
		}
	}
	return "", false
}

func readFrame(raw json.RawMessage) frameInfo {
	var fi frameInfo
	if len(raw) == 0 {
		return fi
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return fi
	}
	str := func(k string) string {
		var s string
		if v, ok := m[k]; ok && json.Unmarshal(v, &s) == nil {
			return s
		}
		return ""
	}
	fi.typ, fi.role, fi.id, fi.name = str("type"), str("role"), str("id"), str("name")
	fi.callID, fi.toolUseID, fi.toolCallID = str("call_id"), str("tool_use_id"), str("tool_call_id")
	fi.path = str("path")
	if v, ok := m["thought"]; ok {
		fi.thought = string(v) == "true"
	}
	if v, ok := m["function"]; ok {
		fi.function = true
		var fn struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(v, &fn) == nil {
			fi.fnName = fn.Name
		}
	}
	return fi
}

// items assigns the roles of the payload pieces of one item sequence; record is true for an
// import.record body, whose path items carry pieces moved out of the record's frame.
func (r *roler) items(format string, items []roleItem, record bool) {
	frames := make([]frameInfo, len(items))
	names := make(map[string]string) // tool call id -> tool name, from this envelope only
	for i, it := range items {
		if len(it.M) > 0 {
			frames[i] = readFrame(it.M)
			continue
		}
		fi := readFrame(it.F)
		frames[i] = fi
		switch format {
		case FormatClaude:
			switch fi.typ {
			case "tool_use", "server_tool_use", "mcp_tool_use":
				names[fi.id] = fi.name
			}
		case FormatOpenAIChat:
			if fi.function && fi.id != "" {
				names[fi.id] = fi.fnName
			}
		case FormatOpenAIResponses:
			if fi.typ == "function_call" || fi.typ == "custom_tool_call" {
				names[fi.callID] = fi.name
			}
		}
	}
	var msg frameInfo // the message the following blocks belong to
	for i, it := range items {
		if len(it.M) > 0 {
			msg = frames[i]
			continue
		}
		if it.P == nil {
			continue
		}
		pathItem := record && frames[i].path != ""
		if pathItem {
			if role, ok := pathRole(frames[i].path); ok {
				r.set(it.P, role)
				continue
			}
		}
		role, pairID := blockRole(format, frames[i], msg)
		switch role {
		case RoleToolResult:
			if r.hasMarker(it.P) || r.archiveTool(names, pairID) {
				role = RoleArchiveMCP
			}
		case RoleText:
			switch {
			case !pathItem && (msg.role == "system" || msg.role == "developer"):
				role = RoleSystem
			case msg.role == "user" && r.startsWith(it.P, ReminderPrefix):
				role = RoleReminder
			}
		}
		r.set(it.P, role)
	}
}

// blockRole returns the role of a block's payload and, for tool results, the id of the tool call it
// answers.
func blockRole(format string, f, msg frameInfo) (Role, string) {
	switch format {
	case FormatClaude:
		switch f.typ {
		case "thinking", "compaction":
			return RoleThinking, ""
		case "tool_use", "server_tool_use", "mcp_tool_use":
			return RoleToolInput, ""
		case "tool_result", "mcp_tool_result":
			return RoleToolResult, f.toolUseID
		}
		if strings.HasSuffix(f.typ, "_tool_result") {
			return RoleToolResult, f.toolUseID
		}
	case FormatOpenAIChat:
		switch {
		case f.typ == "reasoning_content" || f.typ == "reasoning" || f.typ == "reasoning_details":
			return RoleThinking, ""
		case f.function:
			return RoleToolInput, ""
		case msg.role == "tool":
			return RoleToolResult, msg.toolCallID
		}
	case FormatOpenAIResponses:
		switch {
		case f.typ == "reasoning":
			return RoleThinking, ""
		case f.typ == "function_call" || f.typ == "custom_tool_call":
			return RoleToolInput, ""
		case strings.HasSuffix(f.typ, "_call_output"):
			return RoleToolResult, f.callID
		case f.typ == "additional_tools":
			return RoleToolSchema, ""
		}
	case FormatGemini:
		switch f.typ {
		case "":
			if f.thought {
				return RoleThinking, ""
			}
		case "functionCall", "function_call", "executableCode", "executable_code":
			return RoleToolInput, ""
		case "functionResponse", "function_response", "codeExecutionResult", "code_execution_result":
			return RoleToolResult, ""
		}
	}
	return RoleText, ""
}

func (r *roler) archiveTool(names map[string]string, id string) bool {
	if id == "" {
		return false
	}
	name, ok := names[id]
	if !ok {
		return false
	}
	prefixes := r.prefixes
	if len(prefixes) == 0 {
		prefixes = []string{DefaultArchiveToolPrefix}
	}
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func (r *roler) pieceHead(h *roleHash) ([]byte, bool) {
	b, ok := r.head(wire.Hash(*h))
	if !ok {
		return nil, false
	}
	if len(b) > ArchiveMCPMarkerWindow {
		b = b[:ArchiveMCPMarkerWindow]
	}
	return b, true
}

// hasMarker reports whether the piece's first ArchiveMCPMarkerWindow bytes contain ArchiveMCPMarker.
func (r *roler) hasMarker(h *roleHash) bool {
	b, ok := r.pieceHead(h)
	return ok && bytes.Contains(b, []byte(ArchiveMCPMarker))
}

func (r *roler) startsWith(h *roleHash, prefix string) bool {
	b, ok := r.pieceHead(h)
	return ok && bytes.HasPrefix(b, []byte(prefix))
}

// IsClaudeCode reports whether a request is from Claude Code: an x-claude-code-session-id header, or a
// user-agent starting with "claude-cli/". It takes the filtered ExchangeBegin.Headers the
// server holds (names lowercase, values masked).
func IsClaudeCode(h []wire.Header) bool {
	for _, x := range h {
		switch strings.ToLower(x.Name) {
		case "x-claude-code-session-id":
			return true
		case "user-agent":
			for _, v := range x.Values {
				if strings.HasPrefix(v, "claude-cli/") {
					return true
				}
			}
		}
	}
	return false
}
