package outbox

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func openTest(t *testing.T) (*Outbox, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)}
	o, err := Open(Options{Dir: filepath.Join(t.TempDir(), "data"), CapBytes: 1 << 40, Retention: 168 * time.Hour, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { o.Close() })
	return o, c
}

func attemptEnv(t *testing.T, rid string, ord uint32, pieces ...canon.Piece) NewEnvelope {
	t.Helper()
	body, err := canon.Marshal(wire.ExchangeAttempt{RequestID: rid, Ordinal: ord, ObservedAt: time.Unix(1, 0).UTC(), ToFormat: "claude", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	e := NewEnvelope{Kind: wire.KindExchangeAttempt, Key: wire.ExchangeAttemptKey(rid, ord), CreatedAt: time.Unix(1, 0), Body: body, Pieces: pieces}
	for _, p := range pieces {
		e.Refs = append(e.Refs, p.Hash)
	}
	return e
}

var ctx = context.Background()

func TestDuplicateKeySkippedWithoutSeqGap(t *testing.T) {
	o, _ := openTest(t)
	if _, err := o.Write(ctx, []NewEnvelope{attemptEnv(t, "r1", 1)}); err != nil {
		t.Fatal(err)
	}
	res, err := o.Write(ctx, []NewEnvelope{attemptEnv(t, "r2", 1), attemptEnv(t, "r1", 1), attemptEnv(t, "r3", 1)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Duplicates != 1 || len(res.Inserted) != 2 || res.Inserted[0].Seq != 2 || res.Inserted[1].Seq != 3 {
		t.Fatalf("result = %+v", res)
	}
	items, err := o.Pending(ctx, time.Now(), 100, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	for i, it := range items {
		if it.Seq != uint64(i+1) {
			t.Fatalf("seq gap: %d at %d", it.Seq, i)
		}
		env, err := wire.DecodeEnvelope(it.Envelope)
		if err != nil || env.Seq != it.Seq || env.OutboxID != o.Meta().OutboxID || env.MaskVersion != "mask/v1" {
			t.Fatalf("envelope %d: %v %+v", i, err, env)
		}
	}
	if o.Meta().NextSeq != 4 {
		t.Fatalf("next_seq = %d", o.Meta().NextSeq)
	}
}

func TestRequeueAgainstPartialUniqueIndex(t *testing.T) {
	o, _ := openTest(t)
	e := attemptEnv(t, "r1", 1)
	if _, err := o.Write(ctx, []NewEnvelope{e}); err != nil {
		t.Fatal(err)
	}
	if err := o.Apply(ctx, 1, 0, []Outcome{{Seq: 1, Kind: OutDead, Reason: string(wire.ReasonMaskViolation)}}); err != nil {
		t.Fatal(err)
	}
	dead, err := o.DeadLetters(ctx, string(wire.ReasonMaskViolation), 0, 10)
	if err != nil || len(dead) != 1 {
		t.Fatalf("dead = %v %v", dead, err)
	}
	seq, err := o.Requeue(ctx, dead[0].Seq, e)
	if err != nil || seq != 2 {
		t.Fatalf("requeue seq %d err %v", seq, err)
	}
	var state, superseded int
	var reason string
	if err := o.db.QueryRow("SELECT state,dead_reason FROM envelopes WHERE seq=1").Scan(&state, &reason); err != nil || state != StateDead || reason != DeadSuperseded {
		t.Fatalf("old row state %d reason %q err %v", state, reason, err)
	}
	if err := o.db.QueryRow("SELECT count(*) FROM envelopes WHERE key=? AND state=0", e.Key).Scan(&superseded); err != nil || superseded != 1 {
		t.Fatalf("new pending rows = %d", superseded)
	}
	if _, err := o.Requeue(ctx, 1, e); err != ErrNotDead {
		t.Fatalf("second requeue err = %v", err)
	}
	// The live key is still unique: a writer duplicate is skipped.
	if res, _ := o.Write(ctx, []NewEnvelope{e}); res.Duplicates != 1 {
		t.Fatalf("duplicate after requeue: %+v", res)
	}
	if dead, _ := o.DeadLetters(ctx, "", 0, 10); len(dead) != 0 {
		t.Fatalf("superseded listed as dead: %v", dead)
	}
}

// TestConcurrentWriteAndRequeue runs the writer's Write and the maintenance goroutine's Requeue at
// the same time: both allocate seqs, so neither may pick a seq the other took.
func TestConcurrentWriteAndRequeue(t *testing.T) {
	o, _ := openTest(t)
	const n = 200
	var dead []NewEnvelope
	for i := 0; i < n; i++ {
		dead = append(dead, attemptEnv(t, "dead", uint32(i+1)))
	}
	if _, err := o.Write(ctx, dead); err != nil {
		t.Fatal(err)
	}
	var outs []Outcome
	for i := 0; i < n; i++ {
		outs = append(outs, Outcome{Seq: uint64(i + 1), Kind: OutDead, Reason: string(wire.ReasonMaskViolation)})
	}
	if err := o.Apply(ctx, 1, 0, outs); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2*n)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < n; i++ {
			if _, err := o.Requeue(ctx, uint64(i+1), dead[i]); err != nil {
				errs <- err
			}
		}
	}()
	for i := 0; i < n; i++ {
		if _, err := o.Write(ctx, []NewEnvelope{attemptEnv(t, "live", uint32(i+1))}); err != nil {
			errs <- err
		}
	}
	<-done
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent write/requeue: %v", err)
	}
	items, err := o.Pending(ctx, time.Now(), 4*n, 1<<30)
	if err != nil || len(items) != 2*n {
		t.Fatalf("pending = %d err %v", len(items), err)
	}
	for i := 1; i < len(items); i++ {
		if items[i].Seq != items[i-1].Seq+1 {
			t.Fatalf("seq gap between %d and %d", items[i-1].Seq, items[i].Seq)
		}
	}
	if m := o.Meta(); m.NextSeq != uint64(3*n+1) {
		t.Fatalf("next_seq = %d, want %d", m.NextSeq, 3*n+1)
	}
}

func TestLocalDeadLetterTombstone(t *testing.T) {
	o, _ := openTest(t)
	o.Write(ctx, []NewEnvelope{attemptEnv(t, "r1", 1), attemptEnv(t, "r2", 1)})
	if err := o.Apply(ctx, 1, 0, []Outcome{{Seq: 1, Kind: OutDeadLocal, Reason: wire.TombRejectedTooLarge}}); err != nil {
		t.Fatal(err)
	}
	items, _ := o.Pending(ctx, time.Now(), 10, 1<<30)
	if len(items) != 2 || items[0].Tomb == nil || items[0].Tomb.Seq != 1 || items[0].Tomb.Reason != wire.TombRejectedTooLarge || items[1].Seq != 2 {
		t.Fatalf("pending = %+v", items)
	}
	if _, err := wire.DecodeTombstone(mustJSON(t, *items[0].Tomb)); err != nil {
		t.Fatalf("tombstone does not decode: %v", err)
	}
	// A tombstone is never dead-lettered; an ack retires it.
	o.Apply(ctx, 1, 0, []Outcome{{Seq: 1, Tomb: true, Kind: OutDead}})
	if items, _ = o.Pending(ctx, time.Now(), 10, 1<<30); items[0].Tomb == nil {
		t.Fatal("tombstone left the queue on a non-ack")
	}
	o.Apply(ctx, 1, 0, []Outcome{{Seq: 1, Tomb: true, Kind: OutAck}})
	if items, _ = o.Pending(ctx, time.Now(), 10, 1<<30); len(items) != 1 || items[0].Tomb != nil {
		t.Fatalf("pending after tombstone ack = %+v", items)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBlobsGCAndReplay(t *testing.T) {
	o, c := openTest(t)
	big := make([]byte, BlobThreshold+1024)
	rand.Read(big)
	bp := canon.NewPiece(wire.PieceAttachment, "image/png", big)
	tp := canon.NewPiece(wire.PieceText, "", []byte("synthetic text piece"))
	if _, err := o.Write(ctx, []NewEnvelope{attemptEnv(t, "r1", 1, bp, tp), attemptEnv(t, "r2", 1, tp)}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(o.blobPath(bp.Hash)); err != nil {
		t.Fatalf("blob file missing: %v", err)
	}
	kind, ct, raw, err := o.Piece(ctx, bp.Hash)
	if err != nil || kind != wire.PieceAttachment || ct != "image/png" || len(raw) != len(big) {
		t.Fatalf("piece read back: %v %v %q %d", err, kind, ct, len(raw))
	}
	items, _ := o.Pending(ctx, time.Now(), 10, 1<<30)
	if len(items[0].Refs) != 2 || len(items[1].Refs) != 1 {
		t.Fatalf("refs = %v / %v", items[0].Refs, items[1].Refs)
	}
	o.Apply(ctx, 1, 2, []Outcome{{Seq: 1, Kind: OutAck}, {Seq: 2, Kind: OutAck}})
	// Epoch change: every retained acked envelope is pending again; no gap.
	from, to, err := o.Replay(ctx, 2, "archive-b", 0)
	if err != nil || from != 0 || to != 0 {
		t.Fatalf("replay gap %d-%d err %v", from, to, err)
	}
	if items, _ = o.Pending(ctx, time.Now(), 10, 1<<30); len(items) != 2 {
		t.Fatalf("replayed = %d", len(items))
	}
	if m := o.Meta(); m.EpochSeen != 2 || m.ArchiveID != "archive-b" {
		t.Fatalf("meta = %+v", m)
	}
	o.Apply(ctx, 2, 2, []Outcome{{Seq: 1, Kind: OutAck}, {Seq: 2, Kind: OutAck}})
	// Past the replay window acked rows and their pieces go; the blob file is unlinked.
	c.t = c.t.Add(169 * time.Hour)
	if _, err := o.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	o.db.QueryRow("SELECT count(*) FROM envelopes").Scan(&n)
	if n != 0 {
		t.Fatalf("envelopes after GC = %d", n)
	}
	if _, err := o.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	o.db.QueryRow("SELECT count(*) FROM pieces").Scan(&n)
	if _, err := os.Stat(o.blobPath(bp.Hash)); n != 0 || !os.IsNotExist(err) {
		t.Fatalf("pieces after GC = %d, blob err %v", n, err)
	}
	// hw=0 with seq 1..2 gone: a restore to hw 0 is a replay gap.
	from, to, _ = o.Replay(ctx, 3, "archive-b", 0)
	if from != 1 || to != 2 {
		t.Fatalf("gap = %d-%d", from, to)
	}
}

func TestPendingNeverEvictedAndModes(t *testing.T) {
	o, c := openTest(t)
	o.Write(ctx, []NewEnvelope{attemptEnv(t, "r1", 1)})
	c.t = c.t.Add(10000 * time.Hour)
	o.Maintain(ctx)
	if err := o.Evict(ctx); err != nil {
		t.Fatal(err)
	}
	if items, _ := o.Pending(ctx, c.t, 10, 1<<30); len(items) != 1 {
		t.Fatal("pending envelope evicted")
	}
	s, err := o.Stats(ctx)
	if err != nil || s.Pending != 1 || s.OldestPendingAgeS <= 0 {
		t.Fatalf("stats = %+v err %v", s, err)
	}
	if v, _ := o.IntegrityCheck(ctx); v != "ok" {
		t.Fatalf("integrity = %s", v)
	}
	root := filepath.Dir(o.Path())
	o.Close()
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		info, _ := os.Lstat(p)
		want := os.FileMode(0o600)
		if d.IsDir() {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode %v", filepath.Base(p), info.Mode().Perm())
		}
		return nil
	})
	// Reopen keeps identity and seq.
	o2, err := Open(Options{Dir: root, CapBytes: 1 << 40, Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer o2.Close()
	if o2.Meta().NextSeq != 2 || o2.Meta().UncleanRuns != 0 {
		t.Fatalf("reopened meta = %+v", o2.Meta())
	}
	// A second open of the same directory is refused (flock).
	if _, err := Open(Options{Dir: root, CapBytes: 1 << 40, Retention: time.Hour}); err == nil {
		t.Fatal("second open succeeded")
	}
}

// TestStatsUsesCoveringIndex: Stats never reads table rows (bytes_total sits after the body BLOB,
// so a table scan walks every body's overflow chain), and Open adds the index to an older file.
func TestStatsUsesCoveringIndex(t *testing.T) {
	o, _ := openTest(t)
	for _, q := range []string{
		"SELECT count(*), sum(bytes_total), min(created_at) FROM envelopes INDEXED BY envelopes_stats WHERE state=0",
		"SELECT count(*), sum(bytes_total), min(acked_at) FROM envelopes INDEXED BY envelopes_stats WHERE state=2",
		"SELECT dead_reason,count(*) FROM envelopes INDEXED BY envelopes_stats WHERE state=3 AND dead_reason IS NOT 'superseded' GROUP BY dead_reason",
	} {
		rows, err := o.read.Query("EXPLAIN QUERY PLAN " + q)
		if err != nil {
			t.Fatal(err)
		}
		plan := ""
		for rows.Next() {
			var id, parent, unused int
			var detail string
			rows.Scan(&id, &parent, &unused, &detail)
			plan += detail + "; "
		}
		rows.Close()
		if !strings.Contains(plan, "COVERING INDEX envelopes_stats") {
			t.Fatalf("plan for %q = %s", q, plan)
		}
	}
	o.Write(ctx, []NewEnvelope{attemptEnv(t, "r1", 1), attemptEnv(t, "r2", 1)})
	o.Apply(ctx, 1, 0, []Outcome{{Seq: 1, Kind: OutAck}})
	s, err := o.Stats(ctx)
	if err != nil || s.Pending != 1 || s.AckedRetained != 1 || s.PendingBytes == 0 || s.AckedRetainedBytes == 0 {
		t.Fatalf("stats = %+v err %v", s, err)
	}
	// A database created before the index existed gets it on the next Open.
	root := filepath.Dir(o.Path())
	if _, err := o.db.Exec("DROP INDEX envelopes_stats"); err != nil {
		t.Fatal(err)
	}
	o.Close()
	o2, err := Open(Options{Dir: root, CapBytes: 1 << 40, Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer o2.Close()
	if s, err := o2.Stats(ctx); err != nil || s.Pending != 1 {
		t.Fatalf("stats after reopen = %+v err %v", s, err)
	}
}

func TestPendingRespectsBytes(t *testing.T) {
	o, _ := openTest(t)
	var envs []NewEnvelope
	for i := 0; i < 10; i++ {
		envs = append(envs, attemptEnv(t, "r", uint32(i+1)))
	}
	o.Write(ctx, envs)
	items, _ := o.Pending(ctx, time.Now(), 100, 3*(200+wire.CAB1RecordOverhead))
	if len(items) == 0 || len(items) >= 10 {
		t.Fatalf("byte bound ignored: %d items", len(items))
	}
}

// TestBootCountersPersist: a boot's saved counters replace its earlier record, survive a reopen (the
// crash case), are deleted by boot id, are bounded oldest first, and a bad id is refused.
func TestBootCountersPersist(t *testing.T) {
	o, _ := openTest(t)
	dir := filepath.Dir(o.Path())
	id := func(i int) string {
		return strings.Repeat("0", 30) + string("0123456789abcdef"[i/16]) + string("0123456789abcdef"[i%16])
	}
	b := BootCounters{BootID: id(1), StartedAt: time.Date(2026, 11, 14, 1, 2, 3, 0, time.UTC), Component: "cpa-plugin", Version: "0.1.0",
		N: 7, Dropped: map[string]uint64{"parse_error": 1}}
	if err := o.SaveBootCounters(ctx, b); err != nil {
		t.Fatal(err)
	}
	b.Dropped, b.N = map[string]uint64{"parse_error": 5, "stopped": 2}, 9
	if err := o.SaveBootCounters(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := o.SaveBootCounters(ctx, BootCounters{BootID: id(2), Dropped: map[string]uint64{"oversize": 1}}); err != nil {
		t.Fatal(err)
	}
	o.Close()
	o, err := Open(Options{Dir: dir, CapBytes: 1 << 40, Retention: 168 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { o.Close() })
	recs, err := o.BootCounters(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].BootID != id(1) || recs[1].BootID != id(2) {
		t.Fatalf("records = %+v", recs)
	}
	if r := recs[0]; r.N != 9 || r.Dropped["parse_error"] != 5 || r.Dropped["stopped"] != 2 || r.Version != "0.1.0" || !r.StartedAt.Equal(b.StartedAt) {
		t.Fatalf("record = %+v", r)
	}
	if err := o.DeleteBootCounters(ctx, id(1)); err != nil {
		t.Fatal(err)
	}
	if err := o.DeleteBootCounters(ctx, id(1)); err != nil {
		t.Fatal("deleting a missing record:", err)
	}
	for i := 3; i < 3+MaxBootRecords+5; i++ {
		if err := o.SaveBootCounters(ctx, BootCounters{BootID: id(i), Dropped: map[string]uint64{"oversize": uint64(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	// The newest MaxBootRecords saves are kept: id(2) and the oldest of the loop are gone.
	last := 3 + MaxBootRecords + 4
	recs, _ = o.BootCounters(ctx)
	if len(recs) != MaxBootRecords || recs[0].BootID != id(last-MaxBootRecords+1) || recs[len(recs)-1].BootID != id(last) {
		t.Fatalf("bounded records: %d, first %s, last %s", len(recs), recs[0].BootID, recs[len(recs)-1].BootID)
	}
	if m := o.Meta(); m.NextSeq != 1 || len(m.OutboxID) != 32 {
		t.Fatalf("meta disturbed: %+v", m)
	}
	for _, bad := range []string{"", "ABC", "x%", strings.Repeat("a", 49), "a_b"} {
		if err := o.SaveBootCounters(ctx, BootCounters{BootID: bad}); err != ErrBootID {
			t.Fatalf("boot id %q: %v", bad, err)
		}
	}
}

// TestCloseContextBounded: with the single write connection busy, Close waits up to CloseTimeout
// for it to mark the run clean; CloseContext returns when its context ends instead, still closes
// the databases and releases the lock, and leaves the run marked unclean.
func TestCloseContextBounded(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	o, err := Open(Options{Dir: root, CapBytes: 1 << 40, Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	busy, err := o.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	o.CloseContext(cctx)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("CloseContext took %v with a 300ms context", d)
	}
	busy.Close()
	o2, err := Open(Options{Dir: root, CapBytes: 1 << 40, Retention: time.Hour})
	if err != nil {
		t.Fatalf("reopen after a bounded close: %v", err)
	}
	defer o2.Close()
	if o2.Meta().UncleanRuns != 1 {
		t.Fatalf("unclean runs = %d, want 1", o2.Meta().UncleanRuns)
	}
}
