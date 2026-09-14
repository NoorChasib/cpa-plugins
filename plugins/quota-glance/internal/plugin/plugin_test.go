package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
)

type logLine struct {
	level   string
	message string
	fields  map[string]any
}

type fakeHost struct {
	mu    sync.Mutex
	files []protocol.HostAuthFileEntry
	err   error
	lines []logLine
}

func (h *fakeHost) ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.files, h.err
}

func (h *fakeHost) Log(_ context.Context, level, message string, fields map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lines = append(h.lines, logLine{level, message, fields})
}

func (h *fakeHost) logged() []logLine {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]logLine, len(h.lines))
	copy(out, h.lines)
	return out
}

func fixtureSnapshot(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "snapshots", "seven-credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func configure(t *testing.T, p *Plugin, yaml string) (protocol.Registration, error) {
	t.Helper()
	raw, err := json.Marshal(protocol.LifecycleRequest{ConfigYAML: []byte(yaml), SchemaVersion: 4})
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Handle(protocol.MethodPluginRegister, raw)
	if err != nil {
		return protocol.Registration{}, err
	}
	return result.(protocol.Registration), nil
}

func newFixturePlugin(t *testing.T) (*Plugin, *fakeHost, string) {
	t.Helper()
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "snapshot.json")
	if err := os.WriteFile(cachePath, fixtureSnapshot(t), 0o600); err != nil {
		t.Fatal(err)
	}
	host := &fakeHost{files: []protocol.HostAuthFileEntry{
		{AuthIndex: "claude-siphorchannel@example.com.json", Provider: "claude"},
		{AuthIndex: "claude-agency@example.com.json", Provider: "claude"},
		{AuthIndex: "claude-chasibnoor@example.com.json", Provider: "claude"},
		{AuthIndex: "claude-noor@example.com.json", Provider: "claude"},
		{AuthIndex: "claude-noorchasib@example.com.json", Provider: "claude"},
		{AuthIndex: "codex-noor@example.com.json", Provider: "codex"},
		{AuthIndex: "xai-noor@example.com.json", Provider: "xai"},
	}}
	p := New(host)
	t.Cleanup(p.Shutdown)
	cfg := "cache-path: " + cachePath + "\ndata-dir: " + filepath.Join(dir, "data") + "\nstale-after: 45m\n"
	if _, err := configure(t, p, cfg); err != nil {
		t.Fatal(err)
	}
	return p, host, cachePath
}

// The document is on the management tree, which CPA has already authenticated
// by the time Handle sees the request; nothing is presented here.
func resource(t *testing.T, p *Plugin, suffix string) protocol.ManagementResponse {
	t.Helper()
	raw, err := json.Marshal(protocol.ManagementRequest{
		Method: "GET", Path: "/v0/resource/plugins/quota-glance" + suffix,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Handle(protocol.MethodManagementHandle, raw)
	if err != nil {
		t.Fatal(err)
	}
	return result.(protocol.ManagementResponse)
}

func summary(t *testing.T, p *Plugin) protocol.ManagementResponse {
	t.Helper()
	raw, err := json.Marshal(protocol.ManagementRequest{
		Method: "GET", Path: "/v0/management/plugins/quota-glance/summary",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Handle(protocol.MethodManagementHandle, raw)
	if err != nil {
		t.Fatal(err)
	}
	return result.(protocol.ManagementResponse)
}

func waitForDocument(t *testing.T, p *Plugin, match func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		res := summary(t, p)
		if res.StatusCode == http.StatusOK {
			var doc map[string]any
			if json.Unmarshal(res.Body, &doc) == nil {
				last = doc
				if match(doc) {
					return doc
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("document never matched; last = %v", last)
	return nil
}

func TestPluginServesTheSnapshotItWasPointedAt(t *testing.T) {
	p, _, _ := newFixturePlugin(t)
	doc := waitForDocument(t, p, func(doc map[string]any) bool {
		credentials, _ := doc["credentials"].([]any)
		return len(credentials) == 7
	})
	// The runtime builds against the wall clock, and the committed fixture was
	// observed well before now, so it must be reported as stale rather than
	// presented as current. Freshness follows observed_at, not written_at.
	if doc["stale"] != true || doc["staleReason"] != "cacheStale" {
		t.Fatalf("stale=%v reason=%v", doc["stale"], doc["staleReason"])
	}
	// The document is only reachable through CPA's management tree; the public
	// resource tree serves the page and nothing else.
	if res := resource(t, p, "/summary"); res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d; the public tree must not serve the document", res.StatusCode)
	}
}

// If the snapshot goes away, the last good document keeps being served, marked
// stale with a reason. An empty response is indistinguishable from a broken
// install, which is the worse failure.
func TestSnapshotLossServesStaleRatherThanEmpty(t *testing.T) {
	p, _, cachePath := newFixturePlugin(t)
	waitForDocument(t, p, func(doc map[string]any) bool {
		credentials, _ := doc["credentials"].([]any)
		return len(credentials) == 7
	})

	if err := os.Remove(cachePath); err != nil {
		t.Fatal(err)
	}
	p.Rebuild()

	res := summary(t, p)
	var doc map[string]any
	if err := json.Unmarshal(res.Body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["stale"] != true || doc["staleReason"] != "cacheMissing" {
		t.Fatalf("stale=%v reason=%v", doc["stale"], doc["staleReason"])
	}
	credentials, _ := doc["credentials"].([]any)
	if len(credentials) != 7 {
		t.Fatalf("credentials = %d; the last good document must keep being served", len(credentials))
	}

	// A quota-cache schema bump blinds this plugin too, and must say so rather
	// than report a generic read failure.
	if err := os.WriteFile(cachePath, []byte(`{"schema":2,"entries":{},"provider_cooldown":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p.Rebuild()
	res = summary(t, p)
	if err := json.Unmarshal(res.Body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["staleReason"] != "snapshotSchemaUnsupported" {
		t.Fatalf("reason = %v", doc["staleReason"])
	}

	health := managementGet(t, p, "/v0/management/plugins/quota-glance/health")
	if !strings.Contains(string(health.Body), "snapshotSchemaUnsupported") {
		t.Fatalf("health does not surface the schema problem: %s", health.Body)
	}
}

func managementGet(t *testing.T, p *Plugin, path string) protocol.ManagementResponse {
	t.Helper()
	raw, err := json.Marshal(protocol.ManagementRequest{Method: "GET", Path: path, Headers: http.Header{}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Handle(protocol.MethodManagementHandle, raw)
	if err != nil {
		t.Fatal(err)
	}
	return result.(protocol.ManagementResponse)
}

// Refusing plainly beats serving an empty document that looks like a working
// install with no quota anywhere.
func TestUnreadableCachePathRefusesToStart(t *testing.T) {
	dir := t.TempDir()
	host := &fakeHost{}
	p := New(host)
	t.Cleanup(p.Shutdown)
	_, err := configure(t, p, "cache-path: "+filepath.Join(dir, "absent.json")+"\ndata-dir: "+dir+"\n")
	if err == nil {
		t.Fatal("an unreadable cache-path was accepted")
	}
	if !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("err = %v; it should name the problem plainly", err)
	}
}

func TestConfigurationIsValidated(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "snapshot.json")
	if err := os.WriteFile(cachePath, fixtureSnapshot(t), 0o600); err != nil {
		t.Fatal(err)
	}
	base := "cache-path: " + cachePath + "\ndata-dir: " + filepath.Join(dir, "data") + "\n"
	for _, tc := range []struct{ name, yaml string }{
		{"unknown field", base + "refresh-interval: 5m\n"},
		{"stale-after too small", base + "stale-after: 10s\n"},
		{"stale-after too large", base + "stale-after: 48h\n"},
		{"stale-after unparseable", base + "stale-after: soon\n"},
		{"empty cache path", "cache-path: \"\"\ndata-dir: " + dir + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := New(&fakeHost{})
			t.Cleanup(p.Shutdown)
			if _, err := configure(t, p, tc.yaml); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func toString(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	raw, _ := json.Marshal(value)
	return string(raw)
}

// Menu on a management route would make CPA publish it as a public resource.
func TestManagementRoutesCarryNoMenu(t *testing.T) {
	p := New(&fakeHost{})
	t.Cleanup(p.Shutdown)
	result, err := p.Handle(protocol.MethodManagementRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	registration := result.(protocol.ManagementRegistration)
	// Three authenticated routes; exactly one public resource, the page itself.
	// The document belongs on the authenticated tree — a resource entry for it
	// would publish every credential in the pool to anyone who can reach the
	// origin.
	if len(registration.Routes) != 3 || len(registration.Resources) != 1 {
		t.Fatalf("registration = %+v", registration)
	}
	if registration.Resources[0].Path != "/app" {
		t.Fatalf("public resource = %q; only the page is public", registration.Resources[0].Path)
	}
	for _, route := range registration.Routes {
		if route.Menu != "" {
			t.Fatalf("management route %s carries Menu %q, which CPA turns into a public resource", route.Path, route.Menu)
		}
		if route.Method != "GET" {
			t.Fatalf("route %s is %s", route.Path, route.Method)
		}
	}
	// Every registered resource path has at least one segment after the plugin
	// id; a bare "/" is rejected by CPA.
	for _, resource := range registration.Resources {
		if len(resource.Path) < 2 || !strings.HasPrefix(resource.Path, "/") {
			t.Fatalf("resource path %q needs a segment after the plugin id", resource.Path)
		}
	}
}

func TestDisablingStopsTheWatcherWithoutError(t *testing.T) {
	p, _, cachePath := newFixturePlugin(t)
	cfg := "enabled: false\ncache-path: " + cachePath + "\ndata-dir: " + filepath.Join(t.TempDir(), "data") + "\n"
	if _, err := configure(t, p, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Handle(protocol.MethodPluginQuiesce, nil); err != nil {
		t.Fatal(err)
	}
}

// A transient host.auth.list failure must not blank the dashboard, and must not
// be latched as the last good document. Latching an empty document destroys the
// fallback that every later failure depends on.
func TestRosterFailureKeepsServingTheLastGoodDocument(t *testing.T) {
	p, host, cachePath := newFixturePlugin(t)
	waitForDocument(t, p, func(doc map[string]any) bool {
		credentials, _ := doc["credentials"].([]any)
		return len(credentials) == 7
	})

	host.mu.Lock()
	host.err = errors.New("host unavailable")
	host.mu.Unlock()
	p.Rebuild()

	var doc map[string]any
	if err := json.Unmarshal(summary(t, p).Body, &doc); err != nil {
		t.Fatal(err)
	}
	credentials, _ := doc["credentials"].([]any)
	if len(credentials) != 7 {
		t.Fatalf("credentials = %d; a roster failure blanked the dashboard", len(credentials))
	}
	if doc["stale"] != true || doc["staleReason"] != "rosterUnavailable" {
		t.Fatalf("stale=%v reason=%v", doc["stale"], doc["staleReason"])
	}

	// And the fallback still works afterwards: the empty result must not have
	// overwritten the last good document.
	if err := os.Remove(cachePath); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	host.err = nil
	host.mu.Unlock()
	p.Rebuild()
	if err := json.Unmarshal(summary(t, p).Body, &doc); err != nil {
		t.Fatal(err)
	}
	credentials, _ = doc["credentials"].([]any)
	if len(credentials) != 7 || doc["staleReason"] != "cacheMissing" {
		t.Fatalf("the last good document was lost: %d credentials, reason %v", len(credentials), doc["staleReason"])
	}
}
