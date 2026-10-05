package redeem

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
)

// Claude's reset grants, which Anthropic runs as the "cedar_ember" program.
//
// The contract below is Claude Code's own, as the CPA console also implements
// it: the grant status rides on the usage endpoint behind a query flag, the
// claim is addressed to the account's organization, and both are authenticated
// with the credential's OAuth access token.
const (
	claudeOrigin     = "https://api.anthropic.com"
	claudeProfileURL = claudeOrigin + "/api/oauth/profile"
	// claudeStatusURL is Claude Code's exact path. skip_spend skips the
	// spend-store read behind extra_usage, which deciding on a claim does not
	// need; quota-cache leaves it off only because it collects extra_usage from
	// the same response.
	claudeStatusURL = claudeOrigin + "/api/oauth/usage?cedar_ember=1&skip_spend=1"
	claudeProgram   = "cedar_ember"
	claudeOAuthBeta = "oauth-2025-04-20"
	// claudeUserAgent identifies as Claude Code because Anthropic buckets
	// these endpoints by User-Agent, and any other caller lands in a far
	// tighter bucket and is throttled within a few requests. It is Claude
	// Code's own form, verbatim and with no caller segment, because the grant
	// program also judges eligibility by client ("surface" and "cli_version"
	// are both ineligible reasons). It matches quota-cache and the string CPA's
	// management UI sends on these same endpoints.
	claudeUserAgent = "claude-cli/2.1.280 (external, cli)"
)

// Shapes every identifier must have before it goes into a request. Each one is
// read from a response or generated here and then echoed back — in a request
// body, and for the organization in the URL path itself — so each is matched
// against the provider's own published shape first.
var (
	claudeGrantIDPattern   = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
	claudeRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	claudeOrgPattern       = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// claudeWindows are the usage windows a grant can report clearing. A name
// outside this list is not counted: it is a provider string, and a count of
// windows this build cannot name is not one the operator can check.
var claudeWindows = map[string]bool{
	"five_hour":                  true,
	"seven_day":                  true,
	"seven_day_overage_included": true,
	"seven_day_opus":             true,
	"seven_day_sonnet":           true,
	"seven_day_cowork":           true,
	"seven_day_omelette":         true,
	"seven_day_oauth_apps":       true,
}

// redeemClaude spends one Claude reset grant: the organization from the
// profile, the grant status, and one claim naming the grant the provider
// recommends. A repeat of an unresolved claim reads the organization again —
// it must be the same one — skips the status, and claims the same grant with
// the same request_id.
func (r *Redeemer) redeemClaude(ctx context.Context, authIndex string, retry pendingClaim, isRetry bool) (Result, error) {
	token, err := r.claudeAccessToken(ctx, authIndex)
	var org string
	if err == nil {
		org, err = r.claudeOrganization(ctx, token)
	}
	if err != nil {
		// Nothing was sent by this press. On a repeat that is still not an
		// answer: the earlier claim is as unknown as it was.
		if isRetry {
			return Result{}, r.stillUnknown(authIndex, retry)
		}
		return Result{}, err
	}
	return r.claudeClaim(ctx, authIndex, token, org, retry, isRetry)
}

func (r *Redeemer) claudeClaim(ctx context.Context, authIndex, token, org string, retry pendingClaim, isRetry bool) (Result, error) {
	claim := retry
	if isRetry {
		// A claim is addressed to an organization. If the credential now signs
		// in to a different one, repeating it there is a new claim against an
		// account that never asked for one.
		if org != retry.account {
			return Result{}, ErrIdentityChanged
		}
	} else {
		status, err := r.claudeGrantStatus(ctx, token)
		if err != nil {
			return Result{}, err
		}
		now := r.clock()
		grant, blocked := chooseGrant(status, now)
		held := status.held(now)
		if blocked != "" {
			return Result{Outcome: blocked, RemainingCount: held}, nil
		}
		id, err := requestID()
		if err != nil {
			return Result{}, ErrUnavailable
		}
		claim = pendingClaim{
			provider: providerClaude, creditID: grant.id, requestID: id, account: org,
			createdAt: now, others: held - grant.left, grantLeft: grant.left,
		}
	}
	request, ok := claudeClaimRequest(token, claim)
	if !ok {
		if isRetry {
			return Result{}, r.stillUnknown(authIndex, claim)
		}
		return Result{}, ErrUnavailable
	}
	send := func(ctx context.Context) (protocol.HostHTTPResponse, error) {
		return r.host.HTTPDo(ctx, request)
	}
	return r.spend(ctx, authIndex, claim, isRetry, send, func(response protocol.HostHTTPResponse) answer {
		return claudeAnswer(response, claim)
	})
}

// claudeClaimRequest builds the claim, refusing any identifier that is not the
// shape the provider publishes for it.
func claudeClaimRequest(token string, claim pendingClaim) (protocol.HostHTTPRequest, bool) {
	if !claudeGrantIDPattern.MatchString(claim.creditID) ||
		!claudeRequestIDPattern.MatchString(claim.requestID) ||
		!claudeOrgPattern.MatchString(claim.account) {
		return protocol.HostHTTPRequest{}, false
	}
	body, err := json.Marshal(struct {
		Program   string `json:"program"`
		GrantID   string `json:"grant_id"`
		RequestID string `json:"request_id"`
	}{claudeProgram, claim.creditID, claim.requestID})
	if err != nil {
		return protocol.HostHTTPRequest{}, false
	}
	url := claudeOrigin + "/api/organizations/" + claim.account + "/reset_rate_limits"
	return claudeRequest("POST", url, token, body), true
}

// claudeAnswer reads a claim response.
//
// Only a 2xx whose result is one of the six the endpoint defines is an answer.
// Of the rest, two statuses prove nothing was spent — throttling (429) and an
// authentication refusal (401, 403), both of which turn a request away before
// it is acted on — and everything else is unknown: a 5xx, any other status,
// and a 2xx this code cannot read, where the provider answered but not in a
// shape that says what it did.
func claudeAnswer(response protocol.HostHTTPResponse, claim pendingClaim) answer {
	switch status := response.StatusCode; {
	case status == 429:
		return answer{kind: answerRefused, err: ErrRateLimited}
	case status == 401 || status == 403:
		return answer{kind: answerRefused, err: ErrRefused}
	case status < 200 || status >= 300:
		return answer{kind: answerUnknown}
	}
	root, err := decode(response.Body)
	if err != nil {
		return answer{kind: answerUnknown}
	}
	held := claim.others + claim.grantLeft
	switch stringOf(root, "result") {
	case "reset":
		return answer{kind: answerSettled, result: Result{
			Outcome: OutcomeReset, WindowsReset: clearedCount(root), RemainingCount: claim.leftAfterSpend(root),
		}}
	case "already_used":
		return answer{kind: answerSettled, result: Result{Outcome: OutcomeAlreadyUsed, RemainingCount: claim.leftAfterSpend(root)}}
	case "not_limited":
		return answer{kind: answerRefused, result: Result{Outcome: OutcomeNotLimited, RemainingCount: held}}
	case "cooldown":
		return answer{kind: answerRefused, result: Result{Outcome: OutcomeCooldown, RemainingCount: held}}
	case "ineligible":
		return answer{kind: answerRefused, result: Result{Outcome: OutcomeIneligible, RemainingCount: held}}
	case "unavailable":
		return answer{kind: answerRefused, result: Result{Outcome: OutcomeFailed, RemainingCount: held}}
	}
	return answer{kind: answerUnknown}
}

// leftAfterSpend is what the account holds once the claimed grant has given up
// a reset: the provider's own count for that grant when it sent one, and the
// count read before the claim less one when it did not.
func (c pendingClaim) leftAfterSpend(root map[string]any) int {
	if left, ok := countOf(root["resets_left"]); ok {
		return c.others + left
	}
	if c.grantLeft > 0 {
		return c.others + c.grantLeft - 1
	}
	return c.others
}

// clearedCount counts the distinct windows the claim says it cleared.
func clearedCount(root map[string]any) int {
	list, _ := root["cleared"].([]any)
	seen := map[string]bool{}
	for _, item := range list {
		if window, ok := item.(string); ok && claudeWindows[window] {
			seen[window] = true
		}
	}
	return len(seen)
}

// claudeAccessToken decodes the one field a Claude request needs out of the
// physical credential document, in the two places CPA and quota-cache read it
// from.
func (r *Redeemer) claudeAccessToken(ctx context.Context, authIndex string) (string, error) {
	raw, err := r.authDocument(ctx, authIndex)
	if err != nil {
		return "", err
	}
	var doc struct {
		AccessToken string `json:"access_token"`
		Tokens      *struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return "", ErrNoAccessToken
	}
	token := doc.AccessToken
	if token == "" && doc.Tokens != nil {
		token = doc.Tokens.AccessToken
	}
	if token == "" {
		return "", ErrNoAccessToken
	}
	return token, nil
}

// claudeOrganization reads the uuid of the organization the credential signs in
// to, which is where a claim is addressed. It is lower-cased so the same
// organization always compares equal to itself across two reads.
func (r *Redeemer) claudeOrganization(ctx context.Context, token string) (string, error) {
	response, err := r.read(ctx, claudeRequest("GET", claudeProfileURL, token, nil))
	if err != nil {
		return "", ErrUnavailable
	}
	if err := readError(response.StatusCode); err != nil {
		return "", err
	}
	root, err := decode(response.Body)
	if err != nil {
		return "", ErrRefused
	}
	org, _ := root["organization"].(map[string]any)
	uuid := stringOf(org, "uuid")
	if !claudeOrgPattern.MatchString(uuid) {
		return "", ErrRefused
	}
	return strings.ToLower(uuid), nil
}

// claudeGrantStatus reads the account's grants and the provider's verdict on
// spending one.
func (r *Redeemer) claudeGrantStatus(ctx context.Context, token string) (claudeStatus, error) {
	response, err := r.read(ctx, claudeRequest("GET", claudeStatusURL, token, nil))
	if err != nil {
		return claudeStatus{}, ErrUnavailable
	}
	if err := readError(response.StatusCode); err != nil {
		return claudeStatus{}, err
	}
	root, err := decode(response.Body)
	if err != nil {
		return claudeStatus{}, ErrRefused
	}
	status, ok := parseClaudeStatus(root["cedar_ember"])
	if !ok {
		return claudeStatus{}, ErrRefused
	}
	return status, nil
}

func claudeRequest(method, url, token string, body []byte) protocol.HostHTTPRequest {
	headers := map[string][]string{
		"Authorization":  {"Bearer " + token},
		"Accept":         {"application/json"},
		"anthropic-beta": {claudeOAuthBeta},
		"User-Agent":     {claudeUserAgent},
	}
	if body != nil {
		headers["Content-Type"] = []string{"application/json"}
	}
	return protocol.HostHTTPRequest{Method: method, URL: url, Headers: headers, Body: body}
}

// claudeStatus is the cedar_ember block of the usage response, reduced to what
// deciding on a claim needs.
type claudeStatus struct {
	eligible      bool
	atLimit       bool
	cooldownUntil time.Time
	nextGrantID   string
	grants        []claudeGrant
}

type claudeGrant struct {
	id   string
	left int
	// startsAt and endsAt are zero when the provider sent none.
	startsAt, endsAt time.Time
	paused           bool
	usableNow        bool
	requiresLimit    bool
}

// held is every reset the account holds across the grants that have not
// ended, counted as quota-cache counts the card: a grant not yet started is
// held, just not spendable, and one past its end is gone.
func (s claudeStatus) held(now time.Time) int {
	sum := 0
	for _, grant := range s.grants {
		if grant.live(now) {
			sum += grant.left
		}
	}
	return sum
}

// live reports whether a grant still has a reset in it that has not lapsed.
func (g claudeGrant) live(now time.Time) bool {
	return g.left > 0 && (g.endsAt.IsZero() || g.endsAt.After(now))
}

// parseClaudeStatus reads the block strictly: one malformed grant, a duplicate
// id, or an unreadable timestamp rejects all of it.
//
// Claude Code skips a malformed grant and carries on, and so does quota-cache
// when it counts them. That is the right call for something that only
// displays them, and the wrong one for this package, which is about to spend
// one: a block that has drifted from the contract is a reason to send nothing,
// not to pick among what is left. This is the CPA console's rule for the same
// reason. What counts as malformed is Claude Code's schema, though: a field
// that schema lets be absent may be absent here too.
//
// The missing-value defaults all fall on the refusing side. A grant that does
// not say it is usable now is not; one that does not say whether it needs the
// account to be at a limit is assumed to.
func parseClaudeStatus(value any) (claudeStatus, bool) {
	block, ok := value.(map[string]any)
	if !ok {
		return claudeStatus{}, false
	}
	eligible, ok := block["eligible"].(bool)
	if !ok {
		return claudeStatus{}, false
	}
	status := claudeStatus{eligible: eligible, grants: []claudeGrant{}}
	if reason, present := block["ineligible_reason"]; present && reason != nil {
		if _, ok := reason.(string); !ok {
			return claudeStatus{}, false
		}
	}
	var okLimit, okCooldown, okWeekly bool
	status.atLimit, okLimit = optionalBool(block["at_limit"], false)
	status.cooldownUntil, okCooldown = optionalTime(block["cooldown_until"])
	_, okWeekly = optionalTime(block["weekly_resets_at"])
	if !okLimit || !okCooldown || !okWeekly {
		return claudeStatus{}, false
	}
	var list []any
	switch raw := block["grants"].(type) {
	case nil:
	case []any:
		list = raw
	default:
		return claudeStatus{}, false
	}
	seen := map[string]bool{}
	for _, item := range list {
		grant, ok := parseClaudeGrant(item)
		if !ok || seen[grant.id] {
			return claudeStatus{}, false
		}
		seen[grant.id] = true
		status.grants = append(status.grants, grant)
	}
	// A recommendation naming a grant the block does not contain is no
	// recommendation.
	if next, ok := block["next_grant_id"].(string); ok && seen[next] {
		status.nextGrantID = next
	}
	return status, true
}

func parseClaudeGrant(value any) (claudeGrant, bool) {
	item, ok := value.(map[string]any)
	if !ok {
		return claudeGrant{}, false
	}
	grant := claudeGrant{id: stringOf(item, "id")}
	if !claudeGrantIDPattern.MatchString(grant.id) {
		return claudeGrant{}, false
	}
	left, ok := countOf(item["resets_left"])
	if !ok {
		return claudeGrant{}, false
	}
	// resets_total is optional in Claude Code's schema. Present, it must be a
	// count, and one that the grant has not overspent.
	if raw, present := item["resets_total"]; present && raw != nil {
		if total, ok := countOf(raw); !ok || left > total {
			return claudeGrant{}, false
		}
	}
	grant.left = left
	var okStart, okEnd, okPaused, okUsable, okRequires bool
	grant.startsAt, okStart = optionalTime(item["starts_at"])
	grant.endsAt, okEnd = optionalTime(item["ends_at"])
	grant.paused, okPaused = optionalBool(item["paused"], false)
	grant.usableNow, okUsable = optionalBool(item["usable_now"], false)
	grant.requiresLimit, okRequires = optionalBool(item["use_requires_limit"], true)
	if !okStart || !okEnd || !okPaused || !okUsable || !okRequires {
		return claudeGrant{}, false
	}
	if clears := item["clears"]; clears != nil {
		if _, ok := clears.([]any); !ok {
			return claudeGrant{}, false
		}
	}
	return grant, true
}

// optionalBool is a boolean that may be absent or null, in which case it takes
// fallback. Anything else that is not a boolean is malformed.
func optionalBool(value any, fallback bool) (bool, bool) {
	switch v := value.(type) {
	case nil:
		return fallback, true
	case bool:
		return v, true
	}
	return false, false
}

// optionalTime is an RFC 3339 instant that may be absent or null, in which case
// it is the zero time. A string that does not parse is malformed.
func optionalTime(value any) (time.Time, bool) {
	switch v := value.(type) {
	case nil:
		return time.Time{}, true
	case string:
		at, err := time.Parse(time.RFC3339, v)
		return at, err == nil
	}
	return time.Time{}, false
}

// spendable reports whether a grant can be claimed right now, by the same
// tests the CPA console applies before it offers the button.
func (g claudeGrant) spendable(status claudeStatus, now time.Time) bool {
	switch {
	case g.paused, !g.usableNow, g.left <= 0:
		return false
	case g.requiresLimit && !status.atLimit:
		return false
	case !g.startsAt.IsZero() && g.startsAt.After(now):
		return false
	case !g.endsAt.IsZero() && !g.endsAt.After(now):
		return false
	}
	return true
}

// chooseGrant picks the grant to claim, or says why there is none.
//
// The provider's recommendation wins when it is spendable — the claim endpoint
// can refuse any other grant as not the next one. Without a spendable
// recommendation, the spendable grant that lapses soonest is taken, so the
// grant closest to being lost is the one used, and ties go by id so the choice
// does not move between two reads of the same status.
//
// When nothing is spendable, the reason given is the account's own first —
// ineligible, then a cooldown still running — and otherwise the reason the
// grant that would have been chosen gives. Not being at a limit is reported
// ahead of a grant merely saying it is not usable now, because when both are
// true the first is the one the operator can act on.
func chooseGrant(status claudeStatus, now time.Time) (claudeGrant, string) {
	if !status.eligible {
		return claudeGrant{}, OutcomeIneligible
	}
	if !status.cooldownUntil.IsZero() && status.cooldownUntil.After(now) {
		return claudeGrant{}, OutcomeCooldown
	}
	var spendable, held []claudeGrant
	for _, grant := range status.grants {
		if grant.live(now) {
			held = append(held, grant)
		}
		if grant.spendable(status, now) {
			if grant.id == status.nextGrantID {
				return grant, ""
			}
			spendable = append(spendable, grant)
		}
	}
	soonestEnd := func(a, b claudeGrant) bool {
		if a.endsAt.IsZero() != b.endsAt.IsZero() {
			return !a.endsAt.IsZero()
		}
		if !a.endsAt.Equal(b.endsAt) {
			return a.endsAt.Before(b.endsAt)
		}
		return a.id < b.id
	}
	if len(spendable) > 0 {
		sortStable(spendable, soonestEnd)
		return spendable[0], ""
	}
	if len(held) == 0 {
		return claudeGrant{}, OutcomeNoCredit
	}
	sortStable(held, soonestEnd)
	preferred := held[0]
	for _, grant := range held {
		if grant.id == status.nextGrantID {
			preferred = grant
		}
	}
	switch {
	case preferred.paused:
		return claudeGrant{}, OutcomePaused
	case preferred.requiresLimit && !status.atLimit:
		return claudeGrant{}, OutcomeNotLimited
	}
	return claudeGrant{}, OutcomeNoCredit
}
