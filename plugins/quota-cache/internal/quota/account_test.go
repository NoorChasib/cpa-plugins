package quota

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

const (
	claudeAuth = `{"access_token":"synthetic-secret"}`
	codexAuth  = `{"access_token":"synthetic-secret","account_id":"acct-1"}`
	xaiAuth    = `{"access_token":"synthetic-secret","sub":"user-1"}`
)

var codexSubscriptionURL = codexSubscriptionsURL + "?account_id=acct-1"

// recordingDoer answers per URL like routingDoer and keeps whole requests, so
// a test can check the headers an account endpoint was sent.
type recordingDoer struct {
	routingDoer
	requests []protocol.HostHTTPRequest
}

func (r *recordingDoer) HTTPDo(ctx context.Context, request protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	r.requests = append(r.requests, request)
	return r.routingDoer.HTTPDo(ctx, request)
}

func (r *recordingDoer) sent(url string) (protocol.HostHTTPRequest, bool) {
	for _, request := range r.requests {
		if request.URL == url {
			return request, true
		}
	}
	return protocol.HostHTTPRequest{}, false
}

func header(request protocol.HostHTTPRequest, key string) string {
	if values := request.Headers[key]; len(values) > 0 {
		return values[0]
	}
	return ""
}

// Claude's plan, as a fixed machine token a consumer can label. The
// organisation type and rate-limit tier are what Claude Code reads; the
// account flags are the CPA management centre's fallback.
func TestClaudePlanTokenFromTheProfile(t *testing.T) {
	cases := []struct{ name, profile, want string }{
		{"team", `{"organization":{"organization_type":"claude_team","subscription_status":"active","rate_limit_tier":"default_claude_max_5x"},` +
			`"account":{"has_claude_max":false,"has_claude_pro":false}}`, "team"},
		{"team without a status", `{"organization":{"organization_type":"claude_team"}}`, "team"},
		// A member of a lapsed Team can still hold a personal Max.
		{"lapsed team with a personal max", `{"organization":{"organization_type":"claude_team","subscription_status":"canceled"},` +
			`"account":{"has_claude_max":true}}`, "max"},
		{"enterprise", `{"organization":{"organization_type":"claude_enterprise","subscription_status":"active"}}`, "enterprise"},
		{"max 20x", `{"organization":{"organization_type":"claude_max","rate_limit_tier":"default_claude_max_20x"}}`, "max_20x"},
		{"max 5x", `{"organization":{"organization_type":"claude_max","rate_limit_tier":"default_claude_max_5x"}}`, "max_5x"},
		{"max of unknown multiplier", `{"organization":{"organization_type":"claude_max"}}`, "max"},
		{"max by account flag", `{"organization":{"rate_limit_tier":"default_claude_max_20x"},"account":{"has_claude_max":true}}`, "max_20x"},
		{"max by tier alone", `{"organization":{"rate_limit_tier":"default_claude_max_5x"}}`, "max_5x"},
		{"pro", `{"organization":{"organization_type":"claude_pro"}}`, "pro"},
		{"pro by account flag", `{"account":{"has_claude_max":false,"has_claude_pro":true}}`, "pro"},
		{"free", `{"organization":{"organization_type":"claude_free"},"account":{"has_claude_max":false,"has_claude_pro":false}}`, "free"},
		// Flags that are simply absent are not evidence of a free account.
		{"nothing said", `{"organization":{},"account":{"has_claude_max":false}}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &routingDoer{bodies: map[string]string{claudeProfileURL: tc.profile}}
			details := RefreshDetails(context.Background(), doer, "claude", []byte(claudeAuth), nil, now)
			if details == nil || details.Plan != tc.want || !details.CheckedAt.Equal(now) {
				t.Fatalf("details = %+v, want plan %q checked now", details, tc.want)
			}
		})
	}
}

// The profile request is made like the usage request: same token, same beta,
// and the Claude Code User-Agent without which Anthropic rate-limits hard.
func TestClaudeProfileRequestIdentifiesItself(t *testing.T) {
	doer := &recordingDoer{routingDoer: routingDoer{bodies: map[string]string{claudeProfileURL: `{}`}}}
	RefreshDetails(context.Background(), doer, "claude", []byte(claudeAuth), nil, now)
	request, ok := doer.sent(claudeProfileURL)
	if !ok || len(doer.requests) != 1 {
		t.Fatalf("requests = %+v", doer.requests)
	}
	if header(request, "Authorization") != "Bearer synthetic-secret" || header(request, "anthropic-beta") != claudeOAuthBeta ||
		header(request, "User-Agent") != claudeUserAgent {
		t.Fatalf("profile headers = %v", request.Headers)
	}
}

// When the Claude subscription began, read off the profile the plan already
// comes from: the only anchor Anthropic gives for estimating a renewal. Read as
// Codex's active_until is — RFC 3339, or unix seconds or milliseconds as a
// number or a string — and held to the window a start can occupy: no more than
// a day ahead (clock skew), no more than twenty years back.
func TestClaudeSubscriptionStartFromTheProfile(t *testing.T) {
	want := time.Date(2025, time.January, 31, 9, 15, 0, 0, time.UTC)
	profile := func(value string) string {
		return `{"organization":{"organization_type":"claude_max","rate_limit_tier":"default_claude_max_20x",` +
			`"subscription_created_at":` + value + `}}`
	}
	read := func(t *testing.T, body string) *client.AccountDetails {
		t.Helper()
		doer := &routingDoer{bodies: map[string]string{claudeProfileURL: body}}
		details := RefreshDetails(context.Background(), doer, "claude", []byte(claudeAuth), nil, now)
		if details == nil || len(doer.urls) != 1 {
			t.Fatalf("details = %+v urls = %v; the start must ride on the one profile request", details, doer.urls)
		}
		// The start is judged on its own: a bad one costs nothing else.
		if details.Plan != "max_20x" {
			t.Fatalf("plan = %q; the start's reading touched the plan", details.Plan)
		}
		return details
	}
	for name, one := range map[string]struct {
		body string
		want time.Time
	}{
		"rfc3339":             {profile(`"2025-01-31T09:15:00Z"`), want},
		"rfc3339 with offset": {profile(`"2025-01-31T10:15:00+01:00"`), want},
		"rfc3339 fractional":  {profile(`"2025-01-31T09:15:00.250Z"`), want.Add(250 * time.Millisecond)},
		"seconds":             {profile(`1738314900`), want},
		"seconds as string":   {profile(`"1738314900"`), want},
		"milliseconds":        {profile(`1738314900000`), want},
		"ms as string":        {profile(`"1738314900000"`), want},
		"camel case": {`{"organization":{"organization_type":"claude_max","rate_limit_tier":"default_claude_max_20x",` +
			`"subscriptionCreatedAt":"2025-01-31T09:15:00Z"}}`, want},
		// Bought a moment ago, stamped by a clock a little ahead of this one.
		"within a day ahead":      {profile(`"2026-09-04T23:00:00Z"`), now.Add(time.Hour)},
		"nearly twenty years ago": {profile(`"2006-09-05T00:00:00Z"`), time.Date(2006, time.September, 5, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(name, func(t *testing.T) {
			details := read(t, one.body)
			if details.SubscriptionStartedAt == nil || !details.SubscriptionStartedAt.Equal(one.want) ||
				details.SubscriptionStartedAt.Location() != time.UTC {
				t.Fatalf("subscription started at %v, want %s in UTC", details.SubscriptionStartedAt, one.want)
			}
		})
	}
	for name, body := range map[string]string{
		"absent":             `{"organization":{"organization_type":"claude_max","rate_limit_tier":"default_claude_max_20x"}}`,
		"null":               profile(`null`),
		"zero":               profile(`0`),
		"zero as string":     profile(`"0"`),
		"negative":           profile(`-5`),
		"empty":              profile(`""`),
		"garbage":            profile(`"last spring"`),
		"boolean":            profile(`true`),
		"object":             profile(`{"seconds":1738314900}`),
		"list":               profile(`[1738314900]`),
		"absurd":             profile(`99999999999999999`),
		"more than a day on": profile(`"2026-09-06T00:00:00Z"`),
		"years ahead":        profile(`"2030-01-01T00:00:00Z"`),
		"over twenty years":  profile(`"2006-09-04T21:00:00Z"`),
		"ancient":            profile(`"1999-01-01T00:00:00Z"`),
		"the epoch":          profile(`1`),
		// The organisation is where Claude Code reads it, and only there.
		"on the account": `{"organization":{"organization_type":"claude_max","rate_limit_tier":"default_claude_max_20x"},` +
			`"account":{"subscription_created_at":"2025-01-31T09:15:00Z"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if details := read(t, body); details.SubscriptionStartedAt != nil {
				t.Fatalf("subscription started at %s, want none", details.SubscriptionStartedAt)
			}
		})
	}
}

// The start follows the plan's rules: a failed profile read keeps the last
// good one, a profile that answers without a usable one clears it, and the
// two are judged independently. Codex and Grok never read it, so whatever an
// entry carries passes through their reads untouched.
func TestClaudeSubscriptionStartIsCarriedAndCleared(t *testing.T) {
	started := time.Date(2025, time.January, 31, 9, 15, 0, 0, time.UTC)
	previous := func() *client.AccountDetails {
		return &client.AccountDetails{CheckedAt: now.Add(-7 * time.Hour), Plan: "max_5x", SubscriptionStartedAt: &started}
	}
	for name, doer := range map[string]*routingDoer{
		"transport error": {err: map[string]error{claudeProfileURL: errors.New("unreachable")}},
		"rate limited":    {status: map[string]int{claudeProfileURL: 429}},
		"refused":         {status: map[string]int{claudeProfileURL: 401}},
		"unreadable":      {bodies: map[string]string{claudeProfileURL: `not json`}},
	} {
		t.Run("kept on "+name, func(t *testing.T) {
			details := RefreshDetails(context.Background(), doer, "claude", []byte(claudeAuth), previous(), now)
			if details == nil || details.SubscriptionStartedAt == nil || !details.SubscriptionStartedAt.Equal(started) || details.Plan != "max_5x" {
				t.Fatalf("details = %+v; a failed read lost the last good start", details)
			}
		})
	}
	for name, one := range map[string]struct {
		profile string
		plan    string
	}{
		"no start, still a plan": {`{"organization":{"organization_type":"claude_pro"}}`, "pro"},
		"a start out of range":   {`{"organization":{"organization_type":"claude_pro","subscription_created_at":"2031-01-01T00:00:00Z"}}`, "pro"},
		"nothing at all":         {`{}`, ""},
	} {
		t.Run("cleared by "+name, func(t *testing.T) {
			doer := &routingDoer{bodies: map[string]string{claudeProfileURL: one.profile}}
			details := RefreshDetails(context.Background(), doer, "claude", []byte(claudeAuth), previous(), now)
			if details == nil || details.SubscriptionStartedAt != nil || details.Plan != one.plan {
				t.Fatalf("details = %+v; want the start cleared and plan %q", details, one.plan)
			}
		})
	}
	t.Run("replaced, plan cleared on its own", func(t *testing.T) {
		doer := &routingDoer{bodies: map[string]string{claudeProfileURL: `{"organization":{"subscription_created_at":"2026-03-12T17:40:00Z"}}`}}
		details := RefreshDetails(context.Background(), doer, "claude", []byte(claudeAuth), previous(), now)
		moved := time.Date(2026, time.March, 12, 17, 40, 0, 0, time.UTC)
		if details == nil || details.Plan != "" || details.SubscriptionStartedAt == nil || !details.SubscriptionStartedAt.Equal(moved) {
			t.Fatalf("details = %+v; want plan cleared and the new start %s", details, moved)
		}
	})
	for _, tc := range []struct{ provider, auth, url, body string }{
		{"codex", codexAuth, codexSubscriptionURL, `{"active_until":1793270280}`},
		{"xai", xaiAuth, xaiSettingsURL, `{"subscription_tier_display":"SuperGrok"}`},
	} {
		t.Run("untouched by "+tc.provider, func(t *testing.T) {
			doer := &routingDoer{bodies: map[string]string{tc.url: tc.body}}
			details := RefreshDetails(context.Background(), doer, tc.provider, []byte(tc.auth), previous(), now)
			if details == nil || details.SubscriptionStartedAt == nil || !details.SubscriptionStartedAt.Equal(started) {
				t.Fatalf("details = %+v", details)
			}
		})
	}
	// Within the interval nothing is asked, and the start comes back as it was.
	recent := previous()
	recent.CheckedAt = now.Add(-time.Hour)
	doer := &routingDoer{}
	if got := RefreshDetails(context.Background(), doer, "claude", []byte(claudeAuth), recent, now); got != recent || len(doer.urls) != 0 {
		t.Fatalf("details = %+v urls = %v", got, doer.urls)
	}
}

// The start is a fact for a consumer to estimate from, never a renewal: it is
// kept on the details and nothing of it reaches the entry's renewal or plan.
func TestASubscriptionStartIsNotARenewal(t *testing.T) {
	started := time.Date(2025, time.January, 31, 9, 15, 0, 0, time.UTC)
	o := fetchDetails(t, "claude", `{"seven_day":{"utilization":10}}`)
	details := &client.AccountDetails{CheckedAt: now, Plan: "max_20x", SubscriptionStartedAt: &started}
	o.ApplyDetails(details, now)
	if !o.RenewalAt.IsZero() || o.Plan != "max_20x" || o.AccountDetails != details {
		t.Fatalf("observation = %+v; the start must stay on the details", o)
	}
}

// Each provider's account endpoints are asked at most once per interval. A
// restart does not reset that, because the check time is in the snapshot the
// caller hands back, and a clock that moved backwards does not freeze it.
func TestAccountDetailsAreReadAtMostOncePerInterval(t *testing.T) {
	for _, tc := range []struct{ provider, auth, url, body string }{
		{"claude", claudeAuth, claudeProfileURL, `{"organization":{"organization_type":"claude_pro"}}`},
		{"codex", codexAuth, codexSubscriptionURL, `{"active_until":1792838400}`},
		{"xai", xaiAuth, xaiSettingsURL, `{"subscription_tier_display":"SuperGrok"}`},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			doer := &routingDoer{bodies: map[string]string{tc.url: tc.body}}
			read := func(known *client.AccountDetails, at time.Time) *client.AccountDetails {
				return RefreshDetails(context.Background(), doer, tc.provider, []byte(tc.auth), known, at)
			}
			first := read(nil, now)
			if len(doer.urls) != 1 || !doer.asked(tc.url) || first == nil || !first.CheckedAt.Equal(now) {
				t.Fatalf("first read: urls=%v details=%+v", doer.urls, first)
			}
			for _, later := range []time.Duration{0, time.Minute, DetailsInterval - time.Second} {
				if got := read(first, now.Add(later)); got != first {
					t.Fatalf("+%s: details replaced within the interval: %+v", later, got)
				}
			}
			if len(doer.urls) != 1 {
				t.Fatalf("asked again within the interval: %v", doer.urls)
			}
			if again := read(first, now.Add(DetailsInterval)); !again.CheckedAt.Equal(now.Add(DetailsInterval)) || len(doer.urls) != 2 {
				t.Fatalf("not asked once the interval had passed: urls=%v details=%+v", doer.urls, again)
			}
			if read(first, now.Add(-time.Hour)); len(doer.urls) != 3 {
				t.Fatalf("a check dated in the future suppressed the read: %v", doer.urls)
			}
		})
	}
}

// A read that fails keeps the last good value. The check time still advances,
// so a failing endpoint is not asked on every poll.
func TestAccountDetailFailuresKeepTheLastGoodValue(t *testing.T) {
	renewal := time.Date(2026, time.October, 29, 10, 38, 0, 0, time.UTC)
	previous := func() *client.AccountDetails {
		return &client.AccountDetails{CheckedAt: now.Add(-7 * time.Hour), Plan: "kept-plan", RenewalAt: &renewal}
	}
	failures := map[string]func(url string) *routingDoer{
		"transport error": func(url string) *routingDoer {
			return &routingDoer{err: map[string]error{url: errors.New("unreachable")}}
		},
		"rate limited": func(url string) *routingDoer { return &routingDoer{status: map[string]int{url: 429}} },
		"refused":      func(url string) *routingDoer { return &routingDoer{status: map[string]int{url: 401}} },
		"server error": func(url string) *routingDoer { return &routingDoer{status: map[string]int{url: 500}} },
		"unreadable":   func(url string) *routingDoer { return &routingDoer{bodies: map[string]string{url: `not json`}} },
	}
	for name, failure := range failures {
		for _, tc := range []struct{ provider, auth, url string }{
			{"claude", claudeAuth, claudeProfileURL},
			{"codex", codexAuth, codexSubscriptionURL},
			{"xai", xaiAuth, xaiSettingsURL},
		} {
			t.Run(name+"/"+tc.provider, func(t *testing.T) {
				doer := failure(tc.url)
				if tc.provider == "xai" {
					// Both Grok endpoints fail the same way.
					if doer.err != nil {
						doer.err[xaiUserURL] = doer.err[tc.url]
					}
					if doer.status != nil {
						doer.status[xaiUserURL] = doer.status[tc.url]
					}
					if doer.bodies != nil {
						doer.bodies[xaiUserURL] = doer.bodies[tc.url]
					}
				}
				details := RefreshDetails(context.Background(), doer, tc.provider, []byte(tc.auth), previous(), now)
				if details == nil || details.Plan != "kept-plan" || details.RenewalAt == nil || !details.RenewalAt.Equal(renewal) {
					t.Fatalf("details = %+v; the last good value was lost", details)
				}
				if !details.CheckedAt.Equal(now) {
					t.Fatalf("checked at %s; a failed read must still wait out the interval", details.CheckedAt)
				}
			})
		}
	}
}

// An endpoint that answers but no longer names the value is the account saying
// it has none — a lapsed plan, a subscription with no date ahead — not a failed
// read. The carried value is cleared so the weaker sources apply again, as the
// CPA console would show it; keeping it would show a lapsed SuperGrok Heavy, or
// a past renewal, for as long as the endpoint keeps answering.
func TestAnAnswerWithoutTheValueClearsIt(t *testing.T) {
	renewal := time.Date(2026, time.October, 29, 10, 38, 0, 0, time.UTC)
	previous := func() *client.AccountDetails {
		return &client.AccountDetails{CheckedAt: now.Add(-7 * time.Hour), Plan: "kept-plan", RenewalAt: &renewal}
	}
	for name, body := range map[string]string{
		"value missing": `{}`,
		"value null":    `{"subscription_tier_display":null,"subscriptionTier":null,"active_until":null}`,
		"label invalid": `{"subscription_tier_display":"bad\nvalue","subscriptionTier":"bad\nvalue","active_until":"soon"}`,
		"date passed":   `{"active_until":"2026-09-01T00:00:00Z"}`,
	} {
		for _, tc := range []struct{ provider, auth, url string }{
			{"claude", claudeAuth, claudeProfileURL},
			{"codex", codexAuth, codexSubscriptionURL},
			{"xai", xaiAuth, xaiSettingsURL},
		} {
			t.Run(name+"/"+tc.provider, func(t *testing.T) {
				// Both Grok endpoints answer the same way.
				doer := &routingDoer{bodies: map[string]string{tc.url: body, xaiUserURL: body}}
				details := RefreshDetails(context.Background(), doer, tc.provider, []byte(tc.auth), previous(), now)
				if details == nil || !details.CheckedAt.Equal(now) {
					t.Fatalf("details = %+v", details)
				}
				switch tc.provider {
				case "codex":
					if details.RenewalAt != nil || details.Plan != "kept-plan" {
						t.Fatalf("details = %+v; the renewal should be cleared and nothing else touched", details)
					}
				default:
					if details.Plan != "" || details.RenewalAt == nil {
						t.Fatalf("details = %+v; the plan should be cleared and nothing else touched", details)
					}
				}
			})
		}
	}
}

// Grok's plan is cleared only when both endpoints answered without one. If
// either request failed, its silence is not evidence the plan has gone.
func TestAGrokPlanIsClearedOnlyWhenBothEndpointsAnswered(t *testing.T) {
	known := &client.AccountDetails{CheckedAt: now.Add(-7 * time.Hour), Plan: "SuperGrok Heavy"}
	for name, one := range map[string]struct {
		doer *routingDoer
		want string
	}{
		"settings silent, user failed": {&routingDoer{
			bodies: map[string]string{xaiSettingsURL: `{"theme":"dark"}`}, status: map[string]int{xaiUserURL: 503},
		}, "SuperGrok Heavy"},
		"settings failed, user silent": {&routingDoer{
			bodies: map[string]string{xaiUserURL: `{"subscriptionTier":null}`}, status: map[string]int{xaiSettingsURL: 503},
		}, "SuperGrok Heavy"},
		"settings failed, user names one": {&routingDoer{
			bodies: map[string]string{xaiUserURL: `{"subscriptionTier":"SuperGrok"}`}, status: map[string]int{xaiSettingsURL: 503},
		}, "SuperGrok"},
		"both silent": {&routingDoer{
			bodies: map[string]string{xaiSettingsURL: `{"theme":"dark"}`, xaiUserURL: `{"subscriptionTier":null}`},
		}, ""},
	} {
		if details := RefreshDetails(context.Background(), one.doer, "xai", []byte(xaiAuth), known, now); details == nil || details.Plan != one.want {
			t.Errorf("%s: details = %+v, want plan %q", name, details, one.want)
		}
	}
}

// Codex's renewal is when the subscription is paid up to. The endpoint spells
// it in unix seconds, as a number or a string; RFC 3339 and milliseconds are
// read too. Zero is the endpoint's "none".
func TestCodexSubscriptionRenewal(t *testing.T) {
	want := time.Date(2026, time.October, 29, 10, 38, 0, 0, time.UTC)
	for name, body := range map[string]string{
		"seconds":           `{"active_until":1793270280}`,
		"seconds as string": `{"active_until":"1793270280"}`,
		"milliseconds":      `{"active_until":1793270280000}`,
		"rfc3339":           `{"active_until":"2026-10-29T10:38:00Z"}`,
		"camel case":        `{"activeUntil":1793270280}`,
	} {
		t.Run(name, func(t *testing.T) {
			doer := &routingDoer{bodies: map[string]string{codexSubscriptionURL: body}}
			details := RefreshDetails(context.Background(), doer, "codex", []byte(codexAuth), nil, now)
			if details == nil || details.RenewalAt == nil || !details.RenewalAt.Equal(want) {
				t.Fatalf("details = %+v, want renewal %s", details, want)
			}
		})
	}
	for name, body := range map[string]string{
		"zero":      `{"active_until":0}`,
		"negative":  `{"active_until":-5}`,
		"absurd":    `{"active_until":99999999999999999}`,
		"centuries": `{"active_until":"2226-10-29T10:38:00Z"}`,
		"null":      `{"active_until":null}`,
		// A date already behind us is not the next renewal.
		"passed": `{"active_until":"2026-09-01T00:00:00Z"}`,
	} {
		t.Run(name, func(t *testing.T) {
			doer := &routingDoer{bodies: map[string]string{codexSubscriptionURL: body}}
			if details := RefreshDetails(context.Background(), doer, "codex", []byte(codexAuth), nil, now); details == nil || details.RenewalAt != nil {
				t.Fatalf("details = %+v, want no renewal", details)
			}
		})
	}
}

// The subscription request is account-scoped twice over and identifies as the
// CLI, like the usage request beside it.
func TestCodexSubscriptionRequestCarriesTheAccountContext(t *testing.T) {
	doer := &recordingDoer{routingDoer: routingDoer{bodies: map[string]string{codexSubscriptionURL: `{}`}}}
	RefreshDetails(context.Background(), doer, "codex", []byte(`{"access_token":"tok","account_id":"acct 1&x"}`), nil, now)
	if len(doer.requests) != 1 {
		t.Fatalf("requests = %+v", doer.requests)
	}
	request := doer.requests[0]
	if request.URL != codexSubscriptionsURL+"?account_id=acct+1%26x" || header(request, "Chatgpt-Account-Id") != "acct 1&x" ||
		header(request, "Authorization") != "Bearer tok" || !strings.HasPrefix(header(request, "User-Agent"), "codex_cli_rs/") {
		t.Fatalf("request = %+v", request)
	}
}

// The renewal a card shows comes from the best source available: the
// subscription endpoint, then the id_token claim the credential already
// carries, then the spend-control reset the usage response implies.
func TestCodexRenewalPrefersTheSubscriptionOverTheTokenOverSpendControl(t *testing.T) {
	claim := time.Date(2026, time.October, 20, 0, 0, 0, 0, time.UTC)
	endpoint := time.Date(2026, time.October, 29, 10, 38, 0, 0, time.UTC)
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":` +
		`{"chatgpt_account_id":"acct-jwt","chatgpt_subscription_active_until":"2026-10-20T00:00:00+00:00"}}`))
	withClaim := `{"access_token":"tok","id_token":"h.` + claims + `.s"}`
	usage := `{"rate_limit":{"primary_window":{"used_percent":40,"limit_window_seconds":604800}},` +
		`"spend_control":{"individual_limit":{"limit":"50.00","used":"10.00","reset_after_seconds":2592000}}}`

	observe := func(auth string) Observation {
		t.Helper()
		o, err := Fetch(context.Background(), &fakeDoer{response: protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(usage)}}, "codex", []byte(auth), now)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	if o := observe(`{"access_token":"tok","account_id":"acct-1"}`); !o.RenewalAt.Equal(now.Add(30 * 24 * time.Hour)) {
		t.Fatalf("renewal = %s; spend control is the last resort and must still be used", o.RenewalAt)
	}
	o := observe(withClaim)
	if !o.RenewalAt.Equal(claim) {
		t.Fatalf("renewal = %s, want the id_token claim %s", o.RenewalAt, claim)
	}
	o.ApplyDetails(&client.AccountDetails{CheckedAt: now, RenewalAt: &endpoint}, now)
	if !o.RenewalAt.Equal(endpoint) {
		t.Fatalf("renewal = %s, want the subscription endpoint's %s", o.RenewalAt, endpoint)
	}
}

// A higher-ranked renewal outranks a lower one only while it is still ahead.
// The id_token can be days old and name the period before the last renewal,
// and a stored endpoint date can pass between six-hourly reads; laid over a
// future fallback, either would leave the card with no renewal at all, because
// the dashboard drops a date that has passed.
func TestAPastRenewalDoesNotOutrankAFutureOne(t *testing.T) {
	usage := `{"rate_limit":{"primary_window":{"used_percent":40,"limit_window_seconds":604800}},` +
		`"spend_control":{"individual_limit":{"limit":"50.00","used":"10.00","reset_after_seconds":2592000}}}`
	withClaim := func(until string) string {
		claims := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":` +
			`{"chatgpt_account_id":"acct-jwt","chatgpt_subscription_active_until":"` + until + `"}}`))
		return `{"access_token":"tok","id_token":"h.` + claims + `.s"}`
	}
	observe := func(auth string) Observation {
		t.Helper()
		o, err := Fetch(context.Background(), &fakeDoer{response: protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(usage)}}, "codex", []byte(auth), now)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	spendControl := now.Add(30 * 24 * time.Hour)

	// A claim minted before the last renewal names a date already gone.
	o := observe(withClaim("2026-09-01T00:00:00+00:00"))
	if !o.RenewalAt.Equal(spendControl) {
		t.Fatalf("renewal = %s; a past id_token claim replaced the spend-control reset %s", o.RenewalAt, spendControl)
	}

	// A stored endpoint date that has since passed does not outrank a fresh
	// claim that is still ahead.
	claim := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)
	o = observe(withClaim("2026-11-01T00:00:00+00:00"))
	stale := time.Date(2026, time.September, 2, 0, 0, 0, 0, time.UTC)
	o.ApplyDetails(&client.AccountDetails{CheckedAt: now.Add(-time.Hour), RenewalAt: &stale}, now)
	if !o.RenewalAt.Equal(claim) {
		t.Fatalf("renewal = %s; a stored date that has passed outranked the claim %s", o.RenewalAt, claim)
	}
}

// Grok's plan is the display name its own clients print. The user endpoint is
// asked only when settings did not supply one.
func TestXAIPlanNameFromSettingsThenUser(t *testing.T) {
	doer := &recordingDoer{routingDoer: routingDoer{bodies: map[string]string{
		xaiSettingsURL: `{"subscription_tier_display":"SuperGrok Heavy","theme":"dark"}`,
	}}}
	details := RefreshDetails(context.Background(), doer, "xai", []byte(xaiAuth), nil, now)
	if details == nil || details.Plan != "SuperGrok Heavy" {
		t.Fatalf("details = %+v", details)
	}
	if doer.asked(xaiUserURL) {
		t.Fatal("the user endpoint was asked although settings named the plan")
	}
	request, _ := doer.sent(xaiSettingsURL)
	if header(request, "Authorization") != "Bearer synthetic-secret" || header(request, "X-XAI-Token-Auth") != xaiTokenAuthHeader ||
		header(request, "X-Grok-Client-Version") != xaiClientVersion || header(request, "User-Agent") != xaiCLIUserAgent {
		t.Fatalf("settings headers = %v", request.Headers)
	}

	for name, settings := range map[string]string{"no display name": `{"theme":"dark"}`, "settings unreadable": `nope`} {
		doer = &recordingDoer{routingDoer: routingDoer{bodies: map[string]string{
			xaiSettingsURL: settings,
			xaiUserURL:     `{"subscriptionTier":"SuperGrok","email":"synthetic@example.invalid"}`,
		}}}
		details = RefreshDetails(context.Background(), doer, "xai", []byte(xaiAuth), nil, now)
		if details == nil || details.Plan != "SuperGrok" || !doer.asked(xaiUserURL) {
			t.Fatalf("%s: details = %+v urls = %v", name, details, doer.urls)
		}
	}
}

// The account endpoints outrank the usage response's own spellings, and a
// stored value is validated again on the way out.
func TestApplyDetailsOutranksTheUsageResponse(t *testing.T) {
	o := fetchDetails(t, "claude", `{"subscription":{"plan":"Max"},"seven_day":{"utilization":10}}`)
	o.ApplyDetails(nil, now)
	if o.Plan != "Max" || o.AccountDetails != nil {
		t.Fatalf("no details changed the observation: %+v", o)
	}
	details := &client.AccountDetails{CheckedAt: now, Plan: "max_20x"}
	o.ApplyDetails(details, now)
	if o.Plan != "max_20x" || o.AccountDetails != details {
		t.Fatalf("plan = %q; the profile's plan outranks the usage response", o.Plan)
	}
	o = fetchDetails(t, "claude", `{"subscription":{"plan":"Max"},"seven_day":{"utilization":10}}`)
	o.ApplyDetails(&client.AccountDetails{CheckedAt: now, Plan: "bad\nvalue"}, now)
	if o.Plan != "Max" {
		t.Fatalf("plan = %q; a malformed stored plan must not replace a good one", o.Plan)
	}
}

// OpenRouter and unknown providers have no account endpoints: no request, and
// what the caller held comes back unchanged.
func TestProvidersWithoutAccountDetailsAskNothing(t *testing.T) {
	known := &client.AccountDetails{CheckedAt: now.Add(-24 * time.Hour), Plan: "kept"}
	for _, provider := range []string{OpenRouterProvider, "gemini"} {
		doer := &routingDoer{}
		if got := RefreshDetails(context.Background(), doer, provider, []byte(claudeAuth), known, now); got != known || len(doer.urls) != 0 {
			t.Fatalf("%s: details = %+v urls = %v", provider, got, doer.urls)
		}
	}
}
