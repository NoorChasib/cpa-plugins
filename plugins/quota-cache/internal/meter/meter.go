// Package meter counts Claude API-key traffic per Console organization from
// the usage records CPA hands its usage plugins, and keeps the count in the
// file beside the snapshot that client.MeterPath names. It prices nothing and
// sends nothing: a consumer prices the tokens with its own table.
package meter

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// Logger is the one host callback the meter makes: a line in CPA's log.
type Logger interface {
	Log(context.Context, string, string, map[string]any)
}

const (
	// intakeSize bounds the records waiting for the worker. CPA delivers
	// usage records on one goroutine, synchronously, so Offer must never
	// block; a full intake drops the record and counts the drop.
	intakeSize = 4096
	// flushEvery is how often the worker looks at whether to save, and so
	// the most an unclean exit can lose.
	flushEvery = time.Minute
	// heartbeat is the longest the file goes unwritten while the meter runs,
	// so flushed_at shows a reader the meter is alive.
	heartbeat = 10 * time.Minute
	// warnEvery rate-limits the drop and save warnings.
	warnEvery = 10 * time.Minute
	// retention ages dormant organizations, unlinked organizations, auths
	// and gaps: as long as the day buckets reach back.
	retention = client.MeterDays * 24 * time.Hour
	// oversizeDays is how far back the oversize rule keeps day buckets: a
	// credit cycle, so the current cycle's sums survive.
	oversizeDays = 31 * 24 * time.Hour
	// maxAuthIndex is the longest auth index kept; CPA's are 16 hex digits.
	maxAuthIndex = 64
)

// usageWire is the part of CPA's pluginapi.UsageRecord the meter reads. It
// has no Source, APIKey, AuthID, BaseURL, Alias, TraceID, RequestID or
// ParentSessionID field, so the raw upstream key, the client key and the
// credential's name are never materialized. CPA marshals the record with no
// JSON tags, so every key is the Go field name; encoding/json matches keys
// case-insensitively, which also covers the organization header.
type usageWire struct {
	Provider, AuthIndex, AuthType, Model, ResponseModel, SessionID string
	// RequestedAt is raw so that a timestamp that is not a string never
	// rejects the record; the received time stands in for it.
	RequestedAt json.RawMessage
	Latency     int64
	Failed      bool
	Failure     struct {
		StatusCode int
		Body       string
	}
	Detail          struct{ InputTokens, OutputTokens, CacheReadTokens, CacheCreationTokens int64 }
	ResponseHeaders struct {
		OrganizationID []string `json:"Anthropic-Organization-Id"`
	}
}

// event is what the worker counts: one Claude API-key attempt, reduced to
// the facts the meter keeps. The failure body, the session id and the
// headers are gone by the time one exists.
type event struct {
	at                                   time.Time // completion time, clamped to the received time, truncated to the second
	authIndex                            string    // CPA's auth index, "" when absent or over maxAuthIndex bytes
	org                                  string    // the organization header, normalized; "" when absent or invalid
	model                                string    // client.NormalizeMeterModel(Model, ResponseModel)
	prompt                               string    // client.MeterPromptClass(model, input+cacheRead+cacheWrite)
	failed, refusal, claudeCode          bool
	input, output, cacheRead, cacheWrite uint64
}

// Meter is one running count. Offer runs on CPA's usage goroutine and only
// decodes and enqueues; the worker owns the count, the file and the ticker.
type Meter struct {
	path string
	log  Logger

	// stopped is read by Offer without a lock; once set, nothing is taken
	// in. The counters below are Offer's alone, folded into the file at
	// each export.
	stopped        atomic.Bool
	received       atomic.Uint64
	rejected       atomic.Uint64
	dropped        atomic.Uint64
	lastRejectedAt atomic.Int64 // unix seconds; zero when never
	lastDroppedAt  atomic.Int64

	// intake is never closed, so a send can never panic; done tells the
	// worker to drain it and write once more, and exited says it has.
	intake   chan event
	done     chan struct{}
	exited   chan struct{}
	stopOnce sync.Once

	// mu guards data and what is derived from it. No I/O happens under it.
	mu      sync.Mutex
	data    client.APIMeter
	dirty   bool
	entries int       // usage entries across every bucket of every organization
	pruned  time.Time // the latest time the count was pruned against

	// The worker's own bookkeeping, touched by no other goroutine.
	lastSaved     time.Time
	lastSaveWarn  time.Time
	lastDropWarn  time.Time
	warnedDropped uint64
	beforeApply   func(*event) // a test hook, nil in production
}

// Open starts counting into the meter file at path, carrying on from the
// file when there is one. linked names the organizations the configuration
// links; now is the start time. It never fails: a file that cannot be read
// is replaced at the first save, and one warning says so.
func Open(path string, linked []string, now time.Time, log Logger) *Meter {
	m := newMeter(path, log)
	m.restore(linked, now)
	m.log.Log(context.Background(), "info", "quota-cache API meter counting Claude API-key traffic", map[string]any{"organizations": len(linked)})
	go m.work()
	return m
}

func newMeter(path string, log Logger) *Meter {
	return &Meter{path: path, log: log, intake: make(chan event, intakeSize), done: make(chan struct{}), exited: make(chan struct{})}
}

// restore loads the file and turns the stop it recorded, or the save it was
// cut off after, into a gap. The worker is not started.
func (m *Meter) restore(linked []string, now time.Time) {
	now = now.UTC().Truncate(time.Second)
	m.mu.Lock()
	defer m.mu.Unlock()
	loaded, err := client.LoadMeter(m.path)
	switch {
	case err == nil:
		m.data = *loaded
		gap := client.MeterGap{From: m.data.FlushedAt, To: now, Reason: client.MeterGapUncleanStop}
		if m.data.StoppedAt != nil {
			// Every stop is a gap, a clean shutdown included: CPA stops
			// plugins while it keeps serving, so traffic may have passed.
			gap = client.MeterGap{From: *m.data.StoppedAt, To: now, Reason: m.data.StopReason}
		}
		if gap.From.After(gap.To) {
			gap.From = gap.To
		}
		m.data.Gaps = append(m.data.Gaps, gap)
		m.data.StoppedAt, m.data.StopReason = nil, ""
		m.data.Restarts = addSaturating(m.data.Restarts, 1)
	case errors.Is(err, client.ErrMeterMissing):
		m.data = fresh(now)
	default:
		m.log.Log(context.Background(), "warn", "quota-cache could not read its API meter file and starts counting again", nil)
		m.data = fresh(now)
	}
	m.data.StartedAt = now
	m.received.Store(m.data.Received)
	m.rejected.Store(m.data.Rejected)
	m.dropped.Store(m.data.Dropped)
	m.lastRejectedAt.Store(unixOf(m.data.LastRejectedAt))
	m.lastDroppedAt.Store(unixOf(m.data.LastDroppedAt))
	m.entries = countEntries(m.data)
	m.pruneLocked(now)
	m.setLinkedLocked(linked, now)
	m.dirty = true
}

func fresh(now time.Time) client.APIMeter {
	return client.APIMeter{Schema: client.MeterSchema, Since: now, StartedAt: now,
		Gaps: []client.MeterGap{}, Organizations: map[string]client.MeterOrganization{}, Unlinked: []client.MeterUnlinked{}, Auths: map[string]client.MeterAuth{}}
}

// Offer takes one usage record as CPA sent it. It runs on CPA's single usage
// goroutine, so it decodes, filters and enqueues and nothing more: no lock,
// no I/O, and it never blocks. raw is not kept.
func (m *Meter) Offer(raw []byte, received time.Time) {
	if m.stopped.Load() {
		return
	}
	m.received.Add(1)
	received = received.UTC().Truncate(time.Second)
	e, keep, rejected := decode(raw, received)
	if rejected {
		m.rejected.Add(1)
		m.lastRejectedAt.Store(received.Unix())
		return
	}
	if !keep {
		return
	}
	select {
	case m.intake <- e:
	default:
		m.dropped.Add(1)
		m.lastDroppedAt.Store(received.Unix())
	}
}

// decode reads one record. rejected is set when it cannot be read or carries
// a negative token count; keep is set when it is a Claude API-key record,
// the only kind the meter counts, with e filled in. An OAuth record is
// dropped before anything but its provider and auth type is looked at.
func decode(raw []byte, received time.Time) (e event, keep, rejected bool) {
	var w usageWire
	if json.Unmarshal(raw, &w) != nil {
		return event{}, false, true
	}
	d := w.Detail
	if d.InputTokens < 0 || d.OutputTokens < 0 || d.CacheReadTokens < 0 || d.CacheCreationTokens < 0 {
		return event{}, false, true
	}
	if !strings.EqualFold(w.Provider, "claude") || !strings.EqualFold(w.AuthType, "apikey") {
		return event{}, false, false
	}
	e = event{at: completedAt(w.RequestedAt, w.Latency, received), failed: w.Failed,
		input: uint64(d.InputTokens), output: uint64(d.OutputTokens), cacheRead: uint64(d.CacheReadTokens), cacheWrite: uint64(d.CacheCreationTokens)}
	if index := strings.TrimSpace(w.AuthIndex); len(index) <= maxAuthIndex {
		e.authIndex = index
	}
	if values := w.ResponseHeaders.OrganizationID; len(values) > 0 {
		e.org, _ = client.NormalizeOrganizationID(values[0])
	}
	e.model = client.NormalizeMeterModel(w.Model, w.ResponseModel)
	e.prompt = client.MeterPromptClass(e.model, e.input+e.cacheRead+e.cacheWrite)
	if w.Failed {
		// Anthropic's documented message, at whatever status it arrives:
		// the 400 body, a 502 stream error event, or a client that echoes
		// Claude Code's "Credit balance too low".
		body := strings.ToLower(w.Failure.Body)
		e.refusal = strings.Contains(body, "credit balance") && strings.Contains(body, "too low")
	}
	// A Claude Code-based client: Claude Code itself or the Agent SDK.
	e.claudeCode = strings.HasPrefix(w.SessionID, "claude:")
	return e, true, false
}

// completedAt is when the attempt completed, which is when Anthropic bills
// it: the request time plus the latency when both are plausible, else the
// received time, and never after the received time.
func completedAt(requestedAt json.RawMessage, latency int64, received time.Time) time.Time {
	at := received
	var stamp string
	if json.Unmarshal(requestedAt, &stamp) == nil {
		if t, err := time.Parse(time.RFC3339Nano, stamp); err == nil && latency >= 0 && latency <= int64(24*time.Hour) &&
			!t.Before(received.Add(-24*time.Hour)) && !t.After(received.Add(5*time.Minute)) {
			at = t.Add(time.Duration(latency))
		}
	}
	if at.After(received) {
		at = received
	}
	return at.UTC().Truncate(time.Second)
}

// work is the worker: it applies events, saves on a timer, and on stop
// applies what is buffered and saves once more. A panic anywhere in it
// would abort the whole CPA process, so it recovers its own and stops the
// meter instead.
func (m *Meter) work() {
	defer close(m.exited)
	defer m.recoverWorker()
	tick := time.NewTicker(flushEvery)
	defer tick.Stop()
	for {
		select {
		case e := <-m.intake:
			m.apply(e)
		case now := <-tick.C:
			m.flushIfDue(now.UTC())
		case <-m.done:
			m.drain()
			m.save(time.Now().UTC())
			return
		}
	}
}

func (m *Meter) recoverWorker() {
	if recovered := recover(); recovered != nil {
		// The panic value could carry wire input; it is never echoed.
		m.fail(time.Now().UTC())
	}
}

// fail stops the meter after an internal error, writes the stop, and says so
// once. Offer returns at once from then on; the next configure replaces the
// meter by reopening its file, which records the gap.
func (m *Meter) fail(now time.Time) {
	now = now.UTC().Truncate(time.Second)
	m.stopped.Store(true)
	m.mu.Lock()
	m.data.StopReason, m.data.StoppedAt, m.dirty = client.MeterStopFailed, &now, true
	m.mu.Unlock()
	func() {
		defer func() { _ = recover() }()
		_ = m.flush(now)
	}()
	m.log.Log(context.Background(), "error", "quota-cache API meter stopped after an internal error; Claude API credit estimates are incomplete until Quota Cache reloads", nil)
}

// drain applies every event still buffered, without blocking.
func (m *Meter) drain() {
	for {
		select {
		case e := <-m.intake:
			m.apply(e)
		default:
			return
		}
	}
}

// Stop ends counting with the given reason, which the file records so the
// next start can turn the stop into a gap, and waits for the worker to
// apply what is buffered and save. It may be called more than once.
func (m *Meter) Stop(reason string, now time.Time) {
	m.stopOnce.Do(func() {
		now = now.UTC().Truncate(time.Second)
		m.stopped.Store(true)
		m.mu.Lock()
		// A meter that stopped itself keeps saying so.
		if m.data.StopReason != client.MeterStopFailed {
			m.data.StopReason, m.data.StoppedAt = reason, &now
		}
		m.dirty = true
		m.mu.Unlock()
		close(m.done)
	})
	<-m.exited
}

// Failed reports whether the meter stopped itself after an internal error.
func (m *Meter) Failed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data.StopReason == client.MeterStopFailed
}

// SetLinked tells the meter which organizations the configuration links: the
// distinct valid organization ids of the first MaxAPICreditItems items. A
// listed organization not yet known starts counting now; one no longer
// listed becomes dormant, keeping its buckets and still bucketed, so that
// fixing a typo or moving an item loses nothing; a dormant one listed again
// is linked with its history whole.
func (m *Meter) SetLinked(orgIDs []string, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setLinkedLocked(orgIDs, now.UTC().Truncate(time.Second))
}

func (m *Meter) setLinkedLocked(orgIDs []string, now time.Time) {
	listed := map[string]bool{}
	for _, id := range orgIDs {
		listed[id] = true
		o, known := m.data.Organizations[id]
		switch {
		case !known:
			m.data.Organizations[id] = client.MeterOrganization{Since: now, Days: []client.MeterBucket{}, Hours: []client.MeterBucket{}}
			m.evictDormantLocked()
		case o.UnlinkedAt != nil:
			o.UnlinkedAt = nil
			m.data.Organizations[id] = o
		default:
			continue
		}
		m.dirty = true
	}
	for id, o := range m.data.Organizations {
		if !listed[id] && o.UnlinkedAt == nil {
			at := now
			o.UnlinkedAt = &at
			m.data.Organizations[id] = o
			m.dirty = true
		}
	}
	if kept := slices.DeleteFunc(m.data.Unlinked, func(u client.MeterUnlinked) bool { return listed[u.OrganizationID] }); len(kept) != len(m.data.Unlinked) {
		m.data.Unlinked, m.dirty = kept, true
	}
}

// evictDormantLocked keeps Organizations within MaxMeterOrganizations by
// deleting the dormant organization that has been dormant longest.
func (m *Meter) evictDormantLocked() {
	for len(m.data.Organizations) > client.MaxMeterOrganizations {
		oldest := ""
		for id, o := range m.data.Organizations {
			if o.UnlinkedAt != nil && (oldest == "" || o.UnlinkedAt.Before(*m.data.Organizations[oldest].UnlinkedAt)) {
				oldest = id
			}
		}
		if oldest == "" {
			return
		}
		m.entries -= countOrganization(m.data.Organizations[oldest])
		delete(m.data.Organizations, oldest)
	}
}

// apply counts one event. It holds mu for the one event and does no I/O.
func (m *Meter) apply(e event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.beforeApply != nil {
		m.beforeApply(&e)
	}
	m.pruneLocked(e.at)
	org := m.attributeLocked(e)
	if org == "" {
		m.dirty = true
		return
	}
	o, known := m.data.Organizations[org]
	if linked := known && o.UnlinkedAt == nil; !linked {
		m.noteUnlinkedLocked(org, e.at)
		if !known {
			m.dirty = true
			return
		}
	} else {
		m.data.Counted = addSaturating(m.data.Counted, 1)
	}
	// A dormant organization's history is kept whole, so its last-seen
	// times, refusals and buckets move as a linked one's do.
	o.LastSeenAt = latest(o.LastSeenAt, e.at)
	if !e.failed {
		o.LastSuccessAt = latest(o.LastSuccessAt, e.at)
	}
	if e.refusal && e.claudeCode {
		o.ClaudeCodeRefusals = addSaturating(o.ClaudeCodeRefusals, 1)
		o.LastClaudeCodeRefusalAt = latest(o.LastClaudeCodeRefusalAt, e.at)
	} else if e.refusal {
		o.Refusals = addSaturating(o.Refusals, 1)
		o.LastRefusalAt = latest(o.LastRefusalAt, e.at)
	}
	m.bucketLocked(&o, e)
	m.data.Organizations[org] = o
	m.dirty = true
}

// attributeLocked names the event's organization, teaching the auth map as
// it goes, or "" when the event is not an organization's: a record from a
// service that is not Anthropic (foreign), or a failure that nothing can
// attribute (unattributed).
func (m *Meter) attributeLocked(e event) string {
	if e.org != "" {
		if e.authIndex != "" {
			m.learnAuthLocked(e.authIndex, e.org, e.at)
		}
		return e.org
	}
	if a, known := m.data.Auths[e.authIndex]; known && e.authIndex != "" {
		if a.OrganizationID == "" {
			m.data.Foreign = addSaturating(m.data.Foreign, 1)
			return ""
		}
		if e.at.After(a.SeenAt) {
			a.SeenAt = e.at
			m.data.Auths[e.authIndex] = a
		}
		return a.OrganizationID
	}
	if !e.failed {
		// A success with no organization header is not Anthropic's: a
		// claude-api-key entry pointed at another service. Its auth is
		// remembered as foreign so its failures are not unattributed.
		if e.authIndex != "" {
			m.learnAuthLocked(e.authIndex, "", e.at)
		}
		m.data.Foreign = addSaturating(m.data.Foreign, 1)
		return ""
	}
	m.data.Unattributed = addSaturating(m.data.Unattributed, 1)
	if e.input+e.output+e.cacheRead+e.cacheWrite > 0 {
		m.data.LastUnattributedAt = latest(m.data.LastUnattributedAt, e.at)
	}
	return ""
}

func (m *Meter) learnAuthLocked(index, org string, at time.Time) {
	a := m.data.Auths[index]
	a.OrganizationID = org
	if at.After(a.SeenAt) {
		a.SeenAt = at
	}
	m.data.Auths[index] = a
	m.capAuthsLocked()
}

// capAuthsLocked keeps Auths within MaxMeterAuths by forgetting the auth
// seen longest ago.
func (m *Meter) capAuthsLocked() {
	for len(m.data.Auths) > client.MaxMeterAuths {
		oldest := ""
		for index, a := range m.data.Auths {
			if oldest == "" || a.SeenAt.Before(m.data.Auths[oldest].SeenAt) {
				oldest = index
			}
		}
		delete(m.data.Auths, oldest)
	}
}

// noteUnlinkedLocked records traffic from an organization no item links,
// never linked or dormant, in the list a consumer shows the operator.
func (m *Meter) noteUnlinkedLocked(org string, at time.Time) {
	i := slices.IndexFunc(m.data.Unlinked, func(u client.MeterUnlinked) bool { return u.OrganizationID == org })
	if i < 0 {
		m.data.Unlinked = append(m.data.Unlinked, client.MeterUnlinked{OrganizationID: org, FirstSeenAt: at, LastSeenAt: at})
		i = len(m.data.Unlinked) - 1
	}
	u := &m.data.Unlinked[i]
	u.Requests = addSaturating(u.Requests, 1)
	if at.After(u.LastSeenAt) {
		u.LastSeenAt = at
	}
	for len(m.data.Unlinked) > client.MaxMeterUnlinked {
		oldest := 0
		for j, u := range m.data.Unlinked {
			if u.LastSeenAt.Before(m.data.Unlinked[oldest].LastSeenAt) {
				oldest = j
			}
		}
		m.data.Unlinked = slices.Delete(m.data.Unlinked, oldest, oldest+1)
	}
}

// bucketLocked adds the event to its organization's day and hour buckets,
// both or neither, under one key, so the hour buckets a reading's baseline
// is taken from are always a subset of the day buckets it is set against.
func (m *Meter) bucketLocked(o *client.MeterOrganization, e event) {
	dayStart, hourStart := client.MeterDayStart(e.at), client.MeterHourStart(e.at)
	if dayStart.Before(m.dayCutoff()) || hourStart.Before(m.hourCutoff()) {
		// Older than what the meter keeps: counted, but not bucketed.
		return
	}
	day, dayNew := bucketAt(&o.Days, dayStart)
	hour, hourNew := bucketAt(&o.Hours, hourStart)
	key := client.MeterUsage{Model: e.model, Prompt: e.prompt}
	if (!hasUsage(o.Days[day], key) && len(o.Days[day].Usage) >= client.MaxMeterUsagePerBucket) ||
		(!hasUsage(o.Hours[hour], key) && len(o.Hours[hour].Usage) >= client.MaxMeterUsagePerBucket) {
		key = client.MeterUsage{Model: client.MeterOtherModel}
		o.LastOverflowAt = latest(o.LastOverflowAt, e.at)
	}
	need := 0
	if !hasUsage(o.Days[day], key) {
		need++
	}
	if !hasUsage(o.Hours[hour], key) {
		need++
	}
	if m.entries+need > client.MaxMeterUsageEntries {
		o.LastOverflowAt = latest(o.LastOverflowAt, e.at)
		if hourNew {
			o.Hours = slices.Delete(o.Hours, hour, hour+1)
		}
		if dayNew {
			o.Days = slices.Delete(o.Days, day, day+1)
		}
		return
	}
	// Tokens of a failed attempt are partial usage Anthropic bills.
	key.Input, key.Output, key.CacheRead, key.CacheWrite = e.input, e.output, e.cacheRead, e.cacheWrite
	if e.failed {
		key.Failed = 1
	} else {
		key.Requests = 1
	}
	o.Days[day].Usage = client.MergeMeterUsage(o.Days[day].Usage, key)
	o.Hours[hour].Usage = client.MergeMeterUsage(o.Hours[hour].Usage, key)
	m.entries += need
}

// bucketAt is the index of the bucket starting at start, inserted in start
// order when absent.
func bucketAt(buckets *[]client.MeterBucket, start time.Time) (index int, created bool) {
	i, found := slices.BinarySearchFunc(*buckets, start, func(b client.MeterBucket, t time.Time) int { return b.Start.Compare(t) })
	if !found {
		*buckets = slices.Insert(*buckets, i, client.MeterBucket{Start: start, Usage: []client.MeterUsage{}})
	}
	return i, !found
}

func hasUsage(b client.MeterBucket, key client.MeterUsage) bool {
	return slices.ContainsFunc(b.Usage, func(u client.MeterUsage) bool { return u.Model == key.Model && u.Prompt == key.Prompt })
}

func (m *Meter) dayCutoff() time.Time {
	return client.MeterDayStart(m.pruned).Add(-client.MeterDays * 24 * time.Hour)
}

func (m *Meter) hourCutoff() time.Time {
	return client.MeterHourStart(m.pruned).Add(-client.MeterHours * time.Hour)
}

// pruneLocked drops what the meter no longer keeps at t: day buckets past
// MeterDays, hour buckets past MeterHours, and dormant organizations,
// unlinked organizations, auths and gaps older than the retention. It also
// holds Auths and Gaps to their caps.
func (m *Meter) pruneLocked(t time.Time) {
	if t.Before(m.pruned) {
		return
	}
	m.pruned = t
	dayCut, hourCut, ageCut := m.dayCutoff(), m.hourCutoff(), t.Add(-retention)
	for id, o := range m.data.Organizations {
		if o.UnlinkedAt != nil && o.UnlinkedAt.Before(ageCut) {
			m.entries -= countOrganization(o)
			delete(m.data.Organizations, id)
			m.dirty = true
			continue
		}
		days, hours := dropBefore(o.Days, dayCut), dropBefore(o.Hours, hourCut)
		if days < 0 && hours < 0 {
			continue
		}
		if days >= 0 {
			m.entries -= countBuckets(o.Days[:days])
			o.Days = slices.Delete(o.Days, 0, days)
		}
		if hours >= 0 {
			m.entries -= countBuckets(o.Hours[:hours])
			o.Hours = slices.Delete(o.Hours, 0, hours)
		}
		m.data.Organizations[id] = o
		m.dirty = true
	}
	if kept := slices.DeleteFunc(m.data.Unlinked, func(u client.MeterUnlinked) bool { return u.LastSeenAt.Before(ageCut) }); len(kept) != len(m.data.Unlinked) {
		m.data.Unlinked, m.dirty = kept, true
	}
	for index, a := range m.data.Auths {
		if a.SeenAt.Before(ageCut) {
			delete(m.data.Auths, index)
			m.dirty = true
		}
	}
	if before := len(m.data.Auths); before > client.MaxMeterAuths {
		m.capAuthsLocked()
		m.dirty = true
	}
	if kept := capGaps(slices.DeleteFunc(m.data.Gaps, func(g client.MeterGap) bool { return g.To.Before(ageCut) })); len(kept) != len(m.data.Gaps) {
		m.data.Gaps, m.dirty = kept, true
	}
}

// dropBefore is how many leading buckets, oldest first, start before cutoff,
// or -1 when none does.
func dropBefore(buckets []client.MeterBucket, cutoff time.Time) int {
	n := 0
	for n < len(buckets) && buckets[n].Start.Before(cutoff) {
		n++
	}
	if n == 0 {
		return -1
	}
	return n
}

// capGaps holds gaps, oldest first, to MaxMeterGaps. A brief gap, a restart
// or a reload that a consumer ignores, is dropped first, oldest first. Only
// when every gap is long are the two oldest merged into one, which keeps a
// gap from ever being lost, only coarsened. Two brief gaps are never merged
// into one long span, since that span would cover uptime.
func capGaps(gaps []client.MeterGap) []client.MeterGap {
	for len(gaps) > client.MaxMeterGaps {
		if i := slices.IndexFunc(gaps, client.MeterGap.Brief); i >= 0 {
			gaps = slices.Delete(gaps, i, i+1)
			continue
		}
		gaps[1] = client.MeterGap{From: gaps[0].From, To: gaps[1].To, Reason: gaps[1].Reason}
		gaps = slices.Delete(gaps, 0, 1)
	}
	return gaps
}

// flushIfDue saves when the count changed since the last save, or when the
// last save is a heartbeat old. The drop warning is given here, never from
// Offer, and at most once per warnEvery.
func (m *Meter) flushIfDue(now time.Time) {
	if dropped := m.dropped.Load(); dropped != m.warnedDropped && now.Sub(m.lastDropWarn) >= warnEvery {
		m.warnedDropped, m.lastDropWarn = dropped, now
		m.log.Log(context.Background(), "warn", "quota-cache API meter is dropping usage records; Claude API credit estimates will be low", map[string]any{"dropped": dropped})
	}
	m.mu.Lock()
	dirty := m.dirty
	m.mu.Unlock()
	if !dirty && now.Sub(m.lastSaved) < heartbeat {
		return
	}
	m.save(now)
}

// save writes the file, warning at most once per warnEvery when it cannot.
func (m *Meter) save(now time.Time) {
	if err := m.flush(now); err != nil && now.Sub(m.lastSaveWarn) >= warnEvery {
		m.lastSaveWarn = now
		m.log.Log(context.Background(), "warn", "quota-cache cannot save its API meter; Claude API credit estimates may lose recent spend", nil)
	}
}

// flush encodes the count under mu and writes it with mu released: a temp
// file in the directory, write, fsync, rename, fsync the directory, as the
// snapshot is written. A failed write leaves the count dirty for the next
// try.
func (m *Meter) flush(now time.Time) error {
	raw := m.exportLimited(now, client.MaxMeterBytes)
	if err := writeFile(m.path, raw); err != nil {
		m.mu.Lock()
		m.dirty = true
		m.mu.Unlock()
		return err
	}
	m.lastSaved = now
	return nil
}

// exportLimited is the file's bytes at now, held within limit by the
// oversize rule: the caps make a meter over MaxMeterBytes unreachable, but
// one is never refused for size. The oldest hour buckets go first, across
// every organization one hour at a time, then day buckets older than a
// cycle, one day at a time, and each organization that lost a bucket is
// marked overflowed.
func (m *Meter) exportLimited(now time.Time, limit int) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	now = now.UTC().Truncate(time.Second)
	m.pruneLocked(now)
	m.data.Received, m.data.Rejected, m.data.Dropped = m.received.Load(), m.rejected.Load(), m.dropped.Load()
	m.data.LastRejectedAt, m.data.LastDroppedAt = timeOf(m.lastRejectedAt.Load()), timeOf(m.lastDroppedAt.Load())
	m.data.FlushedAt = now
	m.dirty = false
	sort.SliceStable(m.data.Unlinked, func(i, j int) bool { return m.data.Unlinked[i].LastSeenAt.After(m.data.Unlinked[j].LastSeenAt) })
	raw, err := json.Marshal(m.data)
	for err == nil && len(raw) > limit {
		if !m.dropOldestHourLocked(now) && !m.dropOldestDayLocked(now) {
			break
		}
		raw, err = json.Marshal(m.data)
	}
	if err != nil {
		return nil
	}
	return raw
}

// dropOldestHourLocked removes the earliest hour bucket of every
// organization that holds it, and reports whether there was one.
func (m *Meter) dropOldestHourLocked(now time.Time) bool {
	return m.dropOldestLocked(now, time.Time{}, func(o *client.MeterOrganization) *[]client.MeterBucket { return &o.Hours })
}

// dropOldestDayLocked does the same for the earliest day bucket older than a
// cycle, so the current cycle's sums are the last to go.
func (m *Meter) dropOldestDayLocked(now time.Time) bool {
	return m.dropOldestLocked(now, client.MeterDayStart(now).Add(-oversizeDays), func(o *client.MeterOrganization) *[]client.MeterBucket { return &o.Days })
}

func (m *Meter) dropOldestLocked(now, before time.Time, of func(*client.MeterOrganization) *[]client.MeterBucket) bool {
	var oldest time.Time
	for _, o := range m.data.Organizations {
		if buckets := *of(&o); len(buckets) > 0 && (oldest.IsZero() || buckets[0].Start.Before(oldest)) {
			oldest = buckets[0].Start
		}
	}
	if oldest.IsZero() || (!before.IsZero() && !oldest.Before(before)) {
		return false
	}
	for id, o := range m.data.Organizations {
		buckets := of(&o)
		if len(*buckets) == 0 || !(*buckets)[0].Start.Equal(oldest) {
			continue
		}
		m.entries -= len((*buckets)[0].Usage)
		*buckets = slices.Delete(*buckets, 0, 1)
		o.LastOverflowAt = latest(o.LastOverflowAt, now)
		m.data.Organizations[id] = o
	}
	return true
}

func writeFile(path string, raw []byte) error {
	if raw == nil {
		return errors.New("api meter cannot be encoded")
	}
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, ".meter-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(raw); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err == nil {
		err = d.Sync()
		d.Close()
	}
	return err
}

func countEntries(data client.APIMeter) int {
	n := 0
	for _, o := range data.Organizations {
		n += countOrganization(o)
	}
	return n
}

func countOrganization(o client.MeterOrganization) int {
	return countBuckets(o.Days) + countBuckets(o.Hours)
}

func countBuckets(buckets []client.MeterBucket) int {
	n := 0
	for _, b := range buckets {
		n += len(b.Usage)
	}
	return n
}

// latest is the later of a stored time and t, as a pointer for the file.
func latest(stored *time.Time, t time.Time) *time.Time {
	if stored != nil && !t.After(*stored) {
		return stored
	}
	return &t
}

func unixOf(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.Unix()
}

func timeOf(unix int64) *time.Time {
	if unix == 0 {
		return nil
	}
	t := time.Unix(unix, 0).UTC()
	return &t
}

func addSaturating(a, b uint64) uint64 {
	if sum := a + b; sum >= a {
		return sum
	}
	return math.MaxUint64
}
