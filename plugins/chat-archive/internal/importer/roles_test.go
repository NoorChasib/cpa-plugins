package importer

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/importer/synth"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// TestSystemPromptAndToolSchemaRoles: the archive assigns piece roles at ingest with canon.PieceRoles,
// and system prompts and tool schemas must never be word-indexed. This checks the records the importer
// actually writes: prompt_snapshot system prompt and tool entries, api-request-shape system and tool
// entries, the api-request-blob system message, the Codex developer message, the Codex additional_tools
// item and the Codex session_meta base instructions get roles system and tool_schema (index policy
// none), while ordinary transcript text and other moved string leaves stay indexed.
func TestSystemPromptAndToolSchemaRoles(t *testing.T) {
	x := newH(t)
	x.enroll()
	x.writeCorpus()
	// A rollout whose session_meta carries base instructions of at least 1 KiB, which the importer
	// moves out of the frame into a path item (the corpus's own are shorter and stay in the frame).
	const baseThread = "44444444-4444-4444-8444-444444444444"
	baseText := "codex base instructions " + synth.Text(1500, 9)
	if len(baseText) < leafMin {
		t.Fatalf("base instructions of %d bytes stay in the frame", len(baseText))
	}
	cx := &synth.Codex{G: x.g, ThreadID: baseThread, Session: baseThread}
	var meta map[string]any
	if err := json.Unmarshal([]byte(cx.SessionMeta("openai", "", 0)), &meta); err != nil {
		t.Fatal(err)
	}
	meta["payload"].(map[string]any)["base_instructions"] = map[string]any{"text": baseText}
	metaLine, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	x.write(filepath.Join(x.codex(), "2026", "10", "02", "rollout-2026-10-02T09-00-00-"+baseThread+".jsonl"),
		string(metaLine), cx.UserMessage("base question"))
	x.mustRun()
	head := func(h wire.Hash) ([]byte, bool) {
		_, raw, ok := x.srv.Piece(h)
		return raw, ok
	}
	want := []struct {
		recordType string
		content    string // a substring of the piece
		role       canon.Role
	}{
		{"attachment", `"text":"system prompt `, canon.RoleSystem},
		{"attachment", `"text":"second system part"`, canon.RoleSystem},
		{"attachment", `"name":"Bash"`, canon.RoleToolSchema},
		{"attachment", `"name":"Read"`, canon.RoleToolSchema},
		{"api-request-shape", `"text":"shape system `, canon.RoleSystem},
		{"api-request-shape", `"name":"Bash"`, canon.RoleToolSchema},
		{"api-request-blob", "blob system prompt ", canon.RoleSystem},
		{"response_item/message", "developer instructions", canon.RoleSystem},
		{"response_item/message", "first question", canon.RoleText},
		{"response_item/additional_tools", `"name":"spawn_agent"`, canon.RoleToolSchema},
		{"response_item/additional_tools", `"name":"wait_agent"`, canon.RoleToolSchema},
		{"session_meta", "codex base instructions ", canon.RoleSystem},
		{"world_state", strings.SplitN(synth.Text(1500, 7), "\n", 2)[0], canon.RoleText}, // the corpus's large agents_md leaf
		{"user", "please ", canon.RoleText},
	}
	found := make([]bool, len(want))
	for _, rcv := range x.envelopes() {
		e := rcv.Envelope
		if e.Kind != wire.KindImportRecord {
			continue
		}
		rec := decodeRecord(t, e)
		roles, err := canon.PieceRoles(e.Kind, e.Body, nil, head)
		if err != nil {
			t.Fatalf("%s: %v", rec.RecordType, err)
		}
		for h, role := range roles {
			_, raw, ok := x.srv.Piece(h)
			if !ok {
				t.Fatalf("%s: piece %s not stored", rec.RecordType, h)
			}
			for i, w := range want {
				if rec.RecordType != w.recordType || !bytes.Contains(raw, []byte(w.content)) {
					continue
				}
				found[i] = true
				if role != w.role || role.IdxPolicy() != w.role.IdxPolicy() {
					t.Errorf("%s piece with %q: role %s (%s), want %s", w.recordType, w.content, role, role.IdxPolicy(), w.role)
				}
			}
		}
	}
	for i, w := range want {
		if !found[i] {
			t.Errorf("no %s piece with %q", w.recordType, w.content)
		}
	}
}
