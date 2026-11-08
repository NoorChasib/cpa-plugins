// Package synth generates synthetic local history for the importer's tests and the mask-dry-run gate:
// every record type Claude Code and Codex write (Claude Code transcripts, attachments, state and side
// records, workflow journals, runs, scripts, agent meta files, saved tool outputs, skills and Codex
// rollouts) with the real shapes and invented text only. Nothing here is read from real history.
package synth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Gen produces deterministic ids, times and invented text from a seed.
type Gen struct {
	r   *rand.Rand
	t   time.Time
	seq int
}

// New returns a generator; the same seed gives the same output.
func New(seed uint64) *Gen {
	return &Gen{r: rand.New(rand.NewPCG(seed, 0x5157)), t: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)}
}

// UUID returns a deterministic version 4 UUID.
func (g *Gen) UUID() string {
	b := make([]byte, 16)
	for i := range b {
		b[i] = byte(g.r.UintN(256))
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := fmt.Sprintf("%x", b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// Hex returns n random lowercase hex characters.
func (g *Gen) Hex(n int) string {
	const a = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = a[g.r.IntN(16)]
	}
	return string(b)
}

// Alnum returns n random alphanumerics.
func (g *Gen) Alnum(n int) string {
	const a = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = a[g.r.IntN(len(a))]
	}
	return string(b)
}

// TS returns the next timestamp (one second later each call) in RFC 3339 with milliseconds.
func (g *Gen) TS() string {
	g.seq++
	return g.t.Add(time.Duration(g.seq) * time.Second).Format("2006-01-02T15:04:05.000Z")
}

var words = strings.Fields(`the a of to and in for build check run file test value request result output
model update branch merge plan review render parse write read list table index query field note step
config server client deploy cache stream token limit window batch queue session record shape number`)

// Words returns n invented words.
func (g *Gen) Words(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(words[g.r.IntN(len(words))])
	}
	return b.String()
}

// Text returns about n bytes of invented prose and code-like lines (no secrets).
func Text(n int, seed uint64) string {
	g := New(seed)
	var b strings.Builder
	b.Grow(n + 128)
	for line := 1; b.Len() < n; line++ {
		switch {
		case line%9 == 0:
			fmt.Fprintf(&b, "func %s%d(ctx context.Context) error { return check(%q) }\n", words[g.r.IntN(len(words))], line, g.Words(3))
		case line%13 == 0:
			fmt.Fprintf(&b, "  %s_%s = %d  # %s\n", words[g.r.IntN(len(words))], words[g.r.IntN(len(words))], g.r.IntN(1e6), g.Words(5))
		case line%17 == 0:
			fmt.Fprintf(&b, "see https://docs.example.invalid/%s/%s for the %s\n", words[g.r.IntN(len(words))], g.Alnum(8), g.Words(2))
		default:
			b.WriteString(g.Words(8 + g.r.IntN(10)))
			b.WriteString(".\n")
		}
	}
	return b.String()
}

func line(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// M is a JSON object.
type M = map[string]any

// Claude builds Claude Code transcript records for one file.
type Claude struct {
	G         *Gen
	SessionID string
	AgentID   string // set for subagent and workflow-agent files
	Sidechain bool
	last      string
}

func (c *Claude) env(typ string) M {
	u := c.G.UUID()
	m := M{"type": typ, "uuid": u, "parentUuid": nil, "sessionId": c.SessionID, "isSidechain": c.Sidechain,
		"cwd": "/work/example", "gitBranch": "main", "version": "2.1.300", "entrypoint": "cli",
		"userType": "external", "timestamp": c.G.TS()}
	if c.last != "" {
		m["parentUuid"] = c.last
	}
	if c.AgentID != "" {
		m["agentId"] = c.AgentID
	}
	c.last = u
	return m
}

// UserText is a user record with string content.
func (c *Claude) UserText(text string) string {
	m := c.env("user")
	m["promptId"] = c.G.UUID()
	m["message"] = M{"role": "user", "content": text}
	return line(m)
}

// UserBlocks is a user record with a list of text blocks and an image block.
func (c *Claude) UserBlocks(text string, image []byte) string {
	m := c.env("user")
	content := []any{M{"type": "text", "text": text}}
	if image != nil {
		content = append(content, M{"type": "image", "source": M{"type": "base64", "media_type": "image/png", "data": base64.StdEncoding.EncodeToString(image)}})
	}
	m["message"] = M{"role": "user", "content": content}
	return line(m)
}

// ToolResult is a user record answering a tool_use, with a toolUseResult of the given shape.
func (c *Claude) ToolResult(toolUseID string, content any, tur any) string {
	m := c.env("user")
	m["sourceToolAssistantUUID"] = c.G.UUID()
	m["message"] = M{"role": "user", "content": []any{M{"type": "tool_result", "tool_use_id": toolUseID, "content": content, "is_error": false}}}
	if tur != nil {
		m["toolUseResult"] = tur
	}
	return line(m)
}

func (c *Claude) assistant(msgID string, block M) M {
	m := c.env("assistant")
	m["requestRef"] = c.G.UUID()
	m["effort"] = "high"
	m["message"] = M{"id": msgID, "model": "claude-test-model", "role": "assistant", "type": "message",
		"content": []any{block}, "stop_reason": nil, "stop_sequence": nil,
		"usage": M{"input_tokens": 10, "output_tokens": 20, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}}
	return m
}

// Thinking is an assistant record with one thinking block (signature included).
func (c *Claude) Thinking(msgID, text string) string {
	return line(c.assistant(msgID, M{"type": "thinking", "thinking": text, "signature": base64.StdEncoding.EncodeToString([]byte("synthetic-signature-" + c.G.Hex(16)))}))
}

// RedactedThinking is an assistant record with a redacted_thinking block.
func (c *Claude) RedactedThinking(msgID string) string {
	return line(c.assistant(msgID, M{"type": "redacted_thinking", "data": base64.StdEncoding.EncodeToString([]byte("redacted-" + c.G.Hex(24)))}))
}

// AssistantText is an assistant record with one text block.
func (c *Claude) AssistantText(msgID, text string) string {
	return line(c.assistant(msgID, M{"type": "text", "text": text}))
}

// ToolUse is an assistant record with one tool_use block; wire, when set, becomes wireToolInputs.
func (c *Claude) ToolUse(msgID, id, name string, input, wire M) string {
	m := c.assistant(msgID, M{"type": "tool_use", "id": id, "name": name, "input": input, "caller": M{"type": "direct"}})
	if wire != nil {
		m["wireToolInputs"] = M{id: wire}
		m["wireIngestContext"] = M{id: M{"cwd": "/work/example"}}
	}
	return line(m)
}

// System is a system record of the given subtype.
func (c *Claude) System(subtype, content string) string {
	m := c.env("system")
	m["subtype"], m["level"], m["content"] = subtype, "info", content
	if subtype == "compact_boundary" {
		m["compactMetadata"] = M{"trigger": "auto", "preTokens": 1000, "postTokens": 100}
	}
	return line(m)
}

// Attachment is an attachment record of the given type and payload fields.
func (c *Claude) Attachment(typ string, fields M) string {
	m := c.env("attachment")
	att := M{"type": typ}
	for k, v := range fields {
		att[k] = v
	}
	m["attachment"] = att
	return line(m)
}

// PromptSnapshot is a prompt_snapshot attachment with system prompt and tool entries.
func (c *Claude) PromptSnapshot(system []string, tools []string) string {
	var sp, ts []any
	for _, s := range system {
		sp = append(sp, M{"type": "text", "text": s})
	}
	for _, t := range tools {
		ts = append(ts, M{"name": t, "description": c.G.Words(12), "schema": M{"type": "object"}})
	}
	return c.Attachment("prompt_snapshot", M{"systemPrompt": sp, "tools": ts, "cliPrefix": "cli", "inlineTools": true})
}

// APIRequest is an api-request record (frame only).
func (c *Claude) APIRequest(base string) string {
	m := M{"type": "api-request", "id": c.G.UUID(), "base": base, "keep": 3, "tail": []any{c.G.Hex(16)},
		"requestDigest": c.G.Hex(16), "shapeHash": c.G.Hex(16), "querySource": "repl_main_thread",
		"params": M{"model": "claude-test-model", "max_tokens": 32000}, "cacheControl": []any{M{"path": "system.0", "value": "ephemeral"}},
		"sessionId": c.SessionID, "version": "2.1.300", "timestamp": c.G.TS()}
	return line(m)
}

// APIRequestShape is an api-request-shape record.
func (c *Claude) APIRequestShape(system, tools []string) string {
	var sp, ts []any
	for _, s := range system {
		sp = append(sp, M{"type": "text", "text": s})
	}
	for _, t := range tools {
		ts = append(ts, M{"name": t, "input_schema": M{"type": "object"}})
	}
	return line(M{"type": "api-request-shape", "shapeHash": c.G.Hex(16), "shape": M{"system": sp, "tools": ts},
		"sessionId": c.SessionID, "version": "2.1.300", "timestamp": c.G.TS()})
}

// APIRequestBlob is an api-request-blob record of the given role.
func (c *Claude) APIRequestBlob(role, text string) string {
	return line(M{"type": "api-request-blob", "hash": c.G.Hex(32), "sessionId": c.SessionID,
		"message": M{"role": role, "content": []any{M{"type": "text", "text": text}}}})
}

// State is a record without a uuid (queue-operation, pr-link, cost-state, ai-title, custom-title,
// mode, last-prompt, atis-latch, tools-used, permission-mode, agent-setting, file-history-*).
func (c *Claude) State(typ string, fields M) string {
	m := M{"type": typ}
	if !strings.HasPrefix(typ, "file-history-") {
		m["sessionId"] = c.SessionID
	}
	for k, v := range fields {
		m[k] = v
	}
	return line(m)
}

// AllStates returns one record of each state and side type with invented values.
func (c *Claude) AllStates() []string {
	g := c.G
	return []string{
		c.State("queue-operation", M{"operation": "enqueue", "content": "queued: " + g.Words(6), "timestamp": g.TS()}),
		c.State("pr-link", M{"prNumber": 7, "prRepository": "example/example", "prUrl": "https://example.invalid/pr/7", "timestamp": g.TS()}),
		c.State("cost-state", M{"totalCostUSD": 0.5, "totalDuration": 1000, "startTime": 1, "modelUsage": M{"claude-test-model": M{"inputTokens": 10, "outputTokens": 5}}}),
		c.State("ai-title", M{"aiTitle": g.Words(4)}),
		c.State("custom-title", M{"customTitle": g.Words(3)}),
		c.State("mode", M{"mode": "normal"}),
		c.State("last-prompt", M{"lastPrompt": "last: " + g.Words(8), "leafUuid": g.UUID()}),
		c.State("atis-latch", M{"atis": true}),
		c.State("tools-used", M{"tools": []any{"Bash", "Read"}}),
		c.State("permission-mode", M{"permissionMode": "default"}),
		c.State("agent-setting", M{"agentSetting": "default"}),
		c.State("file-history-snapshot", M{"messageId": g.UUID(), "isSnapshotUpdate": false, "snapshot": M{"messageId": g.UUID(), "timestamp": g.TS(), "trackedFileBackups": M{}}}),
		c.State("file-history-delta", M{"messageId": g.UUID(), "timestamp": g.TS(), "backup": M{}}),
	}
}

// Journal builds workflow journal records.
func Journal(g *Gen, agentID string) []string {
	return []string{
		line(M{"type": "launched"}),
		line(M{"type": "started", "agentId": agentID, "key": "k-" + g.Hex(6), "label": g.Words(2), "phase": "build"}),
		line(M{"type": "result", "agentId": agentID, "key": "k-" + g.Hex(6), "result": M{"summary": g.Words(10), "ok": true}, "toolsUsed": []any{"Bash"}}),
		line(M{"type": "failed", "agentId": agentID, "key": "k-" + g.Hex(6)}),
	}
}

// WorkflowRun is a wf_<id>.json body with an inline script.
func WorkflowRun(g *Gen, runID, script string) string {
	return line(M{"runId": runID, "taskId": g.Hex(8), "workflowName": "synthetic", "status": "completed",
		"summary": g.Words(8), "startTime": 1, "timestamp": g.TS(), "durationMs": 1200, "agentCount": 1,
		"script": script, "scriptPath": "/work/example/scripts/flow.js", "args": M{"n": 1}, "result": M{"ok": true},
		"phases": []any{M{"title": "build", "detail": g.Words(4)}}, "workflowProgress": []any{M{"index": 0, "type": "workflow_agent", "agentId": g.Hex(8), "state": "done"}}})
}

// AgentMeta is an agent-<id>.meta.json body.
func AgentMeta(g *Gen) string {
	return line(M{"agentType": "general", "description": g.Words(5), "toolUseId": "toolu_" + g.Alnum(20), "spawnDepth": 1, "finished": true})
}

// Codex builds rollout records.
type Codex struct {
	G        *Gen
	ThreadID string
	Session  string
	ord      int64
}

func (c *Codex) rec(typ string, payload any) string {
	m := M{"timestamp": c.G.TS(), "type": typ, "payload": payload, "ordinal": c.ord}
	c.ord++
	return line(m)
}

// SessionMeta is the first record (and, with fork set, the parent's copy a fork carries).
func (c *Codex) SessionMeta(provider string, forkedFrom string, histStart int64) string {
	p := M{"id": c.ThreadID, "session_id": c.Session, "cwd": "/work/example", "originator": "codex_exec",
		"cli_version": "0.160.0", "model_provider": provider, "source": "cli", "base_instructions": M{"text": c.G.Words(30)}}
	if forkedFrom != "" {
		p["forked_from_id"] = forkedFrom
		p["parent_thread_id"] = forkedFrom
		p["subagent_history_start_ordinal"] = histStart
	}
	return c.rec("session_meta", p)
}

// ParentMeta is the parent's session_meta inside a forked rollout.
func (c *Codex) ParentMeta(parent string) string {
	return c.rec("session_meta", M{"id": parent, "session_id": c.Session, "cwd": "/work/example", "model_provider": "cliproxyapi"})
}

func (c *Codex) TurnContext() string {
	return c.rec("turn_context", M{"turn_id": c.G.Hex(8), "cwd": "/work/example", "model": "gpt-test", "effort": "high", "summary": "auto"})
}

func (c *Codex) WorldState(text string) string {
	return c.rec("world_state", M{"state": M{"agents_md": text}})
}

func (c *Codex) UserMessage(text string) string {
	return c.rec("response_item", M{"type": "message", "role": "user", "content": []any{M{"type": "input_text", "text": text}}})
}

func (c *Codex) DeveloperMessage(text string) string {
	return c.rec("response_item", M{"type": "message", "role": "developer", "content": []any{M{"type": "input_text", "text": text}}})
}

func (c *Codex) AssistantMessage(text string) string {
	return c.rec("response_item", M{"type": "message", "role": "assistant", "phase": "final_answer", "content": []any{M{"type": "output_text", "text": text, "annotations": []any{}}}})
}

func (c *Codex) Reasoning(summary string) string {
	return c.rec("response_item", M{"type": "reasoning", "id": "rs_" + c.G.Hex(8), "summary": []any{M{"type": "summary_text", "text": summary}}, "encrypted_content": base64.StdEncoding.EncodeToString([]byte("enc-" + c.G.Hex(20)))})
}

func (c *Codex) CustomToolCall(callID, input string) string {
	return c.rec("response_item", M{"type": "custom_tool_call", "call_id": callID, "id": "ctc_" + c.G.Hex(8), "name": "exec", "input": input, "status": "completed"})
}

func (c *Codex) CustomToolCallOutput(callID, output string) string {
	return c.rec("response_item", M{"type": "custom_tool_call_output", "call_id": callID, "output": output})
}

func (c *Codex) FunctionCall(callID, name, args string) string {
	return c.rec("response_item", M{"type": "function_call", "call_id": callID, "id": "fc_" + c.G.Hex(8), "name": name, "namespace": "agents", "arguments": args})
}

func (c *Codex) FunctionCallOutput(callID, output string) string {
	return c.rec("response_item", M{"type": "function_call_output", "call_id": callID, "output": output})
}

func (c *Codex) AgentMessage(text string) string {
	return c.rec("response_item", M{"type": "agent_message", "content": []any{M{"type": "input_text", "text": text}}})
}

// CommandExecution is an event_msg/item_completed of a command, with stdout and formatted_output
// equal to aggregated_output when same is set.
func (c *Codex) CommandExecution(output string, same bool) string {
	stdout, formatted := output, output
	if !same {
		stdout, formatted = output+"\n(stdout)", "$ cmd\n"+output
	}
	return c.rec("event_msg", M{"type": "item_completed", "thread_id": c.ThreadID, "turn_id": c.G.Hex(8),
		"started_at_ms": 1000, "completed_at_ms": 1500, "item": M{"type": "CommandExecution", "command": []any{"bash", "-lc", "go test"},
			"cwd": "/work/example", "exit_code": 0, "status": "completed", "duration": M{"secs": 0, "nanos": 500},
			"aggregated_output": output, "stdout": stdout, "stderr": "", "formatted_output": formatted}})
}

func (c *Codex) TokenCount() string {
	return c.rec("event_msg", M{"type": "token_count", "info": M{"last_token_usage": M{"input_tokens": 10, "output_tokens": 5}}})
}

func (c *Codex) TaskStarted() string {
	return c.rec("event_msg", M{"type": "task_started", "turn_id": c.G.Hex(8), "started_at": 1})
}

func (c *Codex) TaskComplete() string {
	return c.rec("event_msg", M{"type": "task_complete", "turn_id": c.G.Hex(8), "started_at": 1, "completed_at": 2, "duration_ms": 1000})
}

func (c *Codex) TurnAborted() string {
	return c.rec("event_msg", M{"type": "turn_aborted", "reason": "interrupted"})
}

func (c *Codex) ThreadSettings() string {
	return c.rec("event_msg", M{"type": "thread_settings_applied", "thread_settings": M{"cwd": "/work/example", "model": "gpt-test", "reasoning_effort": "high"}})
}

func (c *Codex) AdditionalTools(names ...string) string {
	var tools []any
	for _, n := range names {
		tools = append(tools, M{"type": "function", "name": n, "description": c.G.Words(6)})
	}
	return c.rec("response_item", M{"type": "additional_tools", "tools": tools})
}

// UserImage is a user message with an input_image data URL.
func (c *Codex) UserImage(text string, image []byte) string {
	return c.rec("response_item", M{"type": "message", "role": "user", "content": []any{
		M{"type": "input_text", "text": text},
		M{"type": "input_image", "image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(image), "detail": "auto"}}})
}

func (c *Codex) TokenUsage(responseID string) string {
	return c.rec("token_usage_record", M{"response_id": responseID, "turn_id": c.G.Hex(8), "thread_id": c.ThreadID, "session_id": c.Session,
		"usage": M{"input_tokens": 100, "cached_input_tokens": 0, "output_tokens": 20, "reasoning_output_tokens": 5, "total_tokens": 125}})
}

func (c *Codex) Compacted(text string) string {
	return c.rec("compacted", M{"message": text, "window_id": c.G.Hex(8), "window_number": 1})
}

func (c *Codex) InterAgent() string {
	return c.rec("inter_agent_communication_metadata", M{"trigger_turn": true})
}

// Write writes lines (each one record) to path with a trailing newline per line.
func Write(path string, lines ...string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// Append appends lines to path.
func Append(path string, lines ...string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			return err
		}
	}
	return nil
}
