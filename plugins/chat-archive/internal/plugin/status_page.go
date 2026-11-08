package plugin

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"net/http"
	"runtime/debug"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
)

// The embedded assets hold no runtime data, credentials, configuration or query values. The
// public resource is a fixed shell; its script reads the authenticated status route.
//
//go:embed pageassets/index.html
var sidebarHTML string

//go:embed pageassets/style.css
var sidebarStyle string

//go:embed pageassets/app.js
var sidebarScript string

func sidebarHash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

var sidebarDocument = "<!doctype html>\n<html lang=\"en\"><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width, initial-scale=1\"><title>Chat Archive</title><style>" + sidebarStyle + "</style></head><body>" + sidebarHTML + "<script>" + sidebarScript + "</script></body></html>"

var sidebarCSP = "default-src 'none'; script-src " + sidebarHash(sidebarScript) + "; style-src " + sidebarHash(sidebarStyle) + "; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'; object-src 'none'"

// sidebarResponse is independent of the plugin state and the request.
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

func debugMemLimit() int64 { return debug.SetMemoryLimit(-1) }
