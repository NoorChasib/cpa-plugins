package client

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The API meter is quota-cache's count of Claude API-key traffic per Console
// organization, taken from the usage records CPA hands its usage plugins. It is
// saved to its own file beside the snapshot (MeterPath), never into the
// snapshot, so the snapshot's readers, its size limit and its write cadence are
// unaffected by it.
//
// quota-cache counts tokens and never prices them; a consumer prices them with
// its own table. Every time in a meter is UTC, truncated to the second.

// Meter file limits. They bound the file to MaxMeterBytes whatever traffic
// CPA carries; quota-cache enforces them and a reader may rely on them.
const (
	MeterSchema            = 1       // the only schema LoadMeter accepts
	MeterDays              = 40      // daily buckets: today and the 40 days before
	MeterHours             = 72      // hourly buckets: this hour and the 72 before
	MaxMeterOrganizations  = 32      // 16 linked + 16 dormant
	MaxMeterUsagePerBucket = 16      // distinct (Model, Prompt) pairs in one bucket; past it, usage goes to MeterOtherModel
	MaxMeterUsageEntries   = 6000    // usage entries across every bucket of every organization
	MaxMeterUnlinked       = 8       // unlinked organizations listed
	MaxMeterAuths          = 64      // auth indexes remembered
	MaxMeterGaps           = 8       // gaps kept
	MaxMeterBytes          = 2 << 20 // the largest meter file LoadMeter reads
)

// Usage keys that are not a model's own name or prompt size.
const (
	// MeterOtherModel is the model of an entry that sums requests whose model
	// name could not be kept: one NormalizeMeterModel refused, or a pair that
	// arrived when its bucket already held MaxMeterUsagePerBucket entries.
	// It is not a model, and a consumer cannot price it.
	MeterOtherModel = "(other)"
	// MeterPromptOver100K is the prompt class of a Claude Haiku 5.5 request
	// whose prompt (input + cache read + cache write) is over 100,000 tokens,
	// which Anthropic prices higher. Every other request's class is "".
	MeterPromptOver100K = "over_100k"
)

// Why the meter stopped: APIMeter.StopReason, and the Reason of the gap that
// stop leaves once the meter starts again.
const (
	MeterStopShutdown = "shutdown" // CPA shut quota-cache down
	MeterStopQuiesce  = "quiesce"  // CPA quiesced quota-cache, to replace or unload it
	MeterStopDisabled = "disabled" // quota-cache was configured with enabled: false
	MeterStopNoItems  = "no_items" // claude-api-credits was left with no items
	MeterStopFailed   = "failed"   // the meter stopped itself after an internal error
	// MeterGapUncleanStop is the reason of a gap whose meter never said it
	// stopped: the process ended without a final save. The gap runs from the
	// last save.
	MeterGapUncleanStop = "unclean_stop"
)

// MeterBriefGap separates a brief stop from a real one. A gap shorter than
// this is a CPA restart, a plugin update or a reload, during which little or
// no traffic can have passed unseen; a consumer ignores it. quota-cache still
// records every gap, brief ones included.
const MeterBriefGap = 5 * time.Minute

// haikuPromptTier is the prompt size, in tokens, above which Claude Haiku 5.5
// is priced higher.
const haikuPromptTier = 100_000

// ErrMeterMissing is LoadMeter's error when there is no meter file: the
// quota-cache that writes the snapshot predates the meter, or has not saved
// one yet.
var ErrMeterMissing = errors.New("api meter file missing")

// MeterPath is where quota-cache saves its API meter for the snapshot at
// snapshotPath: the same path with a final ".json" replaced by ".meter.json",
// else with ".meter.json" appended. For DefaultPath it is
// plugins/data/quota-cache/snapshot.meter.json.
func MeterPath(snapshotPath string) string {
	return strings.TrimSuffix(snapshotPath, ".json") + ".meter.json"
}

// LoadMeter reads and validates a meter file: a regular file (Lstat), at most
// MaxMeterBytes, valid JSON, Schema == MeterSchema. Nil maps and slices are
// returned empty. ErrMeterMissing when there is no file; ErrUnavailable for
// anything else.
func LoadMeter(path string) (*APIMeter, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrMeterMissing
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, ErrUnavailable
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, MaxMeterBytes+1))
	if err != nil || len(raw) > MaxMeterBytes {
		return nil, ErrUnavailable
	}
	var meter APIMeter
	if json.Unmarshal(raw, &meter) != nil || meter.Schema != MeterSchema {
		return nil, ErrUnavailable
	}
	meter.fill()
	return &meter, nil
}

// APIMeter is the meter file. Its counters are cumulative since Since, across
// restarts; the buckets of each organization hold its usage.
type APIMeter struct {
	Schema int `json:"schema"` // MeterSchema
	// Since is when this file began counting: the first start of the meter
	// that wrote it.
	Since time.Time `json:"since"`
	// StartedAt is when this run of the meter started.
	StartedAt time.Time `json:"started_at"`
	// FlushedAt is when these figures were written. quota-cache saves at least
	// every 10 minutes while counting, so a FlushedAt older than that, with no
	// StoppedAt, means the meter ended without saying so.
	FlushedAt time.Time `json:"flushed_at"`
	// StoppedAt is set while the meter is stopped, with StopReason, a
	// MeterStop* value. The next start turns them into a gap.
	StoppedAt  *time.Time `json:"stopped_at,omitempty"`
	StopReason string     `json:"stop_reason,omitempty"`
	// Restarts counts the starts that carried on from this file.
	Restarts uint64 `json:"restarts,omitempty"`
	// Received counts usage records received while counting, of every
	// provider and auth type.
	Received uint64 `json:"received"`
	// Counted counts the records attributed to a linked organization.
	Counted uint64 `json:"counted"`
	// Foreign counts Claude API-key records from a service other than
	// Anthropic: a success with no anthropic-organization-id header, or any
	// record on an auth known to send none.
	Foreign uint64 `json:"foreign,omitempty"`
	// Rejected counts records that could not be decoded or carried a negative
	// token count.
	Rejected       uint64     `json:"rejected,omitempty"`
	LastRejectedAt *time.Time `json:"last_rejected_at,omitempty"`
	// Dropped counts records dropped because the meter could not keep up.
	Dropped       uint64     `json:"dropped,omitempty"`
	LastDroppedAt *time.Time `json:"last_dropped_at,omitempty"`
	// Unattributed counts failed Claude API-key records with no organization
	// header on an auth the meter does not know, so their organization is
	// unknown. LastUnattributedAt is the last such record that carried tokens.
	Unattributed       uint64     `json:"unattributed,omitempty"`
	LastUnattributedAt *time.Time `json:"last_unattributed_at,omitempty"`
	// Gaps are the periods the meter was not counting, oldest first, at most
	// MaxMeterGaps. Never null.
	Gaps []MeterGap `json:"gaps"`
	// Organizations is keyed by lower-cased organization id. Never null.
	Organizations map[string]MeterOrganization `json:"organizations"`
	// Unlinked lists organizations that sent traffic no linked item names,
	// newest LastSeenAt first, at most MaxMeterUnlinked. Never null.
	Unlinked []MeterUnlinked `json:"unlinked"`
	// Auths maps a CPA auth index to the organization last seen on it, at most
	// MaxMeterAuths. Never null.
	Auths map[string]MeterAuth `json:"auths"`
}

// fill replaces the nil maps and slices a file may hold with empty ones, so a
// reader can range over and index into a loaded meter without checking.
func (m *APIMeter) fill() {
	m.Gaps = nonNil(m.Gaps)
	m.Unlinked = nonNil(m.Unlinked)
	if m.Organizations == nil {
		m.Organizations = map[string]MeterOrganization{}
	}
	if m.Auths == nil {
		m.Auths = map[string]MeterAuth{}
	}
	for id, org := range m.Organizations {
		org.Days, org.Hours = nonNil(org.Days), nonNil(org.Hours)
		for _, buckets := range [][]MeterBucket{org.Days, org.Hours} {
			for i := range buckets {
				buckets[i].Usage = nonNil(buckets[i].Usage)
			}
		}
		m.Organizations[id] = org
	}
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// MeterGap is a period in which the meter was not counting. Usage records CPA
// published in it are lost to the meter, so spend in it is missing.
type MeterGap struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Reason string    `json:"reason"` // a MeterStop* value or MeterGapUncleanStop
}

// Brief reports whether the gap is shorter than MeterBriefGap: a restart, an
// update or a reload, which a consumer ignores.
func (g MeterGap) Brief() bool { return g.To.Sub(g.From) < MeterBriefGap }

// MeterOrganization is one Console organization's count.
type MeterOrganization struct {
	// Since is when counting began for this organization.
	Since time.Time `json:"since"`
	// UnlinkedAt is set while the organization is dormant: no item links it.
	// A dormant organization is still bucketed, so relinking it loses nothing.
	UnlinkedAt *time.Time `json:"unlinked_at,omitempty"`
	// LastSeenAt and LastSuccessAt are the latest attempt and the latest
	// successful attempt.
	LastSeenAt    *time.Time `json:"last_seen_at,omitempty"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	// Refusals counts Anthropic's low-credit refusals of requests with no
	// Claude Code session.
	Refusals      uint64     `json:"refusals,omitempty"`
	LastRefusalAt *time.Time `json:"last_refusal_at,omitempty"`
	// ClaudeCodeRefusals counts low-credit refusals of requests whose session
	// was a Claude Code-based client's ("claude:"): Claude Code, which the
	// credit does not cover, or the Agent SDK, which it does.
	ClaudeCodeRefusals      uint64     `json:"claude_code_refusals,omitempty"`
	LastClaudeCodeRefusalAt *time.Time `json:"last_claude_code_refusal_at,omitempty"`
	// LastOverflowAt is the latest time usage could not be kept under its own
	// model: a bucket or the whole meter was full.
	LastOverflowAt *time.Time `json:"last_overflow_at,omitempty"`
	// Days and Hours are the UTC day and hour buckets, oldest first. Never
	// null.
	Days  []MeterBucket `json:"days"`
	Hours []MeterBucket `json:"hours"`
}

// MeterBucket is one day's or one hour's usage. A request lands in the day
// bucket and the hour bucket that contain its completion, under the same
// (Model, Prompt) key in both, or in neither.
type MeterBucket struct {
	Start time.Time `json:"start"` // 00:00 UTC (days) or hh:00 UTC (hours)
	// Usage is sorted by Model, then Prompt, and never empty: at most
	// MaxMeterUsagePerBucket pairs, plus one MeterOtherModel entry for the
	// pairs that arrived after the bucket held that many.
	Usage []MeterUsage `json:"usage"`
}

// MeterUsage is the attempts and tokens of one (Model, Prompt) pair. Every
// count saturates at math.MaxUint64.
type MeterUsage struct {
	Model    string `json:"model"`              // a NormalizeMeterModel result, or MeterOtherModel
	Prompt   string `json:"prompt,omitempty"`   // "" or MeterPromptOver100K
	Requests uint64 `json:"requests,omitempty"` // successful attempts
	Failed   uint64 `json:"failed,omitempty"`   // failed attempts, refusals included
	// Token counts as Anthropic bills them. Input excludes cache reads and
	// writes; Output includes thinking. A failed attempt's tokens are counted.
	Input      uint64 `json:"input,omitempty"`
	Output     uint64 `json:"output,omitempty"`
	CacheRead  uint64 `json:"cache_read,omitempty"`
	CacheWrite uint64 `json:"cache_write,omitempty"` // 5-minute and 1-hour writes together
}

// MeterUnlinked is an organization that sent Claude API-key traffic through
// CPA while no item linked it.
type MeterUnlinked struct {
	OrganizationID string    `json:"organization_id"`
	FirstSeenAt    time.Time `json:"first_seen_at"`
	LastSeenAt     time.Time `json:"last_seen_at"`
	Requests       uint64    `json:"requests"`
}

// MeterAuth is what the meter learned about one CPA auth: the organization its
// responses named. It lets a failed request that arrives without the
// organization header still be attributed.
type MeterAuth struct {
	OrganizationID string    `json:"organization_id"` // "" for an auth whose responses never name one: not Anthropic's
	SeenAt         time.Time `json:"seen_at"`
}

// MeterDayStart is 00:00 UTC of t's UTC day: the Start of the day bucket that
// holds t.
func MeterDayStart(t time.Time) time.Time {
	year, month, day := t.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// MeterHourStart is the start of t's UTC hour: the Start of the hour bucket
// that holds t.
func MeterHourStart(t time.Time) time.Time {
	t = t.UTC()
	year, month, day := t.Date()
	return time.Date(year, month, day, t.Hour(), 0, 0, 0, time.UTC)
}

// MeterPromptClass is MeterPromptOver100K when model contains
// "claude-haiku-5-5" and promptTokens > 100000, else "".
func MeterPromptClass(model string, promptTokens uint64) string {
	if promptTokens > haikuPromptTier && strings.Contains(model, "claude-haiku-5-5") {
		return MeterPromptOver100K
	}
	return ""
}

var meterModelShape = regexp.MustCompile(`^[a-z0-9][a-z0-9._:@/-]{0,63}$`)

// NormalizeMeterModel picks responseModel when non-empty after trimming, else
// model; lower-cases; returns MeterOtherModel unless the result matches
// ^[a-z0-9][a-z0-9._:@/-]{0,63}$.
func NormalizeMeterModel(model, responseModel string) string {
	name := strings.TrimSpace(responseModel)
	if name == "" {
		name = strings.TrimSpace(model)
	}
	// The shape is at most 64 ASCII bytes, so a longer name is refused before
	// it is copied.
	if len(name) > 64 {
		return MeterOtherModel
	}
	name = strings.ToLower(name)
	if !meterModelShape.MatchString(name) {
		return MeterOtherModel
	}
	return name
}

// MergeMeterUsage adds u's counts (saturating) into the entry with the same
// (Model, Prompt), or inserts it in sorted position. No cap: callers cap.
//
// into must be sorted by Model, then Prompt, as a bucket's Usage is. Like
// append, it may change into's backing array, and the result must be used.
func MergeMeterUsage(into []MeterUsage, u MeterUsage) []MeterUsage {
	i, found := slices.BinarySearchFunc(into, u, compareMeterUsage)
	if !found {
		return slices.Insert(into, i, u)
	}
	e := &into[i]
	e.Requests = addSaturating(e.Requests, u.Requests)
	e.Failed = addSaturating(e.Failed, u.Failed)
	e.Input = addSaturating(e.Input, u.Input)
	e.Output = addSaturating(e.Output, u.Output)
	e.CacheRead = addSaturating(e.CacheRead, u.CacheRead)
	e.CacheWrite = addSaturating(e.CacheWrite, u.CacheWrite)
	return into
}

func compareMeterUsage(a, b MeterUsage) int {
	if c := strings.Compare(a.Model, b.Model); c != 0 {
		return c
	}
	return strings.Compare(a.Prompt, b.Prompt)
}

func addSaturating(a, b uint64) uint64 {
	if sum := a + b; sum >= a {
		return sum
	}
	return math.MaxUint64
}

// UsageFromDay sums every day bucket with Start >= dayStart, sorted by Model,
// then Prompt. Never nil.
//
// With dayStart a cycle's start, which is always 00:00 UTC, this is the
// cycle's usage exactly, as far as the meter saw it.
func (o MeterOrganization) UsageFromDay(dayStart time.Time) []MeterUsage {
	usage := []MeterUsage{}
	for _, day := range o.Days {
		if day.Start.Before(dayStart) {
			continue
		}
		for _, u := range day.Usage {
			usage = MergeMeterUsage(usage, u)
		}
	}
	return usage
}

// UsageInHours sums hour buckets with from <= Start < to, sorted by Model,
// then Prompt, never nil. covered is false when from is before
// MeterHourStart(flushedAt) - MeterHours*time.Hour, or to is after
// MeterHourStart(flushedAt) + time.Hour: the meter flushed at flushedAt no
// longer keeps, or does not yet have, every hour of the window.
func (o MeterOrganization) UsageInHours(from, to, flushedAt time.Time) (usage []MeterUsage, covered bool) {
	usage = []MeterUsage{}
	for _, hour := range o.Hours {
		if hour.Start.Before(from) || !hour.Start.Before(to) {
			continue
		}
		for _, u := range hour.Usage {
			usage = MergeMeterUsage(usage, u)
		}
	}
	flushHour := MeterHourStart(flushedAt)
	covered = !from.Before(flushHour.Add(-MeterHours*time.Hour)) && !to.After(flushHour.Add(time.Hour))
	return usage, covered
}
