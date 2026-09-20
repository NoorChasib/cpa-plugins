// Package aggregate turns one quota-cache snapshot plus the host's credential
// roster into the document the dashboard renders.
//
// Everything here is pure: Build takes the current instant as an argument and
// nothing in this package calls time.Now(). That is what makes the golden tests
// meaningful, and it is enforced by a test.
//
// The one conversion that matters: quota-cache reports USED capacity on 0-100,
// and this document reports REMAINING capacity as a fraction on 0-1 plus a
// rounded percent. The inversion happens exactly once, in remainingOf.
package aggregate

import "time"

// SchemaVersion is the version of the document below. Evolution is additive;
// a breaking change bumps this.
const SchemaVersion = 1

// Stale reasons. staleReason is null when stale is false.
const (
	ReasonNeverObserved     = "neverObserved"
	ReasonCacheMissing      = "cacheMissing"
	ReasonCacheStale        = "cacheStale"
	ReasonSchemaUnsupported = "snapshotSchemaUnsupported"
	// ReasonRosterUnavailable means the host could not list credentials. The
	// snapshot may be perfectly readable; without the roster there is nothing
	// to attribute it to.
	ReasonRosterUnavailable = "rosterUnavailable"
)

// Levels. The threshold lives here, not in CSS, so every client agrees.
const (
	LevelOK       = "ok"
	LevelLow      = "low"
	LevelCritical = "critical"
)

// Activity intensity tiers, the ink ramp for one bucket of the request strip.
// The thresholds that place a bucket on this ramp are in build.go, beside the
// capacity thresholds they follow.
const (
	IntensityNone   = 0
	IntensityLow    = 1
	IntensityMedium = 2
	IntensityHigh   = 3
)

// Trends.
const (
	TrendUp      = "up"
	TrendDown    = "down"
	TrendFlat    = "flat"
	TrendUnknown = "unknown"
)

// Credential statuses and per-entry states.
const (
	StatusOK          = "ok"
	StatusError       = "error"
	StatusUnsupported = "unsupported"
	StatusDisabled    = "disabled"
	StatusUnavailable = "unavailable"
	// StatusPending is a credential quota-cache knows about but has not yet
	// polled successfully. It is not an error and has no data.
	StatusPending = "pending"
	StateStale    = "stale"
	// StateNoData is a row entry with nothing behind it: the credential is in
	// the roster and reported other windows, but not this one. It exists so
	// every card lists every credential — a credential silently missing from a
	// card is indistinguishable from one the operator forgot to add.
	StateNoData = "noData"
)

// Reset display hints. A reset instant slightly in the past is normal between a
// window resetting and the next poll, so the client is told not to run a
// countdown rather than left to render a negative one.
const (
	HintCountdown = "countdown"
	HintNone      = "none"
)

// Identity is one credential as the host reports it. quota-glance takes display
// concerns from here and quota from the snapshot.
type Identity struct {
	AuthIndex   string
	Provider    string
	Label       string
	Email       string
	Disabled    bool
	Unavailable bool
	// Recent is CPA's rolling request counter for this credential, oldest
	// bucket first. It is the one part of the roster that answers a question
	// the snapshot cannot: which credential CPA is actually routing to. Empty
	// when the host does not report it.
	Recent []RecentRequest
}

// RecentRequest is one bucket of that counter.
//
// Label is the host's own range in the CPA process's local time, "14:50-15:00".
// It carries no date and no zone, so it is never parsed into an instant — only
// the width between its halves is read, which is the same in any zone.
type RecentRequest struct {
	Label   string
	Success int64
	Failed  int64
}

// Sample is one observation of one window's remaining fraction, used for trend.
type Sample struct {
	AuthIndex string    `json:"authIndex"`
	WindowKey string    `json:"windowKey"`
	At        time.Time `json:"at"`
	Remaining float64   `json:"remaining"`
}

// Document is the response body of GET .../summary.
type Document struct {
	SchemaVersion    int   `json:"schemaVersion"`
	GeneratedAtEpoch int64 `json:"generatedAtEpoch"`
	// ObservedAtEpoch is the newest successful observation across every
	// credential; NextAttemptEpoch is the soonest poll quota-cache has
	// scheduled. The dashboard header reads "observed 4m ago · next attempt in
	// 11m" from exactly these two. They are here rather than derived in the
	// client because deriving them means a max and a min across every entry,
	// and two clients doing that arithmetic separately is how they come to
	// disagree. Both are null when there is nothing to report.
	ObservedAtEpoch  *int64       `json:"observedAtEpoch"`
	NextAttemptEpoch *int64       `json:"nextAttemptEpoch"`
	Stale            bool         `json:"stale"`
	StaleReason      *string      `json:"staleReason"`
	Counters         Counters     `json:"counters"`
	Credentials      []Credential `json:"credentials"`
	Providers        []Provider   `json:"providers"`
}

type Counters struct {
	Credentials  int `json:"credentials"`
	ObservedOK   int `json:"observedOK"`
	ObserveError int `json:"observeError"`
}

// Credential is the flat catalog, sorted by weekly reset, soonest first. Every
// row's entries appear in this same order; the client does not re-sort.
type Credential struct {
	ID                string `json:"id"`
	Email             string `json:"email"`
	Provider          string `json:"provider"`
	Plan              string `json:"plan"`
	Status            string `json:"status"`
	LastObservedEpoch int64  `json:"lastObservedEpoch"`
	// Activity is what CPA has routed to this credential recently. It hangs off
	// the credential rather than off a row because a request is made against a
	// credential, not against a window: the same strip is correct on every card
	// the credential appears in. Null when the host reports no counter at all.
	Activity *Activity `json:"activity"`
	// ResetCredits is the account's banked rate-limit resets, and is null for
	// every credential that has none — which is every credential on a provider
	// with no such concept, and a Codex account that has not been granted one.
	// Null rather than a zero count, so a client renders nothing at all rather
	// than having to decide that "0 banked" is not worth a badge.
	ResetCredits *ResetCredits `json:"resetCredits"`
}

// ResetCredits is what a credential holds in banked rate-limit resets.
//
// It is not remaining capacity and never becomes a bar: it is a count of
// entitlements that, when one is spent, clear the account's windows outright.
// That is why it hangs off the credential beside the plan badge rather than
// appearing on any of the window cards it would reset.
type ResetCredits struct {
	// AvailableCount is at least 1 whenever this object exists.
	AvailableCount int `json:"availableCount"`
	// ExpiresAtEpoch and ExpiresInSeconds date the soonest credit that can
	// still be spent, and are null when the provider did not say. A banked
	// reset lapses thirty days after it is granted, so a count with no deadline
	// beside it is the shape in which they are quietly lost.
	ExpiresAtEpoch   *int64 `json:"expiresAtEpoch"`
	ExpiresInSeconds *int64 `json:"expiresInSeconds"`
	// Redeemable is false when this plugin cannot spend the credit on the
	// operator's behalf — the credential is not one CPA can hand a token for,
	// or redemption is switched off in configuration. The count still shows;
	// only the button goes. Clients must not offer redemption without it.
	Redeemable bool `json:"redeemable"`
}

// Activity is one credential's recent request traffic, as a fixed ring of
// equal-width buckets, oldest first and newest last.
//
// It answers a question the quota figures cannot: a credential at 100% is
// either being routed to and keeping up, or not being routed to at all, and
// those are opposite facts about a pool that look identical on a capacity bar.
type Activity struct {
	// BucketSeconds is the width of each bucket and WindowSeconds the span of
	// all of them. Both are reported rather than assumed: the host owns the
	// ring, and a client that hardcoded "the last 3h20m" would mislabel the
	// strip the day CPA changed it.
	BucketSeconds int              `json:"bucketSeconds"`
	WindowSeconds int              `json:"windowSeconds"`
	Buckets       []ActivityBucket `json:"buckets"`
	// Success and Failed total the buckets, so a client never sums them to
	// print the count beside the strip.
	Success int64 `json:"success"`
	Failed  int64 `json:"failed"`
	// LastRequestAtEpoch is the end of the newest bucket with any traffic in
	// it, clamped to the build instant, and null when the whole window is
	// empty. It is an upper bound rather than an exact instant: the ring counts
	// per bucket and does not record when inside one a request landed.
	LastRequestAtEpoch *int64 `json:"lastRequestAtEpoch"`
	// Live is traffic in the bucket now in progress — the closest this data can
	// come to "right now". It is served rather than derived because deriving it
	// means knowing which bucket is current, which is the one thing the labels
	// do not say.
	Live bool `json:"live"`
}

// ActivityBucket is one interval of the ring. A bucket that saw nothing is
// present with zeroes rather than omitted: the gaps are half of what the strip
// says, and a ring with its empty buckets removed is a different shape.
type ActivityBucket struct {
	Success int64 `json:"success"`
	Failed  int64 `json:"failed"`
	// Intensity places the bucket on the strip's ink ramp, 0 for no traffic up
	// to 3 for the busiest. Like every other threshold in this document it is
	// decided here, so two clients cannot ink the same bucket differently.
	// Clamp an unknown value to the top of the ramp you implement.
	Intensity int `json:"intensity"`
}

type Provider struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	Order           int    `json:"order"`
	CredentialCount int    `json:"credentialCount"`
	Rows            []Row  `json:"rows"`
}

// Row is one window across the provider's credentials. Matched is false for a
// raw: window, which the client renders generically.
type Row struct {
	RowID     string     `json:"rowId"`
	Title     string     `json:"title"`
	Order     int        `json:"order"`
	Matched   bool       `json:"matched"`
	Aggregate Aggregate  `json:"aggregate"`
	Entries   []RowEntry `json:"entries"`
}

// Aggregate carries both a fraction and a rounded percent so the bar width and
// the printed label cannot disagree. MemberCount counts the credentials the
// mean was taken over — those with a reading for this window — and
// ExcludedCount the rest. The two always sum to the provider's credential
// count, which is also the length of Entries.
type Aggregate struct {
	RemainingFraction     float64 `json:"remainingFraction"`
	RemainingPercent      int     `json:"remainingPercent"`
	MemberCount           int     `json:"memberCount"`
	ExcludedCount         int     `json:"excludedCount"`
	Level                 string  `json:"level"`
	Trend                 string  `json:"trend"`
	SoonestResetAtEpoch   *int64  `json:"soonestResetAtEpoch"`
	SoonestResetInSeconds *int64  `json:"soonestResetInSeconds"`
	ProjectedGainPercent  int     `json:"projectedGainPercent"`
	Subtext               string  `json:"subtext"`
}

type RowEntry struct {
	CredentialID string `json:"credentialId"`
	// HasReading is false when this credential reported no observation for this
	// window. Everything numeric below is then zero and means nothing: render a
	// dash, not 0%. Level is "" in that case rather than the level zero would
	// compute to, so a client that ignores this flag shows a neutral row rather
	// than a confident red one.
	HasReading        bool     `json:"hasReading"`
	RemainingFraction float64  `json:"remainingFraction"`
	RemainingPercent  int      `json:"remainingPercent"`
	Level             string   `json:"level"`
	ResetAtEpoch      *int64   `json:"resetAtEpoch"`
	ResetInSeconds    *int64   `json:"resetInSeconds"`
	ResetDisplayHint  string   `json:"resetDisplayHint"`
	ObservedAtEpoch   int64    `json:"observedAtEpoch"`
	NextAttemptEpoch  int64    `json:"nextAttemptEpoch"`
	SourceWindowKey   string   `json:"sourceWindowKey"`
	SourceModel       *string  `json:"sourceModel"`
	DataIssues        []string `json:"dataIssues"`
	State             string   `json:"state"`
}
