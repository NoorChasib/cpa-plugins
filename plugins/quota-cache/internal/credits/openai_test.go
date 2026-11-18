package credits

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/settings"
)

// The OpenAI lane of the degraded-states fixture: the OpenAI meter and
// settings beside the degraded-states snapshot, computed at fixtureNow. It is
// not in fixtures, whose every entry has a Quota Glance characterization of
// the Claude card; only the tests here read it.
var (
	openAIFixtureSnapshot = filepath.Join("testdata", "snapshots", "degraded-states.json")
	openAIFixtureSettings = client.OpenAISettingsPath(filepath.Join("testdata", "settings", "degraded-states.json"))
	openAIFixtureGolden   = client.OpenAICreditsPath(filepath.Join("testdata", "golden", "degraded-states.json"))
)

// openAIFixtureSlugs are the fixture's OpenAI organizations as the
// openai-organization header names them: a personal organization, a named
// one, and one the meter has not counted yet.
var openAIFixtureSlugs = []string{"user-AbCdEfGhIjKlMnOpQrStUvWx", "acme-x1wegt", "side-k2m9qz"}

// openAIFixtureItems is the OpenAI organizations list of the fixture, as the
// publisher passes it: each organization's account id, label, position and
// slug.
func openAIFixtureItems() []Item {
	labels := []string{"Pro", "Business", "Side project"}
	items := make([]Item, 0, len(openAIFixtureSlugs))
	for i, slug := range openAIFixtureSlugs {
		items = append(items, Item{
			ID:     client.OpenAICreditOrgAccount(slug),
			Credit: &client.APICredit{Label: labels[i], Position: i, OrganizationID: slug},
		})
	}
	return items
}

// openAIInputOf is what the publisher would pass for the fixture's OpenAI
// lane at fixtureNow. The settings file is read with the OpenAI store's own
// rules, so the fixture is one quota-cache would load.
func openAIInputOf(t *testing.T) Input {
	t.Helper()
	snapshot, err := client.Load(openAIFixtureSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	meter, err := client.LoadMeter(client.OpenAIMeterPath(openAIFixtureSnapshot))
	if err != nil {
		t.Fatalf("OpenAI meter beside %s: %v", openAIFixtureSnapshot, err)
	}
	values := settings.LoadFor(settings.OpenAI, openAIFixtureSettings)
	if values.Unreadable || len(values.APICredits) == 0 {
		t.Fatalf("%s does not load as OpenAI settings", openAIFixtureSettings)
	}
	return Input{
		Provider:      OpenAI,
		Now:           time.Unix(fixtureNow, 0).UTC(),
		Snapshot:      snapshot,
		Items:         openAIFixtureItems(),
		Meter:         meter,
		Settings:      values,
		SettingsState: client.CreditSettingsOK,
		Organizations: client.CreditOrganizations{State: client.OrganizationsStateOK, Revision: "3"},
		Version:       "0.6.0",
	}
}

// The committed OpenAI state is what quota-cache publishes for the fixture's
// OpenAI lane. Quota Glance's and Reset Priority's tests read it as published
// input.
func TestGoldenOpenAICreditStates(t *testing.T) {
	raw, err := json.MarshalIndent(Compute(openAIInputOf(t)), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	if *update {
		if err := os.WriteFile(openAIFixtureGolden, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(openAIFixtureGolden)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/credits -update to write it)", err)
	}
	if !bytes.Equal(raw, want) {
		t.Errorf("%s differs from the committed state; if the change is intended, rerun with -update and review the diff", openAIFixtureGolden)
	}
	state, err := client.LoadCredits(openAIFixtureGolden)
	if err != nil || !reflect.DeepEqual(state, Compute(openAIInputOf(t))) {
		t.Errorf("%s does not load back to the state it was written from: %v", openAIFixtureGolden, err)
	}
	for _, key := range []string{`"provider": "openai-api"`, `"renewals": {}`, `"config_items_ignored": 0`} {
		if !bytes.Contains(want, []byte(key)) {
			t.Errorf("%s has no %s", openAIFixtureGolden, key)
		}
	}
}

// The fixture's OpenAI organizations come out as designed, as published and
// as a reader with the default stale-after sees them at the fixture's
// instant.
func TestOpenAIFixtureOutcomes(t *testing.T) {
	in := openAIInputOf(t)
	published := Compute(in)
	for name, state := range map[string]*client.CreditState{
		"published": published,
		"judged":    client.CreditsAt(published, in.Now, client.DefaultCreditStaleAfter),
	} {
		d := state.APICredits
		if d == nil || len(d.Accounts) != 3 {
			t.Fatalf("%s: api_credits %+v", name, d)
		}
		checkCreditPoolAddsUp(t, d)
		pro, business, side := d.Accounts[0], d.Accounts[1], d.Accounts[2]
		for i, a := range d.Accounts {
			if a.ID != client.OpenAICreditOrgAccount(openAIFixtureSlugs[i]) || a.OrganizationID != openAIFixtureSlugs[i] || !a.Settings.Editable {
				t.Errorf("%s: account %d is %s for %q, editable %v", name, i, a.ID, a.OrganizationID, a.Settings.Editable)
			}
		}

		// Pro: every tier, long context and cache writes, all priced. At the
		// 12-hour rate gpt-6-sol's 2M writes would cost $5.00 more.
		//   gpt-6-sol, Aug 28: 2M in $4.00, 0.4M out $4.00, 6M cached $1.20, 2M written $5.00
		//   gpt-6.1-terra Flex: 4M in $2.00, 1M out $2.50, 2M cached $0.05
		//   gpt-6.2-sol Fast over 272K: 0.6M in $4.80, 50K out $1.50, 0.4M cached $0.16
		//   gpt-5.5: 1M in $5.00, 0.1M out $3.00, 0.5M cached $0.25, 1M written at input $5.00
		//   gpt-6.1-sol Ultrafast: 0.2M in $2.40, 50K out $3.00
		//   gpt-6-sol, Sep 10: 0.1M in $0.20, 20K out $0.20
		if pro.State != client.CreditStateOK || pro.Basis != client.CreditBasisCredit || pro.LowerBound || pro.Level != client.LevelOK ||
			len(pro.Unpriced) != 0 || pro.CacheWriteExtra != 0 || pro.CacheWriteExtraText != "" || len(pro.DataIssues) != 0 ||
			pro.SpentText != "$44.26" || pro.LeftText != "$155.74" || pro.MonthlyCreditText != "$200.00" {
			t.Errorf("%s: Pro %s", name, jsonOf(t, pro))
		}

		// Business: refused after its last success, with $260.00 - $186.00
		// left in the estimate and an image model left out.
		if business.State != client.CreditStateOut || len(business.DataIssues) == 0 || business.DataIssues[0] != client.CreditIssueRefused ||
			!slices.Contains(business.DataIssues, client.CreditIssueUnpricedModel) || business.EstimateLeftText != "$74.00" ||
			business.LeftText != "$0.00" || business.Level != client.LevelCritical || !business.LowerBound ||
			!reflect.DeepEqual(business.Unpriced, []client.CreditUnpriced{{Model: "gpt-image-2", Tokens: 700_000}}) ||
			business.Issue != "OpenAI refused a request with credit_balance_exhausted, so this credit is spent. The estimate had $74.00 left; enter an API Platform reading to correct it." ||
			business.Refusals.Total != 2 || business.Refusals.ClaudeCodeTotal != 0 {
			t.Errorf("%s: Business %s", name, jsonOf(t, business))
		}

		// Side project: listed, not counted yet, nothing set.
		if side.State != client.CreditStatePending ||
			!reflect.DeepEqual(side.DataIssues, []string{client.CreditIssueOrgNotCounted, client.CreditIssueNeedsCredit, client.CreditIssueNeedsRefillDate}) ||
			side.Issue != "Counting starts at Quota Cache's next save." {
			t.Errorf("%s: Side project %s", name, jsonOf(t, side))
		}

		if len(d.Orphans) != 1 || d.Orphans[0].ID != client.OpenAICreditOrgAccount("old-team-p0q1r2") || d.Orphans[0].MonthlyUSD != "100" {
			t.Errorf("%s: orphans %s", name, jsonOf(t, d.Orphans))
		}
		if len(d.Unlinked) != 1 || d.Unlinked[0].OrganizationID != "user-ZyXwVuTsRqPoNmLkJiHgFeDc" || d.Unlinked[0].Requests != 7 ||
			d.Unlinked[0].Reason != client.CreditUnlinkedNotConfigured || d.Unlinked[0].LastSeenEpoch != instant(t, "2026-09-10T03:00:00Z").Unix() {
			t.Errorf("%s: unlinked %s", name, jsonOf(t, d.Unlinked))
		}
		// The long gap is before both cycles began: listed, and no account's
		// issue.
		if d.Meter == nil || d.Meter.Stale || len(d.Meter.Gaps) != 1 || d.Meter.Gaps[0].Reason != client.MeterGapUncleanStop ||
			d.Meter.Foreign != 0 || d.Meter.Unattributed != 0 {
			t.Errorf("%s: meter %s", name, jsonOf(t, d.Meter))
		}
		if d.Pool.AccountCount != 3 || d.Pool.CountedCount != 2 || d.Pool.MissingCount != 1 || !d.Pool.LowerBound || d.Pool.LeftText != "$155.74" {
			t.Errorf("%s: pool %s", name, jsonOf(t, d.Pool))
		}
		if len(state.Renewals) != 0 || state.Renewals == nil || state.Organizations.ConfigItemsIgnored != 0 {
			t.Errorf("%s: renewals %v organizations %+v", name, state.Renewals, state.Organizations)
		}
	}
}

// openAISlug is the organization the OpenAI rule cases below count.
const openAISlug = "user-AbCdEfGhIjKlMnOpQrStUvWx"

// openAICase is one OpenAI organization, metered and set as given, for the
// rule cases below. It is creditCase for the OpenAI lane.
type openAICase struct {
	org     client.MeterOrganization
	meter   func(*client.APIMeter)
	stored  *settings.APICredit
	now     string
	stale   time.Duration
	noMeter bool
	noOrg   bool
}

func (c openAICase) build(t *testing.T) client.CreditAccount {
	t.Helper()
	now := instant(t, workedNow)
	if c.now != "" {
		now = instant(t, c.now)
	}
	id := client.OpenAICreditOrgAccount(openAISlug)
	in := Input{Provider: OpenAI, Now: now, StaleAfter: c.stale, SettingsState: client.CreditSettingsOK,
		Items:    []Item{{ID: id, Credit: &client.APICredit{Label: "Pro", OrganizationID: openAISlug}}},
		Settings: settings.Values{APICredits: map[string]settings.APICredit{}, Renewals: map[string]settings.Renewal{}}}
	if c.stored != nil {
		in.Settings.APICredits[id] = *c.stored
	}
	if !c.noMeter {
		orgs := map[string]client.MeterOrganization{}
		if !c.noOrg {
			orgs[openAISlug] = c.org
		}
		in.Meter = meterOf(now.Add(-time.Minute), orgs)
		if c.meter != nil {
			c.meter(in.Meter)
		}
	}
	d := Compute(in).APICredits
	checkCreditPoolAddsUp(t, d)
	return d.Accounts[0]
}

// openAIStored is a monthly credit and refill date entered on the page.
func openAIStored(t *testing.T, monthly, renews string) *settings.APICredit {
	return &settings.APICredit{MonthlyUSD: monthly, Renews: renews, Rev: 1, UpdatedAt: instant(t, "2026-09-01T00:00:00Z")}
}

// healthyOpenAIOrg spent $20.00 on Oct 1 on gpt-6-sol, counted since
// September, last seen then: healthyOrg's spend, at OpenAI's prices.
func healthyOpenAIOrg(t *testing.T) client.MeterOrganization {
	return client.MeterOrganization{
		Since:      instant(t, "2026-09-01T00:00:00Z"),
		LastSeenAt: ptrAt(t, "2026-10-01T12:00:00Z"), LastSuccessAt: ptrAt(t, "2026-10-01T12:00:00Z"),
		Days: []client.MeterBucket{bucket(t, "2026-10-01T00:00:00Z", client.MeterUsage{Model: "gpt-6-sol", Requests: 10, Input: 10_000_000})},
	}
}

// claudeWords are what an OpenAI account's sentence must never say.
var claudeWords = []string{"Console", "Anthropic", "Claude", "organization-id"}

func namesClaude(sentence string) bool {
	for _, word := range claudeWords {
		if strings.Contains(strings.ToLower(sentence), strings.ToLower(word)) {
			return true
		}
	}
	return false
}

// claudeOnlyIssues are the issues an OpenAI account never has: no OpenAI
// refusal is from a Claude Code-based client, the OpenAI meter never leaves a
// failure unattributed, and every OpenAI item is an organization from the
// page, never one of quota-cache 0.1.x's configured items.
var claudeOnlyIssues = []string{
	client.CreditIssueClaudeCodeRefused, client.CreditIssueRefusedNearlySpent, client.CreditIssueCacheTooOld,
	client.CreditIssueMisconfigured, client.CreditIssueMeterUnattributed,
}

// An OpenAI account's sentences say API Platform where Claude's say Console,
// and never name Anthropic or Claude: every sentence written for an issue the
// OpenAI lane can raise, and each rule's sentence as published.
func TestOpenAISentencesNameAPIPlatform(t *testing.T) {
	healthy := func() client.MeterOrganization { return healthyOpenAIOrg(t) }
	refusedAt := func(o client.MeterOrganization) client.MeterOrganization {
		o.Refusals, o.LastRefusalAt = 1, ptrAt(t, "2026-10-09T10:00:00Z")
		return o
	}
	late := func(since string) client.MeterOrganization {
		o := healthy()
		o.Since, o.Days = instant(t, since), nil
		return o
	}
	reading := func(org, amount, at, renews string) *settings.APICredit {
		r := storedReading(t, org, amount, at)
		r.MonthlyUSD, r.Renews = "200", renews
		return r
	}
	for _, tc := range []struct {
		name, issue, sentence string
		c                     openAICase
	}{
		{"no meter file", client.CreditIssueMeterMissing,
			"Quota Cache has not saved its OpenAI API meter yet; counting starts when it next saves.",
			openAICase{stored: openAIStored(t, "200", "2026-10-29"), noMeter: true}},
		{"not in the meter yet", client.CreditIssueOrgNotCounted, "Counting starts at Quota Cache's next save.",
			openAICase{stored: openAIStored(t, "200", "2026-10-29"), noOrg: true}},
		{"no refill date", client.CreditIssueNeedsRefillDate, "Set the refill date, or enter an API Platform reading.",
			openAICase{org: healthy(), stored: openAIStored(t, "200", "")}},
		{"refused", client.CreditIssueRefused,
			"OpenAI refused a request with credit_balance_exhausted, so this credit is spent. The estimate had $180.00 left; enter an API Platform reading to correct it.",
			openAICase{org: refusedAt(healthy()), stored: openAIStored(t, "200", "2026-10-29")}},
		{"refused with nothing left", client.CreditIssueRefused,
			"OpenAI refused a request with credit_balance_exhausted, so this credit is spent.",
			openAICase{org: refusedAt(healthy()), stored: openAIStored(t, "20", "2026-10-29")}},
		{"stale", client.CreditIssueStale, client.CreditSentenceStale,
			openAICase{org: healthy(), stored: openAIStored(t, "200", "2026-10-29"), stale: 45 * time.Minute,
				meter: func(m *client.APIMeter) { m.FlushedAt = instant(t, "2026-10-09T11:00:00Z") }}},
		// An organization added while the meter already counted others is
		// judged from when it was added.
		{"added after the cycle began", client.CreditIssueMeterStartedLate,
			"Counting began after this cycle started, so earlier spend is missing. Enter an API Platform reading to correct it.",
			openAICase{org: late("2026-10-02T00:00:00Z"), stored: openAIStored(t, "200", "2026-10-29")}},
		{"added after the reading", client.CreditIssueMeterStartedLate,
			"Counting began after your API Platform reading, so spend in between is missing.",
			openAICase{org: late("2026-10-09T10:00:00Z"), stored: reading(openAISlug, "100.00", "2026-10-09T09:00:00Z", "2026-10-29")}},
		{"a long gap", client.CreditIssueMeterGap, client.CreditSentenceOpenAIMeterGap,
			openAICase{org: healthy(), stored: openAIStored(t, "200", "2026-10-29"), meter: func(m *client.APIMeter) {
				m.Gaps = []client.MeterGap{{From: instant(t, "2026-10-02T00:00:00Z"), To: instant(t, "2026-10-02T01:00:00Z"), Reason: client.MeterStopShutdown}}
			}}},
		{"rejected records", client.CreditIssueMeterRejected,
			"Quota Cache could not read some usage records from CPA, so some spend may be missing.",
			openAICase{org: healthy(), stored: openAIStored(t, "200", "2026-10-29"), meter: func(m *client.APIMeter) {
				m.Rejected, m.LastRejectedAt = 1, ptrAt(t, "2026-10-02T00:00:00Z")
			}}},
		{"past the credit", client.CreditIssueOverCredit,
			"Spend is $5.00 past the monthly credit. OpenAI bills purchased credits after the monthly credit; if there are none, enter an API Platform reading.",
			openAICase{org: healthy(), stored: openAIStored(t, "15", "2026-10-29")}},
		{"a reading above the credit", client.CreditIssueReadingAboveCredit,
			"Your API Platform reading is more than the monthly credit; check the monthly credit.",
			openAICase{org: healthy(), stored: reading(openAISlug, "260.00", "2026-10-09T09:00:00Z", "2026-10-29")}},
		{"a reading on the refill day", client.CreditIssueReadingOnRefillDay,
			"This API Platform reading was taken on the refill day. If API Platform did not show the new credit yet, enter a new reading once it does.",
			openAICase{org: healthy(), stored: reading(openAISlug, "190.00", "2026-10-05T08:00:00Z", "2026-10-05")}},
		{"a reading before the refill", client.CreditIssueReadingUnused,
			"Your API Platform reading of $150.00 on Oct 3 was before the last refill, so it is not used.",
			openAICase{org: healthy(), stored: reading(openAISlug, "150.00", "2026-10-03T09:00:00Z", "2026-10-05")}},
		{"a reading for another organization", client.CreditIssueReadingUnused,
			"Your API Platform reading was for a different organization, so it is not used.",
			openAICase{org: healthy(), stored: reading("acme-x1wegt", "150.00", "2026-10-09T09:00:00Z", "2026-10-05")}},
		{"a reading of another case of the organization", client.CreditIssueReadingUnused,
			"Your API Platform reading was for a different organization, so it is not used.",
			openAICase{org: healthy(), stored: reading(strings.ToLower(openAISlug), "150.00", "2026-10-09T09:00:00Z", "2026-10-05")}},
		{"a reading dated ahead", client.CreditIssueReadingUnused,
			"Your API Platform reading is dated in the future, so it is not used.",
			openAICase{org: healthy(), stored: reading(openAISlug, "150.00", "2026-10-09T13:00:00Z", "2026-10-05")}},
		{"no traffic", client.CreditIssueNoTraffic,
			"No traffic from this organization has reached CPA through an openai-compatibility entry since counting began.",
			openAICase{org: client.MeterOrganization{Since: instant(t, "2026-09-01T00:00:00Z")}, stored: openAIStored(t, "200", "2026-10-29")}},
	} {
		got := tc.c.build(t)
		if len(got.DataIssues) == 0 || got.DataIssues[0] != tc.issue || got.Issue != tc.sentence {
			t.Errorf("%s: issues %v, sentence %q; want %s first, %q", tc.name, got.DataIssues, got.Issue, tc.issue, tc.sentence)
		}
	}
	// A reading too old to use is reported with the refill date it lacks
	// first, so its sentence is checked on its own.
	tooOld := &creditAccount{provider: OpenAI, issues: map[string]bool{}, unused: client.CreditReadingTooOld}
	if got := tooOld.issueSentenceOf(client.CreditIssueReadingUnused, nil); got != "Your API Platform reading is over 31 days old and there is no refill date, so it is not used." {
		t.Errorf("too old: %q", got)
	}

	// Every sentence an OpenAI account can be given, whatever its basis, its
	// reading and its estimate.
	credit := &client.APICredit{Label: "Pro", OrganizationID: openAISlug}
	for _, issue := range client.CreditIssueOrder {
		if slices.Contains(claudeOnlyIssues, issue) {
			continue
		}
		for _, basis := range []string{"", client.CreditBasisCredit, client.CreditBasisReading} {
			for _, unused := range []string{"", client.CreditReadingBeforeRefill, client.CreditReadingOtherOrganization, client.CreditReadingTooOld, client.CreditReadingFuture} {
				for _, estimate := range []string{"", "0", "12.34"} {
					a := &creditAccount{provider: OpenAI, issues: map[string]bool{}, unused: unused}
					a.out.Basis, a.out.OverageText = basis, "$1.00"
					a.out.Unpriced = []client.CreditUnpriced{{Model: "gpt-5.6-sol ultrafast", Tokens: 1_200_000}}
					a.out.Settings.Reading = &client.StoredReading{RemainingUSD: "150.00", AtEpoch: instant(t, "2026-10-03T09:00:00Z").Unix()}
					if estimate != "" {
						a.estimate = exactDollars(estimate)
					}
					if sentence := a.issueSentenceOf(issue, credit); namesClaude(sentence) {
						t.Errorf("%s (basis %q, reading %q, estimate %q): %q", issue, basis, unused, estimate, sentence)
					}
				}
			}
		}
	}
	// And every sentence the fixture publishes, or a reader judging it later
	// writes, its meter stopped or stale.
	in := openAIInputOf(t)
	stopped := *in.Meter
	stoppedAt := stopped.FlushedAt
	stopped.StoppedAt, stopped.StopReason = &stoppedAt, client.MeterStopQuiesce
	for _, meter := range []*client.APIMeter{in.Meter, &stopped} {
		in.Meter = meter
		published := Compute(in)
		for _, later := range []time.Duration{0, time.Hour, 6 * time.Hour} {
			for _, a := range client.CreditsAt(published, in.Now.Add(later), client.DefaultCreditStaleAfter).APICredits.Accounts {
				if namesClaude(a.Issue) {
					t.Errorf("%s %v later: %q", a.Label, later, a.Issue)
				}
			}
		}
	}
	// The meter-gap sentence a reader writes is the one quota-cache would.
	in.Meter = &stopped
	judged := client.CreditsAt(Compute(in), in.Now.Add(10*time.Minute), 0).APICredits.Accounts[0]
	in.Now = in.Now.Add(10 * time.Minute)
	if recomputed := Compute(in).APICredits.Accounts[0]; judged.Issue != client.CreditSentenceOpenAIMeterGap || judged.Issue != recomputed.Issue {
		t.Errorf("a stopped meter, judged %q, recomputed %q", judged.Issue, recomputed.Issue)
	}
}

// The Claude-only issues never reach an OpenAI account: not in the fixture,
// not when every organization is refused with its estimate nearly spent, and
// not when a reader judges the state later. No account is held uneditable as
// if quota-cache 0.1.x had written it, and none carries a cache-write
// difference.
func TestOpenAIStateHasNoClaudeOnlyIssues(t *testing.T) {
	check := func(name string, state *client.CreditState) {
		t.Helper()
		d := state.APICredits
		for _, a := range d.Accounts {
			for _, issue := range a.DataIssues {
				if slices.Contains(claudeOnlyIssues, issue) {
					t.Errorf("%s: %s raises %s", name, a.Label, issue)
				}
			}
			if !a.Settings.Editable || a.Settings.NotEditableReason != "" || a.CacheWriteExtra != 0 || a.CacheWriteExtraText != "" {
				t.Errorf("%s: %s %s", name, a.Label, jsonOf(t, a))
			}
		}
		for _, u := range d.Unlinked {
			if u.Reason != client.CreditUnlinkedNotConfigured {
				t.Errorf("%s: unlinked %s is %s", name, u.OrganizationID, u.Reason)
			}
		}
	}
	in := openAIInputOf(t)
	check("the fixture", Compute(in))

	// Every organization refused at the fixture's instant, after its last
	// success, with Pro's estimate nearly spent.
	refused := *in.Meter
	refused.Organizations = map[string]client.MeterOrganization{}
	for slug, org := range in.Meter.Organizations {
		org.Refusals, org.LastRefusalAt = org.Refusals+1, ptrAt(t, "2026-09-10T03:30:00Z")
		refused.Organizations[slug] = org
	}
	nearlySpent := in
	nearlySpent.Meter = &refused
	nearlySpent.Settings.APICredits = map[string]settings.APICredit{}
	for id, stored := range in.Settings.APICredits {
		nearlySpent.Settings.APICredits[id] = stored
	}
	pro := nearlySpent.Settings.APICredits[client.OpenAICreditOrgAccount(openAIFixtureSlugs[0])]
	pro.MonthlyUSD = "45"
	nearlySpent.Settings.APICredits[client.OpenAICreditOrgAccount(openAIFixtureSlugs[0])] = pro
	state := Compute(nearlySpent)
	check("everything refused", state)
	if a := state.APICredits.Accounts[0]; a.State != client.CreditStateOut || a.DataIssues[0] != client.CreditIssueRefused {
		t.Errorf("a refused nearly spent account: %s", jsonOf(t, a))
	}

	for _, later := range []time.Duration{0, 30 * time.Minute, 2 * time.Hour, 24 * time.Hour} {
		check("judged later", client.CreditsAt(Compute(in), in.Now.Add(later), client.DefaultCreditStaleAfter))
	}
}

// The OpenAI document names its provider, its title and its price table's
// provenance; the Claude one is as it always was, with no provider key, and
// Input.Provider nil is Claude, byte for byte.
func TestOpenAIDocumentNamesItsProvider(t *testing.T) {
	d := Compute(openAIInputOf(t)).APICredits
	if d.Provider != client.ProviderOpenAIAPI || d.Title != "Monthly OpenAI API Credit" || d.Currency != "USD" ||
		d.Pricing != (client.CreditPricing{AsOf: "2026-11-18", Source: "https://developers.openai.com/api/docs/pricing", CacheWrites: "30m"}) {
		t.Errorf("document: provider %q title %q pricing %+v", d.Provider, d.Title, d.Pricing)
	}
	if raw := jsonOf(t, d); !strings.HasPrefix(raw, `{"provider":"openai-api","title":`) {
		t.Errorf("provider is not the first key: %.80s", raw)
	}
	for _, f := range fixtures {
		in := inputOf(t, f)
		claude := jsonOf(t, Compute(in))
		if strings.Contains(claude, `"provider"`) {
			t.Errorf("%s: the Claude state carries a provider", f.name)
		}
		in.Provider = Claude
		if jsonOf(t, Compute(in)) != claude {
			t.Errorf("%s: Provider Claude differs from Provider nil", f.name)
		}
	}
}

// OpenAI writes are priced at the 30-minute rate only, so no OpenAI account
// has a cache-write difference to report, however much it writes.
func TestOpenAINeverPublishesACacheWriteExtra(t *testing.T) {
	o := healthyOpenAIOrg(t)
	o.Days = []client.MeterBucket{bucket(t, "2026-10-01T00:00:00Z", client.MeterUsage{Model: "gpt-6-sol", Requests: 10, CacheWrite: 4_000_000})}
	got := openAICase{org: o, stored: openAIStored(t, "200", "2026-10-29")}.build(t)
	if got.CacheWriteExtra != 0 || got.CacheWriteExtraText != "" || got.SpentText != "$10.00" || got.State != client.CreditStateOK {
		t.Errorf("%s", jsonOf(t, got))
	}
}

// An unpriced OpenAI entry's label has no parentheses of its own, so the
// sentence has only the token count's.
func TestOpenAIUnpricedSentenceHasOneParenthesis(t *testing.T) {
	o := healthyOpenAIOrg(t)
	o.Days = append(o.Days, bucket(t, "2026-10-02T00:00:00Z",
		client.MeterUsage{Model: "gpt-5.6-sol", Tier: client.MeterTierUltrafast, Requests: 3, Input: 1_000_000, Output: 200_000}))
	got := openAICase{org: o, stored: openAIStored(t, "200", "2026-10-29")}.build(t)
	want := "Some requests used a model with no listed price and are left out: gpt-5.6-sol ultrafast (1.2M tokens)."
	if got.Issue != want || strings.Count(got.Issue, "(") != 1 || !got.LowerBound || got.SpentText != "$20.00" {
		t.Errorf("%s", jsonOf(t, got))
	}
}

// The OpenAI state, published once and judged by client.CreditsAt at any
// later instant before its valid_until, is the state computed then, as the
// Claude one is: with its meter counting and stopped at its last save, at
// every boundary the overlay or valid_until covers.
func TestOpenAIOverlayEqualsRecompute(t *testing.T) {
	in := openAIInputOf(t)
	stopped := *in.Meter
	stoppedAt := stopped.FlushedAt
	stopped.StoppedAt, stopped.StopReason = &stoppedAt, client.MeterStopQuiesce
	checked := 0
	for name, meter := range map[string]*client.APIMeter{"openai": in.Meter, "openai stopped": &stopped} {
		c := overlayCase{name: name, in: in, floor: meter.FlushedAt}
		c.in.Meter = meter
		boundaries := []time.Time{c.floor, c.floor.Add(client.MeterBriefGap)}
		for _, sa := range overlayStaleAfters {
			boundaries = append(boundaries, c.floor.Add(sa))
		}
		for _, stored := range in.Settings.APICredits {
			for months := range 3 {
				if cycle, err := client.CreditCycleAt(stored.Renews, c.floor.AddDate(0, months, 0)); err == nil {
					boundaries = append(boundaries, cycle.End)
				}
			}
		}
		for _, b := range boundaries {
			for _, t1 := range []time.Time{b.Add(-time.Second), b, b.Add(time.Second), b.Add(time.Hour)} {
				for _, sa := range overlayStaleAfters {
					if ok, _ := checkOverlay(t, c, c.floor, t1, sa); ok {
						checked++
					}
					if ok, _ := checkOverlay(t, c, c.floor.Add(time.Minute), t1, sa); ok {
						checked++
					}
				}
			}
		}
	}
	if checked < 100 {
		t.Fatalf("only %d instants could be checked", checked)
	}
}

// The fixture's OpenAI meter is one the OpenAI meter could have written:
// buckets in order on their day and hour, usage sorted and keyed by
// normalized model, prompt and tier, hours within the hours kept and never
// more than their day, slugs as the header names them, and no auth ID,
// foreign or unattributed count.
func TestOpenAIFixtureMeterKeepsTheMetersInvariants(t *testing.T) {
	m, err := client.LoadMeter(client.OpenAIMeterPath(openAIFixtureSnapshot))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.AuthIDs) != 0 || m.AuthIDsSince != nil || m.Foreign != 0 || m.Unattributed != 0 || m.LastUnattributedAt != nil {
		t.Errorf("auth ids %v since %v, foreign %d, unattributed %d", m.AuthIDs, m.AuthIDsSince, m.Foreign, m.Unattributed)
	}
	sorted := func(usage []client.MeterUsage) bool {
		merged := []client.MeterUsage{}
		for _, u := range usage {
			merged = client.MergeMeterUsage(merged, u)
		}
		return reflect.DeepEqual(merged, usage)
	}
	firstHour := client.MeterHourStart(m.FlushedAt).Add(-client.MeterHours * time.Hour)
	for slug, org := range m.Organizations {
		if normalized, ok := client.NormalizeOpenAIOrganization(slug); !ok || normalized != slug {
			t.Errorf("%q is not a slug as the header names it", slug)
		}
		for _, kind := range []struct {
			name    string
			buckets []client.MeterBucket
			start   func(time.Time) time.Time
		}{{"day", org.Days, client.MeterDayStart}, {"hour", org.Hours, client.MeterHourStart}} {
			for i, b := range kind.buckets {
				if !b.Start.Equal(kind.start(b.Start)) || (i > 0 && !kind.buckets[i-1].Start.Before(b.Start)) || b.Start.After(m.FlushedAt) || !sorted(b.Usage) {
					t.Errorf("%s: %s bucket %s is out of place or unsorted", slug, kind.name, b.Start)
				}
				for _, u := range b.Usage {
					if client.NormalizeMeterModel(u.Model, "") != u.Model || (u.Prompt != "" && u.Prompt != client.MeterPromptOver272K) ||
						client.NormalizeOpenAIServiceTier(u.Tier) != u.Tier {
						t.Errorf("%s: %+v is not an OpenAI meter entry", slug, u)
					}
				}
			}
		}
		for _, hour := range org.Hours {
			if hour.Start.Before(firstHour) {
				t.Errorf("%s: hour %s is older than the meter keeps", slug, hour.Start)
			}
			day := client.MeterDayStart(hour.Start)
			inDay := []client.MeterUsage{}
			for _, d := range org.Days {
				if d.Start.Equal(day) {
					inDay = d.Usage
				}
			}
			hours, _ := org.UsageInHours(day, day.Add(24*time.Hour), m.FlushedAt)
			for _, h := range hours {
				i := slices.IndexFunc(inDay, func(d client.MeterUsage) bool { return d.Model == h.Model && d.Prompt == h.Prompt && d.Tier == h.Tier })
				if i < 0 || h.Requests > inDay[i].Requests || h.Failed > inDay[i].Failed || h.Input > inDay[i].Input || h.Output > inDay[i].Output ||
					h.CacheRead > inDay[i].CacheRead || h.CacheWrite > inDay[i].CacheWrite {
					t.Errorf("%s: the hours of %s hold more than the day: %+v", slug, day.Format(time.DateOnly), h)
				}
			}
		}
	}
	for _, u := range m.Unlinked {
		if _, linked := m.Organizations[u.OrganizationID]; linked || u.LastSeenAt.Before(u.FirstSeenAt) {
			t.Errorf("unlinked %+v", u)
		}
	}
	for index, auth := range m.Auths {
		if _, linked := m.Organizations[auth.OrganizationID]; !linked || len(index) != 16 {
			t.Errorf("auth %s: %+v", index, auth)
		}
	}
}
