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

// claudeLimits reads the structured limits[] array.
//
// Anthropic moved live quota out of the flat seven_day_* keys and into this
// array; the flat keys are still present on the response but arrive null, so a
// reader that only knows them sees a credential with a session and a weekly and
// nothing else — the model-scoped allowances, Fable among them, simply vanish.
// addWindow already drops the nulled flat keys, so both shapes can be read and
// whichever carries data wins.
//
// Each entry is self-describing: a kind, a percent, a reset instant, and for a
// scoped limit the model it applies to. Scoped entries are keyed by that model
// rather than by a fixed list, so a model Anthropic adds later appears on its
// own instead of needing a release here.
// claudeConcept names the window a flat key describes, in the same vocabulary
// claudeLimits reports. Empty for a key the structured array has no equivalent
// of, which is then always read from the flat key.
func claudeConcept(id string) string {
	switch id {
	case "five_hour":
		return "session"
	case "seven_day":
		return "weekly_all"
	case "seven_day_opus":
		return "scoped/fable"
	case "seven_day_sonnet":
		return "scoped/sonnet"
	}
	return ""
}

func claudeLimits(q *client.Quota, root map[string]any) map[string]bool {
	covered := map[string]bool{}
	items, ok := root["limits"].([]any)
	if !ok {
		return covered
	}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		// "percent" is what the array uses; "utilization" is accepted too,
		// because that is the flat keys' spelling and the two have been seen
		// to swap.
		used, resets := percent(entry, "percent", "utilization"), timestamp(entry, "resets_at", "resetsAt")
		if used == nil && resets == nil {
			continue
		}
		duration, id, concept := int64(604800), "", ""
		switch name(entry, "kind") {
		case "session":
			// No duration is declared any more, so none is claimed here.
			id, concept = limitsSession, "session"
		case "weekly_all":
			id, concept = limitsWeeklyAll, "weekly_all"
		case "weekly_scoped":
			model := name(object(object(entry, "scope"), "model"), "display_name", "displayName", "name")
			if model == "" {
				continue
			}
			id, concept = limitsWeeklyScoped+model, "scoped/"+scopeConcept(model)
		default:
			continue
		}
		window := client.Window{UsedPercent: used, ResetsAt: resets}
		if id != limitsSession {
			window.DurationSeconds = &duration
		}
		before := len(q.Windows)
		addWindow(q, id, window)
		// Only claim the concept if the window was actually stored: a value
		// addWindow rejected must not suppress the flat key that still has one.
		if len(q.Windows) > before {
			covered[concept] = true
		}
	}
	return covered
}

func parseDetails(provider string, root map[string]any, now time.Time) *client.Quota {
	q := &client.Quota{Schema: 1, ObservedAt: now, Windows: map[string]client.Window{}, Limits: map[string]client.Limit{}, Balances: map[string]client.Balance{}}
	switch provider {
	case "claude":
		// The subscription object is reported alongside usage and costs no
		// extra request. Names go through the same bounded validation as every
		// other label, so a malformed value is omitted rather than stored.
		subscription := object(root, "subscription")
		// Spelled several ways across revisions of this response, and on some
		// of them absent entirely — the caller supplies a credential-derived
		// fallback in that case rather than leaving the badge blank.
		q.Plan = name(subscription, "plan", "plan_type", "planType", "tier", "name")
		if q.Plan == "" {
			q.Plan = name(root, "subscription_type", "subscriptionType", "plan_type", "planType")
		}
		q.TierName = name(subscription, "tierName", "tier_name")
		if q.TierName == "" {
			q.TierName = name(root, "rate_limit_tier", "rateLimitTier")
		}
		// The structured array first, so the flat keys can be skipped where it
		// already covers them. An account mid-migration reports both, and the
		// two are spellings of one window rather than two windows: published
		// together they reduce to the same canonical identity, and the loser is
		// demoted to a raw: row that shows the reader the same figures twice
		// under a machine name.
		covered := claudeLimits(q, root)
		for _, id := range []string{"five_hour", "seven_day", "seven_day_oauth_apps", "seven_day_opus", "seven_day_sonnet", "seven_day_cowork"} {
			if concept := claudeConcept(id); concept != "" && covered[concept] {
				continue
			}
			w := object(root, id)
			duration := int64(604800)
			if id == "five_hour" {
				duration = 18000
			}
			addWindow(q, id, client.Window{UsedPercent: percent(w, "utilization"), ResetsAt: timestamp(w, "resets_at"), DurationSeconds: &duration})
		}
		extra := object(root, "extra_usage")
		addBalance(q, "extra_usage", client.Balance{Unit: "provider_units", Used: decimal(extra, "used_credits"), Limit: decimal(extra, "monthly_limit"), UsedPercent: percent(extra, "utilization"), Enabled: boolean(extra, "is_enabled")})
		block := object(root, "cedar_ember")
		q.ResetCredits = claudeResetGrants(q, block, now)
		q.BillingPeriod = claudeBillingPeriod(block)
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
		q.ResetCredits = resetCredits(root)
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

// resetCredits reads Codex's banked rate-limit reset count off the usage
// response that was already fetched, so knowing the count costs no request of
// its own.
//
// The key is present and null on an account that has never held one, which is
// not the same as absent: both mean nothing to spend, and both return nil so
// the dashboard says nothing at all. A negative or fractional count is a
// response this code does not understand, and is dropped rather than rendered.
//
// Expiry is deliberately not read here. It lives on a separate endpoint, and
// the caller attaches it only when the count says there is something to expire.
func resetCredits(root map[string]any) *client.ResetCredits {
	o := object(root, "rate_limit_reset_credits", "rateLimitResetCredits")
	if o == nil {
		return nil
	}
	n, ok := integerField(o, "available_count", "availableCount")
	if !ok || n <= 0 || n > maxDetails {
		return nil
	}
	return &client.ResetCredits{AvailableCount: int(n)}
}

// resetGrantID is the id shape Claude Code accepts. Ids are read only to tell
// grants apart and to honour next_grant_id; they never leave this function's
// callers, because they identify the account and have no display use.
var resetGrantID = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)

// resetGrant is one well-formed entry of the cedar_ember block, reduced to what
// counting and the hold need. The label is not read at all.
type resetGrant struct {
	id                               string
	left                             int64
	startsAt, endsAt                 time.Time // zero when not given
	paused, usableNow, requiresLimit bool
}

// flag reads an optional boolean with the default Claude Code falls back to
// when the key is missing or not a boolean.
func flag(m map[string]any, key string, fallback bool) bool {
	if v := boolean(m, key); v != nil {
		return *v
	}
	return fallback
}

// parseResetGrant applies Claude Code's own grant schema: an id and a
// resets_left are required, and every other field falls back to its default
// when it is missing or malformed. A grant failing the schema is ignored on its
// own, as Claude Code ignores it, rather than costing the rest of the block. A
// count above maxDetails is not one this code understands, and is ignored too.
func parseResetGrant(item any) (resetGrant, bool) {
	o, ok := item.(map[string]any)
	if !ok {
		return resetGrant{}, false
	}
	id, _ := o["id"].(string)
	left, ok := integerField(o, "resets_left")
	if !resetGrantID.MatchString(id) || !ok || left < 0 || left > maxDetails {
		return resetGrant{}, false
	}
	g := resetGrant{id: id, left: left, paused: flag(o, "paused", false),
		// Missing usability flags default to the refusing side, as they do in
		// Claude Code: a grant that does not say it is usable is not.
		usableNow: flag(o, "usable_now", false), requiresLimit: flag(o, "use_requires_limit", true)}
	g.startsAt, _ = rfc3339Field(o, "starts_at")
	g.endsAt, _ = rfc3339Field(o, "ends_at")
	return g, true
}

// claudeResetGrants reads Claude's banked rate-limit resets off the cedar_ember
// block the usage request already asked for.
//
// The count is the sum of resets_left over the grants that have not expired. A
// grant that has not started yet still counts: it is held, just not spendable,
// and the hold below is where "not now" is said. The block is optional and its
// shape is not a stable API, so anything wrong with it yields nil — the
// dashboard says nothing — and never a failed poll.
//
// Ids and labels are read and discarded. What survives is a count, a date, and
// the provider's reason none can be spent now, if it gives one.
func claudeResetGrants(q *client.Quota, block map[string]any, now time.Time) *client.ResetCredits {
	eligible := boolean(block, "eligible")
	if eligible == nil {
		return nil
	}
	items, _ := block["grants"].([]any)
	live := make([]resetGrant, 0, len(items))
	seen := map[string]bool{}
	total := 0
	for i, item := range items {
		if i >= maxDetails {
			q.Truncated = true
			break
		}
		g, ok := parseResetGrant(item)
		// A repeated id is one grant reported twice; counting it twice would
		// promise a reset the account does not have.
		if !ok || seen[g.id] {
			continue
		}
		seen[g.id] = true
		if g.left == 0 || (!g.endsAt.IsZero() && !g.endsAt.After(now)) {
			continue
		}
		live = append(live, g)
		total += int(g.left)
	}
	if total == 0 {
		return nil
	}
	credits := &client.ResetCredits{AvailableCount: total}
	for _, g := range live {
		if !g.endsAt.IsZero() && (credits.SoonestExpiry == nil || g.endsAt.Before(*credits.SoonestExpiry)) {
			at := g.endsAt
			credits.SoonestExpiry = &at
		}
	}
	next, _ := block["next_grant_id"].(string)
	credits.Hold, credits.HoldUntil = resetGrantHold(*eligible, flag(block, "at_limit", false), block, live, next, now)
	return credits
}

// claudeBillingPeriod reads how often the subscription is billed from the
// cedar_ember block's event_props, the one field of them kept.
//
// Anthropic reports no renewal date for a Claude subscription. The profile's
// subscription start, read on the account-details schedule, says when the
// billing anchor is; this says whether it comes round monthly or yearly, and
// costs nothing because it rides on the usage request already made.
//
// event_props is analytics context, not a contract, so it is read defensively
// and in isolation: independent of eligibility and of the grants, and
// incapable of touching either. Only the two cadences a reader can schedule
// are kept; "unknown", a value this code was not built for, a non-string, a
// null or missing event_props all store nothing, which a reader takes as
// "not stated". Every other event_props field — the tier, the surface — is
// left where it was found.
func claudeBillingPeriod(block map[string]any) string {
	switch period := stringField(object(block, "event_props"), "billing_period"); period {
	case client.BillingMonthly, client.BillingAnnual:
		return period
	}
	return ""
}

// resetGrantHold is the provider's reason none of the live grants can be spent
// now, judged by the rules the claim endpoint applies, or "" when one can be.
//
// Account-wide refusals come first because no choice of grant gets past them.
// Then, if any grant clears every blocker, there is nothing to report: a
// redemption would select it (next_grant_id when that one is usable, otherwise
// the soonest to expire). Only when none does is a reason worth printing, and it
// is read off the grant a redemption would reach for first — so "paused" covers
// both the server's preferred grant being paused and every grant being paused.
// A grant refused for a reason outside this vocabulary, such as usable_now
// false with no stated cause, reports "": the provider has not said why.
func resetGrantHold(eligible, atLimit bool, block map[string]any, live []resetGrant, next string, now time.Time) (string, *time.Time) {
	if !eligible {
		return client.HoldIneligible, nil
	}
	if until, ok := rfc3339Field(block, "cooldown_until"); ok && until.After(now) {
		return client.HoldCooldown, &until
	}
	for _, g := range live {
		started := g.startsAt.IsZero() || !g.startsAt.After(now)
		if !g.paused && g.usableNow && started && (!g.requiresLimit || atLimit) {
			return "", nil
		}
	}
	first := preferredResetGrant(live, next)
	switch {
	case first.paused:
		return client.HoldPaused, nil
	case first.requiresLimit && !atLimit:
		return client.HoldNotLimited, nil
	}
	return "", nil
}

// preferredResetGrant is the grant a redemption tries first: the one the server
// names in next_grant_id, else the soonest to expire, an undated grant last,
// ties broken by id so the answer does not depend on response order. live is
// never empty here; the caller has already returned for a zero count.
func preferredResetGrant(live []resetGrant, next string) resetGrant {
	for _, g := range live {
		if g.id == next {
			return g
		}
	}
	ordered := append([]resetGrant(nil), live...)
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.endsAt.IsZero() != b.endsAt.IsZero() {
			return !a.endsAt.IsZero()
		}
		if !a.endsAt.Equal(b.endsAt) {
			return a.endsAt.Before(b.endsAt)
		}
		return a.id < b.id
	})
	return ordered[0]
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
