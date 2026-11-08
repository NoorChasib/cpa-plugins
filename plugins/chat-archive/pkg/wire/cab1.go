package wire

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// CAB1 framing. Inside one zstd stream: the magic, then records
// {u8 type, u32 big-endian length, bytes}. Pieces precede the envelopes that reference them.
const (
	CAB1Magic = "CAB1"

	RecEnvelope  byte = 1   // canon/v1 JSON of Envelope
	RecPiece     byte = 2   // 32-byte hash, 1-byte PieceKind, 1-byte content-type length + content type, raw bytes
	RecTombstone byte = 3   // canon/v1 JSON of Tombstone, <= MaxTombstoneJSON
	RecEnd       byte = 255 // length 0

	// MaxTombstoneJSON is the largest tombstone record.
	MaxTombstoneJSON = 1 << 10
)

// ContentTypeBatchV1 is the Content-Type of POST /v1/ingest/commit.
const ContentTypeBatchV1 = "application/vnd.chat-archive.batch.v1"

// BatchPiece is one CAB1 piece record.
type BatchPiece struct {
	Hash        Hash      // sha256 of Raw
	Kind        PieceKind // json | text | attachment
	ContentType string    // attachments only (at most 255 bytes), else empty
	Raw         []byte    // the piece's exact bytes (uncompressed)
}

// Batch is the content of one CAB1 stream. Envelopes and tombstones are carried as their canon/v1
// JSON bytes; the codec does framing only and never re-encodes them.
type Batch struct {
	Pieces     []BatchPiece
	Envelopes  [][]byte // record type 1, in the order written
	Tombstones [][]byte // record type 3, in the order written
}

// CAB1 decode errors. ReadCAB1 wraps one of them; the server answers 400 for ErrCAB1Framing and 413
// for ErrCAB1TooLarge.
var (
	ErrCAB1Framing  = errors.New("wire: CAB1 framing error")
	ErrCAB1TooLarge = errors.New("wire: CAB1 stream too large")
)

// CAB1RecordOverhead is what ReadCAB1 charges against maxBytes for each record other than the end
// record, on top of its bytes: about what decoding one record costs in bookkeeping (a 24-byte slice
// header or an 80-byte BatchPiece, doubled by slice growth). A producer sizing a batch against
// limits.commit_raw counts it per piece, envelope and tombstone.
const CAB1RecordOverhead = 128

const (
	recHeaderLen   = 1 + 4           // u8 type, u32 length
	pieceHeaderLen = len(Hash{}) + 2 // hash, kind byte, content-type length byte
)

// WriteCAB1 writes b to w as an uncompressed CAB1 stream: the magic, every piece, every envelope,
// every tombstone, then the end record. Compression is the caller's (pkg/client).
//
// WriteCAB1 checks only what the framing itself needs: a valid piece kind, a content type of at most
// 255 bytes on attachments and none on other kinds, a tombstone of at most MaxTombstoneJSON bytes,
// and records that fit a u32 length. It does not recompute piece hashes or validate JSON; ReadCAB1
// does the former.
func WriteCAB1(w io.Writer, b Batch) error {
	bw := bufio.NewWriterSize(w, 64<<10)
	if _, err := bw.WriteString(CAB1Magic); err != nil {
		return err
	}
	var hdr [recHeaderLen]byte
	writeHeader := func(typ byte, n int) error {
		if uint64(n) > math.MaxUint32 {
			return fmt.Errorf("wire: CAB1 record of %d bytes exceeds the u32 length", n)
		}
		hdr[0] = typ
		binary.BigEndian.PutUint32(hdr[1:], uint32(n))
		_, err := bw.Write(hdr[:])
		return err
	}
	for i, p := range b.Pieces {
		if !p.Kind.valid() {
			return fmt.Errorf("wire: CAB1 piece %d: invalid kind %d", i, p.Kind)
		}
		if len(p.ContentType) > 255 {
			return fmt.Errorf("wire: CAB1 piece %d: content type longer than 255 bytes", i)
		}
		if p.ContentType != "" && p.Kind != PieceAttachment {
			return fmt.Errorf("wire: CAB1 piece %d: content type on a %s piece", i, p.Kind)
		}
		if err := writeHeader(RecPiece, pieceHeaderLen+len(p.ContentType)+len(p.Raw)); err != nil {
			return err
		}
		if _, err := bw.Write(p.Hash[:]); err != nil {
			return err
		}
		if err := bw.WriteByte(byte(p.Kind)); err != nil {
			return err
		}
		if err := bw.WriteByte(byte(len(p.ContentType))); err != nil {
			return err
		}
		if _, err := bw.WriteString(p.ContentType); err != nil {
			return err
		}
		if _, err := bw.Write(p.Raw); err != nil {
			return err
		}
	}
	for _, e := range b.Envelopes {
		if err := writeHeader(RecEnvelope, len(e)); err != nil {
			return err
		}
		if _, err := bw.Write(e); err != nil {
			return err
		}
	}
	for i, t := range b.Tombstones {
		if len(t) > MaxTombstoneJSON {
			return fmt.Errorf("wire: CAB1 tombstone %d: %d bytes exceeds %d", i, len(t), MaxTombstoneJSON)
		}
		if err := writeHeader(RecTombstone, len(t)); err != nil {
			return err
		}
		if _, err := bw.Write(t); err != nil {
			return err
		}
	}
	if err := writeHeader(RecEnd, 0); err != nil {
		return err
	}
	return bw.Flush()
}

// CAB1Size returns the number of bytes WriteCAB1 writes for b when it succeeds: the magic, a 5-byte
// header per record, every record's bytes and the end record. A producer pre-sizes its buffer with it,
// so encoding a commit does not copy the batch again each time the buffer doubles.
func CAB1Size(b Batch) int {
	n := len(CAB1Magic) + recHeaderLen // magic and the end record
	for _, p := range b.Pieces {
		n += recHeaderLen + pieceHeaderLen + len(p.ContentType) + len(p.Raw)
	}
	for _, e := range b.Envelopes {
		n += recHeaderLen + len(e)
	}
	for _, t := range b.Tombstones {
		n += recHeaderLen + len(t)
	}
	return n
}

// ReadCAB1 decodes an uncompressed CAB1 stream. maxBytes caps the decoded stream
// (limits.commit_raw); exceeding it wraps ErrCAB1TooLarge, and so does a record whose declared length
// would carry the stream past maxBytes. A bad magic, an unknown record type, a truncated or malformed
// record (a piece with an invalid kind, a content type on a non-attachment, or bytes whose sha256 is
// not the record's hash), a tombstone over MaxTombstoneJSON, an end record with a non-zero length,
// bytes after the end record or a missing end record wraps ErrCAB1Framing. JSON inside records is not
// validated here. Read errors of r other than io.EOF are returned as they are.
//
// Memory follows the bytes actually read, never a declared length, so a truncated stream that
// declares a large record allocates only what it carries. Every record other than the end record is
// also charged CAB1RecordOverhead bytes against maxBytes for its decoded bookkeeping (slice headers,
// BatchPiece), so a stream of tiny records cannot decode to many times maxBytes of memory: decoded
// memory stays within about 1.5 × maxBytes.
func ReadCAB1(r io.Reader, maxBytes int64) (Batch, error) {
	var b Batch
	cr := &capReader{r: r, left: maxBytes}
	var magic [len(CAB1Magic)]byte
	if err := cr.readFull(magic[:]); err != nil {
		return Batch{}, framingOr(err, "magic")
	}
	if string(magic[:]) != CAB1Magic {
		return Batch{}, fmt.Errorf("%w: bad magic", ErrCAB1Framing)
	}
	var hdr [recHeaderLen]byte
	for rec := 0; ; rec++ {
		if err := cr.readFull(hdr[:]); err != nil {
			return Batch{}, framingOr(err, fmt.Sprintf("record %d header (missing end record?)", rec))
		}
		typ, n := hdr[0], int64(binary.BigEndian.Uint32(hdr[1:]))
		switch typ {
		case RecEnd:
			if n != 0 {
				return Batch{}, fmt.Errorf("%w: end record with length %d", ErrCAB1Framing, n)
			}
			var one [1]byte
			switch _, err := io.ReadFull(cr.r, one[:]); err {
			case nil:
				return Batch{}, fmt.Errorf("%w: bytes after the end record", ErrCAB1Framing)
			case io.EOF:
				return b, nil
			default:
				return Batch{}, err
			}
		case RecEnvelope, RecPiece, RecTombstone:
		default:
			return Batch{}, fmt.Errorf("%w: record %d: unknown type %d", ErrCAB1Framing, rec, typ)
		}
		if typ == RecTombstone && n > MaxTombstoneJSON {
			return Batch{}, fmt.Errorf("%w: record %d: tombstone of %d bytes exceeds %d", ErrCAB1Framing, rec, n, MaxTombstoneJSON)
		}
		if typ == RecPiece && n < int64(pieceHeaderLen) {
			return Batch{}, fmt.Errorf("%w: record %d: piece record of %d bytes", ErrCAB1Framing, rec, n)
		}
		if cr.left < CAB1RecordOverhead {
			return Batch{}, fmt.Errorf("%w: record %d passes the %d-byte cap (%d bytes charged per record)", ErrCAB1TooLarge, rec, maxBytes, CAB1RecordOverhead)
		}
		cr.left -= CAB1RecordOverhead
		if n > cr.left {
			return Batch{}, fmt.Errorf("%w: record %d of %d bytes passes the %d-byte cap", ErrCAB1TooLarge, rec, n, maxBytes)
		}
		data, err := cr.readN(n)
		if err != nil {
			return Batch{}, framingOr(err, fmt.Sprintf("record %d body", rec))
		}
		switch typ {
		case RecEnvelope:
			b.Envelopes = append(b.Envelopes, data)
		case RecTombstone:
			b.Tombstones = append(b.Tombstones, data)
		case RecPiece:
			p, err := parsePiece(data)
			if err != nil {
				return Batch{}, fmt.Errorf("%w: record %d: %v", ErrCAB1Framing, rec, err)
			}
			b.Pieces = append(b.Pieces, p)
		}
	}
}

func parsePiece(data []byte) (BatchPiece, error) {
	var p BatchPiece
	copy(p.Hash[:], data)
	p.Kind = PieceKind(data[len(Hash{})])
	if !p.Kind.valid() {
		return BatchPiece{}, fmt.Errorf("piece kind %d", p.Kind)
	}
	ctLen := int(data[len(Hash{})+1])
	rest := data[pieceHeaderLen:]
	if ctLen > len(rest) {
		return BatchPiece{}, errors.New("content type longer than the record")
	}
	if ctLen > 0 && p.Kind != PieceAttachment {
		return BatchPiece{}, fmt.Errorf("content type on a %s piece", p.Kind)
	}
	p.ContentType = string(rest[:ctLen])
	p.Raw = rest[ctLen:]
	if sha256.Sum256(p.Raw) != p.Hash {
		return BatchPiece{}, fmt.Errorf("piece %s: hash mismatch", p.Hash)
	}
	return p, nil
}

// capReader counts the bytes it hands out against a cap. Exhausting the cap is ErrCAB1TooLarge.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) readFull(buf []byte) error {
	if int64(len(buf)) > c.left {
		// Read what is there to tell a short stream (framing) from a long one (too large).
		k, err := io.ReadFull(c.r, buf[:c.left])
		c.left -= int64(k)
		if err != nil {
			return err
		}
		var one [1]byte
		if _, err := io.ReadFull(c.r, one[:]); err != nil {
			return err
		}
		return ErrCAB1TooLarge
	}
	k, err := io.ReadFull(c.r, buf)
	c.left -= int64(k)
	return err
}

// readN reads exactly n <= c.left bytes, growing the buffer with the data actually read.
func (c *capReader) readN(n int64) ([]byte, error) {
	const chunk = 1 << 20
	if n <= chunk {
		buf := make([]byte, n)
		if err := c.readFull(buf); err != nil {
			return nil, err
		}
		return buf, nil
	}
	var buf bytes.Buffer
	buf.Grow(chunk)
	k, err := io.CopyN(&buf, c.r, n)
	c.left -= k
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// framingOr maps a short read to ErrCAB1Framing and keeps ErrCAB1TooLarge and real read errors.
func framingOr(err error, what string) error {
	switch {
	case errors.Is(err, ErrCAB1TooLarge):
		return fmt.Errorf("%w: %s", ErrCAB1TooLarge, what)
	case err == io.EOF || err == io.ErrUnexpectedEOF:
		return fmt.Errorf("%w: truncated %s", ErrCAB1Framing, what)
	}
	return err
}
