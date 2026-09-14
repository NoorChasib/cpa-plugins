package quota

import (
	"sort"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// Canonical window derivation.
//
// Upstream window identifiers are provider vocabulary: free-form, unvalidated,
// and changeable. Normalizing them once here means no consumer pattern-matches
// a provider string. The verbatim projection stays available under Entry.Quota;
// this layer only adds a canonical view of it.
//
// A window with no canonical meaning is emitted under a raw: prefix rather than
// dropped, so an unrecognized upstream window is visible before it is mapped.

// sessionWindowSeconds is Claude's and Codex's five-hour window. The weekly
// duration is weeklyWindowSeconds, already defined for the primary parser.
const sessionWindowSeconds = 18000

type canonical struct{ key, model, title string }

// order ranks canonical keys so the emitted array is deterministic. The
// snapshot is rewritten on every poll; map iteration order would churn the file
// and make byte-for-byte comparison useless.
func order(key string) int {
	switch key {
	case client.WindowSession:
		return 0
	case client.WindowWeekly:
		return 1
	case client.WindowWeeklyFable:
		return 2
	case client.WindowModelSession:
		return 3
	case client.WindowModelWeekly:
		return 4
	case client.WindowCredits:
		return 5
	case client.WindowMonthly:
		return 6
	}
	return 7 // raw: sorts last
}

func raw(provider, id string) canonical {
	return canonical{key: client.WindowRawPrefix + provider + ":" + id, title: id}
}

// Synthetic ids for entries read out of the structured limits[] array. They are
// namespaced so they can never collide with a flat key Anthropic ships later,
// and the scoped one carries its model rather than being enumerated here.
const (
	limitsSession      = "limits/session"
	limitsWeeklyAll    = "limits/weekly_all"
	limitsWeeklyScoped = "limits/weekly_scoped/"
)

// fableScope reports whether a model-scoped weekly window is the premium
// allowance this document calls Fable. Anthropic has shipped it under more than
// one display name; both land on the same row rather than on two that each show
// half the picture.
func fableScope(model string) bool {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "fable", "opus", "claude fable", "claude opus":
		return true
	}
	return false
}

// mapClaude. Claude names its windows after their duration; seven_day is the
// account-wide allowance that Entry.Percent already mirrors. The limits/ ids
// come from the structured array that superseded those flat keys.
func mapClaude(id string) canonical {
	if scope, ok := strings.CutPrefix(id, limitsWeeklyScoped); ok {
		if fableScope(scope) {
			return canonical{key: client.WindowWeeklyFable, title: "Weekly (Fable)"}
		}
		return canonical{key: client.WindowModelWeekly, model: scope, title: "Weekly (" + scope + ")"}
	}
	switch id {
	case limitsSession:
		return canonical{key: client.WindowSession, title: "Session"}
	case limitsWeeklyAll:
		return canonical{key: client.WindowWeekly, title: "Weekly"}
	}
	switch id {
	case "five_hour":
		return canonical{key: client.WindowSession, title: "Session"}
	case "seven_day":
		return canonical{key: client.WindowWeekly, title: "Weekly"}
	case "seven_day_opus":
		return canonical{key: client.WindowWeeklyFable, title: "Weekly (Fable)"}
	case "seven_day_sonnet":
		return canonical{key: client.WindowModelWeekly, model: "sonnet", title: "Weekly (sonnet)"}
	}
	return raw("claude", id)
}

// mapCodex. Collected ids are "<group>/<slot>", where slot is the provider's
// primary/secondary naming. The slots are not stable across responses, so the
// window's declared duration decides the canonical key, never the slot name.
// The "regular" group is the account-wide limit; every other group is a
// per-feature or per-model limit and carries its name as Model.
func mapCodex(id string, duration int64, hasDuration bool) canonical {
	group := id
	if cut := strings.LastIndex(id, "/"); cut > 0 {
		group = id[:cut]
	}
	if group == "regular" {
		switch {
		case hasDuration && duration == sessionWindowSeconds:
			return canonical{key: client.WindowSession, title: "Session"}
		case hasDuration && duration == weeklyWindowSeconds:
			return canonical{key: client.WindowWeekly, title: "Weekly"}
		}
		return raw("codex", id)
	}
	model := strings.TrimPrefix(group, "additional/")
	switch {
	case hasDuration && duration == sessionWindowSeconds:
		return canonical{key: client.WindowModelSession, model: model, title: "Session (" + model + ")"}
	case hasDuration && duration == weeklyWindowSeconds:
		return canonical{key: client.WindowModelWeekly, model: model, title: "Weekly (" + model + ")"}
	}
	return raw("codex", id)
}

// mapXAI. Grok bills a shared consumable credit pool rather than a rate window,
// which is what the credits key exists to distinguish.
func mapXAI(id string) canonical {
	if id == "shared" {
		return canonical{key: client.WindowCredits, title: "Credits"}
	}
	return raw("xai", id)
}

func mapWindow(provider, id string, w client.Window) canonical {
	switch provider {
	case "claude":
		return mapClaude(id)
	case "codex":
		duration := int64(0)
		if w.DurationSeconds != nil {
			duration = *w.DurationSeconds
		}
		return mapCodex(id, duration, w.DurationSeconds != nil)
	case "xai":
		return mapXAI(id)
	}
	return raw(provider, id)
}

// primaryKeyOf reports which canonical window a provider's TOP-LEVEL
// observation corresponds to. Claude and Codex report a weekly allowance there;
// Grok's headline figure is its shared credit pool, which is a consumable
// balance rather than a rate window. Synthesis is keyed off this so a provider
// never gains a window it does not actually have.
func primaryKeyOf(provider string) string {
	if provider == "xai" {
		return client.WindowCredits
	}
	return client.WindowWeekly
}

func titleOf(key string) string {
	switch key {
	case client.WindowCredits:
		return "Credits"
	case client.WindowWeekly:
		return "Weekly"
	}
	return key
}

// derived pairs a canonical window with the upstream id it came from. The id is
// kept only to break ordering ties; it never reaches the wire.
type derived struct {
	window client.EntryWindow
	source string
}

func identityOf(w client.EntryWindow) string { return w.Key + "\x00" + w.Model }

// canonicalWindows projects the collected per-provider windows into the
// canonical vocabulary.
//
// A window without a usable percentage is skipped: EntryWindow.UsedPercent is
// not a pointer, so emitting one would publish 0% used, which reads as full
// remaining capacity and is a silent lie. Nothing is lost — the verbatim window
// remains under Entry.Quota.
//
// If the provider's primary observation succeeded but produced no canonical
// window of its own key, one is synthesized from the top-level fields. That
// covers both a provider that yields nothing extended and the subtler case
// where the extended parser rejected a percentage the primary parser accepted:
// without it a credential looks fully observed yet silently vanishes from the
// row its top-level figures describe.
func canonicalWindows(provider string, q *client.Quota, primary Observation) []client.EntryWindow {
	// Iterate in sorted id order, not map order: the result must be identical
	// for identical input, or an unchanged poll rewrites the snapshot.
	ids := make([]string, 0, len(q.Windows))
	for id := range q.Windows {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	items := make([]derived, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		w := q.Windows[id]
		if w.UsedPercent == nil {
			continue
		}
		mapped := mapWindow(provider, id, w)
		window := client.EntryWindow{
			Key:   mapped.key,
			Title: mapped.title,
			Model: mapped.model,
			// Clamped to match Entry.Percent's convention. The unclamped value
			// stays available under Entry.Quota for callers that want it.
			UsedPercent: clampPercent(*w.UsedPercent),
			ObservedAt:  q.ObservedAt,
		}
		// Two upstream windows can reduce to one canonical identity — Codex
		// declares the same duration in both slots of a group, for instance.
		// The later one is demoted to raw: rather than emitted twice, because a
		// consumer grouping by key would otherwise count one credential twice
		// and skew the row it belongs to.
		if seen[identityOf(window)] {
			fallback := raw(provider, id)
			window.Key, window.Title, window.Model = fallback.key, fallback.title, fallback.model
		}
		seen[identityOf(window)] = true
		if w.ResetsAt != nil {
			window.ResetAt = *w.ResetsAt
		}
		items = append(items, derived{window: window, source: id})
	}

	if key := primaryKeyOf(provider); !primary.ObservedAt.IsZero() && !seen[key+"\x00"] {
		items = append(items, derived{window: client.EntryWindow{
			Key:         key,
			Title:       titleOf(key),
			UsedPercent: clampPercent(primary.Percent),
			ResetAt:     primary.ResetAt,
			ObservedAt:  primary.ObservedAt,
		}})
	}

	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if ra, rb := order(a.window.Key), order(b.window.Key); ra != rb {
			return ra < rb
		}
		if a.window.Key != b.window.Key {
			return a.window.Key < b.window.Key
		}
		if a.window.Model != b.window.Model {
			return a.window.Model < b.window.Model
		}
		// Total order: without this last tie-break two windows sharing a key
		// and model compare equal in both directions and sort.Slice leaves
		// their order up to the randomized map iteration that produced them.
		return a.source < b.source
	})
	windows := make([]client.EntryWindow, 0, len(items))
	for _, item := range items {
		windows = append(windows, item.window)
	}
	return windows
}

// identity reads the subscription fields the dashboard shows beside a
// credential. Nothing here costs a request: every value comes from the response
// already fetched for quota.
//
// Codex has no renewal field in its usage payload; the renewal instant the CLI
// displays is the spend-control limit's reset, which is already collected.
func identity(provider string, q *client.Quota) (plan, tier string, renewal time.Time) {
	plan, tier = q.Plan, q.TierName
	if provider == "codex" {
		if balance, ok := q.Balances["spend_control"]; ok && balance.ResetsAt != nil {
			renewal = *balance.ResetsAt
		}
	}
	return plan, tier, renewal
}
