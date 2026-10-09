package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/cache"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/quota"
	"gopkg.in/yaml.v3"
)

const ID = "quota-cache"

var Version = "0.1.13"

type Host interface {
	ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error)
	GetAuth(context.Context, string) ([]byte, error)
	HTTPDo(context.Context, protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error)
	Log(context.Context, string, string, map[string]any)
}

type Plugin struct {
	mu             sync.Mutex
	host           Host
	cache          *cache.Cache
	opts           cache.Options
	cancel         context.CancelFunc
	done           chan struct{}
	wake           chan time.Duration
	terminal       bool
	statusMu       sync.Mutex
	statusReads    uint64
	lastStatusRead time.Time
	// openRouterKey is the configured OpenRouter management key, or empty. The
	// fetcher reads it on every scan, so adding, rotating or removing the key
	// takes effect without restarting the writer.
	openRouterKey atomic.Pointer[string]
	// apiCredits is the parsed claude-api-credits list, read by the fetcher on
	// every scan like openRouterKey, so editing it needs no restart.
	apiCredits atomic.Pointer[[]creditItem]
}

func New(host Host) *Plugin { return &Plugin{host: host} }

func (p *Plugin) Handle(method string, raw []byte) (any, error) {
	switch method {
	case protocol.MethodPluginRegister, protocol.MethodPluginReconfigure:
		return p.configure(raw)
	case protocol.MethodPluginQuiesce:
		p.stop(false)
		return struct{}{}, nil
	case protocol.MethodManagementRegister:
		return protocol.ManagementRegistration{
			Routes:    []protocol.ManagementRoute{{Method: "GET", Path: "/plugins/" + ID + "/status", Description: "Private cached quota observations and polling activity"}},
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
		if req.Method != "GET" || req.Path != "/v0/management/plugins/"+ID+"/status" {
			return response(404, map[string]string{"error": "not_found"}), nil
		}
		p.mu.Lock()
		opts, current := p.opts, p.cache
		p.mu.Unlock()
		if current == nil {
			return response(503, map[string]string{"error": "cache_stopped"}), nil
		}
		snapshot, err := client.Load(opts.Path)
		if err != nil {
			return response(503, map[string]string{"error": "cache_unavailable"}), nil
		}
		now := time.Now().UTC()
		p.statusMu.Lock()
		previous := p.lastStatusRead
		p.lastStatusRead, p.statusReads = now, p.statusReads+1
		reads := p.statusReads
		p.statusMu.Unlock()
		return response(200, struct {
			client.Snapshot
			GeneratedAt        time.Time      `json:"generated_at"`
			Running            bool           `json:"running"`
			CachePath          string         `json:"cache_path"`
			PollInterval       string         `json:"poll_interval"`
			RequestSpacing     string         `json:"request_spacing"`
			Activity           cache.Activity `json:"activity"`
			StatusReads        uint64         `json:"status_reads"`
			PreviousStatusRead time.Time      `json:"previous_status_read"`
		}{snapshot, now, true, opts.Path, opts.Interval.String(), opts.Spacing.String(), current.Activity(), reads, previous}), nil
	default:
		return nil, errors.New("unknown method")
	}
}

func response(status int, value any) protocol.ManagementResponse {
	raw, _ := json.Marshal(value)
	return protocol.ManagementResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, Body: raw}
}

func (p *Plugin) configure(raw []byte) (protocol.Registration, error) {
	var req protocol.LifecycleRequest
	if json.Unmarshal(raw, &req) != nil || req.SchemaVersion < 4 {
		return protocol.Registration{}, errors.New("schema 4 or newer required")
	}
	cfg := struct {
		Path          string `yaml:"cache-path"`
		Interval      string `yaml:"poll-interval"`
		Spacing       string `yaml:"request-spacing"`
		OpenRouterKey string `yaml:"openrouter-management-key"`
		// CreditItems is decoded as raw nodes so that each item is judged on
		// its own by parseAPICredits. A value that is not a list still fails
		// the whole configuration, as any mistyped key does.
		CreditItems []yaml.Node    `yaml:"claude-api-credits"`
		Enabled     *bool          `yaml:"enabled"`
		Priority    *int           `yaml:"priority"`
		Store       map[string]any `yaml:"store"`
	}{Path: client.DefaultPath, Interval: "15m", Spacing: "10s"}
	if len(req.ConfigYAML) > 0 {
		decoder := yaml.NewDecoder(bytes.NewReader(req.ConfigYAML))
		decoder.KnownFields(true)
		if decoder.Decode(&cfg) != nil {
			return protocol.Registration{}, errors.New("invalid quota-cache configuration")
		}
	}
	interval, e1 := time.ParseDuration(cfg.Interval)
	spacing, e2 := time.ParseDuration(cfg.Spacing)
	if e1 != nil || e2 != nil || interval < time.Minute || interval > 24*time.Hour || spacing < time.Second || spacing > time.Hour || cfg.Path == "" {
		return protocol.Registration{}, errors.New("invalid cache schedule or path")
	}
	opts := cache.Options{Path: cfg.Path, Interval: interval, Spacing: spacing}
	// Compare locations, not the spelling of the path. CPA can rewrite a
	// relative default as an absolute path when saving user configuration.
	opts.Path, e1 = filepath.Abs(opts.Path)
	if e1 != nil {
		return protocol.Registration{}, errors.New("cache path cannot be resolved")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.terminal {
		return protocol.Registration{}, errors.New("cache shut down")
	}
	if p.cache != nil && p.opts.Path != opts.Path {
		return protocol.Registration{}, errors.New("cache path changes require native restart")
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
	credits := parseAPICredits(cfg.CreditItems)
	p.apiCredits.Store(&credits)
	if cfg.Enabled != nil && !*cfg.Enabled && p.cache != nil {
		p.cancel()
		<-p.done
		p.cache.Close()
		p.cache = nil
	}
	if p.cache == nil && (cfg.Enabled == nil || *cfg.Enabled) {
		current, err := cache.Open(opts, hostFetcher{host: p.host, openRouterKey: &p.openRouterKey, apiCredits: &p.apiCredits})
		if err != nil {
			return protocol.Registration{}, err
		}
		ctx, cancel := context.WithCancel(context.Background())
		p.opts, p.cache, p.cancel, p.done = opts, current, cancel, make(chan struct{})
		p.wake = make(chan time.Duration, 1)
		go p.run(ctx, current, p.done, p.wake, spacing)
	}
	return protocol.Registration{SchemaVersion: 4, Metadata: protocol.Metadata{Name: ID, Version: Version, Author: "NoorChasib", GitHubRepository: "https://github.com/NoorChasib/cpa-plugins", ConfigFields: []protocol.ConfigField{
		{Name: "cache-path", Type: "string", Description: "Shared private snapshot path; preserve across restarts"},
		{Name: "poll-interval", Type: "string", Description: "Minimum interval per credential; default 15m"},
		{Name: "request-spacing", Type: "string", Description: "Minimum spacing between provider requests; default 10s"},
		{Name: "openrouter-management-key", Type: "string", Description: "OpenRouter management key for reading your account balance; empty disables it. It can also create and delete API keys, so give it an expiry"},
	}}, Capabilities: protocol.RegistrationCapabilities{ManagementAPI: true}}, nil
}

func (p *Plugin) run(ctx context.Context, current *cache.Cache, done chan struct{}, wake <-chan time.Duration, spacing time.Duration) {
	defer close(done)
	ticker := time.NewTicker(spacing)
	defer ticker.Stop()
	var wait rosterWait
	for {
		// A roster that is not ready yet is expected for a few seconds on every
		// CPA start, so it is not worth a warning unless it lasts. The status
		// page's activity shows it while it lasts.
		started := time.Now()
		err := current.Step(ctx, started.UTC())
		if ctx.Err() == nil {
			if wait.observe(err, started) {
				p.host.Log(ctx, "warn", "quota-cache is still waiting for CPA to load credentials; saved observations are kept but not refreshed", nil)
			} else if err != nil && !errors.Is(err, cache.ErrRosterNotReady) {
				p.host.Log(ctx, "warn", "quota-cache refresh or persistence unavailable", nil)
			}
		}
		select {
		case <-ctx.Done():
			return
		case spacing = <-wake:
			ticker.Reset(spacing)
		case <-ticker.C:
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
	if p.cache != nil {
		p.cancel()
		<-p.done
		p.cache.Close()
		p.cache = nil
	}
}
func (p *Plugin) Shutdown() { p.stop(true) }

type hostFetcher struct {
	host          Host
	openRouterKey *atomic.Pointer[string]
	apiCredits    *atomic.Pointer[[]creditItem]
}

// creditUserAgent names this plugin to Anthropic's Admin API. It is never
// claudeUserAgent: these are the operator's own reads with their own key, not
// Claude Code's, and Anthropic asks integrations to identify themselves.
func creditUserAgent() string {
	return "cpa-plugins-quota-cache/" + Version + " (https://github.com/NoorChasib/cpa-plugins)"
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
	unindexed := false
	for _, entry := range roster {
		provider := strings.ToLower(strings.TrimSpace(entry.Provider))
		if provider == "" {
			provider = strings.ToLower(strings.TrimSpace(entry.Type))
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
	if key := f.openRouter(); key != "" {
		accounts = append(accounts, cache.Account{Provider: quota.OpenRouterProvider, AuthIndex: openRouterAccount(key)})
	}
	// Every configured item is listed, misconfigured or not, so the snapshot
	// can say what is wrong with one; the cache never polls an item with a
	// problem.
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
		// The same guard as OpenRouter's: an item removed, broken or given
		// another key since the scan that listed it is not read with whatever
		// key it holds now.
		item, ok := f.credit(account.AuthIndex)
		if !ok || account.Credit == nil || item.credit.Problem != "" || item.credit.KeyFingerprint != account.Credit.KeyFingerprint {
			return cache.Observation{}, errors.New("credential read failed")
		}
		doer := &captureDoer{host: f.host, everyRequest: true}
		observation, err := quota.FetchAPICredit(ctx, doer, item.key, item.credit.Renews, item.credit.KeyFingerprint, creditUserAgent(), time.Now().UTC())
		return observed(doer, observation, creditFailure(err))
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

func (f hostFetcher) credit(id string) (creditItem, bool) {
	for _, item := range f.credits() {
		if item.id == id {
			return item, true
		}
	}
	return creditItem{}, false
}

// ReportFailure puts one line in CPA's log for each failed poll. The snapshot
// keeps only the last hundred polls and host HTTP traffic never reaches CPA's
// own log, so without this a rate limit older than the history cannot be
// dated. A 429 is a warning because it pauses every credential of its
// provider, or, for a Claude API credit organization, stops that organization
// being read until its own retry; any other failure backs off one credential,
// which the status page already shows, and is logged as info. The poll schedule bounds the volume.
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
//
// A Claude API credit poll is the exception, set by everyRequest. Every
// request it makes, each cost-report page and the /v1/organizations/me
// fallback, is part of the one reading, and the reading fails with any of
// them. So each request counts until one is refused: the poll's status is the
// first that is not 2xx, a 429 on any of them makes the poll rate-limited, and
// Retry-After is read from that response.
type captureDoer struct {
	host         Host
	everyRequest bool
	status       int
	retryAfter   time.Time
	sent         bool
}

// limited reports whether the poll's own request was refused with 429, which
// turns the whole poll into a rate limit.
func (d *captureDoer) limited() bool { return d.status == 429 }

func (d *captureDoer) HTTPDo(ctx context.Context, req protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	counts := !d.sent || (d.everyRequest && d.status >= 200 && d.status < 300)
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
