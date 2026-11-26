package capture

// A deterministic rig for the 0.1.5 reuse cases: the real hooks run on a fake clock, and the test
// feeds the stream and control items they queue to the sequencer in the order it chooses (the
// stream goroutine reads its channels in no fixed order). Workers and the writer run, so the
// envelopes land in a real outbox. Every value is synthetic.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/safego"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

type fakeClock struct{ ns atomic.Int64 }

func (c *fakeClock) Now() time.Time { return time.Unix(0, c.ns.Load()).UTC() }

// step advances the clock by a millisecond and returns the new time.
func (c *fakeClock) step() time.Time { return time.Unix(0, c.ns.Add(int64(time.Millisecond))).UTC() }

func (c *fakeClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

type rig struct {
	t        *testing.T
	e        *hooks.Engine
	rt       *Runtime
	ob       *outbox.Outbox
	sq       *seqState
	clk      *fakeClock
	streams  []hooks.StreamItem
	controls []hooks.Control
}

type rigOpts struct {
	maxRPC  int
	capture string
	ob      *outbox.Outbox // shared outbox (a second instance); nil = a new one
	clk     *fakeClock
}

func newRig(t *testing.T, o rigOpts) *rig {
	t.Helper()
	if o.maxRPC == 0 {
		o.maxRPC = protocol.MaxRPCDefault
	}
	if o.capture == "" {
		o.capture = "full"
	}
	e := hooks.NewEngine()
	clk := o.clk
	if clk == nil {
		clk = &fakeClock{}
		clk.ns.Store(time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC).UnixNano())
	}
	e.Now = clk.Now
	ob := o.ob
	if ob == nil {
		var err error
		ob, err = outbox.Open(outbox.Options{Dir: filepath.Join(t.TempDir(), "data"), CapBytes: 1 << 40, Retention: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ob.Close() })
	}
	m, _ := mask.New(mask.Config{})
	rt := &Runtime{opts: Options{Engine: e, Outbox: ob, Workers: 1, Masker: NewMaskerHolder(m, 0, 1<<20)}, prefix: NewPrefixMap(),
		writes: make(chan writeReq, 1024)}
	rt.initContexts()
	safego.Loop(rt.runCtx, &rt.wg, "worker", e.Stats.Panic, rt.worker)
	safego.Loop(rt.writerCtx, &rt.writerWG, "writer", e.Stats.Panic, rt.writer)
	t.Cleanup(func() {
		rt.runCancel()
		rt.wg.Wait()
		rt.drainStop()
		rt.writerEnd()
		rt.writerWG.Wait()
	})
	e.SetConfig(&hooks.Config{Configured: true, Capture: o.capture, MaxRPC: o.maxRPC})
	e.Start()
	clk.step()
	return &rig{t: t, e: e, rt: rt, ob: ob, sq: newSeqState(), clk: clk}
}

func rawJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

var rigMeta = map[string]any{"request_path": "/v1/responses", "execution_session_id": "ws-rig"}

// collect moves what the hooks queued into the rig's queues.
func (r *rig) collect() {
	for {
		select {
		case it := <-r.e.Stream:
			r.streams = append(r.streams, it)
		case c := <-r.e.Control:
			r.controls = append(r.controls, c)
		default:
			return
		}
	}
}

// Hooks: each advances the clock first, so every hook has its own start time, which it returns.

func (r *rig) ba(rid, text string) time.Time {
	at := r.clk.step()
	r.e.BeforeAuth(rawJSON(map[string]any{"RequestID": rid, "SourceFormat": "openai-response", "Model": "model-x", "RequestedModel": "model-x",
		"Body": []byte(`{"type":"response.create","model":"model-x","input":"` + text + `"}`), "Metadata": rigMeta}))
	r.collect()
	return at
}

func (r *rig) raw(method string, raw []byte) time.Time {
	at := r.clk.step()
	switch method {
	case protocol.MethodRequestInterceptBefore:
		r.e.BeforeAuth(raw)
	case protocol.MethodRequestComplete:
		r.e.Complete(raw)
	}
	r.collect()
	return at
}

func (r *rig) aa(rid string) time.Time {
	at := r.clk.step()
	r.e.AfterAuth(rawJSON(map[string]any{"RequestID": rid, "ToFormat": "codex", "Model": "model-x", "Metadata": rigMeta}))
	r.collect()
	return at
}

func (r *rig) chunk(rid string, index int, body string) time.Time {
	at := r.clk.step()
	m := map[string]any{"RequestID": rid, "SourceFormat": "openai-response", "Model": "model-x", "RequestedModel": "model-x",
		"ChunkIndex": index, "Metadata": rigMeta}
	if index < 0 {
		m["ResponseHeaders"] = map[string][]string{"Content-Type": {"text/event-stream"}}
	} else {
		m["Body"] = []byte(body)
	}
	r.e.StreamChunk(rawJSON(m))
	r.collect()
	return at
}

func (r *rig) response(rid, id string) time.Time {
	at := r.clk.step()
	r.e.Response(rawJSON(map[string]any{"RequestID": rid, "SourceFormat": "openai-response", "Model": "model-x", "RequestedModel": "model-x",
		"ResponseHeaders": map[string][]string{"Content-Type": {"application/json"}}, "OriginalRequest": []byte("{}"),
		"Body": []byte(`{"id":"` + id + `","object":"response","status":"completed","output":[]}`), "StatusCode": 200, "Metadata": rigMeta}))
	r.collect()
	return at
}

func completeRaw(rid, outcome string, started, completed time.Time) []byte {
	m := map[string]any{"RequestID": rid, "SourceFormat": "openai-response", "Model": "model-x", "RequestedModel": "model-x",
		"Stream": true, "Outcome": outcome, "StatusCode": map[string]int{"succeeded": 200, "failed": 429, "canceled": 0}[outcome],
		"CompletedAt": completed, "Metadata": rigMeta}
	if !started.IsZero() {
		m["StartedAt"] = started
	}
	return rawJSON(m)
}

func (r *rig) complete(rid, outcome string, started, completed time.Time) {
	r.clk.step()
	r.e.Complete(completeRaw(rid, outcome, started, completed))
	r.collect()
}

// Feeding the sequencer.

func (r *rig) feedStream(n int) {
	r.t.Helper()
	if n > len(r.streams) {
		r.t.Fatalf("feedStream(%d): only %d queued", n, len(r.streams))
	}
	for _, it := range r.streams[:n] {
		r.rt.onStream(r.sq, it)
	}
	r.streams = r.streams[n:]
}

func (r *rig) feedStreams() { r.feedStream(len(r.streams)) }

// feedAttempts processes every queued control that is not a completion.
func (r *rig) feedAttempts() {
	keep := r.controls[:0]
	for _, c := range r.controls {
		if c.Done != nil {
			keep = append(keep, c)
			continue
		}
		r.rt.onControl(r.sq, c)
	}
	r.controls = keep
}

// feedDone processes the queued completion of the lifecycle that started at started.
func (r *rig) feedDone(started time.Time) {
	r.t.Helper()
	i := slices.IndexFunc(r.controls, func(c hooks.Control) bool { return c.Done != nil && c.Done.Req.StartedAt.Equal(started) })
	if i < 0 {
		r.t.Fatalf("no queued completion started at %v", started)
	}
	c := r.controls[i]
	r.controls = slices.Delete(r.controls, i, i+1)
	r.rt.onControl(r.sq, c)
}

// tick runs the sequencer tick: held completions settle once OwnerSettle passed.
func (r *rig) tick() {
	for rid := range r.sq.pend {
		r.rt.evalPending(r.sq, rid, false)
	}
}

// Reading the outbox.

type got struct {
	keys  []string
	env   map[string]wire.Envelope
	resp  map[string]wire.ExchangeResponse
	comp  map[string]wire.ExchangeCompletion
	begin map[string]wire.ExchangeBegin
}

func (r *rig) read() got {
	items, err := r.ob.Pending(context.Background(), time.Now().Add(24*time.Hour), 10000, 1<<40)
	if err != nil {
		r.t.Fatal(err)
	}
	g := got{env: map[string]wire.Envelope{}, resp: map[string]wire.ExchangeResponse{}, comp: map[string]wire.ExchangeCompletion{},
		begin: map[string]wire.ExchangeBegin{}}
	for _, it := range items {
		if it.Tomb != nil {
			continue
		}
		env, err := wire.DecodeEnvelope(it.Envelope)
		if err != nil {
			r.t.Fatal(err)
		}
		g.keys = append(g.keys, env.Key)
		g.env[env.Key] = env
		switch env.Kind {
		case wire.KindExchangeResponse:
			var x wire.ExchangeResponse
			json.Unmarshal(env.Body, &x)
			g.resp[env.Key] = x
		case wire.KindExchangeDone:
			var x wire.ExchangeCompletion
			json.Unmarshal(env.Body, &x)
			g.comp[env.Key] = x
		case wire.KindExchangeBegin:
			var x wire.ExchangeBegin
			json.Unmarshal(env.Body, &x)
			g.begin[env.Key] = x
		}
	}
	slices.Sort(g.keys)
	return g
}

// waitFor polls the outbox until cond holds, then waits for the workers to go idle.
func (r *rig) waitFor(what string, cond func(got) bool) got {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		g := r.read()
		if cond(g) {
			return g
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("timeout waiting for %s; stored %v", what, g.keys)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *rig) waitKeys(keys ...string) got {
	r.t.Helper()
	return r.waitFor(strings.Join(keys, ", "), func(g got) bool {
		for _, k := range keys {
			if _, ok := g.env[k]; !ok {
				return false
			}
		}
		return true
	})
}

// settled waits until no entry and no reservation is left.
func (r *rig) settled() {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for r.e.Entries() != 0 || r.e.Pool.Used() != 0 {
		if time.Now().After(deadline) {
			r.t.Fatalf("state left: entries %d pool %d", r.e.Entries(), r.e.Pool.Used())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *rig) counter(name string) uint64 { return r.e.Stats.Other()[name] }

func (r *rig) noDuplicates() {
	r.t.Helper()
	if n := r.e.Stats.Dropped()["duplicate_key"]; n != 0 {
		r.t.Fatalf("dropped.duplicate_key = %d", n)
	}
}

// respByID returns the stored responses with response id id.
func (g got) respByID(id string) []wire.ExchangeResponse {
	var out []wire.ExchangeResponse
	for _, k := range g.keys {
		if r, ok := g.resp[k]; ok && r.ResponseID == id {
			out = append(out, r)
		}
	}
	return out
}

// Synthetic Responses stream events.
func evCreated(id string) string { return `{"type":"response.created","response":{"id":"` + id + `"}}` }
func evItem(text string) string {
	return `{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + text + `"}]}}`
}
func evDone(id string) string {
	return `{"type":"response.completed","response":{"id":"` + id + `","status":"completed","output":[]}}`
}
