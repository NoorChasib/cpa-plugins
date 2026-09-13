package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/token-usage/internal/protocol"
)

const validUsage = `{"Provider":"test","Model":"model","ExecutorType":"CustomExecutor","RequestedAt":"2026-09-09T00:00:00Z","Generate":true,"Detail":{"InputTokens":9007199254740993,"OutputTokens":20,"TotalTokens":9007199254741013},"AuthIndex":"","APIKey":"secret-canary","Source":"secret-canary","Failure":{"Body":"secret-canary"},"ResponseHeaders":{"X-Test":["secret-canary"]},"Unknown":{"ignored":true}}`

var testNow = time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

func registrationRequest(path string) []byte {
	raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte(fmt.Sprintf("database-path: %q\nqueue-capacity: 1024\nbatch-size: 32\nflush-interval: 10ms\n", path))})
	return raw
}
func newRegistered(t *testing.T) *Plugin {
	t.Helper()
	p := New()
	p.now = func() time.Time { return testNow }
	if _, err := p.Handle(protocol.MethodPluginRegister, registrationRequest(filepath.Join(t.TempDir(), "private", "usage.sqlite"))); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Shutdown)
	return p
}
func manage(t *testing.T, p *Plugin, path string, q url.Values) protocol.ManagementResponse {
	t.Helper()
	raw, _ := json.Marshal(protocol.ManagementRequest{Method: http.MethodGet, Path: path, Query: q, Headers: http.Header{"Authorization": {"secret-canary"}}})
	value, err := p.Handle(protocol.MethodManagementHandle, raw)
	if err != nil {
		t.Fatal(err)
	}
	return value.(protocol.ManagementResponse)
}
func waitCommitted(t *testing.T, p *Plugin, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s := p.runtime().Snapshot()
		if s["diagnostics"].(map[string]string)["committed_events"] == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("commit timeout: %#v", p.runtime().Snapshot())
}
func query() url.Values {
	return url.Values{"from": {"2026-09-09T00:00:00Z"}, "to": {"2026-09-09T12:00:00Z"}}
}

func TestPersistentExactTotalsPrivacyFailureAndRestart(t *testing.T) {
	p := newRegistered(t)
	for _, raw := range []string{validUsage, validUsage, strings.Replace(validUsage, `"Generate":true`, `"Generate":true,"Failed":true`, 1)} {
		if _, err := p.Handle(protocol.MethodUsageHandle, []byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	waitCommitted(t, p, "3")
	response := manage(t, p, "/v0/management/plugins/token-usage/summary", query())
	if response.StatusCode != 200 {
		t.Fatalf("summary %d %s", response.StatusCode, response.Body)
	}
	var result struct {
		Totals struct {
			Observed string            `json:"observed_events"`
			Failed   string            `json:"failed_events"`
			Tokens   map[string]string `json:"reported_tokens"`
		}
	}
	if err := json.Unmarshal(response.Body, &result); err != nil {
		t.Fatal(err)
	}
	if result.Totals.Observed != "3" || result.Totals.Failed != "1" || result.Totals.Tokens["input_tokens"] != "27021597764222979" {
		t.Fatalf("totals %s", response.Body)
	}
	for _, path := range []string{"status", "summary", "models"} {
		q := query()
		if path == "status" {
			q = nil
		}
		r := manage(t, p, "/v0/management/plugins/token-usage/"+path, q)
		if strings.Contains(string(r.Body), "secret-canary") || strings.Contains(string(r.Body), p.cfg.DatabasePath) {
			t.Fatal("sensitive output")
		}
	}
	cfg := p.cfg
	p.Shutdown()
	reopened := New()
	reopened.now = p.now
	t.Cleanup(reopened.Shutdown)
	if _, err := reopened.Handle(protocol.MethodPluginRegister, registrationRequest(cfg.DatabasePath)); err != nil {
		t.Fatal(err)
	}
	r := manage(t, reopened, "/v0/management/plugins/token-usage/summary", query())
	if r.StatusCode != 200 || !strings.Contains(string(r.Body), `"observed_events":"3"`) {
		t.Fatalf("restart %s", r.Body)
	}
	if _, err := reopened.Handle(protocol.MethodUsageHandle, []byte(validUsage)); err != nil {
		t.Fatal(err)
	}
	waitCommitted(t, reopened, "4")
}
func TestValidationAndTimestampFallback(t *testing.T) {
	p := newRegistered(t)
	invalid := []string{`{`, `null`, `{}`, strings.Replace(validUsage, `"Model":"model"`, `"Model":""`, 1)}
	for _, counter := range []string{"-1", "1.5", "9223372036854775808"} {
		invalid = append(invalid, strings.Replace(validUsage, "9007199254740993", counter, 1))
	}
	for _, raw := range invalid {
		if _, err := p.Handle(protocol.MethodUsageHandle, []byte(raw)); err == nil {
			t.Fatal("accepted invalid record")
		}
	}
	if _, err := p.Handle(protocol.MethodUsageHandle, []byte(strings.Replace(validUsage, "2026-09-09T00:00:00Z", "invalid-time", 1))); err != nil {
		t.Fatal("timestamp fallback rejected")
	}
	waitCommitted(t, p, "1")
	diag := p.runtime().Snapshot()["diagnostics"].(map[string]string)
	if diag["rejected_events"] != fmt.Sprint(len(invalid)) {
		t.Fatalf("diagnostics %v", diag)
	}
}
func TestPrivateRoutesQueryValidationAndExactFilters(t *testing.T) {
	p := newRegistered(t)
	v, _ := p.Handle(protocol.MethodManagementRegister, nil)
	if len(v.(protocol.ManagementRegistration).Routes) != 3 {
		t.Fatal("route count")
	}
	for _, r := range v.(protocol.ManagementRegistration).Routes {
		if r.Menu != "" {
			t.Fatal("public Menu")
		}
	}
	for _, path := range []string{"summary", "models"} {
		if manage(t, p, "/v0/resource/plugins/token-usage/"+path, nil).StatusCode != 404 {
			t.Fatal("private statistics acquired a public route")
		}
	}
	for _, q := range []url.Values{nil, {"from": {"x"}, "to": {"y"}}, {"from": {"2026-09-09T00:00:00Z", "2026-09-09T01:00:00Z"}, "to": {"2026-09-09T12:00:00Z"}}, {"from": {"2026-09-09T00:00:00Z"}, "to": {"2026-09-10T12:00:00Z"}}, {"from": {"2026-09-09T00:00:00Z"}, "to": {"2026-09-09T12:00:00Z"}, "account": {"x"}}} {
		if r := manage(t, p, "/v0/management/plugins/token-usage/summary", q); r.StatusCode != 400 {
			t.Fatalf("query accepted %v: %d", q, r.StatusCode)
		}
	}
	if r := manage(t, p, "/v0/management/plugins/token-usage/summary", query()); r.StatusCode != 416 {
		t.Fatalf("uncovered period became zero: %d", r.StatusCode)
	}
	p.Handle(protocol.MethodUsageHandle, []byte(validUsage))
	waitCommitted(t, p, "1")
	q := query()
	q.Set("provider", "x' OR 1=1 --")
	r := manage(t, p, "/v0/management/plugins/token-usage/summary", q)
	if r.StatusCode != 200 || !strings.Contains(string(r.Body), `"input_tokens":"0"`) {
		t.Fatalf("filter %s", r.Body)
	}
}
func TestConfigurationHistoryTracksOnlySuccessfulInitialization(t *testing.T) {
	p := New()
	p.now = func() time.Time { return testNow }
	t.Cleanup(p.Shutdown)
	badPath := filepath.Join(t.TempDir(), "usage.sqlite")
	if err := os.WriteFile(badPath, []byte("not a SQLite database"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Handle(protocol.MethodPluginRegister, registrationRequest(badPath)); err != nil {
		t.Fatalf("initial storage failure hid configuration metadata: %v", err)
	}
	if r := manage(t, p, "/v0/management/plugins/token-usage/status", nil); r.StatusCode != 503 {
		t.Fatalf("expected visible storage initialization failure: %d %s", r.StatusCode, r.Body)
	}
	good := registrationRequest(filepath.Join(t.TempDir(), "private", "usage.sqlite"))
	if _, err := p.Handle(protocol.MethodPluginRegister, good); err != nil {
		t.Fatalf("failed initialization fixed the configuration: %v", err)
	}
	if _, err := p.Handle(protocol.MethodPluginQuiesce, nil); err != nil {
		t.Fatal(err)
	}
	other := registrationRequest(filepath.Join(t.TempDir(), "private", "other.sqlite"))
	if _, err := p.Handle(protocol.MethodPluginReconfigure, other); err == nil {
		t.Fatal("quiesce forgot the successful configuration")
	}
	if _, err := p.Handle(protocol.MethodPluginRegister, good); err != nil {
		t.Fatalf("same configuration did not reopen: %v", err)
	}
}

func TestConcurrentQueriesDoNotSerializeAdmissionAndLifecycle(t *testing.T) {
	p := newRegistered(t)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if _, err := p.Handle(protocol.MethodUsageHandle, []byte(validUsage)); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				raw, _ := json.Marshal(protocol.ManagementRequest{Method: "GET", Path: "/v0/management/plugins/token-usage/summary", Query: query()})
				p.Handle(protocol.MethodManagementHandle, raw)
			}
		}()
	}
	wg.Wait()
	waitCommitted(t, p, "400")
	same := registrationRequest(p.cfg.DatabasePath)
	if _, err := p.Handle(protocol.MethodPluginReconfigure, same); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Handle(protocol.MethodPluginReconfigure, registrationRequest(filepath.Join(t.TempDir(), "private", "other.sqlite"))); err == nil {
		t.Fatal("path change accepted")
	}
	p.Handle(protocol.MethodPluginQuiesce, nil)
	if _, err := p.Handle(protocol.MethodUsageHandle, []byte(validUsage)); err == nil {
		t.Fatal("admitted after quiesce")
	}
	if _, err := p.Handle(protocol.MethodPluginRegister, same); err != nil {
		t.Fatal(err)
	}
	p.Shutdown()
	if _, err := p.Handle(protocol.MethodPluginRegister, same); err == nil {
		t.Fatal("registered after final shutdown")
	}
}
