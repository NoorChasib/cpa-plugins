package wire

import (
	"encoding/json"
	"time"
)

// ExchangeBegin is the body of an exchange.begin envelope: the before-auth request.
type ExchangeBegin struct {
	RequestID      string       `json:"request_id"`
	InputSeq       uint32       `json:"input_seq"`            // 0; Codex duplex follow-ups 1, 2, ...
	InputType      string       `json:"input_type,omitempty"` // response.create | response.append | response.steer
	TraceID        string       `json:"trace_id,omitempty"`
	ExecSessionID  string       `json:"exec_session_id,omitempty"`
	ObservedAt     time.Time    `json:"observed_at"`
	SourceFormat   string       `json:"source_format"`
	RequestPath    string       `json:"request_path,omitempty"` // Metadata request_path (route pattern, never a query)
	Kind           string       `json:"kind"`                   // chat | count_tokens | compaction | prewarm | image | video_create | video_poll | speech | opaque
	Transport      string       `json:"transport"`              // http | ws
	RequestedModel string       `json:"requested_model"`
	Model          string       `json:"model"`
	Stream         bool         `json:"stream"`
	CallerFP       string       `json:"caller_fp,omitempty"`
	MetaSource     string       `json:"meta_source,omitempty"`   // Metadata source, e.g. plugin_host_model_callback
	ClientEffort   string       `json:"client_effort,omitempty"` // Metadata reasoning_effort
	ClientTier     string       `json:"client_tier,omitempty"`   // Metadata service_tier
	Generate       *bool        `json:"generate,omitempty"`
	IdempotencyFP  string       `json:"idempotency_fp,omitempty"`
	Hints          ArchiveHints `json:"hints"`
	Headers        []Header     `json:"headers"`
	Body           Body         `json:"body"`
}

// ExchangeAttempt is the body of an exchange.attempt envelope: one after-auth call.
type ExchangeAttempt struct {
	RequestID          string    `json:"request_id"`
	Ordinal            uint32    `json:"ordinal"` // 1-based order of after-auth calls
	ObservedAt         time.Time `json:"observed_at"`
	AuthIndex          string    `json:"auth_index"`
	AuthFP             string    `json:"auth_fp"` // HMAC(selected_auth_id)
	ToFormat           string    `json:"to_format"`
	Model              string    `json:"model"`
	CanonicalSessionID string    `json:"canonical_session_id,omitempty"`
	ParentSessionID    string    `json:"parent_session_id,omitempty"`
	SelectionID        string    `json:"selection_id,omitempty"`
	NodeKind           string    `json:"node_kind,omitempty"`
	IsFork             bool      `json:"is_fork,omitempty"`
	IsCompaction       bool      `json:"is_compaction,omitempty"`
}

// StreamEvent is small stream metadata kept with a response.
type StreamEvent struct {
	Type           string    `json:"type"` // response.steer.accepted | .failed | .pending | interrupt | ...
	At             time.Time `json:"at"`
	SteerID        string    `json:"steer_id,omitempty"`
	PrevResponseID string    `json:"prev_response_id,omitempty"`
}

// ExchangeResponse is the body of an exchange.response envelope.
type ExchangeResponse struct {
	RequestID        string        `json:"request_id"`
	Index            uint32        `json:"index"`                 // 0 for HTTP; successor responses on one RequestID 1, 2, ...
	ResponseID       string        `json:"response_id,omitempty"` // message.id / response.id / responseId
	PrevResponseID   string        `json:"prev_response_id,omitempty"`
	ObservedAt       time.Time     `json:"observed_at"`
	HeaderInitAt     *time.Time    `json:"header_init_at,omitempty"`
	FirstChunkAt     *time.Time    `json:"first_chunk_at,omitempty"`
	TerminalAt       *time.Time    `json:"terminal_at,omitempty"`
	StatusCode       int           `json:"status_code"`
	Stream           bool          `json:"stream"`
	TerminalStatus   string        `json:"terminal_status,omitempty"` // completed | incomplete | failed | done | superseded | canceled
	IncompleteReason string        `json:"incomplete_reason,omitempty"`
	Chunks           uint32        `json:"chunks,omitempty"`
	StreamBytes      int64         `json:"stream_bytes,omitempty"`
	Truncated        bool          `json:"truncated,omitempty"`
	BootstrapResets  uint32        `json:"bootstrap_resets,omitempty"`
	Events           []StreamEvent `json:"events,omitempty"`
	Headers          []Header      `json:"headers"`
	Body             Body          `json:"body"` // Base is always nil for responses
}

// ExchangeCompletion is the body of an exchange.completion envelope.
type ExchangeCompletion struct {
	RequestID      string            `json:"request_id"`
	TraceID        string            `json:"trace_id,omitempty"`
	SourceFormat   string            `json:"source_format"`
	Model          string            `json:"model"`
	RequestedModel string            `json:"requested_model"`
	Stream         bool              `json:"stream"`
	Outcome        string            `json:"outcome"` // succeeded | failed | rejected | canceled
	StatusCode     int               `json:"status_code"`
	Error          string            `json:"error,omitempty"` // masked, <= 4 KiB
	StartedAt      time.Time         `json:"started_at"`
	CompletedAt    time.Time         `json:"completed_at"`
	BeginWritten   bool              `json:"begin_written"`
	Responses      uint32            `json:"responses"`
	HookMicros     map[string]uint32 `json:"hook_micros,omitempty"` // time spent inside this plugin's sync hooks, per capability
}

// MediaFetch is NOT an envelope and is never queued in the outbox. The plugin keeps it in memory only
// (bounded queue of 256, dropped on restart, counter dropped.media_fetch) and sends it best-effort to
// POST /v1/media-fetch until acked or expired. The server keeps URL only in /data/blobs/.fetchq.
type MediaFetch struct {
	RequestID string     `json:"request_id"`
	Ordinal   uint32     `json:"ordinal"`
	URL       string     `json:"url"`        // the only unmasked secret-bearing value in v1; memory and the fetch route only
	MaskedURL string     `json:"masked_url"` // StripURL(URL): equals the string stored in the exchange.response body
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Tokens are the token counts of one usage attempt.
type Tokens struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	Reasoning     int64 `json:"reasoning"`
	Cached        int64 `json:"cached"`
	CacheRead     int64 `json:"cache_read"`
	CacheCreation int64 `json:"cache_creation"`
	Total         int64 `json:"total"`
}

// UsageAttempt is the body of a usage.attempt envelope: one CPA UsageRecord.
type UsageAttempt struct {
	UsageRequestID       string    `json:"usage_request_id"`
	TraceID              string    `json:"trace_id,omitempty"`
	SelectionID          string    `json:"selection_id,omitempty"`
	Provider             string    `json:"provider"`
	BaseURL              string    `json:"base_url,omitempty"` // StripURL
	ExecutorType         string    `json:"executor_type,omitempty"`
	Model                string    `json:"model"`
	Alias                string    `json:"alias,omitempty"`
	CallerFP             string    `json:"caller_fp,omitempty"` // CallerScope(APIKey); APIKey never kept
	IsNativeKey          bool      `json:"is_native_key"`
	AccessProvider       string    `json:"access_provider,omitempty"`
	SessionID            string    `json:"session_id,omitempty"`
	ParentSessionID      string    `json:"parent_session_id,omitempty"`
	AuthIndex            string    `json:"auth_index,omitempty"`
	AuthFP               string    `json:"auth_fp,omitempty"`
	AuthType             string    `json:"auth_type,omitempty"`
	AccountFP            string    `json:"account_fp,omitempty"`
	SourceClear          string    `json:"source_clear,omitempty"`
	SourceKind           string    `json:"source_kind"` // email | project | hashed | empty
	AccountLabel         string    `json:"account_label,omitempty"`
	ReasoningEffort      string    `json:"reasoning_effort,omitempty"`
	ServiceTier          string    `json:"service_tier,omitempty"`
	EffectiveServiceTier string    `json:"effective_service_tier,omitempty"`
	ResponseServiceTier  string    `json:"response_service_tier,omitempty"`
	ResponseModel        string    `json:"response_model,omitempty"`
	Generate             bool      `json:"generate"`
	Stream               bool      `json:"stream"`
	RequestedAt          time.Time `json:"requested_at"`
	LatencyMS            int64     `json:"latency_ms"`
	TTFTMS               int64     `json:"ttft_ms"`
	Failed               bool      `json:"failed"`
	FailureStatus        int       `json:"failure_status,omitempty"`
	FailureBody          *Hash     `json:"failure_body,omitempty"` // text piece, masked
	Tokens               Tokens    `json:"tokens"`
	ResponseHeaders      []Header  `json:"response_headers,omitempty"`
}

// ImportRecord is the body of an import.record envelope. Import bodies hold content and identity
// only; machine, file position, size and mtime are in Envelope.Loc.
type ImportRecord struct {
	Client     string          `json:"client"`               // claude-code | codex
	FileRole   string          `json:"file_role"`            // main | subagent | workflow_agent | workflow_journal | codex_rollout
	SessionID  string          `json:"session_id,omitempty"` // Claude sessionId; Codex session_id (root thread)
	ThreadID   string          `json:"thread_id,omitempty"`  // Codex session_meta.id
	AgentID    string          `json:"agent_id,omitempty"`
	RecordUUID string          `json:"record_uuid,omitempty"`
	RecordType string          `json:"record_type"` // Claude `type`; Codex `type` or `type/payload.type`
	RecordTS   *time.Time      `json:"record_ts,omitempty"`
	Mode       string          `json:"mode"`                // full | metadata
	Inherited  bool            `json:"inherited,omitempty"` // Codex fork: ordinal < subagent_history_start_ordinal
	Frame      json.RawMessage `json:"frame"`               // record minus content and dropped fields; canon/v1, masked
	Items      []Item          `json:"items,omitempty"`     // message content, split with the Claude/Responses block rules
	Extra      *Hash           `json:"extra,omitempty"`     // json piece: kept structured fields
}

// ImportFile is the body of an import.file envelope.
type ImportFile struct {
	Client  string `json:"client"`
	FileKey string `json:"file_key"` // also hashed into the key, so a same key always has the same file_key
	Content Hash   `json:"content"`  // text piece (masked) or attachment
	Kind    string `json:"kind"`     // tool_result_file | workflow_run | workflow_script | agent_meta | skill
}
