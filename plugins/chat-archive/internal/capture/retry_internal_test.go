package capture

import (
	"context"
	"io/fs"
	"reflect"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	sqlite "github.com/mattn/go-sqlite3"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// setRetry shortens the writer's retry timings for one test.
func setRetry(t *testing.T, attempt, window, backoff time.Duration) {
	t.Helper()
	a, w, b := WriteAttemptTimeout, WriteRetryWindow, WriteRetryBackoff
	WriteAttemptTimeout, WriteRetryWindow, WriteRetryBackoff = attempt, window, backoff
	t.Cleanup(func() { WriteAttemptTimeout, WriteRetryWindow, WriteRetryBackoff = a, w, b })
}

func retryEnv(t *testing.T, rid string) outbox.NewEnvelope {
	t.Helper()
	body, err := canon.Marshal(wire.ExchangeAttempt{RequestID: rid, Ordinal: 1, ObservedAt: time.Unix(1, 0).UTC(), ToFormat: "claude", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	return outbox.NewEnvelope{Kind: wire.KindExchangeAttempt, Key: wire.ExchangeAttemptKey(rid, 1), CreatedAt: time.Unix(1, 0), Body: body}
}

// nonZeroDrops is the engine's dropped counters that are not zero.
func nonZeroDrops(e *hooks.Engine) map[string]uint64 {
	d := map[string]uint64{}
	for k, v := range e.Stats.Dropped() {
		if v != 0 {
			d[k] = v
		}
	}
	return d
}

// blockWrite is an outbox write that hangs until its context ends (a stalled disk).
func blockWrite(ctx context.Context, _ []outbox.NewEnvelope) (outbox.WriteResult, error) {
	<-ctx.Done()
	return outbox.WriteResult{}, ctx.Err()
}

// TestWriteRetriesTransientFailuresInOrder: a group whose outbox write fails with busy, locked,
// I/O errors or times out, several times within the retry window, is written once the disk
// recovers: nothing is dropped, no fault is raised, and the later group follows it in order.
func TestWriteRetriesTransientFailuresInOrder(t *testing.T) {
	setRetry(t, 50*time.Millisecond, 10*time.Second, time.Millisecond)
	rt, e, ob := stoppingRuntime(t, false)
	fails := []error{
		sqlite.Error{Code: sqlite.ErrBusy},
		sqlite.Error{Code: sqlite.ErrLocked},
		sqlite.Error{Code: sqlite.ErrIoErr, ExtendedCode: sqlite.ErrIoErrFsync},
		&fs.PathError{Op: "write", Path: "blob", Err: syscall.EIO},
		nil, // a timeout: the write hangs past its attempt budget
		nil,
		sqlite.Error{Code: sqlite.ErrBusy},
	}
	var calls atomic.Int32
	firstCall := make(chan struct{})
	var once sync.Once
	rt.outboxWrite = func(ctx context.Context, envs []outbox.NewEnvelope) (outbox.WriteResult, error) {
		once.Do(func() { close(firstCall) })
		n := int(calls.Add(1)) - 1
		if n < len(fails) {
			if fails[n] == nil {
				return blockWrite(ctx, envs)
			}
			return outbox.WriteResult{}, fails[n]
		}
		return ob.Write(ctx, envs)
	}
	var committed []bool
	var mu sync.Mutex
	commit := func(ok bool) { mu.Lock(); committed = append(committed, ok); mu.Unlock() }
	var released atomic.Int32
	release := func() { released.Add(1) }
	group := []writeReq{
		{env: retryEnv(t, "retry-a1"), commit: commit, release: release},
		{env: retryEnv(t, "retry-a2"), commit: commit, release: release},
		{env: retryEnv(t, "retry-a3"), commit: commit, release: release},
	}
	var probe time.Time
	done := make(chan struct{})
	go func() {
		defer close(done)
		rt.flush(group, &probe)
		rt.flush([]writeReq{{env: retryEnv(t, "retry-b1"), commit: commit, release: release}}, &probe)
	}()
	<-firstCall
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("flush did not finish")
	}
	if d := nonZeroDrops(e); len(d) != 0 {
		t.Fatalf("dropped = %v, want none", d)
	}
	if e.Stats.HasFault("write_unavailable") {
		t.Fatal("write_unavailable raised for failures inside the retry window")
	}
	if got := int(calls.Load()); got != len(fails)+2 {
		t.Fatalf("write calls = %d, want %d", got, len(fails)+2)
	}
	if !reflect.DeepEqual(committed, []bool{true, true, true, true}) || released.Load() != 4 {
		t.Fatalf("commits %v, releases %d", committed, released.Load())
	}
	items, err := ob.Pending(context.Background(), time.Now(), 10, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, it := range items {
		keys = append(keys, it.Key)
	}
	want := []string{wire.ExchangeAttemptKey("retry-a1", 1), wire.ExchangeAttemptKey("retry-a2", 1),
		wire.ExchangeAttemptKey("retry-a3", 1), wire.ExchangeAttemptKey("retry-b1", 1)}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("outbox order = %v, want %v", keys, want)
	}
}

// TestWriteStallPastRetryWindowDropsAsBefore: a write that stalls longer than the retry window
// ends exactly as 0.1.4's failed write: the group is counted disk_hard, its commits report not
// inserted, its budgets are released, write_unavailable is raised and the next group inside the
// probe interval is dropped untried.
func TestWriteStallPastRetryWindowDropsAsBefore(t *testing.T) {
	setRetry(t, 40*time.Millisecond, 150*time.Millisecond, 10*time.Millisecond)
	rt, e, _ := stoppingRuntime(t, false)
	var calls atomic.Int32
	rt.outboxWrite = func(ctx context.Context, envs []outbox.NewEnvelope) (outbox.WriteResult, error) {
		calls.Add(1)
		return blockWrite(ctx, envs)
	}
	var committed []bool
	commit := func(ok bool) { committed = append(committed, ok) }
	var released int
	release := func() { released++ }
	group := []writeReq{
		{env: retryEnv(t, "stall-1"), commit: commit, release: release},
		{env: retryEnv(t, "stall-2"), commit: commit, release: release},
		{env: retryEnv(t, "stall-3"), commit: commit, release: release},
	}
	var probe time.Time
	start := time.Now()
	rt.flush(group, &probe)
	took := time.Since(start)
	if d := nonZeroDrops(e); !reflect.DeepEqual(d, map[string]uint64{"disk_hard": 3}) {
		t.Fatalf("dropped = %v, want disk_hard 3", d)
	}
	if !e.Stats.HasFault("write_unavailable") {
		t.Fatal("write_unavailable not raised")
	}
	if !reflect.DeepEqual(committed, []bool{false, false, false}) || released != 3 {
		t.Fatalf("commits %v, releases %d", committed, released)
	}
	if probe.Before(start) {
		t.Fatal("the probe interval did not restart at the failure")
	}
	// The probe gate is unchanged: the next group inside the interval is dropped untried.
	before := calls.Load()
	rt.flush([]writeReq{{env: retryEnv(t, "stall-4"), commit: commit, release: release}}, &probe)
	if d := e.Stats.Dropped()["disk_hard"]; d != 4 || calls.Load() != before {
		t.Fatalf("after the stall: disk_hard %d, write calls %d -> %d", d, before, calls.Load())
	}
	// The stalled group was retried until the window passed, and not much longer.
	if before < 2 || took < WriteRetryWindow || took > WriteRetryWindow+WriteAttemptTimeout+500*time.Millisecond {
		t.Errorf("stalled group: %d attempts in %v, want retries for the %v window", before, took, WriteRetryWindow)
	}
}

// TestStopDuringWriteRetryReturnsByDeadline: a stop whose deadline passes while the writer is
// retrying a failing group, inside an attempt or inside the backoff, returns by that deadline and
// counts the group stopped (the stop, not the disk, ended the write).
func TestStopDuringWriteRetryReturnsByDeadline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backoff time.Duration
		write   func(context.Context, []outbox.NewEnvelope) (outbox.WriteResult, error)
	}{
		{"in_flight_attempt", time.Millisecond, blockWrite},
		{"backoff", 5 * time.Second, func(context.Context, []outbox.NewEnvelope) (outbox.WriteResult, error) {
			return outbox.WriteResult{}, sqlite.Error{Code: sqlite.ErrBusy}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setRetry(t, 5*time.Second, 20*time.Second, tc.backoff)
			rt, e, _ := stoppingRuntime(t, true)
			called := make(chan struct{})
			var once sync.Once
			rt.outboxWrite = func(ctx context.Context, envs []outbox.NewEnvelope) (outbox.WriteResult, error) {
				once.Do(func() { close(called) })
				return tc.write(ctx, envs)
			}
			rt.writes <- writeReq{env: retryEnv(t, "stop-1")}
			select {
			case <-called:
			case <-time.After(5 * time.Second):
				t.Fatal("the writer did not try the group")
			}
			const grace = 300 * time.Millisecond
			start := time.Now()
			rt.StopBy(start.Add(grace))
			if took := time.Since(start); took > grace+200*time.Millisecond {
				t.Fatalf("StopBy took %v with a %v deadline", took, grace)
			}
			if d := nonZeroDrops(e); !reflect.DeepEqual(d, map[string]uint64{"stopped": 1}) {
				t.Fatalf("dropped = %v, want stopped 1", d)
			}
			if e.Stats.HasFault("write_unavailable") {
				t.Fatal("write_unavailable raised by a stop")
			}
		})
	}
}

// TestRetryExhaustionKeepsBaselineProbeCooldown: the probe interval after a group that kept failing
// runs from where 0.1.4's write would have given up (its first timed-out attempt), not from the end of
// the retry window. A later group that arrives once 0.1.4's interval has passed is probed and stored,
// although it waited behind the failing group's retry.
func TestRetryExhaustionKeepsBaselineProbeCooldown(t *testing.T) {
	setRetry(t, time.Second, 2*time.Second, 10*time.Millisecond)
	pi := ProbeInterval
	ProbeInterval = 400 * time.Millisecond
	t.Cleanup(func() { ProbeInterval = pi })
	rt, e, ob := stoppingRuntime(t, true)
	bad := wire.ExchangeAttemptKey("cool-a", 1)
	rt.outboxWrite = func(ctx context.Context, envs []outbox.NewEnvelope) (outbox.WriteResult, error) {
		for _, en := range envs {
			if en.Key == bad {
				return blockWrite(ctx, envs) // a disk that cannot take this group
			}
		}
		return ob.Write(ctx, envs)
	}
	got := make(chan [2]any, 2)
	commit := func(key string) func(bool) { return func(ok bool) { got <- [2]any{key, ok} } }
	start := time.Now()
	rt.writes <- writeReq{env: retryEnv(t, "cool-a"), commit: commit("a")}
	// 0.1.4 gives up on a at about 1 s and probes again from 1.4 s: b arrives at 1.7 s, while a's
	// retry runs until about 2 s.
	time.Sleep(time.Until(start.Add(1700 * time.Millisecond)))
	rt.writes <- writeReq{env: retryEnv(t, "cool-b"), commit: commit("b")}
	want := map[string]bool{"a": false, "b": true}
	for range 2 {
		select {
		case c := <-got:
			if k := c[0].(string); c[1].(bool) != want[k] {
				t.Fatalf("group %s committed %v, want %v (dropped %v)", k, c[1], want[k], nonZeroDrops(e))
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the writer did not finish both groups")
		}
	}
	if d := nonZeroDrops(e); !reflect.DeepEqual(d, map[string]uint64{"disk_hard": 1}) {
		t.Fatalf("dropped = %v, want disk_hard 1 (the failing group only)", d)
	}
	if e.Stats.HasFault("write_unavailable") {
		t.Fatal("write_unavailable still raised after the probe and write succeeded")
	}
}
