package plugin

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
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
