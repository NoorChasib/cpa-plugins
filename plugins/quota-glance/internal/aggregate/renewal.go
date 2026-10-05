package aggregate

import (
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// renewalFor is the credential's renewal instant and whether it is an
// estimate.
//
// A renewal the provider reports always wins, and is never marked estimated:
// Codex says when its subscription is paid up to, and a guess laid over that
// would only be a worse copy of it. Only where there is no such date — Claude,
// whose provider reports none at all — is one estimated, from when the
// subscription began.
func renewalFor(r record, now time.Time) (epoch *int64, estimated bool) {
	if reported := renewalOf(r, now); reported != nil {
		return reported, false
	}
	if guess := estimatedRenewalOf(r, now); guess != nil {
		return guess, true
	}
	return nil, false
}

// renewalOf is the subscription's renewal instant, when quota-cache read one.
// One already behind us is dropped: the subscription either renewed, and the
// next poll will carry the new date, or it lapsed, and either way the old date
// says nothing true about the future.
func renewalOf(r record, now time.Time) *int64 {
	if !r.hasEntry || r.entry.RenewalAt == nil || !r.entry.RenewalAt.After(now) {
		return nil
	}
	epoch := r.entry.RenewalAt.Unix()
	return &epoch
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
func estimatedRenewalOf(r record, now time.Time) *int64 {
	if !r.hasEntry || r.entry.AccountDetails == nil || r.entry.AccountDetails.SubscriptionStartedAt == nil {
		return nil
	}
	months := 1
	if r.entry.Quota != nil && r.entry.Quota.BillingPeriod == qc.BillingAnnual {
		months = 12
	}
	at, ok := nextAnniversary(*r.entry.AccountDetails.SubscriptionStartedAt, months, now)
	if !ok {
		return nil
	}
	epoch := at.Unix()
	return &epoch
}

// nextAnniversary is the first anniversary of start strictly after now, every
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
func nextAnniversary(start time.Time, months int, now time.Time) (time.Time, bool) {
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
