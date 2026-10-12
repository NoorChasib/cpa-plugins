package route

import (
	"sync"
	"time"
)

// Exploration gives a key the ranked choice would never pick one attempt per
// wait, ahead of the ranked choice:
//
//   - unmapped: a key the table does not map, so the meter can learn it;
//   - failing: a key mapped to an eligible organization but not proven;
//   - recheck: the preferred key of an organization with Recheck, its
//     smallest proven offered key, or each of its keys in turn while it
//     offers none, so a false or premature out cannot strand its credit.
//
// Each claim that teaches nothing doubles the key's wait, from ExploreEvery
// up to MaxExploreWait. A claim whose attempt produced no usage record at all
// (a count_tokens call, or one that never reached the upstream) gets one
// quick re-arm after QuickRearm first. That bounds what a key that never
// becomes proven costs its clients, however it fails.

type exploreKind uint8

const (
	kindUnmapped exploreKind = iota
	kindFailing
	kindRecheck
)

var kindNames = [...]string{kindUnmapped: "unmapped", kindFailing: "failing", kindRecheck: "recheck"}

const maxStep = 6 // wait(6) is already MaxExploreWait

type explore struct {
	mu sync.Mutex
	m  map[string]*entry // auth ID → entry
}

// entry is one auth ID's exploration history.
type entry struct {
	claimedAt int64 // unix seconds
	step      uint8 // 0 to maxStep
	// quickPending makes the entry ready again at claimedAt + QuickRearm;
	// quickUsed is set once that re-arm has been spent at this step.
	quickPending, quickUsed bool
	// failedInBand is set when a later pick in the same round named this
	// claim's attempt as the failed previous one.
	failedInBand bool
	// evaluated is set once Publish has judged the latest claim.
	evaluated   bool
	kind        exploreKind
	configIndex string // the key's 0-based position among CPA's Claude API keys, or ""
}

func seconds(d time.Duration) int64 { return int64(d / time.Second) }

// wait is min(ExploreEvery << step, MaxExploreWait): 10, 20, 40, 80, 160 and
// 320 minutes, then 6 hours.
func wait(step uint8) int64 {
	if w := ExploreEvery << step; w < MaxExploreWait {
		return seconds(w)
	}
	return seconds(MaxExploreWait)
}

// nextAt is when the entry may be claimed again, unix seconds. QuickRearm is
// shorter than every wait, so a pending re-arm always comes first.
func (e *entry) nextAt() int64 {
	if e.quickPending {
		return e.claimedAt + seconds(QuickRearm)
	}
	return e.claimedAt + wait(e.step)
}

func (e *entry) ready(at int64) bool { return at >= e.nextAt() }

// claim takes id's exploration slot for this attempt. It returns false when
// id has an entry that is not ready. Only an ID it has never seen allocates.
func (r *Router) claim(id string, kind exploreKind, configIndex string, now time.Time) bool {
	x := &r.explore
	x.mu.Lock()
	defer x.mu.Unlock()
	if hook := r.beforeClaim.Load(); hook != nil {
		(*hook)()
	}
	at := now.Unix()
	e, ok := x.m[id]
	switch {
	case ok && !e.ready(at):
		return false
	case !ok:
		// Dead IDs must not block new keys, so the oldest claim makes room.
		if len(x.m) >= MaxExplore {
			x.evictOldest()
		}
		e = &entry{}
		x.m[id] = e
	}
	e.claimedAt, e.kind = at, kind
	e.failedInBand, e.evaluated, e.quickPending = false, false, false
	e.configIndex = ""
	if validIndex(configIndex) {
		e.configIndex = configIndex
	}
	return true
}

// evictOldest removes the entry with the oldest claim, the smaller ID on a
// tie. The caller holds mu.
func (x *explore) evictOldest() {
	oldest := ""
	for id, e := range x.m {
		if o, ok := x.m[oldest]; !ok || e.claimedAt < o.claimedAt || e.claimedAt == o.claimedAt && id < oldest {
			oldest = id
		}
	}
	delete(x.m, oldest)
}

// failedInBand marks id's latest claim as failed when it has not been judged
// yet: a pick naming it as the previous attempt of the round means that
// attempt failed.
func (r *Router) failedInBand(id string) {
	x := &r.explore
	x.mu.Lock()
	defer x.mu.Unlock()
	if e, ok := x.m[id]; ok && !e.evaluated {
		e.failedInBand = true
	}
}

// evaluate judges the claims against a newly published table, on the rebuild
// path. A claim is judged once it is at least QuickRearm old, which leaves
// time for the meter to flush its usage record and for a rebuild to read it.
// An entry that is moot goes at any active table, judged or not.
func (r *Router) evaluate(t *Table, now time.Time) {
	x := &r.explore
	x.mu.Lock()
	defer x.mu.Unlock()
	at := now.Unix()
	for id, e := range x.m {
		// An ID not claimed for this long most likely left CPA's
		// configuration; a live one simply starts again at the first wait.
		if at-e.claimedAt > seconds(MaxExploreAge) {
			delete(x.m, id)
			continue
		}
		// An inactive table holds no keys, so it can say nothing about a
		// claim.
		if t.Reason != ReasonNone {
			continue
		}
		k, known := t.keys[id]
		if moot(t, k, known, e.kind) {
			delete(x.m, id)
			continue
		}
		if e.evaluated || at-e.claimedAt < seconds(QuickRearm) {
			continue
		}
		switch {
		case known && k.seen >= e.claimedAt, e.failedInBand:
			// The attempt reached the meter, or failed in band, and the key
			// is still not settled: it taught nothing. That holds while its
			// organization is ineligible too, so a stale or out spell at
			// the judgement cannot hold the step back.
			e.backOff()
		case !e.quickUsed:
			// No usage record: a count_tokens attempt, or one that never
			// reached the upstream. One quick re-arm per step.
			e.quickPending, e.quickUsed, e.evaluated = true, true, true
		default:
			e.backOff()
		}
	}
}

func (e *entry) backOff() {
	if e.step < maxStep {
		e.step++
	}
	e.quickPending, e.quickUsed, e.evaluated = false, false, true
}

// moot reports whether exploring the ID is settled for good: the key is
// proven, or mapped somewhere the router never explores (foreign, unlinked),
// or, for a re-check, its organization is eligible and no longer demoted, or
// the key has left it. Deleting the entry resets the key's wait.
//
// An organization that is ineligible for a while (stale between two meter
// saves, out until a re-check succeeds, pending) settles nothing, so its
// keys' entries keep their step. Otherwise a state that flips would restart a
// failing key's backoff at the first wait each time, and the key would cost
// its clients far more than the backoff allows. An entry whose organization
// stays ineligible goes after MaxExploreAge.
func moot(t *Table, k key, known bool, kind exploreKind) bool {
	if kind == kindRecheck {
		if !known || k.org < 0 {
			return true
		}
		org := &t.Orgs[k.org]
		return org.Eligible && !org.Demoted
	}
	return known && (k.class == classRanked || k.class == classForeign || k.class == classUnlinked)
}
