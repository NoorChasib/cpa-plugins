package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// Typed errors. Callers must not retry ErrUnauthorized.
var (
	// ErrUnauthorized: 401 (a bad or missing token), and also 403, wrapped as
	// "client: unauthorized (HTTP 403)". The archive never sends 403, so one means a proxy or a
	// misrouted request: an auth-class failure, not a transient one. Either way stop uploading 15
	// minutes, raise auth_failed, never loop.
	ErrUnauthorized = errors.New("client: unauthorized")
	// ErrMalformed: 400 other than unsupported_protocol (framing, bad JSON, piece_hash_mismatch).
	ErrMalformed = errors.New("client: malformed request")
	// ErrTooLarge: 413.
	ErrTooLarge = errors.New("client: request too large")
	// ErrUnsupportedProtocol: 400 {"error":"unsupported_protocol"}. Producers keep the envelopes
	// pending and raise server_too_old instead of halving the batch.
	ErrUnsupportedProtocol = errors.New("client: unsupported protocol")
)

// EpochChangedError is a 409 {"error":"epoch_changed","epoch":N,"archive_id":"…"}.
type EpochChangedError struct {
	Epoch     uint64
	ArchiveID string
}

// Error implements error.
func (e *EpochChangedError) Error() string {
	return fmt.Sprintf("client: epoch changed (epoch %d, archive %s)", e.Epoch, e.ArchiveID)
}

// RetryAfterError is a 429 or 503; After is the Retry-After header value, 0 when absent or
// unparsable. The caller waits at least that long before the next attempt.
type RetryAfterError struct {
	After time.Duration
}

// Error implements error.
func (e *RetryAfterError) Error() string {
	return fmt.Sprintf("client: server busy, retry after %s", e.After)
}

// StatusError is any other non-2xx answer: 409 from PUT /v1/callers, 422 from PUT /v1/pieces
// ({"reason":"mask_violation","rules":[…]}), 5xx and unexpected 4xx other than 403. Code is the body's "error" or
// "reason" field when present; Rules are the 422 rule ids.
type StatusError struct {
	StatusCode int
	Code       string
	Rules      []string
}

// Error implements error.
func (e *StatusError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("client: HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("client: HTTP %d %s", e.StatusCode, e.Code)
}

// statusError maps one non-2xx answer to its typed error. The body is read only
// as a wire.ErrorBody; anything else in it is ignored.
func statusError(status int, h http.Header, body []byte) error {
	var eb wire.ErrorBody
	_ = json.Unmarshal(body, &eb) // best effort: a non-JSON body leaves the codes empty
	code := eb.Error
	if code == "" {
		code = eb.Reason
	}
	switch {
	case status == http.StatusUnauthorized:
		return ErrUnauthorized
	case status == http.StatusForbidden:
		return fmt.Errorf("%w (HTTP 403)", ErrUnauthorized)
	case status == http.StatusBadRequest && eb.Error == wire.ErrCodeUnsupportedProtocol:
		return ErrUnsupportedProtocol
	case status == http.StatusBadRequest:
		if code != "" {
			return fmt.Errorf("%w: %s", ErrMalformed, code)
		}
		return ErrMalformed
	case status == http.StatusRequestEntityTooLarge:
		return ErrTooLarge
	case status == http.StatusConflict && eb.Error == wire.ErrCodeEpochChanged:
		return &EpochChangedError{Epoch: eb.Epoch, ArchiveID: eb.ArchiveID}
	case status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable:
		return &RetryAfterError{After: retryAfter(h.Get("Retry-After"), time.Now())}
	}
	return &StatusError{StatusCode: status, Code: code, Rules: eb.Rules}
}

// retryAfter parses a Retry-After value: delay seconds or an HTTP date. Absent, unparsable or past
// values are 0.
func retryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.ParseUint(v, 10, 32); err == nil {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}
