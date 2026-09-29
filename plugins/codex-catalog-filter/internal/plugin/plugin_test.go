package plugin

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/protocol"
)

// wireRequest mirrors CPA's untagged pluginapi.ResponseInterceptRequest plus the
// RPC callback field, so the JSON below is what the host actually sends.
type wireRequest struct {
	RequestID       string
	SourceFormat    string
	Model           string
	RequestedModel  string
	Stream          bool
	RequestHeaders  http.Header
	ResponseHeaders http.Header
	OriginalRequest []byte
	RequestBody     []byte
	Body            []byte
	StatusCode      int
	Metadata        map[string]any
	HostCallbackID  string `json:"host_callback_id,omitempty"`
}

// wireResponse mirrors pluginapi.ResponseInterceptResponse as CPA decodes it.
type wireResponse struct {
	Headers      http.Header
	Body         []byte
	ClearHeaders []string
}

const codexCatalog = `{"models":[{"slug":"gpt-6-sol","visibility":"list","base_instructions":"<b>&</b>"},{"slug":"claude-fable-5-1","visibility":"list"},{"slug":"or-kimi-k2","visibility":"list"},{"slug":"cpa-sonnet","visibility":"list"},{"slug":"gpt-5.5","visibility":"list"},{"slug":"codex-auto-review","visibility":"hide"}]}`

func modelList(body string) wireRequest {
	return wireRequest{
		RequestID:       "req-1",
		SourceFormat:    "openai",
		RequestHeaders:  http.Header{"Authorization": {"Bearer client-key"}, "User-Agent": {"codex_cli_rs/0.159.0"}},
		ResponseHeaders: http.Header{"Content-Type": {"application/json; charset=utf-8"}},
		Body:            []byte(body),
		StatusCode:      http.StatusOK,
		HostCallbackID:  "cb-1",
	}
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

// intercept round-trips through JSON exactly as the native boundary does.
func intercept(t *testing.T, p *Plugin, req wireRequest) wireResponse {
	t.Helper()
	result, err := p.Handle(protocol.MethodResponseInterceptAfter, encode(t, req))
	if err != nil {
		t.Fatalf("intercept returned an error: %v", err)
	}
	var out wireResponse
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

const codexRules = "enabled: true\ninclude: ['gpt-[0-9]*', 'codex-*']\n"

func TestRegistrationAdvertisesOnlyResponseInterceptor(t *testing.T) {
	p := New()
	result, err := p.Handle(protocol.MethodPluginRegister, lifecycle(t, codexRules, protocol.SchemaVersion))
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
	if reg.SchemaVersion != 6 || len(reg.Capabilities) != 1 || !reg.Capabilities["response_interceptor"] {
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
	if !slices.Equal(fields, []string{"include:array", "exclude:array", "action:enum"}) {
		t.Fatalf("config fields = %v", fields)
	}
}

func TestLifecycleRejectsOldSchemaAndBadConfig(t *testing.T) {
	p := New()
	if _, err := p.Handle(protocol.MethodPluginRegister, lifecycle(t, codexRules, 5)); err == nil {
		t.Fatal("schema 5 accepted")
	}
	if _, err := p.Handle(protocol.MethodPluginRegister, []byte("{")); err == nil {
		t.Fatal("malformed lifecycle accepted")
	}
	if _, err := p.Handle(protocol.MethodPluginRegister, lifecycle(t, "include: ['[']\n", protocol.SchemaVersion)); err == nil {
		t.Fatal("malformed glob accepted")
	}
	if _, err := p.Handle("model.register", nil); err == nil {
		t.Fatal("unknown method accepted")
	}
	for _, method := range []string{protocol.MethodPluginQuiesce, protocol.MethodPluginShutdown} {
		if _, err := p.Handle(method, nil); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}
}

func TestInterceptFiltersCodexCatalog(t *testing.T) {
	p := configured(t, codexRules)
	out := intercept(t, p, modelList(codexCatalog))
	if got := slugs(t, out.Body); !slices.Equal(got, []string{"gpt-6-sol", "gpt-5.5", "codex-auto-review"}) {
		t.Fatalf("slugs = %v", got)
	}
	if !strings.Contains(string(out.Body), `"base_instructions":"<b>&</b>"`) {
		t.Fatalf("kept entry was re-encoded: %s", out.Body)
	}
	if out.Headers != nil || out.ClearHeaders != nil {
		t.Fatalf("unexpected header changes: %+v", out)
	}
}

func TestInterceptHideKeepsEveryEntry(t *testing.T) {
	p := configured(t, codexRules+"action: hide\n")
	out := intercept(t, p, modelList(codexCatalog))
	if got := slugs(t, out.Body); len(got) != 6 {
		t.Fatalf("slugs = %v", got)
	}
	if !strings.Contains(string(out.Body), `{"slug":"claude-fable-5-1","visibility":"hide"}`) {
		t.Fatalf("claude entry not hidden: %s", out.Body)
	}
}

func TestInterceptLeavesOtherResponsesAlone(t *testing.T) {
	p := configured(t, codexRules)
	openaiList := `{"object":"list","data":[{"id":"gpt-6-sol","object":"model"},{"id":"claude-fable-5-1","object":"model"}]}`
	gemini := `{"models":[{"name":"models/gemini-3-pro"},{"name":"models/claude-x"}]}`
	cases := map[string]func(*wireRequest){
		"claude source":        func(r *wireRequest) { r.SourceFormat = "claude" },
		"gemini source":        func(r *wireRequest) { r.SourceFormat = "gemini"; r.Body = []byte(gemini) },
		"gemini body":          func(r *wireRequest) { r.Body = []byte(gemini) },
		"responses source":     func(r *wireRequest) { r.SourceFormat = "openai-response" },
		"openai list":          func(r *wireRequest) { r.Body = []byte(openaiList) },
		"completion model":     func(r *wireRequest) { r.Model = "gpt-6-sol" },
		"requested model":      func(r *wireRequest) { r.RequestedModel = "gpt-6-sol" },
		"stream":               func(r *wireRequest) { r.Stream = true },
		"original request":     func(r *wireRequest) { r.OriginalRequest = []byte(`{"model":"x"}`) },
		"request body":         func(r *wireRequest) { r.RequestBody = []byte(`{"model":"x"}`) },
		"error status":         func(r *wireRequest) { r.StatusCode = http.StatusBadGateway },
		"malformed catalog":    func(r *wireRequest) { r.Body = []byte(codexCatalog[:len(codexCatalog)-3]) },
		"empty body":           func(r *wireRequest) { r.Body = nil },
		"only unmatched model": func(r *wireRequest) { r.Body = []byte(`{"models":[{"slug":"claude-x","visibility":"list"}]}`) },
	}
	for name, mutate := range cases {
		req := modelList(codexCatalog)
		mutate(&req)
		if out := intercept(t, p, req); out.Body != nil || out.Headers != nil || out.ClearHeaders != nil {
			t.Errorf("%s: changed response: %+v", name, out)
		}
	}
	for name, raw := range map[string][]byte{"not json": []byte("{"), "empty": nil, "wrong types": []byte(`{"SourceFormat":7}`)} {
		result, err := p.Handle(protocol.MethodResponseInterceptAfter, raw)
		if err != nil || string(encode(t, result)) != "{}" {
			t.Errorf("%s: result %s, err %v", name, encode(t, result), err)
		}
	}
}

func TestUnchangedResponseEncodesAsEmptyObject(t *testing.T) {
	p := configured(t, codexRules)
	req := modelList(codexCatalog)
	req.SourceFormat = "claude"
	result, err := p.Handle(protocol.MethodResponseInterceptAfter, encode(t, req))
	if err != nil || string(encode(t, result)) != "{}" {
		t.Fatalf("unchanged result = %s, %v", encode(t, result), err)
	}
}

func TestInterceptClearsValidatorsForTheOldBody(t *testing.T) {
	p := configured(t, codexRules)
	req := modelList(codexCatalog)
	req.ResponseHeaders.Set("ETag", `"unfiltered"`)
	req.ResponseHeaders.Set("Content-Length", "999")
	out := intercept(t, p, req)
	slices.Sort(out.ClearHeaders)
	if out.Body == nil || !slices.Equal(out.ClearHeaders, []string{"Content-Length", "Etag"}) {
		t.Fatalf("response = %+v", out)
	}
}

func TestInterceptIsIdleUntilConfiguredAndAfterShutdown(t *testing.T) {
	p := New()
	if out := intercept(t, p, modelList(codexCatalog)); out.Body != nil {
		t.Fatal("unconfigured plugin rewrote a catalog")
	}
	p = configured(t, "enabled: true\n")
	if out := intercept(t, p, modelList(codexCatalog)); out.Body != nil {
		t.Fatal("plugin without include rewrote a catalog")
	}
	p = configured(t, codexRules)
	p.Shutdown()
	if out := intercept(t, p, modelList(codexCatalog)); out.Body != nil {
		t.Fatal("shut-down plugin rewrote a catalog")
	}
	if _, err := p.Handle(protocol.MethodPluginReconfigure, lifecycle(t, codexRules, protocol.SchemaVersion)); err == nil {
		t.Fatal("shut-down plugin accepted reconfiguration")
	}
}

func TestReconfigureSwapsRulesAndKeepsThemOnRejection(t *testing.T) {
	p := configured(t, codexRules)
	if _, err := p.Handle(protocol.MethodPluginReconfigure, lifecycle(t, "include: ['gpt-6-*']\n", protocol.SchemaVersion)); err != nil {
		t.Fatal(err)
	}
	if got := slugs(t, intercept(t, p, modelList(codexCatalog)).Body); !slices.Equal(got, []string{"gpt-6-sol"}) {
		t.Fatalf("after reconfigure = %v", got)
	}
	if _, err := p.Handle(protocol.MethodPluginReconfigure, lifecycle(t, "include: ['gpt-[']\n", protocol.SchemaVersion)); err == nil {
		t.Fatal("bad reconfigure accepted")
	}
	if got := slugs(t, intercept(t, p, modelList(codexCatalog)).Body); !slices.Equal(got, []string{"gpt-6-sol"}) {
		t.Fatalf("rejected reconfigure changed rules: %v", got)
	}
}

func TestConcurrentInterceptAndReconfigure(t *testing.T) {
	p := configured(t, codexRules)
	raw := encode(t, modelList(codexCatalog))
	hide := lifecycle(t, codexRules+"action: hide\n", protocol.SchemaVersion)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				if i == 0 {
					_, _ = p.Handle(protocol.MethodPluginReconfigure, hide)
					continue
				}
				result, err := p.Handle(protocol.MethodResponseInterceptAfter, raw)
				if err != nil || result.(protocol.ResponseInterceptResponse).Body == nil {
					t.Error("catalog not rewritten during reconfiguration")
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestScreenRejectsOrdinaryResponsesAtTheFirstDisqualifyingField(t *testing.T) {
	// Everything after the disqualifying field is garbage: Screen must stop
	// before reaching it, which is what keeps large prompts cheap.
	garbage := `,"RequestHeaders":` + strings.Repeat("\x00not json", 1000)
	for name, raw := range map[string]string{
		"completion model": `{"RequestID":"r","SourceFormat":"openai","Model":"gpt-6-sol"` + garbage,
		"claude source":    `{"RequestID":"r","SourceFormat":"claude"` + garbage,
		"requested model":  `{"SourceFormat":"openai","Model":"","RequestedModel":"x"` + garbage,
		"stream":           `{"SourceFormat":"openai","Model":"","RequestedModel":"","Stream":true` + garbage,
		"request body":     `{"SourceFormat":"openai","Model":"","OriginalRequest":"e30="` + garbage,
		"not an object":    `[1,2]`,
		"not json":         `garbage`,
		"empty":            ``,
		"wrong type":       `{"SourceFormat":7}`,
	} {
		if Screen([]byte(raw)) {
			t.Errorf("%s: screened in", name)
		}
	}
	if !Screen(encode(t, modelList(codexCatalog))) {
		t.Fatal("a model-list request was screened out")
	}
	// A layout Screen does not recognise is never rejected; Intercept decides.
	if !Screen([]byte(`{"sourceformat":"openai","model":"x","body":"e30="}`)) {
		t.Fatal("an unrecognised layout was screened out")
	}
}

func BenchmarkScreenLargeCompletion(b *testing.B) {
	req := modelList(codexCatalog)
	req.Model = "gpt-6-sol"
	req.OriginalRequest = []byte(strings.Repeat("x", 32<<20))
	req.Body = req.OriginalRequest
	raw, _ := json.Marshal(req)
	b.SetBytes(int64(len(raw)))
	for b.Loop() {
		if Screen(raw) {
			b.Fatal("screened in")
		}
	}
}

func TestBodyPrefixDecodesOnlyTheStart(t *testing.T) {
	long := `{"models":[` + strings.Repeat(`{"slug":"gpt-6-sol","visibility":"list"},`, 1000) + `{"slug":"x"}]}`
	var head struct{ Body bodyPrefix }
	if err := json.Unmarshal(encode(t, struct{ Body []byte }{[]byte(long)}), &head); err != nil {
		t.Fatal(err)
	}
	if len(head.Body) != 48 || !strings.HasPrefix(long, string(head.Body)) {
		t.Fatalf("prefix = %q", head.Body)
	}
	for body, want := range map[string]bool{
		`{"models":[]}`:                  true,
		" \n{ \"models\" : [":            true,
		`{"object":"list","data":[]}`:    false,
		`{"data":[],"models":[]}`:        false,
		`["models"]`:                     false,
		``:                               false,
		`{"model":"gpt-6-sol"}`:          false,
		`{"modelsX":[{"slug":"gpt-6"}]}`: false,
	} {
		var h struct{ Body bodyPrefix }
		if err := json.Unmarshal(encode(t, struct{ Body []byte }{[]byte(body)}), &h); err != nil {
			t.Fatal(err)
		}
		if got := looksLikeCatalog(h.Body); got != want {
			t.Errorf("looksLikeCatalog(%q) = %v", body, got)
		}
	}
}
