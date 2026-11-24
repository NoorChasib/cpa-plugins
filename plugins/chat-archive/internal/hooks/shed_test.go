package hooks

import (
	"fmt"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// TestBodiesShedBeforeControlFull (review 2 of the round-4 fixes): with the stream goroutine blocked on a held writer
// (nothing takes Control) while the workers keep Handoff empty, mixed traffic (a full-body begin and response, retried
// attempts and a completion per exchange) fills Control. Bodies degrade to metadata only (body_budget) once Control is
// down to its metadata reserve, before any whole envelope is lost (control_full), and no full body is queued after
// that. Before, the hooks queued every body while Handoff had room, and the first loss was a completion.
func TestBodiesShedBeforeControlFull(t *testing.T) {
	e := running(t, "full")
	body := []byte(`{"model":"model-x","max_tokens":1,"messages":[{"role":"user","content":"synthetic text"}]}`)
	firstShed, firstLoss, bodiesAfterShed := -1, -1, 0
	for i := 0; firstLoss < 0 && i < ControlEntries; i++ {
		rid := fmt.Sprintf("mix-%05d", i)
		e.BeforeAuth(rpc(map[string]any{"RequestID": rid, "SourceFormat": "claude", "Model": "model-x", "Body": body}))
		for range 20 {
			e.AfterAuth(rpc(map[string]any{"RequestID": rid}))
		}
		e.Response(rpc(map[string]any{"RequestID": rid, "SourceFormat": "claude", "Model": "model-x", "StatusCode": 200,
			"Body": []byte(`{"id":"synthetic","type":"message"}`), "Metadata": map[string]any{"requested_model": "model-x"}}))
		e.Complete(rpc(map[string]any{"RequestID": rid, "Model": "model-x", "Metadata": map[string]any{"requested_model": "model-x"}}))
		for len(e.Handoff) > 0 { // the workers keep up
			w := <-e.Handoff
			if firstShed >= 0 {
				bodiesAfterShed++
			}
			if w.Begin != nil {
				e.Pool.Release(w.Begin.Reserved)
			}
			if w.Response != nil {
				e.Pool.Release(w.Response.Reserved)
			}
		}
		d := e.Stats.Dropped()
		if firstShed < 0 && d["body_budget"] > 0 {
			firstShed = i
		}
		if d["control_full"] > 0 || d["handoff_full"] > 0 {
			firstLoss = i
		}
	}
	if firstShed < 0 || firstLoss >= 0 && firstShed >= firstLoss {
		t.Fatalf("first body shed at exchange %d, first loss at %d (dropped %v): bodies must degrade first", firstShed, firstLoss, e.Stats.Dropped())
	}
	if bodiesAfterShed != 0 {
		t.Fatalf("%d full bodies queued after shedding began", bodiesAfterShed)
	}
	if firstLoss >= 0 && len(e.Control) != cap(e.Control) {
		t.Fatalf("an envelope was lost with Control at %d of %d", len(e.Control), cap(e.Control))
	}
	// The shed begins and responses are in Control as metadata only.
	var metaBegins, metaResponses int
	for len(e.Control) > 0 {
		c := <-e.Control
		if c.Begin != nil && c.Begin.MetaOnly == wire.DroppedBodyBudget {
			metaBegins++
		}
		if c.Response != nil && c.Response.MetaOnly == wire.DroppedBodyBudget {
			metaResponses++
		}
	}
	if metaBegins == 0 || metaResponses == 0 {
		t.Fatalf("metadata-only begins %d, responses %d in Control: want both", metaBegins, metaResponses)
	}
}

// TestWriteStallShedsBodies: while the capture runtime's write queue is full (SetWriteStall), a full-body begin or
// response keeps its metadata only (body_budget) even with Control and the content pool empty, and reserves nothing;
// once the writer drained the queue, bodies are queued again.
func TestWriteStallShedsBodies(t *testing.T) {
	e := running(t, "full")
	call := func(rid string) {
		e.BeforeAuth(rpc(map[string]any{"RequestID": rid, "SourceFormat": "claude", "Body": []byte(`{"messages":[]}`)}))
		e.Response(rpc(map[string]any{"RequestID": rid, "SourceFormat": "claude", "StatusCode": 200, "Body": []byte(`{"id":"x"}`),
			"Metadata": map[string]any{"requested_model": "model-x"}}))
	}
	e.SetWriteStall(true)
	call("stall-1")
	if d := e.Stats.Dropped()["body_budget"]; d != 2 || len(e.Handoff) != 0 || len(e.Control) != 2 || e.Pool.Used() != 0 {
		t.Fatalf("stalled: body_budget %d, Handoff %d, Control %d, pool %d; want 2, 0, 2, 0", d, len(e.Handoff), len(e.Control), e.Pool.Used())
	}
	e.SetWriteStall(false)
	call("stall-2")
	if d := e.Stats.Dropped()["body_budget"]; d != 2 || len(e.Handoff) != 2 {
		t.Fatalf("drained: body_budget %d, Handoff %d; want 2, 2", d, len(e.Handoff))
	}
}
