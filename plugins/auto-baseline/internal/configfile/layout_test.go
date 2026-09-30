package configfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NoorChasib/cpa-plugins/plugins/auto-baseline/internal/fingerprint"
)

// Layout fixtures shared by the read, write, and export tests. Every one
// enables the plugin on disk so Apply's callers can use them unchanged.
const layoutPlugins = `plugins:
  enabled: true
  configs:
    auto-baseline:
      enabled: true
`

const (
	fixtureLegacyOnly = `claude-header-defaults:
  user-agent: "claude-cli/2.1.250 (external, cli)"
  package-version: "0.100.0"
  runtime-version: "v24.0.0"
codex-header-defaults:
  user-agent: "codex-tui/0.150.0 (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 (codex-tui; 0.150.0)"
codex:
  disable-codex-cloaking: true
` + layoutPlugins

	fixtureV8Only = `config-version: 8
server:
  port: 8317
oauth:
  providers:
    claude:
      # measured baseline
      header-defaults:
        user-agent: "claude-cli/2.1.285 (external, cli)"
        package-version: "0.112.1"
        runtime-version: "v26.3.0"
    codex:
      disable-codex-cloaking: true
      header-defaults:
        user-agent: "codex-tui/0.155.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.155.0)"
` + layoutPlugins

	// fixtureInterim is the production stop-gap: v8 layout, but the three
	// settings the plugin reads kept at their legacy root keys.
	fixtureInterim = `config-version: 8
server:
  port: 8317
oauth:
  auth-dir: "/root/.cli-proxy-api"
claude-header-defaults:
  user-agent: "claude-cli/2.1.285 (external, cli)"
  package-version: "0.112.1"
  runtime-version: "v26.3.0"
codex-header-defaults:
  user-agent: "codex-tui/0.155.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.155.0)"
codex:
  disable-codex-cloaking: true
` + layoutPlugins

	fixtureV8Empty     = "config-version: 8\nserver:\n  port: 8317\n" + layoutPlugins
	fixtureLegacyEmpty = "port: 8317\n" + layoutPlugins

	// fixtureV8BlockLegacyFile has a v8 block but no config-version.
	fixtureV8BlockLegacyFile = `port: 8317
oauth:
  providers:
    claude:
      header-defaults:
        user-agent: "claude-cli/2.1.285 (external, cli)"
` + layoutPlugins

	// fixtureMixed: the v8 block sets only user-agent; package and runtime
	// still come from the legacy block, and os is legacy-only.
	fixtureMixed = `config-version: 8
oauth:
  providers:
    claude:
      header-defaults:
        user-agent: "claude-cli/2.1.286 (external, cli)"
claude-header-defaults:
  user-agent: "claude-cli/2.1.250 (external, cli)"
  package-version: "0.100.0"
  runtime-version: "v24.0.0"
  os: "Linux"
` + layoutPlugins

	fixtureV8BlankWins = `config-version: 8
oauth:
  providers:
    claude:
      header-defaults:
        user-agent: ""
        package-version:
    codex:
      disable-codex-cloaking: false
claude-header-defaults:
  user-agent: "claude-cli/2.1.250 (external, cli)"
  package-version: "0.100.0"
codex:
  disable-codex-cloaking: true
` + layoutPlugins

	fixtureV8NullCloaking = `config-version: 8
oauth:
  providers:
    codex:
      disable-codex-cloaking:
codex:
  disable-codex-cloaking: true
` + layoutPlugins

	fixtureV8Aliases = `config-version: 8
x-shared:
  claude: &claudehd
    user-agent: "claude-cli/2.1.287 (external, cli)"
    package-version: "0.112.2"
  base: &base
    package-version: "0.112.3"
    runtime-version: "v26.4.0"
oauth:
  providers:
    claude:
      header-defaults: *claudehd
    codex:
      header-defaults:
        <<: *base
        user-agent: "codex-tui/0.156.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.156.0)"
` + layoutPlugins

	fixtureV8Merge = `config-version: 8
x-base: &base
  package-version: "0.112.3"
  runtime-version: "v26.4.0"
oauth:
  providers:
    claude:
      header-defaults:
        <<: *base
        user-agent: "claude-cli/2.1.288 (external, cli)"
claude-header-defaults:
  package-version: "0.100.0"
` + layoutPlugins

	fixtureV8NullParent = `config-version: 8
oauth:
  providers:
    claude: ~
claude-header-defaults:
  user-agent: "claude-cli/2.1.250 (external, cli)"
` + layoutPlugins
)

type leafWant struct {
	value  string
	source Source
}

func legacyLeaf(v string) leafWant { return leafWant{v, SourceLegacy} }
func v8Leaf(v string) leafWant     { return leafWant{v, SourceV8} }

var (
	defaultClaudeUA  = leafWant{fingerprint.CompiledClaudeUserAgent, SourceDefault}
	defaultClaudePkg = leafWant{fingerprint.CompiledClaudePackageVersion, SourceDefault}
	defaultClaudeRT  = leafWant{fingerprint.CompiledClaudeRuntimeVersion, SourceDefault}
	defaultCodexUA   = leafWant{fingerprint.CompiledCodexUserAgent, SourceDefault}
)

type readCase struct {
	name                 string
	yaml                 string
	layout               Layout
	claudeUA             leafWant
	claudePkg            leafWant
	claudeRT             leafWant
	claudeTarget         string
	codexUA              leafWant
	codexTarget          string
	cloaking             bool
	cloakingSource       Source
	claudeUnsupported    string
	claudeWantsNoCompare bool
}

func readCases() []readCase {
	return []readCase{
		{
			name: "legacy-only", yaml: fixtureLegacyOnly, layout: LayoutLegacy,
			claudeUA: legacyLeaf("claude-cli/2.1.250 (external, cli)"), claudePkg: legacyLeaf("0.100.0"), claudeRT: legacyLeaf("v24.0.0"),
			claudeTarget: "claude-header-defaults",
			codexUA:      legacyLeaf("codex-tui/0.150.0 (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 (codex-tui; 0.150.0)"), codexTarget: "codex-header-defaults",
			cloaking: true, cloakingSource: SourceLegacy,
		},
		{
			name: "v8-only", yaml: fixtureV8Only, layout: LayoutV8,
			claudeUA: v8Leaf("claude-cli/2.1.285 (external, cli)"), claudePkg: v8Leaf("0.112.1"), claudeRT: v8Leaf("v26.3.0"),
			claudeTarget: "oauth.providers.claude.header-defaults",
			codexUA:      v8Leaf("codex-tui/0.155.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.155.0)"), codexTarget: "oauth.providers.codex.header-defaults",
			cloaking: true, cloakingSource: SourceV8,
		},
		{
			name: "interim-v8-file-with-legacy-header-keys", yaml: fixtureInterim, layout: LayoutV8,
			claudeUA: legacyLeaf("claude-cli/2.1.285 (external, cli)"), claudePkg: legacyLeaf("0.112.1"), claudeRT: legacyLeaf("v26.3.0"),
			claudeTarget: "claude-header-defaults",
			codexUA:      legacyLeaf("codex-tui/0.155.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.155.0)"), codexTarget: "codex-header-defaults",
			cloaking: true, cloakingSource: SourceLegacy,
		},
		{
			name: "v8-layout-without-blocks", yaml: fixtureV8Empty, layout: LayoutV8,
			claudeUA: defaultClaudeUA, claudePkg: defaultClaudePkg, claudeRT: defaultClaudeRT,
			claudeTarget: "oauth.providers.claude.header-defaults",
			codexUA:      defaultCodexUA, codexTarget: "oauth.providers.codex.header-defaults",
			cloakingSource: SourceDefault,
		},
		{
			name: "legacy-layout-without-blocks", yaml: fixtureLegacyEmpty, layout: LayoutLegacy,
			claudeUA: defaultClaudeUA, claudePkg: defaultClaudePkg, claudeRT: defaultClaudeRT,
			claudeTarget: "claude-header-defaults",
			codexUA:      defaultCodexUA, codexTarget: "codex-header-defaults",
			cloakingSource: SourceDefault,
		},
		{
			name: "v8-block-without-config-version", yaml: fixtureV8BlockLegacyFile, layout: LayoutLegacy,
			claudeUA: v8Leaf("claude-cli/2.1.285 (external, cli)"), claudePkg: defaultClaudePkg, claudeRT: defaultClaudeRT,
			claudeTarget: "oauth.providers.claude.header-defaults",
			codexUA:      defaultCodexUA, codexTarget: "codex-header-defaults",
			cloakingSource: SourceDefault,
		},
		{
			name: "mixed-per-leaf", yaml: fixtureMixed, layout: LayoutV8,
			claudeUA: v8Leaf("claude-cli/2.1.286 (external, cli)"), claudePkg: legacyLeaf("0.100.0"), claudeRT: legacyLeaf("v24.0.0"),
			claudeTarget: "oauth.providers.claude.header-defaults",
			codexUA:      defaultCodexUA, codexTarget: "oauth.providers.codex.header-defaults",
			cloakingSource: SourceDefault,
		},
		{
			// A present v8 leaf wins even when it is blank, null, or false:
			// the legacy values are ignored and CPA's compiled defaults apply.
			name: "v8-blank-null-and-false-win", yaml: fixtureV8BlankWins, layout: LayoutV8,
			claudeUA: defaultClaudeUA, claudePkg: defaultClaudePkg, claudeRT: defaultClaudeRT,
			claudeTarget: "oauth.providers.claude.header-defaults",
			codexUA:      defaultCodexUA, codexTarget: "oauth.providers.codex.header-defaults",
			cloaking: false, cloakingSource: SourceV8,
		},
		{
			name: "v8-null-cloaking-wins", yaml: fixtureV8NullCloaking, layout: LayoutV8,
			claudeUA: defaultClaudeUA, claudePkg: defaultClaudePkg, claudeRT: defaultClaudeRT,
			claudeTarget: "oauth.providers.claude.header-defaults",
			codexUA:      defaultCodexUA, codexTarget: "oauth.providers.codex.header-defaults",
			cloaking: false, cloakingSource: SourceV8,
		},
		{
			name: "v8-aliases-and-merge-keys", yaml: fixtureV8Aliases, layout: LayoutV8,
			claudeUA: v8Leaf("claude-cli/2.1.287 (external, cli)"), claudePkg: v8Leaf("0.112.2"), claudeRT: defaultClaudeRT,
			claudeTarget: "oauth.providers.claude.header-defaults",
			codexUA:      v8Leaf("codex-tui/0.156.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.156.0)"), codexTarget: "oauth.providers.codex.header-defaults",
			cloakingSource: SourceDefault,
		},
		{
			name: "v8-merge-key-beats-legacy", yaml: fixtureV8Merge, layout: LayoutV8,
			claudeUA: v8Leaf("claude-cli/2.1.288 (external, cli)"), claudePkg: v8Leaf("0.112.3"), claudeRT: v8Leaf("v26.4.0"),
			claudeTarget: "oauth.providers.claude.header-defaults",
			codexUA:      defaultCodexUA, codexTarget: "oauth.providers.codex.header-defaults",
			cloakingSource: SourceDefault,
		},
		{
			// CPA refuses to load a file whose v8 parent is not a mapping.
			name: "v8-parent-not-a-mapping", yaml: fixtureV8NullParent, layout: LayoutV8,
			claudeUnsupported: "unsupported_config_shape", claudeWantsNoCompare: true,
			codexUA: defaultCodexUA, codexTarget: "oauth.providers.codex.header-defaults",
			cloakingSource: SourceDefault,
		},
	}
}

func checkLeaf(t *testing.T, what string, gotValue string, gotSource Source, want leafWant) {
	t.Helper()
	if gotValue != want.value || gotSource != want.source {
		t.Errorf("%s = %q from %s, want %q from %s", what, gotValue, gotSource, want.value, want.source)
	}
}

func TestReadResolvesBothLayoutsLeafByLeaf(t *testing.T) {
	for _, tc := range readCases() {
		t.Run(tc.name, func(t *testing.T) {
			p := newPaths(t, tc.yaml)
			snap, err := Read(p.config)
			if err != nil {
				t.Fatal(err)
			}
			if snap.Layout != tc.layout {
				t.Errorf("layout = %s, want %s", snap.Layout, tc.layout)
			}
			if tc.claudeUnsupported != "" {
				if snap.Claude.Unsupported != tc.claudeUnsupported {
					t.Errorf("claude unsupported = %q, want %q", snap.Claude.Unsupported, tc.claudeUnsupported)
				}
			} else {
				c := snap.Claude
				checkLeaf(t, "claude user-agent", c.UserAgent, c.UserAgentSource, tc.claudeUA)
				checkLeaf(t, "claude package-version", c.PackageVersion, c.PackageVersionSource, tc.claudePkg)
				checkLeaf(t, "claude runtime-version", c.RuntimeVersion, c.RuntimeVersionSource, tc.claudeRT)
				if c.TargetPath != tc.claudeTarget {
					t.Errorf("claude target = %s, want %s", c.TargetPath, tc.claudeTarget)
				}
				if c.Explicit != (tc.claudeUA.source != SourceDefault) || c.Blocked() != "" {
					t.Errorf("claude explicit/blocked = %t/%q", c.Explicit, c.Blocked())
				}
			}
			checkLeaf(t, "codex user-agent", snap.Codex.UserAgent, snap.Codex.UserAgentSource, tc.codexUA)
			if snap.Codex.TargetPath != tc.codexTarget {
				t.Errorf("codex target = %s, want %s", snap.Codex.TargetPath, tc.codexTarget)
			}
			if snap.DisableCodexCloaking != tc.cloaking || snap.DisableCodexCloakingSource != tc.cloakingSource {
				t.Errorf("cloaking = %t from %s, want %t from %s", snap.DisableCodexCloaking, snap.DisableCodexCloakingSource, tc.cloaking, tc.cloakingSource)
			}
			if !snap.PluginsEnabled || !snap.InstanceEnabled {
				t.Error("plugin flags not read")
			}
			exportV8AuditFixture(t, tc.name, tc.yaml, legacyTwin(snap, !tc.claudeWantsNoCompare))
		})
	}
}

func TestReadRefusesUnsupportedConfigVersion(t *testing.T) {
	for _, value := range []string{`"8"`, "7", "8.0", "[8]"} {
		p := newPaths(t, "config-version: "+value+"\n"+layoutPlugins)
		if _, err := Read(p.config); !errors.Is(err, ErrUnsupportedConfigVersion) {
			t.Errorf("config-version: %s -> %v, want ErrUnsupportedConfigVersion", value, err)
		}
	}
}

// yamlHasPath reports whether text contains the explicit key path.
func yamlHasPath(t *testing.T, text string, path ...string) bool {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("written config does not parse: %v\n%s", err, text)
	}
	node := documentRootRaw(&doc)
	for _, key := range path {
		next, err := lookupExplicit(node, key)
		if err != nil || next == nil {
			return false
		}
		node = next
	}
	return true
}

var c290 = claudeCandidate("2.1.290", "0.113.0", "v26.4.0")

func TestApplySelectsWriteTargetPerLayout(t *testing.T) {
	cases := []struct {
		name   string
		yaml   string
		target Layout
		// present / absent list key paths (dot-separated) checked on the
		// written file.
		present []string
		absent  []string
		// contains is checked verbatim (indentation, comments).
		contains []string
	}{
		{
			name: "legacy file without blocks creates the legacy block", yaml: fixtureLegacyEmpty, target: LayoutLegacy,
			present: []string{"claude-header-defaults.user-agent"}, absent: []string{"oauth"},
		},
		{
			name: "v8 file without blocks creates the v8 block", yaml: fixtureV8Empty, target: LayoutV8,
			present:  []string{"oauth.providers.claude.header-defaults.runtime-version"},
			absent:   []string{"claude-header-defaults"},
			contains: []string{"oauth:\n  providers:\n    claude:\n      header-defaults:\n        user-agent: \"claude-cli/2.1.290 (external, cli)\"\n"},
		},
		{
			name: "existing v8 block is updated in place", yaml: fixtureV8Only, target: LayoutV8,
			present:  []string{"oauth.providers.codex.header-defaults.user-agent", "oauth.providers.codex.disable-codex-cloaking"},
			absent:   []string{"claude-header-defaults"},
			contains: []string{"      # measured baseline\n      header-defaults:\n", `codex-tui/0.155.0`},
		},
		{
			name: "interim file keeps writing its legacy block", yaml: fixtureInterim, target: LayoutLegacy,
			present: []string{"claude-header-defaults.package-version", "oauth.auth-dir"},
			absent:  []string{"oauth.providers"},
		},
		{
			name: "v8 block wins without config-version", yaml: fixtureV8BlockLegacyFile, target: LayoutV8,
			present: []string{"oauth.providers.claude.header-defaults.package-version"},
			absent:  []string{"claude-header-defaults"},
		},
		{
			// Shadowed legacy leaves are dropped; the legacy-only os stays.
			name: "mixed file writes v8 and drops shadowed legacy leaves", yaml: fixtureMixed, target: LayoutV8,
			present: []string{"oauth.providers.claude.header-defaults.package-version", "claude-header-defaults.os"},
			absent:  []string{"claude-header-defaults.user-agent", "claude-header-defaults.package-version", "claude-header-defaults.runtime-version"},
		},
		{
			name: "empty legacy block beside a v8 block is removed", yaml: strings.Replace(fixtureV8Only, "server:", "claude-header-defaults: {}\nserver:", 1), target: LayoutV8,
			absent: []string{"claude-header-defaults"},
		},
		{
			name: "null legacy block beside a v8 block is removed", yaml: strings.Replace(fixtureV8Only, "server:", "claude-header-defaults:\nserver:", 1), target: LayoutV8,
			absent: []string{"claude-header-defaults"},
		},
		{
			name: "v8 block with a merge key is refused", yaml: fixtureV8Merge, target: LayoutV8,
			// The v8 block itself has a merge key, so it is refused below.
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPaths(t, tc.yaml)
			before, err := Read(p.config)
			if err != nil {
				t.Fatal(err)
			}
			if before.Claude.Target != tc.target {
				t.Fatalf("target = %s, want %s", before.Claude.Target, tc.target)
			}
			infoBefore, _ := os.Stat(p.config)
			_, err = Apply(p.config, p.backup, c290, nil)
			if tc.yaml == fixtureV8Merge {
				if !errors.Is(err, ErrUnsupportedShape) {
					t.Fatalf("merge-key v8 block -> %v, want ErrUnsupportedShape", err)
				}
				if mustReadFile(t, p.config) != tc.yaml {
					t.Fatal("refused write changed the file")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			text := mustReadFile(t, p.config)
			infoAfter, _ := os.Stat(p.config)
			if !os.SameFile(infoBefore, infoAfter) {
				t.Error("write replaced the inode")
			}
			if mustReadFile(t, BackupPath(p.backup)) != tc.yaml {
				t.Error("backup does not hold the previous file")
			}
			after, err := Read(p.config)
			if err != nil {
				t.Fatal(err)
			}
			if !CarriesCandidate(after.Claude, c290) {
				t.Errorf("written file does not carry the candidate: %+v\n%s", after.Claude, text)
			}
			want := Source(tc.target)
			if after.Claude.UserAgentSource != want || after.Claude.PackageVersionSource != want || after.Claude.RuntimeVersionSource != want {
				t.Errorf("sources after write = %s/%s/%s, want %s", after.Claude.UserAgentSource, after.Claude.PackageVersionSource, after.Claude.RuntimeVersionSource, want)
			}
			if !sameValues(before.Codex, after.Codex) || before.DisableCodexCloaking != after.DisableCodexCloaking {
				t.Error("codex values changed")
			}
			for _, path := range tc.present {
				if !yamlHasPath(t, text, strings.Split(path, ".")...) {
					t.Errorf("missing %s:\n%s", path, text)
				}
			}
			for _, path := range tc.absent {
				if yamlHasPath(t, text, strings.Split(path, ".")...) {
					t.Errorf("unexpected %s (a legacy leaf beside its v8 counterpart would be deleted by CPA):\n%s", path, text)
				}
			}
			for _, s := range tc.contains {
				if !strings.Contains(text, s) {
					t.Errorf("missing %q:\n%s", s, text)
				}
			}
			exportV8AuditFixture(t, "write-"+strings.ReplaceAll(tc.name, " ", "-"), text, legacyTwin(after, true))
		})
	}
}

func TestApplyCodexUsesTheSameTargetRules(t *testing.T) {
	ua := "codex-tui/0.160.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.160.1)"
	for _, tc := range []struct {
		yaml, present, absent string
	}{
		{fixtureV8Empty, "oauth.providers.codex.header-defaults.user-agent", "codex-header-defaults"},
		{fixtureV8Only, "oauth.providers.codex.header-defaults.user-agent", "codex-header-defaults"},
		{fixtureInterim, "codex-header-defaults.user-agent", "oauth.providers"},
		{fixtureLegacyEmpty, "codex-header-defaults.user-agent", "oauth"},
	} {
		p := newPaths(t, tc.yaml)
		if _, err := Apply(p.config, p.backup, codexCandidate(t, ua), nil); err != nil {
			t.Fatal(err)
		}
		text := mustReadFile(t, p.config)
		if !yamlHasPath(t, text, strings.Split(tc.present, ".")...) || yamlHasPath(t, text, strings.Split(tc.absent, ".")...) {
			t.Errorf("codex write landed in the wrong place (want %s, not %s):\n%s", tc.present, tc.absent, text)
		}
		snap, err := Read(p.config)
		if err != nil || snap.Codex.UserAgent != ua || snap.DisableCodexCloaking != strings.Contains(tc.yaml, "disable-codex-cloaking: true") {
			t.Errorf("codex after write = %+v cloaking=%t err=%v", snap.Codex, snap.DisableCodexCloaking, err)
		}
	}
}

func TestApplyRefusesUneditableV8Paths(t *testing.T) {
	for name, content := range map[string]string{
		"null v8 parent": fixtureV8NullParent,
		// The v8 block is reachable only through a merge key: writing an
		// explicit key would shadow the shared defaults, and falling back to
		// the legacy block would be discarded by CPA.
		"v8 block only through a merge key": `config-version: 8
x-base: &base
  header-defaults:
    user-agent: "claude-cli/2.1.285 (external, cli)"
oauth:
  providers:
    claude:
      <<: *base
` + layoutPlugins,
		"v8 block is an alias": fixtureV8Aliases,
	} {
		t.Run(name, func(t *testing.T) {
			p := newPaths(t, content)
			_, err := Apply(p.config, p.backup, c290, nil)
			if !errors.Is(err, ErrUnsupportedShape) {
				t.Fatalf("Apply -> %v, want ErrUnsupportedShape", err)
			}
			if mustReadFile(t, p.config) != content {
				t.Error("refused write changed the file")
			}
			if _, err := os.Stat(BackupPath(p.backup)); !errors.Is(err, os.ErrNotExist) {
				t.Error("refused write left a backup")
			}
		})
	}
}

func TestLoopGuardRefusesIneffectiveRender(t *testing.T) {
	t.Run("edit would change another provider", func(t *testing.T) {
		// The codex block aliases the anchored claude block, so editing the
		// claude block in place would also change what CPA loads for codex.
		content := `claude-header-defaults: &hd
  user-agent: "claude-cli/2.1.250 (external, cli)"
codex-header-defaults: *hd
` + layoutPlugins
		p := newPaths(t, content)
		_, err := Apply(p.config, p.backup, c290, nil)
		if !errors.Is(err, ErrNotEffective) {
			t.Fatalf("Apply -> %v, want ErrNotEffective", err)
		}
		if mustReadFile(t, p.config) != content {
			t.Error("guarded write changed the file")
		}
		if _, err := os.Stat(BackupPath(p.backup)); !errors.Is(err, os.ErrNotExist) {
			t.Error("guarded write left a backup")
		}
	})
	t.Run("rendered value would not parse", func(t *testing.T) {
		bad := c290
		bad.UserAgent = "not-a-claude-client/1.0"
		p := newPaths(t, fixtureV8Only)
		if _, err := Apply(p.config, p.backup, bad, nil); !errors.Is(err, ErrNotEffective) {
			t.Fatalf("Apply -> %v, want ErrNotEffective", err)
		}
		if mustReadFile(t, p.config) != fixtureV8Only {
			t.Error("guarded write changed the file")
		}
	})
}

func TestPreviewOnV8FileNeverWrites(t *testing.T) {
	p := newPaths(t, fixtureV8Only)
	checked := false
	snap, err := Preview(p.config, c290, func(s Snapshot) error {
		checked = true
		if s.Claude.TargetPath != "oauth.providers.claude.header-defaults" {
			t.Errorf("check saw target %s", s.Claude.TargetPath)
		}
		return nil
	})
	if err != nil || !checked {
		t.Fatalf("Preview -> %v (checked=%t)", err, checked)
	}
	if snap.Claude.Version.String() != "2.1.285" || snap.Layout != LayoutV8 {
		t.Errorf("preview snapshot = %+v", snap.Claude)
	}
	if mustReadFile(t, p.config) != fixtureV8Only {
		t.Error("preview wrote the file")
	}
	if _, err := os.Stat(p.backup); !errors.Is(err, os.ErrNotExist) {
		t.Error("preview created the backup dir")
	}
	// Preview runs the same refusals as a live write.
	q := newPaths(t, fixtureV8NullParent)
	if _, err := Preview(q.config, c290, nil); !errors.Is(err, ErrUnsupportedShape) {
		t.Errorf("preview of an uneditable file -> %v", err)
	}
	stop := errors.New("stop")
	if _, err := Preview(p.config, c290, func(Snapshot) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("preview ignored the check: %v", err)
	}
}

func TestDryRunToggleOnV8File(t *testing.T) {
	p := newPaths(t, fixtureV8Only)
	snap, changed, err := ApplyDryRun(p.config, p.backup, true, nil)
	if err != nil || !changed {
		t.Fatalf("ApplyDryRun -> changed=%t err=%v", changed, err)
	}
	after, err := Read(p.config)
	if err != nil || !after.DryRun || !after.DryRunPresent {
		t.Fatalf("dry-run not set: %+v %v", after, err)
	}
	if !sameValues(snap.Claude, after.Claude) || after.Claude.UserAgentSource != SourceV8 {
		t.Errorf("toggle changed the baseline: %+v", after.Claude)
	}
	if !strings.Contains(mustReadFile(t, p.config), "    auto-baseline:\n      enabled: true\n      dry-run: true\n") {
		t.Errorf("dry-run not under plugins.configs.auto-baseline:\n%s", mustReadFile(t, p.config))
	}
}

// legacyTwin renders the effective values of snap as a legacy-only file:
// the file CPA must load identically for the plugin's reading to be right.
func legacyTwin(snap Snapshot, includeClaude bool) string {
	var b strings.Builder
	leaf := func(key, value string, source Source) {
		if source != SourceDefault {
			fmt.Fprintf(&b, "  %s: %q\n", key, value)
		}
	}
	if includeClaude {
		b.WriteString("claude-header-defaults:\n")
		leaf("user-agent", snap.Claude.UserAgent, snap.Claude.UserAgentSource)
		leaf("package-version", snap.Claude.PackageVersion, snap.Claude.PackageVersionSource)
		leaf("runtime-version", snap.Claude.RuntimeVersion, snap.Claude.RuntimeVersionSource)
	}
	b.WriteString("codex-header-defaults:\n")
	leaf("user-agent", snap.Codex.UserAgent, snap.Codex.UserAgentSource)
	fmt.Fprintf(&b, "codex:\n  disable-codex-cloaking: %t\n", snap.DisableCodexCloaking)
	return b.String()
}

// exportV8AuditFixture writes a fixture and its legacy twin when
// AUTO_BASELINE_V8AUDIT_DIR is set, so they can be checked against CPA's real
// loader with `v8audit compare <name>.yaml <name>.twin.yaml` (see
// docs/architecture.md section 2.4).
func exportV8AuditFixture(t *testing.T, name, content, twin string) {
	t.Helper()
	dir := os.Getenv("AUTO_BASELINE_V8AUDIT_DIR")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for file, data := range map[string]string{name + ".yaml": content, name + ".twin.yaml": twin} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
