package hooks_test

// Engine-level cases of the 0.1.5 design for a RequestID that CPA reuses for an in-place WebSocket
// failover: completion keys, begin/attempt/response numbering and lifecycle_input_seq in every
// arrival order (O1 to O9, O2r), tombstone expiry and eviction, and the carry across a stop. The
// hooks run on a fake clock; the test plays the workers and the stream goroutine.

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

type eng struct {
	t   *testing.T
	e   *hooks.Engine
	clk *atomic.Int64
}

func newEng(t *testing.T, clk *atomic.Int64) *eng {
	if clk == nil {
		clk = new(atomic.Int64)
		clk.Store(time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC).UnixNano())
	}
	e := hooks.NewEngine()
	e.Now = func() time.Time { return time.Unix(0, clk.Load()).UTC() }
	e.SetConfig(&hooks.Config{Configured: true, Capture: "full", MaxRPC: protocol.MaxRPCDefault})
	e.Start()
	g := &eng{t: t, e: e, clk: clk}
	g.step()
	return g
}

func (g *eng) step() time.Time { return time.Unix(0, g.clk.Add(int64(time.Millisecond))).UTC() }

func (g *eng) advance(d time.Duration) { g.clk.Add(int64(d)) }

func js(v any) []byte { b, _ := json.Marshal(v); return b }

var reuseMeta = map[string]any{"request_path": "/v1/responses", "execution_session_id": "ws-eng"}

// ba runs a before-auth and plays its worker; it returns the input_seq.
func (g *eng) ba(rid string) uint32 {
	g.step()
	g.e.BeforeAuth(js(map[string]any{"RequestID": rid, "SourceFormat": "openai-response", "Model": "m", "RequestedModel": "m",
		"Body": []byte(`{"type":"response.create","input":"x"}`), "Metadata": reuseMeta}))
	w := <-g.e.Handoff
	g.e.Pool.Release(w.Begin.Reserved)
	g.e.WorkDone(rid, true, true)
	return w.Begin.InputSeq
}

func (g *eng) aa(rid string) uint32 {
	g.step()
	g.e.AfterAuth(js(map[string]any{"RequestID": rid, "Model": "m", "Metadata": reuseMeta}))
	c := <-g.e.Control
	return c.Attempt.Ordinal
}

func (g *eng) resp(rid string) uint32 {
	g.step()
	g.e.Response(js(map[string]any{"RequestID": rid, "SourceFormat": "openai-response", "Model": "m", "RequestedModel": "m",
		"Body": []byte(`{"id":"r","status":"completed"}`), "StatusCode": 200, "Metadata": reuseMeta}))
	w := <-g.e.Handoff
	g.e.Pool.Release(w.Response.Reserved)
	g.e.WorkDone(rid, false, true)
	return w.Response.Index
}

// headerInit queues a header-init and plays the stream goroutine processing it.
func (g *eng) headerInit(rid string) {
	g.step()
	g.e.StreamChunk(js(map[string]any{"RequestID": rid, "SourceFormat": "openai-response", "Model": "m", "ChunkIndex": -1, "Metadata": reuseMeta}))
	<-g.e.Stream
	g.e.ChunkDone(rid, false)
}

// take takes the completion of the lifecycle [s, c] with held stream segments still held.
func (g *eng) take(rid string, s, c time.Time, held uint32) hooks.CompletionState {
	g.t.Helper()
	g.e.SetHeld(rid, held)
	cs, ok := g.e.TakeCompletion(&hooks.Done{Req: protocol.Completion{RequestID: rid, Outcome: "failed", StartedAt: s, CompletedAt: c}})
	if !ok {
		g.t.Fatal("completion deferred")
	}
	return cs
}

type want struct {
	key    string
	alt    string
	opener uint32
}

func check(t *testing.T, what string, cs hooks.CompletionState, w want) {
	t.Helper()
	if cs.Key != w.key || cs.AltKey != w.alt || cs.LifecycleInputSeq != w.opener {
		t.Fatalf("%s: key %q alt %q opener %d; want %q %q %d", what, cs.Key, cs.AltKey, cs.LifecycleInputSeq, w.key, w.alt, w.opener)
	}
}

const rrid = "rid-eng"

func bare() string                     { return wire.ExchangeCompletionKey(rrid) }
func at(s time.Time) string            { return wire.ExchangeCompletionKeyAt(rrid, s) }
func bareFor(s time.Time) want         { return want{key: bare(), alt: at(s)} }
func later(s time.Time, n uint32) want { return want{key: at(s), opener: n} }

// TestReuseKeysPerArrivalOrder: whichever way C1's take falls against L2's hooks, L2 continues the
// numbering (begin 1, attempt 2, response 0), C1 keeps the bare key and C2 gets completion/<S2>;
// lifecycle_input_seq names begin 1 where the plugin can prove it opened L2.
func TestReuseKeysPerArrivalOrder(t *testing.T) {
	type l2nums struct{ seq, ord uint32 }
	cases := []struct {
		name string
		run  func(g *eng) (c1, c2 hooks.CompletionState, s1, s2 time.Time, n l2nums)
		c2   func(s2 time.Time) want
	}{
		{"O1_before_B2", func(g *eng) (c1, c2 hooks.CompletionState, s1, s2 time.Time, n l2nums) {
			s1 = g.step()
			g.ba(rrid)
			g.aa(rrid)
			c1 = g.take(rrid, s1, g.step(), 0)
			s2 = g.step()
			n.seq, n.ord = g.ba(rrid), g.aa(rrid)
			g.resp(rrid)
			c2 = g.take(rrid, s2, g.step(), 0)
			return
		}, func(s2 time.Time) want { return later(s2, 1) }},
		{"O2_after_B2", func(g *eng) (c1, c2 hooks.CompletionState, s1, s2 time.Time, n l2nums) {
			s1 = g.step()
			g.ba(rrid)
			g.aa(rrid)
			ct := g.step()
			s2 = g.step()
			n.seq = g.ba(rrid)
			c1 = g.take(rrid, s1, ct, 0)
			if g.e.Entries() != 1 {
				g.t.Fatal("C1 closed the entry of an active later lifecycle")
			}
			n.ord = g.aa(rrid)
			g.resp(rrid)
			c2 = g.take(rrid, s2, g.step(), 0)
			return
		}, func(s2 time.Time) want { return later(s2, 1) }},
		{"O3_after_A2", func(g *eng) (c1, c2 hooks.CompletionState, s1, s2 time.Time, n l2nums) {
			s1 = g.step()
			g.ba(rrid)
			g.aa(rrid)
			ct := g.step()
			s2 = g.step()
			n.seq, n.ord = g.ba(rrid), g.aa(rrid)
			c1 = g.take(rrid, s1, ct, 0)
			g.resp(rrid)
			c2 = g.take(rrid, s2, g.step(), 0)
			return
		}, func(s2 time.Time) want { return later(s2, 1) }},
		{"O4_held_segment", func(g *eng) (c1, c2 hooks.CompletionState, s1, s2 time.Time, n l2nums) {
			s1 = g.step()
			g.ba(rrid)
			g.aa(rrid)
			g.headerInit(rrid)
			ct := g.step()
			s2 = g.step()
			n.seq, n.ord = g.ba(rrid), g.aa(rrid)
			g.headerInit(rrid)
			c1 = g.take(rrid, s1, ct, 1) // taken while L2 streams: the sequencer still holds L2's segment
			if g.e.Entries() != 1 {
				g.t.Fatal("C1 closed the entry while a segment was held")
			}
			c2 = g.take(rrid, s2, g.step(), 0)
			return
		}, func(s2 time.Time) want { return later(s2, 1) }},
		{"O2r_race", func(g *eng) (c1, c2 hooks.CompletionState, s1, s2 time.Time, n l2nums) {
			s1 = g.step()
			g.ba(rrid)
			g.aa(rrid)
			s2 = g.step()
			n.seq = g.ba(rrid)
			c1 = g.take(rrid, s1, g.step(), 0) // B2 started before CompletedAt_1: the entry closes
			if g.e.Entries() != 0 || g.e.Tombstones() != 1 {
				g.t.Fatal("O2r: no tombstone")
			}
			n.ord = g.aa(rrid)
			g.resp(rrid)
			c2 = g.take(rrid, s2, g.step(), 0)
			return
		}, func(s2 time.Time) want { return later(s2, 1) }},
		{"O6_after_C2", func(g *eng) (c1, c2 hooks.CompletionState, s1, s2 time.Time, n l2nums) {
			s1 = g.step()
			g.ba(rrid)
			g.aa(rrid)
			ct := g.step()
			s2 = g.step()
			n.seq, n.ord = g.ba(rrid), g.aa(rrid)
			g.resp(rrid)
			c2 = g.take(rrid, s2, g.step(), 0)
			c1 = g.take(rrid, s1, ct, 0)
			return
		}, func(s2 time.Time) want { return later(s2, 1) }},
		{"O8_late_C1", func(g *eng) (c1, c2 hooks.CompletionState, s1, s2 time.Time, n l2nums) {
			s1 = g.step()
			g.ba(rrid)
			g.aa(rrid)
			s2 = g.step()
			n.seq, n.ord = g.ba(rrid), g.aa(rrid)
			g.resp(rrid)
			c1 = g.take(rrid, s1, g.step(), 0) // after every L2 hook
			c2 = g.take(rrid, s2, g.step(), 0) // from the tombstone: opener unproven
			return
		}, func(s2 time.Time) want { return later(s2, 0) }},
		{"O9_after_idle_sweep", func(g *eng) (c1, c2 hooks.CompletionState, s1, s2 time.Time, n l2nums) {
			s1 = g.step()
			g.ba(rrid)
			g.aa(rrid)
			c1 = g.take(rrid, s1, g.step(), 0)
			s2 = g.step()
			n.seq, n.ord = g.ba(rrid), g.aa(rrid)
			g.resp(rrid)
			g.advance(hooks.IdleAfter + time.Minute)
			if g.e.Sweep(g.e.Now().Add(-hooks.IdleAfter)) != 1 {
				g.t.Fatal("not swept")
			}
			c2 = g.take(rrid, s2, g.step(), 0)
			return
		}, func(s2 time.Time) want { return later(s2, 0) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newEng(t, nil)
			c1, c2, s1, s2, n := c.run(g)
			check(t, "C1", c1, bareFor(s1))
			check(t, "C2", c2, c.c2(s2))
			if n.seq != 1 || n.ord != 2 {
				t.Fatalf("L2 numbering: input_seq %d ordinal %d", n.seq, n.ord)
			}
			if !c2.Reused || c1.Reused {
				t.Fatalf("reused flags: C1 %v C2 %v", c1.Reused, c2.Reused)
			}
			if g.e.Entries() != 0 {
				t.Fatalf("%d entries left", g.e.Entries())
			}
		})
	}
}

// TestSingleLifecycleBare: a RequestID that is never reused gets the bare key, with the fallback
// set and no lifecycle_input_seq, also with duplex follow-ups.
func TestSingleLifecycleBare(t *testing.T) {
	g := newEng(t, nil)
	s := g.step()
	g.ba(rrid)
	g.aa(rrid)
	g.ba(rrid)
	g.aa(rrid)
	g.resp(rrid)
	check(t, "single", g.take(rrid, s, g.step(), 0), bareFor(s))
	if g.e.Entries() != 0 || g.e.Tombstones() != 1 {
		t.Fatalf("entries %d tombstones %d", g.e.Entries(), g.e.Tombstones())
	}
}

// sameShard returns n RequestIDs that hash to rid's shard (64 shards, FNV-1a, as the engine).
func sameShard(rid string, n int) []string {
	h := func(s string) uint32 { f := fnv.New32a(); f.Write([]byte(s)); return f.Sum32() % 64 }
	var out []string
	for i := 0; len(out) < n; i++ {
		if c := fmt.Sprintf("filler-%d", i); h(c) == h(rid) {
			out = append(out, c)
		}
	}
	return out
}

// TestLateCompletionWithNoState (round 1, finding 3): C2 is taken first and C1 arrives after the
// tombstone expired, after it was evicted from its shard, or at the next plugin instance with or
// without the carried row. C1 is bare in every case (its own lifecycle is the first).
func TestLateCompletionWithNoState(t *testing.T) {
	setup := func(t *testing.T, clk *atomic.Int64) (*eng, time.Time, time.Time) {
		g := newEng(t, clk)
		s1 := g.step()
		g.ba(rrid)
		g.aa(rrid)
		c1 := g.step()
		s2 := g.step()
		g.ba(rrid)
		g.aa(rrid)
		g.resp(rrid)
		check(t, "C2", g.take(rrid, s2, g.step(), 0), later(s2, 1))
		return g, s1, c1
	}
	t.Run("ttl", func(t *testing.T) {
		g, s1, c1 := setup(t, nil)
		g.advance(hooks.TombstoneTTL + time.Second)
		g.e.Sweep(g.e.Now().Add(-hooks.IdleAfter))
		check(t, "C1", g.take(rrid, s1, c1, 0), bareFor(s1))
	})
	t.Run("evicted", func(t *testing.T) {
		g, s1, c1 := setup(t, nil)
		for _, f := range sameShard(rrid, hooks.ClosedPerShard) {
			g.e.With(f, true, func(*hooks.Entry) {})
			g.take(f, time.Time{}, time.Time{}, 0)
		}
		if g.e.Stats.Other()["numbering_evicted"] == 0 {
			t.Fatal("nothing evicted")
		}
		check(t, "C1", g.take(rrid, s1, c1, 0), bareFor(s1))
	})
	for _, carried := range []bool{true, false} {
		t.Run(fmt.Sprintf("carry_%v", carried), func(t *testing.T) {
			clk := new(atomic.Int64)
			clk.Store(time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC).UnixNano())
			g, s1, c1 := setup(t, clk)
			g2 := newEng(t, clk)
			if carried {
				if n := g2.e.AdoptNumbering(g.e.Numbering()); n != 1 {
					t.Fatalf("adopted %d rows", n)
				}
			}
			check(t, "C1", g2.take(rrid, s1, c1, 0), bareFor(s1))
		})
	}
}

// TestCarryAcrossStop: the next plugin instance continues the numbering and the completion keys of
// a reused RequestID from the carried open request and numbering rows (O1, O4, O6 straddling the
// stop). The opener is unproven across the stop.
func TestCarryAcrossStop(t *testing.T) {
	stop := func(g *eng) *eng {
		g2 := newEng(g.t, g.clk)
		g2.e.Adopt(g.e.OpenRequests())
		g2.e.AdoptNumbering(g.e.Numbering())
		return g2
	}
	t.Run("O1", func(t *testing.T) {
		g := newEng(t, nil)
		s1 := g.step()
		g.ba(rrid)
		g.aa(rrid)
		check(t, "C1", g.take(rrid, s1, g.step(), 0), bareFor(s1))
		g = stop(g)
		s2 := g.step()
		if seq, ord := g.ba(rrid), g.aa(rrid); seq != 1 || ord != 2 {
			t.Fatalf("numbering %d %d", seq, ord)
		}
		check(t, "C2", g.take(rrid, s2, g.step(), 0), later(s2, 1))
	})
	t.Run("O4", func(t *testing.T) {
		g := newEng(t, nil)
		s1 := g.step()
		g.ba(rrid)
		g.aa(rrid)
		g.headerInit(rrid)
		s2 := g.step()
		g.ba(rrid)
		g.aa(rrid)
		g.headerInit(rrid)
		check(t, "C1", g.take(rrid, s1, g.step(), 1), bareFor(s1))
		g = stop(g)
		if idx := g.resp(rrid); idx != 0 {
			t.Fatalf("response index %d", idx)
		}
		check(t, "C2", g.take(rrid, s2, g.step(), 0), later(s2, 0))
	})
	t.Run("O6", func(t *testing.T) {
		g := newEng(t, nil)
		s1 := g.step()
		g.ba(rrid)
		g.aa(rrid)
		c1 := g.step()
		s2 := g.step()
		g.ba(rrid)
		g.aa(rrid)
		check(t, "C2", g.take(rrid, s2, g.step(), 0), later(s2, 1))
		g = stop(g)
		check(t, "C1", g.take(rrid, s1, c1, 0), bareFor(s1))
	})
}
