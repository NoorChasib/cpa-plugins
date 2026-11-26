package capture

// K10 harness pins for design revision 3 (R4b's units): how plugin 0.1.5's stop accounts for a
// stream it cuts. Both pin current behaviour that the 0.1.6 design changes.

import (
	"testing"
	"time"
)

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
