package api

import (
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/aggregate"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
)

const summaryPath = "/v0/management/plugins/quota-glance/summary"

func newTestAPI() *API {
	a := New("quota-glance")
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

// The document lives on the management tree, which CPA authenticates with the
// full management key before this plugin sees the request. It must not also be
// reachable from the public resource tree, where CPA authenticates nothing —
// that route existing at all would publish every credential in the pool.
func TestSummaryIsOnlyOnTheAuthenticatedTree(t *testing.T) {
	a := newTestAPI()

	if res := get(a, "/v0/resource/plugins/quota-glance/summary", nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("the public resource tree served the document: %d", res.StatusCode)
	}
	res := get(a, summaryPath, nil)
	if res.StatusCode != http.StatusOK || len(res.Body) == 0 {
		t.Fatalf("management summary: %d, %d bytes", res.StatusCode, len(res.Body))
	}
	if got := res.Headers.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q; the document must not be cached by a proxy", got)
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
