package config

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/auto-baseline/internal/fingerprint"
	"github.com/NoorChasib/cpa-plugins/plugins/auto-baseline/internal/learner"
)

func TestParseEmptyYieldsDefaultsDisabled(t *testing.T) {
	for _, raw := range []string{"", "   \n"} {
		cfg, err := Parse([]byte(raw))
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		if cfg.Enabled {
			t.Errorf("empty config must not be enabled")
		}
		assertDefaults(t, cfg)
	}
}

func TestParseHostDefaultSubtree(t *testing.T) {
	cfg, err := Parse([]byte("enabled: false\npriority: 0\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Enabled {
		t.Errorf("enabled = true, want false")
	}
	assertDefaults(t, cfg)
}

func assertDefaults(t *testing.T, cfg Config) {
	t.Helper()
	if cfg.DryRun {
		t.Errorf("DryRun = true")
	}
	if cfg.ConfigPath != "" {
		t.Errorf("ConfigPath = %q", cfg.ConfigPath)
	}
	if cfg.StateDir != "plugins/auto-baseline" {
		t.Errorf("StateDir = %q", cfg.StateDir)
	}
	if !cfg.ManageClaude || !cfg.ManageCodex {
		t.Errorf("manage = %t/%t", cfg.ManageClaude, cfg.ManageCodex)
	}
	if strings.Join(cfg.ClaudeEntrypoints, ",") != "cli,sdk-cli,claude-vscode,sdk-ts,sdk-py" {
		t.Errorf("entrypoints = %v", cfg.ClaudeEntrypoints)
	}
	if !cfg.RequireClaudeCodeBeta {
		t.Errorf("RequireClaudeCodeBeta = false")
	}
	if cfg.MinObservations != 3 || cfg.MinDistinctSessions != 1 {
		t.Errorf("quorum = %d/%d", cfg.MinObservations, cfg.MinDistinctSessions)
	}
	if cfg.BackupDir != "" || cfg.EffectiveBackupDir() != cfg.StateDir {
		t.Errorf("backup dir = %q effective %q", cfg.BackupDir, cfg.EffectiveBackupDir())
	}
	if cfg.ObservationWindow != 24*time.Hour || cfg.PromotionCooldown != 60*time.Second {
		t.Errorf("window/cooldown = %s/%s", cfg.ObservationWindow, cfg.PromotionCooldown)
	}
	if cfg.ClaudeMinVersion != fingerprint.CompiledClaudeBaselineVersion || cfg.CodexMinVersion != fingerprint.CompiledCodexBaselineVersion {
		t.Errorf("floors = %s/%s", cfg.ClaudeMinVersion, cfg.CodexMinVersion)
	}
	if cfg.DisplayTimezone != "UTC" {
		t.Errorf("DisplayTimezone = %q", cfg.DisplayTimezone)
	}
	if cfg.RequireExplicitBaseline {
		t.Errorf("RequireExplicitBaseline defaulted true")
	}
}

func TestWriteFingerprintTracksWriteFields(t *testing.T) {
	base, err := Parse([]byte("enabled: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	same, err := Parse([]byte("enabled: true\ndisplay-timezone: UTC\npromotion-cooldown: 9s\nmin-observations: 9\n"))
	if err != nil {
		t.Fatal(err)
	}
	if base.WriteFingerprint() != same.WriteFingerprint() {
		t.Error("non-write fields changed the write fingerprint")
	}
	if off, err := Parse([]byte("enabled: false\n")); err != nil || off.WriteFingerprint() == base.WriteFingerprint() {
		t.Errorf("enabled did not change the write fingerprint (err=%v)", err)
	}
	for _, raw := range []string{"dry-run: true\n", "config-path: /x\n", "backup-dir: /b\n", "state-dir: /s\n", "manage-codex: false\n", "claude-min-version: 2.1.250\n", "require-explicit-baseline: true\n"} {
		cfg, err := Parse([]byte("enabled: true\n" + raw))
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if cfg.WriteFingerprint() == base.WriteFingerprint() {
			t.Errorf("%q did not change the write fingerprint", raw)
		}
	}
}

func TestParseFullConfig(t *testing.T) {
	cfg, err := Parse([]byte(`
enabled: true
priority: 10
dry-run: true
config-path: /CLIProxyAPI/config.yaml
state-dir: /CLIProxyAPI/plugins/state
backup-dir: /CLIProxyAPI/backups
store:
  version: 0.1.0
  release-tag: v0.1.0
manage-claude: true
manage-codex: false
claude-entrypoints: [CLI, " sdk-ts "]
require-claude-code-beta: false
min-observations: 5
min-distinct-sessions: 3
observation-window: 12h
promotion-cooldown: 0s
claude-min-version: 2.1.250
codex-min-version: v0.150.0
require-explicit-baseline: true
display-timezone: Europe/Berlin
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !cfg.Enabled || !cfg.DryRun {
		t.Errorf("enabled/dry-run = %t/%t", cfg.Enabled, cfg.DryRun)
	}
	if cfg.ConfigPath != "/CLIProxyAPI/config.yaml" || cfg.StateDir != "/CLIProxyAPI/plugins/state" || cfg.EffectiveBackupDir() != "/CLIProxyAPI/backups" {
		t.Errorf("paths = %q/%q/%q", cfg.ConfigPath, cfg.StateDir, cfg.BackupDir)
	}
	if !cfg.ManageClaude || cfg.ManageCodex {
		t.Errorf("manage = %t/%t", cfg.ManageClaude, cfg.ManageCodex)
	}
	if !cfg.Manages(fingerprint.ProviderClaude) || cfg.Manages(fingerprint.ProviderCodex) || cfg.Manages("other") {
		t.Errorf("Manages mismatch")
	}
	if strings.Join(cfg.ClaudeEntrypoints, ",") != "cli,sdk-ts" {
		t.Errorf("entrypoints = %v", cfg.ClaudeEntrypoints)
	}
	if cfg.RequireClaudeCodeBeta {
		t.Errorf("RequireClaudeCodeBeta = true")
	}
	if cfg.MinObservations != 5 || cfg.MinDistinctSessions != 3 {
		t.Errorf("quorum = %d/%d", cfg.MinObservations, cfg.MinDistinctSessions)
	}
	if cfg.ObservationWindow != 12*time.Hour || cfg.PromotionCooldown != 0 {
		t.Errorf("window/cooldown = %s/%s", cfg.ObservationWindow, cfg.PromotionCooldown)
	}
	if cfg.ClaudeMinVersion.String() != "2.1.250" || cfg.CodexMinVersion.String() != "0.150.0" {
		t.Errorf("floors = %s/%s", cfg.ClaudeMinVersion, cfg.CodexMinVersion)
	}
	if cfg.MinVersion(fingerprint.ProviderClaude).String() != "2.1.250" || !cfg.MinVersion("x").IsZero() {
		t.Errorf("MinVersion mismatch")
	}
	if !cfg.RequireExplicitBaseline {
		t.Errorf("RequireExplicitBaseline = false")
	}
	if cfg.DisplayTimezone != "Europe/Berlin" || len(cfg.Warnings) != 0 {
		t.Errorf("tz = %q warnings = %v", cfg.DisplayTimezone, cfg.Warnings)
	}
	rules := cfg.Rules()
	if rules.RequireClaudeCodeBeta || len(rules.ClaudeEntrypoints) != 2 {
		t.Errorf("rules = %+v", rules)
	}
}

func TestParseDurationsAcceptIntegersRejectFloats(t *testing.T) {
	cfg, err := Parse([]byte("observation-window: 3600\npromotion-cooldown: 30\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.ObservationWindow != time.Hour || cfg.PromotionCooldown != 30*time.Second {
		t.Errorf("durations = %s/%s", cfg.ObservationWindow, cfg.PromotionCooldown)
	}
	for _, bad := range []string{
		"observation-window: 1.5\n",
		"observation-window: [1]\n",
		"observation-window: nope\n",
		"min-observations: 2.5\n",
		"min-observations: three\n",
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseValidationErrors(t *testing.T) {
	cases := map[string]string{
		"state-dir: ''":                                        "state-dir",
		"min-observations: 0":                                  "min-observations",
		"min-distinct-sessions: 0":                             "min-distinct-sessions",
		"min-observations: 257":                                "cannot exceed 256",
		"min-observations: 2\nmin-distinct-sessions: 3":        "cannot exceed",
		"observation-window: 0s":                               "observation-window",
		"promotion-cooldown: -1s":                              "promotion-cooldown",
		"claude-entrypoints: []":                               "claude-entrypoints",
		"claude-entrypoints: ['bad entry']":                    "not a valid entrypoint",
		"claude-min-version: 2.1":                              "claude-min-version",
		"codex-min-version: abc":                               "codex-min-version",
		"enabled: [":                                           "parse auto-baseline config",
		"manage-claude: false\nclaude-entrypoints: []\nx: [\n": "parse",
	}
	for raw, want := range cases {
		_, err := Parse([]byte(raw))
		if err == nil {
			t.Errorf("%q accepted", raw)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q error = %q, want substring %q", raw, err, want)
		}
	}
	// Empty entrypoints are fine when Claude is unmanaged.
	if _, err := Parse([]byte("manage-claude: false\nclaude-entrypoints: []\n")); err != nil {
		t.Errorf("unmanaged claude with empty entrypoints rejected: %v", err)
	}
}

func TestDisplayTimezoneFallbacks(t *testing.T) {
	cfg, err := Parse([]byte("display-timezone: Mars/Olympus\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DisplayTimezone != "UTC" || len(cfg.Warnings) != 1 {
		t.Errorf("tz = %q warnings = %v", cfg.DisplayTimezone, cfg.Warnings)
	}
	cfg, _ = Parse([]byte("display-timezone: local\n"))
	if cfg.DisplayTimezone != "Local" || LoadDisplayLocation(cfg.DisplayTimezone) != time.Local {
		t.Errorf("local zone = %q", cfg.DisplayTimezone)
	}
	if LoadDisplayLocation("") != time.UTC || LoadDisplayLocation("garbage") != time.UTC {
		t.Errorf("fallback location is not UTC")
	}
	if LoadDisplayLocation("America/New_York").String() != "America/New_York" {
		t.Errorf("IANA zone did not load")
	}
}

func TestMinObservationsBoundedByRecordCap(t *testing.T) {
	cfg, err := Parse([]byte(fmt.Sprintf("min-observations: %d\n", learner.MaxRecordsPerCandidate)))
	if err != nil {
		t.Fatalf("min-observations at the cap rejected: %v", err)
	}
	if cfg.MinObservations != learner.MaxRecordsPerCandidate {
		t.Errorf("MinObservations = %d", cfg.MinObservations)
	}
	if _, err := Parse([]byte(fmt.Sprintf("min-observations: %d\n", learner.MaxRecordsPerCandidate+1))); err == nil {
		t.Error("min-observations above the record cap accepted")
	}
}

func TestClassifierFingerprintTracksRuleFields(t *testing.T) {
	base, err := Parse([]byte("enabled: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	same, err := Parse([]byte("enabled: true\ndry-run: true\npromotion-cooldown: 5s\ndisplay-timezone: UTC\n"))
	if err != nil {
		t.Fatal(err)
	}
	if base.ClassifierFingerprint() != same.ClassifierFingerprint() {
		t.Error("non-classifier fields changed the fingerprint")
	}
	for _, raw := range []string{
		"require-claude-code-beta: false\n",
		"claude-entrypoints: [cli]\n",
		"min-observations: 5\n",
		"min-distinct-sessions: 2\nmin-observations: 3\n",
		"observation-window: 1h\n",
		"claude-min-version: 2.1.250\n",
		"manage-codex: false\n",
	} {
		cfg, err := Parse([]byte("enabled: true\n" + raw))
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if cfg.ClassifierFingerprint() == base.ClassifierFingerprint() {
			t.Errorf("%q did not change the fingerprint", raw)
		}
	}
}
