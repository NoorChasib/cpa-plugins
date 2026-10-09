package plugin

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/cache"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

type logLine struct {
	level, message string
	fields         map[string]any
}

// logHost is claudeHost keeping every line logged to it. Its one credential is
// listed the way CPA lists a real one, with a file name and an email, so a
// line that leaked either would show.
type logHost struct {
	*claudeHost
	authErr error
	mu      sync.Mutex
	lines   []logLine
}

func (*logHost) ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error) {
	return []protocol.HostAuthFileEntry{{AuthIndex: "one", Provider: "claude", Name: "claude-private-name.json",
		Email: "private-person@example.com", Path: "/root/.cli-proxy-api/claude-private-name.json"}}, nil
}
func (h *logHost) GetAuth(ctx context.Context, index string) ([]byte, error) {
	if h.authErr != nil {
		return nil, h.authErr
	}
	return h.claudeHost.GetAuth(ctx, index)
}
func (h *logHost) Log(_ context.Context, level, message string, fields map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lines = append(h.lines, logLine{level, message, fields})
}

func (h *logHost) logged() []logLine {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]logLine(nil), h.lines...)
}

// pollOnce runs one poll through the real writer and returns what it saved.
func pollOnce(t *testing.T, host *logHost, now time.Time) client.Snapshot {
	t.Helper()
	opts := cache.Options{Path: filepath.Join(t.TempDir(), "cache", "snapshot.json"), Interval: 15 * time.Minute, Spacing: time.Second}
	current, err := cache.Open(opts, fetcherWith(host, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	if err := current.Step(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Load(opts.Path)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func logTimeOf(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// The snapshot keeps the last hundred polls and host HTTP traffic never reaches
// CPA's log, so a rate limit older than the history could not be dated. Each
// one is now a warning in CPA's log naming the provider, the credential's
// opaque index, the status, Retry-After, when the credential is tried next and
// when its provider's pause ends.
func TestARateLimitedPollIsLoggedAsAWarning(t *testing.T) {
	host := &logHost{claudeHost: newClaudeHost()}
	host.statuses[usageURL] = 429
	now := time.Now().UTC()
	snapshot := pollOnce(t, host, now)
	lines := host.logged()
	if len(lines) != 1 || lines[0].level != "warn" {
		t.Fatalf("logged %+v; want one warning", lines)
	}
	fields := lines[0].fields
	entry := snapshot.Entries[client.Key("claude", "one")]
	want := map[string]any{
		"provider": "claude", "auth_index": "one", "http_status": 429,
		"next_attempt":          logTimeOf(entry.NextAttempt),
		"provider_paused_until": logTimeOf(snapshot.ProviderCooldown["claude"]),
	}
	for key, value := range want {
		if fields[key] != value {
			t.Fatalf("%s = %v; want %v (fields %v)", key, fields[key], value, fields)
		}
	}
	// claudeHost answers with Retry-After: 3600.
	retryAfter, err := time.Parse(time.RFC3339, fmt.Sprint(fields["retry_after"]))
	if err != nil || retryAfter.Before(now.Add(time.Hour-time.Second)) || retryAfter.After(now.Add(time.Hour+time.Minute)) {
		t.Fatalf("retry_after = %v; want about an hour from now", fields["retry_after"])
	}
	if len(fields) != len(want)+1 {
		t.Fatalf("fields = %v; want exactly %d", fields, len(want)+1)
	}
}

// Any other failure backs off one credential rather than pausing its provider,
// so it is logged at info, and names only what applies to it: no Retry-After,
// no pause, and no status when no response arrived.
func TestOtherFailedPollsAreLoggedAsInfo(t *testing.T) {
	for name, tc := range map[string]struct {
		fail   func(*logHost)
		status any
	}{
		"refused":          {func(h *logHost) { h.statuses[usageURL] = 401 }, 401},
		"server error":     {func(h *logHost) { h.statuses[usageURL] = 503 }, 503},
		"transport error":  {func(h *logHost) { h.failures[usageURL] = errors.New("unreachable") }, nil},
		"credential error": {func(h *logHost) { h.authErr = errors.New("unreadable") }, nil},
	} {
		t.Run(name, func(t *testing.T) {
			host := &logHost{claudeHost: newClaudeHost()}
			tc.fail(host)
			snapshot := pollOnce(t, host, time.Now().UTC())
			lines := host.logged()
			if len(lines) != 1 || lines[0].level != "info" {
				t.Fatalf("logged %+v; want one info line", lines)
			}
			fields := lines[0].fields
			if fields["provider"] != "claude" || fields["auth_index"] != "one" || fields["http_status"] != tc.status ||
				fields["next_attempt"] != logTimeOf(snapshot.Entries[client.Key("claude", "one")].NextAttempt) {
				t.Fatalf("fields = %v", fields)
			}
			for _, absent := range []string{"retry_after", "provider_paused_until"} {
				if _, ok := fields[absent]; ok {
					t.Fatalf("fields = %v; %s does not apply", fields, absent)
				}
			}
		})
	}
}

// A log line is read by more people than the snapshot. Nothing that identifies
// the account beyond its opaque index, and nothing the provider sent back, may
// reach one, and a successful poll logs nothing at all.
func TestFailureLinesNameNothingPrivate(t *testing.T) {
	host := &logHost{claudeHost: newClaudeHost()}
	pollOnce(t, host, time.Now().UTC())
	if lines := host.logged(); len(lines) != 0 {
		t.Fatalf("a successful poll logged %+v", lines)
	}
	for _, status := range []int{401, 429} {
		host := &logHost{claudeHost: newClaudeHost()}
		host.statuses[usageURL] = status
		host.bodies[usageURL] = `{"error":"private-upstream-body-canary"}`
		pollOnce(t, host, time.Now().UTC())
		text := fmt.Sprint(host.logged())
		for _, secret := range []string{"synthetic-not-a-real-token", "Bearer", "https://", "private-upstream-body-canary",
			"claude-private-name", "private-person@example.com", "/root/"} {
			if strings.Contains(text, secret) {
				t.Fatalf("HTTP %d: the log carries %q: %s", status, secret, text)
			}
		}
	}
}

// A poll cut short by shutdown is not a failure worth a line.
func TestNoFailureIsLoggedAfterShutdown(t *testing.T) {
	host := &logHost{claudeHost: newClaudeHost()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fetcherWith(host, "").ReportFailure(ctx, cache.Failure{Provider: "claude", AuthIndex: "one", NextAttempt: time.Now()})
	if lines := host.logged(); len(lines) != 0 {
		t.Fatalf("logged %+v after shutdown", lines)
	}
}

// The writer finds the reporter by asking its fetcher, so a hostFetcher that
// stopped being one would silently stop logging.
var _ cache.FailureReporter = hostFetcher{}
