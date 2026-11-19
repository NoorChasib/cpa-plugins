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

func TestParseClientDefaults(t *testing.T) {
	settings, err := Parse([]byte("enabled: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	a, c := settings.Antigravity, settings.ClaudeCode
	if a.Configured || a.Err != "" || a.Source != FromConfig || a.Rules.EnableNew() || len(a.Rules.Switches()) != 0 || a.DefaultModel != "" || a.TitleModel != "" {
		t.Fatalf("antigravity defaults = %+v", a)
	}
	if c.Configured || c.Err != "" || c.Source != FromConfig || !c.Filter || !c.Rules.EnableNew() || len(c.Rules.Switches()) != 0 {
		t.Fatalf("claude-code defaults = %+v", c)
	}
	// A null value, as left by a PATCH that deletes the key, is absent.
	settings, err = Parse([]byte("antigravity: null\nclaude-code:\n"))
	if err != nil || settings.Antigravity.Configured || settings.ClaudeCode.Configured || settings.Antigravity.Err != "" || settings.ClaudeCode.Err != "" {
		t.Fatalf("null subtrees = %+v, %v", settings, err)
	}
}

func TestParseClientSubtrees(t *testing.T) {
	settings, err := Parse([]byte(`models: {gpt-x: false}
antigravity:
  models:
    gemini-4-argon-high: true
    grok-4.7: false
  new-models: enabled
  default-model: gemini-4-argon-high
  title-model: gemini-3.1-flash-lite
claude-code:
  filter: false
  models: {gpt-6-sol: false}
  new-models: disabled
`))
	if err != nil {
		t.Fatal(err)
	}
	a := settings.Antigravity
	if !a.Configured || a.Err != "" || !a.Rules.EnableNew() || a.DefaultModel != "gemini-4-argon-high" || a.TitleModel != "gemini-3.1-flash-lite" {
		t.Fatalf("antigravity = %+v", a)
	}
	if on, ok := a.Rules.Switched("grok-4.7"); !ok || on {
		t.Fatal("grok-4.7 switch lost")
	}
	c := settings.ClaudeCode
	if !c.Configured || c.Err != "" || c.Filter || c.Rules.EnableNew() || c.Rules.Allows("gpt-6-sol") {
		t.Fatalf("claude-code = %+v", c)
	}
	// Codex keeps its own switches.
	if settings.Rules.Allows("gpt-x") || !settings.Rules.Allows("grok-4.7") {
		t.Fatal("codex switches mixed with a client's")
	}
}

// A bad client subtree never fails Parse, so Codex keeps working; the client
// reports its error and falls back to its defaults.
func TestParseBadClientSubtreeDisablesOnlyThatClient(t *testing.T) {
	for name, doc := range map[string]string{
		"antigravity not mapping":    "antigravity: yes\n",
		"antigravity list":           "antigravity: [a]\n",
		"antigravity unknown key":    "antigravity: {modles: {a: true}}\n",
		"antigravity retired key":    "antigravity: {order: [a]}\n",
		"antigravity bad switch":     "antigravity: {models: {a: maybe}}\n",
		"antigravity string switch":  "antigravity: {models: {a: 'true'}}\n",
		"antigravity bad new-models": "antigravity: {new-models: sometimes}\n",
		"antigravity padded default": "antigravity: {default-model: ' x'}\n",
		"antigravity long title":     "antigravity: {title-model: '" + strings.Repeat("a", maxSlugBytes+1) + "'}\n",
		"antigravity default list":   "antigravity: {default-model: [a]}\n",
		"antigravity empty slug":     "antigravity: {models: {'': true}}\n",
		"antigravity duplicate key":  "antigravity:\n  new-models: enabled\n  new-models: disabled\n",
		"claude not mapping":         "claude-code: true\n",
		"claude unknown key":         "claude-code: {filters: true}\n",
		"claude bad filter":          "claude-code: {filter: maybe}\n",
		"claude string filter":       "claude-code: {filter: 'false'}\n",
		"claude bad switch":          "claude-code: {models: {a: 1}}\n",
		"claude bad new-models":      "claude-code: {new-models: off}\n",
		// YAML 1.1 words, which CPA writes unquoted for a PATCHed JSON string
		// and yaml.v3 would decode into a bool, are not booleans here.
		"antigravity yes switch":   "antigravity: {models: {a: yes}}\n",
		"antigravity off switch":   "antigravity: {models: {a: off}}\n",
		"claude yes filter":        "claude-code: {filter: yes}\n",
		"claude no filter":         "claude-code: {filter: no}\n",
		"claude on switch":         "claude-code: {models: {a: on}}\n",
		"antigravity number id":    "antigravity: {default-model: 5}\n",
		"antigravity bool title":   "antigravity: {title-model: true}\n",
		"antigravity number key":   "antigravity: {models: {1234: true}}\n",
		"antigravity models list":  "antigravity: {models: [a]}\n",
		"claude number new-models": "claude-code: {new-models: 1}\n",
		"claude filter mapping":    "claude-code: {filter: {a: true}}\n",
		"antigravity alias switch": "antigravity: {models: {a: &t true, b: *t}}\n",
		"claude explicit bool tag": "claude-code: {filter: !!bool yes}\n",
		"antigravity sequence key": "antigravity: {[a]: true}\n",
	} {
		settings, err := Parse([]byte("models: {gpt-x: false}\n" + doc))
		if err != nil {
			t.Errorf("%s: Parse failed: %v", name, err)
			continue
		}
		if settings.Rules.Allows("gpt-x") {
			t.Errorf("%s: codex switches lost", name)
		}
		var client string
		var source Source
		if strings.HasPrefix(name, "antigravity") {
			client, source = settings.Antigravity.Err, settings.Antigravity.Source
			if !settings.Antigravity.Configured || settings.Antigravity.Rules.EnableNew() || settings.ClaudeCode.Err != "" {
				t.Errorf("%s: %+v", name, settings.Antigravity)
			}
		} else {
			client, source = settings.ClaudeCode.Err, settings.ClaudeCode.Source
			if !settings.ClaudeCode.Configured || !settings.ClaudeCode.Filter || settings.Antigravity.Err != "" {
				t.Errorf("%s: %+v", name, settings.ClaudeCode)
			}
		}
		if client == "" || source != FromDefaults {
			t.Errorf("%s: error %q source %q", name, client, source)
		}
	}
}

func TestParseClientSubtreeErrorsNameTheKey(t *testing.T) {
	for doc, want := range map[string]string{
		"antigravity: {bogus: 1}\n":                            `antigravity: unknown key "bogus"`,
		"claude-code: {extra: true}\n":                         `claude-code: unknown key "extra"`,
		"antigravity: {" + strings.Repeat("k", 900) + ": 1}\n": `antigravity: unknown key "` + strings.Repeat("k", 64) + `"...`,
		"claude-code:\n  filter: true\n  filter: false\n":      `claude-code: duplicate key "filter"`,
		"antigravity: {models: {gemini-4-argon: maybe}}\n":     `antigravity: models: the switch for "gemini-4-argon" must be true or false`,
		"claude-code: {filter: yes}\n":                         `claude-code: filter must be true or false`,
		"antigravity: {default-model: 5}\n":                    `antigravity: default-model must be a string`,
	} {
		settings, err := Parse([]byte(doc))
		if err != nil {
			t.Fatalf("%q: %v", doc, err)
		}
		got := settings.Antigravity.Err + settings.ClaudeCode.Err
		if got != want {
			t.Errorf("%q: error %q, want %q", doc, got, want)
		}
	}
}

// Real booleans and strings, in any form CPA or a person writes them, and
// null fields are accepted.
func TestParseClientSubtreesAcceptRealBooleansAndNull(t *testing.T) {
	settings, err := Parse([]byte("antigravity: {models: {\"1234\": True, 'yes': FALSE}, new-models: null, default-model: \"5\"}\nclaude-code: {filter: null, models: null}\n"))
	if err != nil {
		t.Fatal(err)
	}
	a, c := settings.Antigravity, settings.ClaudeCode
	if a.Err != "" || c.Err != "" || a.DefaultModel != "5" || a.Rules.EnableNew() || !c.Filter || !c.Rules.EnableNew() {
		t.Fatalf("antigravity %+v, claude-code %+v", a, c)
	}
	if on, ok := a.Rules.Switched("1234"); !ok || !on {
		t.Fatal("a quoted numeric id is a string id")
	}
	if on, ok := a.Rules.Switched("yes"); !ok || on {
		t.Fatal("a quoted yes is a string id")
	}
}

func TestParseStillRejectsUnknownTopLevelKeys(t *testing.T) {
	for _, doc := range []string{"clients: {antigravity: {}}\n", "agy: {}\n", "claude_code: {}\n"} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%q accepted", doc)
		}
	}
}
