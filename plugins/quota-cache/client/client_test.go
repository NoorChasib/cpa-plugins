package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFreshnessIdentityAndExpiredWindow(t *testing.T) {
	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	good := Entry{Provider: "claude", AuthIndex: "one", ObservedAt: now.Add(-time.Minute), ResetAt: now.Add(time.Hour), Percent: 20}
	for _, tc := range []struct {
		name   string
		change func(*Entry)
	}{
		{"old", func(e *Entry) { e.ObservedAt = now.Add(-time.Hour) }},
		{"future", func(e *Entry) { e.ObservedAt = now.Add(time.Minute) }},
		{"wrong account", func(e *Entry) { e.AuthIndex = "two" }},
		{"expired window", func(e *Entry) { e.ResetAt = now }},
		{"failed", func(e *Entry) { e.LastError = "provider rate limited" }},
		{"invalid percentage", func(e *Entry) { e.Percent = 101 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := good
			tc.change(&entry)
			snapshot := Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]Entry{"claude:one": entry}}
			raw, _ := json.Marshal(snapshot)
			path := filepath.Join(t.TempDir(), "snapshot.json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadFresh(path, "claude", "one", now, 30*time.Minute); err == nil {
				t.Fatal("invalid observation accepted")
			}
		})
	}
}

func TestExtendedAndLegacyFreshnessRemainIndependent(t *testing.T) {
	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	reset := now.Add(time.Minute)
	pct := float64(10)
	entry := Entry{Provider: "claude", AuthIndex: "one", Quota: &Quota{Schema: 1, ObservedAt: now, Windows: map[string]Window{"five_hour": {UsedPercent: &pct, ResetsAt: &reset}}}}
	path := filepath.Join(t.TempDir(), "snapshot.json")
	write := func() {
		raw, _ := json.Marshal(Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]Entry{"claude:one": entry}})
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if _, err := ReadFresh(path, "claude", "one", now, time.Hour); err == nil {
		t.Fatal("legacy reader accepted missing weekly observation")
	}
	if _, err := ReadWindow(path, "claude", "one", "five_hour", now, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadWindow(path, "claude", "one", "five_hour", reset, time.Hour); err == nil {
		t.Fatal("expired individual window accepted")
	}
	for _, when := range []time.Time{now.Add(-time.Second), now.Add(2 * time.Hour)} {
		if _, err := ReadQuota(path, "claude", "one", when, time.Hour); err == nil {
			t.Fatal("future or stale observation accepted")
		}
	}
	entry.Quota = nil
	entry.ObservedAt = now
	entry.Percent = 10
	write()
	if _, err := ReadFresh(path, "claude", "one", now, time.Hour); err != nil {
		t.Fatal("legacy snapshot rejected")
	}
	if _, err := ReadQuota(path, "claude", "one", now, time.Hour); err == nil {
		t.Fatal("legacy snapshot invented extended fields")
	}
}

// The additive fields must not require a consumer rebuild. Account Health and
// Reset Priority ship as separate binaries with their own compiled copy of this
// package; a snapshot carrying fields they predate has to decode in their build
// exactly as it did before, with the weekly observation untouched.
func TestNewFieldsAreAdditiveForConsumersBuiltBeforeThem(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	observed := now.Add(-2 * time.Minute)
	reset := now.Add(62 * time.Hour)
	entry := Entry{
		Provider: "claude", AuthIndex: "claude-one@example.com.json",
		Percent: 76, ResetAt: reset, ObservedAt: observed,
		LastAttempt: observed, NextAttempt: now.Add(13 * time.Minute),
		Plan: "Max", TierName: "max_20x",
		Windows: []EntryWindow{
			{Key: WindowSession, Title: "Session", UsedPercent: 31, ResetAt: now.Add(75 * time.Minute), ObservedAt: observed},
			{Key: WindowWeekly, Title: "Weekly", UsedPercent: 76, ResetAt: reset, ObservedAt: observed},
			{Key: WindowWeeklyFable, UsedPercent: 100, ResetAt: reset.Add(-time.Minute), ObservedAt: observed},
		},
	}
	raw, err := json.Marshal(Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{},
		Entries: map[string]Entry{Key("claude", entry.AuthIndex): entry}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}

	// The snapshot stays on schema 1. Emitting 2 would make every Load in an
	// already-released consumer return ErrUnavailable, which both report as
	// "no data" rather than as an error.
	snapshot, err := Load(path)
	if err != nil || snapshot.Schema != 1 {
		t.Fatalf("load err=%v schema=%d", err, snapshot.Schema)
	}
	fresh, err := ReadFresh(path, "claude", entry.AuthIndex, now, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Percent != 76 || !fresh.ResetAt.Equal(reset) || !fresh.ObservedAt.Equal(observed) {
		t.Fatalf("weekly observation changed meaning: %+v", fresh)
	}

	// A decoder compiled before these fields existed: encoding/json drops keys
	// it has no field for, so the weekly observation still arrives intact.
	var legacy struct {
		Entries map[string]struct {
			Provider   string    `json:"provider"`
			AuthIndex  string    `json:"auth_index"`
			Percent    float64   `json:"used_percent"`
			ResetAt    time.Time `json:"reset_at"`
			ObservedAt time.Time `json:"observed_at"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatalf("snapshot is undecodable by a consumer built before the new fields: %v", err)
	}
	old := legacy.Entries[Key("claude", entry.AuthIndex)]
	if old.Percent != 76 || !old.ResetAt.Equal(reset) || !old.ObservedAt.Equal(observed) {
		t.Fatalf("legacy decode differs: %+v", old)
	}

	// Wire names are the contract quota-glance reads; assert them literally.
	for _, want := range []string{`"windows":`, `"key":"session"`, `"used_percent":76`, `"plan":"Max"`, `"tier_name":"max_20x"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("snapshot is missing %s", want)
		}
	}
	// An entry with no windows must not gain an empty key, so snapshots from
	// before this change stay byte-identical.
	bare, _ := json.Marshal(Entry{Provider: "codex", AuthIndex: "two"})
	if strings.Contains(string(bare), "windows") || strings.Contains(string(bare), "plan") {
		t.Fatalf("empty optional fields serialized: %s", bare)
	}
}
