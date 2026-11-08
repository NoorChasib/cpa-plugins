// Package protocol mirrors the subset of CPA ABI 1 / RPC schema 7 that chat-archive uses
// (CPA v8.0.60 sdk/pluginabi/types.go and sdk/pluginapi/types.go).
//
// Wire rules: requests and responses are JSON with Go field names
// (ClearHeaders, never clear_headers); []byte is base64; time.Duration is nanoseconds.
//
// Hook requests carry raw secrets: the client's Authorization headers, UsageRecord.APIKey,
// UsageRecord.Source and AuthID, and the executed upstream RequestHeaders of response and stream
// hooks. The decoders in decode.go never materialize RequestHeaders, and read APIKey, Source and
// AuthID only into a scratch buffer that is zeroed before the call returns.
package protocol

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

// ABI and schema versions: the plugin declares schema 7 and rejects an older host.
const (
	ABIVersion    uint32 = 1
	SchemaVersion uint32 = 7
)

// RPC methods (CPA sdk/pluginabi/types.go).
const (
	MethodPluginRegister     = "plugin.register"
	MethodPluginReconfigure  = "plugin.reconfigure"
	MethodPluginQuiesce      = "plugin.quiesce"
	MethodPluginShutdown     = "plugin.shutdown"
	MethodManagementRegister = "management.register"
	MethodManagementHandle   = "management.handle"
	MethodUsageHandle        = "usage.handle"

	MethodRequestInterceptBefore       = "request.intercept_before"
	MethodRequestInterceptAfter        = "request.intercept_after"
	MethodRequestComplete              = "request.complete"
	MethodResponseInterceptAfter       = "response.intercept_after"
	MethodResponseInterceptStreamChunk = "response.intercept_stream_chunk"

	MethodHostAuthList       = "host.auth.list"
	MethodHostAuthGetRuntime = "host.auth.get_runtime"
)

// Per-method raw RPC caps; a larger call passes through uncaptured. MaxRPCDefault is the configurable body-hook cap
// (max-rpc-mib, 160 MiB by default).
const (
	MaxRPCDefault    = 160 << 20
	MaxStreamChunk   = 16 << 20
	MaxSmallHook     = 1 << 20 // request.intercept_after, request.complete, management.handle
	MaxUsage         = 4 << 20
	MaxLifecycle     = 128 << 10 // plugin.register, plugin.reconfigure
	MaxOtherMethod   = 64 << 10  // quiesce, shutdown, management.register, unknown methods
	MaxHostResponse  = 4 << 20
	StreamHeaderInit = -1
)

// Envelope is the RPC result envelope.
type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

// EnvelopeError is an RPC error.
type EnvelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// LifecycleRequest is the body of plugin.register and plugin.reconfigure.
type LifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// Registration is the result of plugin.register and plugin.reconfigure.
type Registration struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      Metadata     `json:"metadata"`
	Capabilities  Capabilities `json:"capabilities"`
}

// Metadata keys are PascalCase on the wire (no JSON tags in CPA).
type Metadata struct {
	Name             string
	Version          string
	Author           string
	GitHubRepository string
	Logo             string
	ConfigFields     []ConfigField
}

// ConfigField describes one plugin config key. No field carries a secret default.
type ConfigField struct {
	Name        string
	Type        string
	EnumValues  []string `json:",omitempty"`
	Description string
}

// Capabilities are the declared capability flags.
type Capabilities struct {
	RequestInterceptor              bool `json:"request_interceptor"`
	RequestInterceptorOmitBodyAfter bool `json:"request_interceptor_omit_body_after"`
	ResponseInterceptor             bool `json:"response_interceptor"`
	ResponseStreamInterceptor       bool `json:"response_stream_interceptor"`
	RequestLifecyclePlugin          bool `json:"request_lifecycle_plugin"`
	UsagePlugin                     bool `json:"usage_plugin"`
	ManagementAPI                   bool `json:"management_api"`
}

// ManagementRegistration is the result of management.register.
type ManagementRegistration struct {
	Routes    []ManagementRoute `json:"routes,omitempty"`
	Resources []ResourceRoute   `json:"resources,omitempty"`
}

// ManagementRoute is one private route. Menu must stay empty: a GET with a Menu becomes public.
type ManagementRoute struct {
	Method      string
	Path        string
	Menu        string
	Description string
}

// ResourceRoute is one public GET route under /v0/resource/plugins/<id>/.
type ResourceRoute struct {
	Path        string
	Menu        string
	Description string
}

// ManagementRequest is the decoded part of management.handle. Headers (which carry the
// management key) and Body are never decoded.
type ManagementRequest struct {
	Method string
	Path   string
	Query  url.Values
}

// ManagementResponse is the management.handle result; Body is base64 on the wire.
type ManagementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// RequestInterceptResponse is the before-auth result; only ClearHeaders is ever set.
type RequestInterceptResponse struct {
	ClearHeaders []string `json:",omitempty"`
}

// HostAuthFileEntry holds the fields of CPA's HostAuthFileEntry the label loop reads. Other
// fields (path, note, account, name) are personal data and are never decoded.
type HostAuthFileEntry struct {
	AuthIndex   string `json:"auth_index,omitempty"`
	Type        string `json:"type,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Label       string `json:"label,omitempty"`
	RuntimeOnly bool   `json:"runtime_only,omitempty"`
	Source      string `json:"source,omitempty"`
	Email       string `json:"email,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
	BaseURL     string `json:"base_url,omitempty"`
}

// HostAuthListResponse is the result of host.auth.list.
type HostAuthListResponse struct {
	Files []HostAuthFileEntry `json:"files"`
}

// HostAuthGetRuntimeRequest is the body of host.auth.get_runtime.
type HostAuthGetRuntimeRequest struct {
	AuthIndex string `json:"auth_index"`
}

// HostAuthGetRuntimeResponse is the result of host.auth.get_runtime.
type HostAuthGetRuntimeResponse struct {
	Auth HostAuthFileEntry `json:"auth"`
}

// Hook-side decoded records. Each holds only what the plugin keeps; see decode.go.

// Meta holds the Metadata keys the plugin reads from CPA's per-request metadata.
type Meta struct {
	Present            bool // Metadata was a JSON object (false for null or absent)
	RequestPath        string
	ExecutionSessionID string
	IdempotencyKey     string
	Source             string
	ReasoningEffort    string
	ServiceTier        string
	Generate           *bool
	CallerScope        string
	SelectedAuthID     string
	SelectedAuthIndex  string
	CanonicalSessionID string
	ParentSessionID    string
	SelectionID        string
	NodeKind           string
	IsFork             bool
	IsCompaction       bool
}

// BeforeAuth is the decoded request.intercept_before call.
type BeforeAuth struct {
	RequestID      string
	TraceID        string
	SourceFormat   string
	Model          string
	RequestedModel string
	Stream         bool
	Headers        http.Header
	Body           []byte // nil when skipped (budget or metadata-only)
	BodyLen        int    // decoded length when known, else base64 length * 3/4
	CallerScope    string
	Meta           Meta
}

// AfterAuth is the decoded request.intercept_after call (body omitted, schema 7).
type AfterAuth struct {
	RequestID string
	ToFormat  string
	Model     string
	Meta      Meta
}

// ResponseAfter is the decoded response.intercept_after call. RequestHeaders, OriginalRequest
// and RequestBody are skipped without materializing.
type ResponseAfter struct {
	RequestID       string
	SourceFormat    string
	Model           string
	RequestedModel  string
	Stream          bool
	ResponseHeaders http.Header
	Body            []byte
	BodyLen         int
	StatusCode      int
	OriginalAbsent  bool // OriginalRequest and RequestBody were null or empty
	Meta            Meta
}

// StreamChunk is the decoded response.intercept_stream_chunk call. RequestHeaders,
// OriginalRequest, RequestBody and HistoryChunks are skipped without materializing.
type StreamChunk struct {
	RequestID       string
	SourceFormat    string
	Model           string
	ChunkIndex      int
	ResponseHeaders http.Header // header-init only
	Body            []byte
	BodyLen         int
	Meta            Meta
}

// Completion is the decoded request.complete call.
type Completion struct {
	RequestID      string
	TraceID        string
	SourceFormat   string
	Model          string
	RequestedModel string
	Stream         bool
	Outcome        string
	StatusCode     int
	Error          string
	StartedAt      time.Time
	CompletedAt    time.Time
	Meta           Meta
}

// Usage is the decoded usage.handle call after the secret fields were reduced to fingerprints.
// It never holds APIKey, AuthID, or Source (except SourceClear, a Source kept clear because it equals the auth's own email or project).
type Usage struct {
	RequestID            string
	TraceID              string
	SelectionID          string
	Provider             string
	BaseURL              string
	ExecutorType         string
	Model                string
	Alias                string
	IsNativeKey          bool
	AccessProvider       string
	SessionID            string
	ParentSessionID      string
	AuthIndex            string
	AuthType             string
	ReasoningEffort      string
	ServiceTier          string
	EffectiveServiceTier string
	ResponseServiceTier  string
	ResponseModel        string
	Generate             bool
	Stream               bool
	RequestedAt          time.Time
	Latency              time.Duration
	TTFT                 time.Duration
	Failed               bool
	FailureStatus        int
	FailureBody          string
	Tokens               UsageDetail
	ResponseHeaders      http.Header

	// Fingerprints computed in the hook from the scratch buffer.
	CallerFP    string
	AuthFP      string
	AccountFP   string
	SourceClear string
	SourceKind  string // email | project | hashed | empty
}

// UsageDetail mirrors pluginapi.UsageDetail.
type UsageDetail struct {
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
}
