package plugin

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"net/http"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

// These embedded assets contain no runtime data, credentials, configuration or
// query values. The public resource must never become an operational snapshot.
//
//go:embed pageassets/index.html
var sidebarHTML string

//go:embed pageassets/style.css
var sidebarStyle string

//go:embed pageassets/app.js
var sidebarScript string

func sidebarHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// Build the immutable document and asset hashes once, not on each public GET.
var sidebarDocument = "<!doctype html>\n<html lang=\"en\"><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width, initial-scale=1\"><title>Quota Cache</title><style>" + sidebarStyle + "</style></head><body>" + sidebarHTML + "<script>" + browserAuthScript + sidebarScript + "</script></body></html>"
var sidebarCSP = "default-src 'none'; script-src " + sidebarHash(browserAuthScript+sidebarScript) + "; style-src " + sidebarHash(sidebarStyle) + "; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'; object-src 'none'"

// sidebarResponse is deliberately independent of Plugin and ManagementRequest.
// Same-origin plug-ins share the console's credential trust boundary; CSP does
// not isolate mutually untrusted plug-ins installed on that origin.
func sidebarResponse() protocol.ManagementResponse {
	return protocol.ManagementResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(sidebarDocument),
		Headers: http.Header{
			"Content-Type":            {"text/html; charset=utf-8"},
			"Cache-Control":           {"no-store"},
			"X-Content-Type-Options":  {"nosniff"},
			"Referrer-Policy":         {"no-referrer"},
			"Content-Security-Policy": {sidebarCSP},
		},
	}
}
