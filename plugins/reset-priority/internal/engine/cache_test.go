package engine

import (
	"context"
	"encoding/json"
	quotaclient "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/config"
	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/hostapi"
)

func writeQuotaFixture(t *testing.T, path string, observed time.Time) {
	t.Helper()
	snapshot := quotaclient.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]quotaclient.Entry{
		"claude:idx-a": {Provider: "claude", AuthIndex: "idx-a", Percent: 50, ResetAt: day(1), ObservedAt: observed},
	}}
	raw, _ := json.Marshal(snapshot)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// cacheEntry is one Claude account's quota-cache entry in a test snapshot.
type cacheEntry struct {
	// resetAt is the regular weekly reset. Zero means the provider answered
	// without a weekly window.
	resetAt    time.Time
	observedAt time.Time
	lastError  string
}

// writeCacheEntries replaces the snapshot at path with one Claude entry per
// account name (auth index "idx-"+name). An empty map writes a valid snapshot
// with no entries, as quota-cache leaves it after a bootstrap wipe. It writes a
// temporary file and renames it, as quota-cache does, so a reader never sees a
// partial file.
func writeCacheEntries(t *testing.T, path string, entries map[string]cacheEntry) {
	t.Helper()
	snapshot := quotaclient.Snapshot{
		Schema:           1,
		ProviderCooldown: map[string]time.Time{},
		Entries:          map[string]quotaclient.Entry{},
	}
	for name, entry := range entries {
		authIndex := "idx-" + name
		snapshot.Entries[quotaclient.Key("claude", authIndex)] = quotaclient.Entry{
			Provider:    "claude",
			AuthIndex:   authIndex,
			Percent:     40,
			ResetAt:     entry.resetAt,
			ObservedAt:  entry.observedAt,
			LastAttempt: entry.observedAt,
			LastError:   entry.lastError,
		}
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func cacheConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := defaultConfig()
	cfg.QuotaCachePath = filepath.Join(t.TempDir(), "snapshot.json")
	return cfg
}

// retryState returns an account's retry generation and the number of ladder
// timers armed since its last cancellation (fired timers included).
func (env *testEnv) retryState(authIndex string) (seq, timers int) {
	env.t.Helper()
	env.eng.mu.Lock()
	defer env.eng.mu.Unlock()
	acct := env.eng.accounts[authIndex]
	if acct == nil {
		env.t.Fatalf("account %s is not managed", authIndex)
	}
	return acct.retrySeq, len(acct.retryTimers)
}

// confirmWaveTimers returns the number of first-confirmation wave timers armed
// since the wave was last cancelled or re-armed (fired timers included). Zero
// means no wave is armed.
func (env *testEnv) confirmWaveTimers() int {
	env.t.Helper()
	env.eng.mu.Lock()
	defer env.eng.mu.Unlock()
	return len(env.eng.confirmTimers)
}

// timersBeforeNextReconcile returns every pending timer due before the
// background reconciliation tick. With no ladder, health poll, or near
// deadline, it is empty.
func (env *testEnv) timersBeforeNextReconcile() []time.Time {
	env.t.Helper()
	env.eng.mu.Lock()
	next := env.eng.nextReconcileAt
	env.eng.mu.Unlock()
	if next.IsZero() {
		env.t.Fatal("no reconciliation tick is scheduled")
	}
	var out []time.Time
	for _, at := range env.clk.PendingAt() {
		if at.Before(next) {
			out = append(out, at)
		}
	}
	return out
}

// assertLadderArmedOnce checks that exactly one bounded ladder, measured from
// armedAt, is pending: each resetRetryDelays offset appears exactly once.
func assertLadderArmedOnce(t *testing.T, pending []time.Time, armedAt time.Time) {
	t.Helper()
	for _, delay := range resetRetryDelays {
		want := armedAt.Add(delay)
		count := 0
		for _, at := range pending {
			if at.Equal(want) {
				count++
			}
		}
		if count != 1 {
			t.Errorf("ladder step +%s: %d pending timers, want 1; pending: %v", delay, count, pending)
		}
	}
}

func TestCacheOnlyRankingAndMissingCacheNeverPollProvider(t *testing.T) {
	cfg := defaultConfig()
	cfg.DryRun = true
	cfg.QuotaCachePath = filepath.Join(t.TempDir(), "snapshot.json")
	env := newTestEnv(t, cfg)
	env.addAccount("claude", "a", day(2))
	writeQuotaFixture(t, cfg.QuotaCachePath, baseTime.Add(-time.Minute))
	env.reconcile()
	row, _ := env.statusRow("a")
	if row.ResetAt != day(1).Format(time.RFC3339) {
		t.Fatalf("cache deadline not used: %+v", row)
	}
	if err := os.Remove(cfg.QuotaCachePath); err != nil {
		t.Fatal(err)
	}
	env.reconcile()
	if len(env.claude.calls) != 0 {
		t.Fatal("cache consumer fell back to provider")
	}
	before := len(env.host.getCalls)
	env.eng.fetchOne(context.Background(), "idx-a")
	if len(env.host.getCalls) != before {
		t.Fatal("quota acquisition read credential tokens")
	}
}

func TestCacheObservationBeforeRecoveryCannotPromoteAccount(t *testing.T) {
	cfg := defaultConfig()
	cfg.DryRun = true
	cfg.QuotaCachePath = filepath.Join(t.TempDir(), "snapshot.json")
	env := newTestEnv(t, cfg)
	env.addAccount("claude", "a", day(2))
	writeQuotaFixture(t, cfg.QuotaCachePath, baseTime)
	env.reconcile()
	quarantineAccount(env, "idx-a")
	env.reconcile()
	env.clk.Advance(time.Minute)
	recoverAccount(env, "idx-a")
	env.reconcile()
	row, _ := env.statusRow("a")
	if row.Health == string(HealthHealthy) {
		t.Fatal("pre-recovery cached quota promoted account")
	}
	env.clk.Advance(time.Minute)
	writeQuotaFixture(t, cfg.QuotaCachePath, env.clk.Now())
	env.reconcile()
	row, _ = env.statusRow("a")
	if row.Health != string(HealthHealthy) {
		t.Fatalf("fresh observation did not recover: %+v", row)
	}
	if len(env.claude.calls) != 0 {
		t.Fatal("recovery polled provider")
	}
}

// TestCacheNewAccountConfirmedByLadder: an account added between passes has no
// quota-cache entry yet when the next reconciliation discovers it. The
// first-confirmation wave re-reads the snapshot and inserts it at its reset
// position once quota-cache has polled it, well before reconcile-interval.
func TestCacheNewAccountConfirmedByLadder(t *testing.T) {
	env := newTestEnv(t, cacheConfig(t))
	path := env.eng.cfg.QuotaCachePath
	env.addAccount("claude", "a", day(2))
	env.addAccount("claude", "b", day(3))
	entries := map[string]cacheEntry{
		"a": {resetAt: day(2), observedAt: baseTime},
		"b": {resetAt: day(3), observedAt: baseTime},
	}
	writeCacheEntries(t, path, entries)
	env.reconcile()
	env.assertPhysical(map[string]int{"a": 200, "b": 100})

	// A new account with the earliest reset is discovered by a management
	// refresh before quota-cache has polled it.
	env.clk.Advance(10 * time.Minute)
	refreshAt := env.clk.Now()
	env.addAccount("claude", "e", day(1))
	env.reconcile()
	if row, _ := env.statusRow("e"); row.ResetState != string(ResetUnknown) {
		t.Fatalf("new account reset state = %s, want unknown before quota-cache polls it", row.ResetState)
	}
	env.assertPhysical(map[string]int{"a": 300, "b": 200, "e": 100})
	assertLadderArmedOnce(t, env.timersBeforeNextReconcile(), refreshAt)

	// quota-cache polls the new account at +20s; the +30s step confirms it.
	env.clk.Advance(20 * time.Second)
	entries["e"] = cacheEntry{resetAt: day(1), observedAt: env.clk.Now()}
	writeCacheEntries(t, path, entries)
	env.clk.AdvanceTo(refreshAt.Add(30 * time.Second))
	env.async.drain()

	if row, _ := env.statusRow("e"); row.ResetState != string(ResetConfirmed) {
		t.Fatalf("new account reset state = %s at the +30s step, want confirmed", row.ResetState)
	}
	env.assertPhysical(map[string]int{"e": 300, "a": 200, "b": 100})
	if got := env.claude.callCount(token("e")); got != 0 {
		t.Fatalf("cache mode made %d provider calls for the new account, want 0", got)
	}
}

// TestCacheNoWeeklyWindowIsNotRetried: a fresh entry with a zero reset_at is a
// complete answer (the provider reported no weekly window). It sets
// observedAt, so the account stays unknown but arms no first-confirmation
// wave, on this pass or on later ones.
func TestCacheNoWeeklyWindowIsNotRetried(t *testing.T) {
	env := newTestEnv(t, cacheConfig(t))
	path := env.eng.cfg.QuotaCachePath
	env.addAccount("claude", "a", time.Time{})
	env.addAccount("claude", "b", day(2))
	writeCacheEntries(t, path, map[string]cacheEntry{
		"a": {observedAt: baseTime},
		"b": {resetAt: day(2), observedAt: baseTime},
	})
	env.reconcile()

	row, _ := env.statusRow("a")
	if row.ResetState != string(ResetUnknown) || row.ObservedAt == "" {
		t.Fatalf("no-window account status = %+v, want unknown with an observation time", row)
	}
	if pending := env.timersBeforeNextReconcile(); len(pending) != 0 {
		t.Fatalf("no-window answer armed timers before reconcile-interval: %v", pending)
	}
	if timers := env.confirmWaveTimers(); timers != 0 {
		t.Fatalf("no-window account armed %d wave timers, want 0", timers)
	}

	// The next pass gets the same answer and must not re-arm the wave either.
	env.clk.Advance(59 * time.Minute)
	writeCacheEntries(t, path, map[string]cacheEntry{
		"a": {observedAt: env.clk.Now()},
		"b": {resetAt: day(2), observedAt: env.clk.Now()},
	})
	env.clk.Advance(time.Minute)
	if got := env.host.listCount(); got != 2 {
		t.Fatalf("reconciles = %d, want 2 (startup + interval)", got)
	}
	if pending := env.timersBeforeNextReconcile(); len(pending) != 0 {
		t.Fatalf("second pass re-laddered the no-window account: %v", pending)
	}
}

// recoveringReconcile drives account a through quarantine into recovering and
// returns the retry-generation change across the reconciliation that enters
// recovery. The fetch in that pass cannot promote a: in cache mode the only
// entry predates recovery, and standalone mode's provider fails.
func recoveringReconcile(t *testing.T, cacheMode bool) (*testEnv, time.Time, int) {
	t.Helper()
	cfg := defaultConfig()
	if cacheMode {
		cfg = cacheConfig(t)
	}
	env := newTestEnv(t, cfg)
	env.addAccount("claude", "a", day(1))
	if cacheMode {
		writeCacheEntries(t, cfg.QuotaCachePath, map[string]cacheEntry{"a": {resetAt: day(1), observedAt: baseTime}})
	}
	env.reconcile()
	quarantineAccount(env, "idx-a")
	env.reconcile()
	env.clk.Advance(time.Minute)
	recoverAccount(env, "idx-a")
	if !cacheMode {
		env.claude.setErr(token("a"), errFake("provider unavailable"))
	}

	before, _ := env.retryState("idx-a")
	recoveredAt := env.clk.Now()
	env.reconcile()
	after, _ := env.retryState("idx-a")
	if row, _ := env.statusRow("a"); row.Health != string(HealthRecovering) {
		t.Fatalf("health = %s, want recovering", row.Health)
	}
	return env, recoveredAt, after - before
}

// TestCacheRecoveringAccountKeepsOneLadder: in cache mode a recovering account
// still gets exactly the recovery ladder plus the health poll. The
// first-confirmation wave must neither be armed for it nor re-arm (and so
// restart) the recovery ladder.
func TestCacheRecoveringAccountKeepsOneLadder(t *testing.T) {
	env, recoveredAt, cacheDelta := recoveringReconcile(t, true)
	pending := env.timersBeforeNextReconcile()
	assertLadderArmedOnce(t, pending, recoveredAt)
	if got, want := len(pending), len(resetRetryDelays)+1; got != want {
		t.Errorf("pending timers before reconcile-interval = %d, want %d (one ladder + the health poll): %v", got, want, pending)
	}
	if _, timers := env.retryState("idx-a"); timers != len(resetRetryDelays) {
		t.Errorf("ladder timers armed = %d, want %d", timers, len(resetRetryDelays))
	}
	env.eng.mu.Lock()
	polling := env.eng.healthPollTimer != nil
	env.eng.mu.Unlock()
	if !polling {
		t.Error("health poll is not armed for the recovering account")
	}

	_, _, standaloneDelta := recoveringReconcile(t, false)
	if cacheDelta != standaloneDelta {
		t.Errorf("retry generations advanced %d in cache mode, %d standalone; the recovery ladder was re-armed", cacheDelta, standaloneDelta)
	}
	if got := env.claude.callCount(token("a")); got != 0 {
		t.Errorf("cache mode made %d provider calls, want 0", got)
	}
}

// awaitingReconcile confirms account a with a reset at +30m, moves the clock
// past it without firing the deadline timer, and returns the retry-generation
// change across the full reconciliation that discovers the expiry. That pass
// cannot find a new window: the cache still reports the expired reset, and
// standalone mode's provider fails.
func awaitingReconcile(t *testing.T, cacheMode bool) (*testEnv, int) {
	t.Helper()
	cfg := defaultConfig()
	if cacheMode {
		cfg = cacheConfig(t)
	}
	env := newTestEnv(t, cfg)
	deadline := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", deadline)
	env.addAccount("claude", "b", day(2))
	if cacheMode {
		writeCacheEntries(t, cfg.QuotaCachePath, map[string]cacheEntry{
			"a": {resetAt: deadline, observedAt: baseTime},
			"b": {resetAt: day(2), observedAt: baseTime},
		})
	}
	env.reconcile()
	env.clk.Set(deadline.Add(time.Nanosecond))
	if cacheMode {
		writeCacheEntries(t, cfg.QuotaCachePath, map[string]cacheEntry{
			"a": {resetAt: deadline, observedAt: env.clk.Now()},
			"b": {resetAt: day(2), observedAt: env.clk.Now()},
		})
	} else {
		env.claude.setErr(token("a"), errFake("provider unavailable"))
	}

	before, _ := env.retryState("idx-a")
	env.reconcile()
	after, _ := env.retryState("idx-a")
	if row, _ := env.statusRow("a"); row.ResetState != string(ResetAwaitingNewWindow) {
		t.Fatalf("reset state = %s, want awaiting_new_window", row.ResetState)
	}
	return env, after - before
}

// TestCacheAwaitingLadderIsNotRearmed: the first-confirmation wave is
// scheduled after flushAfterFetch has armed an awaiting_new_window ladder. It
// must leave that ladder alone, so the retry generation moves exactly as in
// standalone mode and the ladder's next step still confirms the new window.
func TestCacheAwaitingLadderIsNotRearmed(t *testing.T) {
	env, cacheDelta := awaitingReconcile(t, true)
	_, standaloneDelta := awaitingReconcile(t, false)
	if cacheDelta != standaloneDelta {
		t.Fatalf("retry generations advanced %d in cache mode, %d standalone; the awaiting ladder was re-armed", cacheDelta, standaloneDelta)
	}
	assertLadderArmedOnce(t, env.timersBeforeNextReconcile(), env.clk.Now())

	newWindow := baseTime.Add(30*time.Minute + 7*24*time.Hour)
	writeCacheEntries(t, env.eng.cfg.QuotaCachePath, map[string]cacheEntry{
		"a": {resetAt: newWindow, observedAt: env.clk.Now()},
		"b": {resetAt: day(2), observedAt: env.clk.Now()},
	})
	env.clk.Advance(5 * time.Second)
	env.async.drain()
	if row, _ := env.statusRow("a"); row.ResetState != string(ResetConfirmed) {
		t.Fatalf("reset state after the +5s step = %s, want confirmed", row.ResetState)
	}
	if got := env.claude.callCount(token("a")); got != 0 {
		t.Fatalf("cache mode made %d provider calls, want 0", got)
	}
}

// TestCacheAwaitingImmediateAttemptSurvivesScheduler: when roster discovery
// fails after a missed deadline, flush arms the awaiting ladder with its
// required immediate attempt. The first-confirmation wave is then scheduled in
// the reconcile tail and must not cancel that queued attempt.
func TestCacheAwaitingImmediateAttemptSurvivesScheduler(t *testing.T) {
	env := newTestEnv(t, cacheConfig(t))
	path := env.eng.cfg.QuotaCachePath
	deadline := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", deadline)
	writeCacheEntries(t, path, map[string]cacheEntry{"a": {resetAt: deadline, observedAt: baseTime}})
	env.reconcile()

	env.clk.Set(deadline.Add(time.Nanosecond))
	env.host.mu.Lock()
	env.host.listErr = errFake("roster unavailable")
	env.host.mu.Unlock()
	if result := env.eng.Reconcile(context.Background()); result != ReconcileResultError {
		t.Fatalf("reconcile result = %s, want error", result)
	}
	if pending := env.async.pending(); pending != 1 {
		t.Fatalf("queued immediate attempts = %d, want 1", pending)
	}

	writeCacheEntries(t, path, map[string]cacheEntry{
		"a": {resetAt: deadline.Add(7 * 24 * time.Hour), observedAt: env.clk.Now()},
	})
	env.async.drain()
	if row, _ := env.statusRow("a"); row.ResetState != string(ResetConfirmed) {
		t.Fatalf("reset state after the immediate attempt = %s, want confirmed", row.ResetState)
	}
}

// TestStandaloneUnknownAccountGetsNoLadder: first-confirmation retries exist
// only in cache mode. In standalone mode each attempt would be a provider
// request, so an unknown account waits for reconcile-interval as before.
func TestStandaloneUnknownAccountGetsNoLadder(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.claude.setErr(token("a"), errFake("provider unavailable"))
	env.reconcile()
	if row, _ := env.statusRow("a"); row.ResetState != string(ResetUnknown) {
		t.Fatalf("reset state = %s, want unknown", row.ResetState)
	}
	if pending := env.timersBeforeNextReconcile(); len(pending) != 0 {
		t.Fatalf("standalone unknown account armed timers before reconcile-interval: %v", pending)
	}
	env.clk.Advance(time.Hour - time.Second)
	env.async.drain()
	if got := env.claude.callCount(token("a")); got != 1 {
		t.Fatalf("provider calls before reconcile-interval = %d, want 1 (startup only)", got)
	}
}

// TestCacheLadderGoesQuietAfterStandaloneReconfigure: a first-confirmation
// wave armed in cache mode must not turn into provider requests when the
// operator switches to standalone mode and a wave timer fires before the
// reconfiguration's reconciliation has run.
func TestCacheLadderGoesQuietAfterStandaloneReconfigure(t *testing.T) {
	env := newTestEnv(t, cacheConfig(t))
	env.addAccount("claude", "a", day(1))
	env.reconcile() // no snapshot: a stays unknown
	if timers := env.confirmWaveTimers(); timers != len(resetRetryDelays) {
		t.Fatalf("wave timers armed = %d, want %d", timers, len(resetRetryDelays))
	}

	env.eng.Reconfigure(defaultConfig())
	// Leave the reconfiguration's reconcile queued so the cache-mode wave, not
	// that pass, decides what happens at each step.
	env.clk.Advance(16 * time.Minute)
	if got := env.claude.callCount(token("a")); got != 0 {
		t.Fatalf("cache-mode wave made %d provider calls after switching to standalone, want 0", got)
	}
}

// TestCacheQuarantinedAccountsGetNoWave: quarantined accounts (reauth-required
// or disabled) are not candidates for first confirmation, even though their
// reset state is unknown and they were never observed. With every healthy
// account confirmed, only the health poll is armed before reconcile-interval.
func TestCacheQuarantinedAccountsGetNoWave(t *testing.T) {
	env := newTestEnv(t, cacheConfig(t))
	env.addAccount("claude", "a", day(1))
	env.addAccount("claude", "b", day(2))
	env.addAccount("claude", "c", day(3))
	quarantineAccount(env, "idx-a")
	env.host.updateEntry("idx-c", func(entry *hostapi.AuthEntry) { entry.Disabled = true })
	writeCacheEntries(t, env.eng.cfg.QuotaCachePath, map[string]cacheEntry{"b": {resetAt: day(2), observedAt: baseTime}})
	env.reconcile()

	for _, name := range []string{"a", "c"} {
		if row, _ := env.statusRow(name); row.Health != string(HealthQuarantined) || row.ResetState != string(ResetUnknown) {
			t.Fatalf("%s = %s/%s, want quarantined/unknown", name, row.Health, row.ResetState)
		}
	}
	if timers := env.confirmWaveTimers(); timers != 0 {
		t.Fatalf("quarantined accounts armed %d wave timers, want 0", timers)
	}
	if pending := env.timersBeforeNextReconcile(); len(pending) != 1 {
		t.Fatalf("pending timers before reconcile-interval = %v, want only the health poll", pending)
	}
}

// TestCacheNoWeeklyAnswerEndsWave: an account leaves the wave at its first
// cache answer, including a fresh entry that reports no weekly window. A
// weekly reset that appears later is picked up by the next reconciliation, not
// by the remaining wave steps, and the wave cancels itself once no account is
// unconfirmed.
func TestCacheNoWeeklyAnswerEndsWave(t *testing.T) {
	env := newTestEnv(t, cacheConfig(t))
	path := env.eng.cfg.QuotaCachePath
	env.addAccount("claude", "a", day(1))
	env.reconcile() // no snapshot: a stays unknown and the wave is armed
	start := env.clk.Now()

	env.clk.Advance(time.Second)
	writeCacheEntries(t, path, map[string]cacheEntry{"a": {observedAt: env.clk.Now()}})
	env.clk.AdvanceTo(start.Add(6 * time.Second)) // the +5s step reads the no-weekly answer
	if row, _ := env.statusRow("a"); row.ResetState != string(ResetUnknown) || row.ObservedAt == "" {
		t.Fatalf("after the +5s step: %+v, want unknown with an observation time", row)
	}
	if timers := env.confirmWaveTimers(); timers != 0 {
		t.Fatalf("wave still armed after its only account was answered: %d timers", timers)
	}

	writeCacheEntries(t, path, map[string]cacheEntry{"a": {resetAt: day(1), observedAt: env.clk.Now()}})
	env.clk.AdvanceTo(start.Add(20 * time.Minute))
	if row, _ := env.statusRow("a"); row.ResetState != string(ResetUnknown) {
		t.Fatalf("wave kept reading after a no-weekly answer: reset_state = %s", row.ResetState)
	}
}

// TestCacheRosterErrorPassRearmsWave: a pass whose roster read fails still
// replaces the wave, because its reservation cancels the old one. Its tail
// must arm a new wave, or unconfirmed accounts would wait for
// reconcile-interval.
func TestCacheRosterErrorPassRearmsWave(t *testing.T) {
	env := newTestEnv(t, cacheConfig(t))
	path := env.eng.cfg.QuotaCachePath
	env.addAccount("claude", "a", day(1))
	env.reconcile() // no snapshot: a stays unknown
	env.clk.Advance(time.Minute)

	env.host.mu.Lock()
	env.host.listErr = errFake("roster unavailable")
	env.host.mu.Unlock()
	errorPassAt := env.clk.Now()
	if result := env.eng.Reconcile(context.Background()); result != ReconcileResultError {
		t.Fatalf("reconcile result = %s, want error", result)
	}
	pending := env.timersBeforeNextReconcile()
	assertLadderArmedOnce(t, pending, errorPassAt)
	if len(pending) != len(resetRetryDelays) {
		t.Fatalf("pending timers = %v, want only the new wave (the old one cancelled)", pending)
	}

	env.clk.Advance(time.Second)
	writeCacheEntries(t, path, map[string]cacheEntry{"a": {resetAt: day(1), observedAt: env.clk.Now()}})
	env.clk.AdvanceTo(errorPassAt.Add(5 * time.Second))
	if row, _ := env.statusRow("a"); row.ResetState != string(ResetConfirmed) {
		t.Fatalf("reset state after the new wave's +5s step = %s, want confirmed", row.ResetState)
	}
}

// TestCacheHeldConfirmationArmsItsDeadline: the +5s step confirms d while u is
// still unconfirmed, so the wave holds d's ranking unwritten. d's reset falls
// inside that hold. Holding must still arm d's exact-deadline timer, so at the
// deadline d is demoted and gets its immediate attempt, as any confirmed
// account does.
func TestCacheHeldConfirmationArmsItsDeadline(t *testing.T) {
	env := newTestEnv(t, cacheConfig(t))
	path := env.eng.cfg.QuotaCachePath
	deadline := baseTime.Add(20 * time.Second)
	env.addAccount("claude", "d", deadline)
	env.addAccount("claude", "u", day(2))
	env.addAccount("claude", "x", day(1))
	writeCacheEntries(t, path, map[string]cacheEntry{"x": {resetAt: day(1), observedAt: baseTime}})
	env.reconcile() // x is confirmed; d and u are unknown, so the wave is armed

	env.clk.Advance(3 * time.Second)
	writeCacheEntries(t, path, map[string]cacheEntry{
		"x": {resetAt: day(1), observedAt: baseTime},
		"d": {resetAt: deadline, observedAt: env.clk.Now()},
	})
	env.clk.AdvanceTo(baseTime.Add(5 * time.Second))
	if row, _ := env.statusRow("d"); row.ResetState != string(ResetConfirmed) {
		t.Fatalf("d reset_state after the +5s step = %s, want confirmed", row.ResetState)
	}
	if next := env.eng.Status().NextDeadlineAt; next == nil || !next.Equal(deadline) {
		t.Fatalf("next deadline while d is held = %v, want %v", next, deadline)
	}

	env.clk.AdvanceTo(deadline)
	if row, _ := env.statusRow("d"); row.ResetState != string(ResetAwaitingNewWindow) {
		t.Fatalf("d reset_state at its deadline = %s, want awaiting_new_window", row.ResetState)
	}
	if pending := env.async.pending(); pending != 1 {
		t.Fatalf("queued immediate attempts at d's deadline = %d, want 1", pending)
	}
}

// TestCacheHeldStepAtDeadlineInstantDemotes: b's exact-deadline timer is
// re-armed after the wave's timers, so at b's deadline the +30s step runs
// first. It confirms u1 while u2 is still unconfirmed, which would hold the
// ranking and re-arm the deadline timer. Re-arming considers future resets
// only, so it would cancel b's due timer without demoting b. The step must
// flush instead, which demotes b and arms its retries.
func TestCacheHeldStepAtDeadlineInstantDemotes(t *testing.T) {
	env := newTestEnv(t, cacheConfig(t))
	path := env.eng.cfg.QuotaCachePath
	cDeadline := baseTime.Add(time.Second)
	bDeadline := baseTime.Add(30 * time.Second)
	env.addAccount("claude", "b", bDeadline)
	env.addAccount("claude", "c", cDeadline)
	env.addAccount("claude", "u1", day(1))
	env.addAccount("claude", "u2", day(2))
	entries := map[string]cacheEntry{
		"b": {resetAt: bDeadline, observedAt: baseTime},
		"c": {resetAt: cDeadline, observedAt: baseTime},
	}
	writeCacheEntries(t, path, entries)
	// b and c are confirmed. The deadline timer for c is armed before the wave.
	env.reconcile()
	// c's deadline re-arms the deadline timer, now for b, after the wave's timers.
	env.clk.AdvanceTo(cDeadline)
	env.async.drain()

	env.clk.AdvanceTo(baseTime.Add(10 * time.Second))
	entries["u1"] = cacheEntry{resetAt: day(1), observedAt: env.clk.Now()}
	writeCacheEntries(t, path, entries)
	env.clk.AdvanceTo(bDeadline)
	if row, _ := env.statusRow("u1"); row.ResetState != string(ResetConfirmed) {
		t.Fatalf("u1 reset_state after the +30s step = %s, want confirmed", row.ResetState)
	}
	if row, _ := env.statusRow("b"); row.ResetState != string(ResetAwaitingNewWindow) {
		t.Fatalf("b reset_state at its deadline = %s, want awaiting_new_window", row.ResetState)
	}
	if _, timers := env.retryState("idx-b"); timers != len(resetRetryDelays) {
		t.Fatalf("b retry timers at its deadline = %d, want %d", timers, len(resetRetryDelays))
	}
}
