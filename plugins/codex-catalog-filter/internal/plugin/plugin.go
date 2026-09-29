// Package plugin answers CPA's native calls for Codex Catalog Filter.
package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/catalog"
	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/config"
	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/protocol"
)

const ID = "codex-catalog-filter"

var Version = "0.1.0"

// CatalogPath is where CPA serves the filtered catalog. Point Codex's
// model_catalog_url at it; Codex appends ?client_version=<version>.
const CatalogPath = "/v0/resource/plugins/" + ID + "/models"

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
}

func New() *Plugin {
	return &Plugin{client: &http.Client{
		Timeout: fetchTimeout,
		// Loopback to CPA itself: never through an environment proxy, and never
		// follow a redirect somewhere the forwarded credential should not go.
		Transport:     &http.Transport{Proxy: nil, MaxIdleConns: 2, IdleConnTimeout: time.Minute},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
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
		// No Menu: this is an endpoint for Codex, not a sidebar page.
		return protocol.ManagementRegistration{Resources: []protocol.ResourceRoute{
			{Path: "/models", Description: "Filtered Codex model catalog for Codex's model_catalog_url."},
		}}, nil
	case protocol.MethodManagementHandle:
		return p.Serve(raw), nil
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
	// A rejected reconfiguration returns above and keeps the previous settings.
	p.settings.Store(settings)
	return registration(), nil
}

func (p *Plugin) Shutdown() {
	p.lifecycle.Lock()
	defer p.lifecycle.Unlock()
	p.terminal.Store(true)
	p.settings.Store(nil)
	p.client.CloseIdleConnections()
}

// Serve answers GET CatalogPath with CPA's Codex catalog, filtered.
//
// CPA does not authenticate resource routes, and it has no host call that
// returns its model catalog. The catalog is therefore fetched from CPA's own
// /v1/models?client_version=... with the caller's Authorization header, which
// Codex always sends to model_catalog_url. CPA checks that key as it would
// for Codex directly; a request without a valid one gets CPA's own 401.
// Failed client keys do not count toward CPA's management-key IP ban.
func (p *Plugin) Serve(raw []byte) protocol.ManagementResponse {
	// Request headers may carry credentials; they are only ever forwarded.
	var req protocol.ManagementRequest
	if json.Unmarshal(raw, &req) != nil {
		return failure(http.StatusBadRequest, "invalid_request")
	}
	if req.Method != http.MethodGet || req.Path != CatalogPath {
		return failure(http.StatusNotFound, "not_found")
	}
	settings := p.settings.Load()
	if settings == nil {
		return failure(http.StatusServiceUnavailable, "not_configured")
	}
	status, body := p.fetch(settings.CPAURL, req.Query.Get("client_version"), req.Headers.Get("Authorization"))
	switch {
	case status == http.StatusOK:
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
		{Name: "include", Type: "array", Description: "Glob patterns for the model slugs Codex should keep, such as gpt-[0-9]* and codex-*. When empty, the catalog is served unfiltered."},
		{Name: "exclude", Type: "array", Description: "Glob patterns that drop a slug even when include matches it."},
		{Name: "action", Type: "enum", EnumValues: []string{string(catalog.Remove), string(catalog.Hide)}, Description: "remove (default) deletes other entries; hide keeps them with visibility hide so they stay selectable by name."},
		{Name: "cpa-url", Type: "string", Description: "Origin the plugin fetches CPA's own catalog from. Defaults to http://127.0.0.1:8317, CPA's listener inside the standard image."},
	}}, Capabilities: protocol.RegistrationCapabilities{ManagementAPI: true}}
}
