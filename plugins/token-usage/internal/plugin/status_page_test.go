package plugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestSidebarResponseSecurityHeaders(t *testing.T) {
	r := sidebarResponse()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", r.StatusCode)
	}
	for name, want := range map[string]string{
		"Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store",
		"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer",
	} {
		if got := r.Headers.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	csp := r.Headers.Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'none'", "connect-src 'self'", "frame-ancestors 'self'", "base-uri 'none'", "form-action 'none'", "object-src 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("missing CSP directive %s", directive)
		}
	}
	for _, tag := range []string{"script", "style"} {
		matches := regexp.MustCompile(`(?s)<`+tag+`>(.*?)</`+tag+`>`).FindAllSubmatch(r.Body, -1)
		if len(matches) != 1 {
			t.Fatalf("%s elements = %d, want one fixed asset", tag, len(matches))
		}
		sum := sha256.Sum256(matches[0][1])
		want := tag + "-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
		if !strings.Contains(csp, want) {
			t.Errorf("CSP does not hash exact served %s bytes", tag)
		}
	}
	for _, forbidden := range []string{"'unsafe-inline'", "'unsafe-eval'", "https:", "data:", "*"} {
		if strings.Contains(csp, forbidden) {
			t.Errorf("unsafe CSP token %q", forbidden)
		}
	}
}

func TestSidebarResponseIsFixedAndIndependent(t *testing.T) {
	first := sidebarResponse()
	want := append([]byte(nil), first.Body...)
	// Callers cannot mutate a shared response buffer or shared headers.
	first.Body[0] = '!'
	first.Headers.Set("Content-Type", "application/private-canary")
	first.Headers["Content-Security-Policy"][0] = "private-canary"
	for range 10 {
		r := sidebarResponse()
		if !bytes.Equal(r.Body, want) || r.Headers.Get("Content-Type") != "text/html; charset=utf-8" || r.Headers.Get("Content-Security-Policy") != sidebarCSP {
			t.Fatal("public shell changed between independent calls")
		}
	}
	if !bytes.Contains(want, []byte(`<div id="statistics" hidden>`)) || !bytes.Contains(want, []byte(`id="input-total">—`)) {
		t.Fatal("public shell must start without private statistics")
	}
	for _, forbidden := range []string{"document.write", "innerHTML", "outerHTML", "insertAdjacentHTML", "localStorage.setItem", "localStorage.removeItem", "postMessage", "window.parent", "window.opener", "eval(", "<iframe", "<img", "<link", "style=", "onclick=", "src="} {
		if bytes.Contains(want, []byte(forbidden)) {
			t.Errorf("unexpected public-shell sink or external resource %q", forbidden)
		}
	}
}

// Console storage vectors. Every stored value below was produced by an
// unmodified upstream Management Center src/utils/encryption.ts obfuscateData(),
// not by this repository, for the plaintext shown in its comment:
//   - enc::v2:: values: tag v1.27.2, commit f154fa9380825c4fe94bc577a9abaa7e505f8875
//     (frontendtests/upstream/encryption.ts), key "...|" + host only.
//   - enc::v1:: value: v1.22.15, commit ed5f1c48e11ba7335f1e8f676f228c280196af85
//     (frontendtests/upstream-v1.22.15/encryption.ts, byte-identical through
//     v1.26.7), key "...|" + host + "|" + sessionVectorUA.
//
// Unless noted, the plaintext is the v1.27.2 persisted shape for host
// 127.0.0.1:8317: {"state":{"apiBase":"http://127.0.0.1:8317",
// "managementKey":"test-management-key-0123456789","rememberPassword":true,
// "serverVersion":"8.0.53","serverBuildDate":null},"version":0}.
const (
	sessionVectorHost = "127.0.0.1:8317"
	sessionVectorKey  = "test-management-key-0123456789"
	sessionVectorUA   = "Mozilla/5.0 (X11; Linux x86_64) TestUA/1"
	// v1.27.2, host 127.0.0.1:8317.
	sessionVectorV2 = "enc::v2::GE4aWREGClpDVkMRGUQ1BBEQSwAYGxEXBUhKAkJGWFxRSVVSAAgPHQEZEgITV1ldUFAGAQxDBDkKAVsXQwQMXgNIDxQHW10WCAYbBkhGFg1CQlBVVkgEBAAWCQwcDENfVVZcVQYeOUwDARgXC0lDSh1fAgBOVxpfSAUAESMXF14aGwFQW0VdUgEcAh0SAhJdVEhOVkN1FgUFSTQTGx1bFw8FBUEKSUADDEhJGgoNV0hVUA=="
	// v1.27.2, same plaintext (apiBase still http://127.0.0.1:8317) written by
	// a console whose location.host was console.example.invalid:8443.
	sessionVectorV2ForeignHost = "enc::v2::GE4aWREGClpDVkMRGUQ1BBEQSwAYGxEXBUhKAkJGWFxRSVVSUlVWQF5bRwJHFQADEQsAQwwAAioJEEYAGkBRQBdBBEweEwgdFEgPBERGEhxPRVgICUdQVUJKXA9fVh0XDAIIHgYdPhIcHxJBFxxDVwQeEEtFTAUEHh8BSG5RRkAKAwcPSlBXVkkDVENLAVUWBwcfX0gxEAoZFiFMBxFNSA8SCRAeQ0wFCh4WRwoWQ1dAEQ=="
	// v1.27.2, host 127.0.0.1:8317, plaintext with "rememberPassword":false.
	// The key is deliberately still present so only the remember gate refuses it.
	sessionVectorV2RememberOff = "enc::v2::GE4aWREGClpDVkMRGUQ1BBEQSwAYGxEXBUhKAkJGWFxRSVVSAAgPHQEZEgITV1ldUFAGAQxDBDkKAVsXQwQMXgNIDxQHW10WCAYbBkhGFg1CQlBVVkgEBAAWCQwcDENfVVZcVQYeOUwDARgXC0lDSg9MGxYHWUtJXwETBgckAF8AHQAcQ11HRB8CGRsDDBwMQl9KRVRFIRkAQRQ2DgwcD1seHEEbGE5XH19IAAwMG1BfHQ4="
	// v1.22.15, host 127.0.0.1:8317, user agent sessionVectorUA.
	sessionVectorV1 = "enc::v1::GE4aWREGClpDVkMRGUQ1BBEQSwAYGxEXBUhKAkJGWFxRSVVSAAgPHQEZEgITV1ldUFAZIAoUHScJGA0PDERFWywcXFpOLQ4LGB1ODBVdOk8ZGRFmVkdBY3YXCEFAS18VHwoVG0gTIAheBBINBw0YAAcXFhBeR14WBhkXEzEADkJbWEASFBIWHwoWBgIVUG8cHxsaCRNtQEdcRGw5RVQZGiIcAhkFDFpOUy1FXUZOdl9DCQ=="
)

// sessionHarness evaluates the exact served page script in a Node vm with inert
// DOM stand-ins and a recording fetch stub. No request leaves the process; the
// only observable is whether the shipped reader authorizes a status request.
const sessionHarness = `'use strict';
const vm = require('node:vm');
const { script, cases } = JSON.parse(require('node:fs').readFileSync(0, 'utf8'));
const inert = () => new Proxy(function () {}, {
  get: (_, prop) => prop === 'then' ? undefined : prop === Symbol.toPrimitive ? () => '' : inert(),
  set: () => true,
  apply: () => inert()
});
const results = cases.map(({ host, userAgent, storage }) => {
  const requests = [];
  const context = {
    location: { protocol: 'http:', host, origin: 'http://' + host, pathname: '/v0/resource/plugins/token-usage/status' },
    navigator: { userAgent },
    localStorage: { getItem: (name) => Object.hasOwn(storage, name) ? storage[name] : null },
    document: { getElementById: inert, createElement: inert, addEventListener() {}, hidden: false },
    addEventListener() {},
    fetch: (url, options) => { requests.push({ url, authorization: options.headers.Authorization }); return new Promise(() => {}); },
    setTimeout: () => 0, clearTimeout() {},
    AbortController, Headers, URL, URLSearchParams, TextEncoder, TextDecoder, atob
  };
  context.window = context;
  vm.runInNewContext(script, context);
  return requests;
});
process.stdout.write(JSON.stringify(results));
`

type sessionCase struct {
	Host      string            `json:"host"`
	UserAgent string            `json:"userAgent"`
	Storage   map[string]string `json:"storage"`
}

type sessionRequest struct {
	URL           string `json:"url"`
	Authorization string `json:"authorization"`
}

func runServedSessionReader(t *testing.T, cases []sessionCase) [][]sessionRequest {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is required in CI to execute the served session reader")
		}
		t.Skip("node not found; the served session reader needs Node to execute")
	}
	scripts := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllSubmatch(sidebarResponse().Body, -1)
	if len(scripts) != 1 {
		t.Fatalf("script elements = %d, want one", len(scripts))
	}
	input, err := json.Marshal(map[string]any{"script": string(scripts[0][1]), "cases": cases})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "-e", sessionHarness)
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node harness: %v\n%s", err, stderr.String())
	}
	var results [][]sessionRequest
	if err := json.Unmarshal(out, &results); err != nil || len(results) != len(cases) {
		t.Fatalf("harness output %q: %v", out, err)
	}
	return results
}

// The served controller must read both console codecs: v2 (Management Center
// v1.27.0+, host-only key) and legacy v1 (host plus user-agent key).
func TestSidebarReadsConsoleStorageCodecs(t *testing.T) {
	const otherUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 15_0) OtherBrowser/2"
	auth := func(stored string) map[string]string { return map[string]string{"cli-proxy-auth": stored} }
	cases := []struct {
		name      string
		in        sessionCase
		recovered bool
	}{
		{"v1.27.2 enc::v2:: session recovered regardless of user agent", sessionCase{sessionVectorHost, otherUA, auth(sessionVectorV2)}, true},
		{"enc::v2:: session written under another host not recovered", sessionCase{sessionVectorHost, sessionVectorUA, auth(sessionVectorV2ForeignHost)}, false},
		{"enc::v2:: session with rememberPassword false not recovered", sessionCase{sessionVectorHost, sessionVectorUA, auth(sessionVectorV2RememberOff)}, false},
		{"unknown enc::v3:: prefix stays unreadable", sessionCase{sessionVectorHost, sessionVectorUA, auth("enc::v3::" + strings.TrimPrefix(sessionVectorV2, "enc::v2::"))}, false},
		{"legacy v1.22.15 enc::v1:: session still recovered", sessionCase{sessionVectorHost, sessionVectorUA, auth(sessionVectorV1)}, true},
		{"legacy enc::v1:: session stays bound to its user agent", sessionCase{sessionVectorHost, otherUA, auth(sessionVectorV1)}, false},
	}
	input := make([]sessionCase, len(cases))
	for i, c := range cases {
		input[i] = c.in
	}
	results := runServedSessionReader(t, input)
	for i, c := range cases {
		requests := results[i]
		if !c.recovered {
			if len(requests) != 0 {
				t.Errorf("%s: made %d authorized requests, want none", c.name, len(requests))
			}
			continue
		}
		if len(requests) != 1 {
			t.Errorf("%s: requests = %d, want exactly the initial status request", c.name, len(requests))
			continue
		}
		if want := "http://" + sessionVectorHost + "/v0/management/plugins/token-usage/status"; requests[0].URL != want {
			t.Errorf("%s: request URL = %q, want %q", c.name, requests[0].URL, want)
		}
		if requests[0].Authorization != "Bearer "+sessionVectorKey {
			t.Errorf("%s: request did not carry the recovered management key", c.name)
		}
	}
}
