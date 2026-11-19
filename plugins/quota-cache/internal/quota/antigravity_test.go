package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

// antigravityAuth is a synthetic credential shaped like CPA's
// antigravity-<email>.json. None of its values is real.
const antigravityAuth = `{"type":"antigravity","access_token":"synthetic-secret","refresh_token":"synthetic-refresh",` +
	`"expires_in":3599,"timestamp":1788000000000,"expired":"2099-01-01T00:00:00Z","email":"synthetic@example.invalid",` +
	`"project_id":"synthetic-project"}`

var (
	antigravitySummaryURL = antigravityDailyBase + antigravitySummaryPath
	antigravityModelsURL  = antigravityDailyBase + antigravityModelsPath
	antigravityLoadURL    = antigravityDailyBase + antigravityLoadCodeAssistPath
)

// antigravitySummary is the shape t3code's fixtures and live accounts report:
// grouped buckets, one exhausted 3p bucket whose remainingFraction proto3 left
// out, and descriptions and display names that must never be stored.
const antigravitySummaryBody = `{"groups":[` +
	`{"displayName":"Gemini","buckets":[` +
	`{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.75,"resetTime":"2026-09-05T01:00:00Z","description":"synthetic-description"},` +
	`{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.96,"resetTime":"2026-09-10T00:00:00Z","displayName":"synthetic-display"}]},` +
	`{"displayName":"Claude & GPT","buckets":[` +
	`{"bucketId":"3p-5h","window":"5h","resetTime":"2026-09-04T23:00:00Z"},` +
	`{"bucketId":"3p-weekly","window":"weekly","remainingFraction":0.5,"resetTime":"2026-09-08T00:00:00Z"}]}]}`

// antigravityDoer answers the summary with body and records every request.
func antigravityDoer(bodies map[string]string) *recordingDoer {
	return &recordingDoer{routingDoer: routingDoer{bodies: bodies}}
}

func fetchAntigravity(t *testing.T, auth, summary string) (Observation, *recordingDoer) {
	t.Helper()
	doer := antigravityDoer(map[string]string{antigravitySummaryURL: summary})
	o, err := Fetch(context.Background(), doer, client.ProviderAntigravity, []byte(auth), now)
	if err != nil {
		t.Fatal(err)
	}
	return o, doer
}

func requestBody(t *testing.T, request protocol.HostHTTPRequest) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(request.Body, &body); err != nil {
		t.Fatalf("request body %q is not a JSON object: %v", request.Body, err)
	}
	return body
}

func TestAntigravityIsSupported(t *testing.T) {
	if !Supported("antigravity") || !Supported(" Antigravity ") || client.ProviderAntigravity != "antigravity" {
		t.Fatal("antigravity is not a supported provider")
	}
	// Gemini CLI and Vertex are other CPA providers, not aliases of it.
	for _, other := range []string{"gemini", "gemini-cli", "vertex", "google"} {
		if Supported(other) {
			t.Fatalf("%q is supported", other)
		}
	}
}

// The summary request is the one CPA's executor would make: a POST to the
// daily host, or a base_url only when it is an https googleapis.com URL, with
// the bearer token, JSON headers, a User-Agent and the project.
func TestAntigravitySummaryRequestShape(t *testing.T) {
	longUA := strings.Repeat("a", maxAntigravityUserAgent+1)
	cases := []struct {
		name    string
		auth    map[string]any
		listing string
		url     string
		ua      string
		body    string
	}{
		{"defaults with the file's project", nil, "", antigravitySummaryURL, antigravityUserAgent, `{"project":"synthetic-project"}`},
		{"the file's project outranks the listing's", nil, "listed-project", antigravitySummaryURL, antigravityUserAgent, `{"project":"synthetic-project"}`},
		{"the listing's project when the file has none", map[string]any{"project_id": nil}, "listed-project", antigravitySummaryURL, antigravityUserAgent, `{"project":"listed-project"}`},
		{"an empty project falls back to the listing", map[string]any{"project_id": "  "}, "listed-project", antigravitySummaryURL, antigravityUserAgent, `{"project":"listed-project"}`},
		{"an invalid project falls back to the listing", map[string]any{"project_id": "bad project\"}"}, "listed-project", antigravitySummaryURL, antigravityUserAgent, `{"project":"listed-project"}`},
		{"no project anywhere sends an empty object", map[string]any{"project_id": nil}, "", antigravitySummaryURL, antigravityUserAgent, `{}`},
		{"an invalid listed project sends an empty object", map[string]any{"project_id": nil}, "bad listed\nproject", antigravitySummaryURL, antigravityUserAgent, `{}`},
		{"a googleapis base_url is honoured", map[string]any{"base_url": "https://cloudcode-pa.googleapis.com/"}, "", "https://cloudcode-pa.googleapis.com" + antigravitySummaryPath, antigravityUserAgent, `{"project":"synthetic-project"}`},
		{"an upper-case googleapis host is honoured", map[string]any{"base_url": "https://CloudCode-PA.GoogleAPIs.com"}, "", "https://cloudcode-pa.googleapis.com" + antigravitySummaryPath, antigravityUserAgent, `{"project":"synthetic-project"}`},
		{"port 443 is honoured", map[string]any{"base_url": "https://autopush-cloudcode-pa.sandbox.googleapis.com:443"}, "", "https://autopush-cloudcode-pa.sandbox.googleapis.com" + antigravitySummaryPath, antigravityUserAgent, `{"project":"synthetic-project"}`},
		{"http is refused", map[string]any{"base_url": "http://cloudcode-pa.googleapis.com"}, "", antigravitySummaryURL, antigravityUserAgent, `{"project":"synthetic-project"}`},
		{"another domain is refused", map[string]any{"base_url": "https://proxy.example.invalid"}, "", antigravitySummaryURL, antigravityUserAgent, `{"project":"synthetic-project"}`},
		{"a suffix-lookalike host is refused", map[string]any{"base_url": "https://cloudcode-pa.googleapis.com.example.invalid"}, "", antigravitySummaryURL, antigravityUserAgent, `{"project":"synthetic-project"}`},
		{"the bare apex is refused", map[string]any{"base_url": "https://googleapis.com"}, "", antigravitySummaryURL, antigravityUserAgent, `{"project":"synthetic-project"}`},
		{"userinfo is refused", map[string]any{"base_url": "https://user:pass@cloudcode-pa.googleapis.com"}, "", antigravitySummaryURL, antigravityUserAgent, `{"project":"synthetic-project"}`},
		{"another port is refused", map[string]any{"base_url": "https://cloudcode-pa.googleapis.com:8443"}, "", antigravitySummaryURL, antigravityUserAgent, `{"project":"synthetic-project"}`},
		{"a query is refused", map[string]any{"base_url": "https://cloudcode-pa.googleapis.com?key=x"}, "", antigravitySummaryURL, antigravityUserAgent, `{"project":"synthetic-project"}`},
		{"a fragment trick is refused", map[string]any{"base_url": "https://example.invalid#.googleapis.com"}, "", antigravitySummaryURL, antigravityUserAgent, `{"project":"synthetic-project"}`},
		{"a non-URL is refused", map[string]any{"base_url": "cloudcode-pa.googleapis.com"}, "", antigravitySummaryURL, antigravityUserAgent, `{"project":"synthetic-project"}`},
		{"an operator user_agent is honoured", map[string]any{"user_agent": "antigravity/hub/2.30.0 (aidev_client; os_type=linux; arch=amd64)"}, "", antigravitySummaryURL, "antigravity/hub/2.30.0 (aidev_client; os_type=linux; arch=amd64)", `{"project":"synthetic-project"}`},
		{"a control character in user_agent is refused", map[string]any{"user_agent": "agent\x01/1"}, "", antigravitySummaryURL, antigravityUserAgent, `{"project":"synthetic-project"}`},
		{"non-ASCII user_agent is refused", map[string]any{"user_agent": "agent/é"}, "", antigravitySummaryURL, antigravityUserAgent, `{"project":"synthetic-project"}`},
		{"an oversized user_agent is refused", map[string]any{"user_agent": longUA}, "", antigravitySummaryURL, antigravityUserAgent, `{"project":"synthetic-project"}`},
		{"a non-string user_agent is ignored", map[string]any{"user_agent": 7}, "", antigravitySummaryURL, antigravityUserAgent, `{"project":"synthetic-project"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var auth map[string]any
			if err := json.Unmarshal([]byte(antigravityAuth), &auth); err != nil {
				t.Fatal(err)
			}
			for key, value := range tc.auth {
				if value == nil {
					delete(auth, key)
				} else {
					auth[key] = value
				}
			}
			raw, _ := json.Marshal(auth)
			doer := &fakeDoer{response: protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(antigravitySummaryBody)}}
			if _, err := FetchListed(context.Background(), doer, "antigravity", raw, Listing{ProjectID: tc.listing}, now); err != nil {
				t.Fatal(err)
			}
			request := doer.request
			if request.Method != "POST" || request.URL != tc.url {
				t.Fatalf("request = %s %s; want POST %s", request.Method, request.URL, tc.url)
			}
			want := map[string]string{"Authorization": "Bearer synthetic-secret", "Content-Type": "application/json", "Accept": "application/json", "User-Agent": tc.ua}
			if len(request.Headers) != len(want) {
				t.Fatalf("headers = %v; want exactly %v", request.Headers, want)
			}
			for key, value := range want {
				if got := header(request, key); got != value || len(request.Headers[key]) != 1 {
					t.Fatalf("header %s = %q; want %q", key, got, value)
				}
			}
			if string(request.Body) != tc.body {
				t.Fatalf("body = %s; want %s", request.Body, tc.body)
			}
		})
	}
}

// A stored token already past its expiry means CPA's refresh is failing, and a
// request would only collect a 401. Nothing is sent.
func TestAntigravityExpiredTokenIsNeverSent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		expired string
		sends   bool
	}{
		{"expired an hour ago", `"2026-09-04T21:00:00Z"`, false},
		{"expiring this instant", `"2026-09-04T22:00:00Z"`, false},
		{"expired, with an offset", `"2026-09-04T23:30:00+02:00"`, false},
		{"still valid", `"2026-09-04T22:00:01Z"`, true},
		{"unparseable", `"tomorrow"`, true},
		{"not a string", `1788000000`, true},
		{"absent", ``, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := `{"access_token":"synthetic-secret","project_id":"p1"}`
			if tc.expired != "" {
				auth = `{"access_token":"synthetic-secret","project_id":"p1","expired":` + tc.expired + `}`
			}
			doer := antigravityDoer(map[string]string{antigravitySummaryURL: antigravitySummaryBody})
			_, err := Fetch(context.Background(), doer, "antigravity", []byte(auth), now)
			if tc.sends {
				if err != nil || len(doer.urls) != 1 {
					t.Fatalf("err=%v urls=%v; want one request", err, doer.urls)
				}
				return
			}
			if !errors.Is(err, ErrTokenExpired) || len(doer.urls) != 0 {
				t.Fatalf("err=%v urls=%v; want ErrTokenExpired and no request", err, doer.urls)
			}
		})
	}
	doer := antigravityDoer(nil)
	if _, err := Fetch(context.Background(), doer, "antigravity", []byte(`{"refresh_token":"synthetic-refresh","project_id":"p1"}`), now); !errors.Is(err, ErrNoAccessToken) || len(doer.urls) != 0 {
		t.Fatalf("err=%v urls=%v; a credential without an access token must fail before HTTP", err, doer.urls)
	}
}

func usedOf(t *testing.T, q *client.Quota, id string) float64 {
	t.Helper()
	w, ok := q.Windows[id]
	if !ok || w.UsedPercent == nil {
		t.Fatalf("window %q missing from %v", id, q.Windows)
	}
	return *w.UsedPercent
}

// The summary's buckets become quota.windows verbatim: used share, declared
// duration, reset and window name, keyed by bucketId.
func TestAntigravitySummaryBucketsBecomeWindows(t *testing.T) {
	o, doer := fetchAntigravity(t, antigravityAuth, antigravitySummaryBody)
	q := o.Quota
	if len(q.Windows) != 4 || len(q.Limits) != 0 || len(q.Balances) != 0 || q.Plan != "" || q.Truncated {
		t.Fatalf("quota = %+v", q)
	}
	for id, want := range map[string]float64{"gemini-5h": 25, "gemini-weekly": 4, "3p-5h": 100, "3p-weekly": 50} {
		if got := usedOf(t, q, id); got != want {
			t.Fatalf("%s used %v; want %v", id, got, want)
		}
	}
	weekly := q.Windows["gemini-weekly"]
	if weekly.DurationSeconds == nil || *weekly.DurationSeconds != 604800 || weekly.Period != "weekly" ||
		weekly.ResetsAt == nil || !weekly.ResetsAt.Equal(time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("gemini-weekly = %+v", weekly)
	}
	session := q.Windows["gemini-5h"]
	if session.DurationSeconds == nil || *session.DurationSeconds != 18000 || session.Period != "5h" {
		t.Fatalf("gemini-5h = %+v", session)
	}
	// A summary with usable buckets never costs the model-list request.
	if doer.asked(antigravityModelsURL) || len(doer.urls) != 1 {
		t.Fatalf("urls = %v; want the summary alone", doer.urls)
	}
	raw, _ := json.Marshal(o)
	for _, secret := range []string{"synthetic-secret", "synthetic-refresh", "synthetic-description", "synthetic-display", "synthetic-project", "example.invalid"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("the observation carries %q", secret)
		}
	}
}

// Table-driven bucket rules: where the buckets are read from, what is skipped,
// and how each reading becomes a window.
func TestAntigravitySummaryBucketRules(t *testing.T) {
	reset := `"2026-09-06T00:00:00Z"`
	bucket := func(id, window, extra string) string {
		return `{"bucketId":"` + id + `","window":"` + window + `"` + extra + `}`
	}
	cases := []struct {
		name string
		body string
		want map[string]float64 // id -> used percent; absent ids must be absent
	}{
		{"groups win over top-level buckets",
			`{"groups":[{"buckets":[` + bucket("gemini-weekly", "weekly", `,"remainingFraction":0.9,"resetTime":`+reset) + `]}],` +
				`"buckets":[` + bucket("3p-weekly", "weekly", `,"remainingFraction":0.1,"resetTime":`+reset) + `]}`,
			map[string]float64{"gemini-weekly": 10}},
		{"top-level buckets when no group has any",
			`{"groups":[{"displayName":"Empty","buckets":[]},{"displayName":"None"}],` +
				`"buckets":[` + bucket("3p-weekly", "weekly", `,"remainingFraction":0.1,"resetTime":`+reset) + `]}`,
			map[string]float64{"3p-weekly": 90}},
		{"every group's buckets are read",
			`{"groups":[{"buckets":[` + bucket("gemini-weekly", "weekly", `,"remainingFraction":1,"resetTime":`+reset) + `]},` +
				`{"buckets":[` + bucket("3p-5h", "5h", `,"remainingFraction":0,"resetTime":`+reset) + `]}]}`,
			map[string]float64{"gemini-weekly": 0, "3p-5h": 100}},
		{"disabled buckets are skipped",
			`{"buckets":[` + bucket("gemini-weekly", "weekly", `,"remainingFraction":0.5,"disabled":true,"resetTime":`+reset) + `,` +
				bucket("3p-weekly", "weekly", `,"remainingFraction":0.5,"disabled":false,"resetTime":`+reset) + `]}`,
			map[string]float64{"3p-weekly": 50}},
		{"a missing fraction with a reset is exhausted",
			`{"buckets":[` + bucket("gemini-weekly", "weekly", `,"resetTime":`+reset) + `]}`,
			map[string]float64{"gemini-weekly": 100}},
		{"a null fraction with a reset is exhausted",
			`{"buckets":[` + bucket("gemini-weekly", "weekly", `,"remainingFraction":null,"resetTime":`+reset) + `]}`,
			map[string]float64{"gemini-weekly": 100}},
		{"neither fraction nor reset is skipped",
			`{"buckets":[` + bucket("gemini-weekly", "weekly", ``) + `,` + bucket("3p-weekly", "weekly", `,"remainingFraction":0.25`) + `]}`,
			map[string]float64{"3p-weekly": 75}},
		{"fractions are clamped to 0..1",
			`{"buckets":[` + bucket("gemini-weekly", "weekly", `,"remainingFraction":1.5,"resetTime":`+reset) + `,` +
				bucket("3p-weekly", "weekly", `,"remainingFraction":-0.5,"resetTime":`+reset) + `]}`,
			map[string]float64{"gemini-weekly": 0, "3p-weekly": 100}},
		{"invalid bucket ids are skipped",
			`{"buckets":[` + bucket("", "weekly", `,"remainingFraction":0.5`) + `,` + bucket("-leading-dash", "weekly", `,"remainingFraction":0.5`) + `,` +
				bucket("has space", "weekly", `,"remainingFraction":0.5`) + `,` + bucket("models/gemini-weekly", "weekly", `,"remainingFraction":0.5`) + `,` +
				bucket("gemini-<script>", "weekly", `,"remainingFraction":0.5`) + `,` + bucket(strings.Repeat("g", 65), "weekly", `,"remainingFraction":0.5`) + `,` +
				`{"window":"weekly","remainingFraction":0.5},{"bucketId":7,"remainingFraction":0.5},"not an object",` +
				bucket(strings.Repeat("g", 64), "weekly", `,"remainingFraction":0.5`) + `]}`,
			map[string]float64{strings.Repeat("g", 64): 50}},
		{"a repeated bucket keeps its first reading",
			`{"groups":[{"buckets":[` + bucket("gemini-weekly", "weekly", `,"remainingFraction":0.8`) + `]},` +
				`{"buckets":[` + bucket("gemini-weekly", "weekly", `,"remainingFraction":0.1`) + `]}]}`,
			map[string]float64{"gemini-weekly": 20}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := antigravityDoer(map[string]string{antigravitySummaryURL: tc.body})
			o, err := Fetch(context.Background(), doer, "antigravity", []byte(antigravityAuth), now)
			if err != nil {
				t.Fatal(err)
			}
			if len(o.Quota.Windows) != len(tc.want) {
				t.Fatalf("windows = %v; want %v", o.Quota.Windows, tc.want)
			}
			for id, want := range tc.want {
				if got := usedOf(t, o.Quota, id); got != want {
					t.Fatalf("%s used %v; want %v", id, got, want)
				}
			}
		})
	}
}

// Windows other than 5h and weekly are kept with their name but no duration,
// and so are never given a canonical meaning they may not have.
func TestAntigravityUnknownWindowHasNoDurationAndIsRaw(t *testing.T) {
	o, _ := fetchAntigravity(t, antigravityAuth, `{"buckets":[`+
		`{"bucketId":"gemini-daily","window":"daily","remainingFraction":0.5,"resetTime":"2026-09-05T00:00:00Z"},`+
		`{"bucketId":"gemini-other","remainingFraction":0.5},`+
		`{"bucketId":"gemini-weird","window":"bad\nwindow","remainingFraction":0.5}]}`)
	for id, period := range map[string]string{"gemini-daily": "daily", "gemini-other": "", "gemini-weird": ""} {
		w := o.Quota.Windows[id]
		if w.DurationSeconds != nil || w.Period != period {
			t.Fatalf("%s = %+v; want no duration and period %q", id, w, period)
		}
	}
	want := []string{"raw:antigravity:gemini-daily", "raw:antigravity:gemini-other", "raw:antigravity:gemini-weird"}
	if got := keysOf(o.Windows); !equal(got, want) {
		t.Fatalf("keys = %v; want %v", got, want)
	}
	if !o.ObservedAt.IsZero() {
		t.Fatal("an account with no Gemini weekly reported a weekly observation")
	}
}

// An untouched bucket reports a rolling reset one full window from now. It
// records no reset, so the snapshot does not churn and no consumer shows a
// reset that never arrives; anything that says the bucket was touched keeps it.
func TestAntigravityUntouchedRollingBucketRecordsNoReset(t *testing.T) {
	week := now.Add(7 * 24 * time.Hour)
	fiveHours := now.Add(5 * time.Hour)
	at := func(t time.Time) string { return `"` + t.Format(time.RFC3339) + `"` }
	cases := []struct {
		name, bucket string
		keepsReset   bool
	}{
		{"untouched weekly", `{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":1,"resetTime":` + at(week) + `}`, false},
		{"untouched weekly, 60s late", `{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":1,"resetTime":` + at(week.Add(time.Minute)) + `}`, false},
		{"untouched weekly, 60s early", `{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":1,"resetTime":` + at(week.Add(-time.Minute)) + `}`, false},
		{"untouched weekly, empty description", `{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":1,"description":"","resetTime":` + at(week) + `}`, false},
		{"untouched 5h", `{"bucketId":"gemini-weekly","window":"5h","remainingFraction":1,"resetTime":` + at(fiveHours) + `}`, false},
		{"61s late", `{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":1,"resetTime":` + at(week.Add(61*time.Second)) + `}`, true},
		{"used a little", `{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.99,"resetTime":` + at(week) + `}`, true},
		{"described", `{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":1,"description":"synthetic","resetTime":` + at(week) + `}`, true},
		{"a 5h reset on a weekly bucket", `{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":1,"resetTime":` + at(fiveHours) + `}`, true},
		{"unknown window", `{"bucketId":"gemini-weekly","window":"daily","remainingFraction":1,"resetTime":` + at(now.Add(24*time.Hour)) + `}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := fetchAntigravity(t, antigravityAuth, `{"buckets":[`+tc.bucket+`]}`)
			w := o.Quota.Windows["gemini-weekly"]
			if (w.ResetsAt != nil) != tc.keepsReset {
				t.Fatalf("resets_at = %v; keeps reset should be %v", w.ResetsAt, tc.keepsReset)
			}
			if w.UsedPercent == nil {
				t.Fatal("the bucket lost its reading")
			}
		})
	}
	// The entry's compatibility reset is the window's: unknown, not a date.
	o, _ := fetchAntigravity(t, antigravityAuth, `{"buckets":[{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":1,"resetTime":`+at(week)+`}]}`)
	if o.Percent != 0 || !o.ResetAt.IsZero() || !o.ObservedAt.Equal(now) || len(o.Windows) != 1 || !o.Windows[0].ResetAt.IsZero() {
		t.Fatalf("observation = %+v", o)
	}
}

// The canonical mapping keys on the family and the declared duration.
func TestAntigravityCanonicalMappingPerFamily(t *testing.T) {
	cases := []struct {
		id, window        string
		key, model, title string
	}{
		{"gemini-5h", "5h", client.WindowSession, "", "Session (Gemini)"},
		{"gemini-weekly", "weekly", client.WindowWeekly, "", "Weekly (Gemini)"},
		{"gemini-pro-weekly", "weekly", client.WindowWeekly, "", "Weekly (Gemini)"},
		{"3p-5h", "5h", client.WindowModelSession, client.AntigravityModelShared, "Session (Claude & GPT)"},
		{"3p-weekly", "weekly", client.WindowModelWeekly, client.AntigravityModelShared, "Weekly (Claude & GPT)"},
		{"gemini-argon-5h", "5h", client.WindowModelSession, client.AntigravityModelArgon, "Session (Gemini Argon)"},
		{"gemini-argon-weekly", "weekly", client.WindowModelWeekly, client.AntigravityModelArgon, "Weekly (Gemini Argon)"},
		{"gemini-carbon-5h", "5h", client.WindowModelSession, client.AntigravityModelCarbon, "Session (Gemini Carbon)"},
		{"gemini-carbon-weekly", "weekly", client.WindowModelWeekly, client.AntigravityModelCarbon, "Weekly (Gemini Carbon)"},
		{"gemini-hydrogen-5h", "5h", client.WindowModelSession, client.AntigravityModelHydrogen, "Session (Gemini Hydrogen)"},
		{"gemini-hydrogen-weekly", "weekly", client.WindowModelWeekly, client.AntigravityModelHydrogen, "Weekly (Gemini Hydrogen)"},
		{"gemini-weekly", "daily", "raw:antigravity:gemini-weekly", "", "gemini-weekly"},
		{"3p-5h", "", "raw:antigravity:3p-5h", "", "3p-5h"},
		{"claude-weekly", "weekly", "raw:antigravity:claude-weekly", "", "claude-weekly"},
		{"geminiweekly", "weekly", "raw:antigravity:geminiweekly", "", "geminiweekly"},
	}
	for _, tc := range cases {
		t.Run(tc.id+"/"+tc.window, func(t *testing.T) {
			o, _ := fetchAntigravity(t, antigravityAuth, `{"buckets":[{"bucketId":"`+tc.id+`","window":"`+tc.window+`","remainingFraction":0.4}]}`)
			if len(o.Windows) != 1 {
				t.Fatalf("windows = %+v", o.Windows)
			}
			w := o.Windows[0]
			if w.Key != tc.key || w.Model != tc.model || w.Title != tc.title || w.UsedPercent != 60 || !w.ObservedAt.Equal(now) {
				t.Fatalf("window = %+v; want key %q model %q title %q", w, tc.key, tc.model, tc.title)
			}
			if strings.Contains(w.Title, " · ") {
				t.Fatalf("title %q carries a separator", w.Title)
			}
		})
	}
}

// Every family at once: each lands on its own identity, in the fixed order,
// and the Gemini families Google meters apart never take the default weekly,
// even when their ids sort ahead of it.
func TestAntigravityFamiliesNeverCollideWithTheDefaultWeekly(t *testing.T) {
	var buckets []string
	for _, id := range []string{"gemini-hydrogen", "gemini-argon", "gemini-carbon", "3p", "gemini"} {
		buckets = append(buckets,
			`{"bucketId":"`+id+`-weekly","window":"weekly","remainingFraction":0.1,"resetTime":"2026-09-09T00:00:00Z"}`,
			`{"bucketId":"`+id+`-5h","window":"5h","remainingFraction":0.2,"resetTime":"2026-09-05T00:00:00Z"}`)
	}
	// The default Gemini weekly alone has a distinctive reading.
	body := strings.Replace(`{"buckets":[`+strings.Join(buckets, ",")+`]}`,
		`{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.1`, `{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.7`, 1)
	o, _ := fetchAntigravity(t, antigravityAuth, body)
	type identity struct{ key, model string }
	var got []identity
	for _, w := range o.Windows {
		got = append(got, identity{w.Key, w.Model})
	}
	want := []identity{
		{client.WindowSession, ""}, {client.WindowWeekly, ""},
		{client.WindowModelSession, "argon"}, {client.WindowModelSession, "carbon"}, {client.WindowModelSession, "hydrogen"}, {client.WindowModelSession, "shared"},
		{client.WindowModelWeekly, "argon"}, {client.WindowModelWeekly, "carbon"}, {client.WindowModelWeekly, "hydrogen"}, {client.WindowModelWeekly, "shared"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("identities = %v\nwant %v", got, want)
	}
	if o.Percent != 30 || o.Windows[1].UsedPercent != 30 || !o.ResetAt.Equal(time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("the entry is not the default Gemini weekly: percent %v, weekly %+v", o.Percent, o.Windows[1])
	}
}

// Two default Gemini weeklies reduce to one identity; the later in id order is
// demoted to raw:, and the entry mirrors the one that kept the key.
// A Gemini family Google adds later (one this build does not name) maps to the
// default family too. When the real default weekly is also there it keeps the
// key and Entry.Percent, although "gemini-flash-weekly" sorts before it; the
// stranger is demoted to raw:. Alone, the stranger is the default.
func TestAntigravityDuplicateGeminiWeekliesAreDemoted(t *testing.T) {
	o, _ := fetchAntigravity(t, antigravityAuth, `{"buckets":[`+
		`{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.9},`+
		`{"bucketId":"gemini-flash-weekly","window":"weekly","remainingFraction":0.2}]}`)
	if got := keysOf(o.Windows); !equal(got, []string{client.WindowWeekly, "raw:antigravity:gemini-flash-weekly"}) {
		t.Fatalf("keys = %v", got)
	}
	if o.Percent != 10 || o.Windows[0].UsedPercent != 10 {
		t.Fatalf("the entry (%v) does not mirror the default weekly (%+v)", o.Percent, o.Windows[0])
	}

	o, _ = fetchAntigravity(t, antigravityAuth, `{"buckets":[`+
		`{"bucketId":"gemini-flash-weekly","window":"weekly","remainingFraction":0.2}]}`)
	if got := keysOf(o.Windows); !equal(got, []string{client.WindowWeekly}) || o.Percent != 80 {
		t.Fatalf("a lone unnamed Gemini weekly: keys = %v, percent = %v", got, o.Percent)
	}
}

// Entry.Percent is the default Gemini family's weekly and nothing else: a far
// fuller Claude & GPT pool does not move it.
func TestAntigravityEntryIsTheGeminiWeeklyOnly(t *testing.T) {
	o, _ := fetchAntigravity(t, antigravityAuth, antigravitySummaryBody)
	if o.Percent != 4 || !o.ResetAt.Equal(time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)) || !o.ObservedAt.Equal(now) || o.Provider != "antigravity" {
		t.Fatalf("observation = %+v", o)
	}
	want := []string{client.WindowSession, client.WindowWeekly, client.WindowModelSession, client.WindowModelWeekly}
	if got := keysOf(o.Windows); !equal(got, want) {
		t.Fatalf("keys = %v; want %v", got, want)
	}
	if o.Windows[1].UsedPercent != o.Percent || !o.Windows[1].ResetAt.Equal(o.ResetAt) {
		t.Fatalf("weekly %+v does not mirror the entry", o.Windows[1])
	}
	if w := o.Windows[2]; w.Model != "shared" || w.UsedPercent != 100 || !w.ResetAt.Equal(time.Date(2026, 9, 4, 23, 0, 0, 0, time.UTC)) {
		t.Fatalf("the exhausted shared session = %+v", w)
	}
}

// With no default Gemini weekly the account is a Codex short-window-only
// account: its windows are published, each with its own observation time, and
// the entry-level observation stays empty rather than claiming 0% used. This
// is a success, not an error, so the cache records no last_error and no
// backoff.
func TestAntigravityWithoutAGeminiWeeklyPublishesWindowsOnly(t *testing.T) {
	for name, body := range map[string]string{
		"claude & gpt only": `{"groups":[{"buckets":[` +
			`{"bucketId":"3p-5h","window":"5h","remainingFraction":0.5,"resetTime":"2026-09-05T00:00:00Z"},` +
			`{"bucketId":"3p-weekly","window":"weekly","resetTime":"2026-09-08T00:00:00Z"}]}]}`,
		"gemini session only":  `{"buckets":[{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.5}]}`,
		"hydrogen weekly only": `{"buckets":[{"bucketId":"gemini-hydrogen-weekly","window":"weekly","remainingFraction":0.5}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			o, doer := fetchAntigravity(t, antigravityAuth, body)
			if !o.ObservedAt.IsZero() || o.Percent != 0 || !o.ResetAt.IsZero() {
				t.Fatalf("observation = %+v; an account without a Gemini weekly must report no weekly", o)
			}
			if len(o.Windows) == 0 {
				t.Fatal("the account's windows were not published")
			}
			for _, w := range o.Windows {
				if w.Key == client.WindowWeekly || !w.ObservedAt.Equal(now) {
					t.Fatalf("window = %+v", w)
				}
			}
			if doer.asked(antigravityModelsURL) {
				t.Fatal("usable buckets still cost the model-list request")
			}
		})
	}
}

const antigravityModelsBody = `{"models":{` +
	`"gemini-3-pro":{"modelProvider":"MODEL_PROVIDER_GOOGLE","quotaInfo":{"remainingFraction":0.8,"resetTime":"2026-09-05T02:00:00Z"},"weeklyQuotaInfo":{"remainingFraction":0.6,"resetTime":"2026-09-09T00:00:00Z"}},` +
	`"gemini-3-flash":{"quotaInfo":{"resetTime":"2026-09-05T03:00:00Z"},"weeklyQuotaInfo":{"remainingFraction":0.9,"resetTime":"2026-09-09T00:00:00Z"}},` +
	`"claude-sonnet-5":{"modelProvider":"MODEL_PROVIDER_ANTHROPIC","weeklyQuotaInfo":{"remainingFraction":0.7,"resetTime":"2026-09-08T00:00:00Z"}},` +
	`"gpt-oss-120b":{"modelProvider":"MODEL_PROVIDER_OPENAI","weeklyQuotaInfo":{"remainingFraction":0.4,"resetTime":"2026-09-08T00:00:00Z"},"quotaInfo":{"remainingFraction":1}},` +
	`"opaque-model":{"modelProvider":"MODEL_PROVIDER_OTHER","weeklyQuotaInfo":{"remainingFraction":0}},` +
	`"chat_internal":{"quotaInfo":{"remainingFraction":0}},` +
	`"gemini-empty":{"modelProvider":"MODEL_PROVIDER_GOOGLE"}}}`

// When the summary names no usable bucket, the model list is asked once, with
// the same request, and each family keeps its most-used reading.
func TestAntigravityModelListFallback(t *testing.T) {
	for name, summary := range map[string]string{
		"empty summary":      `{}`,
		"empty groups":       `{"groups":[{"displayName":"Gemini","buckets":[]}]}`,
		"only disabled":      `{"buckets":[{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.5,"disabled":true}]}`,
		"only unusable data": `{"buckets":[{"bucketId":"gemini-weekly","window":"weekly"},{"bucketId":"bad id","remainingFraction":0.5}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			doer := antigravityDoer(map[string]string{antigravitySummaryURL: summary, antigravityModelsURL: antigravityModelsBody})
			o, err := FetchListed(context.Background(), doer, "antigravity", []byte(`{"access_token":"synthetic-secret"}`), Listing{ProjectID: "listed-project"}, now)
			if err != nil {
				t.Fatal(err)
			}
			if len(doer.requests) != 2 || doer.requests[0].URL != antigravitySummaryURL || doer.requests[1].URL != antigravityModelsURL {
				t.Fatalf("urls = %v; want the summary, then the model list", doer.urls)
			}
			first, second := doer.requests[0], doer.requests[1]
			if second.Method != "POST" || !bytes.Equal(first.Body, second.Body) || requestBody(t, second)["project"] != "listed-project" {
				t.Fatalf("model-list request = %s %s", second.Method, second.Body)
			}
			for key := range first.Headers {
				if header(first, key) != header(second, key) {
					t.Fatalf("header %s differs: %q vs %q", key, header(first, key), header(second, key))
				}
			}
			q := o.Quota
			want := map[string]float64{"models/gemini-quota": 100, "models/gemini-weekly": 40, "models/shared-weekly": 60, "models/shared-quota": 0}
			if len(q.Windows) != len(want) {
				t.Fatalf("windows = %v", q.Windows)
			}
			for id, used := range want {
				if got := usedOf(t, q, id); got != used {
					t.Fatalf("%s used %v; want %v", id, got, used)
				}
			}
			if w := q.Windows["models/gemini-weekly"]; w.DurationSeconds == nil || *w.DurationSeconds != 604800 || w.Period != "weekly" ||
				w.ResetsAt == nil || !w.ResetsAt.Equal(time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)) {
				t.Fatalf("models/gemini-weekly = %+v", w)
			}
			// No cadence is invented for a reading that declares none.
			if w := q.Windows["models/gemini-quota"]; w.DurationSeconds != nil || w.Period != "" || w.ResetsAt == nil {
				t.Fatalf("models/gemini-quota = %+v", w)
			}
			wantKeys := []string{client.WindowWeekly, client.WindowModelWeekly, "raw:antigravity:models/gemini-quota", "raw:antigravity:models/shared-quota"}
			if got := keysOf(o.Windows); !equal(got, wantKeys) {
				t.Fatalf("keys = %v; want %v", got, wantKeys)
			}
			if o.Windows[0].Title != "Weekly (Gemini)" || o.Windows[1].Model != "shared" || o.Windows[1].Title != "Weekly (Claude & GPT)" {
				t.Fatalf("windows = %+v", o.Windows)
			}
			if o.Percent != 40 || !o.ObservedAt.Equal(now) || !o.ResetAt.Equal(time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)) {
				t.Fatalf("the entry is not the fallback's Gemini weekly: %+v", o)
			}
		})
	}
}

// A model-list request that fails leaves the summary's empty answer: the poll
// has nothing to report and fails, as any response with no window does. It
// never turns into the poll's own HTTP error.
func TestAntigravityFailedFallbackLeavesNoWindow(t *testing.T) {
	for name, doer := range map[string]*recordingDoer{
		"rate limited":  {routingDoer: routingDoer{bodies: map[string]string{antigravitySummaryURL: `{}`}, status: map[string]int{antigravityModelsURL: 429}}},
		"server error":  {routingDoer: routingDoer{bodies: map[string]string{antigravitySummaryURL: `{}`}, status: map[string]int{antigravityModelsURL: 500}}},
		"transport":     {routingDoer: routingDoer{bodies: map[string]string{antigravitySummaryURL: `{}`}, err: map[string]error{antigravityModelsURL: errors.New("unreachable")}}},
		"unreadable":    {routingDoer: routingDoer{bodies: map[string]string{antigravitySummaryURL: `{}`, antigravityModelsURL: `not json`}}},
		"no models":     {routingDoer: routingDoer{bodies: map[string]string{antigravitySummaryURL: `{}`, antigravityModelsURL: `{"models":{}}`}}},
		"other vendors": {routingDoer: routingDoer{bodies: map[string]string{antigravitySummaryURL: `{}`, antigravityModelsURL: `{"models":{"x":{"modelProvider":"MODEL_PROVIDER_OTHER","weeklyQuotaInfo":{"remainingFraction":0}}}}`}}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Fetch(context.Background(), doer, "antigravity", []byte(antigravityAuth), now)
			if !errors.Is(err, ErrNoWeeklyWindow) {
				t.Fatalf("err = %v; want ErrNoWeeklyWindow", err)
			}
			if len(doer.urls) != 2 {
				t.Fatalf("urls = %v; want the summary and one model-list request, never a retry", doer.urls)
			}
		})
	}
}

// A summary that is refused or unreadable fails the poll on its own, with a
// static error, and the model list is never asked.
func TestAntigravityErrorsAreStaticAndNeverFallBack(t *testing.T) {
	secret := "SECRET-TOKEN-VALUE"
	auth := `{"access_token":"` + secret + `","project_id":"p1"}`
	for _, tc := range []struct {
		name     string
		response protocol.HostHTTPResponse
		err      error
		want     error
	}{
		{"http 401", protocol.HostHTTPResponse{StatusCode: 401, Body: []byte(`{"error":"` + secret + `"}`)}, nil, HTTPStatusError{StatusCode: 401}},
		{"http 403", protocol.HostHTTPResponse{StatusCode: 403, Body: []byte(`{"error":{"status":"PERMISSION_DENIED","message":"` + secret + `"}}`)}, nil, HTTPStatusError{StatusCode: 403}},
		{"http 404", protocol.HostHTTPResponse{StatusCode: 404}, nil, HTTPStatusError{StatusCode: 404}},
		{"http 429", protocol.HostHTTPResponse{StatusCode: 429}, nil, HTTPStatusError{StatusCode: 429}},
		{"invalid json", protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`not json ` + secret)}, nil, ErrInvalidResponse},
		{"oversized", protocol.HostHTTPResponse{StatusCode: 200, Body: bytes.Repeat([]byte(" "), maxResponseBytes+1)}, nil, ErrInvalidResponse},
		{"transport", protocol.HostHTTPResponse{}, errors.New("dial failed"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &sequenceDoer{responses: []protocol.HostHTTPResponse{tc.response}, errs: []error{tc.err}}
			_, err := Fetch(context.Background(), fake, "antigravity", []byte(auth), now)
			if err == nil {
				t.Fatal("expected an error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) && err.Error() != tc.want.Error() {
				t.Fatalf("err = %v; want %v", err, tc.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaked the secret: %v", err)
			}
			if fake.calls != 1 {
				t.Fatalf("%d requests; a failed summary must not fall back or retry", fake.calls)
			}
		})
	}
}

// sequenceDoer answers its requests in order and counts them.
type sequenceDoer struct {
	responses []protocol.HostHTTPResponse
	errs      []error
	calls     int
}

func (s *sequenceDoer) HTTPDo(context.Context, protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	i := s.calls
	s.calls++
	if i >= len(s.responses) {
		return protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{}`)}, nil
	}
	return s.responses[i], s.errs[i]
}

// Too many buckets are bounded like every other provider's windows.
func TestAntigravityBucketsAreBounded(t *testing.T) {
	var buckets []string
	for i := 0; i < 3*maxDetails; i++ {
		buckets = append(buckets, fmt.Sprintf(`{"bucketId":"x-%03d","window":"weekly","remainingFraction":0.5}`, i))
	}
	o, _ := fetchAntigravity(t, antigravityAuth, `{"buckets":[`+strings.Join(buckets, ",")+`]}`)
	if !o.Quota.Truncated || len(o.Quota.Windows) != maxDetails {
		t.Fatalf("truncated=%v windows=%d", o.Quota.Truncated, len(o.Quota.Windows))
	}
}

// The snapshot is rewritten on every poll; an unchanged answer must produce
// byte-identical windows.
func TestAntigravityRepeatedParsesAreByteIdentical(t *testing.T) {
	first, _ := fetchAntigravity(t, antigravityAuth, antigravitySummaryBody)
	want, _ := json.Marshal(first)
	for i := 0; i < 40; i++ {
		again, _ := fetchAntigravity(t, antigravityAuth, antigravitySummaryBody)
		if got, _ := json.Marshal(again); !bytes.Equal(got, want) {
			t.Fatalf("parse %d differs\nfirst: %s\nagain: %s", i, want, got)
		}
	}
	doer := antigravityDoer(map[string]string{antigravitySummaryURL: `{}`, antigravityModelsURL: antigravityModelsBody})
	firstModels, err := Fetch(context.Background(), doer, "antigravity", []byte(antigravityAuth), now)
	if err != nil {
		t.Fatal(err)
	}
	wantModels, _ := json.Marshal(firstModels)
	for i := 0; i < 40; i++ {
		again, _ := Fetch(context.Background(), doer, "antigravity", []byte(antigravityAuth), now)
		if got, _ := json.Marshal(again); !bytes.Equal(got, wantModels) {
			t.Fatalf("fallback parse %d differs", i)
		}
	}
}

// The plan is read from loadCodeAssist on the account-details schedule, with
// the request CPA's executor makes.
func TestAntigravityPlanFromLoadCodeAssist(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"paid tier", `{"currentTier":{"id":"free-tier","name":"Antigravity"},"paidTier":{"id":"g1-pro-tier","name":"Google AI Pro","availableCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":"100"}]},"cloudaicompanionProject":"synthetic-project"}`, "Google AI Pro"},
		{"current tier when no paid tier", `{"currentTier":{"id":"free-tier","name":"Antigravity Free"}}`, "Antigravity Free"},
		{"current tier when the paid name is invalid", `{"currentTier":{"name":"Antigravity Free"},"paidTier":{"id":"g1-pro-tier","name":"bad\nname"}}`, "Antigravity Free"},
		{"current tier when the paid tier has no name", `{"currentTier":{"name":"Antigravity Free"},"paidTier":{"id":"g1-pro-tier"}}`, "Antigravity Free"},
		{"ids are never the plan", `{"currentTier":{"id":"free-tier"},"paidTier":{"id":"g1-pro-tier"}}`, ""},
		{"nothing named", `{}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := antigravityDoer(map[string]string{antigravityLoadURL: tc.body})
			known := &client.AccountDetails{CheckedAt: now.Add(-7 * time.Hour), Plan: "kept-plan"}
			details := RefreshDetails(context.Background(), doer, "antigravity", []byte(antigravityAuth), known, now)
			if details == nil || details.Plan != tc.want || !details.CheckedAt.Equal(now) || details.RenewalAt != nil || details.SubscriptionStartedAt != nil {
				t.Fatalf("details = %+v; want plan %q", details, tc.want)
			}
			if len(doer.requests) != 1 {
				t.Fatalf("urls = %v; want loadCodeAssist alone", doer.urls)
			}
			request := doer.requests[0]
			if request.Method != "POST" || request.URL != antigravityLoadURL || string(request.Body) != `{"metadata":{"ideType":"ANTIGRAVITY"}}` {
				t.Fatalf("request = %s %s %s", request.Method, request.URL, request.Body)
			}
			if header(request, "Authorization") != "Bearer synthetic-secret" || header(request, "Content-Type") != "application/json" ||
				header(request, "Accept") != "application/json" || header(request, "User-Agent") != antigravityUserAgent {
				t.Fatalf("headers = %v", request.Headers)
			}
			raw, _ := json.Marshal(details)
			for _, gone := range []string{"g1-pro-tier", "free-tier", "synthetic-project", "GOOGLE_ONE_AI", "creditAmount"} {
				if bytes.Contains(raw, []byte(gone)) {
					t.Fatalf("details keep %q: %s", gone, raw)
				}
			}
		})
	}
	// The plan request honours the same validated base_url and user_agent.
	auth := `{"access_token":"synthetic-secret","base_url":"https://cloudcode-pa.googleapis.com","user_agent":"synthetic-agent/1.0"}`
	doer := antigravityDoer(map[string]string{"https://cloudcode-pa.googleapis.com" + antigravityLoadCodeAssistPath: `{"paidTier":{"name":"Google AI Ultra"}}`})
	if details := RefreshDetails(context.Background(), doer, "antigravity", []byte(auth), nil, now); details == nil || details.Plan != "Google AI Ultra" ||
		header(doer.requests[0], "User-Agent") != "synthetic-agent/1.0" {
		t.Fatalf("details = %+v requests = %+v", details, doer.requests)
	}
}

// loadCodeAssist failures keep the last good plan, it is read at most once
// per interval, and the plan reaches the observation through ApplyDetails.
func TestAntigravityPlanFollowsTheAccountDetailRules(t *testing.T) {
	known := func() *client.AccountDetails {
		return &client.AccountDetails{CheckedAt: now.Add(-7 * time.Hour), Plan: "Google AI Pro"}
	}
	for name, doer := range map[string]*routingDoer{
		"rate limited": {status: map[string]int{antigravityLoadURL: 429}},
		"refused":      {status: map[string]int{antigravityLoadURL: 401}},
		"transport":    {err: map[string]error{antigravityLoadURL: errors.New("unreachable")}},
		"unreadable":   {bodies: map[string]string{antigravityLoadURL: `<html>`}},
	} {
		details := RefreshDetails(context.Background(), doer, "antigravity", []byte(antigravityAuth), known(), now)
		if details == nil || details.Plan != "Google AI Pro" || !details.CheckedAt.Equal(now) {
			t.Fatalf("%s: details = %+v; the last good plan was lost", name, details)
		}
	}
	doer := &routingDoer{bodies: map[string]string{antigravityLoadURL: `{"paidTier":{"name":"Google AI Ultra"}}`}}
	first := RefreshDetails(context.Background(), doer, "antigravity", []byte(antigravityAuth), nil, now)
	if again := RefreshDetails(context.Background(), doer, "antigravity", []byte(antigravityAuth), first, now.Add(DetailsInterval-time.Second)); again != first || len(doer.urls) != 1 {
		t.Fatalf("asked again within the interval: %v", doer.urls)
	}
	o, _ := fetchAntigravity(t, antigravityAuth, antigravitySummaryBody)
	if o.Plan != "" {
		t.Fatalf("plan = %q before any account details", o.Plan)
	}
	o.ApplyDetails(first, now)
	if o.Plan != "Google AI Ultra" {
		t.Fatalf("plan = %q; the loadCodeAssist plan was not applied", o.Plan)
	}
}

// The unexported bounds directly, so a change to one is deliberate.
func TestAntigravityValidators(t *testing.T) {
	for raw, want := range map[string]string{
		"":                                       antigravityDailyBase,
		"https://cloudcode-pa.googleapis.com":    "https://cloudcode-pa.googleapis.com",
		"https://cloudcode-pa.googleapis.com/":   "https://cloudcode-pa.googleapis.com",
		"https://cloudcode-pa.googleapis.com/v1": "https://cloudcode-pa.googleapis.com/v1",
		"https://a..googleapis.com":              antigravityDailyBase,
		"https://-a.googleapis.com":              antigravityDailyBase,
		"https://a_b.googleapis.com":             antigravityDailyBase,
		"https://[::1]":                          antigravityDailyBase,
		"https://cloudcode-pa.googleapis.com?":   antigravityDailyBase,
		"ftp://cloudcode-pa.googleapis.com":      antigravityDailyBase,
		"https:cloudcode-pa.googleapis.com":      antigravityDailyBase,
		"https://cloudcode-pa.googleapis.com\n":  antigravityDailyBase,
	} {
		if got := antigravityBaseURL(raw); got != want {
			t.Errorf("antigravityBaseURL(%q) = %q; want %q", raw, got, want)
		}
	}
	for raw, want := range map[string]string{
		"rising-fact-p41fc":      "rising-fact-p41fc",
		" padded-project ":       "padded-project",
		"example.com:project":    "example.com:project",
		"":                       "",
		"has space":              "",
		"quote\"":                "",
		strings.Repeat("p", 129): "",
	} {
		if got := projectID(raw); got != want {
			t.Errorf("projectID(%q) = %q; want %q", raw, got, want)
		}
	}
	if got := antigravityUserAgentOf(strings.Repeat("a", maxAntigravityUserAgent)); len(got) != maxAntigravityUserAgent {
		t.Errorf("a %d-byte user_agent was refused", maxAntigravityUserAgent)
	}
}
