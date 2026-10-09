package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/redeem"
)

// The Codex endpoints, as the redeemer addresses them. Copied rather than
// exported: these tests stand in for the provider, and the provider's URLs are
// part of what they pin.
const (
	codexCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	codexConsumeURL = codexCreditsURL + "/consume"
)

// providerHost is a synthetic CPA host behind the real redeemer: a Codex
// account holding credits, and CPA's cooldown callback. Nothing leaves the
// process. It counts the two things a press can cost — a consume sent to the
// provider, and a cooldown cleared in CPA — per credential.
type providerHost struct {
	mu sync.Mutex
	// consumes is the scripted answer to each consume, oldest first; once it
	// runs out every consume is a reset.
	consumes    []protocol.HostHTTPResponse
	consumeErrs []error
	cooldownErr error

	spent   map[string]int
	cleared map[string]int
	current string
}

func newProviderHost() *providerHost {
	return &providerHost{spent: map[string]int{}, cleared: map[string]int{}}
}

func (h *providerHost) GetAuth(_ context.Context, authIndex string) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.current = authIndex
	return []byte(`{"access_token":"synthetic-token","account_id":"synthetic-` + authIndex + `"}`), nil
}

func (h *providerHost) HTTPDo(_ context.Context, request protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch request.URL {
	case codexCreditsURL:
		return protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"credits":[{"id":"credit-a","status":"available"},{"id":"credit-b","status":"available"}]}`)}, nil
	case codexConsumeURL:
		h.spent[h.current]++
		if len(h.consumeErrs) > 0 {
			err := h.consumeErrs[0]
			h.consumeErrs = h.consumeErrs[1:]
			if err != nil {
				return protocol.HostHTTPResponse{}, err
			}
		}
		if len(h.consumes) > 0 {
			next := h.consumes[0]
			h.consumes = h.consumes[1:]
			return next, nil
		}
		return protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"code":"reset","windows_reset":2}`)}, nil
	}
	return protocol.HostHTTPResponse{StatusCode: 404}, nil
}

func (h *providerHost) ResetCooldown(_ context.Context, authIndex string) (protocol.HostRoutingResetCooldownResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cleared[authIndex]++
	if h.cooldownErr != nil {
		return protocol.HostRoutingResetCooldownResponse{}, h.cooldownErr
	}
	return protocol.HostRoutingResetCooldownResponse{AuthIndex: authIndex}, nil
}

func (h *providerHost) counts(id string) (spent, cleared int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.spent[id], h.cleared[id]
}

// realRedeemAPI is the served document from redeemableAPI, with the real
// redeemer behind it on a synthetic host.
func realRedeemAPI(t *testing.T) (*API, *providerHost) {
	t.Helper()
	a, _ := redeemableAPI(t)
	host := newProviderHost()
	a.SetRedeemer(redeem.New(host))
	return a, host
}

// A confirmed reset, by either door, answers with the cooldown cleared, and
// clears it on the credential that was pressed and on no other.
func TestAConfirmedResetReportsItsCooldownClearedOnBothDoors(t *testing.T) {
	a, host := realRedeemAPI(t)
	byPost := post(a, redeemPath, nil, map[string]any{"credentialId": codexID, "confirmed": true, "pressId": pressID(1)})
	byGet := spendGET(a, spendHeaders(confirmedPress(codexID, pressID(2))))
	for door, res := range map[string]protocol.ManagementResponse{"POST": byPost, "GET": byGet} {
		body := decodeBody(t, res)
		if res.StatusCode != http.StatusOK || body["outcome"] != redeem.OutcomeReset || body["cooldown"] != redeem.CooldownCleared {
			t.Fatalf("%s: %d %v, want a reset with its cooldown cleared", door, res.StatusCode, body)
		}
		if bytes.Contains(res.Body, []byte("synthetic")) {
			t.Fatalf("%s: the answer carries credential material: %s", door, res.Body)
		}
	}
	if spent, cleared := host.counts(codexID); spent != 2 || cleared != 2 {
		t.Fatalf("two presses spent %d and cleared %d", spent, cleared)
	}
	if spent, cleared := host.counts(claudeID); spent != 0 || cleared != 0 {
		t.Fatalf("another credential was touched: spent %d, cleared %d", spent, cleared)
	}
}

// A clear that fails is a 200 with the reset in it and the cooldown's status
// beside it — a partial success. A copy of that press is handed the same answer
// from the ledger: no second consume, and no second clear.
func TestAFailedClearIsReportedAndNeverRespent(t *testing.T) {
	for name, err := range map[string]error{
		"failed":      errors.New("core auth manager unavailable"),
		"unsupported": protocol.ErrUnsupportedCallback,
	} {
		a, host := realRedeemAPI(t)
		host.cooldownErr = err
		headers := spendHeaders(confirmedPress(codexID, pressID(1)))
		first := spendGET(a, headers)
		body := decodeBody(t, first)
		if first.StatusCode != http.StatusOK || body["outcome"] != redeem.OutcomeReset || body["cooldown"] != name {
			t.Fatalf("%s: %d %v, want a reset with cooldown %q", name, first.StatusCode, body, name)
		}
		replay := spendGET(a, headers)
		if replay.StatusCode != http.StatusOK || !bytes.Equal(replay.Body, first.Body) || replay.Headers.Get(replayedHeader) != "1" {
			t.Fatalf("%s: replay = %d %s", name, replay.StatusCode, replay.Body)
		}
		if spent, cleared := host.counts(codexID); spent != 1 || cleared != 1 {
			t.Fatalf("%s: one press and its copy spent %d and cleared %d, want one each", name, spent, cleared)
		}
	}
}

// Eight copies of one press at once: one consume, one clear, one answer.
func TestConcurrentCopiesOfAPressClearOnce(t *testing.T) {
	a, host := realRedeemAPI(t)
	var wg sync.WaitGroup
	got := make([]protocol.ManagementResponse, 8)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = spendGET(a, spendHeaders(confirmedPress(codexID, pressID(1))))
		}()
	}
	wg.Wait()
	for _, res := range got {
		if res.StatusCode != http.StatusOK || !bytes.Equal(res.Body, got[0].Body) {
			t.Fatalf("answers differ: %d %s vs %s", res.StatusCode, res.Body, got[0].Body)
		}
	}
	if spent, cleared := host.counts(codexID); spent != 1 || cleared != 1 {
		t.Fatalf("eight copies spent %d and cleared %d, want one each", spent, cleared)
	}
}

// An unknown outcome clears nothing and says nothing about a cooldown. The
// next press repeats the claim, and the provider's "already redeemed" for that
// same claim confirms the earlier reset: that answer clears the cooldown, and
// still no second credit is spent.
func TestAnUnknownOutcomeClearsNothingUntilItsRepeatConfirms(t *testing.T) {
	a, host := realRedeemAPI(t)
	host.consumeErrs = []error{errors.New("context deadline exceeded")}
	host.consumes = []protocol.HostHTTPResponse{{StatusCode: 200, Body: []byte(`{"code":"already_redeemed"}`)}}

	first := spendGET(a, spendHeaders(confirmedPress(codexID, pressID(1))))
	if first.StatusCode != http.StatusBadGateway || decodeBody(t, first)["error"] != "outcome_unknown" {
		t.Fatalf("first press = %d %s, want outcome_unknown", first.StatusCode, first.Body)
	}
	if _, cleared := host.counts(codexID); cleared != 0 {
		t.Fatalf("an unknown outcome cleared the cooldown %d times", cleared)
	}
	second := spendGET(a, spendHeaders(confirmedPress(codexID, pressID(2))))
	body := decodeBody(t, second)
	if second.StatusCode != http.StatusOK || body["outcome"] != redeem.OutcomeAlreadyUsed || body["cooldown"] != redeem.CooldownCleared {
		t.Fatalf("repeat = %d %v, want alreadyUsed with the cooldown cleared", second.StatusCode, body)
	}
	if spent, cleared := host.counts(codexID); spent != 2 || cleared != 1 {
		t.Fatalf("spent %d consumes (one claim sent twice) and cleared %d, want 2 and 1", spent, cleared)
	}
}

// An outcome that is not a reset carries no cooldown field at all, so an older
// page reads exactly the answer it always did.
func TestANonResetAnswerCarriesNoCooldown(t *testing.T) {
	a, host := realRedeemAPI(t)
	host.consumes = []protocol.HostHTTPResponse{{StatusCode: 200, Body: []byte(`{"code":"nothing_to_reset"}`)}}
	res := post(a, redeemPath, nil, map[string]any{"credentialId": codexID, "confirmed": true})
	body := decodeBody(t, res)
	if _, present := body["cooldown"]; present || body["outcome"] != redeem.OutcomeNothingToReset {
		t.Fatalf("body = %v, want nothingToReset and no cooldown field", body)
	}
	if _, cleared := host.counts(codexID); cleared != 0 {
		t.Fatalf("nothing to reset cleared the cooldown %d times", cleared)
	}
}
