package aggregate

import (
	"sort"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// renewalsOf is every renewal date Quota Cache stores, keyed by CPA auth
// index, and nil without a usable state: no date entered by the operator is
// then known, and each credential's renewal is reported or estimated.
func renewalsOf(in Input) map[string]RenewalSetting {
	if state := usableCredits(in); state != nil {
		return state.Renewals
	}
	return nil
}

// snapshotEntryOf is the credential's snapshot entry, nil when the snapshot
// has none.
func snapshotEntryOf(r record) *qc.Entry {
	if !r.hasEntry {
		return nil
	}
	return &r.entry
}

// renewalFor is the credential's renewal instant and where it comes from, or
// nil for both when there is none. The precedence is Quota Cache's client's,
// so this page and Quota Cache's agree at every instant: a reported renewal
// wins, then the date the operator entered, then the estimate.
func renewalFor(r record, setting *RenewalSetting, now time.Time) (epoch *int64, source *string) {
	date := ""
	if setting != nil {
		date = setting.Date
	}
	at, from := qc.SubscriptionRenewal(r.identity.Provider, snapshotEntryOf(r), date, now)
	if at == nil {
		return nil, nil
	}
	return epochPointerOf(*at), &from
}

// renewalEstimateOf is a Claude credential's estimated renewal whatever wins,
// shown beside a date the operator entered; nil for any other provider.
func renewalEstimateOf(r record, now time.Time) *int64 {
	if at := qc.SubscriptionRenewalEstimate(r.identity.Provider, snapshotEntryOf(r), now); at != nil {
		return epochPointerOf(*at)
	}
	return nil
}

// renewalOrphansOf is every stored renewal date whose credential the roster
// does not list, oldest first. Without a roster to compare against there is
// no telling an orphan from a credential the host failed to report, so none
// are listed.
func renewalOrphansOf(in Input) []RenewalOrphan {
	out := []RenewalOrphan{}
	if in.SourceReason != "" {
		return out
	}
	listed := map[string]bool{}
	for _, identity := range in.Identities {
		listed[identity.AuthIndex] = true
	}
	for id, setting := range renewalsOf(in) {
		if !listed[id] {
			out = append(out, RenewalOrphan{
				ID: id, Date: setting.Date, Revision: setting.Revision, UpdatedAtEpoch: setting.UpdatedAtEpoch,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAtEpoch != out[j].UpdatedAtEpoch {
			return out[i].UpdatedAtEpoch < out[j].UpdatedAtEpoch
		}
		return out[i].ID < out[j].ID
	})
	return out
}
