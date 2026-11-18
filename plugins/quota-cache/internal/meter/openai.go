package meter

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// openAIWire is the part of CPA's pluginapi.UsageRecord the OpenAI meter
// reads. It has no Source, APIKey, BaseURL, Alias, TraceID, RequestID,
// SelectionID, AccessProvider, SessionID, ParentSessionID or AuthID field,
// and reads Provider only through openAIProvider: an openai-compatibility
// provider key and auth ID both carry the entry's name, which the operator
// typed, so neither is ever materialized. As with usageWire, every key is the
// Go field name and the header is matched case-insensitively.
type openAIWire struct {
	Provider                                                       openAIProvider
	AuthIndex, AuthType, Model, ResponseModel, ResponseServiceTier string
	// RequestedAt is raw so that a timestamp that is not a string never
	// rejects the record; the received time stands in for it.
	RequestedAt json.RawMessage
	Latency     int64
	Failed      bool
	Failure     struct {
		StatusCode int
		Body       string
	}
	Detail          struct{ InputTokens, OutputTokens, CacheReadTokens, CacheCreationTokens int64 }
	ResponseHeaders struct {
		Organization []string `json:"Openai-Organization"`
	}
}

// openAICompatibleProvider is how CPA's provider key for a named
// openai-compatibility entry begins; the entry's lower-cased name follows.
// An unnamed entry's key is "openai-compatibility".
const openAICompatibleProvider = "openai-compatible-"

// openAIProvider is whether a record's Provider is an openai-compatibility
// provider key. The raw JSON is tested before any string is made, so the
// entry name it carries is never materialized. It never fails, so an odd
// Provider never rejects a record.
type openAIProvider bool

func (p *openAIProvider) UnmarshalJSON(b []byte) error {
	*p = false
	// A JSON string. Anything else, null, a number or an object included, is
	// no openai-compatibility key. Both keys are compared byte for byte with
	// literals that hold no backslash, so an escape in the unnamed key or in
	// the named prefix never matches. One in the entry name after the prefix
	// does: CPA's encoder escapes &, < and > in the names operators type.
	if len(b) < 2 || b[0] != '"' || b[len(b)-1] != '"' {
		return nil
	}
	key := b[1 : len(b)-1]
	*p = openAIProvider(bytes.EqualFold(key, []byte("openai-compatibility")) ||
		(len(key) > len(openAICompatibleProvider) && bytes.EqualFold(key[:len(openAICompatibleProvider)], []byte(openAICompatibleProvider))))
	return nil
}

// decodeOpenAI reads one record for the OpenAI meter. rejected is set when it
// cannot be read, carries a negative token count, or is an
// openai-compatibility API-key record whose openai-organization header names
// no organization NormalizeOpenAIOrganization reads: a named organization's
// slug is undocumented, so spend the shape refuses is flagged, not passed
// over. keep is set when it is an openai-compatibility API-key record, the
// only kind this meter counts, with e filled in. A codex record, a
// codex-api-key's included, is never kept: OpenAI says Codex usage through
// API keys is not covered by the credit.
func decodeOpenAI(raw []byte, received time.Time) (e event, keep, rejected bool) {
	var w openAIWire
	if json.Unmarshal(raw, &w) != nil {
		return event{}, false, true
	}
	d := w.Detail
	if d.InputTokens < 0 || d.OutputTokens < 0 || d.CacheReadTokens < 0 || d.CacheCreationTokens < 0 {
		return event{}, false, true
	}
	if !bool(w.Provider) || !strings.EqualFold(w.AuthType, "apikey") {
		return event{}, false, false
	}
	// OpenAI reports the whole prompt as input, cached and written tokens
	// included, and the meter keeps input without them, as Anthropic bills
	// it. A record whose cached and written tokens exceed its input is kept
	// as reported, with no input.
	total, cacheRead, cacheWrite := uint64(d.InputTokens), uint64(d.CacheReadTokens), uint64(d.CacheCreationTokens)
	// Output includes reasoning, so ReasoningTokens is never added.
	e = event{at: completedAt(w.RequestedAt, w.Latency, received), failed: w.Failed,
		input: total - min(total, cacheRead+cacheWrite), output: uint64(d.OutputTokens), cacheRead: cacheRead, cacheWrite: cacheWrite}
	if index := strings.TrimSpace(w.AuthIndex); len(index) <= maxAuthIndex {
		e.authIndex = index
	}
	if values := w.ResponseHeaders.Organization; len(values) > 0 && strings.TrimSpace(values[0]) != "" {
		org, ok := client.NormalizeOpenAIOrganization(values[0])
		if !ok {
			return event{}, false, true
		}
		e.org = org
	}
	e.model = openAIMeterModel(w.Model, w.ResponseModel)
	// Long context is judged on the whole input, as OpenAI prices it.
	e.prompt = client.OpenAIMeterPromptClass(total)
	e.tier = client.NormalizeOpenAIServiceTier(w.ResponseServiceTier)
	if w.Failed {
		// OpenAI's code for an organization with no credit left, which a
		// plain rate limit or insufficient_quota alone is not.
		e.refusal = w.Failure.StatusCode == 429 && strings.Contains(w.Failure.Body, "credit_balance_exhausted")
		// CPA's status for a client that went away before the attempt
		// finished, which says nothing about the credential.
		e.cancelled = w.Failure.StatusCode == 499
	}
	return e, true, false
}

// openAIMeterModel is client.NormalizeMeterModel's name for the record, after
// a fine-tuned id "ft:<base>:<org>:<suffix>:<id>" is cut to "ft:<base>": the
// rest is the operator's organization name and chosen suffix.
func openAIMeterModel(model, responseModel string) string {
	name := strings.TrimSpace(responseModel)
	if name == "" {
		name = strings.TrimSpace(model)
	}
	if len(name) >= 3 && strings.EqualFold(name[:3], "ft:") {
		if i := strings.IndexByte(name[3:], ':'); i >= 0 {
			name = name[:3+i]
		}
	}
	return client.NormalizeMeterModel(name, "")
}
