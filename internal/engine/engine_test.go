package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/clock"
	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/config"
	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/configfile"
	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/fingerprint"
	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/learner"
	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/statefile"
)

var t0 = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) log(level, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+": "+msg)
}

func (l *logSink) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if strings.Contains(line, sub) {
			n++
		}
	}
	return n
}

func (l *logSink) contains(sub string) bool { return l.count(sub) > 0 }

type harness struct {
	t           *testing.T
	config      string
	clk         *clock.Fake
	logs        *logSink
	cfg         config.Config
	eng         *Engine
	mode        configfile.DeploymentMode
	apply       ApplyFunc
	dryRunApply DryRunApplyFunc
	async       func(func())
}

// baseConfig enables the plugin on disk: the promotion worker refuses to
// write when plugins.enabled or plugins.configs.auto-baseline.enabled is not
// true in the fresh read (plugin_disabled_on_disk).
const (
	pluginsEnabledYAML = "plugins:\n  enabled: true\n  configs:\n    auto-baseline:\n      enabled: true\n"
	baseConfig         = "port: 8317\napi-keys: [\"k\"]\n" + pluginsEnabledYAML
)

type harnessOption func(*harness)

func withConfig(mutate func(*config.Config)) harnessOption {
	return func(h *harness) { mutate(&h.cfg) }
}

func withApply(apply ApplyFunc) harnessOption {
	return func(h *harness) { h.apply = apply }
}

func withMode(mode configfile.DeploymentMode) harnessOption {
	return func(h *harness) { h.mode = mode }
}

func withDryRunApply(apply DryRunApplyFunc) harnessOption {
	return func(h *harness) { h.dryRunApply = apply }
}

func withGoroutines() harnessOption {
	return func(h *harness) { h.async = func(f func()) { go f() } }
}

func newHarness(t *testing.T, opts ...harnessOption) *harness {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(baseConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Enabled = true
	cfg.ConfigPath = configPath
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.PromotionCooldown = 0
	cfg.MinDistinctSessions = 2
	h := &harness{t: t, config: configPath, clk: clock.NewFake(t0), logs: &logSink{}, cfg: cfg}
	h.async = func(f func()) { f() } // synchronous for deterministic tests
	for _, opt := range opts {
		opt(h)
	}
	h.eng = h.newEngine(h.cfg)
	return h
}

func (h *harness) newEngine(cfg config.Config) *Engine {
	return New(cfg, Deps{
		Clock:       h.clk,
		Log:         h.logs.log,
		RunAsync:    h.async,
		Apply:       h.apply,
		ApplyDryRun: h.dryRunApply,
		DetectMode:  func() configfile.DeploymentMode { return h.mode },
	})
}

func (h *harness) readConfig() string {
	h.t.Helper()
	raw, err := os.ReadFile(h.config)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(raw)
}

// waitWorkerIdle waits until no promotion worker is in flight. Timers hold
// WaitGroup slots while armed, so tests cannot use workers.Wait directly.
func (h *harness) waitWorkerIdle() {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.eng.mu.Lock()
		busy := h.eng.promotionInFlight
		h.eng.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatal("promotion worker did not become idle")
}

func (h *harness) backupPath() string { return configfile.BackupPath(h.cfg.EffectiveBackupDir()) }

func (h *harness) status() Snapshot { return h.eng.Status("auto-baseline", "0.1.0") }

func (h *harness) provider(p fingerprint.Provider) ProviderStatus {
	h.t.Helper()
	for _, ps := range h.status().Baselines {
		if ps.Provider == p {
			return ps
		}
	}
	h.t.Fatalf("provider %s missing from status", p)
	return ProviderStatus{}
}

func (h *harness) claude() ProviderStatus { return h.provider(fingerprint.ProviderClaude) }
func (h *harness) codex() ProviderStatus  { return h.provider(fingerprint.ProviderCodex) }

func (h *harness) observeClaude(sessions ...string) {
	for _, s := range sessions {
		h.eng.Observe(claudeHeaders(s))
	}
}

func claudeHeaders(session string) map[string][]string {
	h := map[string][]string{
		"User-Agent":                  {"claude-cli/2.1.258 (external, sdk-ts, agent-sdk/0.3.170)"},
		"X-App":                       {"cli"},
		"Anthropic-Version":           {"2023-06-01"},
		"Anthropic-Beta":              {"claude-code-20250219,oauth-2025-04-20"},
		"X-Stainless-Lang":            {"js"},
		"X-Stainless-Runtime":         {"node"},
		"X-Stainless-Package-Version": {"0.112.1"},
		"X-Stainless-Runtime-Version": {"v26.3.0"},
		"X-Stainless-Os":              {"Linux"},
		"X-Stainless-Arch":            {"x64"},
	}
	if session != "" {
		h["X-Claude-Code-Session-Id"] = []string{session}
	}
	return h
}

func claudeHeadersVersion(session, version, pkg string) map[string][]string {
	h := claudeHeaders(session)
	h["User-Agent"] = []string{"claude-cli/" + version + " (external, cli)"}
	h["X-Stainless-Package-Version"] = []string{pkg}
	return h
}

func codexHeaders(ua, session string) map[string][]string {
	return map[string][]string{
		"User-Agent": {ua},
		"Originator": {"codex-tui"},
		"Session_id": {session},
	}
}

const codexUA = "codex-tui/0.152.1 (Ubuntu 24.4.0; x86_64) WezTerm/20240203 (codex-tui; 0.152.1)"

// gatedApply blocks each Apply call until released and records calls.
type gatedApply struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
	inner   ApplyFunc
}

func newGatedApply(inner ApplyFunc) *gatedApply {
	return &gatedApply{entered: make(chan struct{}, 16), release: make(chan struct{}), inner: inner}
}

func (g *gatedApply) apply(path, backupDir string, c fingerprint.Candidate, check func(configfile.Snapshot) error) (configfile.Snapshot, error) {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	g.entered <- struct{}{}
	<-g.release
	return g.inner(path, backupDir, c, check)
}

func (g *gatedApply) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func (g *gatedApply) awaitEntry(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("apply was not entered")
	}
}

func TestStartReadsEffectiveBaselineAndLogs(t *testing.T) {
	h := newHarness(t)
	h.eng.Start()
	snap := h.status()
	if snap.Stopped || !snap.Enabled || snap.Faulted {
		t.Fatalf("status = stopped=%t enabled=%t faulted=%t", snap.Stopped, snap.Enabled, snap.Faulted)
	}
	if !snap.Config.Exists || !snap.Config.Writable || snap.Config.Error != "" || snap.Config.Path != h.config || snap.Config.Source != "config-path" || snap.Config.ModeUnsupported {
		t.Errorf("config status = %+v", snap.Config)
	}
	if !snap.Backup.Writable || snap.Backup.Dir != h.cfg.StateDir || snap.Backup.Error != "" {
		t.Errorf("backup status = %+v", snap.Backup)
	}
	claude, codex := h.claude(), h.codex()
	if claude.Effective.Version != fingerprint.CompiledClaudeBaselineVersion || claude.Effective.Explicit {
		t.Errorf("claude effective = %+v", claude.Effective)
	}
	if codex.Effective.Version != fingerprint.CompiledCodexBaselineVersion {
		t.Errorf("codex effective = %+v", codex.Effective)
	}
	if len(codex.Warnings) == 0 || !strings.Contains(codex.Warnings[0], "disable-codex-cloaking") {
		t.Errorf("codex cloaking warning missing: %v", codex.Warnings)
	}
	if !h.logs.contains("learning enabled") {
		t.Errorf("start not logged: %v", h.logs.lines)
	}
	if snap.CPAVersion != fingerprint.CompiledCPAVersion {
		t.Errorf("assumed CPA version = %q", snap.CPAVersion)
	}
}

func TestQuorumPromotesClaudeIntoConfig(t *testing.T) {
	h := newHarness(t)
	h.eng.Start()
	h.observeClaude("s1", "s1")
	if strings.Contains(h.readConfig(), "claude-header-defaults") {
		t.Fatal("promoted before quorum")
	}
	h.observeClaude("s2")
	text := h.readConfig()
	for _, want := range []string{
		"claude-header-defaults:",
		`user-agent: "claude-cli/2.1.258 (external, cli)"`,
		`package-version: "0.112.1"`,
		`runtime-version: "v26.3.0"`,
		"port: 8317",
		"api-keys: [\"k\"]",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("config missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "sdk-ts") || strings.Contains(text, "os:") || strings.Contains(text, "arch:") {
		t.Errorf("config carries fields the plugin must not write:\n%s", text)
	}
	if _, err := os.Stat(h.backupPath()); err != nil {
		t.Errorf("backup missing in state dir: %v", err)
	}
	if !h.logs.contains("promoted claude baseline 2.1.220 -> 2.1.258") {
		t.Errorf("promotion not logged: %v", h.logs.lines)
	}

	snap := h.status()
	claude := h.claude()
	if claude.Effective.Version.String() != "2.1.258" || !claude.Effective.Explicit {
		t.Errorf("effective after promotion = %+v", claude.Effective)
	}
	if claude.LastPromotion == nil {
		t.Fatal("last promotion missing")
	}
	if claude.LastPromotion.From.String() != "2.1.220" || claude.LastPromotion.Observations != 3 || claude.LastPromotion.DistinctSessions != 2 || claude.LastPromotion.Source != sourceObserved {
		t.Errorf("last promotion = %+v", claude.LastPromotion)
	}
	if len(claude.Pending) != 0 {
		t.Errorf("candidate still pending after promotion: %+v", claude.Pending)
	}
	if len(snap.History) != 1 {
		t.Errorf("history = %+v", snap.History)
	}
	if snap.Counters.Requests != 3 || snap.Counters.Accepted != 3 {
		t.Errorf("counters = %+v", snap.Counters)
	}

	st, err := statefile.Load(h.cfg.StateDir)
	if err != nil {
		t.Fatalf("state load: %v", err)
	}
	if len(st.History) != 1 || st.Baselines[fingerprint.ProviderClaude].Version.String() != "2.1.258" {
		t.Errorf("state = %+v", st)
	}

	// Same version observed again: not newer, never re-promoted.
	before := h.readConfig()
	h.observeClaude("s3", "s3", "s3", "s3")
	if h.readConfig() != before {
		t.Error("equal version was re-promoted")
	}
	if got := h.status().Counters.Decisions[learner.DecisionNotNewer]; got != 4 {
		t.Errorf("not-newer decisions = %d", got)
	}
}

func TestDefaultQuorumAcceptsAnonymousClients(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) { c.MinDistinctSessions = config.DefaultMinDistinctSessions }))
	h.eng.Start()
	h.observeClaude("", "")
	if strings.Contains(h.readConfig(), "2.1.258") {
		t.Fatal("promoted with two observations")
	}
	h.observeClaude("")
	if !strings.Contains(h.readConfig(), "2.1.258") {
		t.Errorf("anonymous observations did not reach the default quorum:\n%s", h.readConfig())
	}
	if lp := h.claude().LastPromotion; lp == nil || lp.Observations != 3 || lp.DistinctSessions != 0 {
		t.Errorf("last promotion = %+v", lp)
	}
}

func TestDryRunNeverWrites(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) { c.DryRun = true }))
	h.eng.Start()
	h.observeClaude("a", "b", "c")
	if h.readConfig() != baseConfig {
		t.Fatal("dry-run wrote config.yaml")
	}
	if _, err := os.Stat(h.backupPath()); !errors.Is(err, os.ErrNotExist) {
		t.Error("dry-run wrote a backup")
	}
	claude := h.claude()
	if claude.LastPromotion == nil || !claude.LastPromotion.DryRun || claude.LastPromotion.Candidate.Version.String() != "2.1.258" || claude.LastPromotion.AwaitingReload {
		t.Errorf("dry-run promotion record = %+v", claude.LastPromotion)
	}
	if !h.logs.contains("dry-run would promote claude baseline 2.1.220 -> 2.1.258") {
		t.Errorf("dry-run not logged: %v", h.logs.lines)
	}
	if len(claude.Pending) != 1 || !claude.Pending[0].QuorumMet {
		t.Errorf("pending = %+v", claude.Pending)
	}
	h.observeClaude("d")
	if len(h.status().History) != 1 {
		t.Error("dry-run history duplicated")
	}
	found := false
	for _, w := range h.status().Warnings {
		if strings.Contains(w, "dry-run") {
			found = true
		}
	}
	if !found {
		t.Errorf("dry-run warning missing: %v", h.status().Warnings)
	}
}

func TestCodexPromotionWritesFullUserAgent(t *testing.T) {
	h := newHarness(t)
	h.eng.Start()
	h.eng.Observe(codexHeaders(codexUA, "t1"))
	h.eng.Observe(codexHeaders(codexUA, "t2"))
	h.eng.Observe(codexHeaders(codexUA, "t1"))
	text := h.readConfig()
	if !strings.Contains(text, "codex-header-defaults:\n  user-agent: \""+codexUA+"\"") {
		t.Errorf("codex baseline not written:\n%s", text)
	}
	if strings.Contains(text, "beta-features") || strings.Contains(text, "disable-codex-cloaking") {
		t.Errorf("plugin touched keys it must not:\n%s", text)
	}
}

func TestCooldownDefersSecondWriteUntilTimer(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) { c.PromotionCooldown = time.Minute }))
	h.eng.Start()
	h.observeClaude("a", "b", "c")
	if !strings.Contains(h.readConfig(), "2.1.258") {
		t.Fatal("first promotion did not happen")
	}
	for _, s := range []string{"a", "b", "c"} {
		h.eng.Observe(claudeHeadersVersion(s, "2.1.270", "0.120.0"))
	}
	if strings.Contains(h.readConfig(), "2.1.270") {
		t.Fatal("second promotion ignored cooldown")
	}
	if h.claude().NextWriteAfter.IsZero() {
		t.Error("status does not show the cooldown deadline")
	}
	if len(h.clk.PendingAt()) == 0 {
		t.Fatal("no retry timer armed")
	}
	h.clk.Advance(time.Minute)
	if text := h.readConfig(); !strings.Contains(text, "2.1.270") || !strings.Contains(text, "0.120.0") {
		t.Errorf("promotion did not fire after cooldown:\n%s", text)
	}
}

func TestOnDiskRecheckSkipsWhenSomeoneElseRaisedBaseline(t *testing.T) {
	h := newHarness(t)
	h.eng.Start()
	h.observeClaude("a", "b")
	if err := os.WriteFile(h.config, []byte(baseConfig+"claude-header-defaults:\n  user-agent: \"claude-cli/2.1.300 (external, cli)\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.observeClaude("c")
	text := h.readConfig()
	if strings.Contains(text, "2.1.258") || !strings.Contains(text, "2.1.300") {
		t.Errorf("baseline downgraded:\n%s", text)
	}
	if !h.logs.contains("skipped claude candidate 2.1.258") {
		t.Errorf("skip not logged: %v", h.logs.lines)
	}
	claude := h.claude()
	if claude.Effective.Version.String() != "2.1.300" || len(claude.Pending) != 0 {
		t.Errorf("status after skip = %+v", claude)
	}
	if le := h.status().LastError; le != "" {
		t.Errorf("skip must not be an error: %q", le)
	}
}

func TestExplicitOldBaselineOnDiskIsRaised(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(h.config, []byte(pluginsEnabledYAML+"claude-header-defaults:\n  user-agent: \"claude-cli/2.1.230 (external, cli)\"\n  package-version: \"0.95.0\"\n  runtime-version: \"v26.3.0\"\n  os: \"Linux\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.eng.Start()
	if v := h.claude().Effective; v.Version.String() != "2.1.230" || !v.Explicit {
		t.Fatalf("effective = %+v", v)
	}
	h.observeClaude("a", "b", "c")
	if text := h.readConfig(); !strings.Contains(text, "2.1.258") || !strings.Contains(text, `os: "Linux"`) {
		t.Errorf("config:\n%s", text)
	}
}

func TestMalformedExplicitBaselineRefusesPromotion(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(h.config, []byte(pluginsEnabledYAML+"claude-header-defaults:\n  user-agent: \"my-proxy/1.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.eng.Start()
	claude := h.claude()
	if !claude.Effective.Malformed || len(claude.Warnings) == 0 || !strings.Contains(claude.Warnings[0], learner.DecisionBaselineMalformed) {
		t.Fatalf("malformed baseline not surfaced: %+v", claude)
	}
	h.observeClaude("a", "b", "c")
	if text := h.readConfig(); !strings.Contains(text, "my-proxy/1.0") || strings.Contains(text, "2.1.258") {
		t.Errorf("malformed explicit baseline was overwritten:\n%s", text)
	}
	if got := h.status().Counters.Decisions[learner.DecisionBaselineMalformed]; got != 3 {
		t.Errorf("baseline_malformed decisions = %d", got)
	}
	// Evidence is retained while blocked...
	if p := h.claude().Pending; len(p) != 1 || p[0].Observations != 3 {
		t.Fatalf("evidence pruned while blocked: %+v", p)
	}
	// ...and promotion resumes on it as soon as the file is repaired.
	if err := os.WriteFile(h.config, []byte(baseConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	h.eng.Reconfigure(h.cfg)
	if !strings.Contains(h.readConfig(), "2.1.258") {
		t.Errorf("promotion did not resume on retained evidence after repair:\n%s", h.readConfig())
	}
	// Codex is independent.
	h.eng.Observe(codexHeaders(codexUA, "t1"))
	h.eng.Observe(codexHeaders(codexUA, "t2"))
	h.eng.Observe(codexHeaders(codexUA, "t1"))
	if !strings.Contains(h.readConfig(), codexUA) {
		t.Error("codex blocked by the malformed claude baseline")
	}

	// Malformed value appearing on disk between observation and promotion
	// is caught by the pre-write check.
	h2 := newHarness(t)
	h2.eng.Start()
	h2.observeClaude("a", "b")
	if err := os.WriteFile(h2.config, []byte(pluginsEnabledYAML+"claude-header-defaults:\n  user-agent: \"garbage\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h2.observeClaude("c")
	if strings.Contains(h2.readConfig(), "2.1.258") {
		t.Error("pre-write check did not refuse the malformed baseline")
	}
	if le := h2.status().LastError; !strings.Contains(le, learner.DecisionBaselineMalformed) {
		t.Errorf("last error = %q", le)
	}
	if len(h2.claude().Pending) != 1 {
		t.Error("pre-write refusal pruned the evidence")
	}
}

func TestDuplicateKeyAndCycleBlockProviderWithoutCrash(t *testing.T) {
	for name, block := range map[string]string{
		"duplicate key": "claude-header-defaults:\n  package-version: \"0.1.0\"\n  package-version: \"0.2.0\"\n",
		"merge cycle":   "x: &x\n  <<: *x\nclaude-header-defaults:\n  <<: *x\n",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			content := pluginsEnabledYAML + block
			if err := os.WriteFile(h.config, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			h.eng.Start()
			claude := h.claude()
			if claude.Effective.Unsupported == "" || len(claude.Warnings) == 0 {
				t.Fatalf("block not surfaced: %+v", claude)
			}
			h.observeClaude("a", "b", "c")
			if h.readConfig() != content {
				t.Error("file modified despite unsupported block")
			}
			if got := h.status().Counters.Decisions[claude.Effective.Unsupported]; got != 3 {
				t.Errorf("decisions[%s] = %d", claude.Effective.Unsupported, got)
			}
			// Codex still works.
			h.eng.Observe(codexHeaders(codexUA, "t1"))
			h.eng.Observe(codexHeaders(codexUA, "t2"))
			h.eng.Observe(codexHeaders(codexUA, "t1"))
			if !strings.Contains(h.readConfig(), codexUA) {
				t.Error("codex blocked by a claude-only problem")
			}
		})
	}
}

func TestPluginDisabledOnDiskRefusesWrite(t *testing.T) {
	for name, content := range map[string]string{
		"plugins disabled":  "port: 1\nplugins:\n  enabled: false\n  configs:\n    auto-baseline:\n      enabled: true\n",
		"instance disabled": "port: 1\nplugins:\n  enabled: true\n  configs:\n    auto-baseline:\n      enabled: false\n",
		"no plugins block":  "port: 1\n",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			if err := os.WriteFile(h.config, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			h.eng.Start()
			h.observeClaude("a", "b", "c")
			if h.readConfig() != content {
				t.Error("wrote although the plugin is disabled on disk")
			}
			if got := h.status().Counters.Decisions[DecisionPluginDisabledOnDisk]; got != 1 {
				t.Errorf("plugin_disabled_on_disk decisions = %d", got)
			}
			if le := h.status().LastError; !strings.Contains(le, DecisionPluginDisabledOnDisk) {
				t.Errorf("last error = %q", le)
			}
			if len(h.claude().Pending) != 1 {
				t.Error("evidence dropped")
			}
			// Re-enabling on disk lets the retained evidence through.
			if err := os.WriteFile(h.config, []byte(baseConfig), 0o644); err != nil {
				t.Fatal(err)
			}
			h.eng.Reconfigure(h.cfg)
			if !strings.Contains(h.readConfig(), "2.1.258") {
				t.Error("promotion did not resume after re-enable")
			}
		})
	}
	// Dry-run still reports even when disabled on disk (nothing is written anyway).
	dry := newHarness(t, withConfig(func(c *config.Config) { c.DryRun = true }))
	if err := os.WriteFile(dry.config, []byte("port: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dry.eng.Start()
	dry.observeClaude("a", "b", "c")
	if lp := dry.claude().LastPromotion; lp == nil || !lp.DryRun {
		t.Errorf("dry-run record missing: %+v", lp)
	}
}

func TestRequireExplicitBaseline(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) { c.RequireExplicitBaseline = true }))
	h.eng.Start()
	if w := h.claude().Warnings; len(w) == 0 || !strings.Contains(w[0], DecisionBaselineImplicit) {
		t.Fatalf("implicit-baseline warning missing: %v", w)
	}
	h.observeClaude("a", "b", "c")
	if h.readConfig() != baseConfig {
		t.Error("implicit baseline was promoted despite require-explicit-baseline")
	}
	if got := h.status().Counters.Decisions[DecisionBaselineImplicit]; got != 1 {
		t.Errorf("baseline_implicit decisions = %d", got)
	}
	if len(h.claude().Pending) != 1 {
		t.Error("evidence dropped")
	}
	// An explicit (older) baseline on disk satisfies the requirement.
	if err := os.WriteFile(h.config, []byte(baseConfig+"claude-header-defaults:\n  user-agent: \"claude-cli/2.1.230 (external, cli)\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.eng.Reconfigure(h.cfg)
	if !strings.Contains(h.readConfig(), "2.1.258") {
		t.Errorf("explicit baseline not promoted:\n%s", h.readConfig())
	}
}

func TestMissingOrUnwritableConfigLearnsButDoesNotPromote(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) { c.ConfigPath = filepath.Join(c.StateDir, "..", "nope", "config.yaml") }))
	h.eng.Start()
	if snap := h.status(); snap.Config.Exists || !strings.Contains(snap.Config.Error, "does not exist") {
		t.Fatalf("config status = %+v", snap.Config)
	}
	h.observeClaude("a", "b", "c")
	claude := h.claude()
	if len(claude.Pending) != 1 || !claude.Pending[0].QuorumMet {
		t.Errorf("evidence lost: %+v", claude.Pending)
	}
	if le := h.status().LastError; !strings.Contains(le, "cannot promote claude 2.1.258") {
		t.Errorf("last error = %q", le)
	}

	t.Run("read-only config", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root bypasses file permissions")
		}
		h2 := newHarness(t)
		if err := os.Chmod(h2.config, 0o444); err != nil {
			t.Fatal(err)
		}
		h2.eng.Start()
		if s := h2.status(); s.Config.Writable || !strings.Contains(s.Config.Error, "not writable") {
			t.Errorf("read-only config status = %+v", s.Config)
		}
		h2.observeClaude("a", "b", "c")
		if h2.readConfig() != baseConfig {
			t.Error("read-only config was modified")
		}
	})
}

func TestUnwritableBackupDirBlocksPromotion(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, withConfig(func(c *config.Config) { c.BackupDir = filepath.Join(file, "backups") }))
	h.eng.Start()
	if b := h.status().Backup; b.Writable || b.Error == "" || b.Dir != h.cfg.BackupDir {
		t.Fatalf("backup status = %+v", b)
	}
	h.observeClaude("a", "b", "c")
	if h.readConfig() != baseConfig {
		t.Error("config written although the backup dir is unusable")
	}
	if le := h.status().LastError; !strings.Contains(le, "cannot promote") {
		t.Errorf("last error = %q", le)
	}
}

func TestUnsupportedDeploymentModeDisablesWrites(t *testing.T) {
	mode := configfile.DeploymentMode{Name: "home", Reason: "-home-jwt flag"}
	h := newHarness(t, withMode(mode), withConfig(func(c *config.Config) { c.ConfigPath = "" }))
	h.eng.Start()
	snap := h.status()
	if !snap.Config.ModeUnsupported || snap.Config.Mode != "home" || snap.Config.ModeReason != mode.Reason {
		t.Fatalf("mode status = %+v", snap.Config)
	}
	if !strings.Contains(strings.Join(snap.Warnings, "\n"), "home mode") {
		t.Errorf("warning missing: %v", snap.Warnings)
	}
	if h.logs.count("home mode") != 1 {
		t.Errorf("mode logged %d times, want 1", h.logs.count("home mode"))
	}
	h.eng.Reconfigure(h.cfg) // refreshes config again: still logged once
	if h.logs.count("home mode") != 1 {
		t.Errorf("mode re-logged on refresh: %d", h.logs.count("home mode"))
	}
	if b := h.eng.writeBlocked(); !strings.Contains(b, "home mode") {
		t.Errorf("writes not blocked: %q", b)
	}

	// An explicit config-path overrides the detection.
	h2 := newHarness(t, withMode(mode))
	h2.eng.Start()
	if s := h2.status(); s.Config.ModeUnsupported || s.Config.Mode != "home" {
		t.Errorf("explicit config-path should report the mode but not block: %+v", s.Config)
	}
	h2.observeClaude("a", "b", "c")
	if !strings.Contains(h2.readConfig(), "2.1.258") {
		t.Error("explicit config-path did not allow the write")
	}
}

func (e *Engine) writeBlocked() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.writeBlockedLocked()
}

func TestRejectionsAndIgnoresAreCounted(t *testing.T) {
	h := newHarness(t)
	h.eng.Start()
	h.eng.Observe(map[string][]string{"User-Agent": {"curl/8"}})
	h.eng.Observe(nil)
	bad := claudeHeaders("s")
	delete(bad, "X-App")
	h.eng.Observe(bad)
	h.eng.Observe(claudeHeadersVersion("s", "2.1.100", "0.90.0"))
	c := h.status().Counters
	if c.Requests != 4 || c.Ignored != 2 || c.Rejected != 1 || c.Accepted != 1 {
		t.Errorf("counters = %+v", c)
	}
	if c.RejectReasons[fingerprint.ReasonMissingXApp] != 1 || c.Decisions[learner.DecisionBelowFloor] != 1 {
		t.Errorf("buckets = %+v / %+v", c.RejectReasons, c.Decisions)
	}
}

func TestStateSurvivesRestartAndInvalidEntriesAreDropped(t *testing.T) {
	h := newHarness(t)
	h.eng.Start()
	h.observeClaude("a", "b")
	h.eng.Shutdown()

	// Inject a corrupt candidate next to the valid one.
	st, err := statefile.Load(h.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	bogus := fingerprint.Candidate{Provider: fingerprint.ProviderClaude, Version: fingerprint.MustParseVersion("2.1.259"), UserAgent: "claude-cli/2.1.259 (external, sdk-ts)", PackageVersion: "0.1.0", RuntimeVersion: "v1.0.0"}
	st.Pending = append(st.Pending, learner.Pending{Key: bogus.Key(), Candidate: bogus, Records: []learner.Record{{SessionID: "x", At: t0}}, FirstSeen: t0, LastSeen: t0})
	if err := statefile.Save(h.cfg.StateDir, st, t0); err != nil {
		t.Fatal(err)
	}

	eng2 := h.newEngine(h.cfg)
	eng2.Start()
	s := eng2.Status("x", "y")
	if !s.State.Loaded || s.State.RestoredDropped != 1 {
		t.Fatalf("state status = %+v", s.State)
	}
	if !h.logs.contains("dropped 1 restored candidate") {
		t.Errorf("drop not logged: %v", h.logs.lines)
	}
	eng2.Observe(claudeHeaders("a"))
	if !strings.Contains(h.readConfig(), "2.1.258") {
		t.Errorf("evidence did not survive restart:\n%s", h.readConfig())
	}
	if strings.Contains(h.readConfig(), "2.1.259") {
		t.Error("invalid restored candidate was promoted")
	}
}

func TestCorruptStateStartsFreshWithWarning(t *testing.T) {
	h := newHarness(t)
	if err := os.MkdirAll(h.cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statefile.Path(h.cfg.StateDir), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.eng.Start()
	if s := h.status(); s.State.Loaded || s.State.Error == "" {
		t.Errorf("state status = %+v", s.State)
	}
	if !h.logs.contains("state file unusable") {
		t.Errorf("warning not logged: %v", h.logs.lines)
	}
}

func TestPeriodicSaveTimer(t *testing.T) {
	h := newHarness(t)
	h.eng.Start()
	h.observeClaude("a")
	if _, err := os.Stat(statefile.Path(h.cfg.StateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("state saved before the periodic timer")
	}
	h.clk.Advance(stateSaveInterval)
	st, err := statefile.Load(h.cfg.StateDir)
	if err != nil {
		t.Fatalf("periodic save: %v", err)
	}
	if len(st.Pending) != 1 || st.Counters.Requests != 1 {
		t.Errorf("periodic save state = %+v", st)
	}
	if len(h.clk.PendingAt()) == 0 {
		t.Error("save timer not re-armed")
	}
	// After Stop the timer callback must not run any more.
	h.eng.Stop()
	h.clk.Advance(stateSaveInterval)
	if len(h.clk.PendingAt()) != 0 {
		t.Error("save timer re-armed after Stop")
	}
}

func TestSaveStateIsSingleFlightAndMonotonic(t *testing.T) {
	h := newHarness(t)
	h.eng.Start()
	h.observeClaude("a")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.eng.saveState(true)
		}()
	}
	wg.Wait()
	st, err := statefile.Load(h.cfg.StateDir)
	if err != nil {
		t.Fatalf("state after concurrent saves: %v", err)
	}
	if len(st.Pending) != 1 {
		t.Errorf("pending = %+v", st.Pending)
	}
	h.eng.mu.Lock()
	if h.eng.savedGen != h.eng.mutations {
		t.Errorf("savedGen %d != mutations %d", h.eng.savedGen, h.eng.mutations)
	}
	h.eng.mu.Unlock()
}

func TestStopAndReconfigure(t *testing.T) {
	h := newHarness(t)
	h.eng.Start()
	h.eng.Stop()
	if !h.status().Stopped {
		t.Fatal("not stopped")
	}
	h.observeClaude("a")
	if h.status().Counters.Requests != 0 {
		t.Error("stopped engine counted a request")
	}
	off := h.cfg
	off.Enabled = false
	h.eng.Reconfigure(off)
	if s := h.status(); !s.Stopped || s.Enabled {
		t.Errorf("status = %+v", s)
	}
	on := h.cfg
	on.MinObservations = 2
	on.MinDistinctSessions = 1
	h.eng.Reconfigure(on)
	if s := h.status(); s.Stopped || s.Rules.MinObservations != 2 {
		t.Errorf("status after re-enable = %+v", s)
	}
	h.observeClaude("a", "a")
	if !strings.Contains(h.readConfig(), "2.1.258") {
		t.Error("promotion did not happen with relaxed quorum")
	}
}

func TestStopIsAWriteBarrier(t *testing.T) {
	gate := newGatedApply(func(string, string, fingerprint.Candidate, func(configfile.Snapshot) error) (configfile.Snapshot, error) {
		// First attempt reports a concurrent change so the worker would
		// normally retry; Stop must prevent the retry.
		return configfile.Snapshot{}, configfile.ErrChanged
	})
	h := newHarness(t, withGoroutines(), withApply(gate.apply))
	h.eng.Start()
	h.observeClaude("a", "b", "c")
	gate.awaitEntry(t)

	stopped := make(chan struct{})
	go func() {
		h.eng.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while a promotion worker was inside apply")
	case <-time.After(50 * time.Millisecond):
	}
	close(gate.release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the worker finished")
	}
	if gate.count() != 1 {
		t.Errorf("apply called %d times; the retry after Stop must not happen", gate.count())
	}
	if !h.logs.contains("plugin stopped before the write") {
		t.Errorf("abort not logged: %v", h.logs.lines)
	}
	if le := h.status().LastError; le != "" {
		t.Errorf("a barrier abort is not an error: %q", le)
	}
	h.eng.mu.Lock()
	inFlight := h.eng.promotionInFlight
	h.eng.mu.Unlock()
	if inFlight {
		t.Error("promotionInFlight still set after Stop")
	}
}

func TestReadyCandidateDuringWorkerTriggersRescan(t *testing.T) {
	gate := newGatedApply(configfile.Apply)
	h := newHarness(t, withGoroutines(), withApply(gate.apply))
	h.eng.Start()
	h.observeClaude("a", "b", "c")
	gate.awaitEntry(t)
	// Codex reaches quorum while the claude write is in flight.
	h.eng.Observe(codexHeaders(codexUA, "t1"))
	h.eng.Observe(codexHeaders(codexUA, "t2"))
	h.eng.Observe(codexHeaders(codexUA, "t1"))
	h.eng.mu.Lock()
	rescan := h.eng.rescan
	h.eng.mu.Unlock()
	if !rescan {
		t.Fatal("rescan flag not set while a worker was running")
	}
	close(gate.release)
	h.waitWorkerIdle()
	text := h.readConfig()
	if !strings.Contains(text, "2.1.258") || !strings.Contains(text, codexUA) {
		t.Errorf("rescan lost the codex promotion:\n%s", text)
	}
	if gate.count() != 2 {
		t.Errorf("apply calls = %d, want 2", gate.count())
	}
}

func TestForcedPromotionIsQueuedWhileWorkerRuns(t *testing.T) {
	gate := newGatedApply(configfile.Apply)
	h := newHarness(t, withGoroutines(), withApply(gate.apply))
	h.eng.Start()
	h.observeClaude("a", "b", "c")
	gate.awaitEntry(t)
	out := h.eng.ReportObservation(Report{Provider: "codex", UserAgent: codexUA, Force: true})
	if !out.Accepted || !out.Queued || !strings.Contains(out.Reason, "queued") {
		t.Fatalf("outcome = %+v", out)
	}
	close(gate.release)
	h.waitWorkerIdle()
	if text := h.readConfig(); !strings.Contains(text, codexUA) {
		t.Errorf("queued forced promotion was lost:\n%s", text)
	}
	if lp := h.codex().LastPromotion; lp == nil || !lp.Forced || lp.Source != sourceManagement {
		t.Errorf("last promotion = %+v", lp)
	}
}

func TestWorkerPanicFaultsEngineUntilReconfigure(t *testing.T) {
	calls := 0
	h := newHarness(t, withApply(func(path, backupDir string, c fingerprint.Candidate, check func(configfile.Snapshot) error) (configfile.Snapshot, error) {
		calls++
		if calls == 1 {
			panic("boom in apply")
		}
		return configfile.Apply(path, backupDir, c, check)
	}))
	h.eng.Start()
	h.observeClaude("a", "b", "c") // must not crash the test process
	snap := h.status()
	if !snap.Faulted || !strings.Contains(snap.FaultReason, "boom in apply") || !strings.Contains(snap.LastError, "panic") {
		t.Fatalf("status = faulted=%t reason=%q err=%q", snap.Faulted, snap.FaultReason, snap.LastError)
	}
	h.eng.mu.Lock()
	inFlight := h.eng.promotionInFlight
	h.eng.mu.Unlock()
	if inFlight {
		t.Error("promotionInFlight stuck after panic")
	}
	if !h.logs.contains("writes suspended") {
		t.Errorf("panic not logged: %v", h.logs.lines)
	}
	// Further observations do not write while faulted.
	h.observeClaude("d")
	if h.readConfig() != baseConfig || calls != 1 {
		t.Errorf("write attempted while faulted (calls=%d)", calls)
	}
	// Reconfigure clears the fault and the pending candidate is promoted.
	h.eng.Reconfigure(h.cfg)
	if s := h.status(); s.Faulted {
		t.Error("fault not cleared by reconfigure")
	}
	if !strings.Contains(h.readConfig(), "2.1.258") {
		t.Errorf("promotion did not resume after reconfigure (calls=%d)", calls)
	}
	h.eng.Stop() // must not hang: WaitGroup was released despite the panic
}

func TestReconfigureWithChangedRulesClearsEvidence(t *testing.T) {
	h := newHarness(t)
	h.eng.Start()
	h.observeClaude("a", "b")
	if len(h.claude().Pending) != 1 {
		t.Fatal("precondition: pending candidate")
	}
	same := h.cfg
	same.PromotionCooldown = 5 * time.Second
	h.eng.Reconfigure(same)
	if len(h.claude().Pending) != 1 {
		t.Fatal("non-classifier change cleared evidence")
	}
	stricter := h.cfg
	stricter.ClaudeEntrypoints = []string{"cli"}
	h.eng.Reconfigure(stricter)
	if len(h.claude().Pending) != 0 {
		t.Error("evidence kept after classifier rules changed")
	}
	if !h.logs.contains("classifier rules changed") {
		t.Errorf("not logged: %v", h.logs.lines)
	}
	// sdk-ts is no longer allowed, so the same client is now rejected.
	h.observeClaude("c")
	if got := h.status().Counters.RejectReasons[fingerprint.ReasonEntrypointDenied]; got != 1 {
		t.Errorf("entrypoint rejections = %d", got)
	}
}

func TestPromotionAwaitsReloadConfirmation(t *testing.T) {
	h := newHarness(t)
	h.eng.Start()
	h.observeClaude("a", "b", "c")
	claude := h.claude()
	if !claude.AwaitingReload || claude.LastPromotion == nil || !claude.LastPromotion.AwaitingReload {
		t.Fatalf("promotion not marked awaiting reload: %+v", claude)
	}
	if len(claude.Warnings) != 0 {
		t.Errorf("premature warning: %v", claude.Warnings)
	}
	h.clk.Advance(reloadGracePeriod + time.Minute)
	if w := h.claude().Warnings; len(w) == 0 || !strings.Contains(w[0], "has not reloaded") {
		t.Errorf("no warning after the grace period: %v", w)
	}
	// CPA reconfigures plugins on every config reload: that confirms it.
	h.eng.Reconfigure(h.cfg)
	claude = h.claude()
	if claude.AwaitingReload || claude.LastPromotion.AwaitingReload || claude.LastPromotion.ConfirmedAt.IsZero() || len(claude.Warnings) != 0 {
		t.Errorf("reconfigure did not confirm: %+v", claude)
	}
	if h.status().History[0].AwaitingReload {
		t.Error("history entry not confirmed")
	}

	// Alternative confirmation: a restart reads the promoted value back.
	h2 := newHarness(t)
	h2.eng.Start()
	h2.observeClaude("a", "b", "c")
	h2.eng.Shutdown()
	eng2 := h2.newEngine(h2.cfg)
	eng2.Start()
	for _, ps := range eng2.Status("x", "y").Baselines {
		if ps.Provider == fingerprint.ProviderClaude && (ps.AwaitingReload || ps.LastPromotion == nil || ps.LastPromotion.AwaitingReload) {
			t.Errorf("fresh read did not confirm: %+v", ps)
		}
	}
}

func TestReconfigureUnmanagedProviderStopsLearningIt(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) { c.ManageClaude = false }))
	h.eng.Start()
	h.observeClaude("a", "b", "c")
	if h.readConfig() != baseConfig {
		t.Fatal("unmanaged provider was promoted")
	}
	if h.status().Counters.Decisions[learner.DecisionProviderUnmanaged] != 3 {
		t.Error("unmanaged decisions not counted")
	}
}

func TestResetClearsPending(t *testing.T) {
	h := newHarness(t)
	h.eng.Start()
	h.observeClaude("a")
	h.eng.Reset()
	if len(h.claude().Pending) != 0 {
		t.Error("pending not cleared")
	}
	if !h.logs.contains("cleared by operator") {
		t.Error("reset not logged")
	}
}

func TestReportObservationGoesThroughQuorum(t *testing.T) {
	h := newHarness(t)
	h.eng.Start()
	report := Report{Provider: "claude", UserAgent: "claude-cli/2.1.258 (external, cli)", PackageVersion: "0.112.1", RuntimeVersion: "v26.3.0", OS: "Linux", Arch: "x64", SessionID: "host-1"}
	out := h.eng.ReportObservation(report)
	if !out.Accepted || out.Decision != learner.DecisionTracked || out.Queued {
		t.Fatalf("outcome = %+v", out)
	}
	if h.readConfig() != baseConfig {
		t.Fatal("single report promoted")
	}
	report.SessionID = "host-2"
	h.eng.ReportObservation(report)
	out = h.eng.ReportObservation(report)
	if out.Decision != learner.DecisionQuorum || !strings.Contains(h.readConfig(), "2.1.258") {
		t.Errorf("outcome = %+v config:\n%s", out, h.readConfig())
	}
}

func TestReportObservationValidationAndForce(t *testing.T) {
	h := newHarness(t)
	h.eng.Start()
	if out := h.eng.ReportObservation(Report{Provider: "gemini"}); out.Accepted || !strings.Contains(out.Reason, "provider") {
		t.Errorf("bad provider = %+v", out)
	}
	if out := h.eng.ReportObservation(Report{Provider: "claude", UserAgent: "claude-cli/2.1.258 (external, cli)", PackageVersion: "bad"}); out.Accepted || out.Reason != fingerprint.ReasonPackageVersion {
		t.Errorf("bad package = %+v", out)
	}
	if out := h.eng.ReportObservation(Report{Provider: "claude", UserAgent: "claude-cli/2.1.258 (external, mcp)", PackageVersion: "0.1.0", RuntimeVersion: "v1.0.0", OS: "L", Arch: "x"}); out.Accepted || out.Reason != fingerprint.ReasonEntrypointDenied {
		t.Errorf("denied entrypoint = %+v", out)
	}
	out := h.eng.ReportObservation(Report{Provider: "claude", UserAgent: "claude-cli/2.1.100 (external, cli)", PackageVersion: "0.1.0", RuntimeVersion: "v1.0.0", OS: "L", Arch: "x", Force: true})
	if out.Queued || !strings.Contains(out.Reason, learner.DecisionBelowFloor) {
		t.Errorf("force below floor = %+v", out)
	}
	out = h.eng.ReportObservation(Report{Provider: "codex", UserAgent: "codex-tui/0.160.0 (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 (codex-tui; 0.160.0)", Force: true})
	if !out.Queued || !out.Accepted {
		t.Fatalf("force = %+v", out)
	}
	if !strings.Contains(h.readConfig(), "codex-tui/0.160.0") {
		t.Errorf("forced promotion missing:\n%s", h.readConfig())
	}
	if lp := h.codex().LastPromotion; lp == nil || !lp.Forced || lp.Source != sourceManagement {
		t.Errorf("last promotion = %+v", lp)
	}
	dry := newHarness(t, withConfig(func(c *config.Config) { c.DryRun = true }))
	dry.eng.Start()
	dry.eng.ReportObservation(Report{Provider: "codex", UserAgent: "codex-tui/0.160.0 (x)", Force: true})
	if dry.readConfig() != baseConfig {
		t.Error("forced dry-run wrote config")
	}
}

func TestApplyRetriesOnConcurrentChangeThenFails(t *testing.T) {
	calls := 0
	h := newHarness(t, withApply(func(string, string, fingerprint.Candidate, func(configfile.Snapshot) error) (configfile.Snapshot, error) {
		calls++
		return configfile.Snapshot{}, configfile.ErrChanged
	}))
	h.eng.Start()
	h.observeClaude("a", "b", "c")
	if calls != maxWriteAttempts {
		t.Errorf("apply calls = %d, want %d", calls, maxWriteAttempts)
	}
	if le := h.status().LastError; !strings.Contains(le, "changed during read-modify-write") {
		t.Errorf("last error = %q", le)
	}
	if len(h.claude().Pending) != 1 {
		t.Error("candidate dropped after transient failure")
	}
}

func TestApplyErrorIsSanitized(t *testing.T) {
	h := newHarness(t, withApply(func(string, string, fingerprint.Candidate, func(configfile.Snapshot) error) (configfile.Snapshot, error) {
		return configfile.Snapshot{}, errors.New("disk full Bearer abc.def.ghi")
	}))
	h.eng.Start()
	h.observeClaude("a", "b", "c")
	if le := h.status().LastError; !strings.Contains(le, "disk full") || strings.Contains(le, "abc.def.ghi") {
		t.Errorf("last error = %q", le)
	}
}

func TestObserveIsSafeConcurrently(t *testing.T) {
	h := newHarness(t, withGoroutines())
	h.eng.Start()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				h.eng.Observe(claudeHeaders([]string{"a", "b", "c"}[(i+j)%3]))
				_ = h.status()
			}
		}(i)
	}
	wg.Wait()
	h.eng.Shutdown()
	if !strings.Contains(h.readConfig(), "2.1.258") {
		t.Error("promotion missing after concurrent observations")
	}
}

func TestStateDirChangeMigratesState(t *testing.T) {
	h := newHarness(t)
	h.eng.Start()
	h.observeClaude("a", "b")
	newDir := filepath.Join(filepath.Dir(h.cfg.StateDir), "state2")
	// Pre-seed the new dir with evidence from "another" run so the merge is
	// visible: one observation from session c.
	seed := h.newEngine(func() config.Config { c := h.cfg; c.StateDir = newDir; return c }())
	seed.Start()
	seed.Observe(claudeHeaders("c"))
	seed.Shutdown()

	moved := h.cfg
	moved.StateDir = newDir
	h.eng.Reconfigure(moved)
	// Old dir was flushed with the two live observations.
	old, err := statefile.Load(h.cfg.StateDir)
	if err != nil || len(old.Pending) != 1 || len(old.Pending[0].Records) != 2 {
		t.Fatalf("old dir not flushed: %+v err=%v", old, err)
	}
	if !h.logs.contains("state dir changed") {
		t.Errorf("migration not logged: %v", h.logs.lines)
	}
	// The new dir's evidence (session c) merged with the live evidence
	// (sessions a, b) meets quorum, so Reconfigure's kick promoted right away
	// with 3 observations from 3 distinct sessions, and the save landed in
	// the NEW dir.
	if !strings.Contains(h.readConfig(), "2.1.258") {
		t.Error("promotion missing after migration")
	}
	if lp := h.claude().LastPromotion; lp == nil || lp.Observations != 3 || lp.DistinctSessions != 3 {
		t.Fatalf("merged evidence not reflected in the promotion: %+v", lp)
	}
	st, err := statefile.Load(newDir)
	if err != nil || len(st.History) != 1 {
		t.Errorf("new dir not used for saves: %+v err=%v", st, err)
	}
	if s := h.status(); s.State.Dir != newDir || s.Backup.Dir != newDir {
		t.Errorf("status dirs = %s / %s", s.State.Dir, s.Backup.Dir)
	}
}

func TestPanicUnderLockIsRecovered(t *testing.T) {
	h := newHarness(t, withGoroutines())
	h.eng.postWriteHook = func() { panic("boom under mu") }
	h.eng.Start()
	h.observeClaude("a", "b", "c")
	h.waitWorkerIdle()
	done := make(chan struct{})
	go func() {
		h.eng.Stop() // must not deadlock on a poisoned mutex
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop hung after a panic under the lock")
	}
	snap := h.status()
	if !snap.Faulted || !strings.Contains(snap.FaultReason, "boom under mu") || !strings.Contains(snap.LastError, "boom under mu") {
		t.Errorf("fault not recorded: faulted=%t reason=%q err=%q", snap.Faulted, snap.FaultReason, snap.LastError)
	}
	h.eng.mu.Lock()
	inFlight := h.eng.promotionInFlight
	h.eng.mu.Unlock()
	if inFlight {
		t.Error("promotionInFlight still set")
	}
	// The write itself completed before the hook fired.
	if !strings.Contains(h.readConfig(), "2.1.258") {
		t.Error("write missing")
	}
}

func TestForcedQueueKeepsHighestAndReplaysAfterStop(t *testing.T) {
	gate := newGatedApply(configfile.Apply)
	h := newHarness(t, withGoroutines(), withApply(gate.apply))
	h.eng.Start()
	h.observeClaude("a", "b", "c") // occupies the worker
	gate.awaitEntry(t)
	lo := "codex-tui/0.150.0 (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 (codex-tui; 0.150.0)"
	hi := "codex-tui/0.160.0 (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 (codex-tui; 0.160.0)"
	hiTie := "codex-tui/0.160.0 (Ubuntu 24.4.0; x86_64) WezTerm/1 (codex-tui; 0.160.0)"
	for _, ua := range []string{lo, hi, hiTie} {
		if out := h.eng.ReportObservation(Report{Provider: "codex", UserAgent: ua, Force: true}); !out.Queued {
			t.Fatalf("%s not queued: %+v", ua, out)
		}
	}
	h.eng.mu.Lock()
	queued := h.eng.forcedQueue[fingerprint.ProviderCodex].UserAgent
	h.eng.mu.Unlock()
	if queued != hi {
		t.Fatalf("queued = %q, want highest version with first-wins tie (%q)", queued, hi)
	}
	// Stop while the claude write is in flight: the forced entry must survive.
	stopped := make(chan struct{})
	go func() { h.eng.Stop(); close(stopped) }()
	close(gate.release)
	<-stopped
	h.eng.mu.Lock()
	_, still := h.eng.forcedQueue[fingerprint.ProviderCodex]
	h.eng.mu.Unlock()
	if !still {
		t.Fatal("forced entry dropped by Stop")
	}
	if strings.Contains(h.readConfig(), "codex-tui") {
		t.Fatal("forced entry written after Stop")
	}
	// Re-enabling replays it.
	h.eng.Reconfigure(h.cfg)
	h.waitWorkerIdle()
	if !strings.Contains(h.readConfig(), hi) {
		t.Errorf("forced entry not replayed after reconfigure:\n%s", h.readConfig())
	}
	h.eng.mu.Lock()
	_, still = h.eng.forcedQueue[fingerprint.ProviderCodex]
	h.eng.mu.Unlock()
	if still {
		t.Error("forced entry not dequeued after a terminal outcome")
	}
}

func TestReloadConfirmationIsValueCorrelated(t *testing.T) {
	h := newHarness(t)
	h.eng.Start()
	h.observeClaude("a", "b", "c")
	if !h.claude().AwaitingReload {
		t.Fatal("precondition: awaiting reload")
	}
	// Someone edits the file to a DIFFERENT value before CPA reloads: the
	// reconfigure must not confirm our promotion.
	if err := os.WriteFile(h.config, []byte(baseConfig+"claude-header-defaults:\n  user-agent: \"claude-cli/2.1.258 (external, cli)\"\n  package-version: \"9.9.9\"\n  runtime-version: \"v26.3.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.eng.Reconfigure(h.cfg)
	if c := h.claude(); !c.AwaitingReload {
		t.Errorf("confirmed although the on-disk tuple differs: %+v", c.LastPromotion)
	}
	// Restoring the exact tuple confirms it on the next read.
	if err := os.WriteFile(h.config, []byte(baseConfig+"claude-header-defaults:\n  user-agent: \"claude-cli/2.1.258 (external, cli)\"\n  package-version: \"0.112.1\"\n  runtime-version: \"v26.3.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.eng.Reconfigure(h.cfg)
	if c := h.claude(); c.AwaitingReload || c.LastPromotion.ConfirmedAt.IsZero() {
		t.Errorf("exact tuple not confirmed: %+v", c.LastPromotion)
	}
}

func TestReconfigureWriteChangeDrainsAndGenerationAbortsStaleAttempt(t *testing.T) {
	gate := newGatedApply(configfile.Apply)
	h := newHarness(t, withGoroutines(), withApply(gate.apply))
	h.eng.Start()
	h.observeClaude("a", "b", "c")
	gate.awaitEntry(t) // attempt planned under generation 0 is inside apply
	done := make(chan struct{})
	go func() {
		dry := h.cfg
		dry.DryRun = true // write-affecting change
		h.eng.Reconfigure(dry)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Reconfigure returned while a write was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(gate.release)
	<-done
	if !h.status().DryRun {
		t.Fatal("new config not applied")
	}
	// The in-flight write completed (it was already past the barrier); no
	// further attempt may write under the new dry-run config.
	before := h.readConfig()
	for _, s := range []string{"a", "b", "c"} {
		h.eng.Observe(claudeHeadersVersion(s, "2.1.270", "0.120.0"))
	}
	h.waitWorkerIdle()
	if h.readConfig() != before {
		t.Error("write happened under dry-run after reconfigure")
	}
	if gate.count() != 1 {
		t.Errorf("apply calls = %d, want 1", gate.count())
	}

	// Generation check in isolation: bump the generation between planning
	// and apply and assert the attempt is abandoned, not written.
	h2 := newHarness(t)
	h2.eng.apply = func(path, backupDir string, c fingerprint.Candidate, check func(configfile.Snapshot) error) (configfile.Snapshot, error) {
		t.Fatal("apply must not run for a stale generation")
		return configfile.Snapshot{}, nil
	}
	h2.eng.Start()
	h2.eng.mu.Lock()
	h2.eng.configGen++ // simulates a reconfigure racing the planned attempt
	gen := h2.eng.configGen - 1
	h2.eng.mu.Unlock()
	if err := h2.eng.preWriteAbort(gen, fingerprint.ProviderClaude, fingerprint.Candidate{}); !errors.Is(err, errGeneration) {
		t.Errorf("preWriteAbort = %v, want errGeneration", err)
	}
}

func TestSetDryRunTogglesConfigAndAwaitsReload(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) { c.DryRun = true }))
	if err := os.WriteFile(h.config, []byte("port: 1\nplugins:\n  enabled: true\n  configs:\n    auto-baseline:\n      enabled: true\n      dry-run: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.eng.Start()
	out := h.eng.SetDryRun(false)
	if out.Status != 200 || out.Result != "ok" || !out.AwaitingReload || out.DryRun {
		t.Fatalf("outcome = %+v", out)
	}
	if text := h.readConfig(); !strings.Contains(text, "dry-run: false") {
		t.Errorf("config not updated:\n%s", text)
	}
	if _, err := os.Stat(h.backupPath()); err != nil {
		t.Errorf("backup missing: %v", err)
	}
	snap := h.status()
	if !snap.DryRun || !snap.DryRunAwaitingReload || snap.DryRunTarget {
		t.Errorf("status = dry_run=%t awaiting=%t target=%t", snap.DryRun, snap.DryRunAwaitingReload, snap.DryRunTarget)
	}
	if !h.logs.contains("dry-run set to false") {
		t.Errorf("not logged: %v", h.logs.lines)
	}
	// Same value again while pending is still a write attempt (no-op on
	// disk); a genuinely equal, non-pending value is "unchanged".
	h.clk.Advance(reloadGracePeriod + time.Minute)
	if w := h.status().Warnings; len(w) == 0 || !strings.Contains(strings.Join(w, "\n"), "dry-run was set to false") {
		t.Errorf("no grace warning: %v", w)
	}
	// CPA reloads: reconfigure with the new value confirms the toggle.
	live := h.cfg
	live.DryRun = false
	h.eng.Reconfigure(live)
	snap = h.status()
	if snap.DryRun || snap.DryRunAwaitingReload {
		t.Errorf("not confirmed: dry_run=%t awaiting=%t", snap.DryRun, snap.DryRunAwaitingReload)
	}
	if out := h.eng.SetDryRun(false); out.Status != 200 || out.Result != "unchanged" {
		t.Errorf("unchanged outcome = %+v", out)
	}
	// Switching back to dry-run works the same way.
	if out := h.eng.SetDryRun(true); out.Status != 200 || !out.AwaitingReload {
		t.Errorf("re-enable = %+v", out)
	}
	if !strings.Contains(h.readConfig(), "dry-run: true") {
		t.Error("config not switched back")
	}
}

func TestSetDryRunRefusals(t *testing.T) {
	// 409 while a promotion write is in flight.
	gate := newGatedApply(configfile.Apply)
	h := newHarness(t, withGoroutines(), withApply(gate.apply))
	h.eng.Start()
	h.observeClaude("a", "b", "c")
	gate.awaitEntry(t)
	if out := h.eng.SetDryRun(true); out.Status != 409 {
		t.Errorf("in-flight outcome = %+v", out)
	}
	close(gate.release)
	h.waitWorkerIdle()

	// 503 in an unsupported deployment mode.
	h2 := newHarness(t, withMode(configfile.DeploymentMode{Name: "home", Reason: "-home-jwt flag"}), withConfig(func(c *config.Config) { c.ConfigPath = "" }))
	h2.eng.Start()
	if out := h2.eng.SetDryRun(true); out.Status != 503 || !strings.Contains(out.Detail, "home mode") {
		t.Errorf("mode outcome = %+v", out)
	}

	// 422 when the plugin subtree is missing: never created.
	h3 := newHarness(t)
	if err := os.WriteFile(h3.config, []byte("port: 1\nplugins:\n  enabled: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h3.eng.Start()
	out := h3.eng.SetDryRun(true)
	if out.Status != 422 || !strings.Contains(out.Detail, "plugin_subtree_missing") {
		t.Errorf("missing subtree outcome = %+v", out)
	}
	if got := h3.readConfig(); got != "port: 1\nplugins:\n  enabled: true\n" {
		t.Errorf("subtree was created:\n%s", got)
	}
	if le := h3.status().LastError; !strings.Contains(le, "plugin_subtree_missing") {
		t.Errorf("last error = %q", le)
	}

	// 503 when stopped.
	h4 := newHarness(t)
	h4.eng.Start()
	h4.eng.Stop()
	if out := h4.eng.SetDryRun(true); out.Status != 503 {
		t.Errorf("stopped outcome = %+v", out)
	}

	// 409 when the plugin is disabled on disk (the same rule promotions use).
	h5 := newHarness(t)
	if err := os.WriteFile(h5.config, []byte("plugins:\n  enabled: false\n  configs:\n    auto-baseline:\n      enabled: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h5.eng.Start()
	if out := h5.eng.SetDryRun(true); out.Status != 409 || !strings.Contains(out.Detail, DecisionPluginDisabledOnDisk) {
		t.Errorf("disabled-on-disk outcome = %+v", out)
	}
}

func TestPendingDryRunSuspendsWritesImmediately(t *testing.T) {
	// Runtime is live; the operator switches to dry-run. Before CPA reloads,
	// a quorum must NOT write baselines.
	h := newHarness(t)
	if err := os.WriteFile(h.config, []byte(baseConfig+"      dry-run: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.eng.Start()
	if out := h.eng.SetDryRun(true); out.Status != 200 || !out.AwaitingReload || !out.DryRun {
		t.Fatalf("outcome = %+v", out)
	}
	h.observeClaude("a", "b", "c")
	if strings.Contains(h.readConfig(), "claude-header-defaults") {
		t.Fatalf("baseline written while a switch to dry-run was pending:\n%s", h.readConfig())
	}
	if lp := h.claude().LastPromotion; lp == nil || !lp.DryRun {
		t.Errorf("expected a dry-run record, got %+v", lp)
	}
	// A forced promotion is also held to dry-run.
	h.eng.ReportObservation(Report{Provider: "codex", UserAgent: codexUA, Force: true})
	if strings.Contains(h.readConfig(), "codex-header-defaults") {
		t.Error("forced promotion wrote while dry-run was pending")
	}

	// The other direction waits for reconfigure: runtime dry-run, operator
	// switches to live; quorum before the reload must still not write.
	h2 := newHarness(t, withConfig(func(c *config.Config) { c.DryRun = true }))
	if err := os.WriteFile(h2.config, []byte(baseConfig+"      dry-run: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h2.eng.Start()
	if out := h2.eng.SetDryRun(false); out.Status != 200 || !out.AwaitingReload {
		t.Fatalf("outcome = %+v", out)
	}
	h2.observeClaude("a", "b", "c")
	if strings.Contains(h2.readConfig(), "claude-header-defaults") {
		t.Fatal("baseline written before CPA applied the switch to live writes")
	}
	live := h2.cfg
	live.DryRun = false
	h2.eng.Reconfigure(live)
	if !strings.Contains(h2.readConfig(), "claude-header-defaults") {
		t.Error("promotion did not proceed after the reload applied live writes")
	}
}

func TestSetDryRunHonoursWorkArrivingDuringToggle(t *testing.T) {
	// Runtime is dry-run; the operator switches to live. While the toggle
	// holds the worker slot, three observations reach quorum and set rescan;
	// releasing the slot must launch a real worker. The write itself is a
	// dry-run record here (the reload has not applied live writes yet), which
	// is exactly what proves the worker ran.
	h := newHarness(t, withConfig(func(c *config.Config) { c.DryRun = true }))
	h.dryRunApply = func(path, backupDir string, enabled bool, check func(configfile.Snapshot) error) (configfile.Snapshot, bool, error) {
		h.observeClaude("a", "b", "c")
		h.eng.mu.Lock()
		rescan := h.eng.rescan
		h.eng.mu.Unlock()
		if !rescan {
			t.Error("observations during the toggle did not set rescan")
		}
		return configfile.ApplyDryRun(path, backupDir, enabled, check)
	}
	h.eng = h.newEngine(h.cfg)
	if err := os.WriteFile(h.config, []byte(baseConfig+"      dry-run: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.eng.Start()
	if out := h.eng.SetDryRun(false); out.Status != 200 {
		t.Fatalf("outcome = %+v", out)
	}
	if lp := h.claude().LastPromotion; lp == nil || !lp.DryRun || lp.Observations != 3 {
		t.Errorf("quorum reached during the toggle was lost: %+v", lp)
	}
	// Same for a force queued during the toggle.
	var h3 *harness
	h3 = newHarness(t, withConfig(func(c *config.Config) { c.DryRun = true }), withDryRunApply(func(path, backupDir string, enabled bool, check func(configfile.Snapshot) error) (configfile.Snapshot, bool, error) {
		h3.eng.ReportObservation(Report{Provider: "codex", UserAgent: codexUA, Force: true})
		return configfile.ApplyDryRun(path, backupDir, enabled, check)
	}))
	if err := os.WriteFile(h3.config, []byte(baseConfig+"      dry-run: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h3.eng.Start()
	h3.eng.SetDryRun(false)
	if lp := h3.codex().LastPromotion; lp == nil || !lp.Forced || !lp.DryRun {
		t.Errorf("force queued during the toggle was lost: %+v", lp)
	}
}

func TestSetDryRunNoopDoesNotArmReloadMarker(t *testing.T) {
	// Disk already says dry-run: true while the runtime is still live (CPA
	// has not reloaded yet, or the file was edited by hand): writing again
	// would be a no-op CPA never reloads, so no marker may be armed.
	h := newHarness(t)
	if err := os.WriteFile(h.config, []byte(baseConfig+"      dry-run: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.eng.Start()
	before := h.readConfig()
	out := h.eng.SetDryRun(true)
	if out.Status != 200 || out.Result != "unchanged" || out.AwaitingReload {
		t.Fatalf("outcome = %+v", out)
	}
	if h.readConfig() != before {
		t.Error("no-op rewrote the file")
	}
	if s := h.status(); s.DryRunAwaitingReload {
		t.Error("marker armed for a no-op")
	}
	if _, err := os.Stat(h.backupPath()); !errors.Is(err, os.ErrNotExist) {
		t.Error("backup written for a no-op")
	}
	// A pending marker's clock is not reset by a repeated no-op request.
	h2 := newHarness(t)
	if err := os.WriteFile(h2.config, []byte(baseConfig+"      dry-run: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h2.eng.Start()
	if out := h2.eng.SetDryRun(true); out.Result != "ok" {
		t.Fatalf("first toggle = %+v", out)
	}
	h2.eng.mu.Lock()
	written := h2.eng.dryRunWrittenAt
	h2.eng.mu.Unlock()
	h2.clk.Advance(time.Minute)
	if out := h2.eng.SetDryRun(true); out.Result != "unchanged" || !out.AwaitingReload {
		t.Fatalf("repeat = %+v", out)
	}
	h2.eng.mu.Lock()
	defer h2.eng.mu.Unlock()
	if !h2.eng.dryRunWrittenAt.Equal(written) || !h2.eng.dryRunPending {
		t.Error("repeat request reset the pending marker clock")
	}
}
