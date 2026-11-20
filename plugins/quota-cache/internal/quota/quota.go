// Package quota reads each OAuth account's quota windows and balances
// directly from the provider usage endpoint that the provider's own CLI uses.
//
// Only the credential fields required to authenticate one usage request are
// decoded from the physical auth JSON. Responses are projected into a regular
// weekly/pool compatibility observation and bounded extended quota fields.
// Tokens, raw response bodies,
// and provider error text are never logged, persisted, rendered, or copied
// into notifications: every error returned here is a static string.
//
// Verified against live accounts on 2026-09-04:
//
//	Claude: GET https://api.anthropic.com/api/oauth/usage
//	        -> seven_day.utilization (0..100), seven_day.resets_at (RFC3339)
//	        Anthropic is migrating this response: the flat five_hour /
//	        seven_day / seven_day_* keys are being nulled in favour of a
//	        structured limits[] array whose entries carry kind (session /
//	        weekly_all / weekly_scoped), percent, resets_at, and for a scoped
//	        entry scope.model.display_name. Both shapes are read, and the
//	        request must carry a claude-code User-Agent or the endpoint
//	        rate-limits it hard. The cedar_ember=1 query adds the banked
//	        reset grants as a cedar_ember block beside everything else.
//	Codex:  GET https://chatgpt.com/backend-api/wham/usage
//	        -> rate_limit.<window with limit_window_seconds=604800>.used_percent,
//	           reset_at (unix seconds) or reset_after_seconds
//	Grok:   GET https://cli-chat-proxy.grok.com/v1/billing?format=credits
//	        -> config.creditUsagePercent (0..100), config.currentPeriod.end
package quota

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

const (
	// claudeUsageURL asks for the reset-grant block with the usage it always
	// read, so knowing an account's banked resets costs no request of its own.
	// Claude Code sends skip_spend=1 beside cedar_ember=1 on this path; it is
	// deliberately left off here, because it skips the spend-store read that
	// fills extra_usage, and extra_usage is collected from this same response.
	claudeUsageURL  = "https://api.anthropic.com/api/oauth/usage?cedar_ember=1"
	claudeOAuthBeta = "oauth-2025-04-20"

	codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"
	// codexResetCreditsURL enumerates the account's banked rate-limit resets.
	// The usage response above already carries the count, so this is read only
	// to date the credits: a banked reset lapses thirty days after it is
	// granted, and a count that cannot say "one expires Friday" is how they are
	// lost. It is requested only when the count is non-zero, so an account with
	// none banked — the common case — costs exactly the one usage request it
	// always did.
	codexResetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	// codexUserAgent is CLI-shaped because the ChatGPT backend edge rejects
	// non-CLI clients on this endpoint. The comment segment identifies the
	// real caller.
	codexUserAgent = "codex_cli_rs/0.0.0 (cpa-plugins/quota-cache)"
	// Anthropic buckets this endpoint by User-Agent: a caller that does not
	// identify as Claude Code lands in a far tighter bucket and starts
	// collecting 429s after a handful of polls. Sending it is the difference
	// between a credential that reports and one that sits in backoff.
	//
	// It is Claude Code's own form, verbatim, with no caller segment, because
	// the reset-grant program also judges eligibility by client: its
	// ineligible_reason vocabulary includes "surface" and "cli_version". This is
	// the exact string CPA's management UI sends on the same three endpoints
	// (Cli-Proxy-API-Management-Center d554bb0, the commit that added reset
	// grants), which is the only form observed to return grants.
	claudeUserAgent = "claude-cli/2.1.280 (external, cli)"

	xaiBillingURL = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"
	// xaiTokenAuthHeader mirrors the open-source Grok Build CLI, which sends
	// this constant with every grok.com billing request.
	xaiTokenAuthHeader = "xai-grok-cli"

	weeklyWindowSeconds = 604800
	weeklyWindowMinutes = 10080

	// maxResponseBytes bounds the decoded usage body; real responses are a few
	// kilobytes.
	maxResponseBytes = 1 << 20
)

var (
	ErrUnsupportedProvider = errors.New("quota polling is not supported for this provider")
	ErrNoAccessToken       = errors.New("auth document has no access token")
	ErrNoAccountID         = errors.New("codex account id could not be resolved")
	ErrInvalidResponse     = errors.New("usage endpoint returned an unreadable response")
	ErrNoWeeklyWindow      = errors.New("usage endpoint did not report a weekly window")
)

// HTTPStatusError reports a non-2xx usage endpoint status. The body is never
// retained.
type HTTPStatusError struct {
	StatusCode int
}

func (e HTTPStatusError) Error() string {
	return fmt.Sprintf("usage endpoint returned HTTP %d", e.StatusCode)
}

// Doer abstracts host.http.do.
type Doer interface {
	HTTPDo(context.Context, protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error)
}

// Observation is one account's regular weekly window as reported by its
// provider. Percent is used capacity from 0 to 100 (values above 100 are
// clamped). ResetAt is zero when the provider omitted a usable reset instant.
type Observation struct {
	Quota      *client.Quota
	Provider   string
	Percent    float64
	ResetAt    time.Time
	ObservedAt time.Time

	// Windows is the same response projected into the canonical vocabulary.
	// Percent/ResetAt above remain the regular weekly window regardless.
	Windows   []client.EntryWindow
	Plan      string
	TierName  string
	RenewalAt time.Time

	// AccountDetails is set by ApplyDetails, never by Fetch: the slow account
	// endpoints are read on their own schedule, not on every poll.
	AccountDetails *client.AccountDetails
}

// Supported reports whether the provider has a usage endpoint this package
// can read.
func Supported(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude", "codex", "xai":
		return true
	}
	return false
}

// Fetch reads quota fields for one credential. rawAuth is the physical
// auth JSON from host.auth.get; only the token fields are extracted.
func Fetch(ctx context.Context, doer Doer, provider string, rawAuth []byte, now time.Time) (Observation, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if !Supported(provider) {
		return Observation{}, ErrUnsupportedProvider
	}
	creds, err := extractCredentials(provider, rawAuth)
	if err != nil {
		return Observation{}, err
	}
	var request protocol.HostHTTPRequest
	switch provider {
	case "claude":
		request = protocol.HostHTTPRequest{
			Method: "GET",
			URL:    claudeUsageURL,
			Headers: map[string][]string{
				"Authorization":  {"Bearer " + creds.accessToken},
				"Accept":         {"application/json"},
				"anthropic-beta": {claudeOAuthBeta},
				"User-Agent":     {claudeUserAgent},
			},
		}
	case "codex":
		if creds.accountID == "" {
			return Observation{}, ErrNoAccountID
		}
		request = protocol.HostHTTPRequest{
			Method: "GET",
			URL:    codexUsageURL,
			Headers: map[string][]string{
				"Authorization":      {"Bearer " + creds.accessToken},
				"Accept":             {"application/json"},
				"User-Agent":         {codexUserAgent},
				"Chatgpt-Account-Id": {creds.accountID},
			},
		}
	case "xai":
		headers := map[string][]string{
			"Authorization":    {"Bearer " + creds.accessToken},
			"Accept":           {"application/json"},
			"X-XAI-Token-Auth": {xaiTokenAuthHeader},
		}
		if creds.userID != "" {
			headers["x-userid"] = []string{creds.userID}
		}
		request = protocol.HostHTTPRequest{Method: "GET", URL: xaiBillingURL, Headers: headers}
	}
	response, err := doer.HTTPDo(ctx, request)
	if err != nil {
		return Observation{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Observation{}, HTTPStatusError{StatusCode: response.StatusCode}
	}
	if len(response.Body) > maxResponseBytes {
		return Observation{}, ErrInvalidResponse
	}
	root, err := decodeObject(response.Body)
	if err != nil {
		return Observation{}, err
	}
	observation, primaryErr := parsePrimary(provider, root, now)
	details := parseDetails(provider, root, now)
	if primaryErr != nil && !errors.Is(primaryErr, ErrNoWeeklyWindow) {
		return Observation{}, primaryErr
	}
	if primaryErr != nil && len(details.Windows) == 0 && len(details.Limits) == 0 && len(details.Balances) == 0 {
		return Observation{}, primaryErr
	}
	observation.Provider = provider
	// Old readers must not mistake a short-window-only account for 0% weekly use.
	if primaryErr == nil {
		observation.ObservedAt = now
	}
	observation.Quota = details
	// Dating the banked resets is a second request, so it is made only when the
	// first one said there is something to date. A failure here is not a failed
	// observation: the count is already known and everything else in this
	// response is good, so the expiry is simply left unknown. Nor is a 429 here
	// the poll's: the caller judges the poll by the usage request, the first
	// one made through doer, and never by this one.
	if provider == "codex" && details != nil && details.ResetCredits != nil {
		if expiry, ok := codexResetCreditExpiry(ctx, doer, creds, now); ok {
			details.ResetCredits.SoonestExpiry = &expiry
		}
	}
	// Derived after ObservedAt is set: a weekly window is only synthesized when
	// the primary observation actually succeeded.
	observation.Windows = canonicalWindows(provider, details, observation)
	observation.Plan, observation.TierName, observation.RenewalAt = identity(provider, details)
	// Last resort, so a tier the usage response stopped reporting still reaches
	// the dashboard as a name rather than an empty badge.
	if observation.Plan == "" {
		observation.Plan = planName(creds.plan)
	}
	// The subscription's own paid-up date outranks the spend-control reset,
	// which is a renewal only by inference — but only while it is ahead. The
	// claim is as old as the id_token, which CPA refreshes about a day before
	// it expires, so it can still name the period before the last renewal; a
	// date behind us must not replace a fallback that is still in front. The
	// account endpoint's figure outranks both, on the same terms, and is laid
	// over this by ApplyDetails.
	if futureInstant(creds.subscriptionEnd, now) {
		observation.RenewalAt = creds.subscriptionEnd
	}
	return observation, nil
}

type credentials struct {
	accessToken string
	accountID   string // Codex ChatGPT account ID
	userID      string // xAI OpenID subject
	// plan is the subscription tier as the stored credential records it. It is
	// the fallback for a usage response that no longer carries one, and costs
	// nothing: the credential has already been read.
	plan string
	// subscriptionEnd is the Codex id_token's chatgpt_subscription_active_until
	// claim, unchecked against the clock. Free for the same reason as plan, and
	// as old as the token, which is why the subscription endpoint outranks it.
	subscriptionEnd time.Time
}

// extractCredentials decodes the minimum fields for one usage request.
// Supported shapes: top-level access_token/account_id/chatgpt_account_id/sub,
// a nested Codex CLI-style "tokens" object, and (Codex only) the ChatGPT
// account ID claim inside the unverified id_token payload, which is used
// solely as a routing header.
// planName validates a credential-derived tier the same way every other label
// is validated, so a malformed stored value is dropped rather than displayed.
func planName(value string) string {
	value = strings.TrimSpace(value)
	if !detailName.MatchString(value) {
		return ""
	}
	return value
}

func extractCredentials(provider string, rawAuth []byte) (credentials, error) {
	var root map[string]any
	if err := json.Unmarshal(rawAuth, &root); err != nil || root == nil {
		return credentials{}, ErrInvalidResponse
	}
	access := stringField(root, "access_token")
	account := firstStringField(root, "account_id", "chatgpt_account_id")
	idToken := stringField(root, "id_token")
	if nested, ok := root["tokens"].(map[string]any); ok {
		if access == "" {
			access = stringField(nested, "access_token")
		}
		if account == "" {
			account = firstStringField(nested, "account_id", "chatgpt_account_id")
		}
		if idToken == "" {
			idToken = stringField(nested, "id_token")
		}
	}
	if access == "" {
		return credentials{}, ErrNoAccessToken
	}
	creds := credentials{accessToken: access}
	creds.plan = firstStringField(root, "subscription_type", "subscriptionType", "plan", "plan_type", "planType")
	if creds.plan == "" {
		if oauth, ok := root["claudeAiOauth"].(map[string]any); ok {
			creds.plan = firstStringField(oauth, "subscription_type", "subscriptionType")
		}
	}
	switch provider {
	case "codex":
		claims := idTokenClaims(idToken)
		if account == "" {
			account = accountIDFromClaims(claims)
		}
		creds.accountID = account
		creds.subscriptionEnd = subscriptionEndFromClaims(claims)
	case "xai":
		creds.userID = stringField(root, "sub")
	}
	return creds, nil
}

// idTokenClaims decodes the unverified id_token payload, or returns nil. Its
// claims are used only as routing hints and display values, never to decide
// anything about authority.
func idTokenClaims(idToken string) map[string]any {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return nil
	}
	return claims
}

func accountIDFromClaims(claims map[string]any) string {
	if id := stringField(claims, "https://api.openai.com/auth.chatgpt_account_id"); id != "" {
		return id
	}
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if id := stringField(auth, "chatgpt_account_id"); id != "" {
			return id
		}
	}
	return stringField(claims, "chatgpt_account_id")
}

// subscriptionEndFromClaims reads chatgpt_subscription_active_until, which
// OpenAI nests under its auth namespace claim like the account id beside it.
func subscriptionEndFromClaims(claims map[string]any) time.Time {
	for _, scope := range []map[string]any{object(claims, "https://api.openai.com/auth"), claims} {
		for _, key := range []string{"chatgpt_subscription_active_until", "chatgptSubscriptionActiveUntil"} {
			if at, ok := instant(scope[key]); ok {
				return at
			}
		}
	}
	return time.Time{}
}

// codexResetCreditExpiry returns the soonest expiry among the credits that are
// still available to spend.
//
// Only entries whose status is exactly "available" are considered: a redeemed
// or expired credit is in the list and has a date, and letting one of those set
// the headline would announce a deadline that has already passed. An expiry
// already behind us is dropped for the same reason.
//
// Every failure path returns false rather than an error. The caller has a
// perfectly good observation in hand and this is an ornament on it; a provider
// that renames a field here must cost the expiry line, never the poll.
func codexResetCreditExpiry(ctx context.Context, doer Doer, creds credentials, now time.Time) (time.Time, bool) {
	response, err := doer.HTTPDo(ctx, protocol.HostHTTPRequest{
		Method: "GET",
		URL:    codexResetCreditsURL,
		Headers: map[string][]string{
			"Authorization":      {"Bearer " + creds.accessToken},
			"Accept":             {"application/json"},
			"User-Agent":         {codexUserAgent},
			"Chatgpt-Account-Id": {creds.accountID},
		},
	})
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 || len(response.Body) > maxResponseBytes {
		return time.Time{}, false
	}
	root, err := decodeObject(response.Body)
	if err != nil {
		return time.Time{}, false
	}
	list, _ := root["credits"].([]any)
	var soonest time.Time
	for i, item := range list {
		if i >= maxDetails {
			break
		}
		credit, ok := item.(map[string]any)
		if !ok || stringField(credit, "status") != "available" {
			continue
		}
		at, ok := rfc3339Field(credit, "expires_at", "expiresAt")
		if !ok || !at.After(now) {
			continue
		}
		if soonest.IsZero() || at.Before(soonest) {
			soonest = at
		}
	}
	return soonest, !soonest.IsZero()
}

func parsePrimary(provider string, root map[string]any, now time.Time) (Observation, error) {
	switch provider {
	case "claude":
		return parseClaude(root)
	case "codex":
		return parseCodex(root, now)
	case "xai":
		return parseXAI(root)
	}
	return Observation{}, ErrUnsupportedProvider
}

func decodeObject(body []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil || root == nil {
		return nil, ErrInvalidResponse
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, ErrInvalidResponse
	}
	return root, nil
}

// parseClaude reads the account-wide weekly allowance. Model-scoped windows
// and short windows are separate in the extended observation.
//
// Two shapes, because Anthropic is moving between them: the flat seven_day key,
// and the weekly_all entry of the structured limits[] array that replaces it.
// The flat key is tried first and is still authoritative where it carries data;
// the array is what keeps this projection alive once it does not. Without the
// fallback a nulled seven_day fails the primary parse outright, which leaves
// the credential with no observation time at all — and a credential that has
// never been observed is excluded from every row downstream, so the dashboard
// would not show it as stale, it would show nothing.
func parseClaude(root map[string]any) (Observation, error) {
	if window, ok := root["seven_day"].(map[string]any); ok {
		if percent, ok := numberField(window, "utilization"); ok {
			observation := Observation{Percent: clampPercent(percent)}
			if reset, ok := rfc3339Field(window, "resets_at"); ok {
				observation.ResetAt = reset
			}
			return observation, nil
		}
	}
	items, _ := root["limits"].([]any)
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := entry["kind"].(string); kind != "weekly_all" {
			continue
		}
		percent, ok := numberField(entry, "percent")
		if !ok {
			percent, ok = numberField(entry, "utilization")
		}
		if !ok {
			continue
		}
		observation := Observation{Percent: clampPercent(percent)}
		if reset, ok := rfc3339Field(entry, "resets_at"); ok {
			observation.ResetAt = reset
		}
		return observation, nil
	}
	return Observation{}, ErrNoWeeklyWindow
}

// parseCodex locates the window whose declared duration is exactly one week
// under rate_limit / rate_limits and reads its used_percent. Additional
// and short limits remain separate in the extended observation.
func parseCodex(root map[string]any, now time.Time) (Observation, error) {
	for _, window := range codexWindows(root) {
		if !codexWeekly(window) {
			continue
		}
		percent, ok := numberField(window, "used_percent", "usedPercent")
		if !ok {
			continue
		}
		observation := Observation{Percent: clampPercent(percent)}
		if reset, ok := codexResetAt(window, now); ok {
			observation.ResetAt = reset
		}
		return observation, nil
	}
	return Observation{}, ErrNoWeeklyWindow
}

func codexWindows(root map[string]any) []map[string]any {
	var out []map[string]any
	collect := func(envelope map[string]any) {
		for _, key := range []string{"primary_window", "primaryWindow", "secondary_window", "secondaryWindow", "primary", "secondary"} {
			if window, ok := envelope[key].(map[string]any); ok {
				out = append(out, window)
			}
		}
	}
	for _, key := range []string{"rate_limit", "rateLimit", "rate_limits", "rateLimits"} {
		if envelope, ok := root[key].(map[string]any); ok {
			collect(envelope)
		}
	}
	collect(root)
	return out
}

func codexWeekly(window map[string]any) bool {
	if seconds, ok := integerField(window, "limit_window_seconds", "limitWindowSeconds"); ok {
		return seconds == weeklyWindowSeconds
	}
	if minutes, ok := integerField(window, "window_minutes", "windowMinutes"); ok {
		return minutes == weeklyWindowMinutes
	}
	return false
}

func codexResetAt(window map[string]any, now time.Time) (time.Time, bool) {
	for _, key := range []string{"reset_at", "resetAt"} {
		switch value := window[key].(type) {
		case string:
			if t, ok := parseRFC3339(value); ok && plausibleReset(t, now) {
				return t, true
			}
		case json.Number:
			if seconds, err := strconv.ParseInt(value.String(), 10, 64); err == nil {
				t := time.Unix(seconds, 0).UTC()
				if plausibleReset(t, now) {
					return t, true
				}
			}
		}
	}
	if seconds, ok := numberField(window, "reset_after_seconds", "resetAfterSeconds"); ok && seconds >= 0 && seconds <= weeklyWindowSeconds {
		// Relative offsets drift by a few seconds between polls; round to the
		// minute so the value is stable enough to key a window.
		return now.Add(time.Duration(seconds * float64(time.Second))).Round(time.Minute), true
	}
	return time.Time{}, false
}

func plausibleReset(reset, now time.Time) bool {
	const slack = weeklyWindowSeconds*time.Second + time.Hour
	return !reset.Before(now.Add(-slack)) && !reset.After(now.Add(slack))
}

// parseXAI reads Grok's unified credit usage. creditUsagePercent is the
// shared pool percentage; currentPeriod.end (or the deprecated
// billingPeriodEnd) is the reset instant.
func parseXAI(root map[string]any) (Observation, error) {
	cfg, ok := root["config"].(map[string]any)
	if !ok {
		return Observation{}, ErrNoWeeklyWindow
	}
	percent, ok := numberField(cfg, "creditUsagePercent", "credit_usage_percent")
	if !ok {
		return Observation{}, ErrNoWeeklyWindow
	}
	observation := Observation{Percent: clampPercent(percent)}
	if period, ok := cfg["currentPeriod"].(map[string]any); ok {
		if reset, ok := rfc3339Field(period, "end"); ok {
			observation.ResetAt = reset
		}
	} else if period, ok := cfg["current_period"].(map[string]any); ok {
		if reset, ok := rfc3339Field(period, "end"); ok {
			observation.ResetAt = reset
		}
	}
	if observation.ResetAt.IsZero() {
		if reset, ok := rfc3339Field(cfg, "billingPeriodEnd", "billing_period_end"); ok {
			observation.ResetAt = reset
		}
	}
	return observation, nil
}

func clampPercent(value float64) float64 {
	if math.IsNaN(value) || value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func stringField(obj map[string]any, key string) string {
	if value, ok := obj[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func firstStringField(obj map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringField(obj, key); value != "" {
			return value
		}
	}
	return ""
}

func numberField(obj map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		switch value := obj[key].(type) {
		case json.Number:
			if f, err := value.Float64(); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
				return f, true
			}
		case float64:
			if !math.IsNaN(value) && !math.IsInf(value, 0) {
				return value, true
			}
		}
	}
	return 0, false
}

func integerField(obj map[string]any, keys ...string) (int64, bool) {
	for _, key := range keys {
		if value, ok := obj[key].(json.Number); ok {
			if n, err := strconv.ParseInt(value.String(), 10, 64); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

func rfc3339Field(obj map[string]any, keys ...string) (time.Time, bool) {
	for _, key := range keys {
		if value, ok := obj[key].(string); ok {
			if t, ok := parseRFC3339(value); ok {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

func parseRFC3339(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}
