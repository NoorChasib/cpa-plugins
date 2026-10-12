package credits

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/settings"
)

// fakeOrg is one of the fixtures' fake organization ids, ...0a to ...0e.
func fakeOrg(last string) string { return "00000000-0000-4000-8000-00000000000" + last }

// creditEntry is quota-cache 0.1.14's entry for one configured organization:
// the configured facts and nothing else. It is never polled. onPage, which
// buildCredits applies, enters its credit and refill date on the page, where
// quota-cache keeps them now.
func creditEntry(label string, position int, org, monthly, renews string) client.Entry {
	return client.Entry{
		Provider:  client.ProviderAnthropicAPI,
		AuthIndex: client.APICreditOrgAccount(org),
		APICredit: &client.APICredit{Label: label, Position: position, MonthlyUSD: monthly, Renews: renews, OrganizationID: org},
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
func bucket(t *testing.T, start string, usage ...client.MeterUsage) client.MeterBucket {
	t.Helper()
	sorted := []client.MeterUsage{}
	for _, u := range usage {
		sorted = client.MergeMeterUsage(sorted, u)
	}
	return client.MeterBucket{Start: instant(t, start), Usage: sorted}
}

// meterOf is a meter flushed at flushed, counting the organizations given.
func meterOf(flushed time.Time, orgs map[string]client.MeterOrganization) *client.APIMeter {
	for id, org := range orgs {
		if org.Days == nil {
			org.Days = []client.MeterBucket{}
		}
		if org.Hours == nil {
			org.Hours = []client.MeterBucket{}
		}
		orgs[id] = org
	}
	return &client.APIMeter{
		Schema: client.MeterSchema, Since: flushed.Add(-60 * 24 * time.Hour), StartedAt: flushed.Add(-24 * time.Hour), FlushedAt: flushed,
		Gaps: []client.MeterGap{}, Organizations: orgs, Unlinked: []client.MeterUnlinked{}, Auths: map[string]client.MeterAuth{},
	}
}

// itemsOf is the item list that lists entries, as the publisher passes it.
func itemsOf(entries ...client.Entry) []Item {
	items := make([]Item, 0, len(entries))
	for _, entry := range entries {
		items = append(items, Item{ID: entry.AuthIndex, Credit: entry.APICredit})
	}
	return items
}

// buildCredits computes the card for in at now, with each item's credit and
// refill date entered on the page (onPage), and holds it to the sums the
// contract promises.
func buildCredits(t *testing.T, in Input, now time.Time) *client.CreditDocument {
	t.Helper()
	in.Now = now
	state := Compute(onPage(in))
	if state.APICredits == nil {
		t.Fatal("api_credits is null with items listed")
	}
	checkCreditPoolAddsUp(t, state.APICredits)
	return state.APICredits
}

// checkCreditPoolAddsUp holds every document to the invariants the contract
// promises, so a fixture cannot drift from them.
func checkCreditPoolAddsUp(t *testing.T, credits *client.CreditDocument) {
	t.Helper()
	pool := credits.Pool
	if pool.AccountCount != len(credits.Accounts) || pool.CountedCount+pool.MissingCount != pool.AccountCount {
		t.Errorf("counts do not add up: %+v over %d accounts", pool, len(credits.Accounts))
	}
	counted, lowerBound, estimate := 0, false, false
	for _, account := range credits.Accounts {
		wantCounted := account.State == client.CreditStateOK || account.State == client.CreditStateStale || account.State == client.CreditStateOut
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
		case client.CreditStateMisconfigured, client.CreditStateCacheTooOld, client.CreditStatePending:
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

func epoch(value int64) *int64 { return &value }

func jsonOf(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// workedInput is one of the named fixtures: the snapshot, the meter beside
// it, and the settings, with stale-after as Quota Glance applies it.
func workedInput(t *testing.T, name string) Input {
	t.Helper()
	in := inputOf(t, fixtureNamed(t, name))
	in.StaleAfter = 45 * time.Minute
	return in
}

const workedNow = "2026-10-09T12:00:00Z"

// The worked example, the api-credits fixture, field by field.
func TestAPICreditsWorkedExample(t *testing.T) {
	now := instant(t, workedNow)
	if now.Unix() != 1791547200 {
		t.Fatalf("now = %d", now.Unix())
	}
	credits := buildCredits(t, workedInput(t, "api-credits"), now)

	if credits.Title != "Monthly API Credit" || credits.Currency != "USD" ||
		credits.Pricing != (client.CreditPricing{AsOf: "2026-10-09", Source: "https://platform.claude.com/docs/en/about-claude/pricing", CacheWrites: "5m"}) ||
		credits.Editing != (client.CreditEditing{Available: true}) {
		t.Errorf("card: %s", jsonOf(t, credits))
	}
	wantMeter := &client.CreditMeter{
		SinceEpoch: instant(t, "2026-09-01T00:00:00Z").Unix(), UpdatedAtEpoch: instant(t, "2026-10-09T11:59:00Z").Unix(),
		Gaps: []client.CreditGap{},
	}
	if !reflect.DeepEqual(credits.Meter, wantMeter) {
		t.Errorf("meter:\n got %s\nwant %s", jsonOf(t, credits.Meter), jsonOf(t, wantMeter))
	}

	alpha := client.CreditAccount{
		ID: client.APICreditOrgAccount(fakeOrg("a")), Label: "alpha", Order: 0, OrganizationID: fakeOrg("a"),
		State: client.CreditStateOK, Counted: true, HasEstimate: true, Basis: client.CreditBasisCredit,
		MonthlyCredit: 200, MonthlyCreditText: "$200.00", MonthlyCreditSource: client.CreditSourceDashboard,
		Spent: 55, SpentText: "$55.00", SpentSinceEpoch: epoch(instant(t, "2026-09-29T00:00:00Z").Unix()),
		Used: 55, UsedText: "$55.00", Left: 145, LeftText: "$145.00", Overage: 0, OverageText: "$0.00",
		RemainingFraction: 0.725, RemainingPercent: 73, Level: client.LevelOK,
		CycleStartEpoch: epoch(instant(t, "2026-09-29T00:00:00Z").Unix()), RenewsAtEpoch: epoch(1793232000), RenewsInSeconds: epoch(1684800),
		RenewsSource:    client.CreditSourceDashboard,
		CacheWriteExtra: 6, CacheWriteExtraText: "$6.00",
		Unpriced:        []client.CreditUnpriced{},
		MeterSinceEpoch: epoch(instant(t, "2026-09-20T00:00:00Z").Unix()), LastSeenEpoch: epoch(instant(t, "2026-10-01T18:20:00Z").Unix()),
		DataIssues: []string{},
		Settings: client.CreditSettings{
			Editable: true, Revision: "2", MonthlyUSD: "200", Renews: "2026-10-29", UpdatedAtEpoch: epoch(pageEnteredAt.Unix()),
		},
	}
	if got := credits.Accounts[0]; !reflect.DeepEqual(got, alpha) {
		t.Errorf("alpha:\n got %s\nwant %s", jsonOf(t, got), jsonOf(t, alpha))
	}

	team := client.CreditAccount{
		ID: client.APICreditOrgAccount(fakeOrg("b")), Label: "team", Order: 1, OrganizationID: fakeOrg("b"),
		State: client.CreditStateOK, Counted: true, HasEstimate: true, Basis: client.CreditBasisReading,
		MonthlyCredit: 300, MonthlyCreditText: "$300.00", MonthlyCreditSource: client.CreditSourceDashboard,
		Spent: 140, SpentText: "$140.00", SpentSinceEpoch: epoch(instant(t, "2026-09-15T00:00:00Z").Unix()),
		Used: 194, UsedText: "$194.00", Left: 106, LeftText: "$106.00", Overage: 0, OverageText: "$0.00",
		RemainingFraction: 106.0 / 300, RemainingPercent: 35, Level: client.LevelLow,
		CycleStartEpoch: epoch(instant(t, "2026-09-15T00:00:00Z").Unix()), RenewsAtEpoch: epoch(1792022400), RenewsInSeconds: epoch(475200),
		RenewsSource: client.CreditSourceDashboard,
		Reading: &client.CreditReading{
			Remaining: 120, RemainingText: "$120.00",
			AtEpoch: instant(t, "2026-10-08T15:40:00Z").Unix(), EnteredAtEpoch: instant(t, "2026-10-08T15:44:00Z").Unix(),
			SpentSince: 14, SpentSinceText: "$14.00",
		},
		Unpriced:        []client.CreditUnpriced{},
		MeterSinceEpoch: epoch(instant(t, "2026-09-01T00:00:00Z").Unix()), LastSeenEpoch: epoch(instant(t, "2026-10-08T16:30:00Z").Unix()),
		DataIssues: []string{},
		Settings: client.CreditSettings{
			Editable: true, Revision: "2", MonthlyUSD: "300", Renews: "2026-10-15",
			Reading:        &client.StoredReading{RemainingUSD: "120.00", AtEpoch: instant(t, "2026-10-08T15:40:00Z").Unix(), EnteredAtEpoch: instant(t, "2026-10-08T15:44:00Z").Unix()},
			UpdatedAtEpoch: epoch(instant(t, "2026-10-08T15:44:00Z").Unix()),
		},
	}
	if got := credits.Accounts[1]; !reflect.DeepEqual(got, team) {
		t.Errorf("team:\n got %s\nwant %s", jsonOf(t, got), jsonOf(t, team))
	}

	charlie := credits.Accounts[2]
	if charlie.Label != "charlie" || charlie.State != client.CreditStateOut || !charlie.Counted || charlie.Basis != client.CreditBasisCredit ||
		charlie.Spent != 165 || charlie.Left != 0 || charlie.LeftText != "$0.00" || charlie.Used != 200 || charlie.UsedText != "$200.00" ||
		charlie.EstimateLeftText != "$35.00" || charlie.RemainingFraction != 0 || charlie.RemainingPercent != 0 || charlie.Level != client.LevelCritical ||
		!reflect.DeepEqual(charlie.DataIssues, []string{client.CreditIssueRefused}) ||
		charlie.Issue != "Anthropic refused a request for low credit, so this credit is spent. The estimate had $35.00 left; enter a Console reading to correct it." ||
		charlie.Refusals.Total != 3 || *charlie.Refusals.LastAtEpoch != instant(t, "2026-10-09T10:05:00Z").Unix() ||
		charlie.Refusals.ClaudeCodeTotal != 0 || charlie.Refusals.ClaudeCodeLastAtEpoch != nil {
		t.Errorf("charlie: %s", jsonOf(t, charlie))
	}

	delta := credits.Accounts[3]
	if delta.Label != "delta" || delta.State != client.CreditStateNeedsSettings || delta.Counted || delta.HasEstimate || delta.Basis != "" ||
		delta.SpentText != "$50.45" || *delta.SpentSinceEpoch != instant(t, "2026-10-02T00:00:00Z").Unix() || !delta.LowerBound ||
		delta.LeftText != "" || delta.UsedText != "" || delta.MonthlyCreditText != "" || delta.MonthlyCreditSource != client.CreditSourceNone ||
		!reflect.DeepEqual(delta.Unpriced, []client.CreditUnpriced{{Model: "claude-mythos-preview", Tokens: 1000000}}) ||
		!reflect.DeepEqual(delta.DataIssues, []string{client.CreditIssueNeedsCredit, client.CreditIssueUnpricedModel}) ||
		delta.Issue != "Set this organization's monthly credit to count it in the total." {
		t.Errorf("delta: %s", jsonOf(t, delta))
	}

	echo := credits.Accounts[4]
	if echo.Label != "echo" || echo.State != client.CreditStateOK || !echo.Counted || echo.Basis != client.CreditBasisCredit || !echo.LowerBound ||
		echo.LeftText != "$200.00" || echo.UsedText != "$0.00" || echo.SpentText != "$0.00" || echo.RemainingPercent != 100 || echo.Level != client.LevelOK ||
		echo.LastSeenEpoch != nil ||
		!reflect.DeepEqual(echo.DataIssues, []string{client.CreditIssueMeterStartedLate, client.CreditIssueNoTraffic}) ||
		echo.Issue != "Counting began after this cycle started, so earlier spend is missing. Enter a Console reading to correct it." {
		t.Errorf("echo: %s", jsonOf(t, echo))
	}

	wantPool := client.CreditPool{
		HasEstimate: true, LowerBound: true,
		MonthlyCredit: 900, MonthlyCreditText: "$900.00",
		Used: 449, UsedText: "$449.00", Left: 451, LeftText: "$451.00", Overage: 0, OverageText: "$0.00",
		RemainingFraction: 451.0 / 900, RemainingPercent: 50, Level: client.LevelOK,
		NextRefill: &client.CreditRefill{
			AccountIDs:    []string{client.APICreditOrgAccount(fakeOrg("b"))},
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

	if want := []client.CreditGap{{
		FromEpoch: instant(t, "2026-10-09T06:00:00Z").Unix(), ToEpoch: instant(t, "2026-10-09T07:30:00Z").Unix(), Reason: client.MeterStopDisabled,
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
			if !after.LowerBound || !strings.Contains(strings.Join(after.DataIssues, ","), client.CreditIssueMeterGap) {
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
	entry   client.Entry
	org     client.MeterOrganization
	meter   func(*client.APIMeter)
	stored  *settings.APICredit
	now     string
	stale   time.Duration
	noMeter bool
	noOrg   bool
}

func (c creditCase) build(t *testing.T) client.CreditAccount {
	t.Helper()
	now := instant(t, workedNow)
	if c.now != "" {
		now = instant(t, c.now)
	}
	in := Input{Items: itemsOf(c.entry), StaleAfter: c.stale,
		Settings: settings.Values{APICredits: map[string]settings.APICredit{}, Renewals: map[string]settings.Renewal{}}}
	if c.stored != nil {
		in.Settings.APICredits[c.entry.AuthIndex] = *c.stored
	}
	if !c.noMeter {
		orgs := map[string]client.MeterOrganization{}
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
func healthyOrg(t *testing.T) client.MeterOrganization {
	return client.MeterOrganization{
		Since:      instant(t, "2026-09-01T00:00:00Z"),
		LastSeenAt: ptrAt(t, "2026-10-01T12:00:00Z"), LastSuccessAt: ptrAt(t, "2026-10-01T12:00:00Z"),
		Days: []client.MeterBucket{bucket(t, "2026-10-01T00:00:00Z", client.MeterUsage{Model: "claude-sonnet-5-5", Requests: 10, Input: 10_000_000})},
	}
}

// Every row of the state table, first match winning, with its sentence.
func TestAPICreditStates(t *testing.T) {
	org := fakeOrg("a")
	healthy := func() client.Entry { return creditEntry("alpha", 0, org, "200", "2026-10-29") }
	for _, tc := range []struct {
		name  string
		c     creditCase
		state string
		issue string
	}{
		{"ok", creditCase{entry: healthy(), org: healthyOrg(t)}, client.CreditStateOK, ""},
		{"misconfigured", creditCase{entry: func() client.Entry {
			e := healthy()
			e.APICredit.Problem = client.CreditProblemLabelDuplicate
			return e
		}(), org: healthyOrg(t)}, client.CreditStateMisconfigured, "Another item already uses this label."},
		{"no configuration at all", creditCase{entry: func() client.Entry {
			e := healthy()
			e.APICredit = nil
			return e
		}()}, client.CreditStateMisconfigured, "This item's configuration has a problem."},
		{"written by quota-cache 0.1.13", creditCase{entry: client.Entry{
			Provider: client.ProviderAnthropicAPI, AuthIndex: client.APICreditAccount("alpha"),
			APICredit: &client.APICredit{Label: "alpha", MonthlyUSD: "200", Renews: "2026-10-29", KeyFingerprint: "key-000000000001"},
		}}, client.CreditStateCacheTooOld, "Quota Cache 0.1.13 reads Anthropic's cost report, which Quota Glance no longer uses. Update Quota Cache to 0.1.14 and add this organization's organization-id."},
		{"no meter file", creditCase{entry: healthy(), noMeter: true}, client.CreditStatePending,
			"Quota Cache has not saved its API meter yet; counting starts when it next saves."},
		{"not in the meter yet", creditCase{entry: healthy(), noOrg: true}, client.CreditStatePending, "Counting starts at Quota Cache's next save."},
		{"dormant in the meter", creditCase{entry: healthy(), org: func() client.MeterOrganization {
			o := healthyOrg(t)
			o.UnlinkedAt = ptrAt(t, "2026-10-09T08:00:00Z")
			return o
		}()}, client.CreditStatePending, "Counting starts at Quota Cache's next save."},
		{"no credit", creditCase{entry: creditEntry("alpha", 0, org, "", "2026-10-29"), org: healthyOrg(t)},
			client.CreditStateNeedsSettings, "Set this organization's monthly credit to count it in the total."},
		{"no refill date", creditCase{entry: creditEntry("alpha", 0, org, "200", ""), org: healthyOrg(t)},
			client.CreditStateNeedsSettings, "Set the refill date, or enter a Console reading."},
		{"neither", creditCase{entry: creditEntry("alpha", 0, org, "", ""), org: healthyOrg(t)},
			client.CreditStateNeedsSettings, "Set this organization's monthly credit and refill date."},
		{"out", creditCase{entry: healthy(), org: func() client.MeterOrganization {
			o := healthyOrg(t)
			o.Refusals, o.LastRefusalAt = 1, ptrAt(t, "2026-10-09T10:00:00Z")
			return o
		}()}, client.CreditStateOut, "Anthropic refused a request for low credit, so this credit is spent. The estimate had $180.00 left; enter a Console reading to correct it."},
		{"stale", creditCase{entry: healthy(), org: healthyOrg(t), stale: 45 * time.Minute, meter: func(m *client.APIMeter) {
			m.FlushedAt = instant(t, "2026-10-09T11:00:00Z")
		}}, client.CreditStateStale, "Quota Cache has not saved its meter recently, so recent spend may be missing."},
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
		client.CreditProblemItemInvalid:             "This item in claude-api-credits is not a list of label, organization-id, monthly-usd and renews.",
		client.CreditProblemUnknownField:            "This item has a setting Quota Cache does not know; check the spelling of label, organization-id, monthly-usd and renews.",
		client.CreditProblemTooManyItems:            "Only the first 16 items in claude-api-credits are read.",
		client.CreditProblemLabelMissing:            "Give this item a label, unique and at most 64 characters.",
		client.CreditProblemLabelDuplicate:          "Another item already uses this label.",
		client.CreditProblemOrganizationIDMissing:   "Add this organization's organization-id, from Console under Settings, Organization.",
		client.CreditProblemOrganizationIDInvalid:   "organization-id must be the Organization ID from Console, like 12345678-1234-5678-1234-567812345678.",
		client.CreditProblemOrganizationIDDuplicate: "Another item already uses this organization-id.",
		client.CreditProblemAdminKeyMissing:         "This item's configuration has a problem.",
		client.CreditProblemRenewsInvalid:           "This item's configuration has a problem.",
		"from_a_newer_quota_cache":                  "This item's configuration has a problem.",
	} {
		if got := problemIssueOf(&client.APICredit{Problem: problem}); got != want {
			t.Errorf("%s: %q", problem, got)
		}
	}
	if !strings.HasPrefix(problemIssueOf(&client.APICredit{Problem: client.CreditProblemLabelInvalid}), "This label is over 64 characters") {
		t.Error("label_invalid lost its sentence")
	}
}

// A misconfigured or pending account shows no amounts at all, and carries
// only the flags its own state gives.
func TestAccountsWithoutAnEstimateCarryOnlyTheirFlags(t *testing.T) {
	e := creditEntry("alpha", 0, fakeOrg("a"), "", "")
	got := creditCase{entry: e, noOrg: true}.build(t)
	want := []string{client.CreditIssueOrgNotCounted, client.CreditIssueNeedsCredit, client.CreditIssueNeedsRefillDate}
	if !reflect.DeepEqual(got.DataIssues, want) || got.SpentText != "" || got.MonthlyCreditText != "" {
		t.Errorf("pending: %s", jsonOf(t, got))
	}
	e.APICredit.Problem = client.CreditProblemLabelMissing
	got = creditCase{entry: e, org: healthyOrg(t)}.build(t)
	if !reflect.DeepEqual(got.DataIssues, []string{client.CreditIssueMisconfigured}) || got.SpentText != "" {
		t.Errorf("misconfigured: %s", jsonOf(t, got))
	}
}

// storedReading is a dashboard reading of amount at at, with the baseline the
// meter would have given it.
func storedReading(t *testing.T, org, amount, at string, baseline ...client.MeterUsage) *settings.APICredit {
	t.Helper()
	when := instant(t, at)
	if baseline == nil {
		baseline = []client.MeterUsage{}
	}
	return &settings.APICredit{Rev: 1, UpdatedAt: when, Reading: &settings.Reading{
		RemainingUSD: amount, At: when, EnteredAt: when, OrganizationID: org,
		Baseline: settings.Baseline{DayStart: client.MeterDayStart(when), Until: client.MeterHourStart(when), Usage: baseline},
	}}
}

// A reading is used only while it can describe this cycle of this
// organization, and says why it is not otherwise.
func TestReadingRules(t *testing.T) {
	org := fakeOrg("a")
	usedOn := func(stored *settings.APICredit, renews string) client.CreditAccount {
		return creditCase{entry: creditEntry("alpha", 0, org, "200", renews), org: healthyOrg(t), stored: stored}.build(t)
	}
	// Used: the day's usage less the baseline is the spend since.
	o := healthyOrg(t)
	o.Days = append(o.Days, bucket(t, "2026-10-09T00:00:00Z", client.MeterUsage{Model: "claude-sonnet-5-5", Input: 3_000_000}))
	used := creditCase{entry: creditEntry("alpha", 0, org, "200", "2026-10-29"), org: o,
		stored: storedReading(t, org, "150.00", "2026-10-09T09:30:00Z", client.MeterUsage{Model: "claude-sonnet-5-5", Input: 1_000_000})}.build(t)
	if used.Basis != client.CreditBasisReading || used.Reading == nil || used.Reading.SpentSinceText != "$4.00" || used.LeftText != "$146.00" ||
		used.UsedText != "$54.00" || used.SpentText != "$26.00" || used.Settings.ReadingUnusedReason != "" {
		t.Errorf("used: %s", jsonOf(t, used))
	}
	for _, tc := range []struct {
		name, renews, reason, sentence string
		stored                         *settings.APICredit
	}{
		{"before the refill", "2026-10-05", client.CreditReadingBeforeRefill,
			"Your Console reading of $150.00 on Oct 3 was before the last refill, so it is not used.",
			storedReading(t, org, "150.00", "2026-10-03T09:00:00Z")},
		{"another organization", "2026-10-05", client.CreditReadingOtherOrganization,
			"Your Console reading was for a different organization-id, so it is not used.",
			storedReading(t, fakeOrg("b"), "150.00", "2026-10-09T09:00:00Z")},
		{"too old without a refill date", "", client.CreditReadingTooOld,
			"Your Console reading is over 31 days old and there is no refill date, so it is not used.",
			storedReading(t, org, "150.00", "2026-09-01T09:00:00Z")},
		{"in the future", "2026-10-05", client.CreditReadingFuture,
			"Your Console reading is dated in the future, so it is not used.",
			storedReading(t, org, "150.00", "2026-10-09T13:00:00Z")},
	} {
		got := usedOn(tc.stored, tc.renews)
		if got.Reading != nil || got.Settings.ReadingUnusedReason != tc.reason || got.Settings.Reading == nil ||
			!strings.Contains(strings.Join(got.DataIssues, ","), client.CreditIssueReadingUnused) {
			t.Errorf("%s: %s", tc.name, jsonOf(t, got))
		}
		if sentence := readingUnusedSentenceOf(got.Settings.ReadingUnusedReason, got.Settings.Reading); sentence != tc.sentence {
			t.Errorf("%s: %q", tc.name, sentence)
		}
	}
	// Taken on the refill day: used, and flagged, since Console may not have
	// shown the new credit yet.
	refillDay := usedOn(storedReading(t, org, "190.00", "2026-10-05T08:00:00Z"), "2026-10-05")
	if refillDay.Basis != client.CreditBasisReading || !reflect.DeepEqual(refillDay.DataIssues, []string{client.CreditIssueReadingOnRefillDay}) ||
		refillDay.Issue != "This Console reading was taken on the refill day. If Console did not show the new credit yet, enter a new reading once it does." {
		t.Errorf("refill day: %s", jsonOf(t, refillDay))
	}
	// A day later it is not.
	if next := usedOn(storedReading(t, org, "190.00", "2026-10-06T00:00:00Z"), "2026-10-05"); len(next.DataIssues) != 0 {
		t.Errorf("the day after the refill: %v", next.DataIssues)
	}
	// Without a refill date a recent reading is the basis on its own.
	alone := usedOn(storedReading(t, org, "190.00", "2026-10-09T09:00:00Z"), "")
	if alone.Basis != client.CreditBasisReading || alone.State != client.CreditStateOK || *alone.SpentSinceEpoch != instant(t, "2026-10-09T09:00:00Z").Unix() ||
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
	if got.State != client.CreditStateOK || got.LeftText != "$180.00" || got.EstimateLeftText != "" || got.Refusals.Total != 2 {
		t.Errorf("%s", jsonOf(t, got))
	}
	// With nothing left in the estimate there is nothing to correct, and the
	// sentence stops at the refusal.
	spent := healthyOrg(t)
	spent.Refusals, spent.LastRefusalAt = 1, ptrAt(t, "2026-10-09T10:00:00Z")
	if got := (creditCase{entry: creditEntry("alpha", 0, fakeOrg("a"), "20", "2026-10-29"), org: spent}).build(t); got.State != client.CreditStateOut ||
		got.EstimateLeftText != "$0.00" || got.Issue != "Anthropic refused a request for low credit, so this credit is spent." {
		t.Errorf("refused with nothing left: %s", jsonOf(t, got))
	}
	// A refusal from before the anchor is from another cycle.
	o.LastSuccessAt, o.LastRefusalAt = ptrAt(t, "2026-09-01T00:00:00Z"), ptrAt(t, "2026-09-20T00:00:00Z")
	if got := (creditCase{entry: creditEntry("alpha", 0, fakeOrg("a"), "200", "2026-10-29"), org: o}).build(t); got.State != client.CreditStateOK {
		t.Errorf("an old refusal: %s", got.State)
	}
}

// A refusal of a Claude Code-based client's request is out only when the
// estimate was already nearly spent: Claude Code is not covered by the credit,
// the Agent SDK is.
func TestClaudeCodeRefusals(t *testing.T) {
	org := fakeOrg("a")
	// spentInput Sonnet 5.5 input tokens cost $2.00 a million.
	refusedOn := func(monthly string, spentInput uint64) client.CreditAccount {
		o := healthyOrg(t)
		o.Days = []client.MeterBucket{bucket(t, "2026-10-01T00:00:00Z", client.MeterUsage{Model: "claude-sonnet-5-5", Input: spentInput})}
		o.ClaudeCodeRefusals, o.LastClaudeCodeRefusalAt = 4, ptrAt(t, "2026-10-09T10:00:00Z")
		return creditCase{entry: creditEntry("alpha", 0, org, monthly, "2026-10-29"), org: o}.build(t)
	}
	// $20.00 spent of $20.00: nothing left, so out.
	spent := refusedOn("20", 10_000_000)
	if spent.State != client.CreditStateOut || spent.LeftText != "$0.00" || spent.Level != client.LevelCritical ||
		!reflect.DeepEqual(spent.DataIssues, []string{client.CreditIssueRefusedNearlySpent}) ||
		spent.Issue != "Anthropic refused requests from a Claude Code-based client for low credit, and the estimate is nearly spent, so this credit is shown as spent. Enter a Console reading to check." ||
		spent.Refusals.ClaudeCodeTotal != 4 {
		t.Errorf("spent: %s", jsonOf(t, spent))
	}
	// $20.00 of $40.00: half left, so not out.
	half := refusedOn("40", 10_000_000)
	if half.State != client.CreditStateOK || half.LeftText != "$20.00" || half.RemainingPercent != 50 ||
		!reflect.DeepEqual(half.DataIssues, []string{client.CreditIssueClaudeCodeRefused}) ||
		half.Issue != "Anthropic refused requests from a Claude Code-based client (Claude Code or the Agent SDK) for low credit. If they were Agent SDK requests, this credit may be spent; enter a Console reading." {
		t.Errorf("half: %s", jsonOf(t, half))
	}
	// Exactly a tenth left, $20.00 of $200.00, is nearly spent; a millionth
	// of a dollar more is not.
	if tenth := refusedOn("200", 90_000_000); tenth.State != client.CreditStateOut || tenth.EstimateLeftText != "$20.00" {
		t.Errorf("a tenth left: %s", jsonOf(t, tenth))
	}
	if over := refusedOn("200", 89_999_999); over.State != client.CreditStateOK {
		t.Errorf("just over a tenth left: %s", over.State)
	}
	// A refusal from before the anchor is from another cycle, though no
	// success came after it.
	old := healthyOrg(t)
	old.LastSuccessAt, old.ClaudeCodeRefusals, old.LastClaudeCodeRefusalAt = ptrAt(t, "2026-09-01T00:00:00Z"), 4, ptrAt(t, "2026-09-20T00:00:00Z")
	if got := (creditCase{entry: creditEntry("alpha", 0, org, "200", "2026-10-29"), org: old}).build(t); got.State != client.CreditStateOK ||
		len(got.DataIssues) != 0 || got.Issue != "" || got.Refusals.ClaudeCodeTotal != 4 {
		t.Errorf("an old refusal: %s", jsonOf(t, got))
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
		set      func(*client.APIMeter, *client.MeterOrganization, *time.Time)
	}{
		{client.CreditIssueMeterStartedLate, "Counting began after this cycle started, so earlier spend is missing. Enter a Console reading to correct it.",
			func(_ *client.APIMeter, o *client.MeterOrganization, at *time.Time) { o.Since = *at }},
		{client.CreditIssueMeterGap, "Quota Cache was not counting for part of this period, for example while it was off or reloading, so some spend may be missing. Enter a Console reading to correct it.",
			func(m *client.APIMeter, _ *client.MeterOrganization, at *time.Time) {
				m.Gaps = []client.MeterGap{{From: at.Add(-time.Hour), To: *at, Reason: client.MeterStopShutdown}}
			}},
		{client.CreditIssueMeterDropped, "Quota Cache dropped usage records it could not keep up with, so some spend is missing.",
			func(m *client.APIMeter, _ *client.MeterOrganization, at *time.Time) {
				m.Dropped, m.LastDroppedAt = 9, at
			}},
		{client.CreditIssueMeterFull, "Quota Cache's meter was full, so some spend is missing.",
			func(_ *client.APIMeter, o *client.MeterOrganization, at *time.Time) { o.LastOverflowAt = at }},
		{client.CreditIssueMeterUnattributed, "Some failed requests could not be matched to an organization, so some spend may be missing.",
			func(m *client.APIMeter, _ *client.MeterOrganization, at *time.Time) {
				m.Unattributed, m.LastUnattributedAt = 2, at
			}},
		{client.CreditIssueMeterRejected, "Quota Cache could not read some usage records from CPA, so some spend may be missing.",
			func(m *client.APIMeter, _ *client.MeterOrganization, at *time.Time) {
				m.Rejected, m.LastRejectedAt = 1, at
			}},
	} {
		for _, at := range []*time.Time{before, after} {
			o := healthyOrg(t)
			if tc.issue == client.CreditIssueMeterStartedLate {
				o.Days = nil
			}
			got := creditCase{entry: entry, org: o, meter: func(m *client.APIMeter) {
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
		got := creditCase{entry: entry, org: healthyOrg(t), meter: func(m *client.APIMeter) {
			m.StoppedAt, m.StopReason = ptrAt(t, tc.stopped), client.MeterStopQuiesce
		}}.build(t)
		if got.LowerBound != tc.flagged {
			t.Errorf("stopped at %s: lowerBound %v issues %v", tc.stopped, got.LowerBound, got.DataIssues)
		}
	}
	// A brief gap is ignored wherever it falls; a long one counts.
	for _, tc := range []struct {
		length  time.Duration
		flagged bool
	}{{client.MeterBriefGap - time.Second, false}, {client.MeterBriefGap, true}} {
		got := creditCase{entry: entry, org: healthyOrg(t), meter: func(m *client.APIMeter) {
			from := instant(t, "2026-10-09T09:00:00Z")
			m.Gaps = []client.MeterGap{{From: from, To: from.Add(tc.length), Reason: client.MeterStopShutdown}}
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
	if a := (&creditAccount{issues: map[string]bool{}, out: none}); a.sentenceOf(client.CreditIssueMeterStartedLate, nil) != "Counting began recently, so earlier spend is missing." {
		t.Errorf("no basis: %q", a.sentenceOf(client.CreditIssueMeterStartedLate, nil))
	}
}

// A model the table has no price for is left out of every amount and listed;
// (other) is left out and never listed.
func TestUnpricedModelsAndOther(t *testing.T) {
	o := healthyOrg(t)
	o.Days = append(o.Days, bucket(t, "2026-10-02T00:00:00Z",
		client.MeterUsage{Model: "claude-mythos-preview", Input: 1_000_000},
		client.MeterUsage{Model: "claude-3-7-sonnet", Input: 250_000},
		client.MeterUsage{Model: "claude-3-haiku", Output: 900},
		client.MeterUsage{Model: "made-up-model", Input: 10},
		client.MeterUsage{Model: client.MeterOtherModel, Input: 50_000_000},
	))
	got := creditCase{entry: creditEntry("alpha", 0, fakeOrg("a"), "200", "2026-10-29"), org: o}.build(t)
	if got.SpentText != "$20.00" || !got.LowerBound || len(got.Unpriced) != 4 || got.Unpriced[0].Model != "claude-mythos-preview" ||
		got.Issue != "Some requests used a model with no listed price and are left out: claude-mythos-preview (1.0M tokens), claude-3-7-sonnet (250K tokens), claude-3-haiku (900 tokens) and 1 more." {
		t.Errorf("%s", jsonOf(t, got))
	}
	only := healthyOrg(t)
	only.Days = append(only.Days, bucket(t, "2026-10-02T00:00:00Z", client.MeterUsage{Model: client.MeterOtherModel, Input: 50_000_000}))
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
		org: client.MeterOrganization{Since: instant(t, "2026-09-01T00:00:00Z")}}.build(t)
	if got.LowerBound || got.LeftText != "$200.00" || !reflect.DeepEqual(got.DataIssues, []string{client.CreditIssueNoTraffic}) ||
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
		o.Days = []client.MeterBucket{bucket(t, "2026-10-01T00:00:00Z", client.MeterUsage{Model: "claude-sonnet-5-5", CacheWrite: tc.writes})}
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
	if got.State != client.CreditStateOK || got.Level != "" || got.RemainingFraction != 0 || got.RemainingPercent != 0 ||
		got.LeftText != "$0.00" || got.OverageText != "$20.00" || got.UsedText != "$0.00" ||
		!reflect.DeepEqual(got.DataIssues, []string{client.CreditIssueZeroCredit, client.CreditIssueOverCredit}) ||
		got.Issue != "This organization's monthly credit is set to $0.00." {
		t.Errorf("%s", jsonOf(t, got))
	}
}

// Spend past the credit without a refusal: likely purchased credit.
func TestOverCreditWithoutARefusal(t *testing.T) {
	got := creditCase{entry: creditEntry("alpha", 0, fakeOrg("a"), "15", "2026-10-29"), org: healthyOrg(t)}.build(t)
	if got.State != client.CreditStateOK || got.LeftText != "$0.00" || got.UsedText != "$15.00" || got.OverageText != "$5.00" || got.Level != client.LevelCritical ||
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
		!reflect.DeepEqual(got.DataIssues, []string{client.CreditIssueReadingAboveCredit}) ||
		got.Issue != "Your Console reading is more than the monthly credit; check the monthly credit." {
		t.Errorf("%s", jsonOf(t, got))
	}
	// A reading of the whole credit, as the Console shows before anything of
	// a new cycle is spent, is not above it.
	full := creditCase{entry: creditEntry("alpha", 0, org, "200", "2026-10-29"), org: healthyOrg(t),
		stored: storedReading(t, org, "200.00", "2026-10-09T09:00:00Z")}.build(t)
	if full.RemainingPercent != 100 || full.LeftText != "$200.00" || len(full.DataIssues) != 0 || full.Issue != "" {
		t.Errorf("a reading of the whole credit: %s", jsonOf(t, full))
	}
}

// The values entered on the page are the only ones, field by field: a credit
// with no refill date has no cycle, a refill date with no credit counts
// nothing, and with nothing stored both are none.
func TestThePagesValuesApplyFieldByField(t *testing.T) {
	org := fakeOrg("a")
	entry := creditEntry("alpha", 0, org, "", "")
	stored := &settings.APICredit{MonthlyUSD: "300", Rev: 4, UpdatedAt: instant(t, "2026-10-08T00:00:00Z")}
	got := creditCase{entry: entry, org: healthyOrg(t), stored: stored}.build(t)
	if got.MonthlyCreditText != "$300.00" || got.MonthlyCreditSource != client.CreditSourceDashboard || got.RenewsSource != client.CreditSourceNone ||
		got.CycleStartEpoch != nil || got.State != client.CreditStateNeedsSettings || got.Settings.MonthlyUSD != "300" || got.Settings.Renews != "" ||
		got.Settings.Revision != "4" || *got.Settings.UpdatedAtEpoch != instant(t, "2026-10-08T00:00:00Z").Unix() {
		t.Errorf("a credit: %s", jsonOf(t, got))
	}
	stored = &settings.APICredit{Renews: "2026-10-05", Rev: 5, UpdatedAt: instant(t, "2026-10-08T00:00:00Z")}
	got = creditCase{entry: entry, org: healthyOrg(t), stored: stored}.build(t)
	if got.MonthlyCreditSource != client.CreditSourceNone || got.RenewsSource != client.CreditSourceDashboard || got.MonthlyCreditText != "" ||
		*got.CycleStartEpoch != instant(t, "2026-10-05T00:00:00Z").Unix() || got.SpentText != "$0.00" || got.State != client.CreditStateNeedsSettings {
		t.Errorf("a refill date: %s", jsonOf(t, got))
	}
	got = creditCase{entry: entry, org: healthyOrg(t)}.build(t)
	if got.MonthlyCreditSource != client.CreditSourceNone || got.RenewsSource != client.CreditSourceNone ||
		got.Settings.Revision != "" || got.Settings.UpdatedAtEpoch != nil {
		t.Errorf("nothing stored: %s", jsonOf(t, got))
	}
}

// Who may edit what, and why not.
func TestNotEditableReasons(t *testing.T) {
	org := fakeOrg("a")
	itemOf := func(problem string) client.Entry {
		e := creditEntry("alpha", 0, org, "200", "2026-10-29")
		e.AuthIndex, e.APICredit.Problem = "item-1", problem
		return e
	}
	tooOld := client.Entry{Provider: client.ProviderAnthropicAPI, AuthIndex: client.APICreditAccount("alpha"),
		APICredit: &client.APICredit{Label: "alpha", MonthlyUSD: "200", Renews: "2026-10-29", KeyFingerprint: "key-000000000001"}}
	misconfigured := creditEntry("alpha", 0, org, "200", "2026-10-29")
	misconfigured.APICredit.Problem = client.CreditProblemLabelInvalid
	for name, tc := range map[string]struct {
		entry    client.Entry
		editable bool
		reason   string
	}{
		"linked":                   {creditEntry("alpha", 0, org, "200", "2026-10-29"), true, ""},
		"misconfigured but linked": {misconfigured, true, ""},
		"past the sixteenth":       {itemOf(client.CreditProblemTooManyItems), false, client.CreditNotEditableOverLimit},
		"a duplicate":              {itemOf(client.CreditProblemOrganizationIDDuplicate), false, client.CreditNotEditableDuplicateOrganization},
		"no organization":          {itemOf(client.CreditProblemOrganizationIDMissing), false, client.CreditNotEditableNoOrganization},
		"written by 0.1.13":        {tooOld, false, client.CreditNotEditableCacheTooOld},
	} {
		got := creditCase{entry: tc.entry, org: healthyOrg(t)}.build(t)
		if got.Settings.Editable != tc.editable || got.Settings.NotEditableReason != tc.reason {
			t.Errorf("%s: editable %v reason %q", name, got.Settings.Editable, got.Settings.NotEditableReason)
		}
	}
	// The card's own reason, when editing is unavailable altogether.
	entry := creditEntry("alpha", 0, org, "200", "2026-10-29")
	in := Input{Items: itemsOf(entry), Meter: meterOf(instant(t, "2026-10-09T11:59:00Z"), map[string]client.MeterOrganization{org: healthyOrg(t)})}
	in.Settings = settings.Values{Unreadable: true}
	unreadable := buildCredits(t, in, instant(t, workedNow))
	if unreadable.Editing != (client.CreditEditing{Reason: "settingsUnreadable"}) || unreadable.Accounts[0].Settings.NotEditableReason != "settingsUnreadable" {
		t.Errorf("unreadable %+v %+v", unreadable.Editing, unreadable.Accounts[0].Settings)
	}
}

// Stored values apply only while the settings can be read: an unreadable
// settings file leaves no credit and no refill date in force.
func TestStoredValuesApplyOnlyWhileReadable(t *testing.T) {
	org := fakeOrg("a")
	entry := creditEntry("alpha", 0, org, "", "")
	values := settings.Values{APICredits: map[string]settings.APICredit{
		entry.AuthIndex: {MonthlyUSD: "300", Rev: 1, UpdatedAt: instant(t, "2026-10-08T00:00:00Z")},
	}}
	in := Input{Items: itemsOf(entry), Settings: values,
		Meter: meterOf(instant(t, "2026-10-09T11:59:00Z"), map[string]client.MeterOrganization{org: healthyOrg(t)})}
	if got := buildCredits(t, in, instant(t, workedNow)).Accounts[0]; got.MonthlyCreditText != "$300.00" || !got.Settings.Editable {
		t.Errorf("readable: %s", jsonOf(t, got))
	}
	in.SettingsState = client.CreditSettingsUnreadable
	if got := buildCredits(t, in, instant(t, workedNow)).Accounts[0]; got.MonthlyCreditText != "" || got.MonthlyCreditSource != client.CreditSourceNone ||
		got.State != client.CreditStateNeedsSettings || got.Settings.Editable || got.Settings.MonthlyUSD != "" {
		t.Errorf("unreadable: %s", jsonOf(t, got))
	}
}

// Stored values for an account no longer listed are offered for removal,
// oldest first.
func TestOrphans(t *testing.T) {
	org := fakeOrg("a")
	entry := creditEntry("alpha", 0, org, "200", "2026-10-29")
	values := settings.Values{APICredits: map[string]settings.APICredit{
		entry.AuthIndex:    {MonthlyUSD: "300", Rev: 3, UpdatedAt: instant(t, "2026-10-08T00:00:00Z")},
		"org-3f2a9c1d0b7e": {MonthlyUSD: "260.50", Renews: "2026-10-29", Rev: 2, UpdatedAt: instant(t, "2026-10-02T00:00:00Z")},
		"org-000000000001": {Reading: storedReading(t, fakeOrg("b"), "1.00", "2026-10-01T00:00:00Z").Reading, Rev: 1, UpdatedAt: instant(t, "2026-10-01T00:00:00Z")},
	}}
	in := Input{Items: itemsOf(entry), Settings: values}
	got := buildCredits(t, in, instant(t, workedNow)).Orphans
	want := []client.CreditOrphan{
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
	over.AuthIndex, over.APICredit.Problem = "item-17", client.CreditProblemTooManyItems
	in := Input{Items: itemsOf(creditEntry("alpha", 0, org, "200", "2026-10-29"), over),
		Meter: meterOf(instant(t, "2026-10-09T11:59:00Z"), map[string]client.MeterOrganization{org: healthyOrg(t)})}
	in.Meter.Unlinked = []client.MeterUnlinked{
		{OrganizationID: fakeOrg("c"), FirstSeenAt: instant(t, "2026-10-01T00:00:00Z"), LastSeenAt: instant(t, "2026-10-09T10:00:00Z"), Requests: 37},
		{OrganizationID: fakeOrg("d"), FirstSeenAt: instant(t, "2026-10-02T00:00:00Z"), LastSeenAt: instant(t, "2026-10-09T09:00:00Z"), Requests: 2},
	}
	got := buildCredits(t, in, instant(t, workedNow)).Unlinked
	if len(got) != 2 || got[0].Reason != client.CreditUnlinkedOverLimit || got[0].Requests != 37 || got[1].Reason != client.CreditUnlinkedNotConfigured ||
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
func TestAPICreditsIsNullWithoutItems(t *testing.T) {
	in := inputOf(t, fixtureNamed(t, "seven-credentials"))
	if Compute(in).APICredits == nil {
		t.Fatal("the fixture lists organizations")
	}
	// The snapshot's anthropic-api entries are not where accounts come from.
	in.Items = nil
	state := Compute(in)
	if state.APICredits != nil {
		t.Fatalf("api_credits = %+v", state.APICredits)
	}
	if raw := jsonOf(t, state); !strings.Contains(raw, `"api_credits":null`) || !strings.Contains(raw, `"renewals":{"5f2b8c41d09e7a36"`) {
		t.Fatalf(`the state must carry "api_credits":null and the stored renewals: %s`, raw)
	}
	in.Settings = settings.Values{}
	if raw := jsonOf(t, Compute(in)); !strings.Contains(raw, `"renewals":{}`) {
		t.Fatalf(`no renewal stored must be "renewals":{}: %s`, raw)
	}
}

// Accounts refilling at the same second refill together, and one that has
// used nothing refills nothing.
func TestNextRefillMergesTies(t *testing.T) {
	spentOn := func(dollars uint64) client.MeterOrganization {
		o := healthyOrg(t)
		o.Days = []client.MeterBucket{bucket(t, "2026-10-01T00:00:00Z", client.MeterUsage{Model: "claude-sonnet-5-5", Input: dollars * 500_000})}
		return o
	}
	entries := []client.Entry{
		creditEntry("alpha", 0, fakeOrg("a"), "200", "2026-10-20"),
		creditEntry("bravo", 1, fakeOrg("b"), "100", "2026-10-20"),
		creditEntry("charlie", 2, fakeOrg("c"), "100", "2026-10-12"),
		creditEntry("delta", 3, fakeOrg("d"), "100", "2026-10-25"),
	}
	in := Input{Items: itemsOf(entries...), Meter: meterOf(instant(t, "2026-10-09T11:59:00Z"), map[string]client.MeterOrganization{
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
	misconfigured.APICredit.Problem = client.CreditProblemLabelMissing
	in := Input{Items: itemsOf(creditEntry("alpha", 0, fakeOrg("a"), "200", "2026-10-29"), misconfigured)}
	credits := buildCredits(t, in, instant(t, workedNow))
	want := client.CreditPool{AccountCount: 2, MissingCount: 2}
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
		account := buildCredits(t, Input{Items: itemsOf(creditEntry("alpha", 0, fakeOrg("a"), "100", tc.renews))}, now).Accounts[0]
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

// No key ever reaches quota-cache's input, so none can reach the state; this
// pins that nothing in the state is shaped like one.
func TestAPICreditDocumentCarriesNoKey(t *testing.T) {
	for _, f := range fixtures {
		if raw := jsonOf(t, computeFixture(t, f.name)); strings.Contains(raw, "sk-ant-") {
			t.Fatalf("%s: the state contains something shaped like an Anthropic key", f.name)
		}
	}
}

// The happy-path golden is the card the design is drawn against.
func TestGoldenAPICreditsShowEveryPoolShape(t *testing.T) {
	credits := computeFixture(t, "seven-credentials").APICredits
	if credits == nil || credits.Meter == nil {
		t.Fatal("apiCredits or its meter is null")
	}
	bases, levels := map[string]bool{}, map[string]bool{}
	var sawDashboardGrant, sawCacheWriteExtra bool
	for _, account := range credits.Accounts {
		if account.State != client.CreditStateOK || !account.Counted {
			t.Errorf("%s: state %s", account.Label, account.State)
		}
		bases[account.Basis], levels[account.Level] = true, true
		sawDashboardGrant = sawDashboardGrant || (account.Basis == client.CreditBasisReading && account.MonthlyCreditSource == client.CreditSourceDashboard)
		sawCacheWriteExtra = sawCacheWriteExtra || account.CacheWriteExtraText != ""
	}
	if len(credits.Accounts) != 5 || !bases[client.CreditBasisCredit] || !bases[client.CreditBasisReading] || !levels[client.LevelOK] || !levels[client.LevelLow] ||
		!sawDashboardGrant || !sawCacheWriteExtra || len(credits.Unlinked) != 1 || credits.Unlinked[0].Reason != client.CreditUnlinkedNotConfigured {
		t.Errorf("the happy path misses a shape: bases %v levels %v dashboard grant %v cache-write extra %v unlinked %d",
			bases, levels, sawDashboardGrant, sawCacheWriteExtra, len(credits.Unlinked))
	}
	if !credits.Editing.Available || credits.Pool.NextRefill == nil || !credits.Pool.HasEstimate {
		t.Errorf("pool: %+v editing %+v", credits.Pool, credits.Editing)
	}
}

// Every state and issue a reader has to render for an API credit account must
// appear in the degraded state, as a reader with Quota Glance's default
// stale-after sees it.
func TestDegradedContractCoversEveryAPICreditState(t *testing.T) {
	f := fixtureNamed(t, "degraded-states")
	credits := client.CreditsAt(computeFixture(t, f.name), f.at, client.DefaultCreditStaleAfter).APICredits
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
		sawLowerBoundStartedLate = sawLowerBoundStartedLate || (account.LowerBound && account.Counted && issueIn(account, client.CreditIssueMeterStartedLate))
		sawRefillOnly = sawRefillOnly || (account.State == client.CreditStateNeedsSettings && issueIn(account, client.CreditIssueNeedsRefillDate) && !issueIn(account, client.CreditIssueNeedsCredit))
	}
	// No account is ok here: the degraded meter is stale, which every counted
	// account but an out one shares. The happy path carries ok.
	for _, want := range []string{client.CreditStateStale, client.CreditStateOut, client.CreditStatePending, client.CreditStateNeedsSettings, client.CreditStateMisconfigured, client.CreditStateCacheTooOld} {
		if !states[want] {
			t.Errorf("no account with state %q", want)
		}
	}
	for _, want := range []string{client.CreditIssueRefused, client.CreditIssueStale, client.CreditIssueMeterStartedLate, client.CreditIssueMeterGap, client.CreditIssueUnpricedModel,
		client.CreditIssueMisconfigured, client.CreditIssueCacheTooOld, client.CreditIssueOrgNotCounted, client.CreditIssueNeedsRefillDate} {
		if !issues[want] {
			t.Errorf("no dataIssue %q", want)
		}
	}
	for _, want := range []string{client.CreditNotEditableOverLimit, client.CreditNotEditableDuplicateOrganization, client.CreditNotEditableNoOrganization, client.CreditNotEditableCacheTooOld} {
		if !reasons[want] {
			t.Errorf("no account not editable for %q", want)
		}
	}
	if !sawLowerBoundStartedLate || !sawRefillOnly {
		t.Errorf("missing: lowerBound startedLate %v, needsSettings for the refill date only %v", sawLowerBoundStartedLate, sawRefillOnly)
	}
	if !credits.Editing.Available || !credits.Meter.Stale || len(credits.Meter.Gaps) == 0 || credits.Meter.Gaps[0].Reason != client.MeterStopShutdown ||
		len(credits.Orphans) == 0 {
		t.Errorf("card: editing %+v meter %+v orphans %d", credits.Editing, credits.Meter, len(credits.Orphans))
	}
	overLimit := false
	for _, u := range credits.Unlinked {
		overLimit = overLimit || u.Reason == client.CreditUnlinkedOverLimit
	}
	if !overLimit {
		t.Error("no over-limit unlinked organization")
	}
}

func issueIn(account client.CreditAccount, issue string) bool {
	for _, have := range account.DataIssues {
		if have == issue {
			return true
		}
	}
	return false
}

// A stored reading that has aged past its use does not stop the rest of the
// account's values applying: its credit and refill date apply, and the reading
// is reported unused. Time rules apply when saving, never when loading; that
// the settings store loads such a file is its own tests' to show.
func TestAnOldReadingIsUnusedAndTheRestStillApplies(t *testing.T) {
	org := fakeOrg("a")
	entry := creditEntry("alpha", 0, org, "200", "")
	stored := func(credit settings.APICredit) settings.Values {
		return settings.Values{Revision: 3, APICredits: map[string]settings.APICredit{client.APICreditOrgAccount(org): credit}}
	}
	readingAt := func(when string) *settings.Reading {
		at := instant(t, when)
		return &settings.Reading{RemainingUSD: "143.20", At: at, EnteredAt: at, OrganizationID: org,
			Baseline: settings.Baseline{DayStart: client.MeterDayStart(at), Until: client.MeterHourStart(at), Usage: []client.MeterUsage{}}}
	}
	in := Input{Items: itemsOf(entry),
		Meter: meterOf(instant(t, "2026-10-09T11:59:00Z"), map[string]client.MeterOrganization{org: healthyOrg(t)})}

	// Three days old, and before the refill this file itself sets.
	in.Settings = stored(settings.APICredit{MonthlyUSD: "300", Renews: "2026-10-07", Reading: readingAt("2026-10-06T10:00:00Z"), Rev: 3, UpdatedAt: instant(t, "2026-10-06T10:05:00Z")})
	got := buildCredits(t, in, instant(t, workedNow)).Accounts[0]
	if got.MonthlyCreditText != "$300.00" || got.MonthlyCreditSource != client.CreditSourceDashboard || got.RenewsSource != client.CreditSourceDashboard ||
		*got.CycleStartEpoch != instant(t, "2026-10-07T00:00:00Z").Unix() || got.Basis != client.CreditBasisCredit || got.Reading != nil ||
		got.Settings.ReadingUnusedReason != client.CreditReadingBeforeRefill || got.Settings.Reading == nil {
		t.Errorf("before the refill: %s", jsonOf(t, got))
	}

	// Forty days old with no refill date anywhere.
	in.Settings = stored(settings.APICredit{MonthlyUSD: "300", Reading: readingAt("2026-08-30T10:00:00Z"), Rev: 3, UpdatedAt: instant(t, "2026-08-30T10:05:00Z")})
	got = buildCredits(t, in, instant(t, workedNow)).Accounts[0]
	if got.MonthlyCreditText != "$300.00" || got.Settings.ReadingUnusedReason != client.CreditReadingTooOld || got.State != client.CreditStateNeedsSettings {
		t.Errorf("too old: %s", jsonOf(t, got))
	}
}
