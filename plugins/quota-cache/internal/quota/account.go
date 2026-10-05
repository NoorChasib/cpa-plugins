package quota

import (
	"context"
	"encoding/json"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

// Account details are the facts about a credential that change rarely and that
// the usage response does not carry, or carries only in a weaker spelling:
// Claude's plan, Codex's subscription renewal, Grok's plan name. Each costs a
// request of its own, so they keep a slow schedule of their own — at most one
// read per credential every DetailsInterval — and the last good answer is
// carried between reads and across failed requests. Nothing here can fail a
// poll, back it off, or pause its provider: every request that goes wrong
// leaves the previous value standing and the observation as it was. An
// endpoint that answers but no longer names the value is different: that is
// the account saying it has none now — a lapsed plan, a subscription with no
// renewal date — and the carried value is dropped so the weaker sources apply
// again, rather than showing a plan the account stopped having.
//
// The same endpoints the CPA management centre reads for its own cards:
//
//	Claude: GET https://api.anthropic.com/api/oauth/profile
//	        -> organization.organization_type, .subscription_status,
//	           .rate_limit_tier; account.has_claude_max, .has_claude_pro
//	Codex:  GET https://chatgpt.com/backend-api/subscriptions?account_id=<id>
//	        -> active_until (unix seconds, as a number or a string)
//	Grok:   GET https://cli-chat-proxy.grok.com/v1/settings
//	        -> subscription_tier_display ("SuperGrok Heavy"), else
//	        GET https://cli-chat-proxy.grok.com/v1/user?include=subscription
//	        -> subscriptionTier
const (
	// DetailsInterval spaces the account-detail reads for one credential. A
	// plan or a renewal date changes about once a month; four reads a day
	// notices that within hours, for four requests against a credential the
	// usage poll already asks ninety-six times.
	DetailsInterval = 6 * time.Hour

	claudeProfileURL      = "https://api.anthropic.com/api/oauth/profile"
	codexSubscriptionsURL = "https://chatgpt.com/backend-api/subscriptions"
	xaiSettingsURL        = "https://cli-chat-proxy.grok.com/v1/settings"
	xaiUserURL            = "https://cli-chat-proxy.grok.com/v1/user?include=subscription"
	// The Grok CLI's own identification. These account endpoints sit behind
	// the CLI's edge, and the CPA management centre reads them with exactly
	// this set, so it is copied rather than improvised.
	xaiClientVersion = "0.2.91"
	xaiCLIUserAgent  = "grok-pager/0.2.91 grok-shell/0.2.91 (macos; aarch64)"

	// accountHorizon bounds any date read from an account endpoint, the same
	// ten years detailedReset allows. Further out is a misread unit, not a date.
	accountHorizon = 10 * 366 * 24 * time.Hour
)

// hasAccountDetails reports whether the provider has account endpoints this
// package reads. OpenRouter has none: its one request is its whole observation.
func hasAccountDetails(provider string) bool {
	switch provider {
	case "claude", "codex", "xai":
		return true
	}
	return false
}

// detailsDue reports whether the account endpoints should be asked now. A check
// dated in the future is a clock that moved backwards rather than a read that
// just happened, and trusting it would suppress reads until the clock caught up.
func detailsDue(known *client.AccountDetails, now time.Time) bool {
	return known == nil || known.CheckedAt.IsZero() || known.CheckedAt.After(now) ||
		now.Sub(known.CheckedAt) >= DetailsInterval
}

// RefreshDetails returns the account details to keep after a successful poll.
//
// When known was checked within DetailsInterval it is returned as it is, and no
// request is made. Otherwise the account endpoints are asked and what they
// answer replaces known: a value they answered without — no plan named, no
// renewal still ahead — is cleared, and only a value whose request failed
// (transport error, non-2xx status, unreadable body) keeps the one known
// already had. CheckedAt advances either way, so a failing endpoint is asked
// again in six hours, not on every poll.
//
// Call it only once the usage observation is a success, and with a doer whose
// statuses are not the poll's: a 429 from an account endpoint says nothing
// about the usage endpoint and must not pause it.
func RefreshDetails(ctx context.Context, doer Doer, provider string, rawAuth []byte, known *client.AccountDetails, now time.Time) *client.AccountDetails {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if !hasAccountDetails(provider) || !detailsDue(known, now) {
		return known
	}
	creds, err := extractCredentials(provider, rawAuth)
	if err != nil || (provider == "codex" && creds.accountID == "") {
		return known
	}
	next := &client.AccountDetails{CheckedAt: now}
	if known != nil {
		next.Plan, next.RenewalAt = known.Plan, known.RenewalAt
	}
	switch provider {
	case "claude":
		if plan, answered := claudeProfilePlan(ctx, doer, creds); answered {
			next.Plan = plan
		}
	case "codex":
		if at, answered := codexSubscriptionEnd(ctx, doer, creds, now); answered {
			next.RenewalAt = nil
			if !at.IsZero() {
				next.RenewalAt = &at
			}
		}
	case "xai":
		if plan, answered := xaiPlanName(ctx, doer, creds); answered {
			next.Plan = plan
		}
	}
	return next
}

// ApplyDetails lays the account details over the plan and renewal the usage
// response produced, which they outrank: they come from the provider's account
// endpoints, where the usage response's spellings are weaker or missing. The
// details are kept on the observation so the caller can persist them.
//
// The plan is validated again on the way out, as every label is, because a
// stored value has been through a file since it was first checked. The renewal
// outranks the others only while it is still ahead of now: details are read
// every six hours, and a date that has passed since is the subscription having
// renewed, which the id_token or the spend-control reset may already show.
// Laid over them, it would hide a good date behind one the dashboard drops.
func (o *Observation) ApplyDetails(details *client.AccountDetails, now time.Time) {
	o.AccountDetails = details
	if details == nil {
		return
	}
	if plan := planName(details.Plan); plan != "" {
		o.Plan = plan
	}
	if details.RenewalAt != nil && details.RenewalAt.After(now) {
		o.RenewalAt = details.RenewalAt.UTC()
	}
}

// readAccount performs one account-endpoint GET and returns its JSON object.
// Every failure is just false: the caller keeps what it had.
func readAccount(ctx context.Context, doer Doer, request protocol.HostHTTPRequest) (map[string]any, bool) {
	response, err := doer.HTTPDo(ctx, request)
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 || len(response.Body) > maxResponseBytes {
		return nil, false
	}
	root, err := decodeObject(response.Body)
	return root, err == nil
}

// claudeProfilePlan reads the account's plan from Claude's profile endpoint.
// answered is false only when the request failed; a profile that answered
// without naming a plan is ("", true).
func claudeProfilePlan(ctx context.Context, doer Doer, creds credentials) (plan string, answered bool) {
	root, ok := readAccount(ctx, doer, protocol.HostHTTPRequest{
		Method: "GET",
		URL:    claudeProfileURL,
		Headers: map[string][]string{
			"Authorization":  {"Bearer " + creds.accessToken},
			"Accept":         {"application/json"},
			"anthropic-beta": {claudeOAuthBeta},
			// The same bucket as the usage request: anything that does not
			// identify as Claude Code is rate-limited far harder.
			"User-Agent": {claudeUserAgent},
		},
	})
	if !ok {
		return "", false
	}
	return claudePlanToken(object(root, "organization"), object(root, "account")), true
}

// claudePlanToken names the plan with a fixed machine token for consumers to
// label: "team", "enterprise", "max_20x", "max_5x", "max", "pro" or "free", or
// "" when the profile does not say.
//
// The organisation type is what Claude Code itself reads, and its rate-limit
// tier is where the Max multiplier lives. A Team or Enterprise organisation
// names the seat only while its subscription is current: like the CPA
// management centre, a lapsed one falls through to the account's own flags,
// because a member of a lapsed Team can still hold a personal Max. A missing
// status is taken as current, as Claude Code takes it. Both flags explicitly
// false is a free account; flags that are simply absent are not evidence of it.
func claudePlanToken(organization, account map[string]any) string {
	kind := strings.ToLower(stringField(organization, "organization_type"))
	status := strings.ToLower(stringField(organization, "subscription_status"))
	tier := strings.ToLower(stringField(organization, "rate_limit_tier"))
	hasMax, hasPro := boolean(account, "has_claude_max"), boolean(account, "has_claude_pro")
	current := status == "" || status == "active"
	switch {
	case kind == "claude_team" && current:
		return "team"
	case kind == "claude_enterprise" && current:
		return "enterprise"
	case kind == "claude_max" || (hasMax != nil && *hasMax) || strings.Contains(tier, "claude_max"):
		switch {
		case strings.Contains(tier, "max_20x"):
			return "max_20x"
		case strings.Contains(tier, "max_5x"):
			return "max_5x"
		}
		return "max"
	case kind == "claude_pro" || (hasPro != nil && *hasPro):
		return "pro"
	case hasMax != nil && hasPro != nil:
		return "free"
	}
	return ""
}

// codexSubscriptionEnd reads when the ChatGPT subscription is paid up to: the
// "Renewal time" the CPA management centre shows. The request is account-scoped
// twice over, by query and by header, as the CLI-shaped usage request is.
//
// answered is false only when the request failed. A date already behind us is
// answered as no date: it is a subscription that has lapsed or a read taken
// before the renewal landed, and either way it must not outrank a fallback
// that is still ahead.
func codexSubscriptionEnd(ctx context.Context, doer Doer, creds credentials, now time.Time) (at time.Time, answered bool) {
	root, ok := readAccount(ctx, doer, protocol.HostHTTPRequest{
		Method: "GET",
		URL:    codexSubscriptionsURL + "?account_id=" + url.QueryEscape(creds.accountID),
		Headers: map[string][]string{
			"Authorization":      {"Bearer " + creds.accessToken},
			"Accept":             {"application/json"},
			"User-Agent":         {codexUserAgent},
			"Chatgpt-Account-Id": {creds.accountID},
		},
	})
	if !ok {
		return time.Time{}, false
	}
	for _, key := range []string{"active_until", "activeUntil"} {
		if at, ok := instant(root[key]); ok && futureInstant(at, now) {
			return at, true
		}
	}
	return time.Time{}, true
}

// xaiPlanName reads Grok's plan as the subscription's display name, which is
// the name Grok's own clients print ("SuperGrok Heavy"). The user endpoint's
// tier is asked only when settings did not supply a name, so a healthy account
// costs one request per read rather than two.
//
// answered is true when a plan was named, or when both endpoints answered and
// neither named one. If either request failed, its silence proves nothing.
func xaiPlanName(ctx context.Context, doer Doer, creds credentials) (plan string, answered bool) {
	headers := map[string][]string{
		"Authorization":         {"Bearer " + creds.accessToken},
		"Accept":                {"*/*"},
		"X-XAI-Token-Auth":      {xaiTokenAuthHeader},
		"X-Grok-Client-Version": {xaiClientVersion},
		"User-Agent":            {xaiCLIUserAgent},
	}
	settings, settingsAnswered := readAccount(ctx, doer, protocol.HostHTTPRequest{Method: "GET", URL: xaiSettingsURL, Headers: headers})
	if settingsAnswered {
		if plan := name(settings, "subscription_tier_display", "subscriptionTierDisplay"); plan != "" {
			return plan, true
		}
	}
	user, userAnswered := readAccount(ctx, doer, protocol.HostHTTPRequest{Method: "GET", URL: xaiUserURL, Headers: headers})
	if !userAnswered {
		return "", false
	}
	plan = name(user, "subscriptionTier", "subscription_tier")
	return plan, plan != "" || settingsAnswered
}

// instant reads a date that account endpoints spell three ways: RFC 3339, or
// unix seconds as a number or a numeric string. Milliseconds are recognised by
// magnitude, as the CPA management centre recognises them — seconds stay below
// 1e11 for another three thousand years. Zero or below is a provider's "none",
// not 1970.
func instant(value any) (time.Time, bool) {
	var n float64
	switch v := value.(type) {
	case string:
		s := strings.TrimSpace(v)
		if t, ok := parseRFC3339(s); ok {
			return t, true
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return time.Time{}, false
		}
		n = f
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return time.Time{}, false
		}
		n = f
	case float64:
		n = v
	default:
		return time.Time{}, false
	}
	if math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 {
		return time.Time{}, false
	}
	if n >= 1e11 {
		n /= 1000
	}
	if n >= 1e11 {
		return time.Time{}, false
	}
	return time.Unix(int64(n), 0).UTC(), true
}

// plausibleInstant rejects a zero date and one implausibly far from now.
func plausibleInstant(at, now time.Time) bool {
	return !at.IsZero() && at.After(now.Add(-accountHorizon)) && at.Before(now.Add(accountHorizon))
}

// futureInstant is a plausible date still ahead of now: the only kind of
// renewal date allowed to outrank another source. Ten years back is plausible
// for a date, but a renewal that has passed is not the next one.
func futureInstant(at, now time.Time) bool {
	return plausibleInstant(at, now) && at.After(now)
}
