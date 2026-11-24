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
	var probe time.Time
	rt.flush([]writeReq{r}, &probe)
	if got := e.Pool.Used(); got != 0 {
		t.Fatalf("pool after the writer committed = %d", got)
	}
	if items, _ := ob.Pending(context.Background(), time.Now(), 10, 1<<30); len(items) != 1 {
		t.Fatalf("pending = %d", len(items))
	}
	// A request the writer drops (write_unavailable inside the probe interval) is released too.
	e.Stats.SetFault("write_unavailable", true)
	probe = time.Now()
	e.Pool.TryReserve(100)
	rt.flush([]writeReq{{env: r.env, release: func() { e.Pool.Release(100) }}}, &probe)
	if got := e.Pool.Used(); got != 0 {
		t.Fatalf("pool after a dropped group = %d", got)
	}
}
