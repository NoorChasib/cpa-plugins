// Package plugin implements the RPC-facing runtime: lifecycle dispatch
// (register/reconfigure/quiesce/shutdown), management route registration,
// the authenticated status (JSON and browser HTML) and refresh handlers,
// the static read-only browser resource shell, and the optional API credit
// router: its rebuild loop and the scheduler.pick answer.
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/clock"
	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/config"
	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/engine"
	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/hostapi"
	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/providers"
	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/route"
)

// Plugin identity constants.
const (
	PluginID      = "reset-priority"
	PluginName    = "Reset Priority"
	PluginVersion = "0.2.1"
	PluginAuthor  = "NoorChasib"
	PluginRepo    = "https://github.com/NoorChasib/cpa-plugins"
)

// Management/resource route paths (relative; the host prefixes
// /v0/management and /v0/resource/plugins/<id> respectively).
const (
	managementStatusPath     = "/plugins/" + PluginID + "/status"
	managementStatusPagePath = "/plugins/" + PluginID + "/status/html"
	managementRefreshPath    = "/plugins/" + PluginID + "/refresh"
	resourceStatusPath       = "/status"

	// refreshRequestHeader makes the mutating management request non-simple in
	// browsers, preventing an ambient reverse-proxy management credential from
	// being ridden by a cross-origin HTML form POST.
	refreshRequestHeader      = "X-Reset-Priority-Refresh"
	refreshRequestHeaderValue = "1"
)

// Runtime dispatches host RPC calls and owns the engine lifecycle.
type Runtime struct {
	mu     sync.Mutex
	bridge *hostapi.Bridge
	clk    clock.Clock
	// runAsync lets tests run the startup reconcile synchronously.
	runAsync func(func())

	engine *engine.Engine
	// routing is the effective route-api-credits value of the last
	// successful configure, under mu, so a change is logged once.
	routing bool

	// router is the API credit router. It is created here, never replaced,
	// and lasts as long as the process, so a pick racing a configure always
	// finds one. loop rebuilds its table.
	router *route.Router
	loop   *routerLoop
	// beforePick is a test-only hook, never set in production. It runs at the
	// top of SchedulerPick, outside the router's own recover.
	beforePick func()

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

// WithBeforeSchedulerPick installs a hook that runs at the top of every
// SchedulerPick (tests: a panicking hook proves the ABI still answers
// handled:false).
func WithBeforeSchedulerPick(hook func()) Option {
	return func(r *Runtime) { r.beforePick = hook }
}

// NewRuntime builds a Runtime over a raw host caller.
func NewRuntime(caller hostapi.Caller, opts ...Option) *Runtime {
	r := &Runtime{
		bridge:   hostapi.NewBridge(caller),
		clk:      clock.Real{},
		runAsync: func(f func()) { go f() },
		router:   route.New(),
	}
	for _, opt := range opts {
		opt(r)
	}
	r.loop = newRouterLoop(r.clk, r.router, func(level, message string) { r.bridge.Log(level, message) })
	return r
}

// SchedulerPick answers one scheduler.pick request through the API credit
// router. It takes no lock, does no I/O, logs nothing and makes no host
// callback: CPA gives a scheduler no deadline, so anything that could block
// would hold the client's request. The router recovers its own panics and
// declines every doubt, which CPA resolves with its own selector.
func (r *Runtime) SchedulerPick(raw []byte) route.Decision {
	if r == nil {
		return route.Decision{Reason: route.ReasonDisabled}
	}
	if r.beforePick != nil {
		r.beforePick()
	}
	return r.router.Pick(raw, r.clk.Now().UTC())
}

// Dispatch handles one RPC method call from the host and returns envelope
// bytes. It never panics across the boundary. Native shutdown closes the
// dispatch gate and waits for calls that already entered it.
func (r *Runtime) Dispatch(method string, request []byte) (response []byte) {
	if !r.beginDispatch() {
		return errorEnvelope("plugin_shutdown", "reset-priority is shutting down")
	}
	defer r.dispatchWG.Done()
	defer func() {
		if recovered := recover(); recovered != nil {
			response = errorEnvelope("plugin_panic", fmt.Sprintf("reset-priority: internal error: %v", recovered))
		}
	}()
	switch method {
	case hostapi.MethodPluginRegister, hostapi.MethodPluginReconfigure:
		return r.handleLifecycle(request)
	case hostapi.MethodPluginQuiesce, hostapi.MethodPluginShutdown:
		// The host performs terminal shutdown through the native function
		// pointer. JSON lifecycle messages only quiesce so this Dispatch can
		// return normally and a later reconfigure may restart the engine.
		r.Quiesce()
		return okEnvelope(struct{}{})
	case hostapi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case hostapi.MethodManagementHandle:
		return r.handleManagement(request)
	case hostapi.MethodSchedulerPick:
		// The native entry point answers picks before they reach Dispatch,
		// so that no gate here can fail a request. This answers the same,
		// for a caller that dispatches in process.
		d := r.SchedulerPick(request)
		return okEnvelope(hostapi.SchedulerPickResult{Handled: d.AuthID != "", AuthID: d.AuthID})
	default:
		return errorEnvelope("unknown_method", "reset-priority does not handle method "+method)
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

// Quiesce stops engine scheduling and the router loop while retaining status
// and allowing a later lifecycle reconfigure to restart both. The router's
// table is withdrawn, so picks decline no_table until a configure builds one;
// its exploration history is kept for that configure.
func (r *Runtime) Quiesce() {
	if r == nil {
		return
	}
	r.mu.Lock()
	eng := r.engine
	r.mu.Unlock()
	if eng != nil {
		eng.Stop()
	}
	r.stopRouter(false)
}

// stopRouter stops and drains the router loop, then withdraws the table.
// final also turns routing off, for terminal shutdown. Shutdown runs from the
// native export with nothing above it to recover a panic, so this carries its
// own, and withdraws the table even when stopping the loop panicked: picks
// must not keep routing on a table nobody rebuilds.
func (r *Runtime) stopRouter(final bool) {
	defer func() { _ = recover() }()
	defer func() {
		if final {
			r.router.SetEnabled(false)
		}
		r.router.Publish(nil, r.clk.Now().UTC())
	}()
	r.loop.stop()
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

		// Close the outbound gate before stopping work. A synchronous native
		// callback that already entered cannot be cancelled; Bridge.Drain below
		// waits for it to return.
		r.bridge.Quiesce()
		r.Quiesce()

		// No lifecycle dispatch can still install or reconfigure an engine after
		// this wait, so the final engine pointer is stable. One that entered
		// before the gate closed may have restarted the router loop, so it is
		// stopped again, now for good. The loop logs through the bridge, so it
		// stops before the drain below.
		r.dispatchWG.Wait()
		r.stopRouter(true)
		r.mu.Lock()
		eng := r.engine
		r.mu.Unlock()
		if eng != nil {
			eng.Shutdown()
		}
		r.bridge.Drain()
	})
}

// handleLifecycle processes plugin.register / plugin.reconfigure. The
// request's config_yaml field is a []byte on the wire (base64 via standard
// encoding/json), containing the preserved plugins.configs.reset-priority
// YAML subtree.
//
// The scheduler capability is declared only while the plugin is enabled with
// route-api-credits on. Both directions fail open: turning routing on, CPA can
// call Pick before the loop's first build and gets no_table; turning it off,
// the router is disabled before this returns, so a pick racing CPA's snapshot
// swap gets disabled. Either way CPA's own selector chooses.
func (r *Runtime) handleLifecycle(request []byte) []byte {
	// Cleared first, so any error below leaves the scheduler undeclared. CPA
	// usually drops the record of a plugin whose configure fails, but can
	// keep its old capabilities on a failed rollback, so declared is what
	// this plugin last declared, not what CPA holds.
	r.setDeclared(false)

	var req hostapi.LifecycleRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return errorEnvelope("invalid_request", "decode lifecycle request: malformed JSON")
		}
	}
	cfg, errCfg := config.Parse(req.ConfigYAML)
	if errCfg != nil {
		return errorEnvelope("invalid_config", errCfg.Error())
	}
	routing := cfg.Routing()

	r.mu.Lock()
	if r.engine == nil {
		r.engine = r.buildEngineLocked(cfg)
		if cfg.Enabled {
			r.engine.Start()
		}
		r.mu.Unlock()
	} else {
		eng := r.engine
		r.mu.Unlock()
		eng.Reconfigure(cfg)
	}

	if cfg.Enabled {
		// The loop runs with routing on or off, so the status shows whether
		// turning it on would route before anyone does.
		r.router.SetEnabled(routing)
		r.loop.start(cfg.RouterQuotaCachePath, routing)
	} else {
		r.router.SetEnabled(false)
		r.loop.stop()
		r.router.Publish(nil, r.clk.Now().UTC())
	}
	r.mu.Lock()
	changed := r.routing != routing
	r.routing = routing
	r.mu.Unlock()
	if changed {
		if routing {
			r.bridge.Log("info", logRoutingOn)
		} else {
			r.bridge.Log("info", logRoutingOff)
		}
	}
	r.setDeclared(routing)

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
		Capabilities: hostapi.Capabilities{ManagementAPI: true, Scheduler: routing},
	})
}

// setDeclared records whether the scheduler capability is declared, for the
// status and for the loop's lasting-inactive warning.
func (r *Runtime) setDeclared(on bool) {
	r.router.SetDeclared(on)
	r.loop.setDeclared(on)
}

// buildEngineLocked wires the production engine over the host bridge.
func (r *Runtime) buildEngineLocked(cfg config.Config) *engine.Engine {
	now := r.clk.Now
	return engine.New(cfg, engine.Deps{
		Clock: r.clk,
		Host:  r.bridge,
		Providers: map[string]providers.Provider{
			config.ProviderClaude: providers.NewClaude(r.bridge, now),
			config.ProviderCodex:  providers.NewCodex(r.bridge, now),
		},
		Log:      func(level, message string) { r.bridge.Log(level, message) },
		RunAsync: r.runAsync,
	})
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
			{
				Method:      "GET",
				Path:        managementStatusPath,
				Description: "Reset-priority status snapshot (JSON)",
			},
			{
				Method:      "GET",
				Path:        managementStatusPagePath,
				Description: "Reset-priority status page (browser HTML)",
			},
			{
				Method:      "POST",
				Path:        managementRefreshPath,
				Description: "Run a full reconciliation now",
			},
		},
		Resources: []hostapi.ResourceRoute{
			{
				Path:        resourceStatusPath,
				Menu:        "Reset Priority",
				Description: "Public read-only plugin information",
			},
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
	if method == "GET" && isResourcePath(path) {
		// Static read-only browser shell. Resource routes are NOT management-
		// authenticated, so this handler exposes no snapshot, performs no
		// mutations, and accepts no operations regardless of query parameters.
		// The shell's embedded script may upgrade the view client-side by
		// fetching the authenticated management HTML route, but only with
		// credentials the browser itself already holds.
		return okEnvelope(htmlResponse(200, renderStatusPage()))
	}

	r.mu.Lock()
	eng := r.engine
	r.mu.Unlock()
	if eng == nil {
		return okEnvelope(jsonResponse(503, map[string]string{"error": "plugin is not registered yet"}))
	}

	switch {
	case method == "GET" && strings.HasSuffix(path, managementStatusPagePath) && !isResourcePath(path):
		// Authenticated browser HTML status view. Renders only the published,
		// sanitized snapshot and the router's status through an auto-escaping
		// template; the account label (which may carry an email) is never
		// rendered.
		now := r.clk.Now().UTC()
		page, errPage := renderManagementStatusPage(eng.Status(), r.router.Status(now), now)
		if errPage != nil {
			return okEnvelope(jsonResponse(500, map[string]string{"error": "render status page failed"}))
		}
		return okEnvelope(htmlResponse(200, page))

	case method == "GET" && strings.HasSuffix(path, managementStatusPath) && !isResourcePath(path):
		return okEnvelope(jsonResponse(200, statusResponse{Snapshot: eng.Status(), Router: r.router.Status(r.clk.Now().UTC())}))

	case method == "POST" && strings.HasSuffix(path, managementRefreshPath) && !isResourcePath(path):
		// Authenticated "Refresh now" action (spec section 15). The custom
		// non-simple header blocks cross-origin form submissions when a reverse
		// proxy makes CPA management authentication ambient. Browser requests must
		// also be explicitly same-origin (or navigation-origin "none") so a sibling
		// site cannot use CPA's broad CORS preflight behavior to ride proxy auth.
		if !refreshRequestAllowed(req.Headers) {
			return okEnvelope(jsonResponse(403, map[string]string{
				"status": "forbidden",
				"detail": "refresh requires the plugin CSRF request header and, from a browser, same-origin fetch metadata or a plain-HTTP same-origin page",
			}))
		}
		ctx := hostapi.WithHostCallbackID(context.Background(), req.HostCallbackID)
		switch eng.Reconcile(ctx) {
		case engine.ReconcileResultSuccess:
			return okEnvelope(jsonResponse(200, map[string]string{
				"status": "ok",
				"detail": "reconciliation completed",
			}))
		case engine.ReconcileResultNoOp:
			return okEnvelope(jsonResponse(409, map[string]string{
				"status": "no_op",
				"detail": "reconciliation did not complete because the plugin is disabled or stopping",
			}))
		default:
			return okEnvelope(jsonResponse(503, map[string]string{
				"status": "error",
				"detail": "roster reconciliation deferred; existing roster retained",
			}))
		}

	default:
		return okEnvelope(jsonResponse(404, map[string]string{"error": "unknown route"}))
	}
}

// statusResponse is the JSON status: the engine's snapshot with the API
// credit router's status beside it, computed at the request's time. Snapshot
// has no router key, so the addition breaks no reader.
type statusResponse struct {
	engine.Snapshot
	Router route.Status `json:"router"`
}

// refreshRequestAllowed gates the mutating refresh route.
//
// The plugin CSRF header is always required. It makes the request non-simple,
// so a cross-origin HTML form cannot submit it, and it forces a CORS preflight,
// which browsers refuse to send with credentials against CPA's wildcard
// Access-Control-Allow-Origin.
//
// Browsers attach Fetch Metadata (Sec-Fetch-Site) only when the request URL is
// potentially trustworthy: https, or a loopback host. When present it is
// authoritative and only same-origin and navigation ("none") are accepted.
//
// When CPA is reached over plain HTTP on any other host, even a current browser
// sends no fetch metadata, so Origin is the only browser-controlled signal
// left. An http:// Origin is consistent with the legitimate status page: a
// browser can only post to a plain-HTTP server from a plain-HTTP page, because
// mixed-content blocking stops https pages from fetching http URLs. It is
// accepted. An https://, "null", or malformed Origin without fetch metadata
// cannot come from a supported browser talking to this route and is rejected.
// Requests carrying neither Origin nor fetch metadata are non-browser
// management clients and are accepted.
func refreshRequestAllowed(headers map[string][]string) bool {
	if !headerHasToken(headers, refreshRequestHeader, refreshRequestHeaderValue) {
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
		{Name: "enabled", Type: "boolean", Description: "Enable the reset-priority plugin."},
		{Name: "priority", Type: "integer", Description: "CPA plugin load/order priority. NOT related to the credential priorities this plugin manages."},
		{Name: "reconcile-interval", Type: "string", Description: "Background reconciliation interval (default 1h). Exact reset deadlines fire independently. Without the quota cache each pass makes one provider request per account, so do not shorten it."},
		{Name: "request-timeout", Type: "string", Description: "Per provider quota request timeout (default 10s)."},
		{Name: "priority-floor", Type: "integer", Description: "Priority of the latest-resetting healthy account (default 100)."},
		{Name: "priority-step", Type: "integer", Description: "Priority gap between adjacent ranks (default 100)."},
		{Name: "quarantine-priority", Type: "integer", Description: "Sentinel priority for disabled/reauth-required accounts (default 0)."},
		{Name: "manage-claude", Type: "boolean", Description: "Manage Claude OAuth credentials. Default true; this form shows it off until you save it once."},
		{Name: "manage-codex", Type: "boolean", Description: "Manage Codex OAuth credentials. Default true; this form shows it off until you save it once."},
		{Name: "dry-run", Type: "boolean", Description: "Compute and report priorities without writing auth files (default false; recommended true for first install)."},
		{Name: "use-quota-cache", Type: "boolean", Description: "Use shared quota observations instead of direct requests. Off by default, but on whenever quota-cache-path is set and this is unset, and this form shows it off then. To keep standalone polling with a custom quota-cache-path, switch this on and off again and save, so false is written. When on, wait for fresh data if the cache is unavailable; unconfirmed accounts are re-read from the local snapshot from +5s to +15m after each reconciliation."},
		{Name: "quota-cache-path", Type: "string", Description: "Optional shared snapshot path; default plugins/data/quota-cache/snapshot.json when use-quota-cache is on. While use-quota-cache is unset, setting this path turns it on too; use-quota-cache saved as false overrides that. The API credit router reads the files Quota Cache keeps beside this path, or the default, whatever use-quota-cache says."},
		{Name: "display-timezone", Type: "string", Description: "IANA time zone for timestamps on the HTML status view, e.g. America/Los_Angeles, or local to use the host zone (default UTC). Presentation only."},
		{Name: "route-api-credits", Type: "boolean", Description: "For each request attempt where CPA offers only Claude API keys from its configuration, use the Console organization whose monthly API credit refills soonest and has credit left; default false. Needs Quota Cache 0.2.0 or newer; check the status page before turning it on. Every other attempt is left to CPA"},
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

func htmlResponse(status int, body []byte) hostapi.ManagementResponse {
	return hostapi.ManagementResponse{
		StatusCode: status,
		Headers: map[string][]string{
			"Content-Type":            {"text/html; charset=utf-8"},
			"Cache-Control":           {"no-store"},
			"X-Content-Type-Options":  {"nosniff"},
			"Content-Security-Policy": {"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"},
		},
		Body: body,
	}
}

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
