package stream

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// deltaStream builds n small synthetic deltas per format, each "abcd".
func deltaStream(format string, n int) []string {
	var c []string
	switch format {
	case "claude":
		c = append(c, `event: message_start`+"\n"+`data: {"type":"message_start","message":{"id":"msg_s","role":"assistant","content":[]}}`+"\n\n",
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`+"\n\n")
		for i := 0; i < n; i++ {
			c = append(c, `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"abcd"}}`+"\n\n")
		}
		c = append(c, `data: {"type":"content_block_stop","index":0}`+"\n\n", `data: {"type":"message_stop"}`+"\n\n")
	case "openai":
		for i := 0; i < n; i++ {
			c = append(c, `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"abcd","tool_calls":[{"index":0,"function":{"arguments":"abcd"}}]}}]}`+"\n\n")
		}
		c = append(c, `data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n", "data: [DONE]\n\n")
	case "openai-response":
		c = append(c, `{"type":"response.created","response":{"id":"resp_s"}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ct_1"}}`)
		for i := 0; i < n; i++ {
			c = append(c, `{"type":"response.custom_tool_call_input.delta","output_index":0,"delta":"abcd"}`)
		}
		c = append(c, `{"type":"response.completed","response":{"id":"resp_s","status":"completed"}}`)
	case "gemini":
		for i := 0; i < n; i++ {
			c = append(c, `data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"abcd"}]}}]}`+"\n\n")
		}
		c = append(c, `data: {"candidates":[{"index":0,"finishReason":"STOP","content":{"parts":[]}}]}`+"\n\n")
	}
	return c
}

func allocated(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// TestReassemblyLinearInDeltas: accumulating text from small deltas costs linear work. String
// concatenation per delta copied the whole text each time (quadratic): 4x the deltas cost about
// 16x the allocation. The accumulated text must also be exact.
func TestReassemblyLinearInDeltas(t *testing.T) {
	for _, format := range []string{"claude", "openai", "openai-response", "gemini"} {
		var small, large []Response
		const n = 4096
		a1 := allocated(func() { small = feed(t, format, deltaStream(format, n), StatusCompleted) })
		a4 := allocated(func() { large = feed(t, format, deltaStream(format, 4*n), StatusCompleted) })
		if len(small) != 1 || len(large) != 1 {
			t.Fatalf("%s: responses %d / %d", format, len(small), len(large))
		}
		if want := strings.Repeat("abcd", 4*n); !strings.Contains(string(large[0].Body), want) {
			t.Fatalf("%s: accumulated text is not the concatenation of the deltas", format)
		}
		if ratio := float64(a4) / float64(a1); ratio > 6 {
			t.Errorf("%s: 4x the deltas allocated %.1fx (%d vs %d bytes): not linear", format, ratio, a4, a1)
		}
	}
}

// TestReassemblyMiBOfSmallDeltasIsFast bounds the time of one 1 MiB response made of 4-byte
// deltas (262,144 events). Before the fix it took minutes; the bound is loose for a loaded host.
func TestReassemblyMiBOfSmallDeltasIsFast(t *testing.T) {
	if raceEnabled {
		t.Skip("wall-clock bound; TestReassemblyLinearInDeltas covers the complexity under -race")
	}
	chunks := deltaStream("claude", (1<<20)/4)
	start := time.Now()
	out := feed(t, "claude", chunks, StatusCompleted)
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("1 MiB of small deltas took %v", d)
	}
	if len(out) != 1 || len(out[0].Body) < 1<<20 {
		t.Fatalf("response = %d bytes", len(out[0].Body))
	}
}

// TestResponsesPartIndexBounded: a negative content_index or summary_index is ignored (it used to
// panic in the stream goroutine), and a huge one is ignored instead of allocating a slice of that
// length.
func TestResponsesPartIndexBounded(t *testing.T) {
	for _, idx := range []string{"-1", "2000000", "1000000000000"} {
		chunks := []string{
			`{"type":"response.created","response":{"id":"resp_i"}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m1"}}`,
			fmt.Sprintf(`{"type":"response.content_part.added","output_index":0,"content_index":%s,"part":{"type":"output_text","text":""}}`, idx),
			fmt.Sprintf(`{"type":"response.output_text.delta","output_index":0,"content_index":%s,"delta":"x"}`, idx),
			fmt.Sprintf(`{"type":"response.output_text.done","output_index":0,"content_index":%s,"text":"x"}`, idx),
			fmt.Sprintf(`{"type":"response.output_text.annotation.added","output_index":0,"content_index":%s,"annotation":{}}`, idx),
			fmt.Sprintf(`{"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":%s,"part":{"type":"summary_text"}}`, idx),
			fmt.Sprintf(`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":%s,"delta":"y"}`, idx),
			fmt.Sprintf(`{"type":"response.reasoning_summary_text.done","output_index":0,"summary_index":%s,"text":"y"}`, idx),
			`{"type":"response.output_text.delta","output_index":0,"content_index":1,"delta":"kept"}`,
			`{"type":"response.completed","response":{"id":"resp_i","status":"completed"}}`,
		}
		start := time.Now()
		out := feed(t, "openai-response", chunks, StatusCompleted)
		if time.Since(start) > time.Second || len(out) != 1 {
			t.Fatalf("index %s: %d responses in %v", idx, len(out), time.Since(start))
		}
		if len(out[0].Body) > 4096 || !strings.Contains(string(out[0].Body), `"text":"kept"`) {
			t.Fatalf("index %s: body %d bytes", idx, len(out[0].Body))
		}
	}
}

// TestInteractionsStreamSignatureStubbed: an Interactions stream carries the thought
// signature in a thought_signature delta; after reassembly and the canon split, only its sha256
// and length remain, the thought summary text is kept, and a "signature" inside tool arguments is
// user data and left for masking.
func TestInteractionsStreamSignatureStubbed(t *testing.T) {
	sig := "CsoB" + strings.Repeat("Zm9vYmFy", 16)
	ev := func(name, data string) string { return "event: " + name + "\ndata: " + data + "\n\n" }
	chunks := []string{
		ev("interaction.created", `{"event_type":"interaction.created","interaction":{"id":"int_ca","status":"in_progress","object":"interaction"}}`),
		ev("step.start", `{"event_type":"step.start","index":0,"step":{"type":"thought"}}`),
		ev("step.delta", `{"event_type":"step.delta","index":0,"delta":{"type":"thought_summary","content":{"type":"text","text":"thinking-marker-synthetic"}}}`),
		ev("step.delta", `{"event_type":"step.delta","index":0,"delta":{"type":"thought_signature","signature":"`+sig+`"}}`),
		ev("step.stop", `{"event_type":"step.stop","index":0}`),
		ev("step.start", `{"event_type":"step.start","index":1,"step":{"type":"function_call","name":"f","arguments":{"type":"thought_signature","signature":"tool-arg-synthetic"}}}`),
		ev("interaction.completed", `{"event_type":"interaction.completed","interaction":{"id":"int_ca","status":"completed"}}`),
	}
	out := feed(t, "interactions", chunks, StatusCompleted)
	if len(out) != 1 {
		t.Fatalf("responses = %d", len(out))
	}
	body := string(out[0].Body)
	sum := sha256.Sum256([]byte(sig))
	if strings.Contains(body, sig) || !strings.Contains(body, hex.EncodeToString(sum[:])) {
		t.Fatal("thought_signature delta not stubbed with its sha256")
	}
	if !strings.Contains(body, "thinking-marker-synthetic") || !strings.Contains(body, "tool-arg-synthetic") {
		t.Fatal("thought summary or tool argument lost")
	}
}

// TestAssemblerLargeEventManyChunks: one 4 MiB data event split into 1 KiB chunks is assembled
// with linear work (it used to be re-validated and re-copied at every chunk end).
func TestAssemblerLargeEventManyChunks(t *testing.T) {
	build := func(size int) []byte {
		var b strings.Builder
		b.WriteString(`data: {"type":"response.completed","response":{"id":"r","output":[`)
		for b.Len() < size {
			b.WriteString(`{"type":"message","content":[{"type":"output_text","text":"abcdefgh"}]},`)
		}
		b.WriteString(`{}]}}`)
		b.WriteString("\n\n")
		return []byte(b.String())
	}
	run := func(ev []byte) int {
		var a Assembler
		n := 0
		for i := 0; i < len(ev); i += 1024 {
			n += len(a.Add(ev[i:min(i+1024, len(ev))]))
		}
		return n + len(a.Finish())
	}
	small, large := build(1<<20), build(4<<20)
	var n1, n4 int
	a1 := allocated(func() { n1 = run(small) })
	a4 := allocated(func() { n4 = run(large) })
	if n1 != 1 || n4 != 1 {
		t.Fatalf("events %d / %d", n1, n4)
	}
	if ratio := float64(a4) / float64(a1); ratio > 6 {
		t.Errorf("4x the event size allocated %.1fx (%d vs %d bytes): not linear", ratio, a4, a1)
	}
}
