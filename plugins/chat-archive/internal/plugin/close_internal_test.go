package plugin

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"
)

// TestCloseWhenIdle: a stop that left an outbox write retry behind saves again and closes the
// outbox only after that write returned, saving before closing, each with its own deadline.
func TestCloseWhenIdle(t *testing.T) {
	idle := make(chan struct{})
	var mu sync.Mutex
	var steps []string
	step := func(name string) func(context.Context) {
		return func(ctx context.Context) {
			if _, ok := ctx.Deadline(); !ok || ctx.Err() != nil {
				t.Errorf("%s ran without a live deadline", name)
			}
			mu.Lock()
			steps = append(steps, name)
			mu.Unlock()
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		closeWhenIdle(idle, step("resave"), step("close"))
	}()
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	early := len(steps)
	mu.Unlock()
	if early != 0 {
		t.Fatal("saved or closed while the write was still running")
	}
	close(idle)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("closeWhenIdle did not finish after the write returned")
	}
	if !reflect.DeepEqual(steps, []string{"resave", "close"}) {
		t.Fatalf("steps = %v, want [resave close]", steps)
	}
}
