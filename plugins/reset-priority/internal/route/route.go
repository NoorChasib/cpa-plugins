// Package route is Reset Priority's API credit router: CPA's scheduler for the
// request attempts whose candidates are all Claude API keys from CPA's own
// configuration. For those it picks the key whose Console organization's
// monthly API credit refills soonest and still has credit left. Every other
// attempt, and every doubt, goes back to CPA as handled:false.
//
// It reads Quota Cache's published credit state and its meter's auth IDs. The
// router loop calls Build and Publish; CPA's request goroutines call Pick,
// concurrently and with no CPA lock held. Pick reads an immutable Table
// through one atomic load. It takes no plugin lock, reads no file, calls no
// host callback and logs nothing: CPA gives a scheduler no deadline, so
// anything that could block would hold a client request until its client gave
// up, then stall the plugin's unload. The only lock in here, explore.mu, is a
// leaf held for one lookup or a loop over at most MaxExplore entries, and
// every holder releases it with defer.
//
// Nothing here reads the clock. Every instant is an argument, and a test
// enforces it, so the whole router can be driven through days of simulated
// time.
package route

import (
	"bytes"
	"encoding/json"
	"sync/atomic"
	"time"
)

// The router's limits. None is configurable: each follows from a cadence of
// the router loop or of Quota Cache, or bounds what a key that never becomes
// proven can cost its clients.
const (
	MaxTableAge       = 3 * time.Minute    // 3 × the router loop's 1-minute rebuild (internal/plugin)
	MaxMeterAge       = 15 * time.Minute   // quota-cache saves at least every 10 min + 1-min flush + slack
	MaxCreditStateAge = 3 * time.Minute    // 3 × Quota Cache's 60-second publish heartbeat
	ExploreEvery      = 10 * time.Minute   // backoff base; a claim that taught nothing is next due 2× later
	MaxExploreWait    = 6 * time.Hour      // backoff cap
	QuickRearm        = 3 * time.Minute    // mapping lag: meter flush ≤ 1 min + publish tick ≤ 10 s + rebuild ≤ 1 min + slack
	MaxExploreAge     = 7 * 24 * time.Hour // an entry not claimed for this long is dropped
	MaxExplore        = 64                 // = qc.MaxMeterAuthIDs; the oldest entry is evicted beyond it
	ProvenFailures    = 2                  // failures since the last success at which a key stops being ranked
	RecentKeyAge      = 7 * 24 * time.Hour // keys seen within this count on the status page
	InactiveWarnAfter = 30 * time.Minute   // lasting-inactive warning
)

// Reason says why Pick answered as it did, or why a Table cannot route. The
// three answers that name a key are ReasonRouted, ReasonExplored and
// ReasonRechecked; every other reason is a decline, which CPA treats as
// handled:false and resolves with its own selector.
type Reason uint8

const (
	// ReasonNone is a Table that would route. Pick never answers with it.
	ReasonNone Reason = iota
	ReasonRouted
	ReasonExplored
	ReasonRechecked

	// Declines, in the order Pick checks them. The table's own reasons sit
	// between ReasonNoTable and ReasonTableStale, in the order Build checks
	// them. Pick also declines ReasonCreditsStale after ReasonMeterStale, once
	// the credit state's valid_until has come.
	ReasonDisabled
	ReasonNoTable
	ReasonCreditsMissing
	ReasonCreditsUnreadable
	ReasonCreditsStale
	ReasonSettingsUnavailable
	ReasonMeterMissing
	ReasonMeterUnreadable
	ReasonMeterStopped
	ReasonMeterTooOld
	ReasonMeterStale
	ReasonNoAPICredits
	ReasonNoLinkedOrgs
	ReasonTableStale
	ReasonOutOfScope
	ReasonInvalidRequest
	ReasonUnmapped
	ReasonNoProven
	ReasonNoEligible
	ReasonPanic

	// ReasonBuildFailed is never a decision: the last Build panicked, so the
	// router holds no table and Pick declines ReasonNoTable. Status reports it
	// as the table reason.
	ReasonBuildFailed

	reasonCount
)

var reasonNames = [reasonCount]string{
	ReasonNone:                "",
	ReasonRouted:              "routed",
	ReasonExplored:            "explored",
	ReasonRechecked:           "rechecked",
	ReasonDisabled:            "disabled",
	ReasonNoTable:             "no_table",
	ReasonCreditsMissing:      "credits_missing",
	ReasonCreditsUnreadable:   "credits_unreadable",
	ReasonCreditsStale:        "credits_stale",
	ReasonSettingsUnavailable: "settings_unavailable",
	ReasonMeterMissing:        "meter_missing",
	ReasonMeterUnreadable:     "meter_unreadable",
	ReasonMeterStopped:        "meter_stopped",
	ReasonMeterTooOld:         "meter_too_old",
	ReasonMeterStale:          "meter_stale",
	ReasonNoAPICredits:        "no_api_credits",
	ReasonNoLinkedOrgs:        "no_linked_orgs",
	ReasonTableStale:          "table_stale",
	ReasonOutOfScope:          "out_of_scope",
	ReasonInvalidRequest:      "invalid_request",
	ReasonUnmapped:            "unmapped",
	ReasonNoProven:            "no_proven",
	ReasonNoEligible:          "no_eligible",
	ReasonPanic:               "panic",
	ReasonBuildFailed:         "build_failed",
}

// String is the reason as the status and the logs spell it.
func (r Reason) String() string {
	if r >= reasonCount {
		return "unknown"
	}
	return reasonNames[r]
}

// declines reports whether r is a decision that hands the attempt back to CPA.
func (r Reason) declines() bool { return r >= ReasonDisabled && r <= ReasonPanic }

// Decision is Pick's answer.
type Decision struct {
	AuthID string // non-empty only for ReasonRouted, ReasonExplored and ReasonRechecked
	Reason Reason
}

// Router holds the published table, the exploration history and the
// counters. Every field but explore is an atomic, so Pick, Publish and Status
// never wait on one another except for explore's leaf mutex.
type Router struct {
	table                          atomic.Pointer[Table]
	enabled, declared, buildFailed atomic.Bool
	// active is what the last Publish computed, so it can report an edge.
	active atomic.Bool

	picks, demoted, buildPanics atomic.Uint64
	// decisions counts every answer by its Reason: the three that name a key
	// and every decline.
	decisions [reasonCount]atomic.Uint64

	lastPickAt, lastRoutedAt atomic.Int64 // unix nanoseconds, 0 before the first
	lastDecision             atomic.Uint32
	// lastRoutedOrg points at an account ID inside an immutable Table, and is
	// nil when the last answer with an ID had no organization.
	lastRoutedOrg atomic.Pointer[string]

	explore explore

	// Test-only hooks, never set in production: beforeDecide runs at the top
	// of Pick's decision, beforeClaim inside claim while explore.mu is held.
	beforeDecide, beforeClaim atomic.Pointer[func()]
}

// New returns a disabled router with no table. It is the only constructor: a
// zero Router has no exploration map.
func New() *Router {
	return &Router{explore: explore{m: map[string]*entry{}}}
}

// SetEnabled turns routing on or off. Off, Pick declines ReasonDisabled while
// the table keeps being built and published, so the status can show whether
// turning it on would route.
func (r *Router) SetEnabled(on bool) { r.enabled.Store(on) }

// SetDeclared records whether Reset Priority last declared the scheduler
// capability to CPA. It is the plugin's intent, not CPA's snapshot, which can
// disagree with it for a moment after a configure.
func (r *Router) SetDeclared(on bool) { r.declared.Store(on) }

// Publish stores t, which may be nil, as the table Pick reads, and evaluates
// the exploration claims against it. It reports whether the router was active
// before and is active now, where active means enabled and t would route, so
// the caller can log an edge. It clears build_failed. Publish(nil, now) only
// stores nil: it never touches the exploration history, so it is safe on
// every stop path.
func (r *Router) Publish(t *Table, now time.Time) (wasActive, isActive bool) {
	r.table.Store(t)
	r.buildFailed.Store(false)
	if t != nil {
		r.evaluate(t, now)
	}
	isActive = r.enabled.Load() && t != nil && t.routes(now)
	return r.active.Swap(isActive), isActive
}

// BuildFailed records that Build panicked: the router holds no table, Status
// reports build_failed, and build_panics grows. Atomics only, so it is safe in
// the router loop's recover.
func (r *Router) BuildFailed(now time.Time) {
	r.Publish(nil, now)
	r.buildFailed.Store(true)
	r.buildPanics.Add(1)
}

// outcome is one decision with what Pick records beside it.
type outcome struct {
	d Decision
	// org is the chosen key's account ID inside the table, nil without one.
	org *string
	// demoted is true when ranking the previous attempt's organization last
	// changed the answer.
	demoted bool
}

func declined(reason Reason) outcome { return outcome{d: Decision{Reason: reason}} }

// oauthCandidate is the attribute CPA gives every file-based OAuth
// credential. Its presence anywhere in the request declines it before the
// decode, which spares every subscription request the cost. A false positive,
// a header value holding that text, only declines.
var oauthCandidate = []byte(`"auth_kind":"oauth"`)

// Pick answers one scheduler.pick request. raw is the request exactly as CPA
// sent it and now the instant of the call. It never panics, never blocks and
// never returns an AuthID that is not one of the request's candidates.
func (r *Router) Pick(raw []byte, now time.Time) (d Decision) {
	if r == nil {
		return Decision{Reason: ReasonDisabled}
	}
	var o outcome
	defer func() {
		if recover() != nil {
			o = declined(ReasonPanic)
		}
		r.count(o, now)
		d = o.d
	}()
	o = r.decide(raw, now)
	return o.d
}

// count records one answer. It runs in Pick's deferred function after the
// recover, so it must not be able to panic: atomics and a guarded index only.
func (r *Router) count(o outcome, now time.Time) {
	r.picks.Add(1)
	if o.d.Reason < reasonCount {
		r.decisions[o.d.Reason].Add(1)
	}
	r.lastPickAt.Store(now.UnixNano())
	r.lastDecision.Store(uint32(o.d.Reason))
	if o.d.AuthID != "" {
		r.lastRoutedAt.Store(now.UnixNano())
		r.lastRoutedOrg.Store(o.org)
	}
	if o.demoted {
		r.demoted.Add(1)
	}
}

// decide is Pick's steps 1 to 7: every decline that needs no decode, then the
// decode. Steps 1 to 6 allocate nothing.
func (r *Router) decide(raw []byte, now time.Time) outcome {
	if hook := r.beforeDecide.Load(); hook != nil {
		(*hook)()
	}
	if !r.enabled.Load() {
		return declined(ReasonDisabled)
	}
	t := r.table.Load()
	switch {
	case t == nil:
		return declined(ReasonNoTable)
	case t.Reason != ReasonNone:
		return declined(t.Reason)
	// A stopped router loop (quiesce, disable, a wedged build) stops
	// publishing, so its last table ages out here.
	case now.Sub(t.BuiltAt) > MaxTableAge:
		return declined(ReasonTableStale)
	case now.Sub(t.MeterFlushedAt) > MaxMeterAge:
		return declined(ReasonMeterStale)
	// 5a. From valid_until on, the state's ranking may be last cycle's: a
	// refill has come, or a reading has gone out of use. Until the next build
	// reads Quota Cache's new state, the attempt goes back to CPA.
	case t.ValidUntil != nil && !now.Before(*t.ValidUntil):
		return declined(ReasonCreditsStale)
	case bytes.Contains(raw, oauthCandidate):
		return declined(ReasonOutOfScope)
	}
	var w pickWire
	if json.Unmarshal(raw, &w) != nil || len(w.Candidates) == 0 {
		return declined(ReasonInvalidRequest)
	}
	return r.choose(t, &w, now)
}

// choose is Pick's steps 8 to 11 over a decoded request. It allocates
// nothing, except for a claim on an ID exploration has never seen.
func (r *Router) choose(t *Table, w *pickWire, now time.Time) outcome {
	// 8a. A selected_auth_id names the previous attempt of this round, which
	// failed, or there would be no second pick. Most such failures are
	// organization-wide (low credit, a rate limit), while CPA's tried set and
	// cooldowns are per key, so that organization ranks after every other
	// eligible one for this pick.
	sel := string(w.Options.Metadata.SelectedAuthID)
	failedOrg := int16(-1)
	if sel != "" {
		if k, ok := t.keys[sel]; ok && (k.class == classRanked || k.class == classFailing) {
			failedOrg = k.org
		}
		r.failedInBand(sel)
	}

	// best is the winner with that demotion, plain the winner without it.
	best, plain := -1, -1
	var bestRank, plainRank int64
	mapped, unmapped, failing := false, false, false
	for i := range w.Candidates {
		c := &w.Candidates[i]
		if !c.inScope() {
			return declined(ReasonOutOfScope)
		}
		k, ok := t.keys[c.ID]
		switch {
		case !ok || k.class == classUnmapped:
			unmapped = true
			continue
		case k.class == classFailing:
			failing = true
		case k.class == classRanked:
			rank := int64(k.rank)
			if plain < 0 || rank < plainRank || rank == plainRank && c.ID < w.Candidates[plain].ID {
				plain, plainRank = i, rank
			}
			if k.org == failedOrg {
				rank += 1 << 16
			}
			if best < 0 || rank < bestRank || rank == bestRank && c.ID < w.Candidates[best].ID {
				best, bestRank = i, rank
			}
		}
		mapped = true
	}

	// 9. Exploration, ahead of the ranked choice, except on a failover pick
	// that has a ranked candidate: a request spends at most one attempt on
	// exploration while a ranked key is on offer.
	if sel == "" || best < 0 {
		if o, ok := r.tryExplore(t, w, now, unmapped, failing); ok {
			return o
		}
	}

	// 10. The ranked choice.
	if best >= 0 {
		c := &w.Candidates[best]
		return outcome{
			d:       Decision{AuthID: c.ID, Reason: ReasonRouted},
			org:     &t.Orgs[t.keys[c.ID].org].ID,
			demoted: best != plain,
		}
	}

	// 11. Nothing to answer with.
	switch {
	case !mapped:
		return declined(ReasonUnmapped)
	case failing:
		return declined(ReasonNoProven)
	}
	return declined(ReasonNoEligible)
}

// tryExplore tries the exploration claims in order: unmapped candidates, then
// failing ones, both in the order received, then a key of each organization
// to re-check, soonest refill first. The first claim that succeeds answers.
func (r *Router) tryExplore(t *Table, w *pickWire, now time.Time, unmapped, failing bool) (outcome, bool) {
	if unmapped {
		for i := range w.Candidates {
			c := &w.Candidates[i]
			if k, ok := t.keys[c.ID]; (!ok || k.class == classUnmapped) && r.claim(c.ID, kindUnmapped, c.Attributes.ConfigIndex, now) {
				return outcome{d: Decision{AuthID: c.ID, Reason: ReasonExplored}}, true
			}
		}
	}
	if failing {
		for i := range w.Candidates {
			c := &w.Candidates[i]
			if k, ok := t.keys[c.ID]; ok && k.class == classFailing && r.claim(c.ID, kindFailing, c.Attributes.ConfigIndex, now) {
				return outcome{d: Decision{AuthID: c.ID, Reason: ReasonExplored}, org: &t.Orgs[k.org].ID}, true
			}
		}
	}
	for _, org := range t.rechecks {
		// The organization's preferred key is the one the ranked choice would
		// take: its smallest proven offered ID. While that key's slot waits
		// no sibling is tried, so a dry organization costs one attempt per
		// wait however many keys it has, and a proven key that fails twice is
		// no longer preferred from the next rebuild. While the organization
		// offers no proven key, its keys take turns in the order received,
		// each with its own wait, so one that fails every request, such as a
		// personal key sent without a workspace ID, cannot keep it out.
		pick := -1
		for i := range w.Candidates {
			if k, ok := t.keys[w.Candidates[i].ID]; ok && k.org == org && k.proven && (pick < 0 || w.Candidates[i].ID < w.Candidates[pick].ID) {
				pick = i
			}
		}
		for i := range w.Candidates {
			c := &w.Candidates[i]
			if k, ok := t.keys[c.ID]; !ok || k.org != org || pick >= 0 && i != pick {
				continue
			}
			if r.claim(c.ID, kindRecheck, c.Attributes.ConfigIndex, now) {
				return outcome{d: Decision{AuthID: c.ID, Reason: ReasonRechecked}, org: &t.Orgs[org].ID}, true
			}
		}
	}
	return outcome{}, false
}
