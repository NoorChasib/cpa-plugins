package config

import (
	"fmt"
	"strings"
	"testing"
)

const catalogBody = `{"models":[{"slug":"gpt-6-sol","visibility":"list"},{"slug":"claude-x","visibility":"list"},{"slug":"codex-auto-review","visibility":"hide"}]}`

func TestParseDefaultsToRemove(t *testing.T) {
	settings, err := Parse([]byte("enabled: true\npriority: 3\ninclude: [\"gpt-[0-9]*\", \"codex-*\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(settings.Rules.Rewrite([]byte(catalogBody)))
	if got != `{"models":[{"slug":"gpt-6-sol","visibility":"list"},{"slug":"codex-auto-review","visibility":"hide"}]}` {
		t.Fatalf("default action did not remove: %s", got)
	}
}

func TestParseHide(t *testing.T) {
	settings, err := Parse([]byte("include:\n  - gpt-[0-9]*\n  - codex-*\naction: hide\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(settings.Rules.Rewrite([]byte(catalogBody)))
	if !strings.Contains(got, `{"slug":"claude-x","visibility":"hide"}`) {
		t.Fatalf("hide action not applied: %s", got)
	}
}

func TestParseExclude(t *testing.T) {
	settings, err := Parse([]byte("include: ['*']\nexclude: ['claude-*']\naction: remove\n"))
	if err != nil {
		t.Fatal(err)
	}
	if settings.Rules.Allows("claude-x") || !settings.Rules.Allows("gpt-6-sol") {
		t.Fatal("exclude not applied")
	}
}

func TestParseWithoutIncludeIsIdle(t *testing.T) {
	for _, doc := range []string{"enabled: true\n", "enabled: true\ninclude: []\n", "exclude: ['claude-*']\n"} {
		settings, err := Parse([]byte(doc))
		if err != nil {
			t.Fatalf("%q: %v", doc, err)
		}
		if !settings.Rules.Idle() || settings.Rules.Rewrite([]byte(catalogBody)) != nil {
			t.Fatalf("%q should leave catalogs unchanged", doc)
		}
	}
}

func TestParseCPAURL(t *testing.T) {
	for doc, want := range map[string]string{
		"include: ['gpt-*']\n":                                      DefaultCPAURL,
		"include: ['gpt-*']\ncpa-url: http://127.0.0.1:9000\n":      "http://127.0.0.1:9000",
		"include: ['gpt-*']\ncpa-url: https://cpa.internal:8317/\n": "https://cpa.internal:8317",
	} {
		settings, err := Parse([]byte(doc))
		if err != nil || settings.CPAURL != want {
			t.Errorf("%q: got %+v, %v; want %s", doc, settings, err, want)
		}
	}
	for _, value := range []string{"127.0.0.1:8317", "ftp://127.0.0.1", "http://", "http://127.0.0.1:8317/v1", "http://127.0.0.1:8317?x=1", "http://user:pass@127.0.0.1:8317", "http://127.0.0.1:8317#f"} {
		if _, err := Parse([]byte("include: ['gpt-*']\ncpa-url: '" + value + "'\n")); err == nil {
			t.Errorf("cpa-url %q accepted", value)
		}
	}
}

func TestParseToleratesHostStoreMetadata(t *testing.T) {
	doc := `enabled: true
priority: 0
include: ["gpt-*"]
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
	many := make([]string, maxPatterns+1)
	for i := range many {
		many[i] = fmt.Sprintf("'m-%d'", i)
	}
	cases := map[string]string{
		"empty":              "",
		"misspelled key":     "inlcude: ['gpt-*']\n",
		"unknown action":     "include: ['gpt-*']\naction: drop\n",
		"scalar include":     "include: gpt-*\n",
		"malformed glob":     "include: ['gpt-[0-9']\n",
		"malformed exclude":  "include: ['gpt-*']\nexclude: ['[']\n",
		"empty pattern":      "include: ['']\n",
		"padded pattern":     "include: [' gpt-*']\n",
		"long pattern":       "include: ['" + strings.Repeat("a", maxPatternBytes+1) + "']\n",
		"too many patterns":  "include: [" + strings.Join(many, ",") + "]\n",
		"store not mapping":  "include: ['gpt-*']\nstore: yes\n",
		"two documents":      "include: ['gpt-*']\n---\ninclude: ['x']\n",
		"not a mapping":      "- gpt-*\n",
		"oversized document": "include: ['gpt-*']\n#" + strings.Repeat("x", maxDocumentBytes) + "\n",
	}
	for name, doc := range cases {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
