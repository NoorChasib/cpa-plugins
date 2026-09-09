package config

import (
	"fmt"
	"path/filepath"
	"testing"
)

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
