package importer

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/importer/synth"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

type M = synth.M

const (
	gProject = "-work-example"
	gSession = "11111111-1111-4111-8111-111111111111"
	gThread  = "22222222-2222-4222-8222-222222222222"
	gChild   = "33333333-3333-4333-8333-333333333333"
)

// fakePNG is 2 KiB of bytes that are not UTF-8, so canon keeps them as a binary attachment.
func fakePNG() []byte {
	b := []byte("\x89PNG\r\n\x1a\n")
	for i := 0; len(b) < 2048; i++ {
		b = append(b, byte(0x80+i%64))
	}
	return b
}

// writeCorpus writes one synthetic file of every kind the importer reads, with every record type Claude
// Code and Codex write. It returns the Codex session id whose second call CPA covers.
func (x *h) writeCorpus() {
	g := x.g
	sdir := filepath.Join(x.projects(), gProject, gSession)
	c := &synth.Claude{G: g, SessionID: gSession}
	main := []string{
		c.UserText("please " + g.Words(6)),
		c.UserBlocks(g.Words(5), fakePNG()),
		c.Thinking("msg_synth_1", g.Words(10)),
		c.AssistantText("msg_synth_1", g.Words(8)),
		c.ToolUse("msg_synth_1", "toolu_synth_a", "Bash",
			M{"command": "go test ./...", "description": "run tests", "timeout": 120000, "dangerouslyDisableSandbox": false},
			M{"command": "go test ./...", "description": "run tests", "timeout": 120000}),
		c.ToolResult("toolu_synth_a", "ok  synthetic 0.2s", M{"stdout": "ok  synthetic 0.2s", "stderr": "", "interrupted": false,
			"persistedOutputPath": "/work/tool-results/abc.txt", "persistedOutputSize": 18, "gitOperation": M{"push": true}}),
		c.ToolResult("toolu_synth_b", []any{M{"type": "text", "text": "list " + g.Words(4)}}, "a string toolUseResult"),
		c.ToolResult("toolu_synth_c", "agent done", M{"agentId": "a1", "agentType": "general", "status": "completed", "totalTokens": 50,
			"content": []any{M{"type": "text", "text": "repeats the result"}}, "prompt": "dropped prompt"}),
		c.RedactedThinking("msg_synth_2"),
		c.System("compact_boundary", "Conversation compacted"),
		c.System("api_error", "overloaded"),
		c.PromptSnapshot([]string{"system prompt " + g.Words(6), "second system part"}, []string{"Bash", "Read"}),
		c.Attachment("queued_command", M{"commandMode": "prompt", "prompt": "queued " + g.Words(5)}),
		c.Attachment("invoked_skills", M{"skills": []any{M{"name": "demo", "path": "/skills/demo", "content": "skill text " + g.Words(5)}}}),
		c.Attachment("skill_listing", M{"content": "skills: demo", "names": []any{"demo"}, "skillCount": 1}),
		c.Attachment("file", M{"filename": "a.go", "content": M{"type": "text", "file": M{"filePath": "a.go", "content": "package a"}}}),
		c.Attachment("edited_text_file", M{"filename": "a.go", "snippet": "func A() {}"}),
		c.Attachment("deferred_tools_record", M{"toolInputCopies": []any{M{"id": "x", "copy": "y"}}}),
		c.APIRequest(""),
		c.APIRequestShape([]string{"shape system " + g.Words(4)}, []string{"Bash"}),
		c.APIRequestBlob("system", "blob system prompt "+g.Words(6)),
		c.APIRequestBlob("user", "blob user body dropped"),
		c.APIRequestBlob("assistant", "blob assistant body dropped"),
	}
	main = append(main, c.AllStates()...)
	x.write(x.mainFile(gProject, gSession), main...)

	sub := &synth.Claude{G: g, SessionID: gSession, AgentID: "a1", Sidechain: true}
	x.write(filepath.Join(sdir, "subagents", "agent-a1.jsonl"), sub.UserText("subagent task"), sub.AssistantText("msg_synth_3", "subagent answer"))
	x.writeRaw(filepath.Join(sdir, "subagents", "agent-a1.meta.json"), []byte(synth.AgentMeta(g)))
	wf := filepath.Join(sdir, "subagents", "workflows", "wf_run1")
	wa := &synth.Claude{G: g, SessionID: gSession, AgentID: "a2", Sidechain: true}
	x.write(filepath.Join(wf, "agent-a2.jsonl"), wa.UserText("workflow agent task"), wa.AssistantText("msg_synth_4", "workflow agent answer"))
	x.writeRaw(filepath.Join(wf, "agent-a2.meta.json"), []byte(synth.AgentMeta(g)))
	x.write(filepath.Join(wf, "journal.jsonl"), synth.Journal(g, "a2")...)
	script := "export default async function flow() { return 1 }\n"
	x.writeRaw(filepath.Join(sdir, "workflows", "wf_run1.json"), []byte(synth.WorkflowRun(g, "wf_run1", script)))
	x.writeRaw(filepath.Join(sdir, "workflows", "scripts", "flow.js"), []byte(script)) // embedded: skipped
	x.writeRaw(filepath.Join(sdir, "workflows", "scripts", "other.js"), []byte("export const other = 2\n"))
	x.writeRaw(filepath.Join(sdir, "tool-results", "abc.txt"), []byte("saved output "+g.Words(10)+"\n"))
	x.writeRaw(filepath.Join(sdir, "tool-results", "pic.png"), fakePNG())
	x.writeRaw(filepath.Join(x.skills(), "demo", "SKILL.md"), []byte("# demo\n"+g.Words(12)+"\n"))
	x.writeRaw(filepath.Join(x.skills(), "demo", "blob.bin"), fakePNG()) // not text: skipped

	cx := &synth.Codex{G: g, ThreadID: gThread, Session: gThread}
	big := synth.Text(1500, 7) // a large string leaf: its own text piece
	x.write(filepath.Join(x.codex(), "2026", "10", "01", "rollout-2026-10-01T09-00-00-"+gThread+".jsonl"),
		cx.SessionMeta("cliproxyapi", "", 0),                        // 1
		cx.TurnContext(),                                            // 2
		cx.WorldState(big),                                          // 3
		cx.DeveloperMessage("developer instructions"),               // 4
		cx.UserMessage("first question"),                            // 5  call 1 input
		cx.Reasoning("thinking about it"),                           // 6  call 1 output
		cx.CustomToolCall("call_1", "go test ./..."),                // 7
		cx.TokenUsage("resp_1"),                                     // 8  closes call 1
		cx.CustomToolCallOutput("call_1", "ok  synthetic"),          // 9  call 2 input
		cx.CommandExecution(synth.Text(1200, 8), true),              // 10 in no call (full); big aggregated_output
		cx.AssistantMessage("all tests pass"),                       // 11 call 2 output
		cx.TokenUsage("resp_2"),                                     // 12 closes call 2 (covered)
		cx.UserMessage("second question"),                           // 13 call 3 input
		cx.FunctionCall("call_2", "spawn_agent", `{"task":"look"}`), // 14 call 3 output
		cx.TokenUsage("resp_3"),                                     // 15 closes call 3
		cx.FunctionCallOutput("call_2", "agent started"),            // 16 open call: full
		cx.CommandExecution("short output", false),                  // 17
		cx.TaskComplete(),                                           // 18
		cx.TokenCount(),                                             // 19
		cx.Compacted("compacted summary"),                           // 20
		cx.InterAgent(),                                             // 21
		cx.AgentMessage("note from another agent"),                  // 22
		cx.TaskStarted(),                                            // 23
		cx.TurnAborted(),                                            // 24
		cx.ThreadSettings(),                                         // 25
		cx.AdditionalTools("spawn_agent", "wait_agent"),             // 26
		cx.UserImage("look at this", fakePNG()),                     // 27
	)
	x.srv.SetCoverage("codex:"+gThread, wire.Coverage{CPASeen: true, Exchanges: 3, Responses: []string{"resp_2"}})

	fk := &synth.Codex{G: g, ThreadID: gChild, Session: gThread}
	x.write(filepath.Join(x.codex(), "2026", "10", "01", "rollout-2026-10-01T09-30-00-"+gChild+".jsonl"),
		fk.SessionMeta("openai", gThread, 3), // ordinal 0
		fk.ParentMeta(gThread),               // ordinal 1: skipped
		fk.UserMessage("inherited question"), // ordinal 2: inherited
		fk.AssistantMessage("own answer"),    // ordinal 3: own
	)
}

// TestGoldenEnvelopes: every record type and file kind, Codex full and metadata modes, and a fork, as
// the fake archive received them. Regenerate with -update.
func TestGoldenEnvelopes(t *testing.T) {
	x := newH(t)
	x.enroll()
	x.writeCorpus()
	x.mustRun()
	got := x.envelopes()
	if len(got) == 0 {
		t.Fatal("no envelopes")
	}
	var buf bytes.Buffer
	types := map[string]bool{}
	for _, r := range got {
		e := r.Envelope
		if r.Status != wire.StatusOK {
			t.Errorf("seq %d %s: status %s %s", e.Seq, e.Key, r.Status, r.Reason)
		}
		line := map[string]any{"seq": e.Seq, "key": e.Key, "kind": e.Kind, "created_at": e.CreatedAt, "refs": len(e.Refs), "loc": e.Loc, "body": json.RawMessage(e.Body)}
		b, _ := json.Marshal(line)
		buf.Write(b)
		buf.WriteByte('\n')
		if e.Kind == wire.KindImportRecord {
			types[decodeRecord(t, e).RecordType] = true
		} else {
			var f wire.ImportFile
			json.Unmarshal(e.Body, &f)
			types["file:"+f.Kind] = true
		}
	}
	checkGolden(t, "envelopes.golden.jsonl", buf.Bytes())

	want := []string{"user", "assistant", "system", "attachment", "api-request", "api-request-shape", "api-request-blob",
		"queue-operation", "pr-link", "cost-state", "ai-title", "custom-title", "mode", "last-prompt", "atis-latch", "tools-used",
		"permission-mode", "agent-setting", "file-history-snapshot", "file-history-delta", "launched", "started", "result", "failed",
		"session_meta", "turn_context", "world_state", "response_item/message", "response_item/reasoning", "response_item/custom_tool_call",
		"response_item/custom_tool_call_output", "response_item/function_call", "response_item/function_call_output",
		"response_item/agent_message", "response_item/additional_tools", "event_msg/item_completed", "event_msg/task_complete",
		"event_msg/task_started", "event_msg/turn_aborted", "event_msg/thread_settings_applied", "event_msg/token_count",
		"token_usage_record", "compacted", "inter_agent_communication_metadata",
		"file:workflow_run", "file:workflow_script", "file:agent_meta", "file:tool_result_file", "file:skill"}
	for _, w := range want {
		if !types[w] {
			t.Errorf("no envelope of record type %s", w)
		}
	}
	var keys []string
	for k := range types {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("record types: %s", strings.Join(keys, " "))
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if !bytes.Equal(want, got) {
		gl, wl := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
		for i := 0; i < len(gl) && i < len(wl); i++ {
			if gl[i] != wl[i] {
				t.Fatalf("%s differs at line %d:\n got %s\nwant %s", name, i+1, gl[i], wl[i])
			}
		}
		t.Fatalf("%s differs: %d lines, want %d", name, len(gl), len(wl))
	}
}
