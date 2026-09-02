package plugin

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/clock"
	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/engine"
	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/hostapi"
)

var t0 = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

type fakeHost struct {
	t    *testing.T
	mu   sync.Mutex
	logs []string
}

func (h *fakeHost) Call(method string, request []byte) ([]byte, error) {
	if method == hostapi.MethodHostLog {
		var req hostapi.LogRequest
		if err := json.Unmarshal(request, &req); err != nil {
			h.t.Errorf("host.log request is not JSON: %v: %s", err, request)
		}
		h.mu.Lock()
		h.logs = append(h.logs, req.Level+": "+req.Message)
		h.mu.Unlock()
	}
	return []byte(`{"ok":true}`), nil
}

func (h *fakeHost) logged(sub string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, l := range h.logs {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return raw
}

func mustUnmarshal(t *testing.T, raw []byte, out any) {
	t.Helper()
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("unmarshal into %T: %v: %s", out, err, raw)
	}
}

type fixture struct {
	t      *testing.T
	host   *fakeHost
	rt     *Runtime
	config string
	state  string
}

// baseConfig enables the plugin on disk (the worker refuses to write when
// plugins.enabled / plugins.configs.auto-baseline.enabled is not true).
const baseConfig = "port: 8317\napi-keys: [\"k\"]\nplugins:\n  enabled: true\n  configs:\n    auto-baseline:\n      enabled: true\n"

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(baseConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	host := &fakeHost{t: t}
	rt := NewRuntime(host, WithClock(clock.NewFake(t0)), WithRunAsync(func(f func()) { f() }))
	return &fixture{t: t, host: host, rt: rt, config: cfgPath, state: filepath.Join(dir, "state")}
}

func (f *fixture) pluginYAML(extra string) string {
	return "enabled: true\npriority: 10\nconfig-path: " + f.config + "\nstate-dir: " + f.state + "\npromotion-cooldown: 0s\n" + extra
}

func (f *fixture) lifecycle(method, yaml string, schema uint32) hostapi.Envelope {
	raw := mustMarshal(f.t, map[string]any{
		"config_yaml":    base64.StdEncoding.EncodeToString([]byte(yaml)),
		"schema_version": schema,
	})
	return decode(f.t, f.rt.Dispatch(method, raw))
}

func (f *fixture) register(extra string) hostapi.Envelope {
	return f.lifecycle(hostapi.MethodPluginRegister, f.pluginYAML(extra), hostapi.SchemaVersion)
}

func (f *fixture) intercept(headers map[string][]string) hostapi.Envelope {
	raw := mustMarshal(f.t, hostapi.RequestInterceptRequest{
		RequestID:      "req-1",
		SourceFormat:   "claude",
		Model:          "claude-opus-4-1",
		Headers:        headers,
		Body:           json.RawMessage(`"` + base64.StdEncoding.EncodeToString([]byte(`{"model":"x"}`)) + `"`),
		Metadata:       json.RawMessage(`{"k":"v"}`),
		HostCallbackID: "cb-1",
	})
	return decode(f.t, f.rt.Dispatch(hostapi.MethodRequestInterceptBefore, raw))
}

func (f *fixture) manage(method, path string, headers map[string][]string, body []byte) hostapi.ManagementResponse {
	f.t.Helper()
	raw := mustMarshal(f.t, hostapi.ManagementRequest{Method: method, Path: path, Headers: headers, Body: body})
	env := decode(f.t, f.rt.Dispatch(hostapi.MethodManagementHandle, raw))
	if !env.OK {
		f.t.Fatalf("management envelope error: %+v", env.Error)
	}
	var resp hostapi.ManagementResponse
	mustUnmarshal(f.t, env.Result, &resp)
	return resp
}

func (f *fixture) readConfig() string {
	f.t.Helper()
	raw, err := os.ReadFile(f.config)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(raw)
}

func (f *fixture) statusJSON() engine.Snapshot {
	f.t.Helper()
	resp := f.manage("GET", mgmtPrefix+managementStatusPath, nil, nil)
	if resp.StatusCode != 200 {
		f.t.Fatalf("status = %d %s", resp.StatusCode, resp.Body)
	}
	var snap engine.Snapshot
	mustUnmarshal(f.t, resp.Body, &snap)
	return snap
}

func (f *fixture) claudePending() int {
	f.t.Helper()
	for _, p := range f.statusJSON().Baselines {
		if p.Provider == "claude" {
			return len(p.Pending)
		}
	}
	f.t.Fatal("claude provider missing from status")
	return 0
}

func decode(t *testing.T, raw []byte) hostapi.Envelope {
	t.Helper()
	var env hostapi.Envelope
	mustUnmarshal(t, raw, &env)
	return env
}

func claudeHeaders(session string) map[string][]string {
	return map[string][]string{
		"User-Agent":                  {"claude-cli/2.1.258 (external, sdk-ts, agent-sdk/0.3.170)"},
		"X-App":                       {"cli"},
		"Anthropic-Version":           {"2023-06-01"},
		"Anthropic-Beta":              {"claude-code-20250219,oauth-2025-04-20"},
		"X-Stainless-Lang":            {"js"},
		"X-Stainless-Runtime":         {"node"},
		"X-Stainless-Package-Version": {"0.112.1"},
		"X-Stainless-Runtime-Version": {"v26.3.0"},
		"X-Stainless-Os":              {"Linux"},
		"X-Stainless-Arch":            {"x64"},
		"X-Claude-Code-Session-Id":    {session},
		"Authorization":               {"Bearer smoke-key"},
	}
}

func csrfHeaders(extra map[string][]string) map[string][]string {
	out := map[string][]string{mutationRequestHeader: {mutationRequestHeaderValue}}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

const mgmtPrefix = "/v0/management"

// registration is the typed view of the plugin.register result.
type registration struct {
	SchemaVersion uint32 `json:"schema_version"`
	Metadata      struct {
		Name         string `json:"Name"`
		Version      string `json:"Version"`
		Author       string `json:"Author"`
		ConfigFields []struct {
			Name string `json:"Name"`
			Type string `json:"Type"`
		} `json:"ConfigFields"`
	} `json:"metadata"`
	Capabilities map[string]bool `json:"capabilities"`
}

func TestRegisterReturnsMetadataAndCapabilities(t *testing.T) {
	f := newFixture(t)
	env := f.register("")
	if !env.OK {
		t.Fatalf("register failed: %+v", env.Error)
	}
	var reg registration
	mustUnmarshal(t, env.Result, &reg)
	if reg.SchemaVersion != hostapi.SchemaVersion {
		t.Errorf("schema = %d", reg.SchemaVersion)
	}
	if reg.Metadata.Name != PluginName || reg.Metadata.Version != PluginVersion || reg.Metadata.Author != PluginAuthor {
		t.Errorf("metadata = %+v", reg.Metadata)
	}
	if !reg.Capabilities["request_interceptor"] || !reg.Capabilities["management_api"] {
		t.Errorf("capabilities = %v", reg.Capabilities)
	}
	for _, forbidden := range []string{"request_lifecycle_plugin", "scheduler", "executor"} {
		if _, ok := reg.Capabilities[forbidden]; ok {
			t.Errorf("capability %q must not be declared", forbidden)
		}
	}
	names := map[string]bool{}
	for _, fld := range reg.Metadata.ConfigFields {
		names[fld.Name] = true
	}
	for _, want := range []string{"enabled", "priority", "dry-run", "config-path", "state-dir", "backup-dir", "manage-claude", "manage-codex", "claude-entrypoints", "require-claude-code-beta", "min-observations", "min-distinct-sessions", "observation-window", "promotion-cooldown", "claude-min-version", "codex-min-version", "display-timezone"} {
		if !names[want] {
			t.Errorf("config field %q missing", want)
		}
	}
	// Casing check on the raw wire shape: untagged upstream structs use
	// capitalized keys; the registration wrapper uses lowercase.
	var raw map[string]json.RawMessage
	mustUnmarshal(t, env.Result, &raw)
	if _, ok := raw["metadata"]; !ok {
		t.Errorf("registration wrapper keys = %v", raw)
	}
	if !strings.Contains(string(raw["metadata"]), `"ConfigFields"`) {
		t.Errorf("metadata keys must be capitalized: %s", raw["metadata"])
	}
	if !f.host.logged("learning enabled") {
		t.Errorf("start not logged: %v", f.host.logs)
	}
}

func TestRegisterInvalidConfigRejected(t *testing.T) {
	f := newFixture(t)
	env := f.register("min-observations: 0\n")
	if env.OK || env.Error == nil || env.Error.Code != "invalid_config" {
		t.Fatalf("envelope = %+v", env)
	}
	env = f.register("dry_run: true\n")
	if env.OK || env.Error == nil || !strings.Contains(env.Error.Message, "dry_run") {
		t.Fatalf("misspelled key accepted: %+v", env)
	}
	env = decode(t, f.rt.Dispatch(hostapi.MethodPluginRegister, []byte("{nope")))
	if env.OK || env.Error == nil || env.Error.Code != "invalid_request" {
		t.Fatalf("envelope = %+v", env)
	}
}

func TestSchemaNegotiation(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct{ offered, want uint32 }{{0, 1}, {2, 2}, {4, 4}, {9, 4}} {
		env := f.lifecycle(hostapi.MethodPluginRegister, f.pluginYAML(""), tc.offered)
		if !env.OK {
			t.Fatalf("offered %d: %+v", tc.offered, env.Error)
		}
		var reg registration
		mustUnmarshal(t, env.Result, &reg)
		if reg.SchemaVersion != tc.want {
			t.Errorf("offered %d -> %d, want %d", tc.offered, reg.SchemaVersion, tc.want)
		}
	}
}

func TestInterceptObservesAndReturnsNoop(t *testing.T) {
	f := newFixture(t)
	f.register("")
	for _, s := range []string{"s1", "s2"} {
		env := f.intercept(claudeHeaders(s))
		if !env.OK || strings.TrimSpace(string(env.Result)) != "{}" {
			t.Fatalf("intercept response must be an empty object: %s", env.Result)
		}
	}
	if strings.Contains(f.readConfig(), "claude-header-defaults") {
		t.Fatal("promoted before quorum")
	}
	f.intercept(claudeHeaders("s1"))
	text := f.readConfig()
	if !strings.Contains(text, `user-agent: "claude-cli/2.1.258 (external, cli)"`) || !strings.Contains(text, `package-version: "0.112.1"`) {
		t.Errorf("config after quorum:\n%s", text)
	}
	if !f.host.logged("promoted claude baseline 2.1.220 -> 2.1.258") {
		t.Errorf("promotion not logged: %v", f.host.logs)
	}
	env := decode(t, f.rt.Dispatch(hostapi.MethodRequestInterceptAfter, []byte(`{"Headers":{}}`)))
	if !env.OK || strings.TrimSpace(string(env.Result)) != "{}" {
		t.Errorf("after-auth = %s", env.Result)
	}
	// The precomputed envelope decodes as an empty RequestInterceptResponse.
	var resp hostapi.RequestInterceptResponse
	mustUnmarshal(t, env.Result, &resp)
	if resp.Headers != nil || resp.Terminate || len(resp.Body) != 0 {
		t.Errorf("noop response is not empty: %+v", resp)
	}
}

func TestInterceptNeverErrorsOnBadInput(t *testing.T) {
	f := newFixture(t)
	for _, raw := range [][]byte{nil, []byte("{bad"), []byte(`{"Headers":"not a map"}`), []byte(`{"Headers":null}`)} {
		env := decode(t, f.rt.Dispatch(hostapi.MethodRequestInterceptBefore, raw))
		if !env.OK {
			t.Errorf("input %q produced an error envelope: %+v", raw, env.Error)
		}
	}
	f.register("")
	env := decode(t, f.rt.Dispatch(hostapi.MethodRequestInterceptBefore, []byte(`{"Headers":{"User-Agent":["curl"]}}`)))
	if !env.OK {
		t.Errorf("curl request produced error: %+v", env.Error)
	}
}

func TestDispatchUnknownMethodAndNilRuntime(t *testing.T) {
	f := newFixture(t)
	f.register("")
	var nilRT *Runtime
	env := decode(t, nilRT.Dispatch("x", nil))
	if env.OK || env.Error == nil || env.Error.Code != "plugin_shutdown" {
		t.Errorf("nil runtime dispatch = %+v", env)
	}
	env = decode(t, f.rt.Dispatch("bogus.method", nil))
	if env.OK || env.Error == nil || env.Error.Code != "unknown_method" {
		t.Errorf("unknown method = %+v", env)
	}
}

func TestDispatchRecoversFromHandlerPanic(t *testing.T) {
	f := newFixture(t)
	f.register("")
	// Swap in an engine-less runtime state that makes the management path
	// panic: a nil engine pointer stored behind a non-nil runtime would be
	// caught earlier, so inject a panicking status renderer instead.
	prev := renderStatusPageForTest
	renderStatusPageForTest = func() []byte { panic("boom in resource page") }
	defer func() { renderStatusPageForTest = prev }()
	raw := mustMarshal(t, hostapi.ManagementRequest{Method: "GET", Path: "/v0/resource/plugins/auto-baseline/status"})
	env := decode(t, f.rt.Dispatch(hostapi.MethodManagementHandle, raw))
	if env.OK || env.Error == nil || env.Error.Code != "plugin_panic" || !strings.Contains(env.Error.Message, "boom in resource page") {
		t.Fatalf("panic not converted to an error envelope: %+v", env)
	}
	// The runtime keeps working afterwards.
	if resp := f.manage("GET", mgmtPrefix+managementStatusPath, nil, nil); resp.StatusCode != 200 {
		t.Errorf("status after panic = %d", resp.StatusCode)
	}
}

func TestQuiesceAndReconfigureResume(t *testing.T) {
	f := newFixture(t)
	f.register("")
	env := decode(t, f.rt.Dispatch(hostapi.MethodPluginQuiesce, nil))
	if !env.OK {
		t.Fatal("quiesce failed")
	}
	f.intercept(claudeHeaders("a"))
	status := f.statusJSON()
	if !status.Stopped || status.Counters.Requests != 0 {
		t.Errorf("quiesced runtime still observing: %+v", status.Counters)
	}
	env = f.lifecycle(hostapi.MethodPluginReconfigure, f.pluginYAML("min-observations: 2\nmin-distinct-sessions: 1\n"), hostapi.SchemaVersion)
	if !env.OK {
		t.Fatalf("reconfigure: %+v", env.Error)
	}
	f.intercept(claudeHeaders("a"))
	f.intercept(claudeHeaders("a"))
	if !strings.Contains(f.readConfig(), "2.1.258") {
		t.Error("learning did not resume after reconfigure")
	}
	env = f.lifecycle(hostapi.MethodPluginReconfigure, "enabled: false\nstate-dir: "+f.state+"\n", hostapi.SchemaVersion)
	if !env.OK {
		t.Fatalf("disable reconfigure: %+v", env.Error)
	}
	if s := f.statusJSON(); !s.Stopped || s.Enabled {
		t.Errorf("status after disable = stopped=%t enabled=%t", s.Stopped, s.Enabled)
	}
}

func TestShutdownGatesDispatch(t *testing.T) {
	f := newFixture(t)
	f.register("")
	f.intercept(claudeHeaders("a"))
	f.rt.Shutdown()
	env := decode(t, f.rt.Dispatch(hostapi.MethodRequestInterceptBefore, nil))
	if env.OK || env.Error == nil || env.Error.Code != "plugin_shutdown" {
		t.Errorf("post-shutdown dispatch = %+v", env)
	}
	if _, err := os.Stat(filepath.Join(f.state, "state.json")); err != nil {
		t.Errorf("state not saved on shutdown: %v", err)
	}
	f.rt.Shutdown() // idempotent
}

func TestManagementRegistrationRouteCasing(t *testing.T) {
	f := newFixture(t)
	env := decode(t, f.rt.Dispatch(hostapi.MethodManagementRegister, nil))
	if !env.OK {
		t.Fatal("management.register failed")
	}
	// Raw map only for the casing check.
	var raw struct {
		Routes    []map[string]any `json:"routes"`
		Resources []map[string]any `json:"resources"`
	}
	mustUnmarshal(t, env.Result, &raw)
	if len(raw.Routes) != 4 {
		t.Fatalf("routes = %v", raw.Routes)
	}
	for _, m := range raw.Routes {
		if _, ok := m["Method"]; !ok {
			t.Errorf("route missing capitalized Method: %v", m)
		}
		if m["Method"] == "GET" && m["Menu"] != nil {
			t.Errorf("authenticated GET route must not declare Menu: %v", m)
		}
	}
	if len(raw.Resources) != 1 {
		t.Fatalf("resources = %v", raw.Resources)
	}
	if res := raw.Resources[0]; res["Path"] != resourceStatusPath || res["Menu"] != "Auto Baseline" {
		t.Errorf("resource = %v", res)
	}
}

func TestManagementBeforeRegistrationReturns503(t *testing.T) {
	f := newFixture(t)
	if resp := f.manage("GET", mgmtPrefix+managementStatusPath, nil, nil); resp.StatusCode != 503 {
		t.Errorf("status = %d %s", resp.StatusCode, resp.Body)
	}
	resp := f.manage("GET", "/v0/resource/plugins/auto-baseline/status", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(resp.Body), "<title>Auto Baseline</title>") {
		t.Errorf("resource = %d %s", resp.StatusCode, resp.Body)
	}
}

func TestStatusJSONHasNoSecrets(t *testing.T) {
	f := newFixture(t)
	f.register("")
	f.intercept(claudeHeaders("11111111-2222-3333-4444-555555555555"))
	resp := f.manage("GET", mgmtPrefix+managementStatusPath, nil, nil)
	body := string(resp.Body)
	if resp.StatusCode != 200 || len(resp.Headers["Content-Type"]) == 0 || resp.Headers["Content-Type"][0] != "application/json; charset=utf-8" {
		t.Fatalf("resp = %d %v", resp.StatusCode, resp.Headers)
	}
	for _, leak := range []string{"smoke-key", "Bearer", "11111111-2222", "api-keys"} {
		if strings.Contains(body, leak) {
			t.Errorf("status leaks %q: %s", leak, body)
		}
	}
	var snap engine.Snapshot
	mustUnmarshal(t, resp.Body, &snap)
	if snap.Plugin != PluginID || snap.Version != PluginVersion || len(snap.Baselines) != 2 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if len(snap.Baselines[0].Pending) != 1 || snap.Baselines[0].Pending[0].Observations != 1 || snap.Baselines[0].Pending[0].DistinctSessions != 1 {
		t.Errorf("pending = %+v", snap.Baselines[0].Pending)
	}
}

func TestStatusHTMLRendersAndEscapes(t *testing.T) {
	f := newFixture(t)
	f.register("display-timezone: America/Los_Angeles\ndry-run: true\n")
	f.intercept(claudeHeaders("a"))
	resp := f.manage("GET", mgmtPrefix+managementStatusPagePath, nil, nil)
	if resp.StatusCode != 200 || len(resp.Headers["Content-Type"]) == 0 || !strings.HasPrefix(resp.Headers["Content-Type"][0], "text/html") {
		t.Fatalf("resp = %d %v", resp.StatusCode, resp.Headers)
	}
	body := string(resp.Body)
	for _, want := range []string{"<title>Auto Baseline", "dry-run", "America/Los_Angeles", "2.1.258", "claude-cli/2.1.220 (external, cli)", "disable-codex-cloaking", "X-Auto-Baseline-Request", "PDT", "Assumed CPA build", "floor 2.1.220", "backup dir writable"} {
		if !strings.Contains(body, want) {
			t.Errorf("HTML missing %q", want)
		}
	}
	if resp.Headers["Content-Security-Policy"] == nil {
		t.Error("CSP header missing")
	}
	resp = f.manage("GET", "/v0/resource/plugins/auto-baseline/status/html", nil, nil)
	if !strings.Contains(string(resp.Body), `data-auto-baseline-status="ready"`) || strings.Contains(string(resp.Body), "2.1.258") {
		t.Error("resource path leaked the authenticated view")
	}
}

func TestRenderManagementStatusPageEscapesRenderedFields(t *testing.T) {
	snap := engine.Snapshot{
		GeneratedAt: t0,
		LastError:   `<script>alert("x")</script>`,
		LastErrorAt: t0,
		Warnings:    []string{"<img src=x onerror=alert(1)>"},
		Config:      engine.ConfigStatus{Path: "/etc/<b>cpa</b>/config.yaml", Error: "<i>nope</i>"},
	}
	page, err := renderManagementStatusPage(snap)
	if err != nil {
		t.Fatal(err)
	}
	body := string(page)
	for _, raw := range []string{`<script>alert("x")</script>`, "<img src=x", "<b>cpa</b>", "<i>nope</i>"} {
		if strings.Contains(body, raw) {
			t.Errorf("unescaped %q in HTML", raw)
		}
	}
	for _, escaped := range []string{"&lt;script&gt;", "&lt;img src=x", "&lt;b&gt;cpa&lt;/b&gt;"} {
		if !strings.Contains(body, escaped) {
			t.Errorf("escaped form %q missing", escaped)
		}
	}
	if !strings.Contains(body, "No promotions yet") {
		t.Error("empty snapshot did not render")
	}
	if relativeTime(t0.Add(90*time.Second), t0) != "in 1m" || relativeTime(t0.Add(-25*time.Hour), t0) != "1d 1h ago" || relativeTime(t0, t0) != "now" {
		t.Error("relativeTime formatting")
	}
	if displayZoneName("local", time.UTC) != "host local time (UTC)" || displayZoneName("", nil) != "UTC" {
		t.Error("displayZoneName")
	}
}

func TestResourceShellIsStatic(t *testing.T) {
	f := newFixture(t)
	f.register("")
	a := f.manage("GET", "/v0/resource/plugins/auto-baseline/status", csrfHeaders(nil), nil)
	f.intercept(claudeHeaders("a"))
	b := f.manage("GET", "/v0/resource/plugins/auto-baseline/status?reset=1", nil, nil)
	if string(a.Body) != string(b.Body) || string(a.Body) != string(renderStatusPage()) {
		t.Error("resource shell is not static")
	}
	if strings.Contains(string(a.Body), "2.1.258") || strings.Contains(string(a.Body), f.config) {
		t.Error("resource shell exposes data")
	}
	if !strings.Contains(string(a.Body), "autoBaselineAuth") || !strings.Contains(string(a.Body), `managementPath("/status/html")`) {
		t.Error("same-origin upgrade script missing")
	}
	if f.claudePending() != 1 {
		t.Fatal("precondition: one pending candidate")
	}
	resp := f.manage("POST", "/v0/resource/plugins/auto-baseline/reset", csrfHeaders(nil), nil)
	if resp.StatusCode == 200 && f.claudePending() == 0 {
		t.Error("resource path performed a mutation")
	}
}

func TestMutationCSRFGate(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string][]string
		want    int
	}{
		{"no header", nil, 403},
		{"header only (non-browser client)", csrfHeaders(nil), 200},
		{"lowercase header", map[string][]string{"x-auto-baseline-request": {"1"}}, 200},
		{"wrong header value", map[string][]string{mutationRequestHeader: {"yes"}}, 403},
		{"same-origin fetch", csrfHeaders(map[string][]string{"Sec-Fetch-Site": {"same-origin"}}), 200},
		{"none fetch", csrfHeaders(map[string][]string{"Sec-Fetch-Site": {"none"}}), 200},
		{"same-origin with https origin", csrfHeaders(map[string][]string{"Sec-Fetch-Site": {"same-origin"}, "Origin": {"https://cpa.example"}}), 200},
		{"cross-site", csrfHeaders(map[string][]string{"Sec-Fetch-Site": {"cross-site"}}), 403},
		{"same-site sibling", csrfHeaders(map[string][]string{"Sec-Fetch-Site": {"same-site"}}), 403},
		{"empty fetch site", csrfHeaders(map[string][]string{"Sec-Fetch-Site": {""}}), 403},
		{"unknown fetch site", csrfHeaders(map[string][]string{"Sec-Fetch-Site": {"unknown"}}), 403},
		{"mixed fetch site", csrfHeaders(map[string][]string{"Sec-Fetch-Site": {"same-origin, cross-site"}}), 403},
		// Plain-HTTP fallback: no fetch metadata, Origin is the only signal.
		{"plain http origin tailnet", csrfHeaders(map[string][]string{"Origin": {"http://vps-code.example.ts.net:8317"}, "Content-Length": {"0"}, "Accept": {"*/*"}}), 200},
		{"plain http origin ip", csrfHeaders(map[string][]string{"Origin": {"http://192.0.2.10:8317"}}), 200},
		{"plain http origin uppercase", csrfHeaders(map[string][]string{"Origin": {"HTTP://CPA.EXAMPLE"}}), 200},
		{"plain http origin without csrf header", map[string][]string{"Origin": {"http://cpa.example:8317"}}, 403},
		{"https origin without metadata", csrfHeaders(map[string][]string{"Origin": {"https://unknown-origin.example"}}), 403},
		{"null origin", csrfHeaders(map[string][]string{"Origin": {"null"}}), 403},
		{"empty origin", csrfHeaders(map[string][]string{"Origin": {""}}), 403},
		{"origin no host", csrfHeaders(map[string][]string{"Origin": {"http://"}}), 403},
		{"origin with userinfo", csrfHeaders(map[string][]string{"Origin": {"http://user:pw@cpa.example:8317"}}), 403},
		{"origin with path", csrfHeaders(map[string][]string{"Origin": {"http://cpa.example:8317/path"}}), 403},
		{"origin with query", csrfHeaders(map[string][]string{"Origin": {"http://cpa.example:8317?q=1"}}), 403},
		{"ftp origin", csrfHeaders(map[string][]string{"Origin": {"ftp://cpa.example"}}), 403},
		{"two origins", csrfHeaders(map[string][]string{"Origin": {"http://a.example, http://b.example"}}), 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.register("")
			f.intercept(claudeHeaders("a"))
			resp := f.manage("POST", mgmtPrefix+managementResetPath, tc.headers, nil)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.want, resp.Body)
			}
			pending := f.claudePending()
			if tc.want == 200 && pending != 0 {
				t.Errorf("pending not cleared")
			}
			if tc.want == 403 && pending == 0 {
				t.Errorf("forbidden request still mutated")
			}
		})
	}
}

func TestObserveRoute(t *testing.T) {
	f := newFixture(t)
	f.register("")
	body := []byte(`{"provider":"claude","user_agent":"claude-cli/2.1.258 (external, cli)","package_version":"0.112.1","runtime_version":"v26.3.0","os":"Linux","arch":"x64","session_id":"host-a"}`)
	if resp := f.manage("POST", mgmtPrefix+managementObservePath, nil, body); resp.StatusCode != 403 {
		t.Errorf("no CSRF header -> %d %s", resp.StatusCode, resp.Body)
	}
	resp := f.manage("POST", mgmtPrefix+managementObservePath, csrfHeaders(nil), body)
	if resp.StatusCode != 202 {
		t.Fatalf("observe -> %d %s", resp.StatusCode, resp.Body)
	}
	var out engine.ReportOutcome
	mustUnmarshal(t, resp.Body, &out)
	if !out.Accepted || out.Decision != "tracked" || out.Queued {
		t.Errorf("outcome = %+v", out)
	}
	resp = f.manage("POST", mgmtPrefix+managementObservePath, csrfHeaders(nil), []byte(`{"provider":"claude","user_agent":"claude-cli/2.1.258 (external, cli)","package_version":"x"}`))
	if resp.StatusCode != 422 || !strings.Contains(string(resp.Body), "claude_package_version_malformed") {
		t.Errorf("invalid -> %d %s", resp.StatusCode, resp.Body)
	}
	if resp = f.manage("POST", mgmtPrefix+managementObservePath, csrfHeaders(nil), []byte("{")); resp.StatusCode != 400 {
		t.Errorf("bad json -> %d %s", resp.StatusCode, resp.Body)
	}
	if resp = f.manage("POST", mgmtPrefix+managementObservePath, csrfHeaders(nil), []byte(strings.Repeat("x", maxObserveBodyBytes+1))); resp.StatusCode != 413 {
		t.Errorf("oversize -> %d %s", resp.StatusCode, resp.Body)
	}
	resp = f.manage("POST", mgmtPrefix+managementObservePath, csrfHeaders(nil), []byte(`{"provider":"codex","user_agent":"codex-tui/0.152.1 (Ubuntu 24.4.0; x86_64) WezTerm/1 (codex-tui; 0.152.1)","force":true}`))
	mustUnmarshal(t, resp.Body, &out)
	if resp.StatusCode != 202 || !out.Queued || !strings.Contains(out.Reason, "queued") {
		t.Errorf("force -> %d %+v", resp.StatusCode, out)
	}
	if !strings.Contains(f.readConfig(), "codex-tui/0.152.1") {
		t.Errorf("forced promotion missing:\n%s", f.readConfig())
	}
}

func TestUnknownManagementRoute(t *testing.T) {
	f := newFixture(t)
	f.register("")
	if resp := f.manage("GET", mgmtPrefix+"/plugins/auto-baseline/nope", nil, nil); resp.StatusCode != 404 {
		t.Errorf("unknown -> %d %s", resp.StatusCode, resp.Body)
	}
	if resp := f.manage("DELETE", mgmtPrefix+managementStatusPath, nil, nil); resp.StatusCode != 404 {
		t.Errorf("wrong method -> %d %s", resp.StatusCode, resp.Body)
	}
	env := decode(t, f.rt.Dispatch(hostapi.MethodManagementHandle, []byte("{bad")))
	if env.OK || env.Error == nil || env.Error.Code != "invalid_request" {
		t.Errorf("bad request = %+v", env)
	}
}

func TestInvalidReconfigureQuiescesRunningEngine(t *testing.T) {
	f := newFixture(t)
	f.register("")
	f.intercept(claudeHeaders("a"))
	env := f.lifecycle(hostapi.MethodPluginReconfigure, f.pluginYAML("min-observations: 0\n"), hostapi.SchemaVersion)
	if env.OK || env.Error == nil || env.Error.Code != "invalid_config" {
		t.Fatalf("envelope = %+v", env)
	}
	// The host drops the plugin without quiesce; the old engine must already
	// be stopped so it can neither learn nor write.
	if s := f.statusJSON(); !s.Stopped {
		t.Errorf("engine still running after invalid reconfigure: stopped=%t", s.Stopped)
	}
	f.intercept(claudeHeaders("b"))
	f.intercept(claudeHeaders("a"))
	if strings.Contains(f.readConfig(), "2.1.258") {
		t.Error("write happened after an invalid reconfigure")
	}
	if _, err := os.Stat(filepath.Join(f.state, "state.json")); err != nil {
		t.Errorf("state not flushed on quiesce: %v", err)
	}
}

func TestEnableAfterDisabledAppliesNewConfigBeforeStart(t *testing.T) {
	f := newFixture(t)
	// Register disabled with the default (cwd-relative) state dir.
	env := f.lifecycle(hostapi.MethodPluginRegister, "enabled: false\n", hostapi.SchemaVersion)
	if !env.OK {
		t.Fatalf("register: %+v", env.Error)
	}
	if s := f.statusJSON(); !s.Stopped {
		t.Fatal("disabled registration started the engine")
	}
	// Enable with an explicit state dir: Start must use the NEW dir, never
	// touch the default one.
	env = f.lifecycle(hostapi.MethodPluginReconfigure, f.pluginYAML(""), hostapi.SchemaVersion)
	if !env.OK {
		t.Fatalf("reconfigure: %+v", env.Error)
	}
	s := f.statusJSON()
	if s.Stopped || s.State.Dir != f.state || s.Config.Path != f.config {
		t.Errorf("status after enable = stopped=%t state=%s config=%s", s.Stopped, s.State.Dir, s.Config.Path)
	}
	if _, err := os.Stat(filepath.Join("plugins", "auto-baseline")); err == nil {
		t.Error("default state dir was created in cwd")
	}
	for _, sess := range []string{"a", "b", "a"} {
		f.intercept(claudeHeaders(sess))
	}
	if !strings.Contains(f.readConfig(), "2.1.258") {
		t.Error("learning did not work after enable")
	}
}

func TestLifecycleCallsAreSerialized(t *testing.T) {
	f := newFixture(t)
	f.register("")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			f.rt.Dispatch(hostapi.MethodPluginQuiesce, nil)
		}()
		go func(i int) {
			defer wg.Done()
			extra := ""
			if i%2 == 0 {
				extra = "dry-run: true\n"
			}
			f.lifecycle(hostapi.MethodPluginReconfigure, f.pluginYAML(extra), hostapi.SchemaVersion)
		}(i)
		go func() {
			defer wg.Done()
			f.intercept(claudeHeaders("x"))
			f.manage("GET", mgmtPrefix+managementStatusPath, nil, nil)
		}()
	}
	wg.Wait()
	// Settle into a known state and confirm the runtime still works.
	env := f.lifecycle(hostapi.MethodPluginReconfigure, f.pluginYAML(""), hostapi.SchemaVersion)
	if !env.OK {
		t.Fatalf("final reconfigure: %+v", env.Error)
	}
	if s := f.statusJSON(); s.Stopped || s.Faulted {
		t.Errorf("runtime unhealthy after concurrent lifecycle: stopped=%t faulted=%t", s.Stopped, s.Faulted)
	}
	f.rt.Shutdown()
}
