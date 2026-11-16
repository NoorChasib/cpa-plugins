package plugin_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/capture"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/plugin"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/plugin/plugintest"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

func setStopDrain(t *testing.T, d time.Duration) {
	old := plugin.StopDrainTimeout
	plugin.StopDrainTimeout = d
	t.Cleanup(func() { plugin.StopDrainTimeout = old })
}

func claudeEvent(typ, data string) []byte {
	return []byte("event: " + typ + "\ndata: " + data + "\n\n")
}

func claudeDelta(text string) []byte {
	return claudeEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+text+`"}}`)
}

var claudeStart = append(claudeEvent("message_start", `{"type":"message_start","message":{"id":"msg_drain","role":"assistant","content":[]}}`),
	claudeEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)...)

var claudeStop = append(claudeEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
	claudeEvent("message_stop", `{"type":"message_stop"}`)...)

// archived collects the uploaded responses (by key) and completions (by RequestID).
func archived(h *plugintest.Harness) (map[string]wire.ExchangeResponse, map[string]wire.ExchangeCompletion, map[string]bool) {
	resp, comp, keys := map[string]wire.ExchangeResponse{}, map[string]wire.ExchangeCompletion{}, map[string]bool{}
	for _, r := range h.Srv.Envelopes(h.SourceID) {
		keys[r.Envelope.Key] = true
		switch r.Envelope.Kind {
		case wire.KindExchangeResponse:
			var x wire.ExchangeResponse
			json.Unmarshal(r.Envelope.Body, &x)
			resp[r.Envelope.Key] = x
		case wire.KindExchangeDone:
			var x wire.ExchangeCompletion
			json.Unmarshal(r.Envelope.Body, &x)
			comp[x.RequestID] = x
		}
	}
	return resp, comp, keys
}

// TestQuiesceCapturesRepliesOfBegunExchanges: CPA keeps sending every hook to the old instance
// until plugin.quiesce returns and the new version is loaded. A quiesce therefore keeps capturing
// until no exchange is open, so the response, stream chunks and completion of exchanges begun
// before it are archived, and returns at that quiet point, well before StopDrainTimeout.
func TestQuiesceCapturesRepliesOfBegunExchanges(t *testing.T) {
	setStopDrain(t, 30*time.Second)
	h := plugintest.New(t, nil, "")
	p := h.Plugin
	e := p.Engine()
	meta := plugintest.Meta("/v1/messages")
	// A non-stream and a stream exchange, both begun, neither answered yet.
	p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth("dr-ns", "claude", plugintest.ClaudeBody("x"), nil, meta))
	p.Hook(protocol.MethodRequestInterceptAfter, plugintest.AfterAuth("dr-ns"))
	p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth("dr-st", "claude", plugintest.ClaudeBody("y"), nil, meta))
	p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("dr-st", "claude", -1, nil, meta))
	p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("dr-st", "claude", 0, append(claudeStart, claudeDelta("head")...), meta))

	start := time.Now()
	done := make(chan struct{})
	go func() {
		p.Handle(protocol.MethodPluginQuiesce, nil)
		close(done)
	}()
	plugintest.WaitFor(t, "quiesce started", func() bool {
		select {
		case <-done:
			return true // already over: the replies below arrive too late
		default:
		}
		st, _ := p.State()
		return st == plugin.StateStopping
	})
	time.Sleep(50 * time.Millisecond)
	// The replies arrive while the quiesce runs.
	resp := []byte(`{"id":"msg_ns","type":"message","role":"assistant","content":[{"type":"text","text":"reply"}]}`)
	p.Hook(protocol.MethodResponseInterceptAfter, plugintest.Response("dr-ns", "claude", resp, meta))
	p.Hook(protocol.MethodRequestComplete, plugintest.Complete("dr-ns", "succeeded", meta))
	p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("dr-st", "claude", 1, append(claudeDelta("tail"), claudeStop...), meta))
	p.Hook(protocol.MethodRequestComplete, plugintest.Complete("dr-st", "succeeded", meta))
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("quiesce did not return at the quiet point")
	}
	if d := time.Since(start); d >= plugin.StopDrainTimeout {
		t.Fatalf("quiesce took %v, the whole drain bound", d)
	}
	if st, _ := p.State(); st != plugin.StateStopped {
		t.Fatalf("state after quiesce = %s", st)
	}
	if n := e.Stats.Dropped()["stopped"]; n != 0 {
		t.Fatalf("dropped.stopped = %d, want 0", n)
	}
	// Rollback-style restart: the new runtime uploads what the quiesced one wrote.
	p.Handle(protocol.MethodPluginReconfigure, plugintest.Lifecycle(h.YAML))
	var rs map[string]wire.ExchangeResponse
	var cs map[string]wire.ExchangeCompletion
	plugintest.WaitFor(t, "replies archived", func() bool {
		rs, cs, _ = archived(h)
		return len(cs) == 2 && len(rs) == 2
	})
	for _, rid := range []string{"dr-ns", "dr-st"} {
		r := rs[wire.ExchangeResponseKey(rid, 0)]
		if r.TerminalStatus != "completed" || r.Truncated {
			t.Errorf("%s response = status %q truncated %v", rid, r.TerminalStatus, r.Truncated)
		}
		if c := cs[rid]; !c.BeginWritten || c.Responses != 1 || c.Outcome != "succeeded" {
			t.Errorf("%s completion = %+v", rid, c)
		}
	}
	if !piecesContain(h, "headtail") {
		t.Error("the stream's text is not whole")
	}
}

// TestStopWritesOpenStreamAsPartial: a stream still open when the drain bound passes is not
// discarded. The stop processes every chunk the hooks queued and writes the stream as a partial
// response (incomplete, plugin_stopped, truncated). The rest of the stream, which CPA sends to
// the next runtime, is still admitted and written as the next response, marked truncated.
func TestStopWritesOpenStreamAsPartial(t *testing.T) {
	setStopDrain(t, 0)
	h := plugintest.New(t, nil, "")
	p := h.Plugin
	e := p.Engine()
	meta := plugintest.Meta("/v1/messages")
	const queued = 300
	p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth("dr-open", "claude", plugintest.ClaudeBody("z"), nil, meta))
	p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("dr-open", "claude", -1, nil, meta))
	p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("dr-open", "claude", 0, append(claudeStart, claudeDelta("head-marker-")...), meta))
	for i := 1; i < queued; i++ {
		p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("dr-open", "claude", i, claudeDelta("h"), meta))
	}
	p.Handle(protocol.MethodPluginQuiesce, nil) // no drain time: admission closes at once
	if d := e.Stats.Dropped(); d["stopped"] != 0 || d["stream_overflow"] != 0 {
		t.Fatalf("dropped = %v", d)
	}
	p.Handle(protocol.MethodPluginReconfigure, plugintest.Lifecycle(h.YAML))
	p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("dr-open", "claude", queued, append(claudeDelta("tail-marker"), claudeStop...), meta))
	p.Hook(protocol.MethodRequestComplete, plugintest.Complete("dr-open", "succeeded", meta))
	var rs map[string]wire.ExchangeResponse
	var cs map[string]wire.ExchangeCompletion
	plugintest.WaitFor(t, "completion archived", func() bool {
		rs, cs, _ = archived(h)
		return len(cs) == 1 && len(rs) == 2
	})
	head, tail := rs[wire.ExchangeResponseKey("dr-open", 0)], rs[wire.ExchangeResponseKey("dr-open", 1)]
	if !head.Stream || head.TerminalStatus != "incomplete" || head.IncompleteReason != capture.IncompleteStopped || !head.Truncated ||
		head.Chunks != queued || head.ResponseID != "msg_drain" {
		t.Fatalf("head = status %q reason %q truncated %v chunks %d id %q", head.TerminalStatus, head.IncompleteReason, head.Truncated, head.Chunks, head.ResponseID)
	}
	if !tail.Truncated || tail.TerminalStatus != "completed" || tail.Chunks != 1 {
		t.Fatalf("tail = status %q truncated %v chunks %d", tail.TerminalStatus, tail.Truncated, tail.Chunks)
	}
	if c := cs["dr-open"]; c.Responses != 2 || !c.BeginWritten {
		t.Fatalf("completion = %+v", c)
	}
	if !piecesContain(h, "head-marker-"+strings.Repeat("h", queued-1)) || !piecesContain(h, "tail-marker") {
		t.Fatal("stream text missing from pieces")
	}
	if d := e.Stats.Dropped(); d["stopped"] != 0 || d["stream_overflow"] != 0 {
		t.Fatalf("dropped = %v", d)
	}
}

// TestStreamPickedUpMidwayIsTruncated: chunks of a stream that started before this plugin
// instance (CPA sent its header-init and first chunks to the previous version) are archived as a
// response marked truncated, not as a whole one.
func TestStreamPickedUpMidwayIsTruncated(t *testing.T) {
	h := plugintest.New(t, nil, "")
	p := h.Plugin
	meta := plugintest.Meta("/v1/messages")
	p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("dr-mid", "claude", 7, append(claudeDelta("late"), claudeStop...), meta))
	p.Hook(protocol.MethodRequestComplete, plugintest.Complete("dr-mid", "succeeded", meta))
	var rs map[string]wire.ExchangeResponse
	plugintest.WaitFor(t, "response archived", func() bool {
		rs, _, _ = archived(h)
		return len(rs) == 1
	})
	if r := rs[wire.ExchangeResponseKey("dr-mid", 0)]; !r.Truncated {
		t.Fatalf("response picked up mid-stream = truncated %v status %q", r.Truncated, r.TerminalStatus)
	}
}

// TestCaptureOffReconfigureDoesNotDrain: turning capture off is the kill switch; it stops at
// once even with an exchange open.
func TestCaptureOffReconfigureDoesNotDrain(t *testing.T) {
	setStopDrain(t, 30*time.Second)
	h := plugintest.New(t, nil, "")
	p := h.Plugin
	meta := plugintest.Meta("/v1/messages")
	p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth("dr-off", "claude", plugintest.ClaudeBody("x"), nil, meta))
	start := time.Now()
	p.Handle(protocol.MethodPluginReconfigure, plugintest.Lifecycle(h.YAML+"capture: \"off\"\n"))
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("capture off took %v", d)
	}
	if st, why := p.State(); st != plugin.StateUnconfigured || why != "capture_off" {
		t.Fatalf("state %s %s", st, why)
	}
}
