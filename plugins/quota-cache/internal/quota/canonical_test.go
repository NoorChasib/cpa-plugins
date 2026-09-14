package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

func keysOf(windows []client.EntryWindow) []string {
	out := make([]string, 0, len(windows))
	for _, w := range windows {
		out = append(out, w.Key)
	}
	return out
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestClaudeCanonicalWindowsAndSubscription(t *testing.T) {
	o := fetchDetails(t, "claude", `{"subscription":{"plan":"Max","tierName":"max_20x"},`+
		`"five_hour":{"utilization":31,"resets_at":"2026-09-14T13:15:00Z"},`+
		`"seven_day":{"utilization":76,"resets_at":"2026-09-17T03:00:00Z"},`+
		`"seven_day_opus":{"utilization":100,"resets_at":"2026-09-17T02:59:00Z"},`+
		`"seven_day_sonnet":{"utilization":125},`+
		`"seven_day_cowork":{"utilization":5},`+
		`"seven_day_oauth_apps":{"utilization":null}}`)

	want := []string{client.WindowSession, client.WindowWeekly, client.WindowWeeklyFable,
		client.WindowModelWeekly, "raw:claude:seven_day_cowork"}
	if got := keysOf(o.Windows); !equal(got, want) {
		t.Fatalf("keys=%v want %v", got, want)
	}
	// Entry-level fields keep their existing meaning: the regular weekly window.
	if o.Percent != 76 || !o.ResetAt.Equal(time.Date(2026, 9, 17, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("primary observation changed: %+v", o)
	}
	if o.Windows[1].UsedPercent != 76 || !o.Windows[1].ResetAt.Equal(o.ResetAt) {
		t.Fatalf("weekly window does not mirror the top-level fields: %+v", o.Windows[1])
	}
	if o.Windows[0].UsedPercent != 31 || o.Windows[2].UsedPercent != 100 {
		t.Fatalf("session/fable percentages wrong: %+v", o.Windows)
	}
	// Extended percentages preserve values above 100; the canonical surface
	// declares 0-100, so it clamps exactly like Entry.Percent.
	if o.Windows[3].UsedPercent != 100 || o.Windows[3].Model != "sonnet" {
		t.Fatalf("model window not clamped or unlabelled: %+v", o.Windows[3])
	}
	// A window with no usable percentage is not published as 0% used.
	for _, w := range o.Windows {
		if w.Key == "raw:claude:seven_day_oauth_apps" {
			t.Fatal("window without a percentage was emitted as zero used")
		}
		if !w.ObservedAt.Equal(now) {
			t.Fatalf("window %s has no observation time: %+v", w.Key, w)
		}
	}
	if o.Plan != "Max" || o.TierName != "max_20x" {
		t.Fatalf("subscription identity not collected: plan=%q tier=%q", o.Plan, o.TierName)
	}
}

// The shape Anthropic actually serves now: the flat seven_day_* keys are still
// present but null, and live quota lives in a structured limits[] array. A
// reader that only knows the flat keys sees a session and a weekly and loses
// every model-scoped allowance, Fable included.
func TestClaudeReadsTheStructuredLimitsArray(t *testing.T) {
	o := fetchDetails(t, "claude", `{"subscription":{"plan":"Max"},`+
		`"five_hour":null,"seven_day":null,"seven_day_opus":null,"seven_day_sonnet":null,`+
		`"limits":[`+
		`{"kind":"session","percent":31,"resets_at":"2026-09-14T13:15:00Z"},`+
		`{"kind":"weekly_all","percent":76,"resets_at":"2026-09-17T03:00:00Z"},`+
		`{"kind":"weekly_scoped","percent":88,"resets_at":"2026-09-17T02:59:00Z",`+
		`"scope":{"model":{"display_name":"Fable"}}},`+
		`{"kind":"weekly_scoped","percent":40,"resets_at":"2026-09-17T02:58:00Z",`+
		`"scope":{"model":{"display_name":"Sonnet"}}}]}`)

	want := []string{client.WindowSession, client.WindowWeekly, client.WindowWeeklyFable, client.WindowModelWeekly}
	if got := keysOf(o.Windows); !equal(got, want) {
		t.Fatalf("keys=%v want %v", got, want)
	}
	if o.Windows[2].UsedPercent != 88 || o.Windows[2].Title != "Weekly (Fable)" {
		t.Fatalf("the Fable allowance was not read: %+v", o.Windows[2])
	}
	// A scope Anthropic adds later needs no release here: it is keyed by the
	// model it names rather than by a list.
	if o.Windows[3].Model != "Sonnet" || o.Windows[3].UsedPercent != 40 {
		t.Fatalf("scoped window not carried by model: %+v", o.Windows[3])
	}
	// The entry-level projection still follows the account-wide weekly.
	if o.Percent != 76 || !o.ResetAt.Equal(time.Date(2026, 9, 17, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("primary observation not taken from the array: %+v", o)
	}
}

// Both shapes at once, which is what a rollout looks like from the outside.
// Whichever carries data wins, and neither is published twice.
func TestClaudeFlatKeysAndLimitsArrayDoNotDuplicate(t *testing.T) {
	o := fetchDetails(t, "claude", `{`+
		`"five_hour":{"utilization":31,"resets_at":"2026-09-14T13:15:00Z"},`+
		`"seven_day":{"utilization":76,"resets_at":"2026-09-17T03:00:00Z"},`+
		`"seven_day_opus":null,`+
		`"limits":[{"kind":"weekly_scoped","percent":88,"resets_at":"2026-09-17T02:59:00Z",`+
		`"scope":{"model":{"display_name":"Fable"}}}]}`)

	want := []string{client.WindowSession, client.WindowWeekly, client.WindowWeeklyFable}
	if got := keysOf(o.Windows); !equal(got, want) {
		t.Fatalf("keys=%v want %v", got, want)
	}
	seen := map[string]int{}
	for _, w := range o.Windows {
		seen[w.Key]++
	}
	for key, count := range seen {
		if count > 1 {
			t.Fatalf("%s emitted %d times; one credential would be counted twice in its row", key, count)
		}
	}
}

// The tier stopped appearing in the usage response for some accounts, which
// left the dashboard showing an empty plan badge. The stored credential already
// records it and has already been read, so it costs nothing to fall back to.
func TestClaudePlanFallsBackToTheStoredCredential(t *testing.T) {
	d := &fakeDoer{response: protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"seven_day":{"utilization":10}}`)}}
	o, err := Fetch(context.Background(), d, "claude",
		[]byte(`{"access_token":"synthetic-secret","subscriptionType":"max"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if o.Plan != "max" {
		t.Fatalf("plan = %q; the credential records the tier when the response does not", o.Plan)
	}

	// The response still wins when it has one.
	d = &fakeDoer{response: protocol.HostHTTPResponse{StatusCode: 200,
		Body: []byte(`{"subscription":{"plan":"Max"},"seven_day":{"utilization":10}}`)}}
	o, err = Fetch(context.Background(), d, "claude",
		[]byte(`{"access_token":"synthetic-secret","subscriptionType":"team"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if o.Plan != "Max" {
		t.Fatalf("plan = %q; the response is authoritative where it carries one", o.Plan)
	}
}

// Anthropic buckets this endpoint by User-Agent. A caller that does not
// identify as Claude Code collects 429s after a handful of polls, which shows
// up as a credential permanently in backoff rather than as an obvious error.
func TestClaudeUsageRequestIdentifiesItself(t *testing.T) {
	d := &fakeDoer{response: protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"seven_day":{"utilization":10}}`)}}
	if _, err := Fetch(context.Background(), d, "claude", []byte(`{"access_token":"synthetic-secret"}`), now); err != nil {
		t.Fatal(err)
	}
	agent := ""
	if values := d.request.Headers["User-Agent"]; len(values) > 0 {
		agent = values[0]
	}
	if !strings.HasPrefix(agent, "claude-code/") {
		t.Fatalf("User-Agent = %q; Anthropic rate-limits anything that is not Claude Code far harder", agent)
	}
}

func TestCodexCanonicalWindowsUseDurationNotSlotAndRenewal(t *testing.T) {
	o := fetchDetails(t, "codex", `{"plan_type":"pro",`+
		// Deliberately inverted: the five-hour limit is in the "primary" slot
		// and the weekly in "secondary". Mapping must follow the duration.
		`"rate_limit":{"primary_window":{"used_percent":40,"limit_window_seconds":18000},`+
		`"secondary_window":{"used_percent":25,"limit_window_seconds":604800}},`+
		`"additional_rate_limits":[{"limit_name":"spark","rate_limit":{"primary_window":{"used_percent":9,"limit_window_seconds":18000}}}],`+
		`"spend_control":{"individual_limit":{"limit":"50.00","used":"10.00","reset_after_seconds":2592000}}}`)

	want := []string{client.WindowSession, client.WindowWeekly, client.WindowModelSession}
	if got := keysOf(o.Windows); !equal(got, want) {
		t.Fatalf("keys=%v want %v", got, want)
	}
	if o.Windows[0].UsedPercent != 40 || o.Windows[1].UsedPercent != 25 {
		t.Fatalf("slot/duration mapping wrong: %+v", o.Windows)
	}
	if o.Windows[2].Model != "spark" {
		t.Fatalf("additional limit lost its name: %+v", o.Windows[2])
	}
	if o.Plan != "pro" {
		t.Fatalf("plan=%q", o.Plan)
	}
	// Codex's usage payload has no renewal field; the renewal instant it shows
	// is the spend-control limit reset, which is already collected.
	if !o.RenewalAt.Equal(now.Add(30 * 24 * time.Hour)) {
		t.Fatalf("renewal=%s", o.RenewalAt)
	}
}

func TestCodexShortWindowOnlyAccountInventsNoWeekly(t *testing.T) {
	o := fetchDetails(t, "codex", `{"rate_limit":{"primary_window":{"used_percent":100,"limit_window_seconds":18000}},`+
		`"code_review_rate_limit":{"primary_window":{"used_percent":4,"limit_window_seconds":604800}},`+
		`"additional_rate_limits":[{"limit_name":"spark","rate_limit":{"secondary_window":{"used_percent":12,"limit_window_seconds":604800}}}]}`)

	if !o.ObservedAt.IsZero() {
		t.Fatal("account without a weekly window reported a weekly observation")
	}
	want := []string{client.WindowSession, client.WindowModelWeekly, client.WindowModelWeekly}
	if got := keysOf(o.Windows); !equal(got, want) {
		t.Fatalf("keys=%v want %v", got, want)
	}
	// Per-model windows are ordered by model so the snapshot is stable.
	if o.Windows[1].Model != "code_review" || o.Windows[2].Model != "spark" {
		t.Fatalf("model ordering unstable: %+v", o.Windows)
	}
	for _, w := range o.Windows {
		if w.Key == client.WindowWeekly {
			t.Fatal("synthesized a weekly window from a failed primary observation")
		}
	}
}

func TestXAISharedPoolIsCreditsAndProductsAreRaw(t *testing.T) {
	o := fetchDetails(t, "xai", `{"subscriptionTier":"SuperGrok Heavy","config":{"creditUsagePercent":28,`+
		`"currentPeriod":{"type":"USAGE_PERIOD_TYPE_MONTHLY","start":"2026-09-01T00:00:00Z","end":"2026-10-01T00:00:00Z"},`+
		`"productUsage":[{"product":"BUILD","usagePercent":22}]}}`)

	want := []string{client.WindowCredits, "raw:xai:product/BUILD"}
	if got := keysOf(o.Windows); !equal(got, want) {
		t.Fatalf("keys=%v want %v", got, want)
	}
	if o.Windows[0].UsedPercent != 28 || o.Plan != "SuperGrok Heavy" {
		t.Fatalf("observation=%+v", o)
	}
	// raw: windows keep the upstream identifier as their title so a consumer
	// can render one it does not understand.
	if o.Windows[1].Title != "product/BUILD" {
		t.Fatalf("raw window lost its upstream label: %+v", o.Windows[1])
	}
}

// A provider response that yields no extended windows still produces the window
// its top-level fields describe, so every credential has a weekly to sort by.
func TestWeeklyIsSynthesizedWhenNoExtendedWindowsSurvive(t *testing.T) {
	o := fetchDetails(t, "claude", `{"seven_day":{"utilization":-1}}`)
	if len(o.Windows) != 1 {
		t.Fatalf("windows=%+v", o.Windows)
	}
	w := o.Windows[0]
	if w.Key != client.WindowWeekly || w.UsedPercent != 0 || !w.ObservedAt.Equal(now) {
		t.Fatalf("synthesized window=%+v", w)
	}
	if o.Percent != 0 {
		t.Fatalf("negative percentage not clamped at the entry level: %v", o.Percent)
	}
}

// The canonical percentage is USED, not remaining. Shipping this inverted fails
// silently and plausibly, so it is asserted by name against the handoff's
// worked example: 76 used is 24 left.
func TestCanonicalUsedPercentIs76NotRemaining24(t *testing.T) {
	o := fetchDetails(t, "claude", `{"seven_day":{"utilization":76,"resets_at":"2026-09-17T03:00:00Z"}}`)
	weekly := o.Windows[0]
	if weekly.Key != client.WindowWeekly {
		t.Fatalf("expected a weekly window, got %+v", o.Windows)
	}
	if weekly.UsedPercent != 76 {
		t.Fatalf("UsedPercent=%v; it must carry USED capacity (76), not remaining (24)", weekly.UsedPercent)
	}
	if remaining := 100 - weekly.UsedPercent; remaining != 24 {
		t.Fatalf("100-used=%v want 24", remaining)
	}
}

// The extended parser rejects a negative percentage while the primary parser
// clamps it and succeeds. Before this was fixed, a surviving unrelated window
// suppressed synthesis, so the credential reported a fresh observation with no
// weekly window at all and silently dropped out of the weekly row.
func TestWeeklyIsSynthesizedWhenOnlyTheWeeklyWindowWasRejected(t *testing.T) {
	o := fetchDetails(t, "claude", `{"five_hour":{"utilization":31,"resets_at":"2026-09-04T23:00:00Z"},`+
		`"seven_day":{"utilization":-1,"resets_at":"2026-09-10T03:00:00Z"}}`)

	want := []string{client.WindowSession, client.WindowWeekly}
	if got := keysOf(o.Windows); !equal(got, want) {
		t.Fatalf("keys=%v want %v; a rejected weekly percentage must still yield a weekly window", got, want)
	}
	weekly := o.Windows[1]
	if weekly.UsedPercent != 0 || !weekly.ObservedAt.Equal(now) {
		t.Fatalf("synthesized weekly=%+v", weekly)
	}
	if !weekly.ResetAt.Equal(o.ResetAt) {
		t.Fatalf("synthesized weekly must mirror the top-level reset: %s vs %s", weekly.ResetAt, o.ResetAt)
	}
}

// Grok's headline figure is a consumable credit pool, not a rate window, so it
// must never be presented as a weekly allowance. Synthesis is keyed off the
// provider's own primary window rather than assuming weekly for everyone.
func TestXAIKeepsCreditsAndNeverGainsASpuriousWeekly(t *testing.T) {
	o := fetchDetails(t, "xai", `{"config":{"creditUsagePercent":28,`+
		`"currentPeriod":{"end":"2026-10-01T00:00:00Z"},"productUsage":[{"product":"BUILD","usagePercent":22}]}}`)
	for _, w := range o.Windows {
		if w.Key == client.WindowWeekly {
			t.Fatalf("xAI gained a weekly window it does not have: %+v", o.Windows)
		}
	}

	// And when the pool's own percentage is rejected, credits is synthesized
	// from the primary rather than the credential losing its only row.
	o = fetchDetails(t, "xai", `{"config":{"creditUsagePercent":-5,`+
		`"currentPeriod":{"end":"2026-10-01T00:00:00Z"},"productUsage":[{"product":"BUILD","usagePercent":22}]}}`)
	want := []string{client.WindowCredits, "raw:xai:product/BUILD"}
	if got := keysOf(o.Windows); !equal(got, want) {
		t.Fatalf("keys=%v want %v", got, want)
	}
}

// Two upstream windows can reduce to one canonical identity. Emitting both
// would let a consumer grouping by key count one credential twice and skew the
// row's mean, so the later one is demoted rather than duplicated.
func TestCollidingCanonicalIdentitiesAreDemotedNotDuplicated(t *testing.T) {
	// Both Codex slots of the same group declare a weekly duration.
	o := fetchDetails(t, "codex", `{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":604800},`+
		`"secondary_window":{"used_percent":90,"limit_window_seconds":604800}}}`)
	weekly := 0
	for _, w := range o.Windows {
		if w.Key == client.WindowWeekly {
			weekly++
			if w.UsedPercent != o.Percent {
				t.Fatalf("the surviving weekly (%v) disagrees with the entry (%v)", w.UsedPercent, o.Percent)
			}
		}
	}
	if weekly != 1 {
		t.Fatalf("%d weekly windows emitted for one credential: %+v", weekly, o.Windows)
	}
	// Nothing is lost: the demoted window is still present, generically.
	if got := keysOf(o.Windows); !equal(got, []string{client.WindowWeekly, "raw:codex:regular/secondary"}) {
		t.Fatalf("keys=%v", got)
	}
}

// The snapshot is rewritten on every poll. Identical input must produce
// byte-identical output, or an unchanged poll churns the file and every
// downstream consumer sees a spurious change.
func TestRepeatedParsesOfOneResponseAreByteIdentical(t *testing.T) {
	body := `{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000},` +
		`"secondary_window":{"used_percent":2,"limit_window_seconds":604800}},` +
		`"code_review_rate_limit":{"primary_window":{"used_percent":4,"limit_window_seconds":18000}},` +
		`"additional_rate_limits":[{"limit_name":"code_review",` +
		`"rate_limit":{"primary_window":{"used_percent":77,"limit_window_seconds":18000}}}]}`
	first, _ := json.Marshal(fetchDetails(t, "codex", body).Windows)
	for i := 0; i < 40; i++ {
		again, _ := json.Marshal(fetchDetails(t, "codex", body).Windows)
		if !bytes.Equal(first, again) {
			t.Fatalf("parse %d differs; map iteration order is reaching the output\nfirst: %s\nagain: %s", i, first, again)
		}
	}
}

// renewal_at is absent unless the provider actually supplies one. A value-typed
// time.Time would write a year-1 date into every entry, which a client
// converting to epoch seconds renders as a date in 1 BC.
func TestRenewalAtIsOmittedWhenUnknown(t *testing.T) {
	o := fetchDetails(t, "claude", `{"seven_day":{"utilization":10,"resets_at":"2026-09-10T03:00:00Z"}}`)
	if !o.RenewalAt.IsZero() {
		t.Fatalf("claude reported a renewal instant: %s", o.RenewalAt)
	}
	raw, err := json.Marshal(client.Entry{Provider: "claude", AuthIndex: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("renewal_at")) {
		t.Fatalf("an unknown renewal instant was serialized: %s", raw)
	}
}

// Real plan labels contain parentheses and plus signs; the bounded allowlist
// used to drop them silently, leaving the dashboard with a blank plan.
func TestPlanLabelsWithPunctuationSurvive(t *testing.T) {
	for _, want := range []string{"Pro (20x)", "Team+", "Max", "Pro 20x"} {
		o := fetchDetails(t, "claude", `{"subscription":{"plan":"`+want+`"},"seven_day":{"utilization":5}}`)
		if o.Plan != want {
			t.Fatalf("plan %q was dropped (got %q)", want, o.Plan)
		}
	}
	// The allowlist still rejects anything with control characters.
	o := fetchDetails(t, "claude", `{"subscription":{"plan":"bad\nvalue"},"seven_day":{"utilization":5}}`)
	if o.Plan != "" {
		t.Fatalf("plan=%q; a malformed label must be dropped", o.Plan)
	}
}
