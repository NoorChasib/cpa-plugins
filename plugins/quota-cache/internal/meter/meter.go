// Package meter counts API-key traffic per organization from the usage
// records CPA hands its usage plugins, and keeps the count in a file beside
// the snapshot. A Source says what one meter counts: Claude, Claude API-key
// traffic per Console organization in the file client.MeterPath names, or
// OpenAI, openai-compatibility traffic per OpenAI API organization in the
// file client.OpenAIMeterPath names. It prices nothing and sends nothing: a
// consumer prices the tokens with its own table.
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
	// retention ages dormant organizations, unlinked organizations, auths,
	// auth IDs and gaps: as long as the day buckets reach back.
	retention = client.MeterDays * 24 * time.Hour
	// oversizeDays is how far back the oversize rule keeps day buckets: a
	// credit cycle, so the current cycle's sums survive.
	oversizeDays = 31 * 24 * time.Hour
	// maxAuthIndex is the longest auth index kept; CPA's are 16 hex digits.
	maxAuthIndex = 64
)

// usageWire is the part of CPA's pluginapi.UsageRecord the meter reads. It
// has no Source, APIKey, BaseURL, Alias, TraceID, RequestID or
// ParentSessionID field, and reads AuthID only through configAuthID, which
// keeps nothing but CPA's config API-key shape, so the raw upstream key, the
// client key and the credential's name are never materialized. CPA marshals
// the record with no JSON tags, so every key is the Go field name;
// encoding/json matches keys case-insensitively, which also covers the
// organization header.
type usageWire struct {
	Provider, AuthIndex, AuthType, Model, ResponseModel, SessionID string
	AuthID                                                         configAuthID
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

// configAuthIDPrefix is how a JSON string holding a config API-key auth ID
// begins: the opening quote, then client.ConfigAPIKeyAuthID's prefix.
const configAuthIDPrefix = `"claude:apikey:`

// configAuthID keeps an AuthID only when it is CPA's config API-key shape.
// The raw JSON is tested before any string is made, so a file name or an
// e-mail address an OAuth record carries is never materialized. It never
// fails, so an odd AuthID never rejects a record.
type configAuthID string

func (c *configAuthID) UnmarshalJSON(b []byte) error {
	*c = ""
	// A plain JSON string with no escape, 26 to 31 bytes between its quotes,
	// starting with the prefix. Anything else, null, a number, an object or
	// an escaped string included, is left unread.
	if n := len(b) - 2; n < 26 || n > 31 || string(b[:len(configAuthIDPrefix)]) != configAuthIDPrefix || b[len(b)-1] != '"' || slices.Contains(b, '\\') {
		return nil
	}
	if id := string(b[1 : len(b)-1]); client.ConfigAPIKeyAuthID(id) {
		*c = configAuthID(id)
	}
	return nil
}

// event is what the worker counts: one API-key attempt of the meter's
// source, reduced to the facts the meter keeps. The failure body, the session
// id and the headers are gone by the time one exists.
type event struct {
	at                                   time.Time // completion time, clamped to the received time, truncated to the second
	authIndex                            string    // CPA's auth index, "" when absent or over maxAuthIndex bytes
	authID                               string    // CPA's auth ID when it has the config API-key shape, else ""; always "" for OpenAI
	org                                  string    // the organization header, normalized; "" when absent (or, for Claude, invalid)
	model                                string    // client.NormalizeMeterModel(Model, ResponseModel), or openAIMeterModel's for OpenAI
	prompt                               string    // client.MeterPromptClass(model, input+cacheRead+cacheWrite), or OpenAIMeterPromptClass of the whole input
	tier                                 string    // OpenAI's service tier, client.NormalizeOpenAIServiceTier; always "" for Claude
	failed, refusal, claudeCode          bool      // claudeCode is always false for OpenAI
	cancelled                            bool      // a failure with status 499: the client went away
	input, output, cacheRead, cacheWrite uint64
}

// Source is what one meter counts and how it names itself in CPA's log. The
// count, its file and the machinery that keeps them are the same for every
// source; the decode, the attribution and the auth-ID map are what differ.
type Source struct {
	// decode reads one record: e, keep and rejected as decode returns them.
	decode func(raw []byte, received time.Time) (e event, keep, rejected bool)
	// headerOnly attributes an event by its organization header, else by the
	// auth map for a failure only, else not at all (attributeOpenAILocked),
	// and never counts one as foreign or unattributed.
	headerOnly bool
	// authIDs records config API-key auth IDs, and the file's
	// auth_ids_since.
	authIDs bool
	// lazyFile writes no file until the meter has an organization to keep,
	// linked, dormant or unlinked, so a meter that runs with none listed and
	// has seen none leaves nothing beside the snapshot.
	lazyFile bool
	logs     sourceLogs
}

// sourceLogs are a source's lines in CPA's log: when it starts, when its file
// cannot be read, when it stops itself, when it drops records and when it
// cannot save.
type sourceLogs struct{ started, unreadable, failed, dropping, unsaved string }

// Claude counts Claude API-key traffic per Console organization. Open starts
// a meter of it.
var Claude = Source{decode: decode, authIDs: true, logs: sourceLogs{
	started:    "quota-cache API meter counting Claude API-key traffic",
	unreadable: "quota-cache could not read its API meter file and starts counting again",
	failed:     "quota-cache API meter stopped after an internal error; Claude API credit estimates are incomplete until Quota Cache reloads",
	dropping:   "quota-cache API meter is dropping usage records; Claude API credit estimates will be low",
	unsaved:    "quota-cache cannot save its API meter; Claude API credit estimates may lose recent spend",
}}

// OpenAI counts openai-compatibility API-key traffic per OpenAI API
// organization, as the openai-organization response header names it. A meter
// of it may run with no organization listed: it then counts nothing and notes
// each organization it sees as unlinked, so the page can offer it, and it
// writes its file only once it has noted one.
var OpenAI = Source{decode: decodeOpenAI, headerOnly: true, lazyFile: true, logs: sourceLogs{
	started:    "quota-cache OpenAI API meter counting openai-compatibility traffic",
	unreadable: "quota-cache could not read its OpenAI API meter file and starts counting again",
	failed:     "quota-cache OpenAI API meter stopped after an internal error; OpenAI API credit estimates are incomplete until Quota Cache reloads",
	dropping:   "quota-cache OpenAI API meter is dropping usage records; OpenAI API credit estimates will be low",
	unsaved:    "quota-cache cannot save its OpenAI API meter; OpenAI API credit estimates may lose recent spend",
}}

// Meter is one running count. Offer runs on CPA's usage goroutine and only
// decodes and enqueues; the worker owns the count, the file and the ticker.
type Meter struct {
	path   string
	log    Logger
	source Source

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
	// stopAt is the instant Stop was given. It is written before done is
	// closed and read by the worker only after, so the close orders it.
	stopAt time.Time

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
	// noFile is set while a lazy source's file did not exist at restore and
	// has not been written since.
	noFile      bool
	beforeApply func(*event) // a test hook, nil in production
}

// Open starts counting Claude API-key traffic into the meter file at path,
// carrying on from the file when there is one. linked names the organizations
// listed on quota-cache's page, from its organizations file; now is the start
// time. It never fails: a file that cannot be read is replaced at the first
// save, and one warning says so.
func Open(path string, linked []string, now time.Time, log Logger) *Meter {
	return OpenFor(Claude, path, linked, now, log)
}

// OpenFor is Open for the traffic source counts, with linked naming that
// source's organizations. A lazy source with no file yet creates none until
// it has an organization to keep.
func OpenFor(source Source, path string, linked []string, now time.Time, log Logger) *Meter {
	m := newMeterFor(source, path, log)
	m.restore(linked, now)
	m.log.Log(context.Background(), "info", m.source.logs.started, map[string]any{"organizations": len(linked)})
	go m.work()
	return m
}

func newMeter(path string, log Logger) *Meter {
	return newMeterFor(Claude, path, log)
}

func newMeterFor(source Source, path string, log Logger) *Meter {
	return &Meter{path: path, log: log, source: source, intake: make(chan event, intakeSize), done: make(chan struct{}), exited: make(chan struct{})}
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
		// A file an older quota-cache wrote begins recording auth IDs now;
		// one that already does keeps its first start. A source that records
		// none leaves it unset.
		if m.source.authIDs && m.data.AuthIDsSince == nil {
			since := now
			m.data.AuthIDsSince = &since
		}
	case errors.Is(err, client.ErrMeterMissing):
		m.data = fresh(now, m.source.authIDs)
		m.noFile = m.source.lazyFile
	default:
		m.log.Log(context.Background(), "warn", m.source.logs.unreadable, nil)
		m.data = fresh(now, m.source.authIDs)
	}
	m.data.StartedAt = now
	m.received.Store(m.data.Received)
	m.rejected.Store(m.data.Rejected)
	m.dropped.Store(m.data.Dropped)
	// The file's drops were warned about in the run that made them; the
	// warning is for drops since the last one, so this run starts even.
	m.warnedDropped = m.data.Dropped
	m.lastRejectedAt.Store(unixOf(m.data.LastRejectedAt))
	m.lastDroppedAt.Store(unixOf(m.data.LastDroppedAt))
	m.entries = countEntries(m.data)
	m.pruneLocked(now)
	m.setLinkedLocked(linked, now)
	m.dirty = true
}

// fresh is an empty count starting at now. authIDs sets AuthIDsSince; the
// AuthIDs map is never null either way.
func fresh(now time.Time, authIDs bool) client.APIMeter {
	data := client.APIMeter{Schema: client.MeterSchema, Since: now, StartedAt: now,
		Gaps: []client.MeterGap{}, Organizations: map[string]client.MeterOrganization{}, Unlinked: []client.MeterUnlinked{}, Auths: map[string]client.MeterAuth{},
		AuthIDs: map[string]client.MeterAuthID{}}
	if authIDs {
		data.AuthIDsSince = &now
	}
	return data
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
	e, keep, rejected := m.source.decode(raw, received)
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
	e = event{at: completedAt(w.RequestedAt, w.Latency, received), failed: w.Failed, authID: string(w.AuthID),
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
		// CPA's status for a client that went away before the attempt
		// finished, which says nothing about the credential.
		e.cancelled = w.Failure.StatusCode == 499
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
			// The last save is dated at the stop it records, as every other
			// time in the meter is the caller's, not the wall clock's.
			m.save(m.stopAt)
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
	m.log.Log(context.Background(), "error", m.source.logs.failed, nil)
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
		m.stopAt = now
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

// SetLinked tells the meter which organizations are listed on quota-cache's
// page: the Organization IDs of its organizations file, each distinct and at
// most MaxAPICreditItems. A listed organization not yet known starts counting
// now; one no longer listed becomes dormant, keeping its buckets and still
// bucketed, so that removing an organization and adding it again loses
// nothing; a dormant one listed again is linked with its history whole.
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
	if e.authID != "" && e.authIndex != "" {
		m.learnAuthIDLocked(e)
	}
	var org string
	if m.source.headerOnly {
		org = m.attributeOpenAILocked(e)
	} else {
		org = m.attributeLocked(e)
	}
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

// attributeOpenAILocked names an OpenAI event's organization, teaching the
// auth map from a headed record, or "" when the event is no organization's.
// A failure without the header goes to the organization a headed record last
// showed on its auth, so a refusal that lacks the header still lands. Any
// other headerless record is ignored, a success on a known auth included, and
// is counted as neither foreign nor unattributed: openai-compatibility
// entries also serve upstreams that never send the header, CPA records
// OpenAI's headers on every success, and CPA keys an auth by base URL and key
// alone, so behind a gateway that mixes upstreams a headerless success is
// another upstream's and would be priced at OpenAI's rates.
func (m *Meter) attributeOpenAILocked(e event) string {
	if e.org != "" {
		if e.authIndex != "" {
			m.learnAuthLocked(e.authIndex, e.org, e.at)
		}
		return e.org
	}
	if !e.failed || e.authIndex == "" {
		return ""
	}
	a, known := m.data.Auths[e.authIndex]
	if !known || a.OrganizationID == "" {
		return ""
	}
	if e.at.After(a.SeenAt) {
		a.SeenAt = e.at
		m.data.Auths[e.authIndex] = a
	}
	return a.OrganizationID
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

// learnAuthIDLocked records the auth index CPA reported with a config API
// key's auth ID, and the attempt's outcome. Both are CPA's facts about its
// own credential, independent of Anthropic, so it runs for every event that
// carries both, whatever the event's attribution. A low-credit refusal is
// the organization's, already counted there, and a cancellation is the
// client's, so neither is the key's success or failure.
func (m *Meter) learnAuthIDLocked(e event) {
	a := m.data.AuthIDs[e.authID]
	a.AuthIndex = e.authIndex
	if e.at.After(a.SeenAt) {
		a.SeenAt = e.at
	}
	switch {
	case !e.failed:
		a.LastSuccessAt = latest(a.LastSuccessAt, e.at)
		a.Failures = 0
	case !e.refusal && !e.cancelled:
		a.LastFailureAt = latest(a.LastFailureAt, e.at)
		if a.Failures < math.MaxUint16 {
			a.Failures++
		}
	}
	m.data.AuthIDs[e.authID] = a
	m.capAuthIDsLocked()
}

// capAuthIDsLocked keeps AuthIDs within MaxMeterAuthIDs by forgetting the ID
// seen longest ago, the smaller ID first when two were seen at once.
func (m *Meter) capAuthIDsLocked() {
	for len(m.data.AuthIDs) > client.MaxMeterAuthIDs {
		oldest := ""
		for id, a := range m.data.AuthIDs {
			if oldest == "" {
				oldest = id
				continue
			}
			if seen := m.data.AuthIDs[oldest].SeenAt; a.SeenAt.Before(seen) || (a.SeenAt.Equal(seen) && id < oldest) {
				oldest = id
			}
		}
		delete(m.data.AuthIDs, oldest)
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
	key := client.MeterUsage{Model: e.model, Prompt: e.prompt, Tier: e.tier}
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
	return slices.ContainsFunc(b.Usage, func(u client.MeterUsage) bool {
		return u.Model == key.Model && u.Prompt == key.Prompt && u.Tier == key.Tier
	})
}

func (m *Meter) dayCutoff() time.Time {
	return client.MeterDayStart(m.pruned).Add(-client.MeterDays * 24 * time.Hour)
}

func (m *Meter) hourCutoff() time.Time {
	return client.MeterHourStart(m.pruned).Add(-client.MeterHours * time.Hour)
}

// pruneLocked drops what the meter no longer keeps at t: day buckets past
// MeterDays, hour buckets past MeterHours, and dormant organizations,
// unlinked organizations, auths, auth IDs and gaps older than the retention.
// It also holds Auths, AuthIDs and Gaps to their caps.
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
	for id, a := range m.data.AuthIDs {
		if a.SeenAt.Before(ageCut) {
			delete(m.data.AuthIDs, id)
			m.dirty = true
		}
	}
	if len(m.data.AuthIDs) > client.MaxMeterAuthIDs {
		m.capAuthIDsLocked()
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
		m.log.Log(context.Background(), "warn", m.source.logs.dropping, map[string]any{"dropped": dropped})
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
		m.log.Log(context.Background(), "warn", m.source.logs.unsaved, nil)
	}
}

// flush encodes the count under mu and writes it with mu released: a temp
// file in the directory, write, fsync, rename, fsync the directory, as the
// snapshot is written. A failed write leaves the count dirty for the next
// try. A lazy source with no file yet writes nothing while it has no
// organization to keep, so a stop then leaves no file and no gap: there is
// nothing to carry on from.
func (m *Meter) flush(now time.Time) error {
	if m.awaitingFile() {
		return nil
	}
	raw := m.exportLimited(now, client.MaxMeterBytes)
	if err := writeFile(m.path, raw); err != nil {
		m.mu.Lock()
		m.dirty = true
		m.mu.Unlock()
		return err
	}
	m.lastSaved, m.noFile = now, false
	return nil
}

// awaitingFile is whether a lazy source's meter still has no file and nothing
// to put in one: no organization, linked or dormant, and no unlinked one.
func (m *Meter) awaitingFile() bool {
	if !m.noFile {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.data.Organizations) == 0 && len(m.data.Unlinked) == 0
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
