package hooks

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/fixture"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/headers"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// OK is the pre-encoded pass-through result of every synchronous hook.
var OK = []byte(`{"ok":true,"result":{}}`)

func okClear(clear []string) []byte {
	if len(clear) == 0 {
		return OK
	}
	b, err := json.Marshal(protocol.Envelope{OK: true, Result: mustJSON(protocol.RequestInterceptResponse{ClearHeaders: clear})})
	if err != nil {
		return OK
	}
	return b
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// credentialHeaders are deleted from the decoded before-auth headers at once, so a raw client
// key never travels in a queued work item (pkg/headers drops them later anyway).
var credentialHeaders = []string{"authorization", "proxy-authorization", "x-api-key", "x-goog-api-key", "cookie", "x-management-key"}

func (e *Engine) finish(h Hook, rid string, start time.Time) {
	d := e.Now().Sub(start)
	e.Stats.Call(h, start, d)
	if rid != "" {
		us := uint32(min(max(d.Microseconds(), 0), 1<<31))
		e.With(rid, false, func(en *Entry) { en.HookMicros[h] += us })
	}
}

func (e *Engine) record(h Hook, chunk, n int, reason string) {
	if fixture.Enabled {
		e.Fixture.Add(fixture.Record{Hook: h.String(), Chunk: chunk, Len: n, Reason: reason})
	}
}

// BeforeAuth handles request.intercept_before. raw aliases the C request buffer.
func (e *Engine) BeforeAuth(raw []byte) []byte {
	start := e.Now()
	cfg := e.cfg.Load()
	if len(raw) > cfg.MaxRPC {
		// Oversize: the body is never decoded, but X-Archive-* headers are still cleared and the
		// input_seq still advances; no begin is written (begin_written=false).
		e.Stats.Drop("oversize_rpc")
		rid, h, _ := protocol.ScanHeaders(raw)
		if rid != "" && cfg.Configured && cfg.Capture != "off" && e.admit() {
			e.With(rid, true, func(en *Entry) { en.BeforeAuthSeen = true; en.NextInputSeq++ })
			e.leave()
		}
		e.record(HookBeforeAuth, 0, len(raw), "oversize_rpc")
		e.finish(HookBeforeAuth, rid, start)
		return okClear(headers.ArchiveHeaderNames(h))
	}
	req, sp, err := protocol.ScanBeforeAuth(raw)
	if err != nil {
		e.Stats.HookError(HookBeforeAuth)
		e.finish(HookBeforeAuth, "", start)
		return OK
	}
	out := okClear(headers.ArchiveHeaderNames(req.Headers))
	defer e.finish(HookBeforeAuth, req.RequestID, start)
	if !cfg.Configured || cfg.Capture == "off" || req.RequestID == "" || !e.admit() {
		return out
	}
	defer e.leave()
	for k := range req.Headers {
		for _, c := range credentialHeaders {
			if strings.EqualFold(k, c) {
				delete(req.Headers, k)
			}
		}
	}
	b := &Begin{Req: req, ObservedAt: start}
	e.With(req.RequestID, true, func(en *Entry) {
		en.BeforeAuthSeen = true
		b.InputSeq = en.NextInputSeq
		en.NextInputSeq++
	})
	if cfg.Capture != "full" {
		e.metaBegin(b, "")
		return out
	}
	need := int64(req.BodyLen) * BodyReserveFactor
	if !e.Pool.TryReserve(need) {
		e.Stats.Drop("body_budget")
		e.record(HookBeforeAuth, 0, req.BodyLen, "body_budget")
		e.metaBegin(b, wire.DroppedBodyBudget)
		return out
	}
	b.Reserved = need
	body, err := protocol.DecodeBase64(sp.Body)
	if err != nil {
		e.Pool.Release(need)
		e.Stats.HookError(HookBeforeAuth)
		e.metaBegin(b, wire.DroppedParseError)
		return out
	}
	b.Req.Body = body
	b.Req.BodyLen = len(body)
	e.With(req.RequestID, false, func(en *Entry) { en.Pending++ })
	select {
	case e.Handoff <- Work{Begin: b}:
		e.record(HookBeforeAuth, 0, len(body), "")
	default:
		e.With(req.RequestID, false, func(en *Entry) { en.Pending-- })
		e.Pool.Release(need)
		b.Req.Body, b.Reserved = nil, 0
		e.Stats.Drop("handoff_full")
		e.record(HookBeforeAuth, 0, len(body), "handoff_full")
		e.metaBegin(b, wire.DroppedBodyBudget)
	}
	return out
}

func (e *Engine) metaBegin(b *Begin, reason string) {
	b.MetaOnly = reason
	if reason == "" {
		b.MetaOnly = "metadata"
	}
	b.Req.Body = nil
	select {
	case e.Control <- Control{Begin: b}:
	default:
		e.Stats.Drop("control_full")
	}
}

// AfterAuth handles request.intercept_after (body omitted).
func (e *Engine) AfterAuth(raw []byte) []byte {
	start := e.Now()
	cfg := e.cfg.Load()
	if len(raw) > protocol.MaxSmallHook {
		e.Stats.Drop("oversize_rpc")
		e.finish(HookAfterAuth, "", start)
		return OK
	}
	req, err := protocol.DecodeAfterAuth(raw)
	if err != nil {
		e.Stats.HookError(HookAfterAuth)
		e.finish(HookAfterAuth, "", start)
		return OK
	}
	defer e.finish(HookAfterAuth, req.RequestID, start)
	if !cfg.Configured || cfg.Capture == "off" || req.RequestID == "" || !e.admit() {
		return OK
	}
	defer e.leave()
	a := &Attempt{Req: req, ObservedAt: start}
	e.With(req.RequestID, true, func(en *Entry) { en.NextOrdinal++; a.Ordinal = en.NextOrdinal })
	select {
	case e.Control <- Control{Attempt: a}:
	default:
		e.Stats.Drop("control_full")
	}
	return OK
}

// Response handles response.intercept_after.
func (e *Engine) Response(raw []byte) []byte {
	start := e.Now()
	cfg := e.cfg.Load()
	if len(raw) > cfg.MaxRPC {
		e.Stats.Drop("oversize_rpc")
		e.record(HookResponse, 0, len(raw), "oversize_rpc")
		e.finish(HookResponse, "", start)
		return OK
	}
	req, sp, err := protocol.ScanResponse(raw)
	if err != nil {
		e.Stats.HookError(HookResponse)
		e.finish(HookResponse, "", start)
		return OK
	}
	defer e.finish(HookResponse, req.RequestID, start)
	if !cfg.Configured || cfg.Capture == "off" || req.RequestID == "" {
		return OK
	}
	if req.Model == "" && req.RequestedModel == "" && req.OriginalAbsent && !req.Meta.Present {
		seen := false
		e.With(req.RequestID, false, func(en *Entry) { seen = en.BeforeAuthSeen })
		if !seen {
			e.Stats.Count("ignored_model_list", 1)
			return OK
		}
	}
	if !e.admit() {
		return OK
	}
	defer e.leave()
	r := &Response{RequestID: req.RequestID, SourceFormat: req.SourceFormat, Model: req.Model, StatusCode: req.StatusCode,
		Stream: req.Stream, Headers: req.ResponseHeaders, ObservedAt: start, Meta: req.Meta, RawLen: int64(req.BodyLen)}
	if req.ResponseHeaders != nil {
		r.ContentType = req.ResponseHeaders.Get("Content-Type")
		r.ContentEncoding = req.ResponseHeaders.Get("Content-Encoding")
	}
	e.With(req.RequestID, true, func(en *Entry) { r.Index = en.NextResponse; en.NextResponse++ })
	if cfg.Capture != "full" {
		e.metaResponse(r, "")
		return OK
	}
	need := int64(req.BodyLen) * BodyReserveFactor
	if !e.Pool.TryReserve(need) {
		e.Stats.Drop("body_budget")
		e.metaResponse(r, wire.DroppedBodyBudget)
		return OK
	}
	r.Reserved = need
	body, err := protocol.DecodeBase64(sp.Body)
	if err != nil {
		e.Pool.Release(need)
		e.Stats.HookError(HookResponse)
		e.metaResponse(r, wire.DroppedParseError)
		return OK
	}
	r.Body = body
	e.With(req.RequestID, false, func(en *Entry) { en.Pending++ })
	select {
	case e.Handoff <- Work{Response: r}:
		e.record(HookResponse, 0, len(body), "")
	default:
		e.With(req.RequestID, false, func(en *Entry) { en.Pending-- })
		e.Pool.Release(need)
		r.Body, r.Reserved = nil, 0
		e.Stats.Drop("handoff_full")
		e.metaResponse(r, wire.DroppedBodyBudget)
	}
	return OK
}

func (e *Engine) metaResponse(r *Response, reason string) {
	r.MetaOnly = reason
	if reason == "" {
		r.MetaOnly = "metadata"
	}
	r.Body = nil
	select {
	case e.Control <- Control{Response: r}:
	default:
		e.Stats.Drop("control_full")
	}
}

// StreamChunk handles response.intercept_stream_chunk: header-init (ChunkIndex -1) and payload
// chunks. Never sets DropChunk.
func (e *Engine) StreamChunk(raw []byte) []byte {
	start := e.Now()
	cfg := e.cfg.Load()
	hook := HookStreamChunk
	limit := protocol.MaxStreamChunk
	if len(raw) > protocol.MaxStreamChunk {
		// Could be a header-init (two request copies) under the body-hook cap; peek the index.
		idx := chunkIndex(raw)
		if idx == protocol.StreamHeaderInit {
			hook, limit = HookStreamHeaderInit, cfg.MaxRPC
		}
	}
	if len(raw) > limit {
		e.Stats.Drop("oversize_rpc")
		e.record(hook, 0, len(raw), "oversize_rpc")
		rid := protocol.ScanRequestID(raw)
		if rid != "" && cfg.Configured && cfg.Capture == "full" && e.admit() {
			e.With(rid, false, func(en *Entry) { en.Truncated = true })
			e.leave()
		}
		e.finish(hook, rid, start)
		return OK
	}
	req, sp, err := protocol.ScanStreamChunk(raw)
	if err != nil {
		e.Stats.HookError(hook)
		e.finish(hook, "", start)
		return OK
	}
	if req.ChunkIndex == protocol.StreamHeaderInit {
		hook = HookStreamHeaderInit
	}
	defer e.finish(hook, req.RequestID, start)
	if !cfg.Configured || cfg.Capture == "off" || req.RequestID == "" || !e.admit() {
		return OK
	}
	defer e.leave()
	it := StreamItem{RequestID: req.RequestID, SourceFormat: req.SourceFormat, Model: req.Model, ChunkIndex: req.ChunkIndex,
		Headers: req.ResponseHeaders, At: start, Meta: req.Meta}
	if req.ChunkIndex == protocol.StreamHeaderInit {
		e.With(req.RequestID, true, func(en *Entry) { en.ChunksQueued++; en.Stream = true; en.Truncated = false })
		select {
		case e.Stream <- it:
			e.record(hook, -1, 0, "")
		default:
			e.With(req.RequestID, false, func(en *Entry) { en.ChunksQueued--; en.Truncated = true })
			e.Stats.Drop("stream_overflow")
		}
		return OK
	}
	if cfg.Capture != "full" {
		return OK
	}
	n := int64(req.BodyLen)
	ok := false
	e.With(req.RequestID, true, func(en *Entry) {
		en.Stream = true
		if en.Truncated || en.StreamBytes+n > StreamPerStream {
			en.Truncated = true
			return
		}
		if !e.Pool.TryReserveStream(n) {
			en.Truncated = true
			return
		}
		en.StreamBytes += n
		en.ChunksQueued++
		ok = true
	})
	if !ok {
		e.Stats.Drop("stream_overflow")
		e.record(hook, req.ChunkIndex, req.BodyLen, "stream_overflow")
		return OK
	}
	body, err := protocol.DecodeBase64(sp.Body)
	if err != nil {
		e.unqueueChunk(req.RequestID, n)
		e.Stats.HookError(hook)
		return OK
	}
	it.Body, it.Reserved = body, n
	select {
	case e.Stream <- it:
		e.record(hook, req.ChunkIndex, len(body), "")
	default:
		e.unqueueChunk(req.RequestID, n)
		e.Stats.Drop("stream_overflow")
		e.record(hook, req.ChunkIndex, len(body), "stream_overflow")
	}
	return OK
}

func (e *Engine) unqueueChunk(rid string, n int64) {
	e.Pool.ReleaseStream(n)
	e.With(rid, false, func(en *Entry) {
		en.StreamBytes -= n
		en.ChunksQueued--
		en.Truncated = true
	})
}

func chunkIndex(raw []byte) int {
	idx := 0
	_ = protocol.Fields(raw, func(k, v []byte) error {
		if string(k) == "ChunkIndex" {
			_ = json.Unmarshal(v, &idx)
		}
		return nil
	})
	return idx
}

// Complete handles request.complete (CPA calls it on its own goroutine).
func (e *Engine) Complete(raw []byte) []byte {
	start := e.Now()
	cfg := e.cfg.Load()
	if len(raw) > protocol.MaxSmallHook {
		e.Stats.Drop("oversize_rpc")
		e.finish(HookLifecycle, "", start)
		return OK
	}
	req, err := protocol.DecodeCompletion(raw)
	if err != nil {
		e.Stats.HookError(HookLifecycle)
		e.finish(HookLifecycle, "", start)
		return OK
	}
	e.finish(HookLifecycle, req.RequestID, start)
	if !cfg.Configured || cfg.Capture == "off" || req.RequestID == "" {
		return OK
	}
	seen := false
	var queued uint64
	exists := e.With(req.RequestID, false, func(en *Entry) { seen = en.BeforeAuthSeen; queued = en.ChunksQueued })
	if req.Model == "" && !req.Meta.Present && !seen {
		e.Stats.Count("ignored_model_list", 1)
		if exists {
			e.Delete(req.RequestID)
		}
		return OK
	}
	if !e.admit() {
		return OK
	}
	defer e.leave()
	select {
	case e.Control <- Control{Done: &Done{Req: req, ChunksQueued: queued, ObservedAt: start}}:
	default:
		e.Stats.Drop("control_full")
	}
	return OK
}

// Usage handles usage.handle (CPA's single shared usage dispatcher). APIKey, Source and AuthID
// are reduced to fingerprints from a zeroed scratch buffer inside the call.
func (e *Engine) UsageHook(raw []byte) []byte {
	start := e.Now()
	cfg := e.cfg.Load()
	defer func() { e.Stats.Call(HookUsage, start, e.Now().Sub(start)) }()
	if len(raw) > protocol.MaxUsage {
		e.Stats.Drop("oversize_rpc")
		return OK
	}
	if !cfg.Configured || cfg.Capture == "off" {
		return OK
	}
	rec, err := protocol.DecodeUsage(raw, func(u *protocol.Usage, s protocol.UsageSecrets) {
		fingerprintUsage(cfg, u, s)
	})
	if err != nil {
		e.Stats.HookError(HookUsage)
		return OK
	}
	e.Stats.UsageSeen(start)
	if !e.admit() {
		return OK
	}
	defer e.leave()
	size := int64(len(rec.FailureBody)) + headerBytes(rec.ResponseHeaders) + 512
	if e.usageBytes.Add(size) > UsageQueueBytes {
		e.usageBytes.Add(-size)
		rec.FailureBody = ""
		size = headerBytes(rec.ResponseHeaders) + 512
		e.usageBytes.Add(size)
		e.Stats.Drop("usage_queue")
	}
	select {
	case e.Usage <- UsageItem{Rec: rec, Bytes: size, ObservedAt: start}:
	default:
		e.usageBytes.Add(-size)
		e.Stats.Drop("usage_queue")
	}
	return OK
}

func headerBytes(h map[string][]string) int64 {
	var n int64
	for k, vs := range h {
		n += int64(len(k))
		for _, v := range vs {
			n += int64(len(v))
		}
	}
	return n
}

const callerScopePrefix = "cli-proxy-api:caller-scope:v1\x00"

// CallerScopeBytes equals fingerprint.CallerScope without creating a string of the key.
func CallerScopeBytes(key []byte) string {
	key = bytes.TrimSpace(key)
	if len(key) == 0 {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(callerScopePrefix))
	h.Write(key)
	return hex.EncodeToString(h.Sum(nil))
}

// HMACBytes equals fingerprint.HMAC(keyID, key, parts...) for byte parts.
func HMACBytes(keyID string, key []byte, parts ...[]byte) string {
	m := hmac.New(sha256.New, key)
	for i, p := range parts {
		if i > 0 {
			m.Write([]byte{'|'})
		}
		m.Write(p)
	}
	return keyID + ":" + hex.EncodeToString(m.Sum(nil))
}

func fingerprintUsage(cfg *Config, u *protocol.Usage, s protocol.UsageSecrets) {
	u.CallerFP = CallerScopeBytes(s.APIKey)
	if len(s.AuthID) > 0 {
		u.AuthFP = HMACBytes(cfg.HMACKeyID, cfg.HMACKey, s.AuthID)
	}
	u.AccountFP = HMACBytes(cfg.HMACKeyID, cfg.HMACKey, []byte(u.Provider), s.Source)
	if len(s.Source) == 0 {
		u.SourceKind = "empty"
		return
	}
	u.SourceKind = "hashed"
	if cfg.Labels == nil || u.AuthIndex == "" {
		return
	}
	email, project, ok := cfg.Labels.Lookup(u.AuthIndex)
	if !ok {
		cfg.Labels.Want(u.AuthIndex)
		return
	}
	switch {
	case email != "" && string(s.Source) == email:
		u.SourceClear, u.SourceKind = email, "email"
	case project != "" && string(s.Source) == project:
		u.SourceClear, u.SourceKind = project, "project"
	}
}
