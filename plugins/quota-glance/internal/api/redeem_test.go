package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/aggregate"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/redeem"
)

const (
	redeemPath      = "/v0/management/plugins/quota-glance/redeem"
	redeemTokenPath = "/v0/resource/plugins/quota-glance/redeem"
	codexID         = "codex-noor@example.com.json"
	claudeID        = "claude-northwind@example.com.json"
)

// fakeRedeemer records whether it was asked to spend anything. For a route that
// cannot be undone, "was it called at all" is the assertion that matters most.
type fakeRedeemer struct {
	mu       sync.Mutex
	calls    int
	provider string
	deadline time.Duration
	result   redeem.Result
	err      error
}

func (f *fakeRedeemer) Redeem(ctx context.Context, provider, _ string) (redeem.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.provider = provider
	if deadline, ok := ctx.Deadline(); ok {
		f.deadline = time.Until(deadline)
	}
	return f.result, f.err
}

func (f *fakeRedeemer) called() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// redeemableAPI serves a document with one spendable Codex credential, one
// spendable Claude credential, one Codex credential whose credit the document
// says is not redeemable, and one Claude credential with nothing banked at all.
func redeemableAPI(t *testing.T) (*API, *fakeRedeemer) {
	t.Helper()
	a := New("quota-glance", testToken)
	a.Publish(aggregate.Document{
		SchemaVersion: 1, GeneratedAtEpoch: 1789012800,
		Credentials: []aggregate.Credential{
			{ID: codexID, Provider: "codex", ResetCredits: &aggregate.ResetCredits{AvailableCount: 2, Redeemable: true}},
			{ID: claudeID, Provider: "claude", ResetCredits: &aggregate.ResetCredits{AvailableCount: 3, Redeemable: true, Hold: "cooldown"}},
			{ID: "codex-parked@example.com.json", Provider: "codex", ResetCredits: &aggregate.ResetCredits{AvailableCount: 1}},
			{ID: "claude-a@example.com.json", Provider: "claude"},
		},
		Providers: []aggregate.Provider{},
	}, Health{Version: "test"})
	redeemer := &fakeRedeemer{result: redeem.Result{Outcome: redeem.OutcomeReset, WindowsReset: 2, RemainingCount: 1}}
	a.SetRedeemer(redeemer)
	return a, redeemer
}

func post(a *API, path string, headers http.Header, body any) protocol.ManagementResponse {
	if headers == nil {
		headers = http.Header{}
	}
	if headers.Get("Content-Type") == "" {
		headers.Set("Content-Type", "application/json")
	}
	raw, _ := json.Marshal(body)
	return a.Handle(protocol.ManagementRequest{Method: "POST", Path: path, Headers: headers, Body: raw}, time.Now())
}

func decodeBody(t *testing.T, res protocol.ManagementResponse) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(res.Body, &out); err != nil {
		t.Fatalf("response body is not JSON: %q", res.Body)
	}
	return out
}

func TestRedeemSpendsOneCreditAndReportsIt(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	res := post(a, redeemPath, nil, map[string]any{"credentialId": codexID, "confirmed": true})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.StatusCode, res.Body)
	}
	body := decodeBody(t, res)
	if body["outcome"] != redeem.OutcomeReset || body["windowsReset"] != 2.0 || body["remainingCount"] != 1.0 {
		t.Fatalf("body = %v", body)
	}
	// Which provider answered, so the client can word it in that provider's
	// terms.
	if body["provider"] != "codex" {
		t.Fatalf("provider = %v", body["provider"])
	}
	// The count on the card comes from quota-cache and will not move until its
	// next poll. Saying so is the difference between a dashboard that looks
	// wrong for ten minutes and one that explains itself.
	if body["snapshotPending"] != true {
		t.Fatalf("the response did not warn that the card lags the snapshot: %v", body)
	}
	if redeemer.called() != 1 {
		t.Fatalf("redeemer called %d times", redeemer.called())
	}
}

// The dialog lives in the browser. A request that arrives without its answer
// must not spend a credit merely because it was well formed.
func TestRedeemRefusesAnUnconfirmedRequest(t *testing.T) {
	for name, body := range map[string]any{
		"confirmed false":           map[string]any{"credentialId": codexID, "confirmed": false},
		"confirmed absent":          map[string]any{"credentialId": codexID},
		"confirmed a truthy string": map[string]any{"credentialId": codexID, "confirmed": "yes"},
	} {
		a, redeemer := redeemableAPI(t)
		res := post(a, redeemPath, nil, body)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status = %d", name, res.StatusCode)
		}
		if redeemer.called() != 0 {
			t.Fatalf("%s: a credit was spent without a confirmation", name)
		}
	}
}

// The document decides what may be redeemed against. A caller cannot nominate a
// credential the dashboard is not already offering.
func TestRedeemRefusesACredentialTheDocumentDoesNotOffer(t *testing.T) {
	for name, id := range map[string]string{
		"not redeemable":   "codex-parked@example.com.json",
		"no banked resets": "claude-a@example.com.json",
		"unknown":          "does-not-exist.json",
		"empty":            "",
	} {
		a, redeemer := redeemableAPI(t)
		res := post(a, redeemPath, nil, map[string]any{"credentialId": id, "confirmed": true})
		if res.StatusCode != http.StatusConflict {
			t.Fatalf("%s: status = %d", name, res.StatusCode)
		}
		if redeemer.called() != 0 {
			t.Fatalf("%s: reached the redeemer", name)
		}
	}
}

// With redemption switched off the route is gone, not merely guarded.
func TestRedeemRouteIsAbsentWithoutARedeemer(t *testing.T) {
	a := New("quota-glance", testToken)
	a.Publish(aggregate.Document{
		SchemaVersion: 1,
		Credentials: []aggregate.Credential{
			{ID: codexID, Provider: "codex", ResetCredits: &aggregate.ResetCredits{AvailableCount: 2, Redeemable: true}},
		},
		Providers: []aggregate.Provider{},
	}, Health{})
	for _, path := range []string{redeemPath, redeemTokenPath} {
		headers := http.Header{"Authorization": {"Bearer " + testToken}}
		if res := post(a, path, headers, map[string]any{"credentialId": codexID, "confirmed": true}); res.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", path, res.StatusCode)
		}
	}
}

// The public door carries this plugin's own token, exactly as the summary
// fallback does. CPA authenticates nothing on a resource path.
func TestRedeemOverTheTokenDoorRequiresTheToken(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	body := map[string]any{"credentialId": codexID, "confirmed": true}

	if res := post(a, redeemTokenPath, nil, body); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the public path spent a credit unauthenticated: %d", res.StatusCode)
	}
	if redeemer.called() != 0 {
		t.Fatal("an unauthenticated request reached the redeemer")
	}
	headers := http.Header{"Authorization": {"Bearer " + testToken}, "Content-Type": {"application/json"}}
	if res := post(a, redeemTokenPath, headers, body); res.StatusCode != http.StatusOK {
		t.Fatalf("authenticated token redeem: %d, body = %s", res.StatusCode, res.Body)
	}
	if redeemer.called() != 1 {
		t.Fatalf("redeemer called %d times", redeemer.called())
	}
}

// A form-encoded body is the one a cross-site form can send without a preflight.
// Requiring JSON means any request that gets here was made by script.
func TestRedeemRefusesANonJSONContentType(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	headers := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}
	res := post(a, redeemPath, headers, map[string]any{"credentialId": codexID, "confirmed": true})
	if res.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if redeemer.called() != 0 {
		t.Fatal("a form-shaped request reached the redeemer")
	}
}

func TestRedeemRejectsAnOversizedOrMalformedBody(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	oversized := protocol.ManagementRequest{
		Method: "POST", Path: redeemPath,
		Headers: http.Header{"Content-Type": {"application/json"}},
		Body:    make([]byte, 5000),
	}
	if res := a.Handle(oversized, time.Now()); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized body: %d", res.StatusCode)
	}
	malformed := oversized
	malformed.Body = []byte(`{"credentialId":`)
	if res := a.Handle(malformed, time.Now()); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed body: %d", res.StatusCode)
	}
	if redeemer.called() != 0 {
		t.Fatal("a malformed request reached the redeemer")
	}
}

// Provider error text is unbounded input. None of it may reach a dashboard or a
// log, so every failure is reported as one of this plugin's own fixed codes.
func TestRedeemForwardsNoProviderText(t *testing.T) {
	secret := "upstream said: account 12345 token sk-synthetic-abcdef"
	for _, err := range []error{
		errors.New(secret),
		redeem.ErrRefused,
		redeem.ErrUnavailable,
		redeem.ErrInFlight,
		redeem.ErrUnsupportedProvider,
		redeem.ErrNoAccessToken,
		redeem.ErrNoAccountID,
		redeem.ErrOutcomeUnknown,
		redeem.ErrRateLimited,
		redeem.ErrIdentityChanged,
		redeem.ErrRetryWindowClosed,
		&redeem.OutcomeUnknownError{RetryUntil: time.Unix(1789013400, 0)},
	} {
		a, redeemer := redeemableAPI(t)
		redeemer.err = err
		res := post(a, redeemPath, nil, map[string]any{"credentialId": codexID, "confirmed": true})
		if res.StatusCode < 400 {
			t.Fatalf("%v: status = %d, want a failure", err, res.StatusCode)
		}
		body := decodeBody(t, res)
		code, _ := body["error"].(string)
		if code == "" || strings.Contains(string(res.Body), "sk-synthetic") || strings.Contains(string(res.Body), "12345") {
			t.Fatalf("%v: body = %s", err, res.Body)
		}
		// A code, and beside outcome_unknown the deadline the plugin computed
		// itself. Nothing else.
		for key, value := range body {
			switch {
			case key == "error":
			case key == "retryUntilEpoch" && code == "outcome_unknown":
				if _, number := value.(float64); value != nil && !number {
					t.Fatalf("%v: retryUntilEpoch = %v", err, value)
				}
			default:
				t.Fatalf("%v: the failure carried more than a code: %v", err, body)
			}
		}
	}
}

// outcome_unknown names the instant the claim stops being repeated, as the
// redeemer reports it; without one it is null, never a guessed window.
func TestOutcomeUnknownNamesTheClaimsDeadline(t *testing.T) {
	deadline := time.Date(2026, 10, 5, 12, 10, 0, 0, time.UTC)
	for name, one := range map[string]struct {
		err  error
		want any
	}{
		"with a deadline": {&redeem.OutcomeUnknownError{RetryUntil: deadline}, float64(deadline.Unix())},
		"wrapped":         {fmt.Errorf("claim: %w", &redeem.OutcomeUnknownError{RetryUntil: deadline}), float64(deadline.Unix())},
		"bare sentinel":   {redeem.ErrOutcomeUnknown, nil},
	} {
		a, redeemer := redeemableAPI(t)
		redeemer.err = one.err
		body := decodeBody(t, post(a, redeemPath, nil, map[string]any{"credentialId": codexID, "confirmed": true}))
		got, present := body["retryUntilEpoch"]
		if body["error"] != "outcome_unknown" || !present || got != one.want {
			t.Fatalf("%s: body = %v, want retryUntilEpoch %v", name, body, one.want)
		}
	}
}

func TestRedeemMapsFailuresToStatuses(t *testing.T) {
	for _, one := range []struct {
		err    error
		status int
		code   string
	}{
		{redeem.ErrInFlight, http.StatusConflict, "already_in_flight"},
		{redeem.ErrUnsupportedProvider, http.StatusConflict, "not_redeemable"},
		{redeem.ErrNoAccessToken, http.StatusConflict, "credential_unusable"},
		{redeem.ErrNoAccountID, http.StatusConflict, "credential_unusable"},
		{redeem.ErrIdentityChanged, http.StatusConflict, "credential_unusable"},
		{redeem.ErrRefused, http.StatusBadGateway, "provider_refused"},
		{redeem.ErrUnavailable, http.StatusBadGateway, "provider_unavailable"},
		// The two the client has to tell apart from a plain failure: one
		// spent nothing and says why, the other may have spent a reset.
		{redeem.ErrRateLimited, http.StatusBadGateway, "provider_rate_limited"},
		{redeem.ErrOutcomeUnknown, http.StatusBadGateway, "outcome_unknown"},
		// Wrapped, as a future caller might return it.
		{fmt.Errorf("claim: %w", redeem.ErrOutcomeUnknown), http.StatusBadGateway, "outcome_unknown"},
		// Still unknown, but no longer repeatable: a 5xx, so a client that does
		// not know the code still reads it as "may have been spent".
		{redeem.ErrRetryWindowClosed, http.StatusBadGateway, "retry_window_closed"},
	} {
		a, redeemer := redeemableAPI(t)
		redeemer.err = one.err
		res := post(a, redeemPath, nil, map[string]any{"credentialId": codexID, "confirmed": true})
		if res.StatusCode != one.status {
			t.Fatalf("%v: status = %d, want %d", one.err, res.StatusCode, one.status)
		}
		if got := decodeBody(t, res)["error"]; got != one.code {
			t.Fatalf("%v: code = %v, want %q", one.err, got, one.code)
		}
	}
}

// A disabled plugin must not spend anything. It is off, and off has to mean off
// for the routes that change the world.
func TestRedeemIsClosedWhileThePluginIsDisabled(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	a.Disable()
	res := post(a, redeemPath, nil, map[string]any{"credentialId": codexID, "confirmed": true})
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if redeemer.called() != 0 {
		t.Fatal("a disabled plugin spent a credit")
	}
}

// Read routes stay read-only. A POST to any of them is a 404, never an action.
func TestOnlyTheRedeemPathAcceptsAPost(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	for _, path := range []string{
		summaryPath,
		tokenPath,
		"/v0/management/plugins/quota-glance/health",
		"/v0/management/plugins/quota-glance/windows",
		"/v0/resource/plugins/quota-glance/app",
	} {
		headers := http.Header{"Authorization": {"Bearer " + testToken}, "Content-Type": {"application/json"}}
		res := post(a, path, headers, map[string]any{"credentialId": codexID, "confirmed": true})
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("POST %s = %d, want 404", path, res.StatusCode)
		}
	}
	if redeemer.called() != 0 {
		t.Fatal("a POST to a read route reached the redeemer")
	}
}

// And the redeem path is not a read route either: a GET must not reach it.
func TestRedeemPathDoesNotAnswerAGet(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	for _, path := range []string{redeemPath, redeemTokenPath} {
		if res := get(a, path, bearer(testToken)); res.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", path, res.StatusCode)
		}
	}
	if redeemer.called() != 0 {
		t.Fatal("a GET reached the redeemer")
	}
}

// Failed token attempts on this route are throttled like every other failed
// attempt, and a correct token is never throttled.
func TestRedeemThrottlesFailedTokenAttempts(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	body := map[string]any{"credentialId": codexID, "confirmed": true}
	now := time.Now()
	throttled := false
	for range failureLimit + 5 {
		raw, _ := json.Marshal(body)
		res := a.Handle(protocol.ManagementRequest{
			Method: "POST", Path: redeemTokenPath,
			Headers: http.Header{"Authorization": {"Bearer wrong"}, "Content-Type": {"application/json"}},
			Body:    raw,
		}, now)
		if res.StatusCode == http.StatusTooManyRequests {
			throttled = true
		}
	}
	if !throttled {
		t.Fatal("failed attempts against the redeem route are never throttled")
	}
	// The operator's own correct token still gets through.
	headers := http.Header{"Authorization": {"Bearer " + testToken}, "Content-Type": {"application/json"}}
	raw, _ := json.Marshal(body)
	res := a.Handle(protocol.ManagementRequest{Method: "POST", Path: redeemTokenPath, Headers: headers, Body: raw}, now)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("a correct token was throttled: %d", res.StatusCode)
	}
	if redeemer.called() != 1 {
		t.Fatalf("redeemer called %d times", redeemer.called())
	}
}

// A Claude credential the document offers is redeemed exactly as a Codex one
// is, and the answer names the provider.
func TestRedeemSpendsAClaudeReset(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	redeemer.result = redeem.Result{Outcome: redeem.OutcomeReset, WindowsReset: 2, RemainingCount: 2}
	res := post(a, redeemPath, nil, map[string]any{"credentialId": claudeID, "confirmed": true})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.StatusCode, res.Body)
	}
	body := decodeBody(t, res)
	if body["provider"] != "claude" || body["outcome"] != redeem.OutcomeReset || body["remainingCount"] != 2.0 {
		t.Fatalf("body = %v", body)
	}
	if redeemer.provider != "claude" {
		t.Fatalf("redeemer was asked for %q", redeemer.provider)
	}
}

// A hold on the card is a hint, never a gate: the provider is asked afresh, so
// a credential the last poll saw in cooldown still reaches the redeemer.
func TestAHoldDoesNotStopAPress(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	if res := post(a, redeemPath, nil, map[string]any{"credentialId": claudeID, "confirmed": true}); res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if redeemer.called() != 1 {
		t.Fatal("a credential holding a reset was refused on the strength of a hint")
	}
}

// Every outcome that is an answer — spent or not — is a 200 carrying the
// outcome. Only the failures to answer are errors.
func TestRedeemPassesEveryOutcomeThrough(t *testing.T) {
	for _, outcome := range []string{
		redeem.OutcomeReset, redeem.OutcomeNothingToReset, redeem.OutcomeNoCredit, redeem.OutcomeFailed,
		redeem.OutcomeNotLimited, redeem.OutcomeCooldown, redeem.OutcomePaused, redeem.OutcomeIneligible,
		redeem.OutcomeAlreadyUsed,
	} {
		a, redeemer := redeemableAPI(t)
		redeemer.result = redeem.Result{Outcome: outcome, RemainingCount: 3}
		res := post(a, redeemPath, nil, map[string]any{"credentialId": claudeID, "confirmed": true})
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d", outcome, res.StatusCode)
		}
		body := decodeBody(t, res)
		if body["outcome"] != outcome || body["snapshotPending"] != true || body["provider"] != "claude" {
			t.Fatalf("%s: body = %v", outcome, body)
		}
	}
}

// The press is bounded by the redeemer's own bound, which sits under the
// browser's 65 seconds.
func TestRedeemGivesThePressItsWholeBound(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	post(a, redeemPath, nil, map[string]any{"credentialId": codexID, "confirmed": true})
	if redeemer.deadline <= redeem.Timeout-5*time.Second || redeemer.deadline > redeem.Timeout {
		t.Fatalf("the press had %v, want about %v", redeemer.deadline, redeem.Timeout)
	}
	if redeem.Timeout >= 65*time.Second {
		t.Fatalf("redeem.Timeout = %v outlasts the browser's wait", redeem.Timeout)
	}
}

// A press id on the POST door is optional. With one, a second copy of the same
// press is answered from the ledger instead of being spent again.
func TestRedeemPressIDMakesARepeatedPOSTHarmless(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	body := map[string]any{"credentialId": codexID, "confirmed": true, "pressId": pressID(1)}
	first := post(a, redeemPath, nil, body)
	second := post(a, redeemPath, nil, body)
	if redeemer.called() != 1 {
		t.Fatalf("one press spent %d times", redeemer.called())
	}
	if first.StatusCode != http.StatusOK || second.StatusCode != first.StatusCode || string(second.Body) != string(first.Body) {
		t.Fatalf("first %d %s, second %d %s", first.StatusCode, first.Body, second.StatusCode, second.Body)
	}
	if first.Headers.Get(replayedHeader) != "" || second.Headers.Get(replayedHeader) != "1" {
		t.Fatalf("replay marks: first %q, second %q", first.Headers.Get(replayedHeader), second.Headers.Get(replayedHeader))
	}
	// The token door's POST keeps the same ledger.
	headers := http.Header{"Authorization": {"Bearer " + testToken}}
	if res := post(a, redeemTokenPath, headers, body); res.Headers.Get(replayedHeader) != "1" || redeemer.called() != 1 {
		t.Fatalf("token door: replayed=%q calls=%d", res.Headers.Get(replayedHeader), redeemer.called())
	}
}

// A press id that is present must be well formed, on this door as on /spend.
func TestRedeemRefusesAnInvalidPressID(t *testing.T) {
	for name, id := range map[string]any{
		"empty":        "",
		"too short":    strings.Repeat("a", 15),
		"too long":     strings.Repeat("a", 65),
		"bad chars":    "press id with spaces",
		"not a string": 1234567890123456,
	} {
		a, redeemer := redeemableAPI(t)
		res := post(a, redeemPath, nil, map[string]any{"credentialId": codexID, "confirmed": true, "pressId": id})
		if res.StatusCode != http.StatusBadRequest || decodeBody(t, res)["error"] != "invalid_request" {
			t.Fatalf("%s: %d %s, want 400 invalid_request", name, res.StatusCode, res.Body)
		}
		if redeemer.called() != 0 || len(a.ledger.entries) != 0 {
			t.Fatalf("%s: reached the redeemer or the ledger", name)
		}
	}
	// Confirmation is still checked first.
	a, _ := redeemableAPI(t)
	res := post(a, redeemPath, nil, map[string]any{"credentialId": codexID, "confirmed": false, "pressId": "bad"})
	if decodeBody(t, res)["error"] != "confirmation_required" {
		t.Fatalf("unconfirmed with a bad press id: %s", res.Body)
	}
}

// Without a press id the POST behaves exactly as it did before press ids
// existed, so a page cached from an older release keeps working: every press
// is its own, and nothing is remembered.
func TestRedeemWithoutAPressIDBehavesAsBefore(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	for range 2 {
		for _, body := range []map[string]any{
			{"credentialId": codexID, "confirmed": true},
			{"credentialId": codexID, "confirmed": true, "pressId": nil},
		} {
			res := post(a, redeemPath, nil, body)
			if res.StatusCode != http.StatusOK || res.Headers.Get(replayedHeader) != "" {
				t.Fatalf("status = %d, replayed = %q", res.StatusCode, res.Headers.Get(replayedHeader))
			}
		}
	}
	if redeemer.called() != 4 || len(a.ledger.entries) != 0 {
		t.Fatalf("calls = %d, ledger entries = %d", redeemer.called(), len(a.ledger.entries))
	}
}
