package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/cache"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/meter"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/quota"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/settings"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/warmup"
	"gopkg.in/yaml.v3"
)

const ID = "quota-cache"

var Version = "0.2.1"

// rawManagementSchema is the first schema at which CPA hands this plugin's
// management JSON to the browser as it is. Below it, CPA HTML-escapes every
// string in it, so an apostrophe arrives as &#39;. Every supported CPA offers
// it, and nothing else this plugin does depends on the schema.
const rawManagementSchema = 6

type Host interface {
	ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error)
	GetAuth(context.Context, string) ([]byte, error)
	HTTPDo(context.Context, protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error)
	Log(context.Context, string, string, map[string]any)
}

type Plugin struct {
	mu           sync.Mutex
	host         Host
	warmup       *warmup.Manager
	warmupDone   chan struct{}
	providerGate providerGate
	// Tests may supply a runner; production always uses the native Claude CLI.
	warmupRunner warmup.Runner
	cache        *cache.Cache
	// store, organizations and publisher run while the cache does, and are
	// nil while it is stopped: the settings the operator saves on the page,
	// the Claude Console organizations, and the credit state worked out from
	// them. Each writes only while this plugin holds the cache's writer lock.
	store         *settings.Store
	organizations *settings.OrganizationStore
	publisher     *publisher
	// refresh follows the refresh requests Quota Glance writes after a
	// banked reset. The poller owns it while the cache runs; nil otherwise.
	refresh        *refreshWatch
	opts           cache.Options
	cancel         context.CancelFunc
	done           chan struct{}
	wake           chan time.Duration
	terminal       bool
	statusMu       sync.Mutex
	statusReads    uint64
	lastStatusRead time.Time
	// schema is what the last accepted register or reconfigure answered: the
	// schema CPA offered, at most rawManagementSchema.
	schema atomic.Uint32
	// openRouterKey is the configured OpenRouter management key, or empty. The
	// fetcher reads it on every scan, so adding, rotating or removing the key
	// takes effect without restarting the writer.
	openRouterKey atomic.Pointer[string]
	// configItems is how many items claude-api-credits lists in the
	// configuration, as the last configure read it. They are ignored, and only
	// counted.
	configItems atomic.Int64
	// apiCredits is the item list in force, the organizations file's
	// (itemsOf), and itemsState is the organizations block published with
	// it. The fetcher reads apiCredits on every scan, like openRouterKey, so a
	// change on the page needs no restart.
	apiCredits atomic.Pointer[[]creditItem]
	itemsState atomic.Pointer[client.CreditOrganizations]
	// roster is every Claude credential CPA last listed, for the page's
	// renewal dates. The fetcher fills it on every scan; it is held in memory
	// only, never written to a file or a log.
	roster atomic.Pointer[[]claudeCredential]
	// meter counts Claude API-key traffic per organization while the cache
	// runs and at least one organization is linked; nil otherwise.
	// usage.handle reads it without a lock, since CPA delivers usage records
	// on its own goroutine and must never wait on a configure.
	meter atomic.Pointer[meter.Meter]

	// linkMu guards the item list in force and the meter's lifecycle. A
	// configure and a stop take it under mu; the publisher's ticks and the
	// save route take it alone, so neither ever waits on mu, which a stop
	// holds while it waits for the publisher.
	linkMu sync.Mutex
	// linkOrganizations is the organizations store of the running cache, nil
	// while it is stopped, and metering is set while a meter may run: the
	// cache runs and the last configure was accepted. Guarded by linkMu.
	linkOrganizations *settings.OrganizationStore
	metering          bool
	// linkPath is the running cache's snapshot path, beside which the meter
	// keeps its file. Guarded by linkMu.
	linkPath string

	// trace, when a test sets it, is told each step of a stop as it begins,
	// and when the save route is about to apply an organizations batch.
	trace func(step string)
}

func New(host Host) *Plugin { return &Plugin{host: host} }

func (p *Plugin) Handle(method string, raw []byte) (any, error) {
	switch method {
	case protocol.MethodPluginRegister, protocol.MethodPluginReconfigure:
		return p.configure(raw)
	case protocol.MethodPluginQuiesce:
		p.stop(false)
		return struct{}{}, nil
	case protocol.MethodUsageHandle:
		// CPA calls this once per upstream attempt, synchronously, on the one
		// goroutine every usage plugin shares; the meter only decodes and
		// enqueues. A record that arrives while nothing is counting is lost,
		// which the meter records as a gap at its next start.
		if m := p.meter.Load(); m != nil {
			m.Offer(raw, time.Now().UTC())
		}
		return struct{}{}, nil
	case protocol.MethodManagementRegister:
		return protocol.ManagementRegistration{
			Routes: []protocol.ManagementRoute{
				{Method: "GET", Path: "/plugins/" + ID + "/status", Description: "Private cached quota observations and polling activity"},
				// No Menu: CPA would turn the route into a public resource.
				{Method: "POST", Path: "/plugins/" + ID + "/settings", Description: "Save Claude Console organizations, API credit amounts, refill dates, Console readings and Claude renewal dates; requires the management key"},
				{Method: "POST", Path: "/plugins/" + ID + "/warmup", Description: "Queue idle Claude accounts for warm-up; requires warm-up enabled and the management key"},
			},
			Resources: []protocol.ResourceRoute{{Path: "/status", Menu: "Quota Cache", Description: "Quota cache status and polling history"}},
		}, nil
	case protocol.MethodManagementHandle:
		var req protocol.ManagementRequest
		if json.Unmarshal(raw, &req) != nil {
			return nil, errors.New("invalid management request")
		}
		if req.Method == "GET" && strings.TrimRight(req.Path, "/") == "/v0/resource/plugins/"+ID+"/status" {
			return sidebarResponse(), nil
		}
		if req.Method == "POST" && req.Path == settingsPath {
			return p.saveSettings(req), nil
		}
		if req.Method == "POST" && req.Path == warmupPath {
			return p.warmAll(), nil
		}
		if req.Method != "GET" || req.Path != "/v0/management/plugins/"+ID+"/status" {
			return response(404, map[string]string{"error": "not_found"}), nil
		}
		return p.status(), nil
	default:
		return nil, fmt.Errorf("unknown method: %s", method)
	}
}

// status answers the authenticated status route: the snapshot, the polling
// activity, the meter file, and the credit state and editor the page shows.
func (p *Plugin) status() protocol.ManagementResponse {
	p.mu.Lock()
	opts, current, store, organizations, publisher, refresh, warmer := p.opts, p.cache, p.store, p.organizations, p.publisher, p.refresh, p.warmup
	p.mu.Unlock()
	if current == nil {
		return response(503, map[string]string{"error": "cache_stopped"})
	}
	snapshot, err := client.Load(opts.Path)
	if err != nil {
		return response(503, map[string]string{"error": "cache_unavailable"})
	}
	now := time.Now().UTC()
	p.statusMu.Lock()
	previous := p.lastStatusRead
	p.lastStatusRead, p.statusReads = now, p.statusReads+1
	reads := p.statusReads
	p.statusMu.Unlock()
	// The meter file beside the snapshot, when there is one it can read;
	// the page shows the count and never computes with it.
	apiMeter, _ := client.LoadMeter(client.MeterPath(opts.Path))
	state := publisher.LastState()
	return response(200, struct {
		client.Snapshot
		GeneratedAt        time.Time        `json:"generated_at"`
		Running            bool             `json:"running"`
		CachePath          string           `json:"cache_path"`
		PollInterval       string           `json:"poll_interval"`
		RequestSpacing     string           `json:"request_spacing"`
		Activity           cache.Activity   `json:"activity"`
		StatusReads        uint64           `json:"status_reads"`
		PreviousStatusRead time.Time        `json:"previous_status_read"`
		APIMeter           *client.APIMeter `json:"api_meter,omitempty"`
		// CreditState is the published state as a reader sees it now, with
		// Quota Glance's default stale-after, so the two pages agree; null
		// before the first compute.
		CreditState *client.CreditState `json:"credit_state"`
		Editor      editorStatus        `json:"editor"`
		// StringsEscaped is set while CPA HTML-escapes this response's
		// strings, which the page then decodes. A boolean is never escaped.
		StringsEscaped bool `json:"strings_escaped"`
		// Refresh is the latest refresh after a banked reset.
		Refresh refreshStatus       `json:"refresh"`
		Warmup  *client.WarmupState `json:"warmup,omitempty"`
	}{snapshot, now, true, opts.Path, opts.Interval.String(), opts.Spacing.String(), current.Activity(), reads, previous, apiMeter,
		client.CreditsAt(state, now, client.DefaultCreditStaleAfter), p.editorOf(store, organizations, state, snapshot, now),
		p.schema.Load() < rawManagementSchema, refreshStatusOf(current, refresh), warmupStatus(warmer, now)})
}

func response(status int, value any) protocol.ManagementResponse {
	raw, _ := json.Marshal(value)
	return protocol.ManagementResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, Body: raw}
}

// configuration is quota-cache's block of CPA's configuration. It is decoded
// strictly: a key it does not name rejects the whole configure, so a key is
// never added to it lightly, since an older quota-cache would then refuse the
// configuration after a downgrade.
type configuration struct {
	WarmupEnabled     bool   `yaml:"warmup-enabled"`
	WarmupModel       string `yaml:"warmup-model"`
	WarmupEffort      string `yaml:"warmup-effort"`
	WarmupHours       string `yaml:"warmup-hours"`
	WarmupTimezone    string `yaml:"warmup-timezone"`
	WarmupCLIPath     string `yaml:"warmup-cli-path"`
	WarmupProfileRoot string `yaml:"warmup-profile-root"`
	Path              string `yaml:"cache-path"`
	Interval          string `yaml:"poll-interval"`
	Spacing           string `yaml:"request-spacing"`
	OpenRouterKey     string `yaml:"openrouter-management-key"`
	// CreditItems is claude-api-credits, where quota-cache 0.1.x listed its
	// organizations. They are listed on the page now, so its items are only
	// counted. It is decoded as raw nodes, as in 0.1.x, so a configuration
	// written for one still loads, and a value that is not a list still fails
	// the whole configuration, as any mistyped key does.
	CreditItems []yaml.Node    `yaml:"claude-api-credits"`
	Enabled     *bool          `yaml:"enabled"`
	Priority    *int           `yaml:"priority"`
	Store       map[string]any `yaml:"store"`
}

// configFields are the settings CPA's Plugins page offers in Quota Cache's
// Edit config, under Config fields in the Configure quota-cache dialog. Every
// key of configuration is one of them or is left out on purpose
// (TestEveryKeyIsOfferedOrLeftOutOnPurpose).
var configFields = []protocol.ConfigField{
	{Name: "cache-path", Type: "string", Description: "Shared private snapshot path; preserve across restarts. The organizations and values entered on the Quota Cache page are kept in files beside it, so move the whole directory when changing it"},
	{Name: "poll-interval", Type: "string", Description: "Least time between scheduled polls of one credential; a refresh after a banked reset polls sooner; default 15m"},
	{Name: "request-spacing", Type: "string", Description: "Minimum spacing between provider requests; default 10s"},
	{Name: "openrouter-management-key", Type: "string", Description: "OpenRouter management key for reading your account balance; empty disables it. It can also create and delete API keys, so give it an expiry"},
	{Name: "warmup-enabled", Type: "boolean", Description: "Warm idle Claude subscription windows using separately signed-in Claude Code profiles; default false"},
	{Name: "warmup-model", Type: "string", Description: "Model used only for warm-ups; default claude-haiku-5-5; changes apply without a restart"},
	{Name: "warmup-effort", Type: "enum", EnumValues: []string{"low", "medium", "high", "xhigh", "max"}, Description: "Warm-up effort; default low; the selected model must support it"},
	{Name: "warmup-hours", Type: "string", Description: "Daily automatic warm-up hours HH:MM-HH:MM; default 00:00-24:00; overnight ranges are supported"},
	{Name: "warmup-timezone", Type: "string", Description: "IANA timezone for warm-up hours; default UTC"},
	{Name: "warmup-cli-path", Type: "string", Description: "Claude Code executable available inside the CPA host or container; default claude; requires 2.1.293 or newer"},
	{Name: "warmup-profile-root", Type: "string", Description: "Directory of native Claude Code profiles named by CPA auth index; default claude-profiles beside the snapshot; sign in each profile separately"},
}

func (p *Plugin) configure(raw []byte) (protocol.Registration, error) {
	// Held from the first check: an answer CPA rejects stops the meter, which
	// is done under linkMu, taken under p.mu.
	p.mu.Lock()
	defer p.mu.Unlock()
	var req protocol.LifecycleRequest
	if json.Unmarshal(raw, &req) != nil || req.SchemaVersion < 4 {
		return p.reject(errors.New("schema 4 or newer required"))
	}
	cfg := configuration{Path: client.DefaultPath, Interval: "15m", Spacing: "10s", WarmupModel: warmup.DefaultModel, WarmupEffort: "low", WarmupHours: "00:00-24:00", WarmupTimezone: "UTC", WarmupCLIPath: "claude"}
	if len(req.ConfigYAML) > 0 {
		decoder := yaml.NewDecoder(bytes.NewReader(req.ConfigYAML))
		decoder.KnownFields(true)
		if decoder.Decode(&cfg) != nil {
			return p.reject(errors.New("invalid quota-cache configuration"))
		}
	}
	interval, e1 := time.ParseDuration(cfg.Interval)
	spacing, e2 := time.ParseDuration(cfg.Spacing)
	if e1 != nil || e2 != nil || interval < time.Minute || interval > 24*time.Hour || spacing < time.Second || spacing > time.Hour || cfg.Path == "" {
		return p.reject(errors.New("invalid cache schedule or path"))
	}
	opts := cache.Options{Path: cfg.Path, Interval: interval, Spacing: spacing}
	// Compare locations, not the spelling of the path. CPA can rewrite a
	// relative default as an absolute path when saving user configuration.
	opts.Path, e1 = filepath.Abs(opts.Path)
	if e1 != nil {
		return p.reject(errors.New("cache path cannot be resolved"))
	}
	if p.terminal {
		return p.reject(errors.New("cache shut down"))
	}
	warmOpts := warmup.Defaults(opts.Path)
	warmOpts.Enabled, warmOpts.Model, warmOpts.Effort = cfg.WarmupEnabled, cfg.WarmupModel, cfg.WarmupEffort
	warmOpts.Hours, warmOpts.Timezone, warmOpts.CLIPath, warmOpts.Spacing = cfg.WarmupHours, cfg.WarmupTimezone, cfg.WarmupCLIPath, spacing
	if cfg.WarmupProfileRoot != "" {
		warmOpts.ProfileRoot = cfg.WarmupProfileRoot
	}
	warmOpts, e1 = warmOpts.Validate()
	if e1 != nil {
		return p.reject(e1)
	}
	if p.cache != nil && p.opts.Path != opts.Path {
		return p.reject(errors.New("cache path changes require native restart"))
	}
	if p.cache != nil && p.opts != opts {
		p.cache.SetSchedule(interval, spacing)
		// Keep only the latest requested spacing. This never starts a second poller
		// or cancels an in-flight provider request.
		select {
		case <-p.wake:
		default:
		}
		p.wake <- spacing
	}
	p.opts = opts
	// Stored before a writer starts, so its first scan already sees the keys.
	openRouterKey := strings.TrimSpace(cfg.OpenRouterKey)
	p.openRouterKey.Store(&openRouterKey)
	p.configItems.Store(int64(len(cfg.CreditItems)))
	if cfg.Enabled != nil && !*cfg.Enabled && p.cache != nil {
		p.closeLocked(client.MeterStopDisabled)
	}
	opened := false
	if p.cache == nil && (cfg.Enabled == nil || *cfg.Enabled) {
		current, err := cache.Open(opts, hostFetcher{host: gatedHost{p.host, &p.providerGate}, openRouterKey: &p.openRouterKey, apiCredits: &p.apiCredits, roster: &p.roster})
		if err != nil {
			return p.reject(err)
		}
		p.opts, p.cache, opened = opts, current, true
		// Neither store fails to open or writes anything when it does.
		p.store, p.organizations = settings.Open(opts.Path), settings.OpenOrganizations(opts.Path)
		p.publisher = newPublisher(opts.Path, Version, p.store, p.itemsInForce, p.logLine)
		p.publisher.prepare = p.prepareTick(p.store, p.organizations)
		p.refresh = newRefreshWatch(opts.Path)
		runner := p.warmupRunner
		if runner == nil {
			runner = warmup.CLI{}
		}
		p.warmup = warmup.New(opts.Path, warmOpts, warmup.RunnerFunc(func(ctx context.Context, options warmup.Options, identity warmup.Identity) error {
			if !p.providerGate.acquire(ctx) {
				return ctx.Err()
			}
			defer p.providerGate.release()
			return runner.Warm(ctx, options, identity)
		}), p.warmupRoster, func(id string, now time.Time) { current.RequestPoll(client.Key("claude", id), now) })
	}
	if p.warmup != nil {
		p.warmup.Configure(warmOpts)
	}
	now := time.Now().UTC()
	if p.cache != nil {
		p.store.Sync()
		if p.store.Status().State == client.CreditSettingsUnreadable {
			p.host.Log(context.Background(), "warn", "quota-cache cannot read its saved API credit settings; none apply until the file is moved aside", nil)
		}
		p.organizations.Sync()
	}
	// The meter runs while the cache does and at least one organization is
	// linked, and the usage capability is declared whenever the cache runs, so
	// an organization added on the page is counted from its first request. CPA
	// rebuilds the capability from each answer. With nothing linked the meter
	// file stays put, and the next start carries on from it with a gap.
	p.linkMu.Lock()
	p.linkOrganizations, p.metering, p.linkPath = p.organizations, p.cache != nil, opts.Path
	p.relinkLocked(now, true)
	p.linkMu.Unlock()
	if p.cache != nil && len(cfg.CreditItems) > 0 {
		p.host.Log(context.Background(), "info", "quota-cache keeps Claude Console organizations on its page; claude-api-credits in its configuration is ignored", nil)
	}
	if opened {
		ctx, cancel := context.WithCancel(context.Background())
		p.cancel, p.done = cancel, make(chan struct{})
		p.wake = make(chan time.Duration, 1)
		go p.run(ctx, p.cache, p.refresh, p.done, p.wake, spacing)
		p.warmupDone = make(chan struct{})
		go func(manager *warmup.Manager, done chan struct{}) { defer close(done); manager.Run(ctx) }(p.warmup, p.warmupDone)
		p.publisher.start()
	}
	if p.publisher != nil {
		p.publisher.PublishNow()
	}
	// What CPA holds the plugin to from this answer on, so the status says
	// whether CPA escapes its strings.
	p.schema.Store(min(req.SchemaVersion, rawManagementSchema))
	return protocol.Registration{SchemaVersion: p.schema.Load(), Metadata: protocol.Metadata{Name: ID, Version: Version, Author: "NoorChasib", GitHubRepository: "https://github.com/NoorChasib/cpa-plugins", ConfigFields: configFields},
		Capabilities: protocol.RegistrationCapabilities{UsagePlugin: p.cache != nil, ManagementAPI: true}}, nil
}

// relinkLocked puts the item list in force, from the organizations file, and
// brings the meter in line with it: the organizations it links, a meter opened
// from its file when one is now linked and none runs, which records the gap,
// or the running one stopped with no_items when none is. With none linked and
// none running, the file keeps the stop its last run wrote. Only a configure,
// with replaceFailed, replaces a meter that stopped itself. While no meter may
// run, it stops one with disabled. The caller holds linkMu.
func (p *Plugin) relinkLocked(now time.Time, replaceFailed bool) {
	organizations := settings.Organizations{}
	if p.linkOrganizations != nil {
		organizations = p.linkOrganizations.Current()
	}
	items, state := itemsOf(organizations, int(p.configItems.Load()))
	p.apiCredits.Store(&items)
	p.itemsState.Store(&state)
	linked := linkedOrganizations(items)
	switch m := p.meter.Load(); {
	case !p.metering:
		p.stopMeter(client.MeterStopDisabled)
	case len(linked) == 0:
		p.stopMeter(client.MeterStopNoItems)
	case m == nil || (replaceFailed && m.Failed()):
		// A meter that stopped itself is replaced from its file, which
		// records the gap.
		p.stopMeter(client.MeterStopFailed)
		p.meter.Store(meter.Open(client.MeterPath(p.linkPath), linked, now, p.host))
	default:
		m.SetLinked(linked, now)
	}
}

// itemsInForce is what the publisher computes from: the item list and the
// organizations block published with it.
func (p *Plugin) itemsInForce() ([]creditItem, client.CreditOrganizations) {
	items, state := []creditItem{}, client.CreditOrganizations{State: client.OrganizationsStateMissing}
	if loaded := p.apiCredits.Load(); loaded != nil {
		items = *loaded
	}
	if loaded := p.itemsState.Load(); loaded != nil {
		state = *loaded
	}
	return items, state
}

// prepareTick is what each publisher tick does before it looks at the files:
// the settings store's Sync, which reopens a file that changed, then
// followOrganizations. It reports whether either may have changed what the
// state is computed from, so the tick computes even when no file's stamp
// changed, as when a file that could not be opened opens once its owner is
// fixed. It never takes mu.
func (p *Plugin) prepareTick(store *settings.Store, organizations *settings.OrganizationStore) func(time.Time) bool {
	return func(now time.Time) bool {
		synced := store.Sync()
		return p.followOrganizations(organizations, now) || synced
	}
}

// followOrganizations brings the item list in line with the organizations
// file: the store's Sync, which reopens an unreadable file once it is moved
// aside and sees a deleted one. When the organizations changed, the item list
// and the meter follow, with no configure. It reports whether the
// organizations changed. Each tick runs it, and so does the save route before
// and after an organizations batch. It never takes mu.
func (p *Plugin) followOrganizations(organizations *settings.OrganizationStore, now time.Time) bool {
	if !organizations.Sync() {
		return false
	}
	p.linkMu.Lock()
	defer p.linkMu.Unlock()
	// A stop in progress has already let go of this run's stores.
	if p.linkOrganizations == organizations {
		p.relinkLocked(now, false)
	}
	return true
}

func (p *Plugin) logLine(level, message string, fields map[string]any) {
	p.host.Log(context.Background(), level, message, fields)
}

// stopMeter stops the running meter, if any, with reason, and forgets it.
// The caller holds linkMu.
func (p *Plugin) stopMeter(reason string) {
	if m := p.meter.Swap(nil); m != nil {
		m.Stop(reason, time.Now().UTC())
	}
}

// reject answers a configure with the error CPA is given. From that answer
// until a later configure succeeds, CPA keeps the plugin loaded but delivers
// it no usage records, so a meter left running would heartbeat as if it
// counted and the traffic of that window would vanish from the estimate.
// Instead the meter stops here, and the next good configure reopens it from
// its file, which records the window as a gap; until then no change on the
// page reopens it. The reason is disabled, the one of the fixed set that
// fits: to CPA the plugin is as good as switched off. The cache carries on;
// it needs no usage records. It is called under p.mu.
func (p *Plugin) reject(err error) (protocol.Registration, error) {
	if p.warmup != nil {
		p.warmup.Disable()
	}
	p.linkMu.Lock()
	p.metering = false
	p.stopMeter(client.MeterStopDisabled)
	p.linkMu.Unlock()
	return protocol.Registration{}, err
}

func (p *Plugin) run(ctx context.Context, current *cache.Cache, refresh *refreshWatch, done chan struct{}, wake <-chan time.Duration, spacing time.Duration) {
	defer close(done)
	ticker := time.NewTicker(spacing)
	defer ticker.Stop()
	checks := time.NewTicker(refreshCheckEvery)
	defer checks.Stop()
	var wait rosterWait
	// between is set when a refresh check brought the coming Step forward.
	// slot is set while the ticker is stopped until the slot such a Step left.
	between := false
	var slot <-chan time.Time
	for {
		started := time.Now()
		// A refresh request is acted on here, between polls, so the round it
		// starts begins with this Step.
		p.followRefresh(ctx, current, refresh, started.UTC())
		// A roster that is not ready yet is expected for a few seconds on every
		// CPA start, so it is not worth a warning unless it lasts. The status
		// page's activity shows it while it lasts.
		err := current.Step(ctx, started.UTC())
		if ctx.Err() == nil {
			if wait.observe(err, started) {
				p.host.Log(ctx, "warn", "quota-cache is still waiting for CPA to load credentials; saved observations are kept but not refreshed", nil)
			} else if err != nil && !errors.Is(err, cache.ErrRosterNotReady) {
				p.host.Log(ctx, "warn", "quota-cache refresh or persistence unavailable", nil)
			}
		}
		// A Step between ticks that polled left the next slot one spacing
		// after its poll, but the ticker kept its old beat: its next tick
		// would come before the slot and be turned away, and the round's
		// next poll would wait for the tick after, up to two spacings after
		// this one. So the ticker stops until the slot and keeps time from
		// there. After a Step between ticks that the spacing turned away, the
		// slot is the one the last poll left, which the old beat reaches at
		// about the same time.
		if between {
			if left := time.Until(current.NextRequest()); left > 0 {
				ticker.Stop()
				slot = time.After(left)
			}
		}
		between = false
		// Between ticks the refresh request file is checked on a timer of its
		// own. A request acted on there brings the next Step forward, so its
		// credential is polled at once if the spacing allows, rather than at
		// the next tick, up to a spacing away.
		for next := false; !next; {
			select {
			case <-ctx.Done():
				return
			case spacing = <-wake:
				ticker.Reset(spacing)
				slot, next = nil, true
			case <-ticker.C:
				next = true
			case <-slot:
				ticker.Reset(spacing)
				slot, next = nil, true
			case at := <-checks.C:
				next = p.followRefresh(ctx, current, refresh, at.UTC())
				between = next
			}
		}
	}
}

// rosterWaitWarnAfter is how long CPA may go on listing credentials it has not
// loaded before the wait is worth a warning. CPA attaches its auth manager
// seconds after it starts. A roster still not ready this long after the first
// one means it never will be, for example a host that never attaches the
// manager, and the snapshot would otherwise stop updating with nothing in the
// log. Polls only happen once per request spacing, so with a long spacing the
// warning comes at the first poll past this.
const rosterWaitWarnAfter = 5 * time.Minute

// rosterWait tracks one unbroken run of not-ready rosters.
type rosterWait struct {
	since  time.Time
	warned bool
}

// observe records one Step result and reports whether to warn now: once per
// wait, at the first not-ready result rosterWaitWarnAfter or more after the
// wait began. Any other result ends the wait.
func (w *rosterWait) observe(err error, now time.Time) bool {
	if !errors.Is(err, cache.ErrRosterNotReady) {
		*w = rosterWait{}
		return false
	}
	if w.since.IsZero() {
		w.since = now
	}
	if w.warned || now.Sub(w.since) < rosterWaitWarnAfter {
		return false
	}
	w.warned = true
	return true
}

func (p *Plugin) stop(final bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if final {
		p.terminal = true
	}
	// The stop's reason reaches the meter file: the next start records the
	// stop as a gap, since CPA keeps serving while a plugin is quiesced or
	// shut down.
	reason := client.MeterStopQuiesce
	if final {
		reason = client.MeterStopShutdown
	}
	if p.cache != nil {
		p.closeLocked(reason)
		return
	}
	p.linkMu.Lock()
	p.metering = false
	p.stopMeter(reason)
	p.linkMu.Unlock()
}
func (p *Plugin) Shutdown() { p.stop(true) }

// closeLocked stops what the running cache started, in the one order that
// keeps every write under the cache's writer lock and leaves the last credit
// state saying the meter stopped:
//
//  1. the meter, which records the stop with reason and saves;
//  2. the settings and organizations stores, each waiting for a save in
//     flight, so no later save can write;
//  3. the poller;
//  4. the publisher, which writes the state one last time over the stopped
//     meter's file;
//  5. the cache, releasing the writer lock.
//
// The caller holds p.mu.
func (p *Plugin) closeLocked(reason string) {
	p.step("meter")
	p.linkMu.Lock()
	p.linkOrganizations, p.metering = nil, false
	p.stopMeter(reason)
	p.linkMu.Unlock()
	p.step("settings")
	p.store.Close()
	p.organizations.Close()
	p.step("poller")
	p.cancel()
	<-p.done
	if p.warmupDone != nil {
		<-p.warmupDone
	}
	p.step("publisher")
	p.publisher.Stop()
	p.step("cache")
	p.cache.Close()
	p.cache, p.store, p.organizations, p.publisher, p.refresh = nil, nil, nil, nil, nil
	p.warmup, p.warmupDone = nil, nil
}

func (p *Plugin) step(name string) {
	if p.trace != nil {
		p.trace(name)
	}
}

type hostFetcher struct {
	host          Host
	openRouterKey *atomic.Pointer[string]
	apiCredits    *atomic.Pointer[[]creditItem]
	// roster, when set, receives every Claude credential of each roster that
	// is ready, disabled ones included, for the page's renewal dates.
	roster *atomic.Pointer[[]claudeCredential]
}

func (f hostFetcher) credits() []creditItem {
	if f.apiCredits == nil {
		return nil
	}
	if items := f.apiCredits.Load(); items != nil {
		return *items
	}
	return nil
}

func (f hostFetcher) openRouter() string {
	if f.openRouterKey == nil {
		return ""
	}
	if key := f.openRouterKey.Load(); key != nil {
		return *key
	}
	return ""
}

// openRouterAccount names the OpenRouter account after a fingerprint of its
// key, never the key itself. Rotating the key therefore retires the old entry
// and polls the new one straight away, rather than leaving a corrected key
// waiting out the failure backoff its mistyped predecessor earned.
func openRouterAccount(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "key-" + hex.EncodeToString(sum[:6])
}

func (f hostFetcher) List(ctx context.Context) ([]cache.Account, error) {
	roster, err := f.host.ListAuth(ctx)
	if err != nil {
		return nil, err
	}
	accounts := []cache.Account{}
	claude := []claudeCredential{}
	unindexed := false
	for _, entry := range roster {
		provider := strings.ToLower(strings.TrimSpace(entry.Provider))
		if provider == "" {
			provider = strings.ToLower(strings.TrimSpace(entry.Type))
		}
		if provider == "claude" && entry.AuthIndex != "" && !entry.RuntimeOnly {
			claude = append(claude, claudeCredential{AuthIndex: entry.AuthIndex, Name: credentialName(entry, provider), Disabled: entry.Disabled})
		}
		if entry.Disabled || entry.RuntimeOnly || !quota.Supported(provider) {
			continue
		}
		if entry.AuthIndex == "" {
			unindexed = true
			continue
		}
		accounts = append(accounts, cache.Account{Provider: provider, AuthIndex: entry.AuthIndex})
	}
	// CPA sends plugin.register before its core auth manager exists, and until
	// the manager is attached host.auth.list falls back to reading the auth
	// directory (CPA internal/pluginhost/auth_callbacks.go,
	// listAuthFilesFromDisk). Those entries name each file but carry no
	// auth_index, the identity every snapshot entry is keyed by. Once the
	// manager is attached, every file-backed entry carries one. So a roster
	// whose usable credentials all lack it is CPA still starting, not CPA
	// holding none of them, and the cache must wait rather than prune. A mixed
	// roster is already authoritative; its index-less entries are skipped.
	if unindexed && len(accounts) == 0 {
		return nil, cache.ErrRosterNotReady
	}
	if f.roster != nil {
		f.roster.Store(&claude)
	}
	if key := f.openRouter(); key != "" {
		accounts = append(accounts, cache.Account{Provider: quota.OpenRouterProvider, AuthIndex: openRouterAccount(key)})
	}
	// Every organization on the page is listed. None is ever polled: what an
	// organization spent comes from the meter, not from Anthropic.
	for _, item := range f.credits() {
		credit := item.credit
		accounts = append(accounts, cache.Account{Provider: client.ProviderAnthropicAPI, AuthIndex: item.id, Credit: &credit})
	}
	return accounts, nil
}
func (f hostFetcher) Fetch(ctx context.Context, account cache.Account, known *client.AccountDetails) (cache.Observation, error) {
	if account.Provider == quota.OpenRouterProvider {
		key := f.openRouter()
		// Removed or rotated since the scan that listed this account. The next
		// scan drops it; a request made with a key nobody configured any more
		// would be the wrong account's balance.
		if key == "" || openRouterAccount(key) != account.AuthIndex {
			return cache.Observation{}, errors.New("credential read failed")
		}
		doer := &captureDoer{host: f.host}
		observation, err := quota.FetchOpenRouter(ctx, doer, key, time.Now().UTC())
		return observed(doer, observation, err)
	}
	if account.Provider == client.ProviderAnthropicAPI {
		// Never reached: the cache skips every account that carries a credit.
		// Should it ever ask, the answer is a refusal, not a request.
		return cache.Observation{}, errors.New("not polled")
	}
	raw, err := f.host.GetAuth(ctx, account.AuthIndex)
	if err != nil {
		return cache.Observation{}, errors.New("credential read failed")
	}
	doer := &captureDoer{host: f.host}
	now := time.Now().UTC()
	observation, err := quota.Fetch(ctx, doer, account.Provider, raw, now)
	// Account details only after a poll that will be recorded as a success: a
	// refused or rate-limited usage request says the credential or the
	// provider is in no state to answer more. They go to the host directly,
	// past the capturing doer, so their statuses are never mistaken for the
	// poll's: a 429 from a profile endpoint must not pause the provider or
	// back off a credential whose usage was read perfectly well.
	if err == nil && !doer.limited() {
		observation.ApplyDetails(quota.RefreshDetails(ctx, f.host, account.Provider, raw, known, now), now)
	}
	return observed(doer, observation, err)
}

// ReportFailure puts one line in CPA's log for each failed poll. The snapshot
// keeps only the last hundred polls and host HTTP traffic never reaches CPA's
// own log, so without this a rate limit older than the history cannot be
// dated. A 429 is a warning because it pauses every credential of its
// provider; any other failure backs off one credential, which the status page
// already shows, and is logged as info. The poll schedule bounds the volume.
// A line names only the provider, the opaque auth index already in the
// snapshot, the status and the schedule: never an email, token, file name,
// URL or response body.
func (f hostFetcher) ReportFailure(ctx context.Context, failure cache.Failure) {
	// A poll cut short by shutdown is not a failure worth a line.
	if ctx.Err() != nil {
		return
	}
	level, message := "info", "quota-cache poll failed; this credential is retried at next_attempt"
	fields := map[string]any{"provider": failure.Provider, "auth_index": failure.AuthIndex, "next_attempt": logTime(failure.NextAttempt)}
	switch {
	case failure.RateLimited && !failure.ProviderPause.IsZero():
		level, message = "warn", "quota-cache poll rate limited; this provider's credentials are paused"
	case failure.RateLimited:
		level, message = "warn", "quota-cache poll rate limited; this credential is retried at next_attempt"
	}
	if failure.HTTPStatus != 0 {
		fields["http_status"] = failure.HTTPStatus
	}
	if !failure.RetryAfter.IsZero() {
		fields["retry_after"] = logTime(failure.RetryAfter)
	}
	if !failure.ProviderPause.IsZero() {
		fields["provider_paused_until"] = logTime(failure.ProviderPause)
	}
	f.host.Log(ctx, level, message, fields)
}

func logTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func observed(doer *captureDoer, observation quota.Observation, err error) (cache.Observation, error) {
	result := cache.Observation{RequestSent: doer.sent, HTTPStatus: doer.status}
	if doer.limited() {
		return result, cache.RateLimited{RetryAfter: doer.retryAfter}
	}
	// A PollError carries one of a fixed set of messages, so it passes
	// through; anything else could hold provider text and is collapsed.
	var pollErr cache.PollError
	if errors.As(err, &pollErr) {
		return result, pollErr
	}
	if err != nil {
		return result, errors.New("quota fetch failed")
	}
	result.Percent, result.ResetAt, result.ObservedAt = observation.Percent, observation.ResetAt, observation.ObservedAt
	result.Quota, result.Windows = observation.Quota, observation.Windows
	result.Plan, result.TierName, result.RenewalAt = observation.Plan, observation.TierName, observation.RenewalAt
	result.AccountDetails = observation.AccountDetails
	return result, nil
}

// captureDoer records the poll's own request, the usage or credits read, which
// is always the first a fetch makes. A later request through it, Codex's reset
// inventory, is an optional read on top of a reading already in hand, so its
// status is never the poll's: like a profile 429, an inventory 429 must not
// discard a good usage reading, count as a rate limit, or pause the provider.
type captureDoer struct {
	host       Host
	status     int
	retryAfter time.Time
	sent       bool
}

// limited reports whether the poll's own request was refused with 429, which
// turns the whole poll into a rate limit.
func (d *captureDoer) limited() bool { return d.status == 429 }

func (d *captureDoer) HTTPDo(ctx context.Context, req protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	counts := !d.sent
	d.sent = true
	response, err := d.host.HTTPDo(ctx, req)
	if !counts {
		return response, err
	}
	d.status, d.retryAfter = response.StatusCode, time.Time{}
	value := http.Header(response.Headers).Get("Retry-After")
	if seconds, e := strconv.ParseInt(value, 10, 32); e == nil && seconds > 0 {
		d.retryAfter = time.Now().Add(time.Duration(seconds) * time.Second)
	} else if date, e := http.ParseTime(value); e == nil {
		d.retryAfter = date
	}
	return response, err
}
