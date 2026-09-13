package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-token-usage/internal/config"
	"github.com/NoorChasib/cpa-plugin-token-usage/internal/protocol"
	"github.com/NoorChasib/cpa-plugin-token-usage/internal/usage"
	sqlite "github.com/mattn/go-sqlite3"
)

var now = time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

func settings(path string) config.Config {
	return config.Config{DatabasePath: path, QueueCapacity: 64, BatchSize: 16, FlushInterval: 10 * time.Millisecond, RawRetention: time.Hour, MaintenanceInterval: time.Second, MaxDiskBytes: 1 << 30, MaxModels: 10000, QueryTimeout: time.Second}
}
func openTest(t *testing.T) (*Store, config.Config) {
	t.Helper()
	cfg := settings(filepath.Join(t.TempDir(), "private", "usage.sqlite"))
	s, _, err := Open(cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, cfg
}
func event(id, provider, model string, at time.Time) usage.Event {
	return usage.Event{ID: id, Provider: provider, Model: model, ExecutorType: "TestExecutor", Alias: "request-alias", RequestedAt: at, ReceivedAt: at, Generate: true, Tokens: protocol.UsageDetail{InputTokens: 9223372036854775807, OutputTokens: 20, TotalTokens: 41, ReasoningTokens: 2, CachedTokens: 5, CacheReadTokens: 5, CacheCreationTokens: 3}}
}
func write(t *testing.T, s *Store, events ...usage.Event) BatchResult {
	t.Helper()
	result, err := s.WriteBatch(context.Background(), events, now)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func filter(from, to time.Time) Filter { return Filter{From: from, To: to, Models: true, Limit: 100} }

func TestExactAggregationDimensionsAndLocalIdempotency(t *testing.T) {
	s, _ := openTest(t)
	a := event("1", "p", "same", now)
	b := event("2", "p", "same", now)
	b.Failed = true
	b.Status = 500
	c := event("3", "other", "same", now)
	r := write(t, s, a, b, c)
	if r.Inserted != 3 || r.Committed != "3" {
		t.Fatalf("write %+v", r)
	}
	r = write(t, s, a, b, c)
	if r.Inserted != 0 || r.Committed != "3" {
		t.Fatalf("duplicate %+v", r)
	}
	q, err := s.Query(context.Background(), filter(now, now.Add(time.Second)), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Models) != 2 || q.Totals.Observed != "3" || q.Totals.Failed != "1" || q.Totals.Tokens["input_tokens"] != "27670116110564327421" || q.Totals.Tokens["total_tokens"] != "123" {
		t.Fatalf("totals %+v", q)
	}
	f := filter(now, now.Add(time.Second))
	f.Provider = "p"
	q, err = s.Query(context.Background(), f, now.Add(time.Second))
	if err != nil || q.Totals.Tokens["input_tokens"] != "18446744073709551614" {
		t.Fatalf("overflow-safe sum %+v %v", q, err)
	}
	var alias, executor string
	if err = s.db.QueryRow("SELECT alias,executor_type FROM usage_events WHERE id='1'").Scan(&alias, &executor); err != nil || alias != "request-alias" || executor != "TestExecutor" {
		t.Fatal("provenance not preserved")
	}
}
func TestWriterStatementCacheDSN(t *testing.T) {
	for _, read := range []bool{false, true} {
		u, err := url.Parse(dsn("/tmp/usage.sqlite", read))
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		if read {
			if q.Get("_query_only") != "true" || q.Has("_stmt_cache_size") {
				t.Fatalf("reader settings changed: %v", q)
			}
		} else if q.Get("_stmt_cache_size") != "32" || q.Has("_query_only") {
			t.Fatalf("writer cache is not bounded to 32: %v", q)
		}
	}
}

func TestBatchModelCachePreservesIdentityAndCardinality(t *testing.T) {
	s, _ := openTest(t)
	s.cfg.MaxModels = 2
	write(t, s, event("seed", "p", "same", now))
	result := write(t, s,
		event("a", "p", "same", now), event("b", "p", "same", now),
		event("c", "q", "same", now), event("d", "q", "same", now),
		event("reject-1", "r", "same", now), event("reject-2", "r", "same", now),
		event("seed", "r", "different", now), // Existing ID must skip before cardinality checks.
	)
	if result.Inserted != 4 || result.Rejected != 2 || result.Committed != "5" {
		t.Fatalf("cached model admission changed: %+v", result)
	}
	q, err := s.Query(context.Background(), filter(now, now.Add(time.Second)), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Models) != 2 || q.Models[0].Provider != "p" || q.Models[0].Observed != "3" || q.Models[1].Provider != "q" || q.Models[1].Observed != "2" {
		t.Fatalf("provider/model grouping changed: %+v", q.Models)
	}
	var models int
	if err := s.db.QueryRow("SELECT count(*) FROM model_keys").Scan(&models); err != nil || models != 2 {
		t.Fatalf("model cardinality: %d %v", models, err)
	}
}

func TestBatchModelCacheDoesNotSurviveRollback(t *testing.T) {
	s, _ := openTest(t)
	a := event("a", "p", "same", now)
	bad := event("bad", "p", "same", now)
	bad.Tokens.InputTokens = -1
	if _, err := s.WriteBatch(context.Background(), []usage.Event{a, bad}, now); err == nil {
		t.Fatal("expected transaction rollback")
	}
	result := write(t, s, a)
	if result.Inserted != 1 || result.Committed != "1" {
		t.Fatalf("model cache escaped its transaction: %+v", result)
	}
	var models int
	if err := s.db.QueryRow("SELECT count(*) FROM model_keys").Scan(&models); err != nil || models != 1 {
		t.Fatalf("model key was not recreated after rollback: %d %v", models, err)
	}
}

func TestBatchRollbackAndSQLiteFull(t *testing.T) {
	s, _ := openTest(t)
	a := event("1", "p", "m", now)
	bad := event("2", "q", "n", now)
	bad.Tokens.InputTokens = -1
	if _, err := s.WriteBatch(context.Background(), []usage.Event{a, bad}, now); err == nil {
		t.Fatal("negative store value accepted")
	}
	var rows, models int
	s.db.QueryRow("SELECT count(*) FROM usage_events").Scan(&rows)
	s.db.QueryRow("SELECT count(*) FROM model_keys").Scan(&models)
	if rows != 0 || models != 0 {
		t.Fatal("partial transaction committed")
	}
	var pages int
	if err := s.db.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(fmt.Sprintf("PRAGMA max_page_count=%d", pages)); err != nil {
		t.Fatal(err)
	}
	events := make([]usage.Event, 1000)
	for i := range events {
		events[i] = event(fmt.Sprint(i), "p", "m", now)
		events[i].Alias = strings.Repeat("x", 512)
	}
	_, err := s.WriteBatch(context.Background(), events, now)
	var sqliteErr sqlite.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code != sqlite.ErrFull {
		t.Fatalf("expected real SQLite FULL, got %v", err)
	}
	s.db.QueryRow("SELECT count(*) FROM usage_events").Scan(&rows)
	if rows != 0 {
		t.Fatal("full transaction partially committed")
	}
}
func TestHalfOpenRetentionAndBoundedCleanup(t *testing.T) {
	s, cfg := openTest(t)
	start := now.Add(-30 * time.Minute)
	write(t, s, event("lower", "p", "m", start), event("upper", "p", "m", now))
	q, err := s.Query(context.Background(), filter(start, now), now)
	if err != nil || q.Totals.Observed != "1" {
		t.Fatalf("half-open %+v %v", q, err)
	}
	later := now.Add(2 * time.Hour)
	if _, err = s.Query(context.Background(), filter(start, now), later); !errors.Is(err, ErrCoverage) {
		t.Fatalf("expired query: %v", err)
	}
	many := make([]usage.Event, 600)
	for i := range many {
		many[i] = event(fmt.Sprintf("expired-%d", i), "p", "m", now)
	}
	write(t, s, many...)
	if _, err = s.Maintain(context.Background(), later); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow("SELECT count(*) FROM usage_events").Scan(&n)
	if n != 90 {
		t.Fatalf("cleanup was not bounded to 512: %d", n)
	}
	if _, err = s.Maintain(context.Background(), later); err != nil {
		t.Fatal(err)
	}
	oldFloor, err := integer(context.Background(), s.db, "retention_floor")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	cfg.RawRetention = 24 * time.Hour
	reopened, _, err := Open(cfg, later)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	f := filter(time.Unix(0, oldFloor-1), later)
	if _, err = reopened.Query(context.Background(), f, later); !errors.Is(err, ErrCoverage) {
		t.Fatal("retention extension invented old coverage")
	}
}
func TestCardinalityAndPagination(t *testing.T) {
	s, _ := openTest(t)
	s.cfg.MaxModels = 2
	r := write(t, s, event("1", "a", "m", now), event("2", "b", "m", now), event("3", "c", "m", now))
	if r.Inserted != 2 || r.Rejected != 1 {
		t.Fatalf("cardinality %+v", r)
	}
	f := filter(now, now.Add(time.Second))
	f.Limit = 1
	q, err := s.Query(context.Background(), f, now.Add(time.Second))
	if err != nil || len(q.Models) != 1 || q.Models[0].Provider != "a" || !q.HasMore {
		t.Fatalf("page %+v %v", q, err)
	}
	f.Offset = 1
	q, err = s.Query(context.Background(), f, now.Add(time.Second))
	if err != nil || q.Models[0].Provider != "b" || q.HasMore {
		t.Fatal("second page incorrect")
	}
	f.Offset = 100
	q, err = s.Query(context.Background(), f, now.Add(time.Second))
	if err != nil || len(q.Models) != 0 || q.HasMore {
		t.Fatal("past-end page incorrect")
	}
	if _, err = s.Maintain(context.Background(), now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	r = write(t, s, event("4", "new", "m", now.Add(2*time.Hour)))
	if r.Rejected != 0 {
		t.Fatal("expired model cardinality was not released")
	}
}
func TestOwnershipPermissionsCorruptionAndSchemaFailClosed(t *testing.T) {
	s, cfg := openTest(t)
	if other, _, err := Open(cfg, now); err == nil {
		other.Close()
		t.Fatal("second owner accepted")
	}
	for _, suffix := range []string{"", ".lock", "-wal", "-shm"} {
		if info, err := os.Stat(cfg.DatabasePath + suffix); err == nil && info.Mode().Perm() != 0600 {
			t.Fatalf("insecure file mode %s", suffix)
		}
	}
	s.Close()
	t.Run("newer", func(t *testing.T) {
		db, err := sql.Open("sqlite3", dsn(cfg.DatabasePath, false))
		if err != nil {
			t.Fatal(err)
		}
		db.Exec("PRAGMA user_version=99")
		db.Close()
		if other, _, err := Open(cfg, now); err == nil {
			other.Close()
			t.Fatal("newer schema accepted")
		}
	})
	for _, mode := range []string{"corrupt", "unversioned", "readonly", "directory", "symlink", "hardlink"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "private", "usage.sqlite")
			os.MkdirAll(filepath.Dir(path), 0700)
			c := settings(path)
			switch mode {
			case "corrupt":
				os.WriteFile(path, []byte("not a SQLite database"), 0600)
			case "unversioned":
				db, _ := sql.Open("sqlite3", path)
				db.Exec("CREATE TABLE unowned(x)")
				db.Close()
				os.Chmod(path, 0600)
			case "readonly":
				os.WriteFile(path, nil, 0400)
			case "directory":
				os.Chmod(filepath.Dir(path), 0755)
			case "symlink":
				target := filepath.Join(t.TempDir(), "target")
				os.WriteFile(target, nil, 0600)
				os.Symlink(target, path)
			case "hardlink":
				target := filepath.Join(filepath.Dir(path), "target")
				os.WriteFile(target, nil, 0600)
				os.Link(target, path)
			}
			if opened, _, err := Open(c, now); err == nil {
				opened.Close()
				t.Fatalf("accepted %s", mode)
			}
			if mode == "corrupt" {
				raw, _ := os.ReadFile(path)
				if string(raw) != "not a SQLite database" {
					t.Fatal("corrupt database silently replaced")
				}
			}
		})
	}
}
func TestBusyBudgetCancellationAndReadOnlyQueries(t *testing.T) {
	s, cfg := openTest(t)
	external, _ := sql.Open("sqlite3", dsn(cfg.DatabasePath, false)+"&_txlock=immediate")
	defer external.Close()
	tx, err := external.Begin()
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.WriteBatch(context.Background(), []usage.Event{event("1", "p", "m", now)}, now)
	if !Retryable(err) {
		t.Fatalf("busy not retryable: %v", err)
	}
	tx.Rollback()
	if _, err = s.read.Exec("DELETE FROM usage_events"); err == nil {
		t.Fatal("query connection permits writes")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = s.Query(ctx, filter(now, now.Add(time.Second)), now); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled query %v", err)
	}
	if err = os.Truncate(cfg.DatabasePath+".lock", cfg.MaxDiskBytes+1); err != nil {
		t.Fatal(err)
	}
	if size, err := s.DiskBytes(); err != nil || size <= cfg.MaxDiskBytes {
		t.Fatal("disk budget omitted lock/diagnostic footprint")
	}
	if _, err = s.WriteBatch(context.Background(), []usage.Event{event("2", "p", "m", now)}, now); !errors.Is(err, ErrBudget) {
		t.Fatalf("budget accepted write: %v", err)
	}
}
func TestProcessCrashAndDiagnosticsRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "usage.sqlite")
	command := exec.Command(os.Args[0], "-test.run=^TestCrashHelper$")
	command.Env = append(os.Environ(), "TOKEN_USAGE_CRASH_HELPER="+path)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("helper %v %s", err, out)
	}
	s, initial, err := Open(settings(path), now)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if initial.Committed != "1" || initial.Diagnostics["dropped_queue"] != "2" {
		t.Fatalf("recovered %+v", initial)
	}
	unclean, err := s.StartRun(context.Background(), "next", now.Add(time.Second))
	if err != nil || unclean != 1 {
		t.Fatalf("unclean %d %v", unclean, err)
	}
	if err = s.SaveDiagnostics(context.Background(), initial.Diagnostics, "next", now.Add(2*time.Second), true); err != nil {
		t.Fatal(err)
	}
	s.Close()
	again, _, err := Open(settings(path), now)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	unclean, err = again.StartRun(context.Background(), "third", now.Add(3*time.Second))
	if err != nil || unclean != 1 {
		t.Fatalf("unclean counted again %d %v", unclean, err)
	}
}
func TestCrashHelper(t *testing.T) {
	path := os.Getenv("TOKEN_USAGE_CRASH_HELPER")
	if path == "" {
		return
	}
	s, _, err := Open(settings(path), now)
	if err != nil {
		os.Exit(2)
	}
	if _, err = s.StartRun(context.Background(), "crashed", now); err != nil {
		os.Exit(3)
	}
	if _, err = s.WriteBatch(context.Background(), []usage.Event{event("1", "p", "m", now)}, now); err != nil {
		os.Exit(4)
	}
	if err = s.SaveDiagnostics(context.Background(), map[string]string{"dropped_queue": "2"}, "crashed", now, false); err != nil {
		os.Exit(5)
	}
	os.Exit(0) // Deliberately no FinishRun or Close: committed WAL must survive.
}
