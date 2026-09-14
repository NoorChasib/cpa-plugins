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
	Truncated          bool               `json:"truncated,omitempty"`
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
