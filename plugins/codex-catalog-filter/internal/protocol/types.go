// Package protocol mirrors the subset of CPA ABI 1 / schema 6 used here.
// See REFERENCE.md for the exact upstream source pin.
package protocol

import (
	"encoding/json"
	"net/http"
)

const (
	ABIVersion    uint32 = 1
	SchemaVersion uint32 = 6
	// MaxRequestBytes bounds one native request before it is copied into Go.
	// A Codex catalog carries full model instructions and is several MiB once
	// base64-encoded on the wire, so this is far above token-usage's 1 MiB.
	MaxRequestBytes = 64 << 20

	MethodPluginRegister         = "plugin.register"
	MethodPluginReconfigure      = "plugin.reconfigure"
	MethodPluginQuiesce          = "plugin.quiesce"
	MethodPluginShutdown         = "plugin.shutdown"
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

// A response interceptor sees every successful non-streaming response and,
// from CPA v8.0.0, every model-list response. Nothing else is advertised.
type RegistrationCapabilities struct {
	ResponseInterceptor bool `json:"response_interceptor"`
}

// ResponseInterceptRequest has no JSON tags upstream, so fields travel in
// PascalCase and byte slices as base64. Only the fields read here are declared;
// request headers, request bodies, and metadata are skipped without decoding.
type ResponseInterceptRequest struct {
	SourceFormat    string
	Model           string
	RequestedModel  string
	Stream          bool
	StatusCode      int
	ResponseHeaders http.Header
	Body            []byte
}

// An empty response ({}) leaves CPA's current headers and body unchanged.
// CPA replaces the body only when Body is non-empty.
type ResponseInterceptResponse struct {
	Headers      http.Header `json:",omitempty"`
	Body         []byte      `json:",omitempty"`
	ClearHeaders []string    `json:",omitempty"`
}
