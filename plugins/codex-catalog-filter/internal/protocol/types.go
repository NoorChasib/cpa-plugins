// Package protocol mirrors the subset of CPA ABI 1 / schema 6 used here.
// See REFERENCE.md for the exact upstream source pin.
//
// Wire rules: requests and responses are JSON with Go field names (Body, never
// body) except where a json tag says otherwise; []byte travels as base64.
package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

const (
	ABIVersion    uint32 = 1
	SchemaVersion uint32 = 6
	// MaxRequestBytes bounds every call except response.intercept_after:
	// lifecycle documents are at most 256 KiB of YAML (about 342 KiB as
	// base64) and management requests carry only headers and a query.
	MaxRequestBytes = 1 << 20
	// MaxInterceptRequestBytes bounds response.intercept_after. Once the
	// response interceptor is declared, CPA sends every successful
	// non-streaming response through it, including its 1.4 MB Codex catalog
	// (1.9 MB as base64) and large inference bodies. The request is read in
	// place from CPA's buffer, never copied, and anything that is not a model
	// list is rejected after its first few fields, so the cap only bounds a
	// pathological call. 160 MiB matches chat-archive's default max-rpc-mib.
	// A larger call is passed through unchanged, never answered with an error.
	MaxInterceptRequestBytes = 160 << 20
	// MaxListBodyBytes bounds a model list the interceptor will decode and
	// rewrite. CPA's Claude-format list is about 18 KB for 89 models.
	MaxListBodyBytes = 4 << 20

	MethodPluginRegister         = "plugin.register"
	MethodPluginReconfigure      = "plugin.reconfigure"
	MethodPluginQuiesce          = "plugin.quiesce"
	MethodPluginShutdown         = "plugin.shutdown"
	MethodManagementRegister     = "management.register"
	MethodManagementHandle       = "management.handle"
	MethodResponseInterceptAfter = "response.intercept_after"
)

type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

type EnvelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type LifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type Registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      Metadata                 `json:"metadata"`
	Capabilities  RegistrationCapabilities `json:"capabilities"`
}

type Metadata struct {
	Name             string
	Version          string
	Author           string
	GitHubRepository string
	Logo             string
	ConfigFields     []ConfigField
}

type ConfigField struct {
	Name        string
	Type        string
	EnumValues  []string
	Description string
}

// The management API carries the Codex and Antigravity catalog URLs, the
// settings page, and the page's private data route. The response interceptor
// is declared only while claude-code.filter is on: CPA rebuilds capabilities
// from every reconfigure response, so switching it off removes the RPC cost.
// CPA v8.0.4 to v8.0.77 map response_interceptor without a schema gate.
type RegistrationCapabilities struct {
	ResponseInterceptor bool `json:"response_interceptor,omitempty"`
	ManagementAPI       bool `json:"management_api"`
}

type ManagementRegistration struct {
	Routes    []ManagementRoute `json:"routes,omitempty"`
	Resources []ResourceRoute   `json:"resources,omitempty"`
}

// Management routes live under /v0/management and CPA authenticates them with
// the management key before dispatch. Menu must stay empty: CPA converts a GET
// with a Menu into a public resource.
type ManagementRoute struct {
	Method      string
	Path        string
	Menu        string
	Description string
}

// Resources are public GET routes under /v0/resource/plugins/<pluginID>/.
// CPA does not authenticate them. A Menu adds a sidebar entry; an empty one
// keeps the route out of the sidebar.
type ResourceRoute struct {
	Path        string
	Menu        string
	Description string
}

// Resource requests carry every client header, including credentials.
type ManagementRequest struct {
	Method  string
	Path    string
	Headers http.Header
	Query   url.Values
}

// Body remains base64 on the RPC wire at schema 6. CPA returns the decoded
// bytes without legacy HTML-entity rewriting.
type ManagementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// ResponseInterceptResponse is the result of response.intercept_after. An
// empty Body keeps CPA's response unchanged.
type ResponseInterceptResponse struct {
	Body []byte `json:",omitempty"`
}

// ListIntercept is what the interceptor keeps from a response.intercept_after
// request that may be a Claude-format model list.
type ListIntercept struct {
	UserAgent string
	Body      []byte
}

// ScanClaudeListIntercept returns the decoded body of a response.intercept_after
// request only when it can be CPA's Claude-format model list for a Claude Code
// client: SourceFormat "claude", no model, no request body, not streamed,
// status 200, and a User-Agent starting with claude-code/ or claude-cli/
// (WriteModelListResponse in CPA's sdk/api/handlers/handlers_interceptors.go).
//
// It decides from the small leading fields, which CPA marshals before the
// bodies, so an inference response is rejected without reading or decoding
// its body. Request headers are inspected for the User-Agent only and never
// kept: they carry the caller's raw key.
func ScanClaudeListIntercept(raw []byte, ownUserAgentPrefix string) (ListIntercept, bool) {
	var out ListIntercept
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return out, false
	}
	var (
		body                           json.RawMessage
		sawFormat, sawAgent, sawStatus bool
	)
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return out, false
		}
		key, _ := tok.(string)
		switch key {
		case "SourceFormat":
			var value string
			if dec.Decode(&value) != nil || value != "claude" {
				return out, false
			}
			sawFormat = true
		case "Model", "RequestedModel":
			var value string
			if dec.Decode(&value) != nil || value != "" {
				return out, false
			}
		case "Stream":
			var value bool
			if dec.Decode(&value) != nil || value {
				return out, false
			}
		case "OriginalRequest", "RequestBody":
			var value json.RawMessage
			if dec.Decode(&value) != nil || !emptyBytes(value) {
				return out, false
			}
		case "RequestHeaders":
			var headers http.Header
			if dec.Decode(&headers) != nil {
				return out, false
			}
			agent := headers.Get("User-Agent")
			if ownUserAgentPrefix != "" && strings.HasPrefix(agent, ownUserAgentPrefix) {
				return out, false
			}
			if !strings.HasPrefix(agent, "claude-code/") && !strings.HasPrefix(agent, "claude-cli/") {
				return out, false
			}
			out.UserAgent, sawAgent = agent, true
		case "Body":
			if dec.Decode(&body) != nil || len(body) < 2 || len(body) > base64.StdEncoding.EncodedLen(MaxListBodyBytes)+2 || body[0] != '"' {
				return out, false
			}
		case "StatusCode":
			var value int
			if dec.Decode(&value) != nil || value != http.StatusOK {
				return out, false
			}
			sawStatus = true
		default:
			var skip json.RawMessage
			if dec.Decode(&skip) != nil {
				return out, false
			}
		}
	}
	if !sawFormat || !sawAgent || !sawStatus || body == nil {
		return out, false
	}
	var decoded []byte
	if json.Unmarshal(body, &decoded) != nil || len(decoded) == 0 || len(decoded) > MaxListBodyBytes {
		return out, false
	}
	out.Body = decoded
	return out, true
}

// emptyBytes reports whether a []byte field is absent on the wire: null or "".
func emptyBytes(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || string(trimmed) == "null" || string(trimmed) == `""`
}
