package usage

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/NoorChasib/cpa-plugin-token-usage/internal/protocol"
)

const (
	FlagTimeFallback uint64 = 1 << iota
	FlagMissingTokens
	FlagUnknownExecutor
	FlagMissingGenerate
)

type Event struct {
	ID                                   string
	Provider, ExecutorType, Model, Alias string
	RequestedAt, ReceivedAt              time.Time
	Failed, Generate                     bool
	Status                               int
	Flags                                uint64
	Tokens                               protocol.UsageDetail
}

type wire struct {
	Provider, ExecutorType, Model, Alias string
	RequestedAt                          json.RawMessage
	Failed                               bool
	Generate                             *bool
	Failure                              struct{ StatusCode int }
	Detail                               struct{ InputTokens, OutputTokens, TotalTokens, ReasoningTokens, CachedTokens, CacheReadTokens, CacheCreationTokens *int64 }
}

func MetadataValid(s string, required bool) bool {
	if len(s) > 512 || !utf8.ValidString(s) || (required && strings.TrimSpace(s) == "") {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func Decode(raw []byte, now time.Time) (Event, error) {
	var w wire
	var e Event
	invalid := errors.New("invalid usage record")
	if len(raw) > protocol.MaxRequestBytes || !utf8.Valid(raw) || json.Unmarshal(raw, &w) != nil {
		return e, invalid
	}
	if !MetadataValid(w.Provider, true) || !MetadataValid(w.Model, true) || !MetadataValid(w.ExecutorType, false) || !MetadataValid(w.Alias, false) || w.Failure.StatusCode < 0 || w.Failure.StatusCode > 599 {
		return e, invalid
	}
	e.Provider, e.ExecutorType, e.Model, e.Alias = w.Provider, w.ExecutorType, w.Model, w.Alias
	e.ReceivedAt = now.UTC()
	e.RequestedAt = e.ReceivedAt
	var at string
	if json.Unmarshal(w.RequestedAt, &at) == nil {
		if parsed, err := time.Parse(time.RFC3339Nano, at); err == nil && !parsed.IsZero() {
			e.RequestedAt = parsed.UTC()
		} else {
			e.Flags |= FlagTimeFallback
		}
	} else {
		e.Flags |= FlagTimeFallback
	}
	if e.RequestedAt.Year() < 1970 || e.RequestedAt.Year() > 2100 || e.RequestedAt.After(now.Add(5*time.Minute)) {
		return Event{}, invalid
	}
	if strings.TrimSpace(e.ExecutorType) == "" {
		e.ExecutorType = "unknown"
		e.Flags |= FlagUnknownExecutor
	}
	e.Generate = true
	if w.Generate == nil {
		e.Flags |= FlagMissingGenerate
	} else {
		e.Generate = *w.Generate
	}
	e.Failed, e.Status = w.Failed, w.Failure.StatusCode
	from := []*int64{w.Detail.InputTokens, w.Detail.OutputTokens, w.Detail.TotalTokens, w.Detail.ReasoningTokens, w.Detail.CachedTokens, w.Detail.CacheReadTokens, w.Detail.CacheCreationTokens}
	to := []*int64{&e.Tokens.InputTokens, &e.Tokens.OutputTokens, &e.Tokens.TotalTokens, &e.Tokens.ReasoningTokens, &e.Tokens.CachedTokens, &e.Tokens.CacheReadTokens, &e.Tokens.CacheCreationTokens}
	for i, v := range from {
		if v == nil {
			e.Flags |= FlagMissingTokens
			continue
		}
		if *v < 0 {
			return Event{}, invalid
		}
		*to[i] = *v
	}
	return e, nil
}

func (e Event) WireRecord() protocol.UsageRecord {
	r := protocol.UsageRecord{Provider: e.Provider, ExecutorType: e.ExecutorType, Model: e.Model, Alias: e.Alias, RequestedAt: e.RequestedAt, Failed: e.Failed, Generate: e.Generate, Detail: e.Tokens}
	r.Failure.StatusCode = e.Status
	return r
}
