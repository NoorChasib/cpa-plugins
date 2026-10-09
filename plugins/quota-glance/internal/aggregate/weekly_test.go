package aggregate

import (
	"math"
	"strings"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// win is one window of a test credential. A zero reset is no reset instant.
func win(key string, used float64, reset time.Time) qc.EntryWindow {
	return qc.EntryWindow{Key: key, UsedPercent: used, ResetAt: reset}
}

// buildAccounts builds a document over credentials given as their windows,
// each in the provider its auth index names, the way CPA names them. Absent
// credentials are rostered with nothing in the snapshot.
func buildAccounts(t *testing.T, accounts map[string][]qc.EntryWindow, samples []Sample, absent ...string) Document {
	t.Helper()
	now := at(t, 0)
	observed := now.Add(-2 * time.Minute)
	entries := map[string]qc.Entry{}
	roster := []Identity{}
	for id, windows := range accounts {
		provider, _, _ := strings.Cut(id, "-")
		stamped := make([]qc.EntryWindow, 0, len(windows))
		for _, w := range windows {
			w.ObservedAt = observed
			stamped = append(stamped, w)
		}
		entries[qc.Key(provider, id)] = qc.Entry{Provider: provider, AuthIndex: id, ObservedAt: observed, Windows: stamped}
		roster = append(roster, Identity{AuthIndex: id, Provider: provider})
	}
	for _, id := range absent {
		provider, _, _ := strings.Cut(id, "-")
		roster = append(roster, Identity{AuthIndex: id, Provider: provider})
	}
	return Build(Input{
		Snapshot:   qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: entries},
		Identities: roster,
		Samples:    samples,
		StaleAfter: time.Hour,
	}, now)
}

func epochIs(got *int64, want time.Time) bool { return got != nil && *got == want.Unix() }

// The case the hold-out exists for. An account whose weekly is spent has an
// idle session reading 100% that nothing can use, and counting it showed the
// session pool fuller than the accounts that can still send. It is held out of
// the mean and of everything drawn from it, and still printed with its reading.
func TestSpentWeeklyHoldsItsSessionOut(t *testing.T) {
	const spent, a, b = "claude-spent@example.com.json", "claude-a@example.com.json", "claude-b@example.com.json"
	doc := buildAccounts(t, map[string][]qc.EntryWindow{
		// Its session resets soonest of all, which must not make it the next
		// recovery: that reset returns nothing anything can use.
		spent: {win(qc.WindowSession, 0, at(t, 1800)), win(qc.WindowWeekly, 100, at(t, 2*86400))},
		a:     {win(qc.WindowSession, 40, at(t, 7200)), win(qc.WindowWeekly, 50, at(t, 3*86400))},
		b:     {win(qc.WindowSession, 80, at(t, 10800)), win(qc.WindowWeekly, 20, at(t, 4*86400))},
	}, nil)
	checkPoolAddsUp(t, "held out", doc)

	session := rowOf(t, doc, "claude", qc.WindowSession)
	agg := session.Aggregate
	// 60% and 20% left. Counting the idle session would report 60%.
	if agg.RemainingPercent != 40 || agg.Level != LevelLow {
		t.Errorf("session = %d%% %s; want 40%% low, the mean of the two that can send", agg.RemainingPercent, agg.Level)
	}
	if agg.MemberCount != 2 || agg.ExcludedCount != 1 || agg.HeldOutCount != 1 {
		t.Errorf("membership = %d members, %d excluded, %d held out; want 2, 1, 1",
			agg.MemberCount, agg.ExcludedCount, agg.HeldOutCount)
	}
	if !epochIs(agg.SoonestResetAtEpoch, at(t, 7200)) || !epochIs(agg.FullAtEpoch, at(t, 10800)) {
		t.Errorf("soonest %v, full %v; want a's reset at 2h and b's at 3h", agg.SoonestResetAtEpoch, agg.FullAtEpoch)
	}
	if want := "+20% when a resets in 2h"; agg.Subtext != want {
		t.Errorf("subtext = %q; want %q", agg.Subtext, want)
	}

	held := entryOf(t, session, spent)
	if !held.HeldOut || held.PoolShare != 0 || held.RecoveryShare != 0 || held.ResetsNext {
		t.Errorf("held-out entry = %+v; want heldOut with no share and no part in the recovery", held)
	}
	// Its reading is untouched: the expanded card is where the actual numbers
	// are read, and its pooled figure is that same reading.
	if !held.HasReading || held.RemainingPercent != 100 || held.Level != LevelOK || held.State != StatusOK ||
		held.PooledPercent != 100 || !epochIs(held.ResetAtEpoch, at(t, 1800)) || held.ResetDisplayHint != HintCountdown {
		t.Errorf("held-out entry = %+v; want its real reading, 100%% resetting in 30m", held)
	}
	for _, id := range []string{a, b} {
		if entryOf(t, session, id).HeldOut {
			t.Errorf("%s has weekly to spare but was held out", id)
		}
	}

	// The weekly row is the gate, not gated: the spent account is in its mean.
	weekly := rowOf(t, doc, "claude", qc.WindowWeekly)
	if weekly.Aggregate.MemberCount != 3 || weekly.Aggregate.HeldOutCount != 0 || entryOf(t, weekly, spent).HeldOut {
		t.Errorf("weekly = %+v; the weekly row holds nothing out", weekly.Aggregate)
	}
}

// The user's own example: every account's weekly burned through reads 0% on
// session, weekly and Fable alike. The session has no member left, so it takes
// the empty row's shape — critical, nothing ahead — while every account is
// still listed with its idle session.
func TestAPoolWithEveryWeeklySpentReadsEmpty(t *testing.T) {
	spent := func(fableUsed float64) []qc.EntryWindow {
		return []qc.EntryWindow{
			win(qc.WindowSession, 0, time.Time{}),
			win(qc.WindowWeekly, 100, at(t, 86400)),
			win(qc.WindowWeeklyFable, fableUsed, at(t, 86400-60)),
		}
	}
	const a, b, c = "claude-a@example.com.json", "claude-b@example.com.json", "claude-c@example.com.json"
	doc := buildAccounts(t, map[string][]qc.EntryWindow{a: spent(10), b: spent(60), c: spent(100)},
		nil, "claude-unpolled@example.com.json")
	checkPoolAddsUp(t, "all spent", doc)

	for _, rowID := range []string{qc.WindowSession, qc.WindowWeekly, qc.WindowWeeklyFable} {
		if agg := rowOf(t, doc, "claude", rowID).Aggregate; agg.RemainingFraction != 0 || agg.RemainingPercent != 0 || agg.Level != LevelCritical {
			t.Errorf("%s = %v (%d%%, %s); want 0%% critical", rowID, agg.RemainingFraction, agg.RemainingPercent, agg.Level)
		}
	}

	session := rowOf(t, doc, "claude", qc.WindowSession)
	agg := session.Aggregate
	// Excluded covers both kinds: three held out and one with no reading.
	if agg.MemberCount != 0 || agg.ExcludedCount != 4 || agg.HeldOutCount != 3 {
		t.Errorf("membership = %d members, %d excluded, %d held out; want 0, 4, 3",
			agg.MemberCount, agg.ExcludedCount, agg.HeldOutCount)
	}
	if agg.SoonestResetAtEpoch != nil || agg.SoonestResetInSeconds != nil || agg.FullAtEpoch != nil || agg.FullInSeconds != nil ||
		agg.ProjectedGainFraction != 0 || agg.ProjectedGainPercent != 0 || agg.Subtext != "" {
		t.Errorf("session = %+v; an empty pool has nothing ahead to announce", agg)
	}
	for _, id := range []string{a, b, c} {
		if entry := entryOf(t, session, id); !entry.HeldOut || !entry.HasReading || entry.RemainingPercent != 100 {
			t.Errorf("%s = %+v; want held out at its idle 100%%", id, entry)
		}
	}

	// Fable holds nothing out: each account counts as the 0% its weekly leaves
	// it, while its entry keeps its own Fable figure.
	fable := rowOf(t, doc, "claude", qc.WindowWeeklyFable)
	if fable.Aggregate.MemberCount != 3 || fable.Aggregate.HeldOutCount != 0 {
		t.Errorf("fable membership = %+v; want all three, none held out", fable.Aggregate)
	}
	for id, raw := range map[string]int{a: 90, b: 40, c: 0} {
		if entry := entryOf(t, fable, id); entry.HeldOut || entry.PooledPercent != 0 || entry.RemainingPercent != raw {
			t.Errorf("%s = %+v; want counted at 0%%, printed at its own %d%%", id, entry, raw)
		}
	}
	// Fable and weekly reset a minute apart, one recovery, and together they
	// bring every account back in full.
	if want := "+100% when a resets in 23h 59m"; fable.Aggregate.Subtext != want {
		t.Errorf("fable subtext = %q; want %q", fable.Aggregate.Subtext, want)
	}
	if !epochIs(fable.Aggregate.FullAtEpoch, at(t, 86400)) {
		t.Errorf("fable full at %v; want the weekly's reset, the later of the two", fable.Aggregate.FullAtEpoch)
	}
}

// The gate is the weekly reading the Weekly card prints, and nothing else. Each
// case is one account beside a companion with weekly to spare, and asserts both
// rows it bears on: whether the session is held out, and what Fable counts as.
func TestTheGateIsTheWeeklyReading(t *testing.T) {
	const x, y = "claude-x@example.com.json", "claude-y@example.com.json"
	later := at(t, 2*86400)
	for _, tc := range []struct {
		name    string
		weekly  []qc.EntryWindow
		fable   float64
		heldOut bool
		pooled  float64
	}{
		{"spent, resetting later", []qc.EntryWindow{win(qc.WindowWeekly, 100, later)}, 40, true, 0},
		{"prints 0%", []qc.EntryWindow{win(qc.WindowWeekly, 99.6, later)}, 40, true, 0.004},
		{"prints 1%", []qc.EntryWindow{win(qc.WindowWeekly, 99.4, later)}, 40, false, 0.006},
		// The provider says the limit is reached and not until when.
		{"spent with no reset instant", []qc.EntryWindow{win(qc.WindowWeekly, 100, time.Time{})}, 40, true, 0},
		{"over 100", []qc.EntryWindow{win(qc.WindowWeekly, 101, later)}, 40, true, 0},
		// A reset already behind us has turned the window over since the poll:
		// the 100% no longer holds, so it gates nothing.
		{"reset already passed", []qc.EntryWindow{win(qc.WindowWeekly, 100, at(t, -60))}, 40, false, 0.6},
		{"reset exactly now", []qc.EntryWindow{win(qc.WindowWeekly, 100, at(t, 0))}, 40, false, 0.6},
		// Only a test can produce this. No information gates nothing.
		{"not a number", []qc.EntryWindow{win(qc.WindowWeekly, math.NaN(), later)}, 40, false, 0.6},
		{"no weekly window", nil, 40, false, 0.6},
		// The first weekly wins, as it does on the Weekly card.
		{"first weekly wins", []qc.EntryWindow{win(qc.WindowWeekly, 100, later), win(qc.WindowWeekly, 0, later)}, 40, true, 0},
		// claude-siphorchannel in the healthy contract: Fable spent, weekly
		// not. Fable is not the gate, so its session stays in.
		{"Fable spent, weekly not", []qc.EntryWindow{win(qc.WindowWeekly, 24, later)}, 100, false, 0},
		{"weekly above Fable", []qc.EntryWindow{win(qc.WindowWeekly, 10, later)}, 40, false, 0.6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			windows := append([]qc.EntryWindow{win(qc.WindowSession, 0, at(t, 3600))}, tc.weekly...)
			windows = append(windows, win(qc.WindowWeeklyFable, tc.fable, later.Add(-time.Minute)))
			doc := buildAccounts(t, map[string][]qc.EntryWindow{
				x: windows,
				y: {win(qc.WindowSession, 50, at(t, 7200)), win(qc.WindowWeekly, 10, at(t, 3*86400)),
					win(qc.WindowWeeklyFable, 20, at(t, 3*86400-60))},
			}, nil)
			checkPoolAddsUp(t, tc.name, doc)

			session := rowOf(t, doc, "claude", qc.WindowSession)
			want := struct{ members, heldOut, percent int }{2, 0, 75}
			if tc.heldOut {
				want = struct{ members, heldOut, percent int }{1, 1, 50}
			}
			if entry := entryOf(t, session, x); entry.HeldOut != tc.heldOut || entry.RemainingPercent != 100 {
				t.Errorf("session entry = %+v; want heldOut %v at its own 100%%", entry, tc.heldOut)
			}
			if agg := session.Aggregate; agg.MemberCount != want.members || agg.HeldOutCount != want.heldOut || agg.RemainingPercent != want.percent {
				t.Errorf("session = %d members, %d held out, %d%%; want %+v",
					agg.MemberCount, agg.HeldOutCount, agg.RemainingPercent, want)
			}

			fable := rowOf(t, doc, "claude", qc.WindowWeeklyFable)
			entry := entryOf(t, fable, x)
			if math.Abs(entry.PooledFraction-tc.pooled) > 1e-9 || entry.HeldOut {
				t.Errorf("Fable counts as %v (held out %v); want %v", entry.PooledFraction, entry.HeldOut, tc.pooled)
			}
			if raw, _ := remainingOf(tc.fable); entry.RemainingFraction != raw {
				t.Errorf("Fable entry prints %v; want its own %v", entry.RemainingFraction, raw)
			}
			if fable.Aggregate.MemberCount != 2 {
				t.Errorf("Fable has %d members; it holds nothing out", fable.Aggregate.MemberCount)
			}
		})
	}
}

// Only Claude's windows are nested under its weekly. A Codex account with its
// weekly spent and a session still reporting keeps the plain mean.
func TestOnlyClaudeSessionsAreHeldOut(t *testing.T) {
	const spent = "codex-spent@example.com.json"
	doc := buildAccounts(t, map[string][]qc.EntryWindow{
		spent:                          {win(qc.WindowSession, 10, at(t, 3600)), win(qc.WindowWeekly, 100, at(t, 2*86400))},
		"codex-other@example.com.json": {win(qc.WindowSession, 50, at(t, 7200)), win(qc.WindowWeekly, 20, at(t, 3*86400))},
	}, nil)
	checkPoolAddsUp(t, "codex", doc)
	session := rowOf(t, doc, "codex", qc.WindowSession)
	if agg := session.Aggregate; agg.MemberCount != 2 || agg.HeldOutCount != 0 || agg.RemainingPercent != 70 {
		t.Errorf("codex session = %+v; want both members at 70%%", agg)
	}
	if entry := entryOf(t, session, spent); entry.HeldOut || entry.PooledFraction != entry.RemainingFraction {
		t.Errorf("codex entry = %+v; want it counted at its own reading", entry)
	}
}

// Fable counts each account as the lesser of its Fable and its weekly, since no
// account can spend more Fable than its weekly allows. Its recovery follows:
// either reset can be the next one, a reset only returns what the other window
// then allows, and full again waits for both.
func TestFableCountsWhatItsWeeklyLeaves(t *testing.T) {
	const x, y = "claude-x@example.com.json", "claude-y@example.com.json"
	accounts := func(yWeeklyReset time.Time) map[string][]qc.EntryWindow {
		return map[string][]qc.EntryWindow{
			// 80% Fable under a weekly with 30% left: it can spend 30%. The
			// weekly binds, and resets first.
			x: {win(qc.WindowWeekly, 70, at(t, 3600)), win(qc.WindowWeeklyFable, 20, at(t, 5*3600))},
			// 40% Fable under 90% weekly: Fable binds.
			y: {win(qc.WindowWeekly, 10, yWeeklyReset), win(qc.WindowWeeklyFable, 60, at(t, 3*3600))},
		}
	}
	doc := buildAccounts(t, accounts(at(t, 6*86400)), nil)
	checkPoolAddsUp(t, "capped", doc)
	fable := rowOf(t, doc, "claude", qc.WindowWeeklyFable)
	agg := fable.Aggregate

	// 30% and 40%. Uncapped it read 60%.
	if math.Abs(agg.RemainingFraction-0.35) > 1e-9 || agg.RemainingPercent != 35 {
		t.Errorf("Fable = %v (%d%%); want 0.35", agg.RemainingFraction, agg.RemainingPercent)
	}
	for id, want := range map[string][2]int{x: {80, 30}, y: {40, 40}} {
		if entry := entryOf(t, fable, id); entry.RemainingPercent != want[0] || entry.PooledPercent != want[1] {
			t.Errorf("%s prints %d%% and counts %d%%; want %v", id, entry.RemainingPercent, entry.PooledPercent, want)
		}
	}

	// The next recovery is x's weekly, an hour out and ahead of any Fable
	// reset. It lifts x from 30% to its own 80%, not to 100%: the Fable it has
	// spent stays spent.
	if !epochIs(agg.SoonestResetAtEpoch, at(t, 3600)) {
		t.Errorf("soonest = %v; want x's weekly at 1h", agg.SoonestResetAtEpoch)
	}
	if got := entryOf(t, fable, x); !got.ResetsNext || math.Abs(got.RecoveryShare-0.25) > 1e-9 {
		t.Errorf("x = next %v, recovery %v; want its weekly to return (0.8-0.3)/2", got.ResetsNext, got.RecoveryShare)
	}
	if got := entryOf(t, fable, y); got.ResetsNext || got.RecoveryShare != 0 {
		t.Errorf("y = next %v, recovery %v; it resets later", got.ResetsNext, got.RecoveryShare)
	}
	if agg.ProjectedGainPercent != 25 || agg.Subtext != "+25% when x resets in 1h" {
		t.Errorf("gain %d%%, subtext %q; want +25%% from x in 1h", agg.ProjectedGainPercent, agg.Subtext)
	}
	// x waits for its Fable at 5h, y for its weekly at 6d: full after the later.
	if !epochIs(agg.FullAtEpoch, at(t, 6*86400)) {
		t.Errorf("full at %v; want y's weekly at 6d", agg.FullAtEpoch)
	}

	// A weekly below full with no reset instant never refills on a schedule,
	// so neither does the Fable it caps.
	unknown := rowOf(t, buildAccounts(t, accounts(time.Time{}), nil), "claude", qc.WindowWeeklyFable)
	if unknown.Aggregate.FullAtEpoch != nil || unknown.Aggregate.FullInSeconds != nil {
		t.Errorf("full at %v; want null while a weekly it waits for has no reset", unknown.Aggregate.FullAtEpoch)
	}
	if !epochIs(unknown.Aggregate.SoonestResetAtEpoch, at(t, 3600)) {
		t.Errorf("soonest = %v; only the full instant is unknowable", unknown.Aggregate.SoonestResetAtEpoch)
	}

	// A Fable reset under a weekly that still binds returns nothing. It is
	// still the next thing to happen to the row, so the card names it without
	// inventing a gain.
	held := buildAccounts(t, map[string][]qc.EntryWindow{
		x: {win(qc.WindowWeekly, 90, at(t, 4*86400)), win(qc.WindowWeeklyFable, 80, at(t, 3600))},
		y: {win(qc.WindowWeekly, 0, at(t, 5*86400)), win(qc.WindowWeeklyFable, 50, at(t, 2*86400))},
	}, nil)
	checkPoolAddsUp(t, "held down", held)
	row := rowOf(t, held, "claude", qc.WindowWeeklyFable)
	if got := entryOf(t, row, x); !got.ResetsNext || got.RecoveryShare != 0 {
		t.Errorf("x = next %v, recovery %v; its Fable resets into a weekly with 10%% left", got.ResetsNext, got.RecoveryShare)
	}
	if row.Aggregate.ProjectedGainFraction != 0 || row.Aggregate.Subtext != "x resets in 1h" {
		t.Errorf("gain %v, subtext %q; want nothing returned", row.Aggregate.ProjectedGainFraction, row.Aggregate.Subtext)
	}
	// x waits for its weekly at 4d; y's weekly is full and not waited for.
	if !epochIs(row.Aggregate.FullAtEpoch, at(t, 4*86400)) {
		t.Errorf("full at %v; want x's weekly at 4d", row.Aggregate.FullAtEpoch)
	}
}

// The trend arrow sits beside the headline, so it describes the headline: the
// figures the mean counts, over the members it counts.
func TestTrendFollowsWhatTheMeanCounts(t *testing.T) {
	const x, spent = "claude-x@example.com.json", "claude-spent@example.com.json"
	accounts := map[string][]qc.EntryWindow{
		// Fable 80% left, capped to 30% by its weekly.
		x: {win(qc.WindowSession, 40, at(t, 7200)), win(qc.WindowWeekly, 70, at(t, 86400)),
			win(qc.WindowWeeklyFable, 20, at(t, 86400-60))},
		spent: {win(qc.WindowSession, 0, time.Time{}), win(qc.WindowWeekly, 100, at(t, 2*86400)),
			win(qc.WindowWeeklyFable, 50, at(t, 2*86400-60))},
	}
	history := func(id, rowID string, remaining float64) []Sample {
		return []Sample{
			{AuthIndex: id, WindowKey: rowID, At: at(t, -5400), Remaining: remaining},
			{AuthIndex: id, WindowKey: rowID, At: at(t, -2400), Remaining: remaining},
		}
	}
	trend := func(rowID string, samples ...[]Sample) string {
		all := []Sample{}
		for _, s := range samples {
			all = append(all, s...)
		}
		return rowOf(t, buildAccounts(t, accounts, all), "claude", rowID).Aggregate.Trend
	}

	// Counted at 30% an hour ago and now. Measured on its raw 80%, the same
	// history would read up.
	if got := trend(qc.WindowWeeklyFable, history(x, qc.WindowWeeklyFable, 0.3), history(spent, qc.WindowWeeklyFable, 0)); got != TrendFlat {
		t.Errorf("Fable trend = %q; nothing it counts has moved", got)
	}
	// History recorded before the cap holds the raw figure, so the first hour
	// after an upgrade reads down. Accepted, and documented on SamplesFrom.
	if got := trend(qc.WindowWeeklyFable, history(x, qc.WindowWeeklyFable, 0.8)); got != TrendDown {
		t.Errorf("Fable trend against raw history = %q; want down", got)
	}
	// The held-out account is not compared. Its session was in use an hour ago
	// and is idle now; counting it would read up.
	if got := trend(qc.WindowSession, history(x, qc.WindowSession, 0.6), history(spent, qc.WindowSession, 0)); got != TrendFlat {
		t.Errorf("session trend = %q; the one member it counts is unchanged", got)
	}

	// And what is recorded for later is the counted figure, a held-out entry's
	// being its own reading.
	recorded := map[string]float64{}
	for _, s := range SamplesFrom(buildAccounts(t, accounts, nil), at(t, 0)) {
		recorded[s.WindowKey+"|"+s.AuthIndex] = s.Remaining
	}
	if got := recorded[qc.WindowWeeklyFable+"|"+x]; math.Abs(got-0.3) > 1e-9 {
		t.Errorf("Fable sample = %v; want the capped 0.3", got)
	}
	if got, ok := recorded[qc.WindowSession+"|"+spent]; !ok || got != 1 {
		t.Errorf("held-out session sample = %v (recorded %v); want its own 1", got, ok)
	}
}

// The healthy contract, against the worked numbers. No account there has its
// weekly spent, so the session is unchanged, and siphorchannel — Fable spent,
// weekly not — stays in it. Fable counts each account under its weekly.
func TestHealthyContractCountsFableUnderEachWeekly(t *testing.T) {
	doc := buildFixture(t)
	session := rowOf(t, doc, "claude", qc.WindowSession)
	if session.Aggregate.RemainingPercent != 94 || session.Aggregate.HeldOutCount != 0 {
		t.Errorf("session = %d%%, %d held out; want 94%%, none", session.Aggregate.RemainingPercent, session.Aggregate.HeldOutCount)
	}
	if entryOf(t, session, "claude-siphorchannel@example.com.json").HeldOut {
		t.Error("siphorchannel was held out for its Fable; only the weekly gates the session")
	}

	fable := rowOf(t, doc, "claude", qc.WindowWeeklyFable)
	// (weekly, Fable) remaining: (76, 0), (90, 88), (24, 45), (60, 70), (45, 10).
	for id, want := range map[string][2]int{
		"claude-siphorchannel@example.com.json": {0, 0},
		"claude-agency@example.com.json":        {88, 88},
		"claude-chasibnoor@example.com.json":    {45, 24},
		"claude-noor@example.com.json":          {70, 60},
		"claude-noorchasib@example.com.json":    {10, 10},
	} {
		if entry := entryOf(t, fable, id); entry.RemainingPercent != want[0] || entry.PooledPercent != want[1] {
			t.Errorf("%s prints %d%% and counts %d%%; want %v", id, entry.RemainingPercent, entry.PooledPercent, want)
		}
	}
	if agg := fable.Aggregate; agg.RemainingPercent != 36 || agg.Level != LevelLow {
		t.Errorf("Fable = %d%% %s; want 36%% low", agg.RemainingPercent, agg.Level)
	}
	// siphorchannel's Fable and weekly reset a minute apart, one recovery that
	// brings it from nothing to full.
	if want := "+20% when siphorchannel resets in 1d 1h"; fable.Aggregate.Subtext != want {
		t.Errorf("Fable subtext = %q; want %q", fable.Aggregate.Subtext, want)
	}
}

// The degraded contract's spent account, as the web app receives it. Its
// session resets soonest on the card and is held out, so the recovery still
// names the stale account; its Fable prints 60% and counts as nothing.
func TestDegradedContractHoldsTheSpentWeeklyOut(t *testing.T) {
	doc := buildDegraded(t)
	const spent = "claude-weekly-spent@example.com.json"
	session := rowOf(t, doc, "claude", qc.WindowSession)
	if entry := entryOf(t, session, spent); !entry.HeldOut || entry.RemainingPercent != 98 || entry.ResetDisplayHint != HintCountdown {
		t.Errorf("session entry = %+v; want held out at its own 98%%, counting down", entry)
	}
	if agg := session.Aggregate; agg.HeldOutCount != 1 || agg.Subtext != "+8% when stale resets in 30m" {
		t.Errorf("session = %d held out, subtext %q; want 1, naming stale", agg.HeldOutCount, agg.Subtext)
	}
	if entry := entryOf(t, rowOf(t, doc, "claude", qc.WindowWeeklyFable), spent); entry.RemainingPercent != 60 || entry.PooledPercent != 0 {
		t.Errorf("Fable entry prints %d%% and counts %d%%; want 60 and 0", entry.RemainingPercent, entry.PooledPercent)
	}
}
