package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/account-health-pushover/internal/protocol"
)

// The console sessions below were produced by the official Management
// Center's own obfuscateData (src/utils/encryption.ts in
// github.com/router-for-me/Cli-Proxy-API-Management-Center) for host
// consoleTestHost and user agent consoleTestUserAgent.
const (
	consoleTestHost      = "127.0.0.1:8317"
	consoleTestUserAgent = "Mozilla/5.0 (X11; Linux x86_64) TestUA/1"
	consoleTestOtherUA   = "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) OtherUA/2"
	consoleTestKey       = "test-management-key-0123456789"
	consoleTestLegacyKey = "legacy-management-key-0123456789"

	// Upstream tag v1.27.2 (commit f154fa9380825c4fe94bc577a9abaa7e505f8875):
	// "cli-proxy-auth" after signing in with Remember password on, i.e.
	// {"state":{"apiBase":"http://127.0.0.1:8317","managementKey":"test-management-key-0123456789","rememberPassword":true,"serverVersion":"8.0.53","serverBuildDate":null},"version":0}
	consoleSessionV2 = "enc::v2::GE4aWREGClpDVkMRGUQ1BBEQSwAYGxEXBUhKAkJGWFxRSVVSAAgPHQEZEgITV1ldUFAGAQxDBDkKAVsXQwQMXgNIDxQHW10WCAYbBkhGFg1CQlBVVkgEBAAWCQwcDENfVVZcVQYeOUwDARgXC0lDSh1fAgBOVxpfSAUAESMXF14aGwFQW0VdUgEcAh0SAhJdVEhOVkN1FgUFSTQTGx1bFw8FBUEKSUADDEhJGgoNV0hVUA=="

	// Upstream tag v1.27.2 (commit f154fa9380825c4fe94bc577a9abaa7e505f8875):
	// "cli-proxy-auth" after signing in with Remember password off; the
	// store's partialize leaves managementKey out, i.e.
	// {"state":{"apiBase":"http://127.0.0.1:8317","rememberPassword":false,"serverVersion":"8.0.53","serverBuildDate":null},"version":0}
	consoleSessionV2NoRemember = "enc::v2::GE4aWREGClpDVkMRGUQ1BBEQSwAYGxEXBUhKAkJGWFxRSVVSAAgPHQEZEgITSF1eVFoBCRt9EQEcDxZfBVJTSxYJERBFGEkWFxUQADNIAQcGHQ9FX14JHAcABR0SAhNJXUFHUhEuHEQcFisZDUhDSgdYGwkfWUtMXwEWChocRxdDCQ=="

	// Upstream tag v1.26.7 (the last enc::v1:: release): the same remembered
	// session as consoleSessionV2, keyed to consoleTestUserAgent as well.
	consoleSessionV1 = "enc::v1::GE4aWREGClpDVkMRGUQ1BBEQSwAYGxEXBUhKAkJGWFxRSVVSAAgPHQEZEgITV1ldUFAZIAoUHScJGA0PDERFWywcXFpOLQ4LGB1ODBVdOk8ZGRFmVkdBY3YXCEFAS18VHwoVG0gTIAheBBINBw0YAAcXFhBeR14WBhkXEzEADkJbWEASFBIWHwoWBgIVUG8cHxsaCRNtQEdcRGw5RVQZGiIcAhkFDFpOUy1FXUZOdl9DCQ=="

	// Upstream tag v1.26.7: the legacy "managementKey" entry, written as
	// obfuscatedStorage.setItem("managementKey", consoleTestLegacyKey).
	consoleLegacyKeyV1 = "enc::v1::QQAMShERFlUUTA8RDkgaAAwBRFFfCkhTREBWGUZCWEpYRQ=="
)

// browserAuthScriptDriver runs the embedded script in a fresh node:vm context
// that exposes only a fake window plus the browser globals the script uses,
// so a reference to any other global fails instead of resolving to node's.
const browserAuthScriptDriver = `"use strict";
const fs = require("node:fs");
const vm = require("node:vm");
const [scriptPath, casesPath, namespace] = process.argv.slice(2);
const source = fs.readFileSync(scriptPath, "utf8");
const cases = JSON.parse(fs.readFileSync(casesPath, "utf8"));
const results = cases.map((c) => {
	const store = new Map(Object.entries(c.storage || {}));
	const window = {
		location: { host: c.host, pathname: c.pathname },
		navigator: { userAgent: c.userAgent },
		localStorage: {
			getItem: (k) => (store.has(String(k)) ? store.get(String(k)) : null),
			setItem: (k, v) => { store.set(String(k), String(v)); },
			removeItem: (k) => { store.delete(String(k)); },
		},
	};
	vm.runInNewContext(source, { window, atob, TextEncoder, TextDecoder }, { filename: "browserAuthScript.js" });
	const auth = window[namespace];
	if (!auth || typeof auth.managementKey !== "function" || typeof auth.authHeaders !== "function") {
		throw new Error("script did not install window." + namespace);
	}
	const key = auth.managementKey();
	const headers = auth.authHeaders({ "X-Extra": "kept" });
	return {
		name: c.name,
		key: key,
		authorization: Object.prototype.hasOwnProperty.call(headers, "Authorization") ? headers.Authorization : null,
		extra: headers["X-Extra"],
		storage: Object.fromEntries(store),
	};
});
process.stdout.write(JSON.stringify(results));
`

type browserAuthCase struct {
	Name      string            `json:"name"`
	Host      string            `json:"host"`
	Pathname  string            `json:"pathname"`
	UserAgent string            `json:"userAgent"`
	Storage   map[string]string `json:"storage"`
	want      string
}

type browserAuthResult struct {
	Name          string            `json:"name"`
	Key           *string           `json:"key"`
	Authorization *string           `json:"authorization"`
	Extra         string            `json:"extra"`
	Storage       map[string]string `json:"storage"`
}

// renderedBrowserAuthPages returns the shipped pages that embed
// browserAuthScript: the unauthenticated resource page and the authenticated
// management HTML view.
func renderedBrowserAuthPages(t *testing.T) map[string]string {
	t.Helper()
	p := configurePlugin(t, &pluginFakeHost{}, "")
	pages := make(map[string]string)
	for _, path := range []string{
		"/v0/resource/plugins/account-health-pushover/status",
		"/v0/management/plugins/account-health-pushover/status/html",
	} {
		request, err := json.Marshal(protocol.ManagementRequest{Method: http.MethodGet, Path: path})
		if err != nil {
			t.Fatal(err)
		}
		value, err := p.Handle(protocol.MethodManagementHandle, request)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		response := value.(protocol.ManagementResponse)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d", path, response.StatusCode)
		}
		pages[path] = string(response.Body)
	}
	return pages
}

func TestBrowserAuthScriptRecoversConsoleSessions(t *testing.T) {
	// Execute exactly the script the pages ship, not a re-implementation.
	for path, page := range renderedBrowserAuthPages(t) {
		if !strings.Contains(page, browserAuthScript) {
			t.Fatalf("%s does not embed browserAuthScript verbatim", path)
		}
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; skipping execution of the embedded browser auth script (GitHub ubuntu runners ship node)")
	}

	const pathname = "/v0/resource/plugins/account-health-pushover/status"
	v2Body := strings.TrimPrefix(consoleSessionV2, "enc::v2::")
	cases := []browserAuthCase{
		{Name: "v2 remembered session", Host: consoleTestHost, UserAgent: consoleTestUserAgent,
			Storage: map[string]string{"cli-proxy-auth": consoleSessionV2, "isLoggedIn": "true"}, want: consoleTestKey},
		{Name: "v2 does not depend on the user agent", Host: consoleTestHost, UserAgent: consoleTestOtherUA,
			Storage: map[string]string{"cli-proxy-auth": consoleSessionV2, "isLoggedIn": "true"}, want: consoleTestKey},
		{Name: "v2 under a different host", Host: "127.0.0.1:8318", UserAgent: consoleTestUserAgent,
			Storage: map[string]string{"cli-proxy-auth": consoleSessionV2, "isLoggedIn": "true"}},
		{Name: "v2 signed in without Remember password", Host: consoleTestHost, UserAgent: consoleTestUserAgent,
			Storage: map[string]string{"cli-proxy-auth": consoleSessionV2NoRemember}},
		{Name: "v1 remembered session", Host: consoleTestHost, UserAgent: consoleTestUserAgent,
			Storage: map[string]string{"cli-proxy-auth": consoleSessionV1, "isLoggedIn": "true"}, want: consoleTestKey},
		{Name: "v1 under a different user agent", Host: consoleTestHost, UserAgent: consoleTestOtherUA,
			Storage: map[string]string{"cli-proxy-auth": consoleSessionV1, "isLoggedIn": "true"}},
		{Name: "v1 under a different host", Host: "127.0.0.1:8318", UserAgent: consoleTestUserAgent,
			Storage: map[string]string{"cli-proxy-auth": consoleSessionV1, "isLoggedIn": "true"}},
		{Name: "legacy v1 managementKey entry", Host: consoleTestHost, UserAgent: consoleTestUserAgent,
			Storage: map[string]string{"managementKey": consoleLegacyKeyV1}, want: consoleTestLegacyKey},
		{Name: "unknown prefix stays unreadable", Host: consoleTestHost, UserAgent: consoleTestUserAgent,
			Storage: map[string]string{"cli-proxy-auth": "enc::v3::" + v2Body}},
		{Name: "no console storage", Host: consoleTestHost, UserAgent: consoleTestUserAgent,
			Storage: map[string]string{}},
	}
	for i := range cases {
		cases[i].Pathname = pathname
	}

	results := runBrowserAuthScript(t, node, "accountHealthAuth", cases)
	if len(results) != len(cases) {
		t.Fatalf("got %d results for %d cases", len(results), len(cases))
	}
	for i, tc := range cases {
		got := results[i]
		if got.Name != tc.Name {
			t.Fatalf("result %d is %q, want %q", i, got.Name, tc.Name)
		}
		if derefJSString(got.Key) != tc.want || (tc.want == "" && got.Key != nil) {
			t.Errorf("%s: managementKey() = %v, want %q", tc.Name, describeJSString(got.Key), tc.want)
		}
		wantAuthorization := ""
		if tc.want != "" {
			wantAuthorization = "Bearer " + tc.want
		}
		if gotAuthorization := derefJSString(got.Authorization); gotAuthorization != wantAuthorization {
			t.Errorf("%s: authHeaders().Authorization = %v, want %q", tc.Name, describeJSString(got.Authorization), wantAuthorization)
		}
		if got.Extra != "kept" {
			t.Errorf("%s: authHeaders dropped the caller's header: %q", tc.Name, got.Extra)
		}
		if !maps.Equal(got.Storage, tc.Storage) {
			t.Errorf("%s: script changed localStorage: %v, want %v", tc.Name, got.Storage, tc.Storage)
		}
	}
}

func runBrowserAuthScript(t *testing.T, node, namespace string, cases []browserAuthCase) []browserAuthResult {
	t.Helper()
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "browser-auth.js")
	driverPath := filepath.Join(dir, "driver.cjs")
	casesPath := filepath.Join(dir, "cases.json")
	encodedCases, err := json.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string][]byte{
		scriptPath: []byte(browserAuthScript),
		driverPath: []byte(browserAuthScriptDriver),
		casesPath:  encodedCases,
	} {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	command := exec.CommandContext(ctx, node, driverPath, scriptPath, casesPath, namespace)
	command.Dir = dir
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("node driver failed: %v\nstderr:\n%s", err, stderr.String())
	}
	var results []browserAuthResult
	if err := json.Unmarshal(stdout.Bytes(), &results); err != nil {
		t.Fatalf("node driver output is not JSON: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	return results
}

func derefJSString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func describeJSString(value *string) string {
	if value == nil {
		return "null"
	}
	return `"` + *value + `"`
}
