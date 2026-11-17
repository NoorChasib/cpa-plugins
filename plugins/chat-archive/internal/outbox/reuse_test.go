package outbox

// The outbox side of 0.1.5's completion keys for a RequestID CPA reuses: the AltKey fallback at
// write and the archive-conflict re-key.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

func completionEnv(t *testing.T, rid string, started time.Time, outcome string, alt bool) NewEnvelope {
	t.Helper()
	body, err := canon.Marshal(wire.ExchangeCompletion{RequestID: rid, SourceFormat: "openai-response", Model: "m", RequestedModel: "m",
		Outcome: outcome, StatusCode: 200, StartedAt: started, CompletedAt: started.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	e := NewEnvelope{Kind: wire.KindExchangeDone, Key: wire.ExchangeCompletionKey(rid), CreatedAt: started.Add(time.Second), Body: body}
	if alt && !started.IsZero() {
		e.AltKey = wire.ExchangeCompletionKeyAt(rid, started)
	}
	return e
}

func openReuse(t *testing.T) *Outbox {
	t.Helper()
	o, err := Open(Options{Dir: filepath.Join(t.TempDir(), "data"), CapBytes: 1 << 40, Retention: 168 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { o.Close() })
	return o
}

func storedKeys(t *testing.T, o *Outbox) map[string]wire.Envelope {
	t.Helper()
	items, err := o.Pending(context.Background(), time.Now().Add(time.Hour), 100, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]wire.Envelope{}
	for _, it := range items {
		env, err := wire.DecodeEnvelope(it.Envelope)
		if err != nil {
			t.Fatal(err)
		}
		if env.Key != it.Key {
			t.Fatalf("row key %q, envelope key %q", it.Key, env.Key)
		}
		out[env.Key] = env
	}
	return out
}

// TestAltKeyFallback: a bare completion whose key is already queued (another lifecycle's) is
// inserted at its AltKey in the same write, with the envelope's key rebuilt; the same completion
// written again is a duplicate.
func TestAltKeyFallback(t *testing.T) {
	ctx := context.Background()
	o := openReuse(t)
	s1 := time.Date(2026, 11, 18, 5, 0, 0, 0, time.UTC)
	s2 := s1.Add(time.Second)
	if res, err := o.Write(ctx, []NewEnvelope{completionEnv(t, "r", s1, "failed", true)}); err != nil || len(res.Inserted) != 1 || res.Rekeyed != 0 {
		t.Fatalf("first write = %+v, %v", res, err)
	}
	c2 := completionEnv(t, "r", s2, "succeeded", true)
	res, err := o.Write(ctx, []NewEnvelope{c2})
	if err != nil || res.Rekeyed != 1 || res.Duplicates != 0 || len(res.Inserted) != 1 || res.Inserted[0].Key != c2.AltKey {
		t.Fatalf("second write = %+v, %v", res, err)
	}
	if res, err := o.Write(ctx, []NewEnvelope{c2}); err != nil || res.Duplicates != 1 || res.Rekeyed != 0 {
		t.Fatalf("repeat = %+v, %v", res, err)
	}
	got := storedKeys(t, o)
	var c wire.ExchangeCompletion
	json.Unmarshal(got[c2.AltKey].Body, &c)
	if len(got) != 2 || !c.StartedAt.Equal(s2) {
		t.Fatalf("stored %v", got)
	}
	// Without an AltKey the collision stays a duplicate (0.1.4).
	if res, err := o.Write(ctx, []NewEnvelope{completionEnv(t, "r", s2.Add(time.Second), "failed", false)}); err != nil || res.Duplicates != 1 {
		t.Fatalf("no alt = %+v, %v", res, err)
	}
}

// TestRekey: a bare completion the archive answered conflict for is queued again at its
// started_at key, once; one without started_at, or another kind, is left alone.
func TestRekey(t *testing.T) {
	ctx := context.Background()
	o := openReuse(t)
	s := time.Date(2026, 11, 18, 5, 0, 0, 0, time.UTC)
	res, err := o.Write(ctx, []NewEnvelope{completionEnv(t, "r", s, "succeeded", false), completionEnv(t, "z", time.Time{}, "failed", false)})
	if err != nil || len(res.Inserted) != 2 {
		t.Fatal(res, err)
	}
	if ok, err := o.Rekey(ctx, res.Inserted[0].Seq); !ok || err != nil {
		t.Fatalf("rekey = %v, %v", ok, err)
	}
	if ok, err := o.Rekey(ctx, res.Inserted[0].Seq); ok || err != nil {
		t.Fatalf("second rekey = %v, %v", ok, err)
	}
	if ok, err := o.Rekey(ctx, res.Inserted[1].Seq); ok || err != nil {
		t.Fatalf("rekey without started_at = %v, %v", ok, err)
	}
	got := storedKeys(t, o)
	at := wire.ExchangeCompletionKeyAt("r", s)
	if len(got) != 3 || string(got[at].Body) != string(got[wire.ExchangeCompletionKey("r")].Body) {
		t.Fatalf("stored %v", got)
	}
}
