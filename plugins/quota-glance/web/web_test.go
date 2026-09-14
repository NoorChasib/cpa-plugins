package web

import (
	"regexp"
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
//
// These patterns look for values rather than vocabulary. The app necessarily
// contains the words the summary document is made of — it sends an
// Authorization header and reads credentialId off the response — and banning
// those would ban any client that works. What must never appear is a token, an
// address, or a serialized document.
func TestShellCarriesNoData(t *testing.T) {
	shell := Shell()
	for _, probe := range []struct {
		what    string
		pattern *regexp.Regexp
	}{
		// A credential presented rather than the prefix used to present one:
		// `Bearer ` followed by something long enough to be a secret. The app's
		// own `"Bearer " + token` concatenation has nothing after the space.
		{"a bearer token", regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{16,}`)},
		// Any address at all. Every credential in this document is identified
		// by one, including in the fixtures the app develops against.
		{"an email address", regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)},
		// A serialized summary body. Quoted key plus colon is JSON; the
		// minified property access the app compiles to is not.
		{"a serialized summary document", regexp.MustCompile(`"(schemaVersion|credentialId|remainingFraction|generatedAtEpoch)"\s*:`)},
	} {
		if found := probe.pattern.FindString(shell); found != "" {
			t.Fatalf("the public shell contains %s (%q); it must be a data-free document", probe.what, found)
		}
	}
}

// One inline script or style element, captured so the opening tag can be kept
// and the body dropped. A bundler escapes any literal "</script>" inside the
// code it emits — otherwise the browser would end the script there — so the
// first unescaped one is always the real closing tag.
var (
	scriptPattern = regexp.MustCompile(`(?is)(<script\b[^>]*>).*?</script\s*>`)
	stylePattern  = regexp.MustCompile(`(?is)(<style\b[^>]*>).*?</style\s*>`)
)

// markupOf returns the document with script and style *bodies* removed;
// styleOf returns the style blocks intact.
//
// The split matters because a minified React bundle contains markup-shaped
// string literals — it looks for `link[rel="modulepreload"]` in its own
// resource code — and a scan that cannot tell an element from a string inside a
// script would report the bundle reaching out when it is doing nothing of the
// kind. The opening tags are deliberately kept: `<script src="...">` is exactly
// the thing being looked for, and dropping whole elements would hide it.
func markupOf(shell string) string {
	return stylePattern.ReplaceAllString(scriptPattern.ReplaceAllString(shell, "$1"), "$1")
}

func styleOf(shell string) string {
	return strings.Join(stylePattern.FindAllString(shell, -1), "\n")
}

// subresource matches an element that can pull in a second file, capturing the
// URL it points at.
var subresource = regexp.MustCompile(`(?i)<(?:script|link|img|iframe|source|video|audio|embed|object|track)\b[^>]*\b(?:src|href|data)\s*=\s*"([^"]*)"`)

// external reports whether a reference would make the browser fetch something.
//
// A `data:` URI carries the payload itself rather than pointing at one, and is
// how the favicon is inlined — which is what stops the browser asking for
// /favicon.ico on every visit. A bare fragment goes nowhere either. Everything
// else, absolute or relative, is a request.
func external(reference string) bool {
	reference = strings.TrimSpace(reference)
	switch {
	case reference == "", strings.HasPrefix(reference, "#"):
		return false
	case strings.HasPrefix(strings.ToLower(reference), "data:"):
		return false
	}
	return true
}

// The bundle must issue no request but the one that fetched it.
//
// This is not only about weight. The access token arrives in this page's URL on
// first visit, so a single third-party subresource — a CDN font, an icon
// service, an analytics beacon — would carry that URL out in a Referer header.
// The build inlines everything for this reason, and a future change that
// reintroduces an external asset should fail here rather than in a phone's
// network tab.
func TestShellFetchesNothingExternal(t *testing.T) {
	shell := Shell()
	markup := markupOf(shell)

	for _, match := range subresource.FindAllStringSubmatch(markup, -1) {
		if external(match[1]) {
			t.Fatalf("the shell fetches %q; the bundle must be self-contained", match[1])
		}
	}

	for _, probe := range []struct {
		what    string
		where   string
		pattern *regexp.Regexp
	}{
		{"a preload or prefetch hint", markup, regexp.MustCompile(`(?i)<link\b[^>]*\brel\s*=\s*['"]?(preload|prefetch|preconnect|dns-prefetch|modulepreload)`)},
		{"a CSS url() reference", styleOf(shell), regexp.MustCompile(`(?i)url\(\s*['"]?(https?:)?//`)},
		{"a CSS @import", styleOf(shell), regexp.MustCompile(`(?i)@import\s+(url\(|['"])`)},
	} {
		if found := probe.pattern.FindString(probe.where); found != "" {
			t.Fatalf("the shell references %s (%q); the bundle must be self-contained", probe.what, found)
		}
	}
}

// The shell is the built application, not the placeholder it replaced.
func TestShellIsTheBuiltApplication(t *testing.T) {
	shell := Shell()
	if !strings.Contains(shell, `id="root"`) {
		t.Fatal("shell has no mount point; web/dist/index.html looks like the placeholder, not a build")
	}
	if !strings.Contains(strings.ToLower(shell), "<script") {
		t.Fatal("shell carries no script; the bundle did not inline")
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
