package importer

import (
	"context"
	"crypto/sha256"
	"sort"
	"sync"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// memoMin and memoBudget: the per-run masking memo keeps strings of at least 1 KiB (as canon's memo
// does), up to 64 MiB of masked text; when full it is cleared. It lives in memory only and dies with
// the run.
const (
	memoMin    = 1 << 10
	memoBudget = 64 << 20
)

type memoEntry struct {
	out  string
	hits mask.Hits
}

// memo implements canon.Memo and the importer's own string masking.
type memo struct {
	mu     sync.Mutex
	m      map[wire.Hash]memoEntry
	bytes  int
	hits   uint64
	misses uint64
}

var _ canon.Memo = (*memo)(nil)

func newMemo() *memo { return &memo{m: map[wire.Hash]memoEntry{}} }

func (mm *memo) Get(key wire.Hash) (string, mask.Hits, bool) {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	e, ok := mm.m[key]
	if ok {
		mm.hits++
	} else {
		mm.misses++
	}
	return e.out, e.hits, ok
}

func (mm *memo) Add(key wire.Hash, out string, hits mask.Hits) {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	if mm.bytes+len(out) > memoBudget {
		mm.m = map[wire.Hash]memoEntry{}
		mm.bytes = 0
	}
	if _, ok := mm.m[key]; !ok {
		mm.m[key] = memoEntry{out: out, hits: hits}
		mm.bytes += len(out)
	}
}

// mask masks s, through the memo when s is at least memoMin bytes.
func (mm *memo) mask(ctx context.Context, m *mask.Masker, s string, st *runStats) (string, error) {
	if len(s) < memoMin {
		out, hits, err := m.StringContext(ctx, s)
		if err != nil {
			return "", err
		}
		st.addMasked(int64(len(s)), hits)
		return out, nil
	}
	key := wire.Hash(sha256.Sum256([]byte(s)))
	if out, hits, ok := mm.Get(key); ok {
		st.addHits(hits)
		return out, nil
	}
	out, hits, err := m.StringContext(ctx, s)
	if err != nil {
		return "", err
	}
	st.addMasked(int64(len(s)), hits)
	mm.Add(key, out, hits)
	return out, nil
}

// runStats are one run's counters: counts and timings only, for the heartbeat and `run` output.
type runStats struct {
	mu        sync.Mutex
	files     int64
	lines     int64
	records   int64
	bytesRead int64
	envelopes int64
	maskBytes int64
	matches   map[string]uint64
	dropped   map[string]uint64
	durations []time.Duration // per record build
	statuses  map[string]int64
}

func newRunStats() *runStats {
	return &runStats{matches: map[string]uint64{}, dropped: map[string]uint64{}, statuses: map[string]int64{}}
}

func (s *runStats) addMasked(n int64, hits mask.Hits) {
	s.mu.Lock()
	s.maskBytes += n
	for k, v := range hits {
		s.matches[k] += uint64(v)
	}
	s.mu.Unlock()
}

func (s *runStats) addHits(hits mask.Hits) {
	if len(hits) == 0 {
		return
	}
	s.mu.Lock()
	for k, v := range hits {
		s.matches[k] += uint64(v)
	}
	s.mu.Unlock()
}

func (s *runStats) drop(name string) {
	s.mu.Lock()
	s.dropped[name]++
	s.mu.Unlock()
}

func (s *runStats) status(name string) {
	s.mu.Lock()
	s.statuses[name]++
	s.mu.Unlock()
}

func (s *runStats) took(d time.Duration) {
	s.mu.Lock()
	if len(s.durations) < 1<<20 {
		s.durations = append(s.durations, d)
	}
	s.mu.Unlock()
}

// percentiles returns the p50 and p99 record build time in milliseconds.
func (s *runStats) percentiles() (p50, p99 float64) {
	s.mu.Lock()
	d := append([]time.Duration(nil), s.durations...)
	s.mu.Unlock()
	if len(d) == 0 {
		return 0, 0
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	at := func(q float64) float64 {
		i := int(q * float64(len(d)-1))
		return float64(d[i].Microseconds()) / 1000
	}
	return at(0.50), at(0.99)
}
