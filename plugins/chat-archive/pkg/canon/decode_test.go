package canon

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andybalholm/brotli"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// prettyJSON is a pretty-printed JSON array of n objects, as a server or a client writes a body by hand.
func prettyJSON(rng *rand.Rand, n int) string {
	var sb strings.Builder
	sb.WriteString("[\n")
	for i := range n {
		fmt.Fprintf(&sb, "  {\n    \"candidates\": [\n      {\n        \"index\": %d,\n        \"text\": \"synthetic %x\"\n      }\n    ]\n  }", i, rng.Uint64())
		if i < n-1 {
			sb.WriteString(",")
		}
		sb.WriteString("\n")
	}
	sb.WriteString("]\n")
	return sb.String()
}

// TestDeflateLabelOnPlainBody: a plain body under a stale deflate or br label is never "decoded". A
// pretty-printed array starts 5b 0a 20 20 7b, which raw deflate reads as a final block with fixed codes
// that ends after a symbol or two: it used to inflate to a byte of garbage, stored as an attachment
// flagged content_decoded with no drop counted. Any valid JSON value is plain already; an invalid one
// (a truncated object) inflates only when the stream ends where the body ends, so it is undecodable.
// Neither ever becomes content_decoded.
func TestDeflateLabelOnPlainBody(t *testing.T) {
	e := newEnv(t)
	rng := rand.New(rand.NewPCG(7, 11))
	var bodies []string
	for n := 1; n <= 300; n++ {
		p := prettyJSON(rng, n%9+1)
		bodies = append(bodies, p, p[:len(p)/2], strings.Replace(p, "[\n", "{\n  \"id\": \"resp-1\",\n  \"x\": [\n", 1)[:len(p)/3])
	}
	bodies = append(bodies, "{\n  \"id\": \"msg_1\",\n  \"content\": \"abc", "[\n  1,\n  2\n", "{\n  \"a\": 1\n}\ntrailing text")
	for _, ce := range []string{"deflate", "br"} {
		for _, b := range bodies {
			for _, response := range []bool{false, true} {
				sp, err := e.splitter(Options{}).split(Input{Format: FormatGemini, Body: []byte(b), ContentEncoding: ce}, response)
				if err == nil && slices.Contains(sp.Flags, wire.FlagContentDecoded) {
					t.Fatalf("%s: plain body %q was decoded: flags %v, %d pieces", ce, b[:min(len(b), 24)], sp.Flags, len(sp.Pieces))
				}
				if err != nil && !errors.Is(err, ErrUndecodableBody) {
					t.Fatalf("%s: plain body %q: err %v", ce, b[:min(len(b), 24)], err)
				}
				if err == nil && !slices.Contains(sp.Flags, wire.FlagEncodingIgnored) {
					t.Fatalf("%s: body %q read without encoding_ignored: %v", ce, b[:min(len(b), 24)], sp.Flags)
				}
			}
		}
	}
	// The reviewer's case through the public entry point: dropped as undecodable (parse_error), as
	// 0.1.1 did.
	gem := "[\n  {\n    \"candidates\": [{\"content\": {\"parts\": [{\"text\": \"hi\"}]}}]\n  }\n]"
	if sp, err := SplitResponse(context.Background(), Input{Format: FormatGemini, Body: []byte(gem), ContentEncoding: "deflate"}, Options{Masker: realMasker(t)}); !errors.Is(err, ErrUndecodableBody) {
		t.Fatalf("pretty array under deflate: err %v split %+v", err, sp)
	}
}

// TestDeflateRealStreams: real deflate bodies decode whole, raw and zlib-wrapped, at every level,
// empty, flushed mid-stream and over several blocks; the same streams followed by any extra bytes do
// not.
func TestDeflateRealStreams(t *testing.T) {
	src := []byte(strings.Repeat(`{"role":"user","content":"synthetic deflate text"}`, 3000))
	var bodies [][]byte
	for _, lvl := range []int{flate.HuffmanOnly, flate.NoCompression, flate.BestSpeed, flate.DefaultCompression, flate.BestCompression} {
		for _, n := range []int{0, 1, 300, 70 << 10, len(src)} {
			var raw, zl bytes.Buffer
			w, _ := flate.NewWriter(&raw, lvl)
			zw, _ := zlib.NewWriterLevel(&zl, lvl)
			for _, x := range []interface {
				Write([]byte) (int, error)
				Flush() error
				Close() error
			}{w, zw} {
				x.Write(src[:n/2])
				x.Flush()
				x.Write(src[n/2 : n])
				x.Close()
			}
			bodies = append(bodies, raw.Bytes(), zl.Bytes())
			if got, err := inflate(context.Background(), raw.Bytes(), 1<<30); err != nil || !bytes.Equal(got, src[:n]) {
				t.Fatalf("raw level %d, %d bytes: %v", lvl, n, err)
			}
			if got, err := inflate(context.Background(), zl.Bytes(), 1<<30); err != nil || !bytes.Equal(got, src[:n]) {
				t.Fatalf("zlib level %d, %d bytes: %v", lvl, n, err)
			}
		}
	}
	for _, b := range bodies {
		for _, extra := range []string{"x", "\x00", "\n", " trailing"} {
			if _, err := inflate(context.Background(), append(slices.Clone(b), extra...), 1<<30); err == nil {
				t.Fatalf("a %d-byte stream with %q after it decoded", len(b), extra)
			}
		}
	}
	e := newEnv(t)
	plain := e.readInput("claude-response.in.json")
	var raw bytes.Buffer
	w, _ := flate.NewWriter(&raw, flate.BestCompression)
	w.Write(plain)
	w.Close()
	a := e.response(Input{Format: FormatClaude, Body: plain}, Options{})
	sp := e.response(Input{Format: FormatClaude, Body: raw.Bytes(), ContentEncoding: "deflate"}, Options{})
	if !slices.Equal(ItemIDs(a.Items), ItemIDs(sp.Items)) || !slices.Contains(sp.Flags, wire.FlagContentDecoded) {
		t.Fatalf("raw deflate body split differently: flags %v", sp.Flags)
	}
	if _, err := e.splitter(Options{}).split(Input{Format: FormatClaude, Body: append(raw.Bytes(), "tail"...), ContentEncoding: "deflate"}, true); !errors.Is(err, ErrUndecodableBody) {
		t.Fatalf("raw deflate with trailing bytes: err %v", err)
	}
}

// gzipLevel compresses b with one gzip member at level.
func gzipLevel(b []byte, level int) []byte {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, level)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}

// TestCodingStackBounds: at most three codings are undone; a longer list is undecodable before any
// decoding (even over a plain JSON object), so a stack of stored-block gzip wrappers, each layer as
// large as the last, costs nothing. All layers together decode at most 2×MaxDecoded bytes.
func TestCodingStackBounds(t *testing.T) {
	e := newEnv(t)
	plain := e.readInput("claude-response.in.json")
	a := e.response(Input{Format: FormatClaude, Body: plain}, Options{})
	stack := func(b []byte, n, level int) []byte {
		for range n {
			b = gzipLevel(b, level)
		}
		return b
	}
	three := stack(plain, 3, gzip.DefaultCompression)
	sp := e.response(Input{Format: FormatClaude, Body: three, ContentEncoding: "gzip, gzip, gzip"}, Options{})
	if !slices.Equal(ItemIDs(a.Items), ItemIDs(sp.Items)) || !slices.Contains(sp.Flags, wire.FlagGunzipped) {
		t.Fatalf("three gzip layers: flags %v", sp.Flags)
	}
	deep := stack(bytes.Repeat([]byte("synthetic stored block text "), 2500), 60, gzip.NoCompression)
	for _, c := range []struct {
		name, ce string
		body     []byte
	}{
		{"four layers", "gzip, gzip, gzip, gzip", stack(plain, 4, gzip.DefaultCompression)},
		{"sixty stored layers", strings.TrimSuffix(strings.Repeat("gzip, ", 60), ", "), deep},
		{"plain object under four codings", "br, br, br, br", plain},
		{"repeated field lines", "zstd, identity, br, gzip, deflate", plain},
	} {
		start := time.Now()
		if _, err := e.splitter(Options{}).split(Input{Format: FormatClaude, Body: c.body, ContentEncoding: c.ce}, true); !errors.Is(err, ErrUndecodableBody) {
			t.Errorf("%s: err %v, want ErrUndecodableBody", c.name, err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s: refused after %v", c.name, d)
		}
	}
	// Three stored layers of S bytes each: every layer fits MaxDecoded, all three together do not fit
	// 2×MaxDecoded.
	big := bytes.Repeat([]byte(`{"k":"synthetic"},`), 20000)
	stored := stack(big, 3, gzip.NoCompression)
	maxDec := int64(len(big)) + 1024
	if _, _, ok, err := decodeBody(context.Background(), stored, "gzip, gzip, gzip", maxDec); !errors.Is(err, ErrBodyBudget) || ok {
		t.Fatalf("three layers over the cumulative budget: ok %v err %v", ok, err)
	}
	if out, _, ok, err := decodeBody(context.Background(), stored, "gzip, gzip, gzip", 2*maxDec); err != nil || !ok || !bytes.Equal(out, big) {
		t.Fatalf("three layers within the cumulative budget: ok %v err %v", ok, err)
	}
	if out, _, ok, err := decodeBody(context.Background(), stack(big, 1, gzip.NoCompression), "gzip", maxDec); err != nil || !ok || !bytes.Equal(out, big) {
		t.Fatalf("one layer: ok %v err %v", ok, err)
	}
}

// flipCtx answers Err() with nil for its first left calls, then with err: a deadline or a drain that
// ends in the middle of a decode, at a chosen check.
type flipCtx struct {
	context.Context
	left  atomic.Int64
	calls atomic.Int64
	err   error
}

func newFlipCtx(left int64, err error) *flipCtx {
	c := &flipCtx{Context: context.Background(), err: err}
	c.left.Store(left)
	return c
}

func (c *flipCtx) Err() error {
	c.calls.Add(1)
	if c.left.Add(-1) < 0 {
		return c.err
	}
	return nil
}

// TestDecodeCancellation: a decode checks the split's context between layers and between reads, and
// returns its error as is (DeadlineExceeded becomes mask_timeout in the plugin, a drain's Canceled
// stops the worker), however far into a layer it got. A context that never ends decodes the body.
func TestDecodeCancellation(t *testing.T) {
	src := bytes.Repeat([]byte("synthetic decoded text, "), 200_000) // about 4.8 MB: many reads
	br := func(b []byte) []byte {
		var buf bytes.Buffer
		w := brotli.NewWriter(&buf)
		w.Write(b)
		w.Close()
		return buf.Bytes()
	}
	var raw bytes.Buffer
	fw, _ := flate.NewWriter(&raw, flate.BestSpeed)
	fw.Write(src)
	fw.Close()
	for _, c := range []struct {
		name, ce string
		body     []byte
	}{
		{"gzip", "gzip", gzipBytes(src)},
		{"gzip then br", "gzip, br", br(gzipBytes(src))},
		{"deflate", "deflate", raw.Bytes()},
	} {
		full := newFlipCtx(1<<40, context.Canceled)
		out, _, ok, err := decodeBody(full, c.body, c.ce, 1<<30)
		if err != nil || !ok || !bytes.Equal(out, src) {
			t.Fatalf("%s: ok %v err %v", c.name, ok, err)
		}
		checks := full.calls.Load()
		if checks < int64(len(src)/decodeChunk) {
			t.Fatalf("%s: %d context checks for %d decoded bytes", c.name, checks, len(src))
		}
		// The last check follows the last layer's decode; one before it is the last read's.
		for _, k := range []int64{0, 1, 2, checks / 2, checks - 2} {
			for _, want := range []error{context.Canceled, context.DeadlineExceeded} {
				ctx := newFlipCtx(k, want)
				if _, _, ok, err := decodeBody(ctx, c.body, c.ce, 1<<30); !errors.Is(err, want) || ok {
					t.Fatalf("%s: context ending at check %d: ok %v err %v, want %v", c.name, k, ok, err, want)
				}
				// The layer and decodeBody read the error once more each on the way out; no read follows.
				if n := ctx.calls.Load(); n > k+3 {
					t.Fatalf("%s: %d checks after the context ended at check %d", c.name, n-k-1, k)
				}
			}
		}
	}
	// Through the split: the deadline passes through the decode as DeadlineExceeded.
	e := newEnv(t)
	ctx := newFlipCtx(3, context.DeadlineExceeded)
	if _, err := newSplitter(ctx, e.m, fakeStripURL, Options{}).split(Input{Format: FormatClaude, Body: gzipBytes(src), ContentEncoding: "gzip"}, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("split: err %v", err)
	}
}

// TestBrotliWindowBound: the Brotli decoder allocates its whole window up front, 16 MiB for lgwin 24
// however short the stream. A window over max(MaxDecoded, 4 MiB) is ErrBodyBudget before the decoder
// starts; lgwin 22 and below always decodes, and a larger window decodes when MaxDecoded covers it.
func TestBrotliWindowBound(t *testing.T) {
	plain := []byte(`{"messages":[{"role":"user","content":"synthetic brotli window"}]}`)
	enc := func(lgwin int) []byte {
		var buf bytes.Buffer
		w := brotli.NewWriterOptions(&buf, brotli.WriterOptions{Quality: 5, LGWin: lgwin})
		w.Write(plain)
		w.Close()
		return buf.Bytes()
	}
	for lg := 10; lg <= 24; lg++ {
		if got := brotliWindow(enc(lg)); got != 1<<lg {
			t.Fatalf("lgwin %d: window %d", lg, got)
		}
	}
	for _, c := range []struct {
		lgwin  int
		maxDec int64
		budget bool
	}{
		{24, 1 << 20, true},
		{23, 1 << 20, true},
		{24, 16<<20 - 1, true},
		{22, 1 << 10, false},
		{24, 16 << 20, false},
		{18, 1 << 10, false},
	} {
		out, _, ok, err := decodeBody(context.Background(), enc(c.lgwin), "br", c.maxDec)
		switch {
		case c.budget && !errors.Is(err, ErrBodyBudget):
			t.Errorf("lgwin %d, MaxDecoded %d: ok %v err %v, want ErrBodyBudget", c.lgwin, c.maxDec, ok, err)
		case !c.budget && (err != nil || !ok || !bytes.Equal(out, plain)):
			t.Errorf("lgwin %d, MaxDecoded %d: ok %v err %v", c.lgwin, c.maxDec, ok, err)
		}
	}
}
