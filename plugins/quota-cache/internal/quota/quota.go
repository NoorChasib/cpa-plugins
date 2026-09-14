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
//	        rate-limits it hard.
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
	claudeUsageURL  = "https://api.anthropic.com/api/oauth/usage"
	claudeOAuthBeta = "oauth-2025-04-20"

	codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"
	// codexUserAgent is CLI-shaped because the ChatGPT backend edge rejects
	// non-CLI clients on this endpoint. The comment segment identifies the
	// real caller.
	codexUserAgent = "codex_cli_rs/0.0.0 (cpa-plugins/quota-cache)"
	// Anthropic buckets this endpoint by User-Agent: a caller that does not
	// identify as Claude Code lands in a far tighter bucket and starts
	// collecting 429s after a handful of polls. Sending it is the difference
	// between a credential that reports and one that sits in backoff.
	claudeUserAgent = "claude-code/2.1.0 (cpa-plugins/quota-cache)"

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
	// Derived after ObservedAt is set: a weekly window is only synthesized when
	// the primary observation actually succeeded.
	observation.Windows = canonicalWindows(provider, details, observation)
	observation.Plan, observation.TierName, observation.RenewalAt = identity(provider, details)
	// Last resort, so a tier the usage response stopped reporting still reaches
	// the dashboard as a name rather than an empty badge.
	if observation.Plan == "" {
		observation.Plan = planName(creds.plan)
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
		if account == "" && idToken != "" {
			account = accountIDFromIDToken(idToken)
		}
		creds.accountID = account
	case "xai":
		creds.userID = stringField(root, "sub")
	}
	return creds, nil
}

func accountIDFromIDToken(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
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
