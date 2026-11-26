package diskstall_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	sqlite "github.com/mattn/go-sqlite3"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/diskstall"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

func openOutbox(t *testing.T) (*outbox.Outbox, string) {
	t.Helper()
	if err := diskstall.Install(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ob, err := outbox.Open(outbox.Options{Dir: filepath.Join(dir, "data"), CapBytes: 1 << 40, Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ob.Close() })
	return ob, dir
}

func env(t *testing.T, rid string) outbox.NewEnvelope {
	t.Helper()
	body, err := canon.Marshal(wire.ExchangeAttempt{RequestID: rid, Ordinal: 1, ObservedAt: time.Unix(1, 0).UTC(), ToFormat: "claude", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	return outbox.NewEnvelope{Kind: wire.KindExchangeAttempt, Key: wire.ExchangeAttemptKey(rid, 1), CreatedAt: time.Unix(1, 0), Body: body}
}

func pending(t *testing.T, ob *outbox.Outbox) int {
	t.Helper()
	items, err := ob.Pending(context.Background(), time.Now(), 1000, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	return len(items)
}

// TestCountsTheOutboxCommit: an outbox write under synchronous=FULL in WAL mode writes WAL frames
// and syncs the WAL once per commit; a counting rule sees both and changes nothing.
func TestCountsTheOutboxCommit(t *testing.T) {
	ob, dir := openOutbox(t)
	w := diskstall.Arm(diskstall.Rule{Dir: dir, Ops: diskstall.Write, Files: diskstall.WAL})
	defer w.Disarm()
	s := diskstall.Arm(diskstall.Rule{Dir: dir, Ops: diskstall.Sync, Files: diskstall.WAL})
	defer s.Disarm()
	if _, err := ob.Write(context.Background(), []outbox.NewEnvelope{env(t, "ds-count")}); err != nil {
		t.Fatal(err)
	}
	if got := s.Stats(); got.Affected != 1 || got.Waited != 0 {
		t.Fatalf("WAL syncs %+v, want exactly one commit sync and no wait", got)
	}
	if got := w.Stats(); got.Affected == 0 {
		t.Fatal("no WAL write seen")
	}
	if pending(t, ob) != 1 {
		t.Fatal("the counted write did not store its envelope")
	}
}

// TestFailedSyncFailsTheWrite: a WAL fsync that returns SQLITE_IOERR_FSYNC fails the outbox write
// with that error and stores nothing; the next write succeeds.
func TestFailedSyncFailsTheWrite(t *testing.T) {
	ob, dir := openOutbox(t)
	h := diskstall.Arm(diskstall.Rule{Dir: dir, Ops: diskstall.Sync, Files: diskstall.WAL, Count: 1, Fail: diskstall.IOErrFsync})
	defer h.Disarm()
	_, err := ob.Write(context.Background(), []outbox.NewEnvelope{env(t, "ds-fail")})
	var se sqlite.Error
	if !errors.As(err, &se) || se.Code != sqlite.ErrIoErr {
		t.Fatalf("write error = %v, want SQLITE_IOERR", err)
	}
	if _, err := ob.Write(context.Background(), []outbox.NewEnvelope{env(t, "ds-after")}); err != nil {
		t.Fatalf("the write after the failure: %v", err)
	}
	if n := pending(t, ob); n != 1 {
		t.Fatalf("pending = %d, want only the second envelope", n)
	}
}

// TestHeldSyncIgnoresTheWriteContext: a commit whose WAL fsync stalls past the write's context
// deadline still waits for the disk (SQLite's commit cannot be interrupted) and then commits: the
// caller is blocked for the whole stall, not for its timeout.
func TestHeldSyncIgnoresTheWriteContext(t *testing.T) {
	ob, dir := openOutbox(t)
	hold := make(chan struct{})
	h := diskstall.Arm(diskstall.Rule{Dir: dir, Ops: diskstall.Sync, Files: diskstall.WAL, Count: 1, Hold: hold})
	defer h.Disarm()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := ob.Write(ctx, []outbox.NewEnvelope{env(t, "ds-hold")})
		done <- err
	}()
	if !h.WaitFor(5*time.Second, func(s diskstall.Stats) bool { return s.Waiting == 1 }) {
		t.Fatal("the commit never reached the held sync")
	}
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("write returned during the stall: %v", err)
	default:
	}
	close(hold)
	err := <-done
	if err != nil {
		t.Fatalf("write after the stall: %v", err)
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Fatalf("write took %v", d)
	}
	if n := pending(t, ob); n != 1 {
		t.Fatalf("pending = %d, want the envelope committed after the stall", n)
	}
}

// TestRulesStayInTheirDirectory: a rule armed for one outbox's directory leaves another's I/O
// alone.
func TestRulesStayInTheirDirectory(t *testing.T) {
	_, dirA := openOutbox(t)
	obB, _ := openOutbox(t)
	h := diskstall.Arm(diskstall.Rule{Dir: dirA, Fail: diskstall.IOErr})
	defer h.Disarm()
	if _, err := obB.Write(context.Background(), []outbox.NewEnvelope{env(t, "ds-other")}); err != nil {
		t.Fatal(err)
	}
	if h.Stats().Matched != 0 {
		t.Fatalf("rule matched %d operations in another directory", h.Stats().Matched)
	}
}

// TestDelayAndSkip: Skip lets the first matching operations through and Delay stalls the next
// Count of them for its duration.
func TestDelayAndSkip(t *testing.T) {
	ob, dir := openOutbox(t)
	h := diskstall.Arm(diskstall.Rule{Dir: dir, Ops: diskstall.Sync, Files: diskstall.WAL, Skip: 1, Count: 1, Delay: 200 * time.Millisecond})
	defer h.Disarm()
	for i, rid := range []string{"ds-d1", "ds-d2", "ds-d3"} {
		start := time.Now()
		if _, err := ob.Write(context.Background(), []outbox.NewEnvelope{env(t, rid)}); err != nil {
			t.Fatal(err)
		}
		d := time.Since(start)
		if stalled := d >= 200*time.Millisecond; stalled != (i == 1) {
			t.Fatalf("write %d took %v", i, d)
		}
	}
	if s := h.Stats(); s.Matched != 3 || s.Affected != 1 || s.MaxWait < 200*time.Millisecond {
		t.Fatalf("stats %+v", s)
	}
}

// TestProbeWriteTouchesNoFile pins what the capture writer's write probe (used while
// write_unavailable is raised) does at the disk: its UPDATE sets a value to itself, SQLite skips
// the unchanged cell, and the commit writes and syncs nothing. The probe therefore proves the write
// connection and lock are free, not that the disk takes writes: a WAL that still fails its fsync
// passes the probe.
func TestProbeWriteTouchesNoFile(t *testing.T) {
	ob, dir := openOutbox(t)
	h := diskstall.Arm(diskstall.Rule{Dir: dir, Ops: diskstall.Write | diskstall.Sync, Fail: diskstall.IOErr})
	defer h.Disarm()
	if err := ob.ProbeWrite(context.Background()); err != nil {
		t.Fatalf("probe on a failing disk: %v", err)
	}
	if n := h.Stats().Matched; n != 0 {
		t.Fatalf("the probe wrote or synced %d times", n)
	}
	if _, err := ob.Write(context.Background(), []outbox.NewEnvelope{env(t, "ds-probe")}); err == nil {
		t.Fatal("a real write passed the failing disk")
	}
}
