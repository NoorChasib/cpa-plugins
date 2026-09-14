// Package api serves the built document. It holds no file handle, no watcher,
// and no clock of its own: the runtime publishes a finished document and this
// package hands it out.
//
// That separation is enforced by a test — api must not import the source or
// watch packages — so a request can never trigger a read, and a slow or broken
// filesystem can never turn into a slow response.
package api

import (
	"crypto/sha256"
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

	disabled bool
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

func New(pluginID string) *API {
	a := &API{pluginID: pluginID}
	// Serve a valid, honest document before the first build completes rather
	// than a null body.
	a.Publish(aggregate.Document{
		SchemaVersion: aggregate.SchemaVersion,
		Credentials:   []aggregate.Credential{},
		Providers:     []aggregate.Provider{},
	}, Health{})
	return a
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
	// CPA's management middleware has already required the full management key
	// before any of these runs, which is the whole of this plugin's
	// authentication. The dashboard is served from the resource tree above but
	// reads its data from here, recovering the console's key the same way every
	// other plugin page in this repository does.
	case a.managementPath("/summary"):
		return a.summaryResponse(req)
	case a.managementPath("/health"):
		return a.healthResponse()
	case a.managementPath("/windows"):
		return a.windowsResponse()
	}
	return jsonResponse(http.StatusNotFound, map[string]string{"error": "not_found"})
}

func (a *API) summaryResponse(req protocol.ManagementRequest) protocol.ManagementResponse {
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
