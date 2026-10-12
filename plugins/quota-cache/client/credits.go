package client

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"
	"time"
)

// The credit state is quota-cache's published judgement of the monthly Claude
// API credit of every Console organization it lists: the amounts, the cycle
// and the refill, the out and stale states, and the issues, worked out from
// its organizations, the operator's saved values and its API meter, at the
// list prices of its own table. It is saved to its own file beside the
// snapshot (CreditsPath), so every consumer shows and uses the same numbers
// and none prices the meter itself.
//
// Part of it moves with the clock, and a quota-cache that has stopped cannot
// publish that it has. So a reader loads the file with LoadCredits and judges
// it at its own instant with CreditsAt, which re-applies only the rules whose
// inputs are published facts plus now: the countdowns, stale, a meter gap
// after a stop, and expiry. Nothing else is ever recomputed outside
// quota-cache.
//
// Keys outside api_credits and renewals are snake_case, like the snapshot and
// the meter. Keys inside those two are camelCase, the names of Quota Glance's
// summary document, which passes them through.

// Credit state file limits.
const (
	CreditsSchema   = 1       // the only schema LoadCredits accepts
	MaxCreditsBytes = 1 << 20 // the largest credit state file LoadCredits reads
)

// DefaultCreditStaleAfter is the age past which a reader with no threshold of
// its own calls the state stale. It is Quota Glance's stale-after default, so
// quota-cache's page and Quota Glance agree under defaults.
const DefaultCreditStaleAfter = 45 * time.Minute

// CreditBoundaryGrace is how long past ValidUntil a reader still uses a state
// before calling it stale: long enough for the publisher, which wakes at
// ValidUntil, to write the next one first.
const CreditBoundaryGrace = 2 * time.Minute

// CreditPublishLagLimit is how far a snapshot's written_at may run ahead of the
// state's ComputedAt while quota-cache's publisher runs: it recomputes within
// 10 seconds of every snapshot write and writes at least every 60 seconds. A
// snapshot written later than this after the state means the snapshot's writer
// runs and no publisher does, as after a rollback to a quota-cache that
// predates the file.
const CreditPublishLagLimit = 5 * time.Minute

// ErrCreditsMissing is LoadCredits' error when there is no credit state file:
// the quota-cache that writes the snapshot predates it, or has not written one
// yet.
var ErrCreditsMissing = errors.New("credit state file missing")

// CreditsPath is where quota-cache saves its credit state for the snapshot at
// snapshotPath: the same path with a final ".json" replaced by ".credits.json",
// else with ".credits.json" appended. For DefaultPath it is
// plugins/data/quota-cache/snapshot.credits.json.
func CreditsPath(snapshotPath string) string {
	return strings.TrimSuffix(snapshotPath, ".json") + ".credits.json"
}

// OrganizationsPath is where quota-cache keeps the Claude Console
// organizations the operator edits on its page, for the snapshot at
// snapshotPath: the same path with a final ".json" replaced by
// ".organizations.json", else with ".organizations.json" appended. It is
// operator data, not cache, and must move with the snapshot.
func OrganizationsPath(snapshotPath string) string {
	return strings.TrimSuffix(snapshotPath, ".json") + ".organizations.json"
}

// The states of a CreditAccount. The first rule that matches wins; see Quota
// Glance's docs/summary-contract.md.
const (
	CreditStateOK      = "ok"
	CreditStateStale   = "stale"
	CreditStatePending = "pending"
	// CreditStateMisconfigured is an item quota-cache found a problem with.
	CreditStateMisconfigured = "misconfigured"
	// CreditStateCacheTooOld is an entry quota-cache 0.1.13 wrote: it read the
	// cost report this plugin no longer uses, and names no organization.
	CreditStateCacheTooOld = "cacheTooOld"
	// CreditStateNeedsSettings is an account without a monthly credit, or with
	// neither a refill date nor a Console reading to estimate from.
	CreditStateNeedsSettings = "needsSettings"
	// CreditStateOut is an account Anthropic refused a request for, for low
	// credit, with no success since.
	CreditStateOut = "out"
)

// Data issues of an API credit account, in the order dataIssues lists them
// (CreditIssueOrder). The first is the one issue's sentence describes. A later
// quota-cache may publish a code this list does not know; a reader must keep
// it and skip it. The last three, configMonthlyUsdInvalid, configRenewsInvalid
// and adminKeyIgnored, are about quota-cache 0.1.x's claude-api-credits, which
// this quota-cache ignores, so it never raises them; they stay in Quota
// Glance's summary contract.
const (
	CreditIssueMisconfigured           = "misconfigured"
	CreditIssueCacheTooOld             = "cacheTooOld"
	CreditIssueMeterMissing            = "meterMissing"
	CreditIssueOrgNotCounted           = "orgNotCounted"
	CreditIssueNeedsCredit             = "needsCredit"
	CreditIssueNeedsRefillDate         = "needsRefillDate"
	CreditIssueRefused                 = "refused"
	CreditIssueRefusedNearlySpent      = "refusedNearlySpent"
	CreditIssueStale                   = "stale"
	CreditIssueMeterStartedLate        = "meterStartedLate"
	CreditIssueMeterGap                = "meterGap"
	CreditIssueMeterDropped            = "meterDropped"
	CreditIssueMeterFull               = "meterFull"
	CreditIssueMeterUnattributed       = "meterUnattributed"
	CreditIssueMeterRejected           = "meterRejected"
	CreditIssueUnpricedModel           = "unpricedModel"
	CreditIssueZeroCredit              = "zeroCredit"
	CreditIssueOverCredit              = "overCredit"
	CreditIssueReadingAboveCredit      = "readingAboveCredit"
	CreditIssueReadingOnRefillDay      = "readingOnRefillDay"
	CreditIssueReadingUnused           = "readingUnused"
	CreditIssueClaudeCodeRefused       = "claudeCodeRefused"
	CreditIssueNoTraffic               = "noTraffic"
	CreditIssueConfigMonthlyUSDInvalid = "configMonthlyUsdInvalid"
	CreditIssueConfigRenewsInvalid     = "configRenewsInvalid"
	CreditIssueAdminKeyIgnored         = "adminKeyIgnored"
)

// CreditIssueOrder is the order dataIssues lists the issues in.
var CreditIssueOrder = []string{
	CreditIssueMisconfigured, CreditIssueCacheTooOld, CreditIssueMeterMissing, CreditIssueOrgNotCounted,
	CreditIssueNeedsCredit, CreditIssueNeedsRefillDate, CreditIssueRefused, CreditIssueRefusedNearlySpent,
	CreditIssueStale, CreditIssueMeterStartedLate, CreditIssueMeterGap, CreditIssueMeterDropped, CreditIssueMeterFull,
	CreditIssueMeterUnattributed, CreditIssueMeterRejected, CreditIssueUnpricedModel, CreditIssueZeroCredit,
	CreditIssueOverCredit, CreditIssueReadingAboveCredit, CreditIssueReadingOnRefillDay, CreditIssueReadingUnused,
	CreditIssueClaudeCodeRefused, CreditIssueNoTraffic, CreditIssueConfigMonthlyUSDInvalid,
	CreditIssueConfigRenewsInvalid, CreditIssueAdminKeyIgnored,
}

// creditIssueRank is each code's index in CreditIssueOrder, taken once.
var creditIssueRank = func() map[string]int {
	rank := make(map[string]int, len(CreditIssueOrder))
	for i, code := range CreditIssueOrder {
		rank[code] = i
	}
	return rank
}()

// The sentences of the two issues CreditsAt can add, the same bytes
// quota-cache writes for them.
const (
	CreditSentenceStale    = "Quota Cache has not saved its meter recently, so recent spend may be missing."
	CreditSentenceMeterGap = "Quota Cache was not counting for part of this period, for example while it was off or reloading, so some spend may be missing. Enter a Console reading to correct it."
)

// Levels. The threshold lives here, not in CSS, so every client agrees.
const (
	LevelOK       = "ok"
	LevelLow      = "low"
	LevelCritical = "critical"
)

// Level thresholds, in the whole percent a page prints: 41% and up is ok, 40%
// down to 11% is low, and 10% and under is critical. Quota Glance's rows use
// the same ones.
const (
	CreditLevelCriticalAtOrBelow = 10
	CreditLevelLowAtOrBelow      = 40
)

// Where a credit or refill date comes from: MonthlyCreditSource and
// RenewsSource. "dashboard" is a value the operator entered on quota-cache's
// page. "config" was quota-cache 0.1.x's configuration; it stays in Quota
// Glance's summary contract, and this quota-cache never writes it.
const (
	CreditSourceDashboard = "dashboard"
	CreditSourceConfig    = "config"
	CreditSourceNone      = "none"
)

// What an account's left is estimated from: CreditAccount.Basis.
const (
	CreditBasisReading = "reading"
	CreditBasisCredit  = "credit"
)

// Why a stored Console reading is not used: ReadingUnusedReason.
const (
	CreditReadingBeforeRefill      = "beforeRefill"
	CreditReadingOtherOrganization = "otherOrganization"
	CreditReadingTooOld            = "tooOld"
	CreditReadingFuture            = "future"
)

// Why the values cannot be edited at all: CreditEditing.Reason. quota-cache
// writes "" or CreditEditingSettingsUnreadable; CreditEditingQuotaCache is
// Quota Glance's, which shows the values read-only.
const (
	// CreditEditingSettingsUnreadable: quota-cache cannot read its saved
	// settings; stored values do not apply.
	CreditEditingSettingsUnreadable = "settingsUnreadable"
	// CreditEditingQuotaCache: the values are edited on quota-cache's page.
	CreditEditingQuotaCache = "quotaCache"
)

// Why one account cannot be edited: NotEditableReason, beside the editing
// reasons above.
const (
	CreditNotEditableNoOrganization        = "noOrganization"
	CreditNotEditableDuplicateOrganization = "duplicateOrganization"
	CreditNotEditableOverLimit             = "overLimit"
	CreditNotEditableCacheTooOld           = "cacheTooOld"
)

// Why an organization the meter saw is not counted: CreditUnlinked.Reason.
const (
	CreditUnlinkedNotConfigured = "notConfigured"
	CreditUnlinkedOverLimit     = "overLimit"
)

// The state of the operator's saved values: CreditSettingsState.State.
const (
	CreditSettingsOK = "ok"
	// CreditSettingsUnreadable: the settings file failed a load rule. Nothing
	// in it applies and nothing is saved over it until it is moved aside.
	CreditSettingsUnreadable = "unreadable"
)

// Why there is no meter: CreditInputs.MeterError.
const (
	CreditMeterErrorMissing    = "missing"
	CreditMeterErrorUnreadable = "unreadable"
)

// The states of the organizations file: CreditOrganizations.State.
const (
	OrganizationsStateOK = "ok"
	// OrganizationsStateMissing: there is no file. No organization is listed,
	// and the first one added on the page creates it.
	OrganizationsStateMissing = "missing"
	// OrganizationsStateUnreadable: the file failed a load rule or could not
	// be read. No organization is listed and none can be edited until it is
	// moved aside or fixed.
	OrganizationsStateUnreadable = "unreadable"
)

// CreditState is the credit state file. LoadCredits reads it; CreditsAt judges
// it at a reader's instant.
type CreditState struct {
	Schema int `json:"schema"` // CreditsSchema
	// QuotaCacheVersion is the version of the quota-cache that wrote it.
	QuotaCacheVersion string `json:"quota_cache_version"`
	// ComputedAt is the instant every figure is computed at, in whole seconds.
	// The *InSeconds fields hold values as of it.
	ComputedAt time.Time `json:"computed_at"`
	// ValidUntil is the soonest instant at which a figure would change with
	// nothing else changing: a cycle's end, the day a used reading with no
	// refill date grows too old, or the time of a reading dated ahead. Null
	// when none applies. What moves before it, CreditsAt re-applies.
	ValidUntil *time.Time          `json:"valid_until"`
	Inputs     CreditInputs        `json:"inputs"`
	Settings   CreditSettingsState `json:"settings"`
	// APICredits is computed at ComputedAt with no stale applied, and null
	// when quota-cache lists no organization.
	APICredits *CreditDocument `json:"api_credits"`
	// Renewals is every stored Claude renewal date, keyed by the credential's
	// CPA auth index. Never null; empty when nothing is stored or the settings
	// cannot be read. Its precedence over a reported or estimated date is
	// SubscriptionRenewal's.
	Renewals map[string]RenewalSetting `json:"renewals"`
	// Organizations is the state of the organizations file, the list of
	// organizations api_credits counts.
	Organizations CreditOrganizations `json:"organizations"`
}

// CreditInputs describe what a state was computed from. They are diagnostic.
type CreditInputs struct {
	// SnapshotWrittenAt is the snapshot's written_at; null when it has none.
	SnapshotWrittenAt *time.Time `json:"snapshot_written_at"`
	// MeterFlushedAt is the meter's flushed_at; null without a meter.
	MeterFlushedAt *time.Time `json:"meter_flushed_at"`
	// MeterError is "", or a CreditMeterError value without a meter.
	MeterError string `json:"meter_error"`
	// SettingsRevision is the settings file's revision, "" without one.
	SettingsRevision string `json:"settings_revision"`
}

// CreditSettingsState is the state of the operator's saved values.
type CreditSettingsState struct {
	// State is a CreditSettings value.
	State string `json:"state"`
	// Editable is State == CreditSettingsOK.
	Editable bool `json:"editable"`
	// Revision is the settings file's revision, "" without one.
	Revision string `json:"revision"`
}

// CreditOrganizations is the state of the organizations file, the Claude
// Console organizations the operator lists on quota-cache's page. Nothing
// else lists one.
type CreditOrganizations struct {
	// State is the organizations file's: an OrganizationsState value.
	State string `json:"state"`
	// Revision is the organizations file's revision, "" without one.
	Revision string `json:"revision"`
	// ConfigItemsIgnored counts the items claude-api-credits lists in
	// quota-cache's configuration. quota-cache 0.1.x read its organizations
	// from there; this one ignores them.
	ConfigItemsIgnored int `json:"config_items_ignored"`
}

// LoadCredits reads and validates a credit state file: a regular file (Lstat),
// at most MaxCreditsBytes, valid JSON, Schema == CreditsSchema. Nil maps and
// slices are returned empty. ErrCreditsMissing when there is no file;
// ErrUnavailable for anything else.
func LoadCredits(path string) (*CreditState, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrCreditsMissing
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, ErrUnavailable
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, MaxCreditsBytes+1))
	if err != nil || len(raw) > MaxCreditsBytes {
		return nil, ErrUnavailable
	}
	var state CreditState
	if json.Unmarshal(raw, &state) != nil || state.Schema != CreditsSchema {
		return nil, ErrUnavailable
	}
	state.fill()
	return &state, nil
}

// fill replaces the nil maps and slices a file may hold with empty ones, so a
// reader can range over and index into a loaded state without checking.
func (s *CreditState) fill() {
	if s.Renewals == nil {
		s.Renewals = map[string]RenewalSetting{}
	}
	d := s.APICredits
	if d == nil {
		return
	}
	d.Accounts, d.Unlinked, d.Orphans = nonNil(d.Accounts), nonNil(d.Unlinked), nonNil(d.Orphans)
	if d.Meter != nil {
		d.Meter.Gaps = nonNil(d.Meter.Gaps)
	}
	if r := d.Pool.NextRefill; r != nil {
		r.AccountIDs = nonNil(r.AccountIDs)
	}
	for i := range d.Accounts {
		a := &d.Accounts[i]
		a.DataIssues, a.Unpriced = nonNil(a.DataIssues), nonNil(a.Unpriced)
	}
}

// Current reports whether the state can be used as it stands at now: computed
// at most maxAge before now, and now before ValidUntil.
func (s *CreditState) Current(now time.Time, maxAge time.Duration) bool {
	return s != nil && now.Sub(s.ComputedAt) <= maxAge && (s.ValidUntil == nil || now.Before(*s.ValidUntil))
}

// CreditsAt is the state as a reader at now sees it. It returns a deep copy and
// never mutates s. staleAfter <= 0 disables the two age rules.
//
// It re-applies only the rules whose inputs are published facts plus now, all
// to api_credits:
//
//  1. Expired: the state is older than staleAfter, or now has reached
//     ValidUntil plus CreditBoundaryGrace.
//  2. Stale: the meter was saved longer than staleAfter ago, or the state is
//     expired. The meter is marked stale, and every account that reached the
//     stale rule (ok, stale, out or needsSettings) gains the stale issue; an
//     anchored one (spentSinceEpoch set) becomes a lower bound; ok becomes
//     stale.
//  3. A meter stopped at least MeterBriefGap ago: every such account that is
//     anchored gains the meterGap issue and becomes a lower bound.
//  4. When 2 or 3 added the issue now first, the issue sentence becomes
//     CreditSentenceStale or CreditSentenceMeterGap.
//  5. The pool is a lower bound when any counted account is.
//  6. The countdowns count down to now, never below zero.
//
// Nothing else changes: no amount, count, level or other issue. Stale and
// gaps only grow with now until quota-cache publishes again, so adding is
// enough.
//
// An issue is added just before the first listed code that CreditIssueOrder
// places after it, or last. A code this reader's CreditIssueOrder does not know
// (a later quota-cache's) is skipped: never compared, moved or dropped.
func CreditsAt(s *CreditState, now time.Time, staleAfter time.Duration) *CreditState {
	if s == nil {
		return nil
	}
	out := s.clone()
	d := out.APICredits
	if d == nil {
		return out
	}
	now = now.Truncate(time.Second)
	expired := (staleAfter > 0 && now.Sub(s.ComputedAt) > staleAfter) ||
		(s.ValidUntil != nil && !now.Before(s.ValidUntil.Add(CreditBoundaryGrace)))
	stale := expired || (staleAfter > 0 && d.Meter != nil && now.Sub(time.Unix(d.Meter.UpdatedAtEpoch, 0)) > staleAfter)
	gap := d.Meter != nil && d.Meter.StoppedAtEpoch != nil && now.Sub(time.Unix(*d.Meter.StoppedAtEpoch, 0)) >= MeterBriefGap
	if stale && d.Meter != nil {
		d.Meter.Stale = true
	}
	for i := range d.Accounts {
		a := &d.Accounts[i]
		switch a.State {
		case CreditStateOK, CreditStateStale, CreditStateOut, CreditStateNeedsSettings:
		default:
			continue
		}
		anchored := a.SpentSinceEpoch != nil
		var addedStale, addedGap bool
		if stale {
			a.DataIssues, addedStale = insertCreditIssue(a.DataIssues, CreditIssueStale)
			a.LowerBound = a.LowerBound || anchored
			if a.State == CreditStateOK {
				a.State = CreditStateStale
			}
		}
		if gap && anchored {
			a.DataIssues, addedGap = insertCreditIssue(a.DataIssues, CreditIssueMeterGap)
			a.LowerBound = true
		}
		switch {
		case addedStale && a.DataIssues[0] == CreditIssueStale:
			a.Issue = CreditSentenceStale
		case addedGap && a.DataIssues[0] == CreditIssueMeterGap:
			a.Issue = CreditSentenceMeterGap
		}
	}
	d.Pool.LowerBound = false
	for _, a := range d.Accounts {
		d.Pool.LowerBound = d.Pool.LowerBound || (a.Counted && a.LowerBound)
	}
	until := func(epoch int64) int64 { return max(0, epoch-now.Unix()) }
	for i := range d.Accounts {
		if a := &d.Accounts[i]; a.RenewsAtEpoch != nil {
			in := until(*a.RenewsAtEpoch)
			a.RenewsInSeconds = &in
		}
	}
	if r := d.Pool.NextRefill; r != nil {
		r.RefillInSeconds = until(r.RefillAtEpoch)
	}
	if d.Pool.FullAtEpoch != nil {
		in := until(*d.Pool.FullAtEpoch)
		d.Pool.FullInSeconds = &in
	}
	return out
}

// insertCreditIssue adds code to issues by CreditsAt's rule, and reports
// whether it was added. issues is the caller's own copy.
func insertCreditIssue(issues []string, code string) ([]string, bool) {
	if slices.Contains(issues, code) {
		return issues, false
	}
	rank := creditIssueRank[code]
	for i, have := range issues {
		if r, known := creditIssueRank[have]; known && r > rank {
			return slices.Insert(issues, i, code), true
		}
	}
	return append(issues, code), true
}

// clone is a deep copy of the state: nothing in it shares memory with s.
func (s *CreditState) clone() *CreditState {
	out := *s
	out.ValidUntil = clonePointer(s.ValidUntil)
	out.Inputs.SnapshotWrittenAt = clonePointer(s.Inputs.SnapshotWrittenAt)
	out.Inputs.MeterFlushedAt = clonePointer(s.Inputs.MeterFlushedAt)
	out.APICredits = s.APICredits.clone()
	out.Renewals = maps.Clone(s.Renewals)
	return &out
}

func (d *CreditDocument) clone() *CreditDocument {
	if d == nil {
		return nil
	}
	out := *d
	if m := d.Meter; m != nil {
		meter := *m
		meter.StoppedAtEpoch, meter.LastDroppedEpoch = clonePointer(m.StoppedAtEpoch), clonePointer(m.LastDroppedEpoch)
		meter.LastUnattributedEpoch, meter.LastRejectedEpoch = clonePointer(m.LastUnattributedEpoch), clonePointer(m.LastRejectedEpoch)
		meter.Gaps = slices.Clone(m.Gaps)
		out.Meter = &meter
	}
	if r := d.Pool.NextRefill; r != nil {
		refill := *r
		refill.AccountIDs = slices.Clone(r.AccountIDs)
		out.Pool.NextRefill = &refill
	}
	out.Pool.FullAtEpoch, out.Pool.FullInSeconds = clonePointer(d.Pool.FullAtEpoch), clonePointer(d.Pool.FullInSeconds)
	out.Accounts = slices.Clone(d.Accounts)
	for i := range out.Accounts {
		out.Accounts[i] = d.Accounts[i].clone()
	}
	out.Unlinked, out.Orphans = slices.Clone(d.Unlinked), slices.Clone(d.Orphans)
	return &out
}

func (a CreditAccount) clone() CreditAccount {
	a.SpentSinceEpoch = clonePointer(a.SpentSinceEpoch)
	a.CycleStartEpoch, a.RenewsAtEpoch, a.RenewsInSeconds = clonePointer(a.CycleStartEpoch), clonePointer(a.RenewsAtEpoch), clonePointer(a.RenewsInSeconds)
	a.Reading = clonePointer(a.Reading)
	a.Unpriced = slices.Clone(a.Unpriced)
	a.Refusals.LastAtEpoch, a.Refusals.ClaudeCodeLastAtEpoch = clonePointer(a.Refusals.LastAtEpoch), clonePointer(a.Refusals.ClaudeCodeLastAtEpoch)
	a.MeterSinceEpoch, a.LastSeenEpoch = clonePointer(a.MeterSinceEpoch), clonePointer(a.LastSeenEpoch)
	a.DataIssues = slices.Clone(a.DataIssues)
	a.Settings.Reading, a.Settings.UpdatedAtEpoch = clonePointer(a.Settings.Reading), clonePointer(a.Settings.UpdatedAtEpoch)
	return a
}

// clonePointer is a new pointer to a copy of *p, and nil for nil. Every type it
// is used with holds no pointer of its own.
func clonePointer[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// The types below are api_credits. Their names and their order are Quota
// Glance's summary contract (docs/summary-contract.md), which shows the block
// as its apiCredits.

// CreditDocument is the monthly Claude API credit of every Console organization
// quota-cache lists, estimated from the CPA traffic its meter counted, and
// pooled.
//
// Every amount is an estimate. Anthropic reports neither the credit nor what
// is left of it to anything this plugin can read, so the credit and its refill
// date are what the operator entered on quota-cache's page, the spend is the meter's
// tokens at the list prices of Pricing.AsOf, and a Console reading the
// operator types in is the anchor that corrects it. An account whose figures
// could be missing spend says so with LowerBound.
type CreditDocument struct {
	Title string `json:"title"`
	// Currency is ISO 4217; every amount below is in it.
	Currency string        `json:"currency"`
	Pricing  CreditPricing `json:"pricing"`
	// Meter is quota-cache's API meter, null when its file is missing or
	// cannot be read.
	Meter   *CreditMeter  `json:"meter"`
	Editing CreditEditing `json:"editing"`
	Pool    CreditPool    `json:"pool"`
	// Accounts lists every organization quota-cache lists, counted or not,
	// sorted by order, then label, then id. Do not re-sort.
	Accounts []CreditAccount `json:"accounts"`
	// Unlinked lists organizations that sent API traffic through CPA while no
	// counted item names them, newest lastSeen first. Always an array.
	Unlinked []CreditUnlinked `json:"unlinked"`
	// Orphans are stored dashboard values for an account no longer listed,
	// which the page offers to remove. Always an array.
	Orphans []CreditOrphan `json:"orphans"`
}

// CreditPricing says which prices the estimates use.
type CreditPricing struct {
	// AsOf is the day the embedded price table was read, YYYY-MM-DD.
	AsOf string `json:"asOf"`
	// Source is the pricing page it was read from.
	Source string `json:"source"`
	// CacheWrites is the cache-write rate every amount uses: "5m".
	CacheWrites string `json:"cacheWrites"`
}

// CreditMeter is quota-cache's API meter as last saved.
type CreditMeter struct {
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
	Gaps []CreditGap `json:"gaps"`
}

// CreditGap is one period the meter was not counting.
type CreditGap struct {
	FromEpoch int64  `json:"fromEpoch"`
	ToEpoch   int64  `json:"toEpoch"`
	Reason    string `json:"reason"`
}

// CreditEditing says whether the page may offer to change these values.
type CreditEditing struct {
	Available bool `json:"available"`
	// Reason is "" when available, else one of the CreditEditing constants:
	// settingsUnreadable from quota-cache, when its saved settings could not
	// be read (stored values do not apply), or quotaCache from Quota Glance,
	// which shows the values read-only because they are edited on
	// quota-cache's page.
	Reason string `json:"reason"`
}

// CreditPool sums the counted accounts: those in state ok, stale or out.
// Left is the sum of each account's own left, so one organization's overage
// never consumes another's credit.
type CreditPool struct {
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
	NextRefill *CreditRefill `json:"nextRefill"`
	// FullAtEpoch is the latest refill among the counted accounts that have
	// used any credit: when the pool reads full again if nothing more is spent.
	FullAtEpoch   *int64 `json:"fullAtEpoch"`
	FullInSeconds *int64 `json:"fullInSeconds"`
	// AccountCount == len(accounts) == CountedCount + MissingCount.
	AccountCount int `json:"accountCount"`
	CountedCount int `json:"countedCount"`
	MissingCount int `json:"missingCount"`
}

// CreditRefill is one refill instant and what it returns to the pool,
// assuming each refilling organization's next credit is its current amount.
type CreditRefill struct {
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

// CreditAccount is one Console organization's credit this cycle.
type CreditAccount struct {
	// ID is quota-cache's name for the organization: "org-<12 hex>" from its
	// Organization ID (APICreditOrgAccount). The dashboard's stored values
	// are keyed by it. "item-<n>", an item of quota-cache 0.1.x's
	// configuration it could not link, stays in Quota Glance's summary
	// contract; this quota-cache never lists one.
	ID string `json:"id"`
	// Label is the organization's label on quota-cache's page.
	Label string `json:"label"`
	// Order is the organization's position in that list.
	Order int `json:"order"`
	// OrganizationID is the Organization ID as NormalizeOrganizationID
	// returns it.
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
	// refill date come from: "dashboard" or "none". See CreditSourceConfig.
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
	Reading *CreditReading `json:"reading"`
	// CacheWriteExtra is how much more the estimated window would cost were
	// every cache write a 1-hour one. CacheWriteExtraText is "" below $0.01.
	CacheWriteExtra     float64 `json:"cacheWriteExtra"`
	CacheWriteExtraText string  `json:"cacheWriteExtraText"`
	// Unpriced is every model in the estimated window with no listed price,
	// most tokens first. Always an array.
	Unpriced []CreditUnpriced `json:"unpriced"`
	Refusals CreditRefusals   `json:"refusals"`
	// MeterSinceEpoch is when the meter began counting this organization,
	// LastSeenEpoch its latest request.
	MeterSinceEpoch *int64   `json:"meterSinceEpoch"`
	LastSeenEpoch   *int64   `json:"lastSeenEpoch"`
	DataIssues      []string `json:"dataIssues"`
	// Issue is one plain-language sentence about the account, already
	// written, and "" when there is nothing to say.
	Issue    string         `json:"issue"`
	Settings CreditSettings `json:"settings"`
}

// CreditReading is the Console reading an account's Left is taken from.
type CreditReading struct {
	Remaining      float64 `json:"remaining"`
	RemainingText  string  `json:"remainingText"`
	AtEpoch        int64   `json:"atEpoch"`
	EnteredAtEpoch int64   `json:"enteredAtEpoch"`
	// SpentSince is the metered spend since the start of the reading's hour.
	SpentSince     float64 `json:"spentSince"`
	SpentSinceText string  `json:"spentSinceText"`
}

// CreditRefusals counts Anthropic's low-credit refusals of this
// organization's requests: without a Claude Code session, and with one.
type CreditRefusals struct {
	Total                 uint64 `json:"total"`
	LastAtEpoch           *int64 `json:"lastAtEpoch"`
	ClaudeCodeTotal       uint64 `json:"claudeCodeTotal"`
	ClaudeCodeLastAtEpoch *int64 `json:"claudeCodeLastAtEpoch"`
}

// CreditSettings is what the editor shows and sends back for one account.
type CreditSettings struct {
	Editable bool `json:"editable"`
	// NotEditableReason is "" when editable, else why: noOrganization,
	// duplicateOrganization, overLimit or cacheTooOld for this account (the
	// CreditNotEditable constants), or the editing reason when nothing can be
	// edited (settingsUnreadable or quotaCache).
	NotEditableReason string `json:"notEditableReason"`
	// Revision is the stored entry's, "" with nothing stored; a save sends it
	// back as baseRevision.
	Revision string `json:"revision"`
	// MonthlyUSD and Renews are the values set here, "" when none.
	MonthlyUSD string `json:"monthlyUsd"`
	// ConfigMonthlyUSD and ConfigRenews, and their Invalid flags, were
	// quota-cache 0.1.x's configured values. They stay in Quota Glance's
	// summary contract; this quota-cache leaves them "" and false.
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

// CreditUnlinked is an organization that sent API traffic through CPA
// while no counted item named it.
type CreditUnlinked struct {
	OrganizationID string `json:"organizationId"`
	FirstSeenEpoch int64  `json:"firstSeenEpoch"`
	LastSeenEpoch  int64  `json:"lastSeenEpoch"`
	Requests       uint64 `json:"requests"`
	// Reason is "overLimit" when an item past the first 16 names it, else
	// "notConfigured".
	Reason string `json:"reason"`
}

// CreditOrphan is stored dashboard values for an account no longer listed.
type CreditOrphan struct {
	ID             string `json:"id"`
	MonthlyUSD     string `json:"monthlyUsd"`
	Renews         string `json:"renews"`
	HasReading     bool   `json:"hasReading"`
	Revision       string `json:"revision"`
	UpdatedAtEpoch int64  `json:"updatedAtEpoch"`
}

// CreditUnpriced is one model the price table has no price for.
type CreditUnpriced struct {
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
