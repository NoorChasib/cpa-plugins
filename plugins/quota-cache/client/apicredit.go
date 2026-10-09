package client

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"
)

// ProviderAnthropicAPI is the snapshot provider for a Claude Console
// organization's monthly API credit. It is separate from "claude", the
// subscription poll, so a subscription 429 never pauses these reads. A 429
// from Anthropic's Admin API pauses only the organization that got it: each
// has its own Admin API key and rate limits, so the provider is never paused.
//
// Its entries are not CPA credentials. quota-cache creates one per item of its
// own claude-api-credits configuration and keys it APICreditAccount(label).
// Like OpenRouter's, an entry has no weekly window: used_percent, reset_at and
// observed_at stay empty, and quota.observed_at dates the reading.
const ProviderAnthropicAPI = "anthropic-api"

// APICredit is what the operator configured for one Console organization's
// monthly API credit, copied onto the entry at every scan whether or not the
// organization is polled. Values are kept as the operator wrote them (trimmed);
// quota-cache validates them but never converts or computes with them.
//
// Anthropic reports neither the credit's amount nor its renewal date, so both
// come from here. A consumer computes the cycle with CreditCycleAt(Renews, t)
// and the amount from MonthlyUSD.
type APICredit struct {
	// Label is the operator's unique display name for the organization. Empty
	// when the configured label is missing or invalid; Problem then says so.
	Label string `json:"label"`
	// Position is the item's zero-based index in the configured list. It orders
	// the accounts for display, and when two items read the same organization
	// the one listed first is the one counted.
	Position int `json:"position"`
	// MonthlyUSD is the monthly credit in US dollars, a decimal with at most two
	// places ("200", "260.50"). Empty when it is missing or invalid.
	MonthlyUSD string `json:"monthly_usd,omitempty"`
	// Renews is a date on which the organization's next credit is deposited:
	// the day the linked claude.ai plan renews, as a UTC calendar date. It is
	// not Console's "expires" date for the current credit, which may be the
	// day before. Written "2026-10-29", or the same date at 00:00:00 UTC in
	// RFC 3339 form, which is what a save from CPA's plugin panel turns an
	// unquoted YAML date into. Only its day of the month matters; see
	// CreditCycleAt. Empty when it is missing or invalid.
	Renews string `json:"renews,omitempty"`
	// KeyFingerprint is "key-" and the first 12 hex digits of the SHA-256 of the
	// configured key. Never the key. Empty when no well-formed key is set.
	KeyFingerprint string `json:"key_fingerprint,omitempty"`
	// Problem is the first configuration problem found in the item, one of the
	// CreditProblem values, or empty. An item with a problem is never polled;
	// every other item keeps polling.
	Problem string `json:"problem,omitempty"`
}

// CreditProblem values: why an item of claude-api-credits cannot be polled.
// Fields are checked in the order label, admin-key, monthly-usd, renews, and
// only the first problem is reported. Readers must treat an unknown value as
// "misconfigured for a reason this reader does not know".
const (
	CreditProblemItemInvalid      = "item_invalid"       // the item is not a mapping of scalar values, or repeats a key
	CreditProblemUnknownField     = "unknown_field"      // the item has a key other than the four below
	CreditProblemTooManyItems     = "too_many_items"     // the item is past MaxAPICreditItems
	CreditProblemLabelMissing     = "label_missing"      // label is absent or empty
	CreditProblemLabelInvalid     = "label_invalid"      // label is over 64 characters or has a non-printable character (unicode.IsPrint: a control character, a space other than U+0020, or a joiner such as U+200D)
	CreditProblemLabelDuplicate   = "label_duplicate"    // an earlier item has the same label, ignoring case
	CreditProblemAdminKeyMissing  = "admin_key_missing"  // admin-key is absent or empty
	CreditProblemAdminKeyInvalid  = "admin_key_invalid"  // admin-key is not shaped like an Anthropic key (sk-ant-...); Anthropic judges the rest
	CreditProblemAdminKeyRepeated = "admin_key_repeated" // an earlier item has the same admin-key
	CreditProblemMonthlyMissing   = "monthly_usd_missing"
	CreditProblemMonthlyInvalid   = "monthly_usd_invalid" // not a non-negative decimal with at most two places
	CreditProblemRenewsMissing    = "renews_missing"
	CreditProblemRenewsInvalid    = "renews_invalid" // not a real calendar date accepted by ParseRenewal
)

// MaxAPICreditItems bounds the configured list. Items past it are kept, with
// CreditProblemTooManyItems, and never polled.
const MaxAPICreditItems = 16

// Failure messages an anthropic-api entry's last_error can carry, beside the
// two every provider can: "refresh pending" while a poll is in flight, and
// "provider rate limited" after a 429 (which backs off only this
// organization), and the generic "quota fetch failed" for a transport failure.
// They are static: nothing Anthropic sends is ever copied into one.
const (
	CreditErrorKeyRejected = "admin key rejected"      // HTTP 401
	CreditErrorForbidden   = "admin key not permitted" // HTTP 403
	CreditErrorUnavailable = "cost report unavailable" // HTTP 404
	CreditErrorRefused     = "request refused"         // HTTP 400 and any other 4xx but 401, 403, 404 and 429
	CreditErrorUpstream    = "anthropic server error"  // HTTP 5xx, including 529
	CreditErrorResponse    = "unreadable response"     // 2xx with a body that fails validation
)

// CostReport is one successful read of an organization's daily spend from
// Anthropic's cost report (GET /v1/organizations/cost_report), kept as the
// provider sent it. It lives on Quota, so like every observation it is
// replaced whole by the next successful poll and kept, under last_error, by a
// failed one.
//
// It is gross spend: everything the organization was charged, paid from the
// monthly credit or from purchased credit alike. quota-cache adds nothing up;
// the consumer sums the days of the cycle it computes, up to CoveredUntil.
type CostReport struct {
	// OrganizationID is the organization the key belongs to, from the
	// anthropic-organization-id header of the cost report's response, or from
	// GET /v1/organizations/me when that header is absent. It is what tells a
	// consumer that two configured items read the same organization.
	OrganizationID string `json:"organization_id"`
	// KeyFingerprint is the fingerprint of the key this report was read with.
	// When it differs from the entry's APICredit.KeyFingerprint, the key was
	// changed after this report, which may describe another organization.
	KeyFingerprint string `json:"key_fingerprint"`
	// StartingAt and EndingAt are the window this poll asked for, [StartingAt,
	// EndingAt): 00:00 UTC on the first day of the cycle that contained the
	// poll, and CostReportEndingAt(Quota.ObservedAt), 00:00 UTC on the day
	// after the poll. Asking no further than that keeps every bucket in the
	// past or present, and a cycle's window to at most 31 daily buckets.
	StartingAt time.Time `json:"starting_at"`
	EndingAt   time.Time `json:"ending_at"`
	// Days is one entry per daily bucket, oldest first, exactly as returned,
	// including days with no spend. They run without a gap from StartingAt.
	// quota-cache guarantees every day before the one containing
	// Quota.ObservedAt; that day itself, which is still in progress, may be
	// missing, and CoveredUntil is how a consumer tells "no spend today" from
	// "today not reported".
	Days []CostDay `json:"days"`
}

// CoveredUntil is the end of the days this report covers: StartingAt plus 24
// hours for each day in Days that follows the previous one without a gap. Spend
// in [StartingAt, CoveredUntil) is exact to the UTC day; nothing at or after it
// was reported, which is not the same as nothing spent. A report with no days
// covers nothing, and CoveredUntil is StartingAt.
func (r *CostReport) CoveredUntil() time.Time {
	if r == nil {
		return time.Time{}
	}
	until := r.StartingAt
	for _, day := range r.Days {
		if !day.StartingAt.Equal(until) {
			break
		}
		until = until.Add(24 * time.Hour)
	}
	return until
}

// CostReportEndingAt is the ending_at quota-cache sends with a poll at now:
// 00:00 UTC on the UTC day after now. The cost report returns the buckets that
// end by it, so the window stops at the bucket in progress, never asks for a
// future one, and never runs past the cycle that contains now, whose end is
// always a later 00:00 UTC.
func CostReportEndingAt(now time.Time) time.Time {
	year, month, day := now.UTC().Date()
	return time.Date(year, month, day+1, 0, 0, 0, 0, time.UTC)
}

// CostDay is one daily bucket of the cost report.
type CostDay struct {
	// StartingAt is 00:00 UTC of the day; the bucket ends 24 hours later.
	StartingAt time.Time `json:"starting_at"`
	// Amounts are the bucket's results as returned. Empty, never null, for a
	// day with no spend. Without group_by Anthropic returns at most one, but a
	// consumer must sum however many there are.
	Amounts []CostAmount `json:"amounts"`
}

// CostAmount is one result of a bucket, verbatim.
type CostAmount struct {
	// Amount is a decimal string in the LOWEST units of Currency, which for
	// USD is cents: "123.45" is $1.2345. Anthropic sends fractions of a cent.
	Amount string `json:"amount"`
	// Currency is the ISO 4217 code Anthropic states; currently always "USD".
	Currency string `json:"currency"`
}

// APICreditAccount is the snapshot auth_index of the configured item with this
// label: "label-" and the first 12 hex digits of the SHA-256 of the label,
// trimmed and lower-cased. It depends on nothing but the label, so rotating an
// organization's admin key, or correcting its amount or date, keeps the
// account; renaming the label starts a new one. quota-cache names an item with
// no usable label "item-<position+1>" instead.
func APICreditAccount(label string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(label))))
	return "label-" + hex.EncodeToString(sum[:6])
}

var monthlyUSD = regexp.MustCompile(`^[0-9]{1,7}(?:\.[0-9]{1,2})?$`)

// ValidMonthlyUSD reports whether s is an acceptable monthly-usd value: a
// non-negative decimal in dollars with at most seven whole digits and two
// decimal places. Zero is allowed: an organization with no credit this month.
func ValidMonthlyUSD(s string) bool { return monthlyUSD.MatchString(s) }

// ErrRenewalInvalid is returned for a renewal date ParseRenewal does not accept.
var ErrRenewalInvalid = errors.New("renewal date must be a calendar date such as 2026-10-29")

// ParseRenewal reads a configured renewal date: "2026-10-29", or an RFC 3339
// instant at exactly 00:00:00 UTC ("2026-10-29T00:00:00Z"), which is the form a
// save from CPA's plugin panel turns an unquoted YAML date into. The date must
// exist (no Feb 30) and fall in 2000 to 2099. The result is that date at 00:00
// UTC.
func ParseRenewal(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	day, err := time.Parse(time.DateOnly, s)
	if err != nil {
		instant, err2 := time.Parse(time.RFC3339Nano, s)
		if err2 != nil {
			return time.Time{}, ErrRenewalInvalid
		}
		instant = instant.UTC()
		if instant.Hour() != 0 || instant.Minute() != 0 || instant.Second() != 0 || instant.Nanosecond() != 0 {
			return time.Time{}, ErrRenewalInvalid
		}
		day = instant
	}
	if day.Year() < 2000 || day.Year() > 2099 {
		return time.Time{}, ErrRenewalInvalid
	}
	return day, nil
}

// CreditCycle is one monthly API-credit cycle: [Start, End).
type CreditCycle struct {
	// Start is 00:00 UTC on the renewal day the cycle began.
	Start time.Time
	// End is 00:00 UTC on the next renewal day: when this cycle's credit
	// expires and the next one is deposited.
	End time.Time
}

// CreditCycleAt is the one rule both quota-cache and quota-glance use for an
// organization's monthly credit cycle. Keep it the only one, so that the window
// quota-cache asks Anthropic for and the window quota-glance sums can never
// disagree.
//
// The cycle containing t starts at 00:00 UTC on the most recent occurrence of
// the renewal day of the month on or before t, and ends at the next one. The
// day is renews' day of the month, re-anchored every month and clamped to the
// month's length: a renewal day of 31 falls on Oct 31, Nov 30, Dec 31 and Feb
// 28 (29 in a leap year), and is back on Mar 31 after that. Only the day of
// renews matters; its month and year are ignored, so a date in the past or the
// future works alike.
//
// Start is inclusive and End exclusive: at 00:00 UTC on the renewal day the new
// cycle has begun. Anthropic's cost report resolves to UTC days, so the whole
// renewal day counts toward the new cycle, although the credit is deposited
// "shortly after" payment: spend made on the renewal day before the deposit is
// counted against the new credit. That can overstate the new cycle's spend by
// at most part of one day.
func CreditCycleAt(renews string, t time.Time) (CreditCycle, error) {
	anchor, err := ParseRenewal(renews)
	if err != nil {
		return CreditCycle{}, err
	}
	return cycleOn(anchor.Day(), t), nil
}

// cycleOn is the cycle containing t for a renewal on day of the month.
func cycleOn(day int, t time.Time) CreditCycle {
	t = t.UTC()
	year, month, _ := t.Date()
	this := renewalIn(year, month, day)
	if !this.After(t) {
		return CreditCycle{Start: this, End: renewalIn(year, month+1, day)}
	}
	return CreditCycle{Start: renewalIn(year, month-1, day), End: this}
}

// renewalIn is 00:00 UTC on day of the given month, clamped to the month's last
// day. month may be 0 or 13; time.Date normalizes the year.
func renewalIn(year int, month time.Month, day int) time.Time {
	first := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	// Day zero of the following month is the last day of this one.
	if last := time.Date(first.Year(), first.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day(); day > last {
		day = last
	}
	return time.Date(first.Year(), first.Month(), day, 0, 0, 0, 0, time.UTC)
}
