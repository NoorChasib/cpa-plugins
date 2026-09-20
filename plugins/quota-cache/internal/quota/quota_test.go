package quota

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

type fakeDoer struct {
	request  protocol.HostHTTPRequest
	response protocol.HostHTTPResponse
	err      error
}

func (f *fakeDoer) HTTPDo(_ context.Context, request protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	f.request = request
	return f.response, f.err
}

var now = time.Date(2026, time.September, 4, 22, 0, 0, 0, time.UTC)

func TestClaudeKeepsRegularSevenDayProjection(t *testing.T) {
	body := `{"five_hour":{"utilization":99.0,"resets_at":"2026-09-04T23:30:00+00:00"},"seven_day":{"utilization":14.0,"resets_at":"2026-09-09T10:00:00.438310+00:00","limit_dollars":null},"seven_day_opus":{"utilization":100.0},"limits":[]}`
	doer := &fakeDoer{response: protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(body)}}
	observation, err := Fetch(context.Background(), doer, "claude", []byte(`{"access_token":"tok-secret","refresh_token":"r"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Percent != 14 || observation.Provider != "claude" || !observation.ObservedAt.Equal(now) {
		t.Fatalf("observation=%+v", observation)
	}
	if want := time.Date(2026, time.September, 9, 10, 0, 0, 438310000, time.UTC); !observation.ResetAt.Equal(want) {
		t.Fatalf("reset=%s want %s", observation.ResetAt, want)
	}
	if doer.request.URL != claudeUsageURL || doer.request.Headers["Authorization"][0] != "Bearer tok-secret" || doer.request.Headers["anthropic-beta"][0] != claudeOAuthBeta {
		t.Fatalf("request=%+v", doer.request)
	}
}

func TestCodexSelectsExactWeeklyWindowAndUnixReset(t *testing.T) {
	body := `{"plan_type":"pro","rate_limit":{"allowed":true,"primary_window":{"used_percent":25,"limit_window_seconds":604800,"reset_after_seconds":380992,"reset_at":1788913971},"secondary_window":{"used_percent":90,"limit_window_seconds":18000,"reset_at":1788500000}},"additional_rate_limits":[{"limit_name":"code_review","rate_limit":{"primary_window":{"used_percent":100,"limit_window_seconds":604800}}}]}`
	doer := &fakeDoer{response: protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(body)}}
	observation, err := Fetch(context.Background(), doer, "codex", []byte(`{"access_token":"tok","account_id":"acct-1"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Percent != 25 || !observation.ResetAt.Equal(time.Unix(1788913971, 0).UTC()) {
		t.Fatalf("observation=%+v", observation)
	}
	if doer.request.Headers["Chatgpt-Account-Id"][0] != "acct-1" || !strings.HasPrefix(doer.request.Headers["User-Agent"][0], "codex_cli_rs/") {
		t.Fatalf("request headers=%v", doer.request.Headers)
	}
}

func TestCodexResolvesAccountIDFromIDTokenAndRelativeReset(t *testing.T) {
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-jwt"}}`))
	auth := `{"tokens":{"access_token":"tok","id_token":"h.` + claims + `.s"}}`
	body := `{"rate_limits":{"primary":{"usedPercent":97.5,"windowMinutes":10080,"resetAfterSeconds":3600.4}}}`
	doer := &fakeDoer{response: protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(body)}}
	observation, err := Fetch(context.Background(), doer, "codex", []byte(auth), now)
	if err != nil {
		t.Fatal(err)
	}
	if doer.request.Headers["Chatgpt-Account-Id"][0] != "acct-jwt" {
		t.Fatalf("account id not resolved from id_token: %v", doer.request.Headers)
	}
	if observation.Percent != 97.5 || !observation.ResetAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("observation=%+v", observation)
	}
}

func TestCodexWithoutAccountIDFailsBeforeHTTP(t *testing.T) {
	doer := &fakeDoer{}
	_, err := Fetch(context.Background(), doer, "codex", []byte(`{"access_token":"tok"}`), now)
	if !errors.Is(err, ErrNoAccountID) || doer.request.URL != "" {
		t.Fatalf("err=%v request=%+v", err, doer.request)
	}
}

func TestXAIReadsUnifiedCreditsAndPeriodEnd(t *testing.T) {
	body := `{"config":{"creditUsagePercent":28.0,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-09-01T19:33:46.436099+00:00","end":"2026-09-08T19:33:46.436099+00:00"},"isUnifiedBillingUser":true,"productUsage":[{"product":"BUILD","usagePercent":28.0}]}}`
	doer := &fakeDoer{response: protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(body)}}
	observation, err := Fetch(context.Background(), doer, "xai", []byte(`{"access_token":"tok","sub":"user-123","id_token":"x"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Percent != 28 || !observation.ResetAt.Equal(time.Date(2026, time.September, 8, 19, 33, 46, 436099000, time.UTC)) {
		t.Fatalf("observation=%+v", observation)
	}
	if doer.request.URL != xaiBillingURL || doer.request.Headers["X-XAI-Token-Auth"][0] != xaiTokenAuthHeader || doer.request.Headers["x-userid"][0] != "user-123" {
		t.Fatalf("request=%+v", doer.request)
	}
}

func TestXAIOmittedPercentIsZeroAndFallsBackToBillingPeriodEnd(t *testing.T) {
	// proto3 JSON omits zero-valued scalars: an absent creditUsagePercent in a
	// current period is reported by the CLI as 0% used. We require the field to
	// exist so an unrelated response shape cannot be mistaken for 0%.
	body := `{"config":{"credit_usage_percent":0,"billingPeriodEnd":"2026-09-08T00:00:00Z"}}`
	doer := &fakeDoer{response: protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(body)}}
	observation, err := Fetch(context.Background(), doer, "xai", []byte(`{"access_token":"tok"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Percent != 0 || !observation.ResetAt.Equal(time.Date(2026, time.September, 8, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("observation=%+v", observation)
	}
	if _, ok := doer.request.Headers["x-userid"]; ok {
		t.Fatalf("x-userid must be omitted when sub is absent: %v", doer.request.Headers)
	}
}

func TestErrorsAreStaticAndNeverEchoBodiesOrTokens(t *testing.T) {
	secret := "SECRET-TOKEN-VALUE"
	cases := []struct {
		name     string
		provider string
		auth     string
		response protocol.HostHTTPResponse
		err      error
		want     error
	}{
		{"unsupported", "gemini", `{"access_token":"` + secret + `"}`, protocol.HostHTTPResponse{}, nil, ErrUnsupportedProvider},
		{"no token", "claude", `{"refresh_token":"x"}`, protocol.HostHTTPResponse{}, nil, ErrNoAccessToken},
		{"http 401", "claude", `{"access_token":"` + secret + `"}`, protocol.HostHTTPResponse{StatusCode: 401, Body: []byte(`{"error":"bad ` + secret + `"}`)}, nil, HTTPStatusError{StatusCode: 401}},
		{"invalid json", "claude", `{"access_token":"` + secret + `"}`, protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`not json ` + secret)}, nil, ErrInvalidResponse},
		{"trailing json", "claude", `{"access_token":"` + secret + `"}`, protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"seven_day":{"utilization":1}} {}`)}, nil, ErrInvalidResponse},
		{"missing window", "claude", `{"access_token":"` + secret + `"}`, protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"five_hour":{"utilization":null}}`)}, nil, ErrNoWeeklyWindow},
		{"non weekly codex", "codex", `{"access_token":"` + secret + `","account_id":"a"}`, protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"rate_limit":{"primary_window":{"used_percent":null,"limit_window_seconds":18000}}}`)}, nil, ErrNoWeeklyWindow},
		{"xai no config", "xai", `{"access_token":"` + secret + `"}`, protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"error":"` + secret + `"}`)}, nil, ErrNoWeeklyWindow},
		{"transport", "claude", `{"access_token":"` + secret + `"}`, protocol.HostHTTPResponse{}, errors.New("dial failed"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &fakeDoer{response: tc.response, err: tc.err}
			_, err := Fetch(context.Background(), doer, tc.provider, []byte(tc.auth), now)
			if err == nil {
				t.Fatal("expected error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) && err.Error() != tc.want.Error() {
				t.Fatalf("err=%v want %v", err, tc.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaked secret: %v", err)
			}
		})
	}
}

func TestPercentIsClamped(t *testing.T) {
	body := `{"seven_day":{"utilization":143.2,"resets_at":"bad"}}`
	doer := &fakeDoer{response: protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(body)}}
	observation, err := Fetch(context.Background(), doer, "claude", []byte(`{"access_token":"tok"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Percent != 100 || !observation.ResetAt.IsZero() {
		t.Fatalf("observation=%+v", observation)
	}
}

// routingDoer answers per URL and records every request, so a test can tell
// whether the reset-credit inventory was asked for at all.
type routingDoer struct {
	bodies map[string]string
	status map[string]int
	err    map[string]error
	urls   []string
}

func (r *routingDoer) HTTPDo(_ context.Context, request protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	r.urls = append(r.urls, request.URL)
	if err := r.err[request.URL]; err != nil {
		return protocol.HostHTTPResponse{}, err
	}
	status := r.status[request.URL]
	if status == 0 {
		status = 200
	}
	return protocol.HostHTTPResponse{StatusCode: status, Body: []byte(r.bodies[request.URL])}, nil
}

func (r *routingDoer) asked(url string) bool {
	for _, seen := range r.urls {
		if seen == url {
			return true
		}
	}
	return false
}

const codexUsageWithBankedResets = `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":40,"limit_window_seconds":604800}},"rate_limit_reset_credits":{"available_count":2}}`

// The count rides along on the usage response, and the expiry is the soonest
// among the credits that can still be spent. A redeemed credit and one that
// has already lapsed both carry dates, and neither may set the headline.
func TestCodexBanksResetCountAndDatesTheSoonestSpendableCredit(t *testing.T) {
	doer := &routingDoer{bodies: map[string]string{
		codexUsageURL: codexUsageWithBankedResets,
		codexResetCreditsURL: `{"available_count":2,"credits":[` +
			`{"id":"c1","status":"redeemed","expires_at":"2026-09-05T00:00:00Z"},` +
			`{"id":"c2","status":"available","expires_at":"2026-09-20T00:00:00Z"},` +
			`{"id":"c3","status":"available","expires_at":"2026-09-01T00:00:00Z"},` +
			`{"id":"c4","status":"available","expires_at":"2026-09-11T00:00:00Z"}]}`,
	}}
	observation, err := Fetch(context.Background(), doer, "codex", []byte(`{"access_token":"tok","account_id":"acct-1"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	credits := observation.Quota.ResetCredits
	if credits == nil || credits.AvailableCount != 2 {
		t.Fatalf("reset credits = %+v", credits)
	}
	// c3 is available but already expired at the poll instant, and c1 is not
	// spendable at all, so c4 is the soonest deadline that means anything.
	want := time.Date(2026, time.September, 11, 0, 0, 0, 0, time.UTC)
	if credits.SoonestExpiry == nil || !credits.SoonestExpiry.Equal(want) {
		t.Fatalf("soonest expiry = %v, want %s", credits.SoonestExpiry, want)
	}
}

// The inventory endpoint is an ornament on an observation that is already
// complete. Losing it costs the expiry line and nothing else.
func TestCodexKeepsTheResetCountWhenTheInventoryCannotBeRead(t *testing.T) {
	for name, doer := range map[string]*routingDoer{
		"transport error": {
			bodies: map[string]string{codexUsageURL: codexUsageWithBankedResets},
			err:    map[string]error{codexResetCreditsURL: errors.New("unreachable")},
		},
		"http error": {
			bodies: map[string]string{codexUsageURL: codexUsageWithBankedResets},
			status: map[string]int{codexResetCreditsURL: 500},
		},
		"undated entries": {bodies: map[string]string{
			codexUsageURL:        codexUsageWithBankedResets,
			codexResetCreditsURL: `{"credits":[{"id":"c1","status":"available"}]}`,
		}},
	} {
		observation, err := Fetch(context.Background(), doer, "codex", []byte(`{"access_token":"tok","account_id":"acct-1"}`), now)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		credits := observation.Quota.ResetCredits
		if credits == nil || credits.AvailableCount != 2 || credits.SoonestExpiry != nil {
			t.Fatalf("%s: reset credits = %+v", name, credits)
		}
		if observation.Percent != 40 {
			t.Fatalf("%s: the observation itself was damaged: %+v", name, observation)
		}
	}
}

// Nothing banked must cost nothing. An account with no credits is the common
// case, and it must not turn one poll into two.
func TestCodexAsksForTheInventoryOnlyWhenSomethingIsBanked(t *testing.T) {
	for name, usage := range map[string]string{
		"key absent": `{"rate_limit":{"primary_window":{"used_percent":40,"limit_window_seconds":604800}}}`,
		"key null":   `{"rate_limit":{"primary_window":{"used_percent":40,"limit_window_seconds":604800}},"rate_limit_reset_credits":null}`,
		"count zero": `{"rate_limit":{"primary_window":{"used_percent":40,"limit_window_seconds":604800}},"rate_limit_reset_credits":{"available_count":0}}`,
		"count negative": `{"rate_limit":{"primary_window":{"used_percent":40,"limit_window_seconds":604800}},` +
			`"rate_limit_reset_credits":{"available_count":-3}}`,
		"count not a number": `{"rate_limit":{"primary_window":{"used_percent":40,"limit_window_seconds":604800}},` +
			`"rate_limit_reset_credits":{"available_count":"two"}}`,
	} {
		doer := &routingDoer{bodies: map[string]string{codexUsageURL: usage}}
		observation, err := Fetch(context.Background(), doer, "codex", []byte(`{"access_token":"tok","account_id":"acct-1"}`), now)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if observation.Quota.ResetCredits != nil {
			t.Fatalf("%s: reset credits = %+v, want nil", name, observation.Quota.ResetCredits)
		}
		if doer.asked(codexResetCreditsURL) {
			t.Fatalf("%s: the inventory was requested with nothing banked", name)
		}
	}
}

// The inventory request is account-scoped like every other Codex call here. A
// missing account header reads another account's credits or none at all.
func TestCodexInventoryRequestCarriesTheAccountContext(t *testing.T) {
	doer := &routingDoer{bodies: map[string]string{
		codexUsageURL:        codexUsageWithBankedResets,
		codexResetCreditsURL: `{"credits":[{"id":"c1","status":"available","expires_at":"2026-09-20T00:00:00Z"}]}`,
	}}
	if _, err := Fetch(context.Background(), doer, "codex", []byte(`{"access_token":"tok","account_id":"acct-1"}`), now); err != nil {
		t.Fatal(err)
	}
	if !doer.asked(codexResetCreditsURL) {
		t.Fatal("the inventory was never requested")
	}
}

// Nothing from the inventory response may reach the snapshot except the one
// timestamp: credit ids and labels are account identifiers with no display use.
func TestCodexInventoryLeaksNothingIntoTheSnapshot(t *testing.T) {
	doer := &routingDoer{bodies: map[string]string{
		codexUsageURL: codexUsageWithBankedResets,
		codexResetCreditsURL: `{"credits":[{"id":"RateLimitResetCredit_synthetic-id","status":"available",` +
			`"title":"synthetic-label","expires_at":"2026-09-20T00:00:00Z"}]}`,
	}}
	observation, err := Fetch(context.Background(), doer, "codex", []byte(`{"access_token":"tok","account_id":"acct-1"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(observation.Quota)
	for _, secret := range []string{"synthetic-id", "synthetic-label", "RateLimitResetCredit"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("the snapshot carries %q from the inventory response", secret)
		}
	}
}
