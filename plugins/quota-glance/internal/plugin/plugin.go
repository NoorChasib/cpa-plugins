// Package plugin wires the pieces together: read the snapshot, build the
// document, publish it, and serve.
//
// Nothing this plugin does on a schedule contacts a provider, and it makes zero
// CPA management-API calls. quota-cache owns the polling schedule and remains
// the only component that polls a provider; a second poller would compete for
// the same rate limits. Identity comes from an in-process host callback, which
// costs nothing and needs no key.
//
// The single exception is redemption, in internal/redeem: spending a banked
// Codex or Claude rate-limit reset is a write, it cannot come out of a
// snapshot, and it happens only when the operator presses the button and
// confirms. It runs on the request goroutine of that press — the redeem POST,
// or the spend GET that carries the same press for a reader signed in with the
// web token — and on no timer, so the rebuild path below is as free of provider
// contact as it ever was. Set allow-redeem to false and the capability is never
// constructed.
//
// The dashboard also saves settings — an API credit's amount, refill date and
// Console reading, and a Claude renewal date — to settings.json in data-dir,
// through internal/overrides. That writes one local file and contacts nothing.
// Set allow-edit to false and the saver is never installed; stored values still
// apply.
package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/aggregate"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/api"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/overrides"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/redeem"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/source"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/store"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/watch"
	"gopkg.in/yaml.v3"
)

const ID = "quota-glance"

var Version = "0.7.0"

const (
	defaultStaleAfter = 45 * time.Minute
	defaultDataDir    = "plugins/data/quota-glance"
	tokenFile         = "web-token"
	// defaultWarnBelow is the OpenRouter balance, in dollars, below which its
	// card turns amber.
	defaultWarnBelow = 5.0
)

type Host interface {
	ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error)
	Log(context.Context, string, string, map[string]any)
	// GetAuth and HTTPDo are used only by the redeem path. GetAuth returns the
	// physical credential document, OAuth tokens included; it is decoded for
	// the one or two fields a provider request needs and never logged or
	// persisted.
	GetAuth(context.Context, string) ([]byte, error)
	HTTPDo(context.Context, protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error)
	// ResetCooldown is used only by the redeem path, after a provider
	// confirmed a reset, to clear CPA's routing cooldown on that credential.
	ResetCooldown(context.Context, string) (protocol.HostRoutingResetCooldownResponse, error)
}

type settings struct {
	cachePath  string
	dataDir    string
	staleAfter time.Duration
	planLabels map[string]string
	// allowRedeem gates the whole redeem path. When false the redeemer is never
	// built and the route 404s, so the plugin cannot reach a provider at all.
	allowRedeem bool
	// allowEdit gates the two settings doors the same way. What is stored
	// applies either way.
	allowEdit bool
	warnBelow float64
}

// dollars is a money amount from configuration. The configuration panel saves
// a number field as a YAML int or float depending on what was typed, and a
// hand-edited file may quote it or keep the dollar sign, so all of those
// decode. Anything else is recorded as invalid rather than failing the decode,
// so configure can name the field instead of rejecting the whole block.
type dollars struct {
	value   float64
	set     bool
	invalid bool
}

func (d *dollars) UnmarshalYAML(node *yaml.Node) error {
	// A key saved with no value is the panel clearing the field.
	if node.Kind == yaml.ScalarNode && (node.Tag == "!!null" || strings.TrimSpace(node.Value) == "") {
		return nil
	}
	d.set = true
	text := strings.TrimPrefix(strings.TrimSpace(node.Value), "$")
	value, err := strconv.ParseFloat(text, 64)
	if node.Kind != yaml.ScalarNode || err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		d.invalid = true
		return nil
	}
	d.value = value
	return nil
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
	// overrides is settings.json. It is kept across reconfigures, so its write
	// limit and its values outlive a saved panel, and reopened when data-dir
	// moves or when the file it holds could not be read: moving that file
	// aside and saving the configuration is how editing comes back.
	overrides *overrides.Store

	// meter is the API meter the last rebuild read successfully. A save reads
	// it, without a lock, for a new Console reading's baseline.
	meter atomic.Pointer[qc.APIMeter]

	// rebuildMu serializes Rebuild end to end, from reading its inputs to
	// publishing, so documents are published, and history appended, in the
	// order their inputs were read. Rebuild has two callers, the watcher and a
	// committed save; without it, a rebuild that read the settings before a
	// save could publish after the save's own rebuild and undo it until the
	// next one. Nothing that holds the store's lock, configMu or stateMu ever
	// takes it, and Rebuild never takes mu, so it cannot deadlock a close.
	rebuildMu sync.Mutex
	// afterRead, when set, runs inside Rebuild once its inputs are read and
	// before it publishes. Tests park a rebuild there; it is never set
	// otherwise.
	afterRead atomic.Pointer[func()]

	// lastGood is the most recent document built from a readable snapshot. It
	// is what gets re-served, marked stale, when the snapshot goes away: an
	// empty response is indistinguishable from a broken install. goodInput is
	// the snapshot and roster it was built from, so the parts the dashboard's
	// settings decide can be rebuilt over it while it is re-served.
	stateMu    sync.Mutex
	lastGood   *aggregate.Document
	goodInput  aggregate.Input
	lastError  string
	builtAt    time.Time
	written    time.Time
	nextReq    time.Time
	meterError string
	flushedAt  time.Time

	// redeemer is built the first time redemption is configured on and kept
	// for the life of this Plugin, guarded by mu. Its journal holds the claims
	// whose outcome is unknown, and a new redeemer per reconfigure would forget
	// them: the press after saving an unrelated setting would make a fresh
	// claim where it should have repeated the unresolved one, and that is the
	// press that can spend a second reset. That is all it outlives, though, not
	// the process: CPA applies an update or a switch off and on by loading a
	// fresh copy of the plugin, and the new Plugin starts with an empty journal.
	redeemer *redeem.Redeemer
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
				// The one management route that changes anything; /spend below
				// is the resource tree's. It spends a banked Codex or Claude
				// rate-limit reset, which is irreversible, and it refuses any
				// body that does not carry an explicit confirmation.
				{Method: "POST", Path: "/plugins/" + ID + "/redeem", Description: "Spend one banked Codex or Claude rate-limit reset for a credential"},
				// Writes settings.json in data-dir and nothing else. Like
				// /redeem, CPA's middleware has checked the management key,
				// which it takes only in a header, before it is dispatched.
				{Method: "POST", Path: "/plugins/" + ID + "/settings", Description: "Save API credit amounts, refill dates, Console readings and Claude renewal dates set on the dashboard"},
			},
			// The page, the document down its fallback path, and the press
			// for readers arriving without a console session. CPA
			// authenticates none of them — the page carries no data, and
			// everything else here carries this plugin's own token check.
			Resources: []protocol.ResourceRoute{
				{Path: "/app", Menu: "Quota Glance", Description: "Remaining quota across every credential and window"},
				{Path: "/summary", Description: "Aggregated quota document; requires the plugin web token"},
				// Registered for the reader who has no console session, and
				// carrying the same token check as /summary. CPA v8.0.15
				// dispatches only GET to a resource route
				// (pluginhost/management.go:295-297), so it never reaches
				// this; /spend carries the press meanwhile. It stays because
				// it is where the press belongs once CPA dispatches a POST
				// here, and the smoke reports whether it does.
				{Path: "/redeem", Description: "Spend one banked Codex or Claude rate-limit reset; requires the plugin web token"},
				// The same press as /redeem, as a GET, because that is all CPA
				// dispatches here. Nothing selecting the action is in the URL:
				// the token, the confirmation and a single-use press id all
				// travel in headers. See api.spendResponse.
				{Path: "/spend", Description: "Spend one banked Codex or Claude rate-limit reset by GET for a reader signed in with the web token; CPA dispatches only GET to resource routes"},
				// The same save as POST .../settings, as a GET, for the same
				// reason as /spend, and fenced the same way: the batch and
				// the token travel in headers, never in the URL.
				{Path: "/save-settings", Description: "Save dashboard settings by GET for a reader signed in with the web token; CPA dispatches only GET to resource routes"},
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
		Enabled     *bool             `yaml:"enabled"`
		CachePath   string            `yaml:"cache-path"`
		DataDir     string            `yaml:"data-dir"`
		WebToken    string            `yaml:"web-token"`
		StaleAfter  string            `yaml:"stale-after"`
		PlanLabels  map[string]string `yaml:"plan-labels"`
		AllowRedeem *bool             `yaml:"allow-redeem"`
		AllowEdit   *bool             `yaml:"allow-edit"`
		WarnBelow   dollars           `yaml:"openrouter-warn-below"`
		Priority    *int              `yaml:"priority"`
		Store       map[string]any    `yaml:"store"`
	}{CachePath: qc.DefaultPath, DataDir: defaultDataDir, StaleAfter: defaultStaleAfter.String()}
	if len(req.ConfigYAML) > 0 {
		decoder := yamlDecoder(req.ConfigYAML)
		if decoder.Decode(&cfg) != nil {
			return protocol.Registration{}, errors.New("invalid quota-glance configuration")
		}
	}
	if cfg.WarnBelow.invalid {
		return protocol.Registration{}, errors.New("openrouter-warn-below must be a dollar amount of 0 or more")
	}
	// Absent, or saved empty by the panel, means the default rather than zero:
	// zero is a real choice — warn only once the balance has run out.
	warnBelow := defaultWarnBelow
	if cfg.WarnBelow.set {
		warnBelow = cfg.WarnBelow.value
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
	p.configMu.RLock()
	kept := p.overrides
	p.configMu.RUnlock()
	if kept == nil || kept.Dir() != dataDir || kept.Current().Unreadable {
		kept = overrides.Open(dataDir)
	}
	p.configMu.Lock()
	p.store = current
	p.overrides = kept
	allowRedeem := cfg.AllowRedeem == nil || *cfg.AllowRedeem
	allowEdit := cfg.AllowEdit == nil || *cfg.AllowEdit
	p.settings = settings{
		cachePath: cachePath, dataDir: dataDir, staleAfter: staleAfter,
		planLabels: aggregate.NormalizePlanLabels(cfg.PlanLabels), allowRedeem: allowRedeem,
		allowEdit: allowEdit, warnBelow: warnBelow,
	}
	served := p.api
	p.configMu.Unlock()

	// Last, after everything that can fail. A rejected reconfigure must leave
	// the running configuration — including the token the operator's dashboard
	// may be using — exactly as it was.
	served.Enable()
	served.SetToken(token)
	// Nil when redemption is off, which closes the route rather than leaving it
	// open behind a flag: with no redeemer there is no code path from a request
	// to a provider. When it is on, it is the same redeemer every time, so an
	// unresolved claim outlives the reconfigure.
	if allowRedeem {
		if p.redeemer == nil {
			p.redeemer = redeem.New(hostRedeem{p.host})
		}
		served.SetRedeemer(p.redeemer)
	} else {
		served.SetRedeemer(nil)
	}
	// Nil when editing is off, which closes both settings doors the same way.
	if allowEdit {
		served.SetSaver(saver{p: p, store: kept})
	} else {
		served.SetSaver(nil)
	}
	if generated {
		// Logged once, when it is first minted, because the operator has no
		// other way to learn it. It is persisted, so a restart reuses it.
		p.log("warn", "quota-glance generated a fallback web token; set web-token in plugin configuration to choose your own", map[string]any{"web_token": token})
	}

	watcher, err := watch.Start(watch.Options{
		Path:      cachePath,
		MeterPath: qc.MeterPath(cachePath),
		OnChange:  p.Rebuild,
		Logf:      func(message string) { p.log("warn", message, nil) },
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
				{Name: "web-token", Type: "string", Description: "Dashboard password for browsers without a CPA console session; also spends banked resets while allow-redeem is on; generated and logged once if empty"},
				{Name: "stale-after", Type: "string", Description: "Age at which an observation is shown as stale; default 45m"},
				{Name: "plan-labels", Type: "object", Description: "Overrides for plan display names, keyed by the provider-reported value"},
				{Name: "allow-redeem", Type: "boolean", Description: "Allow spending a banked Codex or Claude rate-limit reset from the dashboard; default true. Set false to show the count without a button; governs both the console and the dashboard-password doors"},
				{Name: "allow-edit", Type: "boolean", Description: "Allow setting API credit amounts, refill dates, Console readings and Claude renewal dates from the dashboard; default true. Values are kept in data-dir."},
				{Name: "openrouter-warn-below", Type: "number", Description: "OpenRouter balance in dollars below which its card turns amber; default 5. It turns red at $0. Needs an OpenRouter management key in Quota Cache"},
			},
		},
		Capabilities: protocol.RegistrationCapabilities{ManagementAPI: true},
	}
}

// Rebuild reads the snapshot and republishes. It runs on the watcher's
// goroutine, on the startup read, and after a committed save, on the request
// goroutine that made it; rebuildMu takes them one at a time.
func (p *Plugin) Rebuild() {
	p.rebuildMu.Lock()
	defer p.rebuildMu.Unlock()
	p.configMu.RLock()
	settings, current, kept, watcher, served := p.settings, p.store, p.overrides, p.watcher, p.api
	p.configMu.RUnlock()
	if settings.cachePath == "" || current == nil || kept == nil {
		return
	}
	now := time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result := source.Read(ctx, hostSource{p.host}, settings.cachePath)
	if result.Meter != nil {
		p.meter.Store(result.Meter)
	}
	// Read before stateMu, never under it: the store's lock is a leaf, and a
	// save holding it must never wait for this rebuild.
	values := kept.Current()
	if hook := p.afterRead.Load(); hook != nil {
		(*hook)()
	}
	in := aggregate.Input{
		Snapshot:     result.Snapshot,
		SourceReason: result.Reason,
		Identities:   result.Identities,
		StaleAfter:   settings.staleAfter,
		PlanLabels:   settings.planLabels,
		Redeemable:   settings.allowRedeem,
		// Read on every rebuild, so a threshold changed in the panel
		// applies at the next one rather than at the next snapshot write.
		BalanceWarnBelow: settings.warnBelow,
		Meter:            result.Meter,
		Overrides:        values,
		AllowEdit:        settings.allowEdit,
	}

	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	var historyErr error
	var doc aggregate.Document
	switch {
	case result.Reason != "" && p.lastGood != nil:
		// Serve stale over empty: the last good document, honestly labelled.
		// What the dashboard's settings decide in it is rebuilt from the
		// snapshot and roster it came from, with the settings and meter as
		// they are now: a save while the source is failing must show, or the
		// page would keep the old values and its next save would conflict.
		settled := in
		settled.Snapshot, settled.SourceReason, settled.Identities = p.goodInput.Snapshot, "", p.goodInput.Identities
		doc = aggregate.WithSettings(*p.lastGood, aggregate.Build(settled, now))
		doc.GeneratedAtEpoch = now.Unix()
		doc.Stale = true
		reason := result.Reason
		doc.StaleReason = &reason
	default:
		in.Samples = current.Samples()
		doc = aggregate.Build(in, now)
		if result.Reason == "" {
			good := doc
			p.lastGood = &good
			p.goodInput = aggregate.Input{Snapshot: result.Snapshot, Identities: result.Identities}
			// History records observations, and a rebuild is not one. Rebuilds
			// now run on a timer as well as on a write — request activity moves
			// while the snapshot sits still — and sampling each of those would
			// store the same reading sixty times an hour, inflate the ring
			// fifteenfold, and change nothing about the trend it feeds.
			fresh := !result.Snapshot.WrittenAt.Equal(p.written)
			p.written, p.nextReq = result.Snapshot.WrittenAt, result.Snapshot.NextRequest
			if fresh {
				historyErr = current.Append(aggregate.SamplesFrom(doc, now), now)
			}
		}
	}
	p.builtAt = now
	p.meterError = result.MeterError
	if result.Meter != nil {
		p.flushedAt = result.Meter.FlushedAt
	}
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
		Settings: api.SettingsHealth{
			Path: kept.Path(), Revision: values.Revision,
			APICreditEntries: len(values.APICredits), RenewalEntries: len(values.Renewals),
			LastError: kept.LastError(),
		},
		Meter: api.MeterHealth{Path: qc.MeterPath(settings.cachePath), FlushedAt: p.flushedAt, LastError: p.meterError},
	}
	if watcher != nil {
		state := watcher.State()
		health.Watcher = api.WatcherState{
			Watching: state.Watching, Directory: state.Directory,
			LastEvent: state.LastEvent, LastError: state.LastError,
			Reloads: state.Reloads, Backstops: state.Backstops,
			Heartbeats: state.Heartbeats,
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

// hostRedeem adapts the plugin host to the three callbacks the redeem path needs,
// and to nothing else. Passing the whole host would hand the provider client
// ListAuth as well, which it has no business calling.
type hostRedeem struct{ host Host }

func (h hostRedeem) GetAuth(ctx context.Context, authIndex string) ([]byte, error) {
	if h.host == nil {
		return nil, errors.New("host unavailable")
	}
	return h.host.GetAuth(ctx, authIndex)
}

func (h hostRedeem) HTTPDo(ctx context.Context, request protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	if h.host == nil {
		return protocol.HostHTTPResponse{}, errors.New("host unavailable")
	}
	return h.host.HTTPDo(ctx, request)
}

func (h hostRedeem) ResetCooldown(ctx context.Context, authIndex string) (protocol.HostRoutingResetCooldownResponse, error) {
	if h.host == nil {
		return protocol.HostRoutingResetCooldownResponse{}, errors.New("host unavailable")
	}
	return h.host.ResetCooldown(ctx, authIndex)
}

// saver is what the settings doors write through: the store, and the rebuild
// that makes a committed save the document the page reads next. The store
// releases its lock before Apply returns, so the rebuild below, and any
// watcher rebuild running beside it, never waits on a save.
type saver struct {
	p     *Plugin
	store *overrides.Store
}

func (s saver) Current() overrides.Values { return s.store.Current() }

func (s saver) Save(batch overrides.Batch, now time.Time) (overrides.Result, error) {
	result, err := s.store.Apply(batch, s.p.meter.Load(), now)
	if err != nil || result.Unchanged {
		return result, err
	}
	// Names only: amounts and dates are not secret, but they are not logged.
	s.p.log("info", "quota-glance settings changed", map[string]any{
		"kind": batch.Kind, "door": batch.Door, "ids": result.IDs, "fields": result.Fields,
	})
	s.p.Rebuild()
	return result, nil
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
