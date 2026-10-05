package aggregate

import (
	"math"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// The dashboard draws each card's pool as the entries' shares laid end to end,
// and what the next reset returns as their recovery shares after them. Neither
// is a second computation of the pool: in every row of both committed contracts
// the slices add back up to the aggregate the headline prints, so the bar and
// the number beside it cannot drift apart.
func TestPoolSharesAddUpToTheAggregate(t *testing.T) {
	const tolerance = 1e-9
	for name, doc := range map[string]Document{"summary": buildFixture(t), "degraded": buildDegraded(t)} {
		for _, provider := range doc.Providers {
			for _, row := range provider.Rows {
				where := name + " " + provider.ID + " " + row.RowID
				pool, gain := 0.0, 0.0
				for _, entry := range row.Entries {
					pool += entry.PoolShare
					gain += entry.RecoveryShare
					if !entry.HasReading && (entry.PoolShare != 0 || entry.RecoveryShare != 0 || entry.ResetsNext) {
						t.Errorf("%s: %s has no reading but holds a slice: %+v", where, entry.CredentialID, entry)
					}
					if entry.RecoveryShare > 0 && !entry.ResetsNext {
						t.Errorf("%s: %s returns capacity at a reset it is not part of", where, entry.CredentialID)
					}
				}
				if math.Abs(pool-row.Aggregate.RemainingFraction) > tolerance {
					t.Errorf("%s: shares sum to %v; the aggregate is %v", where, pool, row.Aggregate.RemainingFraction)
				}
				if math.Abs(gain-row.Aggregate.ProjectedGainFraction) > tolerance {
					t.Errorf("%s: recovery shares sum to %v; the gain is %v", where, gain, row.Aggregate.ProjectedGainFraction)
				}
				if percentOf(row.Aggregate.ProjectedGainFraction) != row.Aggregate.ProjectedGainPercent {
					t.Errorf("%s: gain fraction %v prints as %d%%, not %d%%", where,
						row.Aggregate.ProjectedGainFraction, percentOf(row.Aggregate.ProjectedGainFraction),
						row.Aggregate.ProjectedGainPercent)
				}
			}
		}
	}
}

// poolRow builds one Claude weekly row from (used percent, reset offset)
// pairs, one credential each. A zero offset means the window reports no reset
// instant at all.
func poolRow(t *testing.T, windows map[string][2]int64) Row {
	t.Helper()
	now := at(t, 0)
	observed := now.Add(-2 * time.Minute)
	entries := map[string]qc.Entry{}
	roster := []Identity{}
	for id, w := range windows {
		window := qc.EntryWindow{Key: qc.WindowWeekly, UsedPercent: float64(w[0]), ObservedAt: observed}
		if w[1] != 0 {
			window.ResetAt = now.Add(time.Duration(w[1]) * time.Second)
		}
		entries[qc.Key("claude", id)] = qc.Entry{
			Provider: "claude", AuthIndex: id, ObservedAt: observed, Windows: []qc.EntryWindow{window},
		}
		roster = append(roster, Identity{AuthIndex: id, Provider: "claude"})
	}
	doc := Build(Input{
		Snapshot:   qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: entries},
		Identities: roster,
		StaleAfter: time.Hour,
	}, now)
	return rowOf(t, doc, "claude", qc.WindowWeekly)
}

func entryOf(t *testing.T, row Row, id string) RowEntry {
	t.Helper()
	for _, entry := range row.Entries {
		if entry.CredentialID == id {
			return entry
		}
	}
	t.Fatalf("no entry for %s", id)
	return RowEntry{}
}

// Two credentials resetting within the same minute are one recovery, and each
// returns what it has used; the one resetting later returns nothing yet. A
// credential already full is still part of the recovery it resets in, and
// returns nothing from it — the card names it without inventing a gain.
func TestRecoverySharesFollowTheSoonestMinute(t *testing.T) {
	row := poolRow(t, map[string][2]int64{
		"claude-a@example.com.json": {80, 3600},      // 20% left, resets first
		"claude-b@example.com.json": {50, 3600 + 30}, // 50% left, same minute
		"claude-c@example.com.json": {100, 7200},     // empty, resets an hour later
		"claude-d@example.com.json": {0, 3600},       // full, resets with a
	})
	const quarter = 0.25
	for id, want := range map[string]struct {
		pool, recovery float64
		next           bool
	}{
		"claude-a@example.com.json": {0.2 * quarter, 0.8 * quarter, true},
		"claude-b@example.com.json": {0.5 * quarter, 0.5 * quarter, true},
		"claude-c@example.com.json": {0, 0, false},
		"claude-d@example.com.json": {quarter, 0, true},
	} {
		got := entryOf(t, row, id)
		if math.Abs(got.PoolShare-want.pool) > 1e-9 || math.Abs(got.RecoveryShare-want.recovery) > 1e-9 || got.ResetsNext != want.next {
			t.Errorf("%s = pool %v, recovery %v, next %v; want %+v", id, got.PoolShare, got.RecoveryShare, got.ResetsNext, want)
		}
	}
	if want := (0.8 + 0.5) * quarter; math.Abs(row.Aggregate.ProjectedGainFraction-want) > 1e-9 {
		t.Errorf("gain = %v; want %v", row.Aggregate.ProjectedGainFraction, want)
	}
	// Full again once the last credential below full has reset: c, at 2h.
	if row.Aggregate.FullAtEpoch == nil || *row.Aggregate.FullAtEpoch != at(t, 7200).Unix() ||
		row.Aggregate.FullInSeconds == nil || *row.Aggregate.FullInSeconds != 7200 {
		t.Errorf("full at %v (in %v); want 2h out", row.Aggregate.FullAtEpoch, row.Aggregate.FullInSeconds)
	}
}

// "Full again" is a promise about every credential below full, so one that has
// no reset instant at all voids it rather than being skipped: that window does
// not refill on a schedule, and the row would never reach 100%. A credential
// already full is not waited for, whatever its reset says.
func TestFullAgainNeedsEveryDepletedCredentialToReset(t *testing.T) {
	never := poolRow(t, map[string][2]int64{
		"claude-a@example.com.json": {40, 3600},
		"claude-b@example.com.json": {10, 0}, // below full, no reset instant
	})
	if never.Aggregate.FullAtEpoch != nil || never.Aggregate.FullInSeconds != nil {
		t.Errorf("full at %v (in %v); want null while a depleted window never resets",
			never.Aggregate.FullAtEpoch, never.Aggregate.FullInSeconds)
	}
	if never.Aggregate.SoonestResetAtEpoch == nil {
		t.Fatal("the soonest reset went with it; only the full instant is unknowable")
	}

	topped := poolRow(t, map[string][2]int64{
		"claude-a@example.com.json": {30, 3600},
		"claude-b@example.com.json": {0, 9 * 3600}, // full; its later reset changes nothing
	})
	if topped.Aggregate.FullInSeconds == nil || *topped.Aggregate.FullInSeconds != 3600 {
		t.Errorf("full in %v; want 1h, the only depleted credential's reset", topped.Aggregate.FullInSeconds)
	}

	full := poolRow(t, map[string][2]int64{
		"claude-a@example.com.json": {0, 3600},
		"claude-b@example.com.json": {0, 7200},
	})
	if full.Aggregate.FullAtEpoch != nil {
		t.Errorf("full at %v; a row already full has nothing to wait for", *full.Aggregate.FullAtEpoch)
	}
}
