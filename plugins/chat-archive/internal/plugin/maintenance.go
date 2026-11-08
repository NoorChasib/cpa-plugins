package plugin

import (
	"context"
	"errors"
	"runtime"
	"runtime/metrics"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/capture"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/fixture"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/pushalert"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/safego"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// Maintenance cadence: GC, the cap ladder, the idle sweep, requeue jobs and the status snapshot.
var (
	SnapshotEvery = 5 * time.Second
	GCEvery       = 60 * time.Second
	PushEvery     = 60 * time.Second
	RequeueChunk  = 64
)

// snapshot is the in-memory status the maintenance goroutine refreshes; the status route and
// heartbeats read it and never touch SQLite.
type snapshot struct {
	at     time.Time
	outbox outbox.Stats
	ok     bool
	ob     *outbox.Outbox // the outbox the stats came from
}

// refreshSnapshot stores fresh outbox stats. When Stats fails it keeps the last good stats of the
// same outbox (marked not ok) instead of zeroes, so heartbeats never report an empty backlog
// because one query timed out.
func (p *Plugin) refreshSnapshot(rs *runtimeState) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := rs.ob.Stats(ctx)
	if err != nil {
		if prev := p.snap.Load(); prev != nil && prev.ob == rs.ob {
			p.snap.Store(&snapshot{at: prev.at, outbox: prev.outbox, ok: false, ob: rs.ob})
			return
		}
	}
	p.snap.Store(&snapshot{at: p.now().UTC(), outbox: s, ok: err == nil, ob: rs.ob})
}

// maintenance: status snapshot every 5 s; GC, pressure ladder and idle sweep every 60 s;
// requeue jobs in bounded chunks.
func (p *Plugin) maintenance(ctx context.Context, rs *runtimeState) {
	t := time.NewTicker(SnapshotEvery)
	defer t.Stop()
	lastGC := time.Time{}
	for {
		safego.Checkpoint("maintenance")
		p.runJob(ctx, rs)
		p.refreshSnapshot(rs)
		if time.Since(lastGC) >= GCEvery {
			lastGC = time.Now()
			p.gc(ctx, rs)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (p *Plugin) gc(ctx context.Context, rs *runtimeState) {
	for i := 0; i < 16; i++ {
		gctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		more, err := rs.ob.Maintain(gctx)
		cancel()
		if err != nil || !more {
			break
		}
	}
	ps := rs.capture.RefreshPressure()
	if ps.Level >= outbox.PressureEvict {
		ectx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_ = rs.ob.Evict(ectx)
		cancel()
		rs.capture.RefreshPressure()
	}
	p.engine.Sweep(p.engine.Now().Add(-hooks.IdleAfter))
}

// runJob advances the current requeue job by one bounded chunk.
func (p *Plugin) runJob(ctx context.Context, rs *runtimeState) {
	q := &p.jobs
	q.mu.Lock()
	j := q.cur
	if j != nil {
		j.State = "running"
	}
	q.mu.Unlock()
	if j == nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dead, err := rs.ob.DeadLetters(rctx, j.Reason, j.cursor, RequeueChunk)
	finish := func(state string) {
		q.mu.Lock()
		j.State, j.FinishedAt = state, p.now().UTC()
		q.done = append(q.done, *j)
		if len(q.done) > 8 {
			q.done = q.done[len(q.done)-8:]
		}
		q.cur = nil
		q.mu.Unlock()
		rs.up.Notify()
	}
	if err != nil {
		finish("failed")
		return
	}
	for _, d := range dead {
		if ctx.Err() != nil {
			return // stopping: the job resumes from its cursor if the runtime starts again
		}
		var requeued, skipped, failed uint64
		// One dead letter gets its own bound: masking a large piece may take up to MaskTimeout.
		ictx, icancel := context.WithTimeout(ctx, capture.MaskTimeout+30*time.Second)
		load := func(h wire.Hash) (wire.PieceKind, string, []byte, error) { return rs.ob.Piece(ictx, h) }
		env, held, err := capture.Requeued(ictx, d.Envelope, j.Remask, load, rs.masker, p.engine.Pool)
		switch {
		case errors.Is(err, capture.ErrRemaskBase):
			skipped = 1
		case err != nil:
			failed = 1 // ErrRemaskBudget included: a later requeue, with the pool free, may pass
		default:
			if _, err := rs.ob.Requeue(ictx, d.Seq, env); err != nil {
				failed = 1
			} else {
				requeued = 1
			}
		}
		p.engine.Pool.Release(held)
		icancel()
		q.mu.Lock()
		j.Processed++
		j.Requeued += requeued
		j.Skipped += skipped
		j.Failed += failed
		j.cursor = d.Seq
		q.mu.Unlock()
	}
	if len(dead) < RequeueChunk {
		finish("done")
	}
}

func (p *Plugin) pushLoop(ctx context.Context, rs *runtimeState) {
	safego.Checkpoint("push_alert")
	if rs.alerter == nil {
		<-ctx.Done()
		return
	}
	t := time.NewTicker(PushEvery)
	defer t.Stop()
	for {
		safego.Checkpoint("push_alert")
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var s pushalert.Snapshot
		if sn := p.snap.Load(); sn != nil {
			s.OldestPendingAgeS, s.Pressure = sn.outbox.OldestPendingAgeS, sn.outbox.Pressure
		}
		s.Dropped, s.Faults = p.engine.Stats.Dropped(), p.engine.Stats.Faults()
		if due := rs.alerter.Evaluate(s, p.now()); len(due) > 0 {
			sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			rs.alerter.Send(sctx, due)
			cancel()
		}
	}
}

// ---- status and heartbeat ----

func readRuntime(rs *runtimeState, rt *capture.Runtime, pool *hooks.Pool) wire.RuntimeStats {
	samples := []metrics.Sample{
		{Name: "/memory/classes/total:bytes"},
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/gc/cycles/total:gc-cycles"},
	}
	metrics.Read(samples)
	val := func(i int) uint64 {
		if samples[i].Value.Kind() == metrics.KindUint64 {
			return samples[i].Value.Uint64()
		}
		return 0
	}
	s := wire.RuntimeStats{Sys: val(0), HeapInuse: val(1), GCCycles: val(2), Goroutines: uint64(runtime.NumGoroutine())}
	if lim := debugMemLimit(); lim > 0 {
		s.MemLimit = uint64(lim)
	}
	s.InflightBodyBytes = uint64(max(pool.Used()-pool.StreamUsed(), 0))
	if rt != nil {
		s.StreamAccumBytes = uint64(max(rt.StreamAccumBytes(), 0))
		s.ActiveStreams = uint64(max(rt.ActiveStreams(), 0))
	}
	return s
}

// heartbeat builds one heartbeat (counts, names and times only).
func (p *Plugin) heartbeat(rs *runtimeState, state string) wire.Heartbeat {
	st := p.engine.Stats
	hs := st.Hooks()
	if state == "" {
		state = StateRunning
		if len(st.Faults()) > 0 {
			state = StateDegraded
		}
	}
	hb := wire.Heartbeat{
		V: wire.ProtocolV1, OutboxID: rs.ob.Meta().OutboxID, BootID: p.bootID, N: p.hbN.Add(1), SentAt: p.now().UTC(),
		StartedAt: p.startedAt, Component: "cpa-plugin", Version: Version, State: state, MaskVersion: mask.Version,
		RulesetSHA: mask.RulesetSHA256(), ExactRev: rs.masker.Get().Rev, LastHookAt: hs.LastHookAt, LastUsageAt: hs.LastUsageAt,
		Calls: hs.Calls, HookErrors: hs.Errors, Panics: st.Panics(), Dropped: st.Dropped(), HookP50us: hs.P50us, HookP99us: hs.P99us,
		Uploader: rs.up.Stats(), Runtime: readRuntime(rs, rs.capture, p.engine.Pool), Masker: rs.capture.MaskerStats(),
		Faults: st.Faults(),
	}
	if s := p.hostSchema.Load(); s > 0 {
		hb.HostVersion = sprint("schema:%d", s)
	}
	if sn := p.snap.Load(); sn != nil {
		hb.Outbox = sn.outbox.OutboxStats
	}
	return hb
}

// status is the management status body: counts, names and times only, never content.
func (p *Plugin) status() map[string]any {
	p.mu.RLock()
	rs, cfg, state, reason := p.rt, p.cfg, p.state, p.reason
	p.mu.RUnlock()
	st := p.engine.Stats
	hs := st.Hooks()
	tokenRef := func(env, file string) string {
		switch {
		case env != "":
			return "env"
		case file != "":
			return "file"
		}
		return "missing"
	}
	out := map[string]any{
		"api_schema": 1, "plugin": ID, "version": Version, "schema": protocol.SchemaVersion, "state": state, "boot_id": p.bootID,
		"started_at": p.startedAt,
		"config": map[string]any{
			"archive_url_configured": cfg.ArchiveURL != "", "source_token": tokenRef(cfg.SourceTokenEnv, cfg.SourceTokenFile),
			"hmac_key": tokenRef(cfg.HMACKeyEnv, cfg.HMACKeyFile), "hmac_key_id": cfg.HMACKeyID, "capture": cfg.Capture,
			"workers": cfg.Workers, "outbox_cap_bytes": cfg.OutboxCapBytes, "replay_retention_hours": cfg.ReplayRetentionHours,
			"memory_limit_mib": cfg.MemoryLimitMiB, "max_rpc_mib": cfg.MaxRPCMiB, "data_dir_default": cfg.DataDir == "",
			"account_labels": len(cfg.Labels()), "exact_fingerprints": len(cfg.ExactFPs()),
			"pushover": tokenRef(cfg.PushoverAppTokenEnv, cfg.PushoverAppTokenFile) != "missing",
		},
		"last_hook_at": hs.LastHookAt, "last_usage_at": hs.LastUsageAt, "calls": hs.Calls, "hook_errors": hs.Errors,
		"hook_p50_us": hs.P50us, "hook_p99_us": hs.P99us, "dropped": st.Dropped(), "counters": st.Other(),
		"recovered_panics": st.Panics(), "recovered_panic_types": st.PanicTypes(), "faults": st.Faults(), "requeue_jobs": p.jobs.snapshot(),
		"pool": map[string]int64{"used": p.engine.Pool.Used(), "stream": p.engine.Pool.StreamUsed()},
	}
	if reason != "" {
		out["reason"] = reason
	}
	if s := p.hostSchema.Load(); s > 0 {
		out["host_schema"] = s
	}
	if sn := p.snap.Load(); sn != nil {
		out["outbox"] = sn.outbox.OutboxStats
		out["dead_letters"] = sn.outbox.DeadByReason
		out["unclean_runs"] = sn.outbox.UncleanRuns
		out["snapshot_at"] = sn.at
	}
	if rs != nil {
		out["uploader"] = rs.up.Stats()
		out["masker"] = rs.capture.MaskerStats()
		out["runtime"] = readRuntime(rs, rs.capture, p.engine.Pool)
		out["media_fetch_queue"] = rs.up.Media().Len()
	}
	if fixture.Enabled {
		out["fixture_records"] = p.engine.Fixture.Snapshot()
	}
	return out
}
