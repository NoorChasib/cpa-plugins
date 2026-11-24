package hooks

import (
	"fmt"
	"strings"
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

// TestUsageShedsBodiesBeforeQueueFull (review 3 of the round-4 fixes): with the writer held (nothing takes Usage), usage
// failures with 1 KiB failure bodies fill the 16,384 usage slots while staying far below the 32 MiB byte budget.
// Failure bodies degrade (body_budget) once Usage is down to its metadata reserve, before any whole record is lost
// (usage_queue), and no record queued after that keeps its body. Before, every slot took a body and the next record
// was dropped whole with no body shed.
func TestUsageShedsBodiesBeforeQueueFull(t *testing.T) {
	e := running(t, "full")
	raw := rpc(map[string]any{"RequestID": "u-fill", "Provider": "claude", "Model": "model-x",
		"Failure": map[string]any{"Body": strings.Repeat("f", 1<<10)}})
	firstShed, firstLoss := -1, -1
	for i := 0; firstLoss < 0 && i < UsageQueueEntries+10; i++ {
		e.UsageHook(raw)
		d := e.Stats.Dropped()
		if firstShed < 0 && d["body_budget"] > 0 {
			firstShed = i
		}
		if d["usage_queue"] > 0 {
			firstLoss = i
		}
	}
	if b := e.usageBytes.Load(); b > UsageQueueBytes-(1<<20) {
		t.Fatalf("usage reservation %d: the test must stay below the byte budget", b)
	}
	if firstShed < 0 || firstLoss < 0 || firstShed >= firstLoss {
		t.Fatalf("first failure body shed at record %d, first whole loss at %d (dropped %v): bodies must degrade first", firstShed, firstLoss, e.Stats.Dropped())
	}
	if firstShed != UsageQueueEntries-UsageReserve {
		t.Fatalf("first shed at record %d, want %d (the metadata reserve)", firstShed, UsageQueueEntries-UsageReserve)
	}
	withBody := 0
	for i := 0; len(e.Usage) > 0; i++ {
		u := <-e.Usage
		if u.Rec.FailureBody != "" {
			withBody++
		}
		e.UsageDequeued(u.Bytes)
	}
	if withBody != UsageQueueEntries-UsageReserve {
		t.Fatalf("%d queued records kept their failure body, want %d", withBody, UsageQueueEntries-UsageReserve)
	}
	// While the write queue is full, a failure body is shed with Usage empty.
	e.SetWriteStall(true)
	before := e.Stats.Dropped()["body_budget"]
	e.UsageHook(raw)
	if u := <-e.Usage; u.Rec.FailureBody != "" || e.Stats.Dropped()["body_budget"] != before+1 {
		t.Fatalf("stalled: failure body kept %v, body_budget +%d", u.Rec.FailureBody != "", e.Stats.Dropped()["body_budget"]-before)
	}
}

// TestStreamChunksShedBeforeHeaderInit (review 3 of the round-4 fixes): with the stream goroutine held (nothing takes
// Stream), payload chunks stop being queued once Stream is down to the part kept for header-inits: they are shed
// (body_budget) and their stream marked truncated, and a later stream's header-init is still queued. While the write
// queue is full a payload chunk is shed with Stream empty. Before, payload chunks filled Stream and the next
// header-init was lost (stream_overflow).
func TestStreamChunksShedBeforeHeaderInit(t *testing.T) {
	e := running(t, "full")
	chunk := func(rid string, idx int) []byte {
		m := map[string]any{"RequestID": rid, "SourceFormat": "openai-response", "Model": "model-x", "ChunkIndex": idx}
		if idx < 0 {
			m["ResponseHeaders"] = map[string][]string{"Content-Type": {"text/event-stream"}}
		} else {
			m["Body"] = []byte("data: {}\n\n")
		}
		return rpc(m)
	}
	for i := 0; i < StreamEntries; i++ {
		e.StreamChunk(chunk("s-fill", i))
	}
	if n := len(e.Stream); n != StreamEntries-StreamReserve {
		t.Fatalf("Stream holds %d payload chunks, want %d", n, StreamEntries-StreamReserve)
	}
	d := e.Stats.Dropped()
	if d["body_budget"] != StreamReserve || d["stream_overflow"] != 0 {
		t.Fatalf("dropped %v, want body_budget %d and no stream_overflow", d, StreamReserve)
	}
	truncated := false
	e.With("s-fill", false, func(en *Entry) { truncated = en.Truncated && en.StreamShed })
	if !truncated {
		t.Fatal("the shed stream is not marked truncated")
	}
	e.StreamChunk(chunk("s-new", -1))
	if n := len(e.Stream); n != StreamEntries-StreamReserve+1 || e.Stats.Dropped()["stream_overflow"] != 0 {
		t.Fatalf("header-init not queued: Stream %d, dropped %v", n, e.Stats.Dropped())
	}
	for len(e.Stream) > 0 {
		it := <-e.Stream
		e.Pool.ReleaseStream(it.Reserved)
	}
	e.SetWriteStall(true)
	e.StreamChunk(chunk("s-stall", -1))
	before := e.Stats.Dropped()["body_budget"]
	e.StreamChunk(chunk("s-stall", 0))
	if n := len(e.Stream); n != 1 || e.Stats.Dropped()["body_budget"] != before+1 {
		t.Fatalf("stalled: Stream %d (header-init only), body_budget +%d; want 1, +1", n, e.Stats.Dropped()["body_budget"]-before)
	}
}
