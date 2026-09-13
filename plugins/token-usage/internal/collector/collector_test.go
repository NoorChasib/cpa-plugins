package collector

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-token-usage/internal/config"
	"github.com/NoorChasib/cpa-plugin-token-usage/internal/store"
	"github.com/NoorChasib/cpa-plugin-token-usage/internal/usage"
	sqlite "github.com/mattn/go-sqlite3"
)

var clock = time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
var raw = []byte(`{"Provider":"p","Model":"m","RequestedAt":"2026-09-09T12:00:00Z","Detail":{"InputTokens":5,"OutputTokens":3}}`)

type fakeStore struct {
	write    func(context.Context, []usage.Event, time.Time) (store.BatchResult, error)
	save     func(context.Context, map[string]string, string, time.Time, bool) error
	query    func(context.Context, store.Filter, time.Time) (store.QueryResult, error)
	probe    func(context.Context, string, time.Time) error
	maintain func(context.Context, time.Time) (bool, error)
	disk     func() (int64, error)
	size     atomic.Int64
	closed   atomic.Bool
	mu       sync.Mutex
	ids      map[string]struct{}
	calls    []string
	saved    map[string]string
	finished bool
}

func (f *fakeStore) WriteBatch(ctx context.Context, events []usage.Event, now time.Time) (store.BatchResult, error) {
	if f.write != nil {
		return f.write(ctx, events, now)
	}
	return f.insert(events), nil
}
func (f *fakeStore) insert(events []usage.Event) store.BatchResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ids == nil {
		f.ids = map[string]struct{}{}
	}
	n := 0
	for _, e := range events {
		f.calls = append(f.calls, e.ID)
		if _, exists := f.ids[e.ID]; !exists {
			n++
			f.ids[e.ID] = struct{}{}
		}
	}
	return store.BatchResult{Inserted: n, Committed: fmt.Sprint(len(f.ids)), CoverageStart: clock.UnixNano()}
}
func (f *fakeStore) SaveDiagnostics(ctx context.Context, v map[string]string, run string, at time.Time, finish bool) error {
	if f.save != nil {
		return f.save(ctx, v, run, at, finish)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = v
	f.finished = finish
	return nil
}
func (f *fakeStore) ProbeWrite(ctx context.Context, run string, at time.Time) error {
	if f.probe != nil {
		return f.probe(ctx, run, at)
	}
	return nil
}
func (f *fakeStore) Maintain(ctx context.Context, at time.Time) (bool, error) {
	if f.maintain != nil {
		return f.maintain(ctx, at)
	}
	return false, nil
}
func (f *fakeStore) DiskBytes() (int64, error) {
	if f.disk != nil {
		return f.disk()
	}
	return f.size.Load(), nil
}
func (f *fakeStore) Query(ctx context.Context, q store.Filter, at time.Time) (store.QueryResult, error) {
	if f.query != nil {
		return f.query(ctx, q, at)
	}
	return store.QueryResult{}, nil
}
func (f *fakeStore) Close() error { f.closed.Store(true); return nil }
func testConfig() config.Config {
	return config.Config{QueueCapacity: 1, BatchSize: 1, FlushInterval: 10 * time.Millisecond, RawRetention: time.Hour, MaintenanceInterval: time.Second, MaxDiskBytes: 1 << 20, MaxModels: 10, QueryTimeout: time.Second}
}
func fresh(f *fakeStore) *Collector {
	return start(testConfig(), f, store.Initial{Committed: "0", CoverageStart: clock.UnixNano(), RetentionFloor: clock.Add(-time.Hour).UnixNano()}, "test-run", func() time.Time { return clock })
}
func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	until := time.Now().Add(2 * time.Second)
	for time.Now().Before(until) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition timed out")
}
func TestBoundedAdmissionNeverWaitsForWriter(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	f := &fakeStore{}
	f.write = func(ctx context.Context, events []usage.Event, at time.Time) (store.BatchResult, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return f.insert(events), nil
	}
	c := fresh(f)
	defer func() { once.Do(func() { close(release) }); c.Stop() }()
	if _, err := c.Observe(raw); err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err := c.Observe(raw); err != nil {
		t.Fatal(err)
	}
	returned := make(chan error, 1)
	go func() { _, err := c.Observe(raw); returned <- err }()
	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("full queue accepted event")
		}
	case <-time.After(time.Second):
		t.Fatal("callback blocked on writer")
	}
	if c.counts.droppedQueue.Load() != 1 {
		t.Fatal("queue drop invisible")
	}
	stopped := make(chan struct{})
	go func() { c.Stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("stop returned before writer joined")
	case <-time.After(20 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	<-stopped
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ids) != 2 || !f.finished || f.saved["dropped_queue"] != "1" {
		t.Fatalf("drain or diagnostics %#v %#v", f.ids, f.saved)
	}
}
func TestStableIDsAcrossRetryAndIdempotentAmbiguousCommit(t *testing.T) {
	f := &fakeStore{}
	var attempts atomic.Int32
	f.write = func(ctx context.Context, events []usage.Event, at time.Time) (store.BatchResult, error) {
		r := f.insert(events)
		if attempts.Add(1) == 1 {
			return r, sqlite.Error{Code: sqlite.ErrBusy}
		}
		return r, nil
	}
	c := fresh(f)
	defer c.Stop()
	if _, err := c.Observe(raw); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return c.committed.Load().(string) == "1" })
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 2 || f.calls[0] != f.calls[1] || len(f.ids) != 1 {
		t.Fatalf("retry changed identity: %v", f.calls)
	}
	if c.counts.retries.Load() != 1 || c.counts.droppedStorage.Load() != 0 {
		t.Fatal("retry diagnostics")
	}
}
func TestStorageFailureAndDiskBudgetAreVisible(t *testing.T) {
	f := &fakeStore{write: func(context.Context, []usage.Event, time.Time) (store.BatchResult, error) {
		return store.BatchResult{}, sqlite.Error{Code: sqlite.ErrFull}
	}, save: func(context.Context, map[string]string, string, time.Time, bool) error {
		return sqlite.Error{Code: sqlite.ErrFull}
	}}
	c := fresh(f)
	defer c.Stop()
	c.Observe(raw)
	eventually(t, func() bool { return c.faults.Load() != 0 })
	if _, err := c.Observe(raw); !errors.Is(err, store.ErrUnavailable) {
		t.Fatal("storage-fault admission not stopped")
	}
	if c.counts.droppedStorage.Load() < 2 || c.Snapshot()["degraded"] != true {
		t.Fatalf("fault metrics %v", c.Snapshot())
	}
	budget := &fakeStore{}
	budget.size.Store(2 << 20)
	b := fresh(budget)
	defer b.Stop()
	if _, err := b.Observe(raw); err == nil {
		t.Fatal("over-budget accepted")
	}
	if b.Snapshot()["reason"] != "disk_budget" {
		t.Fatal("budget reason missing")
	}
	budget.size.Store(0)
	eventually(t, func() bool { return b.faults.Load() == 0 })
	if _, err := b.Observe(raw); err != nil {
		t.Fatal("recovery did not reopen admission")
	}
}
func TestQueryCannotStallCallbackAndCloseWaitsForReaders(t *testing.T) {
	entered := make(chan struct{})
	f := &fakeStore{query: func(ctx context.Context, _ store.Filter, _ time.Time) (store.QueryResult, error) {
		close(entered)
		<-ctx.Done()
		return store.QueryResult{}, ctx.Err()
	}}
	c := fresh(f)
	defer c.Stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queryDone := make(chan struct{})
	go func() { c.Query(ctx, store.Filter{}); close(queryDone) }()
	<-entered
	result := make(chan error, 1)
	go func() { _, err := c.Observe(raw); result <- err }()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("query blocked callback")
	}
	stopDone := make(chan struct{})
	go func() { c.Stop(); close(stopDone) }()
	time.Sleep(20 * time.Millisecond)
	if f.closed.Load() {
		t.Fatal("database closed while query active")
	}
	cancel()
	<-queryDone
	<-stopDone
	if !f.closed.Load() {
		t.Fatal("store not closed")
	}
}
func TestBatchReferencesClearedBeforeReuse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		events   int
		interval time.Duration
	}{{"full batch", 2, 10 * time.Second}, {"timer", 1, 10 * time.Millisecond}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.QueueCapacity, cfg.BatchSize, cfg.FlushInterval = 2, 2, tc.interval
			var held []usage.Event
			flushed := make(chan []usage.Event, 1)
			f := &fakeStore{}
			f.write = func(_ context.Context, events []usage.Event, _ time.Time) (store.BatchResult, error) {
				held = events // Retain the backing array solely to inspect it after flush.
				return f.insert(events), nil
			}
			f.save = func(context.Context, map[string]string, string, time.Time, bool) error {
				if held != nil {
					flushed <- held
					held = nil
				}
				return nil
			}
			c := start(cfg, f, store.Initial{Committed: "0"}, "test-run", func() time.Time { return clock })
			defer c.Stop()
			for range tc.events {
				if _, err := c.Observe(raw); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case events := <-flushed:
				if len(events) != tc.events {
					t.Fatalf("flushed %d events, want %d", len(events), tc.events)
				}
				for _, e := range events {
					if e != (usage.Event{}) {
						t.Fatal("idle batch retains event data")
					}
				}
			case <-time.After(2 * time.Second):
				t.Fatal("batch did not flush")
			}
		})
	}
}

func TestRejectionAndDiagnosticSaturation(t *testing.T) {
	f := &fakeStore{}
	c := fresh(f)
	c.RejectOversized()
	if _, err := c.Observe([]byte(`{`)); err == nil {
		t.Fatal("malformed accepted")
	}
	stale := []byte(`{"Provider":"p","Model":"m","RequestedAt":"2026-09-09T10:00:00Z"}`)
	if _, err := c.Observe(stale); err == nil {
		t.Fatal("stale accepted")
	}
	c.Stop()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saved["rejected_events"] != "3" || f.saved["rejected_retention"] != "1" {
		t.Fatalf("saved diagnostics %v", f.saved)
	}
	var n atomic.Uint64
	n.Store(^uint64(0))
	increment(&n, 1)
	if n.Load() != ^uint64(0) {
		t.Fatal("diagnostic overflow wrapped")
	}
}
