// Package store keeps the short sample history that trend needs.
//
// quota-cache holds the current observation, not a series, so the only way to
// say whether a row is rising or falling is to remember what it was. The ring
// is deliberately small: six hours is more than the one-hour comparison needs,
// and a dashboard that loses its arrows after a restart is a far smaller
// problem than one that grows a file without bound.
package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/aggregate"
)

const (
	// Retain bounds the ring in time; MaxSamples bounds it in count so a large
	// roster cannot grow the file without limit.
	Retain     = 6 * time.Hour
	MaxSamples = 20000
	maxBytes   = 8 << 20
	fileName   = "history.json"
)

type Store struct {
	mu      sync.Mutex
	path    string
	samples []aggregate.Sample
}

type document struct {
	Schema  int                `json:"schema"`
	Samples []aggregate.Sample `json:"samples"`
}

// Open loads any existing history from dir. A history that cannot be read is
// not fatal: the dashboard starts with unknown trends and recovers on its own
// as new samples arrive.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("quota-glance data directory is not configured")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.New("quota-glance data directory cannot be created")
	}
	s := &Store{path: filepath.Join(dir, fileName), samples: []aggregate.Sample{}}
	raw, err := os.ReadFile(s.path)
	if err != nil || len(raw) > maxBytes {
		return s, nil
	}
	var doc document
	if json.Unmarshal(raw, &doc) == nil && doc.Schema == 1 {
		s.samples = doc.Samples
	}
	return s, nil
}

// Samples returns a copy so a reader cannot be mutated by a concurrent append.
func (s *Store) Samples() []aggregate.Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]aggregate.Sample, len(s.samples))
	copy(out, s.samples)
	return out
}

// Append records one round of samples and prunes anything past the retention
// window. A persistence failure is returned but never stops the caller: losing
// history degrades the arrows, not the numbers.
func (s *Store) Append(samples []aggregate.Sample, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples = append(s.samples, samples...)
	cutoff := now.Add(-Retain)
	kept := s.samples[:0]
	for _, sample := range s.samples {
		if !sample.At.Before(cutoff) && !sample.At.After(now) {
			kept = append(kept, sample)
		}
	}
	s.samples = kept
	if len(s.samples) > MaxSamples {
		sort.SliceStable(s.samples, func(i, j int) bool { return s.samples[i].At.Before(s.samples[j].At) })
		s.samples = s.samples[len(s.samples)-MaxSamples:]
	}
	return s.persistLocked()
}

// persistLocked writes by atomic rename for the same reason quota-cache does:
// a reader must never observe a half-written file.
func (s *Store) persistLocked() error {
	raw, err := json.Marshal(document{Schema: 1, Samples: s.samples})
	if err != nil {
		return errors.New("history cannot be encoded")
	}
	file, err := os.CreateTemp(filepath.Dir(s.path), ".history-*")
	if err != nil {
		return errors.New("history cannot be written")
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(raw); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, s.path)
	}
	if err != nil {
		return errors.New("history cannot be committed")
	}
	return nil
}
