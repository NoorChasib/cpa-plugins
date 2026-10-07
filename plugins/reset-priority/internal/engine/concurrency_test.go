package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/hostapi"
	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/providers"
)

// gateProvider blocks every fetch until release is closed, signalling each
// start. It honors ctx so a hung test cannot deadlock forever.
type gateProvider struct {
	id      string
	token   string
	started chan struct{}
	release chan struct{}
	result  func() (providers.Observation, error)
}

func (p *gateProvider) ID() string { return p.id }

func (p *gateProvider) FetchWeeklyReset(ctx context.Context, creds providers.Credentials) (providers.Observation, error) {
	if p.token != "" && creds.AccessToken != p.token {
		return p.result()
	}
	select {
	case p.started <- struct{}{}:
	default:
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return providers.Observation{}, ctx.Err()
	}
	return p.result()
}

func waitForReconcileReservation(t *testing.T, eng *Engine, authIndex string) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		eng.mu.Lock()
		acct := eng.accounts[authIndex]
		reserved := acct != nil && acct.reconcileFetchPending
		eng.mu.Unlock()
		if reserved {
			return
		}
		select {
		case <-ticker.C:
		case <-timer.C:
			t.Fatalf("reconcile did not reserve provider fetch for %s before blocking", authIndex)
		}
	}
}

// TestReconcileInvocationsSerialized: overlapping full reconciles (e.g.
// hourly tick plus management refresh) must run one at a time.
func TestReconcileInvocationsSerialized(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))

	gate := &gateProvider{
		id:      "claude",
		started: make(chan struct{}, 8),
		release: make(chan struct{}),
		result: func() (providers.Observation, error) {
			return providers.Observation{HasWeekly: true, ResetAt: day(1), ObservedAt: env.clk.Now()}, nil
		},
	}
	env.eng.providers = map[string]providers.Provider{"claude": gate}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		env.eng.Reconcile(context.Background())
	}()
	<-gate.started // first reconcile is inside its provider fetch

	go func() {
		defer wg.Done()
		env.eng.Reconcile(context.Background())
	}()
	// Give the second reconcile a chance to (incorrectly) start: with
	// serialization it must still be waiting before its roster listing.
	time.Sleep(50 * time.Millisecond)
	if got := env.host.listCount(); got != 1 {
		t.Fatalf("roster listed %d times while a reconcile was in flight, want 1 (not serialized)", got)
	}

	close(gate.release)
	wg.Wait()
	if got := env.host.listCount(); got != 2 {
		t.Fatalf("roster listed %d times after both reconciles, want 2", got)
	}
	env.assertDesired(map[string]int{"a": 100})
}

func TestReconcileReservesFetchBeforeWaitingForWriteback(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.reconcile()

	// Model an exact-deadline flush already holding the writeback lane when a
	// management reconciliation begins. The full pass must reserve its provider
	// attempt before it waits on that local lock.
	env.eng.writeMu.Lock()
	var unlockOnce sync.Once
	unlockWriteback := func() { unlockOnce.Do(env.eng.writeMu.Unlock) }
	defer unlockWriteback()
	done := make(chan struct{})
	go func() {
		env.eng.Reconcile(context.Background())
		close(done)
	}()

	waitForReconcileReservation(t, env.eng, "idx-a")
	unlockWriteback()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reconcile did not finish after writeback lane release")
	}
}

// TestDeadlineDemotionNotBlockedByReconcileNetworkFetch: while a full
// reconcile is stuck in provider network work, the exact deadline event must
// still demote and write back synchronously.
func TestDeadlineDemotionNotBlockedByReconcileNetworkFetch(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	deadline := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", deadline)
	env.addAccount("claude", "b", deadline.Add(24*time.Hour))
	env.reconcile()
	env.assertDesired(map[string]int{"a": 200, "b": 100})

	// Swap in a blocking provider and start a reconcile that hangs in the
	// fetch phase (holding the reconcile mutex).
	gate := &gateProvider{
		id:      "claude",
		token:   token("a"),
		started: make(chan struct{}, 8),
		release: make(chan struct{}),
		result: func() (providers.Observation, error) {
			return providers.Observation{}, errFake("late response")
		},
	}
	env.eng.providers = map[string]providers.Provider{"claude": gate}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		env.eng.Reconcile(context.Background())
	}()
	<-gate.started

	// The deadline fires while the reconcile is still blocked: demotion and
	// writeback must complete immediately.
	env.clk.Advance(30 * time.Minute)
	env.assertDesired(map[string]int{"b": 200, "a": 100})
	env.assertPhysical(map[string]int{"b": 200, "a": 100})
	row, _ := env.statusRow("a")
	if row.ResetState != "awaiting_new_window" {
		t.Errorf("account a reset state = %s, want awaiting_new_window", row.ResetState)
	}

	close(gate.release)
	wg.Wait()
	// The late fetch failure cannot re-promote a, and because that request began
	// before demotion it cannot count as the required post-demotion immediate
	// attempt. One fresh asynchronous attempt must be queued after writeback.
	env.assertDesired(map[string]int{"b": 200, "a": 100})
	if pending := env.async.pending(); pending != 1 {
		t.Fatalf("pre-deadline reconcile result queued %d post-demotion immediate attempts, want 1", pending)
	}
}

func TestRetryCollectedBeforeReconcileReservationCannotArmAfterward(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	deadline := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", deadline)
	env.addAccount("claude", "b", deadline.Add(24*time.Hour))
	env.reconcile()
	env.clk.Set(deadline.Add(-time.Nanosecond))

	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	var blockMu sync.Mutex
	blocked := false
	env.host.beforeSave = func(string) {
		blockMu.Lock()
		if blocked {
			blockMu.Unlock()
			return
		}
		blocked = true
		blockMu.Unlock()
		close(writeStarted)
		<-releaseWrite
	}
	var releaseWriteOnce sync.Once
	allowWrite := func() { releaseWriteOnce.Do(func() { close(releaseWrite) }) }
	defer allowWrite()

	gate := &gateProvider{
		id:      "claude",
		token:   token("a"),
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
		result: func() (providers.Observation, error) {
			return providers.Observation{}, errFake("provider unavailable")
		},
	}
	env.eng.providers = map[string]providers.Provider{"claude": gate}
	var releaseProviderOnce sync.Once
	allowProvider := func() { releaseProviderOnce.Do(func() { close(gate.release) }) }
	defer allowProvider()

	deadlineDone := make(chan struct{})
	go func() {
		env.clk.Advance(time.Nanosecond)
		close(deadlineDone)
	}()
	select {
	case <-writeStarted:
	case <-time.After(time.Second):
		t.Fatal("deadline flush did not reach writeback")
	}

	reconcileDone := make(chan struct{})
	go func() {
		env.eng.Reconcile(context.Background())
		close(reconcileDone)
	}()
	waitForReconcileReservation(t, env.eng, "idx-a")
	allowWrite()
	select {
	case <-deadlineDone:
	case <-time.After(time.Second):
		t.Fatal("deadline flush did not finish after write release")
	}
	select {
	case <-gate.started:
	case <-time.After(time.Second):
		t.Fatal("reconcile did not reach its provider call")
	}

	// The deadline flush collected its retry before writeback, but the later
	// full-pass reservation invalidated that schedule. It must not arm a detached
	// immediate task after writeback completes.
	if pending := env.async.pending(); pending != 0 {
		t.Fatalf("stale collected retry armed %d detached immediate tasks, want 0", pending)
	}

	allowProvider()
	select {
	case <-reconcileDone:
	case <-time.After(time.Second):
		t.Fatal("reconcile did not finish after provider release")
	}
}

func TestReconcilePreadmissionFailureRestoresCollectedImmediateRetry(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	deadline := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", deadline)
	env.addAccount("claude", "b", deadline.Add(24*time.Hour))
	env.reconcile()
	env.clk.Set(deadline.Add(-time.Nanosecond))

	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	var blockMu sync.Mutex
	blocked := false
	env.host.beforeSave = func(string) {
		blockMu.Lock()
		if blocked {
			blockMu.Unlock()
			return
		}
		blocked = true
		blockMu.Unlock()
		close(writeStarted)
		<-releaseWrite
	}
	var releaseWriteOnce sync.Once
	allowWrite := func() { releaseWriteOnce.Do(func() { close(releaseWrite) }) }
	defer allowWrite()

	deadlineDone := make(chan struct{})
	go func() {
		env.clk.Advance(time.Nanosecond)
		close(deadlineDone)
	}()
	select {
	case <-writeStarted:
	case <-time.After(time.Second):
		t.Fatal("deadline flush did not reach writeback")
	}

	// Let roster validation succeed, then fail A's second reconcile read—the
	// credential read immediately before provider admission.
	var readMu sync.Mutex
	aReads := 0
	env.host.beforeGet = func(authIndex string) {
		if authIndex != "idx-a" {
			return
		}
		readMu.Lock()
		aReads++
		fail := aReads == 2
		readMu.Unlock()
		if fail {
			env.host.mu.Lock()
			env.host.getErr[authIndex] = errFake("credential read unavailable")
			env.host.mu.Unlock()
		}
	}

	reconcileDone := make(chan struct{})
	go func() {
		env.eng.Reconcile(context.Background())
		close(reconcileDone)
	}()
	waitForReconcileReservation(t, env.eng, "idx-a")
	allowWrite()
	select {
	case <-deadlineDone:
	case <-time.After(time.Second):
		t.Fatal("deadline flush did not finish after write release")
	}
	select {
	case <-reconcileDone:
	case <-time.After(time.Second):
		t.Fatal("reconcile did not finish after credential read failure")
	}

	// The stale retry collected by the deadline flush was invalidated by the full
	// pass, but that replacement never reached provider admission. A new immediate
	// post-demotion attempt must therefore be queued, not deferred to +5s.
	if pending := env.async.pending(); pending != 1 {
		t.Fatalf("preadmission reconcile failure queued %d immediate retries, want 1", pending)
	}
}

// TestDeadlineCrossedDuringWritebackGetsSecondSynchronousPass verifies that a
// deadline which passes inside host.auth.save is not lost when the old timer is
// replaced at the end of the flush. The newly expired account must be demoted
// and persisted before deadline-triggered provider work is armed.
func TestDeadlineCrossedDuringWritebackGetsSecondSynchronousPass(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	deadline := baseTime.Add(30 * time.Minute)
	env.addAccount("claude", "a", deadline)
	env.addAccount("claude", "b", deadline.Add(24*time.Hour))
	env.reconcile()
	env.assertPhysical(map[string]int{"a": 200, "b": 100})

	// Create unrelated physical drift so the next reconcile has a write pass
	// while the reset deadline is still nominally in the future.
	env.host.mu.Lock()
	for i := range env.host.entries {
		if env.host.entries[i].AuthIndex == "idx-b" {
			env.host.entries[i].Priority = 999
		}
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(env.host.docs["idx-b"], &doc); err != nil {
		env.host.mu.Unlock()
		t.Fatal(err)
	}
	doc["priority"] = json.RawMessage(`999`)
	raw, err := json.Marshal(doc)
	if err != nil {
		env.host.mu.Unlock()
		t.Fatal(err)
	}
	env.host.docs["idx-b"] = raw
	env.host.mu.Unlock()

	// This reconcile performs two validation reads and two provider reads before
	// its first read-before-write. Move wall time across A's deadline during that
	// write without firing the existing timer callback.
	var hookMu sync.Mutex
	getNumber := 0
	env.host.beforeGet = func(string) {
		hookMu.Lock()
		defer hookMu.Unlock()
		getNumber++
		if getNumber == 5 {
			env.clk.Set(deadline.Add(time.Millisecond))
		}
	}
	env.reconcile()

	env.assertDesired(map[string]int{"b": 200, "a": 100})
	env.assertPhysical(map[string]int{"b": 200, "a": 100})
	row, _ := env.statusRow("a")
	if row.ResetState != string(ResetAwaitingNewWindow) {
		t.Fatalf("account a reset state = %s, want awaiting_new_window", row.ResetState)
	}
}

// TestPerformWritesSkipsStaleOrRemovedPlanEntries: a write plan computed
// before a newer pass changed desired priorities must not overwrite the
// newer values, and entries for removed accounts are skipped entirely.
func TestPerformWritesSkipsStaleOrRemovedPlanEntries(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.addAccount("claude", "b", day(2))
	env.reconcile()
	env.assertPhysical(map[string]int{"a": 200, "b": 100})
	savesBefore := env.host.saveCount()
	getsBefore := len(env.host.getCalls)

	// Stale entry: planned desired no longer matches live desired.
	stale := []writeItem{{authIndex: "idx-a", provider: "claude", desired: 999, health: HealthHealthy}}
	env.eng.performWrites(context.Background(), stale)
	if got := env.host.saveCount(); got != savesBefore {
		t.Errorf("stale plan entry executed: %d saves, want %d", got, savesBefore)
	}
	env.assertPhysical(map[string]int{"a": 200})

	// Removed account: skipped without even reading the credential.
	removed := []writeItem{{authIndex: "idx-gone", provider: "claude", desired: 100, health: HealthHealthy}}
	env.eng.performWrites(context.Background(), removed)
	for _, idx := range env.host.getCalls[getsBefore:] {
		if idx == "idx-gone" {
			t.Errorf("removed account was read during writeback")
		}
	}

	// A plan entry matching live desired still executes.
	env.eng.mu.Lock()
	env.eng.accounts["idx-a"].desired = 300
	env.eng.mu.Unlock()
	fresh := []writeItem{{authIndex: "idx-a", provider: "claude", desired: 300, health: HealthHealthy}}
	env.eng.performWrites(context.Background(), fresh)
	env.assertPhysical(map[string]int{"a": 300})
}

// hangingCaller blocks host.http.do until released, proving that
// request-timeout bounds provider fetches end-to-end through the bridge.
type hangingCaller struct {
	release chan struct{}
}

func (c *hangingCaller) Call(method string, request []byte) ([]byte, error) {
	if method != hostapi.MethodHostHTTPDo {
		return nil, fmt.Errorf("unexpected host callback %s", method)
	}
	<-c.release
	return nil, fmt.Errorf("released")
}

// TestRequestTimeoutBoundsProviderFetchEndToEnd: a hung upstream must not
// hang reconciliation; the configured request-timeout applies through
// engine -> provider -> bridge, including fetches started from
// context.Background (deadline/recovery retries use the same fetchOne path).
func TestRequestTimeoutBoundsProviderFetchEndToEnd(t *testing.T) {
	cfg := defaultConfig()
	cfg.RequestTimeout = 50 * time.Millisecond
	env := newTestEnv(t, cfg)
	env.addAccount("claude", "a", day(1))

	caller := &hangingCaller{release: make(chan struct{})}
	t.Cleanup(func() { close(caller.release) })
	bridge := hostapi.NewBridge(caller)
	env.eng.providers = map[string]providers.Provider{
		"claude": providers.NewClaude(bridge, env.clk.Now),
	}

	done := make(chan struct{})
	go func() {
		env.eng.Reconcile(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("reconcile did not return; request-timeout not applied end-to-end")
	}

	row, _ := env.statusRow("a")
	if row.ResetState != "unknown" {
		t.Errorf("reset state = %s, want unknown after timeout", row.ResetState)
	}
	if !strings.Contains(row.LastError, "context deadline exceeded") {
		t.Errorf("last error = %q, want a deadline error", row.LastError)
	}
	// The account still counts and holds the floor.
	env.assertDesired(map[string]int{"a": 100})
}

// TestAllFetchPathsShareGlobalConcurrencyLimit drives fetchOne directly, as
// deadline and recovery retry callbacks do. No more than four provider calls
// may enter even when many retry-style fetches overlap outside fetchAll.
func TestAllFetchPathsShareGlobalConcurrencyLimit(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	const accounts = 8
	for i := 0; i < accounts; i++ {
		env.addAccount("claude", fmt.Sprintf("account-%d", i), day(i+1))
	}
	env.reconcile()

	gate := &gateProvider{
		id:      "claude",
		started: make(chan struct{}, accounts),
		release: make(chan struct{}),
		result: func() (providers.Observation, error) {
			return providers.Observation{HasWeekly: true, ResetAt: day(9), ObservedAt: env.clk.Now()}, nil
		},
	}
	env.eng.providers = map[string]providers.Provider{"claude": gate}

	var wg sync.WaitGroup
	for i := 0; i < accounts; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			env.eng.fetchOne(context.Background(), fmt.Sprintf("idx-account-%d", index))
		}(i)
	}
	for i := 0; i < maxConcurrentFetches; i++ {
		select {
		case <-gate.started:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d provider calls entered, want %d", i, maxConcurrentFetches)
		}
	}
	select {
	case <-gate.started:
		t.Fatalf("a fifth provider call entered before one of the first four completed")
	case <-time.After(50 * time.Millisecond):
	}

	close(gate.release)
	wg.Wait()
}

// TestOlderDisableCannotStopNewerEnable reproduces a disable whose Stop waits
// behind a write while a later enabled configuration is published. The older
// call must not quiesce the new generation after the write lane opens.
func TestOlderDisableCannotStopNewerEnable(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.eng.writeMu.Lock()
	var releaseOnce sync.Once
	releaseWrite := func() { releaseOnce.Do(env.eng.writeMu.Unlock) }
	defer releaseWrite()

	disabled := defaultConfig()
	disabled.Enabled = false
	disabledDone := make(chan struct{})
	go func() {
		env.eng.Reconfigure(disabled)
		close(disabledDone)
	}()

	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		env.eng.mu.Lock()
		published := !env.eng.cfg.Enabled
		env.eng.mu.Unlock()
		if published {
			break
		}
		select {
		case <-time.After(time.Millisecond):
		case <-deadline.C:
			t.Fatal("disabled reconfigure did not publish before waiting for Stop")
		}
	}

	// This call publishes a newer enabled generation without taking writeMu.
	env.eng.Reconfigure(defaultConfig())
	releaseWrite()
	select {
	case <-disabledDone:
	case <-time.After(time.Second):
		t.Fatal("older disabled reconfigure did not finish after write release")
	}

	env.eng.mu.Lock()
	stopped := env.eng.stopped
	enabled := env.eng.cfg.Enabled
	env.eng.mu.Unlock()
	if stopped || !enabled {
		t.Fatalf("older disable left engine stopped=%t enabled=%t, want stopped=false enabled=true", stopped, enabled)
	}
}

// TestProviderOptOutInvalidatesRetriesAcrossRapidReenable proves that retry
// work scheduled before an opt-out cannot survive a disable/enable cycle and
// perform a provider fetch in the new configuration generation.
func TestProviderOptOutInvalidatesRetriesAcrossRapidReenable(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))
	env.reconcile()
	before := env.claude.callCount(token("a"))

	env.eng.mu.Lock()
	acct := env.eng.accounts["idx-a"]
	acct.resetState = ResetAwaitingNewWindow
	acct.resetAt = baseTime
	env.eng.startRetrySequenceLocked(acct, true)
	env.eng.mu.Unlock()

	// The queued callback is the retry's immediate attempt. Leave the two
	// reconfiguration reconciles queued so only old retry work is exercised.
	env.async.mu.Lock()
	if len(env.async.funcs) != 1 {
		env.async.mu.Unlock()
		t.Fatalf("queued async work = %d, want one immediate retry", len(env.async.funcs))
	}
	oldRetry := env.async.funcs[0]
	env.async.funcs = nil
	env.async.mu.Unlock()

	disabled := defaultConfig()
	disabled.ManageClaude = false
	env.eng.Reconfigure(disabled)
	env.eng.Reconfigure(defaultConfig())

	oldRetry()
	env.clk.Advance(15 * time.Minute)
	if got := env.claude.callCount(token("a")); got != before {
		t.Fatalf("pre-opt-out retry work made %d provider call(s) after re-enable, want 0", got-before)
	}
}

// TestCacheRetryBlockedOnFetchSemaphoreCannotReachProviderAfterStandaloneSwitch
// closes a time-of-check/time-of-use gap. A cache-mode first-confirmation wave
// step selects an unconfirmed account, then its read waits for fetchSem. While
// it waits, the operator switches to standalone mode; the reconfiguration's
// reconcile has not run yet, so retrySeq is unchanged. When the read gets its
// slot it must see that standalone mode has no first-confirmation reason,
// rather than read credentials and make a provider request.
//
// synctest makes "blocked on fetchSem" observable: the engine, and so the
// semaphore channel, is created inside the bubble, and synctest.Wait returns
// only once the read goroutine is durably blocked on its send.
func TestCacheRetryBlockedOnFetchSemaphoreCannotReachProviderAfterStandaloneSwitch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		env := newTestEnv(t, cacheConfig(t))
		env.addAccount("claude", "a", day(1))
		env.reconcile() // no snapshot: a stays unknown and the wave is armed

		for i := 0; i < maxConcurrentFetches; i++ {
			env.eng.fetchSem <- struct{}{}
		}
		done := make(chan struct{})
		go func() {
			env.clk.Advance(5 * time.Second) // fires the +5s wave step
			close(done)
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("the +5s wave step did not wait on fetchSem; no first-confirmation wave was armed")
		default:
		}

		env.host.mu.Lock()
		getsBefore := len(env.host.getCalls)
		env.host.mu.Unlock()
		env.eng.Reconfigure(defaultConfig()) // its reconcile stays queued
		for i := 0; i < maxConcurrentFetches; i++ {
			<-env.eng.fetchSem
		}
		<-done

		if got := env.claude.callCount(token("a")); got != 0 {
			t.Fatalf("blocked cache-mode read made %d provider calls after the standalone switch, want 0", got)
		}
		env.host.mu.Lock()
		gets := env.host.getCalls[getsBefore:]
		env.host.mu.Unlock()
		if len(gets) != 0 {
			t.Fatalf("blocked cache-mode read fetched credentials %v after the standalone switch, want none", gets)
		}
	})
}

// blockWaveStep fills fetchSem, advances the clock by d on its own goroutine
// to fire a wave step, and returns once that step's reads are blocked on
// fetchSem. The
// returned channel closes when the step finishes. It must run inside a
// synctest bubble that created the engine.
func blockWaveStep(t *testing.T, env *testEnv, d time.Duration) <-chan struct{} {
	t.Helper()
	for i := 0; i < maxConcurrentFetches; i++ {
		env.eng.fetchSem <- struct{}{}
	}
	done := make(chan struct{})
	go func() {
		env.clk.Advance(d)
		close(done)
	}()
	synctest.Wait()
	select {
	case <-done:
		t.Fatalf("advancing %s fired no wave step that waits on fetchSem", d)
	default:
	}
	return done
}

// releaseFetchSem empties fetchSem, unblocking every waiting read.
func releaseFetchSem(env *testEnv) {
	for i := 0; i < maxConcurrentFetches; i++ {
		<-env.eng.fetchSem
	}
}

// TestCacheWaveStepAfterStandaloneSwitchWritesNothing: a Reconfigure to
// standalone mode lands while a wave step's reads wait on fetchSem, so every
// read drops out. Whether a step observed anything must not depend on the
// configuration: this step observed nothing and must not flush. In dry-run
// mode a flush logs one "would set priority" line per mismatched account; the
// queued reconciliation owns the next flush.
func TestCacheWaveStepAfterStandaloneSwitchWritesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := cacheConfig(t)
		cfg.DryRun = true
		env := newTestEnv(t, cfg)
		var mu sync.Mutex
		proposals := 0
		env.eng.logf = func(_, message string) {
			if strings.Contains(message, "would set priority") {
				mu.Lock()
				proposals++
				mu.Unlock()
			}
		}
		env.addAccount("claude", "a", day(1))
		env.addAccount("claude", "b", day(2))
		env.reconcile() // no snapshot: both stay unknown and the wave is armed
		mu.Lock()
		startup := proposals
		proposals = 0
		mu.Unlock()
		if startup == 0 {
			t.Fatal("startup logged no dry-run proposals, so a flush would be invisible")
		}

		done := blockWaveStep(t, env, 5*time.Second)
		standalone := defaultConfig()
		standalone.DryRun = true
		env.eng.Reconfigure(standalone) // its reconcile stays queued
		releaseFetchSem(env)
		<-done

		mu.Lock()
		defer mu.Unlock()
		if proposals != 0 {
			t.Fatalf("a wave step that observed nothing logged %d dry-run proposals, want 0", proposals)
		}
		if got := env.claude.callCount(token("a")) + env.claude.callCount(token("b")); got != 0 {
			t.Fatalf("provider calls before the standalone reconcile ran = %d, want 0", got)
		}
	})
}

// TestCacheWaveStepSupersededByReconfigureWritesNothing: CPA re-sends an
// unchanged configuration, still in quota-cache mode, while a wave step's
// reads wait on fetchSem. The reads still confirm every account, but the
// Reconfigure cancelled the wave and queued a reconciliation that writes the
// corrected order itself. The superseded step must write nothing.
func TestCacheWaveStepSupersededByReconfigureWritesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		env := newRestartEnv(t)
		cfg := env.eng.cfg
		writeCacheEntries(t, cfg.QuotaCachePath, restartEntries(baseTime.Add(-40*time.Minute)))
		env.restart()
		env.assertPhysical(restartFileOrder())
		writeCacheEntries(t, cfg.QuotaCachePath, restartEntries(baseTime))
		savesBefore := env.host.saveCount()

		done := blockWaveStep(t, env, 5*time.Second)
		env.eng.Reconfigure(cfg) // its reconcile stays queued
		releaseFetchSem(env)
		<-done
		// The superseded step publishes nothing, so read engine state directly.
		env.eng.mu.Lock()
		for _, a := range restartPool {
			if state := env.eng.accounts["idx-"+a.name].resetState; state != ResetConfirmed {
				t.Errorf("%s reset_state after the step's reads = %s, want confirmed", a.name, state)
			}
		}
		env.eng.mu.Unlock()
		if got := env.host.saveCount() - savesBefore; got != 0 {
			t.Fatalf("superseded wave step saved %d times, want 0", got)
		}

		env.async.drain()
		env.assertPhysical(restartCorrect())
		if got := env.host.saveCount() - savesBefore; got != len(restartPool) {
			t.Fatalf("saves by the reconfiguration's reconcile = %d, want %d (one per account)", got, len(restartPool))
		}
		if got := env.restartProviderCalls(); got != 0 {
			t.Fatalf("cache mode made %d provider calls, want 0", got)
		}
	})
}

// TestCacheWaveStepSupersededByReconcileWritesNothingMidPass: a reconciliation
// that reserves while the last (+15m) wave step's reads wait on fetchSem
// supersedes the step. Its reads drop out, and the pass removes c. The step
// observed nothing and must not flush while the pass's own reads are still
// pending: that would write a provisional ranking of the pass's new roster,
// which the pass then rewrites. The last step is used because it is the one
// that writes whatever it holds, even with accounts still unconfirmed.
func TestCacheWaveStepSupersededByReconcileWritesNothingMidPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		env := newTestEnv(t, cacheConfig(t))
		env.addAccount("claude", "a", day(3))
		env.addAccount("claude", "b", day(1))
		env.addAccount("claude", "c", day(2))
		env.reconcile() // no snapshot: provisional a=300 b=200 c=100, wave armed
		env.assertPhysical(map[string]int{"a": 300, "b": 200, "c": 100})
		env.clk.Advance(14 * time.Minute) // the +5s to +5m steps all miss
		savesBefore := env.host.saveCount()

		writeCacheEntries(t, env.eng.cfg.QuotaCachePath, map[string]cacheEntry{
			"a": {resetAt: day(3), observedAt: env.clk.Now()},
			"b": {resetAt: day(1), observedAt: env.clk.Now()},
		})
		env.host.removeEntry("idx-c")

		stepDone := blockWaveStep(t, env, time.Minute) // the +15m step
		// fetchSem hands a freed slot to its longest-waiting sender. This plug
		// queues behind the step's reads and takes the slot they free, so the
		// reconciliation's reads stay blocked until the step has finished.
		plug := make(chan struct{})
		go func() {
			env.eng.fetchSem <- struct{}{}
			close(plug)
		}()
		synctest.Wait()
		reconcileDone := make(chan struct{})
		go func() {
			env.eng.Reconcile(context.Background())
			close(reconcileDone)
		}()
		synctest.Wait() // the pass reserved, dropped c, and queued its reads

		<-env.eng.fetchSem // one slot: the step's reads drop out, then the plug takes it
		<-plug
		<-stepDone
		if got := env.host.saveCount() - savesBefore; got != 0 {
			t.Fatalf("superseded wave step saved %d times while the reconciliation's reads were pending, want 0", got)
		}

		releaseFetchSem(env)
		<-reconcileDone
		env.assertPhysical(map[string]int{"b": 200, "a": 100})
		if got := env.host.saveCount() - savesBefore; got != 1 {
			t.Fatalf("saves for the pass = %d, want 1 (a 300 -> 100)", got)
		}
	})
}
