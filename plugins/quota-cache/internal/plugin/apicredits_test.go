package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
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

// Obviously fake keys. Nothing here talks to Anthropic.
const (
	fakeAdminKey  = "sk-ant-admin01-FAKEFAKEFAKEFAKE"
	fakeOtherKey  = "sk-ant-admin01-FAKEOTHERFAKEOTHER"
	fakeAPIKey    = "sk-ant-api03-FAKEFAKEFAKEFAKE_-"
	fakeOrgHeader = "5b1e3c9a-0d1f-4c8e-9a51-7d0c2b6e4f10"
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
func item(label, key, monthly, renews string) string {
	lines := []string{}
	for _, field := range [][2]string{{"label", label}, {"admin-key", key}, {"monthly-usd", monthly}, {"renews", renews}} {
		if field[1] != "-" {
			lines = append(lines, field[0]+": "+field[1])
		}
	}
	return "  - " + strings.Join(lines, "\n    ") + "\n"
}

const goodItem = "  - label: healthy\n    admin-key: " + fakeOtherKey + "\n    monthly-usd: \"500\"\n    renews: \"2026-10-03\"\n"

func TestCreditItemsAreReadVerbatim(t *testing.T) {
	items := mustCredits(t, "claude-api-credits:\n"+
		item("siphorchannel", fakeAdminKey, `"200"`, `"2026-10-29"`)+
		// Unquoted, YAML would type these; the scalar's text is what counts.
		item("unquoted", fakeOtherKey, "200", "2026-10-29")+
		// What a save from CPA's plugin panel turns them into.
		item("panel", fakeAPIKey, "12.5", "2026-10-29T00:00:00Z")+
		item(`"  padded  "`, "sk-ant-admin01-PADDEDPADDED", `" 0 "`, `" 2026-02-28 "`))
	want := []creditItem{
		{id: client.APICreditAccount("siphorchannel"), key: fakeAdminKey, credit: client.APICredit{Label: "siphorchannel", Position: 0, MonthlyUSD: "200", Renews: "2026-10-29", KeyFingerprint: creditKeyFingerprint(fakeAdminKey)}},
		{id: client.APICreditAccount("unquoted"), key: fakeOtherKey, credit: client.APICredit{Label: "unquoted", Position: 1, MonthlyUSD: "200", Renews: "2026-10-29", KeyFingerprint: creditKeyFingerprint(fakeOtherKey)}},
		{id: client.APICreditAccount("panel"), key: fakeAPIKey, credit: client.APICredit{Label: "panel", Position: 2, MonthlyUSD: "12.5", Renews: "2026-10-29T00:00:00Z", KeyFingerprint: creditKeyFingerprint(fakeAPIKey)}},
		{id: client.APICreditAccount("padded"), key: "sk-ant-admin01-PADDEDPADDED", credit: client.APICredit{Label: "padded", Position: 3, MonthlyUSD: "0", Renews: "2026-02-28", KeyFingerprint: creditKeyFingerprint("sk-ant-admin01-PADDEDPADDED")}},
	}
	if !reflect.DeepEqual(items, want) {
		t.Fatalf("items:\n%+v\nwant:\n%+v", items, want)
	}
	if id := items[0].id; id != "label-9611d9ba844a" {
		t.Fatalf("id=%s; the spec's fixture id for siphorchannel is label-9611d9ba844a", id)
	}
	if fp := items[0].credit.KeyFingerprint; !strings.HasPrefix(fp, "key-") || len(fp) != 16 || fp != openRouterAccount(fakeAdminKey) {
		t.Fatalf("fingerprint=%q", fp)
	}
}

// Each problem is judged on its own item, which is still listed, and the
// valid item after it is still pollable.
func TestEachCreditProblemStopsOnlyItsOwnItem(t *testing.T) {
	long := strings.Repeat("x", 65)
	for _, tc := range []struct {
		name, item, problem, label string
	}{
		{"not a mapping", "  - just text\n", client.CreditProblemItemInvalid, ""},
		{"a list value", "  - label: [a, b]\n    admin-key: " + fakeAdminKey + "\n    monthly-usd: \"1\"\n    renews: \"2026-10-01\"\n", client.CreditProblemItemInvalid, ""},
		{"a mapping value", "  - label: ok\n    admin-key: {a: b}\n    monthly-usd: \"1\"\n    renews: \"2026-10-01\"\n", client.CreditProblemItemInvalid, "ok"},
		{"repeated key", "  - label: one\n    label: two\n    admin-key: " + fakeAdminKey + "\n    monthly-usd: \"1\"\n    renews: \"2026-10-01\"\n", client.CreditProblemItemInvalid, "one"},
		{"unknown field", item("ok", fakeAdminKey, `"1"`, `"2026-10-01"`) + "    monthly: 5\n", client.CreditProblemUnknownField, "ok"},
		{"unknown field beats a missing label", "  - admin-key: " + fakeAdminKey + "\n    note: x\n", client.CreditProblemUnknownField, ""},
		{"label missing", item("-", fakeAdminKey, `"1"`, `"2026-10-01"`), client.CreditProblemLabelMissing, ""},
		{"label blank", item(`"   "`, fakeAdminKey, `"1"`, `"2026-10-01"`), client.CreditProblemLabelMissing, ""},
		{"label too long", item(long, fakeAdminKey, `"1"`, `"2026-10-01"`), client.CreditProblemLabelInvalid, ""},
		{"label control character", item(`"a\tb"`, fakeAdminKey, `"1"`, `"2026-10-01"`), client.CreditProblemLabelInvalid, ""},
		{"label duplicate ignoring case", item("HEALTHY", fakeAdminKey, `"1"`, `"2026-10-01"`), client.CreditProblemLabelDuplicate, ""},
		{"key missing", item("ok", "-", `"1"`, `"2026-10-01"`), client.CreditProblemAdminKeyMissing, "ok"},
		{"key not anthropic", item("ok", "sk-proj-FAKEFAKEFAKEFAKE", `"1"`, `"2026-10-01"`), client.CreditProblemAdminKeyInvalid, "ok"},
		{"key bare token", item("ok", "FAKEFAKEFAKEFAKE", `"1"`, `"2026-10-01"`), client.CreditProblemAdminKeyInvalid, "ok"},
		{"key with a space", item("ok", `"sk-ant-admin01-FAKE FAKEFAKE"`, `"1"`, `"2026-10-01"`), client.CreditProblemAdminKeyInvalid, "ok"},
		{"key too short", item("ok", "sk-ant-short", `"1"`, `"2026-10-01"`), client.CreditProblemAdminKeyInvalid, "ok"},
		{"key too long", item("ok", "sk-ant-"+strings.Repeat("A", 251), `"1"`, `"2026-10-01"`), client.CreditProblemAdminKeyInvalid, "ok"},
		{"key repeated", item("ok", fakeOtherKey, `"1"`, `"2026-10-01"`), client.CreditProblemAdminKeyRepeated, "ok"},
		{"monthly missing", item("ok", fakeAdminKey, "-", `"2026-10-01"`), client.CreditProblemMonthlyMissing, "ok"},
		{"monthly cents", item("ok", fakeAdminKey, `"12.345"`, `"2026-10-01"`), client.CreditProblemMonthlyInvalid, "ok"},
		{"monthly negative", item("ok", fakeAdminKey, "-5", `"2026-10-01"`), client.CreditProblemMonthlyInvalid, "ok"},
		{"monthly dollar sign", item("ok", fakeAdminKey, `"$200"`, `"2026-10-01"`), client.CreditProblemMonthlyInvalid, "ok"},
		{"renews missing", item("ok", fakeAdminKey, `"1"`, "-"), client.CreditProblemRenewsMissing, "ok"},
		{"renews not a date", item("ok", fakeAdminKey, `"1"`, `"2026-02-30"`), client.CreditProblemRenewsInvalid, "ok"},
		{"renews with a time", item("ok", fakeAdminKey, `"1"`, `"2026-10-01T05:00:00Z"`), client.CreditProblemRenewsInvalid, "ok"},
		{"label first", item(long, "nope", "nope", "nope"), client.CreditProblemLabelInvalid, ""},
		{"key before amount", item("ok", "nope", "nope", "nope"), client.CreditProblemAdminKeyInvalid, "ok"},
		{"amount before date", item("ok", fakeAdminKey, "nope", "nope"), client.CreditProblemMonthlyInvalid, "ok"},
	} {
		items := mustCredits(t, "claude-api-credits:\n"+goodItem+tc.item+goodItem2)
		if len(items) != 3 {
			t.Fatalf("%s: %d items", tc.name, len(items))
		}
		got := items[1]
		if got.credit.Problem != tc.problem || got.credit.Label != tc.label || got.credit.Position != 1 {
			t.Errorf("%s: credit=%+v; want problem %q label %q", tc.name, got.credit, tc.problem, tc.label)
		}
		if wantID := "item-2"; tc.label != "" {
			if got.id != client.APICreditAccount(tc.label) {
				t.Errorf("%s: id=%s", tc.name, got.id)
			}
		} else if got.id != wantID {
			t.Errorf("%s: id=%s want %s", tc.name, got.id, wantID)
		}
		for _, other := range []creditItem{items[0], items[2]} {
			if other.credit.Problem != "" {
				t.Errorf("%s: a neighbouring item caught %q", tc.name, other.credit.Problem)
			}
		}
	}
}

const goodItem2 = "  - label: after\n    admin-key: sk-ant-admin01-FAKEAFTERFAKEAFTER\n    monthly-usd: \"100\"\n    renews: \"2026-10-31\"\n"

// The valid fields of a broken item are still filled in.
func TestABrokenCreditItemKeepsItsValidFields(t *testing.T) {
	items := mustCredits(t, "claude-api-credits:\n"+item("ok", fakeAdminKey, `"260.50"`, `"2026-10-31"`)+"    extra: 1\n")
	want := client.APICredit{Label: "ok", MonthlyUSD: "260.50", Renews: "2026-10-31", KeyFingerprint: creditKeyFingerprint(fakeAdminKey), Problem: client.CreditProblemUnknownField}
	if items[0].credit != want {
		t.Fatalf("credit=%+v want %+v", items[0].credit, want)
	}
}

// Null is absent, in every spelling YAML has for it; a quoted "null" is text.
func TestNullCreditValuesAreMissing(t *testing.T) {
	for _, null := range []string{"null", "~", "", "Null", "NULL", "!!null whatever", "!!null"} {
		for field, problem := range map[string]string{
			"label": client.CreditProblemLabelMissing, "admin-key": client.CreditProblemAdminKeyMissing,
			"monthly-usd": client.CreditProblemMonthlyMissing, "renews": client.CreditProblemRenewsMissing,
		} {
			values := map[string]string{"label": "ok", "admin-key": fakeAdminKey, "monthly-usd": `"1"`, "renews": `"2026-10-01"`}
			values[field] = null
			items := mustCredits(t, "claude-api-credits:\n"+item(values["label"], values["admin-key"], values["monthly-usd"], values["renews"]))
			if items[0].credit.Problem != problem {
				t.Errorf("%s: %q gave %q; want %q", field, null, items[0].credit.Problem, problem)
			}
		}
	}
	items := mustCredits(t, "claude-api-credits:\n"+item(`"null"`, fakeAdminKey, `"1"`, `"2026-10-01"`))
	if items[0].credit.Problem != "" || items[0].credit.Label != "null" {
		t.Fatalf("a quoted null label: %+v", items[0].credit)
	}
}

// CPA's panel save writes a JSON null as this node, whose text is "null".
func TestThePanelsNullNodeIsMissing(t *testing.T) {
	scalar := func(tag, value string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value} }
	node := yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
		scalar("!!str", "label"), scalar("!!null", "null"),
		scalar("!!str", "admin-key"), scalar("!!str", fakeAdminKey),
		scalar("!!str", "monthly-usd"), scalar("!!str", "1"),
		scalar("!!str", "renews"), scalar("!!str", "2026-10-01"),
	}}
	items := parseAPICredits([]yaml.Node{node})
	if items[0].credit.Problem != client.CreditProblemLabelMissing || items[0].credit.Label != "" || items[0].id != "item-1" {
		t.Fatalf("item=%+v", items[0].credit)
	}
}

func TestCreditItemsPastTheLimitAreListedButNotPolled(t *testing.T) {
	config := "claude-api-credits:\n"
	for i := 0; i <= client.MaxAPICreditItems; i++ {
		config += item("org"+strconv.Itoa(i), fmt.Sprintf("sk-ant-admin01-FAKEFAKE%04d", i), `"1"`, `"2026-10-01"`)
	}
	items := mustCredits(t, config)
	if len(items) != client.MaxAPICreditItems+1 {
		t.Fatalf("%d items", len(items))
	}
	for i, it := range items {
		want := ""
		if i == client.MaxAPICreditItems {
			want = client.CreditProblemTooManyItems
		}
		if it.credit.Problem != want || it.credit.Label != "org"+strconv.Itoa(i) {
			t.Fatalf("item %d: %+v", i, it.credit)
		}
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
	items := mustCredits(t, "claude-api-credits:\n  - label: a\n    admin-key: &k "+fakeAdminKey+"\n    monthly-usd: &m \"1\"\n    renews: \"2026-10-01\"\n  - label: b\n    admin-key: "+fakeOtherKey+"\n    monthly-usd: *m\n    renews: \"2026-10-01\"\n")
	if items[1].credit.Problem != "" || items[1].credit.MonthlyUSD != "1" {
		t.Fatalf("alias: %+v", items[1].credit)
	}
}

// The panel cannot describe a list of objects, mark a value secret, or check
// one item, so the list is YAML only and the panel's fields are unchanged.
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
	config := "claude-api-credits:\n" + item("siphorchannel", fakeAdminKey, `"200"`, `"2026-10-29"`) + item("-", fakeOtherKey, `"1"`, `"2026-10-01"`)
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
	if first.Provider != client.ProviderAnthropicAPI || first.AuthIndex != "label-9611d9ba844a" || first.Credit == nil || first.Credit.Label != "siphorchannel" {
		t.Fatalf("credit account=%+v", first)
	}
	if second.AuthIndex != "item-2" || second.Credit.Problem != client.CreditProblemLabelMissing {
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

// creditHost serves Anthropic's cost report for whatever window is asked, from
// a scripted status, and records every request and log call.
type creditHost struct {
	mu         sync.Mutex
	status     int
	retryAfter string
	body       string
	requests   []protocol.HostHTTPRequest
	logs       []string
	failHTTP   error
}

func (*creditHost) ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error) { return nil, nil }
func (*creditHost) GetAuth(context.Context, string) ([]byte, error) {
	panic("a credit account must not read a CPA credential")
}
func (h *creditHost) Log(_ context.Context, level, message string, fields map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.logs = append(h.logs, fmt.Sprint(level, message, fields))
}
func (h *creditHost) HTTPDo(_ context.Context, req protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.requests = append(h.requests, req)
	if h.failHTTP != nil {
		return protocol.HostHTTPResponse{}, h.failHTTP
	}
	headers := map[string][]string{"Anthropic-Organization-Id": {fakeOrgHeader}}
	if h.retryAfter != "" {
		headers["Retry-After"] = []string{h.retryAfter}
	}
	if h.status != 0 && h.status != 200 {
		// Error bodies name the key; nothing of them may survive.
		return protocol.HostHTTPResponse{StatusCode: h.status, Headers: headers, Body: []byte(`{"type":"error","error":{"type":"authentication_error","message":"bad key ` + fakeAdminKey + `"},"request_id":"req_FAKEFAKE"}`)}, nil
	}
	if h.body != "" {
		return protocol.HostHTTPResponse{StatusCode: 200, Headers: headers, Body: []byte(h.body)}, nil
	}
	raw, _ := json.Marshal(map[string]any{"data": costReportDays(req.URL), "has_more": false, "next_page": nil})
	return protocol.HostHTTPResponse{StatusCode: 200, Headers: headers, Body: raw}, nil
}

// costReportDays is one bucket for every day of the window a cost report
// request asks for, each charged $12.505.
func costReportDays(rawURL string) []map[string]any {
	parsed, _ := url.Parse(rawURL)
	start, _ := time.Parse(time.RFC3339, parsed.Query().Get("starting_at"))
	end, _ := time.Parse(time.RFC3339, parsed.Query().Get("ending_at"))
	data := []map[string]any{}
	for day := start; day.Before(end); day = day.Add(24 * time.Hour) {
		data = append(data, map[string]any{"starting_at": day.Format(time.RFC3339), "ending_at": day.Add(24 * time.Hour).Format(time.RFC3339),
			"results": []map[string]any{{"amount": "1250.5", "currency": "USD"}}})
	}
	return data
}

func (h *creditHost) sent() []protocol.HostHTTPRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]protocol.HostHTTPRequest(nil), h.requests...)
}

const oneCredit = "claude-api-credits:\n  - label: siphorchannel\n    admin-key: " + fakeAdminKey + "\n    monthly-usd: \"200\"\n    renews: \"2026-10-29\"\n"

func TestCreditFetchReadsTheCostReportHonestly(t *testing.T) {
	host := &creditHost{}
	f := fetcherWithCredits(host, oneCredit, t)
	accounts, _ := f.List(context.Background())
	observation, err := f.Fetch(context.Background(), accounts[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	sent := host.sent()
	if len(sent) != 1 || !strings.HasPrefix(sent[0].URL, "https://api.anthropic.com/v1/organizations/cost_report?starting_at=") ||
		sent[0].Headers["X-Api-Key"][0] != fakeAdminKey || sent[0].Headers["User-Agent"][0] != "cpa-plugins-quota-cache/"+Version+" (https://github.com/NoorChasib/cpa-plugins)" {
		t.Fatalf("sent %s", describeRequests(sent))
	}
	r := observation.Quota.CostReport
	if !observation.RequestSent || observation.HTTPStatus != 200 || r.OrganizationID != fakeOrgHeader || r.KeyFingerprint != accounts[0].Credit.KeyFingerprint || len(r.Days) == 0 {
		t.Fatalf("observation=%+v report=%+v", observation, r)
	}
}

func TestCreditFailuresAreNamedAndNeverEchoAnthropic(t *testing.T) {
	for _, tc := range []struct {
		name    string
		host    *creditHost
		message string
	}{
		{"401", &creditHost{status: 401}, client.CreditErrorKeyRejected},
		{"403", &creditHost{status: 403}, client.CreditErrorForbidden},
		{"404", &creditHost{status: 404}, client.CreditErrorUnavailable},
		{"400", &creditHost{status: 400}, client.CreditErrorRefused},
		{"413", &creditHost{status: 413}, client.CreditErrorRefused},
		{"500", &creditHost{status: 500}, client.CreditErrorUpstream},
		{"529", &creditHost{status: 529}, client.CreditErrorUpstream},
		{"bad body", &creditHost{body: `{"data":"` + fakeAdminKey + `"}`}, client.CreditErrorResponse},
		{"transport", &creditHost{failHTTP: errors.New("dial tcp: " + fakeAdminKey)}, "quota fetch failed"},
	} {
		f := fetcherWithCredits(tc.host, oneCredit, t)
		accounts, _ := f.List(context.Background())
		observation, err := f.Fetch(context.Background(), accounts[0], nil)
		if err == nil || err.Error() != tc.message || !observation.RequestSent {
			t.Fatalf("%s: err=%v observation=%+v", tc.name, err, observation)
		}
		var limited cache.RateLimited
		if errors.As(err, &limited) {
			t.Fatalf("%s: treated as a rate limit", tc.name)
		}
	}
}

// scriptedCreditHost is creditHost with the poll of the organization whose
// Admin API key is key scripted request by request. Its cost report comes in
// two pages; firstPage, secondPage and me refuse the first page, the second
// or /v1/organizations/me with that status. The report lacks the
// organization header when askMe or me is set, so the poll falls back to /me.
// A refusal carries Retry-After when refusalRetryWait is set, and a body that
// names the key. An unscripted first page, and every request made with any
// other key, is creditHost's, with its status, body and failHTTP. Log lines
// are kept whole.
type scriptedCreditHost struct {
	creditHost
	key                       string
	firstPage, secondPage, me int
	askMe                     bool
	refusalRetryWait          string
	lines                     []logLine
}

func (h *scriptedCreditHost) Log(_ context.Context, level, message string, fields map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lines = append(h.lines, logLine{level, message, fields})
}

func (h *scriptedCreditHost) logged() []logLine {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]logLine(nil), h.lines...)
}

func (h *scriptedCreditHost) HTTPDo(ctx context.Context, req protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	if req.Headers["X-Api-Key"][0] != h.key {
		return h.creditHost.HTTPDo(ctx, req)
	}
	paged, me := strings.Contains(req.URL, "&page="), strings.HasSuffix(req.URL, "/v1/organizations/me")
	if !paged && !me && h.firstPage == 0 {
		response, err := h.creditHost.HTTPDo(ctx, req)
		if err != nil || response.StatusCode != 200 || h.body != "" {
			return response, err
		}
		data := costReportDays(req.URL)
		response.Body, _ = json.Marshal(map[string]any{"data": data[:len(data)/2], "has_more": true, "next_page": "page_2"})
		if h.askMe || h.me != 0 {
			delete(response.Headers, "Anthropic-Organization-Id")
		}
		return response, nil
	}
	h.mu.Lock()
	h.requests = append(h.requests, req)
	h.mu.Unlock()
	refuse := func(status int) (protocol.HostHTTPResponse, error) {
		headers := map[string][]string{}
		if h.refusalRetryWait != "" {
			headers["Retry-After"] = []string{h.refusalRetryWait}
		}
		return protocol.HostHTTPResponse{StatusCode: status, Headers: headers, Body: []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"key ` + h.key + `"},"request_id":"req_FAKEFAKE"}`)}, nil
	}
	switch {
	case me && h.me != 0:
		return refuse(h.me)
	case me:
		return protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"id":"org_scripted","type":"organization","name":"Scripted"}`)}, nil
	case !paged:
		return refuse(h.firstPage)
	case h.secondPage != 0:
		return refuse(h.secondPage)
	}
	data := costReportDays(req.URL)
	raw, _ := json.Marshal(map[string]any{"data": data[len(data)/2:], "has_more": false, "next_page": nil})
	return protocol.HostHTTPResponse{StatusCode: 200, Headers: map[string][]string{"Anthropic-Organization-Id": {fakeOrgHeader}}, Body: raw}, nil
}

// Every request of a credit poll is part of the one reading: a cost-report
// page or the /me fallback that is refused fails the poll, its status is the
// poll's, and a 429 on any of them is a rate limit with that response's
// Retry-After. 0.1.12 judges every other poll by its first request alone.
func TestEveryCreditRequestCountsForThePoll(t *testing.T) {
	for _, tc := range []struct {
		name           string
		host           *scriptedCreditHost
		status, sent   int
		limited        bool
		retryAfter     time.Duration
		message        string
		organizationID string
	}{
		{name: "two pages read", host: &scriptedCreditHost{}, status: 200, sent: 2, organizationID: fakeOrgHeader},
		{name: "/me read", host: &scriptedCreditHost{askMe: true}, status: 200, sent: 3, organizationID: "org_scripted"},
		{name: "429 on page 2", host: &scriptedCreditHost{secondPage: 429, refusalRetryWait: "3600"}, status: 429, sent: 2, limited: true, retryAfter: time.Hour},
		{name: "429 on page 2 without Retry-After", host: &scriptedCreditHost{secondPage: 429}, status: 429, sent: 2, limited: true},
		{name: "429 on /me", host: &scriptedCreditHost{me: 429, refusalRetryWait: "1800"}, status: 429, sent: 3, limited: true, retryAfter: 30 * time.Minute},
		{name: "401 on /me", host: &scriptedCreditHost{me: 401}, status: 401, sent: 3, message: client.CreditErrorKeyRejected},
		{name: "401 on page 2", host: &scriptedCreditHost{secondPage: 401}, status: 401, sent: 2, message: client.CreditErrorKeyRejected},
		{name: "500 on page 2", host: &scriptedCreditHost{secondPage: 500}, status: 500, sent: 2, message: client.CreditErrorUpstream},
		{name: "429 on page 1", host: &scriptedCreditHost{firstPage: 429, refusalRetryWait: "60"}, status: 429, sent: 1, limited: true, retryAfter: time.Minute},
		{name: "401 on page 1", host: &scriptedCreditHost{firstPage: 401}, status: 401, sent: 1, message: client.CreditErrorKeyRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.host.key = fakeAdminKey
			f := fetcherWithCredits(tc.host, oneCredit, t)
			checkCreditPoll(t, f, tc.host, tc.status, tc.sent, tc.limited, tc.retryAfter, tc.message, tc.organizationID)
		})
	}
}

func checkCreditPoll(t *testing.T, f hostFetcher, host *scriptedCreditHost, status, sent int, limited bool, retryAfter time.Duration, message, organizationID string) {
	t.Helper()
	accounts, _ := f.List(context.Background())
	before := time.Now()
	observation, err := f.Fetch(context.Background(), accounts[0], nil)
	if got := len(host.sent()); got != sent {
		t.Fatalf("sent %s; want %d", describeRequests(host.sent()), sent)
	}
	if observation.HTTPStatus != status || !observation.RequestSent {
		t.Fatalf("observation status %d sent %v; want %d", observation.HTTPStatus, observation.RequestSent, status)
	}
	var rateLimited cache.RateLimited
	switch {
	case limited:
		if !errors.As(err, &rateLimited) {
			t.Fatalf("err=%v; want a rate limit", err)
		}
		if retryAfter == 0 && !rateLimited.RetryAfter.IsZero() {
			t.Fatalf("Retry-After %v; none was sent", rateLimited.RetryAfter)
		}
		if retryAfter != 0 && (rateLimited.RetryAfter.Before(before.Add(retryAfter-time.Second)) || rateLimited.RetryAfter.After(time.Now().Add(retryAfter+time.Second))) {
			t.Fatalf("Retry-After %v; want %s from now", rateLimited.RetryAfter, retryAfter)
		}
	case message != "":
		if err == nil || err.Error() != message || errors.As(err, &rateLimited) {
			t.Fatalf("err=%v; want %q", err, message)
		}
	default:
		if err != nil {
			t.Fatal(err)
		}
		r := observation.Quota.CostReport
		if r.OrganizationID != organizationID || len(r.Days) != len(costReportDays(host.sent()[0].URL)) {
			t.Fatalf("report organization %q with %d days", r.OrganizationID, len(r.Days))
		}
	}
}

// End to end through the real cache: a 429 on any request of one
// organization's poll, the first page, a later page or /me, backs off that
// organization alone and honours Retry-After. The provider is never paused,
// the other organization is read at the next slot, Claude subscriptions are
// untouched, and the warning says only this credential waits.
func TestACreditRateLimitPausesOnlyThatOrganization(t *testing.T) {
	config := oneCredit + item("healthy", fakeOtherKey, `"500"`, `"2026-10-03"`)
	limitedKey := client.Key(client.ProviderAnthropicAPI, client.APICreditAccount("siphorchannel"))
	healthyKey := client.Key(client.ProviderAnthropicAPI, client.APICreditAccount("healthy"))
	for name, host := range map[string]*scriptedCreditHost{
		"first page": {firstPage: 429, refusalRetryWait: "3600"},
		"page 2":     {secondPage: 429, refusalRetryWait: "3600"},
		"/me":        {me: 429, refusalRetryWait: "3600"},
	} {
		t.Run(name, func(t *testing.T) {
			host.key = fakeAdminKey
			path := filepath.Join(t.TempDir(), "cache", "snapshot.json")
			c, err := cache.Open(cache.Options{Path: path, Interval: 15 * time.Minute, Spacing: time.Second}, fetcherWithCredits(host, config, t))
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			for _, at := range []time.Time{now, now.Add(2 * time.Second)} {
				if err := c.Step(context.Background(), at); err != nil {
					t.Fatal(err)
				}
			}
			c.Close()
			snapshot, _ := client.Load(path)
			limited, healthy := snapshot.Entries[limitedKey], snapshot.Entries[healthyKey]
			if limited.LastError != "provider rate limited" || limited.NextAttempt.Before(now.Add(59*time.Minute)) {
				t.Fatalf("limited entry: last_error %q next %v", limited.LastError, limited.NextAttempt)
			}
			if healthy.LastError != "" || healthy.Quota == nil || healthy.Quota.CostReport == nil {
				t.Fatalf("healthy entry: %+v", healthy)
			}
			if len(snapshot.ProviderCooldown) != 0 {
				t.Fatalf("provider cooldown %v", snapshot.ProviderCooldown)
			}
			lines := host.logged()
			if len(lines) != 1 || lines[0].level != "warn" || lines[0].message != "quota-cache poll rate limited; this credential is retried at next_attempt" {
				t.Fatalf("logged %+v", lines)
			}
			fields := lines[0].fields
			if fields["provider"] != client.ProviderAnthropicAPI || fields["auth_index"] != client.APICreditAccount("siphorchannel") ||
				fields["http_status"] != 429 || fields["next_attempt"] != logTimeOf(limited.NextAttempt) || fields["retry_after"] == nil {
				t.Fatalf("fields %v", fields)
			}
			if _, ok := fields["provider_paused_until"]; ok {
				t.Fatalf("fields %v; nothing else was paused", fields)
			}
		})
	}
}

// No log line, error or failure message of a credit poll ever names an admin
// key, however the poll fails and whatever Anthropic or the transport echoes
// back. A line identifies the organization only by its account id,
// label-<12 hex> or item-N.
func TestNoCreditLogLineOrErrorCarriesTheAdminKey(t *testing.T) {
	accountID := regexp.MustCompile(`^(label-[0-9a-f]{12}|item-[0-9]+)$`)
	echo := `{"error":{"message":"invalid x-api-key ` + fakeAdminKey + `"}}`
	for name, host := range map[string]*scriptedCreditHost{
		"401":                 {firstPage: 401},
		"403":                 {firstPage: 403},
		"429":                 {firstPage: 429, refusalRetryWait: "60"},
		"500":                 {firstPage: 500},
		"429 on page 2":       {secondPage: 429, refusalRetryWait: "60"},
		"401 on page 2":       {secondPage: 401},
		"429 on /me":          {me: 429},
		"401 on /me":          {me: 401},
		"a body echoing it":   {creditHost: creditHost{body: echo}},
		"a transport echoing": {creditHost: creditHost{failHTTP: errors.New("dial tcp: x-api-key " + fakeAdminKey)}},
	} {
		t.Run(name, func(t *testing.T) {
			host.key = fakeAdminKey
			f := fetcherWithCredits(host, oneCredit, t)
			accounts, _ := f.List(context.Background())
			_, fetchErr := f.Fetch(context.Background(), accounts[0], nil)
			if fetchErr == nil {
				t.Fatal("the poll did not fail")
			}
			path := filepath.Join(t.TempDir(), "cache", "snapshot.json")
			c, err := cache.Open(cache.Options{Path: path, Interval: 15 * time.Minute, Spacing: time.Second}, f)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Step(context.Background(), time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			c.Close()
			lines := host.logged()
			if len(lines) != 1 {
				t.Fatalf("logged %d lines; want one", len(lines))
			}
			if id, _ := lines[0].fields["auth_index"].(string); !accountID.MatchString(id) {
				t.Fatalf("auth_index %q is not an account id", id)
			}
			file, _ := os.ReadFile(path)
			for what, text := range map[string]string{
				"log":      fmt.Sprintf("%v %+v %#v", lines, lines, lines),
				"error":    fmt.Sprintf("%v %+v %#v", fetchErr, fetchErr, fetchErr),
				"snapshot": string(file),
			} {
				for _, secret := range []string{fakeAdminKey, "sk-ant-", "FAKEFAKE", "x-api-key", "req_"} {
					if strings.Contains(text, secret) {
						t.Fatalf("the %s carries %q", what, secret)
					}
				}
			}
		})
	}
}

// An item removed, broken or rekeyed after the scan that listed it is never
// read with what it holds now.
func TestCreditFetchRefusesAStaleListing(t *testing.T) {
	host := &creditHost{}
	f := fetcherWithCredits(host, oneCredit, t)
	accounts, _ := f.List(context.Background())
	for _, config := range []string{
		"",
		"claude-api-credits:\n" + item("siphorchannel", fakeOtherKey, `"200"`, `"2026-10-29"`),
		"claude-api-credits:\n" + item("siphorchannel", fakeAdminKey, `"200"`, "-"),
	} {
		items := mustCredits(t, config)
		f.apiCredits.Store(&items)
		if _, err := f.Fetch(context.Background(), accounts[0], nil); err == nil || err.Error() != "credential read failed" {
			t.Fatalf("%q: err=%v", config, err)
		}
	}
	if len(host.sent()) != 0 {
		t.Fatal("a request was sent for a stale listing")
	}
}

// End to end through configure and the real writer: adding the list starts
// polling with no restart, a misconfigured item is visible and never polled,
// removing the list retires the entries, and the key appears in neither the
// snapshot, nor the status route, nor any log.
func TestCreditsAreLiveAndTheKeyIsNeverExposed(t *testing.T) {
	host := &creditHost{}
	p := New(host)
	defer p.Shutdown()
	path := filepath.Join(t.TempDir(), "cache", "snapshot.json")
	configure := func(method, extra string) {
		t.Helper()
		raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("cache-path: " + path + "\nrequest-spacing: 1s\n" + extra)})
		if _, err := p.Handle(method, raw); err != nil {
			t.Fatal(err)
		}
	}
	configure(protocol.MethodPluginRegister, "")
	writer := p.cache
	configure(protocol.MethodPluginReconfigure, oneCredit+item("-", fakeOtherKey, `"5"`, `"2026-10-01"`))
	if p.cache != writer {
		t.Fatal("adding credits restarted the writer")
	}
	good := client.Key(client.ProviderAnthropicAPI, client.APICreditAccount("siphorchannel"))
	broken := client.Key(client.ProviderAnthropicAPI, "item-2")
	snapshot := waitFor(t, path, func(s client.Snapshot) bool {
		entry, ok := s.Entries[good]
		_, seen := s.Entries[broken]
		return ok && seen && entry.Quota != nil && entry.LastError == ""
	})
	if entry := snapshot.Entries[broken]; entry.APICredit == nil || entry.APICredit.Problem != client.CreditProblemLabelMissing || !entry.LastAttempt.IsZero() {
		t.Fatalf("misconfigured entry=%+v", entry)
	}
	if entry := snapshot.Entries[good]; entry.Quota.CostReport.OrganizationID != fakeOrgHeader || entry.APICredit.MonthlyUSD != "200" {
		t.Fatalf("entry=%+v", entry)
	}
	for _, req := range host.sent() {
		if req.Headers["X-Api-Key"][0] != fakeAdminKey {
			t.Fatalf("the misconfigured item was polled: %s", describeRequests([]protocol.HostHTTPRequest{req}))
		}
	}
	// Now a rejected key, so the error path is in the snapshot too.
	host.mu.Lock()
	host.status = 401
	host.mu.Unlock()
	configure(protocol.MethodPluginReconfigure, "claude-api-credits:\n"+item("siphorchannel", fakeOtherKey, `"200"`, `"2026-10-29"`))
	waitFor(t, path, func(s client.Snapshot) bool {
		return s.Entries[good].LastError == client.CreditErrorKeyRejected
	})
	raw, _ := json.Marshal(protocol.ManagementRequest{Method: "GET", Path: "/v0/management/plugins/quota-cache/status"})
	result, err := p.Handle(protocol.MethodManagementHandle, raw)
	if err != nil || result.(protocol.ManagementResponse).StatusCode != 200 {
		t.Fatalf("status err=%v", err)
	}
	status := string(result.(protocol.ManagementResponse).Body)
	file, _ := os.ReadFile(path)
	host.mu.Lock()
	logs := strings.Join(host.logs, "\n")
	host.mu.Unlock()
	for name, text := range map[string]string{"snapshot": string(file), "status": status, "logs": logs} {
		for _, secret := range []string{fakeAdminKey, fakeOtherKey, "FAKEFAKE", "FAKEOTHER", "req_"} {
			if strings.Contains(text, secret) {
				t.Fatalf("%s contains %q", name, secret)
			}
		}
	}
	if !strings.Contains(status, `"api_credit"`) || !strings.Contains(status, `"cost_report"`) {
		t.Fatal("the status route does not carry the credit")
	}
	configure(protocol.MethodPluginReconfigure, "")
	waitFor(t, path, func(s client.Snapshot) bool {
		_, a := s.Entries[good]
		_, b := s.Entries[broken]
		return !a && !b
	})
}

// describeRequests prints requests for a failure message with each key
// replaced by its fingerprint, so a failing test never prints a key, fake or
// not.
func describeRequests(requests []protocol.HostHTTPRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d request(s)", len(requests))
	for _, r := range requests {
		fmt.Fprintf(&b, "\n%s %s", r.Method, r.URL)
		names := make([]string, 0, len(r.Headers))
		for name := range r.Headers {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			values := r.Headers[name]
			if strings.EqualFold(name, "X-Api-Key") {
				printed := make([]string, len(values))
				for i, v := range values {
					printed[i] = creditKeyFingerprint(v)
				}
				values = printed
			}
			fmt.Fprintf(&b, "\n  %s: %q", name, values)
		}
	}
	return b.String()
}

// Formatting an item, in any way, prints its key's fingerprint, never the key.
func TestCreditItemFormattingNeverPrintsTheKey(t *testing.T) {
	item := creditItem{id: "label-x", key: fakeAdminKey, credit: client.APICredit{Label: "x", KeyFingerprint: creditKeyFingerprint(fakeAdminKey)}}
	items := []creditItem{item}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x"} {
		for _, value := range []any{item, &item, items, &items} {
			if out := fmt.Sprintf(verb, value); strings.Contains(out, fakeAdminKey) || strings.Contains(out, "FAKE") || !strings.Contains(out, creditKeyFingerprint(fakeAdminKey)) {
				t.Errorf("%s of %T printed %q", verb, value, out)
			}
		}
	}
	if out := describeRequests([]protocol.HostHTTPRequest{{Headers: map[string][]string{"X-Api-Key": {fakeAdminKey}}}}); strings.Contains(out, fakeAdminKey) {
		t.Errorf("describeRequests printed %q", out)
	}
}
