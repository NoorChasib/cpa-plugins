package route

import (
	"sort"
	"time"
)

// Status is the router as Reset Priority's status reports it, under router. It
// never carries an auth ID, an auth index, an organization UUID, a header or a
// key: an organization shows as its account ID (org-<12 hex>), label and issue
// sentence, which Quota Cache's page and Quota Glance's dashboard already show,
// and an explored key as its config index.
type Status struct {
	Enabled               bool            `json:"enabled"`
	Declared              bool            `json:"declared"` // what Reset Priority last declared, not CPA's snapshot
	Active                bool            `json:"active"`
	Reason                string          `json:"reason"`
	TableReason           string          `json:"table_reason"`
	BuiltAt               *time.Time      `json:"built_at"`
	MeterFlushedAt        *time.Time      `json:"meter_flushed_at"`
	AuthIDsSince          *time.Time      `json:"auth_ids_since"`
	MaxTableAgeSeconds    int             `json:"max_table_age_seconds"`
	MaxMeterAgeSeconds    int             `json:"max_meter_age_seconds"`
	ExploreEverySeconds   int             `json:"explore_every_seconds"`
	MaxExploreWaitSeconds int             `json:"max_explore_wait_seconds"`
	Orgs                  []OrgStatus     `json:"orgs"`
	MappedKeys            int             `json:"mapped_keys"`
	UnmappedKeys          int             `json:"unmapped_keys"`
	FailingKeys           int             `json:"failing_keys"`
	UnlinkedKeys          int             `json:"unlinked_keys"`
	ForeignKeys           int             `json:"foreign_keys"`
	StaleKeys             int             `json:"stale_keys"`
	Exploring             []ExploreStatus `json:"exploring"`
	LastPickAt            *time.Time      `json:"last_pick_at"`
	LastRoutedAt          *time.Time      `json:"last_routed_at"`
	LastDecision          string          `json:"last_decision"`
	LastRoutedOrg         string          `json:"last_routed_org"` // account id of the latest answer's key; "" before the first, or when that key had no organization
	Counters              Counters        `json:"counters"`
}

// OrgStatus is one linked organization.
type OrgStatus struct {
	ID               string     `json:"id"`
	Label            string     `json:"label"`
	Rank             int        `json:"rank"`
	Eligible         bool       `json:"eligible"`
	Demoted          bool       `json:"demoted"`
	Recheck          bool       `json:"recheck"`
	Reason           string     `json:"reason"`
	RenewsAt         *time.Time `json:"renews_at"`
	LeftText         string     `json:"left_text"`
	EstimateLeftText string     `json:"estimate_left_text"` // what the estimate had left before `out`
	Refusal          string     `json:"refusal"`            // refused, refusedNearlySpent, claudeCodeRefused or ""
	Issue            string     `json:"issue"`              // the credit state's sentence for the account
	Keys             int        `json:"keys"`
	FailingKeys      int        `json:"failing_keys"`
}

// ExploreStatus is one exploration entry, without its auth ID.
type ExploreStatus struct {
	ConfigIndex string     `json:"config_index"` // the key's 0-based position among CPA's Claude API keys; "" if unknown
	Kind        string     `json:"kind"`         // unmapped, failing or recheck
	Step        int        `json:"step"`
	NextAt      *time.Time `json:"next_at"`
}

// Counters count Pick's answers since CPA loaded this copy of the plugin.
type Counters struct {
	Picks       uint64            `json:"picks"`
	Routed      uint64            `json:"routed"`
	Explored    uint64            `json:"explored"`
	Rechecked   uint64            `json:"rechecked"`
	Demoted     uint64            `json:"demoted"`
	BuildPanics uint64            `json:"build_panics"`
	Declined    map[string]uint64 `json:"declined"` // every decline reason present, zeros included
}

// Status reports the router at now. Every field is computed here, so table
// age and staleness are as of the request, not the last rebuild.
func (r *Router) Status(now time.Time) Status {
	s := base()
	s.Enabled, s.Declared = r.enabled.Load(), r.declared.Load()
	t := r.table.Load()
	s.TableReason = r.tableReason(t, now).String()
	s.Reason = s.TableReason
	if !s.Enabled {
		s.Reason = ReasonDisabled.String()
	}
	s.Active = s.Enabled && s.TableReason == ""

	if t != nil {
		s.BuiltAt, s.MeterFlushedAt, s.AuthIDsSince = timeOrNil(t.BuiltAt), timeOrNil(t.MeterFlushedAt), timeOrNil(t.AuthIDsSince)
		for i := range t.Orgs {
			s.Orgs = append(s.Orgs, orgStatus(&t.Orgs[i]))
		}
		s.MappedKeys, s.UnmappedKeys, s.FailingKeys = t.MappedKeys, t.UnmappedKeys, t.FailingKeys
		s.UnlinkedKeys, s.ForeignKeys, s.StaleKeys = t.UnlinkedKeys, t.ForeignKeys, t.StaleKeys
	}
	s.Exploring = r.exploring()

	s.LastPickAt, s.LastRoutedAt = nanosOrNil(r.lastPickAt.Load()), nanosOrNil(r.lastRoutedAt.Load())
	s.LastDecision = Reason(r.lastDecision.Load()).String()
	if org := r.lastRoutedOrg.Load(); org != nil {
		s.LastRoutedOrg = *org
	}

	c := &s.Counters
	c.Picks = r.picks.Load()
	c.Routed = r.decisions[ReasonRouted].Load()
	c.Explored = r.decisions[ReasonExplored].Load()
	c.Rechecked = r.decisions[ReasonRechecked].Load()
	c.Demoted, c.BuildPanics = r.demoted.Load(), r.buildPanics.Load()
	for reason := Reason(0); reason < reasonCount; reason++ {
		if reason.declines() {
			c.Declined[reason.String()] = r.decisions[reason].Load()
		}
	}
	return s
}

// Idle is the status of a router that does not exist: the status before the
// plugin has one.
func Idle() Status {
	s := base()
	s.Reason, s.TableReason = ReasonDisabled.String(), ReasonNoTable.String()
	return s
}

// base is a status with the constants set, every array empty and every
// decline counted at zero.
func base() Status {
	s := Status{
		MaxTableAgeSeconds:    int(seconds(MaxTableAge)),
		MaxMeterAgeSeconds:    int(seconds(MaxMeterAge)),
		ExploreEverySeconds:   int(seconds(ExploreEvery)),
		MaxExploreWaitSeconds: int(seconds(MaxExploreWait)),
		Orgs:                  []OrgStatus{},
		Exploring:             []ExploreStatus{},
		Counters:              Counters{Declined: map[string]uint64{}},
	}
	for reason := Reason(0); reason < reasonCount; reason++ {
		if reason.declines() {
			s.Counters.Declined[reason.String()] = 0
		}
	}
	return s
}

// tableReason is the first reason t would not route at now, or ReasonNone,
// whether routing is on or off.
func (r *Router) tableReason(t *Table, now time.Time) Reason {
	switch {
	case r.buildFailed.Load():
		return ReasonBuildFailed
	case t == nil:
		return ReasonNoTable
	case t.Reason != ReasonNone:
		return t.Reason
	case now.Sub(t.BuiltAt) > MaxTableAge:
		return ReasonTableStale
	case now.Sub(t.MeterFlushedAt) > MaxMeterAge:
		return ReasonMeterStale
	case t.ValidUntil != nil && !now.Before(*t.ValidUntil):
		return ReasonCreditsStale
	}
	return ReasonNone
}

func orgStatus(o *Org) OrgStatus {
	s := OrgStatus{
		ID: o.ID, Label: o.Label, Rank: o.Rank, Eligible: o.Eligible, Demoted: o.Demoted, Recheck: o.Recheck,
		Reason: o.Reason, LeftText: o.LeftText, EstimateLeftText: o.EstimateLeftText, Refusal: o.Refusal,
		Issue: o.Issue, Keys: o.Keys, FailingKeys: o.FailingKeys,
	}
	if o.RenewsAt != nil {
		at := *o.RenewsAt
		s.RenewsAt = &at
	}
	return s
}

// exploring lists the exploration entries by when each may be claimed again.
// The auth ID orders ties and is then dropped.
func (r *Router) exploring() []ExploreStatus {
	type listed struct {
		id string
		at int64
		ExploreStatus
	}
	var all []listed
	func() {
		x := &r.explore
		x.mu.Lock()
		defer x.mu.Unlock()
		all = make([]listed, 0, len(x.m))
		for id, e := range x.m {
			all = append(all, listed{id: id, at: e.nextAt(), ExploreStatus: ExploreStatus{
				ConfigIndex: e.configIndex, Kind: kindNames[e.kind], Step: int(e.step),
			}})
		}
	}()
	sort.Slice(all, func(a, b int) bool {
		if all[a].at != all[b].at {
			return all[a].at < all[b].at
		}
		return all[a].id < all[b].id
	})
	out := make([]ExploreStatus, len(all))
	for i := range all {
		next := time.Unix(all[i].at, 0).UTC()
		out[i] = all[i].ExploreStatus
		out[i].NextAt = &next
	}
	return out
}

func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}

func nanosOrNil(n int64) *time.Time {
	if n == 0 {
		return nil
	}
	t := time.Unix(0, n).UTC()
	return &t
}
