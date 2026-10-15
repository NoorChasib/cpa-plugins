package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	want := []protocol.ManagementRoute{{Method: "GET", Path: "/plugins/quota-cache/status"}, {Method: "POST", Path: "/plugins/quota-cache/settings"}, {Method: "POST", Path: "/plugins/quota-cache/warmup"}}
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
		// Accepted and ignored: organizations are added, renamed and removed
		// on Quota Cache's own page. It is decoded so that a configuration
		// written for quota-cache 0.1.x still loads.
		"claude-api-credits": "accepted and ignored",
		// Accepted and ignored: quota-cache 0.3.0's Claude Code runner. Warm-ups
		// go through CPA now; a configuration written for 0.3.0 still loads.
		"warmup-cli-path":     "accepted and ignored",
		"warmup-profile-root": "accepted and ignored",
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
		if !decoded[field.Name] || (field.Type != "string" && field.Type != "boolean" && field.Type != "enum") || field.Description == "" {
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

// fiveOrgs is a five-organization settings file in Quota Glance 0.7's
// format: revision 12, a renewal date for the route fixture's credential and
// two for credentials no longer listed.
func fiveOrgs(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "settings", "testdata", "legacy", "five-orgs.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
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
		if err := os.WriteFile(settings.FilePath(p.path), fiveOrgs(t), 0o600); err != nil {
			t.Fatal(err)
		}
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
	if state == nil || state["schema"] != 1.0 || state["api_credits"] == nil ||
		!reflect.DeepEqual(state["settings"], map[string]any{"state": "ok", "editable": true, "revision": "12"}) {
		t.Fatalf("credit_state %v", status["credit_state"])
	}
	editor := status["editor"].(map[string]any)
	keys := slices.Sorted(maps.Keys(editor))
	if want := []string{"api_credit_orphans", "available", "credentials", "held_api_credits", "max_api_credit_items", "max_batch_bytes",
		"max_renewal_items", "organizations", "reason", "renewal_orphans", "revision"}; !slices.Equal(keys, want) {
		t.Fatalf("editor keys %v, want %v", keys, want)
	}
	if editor["available"] != true || editor["reason"] != "" || editor["revision"] != "12" || editor["max_api_credit_items"] != 16.0 || editor["max_renewal_items"] != 32.0 || editor["max_batch_bytes"] != 4096.0 {
		t.Fatalf("editor %v", editor)
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
	listed := settings.LoadOrganizations(client.OrganizationsPath(p.path))
	if organizations["available"] != true || organizations["reason"] != "" || organizations["revision"] != settings.RevisionText(listed.Revision) || organizations["max_items"] != 16.0 || organizations["max_ops"] != 16.0 ||
		len(items) != 1 || !reflect.DeepEqual(items[0], map[string]any{"id": alphaAccount, "label": "alpha", "organization_id": fakeOrgA}) {
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
		raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: schema, ConfigYAML: []byte("cache-path: " + p.path + "\nrequest-spacing: 1s\n")})
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
	p.configure(protocol.MethodPluginReconfigure, "enabled: false\n")
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
	p.configure(protocol.MethodPluginReconfigure, "")
	if p.meter.Load() == nil || p.store == store || p.publisher == nil {
		t.Fatal("not restarted")
	}
	p.published()
	if code, body, _ := p.post(batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", "300", nil, nil))); code != 200 {
		t.Fatalf("%d %v", code, body)
	}
}

// ignoredLine is what a configure logs while the cache runs and
// claude-api-credits lists items.
const ignoredLine = "quota-cache keeps Claude Console organizations on its page; claude-api-credits in its configuration is ignored"

// saidIgnored is how many times the plugin has logged ignoredLine.
func (p *meterPlugin) saidIgnored() int {
	said := 0
	for _, line := range p.host.logged() {
		if line.message == ignoredLine {
			said++
		}
	}
	return said
}

// claude-api-credits still loads, as in 0.1.x, and is ignored: with no
// organizations file its items list no organization and write no file, the
// list is missing and editable, the count is published, and one info line per
// configure says so. Nothing in an item is read, flagged or warned about. A
// page add then creates the file and lists only the page's organization, and
// each configure still says the configuration is ignored, until the cache is
// turned off.
func TestTheConfiguredOrganizationsAreIgnored(t *testing.T) {
	config := "claude-api-credits:\n" +
		item("one", fakeOrgA, `"200"`, `"2026-10-29"`) + "    admin-key: " + fakeAdminKey + "\n" +
		item("-", "nope", `"5"`, "-") + "  - just text\n"
	p := newMeterPlugin(t)
	p.configure(protocol.MethodPluginRegister, config)
	if _, err := os.Lstat(client.OrganizationsPath(p.path)); !os.IsNotExist(err) {
		t.Fatal("the configuration wrote an organizations file")
	}
	if items := *p.apiCredits.Load(); len(items) != 0 || p.meter.Load() != nil {
		t.Fatalf("items %+v, meter %v", items, p.meter.Load())
	}
	state := p.published()
	if state.APICredits != nil || state.Organizations != (client.CreditOrganizations{State: client.OrganizationsStateMissing, ConfigItemsIgnored: 3}) {
		t.Fatalf("published %+v %+v", state.Organizations, state.APICredits)
	}
	organizations := p.statusOf()["editor"].(map[string]any)["organizations"].(map[string]any)
	if organizations["available"] != true || organizations["revision"] != "" || len(organizations["items"].([]any)) != 0 {
		t.Fatalf("editor %v", organizations)
	}
	p.configure(protocol.MethodPluginReconfigure, config)
	p.configure(protocol.MethodPluginReconfigure, config)
	// A configuration that lists none says nothing.
	p.configure(protocol.MethodPluginReconfigure, "claude-api-credits: []\n")
	if state := p.creditsFile(); state.Organizations.ConfigItemsIgnored != 0 {
		t.Fatalf("published %+v", state.Organizations)
	}
	for _, line := range p.host.logged() {
		switch {
		case line.message == ignoredLine:
			if line.level != "info" || len(line.fields) != 0 {
				t.Fatalf("logged %+v", line)
			}
		case line.level != "info" || strings.Contains(line.message, "admin-key") || strings.Contains(line.message, "moved"):
			t.Fatalf("logged %+v", line)
		}
	}
	if said := p.saidIgnored(); said != 3 {
		t.Fatalf("said %d times that the configuration is ignored, want once per configure that lists items", said)
	}

	// A page add creates the file and lists only the page's organization.
	p.configure(protocol.MethodPluginReconfigure, config)
	p.organizationOps(addOp("bravo", fakeOrgB))
	listed := settings.LoadOrganizations(client.OrganizationsPath(p.path))
	if listed.State != client.OrganizationsStateOK || len(listed.Items) != 1 || listed.Items[0].OrganizationID != fakeOrgB {
		t.Fatalf("the file %+v", listed)
	}
	bravo := client.APICreditOrgAccount(fakeOrgB)
	state = p.creditsFile()
	if state.APICredits == nil || len(state.APICredits.Accounts) != 1 || state.APICredits.Accounts[0].ID != bravo ||
		state.Organizations != (client.CreditOrganizations{State: client.OrganizationsStateOK, Revision: settings.RevisionText(listed.Revision), ConfigItemsIgnored: 3}) {
		t.Fatalf("published %+v %+v", state.Organizations, state.APICredits)
	}
	if p.meter.Load() == nil {
		t.Fatal("adding bravo did not start the meter")
	}
	snapshot := waitFor(t, p.path, func(s client.Snapshot) bool {
		_, ok := s.Entries[client.Key(client.ProviderAnthropicAPI, bravo)]
		return ok
	})
	for key, entry := range snapshot.Entries {
		if entry.Provider == client.ProviderAnthropicAPI && key != client.Key(client.ProviderAnthropicAPI, bravo) {
			t.Fatalf("the snapshot lists %s", key)
		}
	}
	for _, path := range []string{p.path, client.CreditsPath(p.path), client.OrganizationsPath(p.path)} {
		if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "FAKE") || strings.Contains(string(raw), fakeOrgA) {
			t.Fatalf("%s holds something from the configuration", path)
		}
	}

	// With organizations on the page, a configure that lists items still says
	// they are ignored; one that turns the cache off, or comes while it is
	// off, says nothing.
	said := p.saidIgnored()
	p.configure(protocol.MethodPluginReconfigure, config)
	if now := p.saidIgnored(); now != said+1 {
		t.Fatalf("with bravo on the page, said %d times that the configuration is ignored, want %d", now, said+1)
	}
	p.configure(protocol.MethodPluginReconfigure, "enabled: false\n"+config)
	p.configure(protocol.MethodPluginReconfigure, "enabled: false\n"+config)
	if now := p.saidIgnored(); now != said+1 {
		t.Fatalf("while the cache is off, said %d times that the configuration is ignored, want %d", now, said+1)
	}
}

// An organizations file that cannot be read lists no organization: the meter
// stops as when the last one is removed, editing the list is unavailable, and
// the configuration is still ignored, with no list of its own to stand in.
// Moved aside, the file is missing at the next tick, and the list is empty and
// editable again.
func TestAnUnreadableOrganizationsFileListsNothing(t *testing.T) {
	p := newRoutePlugin(t)
	if p.meter.Load() == nil {
		t.Fatal("no meter for alpha")
	}
	p.configure(protocol.MethodPluginReconfigure, alphaCredit)
	p.breakOrganizations()
	p.publisher.tick(time.Now().UTC())
	if p.meter.Load() != nil || len(*p.apiCredits.Load()) != 0 {
		t.Fatalf("meter %v, items %+v", p.meter.Load(), *p.apiCredits.Load())
	}
	if file := p.meterFile(); file.StopReason != client.MeterStopNoItems {
		t.Fatalf("meter file %+v", file)
	}
	check := func(step string) {
		t.Helper()
		state := p.creditsFile()
		if state.APICredits != nil || state.Organizations != (client.CreditOrganizations{State: client.OrganizationsStateUnreadable, ConfigItemsIgnored: 1}) {
			t.Fatalf("%s: published %+v %+v", step, state.Organizations, state.APICredits)
		}
		organizations := p.statusOf()["editor"].(map[string]any)["organizations"].(map[string]any)
		if organizations["available"] != false || organizations["reason"] != organizationsUnreadable || len(organizations["items"].([]any)) != 0 {
			t.Fatalf("%s: editor %v", step, organizations)
		}
		p.wantError(step, http.StatusServiceUnavailable, "organizations_unavailable",
			map[string]any{"kind": settings.KindOrganizations, "baseRevision": "", "ops": []any{addOp("bravo", fakeOrgB)}})
	}
	check("after the tick")
	// A configure changes none of it, and still says the configuration is
	// ignored.
	said := p.saidIgnored()
	p.configure(protocol.MethodPluginReconfigure, alphaCredit)
	check("after a configure")
	if now := p.saidIgnored(); now != said+1 {
		t.Fatalf("with the file unreadable, said %d times that the configuration is ignored, want %d", now, said+1)
	}
	if p.meter.Load() != nil {
		t.Fatal("a configure started the meter")
	}
	if err := os.Rename(client.OrganizationsPath(p.path), client.OrganizationsPath(p.path)+".aside"); err != nil {
		t.Fatal(err)
	}
	p.publisher.tick(time.Now().UTC())
	if state := p.creditsFile(); state.APICredits != nil || state.Organizations != (client.CreditOrganizations{State: client.OrganizationsStateMissing, ConfigItemsIgnored: 1}) {
		t.Fatalf("moved aside: published %+v", state.Organizations)
	}
	p.organizationOps(addOp("alpha", fakeOrgA))
	if p.meter.Load() == nil {
		t.Fatal("adding alpha again did not start the meter")
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
	if state := p.creditsFile(); state.Settings.State != client.CreditSettingsUnreadable || state.Organizations.State != client.OrganizationsStateUnreadable || state.APICredits != nil {
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
	// Both files are gone: no value and no organization, both editable.
	state := p.creditsFile()
	if state.Settings.State != client.CreditSettingsOK || state.Organizations.State != client.OrganizationsStateMissing || state.APICredits != nil {
		t.Fatalf("published %+v %+v", state.Settings, state.Organizations)
	}
	p.organizationOps(addOp("alpha", fakeOrgA))
	if code, body, _ := p.post(batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", "300", nil, nil))); code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	warnings := 0
	for _, line := range p.host.logged() {
		if line.message == "quota-cache cannot read its saved API credit settings; none apply until the file is moved aside" {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatalf("warned %d times", warnings)
	}
}

// A settings or organizations file that could not be opened, as one restored
// as another user, opens at the next tick once its owner is fixed, which
// changes none of what the publisher's stamps compare: that tick publishes the
// file as readable, and a save goes through. Each file is the only one broken,
// so the tick computes on its own store's report.
func TestFilesThatOpenAgainArePublishedAtTheNextTick(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a file whatever its mode")
	}
	for name, pathOf := range map[string]func(string) string{"settings": settings.FilePath, "organizations": client.OrganizationsPath} {
		t.Run(name, func(t *testing.T) {
			p := newRoutePlugin(t)
			store, organizations := p.storesOf()
			fixed := unopenable(t, pathOf(p.path))
			store.Sync()
			p.followOrganizations(organizations, time.Now().UTC())
			// The stamps the state is published from next are those it was
			// last computed from, while the store has not read its file since
			// it failed.
			fixed()
			p.publisher.PublishNow()
			if unread := unreadFiles(p.creditsFile()); !slices.Equal(unread, []string{name}) {
				t.Fatalf("before the tick, published %v as unread", unread)
			}
			p.publisher.tick(time.Now().UTC())
			if unread := unreadFiles(p.creditsFile()); len(unread) != 0 {
				t.Fatalf("after the tick, published %v as unread", unread)
			}
			if editor := p.statusOf()["editor"].(map[string]any); editor["available"] != true || editor["organizations"].(map[string]any)["available"] != true {
				t.Fatalf("editor %v", editor)
			}
			if code, body, _ := p.post(batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", "300", nil, nil))); code != 200 {
				t.Fatalf("%d %v", code, body)
			}
			p.organizationOps(addOp("bravo", fakeOrgB))
		})
	}
}

// unopenable makes path a file this process cannot open, as one restored as
// another user, and returns what fixes it: its mode put back, which leaves
// every field the publisher's stamps compare as it was.
func unopenable(t *testing.T, path string) func() {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if err := os.Chmod(path, info.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
	}
}

// unreadFiles names the files state says were not read: "settings" and
// "organizations".
func unreadFiles(state *client.CreditState) []string {
	unread := []string{}
	if state.Settings.State != client.CreditSettingsOK {
		unread = append(unread, "settings")
	}
	if state.Organizations.State != client.OrganizationsStateOK {
		unread = append(unread, "organizations")
	}
	return unread
}
