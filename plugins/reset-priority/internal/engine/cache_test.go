package engine

import (
	"context"
	"encoding/json"
	quotaclient "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeQuotaFixture(t *testing.T, path string, observed time.Time) {
	t.Helper()
	snapshot := quotaclient.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]quotaclient.Entry{
		"claude:idx-a": {Provider: "claude", AuthIndex: "idx-a", Percent: 50, ResetAt: day(1), ObservedAt: observed},
	}}
	raw, _ := json.Marshal(snapshot)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCacheOnlyRankingAndMissingCacheNeverPollProvider(t *testing.T) {
	cfg := defaultConfig()
	cfg.DryRun = true
	cfg.QuotaCachePath = filepath.Join(t.TempDir(), "snapshot.json")
	env := newTestEnv(t, cfg)
	env.addAccount("claude", "a", day(2))
	writeQuotaFixture(t, cfg.QuotaCachePath, baseTime.Add(-time.Minute))
	env.reconcile()
	row, _ := env.statusRow("a")
	if row.ResetAt != day(1).Format(time.RFC3339) {
		t.Fatalf("cache deadline not used: %+v", row)
	}
	if err := os.Remove(cfg.QuotaCachePath); err != nil {
		t.Fatal(err)
	}
	env.reconcile()
	if len(env.claude.calls) != 0 {
		t.Fatal("cache consumer fell back to provider")
	}
	before := len(env.host.getCalls)
	env.eng.fetchOne(context.Background(), "idx-a")
	if len(env.host.getCalls) != before {
		t.Fatal("quota acquisition read credential tokens")
	}
}

func TestCacheObservationBeforeRecoveryCannotPromoteAccount(t *testing.T) {
	cfg := defaultConfig()
	cfg.DryRun = true
	cfg.QuotaCachePath = filepath.Join(t.TempDir(), "snapshot.json")
	env := newTestEnv(t, cfg)
	env.addAccount("claude", "a", day(2))
	writeQuotaFixture(t, cfg.QuotaCachePath, baseTime)
	env.reconcile()
	quarantineAccount(env, "idx-a")
	env.reconcile()
	env.clk.Advance(time.Minute)
	recoverAccount(env, "idx-a")
	env.reconcile()
	row, _ := env.statusRow("a")
	if row.Health == string(HealthHealthy) {
		t.Fatal("pre-recovery cached quota promoted account")
	}
	env.clk.Advance(time.Minute)
	writeQuotaFixture(t, cfg.QuotaCachePath, env.clk.Now())
	env.reconcile()
	row, _ = env.statusRow("a")
	if row.Health != string(HealthHealthy) {
		t.Fatalf("fresh observation did not recover: %+v", row)
	}
	if len(env.claude.calls) != 0 {
		t.Fatal("recovery polled provider")
	}
}
