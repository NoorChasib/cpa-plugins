package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/redeem"
)

// doorAnswer is one door's answer, and the API and redeemer behind it.
type doorAnswer struct {
	res      protocol.ManagementResponse
	api      *API
	redeemer *fakeRedeemer
}

// pressBothDoors sends the same press object down the POST door and, with a
// press id of its own, down the GET door, each against a fresh API, after
// setup has had its say on that API's redeemer.
func pressBothDoors(t *testing.T, body map[string]any, setup ...func(*fakeRedeemer)) map[string]doorAnswer {
	t.Helper()
	out := map[string]doorAnswer{}
	for _, door := range []string{"POST", "GET"} {
		a, redeemer := redeemableAPI(t)
		for _, change := range setup {
			change(redeemer)
		}
		var res protocol.ManagementResponse
		if door == "POST" {
			res = post(a, redeemPath, nil, body)
		} else {
			withID := map[string]any{"pressId": pressID(1)}
			for key, value := range body {
				withID[key] = value
			}
			res = spendGET(a, spendHeaders(withID))
		}
		out[door] = doorAnswer{res, a, redeemer}
	}
	return out
}

// The kind a press names reaches the redeemer as it was sent, through either
// door, and the answer says which kind it was.
func TestAPressCarriesItsKindToTheRedeemerAndBack(t *testing.T) {
	for _, kind := range []string{"full", "fable"} {
		for door, got := range pressBothDoors(t, map[string]any{"credentialId": claudeID, "confirmed": true, "kind": kind}) {
			if got.res.StatusCode != http.StatusOK || decodeBody(t, got.res)["kind"] != kind {
				t.Fatalf("%s %s: %d %s", kind, door, got.res.StatusCode, got.res.Body)
			}
			if got.redeemer.called() != 1 || got.redeemer.askedFor() != kind {
				t.Fatalf("%s %s: the redeemer was asked for %q", kind, door, got.redeemer.askedFor())
			}
		}
	}
	// Codex names no kind, and its answer says so with a null rather than by
	// leaving the field out, so a client reads one shape.
	for door, got := range pressBothDoors(t, map[string]any{"credentialId": codexID, "confirmed": true}) {
		body := decodeBody(t, got.res)
		if kind, present := body["kind"]; got.res.StatusCode != http.StatusOK || !present || kind != nil {
			t.Fatalf("codex %s: %d %s, want kind null", door, got.res.StatusCode, got.res.Body)
		}
		if got.redeemer.called() != 1 || got.redeemer.askedFor() != "" {
			t.Fatalf("codex %s: the redeemer was asked for %q", door, got.redeemer.askedFor())
		}
	}
}

// A kind that is present must be one a press can spend, on both doors. It is
// refused as a malformed press is, before the document, the ledger or the
// redeemer is consulted.
func TestAPressNamingNoSpendableKindIsMalformed(t *testing.T) {
	for name, kind := range map[string]any{
		"unknown":       "unknown",
		"a new kind":    "monthly",
		"another case":  "Full",
		"empty":         "",
		"padded":        " fable",
		"a number":      7,
		"a boolean":     true,
		"a list":        []string{"full"},
		"an object":     map[string]any{"kind": "full"},
		"the API's own": "five_hour",
	} {
		for _, credential := range []string{claudeID, codexID} {
			for door, got := range pressBothDoors(t, map[string]any{"credentialId": credential, "confirmed": true, "kind": kind}) {
				if got.res.StatusCode != http.StatusBadRequest || decodeBody(t, got.res)["error"] != "invalid_request" {
					t.Fatalf("%s on %s %s: %d %s, want 400 invalid_request", name, credential, door, got.res.StatusCode, got.res.Body)
				}
				if got.redeemer.called() != 0 || len(got.api.ledger.entries) != 0 {
					t.Fatalf("%s on %s %s: reached the redeemer or the ledger", name, credential, door)
				}
			}
		}
	}
	// Confirmation is still checked first.
	for door, got := range pressBothDoors(t, map[string]any{"credentialId": claudeID, "confirmed": false, "kind": "monthly"}) {
		if decodeBody(t, got.res)["error"] != "confirmation_required" {
			t.Fatalf("%s: unconfirmed with a bad kind: %s", door, got.res.Body)
		}
	}
}

// A Claude press that names no kind cannot be spent as sent: Claude's resets
// refill different limits, and nothing here may choose one for the reader. It
// is a 400 of its own, so the page can say to reload rather than that the
// account has nothing. A null kind is no kind, as a null press id is no id.
func TestAClaudePressMustNameAKind(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"absent": {"credentialId": claudeID, "confirmed": true},
		"null":   {"credentialId": claudeID, "confirmed": true, "kind": nil},
		// Its resets may be of any kind, so it is told the same.
		"kinds unread": {"credentialId": claudeOldID, "confirmed": true},
	} {
		for door, got := range pressBothDoors(t, body) {
			if got.res.StatusCode != http.StatusBadRequest || string(got.res.Body) != `{"error":"kind_required","refreshRequested":false}` {
				t.Fatalf("%s %s: %d %s, want 400 kind_required", name, door, got.res.StatusCode, got.res.Body)
			}
			if got.redeemer.called() != 0 {
				t.Fatalf("%s %s: a press with no kind reached the redeemer", name, door)
			}
		}
	}
	// On Codex the same press is as it always was.
	for door, got := range pressBothDoors(t, map[string]any{"credentialId": codexID, "confirmed": true, "kind": nil}) {
		if got.res.StatusCode != http.StatusOK {
			t.Fatalf("codex %s with a null kind: %d %s", door, got.res.StatusCode, got.res.Body)
		}
	}
}

// The document decides which kinds may be spent, per credential. A kind the
// card offers no button for is refused exactly as an account it offers none
// for is, and a Codex press naming any kind is offering something the card
// is not.
func TestAPressForAKindTheDocumentDoesNotOfferIsRefused(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"a kind the account does not hold":  {"credentialId": claudeID, "kind": "weekly"},
		"a kind with nothing left":          {"credentialId": claudeID, "kind": "session"},
		"a kind whose credential is parked": {"credentialId": claudeParkedID, "kind": "full"},
		"an account whose kinds are unread": {"credentialId": claudeOldID, "kind": "full"},
		"an account with nothing banked":    {"credentialId": "claude-a@example.com.json", "kind": "full"},
		"Codex naming a kind":               {"credentialId": codexID, "kind": "full"},
		"Codex naming Fable":                {"credentialId": codexID, "kind": "fable"},
	} {
		body["confirmed"] = true
		for door, got := range pressBothDoors(t, body) {
			if got.res.StatusCode != http.StatusConflict || string(got.res.Body) != `{"error":"not_redeemable","refreshRequested":false}` {
				t.Fatalf("%s %s: %d %s, want 409 not_redeemable", name, door, got.res.StatusCode, got.res.Body)
			}
			if got.redeemer.called() != 0 {
				t.Fatalf("%s %s: reached the redeemer", name, door)
			}
		}
	}
}

// The redeemer's two kind refusals are answered as fixed codes, neither of them
// a 5xx, because neither sent anything. other_kind_unresolved names the kind
// the earlier press was for and the instant its window closes, so the reader
// can settle it first.
func TestTheRedeemersKindRefusalsAreAnsweredAsCodes(t *testing.T) {
	deadline := time.Date(2026, 10, 5, 12, 10, 0, 0, time.UTC)
	for name, one := range map[string]struct {
		err    error
		status int
		body   string
	}{
		"invalid kind": {redeem.ErrInvalidKind, http.StatusBadRequest, `{"error":"invalid_kind","refreshRequested":false}`},
		"another kind unresolved": {&redeem.OtherKindUnresolvedError{Kind: "fable", RetryUntil: deadline}, http.StatusConflict,
			`{"error":"other_kind_unresolved","pendingKind":"fable","refreshRequested":false,"retryUntilEpoch":1791202200}`},
		// Without the details, null rather than a guess at either.
		"bare sentinel": {redeem.ErrOtherKindUnresolved, http.StatusConflict,
			`{"error":"other_kind_unresolved","pendingKind":null,"refreshRequested":false,"retryUntilEpoch":null}`},
	} {
		body := map[string]any{"credentialId": claudeID, "confirmed": true, "kind": "full"}
		for door, got := range pressBothDoors(t, body, func(r *fakeRedeemer) { r.err = one.err }) {
			if got.res.StatusCode != one.status || string(got.res.Body) != one.body {
				t.Fatalf("%s %s: %d %s, want %d %s", name, door, got.res.StatusCode, got.res.Body, one.status, one.body)
			}
		}
	}
}

// A press id is one decision about one kind of reset on one account. The same
// id arriving for another kind is refused rather than answered with the first
// kind's answer, which would tell the reader the wrong reset was spent; the
// same id for the same kind is a copy, and gets the first answer.
func TestAPressIDCannotBeReusedForAnotherKind(t *testing.T) {
	a, redeemer := redeemableAPI(t)
	fable := map[string]any{"credentialId": claudeID, "confirmed": true, "kind": "fable", "pressId": pressID(1)}
	first := spendGET(a, spendHeaders(fable))
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first = %d %s", first.StatusCode, first.Body)
	}
	for name, body := range map[string]map[string]any{
		"another kind": {"credentialId": claudeID, "confirmed": true, "kind": "full", "pressId": pressID(1)},
		"no kind":      {"credentialId": claudeID, "confirmed": true, "pressId": pressID(1)},
	} {
		for door, res := range map[string]protocol.ManagementResponse{
			"GET": spendGET(a, spendHeaders(body)), "POST": post(a, redeemPath, nil, body),
		} {
			if res.StatusCode != http.StatusBadRequest || decodeBody(t, res)["error"] != "invalid_request" {
				t.Fatalf("%s %s: %d %s, want 400 invalid_request", name, door, res.StatusCode, res.Body)
			}
		}
	}
	copied := post(a, redeemPath, nil, fable)
	if copied.Headers.Get(replayedHeader) != "1" || !bytes.Equal(copied.Body, first.Body) {
		t.Fatalf("copy = %d %s", copied.StatusCode, copied.Body)
	}
	if redeemer.called() != 1 {
		t.Fatalf("redeemer called %d times", redeemer.called())
	}
}

// The Claude endpoints, as the redeemer addresses them, for a fictional
// organization. Copied rather than exported, as the Codex ones are.
const (
	claudeOrg         = "0f8fad5b-d9cb-469f-a165-70867728950e"
	claudeProfileURL  = "https://api.anthropic.com/api/oauth/profile"
	claudeStatusURL   = "https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1"
	claudeClaimURL    = "https://api.anthropic.com/api/organizations/" + claudeOrg + "/reset_rate_limits"
	claudeMixedStatus = `{"cedar_ember":{"eligible":true,"at_limit":true,"next_grant_id":"grant-fable","grants":[` +
		`{"id":"grant-full","resets_total":1,"resets_left":1,"ends_at":"2099-10-30T00:00:00Z","type":"full","usable_now":true},` +
		`{"id":"grant-fable","resets_total":2,"resets_left":2,"ends_at":"2099-10-07T00:00:00Z","gives_back_weekly":true,"usable_now":true}]}}`
)

// claudeProviderHost is a synthetic CPA host behind the real redeemer: a
// Claude account at its limit holding one full reset and two Fable resets, the
// Fable grant recommended. The first claim's answer is lost; every later one
// is a reset. Nothing leaves the process.
type claudeProviderHost struct {
	mu     sync.Mutex
	claims []map[string]string
}

func (h *claudeProviderHost) GetAuth(context.Context, string) ([]byte, error) {
	return []byte(`{"access_token":"synthetic-token"}`), nil
}

func (h *claudeProviderHost) HTTPDo(_ context.Context, request protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch request.URL {
	case claudeProfileURL:
		return protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"organization":{"uuid":"` + claudeOrg + `"}}`)}, nil
	case claudeStatusURL:
		return protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(claudeMixedStatus)}, nil
	case claudeClaimURL:
		var claim map[string]string
		_ = json.Unmarshal(request.Body, &claim)
		h.claims = append(h.claims, claim)
		if len(h.claims) == 1 {
			return protocol.HostHTTPResponse{StatusCode: 504}, nil
		}
		// What is left on the grant claimed: one of the two Fable resets, or
		// none of the one full reset.
		left := map[string]string{"grant-fable": "1", "grant-full": "0"}[claim["grant_id"]]
		return protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"result":"reset","resets_left":` + left + `}`)}, nil
	}
	return protocol.HostHTTPResponse{StatusCode: 404}, nil
}

func (h *claudeProviderHost) ResetCooldown(_ context.Context, authIndex string) (protocol.HostRoutingResetCooldownResponse, error) {
	return protocol.HostRoutingResetCooldownResponse{AuthIndex: authIndex}, nil
}

func (h *claudeProviderHost) sent() []map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]string(nil), h.claims...)
}

// End to end, through the real redeemer: each kind claims only its own grant,
// and a Fable press left unknown holds off a full press — nothing sent, the
// Fable claim named — until a Fable press settles it.
func TestEachKindSpendsOnlyItsOwnGrantEndToEnd(t *testing.T) {
	a, _ := redeemableAPI(t)
	host := &claudeProviderHost{}
	a.SetRedeemer(redeem.New(host))
	press := func(kind string, n int) (int, map[string]any) {
		res := spendGET(a, spendHeaders(map[string]any{"credentialId": claudeID, "confirmed": true, "kind": kind, "pressId": pressID(n)}))
		return res.StatusCode, decodeBody(t, res)
	}

	if status, body := press("fable", 1); status != http.StatusBadGateway || body["error"] != "outcome_unknown" {
		t.Fatalf("Fable press = %d %v, want outcome_unknown", status, body)
	}
	status, body := press("full", 2)
	if status != http.StatusConflict || body["error"] != "other_kind_unresolved" || body["pendingKind"] != "fable" || body["retryUntilEpoch"] == nil {
		t.Fatalf("full press = %d %v, want other_kind_unresolved naming the Fable claim", status, body)
	}
	if claims := host.sent(); len(claims) != 1 || claims[0]["grant_id"] != "grant-fable" {
		t.Fatalf("claims = %v, want only the Fable claim", claims)
	}
	// Each answer counts its own kind: one Fable reset left, and no full one,
	// though the account still holds a Fable reset.
	if status, body := press("fable", 3); status != http.StatusOK || body["kind"] != "fable" || body["outcome"] != redeem.OutcomeReset ||
		body["remainingCount"] != 1.0 {
		t.Fatalf("Fable repeat = %d %v", status, body)
	}
	if status, body := press("full", 4); status != http.StatusOK || body["kind"] != "full" || body["remainingCount"] != 0.0 {
		t.Fatalf("full press after the settle = %d %v", status, body)
	}
	claims := host.sent()
	if len(claims) != 3 || claims[1]["request_id"] != claims[0]["request_id"] || claims[1]["grant_id"] != "grant-fable" ||
		claims[2]["grant_id"] != "grant-full" {
		t.Fatalf("claims = %v, want the Fable claim twice and then the full grant", claims)
	}
}

func TestTheLedgerBindsAPressToItsKind(t *testing.T) {
	l := newLedger()
	entry, fresh, err := l.begin(pressID(1), "c", "fable")
	if err != nil || !fresh {
		t.Fatalf("fresh = %v, err = %v", fresh, err)
	}
	l.finish(entry, jsonResponse(http.StatusOK, map[string]string{"outcome": "reset"}))
	for _, kind := range []string{"full", ""} {
		if _, _, err := l.begin(pressID(1), "c", kind); !errors.Is(err, errPressMismatch) {
			t.Fatalf("%q: err = %v, want errPressMismatch", kind, err)
		}
	}
	if _, fresh, err := l.begin(pressID(1), "c", "fable"); err != nil || fresh {
		t.Fatalf("the same kind: fresh = %v, err = %v", fresh, err)
	}
}
