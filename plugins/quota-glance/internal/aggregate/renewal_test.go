package aggregate

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// The renewal math itself, qc.SubscriptionRenewal and qc.NextAnniversary, is
// Quota Cache's client's and tested there. These test the document: that each
// credential carries what that math says, from the dates Quota Cache
// publishes.

func utc(year int, month time.Month, day, hour, minute, second int) time.Time {
	return time.Date(year, month, day, hour, minute, second, 0, time.UTC)
}

// renewalState is a published credit state holding only renewal dates, as
// Quota Cache publishes one with no organization listed.
func renewalState(renewals map[string]RenewalSetting) *qc.CreditState {
	return &qc.CreditState{Schema: qc.CreditsSchema, Settings: qc.CreditSettingsState{State: qc.CreditSettingsOK, Editable: true},
		Renewals: renewals}
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
		"claude-sierra@example.com.json": ptrTime(utc(2026, time.September, 30, 9, 15, 0)),
		"claude-agency@example.com.json":        ptrTime(utc(2027, time.March, 12, 17, 40, 0)),
		"claude-charlie@example.com.json":    ptrTime(utc(2026, time.September, 22, 14, 5, 0)),
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
	// A date entered on Quota Cache's page wins, and its estimate rides beside
	// it.
	if c := byID["5f2b8c41d09e7a36"]; c.RenewalSource == nil || *c.RenewalSource != qc.RenewalDashboard || c.RenewalEstimated ||
		c.RenewalEstimateAtEpoch == nil || *c.RenewalEstimateAtEpoch != utc(2026, time.October, 3, 11, 20, 0).Unix() {
		t.Errorf("dashboard: source = %v estimated = %v estimate = %v", c.RenewalSource, c.RenewalEstimated, c.RenewalEstimateAtEpoch)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// withRenewal builds one credential's document with a renewal date Quota
// Cache publishes for it.
func withRenewal(t *testing.T, identity Identity, entry qc.Entry, date string) Credential {
	t.Helper()
	now := at(t, 0)
	entry.Provider, entry.AuthIndex = identity.Provider, identity.AuthIndex
	entry.ObservedAt = now.Add(-2 * time.Minute)
	renewals := map[string]RenewalSetting{}
	if date != "" {
		renewals[identity.AuthIndex] = RenewalSetting{Date: date, Revision: "6", UpdatedAtEpoch: now.Add(-time.Hour).Unix()}
	}
	doc := Build(Input{
		Snapshot:   qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{qc.Key(identity.Provider, identity.AuthIndex): entry}},
		Identities: []Identity{identity},
		Credits:    renewalState(renewals),
	}, now)
	return doc.Credentials[0]
}

// A renewal the provider reports wins over the one the operator entered, and
// that over the estimate; each says which it is.
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

// A Claude credential carries its estimate whatever wins, so the page can show
// it beside a date the operator entered. Codex has no estimate.
func TestTheEstimateIsCarriedBesideADashboardDate(t *testing.T) {
	claude := Identity{AuthIndex: "0123456789abcdef", Provider: "claude"}
	started := claudeStarted(utc(2025, time.January, 31, 9, 15, 0), qc.BillingMonthly)
	estimate := utc(2026, time.September, 30, 9, 15, 0).Unix()
	for name, tc := range map[string]struct {
		date   string
		source string
	}{
		"set on the dashboard": {"2026-10-01", "dashboard"},
		"estimated":            {"", "estimated"},
	} {
		got := withRenewal(t, claude, started, tc.date)
		if got.RenewalSource == nil || *got.RenewalSource != tc.source || got.RenewalEstimateAtEpoch == nil || *got.RenewalEstimateAtEpoch != estimate {
			t.Errorf("%s: source %v estimate %v, want %d", name, got.RenewalSource, got.RenewalEstimateAtEpoch, estimate)
		}
	}
	if got := withRenewal(t, claude, qc.Entry{}, "2026-10-01"); got.RenewalEstimateAtEpoch != nil {
		t.Errorf("no subscription start: estimate %v", *got.RenewalEstimateAtEpoch)
	}
	codex := Identity{AuthIndex: "0123456789abcdef", Provider: "codex"}
	if got := withRenewal(t, codex, started, ""); got.RenewalEstimateAtEpoch != nil {
		t.Errorf("codex: estimate %v", *got.RenewalEstimateAtEpoch)
	}
	raw, _ := json.Marshal(withRenewal(t, claude, qc.Entry{}, ""))
	if !strings.Contains(string(raw), `"renewalEstimateAtEpoch":null`) {
		t.Errorf("an absent estimate must be null: %s", raw)
	}
}

// A document served again while its source fails takes every part Quota
// Cache's published state decides from one built with the state as it is now,
// and keeps the rest as it was.
func TestWithCreditsTakesOnlyWhatTheCreditStateDecides(t *testing.T) {
	claude := Identity{AuthIndex: "0123456789abcdef", Provider: "claude"}
	started := claudeStarted(utc(2025, time.January, 31, 9, 15, 0), qc.BillingMonthly)
	before := withRenewal(t, claude, started, "")
	after := withRenewal(t, claude, started, "2026-10-01")
	old := Document{Credentials: []Credential{before}, RenewalOrphans: []RenewalOrphan{}, Providers: []Provider{{ID: "claude"}}}
	fresh := Document{Credentials: []Credential{after}, RenewalOrphans: []RenewalOrphan{{ID: "fedcba9876543210"}},
		APICredits: &APICredits{Title: "Monthly API Credit"}, Providers: []Provider{}}
	got := WithCredits(old, fresh)
	c := got.Credentials[0]
	if c.RenewalSource == nil || *c.RenewalSource != "dashboard" || c.RenewalSetting == nil || c.RenewalSetting.Date != "2026-10-01" ||
		*c.RenewalAtEpoch != *after.RenewalAtEpoch || c.RenewalEstimated || *c.RenewalEstimateAtEpoch != *after.RenewalEstimateAtEpoch {
		t.Errorf("credential: %+v", c)
	}
	if got.APICredits != fresh.APICredits || len(got.RenewalOrphans) != 1 || len(got.Providers) != 1 {
		t.Errorf("document: %+v", got)
	}
	if old.Credentials[0].RenewalSetting != nil {
		t.Error("the document served before was modified")
	}
}

// An entered date is 00:00 UTC of that day while it is ahead, and once it has
// come, the next one on the same day of the month, or of the year on an
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

// Codex reports its own renewal and never takes an entered one.
func TestCodexIgnoresADashboardRenewal(t *testing.T) {
	codex := Identity{AuthIndex: "0123456789abcdef", Provider: "codex"}
	got := withRenewal(t, codex, qc.Entry{}, "2026-09-29")
	if got.RenewalAtEpoch != nil || got.RenewalSource != nil || got.RenewalEditable {
		t.Errorf("codex: renewal %v source %v editable %v", got.RenewalAtEpoch, got.RenewalSource, got.RenewalEditable)
	}
}

// Renewal dates are entered on Quota Cache's page, so no credential's is
// editable here, whatever its provider, its auth index or the state of Quota
// Cache's settings.
func TestRenewalsAreNeverEditableHere(t *testing.T) {
	for name, state := range map[string]*qc.CreditState{
		"settings ok":         renewalState(map[string]RenewalSetting{}),
		"settings unreadable": {Schema: qc.CreditsSchema, Settings: qc.CreditSettingsState{State: qc.CreditSettingsUnreadable}},
		"no state":            nil,
	} {
		for _, identity := range []Identity{
			{AuthIndex: "0123456789abcdef", Provider: "claude"},
			{AuthIndex: "0123456789abcdef", Provider: "codex"},
			{AuthIndex: "claude-a@example.com.json", Provider: "claude"},
		} {
			doc := Build(Input{
				Snapshot:   qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{}},
				Identities: []Identity{identity},
				Credits:    state,
			}, at(t, 0))
			if doc.Credentials[0].RenewalEditable {
				t.Errorf("%s: %s %s is editable", name, identity.Provider, identity.AuthIndex)
			}
		}
	}
	for _, doc := range []Document{buildFixture(t), buildDegraded(t)} {
		for _, c := range doc.Credentials {
			if c.RenewalEditable {
				t.Errorf("golden credential %s is editable", c.ID)
			}
		}
	}
}

// A stored date for a credential the roster no longer lists is an orphan,
// oldest first; without a roster, none is, and without a usable state from
// Quota Cache no date is known at all.
func TestRenewalOrphans(t *testing.T) {
	now := at(t, 0)
	state := renewalState(map[string]RenewalSetting{
		"0123456789abcdef": {Date: "2026-10-29", Revision: "5", UpdatedAtEpoch: now.Add(-time.Hour).Unix()},
		"fedcba9876543210": {Date: "2026-09-29", Revision: "2", UpdatedAtEpoch: now.Add(-48 * time.Hour).Unix()},
		"1111111111111111": {Date: "2026-09-12", Revision: "3", UpdatedAtEpoch: now.Add(-24 * time.Hour).Unix()},
	})
	in := Input{
		Snapshot:   qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{}},
		Identities: []Identity{{AuthIndex: "1111111111111111", Provider: "claude"}},
		Credits:    state,
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
	in.SourceReason = ""
	for _, cause := range []string{CreditsMissing, CreditsUnreadable, CreditsAbandoned} {
		in.CreditsError = cause
		if got := Build(in, now).RenewalOrphans; got == nil || len(got) != 0 {
			t.Errorf("%s: %+v", cause, got)
		}
	}
}

// The committed contracts carry the dates Quota Cache publishes: the
// healthy one's dashboard date on its credential, and the degraded one's date
// for a credential its roster does not list as an orphan. Neither is
// editable, and a document whose source failed lists no orphan.
func TestRenewalsFromPublishedDates(t *testing.T) {
	healthy := buildFixture(t)
	for _, c := range healthy.Credentials {
		if c.ID != novemberID {
			if c.RenewalSetting != nil {
				t.Errorf("%s carries a setting it has none of: %+v", c.ID, c.RenewalSetting)
			}
			continue
		}
		if c.RenewalSetting == nil || *c.RenewalSetting != (RenewalSetting{Date: "2026-09-10", Revision: "3", UpdatedAtEpoch: 1788984000}) ||
			c.RenewalSource == nil || *c.RenewalSource != qc.RenewalDashboard || c.RenewalEditable ||
			c.RenewalAtEpoch == nil || *c.RenewalAtEpoch != utc(2026, time.October, 10, 0, 0, 0).Unix() {
			t.Errorf("%s: %+v", c.ID, c)
		}
	}
	if len(healthy.RenewalOrphans) != 0 {
		t.Errorf("healthy orphans: %+v", healthy.RenewalOrphans)
	}
	degraded := buildDegraded(t)
	if want := (RenewalOrphan{ID: "0123456789abcdef", Date: "2026-09-29", Revision: "9", UpdatedAtEpoch: 1788264000}); len(degraded.RenewalOrphans) != 1 ||
		degraded.RenewalOrphans[0] != want {
		t.Errorf("degraded orphans: %+v", degraded.RenewalOrphans)
	}
	failed := Build(Input{
		Snapshot: loadSnapshot(t, "degraded-states.json"), Identities: degradedRoster(), SourceReason: ReasonRosterUnavailable,
		StaleAfter: 45 * time.Minute, Credits: fixtureCredits(t, "degraded-states"),
	}, at(t, 0))
	if failed.RenewalOrphans == nil || len(failed.RenewalOrphans) != 0 {
		t.Errorf("while the source failed: %+v", failed.RenewalOrphans)
	}
}
