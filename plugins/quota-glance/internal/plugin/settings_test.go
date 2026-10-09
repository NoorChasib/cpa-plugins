package plugin

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/overrides"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
)

// alphaAccount is the fixture's first API credit account.
var alphaAccount = qc.APICreditOrgAccount("00000000-0000-4000-8000-000000000001")

// newMeteredPlugin is newFixturePlugin with quota-cache's API meter beside
// the snapshot, configured with extra appended.
func newMeteredPlugin(t *testing.T, extra string) (*Plugin, *fakeHost, string, string) {
	t.Helper()
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "snapshot.json")
	if err := os.WriteFile(cachePath, fixtureSnapshot(t), 0o600); err != nil {
		t.Fatal(err)
	}
	meter, err := os.ReadFile(filepath.Join("..", "..", "testdata", "snapshots", "seven-credentials.meter.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(qc.MeterPath(cachePath), meter, 0o600); err != nil {
		t.Fatal(err)
	}
	host := &fakeHost{files: []protocol.HostAuthFileEntry{
		{AuthIndex: "claude-siphorchannel@example.com.json", Provider: "claude"},
		{AuthIndex: "5f2b8c41d09e7a36", Provider: "claude", Email: "noorchasib@example.com"},
		{AuthIndex: "codex-noor@example.com.json", Provider: "codex"},
	}}
	p := New(host)
	t.Cleanup(p.Shutdown)
	dataDir := filepath.Join(dir, "data")
	if _, err := configure(t, p, meteredConfig(cachePath, dataDir)+extra); err != nil {
		t.Fatal(err)
	}
	return p, host, cachePath, dataDir
}

func meteredConfig(cachePath, dataDir string) string {
	return "cache-path: " + cachePath + "\ndata-dir: " + dataDir + "\nweb-token: test-token\nstale-after: 45m\n"
}

func handle(t *testing.T, p *Plugin, req protocol.ManagementRequest) protocol.ManagementResponse {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Handle(protocol.MethodManagementHandle, raw)
	if err != nil {
		t.Fatal(err)
	}
	return result.(protocol.ManagementResponse)
}

func saveMonthly(t *testing.T, p *Plugin, base, monthly string) protocol.ManagementResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"kind": "apiCredits", "items": []any{map[string]any{
		"id": alphaAccount, "baseRevision": base, "monthlyUsd": monthly, "renews": nil, "reading": nil,
	}}})
	return handle(t, p, protocol.ManagementRequest{Method: "POST", Path: "/v0/management/plugins/quota-glance/settings",
		Headers: http.Header{"Content-Type": {"application/json"}}, Body: body})
}

func meteredDocument(doc map[string]any) bool {
	credits, ok := doc["apiCredits"].(map[string]any)
	return ok && credits["meter"] != nil
}

func alphaOf(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	for _, account := range doc["apiCredits"].(map[string]any)["accounts"].([]any) {
		if a := account.(map[string]any); a["id"] == alphaAccount {
			return a
		}
	}
	t.Fatalf("no account %s", alphaAccount)
	return nil
}

// A save is committed, logged by name only, rebuilt into the served document,
// and answered with the rebuilt row.
func TestASaveIsCommittedRebuiltAndLogged(t *testing.T) {
	p, host, _, dataDir := newMeteredPlugin(t, "")
	waitForDocument(t, p, meteredDocument)

	res := saveMonthly(t, p, "", "312.40")
	if res.StatusCode != http.StatusOK || !strings.Contains(string(res.Body), `"monthlyUsd":"312.40"`) {
		t.Fatalf("save: %d %s", res.StatusCode, res.Body)
	}
	alpha := alphaOf(t, decode(t, summary(t, p)))
	if alpha["monthlyCreditText"] != "$312.40" || alpha["monthlyCreditSource"] != "dashboard" {
		t.Fatalf("served: %v", alpha)
	}
	info, err := os.Stat(filepath.Join(dataDir, overrides.FileName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("settings.json: %v %v", info, err)
	}
	var changed []logLine
	for _, line := range host.logged() {
		if line.message == "quota-glance settings changed" {
			changed = append(changed, line)
		}
		if strings.Contains(fmt.Sprint(line.fields), "312.40") || strings.Contains(fmt.Sprint(line.fields), "test-token") {
			t.Fatalf("a log line carries a value: %+v", line)
		}
	}
	if len(changed) != 1 || changed[0].level != "info" || changed[0].fields["kind"] != "apiCredits" || changed[0].fields["door"] != "console" ||
		fmt.Sprint(changed[0].fields["ids"]) != "["+alphaAccount+"]" || fmt.Sprint(changed[0].fields["fields"]) != "[monthlyUsd]" {
		t.Fatalf("log = %+v", changed)
	}
	// An unchanged resend writes nothing and logs nothing.
	if res := saveMonthly(t, p, "", "312.40"); res.StatusCode != http.StatusOK || !strings.Contains(string(res.Body), `"unchanged":true`) {
		t.Fatalf("resend: %d %s", res.StatusCode, res.Body)
	}
	count := 0
	for _, line := range host.logged() {
		if line.message == "quota-glance settings changed" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%d change lines after an unchanged resend", count)
	}
	if len(host.providerRequests()) != 0 {
		t.Fatal("saving settings reached a provider")
	}
}

func decode(t *testing.T, res protocol.ManagementResponse) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(res.Body, &doc); err != nil {
		t.Fatalf("not JSON: %s", res.Body)
	}
	return doc
}

// The password door saves too, behind the web token.
func TestThePasswordDoorSaves(t *testing.T) {
	p, _, _, _ := newMeteredPlugin(t, "")
	waitForDocument(t, p, meteredDocument)
	batch, _ := json.Marshal(map[string]any{"kind": "renewals", "items": []any{map[string]any{"id": "5f2b8c41d09e7a36", "baseRevision": "", "date": "2026-10-29"}}})
	header := base64.RawURLEncoding.EncodeToString(batch)
	if res := resource(t, p, "/save-settings", http.Header{"X-Quota-Glance-Settings": {header}}); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without the token: %d", res.StatusCode)
	}
	res := resource(t, p, "/save-settings", http.Header{"Authorization": {"Bearer test-token"}, "X-Quota-Glance-Settings": {header}})
	if res.StatusCode != http.StatusOK || !strings.Contains(string(res.Body), `"date":"2026-10-29"`) {
		t.Fatalf("save: %d %s", res.StatusCode, res.Body)
	}
}

// allow-edit off closes both doors and leaves what is stored in force.
func TestAllowEditOffClosesTheDoorsAndKeepsTheValues(t *testing.T) {
	p, _, cachePath, dataDir := newMeteredPlugin(t, "")
	waitForDocument(t, p, meteredDocument)
	if res := saveMonthly(t, p, "", "312.40"); res.StatusCode != http.StatusOK {
		t.Fatalf("save: %d %s", res.StatusCode, res.Body)
	}
	if _, err := configure(t, p, meteredConfig(cachePath, dataDir)+"allow-edit: false\n"); err != nil {
		t.Fatal(err)
	}
	if res := saveMonthly(t, p, "1", "1"); res.StatusCode != http.StatusNotFound {
		t.Fatalf("with editing off: %d %s", res.StatusCode, res.Body)
	}
	if res := resource(t, p, "/save-settings", http.Header{"Authorization": {"Bearer test-token"}, "X-Quota-Glance-Settings": {"e30"}}); res.StatusCode != http.StatusNotFound {
		t.Fatalf("password door with editing off: %d", res.StatusCode)
	}
	doc := waitForDocument(t, p, func(doc map[string]any) bool {
		credits, ok := doc["apiCredits"].(map[string]any)
		return ok && credits["editing"].(map[string]any)["reason"] == "disabled"
	})
	if alpha := alphaOf(t, doc); alpha["monthlyCreditText"] != "$312.40" || alpha["settings"].(map[string]any)["editable"] != false {
		t.Fatalf("stored values stopped applying: %v", alpha)
	}
}

func TestAllowEditDecodes(t *testing.T) {
	p, _, cachePath, dataDir := newMeteredPlugin(t, "")
	for _, value := range []string{"true", "false", ""} {
		if _, err := configure(t, p, meteredConfig(cachePath, dataDir)+"allow-edit: "+value+"\n"); err != nil {
			t.Fatalf("allow-edit: %q: %v", value, err)
		}
	}
	if _, err := configure(t, p, meteredConfig(cachePath, dataDir)+"allow-edit: sometimes\n"); err == nil {
		t.Fatal("allow-edit: sometimes was accepted")
	}
	registration := registration()
	found := false
	for _, field := range registration.Metadata.ConfigFields {
		if field.Name == "allow-edit" {
			found = field.Type == "boolean" && strings.Contains(field.Description, "default true")
		}
	}
	if !found {
		t.Fatalf("allow-edit is not a boolean config field: %+v", registration.Metadata.ConfigFields)
	}
}

// The store is kept across a reconfigure, so its values and its write limit
// outlive a saved panel; a new data-dir opens the store there; and one that
// could not be read is opened again, so moving the file aside and saving the
// configuration brings editing back.
func TestTheSettingsStoreLifetime(t *testing.T) {
	p, _, cachePath, dataDir := newMeteredPlugin(t, "")
	first := p.overrides
	if _, err := configure(t, p, meteredConfig(cachePath, dataDir)+"openrouter-warn-below: 3\n"); err != nil {
		t.Fatal(err)
	}
	if p.overrides != first {
		t.Fatal("a reconfigure replaced the settings store")
	}
	moved := filepath.Join(filepath.Dir(dataDir), "elsewhere")
	if _, err := configure(t, p, meteredConfig(cachePath, moved)); err != nil {
		t.Fatal(err)
	}
	if p.overrides == first || p.overrides.Dir() != moved {
		t.Fatalf("data-dir moved and the store did not: %s", p.overrides.Dir())
	}
	other := filepath.Join(filepath.Dir(dataDir), "third")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, overrides.FileName), []byte(`{"schema":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := configure(t, p, meteredConfig(cachePath, other)); err != nil {
		t.Fatal(err)
	}
	if !p.overrides.Current().Unreadable {
		t.Fatal("a corrupt settings.json loaded")
	}
	if res := saveMonthly(t, p, "", "1"); res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("saving over an unreadable file: %d %s", res.StatusCode, res.Body)
	}
	if err := os.Rename(filepath.Join(other, overrides.FileName), filepath.Join(other, "settings.json.bad")); err != nil {
		t.Fatal(err)
	}
	if _, err := configure(t, p, meteredConfig(cachePath, other)); err != nil {
		t.Fatal(err)
	}
	if p.overrides.Current().Unreadable {
		t.Fatal("moving the file aside and reconfiguring did not bring editing back")
	}
}

// Health reports the settings file and the meter, each with why not.
func TestHealthReportsSettingsAndTheMeter(t *testing.T) {
	p, _, cachePath, dataDir := newMeteredPlugin(t, "")
	waitForDocument(t, p, meteredDocument)
	if res := saveMonthly(t, p, "", "312.40"); res.StatusCode != http.StatusOK {
		t.Fatal(string(res.Body))
	}
	var health struct {
		Settings struct {
			Path             string `json:"path"`
			Revision         uint64 `json:"revision"`
			APICreditEntries int    `json:"api_credit_entries"`
			RenewalEntries   int    `json:"renewal_entries"`
			LastError        string `json:"last_error"`
		} `json:"settings"`
		Meter struct {
			Path      string    `json:"path"`
			FlushedAt time.Time `json:"flushed_at"`
			LastError string    `json:"last_error"`
		} `json:"meter"`
	}
	read := func() {
		t.Helper()
		if err := json.Unmarshal(managementGet(t, p, "/v0/management/plugins/quota-glance/health").Body, &health); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if health.Settings.Path != filepath.Join(dataDir, overrides.FileName) || health.Settings.Revision != 1 ||
		health.Settings.APICreditEntries != 1 || health.Settings.RenewalEntries != 0 || health.Settings.LastError != "" ||
		health.Meter.Path != qc.MeterPath(cachePath) || !health.Meter.FlushedAt.Equal(time.Date(2026, 9, 10, 3, 59, 0, 0, time.UTC)) ||
		health.Meter.LastError != "" {
		t.Fatalf("health = %+v", health)
	}
	if err := os.Remove(qc.MeterPath(cachePath)); err != nil {
		t.Fatal(err)
	}
	p.Rebuild()
	read()
	if health.Meter.LastError != "missing" {
		t.Fatalf("meter = %+v", health.Meter)
	}
	if err := os.WriteFile(qc.MeterPath(cachePath), []byte(`{"schema":1,`), 0o600); err != nil {
		t.Fatal(err)
	}
	p.Rebuild()
	read()
	if health.Meter.LastError != "unreadable" {
		t.Fatalf("meter = %+v", health.Meter)
	}
	// Neither is a source failure: the rest of the dashboard is unaffected.
	if doc := decode(t, summary(t, p)); doc["staleReason"] != nil && doc["staleReason"] != "cacheStale" {
		t.Fatalf("a meter failure became the document's: %v", doc["staleReason"])
	}
}

// A save rebuilds on its own goroutine while the watcher may be rebuilding on
// another. The store's lock is released before either rebuild starts, so the
// two can never wait on each other.
func TestASaveAndAConcurrentWatcherRebuildComplete(t *testing.T) {
	p, _, _, _ := newMeteredPlugin(t, "")
	waitForDocument(t, p, meteredDocument)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 20 {
			p.Rebuild()
		}
	}()
	go func() {
		defer wg.Done()
		base := ""
		for i := range 20 {
			res := saveMonthly(t, p, base, fmt.Sprintf("%d", 100+i))
			if res.StatusCode != http.StatusOK {
				t.Errorf("save %d: %d %s", i, res.StatusCode, res.Body)
				return
			}
			var body struct {
				Revision string `json:"revision"`
			}
			_ = json.Unmarshal(res.Body, &body)
			base = body.Revision
		}
	}()
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("a save and a rebuild deadlocked")
	}
	if alpha := alphaOf(t, decode(t, summary(t, p))); alpha["monthlyCreditText"] != "$119.00" {
		t.Fatalf("served after the saves: %v", alpha["monthlyCreditText"])
	}
}

// quota-cache writes its meter on its own schedule. A meter write alone
// rebuilds the document, and adds no history: history samples follow the
// snapshot's writes.
func TestAMeterWriteRebuildsWithoutAddingHistory(t *testing.T) {
	p, _, cachePath, dataDir := newMeteredPlugin(t, "")
	waitForDocument(t, p, meteredDocument)
	before := samplesOn(t, dataDir)

	raw, err := os.ReadFile(qc.MeterPath(cachePath))
	if err != nil {
		t.Fatal(err)
	}
	moved := strings.Replace(string(raw), `"flushed_at": "2026-09-10T03:59:00Z"`, `"flushed_at": "2026-09-10T04:09:00Z"`, 1)
	if moved == string(raw) {
		t.Fatal("the meter fixture's flushed_at did not change; the test asserts nothing")
	}
	temp := qc.MeterPath(cachePath) + ".tmp"
	if err := os.WriteFile(temp, []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temp, qc.MeterPath(cachePath)); err != nil {
		t.Fatal(err)
	}
	want := float64(time.Date(2026, 9, 10, 4, 9, 0, 0, time.UTC).Unix())
	waitForDocument(t, p, func(doc map[string]any) bool {
		credits, ok := doc["apiCredits"].(map[string]any)
		return ok && credits["meter"] != nil && credits["meter"].(map[string]any)["updatedAtEpoch"] == want
	})
	if got := samplesOn(t, dataDir); got != before {
		t.Fatalf("a meter write changed history from %d to %d samples", before, got)
	}
}
