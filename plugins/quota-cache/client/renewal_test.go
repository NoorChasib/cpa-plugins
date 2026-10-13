package client

import (
	"path/filepath"
	"testing"
	"time"
)

func utcAt(year int, month time.Month, day, hour, minute, second int) time.Time {
	return time.Date(year, month, day, hour, minute, second, 0, time.UTC)
}

// renewalNow is the instant Quota Glance's committed summary documents are
// built at, the one these cases were written against.
var renewalNow = time.Unix(1789012800, 0).UTC() // 2026-09-10T04:00:00Z

// The anniversary rule card billing uses for a subscription's anchor: the
// start's day of the month and time of day in UTC, clamped to the last day of
// a shorter month, every anniversary counted from the start itself so a short
// month never drags the day down for good, and the first one strictly after
// now.
func TestNextAnniversary(t *testing.T) {
	const monthly, yearly = 1, 12
	jan31 := utcAt(2025, time.January, 31, 9, 15, 0)
	for _, tc := range []struct {
		name   string
		start  time.Time
		months int
		now    time.Time
		want   time.Time
	}{
		{"mid-month, later this month", utcAt(2025, time.March, 15, 10, 0, 0), monthly,
			utcAt(2026, time.September, 10, 4, 0, 0), utcAt(2026, time.September, 15, 10, 0, 0)},
		{"mid-month, already past this month", utcAt(2025, time.March, 5, 10, 0, 0), monthly,
			utcAt(2026, time.September, 10, 4, 0, 0), utcAt(2026, time.October, 5, 10, 0, 0)},
		{"today, earlier in the day", utcAt(2025, time.March, 10, 3, 0, 0), monthly,
			utcAt(2026, time.September, 10, 4, 0, 0), utcAt(2026, time.October, 10, 3, 0, 0)},
		{"today, later in the day", utcAt(2025, time.March, 10, 5, 0, 0), monthly,
			utcAt(2026, time.September, 10, 4, 0, 0), utcAt(2026, time.September, 10, 5, 0, 0)},

		// A start on the 31st, across February and the 30-day months.
		{"31st into a short February", jan31, monthly, utcAt(2026, time.February, 10, 0, 0, 0), utcAt(2026, time.February, 28, 9, 15, 0)},
		{"31st into a leap February", jan31, monthly, utcAt(2028, time.February, 10, 0, 0, 0), utcAt(2028, time.February, 29, 9, 15, 0)},
		{"31st back to March 31, no drift", jan31, monthly, utcAt(2026, time.March, 1, 0, 0, 0), utcAt(2026, time.March, 31, 9, 15, 0)},
		{"31st after the February one", jan31, monthly, utcAt(2026, time.February, 28, 10, 0, 0), utcAt(2026, time.March, 31, 9, 15, 0)},
		{"31st into April", jan31, monthly, utcAt(2026, time.April, 2, 0, 0, 0), utcAt(2026, time.April, 30, 9, 15, 0)},
		{"31st into September", jan31, monthly, utcAt(2026, time.September, 10, 4, 0, 0), utcAt(2026, time.September, 30, 9, 15, 0)},
		{"31st into December", jan31, monthly, utcAt(2026, time.December, 1, 0, 0, 0), utcAt(2026, time.December, 31, 9, 15, 0)},

		// The 30th and the 29th, the days February has only sometimes.
		{"30th into a short February", utcAt(2025, time.January, 30, 12, 0, 0), monthly,
			utcAt(2026, time.February, 1, 0, 0, 0), utcAt(2026, time.February, 28, 12, 0, 0)},
		{"30th into a leap February", utcAt(2025, time.January, 30, 12, 0, 0), monthly,
			utcAt(2028, time.February, 1, 0, 0, 0), utcAt(2028, time.February, 29, 12, 0, 0)},
		{"30th back to March 30", utcAt(2025, time.January, 30, 12, 0, 0), monthly,
			utcAt(2026, time.March, 1, 0, 0, 0), utcAt(2026, time.March, 30, 12, 0, 0)},
		{"29th into a short February", utcAt(2025, time.January, 29, 6, 30, 0), monthly,
			utcAt(2026, time.February, 1, 0, 0, 0), utcAt(2026, time.February, 28, 6, 30, 0)},
		{"29th into a leap February", utcAt(2025, time.January, 29, 6, 30, 0), monthly,
			utcAt(2028, time.February, 1, 0, 0, 0), utcAt(2028, time.February, 29, 6, 30, 0)},
		{"29th back to March 29", utcAt(2025, time.January, 29, 6, 30, 0), monthly,
			utcAt(2026, time.March, 1, 0, 0, 0), utcAt(2026, time.March, 29, 6, 30, 0)},

		// Annual plans.
		{"annual, later this year", utcAt(2024, time.March, 12, 17, 40, 0), yearly,
			utcAt(2026, time.March, 1, 0, 0, 0), utcAt(2026, time.March, 12, 17, 40, 0)},
		{"annual, already past this year", utcAt(2024, time.March, 12, 17, 40, 0), yearly,
			utcAt(2026, time.September, 10, 4, 0, 0), utcAt(2027, time.March, 12, 17, 40, 0)},
		{"annual Feb 29 into a common year", utcAt(2024, time.February, 29, 8, 0, 0), yearly,
			utcAt(2026, time.September, 10, 4, 0, 0), utcAt(2027, time.February, 28, 8, 0, 0)},
		{"annual Feb 29 back to a leap year", utcAt(2024, time.February, 29, 8, 0, 0), yearly,
			utcAt(2027, time.March, 1, 0, 0, 0), utcAt(2028, time.February, 29, 8, 0, 0)},
		{"annual 31st of a 31-day month", utcAt(2023, time.October, 31, 0, 0, 0), yearly,
			utcAt(2026, time.September, 10, 4, 0, 0), utcAt(2026, time.October, 31, 0, 0, 0)},

		// At the anniversary's own second it is happening, not ahead.
		{"exactly on a monthly anniversary", utcAt(2025, time.March, 15, 10, 0, 0), monthly,
			utcAt(2026, time.September, 15, 10, 0, 0), utcAt(2026, time.October, 15, 10, 0, 0)},
		{"a second before it", utcAt(2025, time.March, 15, 10, 0, 0), monthly,
			utcAt(2026, time.September, 15, 9, 59, 59), utcAt(2026, time.September, 15, 10, 0, 0)},
		{"exactly on a clamped anniversary", jan31, monthly, utcAt(2026, time.February, 28, 9, 15, 0), utcAt(2026, time.March, 31, 9, 15, 0)},
		{"exactly on an annual anniversary", utcAt(2024, time.February, 29, 8, 0, 0), yearly,
			utcAt(2027, time.February, 28, 8, 0, 0), utcAt(2028, time.February, 29, 8, 0, 0)},
		{"exactly at the start", utcAt(2026, time.August, 31, 9, 0, 0), monthly,
			utcAt(2026, time.August, 31, 9, 0, 0), utcAt(2026, time.September, 30, 9, 0, 0)},

		// Years back, and across the turn of a year.
		{"started years ago, monthly", utcAt(2006, time.December, 31, 23, 59, 59), monthly,
			utcAt(2026, time.September, 10, 4, 0, 0), utcAt(2026, time.September, 30, 23, 59, 59)},
		{"started years ago, annual", utcAt(2006, time.December, 31, 23, 59, 59), yearly,
			utcAt(2026, time.September, 10, 4, 0, 0), utcAt(2026, time.December, 31, 23, 59, 59)},
		{"into the next year", utcAt(2025, time.November, 30, 0, 0, 0), monthly,
			utcAt(2025, time.December, 31, 0, 0, 0), utcAt(2026, time.January, 30, 0, 0, 0)},
		{"December into January", utcAt(2025, time.December, 31, 12, 0, 0), monthly,
			utcAt(2026, time.January, 5, 0, 0, 0), utcAt(2026, time.January, 31, 12, 0, 0)},

		// UTC is the calendar, whatever zone the start was written in: 23:30
		// on the 31st in New York is 04:30 on the 1st.
		{"a start written in another zone", time.Date(2025, time.January, 31, 23, 30, 0, 0, time.FixedZone("EST", -5*3600)), monthly,
			utcAt(2026, time.September, 10, 4, 0, 0), utcAt(2026, time.October, 1, 4, 30, 0)},
		// Whole seconds: a fraction must not put the anniversary a hair past
		// a now that is itself whole, to be printed as already passed.
		{"a fractional start", utcAt(2026, time.August, 10, 4, 0, 0).Add(500 * time.Millisecond), monthly,
			utcAt(2026, time.September, 10, 4, 0, 0), utcAt(2026, time.October, 10, 4, 0, 0)},

		// And whatever zone now is written in. 22:30 UTC on Sep 30 is already
		// Oct 1 in Sydney and Berlin; counted from those calendars the months
		// run one long and skip the anniversary 30 minutes away for the one a
		// month after it. Behind UTC, New York's Sep 30 is still the UTC one's.
		{"a now in a zone already in the next month", utcAt(2026, time.January, 30, 23, 0, 0), monthly,
			utcAt(2026, time.September, 30, 22, 30, 0).In(time.FixedZone("AEST", 10*3600)), utcAt(2026, time.September, 30, 23, 0, 0)},
		{"a month-end now an hour or two ahead", utcAt(2026, time.August, 31, 23, 45, 0), monthly,
			utcAt(2026, time.September, 30, 23, 30, 0).In(time.FixedZone("CEST", 2*3600)), utcAt(2026, time.September, 30, 23, 45, 0)},
		{"a now in a zone still in the last month", utcAt(2026, time.January, 30, 23, 0, 0), monthly,
			utcAt(2026, time.October, 1, 2, 0, 0).In(time.FixedZone("EDT", -4*3600)), utcAt(2026, time.October, 30, 23, 0, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := NextAnniversary(tc.start, tc.months, tc.now)
			if !ok || !got.Equal(tc.want) || got.Location() != time.UTC {
				t.Fatalf("next anniversary of %s after %s = %s (%v), want %s", tc.start, tc.now, got, ok, tc.want)
			}
		})
	}

	// A subscription that has not begun has no renewal to estimate.
	now := utcAt(2026, time.September, 10, 4, 0, 0)
	for name, start := range map[string]time.Time{
		"a second ahead": now.Add(time.Second),
		"hours ahead":    now.Add(16 * time.Hour),
		"years ahead":    utcAt(2030, time.January, 1, 0, 0, 0),
	} {
		if got, ok := NextAnniversary(start, monthly, now); ok {
			t.Errorf("%s: estimated %s from a start still to come", name, got)
		}
	}
}

// claudeStarted is a Claude snapshot entry that has read a subscription start
// and, unless period is "", a billing period.
func claudeStarted(start time.Time, period string) *Entry {
	entry := &Entry{Provider: "claude", AccountDetails: &AccountDetails{CheckedAt: start, SubscriptionStartedAt: &start}}
	if period != "" {
		entry.Quota = &Quota{Schema: 1, BillingPeriod: period}
	}
	return entry
}

func ptrTime(t time.Time) *time.Time { return &t }

// renewalAt is SubscriptionRenewal as an epoch, for the comparisons below.
func renewalAt(provider string, entry *Entry, date string, now time.Time) (*int64, string) {
	at, source := SubscriptionRenewal(provider, entry, date, now)
	if at == nil {
		return nil, source
	}
	epoch := at.Unix()
	return &epoch, source
}

// Anthropic reports no renewal date, so a Claude account's is the next
// anniversary of its start — monthly unless the plan is annual — and the
// source says it is an estimate.
func TestAClaudeRenewalIsEstimatedFromTheSubscriptionStart(t *testing.T) {
	start := utcAt(2025, time.January, 31, 9, 15, 0)
	monthly, annual := utcAt(2026, time.September, 30, 9, 15, 0), utcAt(2027, time.January, 31, 9, 15, 0)
	for name, one := range map[string]struct {
		entry *Entry
		want  time.Time
	}{
		"monthly":                {claudeStarted(start, BillingMonthly), monthly},
		"annual":                 {claudeStarted(start, BillingAnnual), annual},
		"no billing period read": {claudeStarted(start, ""), monthly},
		// quota-cache never stores another value, but a snapshot is a file:
		// whatever it says that is not "annual" is the common case.
		"a period this build does not know": {claudeStarted(start, "fortnightly"), monthly},
		"unknown":                           {claudeStarted(start, "unknown"), monthly},
	} {
		got, source := renewalAt("claude", one.entry, "", renewalNow)
		if got == nil || *got != one.want.Unix() || source != RenewalEstimated {
			t.Errorf("%s: renewal = %v source = %q, want %s estimated", name, got, source, one.want)
		}
	}
}

// The caller passes now, and the estimate is the same instant whatever zone
// that now is written in. One that passes a host-local time.Now() in Sydney
// must not see a renewal 30 minutes away reported a month later.
func TestAnEstimatedRenewalDoesNotDependOnNowsZone(t *testing.T) {
	instant := utcAt(2026, time.September, 30, 22, 30, 0)
	entry := claudeStarted(utcAt(2026, time.January, 30, 23, 0, 0), BillingMonthly)
	want := utcAt(2026, time.September, 30, 23, 0, 0).Unix()
	for _, zone := range []*time.Location{time.UTC, time.FixedZone("AEST", 10*3600), time.FixedZone("EDT", -4*3600)} {
		got, source := renewalAt("claude", entry, "", instant.In(zone))
		if got == nil || *got != want || source != RenewalEstimated {
			t.Errorf("now in %s: renewal = %v source = %q, want %s estimated", zone, got, source, time.Unix(want, 0).UTC())
		}
	}
}

// A renewal the provider reports always wins, and is never marked estimated:
// a guess laid over Codex's own date would only be a worse copy of it.
func TestARealRenewalWinsAndIsNeverEstimated(t *testing.T) {
	reported := renewalNow.Add(19 * 24 * time.Hour)
	start := utcAt(2025, time.January, 31, 9, 15, 0)
	for _, provider := range []string{"codex", "claude"} {
		entry := claudeStarted(start, BillingMonthly)
		entry.RenewalAt = &reported
		got, source := renewalAt(provider, entry, "", renewalNow)
		if got == nil || *got != reported.Unix() || source != RenewalReported {
			t.Fatalf("%s: renewal = %v source = %q; want the reported %s", provider, got, source, reported)
		}
	}
	// And a Codex renewal with no start beside it is simply the renewal.
	if got, source := renewalAt("codex", &Entry{RenewalAt: &reported}, "", renewalNow); got == nil || source != RenewalReported {
		t.Fatalf("codex: renewal = %v source = %q", got, source)
	}
	// One already behind us says nothing about the future.
	past := renewalNow.Add(-time.Second)
	if got, source := renewalAt("codex", &Entry{RenewalAt: &past}, "", renewalNow); got != nil || source != "" {
		t.Fatalf("a reported renewal in the past: %v %q", got, source)
	}
}

// There is no renewal unless one is reported, set or can be estimated: no
// renewal at all, a start still to come, and a credential quota-cache has
// never read all give none.
func TestRenewalEstimatedIsFalseByDefault(t *testing.T) {
	for name, entry := range map[string]*Entry{
		"nothing read":           {},
		"details without start":  {AccountDetails: &AccountDetails{CheckedAt: renewalNow, Plan: "max"}},
		"a period without start": {Quota: &Quota{Schema: 1, BillingPeriod: BillingAnnual}},
		"a start still to come":  claudeStarted(renewalNow.Add(16*time.Hour), BillingMonthly),
		"no snapshot entry":      nil,
	} {
		if got, source := renewalAt("claude", entry, "", renewalNow); got != nil || source != "" {
			t.Errorf("%s: renewal = %v source = %q, want none", name, got, source)
		}
	}
}

// The committed fixtures carry the estimate in each shape a page renders: a
// month-end start clamped to a 30-day month, an annual plan, a period never
// read taken as monthly, an annual Feb 29 start in a common year, a start
// still to come, and Codex's own date beside them, not estimated.
func TestTheGoldenContractsCarryEstimatedRenewals(t *testing.T) {
	entries := map[string]Entry{}
	for _, name := range []string{"seven-credentials", "degraded-states"} {
		snapshot, err := Load(filepath.Join("..", "internal", "credits", "testdata", "snapshots", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range snapshot.Entries {
			entries[entry.AuthIndex] = entry
		}
	}
	renewal := func(id, date string) (*int64, string) {
		entry, ok := entries[id]
		if !ok {
			t.Fatalf("no fixture entry %s", id)
		}
		return renewalAt(entry.Provider, &entry, date, renewalNow)
	}
	for id, want := range map[string]*time.Time{
		"claude-sierra@example.com.json":  ptrTime(utcAt(2026, time.September, 30, 9, 15, 0)),
		"claude-agency@example.com.json":  ptrTime(utcAt(2027, time.March, 12, 17, 40, 0)),
		"claude-charlie@example.com.json": ptrTime(utcAt(2026, time.September, 22, 14, 5, 0)),
		"claude-stale@example.com.json":   ptrTime(utcAt(2027, time.February, 28, 8, 0, 0)),
		"claude-fresh@example.com.json":   nil,
		"claude-noor@example.com.json":    nil,
	} {
		got, source := renewal(id, "")
		switch {
		case want == nil && got != nil:
			t.Errorf("%s: renewal = %v source = %q, want none", id, got, source)
		case want != nil && (got == nil || *got != want.Unix() || source != RenewalEstimated):
			t.Errorf("%s: renewal = %v source = %q, want %s estimated", id, got, source, want)
		}
	}
	if got, source := renewal("codex-noor@example.com.json", ""); got == nil || source != RenewalReported {
		t.Errorf("codex: renewal = %v source = %q; want its own date", got, source)
	}
	// A date the operator set wins, and its estimate rides beside it for the
	// editor's "entered here · estimate ~Oct 3 · Use estimate".
	entry := entries["5f2b8c41d09e7a36"]
	estimate := SubscriptionRenewalEstimate("claude", &entry, renewalNow)
	if _, source := renewal("5f2b8c41d09e7a36", "2026-09-10"); source != RenewalDashboard ||
		estimate == nil || !estimate.Equal(utcAt(2026, time.October, 3, 11, 20, 0)) {
		t.Errorf("dashboard: source = %q estimate = %v", source, estimate)
	}
}

// A renewal the provider reports wins over the operator's, and the
// operator's over the estimate; each says which it is.
func TestRenewalPrecedence(t *testing.T) {
	reported := renewalNow.Add(19 * 24 * time.Hour)
	started := claudeStarted(utcAt(2025, time.January, 31, 9, 15, 0), BillingMonthly)
	withReported := claudeStarted(utcAt(2025, time.January, 31, 9, 15, 0), BillingMonthly)
	withReported.RenewalAt = &reported
	for name, tc := range map[string]struct {
		entry  *Entry
		date   string
		want   time.Time
		source string
	}{
		"reported over dashboard":  {withReported, "2026-10-01", reported, RenewalReported},
		"dashboard over estimated": {started, "2026-10-01", utcAt(2026, time.October, 1, 0, 0, 0), RenewalDashboard},
		"estimated without either": {started, "", utcAt(2026, time.September, 30, 9, 15, 0), RenewalEstimated},
		"an unreadable date":       {started, "2026-13-01", utcAt(2026, time.September, 30, 9, 15, 0), RenewalEstimated},
	} {
		got, source := renewalAt("claude", tc.entry, tc.date, renewalNow)
		if got == nil || *got != tc.want.Unix() || source != tc.source {
			t.Errorf("%s: renewal %v source %q; want %s from %s", name, got, source, tc.want, tc.source)
		}
	}
	if got, source := renewalAt("claude", &Entry{}, "", renewalNow); got != nil || source != "" {
		t.Errorf("nothing at all: %v %q", got, source)
	}
}

// A Claude credential has its estimate whatever wins, so a page can say what
// Use estimate returns to beside a date the operator set. Codex has no
// estimate to return to.
func TestTheEstimateIsCarriedBesideADashboardDate(t *testing.T) {
	started := claudeStarted(utcAt(2025, time.January, 31, 9, 15, 0), BillingMonthly)
	estimate := utcAt(2026, time.September, 30, 9, 15, 0)
	for name, tc := range map[string]struct {
		date   string
		source string
	}{
		"set on the dashboard": {"2026-10-01", RenewalDashboard},
		"estimated":            {"", RenewalEstimated},
	} {
		_, source := SubscriptionRenewal("claude", started, tc.date, renewalNow)
		got := SubscriptionRenewalEstimate("claude", started, renewalNow)
		if source != tc.source || got == nil || !got.Equal(estimate) {
			t.Errorf("%s: source %q estimate %v, want %s", name, source, got, estimate)
		}
	}
	if got := SubscriptionRenewalEstimate("claude", &Entry{}, renewalNow); got != nil {
		t.Errorf("no subscription start: estimate %v", *got)
	}
	if got := SubscriptionRenewalEstimate("claude", nil, renewalNow); got != nil {
		t.Errorf("no snapshot entry: estimate %v", *got)
	}
	if got := SubscriptionRenewalEstimate("codex", started, renewalNow); got != nil {
		t.Errorf("codex: estimate %v", *got)
	}
}

// A date the operator set is 00:00 UTC of that day while it is ahead, and
// once it has come, the next one on the same day of the month, or of the year
// on an annual plan, strictly after now: on the day itself, next month's.
func TestADashboardRenewalIsNeverInThePast(t *testing.T) {
	annual := &Entry{Quota: &Quota{Schema: 1, BillingPeriod: BillingAnnual}}
	for name, tc := range map[string]struct {
		entry *Entry
		date  string
		want  time.Time
	}{
		"ahead":                  {&Entry{}, "2026-09-29", utcAt(2026, time.September, 29, 0, 0, 0)},
		"past, monthly":          {&Entry{}, "2026-08-31", utcAt(2026, time.September, 30, 0, 0, 0)},
		"past, yearly":           {annual, "2025-11-02", utcAt(2026, time.November, 2, 0, 0, 0)},
		"today, so next month":   {&Entry{}, "2026-09-10", utcAt(2026, time.October, 10, 0, 0, 0)},
		"today on a yearly plan": {annual, "2025-09-10", utcAt(2027, time.September, 10, 0, 0, 0)},
		"no snapshot entry":      {nil, "2026-08-31", utcAt(2026, time.September, 30, 0, 0, 0)},
	} {
		got, source := renewalAt("claude", tc.entry, tc.date, renewalNow)
		if got == nil || *got != tc.want.Unix() || *got <= renewalNow.Unix() || source != RenewalDashboard {
			t.Errorf("%s: renewal %v source %q, want %s", name, got, source, tc.want)
		}
	}
}

// Codex reports its own renewal and never takes one the operator set.
func TestCodexIgnoresADashboardRenewal(t *testing.T) {
	if got, source := renewalAt("codex", &Entry{}, "2026-09-29", renewalNow); got != nil || source != "" {
		t.Errorf("codex: renewal %v source %q", got, source)
	}
}

// Only a CPA auth index as CPA forms one today can key a renewal date, which
// is what makes a Claude credential's date editable.
func TestValidRenewalID(t *testing.T) {
	for id, want := range map[string]bool{
		"0123456789abcdef":          true,
		"5f2b8c41d09e7a36":          true,
		"0123456789ABCDEF":          false,
		"0123456789abcde":           false,
		"0123456789abcdef0":         false,
		"claude-a@example.com.json": false,
		"":                          false,
	} {
		if got := ValidRenewalID(id); got != want {
			t.Errorf("ValidRenewalID(%q) = %v", id, got)
		}
	}
}

// A renewal date is saved as YYYY-MM-DD only, a real date in 2000 to 2099.
func TestParseRenewalDate(t *testing.T) {
	if day, ok := ParseRenewalDate("2026-10-29"); !ok || !day.Equal(utcAt(2026, time.October, 29, 0, 0, 0)) || day.Location() != time.UTC {
		t.Errorf("2026-10-29: %s %v", day, ok)
	}
	for _, bad := range []string{"", "2026-10-29T00:00:00Z", "2026-02-30", "2026-1-5", " 2026-10-29", "1999-12-31", "2100-01-01", "29/10/2026"} {
		if _, ok := ParseRenewalDate(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
	for _, edge := range []string{"2000-01-01", "2099-12-31", "2028-02-29"} {
		if _, ok := ParseRenewalDate(edge); !ok {
			t.Errorf("%q refused", edge)
		}
	}
}
