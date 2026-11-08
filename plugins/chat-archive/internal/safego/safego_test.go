package safego

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunRecordsTypeOnly(t *testing.T) {
	var name, typ string
	ok := Run("worker", func(n, ty string) { name, typ = n, ty }, func() { panic("secret-value") })
	if ok || name != "worker" || typ != "string" || strings.Contains(typ, "secret") {
		t.Fatalf("ok=%v name=%q typ=%q", ok, name, typ)
	}
	if !Run("x", nil, func() {}) {
		t.Fatal("normal run reported a panic")
	}
	// A panicking recorder never escapes either.
	Run("x", func(string, string) { panic("again") }, func() { panic(1) })
}

func TestLoopRestartsAfterPanic(t *testing.T) {
	MinBackoff, MaxBackoff = time.Millisecond, 2*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var runs, panics atomic.Int32
	Loop(ctx, &wg, "loop", func(string, string) { panics.Add(1) }, func(ctx context.Context) {
		if runs.Add(1) <= 3 {
			panic(InjectedPanic{})
		}
		<-ctx.Done()
	})
	deadline := time.Now().Add(2 * time.Second)
	for runs.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	wg.Wait()
	if runs.Load() < 4 || panics.Load() != 3 {
		t.Fatalf("runs=%d panics=%d", runs.Load(), panics.Load())
	}
}

func TestCheckpointInjection(t *testing.T) {
	Inject("named", 1)
	if Run("named", nil, func() { Checkpoint("other") }) != true {
		t.Fatal("other goroutine panicked")
	}
	if Run("named", nil, func() { Checkpoint("named") }) {
		t.Fatal("armed checkpoint did not panic")
	}
	if !Run("named", nil, func() { Checkpoint("named") }) {
		t.Fatal("checkpoint panicked twice")
	}
}
