package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/cache"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/quota"
	"gopkg.in/yaml.v3"
)

const ID = "quota-cache"

var Version = "0.1.0"

type Host interface {
	ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error)
	GetAuth(context.Context, string) ([]byte, error)
	HTTPDo(context.Context, protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error)
	Log(context.Context, string, string, map[string]any)
}

type Plugin struct {
	mu       sync.Mutex
	host     Host
	cache    *cache.Cache
	opts     cache.Options
	cancel   context.CancelFunc
	done     chan struct{}
	terminal bool
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
		return protocol.ManagementRegistration{Routes: []protocol.ManagementRoute{{Method: "GET", Path: "/plugins/" + ID + "/status", Description: "Private cached quota observations and retry schedule"}}}, nil
	case protocol.MethodManagementHandle:
		var req protocol.ManagementRequest
		if json.Unmarshal(raw, &req) != nil {
			return nil, errors.New("invalid management request")
		}
		if req.Method != "GET" || req.Path != "/v0/management/plugins/"+ID+"/status" {
			return response(404, map[string]string{"error": "not_found"}), nil
		}
		p.mu.Lock()
		path := p.opts.Path
		running := p.cache != nil
		p.mu.Unlock()
		if !running {
			return response(503, map[string]string{"error": "cache_stopped"}), nil
		}
		snapshot, err := client.Load(path)
		if err != nil {
			return response(503, map[string]string{"error": "cache_unavailable"}), nil
		}
		return response(200, snapshot), nil
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
		Path     string         `yaml:"cache-path"`
		Interval string         `yaml:"poll-interval"`
		Spacing  string         `yaml:"request-spacing"`
		Enabled  *bool          `yaml:"enabled"`
		Priority *int           `yaml:"priority"`
		Store    map[string]any `yaml:"store"`
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
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.terminal {
		return protocol.Registration{}, errors.New("cache shut down")
	}
	if p.opts.Path != "" && p.opts != opts {
		return protocol.Registration{}, errors.New("configuration changes require native restart")
	}
	if cfg.Enabled != nil && !*cfg.Enabled && p.cache != nil {
		p.cancel()
		<-p.done
		p.cache.Close()
		p.cache = nil
	}
	if p.cache == nil && (cfg.Enabled == nil || *cfg.Enabled) {
		current, err := cache.Open(opts, hostFetcher{p.host})
		if err != nil {
			return protocol.Registration{}, err
		}
		ctx, cancel := context.WithCancel(context.Background())
		p.opts, p.cache, p.cancel, p.done = opts, current, cancel, make(chan struct{})
		go p.run(ctx, current, p.done, spacing)
	}
	return protocol.Registration{SchemaVersion: 4, Metadata: protocol.Metadata{Name: ID, Version: Version, Author: "NoorChasib", GitHubRepository: "https://github.com/NoorChasib/cpa-plugins", ConfigFields: []protocol.ConfigField{
		{Name: "cache-path", Type: "string", Description: "Shared private snapshot path; preserve across restarts"},
		{Name: "poll-interval", Type: "string", Description: "Minimum interval per credential; default 15m"},
		{Name: "request-spacing", Type: "string", Description: "Minimum spacing between provider requests; default 10s"},
	}}, Capabilities: protocol.RegistrationCapabilities{ManagementAPI: true}}, nil
}

func (p *Plugin) run(ctx context.Context, current *cache.Cache, done chan struct{}, spacing time.Duration) {
	defer close(done)
	ticker := time.NewTicker(spacing)
	defer ticker.Stop()
	for {
		if err := current.Step(ctx, time.Now().UTC()); err != nil && ctx.Err() == nil {
			p.host.Log(ctx, "warn", "quota-cache refresh or persistence unavailable", nil)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
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

type hostFetcher struct{ host Host }

func (f hostFetcher) List(ctx context.Context) ([]cache.Account, error) {
	roster, err := f.host.ListAuth(ctx)
	if err != nil {
		return nil, err
	}
	accounts := []cache.Account{}
	for _, entry := range roster {
		provider := strings.ToLower(strings.TrimSpace(entry.Provider))
		if provider == "" {
			provider = strings.ToLower(strings.TrimSpace(entry.Type))
		}
		if entry.AuthIndex == "" || entry.Disabled || entry.RuntimeOnly || !quota.Supported(provider) {
			continue
		}
		accounts = append(accounts, cache.Account{Provider: provider, AuthIndex: entry.AuthIndex})
	}
	return accounts, nil
}
func (f hostFetcher) Fetch(ctx context.Context, account cache.Account) (cache.Observation, error) {
	raw, err := f.host.GetAuth(ctx, account.AuthIndex)
	if err != nil {
		return cache.Observation{}, errors.New("credential read failed")
	}
	doer := &captureDoer{host: f.host}
	observation, err := quota.Fetch(ctx, doer, account.Provider, raw, time.Now().UTC())
	if doer.status == 429 {
		return cache.Observation{}, cache.RateLimited{RetryAfter: doer.retryAfter}
	}
	if err != nil {
		return cache.Observation{}, errors.New("quota fetch failed")
	}
	return cache.Observation{Percent: observation.Percent, ResetAt: observation.ResetAt, ObservedAt: observation.ObservedAt}, nil
}

type captureDoer struct {
	host       Host
	status     int
	retryAfter time.Time
}

func (d *captureDoer) HTTPDo(ctx context.Context, req protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	response, err := d.host.HTTPDo(ctx, req)
	d.status = response.StatusCode
	value := http.Header(response.Headers).Get("Retry-After")
	if seconds, e := strconv.ParseInt(value, 10, 32); e == nil && seconds > 0 {
		d.retryAfter = time.Now().Add(time.Duration(seconds) * time.Second)
	} else if date, e := http.ParseTime(value); e == nil {
		d.retryAfter = date
	}
	return response, err
}
