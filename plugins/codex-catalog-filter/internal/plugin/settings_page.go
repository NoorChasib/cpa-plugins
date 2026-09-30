package plugin

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"net/http"

	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/protocol"
)

// These embedded assets contain no runtime data, credentials, configuration or
// query values. The public page must never become an operational snapshot:
// the model list is read from the private state route with the console's
// management key.
//
//go:embed pageassets/index.html
var settingsHTML string

//go:embed pageassets/style.css
var settingsStyle string

//go:embed pageassets/app.js
var settingsScript string

func assetHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// Build the immutable document and asset hashes once, not on each public GET.
var settingsDocument = "<!doctype html>\n<html lang=\"en\"><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width, initial-scale=1\"><title>Codex Models</title><style>" + settingsStyle + "</style></head><body>" + settingsHTML + "<script>" + settingsScript + "</script></body></html>"
var settingsCSP = "default-src 'none'; script-src " + assetHash(settingsScript) + "; style-src " + assetHash(settingsStyle) + "; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'; object-src 'none'"

// settingsPageResponse is deliberately independent of Plugin and the request.
// Same-origin plug-ins share the console's credential trust boundary; CSP does
// not isolate mutually untrusted plug-ins installed on that origin.
func settingsPageResponse() protocol.ManagementResponse {
	return protocol.ManagementResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(settingsDocument),
		Headers: http.Header{
			"Content-Type":            {"text/html; charset=utf-8"},
			"Cache-Control":           {"no-store"},
			"X-Content-Type-Options":  {"nosniff"},
			"Referrer-Policy":         {"no-referrer"},
			"Content-Security-Policy": {settingsCSP},
		},
	}
}
