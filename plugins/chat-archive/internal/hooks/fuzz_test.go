package hooks_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/capture"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/plugin"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/plugin/plugintest"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// TestConcurrentHookFuzzer drives every hook from many goroutines for overlapping RequestIDs
// (duplex follow-ups included) while reconfigure, quiesce and restart cycles run. Run under
// -race. It asserts no panic, input_seq per RequestID equal to call order on the hook side,
// and no reservation or state leak once the runtime stopped and the idle sweep ran.
func TestConcurrentHookFuzzer(t *testing.T) {
	// Duplex RequestIDs never complete, so every stop would wait the whole drain bound; and a stop
	// processes what the flood queued until the capture drain deadline, which 1 s still exercises
	// together with the past-deadline drop path.
	drain, capDrain := plugin.StopDrainTimeout, capture.DrainTimeout
	plugin.StopDrainTimeout, capture.DrainTimeout = 20*time.Millisecond, time.Second
	t.Cleanup(func() { plugin.StopDrainTimeout, capture.DrainTimeout = drain, capDrain })
	// Overlapping before-auths and header-inits on one RequestID look like reused lifecycles, so a
	// non-succeeded completion is held until its segments' owner settles (capture.OwnerSettle). The
	// settle runs on the sequencer tick; shorten both so the leak checks below see it.
	settle, sweep := capture.OwnerSettle, capture.SweepInterval
	capture.OwnerSettle, capture.SweepInterval = 50*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { capture.OwnerSettle, capture.SweepInterval = settle, sweep })
	h := plugintest.New(t, nil, "")
	p := h.Plugin
	e := p.Engine()
	stop := make(chan struct{})
	var cycles atomic.Int32
	var lifecycle sync.WaitGroup
	lifecycle.Add(1)
	go func() {
		defer lifecycle.Done()
		defer close(stop)
		alt := []string{h.YAML, h.YAML + "workers: 1\n", h.YAML + "capture: metadata\n"}
		for i := 0; i < 16; i++ {
			switch i % 4 {
			case 0:
				p.Handle(protocol.MethodPluginReconfigure, plugintest.Lifecycle(h.YAML)) // identical: idempotent
			case 1:
				p.Handle(protocol.MethodPluginReconfigure, plugintest.Lifecycle(alt[rand.IntN(len(alt))])) // restart
			case 2:
				p.Handle(protocol.MethodPluginQuiesce, nil)
			case 3:
				p.Handle(protocol.MethodPluginReconfigure, plugintest.Lifecycle(h.YAML)) // rollback restart
			}
			cycles.Add(1)
			time.Sleep(time.Duration(5+rand.IntN(20)) * time.Millisecond)
		}
	}()

	const workers = 16
	var wg sync.WaitGroup
	var calls atomic.Int64
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				rid := fmt.Sprintf("fz-%d", rand.IntN(24)) // overlapping RequestIDs across goroutines
				meta := plugintest.Meta("/v1/messages")
				switch rand.IntN(8) {
				case 0, 1:
					p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth(rid, "claude", plugintest.ClaudeBody(fmt.Sprintf("synthetic %d %d", w, i)),
						http.Header{"X-Archive-Origin": {"fuzz"}}, meta))
				case 2:
					p.Hook(protocol.MethodRequestInterceptAfter, plugintest.AfterAuth(rid))
				case 3:
					p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(rid, "claude", -1, nil, meta))
					for c := 0; c < 3; c++ {
						p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(rid, "claude", c,
							[]byte(fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"c%d\"}}\n\n", c)), meta))
					}
				case 4:
					p.Hook(protocol.MethodResponseInterceptAfter, plugintest.Response(rid, "openai", []byte(`{"id":"chatcmpl-x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}]}`), meta))
				case 5:
					p.Hook(protocol.MethodRequestComplete, plugintest.Complete(rid, []string{"succeeded", "failed", "canceled"}[rand.IntN(3)], meta))
				case 6:
					p.Hook(protocol.MethodUsageHandle, plugintest.Usage(fmt.Sprintf("u-%d-%d", w, i), "sk-cpa-synthetic", "src", "fail body"))
				case 7:
					// Duplex follow-ups: before-auth again on one RequestID from one goroutine.
					d := fmt.Sprintf("duplex-%d", w)
					for k := 0; k < 3; k++ {
						p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth(d, "openai-response",
							[]byte(fmt.Sprintf(`{"type":"response.create","input":"follow %d"}`, k)), nil, map[string]any{"execution_session_id": "ws-1"}))
					}
				}
				calls.Add(1)
			}
		}(w)
	}
	lifecycle.Wait()
	wg.Wait()
	if cycles.Load() < 16 {
		t.Fatalf("only %d lifecycle cycles ran", cycles.Load())
	}
	if n := e.Stats.Panics(); len(n) != 0 {
		t.Fatalf("recovered panics: %v", n)
	}
	// Duplex follow-ups (never completed, so their entries persist): every begin got its own
	// input_seq, so no begin key repeated at the archive.
	p.Handle(protocol.MethodPluginReconfigure, plugintest.Lifecycle(h.YAML))
	ob, _, _ := p.Runtime()
	plugintest.WaitFor(t, "outbox drained", func() bool {
		items, _ := ob.Pending(context.Background(), time.Now().Add(time.Hour), 10, 1<<30)
		return len(items) == 0
	})
	duplexBegins := 0
	for _, r := range h.Srv.Envelopes(h.SourceID) {
		if strings.HasPrefix(r.Envelope.Key, "ex/duplex-") && strings.Contains(r.Envelope.Key, "/begin/") {
			duplexBegins++
			// duplicate is a legitimate re-send (an ack cut off by a restart); a reused input_seq
			// would put a different body under the same key: conflict.
			if r.Status == wire.StatusConflict || r.Status == wire.StatusRejected {
				t.Fatalf("duplex begin %s answered %s", r.Envelope.Key, r.Status)
			}
		}
	}
	if duplexBegins == 0 {
		t.Fatal("no duplex begin reached the archive")
	}
	// Streams that straddled the restarts above: completing every RequestID while the runtime
	// runs must release every reservation (a completion held for chunks a previous runtime
	// processed would keep its accumulator and stream bytes). A completion claims only its own
	// lifecycle's segments, and the random completions above carry lifecycle times that leave many
	// segments to lifecycles that never complete; a completion without started_at tells no
	// lifecycles apart and claims every segment with no hook after its CompletedAt.
	meta := plugintest.Meta("/v1/messages")
	spanning := func(rid, outcome string) []byte {
		b, _ := json.Marshal(map[string]any{"RequestID": rid, "SourceFormat": "claude", "Model": "model-x", "RequestedModel": "model-x",
			"Outcome": outcome, "StatusCode": 0, "CompletedAt": time.Now().UTC(), "Metadata": meta})
		return b
	}
	for i := 0; i < 24; i++ {
		p.Hook(protocol.MethodRequestComplete, spanning(fmt.Sprintf("fz-%d", i), "canceled"))
	}
	for w := 0; w < workers; w++ {
		p.Hook(protocol.MethodRequestComplete, spanning(fmt.Sprintf("duplex-%d", w), "succeeded"))
	}
	plugintest.WaitFor(t, "no reservation while running", func() bool { return e.Pool.Used() == 0 && e.Pool.StreamUsed() == 0 })
	// Stop the runtime: every queued item is released, then the idle sweep removes all state.
	p.Handle(protocol.MethodPluginQuiesce, nil)
	if used, stream := e.Pool.Used(), e.Pool.StreamUsed(); used != 0 || stream != 0 {
		t.Fatalf("reservation leak after stop: pool %d stream %d", used, stream)
	}
	e.Sweep(time.Now().Add(hooks.IdleAfter + time.Minute))
	if n := e.Entries(); n != 0 {
		t.Fatalf("%d entries after the idle sweep", n)
	}
	t.Logf("%d hook calls, %d lifecycle cycles, dropped %v", calls.Load(), cycles.Load(), e.Stats.Dropped())
}
