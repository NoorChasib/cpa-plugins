package outbox

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// NewEnvelope is one masked, canonical envelope body ready to be queued. The outbox assigns seq
// and builds the canon/v1 envelope around Body.
type NewEnvelope struct {
	Kind      wire.Kind
	Key       string
	CreatedAt time.Time
	ExactRev  uint32
	Refs      []wire.Hash
	Body      json.RawMessage
	Pieces    []canon.Piece // pieces this envelope references (a subset of Refs may already be stored)
}

// WriteResult reports one Write.
type WriteResult struct {
	Inserted   []Inserted
	Duplicates int // keys skipped because they were already queued (duplicate_key)
}

// Inserted is one envelope the write queued.
type Inserted struct {
	Seq uint64
	Key string
}

// buildEnvelope returns the canon/v1 envelope bytes for e at seq.
func (o *Outbox) buildEnvelope(e NewEnvelope, seq, epoch uint64, outboxID string) ([]byte, error) {
	env := wire.Envelope{
		V: wire.ProtocolV1, Kind: e.Kind, OutboxID: outboxID, Seq: seq, EpochSeen: epoch, Key: e.Key,
		MaskVersion: mask.Version, CanonVersion: canon.Version, ExactRev: e.ExactRev,
		CreatedAt: e.CreatedAt.UTC(), Refs: e.Refs, Body: e.Body,
	}
	return canon.Marshal(env)
}

func (o *Outbox) compress(b []byte) ([]byte, int) {
	c := o.enc.EncodeAll(b, make([]byte, 0, len(b)/3+64))
	if len(c) >= len(b) {
		return b, 0
	}
	return c, 1
}

// Write queues envs in one transaction. Each envelope is inserted with INSERT … ON CONFLICT(key)
// DO NOTHING against the partial unique index; seq comes from next_seq only for inserted rows, so a
// skipped duplicate never leaves a gap. Pieces and refs are written only for inserted
// envelopes; pieces over BlobThreshold go to fsynced blob files before the commit.
func (o *Outbox) Write(ctx context.Context, envs []NewEnvelope) (WriteResult, error) {
	var res WriteResult
	if o.closed.Load() {
		return res, ErrClosed
	}
	o.writeMu.Lock()
	defer o.writeMu.Unlock()
	o.mu.Lock()
	meta := o.meta
	o.mu.Unlock()
	now := o.opts.Now().UTC().UnixNano()
	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	next := meta.NextSeq
	var blobAdded int64
	var newBlobs []string
	defer func() {
		if err != nil {
			for _, p := range newBlobs {
				_ = os.Remove(p)
			}
		}
	}()
	for _, e := range envs {
		var body []byte
		body, err = o.buildEnvelope(e, next, meta.EpochSeen, meta.OutboxID)
		if err != nil {
			return res, err
		}
		stored, _ := o.compress(body)
		var r sql.Result
		r, err = tx.ExecContext(ctx, `INSERT INTO envelopes(seq,kind,key,created_at,state,attempts,next_attempt_at,body,body_len,bytes_total)
VALUES(?,?,?,?,0,0,0,?,?,?) ON CONFLICT(key) WHERE NOT (state = 3 AND dead_reason IS 'superseded') DO NOTHING`,
			next, string(e.Kind), e.Key, e.CreatedAt.UTC().UnixNano(), stored, len(body), len(stored))
		if err != nil {
			return res, err
		}
		n, _ := r.RowsAffected()
		if n != 1 {
			res.Duplicates++
			continue
		}
		var added int64
		var paths []string
		added, paths, err = o.putPieces(ctx, tx, e.Pieces, now)
		newBlobs = append(newBlobs, paths...)
		if err != nil {
			return res, err
		}
		blobAdded += added
		if err = putRefs(ctx, tx, next, e.Refs); err != nil {
			return res, err
		}
		res.Inserted = append(res.Inserted, Inserted{Seq: next, Key: e.Key})
		next++
	}
	if err = put(ctx, tx, "next_seq", strconv.FormatUint(next, 10)); err != nil {
		return res, err
	}
	if err = tx.Commit(); err != nil {
		return res, err
	}
	o.blobBytes.Add(blobAdded)
	o.mu.Lock()
	o.meta.NextSeq = next
	o.mu.Unlock()
	return res, nil
}

func putRefs(ctx context.Context, tx *sql.Tx, seq uint64, refs []wire.Hash) error {
	for _, h := range refs {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO envelope_refs(seq,hash) VALUES(?,?)", seq, h[:]); err != nil {
			return err
		}
	}
	return nil
}

// putPieces inserts pieces not yet stored and refreshes last_ref_at of the others. It returns the
// blob bytes added and the paths of blob files it created.
func (o *Outbox) putPieces(ctx context.Context, tx *sql.Tx, pieces []canon.Piece, now int64) (int64, []string, error) {
	var added int64
	var paths []string
	for _, p := range pieces {
		r, err := tx.ExecContext(ctx, "UPDATE pieces SET last_ref_at=? WHERE hash=?", now, p.Hash[:])
		if err != nil {
			return added, paths, err
		}
		if n, _ := r.RowsAffected(); n == 1 {
			continue
		}
		stored, codec := o.compress(p.Bytes)
		var inline []byte
		if len(stored) > BlobThreshold {
			path, err := o.writeBlob(p.Hash, stored)
			if err != nil {
				return added, paths, err
			}
			paths = append(paths, path)
			added += int64(len(stored))
		} else {
			inline = stored
		}
		var ct any
		if p.ContentType != "" {
			ct = p.ContentType
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO pieces(hash,kind,content_type,raw_len,stored_len,codec,inline,last_ref_at) VALUES(?,?,?,?,?,?,?,?)",
			p.Hash[:], int(p.Kind), ct, len(p.Bytes), len(stored), codec, nullBlob(inline), now); err != nil {
			return added, paths, err
		}
	}
	return added, paths, nil
}

func nullBlob(b []byte) any {
	if b == nil {
		return nil
	}
	return b
}

func (o *Outbox) blobPath(h wire.Hash) string {
	x := hex.EncodeToString(h[:])
	return filepath.Join(o.blobDir, x[:2], x+".zst")
}

// writeBlob writes a blob file: tmp file, fsync, rename, directory fsync.
func (o *Outbox) writeBlob(h wire.Hash, data []byte) (string, error) {
	final := o.blobPath(h)
	dir := filepath.Dir(final)
	if err := ensureDir(dir); err != nil {
		return "", err
	}
	tmp := filepath.Join(o.blobDir, "tmp", randomHex(12))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err = os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return final, nil
}

// StoredPiece is one piece as the outbox stores it. With Zstd set, Data is one zstd frame (level 3,
// window 8 MiB), the encoding the archive accepts on PUT /v1/pieces; otherwise Data is the raw
// piece (compression did not make it smaller). RawLen is the decoded size either way.
type StoredPiece struct {
	Kind        wire.PieceKind
	ContentType string
	RawLen      int64
	Zstd        bool
	Data        []byte
}

// StoredPiece returns one piece's stored bytes without decoding them, so the uploader can send a
// large piece as stored instead of holding it decoded and re-encoded.
func (o *Outbox) StoredPiece(ctx context.Context, h wire.Hash) (StoredPiece, error) {
	var kind, codec int
	var ct sql.NullString
	var inline []byte
	var rawLen int64
	err := o.read.QueryRowContext(ctx, "SELECT kind,content_type,codec,inline,raw_len FROM pieces WHERE hash=?", h[:]).Scan(&kind, &ct, &codec, &inline, &rawLen)
	if err != nil {
		return StoredPiece{}, err
	}
	data := inline
	if data == nil {
		f, err := os.OpenFile(o.blobPath(h), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return StoredPiece{}, err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return StoredPiece{}, err
		}
		data = make([]byte, info.Size())
		if _, err := ioReadFull(f, data); err != nil {
			return StoredPiece{}, err
		}
	}
	return StoredPiece{Kind: wire.PieceKind(kind), ContentType: ct.String, RawLen: rawLen, Zstd: codec == 1, Data: data}, nil
}

// Raw returns sp's raw piece bytes, decoding them when they are stored compressed.
func (o *Outbox) Raw(sp StoredPiece) ([]byte, error) {
	if !sp.Zstd {
		return sp.Data, nil
	}
	return o.dec.DecodeAll(sp.Data, make([]byte, 0, sp.RawLen))
}

// Piece returns one stored piece's kind, content type and raw bytes.
func (o *Outbox) Piece(ctx context.Context, h wire.Hash) (wire.PieceKind, string, []byte, error) {
	sp, err := o.StoredPiece(ctx, h)
	if err != nil {
		return 0, "", nil, err
	}
	raw, err := o.Raw(sp)
	if err != nil {
		return 0, "", nil, err
	}
	return sp.Kind, sp.ContentType, raw, nil
}

func ioReadFull(f *os.File, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := f.Read(b[n:])
		n += m
		if err != nil {
			if n == len(b) {
				return n, nil
			}
			return n, err
		}
	}
	return n, nil
}

// Item is one pending envelope or tombstone for the uploader.
type Item struct {
	Seq      uint64
	Key      string
	Kind     wire.Kind
	Envelope []byte // canon/v1 envelope bytes (nil for a tombstone)
	Attempts int
	Tomb     *wire.Tombstone
	Refs     []wire.Hash
}

// Size is the record bytes the item adds to a CAB1 batch.
func (it Item) Size() int64 {
	if it.Tomb != nil {
		return 256 + wire.CAB1RecordOverhead
	}
	return int64(len(it.Envelope)) + wire.CAB1RecordOverhead
}

// Pending returns up to maxItems pending envelopes and tombstones due at now, in seq order,
// stopping before maxBytes of records.
func (o *Outbox) Pending(ctx context.Context, now time.Time, maxItems int, maxBytes int64) ([]Item, error) {
	rows, err := o.read.QueryContext(ctx, `SELECT seq,key,kind,attempts,body,0 FROM envelopes WHERE state=0 AND next_attempt_at<=?
UNION ALL SELECT seq,key,'',0,NULL,1 FROM tombstones WHERE state=0 ORDER BY 1 LIMIT ?`, now.UTC().UnixNano(), maxItems)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	var total int64
	meta := o.Meta()
	for rows.Next() {
		var it Item
		var kind string
		var body []byte
		var tomb int
		if err := rows.Scan(&it.Seq, &it.Key, &kind, &it.Attempts, &body, &tomb); err != nil {
			return nil, err
		}
		if tomb == 1 {
			it.Tomb = &wire.Tombstone{V: wire.ProtocolV1, OutboxID: meta.OutboxID, Seq: it.Seq, Key: it.Key}
		} else {
			it.Kind = wire.Kind(kind)
			it.Envelope, err = o.dec.DecodeAll(body, nil)
			if err != nil {
				// Not zstd (stored raw because compression did not help).
				it.Envelope = body
			}
		}
		if len(out) > 0 && total+it.Size() > maxBytes {
			break
		}
		total += it.Size()
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		if out[i].Tomb != nil {
			var reason string
			if err := o.read.QueryRowContext(ctx, "SELECT reason FROM tombstones WHERE seq=?", out[i].Seq).Scan(&reason); err != nil {
				return nil, err
			}
			out[i].Tomb.Reason = reason
			continue
		}
		refs, err := o.refs(ctx, out[i].Seq)
		if err != nil {
			return nil, err
		}
		out[i].Refs = refs
	}
	return out, nil
}

func (o *Outbox) refs(ctx context.Context, seq uint64) ([]wire.Hash, error) {
	rows, err := o.read.QueryContext(ctx, "SELECT hash FROM envelope_refs WHERE seq=?", seq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []wire.Hash
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var h wire.Hash
		copy(h[:], b)
		out = append(out, h)
	}
	return out, rows.Err()
}

// Outcome kinds for Apply.
const (
	OutAck = iota
	OutDead
	OutDeadLocal
	OutRetry
)

// Outcome is the uploader's decision for one seq.
type Outcome struct {
	Seq         uint64
	Tomb        bool
	Kind        int
	Reason      string    // OutDead: the server reason; OutDeadLocal: the tombstone reason
	NextAttempt time.Time // OutRetry
}

// Apply records outcomes in one transaction. A local dead letter inserts its tombstone (state 0)
// in the same transaction. hw, when non-zero, is the latest server high-water mark.
func (o *Outbox) Apply(ctx context.Context, epoch, hw uint64, outs []Outcome) error {
	if o.closed.Load() {
		return ErrClosed
	}
	now := o.opts.Now().UTC().UnixNano()
	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	acked := false
	for _, oc := range outs {
		switch {
		case oc.Tomb && oc.Kind == OutAck:
			_, err = tx.ExecContext(ctx, "UPDATE tombstones SET state=2 WHERE seq=?", oc.Seq)
			acked = true
		case oc.Tomb:
			// A tombstone is never dead-lettered; it stays pending.
		case oc.Kind == OutAck:
			_, err = tx.ExecContext(ctx, "UPDATE envelopes SET state=2,acked_at=?,acked_epoch=?,attempts=0 WHERE seq=? AND state=0", now, epoch, oc.Seq)
			acked = true
		case oc.Kind == OutDead:
			_, err = tx.ExecContext(ctx, "UPDATE envelopes SET state=3,dead_reason=?,dead_at=? WHERE seq=? AND state=0", oc.Reason, now, oc.Seq)
		case oc.Kind == OutDeadLocal:
			var r sql.Result
			r, err = tx.ExecContext(ctx, "UPDATE envelopes SET state=3,dead_reason=?,dead_at=? WHERE seq=? AND state=0", oc.Reason, now, oc.Seq)
			if err == nil {
				if n, _ := r.RowsAffected(); n == 1 {
					_, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO tombstones(seq,key,reason,state,created_at) SELECT seq,key,?,0,? FROM envelopes WHERE seq=?", oc.Reason, now, oc.Seq)
				}
			}
		case oc.Kind == OutRetry:
			_, err = tx.ExecContext(ctx, "UPDATE envelopes SET attempts=attempts+1,next_attempt_at=? WHERE seq=? AND state=0", oc.NextAttempt.UTC().UnixNano(), oc.Seq)
		}
		if err != nil {
			return err
		}
	}
	if hw > 0 {
		if err = put(ctx, tx, "hw", strconv.FormatUint(hw, 10)); err != nil {
			return err
		}
	}
	if acked {
		if err = put(ctx, tx, "last_ack_at", strconv.FormatInt(now, 10)); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	o.mu.Lock()
	if hw > 0 {
		o.meta.HW = hw
	}
	if acked {
		o.meta.LastAckAt = time.Unix(0, now).UTC()
	}
	o.mu.Unlock()
	return nil
}

// SetServer stores the archive's epoch and archive id without a replay (first contact).
func (o *Outbox) SetServer(ctx context.Context, epoch uint64, archiveID string) error {
	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = put(ctx, tx, "epoch_seen", strconv.FormatUint(epoch, 10)); err != nil {
		return err
	}
	if err = put(ctx, tx, "archive_id", archiveID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	o.mu.Lock()
	o.meta.EpochSeen, o.meta.ArchiveID = epoch, archiveID
	o.mu.Unlock()
	return nil
}

// Replay applies an epoch or archive change (the archive was restored or
// replaced): in one transaction every retained
// acked envelope and every acked tombstone with seq > hw becomes pending again, and the new epoch
// and archive id are stored. It reports a replay gap when hw+1 is older than the oldest retained
// seq.
func (o *Outbox) Replay(ctx context.Context, epoch uint64, archiveID string, hw uint64) (gapFrom, gapTo uint64, err error) {
	o.writeMu.Lock() // reads next_seq
	defer o.writeMu.Unlock()
	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE envelopes SET state=0,attempts=0,next_attempt_at=0 WHERE state=2"); err != nil {
		return 0, 0, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE tombstones SET state=0 WHERE state=2 AND seq>?", hw); err != nil {
		return 0, 0, err
	}
	var oldest sql.NullInt64
	if err = tx.QueryRowContext(ctx, "SELECT min(seq) FROM (SELECT seq FROM envelopes UNION ALL SELECT seq FROM tombstones)").Scan(&oldest); err != nil {
		return 0, 0, err
	}
	o.mu.Lock()
	next := o.meta.NextSeq
	o.mu.Unlock()
	first := next
	if oldest.Valid {
		first = uint64(oldest.Int64)
	}
	if hw+1 < first {
		gapFrom, gapTo = hw+1, first-1
	}
	for k, v := range map[string]string{
		"epoch_seen": strconv.FormatUint(epoch, 10), "archive_id": archiveID, "hw": strconv.FormatUint(hw, 10),
		"replay_gap_from": strconv.FormatUint(gapFrom, 10), "replay_gap_to": strconv.FormatUint(gapTo, 10),
	} {
		if err = put(ctx, tx, k, v); err != nil {
			return 0, 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, err
	}
	o.mu.Lock()
	o.meta.EpochSeen, o.meta.ArchiveID, o.meta.HW = epoch, archiveID, hw
	o.meta.ReplayGapFrom, o.meta.ReplayGapTo = gapFrom, gapTo
	o.mu.Unlock()
	return gapFrom, gapTo, nil
}

// Dead is one dead letter.
type Dead struct {
	Seq      uint64
	Key      string
	Reason   string
	Envelope []byte
}

// DeadLetters returns up to limit dead letters with reason (all reasons but superseded when
// reason is ""), with seq > after, in seq order.
func (o *Outbox) DeadLetters(ctx context.Context, reason string, after uint64, limit int) ([]Dead, error) {
	rows, err := o.read.QueryContext(ctx, `SELECT seq,key,dead_reason,body FROM envelopes WHERE state=3 AND dead_reason IS NOT 'superseded'
AND (?='' OR dead_reason=?) AND seq>? ORDER BY seq LIMIT ?`, reason, reason, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Dead
	for rows.Next() {
		var d Dead
		var body []byte
		var r sql.NullString
		if err := rows.Scan(&d.Seq, &d.Key, &r, &body); err != nil {
			return nil, err
		}
		d.Reason = r.String
		if d.Envelope, err = o.dec.DecodeAll(body, nil); err != nil {
			d.Envelope = body
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ErrNotDead is returned by Requeue when the old row is no longer a dead letter.
var ErrNotDead = errors.New("outbox: not a dead letter")

// Requeue replaces dead letter oldSeq by e in one transaction: the old row becomes state 3 /
// superseded first, then e is inserted with a new seq (same key and created_at as the caller
// passes). Returns the new seq.
func (o *Outbox) Requeue(ctx context.Context, oldSeq uint64, e NewEnvelope) (uint64, error) {
	if o.closed.Load() {
		return 0, ErrClosed
	}
	o.writeMu.Lock()
	defer o.writeMu.Unlock()
	o.mu.Lock()
	meta := o.meta
	o.mu.Unlock()
	now := o.opts.Now().UTC().UnixNano()
	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, "UPDATE envelopes SET dead_reason='superseded',dead_at=? WHERE seq=? AND state=3 AND dead_reason IS NOT 'superseded'", now, oldSeq)
	if err != nil {
		return 0, err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return 0, ErrNotDead
	}
	seq := meta.NextSeq
	body, err := o.buildEnvelope(e, seq, meta.EpochSeen, meta.OutboxID)
	if err != nil {
		return 0, err
	}
	stored, _ := o.compress(body)
	if _, err = tx.ExecContext(ctx, `INSERT INTO envelopes(seq,kind,key,created_at,state,attempts,next_attempt_at,body,body_len,bytes_total)
VALUES(?,?,?,?,0,0,0,?,?,?)`, seq, string(e.Kind), e.Key, e.CreatedAt.UTC().UnixNano(), stored, len(body), len(stored)); err != nil {
		return 0, err
	}
	added, paths, err := o.putPieces(ctx, tx, e.Pieces, now)
	if err != nil {
		for _, p := range paths {
			_ = os.Remove(p)
		}
		return 0, err
	}
	if err = putRefs(ctx, tx, seq, e.Refs); err != nil {
		return 0, err
	}
	if err = put(ctx, tx, "next_seq", strconv.FormatUint(seq+1, 10)); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		for _, p := range paths {
			_ = os.Remove(p)
		}
		return 0, err
	}
	o.blobBytes.Add(added)
	o.mu.Lock()
	o.meta.NextSeq = seq + 1
	o.mu.Unlock()
	return seq, nil
}

// ProbeWrite checks that a write transaction commits, without changing any data.
func (o *Outbox) ProbeWrite(ctx context.Context) error {
	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE metadata SET value=value WHERE key='next_seq'"); err != nil {
		return err
	}
	return tx.Commit()
}
