// Package statefile persists learned state to <state-dir>/state.json so
// pending evidence, promotion history, and counters survive container
// restarts. The state dir is an ordinary directory (not a bind-mounted single
// file), so atomic temp+rename is safe here; config.yaml is a different story
// and is handled by internal/configfile.
package statefile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/fingerprint"
	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/learner"
)

// Limits on stored history and file size.
const (
	FileName        = "state.json"
	SchemaVersion   = 1
	MaxHistory      = 20
	maxStateBytes   = 4 << 20
	stateFileMode   = 0o600
	stateDirMode    = 0o700
	tempFilePattern = ".state-*.json.tmp"
)

// Baseline is the effective on-disk baseline last observed for a provider.
type Baseline struct {
	Version        fingerprint.Version `json:"version"`
	UserAgent      string              `json:"user_agent"`
	PackageVersion string              `json:"package_version,omitempty"`
	RuntimeVersion string              `json:"runtime_version,omitempty"`
	// Explicit reports whether config.yaml carried an explicit value or the
	// compiled default was assumed.
	Explicit   bool      `json:"explicit"`
	ObservedAt time.Time `json:"observed_at"`
}

// Promotion is one baseline write (or dry-run computation).
type Promotion struct {
	At        time.Time             `json:"at"`
	Provider  fingerprint.Provider  `json:"provider"`
	From      fingerprint.Version   `json:"from"`
	Candidate fingerprint.Candidate `json:"candidate"`
	DryRun    bool                  `json:"dry_run"`
	Forced    bool                  `json:"forced,omitempty"`
	// Source is "observed" or "management".
	Source string `json:"source"`
	// Observations / DistinctSessions are the evidence at promotion time.
	Observations     int `json:"observations"`
	DistinctSessions int `json:"distinct_sessions"`
	// AwaitingReload is set after a real write until CPA's hot reload is
	// observed (a plugin.reconfigure arrives, or a fresh read of config.yaml
	// shows the value). ConfirmedAt records when that happened.
	AwaitingReload bool      `json:"awaiting_reload,omitempty"`
	ConfirmedAt    time.Time `json:"confirmed_at,omitempty"`
}

// Counters are monotonically increasing diagnostics.
type Counters struct {
	Requests      uint64            `json:"requests"`
	Ignored       uint64            `json:"ignored"`
	Accepted      uint64            `json:"accepted"`
	Rejected      uint64            `json:"rejected"`
	RejectReasons map[string]uint64 `json:"reject_reasons,omitempty"`
	Decisions     map[string]uint64 `json:"decisions,omitempty"`
}

// State is the persisted document.
type State struct {
	SchemaVersion int                                 `json:"schema_version"`
	SavedAt       time.Time                           `json:"saved_at"`
	Baselines     map[fingerprint.Provider]Baseline   `json:"baselines"`
	Pending       []learner.Pending                   `json:"pending"`
	LastPromotion map[fingerprint.Provider]*Promotion `json:"last_promotion,omitempty"`
	History       []Promotion                         `json:"history"`
	Counters      Counters                            `json:"counters"`
	LastError     string                              `json:"last_error,omitempty"`
	LastErrorAt   time.Time                           `json:"last_error_at,omitempty"`
	LastWriteAt   map[fingerprint.Provider]time.Time  `json:"last_write_at,omitempty"`
}

// New returns an empty state.
func New() *State {
	return &State{
		SchemaVersion: SchemaVersion,
		Baselines:     make(map[fingerprint.Provider]Baseline),
		LastPromotion: make(map[fingerprint.Provider]*Promotion),
		LastWriteAt:   make(map[fingerprint.Provider]time.Time),
		Counters:      Counters{RejectReasons: make(map[string]uint64), Decisions: make(map[string]uint64)},
	}
}

// normalize fills nil maps after decoding.
func (s *State) normalize() {
	if s.Baselines == nil {
		s.Baselines = make(map[fingerprint.Provider]Baseline)
	}
	if s.LastPromotion == nil {
		s.LastPromotion = make(map[fingerprint.Provider]*Promotion)
	}
	if s.LastWriteAt == nil {
		s.LastWriteAt = make(map[fingerprint.Provider]time.Time)
	}
	if s.Counters.RejectReasons == nil {
		s.Counters.RejectReasons = make(map[string]uint64)
	}
	if s.Counters.Decisions == nil {
		s.Counters.Decisions = make(map[string]uint64)
	}
	if len(s.History) > MaxHistory {
		s.History = s.History[len(s.History)-MaxHistory:]
	}
}

// RecordPromotion appends to bounded history and updates last-promotion.
func (s *State) RecordPromotion(p Promotion) {
	s.normalize()
	cp := p
	s.LastPromotion[p.Provider] = &cp
	s.History = append(s.History, p)
	if len(s.History) > MaxHistory {
		s.History = s.History[len(s.History)-MaxHistory:]
	}
	if !p.DryRun {
		s.LastWriteAt[p.Provider] = p.At
	}
}

// Path returns the state file path inside dir.
func Path(dir string) string { return filepath.Join(dir, FileName) }

// Load reads state from dir. A missing file yields a fresh state and no
// error. A corrupt file yields a fresh state and a non-nil error so the
// caller can log a warning and continue.
func Load(dir string) (*State, error) {
	path := Path(dir)
	// Refuse an oversized file BEFORE reading it so a corrupt or hostile
	// state file cannot make the plugin allocate its whole size.
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return New(), nil
		}
		return New(), fmt.Errorf("stat state file: %w", err)
	}
	if info.Size() > maxStateBytes {
		return New(), fmt.Errorf("state file is %d bytes, exceeding the %d byte limit; starting fresh", info.Size(), maxStateBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return New(), nil
		}
		return New(), fmt.Errorf("read state file: %w", err)
	}
	// Belt and braces: the file may have grown between stat and read.
	if len(raw) > maxStateBytes {
		return New(), fmt.Errorf("state file exceeds %d bytes; starting fresh", maxStateBytes)
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return New(), fmt.Errorf("decode state file: %w", err)
	}
	if st.SchemaVersion != SchemaVersion {
		return New(), fmt.Errorf("state file schema %d is not %d; starting fresh", st.SchemaVersion, SchemaVersion)
	}
	st.normalize()
	return &st, nil
}

// Save writes state atomically (temp file + rename inside dir), creating dir
// if needed. The file is owner-readable only: it carries no secrets, but it
// does record session IDs.
func Save(dir string, st *State, now time.Time) error {
	if st == nil {
		return errors.New("nil state")
	}
	st.normalize()
	st.SchemaVersion = SchemaVersion
	st.SavedAt = now
	sort.Slice(st.History, func(i, j int) bool { return st.History[i].At.Before(st.History[j].At) })

	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	if err := os.MkdirAll(dir, stateDirMode); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, tempFilePattern)
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(stateFileMode); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp state file: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("sync temp state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp state file: %w", err)
	}
	if err := os.Rename(tmpName, Path(dir)); err != nil {
		cleanup()
		return fmt.Errorf("replace state file: %w", err)
	}
	return nil
}

// Clone returns a deep copy suitable for serializing outside the engine
// lock while the original keeps being mutated.
func (s *State) Clone() *State {
	if s == nil {
		return New()
	}
	s.normalize()
	out := &State{
		SchemaVersion: s.SchemaVersion,
		SavedAt:       s.SavedAt,
		Baselines:     make(map[fingerprint.Provider]Baseline, len(s.Baselines)),
		Pending:       make([]learner.Pending, 0, len(s.Pending)),
		LastPromotion: make(map[fingerprint.Provider]*Promotion, len(s.LastPromotion)),
		History:       append([]Promotion(nil), s.History...),
		Counters: Counters{
			Requests:      s.Counters.Requests,
			Ignored:       s.Counters.Ignored,
			Accepted:      s.Counters.Accepted,
			Rejected:      s.Counters.Rejected,
			RejectReasons: make(map[string]uint64, len(s.Counters.RejectReasons)),
			Decisions:     make(map[string]uint64, len(s.Counters.Decisions)),
		},
		LastError:   s.LastError,
		LastErrorAt: s.LastErrorAt,
		LastWriteAt: make(map[fingerprint.Provider]time.Time, len(s.LastWriteAt)),
	}
	for k, v := range s.Baselines {
		out.Baselines[k] = v
	}
	for _, p := range s.Pending {
		cp := p
		cp.Records = append([]learner.Record(nil), p.Records...)
		out.Pending = append(out.Pending, cp)
	}
	for k, v := range s.LastPromotion {
		if v != nil {
			cp := *v
			out.LastPromotion[k] = &cp
		}
	}
	for k, v := range s.Counters.RejectReasons {
		out.Counters.RejectReasons[k] = v
	}
	for k, v := range s.Counters.Decisions {
		out.Counters.Decisions[k] = v
	}
	for k, v := range s.LastWriteAt {
		out.LastWriteAt[k] = v
	}
	return out
}
