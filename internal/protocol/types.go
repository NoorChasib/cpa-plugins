// Package protocol mirrors the subset of CPA ABI 1 / schema 6 used here.
// See docs/upstream-compatibility.md for the exact upstream source pin.
package protocol

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"
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
	MethodUsageHandle        = "usage.handle"
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
	Routes []ManagementRoute `json:"routes,omitempty"`
}

// Menu must stay empty: CPA converts a GET with Menu into a public resource.
type ManagementRoute struct {
	Method      string
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

// Body remains base64 on the RPC wire at schema 6. CPA returns the decoded
// bytes without legacy HTML-entity rewriting; JSON is not safe HTML.
type ManagementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// UsageRecord is intentionally narrow. Do not add keys, auth identifiers,
// session metadata, headers, or failure bodies to this type.
// No original provider field-presence or canonical quality reaches this wire.
type UsageRecord struct {
	Provider     string
	ExecutorType string
	Model        string
	Alias        string
	RequestedAt  time.Time
	Generate     bool
	Failed       bool
	Failure      struct{ StatusCode int }
	Detail       UsageDetail
}

type UsageDetail struct {
	InputTokens         int64
	OutputTokens        int64
	TotalTokens         int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
}
