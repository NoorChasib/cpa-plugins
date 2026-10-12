package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/settings"
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

// The status route and the save route are management routes, neither with a
// Menu, since CPA turns a GET route with a Menu into a public resource; the
// page is the one resource. The schema answered is the one offered, at most
// 6, at which CPA stops HTML-escaping management JSON; below 4 is refused.
func TestManagementRegistration(t *testing.T) {
	p := New(&testHost{})
	defer p.Shutdown()
	result, err := p.Handle(protocol.MethodManagementRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	registration := result.(protocol.ManagementRegistration)
	want := []protocol.ManagementRoute{{Method: "GET", Path: "/plugins/quota-cache/status"}, {Method: "POST", Path: "/plugins/quota-cache/settings"}}
	if len(registration.Routes) != len(want) {
		t.Fatalf("routes=%+v", registration.Routes)
	}
	for i, route := range registration.Routes {
		if route.Method != want[i].Method || route.Path != want[i].Path || route.Menu != "" || route.Description == "" {
			t.Fatalf("route %d=%+v", i, route)
		}
	}
	if len(registration.Resources) != 1 || registration.Resources[0].Path != "/status" || registration.Resources[0].Menu != "Quota Cache" {
		t.Fatalf("resources=%+v", registration.Resources)
	}
	for offered, answered := range map[uint32]uint32{4: 4, 5: 5, 6: 6, 9: 6} {
		raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: offered, ConfigYAML: []byte("enabled: false\n")})
		result, err := p.Handle(protocol.MethodPluginRegister, raw)
		if err != nil || result.(protocol.Registration).SchemaVersion != answered {
			t.Fatalf("offered %d: %+v %v; want %d", offered, result, err, answered)
		}
	}
	raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 3, ConfigYAML: []byte("enabled: false\n")})
	if _, err := p.Handle(protocol.MethodPluginRegister, raw); err == nil {
		t.Fatal("schema 3 accepted")
	}
}

// Every key the configuration decodes is offered on CPA's Plugins page or
// left out of it on purpose, so nothing needs a hand edit of config.yaml.
func TestEveryKeyIsOfferedOrLeftOutOnPurpose(t *testing.T) {
	leftOut := map[string]string{
		// Organizations are added, renamed and removed on Quota Cache's own
		// page; the list moves there from the configuration once. The form
		// cannot describe a list of objects either.
		"claude-api-credits": "edited on the page",
		// CPA's own keys, which its Plugins page sets with controls of its
		// own: the switch, the load order and the catalog's bookkeeping.
		"enabled":  "CPA's switch",
		"priority": "CPA's load order",
		"store":    "CPA's catalog",
	}
	decoded := map[string]bool{}
	kind := reflect.TypeOf(configuration{})
	for i := range kind.NumField() {
		name, _, _ := strings.Cut(kind.Field(i).Tag.Get("yaml"), ",")
		decoded[name] = true
	}
	offered := map[string]bool{}
	for _, field := range configFields {
		offered[field.Name] = true
		if !decoded[field.Name] || field.Type != "string" || field.Description == "" {
			t.Errorf("%s is offered but not decoded, or has no type or description", field.Name)
		}
		if _, both := leftOut[field.Name]; both {
			t.Errorf("%s is both offered and left out", field.Name)
		}
	}
	for name := range decoded {
		if _, out := leftOut[name]; !offered[name] && !out {
			t.Errorf("%s is decoded but neither offered nor left out on purpose", name)
		}
	}
	for name := range leftOut {
		if !decoded[name] {
			t.Errorf("%s is left out but not decoded", name)
		}
	}
}

// CPA's plugin list HTML-escapes every config field, and the console draws it
// as plain text, so no description may hold a character html.EscapeString
// changes: an apostrophe would show as &#39; in Edit config. That form is
// where cache-path is changed, so its description says what must move with it.
func TestConfigFieldsSurvivePluginListEscaping(t *testing.T) {
	for _, field := range configFields {
		if escaped := html.EscapeString(field.Description); escaped != field.Description {
			t.Errorf("%s reaches the console as %q", field.Name, escaped)
		}
		if field.Name == "cache-path" && !strings.Contains(field.Description, "values entered on the Quota Cache page are kept in files beside it, so move the whole directory") {
			t.Errorf("cache-path does not say what moves with it: %q", field.Description)
		}
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

// fiveOrgs is the five-organization settings.json Quota Glance 0.7 saved:
// revision 12; 2 monthly credits, 2 refill dates, 2 readings, 3 renewal
// dates, the newest changed 2026-10-10T18:20:00Z.
func fiveOrgs(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "settings", "testdata", "legacy", "five-orgs.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// placeLegacy puts raw where Quota Glance's default data-dir is beside the
// snapshot's directory.
func (p *meterPlugin) placeLegacy(raw []byte) string {
	p.t.Helper()
	path := filepath.Join(filepath.Dir(filepath.Dir(p.path)), "quota-glance", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		p.t.Fatal(err)
	}
	return path
}

func (p *meterPlugin) statusOf() map[string]any {
	p.t.Helper()
	code, raw := p.status()
	if code != 200 {
		p.t.Fatalf("status %d: %s", code, raw)
	}
	status := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &status); err != nil {
		p.t.Fatal(err)
	}
	return status
}

// The status route keeps every key it had, and adds the credit state as a
// reader sees it, the editor, and whether CPA escapes its strings. The editor
// names the roster's Claude credentials, disabled ones too, from memory only:
// no file holds a name.
func TestStatusCarriesCreditStateAndEditor(t *testing.T) {
	p := newRoutePlugin(t, func(p *meterPlugin) {
		if err := os.Remove(settings.FilePath(p.path)); err != nil {
			t.Fatal(err)
		}
		p.placeLegacy(fiveOrgs(t))
	})
	status := p.statusOf()
	for _, key := range []string{"schema", "written_at", "entries", "generated_at", "running", "cache_path", "poll_interval", "request_spacing", "activity", "status_reads", "previous_status_read", "credit_state", "editor", "strings_escaped"} {
		if _, ok := status[key]; !ok {
			t.Errorf("status has no %s", key)
		}
	}
	if status["strings_escaped"] != false {
		t.Fatalf("strings_escaped at schema 6: %v", status["strings_escaped"])
	}
	state, _ := status["credit_state"].(map[string]any)
	if state == nil || state["schema"] != 1.0 || state["api_credits"] == nil {
		t.Fatalf("credit_state %v", status["credit_state"])
	}
	editor := status["editor"].(map[string]any)
	legacy, _ := editor["legacy"].(map[string]any)
	counts, _ := legacy["counts"].(map[string]any)
	if editor["available"] != true || editor["reason"] != "" || editor["revision"] != "12" || editor["max_api_credit_items"] != 16.0 || editor["max_renewal_items"] != 32.0 || editor["max_batch_bytes"] != 4096.0 ||
		legacy["state"] != client.CreditImportStateImported || legacy["kind"] != client.CreditImportKindAutomatic || legacy["following"] != true || legacy["importable"] != true ||
		legacy["legacy_updated_at"] != "2026-10-10T18:20:00Z" || legacy["legacy_revision"] != "12" ||
		!reflect.DeepEqual(counts, map[string]any{"monthly_credits": 2.0, "refill_dates": 2.0, "readings": 2.0, "renewal_dates": 3.0}) ||
		!reflect.DeepEqual(legacy["file_counts"], counts) || legacy["file_updated_at"] != "2026-10-10T18:20:00Z" {
		t.Fatalf("editor %v", editor)
	}
	// The block is the credit state's settings.import, plus what the legacy
	// file holds now and when it last changed.
	imported := maps.Clone(legacy)
	delete(imported, "file_counts")
	delete(imported, "file_updated_at")
	if published := state["settings"].(map[string]any)["import"]; !reflect.DeepEqual(imported, published) {
		t.Fatalf("editor.legacy %v, settings.import %v", legacy, published)
	}
	credentials := editor["credentials"].([]any)
	if len(credentials) != 2 {
		t.Fatalf("credentials %v", credentials)
	}
	// A file-named index spells its address, and cannot take a date.
	if file := credentials[1].(map[string]any); file["auth_index"] != claudeFileID || file["name"] != "two@example.com" || file["renewal_editable"] != false || file["renewal"] != nil {
		t.Fatalf("credential %v", file)
	}
	credential := credentials[0].(map[string]any)
	renewal, _ := credential["renewal"].(map[string]any)
	if credential["auth_index"] != claudeHexID || credential["name"] != claudeEmail || credential["disabled"] != true || credential["renewal_editable"] != true ||
		renewal["date"] == nil || credential["renewal_source"] != client.RenewalDashboard || credential["renewal_at_epoch"] == nil {
		t.Fatalf("credential %v", credential)
	}
	orphans := editor["renewal_orphans"].([]any)
	if len(orphans) != 2 || orphans[0].(map[string]any)["updatedAtEpoch"].(float64) > orphans[1].(map[string]any)["updatedAtEpoch"].(float64) {
		t.Fatalf("renewal orphans %v", orphans)
	}
	organizations := editor["organizations"].(map[string]any)
	items := organizations["items"].([]any)
	moved := settings.LoadOrganizations(client.OrganizationsPath(p.path))
	if organizations["available"] != true || organizations["reason"] != "" || organizations["revision"] != settings.RevisionText(moved.Revision) || organizations["max_items"] != 16.0 || organizations["max_ops"] != 16.0 ||
		len(items) != 1 || items[0].(map[string]any)["id"] != alphaAccount || items[0].(map[string]any)["organization_id"] != fakeOrgA ||
		items[0].(map[string]any)["has_default_renews"] != true {
		t.Fatalf("organizations %v", organizations)
	}
	// The name lives in memory only.
	root := filepath.Dir(filepath.Dir(p.path))
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			if raw, _ := os.ReadFile(path); strings.Contains(string(raw), claudeEmail) || strings.Contains(string(raw), "two@example.com") {
				t.Errorf("%s holds a credential's name", path)
			}
		}
		return nil
	})
	p.noValueLogged(claudeEmail)
	// CPA escapes management strings below schema 6.
	for schema, escaped := range map[uint32]bool{4: true, 5: true, 6: false} {
		raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: schema, ConfigYAML: []byte("cache-path: " + p.path + "\nrequest-spacing: 1s\n" + alphaCredit)})
		if _, err := p.Handle(protocol.MethodPluginReconfigure, raw); err != nil {
			t.Fatal(err)
		}
		if got := p.statusOf()["strings_escaped"]; got != escaped {
			t.Errorf("schema %d: strings_escaped %v", schema, got)
		}
	}
}

// Before the first roster read the editor names no credential and no orphan,
// though a renewal date is saved: until CPA lists its credentials, no saved
// date is known to have lost its own, and none may be offered for removal.
func TestTheEditorWaitsForTheRoster(t *testing.T) {
	p := New(&testHost{})
	snapshot := filepath.Join(t.TempDir(), "snapshot.json")
	writeFile(t, settings.FilePath(snapshot), []byte(`{"schema":1,"revision":1,"apiCredits":{},"renewals":{"`+orphanRenewal+`":{"date":"2026-10-29","rev":1,"updatedAt":"2026-10-01T00:00:00Z"}}}`))
	store, organizations := settings.Open(snapshot), settings.OpenOrganizations(snapshot)
	if len(store.Current().Renewals) != 1 {
		t.Fatalf("the saved date did not load: %+v", store.Current())
	}
	editor := p.editorOf(store, organizations, nil, client.Snapshot{}, time.Now())
	if len(editor.Credentials) != 0 || len(editor.RenewalOrphans) != 0 || editor.Credentials == nil || editor.RenewalOrphans == nil || editor.APICreditOrphans == nil || !editor.Available || !editor.Organizations.Available {
		t.Fatalf("editor %+v", editor)
	}
	// A roster that does not list it makes it an orphan.
	p.roster.Store(&[]claudeCredential{{AuthIndex: claudeHexID, Name: claudeEmail}})
	editor = p.editorOf(store, organizations, nil, client.Snapshot{}, time.Now())
	if len(editor.Credentials) != 1 || len(editor.RenewalOrphans) != 1 || editor.RenewalOrphans[0].ID != orphanRenewal {
		t.Fatalf("after the roster: %+v", editor)
	}
}

// storesOf is the running cache's settings and organizations stores.
func (p *meterPlugin) storesOf() (*settings.Store, *settings.OrganizationStore) {
	p.t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil || p.organizations == nil {
		p.t.Fatal("the cache is not running")
	}
	return p.store, p.organizations
}

// storesClosed requires both stores of a stopped cache to refuse every write
// and to have written nothing: a save that passed the running check just
// before the stop must not reach a file once the cache let go of its writer
// lock.
func storesClosed(t *testing.T, name string, store *settings.Store, organizations *settings.OrganizationStore) {
	t.Helper()
	if _, err := store.Apply(settings.Batch{Kind: settings.KindRenewals, Renewals: []settings.RenewalItem{{ID: claudeHexID}}}, nil, time.Now()); !errors.Is(err, settings.ErrClosed) {
		t.Fatalf("%s: the settings store: %v", name, err)
	}
	before, _ := os.ReadFile(organizations.Path())
	batch := settings.OrganizationsBatch{
		BaseRevision: settings.RevisionText(organizations.Current().Revision),
		Ops:          []settings.OrganizationOp{{Op: settings.OpAdd, Label: "late", OrganizationID: fakeOrgB}},
	}
	if _, err := organizations.Apply(batch, time.Now()); !errors.Is(err, settings.ErrClosed) {
		t.Fatalf("%s: the organizations store's Apply: %v", name, err)
	}
	if _, err := organizations.Migrate([]client.APICredit{{Label: "late", OrganizationID: fakeOrgB}}, time.Now()); !errors.Is(err, settings.ErrClosed) {
		t.Fatalf("%s: the organizations store's Migrate: %v", name, err)
	}
	if after, _ := os.ReadFile(organizations.Path()); !bytes.Equal(before, after) {
		t.Fatalf("%s: a closed organizations store wrote its file", name)
	}
}

// enabled: false stops the meter, closes both stores, writes the state once
// more over the stopped meter and stops the publisher, in that order, all
// before the cache lets go of its writer lock; saves are then refused. Enabled
// again, everything starts again.
func TestEnabledFalseClosesStoreAndPublisher(t *testing.T) {
	p := newRoutePlugin(t)
	store, organizations := p.storesOf()
	steps := p.traceStops()
	p.configure(protocol.MethodPluginReconfigure, "enabled: false\n"+alphaCredit)
	got := steps()
	order := []string{}
	for _, step := range got {
		if step != "write" {
			order = append(order, step)
		}
	}
	if !reflect.DeepEqual(order, []string{"meter", "settings", "stores closed", "poller", "publisher", "cache"}) || !reflect.DeepEqual(got[len(got)-3:], []string{"publisher", "write", "cache"}) {
		t.Fatalf("steps %v", got)
	}
	if m := p.creditsFile().APICredits.Meter; m == nil || m.StoppedAtEpoch == nil || m.StopReason != client.MeterStopDisabled {
		t.Fatalf("meter %+v", m)
	}
	storesClosed(t, "disabled", store, organizations)
	p.wantError("disabled", 503, "cache_stopped", batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", "300", nil, nil)))
	if code, _ := p.status(); code != 503 {
		t.Fatalf("status %d", code)
	}
	p.configure(protocol.MethodPluginReconfigure, alphaCredit)
	if p.meter.Load() == nil || p.store == store || p.publisher == nil {
		t.Fatal("not restarted")
	}
	p.published()
	if code, body, _ := p.post(batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", "300", nil, nil))); code != 200 {
		t.Fatalf("%d %v", code, body)
	}
}

// claude-api-credits moves to the organizations file once: every item with
// no problem, in order, with its valid monthly-usd and renews as defaults,
// and nothing else; an item with a problem is skipped and counted. After that
// the configuration is ignored, with one info line per configure. Deleting
// the file brings the configuration's list back at the next tick. The
// settings file is never touched by the move.
func TestOrganizationsMoveFromTheConfigurationOnce(t *testing.T) {
	config := "claude-api-credits:\n" +
		item("one", fakeOrgA, `"200"`, `"2026-10-29"`) + "    admin-key: " + fakeAdminKey + "\n" +
		item("two", fakeOrgB, `"nope"`, "-") +
		item("-", "00000000-0000-4000-8000-0000000000ff", `"5"`, "-") +
		item("three", fakeOrgC, "-", `"2026-10-03"`) +
		item("four", "00000000-0000-4000-8000-00000000000d", "-", "-") +
		item("five", "00000000-0000-4000-8000-00000000000e", `"12.50"`, `"2026-10-31T00:00:00Z"`)
	p := newMeterPlugin(t)
	settingsRaw := []byte(`{"schema":1,"revision":1,"apiCredits":{},"renewals":{"5f2b8c41d09e7a36":{"date":"2026-10-29","rev":1,"updatedAt":"2026-10-01T00:00:00Z"}}}`)
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings.FilePath(p.path), settingsRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	p.configure(protocol.MethodPluginRegister, config)
	moved := settings.LoadOrganizations(client.OrganizationsPath(p.path))
	type org struct{ label, id, monthly, renews string }
	got := []org{}
	for _, o := range moved.Items {
		got = append(got, org{o.Label, o.OrganizationID, o.DefaultMonthlyUSD, o.DefaultRenews})
	}
	want := []org{{"one", fakeOrgA, "200", "2026-10-29"}, {"two", fakeOrgB, "", ""}, {"three", fakeOrgC, "", "2026-10-03"},
		{"four", "00000000-0000-4000-8000-00000000000d", "", ""}, {"five", "00000000-0000-4000-8000-00000000000e", "12.50", "2026-10-31T00:00:00Z"}}
	// A first file's revision is the time of the move in Unix seconds.
	if moved.State != client.OrganizationsStateOK || moved.Migration == nil || moved.Revision != uint64(moved.Migration.At.Unix()) || !reflect.DeepEqual(got, want) || moved.Migration.Moved != 5 || moved.Migration.Skipped != 1 {
		t.Fatalf("moved %+v %+v", moved, got)
	}
	if raw, _ := os.ReadFile(client.OrganizationsPath(p.path)); strings.Contains(string(raw), "FAKE") || strings.Contains(string(raw), "admin") {
		t.Fatal("the organizations file holds the admin key")
	}
	if raw, _ := os.ReadFile(settings.FilePath(p.path)); string(raw) != string(settingsRaw) {
		t.Fatal("the move touched the settings file")
	}
	// The items now come from the page, problem-free, with the defaults as
	// the configured values.
	items := *p.apiCredits.Load()
	if len(items) != 5 || items[0].credit.MonthlyUSD != "200" || items[0].credit.AdminKeyIgnored || items[1].credit.MonthlyUSDInvalid || items[4].credit.Position != 4 {
		t.Fatalf("items %+v", items)
	}
	source := p.itemSource.Load()
	if source.Source != client.OrganizationsSourcePage || source.ConfigItemsIgnored != 6 || source.Migration == nil {
		t.Fatalf("source %+v", source)
	}
	p.configure(protocol.MethodPluginReconfigure, config)
	p.configure(protocol.MethodPluginReconfigure, config)
	movedLines, ignoredLines := 0, 0
	for _, line := range p.host.logged() {
		switch line.message {
		case "quota-cache moved its Claude Console organizations to its page":
			movedLines++
			if line.level != "info" || line.fields["moved"] != 5 || line.fields["skipped"] != 1 {
				t.Fatalf("logged %+v", line)
			}
		case "quota-cache keeps Claude Console organizations on its page; claude-api-credits in its configuration is ignored":
			ignoredLines++
		}
	}
	if movedLines != 1 || ignoredLines != 2 {
		t.Fatalf("moved %d, ignored %d", movedLines, ignoredLines)
	}
	if again := settings.LoadOrganizations(client.OrganizationsPath(p.path)); again.Revision != moved.Revision {
		t.Fatal("a reconfigure moved them again")
	}
	// Starting over: deleting the file brings the configuration's list back.
	if err := os.Remove(client.OrganizationsPath(p.path)); err != nil {
		t.Fatal(err)
	}
	p.publisher.tick(time.Now().UTC())
	// Moved again past the deleted file's revision, so a page that read that
	// file conflicts rather than editing the list moved since.
	if again := settings.LoadOrganizations(client.OrganizationsPath(p.path)); again.State != client.OrganizationsStateOK || len(again.Items) != 5 || again.Revision <= moved.Revision {
		t.Fatalf("after deleting the file: %+v", again)
	}
	if source := p.itemSource.Load(); source.Source != client.OrganizationsSourcePage || len(*p.apiCredits.Load()) != 5 {
		t.Fatalf("source %+v", source)
	}

	// A configuration whose every item has a problem moves nothing, and its
	// items, with their problems, stay the list.
	broken := newMeterPlugin(t)
	broken.configure(protocol.MethodPluginRegister, "claude-api-credits:\n"+item("-", fakeOrgA, "-", "-")+item("x", "nope", "-", "-"))
	if _, err := os.Lstat(client.OrganizationsPath(broken.path)); !os.IsNotExist(err) {
		t.Fatal("a file with nothing moved")
	}
	if items := *broken.apiCredits.Load(); len(items) != 2 || items[0].credit.Problem == "" || broken.itemSource.Load().Source != client.OrganizationsSourceConfig {
		t.Fatalf("items %+v", items)
	}
	// No item and no file: nothing is written.
	empty := newMeterPlugin(t)
	empty.configure(protocol.MethodPluginRegister, "")
	empty.publisher.tick(time.Now().UTC())
	if _, err := os.Lstat(client.OrganizationsPath(empty.path)); !os.IsNotExist(err) {
		t.Fatal("a file with no item")
	}
}

// A settings or organizations file that cannot be read applies nothing and
// takes no save, until it is moved aside: the next tick then reopens it, with
// no configure.
func TestUnreadableFilesReopenAtTheNextTick(t *testing.T) {
	p := newRoutePlugin(t, func(p *meterPlugin) {
		writeFile(p.t, settings.FilePath(p.path), []byte(`{"schema":2}`))
		writeFile(p.t, client.OrganizationsPath(p.path), []byte(`{`))
	})
	editor := p.statusOf()["editor"].(map[string]any)
	if editor["available"] != false || editor["reason"] != client.CreditEditingSettingsUnreadable || editor["organizations"].(map[string]any)["reason"] != organizationsUnreadable {
		t.Fatalf("editor %v", editor)
	}
	if state := p.creditsFile(); state.Settings.State != client.CreditSettingsUnreadable || state.Organizations.State != client.OrganizationsStateUnreadable || state.Organizations.Source != client.OrganizationsSourceConfig {
		t.Fatalf("published %+v %+v", state.Settings, state.Organizations)
	}
	for _, path := range []string{settings.FilePath(p.path), client.OrganizationsPath(p.path)} {
		if err := os.Rename(path, path+".aside"); err != nil {
			t.Fatal(err)
		}
	}
	p.publisher.tick(time.Now().UTC())
	editor = p.statusOf()["editor"].(map[string]any)
	if editor["available"] != true || editor["organizations"].(map[string]any)["available"] != true {
		t.Fatalf("editor %v", editor)
	}
	// The organizations file was gone with the configuration still listing
	// alpha, so the tick moved it again.
	state := p.creditsFile()
	if state.Settings.State != client.CreditSettingsOK || state.Organizations.State != client.OrganizationsStateOK || state.Organizations.Source != client.OrganizationsSourcePage {
		t.Fatalf("published %+v %+v", state.Settings, state.Organizations)
	}
	if code, body, _ := p.post(batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", "300", nil, nil))); code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	warnings := 0
	for _, line := range p.host.logged() {
		if line.message == "quota-cache cannot read its saved API credit settings; only config values apply until the file is moved aside" {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatalf("warned %d times", warnings)
	}
}
