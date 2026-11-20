package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/cache"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/quota"
)

// diskEntry is a credential as CPA lists it before its auth manager is
// attached, read straight from the auth directory by listAuthFilesFromDisk:
// named, typed and located, with no auth_index.
func diskEntry(name, kind string) protocol.HostAuthFileEntry {
	return protocol.HostAuthFileEntry{Name: name, Type: kind, Provider: kind, Source: "file",
		Path: "/root/.cli-proxy-api/" + name, Status: "active"}
}

// rosterHost lists a fixed roster and holds no credentials.
type rosterHost struct{ roster []protocol.HostAuthFileEntry }

func (h rosterHost) ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error) {
	return h.roster, nil
}
func (rosterHost) GetAuth(context.Context, string) ([]byte, error) {
	panic("unexpected credential read")
}
func (rosterHost) HTTPDo(context.Context, protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	panic("unexpected HTTP")
}
func (rosterHost) Log(context.Context, string, string, map[string]any) {}

// While CPA is still starting, its roster names credentials the cache polls
// but gives none of them the auth_index snapshot entries are keyed by. Listing
// nothing then would read as every credential having been removed.
func TestListWaitsForTheRosterCPAHasNotLoadedYet(t *testing.T) {
	disabled := diskEntry("claude-off.json", "claude")
	disabled.Disabled = true
	for name, roster := range map[string][]protocol.HostAuthFileEntry{
		"claude": {diskEntry("claude-one.json", "claude")},
		"codex":  {diskEntry("codex-two.json", "codex")},
		// The type alone names the provider, normalized as for any entry.
		"type only": {{Name: "codex-three.json", Type: " Codex ", Source: "file", Path: "/root/.cli-proxy-api/codex-three.json"}},
		"beside unsupported and disabled": {
			diskEntry("gemini.json", "gemini"), disabled, diskEntry("claude-one.json", "claude"),
		},
	} {
		// The OpenRouter account waits with the rest: listing it alone would
		// retire every CPA credential's entry just the same.
		for _, key := range []string{"", "sk-or-v1-secret"} {
			accounts, err := fetcherWith(rosterHost{roster}, key).List(context.Background())
			if !errors.Is(err, cache.ErrRosterNotReady) || accounts != nil {
				t.Errorf("%s, key %q: accounts=%+v err=%v; want ErrRosterNotReady", name, key, accounts, err)
			}
		}
	}
}

// Every roster CPA can report once its auth manager is attached is listed as
// before: entries without an auth_index are skipped, and only a credential the
// cache would poll can make the roster look unfinished.
func TestListKeepsRostersThatAreAlreadyAuthoritative(t *testing.T) {
	indexed := diskEntry("claude-one.json", "claude")
	indexed.AuthIndex, indexed.Source = "one", "memory"
	disabled := diskEntry("claude-off.json", "claude")
	disabled.Disabled = true
	runtimeOnly := protocol.HostAuthFileEntry{Name: "codex-runtime", Provider: "codex", RuntimeOnly: true}
	key := "sk-or-v1-secret"
	openRouter := cache.Account{Provider: quota.OpenRouterProvider, AuthIndex: openRouterAccount(key)}
	for _, tc := range []struct {
		name   string
		roster []protocol.HostAuthFileEntry
		key    string
		want   []cache.Account
	}{
		{"empty", nil, "", []cache.Account{}},
		{"unsupported only", []protocol.HostAuthFileEntry{diskEntry("gemini.json", "gemini"), diskEntry("vertex.json", "vertex")}, "", []cache.Account{}},
		{"disabled only", []protocol.HostAuthFileEntry{disabled}, "", []cache.Account{}},
		{"runtime only", []protocol.HostAuthFileEntry{runtimeOnly}, "", []cache.Account{}},
		{"mixed", []protocol.HostAuthFileEntry{indexed, diskEntry("codex-two.json", "codex")}, "", []cache.Account{{Provider: "claude", AuthIndex: "one"}}},
		{"openrouter only", nil, key, []cache.Account{openRouter}},
		{"openrouter beside unsupported", []protocol.HostAuthFileEntry{diskEntry("gemini.json", "gemini")}, key, []cache.Account{openRouter}},
		{"openrouter beside indexed", []protocol.HostAuthFileEntry{indexed}, key, []cache.Account{{Provider: "claude", AuthIndex: "one"}, openRouter}},
	} {
		accounts, err := fetcherWith(rosterHost{tc.roster}, tc.key).List(context.Background())
		if err != nil || !reflect.DeepEqual(accounts, tc.want) {
			t.Errorf("%s: accounts=%+v err=%v; want %+v", tc.name, accounts, err, tc.want)
		}
	}
}

// restartHost is claudeHost as CPA presents it across a restart: the first
// listing is read from the auth directory before the auth manager is attached,
// and every later one comes from the manager.
type restartHost struct {
	*claudeHost
	lists    atomic.Int32
	warnings atomic.Int32
}

func (h *restartHost) ListAuth(ctx context.Context) ([]protocol.HostAuthFileEntry, error) {
	if h.lists.Add(1) == 1 {
		return []protocol.HostAuthFileEntry{diskEntry("claude-one.json", "claude")}, nil
	}
	return h.claudeHost.ListAuth(ctx)
}
func (h *restartHost) Log(_ context.Context, level, _ string, _ map[string]any) {
	if level == "warn" {
		h.warnings.Add(1)
	}
}

// End to end through registration and the real poller, as on every CPA
// start: the snapshot a previous run wrote, with its observation and account
// details, comes through the startup roster byte for byte, and the real roster
// that follows finds nothing due and asks no provider anything.
func TestASnapshotSurvivesTheRosterCPAListsWhileStarting(t *testing.T) {
	host := newClaudeHost()
	path := filepath.Join(t.TempDir(), "cache", "snapshot.json")
	previous, err := cache.Open(cache.Options{Path: path, Interval: 15 * time.Minute, Spacing: time.Second}, fetcherWith(host, ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := previous.Step(context.Background(), time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	previous.Close()
	seeded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if host.count(usageURL) != 1 || host.count(profileURL) != 1 {
		t.Fatalf("setup: usage %d profile %d", host.count(usageURL), host.count(profileURL))
	}

	restarted := &restartHost{claudeHost: host}
	p := New(restarted)
	defer p.Shutdown()
	raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("cache-path: " + path + "\nrequest-spacing: 1s\n")})
	if _, err := p.Handle(protocol.MethodPluginRegister, raw); err != nil {
		t.Fatal(err)
	}
	current := p.cache
	// A third listing starts only once the second, the first from the auth
	// manager, has been acted on.
	deadline := time.Now().Add(10 * time.Second)
	for restarted.lists.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatal("the poller stopped listing credentials")
		}
		time.Sleep(20 * time.Millisecond)
	}
	activity := current.Activity()
	p.Shutdown()

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(seeded, after) {
		t.Fatalf("the startup roster rewrote the snapshot:\nbefore %s\nafter  %s", seeded, after)
	}
	if host.count(usageURL) != 1 || host.count(profileURL) != 1 {
		t.Fatalf("usage asked %d times and profile %d; want 1 and 1", host.count(usageURL), host.count(profileURL))
	}
	entry, err := client.ReadFresh(path, "claude", "one", time.Now().UTC(), 30*time.Minute)
	if err != nil || entry.Percent != 40 {
		t.Fatalf("entry=%+v err=%v", entry, err)
	}
	snapshot, err := client.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if details := snapshot.Entries[client.Key("claude", "one")].AccountDetails; details == nil || details.Plan != "max_20x" {
		t.Fatalf("account details=%+v", details)
	}
	if activity.Error != "" || activity.Accounts != 1 || activity.LastScan.IsZero() {
		t.Fatalf("activity=%+v; want the real roster scanned without error", activity)
	}
	if n := restarted.warnings.Load(); n != 0 {
		t.Fatalf("%d warnings logged for an expected startup wait", n)
	}
}

// failingHost cannot list credentials at all.
type failingHost struct{ warnings atomic.Int32 }

func (*failingHost) ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error) {
	return nil, errors.New("host unavailable")
}
func (*failingHost) GetAuth(context.Context, string) ([]byte, error) {
	return nil, errors.New("unexpected credential read")
}
func (*failingHost) HTTPDo(context.Context, protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	return protocol.HostHTTPResponse{}, errors.New("unexpected HTTP")
}
func (h *failingHost) Log(_ context.Context, level, _ string, _ map[string]any) {
	if level == "warn" {
		h.warnings.Add(1)
	}
}

// Only the startup wait is kept out of the log; a host that cannot list
// credentials is still a warning.
func TestADiscoveryFailureIsStillLogged(t *testing.T) {
	host := &failingHost{}
	p := New(host)
	defer p.Shutdown()
	path := filepath.Join(t.TempDir(), "cache", "snapshot.json")
	raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("cache-path: " + path + "\n")})
	if _, err := p.Handle(protocol.MethodPluginRegister, raw); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for host.warnings.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a discovery failure was not logged")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A startup wait stays out of the log, but one that outlasts any real CPA
// start is reported, once per wait, so a snapshot that has quietly stopped
// updating shows up in CPA's log.
func TestALastingWaitIsWarnedOnce(t *testing.T) {
	var w rosterWait
	start := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	notReady := fmt.Errorf("host: %w", cache.ErrRosterNotReady)
	for _, step := range []struct {
		name  string
		err   error
		after time.Duration
		warn  bool
	}{
		{"first wait", notReady, 0, false},
		{"still starting", notReady, rosterWaitWarnAfter - time.Second, false},
		{"lasting", notReady, rosterWaitWarnAfter, true},
		{"already warned", notReady, 2 * rosterWaitWarnAfter, false},
		// Any other result ends the wait, so the next one starts its own clock.
		{"real roster", nil, 2*rosterWaitWarnAfter + time.Second, false},
		{"next wait", notReady, 3 * rosterWaitWarnAfter, false},
		{"next wait still starting", notReady, 4*rosterWaitWarnAfter - time.Second, false},
		{"next wait lasting", notReady, 4 * rosterWaitWarnAfter, true},
		{"discovery failure", errors.New("credential discovery failed"), 5 * rosterWaitWarnAfter, false},
		{"after a failure", notReady, 6*rosterWaitWarnAfter - time.Second, false},
	} {
		if got := w.observe(step.err, start.Add(step.after)); got != step.warn {
			t.Fatalf("%s: warn=%v; want %v", step.name, got, step.warn)
		}
	}
}
