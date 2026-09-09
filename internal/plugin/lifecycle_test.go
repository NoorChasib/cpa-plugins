package plugin

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-token-usage/internal/protocol"
	"github.com/NoorChasib/cpa-plugin-token-usage/internal/store"
)

func TestQuiesceKeepsLateDiagnosticsReachableAndReopensSameConfig(t *testing.T) {
	p := newRegistered(t)
	p.Handle(protocol.MethodUsageHandle, []byte(validUsage))
	waitCommitted(t, p, "1")
	old := p.runtime()
	if _, err := p.Handle(protocol.MethodPluginQuiesce, nil); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{validUsage, `{`} {
		if _, err := p.Handle(protocol.MethodUsageHandle, []byte(raw)); err == nil {
			t.Fatal("late usage admitted")
		}
	}
	p.RejectNativeUsage()
	if p.runtime() != old {
		t.Fatal("quiesce made stopped diagnostics unreachable")
	}
	diag := old.Snapshot()["diagnostics"].(map[string]string)
	if diag["observed_events"] != "4" || diag["dropped_stopped"] != "3" || diag["committed_events"] != "1" {
		t.Fatalf("late callback accounting: %v", diag)
	}
	r := manage(t, p, "/v0/management/plugins/token-usage/status", nil)
	if r.StatusCode != 200 || !strings.Contains(string(r.Body), `"state":"stopped"`) || !strings.Contains(string(r.Body), `"dropped_stopped":"3"`) {
		t.Fatalf("stopped status %d %s", r.StatusCode, r.Body)
	}
	if r := manage(t, p, "/v0/management/plugins/token-usage/summary", query()); r.StatusCode != 503 {
		t.Fatal("closed storage did not return unavailable")
	}
	// Post-close increments are deliberately RAM-only, not a claim that the
	// stopped worker persisted them or that a new worker was started to do so.
	db, initial, err := store.Open(p.cfg, testNow)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if initial.Diagnostics["observed_events"] != "1" || initial.Diagnostics["dropped_stopped"] != "0" {
		t.Fatalf("post-close callbacks unexpectedly persisted: %+v", initial.Diagnostics)
	}
	if _, err := p.Handle(protocol.MethodPluginReconfigure, registrationRequest(p.cfg.DatabasePath)); err != nil {
		t.Fatal(err)
	}
	if p.runtime() == old || p.runtime().Stopped() {
		t.Fatal("same-config reconfigure reused the closed collector")
	}
	if got := p.runtime().Snapshot()["diagnostics"].(map[string]string)["observed_events"]; got != "1" {
		t.Fatal("reopen did not resume documented durable diagnostics")
	}
	if _, err := p.Handle(protocol.MethodUsageHandle, []byte(validUsage)); err != nil {
		t.Fatal(err)
	}
	waitCommitted(t, p, "2")
	p.Shutdown()
	if _, err := p.Handle(protocol.MethodPluginRegister, registrationRequest(p.cfg.DatabasePath)); err == nil {
		t.Fatal("final shutdown allowed a new worker")
	}
}

func TestLateCallbackDuringDrainReachesFinalBestEffortSave(t *testing.T) {
	p := newRegistered(t)
	external, err := sql.Open("sqlite3", p.cfg.DatabasePath+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer external.Close()
	tx, err := external.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := p.Handle(protocol.MethodUsageHandle, []byte(validUsage)); err != nil {
		t.Fatal(err)
	}
	old := p.runtime()
	done := make(chan struct{})
	go func() { p.Handle(protocol.MethodPluginQuiesce, nil); close(done) }()
	deadline := time.Now().Add(time.Second)
	for !old.Stopped() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !old.Stopped() {
		t.Fatal("quiesce did not close admission")
	}
	if _, err := p.Handle(protocol.MethodUsageHandle, []byte(validUsage)); err == nil {
		t.Fatal("draining collector admitted late event")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("quiesce did not finish draining")
	}
	db, initial, err := store.Open(p.cfg, testNow)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if initial.Committed != "1" || initial.Diagnostics["observed_events"] != "2" || initial.Diagnostics["dropped_stopped"] != "1" {
		t.Fatalf("callback delivered before final save was lost: %+v", initial)
	}
}

func TestConcurrentShutdownCountsEveryDeliveredCallback(t *testing.T) {
	p := newRegistered(t)
	old := p.runtime()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				p.Handle(protocol.MethodUsageHandle, []byte(validUsage))
				old.Snapshot()
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); p.Shutdown() }()
	}
	wg.Wait()
	if p.runtime() != old || !old.Stopped() {
		t.Fatal("shutdown detached stopped diagnostics")
	}
	d := old.Snapshot()["diagnostics"].(map[string]string)
	admitted, _ := strconv.Atoi(d["admitted_events"])
	stopped, _ := strconv.Atoi(d["dropped_stopped"])
	if d["observed_events"] != "800" || admitted+stopped != 800 || d["committed_events"] != d["admitted_events"] {
		t.Fatalf("shutdown race lost callbacks or admitted rows: %v", d)
	}
}

func waitReason(t *testing.T, p *Plugin, reason string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p.runtime().Snapshot()["reason"] == reason {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("reason %q not reached: %v", reason, p.runtime().Snapshot())
}

func assertReadableDegraded(t *testing.T, p *Plugin, reason string) {
	t.Helper()
	for _, route := range []string{"summary", "models"} {
		r := manage(t, p, "/v0/management/plugins/token-usage/"+route, query())
		if r.StatusCode != 200 || !strings.Contains(string(r.Body), `"input_tokens":"9007199254740993"`) || !strings.Contains(string(r.Body), `"degraded":true`) || !strings.Contains(string(r.Body), fmt.Sprintf(`"reason":%q`, reason)) {
			t.Fatalf("degraded %s did not expose committed data: %d %s", route, r.StatusCode, r.Body)
		}
	}
}

func TestDiskBudgetDoesNotHideCommittedManagementQueries(t *testing.T) {
	p := newRegistered(t)
	p.Handle(protocol.MethodUsageHandle, []byte(validUsage))
	waitCommitted(t, p, "1")
	// A sparse lock-file extension exercises actual footprint monitoring without
	// filling the filesystem or modifying committed SQLite history.
	if err := os.Truncate(p.cfg.DatabasePath+".lock", p.cfg.MaxDiskBytes+1); err != nil {
		t.Fatal(err)
	}
	waitReason(t, p, "disk_budget")
	if _, err := p.Handle(protocol.MethodUsageHandle, []byte(validUsage)); err == nil {
		t.Fatal("budget fault accepted new observation")
	}
	assertReadableDegraded(t, p, "disk_budget")
	if err := os.Truncate(p.cfg.DatabasePath+".lock", 0); err != nil {
		t.Fatal(err)
	}
	waitReason(t, p, "")
}

func TestMaintenanceFailureDoesNotHideCommittedManagementQueries(t *testing.T) {
	p := New()
	var at atomic.Int64
	at.Store(testNow.UnixNano())
	p.now = func() time.Time { return time.Unix(0, at.Load()) }
	t.Cleanup(p.Shutdown)
	path := filepath.Join(t.TempDir(), "private", "usage.sqlite")
	request, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte(fmt.Sprintf("database-path: %q\nraw-retention: 1h\nflush-interval: 10ms\nmaintenance-interval: 1s\n", path))})
	if _, err := p.Handle(protocol.MethodPluginRegister, request); err != nil {
		t.Fatal(err)
	}
	wire := strings.Replace(validUsage, "2026-09-09T00:00:00Z", testNow.Format(time.RFC3339Nano), 1)
	if _, err := p.Handle(protocol.MethodUsageHandle, []byte(wire)); err != nil {
		t.Fatal(err)
	}
	waitCommitted(t, p, "1")
	external, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer external.Close()
	if _, err := external.Exec("CREATE TRIGGER reject_cleanup BEFORE DELETE ON usage_events BEGIN SELECT RAISE(ABORT,'injected cleanup failure'); END"); err != nil {
		t.Fatal(err)
	}
	at.Add(int64(2 * time.Hour))
	fresh := p.now()
	wire = strings.Replace(validUsage, "2026-09-09T00:00:00Z", fresh.Format(time.RFC3339Nano), 1)
	if _, err := p.Handle(protocol.MethodUsageHandle, []byte(wire)); err != nil {
		t.Fatal(err)
	}
	waitCommitted(t, p, "2")
	at.Add(int64(time.Second))
	waitReason(t, p, "maintenance_unavailable")
	for _, route := range []string{"summary", "models"} {
		q := query()
		q.Set("from", fresh.Format(time.RFC3339Nano))
		q.Set("to", p.now().Format(time.RFC3339Nano))
		r := manage(t, p, "/v0/management/plugins/token-usage/"+route, q)
		if r.StatusCode != 200 || !strings.Contains(string(r.Body), `"input_tokens":"9007199254740993"`) || !strings.Contains(string(r.Body), `"reason":"maintenance_unavailable"`) || !strings.Contains(string(r.Body), `"degraded":true`) {
			t.Fatalf("maintenance fault hid committed %s: %d %s", route, r.StatusCode, r.Body)
		}
	}
	if _, err := external.Exec("DROP TRIGGER reject_cleanup"); err != nil {
		t.Fatal(err)
	}
	waitReason(t, p, "")
}
