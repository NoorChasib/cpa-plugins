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
// the printed label cannot disagree.
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
	CredentialID      string   `json:"credentialId"`
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
