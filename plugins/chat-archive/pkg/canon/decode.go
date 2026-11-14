package canon

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

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
// (zlib-wrapped as RFC 9110 defines it, or raw) and br, each capped at maxOut decoded bytes
// (ErrBodyBudget past it). flags names what was undone: gunzipped for gzip, content_decoded for the
// others. ok is false when a coding is unknown or its stream does not decode; the caller then reads
// the body as is only when it is a JSON object (a plain body under a stale header), else drops it as
// still encoded. gzip and zstd streams are recognized by their magic bytes; deflate and br have none,
// so a body that is a valid JSON object is never fed to them (CPA hands the hooks a body it decoded
// itself with the original Content-Encoding still set). Only a whole valid object counts: a compressed
// stream can start with '{' or '[' (Brotli at quality 0 or 1 with lgwin 21 starts 09 5b, a raw deflate
// block with fixed codes can start 7b), and such a stream is decoded, never refused.
func decodeBody(body []byte, ce string, maxOut int64) (out []byte, flags []string, ok bool, err error) {
	cs := codings(ce)
	if len(cs) == 0 {
		return body, nil, true, nil
	}
	out = body
	for i := len(cs) - 1; i >= 0; i-- {
		var dec []byte
		switch cs[i] {
		case "gzip", "x-gzip":
			if !isGzip(out) {
				return nil, nil, false, nil
			}
			dec, err = gunzip(out, maxOut)
			flags = append(flags, wire.FlagGunzipped)
		case "zstd":
			if !isZstd(out) {
				return nil, nil, false, nil
			}
			dec, err = unzstd(out, maxOut)
			flags = append(flags, wire.FlagContentDecoded)
		case "deflate":
			if isJSONObject(out) {
				return nil, nil, false, nil
			}
			dec, err = inflate(out, maxOut)
			flags = append(flags, wire.FlagContentDecoded)
		case "br":
			if isJSONObject(out) {
				return nil, nil, false, nil
			}
			dec, err = readCapped(brotli.NewReader(bytes.NewReader(out)), maxOut, 64<<10)
			flags = append(flags, wire.FlagContentDecoded)
		default:
			return nil, nil, false, nil
		}
		if errors.Is(err, ErrBodyBudget) {
			return nil, nil, false, ErrBodyBudget
		}
		if err != nil {
			return nil, nil, false, nil
		}
		out = dec
	}
	return out, flags, true, nil
}

// isZstd reports whether b starts with the zstd frame magic 28 b5 2f fd (RFC 8878).
func isZstd(b []byte) bool {
	return len(b) >= 4 && b[0] == 0x28 && b[1] == 0xb5 && b[2] == 0x2f && b[3] == 0xfd
}

// isJSONObject reports whether b is one valid JSON object (surrounding whitespace allowed): a body that
// is plain already. A compressed stream that merely starts with '{' is not valid JSON, so it is decoded.
func isJSONObject(b []byte) bool {
	t := bytes.TrimLeft(b, " \t\r\n")
	return len(t) > 0 && t[0] == '{' && json.Valid(t)
}

func unzstd(b []byte, maxOut int64) ([]byte, error) {
	zr, err := zstd.NewReader(bytes.NewReader(b), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(uint64(maxOut)+1))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out, err := readCapped(zr, maxOut, 64<<10)
	if errors.Is(err, zstd.ErrDecoderSizeExceeded) || errors.Is(err, zstd.ErrWindowSizeExceeded) {
		return nil, ErrBodyBudget // the frame declares more than maxOut: the decoder refuses before reading it
	}
	return out, err
}

// inflate decodes a deflate body: zlib-wrapped (what Content-Encoding deflate means) or, as some
// servers send it, a raw deflate stream.
func inflate(b []byte, maxOut int64) ([]byte, error) {
	if len(b) >= 2 && b[0]&0x0f == 8 && binary.BigEndian.Uint16(b[:2])%31 == 0 {
		if zr, err := zlib.NewReader(bytes.NewReader(b)); err == nil {
			out, err := readCapped(zr, maxOut, 64<<10)
			if err == nil || errors.Is(err, ErrBodyBudget) {
				return out, err
			}
		}
	}
	return readCapped(flate.NewReader(bytes.NewReader(b)), maxOut, 64<<10)
}

// gunzip decodes a gzip body of at most maxOut decoded bytes. A stream that does not decode is
// ErrUndecodableBody (its bytes are never stored, so no compressed plaintext reaches a blob); more than
// maxOut decoded bytes is ErrBodyBudget, found after reading at most maxOut+1 bytes.
func gunzip(b []byte, maxOut int64) ([]byte, error) {
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
	out, err := readCapped(zr, maxOut, hint)
	if err != nil && !errors.Is(err, ErrBodyBudget) {
		return nil, fmt.Errorf("%w: gzip: %v", ErrUndecodableBody, err)
	}
	return out, err
}

// readCapped reads r to its end into a buffer that starts at hint bytes and grows, and returns
// ErrBodyBudget once more than maxOut bytes were read.
func readCapped(r io.Reader, maxOut, hint int64) ([]byte, error) {
	buf := make([]byte, 0, min(hint, maxOut)+1)
	for {
		if len(buf) == cap(buf) {
			next := min(max(2*int64(cap(buf)), 64<<10), maxOut+1)
			if next <= int64(cap(buf)) {
				return nil, ErrBodyBudget
			}
			buf = append(make([]byte, 0, next), buf...)
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
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
