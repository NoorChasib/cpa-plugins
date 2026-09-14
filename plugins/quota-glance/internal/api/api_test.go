package api

import (
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/aggregate"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
)

const testToken = "8Zx1q-test-token-not-a-real-secret"

func newTestAPI() *API {
	a := New("quota-glance", testToken)
	a.Publish(aggregate.Document{
		SchemaVersion: 1, GeneratedAtEpoch: 1789012800,
		Credentials: []aggregate.Credential{{ID: "claude-a@example.com.json"}},
		Providers:   []aggregate.Provider{},
	}, Health{Version: "test"})
	return a
}

func get(a *API, path string, headers http.Header) protocol.ManagementResponse {
	if headers == nil {
		headers = http.Header{}
	}
	return a.Handle(protocol.ManagementRequest{Method: "GET", Path: path, Headers: headers}, time.Now())
}

func bearer(token string) http.Header {
	return http.Header{"Authorization": {"Bearer " + token}}
}

func TestSummaryRequiresTheTokenAndFailsBare(t *testing.T) {
	a := newTestAPI()
	const path = "/v0/resource/plugins/quota-glance/summary"

	for _, tc := range []struct {
		name    string
		headers http.Header
	}{
		{"no header", http.Header{}},
		{"wrong token", bearer("wrong")},
		{"empty bearer", bearer("")},
		{"no scheme", http.Header{"Authorization": {testToken}}},
		// A prefix of the real token must not pass.
		{"prefix", bearer(testToken[:len(testToken)-1])},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := get(a, path, tc.headers)
			if res.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d; want 401", res.StatusCode)
			}
			// Bare: no body, and nothing hinting at why it failed.
			if len(res.Body) != 0 {
				t.Fatalf("401 carried a body: %s", res.Body)
			}
		})
	}

	res := get(a, path, bearer(testToken))
	if res.StatusCode != http.StatusOK || len(res.Body) == 0 {
		t.Fatalf("valid token rejected: %d", res.StatusCode)
	}
}

func TestETagYields304(t *testing.T) {
	a := newTestAPI()
	const path = "/v0/resource/plugins/quota-glance/summary"
	first := get(a, path, bearer(testToken))
	etag := first.Headers.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	headers := bearer(testToken)
	headers.Set("If-None-Match", etag)
	second := a.Handle(protocol.ManagementRequest{Method: "GET", Path: path, Headers: headers}, time.Now())
	if second.StatusCode != http.StatusNotModified || len(second.Body) != 0 {
		t.Fatalf("status = %d, body = %d bytes; want 304 and no body", second.StatusCode, len(second.Body))
	}
	// A new document must invalidate the old tag.
	a.Publish(aggregate.Document{SchemaVersion: 1, GeneratedAtEpoch: 1789012900}, Health{})
	third := a.Handle(protocol.ManagementRequest{Method: "GET", Path: path, Headers: headers}, time.Now())
	if third.StatusCode != http.StatusOK {
		t.Fatalf("stale ETag still matched: %d", third.StatusCode)
	}
	// An unauthenticated conditional request must not reveal the tag either.
	anon := http.Header{"If-None-Match": {etag}}
	if res := get(a, path, anon); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d; a conditional request must still authenticate", res.StatusCode)
	}
}

// Rate limiting slows a brute force. It must never ban: a lockout on the user's
// own dashboard is a worse outcome than a slow attack on a long random token.
func TestRateLimitThrottlesButNeverBans(t *testing.T) {
	a := newTestAPI()
	const path = "/v0/resource/plugins/quota-glance/summary"
	headers := bearer("wrong")
	headers.Set("X-Forwarded-For", "203.0.113.7")
	start := time.Unix(1789012800, 0)

	limited := 0
	for i := 0; i < rateLimit+5; i++ {
		res := a.Handle(protocol.ManagementRequest{Method: "GET", Path: path, Headers: headers}, start)
		if res.StatusCode == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited != 5 {
		t.Fatalf("throttled %d of %d over the limit; want 5", limited, 5)
	}

	// The next window serves the real token again: no lockout carried over.
	valid := bearer(testToken)
	valid.Set("X-Forwarded-For", "203.0.113.7")
	res := a.Handle(protocol.ManagementRequest{Method: "GET", Path: path, Headers: valid}, start.Add(rateWindow))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d after the window elapsed; failed attempts must not ban", res.StatusCode)
	}

	// One caller's bucket must not throttle another.
	other := bearer(testToken)
	other.Set("X-Forwarded-For", "203.0.113.9")
	if res := a.Handle(protocol.ManagementRequest{Method: "GET", Path: path, Headers: other}, start); res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; buckets are per address", res.StatusCode)
	}
}

func TestRoutesAreExactAndGETOnly(t *testing.T) {
	a := newTestAPI()
	for _, path := range []string{
		"/v0/resource/plugins/quota-glance/summary/",
		"/v0/resource/plugins/quota-glance/summary/extra",
		"/v0/resource/plugins/quota-glance/",
		"/v0/resource/plugins/quota-glance",
		"/v0/resource/plugins/quota-glance/../summary",
		"/v0/management/plugins/quota-glance/status",
	} {
		if res := get(a, path, bearer(testToken)); res.StatusCode != http.StatusNotFound {
			t.Fatalf("%s -> %d; resource paths match exactly, with no prefix matching", path, res.StatusCode)
		}
	}
	res := a.Handle(protocol.ManagementRequest{
		Method: "POST", Path: "/v0/resource/plugins/quota-glance/summary", Headers: bearer(testToken),
	}, time.Now())
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("POST -> %d; resource routes are GET only", res.StatusCode)
	}
}

// The app shell is served without authentication because CPA runs none on a
// resource route. It must therefore carry no data at all.
func TestAppShellIsPublicAndDataFree(t *testing.T) {
	a := newTestAPI()
	res := get(a, "/v0/resource/plugins/quota-glance/app", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	body := string(res.Body)
	for _, secret := range []string{testToken, "claude-a@example.com.json", "1789012800"} {
		if strings.Contains(body, secret) {
			t.Fatalf("the public shell leaked %q", secret)
		}
	}
	for _, header := range []string{"Content-Security-Policy", "X-Content-Type-Options", "ETag"} {
		if res.Headers.Get(header) == "" {
			t.Fatalf("missing %s", header)
		}
	}
}

func TestManagementRoutesServeDiagnostics(t *testing.T) {
	a := newTestAPI()
	// CPA's management middleware has already required the management key, so
	// no plugin token is presented here.
	if res := get(a, "/v0/management/plugins/quota-glance/health", nil); res.StatusCode != http.StatusOK ||
		!strings.Contains(string(res.Body), `"watcher"`) {
		t.Fatalf("health = %d %s", res.StatusCode, res.Body)
	}
	if res := get(a, "/v0/management/plugins/quota-glance/windows", nil); res.StatusCode != http.StatusOK ||
		!strings.Contains(string(res.Body), `"windows"`) {
		t.Fatalf("windows = %d %s", res.StatusCode, res.Body)
	}
}

// The token is compared in constant time and never held in recoverable form.
func TestTokenIsHashedComparedInConstantTimeAndNeverEmitted(t *testing.T) {
	source, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "subtle.ConstantTimeCompare") {
		t.Fatal("token comparison must use crypto/subtle.ConstantTimeCompare")
	}

	a := newTestAPI()
	// Only a digest is retained: the struct holds no copy of the token bytes.
	if strings.Contains(string(a.tokenHash[:]), testToken) {
		t.Fatal("the token itself is retained")
	}
	// Nothing the API emits on any route contains the token.
	for _, path := range []string{
		"/v0/resource/plugins/quota-glance/summary",
		"/v0/resource/plugins/quota-glance/app",
		"/v0/management/plugins/quota-glance/health",
		"/v0/management/plugins/quota-glance/windows",
		"/v0/resource/plugins/quota-glance/missing",
	} {
		res := get(a, path, bearer(testToken))
		emitted := string(res.Body)
		for _, values := range res.Headers {
			emitted += strings.Join(values, " ")
		}
		if strings.Contains(emitted, testToken) {
			t.Fatalf("%s echoed the token", path)
		}
	}
}

// api serves what the runtime published. It must not be able to read a file or
// start a watch, so a request can never trigger I/O.
func TestAPIDoesNotDependOnSourceOrWatch(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps",
		"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/api").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, forbidden := range []string{
		"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/source",
		"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/watch",
		"github.com/fsnotify/fsnotify",
	} {
		for _, dep := range strings.Fields(string(out)) {
			if dep == forbidden {
				t.Fatalf("internal/api depends on %s; serving must not be able to read or watch the filesystem", forbidden)
			}
		}
	}
}

// Until a token is configured the summary route is closed, not open. A blank
// credential must never satisfy it, and "Bearer " plus whitespace trims to the
// empty string.
func TestUnconfiguredTokenClosesTheRouteRatherThanOpeningIt(t *testing.T) {
	a := New("quota-glance", "")
	const path = "/v0/resource/plugins/quota-glance/summary"
	for _, value := range []string{"", " ", "   ", "\t"} {
		res := get(a, path, http.Header{"Authorization": {"Bearer " + value}})
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("Bearer %q -> %d; an unset token must not authenticate", value, res.StatusCode)
		}
	}
	// And a blank value is still refused once a real token exists.
	a.SetToken(testToken)
	if res := get(a, path, http.Header{"Authorization": {"Bearer  "}}); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if res := get(a, path, bearer(testToken)); res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
}

// The shell is immutable for a given build, so a browser should be able to
// revalidate it rather than re-download the whole inlined bundle on every
// navigation.
func TestAppShellRevalidates(t *testing.T) {
	a := newTestAPI()
	const path = "/v0/resource/plugins/quota-glance/app"
	first := get(a, path, nil)
	etag := first.Headers.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the shell")
	}
	if got := first.Headers.Get("Cache-Control"); got == "no-store" {
		t.Fatalf("Cache-Control = %q; no-store prevents revalidation entirely", got)
	}
	second := get(a, path, http.Header{"If-None-Match": {etag}})
	if second.StatusCode != http.StatusNotModified || len(second.Body) != 0 {
		t.Fatalf("status = %d with %d bytes; want 304 and no body", second.StatusCode, len(second.Body))
	}
	// Revalidation must stay unauthenticated, like the shell itself.
	if third := get(a, path, http.Header{"If-None-Match": {`"stale"`}}); third.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", third.StatusCode)
	}
}
