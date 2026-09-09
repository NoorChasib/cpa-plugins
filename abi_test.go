//go:build cgo

package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-token-usage/internal/protocol"
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
		raw, ok := dispatch("usage.handle", nil)
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
		dispatch("usage.handle", nil)
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
	_, ok := dispatch("usage.handle", nil)
	globalMu.RUnlock()
	if ok {
		t.Fatal("dispatch accepted after shutdown")
	}
	cliproxyPluginShutdown() // idempotent
}
