package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/warmup"
)

func TestWarmupSettingsAreOfferedAndApplyWithoutRestart(t *testing.T) {
	p := New(&testHost{})
	defer p.Shutdown()
	path := filepath.Join(t.TempDir(), "cache", "snapshot.json")
	configure := func(extra string) protocol.Registration {
		t.Helper()
		raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("cache-path: " + path + "\n" + extra)})
		answer, err := p.Handle(protocol.MethodPluginReconfigure, raw)
		if err != nil {
			t.Fatal(err)
		}
		return answer.(protocol.Registration)
	}
	registration := configure("")
	before := p.cache
	fields := map[string]protocol.ConfigField{}
	for _, field := range registration.Metadata.ConfigFields {
		fields[field.Name] = field
	}
	if fields["warmup-model"].Type != "string" || fields["warmup-enabled"].Type != "boolean" || fields["warmup-effort"].Type != "enum" {
		t.Fatal("plugin settings omit warm-up controls")
	}
	configure("warmup-enabled: true\nwarmup-model: claude-future-model\nwarmup-effort: medium\nwarmup-hours: 09:00-17:00\nwarmup-timezone: Australia/Sydney\n")
	state := p.warmup.Status(time.Now())
	if p.cache != before || !state.Enabled || state.Model != "claude-future-model" || state.Effort != "medium" || state.Hours != "09:00-17:00" {
		t.Fatalf("live settings = %+v", state)
	}
	configure("warmup-enabled: true\nwarmup-cli-path: /opt/claude\nwarmup-profile-root: /data/claude-profiles\n")
	if !p.warmup.Status(time.Now()).Enabled {
		t.Fatal("a configuration written for 0.3.0 no longer loads")
	}
	configure("warmup-enabled: false\n")
	if p.warmup.Status(time.Now()).Enabled {
		t.Fatal("disable did not apply")
	}
	configure("warmup-enabled: true\n")
	raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("cache-path: " + path + "\nwarmup-model: invalid model name\n")})
	if _, err := p.Handle(protocol.MethodPluginReconfigure, raw); err == nil {
		t.Fatal("accepted an invalid model")
	}
	if p.warmup.Status(time.Now()).Enabled {
		t.Fatal("rejected configuration left inference running")
	}
}

func TestWarmAllRequiresTheManagementPOSTAndExplicitEnable(t *testing.T) {
	p := New(&testHost{})
	defer p.Shutdown()
	raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("cache-path: " + filepath.Join(t.TempDir(), "cache", "snapshot.json") + "\n")})
	if _, err := p.Handle(protocol.MethodPluginRegister, raw); err != nil {
		t.Fatal(err)
	}
	for _, request := range []struct {
		method, path string
		want         int
	}{
		{"POST", warmupPath, 409},
		{"GET", warmupPath, 404},
		{"POST", "/v0/resource/plugins/quota-cache/warmup", 404},
	} {
		raw, _ := json.Marshal(protocol.ManagementRequest{Method: request.method, Path: request.path})
		answer, err := p.Handle(protocol.MethodManagementHandle, raw)
		if err != nil || answer.(protocol.ManagementResponse).StatusCode != request.want {
			t.Fatalf("%s %s: %v / %+v", request.method, request.path, err, answer)
		}
	}
}

// modelHost is testHost with the warm-up callbacks. It lists no account, so
// a warm-up stops at the pre-flight, after the probe.
type modelHost struct {
	testHost
	probes atomic.Int32
}

func (h *modelHost) GetRuntime(context.Context, string) (protocol.HostAuthFileEntry, error) {
	return protocol.HostAuthFileEntry{}, errors.New("host callback host.auth.get_runtime failed: host_call_failed")
}
func (h *modelHost) HTTPCancel(context.Context, string) error { h.probes.Add(1); return nil }
func (h *modelHost) ExecuteModel(context.Context, protocol.HostModelExecutionRequest) (protocol.HostModelExecutionResponse, error) {
	panic("unexpected model request")
}

// The production runner calls the plugin's own host, and outlives a stop and
// reopen of the cache, so a request it stopped waiting for still holds off
// the next warm-up after CPA quiesces and registers the plugin again.
func TestTheProductionRunnerUsesTheHostAndOutlivesACacheReopen(t *testing.T) {
	host := &modelHost{}
	p := New(host)
	defer p.Shutdown()
	raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("cache-path: " + filepath.Join(t.TempDir(), "cache", "snapshot.json") + "\n")})
	if _, err := p.Handle(protocol.MethodPluginRegister, raw); err != nil {
		t.Fatal(err)
	}
	first := p.warmer
	options, _ := warmup.Defaults().Validate()
	if err := first.Warm(context.Background(), options, warmup.Identity{AuthIndex: "0000000000000001", Plan: "max"}); err == nil || err.Error() != "account_unavailable" || host.probes.Load() != 1 {
		t.Fatalf("the production runner did not use the plugin's host: %v", err)
	}
	if _, err := p.Handle(protocol.MethodPluginQuiesce, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Handle(protocol.MethodPluginRegister, raw); err != nil {
		t.Fatal(err)
	}
	if p.cache == nil || p.warmer != first {
		t.Fatal("reopening the cache replaced the runner and its admission slot")
	}
}
