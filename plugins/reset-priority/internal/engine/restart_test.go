package engine

// Restart convergence in quota-cache mode.
//
// A CPA restart rebuilds engine state: every account starts healthy with
// reset_state unknown. When the startup cache read cannot confirm an account,
// the startup pass ranks it last with the file-name tie-break and writes that
// provisional order. The first-confirmation wave (+5s, +30s, +2m, +5m, +15m)
// then re-reads the local snapshot until Quota Cache has fresh data, instead
// of leaving the provisional order in place until reconcile-interval. The wave
// writes once: when no account is left unconfirmed, or at its +15m step. It
// never persists the partial rankings in between, in which every confirmed
// account would rank above every unconfirmed one.
//
// Fixture: three Claude accounts whose physical priorities already hold the
// correct reset order, as the previous CPA process left them. File-name order
// disagrees with reset order for every account, so any provisional write is
// visible:
//
//	account  weekly reset  correct priority  file-name-order priority
//	alice    +3d           200               300
//	bob      +5d           100               200
//	carol    +1d           300               100

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/config"
	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/hostapi"
)

var restartPool = []struct {
	name      string
	resetDays int
	correct   int
	fileOrder int
}{
	{name: "alice", resetDays: 3, correct: 200, fileOrder: 300},
	{name: "bob", resetDays: 5, correct: 100, fileOrder: 200},
	{name: "carol", resetDays: 1, correct: 300, fileOrder: 100},
}

func restartCorrect() map[string]int {
	out := make(map[string]int, len(restartPool))
	for _, a := range restartPool {
		out[a.name] = a.correct
	}
	return out
}

func restartFileOrder() map[string]int {
	out := make(map[string]int, len(restartPool))
	for _, a := range restartPool {
		out[a.name] = a.fileOrder
	}
	return out
}

// restartEntries returns a fresh quota-cache entry, observed at observedAt,
// for each named pool account (every account when names is empty).
func restartEntries(observedAt time.Time, names ...string) map[string]cacheEntry {
	out := make(map[string]cacheEntry)
	for _, a := range restartPool {
		if len(names) > 0 && !containsName(names, a.name) {
			continue
		}
		out[a.name] = cacheEntry{resetAt: day(a.resetDays), observedAt: observedAt}
	}
	return out
}

func containsName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// newRestartEnv builds a cache-mode engine over the restart pool. Each auth's
// roster entry and file already carry its correct priority. The provider fakes
// also know each reset, so any provider request would succeed; a cache-mode
// engine must still never make one.
func newRestartEnv(t *testing.T) *testEnv {
	t.Helper()
	env := newTestEnv(t, cacheConfig(t))
	for _, a := range restartPool {
		env.host.setEntry(hostapi.AuthEntry{
			AuthIndex: "idx-" + a.name,
			ID:        "id-" + a.name,
			Name:      a.name + ".json",
			Provider:  "claude",
			Type:      "claude",
			Status:    "active",
			Source:    "file",
			Path:      "/auth/" + a.name + ".json",
			Email:     a.name + "@example.com",
			Priority:  a.correct,
		}, map[string]any{
			"type":          "claude",
			"access_token":  token(a.name),
			"refresh_token": "refresh-" + token(a.name),
			"email":         a.name + "@example.com",
			"priority":      a.correct,
		})
		env.claude.setReset(token(a.name), day(a.resetDays))
	}
	return env
}

// restart runs startup as production does: plugin.register -> Engine.Start ->
// RunAsync(Reconcile).
func (env *testEnv) restart() {
	env.t.Helper()
	env.eng.Start()
	env.async.drain()
}

// advanceSinceRestart moves the clock to baseTime+d, firing every due timer.
func (env *testEnv) advanceSinceRestart(d time.Duration) {
	env.t.Helper()
	env.clk.AdvanceTo(baseTime.Add(d))
	env.async.drain()
}

// writeCacheAt schedules one snapshot write on the engine's clock, so it
// interleaves with engine timers in time order. Offsets must not coincide with
// a ladder step: same-instant timers fire in arming order.
func (env *testEnv) writeCacheAt(d time.Duration, entries func(now time.Time) map[string]cacheEntry) {
	env.clk.AfterFunc(baseTime.Add(d).Sub(env.clk.Now()), func() {
		writeCacheEntries(env.t, env.eng.cfg.QuotaCachePath, entries(env.clk.Now()))
	})
}

// keepCacheFresh models Quota Cache's steady state once it has caught up: it
// re-polls every pool account every 15 minutes (its default poll interval),
// starting at baseTime+first.
func (env *testEnv) keepCacheFresh(first time.Duration) {
	const pollInterval = 15 * time.Minute
	var poll func()
	poll = func() {
		writeCacheEntries(env.t, env.eng.cfg.QuotaCachePath, restartEntries(env.clk.Now()))
		env.clk.AfterFunc(pollInterval, poll)
	}
	env.clk.AfterFunc(baseTime.Add(first).Sub(env.clk.Now()), poll)
}

func (env *testEnv) assertResetStates(want ResetState) {
	env.t.Helper()
	for _, a := range restartPool {
		if row, _ := env.statusRow(a.name); row.ResetState != string(want) {
			env.t.Errorf("%s reset_state = %s, want %s", a.name, row.ResetState, want)
		}
	}
}

func (env *testEnv) restartProviderCalls() int {
	total := 0
	for _, a := range restartPool {
		total += env.claude.callCount(token(a.name))
	}
	return total
}

// assertSettled checks the converged steady state: correct physical order,
// every account confirmed, no wave left armed, no saves or extra
// reconciliations since savesAt, and no provider traffic at all.
func (env *testEnv) assertSettled(label string, savesAt int) {
	env.t.Helper()
	env.assertPhysical(restartCorrect())
	env.assertResetStates(ResetConfirmed)
	if timers := env.confirmWaveTimers(); timers != 0 {
		env.t.Errorf("%s: first-confirmation wave still armed (%d timers)", label, timers)
	}
	if got := env.host.saveCount() - savesAt; got != 0 {
		env.t.Errorf("%s: %d saves after convergence, want 0", label, got)
	}
	if got := env.host.listCount(); got != 1 {
		env.t.Errorf("%s: reconciles = %d, want 1 (startup only)", label, got)
	}
	if got := env.restartProviderCalls(); got != 0 {
		env.t.Errorf("%s: cache mode made %d provider calls, want 0", label, got)
	}
}

// TestRestartStaleSnapshotConvergesOnLadder: Quota Cache's entries are 40
// minutes old at restart (older than ReadFresh's 30m limit), and it catches up
// at +20s. Startup writes the provisional file-name order; the +30s ladder step
// restores the correct order, and nothing is written after that.
func TestRestartStaleSnapshotConvergesOnLadder(t *testing.T) {
	env := newRestartEnv(t)
	writeCacheEntries(t, env.eng.cfg.QuotaCachePath, restartEntries(baseTime.Add(-40*time.Minute)))
	env.restart()

	// Documented provisional behavior: unconfirmed accounts rank last with the
	// stable tie-break, and that order is written.
	env.assertResetStates(ResetUnknown)
	env.assertPhysical(restartFileOrder())

	env.keepCacheFresh(20 * time.Second)
	env.advanceSinceRestart(29 * time.Second)
	env.assertPhysical(restartFileOrder()) // the +5s read still saw stale data

	env.advanceSinceRestart(30 * time.Second)
	env.assertPhysical(restartCorrect())
	env.assertResetStates(ResetConfirmed)
	t.Logf("saves through the +30s step, startup included: %d", env.host.saveCount())

	savesAt := env.host.saveCount()
	env.advanceSinceRestart(59 * time.Minute)
	env.assertSettled("+59m", savesAt)
}

// TestRestartBootstrapWipeConvergesOnLadder models quota-cache's bootstrap
// wipe: at restart its snapshot has no entries, then they come back about one
// per 15 seconds. carol, whose reset is soonest, returns last. The ladder
// confirms every account by the +2m step.
func TestRestartBootstrapWipeConvergesOnLadder(t *testing.T) {
	env := newRestartEnv(t)
	writeCacheEntries(t, env.eng.cfg.QuotaCachePath, map[string]cacheEntry{})
	env.restart()
	env.assertResetStates(ResetUnknown)
	env.assertPhysical(restartFileOrder())

	rebuilt := map[string]cacheEntry{}
	for i, name := range []string{"bob", "alice", "carol"} {
		env.writeCacheAt(10*time.Second+time.Duration(i)*15*time.Second, func(now time.Time) map[string]cacheEntry {
			rebuilt[name] = restartEntries(now, name)[name]
			return rebuilt
		})
	}
	env.keepCacheFresh(15*time.Minute + 10*time.Second)

	// +30s: bob and alice are back and confirmed, but carol is not, so the
	// wave holds their order and the provisional one stays written.
	env.advanceSinceRestart(30 * time.Second)
	for name, want := range map[string]ResetState{"alice": ResetConfirmed, "bob": ResetConfirmed, "carol": ResetUnknown} {
		if row, _ := env.statusRow(name); row.ResetState != string(want) {
			t.Errorf("+30s: %s reset_state = %s, want %s", name, row.ResetState, want)
		}
	}
	env.assertPhysical(map[string]int{"alice": 300, "bob": 200, "carol": 100})

	env.advanceSinceRestart(2 * time.Minute)
	env.assertPhysical(restartCorrect())
	env.assertResetStates(ResetConfirmed)

	savesAt := env.host.saveCount()
	env.advanceSinceRestart(59 * time.Minute)
	env.assertSettled("+59m", savesAt)
}

// TestRestartRefreshPendingEntryCorrectedAtLastStep: quota-cache was stopped
// mid-poll, so carol's entry still carries last_error "refresh pending", which
// ReadFresh rejects. carol has the soonest reset, so ranking her last moves all
// three priorities. The entry clears at +10m and the +15m step corrects it.
func TestRestartRefreshPendingEntryCorrectedAtLastStep(t *testing.T) {
	env := newRestartEnv(t)
	entries := restartEntries(baseTime.Add(-2*time.Minute), "alice", "bob")
	entries["carol"] = cacheEntry{resetAt: day(1), observedAt: baseTime.Add(-10 * time.Minute), lastError: "refresh pending"}
	writeCacheEntries(t, env.eng.cfg.QuotaCachePath, entries)
	env.restart()

	wrong := map[string]int{"alice": 300, "bob": 200, "carol": 100}
	env.assertPhysical(wrong)
	if row, _ := env.statusRow("carol"); row.ResetState != string(ResetUnknown) {
		t.Fatalf("carol reset_state = %s at startup, want unknown", row.ResetState)
	}

	env.keepCacheFresh(10 * time.Minute)
	env.advanceSinceRestart(14 * time.Minute)
	env.assertPhysical(wrong) // +5s, +30s, +2m, and +5m all saw refresh pending

	env.advanceSinceRestart(15 * time.Minute)
	env.assertPhysical(restartCorrect())
	env.assertResetStates(ResetConfirmed)

	savesAt := env.host.saveCount()
	env.advanceSinceRestart(59 * time.Minute)
	env.assertSettled("+59m", savesAt)
}

// TestRestartUnconfirmedAccountDefersWriteToLastStep: after the restart,
// alice's entry keeps failing, while bob's and carol's come back at +3s. The
// +5s step confirms bob and carol but holds their order, because alice could
// still confirm at a later step. The +15m step writes the order of the
// confirmed accounts once, with alice last, and ends the wave. alice then
// waits for the next reconciliation.
func TestRestartUnconfirmedAccountDefersWriteToLastStep(t *testing.T) {
	env := newRestartEnv(t)
	writeCacheEntries(t, env.eng.cfg.QuotaCachePath, restartEntries(baseTime.Add(-40*time.Minute)))
	env.restart()
	env.assertPhysical(restartFileOrder())
	startupSaves := env.host.saveCount()

	env.writeCacheAt(3*time.Second, func(now time.Time) map[string]cacheEntry {
		entries := restartEntries(now, "bob", "carol")
		entries["alice"] = cacheEntry{resetAt: day(3), observedAt: now, lastError: "refresh failed"}
		return entries
	})
	env.advanceSinceRestart(15*time.Minute - time.Second)
	for name, want := range map[string]ResetState{"alice": ResetUnknown, "bob": ResetConfirmed, "carol": ResetConfirmed} {
		if row, _ := env.statusRow(name); row.ResetState != string(want) {
			t.Errorf("before +15m: %s reset_state = %s, want %s", name, row.ResetState, want)
		}
	}
	env.assertPhysical(restartFileOrder())
	if got := env.host.saveCount() - startupSaves; got != 0 {
		t.Fatalf("saves before the +15m step = %d, want 0 (the wave holds a partial ranking)", got)
	}

	env.advanceSinceRestart(15 * time.Minute)
	env.assertPhysical(map[string]int{"carol": 300, "bob": 200, "alice": 100})
	for _, a := range restartPool {
		if got := len(env.host.savesFor(a.name+".json")) - 1; got > 1 {
			t.Errorf("%s saved %d times after startup, want at most 1", a.name, got)
		}
	}
	if timers := env.confirmWaveTimers(); timers != 0 {
		t.Errorf("wave still armed after its last step (%d timers)", timers)
	}
	if got := env.restartProviderCalls(); got != 0 {
		t.Errorf("cache mode made %d provider calls, want 0", got)
	}
}

// TestRestartFreshSnapshotArmsNoLadder is the control: every entry is fresh
// at restart, so startup confirms every account, writes nothing, and arms no
// wave or ladder.
func TestRestartFreshSnapshotArmsNoLadder(t *testing.T) {
	env := newRestartEnv(t)
	writeCacheEntries(t, env.eng.cfg.QuotaCachePath, restartEntries(baseTime.Add(-2*time.Minute)))
	env.restart()

	env.assertResetStates(ResetConfirmed)
	if got := env.host.saveCount(); got != 0 {
		t.Fatalf("startup saves = %d, want 0", got)
	}
	if timers := env.confirmWaveTimers(); timers != 0 {
		t.Errorf("armed %d wave timers, want 0", timers)
	}
	for _, a := range restartPool {
		if _, timers := env.retryState("idx-" + a.name); timers != 0 {
			t.Errorf("%s armed %d ladder timers, want 0", a.name, timers)
		}
	}
	if pending := env.timersBeforeNextReconcile(); len(pending) != 0 {
		t.Fatalf("timers armed before reconcile-interval: %v", pending)
	}

	env.keepCacheFresh(13 * time.Minute)
	env.advanceSinceRestart(59 * time.Minute)
	env.assertSettled("+59m", 0)
}

// Write volume at convergence. These tests use a larger pool, accounts a00 to
// a(n-1), so per-account write amplification would be visible.

const poolSize = 10

// poolName returns pool account i's file-name stem. File-name order is index
// order.
func poolName(i int) string { return fmt.Sprintf("a%02d", i) }

// newPoolEnv builds an engine over poolSize Claude accounts. Account i resets
// in resetDay(i) days, and its roster entry and file carry priority(i).
func newPoolEnv(t *testing.T, cfg config.Config, resetDay, priority func(i int) int) *testEnv {
	t.Helper()
	env := newTestEnv(t, cfg)
	for i := 0; i < poolSize; i++ {
		name := poolName(i)
		env.host.setEntry(hostapi.AuthEntry{
			AuthIndex: "idx-" + name,
			ID:        "id-" + name,
			Name:      name + ".json",
			Provider:  "claude",
			Type:      "claude",
			Status:    "active",
			Source:    "file",
			Path:      "/auth/" + name + ".json",
			Priority:  priority(i),
		}, map[string]any{
			"type":          "claude",
			"access_token":  token(name),
			"refresh_token": "refresh-" + token(name),
			"priority":      priority(i),
		})
		env.claude.setReset(token(name), day(resetDay(i)))
	}
	return env
}

func poolEntries(resetDay func(i int) int, observedAt time.Time) map[string]cacheEntry {
	out := make(map[string]cacheEntry, poolSize)
	for i := 0; i < poolSize; i++ {
		out[poolName(i)] = cacheEntry{resetAt: day(resetDay(i)), observedAt: observedAt}
	}
	return out
}

func poolPriorities(priority func(i int) int) map[string]int {
	out := make(map[string]int, poolSize)
	for i := 0; i < poolSize; i++ {
		out[poolName(i)] = priority(i)
	}
	return out
}

// TestRestartConvergenceWritesEachAccountOnce: reset order is the exact
// inverse of file-name order, so the provisional startup order moves every
// account and convergence moves every account back. The converging wave step
// must save each account once. Writing every partial ranking as accounts
// confirm one at a time would cost several times that.
func TestRestartConvergenceWritesEachAccountOnce(t *testing.T) {
	resetDay := func(i int) int { return poolSize - i }          // a(n-1) resets first
	correct := func(i int) int { return 100 + 100*i }            // so it ranks highest
	fileOrder := func(i int) int { return 100 * (poolSize - i) } // a00 ranks highest
	env := newPoolEnv(t, cacheConfig(t), resetDay, correct)
	path := env.eng.cfg.QuotaCachePath
	writeCacheEntries(t, path, poolEntries(resetDay, baseTime.Add(-40*time.Minute)))
	env.restart()

	env.assertPhysical(poolPriorities(fileOrder))
	if got := env.host.saveCount(); got != poolSize {
		t.Fatalf("provisional startup saves = %d, want %d", got, poolSize)
	}

	env.writeCacheAt(20*time.Second, func(now time.Time) map[string]cacheEntry { return poolEntries(resetDay, now) })
	env.advanceSinceRestart(29 * time.Second)
	if got := env.host.saveCount(); got != poolSize {
		t.Fatalf("saves after the +5s step, which only missed = %d, want %d", got, poolSize)
	}

	env.advanceSinceRestart(30 * time.Second)
	env.assertPhysical(poolPriorities(correct))
	if got := env.host.saveCount() - poolSize; got != poolSize {
		t.Fatalf("saves at the converging +30s step = %d, want %d (one per account)", got, poolSize)
	}
	if timers := env.confirmWaveTimers(); timers != 0 {
		t.Fatalf("wave still armed after every account confirmed (%d timers)", timers)
	}

	savesAt := env.host.saveCount()
	env.advanceSinceRestart(59 * time.Minute)
	if got := env.host.saveCount() - savesAt; got != 0 {
		t.Fatalf("%d saves after convergence, want 0", got)
	}
	for i := 0; i < poolSize; i++ {
		if got := env.claude.callCount(token(poolName(i))); got != 0 {
			t.Fatalf("cache mode made %d provider calls for %s, want 0", got, poolName(i))
		}
	}
}

// TestRestartCorrectOrderWritesNothing: file-name order matches reset order
// and the physical priorities already hold it, so the provisional startup order
// is the correct one. Convergence must then write nothing at all, as before
// first-confirmation retries existed. Confirming accounts one at a time and
// writing each partial ranking would shuffle the pool and restore it.
func TestRestartCorrectOrderWritesNothing(t *testing.T) {
	resetDay := func(i int) int { return i + 1 }               // a00 resets first
	correct := func(i int) int { return 100 * (poolSize - i) } // so it ranks highest
	env := newPoolEnv(t, cacheConfig(t), resetDay, correct)
	path := env.eng.cfg.QuotaCachePath
	writeCacheEntries(t, path, poolEntries(resetDay, baseTime.Add(-40*time.Minute)))
	env.restart()

	env.writeCacheAt(20*time.Second, func(now time.Time) map[string]cacheEntry { return poolEntries(resetDay, now) })
	env.advanceSinceRestart(59 * time.Minute)
	for i := 0; i < poolSize; i++ {
		if row, _ := env.statusRow(poolName(i)); row.ResetState != string(ResetConfirmed) {
			t.Fatalf("%s reset_state = %s, want confirmed", poolName(i), row.ResetState)
		}
	}
	env.assertPhysical(poolPriorities(correct))
	if got := env.host.saveCount(); got != 0 {
		t.Fatalf("saves through +59m = %d, want 0", got)
	}
}

// TestRestartStaggeredRefillWritesNothing: as in
// TestRestartCorrectOrderWritesNothing the provisional order is already
// correct, but Quota Cache refills its entries over two minutes, latest resets
// first, as after its bootstrap wipe. The +5s, +30s and +2m steps each confirm
// one batch. Writing each partial ranking would put the latest resets on top
// and reshuffle the whole pool twice before restoring it.
func TestRestartStaggeredRefillWritesNothing(t *testing.T) {
	resetDay := func(i int) int { return i + 1 }               // a00 resets first
	correct := func(i int) int { return 100 * (poolSize - i) } // so it ranks highest
	env := newPoolEnv(t, cacheConfig(t), resetDay, correct)
	writeCacheEntries(t, env.eng.cfg.QuotaCachePath, poolEntries(resetDay, baseTime.Add(-40*time.Minute)))
	env.restart()

	refilled := map[string]cacheEntry{}
	for _, batch := range []struct {
		at       time.Duration
		from, to int
	}{
		{at: 3 * time.Second, from: 7, to: poolSize}, // confirmed at +5s
		{at: 28 * time.Second, from: 3, to: 7},       // confirmed at +30s
		{at: 118 * time.Second, from: 0, to: 3},      // confirmed at +2m
	} {
		env.writeCacheAt(batch.at, func(now time.Time) map[string]cacheEntry {
			for i := batch.from; i < batch.to; i++ {
				refilled[poolName(i)] = cacheEntry{resetAt: day(resetDay(i)), observedAt: now}
			}
			return refilled
		})
	}

	env.advanceSinceRestart(31 * time.Second)
	for i := 3; i < poolSize; i++ {
		if row, _ := env.statusRow(poolName(i)); row.ResetState != string(ResetConfirmed) {
			t.Fatalf("+31s: %s reset_state = %s, want confirmed", poolName(i), row.ResetState)
		}
	}
	if got := env.host.saveCount(); got != 0 {
		t.Fatalf("saves through the +30s step = %d, want 0 (partial rankings written)", got)
	}

	env.advanceSinceRestart(2*time.Minute + time.Second)
	for i := 0; i < poolSize; i++ {
		if row, _ := env.statusRow(poolName(i)); row.ResetState != string(ResetConfirmed) {
			t.Fatalf("+2m1s: %s reset_state = %s, want confirmed", poolName(i), row.ResetState)
		}
	}
	if timers := env.confirmWaveTimers(); timers != 0 {
		t.Fatalf("wave still armed after every account confirmed (%d timers)", timers)
	}
	env.advanceSinceRestart(59 * time.Minute)
	env.assertPhysical(poolPriorities(correct))
	if got := env.host.saveCount(); got != 0 {
		t.Fatalf("saves through +59m = %d, want 0", got)
	}
	for i := 0; i < poolSize; i++ {
		if got := env.claude.callCount(token(poolName(i))); got != 0 {
			t.Fatalf("cache mode made %d provider calls for %s, want 0", got, poolName(i))
		}
	}
}

// TestCacheDryRunWaveLogsNoProposalsWhileCacheMisses: dry-run never records
// a write, so every flush logs a "would set priority" line per mismatched
// account. A wave step whose reads all miss changes no ranking input and must
// not flush. With no snapshot (Quota Cache not installed yet, or a wrong
// quota-cache-path), the first hour then logs exactly the startup proposals.
func TestCacheDryRunWaveLogsNoProposalsWhileCacheMisses(t *testing.T) {
	cfg := cacheConfig(t) // the snapshot is never written
	cfg.DryRun = true
	env := newPoolEnv(t, cfg, func(i int) int { return i + 1 }, func(int) int { return 1 })
	var mu sync.Mutex
	proposals := 0
	// Install the logger before the engine does any work; nothing runs yet.
	env.eng.logf = func(_, message string) {
		if strings.Contains(message, "would set priority") {
			mu.Lock()
			proposals++
			mu.Unlock()
		}
	}
	env.restart()
	env.advanceSinceRestart(59 * time.Minute)

	mu.Lock()
	defer mu.Unlock()
	if proposals != poolSize {
		t.Fatalf("dry-run proposals logged through +59m = %d, want %d (startup only)", proposals, poolSize)
	}
	if got := env.host.saveCount(); got != 0 {
		t.Fatalf("dry-run saved %d times, want 0", got)
	}
}
