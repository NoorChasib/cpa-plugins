package plugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
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
