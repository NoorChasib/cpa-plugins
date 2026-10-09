package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

const (
	fakeAdminKey   = "sk-ant-admin01-FAKEFAKEFAKEFAKE"
	fakeOrgID      = "5b1e3c9a-0d1f-4c8e-9a51-7d0c2b6e4f10"
	fakeUserAgent  = "cpa-plugins-quota-cache/test (https://github.com/NoorChasib/cpa-plugins)"
	fakeKeyPrint   = "key-000000000001"
	creditRenewsOn = "2026-09-29"
)

// creditNow is 11:55 UTC on Oct 9, in the cycle that began on Sep 29: the
// window asked for is [Sep 29, Oct 10), eleven daily buckets.
var creditNow = time.Date(2026, 10, 9, 11, 55, 0, 0, time.UTC)

// scriptDoer answers each request with the next scripted response and records
// what was asked.
type scriptDoer struct {
	responses []protocol.HostHTTPResponse
	err       error
	requests  []protocol.HostHTTPRequest
}

func (d *scriptDoer) HTTPDo(_ context.Context, request protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	d.requests = append(d.requests, request)
	if d.err != nil {
		return protocol.HostHTTPResponse{}, d.err
	}
	if len(d.responses) == 0 {
		return protocol.HostHTTPResponse{StatusCode: 500}, nil
	}
	next := d.responses[0]
	d.responses = d.responses[1:]
	return next, nil
}

type costBucket struct {
	StartingAt string           `json:"starting_at"`
	EndingAt   string           `json:"ending_at"`
	Results    []map[string]any `json:"results"`
}

// buckets is n consecutive daily buckets from start. amounts gives some days
// results, each one {amount, currency USD} with every grouping field null, as
// Anthropic sends them ungrouped.
func buckets(start time.Time, n int, amounts map[int][]string) []costBucket {
	out := make([]costBucket, 0, n)
	for i := 0; i < n; i++ {
		day := start.Add(time.Duration(i) * 24 * time.Hour)
		results := []map[string]any{}
		for _, amount := range amounts[i] {
			results = append(results, map[string]any{"amount": amount, "currency": "USD", "description": nil, "model": nil, "workspace_id": nil, "cost_type": nil})
		}
		out = append(out, costBucket{StartingAt: day.Format(time.RFC3339), EndingAt: day.Add(24 * time.Hour).Format(time.RFC3339), Results: results})
	}
	return out
}

func costPage(data []costBucket, next string) protocol.HostHTTPResponse {
	page := map[string]any{"data": data, "has_more": next != "", "next_page": nil}
	if next != "" {
		page["next_page"] = next
	}
	raw, _ := json.Marshal(page)
	return protocol.HostHTTPResponse{StatusCode: 200, Headers: map[string][]string{"Anthropic-Organization-Id": {fakeOrgID}}, Body: raw}
}

func cycleStart() time.Time { return time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC) }

func fetchCredit(doer Doer, now time.Time) (Observation, error) {
	return FetchAPICredit(context.Background(), doer, fakeAdminKey, creditRenewsOn, fakeKeyPrint, fakeUserAgent, now)
}

func TestAPICreditAsksForTheCycleSoFarWithOneRequest(t *testing.T) {
	doer := &scriptDoer{responses: []protocol.HostHTTPResponse{
		costPage(buckets(cycleStart(), 11, map[int][]string{0: {"1250"}, 2: {"2000.5"}, 4: {"912.125"}, 8: {"837.5", "-3"}}), ""),
	}}
	observation, err := fetchCredit(doer, creditNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(doer.requests) != 1 {
		t.Fatalf("%d requests; the organization id in the header must spare /me", len(doer.requests))
	}
	request := doer.requests[0]
	want := "https://api.anthropic.com/v1/organizations/cost_report?starting_at=2026-09-29T00:00:00Z&ending_at=2026-10-10T00:00:00Z&bucket_width=1d&limit=31"
	if request.Method != "GET" || request.URL != want {
		t.Fatalf("request %s %s\nwant GET %s", request.Method, request.URL, want)
	}
	wantHeaders := map[string][]string{
		"X-Api-Key": {fakeAdminKey}, "Anthropic-Version": {"2023-06-01"},
		"Accept": {"application/json"}, "User-Agent": {fakeUserAgent},
	}
	if !reflect.DeepEqual(request.Headers, wantHeaders) {
		t.Fatalf("headers=%v", redactedHeaders(request.Headers))
	}
	if strings.Contains(fmt.Sprint(request.Headers), claudeUserAgent) {
		t.Fatal("the Admin API read identified itself as Claude Code")
	}
	if observation.Provider != client.ProviderAnthropicAPI || !observation.ObservedAt.IsZero() || !observation.ResetAt.IsZero() || observation.Percent != 0 || observation.Windows != nil {
		t.Fatalf("observation claims a weekly window: %+v", observation)
	}
	q := observation.Quota
	if q == nil || q.Schema != 1 || !q.ObservedAt.Equal(creditNow) || q.Windows != nil || q.Balances != nil {
		t.Fatalf("quota=%+v", q)
	}
	r := q.CostReport
	if r.OrganizationID != fakeOrgID || r.KeyFingerprint != fakeKeyPrint || !r.StartingAt.Equal(cycleStart()) ||
		!r.EndingAt.Equal(time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)) || len(r.Days) != 11 {
		t.Fatalf("report=%+v", r)
	}
	// Verbatim, unsummed, and an empty day is an empty list, not null.
	if got := r.Days[8].Amounts; !reflect.DeepEqual(got, []client.CostAmount{{Amount: "837.5", Currency: "USD"}, {Amount: "-3", Currency: "USD"}}) {
		t.Fatalf("day 8=%+v", got)
	}
	if r.Days[4].Amounts[0].Amount != "912.125" || r.Days[1].Amounts == nil || len(r.Days[1].Amounts) != 0 {
		t.Fatalf("days=%+v", r.Days)
	}
	if !r.CoveredUntil().Equal(r.EndingAt) {
		t.Fatalf("covered until %v, want %v", r.CoveredUntil(), r.EndingAt)
	}
	raw, _ := json.Marshal(r.Days[1])
	if !strings.Contains(string(raw), `"amounts":[]`) {
		t.Fatalf("an empty day encodes as %s", raw)
	}
}

// The window always starts at the cycle's first 00:00 UTC and stops at the
// next 00:00 UTC after now, at the edges of a day and of a cycle.
func TestAPICreditWindowAtDayAndCycleEdges(t *testing.T) {
	for _, tc := range []struct {
		name       string
		now        time.Time
		start, end string
		days       int
	}{
		{"last second of a day", time.Date(2026, 10, 9, 23, 59, 59, 0, time.UTC), "2026-09-29T00:00:00Z", "2026-10-10T00:00:00Z", 11},
		{"first second of a day", time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), "2026-09-29T00:00:00Z", "2026-10-11T00:00:00Z", 12},
		{"last second of the cycle", time.Date(2026, 10, 28, 23, 59, 59, 0, time.UTC), "2026-09-29T00:00:00Z", "2026-10-29T00:00:00Z", 30},
		{"renewal instant", time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC), "2026-10-29T00:00:00Z", "2026-10-30T00:00:00Z", 1},
		{"renewal day", time.Date(2026, 10, 29, 15, 0, 0, 0, time.UTC), "2026-10-29T00:00:00Z", "2026-10-30T00:00:00Z", 1},
		{"a zone behind UTC", time.Date(2026, 10, 28, 22, 30, 0, 0, time.FixedZone("EDT", -4*3600)), "2026-10-29T00:00:00Z", "2026-10-30T00:00:00Z", 1},
	} {
		start, _ := time.Parse(time.RFC3339, tc.start)
		doer := &scriptDoer{responses: []protocol.HostHTTPResponse{costPage(buckets(start, tc.days, nil), "")}}
		observation, err := fetchCredit(doer, tc.now)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		want := "starting_at=" + tc.start + "&ending_at=" + tc.end + "&"
		if !strings.Contains(doer.requests[0].URL, want) {
			t.Fatalf("%s: url %s, want %s", tc.name, doer.requests[0].URL, want)
		}
		if got := observation.Quota.CostReport; len(got.Days) != tc.days || got.EndingAt.Format(time.RFC3339) != tc.end {
			t.Fatalf("%s: report=%+v", tc.name, got)
		}
	}
}

// Today's bucket is still in progress and may be missing: the reading is kept,
// and covers up to today. Every complete day before it must be there.
func TestAPICreditToleratesOnlyTodayMissing(t *testing.T) {
	today := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	doer := &scriptDoer{responses: []protocol.HostHTTPResponse{costPage(buckets(cycleStart(), 10, nil), "")}}
	observation, err := fetchCredit(doer, creditNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := observation.Quota.CostReport.CoveredUntil(); !got.Equal(today) {
		t.Fatalf("covered until %v, want %v", got, today)
	}
	doer = &scriptDoer{responses: []protocol.HostHTTPResponse{costPage(buckets(cycleStart(), 9, nil), "")}}
	if _, err := fetchCredit(doer, creditNow); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("a missing complete day: err=%v", err)
	}
	// No buckets at all is a reading only on the cycle's first day.
	doer = &scriptDoer{responses: []protocol.HostHTTPResponse{costPage([]costBucket{}, "")}}
	if _, err := fetchCredit(doer, creditNow); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("no days mid-cycle: err=%v", err)
	}
	first := time.Date(2026, 10, 29, 0, 4, 0, 0, time.UTC)
	doer = &scriptDoer{responses: []protocol.HostHTTPResponse{costPage([]costBucket{}, "")}}
	observation, err = fetchCredit(doer, first)
	if err != nil {
		t.Fatal(err)
	}
	if days := observation.Quota.CostReport.Days; days == nil || len(days) != 0 {
		t.Fatalf("days=%#v; want an empty list", days)
	}
	raw, _ := json.Marshal(observation.Quota.CostReport)
	if !strings.Contains(string(raw), `"days":[]`) {
		t.Fatalf("report encodes as %s", raw)
	}
}

// A bucket at or after the ending_at sent should never come back. If it does,
// it and everything after it are dropped, and the poll still succeeds.
func TestAPICreditDropsBucketsFromTheFuture(t *testing.T) {
	data := buckets(cycleStart(), 13, map[int][]string{11: {"999"}})
	// Past the drop, nothing is even looked at.
	data[12].StartingAt = "not a time"
	doer := &scriptDoer{responses: []protocol.HostHTTPResponse{costPage(data, "")}}
	observation, err := fetchCredit(doer, creditNow)
	if err != nil {
		t.Fatal(err)
	}
	if r := observation.Quota.CostReport; len(r.Days) != 11 || !r.Days[10].StartingAt.Equal(time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("days=%+v", r.Days)
	}
}

func TestAPICreditFollowsPagesUpToThree(t *testing.T) {
	all := buckets(cycleStart(), 11, map[int][]string{10: {"5"}})
	doer := &scriptDoer{responses: []protocol.HostHTTPResponse{
		costPage(all[:4], "page/one="), costPage(all[4:8], "page_two"), costPage(all[8:], ""),
	}}
	observation, err := fetchCredit(doer, creditNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(doer.requests) != 3 || !strings.HasSuffix(doer.requests[1].URL, "&limit=31&page=page%2Fone%3D") || !strings.HasSuffix(doer.requests[2].URL, "&page=page_two") {
		t.Fatalf("requests=%s", describe(doer.requests))
	}
	if r := observation.Quota.CostReport; len(r.Days) != 11 || r.Days[10].Amounts[0].Amount != "5" {
		t.Fatalf("days=%+v", r.Days)
	}
	// A fourth page is never asked for.
	doer = &scriptDoer{responses: []protocol.HostHTTPResponse{
		costPage(all[:2], "a"), costPage(all[2:4], "b"), costPage(all[4:6], "c"), costPage(all[6:], ""),
	}}
	if _, err := fetchCredit(doer, creditNow); !errors.Is(err, ErrInvalidResponse) || len(doer.requests) != 3 {
		t.Fatalf("err=%v requests=%d", err, len(doer.requests))
	}
	// has_more with no cursor to follow.
	page := costPage(all[:4], "")
	page.Body = []byte(strings.Replace(string(page.Body), `"has_more":false`, `"has_more":true`, 1))
	doer = &scriptDoer{responses: []protocol.HostHTTPResponse{page}}
	if _, err := fetchCredit(doer, creditNow); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("has_more without next_page: err=%v", err)
	}
	// Buckets must also run on across a page boundary.
	doer = &scriptDoer{responses: []protocol.HostHTTPResponse{costPage(all[:4], "a"), costPage(all[5:], "")}}
	if _, err := fetchCredit(doer, creditNow); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("a gap between pages: err=%v", err)
	}
}

func TestAPICreditAsksForTheOrganizationOnlyWithoutTheHeader(t *testing.T) {
	me := func(body string) protocol.HostHTTPResponse {
		return protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(body)}
	}
	without := func(header ...string) protocol.HostHTTPResponse {
		page := costPage(buckets(cycleStart(), 11, nil), "")
		page.Headers = map[string][]string{}
		if len(header) > 0 {
			page.Headers["Anthropic-Organization-Id"] = header
		}
		return page
	}
	for _, tc := range []struct {
		name  string
		cost  protocol.HostHTTPResponse
		me    protocol.HostHTTPResponse
		id    string
		valid bool
	}{
		{"absent", without(), me(`{"id":"org-from-me","name":"Ignored","type":"organization"}`), "org-from-me", true},
		{"malformed header", without("not an id!"), me(`{"id":"org-from-me","type":"organization"}`), "org-from-me", true},
		{"empty header", without(""), me(`{"id":"org-from-me"}`), "org-from-me", true},
		{"wrong type", without(), me(`{"id":"org-from-me","type":"workspace"}`), "", false},
		{"bad id", without(), me(`{"id":"has space","type":"organization"}`), "", false},
		{"numeric id", without(), me(`{"id":12,"type":"organization"}`), "", false},
		{"long id", without(), me(`{"id":"` + strings.Repeat("a", 65) + `"}`), "", false},
		{"trailing json", without(), me(`{"id":"org"} {}`), "", false},
	} {
		doer := &scriptDoer{responses: []protocol.HostHTTPResponse{tc.cost, tc.me}}
		observation, err := fetchCredit(doer, creditNow)
		if len(doer.requests) != 2 || doer.requests[1].URL != "https://api.anthropic.com/v1/organizations/me" || doer.requests[1].Headers["X-Api-Key"][0] != fakeAdminKey {
			t.Fatalf("%s: requests=%s", tc.name, describe(doer.requests))
		}
		if !tc.valid {
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("%s: err=%v", tc.name, err)
			}
			continue
		}
		if err != nil || observation.Quota.CostReport.OrganizationID != tc.id {
			t.Fatalf("%s: observation=%+v err=%v", tc.name, observation, err)
		}
	}
	// A refused /me fails the poll with its status.
	doer := &scriptDoer{responses: []protocol.HostHTTPResponse{without(), {StatusCode: 429}}}
	if _, err := fetchCredit(doer, creditNow); !errors.Is(err, HTTPStatusError{StatusCode: 429}) {
		t.Fatalf("err=%v", err)
	}
	// The header is read whatever its case.
	page := without()
	page.Headers["anthropic-organization-id"] = []string{"org_lower"}
	doer = &scriptDoer{responses: []protocol.HostHTTPResponse{page}}
	if observation, err := fetchCredit(doer, creditNow); err != nil || observation.Quota.CostReport.OrganizationID != "org_lower" || len(doer.requests) != 1 {
		t.Fatalf("observation=%+v err=%v", observation, err)
	}
}

// The first refusal ends the poll; nothing is retried within it.
func TestAPICreditStopsAtTheFirstRefusal(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 413, 429, 500, 529} {
		doer := &scriptDoer{responses: []protocol.HostHTTPResponse{{StatusCode: status, Body: []byte(`{"type":"error","error":{"type":"x","message":"` + fakeAdminKey + `"}}`)}, costPage(nil, "")}}
		_, err := fetchCredit(doer, creditNow)
		if !errors.Is(err, HTTPStatusError{StatusCode: status}) || len(doer.requests) != 1 {
			t.Fatalf("%d: err=%v requests=%d", status, err, len(doer.requests))
		}
		if strings.Contains(err.Error(), "FAKE") {
			t.Fatalf("%d: the error echoes the body: %v", status, err)
		}
	}
	doer := &scriptDoer{err: errors.New("dial failed")}
	if _, err := fetchCredit(doer, creditNow); err == nil || len(doer.requests) != 1 {
		t.Fatalf("transport: err=%v", err)
	}
	doer = &scriptDoer{}
	if _, err := FetchAPICredit(context.Background(), doer, "  ", creditRenewsOn, "", fakeUserAgent, creditNow); !errors.Is(err, ErrNoAccessToken) || len(doer.requests) != 0 {
		t.Fatalf("no key: err=%v", err)
	}
	if _, err := FetchAPICredit(context.Background(), doer, fakeAdminKey, "2026-02-30", "", fakeUserAgent, creditNow); err == nil || len(doer.requests) != 0 {
		t.Fatalf("bad renewal: err=%v", err)
	}
}

func TestAPICreditRejectsResponsesThatBreakTheContract(t *testing.T) {
	valid := func() []costBucket { return buckets(cycleStart(), 11, map[int][]string{3: {"10"}}) }
	edit := func(change func([]costBucket) []costBucket) protocol.HostHTTPResponse {
		return costPage(change(valid()), "")
	}
	raw := func(body string) protocol.HostHTTPResponse {
		return protocol.HostHTTPResponse{StatusCode: 200, Headers: map[string][]string{"Anthropic-Organization-Id": {fakeOrgID}}, Body: []byte(body)}
	}
	result := func(amount, currency any) func([]costBucket) []costBucket {
		return func(b []costBucket) []costBucket {
			b[3].Results = []map[string]any{{"amount": amount, "currency": currency}}
			return b
		}
	}
	for name, response := range map[string]protocol.HostHTTPResponse{
		"not midnight": edit(func(b []costBucket) []costBucket {
			b[0].StartingAt, b[0].EndingAt = "2026-09-29T01:00:00Z", "2026-09-30T01:00:00Z"
			return b
		}),
		"not one day wide": edit(func(b []costBucket) []costBucket { b[2].EndingAt = "2026-10-03T00:00:00Z"; return b }),
		"gap":              edit(func(b []costBucket) []costBucket { return append(b[:4], b[5:]...) }),
		"repeated day":     edit(func(b []costBucket) []costBucket { return append(b[:4], b[3:]...) }),
		"first day late":   edit(func(b []costBucket) []costBucket { return b[1:] }),
		"first day early":  costPage(buckets(cycleStart().Add(-24*time.Hour), 12, nil), ""),
		"bad starting_at":  edit(func(b []costBucket) []costBucket { b[1].StartingAt = "yesterday"; return b }),
		"fraction amount":  edit(result("1/3", "USD")),
		"long amount":      edit(result(strings.Repeat("1", 65), "USD")),
		"exponent amount":  edit(result("1e3", "USD")),
		"padded amount":    edit(result(" 10", "USD")),
		"number amount":    edit(result(10, "USD")),
		"lower currency":   edit(result("10", "usd")),
		"missing currency": edit(result("10", nil)),
		"results null": edit(func(b []costBucket) []costBucket {
			b[3].Results = nil
			return b
		}),
		"too many results": edit(func(b []costBucket) []costBucket {
			for i := 0; i < 33; i++ {
				b[3].Results = append(b[3].Results, map[string]any{"amount": "1", "currency": "USD"})
			}
			return b
		}),
		"data missing":         raw(`{"has_more":false}`),
		"data not a list":      raw(`{"data":{},"has_more":false}`),
		"bucket not an object": raw(`{"data":[1],"has_more":false}`),
		"trailing json":        raw(`{"data":[],"has_more":false} {}`),
		"not json":             raw(`<html>`),
		"oversize body":        raw(`{"data":[],"pad":"` + strings.Repeat("x", maxResponseBytes) + `"}`),
		"too many buckets": raw(func() string {
			body, _ := json.Marshal(map[string]any{"data": buckets(cycleStart(), 63, nil)})
			return string(body)
		}()),
	} {
		doer := &scriptDoer{responses: []protocol.HostHTTPResponse{response}}
		if _, err := fetchCredit(doer, creditNow); !errors.Is(err, ErrInvalidResponse) {
			t.Errorf("%s: err=%v; want ErrInvalidResponse", name, err)
		}
	}
	// Fractions of a cent and a negative amount are real and kept verbatim.
	doer := &scriptDoer{responses: []protocol.HostHTTPResponse{edit(result("-0.0001", "EUR"))}}
	if observation, err := fetchCredit(doer, creditNow); err != nil || observation.Quota.CostReport.Days[3].Amounts[0] != (client.CostAmount{Amount: "-0.0001", Currency: "EUR"}) {
		t.Fatalf("observation=%+v err=%v", observation, err)
	}
}

// redactedHeaders is headers for a failure message, with the key replaced by
// whether it is the fake key, so a failing test never prints a key.
func redactedHeaders(headers map[string][]string) map[string][]string {
	out := make(map[string][]string, len(headers))
	for name, values := range headers {
		if strings.EqualFold(name, "X-Api-Key") {
			printed := make([]string, len(values))
			for i, v := range values {
				printed[i] = fmt.Sprintf("<key redacted, fake key: %t>", v == fakeAdminKey)
			}
			values = printed
		}
		out[name] = values
	}
	return out
}

func describe(requests []protocol.HostHTTPRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d request(s)", len(requests))
	for _, r := range requests {
		fmt.Fprintf(&b, "\n%s %s %v", r.Method, r.URL, redactedHeaders(r.Headers))
	}
	return b.String()
}
