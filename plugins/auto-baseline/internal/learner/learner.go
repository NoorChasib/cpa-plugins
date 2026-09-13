// Package learner tracks candidate fingerprints per provider and decides
// when one has met quorum. It is pure and clock-injected: no I/O, no
// goroutines, no host calls. Every method does bounded work under one mutex
// so the interceptor hot path can call Observe synchronously.
package learner

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/auto-baseline/internal/fingerprint"
)

// Bounds that keep memory constant under adversarial input.
const (
	// MaxPendingPerProvider caps distinct candidate keys tracked per
	// provider; the stalest candidate is evicted when the cap is hit.
	MaxPendingPerProvider = 32
	// MaxRecordsPerCandidate caps stored observations per candidate; the
	// oldest record is dropped when the cap is hit.
	MaxRecordsPerCandidate = 256
)

// Settings are the quorum rules.
type Settings struct {
	MinObservations     int
	MinDistinctSessions int
	ObservationWindow   time.Duration
	// MinVersion is the per-provider write floor.
	MinVersion map[fingerprint.Provider]fingerprint.Version
	// Rules are the current classifier rules; restored candidates must still
	// satisfy them.
	Rules fingerprint.Rules
}

// Record is one counted observation. An empty SessionID is an anonymous
// observation: it counts toward MinObservations but never toward
// MinDistinctSessions.
type Record struct {
	SessionID string    `json:"session_id,omitempty"`
	At        time.Time `json:"at"`
}

// Pending is one tracked candidate with its evidence.
type Pending struct {
	Key       string                `json:"key"`
	Candidate fingerprint.Candidate `json:"candidate"`
	Records   []Record              `json:"records"`
	FirstSeen time.Time             `json:"first_seen"`
	LastSeen  time.Time             `json:"last_seen"`
}

// Decision reason buckets for Observe.
const (
	DecisionTracked           = "tracked"
	DecisionQuorum            = "quorum_met"
	DecisionNotNewer          = "not_newer_than_baseline"
	DecisionBelowFloor        = "below_min_version"
	DecisionProviderUnmanaged = "provider_unmanaged"
	// DecisionBaselineMalformed means the on-disk baseline for the provider
	// carries an explicit user-agent that does not parse; the plugin refuses
	// to compare against or overwrite it. Evidence is still collected so
	// evaluation resumes as soon as the file is fixed.
	DecisionBaselineMalformed = "baseline_malformed"
)

// Decision reports what Observe did with one observation.
type Decision struct {
	Reason string
	// Ready is set when the candidate meets quorum after this observation.
	Ready *fingerprint.Candidate
}

// Evidence summarizes a pending candidate for status output.
type Evidence struct {
	Candidate        fingerprint.Candidate `json:"candidate"`
	Observations     int                   `json:"observations"`
	DistinctSessions int                   `json:"distinct_sessions"`
	FirstSeen        time.Time             `json:"first_seen"`
	LastSeen         time.Time             `json:"last_seen"`
	QuorumMet        bool                  `json:"quorum_met"`
}

// Learner holds per-provider state.
type Learner struct {
	mu       sync.Mutex
	settings Settings
	now      func() time.Time
	managed  map[fingerprint.Provider]bool
	baseline map[fingerprint.Provider]fingerprint.Version
	// blocked holds a non-empty reason (baseline_malformed, duplicate_key,
	// unsupported_config_shape) while the provider's on-disk block cannot be
	// compared against. Blocked providers keep collecting evidence but never
	// report readiness.
	blocked map[fingerprint.Provider]string
	pending map[fingerprint.Provider]map[string]*Pending
}

// New builds a Learner. managed lists the providers whose observations are
// tracked; others are reported as unmanaged.
func New(settings Settings, managed []fingerprint.Provider, now func() time.Time) *Learner {
	l := &Learner{
		settings: settings,
		now:      now,
		managed:  make(map[fingerprint.Provider]bool, len(managed)),
		baseline: make(map[fingerprint.Provider]fingerprint.Version),
		blocked:  make(map[fingerprint.Provider]string),
		pending:  make(map[fingerprint.Provider]map[string]*Pending),
	}
	for _, p := range managed {
		l.managed[p] = true
	}
	return l
}

// Reconfigure swaps quorum rules and the managed set. Pending evidence is
// retained; candidates that are no longer eligible are dropped on the next
// baseline update or observation.
func (l *Learner) Reconfigure(settings Settings, managed []fingerprint.Provider) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.settings = settings
	l.managed = make(map[fingerprint.Provider]bool, len(managed))
	for _, p := range managed {
		l.managed[p] = true
	}
	for provider := range l.pending {
		if !l.managed[provider] {
			delete(l.pending, provider)
		}
	}
}

// SetBaseline records the effective on-disk baseline for a provider and
// discards pending candidates that are no longer strictly newer than it.
// blocked ("" when fine) marks a provider whose on-disk block cannot be
// compared against (malformed user-agent, duplicate key, unsupported shape):
// evidence is retained and keeps accumulating, but nothing is ready until a
// later read clears the block.
func (l *Learner) SetBaseline(provider fingerprint.Provider, v fingerprint.Version, blocked string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.baseline[provider] = v
	l.blocked[provider] = blocked
	if blocked != "" {
		return
	}
	for key, p := range l.pending[provider] {
		if !l.eligibleLocked(p.Candidate) {
			delete(l.pending[provider], key)
		}
	}
}

// Blocked returns the block reason for a provider, or "".
func (l *Learner) Blocked(provider fingerprint.Provider) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.blocked[provider]
}

// eligibleLocked reports whether a candidate is strictly newer than the
// effective baseline and not below the floor. A blocked provider is still
// "eligible" for tracking (evidence is retained); readiness is gated
// separately.
func (l *Learner) eligibleLocked(c fingerprint.Candidate) bool {
	if !l.managed[c.Provider] {
		return false
	}
	if floor, ok := l.settings.MinVersion[c.Provider]; ok && floor.Newer(c.Version) {
		return false
	}
	base, ok := l.baseline[c.Provider]
	if !ok {
		// Unknown baseline: be conservative and treat the floor as the
		// baseline so nothing at or below it is ever tracked.
		base = l.settings.MinVersion[c.Provider]
	}
	return c.Version.Newer(base)
}

// Observe counts one observation. An empty sessionID is anonymous.
func (l *Learner) Observe(c fingerprint.Candidate, sessionID string, at time.Time) Decision {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.managed[c.Provider] {
		return Decision{Reason: DecisionProviderUnmanaged}
	}
	if floor, ok := l.settings.MinVersion[c.Provider]; ok && floor.Newer(c.Version) {
		return Decision{Reason: DecisionBelowFloor}
	}
	if !l.eligibleLocked(c) {
		return Decision{Reason: DecisionNotNewer}
	}
	blocked := l.blocked[c.Provider]
	byKey := l.pending[c.Provider]
	if byKey == nil {
		byKey = make(map[string]*Pending)
		l.pending[c.Provider] = byKey
	}
	key := c.Key()
	p := byKey[key]
	if p == nil {
		if len(byKey) >= MaxPendingPerProvider {
			evictStalestLocked(byKey)
		}
		p = &Pending{Key: key, Candidate: c, FirstSeen: at}
		byKey[key] = p
	}
	p.Records = append(p.Records, Record{SessionID: sessionID, At: at})
	if len(p.Records) > MaxRecordsPerCandidate {
		p.Records = p.Records[len(p.Records)-MaxRecordsPerCandidate:]
	}
	if at.After(p.LastSeen) {
		p.LastSeen = at
	}
	if p.FirstSeen.IsZero() || at.Before(p.FirstSeen) {
		p.FirstSeen = at
	}
	obs, sessions := l.countLocked(p, at)
	if blocked != "" {
		// Evidence recorded; readiness withheld until the block clears.
		return Decision{Reason: blocked}
	}
	if l.quorumLocked(obs, sessions) {
		ready := p.Candidate
		return Decision{Reason: DecisionQuorum, Ready: &ready}
	}
	return Decision{Reason: DecisionTracked}
}

// quorumLocked applies the quorum rule. Anonymous observations never count
// as sessions, so a MinDistinctSessions of 1 (the default) means "no session
// diversity required" and is satisfied by observation count alone; values
// above 1 require that many distinct named sessions.
func (l *Learner) quorumLocked(obs, sessions int) bool {
	if obs < l.settings.MinObservations {
		return false
	}
	return l.settings.MinDistinctSessions <= 1 || sessions >= l.settings.MinDistinctSessions
}

// countLocked counts observations inside the window ending at now and the
// distinct sessions among them. Records older than the window are pruned.
func (l *Learner) countLocked(p *Pending, now time.Time) (int, int) {
	cutoff := now.Add(-l.settings.ObservationWindow)
	kept := p.Records[:0]
	sessions := make(map[string]struct{}, 4)
	for _, r := range p.Records {
		if r.At.Before(cutoff) {
			continue
		}
		kept = append(kept, r)
		if r.SessionID != "" {
			sessions[r.SessionID] = struct{}{}
		}
	}
	// Zero the tail so dropped records do not linger in the backing array.
	for i := len(kept); i < len(p.Records); i++ {
		p.Records[i] = Record{}
	}
	p.Records = kept
	return len(kept), len(sessions)
}

func evictStalestLocked(byKey map[string]*Pending) {
	var stalestKey string
	var stalest time.Time
	first := true
	for key, p := range byKey {
		if first || p.LastSeen.Before(stalest) {
			stalestKey, stalest, first = key, p.LastSeen, false
		}
	}
	if stalestKey != "" {
		delete(byKey, stalestKey)
	}
}

// Ready returns the highest-version candidate for a provider that currently
// meets quorum, if any. Ties on version are broken by observation count so
// the better-attested tuple wins.
func (l *Learner) Ready(provider fingerprint.Provider) (fingerprint.Candidate, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.blocked[provider] != "" {
		return fingerprint.Candidate{}, false
	}
	now := l.now()
	l.pruneEmptyLocked(provider, now)
	var best *Pending
	bestObs := 0
	for _, p := range l.pending[provider] {
		if !l.eligibleLocked(p.Candidate) {
			continue
		}
		obs, sessions := l.countLocked(p, now)
		if !l.quorumLocked(obs, sessions) {
			continue
		}
		if best == nil || p.Candidate.Version.Newer(best.Candidate.Version) ||
			(p.Candidate.Version == best.Candidate.Version && obs > bestObs) {
			best, bestObs = p, obs
		}
	}
	if best == nil {
		return fingerprint.Candidate{}, false
	}
	return best.Candidate, true
}

// Forget drops one candidate (after it has been promoted or rejected).
func (l *Learner) Forget(provider fingerprint.Provider, key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.pending[provider], key)
}

// Reset clears all pending candidates.
func (l *Learner) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pending = make(map[fingerprint.Provider]map[string]*Pending)
}

// pruneEmptyLocked drops candidates whose records have all aged out of the
// window. Keeping them would make them look like restorable evidence on the
// next save and then count as "dropped" on restore.
func (l *Learner) pruneEmptyLocked(provider fingerprint.Provider, now time.Time) {
	for key, p := range l.pending[provider] {
		if obs, _ := l.countLocked(p, now); obs == 0 {
			delete(l.pending[provider], key)
		}
	}
}

// Export returns a deep copy of pending state for persistence. Candidates
// with no records inside the window are pruned and never exported.
func (l *Learner) Export() []Pending {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	var out []Pending
	for provider, byKey := range l.pending {
		l.pruneEmptyLocked(provider, now)
		for _, p := range byKey {
			cp := *p
			cp.Records = append([]Record(nil), p.Records...)
			out = append(out, cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Candidate.Provider != out[j].Candidate.Provider {
			return out[i].Candidate.Provider < out[j].Candidate.Provider
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Import restores pending state (e.g. from the state file) and MERGES it
// with live evidence: a reconfigure can Start while observations are already
// flowing, so records are unioned (deduplicated by session+time) rather than
// replaced. Every entry passes the structural validator under the CURRENT
// rules; invalid entries are dropped and counted in the returned value.
// Records with future timestamps or older than the observation window are
// pruned; an entry left with no records is dropped. Entries not eligible
// under the current baseline/floor are dropped silently (stale, not corrupt).
func (l *Learner) Import(items []Pending, now time.Time) (dropped int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, item := range items {
		item, err := l.validatePendingLocked(item, now)
		if err != nil {
			dropped++
			continue
		}
		if !l.eligibleLocked(item.Candidate) {
			continue
		}
		byKey := l.pending[item.Candidate.Provider]
		if byKey == nil {
			byKey = make(map[string]*Pending)
			l.pending[item.Candidate.Provider] = byKey
		}
		existing := byKey[item.Key]
		if existing == nil {
			if len(byKey) >= MaxPendingPerProvider {
				evictStalestLocked(byKey)
			}
			cp := item
			cp.Records = append([]Record(nil), item.Records...)
			byKey[item.Key] = &cp
			existing = &cp
		} else {
			existing.Records = mergeRecords(existing.Records, item.Records)
			if item.FirstSeen.Before(existing.FirstSeen) {
				existing.FirstSeen = item.FirstSeen
			}
			if item.LastSeen.After(existing.LastSeen) {
				existing.LastSeen = item.LastSeen
			}
		}
		if len(existing.Records) > MaxRecordsPerCandidate {
			existing.Records = existing.Records[len(existing.Records)-MaxRecordsPerCandidate:]
		}
	}
	return dropped
}

// validatePendingLocked applies the full structural validator to a restored
// entry under the current rules and returns a pruned copy: records with a
// future timestamp, older than the observation window, or with an invalid
// session ID are dropped; an entry with nothing left is an error.
func (l *Learner) validatePendingLocked(item Pending, now time.Time) (Pending, error) {
	if item.Key == "" || item.Key != item.Candidate.Key() {
		return Pending{}, fmt.Errorf("key mismatch")
	}
	if err := fingerprint.ValidateCandidate(item.Candidate, l.settings.Rules); err != nil {
		return Pending{}, err
	}
	if len(item.Records) == 0 || len(item.Records) > MaxRecordsPerCandidate {
		return Pending{}, fmt.Errorf("record count %d out of bounds", len(item.Records))
	}
	if item.FirstSeen.IsZero() || item.LastSeen.IsZero() || item.LastSeen.Before(item.FirstSeen) {
		return Pending{}, fmt.Errorf("first/last seen inconsistent")
	}
	cutoff := now.Add(-l.settings.ObservationWindow)
	kept := make([]Record, 0, len(item.Records))
	for _, r := range item.Records {
		if r.At.IsZero() || r.At.After(now) || r.At.Before(cutoff) || !fingerprint.ValidSessionID(r.SessionID) {
			continue
		}
		kept = append(kept, r)
	}
	if len(kept) == 0 {
		return Pending{}, fmt.Errorf("no records inside the observation window")
	}
	item.Records = kept
	if item.LastSeen.After(now) {
		item.LastSeen = now
	}
	return item, nil
}

// mergeRecords unions two record sets, deduplicating identical
// (session, time) pairs and keeping chronological order.
func mergeRecords(a, b []Record) []Record {
	seen := make(map[Record]struct{}, len(a)+len(b))
	out := make([]Record, 0, len(a)+len(b))
	for _, list := range [][]Record{a, b} {
		for _, r := range list {
			if _, dup := seen[r]; dup {
				continue
			}
			seen[r] = struct{}{}
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// Summarize returns per-provider evidence for status display, newest first.
func (l *Learner) Summarize(provider fingerprint.Provider) []Evidence {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.pruneEmptyLocked(provider, now)
	var out []Evidence
	for _, p := range l.pending[provider] {
		obs, sessions := l.countLocked(p, now)
		out = append(out, Evidence{
			Candidate:        p.Candidate,
			Observations:     obs,
			DistinctSessions: sessions,
			FirstSeen:        p.FirstSeen,
			LastSeen:         p.LastSeen,
			QuorumMet:        l.quorumLocked(obs, sessions) && l.eligibleLocked(p.Candidate),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Candidate.Version != out[j].Candidate.Version {
			return out[i].Candidate.Version.Newer(out[j].Candidate.Version)
		}
		return out[i].Candidate.Key() < out[j].Candidate.Key()
	})
	return out
}
