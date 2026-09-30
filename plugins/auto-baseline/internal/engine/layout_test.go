package engine

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/auto-baseline/internal/config"
	"github.com/NoorChasib/cpa-plugins/plugins/auto-baseline/internal/configfile"
	"github.com/NoorChasib/cpa-plugins/plugins/auto-baseline/internal/fingerprint"
	"github.com/NoorChasib/cpa-plugins/plugins/auto-baseline/internal/statefile"
)

// v8Config is a pure v8-layout file, as CPA leaves it after a
// /v8/management save, carrying the claude baseline the test candidate
// (2.1.318) is newer than.
const v8Config = `config-version: 8
server:
  port: 8317
access:
  api-keys:
    - "k"
oauth:
  providers:
    claude:
      header-defaults:
        user-agent: "claude-cli/2.1.300 (external, cli)"
        package-version: "0.112.1"
        runtime-version: "v26.3.0"
    codex:
      disable-codex-cloaking: true
` + pluginsEnabledYAML

// countingApply counts real Apply calls.
type countingApply struct {
	mu    sync.Mutex
	calls int
}

func (c *countingApply) apply(path, backupDir string, cand fingerprint.Candidate, check func(configfile.Snapshot) error) (configfile.Snapshot, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return configfile.Apply(path, backupDir, cand, check)
}

func (c *countingApply) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (h *harness) writeConfig(content string) {
	h.t.Helper()
	if err := os.WriteFile(h.config, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func TestV8PromotionWritesTheV8BlockAndReportsSources(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(v8Config)
	h.eng.Start()

	snap := h.status()
	claude, codex := h.claude(), h.codex()
	if snap.Config.Layout != "v8" || claude.Effective.Version.String() != "2.1.300" {
		t.Fatalf("before: layout=%q claude=%+v", snap.Config.Layout, claude.Effective)
	}
	if claude.Effective.Sources["user-agent"] != configfile.SourceV8 || claude.Effective.WriteTarget != "oauth.providers.claude.header-defaults" {
		t.Errorf("claude sources/target = %v %q", claude.Effective.Sources, claude.Effective.WriteTarget)
	}
	if codex.DisableCodexCloaking == nil || !codex.DisableCodexCloaking.Value || codex.DisableCodexCloaking.Source != configfile.SourceV8 {
		t.Errorf("codex cloaking = %+v", codex.DisableCodexCloaking)
	}
	for _, w := range codex.Warnings {
		if strings.Contains(w, "disable-codex-cloaking") {
			t.Errorf("v8 cloaking=true still warned: %s", w)
		}
	}

	h.observeClaude("a", "b", "c")
	text := h.readConfig()
	if !strings.Contains(text, "      header-defaults:\n        user-agent: \"claude-cli/2.1.318 (external, cli)\"\n") {
		t.Errorf("v8 block not updated in place:\n%s", text)
	}
	if strings.Contains(text, "claude-header-defaults") {
		t.Errorf("a legacy key was created beside its v8 counterpart:\n%s", text)
	}
	if !h.logs.contains("promoted claude baseline 2.1.300 -> 2.1.318") || !h.logs.contains("at oauth.providers.claude.header-defaults") {
		t.Errorf("promotion log = %v", h.logs.lines)
	}
	claude = h.claude()
	if claude.LastPromotion == nil || claude.LastPromotion.Target != "oauth.providers.claude.header-defaults" || !claude.AwaitingReload {
		t.Errorf("last promotion = %+v", claude.LastPromotion)
	}
	// CPA's reload re-reads the file and reconfigures the plugin.
	h.eng.Reconfigure(h.cfg)
	claude = h.claude()
	if claude.AwaitingReload || claude.LastPromotion.ConfirmedAt.IsZero() || claude.Paused != nil {
		t.Errorf("reload not confirmed: %+v", claude.LastPromotion)
	}
	if claude.Effective.Sources["package-version"] != configfile.SourceV8 {
		t.Errorf("sources after reload = %v", claude.Effective.Sources)
	}
}

// TestNoLoopWhenV8BlockAlreadyCarriesTheObservedVersion replays the
// 2026-09-29 incident: a v8 write moved the baseline into
// oauth.providers.claude.header-defaults. 0.1.4 looked only at the root key,
// assumed the compiled default, and rewrote a root key every cooldown. The
// v8 value must now be read, so the same observations write nothing.
func TestNoLoopWhenV8BlockAlreadyCarriesTheObservedVersion(t *testing.T) {
	counter := &countingApply{}
	h := newHarness(t, withApply(counter.apply), withConfig(func(c *config.Config) { c.PromotionCooldown = time.Minute }))
	h.writeConfig(strings.Replace(v8Config, "2.1.300", "2.1.318", 1))
	h.eng.Start()
	for i := 0; i < 5; i++ {
		h.observeClaude("a", "b", "c")
		h.clk.Advance(time.Minute)
	}
	if counter.count() != 0 || h.claude().LastPromotion != nil {
		t.Fatalf("promoted over an equal v8 baseline: %d applies, %+v", counter.count(), h.claude().LastPromotion)
	}
	if strings.Contains(h.readConfig(), "claude-header-defaults") {
		t.Error("root claude-header-defaults written into a v8 file")
	}
}

// TestIneffectivePromotionPausesInsteadOfRewriting covers the post-reload
// loop guard: the write lands, CPA's reload (or a panel save) rewrites the
// file without it, and the plugin must stop instead of rewriting every
// cooldown.
func TestIneffectivePromotionPausesInsteadOfRewriting(t *testing.T) {
	counter := &countingApply{}
	h := newHarness(t, withApply(counter.apply), withConfig(func(c *config.Config) { c.PromotionCooldown = time.Minute }))
	h.writeConfig(v8Config)
	h.eng.Start()
	h.observeClaude("a", "b", "c")
	if counter.count() != 1 || !strings.Contains(h.readConfig(), "2.1.318") {
		t.Fatalf("first promotion did not land: %d applies", counter.count())
	}
	// The file is rewritten without the promotion, then CPA reloads.
	h.writeConfig(v8Config)
	h.eng.Reconfigure(h.cfg)

	claude := h.claude()
	if claude.Paused == nil || claude.Paused.Reason != DecisionPromotionNotEffective || claude.Paused.ConfigSHA256 != "" {
		t.Fatalf("not paused after an ineffective promotion: %+v", claude.Paused)
	}
	if claude.AwaitingReload || claude.LastPromotion.NotEffectiveAt.IsZero() {
		t.Errorf("last promotion = %+v", claude.LastPromotion)
	}
	if !strings.Contains(strings.Join(claude.Warnings, "\n"), "promotion is paused") {
		t.Errorf("no status warning: %v", claude.Warnings)
	}
	snap := h.status()
	if snap.Counters.Decisions[DecisionPromotionNotEffective] != 1 || !strings.Contains(snap.LastError, DecisionPromotionNotEffective) {
		t.Errorf("decision/last_error = %v %q", snap.Counters.Decisions, snap.LastError)
	}
	if !h.logs.contains("auto-baseline: promotion_not_effective:") {
		t.Errorf("not logged: %v", h.logs.lines)
	}
	// Persisted at once, not only by the periodic save.
	if st, err := statefile.Load(h.cfg.StateDir); err != nil || st.Paused[fingerprint.ProviderClaude] == nil {
		t.Errorf("pause not saved immediately: %+v (%v)", st.Paused, err)
	}

	// Many cooldowns of fresh evidence: nothing is rewritten.
	for i := 0; i < 5; i++ {
		h.clk.Advance(time.Minute)
		h.observeClaude("d", "e", "f")
		h.eng.Reconfigure(h.cfg)
	}
	if counter.count() != 1 || strings.Contains(h.readConfig(), "2.1.318") {
		t.Fatalf("rewrote while paused: %d applies\n%s", counter.count(), h.readConfig())
	}
	if out := h.eng.ReportObservation(Report{Provider: "claude", UserAgent: "claude-cli/2.1.318 (external, cli)", PackageVersion: "0.112.1", RuntimeVersion: "v26.3.0", OS: "Linux", Arch: "x64", Force: true}); out.Queued {
		t.Errorf("force bypassed the pause: %+v", out)
	}

	// The pause survives a restart.
	h2 := newHarness(t, withApply(counter.apply), withConfig(func(c *config.Config) {
		c.StateDir = h.cfg.StateDir
		c.ConfigPath = h.config
		c.PromotionCooldown = time.Minute
	}))
	h.eng.Shutdown()
	h2.eng.Start()
	h2.observeClaude("g", "h", "i")
	if counter.count() != 1 || h2.claude().Paused == nil {
		t.Fatalf("pause lost across restart: %d applies, %+v", counter.count(), h2.claude().Paused)
	}

	// The operator's reset resumes promotion: exactly one more write.
	h2.eng.Reset()
	if h2.claude().Paused != nil {
		t.Fatal("reset did not lift the pause")
	}
	h2.observeClaude("g", "h", "i")
	h2.clk.Advance(time.Minute) // h2's clock restarted at t0, inside the first write's cooldown
	if counter.count() != 2 || !strings.Contains(h.readConfig(), "2.1.318") {
		t.Fatalf("no promotion after reset: %d applies", counter.count())
	}
	st, err := statefile.Load(h.cfg.StateDir)
	if err != nil || len(st.Paused) != 0 {
		t.Errorf("saved pauses = %+v (%v)", st.Paused, err)
	}
}

func TestWriteSettingChangeResumesPausedProvider(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(v8Config)
	h.eng.Start()
	h.observeClaude("a", "b", "c")
	h.writeConfig(v8Config)
	h.eng.Reconfigure(h.cfg)
	if h.claude().Paused == nil {
		t.Fatal("precondition: paused")
	}
	// CPA reconfigures on every reload; an unchanged config keeps the pause.
	h.eng.Reconfigure(h.cfg)
	if h.claude().Paused == nil {
		t.Fatal("an ordinary reload lifted the pause")
	}
	cfg := h.cfg
	cfg.DryRun = true
	h.eng.Reconfigure(cfg)
	if h.claude().Paused != nil {
		t.Error("a dry-run change did not resume the provider")
	}
}

// TestLoopGuardRefusalPausesUntilTheFileChanges covers the pre-write guard:
// the rendered file would not carry the candidate, so nothing is written and
// further evidence does not re-render the same file.
func TestLoopGuardRefusalPausesUntilTheFileChanges(t *testing.T) {
	counter := &countingApply{}
	h := newHarness(t, withApply(counter.apply))
	// Editing the anchored claude block would also change the codex block
	// that aliases it.
	aliased := baseConfig + "claude-header-defaults: &hd\n  user-agent: \"claude-cli/2.1.300 (external, cli)\"\ncodex-header-defaults: *hd\n"
	h.writeConfig(aliased)
	h.eng.Start()
	h.observeClaude("a", "b", "c")
	if counter.count() != 1 || h.readConfig() != aliased {
		t.Fatalf("guard did not refuse: %d applies\n%s", counter.count(), h.readConfig())
	}
	claude := h.claude()
	if claude.Paused == nil || claude.Paused.ConfigSHA256 == "" {
		t.Fatalf("guard refusal did not pause: %+v", claude.Paused)
	}
	h.observeClaude("d", "e", "f")
	if counter.count() != 1 {
		t.Errorf("paused provider re-rendered: %d applies", counter.count())
	}
	// Fixing the file lifts the pause on the next read.
	h.writeConfig(baseConfig + "claude-header-defaults:\n  user-agent: \"claude-cli/2.1.300 (external, cli)\"\n")
	h.eng.Reconfigure(h.cfg)
	if h.claude().Paused != nil {
		t.Fatal("file change did not lift the guard pause")
	}
	h.observeClaude("g", "h", "i")
	if counter.count() != 2 || !strings.Contains(h.readConfig(), "2.1.318") {
		t.Fatalf("no promotion after the fix: %d applies\n%s", counter.count(), h.readConfig())
	}
}

func TestDryRunOnV8FilePreviewsTheV8Target(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) { c.DryRun = true }))
	h.writeConfig(v8Config)
	h.eng.Start()
	h.observeClaude("a", "b", "c")
	if h.readConfig() != v8Config {
		t.Fatal("dry-run wrote config.yaml")
	}
	lp := h.claude().LastPromotion
	if lp == nil || !lp.DryRun || lp.Target != "oauth.providers.claude.header-defaults" || lp.From.String() != "2.1.300" {
		t.Errorf("dry-run record = %+v", lp)
	}
	if !h.logs.contains("dry-run would promote claude baseline 2.1.300 -> 2.1.318") || !h.logs.contains("at oauth.providers.claude.header-defaults") {
		t.Errorf("dry-run log = %v", h.logs.lines)
	}
	// A file the live write would refuse is reported in dry-run too.
	h2 := newHarness(t, withConfig(func(c *config.Config) { c.DryRun = true }))
	h2.writeConfig("config-version: 8\noauth:\n  providers:\n    claude:\n      <<: {header-defaults: {user-agent: \"claude-cli/2.1.300 (external, cli)\"}}\n" + pluginsEnabledYAML)
	h2.eng.Start()
	h2.observeClaude("a", "b", "c")
	if lp := h2.claude().LastPromotion; lp != nil {
		t.Errorf("dry-run claimed a promotion the write would refuse: %+v", lp)
	}
	if le := h2.status().LastError; !strings.Contains(le, "unsupported_config_shape") {
		t.Errorf("dry-run refusal not surfaced: %q", le)
	}
}

// TestReadStartedBeforeTheWriteProvesNothing: a reload that read the file
// before a promotion was recorded must not be taken as proof that the write
// was lost.
func TestReadStartedBeforeTheWriteProvesNothing(t *testing.T) {
	var h *harness
	trigger := false
	h = newHarness(t, withRead(func(path string) (configfile.Snapshot, error) {
		snap, err := configfile.Read(path)
		if trigger {
			trigger = false
			// The promotion lands after this read but before its snapshot
			// is applied.
			h.observeClaude("a", "b", "c")
		}
		return snap, err
	}))
	h.eng.Start()
	trigger = true
	h.eng.Reconfigure(h.cfg)
	claude := h.claude()
	if !claude.AwaitingReload || claude.Paused != nil || !claude.LastPromotion.NotEffectiveAt.IsZero() {
		t.Fatalf("stale read judged the write: awaiting=%t paused=%+v", claude.AwaitingReload, claude.Paused)
	}
	h.eng.Reconfigure(h.cfg)
	if c := h.claude(); c.AwaitingReload || c.LastPromotion.ConfirmedAt.IsZero() {
		t.Errorf("fresh read did not confirm: %+v", c.LastPromotion)
	}
}
