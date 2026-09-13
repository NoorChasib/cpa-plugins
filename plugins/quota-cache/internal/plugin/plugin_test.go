package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

func TestReconfigureEquivalentCachePath(t *testing.T) {
	p := New(&testHost{})
	defer p.Shutdown()
	abs := filepath.Join(t.TempDir(), "cache", "snapshot.json")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(cwd, abs)
	if err != nil {
		t.Fatal(err)
	}
	configure := func(method, path string) error {
		raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("enabled: true\ncache-path: " + path + "\n")})
		_, err := p.Handle(method, raw)
		return err
	}
	if err := configure(protocol.MethodPluginRegister, rel); err != nil {
		t.Fatal(err)
	}
	writer := p.cache
	if err := configure(protocol.MethodPluginReconfigure, abs); err != nil {
		t.Fatalf("same physical cache path must remain registered: %v", err)
	}
	if writer != p.cache {
		t.Fatal("equivalent path restarted the writer")
	}
}

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

func TestPrivateDataRouteAndStaticSidebarAreRegistered(t *testing.T) {
	p := New(&testHost{})
	defer p.Shutdown()
	result, err := p.Handle(protocol.MethodManagementRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	registration := result.(protocol.ManagementRegistration)
	if len(registration.Resources) != 1 || registration.Resources[0].Menu != "Quota Cache" || len(registration.Routes) != 1 || registration.Routes[0].Method != "GET" || registration.Routes[0].Menu != "" {
		t.Fatal("expected static sidebar and private read-only data route")
	}
}

func TestReconfigureScheduleKeepsWriter(t *testing.T) {
	p := New(&testHost{})
	defer p.Shutdown()
	path := filepath.Join(t.TempDir(), "cache", "snapshot.json")
	configure := func(method, interval, spacing string) error {
		raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("cache-path: " + path + "\npoll-interval: " + interval + "\nrequest-spacing: " + spacing + "\n")})
		_, err := p.Handle(method, raw)
		return err
	}
	if err := configure(protocol.MethodPluginRegister, "15m", "10s"); err != nil {
		t.Fatal(err)
	}
	writer := p.cache
	for _, schedule := range [][2]string{{"5m", "10s"}, {"5m", "1s"}, {"30m", "20s"}} {
		if err := configure(protocol.MethodPluginReconfigure, schedule[0], schedule[1]); err != nil {
			t.Fatalf("valid schedule edit must remain registered: %v", err)
		}
		if p.cache != writer {
			t.Fatal("schedule edit replaced the single writer")
		}
	}
}
