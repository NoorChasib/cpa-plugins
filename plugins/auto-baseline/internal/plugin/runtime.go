// Package plugin implements the RPC-facing runtime: lifecycle dispatch
// (register/reconfigure/quiesce/shutdown), the request interceptor observer,
// management route registration, the authenticated status (JSON and browser
// HTML), observe and reset handlers, plus the static read-only browser
// resource shell.
package plugin

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/clock"
	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/config"
	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/engine"
	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/hostapi"
)

// Plugin identity constants.
const (
	PluginID      = "auto-baseline"
	PluginName    = "Auto Baseline"
	PluginVersion = "0.1.3"
	PluginAuthor  = "NoorChasib"
	PluginRepo    = "https://github.com/NoorChasib/cpa-plugins"
)

// Management/resource route paths (relative; the host prefixes
// /v0/management and /v0/resource/plugins/<id> respectively).
const (
	managementStatusPath     = "/plugins/" + PluginID + "/status"
	managementStatusPagePath = "/plugins/" + PluginID + "/status/html"
	managementObservePath    = "/plugins/" + PluginID + "/observe"
	managementResetPath      = "/plugins/" + PluginID + "/reset"
	managementDryRunPath     = "/plugins/" + PluginID + "/dry-run"
	resourceStatusPath       = "/status"

	// actionRequestHeader makes mutating management requests non-simple in
	// browsers, preventing an ambient reverse-proxy management credential
	// from being ridden by a cross-origin HTML form POST. Same convention as
	// the sibling plugins (X-Account-Health-Action, X-Reset-Priority-Refresh).
	actionRequestHeader      = "X-Auto-Baseline-Action"
	actionRequestHeaderValue = "1"

	// maxObserveBodyBytes bounds the JSON body of the observe route.
	maxObserveBodyBytes = 16 << 10
)

// Runtime dispatches host RPC calls and owns the engine lifecycle.
type Runtime struct {
	mu     sync.Mutex
	bridge *hostapi.Bridge
	// lifecycleMu serializes register / reconfigure / quiesce / shutdown so
	// they never interleave. Interceptor and management dispatches do NOT
	// take it: they only read the engine pointer under mu.
	lifecycleMu sync.Mutex
	clk         clock.Clock
	// runAsync lets tests run background promotion synchronously.
	runAsync func(func())

	engine *engine.Engine

	dispatchMu   sync.Mutex
	dispatchWG   sync.WaitGroup
	shuttingDown bool
	shutdownOnce sync.Once
}

// Option customizes a Runtime (test hooks).
type Option func(*Runtime)

// WithClock overrides the clock (tests).
func WithClock(clk clock.Clock) Option {
	return func(r *Runtime) { r.clk = clk }
}

// WithRunAsync overrides deferred execution (tests run inline).
func WithRunAsync(run func(func())) Option {
	return func(r *Runtime) { r.runAsync = run }
}

// NewRuntime builds a Runtime over a raw host caller.
func NewRuntime(caller hostapi.Caller, opts ...Option) *Runtime {
	r := &Runtime{
		bridge:   hostapi.NewBridge(caller),
		clk:      clock.Real{},
		runAsync: func(f func()) { go f() },
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Dispatch handles one RPC method call from the host and returns envelope
// bytes. It never panics across the boundary: a panic inside an interceptor
// call would otherwise fuse (permanently disable) the plugin
// (internal/pluginhost/adapters_interceptors.go:18-35). Native shutdown
// closes the dispatch gate and waits for calls that already entered it.
func (r *Runtime) Dispatch(method string, request []byte) (response []byte) {
	if !r.beginDispatch() {
		return errorEnvelope("plugin_shutdown", "auto-baseline is shutting down")
	}
	defer r.dispatchWG.Done()
	defer func() {
		if recovered := recover(); recovered != nil {
			response = errorEnvelope("plugin_panic", fmt.Sprintf("auto-baseline: internal error: %v", recovered))
		}
	}()
	switch method {
	case hostapi.MethodPluginRegister, hostapi.MethodPluginReconfigure:
		r.lifecycleMu.Lock()
		defer r.lifecycleMu.Unlock()
		return r.handleLifecycle(request)
	case hostapi.MethodPluginQuiesce, hostapi.MethodPluginShutdown:
		// The host performs terminal shutdown through the native function
		// pointer. JSON lifecycle messages only quiesce so this Dispatch can
		// return normally and a later reconfigure may restart the engine.
		r.lifecycleMu.Lock()
		defer r.lifecycleMu.Unlock()
		r.Quiesce()
		return okEnvelope(struct{}{})
	case hostapi.MethodRequestInterceptBefore:
		return r.handleInterceptBefore(request)
	case hostapi.MethodRequestInterceptAfter:
		// Observation happens before credential selection; after-auth is a
		// declared-capability obligation answered with "no changes".
		return interceptNoopEnvelope
	case hostapi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case hostapi.MethodManagementHandle:
		return r.handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "auto-baseline does not handle method "+method)
	}
}

func (r *Runtime) beginDispatch() bool {
	if r == nil {
		return false
	}
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	if r.shuttingDown {
		return false
	}
	r.dispatchWG.Add(1)
	return true
}

// Quiesce stops learning and promotion while retaining status and allowing a
// later lifecycle reconfigure to resume.
func (r *Runtime) Quiesce() {
	if eng := r.currentEngine(); eng != nil {
		eng.Stop()
	}
}

// Shutdown performs terminal native shutdown. It gates new dispatches and host
// callbacks, quiesces promptly, then drains entered dispatches, engine-owned
// work, and native callback workers before returning to the ABI layer.
func (r *Runtime) Shutdown() {
	if r == nil {
		return
	}
	r.shutdownOnce.Do(func() {
		r.dispatchMu.Lock()
		r.shuttingDown = true
		r.dispatchMu.Unlock()

		r.bridge.Quiesce()
		r.Quiesce()

		r.dispatchWG.Wait()
		r.lifecycleMu.Lock()
		if eng := r.currentEngine(); eng != nil {
			eng.Shutdown()
		}
		r.lifecycleMu.Unlock()
		r.bridge.Drain()
	})
}

// handleLifecycle processes plugin.register / plugin.reconfigure. The
// request's config_yaml field is a []byte on the wire (base64 via standard
// encoding/json), containing the preserved plugins.configs.auto-baseline
// YAML subtree.
func (r *Runtime) handleLifecycle(request []byte) []byte {
	var req hostapi.LifecycleRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return errorEnvelope("invalid_request", "decode lifecycle request: malformed JSON")
		}
	}
	cfg, errCfg := config.Parse(req.ConfigYAML)
	if errCfg != nil {
		// The host refuses the plugin on an error envelope and drops its
		// capabilities WITHOUT calling quiesce, so a still-running engine from
		// a previous registration must be stopped here. CPA keeps serving
		// requests with its static baseline, which is the fail-safe posture.
		r.Quiesce()
		return errorEnvelope("invalid_config", errCfg.Error())
	}

	// Never hold r.mu across Engine.Start/Reconfigure: they do state I/O,
	// config probes, and host.log callbacks, and the interceptor path takes
	// r.mu to look up the engine on every request.
	r.mu.Lock()
	eng := r.engine
	fresh := eng == nil
	if fresh {
		eng = r.buildEngine(cfg)
		r.engine = eng
	}
	r.mu.Unlock()
	if fresh {
		if cfg.Enabled {
			eng.Start()
		}
	} else {
		// Apply the NEW config first (it may change state-dir, config-path,
		// or the rules), then resume if it enables the plugin. Reconfigure
		// itself resumes a stopped engine when cfg.Enabled is true.
		eng.Reconfigure(cfg)
	}

	// Negotiate the schema: never claim more than the host offered, never
	// more than we implement. A missing/zero host schema means the original
	// contract (1).
	schema := hostapi.SchemaVersion
	if req.SchemaVersion == 0 {
		schema = 1
	} else if req.SchemaVersion < schema {
		schema = req.SchemaVersion
	}
	return okEnvelope(hostapi.Registration{
		SchemaVersion: schema,
		Metadata: hostapi.Metadata{
			Name:             PluginName,
			Version:          PluginVersion,
			Author:           PluginAuthor,
			GitHubRepository: PluginRepo,
			ConfigFields:     configFields(),
		},
		Capabilities: hostapi.Capabilities{RequestInterceptor: true, ManagementAPI: true},
	})
}

// buildEngine wires the production engine over the host bridge.
func (r *Runtime) buildEngine(cfg config.Config) *engine.Engine {
	return engine.New(cfg, engine.Deps{
		Clock:    r.clk,
		Log:      func(level, message string) { r.bridge.Log(level, message) },
		RunAsync: r.runAsync,
	})
}

func (r *Runtime) currentEngine() *engine.Engine {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.engine
}

// interceptNoopEnvelope is the precomputed "no changes" reply: an empty
// RequestInterceptResponse (adapters_interceptors.go:128-137). It is shared
// and must never be mutated by callers.
var interceptNoopEnvelope = []byte(`{"ok":true,"result":{}}`)

// interceptHeadersOnly is the hot-path projection of the interceptor request:
// only Headers is decoded; Body and Metadata are skipped entirely. The full
// hostapi.RequestInterceptRequest is kept for wire parity and tests.
type interceptHeadersOnly struct {
	Headers map[string][]string `json:"Headers"`
}

// handleInterceptBefore observes the inbound request headers and always
// answers "no changes". Errors are also answered as no-change envelopes: a
// decode failure of our own request must never surface as an interceptor
// error, and observing is best-effort by design.
func (r *Runtime) handleInterceptBefore(request []byte) []byte {
	eng := r.currentEngine()
	if eng == nil || len(request) == 0 {
		return interceptNoopEnvelope
	}
	var req interceptHeadersOnly
	if err := json.Unmarshal(request, &req); err != nil {
		return interceptNoopEnvelope
	}
	eng.Observe(req.Headers)
	return interceptNoopEnvelope
}

// managementRegistration declares the plugin routes.
//
// Wire casing (audited): the wrapper keys are lowercase routes/resources;
// route fields are the capitalized Go names because the upstream structs are
// untagged. Authenticated GET routes must leave Menu empty, otherwise the
// host converts them into unauthenticated legacy resource routes.
func managementRegistration() hostapi.ManagementRegistration {
	return hostapi.ManagementRegistration{
		Routes: []hostapi.ManagementRoute{
			{Method: "GET", Path: managementStatusPath, Description: "Auto-baseline status snapshot (JSON)"},
			{Method: "GET", Path: managementStatusPagePath, Description: "Auto-baseline status page (browser HTML)"},
			{Method: "POST", Path: managementObservePath, Description: "Report a client fingerprint observation (host reporting)"},
			{Method: "POST", Path: managementResetPath, Description: "Clear pending baseline candidates"},
			{Method: "POST", Path: managementDryRunPath, Description: "Set plugins.configs.auto-baseline.dry-run in CPA's config.yaml"},
		},
		Resources: []hostapi.ResourceRoute{
			{Path: resourceStatusPath, Menu: "Auto Baseline", Description: "Public read-only plugin information"},
		},
	}
}

// handleManagement dispatches management.handle requests for both the
// authenticated management routes and the read-only resource page.
func (r *Runtime) handleManagement(request []byte) []byte {
	var req hostapi.ManagementRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return errorEnvelope("invalid_request", "decode management request: malformed JSON")
		}
	}

	path := strings.TrimRight(req.Path, "/")
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	eng := r.currentEngine()
	if method == "GET" && isResourcePath(path) {
		// Resource routes are NOT management-authenticated: render the
		// redacted view (no paths, errors, or warnings) and never mutate,
		// whatever the query string says. The embedded script upgrades the
		// view client-side when the browser already holds a same-origin
		// management session.
		var snap engine.Snapshot
		if eng != nil {
			snap = redactResourceStatus(eng.Status(PluginID, PluginVersion))
		}
		return okEnvelope(htmlResponse(200, renderResourcePageForTest(snap)))
	}

	if eng == nil {
		return okEnvelope(jsonResponse(503, map[string]string{"error": "plugin is not registered yet"}))
	}

	switch {
	case method == "GET" && strings.HasSuffix(path, managementStatusPagePath) && !isResourcePath(path):
		return okEnvelope(htmlResponse(200, renderStatusPage(eng.Status(PluginID, PluginVersion), true)))

	case method == "GET" && strings.HasSuffix(path, managementStatusPath) && !isResourcePath(path):
		return okEnvelope(jsonResponse(200, eng.Status(PluginID, PluginVersion)))

	case method == "POST" && strings.HasSuffix(path, managementObservePath) && !isResourcePath(path):
		if !mutationRequestAllowed(req.Headers) {
			return okEnvelope(forbiddenResponse())
		}
		if len(req.Body) > maxObserveBodyBytes {
			return okEnvelope(jsonResponse(413, map[string]string{"status": "error", "detail": "request body too large"}))
		}
		var report engine.Report
		if err := json.Unmarshal(req.Body, &report); err != nil {
			return okEnvelope(jsonResponse(400, map[string]string{"status": "error", "detail": "body must be a JSON object with provider, user_agent, package_version, runtime_version, os, arch, session_id, force"}))
		}
		outcome := eng.ReportObservation(report)
		status := 202
		if !outcome.Accepted {
			status = 422
		}
		// Queued forced promotions are reported as 202 too; the body says
		// "queued" so callers can tell the two apart.
		return okEnvelope(jsonResponse(status, outcome))

	case method == "POST" && strings.HasSuffix(path, managementResetPath) && !isResourcePath(path):
		if !mutationRequestAllowed(req.Headers) {
			return okEnvelope(forbiddenResponse())
		}
		eng.Reset()
		return okEnvelope(jsonResponse(200, map[string]string{"status": "ok", "detail": "pending candidates cleared"}))

	case method == "POST" && strings.HasSuffix(path, managementDryRunPath) && !isResourcePath(path):
		if !mutationRequestAllowed(req.Headers) {
			return okEnvelope(forbiddenResponse())
		}
		if len(req.Body) > maxObserveBodyBytes {
			return okEnvelope(jsonResponse(413, map[string]string{"status": "error", "detail": "request body too large"}))
		}
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.Unmarshal(req.Body, &body); err != nil || body.Enabled == nil {
			return okEnvelope(jsonResponse(400, map[string]string{"status": "error", "detail": "body must be a JSON object {\"enabled\": true|false}"}))
		}
		outcome := eng.SetDryRun(*body.Enabled)
		return okEnvelope(jsonResponse(outcome.Status, outcome))

	default:
		return okEnvelope(jsonResponse(404, map[string]string{"error": "unknown route"}))
	}
}

func forbiddenResponse() hostapi.ManagementResponse {
	return jsonResponse(403, map[string]string{
		"status": "forbidden",
		"detail": "mutating routes require the " + actionRequestHeader + " header and same-origin browser metadata",
	})
}

// mutationRequestAllowed implements the CSRF gate shared by observe, reset,
// and dry-run. The custom non-simple header is mandatory: a cross-origin HTML form
// cannot set it, and CPA's CORS layer only reflects it after a preflight that
// this check also defends against.
//
// Browsers attach Fetch Metadata (Sec-Fetch-Site) only when the request URL is
// potentially trustworthy: https, or a loopback host. When present it is
// authoritative and only same-origin and navigation ("none") are accepted.
//
// When CPA is reached over plain HTTP on any other host, even a current
// browser sends no fetch metadata, so Origin is the only browser-controlled
// signal left. An http:// Origin is consistent with the legitimate status
// page: a browser can only post to a plain-HTTP server from a plain-HTTP
// page, because mixed-content blocking stops https pages from fetching http
// URLs. It is accepted. An https://, "null", or malformed Origin without
// fetch metadata cannot come from a supported browser talking to this route
// and is rejected. Requests carrying neither Origin nor fetch metadata are
// non-browser management clients and are accepted.
func mutationRequestAllowed(headers map[string][]string) bool {
	if !headerHasToken(headers, actionRequestHeader, actionRequestHeaderValue) {
		return false
	}
	fetchSite, hasFetchSite := headerTokens(headers, "Sec-Fetch-Site")
	if hasFetchSite {
		if len(fetchSite) == 0 {
			return false
		}
		for _, site := range fetchSite {
			if !strings.EqualFold(site, "same-origin") && !strings.EqualFold(site, "none") {
				return false
			}
		}
		return true
	}
	origins, hasOrigin := headerTokens(headers, "Origin")
	if !hasOrigin {
		return true
	}
	if len(origins) != 1 {
		return false
	}
	return isPlainHTTPOrigin(origins[0])
}

// isPlainHTTPOrigin reports whether value is a well-formed http:// origin
// (scheme and host only). Browsers omit fetch metadata for requests to such
// origins, so this is the shape a legitimate plain-HTTP status page produces.
func isPlainHTTPOrigin(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Scheme, "http") &&
		parsed.Host != "" &&
		parsed.User == nil &&
		parsed.Path == "" &&
		parsed.RawQuery == "" &&
		parsed.Fragment == "" &&
		!parsed.ForceQuery
}

func headerHasToken(headers map[string][]string, name, token string) bool {
	for key, values := range headers {
		if !strings.EqualFold(strings.TrimSpace(key), name) {
			continue
		}
		for _, value := range values {
			for _, part := range strings.Split(value, ",") {
				if strings.EqualFold(strings.TrimSpace(part), token) {
					return true
				}
			}
		}
	}
	return false
}

func headerTokens(headers map[string][]string, name string) ([]string, bool) {
	var tokens []string
	present := false
	for key, values := range headers {
		if !strings.EqualFold(strings.TrimSpace(key), name) {
			continue
		}
		present = true
		for _, value := range values {
			for _, part := range strings.Split(value, ",") {
				if token := strings.TrimSpace(part); token != "" {
					tokens = append(tokens, token)
				}
			}
		}
	}
	return tokens, present
}

func isResourcePath(path string) bool {
	return strings.Contains(path, "/v0/resource/") || !strings.Contains(path, "/v0/management")
}

func configFields() []hostapi.ConfigField {
	return []hostapi.ConfigField{
		{Name: "enabled", Type: "boolean", Description: "Enable the auto-baseline plugin."},
		{Name: "priority", Type: "integer", Description: "CPA plugin load/order priority. Unrelated to fingerprint learning."},
		{Name: "dry-run", Type: "boolean", Description: "Compute and report promotions but never write config.yaml (default false; recommended true for first install)."},
		{Name: "config-path", Type: "string", Description: "Path of CPA's config.yaml. Default: the running process's -config flag, else <cwd>/config.yaml."},
		{Name: "state-dir", Type: "string", Description: "Directory for state.json (default plugins/auto-baseline, relative to the CPA working directory)."},
		{Name: "backup-dir", Type: "string", Description: "Directory that receives config.yaml.auto-baseline.bak before each write (default: state-dir)."},
		{Name: "manage-claude", Type: "boolean", Description: "Learn and promote claude-header-defaults (default true)."},
		{Name: "manage-codex", Type: "boolean", Description: "Learn and promote codex-header-defaults.user-agent (default true; requires codex.disable-codex-cloaking: true to take effect)."},
		{Name: "claude-entrypoints", Type: "array", Description: "Claude Code entrypoints whose fingerprints may be learned (default cli, sdk-cli, claude-vscode, sdk-ts, sdk-py)."},
		{Name: "require-claude-code-beta", Type: "boolean", Description: "Require the claude-code-20250219 beta in anthropic-beta before a Claude request counts (default true)."},
		{Name: "min-observations", Type: "integer", Description: "Observations of one identical fingerprint tuple required inside observation-window (default 3)."},
		{Name: "min-distinct-sessions", Type: "integer", Description: "Distinct client session IDs those observations must span (default 1; anonymous requests never count as a session). Set 2 when clients send X-Claude-Code-Session-Id."},
		{Name: "observation-window", Type: "string", Description: "How long an observation counts toward quorum (default 24h)."},
		{Name: "promotion-cooldown", Type: "string", Description: "Minimum spacing between config.yaml writes (default 60s)."},
		{Name: "claude-min-version", Type: "string", Description: "Never write a Claude baseline below this version (default 2.1.220, the compiled default of the audited CPA build)."},
		{Name: "codex-min-version", Type: "string", Description: "Never write a Codex baseline below this version (default 0.146.0, the compiled default of the audited CPA build)."},
		{Name: "require-explicit-baseline", Type: "boolean", Description: "Never promote a provider whose config.yaml baseline is implicit (compiled default assumed). Safe choice after a CPA upgrade (default false)."},
		{Name: "display-timezone", Type: "string", Description: "IANA time zone for timestamps on the HTML status view, e.g. America/Los_Angeles, or \"local\" (default UTC). Presentation only."},
	}
}

func jsonResponse(status int, payload any) hostapi.ManagementResponse {
	body, err := json.Marshal(payload)
	if err != nil {
		body = []byte(`{"error":"encode response failed"}`)
		status = 500
	}
	return hostapi.ManagementResponse{
		StatusCode: status,
		Headers: map[string][]string{
			"Content-Type":           {"application/json; charset=utf-8"},
			"Cache-Control":          {"no-store"},
			"X-Content-Type-Options": {"nosniff"},
		},
		Body: body,
	}
}

// htmlResponse carries the same header set as the sibling plugins: no-store
// caching, nosniff, a CSP that allows only inline style/script and
// same-origin fetches (frame-ancestors 'self' lets the management console
// iframe the page from the CPA origin), and no referrer leakage.
func htmlResponse(status int, body []byte) hostapi.ManagementResponse {
	return hostapi.ManagementResponse{
		StatusCode: status,
		Headers: map[string][]string{
			"Content-Type":            {"text/html; charset=utf-8"},
			"Cache-Control":           {"no-store"},
			"X-Content-Type-Options":  {"nosniff"},
			"Content-Security-Policy": {"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"},
			"Referrer-Policy":         {"no-referrer"},
		},
		Body: body,
	}
}

// renderResourcePageForTest is the seam Dispatch uses for the resource page
// so tests can inject a panicking handler and prove the recover boundary.
// Production never reassigns it.
var renderResourcePageForTest = func(snap engine.Snapshot) []byte { return renderStatusPage(snap, false) }

func okEnvelope(result any) []byte {
	raw, err := json.Marshal(result)
	if err != nil {
		return errorEnvelope("encode_failed", "encode result failed")
	}
	env, errEnv := json.Marshal(hostapi.Envelope{OK: true, Result: raw})
	if errEnv != nil {
		return errorEnvelope("encode_failed", "encode envelope failed")
	}
	return env
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(hostapi.Envelope{
		OK:    false,
		Error: &hostapi.EnvelopeError{Code: code, Message: message},
	})
	return raw
}
