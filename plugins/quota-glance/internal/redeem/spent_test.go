package redeem

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
)

// MayHaveSpent is what decides whether Quota Cache is asked to poll the
// account after a press, so it must be true exactly when this press sent a
// spend request whose answer does not prove it spent nothing. Every press
// that sent nothing, or had its request refused, says false: a refusal, a hold,
// nothing to spend, a failed read, and a repeat that never went out, whatever
// the claim it repeats did.
func TestMayHaveSpentMeansThisPressSentASpendThatMayHaveLanded(t *testing.T) {
	codex := func(consume reply) *fakeHost {
		host := hostWith(map[string]string{codexCreditsURL: oneAvailable})
		host.queue(codexConsumeURL, consume)
		return host
	}
	claude := func(claim reply) *fakeHost {
		host := claudeHost(spendableStatus(), "")
		host.queue(claudeClaimURL, claim)
		return host
	}
	for _, one := range []struct {
		name     string
		provider string
		host     *fakeHost
		want     bool
	}{
		// Codex consumes the credit on acceptance, whatever the body says.
		{"codex reset", "codex", codex(reply{body: `{"code":"reset","windows_reset":2}`}), true},
		{"codex nothing to reset", "codex", codex(reply{body: `{"code":"nothing_to_reset"}`}), true},
		{"codex unknown code", "codex", codex(reply{body: `{"code":"something_new"}`}), true},
		{"codex unreadable 2xx", "codex", codex(reply{body: `not json`}), true},
		{"codex 502", "codex", codex(reply{status: 502}), true},
		{"codex transport failure", "codex", codex(reply{err: errors.New("connection reset")}), true},
		{"codex no credit", "codex", codex(reply{body: `{"code":"no_credit"}`}), false},
		{"codex already redeemed on a fresh claim", "codex", codex(reply{body: `{"code":"already_redeemed"}`}), false},
		{"codex throttled", "codex", codex(reply{status: 429}), false},
		{"codex unauthorized", "codex", codex(reply{status: 401}), false},
		{"codex nothing available", "codex", hostWith(map[string]string{codexCreditsURL: `{"credits":[]}`}), false},
		{"codex inventory refused", "codex", func() *fakeHost {
			host := hostWith(map[string]string{codexCreditsURL: oneAvailable})
			host.status[codexCreditsURL] = 500
			return host
		}(), false},
		{"codex no access token", "codex", func() *fakeHost {
			host := hostWith(map[string]string{codexCreditsURL: oneAvailable})
			host.setAuth(`{"account_id":"synthetic-account"}`)
			return host
		}(), false},

		{"claude reset", "claude", claude(reply{body: `{"result":"reset","resets_left":1}`}), true},
		{"claude unreadable 2xx", "claude", claude(reply{body: `{"result":"something_new"}`}), true},
		{"claude 500", "claude", claude(reply{status: 500}), true},
		// Someone else spent the grant between the status read and the claim.
		{"claude already used on a fresh claim", "claude", claude(reply{body: `{"result":"already_used"}`}), false},
		{"claude not limited", "claude", claude(reply{body: `{"result":"not_limited"}`}), false},
		{"claude unavailable", "claude", claude(reply{body: `{"result":"unavailable"}`}), false},
		{"claude throttled", "claude", claude(reply{status: 429}), false},
		{"claude held before the claim", "claude", claudeHost(statusWith(`{"eligible":true,"at_limit":false,"grants":[`+spendableGrant+`]}`), `{"result":"reset"}`), false},
	} {
		result, _ := redeemerOn(one.host, newClock()).Redeem(context.Background(), one.provider, "c")
		if result.MayHaveSpent != one.want {
			t.Errorf("%s: MayHaveSpent = %v, want %v (result %+v)", one.name, result.MayHaveSpent, one.want, result)
		}
	}
}

// The press after an unknown outcome is judged on what it sent itself.
func TestMayHaveSpentOnARepeatIsAboutTheRepeat(t *testing.T) {
	// first is a press that leaves its claim unknown, and returns the redeemer
	// so the repeat runs against the same journal.
	first := func(t *testing.T, host *fakeHost, clock *stepClock, provider string) *Redeemer {
		t.Helper()
		redeemer := redeemerOn(host, clock)
		result, err := redeemer.Redeem(context.Background(), provider, "c")
		if !errors.Is(err, ErrOutcomeUnknown) || !result.MayHaveSpent {
			t.Fatalf("first press = %+v, %v; want an unknown outcome that may have spent", result, err)
		}
		return redeemer
	}

	t.Run("already used confirms the earlier spend", func(t *testing.T) {
		host := claudeHost(spendableStatus(), "")
		host.queue(claudeClaimURL, reply{status: 502}, reply{body: `{"result":"already_used","resets_left":1}`})
		redeemer := first(t, host, newClock(), "claude")
		if result, err := redeemer.Redeem(context.Background(), "claude", "c"); err != nil || !result.MayHaveSpent {
			t.Fatalf("repeat = %+v, %v", result, err)
		}
		codexHost := hostWith(map[string]string{codexCreditsURL: oneAvailable})
		codexHost.queue(codexConsumeURL, reply{status: 502}, reply{body: `{"code":"already_redeemed"}`})
		redeemer = first(t, codexHost, newClock(), "codex")
		if result, err := redeemer.Redeem(context.Background(), "codex", "c"); err != nil || !result.MayHaveSpent {
			t.Fatalf("codex repeat = %+v, %v", result, err)
		}
	})

	t.Run("a repeat answered with nothing to trust", func(t *testing.T) {
		host := claudeHost(spendableStatus(), "")
		host.queue(claudeClaimURL, reply{status: 502}, reply{status: 504})
		redeemer := first(t, host, newClock(), "claude")
		if result, err := redeemer.Redeem(context.Background(), "claude", "c"); !errors.Is(err, ErrOutcomeUnknown) || !result.MayHaveSpent {
			t.Fatalf("repeat = %+v, %v", result, err)
		}
	})

	t.Run("a refused repeat spent nothing itself", func(t *testing.T) {
		host := claudeHost(spendableStatus(), "")
		host.queue(claudeClaimURL, reply{status: 502}, reply{body: `{"result":"cooldown"}`})
		redeemer := first(t, host, newClock(), "claude")
		if result, err := redeemer.Redeem(context.Background(), "claude", "c"); !errors.Is(err, ErrOutcomeUnknown) || result.MayHaveSpent {
			t.Fatalf("repeat = %+v, %v; want still unknown, and nothing sent by it that could spend", result, err)
		}
	})

	t.Run("a repeat that never went out", func(t *testing.T) {
		host := claudeHost(spendableStatus(), "")
		host.queue(claudeClaimURL, reply{status: 502})
		redeemer := first(t, host, newClock(), "claude")
		host.mu.Lock()
		host.status[claudeProfileURL] = 503
		host.mu.Unlock()
		if result, err := redeemer.Redeem(context.Background(), "claude", "c"); !errors.Is(err, ErrOutcomeUnknown) || result.MayHaveSpent {
			t.Fatalf("repeat with a failed read = %+v, %v", result, err)
		}
		if host.postCount() != 1 {
			t.Fatalf("claims = %d, want the first alone", host.postCount())
		}
	})

	t.Run("a repeat for another account", func(t *testing.T) {
		host := hostWith(map[string]string{codexCreditsURL: oneAvailable})
		host.queue(codexConsumeURL, reply{status: 502})
		redeemer := first(t, host, newClock(), "codex")
		host.setAuth(`{"access_token":"synthetic-token","account_id":"another-account"}`)
		if result, err := redeemer.Redeem(context.Background(), "codex", "c"); !errors.Is(err, ErrIdentityChanged) || result.MayHaveSpent {
			t.Fatalf("repeat = %+v, %v", result, err)
		}
	})

	t.Run("the window closing before the repeat is sent", func(t *testing.T) {
		clock := newClock()
		host := claudeHost(spendableStatus(), "")
		host.queue(claudeClaimURL, reply{status: 502})
		redeemer := first(t, host, clock, "claude")
		clock.advance(retryWindow - time.Second)
		host.onRequest = func(request protocol.HostHTTPRequest) {
			if request.URL == claudeProfileURL {
				clock.advance(2 * time.Second)
			}
		}
		if result, err := redeemer.Redeem(context.Background(), "claude", "c"); !errors.Is(err, ErrRetryWindowClosed) || result.MayHaveSpent {
			t.Fatalf("repeat = %+v, %v", result, err)
		}
	})

	t.Run("the window closing while the repeat is in flight", func(t *testing.T) {
		clock := newClock()
		host := hostWith(map[string]string{codexCreditsURL: oneAvailable})
		host.queue(codexConsumeURL, reply{status: 502}, reply{status: 504})
		redeemer := first(t, host, clock, "codex")
		clock.advance(retryWindow - 10*time.Second)
		host.onRequest = func(request protocol.HostHTTPRequest) {
			if request.Method == "POST" {
				clock.advance(20 * time.Second)
			}
		}
		if result, err := redeemer.Redeem(context.Background(), "codex", "c"); !errors.Is(err, ErrRetryWindowClosed) || !result.MayHaveSpent {
			t.Fatalf("repeat = %+v, %v", result, err)
		}
	})
}
