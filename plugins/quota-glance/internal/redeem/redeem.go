// Package redeem spends one banked rate-limit reset: a Codex rate-limit reset
// credit, or a Claude reset grant.
//
// It is the only part of quota-glance that contacts a provider, and it runs
// only when the operator presses the button and confirms. Nothing on a timer,
// nothing on a rebuild, and nothing on a route that merely reads reaches this
// code. quota-cache still owns every scheduled request; this is a write, made
// once, on a person's instruction.
//
// The action is irreversible. A banked reset is consumed the moment the
// provider accepts the request — there is no unspend — so every guard here is
// about not making that request by accident: the credential must be on a
// provider this package knows, it must hold a reset the provider currently calls
// spendable, and the caller must have confirmed. The confirmation itself lives
// in the browser; what this package guarantees is that a request which arrives
// without one is refused.
//
// The other half of the job is the answer that never arrives. A spend request
// that times out, or comes back in a shape this code cannot read, may or may
// not have spent the reset, and the next press must not be able to spend a
// second one on the strength of not knowing. That is the journal in
// journal.go: the press after an unknown outcome repeats the same claim, with
// the same ids, rather than making a new one.
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
	providerCodex  = "codex"
	providerClaude = "claude"

	// maxResponseBytes bounds a decoded provider response; real ones are a few
	// kilobytes.
	maxResponseBytes = 1 << 20

	// Timeout bounds the whole exchange, and is what the API gives a press. It
	// is generous because a caller who has already confirmed would rather wait
	// than be told the outcome is unknown — and it is the longest exchange,
	// Claude's, that sets it: two reads at readTimeout and one spend at
	// spendTimeout is 49 seconds. Reading the credential is an in-process call
	// to CPA and takes none of that in practice; if the reads ever do run so
	// long that the spend could not have its whole bound, the spend is not
	// sent at all (see spend).
	//
	// CPA puts no deadline of its own on a plugin's management route; the
	// browser does, at 65 seconds, and a reverse proxy in front of CPA commonly
	// does at 60. This bound sits under both, so the plugin always answers
	// before anything in front of it gives up and turns a clean answer into a
	// dropped connection.
	Timeout = 55 * time.Second

	// readTimeout bounds each request that only reads: Codex's inventory,
	// Claude's profile and grant status. The CPA console uses the same bound
	// for the same reads.
	readTimeout = 12 * time.Second

	// spendTimeout bounds the one request that can spend. 25 seconds is the
	// bound Claude Code itself puts on the claim. The spend request is only
	// ever sent with this much of the overall deadline left, so it is never cut
	// short by the exchange around it.
	spendTimeout = 25 * time.Second
)

// Outcomes reported to the caller. These are this plugin's vocabulary, not the
// provider's: the provider's own codes are mapped onto them so a new code
// upstream surfaces as something this plugin chose to say, never as raw text.
const (
	// OutcomeReset is a reset spent and windows cleared.
	OutcomeReset = "reset"
	// OutcomeNothingToReset means the provider accepted the request but no
	// window needed clearing. The credit is still gone: Codex consumes it on
	// acceptance, and saying otherwise would be a lie the operator acts on.
	OutcomeNothingToReset = "nothingToReset"
	// OutcomeNoCredit means there was nothing to spend by the time the request
	// landed — usually a count read from a snapshot that has since been spent
	// elsewhere, or a Claude grant the provider will not let be used right now
	// for a reason it does not name. Nothing was consumed.
	OutcomeNoCredit = "noCredit"
	// OutcomeFailed is a refusal this vocabulary has no better word for —
	// today, Claude saying resets are unavailable. Nothing was consumed.
	OutcomeFailed = "failed"
	// OutcomeNotLimited, OutcomeCooldown, OutcomePaused and OutcomeIneligible
	// are Claude's reasons for not spending, found either in the grant status
	// read before the claim or in the claim's own answer. Nothing was consumed
	// in any of them. Claude spends a grant only on an account that is at a
	// limit, not within a cooldown after the last one, on a grant that is not
	// paused, for an account that is eligible at all.
	OutcomeNotLimited = "notLimited"
	OutcomeCooldown   = "cooldown"
	OutcomePaused     = "paused"
	OutcomeIneligible = "ineligible"
	// OutcomeAlreadyUsed means the provider says the reset this claim names
	// was already consumed. On the press after an unknown outcome, that is the
	// earlier attempt having gone through.
	OutcomeAlreadyUsed = "alreadyUsed"
)

// Errors a caller turns into a status code. Every one of them is a fixed
// string: provider error text is never forwarded, because it is unbounded input
// that would land in a dashboard and a log.
var (
	// ErrUnsupportedProvider covers a credential on any provider other than
	// Codex and Claude. Banked resets exist nowhere else.
	ErrUnsupportedProvider = errors.New("banked resets exist only on Codex and Claude credentials")
	// ErrNoAccessToken and ErrNoAccountID mean the credential cannot
	// authenticate one request — an API-key login has no banked resets at all.
	ErrNoAccessToken = errors.New("credential has no usable access token")
	ErrNoAccountID   = errors.New("credential has no resolvable ChatGPT account id")
	// ErrUnavailable is a read that never reached the provider or never came
	// back. Nothing was spent: no spend request was sent.
	ErrUnavailable = errors.New("the provider could not be reached")
	// ErrRefused is the provider turning a request away — a read, or a spend
	// refused on authentication, which is checked before anything is spent.
	ErrRefused = errors.New("the provider refused the request")
	// ErrRateLimited is the provider throttling this account. A throttled
	// request is turned away before it is acted on, so nothing was spent.
	ErrRateLimited = errors.New("the provider is rate limiting this account")
	// ErrInFlight means a redemption against this credential is already under
	// way. Two presses that both reach the provider can spend two credits, and
	// the second one was never intended: a double-click, an impatient retry, a
	// second tab. Refusing the second is the only guard that holds, because the
	// first may already have passed its inventory check.
	ErrInFlight = errors.New("a redemption is already under way for this credential")
	// ErrOutcomeUnknown means a spend request was sent and no answer this code
	// can trust came back: the reset may have been spent. Pressing again before
	// the claim's retry window closes repeats the same claim, which cannot spend
	// a second. Redeem returns it as an *OutcomeUnknownError, which names that
	// instant.
	ErrOutcomeUnknown = errors.New("the provider may have spent the reset; the outcome is unknown")
	// ErrRetryWindowClosed means an unresolved claim's retry window closed
	// before this press could settle it. Whether that claim spent a reset is
	// still unknown, and it is no longer on record: the next press makes a new
	// claim, which spends a second reset if the first one went through.
	ErrRetryWindowClosed = errors.New("the outcome of an earlier claim is unknown and it can no longer be repeated")
	// ErrIdentityChanged means the credential now authenticates as a different
	// account than the one an unresolved claim was made for. Repeating that
	// claim against another account would be a different claim, so nothing is
	// sent until the retry window closes.
	ErrIdentityChanged = errors.New("the credential belongs to a different account than the unresolved claim")
)

// OutcomeUnknownError is ErrOutcomeUnknown together with the deadline that
// makes pressing again safe.
//
// The deadline is the claim's, fixed when the claim was first made, and no
// answer moves it: the provider matches a repeated request id against the
// original request, not against the latest copy. So every unknown answer names
// the same instant, and a reader told it can see how much of the window is
// left, where "ten minutes" restated on the third answer would promise minutes
// the claim no longer has.
type OutcomeUnknownError struct {
	// RetryUntil is when the claim stops being repeated. A press that starts
	// before it repeats the claim; one that has not sent the repeat by then
	// sends nothing and is answered ErrRetryWindowClosed.
	RetryUntil time.Time
}

func (e *OutcomeUnknownError) Error() string { return ErrOutcomeUnknown.Error() }

// Unwrap lets errors.Is(err, ErrOutcomeUnknown) hold, so a caller that needs
// only the kind of failure does not need the type.
func (e *OutcomeUnknownError) Unwrap() error { return ErrOutcomeUnknown }

// Host is the narrow pair of callbacks this package needs. It is an interface
// so the plugin can withhold both when redemption is switched off: a nil Host
// is a redeemer that cannot make a request at all, which is a stronger
// guarantee than a flag consulted at the top of a function.
type Host interface {
	GetAuth(context.Context, string) ([]byte, error)
	HTTPDo(context.Context, protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error)
}

// Result is one redemption attempt that ended in an answer.
type Result struct {
	Outcome string
	// WindowsReset is what the provider says it cleared. Zero is normal on
	// OutcomeNothingToReset and on every outcome that spent nothing.
	WindowsReset int
	// RemainingCount is how many resets the account still holds, counted from
	// the inventory read at the start of this exchange, less the one just
	// spent when one was. It is a floor rather than a fresh reading: it costs
	// no further request, and the next quota-cache poll replaces it with the
	// truth.
	RemainingCount int
}

// Redeemer spends banked resets for one CPA instance. One lives for the life of
// the plugin, so its journal survives a reconfiguration: a claim left unknown a
// minute before the operator saved a setting is still the claim the next press
// repeats.
type Redeemer struct {
	host Host
	// now is the clock the retry window is measured on. It is a field so a
	// test can step past the window without waiting ten minutes.
	now func() time.Time

	mu sync.Mutex
	// inFlight holds the credentials with a redemption under way, so a second
	// press cannot start a second exchange against the same account while the
	// first is still between its inventory read and its spend.
	inFlight map[string]bool
	// pending is the journal: per credential, the spend request whose answer
	// never arrived. See journal.go.
	pending map[string]pendingClaim
}

func New(host Host) *Redeemer {
	return &Redeemer{host: host, now: time.Now, inFlight: map[string]bool{}, pending: map[string]pendingClaim{}}
}

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

// Redeem spends one reset against the named credential.
//
// The order is deliberate. The provider is asked what is spendable first, so
// the reset being spent is one the provider currently calls available rather
// than one a snapshot claimed minutes ago: a request built from stale evidence
// is how a second reset gets spent on an account that had one. If nothing is
// spendable, no spend request is made at all.
//
// The one exception is the press after an unknown outcome. That press repeats
// the earlier claim exactly, and skips the check, because the check would see
// the reset the earlier claim may have spent as gone and could pick another.
func (r *Redeemer) Redeem(ctx context.Context, provider, authIndex string) (Result, error) {
	if !r.Enabled() {
		return Result{}, ErrUnavailable
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider != providerCodex && provider != providerClaude {
		return Result{}, ErrUnsupportedProvider
	}
	if !r.claim(authIndex) {
		return Result{}, ErrInFlight
	}
	defer r.release(authIndex)
	retry, isRetry := r.unresolved(authIndex, provider)
	if provider == providerClaude {
		return r.redeemClaude(ctx, authIndex, retry, isRetry)
	}
	return r.redeemCodex(ctx, authIndex, retry, isRetry)
}

// answer is what one spend request's response proves.
type answer struct {
	kind   answerKind
	result Result
	err    error
}

type answerKind int

const (
	// answerUnknown proves nothing: the reset may or may not have been spent.
	answerUnknown answerKind = iota
	// answerSettled is final whichever attempt it answers — a spend, or the
	// provider saying the claimed reset is already gone.
	answerSettled
	// answerRefused proves this request spent nothing, and proves nothing
	// about any request before it.
	answerRefused
)

// spend sends the one request that can consume a reset and reads its answer.
//
// The journal entry is written before the request leaves, and removed only on
// an answer that settles the claim. Everything else leaves it in place, so the
// next press within the retry window repeats this claim rather than making a
// new one.
//
// On a repeat, a refusal is reported as unknown rather than as the refusal. A
// claim refused now says nothing about whether the same claim went through a
// minute ago — Claude refuses a second reset inside its cooldown, which is
// exactly what it would say if the first one had worked.
func (r *Redeemer) spend(ctx context.Context, authIndex string, claim pendingClaim, isRetry bool,
	send func(context.Context) (protocol.HostHTTPResponse, error),
	interpret func(protocol.HostHTTPResponse) answer,
) (Result, error) {
	if isRetry && !r.withinWindow(claim) {
		// The window closed during the reads before this point. A request id
		// replayed after the provider has stopped matching it is a new claim
		// wearing an old name, so it is not sent; the next press starts fresh,
		// and the answer says so rather than promising another repeat.
		r.forget(authIndex)
		return Result{}, ErrRetryWindowClosed
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < spendTimeout {
		// Not enough of the exchange's deadline left to give the spend its
		// whole bound. Sending it anyway would turn a slow answer into an
		// unknown one, so it is not sent; nothing has been spent by this press.
		if isRetry {
			return Result{}, r.stillUnknown(authIndex, claim)
		}
		return Result{}, ErrUnavailable
	}
	if !isRetry {
		r.remember(authIndex, claim)
	}
	spendCtx, cancel := context.WithTimeout(ctx, spendTimeout)
	defer cancel()
	response, err := send(spendCtx)
	if err != nil {
		// A transport failure or a timeout after the request may have left. The
		// provider may have acted on it; nothing here can tell.
		return Result{}, r.stillUnknown(authIndex, claim)
	}
	got := interpret(response)
	switch {
	case got.kind == answerSettled, got.kind == answerRefused && !isRetry:
		r.forget(authIndex)
		return got.result, got.err
	default:
		return Result{}, r.stillUnknown(authIndex, claim)
	}
}

// stillUnknown is the answer to a press that leaves claim unresolved.
//
// Inside the claim's window it names the instant the window closes, so the
// reader is told exactly how long pressing again stays a repeat. Once the
// window has closed — during this press's reads, or while its repeat was in
// flight — the claim is dropped and the answer is ErrRetryWindowClosed
// instead: the next press is a fresh claim, and an answer still promising a
// repeat would be how a reader is talked into spending a second reset.
func (r *Redeemer) stillUnknown(authIndex string, claim pendingClaim) error {
	if !r.withinWindow(claim) {
		r.forget(authIndex)
		return ErrRetryWindowClosed
	}
	return &OutcomeUnknownError{RetryUntil: claim.createdAt.Add(retryWindow)}
}

// read sends one request that only reads, under its own bound.
func (r *Redeemer) read(ctx context.Context, request protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	return r.host.HTTPDo(ctx, request)
}

// authDocument fetches the physical credential document. It holds OAuth tokens
// in full: callers decode the one or two fields a request needs and drop the
// rest, and it is never logged, persisted, rendered, or returned.
func (r *Redeemer) authDocument(ctx context.Context, authIndex string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	raw, err := r.host.GetAuth(ctx, authIndex)
	if err != nil {
		return nil, ErrUnavailable
	}
	return raw, nil
}

// readError maps a read's status onto this package's errors. A throttled read
// is said to be one, because it means "try later" and not "this will never
// work".
func readError(status int) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == 429:
		return ErrRateLimited
	default:
		return ErrRefused
	}
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
		if n, ok := countOf(obj[key]); ok {
			return n
		}
	}
	return 0
}

// countOf reads a non-negative whole number, bounded so a hostile value cannot
// overflow arithmetic done with it. 3.0 counts as 3; 2.5 is not a count.
func countOf(value any) (int, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	if n, err := number.Int64(); err == nil {
		if n >= 0 && n < 1<<20 {
			return int(n), true
		}
		return 0, false
	}
	f, err := number.Float64()
	if err != nil || f < 0 || f >= 1<<20 || f != float64(int64(f)) {
		return 0, false
	}
	return int(f), true
}

// requestID is a random 128-bit value in the hyphenated form the providers' own
// clients send. It identifies one claim and carries nothing about the caller.
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

// sortStable is an insertion sort over a handful of entries, kept here so the
// comparison reads beside the rule it implements.
func sortStable[T any](items []T, less func(a, b T) bool) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && less(items[j], items[j-1]); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}
