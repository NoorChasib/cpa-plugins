// Package capture is the worker side of the plugin: it turns hook work items into
// masked, canonical envelopes and queues them in the outbox. Goroutines: the workers (splits and
// usage), the single stream goroutine (line assembly, reassembly, attempts, metadata-only items,
// completions) and the single writer (grouped outbox transactions). Every goroutine runs behind
// safego, and nothing unmasked ever reaches the outbox.
package capture

import (
	"container/list"
	"sync"
	"sync/atomic"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// MaskerState is one masker with the exact-set revision it was built from, and its view of the
// masking memo. A split takes all three from one state, so what it reads from and adds to the
// memo always belongs to the masker it masks with.
type MaskerState struct {
	Masker *mask.Masker
	Rev    uint32
	Memo   *MemoView
}

// MaskerHolder holds the current masker and the masking memo. Memo entries
// are stamped with the masker that produced them: a split still running with a replaced masker
// can add entries after the swap, and those must never be served to the new masker, which may
// know exact values the old one did not.
type MaskerHolder struct {
	cur  atomic.Pointer[MaskerState]
	Memo *Memo
}

// NewMaskerHolder returns a holder with m and an empty memo of memoBytes.
func NewMaskerHolder(m *mask.Masker, rev uint32, memoBytes int64) *MaskerHolder {
	h := &MaskerHolder{Memo: NewMemo(memoBytes)}
	h.cur.Store(&MaskerState{Masker: m, Rev: rev, Memo: &MemoView{memo: h.Memo, owner: m}})
	return h
}

// Get returns the current masker.
func (h *MaskerHolder) Get() *MaskerState { return h.cur.Load() }

// Set installs a new masker and clears the memo. Every swap clears it, whatever the revision:
// two maskers of the same revision can still differ (the local-only set and the server set both
// start at revision 0).
func (h *MaskerHolder) Set(m *mask.Masker, rev uint32) {
	h.cur.Store(&MaskerState{Masker: m, Rev: rev, Memo: &MemoView{memo: h.Memo, owner: m}})
	h.Memo.Clear()
}

// MemoView is one masker's view of the shared memo (canon.Memo): Get misses entries another
// masker added, and Add stamps entries with this view's masker.
type MemoView struct {
	memo  *Memo
	owner *mask.Masker
}

// Get implements canon.Memo.
func (v *MemoView) Get(k wire.Hash) (string, mask.Hits, bool) { return v.memo.get(k, v.owner) }

// Add implements canon.Memo.
func (v *MemoView) Add(k wire.Hash, s string, hits mask.Hits) { v.memo.add(k, s, hits, v.owner) }

// Memo is the masking memo LRU behind every MemoView, keyed by sha256 of the unmasked string and bounded
// by bytes. It lives in memory only and is never persisted.
type Memo struct {
	mu     sync.Mutex
	max    int64
	bytes  int64
	m      map[wire.Hash]*list.Element
	l      *list.List
	Hits   atomic.Uint64
	Misses atomic.Uint64
}

type memoEntry struct {
	k     wire.Hash
	v     string
	hits  mask.Hits
	size  int64
	owner *mask.Masker // the masker that produced v
}

// NewMemo returns an empty memo bounded by maxBytes.
func NewMemo(maxBytes int64) *Memo {
	return &Memo{max: maxBytes, m: map[wire.Hash]*list.Element{}, l: list.New()}
}

func (m *Memo) get(k wire.Hash, owner *mask.Masker) (string, mask.Hits, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.m[k]; ok && e.Value.(*memoEntry).owner == owner {
		m.l.MoveToFront(e)
		m.Hits.Add(1)
		me := e.Value.(*memoEntry)
		return me.v, me.hits, true
	}
	m.Misses.Add(1)
	return "", nil, false
}

func (m *Memo) add(k wire.Hash, v string, hits mask.Hits, owner *mask.Masker) {
	size := int64(len(v)) + 96
	if size > m.max/8 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.m[k]; ok {
		if e.Value.(*memoEntry).owner == owner {
			return
		}
		// A stale entry of a replaced masker: replace it.
		me := e.Value.(*memoEntry)
		m.l.Remove(e)
		delete(m.m, k)
		m.bytes -= me.size
	}
	m.m[k] = m.l.PushFront(&memoEntry{k: k, v: v, hits: hits, size: size, owner: owner})
	m.bytes += size
	for m.bytes > m.max {
		e := m.l.Back()
		if e == nil {
			break
		}
		me := e.Value.(*memoEntry)
		m.l.Remove(e)
		delete(m.m, me.k)
		m.bytes -= me.size
	}
}

// Clear empties the memo.
func (m *Memo) Clear() {
	m.mu.Lock()
	m.m = map[wire.Hash]*list.Element{}
	m.l.Init()
	m.bytes = 0
	m.mu.Unlock()
}
