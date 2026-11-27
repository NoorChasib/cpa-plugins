package plugin_test

// Outbox write-stall scenarios (plugin 0.1.6; see docs/release-notes-0.1.6.md): the whole plugin on a gated outbox,
// production load with headroom (steady about 90 envelopes/s, bursts of 640 envelopes, bodies up to
// 4 MiB, production-shaped request bodies at 2 x the peak minute's body bytes (stall_r1_prod_test.go),
// live streams) through a disk stall or a failure run. Each run settles on an offered-key
// manifest: every key the driver offered is stored, no key outside it is, and every dropped counter
// stays 0. CI stalls 12 s (past 0.1.5's 10 s write timeout); CA_STALL_LONG=1 stalls 60 s. Every
// value is synthetic; results are counts, sizes and durations.

import (
	"fmt"
	"os"
	"runtime/metrics"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/diskstall"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/plugin/plugintest"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
)

// r1Stall is the stall length: 12 s in CI, 60 s with CA_STALL_LONG=1.
func r1Stall() time.Duration {
	if os.Getenv("CA_STALL_LONG") != "" {
		return 60 * time.Second
	}
	return 12 * time.Second
}

// r1Run is a stallRun with the R1 load driver and its offered-key manifest.
type r1Run struct {
	*stallRun
	mu       sync.Mutex
	offered  map[string]bool
	streamSx []string // key suffixes one stream exchange stores (calibrated)
	seq      atomic.Int64
	loads    sync.WaitGroup
	live     atomic.Uint64 // peak live heap during the stall window
	inStall  atomic.Bool
	hookMax  atomic.Int64
	bigText  string
	prod     *prodLoad // the production-shaped body load, when the scenario has one
	steadyN  *steadyLoad
}

// steadyLoad counts the envelopes a steady driver offered, at the time it offers them.
type steadyLoad struct {
	n atomic.Int64
}

// r1Offered is the load a scenario states it offers while the writer is held, as fixed figures that
// match the documented rates and are independent of the drivers' own pacing: a driver that is
// missing or paced too slowly fails the scenario instead of lowering its expectation.
type r1Offered struct {
	envelopesPerSec float64 // steady envelopes a second
	bodyMiBPerMin   float64 // production-shaped request-body MiB a minute
}

// r1ProdBodiesOffered is production_bodies' stated load: 90 envelopes a second and 181 MiB of
// request body a minute (twice production's 90.5 MiB peak minute; REFERENCE.md and the 0.1.6
// release notes), or 32.8 MiB (the p99 active minute) under -race.
func r1ProdBodiesOffered() *r1Offered {
	if raceEnabled {
		return &r1Offered{envelopesPerSec: 90, bodyMiBPerMin: 32.8}
	}
	return &r1Offered{envelopesPerSec: 90, bodyMiBPerMin: 181}
}

// r1DriverLag is how far behind its schedule a load driver may be at any instant. The drivers keep a
// schedule and catch up after a slow call, so they lag only by the call in progress: an exchange
// makes five hook calls, and the scenarios fail a hook call over 250 ms.
const r1DriverLag = time.Second

// checkOfferedRate fails when the load started at start offered less than perSec a second (less
// r1DriverLag) by at. A driver counts an exchange when it begins offering it, so on schedule the
// count at any instant is at least perSec times the time elapsed.
func checkOfferedRate(t *testing.T, what string, got int64, perSec float64, start, at time.Time) {
	t.Helper()
	elapsed := at.Sub(start)
	want := perSec * (elapsed - r1DriverLag).Seconds()
	t.Logf("offered while the writer was held: %s=%d in %v (stated %.0f/s, at least %.0f required)", what, got, elapsed.Round(time.Millisecond), perSec, want)
	if float64(got) < want {
		t.Fatalf("the load offered %d %s in the %v the writer was held, below the stated %.0f a second (at least %.0f allowing %v of lag)",
			got, what, elapsed.Round(time.Millisecond), perSec, want, r1DriverLag)
	}
}

func newR1Run(t *testing.T) *r1Run {
	r := &r1Run{stallRun: newStallRun(t), offered: map[string]bool{}}
	r.prefix = "hold" // the holders' own exchanges (holdCapture), outside the manifest
	// Calibrate a stream exchange's keys, as newStallRun did for the non-stream one.
	r.stream("cal-s", 3, 0)
	plugintest.WaitFor(t, "the stream calibration", func() bool { return r.has("ex/cal-s/completion") })
	rows, err := r.db.Query(`SELECT key FROM envelopes WHERE key LIKE 'ex/cal-s/%'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		rows.Scan(&k)
		r.streamSx = append(r.streamSx, strings.TrimPrefix(k, "ex/cal-s"))
	}
	rows.Close()
	if len(r.streamSx) < 3 {
		t.Fatalf("stream calibration stored %v", r.streamSx)
	}
	r.offered = map[string]bool{}
	r.hookMax.Store(0)
	go r.sampleLive()
	return r
}

// sampleLive records the peak live heap while inStall is set.
func (r *r1Run) sampleLive() {
	s := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	for {
		select {
		case <-r.stop:
			return
		case <-time.After(50 * time.Millisecond):
		}
		if !r.inStall.Load() {
			continue
		}
		metrics.Read(s)
		if s[0].Value.Kind() == metrics.KindUint64 {
			for v := s[0].Value.Uint64(); ; {
				if old := r.live.Load(); v <= old || r.live.CompareAndSwap(old, v) {
					break
				}
			}
		}
	}
}

func liveHeap() uint64 {
	s := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	metrics.Read(s)
	return s[0].Value.Uint64()
}

func (r *r1Run) offer(keys ...string) {
	r.mu.Lock()
	for _, k := range keys {
		r.offered[k] = true
	}
	r.mu.Unlock()
}

func (r *r1Run) call(method string, raw []byte) {
	start := time.Now()
	r.h.Plugin.Hook(method, raw)
	d := int64(time.Since(start))
	for {
		if old := r.hookMax.Load(); d <= old || r.hookMax.CompareAndSwap(old, d) {
			return
		}
	}
}

// nonStream offers one non-stream exchange and its usage record (5 envelopes). reqText and respText
// are its texts; distinct per exchange so piece dedupe does not hide bytes.
func (r *r1Run) nonStream(reqText, respText string) {
	rid := fmt.Sprintf("r1-%d", r.seq.Add(1))
	for _, s := range r.suffixes {
		r.offer("ex/" + rid + s)
	}
	r.offer("usage/u-" + rid)
	meta := plugintest.Meta("/v1/messages")
	r.call(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth(rid, "claude", plugintest.ClaudeBody(rid+" "+reqText), nil, meta))
	r.call(protocol.MethodRequestInterceptAfter, plugintest.AfterAuth(rid))
	r.call(protocol.MethodResponseInterceptAfter, plugintest.Response(rid, "claude", claudeReply(rid+" "+respText), meta))
	r.call(protocol.MethodRequestComplete, plugintest.Complete(rid, "succeeded", meta))
	r.call(protocol.MethodUsageHandle, plugintest.Usage("u-"+rid, "sk-cpa-synthetic", "synthetic-source", ""))
}

// stream offers one Claude stream exchange whose chunks arrive every gap (live across a stall):
// before-auth, after-auth, header-init, deltas, the end, then the completion.
func (r *r1Run) stream(rid string, deltas int, gap time.Duration) {
	if rid == "" {
		rid = fmt.Sprintf("r1-%d", r.seq.Add(1))
		for _, s := range r.streamSx {
			r.offer("ex/" + rid + s)
		}
	}
	meta := plugintest.Meta("/v1/messages")
	r.call(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth(rid, "claude", plugintest.ClaudeBody(rid+" stream request"), nil, meta))
	r.call(protocol.MethodRequestInterceptAfter, plugintest.AfterAuth(rid))
	r.call(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(rid, "claude", -1, nil, meta))
	start := append(claudeEvent("message_start", `{"type":"message_start","message":{"id":"msg_`+rid+`","role":"assistant","content":[]}}`),
		claudeEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)...)
	r.call(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(rid, "claude", 0, start, meta))
	for i := 1; i <= deltas; i++ {
		time.Sleep(gap)
		r.call(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(rid, "claude", i, claudeDelta(fmt.Sprintf("%s part %d ", rid, i)), meta))
	}
	r.call(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(rid, "claude", deltas+1, claudeStop, meta))
	r.call(protocol.MethodRequestComplete, plugintest.Complete(rid, "succeeded", meta))
}

// steady offers non-stream exchanges at rate envelopes/s for d, in the background.
func (r *r1Run) steady(rate float64, d time.Duration) {
	every := time.Duration(float64(5*time.Second) / rate)
	l := &steadyLoad{}
	r.steadyN = l
	r.loads.Add(1)
	go func() {
		defer r.loads.Done()
		next, end := time.Now(), time.Now().Add(d)
		for time.Now().Before(end) {
			l.n.Add(5)
			r.nonStream("synthetic steady request", "synthetic steady answer")
			next = next.Add(every)
			time.Sleep(time.Until(next))
		}
	}()
}

// burst offers 640 envelopes (128 non-stream exchanges with their usage records) from 16 goroutines
// within 2 s, after delay, in the background.
func (r *r1Run) burst(delay time.Duration) {
	r.loads.Add(1)
	go func() {
		defer r.loads.Done()
		time.Sleep(delay)
		var wg sync.WaitGroup
		for g := 0; g < 16; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 8; i++ {
					r.nonStream("synthetic burst request", "synthetic burst answer")
					time.Sleep(250 * time.Millisecond)
				}
			}()
		}
		wg.Wait()
	}()
}

// large offers an exchange with a 4 MiB body every 8 s (0.5 MiB/s of bodies) for d, starting at
// once, alternately a 4 MiB request and a 4 MiB response, in the background.
func (r *r1Run) large(d time.Duration) {
	if r.bigText == "" {
		r.bigText = strings.Repeat("synthetic large body text ", (4<<20)/26)
	}
	r.loads.Add(1)
	go func() {
		defer r.loads.Done()
		for i, end := 0, time.Now().Add(d); time.Now().Before(end); i++ {
			if i%2 == 0 {
				r.nonStream(r.bigText, "synthetic small answer")
			} else {
				r.nonStream("synthetic small request", r.bigText)
			}
			select {
			case <-time.After(8 * time.Second):
			case <-r.stop:
				return
			}
		}
	}()
}

// streams offers n stream exchanges in the background, each with 20 deltas spread over d.
func (r *r1Run) streams(n int, d time.Duration) {
	for i := 0; i < n; i++ {
		r.loads.Add(1)
		go func() {
			defer r.loads.Done()
			r.stream("", 20, d/20)
		}()
	}
}

// stored returns how many manifest keys are stored, and fails on a stored key of the run that the
// driver never offered.
func (r *r1Run) stored() (int, int) {
	rows, err := r.db.Query(`SELECT key FROM envelopes WHERE (key LIKE 'ex/r1-%' OR key LIKE 'usage/u-r1-%') AND NOT (state = 3 AND dead_reason IS 'superseded')`)
	if err != nil {
		r.t.Fatalf("stored: %v", err)
	}
	defer rows.Close()
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for rows.Next() {
		var k string
		rows.Scan(&k)
		if !r.offered[k] {
			r.t.Fatalf("stored key %s was never offered", k)
		}
		n++
	}
	return n, len(r.offered)
}

// wholeLossR1 is every counter that stands for a whole envelope not stored.
func (r *r1Run) wholeLossR1() int {
	d := r.h.Plugin.Engine().Stats.Dropped()
	return int(d["disk_hard"] + d["stopped"] + d["control_full"] + d["duplicate_key"] + d["encode_error"] + d["hold_oversize"])
}

// settleR1 waits until every offered key is stored or counted, exactly.
func (r *r1Run) settleR1() (stored, offered int) {
	r.t.Helper()
	r.loads.Wait()
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		n, off := r.stored()
		switch c := r.wholeLossR1(); {
		case n+c == off:
			return n, off
		case n+c > off:
			r.t.Fatalf("stored %d plus counted %d is more than offered %d", n, c, off)
		}
		time.Sleep(50 * time.Millisecond)
	}
	n, off := r.stored()
	r.t.Fatalf("not every offered envelope was stored or counted: stored %d, counted %d, offered %d, dropped %v", n, r.wholeLossR1(), off, r.h.Plugin.Engine().Stats.Dropped())
	return 0, 0
}

// TestStallR1 runs each R1 scenario. Each passes with zero loss on 0.1.6; each loses or sheds on
// 0.1.5 (a 10 s attempt timeout, the probe gate, a 1,024-entry write channel).
func TestStallR1(t *testing.T) {
	stall := r1Stall()
	tail := 4 * time.Second
	const rate = 90.0 // envelopes per second
	for _, sc := range []struct {
		name   string
		holder string
		load   func(r *r1Run)
		want   *r1Offered // the load offered while the writer is held, when the scenario enforces it
	}{
		{"steady", "other", func(r *r1Run) { r.steady(rate, stall+tail) }, nil},
		{"bursts", "capture", func(r *r1Run) {
			r.steady(rate, stall+tail)
			r.burst(200 * time.Millisecond)
			r.burst(stall - 3*time.Second)
		}, nil},
		{"large_bodies", "other", func(r *r1Run) { r.steady(rate, stall+tail); r.large(stall) }, nil},
		{"production_bodies", "other", func(r *r1Run) {
			r.steady(rate, stall+tail)
			rate := 2 * prodPeakMinMiB
			if raceEnabled {
				// Under -race two workers cannot split 3 MiB of bodies a second (handoff_full, the
				// pool far from full); the 60 s run and TestStallR1PoolSustain carry the pool figures.
				rate = prodP99MinMiB
			}
			r.prod = r.bodies(7, rate, stall, nil)
		}, r1ProdBodiesOffered()},
		{"live_streams", "other", func(r *r1Run) { r.steady(rate, stall+tail); r.streams(16, stall+2*time.Second) }, nil},
		{"write_mutex", "writemu", func(r *r1Run) { r.steady(rate, stall+tail) }, nil},
		{"failure_run", "fail", func(r *r1Run) { r.steady(rate, stall+tail) }, nil},
	} {
		t.Run(sc.name, func(t *testing.T) {
			r := newR1Run(t)
			e := r.h.Plugin.Engine()
			var h *diskstall.Handle
			var release func()
			switch sc.holder {
			case "capture":
				h, release = r.holdCapture()
			case "other":
				h, release = r.holdOther()
			case "writemu":
				h, release = r.holdWriteMu()
			case "fail":
				r.quiet()
				h = r.arm(diskstall.Rule{Ops: diskstall.Write | diskstall.Sync, Files: diskstall.WAL, Fail: diskstall.IOErr})
				release = h.Disarm
			}
			base := liveHeap()
			r.inStall.Store(true)
			// The load starts before the timers, so the release reads the drivers' counters.
			loadStart := time.Now()
			sc.load(r)
			if sc.want != nil && (r.steadyN == nil || r.prod == nil) {
				t.Fatalf("scenario %s states its offered load but is missing a driver (steady %t, production-shaped bodies %t)", sc.name, r.steadyN != nil, r.prod != nil)
			}
			var alerted atomic.Bool
			time.AfterFunc(stall-time.Second, func() { alerted.Store(e.Stats.HasFault("write_unavailable")) })
			released := make(chan struct{})
			var poolAtRelease int64
			var releasedAt time.Time
			var steadyAtRelease, prodAtRelease int64
			time.AfterFunc(stall, func() {
				poolAtRelease = e.Pool.Used()
				releasedAt = time.Now()
				if r.steadyN != nil {
					steadyAtRelease = r.steadyN.n.Load()
				}
				if r.prod != nil {
					prodAtRelease = r.prod.bytes.Load()
				}
				r.inStall.Store(false)
				release()
				close(released)
			})
			<-released
			stored, offered := r.settleR1()
			_, _, rt := r.h.Plugin.Runtime()
			holdPeak, queuePeak := r1Gauges(rt)
			res := r.result(h, 0)
			live := int64(r.live.Load()) - int64(base)
			t.Logf("scenario=%s holder=%s stall=%v offered=%d stored=%d dropped=%v hook_max=%v hold_peak_kib=%d queue_peak=%d pool_peak_mib=%.1f pool_at_release_mib=%.1f live_growth_mib=%.1f rule{affected=%d max_wait=%v}",
				sc.name, sc.holder, stall, offered, stored, res.Dropped, time.Duration(r.hookMax.Load()).Round(time.Microsecond), holdPeak/1024, queuePeak,
				float64(res.PeakPool)/(1<<20), float64(poolAtRelease)/(1<<20), float64(live)/(1<<20), res.Rule.Affected, res.Rule.MaxWait.Round(time.Millisecond))
			if r.prod != nil {
				t.Logf("scenario=%s production-shaped load: %s", sc.name, r.prod.summary())
			}
			if w := sc.want; w != nil {
				// Zero loss proves something only at the load the scenario states: the envelopes
				// and body bytes offered while the writer was held meet its fixed rates.
				checkOfferedRate(t, "envelopes", steadyAtRelease, w.envelopesPerSec, loadStart, releasedAt)
				checkOfferedRate(t, "request-body bytes", prodAtRelease, w.bodyMiBPerMin*(1<<20)/60, loadStart, releasedAt)
			}
			if stored != offered || len(res.Dropped) != 0 {
				t.Fatalf("lost %d of %d envelopes, dropped %v", offered-stored, offered, res.Dropped)
			}
			if sc.holder == "fail" {
				if res.Rule.Affected == 0 {
					t.Fatal("no WAL write or sync failed")
				}
			} else if res.Rule.MaxWait < stall-100*time.Millisecond {
				t.Fatalf("the stall lasted %v, want %v", res.Rule.MaxWait, stall)
			}
			if !alerted.Load() {
				t.Fatal("write_unavailable was not raised during the stall")
			}
			if e.Stats.HasFault("write_unavailable") {
				t.Fatal("write_unavailable still raised after the disk recovered")
			}
			if d := time.Duration(r.hookMax.Load()); d > 250*time.Millisecond {
				t.Fatalf("a hook call took %v", d)
			}
			r1CheckMemory(t, rt, holdPeak, queuePeak, live, res.PeakPool)
		})
	}
}
