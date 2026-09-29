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
	globalMu.Lock()
	globalPlugin = pluginimpl.New()
	globalMu.Unlock()
	defer cliproxyPluginShutdown()
	config, _ := json.Marshal(protocol.LifecycleRequest{ConfigYAML: []byte("include: ['gpt-*']\ncpa-url: " + cpa.URL + "\n"), SchemaVersion: protocol.SchemaVersion})
	request, _ := json.Marshal(protocol.ManagementRequest{
		Method: http.MethodGet, Path: pluginimpl.CatalogPath,
		Headers: http.Header{"Authorization": {"Bearer client-key"}},
		Query:   url.Values{"client_version": {"0.159.0"}},
	})
	globalMu.RLock()
	_, registered := dispatch(protocol.MethodPluginRegister, config)
	raw, ok := dispatch(protocol.MethodManagementHandle, request)
	globalMu.RUnlock()
	var env struct {
		OK     bool
		Result protocol.ManagementResponse
	}
	if !registered || !ok || json.Unmarshal(raw, &env) != nil || !env.OK || env.Result.StatusCode != http.StatusOK ||
		string(env.Result.Body) != `{"models":[{"slug":"gpt-6-sol","visibility":"list"}]}` {
		t.Fatalf("dispatch = %s", raw)
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
		raw, ok := dispatch(protocol.MethodManagementHandle, nil)
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
		dispatch(protocol.MethodManagementHandle, nil)
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
	_, ok := dispatch(protocol.MethodManagementHandle, nil)
	globalMu.RUnlock()
	if ok {
		t.Fatal("dispatch accepted after shutdown")
	}
	cliproxyPluginShutdown() // idempotent
}
