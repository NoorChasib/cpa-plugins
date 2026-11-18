package meter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// Placeholder OpenAI API organizations, as the openai-organization header
// names them. None is a real organization's.
const (
	slugPro      = "user-AbCdEfGhIjKlMnOpQrStUvWx"
	slugBusiness = "acme-x1wegt"
	slugSide     = "side-k2m9qz"
)

// openAICanaries are what an openai-compatibility record carries in the
// fields the OpenAI meter must never keep, matched case-insensitively.
// entryname-canary is the entry name the operator typed, which CPA puts in
// Provider as well as AuthID.
var openAICanaries = []string{"BASEURL-CANARY", "ALIAS-CANARY", "APIKEY-CANARY", "ACCESS-CANARY", "SELECTION-CANARY", "SESSION-CANARY", "PARENT-CANARY",
	"entryname-canary", "abcabcabcabc", "SOURCE-CANARY", "PROJECT-CANARY", "HEADER-CANARY", "COOKIE-CANARY", "BODY-CANARY", "0badf00d",
	"11111111-2222-4333-8444-666666666666"}

// openAICanaryIn is the first canary text carries, or "".
func openAICanaryIn(text string) string {
	lower := strings.ToLower(text)
	for _, canary := range openAICanaries {
		if strings.Contains(lower, strings.ToLower(canary)) {
			return canary
		}
	}
	return ""
}

// oaiWire is an openai-compatibility usage record as CPA marshals
// pluginapi.UsageRecord: Go field names, no tags. It carries the fields the
// OpenAI meter must never read, with canaries in them. Provider is any so a
// test can give it a JSON value that is not a plain string.
type oaiWire struct {
	RequestID, TraceID, SelectionID                                           string
	Provider                                                                  any
	BaseURL, Model, Alias, APIKey, AccessProvider, SessionID, ParentSessionID string
	AuthID, AuthIndex, AuthType, Source, ResponseServiceTier, ResponseModel   string
	RequestedAt                                                               any
	Latency                                                                   int64
	Failed                                                                    bool
	Failure                                                                   struct {
		StatusCode int
		Body       string
	}
	Detail          struct{ InputTokens, OutputTokens, ReasoningTokens, CacheReadTokens, CacheCreationTokens int64 }
	ResponseHeaders map[string][]string
}

// openAIRecord is a successful openai-compatibility API-key record whose
// response names org, requested at at with no latency, carrying 1000 input
// and 100 output tokens.
func openAIRecord(org string, at time.Time) oaiWire {
	w := oaiWire{RequestID: "11111111-2222-4333-8444-666666666666", TraceID: "0badf00d", SelectionID: "SELECTION-CANARY",
		Provider: "openai-compatible-entryname-canary", BaseURL: "https://api.openai.com/v1/BASEURL-CANARY", Model: "gpt-6-sol",
		Alias: "ALIAS-CANARY", APIKey: "sk-cpa-APIKEY-CANARY", AccessProvider: "ACCESS-CANARY", SessionID: "SESSION-CANARY", ParentSessionID: "PARENT-CANARY",
		AuthID: "openai-compatibility:entryname-canary:abcabcabcabc", AuthIndex: "fedcba9876543210", AuthType: "apikey", Source: "sk-proj-SOURCE-CANARY",
		RequestedAt: at.Format(time.RFC3339Nano),
		ResponseHeaders: map[string][]string{"Openai-Project": {"proj_PROJECT-CANARY"}, "X-Request-Id": {"req_HEADER-CANARY"},
			"Set-Cookie": {"__cf_bm=COOKIE-CANARY; path=/"}}}
	if org != "" {
		w.ResponseHeaders["Openai-Organization"] = []string{org}
	}
	w.Detail.InputTokens, w.Detail.OutputTokens = 1000, 100
	return w
}

func (w oaiWire) bytes() []byte {
	raw, err := json.Marshal(w)
	if err != nil {
		panic(err)
	}
	return raw
}

// exhaustedBody is OpenAI's answer to a request from an organization with no
// credit left, as CPA puts it in Failure.Body.
const exhaustedBody = `{"error":{"message":"You have no credits remaining. Add credits to continue using the API. BODY-CANARY","type":"insufficient_quota","param":null,"code":"credit_balance_exhausted"}}`

// testOpenAIMeter is testMeter for the OpenAI source.
func testOpenAIMeter(t *testing.T, linked []string, now time.Time) (*Meter, *fakeLog) {
	t.Helper()
	log := &fakeLog{}
	m := newMeterFor(OpenAI, filepath.Join(t.TempDir(), "snapshot.openai-meter.json"), log)
	m.restore(linked, now)
	return m, log
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func noFileAt(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s exists: %v", filepath.Base(path), err)
	}
}

// The success record marshalled from CPA v8.0.77's own pluginapi.UsageRecord
// and encoder. Decoding it yields the expected event, and every key the
// OpenAI meter reads is in the bytes, so a field renamed upstream fails here
// rather than silently counting nothing.
func TestOpenAIGoldenRecordDecodesAndNamesEveryFieldRead(t *testing.T) {
	raw := fixture(t, "usage-record-openai-compat-v8.0.77.json")
	for _, key := range []string{"Provider", "AuthIndex", "AuthType", "Model", "ResponseModel", "ResponseServiceTier", "RequestedAt", "Latency", "Failed",
		"Failure", "StatusCode", "Body", "Detail", "InputTokens", "OutputTokens", "CacheReadTokens", "CacheCreationTokens", "ResponseHeaders", "Openai-Organization"} {
		if !strings.Contains(string(raw), `"`+key+`":`) {
			t.Errorf("the golden record has no %q key; the field the meter reads has moved", key)
		}
	}
	e, keep, rejected := decodeOpenAI(raw, base.Add(5*time.Second))
	// 300,000 input tokens, of which 200,000 were read from the cache and
	// 50,000 written to it; the whole input is past 272,000.
	want := event{at: base.Add(time.Second), authIndex: "fedcba9876543210", org: slugPro, model: "gpt-6-sol-2026-08-01",
		prompt: client.MeterPromptOver272K, tier: client.MeterTierFlex, input: 50_000, output: 1200, cacheRead: 200_000, cacheWrite: 50_000}
	if !keep || rejected || e != want {
		t.Fatalf("event=%+v keep=%v rejected=%v\nwant %+v", e, keep, rejected, want)
	}
	if canary := openAICanaryIn(fmt.Sprintf("%+v %#v", e, e)); canary != "" {
		t.Fatalf("the event carries %q", canary)
	}
}

// OpenAI refuses an organization with no credit left with a 429 whose code is
// credit_balance_exhausted. Nothing else is a refusal: a plain rate limit,
// insufficient_quota alone, or the code at another status.
func TestOpenAIExhaustionIsARefusal(t *testing.T) {
	raw := fixture(t, "usage-record-openai-compat-429-v8.0.77.json")
	if !strings.Contains(string(raw), `"Body":`) || !strings.Contains(string(raw), "credit_balance_exhausted") {
		t.Fatal("the 429 record has no failure body naming the code")
	}
	e, keep, rejected := decodeOpenAI(raw, base.Add(5*time.Second))
	want := event{at: base.Add(time.Second), authIndex: "fedcba9876543210", org: slugBusiness, model: "gpt-6-sol-2026-08-01", failed: true, refusal: true}
	if !keep || rejected || e != want {
		t.Fatalf("event=%+v keep=%v rejected=%v\nwant %+v", e, keep, rejected, want)
	}
	m, _ := testOpenAIMeter(t, []string{slugBusiness}, base)
	feed(m, raw, base.Add(5*time.Second))
	o := org(t, m, slugBusiness)
	if o.Refusals != 1 || o.LastRefusalAt == nil || !o.LastRefusalAt.Equal(base.Add(time.Second)) || o.ClaudeCodeRefusals != 0 || o.LastClaudeCodeRefusalAt != nil ||
		o.LastSuccessAt != nil || o.Days[0].Usage[0].Failed != 1 || m.data.Counted != 1 {
		t.Fatalf("the refusal was not counted on its organization: %+v counted=%d", o, m.data.Counted)
	}

	for _, tc := range []struct {
		name      string
		failed    bool
		status    int
		body      string
		cancelled bool
	}{
		{"a rate limit", true, 429, `{"error":{"message":"Rate limit reached","type":"requests","code":"rate_limit_exceeded"}}`, false},
		{"insufficient_quota alone", true, 429, `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}}`, false},
		{"the code at 400", true, 400, exhaustedBody, false},
		{"a success is never a refusal", false, 429, exhaustedBody, false},
		{"a cancellation", true, 499, "context canceled", true},
	} {
		w := openAIRecord(slugBusiness, base)
		w.Failed, w.Failure.StatusCode, w.Failure.Body = tc.failed, tc.status, tc.body
		e, keep, rejected := decodeOpenAI(w.bytes(), base)
		if !keep || rejected || e.refusal || e.cancelled != tc.cancelled || e.failed != tc.failed {
			t.Fatalf("%s: event=%+v keep=%v rejected=%v", tc.name, e, keep, rejected)
		}
		feed(m, w.bytes(), base)
	}
	if o := org(t, m, slugBusiness); o.Refusals != 1 || m.data.Counted != 6 {
		t.Fatalf("refusals=%d counted=%d", o.Refusals, m.data.Counted)
	}
}

// A record's secrets, its entry name and its free text never reach the file,
// the log or an event, however it is formatted.
func TestOpenAICanariesNeverLeaveTheRecord(t *testing.T) {
	log := &fakeLog{}
	path := filepath.Join(t.TempDir(), "snapshot.openai-meter.json")
	m := OpenFor(OpenAI, path, []string{slugPro, slugBusiness}, base, log)
	success := fixture(t, "usage-record-openai-compat-v8.0.77.json")
	exhausted := fixture(t, "usage-record-openai-compat-429-v8.0.77.json")
	// A headerless failure on the auth the fixtures taught, and traffic from
	// an organization that is not listed.
	headerless := openAIRecord("", base.Add(7*time.Second))
	headerless.Failed, headerless.Failure.StatusCode, headerless.Failure.Body = true, 500, "upstream failed BODY-CANARY"
	records := [][]byte{success, exhausted, headerless.bytes(), openAIRecord(slugSide, base).bytes()}
	for i, raw := range records {
		m.Offer(raw, base.Add(time.Duration(5+i)*time.Second))
	}
	m.Stop(client.MeterStopShutdown, base.Add(time.Minute))
	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for what, text := range map[string]string{"file": string(file), "log": log.text()} {
		if canary := openAICanaryIn(text); canary != "" {
			t.Fatalf("the %s carries %q", what, canary)
		}
	}
	for _, raw := range records {
		e, _, _ := decodeOpenAI(raw, base.Add(time.Minute))
		if canary := openAICanaryIn(fmt.Sprintf("%+v %#v", e, e)); canary != "" {
			t.Fatalf("an event carries %q", canary)
		}
	}
	meter := load(t, path)
	if meter.Received != 4 || meter.Counted != 3 || meter.Organizations[slugBusiness].Refusals != 1 || len(meter.Unlinked) != 1 || meter.Unlinked[0].OrganizationID != slugSide {
		t.Fatalf("file: %+v", meter)
	}
}

// Only openai-compatibility API-key records are counted: never a codex
// record, though it carries OpenAI's header, nor another provider's, nor one
// whose Provider is not a plain string. Neither kind of record is rejected.
func TestOnlyOpenAICompatibilityAPIKeyRecordsAreCounted(t *testing.T) {
	m, _ := testOpenAIMeter(t, []string{slugPro}, base)
	for name, change := range map[string]func(*oaiWire){
		"codex with the header":            func(w *oaiWire) { w.Provider = "codex" },
		"claude":                           func(w *oaiWire) { w.Provider = "claude" },
		"openai":                           func(w *oaiWire) { w.Provider = "openai" },
		"the named prefix with no name":    func(w *oaiWire) { w.Provider = "openai-compatible-" },
		"a longer unnamed key":             func(w *oaiWire) { w.Provider = "openai-compatibilityx" },
		"spaces around the key":            func(w *oaiWire) { w.Provider = " openai-compatibility " },
		"oauth":                            func(w *oaiWire) { w.Provider, w.AuthType = "openai-compatible-x", "oauth" },
		"no auth type":                     func(w *oaiWire) { w.AuthType = "" },
		"a null provider":                  func(w *oaiWire) { w.Provider = nil },
		"a number":                         func(w *oaiWire) { w.Provider = 123 },
		"an object":                        func(w *oaiWire) { w.Provider = map[string]string{"key": "openai-compatibility"} },
		"an escape that spells the key":    func(w *oaiWire) { w.Provider = json.RawMessage(`"openai-\u0063ompatibility"`) },
		"an escape in a named key's start": func(w *oaiWire) { w.Provider = json.RawMessage(`"openai-compatibl\u0065-x"`) },
	} {
		w := openAIRecord(slugPro, base)
		change(&w)
		if e, keep, rejected := decodeOpenAI(w.bytes(), base); keep || rejected || e != (event{}) {
			t.Fatalf("%s: event=%+v keep=%v rejected=%v", name, e, keep, rejected)
		}
		feed(m, w.bytes(), base)
	}
	if m.received.Load() != 13 || m.rejected.Load() != 0 || m.data.Counted != 0 || len(m.data.Auths) != 0 || len(m.data.Unlinked) != 0 || org(t, m, slugPro).LastSeenAt != nil {
		t.Fatalf("received=%d rejected=%d data=%+v", m.received.Load(), m.rejected.Load(), m.data)
	}
	// An unnamed entry's key and a named one's, and either in any case. A
	// named one's entry name may carry an escape: CPA's encoder, like
	// encoding/json, writes & < > as \u0026 \u003c \u003e.
	for _, tc := range []struct {
		provider any
		authType string
	}{
		{"openai-compatibility", "apikey"}, {"openai-compatible-openai", "apikey"}, {"OpenAI-Compatibility", "APIKey"}, {"OPENAI-COMPATIBLE-X", "ApiKey"},
		{"openai-compatible-r&d", "apikey"}, {"openai-compatible-<team>", "apikey"}, {json.RawMessage(`"openai-compatible-\u0078"`), "apikey"},
	} {
		w := openAIRecord(slugPro, base)
		w.Provider, w.AuthType = tc.provider, tc.authType
		feed(m, w.bytes(), base)
	}
	if m.data.Counted != 7 {
		t.Fatalf("counted=%d; provider and auth type are matched case-insensitively", m.data.Counted)
	}
}

// Provider is read as a bool from its raw bytes, so the entry name a named
// entry's key carries never becomes a string.
func TestOpenAIProviderIsNeverMaterialized(t *testing.T) {
	var w openAIWire
	if err := json.Unmarshal(fixture(t, "usage-record-openai-compat-v8.0.77.json"), &w); err != nil || !bool(w.Provider) {
		t.Fatalf("provider=%v err=%v", w.Provider, err)
	}
	if printed := fmt.Sprintf("%#v", w); strings.Contains(strings.ToLower(printed), "entryname-canary") || strings.Contains(printed, "abcabcabcabc") {
		t.Fatalf("the wire carries the entry name: %s", printed)
	}
	for raw, want := range map[string]bool{
		`"openai-compatibility"`: true, `"OPENAI-COMPATIBILITY"`: true, `"openai-compatible-x"`: true, `"Openai-Compatible-EntryName"`: true,
		`"openai-compatible-r\u0026d"`: true, `"openai-compatibl\u0065-x"`: false,
		`"openai-compatible-"`: false, `"openai-compatib"`: false, `""`: false, `"openai-\u0063ompatibility"`: false,
		`null`: false, `1`: false, `true`: false, `{}`: false, `["openai-compatibility"]`: false,
	} {
		p := openAIProvider(!want)
		if err := p.UnmarshalJSON([]byte(raw)); err != nil || bool(p) != want {
			t.Fatalf("%s: %v err=%v, want %v", raw, p, err, want)
		}
	}
}

// Without the header, only a failure on an auth a headed record taught is
// counted, on that record's organization. A headerless success is counted
// nowhere, even on a known auth, and nothing is ever foreign or unattributed.
func TestOpenAIHeaderlessRecordsNeedAKnownAuth(t *testing.T) {
	m, _ := testOpenAIMeter(t, []string{slugPro}, base)
	success := openAIRecord("", base)
	failure := openAIRecord("", base)
	failure.Failed, failure.Failure.StatusCode = true, 500
	exhausted := openAIRecord("", base)
	exhausted.Failed, exhausted.Failure.StatusCode, exhausted.Failure.Body = true, 429, exhaustedBody
	for _, w := range []oaiWire{success, failure, exhausted} {
		feed(m, w.bytes(), base)
	}
	if m.data.Counted != 0 || m.data.Foreign != 0 || m.data.Unattributed != 0 || m.data.LastUnattributedAt != nil || len(m.data.Auths) != 0 ||
		len(m.data.Unlinked) != 0 || org(t, m, slugPro).LastSeenAt != nil {
		t.Fatalf("an unknown auth's headerless records were counted: %+v", m.data)
	}

	feed(m, openAIRecord(slugPro, base.Add(time.Second)).bytes(), base.Add(time.Second))
	if a := m.data.Auths["fedcba9876543210"]; a.OrganizationID != slugPro || m.data.Counted != 1 {
		t.Fatalf("auths=%v counted=%d", m.data.Auths, m.data.Counted)
	}
	later := base.Add(2 * time.Second)
	for _, w := range []oaiWire{failure, exhausted} {
		w.RequestedAt = later.Format(time.RFC3339)
		feed(m, w.bytes(), later)
	}
	o := org(t, m, slugPro)
	if m.data.Counted != 3 || o.Refusals != 1 || !o.LastRefusalAt.Equal(later) || !o.LastSeenAt.Equal(later) || !m.data.Auths["fedcba9876543210"].SeenAt.Equal(later) ||
		o.Days[0].Usage[0].Failed != 2 || o.Days[0].Usage[0].Requests != 1 {
		t.Fatalf("a headerless failure on a known auth was not counted on its organization: counted=%d %+v", m.data.Counted, o)
	}

	// A headerless success on that known auth, and a failure with no auth
	// index, are still no one's.
	success.RequestedAt = later.Add(time.Second).Format(time.RFC3339)
	feed(m, success.bytes(), later.Add(time.Second))
	failure.AuthIndex = ""
	feed(m, failure.bytes(), later.Add(time.Second))
	o = org(t, m, slugPro)
	if m.data.Counted != 3 || !o.LastSuccessAt.Equal(base.Add(time.Second)) || o.Days[0].Usage[0].Requests != 1 || !m.data.Auths["fedcba9876543210"].SeenAt.Equal(later) {
		t.Fatalf("counted=%d %+v auths=%v", m.data.Counted, o, m.data.Auths)
	}
	if m.data.Foreign != 0 || m.data.Unattributed != 0 || m.data.LastUnattributedAt != nil {
		t.Fatalf("foreign=%d unattributed=%d", m.data.Foreign, m.data.Unattributed)
	}
	if raw := string(m.exportLimited(later.Add(time.Minute), client.MaxMeterBytes)); strings.Contains(raw, "foreign") || strings.Contains(raw, "unattributed") {
		t.Fatalf("export: %s", raw)
	}
}

// A header the organization shape refuses rejects the record, so its spend is
// flagged rather than passed over. An empty value is no header, and the
// provider filter comes first.
func TestAnUnreadableOrganizationHeaderIsRejected(t *testing.T) {
	m, _ := testOpenAIMeter(t, []string{slugPro}, base)
	for _, header := range []string{"acme corp", strings.Repeat("a", 65), "-acme", "acmé"} {
		feed(m, openAIRecord(header, base).bytes(), base)
	}
	if m.rejected.Load() != 4 || m.lastRejectedAt.Load() != base.Unix() || m.data.Counted != 0 || len(m.data.Unlinked) != 0 || len(m.data.Auths) != 0 {
		t.Fatalf("rejected=%d at=%d data=%+v", m.rejected.Load(), m.lastRejectedAt.Load(), m.data)
	}
	if raw := m.exportLimited(base, client.MaxMeterBytes); !strings.Contains(string(raw), `"rejected":4,"last_rejected_at":"2026-10-09T12:00:00Z"`) {
		t.Fatalf("export: %s", raw)
	}

	for _, values := range [][]string{{""}, {"  "}, {}} {
		w := openAIRecord("", base)
		w.ResponseHeaders["Openai-Organization"] = values
		feed(m, w.bytes(), base)
	}
	codex := openAIRecord("acme corp", base)
	codex.Provider = "codex"
	feed(m, codex.bytes(), base)
	if m.rejected.Load() != 4 || m.data.Counted != 0 {
		t.Fatalf("rejected=%d counted=%d", m.rejected.Load(), m.data.Counted)
	}

	// The value is trimmed, the header is matched in any case, and 64 bytes
	// is a slug.
	feed(m, openAIRecord(" "+slugPro+" ", base).bytes(), base)
	lower := openAIRecord("", base)
	lower.ResponseHeaders["openai-organization"] = []string{slugPro}
	feed(m, lower.bytes(), base)
	feed(m, openAIRecord(strings.Repeat("a", 64), base).bytes(), base)
	if m.rejected.Load() != 4 || m.data.Counted != 2 || len(m.data.Unlinked) != 1 || m.data.Unlinked[0].OrganizationID != strings.Repeat("a", 64) {
		t.Fatalf("rejected=%d counted=%d unlinked=%+v", m.rejected.Load(), m.data.Counted, m.data.Unlinked)
	}
}

// Unreadable records and negative token counts are rejected before the
// provider filter, as the Claude meter's are.
func TestUnreadableOpenAIRecordsAndNegativeCountsAreRejected(t *testing.T) {
	m, _ := testOpenAIMeter(t, []string{slugPro}, base)
	negative := openAIRecord(slugPro, base)
	negative.Detail.CacheCreationTokens = -1
	codex := openAIRecord(slugPro, base)
	codex.Provider, codex.Detail.OutputTokens = "codex", -1
	headers := openAIRecord("", base)
	headers.ResponseHeaders["Openai-Organization"] = nil
	badHeader := strings.Replace(string(headers.bytes()), `"Openai-Organization":null`, `"Openai-Organization":"`+slugPro+`"`, 1)
	for _, raw := range [][]byte{[]byte("not json"), []byte(`[]`), []byte(`{"Latency":"slow"}`), negative.bytes(), codex.bytes(), []byte(badHeader)} {
		feed(m, raw, base)
	}
	if m.received.Load() != 6 || m.rejected.Load() != 6 || m.data.Counted != 0 {
		t.Fatalf("received=%d rejected=%d counted=%d", m.received.Load(), m.rejected.Load(), m.data.Counted)
	}
}

// Input is the whole prompt less what was read from and written to the
// cache, never below zero; output is as reported, reasoning included; and the
// prompt class is judged on the whole input.
func TestOpenAIInputExcludesCacheReadsAndWrites(t *testing.T) {
	for _, tc := range []struct {
		name                                    string
		input, cacheRead, cacheWrite, reasoning int64
		wantInput, wantRead, wantWrite, wantOut uint64
		wantPrompt                              string
	}{
		{"no cache", 1000, 0, 0, 0, 1000, 0, 0, 100, ""},
		{"reads and writes", 1000, 300, 200, 80, 500, 300, 200, 100, ""},
		{"more cached than input", 100, 80, 50, 0, 0, 80, 50, 100, ""},
		{"at 272,000", 272_000, 0, 0, 0, 272_000, 0, 0, 100, ""},
		{"past 272,000, all from the cache", 272_001, 272_001, 0, 0, 0, 272_001, 0, 100, client.MeterPromptOver272K},
	} {
		w := openAIRecord(slugPro, base)
		w.Detail.InputTokens, w.Detail.CacheReadTokens, w.Detail.CacheCreationTokens, w.Detail.ReasoningTokens = tc.input, tc.cacheRead, tc.cacheWrite, tc.reasoning
		e, keep, rejected := decodeOpenAI(w.bytes(), base)
		if !keep || rejected || e.input != tc.wantInput || e.cacheRead != tc.wantRead || e.cacheWrite != tc.wantWrite || e.output != tc.wantOut || e.prompt != tc.wantPrompt {
			t.Errorf("%s: event=%+v keep=%v rejected=%v", tc.name, e, keep, rejected)
		}
	}
}

// A fine-tuned model id keeps only "ft:" and its base: the rest is the
// operator's organization name and chosen suffix, which never reach the file.
func TestFineTunedModelIdsKeepOnlyTheirBase(t *testing.T) {
	long := "ft:gpt-4.1-2025-04-14:acme-corp:my-suffix:AbC123" + strings.Repeat("Z", 42)
	if len(long) != 90 {
		t.Fatalf("the long id is %d bytes", len(long))
	}
	for _, tc := range []struct{ model, responseModel, want string }{
		{"ft:gpt-4.1-2025-04-14:acme-corp:my-suffix:AbC123", "", "ft:gpt-4.1-2025-04-14"},
		{long, "", "ft:gpt-4.1-2025-04-14"},
		{"gpt-4.1-mini", "FT:GPT-4.1-mini:acme-corp:my-suffix:AbC123", "ft:gpt-4.1-mini"},
		{" ft:gpt-4o:acme-corp ", "", "ft:gpt-4o"},
		{"ft:gpt-4o", "", "ft:gpt-4o"},
		{"GPT-6-Sol", " ", "gpt-6-sol"},
		{"left:ft:acme-corp", "", "left:ft:acme-corp"},
		{"ft:" + strings.Repeat("b", 70) + ":acme-corp", "", client.MeterOtherModel},
		{strings.Repeat("g", 65), "", client.MeterOtherModel},
	} {
		if got := openAIMeterModel(tc.model, tc.responseModel); got != tc.want {
			t.Errorf("openAIMeterModel(%q, %q) = %q, want %q", tc.model, tc.responseModel, got, tc.want)
		}
	}

	m, _ := testOpenAIMeter(t, []string{slugPro}, base)
	for _, model := range []string{"ft:gpt-4.1-2025-04-14:acme-corp:my-suffix:AbC123", long} {
		w := openAIRecord(slugPro, base)
		w.Model = model
		feed(m, w.bytes(), base)
	}
	raw := strings.ToLower(string(m.exportLimited(base.Add(time.Minute), client.MaxMeterBytes)))
	if !strings.Contains(raw, `"model":"ft:gpt-4.1-2025-04-14","requests":2`) {
		t.Fatalf("export: %s", raw)
	}
	for _, text := range []string{"acme-corp", "my-suffix", "abc123"} {
		if strings.Contains(raw, text) {
			t.Fatalf("the file carries %q", text)
		}
	}
}

// Organizations are matched exactly, case included: a slug no listed
// organization names, even one that differs only in case, is listed as
// unlinked as the header names it, and not counted.
func TestOpenAIUnlistedOrganizationsAreListedExactly(t *testing.T) {
	m, _ := testOpenAIMeter(t, []string{slugPro}, base)
	upper := strings.ToUpper(slugPro)
	feed(m, openAIRecord(slugPro, base).bytes(), base)
	feed(m, openAIRecord(upper, base.Add(time.Second)).bytes(), base.Add(time.Second))
	for i := 0; i < 2; i++ {
		feed(m, openAIRecord(slugBusiness, base.Add(2*time.Second)).bytes(), base.Add(2*time.Second))
	}
	var exported client.APIMeter
	if err := json.Unmarshal(m.exportLimited(base.Add(time.Minute), client.MaxMeterBytes), &exported); err != nil {
		t.Fatal(err)
	}
	if _, listed := exported.Organizations[upper]; listed || len(exported.Organizations) != 1 || exported.Counted != 1 || len(exported.Unlinked) != 2 ||
		exported.Unlinked[0].OrganizationID != slugBusiness || exported.Unlinked[0].Requests != 2 ||
		exported.Unlinked[1].OrganizationID != upper || exported.Unlinked[1].Requests != 1 {
		t.Fatalf("organizations=%v counted=%d unlinked=%+v", exported.Organizations, exported.Counted, exported.Unlinked)
	}
	// Listing the upper-case one makes it its own organization.
	m.SetLinked([]string{slugPro, upper}, base.Add(time.Hour))
	if o := org(t, m, upper); !o.Since.Equal(base.Add(time.Hour)) || len(m.data.Unlinked) != 1 || m.data.Unlinked[0].OrganizationID != slugBusiness {
		t.Fatalf("org=%+v unlinked=%+v", o, m.data.Unlinked)
	}
}

// The OpenAI meter records no config API-key auth IDs: its file's auth_ids is
// always empty and it never says from when it records them.
func TestOpenAIMeterRecordsNoAuthIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.openai-meter.json")
	m := OpenFor(OpenAI, path, []string{slugPro}, base, &fakeLog{})
	// Even an AuthID in the Claude config shape is never read.
	w := openAIRecord(slugPro, base)
	w.AuthID = keyA
	m.Offer(w.bytes(), base.Add(time.Second))
	m.Stop(client.MeterStopShutdown, base.Add(time.Minute))
	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"auth_ids_since", keyA, "foreign", "unattributed"} {
		if strings.Contains(string(file), text) {
			t.Fatalf("the file carries %q: %s", text, file)
		}
	}
	if !strings.Contains(string(file), `"auth_ids":{}`) {
		t.Fatalf("the file has no empty auth_ids: %s", file)
	}
	again := OpenFor(OpenAI, path, []string{slugPro}, base.Add(time.Hour), &fakeLog{})
	again.Stop(client.MeterStopShutdown, base.Add(2*time.Hour))
	if meter := load(t, path); meter.AuthIDsSince != nil || len(meter.AuthIDs) != 0 || meter.Restarts != 1 || meter.Counted != 1 {
		t.Fatalf("restarted: %+v", meter)
	}
}

// Usage is keyed by model, prompt class and service tier, in the day and the
// hour bucket alike, sorted by model, prompt, then tier. A tier is a key of
// its own under the per-bucket cap too.
func TestOpenAIBucketsKeyByTier(t *testing.T) {
	m, _ := testOpenAIMeter(t, []string{slugPro}, base)
	for _, tier := range []string{"flex", "", "priority", "fast", "default", "scale", "auto", "ultrafast", "turbo", "Flex"} {
		w := openAIRecord(slugPro, base)
		w.ResponseServiceTier = tier
		feed(m, w.bytes(), base)
	}
	long := openAIRecord(slugPro, base)
	long.ResponseServiceTier, long.Detail.InputTokens, long.Detail.CacheReadTokens = "flex", 300_000, 100_000
	feed(m, long.bytes(), base)
	want := []client.MeterUsage{
		{Model: "gpt-6-sol", Requests: 4, Input: 4000, Output: 400},
		{Model: "gpt-6-sol", Tier: client.MeterTierFast, Requests: 2, Input: 2000, Output: 200},
		{Model: "gpt-6-sol", Tier: client.MeterTierFlex, Requests: 2, Input: 2000, Output: 200},
		{Model: "gpt-6-sol", Tier: client.MeterTierUltrafast, Requests: 1, Input: 1000, Output: 100},
		{Model: "gpt-6-sol", Tier: client.MeterTierUnknown, Requests: 1, Input: 1000, Output: 100},
		{Model: "gpt-6-sol", Prompt: client.MeterPromptOver272K, Tier: client.MeterTierFlex, Requests: 1, Input: 200_000, Output: 100, CacheRead: 100_000},
	}
	o := org(t, m, slugPro)
	if !reflect.DeepEqual(o.Days[0].Usage, want) || !reflect.DeepEqual(o.Hours[0].Usage, want) || m.entries != 12 {
		t.Fatalf("days=%+v\nhours=%+v\nwant %+v", o.Days[0].Usage, o.Hours[0].Usage, want)
	}
	raw := string(m.exportLimited(base.Add(time.Minute), client.MaxMeterBytes))
	for _, text := range []string{`{"model":"gpt-6-sol","requests":4,`, `{"model":"gpt-6-sol","tier":"fast","requests":2,`, `{"model":"gpt-6-sol","prompt":"over_272k","tier":"flex","requests":1,`} {
		if !strings.Contains(raw, text) {
			t.Fatalf("export has no %s: %s", text, raw)
		}
	}

	// The next day: three models in every tier and a fourth in Standard fill
	// a bucket's sixteen keys, so the fourth model's Flex goes to (other).
	day := base.Add(24 * time.Hour)
	tiers := []string{"", "flex", "fast", "ultrafast", "turbo"}
	for i := 0; i < 16; i++ {
		w := openAIRecord(slugPro, day)
		w.Model, w.ResponseServiceTier = fmt.Sprintf("gpt-model-%d", i/len(tiers)), tiers[i%len(tiers)]
		feed(m, w.bytes(), day)
	}
	extra := openAIRecord(slugPro, day)
	extra.Model, extra.ResponseServiceTier = "gpt-model-3", "flex"
	feed(m, extra.bytes(), day)
	o = org(t, m, slugPro)
	if u := o.Days[1].Usage; len(u) != 17 || u[0].Model != client.MeterOtherModel || u[0].Tier != "" || u[0].Requests != 1 || o.LastOverflowAt == nil || !o.LastOverflowAt.Equal(day) {
		t.Fatalf("usage=%+v overflow=%v", u, o.LastOverflowAt)
	}
	checkEntries(t, m)
}

// Each meter takes in the other's records and counts none of them.
func TestEachMeterIgnoresTheOthersRecords(t *testing.T) {
	claude := fixture(t, "usage-record-v8.0.22.json")
	openai := fixture(t, "usage-record-openai-compat-v8.0.77.json")
	if e, keep, rejected := decodeOpenAI(claude, base); keep || rejected || e != (event{}) {
		t.Fatalf("the OpenAI meter read the Claude record: %+v keep=%v rejected=%v", e, keep, rejected)
	}
	if e, keep, rejected := decode(openai, base); keep || rejected || e != (event{}) {
		t.Fatalf("the Claude meter read the OpenAI record: %+v keep=%v rejected=%v", e, keep, rejected)
	}
	om, _ := testOpenAIMeter(t, []string{slugPro, orgA}, base)
	feed(om, claude, base.Add(5*time.Second))
	cm, _ := testMeter(t, []string{orgA, slugPro}, base)
	feed(cm, openai, base.Add(5*time.Second))
	for name, m := range map[string]*Meter{"OpenAI": om, "Claude": cm} {
		if m.received.Load() != 1 || m.rejected.Load() != 0 || m.data.Counted != 0 || len(m.data.Auths) != 0 || len(m.data.AuthIDs) != 0 || len(m.data.Unlinked) != 0 || m.entries != 0 {
			t.Fatalf("%s meter: received=%d rejected=%d data=%+v", name, m.received.Load(), m.rejected.Load(), m.data)
		}
	}
}

// Every line the OpenAI meter logs names the OpenAI API meter, never Claude.
func TestOpenAILogLinesNameOpenAI(t *testing.T) {
	dir := t.TempDir()
	lines := map[string]logLine{}
	pick := func(log *fakeLog, level string) {
		t.Helper()
		for _, line := range log.logged() {
			if line.level == level {
				lines[line.message] = line
				return
			}
		}
		t.Fatalf("no %s line: %+v", level, log.logged())
	}

	log := &fakeLog{}
	started := OpenFor(OpenAI, filepath.Join(dir, "started.json"), []string{slugPro, slugBusiness}, base, log)
	started.Stop(client.MeterStopShutdown, base)
	if logged := log.logged(); len(logged) != 1 || logged[0].fields["organizations"] != 2 {
		t.Fatalf("logged %+v", logged)
	}
	pick(log, "info")

	unreadable := filepath.Join(dir, "unreadable.json")
	if err := os.WriteFile(unreadable, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	log = &fakeLog{}
	OpenFor(OpenAI, unreadable, nil, base, log).Stop(client.MeterStopShutdown, base)
	pick(log, "warn")

	dropping, log := testOpenAIMeter(t, []string{slugPro}, base)
	for i := 0; i <= intakeSize; i++ {
		dropping.Offer(openAIRecord(slugPro, base).bytes(), base)
	}
	dropping.flushIfDue(base.Add(time.Minute))
	pick(log, "warn")

	log = &fakeLog{}
	unsaved := newMeterFor(OpenAI, filepath.Join(dir, "gone", "snapshot.openai-meter.json"), log)
	unsaved.restore([]string{slugPro}, base)
	unsaved.flushIfDue(base.Add(time.Minute))
	pick(log, "warn")

	log = &fakeLog{}
	failed := OpenFor(OpenAI, filepath.Join(dir, "failed.json"), []string{slugPro}, base, log)
	failed.beforeApply = func(*event) { panic("injected: BODY-CANARY") }
	failed.Offer(openAIRecord(slugPro, base).bytes(), base)
	select {
	case <-failed.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not exit")
	}
	pick(log, "error")

	want := []string{
		"quota-cache OpenAI API meter counting openai-compatibility traffic",
		"quota-cache could not read its OpenAI API meter file and starts counting again",
		"quota-cache OpenAI API meter is dropping usage records; OpenAI API credit estimates will be low",
		"quota-cache cannot save its OpenAI API meter; OpenAI API credit estimates may lose recent spend",
		"quota-cache OpenAI API meter stopped after an internal error; OpenAI API credit estimates are incomplete until Quota Cache reloads",
	}
	for _, message := range want {
		if _, logged := lines[message]; !logged {
			t.Fatalf("never logged %q: %v", message, lines)
		}
	}
	for message := range lines {
		if strings.Contains(message, "Claude") || !strings.Contains(message, "OpenAI API meter") || strings.Contains(message, "CANARY") {
			t.Fatalf("logged %q", message)
		}
	}
}

// With no organization listed the OpenAI meter watches: it counts and buckets
// nothing, and lists each organization it sees as unlinked, with its
// requests, so the page can offer it. The auth map is taught as it goes, so
// a headerless failure is noted for its organization too.
func TestTheOpenAIMeterWatchesWithNoOrganizationListed(t *testing.T) {
	m, _ := testOpenAIMeter(t, nil, base)
	feed(m, openAIRecord(slugPro, base).bytes(), base)
	for i := 0; i < 2; i++ {
		feed(m, openAIRecord(slugBusiness, base.Add(time.Second)).bytes(), base.Add(time.Second))
	}
	failure := openAIRecord("", base.Add(2*time.Second))
	failure.Failed, failure.Failure.StatusCode, failure.Failure.Body = true, 429, exhaustedBody
	feed(m, failure.bytes(), base.Add(2*time.Second))
	feed(m, openAIRecord("", base.Add(3*time.Second)).bytes(), base.Add(3*time.Second))
	if m.data.Counted != 0 || len(m.data.Organizations) != 0 || m.entries != 0 || len(m.data.Unlinked) != 2 ||
		m.data.Unlinked[0].OrganizationID != slugPro || m.data.Unlinked[0].Requests != 1 ||
		m.data.Unlinked[1].OrganizationID != slugBusiness || m.data.Unlinked[1].Requests != 3 || !m.data.Unlinked[1].LastSeenAt.Equal(base.Add(2*time.Second)) ||
		m.data.Auths["fedcba9876543210"].OrganizationID != slugBusiness {
		t.Fatalf("watching: %+v", m.data)
	}
	var exported client.APIMeter
	if err := json.Unmarshal(m.exportLimited(base.Add(time.Minute), client.MaxMeterBytes), &exported); err != nil {
		t.Fatal(err)
	}
	if len(exported.Organizations) != 0 || len(exported.Unlinked) != 2 || exported.Unlinked[0].OrganizationID != slugBusiness || exported.Received != 5 {
		t.Fatalf("export: %+v", exported)
	}
}

// An organization added while the meter watches starts counting when it is
// added: its traffic before then was not counted, so its Since is the add,
// and a consumer judges it from then, as a Claude organization added while
// the Claude meter runs for others. Removing the last organization goes back
// to watching, with the organization dormant and still bucketed, as a Claude
// organization removed while others stay listed, so that adding it again
// loses nothing.
func TestAnOpenAIOrganizationAddedWhileWatchingStartsWhenAdded(t *testing.T) {
	m, _ := testOpenAIMeter(t, nil, base)
	feed(m, openAIRecord(slugPro, base).bytes(), base)
	added := base.Add(time.Hour)
	m.SetLinked([]string{slugPro}, added)
	if o := org(t, m, slugPro); !o.Since.Equal(added) || len(o.Days) != 0 || o.LastSeenAt != nil || len(m.data.Unlinked) != 0 {
		t.Fatalf("added: %+v unlinked=%+v", o, m.data.Unlinked)
	}
	feed(m, openAIRecord(slugPro, added.Add(time.Minute)).bytes(), added.Add(time.Minute))
	if usage := org(t, m, slugPro).UsageFromDay(client.MeterDayStart(base)); m.data.Counted != 1 || len(usage) != 1 || usage[0].Requests != 1 {
		t.Fatalf("counted=%d usage=%+v", m.data.Counted, usage)
	}

	removed := added.Add(2 * time.Hour)
	m.SetLinked(nil, removed)
	feed(m, openAIRecord(slugPro, removed.Add(time.Minute)).bytes(), removed.Add(time.Minute))
	o := org(t, m, slugPro)
	if usage := o.UsageFromDay(client.MeterDayStart(base)); m.data.Counted != 1 || o.UnlinkedAt == nil || !o.UnlinkedAt.Equal(removed) || len(usage) != 1 || usage[0].Requests != 2 ||
		len(m.data.Unlinked) != 1 || m.data.Unlinked[0].OrganizationID != slugPro {
		t.Fatalf("removed: counted=%d org=%+v unlinked=%+v", m.data.Counted, o, m.data.Unlinked)
	}
	m.SetLinked([]string{slugPro}, removed.Add(time.Hour))
	if o := org(t, m, slugPro); o.UnlinkedAt != nil || !o.Since.Equal(added) || o.UsageFromDay(client.MeterDayStart(base))[0].Requests != 2 || len(m.data.Unlinked) != 0 {
		t.Fatalf("added again: %+v unlinked=%+v", o, m.data.Unlinked)
	}

	// Across a restart too: one seen only while unlisted starts when the
	// restart lists it.
	feed(m, openAIRecord(slugBusiness, removed.Add(time.Hour)).bytes(), removed.Add(time.Hour))
	m.flushIfDue(removed.Add(time.Hour + time.Minute))
	again := newMeterFor(OpenAI, m.path, &fakeLog{})
	restarted := removed.Add(3 * time.Hour)
	again.restore([]string{slugPro, slugBusiness}, restarted)
	if b := org(t, again, slugBusiness); !b.Since.Equal(restarted) || len(b.Days) != 0 || len(again.data.Unlinked) != 0 || !org(t, again, slugPro).Since.Equal(added) {
		t.Fatalf("restarted: %+v unlinked=%+v", b, again.data.Unlinked)
	}
}

// The OpenAI meter creates its file only once it has an organization to keep,
// linked or unlinked, so an install whose traffic never names one writes
// nothing beside the snapshot, and a stop before then leaves no gap to carry
// on from. Once the file exists, it is saved as any meter's is.
func TestTheOpenAIMeterWritesNoFileUntilItHasAnOrganization(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.openai-meter.json")
	m := newMeterFor(OpenAI, path, &fakeLog{})
	m.restore(nil, base)
	feed(m, apiKeyRecord(orgA, base).bytes(), base)
	feed(m, openAIRecord("", base).bytes(), base)
	feed(m, []byte("junk"), base)
	for _, at := range []time.Duration{time.Minute, 11 * time.Minute, 30 * time.Minute} {
		m.flushIfDue(base.Add(at))
	}
	noFileAt(t, path)

	first := OpenFor(OpenAI, path, nil, base, &fakeLog{})
	first.Offer(openAIRecord("", base).bytes(), base)
	first.Stop(client.MeterStopShutdown, base.Add(time.Minute))
	noFileAt(t, path)
	failed := OpenFor(OpenAI, path, nil, base, &fakeLog{})
	failed.beforeApply = func(*event) { panic("injected") }
	failed.Offer(openAIRecord("", base).bytes(), base)
	select {
	case <-failed.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not exit")
	}
	noFileAt(t, path)
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("left %v (%v)", entries, err)
	}

	// The first organization seen creates the file, with no gap or restart
	// from the runs that wrote nothing.
	second := OpenFor(OpenAI, path, nil, base.Add(time.Hour), &fakeLog{})
	second.Offer(openAIRecord(slugBusiness, base.Add(time.Hour)).bytes(), base.Add(time.Hour+time.Second))
	second.Stop(client.MeterStopShutdown, base.Add(2*time.Hour))
	meter := load(t, path)
	if !meter.Since.Equal(base.Add(time.Hour)) || len(meter.Gaps) != 0 || meter.Restarts != 0 || meter.Received != 1 || len(meter.Unlinked) != 1 || meter.StopReason != client.MeterStopShutdown {
		t.Fatalf("first file: %+v", meter)
	}
	// From then on a start with none listed carries on from it, gap and all.
	third := OpenFor(OpenAI, path, nil, base.Add(3*time.Hour), &fakeLog{})
	third.Stop(client.MeterStopShutdown, base.Add(4*time.Hour))
	if meter := load(t, path); meter.Restarts != 1 || len(meter.Gaps) != 1 || !meter.Gaps[0].From.Equal(base.Add(2*time.Hour)) || !meter.StoppedAt.Equal(base.Add(4*time.Hour)) {
		t.Fatalf("carried on: %+v", meter)
	}

	// An organization listed is one to keep: the first save writes it.
	listed, _ := testOpenAIMeter(t, []string{slugPro}, base)
	listed.flushIfDue(base.Add(time.Minute))
	if meter := load(t, listed.path); len(meter.Organizations) != 1 {
		t.Fatalf("listed: %+v", meter)
	}
	// A file that exists is kept alive even with nothing in it, and one that
	// cannot be read is replaced.
	for name, content := range map[string]string{
		"empty":      `{"schema":1,"since":"2026-10-01T00:00:00Z","started_at":"2026-10-01T00:00:00Z","flushed_at":"2026-10-09T11:00:00Z","received":0,"counted":0,"gaps":[],"organizations":{},"unlinked":[],"auths":{},"auth_ids":{}}`,
		"unreadable": "not json",
	} {
		path := filepath.Join(t.TempDir(), "snapshot.openai-meter.json")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		kept := newMeterFor(OpenAI, path, &fakeLog{})
		kept.restore(nil, base)
		kept.flushIfDue(base.Add(time.Minute))
		if meter := load(t, path); !meter.FlushedAt.Equal(base.Add(time.Minute)) || len(meter.Organizations) != 0 {
			t.Fatalf("%s: %+v", name, meter)
		}
	}
	// The Claude meter writes its file at the first save whatever it holds.
	claude, _ := testMeter(t, nil, base)
	claude.flushIfDue(base.Add(time.Minute))
	load(t, claude.path)
}
