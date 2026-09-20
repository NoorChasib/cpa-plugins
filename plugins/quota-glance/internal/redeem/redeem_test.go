package redeem

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
)

// fakeHost answers per URL and records everything, so a test can assert not
// only what came back but whether the POST was made at all. For an irreversible
// action that second question is the one that matters.
type fakeHost struct {
	mu       sync.Mutex
	auth     string
	authErr  error
	bodies   map[string]string
	status   map[string]int
	errs     map[string]error
	requests []protocol.HostHTTPRequest
}

func (h *fakeHost) GetAuth(context.Context, string) ([]byte, error) {
	if h.authErr != nil {
		return nil, h.authErr
	}
	if h.auth == "" {
		return []byte(`{"access_token":"synthetic-token","account_id":"synthetic-account"}`), nil
	}
	return []byte(h.auth), nil
}

func (h *fakeHost) HTTPDo(_ context.Context, request protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.requests = append(h.requests, request)
	if err := h.errs[request.URL]; err != nil {
		return protocol.HostHTTPResponse{}, err
	}
	status := h.status[request.URL]
	if status == 0 {
		status = 200
	}
	return protocol.HostHTTPResponse{StatusCode: status, Body: []byte(h.bodies[request.URL])}, nil
}

func (h *fakeHost) posted() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, request := range h.requests {
		if request.Method == "POST" {
			return true
		}
	}
	return false
}

func (h *fakeHost) postBody(t *testing.T) map[string]string {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, request := range h.requests {
		if request.Method != "POST" {
			continue
		}
		var body map[string]string
		if json.Unmarshal(request.Body, &body) != nil {
			t.Fatalf("POST body is not JSON: %q", request.Body)
		}
		return body
	}
	t.Fatal("no POST was made")
	return nil
}

const oneAvailable = `{"available_count":1,"credits":[{"id":"credit-a","status":"available","expires_at":"2026-10-01T00:00:00Z"}]}`

func hostWith(bodies map[string]string) *fakeHost {
	return &fakeHost{bodies: bodies, status: map[string]int{}, errs: map[string]error{}}
}

func TestRedeemSpendsOneCreditAndReportsTheOutcome(t *testing.T) {
	host := hostWith(map[string]string{
		creditsURL: `{"available_count":2,"credits":[` +
			`{"id":"credit-a","status":"available","expires_at":"2026-10-01T00:00:00Z"},` +
			`{"id":"credit-b","status":"available","expires_at":"2026-11-01T00:00:00Z"}]}`,
		consumeURL: `{"code":"reset","windows_reset":2,"credit":{"id":"credit-a","status":"redeemed"}}`,
	})
	result, err := New(host).Redeem(context.Background(), "codex", "codex-noor@example.com.json")
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutcomeReset || result.WindowsReset != 2 || result.RemainingCount != 1 {
		t.Fatalf("result = %+v", result)
	}
	body := host.postBody(t)
	// The credit closest to lapsing is the one worth spending first.
	if body["credit_id"] != "credit-a" {
		t.Fatalf("spent %q, want the soonest-expiring credit", body["credit_id"])
	}
	if len(body["redeem_request_id"]) != 36 {
		t.Fatalf("redeem_request_id = %q, want a uuid-shaped key", body["redeem_request_id"])
	}
}

// Nothing available means no POST. A request built from a stale count is how a
// credit gets spent that the operator no longer had.
func TestRedeemMakesNoRequestWhenNothingIsAvailable(t *testing.T) {
	for name, inventory := range map[string]string{
		"empty list":     `{"available_count":0,"credits":[]}`,
		"all redeemed":   `{"credits":[{"id":"credit-a","status":"redeemed"}]}`,
		"all expired":    `{"credits":[{"id":"credit-a","status":"expired"}]}`,
		"no credits key": `{"available_count":3}`,
	} {
		host := hostWith(map[string]string{creditsURL: inventory})
		result, err := New(host).Redeem(context.Background(), "codex", "codex-noor@example.com.json")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if result.Outcome != OutcomeNoCredit {
			t.Fatalf("%s: outcome = %q", name, result.Outcome)
		}
		if host.posted() {
			t.Fatalf("%s: a credit was spent with none available", name)
		}
	}
}

// A provider that accepted the POST has taken the credit. Whatever it said
// afterwards, reporting failure would invite a second press and a second
// credit, so an unreadable or unfamiliar 2xx body is still a reset.
func TestRedeemTreatsAnyAcceptedPostAsSpent(t *testing.T) {
	for name, body := range map[string]string{
		"unreadable body": `not json at all`,
		"unknown code":    `{"code":"something_new"}`,
		"empty object":    `{}`,
	} {
		host := hostWith(map[string]string{creditsURL: oneAvailable, consumeURL: body})
		result, err := New(host).Redeem(context.Background(), "codex", "codex-noor@example.com.json")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if result.Outcome != OutcomeReset {
			t.Fatalf("%s: outcome = %q, want a spent credit reported as spent", name, result.Outcome)
		}
	}
}

func TestRedeemMapsProviderCodes(t *testing.T) {
	for code, want := range map[string]string{
		"reset":            OutcomeReset,
		"nothing_to_reset": OutcomeNothingToReset,
		"no_credit":        OutcomeNoCredit,
		"already_redeemed": OutcomeNoCredit,
	} {
		host := hostWith(map[string]string{
			creditsURL: oneAvailable,
			consumeURL: `{"code":"` + code + `"}`,
		})
		result, err := New(host).Redeem(context.Background(), "codex", "codex-noor@example.com.json")
		if err != nil {
			t.Fatalf("%s: %v", code, err)
		}
		if result.Outcome != want {
			t.Fatalf("code %q mapped to %q, want %q", code, result.Outcome, want)
		}
	}
}

// Only Codex has banked resets. A credential on any other provider must not
// produce a request at all, whatever the caller claims about it.
func TestRedeemRefusesEveryOtherProvider(t *testing.T) {
	for _, provider := range []string{"claude", "xai", "gemini", ""} {
		host := hostWith(map[string]string{creditsURL: oneAvailable, consumeURL: `{"code":"reset"}`})
		if _, err := New(host).Redeem(context.Background(), provider, "some-credential"); !errors.Is(err, ErrNotCodex) {
			t.Fatalf("provider %q: err = %v, want ErrNotCodex", provider, err)
		}
		if len(host.requests) != 0 {
			t.Fatalf("provider %q reached the provider", provider)
		}
	}
}

// A provider that refuses the inventory read must not be followed by a POST
// built on a guess.
func TestRedeemStopsAtARefusedInventory(t *testing.T) {
	host := hostWith(map[string]string{consumeURL: `{"code":"reset"}`})
	host.status[creditsURL] = 401
	if _, err := New(host).Redeem(context.Background(), "codex", "c"); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if host.posted() {
		t.Fatal("a credit was spent after the inventory was refused")
	}
}

func TestRedeemReportsAnUnreachableProvider(t *testing.T) {
	host := hostWith(map[string]string{})
	host.errs[creditsURL] = errors.New("dial failed")
	if _, err := New(host).Redeem(context.Background(), "codex", "c"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

// A refused POST is the one case where the credit's fate is genuinely unknown.
// It is reported as a refusal so the operator can look, rather than as a spend.
func TestRedeemReportsARefusedConsume(t *testing.T) {
	host := hostWith(map[string]string{creditsURL: oneAvailable})
	host.status[consumeURL] = 500
	if _, err := New(host).Redeem(context.Background(), "codex", "c"); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
}

// An API-key login has no reset credits and no way to authenticate this
// request. It must fail before any request, naming the cause.
func TestRedeemRefusesACredentialItCannotAuthenticate(t *testing.T) {
	for name, doc := range map[string]struct {
		auth string
		want error
	}{
		"no access token": {`{"account_id":"acct"}`, ErrNoAccessToken},
		"not json":        {`api-key-only`, ErrNoAccessToken},
		"no account id":   {`{"access_token":"tok"}`, ErrNoAccountID},
	} {
		host := hostWith(map[string]string{creditsURL: oneAvailable, consumeURL: `{"code":"reset"}`})
		host.auth = doc.auth
		if _, err := New(host).Redeem(context.Background(), "codex", "c"); !errors.Is(err, doc.want) {
			t.Fatalf("%s: err = %v, want %v", name, err, doc.want)
		}
		if len(host.requests) != 0 {
			t.Fatalf("%s: reached the provider with an unusable credential", name)
		}
	}
}

// The account id is resolvable from the id_token when the document omits it,
// the same way quota-cache resolves it. Disagreeing would mean spending a
// credit on a different account than the card reported.
func TestRedeemResolvesTheAccountFromTheIDToken(t *testing.T) {
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-from-token"}}`))
	host := hostWith(map[string]string{creditsURL: oneAvailable, consumeURL: `{"code":"reset"}`})
	host.auth = `{"tokens":{"access_token":"tok","id_token":"header.` + claims + `.sig"}}`
	if _, err := New(host).Redeem(context.Background(), "codex", "c"); err != nil {
		t.Fatal(err)
	}
	for _, request := range host.requests {
		if got := request.Headers["Chatgpt-Account-Id"]; len(got) != 1 || got[0] != "acct-from-token" {
			t.Fatalf("account header = %v", got)
		}
	}
}

// Every request must be account-scoped and CLI-shaped, or the ChatGPT edge
// rejects it — the same finding quota-cache made on the usage endpoint.
func TestRedeemRequestsCarryTheProviderContract(t *testing.T) {
	host := hostWith(map[string]string{creditsURL: oneAvailable, consumeURL: `{"code":"reset"}`})
	if _, err := New(host).Redeem(context.Background(), "codex", "c"); err != nil {
		t.Fatal(err)
	}
	if len(host.requests) != 2 {
		t.Fatalf("made %d requests, want an inventory read then one consume", len(host.requests))
	}
	for _, request := range host.requests {
		if got := request.Headers["Authorization"]; len(got) != 1 || got[0] != "Bearer synthetic-token" {
			t.Fatalf("authorization = %v", got)
		}
		if got := request.Headers["Chatgpt-Account-Id"]; len(got) != 1 || got[0] != "synthetic-account" {
			t.Fatalf("account header = %v", got)
		}
		if got := request.Headers["User-Agent"]; len(got) != 1 || !strings.HasPrefix(got[0], "codex_cli_rs/") {
			t.Fatalf("user agent = %v", got)
		}
	}
	if got := host.requests[1].Headers["Content-Type"]; len(got) != 1 || got[0] != "application/json" {
		t.Fatalf("consume content type = %v", got)
	}
	// The GET must not claim to carry a body.
	if _, ok := host.requests[0].Headers["Content-Type"]; ok {
		t.Fatal("the inventory read declared a content type")
	}
}

// A credit id is echoed straight back into a request body. An unbounded or
// quote-bearing string from a response has no business being one.
func TestRedeemIgnoresMalformedCreditIDs(t *testing.T) {
	host := hostWith(map[string]string{
		creditsURL: `{"credits":[` +
			`{"id":"` + strings.Repeat("x", 200) + `","status":"available"},` +
			`{"id":"with\"quote","status":"available"},` +
			`{"id":"","status":"available"},` +
			`{"id":"credit-good","status":"available","expires_at":"2026-12-01T00:00:00Z"}]}`,
		consumeURL: `{"code":"reset"}`,
	})
	if _, err := New(host).Redeem(context.Background(), "codex", "c"); err != nil {
		t.Fatal(err)
	}
	if got := host.postBody(t)["credit_id"]; got != "credit-good" {
		t.Fatalf("credit_id = %q", got)
	}
}

// An undated credit is spendable but sorts last: a known deadline is a reason
// to spend one first, and an unknown one is not a reason to spend it sooner.
func TestRedeemPrefersADatedCreditOverAnUndatedOne(t *testing.T) {
	host := hostWith(map[string]string{
		creditsURL: `{"credits":[` +
			`{"id":"undated","status":"available"},` +
			`{"id":"dated","status":"available","expires_at":"2026-12-01T00:00:00Z"}]}`,
		consumeURL: `{"code":"reset"}`,
	})
	if _, err := New(host).Redeem(context.Background(), "codex", "c"); err != nil {
		t.Fatal(err)
	}
	if got := host.postBody(t)["credit_id"]; got != "dated" {
		t.Fatalf("credit_id = %q, want the dated credit", got)
	}
}

// Two presses must not become two credits. The second is refused outright,
// because by the time it arrives the first may already have passed its
// inventory check and be about to POST.
func TestRedeemRefusesASecondConcurrentAttempt(t *testing.T) {
	release := make(chan struct{})
	host := &blockingHost{fakeHost: *hostWith(map[string]string{
		creditsURL: oneAvailable,
		consumeURL: `{"code":"reset"}`,
	}), gate: release}
	redeemer := New(host)

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := redeemer.Redeem(context.Background(), "codex", "same-credential")
		done <- err
	}()
	<-started
	host.waitUntilEntered()

	if _, err := redeemer.Redeem(context.Background(), "codex", "same-credential"); !errors.Is(err, ErrInFlight) {
		t.Fatalf("second attempt err = %v, want ErrInFlight", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// And the credential is spendable again once the first attempt finishes.
	if _, err := redeemer.Redeem(context.Background(), "codex", "same-credential"); err != nil {
		t.Fatalf("the credential stayed locked after the attempt finished: %v", err)
	}
}

// A different credential is a different account and is not blocked by the
// first one's attempt.
func TestRedeemLocksOneCredentialNotAllOfThem(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	host := &blockingHost{fakeHost: *hostWith(map[string]string{
		creditsURL: oneAvailable,
		consumeURL: `{"code":"reset"}`,
	}), gate: release}
	redeemer := New(host)
	go func() { _, _ = redeemer.Redeem(context.Background(), "codex", "credential-one") }()
	host.waitUntilEntered()

	// The second credential proceeds; it blocks on the gate rather than on the
	// first credential's claim, so reaching the host at all is the assertion.
	go func() { _, _ = redeemer.Redeem(context.Background(), "codex", "credential-two") }()
	host.waitForRequests(2)
}

// A redeemer with no host cannot make a request, which is what "switched off"
// has to mean for an irreversible action.
func TestRedeemerWithoutAHostIsInert(t *testing.T) {
	redeemer := New(nil)
	if redeemer.Enabled() {
		t.Fatal("a redeemer with no host reports itself enabled")
	}
	if _, err := redeemer.Redeem(context.Background(), "codex", "c"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

// Every request id is fresh. Reusing one across two genuine presses would let
// the provider drop the second as a replay.
func TestRequestIDsAreUniqueAndWellShaped(t *testing.T) {
	seen := map[string]bool{}
	for range 500 {
		id, err := requestID()
		if err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatalf("request id %q was generated twice", id)
		}
		seen[id] = true
		parts := strings.Split(id, "-")
		if len(parts) != 5 || len(id) != 36 || parts[2][0] != '4' {
			t.Fatalf("request id %q is not a version 4 uuid", id)
		}
	}
}
