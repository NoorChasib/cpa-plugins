package meter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// The spec's fake organization ids. Nothing here is a real organization, key
// or account.
const (
	orgA = "00000000-0000-4000-8000-00000000000a"
	orgB = "00000000-0000-4000-8000-00000000000b"
	orgC = "00000000-0000-4000-8000-00000000000c"
)

var base = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// Canaries a record carries in the fields the meter must never read.
var canaries = []string{"SOURCE-CANARY", "APIKEY-CANARY", "AUTHID-CANARY", "BASEURL-CANARY", "SESSION-CANARY", "PARENT-CANARY", "BODY-CANARY", "HEADER-VALUE-CANARY", "ALIAS-CANARY", "req_"}

type logLine struct {
	level, message string
	fields         map[string]any
}

type fakeLog struct {
	mu    sync.Mutex
	lines []logLine
}

func (l *fakeLog) Log(_ context.Context, level, message string, fields map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, logLine{level, message, fields})
}

func (l *fakeLog) logged() []logLine {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]logLine(nil), l.lines...)
}

func (l *fakeLog) text() string {
	return fmt.Sprintf("%+v", l.logged())
}

// wire is a usage record as CPA marshals pluginapi.UsageRecord: Go field
// names, no tags. It carries the fields the meter must never read, with
// canaries in them.
type wire struct {
	Provider, AuthIndex, AuthType, Model, ResponseModel, SessionID string
	Source, APIKey, AuthID, BaseURL, Alias, ParentSessionID        string
	RequestedAt                                                    any
	Latency                                                        int64
	Failed                                                         bool
	Failure                                                        struct {
		StatusCode int
		Body       string
	}
	Detail          struct{ InputTokens, OutputTokens, CacheReadTokens, CacheCreationTokens int64 }
	ResponseHeaders map[string][]string
}

// apiKeyRecord is a successful Claude API-key record for org, requested at
// at with no latency, carrying 1000 input and 100 output tokens.
func apiKeyRecord(org string, at time.Time) wire {
	w := wire{Provider: "claude", AuthIndex: "0123456789abcdef", AuthType: "apikey", Model: "claude-sonnet-5-5",
		RequestedAt: at.Format(time.RFC3339Nano), Source: "sk-ant-api03-SOURCE-CANARY", APIKey: "sk-cpa-APIKEY-CANARY",
		AuthID: "AUTHID-CANARY.json", BaseURL: "https://BASEURL-CANARY", Alias: "ALIAS-CANARY", ParentSessionID: "claude:PARENT-CANARY",
		ResponseHeaders: map[string][]string{"Request-Id": {"req_HEADER-CANARY"}, "X-Canary": {"HEADER-VALUE-CANARY"}}}
	if org != "" {
		w.ResponseHeaders["Anthropic-Organization-Id"] = []string{org}
	}
	w.Detail.InputTokens, w.Detail.OutputTokens = 1000, 100
	return w
}

func (w wire) bytes() []byte {
	raw, err := json.Marshal(w)
	if err != nil {
		panic(err)
	}
	return raw
}

// testMeter is a meter restored from a fresh path with no worker, so a test
// applies events and saves when it chooses.
func testMeter(t *testing.T, linked []string, now time.Time) (*Meter, *fakeLog) {
	t.Helper()
	log := &fakeLog{}
	m := newMeter(filepath.Join(t.TempDir(), "snapshot.meter.json"), log)
	m.restore(linked, now)
	return m, log
}

// feed offers raw at received and applies it at once, as the worker would.
func feed(m *Meter, raw []byte, received time.Time) {
	m.Offer(raw, received)
	m.drain()
}

func org(t *testing.T, m *Meter, id string) client.MeterOrganization {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.data.Organizations[id]
	if !ok {
		t.Fatalf("organization %s is not in the meter", id)
	}
	return o
}

func load(t *testing.T, path string) *client.APIMeter {
	t.Helper()
	meter, err := client.LoadMeter(path)
	if err != nil {
		t.Fatalf("meter file: %v", err)
	}
	return meter
}

func checkEntries(t *testing.T, m *Meter) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if got := countEntries(m.data); got != m.entries {
		t.Fatalf("entries counter %d, counted %d", m.entries, got)
	}
}

// The record CPA v8.0.22 produces, with every field set. Decoding it yields
// the expected event, and every key the meter reads is in the bytes, so a
// field renamed upstream fails here rather than silently counting nothing.
func TestGoldenRecordDecodesAndNamesEveryFieldRead(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "usage-record-v8.0.22.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"Provider", "AuthIndex", "AuthType", "Model", "ResponseModel", "SessionID", "RequestedAt", "Latency", "Failed",
		"Failure", "StatusCode", "Body", "Detail", "InputTokens", "OutputTokens", "CacheReadTokens", "CacheCreationTokens", "ResponseHeaders", "Anthropic-Organization-Id"} {
		if !strings.Contains(string(raw), `"`+key+`":`) {
			t.Errorf("the golden record has no %q key; the field the meter reads has moved", key)
		}
	}
	received := base.Add(5 * time.Second)
	e, keep, rejected := decode(raw, received)
	want := event{at: base.Add(time.Second), authIndex: "0123456789abcdef", org: orgA, model: "claude-sonnet-5-5-20260301",
		failed: true, refusal: true, claudeCode: true, input: 1200, output: 30, cacheRead: 4000, cacheWrite: 500}
	if !keep || rejected || e != want {
		t.Fatalf("event=%+v keep=%v rejected=%v\nwant %+v", e, keep, rejected, want)
	}
	for _, canary := range canaries {
		if printed := fmt.Sprintf("%+v %#v", e, e); strings.Contains(printed, canary) {
			t.Fatalf("the event carries %q: %s", canary, printed)
		}
	}
}

// A record's secrets and free text never reach the file, the log or an
// event, however it is formatted.
func TestCanariesNeverLeaveTheRecord(t *testing.T) {
	log := &fakeLog{}
	path := filepath.Join(t.TempDir(), "snapshot.meter.json")
	m := Open(path, []string{orgA}, base, log)
	w := apiKeyRecord(orgA, base)
	w.SessionID, w.Failed = "claude:SESSION-CANARY", true
	w.Failure.StatusCode, w.Failure.Body = 400, `{"error":{"message":"Your credit balance is too low ... BODY-CANARY"},"request_id":"req_BODYCANARY"}`
	m.Offer(w.bytes(), base.Add(time.Second))
	m.Offer(apiKeyRecord("", base).bytes(), base.Add(2*time.Second))
	m.Stop(client.MeterStopShutdown, base.Add(3*time.Second))
	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for what, text := range map[string]string{"file": string(file), "log": log.text()} {
		for _, canary := range canaries {
			if strings.Contains(text, canary) {
				t.Fatalf("the %s carries %q", what, canary)
			}
		}
	}
	// The headerless success rode on the auth the first record taught.
	meter := load(t, path)
	if o := meter.Organizations[orgA]; o.ClaudeCodeRefusals != 1 || meter.Counted != 2 {
		t.Fatalf("the refusal was not counted: %+v", meter)
	}
}

// Only Claude API-key records are counted. An OAuth record, Claude's
// subscription traffic, is dropped before its headers are read, so the
// claude.ai organization they carry is never counted, listed or learned.
func TestOnlyClaudeAPIKeyRecordsAreCounted(t *testing.T) {
	m, _ := testMeter(t, []string{orgA}, base)
	oauth := apiKeyRecord(orgA, base)
	oauth.AuthType = "oauth"
	other := apiKeyRecord(orgB, base)
	other.Provider = "openai"
	kimi := apiKeyRecord(orgC, base)
	kimi.Provider, kimi.AuthType = "claude", "oauth"
	for _, w := range []wire{oauth, other, kimi} {
		feed(m, w.bytes(), base)
	}
	if m.received.Load() != 3 || m.data.Counted != 0 || len(m.data.Auths) != 0 || len(m.data.Unlinked) != 0 || org(t, m, orgA).LastSeenAt != nil {
		t.Fatalf("received=%d data=%+v", m.received.Load(), m.data)
	}
	upper := apiKeyRecord(orgA, base)
	upper.Provider, upper.AuthType = "Claude", "APIKey"
	feed(m, upper.bytes(), base)
	if m.data.Counted != 1 {
		t.Fatal("provider and auth type are matched case-insensitively")
	}
}

func TestHeaderModelPromptRefusalAndSessionAreRead(t *testing.T) {
	m, _ := testMeter(t, []string{orgA, orgB}, base)
	lower := apiKeyRecord("", base)
	lower.ResponseHeaders["anthropic-organization-id"] = []string{strings.ToUpper(orgA)}
	feed(m, lower.bytes(), base)
	if o := org(t, m, orgA); o.LastSeenAt == nil || m.data.Auths["0123456789abcdef"].OrganizationID != orgA {
		t.Fatalf("a lower-cased header with an upper-cased id was not read: %+v auths=%v", o, m.data.Auths)
	}
	// An invalid header is absent: this failure on an unknown auth is
	// unattributed.
	invalid := apiKeyRecord("{"+orgB+"}", base)
	invalid.AuthIndex, invalid.Failed = "fedcba9876543210", true
	feed(m, invalid.bytes(), base)
	if m.data.Unattributed != 1 || m.data.LastUnattributedAt == nil || org(t, m, orgB).LastSeenAt != nil {
		t.Fatalf("an invalid header was used: %+v", m.data)
	}

	response := apiKeyRecord(orgA, base)
	response.ResponseModel = "Claude-Haiku-5-5-20260401"
	response.Detail.InputTokens, response.Detail.CacheReadTokens, response.Detail.CacheCreationTokens = 50_000, 50_000, 1
	feed(m, response.bytes(), base)
	usage := org(t, m, orgA).UsageFromDay(client.MeterDayStart(base))
	if len(usage) != 2 || usage[0].Model != "claude-haiku-5-5-20260401" || usage[0].Prompt != client.MeterPromptOver100K || usage[1].Model != "claude-sonnet-5-5" {
		t.Fatalf("usage=%+v; the response model is preferred, lower-cased, and classed by prompt size", usage)
	}

	for _, tc := range []struct {
		name       string
		failed     bool
		status     int
		body       string
		session    string
		refusals   uint64
		claudeCode uint64
	}{
		{"documented 400", true, 400, `{"error":{"message":"Your credit balance is too low to access the Anthropic API."}}`, "", 1, 0},
		{"stream error event", true, 502, "claude executor: upstream returned error event: Credit Balance Too Low", "", 2, 0},
		{"Claude Code's own words", true, 400, "Credit balance too low · Add funds", "claude:abc", 2, 1},
		{"a success is never a refusal", false, 200, "credit balance too low", "", 2, 1},
		{"other failures", true, 429, "rate limited", "claude:abc", 2, 1},
	} {
		w := apiKeyRecord(orgB, base)
		w.Failed, w.Failure.StatusCode, w.Failure.Body, w.SessionID = tc.failed, tc.status, tc.body, tc.session
		feed(m, w.bytes(), base)
		if o := org(t, m, orgB); o.Refusals != tc.refusals || o.ClaudeCodeRefusals != tc.claudeCode {
			t.Fatalf("%s: refusals=%d claude code=%d", tc.name, o.Refusals, o.ClaudeCodeRefusals)
		}
	}
	o := org(t, m, orgB)
	if o.LastRefusalAt == nil || o.LastClaudeCodeRefusalAt == nil || o.LastSuccessAt == nil || !o.LastSuccessAt.Equal(base) {
		t.Fatalf("times: %+v", o)
	}
}

// The completion time is the request time plus the latency when both are
// plausible, else the received time, and never after the received time.
func TestCompletionTimeIsClampedAndABadTimestampIsNotRejected(t *testing.T) {
	received := base.Add(10 * time.Second)
	for _, tc := range []struct {
		name        string
		requestedAt any
		latency     int64
		want        time.Time
	}{
		{"request plus latency", base.Format(time.RFC3339Nano), int64(1500 * time.Millisecond), base.Add(time.Second)},
		{"never after received", base.Format(time.RFC3339Nano), int64(time.Minute), received},
		{"a little ahead of the clock", received.Add(4 * time.Minute).Format(time.RFC3339), 0, received},
		{"too far ahead", received.Add(6 * time.Minute).Format(time.RFC3339), 0, received},
		{"a day old", received.Add(-23 * time.Hour).Format(time.RFC3339), 0, received.Add(-23 * time.Hour)},
		{"too old", received.Add(-25 * time.Hour).Format(time.RFC3339), 0, received},
		{"negative latency", base.Format(time.RFC3339Nano), -1, received},
		{"text", "x", 0, received},
		{"a number", 123, 0, received},
		{"null", nil, 0, received},
		{"fractional seconds", base.Add(300 * time.Millisecond).Format(time.RFC3339Nano), int64(900 * time.Millisecond), base.Add(time.Second)},
	} {
		w := apiKeyRecord(orgA, base)
		w.RequestedAt, w.Latency = tc.requestedAt, tc.latency
		e, keep, rejected := decode(w.bytes(), received)
		if !keep || rejected || !e.at.Equal(tc.want) {
			t.Errorf("%s: at=%v keep=%v rejected=%v; want %v", tc.name, e.at, keep, rejected, tc.want)
		}
	}
}

func TestUnreadableRecordsAndNegativeCountsAreRejected(t *testing.T) {
	m, _ := testMeter(t, []string{orgA}, base)
	negative := apiKeyRecord(orgA, base)
	negative.Detail.CacheReadTokens = -1
	for _, raw := range [][]byte{[]byte("not json"), []byte(`[]`), []byte(`{"Latency":"slow"}`), negative.bytes()} {
		feed(m, raw, base)
	}
	if m.received.Load() != 4 || m.rejected.Load() != 4 || m.lastRejectedAt.Load() != base.Unix() || m.data.Counted != 0 {
		t.Fatalf("received=%d rejected=%d at=%d", m.received.Load(), m.rejected.Load(), m.lastRejectedAt.Load())
	}
	raw := m.exportLimited(base, client.MaxMeterBytes)
	if !strings.Contains(string(raw), `"rejected":4,"last_rejected_at":"2026-10-09T12:00:00Z"`) {
		t.Fatalf("export: %s", raw)
	}
}

// With the worker paused and the intake full, Offer returns at once and
// counts the drop; the warning comes from the worker's flush, at most once
// per ten minutes.
func TestOfferNeverBlocksAndDropsAreWarnedSparingly(t *testing.T) {
	m, log := testMeter(t, []string{orgA}, base)
	raw := apiKeyRecord(orgA, base).bytes()
	for i := 0; i < intakeSize; i++ {
		m.Offer(raw, base)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Offer(raw, base.Add(time.Second))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Offer blocked on a full intake")
	}
	if m.dropped.Load() != 1 || m.lastDroppedAt.Load() != base.Add(time.Second).Unix() || m.received.Load() != intakeSize+1 {
		t.Fatalf("dropped=%d at=%d received=%d", m.dropped.Load(), m.lastDroppedAt.Load(), m.received.Load())
	}
	m.flushIfDue(base.Add(time.Minute))
	m.Offer(raw, base.Add(2*time.Second))
	m.flushIfDue(base.Add(2 * time.Minute))
	m.flushIfDue(base.Add(9 * time.Minute))
	m.flushIfDue(base.Add(11 * time.Minute))
	m.flushIfDue(base.Add(12 * time.Minute))
	warnings := dropWarnings(log)
	if len(warnings) != 2 || warnings[0].level != "warn" || warnings[0].fields["dropped"] != uint64(1) || warnings[1].fields["dropped"] != uint64(2) ||
		warnings[0].message != "quota-cache API meter is dropping usage records; Claude API credit estimates will be low" {
		t.Fatalf("warnings=%+v", warnings)
	}
	if meter := load(t, m.path); meter.Dropped != 2 || meter.LastDroppedAt == nil {
		t.Fatalf("file: dropped=%d at=%v", meter.Dropped, meter.LastDroppedAt)
	}
	// A restart carries the file's drops on, but not the warning: they were
	// warned about when they happened. The next warning needs a drop in
	// this run, and then names the whole count.
	restarted := &fakeLog{}
	again := newMeter(m.path, restarted)
	again.restore([]string{orgA}, base.Add(time.Hour))
	again.flushIfDue(base.Add(time.Hour + time.Minute))
	if warned := dropWarnings(restarted); len(warned) != 0 || again.dropped.Load() != 2 {
		t.Fatalf("warned about the file's drops: %+v dropped=%d", warned, again.dropped.Load())
	}
	for i := 0; i <= intakeSize; i++ {
		again.Offer(raw, base.Add(time.Hour+2*time.Second))
	}
	again.flushIfDue(base.Add(time.Hour + 2*time.Minute))
	if warned := dropWarnings(restarted); len(warned) != 1 || warned[0].fields["dropped"] != uint64(3) {
		t.Fatalf("warnings after a drop in this run: %+v", warned)
	}
}

// dropWarnings is every drop warning log holds, in order.
func dropWarnings(log *fakeLog) []logLine {
	warnings := []logLine{}
	for _, line := range log.logged() {
		if strings.Contains(line.message, "dropping") {
			warnings = append(warnings, line)
		}
	}
	return warnings
}

func TestOfferDuringStopNeitherPanicsNorRaces(t *testing.T) {
	m := Open(filepath.Join(t.TempDir(), "snapshot.meter.json"), []string{orgA}, base, &fakeLog{})
	raw := apiKeyRecord(orgA, base).bytes()
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 2000; j++ {
				m.Offer(raw, base.Add(time.Duration(j)*time.Millisecond))
			}
		}()
	}
	close(start)
	time.Sleep(time.Millisecond)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		m.Stop(client.MeterStopQuiesce, base.Add(time.Minute))
	}()
	wg.Wait()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return")
	}
	if !m.stopped.Load() {
		t.Fatal("not stopped")
	}
	// A record offered as the worker drains can land after the drain and is
	// lost, as a record queued at process exit is; it is never miscounted.
	meter := load(t, m.path)
	if meter.StoppedAt == nil || meter.StopReason != client.MeterStopQuiesce || meter.Received == 0 || meter.Counted == 0 || meter.Counted+meter.Dropped > meter.Received {
		t.Fatalf("file: %+v", meter)
	}
}

func TestEventsAreSummedPerOrganizationDayHourModelAndPrompt(t *testing.T) {
	m, _ := testMeter(t, []string{orgA, orgB}, base)
	day1, day2 := base, base.Add(24*time.Hour)
	for _, step := range []struct {
		org    string
		at     time.Time
		model  string
		input  int64
		failed bool
	}{
		{orgA, day1, "claude-sonnet-5-5", 100, false},
		{orgA, day1.Add(30 * time.Minute), "claude-sonnet-5-5", 200, false},
		{orgA, day1.Add(time.Hour), "claude-opus-5-5", 300, false},
		{orgA, day1.Add(time.Hour), "claude-sonnet-5-5", 400, true},
		{orgB, day1, "claude-sonnet-5-5", 500, false},
		{orgA, day2, "claude-sonnet-5-5", 600, false},
	} {
		w := apiKeyRecord(step.org, step.at)
		w.Model, w.Detail.InputTokens, w.Detail.OutputTokens, w.Detail.CacheReadTokens, w.Detail.CacheCreationTokens, w.Failed = step.model, step.input, 10, 20, 30, step.failed
		feed(m, w.bytes(), step.at)
	}
	a := org(t, m, orgA)
	if len(a.Days) != 2 || len(a.Hours) != 3 || !a.Days[0].Start.Equal(client.MeterDayStart(day1)) || !a.Hours[1].Start.Equal(day1.Add(time.Hour)) {
		t.Fatalf("buckets: days=%+v hours=%+v", a.Days, a.Hours)
	}
	want := []client.MeterUsage{
		{Model: "claude-opus-5-5", Requests: 1, Input: 300, Output: 10, CacheRead: 20, CacheWrite: 30},
		{Model: "claude-sonnet-5-5", Requests: 2, Failed: 1, Input: 700, Output: 30, CacheRead: 60, CacheWrite: 90},
	}
	if got := a.Days[0].Usage; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("day usage=%+v want %+v", got, want)
	}
	if got := a.Hours[1].Usage; fmt.Sprint(got) != fmt.Sprint([]client.MeterUsage{want[0], {Model: "claude-sonnet-5-5", Failed: 1, Input: 400, Output: 10, CacheRead: 20, CacheWrite: 30}}) {
		t.Fatalf("hour usage=%+v", got)
	}
	// orgA: 3 day entries and 4 hour entries; orgB: one of each.
	if b := org(t, m, orgB); len(b.Days) != 1 || b.Days[0].Usage[0].Input != 500 || m.data.Counted != 6 || m.entries != 9 {
		t.Fatalf("orgB=%+v counted=%d entries=%d", b, m.data.Counted, m.entries)
	}
	checkEntries(t, m)
	// The hours of a day sum to the day: the baseline a reading takes is
	// always a subset of the daily sum.
	hours, covered := a.UsageInHours(client.MeterDayStart(day1), base.Add(2*time.Hour), base.Add(time.Hour))
	if !covered || fmt.Sprint(hours) != fmt.Sprint(want) {
		t.Fatalf("hours=%+v covered=%v", hours, covered)
	}
}

// The auth map lets a failure that arrives without the organization header
// still be attributed, and marks an auth whose successes carry none as a
// service that is not Anthropic's.
func TestAuthMapLearnsForeignAndUnattributedRecords(t *testing.T) {
	m, _ := testMeter(t, []string{orgA}, base)
	feed(m, apiKeyRecord(orgA, base).bytes(), base)
	headless := apiKeyRecord("", base.Add(time.Second))
	headless.Failed = true
	feed(m, headless.bytes(), base.Add(time.Second))
	if a := org(t, m, orgA); a.LastSeenAt == nil || !a.LastSeenAt.Equal(base.Add(time.Second)) || m.data.Counted != 2 || !m.data.Auths["0123456789abcdef"].SeenAt.Equal(base.Add(time.Second)) {
		t.Fatalf("a headerless failure on a known auth was not attributed: %+v auths=%v", a, m.data.Auths)
	}

	// A headerless success on an unknown auth is not Anthropic traffic.
	foreign := apiKeyRecord("", base)
	foreign.AuthIndex = "ffffffffffffffff"
	feed(m, foreign.bytes(), base)
	foreignFailure := foreign
	foreignFailure.Failed = true
	feed(m, foreignFailure.bytes(), base)
	if m.data.Foreign != 2 || m.data.Unattributed != 0 || m.data.Auths["ffffffffffffffff"].OrganizationID != "" {
		t.Fatalf("foreign=%d unattributed=%d auths=%v", m.data.Foreign, m.data.Unattributed, m.data.Auths)
	}
	// A later headed record on that auth re-links it.
	relink := apiKeyRecord(orgA, base.Add(2*time.Second))
	relink.AuthIndex = "ffffffffffffffff"
	feed(m, relink.bytes(), base.Add(2*time.Second))
	feed(m, foreignFailure.bytes(), base.Add(3*time.Second))
	if m.data.Auths["ffffffffffffffff"].OrganizationID != orgA || m.data.Counted != 4 || m.data.Foreign != 2 {
		t.Fatalf("auths=%v counted=%d foreign=%d", m.data.Auths, m.data.Counted, m.data.Foreign)
	}

	// A headerless failure on an unknown auth is unattributed; only one
	// that carried tokens dates it.
	unknown := apiKeyRecord("", base.Add(4*time.Second))
	unknown.AuthIndex, unknown.Failed = "1111111111111111", true
	unknown.Detail.InputTokens, unknown.Detail.OutputTokens = 0, 0
	feed(m, unknown.bytes(), base.Add(4*time.Second))
	if m.data.Unattributed != 1 || m.data.LastUnattributedAt != nil {
		t.Fatalf("unattributed=%d at=%v", m.data.Unattributed, m.data.LastUnattributedAt)
	}
	unknown.Detail.CacheCreationTokens = 1
	feed(m, unknown.bytes(), base.Add(5*time.Second))
	if m.data.Unattributed != 2 || m.data.LastUnattributedAt == nil || !m.data.LastUnattributedAt.Equal(base.Add(4*time.Second)) || len(m.data.Auths) != 2 {
		t.Fatalf("unattributed=%d at=%v auths=%v", m.data.Unattributed, m.data.LastUnattributedAt, m.data.Auths)
	}
	// No auth index at all: attributed by header only, never learned.
	none := apiKeyRecord(orgA, base)
	none.AuthIndex = ""
	feed(m, none.bytes(), base)
	long := apiKeyRecord(orgA, base)
	long.AuthIndex = strings.Repeat("a", maxAuthIndex+1)
	feed(m, long.bytes(), base)
	if len(m.data.Auths) != 2 || m.data.Counted != 6 {
		t.Fatalf("auths=%v counted=%d", m.data.Auths, m.data.Counted)
	}
}

func TestUnlinkedAndAuthsAreCapped(t *testing.T) {
	m, _ := testMeter(t, []string{orgA}, base)
	for i := 0; i <= client.MaxMeterUnlinked; i++ {
		w := apiKeyRecord(fmt.Sprintf("00000000-0000-4000-8000-%012x", 0x100+i), base.Add(time.Duration(i)*time.Second))
		feed(m, w.bytes(), base.Add(time.Duration(i)*time.Second))
	}
	if len(m.data.Unlinked) != client.MaxMeterUnlinked || m.data.Unlinked[0].OrganizationID != fmt.Sprintf("00000000-0000-4000-8000-%012x", 0x101) || m.data.Counted != 0 {
		t.Fatalf("unlinked=%+v; the oldest is evicted", m.data.Unlinked)
	}
	for i := 0; i <= client.MaxMeterAuths; i++ {
		w := apiKeyRecord(orgA, base.Add(time.Duration(i)*time.Second))
		w.AuthIndex = fmt.Sprintf("%016x", i)
		feed(m, w.bytes(), base.Add(time.Duration(i)*time.Second))
	}
	if _, oldest := m.data.Auths["0000000000000000"]; len(m.data.Auths) != client.MaxMeterAuths || oldest {
		t.Fatalf("%d auths, oldest kept=%v", len(m.data.Auths), oldest)
	}
	raw := m.exportLimited(base.Add(time.Hour), client.MaxMeterBytes)
	var exported client.APIMeter
	if err := json.Unmarshal(raw, &exported); err != nil {
		t.Fatal(err)
	}
	if exported.Unlinked[0].OrganizationID != fmt.Sprintf("00000000-0000-4000-8000-%012x", 0x108) {
		t.Fatalf("exported unlinked=%+v; newest first", exported.Unlinked)
	}
}

// Sixteen distinct (model, prompt) pairs in a bucket stay under their own
// names; the seventeenth goes to (other) in the day and the hour bucket
// alike, and marks the organization overflowed.
func TestASeventeenthPairInABucketBecomesOther(t *testing.T) {
	m, _ := testMeter(t, []string{orgA}, base)
	models := []string{}
	for _, haiku := range []string{"claude-haiku-5-5", "claude-haiku-5-5-20260401", "claude-haiku-5-5-latest"} {
		models = append(models, haiku, haiku+"@big")
	}
	for i := 0; i < 10; i++ {
		models = append(models, fmt.Sprintf("claude-model-%d", i))
	}
	for i, model := range models {
		w := apiKeyRecord(orgA, base.Add(time.Duration(i)*time.Second))
		w.Model = strings.TrimSuffix(model, "@big")
		if strings.HasSuffix(model, "@big") {
			w.Detail.InputTokens = 200_000
		}
		feed(m, w.bytes(), base.Add(time.Duration(i)*time.Second))
	}
	a := org(t, m, orgA)
	if len(a.Days[0].Usage) != 16 || len(a.Hours[0].Usage) != 16 || a.LastOverflowAt != nil || m.entries != 32 {
		t.Fatalf("%d day pairs, %d hour pairs, overflow=%v entries=%d", len(a.Days[0].Usage), len(a.Hours[0].Usage), a.LastOverflowAt, m.entries)
	}
	for _, u := range a.Days[0].Usage {
		if u.Model == client.MeterOtherModel {
			t.Fatal("sixteen pairs must stay fully priced")
		}
	}
	seventeenth := apiKeyRecord(orgA, base.Add(time.Minute))
	seventeenth.Model = "claude-model-10"
	feed(m, seventeenth.bytes(), base.Add(time.Minute))
	feed(m, seventeenth.bytes(), base.Add(time.Minute))
	a = org(t, m, orgA)
	if a.LastOverflowAt == nil || !a.LastOverflowAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("overflow=%v", a.LastOverflowAt)
	}
	for _, usage := range [][]client.MeterUsage{a.Days[0].Usage, a.Hours[0].Usage} {
		if len(usage) != 17 || usage[0].Model != client.MeterOtherModel || usage[0].Requests != 2 || usage[0].Input != 2000 {
			t.Fatalf("usage=%+v", usage)
		}
	}
	// A pair already in the bucket still lands under its own name.
	known := apiKeyRecord(orgA, base.Add(2*time.Minute))
	known.Model = "claude-model-0"
	feed(m, known.bytes(), base.Add(2*time.Minute))
	a = org(t, m, orgA)
	if len(a.Days[0].Usage) != 17 || a.Days[0].Usage[0].Requests != 2 || a.Days[0].Usage[7].Model != "claude-model-0" || a.Days[0].Usage[7].Requests != 2 {
		t.Fatalf("usage=%+v", a.Days[0].Usage)
	}
	checkEntries(t, m)
}

// At the global cap an event is counted but bucketed in neither bucket, and
// no empty bucket is left behind.
func TestTheEntryCapSkipsBothBuckets(t *testing.T) {
	m, _ := testMeter(t, []string{orgA}, base)
	feed(m, apiKeyRecord(orgA, base).bytes(), base)
	m.entries = client.MaxMeterUsageEntries - 1
	// A new hour, so two entries are needed and only one fits.
	later := apiKeyRecord(orgA, base.Add(2*time.Hour))
	later.Model = "claude-opus-5-5"
	feed(m, later.bytes(), base.Add(2*time.Hour))
	a := org(t, m, orgA)
	if len(a.Days) != 1 || len(a.Hours) != 1 || len(a.Days[0].Usage) != 1 || a.LastOverflowAt == nil || m.data.Counted != 2 || m.entries != client.MaxMeterUsageEntries-1 {
		t.Fatalf("org=%+v entries=%d", a, m.entries)
	}
	// An existing pair needs no entry and is still added.
	feed(m, apiKeyRecord(orgA, base.Add(time.Minute)).bytes(), base.Add(time.Minute))
	if a = org(t, m, orgA); a.Days[0].Usage[0].Requests != 2 {
		t.Fatalf("usage=%+v", a.Days[0].Usage)
	}
}

func TestBucketsOrganizationsAndListsAgeOut(t *testing.T) {
	old := base.Add(-30 * 24 * time.Hour)
	m, _ := testMeter(t, []string{orgA}, old)
	feed(m, apiKeyRecord(orgA, old).bytes(), old)
	feed(m, apiKeyRecord(orgA, base).bytes(), base)
	stray := apiKeyRecord(orgB, base)
	stray.AuthIndex = "bbbbbbbbbbbbbbbb"
	feed(m, stray.bytes(), base)
	m.SetLinked([]string{orgA, orgC}, base)
	m.SetLinked([]string{orgA}, base.Add(time.Second))
	if a := org(t, m, orgA); len(a.Days) != 2 || len(a.Hours) != 1 {
		t.Fatalf("days=%d hours=%d; the hour thirty days back is already gone", len(a.Days), len(a.Hours))
	}
	// Seventy-three hours on, the hour at base is past the seventy-two kept.
	at := base.Add(73 * time.Hour)
	feed(m, apiKeyRecord(orgA, at).bytes(), at)
	a := org(t, m, orgA)
	if len(a.Hours) != 1 || !a.Hours[0].Start.Equal(client.MeterHourStart(at)) || len(a.Days) != 3 {
		t.Fatalf("hours=%+v days=%d", a.Hours, len(a.Days))
	}
	// Eleven days on, the old day is past the forty kept; the rest stays.
	at = base.Add(11 * 24 * time.Hour)
	feed(m, apiKeyRecord(orgA, at).bytes(), at)
	a = org(t, m, orgA)
	if len(a.Days) != 3 || !a.Days[0].Start.Equal(client.MeterDayStart(base)) || len(a.Hours) != 1 || len(m.data.Unlinked) != 1 || len(m.data.Auths) != 2 {
		t.Fatalf("days=%+v hours=%d unlinked=%d auths=%d", a.Days, len(a.Hours), len(m.data.Unlinked), len(m.data.Auths))
	}
	if _, dormant := m.data.Organizations[orgC]; !dormant {
		t.Fatal("the dormant organization aged out early")
	}
	// Forty days on, the dormant organization, the unlinked entry and the
	// auth last seen at base are gone, and so is base's day.
	at = base.Add(41 * 24 * time.Hour)
	feed(m, apiKeyRecord(orgA, at).bytes(), at)
	if _, dormant := m.data.Organizations[orgC]; dormant || len(m.data.Unlinked) != 0 || len(m.data.Auths) != 1 || len(org(t, m, orgA).Days) != 3 || !org(t, m, orgA).Days[0].Start.Equal(client.MeterDayStart(base.Add(73*time.Hour))) {
		t.Fatalf("dormant=%v unlinked=%d auths=%d days=%+v", dormant, len(m.data.Unlinked), len(m.data.Auths), org(t, m, orgA).Days)
	}
	checkEntries(t, m)
}

// Past the cap, the oldest brief gap goes first; only when every gap is
// long are the two oldest merged, and never two brief ones into a long one.
func TestGapsAreCappedBriefOnesFirst(t *testing.T) {
	gap := func(from, to time.Duration, reason string) client.MeterGap {
		return client.MeterGap{From: base.Add(from), To: base.Add(to), Reason: reason}
	}
	long := []client.MeterGap{}
	for i := 0; i <= client.MaxMeterGaps; i++ {
		long = append(long, gap(time.Duration(i)*time.Hour, time.Duration(i)*time.Hour+10*time.Minute, client.MeterStopDisabled))
	}
	merged := capGaps(append([]client.MeterGap(nil), long...))
	if len(merged) != client.MaxMeterGaps || !merged[0].From.Equal(long[0].From) || !merged[0].To.Equal(long[1].To) || merged[0].Reason != long[1].Reason || !merged[1].From.Equal(long[2].From) {
		t.Fatalf("merged=%+v", merged)
	}
	mixed := append([]client.MeterGap(nil), long[:client.MaxMeterGaps-1]...)
	mixed = append(mixed[:2], append([]client.MeterGap{gap(90*time.Minute, 90*time.Minute+5*time.Second, client.MeterStopShutdown), gap(100*time.Minute, 100*time.Minute+time.Second, client.MeterStopQuiesce)}, mixed[2:]...)...)
	if len(mixed) != client.MaxMeterGaps+1 {
		t.Fatalf("%d gaps", len(mixed))
	}
	kept := capGaps(append([]client.MeterGap(nil), mixed...))
	if len(kept) != client.MaxMeterGaps || kept[2].Reason != client.MeterStopQuiesce || !kept[0].To.Equal(long[0].To) {
		t.Fatalf("kept=%+v; the oldest brief gap goes, nothing is merged", kept)
	}
	for _, g := range kept {
		if g.Brief() && g.To.Sub(g.From) != time.Second {
			t.Fatalf("a brief gap was merged into %+v", g)
		}
	}
	// Through the meter: nine restores leave eight gaps.
	path := filepath.Join(t.TempDir(), "snapshot.meter.json")
	log := &fakeLog{}
	for i := 0; i <= client.MaxMeterGaps; i++ {
		at := base.Add(time.Duration(i) * time.Hour)
		m := newMeter(path, log)
		m.restore([]string{orgA}, at)
		go m.work()
		m.Stop(client.MeterStopDisabled, at.Add(time.Second))
	}
	if meter := load(t, path); len(meter.Gaps) != client.MaxMeterGaps || meter.Restarts != client.MaxMeterGaps {
		t.Fatalf("gaps=%d restarts=%d", len(meter.Gaps), meter.Restarts)
	}
}

func TestLinkingKeepsHistoryThroughTypoAndFix(t *testing.T) {
	m, _ := testMeter(t, []string{orgA}, base)
	feed(m, apiKeyRecord(orgA, base).bytes(), base)
	since := org(t, m, orgA).Since
	// A typo: orgA is no longer listed. Its traffic is still bucketed, it is
	// listed as unlinked, and it is not counted.
	m.SetLinked([]string{orgB}, base.Add(time.Minute))
	a := org(t, m, orgA)
	if a.UnlinkedAt == nil || !a.UnlinkedAt.Equal(base.Add(time.Minute)) || org(t, m, orgB).Since != base.Add(time.Minute) {
		t.Fatalf("orgA=%+v orgB=%+v", a, org(t, m, orgB))
	}
	feed(m, apiKeyRecord(orgA, base.Add(2*time.Minute)).bytes(), base.Add(2*time.Minute))
	a = org(t, m, orgA)
	if m.data.Counted != 1 || a.Days[0].Usage[0].Requests != 2 || len(m.data.Unlinked) != 1 || m.data.Unlinked[0].OrganizationID != orgA || m.data.Unlinked[0].Requests != 1 || a.LastSeenAt == nil || !a.LastSeenAt.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("counted=%d org=%+v unlinked=%+v", m.data.Counted, a, m.data.Unlinked)
	}
	// The fix: relinked with its Since and buckets, and off the unlinked list.
	m.SetLinked([]string{orgA, orgB}, base.Add(3*time.Minute))
	a = org(t, m, orgA)
	if a.UnlinkedAt != nil || !a.Since.Equal(since) || a.Days[0].Usage[0].Requests != 2 || len(m.data.Unlinked) != 0 {
		t.Fatalf("relinked=%+v unlinked=%+v", a, m.data.Unlinked)
	}
	feed(m, apiKeyRecord(orgA, base.Add(4*time.Minute)).bytes(), base.Add(4*time.Minute))
	if m.data.Counted != 2 || org(t, m, orgA).Days[0].Usage[0].Requests != 3 {
		t.Fatalf("counted=%d usage=%+v", m.data.Counted, org(t, m, orgA).Days[0].Usage)
	}
	// The same list again changes nothing.
	m.dirty = false
	m.SetLinked([]string{orgB, orgA}, base.Add(5*time.Minute))
	if m.dirty {
		t.Fatal("an unchanged list dirtied the meter")
	}
}

func TestDormantOrganizationsAreEvictedPastTheCap(t *testing.T) {
	m, _ := testMeter(t, nil, base)
	ids := func(from, to int) []string {
		var out []string
		for i := from; i < to; i++ {
			out = append(out, fmt.Sprintf("00000000-0000-4000-8000-%012x", i+1))
		}
		return out
	}
	// Sixteen linked, then sixteen others: the first sixteen go dormant.
	m.SetLinked(ids(0, 16), base)
	m.SetLinked(ids(16, 32), base.Add(time.Minute))
	if len(m.data.Organizations) != client.MaxMeterOrganizations {
		t.Fatalf("%d organizations", len(m.data.Organizations))
	}
	// Then sixteen more, each later than the last: the oldest dormant go.
	for i := 32; i < 48; i++ {
		m.SetLinked(append(ids(16, 32), ids(32, i+1)...), base.Add(time.Duration(i)*time.Minute))
	}
	if len(m.data.Organizations) != client.MaxMeterOrganizations {
		t.Fatalf("%d organizations", len(m.data.Organizations))
	}
	for _, id := range ids(0, 16) {
		if _, kept := m.data.Organizations[id]; kept {
			t.Fatalf("%s outlived newer dormant organizations", id)
		}
	}
	for _, id := range ids(32, 48) {
		if o, kept := m.data.Organizations[id]; !kept {
			t.Fatalf("%s is missing", id)
		} else if i := int(o.Since.Sub(base) / time.Minute); id != ids(i, i+1)[0] {
			t.Fatalf("%s since %v", id, o.Since)
		}
	}
}

// Every stop is a gap at the next start, a clean shutdown included, and an
// unclean exit is a gap from the last save.
func TestEveryStopBecomesAGapAtRestore(t *testing.T) {
	for _, reason := range []string{client.MeterStopShutdown, client.MeterStopQuiesce, client.MeterStopDisabled, client.MeterStopNoItems} {
		path := filepath.Join(t.TempDir(), "snapshot.meter.json")
		m := Open(path, []string{orgA}, base, &fakeLog{})
		m.Offer(apiKeyRecord(orgA, base).bytes(), base.Add(time.Second))
		m.Stop(reason, base.Add(time.Minute))
		stopped := load(t, path)
		if stopped.StoppedAt == nil || !stopped.StoppedAt.Equal(base.Add(time.Minute)) || stopped.StopReason != reason || stopped.Counted != 1 {
			t.Fatalf("%s: file after stop %+v", reason, stopped)
		}
		again := Open(path, []string{orgA}, base.Add(2*time.Hour+time.Minute), &fakeLog{})
		again.Stop(client.MeterStopShutdown, base.Add(3*time.Hour))
		restored := load(t, path)
		if len(restored.Gaps) != 1 || !restored.Gaps[0].From.Equal(base.Add(time.Minute)) || !restored.Gaps[0].To.Equal(base.Add(2*time.Hour+time.Minute)) || restored.Gaps[0].Reason != reason {
			t.Fatalf("%s: gaps=%+v", reason, restored.Gaps)
		}
		if restored.Restarts != 1 || !restored.Since.Equal(base) || !restored.StartedAt.Equal(base.Add(2*time.Hour+time.Minute)) || restored.Counted != 1 || restored.Organizations[orgA].Days[0].Usage[0].Requests != 1 || restored.Gaps[0].Brief() {
			t.Fatalf("%s: restored=%+v", reason, restored)
		}
	}
	// Unclean: saved at base+1m, never stopped, restored at base+10m.
	m, _ := testMeter(t, []string{orgA}, base)
	feed(m, apiKeyRecord(orgA, base).bytes(), base)
	m.flushIfDue(base.Add(time.Minute))
	again := newMeter(m.path, &fakeLog{})
	again.restore([]string{orgA}, base.Add(10*time.Minute))
	if gaps := again.data.Gaps; len(gaps) != 1 || !gaps[0].From.Equal(base.Add(time.Minute)) || !gaps[0].To.Equal(base.Add(10*time.Minute)) || gaps[0].Reason != client.MeterGapUncleanStop {
		t.Fatalf("gaps=%+v", gaps)
	}
}

func TestSavesWhenDirtyAndAsAHeartbeat(t *testing.T) {
	m, log := testMeter(t, []string{orgA}, base)
	if _, err := os.Lstat(m.path); !os.IsNotExist(err) {
		t.Fatal("restore wrote the file")
	}
	m.flushIfDue(base.Add(time.Minute))
	first := load(t, m.path)
	if !first.FlushedAt.Equal(base.Add(time.Minute)) || first.StartedAt != base {
		t.Fatalf("first save %+v", first)
	}
	m.flushIfDue(base.Add(2 * time.Minute))
	if load(t, m.path).FlushedAt != first.FlushedAt {
		t.Fatal("a clean meter was saved before the heartbeat")
	}
	feed(m, apiKeyRecord(orgA, base.Add(2*time.Minute)).bytes(), base.Add(2*time.Minute))
	m.flushIfDue(base.Add(3 * time.Minute))
	if saved := load(t, m.path); !saved.FlushedAt.Equal(base.Add(3*time.Minute)) || saved.Counted != 1 {
		t.Fatalf("a dirty meter was not saved: %+v", saved)
	}
	m.flushIfDue(base.Add(12 * time.Minute))
	if load(t, m.path).FlushedAt != base.Add(3*time.Minute) {
		t.Fatal("saved before ten minutes")
	}
	m.flushIfDue(base.Add(13 * time.Minute))
	if load(t, m.path).FlushedAt != base.Add(13*time.Minute) {
		t.Fatal("no heartbeat at ten minutes")
	}
	info, err := os.Lstat(m.path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("mode %v err %v", info.Mode(), err)
	}
	if len(log.logged()) != 0 {
		t.Fatalf("logged %+v", log.logged())
	}
}

func TestAnUnwritableMeterWarnsOncePerTenMinutes(t *testing.T) {
	log := &fakeLog{}
	dir := t.TempDir()
	m := newMeter(filepath.Join(dir, "gone", "snapshot.meter.json"), log)
	m.restore([]string{orgA}, base)
	for _, at := range []time.Duration{time.Minute, 2 * time.Minute, 11 * time.Minute, 12 * time.Minute} {
		m.flushIfDue(base.Add(at))
	}
	lines := log.logged()
	if len(lines) != 2 || lines[0].level != "warn" || lines[0].message != "quota-cache cannot save its API meter; Claude API credit estimates may lose recent spend" || lines[0].fields != nil {
		t.Fatalf("logged %+v", lines)
	}
	if !m.dirty {
		t.Fatal("a failed save left the meter clean")
	}
	if err := os.Mkdir(filepath.Join(dir, "gone"), 0700); err != nil {
		t.Fatal(err)
	}
	m.flushIfDue(base.Add(13 * time.Minute))
	if m.dirty || load(t, m.path).FlushedAt != base.Add(13*time.Minute) {
		t.Fatal("not saved once the directory existed")
	}
}

func TestAnUnreadableFileStartsFreshWithOneWarning(t *testing.T) {
	for name, content := range map[string]string{
		"garbage":   "not json",
		"schema 2":  `{"schema":2,"organizations":{}}`,
		"oversize":  `{"schema":1,"organizations":{}}` + strings.Repeat(" ", client.MaxMeterBytes),
		"directory": "",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "snapshot.meter.json")
			if content == "" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(t.TempDir(), "snapshot.meter.json")
				if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere.json"), path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			log := &fakeLog{}
			m := Open(path, []string{orgA}, base, log)
			m.Offer(apiKeyRecord(orgA, base).bytes(), base)
			m.Stop(client.MeterStopShutdown, base.Add(time.Minute))
			lines := log.logged()
			if len(lines) != 2 || lines[0].level != "warn" || lines[0].message != "quota-cache could not read its API meter file and starts counting again" || lines[1].level != "info" {
				t.Fatalf("logged %+v", lines)
			}
			meter := load(t, path)
			if !meter.Since.Equal(base) || meter.Restarts != 0 || len(meter.Gaps) != 0 || meter.Counted != 1 {
				t.Fatalf("replaced file: %+v", meter)
			}
		})
	}
}

func TestOpenLogsTheStartLine(t *testing.T) {
	log := &fakeLog{}
	m := Open(filepath.Join(t.TempDir(), "snapshot.meter.json"), []string{orgA, orgB}, base, log)
	m.Stop(client.MeterStopShutdown, base)
	lines := log.logged()
	if len(lines) != 1 || lines[0].level != "info" || lines[0].message != "quota-cache API meter counting Claude API-key traffic" || lines[0].fields["organizations"] != 2 {
		t.Fatalf("logged %+v", lines)
	}
}

// The oversize rule drops the oldest hour buckets of every organization,
// one hour at a time, then day buckets older than a cycle, re-encoding
// after each, and marks the organizations that lost one. It never refuses.
func TestOversizeExportDropsOldestHoursThenOldDays(t *testing.T) {
	m, _ := testMeter(t, []string{orgA, orgB}, base.Add(-40*24*time.Hour))
	for _, at := range []time.Time{base.Add(-40 * 24 * time.Hour), base.Add(-35 * 24 * time.Hour), base.Add(-2 * time.Hour), base.Add(-time.Hour), base} {
		for _, id := range []string{orgA, orgB} {
			feed(m, apiKeyRecord(id, at).bytes(), at)
		}
	}
	full := m.exportLimited(base, client.MaxMeterBytes)
	if a := org(t, m, orgA); len(a.Days) != 3 || len(a.Hours) != 3 || a.LastOverflowAt != nil {
		t.Fatalf("before: %+v", a)
	}
	trimmed := m.exportLimited(base, len(full)-1)
	a, b := org(t, m, orgA), org(t, m, orgB)
	if len(trimmed) >= len(full) || len(a.Hours) != 2 || len(b.Hours) != 2 || !a.Hours[0].Start.Equal(base.Add(-time.Hour)) || len(a.Days) != 3 || a.LastOverflowAt == nil || !a.LastOverflowAt.Equal(base) || b.LastOverflowAt == nil {
		t.Fatalf("after one hour: a=%+v b=%+v", a, b)
	}
	checkEntries(t, m)
	tiny := m.exportLimited(base, 10)
	a = org(t, m, orgA)
	if len(a.Hours) != 0 || len(a.Days) != 1 || !a.Days[0].Start.Equal(client.MeterDayStart(base)) || len(tiny) <= 10 {
		t.Fatalf("at the floor: %+v (%d bytes)", a, len(tiny))
	}
	checkEntries(t, m)
	if m.dirty {
		t.Fatal("export left the meter dirty")
	}
}

// A panic in the worker never escapes: the meter stops itself, writes the
// stop, says so once, takes nothing more, and the next open records the gap.
func TestAWorkerPanicStopsTheMeterAndIsLoggedOnce(t *testing.T) {
	log := &fakeLog{}
	path := filepath.Join(t.TempDir(), "snapshot.meter.json")
	m := Open(path, []string{orgA}, base, log)
	m.beforeApply = func(*event) { panic("injected: BODY-CANARY") }
	m.Offer(apiKeyRecord(orgA, base).bytes(), base)
	select {
	case <-m.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not exit")
	}
	if !m.stopped.Load() || !m.Failed() {
		t.Fatal("not stopped as failed")
	}
	m.Offer(apiKeyRecord(orgA, base).bytes(), base)
	if m.received.Load() != 1 {
		t.Fatal("a failed meter took a record")
	}
	meter := load(t, path)
	if meter.StopReason != client.MeterStopFailed || meter.StoppedAt == nil || meter.Received != 1 {
		t.Fatalf("file: %+v", meter)
	}
	errors := []logLine{}
	for _, line := range log.logged() {
		if line.level == "error" {
			errors = append(errors, line)
		}
	}
	if len(errors) != 1 || errors[0].message != "quota-cache API meter stopped after an internal error; Claude API credit estimates are incomplete until Quota Cache reloads" || strings.Contains(log.text(), "CANARY") {
		t.Fatalf("logged %+v", log.logged())
	}
	// Stop on a failed meter returns and keeps the reason.
	m.Stop(client.MeterStopShutdown, base.Add(time.Minute))
	if !m.Failed() {
		t.Fatal("Stop overwrote the failure")
	}
	again := Open(path, []string{orgA}, base.Add(10*time.Minute), &fakeLog{})
	again.Stop(client.MeterStopShutdown, base.Add(11*time.Minute))
	if gaps := load(t, path).Gaps; len(gaps) != 1 || gaps[0].Reason != client.MeterStopFailed || !gaps[0].To.Equal(base.Add(10*time.Minute)) {
		t.Fatalf("gaps=%+v", gaps)
	}
}

// Writing the meter marks nothing but the meter: the snapshot beside it is
// untouched, and counters not yet saved survive a restore from the file.
func TestRestoreCarriesCountersOn(t *testing.T) {
	m, _ := testMeter(t, []string{orgA}, base)
	for i := 0; i < 3; i++ {
		feed(m, apiKeyRecord(orgA, base).bytes(), base)
	}
	m.Offer([]byte("junk"), base)
	m.flushIfDue(base.Add(time.Minute))
	again := newMeter(m.path, &fakeLog{})
	again.restore([]string{orgA}, base.Add(time.Hour))
	if again.received.Load() != 4 || again.rejected.Load() != 1 || again.lastRejectedAt.Load() != base.Unix() || again.data.Counted != 3 || again.entries != 2 {
		t.Fatalf("restored received=%d rejected=%d counted=%d entries=%d", again.received.Load(), again.rejected.Load(), again.data.Counted, again.entries)
	}
	checkEntries(t, again)
}
