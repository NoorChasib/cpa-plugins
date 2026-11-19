//go:build cgo

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	pluginimpl "github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/plugin"
	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/protocol"
)

type stubNativePlugin struct {
	handle   func(string, []byte) (any, error)
	shutdown func()
}

func (p stubNativePlugin) Handle(method string, raw []byte) (any, error) {
	return p.handle(method, raw)
}
func (p stubNativePlugin) Shutdown() {
	if p.shutdown != nil {
		p.shutdown()
	}
}

func install(t *testing.T, p nativePlugin) {
	t.Helper()
	gateMu.Lock()
	globalPlugin, stopping = p, false
	gateMu.Unlock()
	t.Cleanup(shutdownNative)
}

func TestABIEnvelopes(t *testing.T) {
	raw, err := okEnvelope(map[string]bool{"registered": true})
	if err != nil {
		t.Fatal(err)
	}
	var env protocol.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK || env.Error != nil || string(env.Result) != `{"registered":true}` {
		t.Fatalf("envelope = %s", raw)
	}
	raw = errorEnvelope("rejected", "request rejected")
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error == nil || env.Error.Code != "rejected" {
		t.Fatalf("envelope = %s", raw)
	}
}

// A full native round trip through the real plugin, with CPA's catalog served
// by a local stand-in, as the resource handler would see it.
func TestDispatchServesFilteredCatalogThroughRealPlugin(t *testing.T) {
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.URL.Query().Get("client_version") != "0.159.0" || r.Header.Get("Authorization") != "Bearer client-key" {
			http.Error(w, `{"error":"unexpected"}`, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-6-sol","visibility":"list"},{"slug":"claude-x","visibility":"list"}]}`))
	}))
	defer cpa.Close()
	install(t, pluginimpl.New())
	config, _ := json.Marshal(protocol.LifecycleRequest{ConfigYAML: []byte("models: {claude-x: false}\ncpa-url: " + cpa.URL + "\ndata-dir: " + t.TempDir() + "\n"), SchemaVersion: protocol.SchemaVersion})
	request, _ := json.Marshal(protocol.ManagementRequest{
		Method: http.MethodGet, Path: pluginimpl.CatalogPath,
		Headers: http.Header{"Authorization": {"Bearer client-key"}},
		Query:   url.Values{"client_version": {"0.159.0"}},
	})
	_, registered := handleCall(protocol.MethodPluginRegister, config)
	raw, ok := handleCall(protocol.MethodManagementHandle, request)
	var env struct {
		OK     bool
		Result protocol.ManagementResponse
	}
	if !registered || !ok || json.Unmarshal(raw, &env) != nil || !env.OK || env.Result.StatusCode != http.StatusOK ||
		string(env.Result.Body) != `{"models":[{"slug":"gpt-6-sol","visibility":"list"}]}` {
		t.Fatalf("dispatch = %s", raw)
	}
}

// The real plugin rewrites a Claude-format list for Claude Code through the
// native boundary and passes every other response through.
func TestInterceptRewritesClaudeCodeListThroughRealPlugin(t *testing.T) {
	install(t, pluginimpl.New())
	config, _ := json.Marshal(protocol.LifecycleRequest{ConfigYAML: []byte("data-dir: " + t.TempDir() + "\nclaude-code:\n  models: {gpt-6-sol: false}\n"), SchemaVersion: protocol.SchemaVersion})
	reg, ok := handleCall(protocol.MethodPluginRegister, config)
	if !ok || !strings.Contains(string(reg), `"response_interceptor":true`) {
		t.Fatalf("registration = %s", reg)
	}
	list := `{"data":[{"id":"claude-sonnet-5-5","display_name":"Claude Sonnet 5.5"},{"id":"claude-fable-5-dd-los-6-tpg","display_name":"GPT 6.0 Sol"}],"first_id":"claude-sonnet-5-5","has_more":false,"last_id":"claude-fable-5-dd-los-6-tpg"}`
	request := func(format, agent, model string) []byte {
		raw, _ := json.Marshal(map[string]any{
			"RequestID": "r1", "SourceFormat": format, "Model": model, "RequestedModel": "", "Stream": false,
			"RequestHeaders":  http.Header{"User-Agent": {agent}, "Authorization": {"Bearer secret"}},
			"ResponseHeaders": http.Header{"Content-Type": {"application/json"}},
			"OriginalRequest": nil, "RequestBody": nil, "Body": []byte(list), "StatusCode": 200, "Metadata": nil,
		})
		return raw
	}
	raw, ok := handleCall(protocol.MethodResponseInterceptAfter, request("claude", "claude-code/2.1.327", ""))
	var env struct {
		OK     bool
		Result protocol.ResponseInterceptResponse
	}
	if !ok || json.Unmarshal(raw, &env) != nil || !env.OK ||
		string(env.Result.Body) != `{"data":[{"id":"claude-sonnet-5-5","display_name":"Claude Sonnet 5.5"}],"first_id":"claude-sonnet-5-5","has_more":false,"last_id":"claude-sonnet-5-5"}` {
		t.Fatalf("intercept = %s", raw)
	}
	for _, other := range [][]byte{
		request("openai", "claude-code/2.1.327", ""),
		request("claude", "claude-cli/2.1.327 (external, sdk-cli)", "claude-sonnet-5-5"),
		request("claude", pluginimpl.ID+"/"+pluginimpl.Version, ""),
		[]byte("not json"),
	} {
		if raw, ok := handleCall(protocol.MethodResponseInterceptAfter, other); !ok || string(raw) != string(interceptPassThrough) {
			t.Fatalf("not a list for Claude Code, got %s", raw)
		}
	}
}

func TestInterceptCallsAlwaysPassThroughOnFailure(t *testing.T) {
	// Not initialized.
	if raw, ok := handleCall(protocol.MethodResponseInterceptAfter, []byte(`{}`)); !ok || string(raw) != string(interceptPassThrough) {
		t.Fatalf("uninitialized = %s", raw)
	}
	install(t, stubNativePlugin{handle: func(string, []byte) (any, error) { panic("boom") }})
	if raw, ok := handleCall(protocol.MethodResponseInterceptAfter, []byte(`{}`)); !ok || string(raw) != string(interceptPassThrough) {
		t.Fatalf("panic = %s", raw)
	}
	// Oversized: never handed to the plugin.
	big := make([]byte, protocol.MaxInterceptRequestBytes+1)
	if raw, ok := handleCall(protocol.MethodResponseInterceptAfter, big); !ok || string(raw) != string(interceptPassThrough) {
		t.Fatalf("oversized = %s", raw)
	}
	if _, ok := handleCall(protocol.MethodManagementHandle, make([]byte, protocol.MaxRequestBytes+1)); ok {
		t.Fatal("oversized management call accepted")
	}
}

func TestABIErrorsAndPanicsDoNotEchoPayload(t *testing.T) {
	for _, panicInstead := range []bool{false, true} {
		install(t, stubNativePlugin{handle: func(string, []byte) (any, error) {
			if panicInstead {
				panic("secret-payload")
			}
			return nil, errors.New("secret-payload")
		}})
		raw, ok := handleCall(protocol.MethodManagementHandle, nil)
		if ok || strings.Contains(string(raw), "secret-payload") {
			t.Fatalf("unsafe result %s", raw)
		}
		shutdownNative()
	}
}

func TestShutdownDrainsAdmittedNativeCall(t *testing.T) {
	entered, release, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	install(t, stubNativePlugin{
		handle:   func(string, []byte) (any, error) { close(entered); <-release; return struct{}{}, nil },
		shutdown: func() { close(stopped) },
	})
	callDone := make(chan struct{})
	go func() {
		handleCall(protocol.MethodManagementHandle, nil)
		close(callDone)
	}()
	<-entered
	shutdownDone := make(chan struct{})
	go func() { shutdownNative(); close(shutdownDone) }()
	select {
	case <-stopped:
		t.Fatal("shutdown ran while an admitted native call was in flight")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not drain")
	}
	<-callDone
	if _, ok := handleCall(protocol.MethodManagementHandle, nil); ok {
		t.Fatal("dispatch accepted after shutdown")
	}
	shutdownNative() // idempotent
}

// A plugin route that fetches from CPA makes CPA call this library's response
// interceptor while the route is still in flight. With Shutdown waiting, that
// nested call must not block: it passes the response through at once, the
// route finishes, and Shutdown completes, with no timeout involved.
func TestNestedInterceptDuringShutdownDoesNotDeadlock(t *testing.T) {
	entered, stopped := make(chan struct{}), make(chan struct{})
	var nested []byte
	var nestedOK bool
	install(t, stubNativePlugin{
		handle: func(method string, _ []byte) (any, error) {
			if method == protocol.MethodResponseInterceptAfter {
				t.Error("nested interceptor reached the plugin during shutdown")
				return struct{}{}, nil
			}
			close(entered)
			// Wait until Shutdown has started refusing new calls.
			for {
				gateMu.Lock()
				s := stopping
				gateMu.Unlock()
				if s {
					break
				}
				time.Sleep(time.Millisecond)
			}
			nested, nestedOK = handleCall(protocol.MethodResponseInterceptAfter, []byte(`{"SourceFormat":"gemini"}`))
			return struct{}{}, nil
		},
		shutdown: func() { close(stopped) },
	})
	outer := make(chan struct{})
	go func() { handleCall(protocol.MethodManagementHandle, nil); close(outer) }()
	<-entered
	go shutdownNative()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown deadlocked behind a nested interceptor call")
	}
	<-outer
	if !nestedOK || string(nested) != string(interceptPassThrough) {
		t.Fatalf("nested call = %s", nested)
	}
}
