package plugin_test

// K10 R1b scenarios on the whole plugin (plugin 0.1.6): past the budgets every loss is counted.
// Every value is synthetic.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/capture"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/plugin/plugintest"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// TestStallBeyondPoolShedsCounted: 512 KiB requests and responses at 10 exchanges/s through a 12 s
// stall fill the 256 MiB content pool (4 × each body). Later bodies are shed at admission
// (body_budget) and their envelopes kept as metadata: every offered envelope is stored, none is
// lost whole, and the archived envelopes marked body_budget are exactly the counted sheds.
func TestStallBeyondPoolShedsCounted(t *testing.T) {
	r := newR1Run(t)
	e := r.h.Plugin.Engine()
	_, release := r.holdOther()
	text := strings.Repeat("synthetic heavy body ", (512<<10)/21)
	time.AfterFunc(12*time.Second, release)
	r.loads.Add(1)
	go func() {
		defer r.loads.Done()
		next := time.Now()
		for end := time.Now().Add(14 * time.Second); time.Now().Before(end); {
			r.nonStream(text, text)
			next = next.Add(100 * time.Millisecond)
			time.Sleep(time.Until(next))
		}
	}()
	stored, offered := r.settleR1()
	d := e.Stats.Dropped()
	shed := d["body_budget"] + d["handoff_full"]
	if stored != offered || r.wholeLossR1() != 0 || shed == 0 {
		t.Fatalf("stored %d of %d, whole losses %d, shed %d: %v", stored, offered, r.wholeLossR1(), shed, d)
	}
	var marked uint64
	plugintest.WaitFor(t, "every envelope uploaded", func() bool {
		n := 0
		marked = 0
		for _, rec := range r.h.Srv.Envelopes(r.h.SourceID) {
			k := rec.Envelope.Key
			if !strings.HasPrefix(k, "ex/r1-") && !strings.HasPrefix(k, "usage/u-r1-") {
				continue
			}
			n++
			var b struct {
				Body wire.Body `json:"body"`
			}
			if json.Unmarshal(rec.Envelope.Body, &b) == nil && b.Body.Dropped == wire.DroppedBodyBudget {
				marked++
			}
		}
		return n == offered
	})
	t.Logf("offered %d, stored %d, shed %d (%v), archived as body_budget %d", offered, stored, shed, d, marked)
	if marked != shed {
		t.Fatalf("%d archived envelopes are marked body_budget, %d bodies were counted shed", marked, shed)
	}
}

// TestStallHeldDuplexCompletionAccounted: a Responses WebSocket whose RequestID is never reused,
// with a client follow-up between two bootstrap header-inits, ends failed during a stall. Its
// completion is held for ownership (ownership_held) and settled later; meanwhile its segments keep
// their stream charges. After the stall and the settle, the completion and both begins are stored,
// nothing is counted lost, and every pool and stream reservation is returned.
func TestStallHeldDuplexCompletionAccounted(t *testing.T) {
	for _, v := range []struct {
		p *time.Duration
		d time.Duration
	}{{&capture.OwnerSettle, 3 * time.Second}, {&capture.SweepInterval, 200 * time.Millisecond}} {
		old := *v.p
		*v.p = v.d
		t.Cleanup(func() { *v.p = old })
	}
	r := newR1Run(t)
	e := r.h.Plugin.Engine()
	_, release := r.holdOther()
	const rid = "dx-duplex" // outside the steady load's manifest
	ws := map[string]any{"request_path": "/v1/responses", "execution_session_id": "duplex-session"}
	p := r.h.Plugin
	p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth(rid, "openai-response", []byte(`{"type":"response.create","input":"first"}`), nil, ws))
	p.Hook(protocol.MethodRequestInterceptAfter, plugintest.AfterAuth(rid))
	p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(rid, "openai-response", -1, nil, ws))
	turn := wsTurn("resp_d1", "", "synthetic first turn")
	p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(rid, "openai-response", 0, turn[0], ws))
	time.Sleep(10 * time.Millisecond)
	p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth(rid, "openai-response", []byte(`{"type":"response.create","input":"follow-up"}`), nil, ws))
	time.Sleep(10 * time.Millisecond)
	p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(rid, "openai-response", -1, nil, ws))
	p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(rid, "openai-response", 0, wsTurn("resp_d2", "", "synthetic retry")[0], ws))
	p.Hook(protocol.MethodRequestComplete, plugintest.CompleteStatus(rid, "failed", 502, "synthetic upstream failure", ws))
	r.steady(40, 6*time.Second)
	time.Sleep(5 * time.Second)
	release()
	r.loads.Wait()
	plugintest.WaitFor(t, "the held completion stored", func() bool { return r.has(wire.ExchangeCompletionKey(rid)) })
	for _, k := range []string{wire.ExchangeBeginKey(rid, 0), wire.ExchangeBeginKey(rid, 1)} {
		if !r.has(k) {
			t.Fatalf("%s not stored", k)
		}
	}
	r.settleR1()
	if n := e.Stats.Other()["ownership_held"]; n == 0 {
		t.Fatal("the completion was not held for ownership (the case did not arise)")
	}
	if d := e.Stats.Dropped(); len(nonZero(d)) != 0 {
		t.Fatalf("dropped %v", nonZero(d))
	}
	plugintest.WaitFor(t, "every reservation returned", func() bool { return e.Pool.Used() == 0 && e.Pool.StreamUsed() == 0 })
}

func nonZero(m map[string]uint64) map[string]uint64 {
	out := map[string]uint64{}
	for k, v := range m {
		if v != 0 {
			out[k] = v
		}
	}
	return out
}

// TestStallStopAndReconfigureBounded (R3): with another connection user's commit stalled and a deep
// backlog queued behind the writer (8 s at 90 envelopes/s), a shutdown and a reconfigure each return
// within the stop budget plus its close margin and a second, as 0.1.5's do: the writer's connection
// wait is cut at the deadline and the backlog is counted stopped without I/O. Stored plus counted
// equals offered.
func TestStallStopAndReconfigureBounded(t *testing.T) {
	for _, op := range []string{"shutdown", "reconfigure"} {
		t.Run(op, func(t *testing.T) {
			setStopTimeout(t, 2*time.Second)
			r := newR1Run(t)
			e := r.h.Plugin.Engine()
			_, release := r.holdOther()
			r.steady(90, 8*time.Second)
			r.loads.Wait()
			_, _, rt := r.h.Plugin.Runtime()
			if q, _ := rt.WriteQueue(); q < 500 {
				t.Fatalf("backlog %d at the stop, want a deep one", q)
			}
			time.AfterFunc(10*time.Second, release)
			start := time.Now()
			if op == "shutdown" {
				r.h.Plugin.Shutdown()
			} else {
				_, _ = r.h.Plugin.Handle(protocol.MethodPluginReconfigure, plugintest.Lifecycle(r.h.YAML+"workers: 1\n"))
			}
			d := time.Since(start)
			release()
			if limit := 2*time.Second + 500*time.Millisecond + time.Second; d > limit {
				t.Fatalf("%s took %v with a stall in progress, over %v", op, d, limit)
			}
			stored, offered := r.stored()
			counted := r.wholeLossR1()
			t.Logf("%s=%v offered=%d stored=%d counted=%d dropped=%v", op, d.Round(time.Millisecond), offered, stored, counted, nonZero(e.Stats.Dropped()))
			if stored+counted != offered || uint64(counted) != e.Stats.Dropped()["stopped"] {
				t.Fatalf("stored %d + counted %d != offered %d (dropped %v)", stored, counted, offered, nonZero(e.Stats.Dropped()))
			}
		})
	}
}
