package aggregate

import (
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

func TestSessionRecoveryIncludesWeeklyUnlocks(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "every_weekly_spent", true: "weekly_before_session"}[active], func(t *testing.T) {
			accounts := map[string][]qc.EntryWindow{
				"claude-blocked@example.com.json": {win(qc.WindowSession, 0, time.Time{}), win(qc.WindowWeekly, 100, at(t, 3600))},
			}
			if active {
				accounts["claude-active@example.com.json"] = []qc.EntryWindow{win(qc.WindowSession, 100, at(t, 7200)), win(qc.WindowWeekly, 20, at(t, 86400))}
			}
			row := rowOf(t, buildAccounts(t, accounts, nil), "claude", qc.WindowSession)
			if got := row.Aggregate.SoonestResetInSeconds; got == nil || *got != 3600 {
				t.Fatalf("next recovery = %v; want weekly unlock in 3600s", got)
			}
			wantGain := 100
			if active {
				wantGain = 50
			}
			if row.Aggregate.ProjectedGainPercent != wantGain || row.Aggregate.ReturningAccountCount != 1 {
				t.Fatalf("forecast = %+v; want +%d%% and one returning account", row.Aggregate, wantGain)
			}
			entry := entryOf(t, row, "claude-blocked@example.com.json")
			if !entry.HeldOut || !entry.ResetsNext || entry.RecoveryKind != "weekly" || entry.PoolShare != 0 {
				t.Fatalf("weekly unlock = %+v", entry)
			}
			checkPoolAddsUp(t, "weekly unlock", Document{Providers: []Provider{{ID: "claude", CredentialCount: len(accounts), Rows: []Row{row}}}})
		})
	}
}

func TestSessionRecoveryRequiresBothExhaustedWindows(t *testing.T) {
	for _, offsets := range [][2]int64{{3600, 7200}, {7200, 3600}} {
		row := rowOf(t, buildAccounts(t, map[string][]qc.EntryWindow{
			"claude-blocked@example.com.json": {win(qc.WindowSession, 100, at(t, offsets[0])), win(qc.WindowWeekly, 100, at(t, offsets[1]))},
		}, nil), "claude", qc.WindowSession)
		if !epochIs(row.Aggregate.SoonestResetAtEpoch, at(t, 7200)) || !epochIs(row.Aggregate.FullAtEpoch, at(t, 7200)) {
			t.Fatalf("session and weekly at %v: %+v; want recovery and full at 2h", offsets, row.Aggregate)
		}
	}
}

func TestSessionWeeklyUnlockRecomputesTheMean(t *testing.T) {
	row := rowOf(t, buildAccounts(t, map[string][]qc.EntryWindow{
		"claude-active@example.com.json":  {win(qc.WindowSession, 20, at(t, 10800)), win(qc.WindowWeekly, 20, at(t, 86400))},
		"claude-blocked@example.com.json": {win(qc.WindowSession, 60, at(t, 7200)), win(qc.WindowWeekly, 100, at(t, 3600))},
	}, nil), "claude", qc.WindowSession)
	// More usable capacity, but the mean becomes (80 + 40)/2 = 60%, below
	// today's 80%. Announce the account returning without inventing a gain.
	if row.Aggregate.RemainingPercent != 80 || row.Aggregate.ProjectedGainPercent != 0 || row.Aggregate.ReturningAccountCount != 1 {
		t.Fatalf("forecast = %+v", row.Aggregate)
	}
	if !epochIs(row.Aggregate.SoonestResetAtEpoch, at(t, 3600)) || !epochIs(row.Aggregate.FullAtEpoch, at(t, 10800)) {
		t.Fatalf("forecast times = %+v", row.Aggregate)
	}
}

func TestSessionWeeklyUnlockAfterSessionTurnover(t *testing.T) {
	row := rowOf(t, buildAccounts(t, map[string][]qc.EntryWindow{
		"claude-blocked@example.com.json": {win(qc.WindowSession, 100, at(t, -300)), win(qc.WindowWeekly, 100, at(t, 3600))},
	}, nil), "claude", qc.WindowSession)
	if row.Aggregate.RemainingPercent != 0 {
		t.Fatal("forecast changed the current quota reading")
	}
	if !epochIs(row.Aggregate.SoonestResetAtEpoch, at(t, 3600)) || !epochIs(row.Aggregate.FullAtEpoch, at(t, 3600)) {
		t.Fatalf("turnover recovery = %+v; want recovery and full at the weekly reset", row.Aggregate)
	}
	if row.Aggregate.ProjectedGainPercent != 100 || row.Aggregate.ReturningAccountCount != 1 {
		t.Fatalf("turnover gain = %+v; want a full returning account", row.Aggregate)
	}
}

func TestSessionForecastDoesNotNameAnElapsedResetAsANewRecovery(t *testing.T) {
	row := rowOf(t, buildAccounts(t, map[string][]qc.EntryWindow{
		"claude-elapsed@example.com.json": {win(qc.WindowSession, 100, at(t, -300)), win(qc.WindowWeekly, 20, at(t, 86400))},
		"claude-next@example.com.json":    {win(qc.WindowSession, 100, at(t, 3600)), win(qc.WindowWeekly, 20, at(t, 86400))},
	}, nil), "claude", qc.WindowSession)
	if !epochIs(row.Aggregate.SoonestResetAtEpoch, at(t, 3600)) || !entryOf(t, row, "claude-next@example.com.json").ResetsNext {
		t.Fatalf("next recovery = %+v", row)
	}
	if entryOf(t, row, "claude-elapsed@example.com.json").ResetsNext {
		t.Fatal("elapsed reset was attributed to an unrelated future event")
	}
}
