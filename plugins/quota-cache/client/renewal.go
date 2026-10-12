package client

import (
	"regexp"
	"time"
)

// Where a credential's renewal date comes from: SubscriptionRenewal's source.
const (
	RenewalReported  = "reported"
	RenewalDashboard = "dashboard"
	RenewalEstimated = "estimated"
)

// renewalKey is a CPA auth index as CPA forms one today.
var renewalKey = regexp.MustCompile(`^[0-9a-f]{16}$`)

// ValidRenewalID reports whether id can key a renewal date: a CPA auth index,
// which is 16 lower-case hex digits.
func ValidRenewalID(id string) bool { return renewalKey.MatchString(id) }

// ParseRenewalDate reads a renewal date as the operator saves one: YYYY-MM-DD
// only, a real date in 2000 to 2099, at 00:00 UTC. It is stricter than
// ParseRenewal, which also takes the RFC 3339 form a configuration save can
// write.
func ParseRenewalDate(s string) (time.Time, bool) {
	day, err := time.Parse(time.DateOnly, s)
	if err != nil || day.Format(time.DateOnly) != s || day.Year() < 2000 || day.Year() > 2099 {
		return time.Time{}, false
	}
	return day, true
}

// SubscriptionRenewal is the credential's renewal instant and where it comes
// from, a Renewal value, or nil and "" when there is none. provider is the
// roster's, lower-cased; entry is the credential's snapshot entry, nil when the
// snapshot has none; date is the renewal date the operator saved for it, "" for
// none.
//
// A renewal the provider reports always wins: Codex says when its
// subscription is paid up to, and anything laid over that would only be a
// worse copy of it. Claude reports none at all, so a date the operator set on
// the dashboard comes next, read off claude.ai's billing page; and only
// without one is a date estimated, from when the subscription began.
func SubscriptionRenewal(provider string, entry *Entry, date string, now time.Time) (at *time.Time, source string) {
	if reported := renewalOf(entry, now); reported != nil {
		return reported, RenewalReported
	}
	if set := dashboardRenewalOf(provider, entry, date, now); set != nil {
		return set, RenewalDashboard
	}
	if guess := estimatedRenewalOf(entry, now); guess != nil {
		return guess, RenewalEstimated
	}
	return nil, ""
}

// dashboardRenewalOf is the next renewal on the date the operator set: that
// date at 00:00 UTC while it is ahead, and once it has come the next one on
// the same day of the month, or of the year on an annual plan, strictly after
// now. On the renewal day itself that is next month's, so the page never
// counts down past zero, as with an estimate. Codex never takes one: it
// reports its own.
func dashboardRenewalOf(provider string, entry *Entry, date string, now time.Time) *time.Time {
	if date == "" || provider != "claude" {
		return nil
	}
	day, ok := ParseRenewalDate(date)
	if !ok {
		return nil
	}
	if !day.After(now) {
		if day, ok = NextAnniversary(day, billingMonthsOf(entry), now); !ok {
			return nil
		}
	}
	return &day
}

// SubscriptionRenewalEstimate is a Claude credential's estimated renewal
// whatever wins, the date Use estimate returns to; nil for any other provider.
// Codex reports its own date and has no estimate to return to.
func SubscriptionRenewalEstimate(provider string, entry *Entry, now time.Time) *time.Time {
	if provider != "claude" {
		return nil
	}
	return estimatedRenewalOf(entry, now)
}

// billingMonthsOf is the subscription's billing period in months, read off the
// last observation: 12 on an annual plan, else 1.
func billingMonthsOf(entry *Entry) int {
	if entry != nil && entry.Quota != nil && entry.Quota.BillingPeriod == BillingAnnual {
		return 12
	}
	return 1
}

// renewalOf is the subscription's renewal instant, when quota-cache read one.
// One already behind us is dropped: the subscription either renewed, and the
// next poll will carry the new date, or it lapsed, and either way the old date
// says nothing true about the future.
func renewalOf(entry *Entry, now time.Time) *time.Time {
	if entry == nil || entry.RenewalAt == nil || !entry.RenewalAt.After(now) {
		return nil
	}
	at := *entry.RenewalAt
	return &at
}

// estimatedRenewalOf is the next billing anniversary of the subscription's
// start, or nil when quota-cache has not read a start.
//
// Anthropic does not report when a Claude subscription renews: neither Claude
// Code nor the CPA management centre shows a date, because there is none to
// show. What the profile does report is when the subscription was created, and
// subscriptions are billed on the anniversary of that — monthly unless the
// usage response says the plan is annual. That makes the date right for an
// account whose billing has never moved, and wrong by however far it has moved
// for one that was paused, upgraded mid-cycle, or re-subscribed; the document
// says it is an estimate so the page can too.
//
// The cadence is read off the last observation the way the reset count is:
// "monthly", "annual", or nothing. Nothing — a quota-cache too old to read it,
// or a response that did not say — is taken as monthly, which is how almost
// every Claude subscription bills, and an annual plan read as monthly still
// lands on a true anniversary day of the month, if not the right month.
func estimatedRenewalOf(entry *Entry, now time.Time) *time.Time {
	if entry == nil || entry.AccountDetails == nil || entry.AccountDetails.SubscriptionStartedAt == nil {
		return nil
	}
	at, ok := NextAnniversary(*entry.AccountDetails.SubscriptionStartedAt, billingMonthsOf(entry), now)
	if !ok {
		return nil
	}
	return &at
}

// NextAnniversary is the first anniversary of start strictly after now, every
// months months, and false when start is still ahead of now: a subscription
// that has not begun has no renewal to estimate.
//
// The rule is the one card billing uses for a subscription's anchor. Each
// anniversary keeps the start's day of the month and time of day in UTC, and
// in a month too short for that day it falls on the month's last day instead
// — a start on the 31st renews on Feb 28 (29 in a leap year), Mar 31, Apr 30.
// Every anniversary is counted from the start itself, never from the one
// before it, so a short month does not drag the day down for good: Feb 28 is
// followed by Mar 31, not Mar 28. A yearly plan begun on Feb 29 renews on Feb
// 28 outside leap years, and on the 29th again when there is one.
//
// "Strictly after" is what a countdown wants. At the anniversary's own second
// the renewal is happening, not ahead, and the next one is what is worth
// counting down to.
func NextAnniversary(start time.Time, months int, now time.Time) (time.Time, bool) {
	// Whole seconds, like everything else in the document. A start carrying a
	// fraction would otherwise put an anniversary a fraction past a now that
	// Build has already truncated: after it by the clock, equal to it once
	// written out, and printed as a renewal that has just passed.
	//
	// And both in UTC, the calendar the months below are counted on. A now in
	// a zone ahead of UTC can already be in next month there — 22:30 UTC on
	// Sep 30 is Oct 1 in Sydney — and counted from that calendar the months
	// run one long, past an anniversary still ahead, which the loop below can
	// only move further from. Build's shipped callers pass UTC; this does not
	// rely on it.
	start, now = start.UTC().Truncate(time.Second), now.UTC()
	if months < 1 || start.After(now) {
		return time.Time{}, false
	}
	// Months from start to now, rounded down to a whole number of periods. The
	// anniversary that many months on is no later than now's month, so at most
	// one more period is ever needed — however many years back the start is.
	elapsed := (now.Year()-start.Year())*12 + int(now.Month()) - int(start.Month())
	n := elapsed - elapsed%months
	at := anniversary(start, n)
	for !at.After(now) {
		n += months
		at = anniversary(start, n)
	}
	return at, true
}

// anniversary is start moved n calendar months on, with its day of the month
// clamped to the last day of a shorter month. time.AddDate does not clamp: it
// normalises Jan 31 plus one month to Mar 3, which is no billing date at all.
func anniversary(start time.Time, n int) time.Time {
	year, month, day := start.Date()
	index := int(month) - 1 + n
	year, month = year+index/12, time.Month(index%12+1)
	// Day zero of the following month is the last day of this one.
	if last := time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day(); day > last {
		day = last
	}
	return time.Date(year, month, day, start.Hour(), start.Minute(), start.Second(), 0, time.UTC)
}
