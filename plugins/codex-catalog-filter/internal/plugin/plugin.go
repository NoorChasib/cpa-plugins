// Package plugin answers CPA's native calls for Codex Catalog Filter.
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

	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/catalog"
	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/config"
	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/protocol"
)

const ID = "codex-catalog-filter"

var Version = "0.1.0"

const (
	// CatalogPath is where CPA serves the filtered catalog. Point Codex's
	// model_catalog_url at it; Codex appends ?client_version=<version>.
	CatalogPath = "/v0/resource/plugins/" + ID + "/models"
	// SettingsPath is the Codex Models sidebar page.
	SettingsPath = "/v0/resource/plugins/" + ID + "/settings"
	// StatePath is the page's private data. CPA requires the management key.
	StatePath = "/v0/management/plugins/" + ID + "/state"
)

const (
	// Codex abandons a catalog request after 5 seconds. Answering first lets
	// it keep its cached catalog instead of recording a timeout.
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
				{Method: http.MethodGet, Path: "/plugins/" + ID + "/state", Description: "Private model list and switches for the Codex Models page."},
			},
			Resources: []protocol.ResourceRoute{
				// No Menu: this one is an endpoint for Codex, not a sidebar page.
				{Path: "/models", Description: "Filtered Codex model catalog for Codex's model_catalog_url."},
				{Path: "/settings", Menu: "Codex Models", Description: "Choose which models Codex shows."},
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
	// A rejected reconfiguration returns above and keeps the previous settings.
	p.settings.Store(settings)
	p.seen.load(settings.DataDir)
	return registration(), nil
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
	status, body := p.fetch(settings.CPAURL, req.Query.Get("client_version"), req.Headers.Get("Authorization"))
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

type stateModel struct {
	catalog.Entry
	// Enabled is whether the model stays in Codex's catalog as CPA lists it.
	Enabled bool `json:"enabled"`
	// Switched is whether Enabled comes from an explicit switch.
	Switched bool `json:"switched"`
}

type stateResponse struct {
	CatalogPath string          `json:"catalog_path"`
	SeenAt      *time.Time      `json:"seen_at"`
	Saved       bool            `json:"saved"`
	NewModels   string          `json:"new_models"`
	Action      string          `json:"action"`
	Models      []stateModel    `json:"models"`
	Switches    map[string]bool `json:"switches"`
}

// serveState reports the last catalog Codex was offered and how the current
// switches apply to it. The page saves switches through CPA's own
// PATCH /v0/management/plugins/codex-catalog-filter/config.
func (p *Plugin) serveState() protocol.ManagementResponse {
	settings := p.settings.Load()
	if settings == nil {
		return failure(http.StatusServiceUnavailable, "not_configured")
	}
	rules := settings.Rules
	state := stateResponse{
		CatalogPath: CatalogPath,
		Saved:       true,
		NewModels:   "enabled",
		Action:      string(rules.Action()),
		Models:      []stateModel{},
		Switches:    rules.Switches(),
	}
	if !rules.EnableNew() {
		state.NewModels = "disabled"
	}
	if seen, saved := p.seen.current(); seen != nil {
		at := seen.SeenAt.UTC()
		state.SeenAt, state.Saved = &at, saved
		for _, entry := range seen.Models {
			_, switched := rules.Switched(entry.Slug)
			state.Models = append(state.Models, stateModel{Entry: entry, Enabled: rules.Allows(entry.Slug), Switched: switched})
		}
	}
	body, err := json.Marshal(state)
	if err != nil {
		return failure(http.StatusInternalServerError, "encoding_failed")
	}
	return jsonResponse(http.StatusOK, body)
}

// fetch returns CPA's Codex catalog response, or status 0 when CPA could not
// be reached or answered with more than maxCatalogBytes.
func (p *Plugin) fetch(origin, clientVersion, authorization string) (int, []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	// client_version must be present for CPA to answer in Codex's format.
	target := origin + "/v1/models?" + url.Values{"client_version": {clientVersion}}.Encode()
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

func registration() protocol.Registration {
	return protocol.Registration{SchemaVersion: protocol.SchemaVersion, Metadata: protocol.Metadata{Name: "Codex Catalog Filter", Version: Version, Author: "NoorChasib", GitHubRepository: "https://github.com/NoorChasib/cpa-plugins", ConfigFields: []protocol.ConfigField{
		{Name: "models", Type: "object", Description: "Per-model switches written by the Codex Models page: model slug to true (in Codex) or false (not in Codex)."},
		{Name: "new-models", Type: "enum", EnumValues: []string{"enabled", "disabled"}, Description: "Models without a switch: enabled (default) appear in Codex until switched off; disabled stay out until switched on."},
		{Name: "action", Type: "enum", EnumValues: []string{string(catalog.Remove), string(catalog.Hide)}, Description: "remove (default) deletes switched-off models from the catalog; hide keeps them with visibility hide so they stay selectable by name."},
		{Name: "cpa-url", Type: "string", Description: "Origin the plugin fetches CPA's own catalog from. Defaults to http://127.0.0.1:8317, CPA's listener inside the standard image."},
		{Name: "data-dir", Type: "string", Description: "Optional absolute directory for the last model list Codex was offered. Defaults to plugins/data/codex-catalog-filter beneath CPA's working directory."},
	}}, Capabilities: protocol.RegistrationCapabilities{ManagementAPI: true}}
}
