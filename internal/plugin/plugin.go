package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/NoorChasib/cpa-plugin-token-usage/internal/collector"
	"github.com/NoorChasib/cpa-plugin-token-usage/internal/config"
	"github.com/NoorChasib/cpa-plugin-token-usage/internal/protocol"
	"github.com/NoorChasib/cpa-plugin-token-usage/internal/store"
	"github.com/NoorChasib/cpa-plugin-token-usage/internal/usage"
)

const ID = "token-usage"

var Version = "0.1.0"

type Plugin struct {
	lifecycle sync.Mutex
	mu        sync.RWMutex
	current   *collector.Collector
	cfg       config.Config
	terminal  bool
	now       func() time.Time
	fixture   fixtureRecorder
}

func New() *Plugin                              { return &Plugin{now: time.Now} }
func (p *Plugin) runtime() *collector.Collector { p.mu.RLock(); defer p.mu.RUnlock(); return p.current }
func (p *Plugin) Handle(method string, raw []byte) (any, error) {
	if len(raw) > protocol.MaxRequestBytes {
		if method == protocol.MethodUsageHandle {
			p.RejectNativeUsage()
		}
		return nil, errors.New("request too large")
	}
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
		return protocol.ManagementRegistration{Routes: []protocol.ManagementRoute{
			{Method: http.MethodGet, Path: "/plugins/" + ID + "/status", Description: "Private collection health and coverage."},
			{Method: http.MethodGet, Path: "/plugins/" + ID + "/summary", Description: "Private CPA-reported raw totals."},
			{Method: http.MethodGet, Path: "/plugins/" + ID + "/models", Description: "Private provider/model raw totals."},
		}}, nil
	case protocol.MethodManagementHandle:
		return p.management(raw), nil
	case protocol.MethodUsageHandle:
		p.mu.RLock()
		defer p.mu.RUnlock()
		c := p.current
		if c == nil {
			return nil, errors.New("collection unavailable")
		}
		e, err := c.Observe(raw)
		if err == nil {
			p.fixture.record(e)
		}
		return struct{}{}, err
	default:
		return nil, errors.New("unknown method")
	}
}
func (p *Plugin) RejectNativeUsage() {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.current != nil {
		p.current.RejectOversized()
	}
}
func (p *Plugin) configure(raw []byte) (protocol.Registration, error) {
	var request protocol.LifecycleRequest
	if json.Unmarshal(raw, &request) != nil || request.SchemaVersion < protocol.SchemaVersion {
		return protocol.Registration{}, errors.New("schema 6 lifecycle request required")
	}
	cfg, err := config.Parse(request.ConfigYAML)
	if err != nil {
		return protocol.Registration{}, err
	}
	p.lifecycle.Lock()
	defer p.lifecycle.Unlock()
	p.mu.RLock()
	old, prior, terminal := p.current, p.cfg, p.terminal
	p.mu.RUnlock()
	if terminal {
		return protocol.Registration{}, errors.New("plugin is shut down")
	}
	if prior.DatabasePath != "" && cfg != prior {
		return protocol.Registration{}, errors.New("configuration change requires native restart")
	}
	if old != nil && !old.Stopped() {
		return registration(), nil
	}
	current, err := collector.New(cfg, p.now)
	if err != nil {
		return protocol.Registration{}, errors.New("storage initialization failed")
	}
	p.mu.Lock()
	p.current = current
	p.cfg = cfg
	p.mu.Unlock()
	return registration(), nil
}
func (p *Plugin) stop(final bool) {
	p.lifecycle.Lock()
	defer p.lifecycle.Unlock()
	p.mu.Lock()
	old := p.current
	// Keep the stopped collector reachable for late callback diagnostics/status.
	// Its final persisted snapshot is best effort; post-close increments are RAM
	// only and a same-config reopen resumes the durable snapshot instead.
	if final {
		p.terminal = true
	}
	p.mu.Unlock()
	if old != nil {
		old.Stop()
	}
}
func (p *Plugin) Shutdown() { p.stop(true) }
func (p *Plugin) management(raw []byte) protocol.ManagementResponse {
	// Management headers/body may carry credentials; no field here decodes them.
	var req struct {
		Method, Path string
		Query        url.Values
	}
	if json.Unmarshal(raw, &req) != nil {
		return failure(400, "invalid_request")
	}
	base := "/v0/management/plugins/" + ID
	if req.Method != "GET" || (req.Path != base+"/status" && req.Path != base+"/summary" && req.Path != base+"/models") {
		return failure(404, "not_found")
	}
	c := p.runtime()
	if c == nil {
		return failure(503, "collection_unavailable")
	}
	p.mu.RLock()
	cfg := p.cfg
	p.mu.RUnlock()
	if req.Path == base+"/status" {
		if len(req.Query) > 0 {
			return failure(400, "invalid_query")
		}
		snapshot := c.Snapshot()
		value := map[string]any{"api_schema": 1, "source": "cpa_reported", "version": Version, "storage": "sqlite", "state": snapshot["state"], "coverage": c.Coverage(), "collection": snapshot, "upstream_completeness": "unknown", "limitations": []string{"CPA delivery has no durable replay guarantee", "Claude split/cumulative streams can omit input/cache or later output", "Failed/disconnected streams can lose tokens or retain an earlier success", "Zero reported usage does not prove zero consumption"}, "limits": map[string]any{"queue_capacity": cfg.QueueCapacity, "batch_size": cfg.BatchSize, "raw_retention": cfg.RawRetention.String(), "max_disk_bytes": strconv.FormatInt(cfg.MaxDiskBytes, 10), "max_models": cfg.MaxModels, "query_timeout": cfg.QueryTimeout.String()}}
		p.fixture.addStatus(value, snapshot)
		return jsonResponse(200, value)
	}
	f, err := parseQuery(req.Query, req.Path == base+"/models", cfg, p.now())
	if err != nil {
		return failure(400, "invalid_query")
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.QueryTimeout)
	defer cancel()
	result, err := c.Query(ctx, f)
	if err != nil {
		if errors.Is(err, store.ErrCoverage) {
			return jsonResponse(416, map[string]any{"error": "outside_retained_coverage", "coverage": result.Coverage})
		}
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			return failure(504, "query_timeout")
		}
		return failure(503, "storage_unavailable")
	}
	value := map[string]any{"api_schema": 1, "source": "cpa_reported", "interval": map[string]time.Time{"from": f.From, "to": f.To}, "coverage": result.Coverage, "collection": c.Snapshot(), "upstream_completeness": "unknown", "aggregation": "provider_model_combined_executors", "auxiliary_counters_additive": false}
	if f.Models {
		value["models"] = result.Models
		value["limit"] = f.Limit
		value["offset"] = f.Offset
		value["has_more"] = result.HasMore
	} else {
		value["totals"] = result.Totals
	}
	return jsonResponse(200, value)
}
func parseQuery(q url.Values, models bool, cfg config.Config, now time.Time) (store.Filter, error) {
	f := store.Filter{Models: models, Limit: 100}
	invalid := errors.New("invalid query")
	allowed := map[string]bool{"from": true, "to": true, "provider": true, "model": true}
	if models {
		allowed["limit"] = true
		allowed["offset"] = true
	}
	for key, values := range q {
		if !allowed[key] || len(values) != 1 || values[0] == "" {
			return f, invalid
		}
	}
	var err error
	if f.From, err = time.Parse(time.RFC3339Nano, q.Get("from")); err != nil {
		return f, invalid
	}
	if f.To, err = time.Parse(time.RFC3339Nano, q.Get("to")); err != nil {
		return f, invalid
	}
	f.From = f.From.UTC()
	f.To = f.To.UTC()
	if f.From.Year() < 1970 || f.To.Year() > 2100 || !f.From.Before(f.To) || f.To.After(now) || f.To.Sub(f.From) > cfg.RawRetention {
		return f, invalid
	}
	f.Provider, f.Model = q.Get("provider"), q.Get("model")
	for _, key := range []string{"provider", "model"} {
		if value, ok := q[key]; ok && !usage.MetadataValid(value[0], true) {
			return f, invalid
		}
	}
	if raw := q.Get("limit"); raw != "" {
		if f.Limit, err = strconv.Atoi(raw); err != nil || f.Limit < 1 || f.Limit > 1000 {
			return f, invalid
		}
	}
	if raw := q.Get("offset"); raw != "" {
		if f.Offset, err = strconv.Atoi(raw); err != nil || f.Offset < 0 || f.Offset > 100000 {
			return f, invalid
		}
	}
	return f, nil
}
func registration() protocol.Registration {
	return protocol.Registration{SchemaVersion: protocol.SchemaVersion, Metadata: protocol.Metadata{Name: "Token Usage", Version: Version, Author: "NoorChasib", GitHubRepository: "https://github.com/NoorChasib/cpa-plugin-token-usage", ConfigFields: []protocol.ConfigField{{Name: "database-path", Type: "string", Description: "Required absolute path in a dedicated private persistent directory."}}}, Capabilities: protocol.RegistrationCapabilities{UsagePlugin: true, ManagementAPI: true}}
}
func failure(status int, code string) protocol.ManagementResponse {
	return jsonResponse(status, map[string]string{"error": code})
}
func jsonResponse(status int, value any) protocol.ManagementResponse {
	body, err := json.Marshal(value)
	if err != nil {
		status, body = 500, []byte(`{"error":"encoding_failed"}`)
	}
	return protocol.ManagementResponse{StatusCode: status, Body: body, Headers: http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"}}}
}
