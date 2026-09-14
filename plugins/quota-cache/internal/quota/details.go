package quota

import (
	"encoding/json"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"math"
	"regexp"
	"sort"
	"strconv"
	"time"
)

const maxDetails = 32

var decimalValue = regexp.MustCompile(`^-?[0-9]+(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

// Bounded allowlist for any label copied out of a provider response. Kept
// deliberately narrow; parentheses and "+" are included because real plan names
// use them ("Pro (20x)", "Team+") and were otherwise dropped in silence.
var detailName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.()+/-]{0,63}$`)

func object(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		if v, ok := m[k].(map[string]any); ok {
			return v
		}
	}
	return nil
}
func boolean(m map[string]any, keys ...string) *bool {
	for _, k := range keys {
		if v, ok := m[k].(bool); ok {
			return &v
		}
	}
	return nil
}
func percent(m map[string]any, keys ...string) *float64 {
	if v, ok := numberField(m, keys...); ok && v >= 0 {
		return &v
	}
	return nil
}
func timestamp(m map[string]any, keys ...string) *time.Time {
	if v, ok := rfc3339Field(m, keys...); ok {
		return &v
	}
	return nil
}
func name(m map[string]any, keys ...string) string {
	v := firstStringField(m, keys...)
	if detailName.MatchString(v) {
		return v
	}
	return ""
}
func decimal(m map[string]any, keys ...string) string {
	for _, k := range keys {
		var s string
		switch v := m[k].(type) {
		case json.Number:
			s = v.String()
		case string:
			s = v
		}
		if len(s) == 0 || len(s) > 64 || !decimalValue.MatchString(s) {
			continue
		}
		v, err := strconv.ParseFloat(s, 64)
		if err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) {
			return s
		}
	}
	return ""
}
func cents(m map[string]any, keys ...string) string {
	o := object(m, keys...)
	if o == nil {
		return ""
	}
	if len(o) == 0 {
		return "0"
	}
	return decimal(o, "val")
}
func addWindow(q *client.Quota, id string, w client.Window) {
	if w.UsedPercent == nil && w.ResetsAt == nil {
		return
	}
	if len(q.Windows) >= maxDetails {
		q.Truncated = true
		return
	}
	q.Windows[id] = w
}
func addBalance(q *client.Quota, id string, b client.Balance) {
	if b.Used == "" && b.Limit == "" && b.Remaining == "" && b.UsedPercent == nil && b.RemainingPercent == nil && b.ResetsAt == nil && b.Enabled == nil && b.HasCredits == nil && b.Unlimited == nil {
		return
	}
	q.Balances[id] = b
}
func parseDetails(provider string, root map[string]any, now time.Time) *client.Quota {
	q := &client.Quota{Schema: 1, ObservedAt: now, Windows: map[string]client.Window{}, Limits: map[string]client.Limit{}, Balances: map[string]client.Balance{}}
	switch provider {
	case "claude":
		// The subscription object is reported alongside usage and costs no
		// extra request. Names go through the same bounded validation as every
		// other label, so a malformed value is omitted rather than stored.
		subscription := object(root, "subscription")
		q.Plan = name(subscription, "plan")
		q.TierName = name(subscription, "tierName", "tier_name")
		for _, id := range []string{"five_hour", "seven_day", "seven_day_oauth_apps", "seven_day_opus", "seven_day_sonnet", "seven_day_cowork"} {
			w := object(root, id)
			duration := int64(604800)
			if id == "five_hour" {
				duration = 18000
			}
			addWindow(q, id, client.Window{UsedPercent: percent(w, "utilization"), ResetsAt: timestamp(w, "resets_at"), DurationSeconds: &duration})
		}
		extra := object(root, "extra_usage")
		addBalance(q, "extra_usage", client.Balance{Unit: "provider_units", Used: decimal(extra, "used_credits"), Limit: decimal(extra, "monthly_limit"), UsedPercent: percent(extra, "utilization"), Enabled: boolean(extra, "is_enabled")})
	case "codex":
		q.Plan = name(root, "plan_type", "planType")
		q.ActiveLimit = name(root, "metered_limit_name", "meteredLimitName", "limit_name", "limitName")
		q.LimitReachedReason = name(object(root, "rate_limit_reached_type", "rateLimitReachedType"), "type")
		spend := object(root, "spend_control", "spendControl")
		if reached := boolean(spend, "reached"); reached != nil {
			q.Limits["spend_control"] = client.Limit{Reached: reached}
		}
		individual := object(spend, "individual_limit", "individualLimit")
		balance := client.Balance{Unit: "provider_units", Used: decimal(individual, "used"), Limit: decimal(individual, "limit"), Remaining: decimal(individual, "remaining"), UsedPercent: percent(individual, "used_percent", "usedPercent"), RemainingPercent: percent(individual, "remaining_percent", "remainingPercent"), Source: name(individual, "source")}
		if reset, ok := detailedReset(individual, now); ok {
			balance.ResetsAt = &reset
		}
		addBalance(q, "spend_control", balance)
		base := object(root, "rate_limit", "rateLimit", "rate_limits", "rateLimits")
		if base == nil {
			base = root
		}
		codexGroup(q, "regular", base, now, "")
		codexGroup(q, "code_review", object(root, "code_review_rate_limit", "code_review_rate_limits", "codeReviewRateLimit", "codeReviewRateLimits"), now, "")
		additional := root["additional_rate_limits"]
		if additional == nil {
			additional = root["additionalRateLimits"]
		}
		add := func(id string, v map[string]any) {
			if !detailName.MatchString(id) {
				return
			}
			g := object(v, "rate_limit", "rateLimit")
			if g == nil {
				g = v
			}
			codexGroup(q, "additional/"+id, g, now, name(v, "metered_feature", "meteredFeature"))
		}
		switch values := additional.(type) {
		case []any:
			for i, v := range values {
				if i >= maxDetails {
					q.Truncated = true
					break
				}
				if o, ok := v.(map[string]any); ok {
					add(name(o, "limit_name", "limitName", "name"), o)
				}
			}
		case map[string]any:
			keys := make([]string, 0, len(values))
			for k := range values {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for i, k := range keys {
				if i >= maxDetails {
					q.Truncated = true
					break
				}
				if o, ok := values[k].(map[string]any); ok {
					add(k, o)
				}
			}
		}
		credits := object(root, "credits")
		addBalance(q, "credits", client.Balance{Unit: "credits", Remaining: decimal(credits, "balance"), HasCredits: boolean(credits, "has_credits", "hasCredits"), Unlimited: boolean(credits, "unlimited")})
	case "xai":
		cfg := object(root, "config")
		q.Plan = name(root, "subscriptionTier", "subscription_tier")
		q.UnifiedBilling = boolean(cfg, "isUnifiedBillingUser", "is_unified_billing_user")
		period := object(cfg, "currentPeriod", "current_period")
		w := client.Window{UsedPercent: percent(cfg, "creditUsagePercent", "credit_usage_percent"), StartsAt: timestamp(period, "start"), ResetsAt: timestamp(period, "end"), Period: name(period, "type")}
		if w.StartsAt == nil {
			w.StartsAt = timestamp(cfg, "billingPeriodStart", "billing_period_start")
		}
		if w.ResetsAt == nil {
			w.ResetsAt = timestamp(cfg, "billingPeriodEnd", "billing_period_end")
		}
		addWindow(q, "shared", w)
		addBalance(q, "included", client.Balance{Unit: "usd_cents", Used: cents(cfg, "used"), Limit: cents(cfg, "monthlyLimit", "monthly_limit")})
		addBalance(q, "prepaid", client.Balance{Unit: "usd_cents", Remaining: cents(cfg, "prepaidBalance", "prepaid_balance")})
		addBalance(q, "on_demand", client.Balance{Unit: "usd_cents", Used: cents(cfg, "onDemandUsed", "on_demand_used"), Limit: cents(cfg, "onDemandCap", "on_demand_cap"), Enabled: boolean(root, "onDemandEnabled", "on_demand_enabled")})
		products, ok := cfg["productUsage"].([]any)
		if !ok {
			products, _ = cfg["product_usage"].([]any)
		}
		for i, v := range products {
			if i >= maxDetails {
				q.Truncated = true
				break
			}
			if o, ok := v.(map[string]any); ok {
				if id := name(o, "product"); id != "" {
					addWindow(q, "product/"+id, client.Window{UsedPercent: percent(o, "usagePercent", "usage_percent"), StartsAt: w.StartsAt, ResetsAt: w.ResetsAt, Period: w.Period})
				}
			}
		}
	}
	return q
}
func codexGroup(q *client.Quota, id string, g map[string]any, now time.Time, feature string) {
	if g == nil {
		return
	}
	l := client.Limit{MeteredFeature: feature, Allowed: boolean(g, "allowed"), Reached: boolean(g, "limit_reached", "limitReached")}
	if l.Allowed != nil || l.Reached != nil || l.MeteredFeature != "" {
		if len(q.Limits) < maxDetails {
			q.Limits[id] = l
		} else {
			q.Truncated = true
		}
	}
	for _, slot := range []string{"primary", "secondary"} {
		w := object(g, slot+"_window", slot+"Window", slot)
		if w == nil {
			continue
		}
		item := client.Window{UsedPercent: percent(w, "used_percent", "usedPercent")}
		if d, ok := integerField(w, "limit_window_seconds", "limitWindowSeconds"); ok && d > 0 {
			item.DurationSeconds = &d
		} else if m, ok := integerField(w, "window_minutes", "windowMinutes"); ok && m > 0 && m <= math.MaxInt64/60 {
			d := m * 60
			item.DurationSeconds = &d
		}
		if t, ok := detailedReset(w, now); ok {
			item.ResetsAt = &t
		}
		addWindow(q, id+"/"+slot, item)
	}
}

// Extended windows may exceed a week. Keep the legacy weekly parser's bounds
// separate and anchor relative reset values to the original observation time.
func detailedReset(w map[string]any, now time.Time) (time.Time, bool) {
	const horizon = 10 * 366 * 24 * time.Hour
	for _, key := range []string{"reset_at", "resetAt"} {
		var t time.Time
		switch v := w[key].(type) {
		case string:
			t, _ = parseRFC3339(v)
		case json.Number:
			if n, err := strconv.ParseInt(v.String(), 10, 64); err == nil {
				t = time.Unix(n, 0).UTC()
			}
		}
		if !t.IsZero() && t.After(now.Add(-horizon)) && t.Before(now.Add(horizon)) {
			return t, true
		}
	}
	if seconds, ok := numberField(w, "reset_after_seconds", "resetAfterSeconds"); ok && seconds >= 0 && seconds < horizon.Seconds() {
		return now.Add(time.Duration(seconds * float64(time.Second))), true
	}
	return time.Time{}, false
}
