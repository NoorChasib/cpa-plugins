package plugin_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/capture"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/plugin"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/plugin/plugintest"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/uploader"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

func setVar[T any](t *testing.T, p *T, v T) {
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// bodies returns every uploaded envelope body by key, and how often each key arrived.
func bodies(h *plugintest.Harness) (map[string]json.RawMessage, map[string]int) {
	out, seen := map[string]json.RawMessage{}, map[string]int{}
	for _, r := range h.Srv.Envelopes(h.SourceID) {
		out[r.Envelope.Key] = r.Envelope.Body
		seen[r.Envelope.Key]++
	}
	return out, seen
}

func decode[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

func wsEvent(s string) []byte { return []byte(s) }

func wsTurn(id, prev, text string) [][]byte {
	created := `{"type":"response.created","response":{"id":"` + id + `"`
	if prev != "" {
		created += `,"previous_response_id":"` + prev + `"`
	}
	return [][]byte{
		wsEvent(created + `}}`),
		wsEvent(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + text + `"}]}}`),
		wsEvent(`{"type":"response.completed","response":{"id":"` + id + `","status":"completed","output":[]}}`),
	}
}

// TestReplacementContinuesOpenExchanges: CPA replaces the plugin instance for a version update
// (plugin.quiesce on the old one, then a new instance with a new engine registers on the same
// outbox and gets the rest of every open exchange). A Claude stream that outlasts the drain bound
// and a Codex Responses WebSocket left open across the replacement both keep both halves: the old
// instance writes what it saw, the new one continues the request's numbering, so its envelopes
// get new keys and none is skipped as a duplicate.
func TestReplacementContinuesOpenExchanges(t *testing.T) {
	setStopDrain(t, 1500*time.Millisecond)
	h := plugintest.New(t, nil, "")
	p1 := h.Plugin
	meta := plugintest.Meta("/v1/messages")
	ws := map[string]any{"request_path": "/v1/responses", "execution_session_id": "rp-session"}

	// The stream: begun and streaming on the old instance.
	p1.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth("rp-st", "claude", plugintest.ClaudeBody("q"), nil, meta))
	p1.Hook(protocol.MethodRequestInterceptAfter, plugintest.AfterAuth("rp-st"))
	p1.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("rp-st", "claude", -1, nil, meta))
	p1.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("rp-st", "claude", 0, append(claudeStart, claudeDelta("head-")...), meta))
	// The WebSocket: one RequestID for the socket, turn 0 answered on the old instance, socket open.
	p1.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth("rp-ws", "openai-response", []byte(`{"type":"response.create","input":"turn0"}`), nil, ws))
	p1.Hook(protocol.MethodRequestInterceptAfter, plugintest.AfterAuth("rp-ws"))
	p1.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("rp-ws", "openai-response", -1, nil, ws))
	wsIndex := 0
	for _, c := range wsTurn("resp_t0", "", "turn-zero-text") {
		p1.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("rp-ws", "openai-response", wsIndex, c, ws))
		wsIndex++
	}

	// The update: quiesce the old instance while the stream keeps streaming to it.
	const during = 25
	start := time.Now()
	done := make(chan struct{})
	go func() {
		p1.Handle(protocol.MethodPluginQuiesce, nil)
		close(done)
	}()
	plugintest.WaitFor(t, "drain started", func() bool { st, why := p1.State(); return st == plugin.StateStopping && why == "draining" })
	var sentAll atomic.Bool
	for i := 1; i <= during; i++ {
		p1.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("rp-st", "claude", i, claudeDelta("m"), meta))
		time.Sleep(10 * time.Millisecond)
	}
	sentAll.Store(true)
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("quiesce did not return")
	}
	if !sentAll.Load() || time.Since(start) < plugin.StopDrainTimeout {
		t.Fatalf("quiesce returned after %v, before the drain bound with both exchanges open", time.Since(start))
	}
	if d := p1.Engine().Stats.Dropped(); d["stopped"] != 0 || d["duplicate_key"] != 0 {
		t.Fatalf("old instance dropped = %v", d)
	}

	// The new instance: a new engine on the same outbox gets the rest of both exchanges.
	p2 := plugin.New(nil)
	if _, err := p2.Handle(protocol.MethodPluginRegister, plugintest.Lifecycle(h.YAML)); err != nil {
		t.Fatalf("register new instance: %v", err)
	}
	t.Cleanup(p2.Shutdown)
	p2.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("rp-st", "claude", during+1, append(claudeDelta("tail-marker"), claudeStop...), meta))
	p2.Hook(protocol.MethodRequestComplete, plugintest.Complete("rp-st", "succeeded", meta))
	p2.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth("rp-ws", "openai-response",
		[]byte(`{"type":"response.create","input":"turn1","previous_response_id":"resp_t0"}`), nil, ws))
	p2.Hook(protocol.MethodRequestInterceptAfter, plugintest.AfterAuth("rp-ws"))
	for _, c := range wsTurn("resp_t1", "resp_t0", "turn-one-text") {
		p2.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("rp-ws", "openai-response", wsIndex, c, ws))
		wsIndex++
	}
	// CPA reports a duplex RequestID's end (the socket closing after its last turn) as canceled.
	p2.Hook(protocol.MethodRequestComplete, plugintest.Complete("rp-ws", "canceled", ws))

	want := []string{
		wire.ExchangeBeginKey("rp-st", 0), wire.ExchangeAttemptKey("rp-st", 1), wire.ExchangeResponseKey("rp-st", 0),
		wire.ExchangeResponseKey("rp-st", 1), wire.ExchangeCompletionKey("rp-st"),
		wire.ExchangeBeginKey("rp-ws", 0), wire.ExchangeBeginKey("rp-ws", 1), wire.ExchangeAttemptKey("rp-ws", 1),
		wire.ExchangeAttemptKey("rp-ws", 2), wire.ExchangeResponseKey("rp-ws", 0), wire.ExchangeResponseKey("rp-ws", 1),
		wire.ExchangeCompletionKey("rp-ws"),
	}
	var got map[string]json.RawMessage
	var seen map[string]int
	plugintest.WaitFor(t, "both halves archived", func() bool {
		got, seen = bodies(h)
		for _, k := range want {
			if got[k] == nil {
				return false
			}
		}
		return true
	})
	for k, n := range seen {
		if n != 1 {
			t.Errorf("key %s uploaded %d times", k, n)
		}
	}
	if n := len(got); n != len(want) {
		t.Errorf("archived %d keys, want %d", n, len(want))
	}

	head := decode[wire.ExchangeResponse](t, got[wire.ExchangeResponseKey("rp-st", 0)])
	tail := decode[wire.ExchangeResponse](t, got[wire.ExchangeResponseKey("rp-st", 1)])
	if head.TerminalStatus != "incomplete" || head.IncompleteReason != capture.IncompleteStopped || !head.Truncated || head.Chunks != during+1 {
		t.Errorf("stream head = status %q reason %q truncated %v chunks %d", head.TerminalStatus, head.IncompleteReason, head.Truncated, head.Chunks)
	}
	if tail.TerminalStatus != "completed" || !tail.Truncated || tail.Index != 1 {
		t.Errorf("stream tail = status %q truncated %v index %d", tail.TerminalStatus, tail.Truncated, tail.Index)
	}
	if c := decode[wire.ExchangeCompletion](t, got[wire.ExchangeCompletionKey("rp-st")]); c.Responses != 2 || !c.BeginWritten || c.Outcome != "succeeded" {
		t.Errorf("stream completion = responses %d begin_written %v outcome %q", c.Responses, c.BeginWritten, c.Outcome)
	}
	if b := decode[wire.ExchangeBegin](t, got[wire.ExchangeBeginKey("rp-ws", 1)]); b.InputSeq != 1 {
		t.Errorf("websocket turn 1 begin input_seq = %d", b.InputSeq)
	}
	w0 := decode[wire.ExchangeResponse](t, got[wire.ExchangeResponseKey("rp-ws", 0)])
	w1 := decode[wire.ExchangeResponse](t, got[wire.ExchangeResponseKey("rp-ws", 1)])
	if w0.ResponseID != "resp_t0" || w0.TerminalStatus != "completed" || w1.ResponseID != "resp_t1" || w1.PrevResponseID != "resp_t0" ||
		w1.TerminalStatus != "completed" || w1.Index != 1 {
		t.Errorf("websocket responses = %q %q / %q prev %q %q index %d", w0.ResponseID, w0.TerminalStatus, w1.ResponseID, w1.PrevResponseID, w1.TerminalStatus, w1.Index)
	}
	if a := decode[wire.ExchangeAttempt](t, got[wire.ExchangeAttemptKey("rp-ws", 2)]); a.Ordinal != 2 {
		t.Errorf("websocket turn 1 attempt ordinal = %d", a.Ordinal)
	}
	if c := decode[wire.ExchangeCompletion](t, got[wire.ExchangeCompletionKey("rp-ws")]); c.Responses != 2 || !c.BeginWritten || c.Outcome != "succeeded" {
		t.Errorf("websocket completion = responses %d begin_written %v outcome %q", c.Responses, c.BeginWritten, c.Outcome)
	}
	for _, m := range []string{"head-" + strings.Repeat("m", during), "tail-marker", "turn-zero-text", "turn-one-text"} {
		if !piecesContain(h, m) {
			t.Errorf("pieces miss %q", m)
		}
	}
	if d := p2.Engine().Stats.Dropped(); d["stopped"] != 0 || d["duplicate_key"] != 0 {
		t.Fatalf("new instance dropped = %v", d)
	}
}

// TestDuplicateKeyReportedAsDropped: when a continuation does reuse a key the outbox already
// holds (here the new instance found no saved numbering), the skipped envelope is a loss and the
// heartbeat reports it as dropped.duplicate_key.
func TestDuplicateKeyReportedAsDropped(t *testing.T) {
	setStopDrain(t, 0)
	setVar(t, &plugin.CarryMaxAge, -time.Hour) // the saved numbering is too old to adopt
	setVar(t, &uploader.HeartbeatEvery, 200*time.Millisecond)
	h := plugintest.New(t, nil, "")
	meta := plugintest.Meta("/v1/messages")
	h.Plugin.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth("dk-st", "claude", plugintest.ClaudeBody("q"), nil, meta))
	h.Plugin.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("dk-st", "claude", -1, nil, meta))
	h.Plugin.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("dk-st", "claude", 0, append(claudeStart, claudeDelta("head")...), meta))
	h.Plugin.Handle(protocol.MethodPluginQuiesce, nil)
	p2 := plugin.New(nil)
	if _, err := p2.Handle(protocol.MethodPluginRegister, plugintest.Lifecycle(h.YAML)); err != nil {
		t.Fatalf("register new instance: %v", err)
	}
	t.Cleanup(p2.Shutdown)
	p2.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("dk-st", "claude", 1, append(claudeDelta("tail"), claudeStop...), meta))
	p2.Hook(protocol.MethodRequestComplete, plugintest.Complete("dk-st", "succeeded", meta))
	plugintest.WaitFor(t, "dropped.duplicate_key in a heartbeat", func() bool {
		for _, hb := range h.Srv.Heartbeats(h.SourceID) {
			if hb.Dropped["duplicate_key"] == 1 {
				return true
			}
		}
		return false
	})
	if n := p2.Engine().Stats.Other()["duplicate_key"]; n != 0 {
		t.Fatalf("duplicate_key still counted outside dropped: %d", n)
	}
}

// TestDrainWaitsOnlyForRequestsOpenAtStart: under steady traffic some exchange is always open, so
// a drain that waited for no open request at all would always run to its bound. The drain waits
// only for the exchanges open when it began: it returns once they finished.
func TestDrainWaitsOnlyForRequestsOpenAtStart(t *testing.T) {
	setStopDrain(t, 10*time.Second)
	h := plugintest.New(t, nil, "")
	p := h.Plugin
	meta := plugintest.Meta("/v1/messages")
	p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth("dw-old", "claude", plugintest.ClaudeBody("x"), nil, meta))
	stop := make(chan struct{})
	traffic := make(chan struct{})
	go func() {
		// Overlapping exchanges: each new one begins before the previous one finishes.
		defer close(traffic)
		prev := ""
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			rid := "dw-" + strings.Repeat("n", 1+i%3) + "-" + time.Now().Format("150405.000000")
			p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth(rid, "claude", plugintest.ClaudeBody("y"), nil, meta))
			if prev != "" {
				p.Hook(protocol.MethodRequestComplete, plugintest.Complete(prev, "succeeded", meta))
			}
			prev = rid
			time.Sleep(5 * time.Millisecond)
		}
	}()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	done := make(chan struct{})
	go func() {
		p.Handle(protocol.MethodPluginQuiesce, nil)
		close(done)
	}()
	plugintest.WaitFor(t, "drain started", func() bool { st, why := p.State(); return st == plugin.StateStopping && why == "draining" })
	time.Sleep(200 * time.Millisecond)
	resp := []byte(`{"id":"msg_old","type":"message","role":"assistant","content":[{"type":"text","text":"reply"}]}`)
	p.Hook(protocol.MethodResponseInterceptAfter, plugintest.Response("dw-old", "claude", resp, meta))
	p.Hook(protocol.MethodRequestComplete, plugintest.Complete("dw-old", "succeeded", meta))
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("the drain waited for exchanges begun after it started")
	}
	close(stop)
	<-traffic
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("quiesce took %v under traffic", d)
	}
	p.Handle(protocol.MethodPluginReconfigure, plugintest.Lifecycle(h.YAML))
	plugintest.WaitFor(t, "the exchange open at the stop archived", func() bool {
		rs, cs, _ := archived(h)
		_, ok := rs[wire.ExchangeResponseKey("dw-old", 0)]
		return ok && cs["dw-old"].Responses == 1
	})
}

// TestStopWithinOneDeadline: the drain, the uploader stop (here stuck in a held commit, which
// alone would take uploader.StopDeadline plus FinalHeartbeat), the capture drain and the final
// saves all end by one deadline, StopTimeout after the stop began.
func TestStopWithinOneDeadline(t *testing.T) {
	setStopDrain(t, 15*time.Second)
	setVar(t, &plugin.StopTimeout, 2500*time.Millisecond)
	setVar(t, &plugin.StopAfterDrain, time.Second)
	setVar(t, &plugin.CaptureStopMin, 300*time.Millisecond)
	setVar(t, &plugin.FinalSaveTimeout, 200*time.Millisecond)
	h := plugintest.New(t, nil, "")
	p := h.Plugin
	release := h.Srv.Hold("POST " + wire.PathCommit)
	defer release()
	meta := plugintest.Meta("/v1/messages")
	p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth("dl-open", "claude", plugintest.ClaudeBody("x"), nil, meta))
	ob, _, _ := p.Runtime()
	plugintest.WaitFor(t, "the uploader inside the held commit", func() bool { return ob.Meta().NextSeq > 1 })
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	p.Handle(protocol.MethodPluginQuiesce, nil)
	d := time.Since(start)
	if d < plugin.StopTimeout-plugin.StopAfterDrain-100*time.Millisecond {
		t.Fatalf("quiesce took %v: the drain did not wait for the open exchange", d)
	}
	if d > plugin.StopTimeout+time.Second {
		t.Fatalf("quiesce took %v, past StopTimeout %v", d, plugin.StopTimeout)
	}
	t.Logf("quiesce with an open exchange and a held commit took %v (StopTimeout %v)", d, plugin.StopTimeout)
}

// TestStopWithSlowOutboxClose: the outbox close comes after the final saves, which end at the
// stop's deadline. A close that cannot get the write connection (here it waits as long as its
// context lets it, up to outbox.CloseTimeout) does not extend the stop past the deadline plus
// StopCloseMargin, and the next runtime still opens the outbox.
func TestStopWithSlowOutboxClose(t *testing.T) {
	setStopDrain(t, 15*time.Second)
	setVar(t, &plugin.StopTimeout, 2500*time.Millisecond)
	setVar(t, &plugin.StopAfterDrain, time.Second)
	setVar(t, &plugin.CaptureStopMin, 300*time.Millisecond)
	setVar(t, &plugin.FinalSaveTimeout, 200*time.Millisecond)
	h := plugintest.New(t, nil, "")
	// Set after New, so the harness's final shutdown closes the outbox normally.
	var closeBy atomic.Int64
	plugin.SetCloseOutbox(t, func(ob *outbox.Outbox, ctx context.Context) error {
		if dl, ok := ctx.Deadline(); ok {
			closeBy.Store(dl.UnixNano())
		}
		select {
		case <-ctx.Done():
		case <-time.After(outbox.CloseTimeout):
		}
		return ob.CloseContext(ctx)
	})
	p := h.Plugin
	release := h.Srv.Hold("POST " + wire.PathCommit)
	defer release()
	meta := plugintest.Meta("/v1/messages")
	p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth("sc-open", "claude", plugintest.ClaudeBody("x"), nil, meta))
	ob, _, _ := p.Runtime()
	plugintest.WaitFor(t, "the uploader inside the held commit", func() bool { return ob.Meta().NextSeq > 1 })
	start := time.Now()
	p.Handle(protocol.MethodPluginQuiesce, nil)
	d := time.Since(start)
	limit := plugin.StopTimeout + plugin.StopCloseMargin + 250*time.Millisecond
	if plugin.StopCloseMargin > time.Second || d > limit {
		t.Fatalf("quiesce with a slow outbox close took %v (StopTimeout %v, margin %v)", d, plugin.StopTimeout, plugin.StopCloseMargin)
	}
	if by := time.Unix(0, closeBy.Load()); by.Sub(start) > plugin.StopTimeout+plugin.StopCloseMargin+100*time.Millisecond {
		t.Fatalf("the close's bound ends %v after the stop began", by.Sub(start))
	}
	t.Logf("quiesce with a held commit and a slow outbox close took %v (StopTimeout %v, margin %v)", d, plugin.StopTimeout, plugin.StopCloseMargin)
	release()
	p.Handle(protocol.MethodPluginReconfigure, plugintest.Lifecycle(h.YAML))
	if ob2, _, _ := p.Runtime(); ob2 == nil {
		st, why := p.State()
		t.Fatalf("no runtime after the restart: %s %s", st, why)
	}
}
