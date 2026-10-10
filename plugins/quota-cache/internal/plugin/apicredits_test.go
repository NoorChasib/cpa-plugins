package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/cache"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
	"gopkg.in/yaml.v3"
)

// The spec's fake organization ids, and an obviously fake admin key that
// must never be read. Nothing here talks to Anthropic.
const (
	fakeOrgA     = "00000000-0000-4000-8000-00000000000a"
	fakeOrgB     = "00000000-0000-4000-8000-00000000000b"
	fakeOrgC     = "00000000-0000-4000-8000-00000000000c"
	fakeAdminKey = "sk-ant-admin01-FAKEFAKEFAKEFAKE"
)

// creditsOf registers a disabled plugin with yaml and returns the items its
// configuration produced, through the real strict decoder.
func creditsOf(t *testing.T, config string) ([]creditItem, error) {
	t.Helper()
	p := New(&testHost{})
	defer p.Shutdown()
	raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("enabled: false\n" + config)})
	if _, err := p.Handle(protocol.MethodPluginRegister, raw); err != nil {
		return nil, err
	}
	return *p.apiCredits.Load(), nil
}

func mustCredits(t *testing.T, config string) []creditItem {
	t.Helper()
	items, err := creditsOf(t, config)
	if err != nil {
		t.Fatalf("configuration rejected: %v\n%s", err, config)
	}
	return items
}

// item renders one list item from the four fields, each a raw YAML value; a
// field given as "-" is left out.
func item(label, org, monthly, renews string) string {
	lines := []string{}
	for _, field := range [][2]string{{"label", label}, {"organization-id", org}, {"monthly-usd", monthly}, {"renews", renews}} {
		if field[1] != "-" {
			lines = append(lines, field[0]+": "+field[1])
		}
	}
	return "  - " + strings.Join(lines, "\n    ") + "\n"
}

const goodItem = "  - label: healthy\n    organization-id: " + fakeOrgB + "\n    monthly-usd: \"500\"\n    renews: \"2026-10-03\"\n"
const goodItem2 = "  - label: after\n    organization-id: " + fakeOrgC + "\n    monthly-usd: \"100\"\n    renews: \"2026-10-31\"\n"

func TestCreditItemsAreReadVerbatim(t *testing.T) {
	items := mustCredits(t, "claude-api-credits:\n"+
		item("siphorchannel", fakeOrgA, `"200"`, `"2026-10-29"`)+
		// Unquoted, YAML would type these; the scalar's text is what counts.
		// The organization id is stored lower-cased.
		item("unquoted", strings.ToUpper(fakeOrgB), "200", "2026-10-29")+
		// What a save from CPA's plugin panel turns them into.
		item("panel", fakeOrgC, "12.5", "2026-10-29T00:00:00Z")+
		item(`"  padded  "`, `" 12345678-1234-5678-1234-567812345678 "`, `" 0 "`, `" 2026-02-28 "`))
	want := []creditItem{
		{id: client.APICreditOrgAccount(fakeOrgA), credit: client.APICredit{Label: "siphorchannel", Position: 0, MonthlyUSD: "200", Renews: "2026-10-29", OrganizationID: fakeOrgA}},
		{id: client.APICreditOrgAccount(fakeOrgB), credit: client.APICredit{Label: "unquoted", Position: 1, MonthlyUSD: "200", Renews: "2026-10-29", OrganizationID: fakeOrgB}},
		{id: client.APICreditOrgAccount(fakeOrgC), credit: client.APICredit{Label: "panel", Position: 2, MonthlyUSD: "12.5", Renews: "2026-10-29T00:00:00Z", OrganizationID: fakeOrgC}},
		{id: client.APICreditOrgAccount("12345678-1234-5678-1234-567812345678"), credit: client.APICredit{Label: "padded", Position: 3, MonthlyUSD: "0", Renews: "2026-02-28", OrganizationID: "12345678-1234-5678-1234-567812345678"}},
	}
	if !reflect.DeepEqual(items, want) {
		t.Fatalf("items:\n%+v\nwant:\n%+v", items, want)
	}
	if id := items[0].id; !strings.HasPrefix(id, "org-") || len(id) != 16 || strings.Contains(id, "000a") {
		t.Fatalf("id=%s", id)
	}
	if linked := linkedOrganizations(items); !reflect.DeepEqual(linked, []string{fakeOrgA, fakeOrgB, fakeOrgC, "12345678-1234-5678-1234-567812345678"}) {
		t.Fatalf("linked=%v", linked)
	}
}

// Each problem is judged on its own item, which is still listed, and the
// valid item after it is still counted. The problems come in the order of
// the CreditProblem values; an organization id with a valid shape is kept
// whatever the problem.
func TestEachCreditProblemStopsOnlyItsOwnItem(t *testing.T) {
	long := strings.Repeat("x", 65)
	for _, tc := range []struct {
		name, item, problem, label, org string
	}{
		{"not a mapping", "  - just text\n", client.CreditProblemItemInvalid, "", ""},
		{"a list value", "  - label: [a, b]\n    organization-id: " + fakeOrgA + "\n", client.CreditProblemItemInvalid, "", fakeOrgA},
		{"a mapping value", "  - label: ok\n    organization-id: {a: b}\n", client.CreditProblemItemInvalid, "ok", ""},
		{"repeated key", "  - label: one\n    label: two\n    organization-id: " + fakeOrgA + "\n", client.CreditProblemItemInvalid, "one", fakeOrgA},
		{"unknown field", item("ok", fakeOrgA, `"1"`, `"2026-10-01"`) + "    monthly: 5\n", client.CreditProblemUnknownField, "ok", fakeOrgA},
		{"unknown field beats a missing label", "  - organization-id: " + fakeOrgA + "\n    note: x\n", client.CreditProblemUnknownField, "", fakeOrgA},
		{"label missing", item("-", fakeOrgA, `"1"`, `"2026-10-01"`), client.CreditProblemLabelMissing, "", fakeOrgA},
		{"label blank", item(`"   "`, fakeOrgA, `"1"`, `"2026-10-01"`), client.CreditProblemLabelMissing, "", fakeOrgA},
		{"label too long", item(long, fakeOrgA, `"1"`, `"2026-10-01"`), client.CreditProblemLabelInvalid, "", fakeOrgA},
		{"label control character", item(`"a\tb"`, fakeOrgA, `"1"`, `"2026-10-01"`), client.CreditProblemLabelInvalid, "", fakeOrgA},
		{"label duplicate ignoring case", item("HEALTHY", fakeOrgA, `"1"`, `"2026-10-01"`), client.CreditProblemLabelDuplicate, "", fakeOrgA},
		{"organization missing", item("ok", "-", `"1"`, `"2026-10-01"`), client.CreditProblemOrganizationIDMissing, "ok", ""},
		{"organization empty", item("ok", `""`, `"1"`, `"2026-10-01"`), client.CreditProblemOrganizationIDMissing, "ok", ""},
		{"organization braced", item("ok", `"{`+fakeOrgA+`}"`, `"1"`, `"2026-10-01"`), client.CreditProblemOrganizationIDInvalid, "ok", ""},
		{"organization urn", item("ok", "urn:uuid:"+fakeOrgA, `"1"`, `"2026-10-01"`), client.CreditProblemOrganizationIDInvalid, "ok", ""},
		{"organization undashed", item("ok", strings.ReplaceAll(fakeOrgA, "-", ""), `"1"`, `"2026-10-01"`), client.CreditProblemOrganizationIDInvalid, "ok", ""},
		{"organization short group", item("ok", "00000000-000-4000-8000-00000000000a", `"1"`, `"2026-10-01"`), client.CreditProblemOrganizationIDInvalid, "ok", ""},
		{"organization not hex", item("ok", "0000000g-0000-4000-8000-00000000000a", `"1"`, `"2026-10-01"`), client.CreditProblemOrganizationIDInvalid, "ok", ""},
		{"organization all zeros", item("ok", "00000000-0000-0000-0000-000000000000", `"1"`, `"2026-10-01"`), client.CreditProblemOrganizationIDInvalid, "ok", ""},
		{"organization duplicate", item("ok", strings.ToUpper(fakeOrgB), `"1"`, `"2026-10-01"`), client.CreditProblemOrganizationIDDuplicate, "ok", fakeOrgB},
		{"label before organization", item(long, "nope", "nope", "nope"), client.CreditProblemLabelInvalid, "", ""},
		{"organization before the optional values", item("ok", "nope", "nope", "nope"), client.CreditProblemOrganizationIDInvalid, "ok", ""},
	} {
		items := mustCredits(t, "claude-api-credits:\n"+goodItem+tc.item+goodItem2)
		if len(items) != 3 {
			t.Fatalf("%s: %d items", tc.name, len(items))
		}
		got := items[1]
		if got.credit.Problem != tc.problem || got.credit.Label != tc.label || got.credit.Position != 1 || got.credit.OrganizationID != tc.org {
			t.Errorf("%s: credit=%+v; want problem %q label %q organization %q", tc.name, got.credit, tc.problem, tc.label, tc.org)
		}
		// Whatever else is wrong, an item whose organization is valid and
		// first-occurring is keyed by it and linked, so fixing the rest of
		// the item loses no history; any other item is positional.
		wantID, wantLinked := "item-2", []string{fakeOrgB, fakeOrgC}
		if tc.org != "" && tc.problem != client.CreditProblemOrganizationIDDuplicate {
			wantID, wantLinked = client.APICreditOrgAccount(tc.org), []string{fakeOrgB, tc.org, fakeOrgC}
		}
		if got.id != wantID {
			t.Errorf("%s: id=%s want %s", tc.name, got.id, wantID)
		}
		for _, other := range []creditItem{items[0], items[2]} {
			if other.credit.Problem != "" {
				t.Errorf("%s: a neighbouring item caught %q", tc.name, other.credit.Problem)
			}
		}
		if linked := linkedOrganizations(items); !reflect.DeepEqual(linked, wantLinked) {
			t.Errorf("%s: linked=%v want %v", tc.name, linked, wantLinked)
		}
	}
}

// monthly-usd and renews are optional: an invalid value is ignored and
// flagged, never a problem, and the item stays counted. admin-key is
// accepted, flagged, and never read.
func TestOptionalValuesAreFlaggedNotProblems(t *testing.T) {
	for _, tc := range []struct {
		name, item string
		want       client.APICredit
	}{
		{"nothing optional", item("ok", fakeOrgA, "-", "-"), client.APICredit{Label: "ok", OrganizationID: fakeOrgA}},
		{"monthly cents", item("ok", fakeOrgA, `"12.345"`, `"2026-10-01"`), client.APICredit{Label: "ok", OrganizationID: fakeOrgA, Renews: "2026-10-01", MonthlyUSDInvalid: true}},
		{"monthly negative", item("ok", fakeOrgA, "-5", "-"), client.APICredit{Label: "ok", OrganizationID: fakeOrgA, MonthlyUSDInvalid: true}},
		{"monthly dollar sign", item("ok", fakeOrgA, `"$200"`, "-"), client.APICredit{Label: "ok", OrganizationID: fakeOrgA, MonthlyUSDInvalid: true}},
		{"monthly zero", item("ok", fakeOrgA, `"0"`, "-"), client.APICredit{Label: "ok", OrganizationID: fakeOrgA, MonthlyUSD: "0"}},
		{"renews not a date", item("ok", fakeOrgA, `"1"`, `"2026-02-30"`), client.APICredit{Label: "ok", OrganizationID: fakeOrgA, MonthlyUSD: "1", RenewsInvalid: true}},
		{"renews with a time", item("ok", fakeOrgA, "-", `"2026-10-01T05:00:00Z"`), client.APICredit{Label: "ok", OrganizationID: fakeOrgA, RenewsInvalid: true}},
		{"both invalid", item("ok", fakeOrgA, "nope", "nope"), client.APICredit{Label: "ok", OrganizationID: fakeOrgA, MonthlyUSDInvalid: true, RenewsInvalid: true}},
		{"admin key", item("ok", fakeOrgA, `"1"`, "-") + "    admin-key: " + fakeAdminKey + "\n", client.APICredit{Label: "ok", OrganizationID: fakeOrgA, MonthlyUSD: "1", AdminKeyIgnored: true}},
		{"admin key of any shape", item("ok", fakeOrgA, "-", "-") + "    admin-key: not-a-key\n", client.APICredit{Label: "ok", OrganizationID: fakeOrgA, AdminKeyIgnored: true}},
		{"admin key repeated elsewhere", item("ok", fakeOrgA, "-", "-") + "    admin-key: " + fakeAdminKey + "\n", client.APICredit{Label: "ok", OrganizationID: fakeOrgA, AdminKeyIgnored: true}},
	} {
		items := mustCredits(t, "claude-api-credits:\n"+goodItem+"    admin-key: "+fakeAdminKey+"\n"+tc.item)
		got := items[1]
		tc.want.Position = 1
		if got.credit != tc.want || got.id != client.APICreditOrgAccount(fakeOrgA) {
			t.Errorf("%s: credit=%+v id=%s; want %+v", tc.name, got.credit, got.id, tc.want)
		}
		if printed := fmt.Sprintf("%v %+v %#v", items, items, items); strings.Contains(printed, "FAKE") || strings.Contains(printed, "sk-ant") {
			t.Errorf("%s: formatting the items printed the key: %s", tc.name, printed)
		}
	}
}

// Null is absent, in every spelling YAML has for it; a quoted "null" is text.
func TestNullCreditValuesAreMissing(t *testing.T) {
	for _, null := range []string{"null", "~", "", "Null", "NULL", "!!null whatever", "!!null"} {
		for field, want := range map[string]client.APICredit{
			"label":           {OrganizationID: fakeOrgA, MonthlyUSD: "1", Renews: "2026-10-01", Problem: client.CreditProblemLabelMissing},
			"organization-id": {Label: "ok", MonthlyUSD: "1", Renews: "2026-10-01", Problem: client.CreditProblemOrganizationIDMissing},
			"monthly-usd":     {Label: "ok", OrganizationID: fakeOrgA, Renews: "2026-10-01"},
			"renews":          {Label: "ok", OrganizationID: fakeOrgA, MonthlyUSD: "1"},
		} {
			values := map[string]string{"label": "ok", "organization-id": fakeOrgA, "monthly-usd": `"1"`, "renews": `"2026-10-01"`}
			values[field] = null
			items := mustCredits(t, "claude-api-credits:\n"+item(values["label"], values["organization-id"], values["monthly-usd"], values["renews"]))
			if items[0].credit != want {
				t.Errorf("%s: %q gave %+v; want %+v", field, null, items[0].credit, want)
			}
		}
		// admin-key is flagged by its presence, null or not.
		items := mustCredits(t, "claude-api-credits:\n"+item("ok", fakeOrgA, "-", "-")+"    admin-key: "+null+"\n")
		if want := (client.APICredit{Label: "ok", OrganizationID: fakeOrgA, AdminKeyIgnored: true}); items[0].credit != want {
			t.Errorf("admin-key %q gave %+v", null, items[0].credit)
		}
	}
	items := mustCredits(t, "claude-api-credits:\n"+item(`"null"`, fakeOrgA, `"1"`, `"2026-10-01"`))
	if items[0].credit.Problem != "" || items[0].credit.Label != "null" {
		t.Fatalf("a quoted null label: %+v", items[0].credit)
	}
}

// CPA's panel save writes a JSON null as this node, whose text is "null".
func TestThePanelsNullNodeIsMissing(t *testing.T) {
	scalar := func(tag, value string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value} }
	node := yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
		scalar("!!str", "label"), scalar("!!null", "null"),
		scalar("!!str", "organization-id"), scalar("!!str", fakeOrgA),
		scalar("!!str", "monthly-usd"), scalar("!!null", "null"),
		scalar("!!str", "renews"), scalar("!!str", "2026-10-01"),
	}}
	items := parseAPICredits([]yaml.Node{node})
	if want := (client.APICredit{OrganizationID: fakeOrgA, Renews: "2026-10-01", Problem: client.CreditProblemLabelMissing}); items[0].credit != want || items[0].id != client.APICreditOrgAccount(fakeOrgA) {
		t.Fatalf("item=%+v", items[0].credit)
	}
}

// The 17th item is listed with its organization id, so a consumer can name
// it, and is not linked to the meter.
func TestCreditItemsPastTheLimitAreListedButNotLinked(t *testing.T) {
	config := "claude-api-credits:\n"
	for i := 0; i <= client.MaxAPICreditItems; i++ {
		config += item("org"+strconv.Itoa(i), fmt.Sprintf("00000000-0000-4000-8000-%012x", i+1), `"1"`, `"2026-10-01"`)
	}
	items := mustCredits(t, config)
	if len(items) != client.MaxAPICreditItems+1 {
		t.Fatalf("%d items", len(items))
	}
	for i, it := range items {
		want, id := "", client.APICreditOrgAccount(fmt.Sprintf("00000000-0000-4000-8000-%012x", i+1))
		if i == client.MaxAPICreditItems {
			want, id = client.CreditProblemTooManyItems, "item-17"
		}
		if it.credit.Problem != want || it.credit.Label != "org"+strconv.Itoa(i) || it.credit.OrganizationID == "" || it.id != id {
			t.Fatalf("item %d: %+v id=%s", i, it.credit, it.id)
		}
	}
	if linked := linkedOrganizations(items); len(linked) != client.MaxAPICreditItems || linked[0] != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("linked=%v", linked)
	}
}

func TestDuplicateOrganizationsKeepTheFirstItem(t *testing.T) {
	items := mustCredits(t, "claude-api-credits:\n"+item("one", fakeOrgA, `"1"`, "-")+item("two", strings.ToUpper(fakeOrgA), "-", "-")+item("three", fakeOrgA, "-", "-"))
	if items[0].credit.Problem != "" || items[0].id != client.APICreditOrgAccount(fakeOrgA) {
		t.Fatalf("first=%+v id=%s", items[0].credit, items[0].id)
	}
	for _, later := range items[1:] {
		if later.credit.Problem != client.CreditProblemOrganizationIDDuplicate || later.credit.OrganizationID != fakeOrgA || !strings.HasPrefix(later.id, "item-") {
			t.Fatalf("later=%+v id=%s", later.credit, later.id)
		}
	}
	if linked := linkedOrganizations(items); !reflect.DeepEqual(linked, []string{fakeOrgA}) {
		t.Fatalf("linked=%v", linked)
	}
}

func TestCreditListShapeAndOtherSettings(t *testing.T) {
	for _, bad := range []string{"claude-api-credits: {}\n", "claude-api-credits: text\n", "claude-api-credits:\n  label: x\n", "claude-api-credit:\n  - label: x\n"} {
		if _, err := creditsOf(t, bad); err == nil || err.Error() != "invalid quota-cache configuration" {
			t.Errorf("%q: err=%v; a list is required", bad, err)
		}
	}
	for _, empty := range []string{"", "claude-api-credits:\n", "claude-api-credits: []\n", "claude-api-credits: null\n"} {
		if items, err := creditsOf(t, empty); err != nil || len(items) != 0 {
			t.Errorf("%q: items=%+v err=%v", empty, items, err)
		}
	}
	// Anchors and aliases resolve.
	items := mustCredits(t, "claude-api-credits:\n  - label: a\n    organization-id: "+fakeOrgA+"\n    monthly-usd: &m \"1\"\n    renews: \"2026-10-01\"\n  - label: b\n    organization-id: "+fakeOrgB+"\n    monthly-usd: *m\n    renews: \"2026-10-01\"\n")
	if items[1].credit.Problem != "" || items[1].credit.MonthlyUSD != "1" {
		t.Fatalf("alias: %+v", items[1].credit)
	}
}

// The panel cannot describe a list of objects or check one item, so the
// list is YAML only and the panel's fields are unchanged.
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

func fetcherWithCredits(host Host, config string, t *testing.T) hostFetcher {
	t.Helper()
	items := mustCredits(t, config)
	f := fetcherWith(host, "")
	f.apiCredits = &atomic.Pointer[[]creditItem]{}
	f.apiCredits.Store(&items)
	return f
}

func TestCreditAccountsAreListedAfterTheRosterChecks(t *testing.T) {
	config := "claude-api-credits:\n" + item("siphorchannel", fakeOrgA, `"200"`, `"2026-10-29"`) + item("-", "nope", `"1"`, `"2026-10-01"`)
	indexed := diskEntry("claude-one.json", "claude")
	indexed.AuthIndex = "one"
	accounts, err := fetcherWithCredits(rosterHost{[]protocol.HostAuthFileEntry{indexed}}, config, t).List(context.Background())
	if err != nil || len(accounts) != 3 {
		t.Fatalf("accounts=%+v err=%v", accounts, err)
	}
	if accounts[0].Provider != "claude" || accounts[0].Credit != nil {
		t.Fatalf("first=%+v", accounts[0])
	}
	first, second := accounts[1], accounts[2]
	if first.Provider != client.ProviderAnthropicAPI || first.AuthIndex != client.APICreditOrgAccount(fakeOrgA) || first.Credit == nil || first.Credit.Label != "siphorchannel" {
		t.Fatalf("credit account=%+v", first)
	}
	if second.AuthIndex != "item-2" || second.Credit.Problem != client.CreditProblemLabelMissing || second.Credit.OrganizationID != "" {
		t.Fatalf("misconfigured account=%+v credit=%+v", second, second.Credit)
	}
	// Credit accounts are no reason to stop waiting for CPA's roster.
	accounts, err = fetcherWithCredits(rosterHost{[]protocol.HostAuthFileEntry{diskEntry("claude-one.json", "claude")}}, config, t).List(context.Background())
	if !errors.Is(err, cache.ErrRosterNotReady) || accounts != nil {
		t.Fatalf("accounts=%+v err=%v", accounts, err)
	}
	// Each listing hands out its own copy of the configured facts.
	f := fetcherWithCredits(rosterHost{}, config, t)
	accounts, _ = f.List(context.Background())
	accounts[0].Credit.Label = "changed"
	if again, _ := f.List(context.Background()); again[0].Credit.Label != "siphorchannel" {
		t.Fatal("a listing shares its credit with the configuration")
	}
}

// The cache never asks for a credit account; should it, the fetcher refuses
// without a request. testHost panics on any credential read or HTTP call.
func TestCreditFetchNeverCallsTheHost(t *testing.T) {
	f := fetcherWithCredits(&testHost{}, "claude-api-credits:\n"+item("siphorchannel", fakeOrgA, `"200"`, `"2026-10-29"`), t)
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

const oneCredit = "claude-api-credits:\n  - label: siphorchannel\n    organization-id: " + fakeOrgA + "\n    monthly-usd: \"200\"\n    renews: \"2026-10-29\"\n"

// The capability follows the items: usage records are declared wanted only
// while the cache runs and at least one item is configured, on register
// and on every reconfigure. With no items the meter stops with no_items and
// its file stays; disabled stops it with disabled. Routes are unchanged.
func TestUsagePluginIsDeclaredWhileItemsAreConfigured(t *testing.T) {
	p := newMeterPlugin(t)
	if reg := p.configure(protocol.MethodPluginRegister, ""); reg.Capabilities.UsagePlugin || !reg.Capabilities.ManagementAPI || p.meter.Load() != nil {
		t.Fatalf("registration=%+v meter=%v", reg.Capabilities, p.meter.Load())
	}
	if _, err := os.Lstat(client.MeterPath(p.path)); !os.IsNotExist(err) {
		t.Fatal("a meter file exists with no items")
	}
	reg := p.configure(protocol.MethodPluginReconfigure, oneCredit)
	m := p.meter.Load()
	if !reg.Capabilities.UsagePlugin || m == nil {
		t.Fatalf("registration=%+v meter=%v", reg.Capabilities, m)
	}
	p.usage(goldenRecord(t))
	// The same items again keep the meter; so does a schedule change.
	p.configure(protocol.MethodPluginReconfigure, oneCredit)
	p.configure(protocol.MethodPluginReconfigure, "poll-interval: 30m\n"+oneCredit)
	if p.meter.Load() != m {
		t.Fatal("a reconfigure replaced the meter")
	}
	if reg := p.configure(protocol.MethodPluginReconfigure, ""); reg.Capabilities.UsagePlugin || p.meter.Load() != nil {
		t.Fatalf("registration=%+v meter=%v", reg.Capabilities, p.meter.Load())
	}
	file := p.meterFile()
	if file.StopReason != client.MeterStopNoItems || file.StoppedAt == nil || file.Counted != 1 || file.Received != 1 {
		t.Fatalf("file after no_items: %+v", file)
	}
	// Items again: the meter carries on from the file, with a gap.
	if reg := p.configure(protocol.MethodPluginReconfigure, oneCredit); !reg.Capabilities.UsagePlugin || p.meter.Load() == nil {
		t.Fatalf("registration=%+v", reg.Capabilities)
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
	if len(registration.Routes) != 1 || registration.Routes[0].Path != "/plugins/quota-cache/status" || len(registration.Resources) != 1 || registration.Resources[0].Path != "/status" {
		t.Fatalf("routes=%+v", registration)
	}
}

// End to end through configure, the real writer and the meter: a record CPA
// sends is counted for its organization, an OAuth record never is, and
// nothing the record carries beyond what the meter keeps reaches the meter
// file, the snapshot, the status route or the log. admin-key is flagged,
// warned about once per configure, and never read.
func TestUsageRecordsAreMeteredAndSecretsNeverEscape(t *testing.T) {
	p := newMeterPlugin(t)
	p.configure(protocol.MethodPluginRegister, oneCredit+"    admin-key: "+fakeAdminKey+"\n"+item("-", "nope", `"5"`, `"2026-10-01"`)+"    admin-key: "+fakeAdminKey+"\n")
	good := client.Key(client.ProviderAnthropicAPI, client.APICreditOrgAccount(fakeOrgA))
	broken := client.Key(client.ProviderAnthropicAPI, "item-2")
	snapshot := waitFor(t, p.path, func(s client.Snapshot) bool {
		_, ok := s.Entries[good]
		_, seen := s.Entries[broken]
		return ok && seen
	})
	if entry := snapshot.Entries[good]; entry.APICredit == nil || !entry.APICredit.AdminKeyIgnored || entry.APICredit.KeyFingerprint != "" || entry.APICredit.OrganizationID != fakeOrgA || !entry.LastAttempt.IsZero() {
		t.Fatalf("entry=%+v", entry.APICredit)
	}
	if entry := snapshot.Entries[broken]; entry.APICredit == nil || entry.APICredit.Problem != client.CreditProblemLabelMissing || !entry.APICredit.AdminKeyIgnored {
		t.Fatalf("misconfigured entry=%+v", entry.APICredit)
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
	// The configuration is unchanged, so no scan rewrites it either.
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
	if code != 200 || !strings.Contains(status, `"api_meter":{"schema":1,`) || !strings.Contains(status, `"admin_key_ignored":true`) {
		t.Fatalf("status %d: %s", code, status)
	}
	rawMeter, _ := os.ReadFile(client.MeterPath(p.path))
	lines := p.host.logged()
	items := *p.apiCredits.Load()
	for name, text := range map[string]string{
		"snapshot": string(before), "meter": string(rawMeter), "status": status,
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
	warnings := []logLine{}
	for _, line := range lines {
		if strings.Contains(line.message, "admin-key") {
			warnings = append(warnings, line)
		}
	}
	// Once for the register that had two such items, none for the
	// reconfigure that had none.
	if len(warnings) != 1 || warnings[0].level != "warn" || warnings[0].message != "quota-cache no longer uses admin-key in claude-api-credits; delete it from the configuration" || warnings[0].fields["items"] != 2 {
		t.Fatalf("warnings=%+v", warnings)
	}
	for _, line := range lines {
		if line.level == "error" {
			t.Fatalf("logged an error: %+v", line)
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
	p.configure(protocol.MethodPluginReconfigure, oneCredit)
	p.configure(protocol.MethodPluginReconfigure, "")
	rawMeter, err := os.ReadFile(client.MeterPath(p.path))
	if err != nil {
		t.Fatal(err)
	}
	if code, status := p.status(); code != 200 || !strings.Contains(status, `"api_meter":{"schema":1,`) || !strings.Contains(status, `"stop_reason":"no_items"`) {
		t.Fatalf("status %d: %s", code, status)
	}
	// A configuration change makes the writer save the snapshot, while the
	// meter it restarts has not saved yet; the meter file is as it was.
	p.configure(protocol.MethodPluginReconfigure, oneCredit+item("-", "nope", `"5"`, `"2026-10-01"`))
	waitFor(t, p.path, func(s client.Snapshot) bool {
		_, ok := s.Entries[client.Key(client.ProviderAnthropicAPI, "item-2")]
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
	p.configure(protocol.MethodPluginRegister, oneCredit)
	p.usage(goldenRecord(t))
	if _, err := p.Handle(protocol.MethodPluginQuiesce, nil); err != nil {
		t.Fatal(err)
	}
	if file := p.meterFile(); file.StopReason != client.MeterStopQuiesce || file.Counted != 1 || p.meter.Load() != nil {
		t.Fatalf("after quiesce: %+v meter=%v", file, p.meter.Load())
	}
	p.configure(protocol.MethodPluginRegister, oneCredit)
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

// A meter that stopped itself is replaced at the next configure by one
// opened from its file, which records a failed gap.
func TestAFailedMeterIsReplacedAtTheNextConfigure(t *testing.T) {
	p := newMeterPlugin(t)
	p.configure(protocol.MethodPluginRegister, oneCredit)
	failed := p.meter.Load()
	failed.Stop(client.MeterStopFailed, time.Now().UTC())
	if !failed.Failed() {
		t.Fatal("not failed")
	}
	p.configure(protocol.MethodPluginReconfigure, oneCredit)
	replaced := p.meter.Load()
	if replaced == nil || replaced == failed || replaced.Failed() {
		t.Fatalf("meter=%v", replaced)
	}
	p.usage(goldenRecord(t))
	p.configure(protocol.MethodPluginReconfigure, "")
	if file := p.meterFile(); len(file.Gaps) != 1 || file.Gaps[0].Reason != client.MeterStopFailed || file.Counted != 1 || file.StopReason != client.MeterStopNoItems {
		t.Fatalf("file: %+v", file)
	}
}

// A configure quota-cache rejects leaves CPA delivering it no usage records
// until one succeeds, so the meter stops with the rejection instead of
// heartbeating through a window it cannot see, and the good configure that
// follows carries on from the file with that window as a gap. Every
// rejection does it: a schedule without a unit, a mistyped key, a moved
// cache path and an old schema. The cache itself carries on.
func TestARejectedConfigureStopsTheMeterUntilTheNextGoodOne(t *testing.T) {
	p := newMeterPlugin(t)
	p.configure(protocol.MethodPluginRegister, oneCredit)
	attempt := func(schema uint32, yaml string) error {
		raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: schema, ConfigYAML: []byte(yaml)})
		_, err := p.Handle(protocol.MethodPluginReconfigure, raw)
		return err
	}
	good := "cache-path: " + p.path + "\nrequest-spacing: 1s\n" + oneCredit
	rejections := []struct {
		name   string
		schema uint32
		yaml   string
	}{
		{"a poll interval without a unit", 6, "cache-path: " + p.path + "\npoll-interval: \"15\"\n" + oneCredit},
		{"a request spacing without a unit", 6, "cache-path: " + p.path + "\nrequest-spacing: 15\n" + oneCredit},
		{"a mistyped key", 6, "cache-path: " + p.path + "\npol-interval: 15m\n" + oneCredit},
		{"a moved cache path", 6, "cache-path: " + filepath.Join(t.TempDir(), "elsewhere", "snapshot.json") + "\n" + oneCredit},
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
		// counted as if the meter had been running.
		p.usage(goldenRecord(t))
		if err := attempt(6, good); err != nil {
			t.Fatalf("%s: the fix was rejected: %v", bad.name, err)
		}
		if m := p.meter.Load(); m == nil || m == running {
			t.Fatalf("%s: meter after the fix: %v", bad.name, m)
		}
	}
	p.configure(protocol.MethodPluginReconfigure, "")
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
