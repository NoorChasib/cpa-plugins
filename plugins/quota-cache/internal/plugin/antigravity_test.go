package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/cache"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

const (
	agSummaryURL = "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary"
	agModelsURL  = "https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels"
	agLoadURL    = "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"
	agSummary    = `{"groups":[{"buckets":[` +
		`{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.75,"resetTime":"2099-01-01T00:00:00Z"},` +
		`{"bucketId":"3p-weekly","window":"weekly","resetTime":"2099-01-01T00:00:00Z"}]}]}`
	agModels = `{"models":{"gemini-3-pro":{"modelProvider":"MODEL_PROVIDER_GOOGLE",` +
		`"weeklyQuotaInfo":{"remainingFraction":0.5,"resetTime":"2099-01-01T00:00:00Z"}}}}`
)

// antigravityHost lists one Antigravity credential whose file does not name
// its project yet, as CPA lists it before the account's first proxied
// request, and answers each Cloud Code method as configured. Nothing here
// reaches Google.
type antigravityHost struct {
	mu       sync.Mutex
	bodies   map[string]string
	statuses map[string]int
	requests []protocol.HostHTTPRequest
}

func newAntigravityHost() *antigravityHost {
	return &antigravityHost{
		bodies:   map[string]string{agSummaryURL: agSummary, agModelsURL: agModels, agLoadURL: `{"paidTier":{"name":"Google AI Pro"}}`},
		statuses: map[string]int{},
	}
}

func (*antigravityHost) ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error) {
	return []protocol.HostAuthFileEntry{{AuthIndex: "ag", Provider: "antigravity", ProjectID: " listed-project "}}, nil
}
func (*antigravityHost) GetAuth(context.Context, string) ([]byte, error) {
	return []byte(`{"type":"antigravity","access_token":"synthetic-not-a-real-token","refresh_token":"synthetic-refresh",` +
		`"expired":"2099-01-01T00:00:00Z","email":"synthetic@example.invalid"}`), nil
}
func (h *antigravityHost) HTTPDo(_ context.Context, req protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.requests = append(h.requests, req)
	status := h.statuses[req.URL]
	if status == 0 {
		status = 200
	}
	return protocol.HostHTTPResponse{StatusCode: status, Headers: map[string][]string{"Retry-After": {"3600"}}, Body: []byte(h.bodies[req.URL])}, nil
}
func (*antigravityHost) Log(context.Context, string, string, map[string]any) {}

func (h *antigravityHost) urls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var urls []string
	for _, r := range h.requests {
		urls = append(urls, r.URL)
	}
	return urls
}

var antigravityAccount = cache.Account{Provider: "antigravity", AuthIndex: "ag", ProjectID: "listed-project"}

// The roster carries the project CPA lists for an Antigravity credential, and
// only for Antigravity.
func TestListCarriesTheAntigravityProject(t *testing.T) {
	accounts, err := fetcherWith(&antigravityHost{}, "").List(context.Background())
	if err != nil || len(accounts) != 1 || accounts[0] != antigravityAccount {
		t.Fatalf("accounts=%+v err=%v; want %+v", accounts, err, antigravityAccount)
	}
	other := diskEntry("claude-one.json", "claude")
	other.AuthIndex, other.ProjectID = "one", "not-for-claude"
	accounts, err = fetcherWith(rosterHost{[]protocol.HostAuthFileEntry{other}}, "").List(context.Background())
	if err != nil || len(accounts) != 1 || accounts[0].ProjectID != "" {
		t.Fatalf("accounts=%+v err=%v; a Claude account carries no project", accounts, err)
	}
}

// A good poll: one summary request with the listed project, then the plan
// from loadCodeAssist, with the poll recorded as the summary's.
func TestAntigravityPollEndToEnd(t *testing.T) {
	host := newAntigravityHost()
	observation, err := fetcherWith(host, "").Fetch(context.Background(), antigravityAccount, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := host.urls(); strings.Join(got, ",") != agSummaryURL+","+agLoadURL {
		t.Fatalf("urls = %v; want the summary, then loadCodeAssist", got)
	}
	var body map[string]string
	if err := json.Unmarshal(host.requests[0].Body, &body); err != nil || body["project"] != "listed-project" || host.requests[0].Method != "POST" {
		t.Fatalf("summary request = %s %s", host.requests[0].Method, host.requests[0].Body)
	}
	if observation.HTTPStatus != 200 || !observation.RequestSent || observation.Percent != 25 || observation.ObservedAt.IsZero() || observation.Plan != "Google AI Pro" {
		t.Fatalf("observation = %+v", observation)
	}
}

// The model list is a fallback on a summary that answered 2xx. However it
// fails — including with 429 — the poll is the summary's: never a rate limit,
// never a provider pause.
func TestAntigravityPollIsJudgedByTheSummaryAlone(t *testing.T) {
	t.Run("fallback rate limited", func(t *testing.T) {
		host := newAntigravityHost()
		host.bodies[agSummaryURL] = `{}`
		host.statuses[agModelsURL] = 429
		observation, err := fetcherWith(host, "").Fetch(context.Background(), antigravityAccount, nil)
		var limited cache.RateLimited
		if err == nil || errors.As(err, &limited) {
			t.Fatalf("err = %v; want an ordinary failure, not a rate limit", err)
		}
		if observation.HTTPStatus != 200 || !observation.RequestSent {
			t.Fatalf("observation = %+v; the poll's status is the summary's", observation)
		}
		if got := host.urls(); strings.Join(got, ",") != agSummaryURL+","+agModelsURL {
			t.Fatalf("urls = %v; no plan is read after a failed poll", got)
		}
	})
	t.Run("fallback answers", func(t *testing.T) {
		host := newAntigravityHost()
		host.bodies[agSummaryURL] = `{"groups":[]}`
		observation, err := fetcherWith(host, "").Fetch(context.Background(), antigravityAccount, nil)
		if err != nil {
			t.Fatal(err)
		}
		if observation.HTTPStatus != 200 || observation.Percent != 50 || observation.ObservedAt.IsZero() {
			t.Fatalf("observation = %+v", observation)
		}
		if got := host.urls(); strings.Join(got, ",") != agSummaryURL+","+agModelsURL+","+agLoadURL {
			t.Fatalf("urls = %v", got)
		}
	})
	t.Run("summary rate limited", func(t *testing.T) {
		host := newAntigravityHost()
		host.statuses[agSummaryURL] = 429
		observation, err := fetcherWith(host, "").Fetch(context.Background(), antigravityAccount, nil)
		var limited cache.RateLimited
		if !errors.As(err, &limited) || observation.HTTPStatus != 429 {
			t.Fatalf("err = %v observation = %+v; a summary 429 is the poll's", err, observation)
		}
		if got := host.urls(); len(got) != 1 {
			t.Fatalf("urls = %v; a refused summary is never followed by the fallback", got)
		}
	})
}

// Through the real writer: the Antigravity entry reaches the snapshot with
// its plan and per-family windows, and a 429 on the summary pauses only the
// antigravity provider.
func TestAntigravityReachesTheSnapshot(t *testing.T) {
	host := newAntigravityHost()
	opts := cache.Options{Path: filepath.Join(t.TempDir(), "cache", "snapshot.json"), Interval: 15 * time.Minute, Spacing: time.Second}
	c, err := cache.Open(opts, fetcherWith(host, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	now := time.Now().UTC()
	if err := c.Step(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Load(opts.Path)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := snapshot.Entries[client.Key(client.ProviderAntigravity, "ag")]
	if !ok || entry.LastError != "" || entry.Percent != 25 || entry.Plan != "Google AI Pro" || len(entry.Windows) != 2 {
		t.Fatalf("entry = %+v", entry)
	}
	if entry.Windows[0].Key != client.WindowWeekly || entry.Windows[1].Key != client.WindowModelWeekly || entry.Windows[1].Model != client.AntigravityModelShared ||
		entry.Windows[1].UsedPercent != 100 || entry.Windows[1].Title != "Weekly (Claude & GPT)" {
		t.Fatalf("windows = %+v", entry.Windows)
	}
	raw, _ := json.Marshal(snapshot)
	for _, secret := range []string{"synthetic-not-a-real-token", "synthetic-refresh", "listed-project", "example.invalid"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("the snapshot carries %q", secret)
		}
	}

	host.statuses[agSummaryURL] = 429
	if err := c.Step(context.Background(), now.Add(16*time.Minute)); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = client.Load(opts.Path)
	if pause := snapshot.ProviderCooldown[client.ProviderAntigravity]; pause.IsZero() {
		t.Fatalf("cooldowns = %v; a summary 429 pauses antigravity", snapshot.ProviderCooldown)
	}
	for provider := range snapshot.ProviderCooldown {
		if provider != client.ProviderAntigravity {
			t.Fatalf("cooldowns = %v; only antigravity may pause", snapshot.ProviderCooldown)
		}
	}
}

// The sidebar names Antigravity wherever it names Grok, and says its poll is a
// POST. It is judged fresh by its weekly like Claude, Codex and Grok, not by
// the nested observation the providers without a weekly use.
func TestStatusPageNamesAntigravity(t *testing.T) {
	for _, rule := range []string{
		"antigravity: 'Antigravity'",
		"antigravity: 'daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary'",
		"const methods = {antigravity: 'POST'};",
		"(methods[p.provider] || 'GET') + ' ' + (endpoints[p.provider] || 'Provider quota endpoint')",
		"const noWeekly = ['openrouter', 'anthropic-api'];",
		"Enable a Claude, Codex, Grok, or Antigravity OAuth account in CPA",
	} {
		if !strings.Contains(sidebarApp, rule) {
			t.Fatalf("page script lost %q", rule)
		}
	}
	for _, rule := range []string{`<option value="xai">Grok</option><option value="antigravity">Antigravity</option>`, "Enable a Claude, Codex, Grok, or Antigravity OAuth account in CPA"} {
		if !strings.Contains(sidebarHTML, rule) {
			t.Fatalf("page lost %q", rule)
		}
	}
}
