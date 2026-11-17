package uploader

import (
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
	completion := func(started time.Time, outcome string) outbox.NewEnvelope {
		body, err := canon.Marshal(wire.ExchangeCompletion{RequestID: "rid-up", SourceFormat: "openai-response", Model: "m",
			RequestedModel: "m", Outcome: outcome, StatusCode: 200, StartedAt: started, CompletedAt: started.Add(time.Second)})
		if err != nil {
			t.Fatal(err)
		}
		return outbox.NewEnvelope{Kind: wire.KindExchangeDone, Key: wire.ExchangeCompletionKey("rid-up"), CreatedAt: started, Body: body}
	}
	archived := func(key string) bool {
		for _, r := range srv.Envelopes(src) {
			if r.Envelope.Key == key && r.Status == wire.StatusOK {
				return true
			}
		}
		return false
	}
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
	run(completion(s1, "failed"), always)
	st := run(completion(s2, "succeeded"), func() bool { return archived(at) })
	keys := map[string]wire.Status{}
	for _, r := range srv.Envelopes(src) {
		keys[r.Envelope.Key] = r.Status
	}
	if keys[at] != wire.StatusOK || st.Other()["completion_rekeyed.archive"] != 1 || st.Other()["reused_request_id"] != 1 {
		t.Fatalf("archive keys %v, counters %v", keys, st.Other())
	}
	// No started_at: nothing to move it to.
	st = run(completion(time.Time{}, "failed"), always)
	if st.Other()["completion_rekeyed.archive"] != 0 {
		t.Fatal("a completion without started_at was re-keyed")
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
