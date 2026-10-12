// Package settings keeps the values an operator sets on quota-cache's page: an
// API credit's monthly amount and refill date, a Console reading of what is
// left, and a Claude subscription's renewal date. They live in
// snapshot.settings.json beside the snapshot, in exactly the schema of Quota
// Glance's settings.json.
//
// Loading checks shape only. A value that was valid when it was saved stays
// loadable however old it gets: a reading taken before the last refill, or for
// an organization no longer listed, is still a well-formed reading, and the
// credit state reports it as unused rather than this package refusing the
// whole file. The time rules apply when a value is saved.
//
// A file that fails a shape rule is never overwritten. Nothing in it applies,
// editing is unavailable, and the credit state says why, until the operator
// moves it aside.
package settings

import (
	"strconv"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

const (
	// Schema is the only settings schema this build reads or writes: Quota
	// Glance's settings.json schema 1.
	Schema = 1
	// The file's bounds. A save past one first evicts renewal dates for
	// credentials no longer listed, and is refused when that is not enough.
	MaxAPICredits = 64
	MaxRenewals   = 256
	MaxBytes      = 1 << 20
	// MaxBaseline bounds a reading's baseline. Usage past it is summed under
	// client.MeterOtherModel, which is never priced: the baseline then reads
	// low, and the spend since the reading high, the safe direction.
	MaxBaseline = client.MaxMeterUsagePerBucket
)

// Values is one committed state of the settings file. It is never modified
// once a Store hands it out; a save swaps in a new one.
type Values struct {
	// Revision goes up with each committed write. A save writes past every
	// revision its store has seen and at least at the time in Unix seconds,
	// so a page that read an earlier file, since deleted, moved aside or
	// replaced by an older copy, even while CPA was stopped, holds no revision
	// a new row can have. Each entry's Rev is the revision that last wrote it.
	// 0 without a file.
	Revision uint64
	// APICredits is keyed by the anthropic-api account id, "org-" and 12 hex
	// digits.
	APICredits map[string]APICredit
	// Renewals is keyed by a Claude credential's CPA auth index, 16 hex
	// digits.
	Renewals map[string]Renewal
	// Unreadable is set when the settings file exists but failed a shape rule.
	// Nothing in it applies, the maps are empty, and nothing may be saved
	// until the file is moved aside.
	Unreadable bool
}

// APICredit is the page's values for one Console organization. An empty
// string is a value not set.
type APICredit struct {
	MonthlyUSD string
	// Renews is a date, "2026-10-29".
	Renews    string
	Reading   *Reading
	Rev       uint64
	UpdatedAt time.Time
}

// Reading is what Console showed as left of the credit, and when.
type Reading struct {
	RemainingUSD string
	At           time.Time
	EnteredAt    time.Time
	// OrganizationID is the organization the account named when the reading
	// was saved. A reading for another organization is never used.
	OrganizationID string
	// Baseline is the meter's usage from 00:00 UTC of the reading's day to the
	// start of its hour, taken when it was saved. What the organization spent
	// since the reading is its daily usage from that day, less this.
	Baseline Baseline
}

// Baseline is a reading's token baseline. Usage is in the meter's own form, so
// it is priced by the same function as the meter it is subtracted from.
type Baseline struct {
	DayStart time.Time
	Until    time.Time
	Usage    []client.MeterUsage
}

// Renewal is a Claude subscription's renewal date as the operator set it.
type Renewal struct {
	Date      string
	Rev       uint64
	UpdatedAt time.Time
}

// RevisionText is a revision as the wire carries it: decimal, and "" for
// nothing stored.
func RevisionText(rev uint64) string {
	if rev == 0 {
		return ""
	}
	return strconv.FormatUint(rev, 10)
}
