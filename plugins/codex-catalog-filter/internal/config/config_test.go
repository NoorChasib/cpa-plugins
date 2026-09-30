package config

import (
	"fmt"
	"strings"
	"testing"
)

const catalogBody = `{"models":[{"slug":"gpt-6-sol","visibility":"list"},{"slug":"claude-x","visibility":"list"},{"slug":"codex-auto-review","visibility":"hide"}]}`

func TestParseDefaults(t *testing.T) {
	settings, err := Parse([]byte("enabled: true\npriority: 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	r := settings.Rules
	if !r.Idle() || !r.EnableNew() || r.Action() != "remove" || settings.CPAURL != DefaultCPAURL || settings.DataDir != "" {
		t.Fatalf("defaults = %+v", settings)
	}
	if r.Rewrite([]byte(catalogBody)) != nil {
		t.Fatal("no switches must leave the catalog unchanged")
	}
}

func TestParseSwitches(t *testing.T) {
	settings, err := Parse([]byte("models:\n  claude-x: false\n  gpt-5.5: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(settings.Rules.Rewrite([]byte(catalogBody)))
	if got != `{"models":[{"slug":"gpt-6-sol","visibility":"list"},{"slug":"codex-auto-review","visibility":"hide"}]}` {
		t.Fatalf("switch not applied: %s", got)
	}
	if enabled, ok := settings.Rules.Switched("gpt-5.5"); !ok || !enabled {
		t.Fatal("explicit on switch lost")
	}
}

func TestParseNewModelsAndAction(t *testing.T) {
	settings, err := Parse([]byte("new-models: disabled\naction: hide\nmodels:\n  gpt-6-sol: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(settings.Rules.Rewrite([]byte(catalogBody)))
	if got != `{"models":[{"slug":"gpt-6-sol","visibility":"list"},{"slug":"claude-x","visibility":"hide"},{"slug":"codex-auto-review","visibility":"hide"}]}` {
		t.Fatalf("new-models/action not applied: %s", got)
	}
}

func TestParseCPAURL(t *testing.T) {
	for doc, want := range map[string]string{
		"enabled: true\n":                            DefaultCPAURL,
		"cpa-url: http://127.0.0.1:9000\n":           "http://127.0.0.1:9000",
		"cpa-url: https://cpa.internal:8317/\n":      "https://cpa.internal:8317",
		"cpa-url: ''\nmodels: {claude-x: false}\n":   DefaultCPAURL,
		"models: {claude-x: false}\ncpa-url: null\n": DefaultCPAURL,
	} {
		settings, err := Parse([]byte(doc))
		if err != nil || settings.CPAURL != want {
			t.Errorf("%q: got %+v, %v; want %s", doc, settings, err, want)
		}
	}
	for _, value := range []string{"127.0.0.1:8317", "ftp://127.0.0.1", "http://", "http://127.0.0.1:8317/v1", "http://127.0.0.1:8317?x=1", "http://user:pass@127.0.0.1:8317", "http://127.0.0.1:8317#f"} {
		if _, err := Parse([]byte("cpa-url: '" + value + "'\n")); err == nil {
			t.Errorf("cpa-url %q accepted", value)
		}
	}
}

func TestParseDataDir(t *testing.T) {
	settings, err := Parse([]byte("data-dir: /CLIProxyAPI/plugins/data/codex-catalog-filter\n"))
	if err != nil || settings.DataDir != "/CLIProxyAPI/plugins/data/codex-catalog-filter" {
		t.Fatalf("data-dir = %+v, %v", settings, err)
	}
	for _, value := range []string{"relative/dir", "/tmp/../etc", "/tmp/x/", "/"} {
		if _, err := Parse([]byte("data-dir: '" + value + "'\n")); err == nil {
			t.Errorf("data-dir %q accepted", value)
		}
	}
}

func TestParseToleratesHostStoreMetadata(t *testing.T) {
	doc := `enabled: true
priority: 0
models:
  claude-x: false
store:
  schema-version: 2
  id: codex-catalog-filter
  version: 0.1.0
  source-url: https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/preview/registry.json
  install:
    type: direct
    artifacts:
      - goos: linux
        goarch: amd64
`
	if _, err := Parse([]byte(doc)); err != nil {
		t.Fatal(err)
	}
}

func TestParseRejectsInvalidConfiguration(t *testing.T) {
	var many strings.Builder
	many.WriteString("models:\n")
	for i := range maxSwitches + 1 {
		fmt.Fprintf(&many, "  m-%d: false\n", i)
	}
	cases := map[string]string{
		"empty":               "",
		"unknown key":         "modles:\n  claude-x: false\n",
		"retired include key": "include: ['gpt-*']\n",
		"unknown action":      "action: drop\n",
		"unknown new-models":  "new-models: maybe\n",
		"non-boolean switch":  "models:\n  claude-x: maybe\n",
		"string switch":       "models:\n  claude-x: 'false'\n",
		"models not a map":    "models: [claude-x]\n",
		"empty slug":          "models:\n  '': false\n",
		"padded slug":         "models:\n  ' claude-x': false\n",
		"long slug":           "models:\n  '" + strings.Repeat("a", maxSlugBytes+1) + "': false\n",
		"too many switches":   many.String(),
		"store not mapping":   "store: yes\n",
		"two documents":       "models: {a: false}\n---\nmodels: {b: false}\n",
		"not a mapping":       "- claude-x\n",
		"oversized document":  "enabled: true\n#" + strings.Repeat("x", maxDocumentBytes) + "\n",
	}
	for name, doc := range cases {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
