package engine

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/clock"
	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/hostapi"
	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/providers"
)

// panicValue is what the hosts below panic with. It stands for a value that
// carries credential data, which no log line may repeat.
const panicValue = "panic carrying tok-secret"

// wantFetchPanicError is the last_error an account shows when its quota read
// panicked. REFERENCE.md and docs/troubleshooting.md quote it.
const wantFetchPanicError = "internal error while reading the weekly reset"

// panicHost panics where a test arms it: in the roster list, in the
// credential read a fetch makes for one account, or in the next priority
// save. The fetch read is the only one with a deadline, because
// request-timeout bounds it, which tells it apart from the roster validation
// and writeback reads of the same account.
type panicHost struct {
	*fakeHost
	list  atomic.Bool
	fetch string      // auth index
	save  atomic.Bool // panics in the next save only
}

func (h *panicHost) AuthList(ctx context.Context) ([]hostapi.AuthEntry, error) {
	if h.list.Load() {
		panic(panicValue)
	}
	return h.fakeHost.AuthList(ctx)
}

func (h *panicHost) AuthGet(ctx context.Context, authIndex string) (hostapi.AuthGetResponse, error) {
	if _, fetch := ctx.Deadline(); fetch && authIndex == h.fetch {
		panic(panicValue)
	}
	return h.fakeHost.AuthGet(ctx, authIndex)
}

func (h *panicHost) AuthSave(ctx context.Context, name string, doc json.RawMessage) error {
	if h.save.CompareAndSwap(true, false) {
		panic(panicValue)
	}
	return h.fakeHost.AuthSave(ctx, name, doc)
}

// panicProvider panics in the next weekly reset read for one token once
// armed, as a bug parsing a provider's response would.
type panicProvider struct {
	*fakeProvider
	token string
	armed atomic.Bool
}

func (p *panicProvider) FetchWeeklyReset(ctx context.Context, creds providers.Credentials) (providers.Observation, error) {
	if creds.AccessToken == p.token && p.armed.CompareAndSwap(true, false) {
		panic(panicValue)
	}
	return p.fakeProvider.FetchWeeklyReset(ctx, creds)
}

// panicClock panics in its next Now once armed. A test arms it just before
// the engine reads the clock inside a locked section, which stands for a bug
// that panics there.
type panicClock struct {
	*clock.Fake
	armed atomic.Bool
}

func (c *panicClock) Now() time.Time {
	if c.armed.CompareAndSwap(true, false) {
		panic(panicValue)
	}
	return c.Fake.Now()
}

// logRecorder collects log lines from every engine goroutine.
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *logRecorder) log(level, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, level+" "+message)
}

// assertRecovered checks that want recovered panics were logged, as warnings
// without the panic value.
func (r *logRecorder) assertRecovered(t *testing.T, want int) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	recovered := 0
	for _, line := range r.lines {
		if strings.Contains(line, "tok-secret") {
			t.Errorf("log line repeats the panic value: %q", line)
		}
		if line == "warn "+logWorkPanic {
			recovered++
		}
	}
	if recovered != want {
		t.Errorf("%d recovered-panic warnings, want %d; log: %q", recovered, want, r.lines)
	}
}

// newPanicEngine builds an engine over host, logging to the returned recorder.
func newPanicEngine(t *testing.T, host HostAuth, clk *clock.Fake, run func(func())) (*Engine, *fakeProvider, *logRecorder) {
	t.Helper()
	claude := newFakeProvider("claude", clk.Now)
	logs := &logRecorder{}
	eng := New(defaultConfig(), Deps{
		Clock:     clk,
		Host:      host,
		Providers: map[string]providers.Provider{"claude": claude},
		Log:       logs.log,
		RunAsync:  run,
	})
	return eng, claude, logs
}

// addClaude registers a healthy Claude account whose weekly reset is resetAt.
func addClaude(host *fakeHost, claude *fakeProvider, name string, resetAt time.Time) {
	host.setEntry(hostapi.AuthEntry{
		AuthIndex: "idx-" + name, ID: "id-" + name, Name: name + ".json", Provider: "claude",
		Type: "claude", Status: "active", Source: "file", Path: "/auth/" + name + ".json",
	}, map[string]any{"type": "claude", "access_token": token(name), "refresh_token": "refresh-" + token(name)})
	claude.setReset(token(name), resetAt)
}

// accountRows returns the published status rows by account name.
func accountRows(eng *Engine) map[string]AccountStatus {
	rows := map[string]AccountStatus{}
	for _, group := range eng.Status().Providers {
		for _, acct := range group.Accounts {
			rows[acct.Name] = acct
		}
	}
	return rows
}

// assertReturns checks that f returns, which it cannot while work that
// panicked still holds a lock f needs.
func assertReturns(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		f()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s is still blocked after the work that panicked", what)
	}
}

// assertShutdownReturns checks that the work which panicked released its
// reservation: Shutdown waits for every reservation.
func assertShutdownReturns(t *testing.T, eng *Engine) {
	t.Helper()
	assertReturns(t, "Shutdown", eng.Shutdown)
}

// reconcileRecovering runs a pass under a recover, as Dispatch runs a
// management refresh, and returns what it recovered.
func reconcileRecovering(eng *Engine) (recovered any) {
	defer func() { recovered = recover() }()
	eng.Reconcile(context.Background())
	return nil
}

// assertNextPassRuns checks that a reconciliation is armed after a pass that
// panicked, and that it runs when its time comes.
func assertNextPassRuns(t *testing.T, eng *Engine, host *panicHost, clk *clock.Fake) {
	t.Helper()
	now := clk.Now()
	next := eng.Status().NextReconcileAt
	if next == nil || !next.After(now) {
		t.Fatalf("next_reconcile_at = %v at %v; the pass that panicked armed no next pass", next, now)
	}
	lists := host.listCount()
	clk.AdvanceTo(*next)
	if got := host.listCount(); got != lists+1 {
		t.Fatalf("%d roster lists when the next pass was due, want 1", got-lists)
	}
}

// TestAsyncWorkPanicIsRecovered: production RunAsync is `go f()`, so a panic
// that escapes the work handed to it ends CPA. The startup reconciliation
// panics in the roster list; the work handed to RunAsync must return normally.
func TestAsyncWorkPanicIsRecovered(t *testing.T) {
	clk := clock.NewFake(baseTime)
	host := &panicHost{fakeHost: newFakeHost()}
	host.list.Store(true)
	var escaped any
	eng, _, logs := newPanicEngine(t, host, clk, func(f func()) {
		defer func() { escaped = recover() }()
		f()
	})

	eng.Start()

	if escaped != nil {
		t.Fatalf("a panic escaped the work handed to RunAsync, which would end CPA: %v", escaped)
	}
	logs.assertRecovered(t, 1)
	assertShutdownReturns(t, eng)
}

// TestTimerCallbackPanicIsRecovered: the clock runs a timer callback on a
// goroutine of its own, so a panic that escapes it ends CPA. The fake clock
// runs callbacks on the caller, so an escaping panic would leave Advance. The
// hourly reconciliation panics in the roster list.
func TestTimerCallbackPanicIsRecovered(t *testing.T) {
	clk := clock.NewFake(baseTime)
	host := &panicHost{fakeHost: newFakeHost()}
	host.now = clk.Now
	eng, claude, logs := newPanicEngine(t, host, clk, func(f func()) { f() })
	addClaude(host.fakeHost, claude, "a", day(2))
	eng.Reconcile(context.Background())

	host.list.Store(true)
	func() {
		defer func() {
			if escaped := recover(); escaped != nil {
				t.Fatalf("a panic escaped the hourly reconciliation timer, which would end CPA: %v", escaped)
			}
		}()
		clk.Advance(time.Hour)
	}()

	logs.assertRecovered(t, 1)
	assertShutdownReturns(t, eng)
}

// TestFetchGoroutinePanicIsRecovered: a reconciliation fetches each account
// on a goroutine of its own, as the first-confirmation wave does, so a panic
// in one fetch ends CPA even when the reconciliation itself runs under a
// recover, as a management refresh does. Without the recover this test ends
// the test binary. The panicking fetch must leave the rest of the pass alone:
// the other account is still read and ranked.
func TestFetchGoroutinePanicIsRecovered(t *testing.T) {
	clk := clock.NewFake(baseTime)
	host := &panicHost{fakeHost: newFakeHost(), fetch: "idx-a"}
	host.now = clk.Now
	eng, claude, logs := newPanicEngine(t, host, clk, func(f func()) { f() })
	addClaude(host.fakeHost, claude, "a", day(1))
	addClaude(host.fakeHost, claude, "b", day(2))

	if got := eng.Reconcile(context.Background()); got != ReconcileResultSuccess {
		t.Fatalf("reconcile = %s, want success", got)
	}

	assertFetchPanicRecorded(t, eng, "a.json", "b.json")
	if got := claude.callCount(token("b")); got != 1 {
		t.Errorf("b was fetched %d times, want 1", got)
	}
	logs.assertRecovered(t, 1)
	assertShutdownReturns(t, eng)
}

// assertFetchPanicRecorded checks that the account whose quota read panicked
// shows that as its error, as any failed read does, without the panic value,
// and that the other account was still read.
func assertFetchPanicRecorded(t *testing.T, eng *Engine, panicked, other string) {
	t.Helper()
	rows := accountRows(eng)
	if got := rows[panicked]; got.ResetState != string(ResetUnknown) || got.LastError != wantFetchPanicError {
		t.Errorf("%s reset_state=%q last_error=%q, want unknown and %q", panicked, got.ResetState, got.LastError, wantFetchPanicError)
	}
	if got := rows[other]; got.ResetState != string(ResetConfirmed) || got.LastError != "" {
		t.Errorf("%s reset_state=%q last_error=%q, want confirmed with no error", other, got.ResetState, got.LastError)
	}
}

// TestProviderPanicReleasesProviderAdmission: a weekly reset read runs while
// its fetch holds the read side of providerCallMu, which Reconfigure takes
// exclusively. A read that panicked used to keep it: Reconfigure then waited
// forever, and CPA applies every plugin's configuration under one lock, so
// did every later configuration apply. A waiting writer also holds back new
// readers, so every later fetch, a refresh among them, and Shutdown behind
// them waited too.
func TestProviderPanicReleasesProviderAdmission(t *testing.T) {
	clk := clock.NewFake(baseTime)
	host := newFakeHost()
	host.now = clk.Now
	claude := &panicProvider{fakeProvider: newFakeProvider("claude", clk.Now), token: token("a")}
	claude.armed.Store(true)
	logs := &logRecorder{}
	eng := New(defaultConfig(), Deps{
		Clock:     clk,
		Host:      host,
		Providers: map[string]providers.Provider{"claude": claude},
		Log:       logs.log,
		RunAsync:  func(f func()) { f() },
	})
	addClaude(host, claude.fakeProvider, "a", day(1))
	addClaude(host, claude.fakeProvider, "b", day(2))

	if got := eng.Reconcile(context.Background()); got != ReconcileResultSuccess {
		t.Fatalf("reconcile = %s, want success", got)
	}
	assertFetchPanicRecorded(t, eng, "a.json", "b.json")
	logs.assertRecovered(t, 1)

	// Reconfigure queues a pass, which runs inline here.
	assertReturns(t, "Reconfigure", func() { eng.Reconfigure(defaultConfig()) })
	assertReturns(t, "Reconcile", func() { eng.Reconcile(context.Background()) })
	if got := accountRows(eng)["a.json"]; got.ResetState != string(ResetConfirmed) || got.LastError != "" {
		t.Errorf("a.json reset_state=%q last_error=%q after its read answered, want confirmed with no error", got.ResetState, got.LastError)
	}
	assertShutdownReturns(t, eng)
}

// TestSavePanicReleasesTheSaveBarrier: a priority save runs while writeOne
// holds saveMu, which Reconfigure also takes. A save that panicked used to
// keep it, so Reconfigure waited forever, and so did every later
// configuration apply in CPA.
func TestSavePanicReleasesTheSaveBarrier(t *testing.T) {
	clk := clock.NewFake(baseTime)
	host := &panicHost{fakeHost: newFakeHost()}
	host.now = clk.Now
	eng, claude, _ := newPanicEngine(t, host, clk, func(f func()) { f() })
	addClaude(host.fakeHost, claude, "a", day(1))
	host.save.Store(true)

	if recovered := reconcileRecovering(eng); recovered == nil {
		t.Fatal("the pass did not panic in its save")
	}

	// Reconfigure queues a pass, which runs inline here and saves a.
	assertReturns(t, "Reconfigure", func() { eng.Reconfigure(defaultConfig()) })
	if got := len(host.savesFor("a.json")); got != 1 {
		t.Errorf("a.json was saved %d times after the save barrier was released, want 1", got)
	}
	assertShutdownReturns(t, eng)
}

// TestRosterPanicReleasesEngineLocks: a pass applies the roster while it
// holds writeMu and the state lock. A panic there used to keep both: the
// pass's own deferred cleanup then waited for the state lock it held, and so
// did Status, every later pass and Shutdown. The clock read inside that
// section stands for the panic.
func TestRosterPanicReleasesEngineLocks(t *testing.T) {
	clk := &panicClock{Fake: clock.NewFake(baseTime)}
	host := &panicHost{fakeHost: newFakeHost()}
	host.now = clk.Fake.Now
	claude := newFakeProvider("claude", clk.Fake.Now)
	eng := New(defaultConfig(), Deps{
		Clock:     clk,
		Host:      host,
		Providers: map[string]providers.Provider{"claude": claude},
		RunAsync:  func(f func()) { f() },
	})
	addClaude(host.fakeHost, claude, "a", day(1))
	// The roster's validation read is the last host call before the roster is
	// applied, and nothing reads the clock in between.
	var armed atomic.Bool
	armed.Store(true)
	host.beforeGet = func(string) {
		if armed.CompareAndSwap(true, false) {
			clk.armed.Store(true)
		}
	}

	var recovered any
	assertReturns(t, "The pass that panicked", func() { recovered = reconcileRecovering(eng) })
	if recovered == nil {
		t.Fatal("the pass did not panic while it applied the roster")
	}
	assertReturns(t, "Status", func() { eng.Status() })
	assertReturns(t, "Reconcile", func() {
		if got := eng.Reconcile(context.Background()); got != ReconcileResultSuccess {
			t.Errorf("reconcile = %s, want success", got)
		}
	})
	if got := accountRows(eng)["a.json"].ResetState; got != string(ResetConfirmed) {
		t.Errorf("a.json reset_state = %q after the next pass, want confirmed", got)
	}
	assertShutdownReturns(t, eng)
}

// TestPanickingPassStillArmsTheNextPass: a pass arms the next one at its
// tail, which a pass that panics never reaches. Background reconciliation,
// the safety net for every other timer, then stopped until a refresh, a
// configuration save or a restart: after a startup pass that panicked no pass
// was ever armed, and after an hourly one the status kept a
// next_reconcile_at in the past.
func TestPanickingPassStillArmsTheNextPass(t *testing.T) {
	clk := clock.NewFake(baseTime)
	host := &panicHost{fakeHost: newFakeHost()}
	host.now = clk.Now
	eng, claude, logs := newPanicEngine(t, host, clk, func(f func()) { f() })
	addClaude(host.fakeHost, claude, "a", day(2))

	host.list.Store(true)
	eng.Start()
	host.list.Store(false)
	assertNextPassRuns(t, eng, host, clk)

	host.list.Store(true)
	clk.Advance(time.Hour)
	host.list.Store(false)
	assertNextPassRuns(t, eng, host, clk)

	logs.assertRecovered(t, 2)
	assertShutdownReturns(t, eng)
}
