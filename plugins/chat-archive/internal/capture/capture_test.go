package capture_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/capture"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/plugin/plugintest"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

func ids(n ...byte) []wire.Hash {
	out := make([]wire.Hash, len(n))
	for i, b := range n {
		out[i][0] = b
	}
	return out
}

func TestPrefixDeltaChoice(t *testing.T) {
	p := capture.NewPrefixMap()
	key := capture.ConvKey("c", "claude", "", http.Header{"X-Claude-Code-Session-Id": {"s1"}}, nil)
	if d := p.Choose(key, ids(1, 2), "", "", 0); d.Base != nil {
		t.Fatal("first exchange is not a keyframe")
	}
	p.Committed(key, "ex/a/begin/0", ids(1, 2), 0)
	p.ResponseCommitted("ex/a/begin/0", "ex/a/response/0", 0, ids(3), "", "")
	// Committed but not acked by the archive: never a base (it may still be rejected and
	// remasked with new item ids).
	if d := p.Choose(key, ids(1, 2, 3, 4, 5), "", "", 0); d.Base != nil {
		t.Fatalf("unacked begin used as a base: %+v", d)
	}
	p.Acked("ex/a/begin/0")
	// Begin acked, response not yet: S(base) is the input items only.
	if d := p.Choose(key, ids(1, 2, 3), "", "", 0); d.Base == nil || d.Base.Kept != 2 || d.Base.Resp != -1 || d.From != 2 {
		t.Fatalf("input-only delta = %+v", d)
	}
	p.Acked("ex/a/response/0")
	d := p.Choose(key, ids(1, 2, 3, 4, 5), "", "", 0)
	if d.Base == nil || d.Base.Key != "ex/a/begin/0" || d.Base.Kept != 3 || d.Base.Resp != 0 || d.From != 3 || len(d.Expanded) != 5 {
		t.Fatalf("delta = %+v", d)
	}
	// kept < n/2 → keyframe; a dropped counter that moved → keyframe; a dead base → keyframe.
	if d := p.Choose(key, ids(1, 9, 9, 9, 9, 9), "", "", 0); d.Base != nil {
		t.Fatal("short prefix used a base")
	}
	if d := p.Choose(key, ids(1, 2, 3, 4), "", "", 1); d.Base != nil {
		t.Fatal("base used after drops")
	}
	p.Dead("ex/a/begin/0")
	if d := p.Choose(key, ids(1, 2, 3, 4), "", "", 0); d.Base != nil {
		t.Fatal("dead base used")
	}
	// Native WebSocket turn: previous_response_id selects the base, kept = len(S(base)).
	p.Committed(key, "ex/w/begin/0", ids(7), 0)
	p.ResponseCommitted("ex/w/begin/0", "ex/w/response/0", 0, ids(8), "ws-1", "resp_1")
	if d := p.Choose(key, ids(9), "ws-1", "resp_1", 0); d.Base != nil {
		t.Fatal("unacked native-turn base used")
	}
	p.Acked("ex/w/begin/0")
	p.Acked("ex/w/response/0")
	d = p.Choose(key, ids(9), "ws-1", "resp_1", 0)
	if d.Base == nil || d.Base.Kept != 2 || len(d.Expanded) != 3 || d.From != 0 {
		t.Fatalf("native delta = %+v", d)
	}
	// Session ids are hashed into the key; different sessions never share a base.
	if capture.ConvKey("c", "claude", "", http.Header{"X-Claude-Code-Session-Id": {"s2"}}, nil) == key {
		t.Fatal("distinct sessions share a key")
	}
}

func TestRemaskRequeue(t *testing.T) {
	m0, _ := mask.New(mask.Config{})
	secret := "registered-exact-value-0123456789"
	fp := canon.NewPiece(wire.PieceText, "", []byte("hello "+secret))
	item := wire.Item{F: []byte(`{"type":"text"}`), P: &fp.Hash}
	items := []wire.Item{item}
	b := wire.ExchangeResponse{RequestID: "r", ObservedAt: canonTime(), StatusCode: 200, Headers: []wire.Header{},
		Body: wire.Body{Format: canon.FormatClaude, Items: items, N: 1, ListSHA: canon.ListSHA(canon.ItemIDs(items))}}
	body, _ := canon.Marshal(b)
	env := wire.Envelope{V: 1, Kind: wire.KindExchangeResponse, OutboxID: strings.Repeat("a", 32), Seq: 1, Key: "ex/r/response/0",
		MaskVersion: "mask/v1", CanonVersion: "canon/v1", CreatedAt: canonTime(), Refs: []wire.Hash{fp.Hash}, Body: body}
	raw, _ := canon.Marshal(env)
	fpx := canonExact(secret)
	m1, _ := mask.New(mask.Config{ExactFPs: [][32]byte{fpx}, ExactRev: 2})
	h := capture.NewMaskerHolder(m0, 0, 1<<20)
	h.Set(m1, 2)
	load := func(x wire.Hash) (wire.PieceKind, string, []byte, error) { return fp.Kind, "", fp.Bytes, nil }
	pool := hooks.NewPool(1<<20, 1<<10)
	ctx := context.Background()
	out, held, err := capture.Requeued(ctx, raw, true, load, h, pool)
	if err != nil {
		t.Fatal(err)
	}
	if held != int64(len(out.Pieces[0].Bytes)) || pool.Used() != held {
		t.Fatalf("held %d, pool %d: only the rewritten piece stays reserved", held, pool.Used())
	}
	pool.Release(held)
	if len(out.Pieces) != 1 || strings.Contains(string(out.Pieces[0].Bytes), secret) || !strings.Contains(string(out.Pieces[0].Bytes), "[masked:exact:") {
		t.Fatalf("remasked pieces = %+v", out.Pieces)
	}
	if out.Refs[0] != out.Pieces[0].Hash || out.Key != env.Key || !out.CreatedAt.Equal(env.CreatedAt) || out.ExactRev != 2 {
		t.Fatalf("remasked envelope = %+v", out)
	}
	v, err := wire.DecodeBody(wire.KindExchangeResponse, out.Body)
	if err != nil {
		t.Fatal(err)
	}
	nb := v.(*wire.ExchangeResponse).Body
	if *nb.Items[0].P != out.Pieces[0].Hash || nb.ListSHA != canon.ListSHA(canon.ItemIDs(nb.Items)) {
		t.Fatal("item references or ListSHA not rewritten")
	}
	plain, _, err := capture.Requeued(ctx, raw, false, load, h, pool)
	if err != nil || string(plain.Body) != string(env.Body) || plain.Pieces != nil {
		t.Fatalf("plain requeue changed the body: %v", err)
	}
	var _ outbox.NewEnvelope = plain

	// Budget: a pool that cannot hold 4 × the piece refuses the remask and keeps nothing.
	small := hooks.NewPool(int64(len(fp.Bytes))*3, 1)
	if _, held, err := capture.Requeued(ctx, raw, true, load, h, small); !errors.Is(err, capture.ErrRemaskBudget) || held != 0 || small.Used() != 0 {
		t.Fatalf("over budget: err %v held %d used %d", err, held, small.Used())
	}
	// A cancelled context (the runtime stopping) ends the remask without masking.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := capture.Requeued(cctx, raw, true, load, h, pool); !errors.Is(err, context.Canceled) || pool.Used() != 0 {
		t.Fatalf("cancelled: err %v used %d", err, pool.Used())
	}
	// A json piece over the node cap (the pool's bytes / 250 per value) is refused.
	vals := "[" + strings.TrimSuffix(strings.Repeat("1,", 2000), ",") + "]"
	jp := canon.NewPiece(wire.PieceJSON, "", []byte(vals))
	jitems := []wire.Item{{F: []byte(`{"type":"tool_use"}`), P: &jp.Hash}}
	jb := b
	jb.Body = wire.Body{Format: canon.FormatClaude, Items: jitems, N: 1, ListSHA: canon.ListSHA(canon.ItemIDs(jitems))}
	jbody, _ := canon.Marshal(jb)
	jenv := env
	jenv.Refs, jenv.Body = []wire.Hash{jp.Hash}, jbody
	jraw, _ := canon.Marshal(jenv)
	jload := func(x wire.Hash) (wire.PieceKind, string, []byte, error) { return jp.Kind, "", jp.Bytes, nil }
	tight := hooks.NewPool(int64(len(jp.Bytes))*4+1000*250, 1) // room for about 1,000 values, the piece has 2,001
	if _, _, err := capture.Requeued(ctx, jraw, true, jload, h, tight); !errors.Is(err, capture.ErrRemaskBudget) || tight.Used() != 0 {
		t.Fatalf("json over the node cap: err %v used %d", err, tight.Used())
	}
	if _, held, err := capture.Requeued(ctx, jraw, true, jload, h, pool); err != nil || held != 0 {
		t.Fatalf("json within the cap: err %v held %d", err, held)
	}
	// A stop that arrives while a json piece is being masked ends that piece's masking: the
	// context ends after the piece is loaded, and the remask returns the context's error with
	// nothing reserved, instead of finishing the piece and the envelope.
	sctx, stop := context.WithCancel(ctx)
	defer stop()
	sload := func(x wire.Hash) (wire.PieceKind, string, []byte, error) { stop(); return jp.Kind, "", jp.Bytes, nil }
	if _, _, err := capture.Requeued(sctx, jraw, true, sload, h, pool); !errors.Is(err, context.Canceled) || pool.Used() != 0 {
		t.Fatalf("json piece masking not stopped: err %v used %d", err, pool.Used())
	}
}

// TestMemoNeverServesAnotherMaskersResult: a split still holding the previous masker can add a
// memo entry after the swap; the new masker (which knows an exact value the old one did not)
// must not get it. Swaps at the same revision clear the memo too.
func TestMemoNeverServesAnotherMaskersResult(t *testing.T) {
	secret := "registered-exact-value-0123456789"
	m0, _ := mask.New(mask.Config{})
	m1, _ := mask.New(mask.Config{ExactFPs: [][32]byte{canonExact(secret)}})
	h := capture.NewMaskerHolder(m0, 0, 1<<20)
	old := h.Get()
	var k wire.Hash
	k[0] = 1
	old.Memo.Add(k, "unmasked by m0 "+secret, nil)
	h.Set(m1, 0) // same revision 0: the server set replacing the local-only set
	if _, _, ok := h.Get().Memo.Get(k); ok {
		t.Fatal("memo survived a same-revision swap")
	}
	old.Memo.Add(k, "late add by m0 "+secret, nil) // a split that started before the swap
	if _, _, ok := h.Get().Memo.Get(k); ok {
		t.Fatal("new masker served the old masker's memo entry")
	}
	h.Get().Memo.Add(k, "masked by m1", nil)
	if v, _, ok := h.Get().Memo.Get(k); !ok || v != "masked by m1" {
		t.Fatalf("own entry = %q %v", v, ok)
	}
	if _, _, ok := old.Memo.Get(k); ok {
		t.Fatal("old view served the new masker's entry")
	}
}

func TestClassify(t *testing.T) {
	f := false
	cases := []struct {
		format string
		meta   protocol.Meta
		kind   string
	}{
		{"claude", protocol.Meta{RequestPath: "/v1/messages"}, capture.KindChat},
		{"claude", protocol.Meta{RequestPath: "/v1/messages/count_tokens"}, capture.KindCountTokens},
		{"openai-response", protocol.Meta{RequestPath: "/v1/responses/compact"}, capture.KindCompaction},
		{"openai-response", protocol.Meta{RequestPath: "/v1/responses", Generate: &f, ExecutionSessionID: "ws"}, capture.KindPrewarm},
		{"openai-image", protocol.Meta{RequestPath: "/v1/images/edits"}, capture.KindImage},
		{"openai-video", protocol.Meta{RequestPath: "/v1/videos"}, capture.KindVideoCreate},
		{"openai-video", protocol.Meta{RequestPath: "/v1/videos/:request_id"}, capture.KindVideoPoll},
		{"openai-speech", protocol.Meta{RequestPath: "/v1/audio/speech"}, capture.KindSpeech},
		{"mystery", protocol.Meta{}, capture.KindOpaque},
	}
	for _, c := range cases {
		if got := capture.Classify(c.format, c.meta); got.Kind != c.kind {
			t.Errorf("%s %s = %s, want %s", c.format, c.meta.RequestPath, got.Kind, c.kind)
		}
	}
	if c := capture.Classify("openai", protocol.Meta{RequestPath: "/v1/completions"}); !c.Legacy || c.Transport != "http" {
		t.Fatalf("legacy = %+v", c)
	}
}

func TestStreamCaptureClaudeAndWebSocketResponses(t *testing.T) {
	h := plugintest.New(t, nil, "")
	p := h.Plugin
	meta := plugintest.Meta("/v1/messages")
	p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth("st-1", "claude", plugintest.ClaudeBody("hi"), nil, meta))
	p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("st-1", "claude", -1, nil, meta))
	for i, c := range []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_st\",\"role\":\"assistant\",\"content\":[]}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"stream-marker-",
		"omega\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	} {
		p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("st-1", "claude", i, []byte(c), meta))
	}
	p.Hook(protocol.MethodRequestComplete, plugintest.Complete("st-1", "succeeded", meta))

	// Responses WebSocket: one RequestID, two responses, then the socket ends.
	ws := map[string]any{"request_path": "/v1/responses", "execution_session_id": "ws-9"}
	p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth("ws-1", "openai-response", []byte(`{"type":"response.create","input":"a"}`), nil, ws))
	p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("ws-1", "openai-response", -1, nil, ws))
	for i, c := range []string{
		`{"type":"response.created","response":{"id":"resp_w1"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first"}]}}`,
		`{"type":"response.completed","response":{"id":"resp_w1","status":"completed","output":[]}}`,
		`{"type":"response.created","response":{"id":"resp_w2","previous_response_id":"resp_w1"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"second"}]}}`,
		`{"type":"response.completed","response":{"id":"resp_w2","status":"completed","output":[]}}`,
	} {
		p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("ws-1", "openai-response", i, []byte(c), ws))
	}
	p.Hook(protocol.MethodRequestComplete, plugintest.Complete("ws-1", "succeeded", ws))

	var resp map[string]wire.ExchangeResponse
	var comp map[string]wire.ExchangeCompletion
	plugintest.WaitFor(t, "stream envelopes", func() bool {
		resp, comp = map[string]wire.ExchangeResponse{}, map[string]wire.ExchangeCompletion{}
		for _, r := range h.Srv.Envelopes(h.SourceID) {
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
		return len(resp) == 3 && len(comp) == 2
	})
	cl := resp["ex/st-1/response/0"]
	if !cl.Stream || cl.ResponseID != "msg_st" || cl.TerminalStatus != "completed" || cl.Chunks != 3 || cl.TerminalAt == nil || cl.HeaderInitAt == nil {
		t.Fatalf("claude stream response = %+v", cl)
	}
	w0, w1 := resp["ex/ws-1/response/0"], resp["ex/ws-1/response/1"]
	if w0.ResponseID != "resp_w1" || w1.ResponseID != "resp_w2" || w1.PrevResponseID != "resp_w1" || w1.Index != 1 {
		t.Fatalf("ws responses = %+v / %+v", w0, w1)
	}
	if c := comp["ws-1"]; c.Responses != 2 || !c.BeginWritten {
		t.Fatalf("ws completion = %+v", c)
	}
	found := false
	for _, r := range h.Srv.Envelopes(h.SourceID) {
		for _, ref := range r.Envelope.Refs {
			if _, raw, ok := h.Srv.Piece(ref); ok && strings.Contains(string(raw), "stream-marker-omega") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("reassembled stream text missing from pieces")
	}
	if e := p.Engine(); e.Entries() != 0 || e.Pool.Used() != 0 {
		t.Fatalf("state left: entries %d pool %d", e.Entries(), e.Pool.Used())
	}
}

// TestCanceledAfterTerminalEvent: CPA v8.0.60 reports a Responses WebSocket turn as canceled
// (status 0, "context canceled") when the client closes the socket after the turn's last
// terminal event but before CPA's stream goroutine reads the upstream close, and always for a
// duplex RequestID, which ends only with the socket. When every response the stream carried
// ended with a completed, done or incomplete event, the completion records succeeded with 200,
// as CPA itself does for an HTTP Responses stream whose delivery was acknowledged. A response
// still open at the completion, or a last response that failed, keeps CPA's canceled.
func TestCanceledAfterTerminalEvent(t *testing.T) {
	h := plugintest.New(t, nil, "")
	p := h.Plugin
	created := func(id string) string { return `{"type":"response.created","response":{"id":"` + id + `"}}` }
	item := `{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"synthetic"}]}}`
	done := func(id string) string {
		return `{"type":"response.completed","response":{"id":"` + id + `","status":"completed","output":[]}}`
	}
	cases := []struct {
		rid, format string
		chunks      []string
		outcome     string
		status      int
		terminal    []string // terminal_status of each stored response, in index order
	}{
		{"cx-ws-turn", "openai-response", []string{created("resp_c1"), item, done("resp_c1")}, "succeeded", 200, []string{"completed"}},
		{"cx-ws-duplex", "openai-response", []string{created("resp_d1"), item, done("resp_d1"), created("resp_d2"), item, done("resp_d2")},
			"succeeded", 200, []string{"completed", "completed"}},
		{"cx-ws-incomplete", "openai-response", []string{created("resp_i1"), item,
			`{"type":"response.incomplete","response":{"id":"resp_i1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`},
			"succeeded", 200, []string{"incomplete"}},
		{"cx-ws-open", "openai-response", []string{created("resp_o1"), item, done("resp_o1"), created("resp_o2"), item},
			"canceled", 0, []string{"completed", "canceled"}},
		{"cx-ws-cut", "openai-response", []string{created("resp_x1"), item}, "canceled", 0, []string{"canceled"}},
		{"cx-ws-error", "openai-response", []string{created("resp_e1"), `{"type":"error","error":{"type":"server_error","message":"synthetic"}}`},
			"canceled", 0, []string{"failed"}},
		{"cx-claude", "claude", []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_cx\",\"role\":\"assistant\",\"content\":[]}}\n\n" +
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"synthetic\"}}\n\n" +
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"},
			"succeeded", 200, []string{"completed"}},
	}
	for _, c := range cases {
		meta := map[string]any{"request_path": "/v1/responses", "execution_session_id": "ws-" + c.rid}
		body := []byte(`{"type":"response.create","input":"synthetic"}`)
		if c.format == "claude" {
			meta, body = plugintest.Meta("/v1/messages"), plugintest.ClaudeBody("synthetic")
		}
		p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth(c.rid, c.format, body, nil, meta))
		p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(c.rid, c.format, -1, nil, meta))
		for i, ch := range c.chunks {
			p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(c.rid, c.format, i, []byte(ch), meta))
		}
		p.Hook(protocol.MethodRequestComplete, plugintest.CompleteStatus(c.rid, "canceled", 0, "context canceled", meta))
	}
	var comp map[string]wire.ExchangeCompletion
	var resp map[string]wire.ExchangeResponse
	plugintest.WaitFor(t, "completions", func() bool {
		comp, resp = map[string]wire.ExchangeCompletion{}, map[string]wire.ExchangeResponse{}
		for _, r := range h.Srv.Envelopes(h.SourceID) {
			switch r.Envelope.Kind {
			case wire.KindExchangeDone:
				var x wire.ExchangeCompletion
				json.Unmarshal(r.Envelope.Body, &x)
				comp[x.RequestID] = x
			case wire.KindExchangeResponse:
				var x wire.ExchangeResponse
				json.Unmarshal(r.Envelope.Body, &x)
				resp[r.Envelope.Key] = x
			}
		}
		return len(comp) == len(cases)
	})
	for _, c := range cases {
		x := comp[c.rid]
		wantErr := ""
		if c.outcome == "canceled" {
			wantErr = "context canceled"
		}
		if x.Outcome != c.outcome || x.StatusCode != c.status || x.Error != wantErr || x.Responses != uint32(len(c.terminal)) {
			t.Errorf("%s: outcome %q status %d error %q responses %d; want %q %d %q %d",
				c.rid, x.Outcome, x.StatusCode, x.Error, x.Responses, c.outcome, c.status, wantErr, len(c.terminal))
		}
		for i, want := range c.terminal {
			if r := resp[wire.ExchangeResponseKey(c.rid, uint32(i))]; r.TerminalStatus != want {
				t.Errorf("%s response %d: terminal_status %q, want %q", c.rid, i, r.TerminalStatus, want)
			}
		}
	}
	if e := p.Engine(); e.Entries() != 0 || e.Pool.Used() != 0 {
		t.Fatalf("state left: entries %d pool %d", e.Entries(), e.Pool.Used())
	}
}

// TestNonStreamTerminalStatus: a non-stream response records a terminal status, as a stream
// does. CPA calls the response hook only after a successful non-stream execution, so the
// response completed; a Responses object's own status refines that. A body the plugin does not
// read (metadata-only capture) records completed.
func TestNonStreamTerminalStatus(t *testing.T) {
	respObj := func(id, status string) string {
		st := ""
		if status != "" {
			st = `"status":"` + status + `",`
		}
		return `{"id":"` + id + `","object":"response",` + st + `"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"synthetic"}]}]}`
	}
	cases := []struct {
		rid, format, path, req, body, want, respID string
	}{
		{"ns-claude", "claude", "/v1/messages", string(plugintest.ClaudeBody("hi")),
			`{"id":"msg_ns","type":"message","role":"assistant","content":[{"type":"text","text":"synthetic"}],"stop_reason":"end_turn"}`, "completed", "msg_ns"},
		{"ns-chat", "openai", "/v1/chat/completions", `{"messages":[{"role":"user","content":"hi"}]}`,
			`{"id":"chatcmpl-ns","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"synthetic"},"finish_reason":"stop"}]}`, "completed", "chatcmpl-ns"},
		{"ns-gemini", "gemini", "/v1beta/models/m:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			`{"responseId":"gem-ns","candidates":[{"content":{"role":"model","parts":[{"text":"synthetic"}]},"finishReason":"STOP"}]}`, "completed", "gem-ns"},
		{"ns-resp", "openai-response", "/v1/responses", `{"input":"hi"}`, respObj("resp_ns1", "completed"), "completed", "resp_ns1"},
		{"ns-resp-nostatus", "openai-response", "/v1/responses", `{"input":"hi"}`, respObj("resp_ns2", ""), "completed", "resp_ns2"},
		{"ns-resp-incomplete", "openai-response", "/v1/responses", `{"input":"hi"}`, respObj("resp_ns3", "incomplete"), "incomplete", "resp_ns3"},
		{"ns-resp-failed", "openai-response", "/v1/responses", `{"input":"hi"}`, respObj("resp_ns4", "failed"), "failed", "resp_ns4"},
		{"ns-resp-cancelled", "codex", "/v1/responses", `{"input":"hi"}`, respObj("resp_ns5", "cancelled"), "canceled", "resp_ns5"},
		{"ns-resp-queued", "openai-response", "/v1/responses", `{"input":"hi"}`, respObj("resp_ns6", "queued"), "", "resp_ns6"},
	}
	run := func(h *plugintest.Harness, n int) map[string]wire.ExchangeResponse {
		t.Helper()
		var got map[string]wire.ExchangeResponse
		plugintest.WaitFor(t, "non-stream responses", func() bool {
			got = map[string]wire.ExchangeResponse{}
			for _, r := range h.Srv.Envelopes(h.SourceID) {
				if r.Envelope.Kind == wire.KindExchangeResponse {
					var x wire.ExchangeResponse
					json.Unmarshal(r.Envelope.Body, &x)
					got[x.RequestID] = x
				}
			}
			return len(got) == n
		})
		return got
	}
	h := plugintest.New(t, nil, "")
	for _, c := range cases {
		meta := plugintest.Meta(c.path)
		h.Plugin.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth(c.rid, c.format, []byte(c.req), nil, meta))
		h.Plugin.Hook(protocol.MethodResponseInterceptAfter, plugintest.Response(c.rid, c.format, []byte(c.body), meta))
		h.Plugin.Hook(protocol.MethodRequestComplete, plugintest.Complete(c.rid, "succeeded", meta))
	}
	got := run(h, len(cases))
	for _, c := range cases {
		r := got[c.rid]
		if r.Stream || r.TerminalStatus != c.want || r.ResponseID != c.respID || r.Body.Dropped != "" || r.Body.N == 0 {
			t.Errorf("%s: stream %v terminal_status %q response_id %q dropped %q n %d; want terminal_status %q response_id %q",
				c.rid, r.Stream, r.TerminalStatus, r.ResponseID, r.Body.Dropped, r.Body.N, c.want, c.respID)
		}
	}

	m := plugintest.New(t, nil, "capture: metadata\n")
	meta := plugintest.Meta("/v1/responses")
	m.Plugin.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth("ns-meta", "openai-response", []byte(`{"input":"hi"}`), nil, meta))
	m.Plugin.Hook(protocol.MethodResponseInterceptAfter, plugintest.Response("ns-meta", "openai-response", []byte(respObj("resp_ns7", "incomplete")), meta))
	m.Plugin.Hook(protocol.MethodRequestComplete, plugintest.Complete("ns-meta", "succeeded", meta))
	if r := run(m, 1)["ns-meta"]; r.Stream || r.TerminalStatus != "completed" || r.Body.N != 0 {
		t.Fatalf("metadata-only non-stream response: stream %v terminal_status %q n %d", r.Stream, r.TerminalStatus, r.Body.N)
	}
}
