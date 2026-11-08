// Package plugin is chat-archive's lifecycle: registration, strict
// config, start/stop of the capture pipeline, uploader and background loops, quiesce and
// rollback, the management routes and the status snapshot. Hooks are served by internal/hooks
// through the Engine created once per instance.
package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/capture"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/config"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/labels"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/pushalert"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/safego"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/uploader"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/fingerprint"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// ID is the plugin id.
const ID = "chat-archive"

// Version is stamped by the Makefile (-X …/internal/plugin.Version=$(VERSION)).
var Version = "0.1.0"

// Plugin states reported in status and heartbeats.
const (
	StateUnconfigured = "unconfigured"
	StateRunning      = "running"
	StateDegraded     = "degraded"
	StateStopping     = "stopping"
	StateStopped      = "stopped"
	StateUnavailable  = "storage_unavailable"
	StateConfigError  = "config_error"
)

// MemoBytes is the masking memo budget, part of the plugin's memory budgets.
const MemoBytes = 48 << 20

// Plugin is one native plugin instance.
type Plugin struct {
	engine *hooks.Engine
	host   labels.Host

	lifecycle sync.Mutex
	mu        sync.RWMutex
	cfg       config.Config
	cfgSet    bool
	rt        *runtimeState
	retry     *openRetry // background outbox open; read and written under lifecycle
	state     string
	reason    string
	terminal  bool

	defaultDataDir string
	bootID         string
	startedAt      time.Time
	hostSchema     atomic.Uint32
	hbN            atomic.Uint64

	snap atomic.Pointer[snapshot]
	jobs jobQueue
	now  func() time.Time
}

// runtimeState is one started configuration.
type runtimeState struct {
	cfg      config.Config
	secrets  config.Secrets
	ob       *outbox.Outbox
	masker   *capture.MaskerHolder
	capture  *capture.Runtime
	up       *uploader.Uploader
	labels   *labels.Cache
	alerter  *pushalert.Alerter
	prefix   *capture.PrefixMap
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	localFPs [][32]byte
	exactRev atomic.Uint32
}

// New returns a plugin instance. It captures the working directory only: init must not create
// files or start network activity, because CPA's preflight probe runs it in a throwaway process.
func New(host labels.Host) *Plugin {
	p := &Plugin{engine: hooks.NewEngine(), host: host, state: StateUnconfigured, reason: "not_registered", now: time.Now}
	if cwd, err := os.Getwd(); err == nil {
		p.defaultDataDir = filepath.Join(cwd, "plugins", "data", ID)
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	p.bootID = hex.EncodeToString(b)
	p.startedAt = time.Now().UTC()
	return p
}

// Engine exposes the hook engine (tests).
func (p *Plugin) Engine() *hooks.Engine { return p.engine }

// IsHook reports whether method is a hook served by the engine: such calls always return a
// pre-encoded OK, even when the handler panics (abi.go).
func IsHook(method string) bool {
	switch method {
	case protocol.MethodRequestInterceptBefore, protocol.MethodRequestInterceptAfter, protocol.MethodResponseInterceptAfter,
		protocol.MethodResponseInterceptStreamChunk, protocol.MethodRequestComplete, protocol.MethodUsageHandle:
		return true
	}
	return false
}

// Hook serves one hook call; it never fails.
func (p *Plugin) Hook(method string, raw []byte) []byte {
	e := p.engine
	switch method {
	case protocol.MethodRequestInterceptBefore:
		return e.BeforeAuth(raw)
	case protocol.MethodRequestInterceptAfter:
		return e.AfterAuth(raw)
	case protocol.MethodResponseInterceptAfter:
		return e.Response(raw)
	case protocol.MethodResponseInterceptStreamChunk:
		return e.StreamChunk(raw)
	case protocol.MethodRequestComplete:
		return e.Complete(raw)
	case protocol.MethodUsageHandle:
		return e.UsageHook(raw)
	}
	return hooks.OK
}

// HookPanicked records a panic the ABI recovered inside a hook (name and type only).
func (p *Plugin) HookPanicked(method, typ string) {
	p.engine.Stats.Panic(method, typ)
}

// Handle serves lifecycle and management methods.
func (p *Plugin) Handle(method string, raw []byte) (any, error) {
	switch method {
	case protocol.MethodPluginRegister, protocol.MethodPluginReconfigure:
		return p.configure(raw)
	case protocol.MethodPluginQuiesce:
		p.stop(false)
		return struct{}{}, nil
	case protocol.MethodPluginShutdown:
		p.Shutdown()
		return struct{}{}, nil
	case protocol.MethodManagementRegister:
		return managementRegistration(), nil
	case protocol.MethodManagementHandle:
		return p.management(raw), nil
	}
	return nil, errors.New("unknown method")
}

func registration() protocol.Registration {
	return protocol.Registration{
		SchemaVersion: protocol.SchemaVersion,
		Metadata: protocol.Metadata{Name: "Chat Archive", Version: Version, Author: "NoorChasib",
			GitHubRepository: "https://github.com/NoorChasib/cpa-plugins", ConfigFields: configFields()},
		Capabilities: protocol.Capabilities{RequestInterceptor: true, RequestInterceptorOmitBodyAfter: true, ResponseInterceptor: true,
			ResponseStreamInterceptor: true, RequestLifecyclePlugin: true, UsagePlugin: true, ManagementAPI: true},
	}
}

func configFields() []protocol.ConfigField {
	f := func(name, typ, desc string) protocol.ConfigField {
		return protocol.ConfigField{Name: name, Type: typ, Description: desc}
	}
	return []protocol.ConfigField{
		f("archive-url", "string", "Archive base URL, http(s)://host:port without path, userinfo or query."),
		f("source-token-env", "string", "Name of the environment variable holding the archive source token."),
		f("source-token-file", "string", "Absolute path of a 0600 file holding the archive source token (instead of -env)."),
		f("hmac-key-id", "string", "HMAC key id, default k1."),
		f("hmac-key-env", "string", "Name of the environment variable holding the HMAC key (at least 32 bytes)."),
		f("hmac-key-file", "string", "Absolute path of a 0600 file holding the HMAC key (instead of -env)."),
		f("exact-fingerprints", "array", "ExactFP values (64 hex) of extra secrets to mask exactly."),
		f("account-labels", "object", "Map of AuthIndex or account fingerprint to a display label."),
		f("data-dir", "string", "Outbox directory; default plugins/data/chat-archive under CPA's working directory."),
		f("outbox-cap-bytes", "integer", "Outbox size cap, default 20 GiB."),
		f("min-free-bytes", "integer", "Free-space guard in bytes, default 10 GiB."),
		f("min-free-percent", "integer", "Free-space guard in percent of the filesystem, default 5."),
		f("replay-retention-hours", "integer", "Acked envelopes kept for replay, 24-720, default 168."),
		f("memory-limit-mib", "integer", "Plugin runtime memory limit, 128-4096, default 768."),
		f("workers", "integer", "Capture workers, 1-4, default 2."),
		f("max-rpc-mib", "integer", "Largest body-carrying hook call captured, 8-512, default 160."),
		{Name: "capture", Type: "enum", EnumValues: []string{"full", "metadata", "off"}, Description: "Capture mode; off is the kill switch."},
		f("pushover-app-token-env", "string", "Optional Pushover app token environment variable."),
		f("pushover-app-token-file", "string", "Optional Pushover app token file."),
		f("pushover-user-key-env", "string", "Optional Pushover user key environment variable."),
		f("pushover-user-key-file", "string", "Optional Pushover user key file."),
		f("alert-backlog-hours", "integer", "Alert when the oldest pending envelope is older, 1-168, default 6."),
	}
}

// configure handles plugin.register and plugin.reconfigure. Config errors are the
// only errors returned; they never echo values.
func (p *Plugin) configure(raw []byte) (protocol.Registration, error) {
	var req protocol.LifecycleRequest
	if json.Unmarshal(raw, &req) != nil || req.SchemaVersion < protocol.SchemaVersion {
		return protocol.Registration{}, errors.New("schema 7 lifecycle request required")
	}
	cfg, err := config.Parse(req.ConfigYAML)
	if err != nil {
		return protocol.Registration{}, err
	}
	p.hostSchema.Store(req.SchemaVersion)
	p.lifecycle.Lock()
	defer p.lifecycle.Unlock()
	p.mu.RLock()
	terminal, prior, set, running := p.terminal, p.cfg, p.cfgSet, p.rt != nil
	p.mu.RUnlock()
	if terminal {
		return protocol.Registration{}, errors.New("plugin is shut down")
	}
	if set && cfg == prior && (running || p.currentState() == StateUnconfigured || p.retry != nil) {
		// Identical config: idempotent, also while the background outbox open retries (CPA saves
		// its config often; each save must not restart the retry with a synchronous open).
		return registration(), nil
	}
	if running {
		p.stopRuntime()
	}
	p.mu.Lock()
	p.cfg, p.cfgSet = cfg, true
	p.mu.Unlock()
	debug.SetMemoryLimit(int64(cfg.MemoryLimitMiB) << 20)
	p.startRuntime(cfg)
	return registration(), nil
}

func (p *Plugin) currentState() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state
}

func (p *Plugin) setState(state, reason string) {
	p.mu.Lock()
	p.state, p.reason = state, reason
	p.mu.Unlock()
}

// startRuntime starts capture for cfg. Without archive-url, token or key the plugin stays
// unconfigured: hooks pass through (still clearing X-Archive-*) and nothing is captured.
func (p *Plugin) startRuntime(cfg config.Config) {
	e := p.engine
	p.cancelOpenRetry()
	if ok, why := cfg.Configured(); !ok || cfg.Capture == config.CaptureOff {
		if ok {
			why = "capture_off"
		}
		e.SetConfig(&hooks.Config{Capture: config.CaptureOff, MaxRPC: cfg.MaxRPCMiB << 20})
		p.setState(StateUnconfigured, why)
		return
	}
	secrets, err := cfg.Resolve()
	if err != nil {
		e.SetConfig(&hooks.Config{Capture: config.CaptureOff, MaxRPC: cfg.MaxRPCMiB << 20})
		p.setState(StateConfigError, "secret_unavailable")
		return
	}
	dir := cfg.DataDir
	if dir == "" {
		dir = p.defaultDataDir
	}
	if dir == "" {
		p.setState(StateUnavailable, "working_directory_unavailable")
		return
	}
	opts := outbox.Options{Dir: dir, CapBytes: cfg.OutboxCapBytes, MinFreeBytes: cfg.MinFreeBytes,
		MinFreePercent: cfg.MinFreePercent, Retention: time.Duration(cfg.ReplayRetentionHours) * time.Hour}
	ob, err := outbox.Open(opts)
	if err != nil {
		// Capture stays off (hooks pass through) while a background loop retries the open, so
		// one failed open never leaves the plugin unavailable until the next config change.
		e.SetConfig(&hooks.Config{Capture: config.CaptureOff, MaxRPC: cfg.MaxRPCMiB << 20})
		p.setState(StateUnavailable, "outbox_unavailable")
		p.startOpenRetry(cfg, secrets, opts)
		return
	}
	p.startWith(cfg, secrets, ob)
}

// Outbox open retry: after a failed open the plugin stays storage_unavailable with hooks passing
// through, and retries in the background (off the lifecycle path) with this backoff between
// attempts and this bound on one attempt.
var (
	OpenRetryMin     = 5 * time.Second
	OpenRetryMax     = 5 * time.Minute
	OpenRetryTimeout = 10 * time.Minute
)

// openRetry is one background outbox-open loop.
type openRetry struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startOpenRetry starts the background open loop. The caller holds p.lifecycle. On success the
// loop takes p.lifecycle (without ever blocking a lifecycle call that wants to cancel it) and
// finishes the start with the opened outbox.
func (p *Plugin) startOpenRetry(cfg config.Config, secrets config.Secrets, opts outbox.Options) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &openRetry{cancel: cancel, done: make(chan struct{})}
	p.retry = r
	go func() {
		defer close(r.done)
		safego.Run("outbox_open", p.engine.Stats.Panic, func() {
			delay := OpenRetryMin
			for {
				t := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					t.Stop()
					return
				case <-t.C:
				}
				delay = min(delay*2, OpenRetryMax)
				octx, ocancel := context.WithTimeout(ctx, OpenRetryTimeout)
				ob, err := outbox.OpenContext(octx, opts)
				ocancel()
				if err != nil {
					continue
				}
				for !p.lifecycle.TryLock() {
					if ctx.Err() != nil {
						ob.Close()
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
				if ctx.Err() != nil || p.retry != r {
					p.lifecycle.Unlock()
					ob.Close()
					return
				}
				p.retry = nil
				p.startWith(cfg, secrets, ob)
				p.lifecycle.Unlock()
				return
			}
		})
	}()
}

// cancelOpenRetry stops the background open loop and waits until it released any outbox it
// opened. The caller holds p.lifecycle; the loop never blocks on it while cancelled.
func (p *Plugin) cancelOpenRetry() {
	r := p.retry
	if r == nil {
		return
	}
	p.retry = nil
	r.cancel()
	<-r.done
}

// startWith finishes a start with an open outbox.
func (p *Plugin) startWith(cfg config.Config, secrets config.Secrets, ob *outbox.Outbox) {
	e := p.engine
	rs := &runtimeState{cfg: cfg, secrets: secrets, ob: ob, prefix: capture.NewPrefixMap(), labels: labels.New(cfg.Labels())}
	rs.localFPs = append(rs.localFPs, fingerprint.ExactFP(secrets.SourceToken), fingerprint.ExactFP(string(secrets.HMACKey)))
	for _, h := range cfg.ExactFPs() {
		if x, err := wire.ParseHash(h); err == nil {
			rs.localFPs = append(rs.localFPs, x)
		}
	}
	m, err := mask.New(mask.Config{ExactFPs: rs.localFPs})
	if err != nil {
		ob.Close()
		p.setState(StateConfigError, "masker_unavailable")
		return
	}
	rs.masker = capture.NewMaskerHolder(m, 0, MemoBytes)
	rs.up = uploader.New(uploader.Options{
		BaseURL: cfg.ArchiveURL, Token: secrets.SourceToken, UserAgent: "chat-archive/" + Version, Outbox: ob, Stats: e.Stats,
		OnExact: func(set wire.ExactSet) { p.applyExact(rs, set) }, OnDead: rs.prefix.Dead, OnAck: rs.prefix.Acked,
		Heartbeat: func(state string) wire.Heartbeat { return p.heartbeat(rs, state) },
	})
	rs.capture = capture.Start(capture.Options{
		Engine: e, Outbox: ob, Workers: cfg.Workers, HMACKeyID: cfg.HMACKeyID, HMACKey: secrets.HMACKey, Masker: rs.masker,
		Limits: rs.up.Limits, Media: rs.up.Media(), Labels: rs.labels, Notify: rs.up.Notify, Prefix: rs.prefix,
	})
	e.SetConfig(&hooks.Config{Configured: true, Capture: cfg.Capture, MaxRPC: cfg.MaxRPCMiB << 20, HMACKeyID: cfg.HMACKeyID,
		HMACKey: secrets.HMACKey, Labels: rs.labels})
	if a, ok := pushalert.New(secrets.PushoverApp, secrets.PushoverUser, cfg.AlertBacklogHours); ok {
		rs.alerter = a
	}
	rs.ctx, rs.cancel = context.WithCancel(context.Background())
	e.Start()
	rs.up.Start()
	rec := e.Stats.Panic
	safego.Loop(rs.ctx, &rs.wg, "maintenance", rec, func(ctx context.Context) { p.maintenance(ctx, rs) })
	safego.Loop(rs.ctx, &rs.wg, "labels", rec, func(ctx context.Context) { rs.labels.Run(ctx, p.host) })
	safego.Loop(rs.ctx, &rs.wg, "push_alert", rec, func(ctx context.Context) { p.pushLoop(ctx, rs) })
	p.mu.Lock()
	p.rt = rs
	p.state, p.reason = StateRunning, ""
	p.mu.Unlock()
	p.refreshSnapshot(rs)
}

// applyExact rebuilds the masker from the server's exact set plus the local fingerprints.
func (p *Plugin) applyExact(rs *runtimeState, set wire.ExactSet) {
	if cur := rs.masker.Get(); cur != nil && cur.Rev == set.Rev && rs.exactRev.Load() == set.Rev && set.Rev != 0 {
		return
	}
	cfg := mask.Config{ExactRev: set.Rev}
	cfg.ExactFPs = append(cfg.ExactFPs, rs.localFPs...)
	for _, h := range set.FPs {
		cfg.ExactFPs = append(cfg.ExactFPs, h)
	}
	for _, h := range set.CallerFPs {
		cfg.CallerFPs = append(cfg.CallerFPs, h)
	}
	m, err := mask.New(cfg)
	if err != nil {
		p.engine.Stats.SetFault("masker_rebuild", true)
		return
	}
	p.engine.Stats.SetFault("masker_rebuild", false)
	rs.masker.Set(m, set.Rev)
	rs.exactRev.Store(set.Rev)
}

// stopRuntime is the graceful stop: admission stopping, uploader and media
// sender stopped within their deadline, background loops cancelled, workers drained (≤ 10 s),
// the writer flushed, the outbox closed.
func (p *Plugin) stopRuntime() {
	p.mu.RLock()
	rs := p.rt
	p.mu.RUnlock()
	if rs == nil {
		return
	}
	p.setState(StateStopping, "")
	p.engine.BeginStop()
	rs.up.Stop()
	rs.cancel()
	rs.wg.Wait()
	rs.capture.Stop()
	p.refreshSnapshot(rs)
	_ = rs.ob.Close()
	p.engine.Stopped()
	p.mu.Lock()
	p.rt = nil
	p.state, p.reason = StateStopped, ""
	p.mu.Unlock()
}

func (p *Plugin) stop(final bool) {
	p.lifecycle.Lock()
	defer p.lifecycle.Unlock()
	p.cancelOpenRetry()
	p.stopRuntime()
	if final {
		p.mu.Lock()
		p.terminal = true
		p.mu.Unlock()
		p.engine.SetConfig(&hooks.Config{Capture: config.CaptureOff, MaxRPC: protocol.MaxRPCDefault})
	}
}

// Shutdown is the C shutdown: final stop. The ABI then waits for host calls and clears the host.
func (p *Plugin) Shutdown() { p.stop(true) }

// Runtime returns the running outbox and uploader (tests); nil when stopped.
func (p *Plugin) Runtime() (*outbox.Outbox, *uploader.Uploader, *capture.Runtime) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.rt == nil {
		return nil, nil, nil
	}
	return p.rt.ob, p.rt.up, p.rt.capture
}

// State returns the plugin state and reason.
func (p *Plugin) State() (string, string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state, p.reason
}

func sprint(format string, a ...any) string { return fmt.Sprintf(format, a...) }
