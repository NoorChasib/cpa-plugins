package canon

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// The CPA wire body of one Claude Code turn (synthetic). The wire carries cache_control and the exact
// tool input the model produced.
const wireTurn = `{
  "model": "claude-test-model", "max_tokens": 1000,
  "messages": [
    {"role": "user", "content": "Run the tests."},
    {"role": "assistant", "content": [
      {"type": "thinking", "thinking": "Run go test first.", "signature": "U3ludGhldGljVHJhbnNjcmlwdFNpZw=="},
      {"type": "text", "text": "Running the tests."},
      {"type": "tool_use", "id": "toolu_synth_t1", "name": "Bash", "input": {"command": "go test ./...", "description": "Run the tests", "timeout": 120000}}
    ]},
    {"role": "user", "content": [
      {"type": "tool_result", "tool_use_id": "toolu_synth_t1", "content": "ok  synthetic 0.2s", "cache_control": {"type": "ephemeral"}}
    ]}
  ]
}`

// The same turn as Claude Code transcript records (synthetic shapes): one
// record per content block, message metadata around it, and a tool_use.input in the transcript's own
// form, while wireToolInputs keeps the wire form keyed by tool_use id.
var transcriptRecords = []string{
	`{"type":"assistant","uuid":"u-1","sessionId":"s-1","message":{"id":"msg_synth_t","model":"claude-test-model","role":"assistant","type":"message",
	  "content":[{"signature":"U3ludGhldGljVHJhbnNjcmlwdFNpZw==","thinking":"Run go test first.","type":"thinking"}]}}`,
	`{"type":"assistant","uuid":"u-2","sessionId":"s-1","message":{"id":"msg_synth_t","model":"claude-test-model","role":"assistant","type":"message",
	  "content":[{"text":"Running the tests.","type":"text"}]}}`,
	`{"type":"assistant","uuid":"u-3","sessionId":"s-1","message":{"id":"msg_synth_t","model":"claude-test-model","role":"assistant","type":"message",
	  "content":[{"type":"tool_use","id":"toolu_synth_t1","name":"Bash","input":{"command":"go test ./...","description":"Run the tests","timeout":120000,"dangerouslyDisableSandbox":false}}]},
	  "wireToolInputs":{"toolu_synth_t1":{"timeout":120000,"description":"Run the tests","command":"go test ./..."}}}`,
	`{"type":"user","uuid":"u-4","sessionId":"s-1","message":{"role":"user","content":[{"tool_use_id":"toolu_synth_t1","type":"tool_result","content":"ok  synthetic 0.2s"}]},
	  "toolUseResult":{"stdout":"ok  synthetic 0.2s","interrupted":false}}`,
}

// applyWireToolInputs is the importer's record step, here only to build the golden: every tool_use.input
// is replaced by wireToolInputs[<tool_use id>] when present. It returns the record's message.
func applyWireToolInputs(t *testing.T, record string) json.RawMessage {
	t.Helper()
	var rec struct {
		Message        map[string]json.RawMessage `json:"message"`
		WireToolInputs map[string]json.RawMessage `json:"wireToolInputs"`
	}
	if err := json.Unmarshal([]byte(record), &rec); err != nil {
		t.Fatal(err)
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(rec.Message["content"], &blocks); err != nil {
		t.Fatal(err)
	}
	for _, b := range blocks {
		var typ, id string
		json.Unmarshal(b["type"], &typ)
		json.Unmarshal(b["id"], &id)
		if wi, ok := rec.WireToolInputs[id]; ok && typ == "tool_use" {
			b["input"] = wi
		}
	}
	c, _ := json.Marshal(blocks)
	rec.Message["content"] = c
	m, _ := json.Marshal(rec.Message)
	return m
}

func blockItems(items []wire.Item) []wire.Item {
	var out []wire.Item
	for _, it := range items {
		if len(it.F) > 0 {
			out = append(out, it)
		}
	}
	return out
}

// TestTranscriptBlocksHashEqualWire: after wireToolInputs canonicalization, every Claude transcript
// block hashes equal to the same CPA wire block (item id and payload piece), so the transcript copy and
// the CPA copy dedup.
func TestTranscriptBlocksHashEqualWire(t *testing.T) {
	run := func(t *testing.T, e *env) {
		wireSp := e.request(Input{Format: FormatClaude, Body: []byte(wireTurn)}, Options{})
		wireBlocks := blockItems(wireSp.Items)
		var trBlocks []wire.Item
		for _, rec := range transcriptRecords {
			msg := applyWireToolInputs(t, rec)
			sp := e.messages(FormatClaude, append(append([]byte("["), msg...), ']'), Options{})
			checkInvariants(t, sp)
			trBlocks = append(trBlocks, blockItems(sp.Items)...)
		}
		// The wire has the user prompt's text block first; the transcript records hold the other four.
		wireBlocks = wireBlocks[1:]
		if len(wireBlocks) != len(trBlocks) {
			t.Fatalf("%d wire blocks, %d transcript blocks", len(wireBlocks), len(trBlocks))
		}
		for i := range wireBlocks {
			w, r := wireBlocks[i], trBlocks[i]
			if ItemID(w) != ItemID(r) || *w.P != *r.P {
				t.Errorf("block %d: wire %s / transcript %s differ", i, w.F, r.F)
			}
		}
		if len(wireBlocks[3].V) == 0 || len(trBlocks[3].V) != 0 {
			t.Error("cache_control should be V on the wire only")
		}
		// Without the wireToolInputs step the transcript's own input form does not dedup.
		var rec struct {
			Message json.RawMessage `json:"message"`
		}
		json.Unmarshal([]byte(transcriptRecords[2]), &rec)
		raw := e.messages(FormatClaude, append(append([]byte("["), rec.Message...), ']'), Options{})
		if b := blockItems(raw.Items)[0]; *b.P == *wireBlocks[2].P {
			t.Error("the transcript input form unexpectedly equals the wire form")
		}
	}
	t.Run("test-masker", func(t *testing.T) {
		e := newEnv(t)
		run(t, e)
		// Golden: the shared block ids and payload hashes.
		wireSp := e.request(Input{Format: FormatClaude, Body: []byte(wireTurn)}, Options{})
		var buf bytes.Buffer
		for _, it := range blockItems(wireSp.Items) {
			buf.WriteString(hx(ItemID(it)) + " " + hp(it.P) + " " + string(it.F) + "\n")
		}
		checkGolden(t, "transcript/claude-wire-blocks.golden.txt", buf.Bytes())
	})
	// With pkg/mask.
	t.Run("real-masker", func(t *testing.T) {
		e := newEnv(t)
		e.m = realMasker(t)
		run(t, e)
	})
}
