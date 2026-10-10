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

// Sample is one observation of what one credential counted as in one row's
// mean, used for trend. Remaining keeps its name because the history is
// persisted under it.
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
	// Balances are prepaid accounts that belong to no CPA credential — today,
	// the OpenRouter account quota-cache reads with a management key of its
	// own. Always an array; empty when none is configured. They take no part in
	// the header instants, the counters, or staleReason above, which all
	// describe credentials: each balance carries its own.
	Balances []Balance `json:"balances"`
	// APICredits is the monthly Claude API credit of the Console
	// organizations quota-cache lists, estimated from its usage meter and
	// pooled. Null when the snapshot has none, which is every snapshot until
	// claude-api-credits is configured. Like Balances it takes no part in the
	// header instants, the counters, or staleReason; each account carries its
	// own.
	APICredits *APICredits `json:"apiCredits"`
	// RenewalOrphans are renewal dates set from the dashboard for credentials
	// the roster no longer lists, oldest updatedAt first, which the page
	// offers to remove. Always an array.
	RenewalOrphans []RenewalOrphan `json:"renewalOrphans"`
}

// APICredits is the monthly Claude API credit of every Console organization
// quota-cache lists, estimated from the CPA traffic its meter counted, and
// pooled.
//
// Every amount is an estimate. Anthropic reports neither the credit nor what
// is left of it to anything this plugin can read, so the credit and its refill
// date are what the operator configured or set here, the spend is the meter's
// tokens at the list prices of Pricing.AsOf, and a Console reading the
// operator types in is the anchor that corrects it. An account whose figures
// could be missing spend says so with LowerBound.
type APICredits struct {
	Title string `json:"title"`
	// Currency is ISO 4217; every amount below is in it.
	Currency string           `json:"currency"`
	Pricing  APICreditPricing `json:"pricing"`
	// Meter is quota-cache's API meter, null when its file is missing or
	// cannot be read.
	Meter   *APICreditMeter  `json:"meter"`
	Editing APICreditEditing `json:"editing"`
	Pool    APICreditPool    `json:"pool"`
	// Accounts lists every configured organization, counted or not, sorted by
	// order, then label, then id. Do not re-sort.
	Accounts []APICreditAccount `json:"accounts"`
	// Unlinked lists organizations that sent API traffic through CPA while no
	// counted item names them, newest lastSeen first. Always an array.
	Unlinked []APICreditUnlinked `json:"unlinked"`
	// Orphans are stored dashboard values for an account no longer listed,
	// which the page offers to remove. Always an array.
	Orphans []APICreditOrphan `json:"orphans"`
}

// APICreditPricing says which prices the estimates use.
type APICreditPricing struct {
	// AsOf is the day the embedded price table was read, YYYY-MM-DD.
	AsOf string `json:"asOf"`
	// Source is the pricing page it was read from.
	Source string `json:"source"`
	// CacheWrites is the cache-write rate every amount uses: "5m".
	CacheWrites string `json:"cacheWrites"`
}

// APICreditMeter is quota-cache's API meter as last saved.
type APICreditMeter struct {
	SinceEpoch int64 `json:"sinceEpoch"`
	// UpdatedAtEpoch is when quota-cache last saved it. Stale is that older
	// than stale-after: quota-cache saves at least every 10 minutes while it
	// counts, so a stale meter has stopped.
	UpdatedAtEpoch int64 `json:"updatedAtEpoch"`
	Stale          bool  `json:"stale"`
	// StoppedAtEpoch and StopReason are set while the meter is stopped:
	// shutdown, quiesce, disabled, no_items or failed.
	StoppedAtEpoch        *int64 `json:"stoppedAtEpoch"`
	StopReason            string `json:"stopReason"`
	Dropped               uint64 `json:"dropped"`
	LastDroppedEpoch      *int64 `json:"lastDroppedEpoch"`
	Unattributed          uint64 `json:"unattributed"`
	LastUnattributedEpoch *int64 `json:"lastUnattributedEpoch"`
	Rejected              uint64 `json:"rejected"`
	LastRejectedEpoch     *int64 `json:"lastRejectedEpoch"`
	Foreign               uint64 `json:"foreign"`
	// Gaps are the periods the meter was not counting, oldest first. A gap
	// under five minutes (a restart, an update, a reload) is left out, as it
	// is everywhere in this document. Always an array.
	Gaps []APICreditGap `json:"gaps"`
}

// APICreditGap is one period the meter was not counting.
type APICreditGap struct {
	FromEpoch int64  `json:"fromEpoch"`
	ToEpoch   int64  `json:"toEpoch"`
	Reason    string `json:"reason"`
}

// APICreditEditing says whether the page may offer to change these values.
type APICreditEditing struct {
	Available bool `json:"available"`
	// Reason is "" when available, "disabled" when allow-edit is off (stored
	// values still apply), or "settingsUnreadable" when settings.json could
	// not be read (stored values do not apply).
	Reason string `json:"reason"`
}

// APICreditPool sums the counted accounts: those in state ok, stale or out.
// Left is the sum of each account's own left, so one organization's overage
// never consumes another's credit.
type APICreditPool struct {
	// HasEstimate is false when no account is counted. Every amount is then
	// zero with "" text, Level is "", and NextRefill and FullAtEpoch are null.
	HasEstimate bool `json:"hasEstimate"`
	// LowerBound is true when any counted account's is: used is then at
	// least, and left at most, what is printed.
	LowerBound        bool    `json:"lowerBound"`
	MonthlyCredit     float64 `json:"monthlyCredit"`
	MonthlyCreditText string  `json:"monthlyCreditText"`
	// Used is the part of MonthlyCredit spent: MonthlyCredit - Left.
	Used              float64 `json:"used"`
	UsedText          string  `json:"usedText"`
	Left              float64 `json:"left"`
	LeftText          string  `json:"leftText"`
	Overage           float64 `json:"overage"`
	OverageText       string  `json:"overageText"`
	RemainingFraction float64 `json:"remainingFraction"`
	RemainingPercent  int     `json:"remainingPercent"`
	Level             string  `json:"level"`
	// NextRefill is the soonest refill that gives anything back, and null
	// when no counted account has used any credit.
	NextRefill *APICreditRefill `json:"nextRefill"`
	// FullAtEpoch is the latest refill among the counted accounts that have
	// used any credit: when the pool reads full again if nothing more is spent.
	FullAtEpoch   *int64 `json:"fullAtEpoch"`
	FullInSeconds *int64 `json:"fullInSeconds"`
	// AccountCount == len(accounts) == CountedCount + MissingCount.
	AccountCount int `json:"accountCount"`
	CountedCount int `json:"countedCount"`
	MissingCount int `json:"missingCount"`
}

// APICreditRefill is one refill instant and what it returns to the pool,
// assuming each refilling organization's next credit is its current amount.
type APICreditRefill struct {
	// AccountIDs is every counted account refilling at this instant, in
	// account order.
	AccountIDs      []string `json:"accountIds"`
	RefillAtEpoch   int64    `json:"refillAtEpoch"`
	RefillInSeconds int64    `json:"refillInSeconds"`
	// Gain is the credit those accounts have used, which the refill restores.
	Gain     float64 `json:"gain"`
	GainText string  `json:"gainText"`
	// GainFraction is Gain on the pool's 0-1 scale, 0 when the pool's credit
	// is 0; GainPercent is the same figure as printed.
	GainFraction float64 `json:"gainFraction"`
	GainPercent  int     `json:"gainPercent"`
}

// APICreditAccount is one Console organization's credit this cycle.
type APICreditAccount struct {
	// ID is quota-cache's name for the item: "org-<12 hex>" from its
	// organization, or "item-<n>" for an item it could not link to one. The
	// dashboard's stored values are keyed by it.
	ID string `json:"id"`
	// Label is "" when the item has no usable label.
	Label string `json:"label"`
	// Order is the item's configured position.
	Order int `json:"order"`
	// OrganizationID is the item's organization-id as quota-cache read it,
	// and "" when it is missing or invalid.
	OrganizationID string `json:"organizationId"`
	// State is ok, stale, out, pending, needsSettings, misconfigured or
	// cacheTooOld. Counted is true for ok, stale and out.
	State   string `json:"state"`
	Counted bool   `json:"counted"`
	// HasEstimate is true when Left and Used mean something: Basis is not
	// "".
	HasEstimate bool `json:"hasEstimate"`
	// Basis is "reading" when Left is a Console reading less the spend since,
	// "credit" when it is the monthly credit less the spend this cycle, and
	// "" when there is neither.
	Basis string `json:"basis"`
	// LowerBound is true when spend may be missing: Spent and Used are then
	// at least, and Left at most, what is printed.
	LowerBound        bool    `json:"lowerBound"`
	MonthlyCredit     float64 `json:"monthlyCredit"`
	MonthlyCreditText string  `json:"monthlyCreditText"`
	// MonthlyCreditSource and RenewsSource say where the credit and the
	// refill date come from: "dashboard", "config" or "none".
	MonthlyCreditSource string `json:"monthlyCreditSource"`
	// Spent is the metered spend since SpentSinceEpoch: the cycle's start,
	// or without a cycle the reading's time. Null and "" without either.
	Spent           float64 `json:"spent"`
	SpentText       string  `json:"spentText"`
	SpentSinceEpoch *int64  `json:"spentSinceEpoch"`
	// Used is the part of the monthly credit spent: MonthlyCredit - Left,
	// clamped to the credit. "" when the credit is unknown.
	Used     float64 `json:"used"`
	UsedText string  `json:"usedText"`
	Left     float64 `json:"left"`
	LeftText string  `json:"leftText"`
	// EstimateLeftText is what the estimate had left before a refusal from
	// Anthropic marked the account out, and "" unless it is out.
	EstimateLeftText string  `json:"estimateLeftText"`
	Overage          float64 `json:"overage"`
	OverageText      string  `json:"overageText"`
	// RemainingFraction is Left over MonthlyCredit, clamped to 0-1.
	RemainingFraction float64 `json:"remainingFraction"`
	RemainingPercent  int     `json:"remainingPercent"`
	Level             string  `json:"level"`
	// CycleStartEpoch and RenewsAtEpoch bound the cycle containing the build
	// instant, from the refill date in force; null without one.
	CycleStartEpoch *int64 `json:"cycleStartEpoch"`
	RenewsAtEpoch   *int64 `json:"renewsAtEpoch"`
	RenewsInSeconds *int64 `json:"renewsInSeconds"`
	RenewsSource    string `json:"renewsSource"`
	// Reading is the Console reading in use, and null when none is.
	Reading *APICreditReading `json:"reading"`
	// CacheWriteExtra is how much more the estimated window would cost were
	// every cache write a 1-hour one. CacheWriteExtraText is "" below $0.01.
	CacheWriteExtra     float64 `json:"cacheWriteExtra"`
	CacheWriteExtraText string  `json:"cacheWriteExtraText"`
	// Unpriced is every model in the estimated window with no listed price,
	// most tokens first. Always an array.
	Unpriced []APICreditUnpriced `json:"unpriced"`
	Refusals APICreditRefusals   `json:"refusals"`
	// MeterSinceEpoch is when the meter began counting this organization,
	// LastSeenEpoch its latest request.
	MeterSinceEpoch *int64   `json:"meterSinceEpoch"`
	LastSeenEpoch   *int64   `json:"lastSeenEpoch"`
	DataIssues      []string `json:"dataIssues"`
	// Issue is one plain-language sentence about the account, already
	// written, and "" when there is nothing to say.
	Issue    string            `json:"issue"`
	Settings APICreditSettings `json:"settings"`
}

// APICreditReading is the Console reading an account's Left is taken from.
type APICreditReading struct {
	Remaining      float64 `json:"remaining"`
	RemainingText  string  `json:"remainingText"`
	AtEpoch        int64   `json:"atEpoch"`
	EnteredAtEpoch int64   `json:"enteredAtEpoch"`
	// SpentSince is the metered spend since the start of the reading's hour.
	SpentSince     float64 `json:"spentSince"`
	SpentSinceText string  `json:"spentSinceText"`
}

// APICreditRefusals counts Anthropic's low-credit refusals of this
// organization's requests: without a Claude Code session, and with one.
type APICreditRefusals struct {
	Total                 uint64 `json:"total"`
	LastAtEpoch           *int64 `json:"lastAtEpoch"`
	ClaudeCodeTotal       uint64 `json:"claudeCodeTotal"`
	ClaudeCodeLastAtEpoch *int64 `json:"claudeCodeLastAtEpoch"`
}

// APICreditSettings is what the editor shows and sends back for one account.
type APICreditSettings struct {
	Editable bool `json:"editable"`
	// NotEditableReason is "" when editable, else noOrganization,
	// duplicateOrganization, overLimit, cacheTooOld, disabled or
	// settingsUnreadable.
	NotEditableReason string `json:"notEditableReason"`
	// Revision is the stored entry's, "" with nothing stored; a save sends it
	// back as baseRevision.
	Revision string `json:"revision"`
	// MonthlyUSD and Renews are the values set here, "" when none.
	MonthlyUSD string `json:"monthlyUsd"`
	// ConfigMonthlyUSD and ConfigRenews are quota-cache's configured values,
	// shown beside an override; "" when unset or invalid, which the Invalid
	// flags tell apart.
	ConfigMonthlyUSD        string `json:"configMonthlyUsd"`
	ConfigMonthlyUSDInvalid bool   `json:"configMonthlyUsdInvalid"`
	Renews                  string `json:"renews"`
	ConfigRenews            string `json:"configRenews"`
	ConfigRenewsInvalid     bool   `json:"configRenewsInvalid"`
	// Reading is the stored reading, used or not; null when there is none.
	Reading *StoredReading `json:"reading"`
	// ReadingUnusedReason says why a stored reading is not used: beforeRefill,
	// otherOrganization, tooOld or future; "" when it is used or absent.
	ReadingUnusedReason string `json:"readingUnusedReason"`
	UpdatedAtEpoch      *int64 `json:"updatedAtEpoch"`
}

// StoredReading is a reading as saved.
type StoredReading struct {
	RemainingUSD   string `json:"remainingUsd"`
	AtEpoch        int64  `json:"atEpoch"`
	EnteredAtEpoch int64  `json:"enteredAtEpoch"`
}

// APICreditUnlinked is an organization that sent API traffic through CPA
// while no counted item named it.
type APICreditUnlinked struct {
	OrganizationID string `json:"organizationId"`
	FirstSeenEpoch int64  `json:"firstSeenEpoch"`
	LastSeenEpoch  int64  `json:"lastSeenEpoch"`
	Requests       uint64 `json:"requests"`
	// Reason is "overLimit" when an item past the first 16 names it, else
	// "notConfigured".
	Reason string `json:"reason"`
}

// APICreditOrphan is stored dashboard values for an account no longer listed.
type APICreditOrphan struct {
	ID             string `json:"id"`
	MonthlyUSD     string `json:"monthlyUsd"`
	Renews         string `json:"renews"`
	HasReading     bool   `json:"hasReading"`
	Revision       string `json:"revision"`
	UpdatedAtEpoch int64  `json:"updatedAtEpoch"`
}

// APICreditUnpriced is one model the price table has no price for.
type APICreditUnpriced struct {
	Model string `json:"model"`
	// Tokens is input, output, cache read and cache write together, in the
	// estimated window.
	Tokens uint64 `json:"tokens"`
}

// RenewalSetting is a renewal date set from the dashboard.
type RenewalSetting struct {
	Date           string `json:"date"`
	Revision       string `json:"revision"`
	UpdatedAtEpoch int64  `json:"updatedAtEpoch"`
}

// RenewalOrphan is a renewal date stored for a credential the roster no
// longer lists.
type RenewalOrphan struct {
	ID             string `json:"id"`
	Date           string `json:"date"`
	Revision       string `json:"revision"`
	UpdatedAtEpoch int64  `json:"updatedAtEpoch"`
}

// Balance is the money left on one prepaid account.
//
// It is never a bar. The provider reports all-time totals, so a fraction would
// be a share of everything ever bought, which says nothing about whether the
// next request will be paid for. The amount is the headline, and the level is
// judged against the operator's own warn-below threshold.
type Balance struct {
	// ID is quota-cache's name for the account: a fingerprint of the key it is
	// read with, so it changes when the key is rotated.
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Title    string `json:"title"`
	Order    int    `json:"order"`
	// Currency is ISO 4217. Every amount below is in it.
	Currency string `json:"currency"`
	// HasReading is false until a poll has succeeded. Every amount is then zero
	// and means nothing, and Level is "": render a dash, never $0.00, because an
	// unread balance and an empty one are opposite facts.
	HasReading bool `json:"hasReading"`
	// Remaining is Purchased minus Used, and is negative on an overdrawn
	// account. RemainingText is the same amount as the dashboard prints it.
	Remaining     float64 `json:"remaining"`
	RemainingText string  `json:"remainingText"`
	Purchased     float64 `json:"purchased"`
	Used          float64 `json:"used"`
	// WarnBelow is the configured threshold. Level is low below it and critical
	// once nothing is left.
	WarnBelow float64 `json:"warnBelow"`
	Level     string  `json:"level"`
	// Subtext is the line under the amount, already written out.
	Subtext          string   `json:"subtext"`
	ObservedAtEpoch  int64    `json:"observedAtEpoch"`
	NextAttemptEpoch int64    `json:"nextAttemptEpoch"`
	DataIssues       []string `json:"dataIssues"`
	State            string   `json:"state"`
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
	// with no such concept, and a Codex or Claude account that has not been
	// granted one. Null rather than a zero count, so a client renders nothing at
	// all rather than having to decide that "0 banked" is not worth a badge.
	ResetCredits *ResetCredits `json:"resetCredits"`
	// RenewalAtEpoch is when the subscription renews or ends, for a provider
	// that reports it — Codex does — and an estimate of it for one that
	// reports only when the subscription began — Claude. Null when there is
	// neither, and null once a reported instant has passed: a renewal behind
	// us is a poll that has not yet seen the new one, and counting down past
	// zero to it would be wrong. An estimate is always the next one ahead.
	RenewalAtEpoch *int64 `json:"renewalAtEpoch"`
	// RenewalEstimated is true when RenewalAtEpoch is not the provider's date
	// but the next billing anniversary of the subscription's start, and false
	// otherwise, including when there is no renewal at all. A client must say
	// so beside the date: an anniversary is wrong for an account whose billing
	// date has moved since it subscribed, and nothing here can see that.
	RenewalEstimated bool `json:"renewalEstimated"`
	// RenewalSource says where RenewalAtEpoch comes from: "reported" by the
	// provider, set on the "dashboard", or "estimated" from the subscription's
	// start; null when there is no renewal. A reported date wins over the
	// dashboard's, and the dashboard's over the estimate.
	RenewalSource *string `json:"renewalSource"`
	// RenewalEstimateAtEpoch is the estimate for a Claude credential, the next
	// billing anniversary of the subscription's start, whatever RenewalSource
	// says, so the page can show what Use estimate returns to beside a date
	// set on the dashboard. Null for any other provider, and when quota-cache
	// has not read a start.
	RenewalEstimateAtEpoch *int64 `json:"renewalEstimateAtEpoch"`
	// RenewalEditable is true for a Claude credential whose renewal date the
	// page may set, while editing is available; false otherwise.
	RenewalEditable bool `json:"renewalEditable"`
	// RenewalSetting is the renewal date stored from the dashboard, used or
	// not, and null when there is none.
	RenewalSetting *RenewalSetting `json:"renewalSetting"`
	// Credits is the prepaid or granted balance the account can spend beyond
	// its windows, and null for a provider that reports none. Codex reports
	// credits; Grok reports a prepaid dollar balance.
	Credits *Credits `json:"credits"`
}

// Credits is a credential's spendable balance, already written out.
//
// It is a figure rather than a bar for the same reason an OpenRouter balance
// is: there is no ceiling to take a fraction of. Display is the headline and
// is all a client needs; Amount and Unit are the same figure as data, for a
// client that wants to compare or sort rather than print.
type Credits struct {
	// Display is formatted for direct rendering: "57,706.15" for Codex
	// credits, "$12.40" for a dollar balance, "Unlimited" when the provider
	// says there is no limit.
	Display   string `json:"display"`
	Unlimited bool   `json:"unlimited"`
	// Amount is the exact decimal in Unit, unrounded and ungrouped, and "" when
	// Unlimited is true.
	Amount string `json:"amount"`
	// Unit is "credits" or "usd".
	Unit string `json:"unit"`
}

// ResetCredits is what a credential holds in banked rate-limit resets — Codex's
// rate-limit reset credits, or Claude's reset grants.
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
	// reset lapses — a Codex credit thirty days after it is granted, a Claude
	// grant at its own end date — so a count with no deadline beside it is the
	// shape in which they are quietly lost.
	ExpiresAtEpoch   *int64 `json:"expiresAtEpoch"`
	ExpiresInSeconds *int64 `json:"expiresInSeconds"`
	// Redeemable is false when this plugin cannot spend the credit on the
	// operator's behalf — the provider is not one it knows how to redeem on,
	// the credential is disabled in CPA, or redemption is switched off in
	// configuration. The count still shows; only the button goes. Clients must
	// not offer redemption without it.
	Redeemable bool `json:"redeemable"`
	// Hold is the provider's reason, at the last poll, that none can be spent
	// right now: "notLimited", "cooldown", "paused" or "ineligible". Empty when
	// one can be, or when the provider does not say. It is a hint to print
	// beside the button, never a reason to hide it: the reading can be a poll
	// old, and the plugin checks the provider afresh before spending anything.
	Hold string `json:"hold"`
	// HoldUntilEpoch is when a "cooldown" hold lifts, and null otherwise.
	HoldUntilEpoch *int64 `json:"holdUntilEpoch"`
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
// mean was taken over — those with a reading for this window that are not held
// out — and ExcludedCount the rest. The two always sum to the provider's
// credential count, which is also the length of Entries.
type Aggregate struct {
	RemainingFraction float64 `json:"remainingFraction"`
	RemainingPercent  int     `json:"remainingPercent"`
	MemberCount       int     `json:"memberCount"`
	ExcludedCount     int     `json:"excludedCount"`
	// HeldOutCount is the part of ExcludedCount that has a reading but is left
	// out of the mean regardless: on Claude's session row, the accounts whose
	// weekly is spent. The rest of ExcludedCount is the credentials with no
	// reading. Zero on every other row.
	HeldOutCount          int    `json:"heldOutCount"`
	Level                 string `json:"level"`
	Trend                 string `json:"trend"`
	SoonestResetAtEpoch   *int64 `json:"soonestResetAtEpoch"`
	SoonestResetInSeconds *int64 `json:"soonestResetInSeconds"`
	ProjectedGainPercent  int    `json:"projectedGainPercent"`
	// ProjectedGainFraction is the same gain unrounded, on the same 0-1 scale
	// as RemainingFraction, so a client drawing what the next reset returns
	// sizes it from this and prints ProjectedGainPercent beside it — the pair
	// that keeps every other bar and label in this document in agreement.
	ProjectedGainFraction float64 `json:"projectedGainFraction"`
	// FullAtEpoch is when the row would read 100% if nothing more were used:
	// the latest reset among the members below full. On Claude's Fable row a
	// member waits for its weekly's reset as well, when that is below full.
	// Null when every member is already full, when a reset a member below full
	// waits for has no instant (that window does not refill on a schedule), or
	// when there is nothing ahead to count down to. It is the far end of the
	// recovery SoonestResetAtEpoch begins.
	FullAtEpoch   *int64 `json:"fullAtEpoch"`
	FullInSeconds *int64 `json:"fullInSeconds"`
	Subtext       string `json:"subtext"`
}

type RowEntry struct {
	CredentialID string `json:"credentialId"`
	// HasReading is false when this credential reported no observation for this
	// window. Everything numeric below is then zero and means nothing: render a
	// dash, not 0%. Level is "" in that case rather than the level zero would
	// compute to, so a client that ignores this flag shows a neutral row rather
	// than a confident red one.
	HasReading        bool    `json:"hasReading"`
	RemainingFraction float64 `json:"remainingFraction"`
	RemainingPercent  int     `json:"remainingPercent"`
	Level             string  `json:"level"`
	// HeldOut is true for an entry with a reading that the row's mean leaves
	// out: on Claude's session row, an account whose weekly is spent, whose
	// session can read 100% with nothing able to use it. The figures above are
	// still its real reading; its shares are zero and ResetsNext is false.
	// False on every other row.
	HeldOut bool `json:"heldOut"`
	// PooledFraction is what this credential counts as in the row's mean, and
	// PooledPercent the same figure as printed. Both equal RemainingFraction
	// and RemainingPercent on every row but Claude's Fable, where an account
	// counts as the lesser of its Fable and its weekly remaining, since it
	// cannot spend more Fable than its weekly allows. A held-out entry's is its
	// own reading, though the mean leaves it out. Zero with no reading.
	PooledFraction float64 `json:"pooledFraction"`
	PooledPercent  int     `json:"pooledPercent"`
	// PoolShare is this credential's slice of the row's aggregate, on the
	// row's 0-1 scale: across a row's entries the shares sum to the
	// aggregate's RemainingFraction, so a pooled bar is these laid end to end.
	// Zero for an entry with no reading or held out, neither of which is in
	// the mean.
	PoolShare float64 `json:"poolShare"`
	// RecoveryShare is what the row regains from this credential at its next
	// recovery, on the same scale, and zero unless ResetsNext. Across a row the
	// shares sum to the aggregate's ProjectedGainFraction: the next reset's
	// return, credential by credential.
	RecoveryShare float64 `json:"recoveryShare"`
	// ResetsNext is true for every credential whose window resets at the
	// row's soonest reset or within the minute after it — on Claude's Fable
	// row, whose Fable window or weekly does — the ones that recovery is made
	// of. True even for a credential whose reset returns nothing, full already
	// or still held down by its weekly: it is still the next thing to happen to
	// the row.
	ResetsNext       bool     `json:"resetsNext"`
	ResetAtEpoch     *int64   `json:"resetAtEpoch"`
	ResetInSeconds   *int64   `json:"resetInSeconds"`
	ResetDisplayHint string   `json:"resetDisplayHint"`
	ObservedAtEpoch  int64    `json:"observedAtEpoch"`
	NextAttemptEpoch int64    `json:"nextAttemptEpoch"`
	SourceWindowKey  string   `json:"sourceWindowKey"`
	SourceModel      *string  `json:"sourceModel"`
	DataIssues       []string `json:"dataIssues"`
	State            string   `json:"state"`
}
