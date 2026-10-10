package aggregate

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/overrides"
)

// Level thresholds, in the whole percent the page prints: 41% and up is ok,
// 40% down to 11% is low, and 10% and under is critical. Bars are blue, amber
// and red in turn, and the big number green, amber and red. The rule lives
// here so every client agrees and none of them recomputes it in CSS.
const (
	criticalAtOrBelow = 10
	lowAtOrBelow      = 40
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
	// provider-reported value in lower case, bare or as "provider:value". See
	// planLabelOf for which wins.
	PlanLabels map[string]string
	// Redeemable reports whether this plugin is configured and able to spend a
	// banked reset on the operator's behalf. It is an input rather than a
	// constant because it depends on configuration and on the host callbacks
	// available at runtime, neither of which this package may look at.
	Redeemable bool
	// BalanceWarnBelow is the amount below which a prepaid balance is reported
	// low. Zero turns the warning off; an empty balance is critical regardless.
	BalanceWarnBelow float64
	// Meter is quota-cache's API meter, read beside the snapshot, and nil when
	// its file is missing or cannot be read. The API credit estimates are
	// priced from it.
	Meter *qc.APIMeter
	// Overrides is what the operator set from the dashboard: API credit
	// amounts, refill dates and Console readings, and Claude renewal dates.
	// They win over quota-cache's configuration, field by field.
	Overrides overrides.Values
	// AllowEdit reports whether the page may change those values (allow-edit).
	// Stored values apply either way.
	AllowEdit bool
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

// levelOf judges the rounded percent rather than the raw fraction, so the
// colour always agrees with the figure printed beside it: a pool reading
// 40.4% is printed "40%" and is amber, never a green "40".
func levelOf(remaining float64) string {
	switch percent := percentOf(remaining); {
	case percent <= criticalAtOrBelow:
		return LevelCritical
	case percent <= lowAtOrBelow:
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
		Balances:         balancesOf(in, now),
		APICredits:       apiCreditsOf(in, now),
		RenewalOrphans:   renewalOrphansOf(in),
	}
	renewalsEditable := editingOf(in).Available

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
		stored, hasStored := in.Overrides.Renewals[r.identity.AuthIndex]
		var setting *overrides.Renewal
		if hasStored {
			setting = &stored
		}
		renewal, source := renewalFor(r, setting, now)
		doc.Credentials = append(doc.Credentials, Credential{
			ID:                     r.identity.AuthIndex,
			Email:                  emailOf(r.identity),
			Provider:               r.identity.Provider,
			Plan:                   planLabelOf(r.identity.Provider, r.entry.Plan, in.PlanLabels),
			Status:                 r.status,
			LastObservedEpoch:      epochOf(r.freshest),
			Activity:               activityOf(r.identity.Recent, peaks[r.identity.Provider], now),
			ResetCredits:           resetCreditsOf(r, in.Redeemable, now),
			RenewalAtEpoch:         renewal,
			RenewalEstimated:       source != nil && *source == renewalEstimated,
			RenewalSource:          source,
			RenewalEstimateAtEpoch: renewalEstimateOf(r, now),
			RenewalEditable:        renewalsEditable && renewalEditable(r.identity),
			RenewalSetting:         renewalSettingOf(setting),
			Credits:                accountCreditsOf(r),
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

// WithSettings is doc with every part the dashboard's settings decide taken
// from fresh: the API credit card, the renewal orphans, and each credential's
// renewal. doc is a document served again while its source is failing, and
// fresh is built from the same snapshot and roster with the settings as they
// are now, so a value saved meanwhile shows at once and the page's next save
// starts from its revision. doc itself is not modified.
func WithSettings(doc, fresh Document) Document {
	doc.APICredits = fresh.APICredits
	doc.RenewalOrphans = fresh.RenewalOrphans
	now := make(map[string]Credential, len(fresh.Credentials))
	for _, credential := range fresh.Credentials {
		now[credential.ID] = credential
	}
	doc.Credentials = slices.Clone(doc.Credentials)
	for i := range doc.Credentials {
		c := &doc.Credentials[i]
		if f, ok := now[c.ID]; ok {
			c.RenewalAtEpoch, c.RenewalEstimated, c.RenewalSource = f.RenewalAtEpoch, f.RenewalEstimated, f.RenewalSource
			c.RenewalEstimateAtEpoch, c.RenewalEditable, c.RenewalSetting = f.RenewalEstimateAtEpoch, f.RenewalEditable, f.RenewalSetting
		}
	}
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
		// Every account holding a reset gets its own button, so the operator
		// chooses which account to spend on. Two things take it away. A
		// disabled credential is one the operator switched off, and spending on
		// it would be acting against that. And a provider this plugin cannot
		// redeem on has nothing to press.
		//
		// CPA's routing state deliberately does not. A credential CPA has
		// parked in a cooldown is reported unavailable, but that is a statement
		// about routing, not about the account: CPA still hands its token to
		// host.auth.get, quota-cache polls it with that same token, and the
		// provider still answers. A cooldown is also exactly when an operator
		// reaches for a reset — hiding the button then removed it from the one
		// account that needed it.
		Redeemable: redeemable && redeemableProvider(r.identity.Provider) && !r.identity.Disabled,
	}
	// An expiry already behind us is dropped rather than counted down past
	// zero: quota-cache filters the same way at the poll, and this covers the
	// gap between that poll and this build.
	if at := source.SoonestExpiry; at != nil && at.After(now) {
		epoch, seconds := at.Unix(), int64(at.Sub(now)/time.Second)
		credits.ExpiresAtEpoch, credits.ExpiresInSeconds = &epoch, &seconds
	}
	credits.Hold, credits.HoldUntilEpoch = holdOf(source, now)
	return credits
}

// redeemableProvider reports whether this plugin knows how to spend a banked
// reset on a provider. The redeem package refuses every other provider as
// well; this keeps the button from being offered for a press that would only be
// refused.
func redeemableProvider(provider string) bool {
	return provider == "codex" || provider == "claude"
}

// holdOf translates quota-cache's reading of why no reset can be spent right
// now into this document's vocabulary.
//
// A hold this build does not know is dropped rather than passed through: it is
// a provider string, and an unrecognised one has no wording a client could put
// beside the button. A cooldown whose end has already passed is dropped too,
// along with its instant, for the same reason an expired expiry is — it was
// true at the poll and is not now, and a hint that contradicts the clock is
// worse than none. The hint never hides the button; the plugin asks the
// provider afresh before spending anything.
func holdOf(source *qc.ResetCredits, now time.Time) (string, *int64) {
	hold := ""
	switch source.Hold {
	case "not_limited":
		hold = "notLimited"
	case "cooldown", "paused", "ineligible":
		hold = source.Hold
	default:
		return "", nil
	}
	if hold != "cooldown" || source.HoldUntil == nil {
		return hold, nil
	}
	if !source.HoldUntil.After(now) {
		return "", nil
	}
	epoch := source.HoldUntil.Unix()
	return hold, &epoch
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
	record record
	window qc.EntryWindow
	// remaining is the window's own reading, which the entry prints.
	remaining float64
	// pooled is what the member counts as in the row's mean. It is remaining
	// on every row but one a Claude weekly caps; see gatedBy.
	pooled float64
	// heldOut leaves the member out of the mean, the recovery and the shares,
	// while its entry still prints its reading.
	heldOut bool
	// weekly is the credential's weekly gate on a row it caps, so the row's
	// recovery counts the weekly's reset as well as the window's own. Unknown
	// on every other row.
	weekly weeklyGate
}

// resets lists the instants at which the member's pooled value can rise: its
// window's own reset and, on a row its weekly caps, the weekly's. Either may be
// zero, which is no instant at all.
func (m member) resets() []time.Time {
	if m.weekly.known {
		return []time.Time{m.window.ResetAt, m.weekly.reset}
	}
	return []time.Time{m.window.ResetAt}
}

// resetsAt reports whether one of the member's resets falls exactly at t.
func (m member) resetsAt(t time.Time) bool {
	for _, reset := range m.resets() {
		if reset.Equal(t) {
			return true
		}
	}
	return false
}

// pooledAfter is the member's pooled value once every reset after now and no
// later than through has restored its window. Neither the window nor the
// weekly can fall at a reset, so neither can the lesser of them, and the gain
// this is measured against is never negative. On an uncapped row it is 1 for a
// member resetting in that span: everything it has used comes back, which is
// all a recovery has ever projected.
func (m member) pooledAfter(now, through time.Time) float64 {
	restored := func(reset time.Time) bool { return reset.After(now) && !reset.After(through) }
	after := m.remaining
	if restored(m.window.ResetAt) {
		after = 1
	}
	if !m.weekly.known {
		return after
	}
	weekly := m.weekly.remaining
	if restored(m.weekly.reset) {
		weekly = 1
	}
	return math.Min(after, weekly)
}

// pooledOf is the members the row's mean is taken over, in catalog order.
func pooledOf(members []member) []member {
	pooled := make([]member, 0, len(members))
	for _, m := range members {
		if !m.heldOut {
			pooled = append(pooled, m)
		}
	}
	return pooled
}

func buildRows(records []record, in Input, now time.Time) []Row {
	byRow := map[string][]member{}
	rowIDs := []string{}
	titles := map[string]string{}
	windowKeys := map[string]string{}
	models := map[string]string{}
	for _, r := range records {
		// Whether a credential lands in a row turns on one thing only: did it
		// report this window. A credential CPA has parked in a quota cooldown
		// reports the same figures it did a minute earlier, and those figures
		// are the whole reason to look at this dashboard — a credential
		// vanishing from every card at the exact moment it runs out is the
		// opposite of what the card is read for. Disabled is the same story: the
		// quota is still there and comes back with the credential.
		//
		// The mean is narrower on Claude's session row only. A Claude credential
		// whose weekly is spent still lands in its session row here and is still
		// listed with its reading. gatedBy marks it held out, deciding from the
		// weekly reading alone and never from routing state, and pooledOf then
		// keeps it out of the mean and out of memberCount.
		//
		// A credential with no observation at all has nothing to average. It is
		// not dropped either; it lands below as an entry with no reading.
		if !r.observed {
			continue
		}
		// Read once per credential, before its windows, so every row its weekly
		// bears on is judged against the same reading.
		gate := weeklyGateOf(r.windows, now)
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
			m := member{record: r, window: w, remaining: remaining, pooled: remaining}
			byRow[id] = append(byRow[id], gatedBy(m, gate))
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
		// Found once and handed to both halves of the row, so the gain the
		// aggregate announces and the per-entry shares a client draws it from
		// are taken over the same credentials by the same rule. A held-out
		// member is in neither: its reset returns nothing the row can use.
		members := byRow[id]
		pooled := pooledOf(members)
		next := nextRecoveryOf(pooled, now)
		rows = append(rows, Row{
			RowID:     id,
			Title:     title,
			Order:     rowOrderOf(windowKeys[id]),
			Matched:   !strings.HasPrefix(windowKeys[id], qc.WindowRawPrefix),
			Aggregate: buildAggregate(id, pooled, len(members)-len(pooled), next, len(records), in, now),
			Entries:   buildEntries(records, members, len(pooled), next, now),
		})
	}
	return rows
}

// recovery is a row's next recovery: the soonest future reset among its
// members, and every member resetting with it.
type recovery struct {
	// at is the soonest future reset, and zero when no member has one.
	at time.Time
	// with holds the auth index of each member with a reset at `at` or within
	// the minute after it. They land together because a card claiming two
	// separate gains seconds apart would be noise.
	with map[string]bool
	// gain is what each member in `with` regains then, on its own 0-1 scale:
	// everything it has used, on an uncapped row, and on a row its weekly caps
	// the rise in the lesser of the two. A Fable reset under a spent weekly
	// returns nothing, which is the point of the cap.
	gain map[string]float64
}

func nextRecoveryOf(members []member, now time.Time) recovery {
	next := recovery{with: map[string]bool{}, gain: map[string]float64{}}
	for _, m := range members {
		for _, reset := range m.resets() {
			if reset.IsZero() || !reset.After(now) {
				continue
			}
			if next.at.IsZero() || reset.Before(next.at) {
				next.at = reset
			}
		}
	}
	if next.at.IsZero() {
		return next
	}
	through := next.at.Add(time.Minute)
	for _, m := range members {
		for _, reset := range m.resets() {
			if reset.IsZero() || reset.Before(next.at) || reset.After(through) {
				continue
			}
			id := m.record.identity.AuthIndex
			next.with[id] = true
			next.gain[id] = m.pooledAfter(now, through) - m.pooled
			break
		}
	}
	return next
}

// fullAgainOf is when the row would read 100% if nothing more were used: the
// latest reset among the members below full, since a reset restores its window
// outright and a full member has nothing to wait for. A member its weekly caps
// waits for each of the two that is below full.
//
// It is unknowable, and reported as such, when a reset a member waits for has
// no instant at all — that window does not refill on a schedule. A member
// whose reset has already passed is mid-turnover and refilling now, so it
// neither delays the instant nor voids it. Zero when there is nothing ahead to
// count down to: every member full, or every one below full already turning
// over.
func fullAgainOf(members []member, now time.Time) time.Time {
	latest := now
	for _, m := range members {
		if m.pooled >= 1 {
			continue
		}
		needed := make([]time.Time, 0, 2)
		if m.remaining < 1 {
			needed = append(needed, m.window.ResetAt)
		}
		if m.weekly.known && m.weekly.remaining < 1 {
			needed = append(needed, m.weekly.reset)
		}
		for _, reset := range needed {
			if reset.IsZero() {
				return time.Time{}
			}
			if reset.After(latest) {
				latest = reset
			}
		}
	}
	if !latest.After(now) {
		return time.Time{}
	}
	return latest
}

// buildAggregate computes the row summary. The mean is arithmetic over the
// pooled members only: a credential that did not report this window is excluded
// rather than counted as full, which would let a silent credential inflate the
// number that the whole card is read from, and one held out by its weekly is
// excluded for the same reason.
//
// Every reporting credential held out leaves nothing to average, and the row
// reads 0% critical with nothing ahead: no account in it can send.
func buildAggregate(rowID string, pooled []member, heldOut int, next recovery, providerCredentials int, in Input, now time.Time) Aggregate {
	excluded := providerCredentials - len(pooled)
	if excluded < 0 {
		excluded = 0
	}
	agg := Aggregate{
		MemberCount:   len(pooled),
		ExcludedCount: excluded,
		HeldOutCount:  heldOut,
		Trend:         TrendUnknown,
	}
	if len(pooled) == 0 {
		agg.Level = levelOf(0)
		return agg
	}
	sum := 0.0
	for _, m := range pooled {
		sum += m.pooled
	}
	agg.RemainingFraction = sum / float64(len(pooled))
	agg.RemainingPercent = percentOf(agg.RemainingFraction)
	agg.Level = levelOf(agg.RemainingFraction)

	soonest := next.at
	// The subtext names the first member resetting at that exact instant —
	// which, with members in catalog order, is the credential at the top of the
	// card.
	var soonestName string
	for _, m := range pooled {
		if !soonest.IsZero() && m.resetsAt(soonest) {
			soonestName = shortName(emailOf(m.record.identity))
			break
		}
	}
	// The trend is of the headline, so it compares what each member counts as
	// in the mean, over the members the mean is taken over.
	current := make([]observation, 0, len(pooled))
	for _, m := range pooled {
		current = append(current, observation{authIndex: m.record.identity.AuthIndex, remaining: m.pooled})
	}
	agg.Trend = trendOf(in.Samples, current, rowID, now)
	if soonest.IsZero() {
		return agg
	}

	epoch, seconds := soonest.Unix(), int64(soonest.Sub(now)/time.Second)
	agg.SoonestResetAtEpoch, agg.SoonestResetInSeconds = &epoch, &seconds
	if full := fullAgainOf(pooled, now); !full.IsZero() {
		epoch, seconds := full.Unix(), int64(full.Sub(now)/time.Second)
		agg.FullAtEpoch, agg.FullInSeconds = &epoch, &seconds
	}

	// Capacity the row regains when the soonest window resets: what each member
	// resetting with it gets back, as a share of the whole row. The same sum,
	// member by member, is each entry's recoveryShare.
	gain := 0.0
	for _, m := range pooled {
		if id := m.record.identity.AuthIndex; next.with[id] {
			gain += next.gain[id]
		}
	}
	agg.ProjectedGainFraction = gain / float64(len(pooled))
	agg.ProjectedGainPercent = percentOf(agg.ProjectedGainFraction)

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
func buildEntries(records []record, members []member, pooledCount int, next recovery, now time.Time) []RowEntry {
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
		// This credential's slice of the row, in the row's own units: the mean
		// is taken over pooled members, so each holds an equal 1/members of the
		// bar, and a held-out one holds none. Served rather than left to the
		// client so that a weighted mean, if the row ever takes one, changes
		// the slices in the same place as the headline they add up to.
		poolShare, recoveryShare, resetsNext := 0.0, 0.0, false
		if !m.heldOut {
			weight := 1 / float64(pooledCount)
			poolShare = m.pooled * weight
			resetsNext = next.with[m.record.identity.AuthIndex]
			if resetsNext {
				recoveryShare = next.gain[m.record.identity.AuthIndex] * weight
			}
		}
		entry := RowEntry{
			CredentialID:      m.record.identity.AuthIndex,
			HasReading:        true,
			RemainingFraction: remaining,
			RemainingPercent:  percentOf(remaining),
			Level:             levelOf(remaining),
			HeldOut:           m.heldOut,
			PooledFraction:    m.pooled,
			PooledPercent:     percentOf(m.pooled),
			PoolShare:         poolShare,
			RecoveryShare:     recoveryShare,
			ResetsNext:        resetsNext,
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
