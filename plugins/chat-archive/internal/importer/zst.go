package importer

import (
	"errors"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
)

// Codex compresses a rollout it has not written to for about a week in place: rollout-<ts>-<id>.jsonl
// becomes rollout-<ts>-<id>.jsonl.zst (zstd, level 3), and it restores the plain file when the thread is
// resumed. The importer reads a compressed rollout as its decompressed content under the plain file's key
// (fileRef.Zstd), so the archive sees one file however Codex stores it: offsets, line ordinals and
// head_sha256 are the decompressed content's. When both forms exist, Codex reads the plain one, and so does
// the importer (discover).

// zstSuffix is the suffix Codex adds to a rollout it compressed.
const zstSuffix = ".zst"

// zstMaxWindow bounds a compressed rollout's decoder window, and so its memory (Codex's level 3 uses a few
// MiB).
const zstMaxWindow = 128 << 20

// zstReader is a compressed rollout's decompressed stream; Close releases the decoder and the file.
type zstReader struct {
	d  *zstd.Decoder
	fh *os.File
}

// errZstDamaged is a compressed rollout that ends inside a zstd frame (cut short) or fails to decode.
var errZstDamaged = errors.New("importer: damaged zstd stream")

// Read returns the decompressed content. The decoder reports a stream cut off inside a frame as
// io.ErrUnexpectedEOF; it becomes errZstDamaged, so io.EOF and io.ErrUnexpectedEOF from io.ReadFull and
// io.CopyN over this reader always mean content that is shorter, never a damaged file.
func (z *zstReader) Read(p []byte) (int, error) {
	n, err := z.d.Read(p)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = errZstDamaged
	}
	return n, err
}

func (z *zstReader) Close() error {
	z.d.Close()
	return z.fh.Close()
}

// openContent opens a file's content: the file itself, or a compressed rollout's decompressed stream.
func openContent(f fileRef) (io.ReadCloser, error) {
	fh, err := os.Open(f.Path)
	if err != nil {
		return nil, err
	}
	if !f.Zstd {
		return fh, nil
	}
	d, err := zstd.NewReader(fh, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxWindow(zstMaxWindow))
	if err != nil {
		fh.Close()
		return nil, err
	}
	return &zstReader{d: d, fh: fh}, nil
}

// readContentAt fills buf with f's content at off. A compressed rollout is decompressed up to off.
func readContentAt(f fileRef, buf []byte, off int64) error {
	if !f.Zstd {
		fh, err := os.Open(f.Path)
		if err != nil {
			return err
		}
		defer fh.Close()
		_, err = fh.ReadAt(buf, off)
		return err
	}
	rc, err := openContent(f)
	if err != nil {
		return err
	}
	defer rc.Close()
	if _, err := io.CopyN(io.Discard, rc, off); err != nil {
		return err
	}
	_, err = io.ReadFull(rc, buf)
	return err
}

// contentPrefix returns up to n bytes from the start of f's content.
func contentPrefix(f fileRef, n int64) []byte {
	rc, err := openContent(f)
	if err != nil {
		return nil
	}
	defer rc.Close()
	buf := make([]byte, n)
	k, _ := io.ReadFull(rc, buf)
	return buf[:k]
}

// contentSize returns the length of f's content: a compressed rollout's decompressed length, or its own size
// when it fails to decompress.
func contentSize(f fileRef) int64 {
	rc, err := openContent(f)
	if err != nil {
		return f.Size
	}
	defer rc.Close()
	n, err := io.Copy(io.Discard, rc)
	if err != nil {
		return f.Size
	}
	return n
}
