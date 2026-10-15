package plugin

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/warmup"
)

const warmupPath = "/v0/management/plugins/" + ID + "/warmup"

// providerGate serializes quota HTTP requests with warm-ups. A warm-up holds
// it only while it waits for its model request, at most a minute, so one CPA
// cannot stop does not hold up the polls or create a second scheduled poller.
type providerGate struct {
	once sync.Once
	slot chan struct{}
}

func (g *providerGate) acquire(ctx context.Context) bool {
	g.once.Do(func() { g.slot = make(chan struct{}, 1) })
	select {
	case g.slot <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}
func (g *providerGate) release() { <-g.slot }

type gatedHost struct {
	Host
	gate *providerGate
}

func (h gatedHost) HTTPDo(ctx context.Context, req protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	if !h.gate.acquire(ctx) {
		return protocol.HostHTTPResponse{}, ctx.Err()
	}
	defer h.gate.release()
	return h.Host.HTTPDo(ctx, req)
}

func (p *Plugin) warmupRoster(ctx context.Context) ([]warmup.Identity, error) {
	roster, err := p.host.ListAuth(ctx)
	if err != nil {
		return nil, err
	}
	identities := []warmup.Identity{}
	for _, entry := range roster {
		provider := entry.Provider
		if provider == "" {
			provider = entry.Type
		}
		if strings.ToLower(strings.TrimSpace(provider)) != "claude" || entry.RuntimeOnly {
			continue
		}
		identities = append(identities, warmup.Identity{AuthIndex: entry.AuthIndex, ID: entry.ID, Email: credentialName(entry, "claude"), Disabled: entry.Disabled})
	}
	return identities, nil
}

func (p *Plugin) warmAll() protocol.ManagementResponse {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cache == nil || p.warmup == nil {
		return response(503, map[string]string{"error": "cache_stopped"})
	}
	if !p.warmup.WarmAll() {
		return response(409, map[string]string{"error": "warmup_disabled_or_unavailable"})
	}
	return response(202, map[string]string{"status": "queued"})
}

// warmupStatus is safe for the authenticated settings page. No credential,
// token or model response is part of it.
func warmupStatus(manager *warmup.Manager, now time.Time) *client.WarmupState {
	if manager == nil {
		return nil
	}
	state := manager.Status(now)
	return &state
}
