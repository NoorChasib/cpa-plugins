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

type Entry struct {
	Provider    string    `json:"provider"`
	AuthIndex   string    `json:"auth_index"`
	Percent     float64   `json:"used_percent"`
	ResetAt     time.Time `json:"reset_at"`
	ObservedAt  time.Time `json:"observed_at"`
	LastAttempt time.Time `json:"last_attempt"`
	NextAttempt time.Time `json:"next_attempt"`
	LastError   string    `json:"last_error,omitempty"`
	Failures    int       `json:"failures"`
}

type Snapshot struct {
	Schema           int                  `json:"schema"`
	WrittenAt        time.Time            `json:"written_at"`
	NextRequest      time.Time            `json:"next_request"`
	ProviderCooldown map[string]time.Time `json:"provider_cooldown"`
	Entries          map[string]Entry     `json:"entries"`
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
