package protocol

import (
	"encoding/json"
	"net/http"
	"time"
)

// Spans are raw value spans that alias the RPC buffer. They are valid only during the call.
type Spans struct {
	Body []byte // the Body field (base64), nil when absent
}

func decodeInto(val []byte, v any) error {
	if IsNull(val) {
		return nil
	}
	if err := json.Unmarshal(val, v); err != nil {
		return ErrSyntax
	}
	return nil
}

func decodeHeaders(val []byte) (http.Header, error) {
	if IsNull(val) {
		return nil, nil
	}
	var h http.Header
	if err := json.Unmarshal(val, &h); err != nil {
		return nil, ErrSyntax
	}
	return h, nil
}

// DecodeMeta reads the Metadata keys the plugin keeps; every other key is skipped without
// decoding. A non-object (null) leaves Present false.
func DecodeMeta(val []byte) (Meta, error) {
	var m Meta
	if len(val) == 0 || IsNull(val) {
		return m, nil
	}
	m.Present = true
	err := Fields(val, func(k, v []byte) error {
		var dst *string
		switch string(k) {
		case "request_path":
			dst = &m.RequestPath
		case "execution_session_id":
			dst = &m.ExecutionSessionID
		case "idempotency_key":
			dst = &m.IdempotencyKey
		case "source":
			dst = &m.Source
		case "reasoning_effort":
			dst = &m.ReasoningEffort
		case "service_tier":
			dst = &m.ServiceTier
		case "caller_scope":
			dst = &m.CallerScope
		case "selected_auth_id":
			dst = &m.SelectedAuthID
		case "selected_auth_index":
			dst = &m.SelectedAuthIndex
		case "canonical_session_id":
			dst = &m.CanonicalSessionID
		case "parent_session_id":
			dst = &m.ParentSessionID
		case "selection_id":
			dst = &m.SelectionID
		case "node_kind":
			dst = &m.NodeKind
		case "generate":
			if b, ok := boolValue(v); ok {
				m.Generate = &b
			}
			return nil
		case "is_fork":
			m.IsFork, _ = boolValue(v)
			return nil
		case "is_compaction":
			m.IsCompaction, _ = boolValue(v)
			return nil
		default:
			return nil
		}
		if len(v) > 0 && v[0] == '"' {
			s, err := String(v)
			if err == nil {
				*dst = s
			}
		}
		return nil
	})
	if err != nil {
		return Meta{}, err
	}
	return m, nil
}

func boolValue(v []byte) (bool, bool) {
	switch string(v) {
	case "true", `"true"`:
		return true, true
	case "false", `"false"`:
		return false, true
	}
	return false, false
}

// ScanBeforeAuth decodes request.intercept_before except the Body, whose span is returned so the
// hook can reserve budget from its length before decoding it.
func ScanBeforeAuth(raw []byte) (BeforeAuth, Spans, error) {
	var r BeforeAuth
	var sp Spans
	err := Fields(raw, func(k, v []byte) (err error) {
		switch string(k) {
		case "RequestID":
			r.RequestID, err = String(v)
		case "TraceID":
			r.TraceID, err = String(v)
		case "SourceFormat":
			r.SourceFormat, err = String(v)
		case "Model":
			r.Model, err = String(v)
		case "RequestedModel":
			r.RequestedModel, err = String(v)
		case "Stream":
			r.Stream, _ = boolValue(v)
		case "Headers":
			r.Headers, err = decodeHeaders(v)
		case "Body":
			sp.Body = v
			r.BodyLen = Base64Len(v) / 4 * 3
		case "CallerScope":
			r.CallerScope, err = String(v)
		case "Metadata":
			r.Meta, err = DecodeMeta(v)
		}
		return err
	})
	return r, sp, err
}

// ScanHeaders returns only the Headers of a before-auth call, for an oversize call whose body is
// never decoded but whose X-Archive-* headers must still be cleared.
func ScanHeaders(raw []byte) (string, http.Header, error) {
	var id string
	var h http.Header
	err := Fields(raw, func(k, v []byte) (err error) {
		switch string(k) {
		case "Headers":
			h, err = decodeHeaders(v)
		case "RequestID":
			id, err = String(v)
		}
		return err
	})
	return id, h, err
}

// ScanRequestID returns only the RequestID of a hook call.
func ScanRequestID(raw []byte) string {
	var id string
	_ = Fields(raw, func(k, v []byte) error {
		if string(k) == "RequestID" {
			id, _ = String(v)
		}
		return nil
	})
	return id
}

// DecodeAfterAuth decodes request.intercept_after (body omitted at schema 7; any Body is
// skipped).
func DecodeAfterAuth(raw []byte) (AfterAuth, error) {
	var r AfterAuth
	err := Fields(raw, func(k, v []byte) (err error) {
		switch string(k) {
		case "RequestID":
			r.RequestID, err = String(v)
		case "ToFormat":
			r.ToFormat, err = String(v)
		case "Model":
			r.Model, err = String(v)
		case "Metadata":
			r.Meta, err = DecodeMeta(v)
		}
		return err
	})
	return r, err
}

// ScanResponse decodes response.intercept_after except Body (span returned). RequestHeaders,
// OriginalRequest and RequestBody are skipped without materializing, so the upstream
// credentials in RequestHeaders never reach the Go heap.
func ScanResponse(raw []byte) (ResponseAfter, Spans, error) {
	var r ResponseAfter
	var sp Spans
	r.OriginalAbsent = true
	err := Fields(raw, func(k, v []byte) (err error) {
		switch string(k) {
		case "RequestID":
			r.RequestID, err = String(v)
		case "SourceFormat":
			r.SourceFormat, err = String(v)
		case "Model":
			r.Model, err = String(v)
		case "RequestedModel":
			r.RequestedModel, err = String(v)
		case "Stream":
			r.Stream, _ = boolValue(v)
		case "ResponseHeaders":
			r.ResponseHeaders, err = decodeHeaders(v)
		case "Body":
			sp.Body = v
			r.BodyLen = Base64Len(v) / 4 * 3
		case "StatusCode":
			err = decodeInto(v, &r.StatusCode)
		case "OriginalRequest", "RequestBody":
			if Base64Len(v) > 0 {
				r.OriginalAbsent = false
			}
		case "Metadata":
			r.Meta, err = DecodeMeta(v)
		}
		return err
	})
	return r, sp, err
}

// ScanStreamChunk decodes response.intercept_stream_chunk except Body (span returned).
// RequestHeaders, OriginalRequest, RequestBody and HistoryChunks are skipped.
func ScanStreamChunk(raw []byte) (StreamChunk, Spans, error) {
	var r StreamChunk
	var sp Spans
	var headers []byte
	err := Fields(raw, func(k, v []byte) (err error) {
		switch string(k) {
		case "RequestID":
			r.RequestID, err = String(v)
		case "SourceFormat":
			r.SourceFormat, err = String(v)
		case "Model":
			r.Model, err = String(v)
		case "ChunkIndex":
			err = decodeInto(v, &r.ChunkIndex)
		case "ResponseHeaders":
			headers = v
		case "Body":
			sp.Body = v
			r.BodyLen = Base64Len(v) / 4 * 3
		case "Metadata":
			r.Meta, err = DecodeMeta(v)
		}
		return err
	})
	if err == nil && r.ChunkIndex == StreamHeaderInit && headers != nil {
		r.ResponseHeaders, err = decodeHeaders(headers)
	}
	return r, sp, err
}

// DecodeCompletion decodes request.complete.
func DecodeCompletion(raw []byte) (Completion, error) {
	var r Completion
	err := Fields(raw, func(k, v []byte) (err error) {
		switch string(k) {
		case "RequestID":
			r.RequestID, err = String(v)
		case "TraceID":
			r.TraceID, err = String(v)
		case "SourceFormat":
			r.SourceFormat, err = String(v)
		case "Model":
			r.Model, err = String(v)
		case "RequestedModel":
			r.RequestedModel, err = String(v)
		case "Stream":
			r.Stream, _ = boolValue(v)
		case "Outcome":
			r.Outcome, err = String(v)
		case "StatusCode":
			err = decodeInto(v, &r.StatusCode)
		case "Error":
			r.Error, err = String(v)
		case "StartedAt":
			err = decodeInto(v, &r.StartedAt)
		case "CompletedAt":
			err = decodeInto(v, &r.CompletedAt)
		case "Metadata":
			r.Meta, err = DecodeMeta(v)
		}
		return err
	})
	return r, err
}

// UsageSecrets are the three secret-bearing UsageRecord fields, unescaped into one scratch buffer
// that DecodeUsage zeroes after fn returns. fn must not retain any of the slices.
type UsageSecrets struct {
	APIKey, Source, AuthID []byte
}

// DecodeUsage decodes usage.handle. APIKey, Source and AuthID are never stored: they are
// unescaped into a scratch buffer, passed to fn together with the decoded record (so fn can set
// CallerFP, AuthFP, AccountFP, SourceClear and SourceKind), then zeroed.
func DecodeUsage(raw []byte, fn func(*Usage, UsageSecrets)) (Usage, error) {
	var r Usage
	var apiKey, source, authID []byte
	var latency, ttft int64
	err := Fields(raw, func(k, v []byte) (err error) {
		switch string(k) {
		case "RequestID":
			r.RequestID, err = String(v)
		case "TraceID":
			r.TraceID, err = String(v)
		case "SelectionID":
			r.SelectionID, err = String(v)
		case "Provider":
			r.Provider, err = String(v)
		case "BaseURL":
			r.BaseURL, err = String(v)
		case "ExecutorType":
			r.ExecutorType, err = String(v)
		case "Model":
			r.Model, err = String(v)
		case "Alias":
			r.Alias, err = String(v)
		case "APIKey":
			apiKey = v
		case "IsNativeKey":
			r.IsNativeKey, _ = boolValue(v)
		case "AccessProvider":
			r.AccessProvider, err = String(v)
		case "SessionID":
			r.SessionID, err = String(v)
		case "ParentSessionID":
			r.ParentSessionID, err = String(v)
		case "AuthID":
			authID = v
		case "AuthIndex":
			r.AuthIndex, err = String(v)
		case "AuthType":
			r.AuthType, err = String(v)
		case "Source":
			source = v
		case "ReasoningEffort":
			r.ReasoningEffort, err = String(v)
		case "ServiceTier":
			r.ServiceTier, err = String(v)
		case "EffectiveServiceTier":
			r.EffectiveServiceTier, err = String(v)
		case "ResponseServiceTier":
			r.ResponseServiceTier, err = String(v)
		case "ResponseModel":
			r.ResponseModel, err = String(v)
		case "Generate":
			r.Generate, _ = boolValue(v)
		case "Stream":
			r.Stream, _ = boolValue(v)
		case "RequestedAt":
			err = decodeInto(v, &r.RequestedAt)
		case "Latency":
			err = decodeInto(v, &latency)
		case "TTFT":
			err = decodeInto(v, &ttft)
		case "Failed":
			r.Failed, _ = boolValue(v)
		case "Failure":
			if IsNull(v) {
				return nil
			}
			err = Fields(v, func(k2, v2 []byte) (err error) {
				switch string(k2) {
				case "StatusCode":
					err = decodeInto(v2, &r.FailureStatus)
				case "Body":
					r.FailureBody, err = String(v2)
				}
				return err
			})
		case "Detail":
			err = decodeInto(v, &r.Tokens)
		case "ResponseHeaders":
			r.ResponseHeaders, err = decodeHeaders(v)
		}
		return err
	})
	if err != nil {
		return Usage{}, err
	}
	r.Latency, r.TTFT = time.Duration(latency), time.Duration(ttft)
	// One allocation sized from the raw spans (an unescaped string is never longer than its
	// JSON form), so append never reallocates and leaves a copy behind.
	var stack [512]byte
	scratch := stack[:0]
	if need := len(apiKey) + len(source) + len(authID); need > len(stack) {
		scratch = make([]byte, 0, need)
	}
	defer clear(stack[:])
	var s UsageSecrets
	var e1, e2, e3 error
	scratch, e1 = UnquoteInto(scratch, apiKey)
	a := len(scratch)
	scratch, e2 = UnquoteInto(scratch, source)
	b := len(scratch)
	scratch, e3 = UnquoteInto(scratch, authID)
	defer clear(scratch[:cap(scratch)])
	if e1 != nil || e2 != nil || e3 != nil {
		return Usage{}, ErrSyntax
	}
	s.APIKey, s.Source, s.AuthID = scratch[:a:a], scratch[a:b:b], scratch[b:]
	if fn != nil {
		fn(&r, s)
	}
	return r, nil
}
