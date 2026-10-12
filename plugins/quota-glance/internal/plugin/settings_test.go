package plugin

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/source"
)

// API credit amounts, refill dates, Console readings and Claude renewal dates
// are Quota Cache's to keep and edit since quota-glance 0.8.0. These test
// that this plugin shows what Quota Cache publishes, read-only, reads its own
// old settings.json without ever writing it, and still accepts the
// configuration 0.7 accepted.

// alphaAccount is the fixture's first API credit account.
var alphaAccount = qc.APICreditOrgAccount("00000000-0000-4000-8000-000000000001")

// publishedState is Quota Cache's committed credit state for the
// seven-credentials fixture, as raw JSON.
func publishedState(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "quota-cache", "internal", "credits", "testdata", "golden", "seven-credentials.credits.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// newPublishedPlugin is newFixturePlugin with Quota Cache's credit state
// beside the snapshot, configured with extra appended. The data directory is
// quota-glance's default beside quota-cache's, plugins/data/quota-glance, so
// it is where Quota Cache would look for settings.json.
func newPublishedPlugin(t *testing.T, extra string) (p *Plugin, host *fakeHost, cachePath, dataDir string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "plugins", "data")
	cachePath, dataDir = filepath.Join(root, "quota-cache", "snapshot.json"), filepath.Join(root, "quota-glance")
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, fixtureSnapshot(t), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(qc.CreditsPath(cachePath), publishedState(t), 0o600); err != nil {
		t.Fatal(err)
	}
	host = &fakeHost{files: []protocol.HostAuthFileEntry{
		{AuthIndex: "claude-sierra@example.com.json", Provider: "claude"},
		{AuthIndex: "5f2b8c41d09e7a36", Provider: "claude", Email: "november@example.com"},
		{AuthIndex: "codex-noor@example.com.json", Provider: "codex"},
	}}
	p = New(host)
	t.Cleanup(p.Shutdown)
	if _, err := configure(t, p, publishedConfig(cachePath, dataDir)+extra); err != nil {
		t.Fatal(err)
	}
	return p, host, cachePath, dataDir
}

func publishedConfig(cachePath, dataDir string) string {
	return "cache-path: " + cachePath + "\ndata-dir: " + dataDir + "\nweb-token: test-token\nstale-after: 45m\n"
}

func decode(t *testing.T, res protocol.ManagementResponse) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(res.Body, &doc); err != nil {
		t.Fatalf("not JSON: %s", res.Body)
	}
	return doc
}

func creditsOf(doc map[string]any) (map[string]any, bool) {
	credits, ok := doc["apiCredits"].(map[string]any)
	return credits, ok
}

func publishedDocument(doc map[string]any) bool {
	credits, ok := creditsOf(doc)
	return ok && credits["meter"] != nil
}

func accountOf(t *testing.T, doc map[string]any, id string) map[string]any {
	t.Helper()
	credits, _ := creditsOf(doc)
	for _, account := range credits["accounts"].([]any) {
		if a := account.(map[string]any); a["id"] == id {
			return a
		}
	}
	t.Fatalf("no account %s", id)
	return nil
}

func credentialOf(t *testing.T, doc map[string]any, id string) map[string]any {
	t.Helper()
	for _, c := range doc["credentials"].([]any) {
		if credential := c.(map[string]any); credential["id"] == id {
			return credential
		}
	}
	t.Fatalf("no credential %s", id)
	return nil
}

// editState rewrites the credit state beside the snapshot through edit, as
// Quota Cache's publisher does: a new file renamed over the old.
func editState(t *testing.T, cachePath string, edit func(state map[string]any)) {
	t.Helper()
	raw, err := os.ReadFile(qc.CreditsPath(cachePath))
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	edit(state)
	if raw, err = json.Marshal(state); err != nil {
		t.Fatal(err)
	}
	temp := qc.CreditsPath(cachePath) + ".tmp"
	if err := os.WriteFile(temp, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temp, qc.CreditsPath(cachePath)); err != nil {
		t.Fatal(err)
	}
}

// The card is Quota Cache's, read-only: its figures, its renewal dates, and
// nothing editable. Showing it writes no settings file and reaches no
// provider.
func TestThePublishedStateIsShownReadOnly(t *testing.T) {
	p, host, _, dataDir := newPublishedPlugin(t, "")
	doc := waitForDocument(t, p, publishedDocument)
	credits, _ := creditsOf(doc)
	if editing := credits["editing"].(map[string]any); editing["available"] != false || editing["reason"] != "quotaCache" || editing["migration"] != "" {
		t.Fatalf("editing = %v", editing)
	}
	alpha := accountOf(t, doc, alphaAccount)
	settings := alpha["settings"].(map[string]any)
	if alpha["monthlyCreditText"] != "$200.00" || alpha["leftText"] != "$160.00" || settings["editable"] != false || settings["notEditableReason"] != "quotaCache" {
		t.Fatalf("alpha = %v", alpha)
	}
	renewal := credentialOf(t, doc, "5f2b8c41d09e7a36")
	if renewal["renewalSource"] != "dashboard" || renewal["renewalEditable"] != false || renewal["renewalSetting"].(map[string]any)["date"] != "2026-09-10" {
		t.Fatalf("renewal = %v", renewal)
	}
	if _, err := os.Stat(filepath.Join(dataDir, source.LegacySettingsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("settings.json appeared: %v", err)
	}
	if len(host.providerRequests()) != 0 {
		t.Fatal("showing the card reached a provider")
	}
}

// allow-edit is accepted with any boolean or empty value, as 0.7
// accepted it, and refused otherwise, as 0.7 refused it. Its value changes
// nothing, and a configure that sets it says so once.
func TestAllowEditIsAcceptedAndIgnored(t *testing.T) {
	p, host, cachePath, dataDir := newPublishedPlugin(t, "")
	ignored := func() int {
		count := 0
		for _, line := range host.logged() {
			if line.message == logAllowEditIgnored {
				if line.level != "warn" {
					t.Fatalf("logged at %s", line.level)
				}
				count++
			}
		}
		return count
	}
	if got := ignored(); got != 0 {
		t.Fatalf("warned %d times without allow-edit", got)
	}
	for i, tc := range []struct {
		value string
		warns int
	}{{"true", 1}, {"false", 2}, {"", 2}} {
		if _, err := configure(t, p, publishedConfig(cachePath, dataDir)+"allow-edit: "+tc.value+"\n"); err != nil {
			t.Fatalf("allow-edit: %q: %v", tc.value, err)
		}
		if got := ignored(); got != tc.warns {
			t.Fatalf("after configure %d (allow-edit: %q) warned %d times, want %d", i+1, tc.value, got, tc.warns)
		}
		doc := waitForDocument(t, p, publishedDocument)
		credits, _ := creditsOf(doc)
		if credits["editing"].(map[string]any)["reason"] != "quotaCache" {
			t.Fatalf("allow-edit: %q changed editing: %v", tc.value, credits["editing"])
		}
	}
	if _, err := configure(t, p, publishedConfig(cachePath, dataDir)+"allow-edit: sometimes\n"); err == nil {
		t.Fatal("allow-edit: sometimes was accepted")
	}
	// Disabled with it set still says so: the warning is about the key.
	if _, err := configure(t, p, publishedConfig(cachePath, dataDir)+"enabled: false\nallow-edit: false\n"); err != nil {
		t.Fatal(err)
	}
	if got := ignored(); got != 3 {
		t.Fatalf("a disabled configure warned %d times in all, want 3", got)
	}
}

// route-api-credits was never released by quota-glance: API credit routing is
// Reset Priority's. The strict decode refuses it, as 0.7.2 does.
func TestRouteAPICreditsIsRefused(t *testing.T) {
	p, _, cachePath, dataDir := newPublishedPlugin(t, "")
	for _, value := range []string{"true", "false"} {
		if _, err := configure(t, p, publishedConfig(cachePath, dataDir)+"route-api-credits: "+value+"\n"); err == nil {
			t.Fatalf("route-api-credits: %s was accepted", value)
		}
	}
}

// configFieldExclusions are the decoded keys deliberately left out of
// ConfigFields, and why. Every other key an operator sets is in CPA's Plugins
// form.
var configFieldExclusions = map[string]string{
	"enabled":    "CPA's own: the Plugins page switches a plugin on and off itself",
	"priority":   "CPA's own: the Plugins page orders plugins itself",
	"store":      "CPA's own: where the plugin was installed from",
	"allow-edit": "ignored since 0.8.0; accepted only so a 0.7 configuration still loads",
}

// Every key the decoder knows is in ConfigFields or named, with its reason,
// in configFieldExclusions; no field names a key the decoder does not know.
func TestConfigFieldsCoverEveryKey(t *testing.T) {
	decoded := map[string]bool{}
	fields := reflect.TypeFor[config]()
	for i := range fields.NumField() {
		key, _, _ := strings.Cut(fields.Field(i).Tag.Get("yaml"), ",")
		decoded[key] = true
	}
	offered := map[string]protocol.ConfigField{}
	for _, field := range registration().Metadata.ConfigFields {
		if !decoded[field.Name] {
			t.Errorf("ConfigFields offers %q, which the configuration does not decode", field.Name)
		}
		if _, excluded := configFieldExclusions[field.Name]; excluded {
			t.Errorf("%q is both offered and excluded", field.Name)
		}
		offered[field.Name] = field
	}
	for key := range decoded {
		if _, ok := offered[key]; !ok && configFieldExclusions[key] == "" {
			t.Errorf("%q is decoded but neither offered in ConfigFields nor excluded with a reason", key)
		}
	}
	for key := range configFieldExclusions {
		if !decoded[key] {
			t.Errorf("excluded %q is not decoded", key)
		}
	}
	for _, gone := range []string{"allow-edit", "route-api-credits"} {
		if _, ok := offered[gone]; ok {
			t.Errorf("ConfigFields offers %q", gone)
		}
	}
	// The form shows a boolean that is not written as off, and allow-redeem
	// defaults on.
	if redeem := offered["allow-redeem"]; redeem.Type != "boolean" ||
		!strings.Contains(redeem.Description, "Default true; this form shows it off until you save it once") {
		t.Errorf("allow-redeem = %+v", redeem)
	}
	if dataDir := offered["data-dir"]; dataDir.Description != "Directory where this plugin keeps its history and generated web token" {
		t.Errorf("data-dir = %+v", dataDir)
	}
}

// health is the parts of /health these tests read.
type health struct {
	Settings struct {
		Path         string `json:"path"`
		Present      bool   `json:"present"`
		Revision     uint64 `json:"revision"`
		SHA256Prefix string `json:"sha256_prefix"`
		LastError    string `json:"last_error"`
		Migration    string `json:"migration"`
	} `json:"settings"`
	Meter struct {
		Path      string    `json:"path"`
		FlushedAt time.Time `json:"flushed_at"`
		LastError string    `json:"last_error"`
	} `json:"meter"`
	CreditState struct {
		Path              string     `json:"path"`
		LastError         string     `json:"last_error"`
		ComputedAt        *time.Time `json:"computed_at"`
		ValidUntil        *time.Time `json:"valid_until"`
		SettingsState     string     `json:"settings_state"`
		ImportState       string     `json:"import_state"`
		QuotaCacheVersion string     `json:"quota_cache_version"`
	} `json:"credit_state"`
}

func healthOf(t *testing.T, p *Plugin) (health, map[string]any) {
	t.Helper()
	res := managementGet(t, p, "/v0/management/plugins/quota-glance/health")
	var h health
	if err := json.Unmarshal(res.Body, &h); err != nil {
		t.Fatal(err)
	}
	return h, decode(t, res)
}

// Health reports quota-glance's own settings.json, the meter as Quota
// Cache's state describes it, and the state itself, each with why not. There
// is no router: routing is Reset Priority's.
func TestHealthReportsLegacySettingsAndCreditState(t *testing.T) {
	p, _, cachePath, dataDir := newPublishedPlugin(t, "")
	waitForDocument(t, p, publishedDocument)
	h, raw := healthOf(t, p)
	if _, ok := raw["router"]; ok {
		t.Fatal("health still reports a router")
	}
	legacyPath := filepath.Join(dataDir, source.LegacySettingsFile)
	computed, until := time.Date(2026, 9, 10, 4, 0, 0, 0, time.UTC), time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	if h.Settings.Path != legacyPath || h.Settings.Present || h.Settings.LastError != "" || h.Settings.Migration != "" ||
		h.Meter.Path != qc.MeterPath(cachePath) || !h.Meter.FlushedAt.Equal(time.Date(2026, 9, 10, 3, 59, 0, 0, time.UTC)) || h.Meter.LastError != "" ||
		h.CreditState.Path != qc.CreditsPath(cachePath) || h.CreditState.LastError != "" || h.CreditState.ComputedAt == nil ||
		!h.CreditState.ComputedAt.Equal(computed) || h.CreditState.ValidUntil == nil || !h.CreditState.ValidUntil.Equal(until) ||
		h.CreditState.SettingsState != "ok" || h.CreditState.ImportState != "" || h.CreditState.QuotaCacheVersion != "0.2.0" {
		t.Fatalf("health = %+v", h)
	}

	// quota-glance 0.7's settings.json, which this state never imported.
	const legacy = `{"schema":1,"revision":12,"apiCredits":{},"renewals":{}}`
	if err := os.WriteFile(legacyPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	p.Rebuild()
	if h, _ = healthOf(t, p); !h.Settings.Present || h.Settings.Revision != 12 || len(h.Settings.SHA256Prefix) != 12 ||
		h.Settings.LastError != "" || h.Settings.Migration != "notImported" {
		t.Fatalf("settings = %+v", h.Settings)
	}
	if err := os.WriteFile(legacyPath, []byte(`{"revision":`), 0o600); err != nil {
		t.Fatal(err)
	}
	p.Rebuild()
	if h, _ = healthOf(t, p); h.Settings.LastError != "unreadable" {
		t.Fatalf("settings = %+v", h.Settings)
	}

	if err := os.Remove(qc.CreditsPath(cachePath)); err != nil {
		t.Fatal(err)
	}
	p.Rebuild()
	if h, _ = healthOf(t, p); h.CreditState.LastError != "missing" || h.CreditState.ComputedAt != nil || h.Meter.LastError != "missing" {
		t.Fatalf("no state: %+v %+v", h.CreditState, h.Meter)
	}
	if err := os.WriteFile(qc.CreditsPath(cachePath), []byte(`{"schema":1,`), 0o600); err != nil {
		t.Fatal(err)
	}
	p.Rebuild()
	if h, _ = healthOf(t, p); h.CreditState.LastError != "unreadable" || h.Meter.LastError != "unreadable" {
		t.Fatalf("unreadable state: %+v %+v", h.CreditState, h.Meter)
	}
	// Neither is a source failure: the rest of the dashboard is unaffected.
	if doc := decode(t, summary(t, p)); doc["staleReason"] != nil && doc["staleReason"] != "cacheStale" {
		t.Fatalf("a credit state failure became the document's: %v", doc["staleReason"])
	}
}

// A state left by a Quota Cache that no longer publishes, its snapshot
// written more than five minutes after the state, is not shown as current:
// the card lists the organizations without figures and says why, and health
// still shows how old the ignored state is.
func TestAnAbandonedStateFallsBack(t *testing.T) {
	p, _, cachePath, _ := newPublishedPlugin(t, "")
	waitForDocument(t, p, publishedDocument)
	editState(t, cachePath, func(state map[string]any) {
		state["computed_at"] = "2026-09-10T03:49:59Z" // the snapshot was written at 03:55:00
	})
	doc := waitForDocument(t, p, func(doc map[string]any) bool {
		credits, ok := creditsOf(doc)
		return ok && credits["meter"] == nil
	})
	alpha := accountOf(t, doc, alphaAccount)
	if alpha["state"] != "cacheTooOld" || alpha["leftText"] != "" ||
		!strings.Contains(alpha["issue"].(string), "no longer publishes API credit figures") {
		t.Fatalf("alpha = %v", alpha)
	}
	if renewal := credentialOf(t, doc, "5f2b8c41d09e7a36"); renewal["renewalSetting"] != nil || renewal["renewalSource"] == "dashboard" {
		t.Fatalf("an abandoned state's renewal date is shown: %v", renewal)
	}
	h, _ := healthOf(t, p)
	if h.CreditState.LastError != "abandoned" || h.CreditState.ComputedAt == nil ||
		!h.CreditState.ComputedAt.Equal(time.Date(2026, 9, 10, 3, 49, 59, 0, time.UTC)) {
		t.Fatalf("credit state = %+v", h.CreditState)
	}
}

// Before Quota Cache publishes, the card says what its first start will do
// with quota-glance's settings.json, here at Quota Cache's sibling path, the
// one it imports: nothing, once the file cannot be read. Reading the file
// never writes it.
func TestTheMigrationNoteFollowsGlancesOwnFile(t *testing.T) {
	p, _, cachePath, dataDir := newPublishedPlugin(t, "")
	waitForDocument(t, p, publishedDocument)
	legacyPath := filepath.Join(dataDir, source.LegacySettingsFile)
	const legacy = `{"schema":1,"revision":12,"apiCredits":{},"renewals":{}}`
	if err := os.WriteFile(legacyPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(legacyPath, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(qc.CreditsPath(cachePath)); err != nil {
		t.Fatal(err)
	}
	p.Rebuild()
	credits, _ := creditsOf(decode(t, summary(t, p)))
	if editing := credits["editing"].(map[string]any); editing["reason"] != "quotaCache" || editing["migration"] != "awaitingImport" {
		t.Fatalf("editing = %v", editing)
	}
	info, err := os.Stat(legacyPath)
	if err != nil || !info.ModTime().Equal(old) {
		t.Fatalf("settings.json was written: %v %v", info, err)
	}

	// Cut short, it is no longer awaited, on the card and in /health alike.
	if err := os.WriteFile(legacyPath, []byte(legacy[:20]), 0o600); err != nil {
		t.Fatal(err)
	}
	p.Rebuild()
	credits, _ = creditsOf(decode(t, summary(t, p)))
	if editing := credits["editing"].(map[string]any); editing["reason"] != "quotaCache" || editing["migration"] != "notImported" {
		t.Fatalf("unreadable: editing = %v", editing)
	}
	if h, _ := healthOf(t, p); h.Settings.LastError != "unreadable" || h.Settings.Migration != "notImported" {
		t.Fatalf("unreadable: settings = %+v", h.Settings)
	}
}

// quota-cache publishes its credit state on its own schedule. A write to it
// alone rebuilds the document, and adds no history: history samples follow
// the snapshot's writes.
func TestACreditStateWriteRebuildsWithoutAddingHistory(t *testing.T) {
	p, _, cachePath, dataDir := newPublishedPlugin(t, "")
	waitForDocument(t, p, publishedDocument)
	before := samplesOn(t, dataDir)
	editState(t, cachePath, func(state map[string]any) {
		state["api_credits"].(map[string]any)["meter"].(map[string]any)["updatedAtEpoch"] = 1789013340
	})
	waitForDocument(t, p, func(doc map[string]any) bool {
		credits, ok := creditsOf(doc)
		return ok && credits["meter"] != nil && credits["meter"].(map[string]any)["updatedAtEpoch"] == float64(1789013340)
	})
	if got := samplesOn(t, dataDir); got != before {
		t.Fatalf("a credit state write changed history from %d to %d samples", before, got)
	}
}

// While the source fails, the last good document is served, marked
// stale. What Quota Cache's state decides in it follows the state as it is
// now, so a value entered on Quota Cache's page meanwhile shows at once.
func TestWithCreditsReServes(t *testing.T) {
	p, host, cachePath, _ := newPublishedPlugin(t, "")
	waitForDocument(t, p, publishedDocument)
	host.mu.Lock()
	host.err = errors.New("host unavailable")
	host.mu.Unlock()
	p.Rebuild()
	if doc := decode(t, summary(t, p)); doc["staleReason"] != "rosterUnavailable" {
		t.Fatalf("staleReason = %v", doc["staleReason"])
	}

	editState(t, cachePath, func(state map[string]any) {
		for _, account := range state["api_credits"].(map[string]any)["accounts"].([]any) {
			if a := account.(map[string]any); a["id"] == alphaAccount {
				a["leftText"] = "$123.45"
			}
		}
		state["renewals"].(map[string]any)["5f2b8c41d09e7a36"] = map[string]any{"date": "2026-10-29", "revision": "4", "updatedAtEpoch": 1789012000}
	})
	p.Rebuild()
	doc := decode(t, summary(t, p))
	if doc["stale"] != true || doc["staleReason"] != "rosterUnavailable" {
		t.Fatalf("stale=%v reason=%v", doc["stale"], doc["staleReason"])
	}
	if alpha := accountOf(t, doc, alphaAccount); alpha["leftText"] != "$123.45" {
		t.Fatalf("served alpha: %v", alpha["leftText"])
	}
	renewal := credentialOf(t, doc, "5f2b8c41d09e7a36")
	if renewal["renewalSource"] != "dashboard" || renewal["renewalSetting"].(map[string]any)["date"] != "2026-10-29" {
		t.Fatalf("served credential: %v", renewal)
	}
}

// quota-glance 0.7's settings routes are still registered, and through the
// plugin they answer settings_moved.
func TestTheRetiredSettingsRoutesAnswerThroughThePlugin(t *testing.T) {
	p, _, _, _ := newPublishedPlugin(t, "")
	raw, err := json.Marshal(protocol.ManagementRequest{Method: "POST", Path: "/v0/management/plugins/quota-glance/settings",
		Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"kind":"apiCredits","items":[]}`)})
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Handle(protocol.MethodManagementHandle, raw)
	if err != nil {
		t.Fatal(err)
	}
	if res := result.(protocol.ManagementResponse); res.StatusCode != http.StatusGone || !strings.Contains(string(res.Body), `"settings_moved"`) {
		t.Fatalf("POST settings: %d %s", res.StatusCode, res.Body)
	}
	if res := resource(t, p, "/save-settings", http.Header{"Authorization": {"Bearer test-token"}}); res.StatusCode != http.StatusGone {
		t.Fatalf("GET save-settings: %d %s", res.StatusCode, res.Body)
	}
}
