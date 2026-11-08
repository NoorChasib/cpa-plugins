//go:build cgo

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	pluginimpl "github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/plugin"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/plugin/plugintest"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/uploader"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

type stubPlugin struct {
	hook     func(string, []byte) []byte
	handle   func(string, []byte) (any, error)
	shutdown func()
	panics   []string
}

func (s *stubPlugin) Hook(m string, raw []byte) []byte         { return s.hook(m, raw) }
func (s *stubPlugin) HookPanicked(m, typ string)               { s.panics = append(s.panics, m+":"+typ) }
func (s *stubPlugin) Handle(m string, raw []byte) (any, error) { return s.handle(m, raw) }
func (s *stubPlugin) Shutdown() {
	if s.shutdown != nil {
		s.shutdown()
	}
}

func install(p nativePlugin) {
	globalMu.Lock()
	globalPlugin = p
	globalMu.Unlock()
}

func TestHookPanicStillReturnsOK(t *testing.T) {
	s := &stubPlugin{hook: func(string, []byte) []byte { panic("secret-payload-canary") },
		handle: func(string, []byte) (any, error) { panic("secret-payload-canary") }}
	install(s)
	defer shutdownNative()
	for _, m := range []string{protocol.MethodRequestInterceptBefore, protocol.MethodRequestInterceptAfter, protocol.MethodResponseInterceptAfter,
		protocol.MethodResponseInterceptStreamChunk, protocol.MethodRequestComplete, protocol.MethodUsageHandle} {
		out, rc := handleCall(m, []byte(`{}`))
		if rc != 0 || string(out) != string(hooks.OK) {
			t.Fatalf("%s: rc %d out %s", m, rc, out)
		}
	}
	if len(s.panics) != 6 || !strings.HasSuffix(s.panics[0], ":string") || strings.Contains(strings.Join(s.panics, ","), "canary") {
		t.Fatalf("panics recorded = %v", s.panics)
	}
	out, rc := handleCall(protocol.MethodManagementHandle, []byte(`{}`))
	if rc != 1 || strings.Contains(string(out), "canary") || !strings.Contains(string(out), "plugin_panic") {
		t.Fatalf("management panic = %d %s", rc, out)
	}
}

func TestErrorsDoNotEchoPayload(t *testing.T) {
	install(&stubPlugin{hook: func(string, []byte) []byte { return hooks.OK },
		handle: func(string, []byte) (any, error) { return nil, errFromPayload("secret-payload-canary") }})
	defer shutdownNative()
	out, rc := handleCall(protocol.MethodManagementRegister, nil)
	if rc != 1 || strings.Contains(string(out), "canary") {
		t.Fatalf("error echoed: %s", out)
	}
}

type errFromPayload string

func (e errFromPayload) Error() string { return string(e) }

func TestOversizeCallsPassThroughAndCount(t *testing.T) {
	h := plugintest.New(t, nil, "max-rpc-mib: 8\n")
	install(h.Plugin)
	defer shutdownNative()
	big := make([]byte, 9<<20)
	out, rc := handleCall(protocol.MethodRequestInterceptBefore,
		plugintest.BeforeAuth("big-1", "claude", big, http.Header{"X-Archive-Machine": {"m"}}, plugintest.Meta("/v1/messages")))
	if rc != 0 || !strings.Contains(string(out), `"ClearHeaders":["X-Archive-Machine"]`) {
		t.Fatalf("oversize before-auth = %d %.200s", rc, out)
	}
	if out, rc := handleCall(protocol.MethodResponseInterceptAfter, plugintest.Response("big-1", "claude", big, nil)); rc != 0 || string(out) != string(hooks.OK) {
		t.Fatalf("oversize response = %d", rc)
	}
	if out, rc := handleCall(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("big-1", "claude", 0, make([]byte, 17<<20), nil)); rc != 0 || string(out) != string(hooks.OK) {
		t.Fatalf("oversize chunk = %d", rc)
	}
	if out, rc := handleCall(protocol.MethodUsageHandle, plugintest.Usage("u", "k", "s", strings.Repeat("f", 5<<20))); rc != 0 || string(out) != string(hooks.OK) {
		t.Fatalf("oversize usage = %d", rc)
	}
	handleCall(protocol.MethodRequestComplete, plugintest.Complete("big-1", "succeeded", plugintest.Meta("/v1/messages")))
	if n := h.Plugin.Engine().Stats.Dropped()["oversize_rpc"]; n != 4 {
		t.Fatalf("oversize_rpc = %d", n)
	}
	out, rc = handleCall(protocol.MethodManagementHandle, []byte(`{"Method":"GET","Path":"/x","Body":"`+strings.Repeat("A", 2<<20)+`"}`))
	var env protocol.Envelope
	json.Unmarshal(out, &env)
	if rc != 0 || !strings.Contains(string(env.Result), `"StatusCode":413`) {
		t.Fatalf("oversize management = %d %.200s", rc, out)
	}
	if _, rc := handleCall(protocol.MethodPluginReconfigure, make([]byte, 129<<10)); rc != 1 {
		t.Fatal("oversize reconfigure accepted")
	}
	// The completion records that no begin was written for the oversize request.
	plugintest.WaitFor(t, "completion", func() bool {
		for _, r := range h.Srv.Envelopes(h.SourceID) {
			if r.Envelope.Kind == wire.KindExchangeDone {
				var c wire.ExchangeCompletion
				json.Unmarshal(r.Envelope.Body, &c)
				if c.BeginWritten {
					t.Fatal("begin_written for an oversize request")
				}
				return true
			}
		}
		return false
	})
}

// Shutdown completes within its deadline while an upload is stuck: the commit route is held,
// the uploader gets StopDeadline for in-flight HTTP, the outbox keeps the envelope pending.
func TestShutdownWithinDeadlineWithStuckUpload(t *testing.T) {
	h := plugintest.New(t, nil, "")
	install(h.Plugin)
	release := h.Srv.Hold("POST " + wire.PathCommit)
	defer release()
	handleCall(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth("stuck-1", "claude", plugintest.ClaudeBody("held"), nil, plugintest.Meta("/v1/messages")))
	ob, _, _ := h.Plugin.Runtime()
	plugintest.WaitFor(t, "envelope queued", func() bool {
		items, _ := ob.Pending(context.Background(), time.Now().Add(time.Hour), 10, 1<<30)
		return len(items) == 1
	})
	time.Sleep(300 * time.Millisecond) // the uploader is now inside the held commit
	start := time.Now()
	shutdownNative()
	elapsed := time.Since(start)
	limit := uploader.StopDeadline + uploader.FinalHeartbeat + 3*time.Second
	if elapsed > limit {
		t.Fatalf("shutdown took %v (limit %v)", elapsed, limit)
	}
	t.Logf("shutdown with a held commit took %v (uploader deadline %v)", elapsed, uploader.StopDeadline)
	if out, rc := handleCall(protocol.MethodRequestInterceptBefore, []byte(`{}`)); rc != 0 || string(out) != string(hooks.OK) {
		t.Fatal("hook after shutdown did not pass through")
	}
	if len(h.Srv.Envelopes(h.SourceID)) != 0 {
		t.Fatal("held commit was answered")
	}
	// Init works again in the same process; the envelope is still pending and now uploads.
	release()
	install(pluginimpl.New(nil))
	defer shutdownNative()
	p := globalPlugin.(*pluginimpl.Plugin)
	if _, err := p.Handle(protocol.MethodPluginRegister, plugintest.Lifecycle(h.YAML)); err != nil {
		t.Fatal(err)
	}
	plugintest.WaitFor(t, "pending envelope uploaded after restart", func() bool { return len(h.Srv.Envelopes(h.SourceID)) == 1 })
}

func TestShutdownDrainsAdmittedNativeCall(t *testing.T) {
	entered, releaseCall := make(chan struct{}), make(chan struct{})
	stopped := make(chan struct{})
	install(&stubPlugin{
		hook:     func(string, []byte) []byte { close(entered); <-releaseCall; return hooks.OK },
		handle:   func(string, []byte) (any, error) { return struct{}{}, nil },
		shutdown: func() { close(stopped) },
	})
	callDone := make(chan struct{})
	go func() { handleCall(protocol.MethodRequestInterceptAfter, []byte(`{}`)); close(callDone) }()
	<-entered
	shutdownDone := make(chan struct{})
	go func() { shutdownNative(); close(shutdownDone) }()
	<-stopped // the plugin stops while the call is in flight (hooks pass through by state)
	select {
	case <-shutdownDone:
		t.Fatal("shutdown cleared the instance while a call was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseCall)
	<-callDone
	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not drain")
	}
	if _, rc := handleCall(protocol.MethodManagementRegister, nil); rc != 1 {
		t.Fatal("call accepted after shutdown")
	}
	shutdownNative() // idempotent
}

// Allocation per before-auth call stays within 1.1 × the decoded body + 1 MiB: the
// base64 RPC is decoded in place, never copied into the Go heap.
func TestBeforeAuthAllocationBound(t *testing.T) {
	if testing.Short() {
		t.Skip("allocation measurement")
	}
	const size = 8 << 20
	raw := plugintest.BeforeAuth("alloc", "claude", make([]byte, size), http.Header{"User-Agent": {"x"}}, plugintest.Meta("/v1/messages"))
	e := hooks.NewEngine()
	e.SetConfig(&hooks.Config{Configured: true, Capture: "full", MaxRPC: 160 << 20})
	e.Start()
	drain := func() {
		for {
			select {
			case w := <-e.Handoff:
				e.Pool.Release(w.Begin.Reserved)
			case <-e.Control:
			default:
				return
			}
		}
	}
	res := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			e.BeforeAuth(raw)
			drain()
		}
	})
	limit := int64(size)*11/10 + 1<<20
	if got := res.AllocedBytesPerOp(); got > limit {
		t.Fatalf("before-auth allocates %d bytes/op for an %d-byte body (limit %d)", got, size, limit)
	}
	t.Logf("before-auth: %d bytes/op, %d allocs/op for %d-byte body", res.AllocedBytesPerOp(), res.AllocsPerOp(), size)
	runtime.KeepAlive(raw)
}

// bigHeaders is a multi-MiB executed-upstream RequestHeaders map (synthetic).
func bigHeaders(n int) map[string][]string {
	h := map[string][]string{"X-Api-Key": {"upstream-request-header-canary"}}
	for i := 0; i < n/(64<<10); i++ {
		h["X-Synthetic-"+strings.Repeat("h", 8)+string(rune('A'+i%26))+string(rune('a'+i/26%26))] = []string{strings.Repeat("v", 64<<10)}
	}
	return h
}

// TestResponseAndStreamAllocationBound: the allocation bound (Go-heap allocation per hook call
// ≤ 1.1 × the decoded size of the kept fields + 1 MiB) holds for response.intercept_after and both
// stream calls when the fields the plugin skips (RequestHeaders, OriginalRequest, RequestBody,
// HistoryChunks) are multi-MiB: they are skipped in the C buffer, never materialized, so the upstream credentials in
// RequestHeaders never reach the Go heap.
func TestResponseAndStreamAllocationBound(t *testing.T) {
	if testing.Short() {
		t.Skip("allocation measurement")
	}
	const size = 8 << 20
	big := make([]byte, 4<<20)
	history := [][]byte{make([]byte, 512<<10), make([]byte, 512<<10)}
	meta := plugintest.Meta("/v1/messages")
	enc := func(v any) []byte {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	response := enc(map[string]any{"RequestID": "alloc-r", "SourceFormat": "claude", "Model": "model-x", "RequestedModel": "model-x",
		"RequestHeaders": bigHeaders(4 << 20), "ResponseHeaders": map[string][]string{"Content-Type": {"application/json"}},
		"OriginalRequest": big, "RequestBody": big, "Body": make([]byte, size), "StatusCode": 200, "Metadata": meta})
	headerInit := enc(map[string]any{"RequestID": "alloc-s", "SourceFormat": "claude", "Model": "model-x", "ChunkIndex": -1,
		"RequestHeaders": bigHeaders(4 << 20), "ResponseHeaders": map[string][]string{"Content-Type": {"text/event-stream"}},
		"OriginalRequest": big, "RequestBody": big, "HistoryChunks": history, "Metadata": meta})
	const chunkSize = 4 << 20
	chunk := enc(map[string]any{"RequestID": "alloc-s", "SourceFormat": "claude", "Model": "model-x", "ChunkIndex": 0,
		"RequestHeaders": bigHeaders(4 << 20), "HistoryChunks": history, "Body": make([]byte, chunkSize), "Metadata": meta})

	e := hooks.NewEngine()
	e.SetConfig(&hooks.Config{Configured: true, Capture: "full", MaxRPC: 160 << 20})
	e.Start()
	drain := func() {
		for {
			select {
			case w := <-e.Handoff:
				if w.Response != nil {
					e.Pool.Release(w.Response.Reserved)
				}
			case s := <-e.Stream:
				e.Pool.ReleaseStream(s.Reserved)
				e.With(s.RequestID, false, func(en *hooks.Entry) { en.StreamBytes -= s.Reserved })
			case <-e.Control:
			default:
				return
			}
		}
	}
	for _, c := range []struct {
		name string
		call func([]byte) []byte
		raw  []byte
		kept int64
	}{
		{"response", e.Response, response, size},
		{"stream_header_init", e.StreamChunk, headerInit, 0},
		{"stream_chunk", e.StreamChunk, chunk, chunkSize},
	} {
		res := testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				c.call(c.raw)
				drain()
			}
		})
		limit := c.kept*11/10 + 1<<20
		if got := res.AllocedBytesPerOp(); got > limit {
			t.Errorf("%s allocates %d bytes/op for %d kept bytes in a %d-byte RPC (limit %d)", c.name, got, c.kept, len(c.raw), limit)
		}
		t.Logf("%s: %d bytes/op, %d allocs/op (RPC %d bytes, kept %d)", c.name, res.AllocedBytesPerOp(), res.AllocsPerOp(), len(c.raw), c.kept)
	}
	runtime.KeepAlive(response)
}

func BenchmarkBeforeAuth8MiB(b *testing.B) {
	raw := plugintest.BeforeAuth("alloc", "claude", make([]byte, 8<<20), nil, nil)
	e := hooks.NewEngine()
	e.SetConfig(&hooks.Config{Configured: true, Capture: "full", MaxRPC: 160 << 20})
	e.Start()
	b.ReportAllocs()
	b.SetBytes(8 << 20)
	for i := 0; i < b.N; i++ {
		e.BeforeAuth(raw)
		for len(e.Handoff) > 0 {
			w := <-e.Handoff
			e.Pool.Release(w.Begin.Reserved)
		}
	}
}
