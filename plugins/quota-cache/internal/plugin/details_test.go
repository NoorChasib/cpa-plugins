package plugin

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/cache"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

const (
	usageURL   = "https://api.anthropic.com/api/oauth/usage?cedar_ember=1"
	profileURL = "https://api.anthropic.com/api/oauth/profile"
	usageBody  = `{"seven_day":{"utilization":40,"resets_at":"2099-01-01T00:00:00Z"}}`
)

// claudeHost holds one synthetic Claude credential and answers each URL as
// configured, counting what was asked. Nothing here reaches a real provider.
type claudeHost struct {
	mu       sync.Mutex
	bodies   map[string]string
	statuses map[string]int
	failures map[string]error
	asked    map[string]int
}

func newClaudeHost() *claudeHost {
	return &claudeHost{
		bodies: map[string]string{
			usageURL:   usageBody,
			profileURL: `{"organization":{"organization_type":"claude_max","rate_limit_tier":"default_claude_max_20x"}}`,
		},
		statuses: map[string]int{}, failures: map[string]error{}, asked: map[string]int{},
	}
}

func (*claudeHost) ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error) {
	return []protocol.HostAuthFileEntry{{AuthIndex: "one", Provider: "claude"}}, nil
}
func (*claudeHost) GetAuth(context.Context, string) ([]byte, error) {
	return []byte(`{"access_token":"synthetic-not-a-real-token"}`), nil
}
func (h *claudeHost) HTTPDo(_ context.Context, req protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.asked[req.URL]++
	if err := h.failures[req.URL]; err != nil {
		return protocol.HostHTTPResponse{}, err
	}
	status := h.statuses[req.URL]
	if status == 0 {
		status = 200
	}
	return protocol.HostHTTPResponse{StatusCode: status, Headers: map[string][]string{"Retry-After": {"3600"}}, Body: []byte(h.bodies[req.URL])}, nil
}
func (*claudeHost) Log(context.Context, string, string, map[string]any) {}

func (h *claudeHost) count(url string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.asked[url]
}

var claudeAccount = cache.Account{Provider: "claude", AuthIndex: "one"}

// The usage observation is the poll. However an account endpoint fails —
// including with 429 — the poll is recorded with the usage request's status,
// as a success, and the provider is not paused.
func TestAccountDetailFailuresNeverTouchThePoll(t *testing.T) {
	for name, fail := range map[string]func(*claudeHost){
		"rate limited":    func(h *claudeHost) { h.statuses[profileURL] = 429 },
		"server error":    func(h *claudeHost) { h.statuses[profileURL] = 500 },
		"transport error": func(h *claudeHost) { h.failures[profileURL] = errors.New("unreachable") },
		"unreadable":      func(h *claudeHost) { h.bodies[profileURL] = "<html>" },
	} {
		t.Run(name, func(t *testing.T) {
			host := newClaudeHost()
			fail(host)
			observation, err := fetcherWith(host, "").Fetch(context.Background(), claudeAccount, nil)
			if err != nil {
				t.Fatalf("the poll failed because an account endpoint did: %v", err)
			}
			if observation.HTTPStatus != 200 || !observation.RequestSent || observation.Percent != 40 || observation.ObservedAt.IsZero() {
				t.Fatalf("observation = %+v", observation)
			}
			if host.count(profileURL) != 1 {
				t.Fatalf("profile asked %d times", host.count(profileURL))
			}
			// The read was attempted, so the next one waits out the interval.
			if d := observation.AccountDetails; d == nil || d.CheckedAt.IsZero() || d.Plan != "" {
				t.Fatalf("details = %+v", d)
			}
		})
	}
}

// A refused or rate-limited usage request says the credential or provider is
// in no state to answer more, so no account endpoint is asked after it.
func TestAccountDetailsAreNotReadAfterAFailedPoll(t *testing.T) {
	for status, limited := range map[int]bool{429: true, 401: false, 500: false} {
		host := newClaudeHost()
		host.statuses[usageURL] = status
		_, err := fetcherWith(host, "").Fetch(context.Background(), claudeAccount, nil)
		var rateLimited cache.RateLimited
		if err == nil || errors.As(err, &rateLimited) != limited {
			t.Fatalf("HTTP %d: err = %v", status, err)
		}
		if host.count(profileURL) != 0 {
			t.Fatalf("HTTP %d: the profile was asked after a failed poll", status)
		}
	}
}

// Details checked within the interval are used as they are, with no request.
func TestRecentAccountDetailsAreUsedWithoutARequest(t *testing.T) {
	host := newClaudeHost()
	known := &client.AccountDetails{CheckedAt: time.Now().UTC().Add(-time.Hour), Plan: "max_5x"}
	observation, err := fetcherWith(host, "").Fetch(context.Background(), claudeAccount, known)
	if err != nil {
		t.Fatal(err)
	}
	if host.count(profileURL) != 0 || host.count(usageURL) != 1 {
		t.Fatalf("asked %v", host.asked)
	}
	if observation.Plan != "max_5x" || observation.AccountDetails != known {
		t.Fatalf("observation plan = %q details = %+v", observation.Plan, observation.AccountDetails)
	}
}

// End to end through the real writer: the first poll reads the profile once,
// the plan and the subscription start reach the snapshot entry consumers read,
// the billing period arrives with every usage poll, and neither a later poll
// nor a restart asks for the profile again inside the interval.
func TestAccountDetailsSurviveARestartWithoutARead(t *testing.T) {
	host := newClaudeHost()
	host.bodies[profileURL] = `{"organization":{"organization_type":"claude_max","rate_limit_tier":"default_claude_max_20x",` +
		`"subscription_created_at":"2025-01-31T09:15:00Z"}}`
	host.bodies[usageURL] = `{"seven_day":{"utilization":40,"resets_at":"2099-01-01T00:00:00Z"},` +
		`"cedar_ember":{"eligible":false,"event_props":{"billing_period":"monthly","tier":"claude_max_20x"}}}`
	opts := cache.Options{Path: filepath.Join(t.TempDir(), "cache", "snapshot.json"), Interval: 15 * time.Minute, Spacing: time.Second}
	start := time.Now().UTC()
	current, err := cache.Open(opts, fetcherWith(host, ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := current.Step(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	if err := current.Step(context.Background(), start.Add(16*time.Minute)); err != nil {
		t.Fatal(err)
	}
	current.Close()
	restarted, err := cache.Open(opts, fetcherWith(host, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.Step(context.Background(), start.Add(32*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if host.count(usageURL) != 3 || host.count(profileURL) != 1 {
		t.Fatalf("usage asked %d times and profile %d; want 3 and 1", host.count(usageURL), host.count(profileURL))
	}
	snapshot, err := client.Load(opts.Path)
	if err != nil {
		t.Fatal(err)
	}
	entry := snapshot.Entries[client.Key("claude", "one")]
	if entry.Plan != "max_20x" || entry.AccountDetails == nil || entry.AccountDetails.Plan != "max_20x" || entry.LastError != "" {
		t.Fatalf("entry plan = %q details = %+v error = %q", entry.Plan, entry.AccountDetails, entry.LastError)
	}
	started := time.Date(2025, time.January, 31, 9, 15, 0, 0, time.UTC)
	if at := entry.AccountDetails.SubscriptionStartedAt; at == nil || !at.Equal(started) || entry.RenewalAt != nil {
		t.Fatalf("subscription started at %v, renewal %v; want the start kept as a start", at, entry.RenewalAt)
	}
	if entry.Quota == nil || entry.Quota.BillingPeriod != client.BillingMonthly {
		t.Fatalf("quota = %+v; want the billing period from the last usage poll", entry.Quota)
	}
}
