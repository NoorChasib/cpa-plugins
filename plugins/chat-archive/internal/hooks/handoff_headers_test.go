package hooks

// P4a (plugin 0.1.6): a Handoff item's decoded headers are charged to its content-pool reservation
// with its body, so the bytes a queued body keeps while the writer is held stay inside the pool.
// Every value is synthetic.

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func syntheticHeaders(n, size int) http.Header {
	h := http.Header{}
	val := strings.Repeat("h", size)
	for i := 0; i < n; i++ {
		h.Add(fmt.Sprintf("X-Synthetic-%d", i), val)
	}
	return h
}

// decodedLen is the body length the hooks reserve for (protocol.Base64Len / 4 * 3).
func decodedLen(body []byte) int64 {
	return int64(base64.StdEncoding.DecodedLen(base64.StdEncoding.EncodedLen(len(body))))
}

// TestHandoffHeadersCharged: a begin's request headers and a non-stream response's response headers
// are reserved at 1 × their decoded size on top of 4 × the body, and the reservation goes to the
// worker with the item. A body whose headers do not fit the pool is shed (body_budget) and its
// envelope kept as metadata, with nothing left reserved.
func TestHandoffHeadersCharged(t *testing.T) {
	e := running(t, "full")
	reqH, respH := syntheticHeaders(8, 1024), syntheticHeaders(4, 2048)
	reqH.Set("Content-Type", "application/json")
	body := []byte(`{"synthetic":"handoff headers"}`)
	e.BeforeAuth(rpc(map[string]any{"RequestID": "hdr-1", "SourceFormat": "claude", "Headers": reqH, "Body": body}))
	e.Response(rpc(map[string]any{"RequestID": "hdr-1", "SourceFormat": "claude", "Model": "model-x", "StatusCode": 200,
		"ResponseHeaders": respH, "Body": body, "Metadata": map[string]any{"requested_model": "model-x"}}))
	if len(e.Handoff) != 2 {
		t.Fatalf("handoff holds %d items, want 2", len(e.Handoff))
	}
	b, r := <-e.Handoff, <-e.Handoff
	if want := 4*decodedLen(body) + headerBytes(b.Begin.Req.Headers); b.Begin.Reserved != want || headerBytes(b.Begin.Req.Headers) < 8*1024 {
		t.Fatalf("begin reserved %d, want 4 × body + headers = %d", b.Begin.Reserved, want)
	}
	if want := 4*decodedLen(body) + headerBytes(respH); r.Response.Reserved != want {
		t.Fatalf("response reserved %d, want 4 × body + headers = %d", r.Response.Reserved, want)
	}
	if got := e.Pool.Used(); got != b.Begin.Reserved+r.Response.Reserved {
		t.Fatalf("pool %d, reservations %d", got, b.Begin.Reserved+r.Response.Reserved)
	}
	e.Pool.Release(b.Begin.Reserved + r.Response.Reserved)

	// Headers that alone exceed what the pool has left: the body is shed, the begin kept as metadata.
	big := NewEngine()
	big.Pool = NewPool(64<<10, 32<<10)
	big.SetConfig(e.Config())
	big.Start()
	big.BeforeAuth(rpc(map[string]any{"RequestID": "hdr-2", "SourceFormat": "claude", "Headers": syntheticHeaders(16, 8192), "Body": body}))
	if big.Stats.Dropped()["body_budget"] != 1 || len(big.Handoff) != 0 || big.Pool.Used() != 0 {
		t.Fatalf("over-budget headers: body_budget %d, handoff %d, pool %d", big.Stats.Dropped()["body_budget"], len(big.Handoff), big.Pool.Used())
	}
	if c := <-big.Control; c.Begin == nil || c.Begin.MetaOnly == "" {
		t.Fatal("the shed begin was not kept as metadata")
	}
}
