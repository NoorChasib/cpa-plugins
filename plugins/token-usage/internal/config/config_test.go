package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// CPA v7.2.155's store-install output, excluding deployment-wide settings.
const installedStore = `enabled: true
store:
  id: token-usage
  name: Token Usage
  description: Persistent CPA-reported raw token usage by provider/model. Linux amd64 only; not billing or lossless accounting.
  author: NoorChasib
  version: 0.1.0
  release-tag: v0.1.0
  repository: https://github.com/NoorChasib/cpa-plugin-token-usage
  license: MIT
  source-id: source-97e4ec28aa27
  source-name: raw.githubusercontent.com
  source-url: https://raw.githubusercontent.com/NoorChasib/cpa-plugin-token-usage/main/registry.json
  install:
    type: github-release
`

func TestStoreInstalledMetadataDoesNotChangeRuntimeConfiguration(t *testing.T) {
	base := fmt.Sprintf("database-path: %q\n", filepath.Join(t.TempDir(), "private", "usage.sqlite"))
	want, err := Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse([]byte(base + installedStore))
	if err != nil || got != want {
		t.Fatalf("store-installed config must match runtime defaults: got %+v, err %v", got, err)
	}
}

func TestStoreInstalledConfigurationMayOmitDatabasePath(t *testing.T) {
	c, err := Parse([]byte(installedStore))
	if err != nil || c.DatabasePath != "" || c.QueueCapacity != 8192 || c.BatchSize != 256 {
		t.Fatalf("omitted database-path must leave resolution to the native instance: %+v %v", c, err)
	}
}

func TestDefaultDoesNotAcceptMalformedOrUnboundedConfiguration(t *testing.T) {
	for _, raw := range []string{"null", "---", " ", "# comment only", "[]", "true", "store: secret-canary", "store: [secret-canary]", "store: {}\nunknown-secret-canary: true", "store:\n  note: " + strings.Repeat("x", 64<<10)} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Fatalf("accepted malformed or unbounded configuration beginning %.80q", raw)
		} else if strings.Contains(err.Error(), "secret-canary") {
			t.Fatalf("configuration error echoed host identity: %v", err)
		}
	}
}

func TestStrictConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "usage.sqlite")
	base := fmt.Sprintf("database-path: %q\n", path)
	c, err := Parse([]byte(base))
	if err != nil || c.QueueCapacity != 8192 || c.BatchSize != 256 {
		t.Fatalf("defaults %+v %v", c, err)
	}
	invalid := []string{"", "database-path: relative.sqlite", base + "unknown: true", base + "queue-capacity: 0", base + "batch-size: 8193", base + "flush-interval: 0s", base + "raw-retention: 999999999999h", base + "max-disk-bytes: 9223372036854775808", base + "max-models: 0", base + "query-timeout: 31s", base + "---\n{}", base + "database-path: /another/path"}
	for _, raw := range invalid {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid configuration %q", raw)
		}
	}
	a, err := Parse([]byte(base + "enabled: true\npriority: 20"))
	if err != nil || a != c {
		t.Fatalf("CPA selection fields changed runtime config: %+v %v", a, err)
	}
}
