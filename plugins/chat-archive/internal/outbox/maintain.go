package outbox

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

const chunk = 512

// deadRetention: dead letters are deleted after 90 days.
const deadRetention = 90 * 24 * time.Hour

// Maintain runs one bounded GC pass: acked envelopes and acked tombstones
// past the replay window, unreferenced pieces past the window, dead letters after 90 days, orphan
// blob files, then an incremental vacuum and a best-effort WAL truncate. It reports whether a
// chunk was full (more work remains). Pending envelopes and tombstones are never deleted.
func (o *Outbox) Maintain(ctx context.Context) (bool, error) {
	if o.closed.Load() {
		return false, ErrClosed
	}
	now := o.opts.Now().UTC()
	cutoff := now.Add(-o.opts.Retention).UnixNano()
	more := false
	n, err := o.execN(ctx, "DELETE FROM envelopes WHERE seq IN (SELECT seq FROM envelopes WHERE state=2 AND acked_at<? ORDER BY acked_at LIMIT ?)", cutoff, chunk)
	if err != nil {
		return false, err
	}
	more = more || n == chunk
	if n, err = o.execN(ctx, "DELETE FROM tombstones WHERE seq IN (SELECT seq FROM tombstones WHERE state=2 AND created_at<? LIMIT ?)", cutoff, chunk); err != nil {
		return false, err
	}
	more = more || n == chunk
	if n, err = o.execN(ctx, "DELETE FROM envelopes WHERE seq IN (SELECT seq FROM envelopes WHERE state=3 AND dead_at<? LIMIT ?)", now.Add(-deadRetention).UnixNano(), chunk); err != nil {
		return false, err
	}
	more = more || n == chunk
	full, err := o.gcPieces(ctx, cutoff)
	if err != nil {
		return false, err
	}
	more = more || full
	o.gcOrphanBlobs(now)
	if _, err = o.db.ExecContext(ctx, "PRAGMA incremental_vacuum(128)"); err != nil {
		return more, err
	}
	_, _ = o.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	return more, nil
}

func (o *Outbox) execN(ctx context.Context, q string, args ...any) (int64, error) {
	r, err := o.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// gcPieces deletes up to one chunk of unreferenced pieces whose last_ref_at is older than cutoff;
// blob files are unlinked after the commit.
func (o *Outbox) gcPieces(ctx context.Context, cutoff int64) (bool, error) {
	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT hash,inline IS NULL,stored_len FROM pieces p WHERE last_ref_at<?
AND NOT EXISTS(SELECT 1 FROM envelope_refs r WHERE r.hash=p.hash) LIMIT ?`, cutoff, chunk)
	if err != nil {
		return false, err
	}
	type gone struct {
		h      wire.Hash
		blob   bool
		stored int64
	}
	var victims []gone
	for rows.Next() {
		var b []byte
		var g gone
		if err := rows.Scan(&b, &g.blob, &g.stored); err != nil {
			rows.Close()
			return false, err
		}
		copy(g.h[:], b)
		victims = append(victims, g)
	}
	rows.Close()
	for _, v := range victims {
		if _, err := tx.ExecContext(ctx, "DELETE FROM pieces WHERE hash=?", v.h[:]); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	for _, v := range victims {
		if v.blob {
			if os.Remove(o.blobPath(v.h)) == nil {
				o.blobBytes.Add(-v.stored)
			}
		}
	}
	return len(victims) == chunk, nil
}

// gcOrphanBlobs removes files left in blobs/tmp and blob files with no row older than 1 h.
func (o *Outbox) gcOrphanBlobs(now time.Time) {
	tmp := filepath.Join(o.blobDir, "tmp")
	if ents, err := os.ReadDir(tmp); err == nil {
		for _, e := range ents {
			if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > time.Hour {
				_ = os.Remove(filepath.Join(tmp, e.Name()))
			}
		}
	}
	dirs, err := os.ReadDir(o.blobDir)
	if err != nil {
		return
	}
	for _, d := range dirs {
		if !d.IsDir() || len(d.Name()) != 2 {
			continue
		}
		files, err := os.ReadDir(filepath.Join(o.blobDir, d.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			name := f.Name()
			if len(name) != 68 || filepath.Ext(name) != ".zst" {
				continue
			}
			h, err := wire.ParseHash(name[:64])
			if err != nil {
				continue
			}
			info, err := f.Info()
			if err != nil || now.Sub(info.ModTime()) < time.Hour {
				continue
			}
			var one int
			err = o.read.QueryRow("SELECT 1 FROM pieces WHERE hash=? AND inline IS NULL", h[:]).Scan(&one)
			if err == sql.ErrNoRows {
				_ = os.Remove(filepath.Join(o.blobDir, d.Name(), name))
			}
		}
	}
}

// Pressure levels: 0 normal; 1 above 80% of the cap or under twice the free-space guard (evict
// acked envelopes); 2 above the cap or under the guard (bodies become metadata-only); 3 more than
// 1 GiB above the cap or under 1 GiB free (nothing is written).
const (
	PressureNormal = 0
	PressureEvict  = 1
	PressureGuard  = 2
	PressureHard   = 3
)

// PressureState is one pressure computation.
type PressureState struct {
	Level   int
	Total   int64
	Free    int64
	Cap     int64
	ByCap   bool // the level comes from the cap (outbox_cap) rather than free space (disk_guard)
	FSBytes int64
}

// Pressure computes the cap and free-space ladder.
func (o *Outbox) Pressure() PressureState {
	ps := PressureState{Total: o.TotalBytes(), Cap: o.opts.CapBytes}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(o.path), &fs); err == nil {
		ps.Free = int64(fs.Bavail) * int64(fs.Bsize)
		ps.FSBytes = int64(fs.Blocks) * int64(fs.Bsize)
	} else {
		ps.Free = 1 << 62
	}
	guard := max(o.opts.MinFreeBytes, ps.FSBytes*int64(o.opts.MinFreePercent)/100)
	switch {
	case ps.Free < 1<<30 || ps.Total > ps.Cap+(1<<30):
		ps.Level, ps.ByCap = PressureHard, ps.Total > ps.Cap+(1<<30)
	case ps.Total > ps.Cap || ps.Free < guard:
		ps.Level, ps.ByCap = PressureGuard, ps.Total > ps.Cap
	case ps.Total > ps.Cap*8/10 || ps.Free < 2*guard:
		ps.Level, ps.ByCap = PressureEvict, ps.Total > ps.Cap*8/10
	}
	return ps
}

// Evict implements pressure level 1: acked-retained envelopes oldest first until the outbox is
// below 70% of the cap, then dead letters older than 30 days. Pending data is never evicted.
func (o *Outbox) Evict(ctx context.Context) error {
	target := o.opts.CapBytes * 7 / 10
	for i := 0; i < 64 && o.TotalBytes() > target; i++ {
		n, err := o.execN(ctx, "DELETE FROM envelopes WHERE seq IN (SELECT seq FROM envelopes WHERE state=2 ORDER BY acked_at LIMIT ?)", chunk)
		if err != nil {
			return err
		}
		if _, err := o.gcPieces(ctx, o.opts.Now().UTC().UnixNano()); err != nil {
			return err
		}
		if _, err := o.db.ExecContext(ctx, "PRAGMA incremental_vacuum(1024)"); err != nil {
			return err
		}
		if n == 0 {
			break
		}
	}
	if o.TotalBytes() > target {
		cut := o.opts.Now().UTC().Add(-30 * 24 * time.Hour).UnixNano()
		if _, err := o.execN(ctx, "DELETE FROM envelopes WHERE seq IN (SELECT seq FROM envelopes WHERE state=3 AND dead_at<? LIMIT ?)", cut, chunk); err != nil {
			return err
		}
	}
	return nil
}

// Stats is the outbox part of status and heartbeats.
type Stats struct {
	wire.OutboxStats
	DeadByReason map[string]uint64
	UncleanRuns  uint64
}

// Stats reads counts through the reader pool. Only the maintenance goroutine calls it; the
// status route serves its cached result.
func (o *Outbox) Stats(ctx context.Context) (Stats, error) {
	o.StatsCalls.Add(1)
	var s Stats
	if o.closed.Load() {
		return s, ErrClosed
	}
	now := o.opts.Now().UTC()
	var pending, pendingBytes, acked, ackedBytes sql.NullInt64
	var oldest, oldestAck sql.NullInt64
	// Both aggregates are range scans of envelopes_stats (covering), never of the table rows.
	if err := o.read.QueryRowContext(ctx, "SELECT count(*), sum(bytes_total), min(created_at) FROM envelopes INDEXED BY envelopes_stats WHERE state=0").
		Scan(&pending, &pendingBytes, &oldest); err != nil {
		return s, err
	}
	if err := o.read.QueryRowContext(ctx, "SELECT count(*), sum(bytes_total), min(acked_at) FROM envelopes INDEXED BY envelopes_stats WHERE state=2").
		Scan(&acked, &ackedBytes, &oldestAck); err != nil {
		return s, err
	}
	var tombs sql.NullInt64
	if err := o.read.QueryRowContext(ctx, "SELECT count(*) FROM tombstones WHERE state=0").Scan(&tombs); err != nil {
		return s, err
	}
	s.Pending = uint64(pending.Int64 + tombs.Int64)
	s.PendingBytes = uint64(pendingBytes.Int64)
	s.AckedRetained = uint64(acked.Int64)
	s.AckedRetainedBytes = uint64(ackedBytes.Int64)
	if oldest.Valid {
		s.OldestPendingAgeS = max(int64(now.Sub(time.Unix(0, oldest.Int64)).Seconds()), 0)
	}
	if oldestAck.Valid {
		s.ReplayWindowH = now.Sub(time.Unix(0, oldestAck.Int64)).Hours()
	} else {
		s.ReplayWindowH = o.opts.Retention.Hours()
	}
	rows, err := o.read.QueryContext(ctx, "SELECT dead_reason,count(*) FROM envelopes INDEXED BY envelopes_stats WHERE state=3 AND dead_reason IS NOT 'superseded' GROUP BY dead_reason")
	if err != nil {
		return s, err
	}
	s.DeadByReason = map[string]uint64{}
	for rows.Next() {
		var r sql.NullString
		var n uint64
		if err := rows.Scan(&r, &n); err != nil {
			rows.Close()
			return s, err
		}
		s.DeadByReason[r.String] = n
		s.Dead += n
	}
	rows.Close()
	ps := o.Pressure()
	s.DiskBytes, s.CapBytes, s.FreeBytes, s.Pressure = uint64(ps.Total), uint64(ps.Cap), uint64(max(ps.Free, 0)), ps.Level
	m := o.Meta()
	s.NextSeq, s.HW, s.LastAckAt, s.UncleanRuns = m.NextSeq, m.HW, m.LastAckAt, m.UncleanRuns
	return s, nil
}

// IntegrityCheck runs PRAGMA integrity_check (tests and native gates).
func (o *Outbox) IntegrityCheck(ctx context.Context) (string, error) {
	var s string
	err := o.db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&s)
	return s, err
}
