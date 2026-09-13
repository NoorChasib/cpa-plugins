// Package engine wires the pure pieces together: it classifies intercepted
// requests, feeds the learner, promotes quorum candidates into config.yaml
// from a background worker, persists state, and publishes a status snapshot.
//
// Hot-path contract: Observe is called synchronously from the host's
// request.intercept_before RPC for every model request. It performs bounded
// header parsing and bounded bookkeeping under one mutex, never touches
// disk, never calls back into the host, and never blocks on the promotion
// worker.
//
// Concurrency contract: every plugin-owned goroutine and timer callback is
// registered in the workers WaitGroup and recovers panics into a faulted
// state, so Stop is a write barrier (no config write can start or finish
// after it returns) and a bug in the plugin can never take CPA down.
package engine

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/auto-baseline/internal/clock"
	"github.com/NoorChasib/cpa-plugins/plugins/auto-baseline/internal/config"
	"github.com/NoorChasib/cpa-plugins/plugins/auto-baseline/internal/configfile"
	"github.com/NoorChasib/cpa-plugins/plugins/auto-baseline/internal/fingerprint"
	"github.com/NoorChasib/cpa-plugins/plugins/auto-baseline/internal/learner"
	"github.com/NoorChasib/cpa-plugins/plugins/auto-baseline/internal/sanitize"
	"github.com/NoorChasib/cpa-plugins/plugins/auto-baseline/internal/statefile"
)

// Operational constants.
const (
	// stateSaveInterval bounds how long accepted observations stay unsaved.
	stateSaveInterval = 5 * time.Minute
	// maxWriteAttempts bounds read-modify-write retries when another writer
	// changes config.yaml between the plugin's read and write.
	maxWriteAttempts = 3
	// reloadGracePeriod is how long a written promotion may stay unconfirmed
	// before status warns that CPA did not reload the file.
	reloadGracePeriod = 2 * time.Minute

	sourceObserved   = "observed"
	sourceManagement = "management"
)

// providers lists the managed providers in status order.
var providers = [...]fingerprint.Provider{fingerprint.ProviderClaude, fingerprint.ProviderCodex}

// ApplyFunc is the config.yaml read-modify-write seam (tests inject fakes).
type ApplyFunc func(path, backupDir string, candidate fingerprint.Candidate, check func(configfile.Snapshot) error) (configfile.Snapshot, error)

// ReadFunc is the config.yaml read seam.
type ReadFunc func(path string) (configfile.Snapshot, error)

// DryRunApplyFunc is the config.yaml dry-run edit seam. changed reports
// whether the file was rewritten.
type DryRunApplyFunc func(path, backupDir string, enabled bool, check func(configfile.Snapshot) error) (configfile.Snapshot, bool, error)

// Deps are the engine's injected collaborators.
type Deps struct {
	Clock    clock.Clock
	Log      func(level, message string)
	RunAsync func(func())
	// Apply / ApplyDryRun / Read / DetectMode default to the configfile package.
	Apply       ApplyFunc
	ApplyDryRun DryRunApplyFunc
	Read        ReadFunc
	DetectMode  func() configfile.DeploymentMode
}

// Engine is the plugin's stateful core.
type Engine struct {
	mu       sync.Mutex
	cfg      config.Config
	rules    fingerprint.Rules
	clk      clock.Clock
	log      func(level, message string)
	runAsync func(func())
	apply    ApplyFunc
	read     ReadFunc
	detect   func() configfile.DeploymentMode

	learner *learner.Learner
	state   *statefile.State

	started bool
	stopped bool
	// configGen increments on every Reconfigure; a promotion attempt stamps
	// the generation it planned under and aborts if it changed before apply.
	configGen uint64

	// faultMu guards the fault fields. It is separate from mu so that
	// recoverWorker can record a panic that happened while mu was held
	// without deadlocking; every mu critical section uses defer Unlock so the
	// panic releases mu on its way out.
	faultMu     sync.Mutex
	faulted     bool
	faultReason string

	// mutations counts state changes; savedGen is the generation last
	// committed to disk. saveMu serializes saves (single flight).
	mutations uint64
	savedGen  uint64
	saveMu    sync.Mutex

	configPath      string
	configSource    string
	configExists    bool
	configWritable  bool
	configError     string
	configReadAt    time.Time
	configHash      string
	backupDir       string
	backupWritable  bool
	backupError     string
	mode            configfile.DeploymentMode
	modeUnsupported bool
	modeLogged      bool
	effective       map[fingerprint.Provider]configfile.Effective
	disableCodex    bool
	// awaitingReload records, per provider, when a written promotion is
	// still waiting for CPA's hot reload to be observed.
	awaitingReload map[fingerprint.Provider]time.Time
	// dryRunPending is set after the dry-run toggle wrote config.yaml and
	// until a reconfigure delivers the new value (or reverts it).
	dryRunPending   bool
	dryRunTarget    bool
	dryRunWrittenAt time.Time
	// applyDryRun is the config.yaml dry-run edit seam (tests inject fakes).
	applyDryRun DryRunApplyFunc

	stateLoaded     bool
	stateError      string
	stateSaveAt     time.Time
	restoredDropped int

	saveTimer  clock.Timer
	retryTimer clock.Timer

	promotionInFlight bool
	rescan            bool
	// forcedQueue holds at most one forced candidate per provider (the
	// highest version wins; ties keep the first). An entry stays queued
	// until its attempt reaches a terminal outcome; an attempt aborted by the
	// write barrier re-queues it so an enabling reconfigure replays it.
	forcedQueue map[fingerprint.Provider]fingerprint.Candidate
	workers     sync.WaitGroup

	// postWriteHook runs inside the post-write critical section; tests use
	// it to inject a panic while mu is held.
	postWriteHook func()
}

// New builds an Engine. Call Start to load state and begin learning.
func New(cfg config.Config, deps Deps) *Engine {
	if deps.Clock == nil {
		deps.Clock = clock.Real{}
	}
	if deps.Log == nil {
		deps.Log = func(string, string) {}
	}
	if deps.RunAsync == nil {
		deps.RunAsync = func(f func()) { go f() }
	}
	if deps.Apply == nil {
		deps.Apply = configfile.Apply
	}
	if deps.Read == nil {
		deps.Read = configfile.Read
	}
	if deps.ApplyDryRun == nil {
		deps.ApplyDryRun = configfile.ApplyDryRun
	}
	if deps.DetectMode == nil {
		deps.DetectMode = configfile.DetectDeploymentMode
	}
	e := &Engine{
		cfg:            cfg,
		rules:          cfg.Rules(),
		clk:            deps.Clock,
		log:            deps.Log,
		runAsync:       deps.RunAsync,
		apply:          deps.Apply,
		applyDryRun:    deps.ApplyDryRun,
		read:           deps.Read,
		detect:         deps.DetectMode,
		state:          statefile.New(),
		effective:      make(map[fingerprint.Provider]configfile.Effective),
		awaitingReload: make(map[fingerprint.Provider]time.Time),
		forcedQueue:    make(map[fingerprint.Provider]fingerprint.Candidate),
	}
	e.learner = learner.New(settingsFor(cfg), managedFor(cfg), e.clk.Now)
	return e
}

func settingsFor(cfg config.Config) learner.Settings {
	return learner.Settings{
		MinObservations:     cfg.MinObservations,
		MinDistinctSessions: cfg.MinDistinctSessions,
		ObservationWindow:   cfg.ObservationWindow,
		MinVersion: map[fingerprint.Provider]fingerprint.Version{
			fingerprint.ProviderClaude: cfg.ClaudeMinVersion,
			fingerprint.ProviderCodex:  cfg.CodexMinVersion,
		},
		Rules: cfg.Rules(),
	}
}

func managedFor(cfg config.Config) []fingerprint.Provider {
	var out []fingerprint.Provider
	for _, p := range providers {
		if cfg.Manages(p) {
			out = append(out, p)
		}
	}
	return out
}

// Start loads persisted state, discovers config.yaml, reads the effective
// baselines, and arms the periodic save. It is idempotent.
func (e *Engine) Start() {
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return
	}
	e.started = true
	e.stopped = false
	stateDir := e.cfg.StateDir
	e.mu.Unlock()

	e.loadState(stateDir)
	e.refreshConfig()

	e.mu.Lock()
	dropped := e.importPendingLocked()
	e.armSaveTimerLocked()
	dryRun, path, source := e.cfg.DryRun, e.configPath, e.configSource
	e.mu.Unlock()
	if dropped > 0 {
		e.log("warn", fmt.Sprintf("auto-baseline: dropped %d restored candidate(s) that failed validation", dropped))
	}
	e.log("info", fmt.Sprintf("auto-baseline started: learning enabled (dry-run=%t, config=%s via %s)", dryRun, path, source))
	e.kickPromotion()
}

// loadState reads the state file from dir into the engine and restores the
// awaiting-reload markers before the first config read, so a read that
// already shows the promoted value confirms them.
func (e *Engine) loadState(dir string) {
	st, err := statefile.Load(dir)
	e.mu.Lock()
	e.state = st
	e.stateLoaded = err == nil
	e.stateError = ""
	if err != nil {
		e.stateError = sanitize.Error(err)
	}
	e.savedGen = e.mutations
	e.awaitingReload = make(map[fingerprint.Provider]time.Time)
	for p, lp := range e.state.LastPromotion {
		if lp != nil && lp.AwaitingReload {
			e.awaitingReload[p] = lp.At
		}
	}
	e.mu.Unlock()
	if err != nil {
		e.log("warn", "auto-baseline: state file unusable, starting fresh: "+sanitize.Error(err))
	}
}

// importPendingLocked merges restored evidence into the learner.
func (e *Engine) importPendingLocked() int {
	dropped := e.learner.Import(e.state.Pending, e.clk.Now())
	e.state.Pending = nil
	e.restoredDropped = dropped
	return dropped
}

// Reconfigure applies a new config. When any write-affecting field changed
// (config-path, backup-dir, state-dir, dry-run, floors, manage-*, enabled) it
// first drains in-flight workers and timers exactly like Stop, so no attempt
// planned under the old config can write under the new one; every attempt
// additionally carries a config generation that is re-checked before apply.
// A state-dir change flushes to the old dir, then loads and merges from the
// new one. Reconfigure re-resolves config.yaml, refreshes baselines (which
// confirms awaiting promotions whose value is now on disk), and clears the
// faulted flag. Disabling stops promotions while retaining status. When any
// classifier-affecting rule changed, pending evidence gathered under the old
// rules is discarded.
func (e *Engine) Reconfigure(cfg config.Config) {
	e.mu.Lock()
	old := e.cfg
	writeChanged := old.WriteFingerprint() != cfg.WriteFingerprint()
	rulesChanged := old.ClassifierFingerprint() != cfg.ClassifierFingerprint()
	e.configGen++
	wasStopped := e.stopped
	e.mu.Unlock()

	if writeChanged {
		e.drain() // write barrier: nothing planned under the old config may proceed
	}

	e.mu.Lock()
	e.cfg = cfg
	e.rules = cfg.Rules()
	e.learner.Reconfigure(settingsFor(cfg), managedFor(cfg))
	if rulesChanged {
		e.learner.Reset()
		e.mutations++
	}
	if e.dryRunPending && cfg.DryRun == e.dryRunTarget {
		// CPA reloaded the file the toggle wrote: the runtime flag now
		// matches the requested value.
		e.dryRunPending = false
	}
	e.mu.Unlock()
	wasFaulted, _ := e.isFaulted()
	e.setFaulted(false, "")

	if rulesChanged {
		e.log("info", "auto-baseline: classifier rules changed; pending evidence cleared")
	}
	if wasFaulted {
		e.log("info", "auto-baseline: fault cleared by reconfigure")
	}
	if e.started && old.StateDir != cfg.StateDir {
		e.migrateState(old.StateDir, cfg.StateDir)
	}
	if !cfg.Enabled {
		e.Stop()
		return
	}
	e.mu.Lock()
	started := e.started
	e.mu.Unlock()
	if !started {
		// Registered disabled, enabled later: the new config is already in
		// place, so a plain Start does the right thing.
		e.Start()
		return
	}
	e.refreshConfig()
	e.mu.Lock()
	if wasStopped || writeChanged {
		e.stopped = false
		e.armSaveTimerLocked()
	}
	e.mu.Unlock()
	e.kickPromotion()
}

// migrateState flushes the current state to the old dir, then loads,
// validates, and merges whatever the new dir holds.
func (e *Engine) migrateState(oldDir, newDir string) {
	e.saveStateTo(oldDir, true)
	e.mu.Lock()
	live := e.learner.Export()
	e.mu.Unlock()
	e.loadState(newDir)
	e.mu.Lock()
	dropped := e.importPendingLocked()
	// Evidence observed since the last flush survives the swap.
	dropped += e.learner.Import(live, e.clk.Now())
	e.mutations++
	e.mu.Unlock()
	e.log("info", fmt.Sprintf("auto-baseline: state dir changed %s -> %s (merged; %d restored candidate(s) dropped)", oldDir, newDir, dropped))
}

// drain stops timers and waits for every worker and timer callback to
// leave. It leaves the engine marked stopped; callers that intend to resume
// clear the flag afterwards.
func (e *Engine) drain() {
	e.mu.Lock()
	e.stopped = true
	e.stopTimersLocked()
	e.mu.Unlock()
	e.workers.Wait()
}

// stopTimersLocked cancels the save and cooldown timers. Each timer was
// counted into workers when armed (see armTimerLocked); when Stop() reports
// the callback had not yet fired we own its Done, otherwise the callback
// does.
func (e *Engine) stopTimersLocked() {
	for _, t := range []*clock.Timer{&e.saveTimer, &e.retryTimer} {
		if *t == nil {
			continue
		}
		if (*t).Stop() {
			e.workers.Done()
		}
		*t = nil
	}
}

// armTimerLocked registers the timer with the WaitGroup BEFORE arming it so
// no Add can ever race a Wait. The callback runs f and owns Done unless it
// was cancelled first (then stopTimersLocked owns it). f runs outside mu.
func (e *Engine) armTimerLocked(slot *clock.Timer, after time.Duration, name string, f func()) {
	if *slot != nil {
		if (*slot).Stop() {
			e.workers.Done()
		}
		*slot = nil
	}
	e.workers.Add(1)
	var self clock.Timer
	self = e.clk.AfterFunc(after, func() {
		defer e.workers.Done()
		defer e.recoverWorker(name)
		e.mu.Lock()
		stopped := e.stopped
		if *slot == self {
			*slot = nil
		}
		e.mu.Unlock()
		if !stopped {
			f()
		}
	})
	*slot = self
}

// Stop halts timers and promotions, waits for every in-flight worker and
// timer callback, and flushes state. After Stop returns no config write can
// start or complete. A later Reconfigure with Enabled=true resumes.
func (e *Engine) Stop() {
	e.drain()
	e.saveState(true)
}

// Shutdown is Stop plus a final flush; kept as a distinct entry point for the
// runtime's terminal path.
func (e *Engine) Shutdown() {
	e.Stop()
}

// isFaulted reads the fault flag without touching mu.
func (e *Engine) isFaulted() (bool, string) {
	e.faultMu.Lock()
	defer e.faultMu.Unlock()
	return e.faulted, e.faultReason
}

func (e *Engine) setFaulted(faulted bool, reason string) {
	e.faultMu.Lock()
	defer e.faultMu.Unlock()
	e.faulted = faulted
	e.faultReason = reason
}

// recoverWorker converts a panic in a plugin-owned goroutine into a faulted
// engine state. It must be deferred directly by the goroutine function. It
// never takes mu: the panic may have happened inside a mu critical section
// (which defer Unlock has already released by the time this runs, but the
// engine state under it may be half-updated), so the fault is recorded under
// faultMu only and mirrored into last_error on the next Status call.
func (e *Engine) recoverWorker(name string) {
	recovered := recover()
	if recovered == nil {
		return
	}
	err := fmt.Errorf("panic in %s: %v", name, recovered)
	e.setFaulted(true, sanitize.Error(err))
	e.log("error", "auto-baseline: "+sanitize.Error(err)+"; writes suspended until reconfigure")
}

// refreshConfig resolves the config path, probes it and the backup dir,
// detects unsupported deployment modes, and reads effective baselines.
func (e *Engine) refreshConfig() {
	e.mu.Lock()
	override := e.cfg.ConfigPath
	backupDir := e.cfg.EffectiveBackupDir()
	e.mu.Unlock()

	path, source := configfile.ResolvePath(override)
	mode := e.detect()
	exists, writable, probeErr := configfile.Probe(path)
	backupWritable, backupErr := configfile.ProbeDir(backupDir)
	var snap configfile.Snapshot
	var readErr error
	if exists && probeErr == nil {
		snap, readErr = e.read(path)
	}

	e.mu.Lock()
	e.configPath = path
	e.configSource = source
	e.configExists = exists
	e.configWritable = writable
	e.configReadAt = e.clk.Now()
	e.backupDir = backupDir
	e.backupWritable = backupWritable
	e.backupError = ""
	if backupErr != nil {
		e.backupError = sanitize.Error(backupErr)
	} else if !backupWritable {
		e.backupError = "backup dir is not writable; nothing can be promoted"
	}
	e.mode = mode
	e.modeUnsupported = mode.Name != "" && override == ""
	logMode := e.modeUnsupported && !e.modeLogged
	if logMode {
		e.modeLogged = true
	}
	switch {
	case probeErr != nil:
		e.configError = sanitize.Error(probeErr)
	case !exists:
		e.configError = "config file does not exist; learning continues but nothing can be promoted"
	case readErr != nil:
		e.configError = sanitize.Error(readErr)
	case !writable:
		e.configError = "config file is not writable by the CPA process; learning continues but nothing can be promoted"
	default:
		e.configError = ""
	}
	if exists && probeErr == nil && readErr == nil {
		e.applySnapshotLocked(snap)
	}
	e.mu.Unlock()
	if logMode {
		e.log("warn", fmt.Sprintf("auto-baseline: CPA appears to run in %s mode (%s); the local config file is not the effective configuration, so automatic writes are disabled. Set config-path explicitly to override, or run a host-side updater (see docs/troubleshooting.md).", mode.Name, mode.Reason))
	}
}

// applySnapshotLocked records a freshly read config snapshot as the effective
// baseline, informs the learner, and confirms awaiting promotions whose value
// is now visible on disk.
func (e *Engine) applySnapshotLocked(snap configfile.Snapshot) {
	e.configHash = snap.SHA256
	e.disableCodex = snap.DisableCodexCloaking
	now := e.clk.Now()
	for _, eff := range []configfile.Effective{snap.Claude, snap.Codex} {
		e.effective[eff.Provider] = eff
		e.learner.SetBaseline(eff.Provider, eff.Version, eff.Blocked())
		e.state.Baselines[eff.Provider] = statefile.Baseline{
			Version:        eff.Version,
			UserAgent:      eff.UserAgent,
			PackageVersion: eff.PackageVersion,
			RuntimeVersion: eff.RuntimeVersion,
			Explicit:       eff.Explicit,
			ObservedAt:     now,
		}
		// Reload confirmation is value-correlated: the on-disk tuple must
		// equal the promoted tuple exactly.
		if _, waiting := e.awaitingReload[eff.Provider]; waiting {
			if lp := e.state.LastPromotion[eff.Provider]; lp != nil && effectiveMatches(eff, lp.Candidate) {
				e.confirmProviderLocked(eff.Provider, now)
			}
		}
	}
	e.mutations++
}

// effectiveMatches reports whether the on-disk baseline equals a promoted
// candidate tuple.
func effectiveMatches(eff configfile.Effective, c fingerprint.Candidate) bool {
	if eff.Blocked() != "" || !eff.Explicit || eff.UserAgent != c.UserAgent {
		return false
	}
	if c.Provider == fingerprint.ProviderClaude {
		return eff.PackageVersion == c.PackageVersion && eff.RuntimeVersion == c.RuntimeVersion
	}
	return true
}

func (e *Engine) confirmProviderLocked(p fingerprint.Provider, now time.Time) {
	delete(e.awaitingReload, p)
	if lp := e.state.LastPromotion[p]; lp != nil && lp.AwaitingReload {
		lp.AwaitingReload = false
		lp.ConfirmedAt = now
		for i := range e.state.History {
			h := &e.state.History[i]
			if h.Provider == p && h.At.Equal(lp.At) && h.AwaitingReload {
				h.AwaitingReload = false
				h.ConfirmedAt = now
			}
		}
		e.mutations++
	}
}

// Observe is the interceptor hot path.
func (e *Engine) Observe(headers map[string][]string) {
	e.mu.Lock()
	if !e.started || e.stopped {
		e.mu.Unlock()
		return
	}
	e.state.Counters.Requests++
	e.mutations++
	res := fingerprint.Classify(headers, e.rules)
	if !res.OK {
		if res.Provider == "" {
			e.state.Counters.Ignored++
		} else {
			e.state.Counters.Rejected++
			e.state.Counters.RejectReasons[res.Reason]++
		}
		e.mu.Unlock()
		return
	}
	e.state.Counters.Accepted++
	d := e.recordObservationLocked(res.Candidate, res.SessionID)
	e.mu.Unlock()
	if d.Ready != nil {
		e.startPromotionWorker()
	}
}

// recordObservationLocked feeds the learner and counts the decision.
func (e *Engine) recordObservationLocked(c fingerprint.Candidate, sessionID string) learner.Decision {
	d := e.learner.Observe(c, sessionID, e.clk.Now())
	e.state.Counters.Decisions[d.Reason]++
	return d
}

// startPromotionWorker launches the background promotion worker unless one
// is already running, in which case it asks the running worker to rescan
// when it finishes so no ready candidate or queued forced promotion is lost.
// It reports whether a worker is now guaranteed to process pending work.
func (e *Engine) startPromotionWorker() bool {
	if faulted, _ := e.isFaulted(); faulted {
		return false
	}
	e.mu.Lock()
	if !e.started || e.stopped {
		e.mu.Unlock()
		return false
	}
	if e.promotionInFlight {
		e.rescan = true
		e.mu.Unlock()
		return true
	}
	e.promotionInFlight = true
	e.rescan = false
	e.workers.Add(1)
	e.mu.Unlock()
	e.runAsync(func() {
		defer e.workers.Done()
		defer func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.promotionInFlight = false
		}()
		defer e.recoverWorker("promotion worker")
		for {
			e.promotionPass()
			if !e.workerShouldContinue() {
				return
			}
		}
	})
	return true
}

func (e *Engine) workerShouldContinue() bool {
	faulted, _ := e.isFaulted()
	e.mu.Lock()
	defer e.mu.Unlock()
	again := (e.rescan || len(e.forcedQueue) > 0) && !e.stopped && !faulted
	e.rescan = false
	return again
}

// promotionPass replays queued forced promotions, then evaluates every
// provider's observed evidence. A forced entry stays queued until its
// attempt reaches a terminal outcome (promoteProvider dequeues it); an
// attempt aborted by the write barrier leaves it queued for the next pass.
func (e *Engine) promotionPass() {
	for _, p := range providers {
		e.mu.Lock()
		c, queued := e.forcedQueue[p]
		e.mu.Unlock()
		if queued {
			e.promoteProvider(p, &c, sourceManagement)
		}
	}
	for _, p := range providers {
		e.promoteProvider(p, nil, sourceObserved)
	}
}

// dequeueForcedLocked removes a forced entry once its attempt is terminal,
// but only if it is still the same candidate (a newer force may have
// replaced it meanwhile).
func (e *Engine) dequeueForcedLocked(c fingerprint.Candidate) {
	if q, ok := e.forcedQueue[c.Provider]; ok && q.Key() == c.Key() {
		delete(e.forcedQueue, c.Provider)
	}
}

// enqueueForcedLocked keeps the highest version per provider; ties keep the
// existing entry.
func (e *Engine) enqueueForcedLocked(c fingerprint.Candidate) {
	if q, ok := e.forcedQueue[c.Provider]; ok && !c.Version.Newer(q.Version) {
		return
	}
	e.forcedQueue[c.Provider] = c
}

// kickPromotion re-evaluates quorum for every provider (after start,
// reconfigure, or a cooldown timer).
func (e *Engine) kickPromotion() {
	e.mu.Lock()
	ready := len(e.forcedQueue) > 0
	for _, p := range providers {
		if _, ok := e.learner.Ready(p); ok {
			ready = true
			break
		}
	}
	e.mu.Unlock()
	if ready {
		e.startPromotionWorker()
	}
}

// Decision buckets for refusals decided against the fresh on-disk read.
const (
	DecisionPluginDisabledOnDisk = "plugin_disabled_on_disk"
	DecisionBaselineImplicit     = "baseline_implicit"
)

// checkError carries a decision bucket for a refused pre-write check.
type checkError struct {
	reason string
	detail string
}

func (c *checkError) Error() string { return c.reason + ": " + c.detail }

var (
	errNotNewer   = errors.New("candidate is not newer than the baseline currently on disk")
	errBelowFloor = errors.New("candidate is below the configured floor")
	errStopped    = errors.New("plugin stopped before the write")
	errGeneration = errors.New("configuration changed before the write; attempt abandoned")
)

// writeBlockedLocked reports why a write cannot happen right now, or "".
func (e *Engine) writeBlockedLocked() string {
	faulted, reason := e.isFaulted()
	switch {
	case e.stopped:
		return "plugin is stopped"
	case faulted:
		return "plugin is faulted: " + reason
	case !e.cfg.Enabled:
		return "plugin is disabled"
	case e.modeUnsupported:
		return fmt.Sprintf("CPA runs in %s mode (%s) and config-path is not set; automatic writes are disabled", e.mode.Name, e.mode.Reason)
	case !e.configExists || !e.configWritable:
		return e.configError
	case !e.backupWritable:
		return e.backupError
	default:
		return ""
	}
}

// promoteProvider writes one provider's ready candidate (or the forced
// candidate) into config.yaml, subject to cooldown, dry-run, and the
// on-disk re-check. It runs on the background worker only.
func (e *Engine) promoteProvider(provider fingerprint.Provider, forced *fingerprint.Candidate, source string) {
	faulted, _ := e.isFaulted()
	e.mu.Lock()
	if e.stopped || faulted || !e.cfg.Manages(provider) {
		e.mu.Unlock()
		return
	}
	gen := e.configGen
	var cand fingerprint.Candidate
	if forced != nil {
		cand = *forced
	} else {
		ready, ok := e.learner.Ready(provider)
		if !ok {
			e.mu.Unlock()
			return
		}
		cand = ready
	}
	now := e.clk.Now()
	if forced == nil {
		if last, ok := e.state.LastWriteAt[provider]; ok {
			until := last.Add(e.cfg.PromotionCooldown)
			if now.Before(until) {
				e.armRetryTimerLocked(until.Sub(now))
				e.mu.Unlock()
				return
			}
		}
	}
	dryRun := e.effectiveDryRunLocked()
	requireExplicit := e.cfg.RequireExplicitBaseline
	path, backupDir := e.configPath, e.backupDir
	floor := e.cfg.MinVersion(provider)
	obs, sessions := e.evidenceLocked(provider, cand.Key())
	if !dryRun {
		if blocked := e.writeBlockedLocked(); blocked != "" {
			e.setErrorLocked(fmt.Errorf("cannot promote %s %s: %s", provider, cand.Version, blocked))
			e.mu.Unlock()
			return
		}
	}
	e.mu.Unlock()

	// check runs against the FRESH on-disk read inside the read-modify-write.
	check := func(snap configfile.Snapshot) error {
		eff := effectiveFor(snap, provider)
		if b := eff.Blocked(); b != "" {
			return &checkError{reason: b, detail: "the provider block in config.yaml cannot be compared against or edited; fix it before the plugin can promote"}
		}
		if !dryRun && (!snap.PluginsEnabled || !snap.InstanceEnabled) {
			return &checkError{reason: DecisionPluginDisabledOnDisk, detail: "plugins.enabled or plugins.configs.auto-baseline.enabled is not true in config.yaml; the host may have disabled the plugin without quiesce"}
		}
		if requireExplicit && !eff.Explicit {
			return &checkError{reason: DecisionBaselineImplicit, detail: "require-explicit-baseline is set and config.yaml carries no explicit " + string(provider) + " baseline"}
		}
		if floor.Newer(cand.Version) {
			return errBelowFloor
		}
		if !cand.Version.Newer(eff.Version) {
			return errNotNewer
		}
		return nil
	}

	record := statefile.Promotion{
		At:               now,
		Provider:         provider,
		Candidate:        cand,
		DryRun:           dryRun,
		Forced:           forced != nil,
		Source:           source,
		Observations:     obs,
		DistinctSessions: sessions,
	}

	if dryRun {
		snap, err := e.read(path)
		if err != nil {
			e.recordFailure(err)
			return
		}
		if err := check(snap); err != nil {
			e.handleCheckFailure(provider, cand, snap, err)
			return
		}
		record.From = effectiveFor(snap, provider).Version
		e.mu.Lock()
		e.applySnapshotLocked(snap)
		e.dequeueForcedLocked(cand)
		already := e.state.LastPromotion[provider]
		if already != nil && already.DryRun && already.Candidate.Key() == cand.Key() {
			e.mu.Unlock()
			return
		}
		e.state.RecordPromotion(record)
		e.mutations++
		e.mu.Unlock()
		e.log("info", fmt.Sprintf("auto-baseline: dry-run would promote %s baseline %s -> %s (%s)", provider, record.From, cand.Version, describe(cand)))
		e.saveState(false)
		return
	}

	var snap configfile.Snapshot
	var err error
	for attempt := 0; attempt < maxWriteAttempts; attempt++ {
		// Re-check the write barrier and the config generation immediately
		// before each destructive attempt: Stop or Reconfigure may have run
		// while an earlier attempt was in progress.
		if abort := e.preWriteAbort(gen, provider, cand); abort != nil {
			if errors.Is(abort, errStopped) || errors.Is(abort, errGeneration) {
				// Not terminal: a forced entry stays queued for replay.
				e.mu.Lock()
				e.rescan = true
				e.mu.Unlock()
				e.log("info", fmt.Sprintf("auto-baseline: %s %s: %s", provider, cand.Version, sanitize.Error(abort)))
				return
			}
			e.recordFailure(abort)
			return
		}
		snap, err = e.apply(path, backupDir, cand, check)
		if !errors.Is(err, configfile.ErrChanged) {
			break
		}
	}
	if err != nil {
		var ce *checkError
		if errors.Is(err, errNotNewer) || errors.Is(err, errBelowFloor) || errors.As(err, &ce) {
			e.handleCheckFailure(provider, cand, snap, err)
			return
		}
		e.recordFailure(fmt.Errorf("promote %s %s: %w", provider, cand.Version, err))
		return
	}
	record.From = effectiveFor(snap, provider).Version
	record.AwaitingReload = true

	e.recordSuccessfulWrite(provider, cand, record, now)
	e.log("info", fmt.Sprintf("auto-baseline: promoted %s baseline %s -> %s (%s); CPA will hot-reload config.yaml", provider, record.From, cand.Version, describe(cand)))
	e.saveState(false)
}

// preWriteAbort is the last check before a destructive apply. It runs under
// the lock so nothing can slip in between the check and the decision.
func (e *Engine) preWriteAbort(gen uint64, provider fingerprint.Provider, cand fingerprint.Candidate) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped {
		return errStopped
	}
	if gen != e.configGen {
		return errGeneration
	}
	if blocked := e.writeBlockedLocked(); blocked != "" {
		return fmt.Errorf("cannot promote %s %s: %s", provider, cand.Version, blocked)
	}
	return nil
}

// recordSuccessfulWrite installs the promotion, the awaiting-reload marker,
// and the new effective baseline in ONE critical section, so a reload that
// lands during marker installation cannot observe a half-installed state.
// The section uses defer Unlock so a panic (see postWriteHook) releases mu.
func (e *Engine) recordSuccessfulWrite(provider fingerprint.Provider, cand fingerprint.Candidate, record statefile.Promotion, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.state.RecordPromotion(record)
	e.state.LastError = ""
	e.state.LastErrorAt = time.Time{}
	e.awaitingReload[provider] = now
	e.dequeueForcedLocked(cand)
	e.learner.Forget(provider, cand.Key())
	e.learner.SetBaseline(provider, cand.Version, "")
	e.effective[provider] = configfile.Effective{
		Provider:       provider,
		Version:        cand.Version,
		UserAgent:      cand.UserAgent,
		PackageVersion: cand.PackageVersion,
		RuntimeVersion: cand.RuntimeVersion,
		Explicit:       true,
	}
	e.state.Baselines[provider] = statefile.Baseline{
		Version:        cand.Version,
		UserAgent:      cand.UserAgent,
		PackageVersion: cand.PackageVersion,
		RuntimeVersion: cand.RuntimeVersion,
		Explicit:       true,
		ObservedAt:     now,
	}
	e.mutations++
	if e.postWriteHook != nil {
		e.postWriteHook()
	}
}

// handleCheckFailure runs when the on-disk baseline turned out to be at least
// as new as the candidate (someone else raised it), the candidate is below
// the floor, or the on-disk baseline is malformed: adopt the on-disk state,
// drop the candidate, record the decision, no error unless malformed.
func (e *Engine) handleCheckFailure(provider fingerprint.Provider, cand fingerprint.Candidate, snap configfile.Snapshot, err error) {
	e.mu.Lock()
	if snap.Path != "" {
		e.applySnapshotLocked(snap)
	}
	e.dequeueForcedLocked(cand)
	var ce *checkError
	if errors.As(err, &ce) {
		// A refusal decided by file state (blocked block, plugin disabled on
		// disk, implicit baseline): keep the evidence so the candidate is
		// re-evaluated once the file changes; surface the reason.
		e.state.Counters.Decisions[ce.reason]++
		e.setErrorLocked(fmt.Errorf("cannot promote %s %s: %w", provider, cand.Version, err))
	} else {
		e.learner.Forget(provider, cand.Key())
	}
	e.mu.Unlock()
	e.log("info", fmt.Sprintf("auto-baseline: skipped %s candidate %s: %s", provider, cand.Version, sanitize.Error(err)))
}

func (e *Engine) recordFailure(err error) {
	e.mu.Lock()
	e.setErrorLocked(err)
	e.mu.Unlock()
	e.log("warn", "auto-baseline: "+sanitize.Error(err))
	e.saveState(false)
}

func (e *Engine) setErrorLocked(err error) {
	e.state.LastError = sanitize.Error(err)
	e.state.LastErrorAt = e.clk.Now()
	e.mutations++
}

func effectiveFor(snap configfile.Snapshot, provider fingerprint.Provider) configfile.Effective {
	if provider == fingerprint.ProviderCodex {
		return snap.Codex
	}
	return snap.Claude
}

func describe(c fingerprint.Candidate) string {
	if c.Provider == fingerprint.ProviderClaude {
		return fmt.Sprintf("user-agent %q package-version %s runtime-version %s", c.UserAgent, c.PackageVersion, c.RuntimeVersion)
	}
	return fmt.Sprintf("user-agent %q", c.UserAgent)
}

func (e *Engine) evidenceLocked(provider fingerprint.Provider, key string) (int, int) {
	for _, ev := range e.learner.Summarize(provider) {
		if ev.Candidate.Key() == key {
			return ev.Observations, ev.DistinctSessions
		}
	}
	return 0, 0
}

// armRetryTimerLocked schedules a promotion re-check when a cooldown ends.
func (e *Engine) armRetryTimerLocked(after time.Duration) {
	e.armTimerLocked(&e.retryTimer, after, "cooldown timer", e.kickPromotion)
}

func (e *Engine) armSaveTimerLocked() {
	e.armTimerLocked(&e.saveTimer, stateSaveInterval, "save timer", func() {
		e.saveState(false)
		e.mu.Lock()
		defer e.mu.Unlock()
		if !e.stopped {
			e.armSaveTimerLocked()
		}
	})
}

// saveState persists state when dirty (or always when force is set). Saves
// are single-flight: the state is deep-copied under the lock and serialized
// outside it, one save at a time, and a generation is only committed when it
// is at least as new as the last committed one.
func (e *Engine) saveState(force bool) {
	e.mu.Lock()
	dir := e.cfg.StateDir
	e.mu.Unlock()
	e.saveStateTo(dir, force)
}

func (e *Engine) saveStateTo(dir string, force bool) {
	e.saveMu.Lock()
	defer e.saveMu.Unlock()

	e.mu.Lock()
	if !e.started || (e.mutations == e.savedGen && !force) {
		e.mu.Unlock()
		return
	}
	gen := e.mutations
	st := e.state.Clone()
	st.Pending = e.learner.Export()
	now := e.clk.Now()
	e.mu.Unlock()

	err := statefile.Save(dir, st, now)

	e.mu.Lock()
	if err != nil {
		e.stateError = sanitize.Error(err)
	} else {
		e.stateError = ""
		if gen >= e.savedGen {
			e.savedGen = gen
			e.stateSaveAt = now
		}
	}
	e.mu.Unlock()
	if err != nil {
		e.log("warn", "auto-baseline: failed to save state: "+sanitize.Error(err))
	}
}

// effectiveDryRunLocked is the dry-run flag the promotion path obeys. A
// pending switch TO dry-run takes effect immediately (the operator asked for
// writes to stop; CPA's reload only catches the runtime config up), while a
// pending switch to live writes waits for plugin.reconfigure, so nothing can
// be written under a config CPA has not yet applied.
func (e *Engine) effectiveDryRunLocked() bool {
	return e.cfg.DryRun || (e.dryRunPending && e.dryRunTarget)
}

// DryRunOutcome is the result of SetDryRun.
type DryRunOutcome struct {
	// Status is the HTTP status the management route should answer with.
	Status int    `json:"-"`
	Result string `json:"status"`
	Detail string `json:"detail"`
	// DryRun is the value now on disk (after a successful write) or the
	// current runtime value otherwise.
	DryRun bool `json:"dry_run"`
	// AwaitingReload is true after a successful write until CPA's reload
	// delivers the new value through plugin.reconfigure.
	AwaitingReload bool `json:"awaiting_reload"`
}

// SetDryRun edits plugins.configs.auto-baseline.dry-run in CPA's config.yaml
// through the same read-modify-write discipline as a promotion (shape
// refusals, backup, re-hash, in-place write + verify, deployment-mode and
// writability guards). It refuses with 409 while a promotion write is in
// flight and with 503 when writes are disabled. The runtime flag itself only
// flips when CPA hot-reloads and reconfigures the plugin; until then status
// reports the toggle as awaiting reload.
func (e *Engine) SetDryRun(enabled bool) DryRunOutcome {
	path, backupDir, current, early := e.beginDryRunToggle(enabled)
	if early != nil {
		return *early
	}
	defer e.workers.Done()
	defer func() {
		// Release the slot and honour any work that arrived while it was
		// held: observations that reached quorum set rescan, and forces were
		// queued. A real worker must process them.
		e.mu.Lock()
		e.promotionInFlight = false
		pending := e.rescan || len(e.forcedQueue) > 0
		e.rescan = false
		e.mu.Unlock()
		if pending {
			e.startPromotionWorker()
		}
	}()
	defer e.recoverWorker("dry-run toggle")

	snap, changed, err := e.applyDryRun(path, backupDir, enabled, func(snap configfile.Snapshot) error {
		if !snap.InstancePresent {
			// Reported as the more actionable problem: nothing to toggle.
			return configfile.ErrPluginSubtreeMissing
		}
		if !snap.PluginsEnabled || !snap.InstanceEnabled {
			return &checkError{reason: DecisionPluginDisabledOnDisk, detail: "plugins.enabled or plugins.configs.auto-baseline.enabled is not true in config.yaml"}
		}
		return nil
	})
	if err != nil {
		e.recordDryRunFailure(snap, enabled, err)
		e.log("warn", fmt.Sprintf("auto-baseline: dry-run toggle failed: %s", sanitize.Error(err)))
		status := 500
		if errors.Is(err, configfile.ErrPluginSubtreeMissing) || errors.Is(err, configfile.ErrUnsupportedShape) || errors.Is(err, configfile.ErrDuplicateKey) || errors.Is(err, configfile.ErrMultiDocument) || errors.Is(err, configfile.ErrCycle) {
			status = 422
		}
		var ce *checkError
		if errors.As(err, &ce) {
			status = 409
		}
		return DryRunOutcome{Status: status, Result: "error", Detail: sanitize.Error(err), DryRun: current}
	}
	if !changed {
		// The file already carried the target: CPA will not reload, so do
		// not arm a marker that could never be confirmed (and do not reset
		// the warning clock of one that is already pending).
		e.mu.Lock()
		defer e.mu.Unlock()
		e.applySnapshotLocked(snap)
		return DryRunOutcome{Status: 200, Result: "unchanged", Detail: fmt.Sprintf("config.yaml already carries dry-run: %t", enabled), DryRun: e.effectiveDryRunLocked(), AwaitingReload: e.dryRunPending}
	}
	now := e.clk.Now()
	e.mu.Lock()
	e.dryRunPending = true
	e.dryRunTarget = enabled
	e.dryRunWrittenAt = now
	e.state.LastError = ""
	e.state.LastErrorAt = time.Time{}
	e.mutations++
	e.mu.Unlock()
	e.log("info", fmt.Sprintf("auto-baseline: dry-run set to %t in config.yaml by operator; CPA will hot-reload", enabled))
	e.saveState(false)
	detail := fmt.Sprintf("config.yaml updated; dry-run becomes %t when CPA reloads", enabled)
	if enabled {
		detail = "config.yaml updated; writes are suspended now and CPA will reload the flag"
	}
	return DryRunOutcome{Status: 200, Result: "ok", Detail: detail, DryRun: enabled, AwaitingReload: true}
}

// beginDryRunToggle validates the request and, when it may proceed, takes
// the promotion-worker slot and a WaitGroup admission. Every exit path runs
// under a deferred unlock.
func (e *Engine) beginDryRunToggle(enabled bool) (path, backupDir string, current bool, early *DryRunOutcome) {
	faulted, reason := e.isFaulted()
	e.mu.Lock()
	defer e.mu.Unlock()
	current = e.cfg.DryRun
	if !e.started || e.stopped {
		return "", "", current, &DryRunOutcome{Status: 503, Result: "unavailable", Detail: "plugin is disabled or stopped", DryRun: current}
	}
	if e.promotionInFlight {
		return "", "", current, &DryRunOutcome{Status: 409, Result: "busy", Detail: "a promotion write is in flight; retry in a moment", DryRun: current}
	}
	if e.cfg.DryRun == enabled && !e.dryRunPending {
		return "", "", current, &DryRunOutcome{Status: 200, Result: "unchanged", Detail: fmt.Sprintf("dry-run is already %t", enabled), DryRun: enabled}
	}
	// Reuse the write barrier, but the dry-run flag itself must not block
	// its own toggle and a stopped engine was handled above.
	blocked := ""
	switch {
	case faulted:
		blocked = "plugin is faulted: " + reason
	case e.modeUnsupported:
		blocked = fmt.Sprintf("CPA runs in %s mode (%s) and config-path is not set; automatic writes are disabled", e.mode.Name, e.mode.Reason)
	case !e.configExists || !e.configWritable:
		blocked = e.configError
	case !e.backupWritable:
		blocked = e.backupError
	}
	if blocked != "" {
		return "", "", current, &DryRunOutcome{Status: 503, Result: "unavailable", Detail: blocked, DryRun: current}
	}
	// Hold the worker slot so no promotion can start mid-edit.
	e.promotionInFlight = true
	e.workers.Add(1)
	return e.configPath, e.backupDir, current, nil
}

// recordDryRunFailure records a failed toggle under the lock with a deferred
// unlock so a panic inside cannot leave mu held.
func (e *Engine) recordDryRunFailure(snap configfile.Snapshot, enabled bool, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if snap.Path != "" {
		e.applySnapshotLocked(snap)
	}
	e.setErrorLocked(fmt.Errorf("set dry-run=%t: %w", enabled, err))
}

// Reset discards pending candidates (baselines and history are kept).
func (e *Engine) Reset() {
	e.mu.Lock()
	e.learner.Reset()
	e.mutations++
	e.mu.Unlock()
	e.saveState(false)
	e.log("info", "auto-baseline: pending candidates cleared by operator")
}

// Report is a management-supplied fingerprint (host reporting).
type Report struct {
	Provider       string `json:"provider"`
	UserAgent      string `json:"user_agent"`
	PackageVersion string `json:"package_version"`
	RuntimeVersion string `json:"runtime_version"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	SessionID      string `json:"session_id"`
	Force          bool   `json:"force"`
}

// ReportOutcome is the result of ReportObservation.
type ReportOutcome struct {
	Accepted  bool                  `json:"accepted"`
	Reason    string                `json:"reason"`
	Decision  string                `json:"decision,omitempty"`
	Candidate fingerprint.Candidate `json:"candidate,omitempty"`
	// Queued is set when a forced promotion was accepted into the bounded
	// per-provider queue (one entry each; a newer force replaces an older
	// queued one). It is executed by the promotion worker.
	Queued bool `json:"queued,omitempty"`
}

// ReportObservation treats a management-supplied fingerprint exactly like an
// observed request: it is rebuilt into headers, run through the same
// classifier, and counted toward quorum. With Force it is queued for
// immediate promotion after validation (still never below the on-disk
// baseline or the floor, never when the plugin is disabled, and never
// written in dry-run).
func (e *Engine) ReportObservation(r Report) ReportOutcome {
	headers := map[string][]string{
		fingerprint.HeaderUserAgent: {strings.TrimSpace(r.UserAgent)},
	}
	switch fingerprint.Provider(strings.ToLower(strings.TrimSpace(r.Provider))) {
	case fingerprint.ProviderClaude:
		headers[fingerprint.HeaderXApp] = []string{"cli"}
		headers[fingerprint.HeaderAnthropicVersion] = []string{fingerprint.RequiredAnthropicVersion}
		headers[fingerprint.HeaderAnthropicBeta] = []string{fingerprint.ClaudeCodeBeta}
		headers[fingerprint.HeaderStainlessLang] = []string{"js"}
		headers[fingerprint.HeaderStainlessRuntime] = []string{"node"}
		headers[fingerprint.HeaderStainlessPackageVersion] = []string{strings.TrimSpace(r.PackageVersion)}
		headers[fingerprint.HeaderStainlessRuntimeVersion] = []string{strings.TrimSpace(r.RuntimeVersion)}
		headers[fingerprint.HeaderStainlessOS] = []string{strings.TrimSpace(r.OS)}
		headers[fingerprint.HeaderStainlessArch] = []string{strings.TrimSpace(r.Arch)}
		if s := strings.TrimSpace(r.SessionID); s != "" {
			headers[fingerprint.HeaderClaudeSessionID] = []string{s}
		}
	case fingerprint.ProviderCodex:
		headers[fingerprint.HeaderOriginator] = []string{"management"}
		if s := strings.TrimSpace(r.SessionID); s != "" {
			headers[fingerprint.HeaderCodexSessionID] = []string{s}
		}
	default:
		return ReportOutcome{Reason: "provider must be \"claude\" or \"codex\""}
	}

	e.mu.Lock()
	if !e.started || e.stopped {
		e.mu.Unlock()
		return ReportOutcome{Reason: "plugin is disabled or stopped"}
	}
	res := fingerprint.Classify(headers, e.rules)
	if !res.OK {
		e.state.Counters.Rejected++
		e.state.Counters.RejectReasons[res.Reason]++
		e.mutations++
		e.mu.Unlock()
		return ReportOutcome{Reason: res.Reason}
	}
	if !e.cfg.Manages(res.Candidate.Provider) {
		e.mu.Unlock()
		return ReportOutcome{Reason: learner.DecisionProviderUnmanaged, Candidate: res.Candidate}
	}
	e.state.Counters.Accepted++
	e.mutations++
	d := e.recordObservationLocked(res.Candidate, res.SessionID)
	out := ReportOutcome{Accepted: true, Reason: "observation recorded", Decision: d.Reason, Candidate: res.Candidate}
	if r.Force {
		switch d.Reason {
		case learner.DecisionTracked, learner.DecisionQuorum:
			e.enqueueForcedLocked(res.Candidate)
			out.Queued = true
			out.Reason = "forced promotion queued"
		default:
			e.mu.Unlock()
			out.Reason = "force ignored: " + d.Reason
			return out
		}
	}
	e.mu.Unlock()
	if r.Force || d.Ready != nil {
		e.startPromotionWorker()
	}
	return out
}
