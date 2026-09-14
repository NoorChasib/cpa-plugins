// Package api serves the built document. It holds no file handle, no watcher,
// and no clock of its own: the runtime publishes a finished document and this
// package hands it out.
//
// That separation is enforced by a test — api must not import the source or
// watch packages — so a request can never trigger a read, and a slow or broken
// filesystem can never turn into a slow response.
package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/aggregate"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
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
}

// Health is what the management health route reports.
type Health struct {
	Version             string       `json:"version"`
	CachePath           string       `json:"cache_path"`
	StaleAfter          string       `json:"stale_after"`
	SnapshotWrittenAt   time.Time    `json:"snapshot_written_at"`
	SnapshotNextRequest time.Time    `json:"snapshot_next_request"`
	BuiltAt             time.Time    `json:"built_at"`
	Stale               bool         `json:"stale"`
	StaleReason         string       `json:"stale_reason,omitempty"`
	LastError           string       `json:"last_error,omitempty"`
	Watcher             WatcherState `json:"watcher"`
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
	a := &API{pluginID: pluginID, limiter: newLimiter()}
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
	if req.Method != http.MethodGet {
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "not_found"})
	}
	a.mu.RLock()
	disabled := a.disabled
	a.mu.RUnlock()
	if disabled {
		return jsonResponse(http.StatusServiceUnavailable, map[string]string{"error": "disabled"})
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
	case a.managementPath("/health"):
		return a.healthResponse()
	case a.managementPath("/windows"):
		return a.windowsResponse()
	}
	return jsonResponse(http.StatusNotFound, map[string]string{"error": "not_found"})
}

// tokenSummaryResponse is the public, plugin-authenticated path.
func (a *API) tokenSummaryResponse(req protocol.ManagementRequest, now time.Time) protocol.ManagementResponse {
	// Authenticate first, and never throttle a request that presents the right
	// token. That is what makes a lockout impossible: no volume of hostile
	// traffic can stop the operator reaching their own dashboard.
	if !a.authorized(req.Headers) {
		if !a.limiter.allowFailure(now) {
			return protocol.ManagementResponse{
				StatusCode: http.StatusTooManyRequests,
				Headers:    http.Header{"Retry-After": {"60"}, "Cache-Control": {"no-store"}},
			}
		}
		// Bare: no hint about whether the token was absent, malformed, or
		// merely wrong.
		return protocol.ManagementResponse{
			StatusCode: http.StatusUnauthorized,
			Headers:    http.Header{"Cache-Control": {"no-store"}},
		}
	}
	return a.documentResponse(req)
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
