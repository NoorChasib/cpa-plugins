package aggregate

import (
	"bytes"
	"encoding/json"
	"flag"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

var update = flag.Bool("update", false, "rewrite testdata/golden/summary.json")

// fixtureNow is the instant the committed golden document is built at. The web
// app develops against that file, so this constant is part of the contract.
const fixtureNow = 1789012800 // 2026-09-10T04:00:00Z

func at(t *testing.T, offset int64) time.Time {
	t.Helper()
	return time.Unix(fixtureNow+offset, 0).UTC()
}

func loadSnapshot(t *testing.T, name string) qc.Snapshot {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "snapshots", name)
	snapshot, err := qc.Load(path)
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return snapshot
}

// ringOf builds a recent-request ring the way CPA reports one: a fixed number
// of equal buckets, oldest first, each labelled with the local-time range it
// covers. The maps are sparse and indexed the same way, so a fixture names only
// the buckets that saw traffic.
func ringOf(count int, width time.Duration, success, failed map[int]int64) []RecentRequest {
	out := make([]RecentRequest, 0, count)
	for i := 0; i < count; i++ {
		start := time.Unix(fixtureNow, 0).UTC().Add(time.Duration(i-(count-1)) * width)
		out = append(out, RecentRequest{
			Label:   start.Format("15:04") + "-" + start.Add(width).Format("15:04"),
			Success: success[i],
			Failed:  failed[i],
		})
	}
	return out
}

// tenMinuteRing is CPA's own shape: twenty buckets of ten minutes, the last
// 3h20m. Every fixture below uses it except the one that deliberately does not.
func tenMinuteRing(success, failed map[int]int64) []RecentRequest {
	return ringOf(20, 10*time.Minute, success, failed)
}

// fixtureRoster is what host.auth.list reports. Order is deliberately not the
// display order: the server sorts by weekly reset, and a roster that arrived
// pre-sorted would hide a failure to do so.
//
// The rings are the pool as it actually behaves: one credential carrying the
// traffic right now, one that carried it two hours ago, one that failed a
// couple of requests, two that nothing has routed to all window, and one host
// entry with no ring at all — an older CPA, which reports no counter.
func fixtureRoster() []Identity {
	return []Identity{
		{AuthIndex: "xai-noor@example.com.json", Provider: "xai"},
		{AuthIndex: "claude-chasibnoor@example.com.json", Provider: "claude",
			Recent: tenMinuteRing(map[int]int64{3: 6, 4: 11, 5: 8, 6: 4, 7: 2}, nil)},
		{AuthIndex: "claude-noorchasib@example.com.json", Provider: "claude",
			Recent: tenMinuteRing(nil, nil)},
		{AuthIndex: "codex-noor@example.com.json", Provider: "codex",
			Recent: tenMinuteRing(map[int]int64{18: 4, 19: 3}, nil)},
		{AuthIndex: "claude-siphorchannel@example.com.json", Provider: "claude",
			Recent: tenMinuteRing(map[int]int64{16: 1}, map[int]int64{17: 2})},
		{AuthIndex: "claude-noor@example.com.json", Provider: "claude",
			Recent: tenMinuteRing(nil, nil)},
		{AuthIndex: "claude-agency@example.com.json", Provider: "claude",
			Recent: tenMinuteRing(map[int]int64{
				10: 2, 11: 5, 12: 9, 13: 12, 14: 8, 15: 15, 16: 17, 17: 14, 18: 21, 19: 14,
			}, nil)},
	}
}

func buildFixture(t *testing.T) Document {
	t.Helper()
	return Build(Input{
		Snapshot:   loadSnapshot(t, "seven-credentials.json"),
		Identities: fixtureRoster(),
		StaleAfter: 45 * time.Minute,
	}, at(t, 0))
}

func rowOf(t *testing.T, doc Document, provider, rowID string) Row {
	t.Helper()
	for _, p := range doc.Providers {
		if p.ID != provider {
			continue
		}
		for _, r := range p.Rows {
			if r.RowID == rowID {
				return r
			}
		}
	}
	t.Fatalf("provider %s has no row %s", provider, rowID)
	return Row{}
}

// The named inversion test. quota-cache reports USED capacity; this document
// reports REMAINING. Shipping it backwards produces numbers that look entirely
// plausible on a dashboard, so it is asserted directly against the worked
// example in the handoff: 76 used renders as 24% left.
func TestUsedPercent76RendersAs24PercentLeft(t *testing.T) {
	remaining, issue := remainingOf(76)
	if issue != "" {
		t.Fatalf("unexpected issue %q", issue)
	}
	if remaining != 0.24 {
		t.Fatalf("remainingOf(76) = %v; want 0.24 (24%% left, not 76%%)", remaining)
	}
	if got := percentOf(remaining); got != 24 {
		t.Fatalf("percentOf = %d; want 24", got)
	}

	// And end to end: the credential whose weekly window is 76 used must show
	// 24% left in the document the dashboard actually receives.
	doc := buildFixture(t)
	weekly := rowOf(t, doc, "claude", qc.WindowWeekly)
	for _, entry := range weekly.Entries {
		if entry.CredentialID != "claude-chasibnoor@example.com.json" {
			continue
		}
		if entry.RemainingPercent != 24 || entry.RemainingFraction != 0.24 {
			t.Fatalf("76 used rendered as %d%% (%v); want 24%%", entry.RemainingPercent, entry.RemainingFraction)
		}
		return
	}
	t.Fatal("weekly row is missing the credential at 76 used")
}

// The acceptance case from the handoff, asserted on the document rather than
// only through curl.
func TestSessionRowMatchesTheDesign(t *testing.T) {
	doc := buildFixture(t)
	session := rowOf(t, doc, "claude", qc.WindowSession)

	if session.Aggregate.RemainingPercent != 94 {
		t.Fatalf("session = %d%%; want 94", session.Aggregate.RemainingPercent)
	}
	if want := "+6% when siphorchannel resets in 1h 15m"; session.Aggregate.Subtext != want {
		t.Fatalf("subtext = %q; want %q", session.Aggregate.Subtext, want)
	}
	if len(session.Entries) != 5 {
		t.Fatalf("session has %d entries; want 5", len(session.Entries))
	}
	if session.Aggregate.MemberCount != 5 || session.Aggregate.ExcludedCount != 0 {
		t.Fatalf("membership = %+v", session.Aggregate)
	}
	// Every row repeats the catalog order, so the top row of each card is
	// always the credential that recovers next.
	order := []string{}
	for _, c := range doc.Credentials {
		if c.Provider == "claude" {
			order = append(order, c.ID)
		}
	}
	want := []string{
		"claude-siphorchannel@example.com.json",
		"claude-agency@example.com.json",
		"claude-chasibnoor@example.com.json",
		"claude-noor@example.com.json",
		"claude-noorchasib@example.com.json",
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("credential order = %v; want %v", order, want)
		}
		if session.Entries[i].CredentialID != want[i] {
			t.Fatalf("session entry %d = %s; want %s", i, session.Entries[i].CredentialID, want[i])
		}
	}
	if doc.Counters != (Counters{Credentials: 7, ObservedOK: 6, ObserveError: 1}) {
		t.Fatalf("counters = %+v", doc.Counters)
	}
	if doc.Credentials[0].Email != "siphorchannel@example.com" || doc.Credentials[0].Plan != "Max" {
		t.Fatalf("identity = %+v", doc.Credentials[0])
	}
}

// The golden document is the contract between this half and the web app. It is
// committed, and it is byte-identical to what the summary route serves.
// A credential stuck in backoff must not hold the header at "next attempt due"
// while everything else keeps polling on schedule. The reader is being told
// when this page can next change, and that is the soonest attempt still ahead.
func TestNextAttemptIgnoresOneStuckCredential(t *testing.T) {
	now := at(t, 0)
	observed := now.Add(-5 * time.Minute)
	snapshot := qc.Snapshot{Entries: map[string]qc.Entry{
		qc.Key("claude", "claude-healthy@example.com.json"): {
			ObservedAt: observed, NextAttempt: now.Add(10 * time.Minute),
			Windows: []qc.EntryWindow{{Key: qc.WindowWeekly, UsedPercent: 10, ObservedAt: observed, ResetAt: now.Add(time.Hour)}},
		},
		// Overdue: quota-cache has not got back to it yet.
		qc.Key("claude", "claude-stuck@example.com.json"): {
			ObservedAt: observed, NextAttempt: now.Add(-2 * time.Hour), Failures: 3,
			Windows: []qc.EntryWindow{{Key: qc.WindowWeekly, UsedPercent: 50, ObservedAt: observed, ResetAt: now.Add(time.Hour)}},
		},
	}}
	identities := []Identity{
		{AuthIndex: "claude-healthy@example.com.json", Provider: "claude"},
		{AuthIndex: "claude-stuck@example.com.json", Provider: "claude"},
	}

	doc := Build(Input{Snapshot: snapshot, Identities: identities, StaleAfter: time.Hour}, now)

	if doc.NextAttemptEpoch == nil {
		t.Fatal("nextAttemptEpoch is null; the healthy credential has one scheduled")
	}
	if got := *doc.NextAttemptEpoch; got != now.Add(10*time.Minute).Unix() {
		t.Fatalf("nextAttemptEpoch = %d; want the soonest FUTURE attempt %d, not the stuck one",
			got, now.Add(10*time.Minute).Unix())
	}

	// With nothing scheduled ahead at all, the stalled poller must still be
	// reported rather than vanishing: the header reads "due", not blank.
	onlyStuck := qc.Snapshot{Entries: map[string]qc.Entry{
		qc.Key("claude", "claude-stuck@example.com.json"): snapshot.Entries[qc.Key("claude", "claude-stuck@example.com.json")],
	}}
	stalled := Build(Input{Snapshot: onlyStuck, Identities: identities[1:], StaleAfter: time.Hour}, now)
	if stalled.NextAttemptEpoch == nil || *stalled.NextAttemptEpoch != now.Add(-2*time.Hour).Unix() {
		t.Fatalf("a wholly stalled poller must still report an attempt, got %v", stalled.NextAttemptEpoch)
	}
}

func TestGoldenSummaryDocument(t *testing.T) {
	doc := buildFixture(t)
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	path := filepath.Join("..", "..", "testdata", "golden", "summary.json")
	if *update {
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("golden document rewritten")
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden document missing; regenerate with: make golden (%v)", err)
	}
	if !bytes.Equal(raw, want) {
		t.Fatalf("built document differs from testdata/golden/summary.json.\n"+
			"The web app develops against that file, so this is a contract change.\n"+
			"Review it, then regenerate with: make golden\n\ngot %d bytes, want %d bytes", len(raw), len(want))
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	first, second := buildFixture(t), buildFixture(t)
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if !bytes.Equal(a, b) {
		t.Fatal("two builds of the same input differ; map iteration is leaking into the output")
	}
}

func TestLevelThresholdIsServerSide(t *testing.T) {
	for _, tc := range []struct {
		used  float64
		level string
	}{{0, LevelOK}, {60, LevelOK}, {61, LevelLow}, {80, LevelLow}, {81, LevelCritical}, {100, LevelCritical}} {
		remaining, _ := remainingOf(tc.used)
		if got := levelOf(remaining); got != tc.level {
			t.Fatalf("used %v -> %s; want %s", tc.used, got, tc.level)
		}
	}
}

func TestHumanDurationMatchesTheDesign(t *testing.T) {
	for _, tc := range []struct {
		seconds int64
		want    string
	}{{4500, "1h 15m"}, {93600, "1d 2h"}, {158400, "1d 20h"}, {399600, "4d 15h"},
		{442800, "5d 3h"}, {3600, "1h"}, {2700, "45m"}, {172800, "2d"}, {30, "<1m"}, {-5, "<1m"}} {
		if got := humanDuration(time.Duration(tc.seconds) * time.Second); got != tc.want {
			t.Fatalf("%ds -> %q; want %q", tc.seconds, got, tc.want)
		}
	}
}

func TestAdversarialWindowValues(t *testing.T) {
	now := at(t, 0)
	observed := now.Add(-5 * time.Minute)
	entry := func(windows ...qc.EntryWindow) qc.Entry {
		return qc.Entry{
			Provider: "claude", AuthIndex: "claude-a@example.com.json",
			ObservedAt: observed, NextAttempt: now.Add(10 * time.Minute), Windows: windows,
		}
	}
	base := func(used float64, reset time.Time) qc.Snapshot {
		return qc.Snapshot{
			Schema: 1, ProviderCooldown: map[string]time.Time{},
			Entries: map[string]qc.Entry{
				"claude:claude-a@example.com.json": entry(qc.EntryWindow{
					Key: qc.WindowWeekly, UsedPercent: used, ResetAt: reset, ObservedAt: observed,
				}),
			},
		}
	}
	roster := []Identity{{AuthIndex: "claude-a@example.com.json", Provider: "claude"}}
	build := func(s qc.Snapshot) Row {
		doc := Build(Input{Snapshot: s, Identities: roster, StaleAfter: time.Hour}, now)
		return rowOf(t, doc, "claude", qc.WindowWeekly)
	}

	// An out-of-range percentage is clamped and flagged, never silently used.
	for _, tc := range []struct {
		name      string
		used      float64
		remaining float64
		issue     string
	}{
		{"negative", -1, 1, issuePercentOutOfRange},
		{"above 100", 101, 0, issuePercentOutOfRange},
		// No information at all reports no remaining capacity: overstating
		// headroom is the damaging direction.
		{"NaN", math.NaN(), 0, issuePercentInvalid},
		{"infinite", math.Inf(1), 0, issuePercentInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := build(base(tc.used, now.Add(time.Hour)))
			got := row.Entries[0]
			if got.RemainingFraction != tc.remaining {
				t.Fatalf("remaining = %v; want %v", got.RemainingFraction, tc.remaining)
			}
			if len(got.DataIssues) == 0 || got.DataIssues[0] != tc.issue {
				t.Fatalf("issues = %v; want %s", got.DataIssues, tc.issue)
			}
		})
	}

	t.Run("reset in the past", func(t *testing.T) {
		row := build(base(50, now.Add(-time.Minute)))
		got := row.Entries[0]
		if got.ResetDisplayHint != HintNone {
			t.Fatalf("hint = %q; a past reset must not start a countdown", got.ResetDisplayHint)
		}
		if got.ResetAtEpoch == nil || *got.ResetInSeconds != 0 {
			t.Fatalf("entry = %+v; the instant stays visible so the client can say resetting", got)
		}
		if row.Aggregate.SoonestResetAtEpoch != nil {
			t.Fatal("a past reset was offered as the next reset")
		}
	})

	t.Run("failed poll keeps last known figures", func(t *testing.T) {
		snapshot := base(50, now.Add(time.Hour))
		e := snapshot.Entries["claude:claude-a@example.com.json"]
		e.Failures, e.LastError = 2, "quota fetch failed"
		snapshot.Entries["claude:claude-a@example.com.json"] = e
		doc := Build(Input{Snapshot: snapshot, Identities: roster, StaleAfter: time.Hour}, now)
		if doc.Counters.ObserveError != 1 || doc.Credentials[0].Status != StatusError {
			t.Fatalf("counters=%+v status=%s", doc.Counters, doc.Credentials[0].Status)
		}
		row := rowOf(t, doc, "claude", qc.WindowWeekly)
		if row.Entries[0].RemainingPercent != 50 || row.Entries[0].State != StatusError {
			t.Fatalf("entry = %+v; last known data must stay visible, marked", row.Entries[0])
		}
	})

	t.Run("refresh pending is not an error", func(t *testing.T) {
		snapshot := base(50, now.Add(time.Hour))
		e := snapshot.Entries["claude:claude-a@example.com.json"]
		e.LastError = "refresh pending"
		snapshot.Entries["claude:claude-a@example.com.json"] = e
		doc := Build(Input{Snapshot: snapshot, Identities: roster, StaleAfter: time.Hour}, now)
		if doc.Credentials[0].Status != StatusOK || doc.Counters.ObserveError != 0 {
			t.Fatalf("a credential mid-refresh was reported as failed: %+v", doc.Credentials[0])
		}
	})
}

// Against a snapshot written before canonical windows existed, the weekly row
// is synthesized from the top-level fields, so the plugin is useful immediately
// and the remaining rows appear on their own once quota-cache supplies them.
func TestSnapshotWithoutWindowsStillProducesWeekly(t *testing.T) {
	now := at(t, 0)
	observed := now.Add(-5 * time.Minute)
	snapshot := qc.Snapshot{
		Schema: 1, ProviderCooldown: map[string]time.Time{},
		Entries: map[string]qc.Entry{
			"claude:claude-a@example.com.json": {
				Provider: "claude", AuthIndex: "claude-a@example.com.json",
				Percent: 76, ResetAt: now.Add(48 * time.Hour), ObservedAt: observed,
			},
		},
	}
	doc := Build(Input{
		Snapshot:   snapshot,
		Identities: []Identity{{AuthIndex: "claude-a@example.com.json", Provider: "claude"}},
		StaleAfter: time.Hour,
	}, now)
	row := rowOf(t, doc, "claude", qc.WindowWeekly)
	if row.Aggregate.RemainingPercent != 24 || len(row.Entries) != 1 {
		t.Fatalf("row = %+v", row.Aggregate)
	}
	if len(doc.Providers[0].Rows) != 1 {
		t.Fatalf("only weekly can be synthesized, got %d rows", len(doc.Providers[0].Rows))
	}
}

// A credential that did not report a window is excluded from the mean rather
// than counted as full: otherwise a silent credential quietly inflates the one
// number the whole card is read from.
func TestNonReportingCredentialIsExcludedNotCountedAsFull(t *testing.T) {
	now := at(t, 0)
	observed := now.Add(-5 * time.Minute)
	snapshot := qc.Snapshot{
		Schema: 1, ProviderCooldown: map[string]time.Time{},
		Entries: map[string]qc.Entry{
			"claude:claude-a@example.com.json": {
				Provider: "claude", AuthIndex: "claude-a@example.com.json", ObservedAt: observed,
				Windows: []qc.EntryWindow{{Key: qc.WindowWeekly, UsedPercent: 80, ResetAt: now.Add(time.Hour), ObservedAt: observed}},
			},
		},
	}
	roster := []Identity{
		{AuthIndex: "claude-a@example.com.json", Provider: "claude"},
		// In the roster, absent from the snapshot.
		{AuthIndex: "claude-b@example.com.json", Provider: "claude"},
	}
	doc := Build(Input{Snapshot: snapshot, Identities: roster, StaleAfter: time.Hour}, now)
	row := rowOf(t, doc, "claude", qc.WindowWeekly)
	if row.Aggregate.MemberCount != 1 || row.Aggregate.ExcludedCount != 1 {
		t.Fatalf("membership = %+v", row.Aggregate)
	}
	// One member at 20% left. Counting the silent credential as full would
	// report 60%.
	if row.Aggregate.RemainingPercent != 20 {
		t.Fatalf("aggregate = %d%%; want 20", row.Aggregate.RemainingPercent)
	}
	if doc.Credentials[1].Status != StatusUnsupported {
		t.Fatalf("status = %s", doc.Credentials[1].Status)
	}
}

// Disabled and unavailable are facts about routing, not about quota. The
// figures a parked credential last reported are still true, so it keeps its
// place on every card and in the mean; credentials[].status is where a client
// learns it cannot be routed to right now.
func TestDisabledCredentialStillCountsAndIsFlagged(t *testing.T) {
	now := at(t, 0)
	observed := now.Add(-5 * time.Minute)
	entry := qc.Entry{
		Provider: "claude", AuthIndex: "claude-b@example.com.json", ObservedAt: observed,
		Windows: []qc.EntryWindow{{Key: qc.WindowWeekly, UsedPercent: 0, ResetAt: now.Add(time.Hour), ObservedAt: observed}},
	}
	first := entry
	first.AuthIndex = "claude-a@example.com.json"
	first.Windows = []qc.EntryWindow{{Key: qc.WindowWeekly, UsedPercent: 50, ResetAt: now.Add(time.Hour), ObservedAt: observed}}
	snapshot := qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{
		"claude:claude-a@example.com.json": first,
		"claude:claude-b@example.com.json": entry,
	}}
	doc := Build(Input{Snapshot: snapshot, Identities: []Identity{
		{AuthIndex: "claude-a@example.com.json", Provider: "claude"},
		{AuthIndex: "claude-b@example.com.json", Provider: "claude", Disabled: true},
	}, StaleAfter: time.Hour}, now)

	row := rowOf(t, doc, "claude", qc.WindowWeekly)
	// 50% and 100% left: the mean covers both, and the card shows both.
	if row.Aggregate.MemberCount != 2 || row.Aggregate.RemainingPercent != 75 {
		t.Fatalf("a disabled credential was dropped from the mean: %+v", row.Aggregate)
	}
	if len(row.Entries) != 2 {
		t.Fatalf("row has %d entries; a disabled credential is still a row", len(row.Entries))
	}
	if len(doc.Credentials) != 2 {
		t.Fatal("a disabled credential must still be listed")
	}
	for _, c := range doc.Credentials {
		if c.ID == "claude-b@example.com.json" && c.Status != StatusDisabled {
			t.Fatalf("status = %s", c.Status)
		}
	}
}

// A window with no canonical meaning is still rendered, generically, and sorts
// after everything that has one.
func TestRawWindowIsKeptAndSortsLast(t *testing.T) {
	now := at(t, 0)
	observed := now.Add(-5 * time.Minute)
	snapshot := qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{
		"claude:claude-a@example.com.json": {
			Provider: "claude", AuthIndex: "claude-a@example.com.json", ObservedAt: observed,
			Windows: []qc.EntryWindow{
				{Key: "raw:claude:seven_day_cowork", Title: "seven_day_cowork", UsedPercent: 10, ObservedAt: observed},
				{Key: qc.WindowWeekly, UsedPercent: 40, ResetAt: now.Add(time.Hour), ObservedAt: observed},
			},
		},
	}}
	doc := Build(Input{Snapshot: snapshot,
		Identities: []Identity{{AuthIndex: "claude-a@example.com.json", Provider: "claude"}},
		StaleAfter: time.Hour}, now)
	rows := doc.Providers[0].Rows
	if len(rows) != 2 || rows[0].RowID != qc.WindowWeekly || rows[1].RowID != "raw:claude:seven_day_cowork" {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[1].Matched || rows[1].Title != "seven_day_cowork" {
		t.Fatalf("raw row = %+v; it must be marked unmatched and keep a label", rows[1])
	}
	if !rows[0].Matched {
		t.Fatal("a canonical row must be marked matched")
	}
}

func TestStaleReasons(t *testing.T) {
	now := at(t, 0)
	roster := []Identity{{AuthIndex: "claude-a@example.com.json", Provider: "claude"}}
	observed := now.Add(-2 * time.Hour)
	snapshot := qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{
		"claude:claude-a@example.com.json": {
			Provider: "claude", AuthIndex: "claude-a@example.com.json", ObservedAt: observed,
			Windows: []qc.EntryWindow{{Key: qc.WindowWeekly, UsedPercent: 10, ObservedAt: observed}},
		},
	}}

	// WrittenAt is not freshness: a snapshot rewritten during a cooldown is
	// fresh on disk and stale in substance, so staleness follows ObservedAt.
	snapshot.WrittenAt = now
	doc := Build(Input{Snapshot: snapshot, Identities: roster, StaleAfter: 45 * time.Minute}, now)
	if !doc.Stale || *doc.StaleReason != ReasonCacheStale {
		t.Fatalf("stale=%v reason=%v", doc.Stale, doc.StaleReason)
	}
	row := rowOf(t, doc, "claude", qc.WindowWeekly)
	if row.Entries[0].State != StateStale {
		t.Fatalf("entry state = %s", row.Entries[0].State)
	}

	// A schema bump in quota-cache blinds this plugin too, and must surface as
	// itself rather than as a generic read failure.
	doc = Build(Input{SourceReason: ReasonSchemaUnsupported, Identities: roster}, now)
	if !doc.Stale || *doc.StaleReason != ReasonSchemaUnsupported {
		t.Fatalf("reason = %v", doc.StaleReason)
	}

	doc = Build(Input{Snapshot: qc.Snapshot{Schema: 1, Entries: map[string]qc.Entry{}}, Identities: roster}, now)
	if !doc.Stale || *doc.StaleReason != ReasonNeverObserved {
		t.Fatalf("reason = %v", doc.StaleReason)
	}

	doc = buildFixture(t)
	if doc.Stale || doc.StaleReason != nil {
		t.Fatalf("fresh fixture reported stale: %v", doc.StaleReason)
	}
}

// The runtime passes time.Now(), which carries nanoseconds. Every value the
// document reports is in whole seconds, so a sub-second now used to make
// generatedAtEpoch + resetInSeconds disagree with resetAtEpoch, and round every
// countdown down — printing "1h 14m" for data that reads "1h 15m" on the second.
func TestSubSecondClockDoesNotSkewCountdowns(t *testing.T) {
	for _, offset := range []time.Duration{0, 1, 500 * time.Millisecond, 999999999} {
		now := at(t, 0).Add(offset)
		doc := Build(Input{
			Snapshot:   loadSnapshot(t, "seven-credentials.json"),
			Identities: fixtureRoster(),
			StaleAfter: 45 * time.Minute,
		}, now)
		session := rowOf(t, doc, "claude", qc.WindowSession)
		agg := session.Aggregate
		if got := doc.GeneratedAtEpoch + *agg.SoonestResetInSeconds; got != *agg.SoonestResetAtEpoch {
			t.Fatalf("offset %v: generatedAt+seconds = %d, resetAtEpoch = %d", offset, got, *agg.SoonestResetAtEpoch)
		}
		if want := "+6% when siphorchannel resets in 1h 15m"; agg.Subtext != want {
			t.Fatalf("offset %v: subtext = %q; want %q", offset, agg.Subtext, want)
		}
		for _, entry := range session.Entries {
			if entry.ResetAtEpoch == nil {
				continue
			}
			if got := doc.GeneratedAtEpoch + *entry.ResetInSeconds; got != *entry.ResetAtEpoch {
				t.Fatalf("offset %v: entry %s epoch mismatch", offset, entry.CredentialID)
			}
		}
	}
}

// The acceptance snapshot in the handoff carries no window titles at all, so a
// heading fell back to the raw row id. Card headings come from a server-side
// table when the writer supplies nothing.
func TestRowTitlesFallBackToCanonicalNames(t *testing.T) {
	now := at(t, 0)
	observed := now.Add(-2 * time.Minute)
	untitled := func(key, model string, used float64) qc.EntryWindow {
		return qc.EntryWindow{Key: key, Model: model, UsedPercent: used, ResetAt: now.Add(time.Hour), ObservedAt: observed}
	}
	snapshot := qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{
		"claude:claude-a@example.com.json": {
			Provider: "claude", AuthIndex: "claude-a@example.com.json", ObservedAt: observed,
			Windows: []qc.EntryWindow{
				untitled(qc.WindowSession, "", 31),
				untitled(qc.WindowWeekly, "", 76),
				untitled(qc.WindowWeeklyFable, "", 100),
				untitled(qc.WindowModelWeekly, "sonnet", 10),
				untitled("raw:claude:seven_day_cowork", "", 5),
			},
		},
	}}
	doc := Build(Input{Snapshot: snapshot,
		Identities: []Identity{{AuthIndex: "claude-a@example.com.json", Provider: "claude"}},
		StaleAfter: time.Hour}, now)
	want := []string{"Session", "Weekly", "Weekly (Fable)", "Weekly (sonnet)", "seven_day_cowork"}
	for i, row := range doc.Providers[0].Rows {
		if row.Title != want[i] {
			t.Fatalf("row %d title = %q; want %q", i, row.Title, want[i])
		}
	}
}

// A credential quota-cache knows about but has not polled yet is not "ok": it
// has no data, contributes to no row, and must not inflate observedOK.
func TestNeverObservedCredentialIsPendingNotOK(t *testing.T) {
	now := at(t, 0)
	snapshot := qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{
		"claude:claude-new@example.com.json": {
			Provider: "claude", AuthIndex: "claude-new@example.com.json",
			NextAttempt: now.Add(10 * time.Minute),
		},
	}}
	doc := Build(Input{Snapshot: snapshot,
		Identities: []Identity{{AuthIndex: "claude-new@example.com.json", Provider: "claude"}},
		StaleAfter: time.Hour}, now)
	if doc.Credentials[0].Status != StatusPending {
		t.Fatalf("status = %q; want %q", doc.Credentials[0].Status, StatusPending)
	}
	if doc.Counters.ObservedOK != 0 || doc.Counters.ObserveError != 0 {
		t.Fatalf("counters = %+v; a credential awaiting its first poll is neither", doc.Counters)
	}
}

// One credential contributes at most once to a row. Two windows collapsing to
// the same row id used to weight it twice and drive excludedCount negative.
func TestOneCredentialContributesOncePerRow(t *testing.T) {
	now := at(t, 0)
	observed := now.Add(-2 * time.Minute)
	snapshot := qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{
		"codex:codex-a@example.com.json": {
			Provider: "codex", AuthIndex: "codex-a@example.com.json", ObservedAt: observed,
			Windows: []qc.EntryWindow{
				{Key: qc.WindowWeekly, UsedPercent: 10, ResetAt: now.Add(time.Hour), ObservedAt: observed},
				{Key: qc.WindowWeekly, UsedPercent: 90, ResetAt: now.Add(time.Hour), ObservedAt: observed},
			},
		},
	}}
	doc := Build(Input{Snapshot: snapshot,
		Identities: []Identity{{AuthIndex: "codex-a@example.com.json", Provider: "codex"}},
		StaleAfter: time.Hour}, now)
	row := rowOf(t, doc, "codex", qc.WindowWeekly)
	if row.Aggregate.MemberCount != 1 || len(row.Entries) != 1 {
		t.Fatalf("credential counted %d times: %+v", row.Aggregate.MemberCount, row.Aggregate)
	}
	if row.Aggregate.ExcludedCount < 0 {
		t.Fatalf("excludedCount = %d; it must never be negative", row.Aggregate.ExcludedCount)
	}
	if row.Aggregate.RemainingPercent != 90 {
		t.Fatalf("aggregate = %d%%; the first window wins", row.Aggregate.RemainingPercent)
	}
}

// Trend compares the row now against the row an hour ago. Both means must cover
// the SAME members: taking the current mean over everyone and the historical
// mean over whoever happens to have samples compares two populations and
// reports movement where nothing moved.
func TestTrendComparesLikeForLike(t *testing.T) {
	now := at(t, 0)
	roster := fixtureRoster()
	snapshot := loadSnapshot(t, "seven-credentials.json")
	const siphor = "claude-siphorchannel@example.com.json"

	// History for exactly one of the five session members, unchanged at 0.69.
	partial := []Sample{
		{AuthIndex: siphor, WindowKey: qc.WindowSession, At: now.Add(-90 * time.Minute), Remaining: 0.69},
		{AuthIndex: siphor, WindowKey: qc.WindowSession, At: now.Add(-40 * time.Minute), Remaining: 0.69},
	}
	doc := Build(Input{Snapshot: snapshot, Identities: roster, Samples: partial, StaleAfter: time.Hour}, now)
	if got := rowOf(t, doc, "claude", qc.WindowSession).Aggregate.Trend; got != TrendFlat {
		t.Fatalf("trend = %q; nothing moved, and the one member with history is unchanged", got)
	}

	// Real movement in the member that has history is still detected.
	moved := []Sample{
		{AuthIndex: siphor, WindowKey: qc.WindowSession, At: now.Add(-90 * time.Minute), Remaining: 1.0},
		{AuthIndex: siphor, WindowKey: qc.WindowSession, At: now.Add(-40 * time.Minute), Remaining: 1.0},
	}
	doc = Build(Input{Snapshot: snapshot, Identities: roster, Samples: moved, StaleAfter: time.Hour}, now)
	if got := rowOf(t, doc, "claude", qc.WindowSession).Aggregate.Trend; got != TrendDown {
		t.Fatalf("trend = %q; the member with history fell from 1.0 to 0.69", got)
	}

	// Too little history stays unknown rather than guessing.
	short := []Sample{{AuthIndex: siphor, WindowKey: qc.WindowSession, At: now.Add(-5 * time.Minute), Remaining: 0.2}}
	doc = Build(Input{Snapshot: snapshot, Identities: roster, Samples: short, StaleAfter: time.Hour}, now)
	if got := rowOf(t, doc, "claude", qc.WindowSession).Aggregate.Trend; got != TrendUnknown {
		t.Fatalf("trend = %q; one sample is not a trend", got)
	}
}

// A snapshot entry with no matching credential in the host roster is ignored:
// the roster is authoritative for what exists. Asserted so the behaviour is
// deliberate rather than incidental.
func TestSnapshotEntryAbsentFromRosterIsIgnored(t *testing.T) {
	now := at(t, 0)
	observed := now.Add(-2 * time.Minute)
	window := []qc.EntryWindow{{Key: qc.WindowWeekly, UsedPercent: 50, ResetAt: now.Add(time.Hour), ObservedAt: observed}}
	snapshot := qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{
		"claude:claude-known@example.com.json": {
			Provider: "claude", AuthIndex: "claude-known@example.com.json", ObservedAt: observed, Windows: window,
		},
		"claude:claude-ghost@example.com.json": {
			Provider: "claude", AuthIndex: "claude-ghost@example.com.json", ObservedAt: observed, Windows: window,
		},
	}}
	doc := Build(Input{Snapshot: snapshot,
		Identities: []Identity{{AuthIndex: "claude-known@example.com.json", Provider: "claude"}},
		StaleAfter: time.Hour}, now)
	if len(doc.Credentials) != 1 || doc.Credentials[0].ID != "claude-known@example.com.json" {
		t.Fatalf("credentials = %+v; the roster decides what exists", doc.Credentials)
	}
	if doc.Counters.Credentials != 1 {
		t.Fatalf("counters = %+v", doc.Counters)
	}
	row := rowOf(t, doc, "claude", qc.WindowWeekly)
	if row.Aggregate.MemberCount != 1 {
		t.Fatalf("an unrostered entry reached a row: %+v", row.Aggregate)
	}
}

// degradedRoster adds two credentials the snapshot does not know about, and
// flags the disabled and unavailable ones.
func degradedRoster() []Identity {
	return []Identity{
		{AuthIndex: "claude-fresh@example.com.json", Provider: "claude"},
		{AuthIndex: "claude-stale@example.com.json", Provider: "claude"},
		{AuthIndex: "claude-failing@example.com.json", Provider: "claude"},
		{AuthIndex: "claude-pending@example.com.json", Provider: "claude"},
		{AuthIndex: "claude-disabled@example.com.json", Provider: "claude", Disabled: true},
		// Parked by CPA after a run of failures, with the run still in its ring.
		// A credential in cooldown is the one case where the strip and the
		// routing label disagree on purpose: it was busy, and now nothing goes
		// to it.
		{AuthIndex: "claude-unavailable@example.com.json", Provider: "claude", Unavailable: true,
			Recent: tenMinuteRing(map[int]int64{15: 3}, map[int]int64{16: 4, 17: 2})},
		// In the roster, absent from the snapshot: quota-cache does not poll it.
		{AuthIndex: "gemini-unsupported@example.com.json", Provider: "gemini"},
		// A host whose ring is not CPA's current shape — twelve buckets of five
		// minutes. Nothing may assume 10 minutes or 3h20m; both are read off
		// what the host sent.
		{AuthIndex: "codex-model@example.com.json", Provider: "codex",
			Recent: ringOf(12, 5*time.Minute, map[int]int64{10: 1, 11: 2}, nil)},
		{AuthIndex: "xai-raw@example.com.json", Provider: "xai"},
	}
}

// degradedSamples is an hour of history for the Claude session row, enough for
// a real trend arrow rather than unknown.
func degradedSamples(t *testing.T) []Sample {
	t.Helper()
	samples := []Sample{}
	for _, id := range []string{
		"claude-fresh@example.com.json",
		"claude-stale@example.com.json",
		"claude-failing@example.com.json",
	} {
		samples = append(samples,
			Sample{AuthIndex: id, WindowKey: qc.WindowSession, At: at(t, -5400), Remaining: 0.95},
			Sample{AuthIndex: id, WindowKey: qc.WindowSession, At: at(t, -3600), Remaining: 0.90},
			Sample{AuthIndex: id, WindowKey: qc.WindowSession, At: at(t, -900), Remaining: 0.70},
		)
	}
	return samples
}

func buildDegraded(t *testing.T) Document {
	t.Helper()
	return Build(Input{
		Snapshot:   loadSnapshot(t, "degraded-states.json"),
		Identities: degradedRoster(),
		Samples:    degradedSamples(t),
		StaleAfter: 45 * time.Minute,
	}, at(t, 0))
}

// The second committed contract. The happy-path golden shows none of the
// degraded states a real deployment produces, so the web app would have to
// invent them; this one exercises every branch it has to render.
func TestGoldenDegradedDocument(t *testing.T) {
	doc := buildDegraded(t)
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	path := filepath.Join("..", "..", "testdata", "golden", "summary-degraded.json")
	if *update {
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("degraded golden document rewritten")
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("degraded golden missing; regenerate with: make golden (%v)", err)
	}
	if !bytes.Equal(raw, want) {
		t.Fatalf("built document differs from testdata/golden/summary-degraded.json.\n" +
			"The web app develops against that file too, so this is a contract change.\n" +
			"Review it, then regenerate with: make golden")
	}
}

// Every state the web app has to render must actually appear in the degraded
// contract, or it is not doing its job.
func TestDegradedContractCoversEveryRenderableState(t *testing.T) {
	doc := buildDegraded(t)

	statuses := map[string]bool{}
	for _, c := range doc.Credentials {
		statuses[c.Status] = true
	}
	for _, want := range []string{StatusOK, StatusError, StatusPending, StatusDisabled, StatusUnavailable, StatusUnsupported} {
		if !statuses[want] {
			t.Errorf("no credential with status %q", want)
		}
	}

	levels, trends, states, hints := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	issues := map[string]bool{}
	var sawNullReset, sawEmptySubtext, sawUnmatched, sawModel, sawExcluded bool
	for _, provider := range doc.Providers {
		for _, row := range provider.Rows {
			levels[row.Aggregate.Level] = true
			trends[row.Aggregate.Trend] = true
			if !row.Matched {
				sawUnmatched = true
			}
			if row.Aggregate.SoonestResetAtEpoch == nil {
				sawNullReset = true
			}
			if row.Aggregate.Subtext == "" {
				sawEmptySubtext = true
			}
			if row.Aggregate.ExcludedCount > 0 {
				sawExcluded = true
			}
			for _, entry := range row.Entries {
				levels[entry.Level] = true
				states[entry.State] = true
				hints[entry.ResetDisplayHint] = true
				if entry.SourceModel != nil {
					sawModel = true
				}
				for _, issue := range entry.DataIssues {
					issues[issue] = true
				}
			}
		}
	}
	for name, set := range map[string][]string{
		"level": {LevelOK, LevelLow, LevelCritical},
		"trend": {TrendDown, TrendUnknown},
		"state": {StatusOK, StatusError, StateStale, StateNoData, StatusPending},
		"hint":  {HintCountdown, HintNone},
	} {
		var have map[string]bool
		switch name {
		case "level":
			have = levels
		case "trend":
			have = trends
		case "state":
			have = states
		case "hint":
			have = hints
		}
		for _, want := range set {
			if !have[want] {
				t.Errorf("no %s = %q anywhere in the degraded contract", name, want)
			}
		}
	}
	for _, want := range []string{issueResetInPast, issueObserveError, issueStale} {
		if !issues[want] {
			t.Errorf("no dataIssue %q", want)
		}
	}
	if !sawNullReset || !sawEmptySubtext || !sawUnmatched || !sawModel || !sawExcluded {
		t.Errorf("missing: nullReset=%v emptySubtext=%v unmatchedRow=%v sourceModel=%v excluded=%v",
			sawNullReset, sawEmptySubtext, sawUnmatched, sawModel, sawExcluded)
	}
}

// End to end: the plan badge the design shows beside each credential. Claude
// reports a presentable name already; Codex reports an enum token and must
// arrive as the name the tier is sold under, precomputed, so no client needs a
// mapping table of its own.
func TestPlanBadgesAreDisplayReadyInTheDocument(t *testing.T) {
	doc := buildFixture(t)
	want := map[string]string{
		"claude-siphorchannel@example.com.json": "Max",
		"claude-agency@example.com.json":        "Team",
		"codex-noor@example.com.json":           "Pro 20x",
		"xai-noor@example.com.json":             "SuperGrok Heavy",
	}
	for _, c := range doc.Credentials {
		if expected, ok := want[c.ID]; ok && c.Plan != expected {
			t.Errorf("%s plan = %q; want %q", c.ID, c.Plan, expected)
		}
	}

	// And an operator override reaches the document.
	doc = Build(Input{
		Snapshot:   loadSnapshot(t, "seven-credentials.json"),
		Identities: fixtureRoster(),
		StaleAfter: 45 * time.Minute,
		PlanLabels: NormalizePlanLabels(map[string]string{"pro": "Codex Pro"}),
	}, at(t, 0))
	for _, c := range doc.Credentials {
		if c.ID == "codex-noor@example.com.json" && c.Plan != "Codex Pro" {
			t.Fatalf("override did not reach the document: %q", c.Plan)
		}
	}
}

// quota-cache leaves the legacy ObservedAt empty on a successful poll that
// produced no weekly window, so old readers cannot mistake a short-window-only
// account for 0% weekly use. That credential is polled, not pending — badging
// it "awaiting first poll" while its windows drive rows, and letting it stamp
// the whole document neverObserved, is worse than the bug it replaced.
func TestShortWindowOnlyAccountIsObservedNotPending(t *testing.T) {
	now := at(t, 0)
	observed := now.Add(-2 * time.Minute)
	snapshot := qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{
		"codex:codex-a@example.com.json": {
			Provider: "codex", AuthIndex: "codex-a@example.com.json",
			// ObservedAt deliberately zero; windows carry their own time.
			Windows: []qc.EntryWindow{
				{Key: qc.WindowSession, UsedPercent: 40, ResetAt: now.Add(time.Hour), ObservedAt: observed},
			},
		},
	}}
	doc := Build(Input{Snapshot: snapshot,
		Identities: []Identity{{AuthIndex: "codex-a@example.com.json", Provider: "codex"}},
		StaleAfter: time.Hour}, now)

	if doc.Credentials[0].Status != StatusOK {
		t.Fatalf("status = %q; a successful poll without a weekly window is not pending", doc.Credentials[0].Status)
	}
	if doc.Credentials[0].LastObservedEpoch != observed.Unix() {
		t.Fatalf("lastObservedEpoch = %d; it must follow the window observation", doc.Credentials[0].LastObservedEpoch)
	}
	if doc.Counters.ObservedOK != 1 {
		t.Fatalf("counters = %+v", doc.Counters)
	}
	if doc.Stale {
		t.Fatalf("the document was stamped %v with two-minute-old data", *doc.StaleReason)
	}
	if row := rowOf(t, doc, "codex", qc.WindowSession); row.Aggregate.MemberCount != 1 {
		t.Fatalf("its data is aggregated, so it must count as observed: %+v", row.Aggregate)
	}
}

// A credential with no observation anywhere must not become a row member at
// full remaining — the exact way a silent credential inflates a card.
func TestNeverObservedCredentialIsNeverARowMember(t *testing.T) {
	now := at(t, 0)
	snapshot := qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{
		"claude:claude-new@example.com.json": {
			Provider: "claude", AuthIndex: "claude-new@example.com.json",
			// Windows present but never observed: no timestamps at all.
			Windows: []qc.EntryWindow{{Key: qc.WindowWeekly, UsedPercent: 0}},
		},
	}}
	doc := Build(Input{Snapshot: snapshot,
		Identities: []Identity{{AuthIndex: "claude-new@example.com.json", Provider: "claude"}},
		StaleAfter: time.Hour}, now)
	if doc.Credentials[0].Status != StatusPending {
		t.Fatalf("status = %q; want pending", doc.Credentials[0].Status)
	}
	for _, p := range doc.Providers {
		for _, row := range p.Rows {
			if row.Aggregate.MemberCount != 0 {
				t.Fatalf("a never-observed credential was counted at %d%% in row %s",
					row.Aggregate.RemainingPercent, row.RowID)
			}
		}
	}
}

// The reported bug. CPA flags a credential unavailable while it sits in a quota
// cooldown, which is exactly the moment its figures matter most — and the row
// it vanished from was the one that would have explained why. It stays, with
// its real reading, on every card its provider has.
func TestCooldownCredentialStaysOnEveryCard(t *testing.T) {
	now := at(t, 0)
	observed := now.Add(-2 * time.Minute)
	windows := func(session, weekly float64) []qc.EntryWindow {
		return []qc.EntryWindow{
			{Key: qc.WindowSession, UsedPercent: session, ResetAt: now.Add(time.Hour), ObservedAt: observed},
			{Key: qc.WindowWeekly, UsedPercent: weekly, ResetAt: now.Add(48 * time.Hour), ObservedAt: observed},
		}
	}
	snapshot := qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{
		"claude:claude-fine@example.com.json": {
			Provider: "claude", AuthIndex: "claude-fine@example.com.json",
			ObservedAt: observed, Windows: windows(0, 40),
		},
		// Rate limited a minute ago: CPA will not route to it, quota-cache
		// polled it anyway, and it is empty. All three are true at once.
		"claude:claude-cooling@example.com.json": {
			Provider: "claude", AuthIndex: "claude-cooling@example.com.json",
			ObservedAt: observed, Windows: windows(100, 100),
		},
	}}
	doc := Build(Input{Snapshot: snapshot, Identities: []Identity{
		{AuthIndex: "claude-fine@example.com.json", Provider: "claude"},
		{AuthIndex: "claude-cooling@example.com.json", Provider: "claude", Unavailable: true},
	}, StaleAfter: time.Hour}, now)

	for _, rowID := range []string{qc.WindowSession, qc.WindowWeekly} {
		row := rowOf(t, doc, "claude", rowID)
		if len(row.Entries) != 2 || row.Aggregate.MemberCount != 2 {
			t.Fatalf("row %s dropped the credential in cooldown: %d entries, %+v",
				rowID, len(row.Entries), row.Aggregate)
		}
	}
	// Means of (100%, 0%) and (60%, 0%). Dropping the exhausted credential
	// would report 100% and 60% — a dashboard claiming full capacity at the
	// moment half the fleet is rate limited.
	if got := rowOf(t, doc, "claude", qc.WindowSession).Aggregate.RemainingPercent; got != 50 {
		t.Fatalf("session = %d%%; want 50", got)
	}
	if got := rowOf(t, doc, "claude", qc.WindowWeekly).Aggregate.RemainingPercent; got != 30 {
		t.Fatalf("weekly = %d%%; want 30", got)
	}
	// The reason it is parked is still reported, on the credential where a
	// client looks for it rather than on the reading, which is perfectly good.
	for _, c := range doc.Credentials {
		if c.ID == "claude-cooling@example.com.json" && c.Status != StatusUnavailable {
			t.Fatalf("status = %q; want unavailable", c.Status)
		}
	}
	for _, entry := range rowOf(t, doc, "claude", qc.WindowSession).Entries {
		if entry.CredentialID != "claude-cooling@example.com.json" {
			continue
		}
		if !entry.HasReading || entry.State != StatusOK || entry.RemainingPercent != 0 {
			t.Fatalf("a fresh reading from a parked credential is still a good reading: %+v", entry)
		}
	}
}

// Every card lists every credential the provider has, in catalog order,
// whatever state each one is in.
func TestEveryCredentialAppearsInEveryRow(t *testing.T) {
	doc := buildDegraded(t)
	for _, provider := range doc.Providers {
		catalog := []string{}
		for _, c := range doc.Credentials {
			if c.Provider == provider.ID {
				catalog = append(catalog, c.ID)
			}
		}
		for _, row := range provider.Rows {
			if len(row.Entries) != provider.CredentialCount {
				t.Fatalf("%s/%s has %d entries for %d credentials",
					provider.ID, row.RowID, len(row.Entries), provider.CredentialCount)
			}
			if sum := row.Aggregate.MemberCount + row.Aggregate.ExcludedCount; sum != provider.CredentialCount {
				t.Fatalf("%s/%s: members+excluded = %d, credentials = %d",
					provider.ID, row.RowID, sum, provider.CredentialCount)
			}
			for i, entry := range row.Entries {
				if entry.CredentialID != catalog[i] {
					t.Fatalf("%s/%s entry %d = %s; want %s",
						provider.ID, row.RowID, i, entry.CredentialID, catalog[i])
				}
			}
		}
	}
}

// An entry with nothing behind it must not read as a credential at zero. The
// two look identical in every numeric field, so hasReading is what separates
// them, and level is left empty rather than resolving to critical.
func TestEntryWithNoReadingIsNotAZeroReading(t *testing.T) {
	doc := buildDegraded(t)
	fable := rowOf(t, doc, "claude", qc.WindowWeeklyFable)
	seen := map[string]RowEntry{}
	for _, entry := range fable.Entries {
		seen[entry.CredentialID] = entry
	}
	absent, ok := seen["claude-pending@example.com.json"]
	if !ok {
		t.Fatal("a never-polled credential is missing from the card")
	}
	if absent.HasReading || absent.Level != "" || absent.State != StatusPending {
		t.Fatalf("placeholder = %+v; want no reading, no level, pending", absent)
	}
	if absent.ResetAtEpoch != nil || absent.ResetDisplayHint != HintNone {
		t.Fatalf("placeholder invented a reset: %+v", absent)
	}
	// Polling fine, simply has no Fable allowance. Nothing is wrong with it and
	// its state must not say otherwise.
	if healthy := seen["claude-stale@example.com.json"]; healthy.HasReading || healthy.State != StateNoData {
		t.Fatalf("no-such-window = %+v; want no reading, noData", healthy)
	}
	if reporting := seen["claude-fresh@example.com.json"]; !reporting.HasReading {
		t.Fatal("the one credential that did report the window lost its reading")
	}
}

// Trend history is keyed off the document, so a placeholder would write a
// fabricated 0% into it and every later comparison would measure against that.
func TestSamplesFromSkipsEntriesWithNoReading(t *testing.T) {
	doc := buildDegraded(t)
	now := at(t, 0)
	recorded := map[string]bool{}
	for _, s := range SamplesFrom(doc, now) {
		recorded[s.WindowKey+"|"+s.AuthIndex] = true
	}
	if !recorded[qc.WindowWeeklyFable+"|claude-fresh@example.com.json"] {
		t.Fatal("a real reading was not sampled")
	}
	for _, id := range []string{"claude-pending@example.com.json", "claude-stale@example.com.json"} {
		if recorded[qc.WindowWeeklyFable+"|"+id] {
			t.Fatalf("%s has no fable window, but a 0%% sample was recorded for it", id)
		}
	}
}
