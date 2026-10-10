package aggregate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/overrides"
)

// fakeOrg is one of the spec's fake organization ids, ...0a to ...0e.
func fakeOrg(last string) string { return "00000000-0000-4000-8000-00000000000" + last }

// creditEntry is quota-cache 0.1.14's entry for one configured organization:
// the configured facts and nothing else. It is never polled.
func creditEntry(label string, position int, org, monthly, renews string) qc.Entry {
	return qc.Entry{
		Provider:  qc.ProviderAnthropicAPI,
		AuthIndex: qc.APICreditOrgAccount(org),
		APICredit: &qc.APICredit{Label: label, Position: position, MonthlyUSD: monthly, Renews: renews, OrganizationID: org},
	}
}

func instant(t *testing.T, text string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func ptrAt(t *testing.T, text string) *time.Time {
	at := instant(t, text)
	return &at
}

// day and hour are meter buckets.
func bucket(t *testing.T, start string, usage ...qc.MeterUsage) qc.MeterBucket {
	t.Helper()
	sorted := []qc.MeterUsage{}
	for _, u := range usage {
		sorted = qc.MergeMeterUsage(sorted, u)
	}
	return qc.MeterBucket{Start: instant(t, start), Usage: sorted}
}

// meterOf is a meter flushed at flushed, counting the organizations given.
func meterOf(flushed time.Time, orgs map[string]qc.MeterOrganization) *qc.APIMeter {
	for id, org := range orgs {
		if org.Days == nil {
			org.Days = []qc.MeterBucket{}
		}
		if org.Hours == nil {
			org.Hours = []qc.MeterBucket{}
		}
		orgs[id] = org
	}
	return &qc.APIMeter{
		Schema: qc.MeterSchema, Since: flushed.Add(-60 * 24 * time.Hour), StartedAt: flushed.Add(-24 * time.Hour), FlushedAt: flushed,
		Gaps: []qc.MeterGap{}, Organizations: orgs, Unlinked: []qc.MeterUnlinked{}, Auths: map[string]qc.MeterAuth{},
	}
}

func snapshotOf(entries ...qc.Entry) qc.Snapshot {
	snapshot := qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{}}
	for _, entry := range entries {
		snapshot.Entries[qc.Key(entry.Provider, entry.AuthIndex)] = entry
	}
	return snapshot
}

// buildCredits builds the card for in at now, and holds it to the sums the
// contract promises.
func buildCredits(t *testing.T, in Input, now time.Time) *APICredits {
	t.Helper()
	doc := Build(in, now)
	if doc.APICredits == nil {
		t.Fatal("apiCredits is null with anthropic-api entries in the snapshot")
	}
	checkCreditPoolAddsUp(t, doc.APICredits)
	return doc.APICredits
}

// checkCreditPoolAddsUp holds every document to the invariants the contract
// promises, so a fixture cannot drift from them.
func checkCreditPoolAddsUp(t *testing.T, credits *APICredits) {
	t.Helper()
	pool := credits.Pool
	if pool.AccountCount != len(credits.Accounts) || pool.CountedCount+pool.MissingCount != pool.AccountCount {
		t.Errorf("counts do not add up: %+v over %d accounts", pool, len(credits.Accounts))
	}
	counted, lowerBound, estimate := 0, false, false
	for _, account := range credits.Accounts {
		wantCounted := account.State == StatusOK || account.State == StateStale || account.State == StateOut
		if account.Counted != wantCounted {
			t.Errorf("%s: state %s counted %v", account.ID, account.State, account.Counted)
		}
		if account.Counted {
			counted++
			lowerBound = lowerBound || account.LowerBound
			estimate = estimate || account.HasEstimate
			if !account.HasEstimate || account.MonthlyCreditText == "" {
				t.Errorf("%s is counted without an estimate: %+v", account.ID, account)
			}
		}
		switch account.State {
		case StateMisconfigured, StateCacheTooOld, StatusPending:
			if account.SpentText != "" || account.LeftText != "" || account.UsedText != "" || account.MonthlyCreditText != "" || account.Level != "" {
				t.Errorf("%s is %s but carries amounts: %+v", account.ID, account.State, account)
			}
		}
		if account.HasEstimate != (account.Basis != "") {
			t.Errorf("%s: hasEstimate %v with basis %q", account.ID, account.HasEstimate, account.Basis)
		}
		if account.DataIssues == nil || account.Unpriced == nil {
			t.Errorf("%s: arrays must never be null", account.ID)
		}
		if (len(account.DataIssues) == 0) != (account.Issue == "") {
			t.Errorf("%s: issues %v but issue %q", account.ID, account.DataIssues, account.Issue)
		}
		if account.RemainingFraction < 0 || account.RemainingFraction > 1 {
			t.Errorf("%s: fraction %v out of range", account.ID, account.RemainingFraction)
		}
	}
	if counted != pool.CountedCount || pool.HasEstimate != estimate || pool.LowerBound != lowerBound {
		t.Errorf("pool %+v disagrees with its accounts: %d counted, estimate %v, lower bound %v", pool, counted, estimate, lowerBound)
	}
	if credits.Unlinked == nil || credits.Orphans == nil || (credits.Meter != nil && credits.Meter.Gaps == nil) {
		t.Error("arrays must never be null")
	}
}

func creditAccountByLabel(t *testing.T, credits *APICredits, label string) APICreditAccount {
	t.Helper()
	for _, account := range credits.Accounts {
		if account.Label == label {
			return account
		}
	}
	t.Fatalf("no account %q in %+v", label, credits.Accounts)
	return APICreditAccount{}
}

func epoch(value int64) *int64 { return &value }

func jsonOf(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// workedInput is a D.7 fixture: the snapshot, the meter beside it, and the
// dashboard's settings.
func workedInput(t *testing.T, name string) Input {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "snapshots", name+".json")
	meter, err := qc.LoadMeter(qc.MeterPath(path))
	if err != nil {
		t.Fatalf("meter beside %s: %v", name, err)
	}
	values := overrides.Load(filepath.Join("..", "..", "testdata", "overrides", "api-credits.json"))
	if values.Unreadable {
		t.Fatal("testdata/overrides/api-credits.json does not load")
	}
	return Input{Snapshot: loadSnapshot(t, name+".json"), Meter: meter, Overrides: values, StaleAfter: 45 * time.Minute, AllowEdit: true}
}

const workedNow = "2026-10-09T12:00:00Z"

// The worked example in the spec and the contract, field by field.
func TestAPICreditsWorkedExample(t *testing.T) {
	now := instant(t, workedNow)
	if now.Unix() != 1791547200 {
		t.Fatalf("now = %d", now.Unix())
	}
	credits := buildCredits(t, workedInput(t, "api-credits"), now)

	if credits.Title != "Monthly API Credit" || credits.Currency != "USD" ||
		credits.Pricing != (APICreditPricing{AsOf: "2026-10-09", Source: "https://platform.claude.com/docs/en/about-claude/pricing", CacheWrites: "5m"}) ||
		credits.Editing != (APICreditEditing{Available: true}) {
		t.Errorf("card: %s", jsonOf(t, credits))
	}
	wantMeter := &APICreditMeter{
		SinceEpoch: instant(t, "2026-09-01T00:00:00Z").Unix(), UpdatedAtEpoch: instant(t, "2026-10-09T11:59:00Z").Unix(),
		Gaps: []APICreditGap{},
	}
	if !reflect.DeepEqual(credits.Meter, wantMeter) {
		t.Errorf("meter:\n got %s\nwant %s", jsonOf(t, credits.Meter), jsonOf(t, wantMeter))
	}

	alpha := APICreditAccount{
		ID: qc.APICreditOrgAccount(fakeOrg("a")), Label: "alpha", Order: 0, OrganizationID: fakeOrg("a"),
		State: StatusOK, Counted: true, HasEstimate: true, Basis: basisCredit,
		MonthlyCredit: 200, MonthlyCreditText: "$200.00", MonthlyCreditSource: sourceConfig,
		Spent: 55, SpentText: "$55.00", SpentSinceEpoch: epoch(instant(t, "2026-09-29T00:00:00Z").Unix()),
		Used: 55, UsedText: "$55.00", Left: 145, LeftText: "$145.00", Overage: 0, OverageText: "$0.00",
		RemainingFraction: 0.725, RemainingPercent: 73, Level: LevelOK,
		CycleStartEpoch: epoch(instant(t, "2026-09-29T00:00:00Z").Unix()), RenewsAtEpoch: epoch(1793232000), RenewsInSeconds: epoch(1684800),
		RenewsSource:    sourceConfig,
		CacheWriteExtra: 6, CacheWriteExtraText: "$6.00",
		Unpriced:        []APICreditUnpriced{},
		MeterSinceEpoch: epoch(instant(t, "2026-09-20T00:00:00Z").Unix()), LastSeenEpoch: epoch(instant(t, "2026-10-01T18:20:00Z").Unix()),
		DataIssues: []string{},
		Settings:   APICreditSettings{Editable: true, ConfigMonthlyUSD: "200", ConfigRenews: "2026-10-29"},
	}
	if got := credits.Accounts[0]; !reflect.DeepEqual(got, alpha) {
		t.Errorf("alpha:\n got %s\nwant %s", jsonOf(t, got), jsonOf(t, alpha))
	}

	team := APICreditAccount{
		ID: qc.APICreditOrgAccount(fakeOrg("b")), Label: "team", Order: 1, OrganizationID: fakeOrg("b"),
		State: StatusOK, Counted: true, HasEstimate: true, Basis: basisReading,
		MonthlyCredit: 300, MonthlyCreditText: "$300.00", MonthlyCreditSource: sourceDashboard,
		Spent: 140, SpentText: "$140.00", SpentSinceEpoch: epoch(instant(t, "2026-09-15T00:00:00Z").Unix()),
		Used: 194, UsedText: "$194.00", Left: 106, LeftText: "$106.00", Overage: 0, OverageText: "$0.00",
		RemainingFraction: 106.0 / 300, RemainingPercent: 35, Level: LevelLow,
		CycleStartEpoch: epoch(instant(t, "2026-09-15T00:00:00Z").Unix()), RenewsAtEpoch: epoch(1792022400), RenewsInSeconds: epoch(475200),
		RenewsSource: sourceConfig,
		Reading: &APICreditReading{
			Remaining: 120, RemainingText: "$120.00",
			AtEpoch: instant(t, "2026-10-08T15:40:00Z").Unix(), EnteredAtEpoch: instant(t, "2026-10-08T15:44:00Z").Unix(),
			SpentSince: 14, SpentSinceText: "$14.00",
		},
		Unpriced:        []APICreditUnpriced{},
		MeterSinceEpoch: epoch(instant(t, "2026-09-01T00:00:00Z").Unix()), LastSeenEpoch: epoch(instant(t, "2026-10-08T16:30:00Z").Unix()),
		DataIssues: []string{},
		Settings: APICreditSettings{
			Editable: true, Revision: "2", MonthlyUSD: "300", ConfigMonthlyUSD: "200", ConfigRenews: "2026-10-15",
			Reading:        &StoredReading{RemainingUSD: "120.00", AtEpoch: instant(t, "2026-10-08T15:40:00Z").Unix(), EnteredAtEpoch: instant(t, "2026-10-08T15:44:00Z").Unix()},
			UpdatedAtEpoch: epoch(instant(t, "2026-10-08T15:44:00Z").Unix()),
		},
	}
	if got := credits.Accounts[1]; !reflect.DeepEqual(got, team) {
		t.Errorf("team:\n got %s\nwant %s", jsonOf(t, got), jsonOf(t, team))
	}

	charlie := credits.Accounts[2]
	if charlie.Label != "charlie" || charlie.State != StateOut || !charlie.Counted || charlie.Basis != basisCredit ||
		charlie.Spent != 165 || charlie.Left != 0 || charlie.LeftText != "$0.00" || charlie.Used != 200 || charlie.UsedText != "$200.00" ||
		charlie.EstimateLeftText != "$35.00" || charlie.RemainingFraction != 0 || charlie.RemainingPercent != 0 || charlie.Level != LevelCritical ||
		!reflect.DeepEqual(charlie.DataIssues, []string{issueRefused}) ||
		charlie.Issue != "Anthropic refused a request for low credit, so this credit is spent. The estimate had $35.00 left; enter a Console reading to correct it." ||
		charlie.Refusals.Total != 3 || *charlie.Refusals.LastAtEpoch != instant(t, "2026-10-09T10:05:00Z").Unix() ||
		charlie.Refusals.ClaudeCodeTotal != 0 || charlie.Refusals.ClaudeCodeLastAtEpoch != nil {
		t.Errorf("charlie: %s", jsonOf(t, charlie))
	}

	delta := credits.Accounts[3]
	if delta.Label != "delta" || delta.State != StateNeedsSettings || delta.Counted || delta.HasEstimate || delta.Basis != "" ||
		delta.SpentText != "$50.45" || *delta.SpentSinceEpoch != instant(t, "2026-10-02T00:00:00Z").Unix() || !delta.LowerBound ||
		delta.LeftText != "" || delta.UsedText != "" || delta.MonthlyCreditText != "" || delta.MonthlyCreditSource != sourceNone ||
		!reflect.DeepEqual(delta.Unpriced, []APICreditUnpriced{{Model: "claude-mythos-preview", Tokens: 1000000}}) ||
		!reflect.DeepEqual(delta.DataIssues, []string{issueNeedsCredit, issueUnpricedModel}) ||
		delta.Issue != "Set this organization's monthly credit to count it in the total." {
		t.Errorf("delta: %s", jsonOf(t, delta))
	}

	echo := credits.Accounts[4]
	if echo.Label != "echo" || echo.State != StatusOK || !echo.Counted || echo.Basis != basisCredit || !echo.LowerBound ||
		echo.LeftText != "$200.00" || echo.UsedText != "$0.00" || echo.SpentText != "$0.00" || echo.RemainingPercent != 100 || echo.Level != LevelOK ||
		echo.LastSeenEpoch != nil ||
		!reflect.DeepEqual(echo.DataIssues, []string{issueMeterStartedLate, issueNoTraffic}) ||
		echo.Issue != "Counting began after this cycle started, so earlier spend is missing. Enter a Console reading to correct it." {
		t.Errorf("echo: %s", jsonOf(t, echo))
	}

	wantPool := APICreditPool{
		HasEstimate: true, LowerBound: true,
		MonthlyCredit: 900, MonthlyCreditText: "$900.00",
		Used: 449, UsedText: "$449.00", Left: 451, LeftText: "$451.00", Overage: 0, OverageText: "$0.00",
		RemainingFraction: 451.0 / 900, RemainingPercent: 50, Level: LevelOK,
		NextRefill: &APICreditRefill{
			AccountIDs:    []string{qc.APICreditOrgAccount(fakeOrg("b"))},
			RefillAtEpoch: 1792022400, RefillInSeconds: 475200,
			Gain: 194, GainText: "$194.00", GainFraction: 194.0 / 900, GainPercent: 22,
		},
		FullAtEpoch: epoch(1793232000), FullInSeconds: epoch(1684800),
		AccountCount: 5, CountedCount: 4, MissingCount: 1,
	}
	if !reflect.DeepEqual(credits.Pool, wantPool) {
		t.Errorf("pool:\n got %s\nwant %s", jsonOf(t, credits.Pool), jsonOf(t, wantPool))
	}
}

// The meter-gaps fixture: the worked example with a brief stop, which changes
// nothing, and a long one, which makes every counted account a lower bound
// with meterGap and moves no amount.
func TestAPICreditsMeterGaps(t *testing.T) {
	now := instant(t, workedNow)
	plain := buildCredits(t, workedInput(t, "api-credits"), now)
	gapped := buildCredits(t, workedInput(t, "meter-gaps"), now)

	if want := []APICreditGap{{
		FromEpoch: instant(t, "2026-10-09T06:00:00Z").Unix(), ToEpoch: instant(t, "2026-10-09T07:30:00Z").Unix(), Reason: qc.MeterStopDisabled,
	}}; !reflect.DeepEqual(gapped.Meter.Gaps, want) {
		t.Errorf("gaps = %+v; the brief one must be left out", gapped.Meter.Gaps)
	}
	for i, before := range plain.Accounts {
		after := gapped.Accounts[i]
		if after.Spent != before.Spent || after.Left != before.Left || after.Used != before.Used || after.State != before.State {
			t.Errorf("%s: a gap moved an amount: %+v then %+v", before.Label, before, after)
		}
		switch before.Label {
		case "alpha", "team", "charlie", "echo":
			if !after.LowerBound || !strings.Contains(strings.Join(after.DataIssues, ","), issueMeterGap) {
				t.Errorf("%s: lowerBound %v issues %v; want meterGap", before.Label, after.LowerBound, after.DataIssues)
			}
		}
	}
	if alpha := gapped.Accounts[0]; alpha.Issue != "Quota Cache was not counting for part of this period, for example while it was off or reloading, so some spend may be missing. Enter a Console reading to correct it." {
		t.Errorf("alpha's issue = %q", alpha.Issue)
	}
	if !gapped.Pool.LowerBound || gapped.Pool.Left != plain.Pool.Left || gapped.Pool.Used != plain.Pool.Used {
		t.Errorf("pool: %+v then %+v", plain.Pool, gapped.Pool)
	}
}

// One counted organization, metered and configured as given, for the rule
// tests below.
type creditCase struct {
	entry    qc.Entry
	org      qc.MeterOrganization
	meter    func(*qc.APIMeter)
	stored   *overrides.APICredit
	now      string
	stale    time.Duration
	noMeter  bool
	noOrg    bool
	editable bool
}

func (c creditCase) build(t *testing.T) APICreditAccount {
	t.Helper()
	now := instant(t, workedNow)
	if c.now != "" {
		now = instant(t, c.now)
	}
	in := Input{Snapshot: snapshotOf(c.entry), StaleAfter: c.stale, AllowEdit: true,
		Overrides: overrides.Values{APICredits: map[string]overrides.APICredit{}, Renewals: map[string]overrides.Renewal{}}}
	if c.stored != nil {
		in.Overrides.APICredits[c.entry.AuthIndex] = *c.stored
	}
	if !c.noMeter {
		orgs := map[string]qc.MeterOrganization{}
		if !c.noOrg && c.entry.APICredit != nil && c.entry.APICredit.OrganizationID != "" {
			orgs[c.entry.APICredit.OrganizationID] = c.org
		}
		in.Meter = meterOf(now.Add(-time.Minute), orgs)
		if c.meter != nil {
			c.meter(in.Meter)
		}
	}
	return buildCredits(t, in, now).Accounts[0]
}

// healthyOrg spent $20.00 on Oct 1, counted since September, last seen then.
func healthyOrg(t *testing.T) qc.MeterOrganization {
	return qc.MeterOrganization{
		Since:      instant(t, "2026-09-01T00:00:00Z"),
		LastSeenAt: ptrAt(t, "2026-10-01T12:00:00Z"), LastSuccessAt: ptrAt(t, "2026-10-01T12:00:00Z"),
		Days: []qc.MeterBucket{bucket(t, "2026-10-01T00:00:00Z", qc.MeterUsage{Model: "claude-sonnet-5-5", Requests: 10, Input: 10_000_000})},
	}
}

// Every row of the state table, first match winning, with its sentence.
func TestAPICreditStates(t *testing.T) {
	org := fakeOrg("a")
	healthy := func() qc.Entry { return creditEntry("alpha", 0, org, "200", "2026-10-29") }
	for _, tc := range []struct {
		name  string
		c     creditCase
		state string
		issue string
	}{
		{"ok", creditCase{entry: healthy(), org: healthyOrg(t)}, StatusOK, ""},
		{"misconfigured", creditCase{entry: func() qc.Entry {
			e := healthy()
			e.APICredit.Problem = qc.CreditProblemLabelDuplicate
			return e
		}(), org: healthyOrg(t)}, StateMisconfigured, "Another item already uses this label."},
		{"no configuration at all", creditCase{entry: func() qc.Entry {
			e := healthy()
			e.APICredit = nil
			return e
		}()}, StateMisconfigured, "This item's configuration has a problem."},
		{"written by quota-cache 0.1.13", creditCase{entry: qc.Entry{
			Provider: qc.ProviderAnthropicAPI, AuthIndex: qc.APICreditAccount("alpha"),
			APICredit: &qc.APICredit{Label: "alpha", MonthlyUSD: "200", Renews: "2026-10-29", KeyFingerprint: "key-000000000001"},
		}}, StateCacheTooOld, "Quota Cache 0.1.13 reads Anthropic's cost report, which Quota Glance no longer uses. Update Quota Cache to 0.1.14 and add this organization's organization-id."},
		{"no meter file", creditCase{entry: healthy(), noMeter: true}, StatusPending,
			"Quota Cache has not saved an API meter yet. Update it to 0.1.14 or newer; counting starts when it next loads."},
		{"not in the meter yet", creditCase{entry: healthy(), noOrg: true}, StatusPending, "Counting starts at Quota Cache's next save."},
		{"dormant in the meter", creditCase{entry: healthy(), org: func() qc.MeterOrganization {
			o := healthyOrg(t)
			o.UnlinkedAt = ptrAt(t, "2026-10-09T08:00:00Z")
			return o
		}()}, StatusPending, "Counting starts at Quota Cache's next save."},
		{"no credit", creditCase{entry: creditEntry("alpha", 0, org, "", "2026-10-29"), org: healthyOrg(t)},
			StateNeedsSettings, "Set this organization's monthly credit to count it in the total."},
		{"no refill date", creditCase{entry: creditEntry("alpha", 0, org, "200", ""), org: healthyOrg(t)},
			StateNeedsSettings, "Set the refill date, or enter a Console reading."},
		{"neither", creditCase{entry: creditEntry("alpha", 0, org, "", ""), org: healthyOrg(t)},
			StateNeedsSettings, "Set this organization's monthly credit and refill date."},
		{"out", creditCase{entry: healthy(), org: func() qc.MeterOrganization {
			o := healthyOrg(t)
			o.Refusals, o.LastRefusalAt = 1, ptrAt(t, "2026-10-09T10:00:00Z")
			return o
		}()}, StateOut, "Anthropic refused a request for low credit, so this credit is spent. The estimate had $180.00 left; enter a Console reading to correct it."},
		{"stale", creditCase{entry: healthy(), org: healthyOrg(t), stale: 45 * time.Minute, meter: func(m *qc.APIMeter) {
			m.FlushedAt = instant(t, "2026-10-09T11:00:00Z")
		}}, StateStale, "Quota Cache has not saved its meter recently, so recent spend may be missing."},
	} {
		got := tc.c.build(t)
		if got.State != tc.state || got.Issue != tc.issue {
			t.Errorf("%s: state %q issue %q; want %q %q (issues %v)", tc.name, got.State, got.Issue, tc.state, tc.issue, got.DataIssues)
		}
	}
}

// The problem sentences, including the generic one for a code this build
// does not know and for the codes only 0.1.13 wrote.
func TestProblemSentences(t *testing.T) {
	for problem, want := range map[string]string{
		qc.CreditProblemItemInvalid:             "This item in claude-api-credits is not a list of label, organization-id, monthly-usd and renews.",
		qc.CreditProblemUnknownField:            "This item has a setting Quota Cache does not know; check the spelling of label, organization-id, monthly-usd and renews.",
		qc.CreditProblemTooManyItems:            "Only the first 16 items in claude-api-credits are read.",
		qc.CreditProblemLabelMissing:            "Give this item a label, unique and at most 64 characters.",
		qc.CreditProblemLabelDuplicate:          "Another item already uses this label.",
		qc.CreditProblemOrganizationIDMissing:   "Add this organization's organization-id, from Console under Settings, Organization.",
		qc.CreditProblemOrganizationIDInvalid:   "organization-id must be the Organization ID from Console, like 12345678-1234-5678-1234-567812345678.",
		qc.CreditProblemOrganizationIDDuplicate: "Another item already uses this organization-id.",
		qc.CreditProblemAdminKeyMissing:         "This item's configuration has a problem.",
		qc.CreditProblemRenewsInvalid:           "This item's configuration has a problem.",
		"from_a_newer_quota_cache":              "This item's configuration has a problem.",
	} {
		if got := problemIssueOf(&qc.APICredit{Problem: problem}); got != want {
			t.Errorf("%s: %q", problem, got)
		}
	}
	if !strings.HasPrefix(problemIssueOf(&qc.APICredit{Problem: qc.CreditProblemLabelInvalid}), "This label is over 64 characters") {
		t.Error("label_invalid lost its sentence")
	}
}

// A misconfigured, too old or pending account shows no amounts at all, and
// its flags still say what is wrong with its configuration.
func TestAccountsWithoutAnEstimateCarryOnlyTheirFlags(t *testing.T) {
	e := creditEntry("alpha", 0, fakeOrg("a"), "", "")
	e.APICredit.MonthlyUSDInvalid, e.APICredit.RenewsInvalid, e.APICredit.AdminKeyIgnored = true, true, true
	got := creditCase{entry: e, noOrg: true}.build(t)
	want := []string{issueOrgNotCounted, issueNeedsCredit, issueNeedsRefillDate, issueConfigMonthlyUSDInvalid, issueConfigRenewsInvalid, issueAdminKeyIgnored}
	if !reflect.DeepEqual(got.DataIssues, want) || got.Settings.ConfigMonthlyUSDInvalid != true || !got.Settings.ConfigRenewsInvalid {
		t.Errorf("issues %v settings %+v", got.DataIssues, got.Settings)
	}
	e.APICredit.Problem = qc.CreditProblemLabelMissing
	got = creditCase{entry: e, org: healthyOrg(t)}.build(t)
	if !reflect.DeepEqual(got.DataIssues, []string{issueMisconfigured, issueConfigMonthlyUSDInvalid, issueConfigRenewsInvalid, issueAdminKeyIgnored}) {
		t.Errorf("misconfigured issues %v", got.DataIssues)
	}
	for issue, sentence := range map[string]string{
		issueConfigMonthlyUSDInvalid: "monthly-usd in Quota Cache's config is not a dollar amount, so it is ignored.",
		issueConfigRenewsInvalid:     "renews in Quota Cache's config is not a date, so it is ignored.",
		issueAdminKeyIgnored:         "Quota Cache no longer uses this item's admin-key. Delete it from the config.",
	} {
		a := &creditAccount{issues: map[string]bool{}}
		if got := a.sentenceOf(issue, nil); got != sentence {
			t.Errorf("%s: %q", issue, got)
		}
	}
}

// storedReading is a dashboard reading of amount at at, with the baseline the
// meter would have given it.
func storedReading(t *testing.T, org, amount, at string, baseline ...qc.MeterUsage) *overrides.APICredit {
	t.Helper()
	when := instant(t, at)
	if baseline == nil {
		baseline = []qc.MeterUsage{}
	}
	return &overrides.APICredit{Rev: 1, UpdatedAt: when, Reading: &overrides.Reading{
		RemainingUSD: amount, At: when, EnteredAt: when, OrganizationID: org,
		Baseline: overrides.Baseline{DayStart: qc.MeterDayStart(when), Until: qc.MeterHourStart(when), Usage: baseline},
	}}
}

// A reading is used only while it can describe this cycle of this
// organization, and says why it is not otherwise.
func TestReadingRules(t *testing.T) {
	org := fakeOrg("a")
	usedOn := func(stored *overrides.APICredit, renews string) APICreditAccount {
		return creditCase{entry: creditEntry("alpha", 0, org, "200", renews), org: healthyOrg(t), stored: stored}.build(t)
	}
	// Used: the day's usage less the baseline is the spend since.
	o := healthyOrg(t)
	o.Days = append(o.Days, bucket(t, "2026-10-09T00:00:00Z", qc.MeterUsage{Model: "claude-sonnet-5-5", Input: 3_000_000}))
	used := creditCase{entry: creditEntry("alpha", 0, org, "200", "2026-10-29"), org: o,
		stored: storedReading(t, org, "150.00", "2026-10-09T09:30:00Z", qc.MeterUsage{Model: "claude-sonnet-5-5", Input: 1_000_000})}.build(t)
	if used.Basis != basisReading || used.Reading == nil || used.Reading.SpentSinceText != "$4.00" || used.LeftText != "$146.00" ||
		used.UsedText != "$54.00" || used.SpentText != "$26.00" || used.Settings.ReadingUnusedReason != "" {
		t.Errorf("used: %s", jsonOf(t, used))
	}
	for _, tc := range []struct {
		name, renews, reason, sentence string
		stored                         *overrides.APICredit
	}{
		{"before the refill", "2026-10-05", readingBeforeRefill,
			"Your Console reading of $150.00 on Oct 3 was before the last refill, so it is not used.",
			storedReading(t, org, "150.00", "2026-10-03T09:00:00Z")},
		{"another organization", "2026-10-05", readingOtherOrganization,
			"Your Console reading was for a different organization-id, so it is not used.",
			storedReading(t, fakeOrg("b"), "150.00", "2026-10-09T09:00:00Z")},
		{"too old without a refill date", "", readingTooOld,
			"Your Console reading is over 31 days old and there is no refill date, so it is not used.",
			storedReading(t, org, "150.00", "2026-09-01T09:00:00Z")},
		{"in the future", "2026-10-05", readingFuture,
			"Your Console reading is dated in the future, so it is not used.",
			storedReading(t, org, "150.00", "2026-10-09T13:00:00Z")},
	} {
		got := usedOn(tc.stored, tc.renews)
		if got.Reading != nil || got.Settings.ReadingUnusedReason != tc.reason || got.Settings.Reading == nil ||
			!strings.Contains(strings.Join(got.DataIssues, ","), issueReadingUnused) {
			t.Errorf("%s: %s", tc.name, jsonOf(t, got))
		}
		if sentence := readingUnusedSentenceOf(got.Settings.ReadingUnusedReason, got.Settings.Reading); sentence != tc.sentence {
			t.Errorf("%s: %q", tc.name, sentence)
		}
	}
	// Taken on the refill day: used, and flagged, since Console may not have
	// shown the new credit yet.
	refillDay := usedOn(storedReading(t, org, "190.00", "2026-10-05T08:00:00Z"), "2026-10-05")
	if refillDay.Basis != basisReading || !reflect.DeepEqual(refillDay.DataIssues, []string{issueReadingOnRefillDay}) ||
		refillDay.Issue != "This Console reading was taken on the refill day. If Console did not show the new credit yet, enter a new reading once it does." {
		t.Errorf("refill day: %s", jsonOf(t, refillDay))
	}
	// A day later it is not.
	if next := usedOn(storedReading(t, org, "190.00", "2026-10-06T00:00:00Z"), "2026-10-05"); len(next.DataIssues) != 0 {
		t.Errorf("the day after the refill: %v", next.DataIssues)
	}
	// Without a refill date a recent reading is the basis on its own.
	alone := usedOn(storedReading(t, org, "190.00", "2026-10-09T09:00:00Z"), "")
	if alone.Basis != basisReading || alone.State != StatusOK || *alone.SpentSinceEpoch != instant(t, "2026-10-09T09:00:00Z").Unix() ||
		alone.CycleStartEpoch != nil || alone.LeftText != "$190.00" {
		t.Errorf("no refill date: %s", jsonOf(t, alone))
	}
}

// A refusal Anthropic followed with a success is cleared: purchased credit or
// a refill paid for the request after it.
func TestARefusalFollowedByASuccessIsNotOut(t *testing.T) {
	o := healthyOrg(t)
	o.Refusals, o.LastRefusalAt = 2, ptrAt(t, "2026-10-09T10:00:00Z")
	o.LastSuccessAt = ptrAt(t, "2026-10-09T10:01:00Z")
	got := creditCase{entry: creditEntry("alpha", 0, fakeOrg("a"), "200", "2026-10-29"), org: o}.build(t)
	if got.State != StatusOK || got.LeftText != "$180.00" || got.EstimateLeftText != "" || got.Refusals.Total != 2 {
		t.Errorf("%s", jsonOf(t, got))
	}
	// With nothing left in the estimate there is nothing to correct, and the
	// sentence stops at the refusal.
	spent := healthyOrg(t)
	spent.Refusals, spent.LastRefusalAt = 1, ptrAt(t, "2026-10-09T10:00:00Z")
	if got := (creditCase{entry: creditEntry("alpha", 0, fakeOrg("a"), "20", "2026-10-29"), org: spent}).build(t); got.State != StateOut ||
		got.EstimateLeftText != "$0.00" || got.Issue != "Anthropic refused a request for low credit, so this credit is spent." {
		t.Errorf("refused with nothing left: %s", jsonOf(t, got))
	}
	// A refusal from before the anchor is from another cycle.
	o.LastSuccessAt, o.LastRefusalAt = ptrAt(t, "2026-09-01T00:00:00Z"), ptrAt(t, "2026-09-20T00:00:00Z")
	if got := (creditCase{entry: creditEntry("alpha", 0, fakeOrg("a"), "200", "2026-10-29"), org: o}).build(t); got.State != StatusOK {
		t.Errorf("an old refusal: %s", got.State)
	}
}

// A refusal of a Claude Code-based client's request is out only when the
// estimate was already nearly spent: Claude Code is not covered by the credit,
// the Agent SDK is.
func TestClaudeCodeRefusals(t *testing.T) {
	org := fakeOrg("a")
	// spentInput Sonnet 5.5 input tokens cost $2.00 a million.
	refusedOn := func(monthly string, spentInput uint64) APICreditAccount {
		o := healthyOrg(t)
		o.Days = []qc.MeterBucket{bucket(t, "2026-10-01T00:00:00Z", qc.MeterUsage{Model: "claude-sonnet-5-5", Input: spentInput})}
		o.ClaudeCodeRefusals, o.LastClaudeCodeRefusalAt = 4, ptrAt(t, "2026-10-09T10:00:00Z")
		return creditCase{entry: creditEntry("alpha", 0, org, monthly, "2026-10-29"), org: o}.build(t)
	}
	// $20.00 spent of $20.00: nothing left, so out.
	spent := refusedOn("20", 10_000_000)
	if spent.State != StateOut || spent.LeftText != "$0.00" || spent.Level != LevelCritical ||
		!reflect.DeepEqual(spent.DataIssues, []string{issueRefusedNearlySpent}) ||
		spent.Issue != "Anthropic refused requests from a Claude Code-based client for low credit, and the estimate is nearly spent, so this credit is shown as spent. Enter a Console reading to check." ||
		spent.Refusals.ClaudeCodeTotal != 4 {
		t.Errorf("spent: %s", jsonOf(t, spent))
	}
	// $20.00 of $40.00: half left, so not out.
	half := refusedOn("40", 10_000_000)
	if half.State != StatusOK || half.LeftText != "$20.00" || half.RemainingPercent != 50 ||
		!reflect.DeepEqual(half.DataIssues, []string{issueClaudeCodeRefused}) ||
		half.Issue != "Anthropic refused requests from a Claude Code-based client (Claude Code or the Agent SDK) for low credit. If they were Agent SDK requests, this credit may be spent; enter a Console reading." {
		t.Errorf("half: %s", jsonOf(t, half))
	}
	// Exactly a tenth left, $20.00 of $200.00, is nearly spent; a millionth
	// of a dollar more is not.
	if tenth := refusedOn("200", 90_000_000); tenth.State != StateOut || tenth.EstimateLeftText != "$20.00" {
		t.Errorf("a tenth left: %s", jsonOf(t, tenth))
	}
	if over := refusedOn("200", 89_999_999); over.State != StatusOK {
		t.Errorf("just over a tenth left: %s", over.State)
	}
}

// Each thing that can hide spend after the anchor makes the account a lower
// bound with its own issue; the same before the anchor does not.
func TestEachLowerBoundReason(t *testing.T) {
	org := fakeOrg("a")
	entry := creditEntry("alpha", 0, org, "200", "2026-10-29") // anchor Sep 29
	before, after := ptrAt(t, "2026-09-20T00:00:00Z"), ptrAt(t, "2026-10-02T00:00:00Z")
	for _, tc := range []struct {
		issue    string
		sentence string
		set      func(*qc.APIMeter, *qc.MeterOrganization, *time.Time)
	}{
		{issueMeterStartedLate, "Counting began after this cycle started, so earlier spend is missing. Enter a Console reading to correct it.",
			func(_ *qc.APIMeter, o *qc.MeterOrganization, at *time.Time) { o.Since = *at }},
		{issueMeterGap, "Quota Cache was not counting for part of this period, for example while it was off or reloading, so some spend may be missing. Enter a Console reading to correct it.",
			func(m *qc.APIMeter, _ *qc.MeterOrganization, at *time.Time) {
				m.Gaps = []qc.MeterGap{{From: at.Add(-time.Hour), To: *at, Reason: qc.MeterStopShutdown}}
			}},
		{issueMeterDropped, "Quota Cache dropped usage records it could not keep up with, so some spend is missing.",
			func(m *qc.APIMeter, _ *qc.MeterOrganization, at *time.Time) { m.Dropped, m.LastDroppedAt = 9, at }},
		{issueMeterFull, "Quota Cache's meter was full, so some spend is missing.",
			func(_ *qc.APIMeter, o *qc.MeterOrganization, at *time.Time) { o.LastOverflowAt = at }},
		{issueMeterUnattributed, "Some failed requests could not be matched to an organization, so some spend may be missing.",
			func(m *qc.APIMeter, _ *qc.MeterOrganization, at *time.Time) {
				m.Unattributed, m.LastUnattributedAt = 2, at
			}},
		{issueMeterRejected, "Quota Cache could not read some usage records from CPA, so some spend may be missing.",
			func(m *qc.APIMeter, _ *qc.MeterOrganization, at *time.Time) { m.Rejected, m.LastRejectedAt = 1, at }},
	} {
		for _, at := range []*time.Time{before, after} {
			o := healthyOrg(t)
			if tc.issue == issueMeterStartedLate {
				o.Days = nil
			}
			got := creditCase{entry: entry, org: o, meter: func(m *qc.APIMeter) {
				org := m.Organizations[fakeOrg("a")]
				tc.set(m, &org, at)
				m.Organizations[fakeOrg("a")] = org
			}}.build(t)
			flagged := got.LowerBound && len(got.DataIssues) > 0 && got.DataIssues[0] == tc.issue
			if want := at == after; flagged != want {
				t.Errorf("%s at %s: lowerBound %v issues %v", tc.issue, at.Format(time.DateOnly), got.LowerBound, got.DataIssues)
			}
			if at == after && got.Issue != tc.sentence {
				t.Errorf("%s: %q", tc.issue, got.Issue)
			}
		}
	}
	// A stopped meter is an open gap once it has been stopped five minutes.
	for _, tc := range []struct {
		stopped string
		flagged bool
	}{{"2026-10-09T11:56:00Z", false}, {"2026-10-09T11:55:00Z", true}} {
		got := creditCase{entry: entry, org: healthyOrg(t), meter: func(m *qc.APIMeter) {
			m.StoppedAt, m.StopReason = ptrAt(t, tc.stopped), qc.MeterStopQuiesce
		}}.build(t)
		if got.LowerBound != tc.flagged {
			t.Errorf("stopped at %s: lowerBound %v issues %v", tc.stopped, got.LowerBound, got.DataIssues)
		}
	}
	// A brief gap is ignored wherever it falls; a long one counts.
	for _, tc := range []struct {
		length  time.Duration
		flagged bool
	}{{qc.MeterBriefGap - time.Second, false}, {qc.MeterBriefGap, true}} {
		got := creditCase{entry: entry, org: healthyOrg(t), meter: func(m *qc.APIMeter) {
			from := instant(t, "2026-10-09T09:00:00Z")
			m.Gaps = []qc.MeterGap{{From: from, To: from.Add(tc.length), Reason: qc.MeterStopShutdown}}
		}}.build(t)
		if got.LowerBound != tc.flagged {
			t.Errorf("a %v gap: lowerBound %v", tc.length, got.LowerBound)
		}
	}
	// The started-late sentence follows the basis.
	o := healthyOrg(t)
	o.Since = instant(t, "2026-10-09T10:00:00Z")
	reading := creditCase{entry: entry, org: o, stored: storedReading(t, org, "100.00", "2026-10-09T09:00:00Z")}.build(t)
	if reading.Issue != "Counting began after your Console reading, so spend in between is missing." {
		t.Errorf("reading basis: %q", reading.Issue)
	}
	none := creditCase{entry: creditEntry("alpha", 0, org, "", "2026-10-29"), org: o}.build(t)
	if a := (&creditAccount{issues: map[string]bool{}, out: none}); a.sentenceOf(issueMeterStartedLate, nil) != "Counting began recently, so earlier spend is missing." {
		t.Errorf("no basis: %q", a.sentenceOf(issueMeterStartedLate, nil))
	}
}

// A model the table has no price for is left out of every amount and listed;
// (other) is left out and never listed.
func TestUnpricedModelsAndOther(t *testing.T) {
	o := healthyOrg(t)
	o.Days = append(o.Days, bucket(t, "2026-10-02T00:00:00Z",
		qc.MeterUsage{Model: "claude-mythos-preview", Input: 1_000_000},
		qc.MeterUsage{Model: "claude-3-7-sonnet", Input: 250_000},
		qc.MeterUsage{Model: "claude-3-haiku", Output: 900},
		qc.MeterUsage{Model: "made-up-model", Input: 10},
		qc.MeterUsage{Model: qc.MeterOtherModel, Input: 50_000_000},
	))
	got := creditCase{entry: creditEntry("alpha", 0, fakeOrg("a"), "200", "2026-10-29"), org: o}.build(t)
	if got.SpentText != "$20.00" || !got.LowerBound || len(got.Unpriced) != 4 || got.Unpriced[0].Model != "claude-mythos-preview" ||
		got.Issue != "Some requests used a model with no listed price and are left out: claude-mythos-preview (1.0M tokens), claude-3-7-sonnet (250K tokens), claude-3-haiku (900 tokens) and 1 more." {
		t.Errorf("%s", jsonOf(t, got))
	}
	only := healthyOrg(t)
	only.Days = append(only.Days, bucket(t, "2026-10-02T00:00:00Z", qc.MeterUsage{Model: qc.MeterOtherModel, Input: 50_000_000}))
	if got := (creditCase{entry: creditEntry("alpha", 0, fakeOrg("a"), "200", "2026-10-29"), org: only}).build(t); got.LowerBound || len(got.Unpriced) != 0 {
		t.Errorf("(other) alone: %s", jsonOf(t, got))
	}
	for n, want := range map[uint64]string{0: "0", 900: "900", 999: "999", 1_000: "1K", 250_000: "250K", 999_499: "999K",
		999_950: "1.0M", 1_000_000: "1.0M", 1_250_000: "1.3M", 12_340_000: "12.3M"} {
		if got := tokensText(n); got != want {
			t.Errorf("tokensText(%d) = %q, want %q", n, got, want)
		}
	}
}

// Nothing seen is said, and is not a bound by itself.
func TestNoTraffic(t *testing.T) {
	got := creditCase{entry: creditEntry("alpha", 0, fakeOrg("a"), "200", "2026-10-29"),
		org: qc.MeterOrganization{Since: instant(t, "2026-09-01T00:00:00Z")}}.build(t)
	if got.LowerBound || got.LeftText != "$200.00" || !reflect.DeepEqual(got.DataIssues, []string{issueNoTraffic}) ||
		got.Issue != "No API traffic for this organization has reached CPA since counting began." || got.LastSeenEpoch != nil {
		t.Errorf("%s", jsonOf(t, got))
	}
}

// Cache writes cost the 5-minute rate; the 1-hour difference is beside them,
// and its text only once it is worth a cent.
func TestCacheWriteExtra(t *testing.T) {
	for _, tc := range []struct {
		writes uint64
		text   string
	}{{4_000_000, "$6.00"}, {6_667, "$0.01"}, {6_666, ""}} {
		o := healthyOrg(t)
		o.Days = []qc.MeterBucket{bucket(t, "2026-10-01T00:00:00Z", qc.MeterUsage{Model: "claude-sonnet-5-5", CacheWrite: tc.writes})}
		got := creditCase{entry: creditEntry("alpha", 0, fakeOrg("a"), "200", "2026-10-29"), org: o}.build(t)
		if got.CacheWriteExtraText != tc.text {
			t.Errorf("%d writes: %q (%v)", tc.writes, got.CacheWriteExtraText, got.CacheWriteExtra)
		}
	}
}

// A credit of nothing has nothing left to show as a fraction, and any spend
// is past it.
func TestZeroCredit(t *testing.T) {
	got := creditCase{entry: creditEntry("alpha", 0, fakeOrg("a"), "0", "2026-10-29"), org: healthyOrg(t)}.build(t)
	if got.State != StatusOK || got.Level != "" || got.RemainingFraction != 0 || got.RemainingPercent != 0 ||
		got.LeftText != "$0.00" || got.OverageText != "$20.00" || got.UsedText != "$0.00" ||
		!reflect.DeepEqual(got.DataIssues, []string{issueZeroCredit, issueOverCredit}) ||
		got.Issue != "This organization's monthly credit is set to $0.00." {
		t.Errorf("%s", jsonOf(t, got))
	}
}

// Spend past the credit without a refusal: likely purchased credit.
func TestOverCreditWithoutARefusal(t *testing.T) {
	got := creditCase{entry: creditEntry("alpha", 0, fakeOrg("a"), "15", "2026-10-29"), org: healthyOrg(t)}.build(t)
	if got.State != StatusOK || got.LeftText != "$0.00" || got.UsedText != "$15.00" || got.OverageText != "$5.00" || got.Level != LevelCritical ||
		got.Issue != "Spend is $5.00 past the monthly credit. Anthropic bills purchased credit after the monthly credit; if there is none, enter a Console reading." {
		t.Errorf("%s", jsonOf(t, got))
	}
}

// A reading above the credit fills the bar and asks the operator to check the
// credit.
func TestReadingAboveTheCredit(t *testing.T) {
	org := fakeOrg("a")
	got := creditCase{entry: creditEntry("alpha", 0, org, "200", "2026-10-29"), org: healthyOrg(t),
		stored: storedReading(t, org, "260.00", "2026-10-09T09:00:00Z")}.build(t)
	if got.RemainingFraction != 1 || got.RemainingPercent != 100 || got.LeftText != "$260.00" || got.UsedText != "$0.00" ||
		!reflect.DeepEqual(got.DataIssues, []string{issueReadingAboveCredit}) ||
		got.Issue != "Your Console reading is more than the monthly credit; check the monthly credit." {
		t.Errorf("%s", jsonOf(t, got))
	}
}

// Dashboard values win field by field; the config's are always shown beside
// them; and Use config, which deletes the dashboard's, brings them back.
func TestDashboardValuesWinFieldByField(t *testing.T) {
	org := fakeOrg("a")
	entry := creditEntry("alpha", 0, org, "200", "2026-10-29T00:00:00Z")
	stored := &overrides.APICredit{MonthlyUSD: "300", Rev: 4, UpdatedAt: instant(t, "2026-10-08T00:00:00Z")}
	got := creditCase{entry: entry, org: healthyOrg(t), stored: stored}.build(t)
	if got.MonthlyCreditText != "$300.00" || got.MonthlyCreditSource != sourceDashboard || got.RenewsSource != sourceConfig ||
		got.Settings.MonthlyUSD != "300" || got.Settings.ConfigMonthlyUSD != "200" || got.Settings.ConfigRenews != "2026-10-29" ||
		got.Settings.Renews != "" || got.Settings.Revision != "4" || *got.Settings.UpdatedAtEpoch != instant(t, "2026-10-08T00:00:00Z").Unix() {
		t.Errorf("%s", jsonOf(t, got))
	}
	stored = &overrides.APICredit{Renews: "2026-10-05", Rev: 5, UpdatedAt: instant(t, "2026-10-08T00:00:00Z")}
	got = creditCase{entry: entry, org: healthyOrg(t), stored: stored}.build(t)
	if got.MonthlyCreditSource != sourceConfig || got.RenewsSource != sourceDashboard ||
		*got.CycleStartEpoch != instant(t, "2026-10-05T00:00:00Z").Unix() || got.SpentText != "$0.00" {
		t.Errorf("%s", jsonOf(t, got))
	}
	if got := (creditCase{entry: entry, org: healthyOrg(t)}).build(t); got.Settings.Revision != "" || got.Settings.UpdatedAtEpoch != nil {
		t.Errorf("nothing stored: %+v", got.Settings)
	}
}

// Who may edit what, and why not.
func TestNotEditableReasons(t *testing.T) {
	org := fakeOrg("a")
	itemOf := func(problem string) qc.Entry {
		e := creditEntry("alpha", 0, org, "200", "2026-10-29")
		e.AuthIndex, e.APICredit.Problem = "item-1", problem
		return e
	}
	tooOld := qc.Entry{Provider: qc.ProviderAnthropicAPI, AuthIndex: qc.APICreditAccount("alpha"),
		APICredit: &qc.APICredit{Label: "alpha", MonthlyUSD: "200", Renews: "2026-10-29", KeyFingerprint: "key-000000000001"}}
	misconfigured := creditEntry("alpha", 0, org, "200", "2026-10-29")
	misconfigured.APICredit.Problem = qc.CreditProblemLabelInvalid
	for name, tc := range map[string]struct {
		entry    qc.Entry
		editable bool
		reason   string
	}{
		"linked":                   {creditEntry("alpha", 0, org, "200", "2026-10-29"), true, ""},
		"misconfigured but linked": {misconfigured, true, ""},
		"past the sixteenth":       {itemOf(qc.CreditProblemTooManyItems), false, notEditableOverLimit},
		"a duplicate":              {itemOf(qc.CreditProblemOrganizationIDDuplicate), false, notEditableDuplicate},
		"no organization":          {itemOf(qc.CreditProblemOrganizationIDMissing), false, notEditableNoOrganization},
		"written by 0.1.13":        {tooOld, false, notEditableCacheTooOld},
	} {
		got := creditCase{entry: tc.entry, org: healthyOrg(t)}.build(t)
		if got.Settings.Editable != tc.editable || got.Settings.NotEditableReason != tc.reason {
			t.Errorf("%s: editable %v reason %q", name, got.Settings.Editable, got.Settings.NotEditableReason)
		}
	}
	// The card's own reason, when editing is unavailable altogether.
	entry := creditEntry("alpha", 0, org, "200", "2026-10-29")
	in := Input{Snapshot: snapshotOf(entry), Meter: meterOf(instant(t, "2026-10-09T11:59:00Z"), map[string]qc.MeterOrganization{org: healthyOrg(t)})}
	disabled := buildCredits(t, in, instant(t, workedNow))
	in.AllowEdit, in.Overrides = true, overrides.Values{Unreadable: true}
	unreadable := buildCredits(t, in, instant(t, workedNow))
	if disabled.Editing != (APICreditEditing{Reason: "disabled"}) || disabled.Accounts[0].Settings.NotEditableReason != "disabled" ||
		unreadable.Editing != (APICreditEditing{Reason: "settingsUnreadable"}) || unreadable.Accounts[0].Settings.NotEditableReason != "settingsUnreadable" {
		t.Errorf("disabled %+v %+v; unreadable %+v %+v", disabled.Editing, disabled.Accounts[0].Settings, unreadable.Editing, unreadable.Accounts[0].Settings)
	}
}

// allow-edit off stops the page editing, not the stored values applying.
func TestStoredValuesApplyWithEditingOff(t *testing.T) {
	org := fakeOrg("a")
	entry := creditEntry("alpha", 0, org, "200", "2026-10-29")
	values := overrides.Values{APICredits: map[string]overrides.APICredit{
		entry.AuthIndex: {MonthlyUSD: "300", Rev: 1, UpdatedAt: instant(t, "2026-10-08T00:00:00Z")},
	}}
	in := Input{Snapshot: snapshotOf(entry), Overrides: values,
		Meter: meterOf(instant(t, "2026-10-09T11:59:00Z"), map[string]qc.MeterOrganization{org: healthyOrg(t)})}
	if got := buildCredits(t, in, instant(t, workedNow)).Accounts[0]; got.MonthlyCreditText != "$300.00" || got.Settings.Editable {
		t.Errorf("%s", jsonOf(t, got))
	}
}

// Stored values for an account no longer listed are offered for removal,
// oldest first.
func TestOrphans(t *testing.T) {
	org := fakeOrg("a")
	entry := creditEntry("alpha", 0, org, "200", "2026-10-29")
	values := overrides.Values{APICredits: map[string]overrides.APICredit{
		entry.AuthIndex:    {MonthlyUSD: "300", Rev: 3, UpdatedAt: instant(t, "2026-10-08T00:00:00Z")},
		"org-3f2a9c1d0b7e": {MonthlyUSD: "260.50", Renews: "2026-10-29", Rev: 2, UpdatedAt: instant(t, "2026-10-02T00:00:00Z")},
		"org-000000000001": {Reading: storedReading(t, fakeOrg("b"), "1.00", "2026-10-01T00:00:00Z").Reading, Rev: 1, UpdatedAt: instant(t, "2026-10-01T00:00:00Z")},
	}}
	in := Input{Snapshot: snapshotOf(entry), Overrides: values, AllowEdit: true}
	got := buildCredits(t, in, instant(t, workedNow)).Orphans
	want := []APICreditOrphan{
		{ID: "org-000000000001", HasReading: true, Revision: "1", UpdatedAtEpoch: instant(t, "2026-10-01T00:00:00Z").Unix()},
		{ID: "org-3f2a9c1d0b7e", MonthlyUSD: "260.50", Renews: "2026-10-29", Revision: "2", UpdatedAtEpoch: instant(t, "2026-10-02T00:00:00Z").Unix()},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("orphans:\n got %s\nwant %s", jsonOf(t, got), jsonOf(t, want))
	}
}

// Organizations the meter saw that nothing counts, from the meter's own list,
// each with why.
func TestUnlinkedOrganizations(t *testing.T) {
	org := fakeOrg("a")
	over := creditEntry("seventeenth", 16, fakeOrg("c"), "200", "")
	over.AuthIndex, over.APICredit.Problem = "item-17", qc.CreditProblemTooManyItems
	in := Input{Snapshot: snapshotOf(creditEntry("alpha", 0, org, "200", "2026-10-29"), over),
		Meter: meterOf(instant(t, "2026-10-09T11:59:00Z"), map[string]qc.MeterOrganization{org: healthyOrg(t)})}
	in.Meter.Unlinked = []qc.MeterUnlinked{
		{OrganizationID: fakeOrg("c"), FirstSeenAt: instant(t, "2026-10-01T00:00:00Z"), LastSeenAt: instant(t, "2026-10-09T10:00:00Z"), Requests: 37},
		{OrganizationID: fakeOrg("d"), FirstSeenAt: instant(t, "2026-10-02T00:00:00Z"), LastSeenAt: instant(t, "2026-10-09T09:00:00Z"), Requests: 2},
	}
	got := buildCredits(t, in, instant(t, workedNow)).Unlinked
	if len(got) != 2 || got[0].Reason != unlinkedOverLimit || got[0].Requests != 37 || got[1].Reason != unlinkedNotConfigured ||
		got[1].LastSeenEpoch != instant(t, "2026-10-09T09:00:00Z").Unix() {
		t.Errorf("%s", jsonOf(t, got))
	}
	in.Meter = nil
	if got := buildCredits(t, in, instant(t, workedNow)); got.Unlinked == nil || len(got.Unlinked) != 0 || got.Meter != nil {
		t.Errorf("no meter: %s", jsonOf(t, got))
	}
}

// Not configured is null, never omitted and never an empty object: a client
// tells "no credits configured" from "credits with nothing counted" by it.
func TestAPICreditsIsNullWithoutAnthropicEntries(t *testing.T) {
	doc := Build(Input{Snapshot: loadSnapshot(t, "seven-credentials.json"), Identities: fixtureRoster()}, at(t, 0))
	if doc.APICredits == nil {
		t.Fatal("the fixture has anthropic-api entries")
	}
	snapshot := loadSnapshot(t, "seven-credentials.json")
	for key, entry := range snapshot.Entries {
		if entry.Provider == qc.ProviderAnthropicAPI {
			delete(snapshot.Entries, key)
		}
	}
	doc = Build(Input{Snapshot: snapshot, Identities: fixtureRoster()}, at(t, 0))
	if doc.APICredits != nil {
		t.Fatalf("apiCredits = %+v", doc.APICredits)
	}
	if raw := jsonOf(t, doc); !strings.Contains(raw, `"apiCredits":null`) || !strings.Contains(raw, `"renewalOrphans":[]`) {
		t.Fatal(`the document must carry "apiCredits":null and "renewalOrphans":[]`)
	}
}

// Accounts refilling at the same second refill together, and one that has
// used nothing refills nothing.
func TestNextRefillMergesTies(t *testing.T) {
	spentOn := func(dollars uint64) qc.MeterOrganization {
		o := healthyOrg(t)
		o.Days = []qc.MeterBucket{bucket(t, "2026-10-01T00:00:00Z", qc.MeterUsage{Model: "claude-sonnet-5-5", Input: dollars * 500_000})}
		return o
	}
	entries := []qc.Entry{
		creditEntry("alpha", 0, fakeOrg("a"), "200", "2026-10-20"),
		creditEntry("bravo", 1, fakeOrg("b"), "100", "2026-10-20"),
		creditEntry("charlie", 2, fakeOrg("c"), "100", "2026-10-12"),
		creditEntry("delta", 3, fakeOrg("d"), "100", "2026-10-25"),
	}
	in := Input{Snapshot: snapshotOf(entries...), Meter: meterOf(instant(t, "2026-10-09T11:59:00Z"), map[string]qc.MeterOrganization{
		fakeOrg("a"): spentOn(50), fakeOrg("b"): spentOn(120), fakeOrg("c"): {Since: instant(t, "2026-09-01T00:00:00Z")}, fakeOrg("d"): spentOn(1),
	})}
	pool := buildCredits(t, in, instant(t, workedNow)).Pool
	refill := pool.NextRefill
	if refill == nil || !reflect.DeepEqual(refill.AccountIDs, []string{entries[0].AuthIndex, entries[1].AuthIndex}) ||
		refill.RefillAtEpoch != instant(t, "2026-10-20T00:00:00Z").Unix() || refill.Gain != 150 || refill.GainText != "$150.00" ||
		refill.GainFraction != 0.3 || refill.GainPercent != 30 {
		t.Fatalf("refill = %+v", refill)
	}
	if *pool.FullAtEpoch != instant(t, "2026-10-25T00:00:00Z").Unix() {
		t.Errorf("fullAt = %d", *pool.FullAtEpoch)
	}
}

// With nothing counted the pool has no figures at all, not zero ones.
func TestPoolWithNothingCounted(t *testing.T) {
	misconfigured := creditEntry("bravo", 1, fakeOrg("b"), "200", "2026-10-29")
	misconfigured.APICredit.Problem = qc.CreditProblemLabelMissing
	in := Input{Snapshot: snapshotOf(creditEntry("alpha", 0, fakeOrg("a"), "200", "2026-10-29"), misconfigured)}
	credits := buildCredits(t, in, instant(t, workedNow))
	want := APICreditPool{AccountCount: 2, MissingCount: 2}
	if !reflect.DeepEqual(credits.Pool, want) {
		t.Errorf("pool = %+v", credits.Pool)
	}
	if raw := jsonOf(t, credits.Pool); !strings.Contains(raw, `"nextRefill":null`) || !strings.Contains(raw, `"fullAtEpoch":null`) || !strings.Contains(raw, `"level":""`) {
		t.Errorf("pool JSON = %s", raw)
	}
}

// The cycle clamps a refill day past a month's end, through February.
func TestAPICreditCycleClampsAcrossFebruary(t *testing.T) {
	for _, tc := range []struct{ renews, now, start, end string }{
		{"2026-10-31", "2027-02-15T00:00:00Z", "2027-01-31T00:00:00Z", "2027-02-28T00:00:00Z"},
		{"2026-10-31", "2027-03-01T00:00:00Z", "2027-02-28T00:00:00Z", "2027-03-31T00:00:00Z"},
		{"2026-10-31", "2028-03-01T00:00:00Z", "2028-02-29T00:00:00Z", "2028-03-31T00:00:00Z"},
		{"2026-10-29", "2026-10-29T00:00:00Z", "2026-10-29T00:00:00Z", "2026-11-29T00:00:00Z"},
	} {
		now := instant(t, tc.now)
		account := buildCredits(t, Input{Snapshot: snapshotOf(creditEntry("alpha", 0, fakeOrg("a"), "100", tc.renews))}, now).Accounts[0]
		start, end := instant(t, tc.start), instant(t, tc.end)
		if *account.CycleStartEpoch != start.Unix() || *account.RenewsAtEpoch != end.Unix() || *account.RenewsInSeconds != end.Unix()-now.Unix() {
			t.Errorf("renews %s at %s: %s to %s, want %s to %s", tc.renews, tc.now,
				time.Unix(*account.CycleStartEpoch, 0).UTC(), time.Unix(*account.RenewsAtEpoch, 0).UTC(), tc.start, tc.end)
		}
	}
}

// Moving the clock moves every countdown and no instant.
func TestAPICreditCountdownsFollowTheClock(t *testing.T) {
	in := workedInput(t, "api-credits")
	first := buildCredits(t, in, instant(t, workedNow))
	later := buildCredits(t, in, instant(t, workedNow).Add(900*time.Second))
	if first.Pool.NextRefill.RefillInSeconds-later.Pool.NextRefill.RefillInSeconds != 900 ||
		first.Pool.NextRefill.RefillAtEpoch != later.Pool.NextRefill.RefillAtEpoch ||
		*first.Pool.FullInSeconds-*later.Pool.FullInSeconds != 900 || *first.Pool.FullAtEpoch != *later.Pool.FullAtEpoch {
		t.Errorf("pool: %+v then %+v", first.Pool, later.Pool)
	}
	for i := range first.Accounts {
		a, b := first.Accounts[i], later.Accounts[i]
		if *a.RenewsInSeconds-*b.RenewsInSeconds != 900 || *a.RenewsAtEpoch != *b.RenewsAtEpoch || *a.CycleStartEpoch != *b.CycleStartEpoch {
			t.Errorf("%s: %+v then %+v", a.Label, a, b)
		}
	}
}

// No key ever reaches quota-glance's input, so none can reach the document;
// this pins that nothing in the document is shaped like one.
func TestAPICreditDocumentCarriesNoKey(t *testing.T) {
	for _, doc := range []Document{buildFixture(t), buildDegraded(t)} {
		if raw := jsonOf(t, doc); strings.Contains(raw, "sk-ant-") {
			t.Fatal("a document contains something shaped like an Anthropic key")
		}
	}
}

// The happy-path golden is the card the design is drawn against.
func TestGoldenAPICreditsShowEveryPoolShape(t *testing.T) {
	credits := buildFixture(t).APICredits
	if credits == nil || credits.Meter == nil {
		t.Fatal("apiCredits or its meter is null")
	}
	bases, levels := map[string]bool{}, map[string]bool{}
	var sawDashboardGrant, sawCacheWriteExtra bool
	for _, account := range credits.Accounts {
		if account.State != StatusOK || !account.Counted {
			t.Errorf("%s: state %s", account.Label, account.State)
		}
		bases[account.Basis], levels[account.Level] = true, true
		sawDashboardGrant = sawDashboardGrant || (account.Basis == basisReading && account.MonthlyCreditSource == sourceDashboard)
		sawCacheWriteExtra = sawCacheWriteExtra || account.CacheWriteExtraText != ""
	}
	if len(credits.Accounts) != 5 || !bases[basisCredit] || !bases[basisReading] || !levels[LevelOK] || !levels[LevelLow] ||
		!sawDashboardGrant || !sawCacheWriteExtra || len(credits.Unlinked) != 1 || credits.Unlinked[0].Reason != unlinkedNotConfigured {
		t.Errorf("the happy path misses a shape: bases %v levels %v dashboard grant %v cache-write extra %v unlinked %d",
			bases, levels, sawDashboardGrant, sawCacheWriteExtra, len(credits.Unlinked))
	}
	if !credits.Editing.Available || credits.Pool.NextRefill == nil || !credits.Pool.HasEstimate {
		t.Errorf("pool: %+v editing %+v", credits.Pool, credits.Editing)
	}
}

// Every state and issue the web app has to render for an API credit account
// must appear in the degraded contract.
func TestDegradedContractCoversEveryAPICreditState(t *testing.T) {
	credits := buildDegraded(t).APICredits
	if credits == nil || credits.Meter == nil {
		t.Fatal("apiCredits or its meter is null")
	}
	checkCreditPoolAddsUp(t, credits)
	states, issues, reasons := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var sawLowerBoundStartedLate, sawRefillOnly bool
	for _, account := range credits.Accounts {
		states[account.State] = true
		for _, issue := range account.DataIssues {
			issues[issue] = true
		}
		reasons[account.Settings.NotEditableReason] = true
		sawLowerBoundStartedLate = sawLowerBoundStartedLate || (account.LowerBound && account.Counted && issueIn(account, issueMeterStartedLate))
		sawRefillOnly = sawRefillOnly || (account.State == StateNeedsSettings && issueIn(account, issueNeedsRefillDate) && !issueIn(account, issueNeedsCredit))
	}
	// No account is ok here: the degraded meter is stale, which every counted
	// account but an out one shares. The happy path carries ok.
	for _, want := range []string{StateStale, StateOut, StatusPending, StateNeedsSettings, StateMisconfigured, StateCacheTooOld} {
		if !states[want] {
			t.Errorf("no account with state %q", want)
		}
	}
	for _, want := range []string{issueRefused, issueStale, issueMeterStartedLate, issueMeterGap, issueUnpricedModel,
		issueMisconfigured, issueCacheTooOld, issueOrgNotCounted, issueNeedsRefillDate} {
		if !issues[want] {
			t.Errorf("no dataIssue %q", want)
		}
	}
	for _, want := range []string{notEditableOverLimit, notEditableDuplicate, notEditableNoOrganization, notEditableCacheTooOld, editingDisabled} {
		if !reasons[want] {
			t.Errorf("no account not editable for %q", want)
		}
	}
	if !sawLowerBoundStartedLate || !sawRefillOnly {
		t.Errorf("missing: lowerBound startedLate %v, needsSettings for the refill date only %v", sawLowerBoundStartedLate, sawRefillOnly)
	}
	if credits.Editing.Available || !credits.Meter.Stale || len(credits.Meter.Gaps) == 0 || credits.Meter.Gaps[0].Reason != qc.MeterStopShutdown ||
		len(credits.Orphans) == 0 {
		t.Errorf("card: editing %+v meter %+v orphans %d", credits.Editing, credits.Meter, len(credits.Orphans))
	}
	overLimit := false
	for _, u := range credits.Unlinked {
		overLimit = overLimit || u.Reason == unlinkedOverLimit
	}
	if !overLimit {
		t.Error("no over-limit unlinked organization")
	}
}

func issueIn(account APICreditAccount, issue string) bool {
	for _, have := range account.DataIssues {
		if have == issue {
			return true
		}
	}
	return false
}

// A settings.json whose reading has aged past its use still loads: its credit
// and refill date apply, and the reading is reported unused rather than the
// file refused. Time rules apply when saving, never when loading.
func TestAnOldReadingLoadsAndTheRestStillApplies(t *testing.T) {
	org := fakeOrg("a")
	entry := creditEntry("alpha", 0, org, "200", "")
	write := func(entry map[string]any) overrides.Values {
		dir := t.TempDir()
		raw, err := json.Marshal(map[string]any{"schema": 1, "revision": 3, "apiCredits": map[string]any{qc.APICreditOrgAccount(org): entry}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, overrides.FileName), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		values := overrides.Open(dir).Current()
		if values.Unreadable {
			t.Fatal("an aged reading made settings.json unreadable")
		}
		return values
	}
	readingAt := func(when string) map[string]any {
		at := instant(t, when)
		return map[string]any{
			"remainingUsd": "143.20", "at": when, "enteredAt": when, "organizationId": org,
			"baseline": map[string]any{"dayStart": qc.MeterDayStart(at).Format(time.RFC3339), "until": qc.MeterHourStart(at).Format(time.RFC3339), "usage": []any{}},
		}
	}
	in := Input{Snapshot: snapshotOf(entry), AllowEdit: true,
		Meter: meterOf(instant(t, "2026-10-09T11:59:00Z"), map[string]qc.MeterOrganization{org: healthyOrg(t)})}

	// Three days old, and before the refill this file itself sets.
	in.Overrides = write(map[string]any{"monthlyUsd": "300", "renews": "2026-10-07", "reading": readingAt("2026-10-06T10:00:00Z"), "rev": 3, "updatedAt": "2026-10-06T10:05:00Z"})
	got := buildCredits(t, in, instant(t, workedNow)).Accounts[0]
	if got.MonthlyCreditText != "$300.00" || got.MonthlyCreditSource != sourceDashboard || got.RenewsSource != sourceDashboard ||
		*got.CycleStartEpoch != instant(t, "2026-10-07T00:00:00Z").Unix() || got.Basis != basisCredit || got.Reading != nil ||
		got.Settings.ReadingUnusedReason != readingBeforeRefill || got.Settings.Reading == nil {
		t.Errorf("before the refill: %s", jsonOf(t, got))
	}

	// Forty days old with no refill date anywhere.
	in.Overrides = write(map[string]any{"monthlyUsd": "300", "reading": readingAt("2026-08-30T10:00:00Z"), "rev": 3, "updatedAt": "2026-08-30T10:05:00Z"})
	got = buildCredits(t, in, instant(t, workedNow)).Accounts[0]
	if got.MonthlyCreditText != "$300.00" || got.Settings.ReadingUnusedReason != readingTooOld || got.State != StateNeedsSettings {
		t.Errorf("too old: %s", jsonOf(t, got))
	}
}
