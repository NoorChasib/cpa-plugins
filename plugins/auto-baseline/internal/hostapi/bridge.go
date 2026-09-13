package hostapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// ErrBridgeShuttingDown is returned when shutdown has closed the host-callback
// gate. Native callbacks cannot be cancelled once entered, so shutdown rejects
// new callbacks and drains callbacks that already acquired the gate.
var ErrBridgeShuttingDown = errors.New("host bridge is shutting down")

// Caller invokes one raw host callback. The production implementation lives
// in the CGO ABI layer; tests provide fakes.
type Caller interface {
	// Call sends request JSON to the named host callback and returns the raw
	// envelope bytes produced by the host.
	Call(method string, request []byte) ([]byte, error)
}

// Bridge provides typed host callbacks over a raw Caller. This plugin only
// uses host.log, which the audited host serves synchronously from memory
// (internal/pluginhost/host_callbacks.go:322); the gate and drain exist so
// terminal native shutdown never releases the host function table while a
// callback is still inside it.
type Bridge struct {
	mu        sync.Mutex
	drained   *sync.Cond
	caller    Caller
	accepting bool
	inFlight  int

	// limiter caps concurrent native callbacks process-wide. It is immutable
	// after construction, and b.mu is always taken before the limiter's own
	// lock (the limiter never takes b.mu, so the order cannot invert).
	limiter *callbackLimiter
}

// NewBridge wraps a raw Caller, bound by the process-wide callback limit.
func NewBridge(caller Caller) *Bridge {
	return newBridgeWithLimiter(caller, globalCallbackLimiter)
}

// newBridgeWithLimiter builds a Bridge over an explicit limiter so tests can
// exercise saturation without consuming process-wide capacity.
func newBridgeWithLimiter(caller Caller, limiter *callbackLimiter) *Bridge {
	b := &Bridge{caller: caller, accepting: true, limiter: limiter}
	b.drained = sync.NewCond(&b.mu)
	return b
}

// Quiesce closes the callback gate. Calls that already acquired the gate are
// allowed to finish and remain visible to Drain.
func (b *Bridge) Quiesce() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.accepting = false
	b.mu.Unlock()
}

// Drain closes the callback gate and waits until all callbacks that already
// acquired it have fully returned. ABI v1 host callbacks carry no
// cancellation, so the wait is unbounded by design: returning from terminal
// native shutdown while a callback is inside the host would let the host
// unload the library underneath live frames.
func (b *Bridge) Drain() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.accepting = false
	for b.inFlight != 0 {
		b.drained.Wait()
	}
	b.mu.Unlock()
}

// acquireCaller admits one native callback, taking both the bridge's in-flight
// count (which Drain waits on) and one process-wide admission.
func (b *Bridge) acquireCaller() (Caller, error) {
	if b == nil {
		return nil, fmt.Errorf("host bridge is unavailable")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.accepting {
		return nil, ErrBridgeShuttingDown
	}
	if b.caller == nil {
		return nil, fmt.Errorf("host bridge is unavailable")
	}
	if !b.callbackLimiter().acquire() {
		return nil, ErrHostCallbackLimit
	}
	b.inFlight++
	return b.caller, nil
}

// releaseCaller returns one admission after the native call has returned.
func (b *Bridge) releaseCaller() {
	b.mu.Lock()
	b.inFlight--
	b.callbackLimiter().release()
	if b.inFlight == 0 {
		b.drained.Broadcast()
	}
	b.mu.Unlock()
}

func (b *Bridge) callbackLimiter() *callbackLimiter {
	if b.limiter == nil {
		return globalCallbackLimiter
	}
	return b.limiter
}

func marshalRequest(method string, payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal %s request: %w", method, err)
	}
	return raw, nil
}

func invoke(caller Caller, method string, raw []byte, out any) error {
	respRaw, errCall := caller.Call(method, raw)
	if errCall != nil {
		return fmt.Errorf("host callback %s: %w", method, errCall)
	}
	var env Envelope
	if errUnmarshal := json.Unmarshal(respRaw, &env); errUnmarshal != nil {
		return fmt.Errorf("decode %s envelope: %w", method, errUnmarshal)
	}
	if !env.OK {
		if env.Error != nil {
			return fmt.Errorf("host callback %s failed: %s: %s", method, env.Error.Code, env.Error.Message)
		}
		return fmt.Errorf("host callback %s failed", method)
	}
	if out == nil {
		return nil
	}
	if errUnmarshal := json.Unmarshal(env.Result, out); errUnmarshal != nil {
		return fmt.Errorf("decode %s result: %w", method, errUnmarshal)
	}
	return nil
}

// call marshals payload, invokes the callback, and decodes the envelope.
func (b *Bridge) call(method string, payload any, out any) error {
	raw, errMarshal := marshalRequest(method, payload)
	if errMarshal != nil {
		return errMarshal
	}
	caller, errAcquire := b.acquireCaller()
	if errAcquire != nil {
		return errAcquire
	}
	defer b.releaseCaller()
	return invoke(caller, method, raw, out)
}

// Log emits a sanitized log line through the host logger. Failures are
// swallowed: logging must never break learning or promotion.
func (b *Bridge) Log(level, message string) {
	_ = b.call(MethodHostLog, LogRequest{Level: level, Message: message}, nil)
}
