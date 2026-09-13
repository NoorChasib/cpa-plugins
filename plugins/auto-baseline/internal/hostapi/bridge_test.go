package hostapi

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

type recordingCaller struct {
	mu       sync.Mutex
	calls    []string
	payloads [][]byte
	resp     []byte
	err      error
	block    chan struct{}
	entered  chan struct{}
}

func (c *recordingCaller) Call(method string, request []byte) ([]byte, error) {
	c.mu.Lock()
	c.calls = append(c.calls, method)
	c.payloads = append(c.payloads, append([]byte(nil), request...))
	block := c.block
	entered := c.entered
	c.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if block != nil {
		<-block
	}
	if c.err != nil {
		return nil, c.err
	}
	if c.resp == nil {
		return []byte(`{"ok":true}`), nil
	}
	return c.resp, nil
}

func TestLogSendsLevelAndMessage(t *testing.T) {
	caller := &recordingCaller{}
	b := NewBridge(caller)
	b.Log("info", "hello")
	if len(caller.calls) != 1 || caller.calls[0] != MethodHostLog {
		t.Fatalf("calls = %v, want [%s]", caller.calls, MethodHostLog)
	}
	var req LogRequest
	if err := json.Unmarshal(caller.payloads[0], &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Level != "info" || req.Message != "hello" {
		t.Errorf("request = %+v", req)
	}
}

func TestLogSwallowsFailures(t *testing.T) {
	caller := &recordingCaller{err: errors.New("boom")}
	b := NewBridge(caller)
	b.Log("warn", "x") // must not panic or block
	caller = &recordingCaller{resp: []byte(`{"ok":false,"error":{"code":"x","message":"y"}}`)}
	b = NewBridge(caller)
	b.Log("warn", "x")
}

func TestQuiesceRejectsNewCallbacks(t *testing.T) {
	caller := &recordingCaller{}
	b := NewBridge(caller)
	b.Quiesce()
	err := b.call(MethodHostLog, LogRequest{}, nil)
	if !errors.Is(err, ErrBridgeShuttingDown) {
		t.Fatalf("err = %v, want ErrBridgeShuttingDown", err)
	}
	if len(caller.calls) != 0 {
		t.Fatalf("host was called after quiesce")
	}
}

func TestDrainWaitsForEnteredCallback(t *testing.T) {
	caller := &recordingCaller{block: make(chan struct{}), entered: make(chan struct{}, 1)}
	b := NewBridge(caller)
	done := make(chan struct{})
	go func() {
		b.Log("info", "slow")
		close(done)
	}()
	<-caller.entered

	drained := make(chan struct{})
	go func() {
		b.Drain()
		close(drained)
	}()
	select {
	case <-drained:
		t.Fatal("Drain returned while a callback was still inside the host")
	case <-time.After(50 * time.Millisecond):
	}
	close(caller.block)
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("Drain did not return after the callback finished")
	}
	<-done
}

func TestCallbackLimitFailsFast(t *testing.T) {
	limiter := newCallbackLimiter(1)
	caller := &recordingCaller{block: make(chan struct{}), entered: make(chan struct{}, 1)}
	b := newBridgeWithLimiter(caller, limiter)
	go b.Log("info", "occupies the only slot")
	<-caller.entered
	err := b.call(MethodHostLog, LogRequest{}, nil)
	if !errors.Is(err, ErrHostCallbackLimit) {
		t.Fatalf("err = %v, want ErrHostCallbackLimit", err)
	}
	close(caller.block)
	b.Drain()
	if limiter.inFlight() != 0 {
		t.Fatalf("limiter still holds %d admissions", limiter.inFlight())
	}
}

func TestNilBridgeIsSafe(t *testing.T) {
	var b *Bridge
	b.Quiesce()
	b.Drain()
	if _, err := b.acquireCaller(); err == nil {
		t.Fatal("nil bridge must not admit callbacks")
	}
}
