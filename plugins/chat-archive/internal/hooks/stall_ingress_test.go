package hooks

// K10 harness pins for design revision 4 (W's ingress terms): what plugin 0.1.5's admission does
// with a burst that W's envelope and byte terms allow, and which queued stream state no budget
// charges. Both pin current behaviour; every value is synthetic.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestBurstOverHandoffShedsBodies: Handoff holds 256 entries. A burst of 300 small body-bearing
// before-auth calls, which fits W's 400-envelope burst and its byte terms, sheds the bodies of the
// 44 that find Handoff full while no worker has dequeued yet (handoff_full). Their envelopes are
// kept as metadata-only begins on Control, and their pool reservations are returned.
func TestBurstOverHandoffShedsBodies(t *testing.T) {
	e := running(t, "full")
	const burst = 300
	for i := 0; i < burst; i++ {
		e.BeforeAuth(rpc(map[string]any{"RequestID": fmt.Sprintf("burst-%d", i), "SourceFormat": "claude",
			"Body": []byte(`{"synthetic":` + fmt.Sprint(i) + `}`)}))
	}
	shed := burst - HandoffEntries
	if len(e.Handoff) != HandoffEntries || e.Stats.Dropped()["handoff_full"] != uint64(shed) {
		t.Fatalf("handoff %d of %d, handoff_full %d; want %d shed", len(e.Handoff), HandoffEntries, e.Stats.Dropped()["handoff_full"], shed)
	}
	meta := 0
	for len(e.Control) > 0 {
		if c := <-e.Control; c.Begin != nil && c.Begin.MetaOnly != "" && c.Begin.Req.Body == nil {
			meta++
		}
	}
	if meta != shed {
		t.Fatalf("%d metadata-only begins for %d shed bodies", meta, shed)
	}
	var held int64
	for len(e.Handoff) > 0 {
		held += (<-e.Handoff).Begin.Reserved
	}
	if e.Pool.Used() != held {
		t.Fatalf("pool %d, queued reservations %d: a shed body kept its reservation", e.Pool.Used(), held)
	}
}

// TestHeaderInitHeadersUncharged: a stream header-init may be as large as max-rpc-mib (it carries
// two request copies), and its decoded response headers stay on the Stream queue item, and then in
// the stream goroutine's segment, without any charge to the content pool, the stream sub-budget or
// the usage budget. W's byte sum therefore does not bound them.
func TestHeaderInitHeadersUncharged(t *testing.T) {
	e := running(t, "full")
	h := http.Header{}
	val := strings.Repeat("h", 4<<10)
	for i := 0; i < 256; i++ { // about 1 MiB of synthetic header values
		h.Add(fmt.Sprintf("X-Synthetic-%d", i), val)
	}
	e.StreamChunk(rpc(map[string]any{"RequestID": "hdr-init", "ChunkIndex": -1, "ResponseHeaders": h}))
	if len(e.Stream) != 1 {
		t.Fatalf("header-init not queued: %d items", len(e.Stream))
	}
	it := <-e.Stream
	var n int
	for k, vs := range it.Headers {
		n += len(k)
		for _, v := range vs {
			n += len(v)
		}
	}
	if n < 1<<20 {
		t.Fatalf("the queued header-init kept %d header bytes; want at least 1 MiB", n)
	}
	if e.Pool.Used() != 0 || e.Pool.StreamUsed() != 0 || e.usageBytes.Load() != 0 {
		t.Fatalf("a header-init was charged: pool %d stream %d usage %d", e.Pool.Used(), e.Pool.StreamUsed(), e.usageBytes.Load())
	}
}
