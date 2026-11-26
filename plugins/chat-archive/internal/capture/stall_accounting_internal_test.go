package capture

// K10 harness pins for design revisions 3 and 4 (R4b's units): how plugin 0.1.5's stop accounts for
// a stream it cuts. Each pins current behaviour that the 0.1.6 design changes.

import (
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/safego"
)

// beginsWritten waits until the rig's worker has finished every Handoff item of rid (the
// before-auth bodies): until then the drain would count a queued begin stopped, and a worker still
// in rt.write when the drain bound passes counts its begin stopped too, so a pin that measures
// stream units only must not start before. (Revision 5: this made the three pins below flaky.)
func (r *rig) beginsWritten(rid string) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		pending := int32(0)
		r.e.With(rid, false, func(en *hooks.Entry) { pending = en.Pending })
		if pending == 0 && len(r.e.Handoff) == 0 {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("the rig's worker did not finish %d Handoff items", pending)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestStopExitCountsOneStoppedPerAccumulator (revision 4): a RequestID that CPA reuses can hold two
// lifecycles' stream segments in one accumulator: the earlier lifecycle's, parked at the later
// header-init, and the later one's. When a stop's drain bound has passed, the stream goroutine's
// exit path counts dropped.stopped once per accumulator, so two lost responses count one.
func TestStopExitCountsOneStoppedPerAccumulator(t *testing.T) {
	r := newRig(t, rigOpts{})
	rid := "exit-reused"
	r.ba(rid, "synthetic-a")
	r.aa(rid)
	r.chunk(rid, -1, "")
	r.chunk(rid, 0, evCreated("resp_a"))
	r.chunk(rid, 1, evItem("synthetic"))
	r.ba(rid, "synthetic-b") // a later lifecycle under the same RequestID
	r.aa(rid)
	r.chunk(rid, -1, "")
	r.chunk(rid, 0, evCreated("resp_b"))
	r.chunk(rid, 1, evItem("synthetic"))
	r.feedAttempts()
	n := uint64(len(r.streams))
	for _, it := range r.streams { // to the real stream goroutine, in hook order
		r.e.Stream <- it
	}
	r.streams = nil
	r.beginsWritten(rid)
	before := r.e.Stats.Dropped()["stopped"]
	safego.Loop(r.rt.seqCtx, &r.rt.seqWG, "stream", r.e.Stats.Panic, r.rt.sequencer)
	deadline := time.Now().Add(5 * time.Second)
	for !r.e.ChunksProcessed(rid, n) || r.rt.ActiveStreams() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("the stream goroutine did not process the %d items", n)
		}
		time.Sleep(time.Millisecond)
	}
	if got := r.counter("segments_parked"); got != 1 {
		t.Fatalf("segments_parked %d; the earlier lifecycle's segment was not parked", got)
	}
	r.rt.drainStop() // the drain bound passed
	r.rt.seqCancel()
	r.rt.seqWG.Wait()
	if got := r.e.Stats.Dropped()["stopped"] - before; got != 1 {
		t.Fatalf("exit counted stopped %d for one accumulator holding two lifecycles' segments", got)
	}
}

// TestStopDrainCountsQueuedChunksEach: the stop's drain counts dropped.stopped once per queued
// stream item, although the items of one stream segment would have formed a single response. The
// counter's unit for a stream is the chunk, not the envelope it would have become.
func TestStopDrainCountsQueuedChunksEach(t *testing.T) {
	r := newRig(t, rigOpts{})
	rid := "drain-stream"
	r.ba(rid, "synthetic")
	r.aa(rid)
	r.chunk(rid, -1, "")
	r.chunk(rid, 0, evCreated("resp_d"))
	r.chunk(rid, 1, evItem("synthetic"))
	r.chunk(rid, 2, evDone("resp_d"))
	r.feedAttempts()
	n := len(r.streams)
	for _, it := range r.streams { // back on the engine's queue, where the hooks left them
		r.e.Stream <- it
	}
	r.streams = nil
	r.beginsWritten(rid)
	before := r.e.Stats.Dropped()["stopped"]
	r.rt.drainChannels(time.Now())
	if got := r.e.Stats.Dropped()["stopped"] - before; got != uint64(n) || n < 4 {
		t.Fatalf("drained %d stream items of one response, counted stopped %d", n, got)
	}
}

// TestStopDrainLeavesPendingCompletionUncounted: a completion the stream goroutine holds because
// its stream items are not all processed (seqState.pend) is neither written nor counted when the
// drain bound has passed: finishStreams returns before settling it, and the goroutine's exit path
// counts one stopped per open accumulator, never the held completions.
func TestStopDrainLeavesPendingCompletionUncounted(t *testing.T) {
	r := newRig(t, rigOpts{})
	rid := "drain-pend"
	st := r.ba(rid, "synthetic")
	r.aa(rid)
	r.chunk(rid, -1, "")
	r.chunk(rid, 0, evCreated("resp_p"))
	r.feedStreams()
	r.chunk(rid, 1, evItem("synthetic")) // queued, not processed
	r.complete(rid, "canceled", st, r.clk.Now())
	r.feedAttempts()
	r.feedDone(st)
	if len(r.sq.pend[rid]) != 1 {
		t.Fatalf("the completion is not held: %d pending", len(r.sq.pend[rid]))
	}
	r.beginsWritten(rid)
	before := r.e.Stats.Dropped()
	r.rt.drainStop() // the drain bound passed
	r.rt.finishStreams(r.sq)
	after := r.e.Stats.Dropped()
	if len(r.sq.pend[rid]) != 1 {
		t.Fatalf("finishStreams settled the held completion past the drain bound")
	}
	for k, v := range after {
		if v != before[k] {
			t.Fatalf("finishStreams counted %s for the held completion", k)
		}
	}
}
