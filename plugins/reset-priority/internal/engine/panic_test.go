package engine

import (
	"context"
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

// panicHost panics where a test arms it: in the roster list, or in the
// credential read a fetch makes for one account. That read is the only one
// with a deadline, because request-timeout bounds it, which tells it apart
// from the roster validation and writeback reads of the same account.
type panicHost struct {
	*fakeHost
	list  atomic.Bool
	fetch string // auth index
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

// assertRecovered checks that exactly one recovered panic was logged, as a
// warning without the panic value.
func (r *logRecorder) assertRecovered(t *testing.T) {
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
	if recovered != 1 {
		t.Errorf("%d recovered-panic warnings, want 1; log: %q", recovered, r.lines)
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

// assertShutdownReturns checks that the work which panicked released its
// reservation: Shutdown waits for every reservation.
func assertShutdownReturns(t *testing.T, eng *Engine) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		eng.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown is still waiting for the work that panicked")
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
	logs.assertRecovered(t)
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

	logs.assertRecovered(t)
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

	states := map[string]string{}
	for _, group := range eng.Status().Providers {
		for _, acct := range group.Accounts {
			states[acct.Name] = acct.ResetState
		}
	}
	if states["a.json"] != string(ResetUnknown) || states["b.json"] != string(ResetConfirmed) {
		t.Errorf("reset states = %v, want a.json unknown (its fetch panicked) and b.json confirmed", states)
	}
	if got := claude.callCount(token("b")); got != 1 {
		t.Errorf("b was fetched %d times, want 1", got)
	}
	logs.assertRecovered(t)
	assertShutdownReturns(t, eng)
}
