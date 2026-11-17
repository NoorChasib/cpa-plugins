package uploader

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/client/clienttest"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// TestCompletionConflictRekeyed (round 2, finding 6, archive side): the archive already holds the
// failed attempt's completion at ex/<rid>/completion, and the outbox row that would have caught the
// collision is gone. The archive answers the retry's bare completion conflict; the uploader moves
// it to completion/<started_at>, which the archive stores. A conflict on any other key is acked as
// before, and a completion without started_at stays a conflict.
func TestCompletionConflictRekeyed(t *testing.T) {
	IdleWait = 20 * time.Millisecond
	srv := clienttest.NewServer(clienttest.Options{})
	t.Cleanup(srv.Close)
	src, token := srv.AddSource("cpa-plugin", "test")
	s1 := time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)
	s2 := s1.Add(2 * time.Second)
	archived := func(key string) bool { return archivedOK(srv, src, key) }
	// run uploads env from a fresh outbox until it is drained and until holds (the re-keyed row is
	// queued only after the conflict was acked).
	run := func(env outbox.NewEnvelope, until func() bool) *hooks.Stats {
		ob, err := outbox.Open(outbox.Options{Dir: filepath.Join(t.TempDir(), "d"), CapBytes: 1 << 40, Retention: 168 * time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		defer ob.Close()
		st := hooks.NewStats()
		up := New(Options{BaseURL: srv.URL, Token: token, Outbox: ob, Stats: st})
		if _, err := ob.Write(t.Context(), []outbox.NewEnvelope{env}); err != nil {
			t.Fatal(err)
		}
		up.Start()
		defer up.Stop()
		waitFor(t, "outbox drained", func() bool { return until() && pending(t, ob) == 0 })
		return st
	}
	always := func() bool { return true }
	at := wire.ExchangeCompletionKeyAt("rid-up", s2)
	run(rekeyCompletion(t, s1, "failed", 0), always)
	st := run(rekeyCompletion(t, s2, "succeeded", 1), func() bool { return archived(at) })
	keys := map[string]wire.Status{}
	for _, r := range srv.Envelopes(src) {
		keys[r.Envelope.Key] = r.Status
	}
	if keys[at] != wire.StatusOK || st.Other()["completion_rekeyed.archive"] != 1 || st.Other()["reused_request_id"] != 1 ||
		st.Other()["lifecycle_unverified"] != 0 {
		t.Fatalf("archive keys %v, counters %v", keys, st.Other())
	}
	// No started_at: nothing to move it to.
	st = run(rekeyCompletion(t, time.Time{}, "failed", 0), always)
	if st.Other()["completion_rekeyed.archive"] != 0 {
		t.Fatal("a completion without started_at was re-keyed")
	}
}

func rekeyCompletion(t *testing.T, started time.Time, outcome string, opener uint32) outbox.NewEnvelope {
	t.Helper()
	body, err := canon.Marshal(wire.ExchangeCompletion{RequestID: "rid-up", SourceFormat: "openai-response", Model: "m",
		RequestedModel: "m", Outcome: outcome, StatusCode: 200, StartedAt: started, CompletedAt: started.Add(time.Second),
		LifecycleInputSeq: opener})
	if err != nil {
		t.Fatal(err)
	}
	return outbox.NewEnvelope{Kind: wire.KindExchangeDone, Key: wire.ExchangeCompletionKey("rid-up"), CreatedAt: started, Body: body}
}

func archivedOK(srv *clienttest.Server, src, key string) bool {
	for _, r := range srv.Envelopes(src) {
		if r.Envelope.Key == key && r.Status == wire.StatusOK {
			return true
		}
	}
	return false
}

// TestCompletionConflictRekeyFails (review round 1, finding 1): when the re-key insert fails, the
// conflict is not acked: the bare row stays pending, nothing is counted, and once inserts work again
// the conflict is answered again and the completion is uploaded at its started_at key. A completion
// without lifecycle_input_seq counts in lifecycle_unverified.
func TestCompletionConflictRekeyFails(t *testing.T) {
	IdleWait = 20 * time.Millisecond
	srv := clienttest.NewServer(clienttest.Options{})
	t.Cleanup(srv.Close)
	src, token := srv.AddSource("cpa-plugin", "test")
	s1 := time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)
	s2 := s1.Add(2 * time.Second)
	at := wire.ExchangeCompletionKeyAt("rid-up", s2)
	open := func() *outbox.Outbox {
		ob, err := outbox.Open(outbox.Options{Dir: filepath.Join(t.TempDir(), "d"), CapBytes: 1 << 40, Retention: 168 * time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ob.Close() })
		return ob
	}
	first := open()
	if _, err := first.Write(t.Context(), []outbox.NewEnvelope{rekeyCompletion(t, s1, "failed", 0)}); err != nil {
		t.Fatal(err)
	}
	up1 := New(Options{BaseURL: srv.URL, Token: token, Outbox: first, Stats: hooks.NewStats()})
	up1.Start()
	waitFor(t, "first lifecycle archived", func() bool { return pending(t, first) == 0 })
	up1.Stop()

	ob := open()
	res, err := ob.Write(t.Context(), []outbox.NewEnvelope{rekeyCompletion(t, s2, "succeeded", 0)})
	if err != nil || len(res.Inserted) != 1 {
		t.Fatal(res, err)
	}
	db, err := sql.Open("sqlite3", "file:"+ob.Path()+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_rekey BEFORE INSERT ON envelopes WHEN NEW.key GLOB 'ex/*/completion/*'
BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	st := hooks.NewStats()
	up := New(Options{BaseURL: srv.URL, Token: token, Outbox: ob, Stats: st})
	items, err := ob.Pending(t.Context(), time.Now().Add(time.Hour), 10, 1<<40)
	if err != nil || len(items) != 1 {
		t.Fatal(items, err)
	}
	conflict := wire.CommitResponse{Epoch: ob.Meta().EpochSeen, Results: []wire.CommitResult{{Seq: items[0].Seq, Status: wire.StatusConflict}}}
	if err := up.apply(t.Context(), items, conflict, nil); err != nil {
		t.Fatalf("apply = %v", err)
	}
	if again, err := ob.Pending(t.Context(), time.Now().Add(time.Hour), 10, 1<<40); err != nil || len(again) != 1 || again[0].Seq != items[0].Seq {
		t.Fatalf("after a failed re-key, pending = %+v, %v", again, err)
	}
	if st.Other()["completion_rekeyed.archive"] != 0 || st.Other()["reused_request_id"] != 0 {
		t.Fatalf("counted a failed re-key: %v", st.Other())
	}

	if _, err := db.Exec("DROP TRIGGER fail_rekey"); err != nil {
		t.Fatal(err)
	}
	up.Start()
	defer up.Stop()
	waitFor(t, "re-keyed completion archived", func() bool { return archivedOK(srv, src, at) && pending(t, ob) == 0 })
	if o := st.Other(); o["completion_rekeyed.archive"] != 1 || o["reused_request_id"] != 1 || o["lifecycle_unverified"] != 1 {
		t.Fatalf("counters %v", o)
	}
}

func TestBareCompletionKey(t *testing.T) {
	for k, want := range map[string]bool{"ex/r/completion": true, "ex/r/completion/1": false, "ex/r/begin/0": false,
		"ex//completion": false, "x/r/completion": false} {
		if bareCompletionKey(k) != want {
			t.Errorf("%q: %v", k, !want)
		}
	}
}
