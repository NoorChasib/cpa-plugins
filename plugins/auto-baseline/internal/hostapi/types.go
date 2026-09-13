// Package hostapi defines the JSON wire contract between this plugin and the
// CLIProxyAPI plugin host, plus a typed bridge over the raw host-callback
// function.
//
// The shapes below were audited against CLIProxyAPI commit
// 81e1b5374f99c212f196f34956eeed964a46b8fa (v7.2.146-3-g81e1b53):
//
//   - Native ABI version 1, JSON RPC schema version 4
//     (sdk/pluginabi/types.go).
//   - Lifecycle requests carry `config_yaml` as a Go []byte, which standard
//     encoding/json transports as base64 text.
//   - Registration metadata (`pluginapi.Metadata`) has no JSON tags upstream,
//     so its wire keys are the capitalized Go field names.
//   - Management route/request/response types likewise have no JSON tags
//     upstream, while the registration wrapper uses lowercase
//     `routes` / `resources` and capabilities use snake_case
//     (internal/pluginhost/rpc_schema.go).
//   - Request interceptor calls (`request.intercept_before` /
//     `request.intercept_after`) carry `pluginapi.RequestInterceptRequest`
//     embedded in `rpcRequestInterceptRequest` (rpc_schema.go:89). The
//     embedded struct is untagged, so its keys are capitalized; only the
//     wrapper's `host_callback_id` is snake_case. The response is the
//     untagged `pluginapi.RequestInterceptResponse`; an empty object means
//     "no changes" (internal/pluginhost/adapters_interceptors.go:109-140).
//
// These types are written independently against that wire contract; no
// upstream implementation code is copied.
package hostapi

import "encoding/json"

// ABI / RPC schema constants negotiated with the host.
const (
	ABIVersion    uint32 = 1
	SchemaVersion uint32 = 4
)

// Lifecycle and capability RPC method names (host -> plugin).
const (
	MethodPluginRegister    = "plugin.register"
	MethodPluginReconfigure = "plugin.reconfigure"
	MethodPluginQuiesce     = "plugin.quiesce"
	MethodPluginShutdown    = "plugin.shutdown"

	MethodManagementRegister = "management.register"
	MethodManagementHandle   = "management.handle"

	MethodRequestInterceptBefore = "request.intercept_before"
	MethodRequestInterceptAfter  = "request.intercept_after"
)

// Host callback method names (plugin -> host). This plugin only logs; it
// never reads or writes credentials and never issues HTTP through the host.
const (
	MethodHostLog = "host.log"
)

// Envelope is the common RPC result wrapper used in both directions.
type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

// EnvelopeError carries a structured RPC failure.
type EnvelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// LifecycleRequest is the plugin.register / plugin.reconfigure request body.
// ConfigYAML is base64 on the wire because it is a []byte in the host's
// request struct; encoding/json decodes it transparently.
type LifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// Metadata mirrors upstream pluginapi.Metadata, whose fields have no JSON
// tags; the wire keys are therefore the capitalized Go names.
type Metadata struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	Logo             string        `json:"Logo,omitempty"`
	ConfigFields     []ConfigField `json:"ConfigFields"`
}

// ConfigField mirrors upstream pluginapi.ConfigField (no JSON tags upstream).
type ConfigField struct {
	Name        string   `json:"Name"`
	Type        string   `json:"Type"`
	EnumValues  []string `json:"EnumValues,omitempty"`
	Description string   `json:"Description"`
}

// Registration is the plugin.register / plugin.reconfigure result.
type Registration struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      Metadata     `json:"metadata"`
	Capabilities  Capabilities `json:"capabilities"`
}

// Capabilities declares only the capabilities this plugin implements.
// Capability keys are snake_case on the wire (rpc_schema.go:21-47).
type Capabilities struct {
	RequestInterceptor bool `json:"request_interceptor"`
	ManagementAPI      bool `json:"management_api"`
}

// ManagementRegistration is the management.register result. The wrapper keys
// are lowercase; the route fields are capitalized (untagged upstream).
type ManagementRegistration struct {
	Routes    []ManagementRoute `json:"routes,omitempty"`
	Resources []ResourceRoute   `json:"resources,omitempty"`
}

// ManagementRoute declares one authenticated Management API route. GET routes
// with a non-empty Menu are treated by the host as legacy resource routes, so
// authenticated routes must leave Menu empty.
type ManagementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

// ResourceRoute declares one unauthenticated, GET-only browser resource under
// /v0/resource/plugins/<pluginID>/. It must never perform mutations.
type ResourceRoute struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

// ManagementRequest is the management.handle request (untagged upstream, so
// capitalized keys; Body is base64 []byte on the wire).
type ManagementRequest struct {
	Method         string              `json:"Method"`
	Path           string              `json:"Path"`
	Headers        map[string][]string `json:"Headers"`
	Query          map[string][]string `json:"Query"`
	Body           []byte              `json:"Body"`
	HostCallbackID string              `json:"host_callback_id,omitempty"`
}

// ManagementResponse is the management.handle result (untagged upstream).
type ManagementResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers"`
	Body       []byte              `json:"Body"`
}

// RequestInterceptRequest is the request.intercept_before /
// request.intercept_after request body. It mirrors the untagged upstream
// pluginapi.RequestInterceptRequest (capitalized keys) embedded in the host's
// rpcRequestInterceptRequest wrapper, which adds host_callback_id.
//
// Headers are the inbound client's real request headers: the audited host
// passes a clone of the gin request header
// (sdk/api/handlers/handlers_execution.go:76-82 -> modelExecutionHeaders ->
// headersFromContext, handlers_context.go:137) into opts.Headers, which
// applyRequestInterceptorsBeforeAuth copies into this request
// (handlers_interceptors.go:453-463).
//
// All fields are retained for wire parity with the host; the runtime's hot
// path decodes a Headers-only projection and this plugin never inspects
// Body or Metadata.
type RequestInterceptRequest struct {
	RequestID      string              `json:"RequestID"`
	TraceID        string              `json:"TraceID"`
	SourceFormat   string              `json:"SourceFormat"`
	ToFormat       string              `json:"ToFormat"`
	Model          string              `json:"Model"`
	RequestedModel string              `json:"RequestedModel"`
	Stream         bool                `json:"Stream"`
	Headers        map[string][]string `json:"Headers"`
	Body           json.RawMessage     `json:"Body,omitempty"`
	Metadata       json.RawMessage     `json:"Metadata,omitempty"`
	HostCallbackID string              `json:"host_callback_id,omitempty"`
}

// RequestInterceptResponse is the request.intercept_* result (untagged
// upstream). This plugin always answers with the zero value, which the host
// applies as "no header changes, no body changes, do not terminate".
type RequestInterceptResponse struct {
	Headers         map[string][]string `json:"Headers,omitempty"`
	Body            []byte              `json:"Body,omitempty"`
	ClearHeaders    []string            `json:"ClearHeaders,omitempty"`
	Terminate       bool                `json:"Terminate,omitempty"`
	StatusCode      int                 `json:"StatusCode,omitempty"`
	ResponseHeaders map[string][]string `json:"ResponseHeaders,omitempty"`
	ResponseBody    []byte              `json:"ResponseBody,omitempty"`
}

// LogRequest is the host.log request.
type LogRequest struct {
	Level   string         `json:"level,omitempty"`
	Message string         `json:"message,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
}
