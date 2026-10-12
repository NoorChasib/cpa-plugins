package route

import (
	"encoding/json"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// Every decline of Pick, at the edges of each age, and in order where
// several hold.
func TestPickDeclinesInOrder(t *testing.T) {
	f := worked(t)
	now := f.Now.Add(42 * time.Second)
	oauth := cpaCandidate{ID: "claude-oauth-placeholder.json", Provider: "claude", Status: "active",
		Attributes: map[string]string{"auth_kind": "oauth"}}
	oauthRequest := newRequest("", configKey("0b"), oauth).bytes(t)

	// Disabled wins over everything, a missing table included.
	r := New()
	expect(t, r, request(t, "", "0b"), now, decline(ReasonDisabled))
	r.Publish(Build(f.input()), f.Now)
	expect(t, r, request(t, "", "0b"), now, decline(ReasonDisabled))
	r = New()
	r.SetEnabled(true)
	expect(t, r, oauthRequest, now, decline(ReasonNoTable))
	r.Publish(nil, now)
	expect(t, r, request(t, "", "0b"), now, decline(ReasonNoTable))

	// The table's own reasons, ahead of the request's.
	for _, c := range []struct {
		change func(*Input)
		want   Reason
	}{
		{func(in *Input) { in.Credits, in.CreditsError = nil, "missing" }, ReasonCreditsMissing},
		{func(in *Input) { in.Credits, in.CreditsError = nil, "unreadable" }, ReasonCreditsUnreadable},
		{func(in *Input) { in.Credits.ComputedAt = in.Now.Add(-time.Hour) }, ReasonCreditsStale},
		{func(in *Input) { in.Credits.Settings.State = qc.CreditSettingsUnreadable }, ReasonSettingsUnavailable},
		{func(in *Input) { in.Meter, in.MeterError = nil, "missing" }, ReasonMeterMissing},
		{func(in *Input) { in.Meter, in.MeterError = nil, "unreadable" }, ReasonMeterUnreadable},
		{func(in *Input) { in.Meter.AuthIDsSince = nil }, ReasonMeterTooOld},
		{func(in *Input) { in.Meter.StoppedAt = ptr(in.Now) }, ReasonMeterStopped},
		{func(in *Input) { in.Meter.FlushedAt = in.Now.Add(-time.Hour) }, ReasonMeterStale},
		{func(in *Input) { in.Credits.APICredits = nil }, ReasonNoAPICredits},
		{func(in *Input) { in.Credits = credits(in.Now, account("item-1", "", 0, nil)) }, ReasonNoLinkedOrgs},
	} {
		f := worked(t)
		in := f.input()
		c.change(&in)
		r := routing(t, in)
		expect(t, r, oauthRequest, now, decline(c.want))
		expect(t, r, []byte("not json"), now, decline(c.want))
	}

	// The table's age: exactly MaxTableAge still routes.
	r = routing(t, f.input())
	expect(t, r, request(t, "", "0b"), f.Now.Add(MaxTableAge), routed("0b"))
	expect(t, r, request(t, "", "0b"), f.Now.Add(MaxTableAge+time.Second), decline(ReasonTableStale))
	expect(t, r, oauthRequest, f.Now.Add(MaxTableAge+time.Second), decline(ReasonTableStale))

	// The meter's age at the pick: flushed 13 minutes before the build, so
	// two minutes later it is exactly MaxMeterAge old.
	f = worked(t)
	f.Meter.FlushedAt = f.Now.Add(-13 * time.Minute)
	r = routing(t, f.input())
	expect(t, r, request(t, "", "0b"), f.Now.Add(2*time.Minute), routed("0b"))
	if s := r.Status(f.Now.Add(2 * time.Minute)); !s.Active || s.TableReason != "" {
		t.Fatalf("meter exactly MaxMeterAge old at the pick: active %v table_reason %q", s.Active, s.TableReason)
	}
	expect(t, r, request(t, "", "0b"), f.Now.Add(2*time.Minute+time.Second), decline(ReasonMeterStale))
	expect(t, r, oauthRequest, f.Now.Add(2*time.Minute+time.Second), decline(ReasonMeterStale))
	if s := r.Status(f.Now.Add(2*time.Minute + time.Second)); s.Active || s.TableReason != "meter_stale" || s.Reason != "meter_stale" {
		t.Fatalf("meter stale at the pick: active %v reason %q table_reason %q", s.Active, s.Reason, s.TableReason)
	}

	// The state's valid_until: a refill boundary 90 seconds after the build.
	// One second before it still routes; from it on, until the next build
	// reads Quota Cache's new state, every attempt goes back to CPA, ahead of
	// the request's own declines and behind the table's and the meter's ages.
	f = worked(t)
	in := f.input()
	boundary := f.Now.Add(90 * time.Second)
	in.Credits.ValidUntil = &boundary
	r = routing(t, in)
	expect(t, r, request(t, "", "0b"), boundary.Add(-time.Second), routed("0b"))
	if s := r.Status(boundary.Add(-time.Second)); !s.Active || s.TableReason != "" {
		t.Fatalf("a second before valid_until: active %v table_reason %q", s.Active, s.TableReason)
	}
	expect(t, r, request(t, "", "0b"), boundary, decline(ReasonCreditsStale))
	expect(t, r, oauthRequest, boundary, decline(ReasonCreditsStale))
	expect(t, r, []byte("not json"), boundary.Add(time.Minute), decline(ReasonCreditsStale))
	if s := r.Status(boundary); s.Active || s.TableReason != "credits_stale" || s.Reason != "credits_stale" {
		t.Fatalf("at valid_until: active %v reason %q table_reason %q", s.Active, s.Reason, s.TableReason)
	}
	expect(t, r, request(t, "", "0b"), f.Now.Add(MaxTableAge+time.Second), decline(ReasonTableStale))
	f = worked(t)
	f.Meter.FlushedAt = f.Now.Add(-13 * time.Minute)
	in = f.input()
	in.Credits.ValidUntil = &boundary
	r = routing(t, in)
	expect(t, r, request(t, "", "0b"), f.Now.Add(2*time.Minute+time.Second), decline(ReasonMeterStale))

	// The request's own declines.
	f = worked(t)
	r = routing(t, f.input())
	expect(t, r, oauthRequest, now, decline(ReasonOutOfScope))
	// The OAuth fast path runs before the decode: invalid JSON holding the
	// attribute is out of scope, not invalid.
	expect(t, r, []byte(`{"Candidates":[{"Attributes":{"auth_kind":"oauth"}}`), now, decline(ReasonOutOfScope))
	for _, raw := range []string{"", "not json", `{"Candidates":[]}`, `{}`, `null`, `{"Candidates":"x"}`, `[1,2]`} {
		expect(t, r, []byte(raw), now, decline(ReasonInvalidRequest))
	}

	// unmapped: no offered key is mapped and every exploration slot is taken.
	expect(t, r, request(t, "", "12"), now, explored("12"))
	expect(t, r, request(t, "", "12"), now, decline(ReasonUnmapped))
	unknown := configKey("99")
	unknown.Attributes["source"] = "config:claude[000000000099]"
	expect(t, r, newRequest("", unknown).bytes(t), now, Decision{AuthID: id("99"), Reason: ReasonExplored})
	expect(t, r, newRequest("", unknown, configKey("12")).bytes(t), now, decline(ReasonUnmapped))

	// no_proven: a key of an eligible organization that is not proven, its
	// slot taken.
	f = worked(t)
	b := f.Meter.AuthIDs[id("0b")]
	b.Failures = 2
	f.Meter.AuthIDs[id("0b")] = b
	r = routing(t, f.input())
	expect(t, r, request(t, "", "0a", "0b", "13"), now, explored("0b"))
	expect(t, r, request(t, "", "0a", "0b", "13"), now, decline(ReasonNoProven))
	expect(t, r, request(t, "", "0b", "12"), now, explored("12"))
	expect(t, r, request(t, "", "0b", "12"), now, decline(ReasonNoProven))

	// no_eligible: mapped keys, none of them eligible.
	expect(t, r, request(t, "", "0a", "0c", "13"), now, decline(ReasonNoEligible))
	// That includes a request offered only foreign or unlinked keys, such as
	// another api-keys.claude group's, while the credits organizations still
	// have credit.
	f = worked(t)
	seen := f.Now
	keyed(&f.Meter, "21", fakeOrg("99"), seen, &seen, 0) // an organization no item names
	r = routing(t, f.input())
	for _, offered := range [][]string{{"13"}, {"21"}, {"13", "21"}} {
		expect(t, r, request(t, "", offered...), now, decline(ReasonNoEligible))
	}
}

// Scope is every candidate a Claude API key from CPA's configuration, decided
// per attempt and never by model.
func TestPickScope(t *testing.T) {
	f := worked(t)
	now := f.Now.Add(42 * time.Second)
	change := func(fn func(*cpaCandidate)) cpaCandidate {
		c := configKey("0d")
		fn(&c)
		return c
	}
	out := map[string]cpaCandidate{
		"provider codex":            change(func(c *cpaCandidate) { c.Provider = "codex" }),
		"auth_kind missing":         change(func(c *cpaCandidate) { delete(c.Attributes, "auth_kind") }),
		"auth_kind empty":           change(func(c *cpaCandidate) { c.Attributes["auth_kind"] = "" }),
		"auth_kind APIKEY":          change(func(c *cpaCandidate) { c.Attributes["auth_kind"] = "APIKEY" }),
		"no attributes":             change(func(c *cpaCandidate) { c.Attributes = nil }),
		"source not config:claude[": change(func(c *cpaCandidate) { c.Attributes["source"] = "config:codex[00000000000d]" }),
		"source missing":            change(func(c *cpaCandidate) { delete(c.Attributes, "source") }),
		"source of another ID":      change(func(c *cpaCandidate) { c.Attributes["source"] = "config:claude[00000000000c]" }),
		"source with a suffix":      change(func(c *cpaCandidate) { c.Attributes["source"] = "config:claude[00000000000d]x" }),
		"source unterminated":       change(func(c *cpaCandidate) { c.Attributes["source"] = "config:claude[00000000000d" }),
		"ID of the wrong shape": change(func(c *cpaCandidate) {
			c.ID, c.Attributes["source"] = "claude:apikey:0000000000XY", "config:claude[0000000000XY]"
		}),
		"ID of an OAuth file": change(func(c *cpaCandidate) {
			c.ID, c.Attributes["source"] = "claude-oauth-placeholder.json", "config:claude[claude-oauth-placeholder.json]"
		}),
	}
	for name, candidate := range out {
		r := routing(t, f.input())
		expect(t, r, newRequest("", configKey("0b"), candidate).bytes(t), now, decline(ReasonOutOfScope))
		expect(t, r, newRequest("", candidate).bytes(t), now, decline(ReasonOutOfScope))
		_ = name
	}

	// oauth spelled with a space after the colon misses the fast path, and
	// the decode still declines it.
	r := routing(t, f.input())
	oauth := change(func(c *cpaCandidate) { c.Attributes["auth_kind"] = "oauth" })
	indented, err := json.MarshalIndent(newRequest("", configKey("0b"), oauth), "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(indented), `"auth_kind":"oauth"`) {
		t.Fatal("the indented request still holds the fast-path text")
	}
	expect(t, r, indented, now, decline(ReasonOutOfScope))
	// The fast path: one OAuth subscription among API keys.
	sub := cpaCandidate{ID: "claude-oauth-placeholder.json", Provider: "claude", Priority: 100, Status: "active",
		Attributes: map[string]string{"auth_kind": "oauth", "source": "file"}}
	expect(t, r, newRequest("", configKey("0b"), sub, configKey("0d")).bytes(t), now, decline(ReasonOutOfScope))

	// In scope: an unprefixed model whose offered tier holds only config
	// keys. The router never reads Model.
	unprefixed := newRequest("", configKey("0b"), configKey("0d"))
	unprefixed.Model = "claude-haiku-5-5"
	unprefixed.Options.Metadata["requested_model"] = "claude-haiku-5-5"
	expect(t, r, unprefixed.bytes(t), now, routed("0b"))
	// A duplicate key's ID and source carry CPA's -N suffix together.
	dup := configKey("0b")
	dup.ID, dup.Attributes["source"] = id("0b")+"-1", "config:claude[00000000000b-1]"
	expect(t, r, newRequest("", dup).bytes(t), now, Decision{AuthID: id("0b") + "-1", Reason: ReasonExplored})
}

// The worked example, pick by pick as each attempt of one request fails over
// to the next, then its variants.
func TestPickRoutesWorkedExample(t *testing.T) {
	f := worked(t)
	r := routing(t, f.input())
	now := at(t, "2026-10-11T12:00:42Z")

	steps := []struct {
		offered []string
		sel     string
		want    Decision
	}{
		{all, "", explored("12")},
		{without("12"), "12", routed("0b")},
		{without("12", "0b"), "0b", routed("0d")},
		{without("12", "0b", "0d"), "0d", routed("0f")},
		{without("12", "0b", "0d", "0f"), "0f", routed("11")}, // echo ranks last for this pick
		{without("12", "0b", "0d", "0f", "11"), "11", routed("10")},
		{[]string{"0a", "0c", "13"}, "10", decline(ReasonNoEligible)},
	}
	for i, step := range steps {
		sel := ""
		if step.sel != "" {
			sel = id(step.sel)
		}
		if got := r.Pick(request(t, sel, step.offered...), now); got != step.want {
			t.Fatalf("pick %d = {%q %s}; want {%q %s}", i+1, got.AuthID, got.Reason, step.want.AuthID, step.want.Reason)
		}
	}
	if e, ok := entryOf(r, "12"); !ok || !e.failedInBand || e.evaluated {
		t.Fatalf("12's claim is %+v; want it failed in band and not yet evaluated", e)
	}
	if s := r.Status(now).Counters; s.Demoted != 1 || s.Routed != 5 || s.Explored != 1 || s.Declined["no_eligible"] != 1 {
		t.Fatalf("counters = %+v", s)
	}

	// A second request a minute later: 12's slot is not ready.
	expect(t, r, request(t, "", all...), now.Add(time.Minute), routed("0b"))
	// Its claim is evaluated once QuickRearm old, and having failed in band
	// its next wait is 20 minutes.
	r.Publish(Build(f.inputAt(at(t, "2026-10-11T12:04:00Z"))), at(t, "2026-10-11T12:04:00Z"))
	if e, _ := entryOf(r, "12"); e.step != 1 || !e.evaluated || e.nextAt() != at(t, "2026-10-11T12:20:42Z").Unix() {
		t.Fatalf("12's claim after evaluation is %+v", e)
	}

	// The last pick hands the request back only because alpha's estimate is
	// spent too. With credit left in it, that pick re-checks alpha instead.
	t.Run("alpha's estimate with credit left", func(t *testing.T) {
		f := worked(t)
		f.Accounts[0].EstimateLeftText = "$6.20"
		r := routing(t, f.input())
		expect(t, r, request(t, id("10"), "0a", "0c", "13"), now, rechecked("0a"))
		expect(t, r, request(t, id("10"), "0a", "0c", "13"), now, decline(ReasonNoEligible))
	})

	t.Run("bootstrap", func(t *testing.T) {
		f := worked(t)
		f.Meter.AuthIDs = map[string]qc.MeterAuthID{}
		r := routing(t, f.input())
		offered, sel := append([]string(nil), all...), ""
		for _, suffix := range all {
			expect(t, r, request(t, sel, offered...), now, explored(suffix))
			offered, sel = offered[1:], id(suffix)
		}
		expect(t, r, request(t, "", all...), now, decline(ReasonUnmapped))
	})

	t.Run("unproven key", func(t *testing.T) {
		f := worked(t)
		b := f.Meter.AuthIDs[id("0b")]
		b.Failures = 2
		f.Meter.AuthIDs[id("0b")] = b
		r := routing(t, f.input())
		expect(t, r, request(t, "", all...), now, explored("12"))
		expect(t, r, request(t, id("12"), without("12")...), now, routed("0d"))
		expect(t, r, request(t, "", all...), now.Add(time.Minute), explored("0b"))
		expect(t, r, request(t, id("0b"), without("0b")...), now.Add(time.Minute), routed("0d"))
	})
}

// After a failed attempt, the next pick of the round ranks that attempt's
// organization last, for that pick only.
func TestPickRanksTheFailedOrgLast(t *testing.T) {
	f := worked(t)
	r := routing(t, f.input())
	now := at(t, "2026-10-11T12:00:42Z")

	// The dry pool: echo's credit is gone, the meter has not marked it out.
	// Request A.
	expect(t, r, request(t, "", "0f", "10", "11"), now, routed("0f"))
	expect(t, r, request(t, id("0f"), "10", "11"), now, routed("11"))
	// Request B within the minute, 0f cooled.
	expect(t, r, request(t, "", "10", "11"), now.Add(20*time.Second), routed("10"))
	expect(t, r, request(t, id("10"), "11"), now.Add(20*time.Second), routed("11"))
	if got := r.Status(now).Counters.Demoted; got != 1 {
		t.Fatalf("demoted = %d; want 1: only request A's second pick changed its answer", got)
	}

	// The demotion lasts one pick: the next round's first pick takes echo.
	expect(t, r, request(t, "", "10", "11"), now.Add(30*time.Second), routed("10"))

	// A previous attempt on an ineligible, foreign or unmapped key, or a key
	// the table does not know, demotes nothing.
	for _, sel := range []string{id("0a"), id("0c"), id("13"), id("12"), id("99")} {
		expect(t, r, request(t, sel, "0f", "10", "11"), now, routed("0f"))
	}
	if got := r.Status(now).Counters.Demoted; got != 1 {
		t.Fatalf("demoted = %d; want 1", got)
	}

	// A failing key's organization ranks last too.
	f = worked(t)
	keyed(&f.Meter, "20", fakeOrg("0e"), at(t, "2026-10-11T11:55:00Z"), nil, 0) // echo, never succeeded
	r = routing(t, f.input())
	expect(t, r, request(t, id("20"), "10", "11"), now, routed("11"))
	expect(t, r, request(t, id("20"), "0b", "10"), now, routed("0b"))
	if got := r.Status(now).Counters.Demoted; got != 1 {
		t.Fatalf("demoted = %d; want 1: the second pick's answer was 0b either way", got)
	}
}

// An out organization whose estimate still had credit left gets one request
// per wait, ahead of the ranked choice, on its smallest proven offered key;
// one out at $0.00 and one with no credit left never do.
func TestPickRechecks(t *testing.T) {
	f := worked(t)
	// alpha's estimate had $6.20 left when Anthropic refused it.
	f.Accounts[0].EstimateLeftText = "$6.20"
	// A second out organization with credit in its estimate, listed after
	// alpha (Oct 13) but refilling sooner.
	golf := account("org-000000000007", fakeOrg("07"), 6, epoch(t, "2026-10-12T00:00:00Z"))
	golf.State, golf.Left, golf.LeftText, golf.EstimateLeftText = qc.CreditStateOut, 0, "$0.00", "$12.00"
	f.Accounts = append(f.Accounts, golf)
	seen := at(t, "2026-10-11T11:00:00Z")
	keyed(&f.Meter, "20", fakeOrg("07"), seen, &seen, 0)
	keyed(&f.Meter, "21", fakeOrg("07"), seen, &seen, 0)
	r := routing(t, f.input())
	now := at(t, "2026-10-11T12:00:42Z")

	if got := recheckIDs(r); !slices.Equal(got, []string{"org-000000000007", "org-aaaaaaaaaaaa"}) {
		t.Fatalf("rechecks = %v; want golf, then alpha", got)
	}
	// Soonest refill first, not in the credit state's order: the smallest
	// proven offered ID of each, ahead of bravo.
	offered := []string{"0a", "0b", "0c", "21", "20"}
	expect(t, r, request(t, "", offered...), now, rechecked("20"))
	expect(t, r, request(t, "", offered...), now, rechecked("0a"))
	expect(t, r, request(t, "", offered...), now, routed("0b"))
	// A failover pick with a ranked candidate never re-checks.
	r = routing(t, f.input())
	expect(t, r, request(t, id("0d"), offered...), now, routed("0b"))
	// Without a ranked candidate it does.
	expect(t, r, request(t, id("0d"), "0a", "0c"), now, rechecked("0a"))
	// charlie (no credit left) alone is never re-checked.
	expect(t, r, request(t, "", "0c"), now, decline(ReasonNoEligible))

	// Once per wait: ready again after it, once evaluated as taught nothing.
	later := now.Add(4 * time.Minute)
	f.Meter.FlushedAt = later.Add(-30 * time.Second)
	a := f.Meter.AuthIDs[id("0a")]
	a.SeenAt = now.Add(time.Second) // refused again
	f.Meter.AuthIDs[id("0a")] = a
	r.Publish(Build(f.inputAt(later)), later)
	if e, _ := entryOf(r, "0a"); e.step != 1 || e.kind != kindRecheck || e.nextAt() != now.Add(20*time.Minute).Unix() {
		t.Fatalf("0a's re-check after a refusal is %+v", e)
	}
	expect(t, r, request(t, "", "0a", "0b"), later, routed("0b"))

	// A re-check entry goes when its organization stops being out.
	f.Accounts[0].State, f.Accounts[0].Left, f.Accounts[0].LeftText, f.Accounts[0].EstimateLeftText = qc.CreditStateOK, 6, "$6.00", ""
	r.Publish(Build(f.inputAt(later.Add(time.Minute))), later.Add(time.Minute))
	if _, ok := entryOf(r, "0a"); ok {
		t.Fatal("0a's re-check entry outlived alpha's out")
	}

	// A demoted claudeCodeRefused organization is re-checked: bravo here.
	f = worked(t)
	f.Accounts[1].DataIssues = []string{"claudeCodeRefused"}
	r = routing(t, f.input())
	expect(t, r, request(t, "", "0b", "0d"), now, rechecked("0b"))
	expect(t, r, request(t, "", "0b", "0d"), now, routed("0d"))
	// Without any clean candidate, the demoted one is still routed to.
	expect(t, r, request(t, "", "0b", "0a"), now, routed("0b"))
	// Refused again, the re-check keeps its entry while bravo stays demoted,
	// and backs off: the next rebuild must not reset its wait, or 0b would
	// take a request ahead of the ranked choice after every rebuild.
	f.Meter.FlushedAt = later.Add(-30 * time.Second)
	b := f.Meter.AuthIDs[id("0b")]
	b.SeenAt = now.Add(time.Second)
	f.Meter.AuthIDs[id("0b")] = b
	r.Publish(Build(f.inputAt(later)), later)
	if e, ok := entryOf(r, "0b"); !ok || e.step != 1 || e.kind != kindRecheck || !e.evaluated {
		t.Fatalf("0b's re-check after a rebuild is %+v, kept %v; want it at step 1", e, ok)
	}
	expect(t, r, request(t, "", "0b", "0d"), later, routed("0d"))
	// The entry goes once bravo is no longer demoted.
	f.Accounts[1].DataIssues = []string{}
	r.Publish(Build(f.inputAt(later.Add(time.Minute))), later.Add(time.Minute))
	if _, ok := entryOf(r, "0b"); ok {
		t.Fatal("0b's re-check entry outlived bravo's demotion")
	}

	// Re-checks go soonest refill first whether the organization is out or
	// demoted: alpha (Oct 13) ahead of bravo (Oct 21), though bravo, being
	// eligible, is listed ahead of alpha.
	f = worked(t)
	f.Accounts[0].EstimateLeftText = "$6.20"
	f.Accounts[1].DataIssues = []string{"claudeCodeRefused"}
	r = routing(t, f.input())
	if got := recheckIDs(r); !slices.Equal(got, []string{"org-aaaaaaaaaaaa", "org-bbbbbbbbbbbb"}) {
		t.Fatalf("rechecks = %v; want alpha, then bravo", got)
	}
	expect(t, r, request(t, "", "0a", "0b", "0d"), now, rechecked("0a"))
	expect(t, r, request(t, "", "0a", "0b", "0d"), now, rechecked("0b"))
	expect(t, r, request(t, "", "0a", "0b", "0d"), now, routed("0d"))

	// echo, the Team pool, marked out early with $80 left in its estimate.
	// Its smaller key, 0f, is a personal key sent without a workspace ID: a
	// 400 on every request, which CPA neither fails over nor cools, so it
	// stays on offer. The re-check goes to echo's proven key instead, and a
	// request that finds that key's slot waiting tries no sibling: one
	// attempt per wait.
	echoOut := func() fixture {
		f := worked(t)
		echo := &f.Accounts[4]
		echo.State, echo.Left, echo.LeftText, echo.EstimateLeftText = qc.CreditStateOut, 0, "$0.00", "$80.00"
		return f
	}
	f = echoOut()
	personal := f.Meter.AuthIDs[id("0f")]
	personal.LastSuccessAt, personal.LastFailureAt, personal.Failures = nil, &seen, 9
	f.Meter.AuthIDs[id("0f")] = personal
	r = routing(t, f.input())
	offered = []string{"0b", "0f", "10"}
	expect(t, r, request(t, "", offered...), now, rechecked("10"))
	expect(t, r, request(t, "", offered...), now, routed("0b"))

	// While echo offers no proven key, its keys take turns, each with its
	// own wait, so the one that fails every request cannot keep echo out.
	other := f.Meter.AuthIDs[id("10")]
	other.Failures = ProvenFailures
	f.Meter.AuthIDs[id("10")] = other
	r = routing(t, f.input())
	expect(t, r, request(t, "", offered...), now, rechecked("0f"))
	expect(t, r, request(t, "", offered...), now, rechecked("10"))
	expect(t, r, request(t, "", offered...), now, routed("0b"))

	// A spend limit on 0f's workspace after it had succeeded: its re-checks
	// fail, and once it has failed twice since its success the next rebuild
	// moves the re-check to echo's other key.
	f = echoOut()
	r = routing(t, f.input())
	expect(t, r, request(t, "", offered...), now, rechecked("0f"))
	limited := f.Meter.AuthIDs[id("0f")]
	limited.SeenAt, limited.LastFailureAt, limited.Failures = now.Add(time.Second), ptr(now.Add(time.Second)), ProvenFailures
	f.Meter.AuthIDs[id("0f")] = limited
	r.Publish(Build(f.inputAt(later)), later)
	expect(t, r, request(t, "", offered...), later, rechecked("10"))
}

// recheckIDs is the published table's organizations to re-check, in the order
// a pick tries them.
func recheckIDs(r *Router) []string {
	table := r.table.Load()
	ids := []string{}
	for _, i := range table.rechecks {
		ids = append(ids, table.Orgs[i].ID)
	}
	return ids
}

// selected_auth_id is kept only in the config-key shape; anything else is
// never turned into a string, and never fails the decode.
func TestSelectedAuthIDIsDecodedOnlyInShape(t *testing.T) {
	const canary = "SELECTED-CANARY"
	for _, raw := range []string{
		`"` + canary + `.json"`,
		`"` + canary + `@example.invalid"`,
		`"claude:apikey:00000000000\u0062"`,
		`"claude:apikey:00000000000b\""`,
		`"claude:apikey:00000000000B"`,
		`"claude:apikey:00000000000b-0"`,
		`"claude:apikey:00000000000b "`,
		`" claude:apikey:00000000000b"`,
		`12345678901234567890123456789`,
		`null`,
		`true`,
		`{"id":"` + canary + `"}`,
		`["claude:apikey:00000000000b"]`,
	} {
		var w pickWire
		body := `{"Options":{"Metadata":{"selected_auth_id":` + raw + `}},"Candidates":[{"ID":"x"}]}`
		if err := json.Unmarshal([]byte(body), &w); err != nil {
			t.Fatalf("%s failed the decode: %v", raw, err)
		}
		if w.Options.Metadata.SelectedAuthID != "" || len(w.Candidates) != 1 {
			t.Fatalf("%s decoded to %q", raw, w.Options.Metadata.SelectedAuthID)
		}
	}
	for _, kept := range []string{id("0b"), id("0b") + "-1", id("0b") + "-9999"} {
		var w pickWire
		if err := json.Unmarshal([]byte(`{"Options":{"Metadata":{"selected_auth_id":"`+kept+`"}}}`), &w); err != nil {
			t.Fatal(err)
		}
		if string(w.Options.Metadata.SelectedAuthID) != kept {
			t.Fatalf("%s decoded to %q", kept, w.Options.Metadata.SelectedAuthID)
		}
	}

	// Through Pick: no canary reaches the decision or the status.
	f := worked(t)
	r := routing(t, f.input())
	now := f.Now.Add(42 * time.Second)
	for _, sel := range []string{canary + ".json", canary + "@example.invalid"} {
		expect(t, r, request(t, sel, "0f", "10", "11"), now, routed("0f"))
	}
	checkNoCanary(t, r, canary)
}

// 10,000 random tables and requests: an answer that names a key names one of
// the request's candidates, and only the three answering reasons name one.
func TestPickAlwaysAnswersWithACandidate(t *testing.T) {
	rng := rand.New(rand.NewPCG(20261011, 1))
	now := at(t, "2026-10-11T12:00:00Z")
	states := []string{qc.CreditStateOK, qc.CreditStateOK, qc.CreditStateOK, qc.CreditStateOut, qc.CreditStateStale, qc.CreditStatePending, "bogus"}
	suffix := func(n int) string { return strconv.FormatInt(int64(0x20+n), 16) }
	for round := range 10000 {
		accounts := []qc.CreditAccount{}
		for n := range rng.IntN(5) {
			var renews *int64
			if rng.IntN(4) > 0 {
				renews = ptr(now.Add(time.Duration(rng.IntN(30*24)) * time.Hour).Unix())
			}
			a := account("org-0000000000"+suffix(n), fakeOrg(suffix(n)), rng.IntN(3), renews)
			a.State, a.Left, a.HasEstimate = states[rng.IntN(len(states))], float64(rng.IntN(3)), rng.IntN(5) > 0
			a.EstimateLeftText = []string{"", "$0.00", "$3.00"}[rng.IntN(3)]
			if rng.IntN(4) == 0 {
				a.DataIssues = []string{"claudeCodeRefused"}
			}
			accounts = append(accounts, a)
		}
		m := meterFor(now)
		for n := range rng.IntN(8) {
			var success *time.Time
			if rng.IntN(3) > 0 {
				success = ptr(now.Add(-time.Hour))
			}
			org := []string{"-", "", fakeOrg(suffix(rng.IntN(6)))}[rng.IntN(3)]
			keyed(m, suffix(n), org, now.Add(-time.Duration(rng.IntN(600))*time.Second), success, uint16(rng.IntN(3)))
		}
		in := Input{Now: now, Meter: m, Credits: credits(now.Add(-time.Duration(rng.IntN(240))*time.Second), accounts...)}
		switch rng.IntN(20) {
		case 0:
			in.Credits, in.CreditsError = nil, []string{"missing", "unreadable"}[rng.IntN(2)]
		case 1:
			in.Credits.Settings.State = qc.CreditSettingsUnreadable
		case 2:
			// A refill boundary during the picks below.
			in.Credits.ValidUntil = ptr(now.Add(time.Duration(1+rng.IntN(2)) * time.Second))
		case 3:
			in.Meter.AuthIDsSince = nil
		}
		r := routing(t, in)

		candidates := []cpaCandidate{}
		for n := range rng.IntN(9) {
			c := configKey(suffix(n))
			c.Attributes["source"] = "config:claude[0000000000" + suffix(n) + "]"
			switch rng.IntN(25) {
			case 0:
				c.Provider = "codex"
			case 1:
				c.Attributes["auth_kind"] = "oauth"
			case 2:
				c.ID = "claude-oauth-placeholder.json"
			}
			candidates = append(candidates, c)
		}
		sel := ""
		if rng.IntN(2) == 0 {
			sel = id(suffix(rng.IntN(9)))
		}
		raw := newRequest(sel, candidates...).bytes(t)
		for pick := range 3 {
			d := r.Pick(raw, now.Add(time.Duration(pick)*time.Second))
			answers := d.Reason == ReasonRouted || d.Reason == ReasonExplored || d.Reason == ReasonRechecked
			if answers != (d.AuthID != "") || d.Reason == ReasonNone || d.Reason == ReasonPanic {
				t.Fatalf("round %d: {%q %s}", round, d.AuthID, d.Reason)
			}
			if d.AuthID == "" {
				continue
			}
			found := false
			for _, c := range candidates {
				found = found || c.ID == d.AuthID
			}
			if !found {
				t.Fatalf("round %d: answered %q, which is not a candidate", round, d.AuthID)
			}
		}
	}
}

// Steps 1 to 6 allocate nothing, and neither does the decision over a
// decoded request once exploration knows its candidates.
func TestPickAllocations(t *testing.T) {
	f := worked(t)
	now := f.Now.Add(42 * time.Second)
	oauthRequest := newRequest("", configKey("0b"), cpaCandidate{ID: "claude-oauth-placeholder.json", Provider: "claude",
		Attributes: map[string]string{"auth_kind": "oauth"}}).bytes(t)
	inScope := request(t, "", all...)

	disabled := New()
	disabled.Publish(Build(f.input()), f.Now)
	noTable := New()
	noTable.SetEnabled(true)
	inactiveIn := f.input()
	inactiveIn.Meter.AuthIDsSince = nil
	inactiveTable := routing(t, inactiveIn)
	fresh := worked(t)
	active := routing(t, fresh.input())
	stale := worked(t)
	// Flushed 15 minutes and a second before the pick, the table still fresh.
	stale.Meter.FlushedAt = stale.Now.Add(-MaxMeterAge + MaxTableAge - time.Second)
	meterStale := routing(t, stale.input())
	bounded := worked(t)
	boundedIn := bounded.input()
	boundedIn.Credits.ValidUntil = ptr(now)
	creditsStale := routing(t, boundedIn)
	for name, c := range map[string]struct {
		r    *Router
		raw  []byte
		now  time.Time
		want Reason
	}{
		"disabled":      {disabled, inScope, now, ReasonDisabled},
		"no table":      {noTable, inScope, now, ReasonNoTable},
		"table reason":  {inactiveTable, inScope, now, ReasonMeterTooOld},
		"table stale":   {active, inScope, f.Now.Add(time.Hour), ReasonTableStale},
		"meter stale":   {meterStale, inScope, f.Now.Add(MaxTableAge), ReasonMeterStale},
		"credits stale": {creditsStale, inScope, now, ReasonCreditsStale},
		"out of scope":  {active, oauthRequest, now, ReasonOutOfScope},
	} {
		if got := c.r.Pick(c.raw, c.now); got != decline(c.want) {
			t.Fatalf("%s answered {%q %s}", name, got.AuthID, got.Reason)
		}
		if allocs := testing.AllocsPerRun(200, func() { c.r.Pick(c.raw, c.now) }); allocs != 0 {
			t.Errorf("%s allocates %v times per pick; want 0", name, allocs)
		}
	}

	// The decision over a decoded request, for every kind of answer, with
	// every candidate already known to exploration.
	r := routing(t, f.input())
	r.Pick(inScope, now) // explores 12, which creates its entry
	table := r.table.Load()
	for name, raw := range map[string][]byte{
		"routed":            inScope,
		"failover":          request(t, id("0f"), "10", "11", "12"),
		"in-band failure":   request(t, id("12"), without("12")...),
		"no eligible":       request(t, "", "0a", "0c", "13"),
		"unmapped":          request(t, "", "12"),
		"explore not ready": request(t, id("0b"), "12", "0a"),
	} {
		var w pickWire
		if err := json.Unmarshal(raw, &w); err != nil {
			t.Fatal(err)
		}
		if allocs := testing.AllocsPerRun(200, func() { r.choose(table, &w, now) }); allocs != 0 {
			t.Errorf("deciding %s allocates %v times; want 0", name, allocs)
		}
	}
	// Re-claiming an entry that is ready again allocates nothing either.
	twelve := id("12")
	if allocs := testing.AllocsPerRun(200, func() { r.claim(twelve, kindUnmapped, "7", now.Add(24*time.Hour)) }); allocs != 0 {
		t.Errorf("re-claiming allocates %v times; want 0", allocs)
	}
}

// BenchmarkPick is a six-candidate in-scope request carrying 4 KB of
// headers. It is reported, not gated; the target is 50µs/op.
func BenchmarkPick(b *testing.B) {
	f := worked(b)
	r := routing(b, f.input())
	req := newRequest("", configKey("0b"), configKey("0d"), configKey("0f"), configKey("10"), configKey("11"), configKey("0a"))
	req.Options.Headers["Authorization"] = []string{"Bearer " + strings.Repeat("k", 48)}
	for n := 0; len(req.Options.Headers) < 40; n++ {
		req.Options.Headers["X-Bench-"+strconv.Itoa(n)] = []string{strings.Repeat("v", 96)}
	}
	raw := req.bytes(b)
	if len(raw) < 4096 {
		b.Fatalf("the request is %d bytes; want at least 4 KB", len(raw))
	}
	now := f.Now.Add(42 * time.Second)
	if d := r.Pick(raw, now); d != routed("0b") {
		b.Fatalf("pick = %+v", d)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	for b.Loop() {
		r.Pick(raw, now)
	}
}
