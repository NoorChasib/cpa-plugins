package capture

// The 0.1.5 design's sequencer and completion-key cases for a RequestID that CPA reuses for an
// in-place WebSocket failover (L1 = the failed attempt, L2 = the retry; S = started_at, C =
// completed_at). Each test fixes the order in which the stream goroutine reads the queued items.

import (
	"reflect"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

const rid = "rid-reuse"

// l1 runs the failed attempt's synchronous hooks: before-auth, after-auth, header-init and one
// bootstrap event that never reached the client. It returns S1.
func (r *rig) l1() time.Time {
	s1 := r.clk.step()
	r.ba(rid, "turn")
	r.aa(rid)
	r.chunk(rid, -1, "")
	r.chunk(rid, 0, evCreated("resp_l1"))
	return s1
}

// l2Start runs the retry's before-auth, after-auth, header-init and first event. It returns S2.
func (r *rig) l2Start() time.Time {
	s2 := r.clk.step()
	r.ba(rid, "turn")
	r.aa(rid)
	r.chunk(rid, -1, "")
	r.chunk(rid, 0, evCreated("resp_l2"))
	return s2
}

func (r *rig) l2End() {
	r.chunk(rid, 1, evItem("retry-answer"))
	r.chunk(rid, 2, evDone("resp_l2"))
}

// checkBoth requires L1's partial under C1 (failed) and L2's answer under C2 (completed), each once,
// the completions at the bare and the started_at key, and no duplicate key.
func (r *rig) checkBoth(s1, s2 time.Time) got {
	r.t.Helper()
	bare, at2 := wire.ExchangeCompletionKey(rid), wire.ExchangeCompletionKeyAt(rid, s2)
	g := r.waitFor("both lifecycles", func(g got) bool {
		_, b := g.comp[bare]
		_, a := g.comp[at2]
		return b && a && len(g.respByID("resp_l1")) == 1 && len(g.respByID("resp_l2")) == 1
	})
	r.noDuplicates()
	if c := g.comp[bare]; !c.StartedAt.Equal(s1) || c.Outcome != "failed" {
		r.t.Fatalf("bare completion = %+v", c)
	}
	if c := g.comp[at2]; !c.StartedAt.Equal(s2) || c.Outcome != "succeeded" {
		r.t.Fatalf("started_at completion = %+v", c)
	}
	if x := g.respByID("resp_l1")[0]; x.TerminalStatus != "failed" {
		r.t.Fatalf("L1 partial: terminal_status %q", x.TerminalStatus)
	}
	if x := g.respByID("resp_l2")[0]; x.TerminalStatus != "completed" {
		r.t.Fatalf("L2 response: terminal_status %q", x.TerminalStatus)
	}
	if len(g.comp) != 2 || len(g.resp) != 2 {
		r.t.Fatalf("stored %v", g.keys)
	}
	return g
}

// TestPendingCompletionsOwnedByRequestID (round 1, finding 1): both completions are read before
// any stream item. C1 flushes at its snapshot N with L1's segment only; C2 waits in the
// RequestID's pending list (0.1.4 kept one pending completion per accumulator and lost C2 when
// C1's flush deleted it) and flushes L2's segment at M.
func TestPendingCompletionsOwnedByRequestID(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		r := newRig(t, rigOpts{})
		s1 := r.l1()
		c1 := r.clk.step()
		r.complete(rid, "failed", s1, c1) // snapshot N = 2
		s2 := r.l2Start()
		r.l2End()
		c2 := r.clk.step()
		r.complete(rid, "succeeded", s2, c2) // snapshot M = 6
		r.feedAttempts()
		if reverse {
			r.feedDone(s2)
			r.feedDone(s1)
		} else {
			r.feedDone(s1)
			r.feedDone(s2)
		}
		r.feedStream(2)
		r.waitKeys(wire.ExchangeCompletionKey(rid))
		if g := r.read(); len(g.respByID("resp_l2")) != 0 || len(g.comp) != 1 {
			t.Fatalf("reverse=%v: at N only C1 and L1's segment may be written: %v", reverse, g.keys)
		}
		r.feedStreams()
		r.checkBoth(s1, s2)
		r.settled()
	}
}

// TestSegmentPreservedInEveryOrder (round 1, finding 2): L1's buffered bootstrap event is stored,
// identically aside from its index, whether C1 is processed before L2's header-init (O3), after
// it (O4: L1's segment is parked) or after C2 (O6). In O6 the entry closes after C1.
func TestSegmentPreservedInEveryOrder(t *testing.T) {
	var l1s []wire.ExchangeResponse
	for _, order := range []string{"O3", "O4", "O6"} {
		r := newRig(t, rigOpts{})
		s1 := r.l1()
		c1 := r.clk.step()
		r.complete(rid, "failed", s1, c1)
		s2 := r.l2Start()
		r.l2End()
		c2 := r.clk.step()
		r.complete(rid, "succeeded", s2, c2)
		r.feedAttempts()
		switch order {
		case "O3":
			r.feedStream(2)
			r.feedDone(s1)
			r.feedStreams()
			r.feedDone(s2)
		case "O4":
			r.feedStream(3) // L2's header-init parks L1's segment
			r.feedDone(s1)
			r.feedStreams()
			r.feedDone(s2)
		case "O6":
			r.feedStreams()
			r.feedDone(s2)
			r.feedDone(s1)
		}
		g := r.checkBoth(s1, s2)
		r.settled()
		if n := r.counter("state_evicted"); n != 0 {
			t.Fatalf("%s: state_evicted %d", order, n)
		}
		if order != "O3" && r.counter("segments_parked") != 1 {
			t.Fatalf("%s: segments_parked %d", order, r.counter("segments_parked"))
		}
		x := g.respByID("resp_l1")[0]
		x.Index = 0
		l1s = append(l1s, x)
	}
	for i := 1; i < len(l1s); i++ {
		if !reflect.DeepEqual(l1s[0], l1s[i]) {
			t.Fatalf("L1's response differs between orders:\n%+v\n%+v", l1s[0], l1s[i])
		}
	}
}

// TestF4Ownership (round 2, finding 7): S1 < H1 < S2 < H2 < C1, with H2 processed before C1. Both
// segments fall inside C1's lifecycle span, and L2's shows two before-auths after S1, so C1 is held
// until causal evidence settles it; nothing is released.
func TestF4Ownership(t *testing.T) {
	setup := func(t *testing.T) (*rig, time.Time, time.Time) {
		r := newRig(t, rigOpts{})
		s1 := r.l1()
		s2 := r.l2Start()
		c1 := r.clk.step() // L1's goroutine completes only now
		r.complete(rid, "failed", s1, c1)
		r.feedAttempts()
		r.feedStreams() // L2's header-init parks L1's segment
		return r, s1, s2
	}
	t.Run("a_later_chunk", func(t *testing.T) {
		r, s1, s2 := setup(t)
		r.feedDone(s1)
		if r.counter("ownership_held") != 1 {
			t.Fatal("C1 was not held")
		}
		r.l2End() // L2's next event starts after C1 (F1): its segment is not C1's
		r.feedStreams()
		r.waitKeys(wire.ExchangeCompletionKey(rid))
		c2 := r.clk.step()
		r.complete(rid, "succeeded", s2, c2)
		r.feedDone(s2)
		r.checkBoth(s1, s2)
		r.settled()
	})
	t.Run("b_c2_read", func(t *testing.T) {
		r, s1, s2 := setup(t)
		r.feedDone(s1)
		r.l2End()
		c2 := r.clk.step()
		r.complete(rid, "succeeded", s2, c2)
		r.feedDone(s2) // not ready (L2's last events unprocessed), but S2 now partitions the segments
		r.waitKeys(wire.ExchangeCompletionKey(rid))
		r.feedStreams()
		r.checkBoth(s1, s2)
		r.settled()
	})
	t.Run("c_c2_first", func(t *testing.T) {
		r, s1, s2 := setup(t)
		r.l2End()
		c2 := r.clk.step()
		r.complete(rid, "succeeded", s2, c2)
		r.feedStreams()
		r.feedDone(s2)
		r.waitKeys(wire.ExchangeCompletionKeyAt(rid, s2))
		r.feedDone(s1)
		r.checkBoth(s1, s2)
		if r.counter("ownership_held") != 0 {
			t.Fatal("C1 was held although C2 had settled ownership")
		}
		r.settled()
	})
	t.Run("d_settle", func(t *testing.T) {
		r, s1, _ := setup(t)
		r.feedDone(s1)
		r.tick()
		if g := r.read(); len(g.comp) != 0 {
			t.Fatalf("settled before OwnerSettle: %v", g.keys)
		}
		r.clk.advance(OwnerSettle)
		r.tick()
		g := r.waitFor("settled C1", func(g got) bool {
			_, ok := g.comp[wire.ExchangeCompletionKey(rid)]
			return ok && len(g.respByID("resp_l1")) == 1 && len(g.respByID("resp_l2")) == 1
		})
		if r.counter("segments_uncertain") != 1 {
			t.Fatalf("segments_uncertain = %d", r.counter("segments_uncertain"))
		}
		// The newest candidate (L2's segment) keeps 0.1.4's index; L1's follows it.
		if g.respByID("resp_l2")[0].Index != 0 || g.respByID("resp_l1")[0].Index != 1 {
			t.Fatalf("indexes: L2 %d L1 %d", g.respByID("resp_l2")[0].Index, g.respByID("resp_l1")[0].Index)
		}
	})
}

// TestPrunedMarksStillPark (round 2, finding 8): five duplex follow-ups prune L2's opener from the
// entry's marks before the sequencer reads L2's header-init. The header-init carries its own
// evidence (BAAt = B2), so L1's segment is parked, not discarded, and stored once C1 is processed.
func TestPrunedMarksStillPark(t *testing.T) {
	for _, order := range []string{"O4", "O6"} {
		r := newRig(t, rigOpts{})
		s1 := r.l1()
		c1 := r.clk.step()
		r.complete(rid, "failed", s1, c1)
		s2 := r.l2Start()
		for i := 0; i < 5; i++ {
			r.ba(rid, "follow-up")
			r.aa(rid)
		}
		pruned := true
		r.e.With(rid, false, func(en *hooks.Entry) {
			for _, m := range en.Marks {
				pruned = pruned && m.Seq != 1
			}
		})
		if !pruned {
			t.Fatal("B2 was not pruned from the marks")
		}
		r.l2End()
		c2 := r.clk.step()
		r.complete(rid, "succeeded", s2, c2)
		r.feedAttempts()
		r.feedStream(3)
		if r.counter("segments_parked") != 1 {
			t.Fatalf("%s: L1's segment was not parked", order)
		}
		if order == "O4" {
			r.feedDone(s1)
			r.feedStreams()
			r.feedDone(s2)
		} else {
			r.feedStreams()
			r.feedDone(s2)
			r.feedDone(s1)
		}
		g := r.checkBoth(s1, s2)
		if c := g.comp[wire.ExchangeCompletionKeyAt(rid, s2)]; c.LifecycleInputSeq != 0 {
			t.Fatalf("%s: a pruned opener was claimed verified: %d", order, c.LifecycleInputSeq)
		}
		r.settled()
	}
}

// TestSingleLifecycleBootstrap: a RequestID that is never reused keeps 0.1.4's stream behaviour.
func TestSingleLifecycleBootstrap(t *testing.T) {
	t.Run("retry_without_before_auth", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		s := r.clk.step()
		r.ba(rid, "turn")
		r.aa(rid)
		r.chunk(rid, -1, "")
		r.chunk(rid, 0, evCreated("resp_a"))
		r.chunk(rid, -1, "") // bootstrap retry
		r.chunk(rid, 0, evCreated("resp_b"))
		r.chunk(rid, 1, evItem("answer"))
		c := r.clk.step()
		r.complete(rid, "failed", s, c)
		r.feedAttempts()
		r.feedStreams()
		r.feedDone(s)
		g := r.waitKeys(wire.ExchangeCompletionKey(rid), wire.ExchangeResponseKey(rid, 0))
		if len(g.resp) != 1 || g.resp[wire.ExchangeResponseKey(rid, 0)].ResponseID != "resp_b" ||
			g.resp[wire.ExchangeResponseKey(rid, 0)].BootstrapResets != 1 || r.counter("ownership_held") != 0 || r.counter("segments_parked") != 0 {
			t.Fatalf("stored %v resp %+v held %d", g.keys, g.resp, r.counter("ownership_held"))
		}
		r.settled()
	})
	t.Run("succeeded_duplex_follow_up_between_header_inits", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		s := r.clk.step()
		r.ba(rid, "turn")
		r.aa(rid)
		r.chunk(rid, -1, "")
		r.chunk(rid, 0, evCreated("resp_a"))
		r.ba(rid, "follow-up")
		r.aa(rid)
		r.chunk(rid, -1, "")
		r.chunk(rid, 0, evCreated("resp_b"))
		r.chunk(rid, 1, evItem("answer"))
		r.chunk(rid, 2, evDone("resp_b"))
		c := r.clk.step()
		r.complete(rid, "succeeded", s, c)
		r.feedAttempts()
		r.feedStreams()
		r.feedDone(s)
		g := r.waitKeys(wire.ExchangeCompletionKey(rid), wire.ExchangeResponseKey(rid, 0))
		x := g.resp[wire.ExchangeResponseKey(rid, 0)]
		if len(g.resp) != 1 || x.ResponseID != "resp_b" || x.BootstrapResets != 1 || r.counter("ownership_held") != 0 {
			t.Fatalf("stored %v resp %+v", g.keys, x)
		}
		if c := g.comp[wire.ExchangeCompletionKey(rid)]; c.LifecycleInputSeq != 0 {
			t.Fatalf("lifecycle_input_seq on a single lifecycle: %d", c.LifecycleInputSeq)
		}
		r.settled()
	})
	t.Run("failed_duplex_follow_up_before_header_init", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		s := r.clk.step()
		r.ba(rid, "turn")
		r.aa(rid)
		r.ba(rid, "follow-up")
		r.aa(rid)
		r.chunk(rid, -1, "")
		r.chunk(rid, 0, evCreated("resp_a"))
		c := r.clk.step()
		r.complete(rid, "failed", s, c)
		r.feedAttempts()
		r.feedStreams()
		r.feedDone(s)
		if r.counter("ownership_held") != 1 {
			t.Fatal("the completion was not held")
		}
		r.clk.advance(OwnerSettle)
		r.tick()
		g := r.waitKeys(wire.ExchangeCompletionKey(rid), wire.ExchangeResponseKey(rid, 0))
		x := g.resp[wire.ExchangeResponseKey(rid, 0)]
		if len(g.resp) != 1 || x.ResponseID != "resp_a" || x.TerminalStatus != "failed" || x.BootstrapResets != 0 {
			t.Fatalf("stored %v resp %+v", g.keys, x)
		}
		r.settled()
	})
	t.Run("failed_duplex_follow_up_between_header_inits", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		s := r.clk.step()
		r.ba(rid, "turn")
		r.aa(rid)
		r.chunk(rid, -1, "")
		r.chunk(rid, 0, evCreated("resp_a"))
		r.ba(rid, "follow-up")
		r.aa(rid)
		r.chunk(rid, -1, "")
		r.chunk(rid, 0, evCreated("resp_b"))
		c := r.clk.step()
		r.complete(rid, "failed", s, c)
		r.feedAttempts()
		r.feedStreams()
		r.feedDone(s)
		r.clk.advance(OwnerSettle)
		r.tick()
		g := r.waitKeys(wire.ExchangeCompletionKey(rid), wire.ExchangeResponseKey(rid, 0), wire.ExchangeResponseKey(rid, 1))
		// The final attempt keeps 0.1.4's index and bytes; the earlier attempt, which 0.1.4
		// released, is written after it.
		if g.resp[wire.ExchangeResponseKey(rid, 0)].ResponseID != "resp_b" || g.resp[wire.ExchangeResponseKey(rid, 0)].BootstrapResets != 1 ||
			g.resp[wire.ExchangeResponseKey(rid, 1)].ResponseID != "resp_a" || r.counter("segments_uncertain") != 1 {
			t.Fatalf("responses %+v", g.resp)
		}
		r.settled()
	})
}

// TestLateCompletionAfterRetry (round 2, finding 6): CompletedAt_1 lies after L2's last hook and
// C1 is taken first. C1 keeps the bare key and C2 is written at completion/<S2>.
func TestLateCompletionAfterRetry(t *testing.T) {
	t.Run("stream", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		s1 := r.l1()
		s2 := r.l2Start()
		r.l2End()
		c1 := r.clk.step()
		r.complete(rid, "failed", s1, c1)
		c2 := r.clk.step()
		r.complete(rid, "succeeded", s2, c2)
		r.feedAttempts()
		r.feedStreams()
		r.feedDone(s1) // held: L2's segment lies inside C1's span
		r.feedDone(s2)
		r.checkBoth(s1, s2)
		r.settled()
	})
	t.Run("non_stream", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		s1 := r.clk.step()
		r.ba(rid, "turn")
		r.aa(rid)
		s2 := r.clk.step()
		r.ba(rid, "turn")
		r.aa(rid)
		r.response(rid, "resp_l2")
		c1 := r.clk.step()
		r.complete(rid, "failed", s1, c1)
		r.feedAttempts()
		r.feedDone(s1)
		r.waitKeys(wire.ExchangeCompletionKey(rid))
		c2 := r.clk.step()
		r.complete(rid, "succeeded", s2, c2)
		r.feedDone(s2)
		g := r.waitKeys(wire.ExchangeCompletionKeyAt(rid, s2), wire.ExchangeBeginKey(rid, 1), wire.ExchangeAttemptKey(rid, 2),
			wire.ExchangeResponseKey(rid, 0))
		r.noDuplicates()
		if !g.comp[wire.ExchangeCompletionKey(rid)].StartedAt.Equal(s1) {
			t.Fatal("C1 is not at the bare key")
		}
		r.settled()
	})
}

// nonStreamL1L2 runs L1 (failed, no output) and L2's hooks, non-stream, and takes C1 first.
func (r *rig) nonStreamL1C1L2() (s1, s2 time.Time) {
	s1 = r.clk.step()
	r.ba(rid, "turn")
	r.aa(rid)
	c1 := r.clk.step()
	r.complete(rid, "failed", s1, c1)
	r.feedAttempts()
	r.feedDone(s1)
	r.waitKeys(wire.ExchangeCompletionKey(rid))
	s2 = r.clk.step()
	r.ba(rid, "turn")
	r.aa(rid)
	r.response(rid, "resp_l2")
	r.feedAttempts()
	r.waitKeys(wire.ExchangeResponseKey(rid, 0))
	return s1, s2
}

// TestCompletionAfterIdleSweep (round 2, finding 6): L2's entry is swept before C2 is delivered.
// Within the tombstone's TTL the sweep's tombstone keys C2; after it, the bare row C1 left in the
// outbox moves C2 to completion/<S2> (completion_rekeyed.outbox). The archive side is in the
// uploader's tests.
func TestCompletionAfterIdleSweep(t *testing.T) {
	for _, afterTTL := range []bool{false, true} {
		r := newRig(t, rigOpts{})
		_, s2 := r.nonStreamL1C1L2()
		r.clk.advance(hooks.IdleAfter + time.Minute)
		if n := r.e.Sweep(r.e.Now().Add(-hooks.IdleAfter)); n != 1 {
			t.Fatalf("swept %d entries", n)
		}
		if afterTTL {
			r.clk.advance(hooks.TombstoneTTL + time.Minute)
		}
		c2 := r.clk.step()
		r.complete(rid, "succeeded", s2, c2)
		r.feedDone(s2)
		g := r.waitKeys(wire.ExchangeCompletionKeyAt(rid, s2))
		r.noDuplicates()
		if len(g.comp) != 2 {
			t.Fatalf("afterTTL=%v: stored %v", afterTTL, g.keys)
		}
		if want := map[bool]uint64{false: 0, true: 1}[afterTTL]; r.counter("completion_rekeyed.outbox") != want {
			t.Fatalf("afterTTL=%v: completion_rekeyed.outbox = %d", afterTTL, r.counter("completion_rekeyed.outbox"))
		}
		// The sweep lost L2's opener either way: counted once, at build (tombstone) or at the re-key.
		if g.comp[wire.ExchangeCompletionKeyAt(rid, s2)].LifecycleInputSeq != 0 || r.counter("reused_request_id") != 1 ||
			r.counter("lifecycle_unverified") != 1 {
			t.Fatalf("afterTTL=%v: counters %v", afterTTL, r.e.Stats.Other())
		}
	}
}

// TestReverseFallback: every in-memory record is gone before C2 is taken, so C2 takes the bare key
// and C1, delivered later still, moves to completion/<S1> through the outbox.
func TestReverseFallback(t *testing.T) {
	r := newRig(t, rigOpts{})
	s1 := r.clk.step()
	r.ba(rid, "turn")
	r.aa(rid)
	s2 := r.clk.step()
	r.ba(rid, "turn")
	r.aa(rid)
	r.response(rid, "resp_l2")
	r.feedAttempts()
	r.waitKeys(wire.ExchangeResponseKey(rid, 0))
	r.clk.advance(hooks.IdleAfter + time.Minute)
	r.e.Sweep(r.e.Now().Add(-hooks.IdleAfter))
	r.clk.advance(hooks.TombstoneTTL + time.Minute)
	c2 := r.clk.step()
	r.complete(rid, "succeeded", s2, c2)
	r.feedDone(s2)
	r.waitKeys(wire.ExchangeCompletionKey(rid))
	c1 := r.clk.step()
	r.complete(rid, "failed", s1, c1)
	r.feedDone(s1)
	g := r.waitKeys(wire.ExchangeCompletionKeyAt(rid, s1))
	r.noDuplicates()
	if !g.comp[wire.ExchangeCompletionKey(rid)].StartedAt.Equal(s2) || !g.comp[wire.ExchangeCompletionKeyAt(rid, s1)].StartedAt.Equal(s1) {
		t.Fatalf("completions %+v", g.comp)
	}
	if r.counter("completion_rekeyed.outbox") != 1 || r.counter("reused_request_id") != 1 || r.counter("lifecycle_unverified") != 1 {
		t.Fatalf("counters %v", r.e.Stats.Other())
	}
}

// TestReplayBeginNamed (round 1, finding 5): the retry's replay begin is named by
// lifecycle_input_seq when the plugin observed its before-auth, even when the begin itself was not
// stored; an undecodable before-auth leaves the opener unproven and the field is omitted. A later
// duplex input always gets its own begin.
func TestReplayBeginNamed(t *testing.T) {
	cases := []struct {
		name    string
		opts    rigOpts
		replay  func(r *rig)
		wantSeq uint32
		follow  uint32 // input_seq of the duplex follow-up
	}{
		{"control_full", rigOpts{capture: "metadata"}, func(r *rig) {
			for len(r.e.Control) < cap(r.e.Control) {
				r.e.Control <- hooks.Control{}
			}
			r.ba(rid, "turn")
			r.collect()
		}, 1, 2},
		{"oversize", rigOpts{maxRPC: 4096}, func(r *rig) {
			r.ba(rid, string(make([]byte, 0))+longText(5000))
		}, 1, 2},
		{"undecodable", rigOpts{}, func(r *rig) {
			r.raw("request.intercept_before", []byte(`{"RequestID":`))
		}, 0, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, c.opts)
			s1 := r.clk.step()
			r.ba(rid, "turn")
			r.aa(rid)
			c1 := r.clk.step()
			r.complete(rid, "failed", s1, c1)
			r.feedAttempts()
			r.feedDone(s1)
			r.waitKeys(wire.ExchangeCompletionKey(rid))
			s2 := r.clk.step()
			c.replay(r)
			r.aa(rid)
			r.ba(rid, "follow-up")
			r.response(rid, "resp_l2")
			c2 := r.clk.step()
			r.complete(rid, "succeeded", s2, c2)
			r.feedAttempts()
			r.feedDone(s2)
			follow := wire.ExchangeBeginKey(rid, c.follow)
			g := r.waitKeys(wire.ExchangeCompletionKeyAt(rid, s2), follow)
			if got := g.comp[wire.ExchangeCompletionKeyAt(rid, s2)].LifecycleInputSeq; got != c.wantSeq {
				t.Fatalf("lifecycle_input_seq = %d, want %d", got, c.wantSeq)
			}
			if b := g.begin[follow]; b.InputSeq != c.follow {
				t.Fatalf("follow-up begin = %+v", b)
			}
			if c.wantSeq == 0 && r.counter("lifecycle_unverified") != 1 {
				t.Fatalf("lifecycle_unverified = %d", r.counter("lifecycle_unverified"))
			}
		})
	}
}

func longText(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}
