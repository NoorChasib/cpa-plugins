// Package hooks is the synchronous hot path. A hook decodes only what it keeps,
// reserves memory with an atomic CAS, updates one sharded-map entry under a short critical
// section, makes one non-blocking channel send and returns a pre-encoded OK. It never does I/O,
// compression or masking, and never returns an error.
//
// The Engine is created once per plugin instance. Its channels are never closed: stop drains
// them by admission state and a restart reuses them, so a hook can never send on a closed channel.
package hooks

import (
	"hash/fnv"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/fixture"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// Admission states.
const (
	StateStopped int32 = iota
	StateRunning
	StateStopping
)

// Memory budgets inside memory-limit-mib (768 MiB by default). The content pool admits bodies;
// the rest bound queues and per-stream state.
const (
	PoolBytes         = 256 << 20
	StreamTotalBytes  = 128 << 20
	StreamPerStream   = 64 << 20
	UsageQueueEntries = 16384
	UsageQueueBytes   = 32 << 20
	ControlEntries    = 4096
	HandoffEntries    = 256
	StreamEntries     = 8192
	IdleAfter         = 2 * time.Hour
	BodyReserveFactor = 4
)

// Config is what hooks read on every call; it is swapped atomically by configure.
type Config struct {
	Configured bool
	Capture    string // full | metadata | off
	MaxRPC     int
	HMACKeyID  string
	HMACKey    []byte
	Labels     LabelCache
}

// LabelCache is the label loop's in-memory cache as usage.handle reads it.
type LabelCache interface {
	// Lookup returns the runtime email and project id for an AuthIndex when cached.
	Lookup(authIndex string) (email, projectID string, ok bool)
	// Want queues an AuthIndex for lookup; it never blocks.
	Want(authIndex string)
}

// Pool is the content pool: every in-flight body reserves 4 × its size before decoding, and
// stream chunks reserve their size.
type Pool struct {
	cap, streamCap   int64
	used, streamUsed atomic.Int64
}

// NewPool returns a pool of the given size with a stream sub-budget.
func NewPool(capBytes, streamBytes int64) *Pool { return &Pool{cap: capBytes, streamCap: streamBytes} }

// TryReserve takes n bytes if they fit.
func (p *Pool) TryReserve(n int64) bool {
	if n < 0 {
		return false
	}
	for {
		u := p.used.Load()
		if u+n > p.cap {
			return false
		}
		if p.used.CompareAndSwap(u, u+n) {
			return true
		}
	}
}

// Release returns n bytes.
func (p *Pool) Release(n int64) {
	if n > 0 {
		p.used.Add(-n)
	}
}

// TryReserveStream takes n bytes from both the pool and the stream sub-budget.
func (p *Pool) TryReserveStream(n int64) bool {
	for {
		u := p.streamUsed.Load()
		if u+n > p.streamCap {
			return false
		}
		if p.streamUsed.CompareAndSwap(u, u+n) {
			break
		}
	}
	if !p.TryReserve(n) {
		p.streamUsed.Add(-n)
		return false
	}
	return true
}

// ReleaseStream returns n stream bytes.
func (p *Pool) ReleaseStream(n int64) {
	if n > 0 {
		p.streamUsed.Add(-n)
		p.used.Add(-n)
	}
}

// Used returns the reserved bytes; StreamUsed the stream part; Free what is left.
func (p *Pool) Used() int64       { return p.used.Load() }
func (p *Pool) StreamUsed() int64 { return p.streamUsed.Load() }
func (p *Pool) Free() int64       { return p.cap - p.used.Load() }

// ---- work items ----

// Begin is one before-auth body to split (Handoff) or a metadata-only begin (Control).
type Begin struct {
	Req        protocol.BeforeAuth
	InputSeq   uint32
	ObservedAt time.Time
	Reserved   int64
	MetaOnly   string // a wire.Dropped* reason, or "" for a body to split
}

// Response is one response body to split: a non-stream response from the hook, or a stream
// response reassembled by the stream goroutine.
type Response struct {
	RequestID       string
	SourceFormat    string
	Model           string
	Index           uint32
	StatusCode      int
	Stream          bool
	Headers         http.Header // response headers
	Body            []byte
	RawLen          int64 // body length when Body is not kept (metadata only)
	ContentType     string
	ContentEncoding string
	ObservedAt      time.Time
	Meta            protocol.Meta
	Reserved        int64
	MetaOnly        string

	ResponseID, PrevResponseID string
	HeaderInitAt, FirstChunkAt time.Time
	TerminalAt                 time.Time
	TerminalStatus             string
	IncompleteReason           string
	Chunks                     uint32
	StreamBytes                int64
	Truncated                  bool
	BootstrapResets            uint32
	Events                     []wire.StreamEvent
	Flags                      []string
}

// Work is one Handoff item: exactly one of Begin or Response.
type Work struct {
	Begin    *Begin
	Response *Response
}

// Attempt is one after-auth call.
type Attempt struct {
	Req        protocol.AfterAuth
	Ordinal    uint32
	ObservedAt time.Time
}

// Done is one request.complete call. ChunksQueued is the number of stream chunks the hooks
// queued for the RequestID, so the stream goroutine flushes only after processing all of them.
type Done struct {
	Req          protocol.Completion
	ChunksQueued uint64
	ObservedAt   time.Time
}

// Control is one Control item: exactly one field set.
type Control struct {
	Begin    *Begin
	Attempt  *Attempt
	Done     *Done
	Response *Response // metadata-only response
}

// StreamItem is one stream event for the stream goroutine.
type StreamItem struct {
	RequestID    string
	SourceFormat string
	Model        string
	ChunkIndex   int
	Headers      http.Header // header-init
	Body         []byte
	At           time.Time
	Reserved     int64
	Meta         protocol.Meta
	Truncate     bool // an oversize chunk was passed through: mark the stream truncated
}

// UsageItem is one usage record.
type UsageItem struct {
	Rec        protocol.Usage
	Bytes      int64
	ObservedAt time.Time
}

// ---- per-RequestID state ----

// Entry is the per-RequestID state. Fields are read and written only inside With.
type Entry struct {
	Touched        int64
	BeforeAuthSeen bool
	NextInputSeq   uint32
	NextOrdinal    uint32
	NextResponse   uint32
	Pending        int32 // Handoff items not yet finished by a worker
	BeginWritten   bool
	Responses      uint32
	Deferred       *Done  // completion waiting for Pending to reach 0
	ChunksQueued   uint64 // stream items the hooks queued (header-inits included)
	// ChunksDone counts the queued stream items the stream goroutine processed or a stop drained.
	// It lives here, not in the stream goroutine's accumulator, so it survives a runtime restart
	// or a stream-goroutine panic restart: a completion waits for ChunksDone to reach its
	// ChunksQueued snapshot, and the two never drift apart across restarts.
	ChunksDone  uint64
	StreamBytes int64 // queued plus assembled bytes of the current stream
	Truncated   bool
	HookMicros  [numHooks]uint32
	Stream      bool
}

type shard struct {
	mu sync.Mutex
	m  map[string]*Entry
}

const numShards = 64

// Engine is the hook side of one plugin instance.
type Engine struct {
	Stats   *Stats
	Pool    *Pool
	Fixture *fixture.Recorder

	Handoff chan Work
	Control chan Control
	Stream  chan StreamItem
	Usage   chan UsageItem

	usageBytes atomic.Int64
	state      atomic.Int32
	inflight   atomic.Int64
	cfg        atomic.Pointer[Config]
	shards     [numShards]shard
	Now        func() time.Time
}

// NewEngine creates the engine and its never-closed channels.
func NewEngine() *Engine {
	e := &Engine{
		Stats:   NewStats(),
		Pool:    NewPool(PoolBytes, StreamTotalBytes),
		Fixture: &fixture.Recorder{},
		Handoff: make(chan Work, HandoffEntries),
		Control: make(chan Control, ControlEntries),
		Stream:  make(chan StreamItem, StreamEntries),
		Usage:   make(chan UsageItem, UsageQueueEntries),
		Now:     time.Now,
	}
	for i := range e.shards {
		e.shards[i].m = map[string]*Entry{}
	}
	e.cfg.Store(&Config{Capture: "off", MaxRPC: protocol.MaxRPCDefault})
	return e
}

// SetConfig swaps the hook configuration.
func (e *Engine) SetConfig(c *Config) { e.cfg.Store(c) }

// Config returns the current hook configuration.
func (e *Engine) Config() *Config { return e.cfg.Load() }

// State returns the admission state.
func (e *Engine) State() int32 { return e.state.Load() }

// Start opens admission.
func (e *Engine) Start() { e.state.Store(StateRunning) }

// BeginStop closes admission and waits (bounded) until no hook is inside an admitted section, so
// no send can happen after it returns until the next Start.
func (e *Engine) BeginStop() {
	e.state.Store(StateStopping)
	deadline := time.Now().Add(2 * time.Second)
	for e.inflight.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Microsecond)
	}
}

// Stopped marks admission fully stopped.
func (e *Engine) Stopped() { e.state.Store(StateStopped) }

// IdlePoll is how often WaitOpen checks for open requests.
var IdlePoll = 10 * time.Millisecond

// WaitOpen waits, while admission stays open, until every request open when it was called has
// closed (its completion was taken, so its RequestID has no state) or deadline passes. It returns
// how many of those requests are still open. Requests that begin during the wait are not waited
// for, so steady traffic cannot hold a stop to its bound. A stop calls it before closing
// admission, so the replies of exchanges already begun are captured instead of cut off.
func (e *Engine) WaitOpen(deadline time.Time) int {
	var ids []string
	for i := range e.shards {
		s := &e.shards[i]
		s.mu.Lock()
		for rid := range s.m {
			ids = append(ids, rid)
		}
		s.mu.Unlock()
	}
	for {
		open := ids[:0]
		for _, rid := range ids {
			s := e.shard(rid)
			s.mu.Lock()
			_, ok := s.m[rid]
			s.mu.Unlock()
			if ok {
				open = append(open, rid)
			}
		}
		ids = open // a request that closed is never waited for again
		if len(ids) == 0 || !time.Now().Before(deadline) {
			return len(ids)
		}
		time.Sleep(min(IdlePoll, time.Until(deadline)))
	}
}

// Carried is the numbering and progress of one open request, carried from a stopping plugin
// instance to the next instance on the same outbox. CPA creates a new instance (and engine) for a
// version update, and sends the rest of every open exchange to it; with the carried state the
// continuation gets the next input_seq, attempt ordinal and response index instead of numbering
// from zero again and reusing keys the old instance already wrote.
type Carried struct {
	RequestID      string
	NextInputSeq   uint32
	NextOrdinal    uint32
	NextResponse   uint32
	Responses      uint32
	BeforeAuthSeen bool
	BeginWritten   bool
	Touched        time.Time
}

// OpenRequests returns the carried state of every open request.
func (e *Engine) OpenRequests() []Carried {
	var out []Carried
	for i := range e.shards {
		s := &e.shards[i]
		s.mu.Lock()
		for rid, en := range s.m {
			out = append(out, Carried{RequestID: rid, NextInputSeq: en.NextInputSeq, NextOrdinal: en.NextOrdinal,
				NextResponse: en.NextResponse, Responses: en.Responses, BeforeAuthSeen: en.BeforeAuthSeen,
				BeginWritten: en.BeginWritten, Touched: time.Unix(0, en.Touched)})
		}
		s.mu.Unlock()
	}
	return out
}

// Adopt seeds the state of carried requests, keeping each one's last-touched time so the idle
// sweep evicts it at the same age. A request that already has state (the same instance restarting
// its runtime) keeps the larger of each counter. It returns the number of requests adopted.
func (e *Engine) Adopt(cs []Carried) int {
	for _, c := range cs {
		s := e.shard(c.RequestID)
		s.mu.Lock()
		en := s.m[c.RequestID]
		if en == nil {
			en = &Entry{Touched: c.Touched.UnixNano()}
			s.m[c.RequestID] = en
		}
		en.NextInputSeq = max(en.NextInputSeq, c.NextInputSeq)
		en.NextOrdinal = max(en.NextOrdinal, c.NextOrdinal)
		en.NextResponse = max(en.NextResponse, c.NextResponse)
		en.Responses = max(en.Responses, c.Responses)
		en.BeforeAuthSeen = en.BeforeAuthSeen || c.BeforeAuthSeen
		en.BeginWritten = en.BeginWritten || c.BeginWritten
		s.mu.Unlock()
	}
	return len(cs)
}

func (e *Engine) admit() bool {
	e.inflight.Add(1)
	if e.state.Load() != StateRunning {
		e.inflight.Add(-1)
		if e.state.Load() == StateStopping {
			e.Stats.Drop("stopped")
		}
		return false
	}
	return true
}

func (e *Engine) leave() { e.inflight.Add(-1) }

func (e *Engine) shard(rid string) *shard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(rid))
	return &e.shards[h.Sum32()%numShards]
}

// With runs fn on the entry for rid under its shard lock. When create is false and no entry
// exists, fn is not called and With returns false. fn must only read and write the entry.
func (e *Engine) With(rid string, create bool, fn func(*Entry)) bool {
	s := e.shard(rid)
	s.mu.Lock()
	defer s.mu.Unlock()
	en := s.m[rid]
	if en == nil {
		if !create {
			return false
		}
		en = &Entry{}
		s.m[rid] = en
	}
	en.Touched = e.Now().UnixNano()
	fn(en)
	return true
}

// Delete removes the entry for rid.
func (e *Engine) Delete(rid string) {
	s := e.shard(rid)
	s.mu.Lock()
	delete(s.m, rid)
	s.mu.Unlock()
}

// Entries returns the number of live entries.
func (e *Engine) Entries() int {
	n := 0
	for i := range e.shards {
		s := &e.shards[i]
		s.mu.Lock()
		n += len(s.m)
		s.mu.Unlock()
	}
	return n
}

// Sweep removes entries untouched since before cutoff and counts state_evicted (the idle
// sweep for requests whose completion never arrived). Entries hold no pool bytes themselves; the stream goroutine sweeps its accumulators.
func (e *Engine) Sweep(cutoff time.Time) int {
	c := cutoff.UnixNano()
	n := 0
	for i := range e.shards {
		s := &e.shards[i]
		s.mu.Lock()
		for k, en := range s.m {
			if en.Touched < c {
				delete(s.m, k)
				n++
			}
		}
		s.mu.Unlock()
	}
	if n > 0 {
		e.Stats.Count("state_evicted", uint64(n))
	}
	return n
}

// WorkDone is called by a worker when a Handoff item finished (written or not). It returns the
// deferred completion when this was the last pending item.
func (e *Engine) WorkDone(rid string, begin, written bool) *Done {
	var d *Done
	e.With(rid, false, func(en *Entry) {
		if en.Pending > 0 {
			en.Pending--
		}
		if written {
			if begin {
				en.BeginWritten = true
			} else {
				en.Responses++
			}
		}
		if en.Pending == 0 && en.Deferred != nil {
			d, en.Deferred = en.Deferred, nil
		}
	})
	return d
}

// NoteWritten records a begin or response written outside the Handoff path (metadata-only).
func (e *Engine) NoteWritten(rid string, begin bool) {
	e.With(rid, false, func(en *Entry) {
		if begin {
			en.BeginWritten = true
		} else {
			en.Responses++
		}
	})
}

// CompletionState is what an exchange.completion needs from the entry.
type CompletionState struct {
	BeginWritten bool
	Responses    uint32
	HookMicros   map[string]uint32
}

// TakeCompletion returns the entry's completion state and removes the entry when no Handoff
// work is pending; otherwise it defers d until the last WorkDone and returns ok=false.
func (e *Engine) TakeCompletion(d *Done) (CompletionState, bool) {
	var cs CompletionState
	ready := true
	found := e.With(d.Req.RequestID, false, func(en *Entry) {
		if en.Pending > 0 {
			en.Deferred = d
			ready = false
			return
		}
		cs = completionState(en)
	})
	if !found {
		return cs, true
	}
	if ready {
		e.Delete(d.Req.RequestID)
	}
	return cs, ready
}

// FinishDeferred returns the state for a deferred completion and removes the entry.
func (e *Engine) FinishDeferred(rid string) CompletionState {
	var cs CompletionState
	e.With(rid, false, func(en *Entry) { cs = completionState(en) })
	e.Delete(rid)
	return cs
}

func completionState(en *Entry) CompletionState {
	cs := CompletionState{BeginWritten: en.BeginWritten, Responses: en.Responses}
	for h, v := range en.HookMicros {
		if v > 0 {
			if cs.HookMicros == nil {
				cs.HookMicros = map[string]uint32{}
			}
			cs.HookMicros[hookNames[h]] = v
		}
	}
	return cs
}

// ChunkDone counts one queued stream item of rid as processed (or drained, with truncate set) and
// returns the new ChunksDone; ok is false when rid has no entry.
func (e *Engine) ChunkDone(rid string, truncate bool) (done uint64, ok bool) {
	ok = e.With(rid, false, func(en *Entry) {
		en.ChunksDone++
		if truncate {
			en.Truncated = true
		}
		done = en.ChunksDone
	})
	return done, ok
}

// ChunksProcessed reports whether every stream item rid's hooks queued up to queued was
// processed. A missing entry counts as processed.
func (e *Engine) ChunksProcessed(rid string, queued uint64) bool {
	ready := true
	e.With(rid, false, func(en *Entry) { ready = en.ChunksDone >= queued })
	return ready
}

// UsageDequeued returns a usage item's bytes to the usage queue byte bound.
func (e *Engine) UsageDequeued(n int64) { e.usageBytes.Add(-n) }
