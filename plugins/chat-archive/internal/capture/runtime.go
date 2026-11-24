package capture

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/labels"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/safego"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/headers"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// Drain bounds: a stop waits at most this long for the workers.
var DrainTimeout = 10 * time.Second

// MediaSink receives media URLs for the archive's media fetch (memory only, never written).
type MediaSink interface {
	Enqueue(wire.MediaFetch) bool
}

// Options configure a capture runtime.
type Options struct {
	Engine    *hooks.Engine
	Outbox    *outbox.Outbox
	Workers   int
	HMACKeyID string
	HMACKey   []byte
	Masker    *MaskerHolder
	Limits    func() wire.Limits
	Media     MediaSink
	Labels    *labels.Cache
	Notify    func() // wakes the uploader after a commit
	Prefix    *PrefixMap
}

// Runtime is one started capture pipeline.
type Runtime struct {
	opts   Options
	prefix *PrefixMap
	writes chan writeReq

	pressure      atomic.Int32
	pressureByCap atomic.Bool
	maskStats     maskStats

	runCtx    context.Context // workers
	runCancel context.CancelFunc
	seqCtx    context.Context // the stream goroutine
	seqCancel context.CancelFunc
	drainCtx  context.Context
	drainStop context.CancelFunc
	wg        sync.WaitGroup
	seqWG     sync.WaitGroup
	writerWG  sync.WaitGroup
	writerCtx context.Context
	writerEnd context.CancelFunc
	// writerDone is closed once the writer goroutine returned (WriterDone).
	doneOnce   sync.Once
	writerDone chan struct{}

	activeStreams atomic.Int64
	streamAccum   atomic.Int64
	stopBy        atomic.Int64 // StopBy's deadline (unix ns), 0 while running
	// cut is canceled at StopBy's deadline: it ends the writer's running outbox write attempt and its backoff wait.
	cutOnce sync.Once
	cut     context.Context
	cutStop context.CancelFunc

	// obWrite is the outbox write the writer calls (Outbox.Write when nil; tests inject failures).
	obWrite func(context.Context, []outbox.NewEnvelope) (outbox.WriteResult, error)
	// obPressure is the pressure the writer reads before each attempt (Outbox.Pressure when nil; tests inject levels).
	obPressure func() outbox.PressureState
	// holdUntil ends the retries of failing writes once a stop without a deadline has lasted
	// DrainTimeout. The writer goroutine alone reads and sets it.
	holdUntil time.Time
}

// writeReq is one envelope for the writer. release returns the budget the envelope's bytes are
// counted against (the body's content-pool reservation, or a usage item's queue bytes); the
// writer calls it after the outbox transaction that wrote the envelope returned, or when it
// drops the envelope, so a queued envelope's pieces and the writer's compressed copies stay
// inside the budget until they are on disk.
type writeReq struct {
	env     outbox.NewEnvelope
	commit  func(inserted bool)
	release func()
}

func (rt *Runtime) stats() *hooks.Stats { return rt.opts.Engine.Stats }

func (rt *Runtime) limits() wire.Limits {
	if rt.opts.Limits != nil {
		return rt.opts.Limits()
	}
	return wire.DefaultLimits
}

// Start launches the workers, the stream goroutine and the writer.
func Start(opts Options) *Runtime {
	if opts.Prefix == nil {
		opts.Prefix = NewPrefixMap()
	}
	rt := &Runtime{opts: opts, prefix: opts.Prefix, writes: make(chan writeReq, 1024)}
	rt.runCtx, rt.runCancel = context.WithCancel(context.Background())
	rt.seqCtx, rt.seqCancel = context.WithCancel(context.Background())
	rt.drainCtx, rt.drainStop = context.WithCancel(context.Background())
	rt.writerCtx, rt.writerEnd = context.WithCancel(context.Background())
	rec := opts.Engine.Stats.Panic
	ps := opts.Outbox.Pressure()
	rt.setPressure(ps)
	opts.Engine.SetWriteStall(false) // a new write queue
	for i := 0; i < max(opts.Workers, 1); i++ {
		safego.Loop(rt.runCtx, &rt.wg, "worker", rec, rt.worker)
	}
	safego.Loop(rt.seqCtx, &rt.seqWG, "stream", rec, rt.sequencer)
	safego.Loop(rt.writerCtx, &rt.writerWG, "writer", rec, rt.writer)
	return rt
}

func (rt *Runtime) setPressure(ps outbox.PressureState) {
	rt.pressure.Store(int32(ps.Level))
	rt.pressureByCap.Store(ps.ByCap)
}

// Prefix returns the prefix map (dead-letter notifications).
func (rt *Runtime) Prefix() *PrefixMap { return rt.prefix }

// Stop is StopBy with no deadline but DrainTimeout.
func (rt *Runtime) Stop() { rt.StopBy(time.Time{}) }

// StopWriterGrace is how long past its deadline StopBy waits for the writer to return from an attempt the deadline
// canceled, before it leaves the writer the outbox and returns.
var StopWriterGrace = 50 * time.Millisecond

// WriterDone is closed once the writer goroutine has returned, after which it no longer uses the outbox.
func (rt *Runtime) WriterDone() <-chan struct{} {
	rt.doneOnce.Do(func() {
		rt.writerDone = make(chan struct{})
		go func() {
			rt.writerWG.Wait()
			close(rt.writerDone)
		}()
	})
	return rt.writerDone
}

// stopCut is the context StopBy's deadline cancels.
func (rt *Runtime) stopCut() context.Context {
	rt.cutOnce.Do(func() { rt.cut, rt.cutStop = context.WithCancel(context.Background()) })
	return rt.cut
}

// StopBy drains what admission let in, within DrainTimeout and before deadline (when not zero).
// The engine's admission must already be stopping, so the queues only shrink. First the stream
// goroutine processes the queued stream and control items and writes each stream still open as a
// partial response (finishStreams), while the workers keep running; then the workers process the
// queued bodies and usage items; then the writer flushes what it holds, its outbox writes also
// bounded by deadline, which cancels a write attempt still running then and ends a wait between
// attempts. Past the drain bound, unfinished bodies and whatever is still queued are counted
// stopped and released, never written; an exchange's completion that waited for a drained body is
// still handed to the writer until deadline (or DrainTimeout), else counted stopped.
//
// The deadline's cancel reaches the backoff wait and the outbox's SQLite statements (the driver
// interrupts them, and the transaction is rolled back), not a blob file's write and fsync, the
// blob directory's fsync, or SQLite's own fsync at commit: on a stalled disk those return only
// when the disk does (review 2 of the round-4 fixes). So with a deadline StopBy waits for the
// writer only until deadline plus StopWriterGrace and then returns false, leaving the writer, which
// still owns the outbox, to finish in the background: the caller must not close the outbox until
// WriterDone. It returns true when the writer has returned. Without a deadline it waits for the
// writer, as long as that takes.
func (rt *Runtime) StopBy(deadline time.Time) bool {
	bound := DrainTimeout
	finalBy := time.Now().Add(DrainTimeout)
	if !deadline.IsZero() {
		finalBy = deadline
		bound = min(bound, max(time.Until(deadline), 0))
		rt.stopBy.Store(deadline.UnixNano())
		rt.stopCut() // sets cutStop
		t := time.AfterFunc(max(time.Until(deadline), 0), rt.cutStop)
		defer t.Stop()
	}
	expire := time.AfterFunc(bound, rt.drainStop)
	defer expire.Stop()
	rt.seqCancel()
	rt.seqWG.Wait()
	rt.runCancel()
	rt.wg.Wait()
	rt.drainStop()
	rt.drainChannels(finalBy)
	rt.writerEnd()
	if deadline.IsZero() {
		<-rt.WriterDone()
		return true
	}
	t := time.NewTimer(max(time.Until(deadline), 0) + StopWriterGrace)
	defer t.Stop()
	select {
	case <-rt.WriterDone():
		return true
	case <-t.C:
		return false
	}
}

// drainChannels counts every queued item stopped and releases its reservation. When a drained
// body was the last work an exchange's completion waited for, the completion (recording the begin
// or response as not written) still goes to the writer, which is running, until by.
func (rt *Runtime) drainChannels(by time.Time) {
	e := rt.opts.Engine
	for {
		select {
		case w := <-e.Handoff:
			e.Stats.Drop("stopped")
			if w.Begin != nil {
				e.Pool.Release(w.Begin.Reserved)
				for _, d := range e.WorkDone(w.Begin.Req.RequestID, true, false) {
					rt.completeDeferredBy(d, by)
				}
			}
			if w.Response != nil {
				e.Pool.Release(w.Response.Reserved)
				for _, d := range e.WorkDone(w.Response.RequestID, false, false) {
					rt.completeDeferredBy(d, by)
				}
			}
		case c := <-e.Control:
			e.Stats.Drop("stopped")
			_ = c
		case s := <-e.Stream:
			e.Stats.Drop("stopped")
			if s.Reserved > 0 {
				e.Pool.ReleaseStream(s.Reserved)
				e.With(s.RequestID, false, func(en *hooks.Entry) { en.StreamBytes -= s.Reserved })
			}
			// A drained item counts as done so a completion after the restart is not held for
			// it; the stream lost data, so it is truncated.
			e.ChunkDone(s.RequestID, true)
		case u := <-e.Usage:
			e.Stats.Drop("stopped")
			e.UsageDequeued(u.Bytes)
		default:
			return
		}
	}
}

// write hands an envelope to the writer. It blocks until accepted or the runtime drains. It
// owns release (nil for envelopes with no budget): called at once when the envelope is not
// accepted, else by the writer once the envelope is written or dropped.
func (rt *Runtime) write(env outbox.NewEnvelope, commit func(bool), release func()) bool {
	if !wire.ValidKey(env.Key) {
		rt.stats().Drop("encode_error")
		if release != nil {
			release()
		}
		return false
	}
	req := writeReq{env: env, commit: commit, release: release}
	select {
	case rt.writes <- req:
		return true
	default:
	}
	// The write queue is full: the hooks shed new bodies until the writer drained it (hooks.Engine.ShedBodies).
	rt.opts.Engine.SetWriteStall(true)
	select {
	case rt.writes <- req:
		return true
	case <-rt.drainCtx.Done():
		rt.stats().Drop("stopped")
		if release != nil {
			release()
		}
		return false
	}
}

// ---- workers ----

func (rt *Runtime) worker(ctx context.Context) {
	e := rt.opts.Engine
	for {
		safego.Checkpoint("worker")
		select {
		case <-ctx.Done():
			rt.finishWork()
			return
		case w := <-e.Handoff:
			rt.doWork(w)
		case u := <-e.Usage:
			rt.doUsage(&u)
		}
	}
}

func (rt *Runtime) doWork(w hooks.Work) {
	if w.Begin != nil {
		rt.doBegin(w.Begin)
	} else if w.Response != nil {
		rt.doResponse(w.Response)
	}
}

// finishWork runs at a stop: it processes the bodies and usage items still queued (admission is
// closed, so the queues only shrink) until they are empty or the drain deadline passed.
func (rt *Runtime) finishWork() {
	e := rt.opts.Engine
	for rt.drainCtx.Err() == nil {
		select {
		case w := <-e.Handoff:
			rt.doWork(w)
		case u := <-e.Usage:
			rt.doUsage(&u)
		default:
			return
		}
	}
}

// poolRelease returns a func releasing n content-pool bytes (nil for n <= 0).
func (rt *Runtime) poolRelease(n int64) func() {
	if n <= 0 {
		return nil
	}
	p := rt.opts.Engine.Pool
	return func() { p.Release(n) }
}

func (rt *Runtime) doBegin(b *hooks.Begin) {
	e := rt.opts.Engine
	written := false
	reserved := b.Reserved
	b.Reserved = 0
	handed := false // the reservation went to the writer with the envelope
	defer func() {
		if !handed {
			e.Pool.Release(reserved)
		}
		b.Req.Body = nil
		for _, d := range e.WorkDone(b.Req.RequestID, true, written) {
			rt.completeDeferred(d)
		}
	}()
	ms := rt.opts.Masker.Get()
	kept, hints, _ := headers.FilterRequest(b.Req.Headers, ms.Masker, rt.opts.HMACKeyID, rt.opts.HMACKey)
	in := canon.Input{Format: CanonFormat(b.Req.SourceFormat), Body: b.Req.Body,
		ContentType: hooks.HeaderGet(b.Req.Headers, "Content-Type"), ContentEncoding: hooks.HeaderList(b.Req.Headers, "Content-Encoding")}
	sr, err := rt.split(rt.drainCtx, in, reserved, false)
	if err != nil {
		e.Stats.Drop("stopped")
		return
	}
	env, commit, err := rt.buildBegin(b, sr, kept, hints)
	if err != nil {
		return
	}
	handed = true
	written = rt.write(env, rt.beginCommitted(commit), rt.poolRelease(reserved))
}

func (rt *Runtime) beginCommitted(c *beginCommit) func(bool) {
	if c == nil {
		return nil
	}
	return func(inserted bool) {
		if inserted {
			rt.prefix.Committed(c.conv, c.key, c.ids, c.dropped)
		}
	}
}

func (rt *Runtime) doResponse(r *hooks.Response) {
	e := rt.opts.Engine
	written := false
	reserved := r.Reserved
	r.Reserved = 0
	handed := false // the reservation went to the writer with the envelope
	defer func() {
		if !handed {
			e.Pool.Release(reserved)
		}
		r.Body = nil
		for _, d := range e.WorkDone(r.RequestID, false, written) {
			rt.completeDeferred(d)
		}
	}()
	in := canon.Input{Format: CanonFormat(r.SourceFormat), Body: r.Body, ContentType: r.ContentType, ContentEncoding: r.ContentEncoding}
	sr, err := rt.split(rt.drainCtx, in, reserved, true)
	if err != nil {
		e.Stats.Drop("stopped")
		return
	}
	env, commit, urls, err := rt.buildResponse(r, sr)
	if err != nil {
		return
	}
	handed = true
	written = rt.write(env, func(inserted bool) {
		if inserted && commit != nil {
			rt.prefix.ResponseCommitted(commit.beginKey, commit.key, commit.index, commit.ids, commit.execSession, commit.responseID)
		}
	}, rt.poolRelease(reserved))
	if written && rt.opts.Media != nil {
		for i, u := range urls {
			exp := time.Now().Add(time.Hour).UTC()
			if !rt.opts.Media.Enqueue(wire.MediaFetch{RequestID: r.RequestID, Ordinal: uint32(i), URL: u.URL, MaskedURL: u.MaskedURL, ExpiresAt: &exp}) {
				e.Stats.Drop("media_fetch")
			}
		}
	}
}

// doUsage writes usage.attempt. The item's usage-queue bytes stay counted until the writer wrote
// the envelope, so masked failure bodies queued for a slow writer stay inside the 32 MiB bound.
func (rt *Runtime) doUsage(u *hooks.UsageItem) {
	e := rt.opts.Engine
	n := u.Bytes
	release := func() { e.UsageDequeued(n) }
	env, err := rt.buildUsage(rt.drainCtx, u)
	u.Rec.FailureBody = ""
	if err != nil {
		if err == errStopped {
			rt.stats().Drop("stopped")
		}
		release()
		return
	}
	rt.write(env, nil, release)
}

// completeDeferred writes a completion whose Handoff work just finished.
func (rt *Runtime) completeDeferred(d *hooks.Done) {
	cs := rt.opts.Engine.FinishDeferred(d)
	rt.writeCompletion(d, cs)
}

// completeDeferredBy writes a deferred completion at a stop, after the drain bound: it waits for
// the writer until by, and counts the completion dropped.stopped when the writer did not take it.
func (rt *Runtime) completeDeferredBy(d *hooks.Done, by time.Time) {
	cs := rt.opts.Engine.FinishDeferred(d)
	env, err := rt.buildCompletion(d, cs)
	if err != nil {
		return
	}
	if !wire.ValidKey(env.Key) {
		rt.stats().Drop("encode_error")
		return
	}
	t := time.NewTimer(max(time.Until(by), 0))
	defer t.Stop()
	select {
	case rt.writes <- writeReq{env: env}:
		return
	default:
	}
	select {
	case rt.writes <- writeReq{env: env}:
	case <-t.C:
		rt.stats().Drop("stopped")
	}
}

func (rt *Runtime) writeCompletion(d *hooks.Done, cs hooks.CompletionState) {
	env, err := rt.buildCompletion(d, cs)
	if err != nil {
		return
	}
	rt.write(env, nil, nil)
}

// ---- writer ----

// Writer grouping: at most 50 envelopes, 32 MiB or 25 ms per transaction.
const (
	groupEnvelopes = 50
	groupBytes     = 32 << 20
	groupLinger    = 25 * time.Millisecond
)

// Writer retries: a group's first outbox write attempt is bounded by WriteTimeout, and each attempt
// after a timeout gets twice the last one's time, up to WriteTimeoutMax, so a disk that is slow but
// working always completes a write. A failed group waits RetryBackoffMin before its next attempt,
// doubling up to RetryBackoffMax.
var (
	WriteTimeout    = 10 * time.Second
	WriteTimeoutMax = time.Minute
	RetryBackoffMin = 25 * time.Millisecond
	RetryBackoffMax = time.Second
)

// busyFaultAfter is how many consecutive SQLITE_BUSY or SQLITE_LOCKED attempts raise write_unavailable;
// any other failure raises it at once.
const busyFaultAfter = 3

func reqBytes(r writeReq) int64 {
	n := int64(len(r.env.Body))
	for _, p := range r.env.Pieces {
		n += int64(len(p.Bytes))
	}
	return n
}

func (rt *Runtime) writer(ctx context.Context) {
	for {
		safego.Checkpoint("writer")
		var group []writeReq
		select {
		case r := <-rt.writes:
			group = append(group, r)
		case <-ctx.Done():
			// Flush everything still queued, then exit.
			for {
				select {
				case r := <-rt.writes:
					group = append(group, r)
					if len(group) >= groupEnvelopes {
						rt.flush(group)
						group = nil
					}
					continue
				default:
				}
				break
			}
			if len(group) > 0 {
				rt.flush(group)
			}
			return
		}
		size := reqBytes(group[0])
		timer := time.NewTimer(groupLinger)
	gather:
		for len(group) < groupEnvelopes && size < groupBytes {
			select {
			case r := <-rt.writes:
				group = append(group, r)
				size += reqBytes(r)
			case <-timer.C:
				break gather
			case <-ctx.Done():
				break gather
			}
		}
		timer.Stop()
		rt.flush(group)
		rt.unstall()
	}
}

// unstall clears the hooks' write stall once the write queue is down to half (hysteresis: a queue that refills at
// once does not flip the hooks between full bodies and metadata only on every group).
func (rt *Runtime) unstall() {
	if len(rt.writes) <= cap(rt.writes)/2 {
		rt.opts.Engine.SetWriteStall(false)
	}
}

// writerStopping reports whether a stop ended the writer's context.
func (rt *Runtime) writerStopping() bool { return rt.writerCtx != nil && rt.writerCtx.Err() != nil }

// pause waits d between two attempts of a failing write; a stop that begins meanwhile ends the
// wait at once, and a stop's deadline ends it whenever it comes.
func (rt *Runtime) pause(d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	var stop <-chan struct{}
	if rt.writerCtx != nil && rt.writerCtx.Err() == nil {
		stop = rt.writerCtx.Done()
	}
	select {
	case <-t.C:
	case <-stop:
	case <-rt.stopCut().Done():
	}
}

// holdEnd is the counter under which a stop ends the hold of a group not written yet: stopped once its deadline
// passed; for a stop without one, disk_hard once the group has been held DrainTimeout into the stop; "" while the
// group is still held.
func (rt *Runtime) holdEnd() string {
	if by := rt.stopBy.Load(); by != 0 && !time.Now().Before(time.Unix(0, by)) {
		return "stopped"
	}
	if rt.writerStopping() {
		if rt.holdUntil.IsZero() {
			rt.holdUntil = time.Now().Add(DrainTimeout)
		}
		if !time.Now().Before(rt.holdUntil) {
			return "disk_hard"
		}
	}
	return ""
}

// flush writes one group in one outbox transaction. A write that fails or times out keeps the group
// and tries it again after a backoff, as long as it takes: the group holds its place, so envelopes
// stay in order, and nothing is skipped untried while write_unavailable is raised. While the writer
// holds a group, the bounded write queue fills; from then until the writer drained it to half (unstall),
// and whenever Control is down to its metadata reserve, the hooks keep each new exchange's metadata
// without its body (hooks.Engine.ShedBodies: body_budget). The workers and the stream goroutine wait on
// the write queue, and the hooks' queues and the content pool fill (handoff_full). A whole envelope is
// lost only when those queues are full too
// (control_full, usage_queue, stream_overflow). At pressure level 3 nothing is written: the writer
// holds its group the same way, without attempting the write, and resumes it when the pressure
// falls. The writer still drops a group, counted under dropped, on an error no retry can fix
// (outbox.Permanent; disk_hard), and at a stop: past its deadline (stopped), which also cancels an
// attempt still running, or, for a stop without one, once it has held the group DrainTimeout into
// the stop (disk_hard).
func (rt *Runtime) flush(group []writeReq) {
	// Every path out of flush, a recovered panic included, returns the group's budgets: by then
	// the envelopes are written or dropped.
	defer func() {
		for _, r := range group {
			if r.release != nil {
				r.release()
			}
		}
	}()
	ob := rt.opts.Outbox
	write := rt.obWrite
	if write == nil {
		write = ob.Write
	}
	drop := func(name string) {
		for _, r := range group {
			rt.stats().Drop(name)
			if r.commit != nil {
				r.commit(false)
			}
		}
	}
	envs := make([]outbox.NewEnvelope, len(group))
	for i, r := range group {
		envs[i] = r.env
	}
	var res outbox.WriteResult
	backoff, failures, attempt := RetryBackoffMin, 0, WriteTimeout
	pressure := rt.obPressure
	if pressure == nil {
		pressure = ob.Pressure
	}
	for {
		ps := pressure()
		rt.setPressure(ps)
		if ps.Level >= outbox.PressureHard {
			// Nothing is written at level 3: hold the group, as for a failing write, until the pressure falls.
			if why := rt.holdEnd(); why != "" {
				drop(why)
				return
			}
			rt.pause(backoff)
			backoff = min(2*backoff, RetryBackoffMax)
			continue
		}
		budget := attempt
		if by := rt.stopBy.Load(); by != 0 {
			// A stop's deadline bounds the final flush too.
			if budget = min(budget, time.Until(time.Unix(0, by))); budget <= 0 {
				drop("stopped")
				return
			}
		}
		ctx, cancel := context.WithTimeout(rt.stopCut(), budget)
		r, err := write(ctx, envs)
		timedOut := ctx.Err() != nil
		cancel()
		if err == nil {
			res = r
			break
		}
		if by := rt.stopBy.Load(); by != 0 && !time.Now().Before(time.Unix(0, by)) {
			drop("stopped") // the stop's deadline cut the write, not the disk
			return
		}
		if failures++; !outbox.Retryable(err) || failures >= busyFaultAfter {
			rt.stats().SetFault("write_unavailable", true)
		}
		if outbox.Permanent(err) {
			drop("disk_hard")
			return
		}
		if why := rt.holdEnd(); why != "" {
			drop(why)
			return
		}
		if timedOut {
			attempt = min(2*attempt, max(WriteTimeoutMax, WriteTimeout))
		}
		rt.pause(backoff)
		backoff = min(2*backoff, RetryBackoffMax)
	}
	rt.stats().SetFault("write_unavailable", false)
	// A skipped key is an envelope that is not stored. Keys are unique per exchange part, and a new
	// plugin instance continues the open requests' numbering (hooks.Engine.Adopt), so this counts a loss.
	// A bare completion key already queued moves the completion to its started_at key instead.
	rt.stats().DropN("duplicate_key", uint64(res.Duplicates))
	rt.stats().Count("completion_rekeyed.outbox", uint64(res.Rekeyed))
	rt.stats().Count("reused_request_id", uint64(res.Rekeyed))
	inserted := make(map[string]bool, len(res.Inserted))
	for _, in := range res.Inserted {
		inserted[in.Key] = true
	}
	for _, r := range group {
		if r.commit != nil {
			r.commit(inserted[r.env.Key])
		}
		if res.Rekeyed > 0 && r.env.AltKey != "" && inserted[r.env.AltKey] && !namedLifecycle(r.env.Body) {
			// A completion built bare (so not counted at build) and stored at its started_at key.
			rt.stats().Count("lifecycle_unverified", 1)
		}
	}
	if rt.opts.Notify != nil && len(res.Inserted) > 0 {
		rt.opts.Notify()
	}
}

// ---- masking stats ----

type maskStats struct {
	mu       sync.Mutex
	bodies   uint64
	bytes    uint64
	timeouts atomic.Uint64
	lat      []float64
	byRule   map[string]uint64
}

func (m *maskStats) observe(d time.Duration, n int64, sp *canon.Split) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bodies++
	m.bytes += uint64(max(n, 0))
	m.lat = append(m.lat, float64(d.Microseconds())/1000)
	if len(m.lat) > 1024 {
		m.lat = m.lat[len(m.lat)-1024:]
	}
	if sp != nil {
		if m.byRule == nil {
			m.byRule = map[string]uint64{}
		}
		for k, v := range sp.Hits {
			m.byRule[k] += uint64(v)
		}
	}
}

// MaskerStats returns the heartbeat masker block.
func (rt *Runtime) MaskerStats() wire.MaskerStats {
	m := &rt.maskStats
	m.mu.Lock()
	defer m.mu.Unlock()
	s := wire.MaskerStats{Bodies: m.bodies, Bytes: m.bytes, Timeouts: m.timeouts.Load(), MatchesByRule: map[string]uint64{}}
	for k, v := range m.byRule {
		s.MatchesByRule[k] = v
	}
	if rt.opts.Masker != nil && rt.opts.Masker.Memo != nil {
		s.MemoHits, s.MemoMisses = rt.opts.Masker.Memo.Hits.Load(), rt.opts.Masker.Memo.Misses.Load()
	}
	if len(m.lat) > 0 {
		l := append([]float64(nil), m.lat...)
		sort.Float64s(l)
		s.P50MS = l[len(l)/2]
		s.P99MS = l[min(len(l)-1, len(l)*99/100)]
	}
	return s
}

// Pressure returns the last pressure level the writer saw.
func (rt *Runtime) Pressure() int { return int(rt.pressure.Load()) }

// RefreshPressure recomputes pressure (maintenance, every 60 s).
func (rt *Runtime) RefreshPressure() outbox.PressureState {
	ps := rt.opts.Outbox.Pressure()
	rt.setPressure(ps)
	return ps
}

// ActiveStreams and StreamAccumBytes feed the heartbeat runtime block.
func (rt *Runtime) ActiveStreams() int64    { return rt.activeStreams.Load() }
func (rt *Runtime) StreamAccumBytes() int64 { return rt.streamAccum.Load() }
