package hooks

import (
	"math/bits"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Hook identifies one synchronous or callback hook (heartbeat last_hook_at names).
type Hook int

// Hooks.
const (
	HookBeforeAuth Hook = iota
	HookAfterAuth
	HookResponse
	HookStreamHeaderInit
	HookStreamChunk
	HookLifecycle
	HookUsage
	HookManagement
	numHooks
)

var hookNames = [numHooks]string{"before_auth", "after_auth", "response", "stream_header_init", "stream_chunk", "lifecycle", "usage", "management"}

func (h Hook) String() string { return hookNames[h] }

// Dropped counter names, as the heartbeat and status report them.
var DroppedNames = []string{"body_budget", "handoff_full", "control_full", "usage_queue", "stream_overflow",
	"mask_timeout", "oversize", "oversize_rpc", "parse_error", "disk_guard", "outbox_cap", "disk_hard",
	"encode_error", "stopped", "media_fetch"}

// OtherNames are plugin counters that are not drops.
var OtherNames = []string{"duplicate_key", "state_evicted", "ignored_model_list"}

// Stats holds every counter the plugin reports. Counters are cumulative for the plugin instance
// (one boot_id). Hook-side updates are atomics only.
type Stats struct {
	calls    [numHooks]atomic.Uint64
	errors   [numHooks]atomic.Uint64
	lastHook [numHooks]atomic.Int64
	lat      [numHooks]histogram
	lastUse  atomic.Int64
	dropped  map[string]*atomic.Uint64
	other    map[string]*atomic.Uint64

	mu         sync.Mutex
	panics     map[string]uint64
	panicTypes map[string]string
	faults     map[string]bool
}

// NewStats returns zeroed counters.
func NewStats() *Stats {
	s := &Stats{dropped: map[string]*atomic.Uint64{}, other: map[string]*atomic.Uint64{}, panics: map[string]uint64{}, panicTypes: map[string]string{}, faults: map[string]bool{}}
	for _, n := range DroppedNames {
		s.dropped[n] = new(atomic.Uint64)
	}
	for _, n := range OtherNames {
		s.other[n] = new(atomic.Uint64)
	}
	return s
}

// Drop increments dropped.<name>; unknown names are ignored.
func (s *Stats) Drop(name string) {
	if c := s.dropped[name]; c != nil {
		c.Add(1)
	}
}

// Count increments a non-drop counter.
func (s *Stats) Count(name string, n uint64) {
	if c := s.other[name]; c != nil {
		c.Add(n)
	}
}

// Dropped returns a snapshot of dropped counters.
func (s *Stats) Dropped() map[string]uint64 {
	out := make(map[string]uint64, len(s.dropped))
	for k, v := range s.dropped {
		out[k] = v.Load()
	}
	return out
}

// DroppedTotal is the sum of every dropped counter (prefix-delta keyframe rule).
func (s *Stats) DroppedTotal() uint64 {
	var t uint64
	for _, v := range s.dropped {
		t += v.Load()
	}
	return t
}

// Other returns a snapshot of the non-drop counters.
func (s *Stats) Other() map[string]uint64 {
	out := make(map[string]uint64, len(s.other))
	for k, v := range s.other {
		out[k] = v.Load()
	}
	return out
}

// Call records one hook call: count, last time, latency.
func (s *Stats) Call(h Hook, start time.Time, d time.Duration) {
	s.calls[h].Add(1)
	s.lastHook[h].Store(start.UnixNano())
	s.lat[h].add(d)
}

// HookError counts one handled internal error.
func (s *Stats) HookError(h Hook) { s.errors[h].Add(1) }

// UsageSeen records the time of the last usage record.
func (s *Stats) UsageSeen(t time.Time) { s.lastUse.Store(t.UnixNano()) }

// Panic records one recovered panic by goroutine or hook name and the panic value's type
// (fmt %T), never its value, which could hold content or a secret.
func (s *Stats) Panic(name, typ string) {
	s.mu.Lock()
	s.panics[name]++
	if len(typ) > 128 {
		typ = typ[:128]
	}
	s.panicTypes[name] = typ
	s.faults["recovered_panic"] = true
	s.mu.Unlock()
}

// PanicTypes returns the last recovered panic type per name.
func (s *Stats) PanicTypes() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.panicTypes))
	for k, v := range s.panicTypes {
		out[k] = v
	}
	return out
}

// SetFault raises or clears a named fault.
func (s *Stats) SetFault(name string, on bool) {
	s.mu.Lock()
	if on {
		s.faults[name] = true
	} else {
		delete(s.faults, name)
	}
	s.mu.Unlock()
}

// Faults returns the active faults, sorted.
func (s *Stats) Faults() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.faults))
	for k := range s.faults {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// HasFault reports whether a fault is active.
func (s *Stats) HasFault(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.faults[name]
}

// Panics returns recovered panics by name.
func (s *Stats) Panics() map[string]uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]uint64, len(s.panics))
	for k, v := range s.panics {
		out[k] = v
	}
	return out
}

// HookSnapshot is the per-hook part of status and heartbeats.
type HookSnapshot struct {
	Calls       map[string]uint64
	Errors      map[string]uint64
	LastHookAt  map[string]time.Time
	P50us       map[string]uint32
	P99us       map[string]uint32
	LastUsageAt *time.Time
}

// Hooks returns the per-hook counters.
func (s *Stats) Hooks() HookSnapshot {
	hs := HookSnapshot{Calls: map[string]uint64{}, Errors: map[string]uint64{}, LastHookAt: map[string]time.Time{},
		P50us: map[string]uint32{}, P99us: map[string]uint32{}}
	for h := Hook(0); h < numHooks; h++ {
		n := hookNames[h]
		hs.Calls[n] = s.calls[h].Load()
		hs.Errors[n] = s.errors[h].Load()
		if t := s.lastHook[h].Load(); t != 0 {
			hs.LastHookAt[n] = time.Unix(0, t).UTC()
		}
		p50, p99 := s.lat[h].quantiles()
		hs.P50us[n], hs.P99us[n] = p50, p99
	}
	if t := s.lastUse.Load(); t != 0 {
		u := time.Unix(0, t).UTC()
		hs.LastUsageAt = &u
	}
	return hs
}

// histogram is a lock-free log2 latency histogram in microseconds.
type histogram struct {
	b [32]atomic.Uint64
}

func (h *histogram) add(d time.Duration) {
	us := uint64(max(d.Microseconds(), 0))
	i := bits.Len64(us)
	if i >= len(h.b) {
		i = len(h.b) - 1
	}
	h.b[i].Add(1)
}

// quantiles returns the upper bound (µs) of the buckets holding p50 and p99.
func (h *histogram) quantiles() (uint32, uint32) {
	var counts [32]uint64
	var total uint64
	for i := range h.b {
		counts[i] = h.b[i].Load()
		total += counts[i]
	}
	if total == 0 {
		return 0, 0
	}
	q := func(p float64) uint32 {
		want := uint64(float64(total)*p + 0.5)
		if want == 0 {
			want = 1
		}
		var run uint64
		for i, c := range counts {
			run += c
			if run >= want {
				if i == 0 {
					return 0
				}
				return uint32(min(uint64(1)<<i, 1<<31))
			}
		}
		return 1 << 31
	}
	return q(0.5), q(0.99)
}
