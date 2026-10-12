package credits

import (
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/settings"
)

// readingWithoutCycle is how old a reading may be when there is no refill
// date to say whether the credit has refilled since: a cycle is at most 31
// days.
const readingWithoutCycle = 31 * 24 * time.Hour

// nearlySpent is the share of the credit at or below which a refusal from a
// Claude Code-based client is taken to mean the credit is spent: such a
// client is Claude Code, which the credit does not cover, or the Agent SDK,
// which it does, and only an estimate already this low settles which.
var nearlySpent = big.NewRat(1, 10)

// centOf is $0.01, below which a cache-write difference is not worth a line.
var centOf = big.NewRat(1, 100)

// creditCycle is the cycle the refill date in force gives, when one does.
type creditCycle struct {
	client.CreditCycle
	OK bool
}

// creditAccount is one account while the pool is worked out: the document's
// view of it, and the exact amounts its float fields are converted from.
type creditAccount struct {
	out    client.CreditAccount
	entry  client.Entry
	credit *client.APICredit

	grant    *big.Rat
	cycle    creditCycle
	reading  *settings.Reading
	unused   string
	left     *big.Rat
	used     *big.Rat
	overage  *big.Rat
	issues   map[string]bool
	counted  bool
	estimate *big.Rat
}

// apiCreditsOf pools every item quota-cache lists, and is nil when there is
// none. Nothing comes from the roster: these are Console organizations from
// quota-cache's own list, so the item is what says the account exists. Each is
// taken as the anthropic-api entry the cache writes for it.
func apiCreditsOf(in Input, now time.Time) *client.CreditDocument {
	accounts := []*creditAccount{}
	for _, item := range in.Items {
		entry := client.Entry{Provider: client.ProviderAnthropicAPI, AuthIndex: item.ID, APICredit: item.Credit}
		accounts = append(accounts, &creditAccount{entry: entry, credit: entry.APICredit, issues: map[string]bool{}})
	}
	if len(accounts) == 0 {
		return nil
	}
	// The list's order, then label, then id, so a document built twice is
	// byte-identical. An entry with no api_credit at all has no position and
	// goes last.
	for _, a := range accounts {
		a.out = client.CreditAccount{ID: a.entry.AuthIndex, Order: -1}
		if a.credit != nil {
			a.out.Label, a.out.Order = a.credit.Label, a.credit.Position
		}
	}
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
	editing := editingOf(in)
	credits := &client.CreditDocument{
		Title:    "Monthly API Credit",
		Currency: "USD",
		Pricing:  client.CreditPricing{AsOf: pricingAsOf, Source: pricingSource, CacheWrites: pricingCacheWrites},
		Meter:    creditMeterOf(in, now),
		Editing:  editing,
		Accounts: make([]client.CreditAccount, 0, len(accounts)),
		Unlinked: unlinkedOf(in.Meter, accounts),
		Orphans:  orphansOf(in.Settings, accounts),
	}
	for _, a := range accounts {
		a.judge(in, editing, now)
		credits.Accounts = append(credits.Accounts, a.out)
	}
	credits.Pool = creditPoolOf(accounts, now)
	return credits
}

// editingOf is whether the page may offer to change anything. A settings file
// that cannot be read applies nothing.
func editingOf(in Input) client.CreditEditing {
	if settingsStateOf(in) == client.CreditSettingsUnreadable {
		return client.CreditEditing{Reason: client.CreditEditingSettingsUnreadable}
	}
	return client.CreditEditing{Available: true}
}

// judge settles the account's state, its issues, and the figures it shows.
// The first state that matches wins; the rules are numbered as in the
// contract.
func (a *creditAccount) judge(in Input, editing client.CreditEditing, now time.Time) {
	out, c, m := &a.out, a.credit, in.Meter
	stored, hasStored := in.Settings.APICredits[a.entry.AuthIndex]
	out.DataIssues, out.Unpriced = []string{}, []client.CreditUnpriced{}
	if c != nil {
		out.OrganizationID = c.OrganizationID
	}

	// 1. The grant, as entered on the page.
	out.MonthlyCreditSource = client.CreditSourceNone
	if stored.MonthlyUSD != "" {
		a.grant, out.MonthlyCreditSource = exactDollars(stored.MonthlyUSD), client.CreditSourceDashboard
	}
	// 2. The cycle, from the refill date entered on the page.
	out.RenewsSource = client.CreditSourceNone
	if stored.Renews != "" {
		out.RenewsSource = client.CreditSourceDashboard
	}
	if cycle, err := client.CreditCycleAt(stored.Renews, now); err == nil {
		a.cycle = creditCycle{CreditCycle: cycle, OK: true}
		out.CycleStartEpoch = epochPointerOf(cycle.Start)
		out.RenewsAtEpoch = epochPointerOf(cycle.End)
		in := cycle.End.Unix() - now.Unix()
		out.RenewsInSeconds = &in
	}
	// 3. The Console reading, used only while it can still describe this
	// cycle of this organization.
	if r := stored.Reading; r != nil {
		a.unused = a.readingUnusedReason(r, now)
		if a.unused == "" {
			a.reading = r
		}
	}
	out.Settings = a.settingsOf(stored, hasStored, editing)

	org, inMeter := client.MeterOrganization{}, false
	if m != nil && c != nil && c.OrganizationID != "" {
		org, inMeter = m.Organizations[c.OrganizationID]
	}
	// The first two cases are the contract's states for an item quota-cache
	// 0.1.x listed from its configuration. The page's organizations meet
	// neither.
	switch {
	case c == nil || c.Problem != "":
		out.State = client.CreditStateMisconfigured
		a.flag(true, client.CreditIssueMisconfigured)
		a.finish(c)
		return
	case c.OrganizationID == "":
		out.State = client.CreditStateCacheTooOld
		a.flag(true, client.CreditIssueCacheTooOld)
		a.finish(c)
		return
	case m == nil || !inMeter || org.UnlinkedAt != nil:
		// The meter has not saved since this organization was linked: there
		// is nothing to count yet, only settings to check.
		out.State = client.CreditStatePending
		a.flag(m == nil, client.CreditIssueMeterMissing)
		a.flag(m != nil, client.CreditIssueOrgNotCounted)
		a.flag(a.grant == nil, client.CreditIssueNeedsCredit)
		a.flag(!a.cycle.OK && a.reading == nil, client.CreditIssueNeedsRefillDate)
		a.flag(a.unused != "", client.CreditIssueReadingUnused)
		a.finish(c)
		return
	}
	out.MeterSinceEpoch = epochPointerOf(org.Since)
	out.LastSeenEpoch = timePointerEpoch(org.LastSeenAt)
	out.Refusals = client.CreditRefusals{
		Total: org.Refusals, LastAtEpoch: timePointerEpoch(org.LastRefusalAt),
		ClaudeCodeTotal: org.ClaudeCodeRefusals, ClaudeCodeLastAtEpoch: timePointerEpoch(org.LastClaudeCodeRefusalAt),
	}

	// 4. Spend, from the meter, priced. The cycle's is exact to the meter's
	// day buckets, since both start at 00:00 UTC; the reading's is its
	// day's usage less the baseline taken when it was saved.
	var cycleCosts, readingCosts costs
	if a.cycle.OK {
		cycleCosts = priceUsage(org.UsageFromDay(a.cycle.Start))
	}
	if a.reading != nil {
		readingCosts = priceUsage(org.UsageFromDay(a.reading.Baseline.DayStart)).minus(priceUsage(a.reading.Baseline.Usage))
	}
	var anchor time.Time
	var window *costs
	switch {
	case a.cycle.OK:
		anchor, window = a.cycle.Start, &cycleCosts
		a.setSpent(cycleCosts.at5m, a.cycle.Start)
	case a.reading != nil:
		anchor, window = a.reading.At, &readingCosts
		a.setSpent(readingCosts.at5m, a.reading.At)
	}

	// 5. The basis, and what is left on it.
	switch {
	case a.reading != nil:
		remaining := exactDollars(a.reading.RemainingUSD)
		out.Basis, anchor, window = client.CreditBasisReading, a.reading.At, &readingCosts
		a.left = nonNegative(new(big.Rat).Sub(remaining, readingCosts.at5m))
		a.overage = nonNegative(new(big.Rat).Sub(readingCosts.at5m, remaining))
		out.Reading = &client.CreditReading{
			Remaining: floatOf(remaining), RemainingText: moneyOf(remaining),
			AtEpoch: a.reading.At.Unix(), EnteredAtEpoch: a.reading.EnteredAt.Unix(),
			SpentSince: floatOf(readingCosts.at5m), SpentSinceText: moneyOf(readingCosts.at5m),
		}
		a.flag(a.grant != nil && remaining.Cmp(a.grant) > 0, client.CreditIssueReadingAboveCredit)
		a.flag(a.cycle.OK && a.reading.At.Before(a.cycle.Start.Add(24*time.Hour)), client.CreditIssueReadingOnRefillDay)
	case a.grant != nil && a.cycle.OK:
		out.Basis = client.CreditBasisCredit
		a.left = nonNegative(new(big.Rat).Sub(a.grant, cycleCosts.at5m))
		a.overage = nonNegative(new(big.Rat).Sub(cycleCosts.at5m, a.grant))
	}
	if a.left != nil {
		a.estimate = new(big.Rat).Set(a.left)
		a.flag(a.overage.Sign() > 0, client.CreditIssueOverCredit)
	}

	// 6. Refusals since the anchor, not followed by a success.
	served := func(t time.Time) bool { return org.LastSuccessAt != nil && org.LastSuccessAt.After(t) }
	out6 := false
	if !anchor.IsZero() {
		if t := org.LastRefusalAt; t != nil && t.After(anchor) && !served(*t) {
			out6 = true
			a.flag(true, client.CreditIssueRefused)
		}
		if t := org.LastClaudeCodeRefusalAt; t != nil && t.After(anchor) && !served(*t) {
			if a.nearlySpent() {
				out6 = true
				a.flag(true, client.CreditIssueRefusedNearlySpent)
			} else {
				a.flag(true, client.CreditIssueClaudeCodeRefused)
			}
		}
	}
	if out6 && a.left != nil {
		a.left = new(big.Rat)
		out.EstimateLeftText = moneyOf(a.estimate)
	}
	if a.left != nil && a.grant != nil {
		a.used = clampRat(new(big.Rat).Sub(a.grant, a.left), a.grant)
	}

	// 9. A meter that has not saved within stale-after has stopped.
	stale := in.StaleAfter > 0 && now.Sub(m.FlushedAt) > in.StaleAfter
	a.flag(stale, client.CreditIssueStale)

	// 7. What could be missing since the anchor makes every figure a bound.
	if !anchor.IsZero() {
		after := func(t *time.Time) bool { return t != nil && t.After(anchor) }
		a.flag(org.Since.After(anchor), client.CreditIssueMeterStartedLate)
		a.flag(meterGapAfter(m, anchor, now), client.CreditIssueMeterGap)
		a.flag(after(m.LastDroppedAt), client.CreditIssueMeterDropped)
		a.flag(after(org.LastOverflowAt), client.CreditIssueMeterFull)
		a.flag(after(m.LastUnattributedAt), client.CreditIssueMeterUnattributed)
		a.flag(after(m.LastRejectedAt), client.CreditIssueMeterRejected)
		out.Unpriced = window.unpricedList()
		a.flag(len(out.Unpriced) > 0, client.CreditIssueUnpricedModel)
		for _, issue := range []string{client.CreditIssueMeterStartedLate, client.CreditIssueMeterGap, client.CreditIssueMeterDropped, client.CreditIssueMeterFull,
			client.CreditIssueMeterUnattributed, client.CreditIssueMeterRejected, client.CreditIssueUnpricedModel} {
			out.LowerBound = out.LowerBound || a.issues[issue]
		}
		out.LowerBound = out.LowerBound || stale
		extra := nonNegative(new(big.Rat).Sub(window.at1h, window.at5m))
		out.CacheWriteExtra = floatOf(extra)
		if extra.Cmp(centOf) >= 0 {
			out.CacheWriteExtraText = moneyOf(extra)
		}
	}

	// 8. Nothing seen at all is not a bound by itself: with every request
	// routed through CPA, nothing seen is nothing spent.
	a.flag(org.LastSeenAt == nil, client.CreditIssueNoTraffic)
	a.flag(a.grant == nil, client.CreditIssueNeedsCredit)
	a.flag(!a.cycle.OK && a.reading == nil, client.CreditIssueNeedsRefillDate)
	a.flag(a.unused != "", client.CreditIssueReadingUnused)

	// 10. The fraction and level, on the 5-minute figure.
	if a.grant != nil {
		out.MonthlyCredit, out.MonthlyCreditText = floatOf(a.grant), moneyOf(a.grant)
		a.flag(a.grant.Sign() == 0, client.CreditIssueZeroCredit)
	}
	if a.left != nil {
		out.HasEstimate = true
		out.Left, out.LeftText = floatOf(a.left), moneyOf(a.left)
		out.Overage, out.OverageText = floatOf(a.overage), moneyOf(a.overage)
		if a.used != nil {
			out.Used, out.UsedText = floatOf(a.used), moneyOf(a.used)
		}
		if a.grant != nil && a.grant.Sign() > 0 {
			out.RemainingFraction = clampFraction(fractionOf(a.left, a.grant))
			out.RemainingPercent = percentOf(out.RemainingFraction)
			out.Level = levelOf(out.RemainingFraction)
		}
		if out6 {
			out.RemainingFraction, out.RemainingPercent, out.Level = 0, 0, client.LevelCritical
		}
	}

	switch {
	case a.grant == nil || out.Basis == "":
		out.State = client.CreditStateNeedsSettings
	case out6:
		out.State = client.CreditStateOut
	case stale:
		out.State = client.CreditStateStale
	default:
		out.State = client.CreditStateOK
	}
	a.counted = out.State == client.CreditStateOK || out.State == client.CreditStateStale || out.State == client.CreditStateOut
	out.Counted = a.counted
	a.finish(c)
}

// setSpent records the metered spend since an instant.
func (a *creditAccount) setSpent(spent *big.Rat, since time.Time) {
	a.out.Spent, a.out.SpentText = floatOf(spent), moneyOf(spent)
	a.out.SpentSinceEpoch = epochPointerOf(since)
}

// nearlySpent is whether the estimate, before any refusal, had at most a
// tenth of the credit left. Without a credit to take a share of, only an
// estimate of nothing left is.
func (a *creditAccount) nearlySpent() bool {
	switch {
	case a.estimate == nil:
		return false
	case a.grant == nil:
		return a.estimate.Sign() == 0
	case a.grant.Sign() == 0:
		return true
	}
	return new(big.Rat).Quo(a.estimate, a.grant).Cmp(nearlySpent) <= 0
}

// readingUnusedReason is why a stored reading cannot describe this cycle of
// this organization, or "" when it can.
func (a *creditAccount) readingUnusedReason(r *settings.Reading, now time.Time) string {
	org := ""
	if a.credit != nil {
		org = a.credit.OrganizationID
	}
	switch {
	case r.OrganizationID != org:
		return client.CreditReadingOtherOrganization
	case r.At.After(now):
		return client.CreditReadingFuture
	case a.cycle.OK && r.At.Before(a.cycle.Start):
		return client.CreditReadingBeforeRefill
	case !a.cycle.OK && r.At.Before(now.Add(-readingWithoutCycle)):
		return client.CreditReadingTooOld
	}
	return ""
}

// settingsOf is the editor's view of the account: what is stored, and whether
// the page may change it.
func (a *creditAccount) settingsOf(stored settings.APICredit, hasStored bool, editing client.CreditEditing) client.CreditSettings {
	c := a.credit
	s := client.CreditSettings{
		Revision:            settings.RevisionText(stored.Rev),
		MonthlyUSD:          stored.MonthlyUSD,
		Renews:              stored.Renews,
		ReadingUnusedReason: a.unused,
	}
	if hasStored {
		s.UpdatedAtEpoch = epochPointerOf(stored.UpdatedAt)
	}
	if r := stored.Reading; r != nil {
		s.Reading = &client.StoredReading{RemainingUSD: r.RemainingUSD, AtEpoch: r.At.Unix(), EnteredAtEpoch: r.EnteredAt.Unix()}
	}
	id := a.entry.AuthIndex
	switch {
	case strings.HasPrefix(id, "item-"):
		switch {
		case c != nil && c.Problem == client.CreditProblemTooManyItems:
			s.NotEditableReason = client.CreditNotEditableOverLimit
		case c != nil && c.Problem == client.CreditProblemOrganizationIDDuplicate:
			s.NotEditableReason = client.CreditNotEditableDuplicateOrganization
		default:
			s.NotEditableReason = client.CreditNotEditableNoOrganization
		}
	// Only an organization's own id keys stored values: a 0.1.13 entry's
	// "label-" id names a label, which is exactly what must not carry an
	// override over to another organization.
	case !strings.HasPrefix(id, "org-") || c == nil || (c.Problem == "" && c.OrganizationID == ""):
		s.NotEditableReason = client.CreditNotEditableCacheTooOld
	case !editing.Available:
		s.NotEditableReason = editing.Reason
	default:
		s.Editable = true
	}
	return s
}

// finish lists the issues in their fixed order and writes the sentence for
// the first.
func (a *creditAccount) finish(c *client.APICredit) {
	out := &a.out
	for _, issue := range client.CreditIssueOrder {
		if a.issues[issue] {
			out.DataIssues = append(out.DataIssues, issue)
		}
	}
	if len(out.DataIssues) > 0 {
		out.Issue = a.sentenceOf(out.DataIssues[0], c)
	}
}

func (a *creditAccount) flag(set bool, issue string) {
	if set {
		a.issues[issue] = true
	}
}

// sentenceOf is the one sentence the account shows for its first issue.
func (a *creditAccount) sentenceOf(issue string, c *client.APICredit) string {
	out := &a.out
	switch issue {
	case client.CreditIssueMisconfigured:
		return problemIssueOf(c)
	case client.CreditIssueCacheTooOld:
		return "Quota Cache 0.1.13 reads Anthropic's cost report, which Quota Glance no longer uses. Update Quota Cache to 0.1.14 and add this organization's organization-id."
	case client.CreditIssueMeterMissing:
		return "Quota Cache has not saved its API meter yet; counting starts when it next saves."
	case client.CreditIssueOrgNotCounted:
		return "Counting starts at Quota Cache's next save."
	case client.CreditIssueNeedsCredit:
		if a.issues[client.CreditIssueNeedsRefillDate] {
			return "Set this organization's monthly credit and refill date."
		}
		return "Set this organization's monthly credit to count it in the total."
	case client.CreditIssueNeedsRefillDate:
		return "Set the refill date, or enter a Console reading."
	case client.CreditIssueRefused:
		sentence := "Anthropic refused a request for low credit, so this credit is spent."
		if a.estimate != nil && a.estimate.Sign() > 0 {
			sentence += " The estimate had " + moneyOf(a.estimate) + " left; enter a Console reading to correct it."
		}
		return sentence
	case client.CreditIssueRefusedNearlySpent:
		return "Anthropic refused requests from a Claude Code-based client for low credit, and the estimate is nearly spent, so this credit is shown as spent. Enter a Console reading to check."
	case client.CreditIssueStale:
		return client.CreditSentenceStale
	case client.CreditIssueMeterStartedLate:
		switch out.Basis {
		case client.CreditBasisCredit:
			return "Counting began after this cycle started, so earlier spend is missing. Enter a Console reading to correct it."
		case client.CreditBasisReading:
			return "Counting began after your Console reading, so spend in between is missing."
		}
		return "Counting began recently, so earlier spend is missing."
	case client.CreditIssueMeterGap:
		return client.CreditSentenceMeterGap
	case client.CreditIssueMeterDropped:
		return "Quota Cache dropped usage records it could not keep up with, so some spend is missing."
	case client.CreditIssueMeterFull:
		return "Quota Cache's meter was full, so some spend is missing."
	case client.CreditIssueMeterUnattributed:
		return "Some failed requests could not be matched to an organization, so some spend may be missing."
	case client.CreditIssueMeterRejected:
		return "Quota Cache could not read some usage records from CPA, so some spend may be missing."
	case client.CreditIssueUnpricedModel:
		return unpricedSentenceOf(out.Unpriced)
	case client.CreditIssueZeroCredit:
		return "This organization's monthly credit is set to $0.00."
	case client.CreditIssueOverCredit:
		return "Spend is " + out.OverageText + " past the monthly credit. Anthropic bills purchased credit after the monthly credit; if there is none, enter a Console reading."
	case client.CreditIssueReadingAboveCredit:
		return "Your Console reading is more than the monthly credit; check the monthly credit."
	case client.CreditIssueReadingOnRefillDay:
		return "This Console reading was taken on the refill day. If Console did not show the new credit yet, enter a new reading once it does."
	case client.CreditIssueReadingUnused:
		return readingUnusedSentenceOf(a.unused, out.Settings.Reading)
	case client.CreditIssueClaudeCodeRefused:
		return "Anthropic refused requests from a Claude Code-based client (Claude Code or the Agent SDK) for low credit. If they were Agent SDK requests, this credit may be spent; enter a Console reading."
	case client.CreditIssueNoTraffic:
		return "No API traffic for this organization has reached CPA since counting began."
	}
	return ""
}

// readingUnusedSentenceOf says why the stored reading is not used.
func readingUnusedSentenceOf(reason string, reading *client.StoredReading) string {
	switch reason {
	case client.CreditReadingBeforeRefill:
		amount, at := "", time.Time{}
		if reading != nil {
			amount = moneyOf(exactDollars(reading.RemainingUSD))
			at = time.Unix(reading.AtEpoch, 0)
		}
		return "Your Console reading of " + amount + " on " + formatUTCDate(at) + " was before the last refill, so it is not used."
	case client.CreditReadingOtherOrganization:
		return "Your Console reading was for a different organization-id, so it is not used."
	case client.CreditReadingTooOld:
		return "Your Console reading is over 31 days old and there is no refill date, so it is not used."
	case client.CreditReadingFuture:
		return "Your Console reading is dated in the future, so it is not used."
	}
	return ""
}

// unpricedSentenceOf names at most three unpriced models, most tokens first.
func unpricedSentenceOf(unpriced []client.CreditUnpriced) string {
	named := make([]string, 0, 3)
	for i, u := range unpriced {
		if i == 3 {
			break
		}
		named = append(named, u.Model+" ("+tokensText(u.Tokens)+" tokens)")
	}
	sentence := "Some requests used a model with no listed price and are left out: " + strings.Join(named, ", ")
	if more := len(unpriced) - len(named); more > 0 {
		sentence += " and " + strconv.Itoa(more) + " more"
	}
	return sentence + "."
}

// tokensText prints a token count the way the page does: 900, 250K, 1.0M.
func tokensText(n uint64) string {
	switch {
	case n >= 999_950:
		tenths := n/100_000 + boolToUint(n%100_000 >= 50_000)
		return strconv.FormatUint(tenths/10, 10) + "." + strconv.FormatUint(tenths%10, 10) + "M"
	case n >= 1_000:
		return strconv.FormatUint(n/1_000+boolToUint(n%1_000 >= 500), 10) + "K"
	}
	return strconv.FormatUint(n, 10)
}

func boolToUint(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// formatUTCDate is a calendar day as UTC sees it, "Sep 30", as the page
// prints refill dates.
func formatUTCDate(t time.Time) string { return t.UTC().Format("Jan 2") }

// meterGapAfter reports whether the meter was not counting, for five minutes
// or more, at some point after the anchor. A briefer stop is a restart, an
// update or a reload, and is ignored.
func meterGapAfter(m *client.APIMeter, anchor, now time.Time) bool {
	for _, gap := range m.Gaps {
		if !gap.Brief() && gap.To.After(anchor) {
			return true
		}
	}
	return m.StoppedAt != nil && now.Sub(*m.StoppedAt) >= client.MeterBriefGap
}

// creditMeterOf is the meter as the document carries it, nil without one.
func creditMeterOf(in Input, now time.Time) *client.CreditMeter {
	m := in.Meter
	if m == nil {
		return nil
	}
	out := &client.CreditMeter{
		SinceEpoch:            m.Since.Unix(),
		UpdatedAtEpoch:        m.FlushedAt.Unix(),
		Stale:                 in.StaleAfter > 0 && now.Sub(m.FlushedAt) > in.StaleAfter,
		StoppedAtEpoch:        timePointerEpoch(m.StoppedAt),
		StopReason:            m.StopReason,
		Dropped:               m.Dropped,
		LastDroppedEpoch:      timePointerEpoch(m.LastDroppedAt),
		Unattributed:          m.Unattributed,
		LastUnattributedEpoch: timePointerEpoch(m.LastUnattributedAt),
		Rejected:              m.Rejected,
		LastRejectedEpoch:     timePointerEpoch(m.LastRejectedAt),
		Foreign:               m.Foreign,
		Gaps:                  []client.CreditGap{},
	}
	for _, gap := range m.Gaps {
		if !gap.Brief() {
			out.Gaps = append(out.Gaps, client.CreditGap{FromEpoch: gap.From.Unix(), ToEpoch: gap.To.Unix(), Reason: gap.Reason})
		}
	}
	return out
}

// unlinkedOf is the meter's own list of organizations nothing counts, in its
// order, each with why.
func unlinkedOf(m *client.APIMeter, accounts []*creditAccount) []client.CreditUnlinked {
	out := []client.CreditUnlinked{}
	if m == nil {
		return out
	}
	overLimit := map[string]bool{}
	for _, a := range accounts {
		if c := a.credit; c != nil && c.Problem == client.CreditProblemTooManyItems && c.OrganizationID != "" {
			overLimit[c.OrganizationID] = true
		}
	}
	for _, u := range m.Unlinked {
		reason := client.CreditUnlinkedNotConfigured
		if overLimit[u.OrganizationID] {
			reason = client.CreditUnlinkedOverLimit
		}
		out = append(out, client.CreditUnlinked{
			OrganizationID: u.OrganizationID,
			FirstSeenEpoch: u.FirstSeenAt.Unix(),
			LastSeenEpoch:  u.LastSeenAt.Unix(),
			Requests:       u.Requests,
			Reason:         reason,
		})
	}
	return out
}

// Orphans is every stored entry as an orphan, oldest first: what
// api_credits.orphans would list with no organization listed. api_credits is
// null then, so the values of a removed last organization reach the page
// through this instead.
func Orphans(v settings.Values) []client.CreditOrphan { return orphansOf(v, nil) }

// orphansOf is every stored entry no account uses, oldest first.
func orphansOf(v settings.Values, accounts []*creditAccount) []client.CreditOrphan {
	listed := map[string]bool{}
	for _, a := range accounts {
		listed[a.entry.AuthIndex] = true
	}
	out := []client.CreditOrphan{}
	for id, stored := range v.APICredits {
		if listed[id] {
			continue
		}
		out = append(out, client.CreditOrphan{
			ID: id, MonthlyUSD: stored.MonthlyUSD, Renews: stored.Renews, HasReading: stored.Reading != nil,
			Revision: settings.RevisionText(stored.Rev), UpdatedAtEpoch: stored.UpdatedAt.Unix(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAtEpoch != out[j].UpdatedAtEpoch {
			return out[i].UpdatedAtEpoch < out[j].UpdatedAtEpoch
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// creditPoolOf sums the counted accounts: those in state ok, stale or out.
func creditPoolOf(accounts []*creditAccount, now time.Time) client.CreditPool {
	pool := client.CreditPool{AccountCount: len(accounts)}
	monthly, used, left, overage := new(big.Rat), new(big.Rat), new(big.Rat), new(big.Rat)
	counted := []*creditAccount{}
	for _, a := range accounts {
		if !a.counted {
			pool.MissingCount++
			continue
		}
		counted = append(counted, a)
		monthly.Add(monthly, a.grant)
		used.Add(used, a.used)
		left.Add(left, a.left)
		overage.Add(overage, a.overage)
		pool.LowerBound = pool.LowerBound || a.out.LowerBound
	}
	pool.CountedCount = len(counted)
	if len(counted) == 0 {
		return pool
	}
	pool.HasEstimate = true
	pool.MonthlyCredit, pool.MonthlyCreditText = floatOf(monthly), moneyOf(monthly)
	pool.Used, pool.UsedText = floatOf(used), moneyOf(used)
	pool.Left, pool.LeftText = floatOf(left), moneyOf(left)
	pool.Overage, pool.OverageText = floatOf(overage), moneyOf(overage)
	if monthly.Sign() > 0 {
		pool.RemainingFraction = clampFraction(fractionOf(left, monthly))
		pool.RemainingPercent = percentOf(pool.RemainingFraction)
		pool.Level = levelOf(pool.RemainingFraction)
	}

	// Only a refill that restores something counts: an account that has used
	// none of its credit refills to where it already is.
	var soonest, latest time.Time
	for _, a := range counted {
		if !a.cycle.OK || a.used.Sign() <= 0 {
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
	refill := &client.CreditRefill{
		AccountIDs:      []string{},
		RefillAtEpoch:   soonest.Unix(),
		RefillInSeconds: soonest.Unix() - now.Unix(),
	}
	gain := new(big.Rat)
	for _, a := range counted {
		if a.cycle.OK && a.used.Sign() > 0 && a.cycle.End.Equal(soonest) {
			refill.AccountIDs = append(refill.AccountIDs, a.out.ID)
			gain.Add(gain, a.used)
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

// problemIssueOf turns the configuration problem quota-cache 0.1.x recorded
// on an item into the sentence that tells the operator what to change. The
// page's organizations have none. An unknown problem still reads as a
// problem, so a newer quota-cache's codes degrade to something true; so do the
// codes only 0.1.13 wrote, which name settings 0.1.14 no longer needs.
func problemIssueOf(c *client.APICredit) string {
	if c == nil {
		return "This item's configuration has a problem."
	}
	switch c.Problem {
	case client.CreditProblemItemInvalid:
		return "This item in claude-api-credits is not a list of label, organization-id, monthly-usd and renews."
	case client.CreditProblemUnknownField:
		return "This item has a setting Quota Cache does not know; check the spelling of label, organization-id, monthly-usd and renews."
	case client.CreditProblemTooManyItems:
		return "Only the first 16 items in claude-api-credits are read."
	case client.CreditProblemLabelMissing:
		return "Give this item a label, unique and at most 64 characters."
	case client.CreditProblemLabelInvalid:
		return "This label is over 64 characters or has a character that cannot be shown, such as a control character, a no-break space or an emoji joiner."
	case client.CreditProblemLabelDuplicate:
		return "Another item already uses this label."
	case client.CreditProblemOrganizationIDMissing:
		return "Add this organization's organization-id, from Console under Settings, Organization."
	case client.CreditProblemOrganizationIDInvalid:
		return "organization-id must be the Organization ID from Console, like 12345678-1234-5678-1234-567812345678."
	case client.CreditProblemOrganizationIDDuplicate:
		return "Another item already uses this organization-id."
	}
	return "This item's configuration has a problem."
}

// exactDollars is a validated dollar amount, exactly. Only values
// client.ValidMonthlyUSD accepted reach it, so it cannot fail.
func exactDollars(text string) *big.Rat {
	amount, ok := amountOf(text)
	if !ok {
		return new(big.Rat)
	}
	return amount
}

// clampRat is r clamped to [0, max].
func clampRat(r, max *big.Rat) *big.Rat {
	if r.Sign() < 0 {
		return r.SetInt64(0)
	}
	if r.Cmp(max) > 0 {
		return r.Set(max)
	}
	return r
}

func clampFraction(f float64) float64 {
	switch {
	case f < 0:
		return 0
	case f > 1:
		return 1
	}
	return f
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

// timePointerEpoch is the nullable epoch of an optional instant.
func timePointerEpoch(t *time.Time) *int64 {
	if t == nil {
		return nil
	}
	return epochPointerOf(*t)
}
