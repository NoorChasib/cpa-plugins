package capture

// K10 R1b (plugin 0.1.6): beyond the budgets, every shed path ends in a dropped.* count, never in a
// silent loss. Each test drives one of the worst cases the revision-5 and revision-6 reviews named.
// Every value is synthetic.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andybalholm/brotli"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/safego"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// TestBrotliWindowAtFullPoolCounted: a small Brotli response declaring a 16 MiB window decodes on an
// empty pool, but late in a stall, with the pool nearly full, the split's allowance falls under the
// window and the body is shed as body_budget; the response is still written, as metadata only.
func TestBrotliWindowAtFullPoolCounted(t *testing.T) {
	plain := []byte(`{"id":"msg_br","type":"message","role":"assistant","content":[{"type":"text","text":"synthetic brotli window"}]}`)
	var buf bytes.Buffer
	w := brotli.NewWriterOptions(&buf, brotli.WriterOptions{Quality: 5, LGWin: 24})
	w.Write(plain)
	w.Close()
	body := buf.Bytes()
	for _, c := range []struct {
		name    string
		used    int64
		dropped string
	}{{"empty_pool", 0, ""}, {"full_pool", hooks.PoolBytes - 40<<20, wire.DroppedBodyBudget}} {
		t.Run(c.name, func(t *testing.T) {
			rt, e := holdRuntime(t, HoldBytes, 1)
			if c.used > 0 && !e.Pool.TryReserve(c.used) {
				t.Fatal("pre-reserve")
			}
			need := int64(len(body)) * hooks.BodyReserveFactor
			e.Pool.TryReserve(need)
			r := &hooks.Response{RequestID: "br-1", SourceFormat: "claude", StatusCode: 200, Body: body, ContentType: "application/json",
				ContentEncoding: "br", ObservedAt: time.Now(), Reserved: need}
			rt.doResponse(r)
			got := <-rt.writes
			var er wire.ExchangeResponse
			if err := json.Unmarshal(got.env.Body, &er); err != nil {
				t.Fatal(err)
			}
			if er.Body.Dropped != c.dropped || e.Stats.Dropped()["body_budget"] != map[bool]uint64{true: 1, false: 0}[c.dropped != ""] {
				t.Fatalf("dropped %q, body_budget %d; want %q", er.Body.Dropped, e.Stats.Dropped()["body_budget"], c.dropped)
			}
			rt.finished(got)
			if e.Pool.Used() != c.used {
				t.Fatalf("pool %d after the write, want %d", e.Pool.Used(), c.used)
			}
		})
	}
}

// TestHoldFullShedsUpstreamCounted: with the writer stalled and a small hold budget full of
// metadata-only begins (each keeps a 10 KiB model name that no other budget charges), the stream
// goroutine blocks in rt.write, the Control queue fills, and later begins are counted control_full
// at the hooks. When the writer resumes, every queued begin is written: written plus counted equals
// offered, and the hold never passed its budget.
func TestHoldFullShedsUpstreamCounted(t *testing.T) {
	rt, e := holdRuntime(t, 2<<20, 1)
	gate := make(chan struct{})
	var blocked atomic.Bool
	rt.obWrite = func(ctx context.Context, envs []outbox.NewEnvelope) (outbox.WriteResult, error) {
		blocked.Store(true)
		<-gate
		return rt.opts.Outbox.Write(ctx, envs)
	}
	e.SetConfig(&hooks.Config{Configured: true, Capture: "metadata", MaxRPC: 8 << 20})
	e.Start()
	safego.Loop(rt.runCtx, &rt.wg, "worker", e.Stats.Panic, rt.worker)
	safego.Loop(rt.seqCtx, &rt.seqWG, "stream", e.Stats.Panic, rt.sequencer)
	safego.Loop(rt.writerCtx, &rt.writerWG, "writer", e.Stats.Panic, rt.writer)
	t.Cleanup(func() { rt.StopBy(time.Now().Add(2 * time.Second)) })
	n := hooks.ControlEntries + 600
	model := strings.Repeat("m", 10<<10)
	for i := 0; i < n; i++ {
		e.BeforeAuth(rpcJSON(map[string]any{"RequestID": fmt.Sprintf("full-%d", i), "SourceFormat": "claude", "Model": model}))
	}
	waitUntil(t, "the writer stalled", 2*time.Second, blocked.Load)
	if now, peak := rt.HoldBytes(); now > 2<<20 || peak > 2<<20 {
		t.Fatalf("hold %d (peak %d) over its 2 MiB budget", now, peak)
	}
	lost := int(e.Stats.Dropped()["control_full"])
	if lost == 0 {
		t.Fatal("no begin was counted with the writer stalled and the hold full")
	}
	close(gate)
	ob := rt.opts.Outbox
	var begins int
	waitUntil(t, "every begin written or counted", 30*time.Second, func() bool {
		begins = 0
		for _, k := range pendingKeys(t, ob) {
			if strings.HasSuffix(k, "/begin/0") {
				begins++
			}
		}
		return begins+lost == n
	})
	if d := e.Stats.Dropped(); d["disk_hard"] != 0 || d["stopped"] != 0 || d["hold_oversize"] != 0 || int(d["control_full"]) != lost {
		t.Fatalf("dropped %v", d)
	}
	t.Logf("offered %d metadata begins: written %d, control_full %d", n, begins, lost)
}

func rpcJSON(v map[string]any) []byte {
	b, _ := json.Marshal(v)
	return b
}
