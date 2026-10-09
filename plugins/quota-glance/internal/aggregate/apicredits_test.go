package aggregate

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// creditEntry is quota-cache's shape for one configured Console organization
// that has never been read: the configured facts and nothing else, with the
// legacy observation empty as it always is on this provider.
func creditEntry(label string, position int, monthly, renews string) qc.Entry {
	return qc.Entry{
		Provider:  qc.ProviderAnthropicAPI,
		AuthIndex: qc.APICreditAccount(label),
		APICredit: &qc.APICredit{
			Label: label, Position: position, MonthlyUSD: monthly, Renews: renews,
			KeyFingerprint: fmt.Sprintf("key-%012d", position+1),
		},
	}
}

// withReport stores a successful read made at observed, asking from start. Each
// of cents is one consecutive day's amount in Anthropic's lowest units; "" is a
// day Anthropic reported with no spend.
func withReport(entry qc.Entry, observed time.Time, org string, start time.Time, cents ...string) qc.Entry {
	days := make([]qc.CostDay, 0, len(cents))
	for i, amount := range cents {
		day := qc.CostDay{StartingAt: start.Add(time.Duration(i) * 24 * time.Hour), Amounts: []qc.CostAmount{}}
		if amount != "" {
			day.Amounts = append(day.Amounts, qc.CostAmount{Amount: amount, Currency: "USD"})
		}
		days = append(days, day)
	}
	entry.LastAttempt, entry.NextAttempt = observed, observed.Add(15*time.Minute)
	entry.Quota = &qc.Quota{Schema: 1, ObservedAt: observed, CostReport: &qc.CostReport{
		OrganizationID: org,
		KeyFingerprint: entry.APICredit.KeyFingerprint,
		StartingAt:     start,
		EndingAt:       qc.CostReportEndingAt(observed),
		Days:           days,
	}}
	return entry
}

func instant(t *testing.T, text string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func buildCredits(t *testing.T, now time.Time, staleAfter time.Duration, entries ...qc.Entry) *APICredits {
	t.Helper()
	snapshot := qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{}}
	for _, entry := range entries {
		snapshot.Entries[qc.Key(entry.Provider, entry.AuthIndex)] = entry
	}
	doc := Build(Input{Snapshot: snapshot, StaleAfter: staleAfter}, now)
	if doc.APICredits == nil {
		t.Fatal("apiCredits is null with anthropic-api entries in the snapshot")
	}
	checkCreditPoolAddsUp(t, doc.APICredits)
	return doc.APICredits
}

// checkCreditPoolAddsUp holds every document to the sums the contract
// promises, so a fixture cannot drift from them.
func checkCreditPoolAddsUp(t *testing.T, credits *APICredits) {
	t.Helper()
	pool := credits.Pool
	if pool.AccountCount != len(credits.Accounts) || pool.CountedCount+pool.MissingCount+pool.DuplicateCount != pool.AccountCount {
		t.Errorf("counts do not add up: %+v over %d accounts", pool, len(credits.Accounts))
	}
	counted, duplicates := 0, 0
	for _, account := range credits.Accounts {
		if account.HasReading {
			counted++
		}
		if account.State == StateDuplicate {
			duplicates++
		}
		if !account.HasReading && (account.Spent != 0 || account.SpentText != "" || account.LeftText != "" || account.Level != "" || len(account.DailySpend) != 0) {
			t.Errorf("%s has no reading but carries figures: %+v", account.ID, account)
		}
		if account.DataIssues == nil || account.DailySpend == nil {
			t.Errorf("%s: arrays must never be null", account.ID)
		}
	}
	if counted != pool.CountedCount || duplicates != pool.DuplicateCount || pool.HasReading != (counted > 0) {
		t.Errorf("pool counts %d counted and %d duplicate; accounts say %d and %d", pool.CountedCount, pool.DuplicateCount, counted, duplicates)
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

// The worked example in the spec and the contract, field by field.
func TestAPICreditsWorkedExample(t *testing.T) {
	now := instant(t, "2026-10-09T12:00:00Z")
	observed := instant(t, "2026-10-09T11:55:00Z")
	a := withReport(creditEntry("siphorchannel", 0, "200", "2026-10-29"), observed,
		"5b1e3c9a-0d1f-4c8e-9a51-7d0c2b6e4f10", instant(t, "2026-09-29T00:00:00Z"),
		"1250", "", "2000.5", "", "912.125", "", "", "", "837.5", "", "")
	b := withReport(creditEntry("agency-team", 1, "500", "2026-10-03"), observed,
		"c2d4e6f8-0000-4000-8000-000000000002", instant(t, "2026-10-03T00:00:00Z"),
		"20000", "15000.5", "19009.5", "", "", "", "")
	c := creditEntry("noor", 2, "100", "2026-10-31")
	c.NextAttempt = instant(t, "2026-10-09T12:15:00Z")
	credits := buildCredits(t, now, 45*time.Minute, a, b, c)

	if credits.Title != "Claude API credits" || credits.Currency != "USD" {
		t.Errorf("title %q currency %q", credits.Title, credits.Currency)
	}
	wantPool := APICreditPool{
		HasReading:    true,
		MonthlyCredit: 700, MonthlyCreditText: "$700.00",
		Spent: 590.10125, SpentText: "$590.10",
		CreditUsed: 550.00125, CreditUsedText: "$550.00",
		Left: 149.99875, LeftText: "$150.00",
		Overage: 40.1, OverageText: "$40.10",
		RemainingFraction: 0.21428392857142858, RemainingPercent: 21, Level: LevelLow,
		NextRefill: &APICreditRefill{
			AccountIDs:    []string{"label-9611d9ba844a"},
			RefillAtEpoch: 1793232000, RefillInSeconds: 1684800,
			Gain: 50.00125, GainText: "$50.00",
			GainFraction: 0.07143035714285714, GainPercent: 7,
		},
		FullAtEpoch: epoch(1793664000), FullInSeconds: epoch(2116800),
		AccountCount: 3, CountedCount: 2, MissingCount: 1, DuplicateCount: 0,
	}
	if !reflect.DeepEqual(credits.Pool, wantPool) {
		t.Errorf("pool:\n got %s\nwant %s", jsonOf(t, credits.Pool), jsonOf(t, wantPool))
	}

	day := func(epoch int64, spent float64, text string) APICreditDay {
		return APICreditDay{DayStartEpoch: epoch, Spent: spent, SpentText: text}
	}
	const d = 86400
	wantA := APICreditAccount{
		ID: "label-9611d9ba844a", Label: "siphorchannel", Order: 0,
		OrganizationID: "5b1e3c9a-0d1f-4c8e-9a51-7d0c2b6e4f10",
		HasReading:     true,
		MonthlyCredit:  200, MonthlyCreditText: "$200.00",
		Spent: 50.00125, SpentText: "$50.00",
		CreditUsed: 50.00125, CreditUsedText: "$50.00",
		Left: 149.99875, LeftText: "$150.00",
		Overage: 0, OverageText: "$0.00",
		RemainingFraction: 0.74999375, RemainingPercent: 75, Level: LevelOK,
		CycleStartEpoch: epoch(1790640000), RenewsAtEpoch: epoch(1793232000), RenewsInSeconds: epoch(1684800),
		DailySpend: []APICreditDay{
			day(1790640000, 12.5, "$12.50"),
			day(1790640000+1*d, 0, "$0.00"),
			day(1790640000+2*d, 20.005, "$20.01"),
			day(1790640000+3*d, 0, "$0.00"),
			day(1790640000+4*d, 9.12125, "$9.12"),
			day(1790640000+5*d, 0, "$0.00"),
			day(1790640000+6*d, 0, "$0.00"),
			day(1790640000+7*d, 0, "$0.00"),
			day(1790640000+8*d, 8.375, "$8.38"),
			day(1790640000+9*d, 0, "$0.00"),
			day(1791504000, 0, "$0.00"),
		},
		ObservedAtEpoch: epoch(1791546900), NextAttemptEpoch: epoch(1791547800),
		State: StatusOK, DataIssues: []string{}, Issue: "",
	}
	wantB := APICreditAccount{
		ID: "label-aa6943c97255", Label: "agency-team", Order: 1,
		OrganizationID: "c2d4e6f8-0000-4000-8000-000000000002",
		HasReading:     true,
		MonthlyCredit:  500, MonthlyCreditText: "$500.00",
		Spent: 540.1, SpentText: "$540.10",
		CreditUsed: 500, CreditUsedText: "$500.00",
		Left: 0, LeftText: "$0.00",
		Overage: 40.1, OverageText: "$40.10",
		RemainingFraction: 0, RemainingPercent: 0, Level: LevelCritical,
		CycleStartEpoch: epoch(1790985600), RenewsAtEpoch: epoch(1793664000), RenewsInSeconds: epoch(2116800),
		DailySpend: []APICreditDay{
			day(1790985600, 200, "$200.00"),
			day(1790985600+1*d, 150.005, "$150.01"),
			day(1790985600+2*d, 190.095, "$190.10"),
			day(1790985600+3*d, 0, "$0.00"),
			day(1790985600+4*d, 0, "$0.00"),
			day(1790985600+5*d, 0, "$0.00"),
			day(1790985600+6*d, 0, "$0.00"),
		},
		ObservedAtEpoch: epoch(1791546900), NextAttemptEpoch: epoch(1791547800),
		State: StatusOK, DataIssues: []string{}, Issue: "",
	}
	wantC := APICreditAccount{
		ID: "label-92a70799e19d", Label: "noor", Order: 2,
		MonthlyCredit: 100, MonthlyCreditText: "$100.00",
		CycleStartEpoch: epoch(1790726400), RenewsAtEpoch: epoch(1793404800), RenewsInSeconds: epoch(1857600),
		DailySpend:       []APICreditDay{},
		NextAttemptEpoch: epoch(1791548100),
		State:            StatusPending, DataIssues: []string{}, Issue: "Waiting for the first reading.",
	}
	for i, want := range []APICreditAccount{wantA, wantB, wantC} {
		if !reflect.DeepEqual(credits.Accounts[i], want) {
			t.Errorf("account %d:\n got %s\nwant %s", i, jsonOf(t, credits.Accounts[i]), jsonOf(t, want))
		}
	}
}

func jsonOf(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
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
	if raw := jsonOf(t, doc); !strings.Contains(raw, `"apiCredits":null`) {
		t.Fatal(`the document must carry "apiCredits":null`)
	}
}

// Every row of the state table, first match winning.
func TestAPICreditStates(t *testing.T) {
	now := instant(t, "2026-10-09T12:00:00Z")
	fresh := instant(t, "2026-10-09T11:55:00Z")
	start := instant(t, "2026-09-29T00:00:00Z")
	days := []string{"1250", "", "", "", "", "", "", "", "", "", ""} // Sep 29 to Oct 9
	healthy := func() qc.Entry {
		return withReport(creditEntry("siphorchannel", 0, "200", "2026-10-29"), fresh, "org-a", start, days...)
	}
	for _, tc := range []struct {
		name       string
		entry      func() qc.Entry
		state      string
		hasReading bool
		issues     []string
		issue      string
	}{
		{"ok", healthy, StatusOK, true, []string{}, ""},
		{"misconfigured", func() qc.Entry {
			e := creditEntry("siphorchannel", 0, "200", "")
			e.APICredit.Problem = qc.CreditProblemRenewsInvalid
			return e
		}, StateMisconfigured, false, []string{issueMisconfigured}, `renews must be a date like "2026-10-29".`},
		{"misconfigured with a reading left from before", func() qc.Entry {
			e := healthy()
			e.APICredit.Problem = qc.CreditProblemUnknownField
			return e
		}, StateMisconfigured, false, []string{issueMisconfigured}, "This item has a setting Quota Cache does not know; check the spelling of label, admin-key, monthly-usd and renews."},
		{"unknown problem", func() qc.Entry {
			e := creditEntry("siphorchannel", 0, "200", "2026-10-29")
			e.APICredit.Problem = "from_a_newer_quota_cache"
			return e
		}, StateMisconfigured, false, []string{issueMisconfigured}, "This item's configuration has a problem."},
		{"no configuration at all", func() qc.Entry {
			e := healthy()
			e.APICredit = nil
			return e
		}, StateMisconfigured, false, []string{issueMisconfigured}, "This item's configuration has a problem."},
		{"failing, reading kept", func() qc.Entry {
			e := healthy()
			e.Failures, e.LastError = 2, qc.CreditErrorUpstream
			return e
		}, StatusError, true, []string{issueObserveError}, "The last read failed; Quota Cache will retry."},
		{"key rejected", func() qc.Entry {
			e := creditEntry("siphorchannel", 0, "200", "2026-10-29")
			e.Failures, e.LastError = 3, qc.CreditErrorKeyRejected
			return e
		}, StatusError, false, []string{issueObserveError, issueKeyRejected}, "Anthropic rejected this admin key. It may be revoked or expired; create a new one in Console under Settings > Admin keys."},
		{"key forbidden", func() qc.Entry {
			e := creditEntry("siphorchannel", 0, "200", "2026-10-29")
			e.Failures, e.LastError = 1, qc.CreditErrorForbidden
			return e
		}, StatusError, false, []string{issueObserveError, issueKeyForbidden}, "This key cannot read the cost report. Use an Admin API key from an organization admin."},
		{"cost report unavailable", func() qc.Entry {
			e := creditEntry("siphorchannel", 0, "200", "2026-10-29")
			e.Failures, e.LastError = 1, qc.CreditErrorUnavailable
			return e
		}, StatusError, false, []string{issueObserveError, issueCostReportUnavailable}, "Anthropic has no cost report for this organization."},
		{"rate limited", func() qc.Entry {
			e := healthy()
			e.Failures, e.LastError = 1, "provider rate limited"
			return e
		}, StatusError, true, []string{issueObserveError, issueRateLimited}, "Anthropic is rate limiting these reads; Quota Cache will retry."},
		{"unsupported currency", func() qc.Entry {
			e := healthy()
			e.Quota.CostReport.Days[0].Amounts[0].Currency = "EUR"
			return e
		}, StatusError, false, []string{issueUnsupportedCurrency}, "Anthropic's report could not be read as US dollars."},
		{"amount invalid", func() qc.Entry {
			e := healthy()
			e.Quota.CostReport.Days[0].Amounts[0].Amount = "1/3"
			return e
		}, StatusError, false, []string{issueAmountInvalid}, "Anthropic's report could not be read as US dollars."},
		{"never read", func() qc.Entry {
			return creditEntry("siphorchannel", 0, "200", "2026-10-29")
		}, StatusPending, false, []string{}, "Waiting for the first reading."},
		{"never read, poll in flight", func() qc.Entry {
			e := creditEntry("siphorchannel", 0, "200", "2026-10-29")
			e.LastError = "refresh pending"
			return e
		}, StatusPending, false, []string{issueRefreshPending}, "Waiting for the first reading."},
		{"previous cycle", func() qc.Entry {
			return withReport(creditEntry("siphorchannel", 0, "200", "2026-10-29"), instant(t, "2026-09-28T23:00:00Z"), "org-a",
				instant(t, "2026-08-29T00:00:00Z"), make([]string, 31)...)
		}, StatusPending, false, []string{issuePreviousCycle}, "Renewed since the last reading; waiting for this cycle's first reading."},
		{"key changed", func() qc.Entry {
			e := healthy()
			e.APICredit.KeyFingerprint = "key-0000000000ff"
			return e
		}, StatusPending, false, []string{issueKeyChanged}, "Waiting for the first reading with the new admin key."},
		{"renewal edited earlier", func() qc.Entry {
			e := healthy()
			e.APICredit.Renews = "2026-10-20" // cycle now starts Sep 20, before the report
			return e
		}, StatusPending, false, []string{issueIncompleteCycle}, "Waiting for a reading that covers this whole cycle."},
		{"coverage stops before yesterday", func() qc.Entry {
			e := healthy()
			e.Quota.CostReport.Days = e.Quota.CostReport.Days[:9] // through Oct 7
			return e
		}, StatusPending, false, []string{issueIncompleteCycle}, "Waiting for a reading that covers this whole cycle."},
		{"stale, judged by quota.observed_at", func() qc.Entry {
			return withReport(creditEntry("siphorchannel", 0, "200", "2026-10-29"), instant(t, "2026-10-09T10:00:00Z"), "org-a", start, days...)
		}, StateStale, true, []string{issueStale}, "This reading is out of date; Quota Cache has not refreshed it."},
		{"ok, poll in flight", func() qc.Entry {
			e := healthy()
			e.LastError = "refresh pending"
			return e
		}, StatusOK, true, []string{issueRefreshPending}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := tc.entry()
			// quota-cache leaves the legacy observation empty on this provider;
			// staleness has to come from quota.observed_at regardless.
			if !entry.ObservedAt.IsZero() {
				t.Fatal("fixture sets the top-level observed_at")
			}
			account := buildCredits(t, now, 45*time.Minute, entry).Accounts[0]
			if account.State != tc.state || account.HasReading != tc.hasReading ||
				!reflect.DeepEqual(account.DataIssues, tc.issues) || account.Issue != tc.issue {
				t.Errorf("state=%s hasReading=%v issues=%v issue=%q\nwant state=%s hasReading=%v issues=%v issue=%q",
					account.State, account.HasReading, account.DataIssues, account.Issue,
					tc.state, tc.hasReading, tc.issues, tc.issue)
			}
		})
	}
}

// A misconfigured item is never polled, so a next attempt scheduled before the
// problem appeared must not be promised; the configured facts that are valid
// still show.
func TestMisconfiguredAccountHasNoNextAttempt(t *testing.T) {
	entry := creditEntry("noorchasib", 3, "200", "")
	entry.APICredit.Problem = qc.CreditProblemRenewsInvalid
	entry.NextAttempt = at(t, 600)
	account := buildCredits(t, at(t, 0), 45*time.Minute, entry).Accounts[0]
	if account.NextAttemptEpoch != nil {
		t.Errorf("nextAttemptEpoch = %d", *account.NextAttemptEpoch)
	}
	if account.MonthlyCreditText != "$200.00" || account.CycleStartEpoch != nil || account.RenewsAtEpoch != nil || account.RenewsInSeconds != nil {
		t.Errorf("configured facts: %+v", account)
	}
}

// An entry with no configuration at all has no position. Its order of -1 and
// empty label would sort it first on every other key, so it must be placed
// after every configured account explicitly.
func TestUnconfiguredAccountSortsLast(t *testing.T) {
	now := instant(t, "2026-10-09T12:00:00Z")
	observed := instant(t, "2026-10-09T11:55:00Z")
	start := instant(t, "2026-09-29T00:00:00Z")
	orphan := withReport(creditEntry("aaa-orphan", 0, "200", "2026-10-29"), observed, "org-orphan", start, "1250")
	orphan.APICredit = nil
	first := creditEntry("zz-first", 0, "200", "2026-10-29")
	second := creditEntry("mm-second", 1, "100", "2026-10-15")
	credits := buildCredits(t, now, 45*time.Minute, orphan, second, first)

	if len(credits.Accounts) != 3 {
		t.Fatalf("accounts: %+v", credits.Accounts)
	}
	if credits.Accounts[0].Label != "zz-first" || credits.Accounts[1].Label != "mm-second" {
		t.Errorf("configured order: %q, %q", credits.Accounts[0].Label, credits.Accounts[1].Label)
	}
	last := credits.Accounts[2]
	if last.ID != qc.APICreditAccount("aaa-orphan") || last.Order != -1 || last.State != StateMisconfigured || last.HasReading {
		t.Errorf("unconfigured entry is not last: %+v", last)
	}
}

// Today's bucket missing is the one gap quota-cache lets through. The account
// is counted on every complete day, says so, and never fills today with $0.
func TestTodayNotReportedIsCountedThroughYesterday(t *testing.T) {
	now := instant(t, "2026-10-09T00:20:00Z")
	entry := withReport(creditEntry("siphorchannel", 0, "200", "2026-10-29"), instant(t, "2026-10-09T00:10:00Z"), "org-a",
		instant(t, "2026-09-29T00:00:00Z"), "1250", "", "", "", "", "", "", "", "", "500") // Sep 29 to Oct 8
	account := buildCredits(t, now, 45*time.Minute, entry).Accounts[0]
	if !account.HasReading || account.State != StatusOK || account.SpentText != "$17.50" {
		t.Fatalf("account = %+v", account)
	}
	if !reflect.DeepEqual(account.DataIssues, []string{issueTodayNotReported}) ||
		account.Issue != "Anthropic has not reported today's spend yet; spent covers the cycle through yesterday." {
		t.Errorf("issues=%v issue=%q", account.DataIssues, account.Issue)
	}
	if len(account.DailySpend) != 10 || account.DailySpend[9].DayStartEpoch != instant(t, "2026-10-08T00:00:00Z").Unix() {
		t.Errorf("dailySpend must end at yesterday: %+v", account.DailySpend)
	}

	// In another state that state's sentence wins, and the flag stays.
	entry.Failures, entry.LastError = 1, qc.CreditErrorUpstream
	account = buildCredits(t, now, 45*time.Minute, entry).Accounts[0]
	if account.State != StatusError || !account.HasReading ||
		!reflect.DeepEqual(account.DataIssues, []string{issueObserveError, issueTodayNotReported}) ||
		account.Issue != "The last read failed; Quota Cache will retry." {
		t.Errorf("failing: %+v", account)
	}

	// A reading from yesterday covering yesterday is complete for its own
	// instant: stale, not today-not-reported.
	old := withReport(creditEntry("siphorchannel", 0, "200", "2026-10-29"), instant(t, "2026-10-08T12:00:00Z"), "org-a",
		instant(t, "2026-09-29T00:00:00Z"), make([]string, 10)...)
	account = buildCredits(t, now, 45*time.Minute, old).Accounts[0]
	if account.State != StateStale || !reflect.DeepEqual(account.DataIssues, []string{issueStale}) {
		t.Errorf("yesterday's reading: %+v", account)
	}
}

// Renewal day is today: the cycle began at midnight, and a rollover poll that
// found no bucket yet is a counted $0 waiting for today, not a missing account.
func TestRenewedTodayCountsOnlyToday(t *testing.T) {
	now := instant(t, "2026-10-09T04:00:00Z")
	start := instant(t, "2026-10-09T00:00:00Z")
	empty := withReport(creditEntry("agency-team", 0, "500", "2026-11-09"), instant(t, "2026-10-09T00:00:30Z"), "org-t", start)
	account := buildCredits(t, now, 0, empty).Accounts[0]
	if !account.HasReading || account.SpentText != "$0.00" || account.RemainingPercent != 100 ||
		!reflect.DeepEqual(account.DataIssues, []string{issueTodayNotReported}) || len(account.DailySpend) != 0 {
		t.Errorf("rollover poll: %+v", account)
	}
	if *account.CycleStartEpoch != start.Unix() || *account.RenewsAtEpoch != instant(t, "2026-11-09T00:00:00Z").Unix() {
		t.Errorf("cycle %d to %d", *account.CycleStartEpoch, *account.RenewsAtEpoch)
	}

	// A reading of the old cycle, taken a second before the renewal, is the
	// previous cycle's from the renewal instant on.
	last := withReport(creditEntry("agency-team", 0, "500", "2026-11-09"), start.Add(-time.Second), "org-t",
		instant(t, "2026-09-09T00:00:00Z"), make([]string, 30)...)
	account = buildCredits(t, start, 0, last).Accounts[0]
	if account.HasReading || !reflect.DeepEqual(account.DataIssues, []string{issuePreviousCycle}) {
		t.Errorf("at the renewal second: %+v", account)
	}
	account = buildCredits(t, start.Add(-time.Second), 0, last).Accounts[0]
	if !account.HasReading {
		t.Errorf("a second before the renewal: %+v", account)
	}
}

// The cycle comes from the one shared rule, clamped to each month's length.
func TestAPICreditCycleClampsAcrossFebruary(t *testing.T) {
	for _, tc := range []struct {
		renews, now, start, end string
	}{
		{"2026-10-31", "2027-02-15T12:00:00Z", "2027-01-31T00:00:00Z", "2027-02-28T00:00:00Z"},
		{"2026-10-31", "2027-03-01T12:00:00Z", "2027-02-28T00:00:00Z", "2027-03-31T00:00:00Z"},
		{"2026-10-30", "2028-02-28T12:00:00Z", "2028-01-30T00:00:00Z", "2028-02-29T00:00:00Z"},
		{"2026-10-30", "2028-02-29T12:00:00Z", "2028-02-29T00:00:00Z", "2028-03-30T00:00:00Z"},
		{"2026-10-30", "2028-03-01T12:00:00Z", "2028-02-29T00:00:00Z", "2028-03-30T00:00:00Z"},
		{"2026-10-29", "2027-02-28T12:00:00Z", "2027-02-28T00:00:00Z", "2027-03-29T00:00:00Z"},
		{"2026-10-29", "2028-02-28T12:00:00Z", "2028-01-29T00:00:00Z", "2028-02-29T00:00:00Z"},
	} {
		now := instant(t, tc.now)
		account := buildCredits(t, now, 0, creditEntry("noor", 0, "100", tc.renews)).Accounts[0]
		start, end := instant(t, tc.start), instant(t, tc.end)
		if *account.CycleStartEpoch != start.Unix() || *account.RenewsAtEpoch != end.Unix() ||
			*account.RenewsInSeconds != end.Unix()-now.Unix() {
			t.Errorf("renews %s at %s: %s to %s, want %s to %s", tc.renews, tc.now,
				time.Unix(*account.CycleStartEpoch, 0).UTC(), time.Unix(*account.RenewsAtEpoch, 0).UTC(), tc.start, tc.end)
		}
	}
}

func TestAPICreditMoneyEdges(t *testing.T) {
	now := instant(t, "2026-10-09T12:00:00Z")
	observed := instant(t, "2026-10-09T11:55:00Z")
	start := instant(t, "2026-10-09T00:00:00Z")
	read := func(monthly string, cents ...string) APICreditAccount {
		entry := withReport(creditEntry("noor", 0, monthly, "2026-11-09"), observed, "org-n", start, cents...)
		return buildCredits(t, now, 45*time.Minute, entry).Accounts[0]
	}

	// Spent exactly the credit: nothing left, nothing over.
	out := read("100", "10000")
	if out.LeftText != "$0.00" || out.OverageText != "$0.00" || out.CreditUsedText != "$100.00" || out.Level != LevelCritical {
		t.Errorf("out: %+v", out)
	}
	// Overspent: the credit is used up and the rest is overage.
	over := read("100", "12345.678")
	if over.Left != 0 || over.CreditUsed != 100 || over.Overage != 23.45678 || over.OverageText != "$23.46" || over.Level != LevelCritical {
		t.Errorf("overspent: %+v", over)
	}
	// No credit this month: everything is overage, and there is nothing to
	// show as a fraction.
	zero := read("0", "250")
	if zero.MonthlyCreditText != "$0.00" || zero.Left != 0 || zero.Overage != 2.5 || zero.RemainingFraction != 0 || zero.Level != LevelCritical {
		t.Errorf("zero monthly: %+v", zero)
	}
	// A credit back: spent may go below zero, the credit used never does, and
	// nothing prints as -$0.00.
	refund := read("100", "-0.4")
	if refund.Spent != -0.004 || refund.SpentText != "$0.00" || refund.CreditUsed != 0 || refund.CreditUsedText != "$0.00" ||
		refund.LeftText != "$100.00" || refund.RemainingPercent != 100 {
		t.Errorf("refund: %+v", refund)
	}
	refund = read("100", "-150")
	if refund.SpentText != "-$1.50" || refund.CreditUsed != 0 || refund.Left != 100 {
		t.Errorf("larger refund: %+v", refund)
	}
	// Half a cent rounds away from zero on the exact value.
	half := read("100", "2000.5")
	if half.SpentText != "$20.01" || half.Spent != 20.005 {
		t.Errorf("half a cent: %+v", half)
	}
	// Several results in one day are all summed.
	entry := withReport(creditEntry("noor", 0, "100", "2026-11-09"), observed, "org-n", start, "100")
	entry.Quota.CostReport.Days[0].Amounts = append(entry.Quota.CostReport.Days[0].Amounts, qc.CostAmount{Amount: "0.5", Currency: "USD"})
	several := buildCredits(t, now, 0, entry).Accounts[0]
	if several.Spent != 1.005 || several.DailySpend[0].SpentText != "$1.01" {
		t.Errorf("several results: %+v", several)
	}
}

// One organization configured twice is counted once, under the item listed
// first, whatever the labels or ids would sort to.
func TestDuplicateOrganizationCountsOnceUnderTheFirst(t *testing.T) {
	now := instant(t, "2026-10-09T12:00:00Z")
	observed := instant(t, "2026-10-09T11:55:00Z")
	start := instant(t, "2026-10-09T00:00:00Z")
	first := withReport(creditEntry("zz-team-seat", 0, "500", "2026-11-09"), observed, "org-team", start, "1000")
	second := withReport(creditEntry("aa-team-seat", 1, "500", "2026-11-09"), observed, "org-team", start, "1000")
	other := withReport(creditEntry("noor", 2, "100", "2026-11-09"), observed, "org-noor", start, "1000")
	credits := buildCredits(t, now, 45*time.Minute, second, other, first)

	if credits.Accounts[0].Label != "zz-team-seat" || credits.Accounts[0].State != StatusOK || !credits.Accounts[0].HasReading {
		t.Errorf("first: %+v", credits.Accounts[0])
	}
	dup := credits.Accounts[1]
	if dup.Label != "aa-team-seat" || dup.State != StateDuplicate || dup.HasReading || dup.SpentText != "" ||
		dup.OrganizationID != "org-team" || dup.MonthlyCreditText != "$500.00" ||
		!reflect.DeepEqual(dup.DataIssues, []string{issueDuplicateOrganization}) ||
		dup.Issue != "Same Console organization as zz-team-seat; counted there." {
		t.Errorf("duplicate: %+v", dup)
	}
	if credits.Pool.MonthlyCredit != 600 || credits.Pool.Spent != 20 || credits.Pool.DuplicateCount != 1 || credits.Pool.CountedCount != 2 {
		t.Errorf("pool: %+v", credits.Pool)
	}

	// A reading made with a key since replaced identifies nothing.
	second.APICredit.KeyFingerprint = "key-0000000000ff"
	credits = buildCredits(t, now, 45*time.Minute, first, second)
	if credits.Accounts[1].State != StatusPending || credits.Accounts[1].OrganizationID != "" || credits.Pool.DuplicateCount != 0 {
		t.Errorf("rotated key: %+v", credits.Accounts[1])
	}
}

// Accounts renewing at the same second refill together.
func TestNextRefillMergesTies(t *testing.T) {
	now := instant(t, "2026-10-09T12:00:00Z")
	observed := instant(t, "2026-10-09T11:55:00Z")
	a := withReport(creditEntry("siphorchannel", 0, "200", "2026-10-20"), observed, "org-a", instant(t, "2026-09-20T00:00:00Z"), make([]string, 19)...)
	a.Quota.CostReport.Days[0].Amounts = []qc.CostAmount{{Amount: "5000", Currency: "USD"}}
	b := withReport(creditEntry("chasibnoor", 1, "100", "2026-10-20"), observed, "org-b", instant(t, "2026-09-20T00:00:00Z"), make([]string, 19)...)
	b.Quota.CostReport.Days[3].Amounts = []qc.CostAmount{{Amount: "12000", Currency: "USD"}}
	// Renews sooner but has used nothing, so it refills nothing.
	c := withReport(creditEntry("noor", 2, "100", "2026-10-12"), observed, "org-c", instant(t, "2026-09-12T00:00:00Z"), make([]string, 27)...)
	d := withReport(creditEntry("noorchasib", 3, "100", "2026-10-25"), observed, "org-d", instant(t, "2026-09-25T00:00:00Z"), make([]string, 14)...)
	d.Quota.CostReport.Days[0].Amounts = []qc.CostAmount{{Amount: "100", Currency: "USD"}}
	pool := buildCredits(t, now, 45*time.Minute, a, b, c, d).Pool

	refill := pool.NextRefill
	if refill == nil || !reflect.DeepEqual(refill.AccountIDs, []string{qc.APICreditAccount("siphorchannel"), qc.APICreditAccount("chasibnoor")}) ||
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
	misconfigured := creditEntry("noor", 1, "", "2026-10-29")
	misconfigured.APICredit.Problem = qc.CreditProblemMonthlyMissing
	credits := buildCredits(t, at(t, 0), 45*time.Minute, creditEntry("siphorchannel", 0, "200", "2026-10-29"), misconfigured)
	want := APICreditPool{AccountCount: 2, MissingCount: 2}
	if !reflect.DeepEqual(credits.Pool, want) {
		t.Errorf("pool = %+v", credits.Pool)
	}
	if raw := jsonOf(t, credits.Pool); !strings.Contains(raw, `"nextRefill":null`) || !strings.Contains(raw, `"fullAtEpoch":null`) || !strings.Contains(raw, `"level":""`) {
		t.Errorf("pool JSON = %s", raw)
	}
}

// Moving the clock moves every countdown and no instant.
func TestAPICreditCountdownsFollowTheClock(t *testing.T) {
	first := buildFixture(t).APICredits
	later := Build(Input{
		Snapshot:   loadSnapshot(t, "seven-credentials.json"),
		Identities: fixtureRoster(),
		StaleAfter: 45 * time.Minute,
	}, at(t, 900)).APICredits
	if first.Pool.NextRefill == nil || later.Pool.NextRefill == nil {
		t.Fatal("the fixture pool has a refill")
	}
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

// The admin key never reaches quota-glance's input, so it cannot reach the
// document; this pins that nothing in the document is shaped like one.
func TestAPICreditDocumentCarriesNoKey(t *testing.T) {
	for _, doc := range []Document{buildFixture(t), buildDegraded(t)} {
		if raw := jsonOf(t, doc); strings.Contains(raw, "sk-ant-") {
			t.Fatal("a document contains something shaped like an Anthropic key")
		}
	}
}

// The happy-path fixture is the card the design is drawn against: one
// organization per pool shape, all read four hours into fixture day.
func TestGoldenAPICreditsShowEveryPoolShape(t *testing.T) {
	credits := buildFixture(t).APICredits
	if credits == nil {
		t.Fatal("apiCredits is null")
	}
	for _, tc := range []struct {
		label, level, spent, left, overage string
	}{
		{"siphorchannel", LevelOK, "$40.00", "$160.00", "$0.00"},
		{"chasibnoor", LevelLow, "$68.00", "$32.00", "$0.00"},
		{"noor", LevelCritical, "$100.00", "$0.00", "$0.00"},
		{"noorchasib", LevelCritical, "$236.40", "$0.00", "$36.40"},
		{"agency-team", LevelOK, "$18.35", "$481.65", "$0.00"},
	} {
		account := creditAccountByLabel(t, credits, tc.label)
		if account.State != StatusOK || !account.HasReading || account.Level != tc.level ||
			account.SpentText != tc.spent || account.LeftText != tc.left || account.OverageText != tc.overage {
			t.Errorf("%s: state=%s level=%s spent=%s left=%s overage=%s", tc.label,
				account.State, account.Level, account.SpentText, account.LeftText, account.OverageText)
		}
	}
	// Renewed at midnight: a cycle four hours old, holding today alone.
	team := creditAccountByLabel(t, credits, "agency-team")
	if *team.CycleStartEpoch != at(t, -4*3600).Unix() || len(team.DailySpend) != 1 {
		t.Errorf("renewed today: %+v", team)
	}
	pool := credits.Pool
	if pool.CountedCount != 5 || pool.NextRefill == nil || pool.NextRefill.GainText != "$100.00" ||
		!reflect.DeepEqual(pool.NextRefill.AccountIDs, []string{qc.APICreditAccount("noor")}) {
		t.Errorf("pool: %+v", pool)
	}
}

// Every state and issue the web app has to render for an API credit account
// must appear in the degraded contract.
func TestDegradedContractCoversEveryAPICreditState(t *testing.T) {
	credits := buildDegraded(t).APICredits
	if credits == nil {
		t.Fatal("apiCredits is null")
	}
	checkCreditPoolAddsUp(t, credits)
	states, issues := map[string]bool{}, map[string]bool{}
	var sawNullNextAttempt, sawCountedError bool
	for _, account := range credits.Accounts {
		states[account.State] = true
		for _, issue := range account.DataIssues {
			issues[issue] = true
		}
		if account.State == StateMisconfigured && account.NextAttemptEpoch == nil {
			sawNullNextAttempt = true
		}
		if account.State == StatusError && account.HasReading {
			sawCountedError = true
		}
	}
	for _, want := range []string{StatusOK, StateStale, StatusError, StatusPending, StateMisconfigured, StateDuplicate} {
		if !states[want] {
			t.Errorf("no account with state %q", want)
		}
	}
	for _, want := range []string{issueStale, issueObserveError, issueKeyRejected, issueTodayNotReported, issueMisconfigured, issueDuplicateOrganization} {
		if !issues[want] {
			t.Errorf("no dataIssue %q", want)
		}
	}
	if !sawNullNextAttempt || !sawCountedError {
		t.Errorf("missing: misconfiguredNullNextAttempt=%v countedError=%v", sawNullNextAttempt, sawCountedError)
	}
	if credits.Pool.CountedCount != 3 || credits.Pool.MissingCount != 3 || credits.Pool.DuplicateCount != 1 {
		t.Errorf("pool counts: %+v", credits.Pool)
	}
}
