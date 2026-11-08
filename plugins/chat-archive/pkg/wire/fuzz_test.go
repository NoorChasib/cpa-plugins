package wire_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// fuzzCap is the decoded-bytes cap the fuzz target passes to ReadCAB1 (the server passes
// limits.commit_raw, 32 MiB; a smaller cap keeps iterations fast and still exercises the cap).
const fuzzCap = 1 << 20

// serverDecoder opens r with the server's zstd options (8 MiB window, 256 MiB decoder memory). The codec itself is framing only;
// this is how a server or the fake archive feeds it.
func serverDecoder(r io.Reader) (*zstd.Decoder, error) {
	return zstd.NewReader(r,
		zstd.WithDecoderMaxWindow(8<<20),
		zstd.WithDecoderMaxMemory(256<<20),
		zstd.WithDecoderConcurrency(1))
}

// fuzzDecoders reuses server decoders across fuzz executions: a fresh decoder per input costs about
// 10 ms of allocation, which stalls input minimization for the whole -fuzzminimizetime.
var fuzzDecoders = sync.Pool{New: func() any {
	d, err := serverDecoder(nil)
	if err != nil {
		panic(err)
	}
	return d
}}

func zstdCompress(t testing.TB, b []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)), zstd.WithWindowSize(8<<20), zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	return enc.EncodeAll(b, nil)
}

// bombStream is a valid CAB1 prefix followed by n 4 KiB envelope records and no end record: a few
// KiB of zstd that decodes to far more than any cap. Large records keep one capped decode to a few
// hundred allocations, so fuzzing (and minimizing) the seed stays fast.
func bombStream(n int) []byte {
	rec := cab1Rec(wire.RecEnvelope, bytes.Repeat([]byte("a"), 4096))
	return append([]byte(wire.CAB1Magic), bytes.Repeat(rec, n)...)
}

// FuzzReadCAB1 feeds arbitrary bytes to ReadCAB1, raw or through the server's zstd decoder. Seeds:
// every golden stream (valid, truncated, bad lengths, unknown types, tombstones, hash mismatch), the
// same streams compressed, and a zstd bomb. Properties: no panic; raw-input errors are always
// ErrCAB1Framing or ErrCAB1TooLarge; a decoded batch respects the framing rules (piece hashes match,
// kinds valid, content types only on attachments, tombstones <= 1 KiB), fits the cap, and survives
// WriteCAB1 then ReadCAB1 unchanged.
func FuzzReadCAB1(f *testing.F) {
	entries, err := os.ReadDir(filepath.Join(goldenDir, "cab1"))
	if err != nil {
		f.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".cab1" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(goldenDir, "cab1", e.Name()))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b, false)
		f.Add(zstdCompress(f, b), true)
	}
	f.Add(zstdCompress(f, bombStream(4000)), true)
	f.Add([]byte{}, false)
	f.Add([]byte{}, true)

	f.Fuzz(func(t *testing.T, data []byte, compressed bool) {
		var r io.Reader = bytes.NewReader(data)
		if compressed {
			dec := fuzzDecoders.Get().(*zstd.Decoder)
			if err := dec.Reset(r); err != nil {
				fuzzDecoders.Put(dec)
				return // not a zstd stream
			}
			defer fuzzDecoders.Put(dec)
			r = dec
		}
		b, err := wire.ReadCAB1(r, fuzzCap)
		if err != nil {
			if !compressed && !errors.Is(err, wire.ErrCAB1Framing) && !errors.Is(err, wire.ErrCAB1TooLarge) {
				t.Fatalf("raw input: unclassified error %v", err)
			}
			return
		}
		size := len(wire.CAB1Magic) + 5
		for _, p := range b.Pieces {
			if sha256.Sum256(p.Raw) != p.Hash {
				t.Fatalf("piece %s: hash mismatch accepted", p.Hash)
			}
			if p.Kind > wire.PieceAttachment || len(p.ContentType) > 255 || (p.ContentType != "" && p.Kind != wire.PieceAttachment) {
				t.Fatalf("piece %s: kind %d content type %q accepted", p.Hash, p.Kind, p.ContentType)
			}
			size += 5 + 34 + len(p.ContentType) + len(p.Raw)
		}
		for _, e := range b.Envelopes {
			size += 5 + len(e)
		}
		for _, tb := range b.Tombstones {
			if len(tb) > wire.MaxTombstoneJSON {
				t.Fatalf("tombstone of %d bytes accepted", len(tb))
			}
			size += 5 + len(tb)
		}
		if size > fuzzCap {
			t.Fatalf("decoded %d bytes past the %d cap", size, fuzzCap)
		}
		var buf bytes.Buffer
		if err := wire.WriteCAB1(&buf, b); err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		if buf.Len() != size {
			t.Fatalf("re-encoded %d bytes, accounted %d", buf.Len(), size)
		}
		if n := wire.CAB1Size(b); n != size {
			t.Fatalf("CAB1Size = %d, encoded %d", n, size)
		}
		again, err := wire.ReadCAB1(&buf, fuzzCap)
		if err != nil {
			t.Fatalf("re-decode: %v", err)
		}
		if !reflect.DeepEqual(normalize(again), normalize(b)) {
			t.Fatalf("round trip changed the batch")
		}
	})
}

// normalize maps empty slices to nil so a decoded empty record equals a re-decoded one.
func normalize(b wire.Batch) wire.Batch {
	fix := func(x []byte) []byte {
		if len(x) == 0 {
			return nil
		}
		return x
	}
	out := wire.Batch{}
	for _, p := range b.Pieces {
		p.Raw = fix(p.Raw)
		out.Pieces = append(out.Pieces, p)
	}
	for _, e := range b.Envelopes {
		out.Envelopes = append(out.Envelopes, fix(e))
	}
	for _, tb := range b.Tombstones {
		out.Tombstones = append(out.Tombstones, fix(tb))
	}
	return out
}

// A zstd bomb (16 MB of records in a few KiB) stops at the cap with ErrCAB1TooLarge, having read at
// most cap+1 decoded bytes.
func TestCAB1ZstdBomb(t *testing.T) {
	bomb := zstdCompress(t, bombStream(4000))
	if len(bomb) > 64<<10 {
		t.Fatalf("bomb is %d bytes compressed", len(bomb))
	}
	dec, err := serverDecoder(bytes.NewReader(bomb))
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	cr := &countingReader{r: dec}
	_, err = wire.ReadCAB1(cr, fuzzCap)
	if !errors.Is(err, wire.ErrCAB1TooLarge) {
		t.Fatalf("err = %v, want ErrCAB1TooLarge", err)
	}
	if cr.n > fuzzCap+1 {
		t.Fatalf("read %d decoded bytes for a %d cap", cr.n, fuzzCap)
	}
}

// A frame whose window exceeds 8 MiB is refused by the server decoder options.
func TestCAB1ZstdWindowLimit(t *testing.T) {
	enc, err := zstd.NewWriter(nil, zstd.WithWindowSize(32<<20), zstd.WithEncoderConcurrency(1), zstd.WithSingleSegment(false))
	if err != nil {
		t.Fatal(err)
	}
	payload := append([]byte(wire.CAB1Magic), bytes.Repeat([]byte{1}, 20<<20)...)
	frame := enc.EncodeAll(payload, nil)
	enc.Close()
	dec, err := serverDecoder(bytes.NewReader(frame))
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	if _, err := wire.ReadCAB1(dec, 64<<20); err == nil || errors.Is(err, wire.ErrCAB1TooLarge) {
		t.Fatalf("err = %v, want the decoder's window error", err)
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}
