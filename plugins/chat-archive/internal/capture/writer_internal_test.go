package capture

// The writer under outbox write failures (plugin 0.1.4 dropped 111 envelopes when its outbox writes
// stalled under disk pressure). The outbox write is replaced by one that fails for a while, or the
// real SQLite write lock is held by another connection. Every value is synthetic.

import (
	"context"
	"database/sql"
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

// fastRetries shortens the writer's attempt budget and backoff for one test.
func fastRetries(t *testing.T, attempt time.Duration) {
	t.Helper()
	wt, lo, hi := WriteTimeout, RetryBackoffMin, RetryBackoffMax
	WriteTimeout, RetryBackoffMin, RetryBackoffMax = attempt, time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { WriteTimeout, RetryBackoffMin, RetryBackoffMax = wt, lo, hi })
}

func openOutbox(t *testing.T, capBytes int64) *outbox.Outbox {
	t.Helper()
	ob, err := outbox.Open(outbox.Options{Dir: filepath.Join(t.TempDir(), "data"), CapBytes: capBytes, Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ob.Close() })
	return ob
}

// hardPressure is an outbox at pressure level 3 (more than 1 GiB above its cap).
func hardPressure(t *testing.T) *outbox.Outbox { return openOutbox(t, -2<<30) }

// writerRuntime is a runtime with its writer running (and, when pipeline is set, its worker and
// stream goroutine) whose outbox writes go through write, which gets the real write to call.
func writerRuntime(t *testing.T, pipeline bool, write func(ctx context.Context, envs []outbox.NewEnvelope, real func(context.Context, []outbox.NewEnvelope) (outbox.WriteResult, error)) (outbox.WriteResult, error)) (*Runtime, *hooks.Engine, *outbox.Outbox) {
	t.Helper()
	e := hooks.NewEngine()
	ob := openOutbox(t, 1<<40)
	m, _ := mask.New(mask.Config{})
	rt := &Runtime{opts: Options{Engine: e, Outbox: ob, Workers: 1, Masker: NewMaskerHolder(m, 0, 1<<20)}, prefix: NewPrefixMap(),
		writes: make(chan writeReq, 1024)}
	rt.obWrite = func(ctx context.Context, envs []outbox.NewEnvelope) (outbox.WriteResult, error) {
		return write(ctx, envs, ob.Write)
	}
	rt.runCtx, rt.runCancel = context.WithCancel(context.Background())
	rt.seqCtx, rt.seqCancel = context.WithCancel(context.Background())
	rt.drainCtx, rt.drainStop = context.WithCancel(context.Background())
	rt.writerCtx, rt.writerEnd = context.WithCancel(context.Background())
	if pipeline {
		safego.Loop(rt.runCtx, &rt.wg, "worker", e.Stats.Panic, rt.worker)
		safego.Loop(rt.seqCtx, &rt.seqWG, "stream", e.Stats.Panic, rt.sequencer)
	}
	safego.Loop(rt.writerCtx, &rt.writerWG, "writer", e.Stats.Panic, rt.writer)
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { rt.StopBy(time.Now().Add(2 * time.Second)) }) })
	return rt, e, ob
}

func syntheticEnv(i int) outbox.NewEnvelope {
	return outbox.NewEnvelope{Kind: wire.KindExchangeDone, Key: fmt.Sprintf("ex/held-%04d/completion", i), CreatedAt: time.Now(),
		Body: json.RawMessage(fmt.Sprintf(`{"request_id":"held-%04d"}`, i))}
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

// TestFailedWriteKeptAndRetriedInOrder: an outbox write that fails (busy, an I/O error) or times out
// N times and then succeeds loses no envelope. The failing group keeps its place, so every envelope
// queued behind it is written after it, in order; write_unavailable is raised while the writes fail
// and cleared by the first one that succeeds. 0.1.4 dropped the group (dropped.disk_hard) and then
// every later group untried until a probe succeeded.
func TestFailedWriteKeptAndRetriedInOrder(t *testing.T) {
	const n, failN = 160, 7
	cases := map[string]func(ctx context.Context) error{
		"busy":     func(context.Context) error { return sqlite.Error{Code: sqlite.ErrBusy} },
		"io_error": func(context.Context) error { return sqlite.Error{Code: sqlite.ErrIoErr} },
		"full":     func(context.Context) error { return sqlite.Error{Code: sqlite.ErrFull} },
		"timeout":  func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, // a stalled fsync past the attempt budget
	}
	for name, fail := range cases {
		t.Run(name, func(t *testing.T) {
			fastRetries(t, 20*time.Millisecond)
			var calls atomic.Int32
			var faultWhileFailing atomic.Bool
			var e *hooks.Engine
			rt, e, ob := writerRuntime(t, false, func(ctx context.Context, envs []outbox.NewEnvelope, real func(context.Context, []outbox.NewEnvelope) (outbox.WriteResult, error)) (outbox.WriteResult, error) {
				if c := calls.Add(1); c <= failN {
					return outbox.WriteResult{}, fail(ctx)
				} else if c == failN+1 {
					faultWhileFailing.Store(e.Stats.HasFault("write_unavailable"))
				}
				return real(ctx, envs)
			})
			var mu sync.Mutex
			var committed []string
			for i := 0; i < n; i++ {
				env := syntheticEnv(i)
				if !rt.write(env, func(inserted bool) {
					mu.Lock()
					defer mu.Unlock()
					if inserted {
						committed = append(committed, env.Key)
					}
				}, nil) {
					t.Fatal("write not accepted")
				}
			}
			waitUntil(t, "every envelope committed", 10*time.Second, func() bool {
				mu.Lock()
				defer mu.Unlock()
				return len(committed) == n
			})
			if d := e.Stats.Dropped(); e.Stats.DroppedTotal() != 0 {
				t.Fatalf("dropped = %v, want none", d)
			}
			if c := calls.Load(); c <= failN {
				t.Fatalf("write calls = %d, want more than %d", c, failN)
			}
			keys := pendingKeys(t, ob)
			if len(keys) != n {
				t.Fatalf("stored %d envelopes, want %d", len(keys), n)
			}
			for i, k := range keys {
				if want := syntheticEnv(i).Key; k != want || committed[i] != want {
					t.Fatalf("envelope %d stored as %q (committed %q), want %q: order not kept", i, k, committed[i], want)
				}
			}
			if !faultWhileFailing.Load() {
				t.Fatal("write_unavailable was not raised while the writes failed")
			}
			if e.Stats.HasFault("write_unavailable") {
				t.Fatal("write_unavailable still raised after the writes succeeded")
			}
		})
	}
}

// TestRealBusyLockOutlastsRetries: another connection holds the outbox's SQLite write lock for
// longer than 0.1.4's three busy retries (about 0.4 s); the group waits for the lock and is stored.
func TestRealBusyLockOutlastsRetries(t *testing.T) {
	fastRetries(t, WriteTimeout)
	rt, e, ob := writerRuntime(t, false, func(ctx context.Context, envs []outbox.NewEnvelope, real func(context.Context, []outbox.NewEnvelope) (outbox.WriteResult, error)) (outbox.WriteResult, error) {
		return real(ctx, envs)
	})
	db, err := sql.Open("sqlite3", "file:"+ob.Path()+"?_busy_timeout=100")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
		conn.Close()
	}()
	for i := 0; i < 3; i++ {
		rt.write(syntheticEnv(i), nil, nil)
	}
	waitUntil(t, "write_unavailable", 10*time.Second, func() bool { return e.Stats.HasFault("write_unavailable") })
	time.Sleep(300 * time.Millisecond) // past 0.1.4's retries, which dropped the group
	if _, err = conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	locked = false
	waitUntil(t, "the held group stored", 10*time.Second, func() bool { return len(pendingKeys(t, ob)) == 3 })
	if e.Stats.DroppedTotal() != 0 {
		t.Fatalf("dropped = %v", e.Stats.Dropped())
	}
	waitUntil(t, "write_unavailable cleared", 10*time.Second, func() bool { return !e.Stats.HasFault("write_unavailable") })
}

// bodyDrops are the counters of an exchange part stored as metadata only; every other dropped
// counter is a whole envelope lost.
var bodyDrops = map[string]bool{"body_budget": true, "handoff_full": true}

func dropSplit(d map[string]uint64) (bodies, envelopes uint64) {
	for k, v := range d {
		if bodyDrops[k] {
			bodies += v
		} else {
			envelopes += v
		}
	}
	return
}

// TestSustainedWriteFailureDegradesBodiesFirst: while every outbox write fails, the writer holds its
// group, the write queue and then the hooks' queues fill, and new begins are kept as metadata only
// (handoff_full) long before any whole envelope is lost; the first whole-envelope drop is
// control_full with the control queue full, never disk_hard. Once the disk recovers, everything
// still queued is written, and the counters account for every begin.
func TestSustainedWriteFailureDegradesBodiesFirst(t *testing.T) {
	fastRetries(t, 20*time.Millisecond)
	var failing atomic.Bool
	failing.Store(true)
	rt, e, ob := writerRuntime(t, true, func(ctx context.Context, envs []outbox.NewEnvelope, real func(context.Context, []outbox.NewEnvelope) (outbox.WriteResult, error)) (outbox.WriteResult, error) {
		if failing.Load() {
			return outbox.WriteResult{}, sqlite.Error{Code: sqlite.ErrIoErr}
		}
		return real(ctx, envs)
	})
	e.SetConfig(&hooks.Config{Configured: true, Capture: "full", MaxRPC: 1 << 20})
	e.Start()
	// The queue budget in envelopes: the write queue, the writer's group, the worker's and the
	// stream goroutine's envelope in hand, Handoff and Control.
	budget := cap(rt.writes) + groupEnvelopes + 2 + cap(e.Handoff) + cap(e.Control)
	n := budget + 400
	// settle waits until the worker and the stream goroutine took what the hooks queued, or are
	// blocked on the full write queue: steady traffic rather than a burst that outruns them.
	settle := func() {
		waitUntil(t, "the pipeline settled", 10*time.Second, func() bool {
			return (len(e.Handoff) == 0 && len(e.Control) == 0) || len(rt.writes) == cap(rt.writes)
		})
	}
	firstBody, firstEnvelope := -1, -1
	for i := 0; i < n; i++ {
		e.BeforeAuth(rawJSON(map[string]any{"RequestID": fmt.Sprintf("held-%05d", i), "SourceFormat": "claude", "Model": "model-x",
			"Body": []byte(`{"model":"model-x","max_tokens":1,"messages":[{"role":"user","content":"synthetic held text"}]}`)}))
		settle()
		bodies, envelopes := dropSplit(e.Stats.Dropped())
		if bodies > 0 && firstBody < 0 {
			firstBody = i
		}
		if envelopes > 0 && firstEnvelope < 0 {
			firstEnvelope = i
			if d := e.Stats.Dropped(); d["control_full"] != envelopes || len(e.Control) != cap(e.Control) {
				t.Fatalf("first whole-envelope drop at begin %d: dropped %v with control queue %d of %d (want control_full on a full queue)",
					i, d, len(e.Control), cap(e.Control))
			}
		}
	}
	d := e.Stats.Dropped()
	if firstBody < 0 || firstEnvelope < 0 || firstBody >= firstEnvelope {
		t.Fatalf("first body drop at begin %d, first envelope drop at %d (dropped %v): bodies must degrade first", firstBody, firstEnvelope, d)
	}
	if firstEnvelope < cap(e.Control) {
		t.Fatalf("an envelope was dropped at begin %d, before the control queue (%d) could fill", firstEnvelope, cap(e.Control))
	}
	if d["disk_hard"] != 0 || d["stopped"] != 0 {
		t.Fatalf("dropped = %v: the writer dropped held envelopes", d)
	}
	if !e.Stats.HasFault("write_unavailable") {
		t.Fatal("write_unavailable not raised")
	}
	if used := e.Pool.Used(); used <= 0 || used > hooks.PoolBytes {
		t.Fatalf("content pool used = %d while held (budget %d)", used, hooks.PoolBytes)
	}

	failing.Store(false)
	want := uint64(n) - d["control_full"]
	waitUntil(t, "the queued begins written", 30*time.Second, func() bool { return uint64(len(pendingKeys(t, ob))) == want })
	items, err := ob.Pending(context.Background(), time.Now().Add(time.Hour), 100000, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	var metaOnly uint64
	for _, it := range items {
		env, err := wire.DecodeEnvelope(it.Envelope)
		if err != nil || env.Kind != wire.KindExchangeBegin {
			t.Fatalf("envelope %q kind %q err %v", it.Key, env.Kind, err)
		}
		var b wire.ExchangeBegin
		if err := json.Unmarshal(env.Body, &b); err != nil {
			t.Fatal(err)
		}
		if b.Body.Dropped != "" {
			metaOnly++
		}
	}
	end := e.Stats.Dropped()
	// Every begin past the first body drop lost its body (handoff_full); those that then found the
	// control queue full were lost whole (control_full), the rest are stored as metadata only.
	if bodies, _ := dropSplit(end); metaOnly == 0 || metaOnly+end["control_full"] != bodies {
		t.Fatalf("metadata-only begins stored = %d + control_full %d, body drops counted = %d (%v)", metaOnly, end["control_full"], bodies, end)
	}
	if end["control_full"] != d["control_full"] || uint64(len(items))+end["control_full"] != uint64(n) {
		t.Fatalf("stored %d + control_full %d != %d begins (%v)", len(items), end["control_full"], n, end)
	}
	waitUntil(t, "write_unavailable cleared", 10*time.Second, func() bool { return !e.Stats.HasFault("write_unavailable") })
}

// TestPermanentWriteErrorDropsOnlyItsGroup: an error no retry can fix (here a constraint) drops its
// group at once, counted disk_hard, instead of holding every later envelope behind it.
func TestPermanentWriteErrorDropsOnlyItsGroup(t *testing.T) {
	fastRetries(t, WriteTimeout)
	var calls atomic.Int32
	rt, e, ob := writerRuntime(t, false, func(ctx context.Context, envs []outbox.NewEnvelope, real func(context.Context, []outbox.NewEnvelope) (outbox.WriteResult, error)) (outbox.WriteResult, error) {
		if calls.Add(1) == 1 {
			return outbox.WriteResult{}, sqlite.Error{Code: sqlite.ErrConstraint}
		}
		return real(ctx, envs)
	})
	rt.write(syntheticEnv(0), nil, nil)
	waitUntil(t, "the poison group dropped", 10*time.Second, func() bool { return e.Stats.Dropped()["disk_hard"] == 1 })
	rt.write(syntheticEnv(1), nil, nil)
	waitUntil(t, "the next group stored", 10*time.Second, func() bool { return len(pendingKeys(t, ob)) == 1 })
	if c := calls.Load(); c != 2 {
		t.Fatalf("write calls = %d, want 2 (no retry of a permanent error)", c)
	}
}

// TestHeldGroupBoundedByStop: a group held by failing writes does not hold a stop past its deadline;
// it is counted dropped.stopped.
func TestHeldGroupBoundedByStop(t *testing.T) {
	fastRetries(t, 50*time.Millisecond)
	rt, e, _ := writerRuntime(t, false, func(context.Context, []outbox.NewEnvelope, func(context.Context, []outbox.NewEnvelope) (outbox.WriteResult, error)) (outbox.WriteResult, error) {
		return outbox.WriteResult{}, sqlite.Error{Code: sqlite.ErrIoErr}
	})
	for i := 0; i < 3; i++ {
		rt.write(syntheticEnv(i), nil, nil)
	}
	waitUntil(t, "write_unavailable", 10*time.Second, func() bool { return e.Stats.HasFault("write_unavailable") })
	start := time.Now()
	rt.StopBy(start.Add(300 * time.Millisecond))
	if d := time.Since(start); d > time.Second {
		t.Fatalf("StopBy took %v past a 300ms deadline", d)
	}
	if d := e.Stats.Dropped(); d["stopped"] != 3 || e.Stats.DroppedTotal() != 3 {
		t.Fatalf("dropped = %v, want stopped 3", d)
	}
}
