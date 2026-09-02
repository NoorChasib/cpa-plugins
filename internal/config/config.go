// Package config parses the plugin's own YAML config subtree.
//
// The host sends the complete preserved `plugins.configs.auto-baseline`
// subtree as the `config_yaml` field of plugin.register / plugin.reconfigure
// (a []byte, so base64 on the wire; the ABI layer decodes it before this
// package sees plain YAML bytes). There is no host callback for reading or
// writing the rest of config.yaml; the plugin reads and edits that file
// directly (see internal/configfile).
package config

import (
	"bytes"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/fingerprint"
	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/learner"
)

// Defaults for every tunable.
const (
	// DefaultStateDir is relative to the CPA working directory, which is the
	// same directory that holds the default plugin dir. The plugin scanner
	// only loads *.so files from the plugin dir root, so a JSON state file in
	// a subdirectory is never mistaken for a plugin.
	DefaultStateDir        = "plugins/auto-baseline"
	DefaultMinObservations = 3
	// DefaultMinDistinctSessions is 1 because anonymous observations (no
	// session header) never count as a session; operators whose clients send
	// X-Claude-Code-Session-Id should raise it to 2.
	DefaultMinDistinctSessions   = 1
	DefaultObservationWindow     = 24 * time.Hour
	DefaultPromotionCooldown     = 60 * time.Second
	DefaultRequireClaudeCodeBeta = true
	// DefaultDisplayTimezone is the IANA zone used to render timestamps on
	// the management HTML status view. It never affects learning or the JSON
	// status route, which stay in RFC3339 UTC.
	DefaultDisplayTimezone = "UTC"
)

// defaultClaudeEntrypoints is the default allowlist of Claude Code
// entrypoints whose fingerprints may be learned. sdk-ts and sdk-py are
// included because Agent SDK hosts (e.g. T3 Code) drive the same Claude Code
// binary and send its real version, package version, and runtime version.
var defaultClaudeEntrypoints = [...]string{"cli", "sdk-cli", "claude-vscode", "sdk-ts", "sdk-py"}

// entrypointPattern bounds entrypoint tokens to the shape CPA itself accepts
// inside the User-Agent (no commas or parentheses).
var entrypointPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Config is the validated plugin configuration.
type Config struct {
	// Enabled mirrors the per-plugin enabled flag the host preserves in the
	// subtree. The host normally does not activate disabled plugins at all;
	// this is a defensive second gate.
	Enabled bool
	// DryRun computes and reports promotions but never writes config.yaml.
	DryRun bool
	// ConfigPath optionally overrides CPA config.yaml discovery.
	ConfigPath string
	// StateDir holds state.json (learned candidates, history, counters).
	StateDir string
	// BackupDir receives config.yaml.auto-baseline.bak before each write.
	// Empty means "same as StateDir".
	BackupDir string
	// ManageClaude / ManageCodex select which baselines are learned.
	ManageClaude bool
	ManageCodex  bool
	// ClaudeEntrypoints is the lowercase allowlist of Claude Code entrypoints.
	ClaudeEntrypoints []string
	// RequireClaudeCodeBeta requires the anthropic-beta header to carry the
	// claude-code-20250219 beta before a Claude request counts.
	RequireClaudeCodeBeta bool
	// MinObservations is the number of observations of one candidate key
	// required inside ObservationWindow before promotion.
	MinObservations int
	// MinDistinctSessions is the number of distinct session IDs those
	// observations must span.
	MinDistinctSessions int
	// ObservationWindow bounds how old an observation may be and still count.
	ObservationWindow time.Duration
	// PromotionCooldown is the minimum spacing between config.yaml writes.
	PromotionCooldown time.Duration
	// ClaudeMinVersion / CodexMinVersion are floors: the plugin never writes a
	// baseline older than these, even if config.yaml currently holds an
	// older explicit value.
	ClaudeMinVersion fingerprint.Version
	CodexMinVersion  fingerprint.Version
	// RequireExplicitBaseline refuses to promote a provider whose on-disk
	// baseline is implicit (compiled default assumed). It is the safe choice
	// after a CPA upgrade whose compiled defaults the plugin does not know.
	RequireExplicitBaseline bool
	// DisplayTimezone is the validated IANA zone name (or "Local") used only
	// for human-readable timestamps on the management HTML view.
	DisplayTimezone string
	// Warnings carries sanitized, non-fatal parse notes for status display.
	Warnings []string
}

// Manages reports whether a provider baseline is managed.
func (c Config) Manages(provider fingerprint.Provider) bool {
	switch provider {
	case fingerprint.ProviderClaude:
		return c.ManageClaude
	case fingerprint.ProviderCodex:
		return c.ManageCodex
	default:
		return false
	}
}

// MinVersion returns the write floor for a provider.
func (c Config) MinVersion(provider fingerprint.Provider) fingerprint.Version {
	switch provider {
	case fingerprint.ProviderClaude:
		return c.ClaudeMinVersion
	case fingerprint.ProviderCodex:
		return c.CodexMinVersion
	default:
		return fingerprint.Version{}
	}
}

// Rules returns the fingerprint classification rules derived from the config.
func (c Config) Rules() fingerprint.Rules {
	return fingerprint.Rules{
		ClaudeEntrypoints:     c.ClaudeEntrypoints,
		RequireClaudeCodeBeta: c.RequireClaudeCodeBeta,
	}
}

// rawConfig models the YAML subtree. Pointers distinguish absent from zero.
// `priority` (load order) and `store` (Plugin Store install metadata,
// internal/pluginhost/config.go pluginConfigDesiredVersion) are host-owned
// keys that share the subtree and are accepted but ignored. Every other
// unknown key is rejected so a misspelling such as `dry_run` cannot silently
// leave a default in place.
type rawConfig struct {
	Enabled               *bool      `yaml:"enabled"`
	LoadPriority          any        `yaml:"priority"` // host-owned; ignored
	Store                 yaml.Node  `yaml:"store"`    // host-owned; ignored
	DryRun                *bool      `yaml:"dry-run"`
	ConfigPath            *string    `yaml:"config-path"`
	StateDir              *string    `yaml:"state-dir"`
	BackupDir             *string    `yaml:"backup-dir"`
	ManageClaude          *bool      `yaml:"manage-claude"`
	ManageCodex           *bool      `yaml:"manage-codex"`
	ClaudeEntrypoints     *[]string  `yaml:"claude-entrypoints"`
	RequireClaudeCodeBeta *bool      `yaml:"require-claude-code-beta"`
	MinObservations       *strictInt `yaml:"min-observations"`
	MinDistinctSessions   *strictInt `yaml:"min-distinct-sessions"`
	ObservationWindow     *duration  `yaml:"observation-window"`
	PromotionCooldown     *duration  `yaml:"promotion-cooldown"`
	ClaudeMinVersion      *string    `yaml:"claude-min-version"`
	CodexMinVersion       *string    `yaml:"codex-min-version"`
	RequireExplicit       *bool      `yaml:"require-explicit-baseline"`
	DisplayTimezone       *string    `yaml:"display-timezone"`
}

// duration accepts Go duration strings ("24h", "60s") and bare YAML integers
// (seconds). yaml.v3 silently truncates floats when decoding into integer
// types, so non-integer numeric scalars are rejected instead of truncated.
type duration struct{ d time.Duration }

func (d *duration) UnmarshalYAML(node *yaml.Node) error {
	switch node.Tag {
	case "!!int":
		var asInt int64
		if err := node.Decode(&asInt); err != nil {
			return fmt.Errorf("invalid duration integer")
		}
		const (
			maxSeconds = int64(math.MaxInt64) / int64(time.Second)
			minSeconds = int64(math.MinInt64) / int64(time.Second)
		)
		if asInt > maxSeconds || asInt < minSeconds {
			return fmt.Errorf("duration seconds overflow time.Duration")
		}
		d.d = time.Duration(asInt) * time.Second
		return nil
	case "!!str":
		var asString string
		if err := node.Decode(&asString); err != nil {
			return fmt.Errorf("invalid duration value")
		}
		parsed, errParse := time.ParseDuration(strings.TrimSpace(asString))
		if errParse != nil {
			return fmt.Errorf("invalid duration %q", asString)
		}
		d.d = parsed
		return nil
	case "!!float":
		return fmt.Errorf("duration must be whole seconds or a duration string like \"60s\"; float values are rejected to avoid truncation")
	default:
		return fmt.Errorf("invalid duration value")
	}
}

// strictInt accepts only true YAML integers.
type strictInt struct{ v int }

func (s *strictInt) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
		return fmt.Errorf("value must be an integer; float values are rejected to avoid truncation")
	}
	if err := node.Decode(&s.v); err != nil {
		return fmt.Errorf("invalid integer value")
	}
	return nil
}

// Defaults returns the default configuration with Enabled=false.
func Defaults() Config {
	return Config{
		Enabled:               false,
		DryRun:                false,
		ConfigPath:            "",
		StateDir:              DefaultStateDir,
		ManageClaude:          true,
		ManageCodex:           true,
		ClaudeEntrypoints:     append([]string(nil), defaultClaudeEntrypoints[:]...),
		RequireClaudeCodeBeta: DefaultRequireClaudeCodeBeta,
		MinObservations:       DefaultMinObservations,
		MinDistinctSessions:   DefaultMinDistinctSessions,
		ObservationWindow:     DefaultObservationWindow,
		PromotionCooldown:     DefaultPromotionCooldown,
		ClaudeMinVersion:      fingerprint.CompiledClaudeBaselineVersion,
		CodexMinVersion:       fingerprint.CompiledCodexBaselineVersion,
		DisplayTimezone:       DefaultDisplayTimezone,
	}
}

// Parse decodes and validates the plugin config subtree. A nil/empty subtree
// yields defaults with Enabled=false (matching the host's default runtime
// config for unconfigured plugins).
func Parse(configYAML []byte) (Config, error) {
	cfg := Defaults()
	trimmed := strings.TrimSpace(string(configYAML))
	if trimmed == "" {
		return cfg, nil
	}

	var raw rawConfig
	dec := yaml.NewDecoder(bytes.NewReader(configYAML))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return Config{}, fmt.Errorf("parse auto-baseline config: %w", err)
	}

	if raw.Enabled != nil {
		cfg.Enabled = *raw.Enabled
	}
	if raw.DryRun != nil {
		cfg.DryRun = *raw.DryRun
	}
	if raw.ConfigPath != nil {
		cfg.ConfigPath = strings.TrimSpace(*raw.ConfigPath)
	}
	if raw.StateDir != nil {
		cfg.StateDir = strings.TrimSpace(*raw.StateDir)
	}
	if raw.BackupDir != nil {
		cfg.BackupDir = strings.TrimSpace(*raw.BackupDir)
	}
	if raw.ManageClaude != nil {
		cfg.ManageClaude = *raw.ManageClaude
	}
	if raw.ManageCodex != nil {
		cfg.ManageCodex = *raw.ManageCodex
	}
	if raw.ClaudeEntrypoints != nil {
		entrypoints := make([]string, 0, len(*raw.ClaudeEntrypoints))
		for _, item := range *raw.ClaudeEntrypoints {
			token := strings.ToLower(strings.TrimSpace(item))
			if token == "" {
				continue
			}
			if !entrypointPattern.MatchString(token) {
				return Config{}, fmt.Errorf("claude-entrypoints entry %q is not a valid entrypoint token", item)
			}
			entrypoints = append(entrypoints, token)
		}
		cfg.ClaudeEntrypoints = entrypoints
	}
	if raw.RequireClaudeCodeBeta != nil {
		cfg.RequireClaudeCodeBeta = *raw.RequireClaudeCodeBeta
	}
	if raw.MinObservations != nil {
		cfg.MinObservations = raw.MinObservations.v
	}
	if raw.MinDistinctSessions != nil {
		cfg.MinDistinctSessions = raw.MinDistinctSessions.v
	}
	if raw.ObservationWindow != nil {
		cfg.ObservationWindow = raw.ObservationWindow.d
	}
	if raw.PromotionCooldown != nil {
		cfg.PromotionCooldown = raw.PromotionCooldown.d
	}
	if raw.ClaudeMinVersion != nil {
		v, err := fingerprint.ParseVersion(*raw.ClaudeMinVersion)
		if err != nil {
			return Config{}, fmt.Errorf("claude-min-version: %w", err)
		}
		cfg.ClaudeMinVersion = v
	}
	if raw.CodexMinVersion != nil {
		v, err := fingerprint.ParseVersion(*raw.CodexMinVersion)
		if err != nil {
			return Config{}, fmt.Errorf("codex-min-version: %w", err)
		}
		cfg.CodexMinVersion = v
	}
	if raw.RequireExplicit != nil {
		cfg.RequireExplicitBaseline = *raw.RequireExplicit
	}
	if raw.DisplayTimezone != nil {
		name, warning := resolveDisplayTimezone(*raw.DisplayTimezone)
		cfg.DisplayTimezone = name
		if warning != "" {
			cfg.Warnings = append(cfg.Warnings, warning)
		}
	}

	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// resolveDisplayTimezone validates a display-timezone value. Because the zone
// only affects presentation, an unknown name degrades to UTC with a warning.
func resolveDisplayTimezone(value string) (string, string) {
	trimmed := strings.TrimSpace(value)
	switch strings.ToLower(trimmed) {
	case "", "utc", "z":
		return DefaultDisplayTimezone, ""
	case "local":
		return "Local", ""
	}
	loc, err := time.LoadLocation(trimmed)
	if err != nil {
		return DefaultDisplayTimezone, fmt.Sprintf(
			"display-timezone %q is not a known IANA zone; timestamps are shown in UTC", trimmed)
	}
	return loc.String(), ""
}

// LoadDisplayLocation resolves a validated display zone name to a location,
// treating any failure as UTC so rendering never fails on presentation input.
func LoadDisplayLocation(name string) *time.Location {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "utc", "z":
		return time.UTC
	case "local":
		return time.Local
	}
	loc, err := time.LoadLocation(strings.TrimSpace(name))
	if err != nil {
		return time.UTC
	}
	return loc
}

func validate(cfg Config) error {
	if cfg.StateDir == "" {
		return fmt.Errorf("state-dir must not be empty")
	}
	if cfg.MinObservations < 1 {
		return fmt.Errorf("min-observations must be at least 1")
	}
	if cfg.MinObservations > learner.MaxRecordsPerCandidate {
		// The learner keeps at most MaxRecordsPerCandidate records per
		// candidate, so a larger quorum could never be reached.
		return fmt.Errorf("min-observations (%d) cannot exceed %d, the maximum number of observations retained per candidate", cfg.MinObservations, learner.MaxRecordsPerCandidate)
	}
	if cfg.MinDistinctSessions < 1 {
		return fmt.Errorf("min-distinct-sessions must be at least 1")
	}
	if cfg.MinDistinctSessions > cfg.MinObservations {
		return fmt.Errorf("min-distinct-sessions (%d) cannot exceed min-observations (%d)", cfg.MinDistinctSessions, cfg.MinObservations)
	}
	if cfg.ObservationWindow <= 0 {
		return fmt.Errorf("observation-window must be positive")
	}
	if cfg.PromotionCooldown < 0 {
		return fmt.Errorf("promotion-cooldown must not be negative")
	}
	if cfg.ManageClaude && len(cfg.ClaudeEntrypoints) == 0 {
		return fmt.Errorf("claude-entrypoints must list at least one entrypoint while manage-claude is true")
	}
	return nil
}

// EffectiveBackupDir is BackupDir or, when unset, StateDir.
func (c Config) EffectiveBackupDir() string {
	if c.BackupDir != "" {
		return c.BackupDir
	}
	return c.StateDir
}

// ClassifierFingerprint summarizes every field that changes which
// observations are accepted or how quorum is reached. When it changes on
// reconfigure, previously collected evidence was gathered under different
// rules and is discarded.
func (c Config) ClassifierFingerprint() string {
	return fmt.Sprintf("claude=%t codex=%t ep=%s beta=%t obs=%d sess=%d win=%s floors=%s/%s",
		c.ManageClaude, c.ManageCodex, strings.Join(c.ClaudeEntrypoints, ","), c.RequireClaudeCodeBeta,
		c.MinObservations, c.MinDistinctSessions, c.ObservationWindow, c.ClaudeMinVersion, c.CodexMinVersion)
}

// WriteFingerprint summarizes every field that affects WHERE or WHETHER the
// plugin writes. When it changes on reconfigure, in-flight promotion work
// must be drained before the new config takes effect.
func (c Config) WriteFingerprint() string {
	return fmt.Sprintf("enabled=%t dry=%t cfg=%s bak=%s state=%s claude=%t codex=%t floors=%s/%s explicit=%t",
		c.Enabled, c.DryRun, c.ConfigPath, c.EffectiveBackupDir(), c.StateDir, c.ManageClaude, c.ManageCodex,
		c.ClaudeMinVersion, c.CodexMinVersion, c.RequireExplicitBaseline)
}
