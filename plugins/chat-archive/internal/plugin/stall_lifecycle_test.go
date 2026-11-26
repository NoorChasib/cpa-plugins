package plugin_test

// More K10 harness pins, for design revision 3: the pool accounting the reference workload W is
// stated in, the public reconfigure path and the open retry's start under the lifecycle lock (R3),
// and a writer stalled before COMMIT, waiting for the outbox's write mutex (the stop's in-flight
// group). Each pins plugin 0.1.5's behaviour; the 0.1.6 tests named in the design flip them.

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/diskstall"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/plugin"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/plugin/plugintest"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
)

// b64Len is the body length the hooks reserve for: the decoded length CPA's base64 implies
// (protocol.Base64Len / 4 * 3), so up to two bytes over the body.
func b64Len(n int) int64 { return int64((n + 2) / 3 * 3) }

// steadyPool waits until nothing is queued for the workers and the content pool holds still,
// and returns Pool.Used.
func steadyPool(t *testing.T, r *stallRun) int64 {
	t.Helper()
	e := r.h.Plugin.Engine()
	last, since := int64(-1), time.Now()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		u := e.Pool.Used()
		if u != last || len(e.Handoff) > 0 || len(e.Stream) > 0 {
			last, since = u, time.Now()
		} else if time.Since(since) > 200*time.Millisecond {
			return u
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the content pool did not hold still")
	return 0
}

// TestStallHarnessPoolChargesEveryBodyClass pins the accounting W is stated in. With the capture
// writer's commit stalled, every queued body stays charged to the content pool until the writer's
// transaction returns: a request and a non-stream response at 4 × their body length each; an open
// stream's chunks at 1 × their bytes, in the stream sub-budget, which is part of Pool.Used (so the
// two are never added); and a stream response, once its terminal event arrives, at 4 × the
// reassembled body, with its chunk reservation returned. After the release, everything is
// returned.
func TestStallHarnessPoolChargesEveryBodyClass(t *testing.T) {
	r := newStallRun(t)
	e := r.h.Plugin.Engine()
	_, release := r.holdCapture()
	base := steadyPool(t, r)

	text := strings.Repeat("synthetic filler ", 64*1024/17)
	req := plugintest.ClaudeBody("pool-1 " + text)
	resp := claudeReply("pool-1 " + text)
	meta := plugintest.Meta("/v1/messages")
	r.hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth("pool-1", "claude", req, nil, meta))
	r.hook(protocol.MethodRequestInterceptAfter, plugintest.AfterAuth("pool-1"))
	r.hook(protocol.MethodResponseInterceptAfter, plugintest.Response("pool-1", "claude", resp, meta))
	r.hook(protocol.MethodRequestComplete, plugintest.Complete("pool-1", "succeeded", meta))
	nonStream := steadyPool(t, r) - base
	if want := 4 * (b64Len(len(req)) + b64Len(len(resp))); nonStream != want {
		t.Fatalf("request and non-stream response charged %d bytes, want 4 × body lengths = %d", nonStream, want)
	}

	ws := map[string]any{"request_path": "/v1/responses", "execution_session_id": "pool-session"}
	open := []byte(`{"type":"response.create","input":"synthetic"}`)
	r.hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth("pool-ws", "openai-response", open, nil, ws))
	r.hook(protocol.MethodRequestInterceptAfter, plugintest.AfterAuth("pool-ws"))
	r.hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("pool-ws", "openai-response", -1, nil, ws))
	events := wsTurn("resp_pool", "", "pool-ws "+text)
	var chunkBytes int64
	for i, c := range events[:2] {
		r.hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("pool-ws", "openai-response", i, c, ws))
		chunkBytes += b64Len(len(c))
	}
	openStream := steadyPool(t, r) - base - nonStream - 4*b64Len(len(open))
	if s := e.Pool.StreamUsed(); s != chunkBytes || openStream != chunkBytes {
		t.Fatalf("open stream: stream part %d, pool delta %d, want both = chunk bytes %d (the stream part is inside Pool.Used)", s, openStream, chunkBytes)
	}
	r.hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("pool-ws", "openai-response", 2, events[2], ws))
	ended := steadyPool(t, r) - base - nonStream - 4*b64Len(len(open))
	if s := e.Pool.StreamUsed(); s != 0 || ended < 4*int64(len(text)) {
		t.Fatalf("ended stream: stream part %d (want 0), pool delta %d (want 4 × the reassembled body, at least %d)", s, ended, 4*len(text))
	}
	t.Logf("charged: request+response %d B for %d+%d B bodies; open stream %d B for %d B of chunks; ended stream %d B",
		nonStream, len(req), len(resp), openStream, chunkBytes, ended)

	release()
	plugintest.WaitFor(t, "every reservation returned", func() bool { return e.Pool.Used() == 0 && e.Pool.StreamUsed() == 0 })
}

// syncSteps is n one-shot holds on an outbox directory's WAL syncs, the i-th holding the i-th sync
// after arming.
type syncSteps struct {
	t     *testing.T
	holds []chan struct{}
	hs    []*diskstall.Handle
	once  sync.Once
}

func armSyncs(t *testing.T, dir string, n int) *syncSteps {
	t.Helper()
	s := &syncSteps{t: t, holds: make([]chan struct{}, n), hs: make([]*diskstall.Handle, n)}
	for i := range s.holds {
		s.holds[i] = make(chan struct{})
		s.hs[i] = diskstall.Arm(diskstall.Rule{Dir: dir, Ops: diskstall.Sync, Files: diskstall.WAL, Skip: i, Count: 1, Hold: s.holds[i]})
	}
	t.Cleanup(s.release)
	return s
}

// release lets every held and future sync through.
func (s *syncSteps) release() {
	s.once.Do(func() {
		for i := range s.holds {
			select {
			case <-s.holds[i]:
			default:
				close(s.holds[i])
			}
			s.hs[i].Disarm()
		}
	})
}

// step walks the syncs as they come: each held sync whose goroutine's stack does not show every one
// of target is let through, until one does, which stays held until release. It fails when none of
// the armed syncs does.
func (s *syncSteps) step(target ...string) *diskstall.Handle {
	s.t.Helper()
	for i := range s.hs {
		if !s.hs[i].WaitFor(15*time.Second, func(st diskstall.Stats) bool { return st.Waiting == 1 }) {
			s.t.Fatalf("WAL sync %d never came", i)
		}
		if stackHas(target...) {
			return s.hs[i]
		}
		close(s.holds[i])
	}
	s.t.Fatalf("none of the first %d WAL syncs was %v", len(s.hs), target)
	return nil
}

// TestStallHarnessReconfigureWaitsForStalledOpen pins the public reconfigure path: a reconfigure
// that changes the config stops the runtime, then opens and starts the replacement synchronously
// under the lifecycle lock. With the replacement's open stalled in its first commit, the
// reconfigure lasts the stall, past the stop budget, and a shutdown that arrives meanwhile waits
// for the lifecycle lock as long. (CPA itself serializes ApplyConfig and ShutdownAll on its
// applyMu, so its shutdown would wait for the reconfigure's return in any case.)
func TestStallHarnessReconfigureWaitsForStalledOpen(t *testing.T) {
	setStopTimeout(t, 2*time.Second)
	r := newStallRun(t)
	dir := filepath.Join(r.h.Dir, "data")
	r.quiet()
	steps := armSyncs(t, dir, 12)
	reconf := make(chan time.Duration, 1)
	start := time.Now()
	go func() {
		_, _ = r.h.Plugin.Handle(protocol.MethodPluginReconfigure, plugintest.Lifecycle(r.h.YAML+"workers: 1\n"))
		reconf <- time.Since(start)
	}()
	// The old runtime's stop may sync too (its saves and the close's checkpoint): step to the
	// replacement's open.
	h := steps.step("outbox.OpenContext", "(*Plugin).configure")
	release := steps.release
	stallEnd := time.Now().Add(4 * time.Second)
	time.AfterFunc(time.Until(stallEnd), release)
	time.Sleep(100 * time.Millisecond)
	shut := make(chan time.Duration, 1)
	shutStart := time.Now()
	go func() { r.h.Plugin.Shutdown(); shut <- time.Since(shutStart) }()
	rd, sd := <-reconf, <-shut
	t.Logf("reconfigure=%v shutdown=%v (budget %v + %v) rule{max_wait=%v}", rd.Round(time.Millisecond), sd.Round(time.Millisecond),
		plugin.StopTimeout, plugin.StopCloseMargin, h.Stats().MaxWait.Round(time.Millisecond))
	if time.Now().Before(stallEnd) || rd < plugin.StopTimeout+plugin.StopCloseMargin+time.Second {
		t.Fatalf("the reconfigure returned in %v, before the replacement's open stall ended", rd)
	}
	if sd < 3*time.Second {
		t.Fatalf("the shutdown returned in %v, before the reconfigure released the lifecycle lock", sd)
	}
}

// TestStallHarnessShutdownWaitsForRetryStartup pins the open retry's start: once a retried open
// succeeds, the loop takes the lifecycle lock and runs startWith, whose carry takes are SQLite
// transactions. With the numbering take stalled in its commit, a shutdown waits for the lifecycle
// lock for the whole stall: the stop cannot even cancel anything until the start's I/O returns.
func TestStallHarnessShutdownWaitsForRetryStartup(t *testing.T) {
	setStopTimeout(t, 2*time.Second)
	setVar(t, &plugin.OpenRetryMin, 20*time.Millisecond)
	r := newStallRun(t) // two closed exchanges: the quiesce below saves their numbering
	p := r.h.Plugin
	data := filepath.Join(r.h.Dir, "data")
	p.Handle(protocol.MethodPluginQuiesce, nil)
	if err := os.Chmod(data, 0o755); err != nil { // the hardened open refuses a group-readable directory
		t.Fatal(err)
	}
	p.Handle(protocol.MethodPluginReconfigure, plugintest.Lifecycle(r.h.YAML))
	if st, _ := p.State(); st != plugin.StateUnavailable {
		t.Fatalf("state %s, want the open retry", st)
	}
	steps := armSyncs(t, data, 12)
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	h := steps.step("(*Plugin).startWith", "(*Outbox).TakeNumbering")
	release := steps.release
	stallEnd := time.Now().Add(4 * time.Second)
	time.AfterFunc(time.Until(stallEnd), release)
	start := time.Now()
	p.Shutdown()
	stop := time.Since(start)
	t.Logf("stop=%v rule{max_wait=%v}", stop.Round(time.Millisecond), h.Stats().MaxWait.Round(time.Millisecond))
	if time.Now().Before(stallEnd) || stop < plugin.StopTimeout+plugin.StopCloseMargin+time.Second {
		t.Fatalf("the stop returned in %v, before the retry's start stall ended", stop)
	}
}

// holdWriteMu makes another holder of the outbox's write mutex (Outbox.Replay, which the uploader
// runs on an epoch change) stall in its commit while holding the mutex. The capture writer's next
// Outbox.Write then waits on the mutex before BeginTx: a wait that no context reaches.
func (r *stallRun) holdWriteMu() (*diskstall.Handle, func()) {
	r.t.Helper()
	r.quiet()
	hold := make(chan struct{})
	h := r.arm(diskstall.Rule{Ops: diskstall.Sync, Files: diskstall.WAL, Hold: hold})
	m := r.ob.Meta()
	r.holders.Add(1)
	go func() {
		defer r.holders.Done()
		_, _, _ = r.ob.Replay(context.Background(), m.EpochSeen, m.ArchiveID, m.HW)
	}()
	if !h.WaitFor(5*time.Second, func(s diskstall.Stats) bool { return s.Waiting == 1 }) {
		r.t.Fatal("the replay's commit never reached the held sync")
	}
	if !stackHas("(*Outbox).Replay") {
		r.t.Fatal("the held sync is not the replay's commit")
	}
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	r.t.Cleanup(release)
	return h, release
}

// TestStallHarnessStopWaitsForWriterOnWriteMu pins a pre-COMMIT stall: the writer's group waits for
// the outbox's write mutex, which another transaction holds while stalled in its commit. The wait
// ignores the write's context, so neither the 10 s write timeout nor the stop's deadline reaches
// it: the stop lasts the stall. When the mutex comes free past the deadline, the group's BeginTx
// fails on its expired context and the group is counted dropped.stopped, although the disk is
// back: the outcome is decided by the deadline, not by the disk.
func TestStallHarnessStopWaitsForWriterOnWriteMu(t *testing.T) {
	setStopTimeout(t, 2*time.Second)
	r := newStallRun(t)
	h, release := r.holdWriteMu()
	r.exchange(fmt.Sprintf("%s-%d", r.prefix, r.n.Add(1)))
	plugintest.WaitFor(t, "the writer on the write mutex", func() bool {
		return stackHas("(*Runtime).flush", "(*Outbox).Write", "sync.(*Mutex).Lock")
	})
	r.loadFor(time.Second, 50*time.Millisecond)
	stallEnd := time.Now().Add(5 * time.Second)
	time.AfterFunc(time.Until(stallEnd), release)
	stop := r.shutdown()
	res := r.result(h, stop)
	t.Log(res)
	if time.Now().Before(stallEnd) || stop < plugin.StopTimeout+plugin.StopCloseMargin+time.Second {
		t.Fatalf("the stop returned in %v, before the write-mutex stall ended", stop)
	}
	if res.Lost == 0 || uint64(res.Lost) != res.Dropped["stopped"] {
		t.Fatalf("want the queued work and the group on the mutex lost and counted stopped: %v", res)
	}
}

// TestStallHarnessCheckpointModes measures, for the design's maintenance-checkpoint choice, how long
// wal_checkpoint(TRUNCATE) and wal_checkpoint(PASSIVE) hold a connection after the same body-heavy
// load on a slow disk (every outbox fsync delayed CA_STALL_SWEEP_SYNC_MS, default 80 ms), and how
// many syncs each issues. In the plugin the checkpoint runs on the single write connection, so its
// duration is what the capture writer waits. Opt-in (CA_STALL_SWEEP=1); it logs, asserts nothing.
func TestStallHarnessCheckpointModes(t *testing.T) {
	if os.Getenv("CA_STALL_SWEEP") != "1" {
		t.Skip("set CA_STALL_SWEEP=1 to run the checkpoint measurement")
	}
	syncMS := 80
	if s := os.Getenv("CA_STALL_SWEEP_SYNC_MS"); s != "" {
		fmt.Sscan(s, &syncMS)
	}
	setVar(t, &plugin.GCEvery, time.Hour) // no maintenance checkpoint during the load
	for _, mode := range []string{"TRUNCATE", "PASSIVE", "TRUNCATE", "PASSIVE"} {
		t.Run(mode, func(t *testing.T) {
			r := newStallRun(t)
			r.withBody(64)
			r.withResponse(64)
			r.arm(diskstall.Rule{Ops: diskstall.Sync, Delay: time.Duration(syncMS) * time.Millisecond})
			r.loadFor(10*time.Second, 100*time.Millisecond)
			r.settle()
			r.quiet()
			count := r.arm(diskstall.Rule{Ops: diskstall.Sync}) // counts syncs (no wait)
			u := url.URL{Scheme: "file", Path: r.ob.Path(), RawQuery: "_busy_timeout=10000"}
			db, err := sql.Open("sqlite3", u.String())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var walBefore int64
			if fi, err := os.Stat(r.ob.Path() + "-wal"); err == nil {
				walBefore = fi.Size()
			}
			before := count.Stats().Matched
			start := time.Now()
			var busy, logFrames, ckpt int
			if err := db.QueryRow("PRAGMA wal_checkpoint("+mode+")").Scan(&busy, &logFrames, &ckpt); err != nil {
				t.Fatal(err)
			}
			d := time.Since(start)
			var walAfter int64
			if fi, err := os.Stat(r.ob.Path() + "-wal"); err == nil {
				walAfter = fi.Size()
			}
			t.Logf("mode=%s sync_ms=%d checkpoint=%v syncs=%d busy=%d frames=%d checkpointed=%d wal_kib_before=%d wal_kib_after=%d",
				mode, syncMS, d.Round(time.Millisecond), count.Stats().Matched-before, busy, logFrames, ckpt, walBefore/1024, walAfter/1024)
			r.result(nil, 0)
		})
	}
}
