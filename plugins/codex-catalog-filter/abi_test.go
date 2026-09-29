//go:build cgo

package main

import (
	"encoding/json"
	"errors"
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

// The oversized-request answer is a literal; it must stay an OK envelope whose
// result decodes to an interceptor response that changes nothing.
func TestUnchangedEnvelopeKeepsResponse(t *testing.T) {
	var env protocol.Envelope
	if err := json.Unmarshal(unchanged, &env); err != nil || !env.OK || env.Error != nil {
		t.Fatalf("unchanged = %s, %v", unchanged, err)
	}
	var result protocol.ResponseInterceptResponse
	if err := json.Unmarshal(env.Result, &result); err != nil || result.Body != nil || result.Headers != nil || result.ClearHeaders != nil {
		t.Fatalf("unchanged result = %s, %v", env.Result, err)
	}
	encoded, err := okEnvelope(protocol.ResponseInterceptResponse{})
	if err != nil || string(encoded) != string(unchanged) {
		t.Fatalf("an empty interceptor response encodes as %s, want %s", encoded, unchanged)
	}
}

func TestDispatchInterceptThroughRealPlugin(t *testing.T) {
	globalMu.Lock()
	globalPlugin = pluginimpl.New()
	globalMu.Unlock()
	defer cliproxyPluginShutdown()
	config, _ := json.Marshal(protocol.LifecycleRequest{ConfigYAML: []byte("include: ['gpt-*']\n"), SchemaVersion: protocol.SchemaVersion})
	globalMu.RLock()
	_, ok := dispatch(protocol.MethodPluginRegister, config)
	globalMu.RUnlock()
	if !ok {
		t.Fatal("register failed")
	}
	request, _ := json.Marshal(map[string]any{
		"SourceFormat": "openai", "StatusCode": 200,
		"Body": []byte(`{"models":[{"slug":"gpt-6-sol","visibility":"list"},{"slug":"claude-x","visibility":"list"}]}`),
	})
	globalMu.RLock()
	raw, ok := dispatch(protocol.MethodResponseInterceptAfter, request)
	globalMu.RUnlock()
	var env struct {
		OK     bool
		Result struct{ Body []byte }
	}
	if !ok || json.Unmarshal(raw, &env) != nil || !env.OK || string(env.Result.Body) != `{"models":[{"slug":"gpt-6-sol","visibility":"list"}]}` {
		t.Fatalf("dispatch = %s", raw)
	}
}

func TestShortcutAnswersWithoutCopying(t *testing.T) {
	never := func() []byte { t.Fatal("request view read"); return nil }
	// Field order as CPA encodes pluginapi.ResponseInterceptRequest.
	completion := []byte(`{"RequestID":"r","SourceFormat":"openai","Model":"gpt-6-sol","RequestedModel":"gpt-6-sol","Stream":false,"OriginalRequest":"e30=","Body":"e30=","StatusCode":200}`)
	catalog := []byte(`{"RequestID":"r","SourceFormat":"openai","Model":"","RequestedModel":"","Stream":false,"OriginalRequest":null,"RequestBody":null,"Body":"eyJtb2RlbHMiOltdfQ==","StatusCode":200}`)
	view := func(raw []byte) func() []byte { return func() []byte { return raw } }
	cases := []struct {
		name    string
		method  string
		size    uint64
		view    func() []byte
		handled bool
	}{
		{"oversized interceptor call", protocol.MethodResponseInterceptAfter, protocol.MaxRequestBytes + 1, never, true},
		{"ordinary completion", protocol.MethodResponseInterceptAfter, uint64(len(completion)), view(completion), true},
		{"model list candidate", protocol.MethodResponseInterceptAfter, uint64(len(catalog)), view(catalog), false},
		{"empty interceptor call", protocol.MethodResponseInterceptAfter, 0, never, false},
		{"oversized lifecycle call", protocol.MethodPluginRegister, protocol.MaxRequestBytes + 1, never, false},
	}
	for _, tc := range cases {
		answer, handled := shortcut(tc.method, tc.size, tc.view)
		if handled != tc.handled || (handled && string(answer) != string(unchanged)) {
			t.Errorf("%s: answer %s, handled %v", tc.name, answer, handled)
		}
	}
}

func TestABIErrorsAndPanicsDoNotEchoPayload(t *testing.T) {
	for _, panicInstead := range []bool{false, true} {
		globalMu.Lock()
		globalPlugin = stubNativePlugin{handle: func(string, []byte) (any, error) {
			if panicInstead {
				panic("secret-payload")
			}
			return nil, errors.New("secret-payload")
		}}
		globalMu.Unlock()
		globalMu.RLock()
		raw, ok := dispatch(protocol.MethodResponseInterceptAfter, nil)
		globalMu.RUnlock()
		if ok || strings.Contains(string(raw), "secret-payload") {
			t.Fatalf("unsafe result %s", raw)
		}
		cliproxyPluginShutdown()
	}
}

func TestShutdownDrainsAdmittedNativeCall(t *testing.T) {
	entered, release, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	globalMu.Lock()
	globalPlugin = stubNativePlugin{
		handle:   func(string, []byte) (any, error) { close(entered); <-release; return struct{}{}, nil },
		shutdown: func() { close(stopped) },
	}
	globalMu.Unlock()
	callDone := make(chan struct{})
	go func() {
		globalMu.RLock()
		dispatch(protocol.MethodResponseInterceptAfter, nil)
		globalMu.RUnlock()
		close(callDone)
	}()
	<-entered
	shutdownDone := make(chan struct{})
	go func() { cliproxyPluginShutdown(); close(shutdownDone) }()
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
	globalMu.RLock()
	_, ok := dispatch(protocol.MethodResponseInterceptAfter, nil)
	globalMu.RUnlock()
	if ok {
		t.Fatal("dispatch accepted after shutdown")
	}
	cliproxyPluginShutdown() // idempotent
}
