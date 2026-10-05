package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/aggregate"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/redeem"
)

const spendPath = "/v0/resource/plugins/quota-glance/spend"

// encodePress is what the page puts in the spend header: the POST body,
// base64url without padding.
func encodePress(body any) string {
	raw, _ := json.Marshal(body)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// spendHeaders are the headers of a well-formed press on the token door.
func spendHeaders(body any) http.Header {
	return http.Header{"Authorization": {"Bearer " + testToken}, spendHeader: {encodePress(body)}}
}

func confirmedPress(credentialID, id string) map[string]any {
	return map[string]any{"credentialId": credentialID, "confirmed": true, "pressId": id}
}

func spendGET(a *API, headers http.Header) protocol.ManagementResponse {
	return a.Handle(protocol.ManagementRequest{Method: "GET", Path: spendPath, Headers: headers}, time.Now())
}

// gatedRedeemer holds every press until the test releases it, so copies of a
// press can be made to arrive while the first is still under way.
type gatedRedeemer struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
	result  redeem.Result
}

func newGatedRedeemer() *gatedRedeemer {
	return &gatedRedeemer{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
		result:  redeem.Result{Outcome: redeem.OutcomeReset, WindowsReset: 2, RemainingCount: 1},
	}
}

func (g *gatedRedeemer) Redeem(context.Context, string, string) (redeem.Result, error) {
	g.calls.Add(1)
	select {
	case g.entered <- struct{}{}:
	default:
	}
	<-g.release
	return g.result, nil
}

// The token door spends exactly as the POST door does: one provider call, and
// the same answer byte for byte, so the page reads either the same way.
func TestSpendByGETSpendsOnceAndAnswersAsThePOSTDoes(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	res := spendGET(a, spendHeaders(confirmedPress(codexID, pressID(1))))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.StatusCode, res.Body)
	}
	if redeemer.called() != 1 || redeemer.provider != "codex" {
		t.Fatalf("redeemer called %d times for %q", redeemer.called(), redeemer.provider)
	}
	for name, want := range map[string]string{"Content-Type": "application/json; charset=utf-8", "Cache-Control": "no-store"} {
		if got := res.Headers.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if res.Headers.Get(replayedHeader) != "" {
		t.Fatal("the first copy of a press was marked as a replay")
	}

	viaPOST, _ := redeemableAPI(t)
	posted := post(viaPOST, redeemPath, nil, map[string]any{"credentialId": codexID, "confirmed": true})
	if posted.StatusCode != res.StatusCode || !bytes.Equal(posted.Body, res.Body) {
		t.Fatalf("the doors disagree:\n GET  %d %s\n POST %d %s", res.StatusCode, res.Body, posted.StatusCode, posted.Body)
	}
}

// CPA authenticates nothing on a resource route. Without the web token the
// press is refused bare, exactly as the summary is, and nothing is spent.
func TestSpendRequiresTheTokenAndFailsBare(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	press := encodePress(confirmedPress(codexID, pressID(1)))
	for name, headers := range map[string]http.Header{
		"no token":     {spendHeader: {press}},
		"wrong token":  {spendHeader: {press}, "Authorization": {"Bearer wrong"}},
		"empty bearer": {spendHeader: {press}, "Authorization": {"Bearer "}},
		"no scheme":    {spendHeader: {press}, "Authorization": {testToken}},
	} {
		res := spendGET(a, headers)
		if res.StatusCode != http.StatusUnauthorized || len(res.Body) != 0 {
			t.Fatalf("%s: %d %s, want a bare 401", name, res.StatusCode, res.Body)
		}
		if res.Headers.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: Cache-Control = %q", name, res.Headers.Get("Cache-Control"))
		}
	}
	if redeemer.called() != 0 {
		t.Fatal("an unauthenticated press reached the redeemer")
	}
}

// Failed attempts here count against the same limiter as every other token
// door, and the operator's own token is never throttled.
func TestSpendThrottlesFailedTokenAttemptsButNeverTheRightToken(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	now := time.Now()
	throttled := false
	for range failureLimit + 5 {
		res := a.Handle(protocol.ManagementRequest{Method: "GET", Path: spendPath, Headers: http.Header{
			"Authorization": {"Bearer wrong"}, spendHeader: {encodePress(confirmedPress(codexID, pressID(1)))},
		}}, now)
		if res.StatusCode == http.StatusTooManyRequests {
			throttled = true
			if res.Headers.Get("Retry-After") != "60" {
				t.Fatalf("Retry-After = %q", res.Headers.Get("Retry-After"))
			}
		} else if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d", res.StatusCode)
		}
	}
	if !throttled {
		t.Fatal("failed attempts against the spend route are never throttled")
	}
	res := a.Handle(protocol.ManagementRequest{Method: "GET", Path: spendPath, Headers: spendHeaders(confirmedPress(codexID, pressID(2)))}, now)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("a correct token was throttled: %d", res.StatusCode)
	}
	if redeemer.called() != 1 {
		t.Fatalf("redeemer called %d times", redeemer.called())
	}
}

// The header is the press. Anything that is not exactly one base64url-encoded
// JSON object is refused before the document or the ledger is consulted.
func TestSpendRefusesAMalformedPressHeader(t *testing.T) {
	valid := encodePress(confirmedPress(codexID, pressID(1)))
	// Trailing spaces are valid JSON; they only make sure the encoding needs
	// padding, so the one difference from a valid header is the "=".
	plain := `{"credentialId":"` + codexID + `","confirmed":true,"pressId":"` + pressID(1) + `"}`
	for len(plain)%3 == 0 {
		plain += " "
	}
	padded := base64.URLEncoding.EncodeToString([]byte(plain))
	if !strings.HasSuffix(padded, "=") {
		t.Fatalf("padded = %q carries no padding", padded)
	}
	for name, values := range map[string][]string{
		"missing":                nil,
		"empty":                  {""},
		"oversized":              {encodePress(map[string]any{"credentialId": codexID, "confirmed": true, "pressId": pressID(1), "pad": strings.Repeat("x", 4096)})},
		"standard alphabet":      {base64.StdEncoding.EncodeToString([]byte(`{"credentialId":"???>>>","confirmed":true}`))},
		"padded":                 {padded},
		"not base64":             {"{not base64}"},
		"an array":               {base64.RawURLEncoding.EncodeToString([]byte(`[]`))},
		"null":                   {base64.RawURLEncoding.EncodeToString([]byte(`null`))},
		"a string":               {base64.RawURLEncoding.EncodeToString([]byte(`"press"`))},
		"a number":               {base64.RawURLEncoding.EncodeToString([]byte(`7`))},
		"trailing data":          {base64.RawURLEncoding.EncodeToString([]byte(`{"credentialId":"x","confirmed":true} {}`))},
		"truncated JSON":         {base64.RawURLEncoding.EncodeToString([]byte(`{"credentialId":`))},
		"two copies":             {valid, valid},
		"confirmed not boolean":  {encodePress(map[string]any{"credentialId": codexID, "confirmed": "yes", "pressId": pressID(1)})},
		"credential not string":  {encodePress(map[string]any{"credentialId": 7, "confirmed": true, "pressId": pressID(1)})},
		"the plain JSON instead": {`{"credentialId":"x","confirmed":true}`},
	} {
		a, redeemer := redeemableAPI(t)
		headers := http.Header{"Authorization": {"Bearer " + testToken}}
		for _, value := range values {
			headers.Add(spendHeader, value)
		}
		res := spendGET(a, headers)
		if res.StatusCode != http.StatusBadRequest || decodeBody(t, res)["error"] != "invalid_request" {
			t.Fatalf("%s: %d %s, want 400 invalid_request", name, res.StatusCode, res.Body)
		}
		if redeemer.called() != 0 || len(a.ledger.entries) != 0 {
			t.Fatalf("%s: a malformed press reached the redeemer or the ledger", name)
		}
	}
}

// Spending cannot be undone, so a press that does not say it was confirmed is
// refused, on this door exactly as on the POST.
func TestSpendRefusesAnUnconfirmedPress(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"confirmed false":  {"credentialId": codexID, "confirmed": false, "pressId": pressID(1)},
		"confirmed absent": {"credentialId": codexID, "pressId": pressID(1)},
		"confirmed null":   {"credentialId": codexID, "confirmed": nil, "pressId": pressID(1)},
		// Confirmation is checked before the press id, as on the POST.
		"no press id either": {"credentialId": codexID, "confirmed": false},
	} {
		a, redeemer := redeemableAPI(t)
		res := spendGET(a, spendHeaders(body))
		if res.StatusCode != http.StatusBadRequest || decodeBody(t, res)["error"] != "confirmation_required" {
			t.Fatalf("%s: %d %s, want 400 confirmation_required", name, res.StatusCode, res.Body)
		}
		if redeemer.called() != 0 {
			t.Fatalf("%s: an unconfirmed press was spent", name)
		}
	}
}

// The press id is what makes a resent GET harmless, so on this door it is
// required, and it must look like something drawn at random.
func TestSpendRequiresAWellFormedPressID(t *testing.T) {
	for name, id := range map[string]any{
		"missing":       nil,
		"empty":         "",
		"too short":     strings.Repeat("a", 15),
		"too long":      strings.Repeat("a", 65),
		"a space":       "press id with spaces",
		"dots":          "press.id.with.dots",
		"padding":       "AAAAAAAAAAAAAAAAAAAAAA==",
		"standard b64":  "AAAAAAAAAAAAAAAAAAAA+/",
		"non-ASCII":     "pressé-identifier-0001",
		"a newline":     "press-identifier-0001\n",
		"not a string":  1234567890123456,
		"an object":     map[string]any{"id": pressID(1)},
		"a JSON number": json.Number("12345678901234567"),
	} {
		a, redeemer := redeemableAPI(t)
		body := map[string]any{"credentialId": codexID, "confirmed": true}
		if id != nil {
			body["pressId"] = id
		}
		res := spendGET(a, spendHeaders(body))
		if res.StatusCode != http.StatusBadRequest || decodeBody(t, res)["error"] != "invalid_request" {
			t.Fatalf("%s: %d %s, want 400 invalid_request", name, res.StatusCode, res.Body)
		}
		if redeemer.called() != 0 {
			t.Fatalf("%s: reached the redeemer", name)
		}
	}
	// The bounds themselves are allowed.
	for _, id := range []string{strings.Repeat("A", 16), strings.Repeat("z", 64), "0123456789_-abcdefXYZ0"} {
		a, redeemer := redeemableAPI(t)
		if res := spendGET(a, spendHeaders(confirmedPress(codexID, id))); res.StatusCode != http.StatusOK {
			t.Fatalf("%q: %d %s", id, res.StatusCode, res.Body)
		}
		if redeemer.called() != 1 {
			t.Fatalf("%q: redeemer called %d times", id, redeemer.called())
		}
	}
}

// A browser that says the request came from another site, a navigation or a
// prefetch is refused before the token is even looked at: nothing was guessed,
// so the limiter is not charged, and nothing is spent.
func TestSpendRefusesWhatTheBrowserSaysIsNotThisPage(t *testing.T) {
	for name, extra := range map[string]http.Header{
		"cross-site":         {"Sec-Fetch-Site": {"cross-site"}},
		"same-site":          {"Sec-Fetch-Site": {"same-site"}},
		"none":               {"Sec-Fetch-Site": {"none"}},
		"navigate":           {"Sec-Fetch-Mode": {"navigate"}},
		"no-cors":            {"Sec-Fetch-Mode": {"no-cors"}},
		"document":           {"Sec-Fetch-Dest": {"document"}},
		"iframe":             {"Sec-Fetch-Dest": {"iframe"}},
		"Sec-Purpose":        {"Sec-Purpose": {"prefetch"}},
		"Purpose":            {"Purpose": {"prefetch"}},
		"one bad of two":     {"Sec-Fetch-Site": {"same-origin", "cross-site"}},
		"good site bad mode": {"Sec-Fetch-Site": {"same-origin"}, "Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"empty"}},
	} {
		for _, token := range []string{testToken, "wrong"} {
			a, redeemer := redeemableAPI(t)
			headers := spendHeaders(confirmedPress(codexID, pressID(1)))
			headers.Set("Authorization", "Bearer "+token)
			for key, values := range extra {
				headers[key] = values
			}
			res := spendGET(a, headers)
			if res.StatusCode != http.StatusForbidden || decodeBody(t, res)["error"] != "cross_site" {
				t.Fatalf("%s (%s): %d %s, want 403 cross_site", name, token, res.StatusCode, res.Body)
			}
			if a.limiter.count != 0 {
				t.Fatalf("%s (%s): the refusal was charged to the limiter", name, token)
			}
			if redeemer.called() != 0 || len(a.ledger.entries) != 0 {
				t.Fatalf("%s (%s): reached the redeemer or the ledger", name, token)
			}
		}
	}
}

// What this page's own fetch sends is let through, and so is a request that
// sends none of these headers: a plain-HTTP origin never does.
func TestSpendAdmitsWhatThisPageSends(t *testing.T) {
	for name, extra := range map[string]http.Header{
		"none of them": {},
		"cors fetch":   {"Sec-Fetch-Site": {"same-origin"}, "Sec-Fetch-Mode": {"cors"}, "Sec-Fetch-Dest": {"empty"}},
		"same-origin fetch": {
			"Sec-Fetch-Site": {"same-origin"}, "Sec-Fetch-Mode": {"same-origin"}, "Sec-Fetch-Dest": {"empty"},
		},
		"only the site": {"Sec-Fetch-Site": {"same-origin"}},
	} {
		a, redeemer := redeemableAPI(t)
		headers := spendHeaders(confirmedPress(codexID, pressID(1)))
		for key, values := range extra {
			headers[key] = values
		}
		if res := spendGET(a, headers); res.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", name, res.StatusCode, res.Body)
		}
		if redeemer.called() != 1 {
			t.Fatalf("%s: redeemer called %d times", name, redeemer.called())
		}
	}
}

// A request in TLS early data can be replayed by anyone on the path before the
// handshake proves who sent it. RFC 8470 treats any instance of the field as 1.
func TestSpendRefusesEarlyData(t *testing.T) {
	for _, value := range []string{"1", "0", ""} {
		a, redeemer := redeemableAPI(t)
		headers := spendHeaders(confirmedPress(codexID, pressID(1)))
		headers["Early-Data"] = []string{value}
		res := spendGET(a, headers)
		if res.StatusCode != http.StatusTooEarly || decodeBody(t, res)["error"] != "too_early" {
			t.Fatalf("Early-Data %q: %d %s, want 425 too_early", value, res.StatusCode, res.Body)
		}
		if redeemer.called() != 0 || a.limiter.count != 0 {
			t.Fatalf("Early-Data %q: reached the redeemer or the limiter", value)
		}
	}
}

// With redemption switched off the route is gone, but only to a caller that
// proved who it is: the token is checked first, as everywhere on this tree.
func TestSpendIsAbsentWithoutARedeemerOnceAuthenticated(t *testing.T) {
	a, _ := redeemableAPI(t)
	a.SetRedeemer(nil)
	headers := spendHeaders(confirmedPress(codexID, pressID(1)))
	if res := spendGET(a, http.Header{spendHeader: headers.Values(spendHeader)}); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d, want 401", res.StatusCode)
	}
	res := spendGET(a, headers)
	if res.StatusCode != http.StatusNotFound || decodeBody(t, res)["error"] != "not_found" {
		t.Fatalf("authenticated: %d %s, want 404 not_found", res.StatusCode, res.Body)
	}
}

func TestSpendIsClosedWhileThePluginIsDisabled(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	a.Disable()
	if res := spendGET(a, spendHeaders(confirmedPress(codexID, pressID(1)))); res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.StatusCode)
	}
	if redeemer.called() != 0 {
		t.Fatal("a disabled plugin spent a credit")
	}
}

// The second copy of a press is told what the first was told, marked as a
// replay, and spends nothing.
func TestARepeatedPressSpendsOnceAndIsToldTheSameThing(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	headers := spendHeaders(confirmedPress(codexID, pressID(1)))
	first := spendGET(a, headers)
	second := spendGET(a, headers)
	if redeemer.called() != 1 {
		t.Fatalf("one press spent %d times", redeemer.called())
	}
	if first.StatusCode != http.StatusOK || second.StatusCode != first.StatusCode || !bytes.Equal(first.Body, second.Body) {
		t.Fatalf("first %d %s, second %d %s", first.StatusCode, first.Body, second.StatusCode, second.Body)
	}
	if first.Headers.Get(replayedHeader) != "" || second.Headers.Get(replayedHeader) != "1" {
		t.Fatalf("replay marks: first %q, second %q", first.Headers.Get(replayedHeader), second.Headers.Get(replayedHeader))
	}
	if second.Headers.Get("Cache-Control") != "no-store" || second.Headers.Get("Content-Type") != first.Headers.Get("Content-Type") {
		t.Fatalf("replay headers = %v", second.Headers)
	}
}

// Eight copies of one press at once, as a resending transport might deliver
// them: one provider call, one answer, and every copy but the first marked.
func TestConcurrentCopiesOfAPressSpendOnce(t *testing.T) {
	a, _ := redeemableAPI(t)
	gate := newGatedRedeemer()
	a.SetRedeemer(gate)
	var wg sync.WaitGroup
	got := make([]protocol.ManagementResponse, 8)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = spendGET(a, spendHeaders(confirmedPress(codexID, pressID(1))))
		}()
	}
	<-gate.entered
	// Give the other copies time to arrive while the first is under way. The
	// assertions hold either way; this only makes the in-flight wait the path
	// most of them take.
	time.Sleep(20 * time.Millisecond)
	close(gate.release)
	wg.Wait()
	if gate.calls.Load() != 1 {
		t.Fatalf("one press spent %d times", gate.calls.Load())
	}
	replayed := 0
	for _, res := range got {
		if res.StatusCode != http.StatusOK || !bytes.Equal(res.Body, got[0].Body) {
			t.Fatalf("answers differ: %d %s vs %s", res.StatusCode, res.Body, got[0].Body)
		}
		if res.Headers.Get(replayedHeader) == "1" {
			replayed++
		}
	}
	if replayed != 7 {
		t.Fatalf("%d answers marked as replays, want 7", replayed)
	}
}

// A press id names one decision about one account. Reusing it for another is
// refused rather than spent.
func TestAPressIDCannotBeReusedForAnotherCredential(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	spendGET(a, spendHeaders(confirmedPress(codexID, pressID(1))))
	res := spendGET(a, spendHeaders(confirmedPress(claudeID, pressID(1))))
	if res.StatusCode != http.StatusBadRequest || decodeBody(t, res)["error"] != "invalid_request" {
		t.Fatalf("status = %d %s, want 400 invalid_request", res.StatusCode, res.Body)
	}
	if redeemer.called() != 1 {
		t.Fatalf("redeemer called %d times", redeemer.called())
	}
}

// A press that spent the last reset leaves a document that no longer offers
// the account. A copy of it arriving after that is still owed the first
// answer, not a refusal that would read as "nothing happened".
func TestAReplayOutlivesTheDocumentDroppingTheCredential(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	first := spendGET(a, spendHeaders(confirmedPress(codexID, pressID(1))))
	a.Publish(aggregate.Document{
		SchemaVersion: 1,
		Credentials:   []aggregate.Credential{{ID: codexID, Provider: "codex"}},
		Providers:     []aggregate.Provider{},
	}, Health{})
	replay := spendGET(a, spendHeaders(confirmedPress(codexID, pressID(1))))
	if replay.StatusCode != http.StatusOK || !bytes.Equal(replay.Body, first.Body) || replay.Headers.Get(replayedHeader) != "1" {
		t.Fatalf("replay = %d %s %v", replay.StatusCode, replay.Body, replay.Headers)
	}
	// A new press, by contrast, meets the document as it now is.
	fresh := spendGET(a, spendHeaders(confirmedPress(codexID, pressID(2))))
	if fresh.StatusCode != http.StatusConflict || decodeBody(t, fresh)["error"] != "not_redeemable" {
		t.Fatalf("new press = %d %s, want 409 not_redeemable", fresh.StatusCode, fresh.Body)
	}
	if redeemer.called() != 1 {
		t.Fatalf("redeemer called %d times", redeemer.called())
	}
}

// An unknown outcome is an answer like any other to the ledger. The same press
// is told it again without the provider being asked a second time; resolving
// it is the journal's job, for a new press.
func TestAStoredUnknownOutcomeReplaysWithoutASecondProviderCall(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	redeemer.err = &redeem.OutcomeUnknownError{RetryUntil: time.Unix(1789013400, 0)}
	first := spendGET(a, spendHeaders(confirmedPress(codexID, pressID(1))))
	if first.StatusCode != http.StatusBadGateway || decodeBody(t, first)["error"] != "outcome_unknown" {
		t.Fatalf("first = %d %s", first.StatusCode, first.Body)
	}
	replay := spendGET(a, spendHeaders(confirmedPress(codexID, pressID(1))))
	if replay.StatusCode != first.StatusCode || !bytes.Equal(replay.Body, first.Body) || replay.Headers.Get(replayedHeader) != "1" {
		t.Fatalf("replay = %d %s %v", replay.StatusCode, replay.Body, replay.Headers)
	}
	if redeemer.called() != 1 {
		t.Fatalf("the provider was asked %d times", redeemer.called())
	}
	// A new press does reach the redeemer, where the journal repeats the claim.
	spendGET(a, spendHeaders(confirmedPress(codexID, pressID(2))))
	if redeemer.called() != 2 {
		t.Fatalf("a new press after an unknown outcome reached the redeemer %d times in all", redeemer.called())
	}
}

// A copy that is still waiting when the wait runs out is told the press is
// under way. It spends nothing, and the original still finishes.
func TestAReplayStillInFlightPastTheWaitIsRefused(t *testing.T) {
	a, _ := redeemableAPI(t)
	gate := newGatedRedeemer()
	a.SetRedeemer(gate)
	a.ledger.wait = 20 * time.Millisecond
	original := make(chan protocol.ManagementResponse, 1)
	go func() { original <- spendGET(a, spendHeaders(confirmedPress(codexID, pressID(1)))) }()
	<-gate.entered

	// Bounded, so a copy that reaches the held redeemer fails the test rather
	// than hanging it.
	copied := make(chan protocol.ManagementResponse, 1)
	go func() { copied <- spendGET(a, spendHeaders(confirmedPress(codexID, pressID(1)))) }()
	var res protocol.ManagementResponse
	select {
	case res = <-copied:
	case <-time.After(5 * time.Second):
		close(gate.release)
		t.Fatal("the copy reached the redeemer instead of waiting on the ledger")
	}
	if res.StatusCode != http.StatusConflict || decodeBody(t, res)["error"] != "already_in_flight" {
		t.Fatalf("status = %d %s, want 409 already_in_flight", res.StatusCode, res.Body)
	}
	if res.Headers.Get(replayedHeader) != "" {
		t.Fatal("a refusal that is not the stored answer was marked as a replay")
	}
	close(gate.release)
	if first := <-original; first.StatusCode != http.StatusOK {
		t.Fatalf("original = %d", first.StatusCode)
	}
	if gate.calls.Load() != 1 {
		t.Fatalf("redeemer called %d times", gate.calls.Load())
	}
}

// Ten minutes after its answer a press is forgotten, and the same id is then a
// new press.
func TestAPressIsForgottenTenMinutesAfterItsAnswer(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	clock := newFakeClock()
	a.ledger.clock = clock.read
	headers := spendHeaders(confirmedPress(codexID, pressID(1)))
	spendGET(a, headers)

	clock.advance(ledgerTTL - time.Second)
	if res := spendGET(a, headers); res.Headers.Get(replayedHeader) != "1" || redeemer.called() != 1 {
		t.Fatalf("inside the window: replayed=%q calls=%d", res.Headers.Get(replayedHeader), redeemer.called())
	}
	clock.advance(time.Second)
	res := spendGET(a, headers)
	if res.StatusCode != http.StatusOK || res.Headers.Get(replayedHeader) != "" || redeemer.called() != 2 {
		t.Fatalf("after the window: %d replayed=%q calls=%d", res.StatusCode, res.Headers.Get(replayedHeader), redeemer.called())
	}
}

// With every remembered press still in flight there is nothing safe to forget,
// so a new press is refused as in flight rather than admitted by evicting one.
// Finishing does not make room on its own either: the finished press is
// remembered for its ten minutes, and only then is its place free.
func TestAFullLedgerOfPressesInFlightRefusesANewOne(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	clock := newFakeClock()
	a.ledger.clock = clock.read
	entries := make([]*pressEntry, ledgerCapacity)
	for i := range ledgerCapacity {
		entry, _, err := a.ledger.begin(pressID(1000+i), "someone-else.json")
		if err != nil {
			t.Fatal(err)
		}
		entries[i] = entry
	}
	res := spendGET(a, spendHeaders(confirmedPress(codexID, pressID(1))))
	if res.StatusCode != http.StatusConflict || decodeBody(t, res)["error"] != "already_in_flight" {
		t.Fatalf("status = %d %s, want 409 already_in_flight", res.StatusCode, res.Body)
	}
	if redeemer.called() != 0 {
		t.Fatal("a press was spent without a place in the ledger")
	}
	a.ledger.finish(entries[0], jsonResponse(http.StatusOK, map[string]string{}))
	if res := spendGET(a, spendHeaders(confirmedPress(codexID, pressID(1)))); res.StatusCode != http.StatusConflict || redeemer.called() != 0 {
		t.Fatalf("a press finished moments ago made room: %d %s", res.StatusCode, res.Body)
	}
	clock.advance(ledgerTTL)
	if res := spendGET(a, spendHeaders(confirmedPress(codexID, pressID(1)))); res.StatusCode != http.StatusOK {
		t.Fatalf("ten minutes after one finished: %d %s", res.StatusCode, res.Body)
	}
}

// A press that spent stays single-use for its whole ten minutes however many
// presses follow it. Presses naming an account the document does not offer
// cost nothing to send and answer at once, so a caller holding the token can
// make the ledger full in moments; that must leave the earlier press
// remembered, and a copy of it arriving afterwards must be told what it did
// rather than spend a second reset.
func TestAPressStaysSingleUseHoweverManyPressesFollowIt(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	headers := spendHeaders(confirmedPress(codexID, pressID(1)))
	first := spendGET(a, headers)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first = %d %s", first.StatusCode, first.Body)
	}
	for i := range ledgerCapacity {
		res := spendGET(a, spendHeaders(confirmedPress("not-offered.json", pressID(10_000+i))))
		if res.StatusCode != http.StatusConflict {
			t.Fatalf("press %d = %d %s, want a 409", i, res.StatusCode, res.Body)
		}
	}
	copied := spendGET(a, headers)
	if redeemer.called() != 1 {
		t.Fatalf("one press spent %d times within its ten minutes", redeemer.called())
	}
	if copied.StatusCode != first.StatusCode || !bytes.Equal(copied.Body, first.Body) || copied.Headers.Get(replayedHeader) != "1" {
		t.Fatalf("copy = %d %s replayed=%q, want the first answer replayed", copied.StatusCode, copied.Body, copied.Headers.Get(replayedHeader))
	}
}

// panicRedeemer fails the way nothing else can: by not returning at all.
type panicRedeemer struct{ calls atomic.Int32 }

func (p *panicRedeemer) Redeem(context.Context, string, string) (redeem.Result, error) {
	p.calls.Add(1)
	panic("redeemer failed")
}

// A redeemer that panics may have sent the claim before it did, so the press
// is recorded as an unknown outcome. A copy of it is told exactly that, at
// once and without a second provider call, rather than an empty success or a
// minute's wait on a press that never finishes.
func TestAPressWhoseRedeemerPanickedReplaysAsAnUnknownOutcome(t *testing.T) {
	a, _ := redeemableAPI(t)
	panicking := &panicRedeemer{}
	a.SetRedeemer(panicking)
	headers := spendHeaders(confirmedPress(codexID, pressID(1)))
	func() {
		// In CPA the plugin's ABI boundary recovers this; here the test does.
		defer func() {
			if recover() == nil {
				t.Error("the redeemer's panic was swallowed")
			}
		}()
		spendGET(a, headers)
	}()

	copied := make(chan protocol.ManagementResponse, 1)
	go func() { copied <- spendGET(a, headers) }()
	var res protocol.ManagementResponse
	select {
	case res = <-copied:
	case <-time.After(5 * time.Second):
		t.Fatal("a copy of the press waited on an answer that was never recorded")
	}
	if res.StatusCode != http.StatusBadGateway || string(res.Body) != `{"error":"outcome_unknown","retryUntilEpoch":null}` {
		t.Fatalf("copy = %d %s, want 502 outcome_unknown with no deadline", res.StatusCode, res.Body)
	}
	if res.Headers.Get(replayedHeader) != "1" || res.Headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("copy headers = %v", res.Headers)
	}
	if panicking.calls.Load() != 1 {
		t.Fatalf("the redeemer was called %d times", panicking.calls.Load())
	}
}

// /spend is one exact path on the resource tree, answering GET only.
func TestSpendRoutesAreExact(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	headers := spendHeaders(confirmedPress(codexID, pressID(1)))
	for _, req := range []protocol.ManagementRequest{
		{Method: "POST", Path: spendPath, Headers: headers, Body: []byte(`{"credentialId":"` + codexID + `","confirmed":true}`)},
		{Method: "PUT", Path: spendPath, Headers: headers},
		{Method: "GET", Path: "/v0/management/plugins/quota-glance/spend", Headers: headers},
		{Method: "GET", Path: spendPath + "/", Headers: headers},
		{Method: "GET", Path: spendPath + "/x", Headers: headers},
	} {
		if res := a.Handle(req, time.Now()); res.StatusCode != http.StatusNotFound {
			t.Fatalf("%s %s = %d, want 404", req.Method, req.Path, res.StatusCode)
		}
	}
	if redeemer.called() != 0 {
		t.Fatal("a request outside the one spend route reached the redeemer")
	}
}

// Both doors keep one ledger, so a press is single-use whichever door its
// copies arrive at.
func TestBothDoorsShareOnePressLedger(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	first := spendGET(a, spendHeaders(confirmedPress(codexID, pressID(1))))
	again := post(a, redeemPath, nil, confirmedPress(codexID, pressID(1)))
	if redeemer.called() != 1 || again.Headers.Get(replayedHeader) != "1" || !bytes.Equal(again.Body, first.Body) {
		t.Fatalf("calls=%d replayed=%q body=%s", redeemer.called(), again.Headers.Get(replayedHeader), again.Body)
	}
}

// Nothing the spend route emits contains the token or the header it was sent.
func TestSpendEchoesNeitherTheTokenNorThePress(t *testing.T) {
	a, _ := redeemableAPI(t)
	headers := spendHeaders(confirmedPress(codexID, pressID(1)))
	for _, res := range []protocol.ManagementResponse{spendGET(a, headers), spendGET(a, headers)} {
		emitted := string(res.Body)
		for _, values := range res.Headers {
			emitted += strings.Join(values, " ")
		}
		if strings.Contains(emitted, testToken) || strings.Contains(emitted, headers.Get(spendHeader)) || strings.Contains(emitted, pressID(1)) {
			t.Fatalf("the answer echoed the request: %s", emitted)
		}
	}
}
