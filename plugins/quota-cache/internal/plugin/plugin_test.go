package plugin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

type testHost struct{ lists atomic.Int32 }

func (h *testHost) ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error) {
	h.lists.Add(1)
	return nil, nil
}
func (*testHost) GetAuth(context.Context, string) ([]byte, error) {
	panic("unexpected credential read")
}
func (*testHost) HTTPDo(context.Context, protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	panic("unexpected HTTP")
}
func (*testHost) Log(context.Context, string, string, map[string]any) {}

func TestDisabledRegistrationDoesNotStartPollerAndUnknownConfigFails(t *testing.T) {
	host := &testHost{}
	p := New(host)
	defer p.Shutdown()
	path := filepath.Join(t.TempDir(), "cache", "snapshot.json")
	raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("enabled: false\ncache-path: " + path + "\n")})
	if _, err := p.Handle(protocol.MethodPluginRegister, raw); err != nil {
		t.Fatal(err)
	}
	if p.cache != nil || host.lists.Load() != 0 {
		t.Fatal("disabled plugin started polling")
	}
	raw, _ = json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("unknown-setting: true\n")})
	if _, err := p.Handle(protocol.MethodPluginReconfigure, raw); err == nil {
		t.Fatal("unknown setting accepted")
	}
}

func TestOnlyPrivateReadRouteIsRegistered(t *testing.T) {
	p := New(&testHost{})
	defer p.Shutdown()
	result, err := p.Handle(protocol.MethodManagementRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	registration := result.(protocol.ManagementRegistration)
	if len(registration.Resources) != 0 || len(registration.Routes) != 1 || registration.Routes[0].Method != "GET" || registration.Routes[0].Menu != "" {
		t.Fatal("unexpected public or mutating route")
	}
}
