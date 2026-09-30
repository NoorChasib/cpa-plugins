// Package protocol mirrors the subset of CPA ABI 1 / schema 6 used here.
// See REFERENCE.md for the exact upstream source pin.
package protocol

import (
	"encoding/json"
	"net/http"
	"net/url"
)

const (
	ABIVersion      uint32 = 1
	SchemaVersion   uint32 = 6
	MaxRequestBytes        = 1 << 20

	MethodPluginRegister     = "plugin.register"
	MethodPluginReconfigure  = "plugin.reconfigure"
	MethodPluginQuiesce      = "plugin.quiesce"
	MethodPluginShutdown     = "plugin.shutdown"
	MethodManagementRegister = "management.register"
	MethodManagementHandle   = "management.handle"
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

// The management API is the only capability: it carries the catalog URL, the
// settings page, and the page's private data route. No interceptor is
// declared, so CPA's own responses never reach here.
type RegistrationCapabilities struct {
	ManagementAPI bool `json:"management_api"`
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
