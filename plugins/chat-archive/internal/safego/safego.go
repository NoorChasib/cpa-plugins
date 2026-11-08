// Package safego starts every plugin-owned goroutine behind a recover barrier.
//
// This library is dlopen'd into CPA and carries its own Go runtime: a panic that escapes a
// plugin goroutine aborts the whole proxy. Every goroutine therefore starts through Go or Loop,
// which recover, count the panic by goroutine name and the panic value's type only (%T, never
// the value), raise a fault, and never re-panic. Long-lived loops restart with 1 s to 60 s
// backoff.
package safego

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Recorder receives recovered panics. name is the goroutine name; typ is fmt.Sprintf("%T", v).
type Recorder func(name, typ string)

// Go runs fn once in a new goroutine tracked by wg. A panic is recovered and recorded.
func Go(wg *sync.WaitGroup, name string, rec Recorder, fn func()) {
	if wg != nil {
		wg.Add(1)
	}
	go func() {
		if wg != nil {
			defer wg.Done()
		}
		Run(name, rec, fn)
	}()
}

// Run calls fn in the current goroutine behind the recover barrier and reports whether fn
// returned normally.
func Run(name string, rec Recorder, fn func()) (ok bool) {
	defer func() {
		if v := recover(); v != nil {
			ok = false
			if rec != nil {
				func() {
					defer func() { _ = recover() }()
					rec(name, fmt.Sprintf("%T", v))
				}()
			}
		}
	}()
	fn()
	return true
}

// Backoff bounds for restarting a loop after a recovered panic.
var (
	MinBackoff = time.Second
	MaxBackoff = 60 * time.Second
)

// Loop runs fn(ctx) in a new goroutine tracked by wg until ctx ends. fn is a long-lived loop
// that returns when ctx ends. If fn panics, the panic is recorded and fn restarts after a
// backoff that doubles from MinBackoff to MaxBackoff. If fn returns normally while ctx is
// still live, Loop returns too (the loop chose to finish).
func Loop(ctx context.Context, wg *sync.WaitGroup, name string, rec Recorder, fn func(context.Context)) {
	if wg != nil {
		wg.Add(1)
	}
	go func() {
		if wg != nil {
			defer wg.Done()
		}
		backoff := MinBackoff
		for {
			if Run(name, rec, func() { fn(ctx) }) {
				return
			}
			if ctx.Err() != nil {
				return
			}
			t := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			backoff = min(backoff*2, MaxBackoff)
		}
	}()
}

// Test fault injection: Checkpoint panics in the named goroutine when a test armed it with
// Inject. Production code calls Checkpoint once per loop iteration; the cost is one atomic load.
var (
	injectArmed atomic.Bool
	injectMu    sync.Mutex
	injectNames = map[string]int{}
)

// InjectedPanic is the panic value Checkpoint raises.
type InjectedPanic struct{}

// Inject arms n panics for the goroutine called name. Tests only.
func Inject(name string, n int) {
	injectMu.Lock()
	injectNames[name] += n
	injectMu.Unlock()
	injectArmed.Store(true)
}

// Checkpoint panics with InjectedPanic when a test armed one for name.
func Checkpoint(name string) {
	if !injectArmed.Load() {
		return
	}
	injectMu.Lock()
	n := injectNames[name]
	if n > 0 {
		injectNames[name] = n - 1
	}
	injectMu.Unlock()
	if n > 0 {
		panic(InjectedPanic{})
	}
}
