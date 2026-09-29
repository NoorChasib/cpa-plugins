package plugin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/protocol"
)

const codexCatalog = `{"models":[{"slug":"gpt-6-sol","visibility":"list","base_instructions":"<b>&</b>"},{"slug":"claude-fable-5-1","visibility":"list"},{"slug":"or-kimi-k2","visibility":"list"},{"slug":"cpa-sonnet","visibility":"list"},{"slug":"gpt-5.5","visibility":"list"},{"slug":"codex-auto-review","visibility":"hide"}]}`

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

func rules(cpa *fakeCPA, extra string) string {
	return "enabled: true\ninclude: ['gpt-[0-9]*', 'codex-*']\ncpa-url: " + cpa.URL + "\n" + extra
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
	result, err := p.Handle(protocol.MethodPluginRegister, lifecycle(t, "include: ['gpt-*']\n", protocol.SchemaVersion))
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
	if !slices.Equal(fields, []string{"include:array", "exclude:array", "action:enum", "cpa-url:string"}) {
		t.Fatalf("config fields = %v", fields)
	}
}

func TestManagementRegisterDeclaresOneMenulessResource(t *testing.T) {
	result, err := New().Handle(protocol.MethodManagementRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		Routes    []any
		Resources []protocol.ResourceRoute
	}
	if err := json.Unmarshal(encode(t, result), &reg); err != nil {
		t.Fatal(err)
	}
	if len(reg.Routes) != 0 || len(reg.Resources) != 1 || reg.Resources[0].Path != "/models" || reg.Resources[0].Menu != "" {
		t.Fatalf("registration = %+v", reg)
	}
}

func TestLifecycleRejectsOldSchemaAndBadConfig(t *testing.T) {
	p := New()
	if _, err := p.Handle(protocol.MethodPluginRegister, lifecycle(t, "include: ['gpt-*']\n", 5)); err == nil {
		t.Fatal("schema 5 accepted")
	}
	if _, err := p.Handle(protocol.MethodPluginRegister, []byte("{")); err == nil {
		t.Fatal("malformed lifecycle accepted")
	}
	if _, err := p.Handle(protocol.MethodPluginRegister, lifecycle(t, "include: ['[']\n", protocol.SchemaVersion)); err == nil {
		t.Fatal("malformed glob accepted")
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
	out := serve(t, configured(t, rules(cpa, "")), codexRequest())
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
	serve(t, configured(t, rules(cpa, "")), req)
	if values, ok := cpa.last(t).URL.Query()["client_version"]; !ok || values[0] != "" {
		t.Fatalf("client_version not sent: %s", cpa.last(t).URL)
	}
}

func TestServeHideKeepsEveryEntry(t *testing.T) {
	cpa := newFakeCPA(t)
	out := serve(t, configured(t, rules(cpa, "action: hide\n")), codexRequest())
	if got := slugs(t, out.Body); len(got) != 6 {
		t.Fatalf("slugs = %v", got)
	}
	if !strings.Contains(string(out.Body), `{"slug":"claude-fable-5-1","visibility":"hide"}`) {
		t.Fatalf("claude entry not hidden: %s", out.Body)
	}
}

func TestServePassesCPAsBodyThroughWhenThereIsNothingToFilter(t *testing.T) {
	cpa := newFakeCPA(t)
	idle := configured(t, "enabled: true\ncpa-url: "+cpa.URL+"\n")
	if out := serve(t, idle, codexRequest()); out.StatusCode != http.StatusOK || string(out.Body) != codexCatalog {
		t.Fatalf("idle = %d %s", out.StatusCode, out.Body)
	}
	for name, body := range map[string]string{
		"not a codex catalog":  `{"object":"list","data":[{"id":"claude-x"}]}`,
		"nothing would remain": `{"models":[{"slug":"claude-x","visibility":"list"}]}`,
		"malformed":            `{"models":[`,
	} {
		cpa.set(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
		if out := serve(t, configured(t, rules(cpa, "")), codexRequest()); out.StatusCode != http.StatusOK || string(out.Body) != body {
			t.Errorf("%s: %d %s", name, out.StatusCode, out.Body)
		}
	}
}

func TestServeReturnsCPAsClientErrors(t *testing.T) {
	cpa := newFakeCPA(t)
	p := configured(t, rules(cpa, ""))
	for name, key := range map[string][]string{"missing key": nil, "wrong key": {"Bearer nope"}} {
		req := codexRequest()
		req.Headers["Authorization"] = key
		if out := serve(t, p, req); out.StatusCode != http.StatusUnauthorized || string(out.Body) != `{"error":"Invalid API key"}` {
			t.Errorf("%s: %d %s", name, out.StatusCode, out.Body)
		}
	}
}

func TestServeReportsAnUnavailableCPA(t *testing.T) {
	cpa := newFakeCPA(t)
	p := configured(t, rules(cpa, ""))
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

func TestServeAnswersOnlyTheCatalogRoute(t *testing.T) {
	cpa := newFakeCPA(t)
	p := configured(t, rules(cpa, ""))
	for name, mutate := range map[string]func(*protocol.ManagementRequest){
		"post":        func(r *protocol.ManagementRequest) { r.Method = http.MethodPost },
		"other path":  func(r *protocol.ManagementRequest) { r.Path = "/v0/resource/plugins/" + ID + "/status" },
		"prefix only": func(r *protocol.ManagementRequest) { r.Path = CatalogPath + "/extra" },
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
	cpa.mu.Lock()
	defer cpa.mu.Unlock()
	if len(cpa.seen) != 0 {
		t.Fatalf("CPA called for a rejected request: %d", len(cpa.seen))
	}
}

func TestServeIsUnavailableUntilConfiguredAndAfterShutdown(t *testing.T) {
	if out := serve(t, New(), codexRequest()); out.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured: %d", out.StatusCode)
	}
	cpa := newFakeCPA(t)
	p := configured(t, rules(cpa, ""))
	p.Shutdown()
	if out := serve(t, p, codexRequest()); out.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("shut down: %d", out.StatusCode)
	}
	if _, err := p.Handle(protocol.MethodPluginReconfigure, lifecycle(t, rules(cpa, ""), protocol.SchemaVersion)); err == nil {
		t.Fatal("shut-down plugin accepted reconfiguration")
	}
}

func TestReconfigureSwapsRulesAndKeepsThemOnRejection(t *testing.T) {
	cpa := newFakeCPA(t)
	p := configured(t, rules(cpa, ""))
	if _, err := p.Handle(protocol.MethodPluginReconfigure, lifecycle(t, "include: ['gpt-6-*']\ncpa-url: "+cpa.URL+"\n", protocol.SchemaVersion)); err != nil {
		t.Fatal(err)
	}
	if got := slugs(t, serve(t, p, codexRequest()).Body); !slices.Equal(got, []string{"gpt-6-sol"}) {
		t.Fatalf("after reconfigure = %v", got)
	}
	if _, err := p.Handle(protocol.MethodPluginReconfigure, lifecycle(t, "include: ['gpt-[']\n", protocol.SchemaVersion)); err == nil {
		t.Fatal("bad reconfigure accepted")
	}
	if got := slugs(t, serve(t, p, codexRequest()).Body); !slices.Equal(got, []string{"gpt-6-sol"}) {
		t.Fatalf("rejected reconfigure changed rules: %v", got)
	}
}

func TestConcurrentServeAndReconfigure(t *testing.T) {
	cpa := newFakeCPA(t)
	p := configured(t, rules(cpa, ""))
	raw := encode(t, codexRequest())
	hide := lifecycle(t, rules(cpa, "action: hide\n"), protocol.SchemaVersion)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				if i == 0 {
					_, _ = p.Handle(protocol.MethodPluginReconfigure, hide)
					continue
				}
				result, err := p.Handle(protocol.MethodManagementHandle, raw)
				out, _ := result.(protocol.ManagementResponse)
				if err != nil || out.StatusCode != http.StatusOK || strings.Contains(string(out.Body), `"slug":"claude-fable-5-1","visibility":"list"`) {
					t.Error("catalog not filtered during reconfiguration")
					return
				}
			}
		}()
	}
	wg.Wait()
}
