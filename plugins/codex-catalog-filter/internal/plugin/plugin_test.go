package plugin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/protocol"
)

const codexCatalog = `{"models":[{"slug":"gpt-6-sol","display_name":"GPT-6 Sol","visibility":"list","base_instructions":"<b>&</b>"},{"slug":"claude-fable-5-1","display_name":"Claude Fable 5.1","visibility":"list"},{"slug":"or-kimi-k2","visibility":"list"},{"slug":"cpa-sonnet","visibility":"list"},{"slug":"gpt-5.5","visibility":"list"},{"slug":"codex-auto-review","visibility":"hide"}]}`

const clientKey = "Bearer client-key"

// fakeCPA stands in for CPA's /v1/models and records what the plugin sent.
type fakeCPA struct {
	*httptest.Server
	mu      sync.Mutex
	seen    []*http.Request
	respond func(http.ResponseWriter, *http.Request)
}

func newFakeCPA(t *testing.T) *fakeCPA {
	t.Helper()
	f := &fakeCPA{respond: func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != clientKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"Invalid API key"}`))
			return
		}
		_, _ = w.Write([]byte(codexCatalog))
	}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r.Clone(r.Context()))
		respond := f.respond
		f.mu.Unlock()
		respond(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeCPA) set(respond func(http.ResponseWriter, *http.Request)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respond = respond
}

func (f *fakeCPA) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen)
}

func (f *fakeCPA) last(t *testing.T) *http.Request {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seen) == 0 {
		t.Fatal("CPA was not called")
	}
	return f.seen[len(f.seen)-1]
}

func encode(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func lifecycle(t *testing.T, yaml string, schema uint32) []byte {
	return encode(t, protocol.LifecycleRequest{ConfigYAML: []byte(yaml), SchemaVersion: schema})
}

func configured(t *testing.T, yaml string) *Plugin {
	t.Helper()
	p := New()
	if _, err := p.Handle(protocol.MethodPluginRegister, lifecycle(t, yaml, protocol.SchemaVersion)); err != nil {
		t.Fatal(err)
	}
	return p
}

// switchedOff is the settings page's typical result: the non-GPT models off,
// new models enabled. Each test gets its own data directory.
func switchedOff(t *testing.T, cpa *fakeCPA, extra string) string {
	return "enabled: true\ncpa-url: " + cpa.URL + "\ndata-dir: " + t.TempDir() + "\nmodels:\n  claude-fable-5-1: false\n  or-kimi-k2: false\n  cpa-sonnet: false\n" + extra
}

// codexRequest is the resource request CPA hands the plugin when Codex
// fetches model_catalog_url.
func codexRequest() protocol.ManagementRequest {
	return protocol.ManagementRequest{
		Method: http.MethodGet,
		Path:   CatalogPath,
		Headers: http.Header{
			"Authorization":     {clientKey},
			"User-Agent":        {"codex_cli_rs/0.159.0"},
			"Anthropic-Version": {"2023-06-01"},
			"X-Private":         {"do-not-forward"},
		},
		Query: url.Values{"client_version": {"0.159.0"}},
	}
}

func stateRequest() protocol.ManagementRequest {
	return protocol.ManagementRequest{Method: http.MethodGet, Path: StatePath, Headers: http.Header{"Authorization": {"Bearer management-key"}}}
}

// serve round-trips through JSON exactly as the native boundary does.
func serve(t *testing.T, p *Plugin, req protocol.ManagementRequest) protocol.ManagementResponse {
	t.Helper()
	result, err := p.Handle(protocol.MethodManagementHandle, encode(t, req))
	if err != nil {
		t.Fatalf("management.handle returned an error: %v", err)
	}
	var out protocol.ManagementResponse
	if err := json.Unmarshal(encode(t, result), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

type state struct {
	CatalogPath string     `json:"catalog_path"`
	SeenAt      *time.Time `json:"seen_at"`
	Saved       bool       `json:"saved"`
	NewModels   string     `json:"new_models"`
	Action      string     `json:"action"`
	Models      []struct {
		Slug        string `json:"slug"`
		DisplayName string `json:"display_name"`
		Visibility  string `json:"visibility"`
		Enabled     bool   `json:"enabled"`
		Switched    bool   `json:"switched"`
	} `json:"models"`
	Switches map[string]bool `json:"switches"`
}

func readState(t *testing.T, p *Plugin) state {
	t.Helper()
	out := serve(t, p, stateRequest())
	if out.StatusCode != http.StatusOK || out.Headers.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("state = %d %s", out.StatusCode, out.Body)
	}
	var s state
	if err := json.Unmarshal(out.Body, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func slugs(t *testing.T, body []byte) []string {
	t.Helper()
	var d struct{ Models []struct{ Slug string } }
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatalf("invalid body %s: %v", body, err)
	}
	var out []string
	for _, m := range d.Models {
		out = append(out, m.Slug)
	}
	return out
}

func TestRegistrationAdvertisesOnlyTheManagementAPI(t *testing.T) {
	p := New()
	result, err := p.Handle(protocol.MethodPluginRegister, lifecycle(t, "enabled: true\n", protocol.SchemaVersion))
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		SchemaVersion uint32          `json:"schema_version"`
		Capabilities  map[string]bool `json:"capabilities"`
		Metadata      struct {
			Name, Version, Author, GitHubRepository string
			ConfigFields                            []struct{ Name, Type string }
		} `json:"metadata"`
	}
	if err := json.Unmarshal(encode(t, result), &reg); err != nil {
		t.Fatal(err)
	}
	if reg.SchemaVersion != 6 || len(reg.Capabilities) != 1 || !reg.Capabilities["management_api"] {
		t.Fatalf("registration = %+v", reg)
	}
	m := reg.Metadata
	if m.Name != "Codex Catalog Filter" || m.Version != Version || m.Author != "NoorChasib" || m.GitHubRepository != "https://github.com/NoorChasib/cpa-plugins" {
		t.Fatalf("metadata = %+v", m)
	}
	var fields []string
	for _, f := range m.ConfigFields {
		fields = append(fields, f.Name+":"+f.Type)
	}
	if !slices.Equal(fields, []string{"models:object", "new-models:enum", "action:enum", "cpa-url:string", "data-dir:string"}) {
		t.Fatalf("config fields = %v", fields)
	}
}

func TestManagementRegisterDeclaresRoutesAndResources(t *testing.T) {
	result, err := New().Handle(protocol.MethodManagementRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	var reg protocol.ManagementRegistration
	if err := json.Unmarshal(encode(t, result), &reg); err != nil {
		t.Fatal(err)
	}
	// A private GET with a Menu would become a public resource.
	if len(reg.Routes) != 1 || reg.Routes[0].Method != http.MethodGet || reg.Routes[0].Path != "/plugins/"+ID+"/state" || reg.Routes[0].Menu != "" {
		t.Fatalf("routes = %+v", reg.Routes)
	}
	if len(reg.Resources) != 2 || reg.Resources[0].Path != "/models" || reg.Resources[0].Menu != "" ||
		reg.Resources[1].Path != "/settings" || reg.Resources[1].Menu != "Codex Models" {
		t.Fatalf("resources = %+v", reg.Resources)
	}
}

func TestLifecycleRejectsOldSchemaAndBadConfig(t *testing.T) {
	p := New()
	if _, err := p.Handle(protocol.MethodPluginRegister, lifecycle(t, "enabled: true\n", 5)); err == nil {
		t.Fatal("schema 5 accepted")
	}
	if _, err := p.Handle(protocol.MethodPluginRegister, []byte("{")); err == nil {
		t.Fatal("malformed lifecycle accepted")
	}
	if _, err := p.Handle(protocol.MethodPluginRegister, lifecycle(t, "models: {a: maybe}\n", protocol.SchemaVersion)); err == nil {
		t.Fatal("non-boolean switch accepted")
	}
	if _, err := p.Handle("response.intercept_after", nil); err == nil {
		t.Fatal("unknown method accepted")
	}
	for _, method := range []string{protocol.MethodPluginQuiesce, protocol.MethodPluginShutdown} {
		if _, err := p.Handle(method, nil); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}
}

func TestServeFiltersCPACatalogWithTheCallersKey(t *testing.T) {
	cpa := newFakeCPA(t)
	out := serve(t, configured(t, switchedOff(t, cpa, "")), codexRequest())
	if out.StatusCode != http.StatusOK || out.Headers.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("response = %d %v", out.StatusCode, out.Headers)
	}
	if got := slugs(t, out.Body); !slices.Equal(got, []string{"gpt-6-sol", "gpt-5.5", "codex-auto-review"}) {
		t.Fatalf("slugs = %v", got)
	}
	if !strings.Contains(string(out.Body), `"base_instructions":"<b>&</b>"`) {
		t.Fatalf("kept entry was re-encoded: %s", out.Body)
	}
	sent := cpa.last(t)
	if sent.Method != http.MethodGet || sent.URL.Path != "/v1/models" || sent.URL.RawQuery != "client_version=0.159.0" {
		t.Fatalf("CPA request = %s %s", sent.Method, sent.URL)
	}
	if sent.Header.Get("Authorization") != clientKey || sent.Header.Get("User-Agent") != ID+"/"+Version {
		t.Fatalf("CPA headers = %v", sent.Header)
	}
	// Claude routing, client identity, and unrelated headers stay behind.
	if sent.Header.Get("Anthropic-Version") != "" || sent.Header.Get("X-Private") != "" {
		t.Fatalf("forwarded more than the key: %v", sent.Header)
	}
}

func TestServeAlwaysAsksForTheCodexFormat(t *testing.T) {
	cpa := newFakeCPA(t)
	req := codexRequest()
	req.Query = nil
	serve(t, configured(t, switchedOff(t, cpa, "")), req)
	if values, ok := cpa.last(t).URL.Query()["client_version"]; !ok || values[0] != "" {
		t.Fatalf("client_version not sent: %s", cpa.last(t).URL)
	}
}

func TestServeAppliesNewModelsAndAction(t *testing.T) {
	cpa := newFakeCPA(t)
	hide := serve(t, configured(t, switchedOff(t, cpa, "action: hide\n")), codexRequest())
	if got := slugs(t, hide.Body); len(got) != 6 || !strings.Contains(string(hide.Body), `{"slug":"claude-fable-5-1","display_name":"Claude Fable 5.1","visibility":"hide"}`) {
		t.Fatalf("hide = %s", hide.Body)
	}
	onlyOn := serve(t, configured(t, "cpa-url: "+cpa.URL+"\ndata-dir: "+t.TempDir()+"\nnew-models: disabled\nmodels:\n  gpt-5.5: true\n"), codexRequest())
	if got := slugs(t, onlyOn.Body); !slices.Equal(got, []string{"gpt-5.5"}) {
		t.Fatalf("new models disabled = %v", got)
	}
}

func TestServePassesCPAsBodyThroughWhenThereIsNothingToFilter(t *testing.T) {
	cpa := newFakeCPA(t)
	idle := configured(t, "enabled: true\ncpa-url: "+cpa.URL+"\ndata-dir: "+t.TempDir()+"\n")
	if out := serve(t, idle, codexRequest()); out.StatusCode != http.StatusOK || string(out.Body) != codexCatalog {
		t.Fatalf("idle = %d %s", out.StatusCode, out.Body)
	}
	for name, body := range map[string]string{
		"not a codex catalog":  `{"object":"list","data":[{"id":"claude-x"}]}`,
		"nothing would remain": `{"models":[{"slug":"claude-fable-5-1","visibility":"list"}]}`,
		"malformed":            `{"models":[`,
	} {
		cpa.set(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
		if out := serve(t, configured(t, switchedOff(t, cpa, "")), codexRequest()); out.StatusCode != http.StatusOK || string(out.Body) != body {
			t.Errorf("%s: %d %s", name, out.StatusCode, out.Body)
		}
	}
}

func TestServeReturnsCPAsClientErrors(t *testing.T) {
	cpa := newFakeCPA(t)
	p := configured(t, switchedOff(t, cpa, ""))
	for name, key := range map[string][]string{"missing key": nil, "wrong key": {"Bearer nope"}} {
		req := codexRequest()
		req.Headers["Authorization"] = key
		if out := serve(t, p, req); out.StatusCode != http.StatusUnauthorized || string(out.Body) != `{"error":"Invalid API key"}` {
			t.Errorf("%s: %d %s", name, out.StatusCode, out.Body)
		}
	}
	if s := readState(t, p); s.SeenAt != nil || len(s.Models) != 0 {
		t.Fatalf("a rejected fetch was remembered: %+v", s)
	}
}

func TestServeReportsAnUnavailableCPA(t *testing.T) {
	cpa := newFakeCPA(t)
	p := configured(t, switchedOff(t, cpa, ""))
	p.client.Timeout = 200 * time.Millisecond
	cases := map[string]func(http.ResponseWriter, *http.Request){
		"server error": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://example.com/", http.StatusFound)
		},
		"slow": func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(time.Second)
			_, _ = w.Write([]byte(codexCatalog))
		},
		"oversized": func(w http.ResponseWriter, _ *http.Request) {
			chunk := []byte(strings.Repeat(" ", 1<<20))
			for range maxCatalogBytes>>20 + 1 {
				_, _ = w.Write(chunk)
			}
		},
	}
	for name, respond := range cases {
		cpa.set(respond)
		if out := serve(t, p, codexRequest()); out.StatusCode != http.StatusBadGateway || string(out.Body) != `{"error":"cpa_unavailable"}` {
			t.Errorf("%s: %d %s", name, out.StatusCode, out.Body)
		}
	}
	cpa.Close()
	if out := serve(t, p, codexRequest()); out.StatusCode != http.StatusBadGateway {
		t.Errorf("unreachable: %d %s", out.StatusCode, out.Body)
	}
}

func TestRouteAnswersOnlyExactGETs(t *testing.T) {
	cpa := newFakeCPA(t)
	p := configured(t, switchedOff(t, cpa, ""))
	for name, mutate := range map[string]func(*protocol.ManagementRequest){
		"post catalog":       func(r *protocol.ManagementRequest) { r.Method = http.MethodPost },
		"other path":         func(r *protocol.ManagementRequest) { r.Path = "/v0/resource/plugins/" + ID + "/status" },
		"prefix only":        func(r *protocol.ManagementRequest) { r.Path = CatalogPath + "/extra" },
		"state as resource":  func(r *protocol.ManagementRequest) { r.Path = "/v0/resource/plugins/" + ID + "/state" },
		"page as management": func(r *protocol.ManagementRequest) { r.Path = "/v0/management/plugins/" + ID + "/settings" },
		"delete state":       func(r *protocol.ManagementRequest) { r.Method, r.Path = http.MethodDelete, StatePath },
	} {
		req := codexRequest()
		mutate(&req)
		if out := serve(t, p, req); out.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", name, out.StatusCode)
		}
	}
	result, _ := p.Handle(protocol.MethodManagementHandle, []byte("{"))
	if out := result.(protocol.ManagementResponse); out.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed request: %d", out.StatusCode)
	}
	if cpa.calls() != 0 {
		t.Fatalf("CPA called for a rejected request: %d", cpa.calls())
	}
}

func TestUnavailableUntilConfiguredAndAfterShutdown(t *testing.T) {
	for _, req := range []protocol.ManagementRequest{codexRequest(), stateRequest()} {
		if out := serve(t, New(), req); out.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s unconfigured: %d", req.Path, out.StatusCode)
		}
	}
	cpa := newFakeCPA(t)
	p := configured(t, switchedOff(t, cpa, ""))
	p.Shutdown()
	if out := serve(t, p, codexRequest()); out.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("shut down: %d", out.StatusCode)
	}
	if _, err := p.Handle(protocol.MethodPluginReconfigure, lifecycle(t, switchedOff(t, cpa, ""), protocol.SchemaVersion)); err == nil {
		t.Fatal("shut-down plugin accepted reconfiguration")
	}
}

func TestStateListsTheLastCatalogWithEffectiveSwitches(t *testing.T) {
	cpa := newFakeCPA(t)
	p := configured(t, switchedOff(t, cpa, "  gpt-5.5: true\n  retired-model: false\n"))
	before := readState(t, p)
	if before.SeenAt != nil || len(before.Models) != 0 || before.CatalogPath != CatalogPath || before.NewModels != "enabled" || before.Action != "remove" {
		t.Fatalf("before Codex fetched = %+v", before)
	}
	p.now = func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) }
	serve(t, p, codexRequest())
	s := readState(t, p)
	if s.SeenAt == nil || !s.SeenAt.Equal(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)) || !s.Saved {
		t.Fatalf("seen = %v saved = %v", s.SeenAt, s.Saved)
	}
	var got []string
	for _, m := range s.Models {
		got = append(got, m.Slug+"/"+m.DisplayName+"/"+m.Visibility+"/"+map[bool]string{true: "on", false: "off"}[m.Enabled]+"/"+map[bool]string{true: "set", false: "default"}[m.Switched])
	}
	want := []string{
		"gpt-6-sol/GPT-6 Sol/list/on/default",
		"claude-fable-5-1/Claude Fable 5.1/list/off/set",
		"or-kimi-k2//list/off/set",
		"cpa-sonnet//list/off/set",
		"gpt-5.5//list/on/set",
		"codex-auto-review//hide/on/default",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("models =\n%v\nwant\n%v", got, want)
	}
	if len(s.Switches) != 5 || s.Switches["retired-model"] != false || s.Switches["gpt-5.5"] != true {
		t.Fatalf("switches = %v", s.Switches)
	}
}

func TestTheLastCatalogSurvivesARestart(t *testing.T) {
	cpa := newFakeCPA(t)
	dir := t.TempDir()
	cfg := "cpa-url: " + cpa.URL + "\ndata-dir: " + dir + "\nmodels:\n  claude-fable-5-1: false\n"
	serve(t, configured(t, cfg), codexRequest())
	raw, err := os.ReadFile(filepath.Join(dir, seenFile))
	if err != nil {
		t.Fatal(err)
	}
	// Only slugs, names, and CPA's visibility: never instructions or keys.
	if strings.Contains(string(raw), "base_instructions") || strings.Contains(string(raw), "client-key") || !strings.Contains(string(raw), `"slug": "gpt-6-sol"`) {
		t.Fatalf("saved list = %s", raw)
	}
	if info, err := os.Stat(filepath.Join(dir, seenFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("saved list mode = %v, %v", info.Mode(), err)
	}
	restarted := configured(t, cfg)
	if s := readState(t, restarted); s.SeenAt == nil || len(s.Models) != 6 || s.Models[1].Slug != "claude-fable-5-1" || s.Models[1].Enabled {
		t.Fatalf("after restart = %+v", s)
	}
	if cpa.calls() != 1 {
		t.Fatalf("reading state contacted CPA: %d calls", cpa.calls())
	}
}

func TestAnUnchangedListIsRewrittenOnlyHourly(t *testing.T) {
	cpa := newFakeCPA(t)
	dir := t.TempDir()
	p := configured(t, "cpa-url: "+cpa.URL+"\ndata-dir: "+dir+"\n")
	start := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	savedAt := func() time.Time {
		saved, err := readSeen(filepath.Join(dir, seenFile))
		if err != nil {
			t.Fatal(err)
		}
		return saved.SeenAt
	}
	for _, step := range []struct {
		at   time.Duration
		body string
		want time.Duration
	}{
		{0, codexCatalog, 0},
		{10 * time.Minute, codexCatalog, 0},
		{20 * time.Minute, `{"models":[{"slug":"gpt-7","visibility":"list"}]}`, 20 * time.Minute},
		{30 * time.Minute, `{"models":[{"slug":"gpt-7","visibility":"list"}]}`, 20 * time.Minute},
		{81 * time.Minute, `{"models":[{"slug":"gpt-7","visibility":"list"}]}`, 81 * time.Minute},
	} {
		p.now = func() time.Time { return start.Add(step.at) }
		cpa.set(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(step.body)) })
		serve(t, p, codexRequest())
		if got := savedAt(); !got.Equal(start.Add(step.want)) {
			t.Fatalf("at %v the file says %v, want %v", step.at, got, start.Add(step.want))
		}
		if s := readState(t, p); !s.SeenAt.Equal(start.Add(step.at)) {
			t.Fatalf("at %v the page says %v", step.at, s.SeenAt)
		}
	}
}

func TestAnUnwritableDataDirStillServesAndSaysSo(t *testing.T) {
	cpa := newFakeCPA(t)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p := configured(t, "cpa-url: "+cpa.URL+"\ndata-dir: "+filepath.Join(blocker, "dir")+"\nmodels: {claude-fable-5-1: false}\n")
	if out := serve(t, p, codexRequest()); out.StatusCode != http.StatusOK || strings.Contains(string(out.Body), "claude-fable-5-1") {
		t.Fatalf("catalog = %d %s", out.StatusCode, out.Body)
	}
	if s := readState(t, p); s.Saved || len(s.Models) != 6 {
		t.Fatalf("state = %+v", s)
	}
}

func TestACorruptSavedListIsIgnored(t *testing.T) {
	dir := t.TempDir()
	for _, raw := range []string{"{", `{"seen_at":"2026-09-30T12:00:00Z","models":[]}`, `{"seen_at":"2026-09-30T12:00:00Z","models":[{"slug":""}]}`, `{"models":[{"slug":"x"}]}`} {
		if err := os.WriteFile(filepath.Join(dir, seenFile), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if s := readState(t, configured(t, "data-dir: "+dir+"\n")); s.SeenAt != nil || len(s.Models) != 0 {
			t.Fatalf("%s loaded as %+v", raw, s)
		}
	}
}

func TestSettingsPageIsFixedAndLockedDown(t *testing.T) {
	out := serve(t, New(), protocol.ManagementRequest{Method: http.MethodGet, Path: SettingsPath, Query: url.Values{"x": {"<script>"}}})
	csp := out.Headers.Get("Content-Security-Policy")
	if out.StatusCode != http.StatusOK || out.Headers.Get("Content-Type") != "text/html; charset=utf-8" || out.Headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("page = %d %v", out.StatusCode, out.Headers)
	}
	for _, want := range []string{"default-src 'none'", "script-src " + assetHash(settingsScript), "style-src " + assetHash(settingsStyle), "connect-src 'self'", "frame-ancestors 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q: %s", want, csp)
		}
	}
	// Fixed bytes: nothing from the request or the plugin's state is reflected.
	if string(out.Body) != settingsDocument || !strings.Contains(settingsDocument, "<title>Codex Models</title>") {
		t.Fatalf("page body is not the fixed document")
	}
	// The script calls exactly the plugin's own state and config routes.
	for _, path := range []string{StatePath, CatalogPath, SettingsPath, "/v0/management/plugins/" + ID + "/config"} {
		if !strings.Contains(settingsScript, "'"+path+"'") {
			t.Errorf("script lacks %s", path)
		}
	}
}

func TestReconfigureSwapsSwitchesAndKeepsThemOnRejection(t *testing.T) {
	cpa := newFakeCPA(t)
	dir := t.TempDir()
	p := configured(t, switchedOff(t, cpa, ""))
	if _, err := p.Handle(protocol.MethodPluginReconfigure, lifecycle(t, "cpa-url: "+cpa.URL+"\ndata-dir: "+dir+"\nnew-models: disabled\nmodels: {gpt-6-sol: true}\n", protocol.SchemaVersion)); err != nil {
		t.Fatal(err)
	}
	if got := slugs(t, serve(t, p, codexRequest()).Body); !slices.Equal(got, []string{"gpt-6-sol"}) {
		t.Fatalf("after reconfigure = %v", got)
	}
	if _, err := p.Handle(protocol.MethodPluginReconfigure, lifecycle(t, "models: {gpt-6-sol: maybe}\n", protocol.SchemaVersion)); err == nil {
		t.Fatal("bad reconfigure accepted")
	}
	if got := slugs(t, serve(t, p, codexRequest()).Body); !slices.Equal(got, []string{"gpt-6-sol"}) {
		t.Fatalf("rejected reconfigure changed switches: %v", got)
	}
}

func TestConcurrentServeStateAndReconfigure(t *testing.T) {
	cpa := newFakeCPA(t)
	cfg := switchedOff(t, cpa, "")
	p := configured(t, cfg)
	catalogRaw, stateRaw := encode(t, codexRequest()), encode(t, stateRequest())
	hide := lifecycle(t, cfg+"action: hide\n", protocol.SchemaVersion)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 40 {
				switch i {
				case 0:
					_, _ = p.Handle(protocol.MethodPluginReconfigure, hide)
				case 1:
					result, err := p.Handle(protocol.MethodManagementHandle, stateRaw)
					if out, _ := result.(protocol.ManagementResponse); err != nil || out.StatusCode != http.StatusOK {
						t.Error("state failed during reconfiguration")
						return
					}
				default:
					result, err := p.Handle(protocol.MethodManagementHandle, catalogRaw)
					out, _ := result.(protocol.ManagementResponse)
					if err != nil || out.StatusCode != http.StatusOK || strings.Contains(string(out.Body), `"slug":"claude-fable-5-1","display_name":"Claude Fable 5.1","visibility":"list"`) {
						t.Error("catalog not filtered during reconfiguration")
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}
