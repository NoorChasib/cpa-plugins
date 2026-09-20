package api

import (
	"context"
	"encoding/json"
	"errors"
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
)

// fakeRedeemer records whether it was asked to spend anything. For a route that
// cannot be undone, "was it called at all" is the assertion that matters most.
type fakeRedeemer struct {
	mu     sync.Mutex
	calls  int
	result redeem.Result
	err    error
}

func (f *fakeRedeemer) Redeem(_ context.Context, _, _ string) (redeem.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.result, f.err
}

func (f *fakeRedeemer) called() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// redeemableAPI serves a document with one spendable Codex credential, one
// Codex credential whose credit the document says is not redeemable, and one
// Claude credential with nothing banked at all.
func redeemableAPI(t *testing.T) (*API, *fakeRedeemer) {
	t.Helper()
	a := New("quota-glance", testToken)
	a.Publish(aggregate.Document{
		SchemaVersion: 1, GeneratedAtEpoch: 1789012800,
		Credentials: []aggregate.Credential{
			{ID: codexID, Provider: "codex", ResetCredits: &aggregate.ResetCredits{AvailableCount: 2, Redeemable: true}},
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
		redeem.ErrNotCodex,
		redeem.ErrNoAccessToken,
		redeem.ErrNoAccountID,
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
		if len(body) != 1 {
			t.Fatalf("%v: the failure carried more than a code: %v", err, body)
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
		{redeem.ErrNotCodex, http.StatusConflict, "not_redeemable"},
		{redeem.ErrNoAccessToken, http.StatusConflict, "credential_unusable"},
		{redeem.ErrNoAccountID, http.StatusConflict, "credential_unusable"},
		{redeem.ErrRefused, http.StatusBadGateway, "provider_refused"},
		{redeem.ErrUnavailable, http.StatusBadGateway, "provider_unavailable"},
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
// for the one route that changes the world.
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
