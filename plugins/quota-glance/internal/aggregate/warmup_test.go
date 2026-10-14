package aggregate

import (
	"encoding/json"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

func TestWarmupSummaryUsesEpochSecondsAndNullUnknownTimes(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	state := &qc.WarmupState{
		Enabled: true, Model: "claude-haiku-5-5", Effort: "low", UpdatedAt: now,
		Accounts: []qc.WarmupAccount{{AuthIndex: "0123456789abcdef", IdentityHash: "internal", State: "awaiting_confirmation", LastAttempt: now}},
	}
	status := Build(Input{Warmup: state}, now).Warmup
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["updatedAtEpoch"] != float64(now.Unix()) {
		t.Fatalf("update timestamp is not Unix seconds: %s", raw)
	}
	accounts := wire["accounts"].([]any)
	account := accounts[0].(map[string]any)
	if account["authIndex"] != "0123456789abcdef" || account["lastAttemptEpoch"] != float64(now.Unix()) {
		t.Fatalf("account contract: %s", raw)
	}
	for _, key := range []string{"lastSuccessEpoch", "nextAttemptEpoch", "resetAtEpoch"} {
		if value, exists := account[key]; !exists || value != nil {
			t.Fatalf("unknown %s must be present and null: %s", key, raw)
		}
	}
	if len(account) != 6 {
		t.Fatalf("unexpected account fields, including internal journal data: %s", raw)
	}
	status.Accounts[0].State = "changed"
	if state.Accounts[0].State != "awaiting_confirmation" {
		t.Fatal("summary shares mutable account data with the journal")
	}
}

func TestWarmupSummaryMarksStaleStatusWithoutChangingTheJournal(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	for _, updated := range []time.Time{time.Time{}, now.Add(-3 * time.Minute), now.Add(2 * time.Minute), now} {
		state := &qc.WarmupState{Enabled: true, UpdatedAt: updated}
		status := Build(Input{Warmup: state}, now).Warmup
		stale := !updated.Equal(now)
		if (status.LastError == "warmup_status_stale") != stale || state.LastError != "" {
			t.Fatalf("staleness at %s: %+v; original error %q", updated, status, state.LastError)
		}
		if status.Accounts == nil {
			t.Fatal("empty accounts must serialize as an array")
		}
	}
	if Build(Input{}, now).Warmup != nil {
		t.Fatal("missing warm-up journal must remain optional")
	}
}
