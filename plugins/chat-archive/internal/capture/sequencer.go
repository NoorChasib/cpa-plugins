package capture

import (
	"context"
	"net/http"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/safego"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/stream"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/headers"
)

// acc is one RequestID's stream accumulator, owned by the stream goroutine.
type acc struct {
	format       string
	model        string
	meta         protocol.Meta
	asm          *stream.Assembler
	re           stream.Reassembler
	headers      http.Header
	headerInitAt time.Time
	firstChunkAt time.Time
	chunks       uint32
	bytes        int64
	resets       uint32
	state        int64 // stream-pool bytes held for assembled state
	pending      *hooks.Done
	touched      time.Time
	ended        bool // the last response handed off ended with a completed, done or incomplete event
	// cut: the next response handed off lost part of its stream, so it is marked truncated. Set
	// when this runtime picks a stream up after its first payload chunk (the start went to an
	// earlier runtime or plugin instance), and when a stop writes a stream that is still open.
	cut bool
}

// IncompleteStopped is the incomplete_reason of a stream response a stop wrote while it was
// still open; the rest of the stream reaches the next runtime or plugin instance, or is lost.
const IncompleteStopped = "plugin_stopped"

// SweepInterval is how often the stream goroutine sweeps idle accumulators.
var SweepInterval = time.Minute

// sequencer is the single stream goroutine: stream chunks (one at a time), attempts,
// metadata-only begins and responses, and completions, in channel order.
func (rt *Runtime) sequencer(ctx context.Context) {
	e := rt.opts.Engine
	accs := map[string]*acc{}
	defer func() {
		// Shutdown or a recovered panic: open streams are unfinished bodies (counted stopped,
		// never written raw). A stream that continues after a restart lost this state, so its
		// later responses are marked truncated.
		for rid, a := range accs {
			e.Stats.Drop("stopped")
			rt.releaseAcc(rid, a)
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
			rt.finishStreams(accs)
			return
		case it := <-e.Stream:
			rt.onStream(accs, it)
		case c := <-e.Control:
			rt.onControl(accs, c)
		case <-tick.C:
			rt.sweepAccs(accs, e.Now().Add(-hooks.IdleAfter))
		}
		rt.activeStreams.Store(int64(len(accs)))
	}
}

// finishStreams runs at a stop, once admission closed (so the queues only shrink) and while the
// workers still run. It processes the stream and control items the hooks queued, then writes
// each stream still open as a partial response: incomplete (IncompleteStopped) and truncated,
// instead of discarding what it captured. The exchange's completion is not written here: CPA
// sends it later, to this instance's next runtime or to the next plugin instance. Past the drain
// deadline it stops, and the deferred cleanup counts what is left stopped.
func (rt *Runtime) finishStreams(accs map[string]*acc) {
	e := rt.opts.Engine
queued:
	for rt.drainCtx.Err() == nil {
		select {
		case it := <-e.Stream:
			rt.onStream(accs, it)
		case c := <-e.Control:
			rt.onControl(accs, c)
		default:
			break queued
		}
	}
	for rid, a := range accs {
		if rt.drainCtx.Err() != nil {
			return
		}
		if a.pending != nil {
			// Every queued chunk was processed: nothing more is coming for this completion.
			d := a.pending
			a.pending = nil
			rt.flushStream(accs, rid, a, d)
			continue
		}
		if a.re != nil {
			at := e.Now()
			for _, ev := range a.asm.Finish() {
				for _, r := range a.re.Feed(ev, at) {
					rt.handOff(rid, a, r)
				}
			}
			for _, r := range a.re.Flush(stream.StatusIncomplete, at) {
				if r.TerminalStatus == stream.StatusIncomplete && r.IncompleteReason == "" {
					r.IncompleteReason = IncompleteStopped
				}
				// Chat and Gemini report completed once a finish reason arrived: the body is whole.
				a.cut = a.cut || r.TerminalStatus != stream.StatusCompleted
				rt.handOff(rid, a, r)
			}
		}
		rt.releaseAcc(rid, a)
		delete(accs, rid)
	}
	rt.activeStreams.Store(int64(len(accs)))
}

func (rt *Runtime) releaseAcc(rid string, a *acc) {
	e := rt.opts.Engine
	if a.state > 0 {
		e.Pool.ReleaseStream(a.state)
		rt.streamAccum.Add(-a.state)
		st := a.state
		e.With(rid, false, func(en *hooks.Entry) { en.StreamBytes -= st })
		a.state = 0
	}
}

// SweepAccsNow runs the accumulator idle sweep (tests).
func (rt *Runtime) sweepAccs(accs map[string]*acc, cutoff time.Time) {
	for rid, a := range accs {
		if a.touched.Before(cutoff) {
			rt.releaseAcc(rid, a)
			delete(accs, rid)
		}
	}
}

func (rt *Runtime) onStream(accs map[string]*acc, it hooks.StreamItem) {
	done, found := rt.opts.Engine.ChunkDone(it.RequestID, false)
	a := accs[it.RequestID]
	if a == nil {
		a = &acc{}
		accs[it.RequestID] = a
	}
	if a.re == nil { // new, or a placeholder holding an early completion
		a.format, a.model, a.meta = it.SourceFormat, it.Model, it.Meta
		a.asm, a.re = &stream.Assembler{}, stream.New(it.SourceFormat)
		// CPA sends the header-init first and numbers payload chunks from 0: a later chunk here
		// means the stream started before this runtime (a restart or a plugin update).
		a.cut = it.ChunkIndex > 0
	}
	a.touched = it.At
	if it.ChunkIndex == protocol.StreamHeaderInit {
		if a.chunks > 0 || a.state > 0 || !a.headerInitAt.IsZero() {
			// Bootstrap retry: CPA ran the stream again and sent a second header-init for the
			// same request. Discard what the previous attempt buffered.
			a.resets++
			rt.releaseAcc(it.RequestID, a)
			a.asm, a.re = &stream.Assembler{}, stream.New(it.SourceFormat)
			a.chunks, a.bytes, a.firstChunkAt, a.cut = 0, 0, time.Time{}, false
		}
		a.headers, a.headerInitAt = it.Headers, it.At
		if it.SourceFormat != "" {
			a.format = it.SourceFormat
		}
	} else {
		if a.firstChunkAt.IsZero() {
			a.firstChunkAt = it.At
		}
		a.chunks++
		a.bytes += int64(len(it.Body))
		a.state += it.Reserved // the raw chunk's reservation becomes assembled state
		rt.streamAccum.Add(it.Reserved)
		for _, ev := range a.asm.Add(it.Body) {
			for _, r := range a.re.Feed(ev, it.At) {
				rt.handOff(it.RequestID, a, r)
			}
		}
	}
	if a.pending != nil && (!found || done >= a.pending.ChunksQueued) {
		d := a.pending
		a.pending = nil
		rt.flushStream(accs, it.RequestID, a, d)
	}
}

func (rt *Runtime) onControl(accs map[string]*acc, c hooks.Control) {
	e := rt.opts.Engine
	switch {
	case c.Attempt != nil:
		if env, err := rt.buildAttempt(c.Attempt); err == nil {
			rt.write(env, nil, nil)
		}
	case c.Begin != nil:
		ms := rt.opts.Masker.Get()
		kept, hints, _ := headers.FilterRequest(c.Begin.Req.Headers, ms.Masker, rt.opts.HMACKeyID, rt.opts.HMACKey)
		if env, _, err := rt.buildBegin(c.Begin, splitResult{dropped: c.Begin.MetaOnly}, kept, hints); err == nil {
			if rt.write(env, nil, nil) {
				e.NoteWritten(c.Begin.Req.RequestID, true)
			}
		}
	case c.Response != nil:
		if env, _, _, err := rt.buildResponse(c.Response, splitResult{dropped: c.Response.MetaOnly}); err == nil {
			if rt.write(env, nil, nil) {
				e.NoteWritten(c.Response.RequestID, false)
			}
		}
	case c.Done != nil:
		rid := c.Done.Req.RequestID
		a := accs[rid]
		if c.Done.ChunksQueued > 0 && !e.ChunksProcessed(rid, c.Done.ChunksQueued) {
			// The channels are read in no fixed order: hold the completion until every chunk the
			// hooks queued for this RequestID was processed.
			if a == nil {
				a = &acc{touched: c.Done.ObservedAt}
				accs[rid] = a
			}
			a.pending = c.Done
			return
		}
		rt.flushStream(accs, rid, a, c.Done)
	}
}

// flushStream closes a RequestID's stream at its completion and writes the completion.
func (rt *Runtime) flushStream(accs map[string]*acc, rid string, a *acc, d *hooks.Done) {
	e := rt.opts.Engine
	if a != nil && a.re != nil {
		status := stream.StatusCompleted
		switch d.Req.Outcome {
		case "failed":
			status = stream.StatusFailed
		case "canceled":
			status = stream.StatusCanceled
		}
		for _, ev := range a.asm.Finish() {
			for _, r := range a.re.Feed(ev, d.ObservedAt) {
				rt.handOff(rid, a, r)
			}
		}
		open := a.re.Flush(status, d.ObservedAt)
		if d.Req.Outcome == "canceled" && len(open) == 0 && a.ended {
			// Every response this stream carried had ended before the cancel. CPA v8.0.60 cancels
			// a Responses WebSocket turn when the client closes the socket after the turn's last
			// event but before CPA reads the upstream close (and always for a duplex RequestID,
			// which ends only with the socket); it does not track WebSocket delivery. Record what
			// CPA reports for an HTTP Responses stream whose delivery was acknowledged.
			d.Req.Outcome, d.Req.StatusCode, d.Req.Error = "succeeded", http.StatusOK, ""
		}
		for _, r := range open {
			rt.handOff(rid, a, r)
		}
	}
	if a != nil {
		rt.releaseAcc(rid, a)
		delete(accs, rid)
	}
	if cs, ready := e.TakeCompletion(d); ready {
		rt.writeCompletion(d, cs)
	}
}

// handOff converts a reassembled response's stream reservation into 4 × its size and hands it
// to a worker; on budget or a full queue it writes metadata only.
func (rt *Runtime) handOff(rid string, a *acc, sr stream.Response) {
	e := rt.opts.Engine
	rt.releaseAcc(rid, a)
	switch sr.TerminalStatus {
	case stream.StatusCompleted, stream.StatusDone, stream.StatusIncomplete:
		a.ended = !sr.TerminalAt.IsZero() // zero when Flush closed it, not a terminal event
	default:
		a.ended = false
	}
	r := &hooks.Response{
		RequestID: rid, SourceFormat: a.format, Model: a.model, StatusCode: 200, Stream: true, Headers: a.headers,
		Body: sr.Body, ContentType: "application/json", ObservedAt: e.Now(), Meta: a.meta,
		ResponseID: sr.ResponseID, PrevResponseID: sr.PrevResponseID, HeaderInitAt: a.headerInitAt, FirstChunkAt: a.firstChunkAt,
		TerminalAt: sr.TerminalAt, TerminalStatus: sr.TerminalStatus, IncompleteReason: sr.IncompleteReason,
		Chunks: a.chunks, StreamBytes: a.bytes, BootstrapResets: a.resets, Events: sr.Events, Flags: sr.Flags,
	}
	e.With(rid, true, func(en *hooks.Entry) {
		r.Index = en.NextResponse
		en.NextResponse++
		r.Truncated = en.Truncated || a.cut
	})
	a.cut = false
	a.chunks, a.bytes, a.firstChunkAt = 0, 0, time.Time{}
	if CanonFormat(a.format) == canon.FormatOpaque {
		r.ContentType = ""
	}
	need := int64(len(r.Body)) * hooks.BodyReserveFactor
	if !e.Pool.TryReserve(need) {
		e.Stats.Drop("body_budget")
		rt.metaResponse(r, "body_budget")
		return
	}
	r.Reserved = need
	e.With(rid, false, func(en *hooks.Entry) { en.Pending++ })
	select {
	case e.Handoff <- hooks.Work{Response: r}:
	default:
		e.With(rid, false, func(en *hooks.Entry) { en.Pending-- })
		e.Pool.Release(need)
		r.Reserved = 0
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
	if rt.write(env, nil, nil) {
		rt.opts.Engine.NoteWritten(r.RequestID, false)
	}
}
