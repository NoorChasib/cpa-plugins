package collector

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/token-usage/internal/store"
)

type maintenanceStep struct {
	more bool
	err  error
}
type observedMaintenance struct {
	*store.Store
	steps chan maintenanceStep
}

func (s *observedMaintenance) Maintain(ctx context.Context, at time.Time) (bool, error) {
	more, err := s.Store.Maintain(ctx, at)
	s.steps <- maintenanceStep{more, err}
	return more, err
}

func TestWorkerContinuesSustainedExpiryWithoutWaitingFullIntervals(t *testing.T) {
	cfg := testConfig()
	cfg.DatabasePath = filepath.Join(t.TempDir(), "private", "usage.sqlite")
	cfg.QueueCapacity, cfg.BatchSize = 2048, 256
	cfg.MaxDiskBytes = 1 << 30
	cfg.MaintenanceInterval = time.Second
	db, initial, err := store.Open(cfg, clock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.StartRun(context.Background(), "cleanup", clock); err != nil {
		db.Close()
		t.Fatal(err)
	}
	watched := &observedMaintenance{Store: db, steps: make(chan maintenanceStep, 64)}
	var at atomic.Int64
	at.Store(clock.UnixNano())
	c := start(cfg, watched, initial, "cleanup", func() time.Time { return time.Unix(0, at.Load()) })
	defer c.Stop()
	observe := func() {
		t.Helper()
		wire := []byte(fmt.Sprintf(`{"Provider":"p","Model":"m","RequestedAt":%q,"Detail":{"InputTokens":5}}`, time.Unix(0, at.Load()).UTC().Format(time.RFC3339Nano)))
		if _, err := c.Observe(wire); err != nil {
			t.Fatal(err)
		}
	}
	var total int
	for round := range 3 {
		// 900 newly expiring observations each scheduled interval, beyond the
		// former 512/interval ceiling, plus the preceding round's fresh event.
		for range 900 {
			observe()
		}
		total += 900
		eventually(t, func() bool { return c.committed.Load() == fmt.Sprint(total) })
		at.Add(int64(2 * time.Hour))
		fresh := time.Unix(0, at.Load())
		observe()
		total++
		at.Add(int64(time.Second))
		eventually(t, func() bool { return c.committed.Load() == fmt.Sprint(total) })

		deadline := time.After(2 * time.Second)
	firstChunk:
		for {
			select {
			case step := <-watched.steps:
				if step.err != nil {
					t.Fatal(step.err)
				}
				if step.more {
					break firstChunk
				}
			case <-deadline:
				t.Fatal("scheduled cleanup did not start")
			}
		}
		// A separate read can run between chunks while fresh history remains.
		q, err := c.Query(context.Background(), store.Filter{From: fresh, To: fresh.Add(time.Second)})
		if err != nil || q.Totals.Observed != "1" {
			t.Fatalf("round %d fresh read %+v %v", round, q, err)
		}
		select {
		case step := <-watched.steps:
			if step.err != nil || step.more {
				t.Fatalf("round %d second chunk did not drain backlog: %+v", round, step)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatal("backlog continuation waited for the full one-second interval")
		}
		eventually(t, func() bool { return !c.cleanupPending.Load() })
		if c.Snapshot()["degraded"] != false {
			t.Fatalf("healthy cleanup degraded admission: %v", c.Snapshot())
		}
	}
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	reopened, final, err := store.Open(cfg, time.Unix(0, at.Load()))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if final.Committed != fmt.Sprint(total) || final.RetentionFloor != at.Load()-int64(cfg.RawRetention) {
		t.Fatalf("durable floor/count lost across continuation and shutdown: %+v", final)
	}
}
