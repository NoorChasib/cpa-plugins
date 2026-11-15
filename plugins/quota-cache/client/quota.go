package client

import "time"

// Quota is an allowlisted projection of one successful provider response.
// Missing values mean unknown. Amounts remain decimal strings in their stated
// provider units; no currency conversion or remaining-balance inference occurs.
type Quota struct {
	Schema             int                `json:"schema"`
	ObservedAt         time.Time          `json:"observed_at"`
	ActiveLimit        string             `json:"active_limit,omitempty"`
	LimitReachedReason string             `json:"limit_reached_reason,omitempty"`
	Plan               string             `json:"plan,omitempty"`
	TierName           string             `json:"tier_name,omitempty"`
	Windows            map[string]Window  `json:"windows,omitempty"`
	Limits             map[string]Limit   `json:"limits,omitempty"`
	Balances           map[string]Balance `json:"balances,omitempty"`
	UnifiedBilling     *bool              `json:"unified_billing,omitempty"`
	ResetCredits       *ResetCredits      `json:"reset_credits,omitempty"`
	// BillingPeriod is how often a Claude subscription is billed, as the
	// usage response's cedar_ember block states it in event_props:
	// BillingMonthly or BillingAnnual. Empty for every other provider, and
	// whenever the block is absent, says "unknown", or says anything else.
	//
	// It rides on the usage request every poll already makes, so it costs
	// nothing, and it is what turns AccountDetails.SubscriptionStartedAt into
	// an estimated renewal: the same anniversary monthly or yearly.
	BillingPeriod string `json:"billing_period,omitempty"`
	// CostReport is an anthropic-api entry's daily spend as Anthropic's cost
	// report returned it. Nil for every other provider.
	//
	// Deprecated: written only by quota-cache 0.1.13; 0.1.14 never writes it.
	CostReport *CostReport `json:"cost_report,omitempty"`
	Truncated  bool        `json:"truncated,omitempty"`
}

// The values Quota.BillingPeriod takes. Anything else the provider says is
// stored as empty, so a reader never has to guess at a cadence it was not
// built for.
const (
	BillingMonthly = "monthly"
	BillingAnnual  = "annual"
)

// ResetCredits is an account's inventory of banked rate-limit resets:
// entitlements already granted to the account, which clear its current windows
// when spent. Codex calls them rate-limit reset credits; Claude calls them reset
// grants.
//
// A banked reset is not extra allowance. Spending one restores the account's
// session and weekly windows, or for some Claude kinds less (see Kinds), so it
// is a thing the account holds rather than a thing it has used, and it belongs
// here beside the balances rather than among the windows.
//
// Nil for every provider that has no such concept, and nil for an account that
// has none banked, so a reader may treat presence as "there is at least one to
// spend".
type ResetCredits struct {
	AvailableCount int `json:"available_count"`
	// SoonestExpiry is the earliest expiry among the available credits. A
	// banked reset lapses — a Codex credit thirty days after it is granted, a
	// Claude grant at its own end date — and the count alone cannot say that
	// one is about to, which is the documented way operators lose them. Nil
	// when it could not be dated: Codex's inventory endpoint was not read or
	// did not date its entries, or no live Claude grant carried an end date.
	// The count above is still authoritative.
	SoonestExpiry *time.Time `json:"soonest_expiry,omitempty"`
	// Hold is the provider's own reason that none can be spent right now, as of
	// this observation: "not_limited" (the provider only allows a reset once the
	// account is at a limit), "cooldown" (one was spent recently), "paused", or
	// "ineligible". Empty when one can be spent now or the provider does not
	// say, which is always the case for Codex. It is a reading, not a verdict:
	// a reset is only ever attempted after a fresh check at the provider.
	Hold string `json:"hold,omitempty"`
	// HoldUntil is when a "cooldown" hold lifts, when the provider says.
	HoldUntil *time.Time `json:"hold_until,omitempty"`
	// Kinds breaks the count down by what spending one does, for a provider
	// whose banked resets are not all alike. Claude's are not: a full reset
	// clears the 5-hour and weekly limits, while a Fable reset only gives back
	// to the weekly limit what Fable used, and spending the wrong one is the
	// mistake this exists to prevent.
	//
	// One entry per kind the account holds, never two for the same kind, in
	// ResetKindRank order, and the counts sum to AvailableCount. Nil for Codex,
	// whose credits are all one sort. Nil too from a Quota Cache older than
	// this field, which a reader must take as "the kinds were not read" and
	// never as "every one is a full reset".
	Kinds []ResetKindCredits `json:"kinds,omitempty"`
}

// ResetKindCredits is the part of an account's banked resets that are of one
// kind. Its fields mean what ResetCredits' do, restricted to that kind: the
// expiry is the soonest of this kind's, and the hold is why none of this kind
// can be spent right now.
type ResetKindCredits struct {
	// Kind is one of the ResetKind values.
	Kind           string     `json:"kind"`
	AvailableCount int        `json:"available_count"`
	SoonestExpiry  *time.Time `json:"soonest_expiry,omitempty"`
	Hold           string     `json:"hold,omitempty"`
	HoldUntil      *time.Time `json:"hold_until,omitempty"`
}

// The values ResetCredits.Hold takes. Readers must treat any other value as
// "the provider gave a reason this reader does not know", never as spendable.
const (
	HoldNotLimited = "not_limited"
	HoldCooldown   = "cooldown"
	HoldPaused     = "paused"
	HoldIneligible = "ineligible"
)

// The kinds of Claude reset grant, as Claude Code 2.1.322 names them.
//
// A grant is a full reset unless the provider says otherwise; the other kinds
// each refill less. ResetKindUnknown is a grant this build cannot classify
// with confidence: it is counted, so the account's total stays true, but it
// is never something a reader may offer to spend, because a reset labelled
// as the wrong kind is exactly the mix-up the kinds are there to prevent.
const (
	// ResetKindFull clears the 5-hour and the weekly limits.
	ResetKindFull = "full"
	// ResetKindFable gives back to the weekly limit what Fable used this week.
	ResetKindFable = "fable"
	// ResetKindWeekly clears the weekly limits and not the 5-hour one.
	ResetKindWeekly = "weekly"
	// ResetKindSession clears the 5-hour limit and not the weekly one.
	ResetKindSession = "session"
	// ResetKindUnknown is a grant this build cannot classify.
	ResetKindUnknown = "unknown"
)

// ResetKindRank is where a kind sorts in ResetCredits.Kinds: full first, then
// Fable, weekly, session, and anything unknown last. A kind outside the
// vocabulary sorts with the unknown.
func ResetKindRank(kind string) int {
	switch kind {
	case ResetKindFull:
		return 0
	case ResetKindFable:
		return 1
	case ResetKindWeekly:
		return 2
	case ResetKindSession:
		return 3
	}
	return 4
}

// ClaudeResetKind classifies one grant object of the usage response's
// cedar_ember block by what spending it does, by Claude Code 2.1.322's rule:
//
//  1. gives_back_weekly true is a Fable reset, whatever else the grant says;
//  2. otherwise the grant's own type, when it is "session", "weekly" or "full";
//  3. otherwise what clears names, counting only the usage windows Claude Code
//     knows: the 5-hour window alone is a session reset, other windows
//     without the 5-hour one a weekly reset, and both, or none at all, a full
//     reset — the grants that predate kinds named none and cleared everything.
//
// Where Claude Code falls back silently this does not. A gives_back_weekly,
// type or clears that is present and malformed, or a type this build does not
// know, is ResetKindUnknown rather than a guess: a Fable reset misread as a
// full one is the mistake both the dashboard and the spend must not make, and
// both classify through this one function so they cannot disagree.
//
// The grant is the decoded JSON object; numbers may be json.Number or
// float64, since nothing here reads one.
func ClaudeResetKind(grant map[string]any) string {
	switch v := grant["gives_back_weekly"].(type) {
	case nil:
	case bool:
		if v {
			return ResetKindFable
		}
	default:
		return ResetKindUnknown
	}
	switch v := grant["type"].(type) {
	case nil:
	case string:
		switch v {
		case ResetKindSession, ResetKindWeekly, ResetKindFull:
			return v
		}
		return ResetKindUnknown
	default:
		return ResetKindUnknown
	}
	var clears []any
	switch v := grant["clears"].(type) {
	case nil:
	case []any:
		clears = v
	default:
		return ResetKindUnknown
	}
	session, other := false, false
	for _, item := range clears {
		window, _ := item.(string)
		switch {
		case window == "five_hour":
			session = true
		case claudeGrantWindows[window]:
			other = true
		}
	}
	switch {
	case session && !other:
		return ResetKindSession
	case other && !session:
		return ResetKindWeekly
	}
	return ResetKindFull
}

// claudeGrantWindows are the usage windows Claude Code 2.1.322 keeps from a
// grant's clears; it drops any other name before classifying, and so does
// ClaudeResetKind.
var claudeGrantWindows = map[string]bool{
	"five_hour":                  true,
	"seven_day":                  true,
	"seven_day_overage_included": true,
	"seven_day_opus":             true,
	"seven_day_sonnet":           true,
	"seven_day_cowork":           true,
	"seven_day_omelette":         true,
	"seven_day_oauth_apps":       true,
}

type Window struct {
	UsedPercent     *float64   `json:"used_percent,omitempty"`
	DurationSeconds *int64     `json:"duration_seconds,omitempty"`
	StartsAt        *time.Time `json:"starts_at,omitempty"`
	ResetsAt        *time.Time `json:"resets_at,omitempty"`
	Period          string     `json:"period,omitempty"`
}
type Limit struct {
	MeteredFeature string `json:"metered_feature,omitempty"`
	Allowed        *bool  `json:"allowed,omitempty"`
	Reached        *bool  `json:"reached,omitempty"`
}
type Balance struct {
	RemainingPercent *float64   `json:"remaining_percent,omitempty"`
	ResetsAt         *time.Time `json:"resets_at,omitempty"`
	Source           string     `json:"source,omitempty"`
	Unit             string     `json:"unit"`
	Used             string     `json:"used,omitempty"`
	Limit            string     `json:"limit,omitempty"`
	Remaining        string     `json:"remaining,omitempty"`
	UsedPercent      *float64   `json:"used_percent,omitempty"`
	Enabled          *bool      `json:"enabled,omitempty"`
	HasCredits       *bool      `json:"has_credits,omitempty"`
	Unlimited        *bool      `json:"unlimited,omitempty"`
}

// ReadQuota returns a recent successful extended observation. Individual
// windows may have reset: use ReadWindow when acting on a window.
func ReadQuota(path, provider, authIndex string, now time.Time, maxAge time.Duration) (Quota, error) {
	s, err := Load(path)
	if err != nil {
		return Quota{}, err
	}
	e, ok := s.Entries[Key(provider, authIndex)]
	if !ok || e.Provider != provider || e.AuthIndex != authIndex || e.LastError != "" || e.Quota == nil || e.Quota.Schema != 1 || maxAge <= 0 || e.Quota.ObservedAt.IsZero() || e.Quota.ObservedAt.After(now) || now.Sub(e.Quota.ObservedAt) > maxAge {
		return Quota{}, ErrUnavailable
	}
	return *e.Quota, nil
}
func ReadWindow(path, provider, authIndex, window string, now time.Time, maxAge time.Duration) (Window, error) {
	q, err := ReadQuota(path, provider, authIndex, now, maxAge)
	if err != nil {
		return Window{}, err
	}
	w, ok := q.Windows[window]
	if !ok || (w.UsedPercent == nil && w.ResetsAt == nil) || (w.StartsAt != nil && w.StartsAt.After(now)) || (w.ResetsAt != nil && !w.ResetsAt.After(now)) {
		return Window{}, ErrUnavailable
	}
	return w, nil
}
