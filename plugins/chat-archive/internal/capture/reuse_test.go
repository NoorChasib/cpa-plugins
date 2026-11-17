package capture_test

// Reused RequestIDs (CPA v8.0.76 and later): an in-place WebSocket failover runs the retry as a
// second lifecycle under the failed attempt's RequestID. These tests drive the plugin's hooks the way
// CPA calls them, in the arrival orders of the 0.1.5 design (O1 to O9), and check that no key is used
// twice, that every lifecycle's completion and captured content is stored, and that the retry's
// completion is keyed by its started_at. Every value is synthetic.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/plugin/plugintest"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// stamp returns a wall-clock time strictly after every hook already called and before the next.
func stamp() time.Time {
	time.Sleep(200 * time.Microsecond)
	t := time.Now().UTC()
	time.Sleep(200 * time.Microsecond)
	return t
}

// completeAt builds request.complete with explicit lifecycle times, as CPA's tracker sets them.
func completeAt(rid, outcome string, status int, started, completed time.Time, meta map[string]any) []byte {
	errText := ""
	if outcome != "succeeded" {
		errText = "synthetic upstream failure"
	}
	b, _ := json.Marshal(map[string]any{"RequestID": rid, "TraceID": "trace-" + rid, "SourceFormat": "openai-response", "Model": "model-x",
		"RequestedModel": "model-x", "Stream": true, "Outcome": outcome, "StatusCode": status, "Error": errText,
		"StartedAt": started, "CompletedAt": completed, "Metadata": meta})
	return b
}

type stored struct {
	keys  map[string]bool
	comp  map[string]wire.ExchangeCompletion // by key
	resp  map[string]wire.ExchangeResponse   // by key
	begin map[string]wire.ExchangeBegin      // by key
}

func storedFor(h *plugintest.Harness, rid string) stored {
	s := stored{keys: map[string]bool{}, comp: map[string]wire.ExchangeCompletion{}, resp: map[string]wire.ExchangeResponse{},
		begin: map[string]wire.ExchangeBegin{}}
	for _, r := range h.Srv.Envelopes(h.SourceID) {
		k := r.Envelope.Key
		if !strings.HasPrefix(k, "ex/"+rid+"/") {
			continue
		}
		s.keys[k] = true
		switch r.Envelope.Kind {
		case wire.KindExchangeDone:
			var x wire.ExchangeCompletion
			json.Unmarshal(r.Envelope.Body, &x)
			s.comp[k] = x
		case wire.KindExchangeResponse:
			var x wire.ExchangeResponse
			json.Unmarshal(r.Envelope.Body, &x)
			s.resp[k] = x
		case wire.KindExchangeBegin:
			var x wire.ExchangeBegin
			json.Unmarshal(r.Envelope.Body, &x)
			s.begin[k] = x
		}
	}
	return s
}

func wsMeta(rid string) map[string]any {
	return map[string]any{"request_path": "/v1/responses", "execution_session_id": "ws-" + rid}
}

func wsCreate(text string) []byte {
	return []byte(`{"type":"response.create","model":"model-x","input":"` + text + `"}`)
}

const (
	wsCreated  = `{"type":"response.created","response":{"id":"resp_l1"}}`
	wsCreated2 = `{"type":"response.created","response":{"id":"resp_l2"}}`
	wsItem     = `{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"retry-answer-marker"}]}}`
	wsDone2    = `{"type":"response.completed","response":{"id":"resp_l2","status":"completed","output":[]}}`
)

// checkReuse waits for both completions and checks the invariants every arrival order keeps.
func checkReuse(t *testing.T, h *plugintest.Harness, rid string, s1, s2 time.Time, wantResponses int) stored {
	t.Helper()
	bare, at2 := wire.ExchangeCompletionKey(rid), wire.ExchangeCompletionKeyAt(rid, s2)
	var s stored
	plugintest.WaitFor(t, "both completions of "+rid, func() bool {
		s = storedFor(h, rid)
		return s.keys[bare] && s.keys[at2] && len(s.resp) >= wantResponses
	})
	e := h.Plugin.Engine()
	if n := e.Stats.Dropped()["duplicate_key"]; n != 0 {
		t.Fatalf("dropped.duplicate_key = %d, want 0", n)
	}
	if c := s.comp[bare]; !c.StartedAt.Equal(s1) || c.Outcome != "failed" {
		t.Fatalf("bare completion = %+v, want the failed attempt", c)
	}
	if c := s.comp[at2]; !c.StartedAt.Equal(s2) || c.Outcome != "succeeded" {
		t.Fatalf("started_at completion = %+v, want the retry", c)
	}
	for _, k := range []string{"ex/" + rid + "/begin/0", "ex/" + rid + "/begin/1", "ex/" + rid + "/attempt/1", "ex/" + rid + "/attempt/2"} {
		if !s.keys[k] {
			t.Fatalf("missing %s; stored %v", k, s.keys)
		}
	}
	if len(s.comp) != 2 || len(s.resp) != wantResponses {
		t.Fatalf("stored %d completions and %d responses, want 2 and %d: %v", len(s.comp), len(s.resp), wantResponses, s.keys)
	}
	if n := e.Stats.Other()["reused_request_id"]; n != 1 {
		t.Fatalf("reused_request_id = %d, want 1", n)
	}
	plugintest.WaitFor(t, "state released", func() bool { return e.Entries() == 0 && e.Pool.Used() == 0 })
	if n := e.Stats.Other()["state_evicted"]; n != 0 {
		t.Fatalf("state_evicted = %d", n)
	}
	return s
}

func waitKey(t *testing.T, h *plugintest.Harness, key string) {
	t.Helper()
	plugintest.WaitFor(t, key, func() bool {
		for _, r := range h.Srv.Envelopes(h.SourceID) {
			if r.Envelope.Key == key {
				return true
			}
		}
		return false
	})
}

// lifecycleNonStream runs one non-stream lifecycle's synchronous hooks.
func lifecycleNonStream(h *plugintest.Harness, rid, text string, respond bool) {
	p, m := h.Plugin, wsMeta(rid)
	p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth(rid, "openai-response", wsCreate(text), nil, m))
	p.Hook(protocol.MethodRequestInterceptAfter, plugintest.AfterAuth(rid))
	if respond {
		p.Hook(protocol.MethodResponseInterceptAfter, plugintest.Response(rid, "openai-response",
			[]byte(`{"id":"resp_l2","object":"response","status":"completed","output":[]}`), m))
	}
}

// TestReusedRequestIDCompletionTakenFirst (O1): the failed attempt's completion is taken before the
// retry's first hook. 0.1.4 deleted the entry, so the retry numbered from zero again and its begin,
// attempt and completion were dropped as duplicate keys.
func TestReusedRequestIDCompletionTakenFirst(t *testing.T) {
	h := plugintest.New(t, nil, "")
	rid := "reuse-o1"
	s1 := stamp()
	lifecycleNonStream(h, rid, "turn-one", false)
	c1 := stamp()
	h.Plugin.Hook(protocol.MethodRequestComplete, completeAt(rid, "failed", 429, s1, c1, wsMeta(rid)))
	waitKey(t, h, wire.ExchangeCompletionKey(rid))
	s2 := stamp()
	lifecycleNonStream(h, rid, "turn-one", true)
	c2 := stamp()
	h.Plugin.Hook(protocol.MethodRequestComplete, completeAt(rid, "succeeded", 200, s2, c2, wsMeta(rid)))
	s := checkReuse(t, h, rid, s1, s2, 1)
	if n := s.comp[wire.ExchangeCompletionKeyAt(rid, s2)].LifecycleInputSeq; n != 1 {
		t.Fatalf("lifecycle_input_seq = %d, want 1 (begin/1 replays the turn)", n)
	}
	if !s.keys["ex/"+rid+"/response/0"] {
		t.Fatalf("retry response missing: %v", s.keys)
	}
}

// TestReusedRequestIDRetryCompletesFirst (O6): the retry's completion arrives before the failed
// attempt's. The failed attempt keeps the bare key.
func TestReusedRequestIDRetryCompletesFirst(t *testing.T) {
	h := plugintest.New(t, nil, "")
	rid := "reuse-o6"
	s1 := stamp()
	lifecycleNonStream(h, rid, "turn-one", false)
	c1 := stamp()
	s2 := stamp()
	lifecycleNonStream(h, rid, "turn-one", true)
	c2 := stamp()
	h.Plugin.Hook(protocol.MethodRequestComplete, completeAt(rid, "succeeded", 200, s2, c2, wsMeta(rid)))
	waitKey(t, h, wire.ExchangeCompletionKeyAt(rid, s2))
	h.Plugin.Hook(protocol.MethodRequestComplete, completeAt(rid, "failed", 429, s1, c1, wsMeta(rid)))
	checkReuse(t, h, rid, s1, s2, 1)
}

// TestReusedRequestIDLateCompletionAfterRetry (O8): the failed attempt's lifecycle completes after
// every hook of the retry, and its completion is taken first.
func TestReusedRequestIDLateCompletionAfterRetry(t *testing.T) {
	h := plugintest.New(t, nil, "")
	rid := "reuse-o8"
	s1 := stamp()
	lifecycleNonStream(h, rid, "turn-one", false)
	s2 := stamp()
	lifecycleNonStream(h, rid, "turn-one", true)
	waitKey(t, h, "ex/"+rid+"/response/0")
	c1 := stamp()
	h.Plugin.Hook(protocol.MethodRequestComplete, completeAt(rid, "failed", 429, s1, c1, wsMeta(rid)))
	waitKey(t, h, wire.ExchangeCompletionKey(rid))
	c2 := stamp()
	h.Plugin.Hook(protocol.MethodRequestComplete, completeAt(rid, "succeeded", 200, s2, c2, wsMeta(rid)))
	checkReuse(t, h, rid, s1, s2, 1)
}

// streamL1 runs a failed attempt's hooks: before-auth, after-auth, header-init and one bootstrap
// event that never reached the client.
func streamL1(h *plugintest.Harness, rid string) {
	p, m := h.Plugin, wsMeta(rid)
	p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth(rid, "openai-response", wsCreate("turn-one"), nil, m))
	p.Hook(protocol.MethodRequestInterceptAfter, plugintest.AfterAuth(rid))
	p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(rid, "openai-response", -1, nil, m))
	p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(rid, "openai-response", 0, []byte(wsCreated), m))
}

// streamL2 runs the retry's before-auth, after-auth, header-init and first events.
func streamL2Start(h *plugintest.Harness, rid string) {
	p, m := h.Plugin, wsMeta(rid)
	p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth(rid, "openai-response", wsCreate("turn-one"), nil, m))
	p.Hook(protocol.MethodRequestInterceptAfter, plugintest.AfterAuth(rid))
	p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(rid, "openai-response", -1, nil, m))
	p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(rid, "openai-response", 0, []byte(wsCreated2), m))
}

func streamL2End(h *plugintest.Harness, rid string) {
	p, m := h.Plugin, wsMeta(rid)
	p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(rid, "openai-response", 1, []byte(wsItem), m))
	p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(rid, "openai-response", 2, []byte(wsDone2), m))
}

// checkStreams requires the failed attempt's buffered event and the retry's answer, each stored once.
func checkStreams(t *testing.T, s stored) {
	t.Helper()
	var l1, l2 int
	for _, r := range s.resp {
		switch r.ResponseID {
		case "resp_l1":
			l1++
			if r.TerminalStatus != "failed" {
				t.Errorf("failed attempt's partial response: terminal_status %q, want failed", r.TerminalStatus)
			}
		case "resp_l2":
			l2++
			if r.TerminalStatus != "completed" {
				t.Errorf("retry response: terminal_status %q, want completed", r.TerminalStatus)
			}
		}
	}
	if l1 != 1 || l2 != 1 {
		t.Fatalf("responses: failed attempt %d, retry %d; want 1 and 1", l1, l2)
	}
}

// TestReusedRequestIDStreamCompletionWhileRetryStreams (O4): the failed attempt's completion is taken
// while the retry streams. 0.1.4 discarded the failed attempt's buffered event at the retry's
// header-init, flushed the retry's half-built stream as failed, and dropped the retry's completion.
func TestReusedRequestIDStreamCompletionWhileRetryStreams(t *testing.T) {
	h := plugintest.New(t, nil, "")
	rid := "reuse-o4"
	s1 := stamp()
	streamL1(h, rid)
	c1 := stamp()
	s2 := stamp()
	streamL2Start(h, rid)
	h.Plugin.Hook(protocol.MethodRequestComplete, completeAt(rid, "failed", 429, s1, c1, wsMeta(rid)))
	waitKey(t, h, wire.ExchangeCompletionKey(rid))
	streamL2End(h, rid)
	c2 := stamp()
	h.Plugin.Hook(protocol.MethodRequestComplete, completeAt(rid, "succeeded", 200, s2, c2, wsMeta(rid)))
	checkStreams(t, checkReuse(t, h, rid, s1, s2, 2))
}

// TestReusedRequestIDStreamRetryCompletesFirst (O6, stream): the retry finishes and completes before
// the failed attempt's completion arrives.
func TestReusedRequestIDStreamRetryCompletesFirst(t *testing.T) {
	h := plugintest.New(t, nil, "")
	rid := "reuse-o6s"
	s1 := stamp()
	streamL1(h, rid)
	c1 := stamp()
	s2 := stamp()
	streamL2Start(h, rid)
	streamL2End(h, rid)
	c2 := stamp()
	h.Plugin.Hook(protocol.MethodRequestComplete, completeAt(rid, "succeeded", 200, s2, c2, wsMeta(rid)))
	waitKey(t, h, wire.ExchangeCompletionKeyAt(rid, s2))
	h.Plugin.Hook(protocol.MethodRequestComplete, completeAt(rid, "failed", 429, s1, c1, wsMeta(rid)))
	checkStreams(t, checkReuse(t, h, rid, s1, s2, 2))
}

// TestReusedRequestIDStreamRace (O7, F4): the failed attempt's lifecycle completes only after the
// retry's header-init, so CompletedAt alone cannot tell which segment is whose. The completion is
// held until the retry's next event (after CompletedAt) settles it; nothing is discarded.
func TestReusedRequestIDStreamRace(t *testing.T) {
	h := plugintest.New(t, nil, "")
	rid := "reuse-o7"
	s1 := stamp()
	streamL1(h, rid)
	s2 := stamp()
	streamL2Start(h, rid)
	c1 := stamp()
	h.Plugin.Hook(protocol.MethodRequestComplete, completeAt(rid, "failed", 429, s1, c1, wsMeta(rid)))
	streamL2End(h, rid)
	waitKey(t, h, wire.ExchangeCompletionKey(rid))
	c2 := stamp()
	h.Plugin.Hook(protocol.MethodRequestComplete, completeAt(rid, "succeeded", 200, s2, c2, wsMeta(rid)))
	checkStreams(t, checkReuse(t, h, rid, s1, s2, 2))
}

// TestSingleLifecycleKeysUnchanged: an exchange whose RequestID is never reused keeps 0.1.4's keys,
// with no lifecycle_input_seq, a bare completion key and no hold.
func TestSingleLifecycleKeysUnchanged(t *testing.T) {
	h := plugintest.New(t, nil, "")
	rid := "single-1"
	s1 := stamp()
	p, m := h.Plugin, wsMeta(rid)
	p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth(rid, "openai-response", wsCreate("one"), nil, m))
	p.Hook(protocol.MethodRequestInterceptAfter, plugintest.AfterAuth(rid))
	p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(rid, "openai-response", -1, nil, m))
	for i, c := range []string{wsCreated2, wsItem, wsDone2} {
		p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(rid, "openai-response", i, []byte(c), m))
	}
	// A duplex follow-up on the same RequestID, as CPA's Codex duplex executor sends it.
	p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth(rid, "openai-response", wsCreate("two"), nil, m))
	p.Hook(protocol.MethodRequestInterceptAfter, plugintest.AfterAuth(rid))
	c1 := stamp()
	p.Hook(protocol.MethodRequestComplete, completeAt(rid, "succeeded", 200, s1, c1, m))
	var s stored
	plugintest.WaitFor(t, "completion", func() bool { s = storedFor(h, rid); return s.keys[wire.ExchangeCompletionKey(rid)] })
	want := []string{"ex/" + rid + "/begin/0", "ex/" + rid + "/begin/1", "ex/" + rid + "/attempt/1", "ex/" + rid + "/attempt/2",
		"ex/" + rid + "/response/0", "ex/" + rid + "/completion"}
	plugintest.WaitFor(t, "all envelopes", func() bool { s = storedFor(h, rid); return len(s.keys) == len(want) })
	for _, k := range want {
		if !s.keys[k] {
			t.Fatalf("missing %s: %v", k, s.keys)
		}
	}
	if c := s.comp[wire.ExchangeCompletionKey(rid)]; c.LifecycleInputSeq != 0 || c.Responses != 1 || !c.BeginWritten {
		t.Fatalf("completion = %+v", c)
	}
	e := p.Engine()
	for _, n := range []string{"reused_request_id", "ownership_held", "segments_parked", "segments_uncertain"} {
		if e.Stats.Other()[n] != 0 {
			t.Fatalf("%s = %d", n, e.Stats.Other()[n])
		}
	}
	plugintest.WaitFor(t, "state released", func() bool { return e.Entries() == 0 })
}
