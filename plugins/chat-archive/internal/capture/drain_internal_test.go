package capture

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/safego"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// stoppingRuntime is a runtime whose workers and stream goroutine are already gone (the drain
// bound passed), so StopBy drains the queues. withWriter starts the writer.
func stoppingRuntime(t *testing.T, withWriter bool) (*Runtime, *hooks.Engine, *outbox.Outbox) {
	t.Helper()
	e := hooks.NewEngine()
	ob, err := outbox.Open(outbox.Options{Dir: filepath.Join(t.TempDir(), "data"), CapBytes: 1 << 40, Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ob.Close() })
	m, _ := mask.New(mask.Config{})
	rt := &Runtime{opts: Options{Engine: e, Outbox: ob, Workers: 1, Masker: NewMaskerHolder(m, 0, 1<<20)}, prefix: NewPrefixMap()}
	rt.initContexts()
	if withWriter {
		rt.writes = make(chan writeReq, 4)
		safego.Loop(rt.writerCtx, &rt.writerWG, "writer", e.Stats.Panic, rt.writer)
	} else {
		rt.writes = make(chan writeReq) // nobody takes a write
	}
	return rt, e, ob
}

// queueDeferred queues a begin body for rid on Handoff and the exchange's completion, which
// waits for that body.
func queueDeferred(t *testing.T, e *hooks.Engine, rid string) {
	t.Helper()
	e.With(rid, true, func(en *hooks.Entry) { en.Pending = 1 })
	body := []byte(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"synthetic drain text"}]}`)
	e.Handoff <- hooks.Work{Begin: &hooks.Begin{Req: protocol.BeforeAuth{RequestID: rid, SourceFormat: "claude", Body: body, BodyLen: len(body)}, ObservedAt: time.Now()}}
	d := &hooks.Done{Req: protocol.Completion{RequestID: rid, Outcome: "succeeded"}, ObservedAt: time.Now()}
	if _, ready := e.TakeCompletion(d); ready {
		t.Fatal("the completion did not wait for the queued body")
	}
}

// TestDrainedBodyStillWritesCompletion: a begin body still queued when the drain bound passed is
// counted stopped, and the completion that waited for it is written (begin not written) while the
// writer runs, instead of being lost without a count.
func TestDrainedBodyStillWritesCompletion(t *testing.T) {
	rt, e, ob := stoppingRuntime(t, true)
	queueDeferred(t, e, "drain-c1")
	rt.StopBy(time.Now().Add(2 * time.Second))
	if n := e.Stats.Dropped()["stopped"]; n != 1 {
		t.Fatalf("dropped.stopped = %d, want 1 (the drained body)", n)
	}
	items, err := ob.Pending(context.Background(), time.Now(), 10, 1<<30)
	if err != nil || len(items) != 1 || items[0].Kind != wire.KindExchangeDone {
		t.Fatalf("pending = %d items, err %v", len(items), err)
	}
	var env wire.Envelope
	var c wire.ExchangeCompletion
	if err := json.Unmarshal(items[0].Envelope, &env); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(env.Body, &c); err != nil {
		t.Fatal(err)
	}
	if c.RequestID != "drain-c1" || c.BeginWritten || c.Responses != 0 || c.Outcome != "succeeded" {
		t.Fatalf("completion = request %q begin_written %v responses %d outcome %q", c.RequestID, c.BeginWritten, c.Responses, c.Outcome)
	}
	if e.With("drain-c1", false, func(*hooks.Entry) {}) {
		t.Fatal("the exchange's entry is still held")
	}
}

// TestDrainedBodyCompletionCountedWhenWriterBusy: when the writer does not take the completion by
// the stop's deadline, the completion is counted dropped.stopped too, so the loss is never silent.
func TestDrainedBodyCompletionCountedWhenWriterBusy(t *testing.T) {
	rt, e, ob := stoppingRuntime(t, false)
	queueDeferred(t, e, "drain-c2")
	start := time.Now()
	rt.StopBy(start.Add(200 * time.Millisecond))
	if d := time.Since(start); d > time.Second {
		t.Fatalf("StopBy took %v past a 200ms deadline", d)
	}
	if n := e.Stats.Dropped()["stopped"]; n != 2 {
		t.Fatalf("dropped.stopped = %d, want 2 (the drained body and its completion)", n)
	}
	if items, _ := ob.Pending(context.Background(), time.Now(), 10, 1<<30); len(items) != 0 {
		t.Fatalf("pending = %d", len(items))
	}
}
