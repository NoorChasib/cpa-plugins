package canon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// pieceStore is the server-side piece lookup PieceRoles' head callback reads.
type pieceStore map[wire.Hash][]byte

func (ps pieceStore) add(sp *Split) {
	for _, p := range sp.Pieces {
		ps[p.Hash] = p.Bytes
	}
}

func (ps pieceStore) head(h wire.Hash) ([]byte, bool) {
	b, ok := ps[h]
	return b, ok
}

func beginBody(t testing.TB, body wire.Body) json.RawMessage {
	t.Helper()
	b, err := Marshal(wire.ExchangeBegin{RequestID: "req-synth", ObservedAt: time.Unix(0, 0).UTC(),
		SourceFormat: "claude", Kind: "chat", Transport: "http", Headers: []wire.Header{}, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rolesDump lists "role idx_policy <quoted piece head>" lines, sorted, for goldens.
func rolesDump(roles map[wire.Hash]Role, ps pieceStore) []byte {
	var lines []string
	for h, r := range roles {
		head := ps[h]
		if len(head) > 60 {
			head = head[:60]
		}
		lines = append(lines, fmt.Sprintf("%-12s %-8s %s", r, r.IdxPolicy(), strconv.Quote(string(head))))
	}
	slices.Sort(lines)
	return []byte(strings.Join(lines, "\n") + "\n")
}

func roleOf(t testing.TB, roles map[wire.Hash]Role, ps pieceStore, content string) Role {
	t.Helper()
	for h, r := range roles {
		if string(ps[h]) == content {
			return r
		}
	}
	t.Fatalf("no role for piece %q", content)
	return ""
}

const marker = ArchiveMCPMarker + "\n"

func claudeTurn(msgs string) Input {
	return Input{Format: FormatClaude, Body: []byte(`{"model":"claude-test-model","system":"Synthetic system prompt.",` +
		`"tools":[{"name":"Bash"},{"name":"mcp__chat-archive__search"}],"messages":[` + msgs + `]}`)}
}

// TestPieceRolesClaude covers every role an exchange.begin can carry, archive MCP pairing in the same
// envelope, the marker, and the 512-byte window.
func TestPieceRolesClaude(t *testing.T) {
	e := newEnv(t)
	far := strings.Repeat("x", ArchiveMCPMarkerWindow) + ArchiveMCPMarker
	msgs := `{"role":"user","content":[{"type":"text","text":"Find my notes."},{"type":"text","text":"<system-reminder>synthetic</system-reminder>"}]},
	{"role":"assistant","content":[{"type":"thinking","thinking":"Search the archive.","signature":"c2ln"},
	  {"type":"tool_use","id":"toolu_a1","name":"mcp__chat-archive__search","input":{"q":"notes"}},
	  {"type":"tool_use","id":"toolu_b1","name":"Bash","input":{"command":"ls"}}]},
	{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a1","content":"paired archive result"},
	  {"type":"tool_result","tool_use_id":"toolu_b1","content":"ordinary bash output"},
	  {"type":"tool_result","tool_use_id":"toolu_zz","content":[{"type":"text","text":"` + strings.ReplaceAll(marker, "\n", `\n`) + `marker result"}]},
	  {"type":"tool_result","tool_use_id":"toolu_yy","content":"` + far + `"},
	  {"type":"text","text":"Synthetic system prompt."}]},
	{"role":"assistant","content":[{"type":"text","text":"<system-reminder>not a user message</system-reminder>"}]}`
	sp := e.request(claudeTurn(msgs), Options{})
	ps := pieceStore{}
	ps.add(sp)
	roles, err := PieceRoles(wire.KindExchangeBegin, beginBody(t, sp.Body()), nil, ps.head)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Role{
		"Synthetic system prompt.":                              RoleSystem, // also a user text block later: first sight wins
		"Find my notes.":                                        RoleText,
		"<system-reminder>synthetic</system-reminder>":          RoleReminder,
		"<system-reminder>not a user message</system-reminder>": RoleText,
		"Search the archive.":                                   RoleThinking,
		`{"q":"notes"}`:                                         RoleToolInput,
		`{"command":"ls"}`:                                      RoleToolInput,
		"paired archive result":                                 RoleArchiveMCP,
		"ordinary bash output":                                  RoleToolResult,
		far:                                                     RoleToolResult,
	}
	for content, role := range want {
		if got := roleOf(t, roles, ps, content); got != role {
			t.Errorf("%.40q: role %s, want %s", content, got, role)
		}
	}
	if roles[*sp.Skeleton] != RoleParams || roles[*sp.Tools] != RoleToolSchema {
		t.Error("skeleton/tools roles")
	}
	for h, r := range roles {
		if strings.HasPrefix(string(ps[h]), `[{"text":"[chat-archive-mcp/v1]`) && r != RoleArchiveMCP {
			t.Errorf("marker result has role %s", r)
		}
	}
	checkGolden(t, "roles/claude-begin.golden.txt", rolesDump(roles, ps))
	// Custom prefixes replace the default.
	roles, _ = PieceRoles(wire.KindExchangeBegin, beginBody(t, sp.Body()), []string{"mcp__other__"}, ps.head)
	if roleOf(t, roles, ps, "paired archive result") != RoleToolResult {
		t.Error("custom prefixes should replace the default")
	}
	// A nil head is an error, never a silent skip of the marker and reminder rules.
	if roles, err := PieceRoles(wire.KindExchangeBegin, beginBody(t, sp.Body()), nil, nil); err == nil || roles != nil {
		t.Errorf("nil head: roles %v err %v, want an error", roles, err)
	}
	// Pieces the head does not know skip only the two content rules; pairing still applies.
	roles, _ = PieceRoles(wire.KindExchangeBegin, beginBody(t, sp.Body()), nil, noHead)
	if len(roles) != len(sp.Pieces)-countAttachments(sp) {
		t.Errorf("%d roles for %d json/text pieces", len(roles), len(sp.Pieces)-countAttachments(sp))
	}
	for h, r := range roles {
		if r == RoleReminder || (r == RoleArchiveMCP && string(ps[h]) != "paired archive result") {
			t.Errorf("role %s without a head", r)
		}
	}
}

// noHead knows no piece.
func noHead(wire.Hash) ([]byte, bool) { return nil, false }

func countAttachments(sp *Split) int {
	n := 0
	for _, p := range sp.Pieces {
		if p.Kind == wire.PieceAttachment {
			n++
		}
	}
	return n
}

// TestArchiveMCPToolUseInPrefixDeltaBase is an archive_mcp golden: the result's tool_use sits in
// the kept prefix of the base, so this envelope cannot pair it; the content marker still makes it
// archive_mcp.
func TestArchiveMCPToolUseInPrefixDeltaBase(t *testing.T) {
	e := newEnv(t)
	turn1 := `{"role":"user","content":"Find my notes."},
	{"role":"assistant","content":[{"type":"tool_use","id":"toolu_a2","name":"mcp__chat-archive__search","input":{"q":"notes"}}]}`
	result := func(text string) string {
		return turn1 + `,{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a2","content":[{"type":"text","text":"` + text + `"}]}]}`
	}
	ps := pieceStore{}
	base := e.request(claudeTurn(turn1), Options{})
	ps.add(base)
	for _, c := range []struct {
		text string
		want Role
	}{{`[chat-archive-mcp/v1]\nsession 12: synthetic hit`, RoleArchiveMCP}, {`synthetic hit without marker`, RoleToolResult}} {
		sp := e.request(claudeTurn(result(c.text)), Options{})
		ps.add(sp)
		kept := LCP(ItemIDs(base.Items), ItemIDs(sp.Items))
		if kept != len(base.Items) {
			t.Fatalf("kept %d of %d", kept, len(base.Items))
		}
		body := sp.Body()
		body.Base = &wire.Base{Key: "ex/req-base/begin/0", Resp: -1, Kept: uint32(kept)}
		body.Items = body.Items[kept:]
		for _, it := range body.Items {
			if bytes.Contains(it.F, []byte("tool_use")) && !bytes.Contains(it.F, []byte("tool_use_id")) {
				t.Fatal("the tool_use must not be in the delta envelope")
			}
		}
		roles, err := PieceRoles(wire.KindExchangeBegin, beginBody(t, body), nil, ps.head)
		if err != nil {
			t.Fatal(err)
		}
		res := *body.Items[len(body.Items)-1].P
		if roles[res] != c.want {
			t.Errorf("%q: role %s, want %s", c.text, roles[res], c.want)
		}
		if c.want == RoleArchiveMCP {
			checkGolden(t, "roles/archive-mcp-prefix-delta.golden.txt", rolesDump(roles, ps))
		}
	}
}

// TestArchiveMCPSeparateTranscriptRecord is the other archive_mcp golden: in a transcript the
// tool_result is its own user record (import.record), apart from the assistant record holding the
// tool_use.
func TestArchiveMCPSeparateTranscriptRecord(t *testing.T) {
	e := newEnv(t)
	ps := pieceStore{}
	record := func(content string) json.RawMessage {
		sp := e.messages(FormatClaude, []byte(`[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a3","content":`+content+`}]}]`), Options{})
		ps.add(sp)
		extra := NewPiece(wire.PieceJSON, "", []byte(`{"interrupted":false}`))
		ps[extra.Hash] = extra.Bytes
		b, err := Marshal(wire.ImportRecord{Client: "claude-code", FileRole: "main", SessionID: "s-synth", RecordUUID: "u-synth",
			RecordType: "user", Mode: "full", Frame: json.RawMessage(`{"type":"user"}`), Items: sp.Items, Extra: &extra.Hash})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, c := range []struct {
		content string
		want    Role
	}{
		{`"[chat-archive-mcp/v1]\nsynthetic transcript hit"`, RoleArchiveMCP},
		{`[{"type":"text","text":"[chat-archive-mcp/v1]\nsynthetic list hit"}]`, RoleArchiveMCP},
		{`"ordinary synthetic output"`, RoleToolResult},
	} {
		body := record(c.content)
		roles, err := PieceRoles(wire.KindImportRecord, body, nil, ps.head)
		if err != nil {
			t.Fatal(err)
		}
		var r struct {
			Items []struct {
				P *wire.Hash `json:"p"`
			} `json:"items"`
			Extra wire.Hash `json:"extra"`
		}
		json.Unmarshal(body, &r)
		if got := roles[*r.Items[1].P]; got != c.want {
			t.Errorf("%s: role %s, want %s", c.content, got, c.want)
		}
		if roles[r.Extra] != RoleParams {
			t.Errorf("extra role %s", roles[r.Extra])
		}
		if c.want == RoleArchiveMCP && strings.HasPrefix(c.content, `"`) {
			checkGolden(t, "roles/archive-mcp-transcript-record.golden.txt", rolesDump(roles, ps))
		}
	}
}

func TestPieceRolesOtherFormats(t *testing.T) {
	e := newEnv(t)
	ps := pieceStore{}
	run := func(in Input, response bool) map[wire.Hash]Role {
		var sp *Split
		if response {
			sp = e.response(in, Options{})
		} else {
			sp = e.request(in, Options{})
		}
		ps.add(sp)
		kind := wire.KindExchangeBegin
		if response {
			kind = wire.KindExchangeResponse
		}
		roles, err := PieceRoles(kind, beginBody(t, sp.Body()), nil, ps.head)
		if err != nil {
			t.Fatal(err)
		}
		return roles
	}
	chat := run(Input{Format: FormatOpenAIChat, Body: []byte(`{"messages":[
	  {"role":"user","content":"<system-reminder>chat reminder</system-reminder>"},
	  {"role":"assistant","reasoning_content":"chat thinking","tool_calls":[{"id":"c1","type":"function","function":{"name":"mcp__chat-archive__get","arguments":"{\"id\":1}"}},{"id":"c2","type":"function","function":{"name":"shell","arguments":"{\"id\":2}"}}]},
	  {"role":"tool","tool_call_id":"c1","content":"chat archive result"},
	  {"role":"tool","tool_call_id":"c2","content":"chat shell result"}]}`)}, false)
	resp := run(Input{Format: FormatOpenAIResponses, Body: []byte(`{"instructions":"resp instructions","input":[
	  {"type":"reasoning","summary":[{"type":"summary_text","text":"resp thinking"}]},
	  {"type":"function_call","call_id":"f1","name":"mcp__chat-archive__get","arguments":"{\"id\":3}"},
	  {"type":"function_call_output","call_id":"f1","output":"resp archive result"},
	  {"type":"custom_tool_call_output","call_id":"f9","output":"[chat-archive-mcp/v1] resp marker result"},
	  {"type":"function_call_output","call_id":"f8","output":"resp shell result"},
	  {"type":"web_search_call","action":{"query":"resp other"}}]}`)}, false)
	gem := run(Input{Format: FormatGemini, Body: []byte(`{"candidates":[{"content":{"role":"model","parts":[
	  {"text":"gem thinking","thought":true},{"text":"gem text"},{"functionCall":{"name":"f","args":{"a":1}}},
	  {"functionResponse":{"name":"f","response":{"r":1}}},{"executableCode":{"code":"print(1)"}},{"codeExecutionResult":{"output":"1"}}]}}]}`)}, true)
	want := []struct {
		roles   map[wire.Hash]Role
		content string
		role    Role
	}{
		{chat, "<system-reminder>chat reminder</system-reminder>", RoleReminder},
		{chat, "chat thinking", RoleThinking},
		{chat, `"{\"id\":1}"`, RoleToolInput},
		{chat, "chat archive result", RoleArchiveMCP},
		{chat, "chat shell result", RoleToolResult},
		{resp, "resp instructions", RoleSystem},
		{resp, `{"summary":[{"text":"resp thinking","type":"summary_text"}]}`, RoleThinking},
		{resp, `"{\"id\":3}"`, RoleToolInput},
		{resp, "resp archive result", RoleArchiveMCP},
		{resp, "[chat-archive-mcp/v1] resp marker result", RoleArchiveMCP},
		{resp, "resp shell result", RoleToolResult},
		{resp, `{"action":{"query":"resp other"}}`, RoleText},
		{gem, "gem thinking", RoleThinking},
		{gem, "gem text", RoleText},
		{gem, `{"args":{"a":1},"name":"f"}`, RoleToolInput},
		{gem, `{"name":"f","response":{"r":1}}`, RoleToolResult},
		{gem, `{"code":"print(1)"}`, RoleToolInput},
		{gem, `{"output":"1"}`, RoleToolResult},
	}
	for _, w := range want {
		if got := roleOf(t, w.roles, ps, w.content); got != w.role {
			t.Errorf("%q: %s, want %s", w.content, got, w.role)
		}
	}
	all := map[wire.Hash]Role{}
	for _, m := range []map[wire.Hash]Role{chat, resp, gem} {
		for h, r := range m {
			all[h] = r
		}
	}
	checkGolden(t, "roles/other-formats.golden.txt", rolesDump(all, ps))
}

// TestPieceRolesSystemAndDeveloperMessages: chat and Responses keep system and developer messages as
// ordinary items, and their payloads are system (never indexed). A Responses function call, output or
// reasoning item that follows a developer message keeps its own role, and a user message's text stays
// text.
func TestPieceRolesSystemAndDeveloperMessages(t *testing.T) {
	e := newEnv(t)
	ps := pieceStore{}
	run := func(in Input) map[wire.Hash]Role {
		sp := e.request(in, Options{})
		ps.add(sp)
		roles, err := PieceRoles(wire.KindExchangeBegin, beginBody(t, sp.Body()), nil, ps.head)
		if err != nil {
			t.Fatal(err)
		}
		return roles
	}
	chat := run(Input{Format: FormatOpenAIChat, Body: []byte(`{"messages":[
	  {"role":"system","content":"chat system prompt"},
	  {"role":"developer","content":[{"type":"text","text":"chat developer note"}]},
	  {"role":"user","content":"chat user text"},
	  {"role":"assistant","content":"chat answer","tool_calls":[{"id":"c1","type":"function","function":{"name":"shell","arguments":"{\"id\":4}"}}]}]}`)})
	resp := run(Input{Format: FormatOpenAIResponses, Body: []byte(`{"input":[
	  {"type":"message","role":"developer","content":[{"type":"input_text","text":"resp developer note"}]},
	  {"type":"function_call","call_id":"f1","name":"shell","arguments":"{\"id\":5}"},
	  {"type":"function_call_output","call_id":"f1","output":"resp shell output"},
	  {"role":"system","content":"resp system string"},
	  {"type":"reasoning","summary":[{"type":"summary_text","text":"resp reasoning"}]},
	  {"type":"message","role":"user","content":[{"type":"input_text","text":"resp user text"}]}]}`)})
	want := []struct {
		roles   map[wire.Hash]Role
		content string
		role    Role
	}{
		{chat, "chat system prompt", RoleSystem},
		{chat, "chat developer note", RoleSystem},
		{chat, "chat user text", RoleText},
		{chat, "chat answer", RoleText},
		{chat, `"{\"id\":4}"`, RoleToolInput},
		{resp, "resp developer note", RoleSystem},
		{resp, `"{\"id\":5}"`, RoleToolInput},
		{resp, "resp shell output", RoleToolResult},
		{resp, "resp system string", RoleSystem},
		{resp, `{"summary":[{"text":"resp reasoning","type":"summary_text"}]}`, RoleThinking},
		{resp, "resp user text", RoleText},
	}
	for _, w := range want {
		if got := roleOf(t, w.roles, ps, w.content); got != w.role {
			t.Errorf("%q: %s, want %s", w.content, got, w.role)
		}
	}
	all := map[wire.Hash]Role{}
	for _, m := range []map[wire.Hash]Role{chat, resp} {
		for h, r := range m {
			all[h] = r
		}
	}
	checkGolden(t, "roles/system-developer.golden.txt", rolesDump(all, ps))
}

// TestPieceRolesImportRecordSystemAndTools: in import records, a Claude Code api-request-blob system
// message and a Codex developer message are system; path items of prompt_snapshot attachments and
// api-request-shape records are system (systemPrompt, system) or tool_schema (tools); any other path
// item keeps the ordinary rules, even right after a system message. Path pointers count only in
// import records.
func TestPieceRolesImportRecordSystemAndTools(t *testing.T) {
	e := newEnv(t)
	ps := pieceStore{}
	piece := func(kind wire.PieceKind, s string) *wire.Hash {
		p := NewPiece(kind, "", []byte(s))
		ps[p.Hash] = p.Bytes
		return &p.Hash
	}
	path := func(ptr string, kind wire.PieceKind, s string) wire.Item {
		f, err := Marshal(map[string]string{"path": ptr})
		if err != nil {
			t.Fatal(err)
		}
		return wire.Item{F: f, P: piece(kind, s)}
	}
	record := func(client, typ string, items []wire.Item) json.RawMessage {
		b, err := Marshal(wire.ImportRecord{Client: client, FileRole: "main", SessionID: "s-synth", RecordType: typ, Mode: "full",
			Frame: json.RawMessage(`{"type":"` + typ + `"}`), Items: items})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	roles := func(body json.RawMessage) map[wire.Hash]Role {
		r, err := PieceRoles(wire.KindImportRecord, body, nil, ps.head)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	blob := e.messages(FormatClaude, []byte(`[{"role":"system","content":[{"type":"text","text":"blob system prompt"}]}]`), Options{})
	ps.add(blob)
	blobItems := append(blob.Items, path("/requestId", wire.PieceText, "blob path item after the system message"))
	dev := e.messages(FormatOpenAIResponses, []byte(`[{"type":"message","role":"developer","content":[{"type":"input_text","text":"codex developer note"}]}]`), Options{})
	ps.add(dev)
	user := e.messages(FormatClaude, []byte(`[{"role":"user","content":"transcript user text"}]`), Options{})
	ps.add(user)
	sets := []map[wire.Hash]Role{
		roles(record("claude-code", "api-request-blob", blobItems)),
		roles(record("codex", "response_item/message", dev.Items)),
		roles(record("claude-code", "user", user.Items)),
		roles(record("claude-code", "attachment", []wire.Item{
			path("/attachment/systemPrompt/0", wire.PieceJSON, `{"text":"snapshot system prompt"}`),
			path("/attachment/systemPrompt/1", wire.PieceJSON, `"snapshot system prompt two"`),
			path("/attachment/tools/0", wire.PieceJSON, `{"input_schema":{"type":"object"},"name":"snapshot_tool"}`),
		})),
		roles(record("claude-code", "attachment", []wire.Item{path("/attachment", wire.PieceJSON, `{"content":"other attachment","type":"skill_listing"}`)})),
		roles(record("claude-code", "api-request-shape", []wire.Item{
			path("/shape/system/0", wire.PieceJSON, `{"text":"shape system block","type":"text"}`),
			path("/shape/tools/0", wire.PieceJSON, `{"name":"shape_tool"}`),
			path("/shape/toolsCount", wire.PieceText, "shape field outside the arrays"),
		})),
		roles(record("claude-code", "last-prompt", []wire.Item{path("/lastPrompt", wire.PieceText, "last prompt text")})),
	}
	all := map[wire.Hash]Role{}
	for _, m := range sets {
		for h, r := range m {
			all[h] = r
		}
	}
	for content, role := range map[string]Role{
		"blob system prompt":                                        RoleSystem,
		"blob path item after the system message":                   RoleText,
		"codex developer note":                                      RoleSystem,
		"transcript user text":                                      RoleText,
		`{"text":"snapshot system prompt"}`:                         RoleSystem,
		`"snapshot system prompt two"`:                              RoleSystem,
		`{"input_schema":{"type":"object"},"name":"snapshot_tool"}`: RoleToolSchema,
		`{"content":"other attachment","type":"skill_listing"}`:     RoleText,
		`{"text":"shape system block","type":"text"}`:               RoleSystem,
		`{"name":"shape_tool"}`:                                     RoleToolSchema,
		"shape field outside the arrays":                            RoleText,
		"last prompt text":                                          RoleText,
	} {
		if got := roleOf(t, all, ps, content); got != role {
			t.Errorf("%q: %s, want %s", content, got, role)
		}
	}
	checkGolden(t, "roles/import-record-system-tools.golden.txt", rolesDump(all, ps))
	// An exchange body never has path items; a "path" frame field there is not a pointer rule.
	stray := path("/shape/system/0", wire.PieceText, "exchange item with a path field")
	ex, err := PieceRoles(wire.KindExchangeBegin, beginBody(t, wire.Body{Format: FormatClaude, Items: []wire.Item{stray}}), nil, ps.head)
	if err != nil {
		t.Fatal(err)
	}
	if got := ex[*stray.P]; got != RoleText {
		t.Errorf("exchange path field: %s, want text", got)
	}
}

// TestPieceRolesCodexToolsAndBaseInstructions: Codex sends tool schemas as Responses items of type
// additional_tools, in requests through CPA and in rollouts, and keeps its system prompt in session_meta
// base_instructions, which the importer moves to a path item under /payload/base_instructions/. The
// additional_tools payload is tool_schema wherever it sits (also right after a developer message), the
// base instructions are system, and other path items and the user's text stay indexed.
func TestPieceRolesCodexToolsAndBaseInstructions(t *testing.T) {
	e := newEnv(t)
	ps := pieceStore{}
	// A Codex request through CPA.
	req := e.request(Input{Format: FormatOpenAIResponses, Body: []byte(`{"instructions":"codex request instructions","input":[
	  {"type":"message","role":"developer","content":[{"type":"input_text","text":"codex request developer note"}]},
	  {"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"spawn_agent","description":"tools after a developer message"}]},
	  {"type":"message","role":"user","content":[{"type":"input_text","text":"codex request user text"}]},
	  {"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"wait_agent","description":"tools after a user message"}]}]}`)}, Options{})
	ps.add(req)
	exchange, err := PieceRoles(wire.KindExchangeBegin, beginBody(t, req.Body()), nil, ps.head)
	if err != nil {
		t.Fatal(err)
	}
	// Rollout records as the importer writes them: a response_item split with the Responses rules, and
	// a session_meta whose long string leaves are path items.
	record := func(typ string, items []wire.Item) map[wire.Hash]Role {
		b, err := Marshal(wire.ImportRecord{Client: "codex", FileRole: "codex_rollout", SessionID: "s-synth", RecordType: typ, Mode: "full",
			Frame: json.RawMessage(`{"type":"` + strings.SplitN(typ, "/", 2)[0] + `"}`), Items: items})
		if err != nil {
			t.Fatal(err)
		}
		r, err := PieceRoles(wire.KindImportRecord, b, nil, ps.head)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	path := func(ptr, s string) wire.Item {
		f, err := Marshal(map[string]string{"path": ptr})
		if err != nil {
			t.Fatal(err)
		}
		p := NewPiece(wire.PieceText, "", []byte(s))
		ps[p.Hash] = p.Bytes
		return wire.Item{F: f, P: &p.Hash}
	}
	tools := e.messages(FormatOpenAIResponses, []byte(`[{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"rollout_tool","description":"tools in a rollout record"}]}]`), Options{})
	ps.add(tools)
	sets := []map[wire.Hash]Role{
		exchange,
		record("response_item/additional_tools", tools.Items),
		record("session_meta", []wire.Item{path("/payload/base_instructions/text", "codex base instructions")}),
		record("world_state", []wire.Item{path("/payload/state/agents_md", "codex agents file")}),
	}
	all := map[wire.Hash]Role{}
	for _, m := range sets {
		for h, r := range m {
			all[h] = r
		}
	}
	// toolsPayload is the json piece the Responses split makes of an additional_tools item.
	toolsPayload := func(name, desc string) string {
		return `{"role":"developer","tools":[{"description":"` + desc + `","name":"` + name + `","type":"function"}]}`
	}
	for content, role := range map[string]Role{
		"codex request instructions":                                   RoleSystem,
		"codex request developer note":                                 RoleSystem,
		toolsPayload("spawn_agent", "tools after a developer message"): RoleToolSchema,
		"codex request user text":                                      RoleText,
		toolsPayload("wait_agent", "tools after a user message"):       RoleToolSchema,
		toolsPayload("rollout_tool", "tools in a rollout record"):      RoleToolSchema,
		"codex base instructions":                                      RoleSystem,
		"codex agents file":                                            RoleText,
	} {
		if got := roleOf(t, all, ps, content); got != role {
			t.Errorf("%q: %s, want %s", content, got, role)
		}
	}
	checkGolden(t, "roles/codex-tools-base-instructions.golden.txt", rolesDump(all, ps))
	// The path rule counts only in import records.
	stray := path("/payload/base_instructions/text", "exchange item with a base_instructions path field")
	ex, err := PieceRoles(wire.KindExchangeBegin, beginBody(t, wire.Body{Format: FormatOpenAIResponses, Items: []wire.Item{stray}}), nil, ps.head)
	if err != nil {
		t.Fatal(err)
	}
	if got := ex[*stray.P]; got != RoleText {
		t.Errorf("exchange path field: %s, want text", got)
	}
}

func TestPieceRolesKinds(t *testing.T) {
	fb := NewPiece(wire.PieceText, "", []byte("upstream failure body"))
	usage, _ := Marshal(wire.UsageAttempt{UsageRequestID: "u1", Provider: "p", Model: "m", SourceKind: "empty",
		RequestedAt: time.Unix(0, 0).UTC(), FailureBody: &fb.Hash})
	roles, err := PieceRoles(wire.KindUsage, usage, nil, noHead)
	if err != nil || roles[fb.Hash] != RoleFailureBody || len(roles) != 1 {
		t.Errorf("usage: %v %v", roles, err)
	}
	for kind, want := range map[string]Role{"tool_result_file": RoleToolResult, "skill": RoleText, "workflow_run": RoleText} {
		c := NewPiece(wire.PieceText, "", []byte("file "+kind))
		f, _ := Marshal(wire.ImportFile{Client: "claude-code", FileKey: "claude-projects/p/x", Content: c.Hash, Kind: kind})
		roles, err := PieceRoles(wire.KindImportFile, f, nil, noHead)
		if err != nil || roles[c.Hash] != want {
			t.Errorf("import.file %s: %v %v", kind, roles, err)
		}
	}
	for _, k := range []wire.Kind{wire.KindExchangeAttempt, wire.KindExchangeDone} {
		if roles, err := PieceRoles(k, json.RawMessage(`{"request_id":"r"}`), nil, noHead); err != nil || len(roles) != 0 {
			t.Errorf("%s: %v %v", k, roles, err)
		}
	}
	for _, c := range []struct {
		kind wire.Kind
		body string
	}{
		{"nope", `{}`},
		{wire.KindExchangeBegin, `[]`},
		{wire.KindExchangeBegin, `{"body":{"skeleton":"ABC"}}`},
		{wire.KindExchangeBegin, `{"body":{"items":[{"p":"` + strings.Repeat("A", 64) + `"}]}}`},
		{wire.KindUsage, `not json`},
	} {
		if _, err := PieceRoles(c.kind, json.RawMessage(c.body), nil, noHead); !errors.Is(err, wire.ErrSchema) {
			t.Errorf("%s %s: err %v, want ErrSchema", c.kind, c.body, err)
		}
	}
}

func TestIdxPolicy(t *testing.T) {
	want := map[Role]IdxPolicy{
		RoleSystem: IdxNone, RoleToolSchema: IdxNone, RoleParams: IdxNone, RoleReminder: IdxNone, RoleArchiveMCP: IdxNone,
		RoleThinking: IdxFull, RoleToolInput: IdxFull, RoleText: IdxFull,
		RoleToolResult: IdxHeadTail, RoleFailureBody: IdxHeadTail,
	}
	for r, p := range want {
		if r.IdxPolicy() != p {
			t.Errorf("%s: %s, want %s", r, r.IdxPolicy(), p)
		}
	}
}

func TestIsClaudeCode(t *testing.T) {
	for _, c := range []struct {
		h    []wire.Header
		want bool
	}{
		{nil, false},
		{[]wire.Header{{Name: "x-claude-code-session-id", Values: []string{"s"}}}, true},
		{[]wire.Header{{Name: "x-claude-code-session-id"}}, true},
		{[]wire.Header{{Name: "user-agent", Values: []string{"claude-cli/2.1.300 (external, cli)"}}}, true},
		{[]wire.Header{{Name: "user-agent", Values: []string{"curl/8", "claude-cli/2.0"}}}, true},
		{[]wire.Header{{Name: "user-agent", Values: []string{"codex_cli_rs/0.1"}}, {Name: "x-claude-code-agent-id", Values: []string{"a"}}}, false},
		{[]wire.Header{{Name: "user-agent", Values: []string{"my claude-cli/1"}}}, false},
	} {
		if got := IsClaudeCode(c.h); got != c.want {
			t.Errorf("%+v: %v, want %v", c.h, got, c.want)
		}
	}
}
