package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/cache"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/settings"
)

// The spec's fake organization ids, and an obviously fake admin key that
// must never be read. Nothing here talks to Anthropic.
const (
	fakeOrgA     = "00000000-0000-4000-8000-00000000000a"
	fakeOrgB     = "00000000-0000-4000-8000-00000000000b"
	fakeOrgC     = "00000000-0000-4000-8000-00000000000c"
	fakeAdminKey = "sk-ant-admin01-FAKEFAKEFAKEFAKE"
)

// configuredItems registers a disabled plugin with yaml, through the real
// strict decoder, and returns how many claude-api-credits items it counted and
// the item list it put in force.
func configuredItems(t *testing.T, config string) (int64, []creditItem, error) {
	t.Helper()
	p := New(&testHost{})
	defer p.Shutdown()
	raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("enabled: false\n" + config)})
	if _, err := p.Handle(protocol.MethodPluginRegister, raw); err != nil {
		return 0, nil, err
	}
	return p.configItems.Load(), *p.apiCredits.Load(), nil
}

// item renders one claude-api-credits item from the four fields, each a raw
// YAML value; a field given as "-" is left out.
func item(label, org, monthly, renews string) string {
	lines := []string{}
	for _, field := range [][2]string{{"label", label}, {"organization-id", org}, {"monthly-usd", monthly}, {"renews", renews}} {
		if field[1] != "-" {
			lines = append(lines, field[0]+": "+field[1])
		}
	}
	return "  - " + strings.Join(lines, "\n    ") + "\n"
}

// claude-api-credits is decoded as in 0.1.x, so a configuration written for
// one still loads: a value that is not a list rejects the configure, as a
// mistyped key does, and a list of anything is accepted and only counted.
// Nothing in it is listed.
func TestCreditListShapeAndOtherSettings(t *testing.T) {
	for _, bad := range []string{"claude-api-credits: {}\n", "claude-api-credits: text\n", "claude-api-credits:\n  label: x\n", "claude-api-credit:\n  - label: x\n"} {
		if _, _, err := configuredItems(t, bad); err == nil || err.Error() != "invalid quota-cache configuration" {
			t.Errorf("%q: err=%v; a list is required", bad, err)
		}
	}
	for config, want := range map[string]int64{
		"": 0, "claude-api-credits:\n": 0, "claude-api-credits: []\n": 0, "claude-api-credits: null\n": 0,
		"claude-api-credits:\n" + item("ok", fakeOrgA, `"200"`, `"2026-10-29"`) + "    admin-key: " + fakeAdminKey + "\n" +
			item("-", "nope", "nope", "nope") + "  - just text\n  - [1, 2]\n": 4,
		"claude-api-credits:\n  - label: a\n    organization-id: " + fakeOrgA + "\n    monthly-usd: &m \"1\"\n  - label: b\n    organization-id: " + fakeOrgB + "\n    monthly-usd: *m\n": 2,
	} {
		count, items, err := configuredItems(t, config)
		if err != nil || count != want || len(items) != 0 {
			t.Errorf("%q: counted %d, listed %+v, err=%v; want %d counted and none listed", config, count, items, err, want)
		}
	}
}

// Organizations are edited on the page, and the panel could not describe a
// list of objects or check one item anyway, so the panel's fields are
// unchanged.
func TestCreditsAreNotAPanelField(t *testing.T) {
	p := New(&testHost{})
	defer p.Shutdown()
	raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("enabled: false\n")})
	result, err := p.Handle(protocol.MethodPluginRegister, raw)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, field := range result.(protocol.Registration).Metadata.ConfigFields {
		names = append(names, field.Name)
	}
	if want := []string{"cache-path", "poll-interval", "request-spacing", "openrouter-management-key"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("fields=%v want %v", names, want)
	}
}

// fetcherWithOrganizations is a fetcher listing the organizations given, each
// a label and an Organization ID, as the page lists them.
func fetcherWithOrganizations(host Host, organizations ...[2]string) hostFetcher {
	listed := settings.Organizations{State: client.OrganizationsStateOK}
	for _, o := range organizations {
		listed.Items = append(listed.Items, settings.Organization{Label: o[0], OrganizationID: o[1]})
	}
	items, _ := itemsOf(listed, 0)
	f := fetcherWith(host, "")
	f.apiCredits = &atomic.Pointer[[]creditItem]{}
	f.apiCredits.Store(&items)
	return f
}

func TestCreditAccountsAreListedAfterTheRosterChecks(t *testing.T) {
	organizations := [][2]string{{"siphorchannel", fakeOrgA}, {"after", fakeOrgC}}
	indexed := diskEntry("claude-one.json", "claude")
	indexed.AuthIndex = "one"
	accounts, err := fetcherWithOrganizations(rosterHost{[]protocol.HostAuthFileEntry{indexed}}, organizations...).List(context.Background())
	if err != nil || len(accounts) != 3 {
		t.Fatalf("accounts=%+v err=%v", accounts, err)
	}
	if accounts[0].Provider != "claude" || accounts[0].Credit != nil {
		t.Fatalf("first=%+v", accounts[0])
	}
	// Each carries its organization and nothing else.
	for i, o := range organizations {
		account := accounts[i+1]
		if account.Provider != client.ProviderAnthropicAPI || account.AuthIndex != client.APICreditOrgAccount(o[1]) || account.Credit == nil ||
			*account.Credit != (client.APICredit{Label: o[0], Position: i, OrganizationID: o[1]}) {
			t.Fatalf("credit account %d=%+v %+v", i, account, account.Credit)
		}
	}
	// Credit accounts are no reason to stop waiting for CPA's roster.
	accounts, err = fetcherWithOrganizations(rosterHost{[]protocol.HostAuthFileEntry{diskEntry("claude-one.json", "claude")}}, organizations...).List(context.Background())
	if !errors.Is(err, cache.ErrRosterNotReady) || accounts != nil {
		t.Fatalf("accounts=%+v err=%v", accounts, err)
	}
	// Each listing hands out its own copy of the organization.
	f := fetcherWithOrganizations(rosterHost{}, organizations...)
	accounts, _ = f.List(context.Background())
	accounts[0].Credit.Label = "changed"
	if again, _ := f.List(context.Background()); again[0].Credit.Label != "siphorchannel" {
		t.Fatal("a listing shares its credit with the item list")
	}
}

// The cache never asks for a credit account; should it, the fetcher refuses
// without a request. testHost panics on any credential read or HTTP call.
func TestCreditFetchNeverCallsTheHost(t *testing.T) {
	f := fetcherWithOrganizations(&testHost{}, [2]string{"siphorchannel", fakeOrgA})
	accounts, _ := f.List(context.Background())
	if _, err := f.Fetch(context.Background(), accounts[0], nil); err == nil || err.Error() != "not polled" {
		t.Fatalf("err=%v", err)
	}
}

// meterHost holds no credentials, answers no HTTP, and keeps every log line.
type meterHost struct {
	mu    sync.Mutex
	lines []logLine
}

func (*meterHost) ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error) { return nil, nil }
func (*meterHost) GetAuth(context.Context, string) ([]byte, error) {
	panic("a credit account must not read a CPA credential")
}
func (*meterHost) HTTPDo(context.Context, protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	panic("the meter must not make a request")
}
func (h *meterHost) Log(_ context.Context, level, message string, fields map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lines = append(h.lines, logLine{level, message, fields})
}

func (h *meterHost) logged() []logLine {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]logLine(nil), h.lines...)
}

// meterPlugin is a registered plugin writing to its own temporary cache
// path, with configure and status helpers.
type meterPlugin struct {
	*Plugin
	t    *testing.T
	host *meterHost
	path string
}

func newMeterPlugin(t *testing.T) *meterPlugin {
	t.Helper()
	host := &meterHost{}
	p := &meterPlugin{Plugin: New(host), t: t, host: host, path: filepath.Join(t.TempDir(), "cache", "snapshot.json")}
	t.Cleanup(p.Shutdown)
	return p
}

func (p *meterPlugin) configure(method, extra string) protocol.Registration {
	p.t.Helper()
	raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("cache-path: " + p.path + "\nrequest-spacing: 1s\n" + extra)})
	result, err := p.Handle(method, raw)
	if err != nil {
		p.t.Fatal(err)
	}
	return result.(protocol.Registration)
}

func (p *meterPlugin) status() (int, string) {
	p.t.Helper()
	raw, _ := json.Marshal(protocol.ManagementRequest{Method: "GET", Path: "/v0/management/plugins/quota-cache/status"})
	result, err := p.Handle(protocol.MethodManagementHandle, raw)
	if err != nil {
		p.t.Fatal(err)
	}
	response := result.(protocol.ManagementResponse)
	return response.StatusCode, string(response.Body)
}

func (p *meterPlugin) meterFile() *client.APIMeter {
	p.t.Helper()
	meter, err := client.LoadMeter(client.MeterPath(p.path))
	if err != nil {
		p.t.Fatalf("meter file: %v", err)
	}
	return meter
}

func (p *meterPlugin) usage(raw []byte) {
	p.t.Helper()
	result, err := p.Handle(protocol.MethodUsageHandle, raw)
	if err != nil || result != struct{}{} {
		p.t.Fatalf("usage.handle answered %#v, %v", result, err)
	}
}

func goldenRecord(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "meter", "testdata", "usage-record-v8.0.22.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// oneCredit is claude-api-credits as quota-cache 0.1.x read it, listing
// siphorchannel. This quota-cache ignores it.
const oneCredit = "claude-api-credits:\n  - label: siphorchannel\n    organization-id: " + fakeOrgA + "\n    monthly-usd: \"200\"\n    renews: \"2026-10-29\"\n"

// Usage records are declared wanted whenever the cache runs, on register and
// on every reconfigure, so an organization added on the page is counted from
// its first record with no configure; disabled declares none. The meter runs
// only while an organization is linked: with none it stops with no_items and
// its file stays; disabled stops it with disabled. Routes are unchanged.
func TestUsagePluginIsDeclaredWhileTheCacheRuns(t *testing.T) {
	p := newMeterPlugin(t)
	if reg := p.configure(protocol.MethodPluginRegister, ""); !reg.Capabilities.UsagePlugin || !reg.Capabilities.ManagementAPI || p.meter.Load() != nil {
		t.Fatalf("registration=%+v meter=%v", reg.Capabilities, p.meter.Load())
	}
	if _, err := os.Lstat(client.MeterPath(p.path)); !os.IsNotExist(err) {
		t.Fatal("a meter file exists with no organization")
	}
	// Listed in the configuration, it is not counted: the meter waits for
	// the page.
	if reg := p.configure(protocol.MethodPluginReconfigure, oneCredit); !reg.Capabilities.UsagePlugin || p.meter.Load() != nil {
		t.Fatalf("registration=%+v meter=%v", reg.Capabilities, p.meter.Load())
	}
	// Added on the page, it starts the meter with no configure.
	p.organizationOps(addOp("siphorchannel", fakeOrgA))
	m := p.meter.Load()
	if m == nil {
		t.Fatal("adding an organization did not start the meter")
	}
	p.usage(goldenRecord(t))
	// A reconfigure keeps the meter; so does a schedule change, and so does a
	// configuration that no longer lists the organization.
	p.configure(protocol.MethodPluginReconfigure, oneCredit)
	p.configure(protocol.MethodPluginReconfigure, "poll-interval: 30m\n"+oneCredit)
	if reg := p.configure(protocol.MethodPluginReconfigure, ""); !reg.Capabilities.UsagePlugin || p.meter.Load() != m {
		t.Fatalf("registration=%+v; a reconfigure replaced the meter", reg.Capabilities)
	}
	// Removing the last organization on the page stops it, with no configure.
	p.removeAll()
	if p.meter.Load() != nil {
		t.Fatal("the meter outlived the last organization")
	}
	file := p.meterFile()
	if file.StopReason != client.MeterStopNoItems || file.StoppedAt == nil || file.Counted != 1 || file.Received != 1 {
		t.Fatalf("file after no_items: %+v", file)
	}
	// An organization again: the meter carries on from the file, with a gap.
	p.organizationOps(addOp("siphorchannel", fakeOrgA))
	if p.meter.Load() == nil {
		t.Fatal("adding an organization did not start the meter")
	}
	if reg := p.configure(protocol.MethodPluginReconfigure, "enabled: false\n"+oneCredit); reg.Capabilities.UsagePlugin || p.meter.Load() != nil || p.cache != nil {
		t.Fatalf("registration=%+v meter=%v cache=%v", reg.Capabilities, p.meter.Load(), p.cache)
	}
	file = p.meterFile()
	if file.StopReason != client.MeterStopDisabled || len(file.Gaps) != 1 || file.Gaps[0].Reason != client.MeterStopNoItems || file.Restarts != 1 || file.Counted != 1 {
		t.Fatalf("file after disabled: %+v", file)
	}
	raw, _ := json.Marshal(protocol.ManagementRequest{})
	result, err := p.Handle(protocol.MethodManagementRegister, raw)
	if err != nil {
		t.Fatal(err)
	}
	registration := result.(protocol.ManagementRegistration)
	if len(registration.Routes) != 2 || registration.Routes[0].Path != "/plugins/quota-cache/status" || registration.Routes[1].Path != "/plugins/quota-cache/settings" ||
		len(registration.Resources) != 1 || registration.Resources[0].Path != "/status" {
		t.Fatalf("routes=%+v", registration)
	}
}

// End to end through configure, the real writer and the meter: a record CPA
// sends is counted for its organization, an OAuth record never is, and
// nothing the record carries beyond what the meter keeps reaches the meter
// file, the snapshot, the status route or the log. claude-api-credits, with
// the admin-key quota-cache 0.1.13 used and an item with a problem, is
// ignored: nothing in it is read, flagged or warned about.
func TestUsageRecordsAreMeteredAndSecretsNeverEscape(t *testing.T) {
	p := newMeterPlugin(t)
	p.writeOrganizations([2]string{"siphorchannel", fakeOrgA})
	p.configure(protocol.MethodPluginRegister, oneCredit+"    admin-key: "+fakeAdminKey+"\n"+item("-", "nope", `"5"`, `"2026-10-01"`)+"    admin-key: "+fakeAdminKey+"\n")
	good := client.Key(client.ProviderAnthropicAPI, client.APICreditOrgAccount(fakeOrgA))
	snapshot := waitFor(t, p.path, func(s client.Snapshot) bool {
		_, ok := s.Entries[good]
		return ok
	})
	if entry := snapshot.Entries[good]; entry.APICredit == nil || *entry.APICredit != (client.APICredit{Label: "siphorchannel", OrganizationID: fakeOrgA}) || !entry.LastAttempt.IsZero() {
		t.Fatalf("entry=%+v", entry.APICredit)
	}
	for key, entry := range snapshot.Entries {
		if entry.Provider == client.ProviderAnthropicAPI && key != good {
			t.Fatalf("the configuration listed %s", key)
		}
	}
	before, _ := os.ReadFile(p.path)

	golden := goldenRecord(t)
	p.usage(golden)
	oauth := strings.Replace(string(golden), `"AuthType":"apikey"`, `"AuthType":"oauth"`, 1)
	oauth = strings.Replace(oauth, fakeOrgA, fakeOrgC, 1)
	p.usage([]byte(oauth))
	p.usage([]byte(`not a record`))
	p.usage(nil)
	// Stopping the meter writes it; the snapshot is not touched by that.
	// The organizations are unchanged, so no scan rewrites it either.
	p.meter.Load().Stop(client.MeterStopQuiesce, time.Now().UTC())
	file := p.meterFile()
	if file.Received != 4 || file.Counted != 1 || file.Rejected != 2 || len(file.Unlinked) != 0 || file.Organizations[fakeOrgA].ClaudeCodeRefusals != 1 {
		t.Fatalf("meter file: %+v", file)
	}
	if _, listed := file.Organizations[fakeOrgC]; listed {
		t.Fatal("an OAuth record's organization reached the meter")
	}
	if after, _ := os.ReadFile(p.path); string(after) != string(before) {
		t.Fatal("writing the meter rewrote the snapshot")
	}
	p.configure(protocol.MethodPluginReconfigure, "")
	code, status := p.status()
	if code != 200 || !strings.Contains(status, `"api_meter":{"schema":1,`) || strings.Contains(status, "admin_key") {
		t.Fatalf("status %d: %s", code, status)
	}
	rawMeter, _ := os.ReadFile(client.MeterPath(p.path))
	p.publisher.PublishNow()
	rawCredits, err := os.ReadFile(client.CreditsPath(p.path))
	if err != nil {
		t.Fatal(err)
	}
	lines := p.host.logged()
	items := *p.apiCredits.Load()
	for name, text := range map[string]string{
		"snapshot": string(before), "meter": string(rawMeter), "credits": string(rawCredits), "status": status,
		"logs": fmt.Sprintf("%v %+v %#v", lines, lines, lines), "items": fmt.Sprintf("%v %+v %#v", items, items, items),
	} {
		for _, secret := range []string{fakeAdminKey, "FAKEFAKE", "sk-ant", "key_fingerprint", "SOURCE-CANARY", "APIKEY-CANARY", "AUTHID-CANARY", "BASEURL-CANARY",
			"SESSION-CANARY", "PARENT-CANARY", "BODY-CANARY", "HEADER-VALUE-CANARY", "ALIAS-CANARY", "req_"} {
			if strings.Contains(text, secret) {
				t.Fatalf("the %s carries %q", name, secret)
			}
		}
	}
	// The meter file keeps model names and organization ids; the log never
	// names either.
	for _, named := range []string{"claude-sonnet", fakeOrgA, fakeOrgC, "0123456789abcdef"} {
		if strings.Contains(fmt.Sprintf("%+v", lines), named) {
			t.Fatalf("the log names %q", named)
		}
	}
	for _, line := range lines {
		if line.level == "error" || strings.Contains(line.message, "admin-key") {
			t.Fatalf("logged %+v", line)
		}
	}
}

// The status route carries the meter file when there is one it can read,
// and omits it otherwise; a poll's save never touches the meter file.
func TestStatusCarriesTheMeterOnlyWhenTheFileExists(t *testing.T) {
	p := newMeterPlugin(t)
	p.configure(protocol.MethodPluginRegister, "")
	waitFor(t, p.path, func(s client.Snapshot) bool { return !s.WrittenAt.IsZero() })
	if code, status := p.status(); code != 200 || strings.Contains(status, "api_meter") {
		t.Fatalf("status %d carries a meter with no file: %s", code, status)
	}
	p.organizationOps(addOp("siphorchannel", fakeOrgA))
	p.removeAll()
	rawMeter, err := os.ReadFile(client.MeterPath(p.path))
	if err != nil {
		t.Fatal(err)
	}
	if code, status := p.status(); code != 200 || !strings.Contains(status, `"api_meter":{"schema":1,`) || !strings.Contains(status, `"stop_reason":"no_items"`) {
		t.Fatalf("status %d: %s", code, status)
	}
	// An organization added on the page makes the writer save the snapshot,
	// while the meter it restarts has not saved yet; the meter file is as it
	// was.
	p.organizationOps(addOp("after", fakeOrgC))
	waitFor(t, p.path, func(s client.Snapshot) bool {
		_, ok := s.Entries[client.Key(client.ProviderAnthropicAPI, client.APICreditOrgAccount(fakeOrgC))]
		return ok
	})
	if again, _ := os.ReadFile(client.MeterPath(p.path)); string(again) != string(rawMeter) {
		t.Fatal("a snapshot save rewrote the meter file")
	}
	if err := os.WriteFile(client.MeterPath(p.path), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if code, status := p.status(); code != 200 || strings.Contains(status, "api_meter") {
		t.Fatalf("status %d carries an unreadable meter: %s", code, status)
	}
}

// Quiesce stops the meter with its reason and the register that follows, as
// CPA's rollback of a failed replacement does, carries on from the file
// with a quiesce gap; shutdown leaves a shutdown stop in the file.
func TestQuiesceAndShutdownAreRecordedForTheNextStart(t *testing.T) {
	p := newMeterPlugin(t)
	p.writeOrganizations([2]string{"siphorchannel", fakeOrgA})
	p.configure(protocol.MethodPluginRegister, "")
	p.usage(goldenRecord(t))
	if _, err := p.Handle(protocol.MethodPluginQuiesce, nil); err != nil {
		t.Fatal(err)
	}
	if file := p.meterFile(); file.StopReason != client.MeterStopQuiesce || file.Counted != 1 || p.meter.Load() != nil {
		t.Fatalf("after quiesce: %+v meter=%v", file, p.meter.Load())
	}
	p.configure(protocol.MethodPluginRegister, "")
	p.usage(goldenRecord(t))
	p.Shutdown()
	file := p.meterFile()
	if file.StopReason != client.MeterStopShutdown || len(file.Gaps) != 1 || file.Gaps[0].Reason != client.MeterStopQuiesce || !file.Gaps[0].Brief() || file.Restarts != 1 || file.Counted != 2 {
		t.Fatalf("after shutdown: %+v", file)
	}
	if p.meter.Load() != nil {
		t.Fatal("the meter outlived shutdown")
	}
}

// A Quota Cache that starts with no organization opens no meter, so the file
// keeps the stop the last run wrote, as an update from 0.1.14 leaves it:
// quiesce, through a CPA restart too. The first organization added carries on
// from the file with a gap of that reason.
func TestAStartWithNoOrganizationKeepsTheStopItFinds(t *testing.T) {
	updated := newMeterPlugin(t)
	updated.writeOrganizations([2]string{"alpha", fakeOrgA})
	updated.configure(protocol.MethodPluginRegister, "")
	updated.usage(goldenRecord(t))
	if _, err := updated.Handle(protocol.MethodPluginQuiesce, nil); err != nil {
		t.Fatal(err)
	}
	stopped := updated.meterFile()
	if stopped.StopReason != client.MeterStopQuiesce || stopped.StoppedAt == nil {
		t.Fatalf("after quiesce: %+v", stopped)
	}
	if err := os.Remove(client.OrganizationsPath(updated.path)); err != nil {
		t.Fatal(err)
	}
	host := &meterHost{}
	p := &meterPlugin{Plugin: New(host), t: t, host: host, path: updated.path}
	t.Cleanup(p.Shutdown)
	p.configure(protocol.MethodPluginRegister, alphaCredit)
	p.configure(protocol.MethodPluginReconfigure, alphaCredit)
	p.Shutdown()
	p = &meterPlugin{Plugin: New(host), t: t, host: host, path: updated.path}
	t.Cleanup(p.Shutdown)
	p.configure(protocol.MethodPluginRegister, alphaCredit)
	if file := p.meterFile(); p.meter.Load() != nil || file.StopReason != client.MeterStopQuiesce || !file.StoppedAt.Equal(*stopped.StoppedAt) || file.Restarts != stopped.Restarts {
		t.Fatalf("with no organization: %+v, meter %v", file, p.meter.Load())
	}
	p.organizationOps(addOp("alpha", fakeOrgA))
	if p.meter.Load() == nil {
		t.Fatal("adding alpha did not start the meter")
	}
	p.Shutdown()
	file := p.meterFile()
	if n := len(file.Gaps); n == 0 || file.Gaps[n-1].Reason != client.MeterStopQuiesce || !file.Gaps[n-1].From.Equal(*stopped.StoppedAt) || file.Counted != 1 {
		t.Fatalf("after the add: %+v", file)
	}
}

// A meter that stopped itself is replaced at the next configure by one
// opened from its file, which records a failed gap.
func TestAFailedMeterIsReplacedAtTheNextConfigure(t *testing.T) {
	p := newMeterPlugin(t)
	p.writeOrganizations([2]string{"siphorchannel", fakeOrgA})
	p.configure(protocol.MethodPluginRegister, "")
	failed := p.meter.Load()
	failed.Stop(client.MeterStopFailed, time.Now().UTC())
	if !failed.Failed() {
		t.Fatal("not failed")
	}
	// A change on the page leaves it stopped: only a configure replaces it.
	p.organizationOps(addOp("after", fakeOrgC))
	if p.meter.Load() != failed {
		t.Fatal("a change on the page replaced a failed meter")
	}
	p.organizationOps(removeOp(fakeOrgC))
	p.configure(protocol.MethodPluginReconfigure, "")
	replaced := p.meter.Load()
	if replaced == nil || replaced == failed || replaced.Failed() {
		t.Fatalf("meter=%v", replaced)
	}
	p.usage(goldenRecord(t))
	p.removeAll()
	if file := p.meterFile(); len(file.Gaps) != 1 || file.Gaps[0].Reason != client.MeterStopFailed || file.Counted != 1 || file.StopReason != client.MeterStopNoItems {
		t.Fatalf("file: %+v", file)
	}
}

// A configure quota-cache rejects leaves CPA delivering it no usage records
// until one succeeds, so the meter stops with the rejection instead of
// heartbeating through a window it cannot see, and the good configure that
// follows carries on from the file with that window as a gap. Every
// rejection does it: a schedule without a unit, a mistyped key, a moved
// cache path, claude-api-credits that is not a list, and an old schema. The
// cache itself carries on.
func TestARejectedConfigureStopsTheMeterUntilTheNextGoodOne(t *testing.T) {
	p := newMeterPlugin(t)
	p.writeOrganizations([2]string{"siphorchannel", fakeOrgA})
	p.configure(protocol.MethodPluginRegister, "")
	attempt := func(schema uint32, yaml string) error {
		raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: schema, ConfigYAML: []byte(yaml)})
		_, err := p.Handle(protocol.MethodPluginReconfigure, raw)
		return err
	}
	good := "cache-path: " + p.path + "\nrequest-spacing: 1s\n"
	rejections := []struct {
		name   string
		schema uint32
		yaml   string
	}{
		{"a poll interval without a unit", 6, "cache-path: " + p.path + "\npoll-interval: \"15\"\n"},
		{"a request spacing without a unit", 6, "cache-path: " + p.path + "\nrequest-spacing: 15\n"},
		{"a mistyped key", 6, "cache-path: " + p.path + "\npol-interval: 15m\n"},
		{"claude-api-credits not a list", 6, "cache-path: " + p.path + "\nclaude-api-credits: text\n"},
		{"a moved cache path", 6, "cache-path: " + filepath.Join(t.TempDir(), "elsewhere", "snapshot.json") + "\n"},
		{"an old schema", 3, good},
	}
	for i, bad := range rejections {
		running := p.meter.Load()
		if running == nil {
			t.Fatalf("%s: no meter was running", bad.name)
		}
		p.usage(goldenRecord(t))
		if err := attempt(bad.schema, bad.yaml); err == nil {
			t.Fatalf("%s was accepted", bad.name)
		}
		if p.meter.Load() != nil || p.cache == nil {
			t.Fatalf("%s: meter running=%t cache running=%t", bad.name, p.meter.Load() != nil, p.cache != nil)
		}
		file := p.meterFile()
		if file.StopReason != client.MeterStopDisabled || file.StoppedAt == nil || file.Counted != uint64(i+1) || len(file.Gaps) != i {
			t.Fatalf("%s: file after the rejection: %+v", bad.name, file)
		}
		// CPA delivers nothing now; a record that arrived anyway is not
		// counted as if the meter had been running, and a change on the page
		// does not start it again.
		p.usage(goldenRecord(t))
		if i == 0 {
			p.organizationOps(addOp("after", fakeOrgC))
			p.organizationOps(removeOp(fakeOrgC))
			if p.meter.Load() != nil {
				t.Fatalf("%s: a change on the page restarted the meter", bad.name)
			}
		}
		if err := attempt(6, good); err != nil {
			t.Fatalf("%s: the fix was rejected: %v", bad.name, err)
		}
		if m := p.meter.Load(); m == nil || m == running {
			t.Fatalf("%s: meter after the fix: %v", bad.name, m)
		}
	}
	p.removeAll()
	file := p.meterFile()
	if n := uint64(len(rejections)); len(file.Gaps) != len(rejections) || file.Restarts != n || file.Counted != n || file.Received != n || file.StopReason != client.MeterStopNoItems {
		t.Fatalf("file after the fixes: %+v", file)
	}
	for _, gap := range file.Gaps {
		if gap.Reason != client.MeterStopDisabled || !gap.Brief() {
			t.Fatalf("gaps=%+v", file.Gaps)
		}
	}
}
