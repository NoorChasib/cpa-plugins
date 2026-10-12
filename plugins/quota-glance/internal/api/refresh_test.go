package api

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/redeem"
)

// refreshedAt is the instant fakeRefresher says each request was made, by
// CPA's clock: a minute on from each, so presses in one test tell apart.
var refreshedAt = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

// fakeRefresher records every refresh request, and answers with ok and, when
// ok, an instant of its own for each.
type fakeRefresher struct {
	mu    sync.Mutex
	ok    bool
	asked [][2]string
}

func (f *fakeRefresher) RequestRefresh(provider, authIndex string) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, [2]string{provider, authIndex})
	if !f.ok {
		return time.Time{}, false
	}
	return refreshedAt.Add(time.Duration(len(f.asked)-1) * time.Minute), true
}

func (f *fakeRefresher) requests() [][2]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]string(nil), f.asked...)
}

// refreshingAPI is redeemableAPI with a refresher that succeeds.
func refreshingAPI(t *testing.T) (*API, *fakeRedeemer, *fakeRefresher) {
	t.Helper()
	a, redeemer := redeemableAPI(t)
	refresher := &fakeRefresher{ok: true}
	a.SetRefresher(refresher)
	return a, redeemer, refresher
}

// A press that spent a reset asks Quota Cache to poll that account, once,
// naming its provider and auth index, and says so in the answer.
func TestASpendAsksQuotaCacheToRefreshTheAccount(t *testing.T) {
	for _, one := range []struct{ id, provider string }{{codexID, "codex"}, {claudeID, "claude"}} {
		a, redeemer, refresher := refreshingAPI(t)
		redeemer.result = redeem.Result{Outcome: redeem.OutcomeReset, WindowsReset: 2, RemainingCount: 1, MayHaveSpent: true}
		res := post(a, redeemPath, nil, map[string]any{"credentialId": one.id, "confirmed": true})
		body := decodeBody(t, res)
		if res.StatusCode != http.StatusOK || body["refreshRequested"] != true {
			t.Fatalf("%s: %d %s, want 200 with refreshRequested true", one.provider, res.StatusCode, res.Body)
		}
		// The request's own instant, by CPA's clock, which the page compares
		// with Quota Cache's observations of the account.
		if body["refreshRequestedAtEpoch"] != float64(refreshedAt.Unix()) {
			t.Fatalf("%s: refreshRequestedAtEpoch = %v, want %d", one.provider, body["refreshRequestedAtEpoch"], refreshedAt.Unix())
		}
		if got := refresher.requests(); len(got) != 1 || got[0] != [2]string{one.provider, one.id} {
			t.Fatalf("%s: refresh requests = %v, want one for this credential", one.provider, got)
		}
	}
}

// A press that spent nothing asks nothing, and every answer the press gives
// says so: an outcome that spent nothing, a failure before the spend was sent,
// a refusal, and a repeat of an unknown claim that sent nothing itself.
func TestAPressThatSpentNothingAsksForNoRefresh(t *testing.T) {
	for name, one := range map[string]struct {
		result redeem.Result
		err    error
	}{
		"no credit":             {result: redeem.Result{Outcome: redeem.OutcomeNoCredit}},
		"not limited":           {result: redeem.Result{Outcome: redeem.OutcomeNotLimited, RemainingCount: 3}},
		"already used, fresh":   {result: redeem.Result{Outcome: redeem.OutcomeAlreadyUsed, RemainingCount: 2}},
		"refused":               {err: redeem.ErrRefused},
		"rate limited":          {err: redeem.ErrRateLimited},
		"unreachable":           {err: redeem.ErrUnavailable},
		"in flight":             {err: redeem.ErrInFlight},
		"unusable":              {err: redeem.ErrNoAccessToken},
		"identity changed":      {err: redeem.ErrIdentityChanged},
		"repeat not sent":       {err: &redeem.OutcomeUnknownError{RetryUntil: time.Unix(1789013400, 0)}},
		"window closed, unsent": {err: redeem.ErrRetryWindowClosed},
	} {
		a, redeemer, refresher := refreshingAPI(t)
		redeemer.result, redeemer.err = one.result, one.err
		res := post(a, redeemPath, nil, map[string]any{"credentialId": codexID, "confirmed": true})
		body := decodeBody(t, res)
		if got, present := body["refreshRequested"]; !present || got != false {
			t.Fatalf("%s: %d %s, want refreshRequested false", name, res.StatusCode, res.Body)
		}
		if _, present := body["refreshRequestedAtEpoch"]; present {
			t.Fatalf("%s: %s names an instant for a refresh never asked for", name, res.Body)
		}
		if got := refresher.requests(); len(got) != 0 {
			t.Fatalf("%s: asked Quota Cache to refresh after a press that spent nothing: %v", name, got)
		}
	}

	// Refused by the document before the redeemer is asked at all.
	a, redeemer, refresher := refreshingAPI(t)
	res := post(a, redeemPath, nil, map[string]any{"credentialId": "codex-parked@example.com.json", "confirmed": true})
	if body := decodeBody(t, res); res.StatusCode != http.StatusConflict || body["error"] != "not_redeemable" || body["refreshRequested"] != false {
		t.Fatalf("not offered: %d %s", res.StatusCode, res.Body)
	}
	if redeemer.called() != 0 || len(refresher.requests()) != 0 {
		t.Fatal("a press the document refused reached the redeemer or the refresher")
	}
}

// An unknown outcome may have spent the reset, so it asks as a spend does,
// and the answer still names the claim's deadline.
func TestAnUnknownOutcomeAsksForARefresh(t *testing.T) {
	deadline := time.Date(2026, 10, 12, 9, 10, 0, 0, time.UTC)
	for name, err := range map[string]error{
		"outcome unknown":          &redeem.OutcomeUnknownError{RetryUntil: deadline},
		"window closed mid-flight": redeem.ErrRetryWindowClosed,
	} {
		a, redeemer, refresher := refreshingAPI(t)
		redeemer.result, redeemer.err = redeem.Result{MayHaveSpent: true}, err
		res := post(a, redeemPath, nil, map[string]any{"credentialId": claudeID, "confirmed": true})
		body := decodeBody(t, res)
		if res.StatusCode != http.StatusBadGateway || body["refreshRequested"] != true || body["refreshRequestedAtEpoch"] != float64(refreshedAt.Unix()) {
			t.Fatalf("%s: %d %s, want 502 with refreshRequested true, and when", name, res.StatusCode, res.Body)
		}
		if got := refresher.requests(); len(got) != 1 || got[0] != [2]string{"claude", claudeID} {
			t.Fatalf("%s: refresh requests = %v", name, got)
		}
		if name == "outcome unknown" && body["retryUntilEpoch"] != float64(deadline.Unix()) {
			t.Fatalf("%s: retryUntilEpoch = %v", name, body["retryUntilEpoch"])
		}
	}
}

// A refresh that could not be asked for changes the answer in that one field
// and no other, on a spend and on an unknown outcome alike. So does having no
// refresher installed at all.
func TestARefreshThatFailsChangesNothingElse(t *testing.T) {
	for name, one := range map[string]struct {
		result redeem.Result
		err    error
	}{
		"spent":   {result: redeem.Result{Outcome: redeem.OutcomeReset, WindowsReset: 2, RemainingCount: 1, Cooldown: redeem.CooldownCleared, MayHaveSpent: true}},
		"unknown": {result: redeem.Result{MayHaveSpent: true}, err: &redeem.OutcomeUnknownError{RetryUntil: time.Unix(1789013400, 0)}},
	} {
		answer := func(refresher Refresher) (int, map[string]any) {
			a, redeemer := redeemableAPI(t)
			a.SetRefresher(refresher)
			redeemer.result, redeemer.err = one.result, one.err
			res := post(a, redeemPath, nil, map[string]any{"credentialId": codexID, "confirmed": true})
			return res.StatusCode, decodeBody(t, res)
		}
		asked, want := answer(&fakeRefresher{ok: true})
		failing := &fakeRefresher{ok: false}
		for label, refresher := range map[string]Refresher{"failing": failing, "absent": nil} {
			status, got := answer(refresher)
			if got["refreshRequested"] != false {
				t.Fatalf("%s, %s: refreshRequested = %v", name, label, got["refreshRequested"])
			}
			if _, present := got["refreshRequestedAtEpoch"]; present {
				t.Fatalf("%s, %s: an instant for a refresh never asked for: %v", name, label, got)
			}
			got["refreshRequested"], got["refreshRequestedAtEpoch"] = want["refreshRequested"], want["refreshRequestedAtEpoch"]
			a, _ := json.Marshal(got)
			b, _ := json.Marshal(want)
			if status != asked || string(a) != string(b) {
				t.Fatalf("%s, %s: %d %s, want %d %s but for refreshRequested", name, label, status, a, asked, b)
			}
		}
		if len(failing.requests()) != 1 {
			t.Fatalf("%s: the failing refresher was asked %d times, want once", name, len(failing.requests()))
		}
	}
}

// A second copy of a press is the first copy's answer, refreshRequested and
// all. It spends nothing, so it asks for nothing either, and it names the
// instant of the first copy's request: the one Quota Cache acts on, which may
// be long past by the time a lost answer is asked for again.
func TestACopyOfAPressAsksForNoSecondRefresh(t *testing.T) {
	a, redeemer, refresher := refreshingAPI(t)
	redeemer.result = redeem.Result{Outcome: redeem.OutcomeReset, RemainingCount: 1, MayHaveSpent: true}
	body := map[string]any{"credentialId": codexID, "confirmed": true, "pressId": pressID(1)}
	first := post(a, redeemPath, nil, body)
	// Another press between the two copies makes a request of its own, so
	// a copy that asked again would name a later instant.
	post(a, redeemPath, nil, map[string]any{"credentialId": claudeID, "confirmed": true, "pressId": pressID(2)})
	second := post(a, redeemPath, nil, body)
	if string(second.Body) != string(first.Body) || second.Headers.Get(replayedHeader) != "1" {
		t.Fatalf("first %s, second %s", first.Body, second.Body)
	}
	if got := decodeBody(t, second)["refreshRequestedAtEpoch"]; got != float64(refreshedAt.Unix()) {
		t.Fatalf("the copy names %v, want the first request's %d", got, refreshedAt.Unix())
	}
	if got := refresher.requests(); redeemer.called() != 2 || len(got) != 2 {
		t.Fatalf("redeems = %d, refresh requests = %v; want one of each per press", redeemer.called(), got)
	}
}
