package capture

// Opt-in measurement (CA_SERVICE_MEASURE=1) of the service the producers before the writer give a
// W-sized burst: the two workers for body-bearing before-auths, and the stream goroutine for stream
// events plus control items. The writer is replaced by a sink that records when each envelope is
// handed to it and returns its budgets at once, so the figures are the producers' own. It reports
// counts and durations only; every value is synthetic.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/safego"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
)

// svcRig runs the hooks, two workers and the stream goroutine against a sink in place of the writer.
type svcRig struct {
	e    *hooks.Engine
	rt   *Runtime
	mu   sync.Mutex
	outs []time.Time // when each envelope reached the sink
	stop chan struct{}
	done sync.WaitGroup
}

func newSvcRig(t *testing.T, workers int) *svcRig {
	t.Helper()
	e := hooks.NewEngine()
	ob, err := outbox.Open(outbox.Options{Dir: filepath.Join(t.TempDir(), "data"), CapBytes: 1 << 40, Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ob.Close() })
	m, _ := mask.New(mask.Config{})
	rt := &Runtime{opts: Options{Engine: e, Outbox: ob, Workers: workers, Masker: NewMaskerHolder(m, 0, 1<<20)}, prefix: NewPrefixMap(),
		writes: make(chan writeReq, 1024)}
	rt.initContexts()
	s := &svcRig{e: e, rt: rt, stop: make(chan struct{})}
	s.done.Add(1)
	go func() {
		defer s.done.Done()
		for {
			select {
			case w := <-rt.writes:
				s.mu.Lock()
				s.outs = append(s.outs, time.Now())
				s.mu.Unlock()
				if w.release != nil {
					w.release()
				}
				if w.commit != nil {
					w.commit(true)
				}
			case <-s.stop:
				return
			}
		}
	}()
	for i := 0; i < workers; i++ {
		safego.Loop(rt.runCtx, &rt.wg, "worker", e.Stats.Panic, rt.worker)
	}
	safego.Loop(rt.seqCtx, &rt.seqWG, "stream", e.Stats.Panic, rt.sequencer)
	t.Cleanup(func() {
		rt.seqCancel()
		rt.seqWG.Wait()
		rt.runCancel()
		rt.wg.Wait()
		rt.drainStop()
		close(s.stop)
		s.done.Wait()
	})
	e.SetConfig(&hooks.Config{Configured: true, Capture: "full", MaxRPC: protocol.MaxRPCDefault})
	e.Start()
	return s
}

func (s *svcRig) out() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.outs...)
}

// waitOut waits until n envelopes reached the sink.
func (s *svcRig) waitOut(t *testing.T, n int, within time.Duration) []time.Time {
	t.Helper()
	end := time.Now().Add(within)
	for time.Now().Before(end) {
		if o := s.out(); len(o) >= n {
			return o
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("only %d of %d envelopes reached the sink within %v", len(s.out()), n, within)
	return nil
}

// svcText is synthetic request text of about n bytes, varied per item so the prefix map and masker
// do real work.
func svcText(i, n int) string {
	var b strings.Builder
	for b.Len() < n {
		fmt.Fprintf(&b, "synthetic item %d line %d of measured text with assorted words ", i, b.Len())
	}
	return b.String()[:n]
}

func svcBeforeAuth(rid, text string) []byte {
	return rawJSON(map[string]any{"RequestID": rid, "SourceFormat": "openai-response", "Model": "model-x", "RequestedModel": "model-x",
		"Body": []byte(`{"type":"response.create","model":"model-x","input":"` + text + `"}`), "Metadata": rigMeta})
}

// svcReport is the service a producer gave a burst offered at t0: the time to the first output, the
// time to the last, and the largest lag of the k-th output behind the k-th offer.
type svcReport struct {
	n                       int
	first, last, maxLag     time.Duration
	minRate500ms, minRate1s float64
}

func (r svcReport) String() string {
	rate := func(v float64) string {
		if v < 0 {
			return "n/a" // the busy period is shorter than the window
		}
		return fmt.Sprintf("%.0f/s", v)
	}
	return fmt.Sprintf("n=%d first=%v last=%v max_lag=%v mean_rate=%.0f/s min_rate{0.5s=%s,1s=%s}", r.n,
		r.first.Round(time.Millisecond), r.last.Round(time.Millisecond), r.maxLag.Round(time.Millisecond),
		float64(r.n)/max(r.last.Seconds(), 1e-9), rate(r.minRate500ms), rate(r.minRate1s))
}

// svcMeasure compares the offer times in (ascending) with the output times out (ascending, same
// length): FIFO order is assumed for the lag, which two workers can bend by one item at most.
func svcMeasure(in, out []time.Time) svcReport {
	r := svcReport{n: len(out)}
	t0 := in[0]
	r.first, r.last = out[0].Sub(t0), out[len(out)-1].Sub(t0)
	for k := range out {
		if lag := out[k].Sub(in[k]); lag > r.maxLag {
			r.maxLag = lag
		}
	}
	// The lowest rate over a window that starts at an output while outputs remain (busy period).
	minRate := func(w time.Duration) float64 {
		best := -1.0
		for i := range out {
			end := out[i].Add(w)
			if end.After(out[len(out)-1]) {
				break
			}
			n := 0
			for j := i + 1; j < len(out) && !out[j].After(end); j++ {
				n++
			}
			if rate := float64(n) / w.Seconds(); best < 0 || rate < best {
				best = rate
			}
		}
		return best
	}
	r.minRate500ms, r.minRate1s = minRate(500*time.Millisecond), minRate(time.Second)
	return r
}

func TestServiceMeasureWorkersBurst(t *testing.T) {
	if os.Getenv("CA_SERVICE_MEASURE") != "1" {
		t.Skip("set CA_SERVICE_MEASURE=1")
	}
	for _, c := range []struct{ kib, n int }{{16, 200}, {32, 200}, {64, 200}, {4096, 1}, {1024, 4}} {
		kib, n := c.kib, c.n
		t.Run(fmt.Sprintf("%dx%dKiB", n, kib), func(t *testing.T) {
			s := newSvcRig(t, 2)
			// n items at once: W's Handoff burst of 200, or W's 4 MiB byte burst as one or four bodies.
			raws := make([][]byte, n)
			for i := range raws {
				raws[i] = svcBeforeAuth(fmt.Sprintf("svc-w-%d", i), svcText(i, kib<<10))
			}
			in := make([]time.Time, n)
			var wg sync.WaitGroup
			var mu sync.Mutex
			next := 0
			start := time.Now()
			for g := 0; g < 16; g++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						mu.Lock()
						i := next
						next++
						mu.Unlock()
						if i >= n {
							return
						}
						s.e.BeforeAuth(raws[i])
					}
				}()
			}
			wg.Wait()
			offered := time.Since(start)
			for i := range in {
				in[i] = start // one burst: every item offered by the time the last call returned
			}
			out := s.waitOut(t, n, time.Minute)
			if d := s.e.Stats.DroppedTotal(); d != 0 {
				t.Fatalf("dropped %d during the burst", d)
			}
			t.Logf("workers burst %d x %d KiB: offered in %v; %v", n, kib, offered.Round(time.Millisecond), svcMeasure(in, out))
		})
	}
}

func TestServiceMeasureWorkersSteady(t *testing.T) {
	if os.Getenv("CA_SERVICE_MEASURE") != "1" {
		t.Skip("set CA_SERVICE_MEASURE=1")
	}
	// W's body rate: 20 items/s at 25 KiB is 0.5 MiB/s, for 10 s.
	s := newSvcRig(t, 2)
	const n, every = 200, 50 * time.Millisecond
	in := make([]time.Time, n)
	start := time.Now()
	for i := 0; i < n; i++ {
		if d := time.Until(start.Add(time.Duration(i) * every)); d > 0 {
			time.Sleep(d)
		}
		in[i] = time.Now()
		s.e.BeforeAuth(svcBeforeAuth(fmt.Sprintf("svc-s-%d", i), svcText(i, 25<<10)))
	}
	out := s.waitOut(t, n, time.Minute)
	t.Logf("workers steady 20/s x 25 KiB: %v", svcMeasure(in, out))
}

func TestServiceMeasureStreamGoroutineBurst(t *testing.T) {
	if os.Getenv("CA_SERVICE_MEASURE") != "1" {
		t.Skip("set CA_SERVICE_MEASURE=1")
	}
	// W's stream and control bursts at one instant: 1,000 stream events (50 streams: a header-init,
	// 18 one-KiB item events and a terminal event) and 400 control items (after-auths), from 16
	// goroutines. The stream goroutine is a single consumer, so the length of its two queues over
	// time is its service.
	s := newSvcRig(t, 2)
	type call struct {
		stream bool
		raw    []byte
	}
	var calls []call
	for k := 0; k < 50; k++ {
		rid := fmt.Sprintf("svc-st-%d", k)
		chunk := func(i int, body string) []byte {
			m := map[string]any{"RequestID": rid, "SourceFormat": "openai-response", "Model": "model-x", "RequestedModel": "model-x",
				"ChunkIndex": i, "Metadata": rigMeta}
			if i < 0 {
				m["ResponseHeaders"] = map[string][]string{"Content-Type": {"text/event-stream"}}
			} else {
				m["Body"] = []byte(body)
			}
			return rawJSON(m)
		}
		calls = append(calls, call{true, chunk(-1, "")})
		calls = append(calls, call{true, chunk(0, "data: "+evCreated("resp_"+rid)+"\n\n")})
		for i := 1; i <= 17; i++ {
			calls = append(calls, call{true, chunk(i, "data: "+evItem(svcText(k*100+i, 1<<10))+"\n\n")})
		}
		calls = append(calls, call{true, chunk(18, "data: "+evDone("resp_"+rid)+"\n\n")})
	}
	for i := 0; i < 400; i++ {
		calls = append(calls, call{false, rawJSON(map[string]any{"RequestID": fmt.Sprintf("svc-c-%d", i), "ToFormat": "codex", "Model": "model-x",
			"Metadata": rigMeta})})
	}
	type sample struct {
		at time.Time
		q  int
	}
	var samples []sample
	stopS := make(chan struct{})
	var sw sync.WaitGroup
	sw.Add(1)
	go func() {
		defer sw.Done()
		for {
			samples = append(samples, sample{time.Now(), len(s.e.Stream) + len(s.e.Control)})
			select {
			case <-stopS:
				return
			default:
				time.Sleep(200 * time.Microsecond)
			}
		}
	}()
	var wg sync.WaitGroup
	start := time.Now()
	// Stream chunks of one RequestID are offered in order by one goroutine (CPA's order); the
	// 16 goroutines interleave the streams and the control items.
	order := make(chan []call, 450)
	for k := 0; k < 50; k++ {
		order <- calls[k*20 : k*20+20]
	}
	for i := 1000; i < len(calls); i++ {
		order <- calls[i : i+1]
	}
	close(order)
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for cs := range order {
				for _, c := range cs {
					if c.stream {
						s.e.StreamChunk(c.raw)
					} else {
						s.e.AfterAuth(c.raw)
					}
				}
			}
		}()
	}
	wg.Wait()
	offered := time.Since(start)
	// 400 attempts plus 50 reassembled responses reach the sink.
	out := s.waitOut(t, 450, time.Minute)
	end := time.Now()
	for len(s.e.Stream)+len(s.e.Control) > 0 && time.Since(end) < 10*time.Second {
		time.Sleep(time.Millisecond)
	}
	close(stopS)
	sw.Wait()
	if d := s.e.Stats.DroppedTotal(); d != 0 {
		t.Fatalf("dropped %d during the burst", d)
	}
	peak, emptyAt := 0, time.Duration(0)
	for _, sm := range samples {
		peak = max(peak, sm.q)
	}
	for i := len(samples) - 1; i >= 0; i-- {
		if samples[i].q != 0 {
			if i+1 < len(samples) {
				emptyAt = samples[i+1].at.Sub(start)
			}
			break
		}
	}
	t.Logf("stream goroutine burst 1000 events + 400 control: offered in %v; queue peak %d, empty at %v (%.0f items/s); last envelope at %v",
		offered.Round(time.Millisecond), peak, emptyAt.Round(time.Millisecond), 1400/max(emptyAt.Seconds(), 1e-9),
		out[len(out)-1].Sub(start).Round(time.Millisecond))
}
