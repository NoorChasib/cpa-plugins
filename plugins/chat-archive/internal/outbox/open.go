// Package outbox is the plugin's durable queue: a hardened SQLite database
// (mattn/go-sqlite3) holding masked, canonical envelopes and pieces until the archive acks them,
// then for the replay window. The open sequence, DSN and file checks are token-usage's
// (internal/store/store.go); retention is ack-driven, never time-based for pending data.
//
// Nothing unmasked is ever written here: envelopes and pieces arrive already masked from the
// capture workers, and no media URL ever reaches this package.
package outbox

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
	sqlite "github.com/mattn/go-sqlite3"
)

// Errors.
var (
	ErrUnavailable = errors.New("outbox unavailable")
	ErrClosed      = errors.New("outbox closed")
)

// Envelope states.
const (
	StatePending = 0
	StateAcked   = 2
	StateDead    = 3
)

// DeadSuperseded marks a dead letter replaced by a requeued row with the same key.
const DeadSuperseded = "superseded"

// BlobThreshold: pieces whose stored form is larger go to blob files.
const BlobThreshold = 256 << 10

// Options configure an outbox.
type Options struct {
	Dir            string
	CapBytes       int64
	MinFreeBytes   int64
	MinFreePercent int
	Retention      time.Duration
	Now            func() time.Time
}

// Outbox is one open outbox. It is safe for concurrent use; writes are serialized on one
// connection.
type Outbox struct {
	opts      Options
	path      string
	blobDir   string
	db, read  *sql.DB
	lock      *os.File
	enc       *zstd.Encoder
	dec       *zstd.Decoder
	blobBytes atomic.Int64
	closed    atomic.Bool

	// writeMu serializes every transaction that allocates seqs or reads next_seq (Write, Requeue,
	// Replay), from reading meta.NextSeq through Commit and the meta update. MaxOpenConns(1)
	// serializes the transactions but not the meta snapshot taken before BeginTx, so without it
	// two writers could pick the same seq.
	writeMu sync.Mutex

	mu   sync.Mutex
	meta Meta
	run  string

	// StatsCalls counts Stats queries (test hook: the status route must never trigger one).
	StatsCalls atomic.Uint64
}

// Meta is the persisted outbox identity and replay state.
type Meta struct {
	OutboxID      string
	EpochSeen     uint64
	ArchiveID     string
	NextSeq       uint64
	HW            uint64
	ReplayGapFrom uint64
	ReplayGapTo   uint64
	UncleanRuns   uint64
	LastAckAt     time.Time
}

func owned(path string, directory bool, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) || info.Mode().Perm() != mode || info.Mode()&os.ModeSymlink != 0 ||
		info.IsDir() != directory || (!directory && (!info.Mode().IsRegular() || st.Nlink != 1)) {
		return errors.New("storage permissions or type invalid")
	}
	return nil
}

func ensureDir(p string) error {
	if err := os.Mkdir(p, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	return owned(p, true, 0o700)
}

func prepare(path string) (*os.File, error) {
	dir := filepath.Dir(path)
	for p := dir; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, ErrUnavailable
		}
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := owned(dir, true, 0o700); err != nil {
		return nil, err
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		return nil, err
	}
	switch uint64(fs.Type) {
	case 0x6969, 0x517b, 0xff534d42, 0xfe534d42:
		return nil, errors.New("network filesystem unsupported")
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal", ".lock"} {
		if _, err := os.Lstat(path + suffix); err == nil {
			if err = owned(path+suffix, false, 0o600); err != nil {
				return nil, err
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("outbox already owned")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		lock.Close()
		return nil, err
	}
	file.Close()
	return lock, nil
}

func dsn(path string, read bool) string {
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{"mode": {"rw"}, "_busy_timeout": {"100"}, "_foreign_keys": {"on"}, "_synchronous": {"FULL"}}
	if read {
		q.Set("_query_only", "true")
	} else {
		q.Set("_stmt_cache_size", "32")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// statsIndex covers every column Stats reads, so its per-state aggregates walk the index instead
// of each row's body overflow chain (bytes_total sits after the body BLOB). Open creates it on a
// database made before it existed; older binaries ignore it, so user_version stays 1.
const statsIndex = `CREATE INDEX IF NOT EXISTS envelopes_stats ON envelopes(state, dead_reason, created_at, acked_at, bytes_total)`

const schema = `CREATE TABLE metadata(key TEXT PRIMARY KEY CHECK(length(key)<=64), value TEXT NOT NULL CHECK(length(value)<=65536));
CREATE TABLE runs(id TEXT PRIMARY KEY, started_at INTEGER NOT NULL, stopped_at INTEGER, clean INTEGER NOT NULL);
CREATE TABLE envelopes(
  seq INTEGER PRIMARY KEY CHECK(seq>0),
  kind TEXT NOT NULL,
  key TEXT NOT NULL CHECK(length(key) BETWEEN 1 AND 200),
  created_at INTEGER NOT NULL,
  state INTEGER NOT NULL CHECK(state IN (0,2,3)),
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL DEFAULT 0,
  acked_at INTEGER, acked_epoch INTEGER,
  dead_reason TEXT, dead_at INTEGER,
  body BLOB NOT NULL,
  body_len INTEGER NOT NULL CHECK(body_len>0),
  bytes_total INTEGER NOT NULL);
CREATE UNIQUE INDEX envelopes_key ON envelopes(key) WHERE NOT (state = 3 AND dead_reason IS 'superseded');
CREATE INDEX envelopes_pending ON envelopes(state, next_attempt_at, seq);
CREATE INDEX envelopes_acked ON envelopes(acked_at) WHERE state=2;
` + statsIndex + `;
CREATE TABLE pieces(
  hash BLOB PRIMARY KEY CHECK(length(hash)=32),
  kind INTEGER NOT NULL CHECK(kind IN (0,1,2)),
  content_type TEXT,
  raw_len INTEGER NOT NULL,
  stored_len INTEGER NOT NULL,
  codec INTEGER NOT NULL CHECK(codec IN (0,1)),
  inline BLOB,
  last_ref_at INTEGER NOT NULL) WITHOUT ROWID;
CREATE TABLE envelope_refs(
  seq INTEGER NOT NULL REFERENCES envelopes(seq) ON DELETE CASCADE,
  hash BLOB NOT NULL,
  PRIMARY KEY(seq, hash)) WITHOUT ROWID;
CREATE INDEX envelope_refs_hash ON envelope_refs(hash);
CREATE TABLE tombstones(seq INTEGER PRIMARY KEY, key TEXT NOT NULL, reason TEXT NOT NULL, state INTEGER NOT NULL CHECK(state IN (0,2)), created_at INTEGER NOT NULL);
PRAGMA user_version=1;`

// OpenTimeout bounds Open (the synchronous attempt on the lifecycle path).
var OpenTimeout = 15 * time.Second

// Open is OpenContext with OpenTimeout.
func Open(opts Options) (*Outbox, error) {
	ctx, cancel := context.WithTimeout(context.Background(), OpenTimeout)
	defer cancel()
	return OpenContext(ctx, opts)
}

// OpenContext opens or creates the outbox in opts.Dir: outbox.sqlite, its companions, and blobs/.
// Directories are 0700, files 0600. It fails closed on a foreign or newer database. ctx bounds
// every query of the open sequence. PRAGMA quick_check runs only when a previous run did not
// close cleanly: a clean Close leaves a consistent database, and on a multi-GB outbox the check
// is the slow step of the open.
func OpenContext(ctx context.Context, opts Options) (o *Outbox, err error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	o = &Outbox{opts: opts, path: filepath.Join(opts.Dir, "outbox.sqlite"), blobDir: filepath.Join(opts.Dir, "blobs")}
	defer func() {
		if err != nil {
			o.Close()
			o, err = nil, ErrUnavailable
		}
	}()
	if o.lock, err = prepare(o.path); err != nil {
		return
	}
	if err = ensureDir(o.blobDir); err != nil {
		return
	}
	if err = ensureDir(filepath.Join(o.blobDir, "tmp")); err != nil {
		return
	}
	if o.db, err = sql.Open("sqlite3", dsn(o.path, false)); err != nil {
		return
	}
	o.db.SetMaxOpenConns(1)
	if err = o.db.PingContext(ctx); err != nil {
		return
	}
	var version int
	if err = o.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return
	}
	if version < 0 || version > 1 {
		err = ErrUnavailable
		return
	}
	if version == 0 {
		var tables int
		if err = o.db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'").Scan(&tables); err != nil {
			return
		}
		if tables != 0 {
			err = ErrUnavailable
			return
		}
		if _, err = o.db.ExecContext(ctx, "PRAGMA auto_vacuum=INCREMENTAL"); err != nil {
			return
		}
		if err = o.create(ctx); err != nil {
			return
		}
	} else {
		var unclean int64
		if err = o.db.QueryRowContext(ctx, "SELECT count(*) FROM runs WHERE clean=0").Scan(&unclean); err != nil {
			return
		}
		if unclean > 0 {
			var check string
			if err = o.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil {
				return
			}
			if check != "ok" {
				err = ErrUnavailable
				return
			}
		}
	}
	var journal string
	if err = o.db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&journal); err != nil {
		return
	}
	if journal != "wal" {
		err = ErrUnavailable
		return
	}
	var syncMode int
	if err = o.db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&syncMode); err != nil {
		return
	}
	if syncMode != 2 {
		err = ErrUnavailable
		return
	}
	if _, err = o.db.ExecContext(ctx, "PRAGMA wal_autocheckpoint=256"); err != nil {
		return
	}
	if err = o.validateSchema(ctx); err != nil {
		return
	}
	if _, err = o.db.ExecContext(ctx, statsIndex); err != nil {
		return
	}
	if err = o.loadMeta(ctx); err != nil {
		return
	}
	if err = o.startRun(ctx); err != nil {
		return
	}
	var blob sql.NullInt64
	if err = o.db.QueryRowContext(ctx, "SELECT sum(stored_len) FROM pieces WHERE inline IS NULL").Scan(&blob); err != nil {
		return
	}
	o.blobBytes.Store(blob.Int64)
	if o.read, err = sql.Open("sqlite3", dsn(o.path, true)); err != nil {
		return
	}
	o.read.SetMaxOpenConns(4)
	o.read.SetMaxIdleConns(4)
	if err = o.read.PingContext(ctx); err != nil {
		return
	}
	if o.enc, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)),
		zstd.WithWindowSize(8<<20), zstd.WithEncoderConcurrency(1)); err != nil {
		return
	}
	if o.dec, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(8<<20),
		zstd.WithDecoderMaxMemory(256<<20)); err != nil {
		return
	}
	_, err = o.DiskBytes()
	return
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (o *Outbox) create(ctx context.Context) error {
	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, schema); err != nil {
		return err
	}
	now := o.opts.Now().UTC().UnixNano()
	for k, v := range map[string]string{
		"outbox_id": randomHex(16), "epoch_seen": "0", "archive_id": "", "next_seq": "1", "hw": "0",
		"created_at": strconv.FormatInt(now, 10), "replay_gap_from": "0", "replay_gap_to": "0",
		"unclean_runs": "0", "last_ack_at": "0",
	} {
		if err = put(ctx, tx, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func get(ctx context.Context, q queryer, key string) (string, error) {
	var v string
	err := q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", key).Scan(&v)
	return v, err
}

func put(ctx context.Context, e execer, key, value string) error {
	_, err := e.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
	return err
}

func getUint(ctx context.Context, q queryer, key string) (uint64, error) {
	v, err := get(ctx, q, key)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(v, 10, 64)
}

func (o *Outbox) validateSchema(ctx context.Context) error {
	for _, q := range []string{
		"SELECT seq,kind,key,created_at,state,attempts,next_attempt_at,acked_at,acked_epoch,dead_reason,dead_at,body,body_len,bytes_total FROM envelopes LIMIT 0",
		"SELECT hash,kind,content_type,raw_len,stored_len,codec,inline,last_ref_at FROM pieces LIMIT 0",
		"SELECT seq,hash FROM envelope_refs LIMIT 0",
		"SELECT seq,key,reason,state,created_at FROM tombstones LIMIT 0",
		"SELECT id,started_at,stopped_at,clean FROM runs LIMIT 0",
	} {
		rows, err := o.db.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		if err = rows.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (o *Outbox) loadMeta(ctx context.Context) error {
	var m Meta
	var err error
	if m.OutboxID, err = get(ctx, o.db, "outbox_id"); err != nil {
		return err
	}
	if len(m.OutboxID) != 32 {
		return ErrUnavailable
	}
	if m.ArchiveID, err = get(ctx, o.db, "archive_id"); err != nil {
		return err
	}
	for _, f := range []struct {
		k string
		p *uint64
	}{
		{"epoch_seen", &m.EpochSeen}, {"next_seq", &m.NextSeq}, {"hw", &m.HW},
		{"replay_gap_from", &m.ReplayGapFrom}, {"replay_gap_to", &m.ReplayGapTo}, {"unclean_runs", &m.UncleanRuns},
	} {
		if *f.p, err = getUint(ctx, o.db, f.k); err != nil {
			return err
		}
	}
	if m.NextSeq < 1 {
		return ErrUnavailable
	}
	var maxSeq sql.NullInt64
	if err = o.db.QueryRowContext(ctx, "SELECT max(seq) FROM (SELECT seq FROM envelopes UNION ALL SELECT seq FROM tombstones)").Scan(&maxSeq); err != nil {
		return err
	}
	if maxSeq.Valid && uint64(maxSeq.Int64) >= m.NextSeq {
		m.NextSeq = uint64(maxSeq.Int64) + 1
	}
	if v, err := getUint(ctx, o.db, "last_ack_at"); err == nil && v > 0 {
		m.LastAckAt = time.Unix(0, int64(v)).UTC()
	}
	o.meta = m
	return nil
}

func (o *Outbox) startRun(ctx context.Context) error {
	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var prior int64
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM runs WHERE clean=0").Scan(&prior); err != nil {
		return err
	}
	unclean, err := getUint(ctx, tx, "unclean_runs")
	if err != nil {
		return err
	}
	unclean += uint64(prior)
	if err = put(ctx, tx, "unclean_runs", strconv.FormatUint(unclean, 10)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE runs SET clean=-1 WHERE clean=0"); err != nil {
		return err
	}
	o.run = randomHex(8)
	if _, err = tx.ExecContext(ctx, "INSERT INTO runs VALUES(?,?,NULL,0)", o.run, o.opts.Now().UTC().UnixNano()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM runs WHERE id NOT IN (SELECT id FROM runs ORDER BY started_at DESC LIMIT 128)"); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	o.meta.UncleanRuns = unclean
	return nil
}

// Meta returns a copy of the persisted identity and replay state.
func (o *Outbox) Meta() Meta {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.meta
}

// DiskBytes is the size of the database and its companions.
func (o *Outbox) DiskBytes() (int64, error) {
	var size int64
	for _, suffix := range []string{"", "-wal", "-shm", "-journal", ".lock"} {
		p := o.path + suffix
		info, err := os.Lstat(p)
		if os.IsNotExist(err) && suffix != "" && suffix != ".lock" {
			continue
		}
		if err != nil {
			return 0, ErrUnavailable
		}
		if err = owned(p, false, 0o600); err != nil {
			return 0, ErrUnavailable
		}
		size += info.Size()
	}
	return size, nil
}

// TotalBytes is DiskBytes plus the blob files' stored bytes, which the outbox cap and the pressure levels count.
func (o *Outbox) TotalBytes() int64 {
	d, err := o.DiskBytes()
	if err != nil {
		return 0
	}
	return d + o.blobBytes.Load()
}

// Retryable reports SQLITE_BUSY and SQLITE_LOCKED.
func Retryable(err error) bool {
	var e sqlite.Error
	return errors.As(err, &e) && (e.Code == sqlite.ErrBusy || e.Code == sqlite.ErrLocked)
}

// Close marks the run clean and closes the databases and the lock. It is idempotent.
func (o *Outbox) Close() error {
	if o == nil || o.closed.Swap(true) {
		return nil
	}
	var err error
	if o.db != nil && o.run != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = o.db.ExecContext(ctx, "UPDATE runs SET stopped_at=?,clean=1 WHERE id=?", o.opts.Now().UTC().UnixNano(), o.run)
		_, _ = o.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
		cancel()
	}
	if o.read != nil {
		err = o.read.Close()
	}
	if o.db != nil {
		if e := o.db.Close(); err == nil {
			err = e
		}
	}
	if o.enc != nil {
		_ = o.enc.Close()
	}
	if o.dec != nil {
		o.dec.Close()
	}
	if o.lock != nil {
		if e := o.lock.Close(); err == nil {
			err = e
		}
	}
	return err
}

// Path returns the database path (tests).
func (o *Outbox) Path() string { return o.path }
