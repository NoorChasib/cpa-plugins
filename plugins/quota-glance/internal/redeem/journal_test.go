package redeem

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
)

// The press after an unknown outcome must not be able to spend a second reset.
// So it repeats the first claim exactly — the same credit, the same request id
// — and skips the inventory read, which would see the credit the first claim
// may have spent as gone and pick another.
func TestAnUnknownCodexClaimIsRepeatedNotReplaced(t *testing.T) {
	clock := newClock()
	host := hostWith(map[string]string{codexCreditsURL: oneAvailable, codexConsumeURL: `{"code":"reset","windows_reset":2}`})
	host.queue(codexConsumeURL, reply{err: errors.New("context deadline exceeded")})
	redeemer := redeemerOn(host, clock)

	if _, err := redeemer.Redeem(context.Background(), "codex", "c"); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("first press err = %v, want ErrOutcomeUnknown", err)
	}
	clock.advance(3 * time.Minute)
	result, err := redeemer.Redeem(context.Background(), "codex", "c")
	if err != nil || result.Outcome != OutcomeReset || result.WindowsReset != 2 {
		t.Fatalf("repeat = %+v, %v", result, err)
	}
	if host.calls(codexCreditsURL) != 1 {
		t.Fatalf("the repeat read the inventory again (%d reads)", host.calls(codexCreditsURL))
	}
	bodies := host.postBodies(t)
	if len(bodies) != 2 || bodies[0]["credit_id"] != bodies[1]["credit_id"] || bodies[0]["redeem_request_id"] != bodies[1]["redeem_request_id"] {
		t.Fatalf("consumes = %v, want the same claim twice", bodies)
	}

	// Settled, so the next press is a fresh claim with a fresh key.
	if _, err := redeemer.Redeem(context.Background(), "codex", "c"); err != nil {
		t.Fatal(err)
	}
	bodies = host.postBodies(t)
	if host.calls(codexCreditsURL) != 2 || bodies[2]["redeem_request_id"] == bodies[0]["redeem_request_id"] {
		t.Fatalf("a settled claim was repeated: reads = %d, consumes = %v", host.calls(codexCreditsURL), bodies)
	}
}

// Claude's repeat reads the organization again, because the claim is addressed
// to it, and nothing else: no status read, the same grant, the same request id.
func TestAnUnknownClaudeClaimIsRepeatedNotReplaced(t *testing.T) {
	clock := newClock()
	host := claudeHost(spendableStatus(), `{"result":"reset","resets_left":1}`)
	host.queue(claudeClaimURL, reply{status: 504})
	redeemer := redeemerOn(host, clock)

	if _, err := redeemer.Redeem(context.Background(), "claude", "c"); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("first press err = %v, want ErrOutcomeUnknown", err)
	}
	clock.advance(9 * time.Minute)
	result, err := redeemer.Redeem(context.Background(), "claude", "c")
	if err != nil || result.Outcome != OutcomeReset || result.RemainingCount != 1 {
		t.Fatalf("repeat = %+v, %v", result, err)
	}
	if host.calls(claudeProfileURL) != 2 || host.calls(claudeStatusURL) != 1 {
		t.Fatalf("profile reads = %d, status reads = %d; want the organization re-read and the status skipped",
			host.calls(claudeProfileURL), host.calls(claudeStatusURL))
	}
	bodies := host.postBodies(t)
	if len(bodies) != 2 || bodies[0]["grant_id"] != bodies[1]["grant_id"] || bodies[0]["request_id"] != bodies[1]["request_id"] {
		t.Fatalf("claims = %v, want the same claim twice", bodies)
	}
}

// A refusal on the repeat proves only that the repeat spent nothing. Claude
// refuses a second reset inside its cooldown, which is exactly what it would
// say had the first one gone through — so the outcome stays unknown, and the
// claim stays repeatable.
func TestARefusalOnARepeatDoesNotSettleTheClaim(t *testing.T) {
	for name, refusal := range map[string]reply{
		"cooldown":     {body: `{"result":"cooldown"}`},
		"not limited":  {body: `{"result":"not_limited"}`},
		"ineligible":   {body: `{"result":"ineligible"}`},
		"unavailable":  {body: `{"result":"unavailable"}`},
		"throttled":    {status: 429},
		"unauthorized": {status: 401},
	} {
		host := claudeHost(spendableStatus(), `{"result":"reset"}`)
		host.queue(claudeClaimURL, reply{err: errors.New("connection reset")}, refusal)
		redeemer := redeemerOn(host, newClock())

		if _, err := redeemer.Redeem(context.Background(), "claude", "c"); !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("%s: first press err = %v", name, err)
		}
		if _, err := redeemer.Redeem(context.Background(), "claude", "c"); !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("%s: a refused repeat reported %v, as if it settled the first claim", name, err)
		}
		// Still the same claim on the third press.
		if result, err := redeemer.Redeem(context.Background(), "claude", "c"); err != nil || result.Outcome != OutcomeReset {
			t.Fatalf("%s: third press = %+v, %v", name, result, err)
		}
		bodies := host.postBodies(t)
		if len(bodies) != 3 || bodies[2]["request_id"] != bodies[0]["request_id"] || host.calls(claudeStatusURL) != 1 {
			t.Fatalf("%s: the claim was replaced after a refused repeat: %v", name, bodies)
		}
	}
}

// The same holds for Codex: "no credit" on the repeat is what the account
// would say if the first consume had spent its last one.
func TestARefusalOnACodexRepeatDoesNotSettleTheClaim(t *testing.T) {
	for name, refusal := range map[string]reply{
		"no credit":    {body: `{"code":"no_credit"}`},
		"throttled":    {status: 429},
		"unauthorized": {status: 403},
	} {
		host := hostWith(map[string]string{codexCreditsURL: oneAvailable, codexConsumeURL: `{"code":"reset"}`})
		host.queue(codexConsumeURL, reply{status: 502}, refusal)
		redeemer := redeemerOn(host, newClock())
		if _, err := redeemer.Redeem(context.Background(), "codex", "c"); !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("%s: first press err = %v", name, err)
		}
		if _, err := redeemer.Redeem(context.Background(), "codex", "c"); !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("%s: a refused repeat reported %v", name, err)
		}
		if _, err := redeemer.Redeem(context.Background(), "codex", "c"); err != nil || host.calls(codexCreditsURL) != 1 {
			t.Fatalf("%s: the claim was not repeated a third time: err = %v, reads = %d", name, err, host.calls(codexCreditsURL))
		}
	}
}

// "Already used" on the repeat is the provider saying the claimed reset is
// gone — the earlier attempt went through. That settles it.
func TestAlreadyUsedOnARepeatSettlesTheClaim(t *testing.T) {
	claude := claudeHost(spendableStatus(), `{"result":"reset"}`)
	claude.queue(claudeClaimURL, reply{status: 500}, reply{body: `{"result":"already_used","resets_left":1}`})
	redeemer := redeemerOn(claude, newClock())
	_, _ = redeemer.Redeem(context.Background(), "claude", "c")
	result, err := redeemer.Redeem(context.Background(), "claude", "c")
	if err != nil || result.Outcome != OutcomeAlreadyUsed || result.RemainingCount != 1 {
		t.Fatalf("claude repeat = %+v, %v", result, err)
	}
	if _, err := redeemer.Redeem(context.Background(), "claude", "c"); err != nil || claude.calls(claudeStatusURL) != 2 {
		t.Fatalf("the settled claim was repeated: err = %v, status reads = %d", err, claude.calls(claudeStatusURL))
	}

	codex := hostWith(map[string]string{codexCreditsURL: oneAvailable, codexConsumeURL: `{"code":"reset"}`})
	codex.queue(codexConsumeURL, reply{status: 500}, reply{body: `{"code":"already_redeemed"}`})
	redeemer = redeemerOn(codex, newClock())
	_, _ = redeemer.Redeem(context.Background(), "codex", "c")
	if result, err := redeemer.Redeem(context.Background(), "codex", "c"); err != nil || result.Outcome != OutcomeAlreadyUsed {
		t.Fatalf("codex repeat = %+v, %v", result, err)
	}
}

// A claim is addressed to one organization. If the credential now signs in to
// another, the repeat is refused before anything is sent, and the claim stays
// on record for the account it was made for.
func TestAClaimIsRepeatedOnlyForTheSameAccount(t *testing.T) {
	host := claudeHost(spendableStatus(), `{"result":"reset"}`)
	host.queue(claudeClaimURL, reply{status: 502})
	redeemer := redeemerOn(host, newClock())
	_, _ = redeemer.Redeem(context.Background(), "claude", "c")

	host.mu.Lock()
	host.bodies[claudeProfileURL] = `{"organization":{"uuid":"7c9e6679-7425-40de-944b-e07fc1f90ae7"}}`
	host.mu.Unlock()
	if _, err := redeemer.Redeem(context.Background(), "claude", "c"); !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("err = %v, want ErrIdentityChanged", err)
	}
	if host.postCount() != 1 {
		t.Fatalf("a claim was sent to a different organization (%d claims)", host.postCount())
	}

	host.mu.Lock()
	host.bodies[claudeProfileURL] = claudeProfile
	host.mu.Unlock()
	if _, err := redeemer.Redeem(context.Background(), "claude", "c"); err != nil {
		t.Fatal(err)
	}
	if bodies := host.postBodies(t); len(bodies) != 2 || bodies[1]["request_id"] != bodies[0]["request_id"] {
		t.Fatalf("the original claim was not kept for its own account: %v", bodies)
	}

	// Codex holds its claim to the ChatGPT account the same way.
	codex := hostWith(map[string]string{codexCreditsURL: oneAvailable, codexConsumeURL: `{"code":"reset"}`})
	codex.queue(codexConsumeURL, reply{status: 502})
	redeemer = redeemerOn(codex, newClock())
	_, _ = redeemer.Redeem(context.Background(), "codex", "c")
	codex.setAuth(`{"access_token":"synthetic-token","account_id":"another-account"}`)
	if _, err := redeemer.Redeem(context.Background(), "codex", "c"); !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("codex err = %v, want ErrIdentityChanged", err)
	}
	if codex.postCount() != 1 {
		t.Fatal("a consume was sent for a different account")
	}
}

// Ten minutes on, the claim is no longer repeated: the provider stops matching
// a request id against its original, so the next press starts again from a
// fresh read — which finds the reset gone if the first claim did spend it.
func TestTheRetryWindowCloses(t *testing.T) {
	clock := newClock()
	host := claudeHost(spendableStatus(), `{"result":"reset"}`)
	host.queue(claudeClaimURL, reply{status: 502}, reply{status: 502})
	redeemer := redeemerOn(host, clock)
	_, _ = redeemer.Redeem(context.Background(), "claude", "c")

	clock.advance(retryWindow - time.Second)
	if _, err := redeemer.Redeem(context.Background(), "claude", "c"); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("inside the window err = %v", err)
	}
	if host.calls(claudeStatusURL) != 1 {
		t.Fatal("a repeat inside the window read the status")
	}

	clock.advance(time.Second)
	if _, err := redeemer.Redeem(context.Background(), "claude", "c"); err != nil {
		t.Fatal(err)
	}
	bodies := host.postBodies(t)
	if host.calls(claudeStatusURL) != 2 || len(bodies) != 3 || bodies[2]["request_id"] == bodies[0]["request_id"] {
		t.Fatalf("after the window: status reads = %d, claims = %v; want a fresh claim", host.calls(claudeStatusURL), bodies)
	}
}

// The window is checked again immediately before the repeat is sent. A repeat
// that began inside it and would leave after it is not sent at all — and the
// answer says the window has closed, because the next press is a fresh claim
// and an outcome_unknown here would promise it was not.
func TestTheWindowClosingMidRepeatSendsNothing(t *testing.T) {
	clock := newClock()
	host := claudeHost(spendableStatus(), `{"result":"reset"}`)
	host.queue(claudeClaimURL, reply{status: 502})
	redeemer := redeemerOn(host, clock)
	_, _ = redeemer.Redeem(context.Background(), "claude", "c")

	clock.advance(retryWindow - time.Second)
	host.onRequest = func(request protocol.HostHTTPRequest) {
		if request.URL == claudeProfileURL {
			clock.advance(2 * time.Second)
		}
	}
	_, err := redeemer.Redeem(context.Background(), "claude", "c")
	if !errors.Is(err, ErrRetryWindowClosed) || errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("err = %v, want ErrRetryWindowClosed and nothing that promises a repeat", err)
	}
	if host.postCount() != 1 {
		t.Fatal("a repeat was sent after its window had closed")
	}
	host.onRequest = nil
	if _, err := redeemer.Redeem(context.Background(), "claude", "c"); err != nil || host.calls(claudeStatusURL) != 2 {
		t.Fatalf("the next press did not start fresh: err = %v, status reads = %d", err, host.calls(claudeStatusURL))
	}
}

// twoAvailable is an account holding two credits, which is the account on which
// a press that is not a repeat spends a second one.
const twoAvailable = `{"credits":[{"id":"credit-a","status":"available","expires_at":"2026-10-20T00:00:00Z"},` +
	`{"id":"credit-b","status":"available","expires_at":"2026-10-25T00:00:00Z"}]}`

// Every unknown answer names the same deadline: the one the claim got when it
// was first made. A repeat does not restart it, so an answer that said "ten
// more minutes" on a repeat would promise time the claim no longer has — and a
// press inside that promised time but past the real deadline is a fresh claim,
// which on Codex spends a second credit.
func TestEveryUnknownAnswerNamesTheOriginalDeadline(t *testing.T) {
	clock := newClock()
	madeAt := clock.Now()
	host := hostWith(map[string]string{codexCreditsURL: twoAvailable, codexConsumeURL: `{"code":"reset"}`})
	host.queue(codexConsumeURL, reply{err: errors.New("context deadline exceeded")}, reply{body: `{"code":"no_credit"}`})
	redeemer := redeemerOn(host, clock)

	deadlineOf := func(err error) time.Time {
		t.Helper()
		var unknown *OutcomeUnknownError
		if !errors.As(err, &unknown) {
			t.Fatalf("err = %v, want an OutcomeUnknownError naming its deadline", err)
		}
		return unknown.RetryUntil
	}
	_, err := redeemer.Redeem(context.Background(), "codex", "c")
	if first := deadlineOf(err); !first.Equal(madeAt.Add(retryWindow)) {
		t.Fatalf("first deadline = %s, want %s", first, madeAt.Add(retryWindow))
	}
	// Eight minutes on, the repeat is refused, which proves nothing about the
	// first consume. Its answer must still name the first deadline.
	clock.advance(8 * time.Minute)
	_, err = redeemer.Redeem(context.Background(), "codex", "c")
	if second := deadlineOf(err); !second.Equal(madeAt.Add(retryWindow)) {
		t.Fatalf("the repeat moved the deadline to %s; the claim's window closes at %s", second, madeAt.Add(retryWindow))
	}
	if bodies := host.postBodies(t); len(bodies) != 2 || bodies[1]["redeem_request_id"] != bodies[0]["redeem_request_id"] {
		t.Fatalf("consumes = %v, want the same claim twice", bodies)
	}
}

// A repeat sent inside the window whose answer is lost after the window has
// closed leaves nothing to repeat: the claim is dropped, and the answer says the
// window has closed rather than offering another repeat.
func TestARepeatAnsweredAfterTheWindowClosesSaysSo(t *testing.T) {
	clock := newClock()
	host := hostWith(map[string]string{codexCreditsURL: oneAvailable, codexConsumeURL: `{"code":"reset"}`})
	host.queue(codexConsumeURL, reply{status: 502}, reply{status: 504})
	redeemer := redeemerOn(host, clock)
	_, _ = redeemer.Redeem(context.Background(), "codex", "c")

	clock.advance(retryWindow - 10*time.Second)
	host.onRequest = func(request protocol.HostHTTPRequest) {
		if request.Method == "POST" {
			clock.advance(20 * time.Second)
		}
	}
	if _, err := redeemer.Redeem(context.Background(), "codex", "c"); !errors.Is(err, ErrRetryWindowClosed) {
		t.Fatalf("err = %v, want ErrRetryWindowClosed", err)
	}
	if host.postCount() != 2 {
		t.Fatalf("consumes = %d, want the repeat sent once", host.postCount())
	}
	host.onRequest = nil
	if _, err := redeemer.Redeem(context.Background(), "codex", "c"); err != nil || host.calls(codexCreditsURL) != 2 {
		t.Fatalf("the next press did not start fresh: err = %v, inventory reads = %d", err, host.calls(codexCreditsURL))
	}
}

// A refusal on a first claim does settle it: that claim spent nothing, and
// there is no earlier one it could be hiding. The next press starts fresh.
func TestARefusedFirstClaimLeavesNothingToRepeat(t *testing.T) {
	host := claudeHost(spendableStatus(), `{"result":"reset"}`)
	host.queue(claudeClaimURL, reply{body: `{"result":"not_limited"}`})
	redeemer := redeemerOn(host, newClock())
	if result, err := redeemer.Redeem(context.Background(), "claude", "c"); err != nil || result.Outcome != OutcomeNotLimited {
		t.Fatalf("first press = %+v, %v", result, err)
	}
	if _, err := redeemer.Redeem(context.Background(), "claude", "c"); err != nil || host.calls(claudeStatusURL) != 2 {
		t.Fatalf("a refused claim was repeated: err = %v, status reads = %d", err, host.calls(claudeStatusURL))
	}

	codex := hostWith(map[string]string{codexCreditsURL: oneAvailable, codexConsumeURL: `{"code":"reset"}`})
	codex.queue(codexConsumeURL, reply{status: 401})
	redeemer = redeemerOn(codex, newClock())
	if _, err := redeemer.Redeem(context.Background(), "codex", "c"); !errors.Is(err, ErrRefused) {
		t.Fatalf("codex first press err = %v", err)
	}
	if _, err := redeemer.Redeem(context.Background(), "codex", "c"); err != nil || codex.calls(codexCreditsURL) != 2 {
		t.Fatalf("a refused consume was repeated: err = %v, reads = %d", err, codex.calls(codexCreditsURL))
	}
}

// Each credential has its own claim on record. An unknown outcome on one
// account does not turn a press on another into a repeat.
func TestTheJournalIsPerCredential(t *testing.T) {
	host := claudeHost(spendableStatus(), `{"result":"reset"}`)
	host.queue(claudeClaimURL, reply{status: 502})
	redeemer := redeemerOn(host, newClock())
	_, _ = redeemer.Redeem(context.Background(), "claude", "claude-one@example.com.json")
	if _, err := redeemer.Redeem(context.Background(), "claude", "claude-two@example.com.json"); err != nil {
		t.Fatal(err)
	}
	if host.calls(claudeStatusURL) != 2 {
		t.Fatal("a second credential's press was treated as the first one's repeat")
	}
}

// The in-flight guard still holds during a repeat: a second press while the
// repeat is mid-flight is refused, not run as a second repeat.
func TestAConcurrentPressDuringARepeatIsRefused(t *testing.T) {
	release := make(chan struct{})
	host := &blockingHost{fakeHost: *claudeHost(spendableStatus(), `{"result":"reset"}`), gate: release}
	clock := newClock()
	redeemer := redeemerOn(host, clock)
	// An unknown claim already on record for the credential.
	redeemer.remember("c", pendingClaim{
		provider: providerClaude, creditID: "grant-a", requestID: "0d4c3f8e-1b2a-4c5d-8e6f-7a8b9c0d1e2f",
		account: claudeOrg, createdAt: clock.Now(), grantLeft: 2,
	})

	done := make(chan error, 1)
	go func() {
		_, err := redeemer.Redeem(context.Background(), "claude", "c")
		done <- err
	}()
	host.waitUntilEntered()
	if _, err := redeemer.Redeem(context.Background(), "claude", "c"); !errors.Is(err, ErrInFlight) {
		t.Fatalf("second press err = %v, want ErrInFlight", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if host.postCount() != 1 || host.calls(claudeStatusURL) != 0 {
		t.Fatalf("claims = %d, status reads = %d; want one repeat and no status read", host.postCount(), host.calls(claudeStatusURL))
	}
}

// Entries past their window are swept when another claim is recorded, so the
// journal never grows past one entry per credential with a recent unknown.
func TestTheJournalSweepsExpiredEntries(t *testing.T) {
	clock := newClock()
	redeemer := redeemerOn(hostWith(nil), clock)
	redeemer.remember("old", pendingClaim{provider: providerCodex, createdAt: clock.Now()})
	clock.advance(retryWindow)
	redeemer.remember("new", pendingClaim{provider: providerCodex, createdAt: clock.Now()})
	redeemer.mu.Lock()
	defer redeemer.mu.Unlock()
	if _, kept := redeemer.pending["old"]; kept || len(redeemer.pending) != 1 {
		t.Fatalf("journal = %+v", redeemer.pending)
	}
}
