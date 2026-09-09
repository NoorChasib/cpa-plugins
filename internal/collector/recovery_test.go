package collector

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-token-usage/internal/store"
	"github.com/NoorChasib/cpa-plugin-token-usage/internal/usage"
)

// Deliberately no worker: operation/fault interleavings and wall-clock throttles
// can be driven deterministically without sleeping through recovery intervals.
func manualCollector(f backend) *Collector {
	c := &Collector{cfg: testConfig(), db: f, now: func() time.Time { return clock }, run: "manual", queue: make(chan usage.Event, 64)}
	c.lastFailure.Store("")
	c.committed.Store("0")
	c.lastPersisted.Store("")
	c.coverageStart.Store(clock.UnixNano())
	c.retentionFloor.Store(clock.Add(-time.Hour).UnixNano())
	return c
}

func TestPermanentWriteFaultCannotBeClearedByHealthyDiagnostics(t *testing.T) {
	var writes, probes int
	broken := true
	f := &fakeStore{}
	f.write = func(_ context.Context, events []usage.Event, _ time.Time) (store.BatchResult, error) {
		writes++
		if broken {
			return store.BatchResult{}, store.ErrUnavailable
		}
		return f.insert(events), nil
	}
	f.probe = func(context.Context, string, time.Time) error {
		probes++
		if broken {
			return store.ErrUnavailable
		}
		return nil
	}
	c := manualCollector(f)
	e, err := c.Observe(raw)
	if err != nil {
		t.Fatal(err)
	}
	<-c.queue
	c.flush([]usage.Event{e})
	for i := range 20 {
		c.checkDisk()
		c.save(false)
		c.recoverWrite(time.Now()) // Still throttled, never trial-admit real events.
		if _, err := c.Observe(raw); !errors.Is(err, store.ErrUnavailable) {
			t.Fatalf("iteration %d reopened broken writer", i)
		}
		c.flush([]usage.Event{{ID: fmt.Sprint(i)}}) // An already-admitted queue tail.
	}
	if writes != 1 || probes != 0 || c.Snapshot()["reason"] != "write_unavailable" {
		t.Fatalf("fault masked / queued events retried: writes=%d probes=%d snapshot=%v", writes, probes, c.Snapshot())
	}
	for range 3 {
		c.recoverWrite(c.nextWriteProbe)
		if c.faults.Load()&faultWrite == 0 {
			t.Fatal("failed recovery probe cleared write fault")
		}
	}
	if writes != 1 || probes != 3 {
		t.Fatal("recovery used real observations")
	}
	broken = false
	c.recoverWrite(c.nextWriteProbe)
	if c.faults.Load() != 0 || c.Snapshot()["degraded"] != true || c.Snapshot()["last_failure_reason"] != "write_unavailable" {
		t.Fatalf("recovery lost history: %v", c.Snapshot())
	}
	e, err = c.Observe(raw)
	if err != nil {
		t.Fatalf("successful matching probe did not recover: %v", err)
	}
	<-c.queue
	c.flush([]usage.Event{e})
	if c.committed.Load() != "1" || writes != 2 {
		t.Fatal("recovered writer did not commit")
	}
}

func TestWorkerMaintenanceFaultSurvivesManySuccessfulSaves(t *testing.T) {
	var attempts, saves atomic.Int32
	var broken atomic.Bool
	broken.Store(true)
	f := &fakeStore{
		maintain: func(ctx context.Context, _ time.Time) (bool, error) {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > cleanupTimeout {
				t.Error("maintenance lacks its bounded deadline")
			}
			attempts.Add(1)
			if broken.Load() {
				return false, store.ErrUnavailable
			}
			return false, nil
		},
		save: func(context.Context, map[string]string, string, time.Time, bool) error {
			saves.Add(1)
			return nil
		},
	}
	cfg := testConfig()
	cfg.MaintenanceInterval = 10 * time.Millisecond
	c := start(cfg, f, store.Initial{Committed: "0"}, "test", func() time.Time { return clock })
	defer c.Stop()
	eventually(t, func() bool { return attempts.Load() >= 12 && saves.Load() >= 12 })
	if s := c.Snapshot(); s["state"] != "degraded" || s["degraded"] != true || s["reason"] != "maintenance_unavailable" {
		t.Fatalf("healthy stat/save masked repeated maintenance failures: %v", s)
	}
	if _, err := c.Observe(raw); !errors.Is(err, store.ErrUnavailable) {
		t.Fatal("active maintenance fault reopened admission")
	}
	broken.Store(false)
	eventually(t, func() bool { return c.faults.Load() == 0 })
	if c.Snapshot()["degraded"] != true || c.counts.storageErrors.Load() < 12 {
		t.Fatal("recovery erased lifetime storage-error degradation")
	}
}

func TestFaultCausesRecoverIndependentlyAndIdentityIsTerminal(t *testing.T) {
	f := &fakeStore{}
	c := manualCollector(f)
	all := faultDiskBudget | faultWrite | faultDiagnostics | faultMaintenance | faultIdentity
	c.updateFault(all, all)
	f.size.Store(c.cfg.MaxDiskBytes + 1)
	c.checkDisk()
	if got := c.Snapshot()["active_faults"]; !reflect.DeepEqual(got, []string{"event_id_exhausted", "disk_budget", "write_unavailable", "diagnostics_unavailable", "maintenance_unavailable"}) {
		t.Fatalf("simultaneous causes missing: %v", got)
	}
	c.save(false)
	if c.faults.Load() != all&^faultDiagnostics {
		t.Fatal("diagnostic recovery cleared another cause")
	}
	c.maintain()
	if c.faults.Load() != faultDiskBudget|faultWrite|faultIdentity {
		t.Fatal("maintenance recovery cleared another cause")
	}
	c.recoverWrite(time.Now()) // Budget suppresses the write probe.
	if c.faults.Load()&faultWrite == 0 {
		t.Fatal("budget did not suppress writer recovery")
	}
	f.size.Store(0)
	c.checkDisk()
	c.recoverWrite(time.Now())
	for range 20 {
		c.checkDisk()
		c.save(false)
		c.maintain()
	}
	if c.faults.Load() != faultIdentity {
		t.Fatal("terminal identity fault was cleared")
	}
	if _, err := c.Observe(raw); !errors.Is(err, store.ErrUnavailable) {
		t.Fatal("terminal fault admitted event")
	}

	// Disk-stat errors and budget are the same operation, not separate stale
	// causes; disk recovery still must not erase write/maintenance faults.
	f.disk = func() (int64, error) { return 0, store.ErrUnavailable }
	c.checkDisk()
	if c.faults.Load() != faultIdentity|faultDiskUnavailable {
		t.Fatal("disk error not tracked independently")
	}
	f.disk = nil
	c.checkDisk()
	if c.faults.Load() != faultIdentity || c.counts.storageErrors.Load() == 0 {
		t.Fatal("disk-stat recovery/history incorrect")
	}
}

func TestConcurrentFaultUpdatesAndIdentityExhaustion(t *testing.T) {
	c := manualCollector(&fakeStore{})
	c.sequence.Store(^uint64(0) - 1)
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Observe(raw)
		}()
	}
	wg.Wait()
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				c.updateFault(faultDiagnostics, faultDiagnostics)
				c.updateFault(faultDiagnostics, 0)
				c.updateFault(faultMaintenance, faultMaintenance)
				c.updateFault(faultDisk, 0)
				c.Snapshot()
			}
		}()
	}
	wg.Wait()
	if c.sequence.Load() != ^uint64(0) || len(c.queue) != 1 || c.faults.Load() != faultIdentity|faultMaintenance {
		t.Fatalf("identity wrapped / simultaneous causes lost: sequence=%d queued=%d faults=%d", c.sequence.Load(), len(c.queue), c.faults.Load())
	}
}

func TestReadableHistoryRemainsAvailableUnderAdmissionFaults(t *testing.T) {
	for _, fault := range []uint32{faultDiskBudget, faultMaintenance, faultWrite, faultDiagnostics} {
		t.Run(fmt.Sprint(fault), func(t *testing.T) {
			want := store.QueryResult{Totals: store.Totals{Observed: "7", Tokens: map[string]string{"input_tokens": "9007199254740993"}}}
			f := &fakeStore{query: func(context.Context, store.Filter, time.Time) (store.QueryResult, error) { return want, nil }}
			c := manualCollector(f)
			c.updateFault(fault, fault)
			got, err := c.Query(context.Background(), store.Filter{})
			if err != nil || !reflect.DeepEqual(got, want) || c.Snapshot()["degraded"] != true {
				t.Fatalf("readable committed history hidden: %+v %v", got, err)
			}
			f.query = func(context.Context, store.Filter, time.Time) (store.QueryResult, error) {
				return store.QueryResult{}, store.ErrUnavailable
			}
			if _, err := c.Query(context.Background(), store.Filter{}); !errors.Is(err, store.ErrUnavailable) {
				t.Fatal("actual read outage became fake zeros")
			}
		})
	}
}

func TestStorageErrorHistoryAloneRemainsDegraded(t *testing.T) {
	c := manualCollector(&fakeStore{})
	c.counts.storageErrors.Store(1)
	if s := c.Snapshot(); s["state"] != "degraded" || s["degraded"] != true || s["reason"] != "" {
		t.Fatalf("storage-error history omitted: %v", s)
	}
}
