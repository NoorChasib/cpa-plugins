// Package plugin wires the pieces together: read the snapshot, build the
// document, publish it, and serve.
//
// This plugin makes zero provider requests and zero CPA management-API calls.
// quota-cache owns the polling schedule and is the only component that contacts
// a provider; a second poller would compete for the same rate limits. Identity
// comes from an in-process host callback, which costs nothing and needs no key.
package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/aggregate"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/api"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/source"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/store"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/watch"
	"gopkg.in/yaml.v3"
)

const ID = "quota-glance"

var Version = "0.1.3"

const (
	defaultStaleAfter = 45 * time.Minute
	defaultDataDir    = "plugins/data/quota-glance"
	tokenFile         = "web-token"
)

type Host interface {
	ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error)
	Log(context.Context, string, string, map[string]any)
}

type settings struct {
	cachePath  string
	dataDir    string
	staleAfter time.Duration
	planLabels map[string]string
}

type Plugin struct {
	// mu serializes lifecycle calls only. Rebuild never takes it: it runs on
	// the watcher's goroutine, and closing a watcher waits for that goroutine,
	// so a rebuild blocking on this lock while configure holds it across the
	// close would deadlock the plugin inside CPA's process.
	mu       sync.Mutex
	host     Host
	terminal bool
	notice   sync.Once

	// configMu guards what a rebuild reads. It is held only for the assignment
	// itself, never across a watcher close.
	configMu sync.RWMutex
	api      *api.API
	watcher  *watch.Watcher
	store    *store.Store
	settings settings

	// lastGood is the most recent document built from a readable snapshot. It
	// is what gets re-served, marked stale, when the snapshot goes away: an
	// empty response is indistinguishable from a broken install.
	stateMu   sync.Mutex
	lastGood  *aggregate.Document
	lastError string
	builtAt   time.Time
	written   time.Time
	nextReq   time.Time
}

func New(host Host) *Plugin { return &Plugin{host: host, api: api.New(ID, "")} }

func (p *Plugin) Handle(method string, raw []byte) (any, error) {
	switch method {
	case protocol.MethodPluginRegister, protocol.MethodPluginReconfigure:
		return p.configure(raw)
	case protocol.MethodPluginQuiesce:
		p.stop(false)
		return struct{}{}, nil
	case protocol.MethodManagementRegister:
		return protocol.ManagementRegistration{
			// Menu stays empty on management routes: CPA turns a GET carrying
			// Menu into a public resource, which would publish these.
			Routes: []protocol.ManagementRoute{
				{Method: "GET", Path: "/plugins/" + ID + "/summary", Description: "Aggregated quota document the dashboard renders"},
				{Method: "GET", Path: "/plugins/" + ID + "/health", Description: "Snapshot time, watcher state, and last error"},
				{Method: "GET", Path: "/plugins/" + ID + "/windows", Description: "Observed window keys and the credentials reporting them"},
			},
			// The page, and the document down its fallback path. CPA
			// authenticates neither — the page carries no data, and the
			// document behind /summary carries this plugin's own token check
			// for readers arriving without a console session.
			Resources: []protocol.ResourceRoute{
				{Path: "/app", Menu: "Quota Glance", Description: "Remaining quota across every credential and window"},
				{Path: "/summary", Description: "Aggregated quota document; requires the plugin web token"},
			},
		}, nil
	case protocol.MethodManagementHandle:
		var req protocol.ManagementRequest
		if json.Unmarshal(raw, &req) != nil {
			return nil, errors.New("invalid management request")
		}
		p.configMu.RLock()
		served := p.api
		p.configMu.RUnlock()
		return served.Handle(req, time.Now().UTC()), nil
	default:
		return nil, errors.New("unknown method")
	}
}

func (p *Plugin) configure(raw []byte) (protocol.Registration, error) {
	var req protocol.LifecycleRequest
	if json.Unmarshal(raw, &req) != nil || req.SchemaVersion < 4 {
		return protocol.Registration{}, errors.New("schema 4 or newer required")
	}
	cfg := struct {
		Enabled    *bool             `yaml:"enabled"`
		CachePath  string            `yaml:"cache-path"`
		DataDir    string            `yaml:"data-dir"`
		WebToken   string            `yaml:"web-token"`
		StaleAfter string            `yaml:"stale-after"`
		PlanLabels map[string]string `yaml:"plan-labels"`
		Priority   *int              `yaml:"priority"`
		Store      map[string]any    `yaml:"store"`
	}{CachePath: qc.DefaultPath, DataDir: defaultDataDir, StaleAfter: defaultStaleAfter.String()}
	if len(req.ConfigYAML) > 0 {
		decoder := yamlDecoder(req.ConfigYAML)
		if decoder.Decode(&cfg) != nil {
			return protocol.Registration{}, errors.New("invalid quota-glance configuration")
		}
	}
	staleAfter, err := time.ParseDuration(cfg.StaleAfter)
	if err != nil || staleAfter < time.Minute || staleAfter > 24*time.Hour {
		return protocol.Registration{}, errors.New("stale-after must be between 1m and 24h")
	}
	if cfg.CachePath == "" || cfg.DataDir == "" {
		return protocol.Registration{}, errors.New("cache-path and data-dir are required")
	}
	// Compare locations, not the spelling of a path: CPA can rewrite a relative
	// default as absolute when it saves user configuration.
	cachePath, err := filepath.Abs(cfg.CachePath)
	if err != nil {
		return protocol.Registration{}, errors.New("cache-path cannot be resolved")
	}
	dataDir, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return protocol.Registration{}, errors.New("data-dir cannot be resolved")
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.terminal {
		return protocol.Registration{}, errors.New("quota-glance shut down")
	}
	enabled := cfg.Enabled == nil || *cfg.Enabled
	if !enabled {
		p.stopWatcher()
		// Close the routes too. A disabled plugin that keeps serving its last
		// document is indistinguishable from one that is still running.
		p.configMu.RLock()
		served := p.api
		p.configMu.RUnlock()
		served.Disable()
		return registration(), nil
	}
	// Refuse plainly rather than serve an empty document that is
	// indistinguishable from a working install with no quota anywhere.
	if err := source.Readable(cachePath); err != nil {
		return protocol.Registration{}, errors.New("quota-cache snapshot is unreadable at cache-path; check that quota-cache is installed and that cache-path matches its own")
	}

	token, generated, err := resolveToken(cfg.WebToken, dataDir)
	if err != nil {
		return protocol.Registration{}, err
	}
	// Stop the old watcher before opening the store: otherwise two stores hold
	// the same history file for as long as the close takes, and whichever
	// writes last wins with a staler view.
	p.stopWatcher()
	current, err := store.Open(dataDir)
	if err != nil {
		return protocol.Registration{}, err
	}
	p.configMu.Lock()
	p.store = current
	p.settings = settings{
		cachePath: cachePath, dataDir: dataDir, staleAfter: staleAfter,
		planLabels: aggregate.NormalizePlanLabels(cfg.PlanLabels),
	}
	served := p.api
	p.configMu.Unlock()

	// Last, after everything that can fail. A rejected reconfigure must leave
	// the running configuration — including the token the operator's dashboard
	// may be using — exactly as it was.
	served.Enable()
	served.SetToken(token)
	if generated {
		// Logged once, when it is first minted, because the operator has no
		// other way to learn it. It is persisted, so a restart reuses it.
		p.log("warn", "quota-glance generated a fallback web token; set web-token in plugin configuration to choose your own", map[string]any{"web_token": token})
	}

	watcher, err := watch.Start(watch.Options{
		Path:     cachePath,
		OnChange: p.Rebuild,
		Logf:     func(message string) { p.log("warn", message, nil) },
	})
	if err != nil {
		return protocol.Registration{}, err
	}
	p.configMu.Lock()
	p.watcher = watcher
	p.configMu.Unlock()
	// Released only now that the watcher is reachable, so the first document
	// already reports the watcher state that health serves.
	watcher.Begin()
	// Resource routes are dispatched only while CPA's own home page is off. The
	// plugin cannot read that setting through any host callback, so this is a
	// note rather than a check: if GET .../app returns 404, this is why. Logged
	// once per process, not on every reconfigure — a notice that fires when
	// nothing is wrong teaches the operator to ignore it.
	p.notice.Do(func() {
		p.log("info", "quota-glance is serving; if the app route returns 404, CPA's home page is enabled and resource routes are disabled", nil)
	})
	return registration(), nil
}

func registration() protocol.Registration {
	return protocol.Registration{
		SchemaVersion: protocol.SchemaVersion,
		Metadata: protocol.Metadata{
			Name: ID, Version: Version, Author: "NoorChasib",
			GitHubRepository: "https://github.com/NoorChasib/cpa-plugins",
			ConfigFields: []protocol.ConfigField{
				{Name: "cache-path", Type: "string", Description: "quota-cache snapshot path; must match quota-cache's own"},
				{Name: "data-dir", Type: "string", Description: "Private directory for trend history"},
				{Name: "web-token", Type: "string", Description: "Fallback password for the dashboard when there is no CPA console session; generated and logged once if empty"},
				{Name: "stale-after", Type: "string", Description: "Age at which an observation is shown as stale; default 45m"},
				{Name: "plan-labels", Type: "object", Description: "Overrides for plan display names, keyed by the provider-reported value"},
			},
		},
		Capabilities: protocol.RegistrationCapabilities{ManagementAPI: true},
	}
}

// Rebuild reads the snapshot and republishes. It runs on the watcher's
// goroutine and on the startup read.
func (p *Plugin) Rebuild() {
	p.configMu.RLock()
	settings, current, watcher, served := p.settings, p.store, p.watcher, p.api
	p.configMu.RUnlock()
	if settings.cachePath == "" || current == nil {
		return
	}
	now := time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result := source.Read(ctx, hostSource{p.host}, settings.cachePath)

	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	var historyErr error
	var doc aggregate.Document
	switch {
	case result.Reason != "" && p.lastGood != nil:
		// Serve stale over empty: the last good document, honestly labelled.
		doc = *p.lastGood
		doc.GeneratedAtEpoch = now.Unix()
		doc.Stale = true
		reason := result.Reason
		doc.StaleReason = &reason
	default:
		doc = aggregate.Build(aggregate.Input{
			Snapshot:     result.Snapshot,
			SourceReason: result.Reason,
			Identities:   result.Identities,
			Samples:      current.Samples(),
			StaleAfter:   settings.staleAfter,
			PlanLabels:   settings.planLabels,
		}, now)
		if result.Reason == "" {
			good := doc
			p.lastGood = &good
			p.written, p.nextReq = result.Snapshot.WrittenAt, result.Snapshot.NextRequest
			historyErr = current.Append(aggregate.SamplesFrom(doc, now), now)
		}
	}
	p.builtAt = now
	switch {
	case result.Reason != "":
		p.lastError = result.Reason
	case historyErr != nil:
		// Losing history costs the trend arrows, not the numbers — but it has
		// to be visible somewhere, and health is the only place it can appear.
		p.lastError = historyErr.Error()
	default:
		p.lastError = ""
	}

	health := api.Health{
		Version:             Version,
		CachePath:           settings.cachePath,
		StaleAfter:          settings.staleAfter.String(),
		SnapshotWrittenAt:   p.written,
		SnapshotNextRequest: p.nextReq,
		BuiltAt:             p.builtAt,
		LastError:           p.lastError,
	}
	if watcher != nil {
		state := watcher.State()
		health.Watcher = api.WatcherState{
			Watching: state.Watching, Directory: state.Directory,
			LastEvent: state.LastEvent, LastError: state.LastError,
			Reloads: state.Reloads, Backstops: state.Backstops,
		}
	}
	served.Publish(doc, health)
}

func (p *Plugin) stop(final bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if final {
		p.terminal = true
	}
	p.stopWatcher()
}

// stopWatcher detaches the watcher before closing it, so configMu is never held
// while waiting for the watcher goroutine to finish a rebuild.
func (p *Plugin) stopWatcher() {
	p.configMu.Lock()
	watcher := p.watcher
	p.watcher = nil
	p.configMu.Unlock()
	if watcher != nil {
		watcher.Close()
	}
}

func (p *Plugin) Shutdown() { p.stop(true) }

// log never receives a snapshot body, a credential, or the web token, with the
// single exception of the one-time generated-token message above.
func (p *Plugin) log(level, message string, fields map[string]any) {
	if p.host == nil {
		return
	}
	p.host.Log(context.Background(), level, message, fields)
}

// resolveToken returns the configured token, or a generated one persisted under
// data-dir.
//
// Persisting matters: the spec says generate and log once, and a token minted
// fresh on every start would silently invalidate a bookmarked dashboard URL
// each time CPA restarts. An unreadable or malformed stored token is replaced
// rather than treated as fatal.
func resolveToken(configured, dataDir string) (token string, generated bool, err error) {
	if configured != "" {
		return configured, false, nil
	}
	// Own the directory rather than depending on the store having been opened
	// first: a token that fails to persist is silently regenerated on the next
	// start, which is exactly the failure this function exists to prevent.
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", false, errors.New("quota-glance data directory cannot be created")
	}
	path := filepath.Join(dataDir, tokenFile)
	if raw, readErr := os.ReadFile(path); readErr == nil {
		if stored := strings.TrimSpace(string(raw)); stored != "" && len(stored) <= 512 {
			return stored, false, nil
		}
	}
	token, err = api.NewToken()
	if err != nil {
		return "", false, errors.New("web token cannot be generated")
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		// A token that cannot be persisted would be reminted on every
		// reconfigure, invalidating the operator's dashboard each time and
		// writing a new secret to the log. Refuse instead, naming the cause.
		return "", false, errors.New("web token cannot be persisted; set web-token in configuration or make data-dir writable")
	}
	return token, true, nil
}

// hostSource adapts the plugin host to the narrower callback the source needs.
type hostSource struct{ host Host }

func (h hostSource) ListAuth(ctx context.Context) ([]protocol.HostAuthFileEntry, error) {
	if h.host == nil {
		return nil, errors.New("host unavailable")
	}
	return h.host.ListAuth(ctx)
}

func yamlDecoder(raw []byte) *yaml.Decoder {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	return decoder
}
