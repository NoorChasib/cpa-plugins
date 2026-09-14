package aggregate

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// Level thresholds. The design turns a bar red below 20% remaining; the rule
// lives here so every client agrees and none of them recomputes it in CSS.
const (
	criticalBelow = 0.20
	lowBelow      = 0.40
)

// Data issues attached to an entry the client can surface without guessing.
const (
	issuePercentOutOfRange = "percentOutOfRange"
	issuePercentInvalid    = "percentInvalid"
	issueResetInPast       = "resetInPast"
	issueObserveError      = "observeError"
	issueRefreshPending    = "refreshPending"
	issueStale             = "stale"
)

// Input is everything Build needs. It holds no clock and no file handle: the
// caller reads the snapshot, asks the host for the roster, and passes both in.
type Input struct {
	Snapshot qc.Snapshot
	// SourceReason is non-empty when the snapshot could not be read at all,
	// carrying ReasonCacheMissing or ReasonSchemaUnsupported.
	SourceReason string
	Identities   []Identity
	// Samples is the trend history. WindowKey holds the row id, so a per-model
	// window trends independently of the shared row it derives from.
	Samples    []Sample
	StaleAfter time.Duration
	// PlanLabels overrides the built-in plan display names, keyed by the
	// provider-reported value in lower case.
	PlanLabels map[string]string
}

// remainingOf is the single conversion from quota-cache's USED percentage on
// 0-100 to the remaining fraction on 0-1 this document reports. Inverting it
// twice, or not at all, fails silently and plausibly, so it exists exactly once
// and is asserted by name in the tests.
func remainingOf(used float64) (float64, string) {
	switch {
	case math.IsNaN(used) || math.IsInf(used, 0):
		// No information. Report no remaining capacity rather than full, since
		// overstating headroom is the damaging direction.
		return 0, issuePercentInvalid
	case used < 0:
		return 1, issuePercentOutOfRange
	case used > 100:
		return 0, issuePercentOutOfRange
	}
	return (100 - used) / 100, ""
}

func percentOf(fraction float64) int { return int(math.Round(fraction * 100)) }

func levelOf(remaining float64) string {
	switch {
	case remaining < criticalBelow:
		return LevelCritical
	case remaining < lowBelow:
		return LevelLow
	}
	return LevelOK
}

func rowIDOf(w qc.EntryWindow) string {
	if w.Model != "" {
		return w.Key + ":" + w.Model
	}
	return w.Key
}

func rowOrderOf(key string) int {
	switch key {
	case qc.WindowSession:
		return 10
	case qc.WindowWeekly:
		return 20
	case qc.WindowWeeklyFable:
		return 30
	case qc.WindowModelSession:
		return 40
	case qc.WindowModelWeekly:
		return 50
	case qc.WindowCredits:
		return 60
	case qc.WindowMonthly:
		return 70
	}
	return 100 // raw: sorts last
}

// rowTitleOf is the fallback heading for a canonical key. quota-cache supplies
// a title today, but a snapshot written without one — including the acceptance
// example in the handoff — would otherwise render raw keys as card headings.
func rowTitleOf(key, model string) string {
	base := ""
	switch key {
	case qc.WindowSession:
		base = "Session"
	case qc.WindowWeekly:
		base = "Weekly"
	case qc.WindowWeeklyFable:
		base = "Weekly (Fable)"
	case qc.WindowModelSession:
		base = "Session"
	case qc.WindowModelWeekly:
		base = "Weekly"
	case qc.WindowCredits:
		base = "Credits"
	case qc.WindowMonthly:
		base = "Monthly"
	default:
		// raw:<provider>:<upstream id> — show the upstream id, which is the
		// only part that means anything to a reader.
		rest := strings.TrimPrefix(key, qc.WindowRawPrefix)
		if colon := strings.Index(rest, ":"); colon >= 0 {
			return rest[colon+1:]
		}
		return rest
	}
	if model != "" {
		return base + " (" + model + ")"
	}
	return base
}

func providerOrderOf(provider string) int {
	switch provider {
	case "claude":
		return 10
	case "codex":
		return 20
	case "xai":
		return 30
	}
	return 100
}

func providerTitleOf(provider string) string {
	switch provider {
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	case "xai":
		return "xAI"
	}
	return provider
}

// emailOf derives the display address from the auth index:
// claude-siphorchannel@example.com.json -> siphorchannel@example.com. The host's
// own email is used only when the derivation produces nothing address-shaped.
func emailOf(id Identity) string {
	derived := strings.TrimSuffix(id.AuthIndex, ".json")
	derived = strings.TrimPrefix(derived, id.Provider+"-")
	if !strings.Contains(derived, "@") && id.Email != "" {
		return id.Email
	}
	return derived
}

// windowsOf falls back to a single weekly window synthesized from the
// entry's top-level fields. That is what lets this plugin work against a
// snapshot written before canonical windows existed; the remaining rows appear
// on their own once the writer supplies them.
func windowsOf(entry qc.Entry) []qc.EntryWindow {
	if len(entry.Windows) > 0 {
		return entry.Windows
	}
	if entry.ObservedAt.IsZero() {
		return nil
	}
	return []qc.EntryWindow{{
		Key: qc.WindowWeekly, Title: "Weekly",
		UsedPercent: entry.Percent, ResetAt: entry.ResetAt, ObservedAt: entry.ObservedAt,
	}}
}

// humanDuration renders a countdown the way the design prints it.
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d / time.Second)
	days, hours, minutes := total/86400, (total%86400)/3600, (total%3600)/60
	switch {
	case days > 0 && hours > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case days > 0:
		return fmt.Sprintf("%dd", days)
	case hours > 0 && minutes > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	case hours > 0:
		return fmt.Sprintf("%dh", hours)
	case minutes > 0:
		return fmt.Sprintf("%dm", minutes)
	}
	return "<1m"
}

func shortName(email string) string {
	if i := strings.Index(email, "@"); i > 0 {
		return email[:i]
	}
	return email
}

// record pairs one rostered credential with its snapshot entry.
type record struct {
	identity  Identity
	entry     qc.Entry
	hasEntry  bool
	windows   []qc.EntryWindow
	weekly    time.Time
	hasWeekly bool
	status    string
	stale     bool
	// observed is true when anything has ever been successfully read for this
	// credential, whether or not it produced a weekly window.
	observed bool
	// freshest is the newest observation across the entry and its windows.
	freshest time.Time
}

// Build renders the document. now is supplied by the caller; nothing in this
// package reads the clock.
func Build(in Input, now time.Time) Document {
	// Truncate to the second before anything derives from it. The document
	// reports whole seconds, so a now carrying nanoseconds would make
	// generatedAtEpoch + resetInSeconds disagree with resetAtEpoch by one, and
	// round every countdown down a second — enough to print "1h 14m" where the
	// same data at a whole second prints "1h 15m".
	now = now.Truncate(time.Second)
	doc := Document{
		SchemaVersion:    SchemaVersion,
		GeneratedAtEpoch: now.Unix(),
		Credentials:      []Credential{},
		Providers:        []Provider{},
	}

	records := make([]record, 0, len(in.Identities))
	var newestObservation time.Time
	for _, identity := range in.Identities {
		r := record{identity: identity}
		r.entry, r.hasEntry = in.Snapshot.Entries[qc.Key(identity.Provider, identity.AuthIndex)]
		if r.hasEntry {
			r.windows = windowsOf(r.entry)
			r.freshest = r.entry.ObservedAt
			for _, w := range r.windows {
				if w.Key == qc.WindowWeekly && !r.hasWeekly {
					r.weekly, r.hasWeekly = w.ResetAt, !w.ResetAt.IsZero()
				}
				if w.ObservedAt.After(r.freshest) {
					r.freshest = w.ObservedAt
				}
			}
			r.observed = !r.freshest.IsZero()
			if r.observed {
				r.stale = in.StaleAfter > 0 && now.Sub(r.freshest) > in.StaleAfter
				if r.freshest.After(newestObservation) {
					newestObservation = r.freshest
				}
			}
		}
		switch {
		case identity.Disabled:
			r.status = StatusDisabled
		case identity.Unavailable:
			r.status = StatusUnavailable
		case !r.hasEntry:
			r.status = StatusUnsupported
		// An entry exists but nothing has ever been observed for it — a
		// credential CPA has only just learned about. Windows are checked too:
		// a successful poll that produced no weekly window leaves the legacy
		// ObservedAt empty by design, and that credential is polled, not pending.
		case !r.observed:
			r.status = StatusPending
		// Failures, not LastError, marks a failed poll: the writer sets
		// LastError to "refresh pending" while an attempt is in flight, and a
		// credential mid-refresh is not a credential in error.
		case r.entry.Failures > 0:
			r.status = StatusError
		default:
			r.status = StatusOK
		}
		records = append(records, r)
	}

	// Sorted by weekly reset, soonest first. A credential with no weekly window
	// has nothing to sort on and goes last, ordered by id so the output is
	// stable. Every row reuses this order.
	sort.SliceStable(records, func(i, j int) bool {
		a, b := records[i], records[j]
		if a.hasWeekly != b.hasWeekly {
			return a.hasWeekly
		}
		if a.hasWeekly && !a.weekly.Equal(b.weekly) {
			return a.weekly.Before(b.weekly)
		}
		return a.identity.AuthIndex < b.identity.AuthIndex
	})

	for _, r := range records {
		doc.Counters.Credentials++
		switch r.status {
		case StatusOK:
			doc.Counters.ObservedOK++
		case StatusError:
			doc.Counters.ObserveError++
		}
		doc.Credentials = append(doc.Credentials, Credential{
			ID:                r.identity.AuthIndex,
			Email:             emailOf(r.identity),
			Provider:          r.identity.Provider,
			Plan:              planLabelOf(r.identity.Provider, r.entry.Plan, in.PlanLabels),
			Status:            r.status,
			LastObservedEpoch: epochOf(r.freshest),
		})
	}

	doc.Providers = buildProviders(records, in, now)
	applyStaleness(&doc, in, newestObservation, now)
	return doc
}

func epochOf(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func applyStaleness(doc *Document, in Input, newestObservation time.Time, now time.Time) {
	reason := ""
	switch {
	case in.SourceReason != "":
		reason = in.SourceReason
	case newestObservation.IsZero():
		reason = ReasonNeverObserved
	case in.StaleAfter > 0 && now.Sub(newestObservation) > in.StaleAfter:
		reason = ReasonCacheStale
	}
	if reason != "" {
		doc.Stale = true
		doc.StaleReason = &reason
	}
}

func buildProviders(records []record, in Input, now time.Time) []Provider {
	byProvider := map[string][]record{}
	providerIDs := []string{}
	for _, r := range records {
		id := r.identity.Provider
		if _, seen := byProvider[id]; !seen {
			providerIDs = append(providerIDs, id)
		}
		byProvider[id] = append(byProvider[id], r)
	}
	sort.SliceStable(providerIDs, func(i, j int) bool {
		a, b := providerOrderOf(providerIDs[i]), providerOrderOf(providerIDs[j])
		if a != b {
			return a < b
		}
		return providerIDs[i] < providerIDs[j]
	})

	providers := make([]Provider, 0, len(providerIDs))
	for _, id := range providerIDs {
		members := byProvider[id]
		providers = append(providers, Provider{
			ID:              id,
			Title:           providerTitleOf(id),
			Order:           providerOrderOf(id),
			CredentialCount: len(members),
			Rows:            buildRows(members, in, now),
		})
	}
	return providers
}

// member is one credential's contribution to one row.
type member struct {
	record    record
	window    qc.EntryWindow
	remaining float64
}

func buildRows(records []record, in Input, now time.Time) []Row {
	byRow := map[string][]member{}
	rowIDs := []string{}
	titles := map[string]string{}
	windowKeys := map[string]string{}
	models := map[string]string{}
	for _, r := range records {
		// A disabled or unavailable credential is not a member of any row. It
		// stays in the catalog so the count still reflects reality.
		if r.identity.Disabled || r.identity.Unavailable || !r.observed {
			continue
		}
		claimed := map[string]bool{}
		for _, w := range r.windows {
			id := rowIDOf(w)
			// One credential contributes at most once to a row. quota-cache
			// already demotes colliding windows, but a document where a
			// credential appears twice in one row would weight it twice in the
			// mean and break the "same order as credentials[]" invariant the
			// client keys against.
			if claimed[id] {
				continue
			}
			claimed[id] = true
			if _, seen := byRow[id]; !seen {
				rowIDs = append(rowIDs, id)
				titles[id], windowKeys[id], models[id] = w.Title, w.Key, w.Model
			}
			remaining, _ := remainingOf(w.UsedPercent)
			byRow[id] = append(byRow[id], member{record: r, window: w, remaining: remaining})
		}
	}
	sort.SliceStable(rowIDs, func(i, j int) bool {
		a, b := rowOrderOf(windowKeys[rowIDs[i]]), rowOrderOf(windowKeys[rowIDs[j]])
		if a != b {
			return a < b
		}
		return rowIDs[i] < rowIDs[j]
	})

	rows := make([]Row, 0, len(rowIDs))
	for _, id := range rowIDs {
		title := titles[id]
		if title == "" {
			title = rowTitleOf(windowKeys[id], models[id])
		}
		rows = append(rows, Row{
			RowID:     id,
			Title:     title,
			Order:     rowOrderOf(windowKeys[id]),
			Matched:   !strings.HasPrefix(windowKeys[id], qc.WindowRawPrefix),
			Aggregate: buildAggregate(id, byRow[id], len(records), in, now),
			Entries:   buildEntries(byRow[id], now),
		})
	}
	return rows
}

// buildAggregate computes the row summary. The mean is arithmetic over member
// credentials only: a credential that did not report this window is excluded
// rather than counted as full, which would let a silent credential inflate the
// number that the whole card is read from.
func buildAggregate(rowID string, members []member, providerCredentials int, in Input, now time.Time) Aggregate {
	excluded := providerCredentials - len(members)
	if excluded < 0 {
		excluded = 0
	}
	agg := Aggregate{
		MemberCount:   len(members),
		ExcludedCount: excluded,
		Trend:         TrendUnknown,
	}
	if len(members) == 0 {
		agg.Level = levelOf(0)
		return agg
	}
	sum := 0.0
	for _, m := range members {
		sum += m.remaining
	}
	agg.RemainingFraction = sum / float64(len(members))
	agg.RemainingPercent = percentOf(agg.RemainingFraction)
	agg.Level = levelOf(agg.RemainingFraction)

	var soonest time.Time
	var soonestName string
	for _, m := range members {
		reset := m.window.ResetAt
		if reset.IsZero() || !reset.After(now) {
			continue
		}
		if soonest.IsZero() || reset.Before(soonest) {
			soonest, soonestName = reset, shortName(emailOf(m.record.identity))
		}
	}
	current := make([]observation, 0, len(members))
	for _, m := range members {
		current = append(current, observation{authIndex: m.record.identity.AuthIndex, remaining: m.remaining})
	}
	agg.Trend = trendOf(in.Samples, current, rowID, now)
	if soonest.IsZero() {
		return agg
	}

	epoch, seconds := soonest.Unix(), int64(soonest.Sub(now)/time.Second)
	agg.SoonestResetAtEpoch, agg.SoonestResetInSeconds = &epoch, &seconds

	// Capacity the row regains when the soonest window resets. Members resetting
	// in the same minute land together, because a card claiming two separate
	// gains seconds apart would be noise.
	gain := 0.0
	for _, m := range members {
		reset := m.window.ResetAt
		if reset.IsZero() || reset.Before(soonest) || reset.After(soonest.Add(time.Minute)) {
			continue
		}
		gain += 1 - m.remaining
	}
	agg.ProjectedGainPercent = percentOf(gain / float64(len(members)))

	// Precomputed so the wording lives in one place rather than in each client.
	countdown := humanDuration(soonest.Sub(now))
	if agg.ProjectedGainPercent > 0 {
		agg.Subtext = fmt.Sprintf("+%d%% when %s resets in %s", agg.ProjectedGainPercent, soonestName, countdown)
	} else {
		agg.Subtext = fmt.Sprintf("%s resets in %s", soonestName, countdown)
	}
	return agg
}

func buildEntries(members []member, now time.Time) []RowEntry {
	entries := make([]RowEntry, 0, len(members))
	for _, m := range members {
		remaining, issue := remainingOf(m.window.UsedPercent)
		entry := RowEntry{
			CredentialID:      m.record.identity.AuthIndex,
			RemainingFraction: remaining,
			RemainingPercent:  percentOf(remaining),
			Level:             levelOf(remaining),
			ResetDisplayHint:  HintNone,
			ObservedAtEpoch:   epochOf(m.window.ObservedAt),
			NextAttemptEpoch:  epochOf(m.record.entry.NextAttempt),
			SourceWindowKey:   m.window.Key,
			DataIssues:        []string{},
			State:             StatusOK,
		}
		if m.window.Model != "" {
			model := m.window.Model
			entry.SourceModel = &model
		}
		if !m.window.ResetAt.IsZero() {
			epoch := m.window.ResetAt.Unix()
			entry.ResetAtEpoch = &epoch
			seconds := int64(0)
			if m.window.ResetAt.After(now) {
				// Upstream reset instants drift; a reset just in the past is
				// normal between the window turning over and the next poll, so
				// the client is told not to count down rather than shown a
				// negative number.
				seconds = int64(m.window.ResetAt.Sub(now) / time.Second)
				entry.ResetDisplayHint = HintCountdown
			} else {
				entry.DataIssues = append(entry.DataIssues, issueResetInPast)
			}
			entry.ResetInSeconds = &seconds
		}
		if issue != "" {
			entry.DataIssues = append(entry.DataIssues, issue)
		}
		if m.window.LastError != "" || m.record.entry.Failures > 0 {
			entry.DataIssues = append(entry.DataIssues, issueObserveError)
			entry.State = StatusError
		} else if m.record.entry.LastError != "" {
			entry.DataIssues = append(entry.DataIssues, issueRefreshPending)
		}
		if m.record.stale {
			entry.DataIssues = append(entry.DataIssues, issueStale)
			if entry.State == StatusOK {
				entry.State = StateStale
			}
		}
		entries = append(entries, entry)
	}
	return entries
}
