package plugin

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/catalog"
)

const (
	seenFile = "catalog.json"
	// An unchanged list is still rewritten this often, so the time shown after a
	// restart is at most this stale. A changed list is written at once.
	seenRewriteInterval = time.Hour
	maxSeenFileBytes    = 4 << 20
)

// seenCatalog is CPA's catalog as Codex was last offered it, before switches.
// It holds only slugs, display names, and CPA's visibility: no instructions,
// keys, or request data.
type seenCatalog struct {
	SeenAt time.Time       `json:"seen_at"`
	Models []catalog.Entry `json:"models"`
}

// seenState remembers the latest catalog in memory and, best effort, on disk,
// so the settings page has a list after a restart and before Codex refetches.
type seenState struct {
	mu        sync.Mutex
	latest    *seenCatalog
	dir       string
	written   *seenCatalog // what the file holds, when this process wrote or read it
	saveFails bool
}

// current returns the latest catalog and whether it is saved to disk.
func (s *seenState) current() (*seenCatalog, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest, !s.saveFails
}

// load reads the saved list from dir the first time dir is configured.
func (s *seenState) load(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if dir == s.dir {
		return
	}
	s.dir, s.written, s.saveFails = dir, nil, false
	if dir == "" {
		return
	}
	if saved, err := readSeen(filepath.Join(dir, seenFile)); err == nil {
		s.written = saved
		if s.latest == nil || s.latest.SeenAt.Before(saved.SeenAt) {
			s.latest = saved
		}
	}
}

// remember records the catalog Codex was just offered and saves it when it
// differs from the file or the file is older than seenRewriteInterval.
func (s *seenState) remember(dir string, models []catalog.Entry, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := &seenCatalog{SeenAt: at.UTC(), Models: models}
	s.latest = next
	if dir == "" || dir != s.dir {
		return
	}
	if s.written != nil && !s.saveFails && slices.Equal(s.written.Models, models) && at.Sub(s.written.SeenAt) < seenRewriteInterval {
		return
	}
	if err := writeSeen(dir, next); err != nil {
		s.saveFails = true
		return
	}
	s.written, s.saveFails = next, false
}

func readSeen(path string) (*seenCatalog, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxSeenFileBytes+1))
	if err != nil || len(raw) > maxSeenFileBytes {
		return nil, errors.New("unreadable model list")
	}
	var saved seenCatalog
	if json.Unmarshal(raw, &saved) != nil || saved.SeenAt.IsZero() || len(saved.Models) == 0 {
		return nil, errors.New("invalid model list")
	}
	for _, entry := range saved.Models {
		if entry.Slug == "" {
			return nil, errors.New("invalid model list")
		}
	}
	return &saved, nil
}

// writeSeen replaces the file atomically: a crash leaves the old list or the
// new one, never a torn file.
func writeSeen(dir string, seen *seenCatalog) error {
	raw, err := json.MarshalIndent(seen, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, seenFile+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(append(raw, '\n')); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), filepath.Join(dir, seenFile))
}
