// Package plugin answers CPA's native calls for Model Picker (plugin ID
// codex-catalog-filter): per-client model lists for Codex, Antigravity, and
// Claude Code.
package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/agy"
	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/catalog"
	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/claudelist"
	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/config"
	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/protocol"
)

// ID stays codex-catalog-filter: CPA namespaces routes, configuration, and the
// data directory by it, and Codex installs already point at its URL.
const ID = "codex-catalog-filter"

// Name is the human-facing plugin name and sidebar label.
const Name = "Model Picker"

var Version = "0.2.0"

const (
	// CatalogPath is where CPA serves the filtered Codex catalog. Point Codex's
	// model_catalog_url at it; Codex appends ?client_version=<version>.
	CatalogPath = "/v0/resource/plugins/" + ID + "/models"
	// AgyPath is the Antigravity catalog, for agy's models.listSource.
	AgyPath = "/v0/resource/plugins/" + ID + "/agy/models"
	// SettingsPath is the Model Picker sidebar page.
	SettingsPath = "/v0/resource/plugins/" + ID + "/settings"
	// StatePath is the page's private data. CPA requires the management key.
	StatePath = "/v0/management/plugins/" + ID + "/state"
)

const (
	// Codex abandons a catalog request after 5 seconds and agy after 15.
	// Answering first lets Codex keep its cached catalog and agy its list.
	fetchTimeout = 4 * time.Second
	// CPA's full catalog is several hundred KiB; this only bounds a runaway.
	maxCatalogBytes = 32 << 20
)

type Plugin struct {
	lifecycle sync.Mutex
	settings  atomic.Pointer[config.Settings]
	terminal  atomic.Bool
	client    *http.Client
	now       func() time.Time
	// Immutable after construction: plugins/data/<id> beneath the working
	// directory CPA had when it loaded the library.
	defaultDataDir string
	seen           seenState
	clients        clientStore
	// The last valid client subtrees this process applied, guarded by
	// lifecycle. An invalid subtree falls back to them.
	lastAntigravity *config.Antigravity
	lastClaudeCode  *config.ClaudeCode
	// effectiveAntigravity outlives Shutdown, so a request racing it still
	// gets the user's list rather than a 503.
	effectiveAntigravity atomic.Pointer[config.Antigravity]
	interceptErrors      atomic.Uint64
}

func New() *Plugin {
	p := &Plugin{now: time.Now, client: &http.Client{
		Timeout: fetchTimeout,
		// Loopback to CPA itself: never through an environment proxy, and never
		// follow a redirect somewhere the forwarded credential should not go.
		Transport:     &http.Transport{Proxy: nil, MaxIdleConns: 2, IdleConnTimeout: time.Minute},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	if cwd, err := os.Getwd(); err == nil {
		p.defaultDataDir = filepath.Join(cwd, "plugins", "data", ID)
	}
	return p
}

func (p *Plugin) Handle(method string, raw []byte) (any, error) {
	switch method {
	case protocol.MethodResponseInterceptAfter:
		return p.intercept(raw), nil
	case protocol.MethodPluginRegister, protocol.MethodPluginReconfigure:
		return p.configure(raw)
	case protocol.MethodPluginQuiesce:
		// Nothing runs in the background, so there is nothing to drain.
		return struct{}{}, nil
	case protocol.MethodPluginShutdown:
		p.Shutdown()
		return struct{}{}, nil
	case protocol.MethodManagementRegister:
		return protocol.ManagementRegistration{
			Routes: []protocol.ManagementRoute{
				{Method: http.MethodGet, Path: "/plugins/" + ID + "/state", Description: "Private model lists and switches for the Model Picker page."},
			},
			Resources: []protocol.ResourceRoute{
				// No Menu: these are endpoints for clients, not sidebar pages.
				{Path: "/models", Description: "Filtered Codex model catalog for Codex's model_catalog_url."},
				{Path: "/agy/models", Description: "Antigravity model catalog for agy's models.listSource."},
				{Path: "/settings", Menu: Name, Description: "Choose which models each client shows."},
			},
		}, nil
	case protocol.MethodManagementHandle:
		return p.route(raw), nil
	default:
		return nil, errors.New("unknown method")
	}
}

func (p *Plugin) configure(raw []byte) (protocol.Registration, error) {
	var request protocol.LifecycleRequest
	if json.Unmarshal(raw, &request) != nil || request.SchemaVersion < protocol.SchemaVersion {
		return protocol.Registration{}, errors.New("schema 6 lifecycle request required")
	}
	// Parsing inside the lock keeps concurrent reconfigurations in call order.
	p.lifecycle.Lock()
	defer p.lifecycle.Unlock()
	settings, err := config.Parse(request.ConfigYAML)
	if err != nil {
		return protocol.Registration{}, err
	}
	if p.terminal.Load() {
		return protocol.Registration{}, errors.New("plugin is shut down")
	}
	if settings.DataDir == "" {
		settings.DataDir = p.defaultDataDir
	}
	// A bad client subtree disables only that client's new settings: it keeps
	// the last valid ones, or its defaults, and /state reports the error.
	if a := settings.Antigravity; a.Err == "" {
		p.lastAntigravity = &a
	} else if p.lastAntigravity != nil {
		kept := *p.lastAntigravity
		kept.Configured, kept.Err, kept.Source = a.Configured, a.Err, config.FromLastGood
		settings.Antigravity = kept
	}
	if c := settings.ClaudeCode; c.Err == "" {
		p.lastClaudeCode = &c
	} else if p.lastClaudeCode != nil {
		kept := *p.lastClaudeCode
		kept.Configured, kept.Err, kept.Source = c.Configured, c.Err, config.FromLastGood
		settings.ClaudeCode = kept
	}
	// A rejected reconfiguration returns above and keeps the previous settings.
	p.settings.Store(settings)
	effective := settings.Antigravity
	p.effectiveAntigravity.Store(&effective)
	p.seen.load(settings.DataDir)
	p.clients.load(settings.DataDir)
	return registration(settings), nil
}

func (p *Plugin) Shutdown() {
	p.lifecycle.Lock()
	defer p.lifecycle.Unlock()
	p.terminal.Store(true)
	p.settings.Store(nil)
	p.client.CloseIdleConnections()
}

// route dispatches one management.handle request by exact method and path.
func (p *Plugin) route(raw []byte) protocol.ManagementResponse {
	// Request headers may carry credentials; they are only ever forwarded.
	var req protocol.ManagementRequest
	if json.Unmarshal(raw, &req) != nil {
		return failure(http.StatusBadRequest, "invalid_request")
	}
	if req.Method != http.MethodGet {
		return failure(http.StatusNotFound, "not_found")
	}
	switch req.Path {
	case CatalogPath:
		return p.serveCatalog(req)
	case AgyPath:
		return p.serveAgy(req)
	case SettingsPath:
		// Fixed bytes, answered before any configuration or state is read.
		return settingsPageResponse()
	case StatePath:
		return p.serveState()
	default:
		return failure(http.StatusNotFound, "not_found")
	}
}

// serveCatalog answers GET CatalogPath with CPA's Codex catalog, filtered.
//
// CPA does not authenticate resource routes, and it has no host call that
// returns its model catalog. The catalog is therefore fetched from CPA's own
// /v1/models?client_version=... with the caller's Authorization header, which
// Codex always sends to model_catalog_url. CPA checks that key as it would
// for Codex directly; a request without a valid one gets CPA's own 401.
// Failed client keys do not count toward CPA's management-key IP ban.
func (p *Plugin) serveCatalog(req protocol.ManagementRequest) protocol.ManagementResponse {
	settings := p.settings.Load()
	if settings == nil {
		return failure(http.StatusServiceUnavailable, "not_configured")
	}
	// client_version must be present for CPA to answer in Codex's format.
	target := settings.CPAURL + "/v1/models?" + url.Values{"client_version": {req.Query.Get("client_version")}}.Encode()
	status, body := p.fetch(target, req.Headers.Get("Authorization"))
	switch {
	case status == http.StatusOK:
		// The settings page lists what Codex was last offered, before switches.
		if entries, ok := catalog.Summarize(body); ok {
			p.seen.remember(settings.DataDir, entries, p.now())
		}
		// Rewrite fails open: anything it will not change is served as CPA sent it.
		if filtered := settings.Rules.Rewrite(body); filtered != nil {
			body = filtered
		}
		return jsonResponse(http.StatusOK, body)
	case status >= 400 && status < 500:
		// CPA's own client error, such as a missing or invalid key.
		return jsonResponse(status, body)
	default:
		return failure(http.StatusBadGateway, "cpa_unavailable")
	}
}

// serveAgy answers GET AgyPath, agy's models.listSource, with CPA's
// /v1beta/models built into agy's catalog format (package agy).
//
// Only the caller's Authorization is forwarded, as for Codex, and CPA's 401
// or 403 is passed through. agy keeps no last good list of its own: when this
// route fails it shows a random part of CPA's whole list. So when CPA cannot
// answer (unreachable, 5xx, timeout, a body that is not a model list, or a
// list with nothing agy can be given), the list is rebuilt from the last one
// CPA returned that could be built, but only for a request whose
// Authorization CPA accepted with a 200 within the last 24 hours. A request
// without Authorization, or with any other key, never receives it: CPA does
// not authenticate this route.
func (p *Plugin) serveAgy(req protocol.ManagementRequest) protocol.ManagementResponse {
	authorization := req.Headers.Get("Authorization")
	settings := p.settings.Load()
	if settings == nil {
		// Shutting down: answer from memory rather than make agy fall back.
		if client := p.effectiveAntigravity.Load(); client != nil {
			if out, ok := p.agyLastGood(*client, authorization); ok {
				return out
			}
		}
		return failure(http.StatusServiceUnavailable, "not_configured")
	}
	now := p.now()
	status, body := p.fetch(settings.CPAURL+"/v1beta/models", authorization)
	switch {
	case status == http.StatusOK:
		models, ok := agy.ParseGeminiList(body)
		if !ok {
			// Not proof that CPA accepted the key: only a model list is.
			p.clients.agyFailed("cpa_invalid_list", now)
			return p.agyFallback(settings, authorization)
		}
		result := agy.Build(models, agySelection(settings.Antigravity))
		if result.Body == nil {
			// CPA accepted the key, but its list has nothing agy can be given,
			// as can happen while CPA is still registering models. The last
			// good list is kept and served instead of this one.
			p.clients.acceptKey(settings.DataDir, authorization, now)
			p.clients.agyFailed("no_text_models", now)
			return p.agyFallback(settings, authorization)
		}
		p.clients.rememberGemini(settings.DataDir, models, authorization, now)
		p.clients.agyServed("cpa", now)
		return jsonResponse(http.StatusOK, result.Body)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		p.clients.forgetKey(settings.DataDir, authorization, now)
		return jsonResponse(status, body)
	case status >= 400 && status < 500:
		return jsonResponse(status, body)
	default:
		p.clients.agyFailed("cpa_unavailable", now)
		return p.agyFallback(settings, authorization)
	}
}

func (p *Plugin) agyFallback(settings *config.Settings, authorization string) protocol.ManagementResponse {
	if out, ok := p.agyLastGood(settings.Antigravity, authorization); ok {
		return out
	}
	return failure(http.StatusBadGateway, "cpa_unavailable")
}

func (p *Plugin) agyLastGood(client config.Antigravity, authorization string) (protocol.ManagementResponse, bool) {
	models := p.clients.lastGoodGemini(authorization, p.now())
	if models == nil {
		return protocol.ManagementResponse{}, false
	}
	result := agy.Build(models, agySelection(client))
	if result.Body == nil {
		return protocol.ManagementResponse{}, false
	}
	p.clients.agyServed("last-good", p.now())
	return jsonResponse(http.StatusOK, result.Body), true
}

func agySelection(client config.Antigravity) agy.Selection {
	rules := client.Rules
	return agy.Selection{
		Switched:     rules.Switched,
		EnableNew:    rules.EnableNew(),
		DefaultModel: client.DefaultModel,
		TitleModel:   client.TitleModel,
	}
}

// intercept answers response.intercept_after. It rewrites only CPA's
// Claude-format model list for Claude Code (protocol.ScanClaudeListIntercept)
// and returns an empty result, which keeps CPA's response, for everything
// else, including any failure here.
func (p *Plugin) intercept(raw []byte) (result any) {
	defer func() {
		if recover() != nil {
			p.interceptErrors.Add(1)
			result = struct{}{}
		}
	}()
	settings := p.settings.Load()
	if settings == nil || !settings.ClaudeCode.Filter {
		return struct{}{}
	}
	// The plugin's own loopback fetches carry its User-Agent and pass through
	// the interceptors too; they are never rewritten.
	list, ok := protocol.ScanClaudeListIntercept(raw, ID+"/")
	if !ok {
		return struct{}{}
	}
	summary, body, ok := claudelist.Rewrite(list.Body, settings.ClaudeCode.Rules.Allows)
	if !ok {
		return struct{}{}
	}
	p.clients.rememberClaude(settings.DataDir, summary, body != nil, p.now())
	if body == nil {
		return struct{}{}
	}
	return protocol.ResponseInterceptResponse{Body: body}
}

// fetch returns CPA's response to a GET of target with only the caller's
// Authorization forwarded, or status 0 when CPA could not be reached or
// answered with more than maxCatalogBytes.
func (p *Plugin) fetch(target, authorization string) (int, []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, nil
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	// A neutral agent keeps CPA from routing this to its Claude or Grok lists.
	request.Header.Set("User-Agent", ID+"/"+Version)
	request.Header.Set("Accept", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return 0, nil
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxCatalogBytes+1))
	if err != nil || len(body) > maxCatalogBytes {
		return 0, nil
	}
	return response.StatusCode, body
}

func failure(status int, code string) protocol.ManagementResponse {
	body, _ := json.Marshal(map[string]string{"error": code})
	return jsonResponse(status, body)
}

func jsonResponse(status int, body []byte) protocol.ManagementResponse {
	return protocol.ManagementResponse{StatusCode: status, Body: body, Headers: http.Header{
		"Content-Type":           {"application/json; charset=utf-8"},
		"Cache-Control":          {"no-store"},
		"X-Content-Type-Options": {"nosniff"},
	}}
}

func registration(settings *config.Settings) protocol.Registration {
	return protocol.Registration{SchemaVersion: protocol.SchemaVersion, Metadata: protocol.Metadata{Name: Name, Version: Version, Author: "NoorChasib", GitHubRepository: "https://github.com/NoorChasib/cpa-plugins", ConfigFields: []protocol.ConfigField{
		{Name: "models", Type: "object", Description: "Codex: per-model switches written by the Model Picker page, model slug to true (in Codex) or false (not in Codex)."},
		{Name: "new-models", Type: "enum", EnumValues: []string{"enabled", "disabled"}, Description: "Codex: models without a switch. enabled (default) appear until switched off; disabled stay out until switched on."},
		{Name: "action", Type: "enum", EnumValues: []string{string(catalog.Remove), string(catalog.Hide)}, Description: "Codex: remove (default) deletes switched-off models from the catalog; hide keeps them with visibility hide so they stay selectable by name."},
		{Name: "cpa-url", Type: "string", Description: "Origin the plugin fetches CPA's own model lists from. Defaults to http://127.0.0.1:8317, CPA's listener inside the standard image."},
		{Name: "data-dir", Type: "string", Description: "Optional absolute directory for the last model lists clients were offered. Defaults to plugins/data/codex-catalog-filter beneath CPA's working directory."},
		{Name: config.AntigravityKey, Type: "object", Description: "Antigravity (agy): models (id to true or false), new-models (default disabled), default-model, and title-model for the list at /agy/models."},
		{Name: config.ClaudeCodeKey, Type: "object", Description: "Claude Code: filter (default true) rewrites CPA's model list for Claude Code; models (real id to true or false) and new-models (default enabled)."},
	}}, Capabilities: protocol.RegistrationCapabilities{ManagementAPI: true, ResponseInterceptor: settings != nil && settings.ClaudeCode.Filter}}
}
