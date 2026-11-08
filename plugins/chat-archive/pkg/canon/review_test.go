package canon

// Canaries for detect-only re-checks, escaped secrets in tool output, attachment masking, base64
// forms, opaque positions, split budgets and masking deadlines. Every value is synthetic and random; failures name the case, never the value.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

func antToken(t *testing.T) string { return "sk-ant-api03-" + randString(t, alnum62+"_-", 93) + "AA" }

// leaks reports whether secret appears in out raw, or in base64 (standard or URL alphabet) at any of
// the three byte alignments.
func leaks(out []byte, secret string) bool {
	if bytes.Contains(out, []byte(secret)) {
		return true
	}
	for k := 0; k < 3; k++ {
		enc := base64.StdEncoding.EncodeToString(append(make([]byte, k), secret...))
		core := enc[[]int{0, 2, 3}[k] : len(enc)-4]
		url := strings.NewReplacer("+", "-", "/", "_").Replace(core)
		if bytes.Contains(out, []byte(core)) || bytes.Contains(out, []byte(url)) {
			return true
		}
	}
	return false
}

// realSplit runs the exported entry point with the real masker and checks the result with
// DetectPiece.
func realSplit(t *testing.T, in Input, response bool, opts Options) *Split {
	t.Helper()
	m := realMasker(t)
	opts.Masker = m
	split := SplitRequest
	if response {
		split = SplitResponse
	}
	sp, err := split(context.Background(), in, opts)
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, sp)
	for _, p := range sp.Pieces {
		if f, err := DetectPiece(p.Kind, p.ContentType, p.Bytes, m); err != nil || len(f) != 0 {
			t.Errorf("%s piece: DetectPiece found %v (err %v)", p.Kind, f, err)
		}
	}
	return sp
}

// TestDetectPiece: per-string detection of json pieces, whole-string text pieces, and attachments
// by their bytes and declared type.
func TestDetectPiece(t *testing.T) {
	m := realMasker(t)
	pw, key := randString(t, alnum62, 22), randString(t, alnum62, 20)
	sp := realSplit(t, Input{Format: FormatClaude, Body: []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"n","input":{"password":"` + pw + `","api_key":"` + key + `"}}]}]}`)}, false, Options{})
	var js Piece
	for _, p := range sp.Pieces {
		if p.Kind == wire.PieceJSON && bytes.Contains(p.Bytes, []byte(`"api_key"`)) {
			js = p
		}
	}
	if js.Bytes == nil {
		t.Fatal("no tool input piece")
	}
	if f, err := DetectPiece(wire.PieceJSON, "", js.Bytes, m); err != nil || len(f) != 0 {
		t.Errorf("tool input piece: %v %v", f, err)
	}

	tok := antToken(t)
	for _, c := range []struct {
		name string
		kind wire.PieceKind
		ct   string
		b    []byte
		want bool
	}{
		{"text", wire.PieceText, "", []byte("key " + tok), true},
		{"json value", wire.PieceJSON, "", []byte(`{"a":["key ` + tok + `"]}`), true},
		{"json key", wire.PieceJSON, "", []byte(`{"key ` + tok + `":1}`), true},
		{"utf-8 attachment, binary type", wire.PieceAttachment, "application/octet-stream", []byte("KEY=" + tok), true},
		{"latin-1 text attachment", wire.PieceAttachment, "text/plain; charset=latin1", []byte("caf\xe9 KEY=" + tok), true},
		{"binary attachment", wire.PieceAttachment, "image/png", []byte("\x89PNG KEY=" + tok), false},
	} {
		f, err := DetectPiece(c.kind, c.ct, c.b, m)
		if err != nil || (len(f) > 0) != c.want {
			t.Errorf("%s: findings %d err %v, want found=%v", c.name, len(f), err, c.want)
		}
	}
	if _, err := DetectPiece(wire.PieceJSON, "", []byte("{not json"), m); err == nil {
		t.Error("unparsable json piece: no error")
	}
	if _, err := DetectPiece(wire.PieceKind(9), "", nil, m); err == nil {
		t.Error("unknown kind: no error")
	}
}

// TestEscapedSecretInToolOutput: a function_call_output whose string is itself JSON holding
// "\n<token>\n" (the masker's decoded view, end to end).
func TestEscapedSecretInToolOutput(t *testing.T) {
	tok := "sk-proj-" + randString(t, alnum62+"_-", 60)
	inner, _ := json.Marshal(map[string]any{"output": "running\n" + tok + "\ndone\n", "metadata": map[string]int{"exit_code": 0}})
	outer, _ := json.Marshal(string(inner))
	body := `{"input":[{"type":"function_call_output","call_id":"c1","output":` + string(outer) + `}]}`
	sp := realSplit(t, Input{Format: FormatOpenAIResponses, Body: []byte(body)}, false, Options{})
	if leaks(allOutput(sp), tok) {
		t.Error("token reached the output")
	}
}

// TestTextAttachmentsAnyType: text in an attachment is masked whatever its declared type; a declared
// text type with invalid UTF-8 is masked run by run.
func TestTextAttachmentsAnyType(t *testing.T) {
	tok := antToken(t)
	doc := []byte("# generated\n" + strings.Repeat("x = 1\n", 180) + "API_KEY = \"" + tok + "\"\n")
	gemini := func(mime string, data []byte) Input {
		return Input{Format: FormatGemini, Body: []byte(`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"` + mime + `","data":"` + base64.StdEncoding.EncodeToString(data) + `"}}]}]}`)}
	}
	for _, mime := range []string{"application/x-python", "application/x-javascript", "application/yaml", "application/toml", "image/svg+xml", "application/ld+json", "text/x-python", "application/octet-stream"} {
		sp := realSplit(t, gemini(mime, doc), false, Options{})
		if leaks(allOutput(sp), tok) {
			t.Errorf("%s: token reached the output", mime)
		}
		if st := attStubOf(t, sp.Items[1].F); !st.Masked || st.MIME != mime {
			t.Errorf("%s: stub %+v", mime, st)
		}
	}
	latin1 := append([]byte("caf\xe9 notes\n"), doc...)
	sp := realSplit(t, gemini("text/plain", latin1), false, Options{})
	if leaks(allOutput(sp), tok) || !attStubOf(t, sp.Items[1].F).Masked {
		t.Error("latin-1 text/plain: token reached the output")
	}
	if !bytes.Contains(allOutput(sp), []byte("caf\xe9 notes")) {
		t.Error("latin-1 text/plain: the invalid byte must be kept")
	}
	// A binary type with invalid UTF-8 stays binary and unmasked.
	png := append([]byte("\x89PNG\r\n\x1a\n"), doc...)
	if st := attStubOf(t, realSplit(t, gemini("image/png", png), false, Options{}).Items[1].F); st.Masked {
		t.Error("binary png must not be masked")
	}

	// A multipart file part sent as application/octet-stream.
	var mp bytes.Buffer
	mp.WriteString("--b\r\nContent-Disposition: form-data; name=\"image\"; filename=\"notes.txt\"\r\nContent-Type: application/octet-stream\r\n\r\nANTHROPIC_API_KEY=" + tok + "\n\r\n--b--\r\n")
	sp = realSplit(t, Input{Format: FormatMediaPrefix + "images", Body: mp.Bytes(), ContentType: "multipart/form-data; boundary=b"}, false, Options{})
	if sp.Format != FormatMultipart || leaks(allOutput(sp), tok) {
		t.Errorf("multipart octet-stream: format %s, or token reached the output", sp.Format)
	}
	// An unparsable body with one invalid byte and a declared text type.
	sp = realSplit(t, Input{Format: FormatClaude, Body: append([]byte("oops \xff KEY="), tok...), ContentType: "text/plain"}, true, Options{})
	if sp.Format != FormatOpaque || leaks(allOutput(sp), tok) {
		t.Errorf("opaque latin-1: format %s, or token reached the output", sp.Format)
	}
}

// TestBase64Forms: small, URL-safe, unpadded and Interactions base64 text documents are decoded and
// masked; small text without a secret stays inline.
func TestBase64Forms(t *testing.T) {
	tok := antToken(t)
	small := []byte("ANTHROPIC_API_KEY=" + tok + "\n")
	big := []byte(strings.Repeat("line of notes\n", 80) + "ANTHROPIC_API_KEY=" + tok + "\n")
	gemini := func(data string) Input {
		return Input{Format: FormatGemini, Body: []byte(`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"text/plain","data":"` + data + `"}}]}]}`)}
	}
	for _, c := range []struct {
		name string
		in   Input
	}{
		{"gemini small std", gemini(base64.StdEncoding.EncodeToString(small))},
		{"gemini url", gemini(base64.URLEncoding.EncodeToString(big))},
		{"gemini raw std", gemini(base64.RawStdEncoding.EncodeToString(big))},
		{"gemini raw url", gemini(base64.RawURLEncoding.EncodeToString(big))},
		{"gemini wrapped", gemini(strings.Join(chunk(base64.StdEncoding.EncodeToString(big), 76), `\r\n`))},
		{"chat file_data small", Input{Format: FormatOpenAIChat, Body: []byte(`{"messages":[{"role":"user","content":[{"type":"file","file":{"filename":"a.env","file_data":"data:text/plain;base64,` + base64.StdEncoding.EncodeToString(small) + `"}}]}]}`)}},
		{"claude document small", Input{Format: FormatClaude, Body: []byte(`{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"text/plain","data":"` + base64.StdEncoding.EncodeToString(small) + `"}}]}]}`)}},
		{"interactions request", Input{Format: FormatInteractions, Body: []byte(`{"input":[{"type":"document","mime_type":"text/plain","data":"` + base64.StdEncoding.EncodeToString(big) + `"}]}`)}},
	} {
		sp := realSplit(t, c.in, false, Options{})
		if leaks(allOutput(sp), tok) {
			t.Errorf("%s: token reached the output (raw or base64)", c.name)
		}
		if countAttachments(sp) != 1 {
			t.Errorf("%s: %d attachments, want 1", c.name, countAttachments(sp))
		}
	}
	resp := `{"outputs":[{"type":"document","mime_type":"text/plain","data":"` + base64.StdEncoding.EncodeToString(big) + `"}]}`
	if sp := realSplit(t, Input{Format: FormatInteractions, Body: []byte(resp)}, true, Options{}); leaks(allOutput(sp), tok) || countAttachments(sp) != 1 {
		t.Error("interactions response: token reached the output")
	}
	// Small text without a secret, and a small binary, stay inline.
	plain := base64.StdEncoding.EncodeToString([]byte("just some synthetic notes\n"))
	if sp := realSplit(t, gemini(plain), false, Options{}); countAttachments(sp) != 0 || !bytes.Contains(allOutput(sp), []byte(plain)) {
		t.Error("small text without a secret must stay inline")
	}
}

func chunk(s string, n int) []string {
	var out []string
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	return append(out, s)
}

// TestOpaqueOnlyAtNamedPositions: payload data shaped like an opaque field is masked, not hashed.
func TestOpaqueOnlyAtNamedPositions(t *testing.T) {
	tok := antToken(t)
	sum := sha256.Sum256([]byte(tok))
	for _, c := range []struct {
		name string
		in   Input
	}{
		{"claude tool_use input thinking", Input{Format: FormatClaude, Body: []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"n","input":{"type":"thinking","signature":"` + tok + `"}}]}]}`)}},
		{"claude tool_result compaction", Input{Format: FormatClaude, Body: []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"compaction","encrypted_content":"` + tok + `"}]}]}]}`)}},
		{"gemini functionCall args", Input{Format: FormatGemini, Body: []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{"thoughtSignature":"` + tok + `"}}}]}]}`)}},
		{"gemini functionResponse", Input{Format: FormatGemini, Body: []byte(`{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"thought_signature":"` + tok + `"}}}]}]}`)}},
		{"responses call output", Input{Format: FormatOpenAIResponses, Body: []byte(`{"input":[{"type":"function_call_output","call_id":"c","output":[{"type":"reasoning","encrypted_content":"` + tok + `"}]}]}`)}},
		{"interactions function result", Input{Format: FormatInteractions, Body: []byte(`{"input":[{"type":"function_result","call_id":"c","result":{"type":"thought","signature":"` + tok + `"}}]}`)}},
	} {
		sp := realSplit(t, c.in, false, Options{})
		out := allOutput(sp)
		if leaks(out, tok) || bytes.Contains(out, []byte(hx(sum))) || !bytes.Contains(out, []byte("[masked:")) {
			t.Errorf("%s: payload value hashed or left raw, not masked", c.name)
		}
	}
}

// TestMaxNodes: the JSON value budget fails the split with ErrBodyBudget, tool arguments included.
func TestMaxNodes(t *testing.T) {
	e := newEnv(t)
	var b strings.Builder
	b.WriteString(`{"messages":[{"role":"user","content":"x"}],"metadata":{`)
	for i := 0; i < 500; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"k` + strings.Repeat("0", 3) + string(rune('a'+i%26)) + `_` + randString(t, alnum62, 4) + `":1`)
	}
	b.WriteString(`}}`)
	body := []byte(b.String())
	if _, err := e.splitter(Options{MaxNodes: 100}).split(Input{Format: FormatClaude, Body: body}, false); !errors.Is(err, ErrBodyBudget) {
		t.Errorf("over MaxNodes: err %v", err)
	}
	if _, err := e.splitter(Options{MaxNodes: 2000}).split(Input{Format: FormatClaude, Body: body}, false); err != nil {
		t.Errorf("under MaxNodes: err %v", err)
	}
	args, _ := json.Marshal(`[` + strings.Repeat(`1,`, 300) + `1]`)
	chat := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":` + string(args) + `}}]}]}`)
	if _, err := e.splitter(Options{MaxNodes: 100}).split(Input{Format: FormatOpenAIChat, Body: chat}, false); !errors.Is(err, ErrBodyBudget) {
		t.Errorf("tool arguments over MaxNodes: err %v", err)
	}
}

// TestGzipBomb: a small gzip body that expands past MaxDecoded fails fast with ErrBodyBudget.
func TestGzipBomb(t *testing.T) {
	e := newEnv(t)
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	zw.Write([]byte(`{"content":[{"type":"text","text":"`))
	zw.Write(bytes.Repeat([]byte("a"), 32<<20))
	zw.Write([]byte(`"}],"role":"assistant"}`))
	zw.Close()
	if _, err := e.splitter(Options{MaxDecoded: 1 << 20}).split(Input{Format: FormatClaude, Body: buf.Bytes(), ContentEncoding: "gzip"}, true); !errors.Is(err, ErrBodyBudget) {
		t.Errorf("gzip bomb: err %v", err)
	}
}

// TestDeadlineBoundsOneString: the masking deadline stops inside one large string (StringContext).
func TestDeadlineBoundsOneString(t *testing.T) {
	m := realMasker(t)
	var b strings.Builder
	for b.Len() < 4<<20 {
		b.WriteString(`api_key = "` + randString(t, alnum62, 24) + `"\n`)
	}
	text, _ := json.Marshal(b.String())
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":` + string(text) + `}]}]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := SplitRequest(ctx, Input{Format: FormatClaude, Body: body}, Options{Masker: m})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err %v, want DeadlineExceeded", err)
	}
	d := time.Since(start)
	t.Logf("deadline of 50ms: returned after %v", d)
	if d > 3*time.Second {
		t.Errorf("returned after %v", d)
	}
}

// TestCanonicalizeMaskedContext: the context-aware entry point gives CanonicalizeMasked's output,
// fails with ErrBodyBudget past maxNodes (0 means no cap), returns ctx.Err() and nothing else for an
// ended context, and stops inside one large string when a deadline passes.
func TestCanonicalizeMaskedContext(t *testing.T) {
	m := realMasker(t)
	canary := newCanary(t)
	tok := antToken(t)
	in := []byte(`{"t":"use ` + canary + ` now","` + tok + `":[1,true,null,"x"],"b":"\ud800"}`)
	want, wantHits, wantFlags, err := CanonicalizeMasked(in, m)
	if err != nil {
		t.Fatal(err)
	}
	for _, maxNodes := range []int{0, -1, 8} { // the body has exactly 8 values
		got, hits, flags, err := CanonicalizeMaskedContext(context.Background(), in, m, maxNodes)
		if err != nil {
			t.Fatalf("maxNodes %d: %v", maxNodes, err)
		}
		if !bytes.Equal(got, want) || !maps.Equal(hits, wantHits) || !slices.Equal(flags, wantFlags) {
			t.Errorf("maxNodes %d: differs from CanonicalizeMasked (hits %v, flags %v)", maxNodes, hits, flags)
		}
		if leaks(got, canary) || leaks(got, tok) || len(flags) != 1 || flags[0] != wire.FlagBadUTF8 {
			t.Errorf("maxNodes %d: canary leaks or flags %v", maxNodes, flags)
		}
	}
	if out, hits, flags, err := CanonicalizeMaskedContext(context.Background(), in, m, 7); !errors.Is(err, ErrBodyBudget) || out != nil || hits != nil || flags != nil {
		t.Errorf("over maxNodes: err %v", err)
	}
	if _, _, _, err := CanonicalizeMaskedContext(context.Background(), in, nil, 0); err == nil {
		t.Error("a nil Masker must fail")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, hits, flags, err := CanonicalizeMaskedContext(ctx, in, m, 0); !errors.Is(err, context.Canceled) || out != nil || hits != nil || flags != nil {
		t.Errorf("cancelled: %d bytes, err %v", len(out), err)
	}

	// A deadline stops one 4 MiB string.
	var b strings.Builder
	for b.Len() < 4<<20 {
		b.WriteString(`api_key = "` + randString(t, alnum62, 24) + `"\n`)
	}
	text, _ := json.Marshal(b.String())
	dctx, dcancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer dcancel()
	start := time.Now()
	out, _, _, err := CanonicalizeMaskedContext(dctx, []byte(`{"t":`+string(text)+`}`), m, 0)
	if !errors.Is(err, context.DeadlineExceeded) || out != nil {
		t.Errorf("deadline: err %v, %d bytes", err, len(out))
	}
	d := time.Since(start)
	t.Logf("deadline of 50ms: returned after %v", d)
	if d > 3*time.Second {
		t.Errorf("deadline: returned after %v", d)
	}
}
