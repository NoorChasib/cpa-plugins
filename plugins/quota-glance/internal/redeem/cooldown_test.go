package redeem

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
)

// A reset the provider confirmed is followed by clearing CPA's routing
// cooldown on that same credential, after the spend and never before it: CPA
// otherwise keeps skipping an account the provider has just reset, for as long
// as the cooldown it recorded at the limit still runs.
func TestAConfirmedResetClearsThatCredentialsCooldown(t *testing.T) {
	codex := hostWith(map[string]string{codexCreditsURL: oneAvailable, codexConsumeURL: `{"code":"reset","windows_reset":2}`})
	result, err := redeemerOn(codex, newClock()).Redeem(context.Background(), "codex", "codex-a")
	if err != nil || result.Outcome != OutcomeReset || result.Cooldown != CooldownCleared {
		t.Fatalf("codex = %+v, %v; want a reset with its cooldown cleared", result, err)
	}
	if got := codex.cleared(); !slices.Equal(got, []string{"codex-a"}) {
		t.Fatalf("codex cleared %v, want exactly the credential that was reset", got)
	}
	if codex.cooldownOrder[0] != 2 {
		t.Fatalf("codex clear came after %d provider requests, want after the inventory and the consume", codex.cooldownOrder[0])
	}

	claude := claudeHost(spendableStatus(), `{"result":"reset","resets_left":1,"cleared":["five_hour"]}`)
	result, err = redeemerOn(claude, newClock()).Redeem(context.Background(), "claude", "claude-a")
	if err != nil || result.Outcome != OutcomeReset || result.Cooldown != CooldownCleared {
		t.Fatalf("claude = %+v, %v; want a reset with its cooldown cleared", result, err)
	}
	if got := claude.cleared(); !slices.Equal(got, []string{"claude-a"}) || claude.cooldownOrder[0] != 3 {
		t.Fatalf("claude cleared %v after %v requests, want claude-a after profile, status and claim", got, claude.cooldownOrder)
	}
}

// The press after an unknown outcome repeats the claim, and a provider that
// then reports it already spent is confirming the earlier attempt's reset. That
// is the one other answer that clears the cooldown. The unknown press itself
// clears nothing: it does not know a reset happened.
func TestARepeatThatConfirmsTheEarlierResetClearsTheCooldown(t *testing.T) {
	cases := map[string]struct {
		host     *fakeHost
		provider string
		url      string
		answers  []reply
		outcome  string
	}{
		"claude already used": {
			host: claudeHost(spendableStatus(), ""), provider: "claude", url: claudeClaimURL,
			answers: []reply{{status: 504}, {body: `{"result":"already_used","resets_left":1}`}}, outcome: OutcomeAlreadyUsed,
		},
		"claude reset on repeat": {
			host: claudeHost(spendableStatus(), ""), provider: "claude", url: claudeClaimURL,
			answers: []reply{{err: errors.New("connection reset")}, {body: `{"result":"reset"}`}}, outcome: OutcomeReset,
		},
		"codex already redeemed": {
			host: hostWith(map[string]string{codexCreditsURL: oneAvailable}), provider: "codex", url: codexConsumeURL,
			answers: []reply{{status: 502}, {body: `{"code":"already_redeemed"}`}}, outcome: OutcomeAlreadyUsed,
		},
		"codex reset on repeat": {
			host: hostWith(map[string]string{codexCreditsURL: oneAvailable}), provider: "codex", url: codexConsumeURL,
			answers: []reply{{err: errors.New("context deadline exceeded")}, {body: `{"code":"reset"}`}}, outcome: OutcomeReset,
		},
	}
	for name, tc := range cases {
		tc.host.queue(tc.url, tc.answers...)
		redeemer := redeemerOn(tc.host, newClock())
		if _, err := redeemer.Redeem(context.Background(), tc.provider, "c"); !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("%s: first press err = %v, want ErrOutcomeUnknown", name, err)
		}
		if got := tc.host.cleared(); len(got) != 0 {
			t.Fatalf("%s: an unknown outcome cleared the cooldown on %v", name, got)
		}
		result, err := redeemer.Redeem(context.Background(), tc.provider, "c")
		if err != nil || result.Outcome != tc.outcome || result.Cooldown != CooldownCleared {
			t.Fatalf("%s: repeat = %+v, %v; want %s with the cooldown cleared", name, result, err, tc.outcome)
		}
		if got := tc.host.cleared(); !slices.Equal(got, []string{"c"}) {
			t.Fatalf("%s: cleared %v, want the one credential once", name, got)
		}
		if tc.host.postCount() != 2 {
			t.Fatalf("%s: %d spend requests, want the claim and its one repeat", name, tc.host.postCount())
		}
	}
}

// Nothing but a confirmed reset is authority to clear CPA's cooldown. A
// refusal, an outcome the provider would not confirm, a failed read, and a
// fresh claim that finds its grant already spent by someone else all leave it
// exactly where CPA put it.
func TestNothingButAConfirmedResetTouchesTheCooldown(t *testing.T) {
	type press struct {
		host     *fakeHost
		provider string
	}
	refusedClaude := func(answer string) press {
		return press{claudeHost(spendableStatus(), answer), "claude"}
	}
	blockedClaude := func(block string) press {
		return press{claudeHost(statusWith(block), `{"result":"reset"}`), "claude"}
	}
	codexWith := func(inventory, consume string) press {
		return press{hostWith(map[string]string{codexCreditsURL: inventory, codexConsumeURL: consume}), "codex"}
	}
	failing := func(p press, url string, r reply) press {
		p.host.queue(url, r)
		return p
	}
	cases := map[string]press{
		"codex nothing to reset":      codexWith(oneAvailable, `{"code":"nothing_to_reset"}`),
		"codex no credit at consume":  codexWith(oneAvailable, `{"code":"no_credit"}`),
		"codex already redeemed":      codexWith(oneAvailable, `{"code":"already_redeemed"}`),
		"codex nothing available":     codexWith(`{"credits":[]}`, `{"code":"reset"}`),
		"codex consume refused":       failing(codexWith(oneAvailable, `{"code":"reset"}`), codexConsumeURL, reply{status: 403}),
		"codex consume throttled":     failing(codexWith(oneAvailable, `{"code":"reset"}`), codexConsumeURL, reply{status: 429}),
		"codex consume unknown":       failing(codexWith(oneAvailable, `{"code":"reset"}`), codexConsumeURL, reply{status: 502}),
		"codex consume lost":          failing(codexWith(oneAvailable, `{"code":"reset"}`), codexConsumeURL, reply{err: errors.New("eof")}),
		"codex inventory unreachable": failing(codexWith(oneAvailable, `{"code":"reset"}`), codexCreditsURL, reply{err: errors.New("eof")}),
		"claude refused cooldown":     refusedClaude(`{"result":"cooldown"}`),
		"claude refused not limited":  refusedClaude(`{"result":"not_limited"}`),
		"claude refused ineligible":   refusedClaude(`{"result":"ineligible"}`),
		"claude refused unavailable":  refusedClaude(`{"result":"unavailable"}`),
		"claude fresh already used":   refusedClaude(`{"result":"already_used","resets_left":0}`),
		"claude unreadable 2xx":       refusedClaude(`{"result":"something_new"}`),
		"claude claim 401":            failing(refusedClaude(`{"result":"reset"}`), claudeClaimURL, reply{status: 401}),
		"claude claim 504":            failing(refusedClaude(`{"result":"reset"}`), claudeClaimURL, reply{status: 504}),
		"claude status unreachable":   failing(refusedClaude(`{"result":"reset"}`), claudeStatusURL, reply{err: errors.New("eof")}),
		"claude ineligible status":    blockedClaude(`{"eligible":false,"grants":[` + spendableGrant + `]}`),
		"claude cooldown status": blockedClaude(`{"eligible":true,"at_limit":true,"cooldown_until":"2026-10-06T00:00:00Z",` +
			`"grants":[` + spendableGrant + `]}`),
		"claude not at limit": blockedClaude(`{"eligible":true,"at_limit":false,"grants":[` + spendableGrant + `]}`),
	}
	for name, p := range cases {
		result, _ := redeemerOn(p.host, newClock()).Redeem(context.Background(), p.provider, "c")
		if got := p.host.cleared(); len(got) != 0 {
			t.Fatalf("%s: outcome %q cleared the cooldown on %v", name, result.Outcome, got)
		}
		if result.Cooldown != "" {
			t.Fatalf("%s: outcome %q reported cooldown %q, want none", name, result.Outcome, result.Cooldown)
		}
	}
}

// A Codex consume accepted with a code this build does not know, or a body it
// cannot read, is still reported as a reset, because the credit is gone. But it
// does not say the windows were cleared, so CPA's cooldown is left alone and
// the answer says why.
func TestAnUnconfirmedResetLeavesTheCooldownAndSaysSo(t *testing.T) {
	for name, body := range map[string]string{
		"unknown code": `{"code":"something_new"}`,
		"unreadable":   `not json`,
		"empty object": `{}`,
	} {
		host := hostWith(map[string]string{codexCreditsURL: oneAvailable, codexConsumeURL: body})
		result, err := redeemerOn(host, newClock()).Redeem(context.Background(), "codex", "c")
		if err != nil || result.Outcome != OutcomeReset || result.Cooldown != CooldownUnconfirmed {
			t.Fatalf("%s: result = %+v, %v; want a reset reported with its cooldown unconfirmed", name, result, err)
		}
		if got := host.cleared(); len(got) != 0 {
			t.Fatalf("%s: an unconfirmed reset cleared the cooldown on %v", name, got)
		}
	}
}

// A clear that fails after the provider confirmed a reset leaves a reset spent
// and CPA still skipping the account. That is reported as exactly that: the
// reset, with the clear's own status beside it — never an error, which would
// read as nothing spent, and never as the unknown outcome that would make the
// next press repeat a claim. The claim is settled: the next press starts
// fresh from the inventory, and no press ever sends a second spend for this
// one.
func TestAFailedClearIsAPartialSuccessAndNeverAnotherSpend(t *testing.T) {
	cases := map[string]struct {
		err  error
		echo func(string) string
		want string
	}{
		"host error":         {err: errors.New("core auth manager unavailable"), want: CooldownFailed},
		"auth not found":     {err: errors.New("auth not found for auth_index c"), want: CooldownFailed},
		"unsupported host":   {err: fmt.Errorf("host callback: %w", protocol.ErrUnsupportedCallback), want: CooldownUnsupported},
		"another credential": {echo: func(string) string { return "someone-else" }, want: CooldownFailed},
		"no credential":      {echo: func(string) string { return "" }, want: CooldownFailed},
		"same with spaces":   {echo: func(s string) string { return "  " + s + " " }, want: CooldownCleared},
	}
	for name, tc := range cases {
		host := hostWith(map[string]string{codexCreditsURL: oneAvailable, codexConsumeURL: `{"code":"reset","windows_reset":1}`})
		host.cooldownErr, host.cooldownEcho = tc.err, tc.echo
		redeemer := redeemerOn(host, newClock())
		result, err := redeemer.Redeem(context.Background(), "codex", "c")
		if err != nil || result.Outcome != OutcomeReset || result.WindowsReset != 1 || result.Cooldown != tc.want {
			t.Fatalf("%s: result = %+v, %v; want a reset with cooldown %q", name, result, err, tc.want)
		}
		if host.postCount() != 1 || len(host.cleared()) != 1 {
			t.Fatalf("%s: %d spends and %d clears for one press, want one of each", name, host.postCount(), len(host.cleared()))
		}
		if _, isRetry := redeemer.unresolved("c", "codex"); isRetry {
			t.Fatalf("%s: a failed clear left the settled claim on the journal to be repeated", name)
		}
		// The next press is a new press: a fresh inventory read and a fresh
		// idempotency key, which is what an operator pressing again gets.
		if _, err := redeemer.Redeem(context.Background(), "codex", "c"); err != nil {
			t.Fatalf("%s: next press: %v", name, err)
		}
		bodies := host.postBodies(t)
		if host.calls(codexCreditsURL) != 2 || bodies[1]["redeem_request_id"] == bodies[0]["redeem_request_id"] {
			t.Fatalf("%s: the press after a failed clear repeated the settled claim: %v", name, bodies)
		}
	}
}

// The clear is owed once the provider has confirmed the reset, even if the
// press's own deadline ran out while the spend was answered. It carries its
// own bound rather than inheriting a context that has already ended.
func TestTheClearSurvivesThePressContextEnding(t *testing.T) {
	host := claudeHost(spendableStatus(), `{"result":"reset"}`)
	ctx, cancel := context.WithCancel(context.Background())
	host.onRequest = func(request protocol.HostHTTPRequest) {
		if request.Method == "POST" {
			cancel()
		}
	}
	result, err := redeemerOn(host, newClock()).Redeem(ctx, "claude", "c")
	if err != nil || result.Cooldown != CooldownCleared {
		t.Fatalf("result = %+v, %v; want the cooldown cleared", result, err)
	}
	if got := host.cooldownCtxErr; len(got) != 1 || got[0] != nil {
		t.Fatalf("the clear ran on an ended context: %v", got)
	}
}

// One credential's reset clears that credential's cooldown and no other, and
// a press on one never waits on, or clears for, another.
func TestTheClearIsPerCredential(t *testing.T) {
	host := hostWith(map[string]string{codexCreditsURL: oneAvailable, codexConsumeURL: `{"code":"reset"}`})
	redeemer := redeemerOn(host, newClock())
	if result, err := redeemer.Redeem(context.Background(), "codex", "codex-a"); err != nil || result.Cooldown != CooldownCleared {
		t.Fatalf("a = %+v, %v", result, err)
	}
	host.queue(codexConsumeURL, reply{body: `{"code":"nothing_to_reset"}`})
	if result, err := redeemer.Redeem(context.Background(), "codex", "codex-b"); err != nil || result.Cooldown != "" {
		t.Fatalf("b = %+v, %v; want nothing to reset and no clear", result, err)
	}
	if got := host.cleared(); !slices.Equal(got, []string{"codex-a"}) {
		t.Fatalf("cleared %v, want codex-a alone", got)
	}
}

// The credential stays reserved while the clear is waited for, so a second
// press arriving in that time — a double-click, another tab — is refused
// rather than sent to the provider as a second spend.
func TestAPressDuringTheClearIsRefused(t *testing.T) {
	host := hostWith(map[string]string{codexCreditsURL: oneAvailable, codexConsumeURL: `{"code":"reset"}`})
	redeemer := redeemerOn(host, newClock())
	var during error
	host.onCooldown = func() {
		_, during = redeemer.Redeem(context.Background(), "codex", "c")
	}
	if result, err := redeemer.Redeem(context.Background(), "codex", "c"); err != nil || result.Cooldown != CooldownCleared {
		t.Fatalf("first press = %+v, %v", result, err)
	}
	if !errors.Is(during, ErrInFlight) {
		t.Fatalf("a press during the clear got %v, want ErrInFlight", during)
	}
	if host.postCount() != 1 || len(host.cleared()) != 1 {
		t.Fatalf("%d spends and %d clears, want one of each", host.postCount(), len(host.cleared()))
	}
}

// CPA serves the clear synchronously, with no deadline across the ABI, so a
// host that stalls is not stopped by the clear's context. The press still
// answers within the clear's bound: the reset, with the cooldown reported not
// cleared, rather than a press held open until the browser gives up and its
// answer lost. The credential is free again for the next press, and the
// settled claim is not repeated.
func TestAStalledClearDoesNotHoldThePress(t *testing.T) {
	host := hostWith(map[string]string{codexCreditsURL: oneAvailable, codexConsumeURL: `{"code":"reset"}`})
	release := make(chan struct{})
	host.onCooldown = func() { <-release }
	redeemer := redeemerOn(host, newClock())
	redeemer.cooldownWait = 20 * time.Millisecond

	answered := make(chan Result, 1)
	go func() {
		result, err := redeemer.Redeem(context.Background(), "codex", "c")
		if err != nil {
			t.Errorf("press: %v", err)
		}
		answered <- result
	}()
	select {
	case result := <-answered:
		if result.Outcome != OutcomeReset || result.Cooldown != CooldownFailed {
			t.Fatalf("result = %+v, want a reset with its cooldown not cleared", result)
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("a stalled clear held the press open past its bound")
	}
	if _, isRetry := redeemer.unresolved("c", "codex"); isRetry {
		t.Fatal("a stalled clear left the settled claim on the journal")
	}
	host.mu.Lock()
	host.onCooldown = nil
	host.mu.Unlock()
	if result, err := redeemer.Redeem(context.Background(), "codex", "c"); err != nil || result.Cooldown != CooldownCleared {
		t.Fatalf("next press = %+v, %v; want the credential free and a fresh claim", result, err)
	}
	close(release)
	if host.postCount() != 2 || host.calls(codexCreditsURL) != 2 {
		t.Fatalf("%d spends and %d inventory reads, want two presses' worth", host.postCount(), host.calls(codexCreditsURL))
	}
}
