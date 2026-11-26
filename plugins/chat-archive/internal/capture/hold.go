package capture

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
)

// The write path's hold (K10, plugin 0.1.6). While the outbox stalls, the writer keeps its group
// and the envelopes behind it wait in the write channel, deep enough for a 60 s stall at about 90
// envelopes/s with two 640-envelope bursts. The hold budget bounds the bytes those envelopes keep
// beyond what another budget already charges (the content pool for bodies, the usage budget for
// usage records), so the deeper channel adds a stated, enforced amount of memory.
const (
	// WriteQueueEntries is the write channel's depth (1,024 before 0.1.6).
	WriteQueueEntries = 16384
	// HoldBytes is the hold budget.
	HoldBytes = 64 << 20
	// HoldEnvelopeMax is the most one envelope may hold. It is half of HoldBytes, so an admissible
	// envelope always fits once the hold drains, and FIFO admission keeps it from being starved. A
	// begin or response over it is written as metadata only (body_budget); an envelope over it even
	// then is not written (hold_oversize).
	HoldEnvelopeMax = 32 << 20
)

// Retained-byte estimate: a fixed overhead per envelope (its writeReq slot is counted with the
// channel; this covers the closures, the outbox's envelope and the commit callback's struct), and
// one per piece (the canon.Piece struct and its slice slot).
const (
	envelopeOverhead = 512
	pieceOverhead    = 96
)

// retainedBytes estimates what a queued envelope keeps alive: its keys, the body JSON's capacity,
// the refs' capacity, each piece's bytes and content type with its overhead, and extra (the commit
// callback's data). A piece's bytes are counted by length: pieces of one split may share a backing
// array, so their length is counted with an eighth more for the allocator's size classes.
// TestHeldBytesCoversAllocation pins that the estimate covers the measured heap per envelope.
func retainedBytes(env outbox.NewEnvelope, extra int64) int64 {
	n := int64(envelopeOverhead+len(env.Key)+len(env.AltKey)+cap(env.Body)+32*cap(env.Refs)+pieceOverhead*cap(env.Pieces)) + extra
	for _, p := range env.Pieces {
		n += int64(len(p.Bytes) + len(p.Bytes)/8 + len(p.ContentType))
	}
	return n
}

// heldBytes is what an envelope holds from the hold budget: what it retains beyond carried, the
// budget its release returns.
func heldBytes(env outbox.NewEnvelope, extra, carried int64) int64 {
	return max(retainedBytes(env, extra)-carried, 0)
}

// holdBudget is a byte budget with FIFO admission: a reservation waits while an earlier one waits
// or while it does not fit. A reservation of 0 bytes never waits. Nothing over HoldEnvelopeMax is
// ever asked for, so the head of the queue always fits once the hold drains.
type holdBudget struct {
	mu       sync.Mutex
	capacity int64
	used     int64
	queue    []*holdWaiter
	peak     atomic.Int64
	now      atomic.Int64 // used, for readers that do not take mu
}

type holdWaiter struct {
	n       int64
	ready   chan struct{}
	granted bool
}

func newHoldBudget(capacity int64) *holdBudget { return &holdBudget{capacity: capacity} }

// acquire reserves n bytes, waiting in FIFO order until they fit or ctx ends. It reports whether
// the bytes are reserved.
func (h *holdBudget) acquire(ctx context.Context, n int64) bool {
	if n <= 0 {
		return true
	}
	h.mu.Lock()
	if len(h.queue) == 0 && h.used+n <= h.capacity {
		h.grantLocked(n)
		h.mu.Unlock()
		return true
	}
	w := &holdWaiter{n: n, ready: make(chan struct{})}
	h.queue = append(h.queue, w)
	h.mu.Unlock()
	select {
	case <-w.ready:
		return true
	case <-ctx.Done():
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if w.granted {
		return true // granted as ctx ended: the caller owns the bytes and returns them
	}
	for i, q := range h.queue {
		if q == w {
			h.queue = append(h.queue[:i], h.queue[i+1:]...)
			break
		}
	}
	h.wakeLocked() // the waiter behind this one may fit now
	return false
}

// release returns n bytes and admits the waiters that now fit, in order.
func (h *holdBudget) release(n int64) {
	if n <= 0 {
		return
	}
	h.mu.Lock()
	h.used -= n
	h.now.Store(h.used)
	h.wakeLocked()
	h.mu.Unlock()
}

func (h *holdBudget) grantLocked(n int64) {
	h.used += n
	h.now.Store(h.used)
	for {
		p := h.peak.Load()
		if h.used <= p || h.peak.CompareAndSwap(p, h.used) {
			return
		}
	}
}

func (h *holdBudget) wakeLocked() {
	for len(h.queue) > 0 && h.used+h.queue[0].n <= h.capacity {
		w := h.queue[0]
		h.queue = h.queue[1:]
		h.grantLocked(w.n)
		w.granted = true
		close(w.ready)
	}
}

// waiting returns how many reservations wait.
func (h *holdBudget) waiting() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.queue)
}

// HoldBytes returns the bytes the hold budget has reserved now and at its peak.
func (rt *Runtime) HoldBytes() (now, peak int64) { return rt.hold.now.Load(), rt.hold.peak.Load() }

// WriteQueue returns the write channel's length now and at its peak.
func (rt *Runtime) WriteQueue() (now, peak int) { return len(rt.writes), int(rt.queuePeak.Load()) }

func (rt *Runtime) noteQueue() {
	l := int64(len(rt.writes))
	for {
		p := rt.queuePeak.Load()
		if l <= p || rt.queuePeak.CompareAndSwap(p, l) {
			return
		}
	}
}
