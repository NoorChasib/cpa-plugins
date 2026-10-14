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
// After a press that spent, or may have spent, a reset, it asks Quota Cache to
// poll that account now, then every other one, so the card catches up as soon
// as Quota Cache can poll, usually within a minute. CPA gives plugins no way
// to call one another, so the request is a small file beside the snapshot,
// and it is the one file in Quota Cache's directory this plugin writes.
//
// API credit amounts, refill dates and Console readings, and Claude renewal
// dates, are Quota Cache's: it keeps them, edits them on its own page, works
// out each organization's credit, and publishes the result beside its
// snapshot. This plugin shows that read-only and writes none of it. The
// settings.json that quota-glance 0.7 and older saved them to stays in
// data-dir, unread.
package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/redeem"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/source"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/store"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/watch"
	"gopkg.in/yaml.v3"
)

const ID = "quota-glance"

var Version = "0.8.1"

// rawManagementSchema is the first RPC schema at which CPA hands this plugin's
// management JSON to the browser as it is. Below it, CPA HTML-escapes every
// string value in it, so an apostrophe arrives as &#39;. Every supported CPA
// offers it. Nothing else this plugin does depends on the schema: CPA's other
// schema-gated changes are to request interceptors, and it registers none.
const rawManagementSchema = 6

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
	warnBelow   float64
}

// config is the plugin's configuration block. It decodes strictly: a key it
// does not know refuses the whole block, and CPA then drops the plugin.
type config struct {
	Enabled     *bool             `yaml:"enabled"`
	CachePath   string            `yaml:"cache-path"`
	DataDir     string            `yaml:"data-dir"`
	WebToken    string            `yaml:"web-token"`
	StaleAfter  string            `yaml:"stale-after"`
	PlanLabels  map[string]string `yaml:"plan-labels"`
	AllowRedeem *bool             `yaml:"allow-redeem"`
	// AllowEdit is accepted and ignored since 0.8.0, when the editor moved to
	// Quota Cache's page, so a configuration 0.7 accepted is still accepted.
	// It is still a boolean: anything else is refused, as before.
	AllowEdit *bool          `yaml:"allow-edit"`
	WarnBelow dollars        `yaml:"openrouter-warn-below"`
	Priority  *int           `yaml:"priority"`
	Store     map[string]any `yaml:"store"`
}

// logAllowEditIgnored is said once per configure that sets allow-edit.
const logAllowEditIgnored = "quota-glance no longer edits API credits or renewal dates; allow-edit is ignored. Edit them on Quota Cache's page"

// logRefreshNotRequested is said once per press whose refresh request could
// not be written. It names neither the file nor the credential.
const logRefreshNotRequested = "quota-glance could not ask Quota Cache to refresh after a banked reset"

// logRebuildFailed is said once per configure whose rebuild panicked, and
// errRebuildFailed is health's last_error until a rebuild works. Neither
// carries the panic value, which can hold a path or a value read from a file.
const (
	logRebuildFailed = "quota-glance could not rebuild its summary and keeps serving the last one"
	errRebuildFailed = "summary document could not be rebuilt"
)

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

	// rebuildMu serializes Rebuild end to end, from reading its inputs to
	// publishing, so documents are published, and history appended, in the
	// order their inputs were read, whoever calls it. Nothing that holds
	// configMu or stateMu ever takes it, and Rebuild never takes mu, so it
	// cannot deadlock a close.
	rebuildMu sync.Mutex
	// rebuildWarned is set once a rebuild that panicked has been logged, and
	// cleared by each configure that starts a watcher.
	rebuildWarned atomic.Bool

	// lastGood is the most recent document built from a readable snapshot. It
	// is what gets re-served, marked stale, when the snapshot goes away: an
	// empty response is indistinguishable from a broken install. goodInput is
	// the snapshot and roster it was built from, so the parts Quota Cache's
	// credit state decides can be rebuilt over it while it is re-served.
	stateMu   sync.Mutex
	lastGood  *aggregate.Document
	goodInput aggregate.Input
	lastError string
	builtAt   time.Time
	written   time.Time
	nextReq   time.Time

	// redeemer is built the first time redemption is configured on and kept
	// for the life of this Plugin, guarded by mu. Its journal holds the claims
	// whose outcome is unknown, and a new redeemer per reconfigure would forget
	// them: the press after saving an unrelated setting would make a fresh
	// claim where it should have repeated the unresolved one, and that is the
	// press that can spend a second reset. That is all it outlives, though, not
	// the process: CPA applies an update or a switch off and on by loading a
	// fresh copy of the plugin, and the new Plugin starts with an empty journal.
	redeemer *redeem.Redeemer

	// refreshMu serializes refresh requests, so two presses that finish
	// together each read the file the other wrote rather than one dropping
	// the other's request.
	refreshMu sync.Mutex
}

func New(host Host) *Plugin {
	p := &Plugin{host: host, api: api.New(ID, "")}
	p.api.SetRefresher(quotaCacheRefresh{p})
	return p
}

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
				// Retired in 0.8.0, when the editor moved to Quota Cache's
				// page, and kept registered so CPA keeps dispatching it: it
				// answers 410 settings_moved and reads nothing.
				{Method: "POST", Path: "/plugins/" + ID + "/settings", Description: retiredSettingsDescription},
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
				// The GET twin of POST .../settings, retired with it.
				{Path: "/save-settings", Description: retiredSettingsDescription},
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
		return nil, fmt.Errorf("unknown method: %s", method)
	}
}

// retiredSettingsDescription describes quota-glance 0.7's two settings doors.
const retiredSettingsDescription = "Retired: API credit and renewal settings are saved on Quota Cache's page"

func (p *Plugin) configure(raw []byte) (protocol.Registration, error) {
	var req protocol.LifecycleRequest
	if json.Unmarshal(raw, &req) != nil || req.SchemaVersion < protocol.SchemaVersion {
		return protocol.Registration{}, errors.New("schema 4 or newer required")
	}
	// What CPA holds the plugin to from this answer on, if it is accepted.
	schema := min(req.SchemaVersion, rawManagementSchema)
	cfg := config{CachePath: qc.DefaultPath, DataDir: defaultDataDir, StaleAfter: defaultStaleAfter.String()}
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
		p.warnIgnored(cfg)
		served.SetStringsEscaped(schema < rawManagementSchema)
		return registration(schema), nil
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
	allowRedeem := cfg.AllowRedeem == nil || *cfg.AllowRedeem
	p.settings = settings{
		cachePath: cachePath, dataDir: dataDir, staleAfter: staleAfter,
		planLabels: aggregate.NormalizePlanLabels(cfg.PlanLabels), allowRedeem: allowRedeem,
		warnBelow: warnBelow,
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
	if generated {
		// Logged once, when it is first minted, because the operator has no
		// other way to learn it. It is persisted, so a restart reuses it.
		p.log("warn", "quota-glance generated a fallback web token; set web-token in plugin configuration to choose your own", map[string]any{"web_token": token})
	}

	p.rebuildWarned.Store(false)
	watcher, err := watch.Start(watch.Options{
		Path:        cachePath,
		CreditsPath: qc.CreditsPath(cachePath),
		WarmupPath:  qc.WarmupPath(cachePath),
		OnChange:    p.Rebuild,
		Logf:        func(message string) { p.log("warn", message, nil) },
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
	p.warnIgnored(cfg)
	// Last, as the configure is accepted: the page decodes the summary's
	// strings only while it says CPA escaped them.
	served.SetStringsEscaped(schema < rawManagementSchema)
	return registration(schema), nil
}

// warnIgnored says, once per configure that is accepted, that a key the
// configuration sets no longer does anything.
func (p *Plugin) warnIgnored(cfg config) {
	if cfg.AllowEdit != nil {
		p.log("warn", logAllowEditIgnored, nil)
	}
}

// registration is the answer to an accepted configure, at schema: the one CPA
// offered, at most rawManagementSchema.
func registration(schema uint32) protocol.Registration {
	return protocol.Registration{
		SchemaVersion: schema,
		Metadata: protocol.Metadata{
			Name: ID, Version: Version, Author: "NoorChasib",
			GitHubRepository: "https://github.com/NoorChasib/cpa-plugins",
			// Every key an operator sets, so CPA's Plugins page can edit
			// them all; configFieldExclusions in the tests names the rest.
			ConfigFields: []protocol.ConfigField{
				// No apostrophes or other HTML-special characters anywhere CPA's
				// plugin list carries: it escapes them, and the console shows
				// the escape. See TestRegisteredTextSurvivesPluginListEscaping.
				{Name: "cache-path", Type: "string", Description: "quota-cache snapshot path; must match the cache-path set in quota-cache"},
				{Name: "data-dir", Type: "string", Description: "Directory where this plugin keeps its history and generated web token"},
				{Name: "web-token", Type: "string", Description: "Dashboard password for browsers without a CPA console session; also spends banked resets while allow-redeem is on; generated and logged once if empty"},
				{Name: "stale-after", Type: "string", Description: "Age at which an observation is shown as stale; default 45m"},
				{Name: "plan-labels", Type: "object", Description: "Overrides for plan display names, keyed by the provider-reported value"},
				{Name: "allow-redeem", Type: "boolean", Description: "Allow spending a banked Codex or Claude rate-limit reset from the dashboard. Default true; this form shows it off until you save it once. Set false to show the count without a button; governs both the console and the dashboard-password doors"},
				{Name: "openrouter-warn-below", Type: "number", Description: "OpenRouter balance in dollars below which its card turns amber; default 5. It turns red at $0. Needs an OpenRouter management key in Quota Cache"},
			},
		},
		Capabilities: protocol.RegistrationCapabilities{ManagementAPI: true},
	}
}

// Rebuild reads the snapshot and republishes. It runs on the watcher's
// goroutine, including its startup read; rebuildMu takes any other caller one
// at a time with it.
//
// Nothing above that goroutine recovers, and in a native plugin a panic that
// escapes it aborts CPA, so Rebuild carries its own barrier. A rebuild that
// panics publishes nothing: the last document keeps being served, and health
// says why until a rebuild works (rebuildFailed).
func (p *Plugin) Rebuild() {
	p.rebuildMu.Lock()
	defer p.rebuildMu.Unlock()
	defer func() {
		if recover() != nil {
			p.rebuildFailed()
		}
	}()
	p.rebuild()
}

// rebuildFailed reports a rebuild that panicked: in health, beside the
// document still served, and in the log once per configure, so a fault that
// recurs on every heartbeat is said once rather than once a minute.
func (p *Plugin) rebuildFailed() {
	p.configMu.RLock()
	served := p.api
	p.configMu.RUnlock()
	served.BuildFailed(errRebuildFailed)
	if p.rebuildWarned.CompareAndSwap(false, true) {
		p.log("warn", logRebuildFailed, nil)
	}
}

// rebuild is Rebuild's work, run behind its barrier and under rebuildMu. Every
// lock it takes is released on the way out of a panic too.
func (p *Plugin) rebuild() {
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
	in := aggregate.Input{
		Warmup:       result.Warmup,
		Snapshot:     result.Snapshot,
		SourceReason: result.Reason,
		Identities:   result.Identities,
		StaleAfter:   settings.staleAfter,
		PlanLabels:   settings.planLabels,
		Redeemable:   settings.allowRedeem,
		// Read on every rebuild, so a threshold changed in the panel
		// applies at the next one rather than at the next snapshot write.
		BalanceWarnBelow: settings.warnBelow,
		Credits:          result.Credits,
		CreditsError:     result.CreditsError,
	}

	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	var historyErr error
	var doc aggregate.Document
	switch {
	case result.Reason != "" && p.lastGood != nil:
		// Serve stale over empty: the last good document, honestly labelled.
		// What Quota Cache's credit state decides in it is rebuilt from the
		// snapshot and roster it came from, with the state as it is now: a
		// value entered on Quota Cache's page while this source is failing
		// still shows.
		settled := in
		settled.Snapshot, settled.SourceReason, settled.Identities = p.goodInput.Snapshot, "", p.goodInput.Identities
		doc = aggregate.WithCredits(*p.lastGood, aggregate.Build(settled, now))
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
		Meter:               meterHealthOf(settings.cachePath, result),
		CreditState:         creditStateHealthOf(settings.cachePath, result),
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

// meterHealthOf is quota-cache's meter as its published state describes it:
// when the meter last saved, from the card's meter or, with no organization
// listed, the state's inputs; and why there is none. Without a usable state
// nothing is known of the meter, which is reported missing unless the state
// itself could not be read.
func meterHealthOf(cachePath string, result source.Result) api.MeterHealth {
	health := api.MeterHealth{Path: qc.MeterPath(cachePath)}
	state := result.Credits
	if result.CreditsError != "" || state == nil {
		health.LastError = qc.CreditMeterErrorMissing
		if result.CreditsError == aggregate.CreditsUnreadable {
			health.LastError = qc.CreditMeterErrorUnreadable
		}
		return health
	}
	health.LastError = state.Inputs.MeterError
	switch {
	case state.APICredits != nil && state.APICredits.Meter != nil:
		health.FlushedAt = time.Unix(state.APICredits.Meter.UpdatedAtEpoch, 0).UTC()
	case state.Inputs.MeterFlushedAt != nil:
		health.FlushedAt = state.Inputs.MeterFlushedAt.UTC()
	}
	return health
}

// creditStateHealthOf is quota-cache's published credit state as /health
// reports it. An abandoned state's fields still show, to say how old it is.
func creditStateHealthOf(cachePath string, result source.Result) api.CreditStateHealth {
	health := api.CreditStateHealth{Path: qc.CreditsPath(cachePath), LastError: result.CreditsError}
	state := result.Credits
	if state == nil {
		return health
	}
	computed := state.ComputedAt
	health.ComputedAt = &computed
	if state.ValidUntil != nil {
		until := *state.ValidUntil
		health.ValidUntil = &until
	}
	health.SettingsState, health.QuotaCacheVersion = state.Settings.State, state.QuotaCacheVersion
	return health
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

// quotaCacheRefresh asks Quota Cache to poll a credential now, then every other
// one, by adding a request to the refresh request file beside the snapshot at
// cache-path. Quota Cache creates that directory and owns everything else in
// it; this writes that one file, atomically, and never creates the directory.
// A Quota Cache older than 0.2.0 never reads it, and the file stays at a few
// hundred bytes however many presses add to it.
type quotaCacheRefresh struct{ p *Plugin }

// refreshReaderMaxAge is how old Quota Cache's credit state may be for the
// Quota Cache that published it to count as running: three of the 60-second
// heartbeats it publishes at.
const refreshReaderMaxAge = 3 * time.Minute

// refreshPollWithin is how soon Quota Cache has to be able to poll the account
// for the page to say the card updates within a minute.
const refreshPollWithin = time.Minute

// RequestRefresh writes the request and, when it reports true, returns the
// instant written in it. One that could not be written — no such directory, a
// write that failed, an auth index that is not CPA's sixteen hex digits — is
// logged once, naming neither the file nor the credential.
//
// It reports true only when a Quota Cache that acts on the request is running
// too, which it knows by the credit state Quota Cache 0.2.0 and newer publish
// beside the snapshot at least every minute, and its snapshot says it can poll
// the account within about a minute (refreshPollSoon). An older Quota Cache
// never reads the file, a stopped one is not polling, and one whose next free
// slot or provider's 429 pause is further off polls the account only then; in
// each case the card updates at a later poll, and the page must not say within
// a minute. The request is written all the same, so this changes only what the
// page is told.
func (r quotaCacheRefresh) RequestRefresh(provider, authIndex string) (time.Time, bool) {
	p := r.p
	p.configMu.RLock()
	cachePath := p.settings.cachePath
	p.configMu.RUnlock()
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	now := time.Now().UTC()
	// An empty cache-path would name a file in CPA's working directory; it is
	// never empty once configured, and nothing can be pressed before that.
	if cachePath == "" || qc.AppendRefreshRequest(qc.RefreshRequestPath(cachePath), qc.RefreshRequest{
		Provider: provider, AuthIndex: authIndex, Reason: qc.RefreshReasonReset,
	}, now) != nil {
		p.log("info", logRefreshNotRequested, nil)
		return time.Time{}, false
	}
	// Read afresh rather than taken from the last rebuild, which can be from
	// before Quota Cache stopped. A state left by a Quota Cache since stopped,
	// or rolled back below 0.2.0, is no longer renewed, so within three
	// minutes it is too old to count.
	state, err := qc.LoadCredits(qc.CreditsPath(cachePath))
	if err != nil || now.Sub(state.ComputedAt) > refreshReaderMaxAge {
		return time.Time{}, false
	}
	// The snapshot too, as the last rebuild may predate the poll that just
	// moved its next free slot.
	snapshot, err := qc.Load(cachePath)
	if err != nil || !refreshPollSoon(snapshot, provider, now) {
		return time.Time{}, false
	}
	return now, true
}

// refreshPollSoon reports whether Quota Cache, as its snapshot describes it,
// can poll a credential of provider within refreshPollWithin of a request made
// at now. A request moves neither next_request, the next free slot under
// request-spacing, nor a 429 pause on the provider, so the poll waits for the
// later of the two. Once both are past, Quota Cache reads the request within
// seconds and polls at once. Until then the poll comes at the poller's first
// tick after them, and ticks come once per spacing, so the spacing has to be
// within the minute as well; at worst the poll is then two minutes off, inside
// the page's three-minute check. The snapshot does not record the spacing,
// but it is no more than next_request less the latest poll's start: that poll
// set next_request to its slot plus the spacing, a longer spacing set since
// moves it out to match, and nothing brings it forward. Quota Cache accepts a
// spacing of up to an hour, and a pause lasts up to six.
func refreshPollSoon(snapshot qc.Snapshot, provider string, now time.Time) bool {
	slot := snapshot.NextRequest
	if pause := snapshot.ProviderCooldown[provider]; pause.After(slot) {
		slot = pause
	}
	if !slot.After(now) {
		return true
	}
	var polled time.Time
	for _, entry := range snapshot.Entries {
		if entry.LastAttempt.After(polled) {
			polled = entry.LastAttempt
		}
	}
	return !slot.After(now.Add(refreshPollWithin)) && !polled.IsZero() && snapshot.NextRequest.Sub(polled) <= refreshPollWithin
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
