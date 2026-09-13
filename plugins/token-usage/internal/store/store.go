package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/token-usage/internal/config"
	"github.com/NoorChasib/cpa-plugins/plugins/token-usage/internal/usage"
	sqlite "github.com/mattn/go-sqlite3"
)

var (
	ErrUnavailable = errors.New("storage unavailable")
	ErrCoverage    = errors.New("interval precedes retained coverage")
	ErrBudget      = errors.New("disk budget exceeded")
)

type Store struct {
	db, read *sql.DB
	lock     *os.File
	cfg      config.Config
}
type Initial struct {
	Diagnostics                   map[string]string
	Committed                     string
	Unclean                       uint64
	CoverageStart, RetentionFloor int64
	LastPersisted                 string
}
type BatchResult struct {
	Inserted, Rejected int
	Committed          string
	CoverageStart      int64
}

func owned(path string, directory bool, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm() != mode || info.Mode()&os.ModeSymlink != 0 || info.IsDir() != directory || (!directory && (!info.Mode().IsRegular() || stat.Nlink != 1)) {
		return errors.New("storage permissions or type invalid")
	}
	return nil
}
func prepare(path string) (*os.File, error) {
	dir := filepath.Dir(path)
	// Reject symlinks in all existing ancestors, not just the final component.
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
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := owned(dir, true, 0700); err != nil {
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
			if err = owned(path+suffix, false, 0600); err != nil {
				return nil, err
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("database already owned")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
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
		// go-sqlite3 v1.14.52 caches at most this many statements per connection.
		q.Set("_stmt_cache_size", "32")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func Open(cfg config.Config, now time.Time) (s *Store, initial Initial, err error) {
	s = &Store{cfg: cfg}
	defer func() {
		if err != nil {
			s.Close()
			s = nil
			err = ErrUnavailable
		}
	}()
	s.lock, err = prepare(cfg.DatabasePath)
	if err != nil {
		return
	}
	s.db, err = sql.Open("sqlite3", dsn(cfg.DatabasePath, false))
	if err != nil {
		return
	}
	s.db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err = s.db.PingContext(ctx); err != nil {
		return
	}
	var version int
	if err = s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return
	}
	if version < 0 || version > 1 {
		err = ErrUnavailable
		return
	}
	if version == 0 {
		var tables int
		if err = s.db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'").Scan(&tables); err != nil {
			return
		}
		if tables != 0 {
			err = ErrUnavailable
			return
		}
		if _, err = s.db.ExecContext(ctx, "PRAGMA auto_vacuum=INCREMENTAL"); err != nil {
			return
		}
		if err = s.migrate(ctx, now); err != nil {
			return
		}
	}
	var check string
	if err = s.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil {
		return
	}
	if check != "ok" {
		err = ErrUnavailable
		return
	}
	var journal string
	if err = s.db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&journal); err != nil {
		return
	}
	if journal != "wal" {
		err = ErrUnavailable
		return
	}
	var sync int
	if err = s.db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync); err != nil {
		return
	}
	if sync != 2 {
		err = ErrUnavailable
		return
	}
	if _, err = s.db.ExecContext(ctx, "PRAGMA wal_autocheckpoint=256"); err != nil {
		return
	}
	if err = s.validateSchema(ctx); err != nil {
		return
	}
	if initial, err = s.loadInitial(ctx); err != nil {
		return
	}
	if initial.RetentionFloor, err = s.resumeRetention(ctx, now); err != nil {
		return
	}
	s.read, err = sql.Open("sqlite3", dsn(cfg.DatabasePath, true))
	if err != nil {
		return
	}
	s.read.SetMaxOpenConns(4)
	s.read.SetMaxIdleConns(4)
	err = s.read.PingContext(ctx)
	if err == nil {
		_, err = s.DiskBytes()
	}
	return
}

func (s *Store) migrate(ctx context.Context, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `CREATE TABLE metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL);
 CREATE TABLE collection_runs(id TEXT PRIMARY KEY,started_at INTEGER NOT NULL,stopped_at INTEGER,clean INTEGER NOT NULL);
 CREATE TABLE model_keys(provider TEXT NOT NULL,model TEXT NOT NULL,PRIMARY KEY(provider,model));
 CREATE TABLE usage_events(
 id TEXT PRIMARY KEY CHECK(length(id)>0),requested_at INTEGER NOT NULL,received_at INTEGER NOT NULL,
 provider TEXT NOT NULL,executor_type TEXT NOT NULL,model TEXT NOT NULL,alias TEXT NOT NULL,
 failed INTEGER NOT NULL CHECK(failed IN (0,1)),generate INTEGER NOT NULL CHECK(generate IN (0,1)),status INTEGER NOT NULL CHECK(status BETWEEN 0 AND 599),flags INTEGER NOT NULL,
 input_tokens INTEGER NOT NULL CHECK(typeof(input_tokens)='integer' AND input_tokens>=0),
 output_tokens INTEGER NOT NULL CHECK(typeof(output_tokens)='integer' AND output_tokens>=0),
 total_tokens INTEGER NOT NULL CHECK(typeof(total_tokens)='integer' AND total_tokens>=0),
 reasoning_tokens INTEGER NOT NULL CHECK(typeof(reasoning_tokens)='integer' AND reasoning_tokens>=0),
 cached_tokens INTEGER NOT NULL CHECK(typeof(cached_tokens)='integer' AND cached_tokens>=0),
 cache_read_tokens INTEGER NOT NULL CHECK(typeof(cache_read_tokens)='integer' AND cache_read_tokens>=0),
 cache_creation_tokens INTEGER NOT NULL CHECK(typeof(cache_creation_tokens)='integer' AND cache_creation_tokens>=0));
 CREATE INDEX usage_time ON usage_events(requested_at);
 CREATE INDEX usage_model_time ON usage_events(provider,model,requested_at);
 PRAGMA user_version=1;`)
	if err != nil {
		return err
	}
	for k, v := range map[string]string{"coverage_start": strconv.FormatInt(now.UnixNano(), 10), "retention_floor": strconv.FormatInt(now.Add(-s.cfg.RawRetention).UnixNano(), 10), "committed_total": "0", "unclean_runs": "0", "diagnostics": "{}", "last_persisted": "", "retention_duration_ns": strconv.FormatInt(int64(s.cfg.RawRetention), 10)} {
		if err = put(ctx, tx, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func get(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, key string) (string, error) {
	var v string
	err := q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", key).Scan(&v)
	return v, err
}
func put(ctx context.Context, tx *sql.Tx, key, value string) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
	return err
}
func integer(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, key string) (int64, error) {
	v, err := get(ctx, q, key)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}
func (s *Store) validateSchema(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id,requested_at,received_at,provider,executor_type,model,alias,failed,generate,status,flags,input_tokens,output_tokens,total_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,cache_creation_tokens FROM usage_events LIMIT 0`)
	if err != nil {
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	var invalid int
	if err = s.db.QueryRowContext(ctx, "SELECT count(*) FROM metadata WHERE length(key)>64 OR length(value)>8192").Scan(&invalid); err != nil {
		return err
	}
	if invalid != 0 {
		return ErrUnavailable
	}
	if err = s.db.QueryRowContext(ctx, "SELECT count(*) FROM model_keys").Scan(&invalid); err != nil {
		return err
	}
	if invalid > 100000 {
		return ErrUnavailable
	}
	return nil
}

func (s *Store) loadInitial(ctx context.Context) (Initial, error) {
	var i Initial
	var err error
	if i.CoverageStart, err = integer(ctx, s.db, "coverage_start"); err != nil {
		return i, err
	}
	if i.RetentionFloor, err = integer(ctx, s.db, "retention_floor"); err != nil {
		return i, err
	}
	if i.Committed, err = get(ctx, s.db, "committed_total"); err != nil {
		return i, err
	}
	n, ok := new(big.Int).SetString(i.Committed, 10)
	if !ok || n.Sign() < 0 {
		return i, ErrUnavailable
	}
	raw, err := get(ctx, s.db, "diagnostics")
	if err != nil {
		return i, err
	}
	if json.Unmarshal([]byte(raw), &i.Diagnostics) != nil {
		return i, ErrUnavailable
	}
	for _, v := range i.Diagnostics {
		if _, err = strconv.ParseUint(v, 10, 64); err != nil {
			return i, ErrUnavailable
		}
	}
	raw, err = get(ctx, s.db, "unclean_runs")
	if err != nil {
		return i, err
	}
	if i.Unclean, err = strconv.ParseUint(raw, 10, 64); err != nil {
		return i, err
	}
	i.LastPersisted, err = get(ctx, s.db, "last_persisted")
	if err != nil {
		return i, err
	}
	if i.LastPersisted != "" {
		if _, err = time.Parse(time.RFC3339Nano, i.LastPersisted); err != nil {
			return i, ErrUnavailable
		}
	}
	for _, at := range []int64{i.CoverageStart, i.RetentionFloor} {
		year := time.Unix(0, at).Year()
		if year < 1970 || year > 2100 {
			return i, ErrUnavailable
		}
	}
	return i, nil
}

// A longer policy after restart cannot restore a range expired under the
// previously installed policy, including time spent offline.
func (s *Store) resumeRetention(ctx context.Context, now time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	prior, err := integer(ctx, tx, "retention_duration_ns")
	if err != nil || prior < int64(time.Hour) || prior > int64(8760*time.Hour) {
		return 0, ErrUnavailable
	}
	floor, err := integer(ctx, tx, "retention_floor")
	if err != nil {
		return 0, err
	}
	for _, duration := range []time.Duration{time.Duration(prior), s.cfg.RawRetention} {
		if cutoff := now.Add(-duration).UnixNano(); cutoff > floor {
			floor = cutoff
		}
	}
	if err = put(ctx, tx, "retention_floor", strconv.FormatInt(floor, 10)); err != nil {
		return 0, err
	}
	if err = put(ctx, tx, "retention_duration_ns", strconv.FormatInt(int64(s.cfg.RawRetention), 10)); err != nil {
		return 0, err
	}
	return floor, tx.Commit()
}

func (s *Store) StartRun(ctx context.Context, id string, now time.Time) (uint64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var prior int64
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM collection_runs WHERE clean=0").Scan(&prior); err != nil {
		return 0, err
	}
	raw, err := get(ctx, tx, "unclean_runs")
	if err != nil {
		return 0, err
	}
	total, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || uint64(prior) > ^uint64(0)-total {
		return 0, ErrUnavailable
	}
	total += uint64(prior)
	if err = put(ctx, tx, "unclean_runs", strconv.FormatUint(total, 10)); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE collection_runs SET clean=-1 WHERE clean=0"); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO collection_runs VALUES(?,?,NULL,0)", id, now.UnixNano()); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM collection_runs WHERE id NOT IN (SELECT id FROM collection_runs ORDER BY started_at DESC LIMIT 128)"); err != nil {
		return 0, err
	}
	return total, tx.Commit()
}

func (s *Store) WriteBatch(ctx context.Context, events []usage.Event, now time.Time) (BatchResult, error) {
	var result BatchResult
	if size, err := s.DiskBytes(); err != nil {
		return result, err
	} else if size > s.cfg.MaxDiskBytes {
		return result, ErrBudget
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var models int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM model_keys").Scan(&models); err != nil {
		return result, err
	}
	start, err := integer(ctx, tx, "coverage_start")
	if err != nil {
		return result, err
	}
	type modelKey struct{ provider, model string }
	knownModels := make(map[modelKey]struct{}) // At most one key per event in this batch.
	for _, e := range events {
		var exists int
		err = tx.QueryRowContext(ctx, "SELECT 1 FROM usage_events WHERE id=?", e.ID).Scan(&exists)
		if err == nil {
			continue
		}
		if err != sql.ErrNoRows {
			return result, err
		}
		key := modelKey{e.Provider, e.Model}
		if _, known := knownModels[key]; !known {
			err = tx.QueryRowContext(ctx, "SELECT 1 FROM model_keys WHERE provider=? AND model=?", e.Provider, e.Model).Scan(&exists)
			if err == sql.ErrNoRows {
				if models >= s.cfg.MaxModels {
					result.Rejected++
					continue
				}
				if _, err = tx.ExecContext(ctx, "INSERT INTO model_keys VALUES(?,?)", e.Provider, e.Model); err != nil {
					return result, err
				}
				models++
			} else if err != nil {
				return result, err
			}
			knownModels[key] = struct{}{}
		}
		if err = insertEvent(ctx, tx, e); err != nil {
			return result, err
		}
		result.Inserted++
		if e.RequestedAt.UnixNano() < start {
			start = e.RequestedAt.UnixNano()
		}
	}
	raw, err := get(ctx, tx, "committed_total")
	if err != nil {
		return result, err
	}
	total, ok := new(big.Int).SetString(raw, 10)
	if !ok || total.Sign() < 0 {
		return result, ErrUnavailable
	}
	total.Add(total, big.NewInt(int64(result.Inserted)))
	result.Committed = total.String()
	result.CoverageStart = start
	if err = put(ctx, tx, "committed_total", result.Committed); err != nil {
		return result, err
	}
	if err = put(ctx, tx, "coverage_start", strconv.FormatInt(start, 10)); err != nil {
		return result, err
	}
	if err = put(ctx, tx, "last_persisted", now.UTC().Format(time.RFC3339Nano)); err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func insertEvent(ctx context.Context, tx *sql.Tx, e usage.Event) error {
	d := e.Tokens
	_, err := tx.ExecContext(ctx, `INSERT INTO usage_events VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, e.ID, e.RequestedAt.UnixNano(), e.ReceivedAt.UnixNano(), e.Provider, e.ExecutorType, e.Model, e.Alias, e.Failed, e.Generate, e.Status, int64(e.Flags), d.InputTokens, d.OutputTokens, d.TotalTokens, d.ReasoningTokens, d.CachedTokens, d.CacheReadTokens, d.CacheCreationTokens)
	return err
}

// ProbeWrite checks the event insert and a write commit without using real
// observations as recovery tests. The savepoint removes the synthetic row/key;
// no counters, coverage or persistence timestamps advance. A small successful
// probe cannot guarantee that a later, larger batch fits the available space.
func (s *Store) ProbeWrite(ctx context.Context, run string, now time.Time) error {
	if size, err := s.DiskBytes(); err != nil {
		return err
	} else if size > s.cfg.MaxDiskBytes {
		return ErrBudget
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = integer(ctx, tx, "coverage_start"); err != nil {
		return err
	}
	raw, err := get(ctx, tx, "committed_total")
	if err != nil {
		return err
	}
	if n, ok := new(big.Int).SetString(raw, 10); !ok || n.Sign() < 0 {
		return ErrUnavailable
	}
	if _, err = tx.ExecContext(ctx, "SAVEPOINT write_probe"); err != nil {
		return err
	}
	// Real local IDs have a hexadecimal sequence suffix, never "probe".
	e := usage.Event{ID: run + "-probe", Provider: "token-usage-recovery", Model: run + "-probe", ExecutorType: "probe", RequestedAt: now, ReceivedAt: now, Generate: true}
	if _, err = tx.ExecContext(ctx, "INSERT INTO model_keys VALUES(?,?) ON CONFLICT DO NOTHING", e.Provider, e.Model); err != nil {
		return err
	}
	if err = insertEvent(ctx, tx, e); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "ROLLBACK TO write_probe; RELEASE write_probe"); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE metadata SET value=value WHERE key IN ('committed_total','coverage_start','last_persisted')"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SaveDiagnostics(ctx context.Context, values map[string]string, run string, now time.Time, finish bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	raw, err := json.Marshal(values)
	if err != nil {
		return err
	}
	if err = put(ctx, tx, "diagnostics", string(raw)); err != nil {
		return err
	}
	floor, err := integer(ctx, tx, "retention_floor")
	if err != nil {
		return err
	}
	if cutoff := now.Add(-s.cfg.RawRetention).UnixNano(); cutoff > floor {
		if err = put(ctx, tx, "retention_floor", strconv.FormatInt(cutoff, 10)); err != nil {
			return err
		}
	}
	if finish {
		if _, err = tx.ExecContext(ctx, "UPDATE collection_runs SET stopped_at=?,clean=1 WHERE id=?", now.UnixNano(), run); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Maintain performs one bounded chunk. A full chunk conservatively requests a
// prompt continuation; it never counts the entire expired backlog. The caller
// supplies a deadline and yields between calls, including after a full chunk.
func (s *Store) Maintain(ctx context.Context, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	old, err := integer(ctx, tx, "retention_floor")
	if err != nil {
		return false, err
	}
	floor := max(old, now.Add(-s.cfg.RawRetention).UnixNano())
	if err = put(ctx, tx, "retention_floor", strconv.FormatInt(floor, 10)); err != nil {
		return false, err
	}
	deleted, err := tx.ExecContext(ctx, "DELETE FROM usage_events WHERE id IN (SELECT id FROM usage_events WHERE requested_at<? ORDER BY requested_at LIMIT 512)", floor)
	if err != nil {
		return false, err
	}
	events, err := deleted.RowsAffected()
	if err != nil {
		return false, err
	}
	deleted, err = tx.ExecContext(ctx, "DELETE FROM model_keys WHERE rowid IN (SELECT k.rowid FROM model_keys k WHERE NOT EXISTS(SELECT 1 FROM usage_events e WHERE e.provider=k.provider AND e.model=k.model) LIMIT 512)")
	if err != nil {
		return false, err
	}
	models, err := deleted.RowsAffected()
	if err != nil {
		return false, err
	}
	more := events == 512 || models == 512
	if err = tx.Commit(); err != nil {
		return more, err
	}
	if _, err = s.db.ExecContext(ctx, "PRAGMA incremental_vacuum(128)"); err != nil {
		return more, err
	}
	// TRUNCATE can report busy while readers pin WAL pages; defer shrinking in
	// that case, without turning an otherwise successful cleanup into a fault.
	_, err = s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	return more, err
}
func (s *Store) DiskBytes() (int64, error) {
	var size int64
	for _, suffix := range []string{"", "-wal", "-shm", "-journal", ".lock"} {
		p := s.cfg.DatabasePath + suffix
		info, err := os.Lstat(p)
		if os.IsNotExist(err) && suffix != "" && suffix != ".lock" {
			continue
		}
		if err != nil {
			return 0, ErrUnavailable
		}
		if err = owned(p, false, 0600); err != nil {
			return 0, ErrUnavailable
		}
		if info.Size() > int64(^uint64(0)>>1)-size {
			return 0, ErrUnavailable
		}
		size += info.Size()
	}
	return size, nil
}
func Retryable(err error) bool {
	var e sqlite.Error
	return errors.As(err, &e) && (e.Code == sqlite.ErrBusy || e.Code == sqlite.ErrLocked)
}
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	var err error
	if s.read != nil {
		err = s.read.Close()
	}
	if s.db != nil {
		if e := s.db.Close(); err == nil {
			err = e
		}
	}
	if s.lock != nil {
		if e := s.lock.Close(); err == nil {
			err = e
		}
	}
	return err
}
func (s *Store) String() string { return fmt.Sprintf("SQLite raw store (schema %d)", 1) }
