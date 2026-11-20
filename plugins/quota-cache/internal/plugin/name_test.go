package plugin

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

// CPA's Plugins page shows the name a plugin registers with, so it is the
// catalog's display name; the ID keeps keying the configuration and routes.
func TestRegistersItsDisplayName(t *testing.T) {
	p := New(newRefreshHost())
	t.Cleanup(p.Shutdown)
	path := filepath.Join(t.TempDir(), "cache", "snapshot.json")
	raw, err := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("cache-path: " + path + "\nrequest-spacing: 1s\n")})
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Handle(protocol.MethodPluginRegister, raw)
	if err != nil {
		t.Fatal(err)
	}
	if name := result.(protocol.Registration).Metadata.Name; name != "Quota Cache" {
		t.Fatalf("registered name = %q, want %q", name, "Quota Cache")
	}
	if ID != "quota-cache" {
		t.Fatalf("ID = %q; it keys the configuration and must not change", ID)
	}
}
