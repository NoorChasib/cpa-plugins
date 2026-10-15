package warmup

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

// fakeHost answers the warm-up callbacks the way CPA v8 does. execute, when
// set, replaces the model request; it may block and ignore its context, as
// CPA's callback does.
type fakeHost struct {
	mu         sync.Mutex
	cancelErr  error
	runtime    protocol.HostAuthFileEntry
	runtimeErr error
	probes     []string
	requests   []protocol.HostModelExecutionRequest
	execute    func() (protocol.HostModelExecutionResponse, error)
	// cancelHook cancels the warm-up's context from inside the probe, as a
	// stop that lands while the probe is in CPA would.
	cancelHook bool
	cancel     context.CancelFunc
}

func newFakeHost() *fakeHost {
	return &fakeHost{runtime: protocol.HostAuthFileEntry{ID: "claude-one@example.com.json", AuthIndex: accountA, Provider: "claude", Type: "claude", AccountType: "oauth"}}
}

func (h *fakeHost) GetRuntime(_ context.Context, index string) (protocol.HostAuthFileEntry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if index != accountA {
		return protocol.HostAuthFileEntry{}, errors.New("host callback host.auth.get_runtime failed: host_call_failed")
	}
	return h.runtime, h.runtimeErr
}

func (h *fakeHost) HTTPCancel(_ context.Context, id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.probes = append(h.probes, id)
	if h.cancelHook && h.cancel != nil {
		h.cancel()
	}
	return h.cancelErr
}

func (h *fakeHost) ExecuteModel(_ context.Context, request protocol.HostModelExecutionRequest) (protocol.HostModelExecutionResponse, error) {
	h.mu.Lock()
	h.requests = append(h.requests, request)
	execute := h.execute
	h.mu.Unlock()
	if execute != nil {
		return execute()
	}
	return protocol.HostModelExecutionResponse{StatusCode: 200, Body: []byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"OK"}]}`)}, nil
}

func (h *fakeHost) sent() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.requests)
}

func warmOptions(t *testing.T) Options {
	t.Helper()
	o, err := Defaults().Validate()
	if err != nil {
		t.Fatal(err)
	}
	return o
}

var subscriber = Identity{AuthIndex: accountA, ID: "claude-one@example.com.json", Email: "one@example.com", Plan: "max_20x"}

func TestCPAPinsTheRequestToTheExactCredential(t *testing.T) {
	host := newFakeHost()
	o := warmOptions(t)
	o.Model, o.Effort = "claude-future-model", "medium"
	runner := NewCPA(host)
	for range 2 {
		if err := runner.Warm(context.Background(), o, subscriber); err != nil {
			t.Fatal(err)
		}
	}
	request := host.requests[0]
	if request.AuthID != "claude-one@example.com.json" || request.ForcedProvider != "claude" {
		t.Fatalf("request is not pinned to the credential ID: auth_id=%q forced_provider=%q", request.AuthID, request.ForcedProvider)
	}
	if request.EntryProtocol != "claude" || request.ExitProtocol != "claude" || request.Stream || request.Model != "claude-future-model" {
		t.Fatalf("request shape = %+v", request)
	}
	var body map[string]any
	if err := json.Unmarshal(request.Body, &body); err != nil {
		t.Fatal(err)
	}
	messages, _ := body["messages"].([]any)
	effort, _ := body["output_config"].(map[string]any)
	tokens, _ := body["max_tokens"].(float64)
	if body["model"] != "claude-future-model" || len(messages) != 1 || effort["effort"] != "medium" || tokens <= 1 || tokens > 64 {
		t.Fatalf("body = %s", request.Body)
	}
	for _, absent := range []string{"thinking", "system", "tools", "stream"} {
		if _, present := body[absent]; present {
			t.Fatalf("body sets %s: %s", absent, request.Body)
		}
	}
	if len(host.probes) != 2 || host.probes[0] == "" || host.probes[0] == host.probes[1] {
		t.Fatalf("each warm-up must probe with a fresh operation ID: %q", host.probes)
	}
}

func TestCPAFailsClosedBeforeAnyRequest(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*fakeHost, *Identity)
		want   string
	}{
		"host before v7.3.16": {func(h *fakeHost, _ *Identity) {
			h.cancelErr = errors.New("host callback host.http.cancel failed: host_call_failed")
		}, "host_unsupported"},
		"runtime unavailable": {func(h *fakeHost, _ *Identity) { h.runtimeErr = errors.New("host_call_failed") }, "account_unavailable"},
		"unlisted account":    {func(_ *fakeHost, id *Identity) { id.AuthIndex = accountB }, "account_unavailable"},
		"another index":       {func(h *fakeHost, _ *Identity) { h.runtime.AuthIndex = accountB }, "identity_unknown"},
		"no credential ID":    {func(h *fakeHost, _ *Identity) { h.runtime.ID = "" }, "identity_unknown"},
		"credential replaced": {func(h *fakeHost, _ *Identity) { h.runtime.ID = "claude-two@example.com.json" }, "identity_unknown"},
		"malformed index":     {func(_ *fakeHost, id *Identity) { id.AuthIndex = "claude-one" }, "identity_unknown"},
		"Claude API key":      {func(h *fakeHost, _ *Identity) { h.runtime.AccountType = "api_key" }, "subscription_login_required"},
		"no account type":     {func(h *fakeHost, _ *Identity) { h.runtime.AccountType = "" }, "subscription_login_required"},
		"another provider":    {func(h *fakeHost, _ *Identity) { h.runtime.Provider, h.runtime.Type = "codex", "codex" }, "subscription_login_required"},
		"runtime only":        {func(h *fakeHost, _ *Identity) { h.runtime.RuntimeOnly = true }, "subscription_login_required"},
		"free plan":           {func(_ *fakeHost, id *Identity) { id.Plan = "free" }, "subscription_login_required"},
		"unknown plan":        {func(_ *fakeHost, id *Identity) { id.Plan = "" }, "subscription_login_required"},
		"disabled in CPA":     {func(h *fakeHost, _ *Identity) { h.runtime.Disabled = true }, "account_unavailable"},
		"unavailable in CPA":  {func(h *fakeHost, _ *Identity) { h.runtime.Unavailable = true }, "account_unavailable"},
		"cooling down in CPA": {func(h *fakeHost, _ *Identity) {
			h.runtime.Unavailable, h.runtime.NextRetryAfter = true, time.Now().Add(time.Hour)
		}, "account_unavailable"},
		"stopped before the probe": {func(h *fakeHost, _ *Identity) {
			h.cancelErr = context.Canceled
			h.cancelHook = true
		}, "runner_unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			host, identity := newFakeHost(), subscriber
			tc.change(host, &identity)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			host.cancel = cancel
			if err := NewCPA(host).Warm(ctx, warmOptions(t), identity); err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %s", err, tc.want)
			}
			if host.sent() != 0 {
				t.Fatal("a model request was sent")
			}
		})
	}
	if err := NewCPA(nil).Warm(context.Background(), warmOptions(t), subscriber); err == nil || err.Error() != "host_unsupported" {
		t.Fatalf("host without the callbacks: %v", err)
	}
	host := newFakeHost()
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewCPA(host).Warm(stopped, warmOptions(t), subscriber); err == nil || err.Error() != "runner_unavailable" || len(host.probes) != 0 {
		t.Fatalf("a stopped warm-up called the host or misreported: %v", err)
	}
}

// CPA leaves Unavailable set once a cooldown has ended, and selects by the
// deadline; a warm-up after the deadline goes out.
func TestCPASendsOnceACooldownHasEnded(t *testing.T) {
	host := newFakeHost()
	host.runtime.Unavailable, host.runtime.NextRetryAfter = true, time.Now().Add(-time.Minute)
	if err := NewCPA(host).Warm(context.Background(), warmOptions(t), subscriber); err != nil || host.sent() != 1 {
		t.Fatalf("error = %v, sent = %d", err, host.sent())
	}
}

func TestCPAProviderAnswersAreNotReturned(t *testing.T) {
	for name, answer := range map[string]func() (protocol.HostModelExecutionResponse, error){
		"callback error": func() (protocol.HostModelExecutionResponse, error) {
			return protocol.HostModelExecutionResponse{}, errors.New("host callback host.model.execute failed: sensitive-provider-body")
		},
		"not OK": func() (protocol.HostModelExecutionResponse, error) {
			return protocol.HostModelExecutionResponse{StatusCode: 500, Body: []byte(`{"type":"message"}`)}, nil
		},
		"error body": func() (protocol.HostModelExecutionResponse, error) {
			return protocol.HostModelExecutionResponse{StatusCode: 200, Body: []byte(`{"type":"error","error":{"message":"sensitive-provider-body"}}`)}, nil
		},
		"not JSON": func() (protocol.HostModelExecutionResponse, error) {
			return protocol.HostModelExecutionResponse{StatusCode: 200, Body: []byte("sensitive-provider-body")}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			host := newFakeHost()
			host.execute = answer
			err := NewCPA(host).Warm(context.Background(), warmOptions(t), subscriber)
			if err == nil || err.Error() != "request_failed" || strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

// CPA's model callback ignores its context, so the runner stops waiting at
// its deadline instead. Until that request returns, no second one starts.
func TestCPAStopsWaitingButAdmitsNoSecondRequest(t *testing.T) {
	host, release := newFakeHost(), make(chan struct{})
	host.execute = func() (protocol.HostModelExecutionResponse, error) {
		<-release
		return protocol.HostModelExecutionResponse{StatusCode: 200, Body: []byte(`{"type":"message"}`)}, nil
	}
	runner, o := NewCPA(host), warmOptions(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := runner.Warm(ctx, o, subscriber); err == nil || err.Error() != "outcome_unknown" {
		t.Fatalf("error = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("runner waited past its deadline")
	}
	if err := runner.Warm(context.Background(), o, subscriber); err == nil || err.Error() != "runner_busy" {
		t.Fatalf("second warm-up beside a running request: %v", err)
	}
	if host.sent() != 1 || len(host.probes) != 1 {
		t.Fatal("a busy runner called the host")
	}
	host.mu.Lock()
	host.execute = nil
	host.mu.Unlock()
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := runner.Warm(context.Background(), o, subscriber)
		if err == nil {
			break
		}
		if err.Error() != "runner_busy" || time.Now().After(deadline) {
			t.Fatalf("runner did not recover after the request returned: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
