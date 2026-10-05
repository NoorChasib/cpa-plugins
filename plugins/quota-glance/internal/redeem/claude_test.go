package redeem

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A fictional organization. The profile sends it upper-cased, so every test
// that reaches the claim also checks it was normalised before use.
const (
	claudeOrgUpper = "0F8FAD5B-D9CB-469F-A165-70867728950E"
	claudeOrg      = "0f8fad5b-d9cb-469f-a165-70867728950e"
	claudeClaimURL = claudeOrigin + "/api/organizations/" + claudeOrg + "/reset_rate_limits"
	claudeProfile  = `{"account":{"email_address":"northwind@example.com"},` +
		`"organization":{"uuid":"` + claudeOrgUpper + `","organization_type":"claude_team"}}`
	// spendableGrant is one grant the provider recommends, on an account at
	// its limit, lapsing well after newClock's instant.
	spendableGrant = `{"id":"grant-a","label":"Banked reset","resets_total":3,"resets_left":2,` +
		`"ends_at":"2026-10-20T00:00:00Z","clears":["five_hour","seven_day"],` +
		`"usable_now":true,"use_requires_limit":true}`
)

// statusWith is the usage response carrying a cedar_ember block.
func statusWith(block string) string {
	return `{"five_hour":{"utilization":100,"resets_at":"2026-10-05T14:00:00Z"},"cedar_ember":` + block + `}`
}

func spendableStatus() string {
	return statusWith(`{"eligible":true,"at_limit":true,"grants":[` + spendableGrant + `],"next_grant_id":"grant-a"}`)
}

func claudeHost(status, claim string) *fakeHost {
	return hostWith(map[string]string{
		claudeProfileURL: claudeProfile,
		claudeStatusURL:  status,
		claudeClaimURL:   claim,
	})
}

func TestClaudeRedeemClaimsTheRecommendedGrant(t *testing.T) {
	host := claudeHost(spendableStatus(), `{"result":"reset","resets_left":1,"cleared":["five_hour","seven_day","seven_day","not_a_window"]}`)
	result, err := redeemerOn(host, newClock()).Redeem(context.Background(), "claude", "claude-northwind@example.com.json")
	if err != nil {
		t.Fatal(err)
	}
	// Two windows, counted once each; a name this build cannot put to a window
	// is not counted. One reset left on the grant, and no other grants.
	if result.Outcome != OutcomeReset || result.WindowsReset != 2 || result.RemainingCount != 1 {
		t.Fatalf("result = %+v", result)
	}
	if len(host.requests) != 3 || host.requests[0].URL != claudeProfileURL || host.requests[1].URL != claudeStatusURL ||
		host.requests[2].URL != claudeClaimURL || host.requests[2].Method != "POST" {
		t.Fatalf("requests = %+v, want profile, status, then the claim addressed to the lower-cased organization", host.requests)
	}
	body := host.postBody(t)
	if body["program"] != "cedar_ember" || body["grant_id"] != "grant-a" || !claudeRequestIDPattern.MatchString(body["request_id"]) || len(body) != 3 {
		t.Fatalf("claim body = %v", body)
	}
}

// Anthropic buckets these endpoints by User-Agent and authenticates them with
// the OAuth beta header; a request without either is throttled or refused.
func TestClaudeRequestsCarryTheProviderContract(t *testing.T) {
	host := claudeHost(spendableStatus(), `{"result":"reset"}`)
	if _, err := redeemerOn(host, newClock()).Redeem(context.Background(), "claude", "c"); err != nil {
		t.Fatal(err)
	}
	for _, request := range host.requests {
		if got := request.Headers["Authorization"]; len(got) != 1 || got[0] != "Bearer synthetic-token" {
			t.Fatalf("%s authorization = %v", request.URL, got)
		}
		if got := request.Headers["anthropic-beta"]; len(got) != 1 || got[0] != "oauth-2025-04-20" {
			t.Fatalf("%s beta = %v", request.URL, got)
		}
		if got := request.Headers["User-Agent"]; len(got) != 1 || got[0] != "claude-cli/2.1.280 (external, cli)" {
			t.Fatalf("%s user agent = %v", request.URL, got)
		}
		if got := request.Headers["Accept"]; len(got) != 1 || got[0] != "application/json" {
			t.Fatalf("%s accept = %v", request.URL, got)
		}
		_, typed := request.Headers["Content-Type"]
		if typed != (request.Method == "POST") {
			t.Fatalf("%s %s content type present = %v", request.Method, request.URL, typed)
		}
	}
}

// The grant claimed is the provider's recommendation when it can be spent, and
// otherwise the one closest to lapsing. A grant that cannot be spent is never
// chosen, recommended or not.
func TestClaudeChoosesTheGrantToClaim(t *testing.T) {
	grant := func(id, ends string, extra string) string {
		endsAt := `null`
		if ends != "" {
			endsAt = `"` + ends + `"`
		}
		return `{"id":"` + id + `","resets_total":1,"resets_left":1,"ends_at":` + endsAt + `,"usable_now":true` + extra + `}`
	}
	for name, one := range map[string]struct {
		grants string
		next   string
		want   string
	}{
		"the recommendation": {
			grants: grant("grant-a", "2026-10-07T00:00:00Z", "") + "," + grant("grant-b", "2026-10-30T00:00:00Z", ""),
			next:   "grant-b", want: "grant-b",
		},
		"soonest to lapse without one": {
			grants: grant("grant-late", "2026-10-30T00:00:00Z", "") + "," + grant("grant-soon", "2026-10-07T00:00:00Z", ""),
			want:   "grant-soon",
		},
		"a dated grant before an undated one": {
			grants: grant("grant-a", "", "") + "," + grant("grant-z", "2026-10-30T00:00:00Z", ""),
			want:   "grant-z",
		},
		"by id when the dates tie": {
			grants: grant("grant-b", "2026-10-07T00:00:00Z", "") + "," + grant("grant-a", "2026-10-07T00:00:00Z", ""),
			want:   "grant-a",
		},
		"past a paused recommendation": {
			grants: grant("grant-a", "2026-10-07T00:00:00Z", `,"paused":true`) + "," + grant("grant-b", "2026-10-30T00:00:00Z", ""),
			next:   "grant-a", want: "grant-b",
		},
		"past a grant not yet started": {
			grants: grant("grant-a", "2026-10-07T00:00:00Z", `,"starts_at":"2026-10-06T00:00:00Z"`) + "," + grant("grant-b", "2026-10-30T00:00:00Z", ""),
			want:   "grant-b",
		},
	} {
		next := `null`
		if one.next != "" {
			next = `"` + one.next + `"`
		}
		status, ok := parseClaudeStatus(mustDecode(t, `{"eligible":true,"at_limit":true,"grants":[`+one.grants+`],"next_grant_id":`+next+`}`))
		if !ok {
			t.Fatalf("%s: fixture did not parse", name)
		}
		got, blocked := chooseGrant(status, newClock().Now())
		if blocked != "" || got.id != one.want {
			t.Fatalf("%s: chose %q (blocked %q), want %q", name, got.id, blocked, one.want)
		}
	}
}

// Nothing is claimed that the provider has already said it will refuse, and
// the reason it gave comes back as the outcome. The count is what the account
// still holds, untouched.
func TestClaudeSendsNothingWhenNoGrantCanBeSpent(t *testing.T) {
	g := func(fields string) string {
		return `{"id":"grant-a","resets_total":2,"resets_left":2,"ends_at":"2026-10-20T00:00:00Z"` + fields + `}`
	}
	for name, one := range map[string]struct {
		block string
		want  string
		held  int
	}{
		"ineligible": {
			block: `{"eligible":false,"ineligible_reason":"tier","at_limit":true,"grants":[` + g(`,"usable_now":true`) + `]}`,
			want:  OutcomeIneligible, held: 2,
		},
		"cooldown running": {
			block: `{"eligible":true,"at_limit":true,"cooldown_until":"2026-10-05T13:00:00Z","grants":[` + g(`,"usable_now":true`) + `]}`,
			want:  OutcomeCooldown, held: 2,
		},
		"not at a limit": {
			block: `{"eligible":true,"at_limit":false,"grants":[` + g(`,"usable_now":true,"use_requires_limit":true`) + `]}`,
			want:  OutcomeNotLimited, held: 2,
		},
		"limit requirement unstated means required": {
			block: `{"eligible":true,"grants":[` + g(`,"usable_now":true`) + `]}`,
			want:  OutcomeNotLimited, held: 2,
		},
		"paused": {
			block: `{"eligible":true,"at_limit":true,"grants":[` + g(`,"usable_now":true,"paused":true`) + `]}`,
			want:  OutcomePaused, held: 2,
		},
		"not usable now": {
			block: `{"eligible":true,"at_limit":true,"grants":[` + g(`,"usable_now":false`) + `]}`,
			want:  OutcomeNoCredit, held: 2,
		},
		"usability unstated means not usable": {
			block: `{"eligible":true,"at_limit":true,"grants":[` + g(``) + `]}`,
			want:  OutcomeNoCredit, held: 2,
		},
		"not started": {
			block: `{"eligible":true,"at_limit":true,"grants":[` +
				`{"id":"grant-a","resets_total":1,"resets_left":1,"starts_at":"2026-10-06T00:00:00Z","usable_now":true}]}`,
			want: OutcomeNoCredit, held: 1,
		},
		// A grant past its end is gone, as the card counts it.
		"ended": {
			block: `{"eligible":true,"at_limit":true,"grants":[` +
				`{"id":"grant-a","resets_total":1,"resets_left":1,"ends_at":"2026-10-05T11:00:00Z","usable_now":true}]}`,
			want: OutcomeNoCredit, held: 0,
		},
		"exhausted": {
			block: `{"eligible":true,"at_limit":true,"grants":[` +
				`{"id":"grant-a","resets_total":2,"resets_left":0,"usable_now":true}]}`,
			want: OutcomeNoCredit, held: 0,
		},
		"no grants": {
			block: `{"eligible":true,"at_limit":true,"grants":[]}`,
			want:  OutcomeNoCredit, held: 0,
		},
	} {
		host := claudeHost(statusWith(one.block), `{"result":"reset"}`)
		result, err := redeemerOn(host, newClock()).Redeem(context.Background(), "claude", "c")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if result.Outcome != one.want || result.RemainingCount != one.held {
			t.Fatalf("%s: result = %+v, want %s holding %d", name, result, one.want, one.held)
		}
		if host.posted() {
			t.Fatalf("%s: a claim was sent the status had already refused", name)
		}
	}
}

// The provider's own reasons not to claim still leave room to: a cooldown that
// has ended, and a grant that does not need the account to be at a limit.
func TestClaudeClaimsWhenTheStatusAllowsIt(t *testing.T) {
	for name, block := range map[string]string{
		"cooldown over": `{"eligible":true,"at_limit":true,"cooldown_until":"2026-10-05T11:59:00Z","grants":[` + spendableGrant + `]}`,
		"no limit needed": `{"eligible":true,"at_limit":false,"grants":[` +
			`{"id":"grant-a","resets_total":1,"resets_left":1,"usable_now":true,"use_requires_limit":false}]}`,
	} {
		host := claudeHost(statusWith(block), `{"result":"reset"}`)
		if _, err := redeemerOn(host, newClock()).Redeem(context.Background(), "claude", "c"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !host.posted() {
			t.Fatalf("%s: no claim was sent", name)
		}
	}
}

// A status block that has drifted from the contract is a reason to send
// nothing, not to pick among what is left of it.
func TestClaudeRejectsAMalformedStatusWhole(t *testing.T) {
	for name, block := range map[string]string{
		"missing":              `null`,
		"not an object":        `[]`,
		"eligibility missing":  `{"at_limit":true,"grants":[` + spendableGrant + `]}`,
		"eligibility a string": `{"eligible":"yes","grants":[` + spendableGrant + `]}`,
		"grants not a list":    `{"eligible":true,"at_limit":true,"grants":{}}`,
		"bad grant id":         `{"eligible":true,"at_limit":true,"grants":[` + spendableGrant + `,{"id":"Grant With Spaces","resets_total":1,"resets_left":1,"usable_now":true}]}`,
		"overlong grant id":    `{"eligible":true,"at_limit":true,"grants":[{"id":"` + strings.Repeat("a", 41) + `","resets_total":1,"resets_left":1,"usable_now":true}]}`,
		"more left than total": `{"eligible":true,"at_limit":true,"grants":[{"id":"grant-a","resets_total":1,"resets_left":2,"usable_now":true}]}`,
		"negative count":       `{"eligible":true,"at_limit":true,"grants":[{"id":"grant-a","resets_total":1,"resets_left":-1,"usable_now":true}]}`,
		"fractional count":     `{"eligible":true,"at_limit":true,"grants":[{"id":"grant-a","resets_total":1.5,"resets_left":1,"usable_now":true}]}`,
		"count missing":        `{"eligible":true,"at_limit":true,"grants":[{"id":"grant-a","resets_total":1,"usable_now":true}]}`,
		"total not a count":    `{"eligible":true,"at_limit":true,"grants":[{"id":"grant-a","resets_total":"one","resets_left":1,"usable_now":true}]}`,
		"duplicate id":         `{"eligible":true,"at_limit":true,"grants":[` + spendableGrant + `,` + spendableGrant + `]}`,
		"bad timestamp":        `{"eligible":true,"at_limit":true,"grants":[{"id":"grant-a","resets_total":1,"resets_left":1,"ends_at":"next week","usable_now":true}]}`,
		"bad cooldown":         `{"eligible":true,"at_limit":true,"cooldown_until":"soon","grants":[` + spendableGrant + `]}`,
		"flag not a boolean":   `{"eligible":true,"at_limit":true,"grants":[{"id":"grant-a","resets_total":1,"resets_left":1,"usable_now":"true"}]}`,
		"limit not a boolean":  `{"eligible":true,"at_limit":1,"grants":[` + spendableGrant + `]}`,
		"clears not a list":    `{"eligible":true,"at_limit":true,"grants":[{"id":"grant-a","resets_total":1,"resets_left":1,"usable_now":true,"clears":"five_hour"}]}`,
		"reason not a string":  `{"eligible":true,"ineligible_reason":7,"at_limit":true,"grants":[` + spendableGrant + `]}`,
	} {
		host := claudeHost(statusWith(block), `{"result":"reset"}`)
		if _, err := redeemerOn(host, newClock()).Redeem(context.Background(), "claude", "c"); !errors.Is(err, ErrRefused) {
			t.Fatalf("%s: err = %v, want ErrRefused", name, err)
		}
		if host.posted() {
			t.Fatalf("%s: a claim was sent on a malformed status", name)
		}
	}
}

// Claude Code's schema requires only an id and resets_left; a grant without a
// total is well formed and can be claimed.
func TestClaudeAcceptsAGrantWithoutATotal(t *testing.T) {
	host := claudeHost(statusWith(`{"eligible":true,"at_limit":true,"grants":[`+
		`{"id":"grant-a","resets_left":1,"usable_now":true}]}`), `{"result":"reset"}`)
	if result, err := redeemerOn(host, newClock()).Redeem(context.Background(), "claude", "c"); err != nil || result.Outcome != OutcomeReset {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

// What remains is counted the way the card counts it: a lapsed grant's resets
// are not held, and a grant not yet started is.
func TestClaudeCountsWhatTheAccountStillHolds(t *testing.T) {
	host := claudeHost(statusWith(`{"eligible":true,"at_limit":true,"next_grant_id":"grant-a","grants":[`+
		spendableGrant+`,`+
		`{"id":"grant-lapsed","resets_total":4,"resets_left":4,"ends_at":"2026-10-01T00:00:00Z","usable_now":true},`+
		`{"id":"grant-later","resets_total":3,"resets_left":3,"starts_at":"2026-11-01T00:00:00Z","usable_now":true}]}`),
		`{"result":"reset"}`)
	result, err := redeemerOn(host, newClock()).Redeem(context.Background(), "claude", "c")
	// Two on the claimed grant, less the one spent, and three not yet started.
	if err != nil || result.RemainingCount != 4 {
		t.Fatalf("result = %+v, err = %v; want 4 left", result, err)
	}
}

// A recommendation naming a grant that is not in the block is ignored rather
// than followed.
func TestClaudeIgnoresARecommendationForAnUnknownGrant(t *testing.T) {
	status, ok := parseClaudeStatus(mustDecode(t, `{"eligible":true,"at_limit":true,"grants":[`+spendableGrant+`],"next_grant_id":"grant-zzz"}`))
	if !ok || status.nextGrantID != "" {
		t.Fatalf("status = %+v, ok = %v", status, ok)
	}
}

// Every read failure ends before the claim, and says which kind it was.
func TestClaudeStopsAtAFailedRead(t *testing.T) {
	for name, one := range map[string]struct {
		setup func(*fakeHost)
		want  error
	}{
		"profile throttled":    {func(h *fakeHost) { h.status[claudeProfileURL] = 429 }, ErrRateLimited},
		"profile refused":      {func(h *fakeHost) { h.status[claudeProfileURL] = 401 }, ErrRefused},
		"profile failing":      {func(h *fakeHost) { h.status[claudeProfileURL] = 500 }, ErrRefused},
		"profile unreachable":  {func(h *fakeHost) { h.errs[claudeProfileURL] = errors.New("dial failed") }, ErrUnavailable},
		"profile unreadable":   {func(h *fakeHost) { h.bodies[claudeProfileURL] = `<html>` }, ErrRefused},
		"organization missing": {func(h *fakeHost) { h.bodies[claudeProfileURL] = `{"account":{}}` }, ErrRefused},
		"organization not a uuid": {func(h *fakeHost) {
			h.bodies[claudeProfileURL] = `{"organization":{"uuid":"../../admin"}}`
		}, ErrRefused},
		"status throttled":      {func(h *fakeHost) { h.status[claudeStatusURL] = 429 }, ErrRateLimited},
		"status refused":        {func(h *fakeHost) { h.status[claudeStatusURL] = 403 }, ErrRefused},
		"status unreachable":    {func(h *fakeHost) { h.errs[claudeStatusURL] = errors.New("dial failed") }, ErrUnavailable},
		"no grant block":        {func(h *fakeHost) { h.bodies[claudeStatusURL] = `{"five_hour":{"utilization":10}}` }, ErrRefused},
		"no access token":       {func(h *fakeHost) { h.auth = `{"type":"claude","email":"northwind@example.com"}` }, ErrNoAccessToken},
		"credential unreadable": {func(h *fakeHost) { h.authErr = errors.New("missing") }, ErrUnavailable},
	} {
		host := claudeHost(spendableStatus(), `{"result":"reset"}`)
		one.setup(host)
		if _, err := redeemerOn(host, newClock()).Redeem(context.Background(), "claude", "c"); !errors.Is(err, one.want) {
			t.Fatalf("%s: err = %v, want %v", name, err, one.want)
		}
		if host.posted() {
			t.Fatalf("%s: a claim was sent after a failed read", name)
		}
	}
}

// The claim's answer, every way it can come back. Six results are answers;
// throttling and an authentication refusal prove nothing was spent; anything
// else may have spent the grant and is reported as unknown.
func TestClaudeReadsEveryClaimAnswer(t *testing.T) {
	for name, one := range map[string]struct {
		reply   reply
		outcome string
		left    int
		err     error
	}{
		"reset":                {reply: reply{body: `{"result":"reset","resets_left":1,"cleared":["five_hour"]}`}, outcome: OutcomeReset, left: 1},
		"reset, count unsent":  {reply: reply{body: `{"result":"reset"}`}, outcome: OutcomeReset, left: 1},
		"already used":         {reply: reply{body: `{"result":"already_used","resets_left":0}`}, outcome: OutcomeAlreadyUsed, left: 0},
		"not limited":          {reply: reply{body: `{"result":"not_limited","reason":"Your account is not at a limit."}`}, outcome: OutcomeNotLimited, left: 2},
		"cooldown":             {reply: reply{body: `{"result":"cooldown","cooldown_until":"2026-10-05T17:00:00Z"}`}, outcome: OutcomeCooldown, left: 2},
		"ineligible":           {reply: reply{body: `{"result":"ineligible"}`}, outcome: OutcomeIneligible, left: 2},
		"unavailable":          {reply: reply{body: `{"result":"unavailable"}`}, outcome: OutcomeFailed, left: 2},
		"throttled":            {reply: reply{status: 429, body: `{"error":"rate_limited"}`}, err: ErrRateLimited},
		"unauthorized":         {reply: reply{status: 401}, err: ErrRefused},
		"forbidden":            {reply: reply{status: 403}, err: ErrRefused},
		"server error":         {reply: reply{status: 500, body: `{"result":"reset"}`}, err: ErrOutcomeUnknown},
		"gateway timeout":      {reply: reply{status: 504}, err: ErrOutcomeUnknown},
		"conflict":             {reply: reply{status: 409, body: `{"result":"already_used"}`}, err: ErrOutcomeUnknown},
		"unreadable 2xx":       {reply: reply{body: `<html>ok</html>`}, err: ErrOutcomeUnknown},
		"unknown result":       {reply: reply{body: `{"result":"queued"}`}, err: ErrOutcomeUnknown},
		"no result":            {reply: reply{body: `{}`}, err: ErrOutcomeUnknown},
		"no response":          {reply: reply{err: errors.New("context deadline exceeded")}, err: ErrOutcomeUnknown},
		"provider text in 2xx": {reply: reply{body: `{"result":"account 12345 suspended"}`}, err: ErrOutcomeUnknown},
	} {
		host := claudeHost(spendableStatus(), "")
		host.queue(claudeClaimURL, one.reply)
		result, err := redeemerOn(host, newClock()).Redeem(context.Background(), "claude", "c")
		if one.err != nil {
			if !errors.Is(err, one.err) {
				t.Fatalf("%s: err = %v, want %v", name, err, one.err)
			}
			continue
		}
		if err != nil || result.Outcome != one.outcome || result.RemainingCount != one.left {
			t.Fatalf("%s: result = %+v, err = %v; want %s with %d left", name, result, err, one.outcome, one.left)
		}
	}
}

// The organization goes into the URL path and the ids into the body. Any of
// them in a shape the provider does not publish is refused before a request
// is built, whatever produced it.
func TestClaudeClaimRequestRefusesMalformedIdentifiers(t *testing.T) {
	good := pendingClaim{creditID: "grant-a", requestID: "0d4c3f8e-1b2a-4c5d-8e6f-7a8b9c0d1e2f", account: claudeOrg}
	if _, ok := claudeClaimRequest("tok", good); !ok {
		t.Fatal("a well-formed claim was refused")
	}
	for name, change := range map[string]func(*pendingClaim){
		"grant with a slash":     func(c *pendingClaim) { c.creditID = "grant/../a" },
		"grant upper case":       func(c *pendingClaim) { c.creditID = "Grant-A" },
		"grant empty":            func(c *pendingClaim) { c.creditID = "" },
		"request id with spaces": func(c *pendingClaim) { c.requestID = "a b" },
		"request id overlong":    func(c *pendingClaim) { c.requestID = strings.Repeat("a", 65) },
		"organization a path":    func(c *pendingClaim) { c.account = "../../v1/admin" },
		"organization empty":     func(c *pendingClaim) { c.account = "" },
	} {
		claim := good
		change(&claim)
		if _, ok := claudeClaimRequest("tok", claim); ok {
			t.Fatalf("%s: a malformed claim was built", name)
		}
	}
}

// The claim is bounded at Claude Code's own 25 seconds, and each read at the
// shorter read bound.
func TestClaudeBoundsEveryRequest(t *testing.T) {
	host := claudeHost(spendableStatus(), `{"result":"reset"}`)
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	if _, err := redeemerOn(host, newClock()).Redeem(ctx, "claude", "c"); err != nil {
		t.Fatal(err)
	}
	for i, budget := range host.budgets {
		limit := readTimeout
		if host.requests[i].Method == "POST" {
			limit = spendTimeout
		}
		if budget <= 0 || budget > limit {
			t.Fatalf("%s had %v, want at most %v", host.requests[i].URL, budget, limit)
		}
	}
	if spend := host.budgets[2]; spend <= 20*time.Second {
		t.Fatalf("the claim had only %v of its 25 seconds", spend)
	}
}
