package stream

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

// feed runs chunks through an assembler and a reassembler, then flushes with status.
func feed(t *testing.T, format string, chunks []string, status string) []Response {
	t.Helper()
	var a Assembler
	re := New(format)
	var out []Response
	for _, c := range chunks {
		for _, ev := range a.Add([]byte(c)) {
			out = append(out, re.Feed(ev, t0)...)
		}
	}
	for _, ev := range a.Finish() {
		out = append(out, re.Feed(ev, t0)...)
	}
	return append(out, re.Flush(status, t0)...)
}

func body(t *testing.T, r Response) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	return m
}

func TestAssemblerFraming(t *testing.T) {
	var a Assembler
	var got []Event
	for _, c := range []string{
		"event: one\r\n", "data: {\"a\":", "1}\r\n\r\n", // split payload, CRLF
		"data: {\"b\":2}", // unterminated but valid JSON: completes at chunk end
		"event: two",      // event line at chunk end, then a data line without a newline before it
		"data: {\"c\":3}\n\n",
		": keep-alive\n\n",
		"data: {\"d\":\ndata: 4}\n\n", // JSON split across data lines
		"{\"bare\":true}",             // bare JSON chunk
		"data: [DONE]\n\n",
	} {
		got = append(got, a.Add([]byte(c))...)
	}
	got = append(got, a.Finish()...)
	want := []struct{ name, data string }{{"one", `{"a":1}`}, {"", `{"b":2}`}, {"two", `{"c":3}`}, {"", "{\"d\":\n4}"}, {"", `{"bare":true}`}, {"", "[DONE]"}}
	if len(got) != len(want) {
		t.Fatalf("got %d events: %q", len(got), got)
	}
	for i, w := range want {
		if got[i].Name != w.name || string(got[i].Data) != w.data {
			t.Errorf("event %d = %q %q, want %q %q", i, got[i].Name, got[i].Data, w.name, w.data)
		}
	}
}

func TestAssemblerBareJSONSplitAcrossChunks(t *testing.T) {
	var a Assembler
	if ev := a.Add([]byte(`{"candidates":[{"content":`)); len(ev) != 0 {
		t.Fatalf("partial JSON emitted: %q", ev)
	}
	ev := a.Add([]byte(`{"parts":[{"text":"x"}]}}]}`))
	if len(ev) != 1 {
		t.Fatalf("events = %q", ev)
	}
	// Gemini array framing: leading [ and , and trailing ].
	ev = a.Add([]byte("[{\"a\":1}\n,{\"b\":2}\n]"))
	if len(ev) != 2 || string(ev[1].Data) != `{"b":2}` {
		t.Fatalf("array framing events = %q", ev)
	}
}

func TestClaudeStream(t *testing.T) {
	chunks := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_syn1\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"usage\":{\"input_tokens\":12345678901234567890}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"pondering \"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"synthetic\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"c2lnbmF0dXJl\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello \"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_bl", // split mid-line
		"ock_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"marker-alpha\"}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"t\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"1}\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":2}\n\n",
		"event: ping\ndata: {\"type\":\"ping\"}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":7}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	rs := feed(t, "claude", chunks, StatusCompleted)
	if len(rs) != 1 || rs[0].TerminalStatus != StatusCompleted || rs[0].ResponseID != "msg_syn1" || rs[0].TerminalAt.IsZero() {
		t.Fatalf("responses = %+v", rs)
	}
	if !strings.Contains(string(rs[0].Body), `"input_tokens":12345678901234567890`) {
		t.Fatalf("big integer literal not kept: %s", rs[0].Body)
	}
	m := body(t, rs[0])
	content := m["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content = %v", content)
	}
	b0, b1, b2 := content[0].(map[string]any), content[1].(map[string]any), content[2].(map[string]any)
	if b0["thinking"] != "pondering synthetic" || b0["signature"] != "c2lnbmF0dXJl" || b1["text"] != "Hello marker-alpha" {
		t.Fatalf("blocks = %v %v", b0, b1)
	}
	if in := b2["input"].(map[string]any); in["q"] != float64(1) {
		t.Fatalf("tool input = %v", b2["input"])
	}
	if m["stop_reason"] != "tool_use" || m["usage"].(map[string]any)["output_tokens"] != float64(7) {
		t.Fatalf("message = %v", m)
	}
}

func TestClaudeIncompleteAndBadJSON(t *testing.T) {
	rs := feed(t, "claude", []string{
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_2\"}}\n\n",
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t\",\"name\":\"n\"}}\n\n",
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{broken\"}}\n\n",
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
	}, StatusCompleted)
	if len(rs) != 1 || rs[0].TerminalStatus != StatusIncomplete || len(rs[0].Flags) != 1 {
		t.Fatalf("responses = %+v", rs)
	}
	if body(t, rs[0])["content"].([]any)[0].(map[string]any)["input"] != "{broken" {
		t.Fatal("raw input not kept")
	}
	// A bare Message JSON chunk is the whole response.
	rs = feed(t, "claude", []string{`{"type":"message","id":"msg_3","content":[{"type":"text","text":"x"}]}`}, StatusCompleted)
	if len(rs) != 1 || rs[0].ResponseID != "msg_3" || rs[0].TerminalStatus != StatusCompleted {
		t.Fatalf("bare message = %+v", rs)
	}
	rs = feed(t, "claude", []string{"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\n"}, StatusCanceled)
	if len(rs) != 1 || rs[0].TerminalStatus != StatusCanceled {
		t.Fatalf("canceled = %+v", rs)
	}
}

func TestChatStream(t *testing.T) {
	rs := feed(t, "openai", []string{
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"think "}}]}`,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"reasoning_content":"more","content":"Hi "}}]}`,
		`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"marker-beta","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":"}}]}}]}`,
		`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}],"reasoning_details":[{"type":"reasoning.encrypted","index":0,"data":"[REDACTED]"}]}}]}`,
		`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"reasoning_details":[{"type":"reasoning.encrypted","index":0,"data":"ZmluYWw="}]},"finish_reason":"tool_calls"}]}`,
		`{"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4}}`,
	}, StatusCompleted)
	if len(rs) != 1 || rs[0].TerminalStatus != StatusCompleted {
		t.Fatalf("responses = %+v", rs)
	}
	m := body(t, rs[0])
	if m["object"] != "chat.completion" || m["usage"] == nil {
		t.Fatalf("top = %v", m)
	}
	ch := m["choices"].([]any)[0].(map[string]any)
	msg := ch["message"].(map[string]any)
	if msg["content"] != "Hi marker-beta" || msg["reasoning_content"] != "think more" || ch["finish_reason"] != "tool_calls" {
		t.Fatalf("message = %v", msg)
	}
	tc := msg["tool_calls"].([]any)[0].(map[string]any)
	if tc["id"] != "call_1" || tc["function"].(map[string]any)["arguments"] != `{"a":1}` {
		t.Fatalf("tool call = %v", tc)
	}
	if d := msg["reasoning_details"].([]any)[0].(map[string]any); d["data"] != "ZmluYWw=" {
		t.Fatalf("reasoning_details = %v", d)
	}
	rs = feed(t, "openai", []string{`{"error":{"message":"synthetic"}}`}, StatusCompleted)
	if len(rs) != 1 || rs[0].TerminalStatus != StatusFailed {
		t.Fatalf("error chunk = %+v", rs)
	}
}

func TestResponsesStreamNativeCodexEmptyCompletedOutput(t *testing.T) {
	ev := func(typ, rest string) string {
		return "event: " + typ + "\ndata: {\"type\":\"" + typ + "\"" + rest + "}\n\n"
	}
	rs := feed(t, "openai-response", []string{
		ev("response.created", `,"response":{"id":"resp_1","status":"in_progress","model":"m"}`),
		ev("codex.rate_limits", `,"limits":{}`),
		ev("response.output_item.added", `,"output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}`),
		ev("response.reasoning_summary_text.delta", `,"output_index":0,"summary_index":0,"delta":"plan"`),
		ev("response.output_item.done", `,"output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"plan"}],"encrypted_content":"ZW5j"}`),
		ev("response.output_item.added", `,"output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}`),
		ev("response.content_part.added", `,"output_index":1,"content_index":0,"part":{"type":"output_text","text":""}`),
		ev("response.output_text.delta", `,"output_index":1,"content_index":0,"delta":"marker-"`),
		"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":1,\"content_index\":0,\"delta\":\"gamma\"}", // unterminated line
		ev("response.output_item.done", `,"output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"marker-gamma"}]}`),
		ev("response.metadata", `,"secretish":"value"`),
		ev("response.completed", `,"response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":1}}`),
	}, StatusCompleted)
	if len(rs) != 1 || rs[0].ResponseID != "resp_1" || rs[0].TerminalStatus != StatusCompleted {
		t.Fatalf("responses = %+v", rs)
	}
	if strings.Contains(string(rs[0].Body), "value") {
		t.Fatal("response.metadata value kept")
	}
	out := body(t, rs[0])["output"].([]any)
	if len(out) != 2 || out[1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "marker-gamma" {
		t.Fatalf("output = %v", out)
	}
}

func TestResponsesSupersededAndSteering(t *testing.T) {
	rs := feed(t, "openai-response", []string{
		`{"type":"response.created","response":{"id":"resp_a"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","content":[{"type":"output_text","text":"partial"}]}}`,
		`{"type":"response.steer.accepted","steer":{"id":"st_1","previous_response_id":"resp_a"}}`,
		`{"type":"response.created","response":{"id":"resp_b"}}`,
		`{"type":"response.completed","response":{"id":"resp_b","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"from completed"}]}]}}`,
		`{"type":"response.created","response":{"id":"resp_c"}}`,
	}, StatusCanceled)
	if len(rs) != 3 {
		t.Fatalf("responses = %d", len(rs))
	}
	if rs[0].ResponseID != "resp_a" || rs[0].TerminalStatus != StatusSuperseded || len(rs[0].Events) != 1 || rs[0].Events[0].SteerID != "st_1" {
		t.Fatalf("first = %+v", rs[0])
	}
	if rs[1].ResponseID != "resp_b" || !strings.Contains(string(rs[1].Body), "from completed") {
		t.Fatalf("second = %+v", rs[1])
	}
	if rs[2].ResponseID != "resp_c" || rs[2].TerminalStatus != StatusCanceled {
		t.Fatalf("third = %+v", rs[2])
	}
}

func TestGeminiStream(t *testing.T) {
	rs := feed(t, "gemini", []string{
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"think ","thought":true}]}}],"responseId":"r1"}`,
		`{"candidates":[{"content":{"parts":[{"text":"more","thought":true}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"answer ","thoughtSignature":"c2ln"}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"marker-delta"}]},"finishReason":"STOP"}],"usageMetadata":{"totalTokenCount":9}}`,
	}, StatusCompleted)
	if len(rs) != 1 || rs[0].TerminalStatus != StatusCompleted || rs[0].ResponseID != "r1" {
		t.Fatalf("responses = %+v", rs)
	}
	parts := body(t, rs[0])["candidates"].([]any)[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)
	if len(parts) != 2 || parts[0].(map[string]any)["text"] != "think more" || parts[1].(map[string]any)["text"] != "answer marker-delta" {
		t.Fatalf("parts = %v", parts)
	}
}

func TestGenericStream(t *testing.T) {
	rs := feed(t, "openai-image", []string{
		"data: {\"type\":\"image_generation.partial_image\",\"partial_image_b64\":\"aGVsbG8=\"}\n\n",
		"data: {\"type\":\"image_generation.completed\",\"b64_json\":\"aGk=\"}\n\n",
	}, StatusCompleted)
	if len(rs) != 1 || strings.Contains(string(rs[0].Body), "aGVsbG8=") || !strings.Contains(string(rs[0].Body), "partial_image") {
		t.Fatalf("generic = %+v %s", rs, rs[0].Body)
	}
}
