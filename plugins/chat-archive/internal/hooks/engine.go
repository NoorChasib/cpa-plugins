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
	"sort"
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
	// TombstoneTTL is how long the numbering of a closed RequestID is kept (its tombstone), so a
	// lifecycle that CPA starts later under the same RequestID continues it.
	TombstoneTTL = 10 * time.Minute
	// ClosedPerShard bounds the tombstones of one shard (64 shards); past it the oldest is dropped.
	ClosedPerShard = 256
	// maxMarks bounds Entry.Marks: the first mark since the entry (re)opened plus the 4 newest.
	maxMarks = 5
)

// Config is what hooks read on every call; it is swapped atomically by configure.
type Config struct {
	Configured bool
	Capture    string // full | metadata | off
	MaxRPC     int
	HMACKeyID  string
	HMACKey    []byte
	Labels     LabelCache
	// Unavailable is the configured capture mode (full or metadata) while capture is configured on
	// but no runtime accepts hooks (the secret, the working directory or the outbox is unavailable,
	// or a start has not finished): each hook call a running runtime would have admitted is counted
	// dropped.storage_unavailable. Empty counts nothing (capture off, or not configured).
	Unavailable string
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
// Req.StartedAt and Req.CompletedAt are CPA's lifecycle times, from the same wall clock as the
// hooks' start times (the plugin runs inside the CPA process).
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

	// Boundary evidence, recorded under the entry lock when the hook queued the item: the start
	// (unix ns) of the latest and second-latest before-auth of the RequestID, and a continuity
	// floor. Every before-auth of the RequestID that started after Since is reflected in BAAt and
	// BAPrevAt. The stream goroutine decides lifecycle boundaries from these, never from queue order.
	BAAt, BAPrevAt, Since int64
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
	Deferred       []*Done // completions waiting for Pending to reach 0
	ChunksQueued   uint64  // stream items the hooks queued (header-inits included)
	// ChunksDone counts the queued stream items the stream goroutine processed or a stop drained.
	// It lives here, not in the stream goroutine's accumulator, so it survives a runtime restart
	// or a stream-goroutine panic restart: a completion waits for ChunksDone to reach its
	// ChunksQueued snapshot, and the two never drift apart across restarts.
	ChunksDone  uint64
	StreamBytes int64 // queued plus assembled bytes of the current stream
	Truncated   bool
	HookMicros  [numHooks]uint32
	Stream      bool

	// Lifecycle evidence for a RequestID that CPA reuses (an in-place WebSocket failover since
	// v8.0.76). Hook times are the synchronous CPA hooks' own start times (unix ns), set only in
	// the hook closures; never by Touched, finish, the completion hook, workers or the sequencer.
	FirstHookAt int64 // min start of any synchronous hook seen for the RequestID; 0 = none
	LastHookAt  int64 // max start of the synchronous hooks seen for the RequestID
	BAAt        int64 // start of the latest before-auth
	BAPrevAt    int64 // start of the second-latest before-auth
	BASince     int64 // continuity floor: every before-auth that started after it is in BAAt/BAPrevAt
	BareStart   int64 // started_at of the completion that took the bare completion key; 0 = not taken
	Marks       []inputMark
	MarksPruned int64  // At of the newest mark pruned from Marks; 0 = none
	Held        uint32 // stream segments the sequencer still holds for the RequestID, unclaimed
	TakenEnd    int64  // max CompletedAt of the completions already taken from this entry
}

// inputMark is one before-auth of a RequestID, for lifecycle_input_seq.
type inputMark struct {
	Seq        uint32 // the input_seq the hook assigned (also on the oversize and dropped paths)
	At         int64  // the before-auth's start
	PrevHookAt int64  // LastHookAt just before this hook
	BlindAt    int64  // Engine.blindAt when this hook ran
}

// closedRID is the tombstone of a RequestID whose entry a completion or the idle sweep closed: the
// numbering and lifecycle evidence a later lifecycle under the same RequestID continues from.
type closedRID struct {
	NextInputSeq, NextOrdinal, NextResponse                               uint32
	FirstHookAt, LastHookAt, BAAt, BAPrevAt, BASince, BareStart, TakenEnd int64
	LastMark                                                              inputMark
	MarksPruned                                                           int64
	ClosedAt                                                              int64
	slot                                                                  int
}

type shard struct {
	mu     sync.Mutex
	m      map[string]*Entry
	closed map[string]*closedRID
	ring   [ClosedPerShard]string // ring of tombstone slots, oldest at head
	head   int
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

	// admittedSince (unix ns) is when the current admission began (Start or SetConfig): from it on,
	// every decodable hook of every RequestID was observed. blindAt (unix ns) is the latest moment a
	// hook could not be attributed to a RequestID (an oversize or undecodable call) or the idle sweep
	// evicted an entry; after it, "no hook of this RequestID was seen" proves nothing.
	admittedSince atomic.Int64
	blindAt       atomic.Int64
	// lostAt (unix ns) is the latest ClosedAt of a tombstone dropped by expiry or for capacity. An
	// admitted before-auth always creates or updates its RequestID's entry, and a closed entry leaves
	// a tombstone that the next one consumes, so an entry created with no tombstone proves that no
	// before-auth of its RequestID started after lostAt: that is its continuity floor (BASince).
	lostAt atomic.Int64
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
		e.shards[i].closed = map[string]*closedRID{}
	}
	e.cfg.Store(&Config{Capture: "off", MaxRPC: protocol.MaxRPCDefault})
	return e
}

// SetConfig swaps the hook configuration. Hooks before it ran under another configuration, so
// nothing they did not record is proven absent (admittedSince).
func (e *Engine) SetConfig(c *Config) {
	e.cfg.Store(c)
	e.admittedSince.Store(e.Now().UnixNano())
}

// StampBlind records that a hook could not be attributed to a RequestID at t.
func (e *Engine) StampBlind(t time.Time) { storeMax(&e.blindAt, t.UnixNano()) }

func storeMax(a *atomic.Int64, v int64) {
	for {
		cur := a.Load()
		if v <= cur || a.CompareAndSwap(cur, v) {
			return
		}
	}
}

// Config returns the current hook configuration.
func (e *Engine) Config() *Config { return e.cfg.Load() }

// State returns the admission state.
func (e *Engine) State() int32 { return e.state.Load() }

// Start opens admission. Hooks before it were not observed (admittedSince).
func (e *Engine) Start() {
	e.admittedSince.Store(e.Now().UnixNano())
	e.state.Store(StateRunning)
}

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

// Numbering is the lifecycle evidence of one RequestID carried across a draining stop (the
// completion_numbering table): an unexpired tombstone (ClosedAt set), or an open entry's hook times
// (ClosedAt 0). The before-auth evidence is not carried: admittedSince makes it unproven anyway.
type Numbering struct {
	RequestID                               string
	NextInputSeq, NextOrdinal, NextResponse uint32
	FirstHookAt, LastHookAt                 int64
	BareStart, TakenEnd                     int64
	ClosedAt                                int64
}

// Numbering returns every unexpired tombstone and every open entry's lifecycle evidence.
func (e *Engine) Numbering() []Numbering {
	now := e.Now().UnixNano()
	var out []Numbering
	for i := range e.shards {
		s := &e.shards[i]
		s.mu.Lock()
		for rid, tb := range s.closed {
			if now-tb.ClosedAt <= int64(TombstoneTTL) {
				out = append(out, Numbering{RequestID: rid, NextInputSeq: tb.NextInputSeq, NextOrdinal: tb.NextOrdinal,
					NextResponse: tb.NextResponse, FirstHookAt: tb.FirstHookAt, LastHookAt: tb.LastHookAt, BareStart: tb.BareStart,
					TakenEnd: tb.TakenEnd, ClosedAt: tb.ClosedAt})
			}
		}
		for rid, en := range s.m {
			out = append(out, Numbering{RequestID: rid, NextInputSeq: en.NextInputSeq, NextOrdinal: en.NextOrdinal,
				NextResponse: en.NextResponse, FirstHookAt: en.FirstHookAt, LastHookAt: en.LastHookAt, BareStart: en.BareStart,
				TakenEnd: en.TakenEnd})
		}
		s.mu.Unlock()
	}
	return out
}

// AdoptNumbering merges carried lifecycle evidence: into the live entry when there is one (adopted
// from open_requests, or still live in this engine), else into the tombstone, else, for a carried
// tombstone still within TombstoneTTL, as a new tombstone. It returns the rows applied.
func (e *Engine) AdoptNumbering(rows []Numbering) int {
	now := e.Now().UnixNano()
	n := 0
	for _, r := range rows {
		if r.ClosedAt != 0 && now-r.ClosedAt > int64(TombstoneTTL) {
			continue
		}
		s := e.shard(r.RequestID)
		s.mu.Lock()
		switch en, tb := s.m[r.RequestID], e.lookupClosed(s, r.RequestID, now); {
		case en != nil:
			en.NextInputSeq, en.NextOrdinal = max(en.NextInputSeq, r.NextInputSeq), max(en.NextOrdinal, r.NextOrdinal)
			en.NextResponse = max(en.NextResponse, r.NextResponse)
			en.FirstHookAt, en.LastHookAt = minNonZero(en.FirstHookAt, r.FirstHookAt), max(en.LastHookAt, r.LastHookAt)
			if en.BareStart == 0 {
				en.BareStart = r.BareStart
			}
			en.TakenEnd = max(en.TakenEnd, r.TakenEnd)
			n++
		case tb != nil:
			tb.NextInputSeq, tb.NextOrdinal = max(tb.NextInputSeq, r.NextInputSeq), max(tb.NextOrdinal, r.NextOrdinal)
			tb.NextResponse = max(tb.NextResponse, r.NextResponse)
			tb.FirstHookAt, tb.LastHookAt = minNonZero(tb.FirstHookAt, r.FirstHookAt), max(tb.LastHookAt, r.LastHookAt)
			if tb.BareStart == 0 {
				tb.BareStart = r.BareStart
			}
			tb.TakenEnd = max(tb.TakenEnd, r.TakenEnd)
			n++
		case r.ClosedAt != 0:
			e.putClosed(s, r.RequestID, &closedRID{NextInputSeq: r.NextInputSeq, NextOrdinal: r.NextOrdinal, NextResponse: r.NextResponse,
				FirstHookAt: r.FirstHookAt, LastHookAt: r.LastHookAt, BareStart: r.BareStart, TakenEnd: r.TakenEnd, ClosedAt: r.ClosedAt})
			n++
		}
		s.mu.Unlock()
	}
	return n
}

func minNonZero(a, b int64) int64 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	}
	return min(a, b)
}

func (e *Engine) admit() bool { return e.admitEvent(false) }

// admitEvent opens an admitted section for one hook call. While stopping, the call is counted
// stopped; while stopped with capture still configured on (between a stop and the next start, or a
// final stop before the config goes off) it is counted storage_unavailable, a payload chunk (chunk)
// only in full capture.
func (e *Engine) admitEvent(chunk bool) bool {
	e.inflight.Add(1)
	if s := e.state.Load(); s != StateRunning {
		e.inflight.Add(-1)
		switch cfg := e.cfg.Load(); {
		case s == StateStopping:
			e.Stats.Drop("stopped")
		case cfg.Configured && (cfg.Capture == "full" || (cfg.Capture == "metadata" && !chunk)):
			e.Stats.Drop("storage_unavailable")
		}
		return false
	}
	return true
}

// unavailable counts one hook call as storage_unavailable when cfg says capture is configured on
// but unavailable; a payload chunk (chunk) counts only in full capture, which takes chunks.
func (e *Engine) unavailable(cfg *Config, chunk bool) {
	if cfg.Unavailable == "full" || (cfg.Unavailable == "metadata" && !chunk) {
		e.Stats.Drop("storage_unavailable")
	}
}

func (e *Engine) leave() { e.inflight.Add(-1) }

func (e *Engine) shard(rid string) *shard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(rid))
	return &e.shards[h.Sum32()%numShards]
}

// With runs fn on the entry for rid under its shard lock. When create is false and no entry
// exists, fn is not called and With returns false. fn must only read and write the entry. An entry
// created for a RequestID that has a tombstone continues its numbering and consumes it.
func (e *Engine) With(rid string, create bool, fn func(*Entry)) bool {
	s := e.shard(rid)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := e.Now().UnixNano()
	en := s.m[rid]
	if en == nil {
		if !create {
			return false
		}
		en = &Entry{BASince: e.lostAt.Load()}
		if tb := e.lookupClosed(s, rid, now); tb != nil {
			seedFromClosed(en, tb)
			s.dropClosed(rid)
		}
		s.m[rid] = en
	}
	en.Touched = now
	fn(en)
	return true
}

func seedFromClosed(en *Entry, tb *closedRID) {
	en.NextInputSeq, en.NextOrdinal, en.NextResponse = tb.NextInputSeq, tb.NextOrdinal, tb.NextResponse
	en.FirstHookAt, en.LastHookAt, en.BAAt, en.BAPrevAt = tb.FirstHookAt, tb.LastHookAt, tb.BAAt, tb.BAPrevAt
	en.BASince, en.BareStart, en.TakenEnd, en.MarksPruned = tb.BASince, tb.BareStart, tb.TakenEnd, tb.MarksPruned
	en.BeforeAuthSeen = tb.BAAt != 0
	if tb.LastMark.At != 0 {
		en.Marks = []inputMark{tb.LastMark}
	}
}

// lookupClosed returns rid's unexpired tombstone, or nil. The caller holds s.mu.
func (e *Engine) lookupClosed(s *shard, rid string, now int64) *closedRID {
	tb := s.closed[rid]
	if tb == nil {
		return nil
	}
	if now-tb.ClosedAt > int64(TombstoneTTL) {
		storeMax(&e.lostAt, tb.ClosedAt)
		s.dropClosed(rid)
		return nil
	}
	return tb
}

func (s *shard) dropClosed(rid string) { delete(s.closed, rid) }

// putClosed stores tb as rid's tombstone, dropping the shard's oldest when the ring is full
// (counted numbering_evicted). The caller holds s.mu.
func (e *Engine) putClosed(s *shard, rid string, tb *closedRID) {
	if old := s.closed[rid]; old != nil {
		tb.slot = old.slot
		s.closed[rid] = tb
		return
	}
	if victim := s.ring[s.head]; victim != "" {
		if v := s.closed[victim]; v != nil && v.slot == s.head {
			storeMax(&e.lostAt, v.ClosedAt)
			delete(s.closed, victim)
			e.Stats.Count("numbering_evicted", 1)
		}
	}
	tb.slot = s.head
	s.ring[s.head] = rid
	s.head = (s.head + 1) % ClosedPerShard
	s.closed[rid] = tb
}

func closedOf(en *Entry, now int64) *closedRID {
	tb := &closedRID{NextInputSeq: en.NextInputSeq, NextOrdinal: en.NextOrdinal, NextResponse: en.NextResponse,
		FirstHookAt: en.FirstHookAt, LastHookAt: en.LastHookAt, BAAt: en.BAAt, BAPrevAt: en.BAPrevAt, BASince: en.BASince,
		BareStart: en.BareStart, TakenEnd: en.TakenEnd, MarksPruned: en.MarksPruned, ClosedAt: now}
	if n := len(en.Marks); n > 0 {
		tb.LastMark = en.Marks[n-1]
		for _, m := range en.Marks[:n-1] {
			tb.MarksPruned = max(tb.MarksPruned, m.At)
		}
	}
	return tb
}

// close deletes rid's entry and leaves its tombstone. The caller holds s.mu.
func (e *Engine) closeEntry(s *shard, rid string, en *Entry, now int64) {
	delete(s.m, rid)
	e.putClosed(s, rid, closedOf(en, now))
}

// noteHook records a synchronous hook's start in the entry's lifecycle evidence.
func noteHook(en *Entry, at int64) {
	if en.FirstHookAt == 0 || at < en.FirstHookAt {
		en.FirstHookAt = at
	}
	en.LastHookAt = max(en.LastHookAt, at)
}

// noteBeforeAuth records a before-auth that assigned input_seq seq: its mark, BAAt/BAPrevAt and
// its hook time.
func (e *Engine) noteBeforeAuth(en *Entry, at int64, seq uint32) {
	en.Marks = append(en.Marks, inputMark{Seq: seq, At: at, PrevHookAt: en.LastHookAt, BlindAt: e.blindAt.Load()})
	if len(en.Marks) > maxMarks {
		en.MarksPruned = max(en.MarksPruned, en.Marks[1].At)
		en.Marks = append(en.Marks[:1], en.Marks[2:]...)
	}
	switch {
	case at >= en.BAAt:
		en.BAPrevAt, en.BAAt = en.BAAt, at
	case at > en.BAPrevAt:
		en.BAPrevAt = at
	}
	noteHook(en, at)
}

// evidence fills a stream item's boundary evidence from the entry, under its lock.
func (e *Engine) evidence(en *Entry, it *StreamItem) {
	it.BAAt, it.BAPrevAt = en.BAAt, en.BAPrevAt
	it.Since = max(en.BASince, e.blindAt.Load(), e.admittedSince.Load())
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
// sweep for requests whose completion never arrived), leaving a tombstone for each so a completion
// delivered later still finds its RequestID's lifecycle evidence. It also drops expired tombstones.
// Entries hold no pool bytes themselves; the stream goroutine sweeps its accumulators.
func (e *Engine) Sweep(cutoff time.Time) int {
	c := cutoff.UnixNano()
	now := e.Now().UnixNano()
	n := 0
	for i := range e.shards {
		s := &e.shards[i]
		s.mu.Lock()
		for k, en := range s.m {
			if en.Touched < c {
				e.closeEntry(s, k, en, now)
				n++
			}
		}
		for k, tb := range s.closed {
			if now-tb.ClosedAt > int64(TombstoneTTL) {
				storeMax(&e.lostAt, tb.ClosedAt)
				delete(s.closed, k)
			}
		}
		s.mu.Unlock()
	}
	if n > 0 {
		e.Stats.Count("state_evicted", uint64(n))
		storeMax(&e.blindAt, now)
	}
	return n
}

// Tombstones returns the number of unexpired tombstones (tests and status).
func (e *Engine) Tombstones() int {
	now := e.Now().UnixNano()
	n := 0
	for i := range e.shards {
		s := &e.shards[i]
		s.mu.Lock()
		for _, tb := range s.closed {
			if now-tb.ClosedAt <= int64(TombstoneTTL) {
				n++
			}
		}
		s.mu.Unlock()
	}
	return n
}

// WorkDone is called by a worker when a Handoff item finished (written or not). It returns the
// deferred completions, in started_at order, when this was the last pending item.
func (e *Engine) WorkDone(rid string, begin, written bool) []*Done {
	var d []*Done
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
		if en.Pending == 0 && len(en.Deferred) > 0 {
			d, en.Deferred = en.Deferred, nil
		}
	})
	sort.SliceStable(d, func(i, j int) bool { return d[i].Req.StartedAt.Before(d[j].Req.StartedAt) })
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
	// Key is the completion's envelope key: the bare ExchangeCompletionKey, or, for a later
	// lifecycle of a reused RequestID, ExchangeCompletionKeyAt(started_at) (Reused). AltKey is the
	// durable fallback of a bare key: the outbox or the archive moves the completion there when
	// the bare key already holds another lifecycle's completion.
	Key, AltKey string
	Reused      bool
	// LifecycleInputSeq is the input_seq of the before-auth that provably opened this lifecycle
	// (>= 1), else 0.
	LifecycleInputSeq uint32
}

// SetHeld records how many stream segments the sequencer still holds for rid that no taken
// completion claimed. A completion taken while it is non-zero keeps the entry open.
func (e *Engine) SetHeld(rid string, n uint32) {
	s := e.shard(rid)
	s.mu.Lock()
	if en := s.m[rid]; en != nil {
		en.Held = n
	}
	s.mu.Unlock()
}

// TakeCompletion returns the completion's state. With no Handoff work pending it takes the
// completion (take); otherwise it defers d until the last WorkDone and returns ok=false.
func (e *Engine) TakeCompletion(d *Done) (CompletionState, bool) {
	rid := d.Req.RequestID
	s := e.shard(rid)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := e.Now().UnixNano()
	if en := s.m[rid]; en != nil && en.Pending > 0 {
		en.Touched = now
		en.Deferred = append(en.Deferred, d)
		return CompletionState{}, false
	}
	return e.take(s, rid, d, now), true
}

// FinishDeferred takes a deferred completion once its Handoff work finished.
func (e *Engine) FinishDeferred(d *Done) CompletionState {
	rid := d.Req.RequestID
	s := e.shard(rid)
	s.mu.Lock()
	defer s.mu.Unlock()
	return e.take(s, rid, d, e.Now().UnixNano())
}

// take keys a completion and closes its entry, under the shard lock. With no live entry the key
// comes from the RequestID's tombstone (numbering untouched). The entry stays open when a later
// lifecycle of the RequestID is active or the sequencer still holds a segment no completion
// claimed; otherwise it is deleted and leaves a tombstone.
func (e *Engine) take(s *shard, rid string, d *Done, now int64) CompletionState {
	c := d.Req.CompletedAt
	en := s.m[rid]
	if en == nil {
		var cs CompletionState
		tb := e.lookupClosed(s, rid, now)
		if tb == nil {
			cs.Key, cs.AltKey, cs.Reused = completionKey(rid, 0, nil, d)
			return cs
		}
		cs.Key, cs.AltKey, cs.Reused = completionKey(rid, tb.FirstHookAt, &tb.BareStart, d)
		if !c.IsZero() {
			tb.TakenEnd = max(tb.TakenEnd, c.UnixNano())
		}
		return cs
	}
	en.Touched = now
	cs := completionState(en)
	cs.Key, cs.AltKey, cs.Reused = completionKey(rid, en.FirstHookAt, &en.BareStart, d)
	cs.LifecycleInputSeq = e.opener(en, d)
	later := laterLifecycle(en, d)
	if !c.IsZero() {
		en.TakenEnd = max(en.TakenEnd, c.UnixNano())
	}
	if later || en.Held > 0 {
		// Keep the entry for the later lifecycle; the body counters are per lifecycle.
		en.BeginWritten, en.Responses, en.HookMicros = false, 0, [numHooks]uint32{}
		if !c.IsZero() {
			cut := c.UnixNano()
			kept := en.Marks[:0]
			for _, m := range en.Marks {
				if m.At > cut {
					kept = append(kept, m)
				} else {
					en.MarksPruned = max(en.MarksPruned, m.At)
				}
			}
			en.Marks = kept
		}
		return cs
	}
	e.closeEntry(s, rid, en, now)
	return cs
}

// laterLifecycle reports whether the entry shows a lifecycle after d's: a hook that started after
// d's CompletedAt (no hook of d's own lifecycle can, F1) and after every completion already taken,
// or a stream item queued after d's completion hook and not yet processed.
func laterLifecycle(en *Entry, d *Done) bool {
	c := d.Req.CompletedAt
	return (!c.IsZero() && en.LastHookAt > max(c.UnixNano(), en.TakenEnd)) || en.ChunksQueued > en.ChunksDone
}

// completionKey returns d's key, its durable fallback and whether the key is non-bare. The bare key
// is used when nothing known shows an earlier lifecycle: no hook of the RequestID started before
// d's started_at, and no other completion took the bare key. bareStart records the taker.
func completionKey(rid string, firstHookAt int64, bareStart *int64, d *Done) (key, alt string, reused bool) {
	bare := wire.ExchangeCompletionKey(rid)
	st := d.Req.StartedAt
	if st.IsZero() {
		return bare, "", false
	}
	at := wire.ExchangeCompletionKeyAt(rid, st)
	if !wire.ValidKey(at) {
		return bare, "", false // a RequestID too long for the suffix: 0.1.4's key
	}
	sn := st.UnixNano()
	earlier := firstHookAt != 0 && firstHookAt < sn
	taken := bareStart != nil && *bareStart != 0 && *bareStart != sn
	if earlier || taken {
		return at, "", true
	}
	if bareStart != nil {
		*bareStart = sn
	}
	return bare, at, false
}

// opener returns the input_seq of the before-auth that provably opened d's lifecycle, or 0. The
// first plugin call of a lifecycle is its handler-level before-auth (F6); the checks prove that
// any hook of the RequestID between started_at and that mark was observed and that no mark after
// started_at was pruned. A duplex follow-up always follows its lifecycle's after-auth, so it never
// qualifies.
func (e *Engine) opener(en *Entry, d *Done) uint32 {
	if d.Req.StartedAt.IsZero() {
		return 0
	}
	sn := d.Req.StartedAt.UnixNano()
	var m *inputMark
	for i := range en.Marks {
		if x := &en.Marks[i]; x.At > sn && (m == nil || x.At < m.At) {
			m = x
		}
	}
	if m == nil || en.MarksPruned >= sn || m.PrevHookAt >= sn || m.BlindAt >= sn || e.admittedSince.Load() > sn || m.Seq < 1 {
		return 0
	}
	return m.Seq
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
