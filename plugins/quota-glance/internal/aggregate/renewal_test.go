package aggregate

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/overrides"
)

func utc(year int, month time.Month, day, hour, minute, second int) time.Time {
	return time.Date(year, month, day, hour, minute, second, 0, time.UTC)
}

// The anniversary rule card billing uses for a subscription's anchor: the
// start's day of the month and time of day in UTC, clamped to the last day of
// a shorter month, every anniversary counted from the start itself so a short
// month never drags the day down for good, and the first one strictly after
// now.
func TestNextAnniversary(t *testing.T) {
	const monthly, yearly = 1, 12
	jan31 := utc(2025, time.January, 31, 9, 15, 0)
	for _, tc := range []struct {
		name   string
		start  time.Time
		months int
		now    time.Time
		want   time.Time
	}{
		{"mid-month, later this month", utc(2025, time.March, 15, 10, 0, 0), monthly,
			utc(2026, time.September, 10, 4, 0, 0), utc(2026, time.September, 15, 10, 0, 0)},
		{"mid-month, already past this month", utc(2025, time.March, 5, 10, 0, 0), monthly,
			utc(2026, time.September, 10, 4, 0, 0), utc(2026, time.October, 5, 10, 0, 0)},
		{"today, earlier in the day", utc(2025, time.March, 10, 3, 0, 0), monthly,
			utc(2026, time.September, 10, 4, 0, 0), utc(2026, time.October, 10, 3, 0, 0)},
		{"today, later in the day", utc(2025, time.March, 10, 5, 0, 0), monthly,
			utc(2026, time.September, 10, 4, 0, 0), utc(2026, time.September, 10, 5, 0, 0)},

		// A start on the 31st, across February and the 30-day months.
		{"31st into a short February", jan31, monthly, utc(2026, time.February, 10, 0, 0, 0), utc(2026, time.February, 28, 9, 15, 0)},
		{"31st into a leap February", jan31, monthly, utc(2028, time.February, 10, 0, 0, 0), utc(2028, time.February, 29, 9, 15, 0)},
		{"31st back to March 31, no drift", jan31, monthly, utc(2026, time.March, 1, 0, 0, 0), utc(2026, time.March, 31, 9, 15, 0)},
		{"31st after the February one", jan31, monthly, utc(2026, time.February, 28, 10, 0, 0), utc(2026, time.March, 31, 9, 15, 0)},
		{"31st into April", jan31, monthly, utc(2026, time.April, 2, 0, 0, 0), utc(2026, time.April, 30, 9, 15, 0)},
		{"31st into September", jan31, monthly, utc(2026, time.September, 10, 4, 0, 0), utc(2026, time.September, 30, 9, 15, 0)},
		{"31st into December", jan31, monthly, utc(2026, time.December, 1, 0, 0, 0), utc(2026, time.December, 31, 9, 15, 0)},

		// The 30th and the 29th, the days February has only sometimes.
		{"30th into a short February", utc(2025, time.January, 30, 12, 0, 0), monthly,
			utc(2026, time.February, 1, 0, 0, 0), utc(2026, time.February, 28, 12, 0, 0)},
		{"30th into a leap February", utc(2025, time.January, 30, 12, 0, 0), monthly,
			utc(2028, time.February, 1, 0, 0, 0), utc(2028, time.February, 29, 12, 0, 0)},
		{"30th back to March 30", utc(2025, time.January, 30, 12, 0, 0), monthly,
			utc(2026, time.March, 1, 0, 0, 0), utc(2026, time.March, 30, 12, 0, 0)},
		{"29th into a short February", utc(2025, time.January, 29, 6, 30, 0), monthly,
			utc(2026, time.February, 1, 0, 0, 0), utc(2026, time.February, 28, 6, 30, 0)},
		{"29th into a leap February", utc(2025, time.January, 29, 6, 30, 0), monthly,
			utc(2028, time.February, 1, 0, 0, 0), utc(2028, time.February, 29, 6, 30, 0)},
		{"29th back to March 29", utc(2025, time.January, 29, 6, 30, 0), monthly,
			utc(2026, time.March, 1, 0, 0, 0), utc(2026, time.March, 29, 6, 30, 0)},

		// Annual plans.
		{"annual, later this year", utc(2024, time.March, 12, 17, 40, 0), yearly,
			utc(2026, time.March, 1, 0, 0, 0), utc(2026, time.March, 12, 17, 40, 0)},
		{"annual, already past this year", utc(2024, time.March, 12, 17, 40, 0), yearly,
			utc(2026, time.September, 10, 4, 0, 0), utc(2027, time.March, 12, 17, 40, 0)},
		{"annual Feb 29 into a common year", utc(2024, time.February, 29, 8, 0, 0), yearly,
			utc(2026, time.September, 10, 4, 0, 0), utc(2027, time.February, 28, 8, 0, 0)},
		{"annual Feb 29 back to a leap year", utc(2024, time.February, 29, 8, 0, 0), yearly,
			utc(2027, time.March, 1, 0, 0, 0), utc(2028, time.February, 29, 8, 0, 0)},
		{"annual 31st of a 31-day month", utc(2023, time.October, 31, 0, 0, 0), yearly,
			utc(2026, time.September, 10, 4, 0, 0), utc(2026, time.October, 31, 0, 0, 0)},

		// At the anniversary's own second it is happening, not ahead.
		{"exactly on a monthly anniversary", utc(2025, time.March, 15, 10, 0, 0), monthly,
			utc(2026, time.September, 15, 10, 0, 0), utc(2026, time.October, 15, 10, 0, 0)},
		{"a second before it", utc(2025, time.March, 15, 10, 0, 0), monthly,
			utc(2026, time.September, 15, 9, 59, 59), utc(2026, time.September, 15, 10, 0, 0)},
		{"exactly on a clamped anniversary", jan31, monthly, utc(2026, time.February, 28, 9, 15, 0), utc(2026, time.March, 31, 9, 15, 0)},
		{"exactly on an annual anniversary", utc(2024, time.February, 29, 8, 0, 0), yearly,
			utc(2027, time.February, 28, 8, 0, 0), utc(2028, time.February, 29, 8, 0, 0)},
		{"exactly at the start", utc(2026, time.August, 31, 9, 0, 0), monthly,
			utc(2026, time.August, 31, 9, 0, 0), utc(2026, time.September, 30, 9, 0, 0)},

		// Years back, and across the turn of a year.
		{"started years ago, monthly", utc(2006, time.December, 31, 23, 59, 59), monthly,
			utc(2026, time.September, 10, 4, 0, 0), utc(2026, time.September, 30, 23, 59, 59)},
		{"started years ago, annual", utc(2006, time.December, 31, 23, 59, 59), yearly,
			utc(2026, time.September, 10, 4, 0, 0), utc(2026, time.December, 31, 23, 59, 59)},
		{"into the next year", utc(2025, time.November, 30, 0, 0, 0), monthly,
			utc(2025, time.December, 31, 0, 0, 0), utc(2026, time.January, 30, 0, 0, 0)},
		{"December into January", utc(2025, time.December, 31, 12, 0, 0), monthly,
			utc(2026, time.January, 5, 0, 0, 0), utc(2026, time.January, 31, 12, 0, 0)},

		// UTC is the calendar, whatever zone the start was written in: 23:30
		// on the 31st in New York is 04:30 on the 1st.
		{"a start written in another zone", time.Date(2025, time.January, 31, 23, 30, 0, 0, time.FixedZone("EST", -5*3600)), monthly,
			utc(2026, time.September, 10, 4, 0, 0), utc(2026, time.October, 1, 4, 30, 0)},
		// Whole seconds: a fraction must not put the anniversary a hair past
		// a now that is itself whole, to be printed as already passed.
		{"a fractional start", utc(2026, time.August, 10, 4, 0, 0).Add(500 * time.Millisecond), monthly,
			utc(2026, time.September, 10, 4, 0, 0), utc(2026, time.October, 10, 4, 0, 0)},

		// And whatever zone now is written in. 22:30 UTC on Sep 30 is already
		// Oct 1 in Sydney and Berlin; counted from those calendars the months
		// run one long and skip the anniversary 30 minutes away for the one a
		// month after it. Behind UTC, New York's Sep 30 is still the UTC one's.
		{"a now in a zone already in the next month", utc(2026, time.January, 30, 23, 0, 0), monthly,
			utc(2026, time.September, 30, 22, 30, 0).In(time.FixedZone("AEST", 10*3600)), utc(2026, time.September, 30, 23, 0, 0)},
		{"a month-end now an hour or two ahead", utc(2026, time.August, 31, 23, 45, 0), monthly,
			utc(2026, time.September, 30, 23, 30, 0).In(time.FixedZone("CEST", 2*3600)), utc(2026, time.September, 30, 23, 45, 0)},
		{"a now in a zone still in the last month", utc(2026, time.January, 30, 23, 0, 0), monthly,
			utc(2026, time.October, 1, 2, 0, 0).In(time.FixedZone("EDT", -4*3600)), utc(2026, time.October, 30, 23, 0, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := nextAnniversary(tc.start, tc.months, tc.now)
			if !ok || !got.Equal(tc.want) || got.Location() != time.UTC {
				t.Fatalf("next anniversary of %s after %s = %s (%v), want %s", tc.start, tc.now, got, ok, tc.want)
			}
		})
	}

	// A subscription that has not begun has no renewal to estimate.
	now := utc(2026, time.September, 10, 4, 0, 0)
	for name, start := range map[string]time.Time{
		"a second ahead": now.Add(time.Second),
		"hours ahead":    now.Add(16 * time.Hour),
		"years ahead":    utc(2030, time.January, 1, 0, 0, 0),
	} {
		if got, ok := nextAnniversary(start, monthly, now); ok {
			t.Errorf("%s: estimated %s from a start still to come", name, got)
		}
	}
}

// claudeStarted is a Claude snapshot entry that has read a subscription start
// and, unless period is "", a billing period.
func claudeStarted(start time.Time, period string) qc.Entry {
	entry := qc.Entry{AccountDetails: &qc.AccountDetails{CheckedAt: start, SubscriptionStartedAt: &start}}
	if period != "" {
		entry.Quota = &qc.Quota{Schema: 1, BillingPeriod: period}
	}
	return entry
}

// Anthropic reports no renewal date, so a Claude account's is the next
// anniversary of its start — monthly unless the plan is annual — and the
// document says it is an estimate.
func TestAClaudeRenewalIsEstimatedFromTheSubscriptionStart(t *testing.T) {
	claude := Identity{AuthIndex: "claude-a@example.com.json", Provider: "claude"}
	start := utc(2025, time.January, 31, 9, 15, 0)
	monthly, annual := utc(2026, time.September, 30, 9, 15, 0), utc(2027, time.January, 31, 9, 15, 0)
	for name, one := range map[string]struct {
		entry qc.Entry
		want  time.Time
	}{
		"monthly":                {claudeStarted(start, qc.BillingMonthly), monthly},
		"annual":                 {claudeStarted(start, qc.BillingAnnual), annual},
		"no billing period read": {claudeStarted(start, ""), monthly},
		// quota-cache never stores another value, but a snapshot is a file:
		// whatever it says that is not "annual" is the common case.
		"a period this build does not know": {claudeStarted(start, "fortnightly"), monthly},
		"unknown":                           {claudeStarted(start, "unknown"), monthly},
	} {
		got := oneCredential(t, claude, one.entry, true)
		if got.RenewalAtEpoch == nil || *got.RenewalAtEpoch != one.want.Unix() || !got.RenewalEstimated {
			t.Errorf("%s: renewal = %v estimated = %v, want %s estimated", name, got.RenewalAtEpoch, got.RenewalEstimated, one.want)
		}
	}
}

// Build takes now from its caller, and the estimate is the same instant
// whatever zone that now is written in. The shipped callers pass UTC; one that
// passes a host-local time.Now() in Sydney must not see a renewal 30 minutes
// away reported a month later.
func TestAnEstimatedRenewalDoesNotDependOnNowsZone(t *testing.T) {
	claude := Identity{AuthIndex: "claude-a@example.com.json", Provider: "claude"}
	instant := utc(2026, time.September, 30, 22, 30, 0)
	entry := claudeStarted(utc(2026, time.January, 30, 23, 0, 0), qc.BillingMonthly)
	entry.Provider, entry.AuthIndex, entry.ObservedAt = claude.Provider, claude.AuthIndex, instant.Add(-2*time.Minute)
	entry.Quota.ObservedAt = entry.ObservedAt
	want := utc(2026, time.September, 30, 23, 0, 0).Unix()
	for _, zone := range []*time.Location{time.UTC, time.FixedZone("AEST", 10*3600), time.FixedZone("EDT", -4*3600)} {
		doc := Build(Input{
			Snapshot: qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{
				qc.Key(claude.Provider, claude.AuthIndex): entry,
			}},
			Identities: []Identity{claude},
		}, instant.In(zone))
		got := doc.Credentials[0]
		if got.RenewalAtEpoch == nil || *got.RenewalAtEpoch != want || !got.RenewalEstimated {
			renewal := "none"
			if got.RenewalAtEpoch != nil {
				renewal = time.Unix(*got.RenewalAtEpoch, 0).UTC().String()
			}
			t.Errorf("now in %s: renewal = %s estimated = %v, want %s estimated", zone, renewal, got.RenewalEstimated, time.Unix(want, 0).UTC())
		}
	}
}

// A renewal the provider reports always wins, and is never marked estimated:
// a guess laid over Codex's own date would only be a worse copy of it.
func TestARealRenewalWinsAndIsNeverEstimated(t *testing.T) {
	now := at(t, 0)
	reported := now.Add(19 * 24 * time.Hour)
	start := utc(2025, time.January, 31, 9, 15, 0)
	for _, provider := range []string{"codex", "claude"} {
		entry := claudeStarted(start, qc.BillingMonthly)
		entry.RenewalAt = &reported
		got := oneCredential(t, Identity{AuthIndex: provider + "-a@example.com.json", Provider: provider}, entry, true)
		if got.RenewalAtEpoch == nil || *got.RenewalAtEpoch != reported.Unix() || got.RenewalEstimated {
			t.Fatalf("%s: renewal = %v estimated = %v; want the reported %s, not estimated", provider, got.RenewalAtEpoch, got.RenewalEstimated, reported)
		}
	}
	// And a Codex renewal with no start beside it is simply the renewal.
	got := oneCredential(t, Identity{AuthIndex: "codex-a@example.com.json", Provider: "codex"}, qc.Entry{RenewalAt: &reported}, true)
	if got.RenewalAtEpoch == nil || got.RenewalEstimated {
		t.Fatalf("codex: renewal = %v estimated = %v", got.RenewalAtEpoch, got.RenewalEstimated)
	}
}

// renewalEstimated is false unless the renewal is an estimate: no renewal at
// all, a start still to come, and a credential quota-cache has never read are
// all false, with no date.
func TestRenewalEstimatedIsFalseByDefault(t *testing.T) {
	now := at(t, 0)
	claude := Identity{AuthIndex: "claude-a@example.com.json", Provider: "claude"}
	for name, entry := range map[string]qc.Entry{
		"nothing read":           {},
		"details without start":  {AccountDetails: &qc.AccountDetails{CheckedAt: now, Plan: "max"}},
		"a period without start": {Quota: &qc.Quota{Schema: 1, BillingPeriod: qc.BillingAnnual}},
		"a start still to come":  claudeStarted(now.Add(16*time.Hour), qc.BillingMonthly),
	} {
		got := oneCredential(t, claude, entry, true)
		if got.RenewalAtEpoch != nil || got.RenewalEstimated {
			t.Errorf("%s: renewal = %v estimated = %v, want none", name, got.RenewalAtEpoch, got.RenewalEstimated)
		}
	}
	doc := Build(Input{
		Snapshot:   qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{}},
		Identities: []Identity{claude},
	}, now)
	if c := doc.Credentials[0]; c.RenewalAtEpoch != nil || c.RenewalEstimated {
		t.Fatalf("an unpolled credential carries renewal %v estimated %v", c.RenewalAtEpoch, c.RenewalEstimated)
	}
	// Always on the wire, so a client never has to tell absent from false.
	raw, _ := json.Marshal(doc.Credentials[0])
	if !strings.Contains(string(raw), `"renewalEstimated":false`) {
		t.Fatalf("renewalEstimated missing from %s", raw)
	}
}

// The committed contracts carry the estimate in each shape the page renders:
// a month-end start clamped to a 30-day month, an annual plan, a period never
// read taken as monthly, an annual Feb 29 start in a common year, a start
// still to come, and Codex's own date beside them, not estimated.
func TestTheGoldenContractsCarryEstimatedRenewals(t *testing.T) {
	byID := map[string]Credential{}
	for _, doc := range []Document{buildFixture(t), buildDegraded(t)} {
		for _, c := range doc.Credentials {
			byID[c.ID] = c
		}
	}
	for id, want := range map[string]*time.Time{
		"claude-siphorchannel@example.com.json": ptrTime(utc(2026, time.September, 30, 9, 15, 0)),
		"claude-agency@example.com.json":        ptrTime(utc(2027, time.March, 12, 17, 40, 0)),
		"claude-chasibnoor@example.com.json":    ptrTime(utc(2026, time.September, 22, 14, 5, 0)),
		"claude-stale@example.com.json":         ptrTime(utc(2027, time.February, 28, 8, 0, 0)),
		"claude-fresh@example.com.json":         nil,
		"claude-noor@example.com.json":          nil,
	} {
		c := byID[id]
		switch {
		case want == nil && (c.RenewalAtEpoch != nil || c.RenewalEstimated):
			t.Errorf("%s: renewal = %v estimated = %v, want none", id, c.RenewalAtEpoch, c.RenewalEstimated)
		case want != nil && (c.RenewalAtEpoch == nil || *c.RenewalAtEpoch != want.Unix() || !c.RenewalEstimated):
			t.Errorf("%s: renewal = %v estimated = %v, want %s estimated", id, c.RenewalAtEpoch, c.RenewalEstimated, want)
		}
	}
	if c := byID["codex-noor@example.com.json"]; c.RenewalAtEpoch == nil || c.RenewalEstimated {
		t.Errorf("codex: renewal = %v estimated = %v; want its own date, not estimated", c.RenewalAtEpoch, c.RenewalEstimated)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// withRenewal builds one credential's document with a renewal date stored for
// it on the dashboard, editing on.
func withRenewal(t *testing.T, identity Identity, entry qc.Entry, date string) Credential {
	t.Helper()
	now := at(t, 0)
	entry.Provider, entry.AuthIndex = identity.Provider, identity.AuthIndex
	entry.ObservedAt = now.Add(-2 * time.Minute)
	values := overrides.Values{Renewals: map[string]overrides.Renewal{}}
	if date != "" {
		values.Renewals[identity.AuthIndex] = overrides.Renewal{Date: date, Rev: 6, UpdatedAt: now.Add(-time.Hour)}
	}
	doc := Build(Input{
		Snapshot:   qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{qc.Key(identity.Provider, identity.AuthIndex): entry}},
		Identities: []Identity{identity},
		Overrides:  values,
		AllowEdit:  true,
	}, now)
	return doc.Credentials[0]
}

// A renewal the provider reports wins over the dashboard's, and the
// dashboard's over the estimate; each says which it is.
func TestRenewalPrecedence(t *testing.T) {
	claude := Identity{AuthIndex: "0123456789abcdef", Provider: "claude"}
	now := at(t, 0) // 2026-09-10T04:00:00Z
	reported := now.Add(19 * 24 * time.Hour)
	started := claudeStarted(utc(2025, time.January, 31, 9, 15, 0), qc.BillingMonthly)
	withReported := claudeStarted(utc(2025, time.January, 31, 9, 15, 0), qc.BillingMonthly)
	withReported.RenewalAt = &reported
	for name, tc := range map[string]struct {
		entry  qc.Entry
		date   string
		want   time.Time
		source string
	}{
		"reported over dashboard":  {withReported, "2026-10-01", reported, "reported"},
		"dashboard over estimated": {started, "2026-10-01", utc(2026, time.October, 1, 0, 0, 0), "dashboard"},
		"estimated without either": {started, "", utc(2026, time.September, 30, 9, 15, 0), "estimated"},
	} {
		got := withRenewal(t, claude, tc.entry, tc.date)
		if got.RenewalAtEpoch == nil || *got.RenewalAtEpoch != tc.want.Unix() || got.RenewalSource == nil || *got.RenewalSource != tc.source ||
			got.RenewalEstimated != (tc.source == "estimated") {
			t.Errorf("%s: renewal %v source %v estimated %v; want %s from %s", name, got.RenewalAtEpoch, got.RenewalSource, got.RenewalEstimated, tc.want, tc.source)
		}
	}
	if got := withRenewal(t, claude, qc.Entry{}, ""); got.RenewalSource != nil || got.RenewalSetting != nil {
		t.Errorf("nothing at all: source %v setting %v", got.RenewalSource, got.RenewalSetting)
	}
	raw, _ := json.Marshal(withRenewal(t, claude, qc.Entry{}, ""))
	if !strings.Contains(string(raw), `"renewalSource":null`) || !strings.Contains(string(raw), `"renewalSetting":null`) {
		t.Errorf("absent renewal fields must be null: %s", raw)
	}
}

// A dashboard date is 00:00 UTC of that day while it is ahead, and once it
// has come, the next one on the same day of the month, or of the year on an
// annual plan, strictly after now: on the day itself, next month's.
func TestADashboardRenewalIsNeverInThePast(t *testing.T) {
	claude := Identity{AuthIndex: "0123456789abcdef", Provider: "claude"}
	now := at(t, 0) // 2026-09-10T04:00:00Z
	annual := qc.Entry{Quota: &qc.Quota{Schema: 1, BillingPeriod: qc.BillingAnnual}}
	for name, tc := range map[string]struct {
		entry qc.Entry
		date  string
		want  time.Time
	}{
		"ahead":                  {qc.Entry{}, "2026-09-29", utc(2026, time.September, 29, 0, 0, 0)},
		"past, monthly":          {qc.Entry{}, "2026-08-31", utc(2026, time.September, 30, 0, 0, 0)},
		"past, yearly":           {annual, "2025-11-02", utc(2026, time.November, 2, 0, 0, 0)},
		"today, so next month":   {qc.Entry{}, "2026-09-10", utc(2026, time.October, 10, 0, 0, 0)},
		"today on a yearly plan": {annual, "2025-09-10", utc(2027, time.September, 10, 0, 0, 0)},
	} {
		got := withRenewal(t, claude, tc.entry, tc.date)
		if got.RenewalAtEpoch == nil || *got.RenewalAtEpoch != tc.want.Unix() || *got.RenewalAtEpoch <= now.Unix() {
			t.Errorf("%s: renewal %v, want %s", name, got.RenewalAtEpoch, tc.want)
		}
		if got.RenewalSetting == nil || got.RenewalSetting.Date != tc.date || got.RenewalSetting.Revision != "6" {
			t.Errorf("%s: setting %+v", name, got.RenewalSetting)
		}
	}
}

// Codex reports its own renewal and never takes one from the dashboard, nor
// offers to.
func TestCodexIgnoresADashboardRenewal(t *testing.T) {
	codex := Identity{AuthIndex: "0123456789abcdef", Provider: "codex"}
	got := withRenewal(t, codex, qc.Entry{}, "2026-09-29")
	if got.RenewalAtEpoch != nil || got.RenewalSource != nil || got.RenewalEditable {
		t.Errorf("codex: renewal %v source %v editable %v", got.RenewalAtEpoch, got.RenewalSource, got.RenewalEditable)
	}
}

// Only a Claude credential under an auth index settings.json can key, while
// editing is available, is offered for editing.
func TestRenewalEditable(t *testing.T) {
	for name, tc := range map[string]struct {
		identity Identity
		allow    bool
		want     bool
	}{
		"claude":                   {Identity{AuthIndex: "0123456789abcdef", Provider: "claude"}, true, true},
		"editing off":              {Identity{AuthIndex: "0123456789abcdef", Provider: "claude"}, false, false},
		"codex":                    {Identity{AuthIndex: "0123456789abcdef", Provider: "codex"}, true, false},
		"an index that cannot key": {Identity{AuthIndex: "claude-a@example.com.json", Provider: "claude"}, true, false},
	} {
		doc := Build(Input{
			Snapshot:   qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{}},
			Identities: []Identity{tc.identity},
			AllowEdit:  tc.allow,
		}, at(t, 0))
		if got := doc.Credentials[0].RenewalEditable; got != tc.want {
			t.Errorf("%s: renewalEditable = %v", name, got)
		}
	}
	// Unreadable settings apply nothing and edit nothing.
	doc := Build(Input{
		Snapshot:   qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{}},
		Identities: []Identity{{AuthIndex: "0123456789abcdef", Provider: "claude"}},
		Overrides:  overrides.Values{Unreadable: true},
		AllowEdit:  true,
	}, at(t, 0))
	if doc.Credentials[0].RenewalEditable {
		t.Error("an unreadable settings.json left renewals editable")
	}
}

// A stored date for a credential the roster no longer lists is offered for
// removal, oldest first; without a roster, none is.
func TestRenewalOrphans(t *testing.T) {
	now := at(t, 0)
	values := overrides.Values{Renewals: map[string]overrides.Renewal{
		"0123456789abcdef": {Date: "2026-10-29", Rev: 5, UpdatedAt: now.Add(-time.Hour)},
		"fedcba9876543210": {Date: "2026-09-29", Rev: 2, UpdatedAt: now.Add(-48 * time.Hour)},
		"1111111111111111": {Date: "2026-09-12", Rev: 3, UpdatedAt: now.Add(-24 * time.Hour)},
	}}
	in := Input{
		Snapshot:   qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{}},
		Identities: []Identity{{AuthIndex: "1111111111111111", Provider: "claude"}},
		Overrides:  values,
		AllowEdit:  true,
	}
	got := Build(in, now).RenewalOrphans
	want := []RenewalOrphan{
		{ID: "fedcba9876543210", Date: "2026-09-29", Revision: "2", UpdatedAtEpoch: now.Add(-48 * time.Hour).Unix()},
		{ID: "0123456789abcdef", Date: "2026-10-29", Revision: "5", UpdatedAtEpoch: now.Add(-time.Hour).Unix()},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("orphans = %+v", got)
	}
	in.SourceReason = ReasonRosterUnavailable
	if got := Build(in, now).RenewalOrphans; got == nil || len(got) != 0 {
		t.Errorf("without a roster: %+v", got)
	}
}
