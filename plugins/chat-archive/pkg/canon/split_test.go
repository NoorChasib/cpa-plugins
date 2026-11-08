package canon

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

type splitCase struct {
	name     string
	file     string            // body from testdata/split/<file>, placeholders expanded
	body     func(*env) []byte // or a body built in Go
	response bool
	format   string
	ct, ce   string
	opts     Options
	secrets  bool // the input holds the canary: golden only with the test masker
}

func splitCases() []splitCase {
	return []splitCase{
		{name: "claude-request-text", file: "claude-request-text.in.json", format: FormatClaude},
		{name: "claude-request-tools", file: "claude-request-tools.in.json", format: FormatClaude},
		{name: "claude-request-thinking", file: "claude-request-thinking.in.json", format: FormatClaude},
		{name: "claude-request-media", file: "claude-request-media.in.json", format: FormatClaude},
		{name: "claude-request-oversize", file: "claude-request-media.in.json", format: FormatClaude,
			opts: Options{MaxAttachment: 2000}},
		{name: "claude-response", file: "claude-response.in.json", format: FormatClaude, response: true},
		{name: "claude-response-gzip", body: func(e *env) []byte { return gzipBytes(e.readInput("claude-response.in.json")) },
			format: FormatClaude, response: true, ce: "gzip"},
		{name: "chat-request", file: "chat-request.in.json", format: FormatOpenAIChat},
		{name: "chat-response", file: "chat-response.in.json", format: FormatOpenAIChat, response: true},
		{name: "responses-request", file: "responses-request.in.json", format: FormatOpenAIResponses},
		{name: "responses-request-string", file: "responses-request-string.in.json", format: FormatOpenAIResponses},
		{name: "responses-ws-create", file: "responses-ws-create.in.json", format: FormatOpenAIResponses},
		{name: "responses-ws-append", file: "responses-ws-append.in.json", format: FormatOpenAIResponses},
		{name: "responses-ws-steer", file: "responses-ws-steer.in.json", format: FormatOpenAIResponses},
		{name: "responses-response", file: "responses-response.in.json", format: FormatOpenAIResponses, response: true},
		{name: "gemini-request", file: "gemini-request.in.json", format: FormatGemini},
		{name: "gemini-response", file: "gemini-response.in.json", format: FormatGemini, response: true},
		{name: "interactions-request", file: "interactions-request.in.json", format: FormatInteractions},
		{name: "interactions-response", file: "interactions-response.in.json", format: FormatInteractions, response: true},
		{name: "media-images-request", file: "media-images-request.in.json", format: FormatMediaPrefix + "images"},
		{name: "media-images-response", file: "media-images-response.in.json", format: FormatMediaPrefix + "images", response: true},
		{name: "media-video-poll-response", file: "media-video-poll-response.in.json", format: FormatMediaPrefix + "videos", response: true},
		{name: "media-speech-response", body: func(e *env) []byte { return e.synth("audio", 3000) },
			format: FormatMediaPrefix + "speech", response: true, ct: "audio/mpeg"},
		{name: "multipart-image-edits", body: multipartBody, format: FormatMediaPrefix + "images",
			ct: "multipart/form-data; boundary=synthboundary", secrets: true},
		{name: "opaque-text", body: func(*env) []byte { return []byte("plain synthetic text body\n") }, format: FormatOpaque},
		{name: "opaque-binary", body: func(e *env) []byte { return e.synth("bin", 1500) }, format: FormatOpaque,
			ct: "application/octet-stream"},
		{name: "opaque-json", body: func(*env) []byte { return []byte(`{"b":[1,2],"a":"synthetic"}`) }, format: FormatOpaque},
		{name: "claude-not-object", body: func(*env) []byte { return []byte(`[1, 2, 3]`) }, format: FormatClaude},
		{name: "claude-bad-json", body: func(*env) []byte { return []byte(`{"model": "claude-test-model",`) }, format: FormatClaude},
		{name: "p23-text-document", file: "p23-text-document.in.json", format: FormatClaude, secrets: true},
		{name: "secrets-volatile", file: "secrets-volatile.in.json", format: FormatClaude, secrets: true},
	}
}

func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}

func multipartBody(e *env) []byte {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.SetBoundary("synthboundary")
	w.WriteField("model", "gpt-image-test")
	w.WriteField("prompt", "Paint the synthetic fence blue.")
	w.WriteField("n", "1")
	file := func(name, filename, ct string, data []byte, withFilename bool) {
		h := textproto.MIMEHeader{}
		if withFilename {
			h.Set("Content-Disposition", multipart.FileContentDisposition(name, filename))
		} else {
			h.Set("Content-Disposition", `form-data; name="`+name+`"`)
		}
		h.Set("Content-Type", ct)
		p, _ := w.CreatePart(h)
		p.Write(data)
	}
	file("image", "fence.png", "image/png", e.synth("png", 2048), true)
	file("mask", "", "image/png", e.synth("png", 1100), true)
	file("notes", "notes.txt", "text/plain; charset=utf-8", e.synth("secrettext", 1100), true)
	file("raw", "", "application/octet-stream", e.synth("bin", 900), false)
	w.Close()
	return buf.Bytes()
}

func (c splitCase) input(e *env) Input {
	var body []byte
	if c.body != nil {
		body = c.body(e)
	} else {
		body = e.readInput(c.file)
	}
	return Input{Format: c.format, Body: body, ContentType: c.ct, ContentEncoding: c.ce}
}

func (c splitCase) run(e *env) *Split {
	if c.response {
		return e.response(c.input(e), c.opts)
	}
	return e.request(c.input(e), c.opts)
}

// TestSplitGolden pins every split rule in testdata/split/*.golden.json (test masker,
// which masks only the run's canary). Inputs are synthetic.
func TestSplitGolden(t *testing.T) {
	for _, c := range splitCases() {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			sp := c.run(e)
			checkInvariants(t, sp)
			if bytes.Contains(allOutput(sp), []byte(e.canary)) {
				t.Fatal("the canary reached the output")
			}
			checkGolden(t, "split/"+c.name+".golden.json", dumpSplit(sp))
		})
	}
}

// TestSplitGoldenRealMasker re-runs the secret-free goldens with pkg/mask: real masking must leave
// synthetic text unchanged, so the bytes equal the goldens (needs pkg/mask).
func TestSplitGoldenRealMasker(t *testing.T) {
	m := realMasker(t)
	for _, c := range splitCases() {
		if c.secrets {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.m = m
			sp := c.run(e)
			if len(sp.Hits) != 0 {
				t.Errorf("real masker hit synthetic text: %v", sp.Hits)
			}
			got := dumpSplit(sp)
			want := readGolden(t, "split/"+c.name+".golden.json")
			if !bytes.Equal(got, want) {
				t.Errorf("real-masker output differs from the golden:\n%s", got)
			}
		})
	}
}

func readGolden(t *testing.T, path string) []byte {
	t.Helper()
	b, err := readTestdata(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// findItem returns the first item whose F (or M) contains sub.
func findItem(t testing.TB, items []wire.Item, sub string) wire.Item {
	t.Helper()
	for _, it := range items {
		if bytes.Contains(it.F, []byte(sub)) || bytes.Contains(it.M, []byte(sub)) {
			return it
		}
	}
	t.Fatalf("no item contains %q", sub)
	return wire.Item{}
}

func piece(t testing.TB, sp *Split, h wire.Hash) Piece {
	t.Helper()
	for _, p := range sp.Pieces {
		if p.Hash == h {
			return p
		}
	}
	t.Fatalf("no piece %s", hx(h))
	return Piece{}
}

func attStubOf(t testing.TB, frame []byte) AttStub {
	t.Helper()
	i := bytes.Index(frame, []byte(`{"$att":`))
	if i < 0 {
		t.Fatalf("no $att in %s", frame)
	}
	dec := json.NewDecoder(bytes.NewReader(frame[i:]))
	var v struct {
		Att AttStub `json:"$att"`
	}
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v.Att
}

// TestP23TextDocument: a base64 text/plain document holding a secret yields "masked":true, sha256 and
// len of the masked text, an attachment piece of exactly those bytes, and no secret anywhere.
func TestP23TextDocument(t *testing.T) {
	check := func(t *testing.T, e *env, placeholder string) {
		body := e.readInput("p23-text-document.in.json")
		sp := e.request(Input{Format: FormatClaude, Body: body}, Options{})
		checkInvariants(t, sp)
		doc := findItem(t, sp.Items, `"type":"document"`)
		stub := attStubOf(t, doc.F)
		decoded := e.synth("secrettext", 1500)
		masked, _ := e.m.String(string(decoded))
		if !stub.Masked || stub.MIME != "text/plain" || stub.Enc != "base64" {
			t.Errorf("stub %+v", stub)
		}
		if stub.SHA256 != sha256.Sum256([]byte(masked)) || stub.Len != int64(len(masked)) {
			t.Errorf("stub sha256/len are not those of the masked text")
		}
		att := piece(t, sp, stub.SHA256)
		if att.Kind != wire.PieceAttachment || string(att.Bytes) != masked || att.ContentType != "text/plain" {
			t.Errorf("attachment piece is not the masked text")
		}
		if !strings.Contains(masked, placeholder) {
			t.Errorf("masked text has no %q placeholder", placeholder)
		}
		out := allOutput(sp)
		if bytes.Contains(out, []byte(e.canary)) || bytes.Contains(out, decoded) {
			t.Error("secret or unmasked document reached the output")
		}
		if sp.Hits == nil {
			t.Error("no hits counted")
		}
	}
	t.Run("test-masker", func(t *testing.T) { check(t, newEnv(t), canaryPlaceholder) })
	// With pkg/mask.
	t.Run("real-masker", func(t *testing.T) {
		e := newEnv(t)
		e.m = realMasker(t)
		check(t, e, "[masked:ca-archive-token")
	})
}

// TestVolatileMasked: secrets in cache_control-style volatile fields are masked in V and ToolsV, and
// V never enters item identity.
func TestVolatileMasked(t *testing.T) {
	run := func(t *testing.T, e *env, placeholder string) {
		sp := e.request(Input{Format: FormatClaude, Body: e.readInput("secrets-volatile.in.json")}, Options{})
		checkInvariants(t, sp)
		if bytes.Contains(allOutput(sp), []byte(e.canary)) {
			t.Fatal("canary in output")
		}
		if !bytes.Contains(sp.ToolsV, []byte(placeholder)) || !bytes.HasPrefix(sp.ToolsV, []byte(`[{"i":0,"v":{"cache_control":`)) {
			t.Errorf("ToolsV not masked as expected: %s", sp.ToolsV)
		}
		var vs int
		for _, it := range append(append([]wire.Item{}, sp.System...), sp.Items...) {
			if len(it.V) > 0 {
				vs++
				if !bytes.Contains(it.V, []byte(placeholder)) {
					t.Errorf("V without the placeholder: %s", it.V)
				}
				noV := it
				noV.V = nil
				if ItemID(noV) != ItemID(it) {
					t.Error("V changed the item id")
				}
			}
		}
		if vs != 3 {
			t.Errorf("%d items with V, want 3", vs)
		}
	}
	t.Run("test-masker", func(t *testing.T) { run(t, newEnv(t), canaryPlaceholder) })
	// With pkg/mask.
	t.Run("real-masker", func(t *testing.T) {
		e := newEnv(t)
		e.m = realMasker(t)
		run(t, e, "[masked:ca-archive-token")
	})
}

// TestCacheControlMovesKeepIDs: clients move cache_control to the newest block every turn; the items of
// the earlier turn keep their ids, so the prefix delta still matches.
func TestCacheControlMovesKeepIDs(t *testing.T) {
	e := newEnv(t)
	turn1 := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"A","cache_control":{"type":"ephemeral"}}]}]}`
	turn2 := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"A"}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"B"}]},` +
		`{"role":"user","content":[{"type":"text","text":"C","cache_control":{"type":"ephemeral"}}]}]}`
	a := ItemIDs(e.request(Input{Format: FormatClaude, Body: []byte(turn1)}, Options{}).Items)
	b := ItemIDs(e.request(Input{Format: FormatClaude, Body: []byte(turn2)}, Options{}).Items)
	if LCP(a, b) != len(a) || len(a) != 2 || len(b) != 6 {
		t.Errorf("LCP %d of %d/%d", LCP(a, b), len(a), len(b))
	}
}

// TestOpaqueStubsEveryField covers every opaque field: the value never appears and its
// stub carries the sha256 and length of the exact string as sent.
func TestOpaqueStubsEveryField(t *testing.T) {
	type oc struct {
		field, format, body string
		response            bool
	}
	cases := []oc{
		{"claude thinking.signature", FormatClaude, `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"%s"}]}]}`, false},
		{"claude signature_delta", FormatClaude, `{"messages":[{"role":"assistant","content":[{"type":"signature_delta","signature":"%s"}]}]}`, false},
		{"claude redacted_thinking.data", FormatClaude, `{"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"%s"}]}]}`, false},
		{"claude compaction.encrypted_content", FormatClaude, `{"messages":[{"role":"assistant","content":[{"type":"compaction","content":"c","encrypted_content":"%s"}]}]}`, false},
		{"claude compaction.signature", FormatClaude, `{"messages":[{"role":"assistant","content":[{"type":"compaction","content":"c","signature":"%s"}]}]}`, false},
		{"claude advisor_redacted_result.encrypted_content", FormatClaude, `{"content":[{"type":"advisor_tool_result","tool_use_id":"x","content":{"type":"advisor_redacted_result","encrypted_content":"%s"}}],"role":"assistant"}`, true},
		{"responses reasoning.encrypted_content", FormatOpenAIResponses, `{"input":[{"type":"reasoning","summary":[],"encrypted_content":"%s"}]}`, false},
		{"responses compaction.encrypted_content", FormatOpenAIResponses, `{"output":[{"type":"compaction","encrypted_content":"%s"}]}`, true},
		{"chat reasoning_details reasoning.encrypted data", FormatOpenAIChat, `{"messages":[{"role":"assistant","reasoning_details":[{"type":"reasoning.encrypted","data":"%s"}]}]}`, false},
		{"chat reasoning_details signature", FormatOpenAIChat, `{"choices":[{"message":{"role":"assistant","reasoning_details":[{"type":"reasoning.text","text":"t","signature":"%s"}]}}]}`, true},
		{"chat tool_calls extra_content.google.thought_signature", FormatOpenAIChat, `{"messages":[{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"},"extra_content":{"google":{"thought_signature":"%s"}}}]}]}`, false},
		{"gemini thoughtSignature", FormatGemini, `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f"},"thoughtSignature":"%s"}]}]}`, false},
		{"gemini thought_signature", FormatGemini, `{"candidates":[{"content":{"role":"model","parts":[{"text":"x","thought_signature":"%s"}]}}]}`, true},
		{"interactions thought signature", FormatInteractions, `{"input":[{"type":"thought","summary":[],"signature":"%s"}]}`, false},
		{"interactions response thought signature", FormatInteractions, `{"steps":[{"type":"thought","signature":"%s"}]}`, true},
	}
	e := newEnv(t)
	for i, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			val := "T1BBUVVFLXN5bnRoZXRpYy0" + string(rune('A'+i)) + "é=="
			body := strings.Replace(c.body, "%s", val, 1)
			in := Input{Format: c.format, Body: []byte(body)}
			var sp *Split
			if c.response {
				sp = e.response(in, Options{})
			} else {
				sp = e.request(in, Options{})
			}
			checkInvariants(t, sp)
			out := allOutput(sp)
			if bytes.Contains(out, []byte(val)) {
				t.Fatalf("opaque value reached the output")
			}
			h := sha256.Sum256([]byte(val))
			stub := `{"$opaque":{"len":` + strconv.Itoa(len(val)) + `,"sha256":"` + hx(h) + `"}}`
			if !bytes.Contains(out, []byte(stub)) {
				t.Errorf("stub %s not found in output", stub)
			}
		})
	}
}

// TestAttachmentThreshold: payloads under 1 KiB decoded stay inline; 1 KiB and more become
// attachments.
func TestAttachmentThreshold(t *testing.T) {
	e := newEnv(t)
	for n, wantAtt := range map[int]bool{1023: false, 1024: true} {
		b64 := base64.StdEncoding.EncodeToString(e.synth("png", n))
		body := `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}]}]}`
		sp := e.request(Input{Format: FormatClaude, Body: []byte(body)}, Options{})
		f := sp.Items[1].F
		if got := bytes.Contains(f, []byte(`"$att"`)); got != wantAtt {
			t.Errorf("%d bytes: attachment %v, want %v", n, got, wantAtt)
		}
		if !wantAtt && !bytes.Contains(f, []byte(b64)) {
			t.Errorf("%d bytes: base64 not kept inline", n)
		}
	}
}

// TestArgumentsCanonical: chat/Responses arguments strings that parse are canonicalized, so equal
// values hash equal; unparsable strings are text pieces with args_raw.
func TestArgumentsCanonical(t *testing.T) {
	e := newEnv(t)
	call := func(args string) *Split {
		a, _ := json.Marshal(args)
		body := `{"messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":` + string(a) + `}}]}]}`
		return e.request(Input{Format: FormatOpenAIChat, Body: []byte(body)}, Options{})
	}
	x := call(`{"b": [1, 2.50], "a": {"z": null, "y": "é"}}`)
	y := call(`{"a":{"y":"é","z":null},"b":[1,2.50]}`)
	if *x.Items[1].P != *y.Items[1].P || ItemID(x.Items[1]) != ItemID(y.Items[1]) {
		t.Error("equal arguments hash differently")
	}
	p := piece(t, x, *x.Items[1].P)
	if p.Kind != wire.PieceJSON || string(p.Bytes) != `"{\"a\":{\"y\":\"é\",\"z\":null},\"b\":[1,2.50]}"` {
		t.Errorf("arguments piece %s", p.Bytes)
	}
	raw := call(`{"a": 1,`)
	if !slices.Contains(raw.Flags, wire.FlagArgsRaw) || piece(t, raw, *raw.Items[1].P).Kind != wire.PieceText {
		t.Errorf("unparsable arguments: flags %v", raw.Flags)
	}
}

// TestGzipSamePieces: a gzip Claude response splits into the same items and pieces as the plain one.
func TestGzipSamePieces(t *testing.T) {
	e := newEnv(t)
	plain := e.readInput("claude-response.in.json")
	a := e.response(Input{Format: FormatClaude, Body: plain}, Options{})
	b := e.response(Input{Format: FormatClaude, Body: gzipBytes(plain), ContentEncoding: "gzip"}, Options{})
	if !slices.Equal(ItemIDs(a.Items), ItemIDs(b.Items)) || !slices.Equal(a.Refs(0), b.Refs(0)) {
		t.Error("gzip body split differently")
	}
	if !slices.Contains(b.Flags, wire.FlagGunzipped) || b.RawLen == a.RawLen {
		t.Errorf("flags %v raw_len %d", b.Flags, b.RawLen)
	}
	// A body still content-encoded never becomes a piece: not gzip at all, a truncated stream, a corrupt
	// stream, or an encoding other than gzip is ErrUndecodableBody; past MaxDecoded is ErrBodyBudget.
	gz := gzipBytes(plain)
	corrupt := append([]byte(nil), gz...)
	corrupt[len(corrupt)/2] ^= 0xff
	var flated bytes.Buffer
	fw, _ := flate.NewWriter(&flated, flate.DefaultCompression)
	fw.Write(plain)
	fw.Close()
	for _, c := range []struct {
		name, ce string
		body     []byte
		max      int64
		want     error
	}{
		{"not gzip", "gzip", []byte("not gzip"), 0, ErrUndecodableBody},
		{"truncated", "gzip", gz[:len(gz)-8], 0, ErrUndecodableBody},
		{"corrupt", "gzip", corrupt, 0, ErrUndecodableBody},
		{"deflate", "deflate", flated.Bytes(), 0, ErrUndecodableBody},
		{"br", "br", plain, 0, ErrUndecodableBody},
		{"over MaxDecoded", "gzip", gz, int64(len(plain) - 1), ErrBodyBudget},
	} {
		sp, err := e.splitter(Options{MaxDecoded: c.max}).split(Input{Format: FormatClaude, Body: c.body, ContentEncoding: c.ce}, true)
		if !errors.Is(err, c.want) || sp != nil {
			t.Errorf("%s: err %v, want %v", c.name, err, c.want)
		}
	}
	for _, ce := range []string{"", "identity", " Identity "} {
		if sp := e.response(Input{Format: FormatClaude, Body: plain, ContentEncoding: ce}, Options{}); !slices.Equal(ItemIDs(a.Items), ItemIDs(sp.Items)) {
			t.Errorf("Content-Encoding %q split differently", ce)
		}
	}
	if sp := e.response(Input{Format: FormatClaude, Body: gz, ContentEncoding: "gzip"}, Options{MaxDecoded: int64(len(plain))}); !slices.Equal(ItemIDs(a.Items), ItemIDs(sp.Items)) {
		t.Error("gzip at exactly MaxDecoded split differently")
	}
}

// TestMediaURLs: every absolute http(s) URL in a media response is replaced by its stripped form and
// reported in body order; no query string or userinfo reaches the output.
func TestMediaURLs(t *testing.T) {
	e := newEnv(t)
	sp := e.response(Input{Format: FormatMediaPrefix + "images", Body: e.readInput("media-images-response.in.json")}, Options{})
	var got []string
	for _, u := range sp.MediaURLs {
		got = append(got, u.URL+" -> "+u.MaskedURL)
	}
	want := []string{
		"https://user:pw@images.example.test/gen/abc123.png?X-Amz-Signature=deadbeefcafe&X-Amz-Expires=3600#frag -> https://images.example.test/gen/abc123.png",
		"https://cdn.example.test/v1/x.png?sig=0011223344 -> https://cdn.example.test/v1/x.png",
	}
	if !slices.Equal(got, want) {
		t.Errorf("media urls\n got %q\nwant %q", got, want)
	}
	out := allOutput(sp)
	for _, bad := range []string{"X-Amz-Signature", "deadbeefcafe", "sig=", "user:pw", "#frag"} {
		if bytes.Contains(out, []byte(bad)) {
			t.Errorf("%q reached the output", bad)
		}
	}
	stripped, urls, err := stripMediaURLs(e.readInput("media-images-response.in.json"), fakeStripURL)
	if err != nil || len(urls) != 2 || bytes.Contains(stripped, []byte("?")) {
		t.Errorf("stripMediaURLs: %v %d %s", err, len(urls), stripped)
	}
}

// TestMediaURLsReal runs the exported StripMediaURLs and SplitResponse with pkg/mask and
// pkg/headers.StripURL.
func TestMediaURLsReal(t *testing.T) {
	m := realMasker(t)
	e := newEnv(t)
	body := e.readInput("media-images-response.in.json")
	stripped, urls, err := StripMediaURLs(body, m)
	if err != nil || len(urls) != 2 {
		t.Fatalf("StripMediaURLs: %v %d", err, len(urls))
	}
	if bytes.Contains(stripped, []byte("X-Amz-Signature")) || bytes.Contains(stripped, []byte("user:pw")) {
		t.Errorf("signed URL survived: %s", stripped)
	}
	sp, err := SplitResponse(context.Background(), Input{Format: FormatMediaPrefix + "images", Body: body}, Options{Masker: m})
	if err != nil {
		t.Fatal(err)
	}
	for i, u := range sp.MediaURLs {
		if u.URL != urls[i].URL || u.MaskedURL != urls[i].MaskedURL {
			t.Errorf("url %d differs between StripMediaURLs and SplitResponse", i)
		}
		if !bytes.Contains(allOutput(sp), []byte(`"`+u.MaskedURL+`"`)) {
			t.Errorf("masked url %d is not the string in the body", i)
		}
	}
	if bytes.Contains(allOutput(sp), []byte("X-Amz-Signature")) {
		t.Error("query string reached a piece")
	}
}

// TestMediaURLEscapedHost: a media URL whose host holds an escape that url.Parse decodes to a byte
// that isn't valid UTF-8 (found by FuzzSplit) still yields canon/v1 pieces, and MaskedURL is the
// exact string in the body.
func TestMediaURLEscapedHost(t *testing.T) {
	m := realMasker(t)
	body := []byte(`{"data":[{"url":"http://u@h%80x.example.test/p%80q.png?sig=1"},{"url":"https://bücher.example.test/a.png"}]}`)
	sp, err := SplitResponse(context.Background(), Input{Format: FormatMediaPrefix + "images", Body: body}, Options{Masker: m})
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, sp)
	want := []string{"http://h%80x.example.test/p%80q.png", "https://b%C3%BCcher.example.test/a.png"}
	if len(sp.MediaURLs) != len(want) {
		t.Fatalf("%d media urls", len(sp.MediaURLs))
	}
	for i, u := range sp.MediaURLs {
		if u.MaskedURL != want[i] {
			t.Errorf("url %d: MaskedURL %q", i, u.MaskedURL)
		}
		if !bytes.Contains(allOutput(sp), []byte(`"`+u.MaskedURL+`"`)) {
			t.Errorf("url %d: MaskedURL is not the string in the body", i)
		}
	}
}

// TestWebSocketItemsMatchHTTP: Responses WebSocket messages (response.create with
// previous_response_id, response.append, response.steer) split their input items exactly like the
// same items in an HTTP body, so prefix-delta and dedup work across transports.
func TestWebSocketItemsMatchHTTP(t *testing.T) {
	e := newEnv(t)
	httpSp := e.request(Input{Format: FormatOpenAIResponses, Body: e.readInput("responses-request.in.json")}, Options{})
	ws := e.request(Input{Format: FormatOpenAIResponses, Body: e.readInput("responses-ws-create.in.json")}, Options{})
	app := e.request(Input{Format: FormatOpenAIResponses, Body: e.readInput("responses-ws-append.in.json")}, Options{})
	httpIDs := ItemIDs(httpSp.Items)
	out := findItem(t, ws.Items, `"call_id":"call_synth_3","type":"function_call_output"`)
	if !slices.Contains(httpIDs, ItemID(out)) {
		t.Error("function_call_output over WebSocket differs from HTTP")
	}
	wsIDs, appIDs := ItemIDs(ws.Items), ItemIDs(app.Items)
	if !slices.Equal(wsIDs[1:], appIDs) {
		t.Error("response.append items differ from the same items in response.create")
	}
	skel := piece(t, ws, *ws.Skeleton)
	for _, k := range []string{`"type":"response.create"`, `"previous_response_id":"resp_synth_prev"`, `"stream_id":"lane-1"`, `"event_id":"evt_synth_1"`} {
		if !bytes.Contains(skel.Bytes, []byte(k)) {
			t.Errorf("skeleton lacks %s", k)
		}
	}
	// The response's output items, re-sent as input in the next merged turn, keep their ids.
	resp := e.response(Input{Format: FormatOpenAIResponses, Body: e.readInput("responses-response.in.json")}, Options{})
	var next bytes.Buffer
	next.WriteString(`{"type":"response.create","input":`)
	var r struct {
		Output json.RawMessage `json:"output"`
	}
	json.Unmarshal(e.readInput("responses-response.in.json"), &r)
	next.Write(r.Output)
	next.WriteString(`}`)
	merged := e.request(Input{Format: FormatOpenAIResponses, Body: next.Bytes()}, Options{})
	if !slices.Equal(ItemIDs(resp.Items), ItemIDs(merged.Items)) {
		t.Error("response output items re-sent as input changed ids")
	}
	if resp.System == nil || resp.Tools == nil || *resp.System[0].P != *httpSp.System[0].P || *resp.Tools != *httpSp.Tools {
		t.Error("response instructions/tools are not split like the request's")
	}
}

// TestRefsFrom: Refs(from) keeps Skeleton, Tools and System, drops the items before from, and keeps the
// $att stubs nested inside json payloads.
func TestRefsFrom(t *testing.T) {
	e := newEnv(t)
	sp := e.request(Input{Format: FormatClaude, Body: e.readInput("claude-request-tools.in.json")}, Options{})
	all := sp.Refs(0)
	// The tool_result list with the nested 2 KiB screenshot.
	idx := -1
	for i, it := range sp.Items {
		if bytes.Contains(it.F, []byte(`"tool_use_id":"toolu_synth_04"`)) {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatal("tool_result item not found")
	}
	var att wire.Hash
	for _, p := range sp.Pieces {
		if p.Kind == wire.PieceAttachment {
			att = p.Hash
		}
	}
	if !slices.Contains(all, att) || !slices.Contains(sp.Refs(idx), att) || slices.Contains(sp.Refs(idx+1), att) {
		t.Error("nested attachment ref not tracked with its item")
	}
	last := sp.Refs(len(sp.Items))
	if !slices.Equal(last, []wire.Hash{*sp.Tools, *sp.Skeleton}) && !slices.Equal(last, []wire.Hash{*sp.Skeleton, *sp.Tools}) {
		t.Errorf("Refs(len) = %d hashes, want Skeleton and Tools", len(last))
	}
	if !slices.Equal(sp.Refs(-3), all) || len(sp.Refs(1000)) != len(last) {
		t.Error("Refs bounds")
	}
}

func TestSplitBody(t *testing.T) {
	e := newEnv(t)
	sp := e.request(Input{Format: FormatClaude, Body: e.readInput("claude-request-text.in.json")}, Options{})
	b := sp.Body()
	if b.N != uint32(len(sp.Items)) || b.ListSHA != ListSHA(ItemIDs(sp.Items)) || b.Base != nil ||
		b.Format != FormatClaude || b.RawLen != sp.RawLen || len(b.System) != 2 || *b.Skeleton != *sp.Skeleton {
		t.Errorf("Body() = %+v", b)
	}
	if _, err := Marshal(b); err != nil {
		t.Errorf("Marshal(Body): %v", err)
	}
}

func TestSplitContextCanceled(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := newSplitter(ctx, e.m, fakeStripURL, Options{}).split(Input{Format: FormatClaude, Body: []byte(`{"messages":[]}`)}, false)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	// Cancelled while masking: the masker cancels after its first call.
	ctx, cancel = context.WithCancel(context.Background())
	m := cancelingMasker{inner: e.m, cancel: cancel}
	sp, err := newSplitter(ctx, m, fakeStripURL, Options{}).split(Input{Format: FormatClaude,
		Body: e.readInput("claude-request-text.in.json")}, false)
	if !errors.Is(err, context.Canceled) || sp != nil {
		t.Errorf("err = %v, split %v", err, sp != nil)
	}
}

type cancelingMasker struct {
	inner  stringMasker
	cancel func()
}

func (c cancelingMasker) String(s string) (string, mask.Hits) {
	c.cancel()
	return c.inner.String(s)
}

type mapMemo struct {
	mu   sync.Mutex
	m    map[wire.Hash]string
	gets int
	hits int
}

func (m *mapMemo) Get(k wire.Hash) (string, mask.Hits, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gets++
	s, ok := m.m[k]
	if ok {
		m.hits++
	}
	return s, nil, ok
}

func (m *mapMemo) Add(k wire.Hash, s string, _ mask.Hits) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m[k] = s
}

func TestMemo(t *testing.T) {
	e := newEnv(t)
	in := Input{Format: FormatClaude, Body: e.readInput("claude-request-media.in.json")}
	long := strings.Repeat("synthetic memo text ", 100)
	in.Body = bytes.Replace(in.Body, []byte("Describe the picture and the report."), []byte(long), 1)
	memo := &mapMemo{m: map[wire.Hash]string{}}
	a := e.request(in, Options{Memo: memo})
	b := e.request(in, Options{Memo: memo})
	c := e.request(in, Options{})
	if !bytes.Equal(dumpSplit(a), dumpSplit(c)) || !bytes.Equal(dumpSplit(b), dumpSplit(c)) {
		t.Error("memo changed the output")
	}
	if memo.gets == 0 || memo.hits == 0 {
		t.Errorf("memo gets %d hits %d", memo.gets, memo.hits)
	}
}

// TestExportedEntryPoints: the exported functions need a Masker and give the same split as the
// internal path (needs pkg/mask).
func TestExportedEntryPoints(t *testing.T) {
	ctx := context.Background()
	in := Input{Format: FormatClaude, Body: []byte(`{"messages":[]}`)}
	if _, err := SplitRequest(ctx, in, Options{}); err == nil {
		t.Error("SplitRequest without a Masker must fail")
	}
	if _, err := SplitResponse(ctx, in, Options{}); err == nil {
		t.Error("SplitResponse without a Masker must fail")
	}
	if _, err := SplitMessages(ctx, FormatClaude, []byte(`[]`), Options{}); err == nil {
		t.Error("SplitMessages without a Masker must fail")
	}
	if _, _, err := StripMediaURLs([]byte(`{}`), nil); err == nil {
		t.Error("StripMediaURLs without a Masker must fail")
	}
	m := realMasker(t)
	e := newEnv(t)
	e.m = m
	body := e.readInput("claude-request-tools.in.json")
	sp, err := SplitRequest(ctx, Input{Format: FormatClaude, Body: body}, Options{Masker: m})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dumpSplit(sp), dumpSplit(e.request(Input{Format: FormatClaude, Body: body}, Options{}))) {
		t.Error("exported SplitRequest differs from the internal path")
	}
	if _, err := SplitMessages(ctx, FormatGemini, []byte(`[]`), Options{Masker: m}); err == nil {
		t.Error("SplitMessages accepts only Claude and Responses")
	}
	if _, err := SplitMessages(ctx, FormatClaude, []byte(`{}`), Options{Masker: m}); err == nil {
		t.Error("SplitMessages needs an array")
	}
}
