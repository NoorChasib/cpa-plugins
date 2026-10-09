package plugin

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/cache"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

const (
	codexUsageURL         = "https://chatgpt.com/backend-api/wham/usage"
	codexResetCreditsURL  = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	codexSubscriptionsURL = "https://chatgpt.com/backend-api/subscriptions?account_id=synthetic-account"
	// Two resets banked, which is what makes the inventory worth asking for.
	codexUsageBody = `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":40,"limit_window_seconds":604800}},` +
		`"rate_limit_reset_credits":{"available_count":2}}`
)

// codexHost is claudeHost holding one synthetic Codex credential instead.
type codexHost struct{ *claudeHost }

func newCodexHost() codexHost {
	host := newClaudeHost()
	host.bodies[codexUsageURL] = codexUsageBody
	host.bodies[codexResetCreditsURL] = `{"credits":[{"id":"c1","status":"available","expires_at":"2099-01-01T00:00:00Z"}]}`
	return codexHost{host}
}

func (codexHost) ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error) {
	return []protocol.HostAuthFileEntry{{AuthIndex: "two", Provider: "codex"}}, nil
}
func (codexHost) GetAuth(context.Context, string) ([]byte, error) {
	return []byte(`{"access_token":"synthetic-not-a-real-token","account_id":"synthetic-account"}`), nil
}

var codexAccount = cache.Account{Provider: "codex", AuthIndex: "two"}

// Dating banked resets is an optional second request on top of a usage reading
// already in hand. However it fails, including with 429, the poll is the usage
// request's: a success with the usage status, followed by the account details
// any successful poll may read. Only the expiry is lost.
func TestResetInventoryFailuresNeverTouchThePoll(t *testing.T) {
	for name, fail := range map[string]func(*claudeHost){
		"rate limited":    func(h *claudeHost) { h.statuses[codexResetCreditsURL] = 429 },
		"server error":    func(h *claudeHost) { h.statuses[codexResetCreditsURL] = 500 },
		"transport error": func(h *claudeHost) { h.failures[codexResetCreditsURL] = errors.New("unreachable") },
	} {
		t.Run(name, func(t *testing.T) {
			host := newCodexHost()
			fail(host.claudeHost)
			observation, err := fetcherWith(host, "").Fetch(context.Background(), codexAccount, nil)
			if err != nil {
				t.Fatalf("the poll failed because the reset inventory did: %v", err)
			}
			if observation.HTTPStatus != 200 || !observation.RequestSent || observation.Percent != 40 || observation.ObservedAt.IsZero() {
				t.Fatalf("observation = %+v", observation)
			}
			if credits := observation.Quota.ResetCredits; credits == nil || credits.AvailableCount != 2 || credits.SoonestExpiry != nil {
				t.Fatalf("reset credits = %+v; want the count kept and the expiry unknown", credits)
			}
			if host.count(codexResetCreditsURL) != 1 || host.count(codexSubscriptionsURL) != 1 {
				t.Fatalf("asked %v", host.asked)
			}
		})
	}
}

// End to end through the real writer: a 429 on the reset inventory leaves the
// poll a success, counts no rate limit, and pauses nothing.
func TestARateLimitedResetInventoryDoesNotPauseCodex(t *testing.T) {
	host := newCodexHost()
	host.statuses[codexResetCreditsURL] = 429
	opts := cache.Options{Path: filepath.Join(t.TempDir(), "cache", "snapshot.json"), Interval: 15 * time.Minute, Spacing: time.Second}
	current, err := cache.Open(opts, fetcherWith(host, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	now := time.Now().UTC()
	if err := current.Step(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Load(opts.Path)
	if err != nil {
		t.Fatal(err)
	}
	entry := snapshot.Entries[client.Key("codex", "two")]
	if entry.LastError != "" || entry.Failures != 0 || entry.Percent != 40 || !entry.NextAttempt.Equal(now.Add(opts.Interval)) {
		t.Fatalf("entry = %+v", entry)
	}
	if pause := snapshot.ProviderCooldown["codex"]; !pause.IsZero() || snapshot.Totals.RateLimits != 0 {
		t.Fatalf("codex paused until %s with %d rate limits; want neither", pause, snapshot.Totals.RateLimits)
	}
	if poll := snapshot.History[len(snapshot.History)-1]; poll.Outcome != "success" || poll.HTTPStatus != 200 {
		t.Fatalf("poll = %+v; want a success with the usage status", poll)
	}
}
