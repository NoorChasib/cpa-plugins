package configfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/fingerprint"
)

const sampleConfig = `# CLIProxyAPI config
host: "0.0.0.0"
port: 8317 # the port
api-keys:
  - "smoke-key"

# Claude fingerprint baseline
claude-header-defaults:
  user-agent: "claude-cli/2.1.220 (external, cli)" # measured
  package-version: "0.94.0"
  runtime-version: "v26.3.0"
  os: "MacOS"
  arch: "arm64"
  timeout: "600"

codex:
  disable-codex-cloaking: true

plugins:
  enabled: true
  dir: "plugins"
  configs:
    auto-baseline:
      enabled: true
`

type paths struct {
	config string
	backup string
}

func newPaths(t *testing.T, content string) paths {
	t.Helper()
	dir := t.TempDir()
	p := paths{config: filepath.Join(dir, "config.yaml"), backup: filepath.Join(dir, "backups")}
	if err := os.WriteFile(p.config, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func mustReadDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func claudeCandidate(version, pkg, rt string) fingerprint.Candidate {
	v := fingerprint.MustParseVersion(version)
	return fingerprint.Candidate{Provider: fingerprint.ProviderClaude, Version: v, UserAgent: fingerprint.CanonicalClaudeUserAgent(v), PackageVersion: pkg, RuntimeVersion: rt, OS: "Linux", Arch: "x64", Entrypoint: "sdk-ts"}
}

func codexCandidate(t *testing.T, ua string) fingerprint.Candidate {
	t.Helper()
	v, ok := fingerprint.ParseCodexUserAgentVersion(ua)
	if !ok {
		t.Fatalf("codex UA %q does not parse", ua)
	}
	return fingerprint.Candidate{Provider: fingerprint.ProviderCodex, Version: v, UserAgent: ua}
}

var c258 = claudeCandidate("2.1.258", "0.112.1", "v26.3.0")

func applyClaude(t *testing.T, p paths) error {
	t.Helper()
	_, err := Apply(p.config, p.backup, c258, nil)
	return err
}

func TestReadEffectiveBaselines(t *testing.T) {
	p := newPaths(t, sampleConfig)
	snap, err := Read(p.config)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if snap.Claude.Version.String() != "2.1.220" || !snap.Claude.Explicit || snap.Claude.PackageVersion != "0.94.0" || snap.Claude.RuntimeVersion != "v26.3.0" {
		t.Errorf("claude = %+v", snap.Claude)
	}
	if snap.Codex.Explicit || snap.Codex.Version != fingerprint.CompiledCodexBaselineVersion || snap.Codex.UserAgent != fingerprint.CompiledCodexUserAgent {
		t.Errorf("codex should be the compiled default: %+v", snap.Codex)
	}
	if !snap.DisableCodexCloaking {
		t.Errorf("disable-codex-cloaking not detected")
	}
	if snap.SHA256 == "" || snap.Path != p.config {
		t.Errorf("snapshot meta = %+v", snap)
	}
}

func TestReadDefaultsWhenBlocksAbsentOrBlank(t *testing.T) {
	for name, content := range map[string]string{
		"no blocks":    "port: 1\n",
		"blank values": "claude-header-defaults:\n  user-agent: \"  \"\n  package-version: \"\"\n",
		"empty file":   "",
		"leading ---":  "---\nport: 1\n",
	} {
		t.Run(name, func(t *testing.T) {
			snap, err := Read(newPaths(t, content).config)
			if err != nil {
				t.Fatal(err)
			}
			if snap.Claude.Explicit || snap.Claude.Version != fingerprint.CompiledClaudeBaselineVersion || snap.Claude.PackageVersion != fingerprint.CompiledClaudePackageVersion {
				t.Errorf("claude = %+v", snap.Claude)
			}
			if snap.DisableCodexCloaking {
				t.Errorf("cloaking flag defaulted true")
			}
		})
	}
}

func TestReadMalformedExplicitUserAgent(t *testing.T) {
	snap, err := Read(newPaths(t, "claude-header-defaults:\n  user-agent: \"my-proxy/1.0\"\ncodex-header-defaults:\n  user-agent: \"codex_cli_rs/0.114.0\"\n").config)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Claude.Explicit || !snap.Claude.Malformed || snap.Claude.Version != fingerprint.CompiledClaudeBaselineVersion {
		t.Errorf("malformed claude UA: %+v", snap.Claude)
	}
	if !snap.Codex.Explicit || snap.Codex.Malformed || snap.Codex.Version.String() != "0.114.0" {
		t.Errorf("codex placeholder: %+v", snap.Codex)
	}
}

func TestReadDuplicateKeysRefused(t *testing.T) {
	// Top-level duplicate: CPA's decoder rejects the whole file, so Read
	// refuses outright.
	content := "claude-header-defaults:\n  user-agent: \"claude-cli/2.1.100 (external, cli)\"\nclaude-header-defaults:\n  user-agent: \"claude-cli/2.1.230 (external, cli)\"\n"
	if _, err := Read(newPaths(t, content).config); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("top-level duplicate: err = %v", err)
	}
	// Duplicate inside one provider block: that provider is marked
	// unsupported, the other keeps working.
	content = "claude-header-defaults:\n  package-version: \"0.90.0\"\n  package-version: \"0.95.0\"\ncodex-header-defaults:\n  user-agent: \"codex-tui/0.150.0 (x)\"\n"
	snap, err := Read(newPaths(t, content).config)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Claude.Unsupported != "duplicate_key" || snap.Claude.Blocked() != "duplicate_key" {
		t.Errorf("claude = %+v", snap.Claude)
	}
	if snap.Codex.Unsupported != "" || snap.Codex.Version.String() != "0.150.0" {
		t.Errorf("codex should be unaffected: %+v", snap.Codex)
	}
	// Duplicate inside a merged mapping is detected too.
	content = "d: &d\n  user-agent: \"claude-cli/2.1.230 (external, cli)\"\n  user-agent: \"claude-cli/2.1.231 (external, cli)\"\nclaude-header-defaults:\n  <<: *d\n"
	snap, err = Read(newPaths(t, content).config)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Claude.Unsupported != "duplicate_key" {
		t.Errorf("merged duplicate not detected: %+v", snap.Claude)
	}
}

func TestReadCyclesDoNotCrash(t *testing.T) {
	// yaml.v3 only resolves backward alias references, so every cycle below
	// closes through an anchor declared earlier in the same document.
	cases := map[string]string{
		"self merge":           "x: &x\n  <<: *x\nclaude-header-defaults:\n  <<: *x\n",
		"mutual merge":         "a: &a\n  b: &b\n    <<: *a\n  <<: *b\nclaude-header-defaults:\n  <<: *a\n",
		"alias into cycle":     "a: &a\n  b: &b\n    <<: *a\n  <<: *b\nclaude-header-defaults: *b\n",
		"sequence merge cycle": "a: &a\n  b: &b\n    <<: [*a]\n  <<: [*b, *a]\nclaude-header-defaults:\n  <<: [*a]\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			snap, err := Read(newPaths(t, content).config)
			if err != nil {
				t.Fatalf("Read returned an error instead of marking the block: %v", err)
			}
			if snap.Claude.Unsupported != "unsupported_config_shape" || snap.Claude.Blocked() == "" {
				t.Errorf("cycle not surfaced: %+v", snap.Claude)
			}
			if snap.Codex.Unsupported != "" {
				t.Errorf("codex affected by claude cycle: %+v", snap.Codex)
			}
			p := newPaths(t, content)
			// Apply refuses either because the block is an alias/merge shape
			// (checked first) or because the cycle is detected; both are
			// ErrUnsupportedShape-class refusals and neither touches the file.
			if err := applyClaude(t, p); !errors.Is(err, ErrCycle) && !errors.Is(err, ErrUnsupportedShape) {
				t.Errorf("Apply err = %v, want a shape/cycle refusal", err)
			}
			if mustReadFile(t, p.config) != content {
				t.Error("file modified despite cycle")
			}
			if _, err := os.Stat(BackupPath(p.backup)); !errors.Is(err, os.ErrNotExist) {
				t.Error("backup written despite cycle")
			}
		})
	}
	// A cycle reachable from the top-level lookup path (plugins block) is
	// refused by Read as a whole rather than attributed to a provider.
	top := "x: &x\n  <<: *x\nplugins:\n  <<: *x\n"
	if _, err := Read(newPaths(t, top).config); !errors.Is(err, ErrCycle) {
		t.Errorf("top-level cycle: err = %v, want ErrCycle", err)
	}
}

func TestMergeKeyRecognizedByTagOnly(t *testing.T) {
	// A quoted "<<" is an ordinary key: no merge happens and the plugin can
	// edit the block. Escaped so the YAML parser sees the quoted form.
	content := "d: &d\n  user-agent: \"claude-cli/2.1.240 (external, cli)\"\nclaude-header-defaults:\n  \"<<\": *d\n  package-version: \"0.1.0\"\n"
	snap, err := Read(newPaths(t, content).config)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Claude.Explicit || snap.Claude.PackageVersion != "0.1.0" {
		t.Errorf("quoted <<: treated as merge: %+v", snap.Claude)
	}
	p := newPaths(t, content)
	if err := applyClaude(t, p); err != nil {
		t.Fatalf("Apply refused an ordinary quoted key: %v", err)
	}
	text := mustReadFile(t, p.config)
	if !strings.Contains(text, `"<<": *d`) || !strings.Contains(text, "2.1.258") {
		t.Errorf("quoted key not preserved or edit missing:\n%s", text)
	}
	// The unquoted form IS a merge and is refused for writing.
	content = strings.Replace(content, "\"<<\": *d", "<<: *d", 1)
	if err := applyClaude(t, newPaths(t, content)); !errors.Is(err, ErrUnsupportedShape) {
		t.Errorf("unquoted merge accepted for write: %v", err)
	}
}

func TestReadPluginEnabledFlags(t *testing.T) {
	snap, err := Read(newPaths(t, "plugins:\n  enabled: true\n  configs:\n    auto-baseline:\n      enabled: true\n").config)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.PluginsEnabled || !snap.InstanceEnabled {
		t.Errorf("flags = %t/%t", snap.PluginsEnabled, snap.InstanceEnabled)
	}
	for name, content := range map[string]string{
		"plugins disabled":  "plugins:\n  enabled: false\n  configs:\n    auto-baseline:\n      enabled: true\n",
		"instance disabled": "plugins:\n  enabled: true\n  configs:\n    auto-baseline:\n      enabled: false\n",
		"instance missing":  "plugins:\n  enabled: true\n",
		"no plugins block":  "port: 1\n",
	} {
		snap, err := Read(newPaths(t, content).config)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if snap.PluginsEnabled && snap.InstanceEnabled {
			t.Errorf("%s: both flags true", name)
		}
	}
}

func TestReadResolvesAliasesAndMergeKeys(t *testing.T) {
	content := `defaults: &defaults
  user-agent: "claude-cli/2.1.240 (external, cli)"
  package-version: "0.100.0"
  runtime-version: "v26.3.0"
claude-header-defaults:
  <<: *defaults
  package-version: "0.101.0"
codex-header-defaults: *codex
codex-defaults: &codex
  user-agent: "codex-tui/0.150.0 (x)"
`
	// yaml.v3 requires anchors before aliases; reorder codex-defaults first.
	content = strings.Replace(content, "codex-header-defaults: *codex\ncodex-defaults: &codex\n  user-agent: \"codex-tui/0.150.0 (x)\"\n", "", 1)
	content = "codex-defaults: &codex\n  user-agent: \"codex-tui/0.150.0 (x)\"\ncodex-header-defaults: *codex\n" + content
	snap, err := Read(newPaths(t, content).config)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Claude.Version.String() != "2.1.240" || snap.Claude.PackageVersion != "0.101.0" || snap.Claude.RuntimeVersion != "v26.3.0" {
		t.Errorf("merge key not resolved: %+v", snap.Claude)
	}
	if snap.Codex.Version.String() != "0.150.0" || !snap.Codex.Explicit {
		t.Errorf("alias not resolved: %+v", snap.Codex)
	}
}

func TestReadErrors(t *testing.T) {
	if _, err := Read(filepath.Join(t.TempDir(), "missing.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	if _, err := Read(t.TempDir()); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Errorf("directory: %v", err)
	}
	if _, err := Read(newPaths(t, "- just\n- a list\n").config); err == nil || !strings.Contains(err.Error(), "mapping") {
		t.Errorf("sequence root: %v", err)
	}
	if _, err := Read(newPaths(t, "a: [\n").config); err == nil {
		t.Errorf("broken yaml accepted")
	}
	if _, err := Read(newPaths(t, "port: 1\n---\nport: 2\n").config); !errors.Is(err, ErrMultiDocument) {
		t.Errorf("multi-document: %v", err)
	}
}

func TestApplyClaudePreservesEverythingElse(t *testing.T) {
	p := newPaths(t, sampleConfig)
	checked := false
	snap, err := Apply(p.config, p.backup, c258, func(s Snapshot) error {
		checked = true
		if s.Claude.Version.String() != "2.1.220" {
			t.Errorf("check saw %+v", s.Claude)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !checked || snap.Claude.Version.String() != "2.1.220" {
		t.Errorf("returned snapshot is not the pre-edit state: %+v", snap.Claude)
	}
	text := mustReadFile(t, p.config)
	for _, want := range []string{
		"# CLIProxyAPI config",
		"port: 8317 # the port",
		"# Claude fingerprint baseline",
		`user-agent: "claude-cli/2.1.258 (external, cli)" # measured`,
		`package-version: "0.112.1"`,
		`runtime-version: "v26.3.0"`,
		`os: "MacOS"`,
		`arch: "arm64"`,
		`timeout: "600"`,
		"disable-codex-cloaking: true",
		"auto-baseline:",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "2.1.220") {
		t.Errorf("old version still present:\n%s", text)
	}
	if strings.Index(text, "host:") > strings.Index(text, "claude-header-defaults:") || strings.Index(text, "claude-header-defaults:") > strings.Index(text, "plugins:") {
		t.Errorf("key order changed:\n%s", text)
	}
	// Backup holds the previous bytes, in the backup dir, mode 0600.
	if got := mustReadFile(t, BackupPath(p.backup)); got != sampleConfig {
		t.Errorf("backup = %q", got)
	}
	info, err := os.Stat(BackupPath(p.backup))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("backup mode = %o", info.Mode().Perm())
	}
	var m map[string]any
	if err := yaml.Unmarshal([]byte(text), &m); err != nil {
		t.Fatal(err)
	}
	block, ok := m["claude-header-defaults"].(map[string]any)
	if !ok || block["package-version"] != "0.112.1" {
		t.Errorf("claude-header-defaults = %#v", m["claude-header-defaults"])
	}
	// No temp files next to the config.
	if names := mustReadDir(t, filepath.Dir(p.config)); len(names) != 2 {
		t.Errorf("unexpected files: %v", names)
	}
}

func TestApplyCreatesMissingBlocksAndKeys(t *testing.T) {
	p := newPaths(t, "port: 8317\napi-keys: [\"k\"]\n")
	if err := applyClaude(t, p); err != nil {
		t.Fatal(err)
	}
	codexUA := "codex-tui/0.152.1 (Ubuntu 24.4.0; x86_64) WezTerm/1.0 (codex-tui; 0.152.1)"
	if _, err := Apply(p.config, p.backup, codexCandidate(t, codexUA), nil); err != nil {
		t.Fatal(err)
	}
	text := mustReadFile(t, p.config)
	want := "port: 8317\napi-keys: [\"k\"]\nclaude-header-defaults:\n  user-agent: \"claude-cli/2.1.258 (external, cli)\"\n  package-version: \"0.112.1\"\n  runtime-version: \"v26.3.0\"\ncodex-header-defaults:\n  user-agent: \"" + codexUA + "\"\n"
	if text != want {
		t.Errorf("got:\n%s\nwant:\n%s", text, want)
	}
	snap, err := Read(p.config)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Claude.Version.String() != "2.1.258" || snap.Codex.Version.String() != "0.152.1" {
		t.Errorf("re-read = %+v / %+v", snap.Claude, snap.Codex)
	}
}

func TestApplyShapes(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr error
		check   func(t *testing.T, text string)
	}{
		{"null placeholder block", "claude-header-defaults:\nport: 1\n", nil, func(t *testing.T, text string) {
			if !strings.HasPrefix(text, "claude-header-defaults:\n  user-agent:") || !strings.Contains(text, "port: 1") {
				t.Errorf("placeholder not filled in place:\n%s", text)
			}
		}},
		{"tilde placeholder", "claude-header-defaults: ~\n", nil, func(t *testing.T, text string) {
			if !strings.Contains(text, "  user-agent:") {
				t.Errorf("tilde not converted:\n%s", text)
			}
		}},
		{"empty file", "", nil, func(t *testing.T, text string) {
			if !strings.HasPrefix(text, "claude-header-defaults:\n  user-agent:") {
				t.Errorf("empty file result:\n%s", text)
			}
		}},
		{"commented-out block", "# claude-header-defaults:\n#   user-agent: \"claude-cli/9.9.9 (external, cli)\"\nport: 1\n", nil, func(t *testing.T, text string) {
			if !strings.Contains(text, "# claude-header-defaults:") || !strings.Contains(text, "9.9.9") {
				t.Errorf("comment lost:\n%s", text)
			}
			if strings.Count(text, "claude-header-defaults:") != 2 {
				t.Errorf("real block not appended:\n%s", text)
			}
		}},
		{"leading document marker", "---\nport: 1\n", nil, func(t *testing.T, text string) {
			if !strings.Contains(text, "port: 1") || !strings.Contains(text, "2.1.258") {
				t.Errorf("content not preserved:\n%s", text)
			}
		}},
		{"quoted values with special chars", "api-keys:\n  - \"a: b # not a comment\"\nname: 'it''s'\nclaude-header-defaults:\n  os: \"Mac: OS\"\n", nil, func(t *testing.T, text string) {
			for _, want := range []string{`"a: b # not a comment"`, "it''s", `"Mac: OS"`, "2.1.258"} {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q:\n%s", want, text)
				}
			}
		}},
		{"CRLF line endings", "port: 1\r\nclaude-header-defaults:\r\n  os: \"MacOS\"\r\n", nil, func(t *testing.T, text string) {
			var m map[string]any
			if err := yaml.Unmarshal([]byte(text), &m); err != nil {
				t.Fatalf("output not YAML: %v", err)
			}
			if m["port"] != 1 || !strings.Contains(text, "2.1.258") || !strings.Contains(text, "MacOS") {
				t.Errorf("CRLF content lost:\n%q", text)
			}
		}},
		{"duplicate top-level keys refused", "claude-header-defaults:\n  user-agent: \"claude-cli/2.1.100 (external, cli)\"\nclaude-header-defaults:\n  user-agent: \"claude-cli/2.1.230 (external, cli)\"\n", ErrDuplicateKey, nil},
		{"duplicate inner keys refused", "claude-header-defaults:\n  package-version: \"0.1.0\"\n  package-version: \"0.2.0\"\n", ErrDuplicateKey, nil},
		{"duplicate unrelated top-level key refused", "port: 1\nport: 2\n", ErrDuplicateKey, nil},
		{"scalar placeholder refused", "claude-header-defaults: \"nope\"\n", ErrUnsupportedShape, nil},
		{"sequence refused", "claude-header-defaults:\n  - a\n", ErrUnsupportedShape, nil},
		{"alias block refused", "d: &d\n  user-agent: \"claude-cli/2.1.230 (external, cli)\"\nclaude-header-defaults: *d\n", ErrUnsupportedShape, nil},
		{"merge key block refused", "d: &d\n  user-agent: \"claude-cli/2.1.230 (external, cli)\"\nclaude-header-defaults:\n  <<: *d\n", ErrUnsupportedShape, nil},
		{"alias value refused", "ua: &ua \"claude-cli/2.1.230 (external, cli)\"\nclaude-header-defaults:\n  user-agent: *ua\n", ErrUnsupportedShape, nil},
		{"block only via top-level merge refused", "d: &d\n  claude-header-defaults:\n    user-agent: \"claude-cli/2.1.230 (external, cli)\"\n<<: *d\n", ErrUnsupportedShape, nil},
		{"multi-document refused", "port: 1\n---\nport: 2\n", ErrMultiDocument, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPaths(t, tc.content)
			err := applyClaude(t, p)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if got := mustReadFile(t, p.config); got != tc.content {
					t.Errorf("file modified on refusal:\n%s", got)
				}
				if _, err := os.Stat(BackupPath(p.backup)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("backup written on refusal")
				}
				return
			}
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			text := mustReadFile(t, p.config)
			if _, err := Read(p.config); err != nil {
				t.Fatalf("output unreadable: %v\n%s", err, text)
			}
			tc.check(t, text)
		})
	}
}

func TestApplyPreservesMissingTrailingNewline(t *testing.T) {
	p := newPaths(t, "port: 1")
	if _, err := Apply(p.config, p.backup, codexCandidate(t, "codex-tui/0.150.0 (x)"), nil); err != nil {
		t.Fatal(err)
	}
	if text := mustReadFile(t, p.config); strings.HasSuffix(text, "\n") {
		t.Errorf("trailing newline added:\n%q", text)
	}
}

func TestApplyCheckFailureLeavesFileUntouched(t *testing.T) {
	p := newPaths(t, sampleConfig)
	sentinel := errors.New("not newer")
	_, err := Apply(p.config, p.backup, c258, func(Snapshot) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v", err)
	}
	if mustReadFile(t, p.config) != sampleConfig {
		t.Error("file modified despite check failure")
	}
	if _, err := os.Stat(BackupPath(p.backup)); !errors.Is(err, os.ErrNotExist) {
		t.Error("backup written despite check failure")
	}
}

func TestApplyNoopWhenAlreadyEqual(t *testing.T) {
	content := "claude-header-defaults:\n  user-agent: \"claude-cli/2.1.258 (external, cli)\"\n  package-version: \"0.112.1\"\n  runtime-version: \"v26.3.0\"\n"
	p := newPaths(t, content)
	if err := applyClaude(t, p); err != nil {
		t.Fatal(err)
	}
	if mustReadFile(t, p.config) != content {
		t.Error("file rewritten although content was identical")
	}
	if _, err := os.Stat(BackupPath(p.backup)); !errors.Is(err, os.ErrNotExist) {
		t.Error("backup written for a no-op")
	}
}

func TestApplyDetectsConcurrentChange(t *testing.T) {
	p := newPaths(t, sampleConfig)
	changed := sampleConfig + "extra: 1\n"
	_, err := Apply(p.config, p.backup, c258, func(Snapshot) error {
		// Simulate another writer between read and write.
		return os.WriteFile(p.config, []byte(changed), 0o644)
	})
	if !errors.Is(err, ErrChanged) {
		t.Fatalf("err = %v, want ErrChanged", err)
	}
	if mustReadFile(t, p.config) != changed {
		t.Error("concurrent write was clobbered")
	}
}

func TestApplyRejectsUnsupportedProvider(t *testing.T) {
	p := newPaths(t, sampleConfig)
	if _, err := Apply(p.config, p.backup, fingerprint.Candidate{Provider: "gemini"}, nil); err == nil {
		t.Fatal("unsupported provider accepted")
	}
}

func TestApplyBackupDirUnwritable(t *testing.T) {
	p := newPaths(t, sampleConfig)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Apply(p.config, filepath.Join(file, "sub"), c258, nil)
	if err == nil || !strings.Contains(err.Error(), "backup") {
		t.Fatalf("err = %v", err)
	}
	if mustReadFile(t, p.config) != sampleConfig {
		t.Error("config written although backup failed")
	}
}

func TestWriteInPlaceRestoresOnFailure(t *testing.T) {
	p := newPaths(t, sampleConfig)
	// A directory cannot be opened O_RDWR: error before any write.
	if err := writeInPlace(t.TempDir(), []byte("x"), []byte(sampleConfig)); err == nil {
		t.Fatal("directory accepted")
	}
	// Successful path keeps the same inode and exact bytes, shrinking the file.
	before, err := os.Stat(p.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeInPlace(p.config, []byte("port: 1\n"), []byte(sampleConfig)); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(p.config)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("inode changed")
	}
	if got := mustReadFile(t, p.config); got != "port: 1\n" {
		t.Errorf("content = %q", got)
	}
}

// faultyFile wraps a real descriptor and fails a chosen operation once.
type faultyFile struct {
	configFile
	failClose bool
	failSync  bool
}

func (f *faultyFile) Close() error {
	err := f.configFile.Close()
	if f.failClose {
		return errors.New("injected close failure")
	}
	return err
}

func (f *faultyFile) Sync() error {
	if f.failSync {
		return errors.New("injected sync failure")
	}
	return f.configFile.Sync()
}

func TestWriteInPlaceRestoresOnCloseAndSyncFailure(t *testing.T) {
	prev := openConfigFile
	defer func() { openConfigFile = prev }()
	for _, tc := range []struct {
		name string
		fail func(*faultyFile)
		want string
	}{
		{"close", func(f *faultyFile) { f.failClose = true }, "close config"},
		{"sync", func(f *faultyFile) { f.failSync = true }, "sync config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPaths(t, sampleConfig)
			calls := 0
			openConfigFile = func(path string) (configFile, error) {
				f, err := os.OpenFile(path, os.O_RDWR, 0o644)
				if err != nil {
					return nil, err
				}
				calls++
				ff := &faultyFile{configFile: f}
				if calls == 1 {
					tc.fail(ff) // only the first descriptor fails; the restore descriptor works
				}
				return ff, nil
			}
			err := writeInPlace(p.config, []byte("port: 1\n"), []byte(sampleConfig))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %s failure", err, tc.want)
			}
			if got := mustReadFile(t, p.config); got != sampleConfig {
				t.Errorf("previous content not restored after %s failure:\n%s", tc.name, got)
			}
		})
	}
}

func TestWriteInPlaceVerifiesBytes(t *testing.T) {
	prev := openConfigFile
	defer func() { openConfigFile = prev }()
	p := newPaths(t, sampleConfig)
	calls := 0
	openConfigFile = func(path string) (configFile, error) {
		f, err := os.OpenFile(path, os.O_RDWR, 0o644)
		if err != nil {
			return nil, err
		}
		calls++
		if calls == 1 {
			return &clobberOnClose{configFile: f, path: path}, nil
		}
		return f, nil
	}
	err := writeInPlace(p.config, []byte("port: 1\n"), []byte(sampleConfig))
	if err == nil || !strings.Contains(err.Error(), "do not match") {
		t.Fatalf("err = %v, want verification failure", err)
	}
	if got := mustReadFile(t, p.config); got != sampleConfig {
		t.Errorf("not restored after verification failure:\n%s", got)
	}
}

// clobberOnClose simulates another writer landing between our close and
// our verification read.
type clobberOnClose struct {
	configFile
	path string
}

func (c *clobberOnClose) Close() error {
	err := c.configFile.Close()
	_ = os.WriteFile(c.path, []byte("someone: else\n"), 0o644)
	return err
}

func TestRenderQuotesVersionLikeScalars(t *testing.T) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("claude-header-defaults:\n  package-version: 0.94\n"), &doc); err != nil {
		t.Fatal(err)
	}
	out, err := render(&doc, nil, c258)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `package-version: "0.112.1"`) {
		t.Errorf("version scalar not quoted:\n%s", out)
	}
}

func TestConfigFlagFromCmdline(t *testing.T) {
	cases := map[string]string{
		"./CLIProxyAPI\x00": "",
		"./CLIProxyAPI\x00-config\x00/etc/cpa/config.yaml\x00": "/etc/cpa/config.yaml",
		"./CLIProxyAPI\x00--config\x00/x.yaml\x00":             "/x.yaml",
		"./CLIProxyAPI\x00-config=/y.yaml\x00":                 "/y.yaml",
		"./CLIProxyAPI\x00--config=/z.yaml\x00-tui\x00":        "/z.yaml",
		"./CLIProxyAPI\x00-config\x00":                         "",
		"./CLIProxyAPI\x00---config\x00/no.yaml\x00":           "",
		"./CLIProxyAPI\x00-configx\x00/no.yaml\x00":            "",
		"": "",
	}
	for raw, want := range cases {
		if got := configFlagFromCmdline([]byte(raw)); got != want {
			t.Errorf("%q -> %q, want %q", raw, got, want)
		}
	}
}

func TestDetectDeploymentMode(t *testing.T) {
	env := func(vars map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
	}
	none := env(nil)
	if m := detectDeploymentMode([]string{"./CLIProxyAPI", "-config", "x.yaml"}, none); m.Name != "" {
		t.Errorf("plain file detected as %+v", m)
	}
	if m := detectDeploymentMode([]string{"./CLIProxyAPI", "-home-jwt", "eyJ"}, none); m.Name != "home" {
		t.Errorf("home flag: %+v", m)
	}
	if m := detectDeploymentMode([]string{"./CLIProxyAPI", "--home-jwt=eyJ"}, none); m.Name != "home" {
		t.Errorf("home flag=: %+v", m)
	}
	if m := detectDeploymentMode(nil, env(map[string]string{"home_jwt": "x"})); m.Name != "home" || !strings.Contains(m.Reason, "home_jwt") {
		t.Errorf("home env: %+v", m)
	}
	if m := detectDeploymentMode(nil, env(map[string]string{"PGSTORE_DSN": "postgres://x"})); m.Name != "postgres" {
		t.Errorf("postgres: %+v", m)
	}
	if m := detectDeploymentMode(nil, env(map[string]string{"OBJECTSTORE_ENDPOINT": "s3.example"})); m.Name != "object-store" {
		t.Errorf("object store: %+v", m)
	}
	if m := detectDeploymentMode(nil, env(map[string]string{"GITSTORE_GIT_URL": "https://git"})); m.Name != "git-store" {
		t.Errorf("git store: %+v", m)
	}
	if m := detectDeploymentMode(nil, env(map[string]string{"PGSTORE_DSN": "  "})); m.Name != "" {
		t.Errorf("blank env detected: %+v", m)
	}
}

func TestResolvePath(t *testing.T) {
	if p, src := ResolvePath("  /explicit.yaml "); p != "/explicit.yaml" || src != "config-path" {
		t.Errorf("override -> %q %q", p, src)
	}
	p, src := ResolvePath("")
	if !filepath.IsAbs(p) && p != "config.yaml" {
		t.Errorf("default path = %q", p)
	}
	if src != "cwd default" && src != "process -config flag" {
		t.Errorf("source = %q", src)
	}
}

func TestProbe(t *testing.T) {
	p := newPaths(t, sampleConfig)
	exists, writable, err := Probe(p.config)
	if err != nil || !exists || !writable {
		t.Errorf("writable file: %v %v %v", exists, writable, err)
	}
	exists, writable, err = Probe(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil || exists || writable {
		t.Errorf("missing file: %v %v %v", exists, writable, err)
	}
	exists, writable, err = Probe(t.TempDir())
	if err == nil || !exists || writable {
		t.Errorf("directory: %v %v %v", exists, writable, err)
	}
	t.Run("read-only file", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root bypasses file permissions")
		}
		if err := os.Chmod(p.config, 0o444); err != nil {
			t.Fatal(err)
		}
		exists, writable, err = Probe(p.config)
		if err != nil || !exists || writable {
			t.Errorf("read-only file: %v %v %v", exists, writable, err)
		}
	})
}

func TestProbeDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new", "backups")
	if ok, err := ProbeDir(dir); err != nil || !ok {
		t.Errorf("creatable dir: %v %v", ok, err)
	}
	if names := mustReadDir(t, dir); len(names) != 0 {
		t.Errorf("probe left files: %v", names)
	}
	if ok, err := ProbeDir(""); err == nil || ok {
		t.Errorf("empty dir accepted")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := ProbeDir(filepath.Join(file, "sub")); err == nil || ok {
		t.Errorf("dir under a file accepted: %v %v", ok, err)
	}
}
