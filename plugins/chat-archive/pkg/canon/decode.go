package canon

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// Bounds of decodeBody.
const (
	// maxCodings is the most content codings a body may name. Real clients and servers send one, rarely
	// two; a longer list is undecodable, so a crafted stack of codings costs no decoding work at all.
	maxCodings = 3
	// decodeChunk is the most decoded bytes one read asks a decoder for, so a long decode checks the
	// split's context often.
	decodeChunk = 256 << 10
	// minBrotliWindow is the largest Brotli window always accepted: lgwin 22, the encoders' default.
	// A larger window must fit the decoded-size cap (brotliWindow).
	minBrotliWindow = 4 << 20
)

// errStreamTrailing: a deflate stream ended before its body did, so the body is not that stream.
var errStreamTrailing = errors.New("canon: bytes after the end of the stream")

// codings returns the content codings of a Content-Encoding value in the order they were applied,
// lowercased, without empty and identity entries.
func codings(ce string) []string {
	var out []string
	for _, c := range strings.Split(ce, ",") {
		c = strings.ToLower(strings.TrimSpace(c))
		if c != "" && c != "identity" {
			out = append(out, c)
		}
	}
	return out
}

// decodeBody undoes the content codings of a body, last applied first: gzip and x-gzip, zstd, deflate
// (zlib-wrapped as RFC 9110 defines it, or raw) and br. flags names what was undone: gunzipped for
// gzip, content_decoded for the others. ok is false when a coding is unknown or its stream does not
// decode; the caller then reads the body as is only when it is a JSON object (a plain body under a
// stale header), else drops it as still encoded.
//
// Bounds: more than maxCodings codings is ErrUndecodableBody before anything is decoded. Each layer
// decodes at most maxOut bytes, and all layers together at most 2×maxOut, both ErrBodyBudget past
// it. ctx is checked between layers and between reads of at most decodeChunk bytes, and its error is
// returned as is, so the split's masking deadline and the capture drain bound a decode as they bound
// masking.
//
// gzip and zstd streams are recognized by their magic bytes; deflate and br have none, so a body that
// is valid JSON (any value: CPA hands the hooks a body it decoded itself with the original
// Content-Encoding still set) is never fed to them. Only whole valid JSON counts: a compressed stream
// can start with '{' or '[' (Brotli at quality 0 or 1 with lgwin 21 starts 09 5b, a raw deflate block
// with fixed codes can start 7b), and such a stream is decoded, never refused. A deflate stream must
// also end where the body ends: a raw block with fixed codes can end within a byte or two, so plain
// text starting with '[' or '{' "inflates" to garbage followed by the rest of the text.
func decodeBody(ctx context.Context, body []byte, ce string, maxOut int64) (out []byte, flags []string, ok bool, err error) {
	cs := codings(ce)
	if len(cs) == 0 {
		return body, nil, true, nil
	}
	if len(cs) > maxCodings {
		return nil, nil, false, fmt.Errorf("%w: %d content codings, at most %d", ErrUndecodableBody, len(cs), maxCodings)
	}
	budget := int64(math.MaxInt64)
	if maxOut <= math.MaxInt64/2 {
		budget = 2 * maxOut
	}
	out = body
	for i := len(cs) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return nil, nil, false, err
		}
		capOut := min(maxOut, budget)
		var dec []byte
		switch cs[i] {
		case "gzip", "x-gzip":
			if !isGzip(out) {
				return nil, nil, false, nil
			}
			dec, err = gunzip(ctx, out, capOut)
			flags = append(flags, wire.FlagGunzipped)
		case "zstd":
			if !isZstd(out) {
				return nil, nil, false, nil
			}
			dec, err = unzstd(ctx, out, capOut)
			flags = append(flags, wire.FlagContentDecoded)
		case "deflate":
			if isPlainJSON(out) {
				return nil, nil, false, nil
			}
			dec, err = inflate(ctx, out, capOut)
			flags = append(flags, wire.FlagContentDecoded)
		case "br":
			if isPlainJSON(out) {
				return nil, nil, false, nil
			}
			dec, err = unbrotli(ctx, out, capOut)
			flags = append(flags, wire.FlagContentDecoded)
		default:
			return nil, nil, false, nil
		}
		if cerr := ctx.Err(); err != nil && cerr != nil {
			return nil, nil, false, cerr
		}
		if errors.Is(err, ErrBodyBudget) {
			return nil, nil, false, ErrBodyBudget
		}
		if err != nil {
			return nil, nil, false, nil
		}
		budget -= int64(len(dec))
		out = dec
	}
	return out, flags, true, nil
}

// isZstd reports whether b starts with the zstd frame magic 28 b5 2f fd (RFC 8878).
func isZstd(b []byte) bool {
	return len(b) >= 4 && b[0] == 0x28 && b[1] == 0xb5 && b[2] == 0x2f && b[3] == 0xfd
}

// isPlainJSON reports whether b is one valid JSON value (surrounding whitespace allowed): a body that is
// plain already. A compressed stream that merely starts with '{' or '[' is not valid JSON, so it is
// decoded.
func isPlainJSON(b []byte) bool {
	return len(bytes.TrimSpace(b)) > 0 && json.Valid(b)
}

func unzstd(ctx context.Context, b []byte, maxOut int64) ([]byte, error) {
	zr, err := zstd.NewReader(bytes.NewReader(b), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(uint64(maxOut)+1))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out, err := readCapped(ctx, zr, maxOut, 64<<10)
	if errors.Is(err, zstd.ErrDecoderSizeExceeded) || errors.Is(err, zstd.ErrWindowSizeExceeded) {
		return nil, ErrBodyBudget // the frame declares more than maxOut: the decoder refuses before reading it
	}
	return out, err
}

// unbrotli decodes a br body. The decoder allocates its whole window before the first byte comes out,
// up to 16 MiB for lgwin 24 however short the stream, so a window over max(maxOut, minBrotliWindow)
// is ErrBodyBudget before anything is allocated, as zstd refuses a window over maxOut.
func unbrotli(ctx context.Context, b []byte, maxOut int64) ([]byte, error) {
	if w := brotliWindow(b); w > max(maxOut, minBrotliWindow) {
		return nil, ErrBodyBudget
	}
	return readCapped(ctx, brotli.NewReader(bytes.NewReader(b)), maxOut, 64<<10)
}

// brotliWindow returns the window size in bytes a Brotli stream header declares (RFC 7932 9.1: WBITS in
// the first 1, 4 or 7 bits), or 0 when there is no header or it is not an RFC 7932 one (the decoder
// then refuses the stream).
func brotliWindow(b []byte) int64 {
	if len(b) == 0 {
		return 0
	}
	c := b[0]
	if c&1 == 0 {
		return 1 << 16
	}
	if n := (c >> 1) & 7; n != 0 {
		return 1 << (17 + n)
	}
	switch m := (c >> 4) & 7; m {
	case 0:
		return 1 << 17
	case 1:
		return 0 // the large-window extension's marker, which the decoder does not accept
	default:
		return 1 << (8 + m)
	}
}

// inflate decodes a deflate body: zlib-wrapped (what Content-Encoding deflate means) or, as some
// servers send it, a raw deflate stream. Either stream must end exactly where the body ends
// (errStreamTrailing otherwise). compress/flate reads a bytes.Reader one byte at a time (io.ByteReader), so
// it never reads past the stream's end and the bytes left are the body's own.
func inflate(ctx context.Context, b []byte, maxOut int64) ([]byte, error) {
	if len(b) >= 2 && b[0]&0x0f == 8 && binary.BigEndian.Uint16(b[:2])%31 == 0 {
		rd := bytes.NewReader(b)
		if zr, err := zlib.NewReader(rd); err == nil {
			out, err := readCapped(ctx, zr, maxOut, 64<<10)
			switch {
			case errors.Is(err, ErrBodyBudget) || ctx.Err() != nil:
				return nil, err
			case err == nil && rd.Len() == 0:
				return out, nil
			}
		}
	}
	rd := bytes.NewReader(b)
	out, err := readCapped(ctx, flate.NewReader(rd), maxOut, 64<<10)
	if err != nil {
		return nil, err
	}
	if rd.Len() != 0 {
		return nil, errStreamTrailing
	}
	return out, nil
}

// gunzip decodes a gzip body of at most maxOut decoded bytes. A stream that does not decode is
// ErrUndecodableBody (its bytes are never stored, so no compressed plaintext reaches a blob); more than
// maxOut decoded bytes is ErrBodyBudget, found after reading at most maxOut+1 bytes; ctx's error is
// returned as is.
func gunzip(ctx context.Context, b []byte, maxOut int64) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("%w: gzip: %v", ErrUndecodableBody, err)
	}
	hint := int64(64 << 10)
	if len(b) >= 4 {
		if isize := int64(binary.LittleEndian.Uint32(b[len(b)-4:])); isize <= maxOut {
			hint = isize
		}
	}
	out, err := readCapped(ctx, zr, maxOut, hint)
	if err != nil && !errors.Is(err, ErrBodyBudget) && ctx.Err() == nil {
		return nil, fmt.Errorf("%w: gzip: %v", ErrUndecodableBody, err)
	}
	return out, err
}

// readCapped reads r to its end into a buffer that starts at hint bytes and grows, and returns
// ErrBodyBudget once more than maxOut bytes were read. It asks r for at most decodeChunk bytes a read
// and returns ctx.Err() as soon as ctx ends, checked before every read.
func readCapped(ctx context.Context, r io.Reader, maxOut, hint int64) ([]byte, error) {
	buf := make([]byte, 0, min(hint, maxOut)+1)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(buf) == cap(buf) {
			next := min(max(2*int64(cap(buf)), 64<<10), maxOut+1)
			if next <= int64(cap(buf)) {
				return nil, ErrBodyBudget
			}
			buf = append(make([]byte, 0, next), buf...)
		}
		n, err := r.Read(buf[len(buf):min(cap(buf), len(buf)+decodeChunk)])
		buf = buf[:len(buf)+n]
		if int64(len(buf)) > maxOut {
			return nil, ErrBodyBudget
		}
		if err == io.EOF {
			return buf, nil
		}
		if err != nil {
			return nil, err
		}
	}
}
