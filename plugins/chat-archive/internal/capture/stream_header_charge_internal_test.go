package capture

// A stream segment's decoded headers are one map shared by the segment and every response handed off
// from it. The sequencer keeps the segment after a terminal event until its completion (or a stop, or
// the idle sweep) retires it, so a worker's trim must not return the map's charge while the segment
// still holds it, and a second response from the same segment must not charge it again. These tests
// stall the writer, hand off two responses from one segment whose headers carry two 8 MiB values the
// envelopes omit, and check that the headers stay charged until both the segment and every response
// have dropped them, that they are unreachable once the charge is returned, and that every path
// returns the charge exactly once. Every value is synthetic.

import (
	"net/http"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/safego"
)

// streamHeaderRig is a runtime with no worker and no writer goroutine and an unbuffered write channel:
// a worker the test starts waits in rt.write until the test takes its envelope.
func streamHeaderRig(t *testing.T) (*Runtime, *hooks.Engine) {
	t.Helper()
	rt, e := holdRuntime(t, HoldBytes, 1)
	rt.writes = make(chan writeReq)
	e.SetConfig(&hooks.Config{Configured: true, Capture: "full", MaxRPC: protocol.MaxRPCDefault})
	e.Start()
	return rt, e
}

// queueTwoResponseSegment sends one header-init and the events of two complete responses through the
// stream hook, swaps the header-init's decoded headers for a map with two n-byte values that report to
// freed once unreachable (x-synthetic-pad-*, which no envelope keeps), and passes each queued item to
// feed. It returns the map's charge. Nothing else keeps the map.
func queueTwoResponseSegment(t *testing.T, e *hooks.Engine, rid string, n int, freed *atomic.Int32, feed func(hooks.StreamItem)) int64 {
	t.Helper()
	h := http.Header{"Content-Type": {"text/event-stream"},
		"X-Synthetic-Pad-A": {bigHeaderValue(n, freed)}, "X-Synthetic-Pad-B": {bigHeaderValue(n, freed)}}
	hb := hooks.HandoffHeaderBytes(h)
	send := func(index int, body string) {
		m := map[string]any{"RequestID": rid, "SourceFormat": "openai-response", "Model": "model-x", "RequestedModel": "model-x",
			"ChunkIndex": index, "Metadata": rigMeta}
		if index < 0 {
			m["ResponseHeaders"] = map[string][]string{"Content-Type": {"text/event-stream"}}
		} else {
			m["Body"] = []byte(body)
		}
		e.StreamChunk(rawJSON(m))
	}
	send(-1, "")
	for i, ev := range []string{evCreated("resp_a"), evItem(synthText(21, 64<<10)), evDone("resp_a"),
		evCreated("resp_b"), evItem(synthText(22, 64<<10)), evDone("resp_b")} {
		send(i, ev)
	}
	if got := len(e.Stream); got != 7 {
		t.Fatalf("%d stream items queued, want 7", got)
	}
	for len(e.Stream) > 0 {
		it := <-e.Stream
		if it.ChunkIndex == protocol.StreamHeaderInit {
			it.Headers, h = h, nil
		}
		feed(it)
	}
	return hb
}

// handedOff takes the two queued responses and checks they share one header charge, taken once.
func handedOff(t *testing.T, e *hooks.Engine, hb int64) (rs [2]*hooks.Response, bodies int64) {
	t.Helper()
	if len(e.Handoff) != 2 {
		t.Fatalf("%d responses handed off, want 2", len(e.Handoff))
	}
	for i := range rs {
		rs[i] = (<-e.Handoff).Response
		bodies += rs[i].Reserved
	}
	if rs[0].HeaderCharge == nil || rs[0].HeaderCharge != rs[1].HeaderCharge || rs[0].HeaderCharge.Bytes() != hb {
		t.Fatalf("the two responses of one segment do not share one %d-byte header charge", hb)
	}
	if used := e.Pool.Used(); used != bodies+hb {
		t.Fatalf("pool %d after two handoffs, want their bodies' %d plus one header charge %d", used, bodies, hb)
	}
	return rs, bodies
}

// blockedWorkers runs a worker for each response and waits until both trimmed their reservations and
// wait in rt.write. The headers must still be charged: the segment holds the same map.
func blockedWorkers(t *testing.T, rt *Runtime, e *hooks.Engine, rs [2]*hooks.Response, bodies, hb int64) (done chan struct{}, blocked int64) {
	t.Helper()
	done = make(chan struct{}, 2)
	for _, r := range rs {
		go func() { rt.doResponse(r); done <- struct{}{} }()
	}
	waitUntil(t, "both reservations trimmed", 10*time.Second, func() bool { return e.Pool.Used() < hb+bodies/2 })
	blocked = e.Pool.Used()
	if blocked < hb {
		t.Fatalf("pool %d with both workers waiting in rt.write, below the header charge %d the open segment still holds", blocked, hb)
	}
	return done, blocked
}

// freedWithin runs GC until want header values were freed, or d passes.
func freedWithin(freed *atomic.Int32, want int32, d time.Duration) int32 {
	for end := time.Now().Add(d); freed.Load() < want && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		runtime.GC()
	}
	return freed.Load()
}

// releaseWorkers takes every envelope the workers (and a deferred completion) offer until both
// workers returned, then checks that the pool and the hold are back at 0.
func releaseWorkers(t *testing.T, rt *Runtime, e *hooks.Engine, done chan struct{}) {
	t.Helper()
	stop := make(chan struct{})
	pumped := make(chan struct{})
	go func() {
		defer close(pumped)
		for {
			select {
			case w := <-rt.writes:
				rt.finished(w)
			case <-stop:
				return
			}
		}
	}()
	for range 2 {
		<-done
	}
	waitUntil(t, "the pool returned", 5*time.Second, func() bool { return e.Pool.Used() == 0 })
	close(stop)
	<-pumped
	balanced(t, rt, e)
}

// TestStreamHeaderChargeHeldUntilSegmentRetired: the writer is stalled with two trimmed responses of
// one segment waiting in rt.write. The segment's 16 MiB of header values stay charged while it waits
// for its completion, and are returned exactly once when the segment is retired (completion, stop,
// stream goroutine exit) or, when the responses are still queued, when the stop drains them.
func TestStreamHeaderChargeHeldUntilSegmentRetired(t *testing.T) {
	const n = 8 << 20
	const rid = "hdr-stream"
	settle := func(t *testing.T, freed *atomic.Int32, e *hooks.Engine, want int64, what string) {
		t.Helper()
		if used := e.Pool.Used(); used != want {
			t.Fatalf("pool %d after %s, want %d: the header charge was not returned exactly once", used, what, want)
		}
		if got := freedWithin(freed, 2, 5*time.Second); got != 2 {
			t.Fatalf("%d of 2 header values freed after %s returned their charge", got, what)
		}
	}

	t.Run("completion", func(t *testing.T) {
		rt, e := streamHeaderRig(t)
		sq := newSeqState()
		var freed atomic.Int32
		hb := queueTwoResponseSegment(t, e, rid, n, &freed, func(it hooks.StreamItem) { rt.onStream(sq, it) })
		rs, bodies := handedOff(t, e, hb)
		done, blocked := blockedWorkers(t, rt, e, rs, bodies, hb)
		rs = [2]*hooks.Response{}
		if got := freedWithin(&freed, 1, 200*time.Millisecond); got != 0 {
			t.Fatalf("%d header values freed while the segment still holds them", got)
		}
		e.Complete(completeRaw(rid, "succeeded", time.Time{}, time.Now()))
		rt.onControl(sq, <-e.Control)
		if sq.accs[rid] != nil {
			t.Fatal("the completion did not retire the segment")
		}
		settle(t, &freed, e, blocked-hb, "the completion retired the segment")
		releaseWorkers(t, rt, e, done)
	})

	t.Run("stop", func(t *testing.T) {
		rt, e := streamHeaderRig(t)
		sq := newSeqState()
		var freed atomic.Int32
		hb := queueTwoResponseSegment(t, e, rid, n, &freed, func(it hooks.StreamItem) { rt.onStream(sq, it) })
		rs, bodies := handedOff(t, e, hb)
		done, blocked := blockedWorkers(t, rt, e, rs, bodies, hb)
		rs = [2]*hooks.Response{}
		rt.finishStreams(sq) // a stop's drain writes the open segment and retires it
		rt.drainChannels(time.Now())
		settle(t, &freed, e, blocked-hb, "the stop retired the segment")
		releaseWorkers(t, rt, e, done)
	})

	// The retired segment's two responses are still queued: they hold the headers and their charge.
	// The first is finished by a worker (the second still holds the charge, so a second drop by the
	// segment would show here), and the stop's drain returns the second, the charge with it.
	queuedAfterRetire := func(t *testing.T, rt *Runtime, e *hooks.Engine, freed *atomic.Int32, hb, queued int64, what string) {
		t.Helper()
		if used := e.Pool.Used(); used != queued || len(e.Handoff) != 2 {
			t.Fatalf("pool %d with %d responses queued after %s, want %d (header charge %d)", used, len(e.Handoff), what, queued, hb)
		}
		if got := freedWithin(freed, 1, 200*time.Millisecond); got != 0 {
			t.Fatalf("%d header values freed while queued responses still hold them", got)
		}
		first := (<-e.Handoff).Response
		pumped := make(chan struct{})
		go func() {
			defer close(pumped)
			select {
			case w := <-rt.writes:
				rt.finished(w)
			case <-rt.drainCtx.Done():
			}
		}()
		rt.doResponse(first)
		<-pumped
		first = nil
		if used := e.Pool.Used(); used < hb {
			t.Fatalf("pool %d after the first queued response finished, below the header charge %d the second still holds", used, hb)
		}
		rt.drainChannels(time.Now())
		settle(t, freed, e, 0, "the drain returned the last queued response")
		balanced(t, rt, e)
	}

	t.Run("exit", func(t *testing.T) {
		rt, e := streamHeaderRig(t)
		var freed atomic.Int32
		var items []hooks.StreamItem
		hb := queueTwoResponseSegment(t, e, rid, n, &freed, func(it hooks.StreamItem) { items = append(items, it) })
		for i := range items { // back on the engine's queue, for the real stream goroutine
			e.Stream <- items[i]
			items[i] = hooks.StreamItem{}
		}
		safego.Loop(rt.seqCtx, &rt.seqWG, "stream", e.Stats.Panic, rt.sequencer)
		waitUntil(t, "both responses handed off", 5*time.Second, func() bool { return len(e.Handoff) == 2 && rt.ActiveStreams() == 1 })
		queued := e.Pool.Used()
		rt.drainStop() // past the drain bound: the goroutine's exit path retires the open segment
		rt.seqCancel()
		rt.seqWG.Wait()
		queuedAfterRetire(t, rt, e, &freed, hb, queued, "the stream goroutine's exit")
	})

	t.Run("queued", func(t *testing.T) {
		rt, e := streamHeaderRig(t)
		sq := newSeqState()
		var freed atomic.Int32
		hb := queueTwoResponseSegment(t, e, rid, n, &freed, func(it hooks.StreamItem) { rt.onStream(sq, it) })
		queued := e.Pool.Used()
		rt.finishStreams(sq)
		if sq.accs[rid] != nil {
			t.Fatal("finishStreams left the segment open")
		}
		queuedAfterRetire(t, rt, e, &freed, hb, queued, "the stop retired the segment")
	})
}
