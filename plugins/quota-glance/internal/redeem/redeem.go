// Package redeem spends one banked Codex rate-limit reset.
//
// It is the only part of quota-glance that contacts a provider, and it runs
// only when the operator presses the button and confirms. Nothing on a timer,
// nothing on a rebuild, and nothing on a route that merely reads reaches this
// code. quota-cache still owns every scheduled request; this is a write, made
// once, on a person's instruction.
//
// The action is irreversible. A banked reset is consumed the moment the
// provider accepts the POST — there is no unspend — so every guard here is
// about not making that request by accident: the credential must be a Codex
// credential, it must hold a credit that is actually available, and the caller
// must have confirmed. The confirmation itself lives in the browser; what this
// package guarantees is that a request which arrives without one is refused.
package redeem

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
)

const (
	creditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	consumeURL = creditsURL + "/consume"

	// userAgent is CLI-shaped because the ChatGPT backend edge rejects clients
	// that do not look like one, exactly as quota-cache found on the usage
	// endpoint. The comment segment identifies the real caller.
	userAgent = "codex_cli_rs/0.0.0 (cpa-plugins/quota-glance)"

	// maxResponseBytes bounds a decoded provider response; real ones are a few
	// kilobytes.
	maxResponseBytes = 1 << 20

	// Timeout bounds the whole two-request exchange. It is generous because a
	// caller who has already confirmed would rather wait than be told to press
	// the button again — and pressing it again is the one retry that can cost a
	// second credit.
	Timeout = 30 * time.Second
)

// Outcomes reported to the caller. These are this plugin's vocabulary, not the
// provider's: the provider's own codes are mapped onto them so a new code
// upstream surfaces as a plain failure rather than as an unrecognized success.
const (
	// OutcomeReset is the only outcome that spent a credit.
	OutcomeReset = "reset"
	// OutcomeNothingToReset means the provider accepted the request but no
	// window needed clearing. The credit is still gone: the provider consumes
	// it on acceptance, and saying otherwise would be a lie the operator acts
	// on.
	OutcomeNothingToReset = "nothingToReset"
	// OutcomeNoCredit means there was nothing to spend by the time the request
	// landed — usually a count read from a snapshot that has since been spent
	// elsewhere.
	OutcomeNoCredit = "noCredit"
	// OutcomeFailed is every other ending, including a refusal by the provider
	// and a response this code does not understand.
	OutcomeFailed = "failed"
)

// Errors a caller turns into a status code. Every one of them is a fixed
// string: provider error text is never forwarded, because it is unbounded input
// that would land in a dashboard and a log.
var (
	// ErrNotCodex covers a credential on any other provider. Banked resets are
	// a Codex concept and there is nothing to spend anywhere else.
	ErrNotCodex = errors.New("banked resets exist only on Codex credentials")
	// ErrNoAccessToken and ErrNoAccountID mean the credential cannot
	// authenticate one request — an API-key login has no reset credits at all.
	ErrNoAccessToken = errors.New("credential has no usable Codex access token")
	ErrNoAccountID   = errors.New("credential has no resolvable ChatGPT account id")
	ErrUnavailable   = errors.New("the provider could not be reached")
	ErrRefused       = errors.New("the provider refused the request")
	// ErrInFlight means a redemption against this credential is already under
	// way. Two presses that both reach the provider can spend two credits, and
	// the second one was never intended: a double-click, an impatient retry, a
	// second tab. Refusing the second is the only guard that holds, because the
	// first may already have passed its inventory check.
	ErrInFlight = errors.New("a redemption is already under way for this credential")
)

// Host is the narrow pair of callbacks this package needs. It is an interface
// so the plugin can withhold both when redemption is switched off: a nil Host
// is a redeemer that cannot make a request at all, which is a stronger
// guarantee than a flag consulted at the top of a function.
type Host interface {
	GetAuth(context.Context, string) ([]byte, error)
	HTTPDo(context.Context, protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error)
}

// Result is one redemption attempt that reached the provider.
type Result struct {
	Outcome string
	// WindowsReset is what the provider says it cleared. Zero is normal on
	// OutcomeNothingToReset and is not by itself a failure.
	WindowsReset int
	// RemainingCount is how many credits the account still holds, counted from
	// the inventory read at the start of this exchange minus the one just
	// spent. It is a floor rather than a fresh reading: it costs no third
	// request, and the next quota-cache poll replaces it with the truth.
	RemainingCount int
}

// Redeemer spends credits for one CPA instance.
type Redeemer struct {
	host Host

	// inFlight holds the credentials with a redemption under way, so a second
	// press cannot start a second exchange against the same account while the
	// first is still between its inventory read and its POST.
	mu       sync.Mutex
	inFlight map[string]bool
}

func New(host Host) *Redeemer { return &Redeemer{host: host, inFlight: map[string]bool{}} }

// claim reserves the credential, or reports that it is already reserved.
func (r *Redeemer) claim(authIndex string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inFlight[authIndex] {
		return false
	}
	if r.inFlight == nil {
		r.inFlight = map[string]bool{}
	}
	r.inFlight[authIndex] = true
	return true
}

func (r *Redeemer) release(authIndex string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.inFlight, authIndex)
}

// Enabled reports whether a redemption could be attempted at all.
func (r *Redeemer) Enabled() bool { return r != nil && r.host != nil }

// Redeem spends one credit against the named credential.
//
// The order is deliberate. The inventory is read first so the credit being
// spent is one the provider currently calls available, rather than one a
// snapshot claimed minutes ago: a POST built from stale evidence is how a
// second credit gets spent on an account that had one. If nothing is available,
// no POST is made at all.
func (r *Redeemer) Redeem(ctx context.Context, provider, authIndex string) (Result, error) {
	if !r.Enabled() {
		return Result{}, ErrUnavailable
	}
	if strings.ToLower(strings.TrimSpace(provider)) != "codex" {
		return Result{}, ErrNotCodex
	}
	if !r.claim(authIndex) {
		return Result{}, ErrInFlight
	}
	defer r.release(authIndex)
	creds, err := credentialsOf(ctx, r.host, authIndex)
	if err != nil {
		return Result{}, err
	}

	available, err := r.available(ctx, creds)
	if err != nil {
		return Result{}, err
	}
	if len(available) == 0 {
		return Result{Outcome: OutcomeNoCredit}, nil
	}

	// A fresh idempotency key per attempt, as the provider's own clients send.
	// It is generated here and never reused: replaying one is the provider's
	// business, and reusing one across two genuine presses would silently drop
	// the second.
	requestID, err := requestID()
	if err != nil {
		return Result{}, ErrUnavailable
	}
	body, err := json.Marshal(map[string]string{"credit_id": available[0], "redeem_request_id": requestID})
	if err != nil {
		return Result{}, ErrUnavailable
	}
	response, err := r.host.HTTPDo(ctx, request("POST", consumeURL, creds, body))
	if err != nil {
		return Result{}, ErrUnavailable
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Result{}, ErrRefused
	}
	root, err := decode(response.Body)
	if err != nil {
		// The provider accepted it. Whatever the body said, the credit is
		// gone — reporting this as a failure would invite a second press.
		return Result{Outcome: OutcomeReset, RemainingCount: len(available) - 1}, nil
	}
	return Result{
		Outcome:        outcomeOf(stringOf(root, "code")),
		WindowsReset:   intOf(root, "windows_reset", "windowsReset"),
		RemainingCount: len(available) - 1,
	}, nil
}

// outcomeOf maps the provider's code onto this package's vocabulary.
//
// An unrecognized code is treated as a successful reset rather than as a
// failure, because control only reaches here on a 2xx: the credit has been
// spent, and the only question left is what to print. Calling that a failure
// would be the one mistake that costs a second credit.
func outcomeOf(code string) string {
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

// available lists the ids of credits the provider currently calls spendable,
// soonest expiry first, so the credit closest to lapsing is the one spent.
func (r *Redeemer) available(ctx context.Context, creds credentials) ([]string, error) {
	response, err := r.host.HTTPDo(ctx, request("GET", creditsURL, creds, nil))
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

// credentials is the pair of values one Codex request needs. Nothing else is
// kept from the credential document, and this value never leaves the package.
type credentials struct {
	accessToken string
	accountID   string
}

// credentialsOf decodes only the two fields a request needs out of the physical
// credential document. The document holds OAuth tokens in full and is never
// logged, persisted, rendered, or returned.
func credentialsOf(ctx context.Context, host Host, authIndex string) (credentials, error) {
	raw, err := host.GetAuth(ctx, authIndex)
	if err != nil {
		return credentials{}, ErrUnavailable
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
		return credentials{}, ErrNoAccessToken
	}
	out := credentials{accessToken: doc.AccessToken, accountID: doc.AccountID}
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
		return credentials{}, ErrNoAccessToken
	}
	if out.accountID == "" {
		out.accountID = accountIDFromIDToken(idToken)
	}
	if out.accountID == "" {
		return credentials{}, ErrNoAccountID
	}
	return out, nil
}

func request(method, url string, creds credentials, body []byte) protocol.HostHTTPRequest {
	headers := map[string][]string{
		"Authorization":      {"Bearer " + creds.accessToken},
		"Accept":             {"application/json"},
		"User-Agent":         {userAgent},
		"Chatgpt-Account-Id": {creds.accountID},
	}
	if body != nil {
		headers["Content-Type"] = []string{"application/json"}
	}
	return protocol.HostHTTPRequest{Method: method, URL: url, Headers: headers, Body: body}
}

func decode(body []byte) (map[string]any, error) {
	if len(body) > maxResponseBytes {
		return nil, errors.New("response too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil || root == nil {
		return nil, errors.New("unreadable response")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("unreadable response")
	}
	return root, nil
}

func stringOf(obj map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := obj[key].(string); ok {
			return value
		}
	}
	return ""
}

func intOf(obj map[string]any, keys ...string) int {
	for _, key := range keys {
		if value, ok := obj[key].(json.Number); ok {
			if n, err := value.Int64(); err == nil && n >= 0 && n < 1<<20 {
				return int(n)
			}
		}
	}
	return 0
}

// requestID is a random 128-bit value in the hyphenated form the provider's own
// clients send. It identifies one attempt and carries nothing about the caller.
func requestID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	// Version 4, variant 1, so a server that validates the shape accepts it.
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	hexed := hex.EncodeToString(raw)
	return strings.Join([]string{hexed[:8], hexed[8:12], hexed[12:16], hexed[16:20], hexed[20:]}, "-"), nil
}

// sortStable is an insertion sort over a handful of credits, kept here so the
// comparison reads beside the rule it implements.
func sortStable[T any](items []T, less func(a, b T) bool) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && less(items[j], items[j-1]); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}
