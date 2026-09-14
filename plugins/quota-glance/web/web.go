// Package web carries the embedded application shell.
//
// The shell is a fixed document that contains no runtime data, no credentials,
// no configuration, and no query values. It is served unauthenticated, so it
// must stay that way: the data lives behind the token on .../summary.
//
// handoff 2 replaces dist/index.html with the built application. Nothing else
// in this package needs to change.
package web

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
)

//go:embed dist/index.html
var shell string

// Shell returns the document served by GET .../app.
func Shell() string { return shell }

// ContentSecurityPolicy is applied to the shell.
//
// Inline script and style are permitted because the shell is a single
// self-contained document: only /app is a registered resource path, so there is
// no second path from which a separate bundle could be fetched. A future build
// that splits its assets out should tighten this to hashes.
//
// CSP here is defence in depth, not isolation: same-origin plugins share the
// console's credential trust boundary regardless of policy.
const ContentSecurityPolicy = "default-src 'none'; " +
	"script-src 'self' 'unsafe-inline'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"font-src 'self' data:; " +
	"connect-src 'self'; " +
	"frame-ancestors 'self'; " +
	"base-uri 'none'; " +
	"form-action 'none'; " +
	"object-src 'none'"

// ETag identifies the shell so a browser can skip re-downloading it.
var ETag = func() string {
	sum := sha256.Sum256([]byte(shell))
	return `"` + base64.RawURLEncoding.EncodeToString(sum[:]) + `"`
}()
