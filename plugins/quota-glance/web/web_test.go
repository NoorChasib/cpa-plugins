package web

import (
	"strings"
	"testing"
)

// The shell is embedded at compile time, so a missing file is a build failure
// rather than a runtime one. What this guards is the subtler case: the file
// present but empty, or replaced by a build that forgot to write anything.
func TestShellIsEmbeddedAndWellFormed(t *testing.T) {
	shell := Shell()
	if len(shell) < 200 {
		t.Fatalf("shell is %d bytes; it looks empty or truncated", len(shell))
	}
	for _, fragment := range []string{"<!doctype html>", "<html", "</html>"} {
		if !strings.Contains(strings.ToLower(shell), fragment) {
			t.Fatalf("shell is missing %q", fragment)
		}
	}
	if ETag == "" || !strings.HasPrefix(ETag, `"`) {
		t.Fatalf("ETag = %q", ETag)
	}
}

// The shell is served unauthenticated, because CPA runs no authentication on a
// resource route. Whatever the web app builds into this file, it must never
// carry data: anything here is readable by anyone who can reach the origin.
func TestShellCarriesNoData(t *testing.T) {
	shell := strings.ToLower(Shell())
	for _, forbidden := range []string{
		"bearer ", "web-token", "authorization:",
		"@example.com",
		"remainingfraction", "credentialid", "generatedatepoch",
	} {
		if strings.Contains(shell, forbidden) {
			t.Fatalf("the public shell contains %q; it must be a data-free document", forbidden)
		}
	}
}

func TestContentSecurityPolicyLocksDownTheDefault(t *testing.T) {
	for _, directive := range []string{
		"default-src 'none'", "frame-ancestors 'self'",
		"base-uri 'none'", "form-action 'none'", "object-src 'none'",
	} {
		if !strings.Contains(ContentSecurityPolicy, directive) {
			t.Fatalf("policy is missing %q", directive)
		}
	}
	// The shell talks only to its own origin; a build that reaches elsewhere
	// should have to change this deliberately.
	if strings.Contains(ContentSecurityPolicy, "connect-src *") {
		t.Fatal("connect-src must stay same-origin")
	}
}
