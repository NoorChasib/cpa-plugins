package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/aggregate"
)

func sample(authIndex string, at time.Time, remaining float64) aggregate.Sample {
	return aggregate.Sample{AuthIndex: authIndex, WindowKey: "session", At: at, Remaining: remaining}
}

func TestHistorySurvivesRestartAndPrunesPastRetention(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1789012800, 0).UTC()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append([]aggregate.Sample{
		sample("a", now.Add(-7*time.Hour), 0.9), // past retention
		sample("a", now.Add(-2*time.Hour), 0.8),
		sample("a", now.Add(-time.Hour), 0.7),
		sample("a", now.Add(time.Hour), 0.6), // in the future
	}, now); err != nil {
		t.Fatal(err)
	}
	if got := len(s.Samples()); got != 2 {
		t.Fatalf("retained %d samples; want the 2 inside the window", got)
	}

	// A restart must not lose the arrows.
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(reopened.Samples()); got != 2 {
		t.Fatalf("reopened with %d samples; want 2", got)
	}

	// Retention is enforced on every append, not only at write time.
	if err := reopened.Append(nil, now.Add(6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := len(reopened.Samples()); got != 0 {
		t.Fatalf("samples = %d; everything should have aged out", got)
	}
}

func TestSamplesAreCopiedNotShared(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1789012800, 0).UTC()
	s, _ := Open(dir)
	if err := s.Append([]aggregate.Sample{sample("a", now, 0.5)}, now); err != nil {
		t.Fatal(err)
	}
	got := s.Samples()
	got[0].Remaining = 0.1
	if s.Samples()[0].Remaining != 0.5 {
		t.Fatal("a caller mutated the store's history")
	}
}

func TestCountIsBounded(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1789012800, 0).UTC()
	s, _ := Open(dir)
	batch := make([]aggregate.Sample, 0, MaxSamples+500)
	for i := 0; i < MaxSamples+500; i++ {
		batch = append(batch, sample("a", now.Add(-time.Duration(i)*time.Millisecond), 0.5))
	}
	if err := s.Append(batch, now); err != nil {
		t.Fatal(err)
	}
	if got := len(s.Samples()); got != MaxSamples {
		t.Fatalf("samples = %d; want the ring capped at %d", got, MaxSamples)
	}
}

// A corrupt history costs the arrows, not the dashboard.
func TestUnreadableHistoryStartsEmptyRatherThanFailing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("a corrupt history must not stop startup: %v", err)
	}
	if len(s.Samples()) != 0 {
		t.Fatal("samples decoded from a corrupt file")
	}
}

func TestOpenRequiresADirectory(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("an empty data directory was accepted")
	}
}

// The writer must never persist more than Open will read back: a file that
// always reloads as zero samples is worse than a shorter history.
func TestPersistedHistoryNeverExceedsTheReadCap(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1789012800, 0).UTC()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A long upstream window key, which a raw: passthrough can produce.
	key := strings.Repeat("k", 1000)
	batch := make([]aggregate.Sample, 0, MaxSamples)
	for i := 0; i < MaxSamples; i++ {
		batch = append(batch, aggregate.Sample{
			AuthIndex: "claude-a@example.com.json", WindowKey: key,
			At: now.Add(-time.Duration(i) * time.Millisecond), Remaining: 0.5,
		})
	}
	if err := s.Append(batch, now); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > maxBytes {
		t.Fatalf("persisted %d bytes against a %d-byte read cap", info.Size(), maxBytes)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.Samples()) == 0 {
		t.Fatal("history reloaded as empty; the write exceeded what Open accepts")
	}
}
