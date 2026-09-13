package monitor

import (
	"context"
	"encoding/json"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/config"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/protocol"
	quotaclient "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheOnlyQuotaReadsNeverFallBackToProvider(t *testing.T) {
	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "snapshot.json")
	host := newQuotaHost()
	m := &Monitor{cfg: config.Config{QuotaCachePath: path}, quotaHost: host}
	entry := protocol.HostAuthFileEntry{Provider: "claude", AuthIndex: "one"}
	if _, err := m.fetchQuota(context.Background(), entry, now); err == nil {
		t.Fatal("missing cache accepted")
	}
	snapshot := quotaclient.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]quotaclient.Entry{
		"claude:one": {Provider: "claude", AuthIndex: "one", Percent: 95, ResetAt: now.Add(time.Hour), ObservedAt: now.Add(-time.Minute)},
	}}
	raw, _ := json.Marshal(snapshot)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		observation, err := m.fetchQuota(context.Background(), entry, now)
		if err != nil || observation.Percent != 95 || !observation.ObservedAt.Equal(now.Add(-time.Minute)) {
			t.Fatalf("%+v %v", observation, err)
		}
	}
	if _, err := m.fetchQuota(context.Background(), entry, now.Add(time.Hour)); err == nil {
		t.Fatal("stale cache accepted")
	}
	if host.authCalls != 0 || host.requestCount() != 0 {
		t.Fatal("cache-only consumer accessed provider credentials or HTTP")
	}
}
