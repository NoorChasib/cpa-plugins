package capture

// K22 (plugin 0.1.6): once a begin's or response's envelope is built, its content-pool reservation
// (4 x the raw body plus its decoded headers) is released down to the bytes the queued envelope
// retains, and the writer releases the rest once the envelope is written or dropped. These tests pin
// the accounting: each reservation is returned exactly once (the pool ends at 0, never below) on
// every path (written, retried then written, dropped, refused at a stop, cut at a stop), the hold
// still charges what a split kept beyond its reservation, and a body that does not fit the pool is
// still shed with its counter. Every value is synthetic.

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	sqlite "github.com/mattn/go-sqlite3"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
)

// synthText returns n bytes of letters and spaces from seed, distinct per seed so piece dedupe
// does not merge bodies.
func synthText(seed int64, n int) string {
	rng := rand.New(rand.NewSource(seed))
	const alphabet = "abcdefghijklmnopqrstuvwxyz     "
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[rng.Intn(len(alphabet))]
	}
	return string(b)
}

var trimHeaders = http.Header{"Content-Type": {"application/json"}}

// textBegin returns a Claude begin carrying size bytes of synthetic text, with its pool
// reservation taken as the hook takes it. ok is false when the pool refused it.
func textBegin(e *hooks.Engine, id string, seed int64, size int) (b *hooks.Begin, need int64, ok bool) {
	body, _ := json.Marshal(map[string]any{"model": "m", "max_tokens": 1, "messages": []any{map[string]any{"role": "user", "content": synthText(seed, size)}}})
	need = int64(len(body))*hooks.BodyReserveFactor + hooks.HandoffHeaderBytes(trimHeaders)
	if !e.Pool.TryReserve(need) {
		return nil, need, false
	}
	return &hooks.Begin{Req: protocol.BeforeAuth{RequestID: id, SourceFormat: "claude", Body: body, BodyLen: len(body), Headers: trimHeaders.Clone()},
		ObservedAt: time.Now(), Reserved: need}, need, true
}

// textResponse is textBegin for a non-stream Claude response.
func textResponse(e *hooks.Engine, id string, seed int64, size int) (r *hooks.Response, need int64, ok bool) {
	body, _ := json.Marshal(map[string]any{"id": "msg_" + id, "type": "message", "role": "assistant",
		"content": []map[string]any{{"type": "text", "text": synthText(seed, size)}}, "stop_reason": "end_turn"})
	need = int64(len(body))*hooks.BodyReserveFactor + hooks.HandoffHeaderBytes(trimHeaders)
	if !e.Pool.TryReserve(need) {
		return nil, need, false
	}
	return &hooks.Response{RequestID: id, SourceFormat: "claude", StatusCode: 200, Body: body, Headers: trimHeaders.Clone(),
		ContentType: "application/json", ObservedAt: time.Now(), Reserved: need}, need, true
}

// TestQueuedBodyKeepsRetainedBytes: with the writer held, a queued begin and a queued response
// each keep only their envelope's retained bytes in the content pool (well under the 4 x
// reservation), charge nothing to the hold, and return the rest when the writer finishes them. A
// begin whose split outgrew its reservation keeps the whole reservation, and the hold charges the
// bytes beyond it.
func TestQueuedBodyKeepsRetainedBytes(t *testing.T) {
	rt, e := holdRuntime(t, HoldBytes, 1)
	const size = 512 << 10
	check := func(kind string, need int64) {
		t.Helper()
		r := <-rt.writes
		retained := retainedBytes(r.env, 0)
		pool := e.Pool.Used()
		hold, _ := rt.HoldBytes()
		t.Logf("%s: reservation %d KiB, pool after hand-over %d KiB, retained %d KiB, hold %d", kind, need>>10, pool>>10, retained>>10, hold)
		if pool < retained || pool > retained+4096 || pool > need/3 {
			t.Fatalf("%s: pool %d after the hand-over, want the envelope's retained %d (plus its commit data), reservation %d", kind, pool, retained, need)
		}
		if hold != 0 || r.held != 0 {
			t.Fatalf("%s: hold %d (req %d), want 0", kind, hold, r.held)
		}
		rt.finished(r)
		if now, _ := rt.HoldBytes(); e.Pool.Used() != 0 || now != 0 {
			t.Fatalf("%s: pool %d, hold %d after the writer finished", kind, e.Pool.Used(), now)
		}
	}
	b, need, _ := textBegin(e, "trim-b", 1, size)
	rt.doBegin(b)
	check("begin", need)
	resp, need, _ := textResponse(e, "trim-r", 2, size)
	rt.doResponse(resp)
	check("response", need)

	// A gzip body that decodes to about 8 MiB from a few KiB: its envelope retains more than its
	// reservation, so the pool keeps the whole reservation and the hold charges the rest.
	plain, _ := json.Marshal(map[string]any{"model": "m", "max_tokens": 1, "messages": []any{map[string]any{"role": "user",
		"content": strings.Repeat("synthetic compressible text ", (8<<20)/28)}}})
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(plain)
	zw.Close()
	h := http.Header{"Content-Type": {"application/json"}, "Content-Encoding": {"gzip"}}
	need = int64(gz.Len())*hooks.BodyReserveFactor + hooks.HandoffHeaderBytes(h)
	if !e.Pool.TryReserve(need) {
		t.Fatal("reserve")
	}
	rt.doBegin(&hooks.Begin{Req: protocol.BeforeAuth{RequestID: "trim-gz", SourceFormat: "claude", Body: gz.Bytes(), BodyLen: gz.Len(), Headers: h},
		ObservedAt: time.Now(), Reserved: need})
	r := <-rt.writes
	hold, _ := rt.HoldBytes()
	if e.Pool.Used() != need || r.held != hold || hold < retainedBytes(r.env, 0)-need {
		t.Fatalf("over-reservation split: pool %d (reservation %d), hold %d (req %d), retained %d", e.Pool.Used(), need, hold, r.held, retainedBytes(r.env, 0))
	}
	rt.finished(r)
	if now, _ := rt.HoldBytes(); e.Pool.Used() != 0 || now != 0 {
		t.Fatalf("pool %d, hold %d after the writer finished", e.Pool.Used(), now)
	}
	if d := nonZero(e.Stats.Dropped()); len(d) != 0 {
		t.Fatalf("dropped %v", d)
	}
}

// offerBodies queues n begins and n responses of size bytes through the workers' code path, each
// reserving as the hooks do, and returns the reservations' total.
func offerBodies(t *testing.T, rt *Runtime, e *hooks.Engine, prefix string, n, size int) (total int64) {
	t.Helper()
	for i := 0; i < n; i++ {
		b, need, ok := textBegin(e, fmt.Sprintf("%s-%d", prefix, i), int64(2*i), size)
		if !ok {
			t.Fatalf("begin %d refused by the pool (used %d)", i, e.Pool.Used())
		}
		rt.doBegin(b)
		total += need
		r, need, ok := textResponse(e, fmt.Sprintf("%s-%d", prefix, i), int64(2*i+1), size)
		if !ok {
			t.Fatalf("response %d refused by the pool (used %d)", i, e.Pool.Used())
		}
		rt.doResponse(r)
		total += need
	}
	return total
}

func balanced(t *testing.T, rt *Runtime, e *hooks.Engine) {
	t.Helper()
	if now, _ := rt.HoldBytes(); e.Pool.Used() != 0 || now != 0 || rt.hold.waiting() != 0 {
		t.Fatalf("pool %d, hold %d, %d waiting: a reservation was not returned exactly once", e.Pool.Used(), now, rt.hold.waiting())
	}
}

// TestTrimBalancesAcrossWriteRetryShedStop: queued begins and responses whose reservations were
// trimmed return the rest exactly once whether the writer writes them, retries them and then writes
// them, drops them (a permanent error, a failure run past the retry window) or the stop cuts them
// (queued behind a stalled write, or refused while waiting for a full write channel). Each path ends
// with the pool and the hold at 0, and stored plus counted equals offered.
func TestTrimBalancesAcrossWriteRetryShedStop(t *testing.T) {
	const n, size = 8, 256 << 10
	const envs = 2 * n
	t.Run("write", func(t *testing.T) {
		rt, e, ob := writerRuntime(t, func(ctx context.Context, es []outbox.NewEnvelope, real func(context.Context, []outbox.NewEnvelope) (outbox.WriteResult, error)) (outbox.WriteResult, error) {
			return real(ctx, es)
		})
		offerBodies(t, rt, e, "w", n, size)
		waitUntil(t, "every envelope written", 10*time.Second, func() bool { return len(pendingKeys(t, ob)) == envs })
		waitUntil(t, "the budgets returned", 2*time.Second, func() bool { return e.Pool.Used() == 0 })
		balanced(t, rt, e)
		if d := nonZero(e.Stats.Dropped()); len(d) != 0 {
			t.Fatalf("dropped %v", d)
		}
	})
	t.Run("retry", func(t *testing.T) {
		fastRetry(t, 10*time.Second)
		var failing atomic.Bool
		var calls atomic.Int32
		failing.Store(true)
		rt, e, ob := writerRuntime(t, func(ctx context.Context, es []outbox.NewEnvelope, real func(context.Context, []outbox.NewEnvelope) (outbox.WriteResult, error)) (outbox.WriteResult, error) {
			calls.Add(1)
			if failing.Load() {
				return outbox.WriteResult{}, sqlite.Error{Code: sqlite.ErrIoErr}
			}
			return real(ctx, es)
		})
		total := offerBodies(t, rt, e, "rt", n, size)
		waitUntil(t, "retries", 5*time.Second, func() bool { return calls.Load() >= 3 })
		if pool := e.Pool.Used(); pool <= 0 || pool > total/3 {
			t.Fatalf("pool %d while the writes fail, want the queued envelopes' retained bytes (reservations %d)", pool, total)
		}
		failing.Store(false)
		waitUntil(t, "every envelope written", 10*time.Second, func() bool { return len(pendingKeys(t, ob)) == envs })
		waitUntil(t, "the budgets returned", 2*time.Second, func() bool { return e.Pool.Used() == 0 })
		balanced(t, rt, e)
		if d := nonZero(e.Stats.Dropped()); len(d) != 0 {
			t.Fatalf("dropped %v", d)
		}
	})
	for _, mode := range []string{"permanent", "window"} {
		t.Run(mode, func(t *testing.T) {
			fastRetry(t, 150*time.Millisecond)
			rt, e, _ := writerRuntime(t, func(ctx context.Context, es []outbox.NewEnvelope, real func(context.Context, []outbox.NewEnvelope) (outbox.WriteResult, error)) (outbox.WriteResult, error) {
				if mode == "permanent" {
					return outbox.WriteResult{}, sqlite.Error{Code: sqlite.ErrConstraint}
				}
				return outbox.WriteResult{}, sqlite.Error{Code: sqlite.ErrIoErr}
			})
			offerBodies(t, rt, e, mode, n, size)
			waitUntil(t, "every group dropped", 10*time.Second, func() bool { return e.Stats.Dropped()["disk_hard"] == envs })
			waitUntil(t, "the budgets returned", 2*time.Second, func() bool { return e.Pool.Used() == 0 })
			balanced(t, rt, e)
		})
	}
	t.Run("stop_cut", func(t *testing.T) {
		rt, e, ob := writerRuntime(t, func(ctx context.Context, _ []outbox.NewEnvelope, _ func(context.Context, []outbox.NewEnvelope) (outbox.WriteResult, error)) (outbox.WriteResult, error) {
			<-ctx.Done() // a stalled connection
			return outbox.WriteResult{}, ctx.Err()
		})
		offerBodies(t, rt, e, "cut", n, size)
		rt.StopBy(time.Now().Add(300 * time.Millisecond))
		if stored, got := len(pendingKeys(t, ob)), int(e.Stats.Dropped()["stopped"]); stored != 0 || got != envs {
			t.Fatalf("stored %d, stopped %d, want 0 and %d", stored, got, envs)
		}
		balanced(t, rt, e)
	})
	t.Run("stop_refused", func(t *testing.T) {
		// No writer and a 2-slot write channel: the producers after the second wait in rt.write
		// and are refused when the drain bound passes, returning their trimmed reservation.
		rt, e := holdRuntime(t, HoldBytes, 1)
		rt.writes = make(chan writeReq, 2)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			b, _, ok := textBegin(e, fmt.Sprintf("ref-%d", i), int64(i), size)
			if !ok {
				t.Fatal("reserve")
			}
			wg.Add(1)
			go func() { defer wg.Done(); rt.doBegin(b) }()
		}
		waitUntil(t, "the channel full", 5*time.Second, func() bool { return len(rt.writes) == 2 })
		time.Sleep(50 * time.Millisecond)
		rt.drainStop()
		wg.Wait()
		if got := e.Stats.Dropped()["stopped"]; got != n-2 {
			t.Fatalf("stopped %d, want %d", got, n-2)
		}
		for len(rt.writes) > 0 {
			rt.finished(<-rt.writes)
		}
		balanced(t, rt, e)
	})
}

// TestTrimmedPoolAdmitsMoreAndStillSheds: with the writer held and a 16 MiB pool, 1 MiB begins
// are admitted until the next one's 4 x reservation does not fit. Queued begins keep about their
// own size, so more than three times as many are queued as when each kept its whole reservation (3);
// the first that does not fit is shed by the hook with its counter (body_budget) and queued as
// metadata. Releasing the queued envelopes returns the pool to 0.
func TestTrimmedPoolAdmitsMoreAndStillSheds(t *testing.T) {
	rt, e := holdRuntime(t, HoldBytes, 1)
	e.Pool = hooks.NewPool(16<<20, 8<<20)
	e.SetConfig(&hooks.Config{Configured: true, Capture: "full", MaxRPC: protocol.MaxRPCDefault})
	e.Start()
	queued := 0
	for i := 0; i < 64 && e.Stats.Dropped()["body_budget"] == 0; i++ {
		body, _ := json.Marshal(map[string]any{"model": "m", "max_tokens": 1, "messages": []any{map[string]any{"role": "user", "content": synthText(int64(i), 1<<20)}}})
		raw, _ := json.Marshal(map[string]any{"RequestID": fmt.Sprintf("adm-%d", i), "SourceFormat": "claude", "Model": "m", "RequestedModel": "m",
			"Body": body, "Metadata": map[string]any{"request_path": "/v1/messages"}})
		e.BeforeAuth(raw)
		for len(e.Handoff) > 0 {
			rt.doWork(<-e.Handoff)
			queued++
		}
	}
	t.Logf("queued %d 1 MiB begins in a 16 MiB pool (pool %.1f MiB), then body_budget %d", queued, float64(e.Pool.Used())/(1<<20), e.Stats.Dropped()["body_budget"])
	if e.Stats.Dropped()["body_budget"] != 1 {
		t.Fatalf("dropped %v, want the begin that did not fit counted body_budget", e.Stats.Dropped())
	}
	if queued < 10 {
		t.Fatalf("%d begins queued before the pool refused one, want at least 10 (3 when each keeps 4 x its body)", queued)
	}
	if len(e.Control) != 1 {
		t.Fatalf("%d metadata begins on the control queue, want the shed one", len(e.Control))
	}
	if len(rt.writes) != queued {
		t.Fatalf("%d envelopes queued for %d begins", len(rt.writes), queued)
	}
	for len(rt.writes) > 0 {
		rt.finished(<-rt.writes)
	}
	balanced(t, rt, e)
}

// bigHeaderValue returns an n-byte header value whose backing array reports to freed once it is
// unreachable. Its bytes are synthetic letters.
func bigHeaderValue(n int, freed *atomic.Int32) string {
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = 'a' + byte(i%26)
	}
	runtime.AddCleanup(&buf[0], func(f *atomic.Int32) { f.Add(1) }, freed)
	return unsafe.String(&buf[0], n)
}

// blockedResponse builds a small non-stream response whose Content-Type and Content-Encoding header
// values are each n bytes, with the item's fields taken from the headers and its reservation taken as
// the response hook takes them. Nothing but the returned item references the values.
func blockedResponse(t *testing.T, e *hooks.Engine, n int, freed *atomic.Int32) (*hooks.Response, int64) {
	t.Helper()
	h := http.Header{"Content-Type": {bigHeaderValue(n, freed)}, "Content-Encoding": {bigHeaderValue(n, freed)}}
	body, _ := json.Marshal(map[string]any{"id": "msg_hdr", "type": "message", "role": "assistant",
		"content": []map[string]any{{"type": "text", "text": synthText(9, 1024)}}, "stop_reason": "end_turn"})
	need := int64(len(body))*hooks.BodyReserveFactor + hooks.HandoffHeaderBytes(h)
	if !e.Pool.TryReserve(need) {
		t.Fatal("reserve")
	}
	return &hooks.Response{RequestID: "hdr-r", SourceFormat: "claude", StatusCode: 200, Body: body, Headers: h,
		ContentType: hooks.HeaderGet(h, "Content-Type"), ContentEncoding: hooks.HeaderList(h, "Content-Encoding"),
		ObservedAt: time.Now(), Reserved: need}, need
}

// blockedBegin is blockedResponse for a begin.
func blockedBegin(t *testing.T, e *hooks.Engine, n int, freed *atomic.Int32) (*hooks.Begin, int64) {
	t.Helper()
	h := http.Header{"Content-Type": {bigHeaderValue(n, freed)}, "Content-Encoding": {bigHeaderValue(n, freed)}}
	body, _ := json.Marshal(map[string]any{"model": "m", "max_tokens": 1, "messages": []any{map[string]any{"role": "user", "content": synthText(10, 1024)}}})
	need := int64(len(body))*hooks.BodyReserveFactor + hooks.HandoffHeaderBytes(h)
	if !e.Pool.TryReserve(need) {
		t.Fatal("reserve")
	}
	return &hooks.Begin{Req: protocol.BeforeAuth{RequestID: "hdr-b", SourceFormat: "claude", Body: body, BodyLen: len(body), Headers: h},
		ObservedAt: time.Now(), Reserved: need}, need
}

// TestBlockedWorkerReleasesTrimmedHeaders: a worker waiting on a full write queue keeps its item
// reachable. Once the trim has returned the decoded headers' charge to the pool, the worker must not
// still hold those header strings: a response's ContentType and ContentEncoding are the header values
// themselves, so with 8 MiB values each one would keep 8 MiB live that neither the pool nor the hold
// counts. Every value is synthetic.
func TestBlockedWorkerReleasesTrimmedHeaders(t *testing.T) {
	const n = 8 << 20
	for _, kind := range []string{"response", "begin"} {
		t.Run(kind, func(t *testing.T) {
			rt, e := holdRuntime(t, HoldBytes, 1)
			rt.writes = make(chan writeReq) // no writer and no free slot: the worker waits in rt.write
			var freed atomic.Int32
			var need int64
			var item any // the test keeps the item reachable, as the waiting worker does
			done := make(chan struct{})
			if kind == "response" {
				r, nd := blockedResponse(t, e, n, &freed)
				item, need = r, nd
				go func() { defer close(done); rt.doResponse(r) }()
			} else {
				b, nd := blockedBegin(t, e, n, &freed)
				item, need = b, nd
				go func() { defer close(done); rt.doBegin(b) }()
			}
			waitUntil(t, "the reservation trimmed", 5*time.Second, func() bool { return e.Pool.Used() < need/4 })
			deadline := time.Now().Add(5 * time.Second)
			for freed.Load() < 2 && time.Now().Before(deadline) {
				runtime.GC()
				time.Sleep(10 * time.Millisecond)
			}
			hold, _ := rt.HoldBytes()
			t.Logf("%s: reservation %d KiB, pool while blocked %d KiB, hold %d KiB, header values freed %d of 2",
				kind, need>>10, e.Pool.Used()>>10, hold>>10, freed.Load())
			if freed.Load() != 2 {
				t.Fatalf("%s: %d of 2 header values freed while the worker waits with its pool charge returned (pool %d of %d)",
					kind, freed.Load(), e.Pool.Used(), need)
			}
			rt.finished(<-rt.writes)
			<-done
			runtime.KeepAlive(item)
			balanced(t, rt, e)
		})
	}
}
