package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/token-usage/internal/usage"
)

func TestMaintenanceContinuationPreservesFreshHistoryAndDurableFloor(t *testing.T) {
	s, cfg := openTest(t)
	const expired = 1800
	batch := make([]usage.Event, expired)
	for i := range batch {
		batch[i] = event(fmt.Sprint(i), "old", fmt.Sprint(i), now)
	}
	write(t, s, batch...)
	later := now.Add(2 * time.Hour)
	write(t, s, event("fresh", "fresh", "model", later))
	for step := 1; step <= 4; step++ {
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		more, err := s.Maintain(ctx, later)
		cancel()
		if err != nil || more != (step < 4) {
			t.Fatalf("step %d: more=%v err=%v", step, more, err)
		}
		var rows, models int
		if err := s.db.QueryRow("SELECT count(*) FROM usage_events").Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow("SELECT count(*) FROM model_keys").Scan(&models); err != nil {
			t.Fatal(err)
		}
		want := max(0, expired-512*step) + 1
		if rows != want || models != want {
			t.Fatalf("step %d exceeded chunk cap or lost fresh history: rows=%d models=%d want=%d", step, rows, models, want)
		}
		q, err := s.Query(context.Background(), filter(later, later.Add(time.Second)), later.Add(time.Second))
		if err != nil || q.Totals.Observed != "1" || q.Totals.Tokens["input_tokens"] != "9223372036854775807" {
			t.Fatalf("fresh query between chunks %+v %v", q, err)
		}
	}
	floor, err := integer(context.Background(), s.db, "retention_floor")
	if err != nil || floor != later.Add(-cfg.RawRetention).UnixNano() {
		t.Fatalf("durable floor %d %v", floor, err)
	}
	if _, err := s.Maintain(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	backward, err := integer(context.Background(), s.db, "retention_floor")
	if err != nil || backward != floor {
		t.Fatal("clock reversal moved floor backwards")
	}
	s.Close()
	cfg.RawRetention = 24 * time.Hour
	reopened, initial, err := Open(cfg, later)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if initial.RetentionFloor != floor || initial.Committed != "1801" {
		t.Fatalf("cleanup changed lifetime commits or resurrected coverage: %+v", initial)
	}
}

func TestMaintenanceCanceledChunkDoesNotAdvanceFloor(t *testing.T) {
	s, _ := openTest(t)
	write(t, s, event("kept", "p", "m", now))
	before, err := integer(context.Background(), s.db, "retention_floor")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Maintain(ctx, now.Add(2*time.Hour)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleanup: %v", err)
	}
	after, err := integer(context.Background(), s.db, "retention_floor")
	if err != nil || before != after {
		t.Fatal("canceled chunk advanced floor")
	}
}

func TestWriteProbeExercisesEventInsertWithoutRetainingObservations(t *testing.T) {
	s, _ := openTest(t)
	write(t, s, event("real", "p", "m", now))
	before, err := s.loadInitial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := s.ProbeWrite(context.Background(), "recovery-run", now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	after, err := s.loadInitial(context.Background())
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("probe changed persistent metadata: before=%+v after=%+v err=%v", before, after, err)
	}
	var rows, models int
	if err := s.db.QueryRow("SELECT count(*) FROM usage_events").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM model_keys").Scan(&models); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || models != 1 {
		t.Fatal("probe retained a synthetic row or model key")
	}
	// A healthy metadata save cannot establish event-insert health.
	if _, err := s.db.Exec("CREATE TRIGGER reject_events BEFORE INSERT ON usage_events BEGIN SELECT RAISE(ABORT,'injected insert failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDiagnostics(context.Background(), map[string]string{}, "unused", now, false); err != nil {
		t.Fatal(err)
	}
	if err := s.ProbeWrite(context.Background(), "recovery-run", now); err == nil {
		t.Fatal("probe missed event-specific insertion failure")
	}
	if _, err := s.db.Exec("DROP TRIGGER reject_events"); err != nil {
		t.Fatal(err)
	}
	if err := s.ProbeWrite(context.Background(), "recovery-run", now); err != nil {
		t.Fatal("probe did not recover after event insert repaired")
	}
}
