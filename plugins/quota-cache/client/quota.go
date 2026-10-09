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
// session and weekly windows, so it is a thing the account holds rather than a
// thing it has used, and it belongs here beside the balances rather than among
// the windows.
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
}

// The values ResetCredits.Hold takes. Readers must treat any other value as
// "the provider gave a reason this reader does not know", never as spendable.
const (
	HoldNotLimited = "not_limited"
	HoldCooldown   = "cooldown"
	HoldPaused     = "paused"
	HoldIneligible = "ineligible"
)

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
