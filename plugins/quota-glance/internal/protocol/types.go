// Package protocol mirrors the subset of CPA ABI 1 / schema 4 this plugin uses.
// It matches the declaration in plugins/quota-cache, the other consumer of the
// same snapshot, so both agree on the host contract.
//
// Only the callbacks quota-glance actually needs are declared. Everything the
// dashboard renders is read from a file and served from memory: no scheduled
// work in this plugin contacts a provider, and quota-cache remains the only
// component that polls one.
//
// host.auth.get and host.http.do are the two exceptions, and they exist for
// exactly one action: spending a banked Codex rate-limit reset when the
// operator presses the button and confirms. That is a write the snapshot
// cannot carry and quota-cache's poller has no business performing, and it
// happens only on an explicit request — never on a timer, never on a rebuild,
// and never on any route that merely reads. See internal/redeem.
package protocol

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

const (
	ABIVersion    uint32 = 1
	SchemaVersion uint32 = 4

	MethodPluginRegister     = "plugin.register"
	MethodPluginReconfigure  = "plugin.reconfigure"
	MethodPluginQuiesce      = "plugin.quiesce"
	MethodManagementRegister = "management.register"
	MethodManagementHandle   = "management.handle"

	MethodHostLog      = "host.log"
	MethodHostAuthList = "host.auth.list"
	// MethodHostAuthGet and MethodHostHTTPDo are reachable only from the
	// redeem path. host.auth.get returns the physical credential document,
	// OAuth tokens included, so its result is decoded for the two fields one
	// request needs and never logged, persisted, or rendered.
	MethodHostAuthGet = "host.auth.get"
	MethodHostHTTPDo  = "host.http.do"
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

type RegistrationCapabilities struct {
	UsagePlugin   bool `json:"usage_plugin"`
	ManagementAPI bool `json:"management_api"`
}

type ManagementRegistration struct {
	Routes    []ManagementRoute `json:"routes,omitempty"`
	Resources []ResourceRoute   `json:"resources,omitempty"`
}

// Menu must stay empty on a management route: CPA converts a GET carrying Menu
// into a public resource, which would publish the management surface.
type ManagementRoute struct {
	Method      string
	Path        string
	Menu        string
	Description string
}

// Resources are public GET routes under /v0/resource/plugins/<pluginID>/.
// CPA runs no authentication on them; this plugin owns its own.
type ResourceRoute struct {
	Path        string
	Menu        string
	Description string
}

type ManagementRequest struct {
	Method         string
	Path           string
	Headers        http.Header
	Query          url.Values
	Body           []byte
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type ManagementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

type HostRecentRequestEntry struct {
	Time    string `json:"time"`
	Success int64  `json:"success"`
	Failed  int64  `json:"failed"`
}

type HostAuthFileEntry struct {
	ID             string                   `json:"id,omitempty"`
	AuthIndex      string                   `json:"auth_index,omitempty"`
	Name           string                   `json:"name"`
	Type           string                   `json:"type,omitempty"`
	Provider       string                   `json:"provider,omitempty"`
	Label          string                   `json:"label,omitempty"`
	Status         string                   `json:"status,omitempty"`
	StatusMessage  string                   `json:"status_message,omitempty"`
	Disabled       bool                     `json:"disabled,omitempty"`
	Unavailable    bool                     `json:"unavailable,omitempty"`
	RuntimeOnly    bool                     `json:"runtime_only,omitempty"`
	Source         string                   `json:"source,omitempty"`
	Path           string                   `json:"path,omitempty"`
	Size           int64                    `json:"size,omitempty"`
	ModTime        time.Time                `json:"modtime,omitempty"`
	UpdatedAt      time.Time                `json:"updated_at,omitempty"`
	CreatedAt      time.Time                `json:"created_at,omitempty"`
	LastRefresh    time.Time                `json:"last_refresh,omitempty"`
	NextRetryAfter time.Time                `json:"next_retry_after,omitempty"`
	Email          string                   `json:"email,omitempty"`
	ProjectID      string                   `json:"project_id,omitempty"`
	AccountType    string                   `json:"account_type,omitempty"`
	Account        string                   `json:"account,omitempty"`
	Priority       int                      `json:"priority,omitempty"`
	Note           string                   `json:"note,omitempty"`
	Websockets     bool                     `json:"websockets,omitempty"`
	Success        int64                    `json:"success,omitempty"`
	Failed         int64                    `json:"failed,omitempty"`
	RecentRequests []HostRecentRequestEntry `json:"recent_requests,omitempty"`
}

type HostAuthListResponse struct {
	Files []HostAuthFileEntry `json:"files"`
}

type HostAuthGetRequest struct {
	AuthIndex string `json:"auth_index"`
}

// HostAuthGetResponse is the host.auth.get result. JSON is the complete
// physical credential document and therefore contains OAuth tokens; callers
// decode only the fields they need and never log, persist, or render it.
type HostAuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name,omitempty"`
	Path      string          `json:"path,omitempty"`
	JSON      json.RawMessage `json:"json"`
}

// HostHTTPRequest is the host.http.do request (snake_case keys; Body is
// base64-encoded by encoding/json).
type HostHTTPRequest struct {
	Method  string              `json:"method,omitempty"`
	URL     string              `json:"url,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`
}

// HostHTTPResponse is the host.http.do result. Upstream returns the untagged
// pluginapi.HTTPResponse, so the wire keys are capitalized.
type HostHTTPResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers"`
	Body       []byte              `json:"Body"`
}

type HostLogRequest struct {
	HostCallbackID string         `json:"host_callback_id,omitempty"`
	Level          string         `json:"level"`
	Message        string         `json:"message"`
	Fields         map[string]any `json:"fields,omitempty"`
}
