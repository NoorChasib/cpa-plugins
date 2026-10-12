package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"gopkg.in/yaml.v3"
)

// route-api-credits is a plain boolean: true, false, absent and null are
// accepted, and anything else fails the configuration, which makes CPA drop the
// record and so fails open. Routing needs the plugin enabled too, and nothing
// else: dry-run and the manage-* flags do not touch it.
func TestRouteAPICreditsDecodes(t *testing.T) {
	for _, tc := range []struct {
		name, yaml   string
		key, routing bool
	}{
		{"absent", "enabled: true\n", false, false},
		{"null", "enabled: true\nroute-api-credits:\n", false, false},
		{"false", "enabled: true\nroute-api-credits: false\n", false, false},
		{"true", "enabled: true\nroute-api-credits: true\n", true, true},
		{"disabled", "enabled: false\nroute-api-credits: true\n", true, false},
		{"enabled absent", "route-api-credits: true\n", true, false},
		{"dry run", "enabled: true\ndry-run: true\nroute-api-credits: true\n", true, true},
		{"no providers managed", "enabled: true\nmanage-claude: false\nmanage-codex: false\nroute-api-credits: true\n", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tc.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.RouteAPICredits != tc.key || cfg.Routing() != tc.routing {
				t.Fatalf("route-api-credits = %v, routing = %v; want %v, %v", cfg.RouteAPICredits, cfg.Routing(), tc.key, tc.routing)
			}
		})
	}
	for _, value := range []string{"sometimes", "1", "[true]", "{on: true}"} {
		if _, err := Parse([]byte("enabled: true\nroute-api-credits: " + value + "\n")); err == nil {
			t.Errorf("route-api-credits: %s was accepted", value)
		}
	}
}

// The router reads quota-cache-path, else Quota Cache's default, whatever
// use-quota-cache says. The OAuth engine's own path keeps its rules
// (TestQuotaCacheOptIn): use-quota-cache: false clears it, and a bare path opts
// the engine in.
func TestRouterQuotaCachePathIgnoresUseQuotaCache(t *testing.T) {
	for _, tc := range []struct{ name, yaml, router, engine string }{
		{"empty", "", qc.DefaultPath, ""},
		{"standalone default", "enabled: true\n", qc.DefaultPath, ""},
		{"toggle on", "use-quota-cache: true\n", qc.DefaultPath, qc.DefaultPath},
		{"toggle off", "use-quota-cache: false\n", qc.DefaultPath, ""},
		{"custom path", "use-quota-cache: true\nquota-cache-path: /data/quota.json\n", "/data/quota.json", "/data/quota.json"},
		{"toggle off keeps the router's path", "use-quota-cache: false\nquota-cache-path: /data/quota.json\n", "/data/quota.json", ""},
		{"legacy opt in", "quota-cache-path: ' /data/quota.json '\n", "/data/quota.json", "/data/quota.json"},
		{"blank path", "quota-cache-path: '  '\n", qc.DefaultPath, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tc.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.RouterQuotaCachePath != tc.router || cfg.QuotaCachePath != tc.engine {
				t.Fatalf("router path %q, engine path %q; want %q, %q", cfg.RouterQuotaCachePath, cfg.QuotaCachePath, tc.router, tc.engine)
			}
		})
	}
}

// Keys is what the ConfigFields coverage test compares against, so it must
// name every key the decoder knows and nothing else.
func TestKeysListsEveryDecodedKey(t *testing.T) {
	keys := Keys()
	for _, want := range []string{
		"quota-cache-path", "use-quota-cache", "enabled", "priority", "reconcile-interval", "refresh-interval",
		"request-timeout", "priority-floor", "priority-step", "quarantine-priority", "manage-claude", "manage-codex",
		"providers", "dry-run", "display-timezone", "codex-reset-window-activation", "route-api-credits",
	} {
		found := false
		for _, key := range keys {
			found = found || key == want
		}
		if !found {
			t.Errorf("Keys() lacks %q: %v", want, keys)
		}
	}
	if len(keys) != 17 {
		t.Errorf("Keys() = %d keys, want 17: %s", len(keys), strings.Join(keys, ", "))
	}
}

// config.example.yaml is what operators copy, so its reset-priority block
// must parse, use only keys the decoder knows, and leave routing off.
func TestConfigExampleParses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := parseExample(t, "config.example.yaml", raw)
	if cfg.RouteAPICredits || cfg.Routing() || !cfg.DryRun {
		t.Fatalf("config.example.yaml: route-api-credits %v, dry-run %v", cfg.RouteAPICredits, cfg.DryRun)
	}
}

// Reset Priority saves credential priorities from its first start unless
// dry-run is already on, and CPA's form for its options stays empty until it
// has loaded. So the README's install steps put dry-run: true into the
// configuration before the step that installs, and so may load, the plugin,
// and its YAML enables the plugin with dry-run on.
func TestReadmeInstallsWithDryRunOn(t *testing.T) {
	if cfg, err := Parse(nil); err != nil || cfg.DryRun {
		t.Fatalf("with no configuration: dry-run %v, %v; want off, which is why the README sets it first", cfg.DryRun, err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, install, found := strings.Cut(string(raw), "\n## Install\n")
	if !found {
		t.Fatal(`README.md has no "Install" section`)
	}
	install, _, _ = strings.Cut(install, "\n## ")
	steps, block, found := strings.Cut(install, "```yaml\n")
	if found {
		block, _, found = strings.Cut(block, "```")
	}
	if !found {
		t.Fatal("README.md's Install section has no YAML block")
	}
	if cfg := parseExample(t, "README.md", []byte(block)); !cfg.Enabled || !cfg.DryRun || cfg.Routing() {
		t.Fatalf("README.md's YAML: enabled %v, dry-run %v, routing %v; want enabled with dry-run on and routing off", cfg.Enabled, cfg.DryRun, cfg.Routing())
	}
	steps = strings.Join(strings.Fields(steps), " ")
	dryRun := strings.Index(steps, "`dry-run: true`")
	if dryRun < 0 {
		t.Fatal("README.md's install steps never put `dry-run: true` into the configuration")
	}
	for _, start := range []string{"Plugin Store", "turn Reset Priority on", "turn it on", "enable the plugin"} {
		if i := strings.Index(steps, start); i >= 0 && i < dryRun {
			t.Errorf("README.md's install steps say %q, which can start the plugin, before they set `dry-run: true`", start)
		}
	}
}

// parseExample decodes plugins.configs.reset-priority from a configuration
// operators copy, which may set only keys the decoder knows.
func parseExample(t *testing.T, name string, raw []byte) Config {
	t.Helper()
	var example struct {
		Plugins struct {
			Configs map[string]yaml.Node `yaml:"configs"`
		} `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(raw, &example); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	block, ok := example.Plugins.Configs["reset-priority"]
	if !ok {
		t.Fatalf("%s has no plugins.configs.reset-priority", name)
	}
	known := map[string]bool{}
	for _, key := range Keys() {
		known[key] = true
	}
	for i := 0; i+1 < len(block.Content); i += 2 {
		if key := block.Content[i].Value; !known[key] {
			t.Errorf("%s sets %q, which the decoder does not know", name, key)
		}
	}
	subtree, err := yaml.Marshal(&block)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(subtree)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return cfg
}
