package capture

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/safego"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/stream"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/headers"
)

// segment is one stream attempt of a RequestID: what the hooks sent from one header-init (or the
// first chunk) up to the next header-init. A RequestID that CPA reuses for an in-place WebSocket
// failover can have one segment per lifecycle, and a bootstrap retry gives a segment per attempt.
type segment struct {
	format       string
	model        string
	meta         protocol.Meta
	asm          *stream.Assembler
	re           stream.Reassembler
	headers      http.Header
	hdr          *hooks.HeaderCharge // headers' pool charge, taken at the first handoff, shared with handed-off responses
	headerInitAt time.Time
	firstChunkAt time.Time
	chunks       uint32
	bytes        int64
	state        int64 // stream-pool bytes held for assembled state
	resets       uint32
	ended        bool // the last response handed off ended with a completed, done or incomplete event
	// cut: the next response handed off lost part of its stream, so it is marked truncated. Set
	// when this runtime picks a stream up after its first payload chunk (the start went to an
	// earlier runtime or plugin instance), and when a stop writes a stream that is still open.
	cut bool

	t          time.Time        // start: the first item's hook time (header-init, else first chunk)
	ev         hooks.StreamItem // boundary evidence of the first item (BAAt, BAPrevAt, Since)
	lastHookAt time.Time        // latest hook time of its items
	kept       bool             // a completion classified it as another lifecycle's
}

func (s *segment) empty() bool {
	return s == nil || (s.chunks == 0 && s.state == 0 && s.headerInitAt.IsZero())
}

// acc is one RequestID's stream accumulator, owned by the stream goroutine: the open segment and
// the segments parked at a later header-init until their completion claims them.
type acc struct {
	cur     *segment
	parked  []*segment
	resets  uint32
	touched time.Time
}

// segments returns the parked segments and the open one, oldest first. The open segment is
// included while it is empty too (every response it carried was handed off): its ended flag still
// decides a canceled completion (flushDone).
func (a *acc) segments() []*segment {
	out := append([]*segment(nil), a.parked...)
	if a.cur != nil && a.cur.re != nil {
		out = append(out, a.cur)
	}
	return out
}

// held is the number of segments that still hold content.
func (a *acc) held() int {
	n := len(a.parked)
	if !a.cur.empty() {
		n++
	}
	return n
}

func (a *acc) remove(s *segment) {
	if a.cur == s {
		a.cur = nil
		return
	}
	if i := slices.Index(a.parked, s); i >= 0 {
		a.parked = slices.Delete(a.parked, i, i+1)
	}
}

// pendDone is a completion the sequencer holds: its stream items are not all processed yet, or
// which segments are its own is not settled yet.
type pendDone struct {
	d       *hooks.Done
	readyAt time.Time // when its stream items were all processed (OwnerSettle counts from it)
	held    bool      // counted ownership_held
}

// seqState is the stream goroutine's state. Pending completions belong to the RequestID, not to a
// segment, so no flush or reset drops one; starts holds the started_at (unix ns) of every completion
// read for the RequestID, which tells lifecycles apart (F2).
type seqState struct {
	accs   map[string]*acc
	pend   map[string][]*pendDone
	starts map[string][]int64
}

func newSeqState() *seqState {
	return &seqState{accs: map[string]*acc{}, pend: map[string][]*pendDone{}, starts: map[string][]int64{}}
}

// IncompleteStopped is the incomplete_reason of a stream response a stop wrote while it was
// still open; the rest of the stream reaches the next runtime or plugin instance, or is lost.
const IncompleteStopped = "plugin_stopped"

// SweepInterval is how often the stream goroutine sweeps idle accumulators and settles held
// completions.
var SweepInterval = time.Minute

// OwnerSettle is how long a completion whose candidate segments' owner is uncertain is held for
// causal evidence (a later hook after its CompletedAt, or another completion's started_at) before it
// is settled without it: every candidate is then written, none released.
var OwnerSettle = time.Minute

// sequencer is the single stream goroutine: stream chunks (one at a time), attempts,
// metadata-only begins and responses, and completions, in channel order.
func (rt *Runtime) sequencer(ctx context.Context) {
	e := rt.opts.Engine
	sq := newSeqState()
	defer func() {
		// Shutdown or a recovered panic: open streams are unfinished bodies (counted stopped,
		// never written raw). A stream that continues after a restart lost this state, so its
		// later responses are marked truncated.
		for rid, a := range sq.accs {
			e.Stats.Drop("stopped")
			rt.retireAcc(rid, a)
			e.With(rid, false, func(en *hooks.Entry) { en.Truncated = true })
		}
		rt.activeStreams.Store(0)
	}()
	tick := time.NewTicker(SweepInterval)
	defer tick.Stop()
	for {
		safego.Checkpoint("stream")
		select {
		case <-ctx.Done():
			rt.finishStreams(sq)
			return
		case it := <-e.Stream:
			rt.onStream(sq, it)
		case c := <-e.Control:
			rt.onControl(sq, c)
		case <-tick.C:
			for rid := range sq.pend {
				rt.evalPending(sq, rid, false)
			}
			rt.sweepAccs(sq, e.Now().Add(-hooks.IdleAfter))
		}
		rt.activeStreams.Store(int64(len(sq.accs)))
	}
}

// finishStreams runs at a stop, once admission closed (so the queues only shrink) and while the
// workers still run. It processes the stream and control items the hooks queued, settles every
// held completion, then writes each segment still open or kept as a partial response: incomplete
// (IncompleteStopped) and truncated, instead of discarding what it captured. An exchange's
// completion not yet received is not written here: CPA sends it later, to this instance's next
// runtime or to the next plugin instance. Past the drain deadline it stops, and the deferred
// cleanup counts what is left stopped.
func (rt *Runtime) finishStreams(sq *seqState) {
	e := rt.opts.Engine
queued:
	for rt.drainCtx.Err() == nil {
		select {
		case it := <-e.Stream:
			rt.onStream(sq, it)
		case c := <-e.Control:
			rt.onControl(sq, c)
		default:
			break queued
		}
	}
	for rid := range sq.pend {
		if rt.drainCtx.Err() != nil {
			return
		}
		// Every queued chunk was processed: nothing more is coming for these completions.
		rt.evalPending(sq, rid, true)
	}
	for rid, a := range sq.accs {
		if rt.drainCtx.Err() != nil {
			return
		}
		for _, s := range a.segments() {
			rt.writeOpen(rid, a, s)
		}
		rt.retireAcc(rid, a)
		delete(sq.accs, rid)
	}
	rt.activeStreams.Store(int64(len(sq.accs)))
}

// writeOpen writes a segment whose completion has not come as a partial response: incomplete
// (IncompleteStopped) and truncated.
func (rt *Runtime) writeOpen(rid string, a *acc, s *segment) {
	if s.re != nil {
		at := rt.opts.Engine.Now()
		for _, ev := range s.asm.Finish() {
			for _, r := range s.re.Feed(ev, at) {
				rt.handOff(rid, a, s, r)
			}
		}
		for _, r := range s.re.Flush(stream.StatusIncomplete, at) {
			if r.TerminalStatus == stream.StatusIncomplete && r.IncompleteReason == "" {
				r.IncompleteReason = IncompleteStopped
			}
			// Chat and Gemini report completed once a finish reason arrived: the body is whole.
			s.cut = s.cut || r.TerminalStatus != stream.StatusCompleted
			rt.handOff(rid, a, s, r)
		}
	}
	rt.retireSeg(rid, s)
	a.remove(s)
}

// releaseSeg returns a segment's assembled stream state (at each handoff, and when it is retired).
func (rt *Runtime) releaseSeg(rid string, s *segment) {
	e := rt.opts.Engine
	if s != nil && s.state > 0 {
		e.Pool.ReleaseStream(s.state)
		rt.streamAccum.Add(-s.state)
		st := s.state
		e.With(rid, false, func(en *hooks.Entry) { en.StreamBytes -= st })
		s.state = 0
	}
}

// retireSeg is a segment's last use: its stream state is returned and it drops its reference to the
// header charge, so the headers stay charged while any response handed off from it still holds them.
func (rt *Runtime) retireSeg(rid string, s *segment) {
	if s == nil {
		return
	}
	rt.releaseSeg(rid, s)
	s.hdr.Unref()
	s.hdr, s.headers = nil, nil
}

// retireAcc retires every segment of an accumulator that is being dropped, the open one included
// even when it has no reassembler (segments omits it then).
func (rt *Runtime) retireAcc(rid string, a *acc) {
	for _, s := range a.parked {
		rt.retireSeg(rid, s)
	}
	rt.retireSeg(rid, a.cur)
	a.parked, a.cur = nil, nil
}

// sweepAccs drops accumulators idle since before cutoff, with their pending completions. A segment
// a completion kept for another lifecycle whose completion never came is written as a partial
// response first, not dropped.
func (rt *Runtime) sweepAccs(sq *seqState, cutoff time.Time) {
	for rid, a := range sq.accs {
		if !a.touched.Before(cutoff) {
			continue
		}
		for _, s := range a.segments() {
			if s.kept || slices.Contains(a.parked, s) {
				s.cut = true
				rt.writeOpen(rid, a, s)
			}
		}
		rt.retireAcc(rid, a)
		delete(sq.accs, rid)
		delete(sq.pend, rid)
		delete(sq.starts, rid)
	}
	for rid, list := range sq.pend {
		if sq.accs[rid] != nil {
			continue
		}
		idle := true
		for _, p := range list {
			idle = idle && p.d.ObservedAt.Before(cutoff)
		}
		if idle {
			delete(sq.pend, rid)
			delete(sq.starts, rid)
		}
	}
}

func (rt *Runtime) newSegment(a *acc, it hooks.StreamItem) *segment {
	s := &segment{format: it.SourceFormat, model: it.Model, meta: it.Meta, asm: &stream.Assembler{}, re: stream.New(it.SourceFormat),
		resets: a.resets, t: it.At, ev: it,
		// CPA sends the header-init first and numbers payload chunks from 0: a later chunk here
		// means the stream started before this runtime (a restart or a plugin update).
		cut: it.ChunkIndex > 0}
	s.ev.Body, s.ev.Headers = nil, nil
	return s
}

// sameLifecycle reports whether segment s provably belongs to the lifecycle of header-init h: no
// before-auth of the RequestID started between s's start and h, and h's record is continuous over
// that span. Every lifecycle opens with a before-auth (F6), so there is no new lifecycle between.
func sameLifecycle(h hooks.StreamItem, s *segment) bool {
	t := s.t.UnixNano()
	return h.BAAt < t && h.Since < t
}

func (rt *Runtime) onStream(sq *seqState, it hooks.StreamItem) {
	e := rt.opts.Engine
	e.ChunkDone(it.RequestID, false)
	a := sq.accs[it.RequestID]
	if a == nil {
		a = &acc{}
		sq.accs[it.RequestID] = a
	}
	a.touched = it.At
	if it.ChunkIndex == protocol.StreamHeaderInit {
		if s := a.cur; !s.empty() {
			a.resets++
			if sameLifecycle(it, s) {
				// Bootstrap retry: CPA ran the stream again and sent a second header-init for the
				// same request. Discard what the previous attempt buffered.
				rt.retireSeg(it.RequestID, s)
			} else {
				// A before-auth came between, or the record is not continuous: the segment may be
				// an earlier lifecycle's. Keep it for the completion that claims it.
				a.parked = append(a.parked, s)
				e.Stats.Count("segments_parked", 1)
			}
			a.cur = nil
		}
		if a.cur.empty() {
			a.cur = rt.newSegment(a, it)
		}
		s := a.cur
		s.headers, s.headerInitAt = it.Headers, it.At
		if it.SourceFormat != "" {
			s.format = it.SourceFormat
		}
	} else {
		if a.cur == nil {
			a.cur = rt.newSegment(a, it)
		}
		s := a.cur
		if s.firstChunkAt.IsZero() {
			s.firstChunkAt = it.At
		}
		s.chunks++
		s.bytes += int64(len(it.Body))
		s.state += it.Reserved // the raw chunk's reservation becomes assembled state
		rt.streamAccum.Add(it.Reserved)
		for _, ev := range s.asm.Add(it.Body) {
			for _, r := range s.re.Feed(ev, it.At) {
				rt.handOff(it.RequestID, a, s, r)
			}
		}
	}
	if it.At.After(a.cur.lastHookAt) {
		a.cur.lastHookAt = it.At
	}
	if len(sq.pend[it.RequestID]) > 0 {
		rt.evalPending(sq, it.RequestID, false)
	}
}

func (rt *Runtime) onControl(sq *seqState, c hooks.Control) {
	e := rt.opts.Engine
	switch {
	case c.Attempt != nil:
		if env, err := rt.buildAttempt(c.Attempt); err == nil {
			rt.write(env, nil, nil, 0, 0)
		}
	case c.Begin != nil:
		ms := rt.opts.Masker.Get()
		kept, hints, _ := headers.FilterRequest(c.Begin.Req.Headers, ms.Masker, rt.opts.HMACKeyID, rt.opts.HMACKey)
		if env, _, err := rt.buildBegin(c.Begin, splitResult{dropped: c.Begin.MetaOnly}, kept, hints); err == nil {
			if rt.write(env, nil, nil, 0, 0) {
				e.NoteWritten(c.Begin.Req.RequestID, true)
			}
		}
	case c.Response != nil:
		if env, _, _, err := rt.buildResponse(c.Response, splitResult{dropped: c.Response.MetaOnly}); err == nil {
			if rt.write(env, nil, nil, 0, 0) {
				e.NoteWritten(c.Response.RequestID, false)
			}
		}
	case c.Done != nil:
		d := c.Done
		rid := d.Req.RequestID
		if !d.Req.StartedAt.IsZero() {
			if sn := d.Req.StartedAt.UnixNano(); !slices.Contains(sq.starts[rid], sn) {
				sq.starts[rid] = append(sq.starts[rid], sn)
			}
		}
		// Ascending snapshot order, ties in arrival order: the snapshot counts the stream items
		// queued before the completion's hook, so the order is causal.
		list := sq.pend[rid]
		i := len(list)
		for i > 0 && list[i-1].d.ChunksQueued > d.ChunksQueued {
			i--
		}
		sq.pend[rid] = slices.Insert(list, i, &pendDone{d: d})
		rt.evalPending(sq, rid, false)
	}
}

// evalPending flushes every pending completion of rid whose stream items were all processed and
// whose segments' ownership is settled. With stop set (a stop's drain), every one is flushed now,
// settled without evidence where it is uncertain.
func (rt *Runtime) evalPending(sq *seqState, rid string, stop bool) {
	e := rt.opts.Engine
	for {
		progressed := false
		for i, p := range sq.pend[rid] {
			if !stop && p.d.ChunksQueued > 0 && !e.ChunksProcessed(rid, p.d.ChunksQueued) {
				continue
			}
			now := e.Now()
			if p.readyAt.IsZero() {
				p.readyAt = now
			}
			cl := rt.classify(sq, rid, p.d)
			settle := stop || now.Sub(p.readyAt) >= OwnerSettle
			if !cl.certain && !settle {
				if !p.held {
					p.held = true
					e.Stats.Count("ownership_held", 1)
				}
				continue
			}
			sq.pend[rid] = slices.Delete(sq.pend[rid], i, i+1)
			rt.flushDone(sq, rid, p.d, cl)
			progressed = true
			break
		}
		if !progressed {
			break
		}
	}
	if len(sq.pend[rid]) == 0 {
		delete(sq.pend, rid)
		if sq.accs[rid] == nil {
			delete(sq.starts, rid)
		}
	}
}

// claim is which segments a completion flushes.
type claim struct {
	cands   []*segment // oldest first
	certain bool       // ownership settled
	release bool       // older candidates are bootstrap retries of a succeeded lifecycle: release them
}

// classify sorts rid's segments for completion d (S = started_at, C = completed_at): a segment that
// started before S is an earlier lifecycle's (F2); one with a hook after C is a later lifecycle's
// (F1); one that started after another completion's started_at S' > S is a later lifecycle's (F2).
// The rest are candidates. A candidate is provably d's when its first item shows at most one
// before-auth after S (d's own opener) over a continuous record.
func (rt *Runtime) classify(sq *seqState, rid string, d *hooks.Done) claim {
	var cl claim
	a := sq.accs[rid]
	if a == nil {
		cl.certain = true
		return cl
	}
	var sn, cn int64
	if !d.Req.StartedAt.IsZero() {
		sn = d.Req.StartedAt.UnixNano()
	}
	if !d.Req.CompletedAt.IsZero() {
		cn = d.Req.CompletedAt.UnixNano()
	}
	provable, content := sn != 0, false
	for _, s := range a.segments() {
		t := s.t.UnixNano()
		later := cn != 0 && (t > cn || s.lastHookAt.UnixNano() > cn)
		if sn != 0 && !later {
			for _, o := range sq.starts[rid] {
				later = later || (o > sn && t > o)
			}
		}
		if (sn != 0 && t < sn) || later {
			s.kept = true
			continue
		}
		cl.cands = append(cl.cands, s)
		if !s.empty() {
			// An empty segment holds nothing to misattribute: it never makes ownership uncertain.
			provable = provable && s.ev.BAPrevAt < sn && s.ev.Since < sn
			content = true
		}
	}
	switch {
	case !content, provable:
		cl.certain = true
	case sn == 0:
		// No started_at: nothing can be told apart, and nothing is released.
		cl.certain = true
	case d.Req.Outcome == "succeeded":
		// No later lifecycle of the RequestID can exist (F7): the older candidates are bootstrap
		// retries, released as 0.1.4 did.
		cl.certain, cl.release = true, true
	}
	return cl
}

// flushDone flushes completion d's claimed segments and writes the completion. The newest candidate
// is flushed with d's status at the response index 0.1.4 would have given it; older candidates are
// released (succeeded) or written as their own partial responses. Then the entry learns how many
// segments are still held, and the completion is taken.
func (rt *Runtime) flushDone(sq *seqState, rid string, d *hooks.Done, cl claim) {
	e := rt.opts.Engine
	a := sq.accs[rid]
	status := stream.StatusCompleted
	switch d.Req.Outcome {
	case "failed":
		status = stream.StatusFailed
	case "canceled":
		status = stream.StatusCanceled
	}
	if n := len(cl.cands); n > 0 {
		if !cl.certain {
			e.Stats.Count("segments_uncertain", 1)
		}
		// Candidates are written newest first; with cl.release the older ones are dropped.
		newest := cl.cands[n-1]
		open := rt.flushSeg(rid, a, newest, status, d.ObservedAt)
		if d.Req.Outcome == "canceled" && len(open) == 0 && newest.ended {
			// Every response this stream carried had ended before the cancel. CPA v8.0.60 cancels
			// a Responses WebSocket turn when the client closes the socket after the turn's last
			// event but before CPA reads the upstream close (and always for a duplex RequestID,
			// which ends only with the socket); it does not track WebSocket delivery. Record what
			// CPA reports for an HTTP Responses stream whose delivery was acknowledged.
			d.Req.Outcome, d.Req.StatusCode, d.Req.Error = "succeeded", http.StatusOK, ""
		}
		for _, r := range open {
			rt.handOff(rid, a, newest, r)
		}
		rt.retireSeg(rid, newest)
		a.remove(newest)
		for _, s := range cl.cands[:n-1] {
			if !cl.release {
				for _, r := range rt.flushSeg(rid, a, s, status, d.ObservedAt) {
					rt.handOff(rid, a, s, r)
				}
			}
			rt.retireSeg(rid, s)
			a.remove(s)
		}
	}
	held := 0
	if a != nil {
		if held = a.held(); held == 0 {
			rt.retireAcc(rid, a)
			delete(sq.accs, rid)
		}
	}
	e.SetHeld(rid, uint32(held))
	if cs, ready := e.TakeCompletion(d); ready {
		rt.writeCompletion(d, cs)
	}
}

// flushSeg finishes a segment's assembly with status and returns the responses still open.
func (rt *Runtime) flushSeg(rid string, a *acc, s *segment, status string, at time.Time) []stream.Response {
	if s.re == nil {
		return nil
	}
	for _, ev := range s.asm.Finish() {
		for _, r := range s.re.Feed(ev, at) {
			rt.handOff(rid, a, s, r)
		}
	}
	return s.re.Flush(status, at)
}

// handOff converts a reassembled response's stream reservation into 4 × its size and hands it
// to a worker; on budget or a full queue it writes metadata only. The segment's headers are charged
// once, at its first handoff, and the charge is shared (HeaderCharge): the segment keeps its
// reference until it is retired and the response holds its own, so a worker's trim cannot return
// the bytes of a header map the segment still holds, and a later response is not charged again.
func (rt *Runtime) handOff(rid string, a *acc, s *segment, sr stream.Response) {
	e := rt.opts.Engine
	rt.releaseSeg(rid, s)
	switch sr.TerminalStatus {
	case stream.StatusCompleted, stream.StatusDone, stream.StatusIncomplete:
		s.ended = !sr.TerminalAt.IsZero() // zero when Flush closed it, not a terminal event
	default:
		s.ended = false
	}
	r := &hooks.Response{
		RequestID: rid, SourceFormat: s.format, Model: s.model, StatusCode: 200, Stream: true, Headers: s.headers,
		Body: sr.Body, ContentType: "application/json", ObservedAt: e.Now(), Meta: s.meta,
		ResponseID: sr.ResponseID, PrevResponseID: sr.PrevResponseID, HeaderInitAt: s.headerInitAt, FirstChunkAt: s.firstChunkAt,
		TerminalAt: sr.TerminalAt, TerminalStatus: sr.TerminalStatus, IncompleteReason: sr.IncompleteReason,
		Chunks: s.chunks, StreamBytes: s.bytes, BootstrapResets: s.resets, Events: sr.Events, Flags: sr.Flags,
	}
	e.With(rid, true, func(en *hooks.Entry) {
		r.Index = en.NextResponse
		en.NextResponse++
		r.Truncated = en.Truncated || s.cut
	})
	s.cut = false
	s.chunks, s.bytes, s.firstChunkAt = 0, 0, time.Time{}
	if CanonFormat(s.format) == canon.FormatOpaque {
		r.ContentType = ""
	}
	need := int64(len(r.Body)) * hooks.BodyReserveFactor
	var hb int64
	if s.hdr == nil {
		hb = hooks.HandoffHeaderBytes(s.headers)
	}
	if !e.Pool.TryReserve(need + hb) {
		e.Stats.Drop("body_budget")
		rt.metaResponse(r, "body_budget")
		return
	}
	if hb > 0 {
		s.hdr = hooks.NewHeaderCharge(e.Pool, hb)
	}
	r.Reserved, r.HeaderCharge = need, s.hdr.Ref()
	e.With(rid, false, func(en *hooks.Entry) { en.Pending++ })
	select {
	case e.Handoff <- hooks.Work{Response: r}:
	default:
		e.With(rid, false, func(en *hooks.Entry) { en.Pending-- })
		e.Pool.Release(need)
		r.HeaderCharge.Unref() // the segment keeps its own reference
		r.Reserved, r.HeaderCharge = 0, nil
		e.Stats.Drop("handoff_full")
		rt.metaResponse(r, "body_budget")
	}
}

func (rt *Runtime) metaResponse(r *hooks.Response, reason string) {
	r.RawLen = int64(len(r.Body))
	r.Body = nil
	env, _, _, err := rt.buildResponse(r, splitResult{dropped: reason})
	if err != nil {
		return
	}
	if rt.write(env, nil, nil, 0, 0) {
		rt.opts.Engine.NoteWritten(r.RequestID, false)
	}
}
