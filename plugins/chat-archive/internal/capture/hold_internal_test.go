package capture

// The write path's hold budget (K10, plugin 0.1.6): FIFO admission bounded by a context, the
// 32 MiB per-envelope ceiling with its counted fallback, the stop with producers waiting on a full
// hold, and the retained-byte estimate against the heap each envelope kind keeps alive. Every value
// is synthetic.

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/safego"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/headers"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// TestHoldAdmissionFIFO: reservations are granted in arrival order; a later small one does not
// pass an earlier large one that does not fit yet; a waiter whose context ends leaves the queue and
// the one behind it is admitted when it fits; 0 bytes never wait.
func TestHoldAdmissionFIFO(t *testing.T) {
	h := newHoldBudget(100)
	if !h.acquire(context.Background(), 60) {
		t.Fatal("first reservation refused")
	}
	var order []int
	var mu sync.Mutex
	var wg sync.WaitGroup
	queued := 0
	start := func(id int, n int64, ctx context.Context) {
		wg.Add(1)
		queued++
		want := queued
		go func() {
			defer wg.Done()
			if h.acquire(ctx, n) {
				mu.Lock()
				order = append(order, id)
				mu.Unlock()
			}
		}()
		waitUntil(t, "the reservation queued", time.Second, func() bool { return h.waiting() >= want })
	}
	cctx, cancel := context.WithCancel(context.Background())
	start(1, 50, cctx)                 // does not fit (60 + 50 > 100): waits
	start(2, 10, context.Background()) // would fit, but waits behind 1
	if h.waiting() != 2 {
		t.Fatalf("%d waiting, want 2", h.waiting())
	}
	if !h.acquire(context.Background(), 0) {
		t.Fatal("a 0-byte reservation waited")
	}
	cancel() // 1 gives up; 2 now fits and is admitted
	waitUntil(t, "2 admitted", time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return len(order) == 1 })
	queued = 0
	start(3, 40, context.Background()) // 60 + 10 + 40 > 100: waits
	start(4, 5, context.Background())  // fits (60 + 10 + 5), but waits behind 3: FIFO
	time.Sleep(20 * time.Millisecond)
	if h.waiting() != 2 {
		t.Fatalf("%d waiting, want 3 and 4: a later reservation passed an earlier one", h.waiting())
	}
	h.release(60)
	wg.Wait()
	sort.Ints(order)
	if fmt.Sprint(order) != "[2 3 4]" {
		t.Fatalf("admitted %v, want 2, 3 and 4", order)
	}
	if now, peak := h.now.Load(), h.peak.Load(); now != 55 || peak != 70 {
		t.Fatalf("hold now %d peak %d, want 55 and 70", now, peak)
	}
}

// holdRuntime is a runtime whose writer is not running (nothing leaves the write channel) and
// whose hold budget is capacity bytes.
func holdRuntime(t *testing.T, capacity int64, workers int) (*Runtime, *hooks.Engine) {
	t.Helper()
	e := hooks.NewEngine()
	ob, err := outbox.Open(outbox.Options{Dir: filepath.Join(t.TempDir(), "data"), CapBytes: 1 << 40, Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ob.Close() })
	m, _ := mask.New(mask.Config{})
	rt := &Runtime{opts: Options{Engine: e, Outbox: ob, Workers: workers, Masker: NewMaskerHolder(m, 0, 1<<20)}, prefix: NewPrefixMap(),
		writes: make(chan writeReq, WriteQueueEntries), hold: newHoldBudget(capacity)}
	rt.initContexts()
	t.Cleanup(rt.drainStop)
	return rt, e
}

// TestHoldWriteOverCeilingCounted: an envelope that would hold more than HoldEnvelopeMax is never
// queued. It is counted hold_oversize, its carried budget is released, and the hold stays empty.
func TestHoldWriteOverCeilingCounted(t *testing.T) {
	rt, e := holdRuntime(t, HoldBytes, 1)
	env := outbox.NewEnvelope{Kind: wire.KindExchangeBegin, Key: "ex/oversize-1/begin/0", CreatedAt: time.Now(),
		Body: json.RawMessage(`{"headers":"` + strings.Repeat("h", 40<<20) + `"}`)}
	var released atomic.Bool
	if rt.write(env, nil, func() { released.Store(true) }, 1<<20, 0) {
		t.Fatal("an envelope over the ceiling was queued")
	}
	if e.Stats.Dropped()["hold_oversize"] != 1 || !released.Load() || len(rt.writes) != 0 {
		t.Fatalf("hold_oversize %d, released %v, queued %d", e.Stats.Dropped()["hold_oversize"], released.Load(), len(rt.writes))
	}
	if now, _ := rt.HoldBytes(); now != 0 {
		t.Fatalf("hold %d after a refused envelope", now)
	}
	// Just under the ceiling it is queued, alone in an otherwise empty hold.
	env.Body = json.RawMessage(`{"headers":"` + strings.Repeat("h", 30<<20) + `"}`)
	if !rt.write(env, nil, nil, 0, 0) {
		t.Fatal("an envelope under the ceiling was refused")
	}
	if now, _ := rt.HoldBytes(); now < 30<<20 || now > HoldEnvelopeMax {
		t.Fatalf("hold %d for a 30 MiB envelope", now)
	}
}

// TestHoldSplitOverCeilingShedsBody: a begin whose gzip body decodes to far more than its
// reservation (4 × the compressed size), past the ceiling, keeps its envelope as metadata only:
// the body is counted body_budget, nothing is counted hold_oversize, and the reservation is
// handed to the writer with the metadata envelope.
func TestHoldSplitOverCeilingShedsBody(t *testing.T) {
	rt, e := holdRuntime(t, HoldBytes, 1)
	text := strings.Repeat("synthetic highly compressible text ", (40<<20)/35)
	plain, _ := json.Marshal(map[string]any{"model": "m", "max_tokens": 1, "messages": []any{map[string]any{"role": "user", "content": text}}})
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(plain)
	zw.Close()
	body := gz.Bytes()
	need := int64(len(body)) * hooks.BodyReserveFactor
	if !e.Pool.TryReserve(need) {
		t.Fatal("reserve")
	}
	b := &hooks.Begin{Req: protocol.BeforeAuth{RequestID: "gz-1", SourceFormat: "claude", Body: body, BodyLen: len(body),
		Headers: http.Header{"Content-Type": {"application/json"}, "Content-Encoding": {"gzip"}}}, ObservedAt: time.Now(), Reserved: need}
	rt.doBegin(b)
	if d := e.Stats.Dropped(); d["body_budget"] != 1 || d["hold_oversize"] != 0 {
		t.Fatalf("dropped %v, want body_budget 1", d)
	}
	r := <-rt.writes
	var eb wire.ExchangeBegin
	if err := json.Unmarshal(r.env.Body, &eb); err != nil || eb.Body.Dropped != wire.DroppedBodyBudget || len(r.env.Pieces) != 0 {
		t.Fatalf("begin body dropped %q with %d pieces (%v), want metadata only", eb.Body.Dropped, len(r.env.Pieces), err)
	}
	if e.Pool.Used() != need {
		t.Fatalf("pool %d, want the reservation %d still held for the writer", e.Pool.Used(), need)
	}
	rt.finished(r)
	if e.Pool.Used() != 0 {
		t.Fatalf("pool %d after the writer finished", e.Pool.Used())
	}
}

// TestStopWhileHoldFull: with the writer blocked and the hold budget full, producers wait in
// rt.write. At the stop the waiting producers give up at the drain bound (stopped, budgets
// returned), the writer's queued envelopes are counted stopped at its cut, the hold returns to 0
// and stored plus counted equals offered. The stop stays within its deadline.
func TestStopWhileHoldFull(t *testing.T) {
	rt, e := holdRuntime(t, 1<<20, 1)
	rt.obWrite = func(ctx context.Context, _ []outbox.NewEnvelope) (outbox.WriteResult, error) {
		<-ctx.Done() // a stalled connection
		return outbox.WriteResult{}, ctx.Err()
	}
	safego.Loop(rt.writerCtx, &rt.writerWG, "writer", e.Stats.Panic, rt.writer)
	const producers, each = 8, 10
	var wg sync.WaitGroup
	var queued atomic.Int32
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				env := outbox.NewEnvelope{Kind: wire.KindExchangeDone, Key: fmt.Sprintf("ex/full-%d-%d/completion", p, i), CreatedAt: time.Now(),
					Body: json.RawMessage(`{"pad":"` + strings.Repeat("p", 200<<10) + `"}`)}
				if rt.write(env, nil, nil, 0, 0) {
					queued.Add(1)
				}
			}
		}(p)
	}
	waitUntil(t, "producers waiting on the full hold", 2*time.Second, func() bool { return rt.hold.waiting() > 0 })
	if now, _ := rt.HoldBytes(); now > 1<<20 {
		t.Fatalf("hold %d over its 1 MiB budget", now)
	}
	start := time.Now()
	rt.StopBy(time.Now().Add(400 * time.Millisecond))
	wg.Wait()
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("the stop took %v", d)
	}
	if got := e.Stats.Dropped()["stopped"]; got != producers*each {
		t.Fatalf("stopped %d, want every one of %d envelopes (none was stored)", got, producers*each)
	}
	if now, _ := rt.HoldBytes(); now != 0 || rt.hold.waiting() != 0 {
		t.Fatalf("hold %d with %d waiting after the stop", now, rt.hold.waiting())
	}
}

// TestWriteQueueSlotsBounded: the deeper write channel's slots are a fixed, stated cost.
func TestWriteQueueSlotsBounded(t *testing.T) {
	slot := unsafe.Sizeof(writeReq{})
	if total := slot * WriteQueueEntries; total > 3<<20 {
		t.Fatalf("write channel slots take %d bytes (%d each), over the stated 3 MiB", total, slot)
	}
	t.Logf("write channel: %d slots of %d bytes = %.2f MiB", WriteQueueEntries, slot, float64(slot*WriteQueueEntries)/(1<<20))
}

// TestHeldBytesCoversAllocation: for each envelope kind the heap an envelope keeps alive, measured
// after a GC with the envelopes referenced, is at most its retained-byte estimate (the hold's
// charge before the carried budget is subtracted).
func TestHeldBytesCoversAllocation(t *testing.T) {
	rt, e := holdRuntime(t, HoldBytes, 1)
	ms := rt.opts.Masker.Get()
	kept, hints, _ := func() ([]wire.Header, wire.ArchiveHints, error) {
		h := http.Header{}
		for i := 0; i < 40; i++ {
			h.Set(fmt.Sprintf("X-Synthetic-%d", i), strings.Repeat("v", 200))
		}
		k, hi, _ := headers.FilterRequest(h, ms.Masker, rt.opts.HMACKeyID, rt.opts.HMACKey)
		return k, hi, nil
	}()
	type built struct {
		env   outbox.NewEnvelope
		extra int64
	}
	kinds := map[string]func(i int) built{
		"begin_body": func(i int) built {
			parts := make([]any, 60)
			for j := range parts {
				parts[j] = map[string]any{"role": "user", "content": fmt.Sprintf("synthetic part %d of %d %s", j, i, strings.Repeat("x", 300))}
			}
			body, _ := json.Marshal(map[string]any{"model": "m", "max_tokens": 1, "messages": parts})
			b := &hooks.Begin{Req: protocol.BeforeAuth{RequestID: fmt.Sprintf("alloc-b-%d", i), SourceFormat: "claude", Body: body, BodyLen: len(body)}, ObservedAt: time.Now()}
			in := canon.Input{Format: CanonFormat(b.Req.SourceFormat), Body: b.Req.Body, ContentType: "application/json"}
			sr, err := rt.split(context.Background(), in, int64(len(body))*4, false)
			if err != nil || sr.sp == nil {
				t.Fatalf("split: %v %q", err, sr.dropped)
			}
			env, c, err := rt.buildBegin(b, sr, kept, hints)
			if err != nil {
				t.Fatal(err)
			}
			return built{env, c.bytes()}
		},
		"begin_metadata": func(i int) built {
			b := &hooks.Begin{Req: protocol.BeforeAuth{RequestID: fmt.Sprintf("alloc-m-%d", i), SourceFormat: "claude", BodyLen: 100}, ObservedAt: time.Now()}
			env, _, err := rt.buildBegin(b, splitResult{dropped: "metadata"}, kept, hints)
			if err != nil {
				t.Fatal(err)
			}
			return built{env, 0}
		},
		"attempt": func(i int) built {
			env, err := rt.buildAttempt(&hooks.Attempt{Req: protocol.AfterAuth{RequestID: fmt.Sprintf("alloc-a-%d", i), Model: "model-x"}, Ordinal: 1, ObservedAt: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			return built{env, 0}
		},
		"completion": func(i int) built {
			d := &hooks.Done{Req: protocol.Completion{RequestID: fmt.Sprintf("alloc-c-%d", i), Outcome: "failed", Error: strings.Repeat("e", 3000)}, ObservedAt: time.Now()}
			env, err := rt.buildCompletion(d, hooks.CompletionState{})
			if err != nil {
				t.Fatal(err)
			}
			return built{env, 0}
		},
		"usage": func(i int) built {
			u := &hooks.UsageItem{Rec: protocol.Usage{RequestID: fmt.Sprintf("alloc-u-%d", i), Model: "model-x", FailureBody: strings.Repeat("f", 4000),
				ResponseHeaders: map[string][]string{"Retry-After": {"1"}}}, ObservedAt: time.Now()}
			env, err := rt.buildUsage(context.Background(), u)
			if err != nil {
				t.Fatal(err)
			}
			return built{env, 0}
		},
	}
	_ = e
	const n = 400
	for name, build := range kinds {
		t.Run(name, func(t *testing.T) {
			build(0) // warm up lazily built tables
			out := make([]built, n)
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			for i := range out {
				out[i] = build(i)
			}
			runtime.GC()
			runtime.ReadMemStats(&after)
			var est int64
			for _, b := range out {
				est += retainedBytes(b.env, b.extra)
			}
			live := int64(after.HeapAlloc) - int64(before.HeapAlloc)
			runtime.KeepAlive(out)
			t.Logf("%s: live %d B per envelope, estimate %d B", name, live/n, est/n)
			if live > est {
				t.Fatalf("%s keeps %d B per envelope alive, over the estimate %d B", name, live/n, est/n)
			}
		})
	}
}

// speechResponse returns a non-stream speech response whose zlib (Content-Encoding deflate) body
// decodes to size bytes of synthetic binary audio, with its pool reservation taken as the hook
// takes it. Deflate declares no decoded size, so the decoder's buffer grows by doubling and ends
// with far more capacity than the decoded length.
func speechResponse(t *testing.T, e *hooks.Engine, id string, size int) *hooks.Response {
	t.Helper()
	unit := make([]byte, 4096)
	for i := range unit {
		unit[i] = byte(0x80 | (i*7+len(id))&0x7f) // never valid UTF-8, so the attachment is the decoded bytes
	}
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	for n := 0; n < size; n += len(unit) {
		zw.Write(unit[:min(len(unit), size-n)])
	}
	zw.Close()
	body := z.Bytes()
	need := int64(len(body)) * hooks.BodyReserveFactor
	if !e.Pool.TryReserve(need) {
		t.Fatal("reserve")
	}
	return &hooks.Response{RequestID: id, SourceFormat: "openai-speech", StatusCode: 200, Body: body,
		ContentType: "audio/mpeg", ContentEncoding: "deflate", ObservedAt: time.Now(), Reserved: need}
}

// liveAfter returns the heap f leaves reachable, measured across a GC before and after it.
func liveAfter(f func()) int64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.GC()
	runtime.ReadMemStats(&after)
	return int64(after.HeapAlloc) - int64(before.HeapAlloc)
}

func nonZero(d map[string]uint64) map[string]uint64 {
	out := map[string]uint64{}
	for k, v := range d {
		if v != 0 {
			out[k] = v
		}
	}
	return out
}

func queuedPieceBytes(rt *Runtime) (pieces, n int) {
	for i := len(rt.writes); i > 0; i-- {
		r := <-rt.writes
		for _, p := range r.env.Pieces {
			n += len(p.Bytes)
			pieces++
		}
		rt.writes <- r
	}
	return pieces, n
}

// TestHoldChargesDecodedBuffers: with the writer held, three deflate speech responses that each
// decode to 17 MiB are queued through the response builder. What they keep alive, beyond their pool
// reservations, is at most what the hold charged them, so the hold budget bounds the queued bytes
// even when the decoder's buffer is far larger than the body it decoded.
func TestHoldChargesDecodedBuffers(t *testing.T) {
	rt, e := holdRuntime(t, HoldBytes, 1)
	const n, size = 3, 17 << 20
	live := liveAfter(func() {
		for i := 0; i < n; i++ {
			rt.doResponse(speechResponse(t, e, fmt.Sprintf("speech-%d", i), size))
		}
	})
	if d := nonZero(e.Stats.Dropped()); len(d) != 0 {
		t.Fatalf("dropped %v, want none", d)
	}
	if pieces, b := queuedPieceBytes(rt); len(rt.writes) != n || pieces != n || b != n*size {
		t.Fatalf("%d queued with %d pieces of %d bytes, want %d of %d bytes", len(rt.writes), pieces, b, n, size)
	}
	hold, _ := rt.HoldBytes()
	pool := e.Pool.Used()
	t.Logf("live %.2f MiB, hold %.2f MiB, pool %.2f MiB", float64(live)/(1<<20), float64(hold)/(1<<20), float64(pool)/(1<<20))
	if hold > HoldBytes || live-pool > hold {
		t.Fatalf("queued responses keep %d bytes beyond their pool reservations, the hold charged %d (budget %d)", live-pool, hold, int64(HoldBytes))
	}
}

// imageBegin returns a gzip-encoded chat begin whose one user message carries an image as a
// base64 data URL that decodes to size bytes of synthetic binary, with its pool reservation taken
// as the hook takes it. The MIME type is lowercase, so the splitter's normalized type is a slice of
// the decoded URL string unless the queue copies it.
func imageBegin(t *testing.T, e *hooks.Engine, id string, size int) *hooks.Begin {
	t.Helper()
	img := make([]byte, size)
	for i := range img {
		img[i] = byte(0x80 | (i*7+len(id))&0x7f) // never valid UTF-8
	}
	url := "data:image/png;base64," + base64.StdEncoding.EncodeToString(img)
	body, _ := json.Marshal(map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "synthetic " + id},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}},
	}}}})
	var z bytes.Buffer
	zw := gzip.NewWriter(&z)
	zw.Write(body)
	zw.Close()
	gz := z.Bytes()
	h := map[string][]string{"Content-Type": {"application/json"}, "Content-Encoding": {"gzip"}}
	need := int64(len(gz))*hooks.BodyReserveFactor + hooks.HandoffHeaderBytes(h)
	if !e.Pool.TryReserve(need) {
		t.Fatal("reserve")
	}
	return &hooks.Begin{Req: protocol.BeforeAuth{RequestID: id, SourceFormat: "openai", Body: gz, BodyLen: len(gz), Headers: h},
		ObservedAt: time.Now(), Reserved: need}
}

// TestHoldChargesDataURLPieces: with the writer held, seven chat begins whose images come from
// 6 MiB data URLs are queued through the begin builder. A data URL's MIME type is parsed out of the
// whole decoded URL string, so a queued piece that kept that type uncopied would keep the URL alive
// too. What the begins keep alive beyond their pool reservations is at most what the hold charged.
func TestHoldChargesDataURLPieces(t *testing.T) {
	rt, e := holdRuntime(t, HoldBytes, 1)
	const n, size = 7, 6 << 20
	live := liveAfter(func() {
		for i := 0; i < n; i++ {
			rt.doBegin(imageBegin(t, e, fmt.Sprintf("image-%d", i), size))
		}
	})
	if d := nonZero(e.Stats.Dropped()); len(d) != 0 {
		t.Fatalf("dropped %v, want none", d)
	}
	if _, b := queuedPieceBytes(rt); len(rt.writes) != n || b < n*size {
		t.Fatalf("%d queued with %d piece bytes, want %d with at least %d", len(rt.writes), b, n, n*size)
	}
	hold, _ := rt.HoldBytes()
	pool := e.Pool.Used()
	t.Logf("live %.2f MiB, hold %.2f MiB, pool %.2f MiB", float64(live)/(1<<20), float64(hold)/(1<<20), float64(pool)/(1<<20))
	if hold > HoldBytes || live-pool > hold {
		t.Fatalf("queued begins keep %d bytes beyond their pool reservations, the hold charged %d (budget %d)", live-pool, hold, int64(HoldBytes))
	}
}

// TestHoldSpeechCeiling: at the per-envelope ceiling, a deflate speech response whose decoded
// body would hold more than HoldEnvelopeMax is written as metadata only (body_budget, never
// hold_oversize) with its reservation handed to the writer; one just under it is queued with its
// body, and what it keeps alive beyond its reservation stays within its charge.
func TestHoldSpeechCeiling(t *testing.T) {
	rt, e := holdRuntime(t, HoldBytes, 1)
	over := speechResponse(t, e, "speech-over", 30<<20)
	rt.doResponse(over)
	if d := e.Stats.Dropped(); d["body_budget"] != 1 || d["hold_oversize"] != 0 {
		t.Fatalf("dropped %v, want body_budget 1", d)
	}
	r := <-rt.writes
	var er wire.ExchangeResponse
	if err := json.Unmarshal(r.env.Body, &er); err != nil || er.Body.Dropped != wire.DroppedBodyBudget || len(r.env.Pieces) != 0 {
		t.Fatalf("response body dropped %q with %d pieces (%v), want metadata only", er.Body.Dropped, len(r.env.Pieces), err)
	}
	if e.Pool.Used() != over.Reserved && over.Reserved > 0 {
		t.Fatalf("pool %d, want the reservation %d handed to the writer", e.Pool.Used(), over.Reserved)
	}
	rt.finished(r)
	if now, _ := rt.HoldBytes(); now != 0 || e.Pool.Used() != 0 {
		t.Fatalf("hold %d, pool %d after the writer finished", now, e.Pool.Used())
	}

	const size = 27 << 20
	live := liveAfter(func() { rt.doResponse(speechResponse(t, e, "speech-under", size)) })
	if d := nonZero(e.Stats.Dropped()); d["body_budget"] != 1 || len(d) != 1 {
		t.Fatalf("dropped %v, want only the earlier body_budget", d)
	}
	if pieces, b := queuedPieceBytes(rt); pieces != 1 || b != size {
		t.Fatalf("%d pieces of %d bytes queued, want the %d-byte body", pieces, b, size)
	}
	hold, _ := rt.HoldBytes()
	pool := e.Pool.Used()
	t.Logf("under the ceiling: live %.2f MiB, hold %.2f MiB, pool %.2f MiB", float64(live)/(1<<20), float64(hold)/(1<<20), float64(pool)/(1<<20))
	if hold > HoldEnvelopeMax || live-pool > hold {
		t.Fatalf("a queued response keeps %d bytes beyond its reservation, the hold charged %d (ceiling %d)", live-pool, hold, int64(HoldEnvelopeMax))
	}
}
