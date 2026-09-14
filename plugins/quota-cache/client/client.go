// Package client reads the quota-cache snapshot without making network requests.
// It is safe to import into independent native plugins: no cache or poller is
// created in the consuming library.
package client

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"time"
)

const DefaultPath = "plugins/data/quota-cache/snapshot.json"
const MaxBytes = 4 << 20

var ErrUnavailable = errors.New("quota cache unavailable, stale, or waiting for refresh")

// Canonical window keys. quota-cache normalizes every provider's free-form
// window identifiers into this vocabulary at collection time so that no
// consumer ever pattern-matches an upstream string. A window with no canonical
// meaning is emitted as WindowRawPrefix+provider+":"+original rather than
// dropped, so an unrecognized upstream window is visible before it is understood.
const (
	WindowSession      = "session"       // short rolling window; Claude's 5-hour limit
	WindowWeekly       = "weekly"        // the regular multi-day window; mirrors Entry.Percent
	WindowWeeklyFable  = "weekly_fable"  // Claude's separate 7-day Fable/Opus allowance
	WindowModelSession = "model_session" // per-model short window; Model is set
	WindowModelWeekly  = "model_weekly"  // per-model multi-day window; Model is set
	WindowCredits      = "credits"       // consumable balance rather than a rate window
	WindowMonthly      = "monthly"       // calendar-month window, where a provider exposes one
	WindowRawPrefix    = "raw:"
)

// EntryWindow is one canonical quota window. It is a separate type from Window,
// which remains the verbatim per-provider projection under Entry.Quota.
//
// UsedPercent is USED capacity on 0-100, the same convention as Entry.Percent
// and clamped the same way. Consumers that display remaining capacity must
// invert it exactly once.
//
// ObservedAt is per window because windows for one credential can be observed
// at different times; an entry-level timestamp alone would overstate freshness.
type EntryWindow struct {
	Key         string  `json:"key"`
	Title       string  `json:"title,omitempty"`
	Model       string  `json:"model,omitempty"`
	UsedPercent float64 `json:"used_percent"`
	// No omitempty: it has no effect on time.Time, and promising otherwise in
	// the tag hides the real contract. A window with no reset instant
	// serializes as the zero time, exactly as Entry.ResetAt above always has.
	// Readers must treat the zero time as "unknown", never as a date — a
	// client converting it to epoch seconds gets a date in 1 BC.
	ResetAt    time.Time `json:"reset_at"`
	ObservedAt time.Time `json:"observed_at"`
	// LastError is reserved for a per-window failure. No provider currently
	// reports one; entry-level LastError covers a failed poll.
	LastError string `json:"last_error,omitempty"`
}

type Entry struct {
	Quota       *Quota    `json:"quota,omitempty"`
	Provider    string    `json:"provider"`
	AuthIndex   string    `json:"auth_index"`
	Percent     float64   `json:"used_percent"`
	ResetAt     time.Time `json:"reset_at"`
	ObservedAt  time.Time `json:"observed_at"`
	LastAttempt time.Time `json:"last_attempt"`
	NextAttempt time.Time `json:"next_attempt"`
	LastError   string    `json:"last_error,omitempty"`
	Failures    int       `json:"failures"`

	// Additive schema-1 fields. Readers built before these existed ignore them;
	// snapshots written before they existed decode them as zero values.
	// Percent and ResetAt keep their exact prior meaning: the regular weekly
	// window. They are never repurposed.
	Windows  []EntryWindow `json:"windows,omitempty"`
	Plan     string        `json:"plan,omitempty"`
	TierName string        `json:"tier_name,omitempty"`
	// Pointer because omitempty does not apply to time.Time: a value field
	// would write "0001-01-01T00:00:00Z" into every entry of every provider
	// that has no renewal concept. Nil means unknown.
	RenewalAt *time.Time `json:"renewal_at,omitempty"`
}

type Snapshot struct {
	Schema           int                  `json:"schema"`
	WrittenAt        time.Time            `json:"written_at"`
	NextRequest      time.Time            `json:"next_request"`
	ProviderCooldown map[string]time.Time `json:"provider_cooldown"`
	Entries          map[string]Entry     `json:"entries"`
	Totals           Totals               `json:"totals"`
	History          []Poll               `json:"history,omitempty"`
}

// Additive v1 fields: old readers ignore these, and old snapshots start with
// zero counters. Never store credentials, request headers, or response bodies.
type Totals struct {
	Attempts   uint64 `json:"attempts"`
	Requests   uint64 `json:"requests"`
	Successes  uint64 `json:"successes"`
	Failures   uint64 `json:"failures"`
	RateLimits uint64 `json:"rate_limits"`
}

type Poll struct {
	Provider    string    `json:"provider"`
	AuthIndex   string    `json:"auth_index"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	DurationMS  int64     `json:"duration_ms"`
	RequestSent bool      `json:"request_sent"`
	HTTPStatus  int       `json:"http_status,omitempty"`
	Outcome     string    `json:"outcome"`
	Error       string    `json:"error,omitempty"`
}

func Key(provider, authIndex string) string { return provider + ":" + authIndex }

func Load(path string) (Snapshot, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return Snapshot{}, ErrUnavailable
	}
	file, err := os.Open(path)
	if err != nil {
		return Snapshot{}, ErrUnavailable
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, MaxBytes+1))
	if err != nil || len(raw) > MaxBytes {
		return Snapshot{}, ErrUnavailable
	}
	var snapshot Snapshot
	if json.Unmarshal(raw, &snapshot) != nil || snapshot.Schema != 1 || snapshot.Entries == nil || snapshot.ProviderCooldown == nil {
		return Snapshot{}, ErrUnavailable
	}
	return snapshot, nil
}

// ReadFresh returns the original provider observation time. Reading an old
// snapshot never makes it fresh; consumers must not fall back to provider HTTP.
func ReadFresh(path, provider, authIndex string, now time.Time, maxAge time.Duration) (Entry, error) {
	snapshot, err := Load(path)
	if err != nil {
		return Entry{}, err
	}
	entry, ok := snapshot.Entries[Key(provider, authIndex)]
	if !ok || entry.Provider != provider || entry.AuthIndex != authIndex || maxAge <= 0 ||
		entry.ObservedAt.IsZero() || entry.ObservedAt.After(now) || now.Sub(entry.ObservedAt) > maxAge ||
		entry.LastError != "" || math.IsNaN(entry.Percent) || math.IsInf(entry.Percent, 0) || entry.Percent < 0 || entry.Percent > 100 ||
		(!entry.ResetAt.IsZero() && !entry.ResetAt.After(now)) {
		return Entry{}, ErrUnavailable
	}
	return entry, nil
}
