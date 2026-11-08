package capture

import (
	"container/list"
	"crypto/sha256"
	"net/http"
	"strings"
	"sync"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// Prefix-delta parameters: exchanges kept per conversation key, the keyframe interval, the memory
// cap of the id lists and the per-socket response map size.
const (
	prefixKeep      = 4
	prefixKeyframe  = 32
	prefixMaxBytes  = 32 << 20
	prefixMaxByResp = 4096
)

// PrefixMap holds, per conversation key, the last committed exchanges with their S(X) id lists
// (input item ids, then the first response's item ids). Records enter only after the outbox committed their begin, so a base is always
// in the same outbox; a dead-lettered base is removed.
type PrefixMap struct {
	mu      sync.Mutex
	convs   map[wire.Hash]*conv
	lru     *list.List
	bytes   int64
	byBegin map[string]*exRec
	byResp  map[string]*exRec // execution session + response id (native turns)
	byRKey  map[string]*exRec // response envelope key (acks)
}

type conv struct {
	key   wire.Hash
	recs  []*exRec
	count uint64
	elem  *list.Element
}

// exRec is one committed exchange. It becomes a base only once the archive acked its begin ok or
// duplicate, and its response items extend S only once that response was acked too: a begin the
// archive rejects (and a later remask rewrites with new item ids) is then never referenced, so no
// later exchange's Base points at item ids the archive will never hold.
type exRec struct {
	beginKey  string
	ids       []wire.Hash // S(X): input ids, then the response's ids once it is acked
	inputN    int         // input ids at the front of ids
	resp      int32       // response index of respIDs; -1 none committed yet
	respIDs   []wire.Hash
	respKey   string // envelope key of that response
	acked     bool   // the begin was acked ok or duplicate
	respAcked bool
	droppedAt uint64
	conv      *conv
}

// NewPrefixMap returns an empty map.
func NewPrefixMap() *PrefixMap {
	return &PrefixMap{convs: map[wire.Hash]*conv{}, lru: list.New(), byBegin: map[string]*exRec{}, byResp: map[string]*exRec{}, byRKey: map[string]*exRec{}}
}

// ConvKey is the conversation key: (caller_fp, primary session id, agent id, source_format).
// Raw header values are only hashed here, never kept.
func ConvKey(callerFP, sourceFormat, execSession string, h http.Header, ids []wire.Hash) wire.Hash {
	session, agent := "", ""
	get := func(names ...string) string {
		for _, n := range names {
			for k, v := range h {
				if strings.EqualFold(k, n) && len(v) > 0 && v[0] != "" {
					return v[0]
				}
			}
		}
		return ""
	}
	if s := get("x-claude-code-session-id"); s != "" {
		session, agent = "cc:"+s, get("x-claude-code-agent-id")
	} else if s := get("session_id", "session-id", "thread-id", "thread_id"); s != "" {
		session = "codex:" + s
	} else if s := get("conversation_id", "x-conversation-id", "x-session-id", "x-thread-id"); s != "" {
		session = "conv:" + s
	} else if execSession != "" {
		session = "ws:" + execSession
	} else {
		x := sha256.New()
		for i := 0; i < len(ids) && i < 2; i++ {
			x.Write(ids[i][:])
		}
		session = "root:" + string(x.Sum(nil))
	}
	k := sha256.New()
	for _, p := range []string{callerFP, session, agent, sourceFormat} {
		k.Write([]byte(p))
		k.Write([]byte{0})
	}
	var out wire.Hash
	copy(out[:], k.Sum(nil))
	return out
}

// Delta is a chosen encoding for one begin.
type Delta struct {
	Base     *wire.Base
	From     int         // index into the split's items where the envelope's Items start
	Expanded []wire.Hash // all N item ids after expansion (ListSHA input, the new S)
}

func respKey(execSession, responseID string) string { return execSession + "\x00" + responseID }

// Choose picks the base for a begin with item ids ids. prevRespID and execSession
// select a Responses native turn's base; droppedTotal is the current sum of dropped counters.
func (p *PrefixMap) Choose(key wire.Hash, ids []wire.Hash, execSession, prevRespID string, droppedTotal uint64) Delta {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.convs[key]
	if c != nil {
		p.lru.MoveToFront(c.elem)
	}
	keyframe := c == nil || c.count%prefixKeyframe == 0
	if c != nil {
		c.count++
	}
	if execSession != "" && prevRespID != "" {
		if r := p.byResp[respKey(execSession, prevRespID)]; r != nil && r.acked && r.respAcked && r.droppedAt == droppedTotal {
			exp := make([]wire.Hash, 0, len(r.ids)+len(ids))
			exp = append(append(exp, r.ids...), ids...)
			return Delta{Base: &wire.Base{Key: r.beginKey, Resp: r.resp, Kept: uint32(len(r.ids))}, From: 0, Expanded: exp}
		}
	}
	full := Delta{Expanded: ids}
	if keyframe || c == nil {
		return full
	}
	var best *exRec
	bestK := 0
	for _, r := range c.recs {
		if !r.acked {
			continue
		}
		if k := canon.LCP(ids, r.ids); k > bestK {
			best, bestK = r, k
		}
	}
	if best == nil || bestK == 0 || bestK*2 < len(ids) || best.droppedAt != droppedTotal {
		return full
	}
	resp := int32(-1)
	if best.respAcked {
		resp = best.resp
	}
	return Delta{Base: &wire.Base{Key: best.beginKey, Resp: resp, Kept: uint32(bestK)}, From: bestK, Expanded: ids}
}

// Committed records a begin the outbox committed, with its expanded input ids.
func (p *PrefixMap) Committed(key wire.Hash, beginKey string, ids []wire.Hash, droppedAt uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.convs[key]
	if c == nil {
		c = &conv{key: key, count: 1}
		c.elem = p.lru.PushFront(c)
		p.convs[key] = c
	}
	r := &exRec{beginKey: beginKey, ids: append([]wire.Hash(nil), ids...), inputN: len(ids), resp: -1, droppedAt: droppedAt, conv: c}
	c.recs = append(c.recs, r)
	p.bytes += int64(len(ids)) * 32
	p.byBegin[beginKey] = r
	for len(c.recs) > prefixKeep {
		p.dropRec(c.recs[0])
	}
	for p.bytes > prefixMaxBytes {
		e := p.lru.Back()
		if e == nil {
			break
		}
		old := e.Value.(*conv)
		for len(old.recs) > 0 {
			p.dropRec(old.recs[0])
		}
		p.lru.Remove(e)
		delete(p.convs, old.key)
	}
}

// ResponseCommitted extends S(begin) with the response's item ids once the response is
// committed, and indexes it by response id for native turns.
func (p *PrefixMap) ResponseCommitted(beginKey, responseKey string, index uint32, ids []wire.Hash, execSession, responseID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.byBegin[beginKey]
	if r == nil {
		return
	}
	if r.resp < 0 {
		r.respIDs = append([]wire.Hash(nil), ids...)
		r.resp = int32(index)
		r.respKey = responseKey
		p.byRKey[responseKey] = r
		p.bytes += int64(len(ids)) * 32
	}
	if execSession != "" && responseID != "" {
		if len(p.byResp) >= prefixMaxByResp {
			for k := range p.byResp {
				delete(p.byResp, k)
				break
			}
		}
		p.byResp[respKey(execSession, responseID)] = r
	}
}

// Acked records that the archive acked the envelope with key ok or duplicate (a conflict holds
// another body, so it never counts). A begin becomes a usable base; a response extends its
// begin's S(X).
func (p *PrefixMap) Acked(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r := p.byBegin[key]; r != nil {
		r.acked = true
		return
	}
	if r := p.byRKey[key]; r != nil && !r.respAcked {
		r.respAcked = true
		r.ids = append(r.ids[:r.inputN:r.inputN], r.respIDs...)
		r.respIDs = r.ids[r.inputN:] // one copy of the response ids
	}
}

// Dead removes a dead-lettered begin from the map.
func (p *PrefixMap) Dead(beginKey string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r := p.byBegin[beginKey]; r != nil {
		p.dropRec(r)
	}
}

func (p *PrefixMap) dropRec(r *exRec) {
	c := r.conv
	for i, x := range c.recs {
		if x == r {
			c.recs = append(c.recs[:i], c.recs[i+1:]...)
			break
		}
	}
	p.bytes -= int64(r.inputN+len(r.respIDs)) * 32
	delete(p.byBegin, r.beginKey)
	if r.respKey != "" {
		delete(p.byRKey, r.respKey)
	}
	for k, v := range p.byResp {
		if v == r {
			delete(p.byResp, k)
		}
	}
}

// Bytes returns the id-list bytes held.
func (p *PrefixMap) Bytes() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bytes
}
