package client

import (
	"encoding/json"
	"errors"
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

// The subscription start and the billing period are what a consumer estimates
// a Claude renewal from. Their wire names are the contract quota-glance reads,
// and an entry without them must not gain an empty key.
func TestTheRenewalEstimateInputsAreAdditive(t *testing.T) {
	started := time.Date(2025, time.January, 31, 9, 15, 0, 0, time.UTC)
	raw, err := json.Marshal(Entry{Provider: "claude", AuthIndex: "one",
		Quota:          &Quota{Schema: 1, BillingPeriod: BillingAnnual},
		AccountDetails: &AccountDetails{CheckedAt: started, SubscriptionStartedAt: &started}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"subscription_started_at":"2025-01-31T09:15:00Z"`, `"billing_period":"annual"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("entry is missing %s: %s", want, raw)
		}
	}
	bare, _ := json.Marshal(Entry{Provider: "claude", AuthIndex: "one", Quota: &Quota{Schema: 1}, AccountDetails: &AccountDetails{}})
	if strings.Contains(string(bare), "subscription_started_at") || strings.Contains(string(bare), "billing_period") {
		t.Fatalf("empty estimate inputs serialized: %s", bare)
	}
}

// The Antigravity names consumers build on are fixed: the provider string CPA
// lists and the model tokens of the non-default quota families.
func TestAntigravityNamesAreFixed(t *testing.T) {
	for got, want := range map[string]string{
		ProviderAntigravity:      "antigravity",
		AntigravityModelShared:   "shared",
		AntigravityModelArgon:    "argon",
		AntigravityModelCarbon:   "carbon",
		AntigravityModelHydrogen: "hydrogen",
	} {
		if got != want {
			t.Fatalf("%q != %q", got, want)
		}
	}
}

// ReadFreshWindows judges each window by its own observation and reset, so an
// entry with no compatibility observation (Entry.ObservedAt zero) still
// yields the windows a successful poll published, and a stale, future,
// expired or invalid one is left out.
func TestReadFreshWindowsJudgesEachWindowOnItsOwn(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	observed := now.Add(-5 * time.Minute)
	window := func(key, model string, change func(*EntryWindow)) EntryWindow {
		w := EntryWindow{Key: key, Model: model, Title: key, UsedPercent: 40, ResetAt: now.Add(time.Hour), ObservedAt: observed}
		if change != nil {
			change(&w)
		}
		return w
	}
	entry := Entry{Provider: ProviderAntigravity, AuthIndex: "ag", Windows: []EntryWindow{
		window(WindowSession, "", func(w *EntryWindow) { w.ResetAt = time.Time{} }), // unknown reset is fine
		window(WindowModelSession, AntigravityModelShared, nil),
		window(WindowModelWeekly, AntigravityModelShared, func(w *EntryWindow) { w.ResetAt = now }),                             // reset already reached
		window(WindowModelWeekly, AntigravityModelArgon, func(w *EntryWindow) { w.ObservedAt = time.Time{} }),                   // never observed
		window(WindowModelWeekly, AntigravityModelCarbon, func(w *EntryWindow) { w.ObservedAt = now.Add(time.Minute) }),         // from the future
		window(WindowModelWeekly, AntigravityModelHydrogen, func(w *EntryWindow) { w.ObservedAt = now.Add(-31 * time.Minute) }), // stale
		window("raw:antigravity:x", "", func(w *EntryWindow) { w.UsedPercent = 101 }),
		window("raw:antigravity:y", "", func(w *EntryWindow) { w.LastError = "window failed" }),
		window(WindowModelWeekly, AntigravityModelShared, func(w *EntryWindow) { w.ObservedAt = now.Add(-30 * time.Minute) }), // exactly maxAge
	}}
	path := filepath.Join(t.TempDir(), "snapshot.json")
	write := func(e Entry) {
		t.Helper()
		raw, _ := json.Marshal(Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]Entry{Key(e.Provider, e.AuthIndex): e}})
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(entry)
	got, err := ReadFreshWindows(path, ProviderAntigravity, "ag", now, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, w := range got {
		keys = append(keys, w.Key+"/"+w.Model)
	}
	want := []string{"session/", "model_session/shared", "model_weekly/shared"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("windows = %v; want %v", keys, want)
	}
	// The compatibility reader still waits for the regular weekly.
	if _, err := ReadFresh(path, ProviderAntigravity, "ag", now, 30*time.Minute); err == nil {
		t.Fatal("ReadFresh accepted an entry with no weekly observation")
	}

	for _, tc := range []struct {
		name            string
		change          func(*Entry)
		provider, index string
		maxAge          time.Duration
	}{
		{"failed poll", func(e *Entry) { e.LastError = "quota fetch failed" }, ProviderAntigravity, "ag", 30 * time.Minute},
		{"pending poll", func(e *Entry) { e.LastError = "refresh pending" }, ProviderAntigravity, "ag", 30 * time.Minute},
		{"no windows", func(e *Entry) { e.Windows = nil }, ProviderAntigravity, "ag", 30 * time.Minute},
		{"none fresh", func(e *Entry) { e.Windows = e.Windows[3:6] }, ProviderAntigravity, "ag", 30 * time.Minute},
		{"another provider", nil, "claude", "ag", 30 * time.Minute},
		{"another account", nil, ProviderAntigravity, "other", 30 * time.Minute},
		{"entry keyed under the wrong account", func(e *Entry) { e.AuthIndex = "other" }, ProviderAntigravity, "ag", 30 * time.Minute},
		{"no max age", nil, ProviderAntigravity, "ag", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := entry
			e.Windows = append([]EntryWindow(nil), entry.Windows...)
			if tc.change != nil {
				tc.change(&e)
			}
			raw, _ := json.Marshal(Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]Entry{Key(ProviderAntigravity, "ag"): e}})
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if windows, err := ReadFreshWindows(path, tc.provider, tc.index, now, tc.maxAge); !errors.Is(err, ErrUnavailable) || windows != nil {
				t.Fatalf("windows=%v err=%v; want ErrUnavailable", windows, err)
			}
		})
	}
	if _, err := ReadFreshWindows(filepath.Join(t.TempDir(), "missing.json"), ProviderAntigravity, "ag", now, time.Hour); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a missing snapshot: %v", err)
	}
}
