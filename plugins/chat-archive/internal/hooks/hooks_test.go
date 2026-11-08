package hooks

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/fingerprint"
)

func rpc(v map[string]any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func running(t *testing.T, capture string) *Engine {
	e := NewEngine()
	e.SetConfig(&Config{Configured: true, Capture: capture, MaxRPC: 8 << 20, HMACKeyID: "k1", HMACKey: []byte(strings.Repeat("k", 32))})
	e.Start()
	return e
}

// input_seq per RequestID equals call order, with many RequestIDs interleaved concurrently.
func TestInputSeqFollowsCallOrder(t *testing.T) {
	e := running(t, "full")
	const rids, calls = 40, 12
	got := make(chan *Begin, rids*calls)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case w := <-e.Handoff:
				e.Pool.Release(w.Begin.Reserved)
				got <- w.Begin
			case c := <-e.Control:
				got <- c.Begin
			case <-done:
				return
			}
		}
	}()
	var wg sync.WaitGroup
	for r := 0; r < rids; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for i := 0; i < calls; i++ {
				body := []byte(fmt.Sprintf(`{"call":%d}`, i))
				e.BeforeAuth(rpc(map[string]any{"RequestID": fmt.Sprintf("rid-%d", r), "SourceFormat": "claude", "Body": body}))
			}
		}(r)
	}
	wg.Wait()
	seen := 0
	for seen < rids*calls {
		b := <-got
		seen++
		if b.Req.Body == nil {
			continue // metadata-only (budget); input_seq still checked below via the counter
		}
		var m struct{ Call uint32 }
		json.Unmarshal(b.Req.Body, &m)
		if m.Call != b.InputSeq {
			t.Fatalf("%s: call %d got input_seq %d", b.Req.RequestID, m.Call, b.InputSeq)
		}
	}
	close(done)
	for r := 0; r < rids; r++ {
		e.With(fmt.Sprintf("rid-%d", r), false, func(en *Entry) {
			if en.NextInputSeq != calls {
				t.Fatalf("next input seq = %d", en.NextInputSeq)
			}
		})
	}
	if e.Pool.Used() != 0 {
		t.Fatalf("pool leak %d", e.Pool.Used())
	}
}

func TestOversizeAndModelList(t *testing.T) {
	e := running(t, "full")
	big := make([]byte, 9<<20)
	out := e.BeforeAuth(rpc(map[string]any{"RequestID": "r", "Headers": map[string][]string{"X-Archive-Machine": {"m"}, "x-archive-bogus": {"1"}}, "Body": big}))
	if !strings.Contains(string(out), `"ClearHeaders":["X-Archive-Machine","x-archive-bogus"]`) {
		t.Fatalf("oversize before-auth = %s", out)
	}
	if e.Stats.Dropped()["oversize_rpc"] != 1 || len(e.Handoff) != 0 || len(e.Control) != 0 {
		t.Fatal("oversize call captured or not counted")
	}
	e.StreamChunk(rpc(map[string]any{"RequestID": "r", "ChunkIndex": 3, "Body": make([]byte, 17<<20)}))
	e.UsageHook(rpc(map[string]any{"RequestID": "u", "Failure": map[string]any{"Body": strings.Repeat("x", 5<<20)}}))
	if e.Stats.Dropped()["oversize_rpc"] != 3 {
		t.Fatalf("oversize_rpc = %d", e.Stats.Dropped()["oversize_rpc"])
	}
	e.Response(rpc(map[string]any{"RequestID": "ml", "Body": []byte(`{"data":[]}`), "Metadata": nil}))
	if e.Stats.Other()["ignored_model_list"] != 1 || len(e.Handoff) != 0 {
		t.Fatal("model list not ignored")
	}
	// Admission stopped: pass-through, counted stopped while stopping.
	e.BeginStop()
	if out := e.AfterAuth(rpc(map[string]any{"RequestID": "s"})); string(out) != string(OK) || e.Stats.Dropped()["stopped"] != 1 {
		t.Fatalf("stopping hook = %s", out)
	}
}

func TestUsageFingerprintsMatchPkgFingerprint(t *testing.T) {
	if CallerScopeBytes([]byte("  key-1 \n")) != fingerprint.CallerScope("key-1") || CallerScopeBytes([]byte(" ")) != "" {
		t.Fatal("CallerScopeBytes differs")
	}
	k := []byte(strings.Repeat("h", 32))
	if HMACBytes("k1", k, []byte("claude"), []byte("src")) != fingerprint.HMAC("k1", k, "claude", "src") {
		t.Fatal("HMACBytes differs")
	}
}

func TestStreamBudgets(t *testing.T) {
	e := running(t, "full")
	e.StreamChunk(rpc(map[string]any{"RequestID": "s1", "ChunkIndex": -1}))
	chunk := make([]byte, 1<<20)
	for i := 0; i < 70; i++ {
		e.StreamChunk(rpc(map[string]any{"RequestID": "s1", "ChunkIndex": i, "Body": chunk}))
	}
	var truncated bool
	var queued uint64
	e.With("s1", false, func(en *Entry) { truncated, queued = en.Truncated, en.ChunksQueued })
	if !truncated || e.Stats.Dropped()["stream_overflow"] == 0 || e.Pool.StreamUsed() > StreamPerStream {
		t.Fatalf("per-stream cap not enforced: truncated=%v used=%d", truncated, e.Pool.StreamUsed())
	}
	if int(queued) != len(e.Stream) {
		t.Fatalf("queued %d, channel %d", queued, len(e.Stream))
	}
}
