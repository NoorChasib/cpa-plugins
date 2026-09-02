package learner

import (
	"fmt"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/fingerprint"
)

var t0 = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

func settings() Settings {
	return Settings{
		Rules:               fingerprint.Rules{ClaudeEntrypoints: []string{"cli", "sdk-cli", "claude-vscode", "sdk-ts", "sdk-py"}, RequireClaudeCodeBeta: true},
		MinObservations:     3,
		MinDistinctSessions: 2,
		ObservationWindow:   24 * time.Hour,
		MinVersion: map[fingerprint.Provider]fingerprint.Version{
			fingerprint.ProviderClaude: fingerprint.MustParseVersion("2.1.220"),
			fingerprint.ProviderCodex:  fingerprint.MustParseVersion("0.146.0"),
		},
	}
}

func claude(version, pkg string) fingerprint.Candidate {
	v := fingerprint.MustParseVersion(version)
	return fingerprint.Candidate{
		Provider:       fingerprint.ProviderClaude,
		Version:        v,
		UserAgent:      fingerprint.CanonicalClaudeUserAgent(v),
		PackageVersion: pkg,
		RuntimeVersion: "v26.3.0",
		OS:             "Linux",
		Arch:           "x64",
		Entrypoint:     "sdk-ts",
	}
}

func codex(version string) fingerprint.Candidate {
	return fingerprint.Candidate{
		Provider:  fingerprint.ProviderCodex,
		Version:   fingerprint.MustParseVersion(version),
		UserAgent: "codex-tui/" + version + " (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 (codex-tui; " + version + ")",
	}
}

func newLearner(now *time.Time) *Learner {
	l := New(settings(), []fingerprint.Provider{fingerprint.ProviderClaude, fingerprint.ProviderCodex}, func() time.Time { return *now })
	l.SetBaseline(fingerprint.ProviderClaude, fingerprint.MustParseVersion("2.1.220"), "")
	l.SetBaseline(fingerprint.ProviderCodex, fingerprint.MustParseVersion("0.146.0"), "")
	return l
}

func TestQuorumRequiresObservationsAndSessions(t *testing.T) {
	now := t0
	l := newLearner(&now)
	c := claude("2.1.258", "0.112.1")

	if d := l.Observe(c, "s1", now); d.Reason != DecisionTracked || d.Ready != nil {
		t.Fatalf("1st: %+v", d)
	}
	if d := l.Observe(c, "s1", now.Add(time.Second)); d.Reason != DecisionTracked {
		t.Fatalf("2nd: %+v", d)
	}
	// Three observations but a single session: not enough.
	if d := l.Observe(c, "s1", now.Add(2*time.Second)); d.Reason != DecisionTracked || d.Ready != nil {
		t.Fatalf("3rd single-session must not reach quorum: %+v", d)
	}
	if _, ok := l.Ready(fingerprint.ProviderClaude); ok {
		t.Fatal("Ready reported a single-session candidate")
	}
	d := l.Observe(c, "s2", now.Add(3*time.Second))
	if d.Reason != DecisionQuorum || d.Ready == nil || d.Ready.Key() != c.Key() {
		t.Fatalf("4th with second session: %+v", d)
	}
	ready, ok := l.Ready(fingerprint.ProviderClaude)
	if !ok || ready.Key() != c.Key() {
		t.Fatalf("Ready = %+v %v", ready, ok)
	}
}

func TestAnonymousObservationsNeverCountAsSessions(t *testing.T) {
	now := t0
	l := newLearner(&now)
	c := codex("0.152.1")
	for i := 0; i < 5; i++ {
		if d := l.Observe(c, "", now); d.Ready != nil {
			t.Fatalf("anonymous-only observations reached a 2-session quorum at %d", i)
		}
	}
	// One named session plus anonymous is still only ONE distinct session.
	if d := l.Observe(c, "thread-1", now); d.Reason != DecisionTracked || d.Ready != nil {
		t.Fatalf("anonymous must not count as a session: %+v", d)
	}
	if d := l.Observe(c, "thread-2", now); d.Reason != DecisionQuorum {
		t.Fatalf("two named sessions: %+v", d)
	}
	ev := l.Summarize(fingerprint.ProviderCodex)
	if len(ev) != 1 || ev[0].Observations != 7 || ev[0].DistinctSessions != 2 {
		t.Fatalf("summary = %+v", ev)
	}

	// With min-distinct-sessions 1 (the default), anonymous-only clients still
	// reach quorum on observation count alone.
	st := settings()
	st.MinDistinctSessions = 1
	l2 := New(st, []fingerprint.Provider{fingerprint.ProviderCodex}, func() time.Time { return now })
	l2.SetBaseline(fingerprint.ProviderCodex, fingerprint.MustParseVersion("0.146.0"), "")
	l2.Observe(c, "", now)
	l2.Observe(c, "", now)
	if d := l2.Observe(c, "", now); d.Reason != DecisionQuorum {
		t.Fatalf("anonymous-only with 1-session rule: %+v", d)
	}
}

func TestNeverDowngradeOrRepromoteEqual(t *testing.T) {
	now := t0
	l := newLearner(&now)
	if d := l.Observe(claude("2.1.220", "0.94.0"), "s1", now); d.Reason != DecisionNotNewer {
		t.Fatalf("equal version: %+v", d)
	}
	if d := l.Observe(claude("2.1.100", "0.90.0"), "s1", now); d.Reason != DecisionBelowFloor {
		t.Fatalf("below floor: %+v", d)
	}
	// Baseline raised above the floor: older-than-baseline but above-floor
	// candidates are "not newer".
	l.SetBaseline(fingerprint.ProviderClaude, fingerprint.MustParseVersion("2.1.260"), "")
	if d := l.Observe(claude("2.1.258", "0.112.1"), "s1", now); d.Reason != DecisionNotNewer {
		t.Fatalf("older than raised baseline: %+v", d)
	}
	if len(l.Export()) != 0 {
		t.Fatalf("rejected candidates were tracked: %+v", l.Export())
	}
}

func TestFloorAppliesEvenWhenBaselineLower(t *testing.T) {
	now := t0
	l := newLearner(&now)
	// Operator wrote an ancient explicit baseline into config.yaml.
	l.SetBaseline(fingerprint.ProviderClaude, fingerprint.MustParseVersion("2.0.0"), "")
	if d := l.Observe(claude("2.1.100", "0.90.0"), "s1", now); d.Reason != DecisionBelowFloor {
		t.Fatalf("floor must win: %+v", d)
	}
	if d := l.Observe(claude("2.1.221", "0.95.0"), "s1", now); d.Reason != DecisionTracked {
		t.Fatalf("above floor and baseline: %+v", d)
	}
}

func TestUnknownBaselineUsesFloor(t *testing.T) {
	now := t0
	l := New(settings(), []fingerprint.Provider{fingerprint.ProviderClaude}, func() time.Time { return now })
	if d := l.Observe(claude("2.1.220", "0.94.0"), "s1", now); d.Reason != DecisionNotNewer {
		t.Fatalf("at floor with unknown baseline: %+v", d)
	}
	if d := l.Observe(claude("2.1.221", "0.94.0"), "s1", now); d.Reason != DecisionTracked {
		t.Fatalf("above floor with unknown baseline: %+v", d)
	}
}

func TestSetBaselineDropsStaleCandidates(t *testing.T) {
	now := t0
	l := newLearner(&now)
	l.Observe(claude("2.1.258", "0.112.1"), "s1", now)
	l.Observe(claude("2.1.270", "0.120.0"), "s1", now)
	l.SetBaseline(fingerprint.ProviderClaude, fingerprint.MustParseVersion("2.1.258"), "")
	items := l.Export()
	if len(items) != 1 || items[0].Candidate.Version.String() != "2.1.270" {
		t.Fatalf("export = %+v", items)
	}
}

func TestObservationWindowExpiresRecords(t *testing.T) {
	now := t0
	l := newLearner(&now)
	c := claude("2.1.258", "0.112.1")
	l.Observe(c, "s1", now)
	l.Observe(c, "s2", now.Add(time.Minute))
	// 25 hours later the two old records are outside the window.
	later := now.Add(25 * time.Hour)
	if d := l.Observe(c, "s3", later); d.Reason != DecisionTracked || d.Ready != nil {
		t.Fatalf("expired records counted: %+v", d)
	}
	now = later
	ev := l.Summarize(fingerprint.ProviderClaude)
	if len(ev) != 1 || ev[0].Observations != 1 || ev[0].DistinctSessions != 1 {
		t.Fatalf("summary = %+v", ev)
	}
}

func TestDifferentTuplesAreDifferentCandidates(t *testing.T) {
	now := t0
	l := newLearner(&now)
	a := claude("2.1.258", "0.112.1")
	b := claude("2.1.258", "0.113.0") // same version, different package -> different key
	l.Observe(a, "s1", now)
	l.Observe(a, "s2", now)
	l.Observe(b, "s3", now)
	if _, ok := l.Ready(fingerprint.ProviderClaude); ok {
		t.Fatal("mixed tuples must never be combined toward quorum")
	}
	if d := l.Observe(a, "s1", now); d.Reason != DecisionQuorum {
		t.Fatalf("a should meet quorum alone: %+v", d)
	}
}

func TestReadyPrefersNewestThenBestAttested(t *testing.T) {
	now := t0
	l := newLearner(&now)
	old := claude("2.1.258", "0.112.1")
	newer := claude("2.1.260", "0.113.0")
	for _, s := range []string{"a", "b", "c", "d"} {
		l.Observe(old, s, now)
	}
	for _, s := range []string{"a", "b", "c"} {
		l.Observe(newer, s, now)
	}
	ready, ok := l.Ready(fingerprint.ProviderClaude)
	if !ok || ready.Version.String() != "2.1.260" {
		t.Fatalf("ready = %+v", ready)
	}

	// Same version, two package tuples; the one with more observations wins.
	l.Reset()
	x := claude("2.1.261", "0.114.0")
	y := claude("2.1.261", "0.115.0")
	for _, s := range []string{"a", "b", "c"} {
		l.Observe(x, s, now)
	}
	for _, s := range []string{"a", "b", "c", "d", "e"} {
		l.Observe(y, s, now)
	}
	ready, _ = l.Ready(fingerprint.ProviderClaude)
	if ready.PackageVersion != "0.115.0" {
		t.Fatalf("ready = %+v, want better attested tuple", ready)
	}
}

func TestProvidersAreIndependent(t *testing.T) {
	now := t0
	l := newLearner(&now)
	for _, s := range []string{"a", "b", "c"} {
		l.Observe(codex("0.152.1"), s, now)
	}
	if _, ok := l.Ready(fingerprint.ProviderClaude); ok {
		t.Fatal("codex evidence leaked into claude")
	}
	if c, ok := l.Ready(fingerprint.ProviderCodex); !ok || c.Version.String() != "0.152.1" {
		t.Fatalf("codex ready = %+v %v", c, ok)
	}
}

func TestUnmanagedProviderRejected(t *testing.T) {
	now := t0
	l := New(settings(), []fingerprint.Provider{fingerprint.ProviderClaude}, func() time.Time { return now })
	if d := l.Observe(codex("0.152.1"), "s", now); d.Reason != DecisionProviderUnmanaged {
		t.Fatalf("decision = %+v", d)
	}
	l.Reconfigure(settings(), []fingerprint.Provider{fingerprint.ProviderCodex})
	l.SetBaseline(fingerprint.ProviderCodex, fingerprint.MustParseVersion("0.146.0"), "")
	if d := l.Observe(codex("0.152.1"), "s", now); d.Reason != DecisionTracked {
		t.Fatalf("after reconfigure: %+v", d)
	}
	l.SetBaseline(fingerprint.ProviderClaude, fingerprint.MustParseVersion("2.1.220"), "")
	if d := l.Observe(claude("2.1.258", "0.112.1"), "s", now); d.Reason != DecisionProviderUnmanaged {
		t.Fatalf("claude should be unmanaged now: %+v", d)
	}
}

func TestReconfigureDropsUnmanagedPending(t *testing.T) {
	now := t0
	l := newLearner(&now)
	l.Observe(codex("0.152.1"), "s", now)
	l.Observe(claude("2.1.258", "0.112.1"), "s", now)
	l.Reconfigure(settings(), []fingerprint.Provider{fingerprint.ProviderClaude})
	items := l.Export()
	if len(items) != 1 || items[0].Candidate.Provider != fingerprint.ProviderClaude {
		t.Fatalf("export = %+v", items)
	}
}

func TestBoundsEvictStalestAndCapRecords(t *testing.T) {
	now := t0
	l := newLearner(&now)
	for i := 0; i < MaxPendingPerProvider+5; i++ {
		c := claude("2.1.258", fmt.Sprintf("0.%d.0", 100+i))
		l.Observe(c, "s", now.Add(time.Duration(i)*time.Second))
	}
	items := l.Export()
	if len(items) != MaxPendingPerProvider {
		t.Fatalf("pending = %d, want %d", len(items), MaxPendingPerProvider)
	}
	for _, item := range items {
		if item.Candidate.PackageVersion == "0.100.0" {
			t.Fatal("stalest candidate was not evicted")
		}
	}

	l.Reset()
	c := claude("2.1.258", "0.112.1")
	for i := 0; i < MaxRecordsPerCandidate+50; i++ {
		l.Observe(c, fmt.Sprintf("s%d", i), now)
	}
	items = l.Export()
	if len(items) != 1 || len(items[0].Records) != MaxRecordsPerCandidate {
		t.Fatalf("records = %d", len(items[0].Records))
	}
}

func TestExportImportRoundTripMergesAndValidates(t *testing.T) {
	now := t0
	l := newLearner(&now)
	c := claude("2.1.258", "0.112.1")
	l.Observe(c, "s1", now)
	l.Observe(c, "s2", now.Add(time.Second))
	l.Observe(codex("0.152.1"), "t1", now)
	exported := l.Export()
	if len(exported) != 2 {
		t.Fatalf("export = %d items", len(exported))
	}
	// Mutating the export must not affect the learner.
	exported[0].Records[0].SessionID = "tampered"

	l2 := newLearner(&now)
	// Import at an instant no earlier than the newest record (now+1s), since
	// future-stamped records are pruned.
	if dropped := l2.Import(l.Export(), now.Add(time.Second)); dropped != 0 {
		t.Fatalf("dropped %d valid entries", dropped)
	}
	if d := l2.Observe(c, "s1", now.Add(2*time.Second)); d.Reason != DecisionQuorum {
		t.Fatalf("imported evidence not counted: %+v", d)
	}

	// Import MERGES with live evidence instead of replacing it.
	l3 := newLearner(&now)
	l3.Observe(c, "live-1", now.Add(3*time.Second))
	l3.Observe(c, "s1", now) // identical (session, time) to an exported record: deduplicated
	l3.Import(l.Export(), now.Add(3*time.Second))
	ev := l3.Summarize(fingerprint.ProviderClaude)
	if len(ev) != 1 || ev[0].Observations != 3 || ev[0].DistinctSessions != 3 {
		t.Fatalf("merged summary = %+v", ev)
	}

	// Structurally invalid entries are dropped and counted; stale ones are
	// dropped silently.
	l4 := newLearner(&now)
	l4.SetBaseline(fingerprint.ProviderClaude, fingerprint.MustParseVersion("2.1.258"), "")
	bogus := l.Export()
	bogus = append(bogus,
		Pending{Key: "claude|9.9.9|x|y", Candidate: claude("1.0.0", "0.1.0"), Records: []Record{{At: now}}, FirstSeen: now, LastSeen: now},
		Pending{Key: claude("2.1.300", "0.1.0").Key(), Candidate: claude("2.1.300", "0.1.0"), FirstSeen: now, LastSeen: now}, // no records
		func() Pending {
			bad := claude("2.1.301", "0.1.0")
			bad.UserAgent = "claude-cli/2.1.301 (external, sdk-ts)" // non-canonical
			return Pending{Key: bad.Key(), Candidate: bad, Records: []Record{{At: now}}, FirstSeen: now, LastSeen: now}
		}(),
		func() Pending {
			future := claude("2.1.302", "0.1.0")
			return Pending{Key: future.Key(), Candidate: future, Records: []Record{{At: now.Add(48 * time.Hour)}}, FirstSeen: now, LastSeen: now}
		}(),
		func() Pending {
			stale := claude("2.1.303", "0.1.0")
			return Pending{Key: stale.Key(), Candidate: stale, Records: []Record{{At: now.Add(-48 * time.Hour)}}, FirstSeen: now.Add(-48 * time.Hour), LastSeen: now.Add(-48 * time.Hour)}
		}(),
		func() Pending {
			badSess := claude("2.1.304", "0.1.0")
			return Pending{Key: badSess.Key(), Candidate: badSess, Records: []Record{{SessionID: "has space", At: now}}, FirstSeen: now, LastSeen: now}
		}(),
		func() Pending {
			noOS := claude("2.1.305", "0.1.0")
			noOS.OS = ""
			return Pending{Key: noOS.Key(), Candidate: noOS, Records: []Record{{At: now}}, FirstSeen: now, LastSeen: now}
		}(),
		func() Pending {
			denied := claude("2.1.306", "0.1.0")
			denied.Entrypoint = "mcp"
			return Pending{Key: denied.Key(), Candidate: denied, Records: []Record{{At: now}}, FirstSeen: now, LastSeen: now}
		}(),
	)
	dropped := l4.Import(bogus, now)
	if dropped != 8 {
		t.Errorf("dropped = %d, want 8", dropped)
	}
	items := l4.Export()
	if len(items) != 1 || items[0].Candidate.Provider != fingerprint.ProviderCodex {
		t.Fatalf("import filtering failed: %+v", items)
	}
}

func TestForgetAndReset(t *testing.T) {
	now := t0
	l := newLearner(&now)
	c := claude("2.1.258", "0.112.1")
	l.Observe(c, "s1", now)
	l.Forget(fingerprint.ProviderClaude, c.Key())
	if len(l.Export()) != 0 {
		t.Fatal("Forget did not remove candidate")
	}
	l.Observe(c, "s1", now)
	l.Observe(codex("0.152.1"), "s1", now)
	l.Reset()
	if len(l.Export()) != 0 {
		t.Fatal("Reset left candidates")
	}
	// Reset keeps baselines: an equal version is still "not newer".
	if d := l.Observe(claude("2.1.220", "0.94.0"), "s1", now); d.Reason != DecisionNotNewer {
		t.Fatalf("baseline lost on Reset: %+v", d)
	}
}

func TestBlockedBaselineRetainsEvidenceAndResumes(t *testing.T) {
	now := t0
	l := newLearner(&now)
	c := claude("2.1.258", "0.112.1")
	l.Observe(c, "s1", now)
	l.SetBaseline(fingerprint.ProviderClaude, fingerprint.MustParseVersion("2.1.220"), DecisionBaselineMalformed)
	if len(l.Export()) != 1 {
		t.Fatal("pending evidence pruned by a malformed baseline")
	}
	// Observations keep accumulating but report the block, never quorum.
	if d := l.Observe(c, "s2", now); d.Reason != DecisionBaselineMalformed || d.Ready != nil {
		t.Fatalf("decision = %+v", d)
	}
	if d := l.Observe(c, "s1", now); d.Reason != DecisionBaselineMalformed || d.Ready != nil {
		t.Fatalf("decision = %+v", d)
	}
	if _, ok := l.Ready(fingerprint.ProviderClaude); ok {
		t.Fatal("ready reported for a blocked baseline")
	}
	if l.Blocked(fingerprint.ProviderClaude) != DecisionBaselineMalformed || l.Blocked(fingerprint.ProviderCodex) != "" {
		t.Fatalf("Blocked() = %q / %q", l.Blocked(fingerprint.ProviderClaude), l.Blocked(fingerprint.ProviderCodex))
	}
	// Codex is unaffected.
	if d := l.Observe(codex("0.152.1"), "s1", now); d.Reason != DecisionTracked {
		t.Fatalf("codex = %+v", d)
	}
	// Repairing the file resumes evaluation on the RETAINED evidence: the
	// three observations above already satisfy quorum.
	l.SetBaseline(fingerprint.ProviderClaude, fingerprint.MustParseVersion("2.1.220"), "")
	ready, ok := l.Ready(fingerprint.ProviderClaude)
	if !ok || ready.Key() != c.Key() {
		t.Fatalf("evaluation did not resume on retained evidence: %+v %v", ready, ok)
	}
	// Other block reasons behave the same way.
	l.SetBaseline(fingerprint.ProviderClaude, fingerprint.MustParseVersion("2.1.220"), "duplicate_key")
	if d := l.Observe(c, "s3", now); d.Reason != "duplicate_key" {
		t.Fatalf("duplicate_key block: %+v", d)
	}
}

func TestSummarizeOrdersNewestFirst(t *testing.T) {
	now := t0
	l := newLearner(&now)
	l.Observe(claude("2.1.258", "0.112.1"), "s1", now)
	l.Observe(claude("2.1.270", "0.120.0"), "s1", now)
	ev := l.Summarize(fingerprint.ProviderClaude)
	if len(ev) != 2 || ev[0].Candidate.Version.String() != "2.1.270" {
		t.Fatalf("summary = %+v", ev)
	}
	if ev[0].QuorumMet {
		t.Fatal("quorum reported for single observation")
	}
	if len(l.Summarize(fingerprint.ProviderCodex)) != 0 {
		t.Fatal("codex summary should be empty")
	}
}

func TestExpiredCandidatesArePrunedNotExported(t *testing.T) {
	now := t0
	l := newLearner(&now)
	c := claude("2.1.258", "0.112.1")
	l.Observe(c, "s1", now)
	if len(l.Export()) != 1 {
		t.Fatal("precondition: one pending candidate")
	}
	// Past the window every record has aged out: the candidate disappears
	// instead of lingering as an empty entry.
	now = now.Add(25 * time.Hour)
	if got := l.Summarize(fingerprint.ProviderClaude); len(got) != 0 {
		t.Errorf("Summarize kept an empty candidate: %+v", got)
	}
	exported := l.Export()
	if len(exported) != 0 {
		t.Fatalf("Export returned empty candidates: %+v", exported)
	}
	if _, ok := l.Ready(fingerprint.ProviderClaude); ok {
		t.Error("Ready reported an expired candidate")
	}
	// A round trip of the (empty) export drops nothing, so a restart does
	// not report misleading restored_dropped counts.
	l2 := newLearner(&now)
	if dropped := l2.Import(exported, now); dropped != 0 {
		t.Errorf("Import dropped %d entries from an empty export", dropped)
	}
	// A fresh observation after expiry starts a clean candidate.
	if d := l.Observe(c, "s2", now); d.Reason != DecisionTracked {
		t.Fatalf("decision after expiry = %+v", d)
	}
	if ev := l.Summarize(fingerprint.ProviderClaude); len(ev) != 1 || ev[0].Observations != 1 {
		t.Errorf("summary after re-observation = %+v", ev)
	}
}

func TestImportPrunesRecordsPartially(t *testing.T) {
	now := t0
	l := newLearner(&now)
	c := claude("2.1.258", "0.112.1")
	item := Pending{Key: c.Key(), Candidate: c, FirstSeen: now.Add(-30 * time.Hour), LastSeen: now.Add(time.Hour), Records: []Record{
		{SessionID: "old", At: now.Add(-30 * time.Hour)}, // outside window
		{SessionID: "future", At: now.Add(time.Hour)},    // in the future
		{SessionID: "bad id", At: now},                   // invalid session id
		{SessionID: "s1", At: now.Add(-time.Hour)},
		{SessionID: "s2", At: now},
	}}
	if dropped := l.Import([]Pending{item}, now); dropped != 0 {
		t.Fatalf("partially valid entry dropped entirely (%d)", dropped)
	}
	ev := l.Summarize(fingerprint.ProviderClaude)
	if len(ev) != 1 || ev[0].Observations != 2 || ev[0].DistinctSessions != 2 {
		t.Fatalf("summary = %+v", ev)
	}
	// Restored entries obey the CURRENT allowlist.
	st := settings()
	st.Rules.ClaudeEntrypoints = []string{"cli"}
	l2 := New(st, []fingerprint.Provider{fingerprint.ProviderClaude}, func() time.Time { return now })
	l2.SetBaseline(fingerprint.ProviderClaude, fingerprint.MustParseVersion("2.1.220"), "")
	if dropped := l2.Import([]Pending{item}, now); dropped != 1 {
		t.Errorf("sdk-ts candidate accepted under a cli-only allowlist (dropped=%d)", dropped)
	}
}
