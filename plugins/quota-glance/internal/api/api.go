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
	// rateLimit is per client address. It exists to slow a brute force against
	// the token, not to lock anyone out: exceeding it costs a 429 for the rest
	// of the minute and nothing more. A ban on the user's own dashboard would
	// be a worse outcome than a slow attack on a long random token.
	rateLimit      = 20
	rateWindow     = time.Minute
	maxTrackedAddr = 4096
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
	limiter   *limiter
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
	switch req.Path {
	case a.resourcePath("/app"):
		return a.appResponse(req)
	case a.resourcePath("/summary"):
		return a.summaryResponse(req, now)
	// CPA's management middleware has already required the full management key
	// before either of these runs.
	case a.managementPath("/health"):
		return a.healthResponse()
	case a.managementPath("/windows"):
		return a.windowsResponse()
	}
	return jsonResponse(http.StatusNotFound, map[string]string{"error": "not_found"})
}

func (a *API) summaryResponse(req protocol.ManagementRequest, now time.Time) protocol.ManagementResponse {
	// Throttle before authenticating, so a brute force is slowed rather than
	// merely counted.
	if !a.limiter.allow(clientAddr(req.Headers), now) {
		return protocol.ManagementResponse{
			StatusCode: http.StatusTooManyRequests,
			Headers:    http.Header{"Retry-After": {"60"}, "Cache-Control": {"no-store"}},
		}
	}
	if !a.authorized(req.Headers) {
		// Bare: no hint about whether the token was absent, malformed, or
		// merely wrong.
		return protocol.ManagementResponse{
			StatusCode: http.StatusUnauthorized,
			Headers:    http.Header{"Cache-Control": {"no-store"}},
		}
	}
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

// clientAddr identifies the caller for rate limiting only. A forged header
// costs the forger their own bucket and nothing else, since exceeding a bucket
// never bans anyone.
func clientAddr(headers http.Header) string {
	for _, name := range []string{"X-Forwarded-For", "X-Real-Ip"} {
		if value := headers.Get(name); value != "" {
			if comma := strings.Index(value, ","); comma > 0 {
				value = value[:comma]
			}
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	return "unknown"
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

// limiter is a fixed-window counter per client address. It never records a
// failure count and never blocks an address beyond the current window.
type limiter struct {
	mu      sync.Mutex
	windows map[string]*bucket
}

type bucket struct {
	start time.Time
	count int
}

func newLimiter() *limiter { return &limiter{windows: map[string]*bucket{}} }

func (l *limiter) allow(addr string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Bound the map so an attacker varying the forwarded address cannot grow
	// it without limit. Expired buckets go first; a full table of live ones is
	// reset outright rather than allowed to grow.
	if len(l.windows) >= maxTrackedAddr {
		for key, b := range l.windows {
			if now.Sub(b.start) >= rateWindow {
				delete(l.windows, key)
			}
		}
		if len(l.windows) >= maxTrackedAddr {
			l.windows = map[string]*bucket{}
		}
	}
	b, ok := l.windows[addr]
	if !ok || now.Sub(b.start) >= rateWindow {
		l.windows[addr] = &bucket{start: now, count: 1}
		return true
	}
	b.count++
	return b.count <= rateLimit
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
