package importer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // pure-Go driver "sqlite", so the importer builds with CGO_ENABLED=0
)

// The state schema: positions, reasons and counters only, never content. meta also holds the last run's
// time, state and faults and the auth backoff deadline, which `status` and the 401 rule (no upload for
// 15 minutes after a 401 or 403) need across runs.
const stateSchema = `
CREATE TABLE IF NOT EXISTS meta(
  outbox_id TEXT NOT NULL,
  next_seq INTEGER NOT NULL,
  epoch_seen INTEGER NOT NULL,
  archive_id TEXT NOT NULL,
  last_run_at INTEGER NOT NULL DEFAULT 0,
  last_run_state TEXT NOT NULL DEFAULT '',
  faults TEXT NOT NULL DEFAULT '',
  auth_backoff_until INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS cursors(
  file_key TEXT PRIMARY KEY,
  inode INTEGER NOT NULL,
  size INTEGER NOT NULL,
  mtime_ns INTEGER NOT NULL,
  end_offset INTEGER NOT NULL,
  line_ordinal INTEGER NOT NULL,
  head_sha256 TEXT NOT NULL,
  scan_gen INTEGER NOT NULL,
  last_seq INTEGER NOT NULL,
  outbox_id TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS dead(
  key TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  file_key TEXT NOT NULL,
  scan_gen INTEGER NOT NULL,
  line_ordinal INTEGER NOT NULL,
  byte_offset INTEGER NOT NULL,
  byte_len INTEGER NOT NULL,
  reason TEXT NOT NULL,
  rule_ids TEXT NOT NULL,
  seq INTEGER NOT NULL,
  dead_at INTEGER NOT NULL,
  state TEXT NOT NULL CHECK(state IN ('dead','requeued','gone'))
);
CREATE TABLE IF NOT EXISTS tombstones(
  seq INTEGER PRIMARY KEY,
  key TEXT NOT NULL,
  reason TEXT NOT NULL,
  state TEXT NOT NULL CHECK(state IN ('pending','acked'))
);
`

// Meta is the one meta row.
type Meta struct {
	OutboxID         string
	NextSeq          uint64
	EpochSeen        uint64
	ArchiveID        string
	LastRunAt        int64 // unix seconds
	LastRunState     string
	Faults           string // comma-separated fault names
	AuthBackoffUntil int64  // unix seconds; no upload before it (a 401 or 403 stops uploads for 15 minutes)
}

// Cursor is one cursors row: how far a file has been read and committed.
type Cursor struct {
	FileKey     string
	Inode       uint64
	Size        int64
	MtimeNS     int64
	EndOffset   int64
	LineOrdinal int64
	HeadSHA     string
	ScanGen     uint32
	LastSeq     uint64
	OutboxID    string
}

// Dead is one dead row: positions and reasons only.
type Dead struct {
	Key         string
	Kind        string
	FileKey     string
	ScanGen     uint32
	LineOrdinal int64
	ByteOffset  int64
	ByteLen     int64
	Reason      string
	RuleIDs     string
	Seq         uint64
	DeadAt      int64
	State       string
}

// TombRow is one tombstones row: a locally dead seq the next commit reports, so hw can pass it.
type TombRow struct {
	Seq    uint64
	Key    string
	Reason string
}

// State is the importer's SQLite file (modernc.org/sqlite).
type State struct {
	db *sql.DB
}

func openState(path string) (*State, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// Create the file 0600 before SQLite does, so it never exists with a wider mode.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(stateSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("state schema: %w", err)
	}
	return &State{db: db}, nil
}

func (s *State) Close() error { return s.db.Close() }

var errNoMeta = errors.New("state has no meta row")

func (s *State) meta() (Meta, error) {
	var m Meta
	err := s.db.QueryRow(`SELECT outbox_id, next_seq, epoch_seen, archive_id, last_run_at, last_run_state, faults, auth_backoff_until FROM meta LIMIT 1`).
		Scan(&m.OutboxID, &m.NextSeq, &m.EpochSeen, &m.ArchiveID, &m.LastRunAt, &m.LastRunState, &m.Faults, &m.AuthBackoffUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return Meta{}, errNoMeta
	}
	return m, err
}

// tx runs fn in one immediate transaction.
func (s *State) tx(fn func(*sql.Tx) error) error {
	t, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	if err := fn(t); err != nil {
		t.Rollback()
		return err
	}
	return t.Commit()
}

func putMeta(t *sql.Tx, m Meta) error {
	if _, err := t.Exec(`DELETE FROM meta`); err != nil {
		return err
	}
	_, err := t.Exec(`INSERT INTO meta(outbox_id, next_seq, epoch_seen, archive_id, last_run_at, last_run_state, faults, auth_backoff_until) VALUES(?,?,?,?,?,?,?,?)`,
		m.OutboxID, m.NextSeq, m.EpochSeen, m.ArchiveID, m.LastRunAt, m.LastRunState, m.Faults, m.AuthBackoffUntil)
	return err
}

func (s *State) putMeta(m Meta) error {
	return s.tx(func(t *sql.Tx) error { return putMeta(t, m) })
}

// setRunInfo records the last run without touching next_seq or the outbox, so a failed cursor
// transaction can never leave next_seq ahead of the cursors.
func (s *State) setRunInfo(at int64, state, faults string) error {
	_, err := s.db.Exec(`UPDATE meta SET last_run_at=?, last_run_state=?, faults=?`, at, state, faults)
	return err
}

func (s *State) setAuthBackoff(until int64) error {
	_, err := s.db.Exec(`UPDATE meta SET auth_backoff_until=?`, until)
	return err
}

func (s *State) cursors() (map[string]Cursor, error) {
	rows, err := s.db.Query(`SELECT file_key, inode, size, mtime_ns, end_offset, line_ordinal, head_sha256, scan_gen, last_seq, outbox_id FROM cursors`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Cursor{}
	for rows.Next() {
		var c Cursor
		var inode int64
		if err := rows.Scan(&c.FileKey, &inode, &c.Size, &c.MtimeNS, &c.EndOffset, &c.LineOrdinal, &c.HeadSHA, &c.ScanGen, &c.LastSeq, &c.OutboxID); err != nil {
			return nil, err
		}
		c.Inode = uint64(inode)
		out[c.FileKey] = c
	}
	return out, rows.Err()
}

func putCursor(t *sql.Tx, c Cursor) error {
	_, err := t.Exec(`INSERT INTO cursors(file_key, inode, size, mtime_ns, end_offset, line_ordinal, head_sha256, scan_gen, last_seq, outbox_id)
VALUES(?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(file_key) DO UPDATE SET inode=excluded.inode, size=excluded.size, mtime_ns=excluded.mtime_ns,
  end_offset=excluded.end_offset, line_ordinal=excluded.line_ordinal, head_sha256=excluded.head_sha256,
  scan_gen=excluded.scan_gen, last_seq=excluded.last_seq, outbox_id=excluded.outbox_id`,
		c.FileKey, int64(c.Inode), c.Size, c.MtimeNS, c.EndOffset, c.LineOrdinal, c.HeadSHA, c.ScanGen, c.LastSeq, c.OutboxID)
	return err
}

func putDead(t *sql.Tx, d Dead) error {
	_, err := t.Exec(`INSERT INTO dead(key, kind, file_key, scan_gen, line_ordinal, byte_offset, byte_len, reason, rule_ids, seq, dead_at, state)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(key) DO UPDATE SET kind=excluded.kind, file_key=excluded.file_key, scan_gen=excluded.scan_gen,
  line_ordinal=excluded.line_ordinal, byte_offset=excluded.byte_offset, byte_len=excluded.byte_len,
  reason=excluded.reason, rule_ids=excluded.rule_ids, seq=excluded.seq, dead_at=excluded.dead_at, state=excluded.state`,
		d.Key, d.Kind, d.FileKey, d.ScanGen, d.LineOrdinal, d.ByteOffset, d.ByteLen, d.Reason, d.RuleIDs, d.Seq, d.DeadAt, d.State)
	return err
}

func setDeadState(t *sql.Tx, key, state string) error {
	_, err := t.Exec(`UPDATE dead SET state=? WHERE key=?`, state, key)
	return err
}

// deadRows lists dead rows, optionally filtered by reason, state and a minimum dead_at.
func (s *State) deadRows(reason, state string, since int64) ([]Dead, error) {
	q := `SELECT key, kind, file_key, scan_gen, line_ordinal, byte_offset, byte_len, reason, rule_ids, seq, dead_at, state FROM dead WHERE dead_at >= ?`
	args := []any{since}
	if reason != "" {
		q += ` AND reason = ?`
		args = append(args, reason)
	}
	if state != "" {
		q += ` AND state = ?`
		args = append(args, state)
	}
	q += ` ORDER BY dead_at, key`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Dead
	for rows.Next() {
		var d Dead
		if err := rows.Scan(&d.Key, &d.Kind, &d.FileKey, &d.ScanGen, &d.LineOrdinal, &d.ByteOffset, &d.ByteLen, &d.Reason, &d.RuleIDs, &d.Seq, &d.DeadAt, &d.State); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func addTombstone(t *sql.Tx, tb TombRow) error {
	_, err := t.Exec(`INSERT OR REPLACE INTO tombstones(seq, key, reason, state) VALUES(?,?,?,'pending')`, tb.Seq, tb.Key, tb.Reason)
	return err
}

func ackTombstone(t *sql.Tx, seq uint64) error {
	_, err := t.Exec(`UPDATE tombstones SET state='acked' WHERE seq=?`, seq)
	return err
}

func (s *State) pendingTombstones() ([]TombRow, error) {
	rows, err := s.db.Query(`SELECT seq, key, reason FROM tombstones WHERE state='pending' ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TombRow
	for rows.Next() {
		var tb TombRow
		if err := rows.Scan(&tb.Seq, &tb.Key, &tb.Reason); err != nil {
			return nil, err
		}
		out = append(out, tb)
	}
	return out, rows.Err()
}

// counts returns small aggregate counts for status and heartbeats.
func (s *State) count(query string, args ...any) (int64, error) {
	var n int64
	err := s.db.QueryRow(query, args...).Scan(&n)
	return n, err
}

func (s *State) deadByReason() (map[string]map[string]int64, error) {
	rows, err := s.db.Query(`SELECT state, reason, count(*) FROM dead GROUP BY state, reason`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]int64{}
	for rows.Next() {
		var st, r string
		var n int64
		if err := rows.Scan(&st, &r, &n); err != nil {
			return nil, err
		}
		if out[st] == nil {
			out[st] = map[string]int64{}
		}
		out[st][r] = n
	}
	return out, rows.Err()
}

func splitFaults(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
