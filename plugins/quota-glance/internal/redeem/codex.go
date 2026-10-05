package redeem

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
)

const (
	codexCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	codexConsumeURL = codexCreditsURL + "/consume"

	// codexUserAgent is CLI-shaped because the ChatGPT backend edge rejects
	// clients that do not look like one, exactly as quota-cache found on the
	// usage endpoint. The comment segment identifies the real caller.
	codexUserAgent = "codex_cli_rs/0.0.0 (cpa-plugins/quota-glance)"
)

// redeemCodex spends one Codex rate-limit reset credit: an inventory read, then
// one consume naming the credit closest to lapsing. A repeat of an unresolved
// claim skips the inventory and consumes the same credit with the same
// redeem_request_id.
func (r *Redeemer) redeemCodex(ctx context.Context, authIndex string, retry pendingClaim, isRetry bool) (Result, error) {
	creds, err := r.codexCredentialsOf(ctx, authIndex)
	if err != nil {
		if isRetry {
			return Result{}, r.stillUnknown(authIndex, retry)
		}
		return Result{}, err
	}
	claim := retry
	if isRetry {
		// The credit id names a credit on one account. Sent on behalf of
		// another, it is at best refused and at worst a different spend.
		if creds.accountID != retry.account {
			return Result{}, ErrIdentityChanged
		}
	} else {
		available, err := r.codexAvailable(ctx, creds)
		if err != nil {
			return Result{}, err
		}
		if len(available) == 0 {
			return Result{Outcome: OutcomeNoCredit}, nil
		}
		// A fresh idempotency key per claim, as the provider's own clients
		// send. It is reused only by the journal, for a repeat of this same
		// claim; reusing one across two genuine presses would silently drop
		// the second.
		id, err := requestID()
		if err != nil {
			return Result{}, ErrUnavailable
		}
		claim = pendingClaim{
			provider: providerCodex, creditID: available[0], requestID: id, account: creds.accountID,
			createdAt: r.clock(), others: len(available) - 1, grantLeft: 1,
		}
	}
	// The request shape is the provider's: a credit id and a redeem request id.
	body, err := json.Marshal(map[string]string{"credit_id": claim.creditID, "redeem_request_id": claim.requestID})
	if err != nil {
		return Result{}, ErrUnavailable
	}
	send := func(ctx context.Context) (protocol.HostHTTPResponse, error) {
		return r.host.HTTPDo(ctx, codexRequest("POST", codexConsumeURL, creds, body))
	}
	return r.spend(ctx, authIndex, claim, isRetry, send, func(response protocol.HostHTTPResponse) answer {
		return codexAnswer(response, claim, isRetry)
	})
}

// codexAnswer reads a consume response.
//
// Any 2xx means the provider accepted the request, and Codex consumes the
// credit on acceptance. Whatever the body said afterwards — a code this build
// has never seen, or nothing readable at all — the credit is gone, and
// reporting a failure would invite a second press and a second credit.
//
// The statuses that prove nothing was spent are the ones a request is turned
// away with before it is acted on: authentication (401, 403) and throttling
// (429). Every other status is unknown, and a 5xx above all. A 502 or 504 is
// what a gateway returns when the service behind it took too long, which is
// not the same as the service not having acted; Codex documents no status that
// promises otherwise, so none is assumed to.
func codexAnswer(response protocol.HostHTTPResponse, claim pendingClaim, isRetry bool) answer {
	switch status := response.StatusCode; {
	case status >= 200 && status < 300:
		result := Result{Outcome: OutcomeReset, RemainingCount: claim.others}
		root, err := decode(response.Body)
		if err != nil {
			return answer{kind: answerSettled, result: result}
		}
		code := stringOf(root, "code")
		result.Outcome = codexOutcomeOf(code)
		result.WindowsReset = intOf(root, "windows_reset", "windowsReset")
		if result.Outcome != OutcomeNoCredit {
			return answer{kind: answerSettled, result: result}
		}
		// "Already redeemed" on the repeat of an unknown claim names the
		// credit that claim chose while it was still available: the most
		// likely hand that redeemed it is the earlier attempt.
		if isRetry && (code == "already_redeemed" || code == "alreadyRedeemed") {
			result.Outcome, result.WindowsReset = OutcomeAlreadyUsed, 0
			return answer{kind: answerSettled, result: result}
		}
		return answer{kind: answerRefused, result: result}
	case status == 401 || status == 403:
		return answer{kind: answerRefused, err: ErrRefused}
	case status == 429:
		return answer{kind: answerRefused, err: ErrRateLimited}
	default:
		return answer{kind: answerUnknown}
	}
}

// codexOutcomeOf maps the provider's code onto this package's vocabulary.
//
// An unrecognized code is treated as a successful reset rather than as a
// failure, because control only reaches here on a 2xx: the credit has been
// spent, and the only question left is what to print. Calling that a failure
// would be the one mistake that costs a second credit.
func codexOutcomeOf(code string) string {
	switch code {
	case "reset":
		return OutcomeReset
	case "nothing_to_reset", "nothingToReset":
		return OutcomeNothingToReset
	case "no_credit", "noCredit", "already_redeemed", "alreadyRedeemed":
		return OutcomeNoCredit
	default:
		return OutcomeReset
	}
}

// codexAvailable lists the ids of credits the provider currently calls
// spendable, soonest expiry first, so the credit closest to lapsing is the one
// spent.
func (r *Redeemer) codexAvailable(ctx context.Context, creds codexCredentials) ([]string, error) {
	response, err := r.read(ctx, codexRequest("GET", codexCreditsURL, creds, nil))
	if err != nil {
		return nil, ErrUnavailable
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, ErrRefused
	}
	root, err := decode(response.Body)
	if err != nil {
		return nil, ErrRefused
	}
	list, _ := root["credits"].([]any)
	type candidate struct {
		id      string
		expires time.Time
	}
	candidates := make([]candidate, 0, len(list))
	for _, item := range list {
		credit, ok := item.(map[string]any)
		if !ok || stringOf(credit, "status") != "available" {
			continue
		}
		id := stringOf(credit, "id")
		// Bounded, and matched against the shape the provider actually sends.
		// An id is echoed straight back in a request body; an unbounded string
		// from a response has no business being one.
		if id == "" || len(id) > 128 || strings.ContainsAny(id, "\x00\n\r\"\\") {
			continue
		}
		at, _ := time.Parse(time.RFC3339, stringOf(credit, "expires_at"))
		candidates = append(candidates, candidate{id: id, expires: at})
	}
	// Undated credits sort last: a known deadline is a reason to spend one
	// first, and an unknown one is not a reason to spend it before a dated one.
	sortStable(candidates, func(a, b candidate) bool {
		if a.expires.IsZero() != b.expires.IsZero() {
			return !a.expires.IsZero()
		}
		return a.expires.Before(b.expires)
	})
	ids := make([]string, 0, len(candidates))
	for _, one := range candidates {
		ids = append(ids, one.id)
	}
	return ids, nil
}

// codexCredentials is the pair of values one Codex request needs. Nothing else
// is kept from the credential document, and this value never leaves the
// package.
type codexCredentials struct {
	accessToken string
	accountID   string
}

// codexCredentialsOf decodes only the two fields a request needs out of the
// physical credential document.
func (r *Redeemer) codexCredentialsOf(ctx context.Context, authIndex string) (codexCredentials, error) {
	raw, err := r.authDocument(ctx, authIndex)
	if err != nil {
		return codexCredentials{}, err
	}
	var doc struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
		IDToken     string `json:"id_token"`
		// Codex CLI nests the same fields one level down.
		Tokens *struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
			IDToken     string `json:"id_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return codexCredentials{}, ErrNoAccessToken
	}
	out := codexCredentials{accessToken: doc.AccessToken, accountID: doc.AccountID}
	idToken := doc.IDToken
	if doc.Tokens != nil {
		if out.accessToken == "" {
			out.accessToken = doc.Tokens.AccessToken
		}
		if out.accountID == "" {
			out.accountID = doc.Tokens.AccountID
		}
		if idToken == "" {
			idToken = doc.Tokens.IDToken
		}
	}
	if out.accessToken == "" {
		return codexCredentials{}, ErrNoAccessToken
	}
	if out.accountID == "" {
		out.accountID = accountIDFromIDToken(idToken)
	}
	if out.accountID == "" {
		return codexCredentials{}, ErrNoAccountID
	}
	return out, nil
}

func codexRequest(method, url string, creds codexCredentials, body []byte) protocol.HostHTTPRequest {
	headers := map[string][]string{
		"Authorization":      {"Bearer " + creds.accessToken},
		"Accept":             {"application/json"},
		"User-Agent":         {codexUserAgent},
		"Chatgpt-Account-Id": {creds.accountID},
	}
	if body != nil {
		headers["Content-Type"] = []string{"application/json"}
	}
	return protocol.HostHTTPRequest{Method: method, URL: url, Headers: headers, Body: body}
}
