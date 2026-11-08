package capture

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/stream"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/fingerprint"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/headers"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// MaskTimeout is the masking deadline for one split; a split past it is stored metadata-only
// (mask_timeout).
var MaskTimeout = 60 * time.Second

// errStopped: the split was cancelled by shutdown; nothing is written.
var errStopped = errors.New("capture: stopped")

// finalBody marshals a kind struct and masks every string and key in it (a defence-in-depth
// pass that is the identity on already-masked frames), so the server's detect re-check of the
// envelope body passes.
func finalBody(v any, m *mask.Masker) ([]byte, error) {
	raw, err := canon.Marshal(v)
	if err != nil {
		return nil, err
	}
	out, _, _, err := canon.CanonicalizeMasked(raw, m)
	return out, err
}

func maskCut(m *mask.Masker, s string, n int) string {
	if s == "" {
		return ""
	}
	out, _ := m.String(s)
	if len(out) <= n {
		return out
	}
	out = out[:n]
	for len(out) > 0 && !utf8.ValidString(out) {
		out = out[:len(out)-1]
	}
	return out
}

func (rt *Runtime) hmac(v string) string {
	if v == "" {
		return ""
	}
	return fingerprint.HMAC(rt.opts.HMACKeyID, rt.opts.HMACKey, v)
}

func headerGet(h http.Header, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

// splitResult is what a split contributes to an envelope.
type splitResult struct {
	sp      *canon.Split
	dropped string
}

// split runs one canon split with the body budget and the masking deadline. A nil sp with dropped set means
// metadata only; errStopped means the runtime is stopping.
func (rt *Runtime) split(ctx context.Context, in canon.Input, reserved int64, response bool) (splitResult, error) {
	if lvl := rt.pressure.Load(); lvl >= outbox.PressureGuard {
		if rt.pressureByCap.Load() {
			rt.stats().Drop("outbox_cap")
		} else {
			rt.stats().Drop("disk_guard")
		}
		return splitResult{dropped: wire.DroppedDiskGuard}, nil
	}
	ms := rt.opts.Masker.Get()
	workers := int64(max(rt.opts.Workers, 1))
	f := reserved + rt.opts.Engine.Pool.Free()
	opts := canon.Options{
		Masker:        ms.Masker,
		MaxAttachment: rt.limits().Attachment,
		Memo:          ms.Memo, // the memo view of the masker this split masks with
		MaxDecoded:    max(f/(4*workers), 1),
		MaxNodes:      int(max(f/(250*workers), 1)),
	}
	sctx, cancel := context.WithTimeout(ctx, MaskTimeout)
	defer cancel()
	start := time.Now()
	var sp *canon.Split
	var err error
	if response {
		sp, err = canon.SplitResponse(sctx, in, opts)
	} else {
		sp, err = canon.SplitRequest(sctx, in, opts)
	}
	rt.maskStats.observe(time.Since(start), int64(len(in.Body)), sp)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return splitResult{}, errStopped
	case errors.Is(err, context.DeadlineExceeded):
		rt.stats().Drop("mask_timeout")
		rt.maskStats.timeouts.Add(1)
		return splitResult{dropped: wire.DroppedMaskTimeout}, nil
	case errors.Is(err, canon.ErrBodyBudget):
		rt.stats().Drop("body_budget")
		return splitResult{dropped: wire.DroppedBodyBudget}, nil
	default:
		rt.stats().Drop("parse_error")
		return splitResult{dropped: wire.DroppedParseError}, nil
	}
	lim := rt.limits()
	for _, p := range sp.Pieces {
		if p.Kind != wire.PieceAttachment && int64(len(p.Bytes)) > lim.TextPiece {
			rt.stats().Drop("oversize")
			return splitResult{dropped: wire.DroppedOversize}, nil
		}
	}
	for i := 0; i < sp.Oversize; i++ {
		rt.stats().Drop("oversize")
	}
	return splitResult{sp: sp}, nil
}

func piecesFor(sp *canon.Split, refs []wire.Hash) []canon.Piece {
	want := make(map[wire.Hash]bool, len(refs))
	for _, h := range refs {
		want[h] = true
	}
	out := make([]canon.Piece, 0, len(refs))
	for _, p := range sp.Pieces {
		if want[p.Hash] {
			out = append(out, p)
		}
	}
	return out
}

func metaBody(format string, rawLen int64, dropped string) wire.Body {
	return wire.Body{Format: format, RawLen: rawLen, Dropped: dropped, ListSHA: canon.ListSHA(nil)}
}

// buildBegin builds exchange.begin for b. sr may be empty (metadata only).
func (rt *Runtime) buildBegin(b *hooks.Begin, sr splitResult, kept []wire.Header, hints wire.ArchiveHints) (outbox.NewEnvelope, *beginCommit, error) {
	req := b.Req
	cls := Classify(req.SourceFormat, req.Meta)
	callerFP := req.CallerScope
	if callerFP == "" {
		callerFP = req.Meta.CallerScope
	}
	eb := wire.ExchangeBegin{
		RequestID: req.RequestID, InputSeq: b.InputSeq, TraceID: req.TraceID, ExecSessionID: req.Meta.ExecutionSessionID,
		ObservedAt: b.ObservedAt.UTC(), SourceFormat: req.SourceFormat, RequestPath: req.Meta.RequestPath, Kind: cls.Kind,
		Transport: cls.Transport, RequestedModel: req.RequestedModel, Model: req.Model, Stream: req.Stream, CallerFP: callerFP,
		MetaSource: req.Meta.Source, ClientEffort: req.Meta.ReasoningEffort, ClientTier: req.Meta.ServiceTier,
		Generate: req.Meta.Generate, IdempotencyFP: rt.hmac(req.Meta.IdempotencyKey), Hints: hints, Headers: kept,
	}
	if eb.Headers == nil {
		eb.Headers = []wire.Header{}
	}
	format := CanonFormat(req.SourceFormat)
	var refs []wire.Hash
	var pieces []canon.Piece
	var commit *beginCommit
	if sr.sp == nil {
		dropped := sr.dropped
		if dropped == "metadata" {
			dropped = ""
		}
		eb.Body = metaBody(format, int64(req.BodyLen), dropped)
	} else {
		sp := sr.sp
		if t := topString(req.Body, "type"); strings.HasPrefix(t, "response.") && cls.Transport == "ws" {
			eb.InputType = t
		}
		ids := canon.ItemIDs(sp.Items)
		ckey := ConvKey(callerFP, req.SourceFormat, req.Meta.ExecutionSessionID, req.Headers, ids)
		prev := ""
		if cls.Transport == "ws" {
			prev = topString(req.Body, "previous_response_id")
		}
		dropped := rt.stats().DroppedTotal()
		d := rt.prefix.Choose(ckey, ids, req.Meta.ExecutionSessionID, prev, dropped)
		body := sp.Body()
		body.Base = d.Base
		body.Items = sp.Items[d.From:]
		body.N = uint32(len(d.Expanded))
		body.ListSHA = canon.ListSHA(d.Expanded)
		if cls.Legacy {
			body.Flags = append(body.Flags, wire.FlagLegacyCompletions)
		}
		eb.Body = body
		refs = sp.Refs(d.From)
		pieces = piecesFor(sp, refs)
		commit = &beginCommit{conv: ckey, ids: d.Expanded, dropped: dropped}
	}
	ms := rt.opts.Masker.Get()
	raw, err := finalBody(eb, ms.Masker)
	if err != nil {
		rt.stats().Drop("encode_error")
		return outbox.NewEnvelope{}, nil, err
	}
	key := wire.ExchangeBeginKey(req.RequestID, b.InputSeq)
	if commit != nil {
		commit.key = key
	}
	return outbox.NewEnvelope{Kind: wire.KindExchangeBegin, Key: key, CreatedAt: b.ObservedAt, ExactRev: ms.Rev, Refs: refs, Body: raw, Pieces: pieces}, commit, nil
}

type beginCommit struct {
	conv    wire.Hash
	key     string
	ids     []wire.Hash
	dropped uint64
}

// topString returns a top-level string field of a JSON object body without parsing the rest.
func topString(body []byte, key string) string {
	var out string
	_ = protocol.Fields(body, func(k, v []byte) error {
		if string(k) == key && len(v) > 0 && v[0] == '"' && len(v) <= 1024 {
			out, _ = protocol.String(v)
		}
		return nil
	})
	return out
}

// nonStreamStatus is the terminal status of a non-stream response. CPA calls the response hook
// only after a successful non-stream execution, with the whole client-facing body, so the
// response completed. A Responses object's own status refines that, as the terminal event does
// for a stream: incomplete and failed as sent, cancelled as canceled, and no terminal status
// for a background response that has not finished (queued, in_progress). A body that is not a
// readable JSON object (metadata only, encoded) leaves completed.
func nonStreamStatus(format string, body []byte) string {
	if format != canon.FormatOpenAIResponses {
		return stream.StatusCompleted
	}
	switch topString(body, "status") {
	case "incomplete":
		return stream.StatusIncomplete
	case "failed":
		return stream.StatusFailed
	case "cancelled", "canceled":
		return stream.StatusCanceled
	case "queued", "in_progress":
		return ""
	}
	return stream.StatusCompleted
}

// buildResponse builds exchange.response for r.
func (rt *Runtime) buildResponse(r *hooks.Response, sr splitResult) (outbox.NewEnvelope, *respCommit, []canon.MediaURL, error) {
	ms := rt.opts.Masker.Get()
	er := wire.ExchangeResponse{
		RequestID: r.RequestID, Index: r.Index, ResponseID: r.ResponseID, PrevResponseID: r.PrevResponseID,
		ObservedAt: r.ObservedAt.UTC(), StatusCode: r.StatusCode, Stream: r.Stream, TerminalStatus: r.TerminalStatus,
		IncompleteReason: r.IncompleteReason, Chunks: r.Chunks, StreamBytes: r.StreamBytes, Truncated: r.Truncated,
		BootstrapResets: r.BootstrapResets, Events: r.Events, Headers: headers.FilterResponse(r.Headers, ms.Masker),
	}
	if er.Headers == nil {
		er.Headers = []wire.Header{}
	}
	for _, t := range []struct {
		src time.Time
		dst **time.Time
	}{{r.HeaderInitAt, &er.HeaderInitAt}, {r.FirstChunkAt, &er.FirstChunkAt}, {r.TerminalAt, &er.TerminalAt}} {
		if !t.src.IsZero() {
			u := t.src.UTC()
			*t.dst = &u
		}
	}
	format := CanonFormat(r.SourceFormat)
	if !r.Stream {
		er.TerminalStatus = nonStreamStatus(format, r.Body)
	}
	var refs []wire.Hash
	var pieces []canon.Piece
	var urls []canon.MediaURL
	var commit *respCommit
	if sr.sp == nil {
		dropped := sr.dropped
		if dropped == "metadata" {
			dropped = ""
		}
		er.Body = metaBody(format, max(int64(len(r.Body)), r.RawLen), dropped)
	} else {
		sp := sr.sp
		body := sp.Body()
		body.Flags = append(body.Flags, r.Flags...)
		er.Body = body
		if er.ResponseID == "" && !r.Stream && r.ContentEncoding == "" {
			er.ResponseID = topString(r.Body, "id")
			if er.ResponseID == "" {
				er.ResponseID = topString(r.Body, "responseId")
			}
			er.PrevResponseID = topString(r.Body, "previous_response_id")
		}
		refs = sp.Refs(0)
		pieces = piecesFor(sp, refs)
		urls = sp.MediaURLs
		commit = &respCommit{ids: canon.ItemIDs(sp.Items), responseID: er.ResponseID, execSession: r.Meta.ExecutionSessionID}
	}
	if er.ResponseID != "" {
		er.ResponseID = maskCut(ms.Masker, er.ResponseID, 256)
	}
	raw, err := finalBody(er, ms.Masker)
	if err != nil {
		rt.stats().Drop("encode_error")
		return outbox.NewEnvelope{}, nil, nil, err
	}
	key := wire.ExchangeResponseKey(r.RequestID, r.Index)
	if commit != nil {
		commit.beginKey = wire.ExchangeBeginKey(r.RequestID, r.Index)
		commit.key = key
		commit.index = r.Index
	}
	return outbox.NewEnvelope{Kind: wire.KindExchangeResponse, Key: key, CreatedAt: r.ObservedAt, ExactRev: ms.Rev, Refs: refs, Body: raw, Pieces: pieces}, commit, urls, nil
}

type respCommit struct {
	beginKey, key, responseID, execSession string
	index                                  uint32
	ids                                    []wire.Hash
}

// buildAttempt builds exchange.attempt.
func (rt *Runtime) buildAttempt(a *hooks.Attempt) (outbox.NewEnvelope, error) {
	ms := rt.opts.Masker.Get()
	m := a.Req.Meta
	ea := wire.ExchangeAttempt{
		RequestID: a.Req.RequestID, Ordinal: a.Ordinal, ObservedAt: a.ObservedAt.UTC(), AuthIndex: m.SelectedAuthIndex,
		AuthFP: rt.hmac(m.SelectedAuthID), ToFormat: a.Req.ToFormat, Model: a.Req.Model, CanonicalSessionID: m.CanonicalSessionID,
		ParentSessionID: m.ParentSessionID, SelectionID: m.SelectionID, NodeKind: m.NodeKind, IsFork: m.IsFork, IsCompaction: m.IsCompaction,
	}
	raw, err := finalBody(ea, ms.Masker)
	if err != nil {
		rt.stats().Drop("encode_error")
		return outbox.NewEnvelope{}, err
	}
	return outbox.NewEnvelope{Kind: wire.KindExchangeAttempt, Key: wire.ExchangeAttemptKey(a.Req.RequestID, a.Ordinal), CreatedAt: a.ObservedAt, ExactRev: ms.Rev, Body: raw}, nil
}

// buildCompletion builds exchange.completion.
func (rt *Runtime) buildCompletion(d *hooks.Done, cs hooks.CompletionState) (outbox.NewEnvelope, error) {
	ms := rt.opts.Masker.Get()
	c := d.Req
	ec := wire.ExchangeCompletion{
		RequestID: c.RequestID, TraceID: c.TraceID, SourceFormat: c.SourceFormat, Model: c.Model, RequestedModel: c.RequestedModel,
		Stream: c.Stream, Outcome: c.Outcome, StatusCode: c.StatusCode, Error: maskCut(ms.Masker, c.Error, 4096),
		StartedAt: c.StartedAt.UTC(), CompletedAt: c.CompletedAt.UTC(), BeginWritten: cs.BeginWritten, Responses: cs.Responses,
		HookMicros: cs.HookMicros,
	}
	raw, err := finalBody(ec, ms.Masker)
	if err != nil {
		rt.stats().Drop("encode_error")
		return outbox.NewEnvelope{}, err
	}
	return outbox.NewEnvelope{Kind: wire.KindExchangeDone, Key: wire.ExchangeCompletionKey(c.RequestID), CreatedAt: d.ObservedAt, ExactRev: ms.Rev, Body: raw}, nil
}

// buildUsage builds usage.attempt; Failure.Body becomes a masked text piece.
func (rt *Runtime) buildUsage(ctx context.Context, u *hooks.UsageItem) (outbox.NewEnvelope, error) {
	ms := rt.opts.Masker.Get()
	r := u.Rec
	ua := wire.UsageAttempt{
		UsageRequestID: r.RequestID, TraceID: r.TraceID, SelectionID: r.SelectionID, Provider: r.Provider,
		BaseURL: headers.StripURL(r.BaseURL, ms.Masker), ExecutorType: r.ExecutorType, Model: r.Model, Alias: r.Alias,
		CallerFP: r.CallerFP, IsNativeKey: r.IsNativeKey, AccessProvider: r.AccessProvider, SessionID: r.SessionID,
		ParentSessionID: r.ParentSessionID, AuthIndex: r.AuthIndex, AuthFP: r.AuthFP, AuthType: r.AuthType,
		AccountFP: r.AccountFP, SourceClear: r.SourceClear, SourceKind: r.SourceKind, ReasoningEffort: r.ReasoningEffort,
		ServiceTier: r.ServiceTier, EffectiveServiceTier: r.EffectiveServiceTier, ResponseServiceTier: r.ResponseServiceTier,
		ResponseModel: r.ResponseModel, Generate: r.Generate, Stream: r.Stream, RequestedAt: r.RequestedAt.UTC(),
		LatencyMS: r.Latency.Milliseconds(), TTFTMS: r.TTFT.Milliseconds(), Failed: r.Failed, FailureStatus: r.FailureStatus,
		Tokens: wire.Tokens{Input: r.Tokens.InputTokens, Output: r.Tokens.OutputTokens, Reasoning: r.Tokens.ReasoningTokens,
			Cached: r.Tokens.CachedTokens, CacheRead: r.Tokens.CacheReadTokens, CacheCreation: r.Tokens.CacheCreationTokens, Total: r.Tokens.TotalTokens},
		ResponseHeaders: headers.FilterResponse(r.ResponseHeaders, ms.Masker),
	}
	if rt.opts.Labels != nil {
		ua.AccountLabel = rt.opts.Labels.Label(r.AuthIndex, r.AccountFP, r.Provider, r.BaseURL, r.AuthType)
	}
	var refs []wire.Hash
	var pieces []canon.Piece
	if r.FailureBody != "" {
		sctx, cancel := context.WithTimeout(ctx, MaskTimeout)
		masked, _, err := ms.Masker.StringContext(sctx, r.FailureBody)
		cancel()
		if err == nil {
			p := canon.NewPiece(wire.PieceText, "", []byte(masked))
			ua.FailureBody = &p.Hash
			refs, pieces = []wire.Hash{p.Hash}, []canon.Piece{p}
		} else if ctx.Err() != nil {
			return outbox.NewEnvelope{}, errStopped
		} else {
			rt.stats().Drop("mask_timeout")
		}
	}
	raw, err := finalBody(ua, ms.Masker)
	if err != nil {
		rt.stats().Drop("encode_error")
		return outbox.NewEnvelope{}, err
	}
	at := u.ObservedAt
	return outbox.NewEnvelope{Kind: wire.KindUsage, Key: wire.UsageKey(r.RequestID), CreatedAt: at, ExactRev: ms.Rev, Refs: refs, Body: raw, Pieces: pieces}, nil
}
