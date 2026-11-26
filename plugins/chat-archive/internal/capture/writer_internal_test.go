package capture

// The 0.1.6 writer (K10): no attempt timeout while running, failed attempts retried in order for
// RetryWindow, the stop's deadline cutting every wait the writer owns. The outbox write is replaced
// by one that fails or blocks as a test says (Runtime.obWrite). Every value is synthetic.

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sqlite "github.com/mattn/go-sqlite3"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/safego"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// fastRetry shortens the writer's retry window and backoff for one test.
func fastRetry(t *testing.T, window time.Duration) {
	t.Helper()
	w, lo, hi := RetryWindow, RetryBackoffMin, RetryBackoffMax
	RetryWindow, RetryBackoffMin, RetryBackoffMax = window, time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { RetryWindow, RetryBackoffMin, RetryBackoffMax = w, lo, hi })
}

// writerRuntime is a runtime with only its writer running, whose outbox writes go through write
// (given the real write to call).
func writerRuntime(t *testing.T, write func(ctx context.Context, envs []outbox.NewEnvelope, real func(context.Context, []outbox.NewEnvelope) (outbox.WriteResult, error)) (outbox.WriteResult, error)) (*Runtime, *hooks.Engine, *outbox.Outbox) {
	t.Helper()
	e := hooks.NewEngine()
	ob, err := outbox.Open(outbox.Options{Dir: filepath.Join(t.TempDir(), "data"), CapBytes: 1 << 40, Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ob.Close() })
	m, _ := mask.New(mask.Config{})
	rt := &Runtime{opts: Options{Engine: e, Outbox: ob, Workers: 1, Masker: NewMaskerHolder(m, 0, 1<<20)}, prefix: NewPrefixMap(),
		writes: make(chan writeReq, WriteQueueEntries)}
	rt.initContexts()
	rt.obWrite = func(ctx context.Context, envs []outbox.NewEnvelope) (outbox.WriteResult, error) {
		return write(ctx, envs, ob.Write)
	}
	safego.Loop(rt.writerCtx, &rt.writerWG, "writer", e.Stats.Panic, rt.writer)
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { rt.StopBy(time.Now().Add(2 * time.Second)) }) })
	return rt, e, ob
}

// queueEnv hands env to the writer as a producer does.
func queueEnv(rt *Runtime, env outbox.NewEnvelope) bool { return rt.write(env, nil, nil, 0, 0) }

func syntheticEnv(i int) outbox.NewEnvelope {
	return outbox.NewEnvelope{Kind: wire.KindExchangeDone, Key: fmt.Sprintf("ex/writer-%04d/completion", i), CreatedAt: time.Now(),
		Body: json.RawMessage(fmt.Sprintf(`{"request_id":"writer-%04d"}`, i))}
}

func waitUntil(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(d); !cond(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// pendingKeys returns the keys of the outbox's pending envelopes in seq order.
func pendingKeys(t *testing.T, ob *outbox.Outbox) []string {
	t.Helper()
	items, err := ob.Pending(context.Background(), time.Now().Add(time.Hour), 100000, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(items))
	for _, it := range items {
		keys = append(keys, it.Key)
	}
	return keys
}

// TestWriterRetriesInOrder: a write that fails (busy, locked, an I/O error, a full disk, a file that
// cannot be opened) several times and then succeeds loses no envelope; the group keeps its place,
// so every envelope behind it is written after it; write_unavailable is raised while the writes
// fail and cleared by the first one that succeeds. 0.1.5 dropped the group after 3 quick retries and
// every later group for 5 s.
func TestWriterRetriesInOrder(t *testing.T) {
	for name, code := range map[string]sqlite.ErrNo{"busy": sqlite.ErrBusy, "locked": sqlite.ErrLocked, "io": sqlite.ErrIoErr,
		"full": sqlite.ErrFull, "cantopen": sqlite.ErrCantOpen} {
		t.Run(name, func(t *testing.T) {
			fastRetry(t, 5*time.Second)
			const n, failN = 120, 6
			var calls atomic.Int32
			var raised atomic.Bool
			var e *hooks.Engine
			rt, e, ob := writerRuntime(t, func(ctx context.Context, envs []outbox.NewEnvelope, real func(context.Context, []outbox.NewEnvelope) (outbox.WriteResult, error)) (outbox.WriteResult, error) {
				if calls.Add(1) <= failN {
					if calls.Load() > 1 && e.Stats.HasFault("write_unavailable") {
						raised.Store(true)
					}
					return outbox.WriteResult{}, sqlite.Error{Code: code}
				}
				return real(ctx, envs)
			})
			for i := 0; i < n; i++ {
				if !queueEnv(rt, syntheticEnv(i)) {
					t.Fatalf("write %d refused", i)
				}
			}
			waitUntil(t, "every envelope written", 10*time.Second, func() bool { return len(pendingKeys(t, ob)) == n })
			keys := pendingKeys(t, ob)
			for i, k := range keys {
				if want := syntheticEnv(i).Key; k != want {
					t.Fatalf("envelope %d is %s, want %s: order not kept", i, k, want)
				}
			}
			if d := e.Stats.Dropped(); d["disk_hard"] != 0 || d["stopped"] != 0 {
				t.Fatalf("dropped %v", d)
			}
			if !raised.Load() || e.Stats.HasFault("write_unavailable") {
				t.Fatalf("write_unavailable raised while failing %v, still raised %v", raised.Load(), e.Stats.HasFault("write_unavailable"))
			}
		})
	}
}

// TestWriterRetryWindowBoundary: with the disk failing for longer than RetryWindow, the head group
// is dropped (disk_hard) only once an attempt that started at or after its first failure plus the
// window fails; the backoff is clamped so that such an attempt starts close to the window's end.
// A recovery just before the window's end loses nothing.
func TestWriterRetryWindowBoundary(t *testing.T) {
	const window = 300 * time.Millisecond
	t.Run("past", func(t *testing.T) {
		fastRetry(t, window)
		var starts []time.Time
		var mu sync.Mutex
		failing := atomic.Bool{}
		failing.Store(true)
		rt, e, ob := writerRuntime(t, func(ctx context.Context, envs []outbox.NewEnvelope, real func(context.Context, []outbox.NewEnvelope) (outbox.WriteResult, error)) (outbox.WriteResult, error) {
			if failing.Load() {
				mu.Lock()
				starts = append(starts, time.Now())
				mu.Unlock()
				return outbox.WriteResult{}, sqlite.Error{Code: sqlite.ErrIoErr}
			}
			return real(ctx, envs)
		})
		queueEnv(rt, syntheticEnv(0))
		waitUntil(t, "the head group dropped", 5*time.Second, func() bool { return e.Stats.Dropped()["disk_hard"] == 1 })
		failing.Store(false)
		mu.Lock()
		first, last := starts[0], starts[len(starts)-1]
		mu.Unlock()
		if last.Sub(first) < window || last.Sub(first) > window+100*time.Millisecond {
			t.Fatalf("the final attempt started %v after the first failure, want at or just past %v", last.Sub(first), window)
		}
		// The next group gets its own window, and is written once the disk is back.
		queueEnv(rt, syntheticEnv(1))
		waitUntil(t, "the next group written", 5*time.Second, func() bool { return len(pendingKeys(t, ob)) == 1 })
		if e.Stats.Dropped()["disk_hard"] != 1 {
			t.Fatalf("disk_hard %d, want only the head group", e.Stats.Dropped()["disk_hard"])
		}
	})
	t.Run("recovers_before_end", func(t *testing.T) {
		fastRetry(t, window)
		var first atomic.Int64
		rt, e, ob := writerRuntime(t, func(ctx context.Context, envs []outbox.NewEnvelope, real func(context.Context, []outbox.NewEnvelope) (outbox.WriteResult, error)) (outbox.WriteResult, error) {
			now := time.Now().UnixNano()
			first.CompareAndSwap(0, now)
			if time.Duration(now-first.Load()) < window-30*time.Millisecond {
				return outbox.WriteResult{}, sqlite.Error{Code: sqlite.ErrIoErr}
			}
			return real(ctx, envs)
		})
		queueEnv(rt, syntheticEnv(0))
		waitUntil(t, "the group written", 5*time.Second, func() bool { return len(pendingKeys(t, ob)) == 1 })
		if d := e.Stats.Dropped(); d["disk_hard"] != 0 {
			t.Fatalf("dropped %v", d)
		}
	})
	t.Run("attempt_spanning_end", func(t *testing.T) {
		// An attempt that started before the window's end and failed after it is followed by one
		// more attempt at once, which succeeds on a healthy disk.
		fastRetry(t, window)
		var calls atomic.Int32
		rt, e, ob := writerRuntime(t, func(ctx context.Context, envs []outbox.NewEnvelope, real func(context.Context, []outbox.NewEnvelope) (outbox.WriteResult, error)) (outbox.WriteResult, error) {
			switch calls.Add(1) {
			case 1:
				return outbox.WriteResult{}, sqlite.Error{Code: sqlite.ErrIoErr}
			case 2:
				time.Sleep(window + 50*time.Millisecond) // held across the window's end, then failed
				return outbox.WriteResult{}, sqlite.Error{Code: sqlite.ErrIoErr}
			}
			return real(ctx, envs)
		})
		queueEnv(rt, syntheticEnv(0))
		waitUntil(t, "the group written", 5*time.Second, func() bool { return len(pendingKeys(t, ob)) == 1 })
		if d := e.Stats.Dropped(); d["disk_hard"] != 0 || calls.Load() != 3 {
			t.Fatalf("dropped %v after %d attempts, want 0 after 3", d, calls.Load())
		}
	})
}

// TestWriterPermanentErrorDropsAtOnce: an error no retry can fix (a closed outbox, a constraint)
// drops the group as disk_hard at once, without a retry, and the next group is written.
func TestWriterPermanentErrorDropsAtOnce(t *testing.T) {
	fastRetry(t, 10*time.Second)
	var calls atomic.Int32
	rt, e, ob := writerRuntime(t, func(ctx context.Context, envs []outbox.NewEnvelope, real func(context.Context, []outbox.NewEnvelope) (outbox.WriteResult, error)) (outbox.WriteResult, error) {
		if calls.Add(1) == 1 {
			return outbox.WriteResult{}, sqlite.Error{Code: sqlite.ErrConstraint}
		}
		return real(ctx, envs)
	})
	queueEnv(rt, syntheticEnv(0))
	waitUntil(t, "the group dropped", 2*time.Second, func() bool { return e.Stats.Dropped()["disk_hard"] == 1 })
	queueEnv(rt, syntheticEnv(1))
	waitUntil(t, "the next group written", 2*time.Second, func() bool { return len(pendingKeys(t, ob)) == 1 })
	if calls.Load() != 2 {
		t.Fatalf("%d attempts, want 2 (no retry of a permanent error)", calls.Load())
	}
}

// TestStopCutsWriterWaits: the stop's deadline cuts every wait the writer owns. A write waiting for
// the outbox's connection (here: blocked until its context ends) and a write in a retry's backoff
// both end at the deadline; their group is counted stopped, and envelopes still queued behind it
// are counted stopped without an attempt. Stored plus counted equals offered.
func TestStopCutsWriterWaits(t *testing.T) {
	for _, mode := range []string{"connection_wait", "backoff"} {
		t.Run(mode, func(t *testing.T) {
			old := RetryBackoffMin
			RetryBackoffMin, RetryBackoffMax = 5*time.Second, 5*time.Second
			t.Cleanup(func() { RetryBackoffMin, RetryBackoffMax = old, 2*time.Second })
			var attempts atomic.Int32
			var afterCut atomic.Int32
			rt, e, ob := writerRuntime(t, func(ctx context.Context, envs []outbox.NewEnvelope, real func(context.Context, []outbox.NewEnvelope) (outbox.WriteResult, error)) (outbox.WriteResult, error) {
				attempts.Add(1)
				if ctx.Err() != nil {
					afterCut.Add(1)
				}
				if mode == "connection_wait" {
					<-ctx.Done()
					return outbox.WriteResult{}, ctx.Err()
				}
				return outbox.WriteResult{}, sqlite.Error{Code: sqlite.ErrIoErr}
			})
			const n = 200
			for i := 0; i < n; i++ {
				queueEnv(rt, syntheticEnv(i))
			}
			waitUntil(t, "the first attempt", 2*time.Second, func() bool { return attempts.Load() >= 1 })
			start := time.Now()
			rt.StopBy(time.Now().Add(300 * time.Millisecond))
			if d := time.Since(start); d > time.Second {
				t.Fatalf("the stop took %v with a 300 ms deadline", d)
			}
			stored := len(pendingKeys(t, ob))
			if got := int(e.Stats.Dropped()["stopped"]); stored+got != n || e.Stats.Dropped()["disk_hard"] != 0 {
				t.Fatalf("stored %d, stopped %d, dropped %v; want stored + stopped = %d", stored, got, e.Stats.Dropped(), n)
			}
			if afterCut.Load() != 0 || attempts.Load() != 1 {
				t.Fatalf("%d attempts, %d after the cut; want 1 and 0", attempts.Load(), afterCut.Load())
			}
		})
	}
}
