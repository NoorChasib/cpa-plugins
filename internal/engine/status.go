package engine

import (
	"fmt"
	"sort"
	"time"

	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/fingerprint"
	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/learner"
	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/statefile"
)

// Snapshot is the published status document (JSON status route and the
// authenticated HTML view). It contains no credentials and no config file
// contents beyond the baseline header values the plugin manages.
type Snapshot struct {
	Plugin      string    `json:"plugin"`
	Version     string    `json:"version"`
	GeneratedAt time.Time `json:"generated_at"`
	Enabled     bool      `json:"enabled"`
	Stopped     bool      `json:"stopped"`
	// Faulted is set when a plugin goroutine panicked; writes are suspended
	// until the next reconfigure.
	Faulted         bool   `json:"faulted"`
	FaultReason     string `json:"fault_reason,omitempty"`
	DryRun          bool   `json:"dry_run"`
	DisplayTimezone string `json:"display_timezone"`

	Config    ConfigStatus          `json:"config_file"`
	Backup    BackupStatus          `json:"backup"`
	State     StateStatus           `json:"state_file"`
	Rules     RulesStatus           `json:"rules"`
	Baselines []ProviderStatus      `json:"providers"`
	History   []statefile.Promotion `json:"history"`

	Counters    statefile.Counters `json:"counters"`
	Warnings    []string           `json:"warnings,omitempty"`
	LastError   string             `json:"last_error,omitempty"`
	LastErrorAt time.Time          `json:"last_error_at,omitempty"`

	// CPAVersion is the audited build whose compiled defaults are assumed.
	CPAVersion string `json:"assumed_cpa_version"`
}

// ConfigStatus describes the managed config.yaml.
type ConfigStatus struct {
	Path     string    `json:"path"`
	Source   string    `json:"path_source"`
	Exists   bool      `json:"exists"`
	Writable bool      `json:"writable"`
	Error    string    `json:"error,omitempty"`
	ReadAt   time.Time `json:"read_at,omitempty"`
	SHA256   string    `json:"sha256,omitempty"`
	// Mode is the detected CPA configuration source when it is not a plain
	// local file (home, postgres, object-store, git-store).
	Mode       string `json:"mode,omitempty"`
	ModeReason string `json:"mode_reason,omitempty"`
	// ModeUnsupported is set when Mode is non-empty and config-path was not
	// set explicitly; automatic writes are disabled.
	ModeUnsupported bool `json:"mode_unsupported"`
}

// BackupStatus describes the backup directory.
type BackupStatus struct {
	Dir      string `json:"dir"`
	Writable bool   `json:"writable"`
	Error    string `json:"error,omitempty"`
}

// StateStatus describes the state file.
type StateStatus struct {
	Dir     string    `json:"dir"`
	Loaded  bool      `json:"loaded"`
	SavedAt time.Time `json:"saved_at,omitempty"`
	Error   string    `json:"error,omitempty"`
	// RestoredDropped counts restored candidates discarded by validation at
	// the last Start.
	RestoredDropped int `json:"restored_dropped,omitempty"`
}

// RulesStatus echoes the effective quorum rules.
type RulesStatus struct {
	MinObservations       int      `json:"min_observations"`
	MinDistinctSessions   int      `json:"min_distinct_sessions"`
	ObservationWindow     string   `json:"observation_window"`
	PromotionCooldown     string   `json:"promotion_cooldown"`
	ClaudeEntrypoints     []string `json:"claude_entrypoints"`
	RequireClaudeCodeBeta bool     `json:"require_claude_code_beta"`
}

// ProviderStatus is one provider's baseline and evidence.
type ProviderStatus struct {
	Provider   fingerprint.Provider `json:"provider"`
	Managed    bool                 `json:"managed"`
	MinVersion fingerprint.Version  `json:"min_version"`
	// Effective is the baseline CPA currently applies (from config.yaml or the
	// compiled default).
	Effective      EffectiveStatus      `json:"effective_baseline"`
	Pending        []learner.Evidence   `json:"pending_candidates"`
	LastPromotion  *statefile.Promotion `json:"last_promotion,omitempty"`
	NextWriteAfter time.Time            `json:"next_write_allowed_at,omitempty"`
	// AwaitingReload is set after a write until CPA's hot reload is observed.
	AwaitingReload bool     `json:"awaiting_reload"`
	Warnings       []string `json:"warnings,omitempty"`
}

// EffectiveStatus is the on-disk/compiled baseline.
type EffectiveStatus struct {
	Version        fingerprint.Version `json:"version"`
	UserAgent      string              `json:"user_agent"`
	PackageVersion string              `json:"package_version,omitempty"`
	RuntimeVersion string              `json:"runtime_version,omitempty"`
	Explicit       bool                `json:"explicit_in_config"`
	Malformed      bool                `json:"malformed_user_agent,omitempty"`
	// Unsupported names a structural problem in the provider block
	// (duplicate_key, unsupported_config_shape) that blocks promotion.
	Unsupported string `json:"unsupported,omitempty"`
}

// Status publishes the current snapshot.
func (e *Engine) Status(pluginID, pluginVersion string) Snapshot {
	faulted, faultReason := e.isFaulted()
	e.mu.Lock()
	defer e.mu.Unlock()
	if faulted && e.state.LastError == "" {
		e.state.LastError = faultReason
		e.state.LastErrorAt = e.clk.Now()
	}
	now := e.clk.Now()
	snap := Snapshot{
		Plugin:          pluginID,
		Version:         pluginVersion,
		GeneratedAt:     now,
		Enabled:         e.cfg.Enabled,
		Stopped:         e.stopped || !e.started,
		Faulted:         faulted,
		FaultReason:     faultReason,
		DryRun:          e.cfg.DryRun,
		DisplayTimezone: e.cfg.DisplayTimezone,
		Config: ConfigStatus{
			Path:            e.configPath,
			Source:          e.configSource,
			Exists:          e.configExists,
			Writable:        e.configWritable,
			Error:           e.configError,
			ReadAt:          e.configReadAt,
			SHA256:          e.configHash,
			Mode:            e.mode.Name,
			ModeReason:      e.mode.Reason,
			ModeUnsupported: e.modeUnsupported,
		},
		Backup: BackupStatus{
			Dir:      e.backupDir,
			Writable: e.backupWritable,
			Error:    e.backupError,
		},
		State: StateStatus{
			Dir:             e.cfg.StateDir,
			Loaded:          e.stateLoaded,
			SavedAt:         e.stateSaveAt,
			Error:           e.stateError,
			RestoredDropped: e.restoredDropped,
		},
		Rules: RulesStatus{
			MinObservations:       e.cfg.MinObservations,
			MinDistinctSessions:   e.cfg.MinDistinctSessions,
			ObservationWindow:     e.cfg.ObservationWindow.String(),
			PromotionCooldown:     e.cfg.PromotionCooldown.String(),
			ClaudeEntrypoints:     append([]string(nil), e.cfg.ClaudeEntrypoints...),
			RequireClaudeCodeBeta: e.cfg.RequireClaudeCodeBeta,
		},
		Counters:    copyCounters(e.state.Counters),
		Warnings:    append([]string(nil), e.cfg.Warnings...),
		LastError:   e.state.LastError,
		LastErrorAt: e.state.LastErrorAt,
		CPAVersion:  fingerprint.CompiledCPAVersion,
	}
	if e.cfg.DryRun {
		snap.Warnings = append(snap.Warnings, "dry-run is enabled: promotions are computed and logged but config.yaml is never written")
	}
	if faulted {
		snap.Warnings = append(snap.Warnings, "a plugin goroutine panicked; writes are suspended until the plugin is reconfigured: "+faultReason)
	}
	if e.modeUnsupported {
		snap.Warnings = append(snap.Warnings, fmt.Sprintf("CPA appears to run in %s mode (%s); the local config file is not the effective configuration and automatic writes are disabled. Set config-path explicitly or run a host-side updater.", e.mode.Name, e.mode.Reason))
	}
	for _, p := range providers {
		ps := ProviderStatus{
			Provider:   p,
			Managed:    e.cfg.Manages(p),
			MinVersion: e.cfg.MinVersion(p),
			Pending:    e.learner.Summarize(p),
		}
		if eff, ok := e.effective[p]; ok {
			ps.Effective = EffectiveStatus{
				Version:        eff.Version,
				UserAgent:      eff.UserAgent,
				PackageVersion: eff.PackageVersion,
				RuntimeVersion: eff.RuntimeVersion,
				Explicit:       eff.Explicit,
				Malformed:      eff.Malformed,
				Unsupported:    eff.Unsupported,
			}
			if eff.Unsupported != "" {
				ps.Warnings = append(ps.Warnings, "the "+string(p)+" block in config.yaml has a structure the plugin will not edit ("+eff.Unsupported+"); evidence is retained and promotion resumes once it is fixed")
			} else if eff.Malformed {
				ps.Warnings = append(ps.Warnings, "the explicit user-agent in config.yaml does not parse as a client version; evidence is retained but the plugin refuses to promote this provider until it is fixed (decision: baseline_malformed)")
			} else if e.cfg.RequireExplicitBaseline && !eff.Explicit && ps.Managed {
				ps.Warnings = append(ps.Warnings, "require-explicit-baseline is set and config.yaml carries no explicit "+string(p)+" baseline; nothing will be promoted for it (decision: baseline_implicit)")
			}
		} else {
			ps.Effective = compiledEffective(p)
		}
		if lp := e.state.LastPromotion[p]; lp != nil {
			cp := *lp
			ps.LastPromotion = &cp
		}
		if since, waiting := e.awaitingReload[p]; waiting {
			ps.AwaitingReload = true
			if now.Sub(since) > reloadGracePeriod {
				ps.Warnings = append(ps.Warnings, fmt.Sprintf("config.yaml was written %s ago but CPA has not reloaded it; check the file watcher, the config path CPA actually uses, and home mode", now.Sub(since).Truncate(time.Second)))
			}
		}
		if last, ok := e.state.LastWriteAt[p]; ok && e.cfg.PromotionCooldown > 0 {
			if until := last.Add(e.cfg.PromotionCooldown); until.After(now) {
				ps.NextWriteAfter = until
			}
		}
		if p == fingerprint.ProviderCodex && ps.Managed && !e.disableCodex {
			ps.Warnings = append(ps.Warnings, "codex.disable-codex-cloaking is not true: CPA forces its compiled Codex User-Agent on outbound requests, so a learned codex-header-defaults.user-agent has no effect until the operator sets codex.disable-codex-cloaking: true")
		}
		snap.Baselines = append(snap.Baselines, ps)
	}
	history := append([]statefile.Promotion(nil), e.state.History...)
	sort.Slice(history, func(i, j int) bool { return history[i].At.After(history[j].At) })
	snap.History = history
	return snap
}

func compiledEffective(p fingerprint.Provider) EffectiveStatus {
	if p == fingerprint.ProviderCodex {
		return EffectiveStatus{Version: fingerprint.CompiledCodexBaselineVersion, UserAgent: fingerprint.CompiledCodexUserAgent}
	}
	return EffectiveStatus{
		Version:        fingerprint.CompiledClaudeBaselineVersion,
		UserAgent:      fingerprint.CompiledClaudeUserAgent,
		PackageVersion: fingerprint.CompiledClaudePackageVersion,
		RuntimeVersion: fingerprint.CompiledClaudeRuntimeVersion,
	}
}

func copyCounters(c statefile.Counters) statefile.Counters {
	out := c
	out.RejectReasons = make(map[string]uint64, len(c.RejectReasons))
	for k, v := range c.RejectReasons {
		out.RejectReasons[k] = v
	}
	out.Decisions = make(map[string]uint64, len(c.Decisions))
	for k, v := range c.Decisions {
		out.Decisions[k] = v
	}
	return out
}
