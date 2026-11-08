package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsAndHostKeys(t *testing.T) {
	c, err := Parse([]byte("enabled: true\npriority: 100\nstore:\n  source: x\n  version: 1.0.0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c != Default() {
		t.Fatalf("store-installed config changed runtime config: %+v", c)
	}
	// One 50 MB prompt peaks at about 530 MiB of plugin memory, so the default limit is 768 MiB.
	if c.MemoryLimitMiB != 768 {
		t.Fatalf("default memory-limit-mib = %d, want 768", c.MemoryLimitMiB)
	}
	if ok, why := c.Configured(); ok || why != "archive_url_missing" {
		t.Fatalf("configured = %v %s", ok, why)
	}
}

func TestStrictAndBounds(t *testing.T) {
	bad := map[string]string{
		"unknown key":     "archive-url: http://h:1\nbogus: 1\n",
		"userinfo":        "archive-url: http://u:p@h:1\n",
		"query":           "archive-url: http://h:1/?token=x\n",
		"path":            "archive-url: http://h:1/v1\n",
		"scheme":          "archive-url: ftp://h\n",
		"both token refs": "source-token-env: A\nsource-token-file: /x/y\n",
		"env name":        "hmac-key-env: lower\n",
		"relative file":   "hmac-key-file: rel/key\n",
		"key id":          "hmac-key-id: K1\n",
		"exact fp":        "exact-fingerprints: [abc]\n",
		"data dir":        "data-dir: /\n",
		"token-usage dir": "data-dir: /CLIProxyAPI/plugins/data/token-usage\n",
		"cap":             "outbox-cap-bytes: 1000\n",
		"retention":       "replay-retention-hours: 1\n",
		"memory":          "memory-limit-mib: 64\n",
		"workers":         "workers: 9\n",
		"rpc":             "max-rpc-mib: 4\n",
		"capture":         "capture: everything\n",
		"two docs":        "workers: 2\n---\nworkers: 3\n",
		"label too long":  "account-labels: {a: " + strings.Repeat("x", 200) + "}\n",
		"store not a map": "store: 3\n",
		"backlog":         "alert-backlog-hours: 0\n",
	}
	for name, doc := range bad {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "u:p") || strings.Contains(err.Error(), "token=x") {
			t.Errorf("%s: error echoes a value: %v", name, err)
		}
	}
	if _, err := Parse(nil); err == nil {
		t.Fatal("empty accepted")
	}
	if _, err := Parse(make([]byte, 65<<10)); err == nil {
		t.Fatal("oversize accepted")
	}
}

func TestEqualityAndLabels(t *testing.T) {
	a, err := Parse([]byte("archive-url: http://192.0.2.10:8790/\nsource-token-env: T\nhmac-key-env: K\naccount-labels: {b: two, a: one}\nexact-fingerprints: [" + strings.Repeat("b", 64) + ", " + strings.Repeat("a", 64) + "]\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Parse([]byte("archive-url: http://192.0.2.10:8790\nsource-token-env: T\nhmac-key-env: K\naccount-labels: {a: one, b: two}\nexact-fingerprints: [" + strings.Repeat("a", 64) + ", " + strings.Repeat("b", 64) + "]\n"))
	if a != b {
		t.Fatal("equal configs compare unequal")
	}
	if l := a.Labels(); l["a"] != "one" || l["b"] != "two" || len(a.ExactFPs()) != 2 {
		t.Fatalf("labels = %v", l)
	}
	if ok, _ := a.Configured(); !ok {
		t.Fatal("not configured")
	}
}

func TestResolveSecrets(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, []byte(strings.Repeat("k", 40)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CA_TEST_TOKEN", "cha_src_synthetic")
	c, err := Parse([]byte("archive-url: http://h:1\nsource-token-env: CA_TEST_TOKEN\nhmac-key-file: " + key + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Resolve()
	if err != nil || s.SourceToken != "cha_src_synthetic" || len(s.HMACKey) != 40 {
		t.Fatalf("resolve = %v", err)
	}
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Resolve(); err == nil || strings.Contains(err.Error(), "kkkk") {
		t.Fatalf("0644 key accepted or echoed: %v", err)
	}
	if err := os.WriteFile(key, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(key, 0o600)
	if _, err := c.Resolve(); err == nil {
		t.Fatal("short key accepted")
	}
	t.Setenv("CA_TEST_TOKEN", "")
	if _, err := c.Resolve(); err == nil {
		t.Fatal("unset env accepted")
	}
}
