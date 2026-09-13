package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/token-usage/internal/protocol"
)

// The plug-in section produced by CPA v7.2.155's store installation. Store URLs
// and identifiers are host metadata, not discovery hints or runtime options.
const installedConfiguration = `enabled: true
store:
  id: token-usage
  name: Token Usage
  description: Persistent CPA-reported raw token usage by provider/model. Linux amd64 only; not billing or lossless accounting.
  author: NoorChasib
  version: 0.1.0
  release-tag: v0.1.0
  repository: https://github.com/NoorChasib/cpa-plugins
  license: MIT
  source-id: source-97e4ec28aa27
  source-name: raw.githubusercontent.com
  source-url: https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/registry.json
  install:
    type: github-release
`

func configurationRequest(yaml string) []byte {
	raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte(yaml)})
	return raw
}

func TestInitialStorageFailureKeepsMetadataAndStatusDiscoverableUntilRecovery(t *testing.T) {
	p := New()
	p.now = func() time.Time { return testNow }
	t.Cleanup(p.Shutdown)
	badPath := filepath.Join(t.TempDir(), "private-path-canary.sqlite")
	corrupt := []byte("private-corrupt-history-canary")
	if err := os.WriteFile(badPath, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	value, err := p.Handle(protocol.MethodPluginRegister, configurationRequest(fmt.Sprintf("database-path: %q\n", badPath)+installedConfiguration))
	if err != nil {
		t.Fatalf("valid config with unavailable storage hid registration metadata: %v", err)
	}
	registration := value.(protocol.Registration)
	if registration.SchemaVersion != 6 || registration.Metadata.Name != "Token Usage" || len(registration.Metadata.ConfigFields) == 0 || !registration.Capabilities.UsagePlugin || !registration.Capabilities.ManagementAPI {
		t.Fatalf("missing bootstrap metadata: %+v", registration)
	}
	field := registration.Metadata.ConfigFields[0]
	if field.Name != "database-path" || field.Type != "string" || !strings.Contains(field.Description, "Optional absolute") || !strings.Contains(field.Description, "plugins/data/token-usage/usage.sqlite") {
		t.Fatalf("metadata did not describe the automatic default and explicit override: %+v", field)
	}
	response := manage(t, p, "/v0/management/plugins/token-usage/status", nil)
	if response.StatusCode != 503 {
		t.Fatalf("failed storage reported success: %d %s", response.StatusCode, response.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(response.Body, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"api_schema": float64(1), "source": "cpa_reported", "version": Version, "storage": "sqlite", "state": "unavailable",
		"error": "storage_unavailable", "collection": map[string]any{"state": "unavailable", "reason": "storage_initialization_failed"},
		"upstream_completeness": "unknown",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("startup failure must have only a sanitized truthful envelope: %s", response.Body)
	}
	for _, route := range []string{"summary", "models"} {
		r := manage(t, p, "/v0/management/plugins/token-usage/"+route, query())
		if r.StatusCode != 503 || strings.Contains(string(r.Body), "observed_events") || strings.Contains(string(r.Body), "canary") {
			t.Fatalf("failed storage fabricated %s data: %d %s", route, r.StatusCode, r.Body)
		}
	}
	for _, raw := range []string{validUsage, `{`} {
		if _, err := p.Handle(protocol.MethodUsageHandle, []byte(raw)); err == nil {
			t.Fatal("failed initial storage admitted an event")
		}
	}
	p.RejectNativeUsage()
	if data, err := os.ReadFile(badPath); err != nil || string(data) != string(corrupt) {
		t.Fatalf("failed initialization reset history: %q %v", data, err)
	}
	if _, err := p.Handle(protocol.MethodPluginReconfigure, registrationRequest(filepath.Join(t.TempDir(), "private", "corrected.sqlite"))); err != nil {
		t.Fatalf("corrected configuration could not recover: %v", err)
	}
	if _, err := p.Handle(protocol.MethodUsageHandle, []byte(validUsage)); err != nil {
		t.Fatal(err)
	}
	waitCommitted(t, p, "1")
	if r := manage(t, p, "/v0/management/plugins/token-usage/summary", query()); r.StatusCode != 200 || !strings.Contains(string(r.Body), `"observed_events":"1"`) {
		t.Fatalf("corrected storage did not durably collect: %d %s", r.StatusCode, r.Body)
	}
}

func TestExplicitDatabaseKeepsHistoryWithoutUsingDefaultOrStoreDiscovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "existing.sqlite")
	seed := New()
	seed.now = func() time.Time { return testNow }
	t.Cleanup(seed.Shutdown)
	request := registrationRequest(path)
	if _, err := seed.Handle(protocol.MethodPluginRegister, request); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Handle(protocol.MethodUsageHandle, []byte(validUsage)); err != nil {
		t.Fatal(err)
	}
	waitCommitted(t, seed, "1")
	seed.Shutdown()

	cwd := t.TempDir()
	// Any attempt to use the default path must fail rather than redirect history.
	trap := filepath.Join(cwd, "plugins")
	if err := os.WriteFile(trap, []byte("default-path-canary"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	p := New()
	p.now = seed.now
	t.Cleanup(p.Shutdown)
	if _, err := p.Handle(protocol.MethodPluginRegister, request); err != nil {
		t.Fatal(err)
	}
	if r := manage(t, p, "/v0/management/plugins/token-usage/summary", query()); r.StatusCode != 200 || !strings.Contains(string(r.Body), `"observed_events":"1"`) {
		t.Fatalf("explicit existing history was redirected or reset: %d %s", r.StatusCode, r.Body)
	}
	var lifecycle protocol.LifecycleRequest
	if err := json.Unmarshal(request, &lifecycle); err != nil {
		t.Fatal(err)
	}
	// A host store update and selection metadata must not close an active
	// collector or require a restart. Changed URLs are never path hints.
	updated := strings.ReplaceAll(installedConfiguration, "0.1.0", "99.0.0") + "priority: 99\n"
	request = configurationRequest(string(lifecycle.ConfigYAML) + updated)
	if _, err := p.Handle(protocol.MethodPluginReconfigure, request); err != nil {
		t.Fatalf("host metadata update changed runtime identity: %v", err)
	}
	if _, err := p.Handle(protocol.MethodUsageHandle, []byte(validUsage)); err != nil {
		t.Fatal(err)
	}
	waitCommitted(t, p, "2")
	p.Handle(protocol.MethodPluginQuiesce, nil)
	if _, err := p.Handle(protocol.MethodPluginReconfigure, request); err != nil {
		t.Fatal(err)
	}
	if r := manage(t, p, "/v0/management/plugins/token-usage/summary", query()); r.StatusCode != 200 || !strings.Contains(string(r.Body), `"observed_events":"2"`) {
		t.Fatalf("metadata-only reopen lost committed history: %d %s", r.StatusCode, r.Body)
	}
	if data, err := os.ReadFile(trap); err != nil || string(data) != "default-path-canary" {
		t.Fatalf("explicit configuration touched default storage: %q %v", data, err)
	}
}

func TestMissingWorkingDirectoryDoesNotPreventExplicitConfiguration(t *testing.T) {
	gone, valid := t.TempDir(), t.TempDir()
	t.Chdir(gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Getwd(); err == nil {
		t.Fatal("test requires an unavailable working directory")
	}
	p := New()
	t.Cleanup(p.Shutdown)
	t.Chdir(valid)
	if _, err := p.Handle(protocol.MethodPluginRegister, configurationRequest(installedConfiguration)); err == nil {
		t.Fatal("default path was resolved from later mutable cwd")
	}
	if _, err := p.Handle(protocol.MethodPluginReconfigure, registrationRequest(filepath.Join(valid, "private", "usage.sqlite"))); err != nil {
		t.Fatalf("missing captured cwd blocked explicit path: %v", err)
	}
	if r := manage(t, p, "/v0/management/plugins/token-usage/status", nil); r.StatusCode != 200 {
		t.Fatalf("explicit path was not usable: %d %s", r.StatusCode, r.Body)
	}
	if _, err := os.Stat(filepath.Join(valid, "plugins")); !os.IsNotExist(err) {
		t.Fatalf("later cwd became a replacement default: %v", err)
	}
}

func TestInvalidConfigAndProtocolDoNotAcquireBootstrapCapabilities(t *testing.T) {
	t.Chdir(t.TempDir())
	p := New()
	t.Cleanup(p.Shutdown)
	oldSchema, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 5, ConfigYAML: []byte(installedConfiguration)})
	for _, raw := range [][]byte{[]byte(`{`), []byte(`null`), oldSchema, configurationRequest("null"), configurationRequest(installedConfiguration + "unknown-secret-canary: true\n"), configurationRequest("database-path: relative.sqlite\n"), configurationRequest("queue-capacity: 0\n")} {
		value, err := p.Handle(protocol.MethodPluginRegister, raw)
		if err == nil || strings.Contains(err.Error(), "secret-canary") {
			t.Fatalf("invalid configuration did not fail safely: %v", err)
		}
		if registration, ok := value.(protocol.Registration); ok && (registration.Capabilities.UsagePlugin || registration.Capabilities.ManagementAPI) {
			t.Fatalf("invalid configuration invented capability success: %+v", registration)
		}
	}
	if _, err := p.Handle(protocol.MethodUsageHandle, []byte(validUsage)); err == nil {
		t.Fatal("invalid configuration enabled admission")
	}
	if r := manage(t, p, "/v0/management/plugins/token-usage/status", nil); r.StatusCode != 503 || string(r.Body) != `{"error":"collection_unavailable"}` {
		t.Fatalf("invalid configuration masqueraded as a storage bootstrap: %d %s", r.StatusCode, r.Body)
	}
	if _, err := os.Stat("plugins"); !os.IsNotExist(err) {
		t.Fatalf("invalid configuration opened storage: %v", err)
	}
}

func TestInitialStorageFailureCannotReopenAfterTerminalShutdown(t *testing.T) {
	p := New()
	t.Cleanup(p.Shutdown)
	bad := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(bad, []byte("canary"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Handle(protocol.MethodPluginRegister, registrationRequest(filepath.Join(bad, "usage.sqlite"))); err != nil {
		t.Fatal(err)
	}
	p.Shutdown()
	for _, method := range []string{protocol.MethodPluginRegister, protocol.MethodPluginReconfigure} {
		if _, err := p.Handle(method, registrationRequest(filepath.Join(t.TempDir(), "private", "usage.sqlite"))); err == nil {
			t.Fatal("initial failure forgot terminal shutdown")
		}
	}
	if _, err := p.Handle(protocol.MethodUsageHandle, []byte(validUsage)); err == nil {
		t.Fatal("terminal failed collector admitted an event")
	}
}

func TestStoreInstallWithoutDatabasePathCapturesWorkingDirectoryOnce(t *testing.T) {
	original, later := t.TempDir(), t.TempDir()
	shared := filepath.Join(original, "plugins")
	if err := os.Mkdir(shared, 0755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(original)
	p := New()
	t.Cleanup(p.Shutdown)
	// CPA can change directory after native construction and before registration.
	// The history location must not follow that change, including on reopen.
	t.Chdir(later)
	request := configurationRequest(installedConfiguration)
	if _, err := p.Handle(protocol.MethodPluginRegister, request); err != nil {
		t.Fatalf("store installation did not bootstrap: %v", err)
	}
	if r := manage(t, p, "/v0/management/plugins/token-usage/status", nil); r.StatusCode != 200 {
		t.Fatalf("default storage unavailable: %d %s", r.StatusCode, r.Body)
	}
	path := filepath.Join(original, "plugins", "data", "token-usage", "usage.sqlite")
	for name, mode := range map[string]os.FileMode{shared: 0755, filepath.Dir(path): 0700, path: 0600, path + ".lock": 0600, path + "-wal": 0600, path + "-shm": 0600} {
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("private leaf/shared volume permissions for %s: %v %v", name, info, err)
		}
	}
	if _, err := os.Stat(filepath.Join(later, "plugins")); !os.IsNotExist(err) {
		t.Fatalf("registration probed or created storage under the later cwd: %v", err)
	}
	p.Handle(protocol.MethodPluginQuiesce, nil)
	if _, err := p.Handle(protocol.MethodPluginReconfigure, request); err != nil {
		t.Fatalf("same default did not reopen: %v", err)
	}
	if r := manage(t, p, "/v0/management/plugins/token-usage/status", nil); r.StatusCode != 200 {
		t.Fatalf("reopened default storage unavailable: %d %s", r.StatusCode, r.Body)
	}
}
