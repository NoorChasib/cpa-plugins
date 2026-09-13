package client

import (
	"encoding/json"
	"os"
	"path/filepath"
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
