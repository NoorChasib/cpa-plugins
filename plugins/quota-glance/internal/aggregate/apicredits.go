package aggregate

import (
	"math/big"
	"sort"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// The states an API credit account can be in beyond the ones every entry
// shares. A misconfigured item is one quota-cache refuses to poll; a duplicate
// reads the same Console organization as an item listed before it.
const (
	StateMisconfigured = "misconfigured"
	StateDuplicate     = "duplicate"
)

// Data issues particular to API credit accounts. The ones that explain a
// missing reading — previousCycle, keyChanged, incompleteCycle — are waits
// that the next poll clears; unsupportedCurrency and amountInvalid are not.
const (
	issueTodayNotReported      = "todayNotReported"
	issueKeyRejected           = "keyRejected"
	issueKeyForbidden          = "keyForbidden"
	issueCostReportUnavailable = "costReportUnavailable"
	issueRateLimited           = "rateLimited"
	issuePreviousCycle         = "previousCycle"
	issueKeyChanged            = "keyChanged"
	issueIncompleteCycle       = "incompleteCycle"
	issueUnsupportedCurrency   = "unsupportedCurrency"
	issueAmountInvalid         = "amountInvalid"
	issueMisconfigured         = "misconfigured"
	issueDuplicateOrganization = "duplicateOrganization"
)

// The two last_error values every quota-cache provider can carry, beside the
// anthropic-api ones the client package names.
const (
	lastErrorRefreshPending = "refresh pending"
	lastErrorRateLimited    = "provider rate limited"
)

// centsPerDollar converts the cost report's lowest units into the dollars the
// credit is configured in. Exactly: "912.125" cents is $9.12125.
var centsPerDollar = big.NewRat(100, 1)

// creditAccount is one account while the pool is worked out: the document's
// view of it, and the exact amounts its float fields are converted from.
type creditAccount struct {
	out    APICreditAccount
	entry  qc.Entry
	credit *qc.APICredit
	// report is the stored cost report when it was read with the configured
	// key, and nil otherwise: a reading from another key may describe another
	// organization, so nothing here trusts it.
	report   *qc.CostReport
	cycle    qc.CreditCycle
	hasCycle bool
	monthly  *big.Rat
	// reading is the current cycle's spend, nil without one; reason is why
	// there is none, "" when there is one or nothing was ever read.
	reading *creditReading
	reason  string
	// duplicateOf is the account listed earlier that read the same
	// organization, and is counted in this one's place.
	duplicateOf *creditAccount
}

// creditReading is an account's cycle so far, every figure exact.
type creditReading struct {
	spent, used, left, overage *big.Rat
	days                       []APICreditDay
	// todayMissing is true when the report stopped at the start of the day it
	// was read on: every complete day is there, today's bucket is not.
	todayMissing bool
}

// apiCreditsOf pools every anthropic-api entry in the snapshot, and is nil when
// there is none. Nothing comes from the roster: these are Console
// organizations quota-cache reads with admin keys from its own configuration,
// so the snapshot entry is what says the account exists.
func apiCreditsOf(in Input, now time.Time) *APICredits {
	accounts := []*creditAccount{}
	for _, entry := range in.Snapshot.Entries {
		if entry.Provider == qc.ProviderAnthropicAPI {
			accounts = append(accounts, creditAccountOf(entry, now))
		}
	}
	if len(accounts) == 0 {
		return nil
	}
	// Configured order, then label, then id, so a document built twice is
	// byte-identical. An entry with no configuration at all has no position
	// and goes last.
	sort.Slice(accounts, func(i, j int) bool {
		a, b := accounts[i], accounts[j]
		if (a.credit == nil) != (b.credit == nil) {
			return b.credit == nil
		}
		if a.out.Order != b.out.Order {
			return a.out.Order < b.out.Order
		}
		if a.out.Label != b.out.Label {
			return a.out.Label < b.out.Label
		}
		return a.out.ID < b.out.ID
	})
	markDuplicates(accounts)
	credits := &APICredits{
		Title:    "Claude API credits",
		Currency: "USD",
		Accounts: make([]APICreditAccount, 0, len(accounts)),
	}
	for _, a := range accounts {
		a.judge(in, now)
		credits.Accounts = append(credits.Accounts, a.out)
	}
	credits.Pool = creditPoolOf(accounts, now)
	return credits
}

// creditAccountOf reads what the entry says without judging it: the configured
// facts, the cycle they give at now, and the reading if it is a current one.
func creditAccountOf(entry qc.Entry, now time.Time) *creditAccount {
	a := &creditAccount{entry: entry, credit: entry.APICredit}
	a.out = APICreditAccount{
		ID:               entry.AuthIndex,
		Order:            -1,
		DailySpend:       []APICreditDay{},
		DataIssues:       []string{},
		NextAttemptEpoch: epochPointerOf(entry.NextAttempt),
	}
	var stored *qc.CostReport
	if entry.Quota != nil && entry.Quota.CostReport != nil {
		stored = entry.Quota.CostReport
		// The reading's own instant. The entry's top-level observed_at is
		// always zero on this provider, which has no weekly window.
		a.out.ObservedAtEpoch = epochPointerOf(entry.Quota.ObservedAt)
	}
	c := entry.APICredit
	if c == nil {
		return a
	}
	a.out.Label, a.out.Order = c.Label, c.Position
	if cycle, err := qc.CreditCycleAt(c.Renews, now); err == nil {
		a.cycle, a.hasCycle = cycle, true
		a.out.CycleStartEpoch = epochPointerOf(cycle.Start)
		a.out.RenewsAtEpoch = epochPointerOf(cycle.End)
		in := cycle.End.Unix() - now.Unix()
		a.out.RenewsInSeconds = &in
	}
	// The rule quota-cache validated with, then exactly: amountOf alone would
	// take a negative amount or an exponent, neither of which is a credit.
	if qc.ValidMonthlyUSD(c.MonthlyUSD) {
		if monthly, ok := amountOf(c.MonthlyUSD); ok {
			a.monthly = monthly
			a.out.MonthlyCredit = floatOf(monthly)
			a.out.MonthlyCreditText = moneyOf(monthly)
		}
	}
	if stored != nil && stored.KeyFingerprint == c.KeyFingerprint {
		a.report = stored
		a.out.OrganizationID = stored.OrganizationID
	}
	if c.Problem == "" {
		a.read(stored)
	}
	return a
}

// read sums the current cycle from the stored report, or says why it cannot.
//
// Only days the report covered are summed: a day Anthropic has not reported is
// not a day nothing was spent, so it is never filled in as $0. The one gap
// quota-cache lets through is today's bucket, still in progress, and that
// reading is counted with todayMissing set.
func (a *creditAccount) read(stored *qc.CostReport) {
	switch {
	case stored == nil:
		return
	case a.report == nil:
		a.reason = issueKeyChanged
		return
	case !a.hasCycle || a.monthly == nil:
		// Not reachable for an item quota-cache found no problem with.
		return
	}
	r, observed := a.report, a.entry.Quota.ObservedAt
	covered := r.CoveredUntil()
	switch {
	case observed.Before(a.cycle.Start):
		a.reason = issuePreviousCycle
		return
	// quota-cache never stores either; this guards a snapshot it did not write.
	case r.StartingAt.After(a.cycle.Start) || covered.Before(dayOf(observed)):
		a.reason = issueIncompleteCycle
		return
	}
	end := a.cycle.End
	if covered.Before(end) {
		end = covered
	}
	reading := &creditReading{spent: new(big.Rat), days: []APICreditDay{}}
	// The same walk CoveredUntil makes, so a repeated day past a gap can never
	// be summed twice.
	expect := r.StartingAt
	for _, day := range r.Days {
		if !day.StartingAt.Equal(expect) {
			break
		}
		expect = expect.Add(24 * time.Hour)
		if day.StartingAt.Before(a.cycle.Start) || !day.StartingAt.Before(end) {
			continue
		}
		dollars := new(big.Rat)
		for _, amount := range day.Amounts {
			if amount.Currency != "USD" {
				a.reason = issueUnsupportedCurrency
				return
			}
			cents, ok := amountOf(amount.Amount)
			if !ok {
				a.reason = issueAmountInvalid
				return
			}
			dollars.Add(dollars, cents)
		}
		dollars.Quo(dollars, centsPerDollar)
		reading.spent.Add(reading.spent, dollars)
		reading.days = append(reading.days, APICreditDay{
			DayStartEpoch: day.StartingAt.Unix(),
			Spent:         floatOf(dollars),
			SpentText:     moneyOf(dollars),
		})
	}
	// What the credit paid for is the spend clamped to [0, credit]; the rest is
	// overage, paid from purchased credit, and never another account's credit.
	reading.used = new(big.Rat).Set(reading.spent)
	if reading.used.Sign() < 0 {
		reading.used.SetInt64(0)
	}
	if reading.used.Cmp(a.monthly) > 0 {
		reading.used.Set(a.monthly)
	}
	reading.left = new(big.Rat).Sub(a.monthly, reading.used)
	reading.overage = new(big.Rat).Sub(reading.spent, a.monthly)
	if reading.overage.Sign() < 0 {
		reading.overage.SetInt64(0)
	}
	reading.todayMissing = !covered.After(observed)
	a.reading = reading
}

// markDuplicates finds items that read the same Console organization. Only a
// reading made with the item's current key identifies it, and a previous
// cycle's still does. The first by position is judged as usual; every later
// one is a duplicate, listed and never counted, so one organization's spend
// cannot enter the pool twice.
func markDuplicates(accounts []*creditAccount) {
	first := map[string]*creditAccount{}
	for _, a := range accounts {
		if a.credit == nil || a.credit.Problem != "" || a.report == nil || a.report.OrganizationID == "" {
			continue
		}
		if earlier, seen := first[a.report.OrganizationID]; seen {
			a.duplicateOf = earlier
			continue
		}
		first[a.report.OrganizationID] = a
	}
}

// judge settles the account's state, its issues, and the figures it shows. The
// first rule that matches wins; see docs/summary-contract.md.
func (a *creditAccount) judge(in Input, now time.Time) {
	out, entry := &a.out, a.entry
	switch {
	case a.credit == nil || a.credit.Problem != "":
		out.State = StateMisconfigured
		out.DataIssues = append(out.DataIssues, issueMisconfigured)
		out.Issue = problemIssueOf(a.credit)
		// quota-cache never polls an item with a problem, so a next attempt
		// left from before it was misconfigured promises a poll that will not
		// happen.
		out.NextAttemptEpoch = nil
	case a.duplicateOf != nil:
		a.reading = nil
		out.State = StateDuplicate
		out.DataIssues = append(out.DataIssues, issueDuplicateOrganization)
		out.Issue = "Same Console organization as " + a.duplicateOf.out.Label + "; counted there."
	// Failures, not LastError, marks a failed poll, as for every entry. The
	// last good reading stays counted under it.
	case entry.Failures > 0:
		out.State = StatusError
		out.DataIssues = append(out.DataIssues, issueObserveError)
		issue, sentence := failureIssueOf(entry.LastError)
		if issue != "" {
			out.DataIssues = append(out.DataIssues, issue)
		}
		out.Issue = sentence
	// A report in another currency, or with an amount that is not a number,
	// will read the same way at the next poll: an error, not a wait.
	case a.reason == issueUnsupportedCurrency || a.reason == issueAmountInvalid:
		out.State = StatusError
		out.DataIssues = append(out.DataIssues, a.reason)
		out.Issue = "Anthropic's report could not be read as US dollars."
	case a.reading == nil:
		out.State = StatusPending
		if a.reason != "" {
			out.DataIssues = append(out.DataIssues, a.reason)
		}
		out.Issue = pendingIssueOf(a.reason)
	case in.StaleAfter > 0 && now.Sub(entry.Quota.ObservedAt) > in.StaleAfter:
		out.State = StateStale
		out.DataIssues = append(out.DataIssues, issueStale)
		out.Issue = "This reading is out of date; Quota Cache has not refreshed it."
	default:
		out.State = StatusOK
	}
	if entry.Failures == 0 && entry.LastError == lastErrorRefreshPending {
		out.DataIssues = append(out.DataIssues, issueRefreshPending)
	}
	r := a.reading
	if r == nil {
		return
	}
	out.HasReading = true
	out.Spent, out.SpentText = floatOf(r.spent), moneyOf(r.spent)
	out.CreditUsed, out.CreditUsedText = floatOf(r.used), moneyOf(r.used)
	out.Left, out.LeftText = floatOf(r.left), moneyOf(r.left)
	out.Overage, out.OverageText = floatOf(r.overage), moneyOf(r.overage)
	out.RemainingFraction = fractionOf(r.left, a.monthly)
	out.RemainingPercent = percentOf(out.RemainingFraction)
	out.Level = levelOf(out.RemainingFraction)
	out.DailySpend = r.days
	if r.todayMissing {
		out.DataIssues = append(out.DataIssues, issueTodayNotReported)
		if out.State == StatusOK {
			out.Issue = "Anthropic has not reported today's spend yet; spent covers the cycle through yesterday."
		}
	}
}

// creditPoolOf sums the counted accounts: those with a current reading, which
// after judging is every account with one. A misconfigured, pending or
// duplicate account has none.
func creditPoolOf(accounts []*creditAccount, now time.Time) APICreditPool {
	pool := APICreditPool{AccountCount: len(accounts)}
	monthly, spent, used, left, overage := new(big.Rat), new(big.Rat), new(big.Rat), new(big.Rat), new(big.Rat)
	counted := []*creditAccount{}
	for _, a := range accounts {
		switch {
		case a.reading != nil:
			counted = append(counted, a)
			monthly.Add(monthly, a.monthly)
			spent.Add(spent, a.reading.spent)
			used.Add(used, a.reading.used)
			left.Add(left, a.reading.left)
			overage.Add(overage, a.reading.overage)
		case a.out.State == StateDuplicate:
			pool.DuplicateCount++
		default:
			pool.MissingCount++
		}
	}
	pool.CountedCount = len(counted)
	if len(counted) == 0 {
		return pool
	}
	pool.HasReading = true
	pool.MonthlyCredit, pool.MonthlyCreditText = floatOf(monthly), moneyOf(monthly)
	pool.Spent, pool.SpentText = floatOf(spent), moneyOf(spent)
	pool.CreditUsed, pool.CreditUsedText = floatOf(used), moneyOf(used)
	pool.Left, pool.LeftText = floatOf(left), moneyOf(left)
	pool.Overage, pool.OverageText = floatOf(overage), moneyOf(overage)
	pool.RemainingFraction = fractionOf(left, monthly)
	pool.RemainingPercent = percentOf(pool.RemainingFraction)
	pool.Level = levelOf(pool.RemainingFraction)

	// Only a renewal that restores something is a refill: an account that has
	// used none of its credit renews to where it already is.
	var soonest, latest time.Time
	for _, a := range counted {
		if a.reading.used.Sign() <= 0 {
			continue
		}
		if soonest.IsZero() || a.cycle.End.Before(soonest) {
			soonest = a.cycle.End
		}
		if a.cycle.End.After(latest) {
			latest = a.cycle.End
		}
	}
	if soonest.IsZero() {
		return pool
	}
	refill := &APICreditRefill{
		AccountIDs:      []string{},
		RefillAtEpoch:   soonest.Unix(),
		RefillInSeconds: soonest.Unix() - now.Unix(),
	}
	gain := new(big.Rat)
	for _, a := range counted {
		if a.reading.used.Sign() > 0 && a.cycle.End.Equal(soonest) {
			refill.AccountIDs = append(refill.AccountIDs, a.out.ID)
			gain.Add(gain, a.reading.used)
		}
	}
	refill.Gain, refill.GainText = floatOf(gain), moneyOf(gain)
	refill.GainFraction = fractionOf(gain, monthly)
	refill.GainPercent = percentOf(refill.GainFraction)
	pool.NextRefill = refill
	pool.FullAtEpoch = epochPointerOf(latest)
	in := latest.Unix() - now.Unix()
	pool.FullInSeconds = &in
	return pool
}

// failureIssueOf names the failure quota-cache recorded, and the sentence the
// account shows for it. The messages are quota-cache's own static strings,
// never anything Anthropic sent.
func failureIssueOf(lastError string) (issue, sentence string) {
	switch lastError {
	case qc.CreditErrorKeyRejected:
		return issueKeyRejected, "Anthropic rejected this admin key. It may be revoked or expired; create a new one in Console under Settings > Admin keys."
	case qc.CreditErrorForbidden:
		return issueKeyForbidden, "This key cannot read the cost report. Use an Admin API key from an organization admin."
	case qc.CreditErrorUnavailable:
		return issueCostReportUnavailable, "Anthropic has no cost report for this organization."
	case lastErrorRateLimited:
		return issueRateLimited, "Anthropic is rate limiting these reads; Quota Cache will retry."
	}
	return "", "The last read failed; Quota Cache will retry."
}

// pendingIssueOf says what an account without a reading is waiting for.
func pendingIssueOf(reason string) string {
	switch reason {
	case issuePreviousCycle:
		return "Renewed since the last reading; waiting for this cycle's first reading."
	case issueKeyChanged:
		return "Waiting for the first reading with the new admin key."
	case issueIncompleteCycle:
		return "Waiting for a reading that covers this whole cycle."
	}
	return "Waiting for the first reading."
}

// problemIssueOf turns quota-cache's configuration problem into the sentence
// that tells the operator what to change. An unknown problem still reads as a
// problem, so a newer quota-cache's codes degrade to something true.
func problemIssueOf(c *qc.APICredit) string {
	if c == nil {
		return "This item's configuration has a problem."
	}
	switch c.Problem {
	case qc.CreditProblemItemInvalid:
		return "This item in claude-api-credits is not a list of label, admin-key, monthly-usd and renews."
	case qc.CreditProblemUnknownField:
		return "This item has a setting Quota Cache does not know; check the spelling of label, admin-key, monthly-usd and renews."
	case qc.CreditProblemTooManyItems:
		return "Only the first 16 items in claude-api-credits are read."
	case qc.CreditProblemLabelMissing:
		return "Give this item a label, unique and at most 64 characters."
	case qc.CreditProblemLabelInvalid:
		return "This label is over 64 characters or has a character that cannot be shown, such as a control character, a no-break space or an emoji joiner."
	case qc.CreditProblemLabelDuplicate:
		return "Another item already uses this label."
	case qc.CreditProblemAdminKeyMissing:
		return "Add this organization's admin key."
	case qc.CreditProblemAdminKeyInvalid:
		return "This does not look like an Anthropic key; it should start with sk-ant-."
	case qc.CreditProblemAdminKeyRepeated:
		return "Another item already uses this admin key."
	case qc.CreditProblemMonthlyMissing:
		return "Set monthly-usd to this organization's monthly credit in dollars."
	case qc.CreditProblemMonthlyInvalid:
		return `monthly-usd must be dollars, like "200" or "260.50".`
	case qc.CreditProblemRenewsMissing:
		return "Set renews to the date your Claude plan next renews."
	case qc.CreditProblemRenewsInvalid:
		return `renews must be a date like "2026-10-29".`
	}
	return "This item's configuration has a problem."
}

// fractionOf is part over whole as the 0-1 figure a bar is sized from, and 0
// when whole is 0: a credit of nothing has nothing left to show. Converted to
// a float once, here, from the exact ratio.
func fractionOf(part, whole *big.Rat) float64 {
	if whole.Sign() == 0 {
		return 0
	}
	return floatOf(new(big.Rat).Quo(part, whole))
}

// floatOf is the one conversion from an exact amount to the JSON number beside
// its text. Nothing is computed from the result.
func floatOf(amount *big.Rat) float64 {
	value, _ := amount.Float64()
	return value
}

// dayOf is 00:00 UTC of the day containing t: the cost report's day.
func dayOf(t time.Time) time.Time {
	year, month, day := t.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}
