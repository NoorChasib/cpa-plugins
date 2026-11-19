package plugin

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/protocol"
)

const geminiList = `{"models":[` +
	`{"name":"models/gpt-6-sol","displayName":"GPT 6.0 Sol","inputTokenLimit":272000,"outputTokenLimit":128000,"supportedGenerationMethods":["generateContent"]},` +
	`{"name":"models/gemini-4-argon-high","displayName":"Gemini 4 Argon","description":"Gemini 4 Argon (High)","inputTokenLimit":1048576,"outputTokenLimit":1048576,"supportedGenerationMethods":["generateContent"]},` +
	`{"name":"models/grok-4.7","displayName":"Grok 4.7","inputTokenLimit":500000,"outputTokenLimit":500000,"supportedGenerationMethods":["generateContent"]},` +
	`{"name":"models/gpt-image-2","displayName":"GPT Image 2","supportedGenerationMethods":["generateContent"]},` +
	`{"name":"models/gemini-3.1-flash-lite","displayName":"Gemini 3.1 Flash Lite","inputTokenLimit":1048576,"outputTokenLimit":65535,"supportedGenerationMethods":["generateContent"]}` +
	`]}`

const claudeListBody = `{"data":[{"id":"claude-opus-5-5","display_name":"Claude Opus 5.5"},{"id":"claude-fable-5-dd-los-6-tpg","display_name":"GPT 6.0 Sol"},{"id":"claude-fable-5-dd-7.4-korg","display_name":"Grok 4.7"}],"first_id":"claude-opus-5-5","has_more":false,"last_id":"claude-fable-5-dd-7.4-korg"}`

const otherKey = "Bearer other-key"

// newGateway stands in for CPA: /v1beta/models answers geminiList and
// /v1/models the Codex catalog, both only to clientKey.
func newGateway(t *testing.T) *fakeCPA {
	t.Helper()
	f := newFakeCPA(t)
	f.set(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != clientKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"Invalid API key"}`))
			return
		}
		if r.URL.Path == "/v1beta/models" {
			_, _ = w.Write([]byte(geminiList))
			return
		}
		_, _ = w.Write([]byte(codexCatalog))
	})
	return f
}

func agyRequest(authorization string) protocol.ManagementRequest {
	headers := http.Header{
		"User-Agent": {"antigravity/cli/1.5.12 (aidev_client; os_type=linux; arch=amd64; auth_method=gateway)"},
		"X-Private":  {"do-not-forward"},
	}
	if authorization != "" {
		headers.Set("Authorization", authorization)
	}
	return protocol.ManagementRequest{Method: http.MethodGet, Path: AgyPath, Headers: headers}
}

type agyCatalog struct {
	DefaultModelID string `json:"defaultModelId"`
	List           []struct {
		ModelID         string `json:"modelId"`
		DisplayName     string `json:"displayName"`
		MaxTokens       int    `json:"maxTokens"`
		MaxOutputTokens int    `json:"maxOutputTokens"`
	} `json:"list"`
}

func agyIDs(t *testing.T, out protocol.ManagementResponse) []string {
	t.Helper()
	if out.StatusCode != http.StatusOK {
		t.Fatalf("agy route = %d %s", out.StatusCode, out.Body)
	}
	var c agyCatalog
	dec := json.NewDecoder(strings.NewReader(string(out.Body)))
	if err := dec.Decode(&c); err != nil || c.DefaultModelID == "" || len(c.List) == 0 || c.List[0].ModelID != c.DefaultModelID {
		t.Fatalf("not an agy catalog: %s", out.Body)
	}
	var ids []string
	for _, e := range c.List {
		ids = append(ids, e.ModelID)
	}
	return ids
}

func agyConfig(cpa *fakeCPA, dir, extra string) string {
	return "cpa-url: " + cpa.URL + "\ndata-dir: " + dir + "\n" + extra
}

func TestAgyRouteBuildsTheCatalogWithTheCallersKeyOnly(t *testing.T) {
	cpa := newGateway(t)
	p := configured(t, agyConfig(cpa, t.TempDir(), "antigravity:\n  models: {gpt-6-sol: true, grok-4.7: true, gemini-4-argon-high: true}\n  default-model: gemini-4-argon-high\n"))
	out := serve(t, p, agyRequest(clientKey))
	if got := agyIDs(t, out); !slices.Equal(got, []string{"gemini-4-argon-high", "gpt-6-sol", "grok-4.7"}) {
		t.Fatalf("list = %v", got)
	}
	if out.Headers.Get("Content-Type") != "application/json; charset=utf-8" || out.Headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("headers = %v", out.Headers)
	}
	if want := `{"defaultModelId":"gemini-4-argon-high","list":[{"modelId":"gemini-4-argon-high","displayName":"Gemini 4 Argon (High)"},{"modelId":"gpt-6-sol","displayName":"GPT 6.0 Sol","maxTokens":272000,"maxOutputTokens":128000},{"modelId":"grok-4.7","displayName":"Grok 4.7","maxTokens":500000,"maxOutputTokens":65536}]}`; string(out.Body) != want {
		t.Fatalf("body = %s", out.Body)
	}
	sent := cpa.last(t)
	if sent.URL.Path != "/v1beta/models" || sent.URL.RawQuery != "" || sent.Header.Get("Authorization") != clientKey || sent.Header.Get("User-Agent") != ID+"/"+Version {
		t.Fatalf("CPA request = %s %v", sent.URL, sent.Header)
	}
	if sent.Header.Get("X-Private") != "" {
		t.Fatalf("forwarded more than the key: %v", sent.Header)
	}
}

func TestAgyRoutePassesCPAsClientErrors(t *testing.T) {
	cpa := newGateway(t)
	p := configured(t, agyConfig(cpa, t.TempDir(), ""))
	for name, key := range map[string]string{"missing key": "", "wrong key": "Bearer nope"} {
		if out := serve(t, p, agyRequest(key)); out.StatusCode != http.StatusUnauthorized || string(out.Body) != `{"error":"Invalid API key"}` {
			t.Errorf("%s: %d %s", name, out.StatusCode, out.Body)
		}
		if got := cpa.last(t).Header.Values("Authorization"); (key == "" && len(got) != 0) || (key != "" && !slices.Equal(got, []string{key})) {
			t.Errorf("%s: forwarded authorization %v", name, got)
		}
	}
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests} {
		cpa.set(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"x"}`))
		})
		if out := serve(t, p, agyRequest(clientKey)); out.StatusCode != status {
			t.Errorf("%d passed as %d", status, out.StatusCode)
		}
	}
}

func cpaDown(cpa *fakeCPA) []func(http.ResponseWriter, *http.Request) {
	return []func(http.ResponseWriter, *http.Request){
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) },
		func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://example.com/", http.StatusFound)
		},
		func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("not json")) },
		func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"models":[]}`)) },
		func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(time.Second)
			_, _ = w.Write([]byte(geminiList))
		},
	}
}

// The route is unauthenticated by CPA, so the last good list goes only to a
// key CPA accepted with a 200 in the last 24 hours, never without one.
func TestAgyLastGoodListGoesOnlyToARecentlyAcceptedKey(t *testing.T) {
	cpa := newGateway(t)
	dir := t.TempDir()
	p := configured(t, agyConfig(cpa, dir, "antigravity: {models: {gpt-6-sol: true}}\n"))
	p.client.Timeout = 200 * time.Millisecond
	start := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return start }
	if got := agyIDs(t, serve(t, p, agyRequest(clientKey))); !slices.Equal(got, []string{"gpt-6-sol"}) {
		t.Fatalf("live = %v", got)
	}
	for i, respond := range cpaDown(cpa) {
		cpa.set(respond)
		if got := agyIDs(t, serve(t, p, agyRequest(clientKey))); !slices.Equal(got, []string{"gpt-6-sol"}) {
			t.Fatalf("case %d: last good = %v", i, got)
		}
		for name, key := range map[string]string{"no key": "", "unknown key": otherKey, "near miss": clientKey + " "} {
			if out := serve(t, p, agyRequest(key)); out.StatusCode != http.StatusBadGateway || string(out.Body) != `{"error":"cpa_unavailable"}` {
				t.Fatalf("case %d, %s: %d %s", i, name, out.StatusCode, out.Body)
			}
		}
	}
	// The current switches apply to the last good list.
	if _, err := p.Handle(protocol.MethodPluginReconfigure, lifecycle(t, agyConfig(cpa, dir, "antigravity: {models: {grok-4.7: true}}\n"), protocol.SchemaVersion)); err != nil {
		t.Fatal(err)
	}
	if got := agyIDs(t, serve(t, p, agyRequest(clientKey))); !slices.Equal(got, []string{"grok-4.7"}) {
		t.Fatalf("after reconfigure = %v", got)
	}
	// 24 hours after CPA last accepted the key, it no longer gets the list.
	p.now = func() time.Time { return start.Add(agyKeyWindow) }
	if out := serve(t, p, agyRequest(clientKey)); out.StatusCode != http.StatusBadGateway {
		t.Fatalf("expired key: %d %s", out.StatusCode, out.Body)
	}
}

// A 200 whose list holds nothing agy can be given (CPA still registering its
// models, say) is not kept as the last good list, and the last good list is
// served instead of a 502.
func TestAgyListWithoutTextModelsKeepsTheLastGoodList(t *testing.T) {
	cpa := newGateway(t)
	p := configured(t, agyConfig(cpa, t.TempDir(), "antigravity: {models: {gpt-6-sol: true}}\n"))
	if got := agyIDs(t, serve(t, p, agyRequest(clientKey))); !slices.Equal(got, []string{"gpt-6-sol"}) {
		t.Fatalf("live = %v", got)
	}
	cpa.set(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != clientKey && r.Header.Get("Authorization") != otherKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"models/grok-tts"},{"name":"models/gpt-image-2"}]}`))
	})
	for range 2 {
		if got := agyIDs(t, serve(t, p, agyRequest(clientKey))); !slices.Equal(got, []string{"gpt-6-sol"}) {
			t.Fatalf("no text models = %v", got)
		}
	}
	a := readFullState(t, p).Antigravity
	if a.LastError != "no_text_models" || a.ServedFrom != "last-good" || len(a.Models) != 5 {
		t.Fatalf("state = %s %s, %d models", a.LastError, a.ServedFrom, len(a.Models))
	}
	// CPA accepted the other key with a model list, so it may get the list too.
	if got := agyIDs(t, serve(t, p, agyRequest(otherKey))); !slices.Equal(got, []string{"gpt-6-sol"}) {
		t.Fatalf("other accepted key = %v", got)
	}
	// The kept list is still the last good one when CPA goes away.
	cpa.set(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	if got := agyIDs(t, serve(t, p, agyRequest(clientKey))); !slices.Equal(got, []string{"gpt-6-sol"}) {
		t.Fatalf("CPA away = %v", got)
	}
	// Without any last good list, the answer is a 502, never an empty list.
	cpa.set(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"name":"models/grok-tts"}]}`))
	})
	fresh := configured(t, agyConfig(cpa, t.TempDir(), ""))
	if out := serve(t, fresh, agyRequest(clientKey)); out.StatusCode != http.StatusBadGateway || string(out.Body) != `{"error":"cpa_unavailable"}` {
		t.Fatalf("no last good list: %d %s", out.StatusCode, out.Body)
	}
}

func TestAgyKeyRejectedByCPALosesTheLastGoodList(t *testing.T) {
	cpa := newGateway(t)
	p := configured(t, agyConfig(cpa, t.TempDir(), ""))
	agyIDs(t, serve(t, p, agyRequest(clientKey)))
	cpa.set(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	if out := serve(t, p, agyRequest(clientKey)); out.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked = %d", out.StatusCode)
	}
	cpa.set(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	if out := serve(t, p, agyRequest(clientKey)); out.StatusCode != http.StatusBadGateway {
		t.Fatalf("revoked key got the last good list: %d", out.StatusCode)
	}
}

func TestAgyLastGoodSurvivesARestartWithoutStoringTheKey(t *testing.T) {
	cpa := newGateway(t)
	dir := t.TempDir()
	cfg := agyConfig(cpa, dir, "antigravity: {models: {gpt-6-sol: true, grok-4.7: true}}\n")
	agyIDs(t, serve(t, configured(t, cfg), agyRequest(clientKey)))
	raw, err := os.ReadFile(filepath.Join(dir, clientsFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "client-key") || !strings.Contains(string(raw), agyKeyHash(clientKey)) || !strings.Contains(string(raw), `"id": "gpt-6-sol"`) {
		t.Fatalf("saved = %s", raw)
	}
	if info, err := os.Stat(filepath.Join(dir, clientsFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v", info, err)
	}
	cpa.set(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	restarted := configured(t, cfg)
	if got := agyIDs(t, serve(t, restarted, agyRequest(clientKey))); !slices.Equal(got, []string{"gpt-6-sol", "grok-4.7"}) {
		t.Fatalf("after restart = %v", got)
	}
	if out := serve(t, restarted, agyRequest(otherKey)); out.StatusCode != http.StatusBadGateway {
		t.Fatalf("unknown key after restart: %d", out.StatusCode)
	}
}

func TestAgyRouteDuringShutdownAnswersAcceptedKeysFromMemory(t *testing.T) {
	cpa := newGateway(t)
	p := configured(t, agyConfig(cpa, t.TempDir(), "antigravity: {models: {grok-4.7: true}}\n"))
	agyIDs(t, serve(t, p, agyRequest(clientKey)))
	p.Shutdown()
	calls := cpa.calls()
	if got := agyIDs(t, serve(t, p, agyRequest(clientKey))); !slices.Equal(got, []string{"grok-4.7"}) {
		t.Fatalf("shut down = %v", got)
	}
	if out := serve(t, p, agyRequest("")); out.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("no key while shut down: %d", out.StatusCode)
	}
	if cpa.calls() != calls {
		t.Fatal("contacted CPA after shutdown")
	}
	if out := serve(t, New(), agyRequest(clientKey)); out.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured: %d", out.StatusCode)
	}
}

func TestAgyRouteAnswersOnlyExactGETs(t *testing.T) {
	cpa := newGateway(t)
	p := configured(t, agyConfig(cpa, t.TempDir(), ""))
	for name, mutate := range map[string]func(*protocol.ManagementRequest){
		"post":           func(r *protocol.ManagementRequest) { r.Method = http.MethodPost },
		"trailing slash": func(r *protocol.ManagementRequest) { r.Path = AgyPath + "/" },
		"prefix":         func(r *protocol.ManagementRequest) { r.Path = "/v0/resource/plugins/" + ID + "/agy" },
		"as management":  func(r *protocol.ManagementRequest) { r.Path = "/v0/management/plugins/" + ID + "/agy/models" },
	} {
		req := agyRequest(clientKey)
		mutate(&req)
		if out := serve(t, p, req); out.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", name, out.StatusCode)
		}
	}
	if cpa.calls() != 0 {
		t.Fatalf("CPA called %d times", cpa.calls())
	}
}

// A bad client subtree must not deactivate the plugin: Codex keeps working,
// the client keeps its last valid settings (or its defaults), and /state says why.
func TestBadClientSubtreeDisablesOnlyThatClient(t *testing.T) {
	cpa := newGateway(t)
	dir := t.TempDir()
	good := agyConfig(cpa, dir, "models: {claude-fable-5-1: false}\nantigravity: {models: {grok-4.7: true}}\nclaude-code: {models: {gpt-6-sol: false}}\n")
	bad := agyConfig(cpa, dir, "models: {claude-fable-5-1: false}\nantigravity: {models: {grok-4.7: maybe}}\nclaude-code: {filter: sometimes}\n")
	p := configured(t, good)
	reg := register(t, p, protocol.MethodPluginReconfigure, bad)
	if !reg.Capabilities["response_interceptor"] {
		t.Fatal("last valid claude-code filter dropped")
	}
	if got := slugs(t, serve(t, p, codexRequest()).Body); slices.Contains(got, "claude-fable-5-1") || len(got) != 5 {
		t.Fatalf("codex = %v", got)
	}
	if got := agyIDs(t, serve(t, p, agyRequest(clientKey))); !slices.Equal(got, []string{"grok-4.7"}) {
		t.Fatalf("agy = %v", got)
	}
	s := readFullState(t, p)
	if s.Antigravity.Error == "" || s.Antigravity.Using != "last-good" || !s.Antigravity.Configured || s.Antigravity.Switches["grok-4.7"] != true {
		t.Fatalf("antigravity state = %+v", s.Antigravity.clientState)
	}
	if s.ClaudeCode.Error == "" || s.ClaudeCode.Using != "last-good" || s.ClaudeCode.Switches["gpt-6-sol"] != false {
		t.Fatalf("claude state = %+v", s.ClaudeCode.clientState)
	}
	// Without a previous valid subtree, the client's defaults apply.
	fresh := configured(t, bad)
	if got := agyIDs(t, serve(t, fresh, agyRequest(clientKey))); !slices.Equal(got, []string{"gemini-3.1-flash-lite", "gemini-4-argon-high"}) {
		t.Fatalf("defaults = %v", got)
	}
	if s := readFullState(t, fresh); s.Antigravity.Using != "defaults" || s.ClaudeCode.Using != "defaults" || !s.ClaudeCode.Filter {
		t.Fatalf("fresh state = %+v %+v", s.Antigravity.clientState, s.ClaudeCode.clientState)
	}
	// Fixing the value applies it.
	if _, err := p.Handle(protocol.MethodPluginReconfigure, lifecycle(t, agyConfig(cpa, dir, "antigravity: {models: {gpt-6-sol: true}}\n"), protocol.SchemaVersion)); err != nil {
		t.Fatal(err)
	}
	if s := readFullState(t, p); s.Antigravity.Error != "" || s.Antigravity.Using != "config" {
		t.Fatalf("fixed = %+v", s.Antigravity.clientState)
	}
}

// interceptRequest is response.intercept_after for a model list, as CPA's
// WriteModelListResponse builds it.
func interceptRequest(t *testing.T, format, agent, model string, body string) []byte {
	t.Helper()
	return encode(t, map[string]any{
		"RequestID": "req-1", "SourceFormat": format, "Model": model, "RequestedModel": "", "Stream": false,
		"RequestHeaders":  http.Header{"User-Agent": {agent}, "Anthropic-Version": {"2023-06-01"}, "Authorization": {clientKey}},
		"ResponseHeaders": http.Header{"Content-Type": {"application/json; charset=utf-8"}},
		"OriginalRequest": nil, "RequestBody": nil, "Body": []byte(body), "StatusCode": 200,
		"Metadata": map[string]any{"caller_scope": "x"},
	})
}

func interceptBody(t *testing.T, p *Plugin, raw []byte) string {
	t.Helper()
	result, err := p.Handle(protocol.MethodResponseInterceptAfter, raw)
	if err != nil {
		t.Fatal(err)
	}
	var out protocol.ResponseInterceptResponse
	if err := json.Unmarshal(encode(t, result), &out); err != nil {
		t.Fatal(err)
	}
	return string(out.Body)
}

func TestInterceptRewritesClaudeCodesListAndRecordsIt(t *testing.T) {
	dir := t.TempDir()
	cfg := "data-dir: " + dir + "\nclaude-code: {models: {gpt-6-sol: false}}\n"
	p := configured(t, cfg)
	p.now = func() time.Time { return time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC) }
	got := interceptBody(t, p, interceptRequest(t, "claude", "claude-code/2.1.327", "", claudeListBody))
	want := `{"data":[{"id":"claude-opus-5-5","display_name":"Claude Opus 5.5"},{"id":"claude-fable-5-dd-7.4-korg","display_name":"Grok 4.7"}],"first_id":"claude-opus-5-5","has_more":false,"last_id":"claude-fable-5-dd-7.4-korg"}`
	if got != want {
		t.Fatalf("rewrite = %s", got)
	}
	s := readFullState(t, p)
	c := s.ClaudeCode
	if c.SeenAt == nil || c.Lists != 1 || c.Rewritten != 1 || c.ModelsSource != "client" || len(c.Models) != 3 {
		t.Fatalf("claude state = %+v", c)
	}
	if m := c.Models[1]; m.ID != "gpt-6-sol" || m.CloakedID != "claude-fable-5-dd-los-6-tpg" || m.Enabled || !m.Switched || m.DisplayName != "GPT 6.0 Sol" {
		t.Fatalf("model = %+v", m)
	}
	if m := c.Models[2]; m.ID != "grok-4.7" || !m.Enabled || m.Switched {
		t.Fatalf("model = %+v", m)
	}
	// The list survives a restart.
	if s := readFullState(t, configured(t, cfg)); len(s.ClaudeCode.Models) != 3 || s.ClaudeCode.SeenAt == nil {
		t.Fatalf("after restart = %+v", s.ClaudeCode)
	}
}

func TestInterceptPassesEverythingElseThrough(t *testing.T) {
	p := configured(t, "data-dir: "+t.TempDir()+"\nclaude-code: {models: {gpt-6-sol: false}}\n")
	for name, raw := range map[string][]byte{
		"openai format":   interceptRequest(t, "openai", "claude-code/2.1.327", "", claudeListBody),
		"inference":       interceptRequest(t, "claude", "claude-cli/2.1.327 (external, sdk-cli)", "claude-opus-5-5", `{"id":"msg"}`),
		"other client":    interceptRequest(t, "claude", "Anthropic/Python 1.0", "", claudeListBody),
		"own loopback":    interceptRequest(t, "claude", ID+"/"+Version, "", claudeListBody),
		"codex catalog":   interceptRequest(t, "openai", ID+"/"+Version, "", codexCatalog),
		"not a list":      interceptRequest(t, "claude", "claude-code/2.1.327", "", `{"type":"error"}`),
		"malformed":       []byte("{"),
		"empty":           nil,
		"everything kept": interceptRequest(t, "claude", "claude-code/2.1.327", "", `{"data":[{"id":"claude-opus-5-5"}],"first_id":"claude-opus-5-5","has_more":false,"last_id":"claude-opus-5-5"}`),
	} {
		if got := interceptBody(t, p, raw); got != "" {
			t.Errorf("%s rewritten: %s", name, got)
		}
	}
	off := configured(t, "data-dir: "+t.TempDir()+"\nclaude-code: {filter: false, models: {gpt-6-sol: false}}\n")
	if got := interceptBody(t, off, interceptRequest(t, "claude", "claude-code/2.1.327", "", claudeListBody)); got != "" {
		t.Fatalf("filter off rewrote: %s", got)
	}
	if got := interceptBody(t, New(), interceptRequest(t, "claude", "claude-code/2.1.327", "", claudeListBody)); got != "" {
		t.Fatalf("unconfigured rewrote: %s", got)
	}
}

type fullState struct {
	state
	Version     string           `json:"version"`
	Antigravity antigravityState `json:"antigravity"`
	ClaudeCode  claudeCodeState  `json:"claude_code"`
}

func readFullState(t *testing.T, p *Plugin) fullState {
	t.Helper()
	out := serve(t, p, stateRequest())
	if out.StatusCode != http.StatusOK {
		t.Fatalf("state = %d %s", out.StatusCode, out.Body)
	}
	var s fullState
	if err := json.Unmarshal(out.Body, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStateReportsEachClient(t *testing.T) {
	cpa := newGateway(t)
	dir := t.TempDir()
	p := configured(t, agyConfig(cpa, dir, "antigravity:\n  models: {grok-4.7: true, gpt-image-2: true}\n  default-model: gpt-6-sol\n  title-model: gemini-3.1-flash-lite\n"))
	s := readFullState(t, p)
	if s.Version != Version || s.CatalogPath != CatalogPath || len(s.Models) != 0 {
		t.Fatalf("codex = %+v", s.state)
	}
	a := s.Antigravity
	if a.ConfigKey != "antigravity" || a.Path != AgyPath || !a.Configured || a.Using != "config" || a.NewModels != "disabled" ||
		a.DefaultModel != "gpt-6-sol" || a.TitleModel != "gemini-3.1-flash-lite" || a.FetchedAt != nil || a.Preview != nil || len(a.Models) != 0 || a.ModelsSource != "" {
		t.Fatalf("antigravity before any fetch = %+v", a)
	}
	c := s.ClaudeCode
	if c.ConfigKey != "claude-code" || c.Configured || c.Using != "config" || !c.Filter || c.NewModels != "enabled" || len(c.Models) != 0 {
		t.Fatalf("claude before any fetch = %+v", c)
	}
	// Before agy fetches, its section lists the models Codex was offered.
	serve(t, p, codexRequest())
	if s := readFullState(t, p); s.Antigravity.ModelsSource != "codex" || len(s.Antigravity.Models) != 6 || s.ClaudeCode.ModelsSource != "codex" {
		t.Fatalf("universe = %s %d %s", s.Antigravity.ModelsSource, len(s.Antigravity.Models), s.ClaudeCode.ModelsSource)
	}
	p.now = func() time.Time { return time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC) }
	agyIDs(t, serve(t, p, agyRequest(clientKey)))
	a = readFullState(t, p).Antigravity
	if a.ModelsSource != "client" || a.FetchedAt == nil || a.ServedFrom != "cpa" || a.ServedAt == nil || a.Preview == nil || len(a.Models) != 5 {
		t.Fatalf("antigravity after fetch = %+v", a)
	}
	pv := a.Preview
	if pv.DefaultModelID != "gpt-6-sol" || pv.TitleModelID != "gemini-3.1-flash-lite" || pv.SlotsUsed != 3 || pv.SlotLimit != 32 || len(pv.Entries) != 4 {
		t.Fatalf("preview = %+v", pv)
	}
	byID := map[string]agyModel{}
	for _, m := range a.Models {
		byID[m.ID] = m
	}
	// The default first, then by id: gpt-6-sol, gemini-3.1-flash-lite, gpt-image-2, grok-4.7.
	if m := byID["grok-4.7"]; !m.Enabled || !m.Switched || !m.InList || m.Position != 4 || m.Slot != "placeholder" || m.Tokens != "clamped" || m.MaxOutputTokens != 65536 {
		t.Fatalf("grok = %+v", m)
	}
	if m := byID["gemini-4-argon-high"]; m.Enabled || m.InList || !m.Builtin || m.BuiltinEnum != 1417 || m.Tokens != "builtin" || m.Label != "Gemini 4 Argon (High)" {
		t.Fatalf("argon = %+v", m)
	}
	if m := byID["gpt-image-2"]; !m.NonText || !m.Enabled || !m.InList || m.HiddenByAgy {
		t.Fatalf("image = %+v", m)
	}
	if m := byID["gemini-3.1-flash-lite"]; !m.Enabled || m.Switched || !m.InList || m.Slot != "builtin" {
		t.Fatalf("title = %+v", m)
	}
	cpa.set(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	serve(t, p, agyRequest(clientKey))
	if a := readFullState(t, p).Antigravity; a.ServedFrom != "last-good" || a.LastError != "cpa_unavailable" || a.LastErrorAt == nil {
		t.Fatalf("after failure = %+v", a)
	}
}

func TestStateReadsTheRealCPAList(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "agy", "testdata", "cpa-v1beta-models.json"))
	if err != nil {
		t.Fatal(err)
	}
	cpa := newFakeCPA(t)
	cpa.set(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(raw) })
	p := configured(t, agyConfig(cpa, t.TempDir(), "antigravity: {new-models: enabled}\n"))
	agyIDs(t, serve(t, p, agyRequest(clientKey)))
	a := readFullState(t, p).Antigravity
	if len(a.Models) != 89 || a.Preview.SlotsUsed != 32 || len(a.Preview.Skipped) != 29 || len(a.Preview.Entries) != 42 {
		t.Fatalf("preview: %d models, %d slots, %d skipped, %d entries", len(a.Models), a.Preview.SlotsUsed, len(a.Preview.Skipped), len(a.Preview.Entries))
	}
	nonText, skipped := 0, 0
	for _, m := range a.Models {
		if m.NonText {
			nonText++
			if m.Enabled || m.InList {
				t.Errorf("%s: non-text model selected", m.ID)
			}
		}
		if m.Skipped {
			skipped++
		}
	}
	if nonText != 18 || skipped != 29 {
		t.Fatalf("non-text %d skipped %d", nonText, skipped)
	}
	// Listed by id, as agy receives them, not in CPA's per-reload order.
	if !slices.IsSortedFunc(a.Models, func(x, y agyModel) int { return strings.Compare(x.ID, y.ID) }) {
		t.Fatal("antigravity models are not sorted by id")
	}
}

func TestConcurrentAgyInterceptStateAndReconfigure(t *testing.T) {
	cpa := newGateway(t)
	dir := t.TempDir()
	cfg := agyConfig(cpa, dir, "antigravity: {models: {grok-4.7: true}}\nclaude-code: {models: {gpt-6-sol: false}}\n")
	p := configured(t, cfg)
	agyRaw, stateRaw := encode(t, agyRequest(clientKey)), encode(t, stateRequest())
	listRaw := interceptRequest(t, "claude", "claude-code/2.1.327", "", claudeListBody)
	other := lifecycle(t, agyConfig(cpa, dir, "antigravity: {models: {grok-4.7: true, gpt-6-sol: true}}\nclaude-code: {models: {gpt-6-sol: false}, filter: true}\n"), protocol.SchemaVersion)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 30 {
				switch i % 4 {
				case 0:
					_, _ = p.Handle(protocol.MethodPluginReconfigure, other)
				case 1:
					if result, _ := p.Handle(protocol.MethodManagementHandle, stateRaw); result.(protocol.ManagementResponse).StatusCode != http.StatusOK {
						t.Error("state failed")
						return
					}
				case 2:
					result, _ := p.Handle(protocol.MethodManagementHandle, agyRaw)
					if out := result.(protocol.ManagementResponse); out.StatusCode != http.StatusOK || !strings.Contains(string(out.Body), "grok-4.7") {
						t.Error("agy failed")
						return
					}
				default:
					result, _ := p.Handle(protocol.MethodResponseInterceptAfter, listRaw)
					if out, ok := result.(protocol.ResponseInterceptResponse); !ok || strings.Contains(string(out.Body), "los-6-tpg") {
						t.Error("intercept failed")
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}
