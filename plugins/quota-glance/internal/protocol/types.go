// Package protocol mirrors the subset of CPA ABI 1 / schema 4 this plugin uses.
// It matches the declaration in plugins/quota-cache, the other consumer of the
// same snapshot, so both agree on the host contract.
//
// Only the callbacks quota-glance actually needs are declared: it reads a file
// and serves memory, and makes no provider or management-API request of any
// kind. There is deliberately no HTTP host method here.
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

type HostLogRequest struct {
	HostCallbackID string         `json:"host_callback_id,omitempty"`
	Level          string         `json:"level"`
	Message        string         `json:"message"`
	Fields         map[string]any `json:"fields,omitempty"`
}
