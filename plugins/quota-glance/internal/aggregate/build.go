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

// Where a request bucket lands on the intensity ramp, as a share of the busiest
// bucket in its provider. Same reasoning as the capacity thresholds above: the
// rule is decided once, here, rather than in each client's stylesheet.
const (
	intensityMediumAbove = 0.34
	intensityHighAbove   = 0.67

	// defaultBucketSeconds is CPA's own width for a recent-request bucket. It
	// is a fallback only — the width is read off the host's labels, so a host
	// that changes it relabels the strip instead of quietly mislabelling it.
	defaultBucketSeconds = 600
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
	// Redeemable reports whether this plugin is configured and able to spend a
	// banked reset on the operator's behalf. It is an input rather than a
	// constant because it depends on configuration and on the host callbacks
	// available at runtime, neither of which this package may look at.
	Redeemable bool
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

// bucketSecondsOf reads the ring's bucket width off the host's own label,
// "14:50-15:00".
//
// Only the difference between the two halves is used. The label is in the CPA
// process's local time and carries no date, so parsing it into an instant would
// be wrong twice a year and wrong every night at midnight; a width is the same
// number in every zone. A label that spans midnight subtracts to a negative and
// is wrapped rather than discarded.
func bucketSecondsOf(recent []RecentRequest) int {
	for _, bucket := range recent {
		from, to, ok := strings.Cut(bucket.Label, "-")
		if !ok {
			continue
		}
		start, errStart := time.Parse("15:04", strings.TrimSpace(from))
		end, errEnd := time.Parse("15:04", strings.TrimSpace(to))
		if errStart != nil || errEnd != nil {
			continue
		}
		width := int(end.Sub(start) / time.Second)
		if width <= 0 {
			width += 24 * 3600
		}
		if width > 0 && width <= 24*3600 {
			return width
		}
	}
	return defaultBucketSeconds
}

// intensityOf places one bucket on the ink ramp, against the busiest bucket
// anywhere in its provider.
//
// Provider-wide on purpose. A strip scaled to its own row makes the credential
// taking a trickle look exactly like the one carrying the pool, which is the
// single question the strip exists to answer.
func intensityOf(total, peak int64) int {
	if total <= 0 || peak <= 0 {
		return IntensityNone
	}
	switch share := float64(total) / float64(peak); {
	case share > intensityHighAbove:
		return IntensityHigh
	case share > intensityMediumAbove:
		return IntensityMedium
	}
	return IntensityLow
}

// peaksOf is the busiest bucket in each provider, the denominator every strip
// in that provider is drawn against.
func peaksOf(records []record) map[string]int64 {
	peaks := map[string]int64{}
	for _, r := range records {
		for _, bucket := range r.identity.Recent {
			if total := bucket.Success + bucket.Failed; total > peaks[r.identity.Provider] {
				peaks[r.identity.Provider] = total
			}
		}
	}
	return peaks
}

// activityOf renders one credential's recent traffic.
//
// The host dates its buckets only with a local-time label, so instants are
// derived from the width instead: buckets are aligned to multiples of that
// width in Unix seconds — the same arithmetic CPA itself uses to choose a
// bucket — and bucket i of n ends (n-1-i) widths before the end of the one now
// in progress. A roster read microseconds either side of a boundary can date
// every bucket one place out; the next rebuild corrects it, and the only field
// that could mislead in the meantime is a last-request time ten minutes stale.
func activityOf(recent []RecentRequest, peak int64, now time.Time) *Activity {
	if len(recent) == 0 {
		// This host reports no counter. Null says exactly that, where a ring of
		// zeroes would claim a credential nothing has routed to in hours.
		return nil
	}
	seconds := bucketSecondsOf(recent)
	width := time.Duration(seconds) * time.Second
	currentEnd := time.Unix((now.Unix()/int64(seconds))*int64(seconds), 0).UTC().Add(width)

	activity := &Activity{
		BucketSeconds: seconds,
		WindowSeconds: seconds * len(recent),
		Buckets:       make([]ActivityBucket, 0, len(recent)),
	}
	for i, bucket := range recent {
		total := bucket.Success + bucket.Failed
		activity.Success += bucket.Success
		activity.Failed += bucket.Failed
		activity.Buckets = append(activity.Buckets, ActivityBucket{
			Success:   bucket.Success,
			Failed:    bucket.Failed,
			Intensity: intensityOf(total, peak),
		})
		if total == 0 {
			continue
		}
		// The bucket's end is the tightest bound its width supports — except
		// for the bucket in progress, which ends in the future. A last request
		// dated ahead of now would count up on a page that only counts down.
		end := currentEnd.Add(-time.Duration(len(recent)-1-i) * width)
		if end.After(now) {
			end = now
		}
		epoch := end.Unix()
		activity.LastRequestAtEpoch = &epoch
	}
	newest := recent[len(recent)-1]
	activity.Live = newest.Success+newest.Failed > 0
	return activity
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
	var newestObservation, soonestAttempt, soonestOverdue time.Time
	for _, identity := range in.Identities {
		r := record{identity: identity}
		r.entry, r.hasEntry = in.Snapshot.Entries[qc.Key(identity.Provider, identity.AuthIndex)]
		if r.hasEntry {
			// The soonest attempt still ahead of us, which is when this
			// document can next change. Attempts already in the past are
			// tracked separately rather than mixed in: one credential stuck in
			// backoff would otherwise hold the minimum in the past forever and
			// the header would read "due" while every other credential kept
			// polling on schedule.
			if at := r.entry.NextAttempt; !at.IsZero() {
				switch {
				case at.After(now):
					if soonestAttempt.IsZero() || at.Before(soonestAttempt) {
						soonestAttempt = at
					}
				case soonestOverdue.IsZero() || at.Before(soonestOverdue):
					soonestOverdue = at
				}
			}
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

	// One pass for the scale before any strip is drawn: every credential in a
	// provider is inked against the same busiest bucket.
	peaks := peaksOf(records)

	for _, r := range records {
		doc.Counters.Credentials++
		// Counted from the poll, not from r.status. Status folds CPA's routing
		// state over the poll outcome and reports a parked credential as
		// "unavailable" — but a parked credential is still polled, and its
		// readings are on every card. Reading the counter off status printed
		// "7 credentials · 3 observed" beneath seven rows showing six live
		// figures, which is the footer calling the cards liars.
		switch {
		case !r.observed:
			// Pending or unsupported: nothing has ever been read, so neither
			// counter claims it.
		case r.entry.Failures > 0:
			doc.Counters.ObserveError++
		default:
			doc.Counters.ObservedOK++
		}
		doc.Credentials = append(doc.Credentials, Credential{
			ID:                r.identity.AuthIndex,
			Email:             emailOf(r.identity),
			Provider:          r.identity.Provider,
			Plan:              planLabelOf(r.identity.Provider, r.entry.Plan, in.PlanLabels),
			Status:            r.status,
			LastObservedEpoch: epochOf(r.freshest),
			Activity:          activityOf(r.identity.Recent, peaks[r.identity.Provider], now),
			ResetCredits:      resetCreditsOf(r, in.Redeemable, now),
		})
	}

	doc.Providers = buildProviders(records, in, now)
	doc.ObservedAtEpoch = epochPointerOf(newestObservation)
	// Falls back to an overdue attempt only when nothing is scheduled ahead,
	// which is a poller that has genuinely stalled rather than one credential
	// waiting its turn. The client reads a past instant as "due".
	if soonestAttempt.IsZero() {
		soonestAttempt = soonestOverdue
	}
	doc.NextAttemptEpoch = epochPointerOf(soonestAttempt)
	applyStaleness(&doc, in, newestObservation, now)
	return doc
}

// resetCreditsOf carries the snapshot's banked-reset count onto the credential,
// and is nil whenever there is nothing to say — no entry, no count, or a count
// the snapshot reports as zero. Nil is what makes the badge absent rather than
// present and empty, which is the whole of the requirement.
//
// A stale entry still counts. The number moves only when the operator spends
// one or a grant lapses, so the last observation is very likely still true, and
// suppressing it would hide a credit on exactly the snapshot age where noticing
// its expiry matters most. Redeemability is the caller's judgement, not the
// snapshot's.
func resetCreditsOf(r record, redeemable bool, now time.Time) *ResetCredits {
	if !r.hasEntry || r.entry.Quota == nil || r.entry.Quota.ResetCredits == nil {
		return nil
	}
	source := r.entry.Quota.ResetCredits
	if source.AvailableCount < 1 {
		return nil
	}
	credits := &ResetCredits{
		AvailableCount: source.AvailableCount,
		// Only a credential CPA is willing to hand a token for can be spent,
		// and a credential CPA will not route to is one it will not authorize
		// either. The count still shows on a parked credential; the button does
		// not, because offering an action that is going to fail is worse than
		// not offering it.
		Redeemable: redeemable && r.identity.Provider == "codex" && !r.identity.Disabled && !r.identity.Unavailable,
	}
	// An expiry already behind us is dropped rather than counted down past
	// zero: quota-cache filters the same way at the poll, and this covers the
	// gap between that poll and this build.
	if at := source.SoonestExpiry; at != nil && at.After(now) {
		epoch, seconds := at.Unix(), int64(at.Sub(now)/time.Second)
		credits.ExpiresAtEpoch, credits.ExpiresInSeconds = &epoch, &seconds
	}
	return credits
}

func epochOf(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// epochPointerOf is the nullable form, for the document fields that report
// "there is nothing to say yet" rather than a zero instant.
func epochPointerOf(t time.Time) *int64 {
	if t.IsZero() {
		return nil
	}
	epoch := t.Unix()
	return &epoch
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
		// Membership turns on one thing only: did this credential report this
		// window. A credential CPA has parked in a quota cooldown reports the
		// same figures it did a minute earlier, and those figures are the whole
		// reason to look at this dashboard — a credential vanishing from every
		// card at the exact moment it runs out is the opposite of what the card
		// is read for. Disabled is the same story: the quota is still there and
		// comes back with the credential.
		//
		// A credential with no observation at all has nothing to average. It is
		// not dropped either; it lands below as an entry with no reading.
		if !r.observed {
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
			Entries:   buildEntries(records, byRow[id], now),
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

// buildEntries emits one entry per provider credential, in catalog order,
// whether or not it reported this window. A credential that did not is still
// printed, with HasReading false: the card is the place an operator counts
// their credentials, and a list that quietly omits the ones in trouble is worth
// less than no list.
func buildEntries(records []record, members []member, now time.Time) []RowEntry {
	byCredential := make(map[string]member, len(members))
	for _, m := range members {
		byCredential[m.record.identity.AuthIndex] = m
	}
	entries := make([]RowEntry, 0, len(records))
	for _, r := range records {
		m, isMember := byCredential[r.identity.AuthIndex]
		if !isMember {
			entries = append(entries, absentEntry(r))
			continue
		}
		remaining, issue := remainingOf(m.window.UsedPercent)
		entry := RowEntry{
			CredentialID:      m.record.identity.AuthIndex,
			HasReading:        true,
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

// absentEntry is a credential with nothing to report for this window. Level is
// left empty rather than computed from a zero remaining fraction, which would
// paint an unknown bright red; the contract has clients fall through to a
// neutral rendering on an enum value they do not recognise.
//
// State says why, but only ever about the missing reading. Disabled and
// unavailable are deliberately not repeated here: they say CPA will not route
// to the credential, which has no bearing on whether this window was reported,
// and a credential reading "ok" on the session card and "disabled" on the one
// below it describes itself two ways in the same column. That fact belongs to
// credentials[].status, once.
func absentEntry(r record) RowEntry {
	state := StateNoData
	switch {
	case !r.hasEntry:
		// quota-cache does not poll this provider at all.
		state = StatusUnsupported
	case !r.observed:
		state = StatusPending
	}
	return RowEntry{
		CredentialID:     r.identity.AuthIndex,
		ResetDisplayHint: HintNone,
		NextAttemptEpoch: epochOf(r.entry.NextAttempt),
		DataIssues:       []string{},
		State:            state,
	}
}
