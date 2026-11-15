package redeem

import (
	"context"
	"errors"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// Grants of each kind as Claude Code 2.1.322's usage response carries them. The
// Fable grant lapses first and is the one the provider recommends, so a press
// that let either rule reach past the kind it asked for would take it.
const (
	fullGrant = `{"id":"grant-full","resets_total":1,"resets_left":1,"ends_at":"2026-10-30T00:00:00Z",` +
		`"type":"full","clears":["five_hour","seven_day"],"usable_now":true,"use_requires_limit":true}`
	fableGrant = `{"id":"grant-fable","resets_total":2,"resets_left":2,"ends_at":"2026-10-07T00:00:00Z",` +
		`"gives_back_weekly":true,"clears":["seven_day"],"usable_now":true,"use_requires_limit":true}`
)

// mixedStatus is an account at its limit holding one full reset and two Fable
// resets, with the Fable grant recommended.
func mixedStatus() string {
	return statusWith(`{"eligible":true,"at_limit":true,"next_grant_id":"grant-fable","grants":[` + fullGrant + `,` + fableGrant + `]}`)
}

// The kind asked for decides the grant claimed, and the provider's
// recommendation of another kind is passed over rather than followed.
func TestAClaudePressClaimsOnlyAGrantOfItsKind(t *testing.T) {
	for kind, want := range map[string]string{qc.ResetKindFull: "grant-full", qc.ResetKindFable: "grant-fable"} {
		host := claudeHost(mixedStatus(), `{"result":"reset"}`)
		result, err := redeemerOn(host, newClock()).Redeem(context.Background(), "claude", "c", kind)
		if err != nil || result.Outcome != OutcomeReset || result.Kind != kind {
			t.Fatalf("%s: result = %+v, err = %v", kind, result, err)
		}
		if got := host.postBody(t)["grant_id"]; got != want {
			t.Fatalf("%s: claimed %q, want %q", kind, got, want)
		}
	}
}

// chooseGrant considers the kind asked for and nothing else. When nothing of
// that kind can be spent, the reason given is that kind's own, however
// spendable the others are, and an account holding none of the kind has
// nothing to spend.
func TestChooseGrantNeverTakesAnotherKind(t *testing.T) {
	grant := func(id, kindFields, ends, extra string) string {
		return `{"id":"` + id + `","resets_total":1,"resets_left":1,"ends_at":"` + ends + `"` + kindFields + extra + `}`
	}
	const fable, full, weekly = `,"gives_back_weekly":true`, `,"type":"full"`, `,"clears":["seven_day"]`
	for name, one := range map[string]struct {
		block, kind, want, blocked string
	}{
		"a recommendation of another kind is passed over": {
			block: `{"eligible":true,"at_limit":true,"next_grant_id":"grant-b","grants":[` +
				grant("grant-a", full, "2026-10-30T00:00:00Z", `,"usable_now":true`) + `,` +
				grant("grant-b", fable, "2026-10-07T00:00:00Z", `,"usable_now":true`) + `]}`,
			kind: qc.ResetKindFull, want: "grant-a",
		},
		"the recommendation of the kind wins within it": {
			block: `{"eligible":true,"at_limit":true,"next_grant_id":"grant-c","grants":[` +
				grant("grant-a", fable, "2026-10-07T00:00:00Z", `,"usable_now":true`) + `,` +
				grant("grant-b", full, "2026-10-08T00:00:00Z", `,"usable_now":true`) + `,` +
				grant("grant-c", fable, "2026-10-30T00:00:00Z", `,"usable_now":true`) + `]}`,
			kind: qc.ResetKindFable, want: "grant-c",
		},
		"soonest of the kind, not soonest of all": {
			block: `{"eligible":true,"at_limit":true,"grants":[` +
				grant("grant-a", fable, "2026-10-06T00:00:00Z", `,"usable_now":true`) + `,` +
				grant("grant-b", weekly, "2026-10-20T00:00:00Z", `,"usable_now":true`) + `,` +
				grant("grant-c", weekly, "2026-10-10T00:00:00Z", `,"usable_now":true`) + `]}`,
			kind: qc.ResetKindWeekly, want: "grant-c",
		},
		"none of the kind held": {
			block: `{"eligible":true,"at_limit":true,"next_grant_id":"grant-a","grants":[` +
				grant("grant-a", fable, "2026-10-07T00:00:00Z", `,"usable_now":true`) + `]}`,
			kind: qc.ResetKindFull, blocked: OutcomeNoCredit,
		},
		"the kind paused while another is spendable": {
			block: `{"eligible":true,"at_limit":true,"next_grant_id":"grant-b","grants":[` +
				grant("grant-a", full, "2026-10-30T00:00:00Z", `,"usable_now":true,"paused":true`) + `,` +
				grant("grant-b", fable, "2026-10-07T00:00:00Z", `,"usable_now":true`) + `]}`,
			kind: qc.ResetKindFull, blocked: OutcomePaused,
		},
		"the kind needs a limit while another does not": {
			block: `{"eligible":true,"at_limit":false,"grants":[` +
				grant("grant-a", full, "2026-10-30T00:00:00Z", `,"usable_now":true,"use_requires_limit":true`) + `,` +
				grant("grant-b", fable, "2026-10-07T00:00:00Z", `,"usable_now":true,"use_requires_limit":false`) + `]}`,
			kind: qc.ResetKindFull, blocked: OutcomeNotLimited,
		},
		"the kind not usable now while another is": {
			block: `{"eligible":true,"at_limit":true,"grants":[` +
				grant("grant-a", full, "2026-10-30T00:00:00Z", `,"usable_now":false`) + `,` +
				grant("grant-b", fable, "2026-10-07T00:00:00Z", `,"usable_now":true`) + `]}`,
			kind: qc.ResetKindFull, blocked: OutcomeNoCredit,
		},
		// The preferred grant of the kind is the kind's own soonest, not the
		// recommendation of another kind, whose state says nothing about it.
		"the hold comes from the kind's own preferred grant": {
			block: `{"eligible":true,"at_limit":true,"next_grant_id":"grant-b","grants":[` +
				grant("grant-a", fable, "2026-10-07T00:00:00Z", `,"usable_now":true,"paused":true`) + `,` +
				grant("grant-b", full, "2026-10-30T00:00:00Z", `,"usable_now":false`) + `]}`,
			kind: qc.ResetKindFable, blocked: OutcomePaused,
		},
		"the account's own reasons hold for every kind": {
			block: `{"eligible":true,"at_limit":true,"cooldown_until":"2026-10-05T13:00:00Z","grants":[` +
				grant("grant-a", fable, "2026-10-07T00:00:00Z", `,"usable_now":true`) + `]}`,
			kind: qc.ResetKindFable, blocked: OutcomeCooldown,
		},
	} {
		status, ok := parseClaudeStatus(mustDecode(t, one.block))
		if !ok {
			t.Fatalf("%s: fixture did not parse", name)
		}
		got, blocked := chooseGrant(status, one.kind, newClock().Now())
		if blocked != one.blocked || got.id != one.want {
			t.Fatalf("%s: chose %q (blocked %q), want %q (blocked %q)", name, got.id, blocked, one.want, one.blocked)
		}
		if got.id != "" && got.kind != one.kind {
			t.Fatalf("%s: chose a %s grant for a %s press", name, got.kind, one.kind)
		}
	}
}

// End to end: a press for a kind the account cannot spend right now sends
// nothing, even with a grant of another kind ready to claim, and says how many
// of the kind asked for it holds.
func TestAPressForAKindThatCannotBeSpentSendsNothing(t *testing.T) {
	pausedFull := `{"id":"grant-full","resets_total":1,"resets_left":1,"type":"full","usable_now":true,"paused":true}`
	for name, one := range map[string]struct {
		block   string
		kind    string
		outcome string
		held    int
	}{
		"full paused, Fable ready": {
			block: `{"eligible":true,"at_limit":true,"next_grant_id":"grant-fable","grants":[` + pausedFull + `,` + fableGrant + `]}`,
			kind:  qc.ResetKindFull, outcome: OutcomePaused, held: 1,
		},
		"no full held, Fable ready": {
			block: `{"eligible":true,"at_limit":true,"next_grant_id":"grant-fable","grants":[` + fableGrant + `]}`,
			kind:  qc.ResetKindFull, outcome: OutcomeNoCredit, held: 0,
		},
		"no Fable held, full ready": {
			block: `{"eligible":true,"at_limit":true,"next_grant_id":"grant-full","grants":[` + fullGrant + `]}`,
			kind:  qc.ResetKindFable, outcome: OutcomeNoCredit, held: 0,
		},
	} {
		host := claudeHost(statusWith(one.block), `{"result":"reset"}`)
		result, err := redeemerOn(host, newClock()).Redeem(context.Background(), "claude", "c", one.kind)
		if err != nil || result.Outcome != one.outcome || result.RemainingCount != one.held || result.Kind != one.kind {
			t.Fatalf("%s: result = %+v, err = %v; want %s holding %d", name, result, err, one.outcome, one.held)
		}
		if host.posted() {
			t.Fatalf("%s: a claim was sent for a kind that could not be spent", name)
		}
	}
}

// A grant whose type is a string this build does not know is well formed —
// Anthropic adding a kind is expected — but it is of no kind a press can name,
// so it is counted for nothing a press reports and never claimed, even
// recommended and spendable.
func TestAGrantOfAnUnrecognisedTypeIsNeverClaimed(t *testing.T) {
	monthly := `{"id":"grant-new","resets_total":1,"resets_left":1,"type":"monthly","usable_now":true}`
	status, ok := parseClaudeStatus(mustDecode(t, `{"eligible":true,"at_limit":true,"next_grant_id":"grant-new","grants":[`+monthly+`]}`))
	if !ok || len(status.grants) != 1 || status.grants[0].kind != qc.ResetKindUnknown {
		t.Fatalf("status = %+v, ok = %v; want the grant kept and classified unknown", status, ok)
	}
	for _, kind := range []string{qc.ResetKindFull, qc.ResetKindFable, qc.ResetKindWeekly, qc.ResetKindSession} {
		if got, blocked := chooseGrant(status, kind, newClock().Now()); got.id != "" || blocked != OutcomeNoCredit {
			t.Fatalf("%s: chose %q (blocked %q)", kind, got.id, blocked)
		}
	}
	// Even asked for by its own name, which no caller can send.
	if got, _ := chooseGrant(status, qc.ResetKindUnknown, newClock().Now()); got.id != "" {
		t.Fatalf("an unknown grant was chosen for an unknown press: %q", got.id)
	}

	host := claudeHost(statusWith(`{"eligible":true,"at_limit":true,"next_grant_id":"grant-new","grants":[`+monthly+`]}`), `{"result":"reset"}`)
	result, err := redeemerOn(host, newClock()).Redeem(context.Background(), "claude", "c", qc.ResetKindFull)
	if err != nil || result.Outcome != OutcomeNoCredit || result.RemainingCount != 0 || host.posted() {
		t.Fatalf("result = %+v, err = %v, posted = %v", result, err, host.posted())
	}
}

// The fields that say a grant's kind are read as strictly as every other: one
// present in the wrong shape rejects the whole block, because a block whose
// kinds cannot be read is no basis for choosing which kind to spend. Absent
// and null are a grant that predates kinds, which is a full reset.
func TestTheKindFieldsAreReadStrictly(t *testing.T) {
	for name, fields := range map[string]string{
		"gives_back_weekly a string": `,"gives_back_weekly":"yes"`,
		"gives_back_weekly a number": `,"gives_back_weekly":1`,
		"gives_back_weekly a list":   `,"gives_back_weekly":[true]`,
		"type a number":              `,"type":3`,
		"type a boolean":             `,"type":true`,
		"type an object":             `,"type":{"name":"full"}`,
	} {
		block := `{"eligible":true,"at_limit":true,"grants":[{"id":"grant-a","resets_total":1,"resets_left":1,"usable_now":true` + fields + `}]}`
		host := claudeHost(statusWith(block), `{"result":"reset"}`)
		if _, err := redeemerOn(host, newClock()).Redeem(context.Background(), "claude", "c", qc.ResetKindFull); !errors.Is(err, ErrRefused) {
			t.Fatalf("%s: err = %v, want ErrRefused", name, err)
		}
		if host.posted() {
			t.Fatalf("%s: a claim was sent on a grant whose kind could not be read", name)
		}
	}
	for name, fields := range map[string]string{
		"both absent":             ``,
		"both null":               `,"gives_back_weekly":null,"type":null`,
		"gives_back_weekly false": `,"gives_back_weekly":false`,
	} {
		status, ok := parseClaudeStatus(mustDecode(t, `{"eligible":true,"grants":[{"id":"grant-a","resets_left":1`+fields+`}]}`))
		if !ok || status.grants[0].kind != qc.ResetKindFull {
			t.Fatalf("%s: status = %+v, ok = %v; want a full reset", name, status, ok)
		}
	}
}

// What a press reports left is what is left of the kind it was for: the
// account's other kinds are not counted into it, before a spend or after.
func TestRemainingCountIsPerKind(t *testing.T) {
	for name, one := range map[string]struct {
		kind, claim string
		outcome     string
		left        int
	}{
		"Fable spent, count unsent": {qc.ResetKindFable, `{"result":"reset"}`, OutcomeReset, 1},
		"Fable spent, count sent":   {qc.ResetKindFable, `{"result":"reset","resets_left":1}`, OutcomeReset, 1},
		"full spent":                {qc.ResetKindFull, `{"result":"reset","resets_left":0}`, OutcomeReset, 0},
		"Fable refused":             {qc.ResetKindFable, `{"result":"cooldown"}`, OutcomeCooldown, 2},
		"full refused":              {qc.ResetKindFull, `{"result":"not_limited"}`, OutcomeNotLimited, 1},
	} {
		host := claudeHost(mixedStatus(), one.claim)
		result, err := redeemerOn(host, newClock()).Redeem(context.Background(), "claude", "c", one.kind)
		if err != nil || result.Outcome != one.outcome || result.RemainingCount != one.left || result.Kind != one.kind {
			t.Fatalf("%s: result = %+v, err = %v; want %s with %d %s left", name, result, err, one.outcome, one.left, one.kind)
		}
	}
}

// A press names exactly the kinds its provider has: one of four on Claude,
// none on Codex. Anything else is refused before a single request is made.
func TestAPressMustNameAKindItsProviderHas(t *testing.T) {
	for name, one := range map[string]struct{ provider, kind string }{
		"Codex naming a kind":         {"codex", qc.ResetKindFull},
		"Codex naming Fable":          {"codex", qc.ResetKindFable},
		"Claude naming none":          {"claude", ""},
		"Claude naming unknown":       {"claude", qc.ResetKindUnknown},
		"Claude naming a new kind":    {"claude", "monthly"},
		"Claude in another case":      {"claude", "Full"},
		"Claude with a space":         {"claude", "fable "},
		"Claude naming the API value": {"claude", "five_hour"},
	} {
		host := claudeHost(mixedStatus(), `{"result":"reset"}`)
		host.bodies[codexCreditsURL] = oneAvailable
		result, err := redeemerOn(host, newClock()).Redeem(context.Background(), one.provider, "c", one.kind)
		if !errors.Is(err, ErrInvalidKind) || result.MayHaveSpent {
			t.Fatalf("%s: result = %+v, err = %v; want ErrInvalidKind", name, result, err)
		}
		if len(host.requests) != 0 {
			t.Fatalf("%s: %d requests were made for a press naming the wrong kind", name, len(host.requests))
		}
	}
	// And each of the four is accepted on Claude, as none is on Codex.
	for _, kind := range []string{qc.ResetKindFull, qc.ResetKindFable, qc.ResetKindWeekly, qc.ResetKindSession} {
		if _, err := redeemerOn(claudeHost(mixedStatus(), `{"result":"reset"}`), newClock()).Redeem(context.Background(), "claude", "c", kind); errors.Is(err, ErrInvalidKind) {
			t.Fatalf("%s was refused as a kind", kind)
		}
	}
	codex := hostWith(map[string]string{codexCreditsURL: oneAvailable, codexConsumeURL: `{"code":"reset"}`})
	if result, err := redeemerOn(codex, newClock()).Redeem(context.Background(), "codex", "c", ""); err != nil || result.Kind != "" {
		t.Fatalf("codex = %+v, %v", result, err)
	}
}

// An unknown outcome on one kind leaves its claim on record, and a press for
// another kind can neither repeat it nor claim beside it: nothing is sent, the
// answer names the kind and the claim's own deadline, and the claim is still
// there for a press of its kind to settle.
func TestAnUnresolvedClaimOfAnotherKindRefusesThePress(t *testing.T) {
	clock := newClock()
	madeAt := clock.Now()
	host := claudeHost(mixedStatus(), `{"result":"reset","resets_left":1}`)
	host.queue(claudeClaimURL, reply{status: 504})
	redeemer := redeemerOn(host, clock)

	if _, err := redeemer.Redeem(context.Background(), "claude", "c", qc.ResetKindFable); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("first press err = %v, want ErrOutcomeUnknown", err)
	}
	requests := len(host.requests)
	clock.advance(4 * time.Minute)
	result, err := redeemer.Redeem(context.Background(), "claude", "c", qc.ResetKindFull)
	var other *OtherKindUnresolvedError
	if !errors.As(err, &other) || !errors.Is(err, ErrOtherKindUnresolved) || errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("full press err = %v, want an OtherKindUnresolvedError", err)
	}
	if other.Kind != qc.ResetKindFable || !other.RetryUntil.Equal(madeAt.Add(retryWindow)) {
		t.Fatalf("error = %+v, want the Fable claim's own deadline %s", other, madeAt.Add(retryWindow))
	}
	if result.MayHaveSpent || len(host.requests) != requests {
		t.Fatalf("the refused press sent %d requests (may have spent: %v)", len(host.requests)-requests, result.MayHaveSpent)
	}

	// The Fable claim is still on record: a Fable press repeats it exactly.
	result, err = redeemer.Redeem(context.Background(), "claude", "c", qc.ResetKindFable)
	if err != nil || result.Outcome != OutcomeReset || result.Kind != qc.ResetKindFable || result.RemainingCount != 1 {
		t.Fatalf("Fable repeat = %+v, %v", result, err)
	}
	bodies := host.postBodies(t)
	if len(bodies) != 2 || bodies[1]["request_id"] != bodies[0]["request_id"] || bodies[1]["grant_id"] != "grant-fable" ||
		host.calls(claudeStatusURL) != 1 {
		t.Fatalf("claims = %v, status reads = %d; want the Fable claim repeated", bodies, host.calls(claudeStatusURL))
	}

	// Settled, so the full press is now a fresh claim of its own kind.
	if result, err := redeemer.Redeem(context.Background(), "claude", "c", qc.ResetKindFull); err != nil || result.Kind != qc.ResetKindFull {
		t.Fatalf("full press after the settle = %+v, %v", result, err)
	}
	if bodies := host.postBodies(t); len(bodies) != 3 || bodies[2]["grant_id"] != "grant-full" || bodies[2]["request_id"] == bodies[0]["request_id"] {
		t.Fatalf("claims = %v, want a fresh claim on the full grant", bodies)
	}
}

// Once the other kind's window has closed, its claim is no longer on record
// and the press goes ahead as a fresh one.
func TestAnotherKindsClaimStopsRefusingOnceItsWindowCloses(t *testing.T) {
	clock := newClock()
	host := claudeHost(mixedStatus(), `{"result":"reset"}`)
	host.queue(claudeClaimURL, reply{status: 502})
	redeemer := redeemerOn(host, clock)
	_, _ = redeemer.Redeem(context.Background(), "claude", "c", qc.ResetKindFable)

	clock.advance(retryWindow - time.Second)
	if _, err := redeemer.Redeem(context.Background(), "claude", "c", qc.ResetKindFull); !errors.Is(err, ErrOtherKindUnresolved) {
		t.Fatalf("inside the window err = %v", err)
	}
	clock.advance(time.Second)
	if result, err := redeemer.Redeem(context.Background(), "claude", "c", qc.ResetKindFull); err != nil || result.Outcome != OutcomeReset {
		t.Fatalf("after the window = %+v, %v", result, err)
	}
	if bodies := host.postBodies(t); len(bodies) != 2 || bodies[1]["grant_id"] != "grant-full" {
		t.Fatalf("claims = %v", bodies)
	}
}
