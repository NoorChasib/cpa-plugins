package capture

import (
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// TestShedStreamWritesMetadataOnlyResponse (review 3 of the round-4 fixes): a stream whose payload chunks the hooks
// shed while the write queue was full (hooks.ShedStreamChunks) leaves only its header-init, so the reassembler has
// nothing to flush. The sequencer still writes the response, as metadata only (dropped body_budget) and truncated,
// at its completion and at a stop. A header-only stream that lost nothing is left as before (no response). Before, a
// shed stream left no response at all.
func TestShedStreamWritesMetadataOnlyResponse(t *testing.T) {
	r := newRig(t, rigOpts{})
	shed := func(rid string) {
		r.ba(rid, "turn")
		r.aa(rid)
		r.chunk(rid, -1, "")
		r.e.SetWriteStall(true)
		r.chunk(rid, 0, evCreated("resp_shed"))
		r.chunk(rid, 1, evItem("synthetic"))
		r.chunk(rid, 2, evDone("resp_shed"))
		r.e.SetWriteStall(false)
	}
	check := func(g got, rid string) {
		t.Helper()
		resp, ok := g.resp[wire.ExchangeResponseKey(rid, 0)]
		if !ok {
			t.Fatalf("%s: no response written; stored %v", rid, g.keys)
		}
		if resp.Body.Dropped != wire.DroppedBodyBudget || !resp.Truncated || !resp.Stream || resp.HeaderInitAt == nil || len(resp.Headers) == 0 {
			t.Fatalf("%s: response = dropped %q truncated %v stream %v header_init %v headers %d; want a metadata-only truncated stream response",
				rid, resp.Body.Dropped, resp.Truncated, resp.Stream, resp.HeaderInitAt != nil, len(resp.Headers))
		}
	}

	// At its completion.
	s1 := r.clk.step()
	shed("shed-done")
	r.complete("shed-done", "succeeded", s1, r.clk.step())
	if len(r.streams) != 1 {
		t.Fatalf("%d stream items queued, want the header-init only", len(r.streams))
	}
	r.feedStreams()
	r.feedAttempts()
	r.feedDone(s1)
	check(r.waitKeys(wire.ExchangeResponseKey("shed-done", 0), wire.ExchangeCompletionKey("shed-done")), "shed-done")

	// A header-only stream that lost nothing: no response, as before.
	s2 := r.clk.step()
	r.ba("whole", "turn")
	r.aa("whole")
	r.chunk("whole", -1, "")
	r.complete("whole", "succeeded", s2, r.clk.step())
	r.feedStreams()
	r.feedAttempts()
	r.feedDone(s2)
	if g := r.waitKeys(wire.ExchangeCompletionKey("whole")); len(g.respByRID("whole")) != 0 {
		t.Fatalf("a header-only stream that lost nothing got a response: %v", g.keys)
	}

	// At a stop, before its completion.
	shed("shed-stop")
	r.feedStreams()
	r.feedAttempts()
	r.rt.finishStreams(r.sq)
	check(r.waitKeys(wire.ExchangeResponseKey("shed-stop", 0)), "shed-stop")
}

// respByRID returns the stored responses of rid.
func (g got) respByRID(rid string) []wire.ExchangeResponse {
	var out []wire.ExchangeResponse
	for _, x := range g.resp {
		if x.RequestID == rid {
			out = append(out, x)
		}
	}
	return out
}
