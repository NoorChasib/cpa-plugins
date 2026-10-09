// Package api serves the built document. It holds no file handle, no watcher,
// and no clock of its own: the runtime publishes a finished document and this
// package hands it out.
//
// That separation is enforced by a test — api must not import the source or
// watch packages — so a request can never trigger a read, and a slow or broken
// filesystem can never turn into a slow response.
package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/aggregate"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/redeem"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/web"
)

const (
	// failureLimit bounds FAILED authentication attempts per window, across all
	// callers. It slows a brute force against the token; it is not a ban.
	failureLimit = 20
	rateWindow   = time.Minute
)

// WatcherState mirrors the watcher's reported state. It is redeclared here
// rather than imported so this package keeps no dependency on the watcher.
type WatcherState struct {
	Watching  bool      `json:"watching"`
	Directory string    `json:"directory"`
	LastEvent time.Time `json:"last_event"`
	LastError string    `json:"last_error,omitempty"`
	Reloads   uint64    `json:"reloads"`
	Backstops uint64    `json:"backstops"`
	// Heartbeats counts rebuilds the timer ran with nothing to reload, which is
	// how request activity stays current between snapshot writes.
	Heartbeats uint64 `json:"heartbeats"`
}

// Health is what the management health route reports.
type Health struct {
	Version             string         `json:"version"`
	CachePath           string         `json:"cache_path"`
	StaleAfter          string         `json:"stale_after"`
	SnapshotWrittenAt   time.Time      `json:"snapshot_written_at"`
	SnapshotNextRequest time.Time      `json:"snapshot_next_request"`
	BuiltAt             time.Time      `json:"built_at"`
	Stale               bool           `json:"stale"`
	StaleReason         string         `json:"stale_reason,omitempty"`
	LastError           string         `json:"last_error,omitempty"`
	Watcher             WatcherState   `json:"watcher"`
	Settings            SettingsHealth `json:"settings"`
	Meter               MeterHealth    `json:"meter"`
}

// SettingsHealth is settings.json as the last rebuild found it. LastError is
// "" or "unreadable": a file that fails a shape rule, which nothing is applied
// from and nothing is saved over until it is moved aside.
type SettingsHealth struct {
	Path             string `json:"path"`
	Revision         uint64 `json:"revision"`
	APICreditEntries int    `json:"api_credit_entries"`
	RenewalEntries   int    `json:"renewal_entries"`
	LastError        string `json:"last_error"`
}

// MeterHealth is quota-cache's API meter as the last rebuild read it.
// LastError is "", "missing" or "unreadable".
type MeterHealth struct {
	Path      string    `json:"path"`
	FlushedAt time.Time `json:"flushed_at"`
	LastError string    `json:"last_error"`
}

type API struct {
	pluginID string

	mu     sync.RWMutex
	body   []byte
	etag   string
	doc    aggregate.Document
	health Health

	tokenHash [sha256.Size]byte
	hasToken  bool
	disabled  bool
	limiter   *limiter

	// redeemer is nil unless redemption is configured on. Holding the capability
	// rather than a flag means a disabled plugin has no way to make the request
	// at all, instead of a branch that could be got wrong.
	redeemer Redeemer
	// ledger remembers how each press was answered, so a second copy of one
	// press is told the same thing instead of spending again. See ledger.go.
	ledger *ledger
	// saver is nil unless allow-edit is on, which closes both settings doors
	// the way a nil redeemer closes the redeem ones. See settings.go.
	saver Saver
}

// Redeemer is the one action this API can take on the world. It is an interface
// so the serving layer keeps no dependency on the provider client, and so a
// test can assert what was and was not attempted.
type Redeemer interface {
	Redeem(ctx context.Context, provider, credentialID string) (redeem.Result, error)
}

// SetRedeemer installs, or removes, the ability to spend a banked reset. Nil
// closes the route: it 404s exactly as it did before the feature existed.
func (a *API) SetRedeemer(r Redeemer) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.redeemer = r
}

// Disable and Enable close and reopen every route. A plugin turned off in
// configuration that kept serving its last document would be indistinguishable
// from one that is still running.
func (a *API) Disable() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.disabled = true
}

func (a *API) Enable() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.disabled = false
}

func New(pluginID, token string) *API {
	a := &API{pluginID: pluginID, limiter: newLimiter(), ledger: newLedger()}
	a.SetToken(token)
	// Serve a valid, honest document before the first build completes rather
	// than a null body.
	a.Publish(aggregate.Document{
		SchemaVersion: aggregate.SchemaVersion,
		Credentials:   []aggregate.Credential{},
		Providers:     []aggregate.Provider{},
	}, Health{})
	return a
}

// NewToken returns a fresh web token. The caller logs it exactly once, when the
// configuration left it empty; it is never logged again and never stored here
// in recoverable form.
func NewToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// SetToken stores only a digest. An empty token leaves the summary route
// closed rather than open: until one is configured there is nothing to
// authenticate against, and the alternative is a route that a blank credential
// satisfies.
func (a *API) SetToken(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tokenHash, a.hasToken = sha256.Sum256([]byte(token)), token != ""
}

// Publish replaces the served document.
func (a *API) Publish(doc aggregate.Document, health Health) {
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		// Keep serving the previous document, but say so: silently serving
		// stale data while health reports the older build is worse than either.
		a.mu.Lock()
		a.health = health
		a.health.LastError = "summary document could not be encoded"
		a.mu.Unlock()
		return
	}
	body = append(body, '\n')
	sum := sha256.Sum256(body)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.body, a.etag, a.doc, a.health = body, `"`+hex.EncodeToString(sum[:])+`"`, doc, health
}

// Document returns the currently served document.
func (a *API) Document() aggregate.Document {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.doc
}

func (a *API) resourcePath(suffix string) string {
	return "/v0/resource/plugins/" + a.pluginID + suffix
}

func (a *API) managementPath(suffix string) string {
	return "/v0/management/plugins/" + a.pluginID + suffix
}

// Handle dispatches one CPA management/resource request.
//
// Resource paths are matched exactly. CPA does no prefix matching on them and
// neither does this: an unexpected path is a 404, not a best guess.
func (a *API) Handle(req protocol.ManagementRequest, now time.Time) protocol.ManagementResponse {
	a.mu.RLock()
	disabled := a.disabled
	a.mu.RUnlock()
	if disabled {
		return jsonResponse(http.StatusServiceUnavailable, map[string]string{"error": "disabled"})
	}
	// Four paths write. /redeem accepts only POST, on both trees, and so does
	// /settings, on the management tree only. /spend and /save-settings accept
	// only GET, on the resource tree only, because CPA dispatches nothing else
	// there (pluginhost/management.go:295-297). They act only with the web
	// token and a body carried in a header. No path both reads and writes.
	if req.Method == http.MethodPost {
		switch req.Path {
		case a.managementPath("/redeem"):
			return a.redeemResponse(req, now, false)
		case a.resourcePath("/redeem"):
			return a.redeemResponse(req, now, true)
		case a.managementPath("/settings"):
			return a.settingsResponse(req, now)
		}
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "not_found"})
	}
	if req.Method != http.MethodGet {
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "not_found"})
	}
	switch req.Path {
	case a.resourcePath("/app"):
		return a.appResponse(req)
	// The same document down two paths, because a reader can arrive two ways.
	//
	// From the console sidebar the browser already holds a CPA session, and the
	// page spends it here: CPA's management middleware has required the full
	// management key before this line runs, so there is nothing further to
	// check. That is the path that needs no sign-in.
	//
	// Opened anywhere else — a phone, a bookmark, a browser that has never seen
	// the console — there is no session to spend. CPA authenticates nothing on
	// a resource route, so that path carries this plugin's own token and this
	// plugin checks it. It is the fallback, and it is why the token still
	// exists.
	case a.managementPath("/summary"):
		return a.documentResponse(req)
	case a.resourcePath("/summary"):
		return a.tokenSummaryResponse(req, now)
	case a.resourcePath("/spend"):
		return a.spendResponse(req, now)
	case a.resourcePath("/save-settings"):
		return a.saveSettingsResponse(req, now)
	case a.managementPath("/health"):
		return a.healthResponse()
	case a.managementPath("/windows"):
		return a.windowsResponse()
	}
	return jsonResponse(http.StatusNotFound, map[string]string{"error": "not_found"})
}

// redeemResponse is the POST door to a press, on either tree. Between them,
// it and spendResponse are the only routes on this plugin that change anything
// anywhere, and both end in spend.
//
// Three things must hold before the provider is contacted, and each of them is
// checked rather than trusted from the caller:
//
//  1. The caller is authenticated, by whichever of the two doors it arrived at.
//  2. The body carries an explicit confirmation. The dialog lives in the
//     browser, where it belongs, but a request that arrives without one — a
//     stray fetch, a replayed URL, a script — must not spend a credit merely
//     because it was well formed.
//  3. The served document says this credential has a redeemable credit. That is
//     what stops a POST naming an arbitrary credential from turning into a
//     provider request, and it is why the check reads the document rather than
//     asking the provider. spend makes this check.
//
// A press id is optional here. With one, a second copy of the same press is
// answered from the ledger rather than spent; without one, the route behaves
// exactly as it did before press ids existed, so a page cached from an older
// release keeps working.
func (a *API) redeemResponse(req protocol.ManagementRequest, now time.Time, viaToken bool) protocol.ManagementResponse {
	if viaToken && !a.authorized(req.Headers) {
		return a.tokenRefusal(now)
	}

	a.mu.RLock()
	redeemer := a.redeemer
	a.mu.RUnlock()
	if redeemer == nil {
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "not_found"})
	}

	// JSON only. A form-encoded body is the one a cross-site form can send
	// without the browser asking permission first; requiring JSON means any
	// request that gets here had to be made by script on this origin, on top of
	// the credential it already had to present in a header.
	if media := req.Headers.Get("Content-Type"); media != "" {
		if base, _, _ := strings.Cut(media, ";"); !strings.EqualFold(strings.TrimSpace(base), "application/json") {
			return jsonResponse(http.StatusUnsupportedMediaType, map[string]string{"error": "unsupported_media_type"})
		}
	}
	if len(req.Body) > maxPressBytes {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	p, code := parsePress(req.Body)
	if code != "" {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": code})
	}
	return a.spend(redeemer, p)
}

// spendResponse is the GET door to a press, for a reader signed in with the
// web token rather than a CPA console session.
//
// It is a GET only because CPA dispatches nothing else to a resource route
// (pluginhost/management.go:295-297), and a resource route is the only place a
// reader without the management key can reach this plugin at all. That makes
// it a GET that acts, which is everything a GET is not supposed to be, so the
// action is fenced off from everything that treats a GET as safe:
//
//   - Nothing selecting the action is in the URL. The credential, the
//     confirmation and the press id travel in one header, and the token in
//     another, so a link, a prefetch, a crawler or a cache that holds this URL
//     holds nothing that can spend.
//   - A browser that says this request did not come from a script on this page
//     — another site, a navigation, a prefetch — is refused before the token is
//     looked at. That costs the limiter nothing, because nothing was guessed.
//   - Early data is refused outright, because it can be replayed by anyone on
//     the path before the handshake proves who sent it.
//   - Every press carries a single-use id, and the ledger answers any second
//     copy of it with the first copy's answer, so a transport that resends a
//     GET it thinks is safe gets an answer and not a second spend.
//
// After that it is the same press as the POST: the same confirmation, the same
// document check, the same redeemer, and the same answer byte for byte.
func (a *API) spendResponse(req protocol.ManagementRequest, now time.Time) protocol.ManagementResponse {
	if crossSite(req.Headers) {
		return jsonResponse(http.StatusForbidden, map[string]string{"error": "cross_site"})
	}
	// RFC 8470 §5.1: any instance of the field, whatever its value, means the
	// request may have arrived as early data.
	if len(req.Headers.Values("Early-Data")) > 0 {
		return jsonResponse(http.StatusTooEarly, map[string]string{"error": "too_early"})
	}
	if !a.authorized(req.Headers) {
		return a.tokenRefusal(now)
	}

	a.mu.RLock()
	redeemer := a.redeemer
	a.mu.RUnlock()
	if redeemer == nil {
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "not_found"})
	}

	// The header is the POST body, base64url-encoded so any JSON survives as a
	// header value. Exactly one copy: two would leave it to whichever layer
	// reads them to decide which was meant.
	values := req.Headers.Values(spendHeader)
	if len(values) != 1 || len(values[0]) > maxPressBytes {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	raw, err := base64.RawURLEncoding.DecodeString(values[0])
	if err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	// An object and nothing else. JSON null decodes into a struct without
	// complaint, and would otherwise read as a press that merely forgot to
	// confirm.
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	p, code := parsePress(raw)
	if code == "" && p.id == "" {
		// Required on this door. It is what makes a resent GET harmless, and
		// this is the door where a resend is most likely.
		code = "invalid_request"
	}
	if code != "" {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": code})
	}
	return a.spend(redeemer, p)
}

const (
	// spendHeader carries the press on the GET door.
	spendHeader = "X-Quota-Glance-Spend"
	// replayedHeader marks an answer handed out from the ledger, so the page
	// can say the reset was spent by an earlier copy of the press.
	replayedHeader = "X-Quota-Glance-Replayed"
	// maxPressBytes bounds the POST body and the encoded GET header alike.
	maxPressBytes = 4096
)

// pressIDPattern is the press id's shape: what the page draws is 22 base64url
// characters, and nothing shorter is random enough to be single-use.
var pressIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

// press is one confirmed request to spend a reset, from either door.
type press struct {
	credentialID string
	// id is empty only on the POST door, where it is optional.
	id string
}

// parsePress reads a press from JSON, returning the error code to answer with
// when it cannot. Both doors read the same body, so a page can send the same
// object down either.
func parsePress(raw []byte) (press, string) {
	var body struct {
		CredentialID string  `json:"credentialId"`
		Confirmed    bool    `json:"confirmed"`
		PressID      *string `json:"pressId"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return press{}, "invalid_request"
	}
	if !body.Confirmed {
		// Spending a credit is irreversible, so the absence of a confirmation
		// is refused rather than defaulted.
		return press{}, "confirmation_required"
	}
	p := press{credentialID: body.CredentialID}
	if body.PressID != nil {
		if !pressIDPattern.MatchString(*body.PressID) {
			return press{}, "invalid_request"
		}
		p.id = *body.PressID
	}
	return p, ""
}

// crossSite reports whether the browser says this request did not come from a
// script on this page. Each header may be absent — a browser on a plain-HTTP
// origin sends none of them, and neither does anything that is not a browser —
// but a present one must say same-origin, a script fetch, and no destination,
// and nothing may say it is a prefetch.
func crossSite(headers http.Header) bool {
	only := func(name string, allowed ...string) bool {
		for _, value := range headers.Values(name) {
			if !slices.Contains(allowed, strings.ToLower(strings.TrimSpace(value))) {
				return false
			}
		}
		return true
	}
	if !only("Sec-Fetch-Site", "same-origin") || !only("Sec-Fetch-Mode", "cors", "same-origin") || !only("Sec-Fetch-Dest", "empty") {
		return true
	}
	return len(headers.Values("Sec-Purpose")) > 0 || len(headers.Values("Purpose")) > 0
}

// spend carries out one press, from either door, against the redeemer the
// door found installed.
//
// A press with an id is looked up in the ledger before anything else, the
// document check included. A second copy of a press is owed the first copy's
// answer, and that answer does not change because the document has since
// stopped offering the account — which, after a press that spent its last
// reset, is exactly what happens.
func (a *API) spend(redeemer Redeemer, p press) protocol.ManagementResponse {
	if p.id == "" {
		return a.redeemOnce(redeemer, p.credentialID)
	}
	entry, fresh, err := a.ledger.begin(p.id, p.credentialID)
	switch {
	case errors.Is(err, errPressMismatch):
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	case err != nil:
		// The ledger is full of presses it may not forget yet. Nothing was
		// spent, and a press made once older ones age out is admitted.
		return jsonResponse(http.StatusConflict, map[string]string{"error": "already_in_flight"})
	case !fresh:
		if answer, ok := a.ledger.replay(entry); ok {
			return answer
		}
		return jsonResponse(http.StatusConflict, map[string]string{"error": "already_in_flight"})
	}
	var answer protocol.ManagementResponse
	defer func() {
		// Every path out of here records an answer, or the entry would stay in
		// flight for good: unevictable, and making its copies wait the full
		// minute. A status of zero is reached only by a panic in the redeemer,
		// which may have happened after the claim was sent, so the copies are
		// told the outcome is unknown rather than given an empty success.
		if answer.StatusCode == 0 {
			answer = jsonResponse(http.StatusBadGateway, map[string]any{"error": "outcome_unknown", "retryUntilEpoch": nil})
		}
		a.ledger.finish(entry, answer)
	}()
	answer = a.redeemOnce(redeemer, p.credentialID)
	return answer
}

// redeemOnce checks the document and asks the redeemer, once.
func (a *API) redeemOnce(redeemer Redeemer, credentialID string) protocol.ManagementResponse {
	credential, ok := redeemableCredential(a.Document(), credentialID)
	if !ok {
		return jsonResponse(http.StatusConflict, map[string]string{"error": "not_redeemable"})
	}

	// The redeemer's own bound, which CPA does not shorten: it imposes no
	// deadline on a plugin's management or resource route, and the browser
	// waits longer than this. See redeem.Timeout for why the number is what
	// it is.
	ctx, cancel := context.WithTimeout(context.Background(), redeem.Timeout)
	defer cancel()
	result, err := redeemer.Redeem(ctx, credential.Provider, credential.ID)
	if err != nil {
		return redeemError(err)
	}
	return jsonResponse(http.StatusOK, map[string]any{
		// Which provider answered, so the client can word the outcome in that
		// provider's terms — a Codex credit and a Claude grant are not the
		// same thing, and "cooldown" is only ever Claude's word.
		"provider":       credential.Provider,
		"outcome":        result.Outcome,
		"windowsReset":   result.WindowsReset,
		"remainingCount": result.RemainingCount,
		// Said here rather than inferred in the client: the count on the card
		// comes from quota-cache's snapshot and will not move until its next
		// poll, and a dashboard that silently disagreed with itself for ten
		// minutes is worse than one that says why.
		"snapshotPending": true,
	})
}

// redeemableCredential finds the credential the document says can be redeemed
// against. Reading the served document rather than the request is the point: a
// caller cannot nominate a credential the dashboard is not already offering.
func redeemableCredential(doc aggregate.Document, id string) (aggregate.Credential, bool) {
	if id == "" {
		return aggregate.Credential{}, false
	}
	for _, credential := range doc.Credentials {
		if credential.ID != id {
			continue
		}
		if credential.ResetCredits == nil || !credential.ResetCredits.Redeemable || credential.ResetCredits.AvailableCount < 1 {
			return aggregate.Credential{}, false
		}
		return credential, true
	}
	return aggregate.Credential{}, false
}

// redeemError maps a redemption failure onto a status and a fixed code.
//
// Provider error text is never forwarded. It is unbounded input that would land
// in a dashboard and a log, and none of it tells the operator anything the code
// below does not.
//
// The 502s split because the operator does something different after each.
// provider_unavailable and provider_refused mean nothing was spent and the
// press can simply be tried again later; provider_rate_limited says the same
// with a reason. outcome_unknown is the one that may have cost a reset, and it
// is the only one where pressing again is a different act: until
// retryUntilEpoch it repeats the same claim rather than making a new one. That
// instant is the claim's own, fixed when it was first made, so it is sent as an
// instant rather than as "ten minutes" a client would count from the answer.
// retry_window_closed is the same uncertainty with that window gone: the next
// press is a new claim.
func redeemError(err error) protocol.ManagementResponse {
	switch {
	case errors.Is(err, redeem.ErrInFlight):
		return jsonResponse(http.StatusConflict, map[string]string{"error": "already_in_flight"})
	case errors.Is(err, redeem.ErrUnsupportedProvider):
		return jsonResponse(http.StatusConflict, map[string]string{"error": "not_redeemable"})
	case errors.Is(err, redeem.ErrNoAccessToken), errors.Is(err, redeem.ErrNoAccountID),
		// The credential can no longer be used for the claim left unresolved
		// on it: it now signs in as a different account. Nothing was sent.
		errors.Is(err, redeem.ErrIdentityChanged):
		return jsonResponse(http.StatusConflict, map[string]string{"error": "credential_unusable"})
	case errors.Is(err, redeem.ErrRetryWindowClosed):
		return jsonResponse(http.StatusBadGateway, map[string]string{"error": "retry_window_closed"})
	case errors.Is(err, redeem.ErrOutcomeUnknown):
		// Null when the redeemer did not say, which a client must read as no
		// promise at all rather than as the usual window.
		var until any
		var unknown *redeem.OutcomeUnknownError
		if errors.As(err, &unknown) && !unknown.RetryUntil.IsZero() {
			until = unknown.RetryUntil.Unix()
		}
		return jsonResponse(http.StatusBadGateway, map[string]any{"error": "outcome_unknown", "retryUntilEpoch": until})
	case errors.Is(err, redeem.ErrRateLimited):
		return jsonResponse(http.StatusBadGateway, map[string]string{"error": "provider_rate_limited"})
	case errors.Is(err, redeem.ErrRefused):
		return jsonResponse(http.StatusBadGateway, map[string]string{"error": "provider_refused"})
	default:
		return jsonResponse(http.StatusBadGateway, map[string]string{"error": "provider_unavailable"})
	}
}

// tokenSummaryResponse is the public, plugin-authenticated path.
func (a *API) tokenSummaryResponse(req protocol.ManagementRequest, now time.Time) protocol.ManagementResponse {
	// Authenticate first, and never throttle a request that presents the right
	// token. That is what makes a lockout impossible: no volume of hostile
	// traffic can stop the operator reaching their own dashboard.
	if !a.authorized(req.Headers) {
		return a.tokenRefusal(now)
	}
	return a.documentResponse(req)
}

// tokenRefusal answers a request on the resource tree that did not present the
// web token. It is the only place a failure is counted, so every token door is
// throttled by the same limiter, and it is reached only after authentication
// failed: a request carrying the right token never gets here.
func (a *API) tokenRefusal(now time.Time) protocol.ManagementResponse {
	if !a.limiter.allowFailure(now) {
		return protocol.ManagementResponse{
			StatusCode: http.StatusTooManyRequests,
			Headers:    http.Header{"Retry-After": {"60"}, "Cache-Control": {"no-store"}},
		}
	}
	// Bare: no hint about whether the token was absent, malformed, or merely
	// wrong.
	return protocol.ManagementResponse{
		StatusCode: http.StatusUnauthorized,
		Headers:    http.Header{"Cache-Control": {"no-store"}},
	}
}

// documentResponse serves the document to a caller that is already authorized,
// by whichever of the two routes it arrived on.
func (a *API) documentResponse(req protocol.ManagementRequest) protocol.ManagementResponse {
	a.mu.RLock()
	body, etag := a.body, a.etag
	a.mu.RUnlock()
	headers := http.Header{
		"Content-Type":           {"application/json; charset=utf-8"},
		"Cache-Control":          {"no-store"},
		"Etag":                   {etag}, // canonical form; Header.Get canonicalizes
		"X-Content-Type-Options": {"nosniff"},
		"Referrer-Policy":        {"no-referrer"},
	}
	if matchesETag(req.Headers.Get("If-None-Match"), etag) {
		return protocol.ManagementResponse{StatusCode: http.StatusNotModified, Headers: headers}
	}
	return protocol.ManagementResponse{StatusCode: http.StatusOK, Headers: headers, Body: body}
}

// authorized compares in constant time. Only the SHA-256 of the configured
// token is held, and neither the token nor the presented value is ever logged.
func (a *API) authorized(headers http.Header) bool {
	presented := strings.TrimSpace(headers.Get("Authorization"))
	const prefix = "Bearer "
	if len(presented) <= len(prefix) || !strings.EqualFold(presented[:len(prefix)], prefix) {
		return false
	}
	// An empty value never authenticates, whatever the configured token is.
	// Without this, "Bearer " plus whitespace would trim to "" and match the
	// digest of an unset token.
	value := strings.TrimSpace(presented[len(prefix):])
	if value == "" {
		return false
	}
	sum := sha256.Sum256([]byte(value))
	a.mu.RLock()
	expected, configured := a.tokenHash, a.hasToken
	a.mu.RUnlock()
	if !configured {
		return false
	}
	return subtle.ConstantTimeCompare(sum[:], expected[:]) == 1
}

func matchesETag(header, etag string) bool {
	if header == "" || etag == "" {
		return false
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

func (a *API) healthResponse() protocol.ManagementResponse {
	a.mu.RLock()
	health, doc := a.health, a.doc
	a.mu.RUnlock()
	health.Stale = doc.Stale
	if doc.StaleReason != nil {
		health.StaleReason = *doc.StaleReason
	}
	return jsonResponse(http.StatusOK, struct {
		Health
		Counters         aggregate.Counters `json:"counters"`
		GeneratedAtEpoch int64              `json:"generated_at_epoch"`
	}{health, doc.Counters, doc.GeneratedAtEpoch})
}

// windowsResponse reports which window keys were observed and which credentials
// reported each, which is the quickest way to see a provider change its
// vocabulary underneath the canonical mapping.
func (a *API) windowsResponse() protocol.ManagementResponse {
	type window struct {
		RowID       string   `json:"row_id"`
		WindowKey   string   `json:"window_key"`
		Provider    string   `json:"provider"`
		Matched     bool     `json:"matched"`
		Credentials []string `json:"credentials"`
	}
	doc := a.Document()
	windows := []window{}
	for _, provider := range doc.Providers {
		for _, row := range provider.Rows {
			item := window{RowID: row.RowID, Provider: provider.ID, Matched: row.Matched, Credentials: []string{}}
			for _, entry := range row.Entries {
				item.WindowKey = entry.SourceWindowKey
				item.Credentials = append(item.Credentials, entry.CredentialID)
			}
			windows = append(windows, item)
		}
	}
	return jsonResponse(http.StatusOK, map[string]any{"windows": windows})
}

func jsonResponse(status int, value any) protocol.ManagementResponse {
	raw, _ := json.Marshal(value)
	return protocol.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"}},
		Body:       raw,
	}
}

// limiter throttles failed authentication attempts, globally.
//
// A per-client limiter is not implementable over this ABI: the request carries
// no peer address, only headers the caller supplies. A key taken from those is
// rotated trivially — which defeats the limit outright — and forged just as
// trivially as the operator's own address, which would lock them out of their
// own dashboard with unauthenticated traffic. It is also unbounded input, so
// keying a map on it retains whatever the caller sends.
//
// Counting failures globally removes the attacker-chosen key. Combined with
// authenticating before throttling, a correct token always gets through and a
// wrong one is slowed, which is the property the design actually asks for.
type limiter struct {
	mu    sync.Mutex
	start time.Time
	count int
}

func newLimiter() *limiter { return &limiter{} }

func (l *limiter) allowFailure(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.start.IsZero() || now.Sub(l.start) >= rateWindow || now.Before(l.start) {
		l.start, l.count = now, 1
		return true
	}
	l.count++
	return l.count <= failureLimit
}

// appResponse serves the application shell. It is deliberately unauthenticated
// and deliberately data-free: CPA runs no authentication on a resource route,
// so anything reachable here is reachable by anyone who can reach the origin.
func (a *API) appResponse(req protocol.ManagementRequest) protocol.ManagementResponse {
	headers := http.Header{
		"Content-Type": {"text/html; charset=utf-8"},
		// The shell is immutable for a given build, so a browser may revalidate
		// rather than re-download the whole inlined bundle on every navigation.
		"Cache-Control":           {"no-cache"},
		"Etag":                    {web.ETag},
		"X-Content-Type-Options":  {"nosniff"},
		"Referrer-Policy":         {"no-referrer"},
		"Content-Security-Policy": {web.ContentSecurityPolicy},
	}
	if matchesETag(req.Headers.Get("If-None-Match"), web.ETag) {
		return protocol.ManagementResponse{StatusCode: http.StatusNotModified, Headers: headers}
	}
	return protocol.ManagementResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(web.Shell()),
		Headers:    headers,
	}
}
