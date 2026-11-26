package plugin_test

// The K10 stall-injection harness: the whole plugin (hooks, capture workers and writer, uploader
// against the fake archive, maintenance, stop) runs on a real outbox whose SQLite I/O goes through
// internal/diskstall, which stalls or fails chosen reads, writes and syncs. A run drives synthetic
// non-stream exchanges through the hooks and reports, as counts and durations only, how many
// envelopes the outbox lost, what the dropped counters say, how long hooks and the stop took, and
// the peak heap and content-pool use. The tests below pin plugin 0.1.5's behaviour; the opt-in
// sweep (CA_STALL_SWEEP=1) measures stall lengths for the 0.1.6 design. Every value is synthetic.

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/capture"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/diskstall"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/plugin"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/plugin/plugintest"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
)

// stallRun is one plugin under the harness.
type stallRun struct {
	t      *testing.T
	h      *plugintest.Harness
	ob     *outbox.Outbox
	db     *sql.DB // read-only view of the outbox file, for counting
	perEx  int     // envelopes one exchange stores (calibrated)
	n      atomic.Int64
	hookNs atomic.Int64 // longest single hook call
	heap   atomic.Uint64
	pool   atomic.Int64
	stop   chan struct{}
	wg     sync.WaitGroup
	prefix string
}

// stallResult is what a run measured. Counts and durations only.
type stallResult struct {
	Exchanges int
	Offered   int               // envelopes the exchanges would have stored without a stall
	Stored    int               // envelopes in the outbox after the stop
	Lost      int               // Offered - Stored
	Dropped   map[string]uint64 // non-zero dropped counters
	HookMax   time.Duration     // longest hook call
	Stop      time.Duration     // the stop's duration (0 when the run did not stop)
	PeakHeap  uint64            // peak Go heap in use, bytes
	PeakPool  int64             // peak content-pool reservation, bytes
	Rule      diskstall.Stats
}

func (r stallResult) String() string {
	keys := make([]string, 0, len(r.Dropped))
	for k := range r.Dropped {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var d []string
	for _, k := range keys {
		d = append(d, fmt.Sprintf("%s=%d", k, r.Dropped[k]))
	}
	return fmt.Sprintf("exchanges=%d offered=%d stored=%d lost=%d dropped{%s} hook_max=%v stop=%v peak_heap_mib=%.1f peak_pool_kib=%d rule{matched=%d affected=%d max_wait=%v}",
		r.Exchanges, r.Offered, r.Stored, r.Lost, strings.Join(d, ","), r.HookMax.Round(time.Microsecond), r.Stop.Round(time.Millisecond),
		float64(r.PeakHeap)/(1<<20), r.PeakPool/1024, r.Rule.Matched, r.Rule.Affected, r.Rule.MaxWait.Round(time.Millisecond))
}

func setProbeInterval(t *testing.T, d time.Duration) {
	old := capture.ProbeInterval
	capture.ProbeInterval = d
	t.Cleanup(func() { capture.ProbeInterval = old })
}

func setStopTimeout(t *testing.T, d time.Duration) {
	old := plugin.StopTimeout
	plugin.StopTimeout = d
	t.Cleanup(func() { plugin.StopTimeout = old })
}

// newStallRun starts a plugin on a gated outbox and calibrates the envelopes per exchange.
func newStallRun(t *testing.T) *stallRun {
	t.Helper()
	if err := diskstall.Install(); err != nil {
		t.Fatal(err)
	}
	h := plugintest.New(t, nil, "")
	r := &stallRun{t: t, h: h, stop: make(chan struct{}), prefix: "stall"}
	plugintest.WaitFor(t, "runtime", func() bool { r.ob, _, _ = h.Plugin.Runtime(); return r.ob != nil })
	u := url.URL{Scheme: "file", Path: r.ob.Path(), RawQuery: "mode=ro&_query_only=true"}
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	r.db = db
	r.wg.Add(1)
	go r.sample()
	t.Cleanup(r.end)
	r.exchange("cal-1")
	r.exchange("cal-2")
	r.perEx = r.settle(1) / 2
	if r.perEx < 3 {
		t.Fatalf("calibration stored %d envelopes per exchange", r.perEx)
	}
	r.n.Store(0)
	r.hookNs.Store(0)
	return r
}

func (r *stallRun) end() {
	select {
	case <-r.stop:
	default:
		close(r.stop)
	}
	r.wg.Wait()
}

// sample records the peak heap and content-pool use every 50 ms.
func (r *stallRun) sample() {
	defer r.wg.Done()
	e := r.h.Plugin.Engine()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	var ms runtime.MemStats
	for {
		runtime.ReadMemStats(&ms)
		for v := ms.HeapInuse; ; {
			if old := r.heap.Load(); v <= old || r.heap.CompareAndSwap(old, v) {
				break
			}
		}
		for v := e.Pool.Used() + e.Pool.StreamUsed(); ; {
			if old := r.pool.Load(); v <= old || r.pool.CompareAndSwap(old, v) {
				break
			}
		}
		select {
		case <-r.stop:
			return
		case <-tick.C:
		}
	}
}

func (r *stallRun) hook(method string, raw []byte) {
	start := time.Now()
	r.h.Plugin.Hook(method, raw)
	d := int64(time.Since(start))
	for {
		if old := r.hookNs.Load(); d <= old || r.hookNs.CompareAndSwap(old, d) {
			return
		}
	}
}

// exchange drives one synthetic non-stream exchange through the hooks.
func (r *stallRun) exchange(rid string) {
	meta := plugintest.Meta("/v1/messages")
	r.hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth(rid, "claude", plugintest.ClaudeBody("synthetic stall text"), nil, meta))
	r.hook(protocol.MethodRequestInterceptAfter, plugintest.AfterAuth(rid))
	r.hook(protocol.MethodResponseInterceptAfter, plugintest.Response(rid, "claude",
		[]byte(`{"id":"msg_stall","type":"message","role":"assistant","content":[{"type":"text","text":"synthetic"}],"stop_reason":"end_turn"}`), meta))
	r.hook(protocol.MethodRequestComplete, plugintest.Complete(rid, "succeeded", meta))
}

// load drives n exchanges, one every every.
func (r *stallRun) load(n int, every time.Duration) {
	next := time.Now()
	for i := 0; i < n; i++ {
		r.exchange(fmt.Sprintf("%s-%d", r.prefix, r.n.Add(1)))
		next = next.Add(every)
		time.Sleep(time.Until(next))
	}
}

// loadFor drives exchanges every every for d.
func (r *stallRun) loadFor(d, every time.Duration) {
	r.load(int(d/every), every)
}

// count returns the envelopes stored in the outbox (a superseded dead letter is not one).
func (r *stallRun) count() int {
	var n int
	if err := r.db.QueryRow(`SELECT count(*) FROM envelopes WHERE NOT (state = 3 AND dead_reason IS 'superseded')`).Scan(&n); err != nil {
		r.t.Fatalf("count: %v", err)
	}
	return n
}

// settle waits until the stored count holds still for quiet and returns it.
func (r *stallRun) settle(atLeast int) int {
	r.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	last, since := -1, time.Now()
	for time.Now().Before(deadline) {
		n := r.count()
		if n != last {
			last, since = n, time.Now()
		} else if n >= atLeast && time.Since(since) > 300*time.Millisecond {
			return n
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.t.Fatalf("outbox count did not settle (last %d)", last)
	return last
}

// arm adds a rule for this run's outbox.
func (r *stallRun) arm(rule diskstall.Rule) *diskstall.Handle {
	rule.Dir = r.h.Dir
	h := diskstall.Arm(rule)
	r.t.Cleanup(h.Disarm)
	return h
}

// holdOther makes another user of the outbox's single write connection (the uploader's ack, a
// maintenance pass, the dropped-counter save; here an open-requests save, which nothing in a run
// reads back) the one whose commit stalls: every WAL sync waits until the returned release runs,
// and that save takes the first. The capture writer then waits for the connection, not for the
// disk. (The writer's ProbeWrite cannot stand in: its UPDATE leaves the value unchanged, SQLite
// writes no page, and its commit never syncs; TestProbeWriteTouchesNoFile.)
func (r *stallRun) holdOther() (*diskstall.Handle, func()) {
	r.t.Helper()
	hold := make(chan struct{})
	h := r.arm(diskstall.Rule{Ops: diskstall.Sync, Files: diskstall.WAL, Hold: hold})
	go func() {
		_, _ = r.ob.SaveOpenRequests(context.Background(), []outbox.OpenRequest{{RequestID: "stall-holder", TouchedAt: time.Now()}})
	}()
	if !h.WaitFor(5*time.Second, func(s diskstall.Stats) bool { return s.Waiting == 1 }) {
		r.t.Fatal("the holder's commit never reached the held sync")
	}
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	r.t.Cleanup(release)
	return h, release
}

// holdCapture makes the capture writer's own next commit stall until release.
func (r *stallRun) holdCapture() (*diskstall.Handle, func()) {
	r.t.Helper()
	hold := make(chan struct{})
	h := r.arm(diskstall.Rule{Ops: diskstall.Sync, Files: diskstall.WAL, Hold: hold})
	r.exchange(fmt.Sprintf("%s-%d", r.prefix, r.n.Add(1)))
	if !h.WaitFor(5*time.Second, func(s diskstall.Stats) bool { return s.Waiting == 1 }) {
		r.t.Fatal("the capture commit never reached the held sync")
	}
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	r.t.Cleanup(release)
	return h, release
}

// shutdown runs the final stop and returns its duration.
func (r *stallRun) shutdown() time.Duration {
	start := time.Now()
	r.h.Plugin.Shutdown()
	return time.Since(start)
}

// result counts what the run lost; call it after the stop or once the outbox settled.
func (r *stallRun) result(h *diskstall.Handle, stop time.Duration) stallResult {
	r.end()
	res := stallResult{Exchanges: int(r.n.Load()), Dropped: map[string]uint64{}, HookMax: time.Duration(r.hookNs.Load()),
		Stop: stop, PeakHeap: r.heap.Load(), PeakPool: r.pool.Load()}
	res.Offered = (res.Exchanges + 2) * r.perEx // the two calibration exchanges are stored too
	res.Stored = r.count()
	res.Lost = res.Offered - res.Stored
	for k, v := range r.h.Plugin.Engine().Stats.Dropped() {
		if v != 0 {
			res.Dropped[k] = v
		}
	}
	if h != nil {
		res.Rule = h.Stats()
	}
	return res
}

// TestStallHarnessShortCommitStallLosesNothing: the capture writer's own commit stalled for 2 s, under
// the write timeout, while exchanges keep arriving: the writer waits, the write queue absorbs the
// backlog, nothing is lost or counted dropped, and no hook waits for the disk.
func TestStallHarnessShortCommitStallLosesNothing(t *testing.T) {
	r := newStallRun(t)
	h, release := r.holdCapture()
	time.AfterFunc(2*time.Second, release)
	r.loadFor(3*time.Second, 50*time.Millisecond)
	r.settle(1)
	res := r.result(h, 0)
	t.Log(res)
	if res.Lost != 0 || len(res.Dropped) != 0 {
		t.Fatalf("a 2 s commit stall lost envelopes: %v", res)
	}
	if res.Rule.MaxWait < 2*time.Second {
		t.Fatalf("the stall was %v", res.Rule.MaxWait)
	}
	if res.HookMax > 500*time.Millisecond {
		t.Fatalf("a hook waited %v during the stall", res.HookMax)
	}
}

// TestStallHarnessStallPastWriteTimeoutDropsGroup pins 0.1.5's known limit ("a stalled outbox write
// still drops its group"): another user of the write connection stalls in its commit for 12 s,
// longer than the capture writer's 10 s write timeout. The writer's group times out waiting for the
// connection and is dropped as disk_hard, write_unavailable is raised, and every group the writer
// takes in the 5 s probe interval after that is dropped untried, including the backlog queued
// during the stall. Every lost envelope is counted; nothing waits in a hook; writes resume once a
// probe succeeds.
func TestStallHarnessStallPastWriteTimeoutDropsGroup(t *testing.T) {
	r := newStallRun(t)
	h, release := r.holdOther()
	time.AfterFunc(12*time.Second, release)
	r.loadFor(17*time.Second, 100*time.Millisecond)
	r.settle(1)
	res := r.result(h, 0)
	t.Log(res)
	if res.Lost == 0 || res.Dropped["disk_hard"] == 0 {
		t.Fatalf("a 12 s stall past the write timeout lost nothing: %v", res)
	}
	if uint64(res.Lost) != res.Dropped["disk_hard"] {
		t.Fatalf("lost %d envelopes but counted disk_hard %d", res.Lost, res.Dropped["disk_hard"])
	}
	// The loss covers the exchanges of the stall and the probe interval after the timeout, not one group.
	if res.Lost < 50 {
		t.Fatalf("lost only %d envelopes", res.Lost)
	}
	if res.HookMax > 500*time.Millisecond {
		t.Fatalf("a hook waited %v during the stall", res.HookMax)
	}
	if r.h.Plugin.Engine().Stats.HasFault("write_unavailable") {
		t.Fatal("write_unavailable still raised after the disk recovered")
	}
}

// TestStallHarnessFailedSyncDropsGroup: one WAL fsync that fails (SQLITE_IOERR_FSYNC) on the capture
// writer's commit drops that group as disk_hard, raises write_unavailable, and drops the groups
// that follow inside the probe interval untried; the next probe clears the fault.
func TestStallHarnessFailedSyncDropsGroup(t *testing.T) {
	setProbeInterval(t, time.Second)
	r := newStallRun(t)
	h := r.arm(diskstall.Rule{Ops: diskstall.Sync, Files: diskstall.WAL, Count: 1, Fail: diskstall.IOErrFsync})
	r.exchange(fmt.Sprintf("%s-%d", r.prefix, r.n.Add(1)))
	plugintest.WaitFor(t, "the failed sync", func() bool { return h.Stats().Affected == 1 })
	r.loadFor(2*time.Second, 50*time.Millisecond)
	r.settle(1)
	res := r.result(h, 0)
	t.Log(res)
	if res.Lost <= r.perEx || uint64(res.Lost) != res.Dropped["disk_hard"] {
		t.Fatalf("one failed sync: %v (want the group and the untried groups after it, all counted disk_hard)", res)
	}
	if r.h.Plugin.Engine().Stats.HasFault("write_unavailable") {
		t.Fatal("write_unavailable still raised after the disk recovered")
	}
}

// TestStallHarnessStopWaitsForStalledCommit pins a second 0.1.5 limit the 0.1.6 design must remove:
// a stop waits for the capture writer, and SQLite's commit cannot be interrupted, so a commit
// stalled in its WAL fsync holds the stop past its deadline for as long as the disk stalls. Here
// the stop budget is 2 s (StopTimeout; production's is 25 s under CPA's 30 s) and the commit stalls
// 5 s: the stop returns only when the stall ends. The stalled group itself is stored.
func TestStallHarnessStopWaitsForStalledCommit(t *testing.T) {
	setStopTimeout(t, 2*time.Second)
	r := newStallRun(t)
	h, release := r.holdCapture()
	stallEnd := time.Now().Add(5 * time.Second)
	time.AfterFunc(time.Until(stallEnd), release)
	stop := r.shutdown()
	res := r.result(h, stop)
	t.Log(res)
	if time.Now().Before(stallEnd) || stop < 2*time.Second+plugin.StopCloseMargin+time.Second {
		t.Fatalf("the stop returned in %v, before the 5 s stall ended", stop)
	}
	if res.Lost != 0 {
		t.Fatalf("the stalled group was lost: %v", res)
	}
}

// TestStallHarnessStopWaitsForStalledMaintenance: the same limit reaches the whole stop, not only
// the writer. Maintenance's wal_checkpoint(TRUNCATE) (every GCEvery) is the only outbox call that
// syncs the main database file, so holding main-database syncs stalls maintenance inside the
// checkpoint while it holds the single write connection. The stop cancels the maintenance loop and
// then waits for it without a bound (rs.wg.Wait), so it returns only when the checkpoint does.
func TestStallHarnessStopWaitsForStalledMaintenance(t *testing.T) {
	setStopTimeout(t, 2*time.Second)
	for _, v := range []struct {
		p *time.Duration
		d time.Duration
	}{{&plugin.SnapshotEvery, 20 * time.Millisecond}, {&plugin.GCEvery, 50 * time.Millisecond}} {
		old := *v.p
		*v.p = v.d
		t.Cleanup(func() { *v.p = old })
	}
	r := newStallRun(t)
	hold := make(chan struct{})
	h := r.arm(diskstall.Rule{Ops: diskstall.Sync, Files: diskstall.MainDB, Hold: hold})
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	t.Cleanup(release)
	r.exchange(fmt.Sprintf("%s-%d", r.prefix, r.n.Add(1))) // WAL frames for the checkpoint to copy
	if !h.WaitFor(10*time.Second, func(s diskstall.Stats) bool { return s.Waiting == 1 }) {
		t.Fatal("maintenance's checkpoint never reached the held main-database sync")
	}
	stallEnd := time.Now().Add(5 * time.Second)
	time.AfterFunc(time.Until(stallEnd), release)
	stop := r.shutdown()
	res := r.result(h, stop)
	t.Log(res)
	if time.Now().Before(stallEnd) || stop < 2*time.Second+plugin.StopCloseMargin+time.Second {
		t.Fatalf("the stop returned in %v, before the 5 s maintenance stall ended", stop)
	}
}

// TestStallHarnessSweep measures 0.1.5 across stall lengths and holders, for the 0.1.6 design. It
// takes minutes, so it runs only with CA_STALL_SWEEP=1 (and CA_STALL_SWEEP_S, a comma list of stall
// seconds, to pick lengths). It asserts nothing beyond the runs completing; it logs one line per run.
func TestStallHarnessSweep(t *testing.T) {
	if os.Getenv("CA_STALL_SWEEP") != "1" {
		t.Skip("set CA_STALL_SWEEP=1 to run the stall sweep")
	}
	secs := []int{1, 5, 9, 11, 15, 20, 30}
	if s := os.Getenv("CA_STALL_SWEEP_S"); s != "" {
		secs = nil
		for _, f := range strings.Split(s, ",") {
			var n int
			fmt.Sscan(f, &n)
			secs = append(secs, n)
		}
	}
	// capture: the writer's own commit stalls; other: another write-connection user's commit
	// stalls and the writer waits for the connection; fail: every WAL write and sync fails
	// (SQLITE_IOERR) for the whole period.
	for _, holder := range []string{"capture", "other", "fail"} {
		for _, s := range secs {
			t.Run(fmt.Sprintf("%s-%ds", holder, s), func(t *testing.T) {
				r := newStallRun(t)
				var h *diskstall.Handle
				var release func()
				switch holder {
				case "capture":
					h, release = r.holdCapture()
				case "other":
					h, release = r.holdOther()
				default:
					h = r.arm(diskstall.Rule{Ops: diskstall.Write | diskstall.Sync, Files: diskstall.WAL, Fail: diskstall.IOErr})
					release = h.Disarm
				}
				stall := time.Duration(s) * time.Second
				time.AfterFunc(stall, release)
				r.loadFor(stall+6*time.Second, 100*time.Millisecond)
				r.settle(1)
				res := r.result(h, 0)
				t.Logf("holder=%s stall=%ds rate=10/s %v", holder, s, res)
			})
		}
	}
	for _, s := range secs {
		t.Run(fmt.Sprintf("stop-capture-%ds", s), func(t *testing.T) {
			r := newStallRun(t)
			h, release := r.holdCapture()
			time.AfterFunc(time.Duration(s)*time.Second, release)
			stop := r.shutdown()
			res := r.result(h, stop)
			t.Logf("stop during capture commit stall=%ds budget=%v %v", s, plugin.StopTimeout, res)
		})
		t.Run(fmt.Sprintf("stop-other-%ds", s), func(t *testing.T) {
			r := newStallRun(t)
			h, release := r.holdOther()
			time.AfterFunc(time.Duration(s)*time.Second, release)
			r.loadFor(time.Second, 100*time.Millisecond)
			stop := r.shutdown()
			res := r.result(h, stop)
			t.Logf("stop during other commit stall=%ds budget=%v %v", s, plugin.StopTimeout, res)
		})
	}
}
