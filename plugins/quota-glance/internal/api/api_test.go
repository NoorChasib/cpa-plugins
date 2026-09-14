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

const (
	summaryPath = "/v0/management/plugins/quota-glance/summary"
	// The fallback path, for a reader with no CPA console session.
	tokenPath = "/v0/resource/plugins/quota-glance/summary"
	testToken = "8Zx1q-test-token-not-a-real-secret"
)

func bearer(token string) http.Header {
	return http.Header{"Authorization": {"Bearer " + token}}
}

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

// Two paths to one document, each with its own gate. The management path is
// CPA's: its middleware has required the full management key before this plugin
// sees the request, so nothing further is checked here. The resource path is
// this plugin's, because CPA authenticates nothing there.
func TestBothSummaryPathsServeTheSameDocumentBehindTheirOwnGate(t *testing.T) {
	a := newTestAPI()

	viaCPA := get(a, summaryPath, nil)
	if viaCPA.StatusCode != http.StatusOK || len(viaCPA.Body) == 0 {
		t.Fatalf("management summary: %d, %d bytes", viaCPA.StatusCode, len(viaCPA.Body))
	}
	if res := get(a, tokenPath, nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the public path served the document unauthenticated: %d", res.StatusCode)
	}
	viaToken := get(a, tokenPath, bearer(testToken))
	if viaToken.StatusCode != http.StatusOK {
		t.Fatalf("token summary: %d", viaToken.StatusCode)
	}
	if string(viaCPA.Body) != string(viaToken.Body) {
		t.Fatal("the two paths served different documents")
	}
	if viaCPA.Headers.Get("ETag") != viaToken.Headers.Get("ETag") {
		t.Fatal("the two paths disagree on the ETag for one document")
	}
	for _, res := range []protocol.ManagementResponse{viaCPA, viaToken} {
		if got := res.Headers.Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q; the document must not be cached by a proxy", got)
		}
	}
}

func TestETagYields304(t *testing.T) {
	a := newTestAPI()
	first := get(a, summaryPath, nil)
	etag := first.Headers.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	headers := http.Header{"If-None-Match": {etag}}
	second := get(a, summaryPath, headers)
	if second.StatusCode != http.StatusNotModified || len(second.Body) != 0 {
		t.Fatalf("status = %d, body = %d bytes; want 304 and no body", second.StatusCode, len(second.Body))
	}
	// A new document must invalidate the old tag.
	a.Publish(aggregate.Document{SchemaVersion: 1, GeneratedAtEpoch: 1789012900}, Health{})
	third := get(a, summaryPath, headers)
	if third.StatusCode != http.StatusOK {
		t.Fatalf("stale ETag still matched: %d", third.StatusCode)
	}
}

func TestRoutesAreExactAndGETOnly(t *testing.T) {
	a := newTestAPI()
	for _, path := range []string{
		"/v0/management/plugins/quota-glance/summary/",
		"/v0/management/plugins/quota-glance/summary/extra",
		"/v0/resource/plugins/quota-glance/summary/",
		"/v0/resource/plugins/quota-glance/app/",
		"/v0/resource/plugins/quota-glance/",
		"/v0/resource/plugins/quota-glance",
		"/v0/management/plugins/quota-glance/../summary",
		"/v0/management/plugins/quota-glance/status",
	} {
		if res := get(a, path, nil); res.StatusCode != http.StatusNotFound {
			t.Fatalf("%s -> %d; paths match exactly, with no prefix matching", path, res.StatusCode)
		}
	}
	res := a.Handle(protocol.ManagementRequest{Method: "POST", Path: summaryPath}, time.Now())
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("POST -> %d; these routes are GET only", res.StatusCode)
	}
}

func TestSummaryRequiresTheTokenAndFailsBare(t *testing.T) {
	a := newTestAPI()
	const path = tokenPath

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

// Throttling must slow a brute force without ever being able to lock the
// operator out. The request carries no peer address — only headers the caller
// supplies — so anything keyed on those is both trivially rotated and trivially
// forged as the operator's own address. Failures are therefore counted
// globally, and a correct token is never throttled at all.
func TestValidTokenIsNeverThrottledAndFailuresAreLimited(t *testing.T) {
	a := newTestAPI()
	const path = tokenPath
	start := time.Unix(1789012800, 0)

	limited := 0
	for i := 0; i < failureLimit*20; i++ {
		headers := bearer("wrong")
		// Rotate every header an attacker controls.
		headers.Set("X-Forwarded-For", "203.0.113."+string(rune('0'+i%10)))
		headers.Set("X-Real-Ip", "198.51.100.1")
		res := a.Handle(protocol.ManagementRequest{Method: "GET", Path: path, Headers: headers}, start)
		if res.StatusCode == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 {
		t.Fatal("rotating caller-supplied headers bypassed the limit entirely")
	}

	// The operator, mid-flood, with the correct token. This must always work.
	if res := a.Handle(protocol.ManagementRequest{
		Method: "GET", Path: path, Headers: bearer(testToken),
	}, start); res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; a correct token must never be throttled", res.StatusCode)
	}

	// Forging the operator's address cannot throttle them either.
	forged := bearer(testToken)
	forged.Set("X-Forwarded-For", "203.0.113.7")
	if res := a.Handle(protocol.ManagementRequest{Method: "GET", Path: path, Headers: forged}, start); res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; a forged address must not lock anyone out", res.StatusCode)
	}

	// The window rolls over; failures are never a lasting ban.
	next := start.Add(rateWindow)
	if res := a.Handle(protocol.ManagementRequest{
		Method: "GET", Path: path, Headers: bearer("wrong"),
	}, next); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401 once the window elapsed", res.StatusCode)
	}
}

// The limiter must retain nothing derived from caller input.
func TestLimiterRetainsNoCallerSuppliedData(t *testing.T) {
	a := newTestAPI()
	const path = tokenPath
	huge := strings.Repeat("a", 256*1024)
	for i := 0; i < 200; i++ {
		headers := bearer("wrong")
		headers.Set("X-Forwarded-For", huge+string(rune(i)))
		a.Handle(protocol.ManagementRequest{Method: "GET", Path: path, Headers: headers}, time.Unix(1789012800, 0))
	}
	// A counter, not a map keyed on unbounded input.
	if a.limiter.count == 0 {
		t.Fatal("failures were not counted")
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
		tokenPath,
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

// Until a token is configured the summary route is closed, not open. A blank
// credential must never satisfy it, and "Bearer " plus whitespace trims to the
// empty string.
func TestUnconfiguredTokenClosesTheRouteRatherThanOpeningIt(t *testing.T) {
	a := New("quota-glance", "")
	const path = tokenPath
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

// The app shell is served without authentication because CPA runs none on a
// resource route. It must therefore carry no data at all.
func TestAppShellIsPublicAndDataFree(t *testing.T) {
	a := newTestAPI()
	res := get(a, "/v0/resource/plugins/quota-glance/app", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	body := string(res.Body)
	for _, secret := range []string{"claude-a@example.com.json", "1789012800"} {
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
	// nothing is presented here.
	if res := get(a, "/v0/management/plugins/quota-glance/health", nil); res.StatusCode != http.StatusOK ||
		!strings.Contains(string(res.Body), `"watcher"`) {
		t.Fatalf("health = %d %s", res.StatusCode, res.Body)
	}
	if res := get(a, "/v0/management/plugins/quota-glance/windows", nil); res.StatusCode != http.StatusOK ||
		!strings.Contains(string(res.Body), `"windows"`) {
		t.Fatalf("windows = %d %s", res.StatusCode, res.Body)
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
