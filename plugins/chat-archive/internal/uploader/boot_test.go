package uploader

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/client/clienttest"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

const (
	bootA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	bootB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// boot is one plugin boot against an outbox directory: its own counters and uploader.
type boot struct {
	ob    *outbox.Outbox
	stats *hooks.Stats
	up    *Uploader
	n     atomic.Uint64
}

func startBoot(t *testing.T, srv *clienttest.Server, token, dir, id string) *boot {
	t.Helper()
	ob, err := outbox.Open(outbox.Options{Dir: dir, CapBytes: 1 << 40, Retention: 168 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	b := &boot{ob: ob, stats: hooks.NewStats()}
	started := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	b.up = New(Options{BaseURL: srv.URL, Token: token, Outbox: ob, Stats: b.stats,
		Heartbeat: func(state string) wire.Heartbeat {
			if state == "" {
				state = "running"
			}
			return wire.Heartbeat{V: wire.ProtocolV1, OutboxID: ob.Meta().OutboxID, BootID: id, N: b.n.Add(1), StartedAt: started,
				Component: "cpa-plugin", Version: "0.0.0-test", State: state, MaskVersion: "mask/v1", Dropped: b.stats.Dropped(),
				Outbox: wire.OutboxStats{Dead: 2}}
		},
		Boot: outbox.BootCounters{BootID: id, StartedAt: started, Component: "cpa-plugin", Version: "0.0.0-" + id[:1]}})
	b.up.Start()
	t.Cleanup(func() { b.up.Stop(); ob.Close() })
	return b
}

// crash stops the loops the way a killed process does: no final heartbeat, no final save.
func (b *boot) crash() {
	b.up.once.Do(func() {
		close(b.up.stop)
		b.up.cancel()
		b.up.wg.Wait()
	})
	b.ob.Close()
}

func heartbeatsOf(srv *clienttest.Server, src, bootID string) []wire.Heartbeat {
	var out []wire.Heartbeat
	for _, hb := range srv.Heartbeats(src) {
		if hb.BootID == bootID {
			out = append(out, hb)
		}
	}
	return out
}

func setHeartbeatEvery(t *testing.T, d time.Duration) {
	old := HeartbeatEvery
	HeartbeatEvery = d
	t.Cleanup(func() { HeartbeatEvery = old })
}

// TestDropCountsSurviveCrash: drops counted after a boot's last heartbeat reach the archive when the
// process dies before the next one. The boot saves them to the outbox as they happen; the next boot
// reports them under the dead boot's id, before its own first heartbeat, then deletes the record.
func TestDropCountsSurviveCrash(t *testing.T) {
	setHeartbeatEvery(t, time.Hour) // one heartbeat at start, then none before the crash
	srv := clienttest.NewServer(clienttest.Options{})
	t.Cleanup(srv.Close)
	src, token := srv.AddSource("cpa-plugin", "test")
	dir := filepath.Join(t.TempDir(), "d")

	a := startBoot(t, srv, token, dir, bootA)
	waitFor(t, "boot A heartbeat", func() bool { return len(heartbeatsOf(srv, src, bootA)) == 1 })
	for range 3 {
		a.stats.Drop("parse_error")
	}
	a.stats.Drop("stopped")
	waitFor(t, "boot A counters saved", func() bool {
		recs, err := a.ob.BootCounters(context.Background())
		return err == nil && len(recs) == 1 && recs[0].Dropped["parse_error"] == 3 && recs[0].Dropped["stopped"] == 1
	})
	a.crash()
	if hbs := heartbeatsOf(srv, src, bootA); len(hbs) != 1 || hbs[0].Dropped["parse_error"] != 0 {
		t.Fatalf("boot A heartbeats before restart: %+v", hbs)
	}

	b := startBoot(t, srv, token, dir, bootB)
	waitFor(t, "boot B heartbeat", func() bool { return len(heartbeatsOf(srv, src, bootB)) == 1 })
	all := srv.Heartbeats(src)
	if len(all) != 3 {
		t.Fatalf("heartbeats = %d, want A, recovered A, B", len(all))
	}
	rec := all[1]
	if rec.BootID != bootA || rec.State != StateRecovered || rec.N != 2 || rec.Version != "0.0.0-a" || rec.Component != "cpa-plugin" ||
		!rec.StartedAt.Equal(time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("recovered heartbeat identity = %+v", rec)
	}
	if rec.Dropped["parse_error"] != 3 || rec.Dropped["stopped"] != 1 {
		t.Fatalf("recovered dropped = %v", rec.Dropped)
	}
	// The archive's checks read outbox and mask fields from a source's newest heartbeat: the
	// recovered one carries the sender's; per-boot counters stay empty.
	if rec.Outbox.Dead != 2 || rec.MaskVersion != "mask/v1" || rec.OutboxID != b.ob.Meta().OutboxID || len(rec.Calls) != 0 || len(rec.LastHookAt) != 0 {
		t.Fatalf("recovered heartbeat fields = %+v", rec)
	}
	if all[2].BootID != bootB {
		t.Fatalf("newest heartbeat is boot %s, want B", all[2].BootID)
	}
	waitFor(t, "record deleted", func() bool {
		recs, err := b.ob.BootCounters(context.Background())
		return err == nil && len(recs) == 0
	})
}

// TestRecoveredCountsRetriedUntilTaken: a recovered heartbeat the archive refuses is sent again on
// the next tick (the record stays until then), this boot's own heartbeat still goes out each tick,
// and this boot's own record is never reported as a recovered one.
func TestRecoveredCountsRetriedUntilTaken(t *testing.T) {
	setHeartbeatEvery(t, 50*time.Millisecond)
	srv := clienttest.NewServer(clienttest.Options{})
	t.Cleanup(srv.Close)
	src, token := srv.AddSource("cpa-plugin", "test")
	dir := filepath.Join(t.TempDir(), "d")
	ob, err := outbox.Open(outbox.Options{Dir: dir, CapBytes: 1 << 40, Retention: 168 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{bootA, bootB} {
		if err := ob.SaveBootCounters(context.Background(), outbox.BootCounters{BootID: id, Dropped: map[string]uint64{"disk_guard": 4}}); err != nil {
			t.Fatal(err)
		}
	}
	ob.Close()
	srv.FailNext("POST "+wire.PathHeartbeat, 503, 0)

	b := startBoot(t, srv, token, dir, bootB)
	waitFor(t, "recovered heartbeat", func() bool { return len(heartbeatsOf(srv, src, bootA)) == 1 })
	waitFor(t, "record of A deleted", func() bool {
		recs, err := b.ob.BootCounters(context.Background())
		return err == nil && len(recs) == 1 && recs[0].BootID == bootB
	})
	n := len(heartbeatsOf(srv, src, bootB))
	waitFor(t, "two more B heartbeats", func() bool { return len(heartbeatsOf(srv, src, bootB)) >= n+2 })
	if hbs := heartbeatsOf(srv, src, bootA); len(hbs) != 1 || hbs[0].Dropped["disk_guard"] != 4 {
		t.Fatalf("boot A heartbeats = %+v", hbs)
	}
	for _, hb := range heartbeatsOf(srv, src, bootB) {
		if hb.State == StateRecovered {
			t.Fatal("this boot's own record was reported as recovered")
		}
	}
}

// TestSaveDropsOnlyOnChange: nothing is saved before the first drop, a drop is saved with the last
// heartbeat's n, and an unchanged total writes nothing.
func TestSaveDropsOnlyOnChange(t *testing.T) {
	setHeartbeatEvery(t, time.Hour)
	srv := clienttest.NewServer(clienttest.Options{})
	t.Cleanup(srv.Close)
	src, token := srv.AddSource("cpa-plugin", "test")
	b := startBoot(t, srv, token, filepath.Join(t.TempDir(), "d"), bootA)
	waitFor(t, "heartbeat", func() bool { return len(heartbeatsOf(srv, src, bootA)) == 1 })
	if err := b.up.SaveDrops(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recs, _ := b.ob.BootCounters(context.Background()); len(recs) != 0 {
		t.Fatalf("saved without drops: %+v", recs)
	}
	b.stats.Drop("oversize")
	waitFor(t, "saved", func() bool {
		recs, _ := b.ob.BootCounters(context.Background())
		return len(recs) == 1 && recs[0].Dropped["oversize"] == 1 && recs[0].N == 1
	})
	// Unchanged total: no write (a deleted record stays deleted).
	if err := b.ob.DeleteBootCounters(context.Background(), bootA); err != nil {
		t.Fatal(err)
	}
	if err := b.up.SaveDrops(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recs, _ := b.ob.BootCounters(context.Background()); len(recs) != 0 {
		t.Fatalf("unchanged counters saved again: %+v", recs)
	}
}
