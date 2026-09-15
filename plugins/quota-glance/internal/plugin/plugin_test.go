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
	cfg := "cache-path: " + cachePath + "\ndata-dir: " + filepath.Join(dir, "data") + "\nweb-token: test-token\nstale-after: 45m\n"
	if _, err := configure(t, p, cfg); err != nil {
		t.Fatal(err)
	}
	return p, host, cachePath
}

// The document is on the management tree, which CPA has already authenticated
// by the time Handle sees the request; nothing is presented here.
func resource(t *testing.T, p *Plugin, suffix string, headers http.Header) protocol.ManagementResponse {
	t.Helper()
	raw, err := json.Marshal(protocol.ManagementRequest{
		Method: "GET", Path: "/v0/resource/plugins/quota-glance" + suffix, Headers: headers,
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
	// The fallback path is public, so it authenticates itself.
	if res := resource(t, p, "/summary", nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d; the fallback path must authenticate", res.StatusCode)
	}
	if res := resource(t, p, "/summary", http.Header{"Authorization": {"Bearer test-token"}}); res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; the configured token must work", res.StatusCode)
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
	// Three authenticated routes, and two public ones: the page, which carries
	// no data, and the document's fallback path, which carries this plugin's
	// own token check because CPA carries none.
	if len(registration.Routes) != 3 || len(registration.Resources) != 2 {
		t.Fatalf("registration = %+v", registration)
	}
	if registration.Resources[0].Path != "/app" || registration.Resources[1].Path != "/summary" {
		t.Fatalf("public resources = %+v", registration.Resources)
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

// The operator has no other way to learn a generated token, so it is logged
// once at generation. A configured token is never logged at all.
func TestGeneratedTokenIsLoggedOnceAndConfiguredTokensNever(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "snapshot.json")
	if err := os.WriteFile(cachePath, fixtureSnapshot(t), 0o600); err != nil {
		t.Fatal(err)
	}
	base := "cache-path: " + cachePath + "\ndata-dir: " + filepath.Join(dir, "data") + "\n"

	host := &fakeHost{}
	p := New(host)
	t.Cleanup(p.Shutdown)
	if _, err := configure(t, p, base+"web-token: \"\"\n"); err != nil {
		t.Fatal(err)
	}
	generated := 0
	var token string
	for _, line := range host.logged() {
		if value, ok := line.fields["web_token"].(string); ok {
			generated++
			token = value
		}
	}
	if generated != 1 || token == "" {
		t.Fatalf("generated token logged %d times", generated)
	}
	// The generated token actually works.
	if res := resource(t, p, "/summary", http.Header{"Authorization": {"Bearer " + token}}); res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}

	configured := &fakeHost{}
	q := New(configured)
	t.Cleanup(q.Shutdown)
	if _, err := configure(t, q, base+"web-token: configured-secret-value\n"); err != nil {
		t.Fatal(err)
	}
	for _, line := range configured.logged() {
		rendered := line.message
		for _, value := range line.fields {
			rendered += " " + strings.TrimSpace(strings.Join(strings.Fields(toString(value)), " "))
		}
		if strings.Contains(rendered, "configured-secret-value") {
			t.Fatalf("a configured token reached the log: %s", rendered)
		}
	}
}

// A generated token is persisted, so a restart does not silently invalidate a
// bookmarked dashboard URL.
func TestGeneratedTokenSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "snapshot.json")
	if err := os.WriteFile(cachePath, fixtureSnapshot(t), 0o600); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(dir, "data")
	cfg := "cache-path: " + cachePath + "\ndata-dir: " + dataDir + "\nweb-token: \"\"\n"

	tokenOf := func(host *fakeHost) string {
		t.Helper()
		for _, line := range host.logged() {
			if value, ok := line.fields["web_token"].(string); ok {
				return value
			}
		}
		return ""
	}

	first := &fakeHost{}
	p := New(first)
	if _, err := configure(t, p, cfg); err != nil {
		t.Fatal(err)
	}
	token := tokenOf(first)
	if token == "" {
		t.Fatal("no token was generated")
	}
	p.Shutdown()

	// A fresh process against the same data directory reuses it, and does not
	// log it a second time.
	second := &fakeHost{}
	q := New(second)
	t.Cleanup(q.Shutdown)
	if _, err := configure(t, q, cfg); err != nil {
		t.Fatal(err)
	}
	if again := tokenOf(second); again != "" {
		t.Fatalf("a persisted token was regenerated and logged again")
	}
	if res := resource(t, q, "/summary", http.Header{"Authorization": {"Bearer " + token}}); res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; the token from the first start must still work", res.StatusCode)
	}

	// Reconfiguring within a process must not mint a new one either.
	third := len(second.logged())
	if _, err := configure(t, q, cfg); err != nil {
		t.Fatal(err)
	}
	if tokenOf(second) != "" {
		t.Fatal("reconfigure regenerated the token")
	}
	_ = third
	info, err := os.Stat(filepath.Join(dataDir, tokenFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("token file mode = %v; want 0600", perm)
	}
}

// builtDocument is the first document with the fixture roster in it. The API
// serves a valid empty one from construction, so a test that waits for "a
// document" is served that instead and asserts against nothing.
func builtDocument(doc map[string]any) bool {
	credentials, ok := doc["credentials"].([]any)
	return ok && len(credentials) == 7
}

// samplesOn counts the trend history on disk.
func samplesOn(t *testing.T, dataDir string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dataDir, "history.json"))
	if err != nil {
		t.Fatalf("history unreadable: %v", err)
	}
	var doc struct {
		Samples []json.RawMessage `json:"samples"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return len(doc.Samples)
}

// Rebuilds now run on a timer as well as on a write, because request activity
// moves while the snapshot sits still. History must not follow: a sample is an
// observation, and quota-cache makes one every fifteen minutes however often
// this plugin redraws. Recording every rebuild would store the same reading
// sixty times an hour, inflate the ring fifteenfold, and feed the trend an hour
// of history that contains one actual poll.
func TestHistoryRecordsSnapshotsNotRebuilds(t *testing.T) {
	p, _, cachePath := newFixturePlugin(t)
	dataDir := filepath.Join(filepath.Dir(cachePath), "data")

	// The API serves an empty document until the first build lands, so wait for
	// the built one rather than for any document at all.
	waitForDocument(t, p, builtDocument)
	first := samplesOn(t, dataDir)
	if first == 0 {
		t.Fatal("the startup read recorded no history at all")
	}

	// What the heartbeat does: rebuild, with the same snapshot underneath.
	for range 5 {
		p.Rebuild()
	}
	if got := samplesOn(t, dataDir); got != first {
		t.Fatalf("history grew from %d to %d samples across five rebuilds of one snapshot", first, got)
	}

	// A genuinely new snapshot is still recorded. Same readings, later write:
	// it is the write that makes it an observation.
	fresh := strings.Replace(string(fixtureSnapshot(t)),
		`"written_at": "2026-09-10T03:55:00Z"`, `"written_at": "2026-09-10T04:10:00Z"`, 1)
	if strings.Contains(fresh, "03:55:00Z\",\n  \"next_request") {
		t.Fatal("fixture write time did not change; the test is asserting nothing")
	}
	if err := os.WriteFile(cachePath, []byte(fresh), 0o600); err != nil {
		t.Fatal(err)
	}
	p.Rebuild()
	if got := samplesOn(t, dataDir); got <= first {
		t.Fatalf("a new snapshot added no history: %d samples, was %d", got, first)
	}
}

// The strip is only as live as the document, and the document is only as live
// as the roster read that built it. A rebuild with an unchanged snapshot must
// still pick up new traffic.
func TestARebuildPicksUpNewActivityWithoutANewSnapshot(t *testing.T) {
	p, host, _ := newFixturePlugin(t)
	waitForDocument(t, p, builtDocument)

	host.mu.Lock()
	for i := range host.files {
		if host.files[i].AuthIndex == "claude-agency@example.com.json" {
			host.files[i].RecentRequests = []protocol.HostRecentRequestEntry{
				{Time: "03:50-04:00", Success: 2},
				{Time: "04:00-04:10", Success: 9},
			}
		}
	}
	host.mu.Unlock()

	p.Rebuild()
	doc := waitForDocument(t, p, func(doc map[string]any) bool {
		for _, raw := range doc["credentials"].([]any) {
			credential := raw.(map[string]any)
			if credential["id"] != "claude-agency@example.com.json" {
				continue
			}
			return credential["activity"] != nil
		}
		return false
	})

	for _, raw := range doc["credentials"].([]any) {
		credential := raw.(map[string]any)
		if credential["id"] != "claude-agency@example.com.json" {
			continue
		}
		activity := credential["activity"].(map[string]any)
		if activity["success"].(float64) != 11 || activity["live"] != true {
			t.Fatalf("activity did not follow the roster: %+v", activity)
		}
		return
	}
	t.Fatal("the credential vanished from the document")
}
