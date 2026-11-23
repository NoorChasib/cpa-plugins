package capture

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
)

// TestBodyReservationHeldUntilWritten: a begin's content-pool reservation travels with its
// envelope to the writer and is released only after the outbox transaction, so a slow writer
// cannot hold pieces outside every budget while hooks admit new bodies into the freed pool.
func TestBodyReservationHeldUntilWritten(t *testing.T) {
	e := hooks.NewEngine()
	ob, err := outbox.Open(outbox.Options{Dir: filepath.Join(t.TempDir(), "data"), CapBytes: 1 << 40, Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer ob.Close()
	m, _ := mask.New(mask.Config{})
	rt := &Runtime{opts: Options{Engine: e, Outbox: ob, Workers: 1, Masker: NewMaskerHolder(m, 0, 1<<20)},
		prefix: NewPrefixMap(), writes: make(chan writeReq, 4)}
	rt.drainCtx, rt.drainStop = context.WithCancel(context.Background())
	defer rt.drainStop()

	body := []byte(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"synthetic budget text"}]}`)
	need := int64(len(body)) * hooks.BodyReserveFactor
	if !e.Pool.TryReserve(need) {
		t.Fatal("reserve")
	}
	b := &hooks.Begin{Req: protocol.BeforeAuth{RequestID: "budget-1", SourceFormat: "claude", Body: body, BodyLen: len(body)},
		ObservedAt: time.Now(), Reserved: need}
	rt.doBegin(b)
	if got := e.Pool.Used(); got != need {
		t.Fatalf("pool after the worker handed the begin over = %d, want %d (held until written)", got, need)
	}
	r := <-rt.writes
	rt.flush([]writeReq{r})
	if got := e.Pool.Used(); got != 0 {
		t.Fatalf("pool after the writer committed = %d", got)
	}
	if items, _ := ob.Pending(context.Background(), time.Now(), 10, 1<<30); len(items) != 1 {
		t.Fatalf("pending = %d", len(items))
	}
	// While write_unavailable is raised the writer still tries every group (0.1.4 dropped it untried
	// until a probe succeeded): the write is stored, the fault clears and the budget is released.
	e.Stats.SetFault("write_unavailable", true)
	e.Pool.TryReserve(100)
	env := r.env
	env.Key = "ex/budget-2/begin/0"
	rt.flush([]writeReq{{env: env, release: func() { e.Pool.Release(100) }}})
	if got := e.Pool.Used(); got != 0 {
		t.Fatalf("pool after a group written under write_unavailable = %d", got)
	}
	if items, _ := ob.Pending(context.Background(), time.Now(), 10, 1<<30); len(items) != 2 {
		t.Fatalf("pending = %d, want 2 (the group was tried and stored)", len(items))
	}
	if e.Stats.HasFault("write_unavailable") || e.Stats.DroppedTotal() != 0 {
		t.Fatalf("fault %v, dropped %v after a successful write", e.Stats.Faults(), e.Stats.Dropped())
	}
	// A group held at pressure level 3 (nothing is written there) until a stop's deadline drops it is released too.
	rt.obWrite = func(context.Context, []outbox.NewEnvelope) (outbox.WriteResult, error) {
		t.Fatal("written at pressure level 3")
		return outbox.WriteResult{}, nil
	}
	rt.opts.Outbox = hardPressure(t)
	e.Pool.TryReserve(100)
	env.Key = "ex/budget-3/begin/0"
	start := time.Now()
	rt.stopBy.Store(start.Add(100 * time.Millisecond).UnixNano())
	rt.flush([]writeReq{{env: env, release: func() { e.Pool.Release(100) }}})
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Fatalf("group dropped after %v at level 3, want it held until the stop's deadline", d)
	}
	if got := e.Pool.Used(); got != 0 {
		t.Fatalf("pool after a dropped group = %d", got)
	}
	if d := e.Stats.Dropped(); d["stopped"] != 1 || d["disk_hard"] != 0 {
		t.Fatalf("dropped = %v, want stopped 1", d)
	}
}
