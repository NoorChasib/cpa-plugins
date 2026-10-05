package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/cache"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/quota"
)

// openRouterHost answers the credits endpoint and records what was asked. It
// has no credentials of its own: the OpenRouter account is not one CPA holds.
type openRouterHost struct {
	mu       sync.Mutex
	requests []protocol.HostHTTPRequest
}

func (*openRouterHost) ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error) {
	return nil, nil
}
func (*openRouterHost) GetAuth(context.Context, string) ([]byte, error) {
	panic("the OpenRouter account must not read a CPA credential")
}
func (h *openRouterHost) HTTPDo(_ context.Context, req protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.requests = append(h.requests, req)
	return protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"data":{"total_credits":20,"total_usage":7.5}}`)}, nil
}
func (*openRouterHost) Log(context.Context, string, string, map[string]any) {}

func (h *openRouterHost) sent() []protocol.HostHTTPRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]protocol.HostHTTPRequest(nil), h.requests...)
}

func fetcherWith(host Host, key string) hostFetcher {
	holder := &atomic.Pointer[string]{}
	holder.Store(&key)
	return hostFetcher{host: host, openRouterKey: holder}
}

func TestOpenRouterIsListedOnlyWhenAKeyIsConfigured(t *testing.T) {
	host := &openRouterHost{}
	accounts, err := fetcherWith(host, "").List(context.Background())
	if err != nil || len(accounts) != 0 {
		t.Fatalf("accounts=%+v err=%v; no key means no OpenRouter account", accounts, err)
	}
	accounts, err = fetcherWith(host, "sk-or-v1-secret").List(context.Background())
	if err != nil || len(accounts) != 1 || accounts[0].Provider != quota.OpenRouterProvider {
		t.Fatalf("accounts=%+v err=%v", accounts, err)
	}
}

// The account name reaches the snapshot and the status page, so it is a
// fingerprint of the key and never any part of it.
func TestOpenRouterAccountIsAFingerprintNotTheKey(t *testing.T) {
	key := "sk-or-v1-0123456789abcdef0123456789abcdef"
	id := openRouterAccount(key)
	if !strings.HasPrefix(id, "key-") || len(id) != len("key-")+12 {
		t.Fatalf("id=%q", id)
	}
	for i := 0; i+6 <= len(key); i++ {
		if strings.Contains(id, key[i:i+6]) {
			t.Fatalf("id %q contains part of the key", id)
		}
	}
	if openRouterAccount(key) != id {
		t.Fatal("fingerprint is not stable")
	}
	if openRouterAccount(key+"x") == id {
		t.Fatal("a rotated key kept the old account name")
	}
}

func TestOpenRouterFetchSendsTheConfiguredKey(t *testing.T) {
	host := &openRouterHost{}
	f := fetcherWith(host, "sk-or-v1-secret")
	accounts, _ := f.List(context.Background())
	observation, err := f.Fetch(context.Background(), accounts[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	sent := host.sent()
	if len(sent) != 1 || sent[0].Headers["Authorization"][0] != "Bearer sk-or-v1-secret" || !observation.RequestSent || observation.HTTPStatus != 200 {
		t.Fatalf("sent=%+v observation=%+v", sent, observation)
	}
	if balance := observation.Quota.Balances["credits"]; balance.Limit != "20" || balance.Used != "7.5" {
		t.Fatalf("balance=%+v", balance)
	}
}

// A scan lists the account, then the operator rotates or removes the key
// before its turn comes. The stale account must not be polled with a key it no
// longer names.
func TestOpenRouterFetchRefusesAnAccountFromARetiredKey(t *testing.T) {
	host := &openRouterHost{}
	f := fetcherWith(host, "old-key")
	accounts, _ := f.List(context.Background())
	for _, replacement := range []string{"new-key", ""} {
		f.openRouterKey.Store(&replacement)
		if _, err := f.Fetch(context.Background(), accounts[0], nil); err == nil {
			t.Fatalf("key %q: fetched an account listed under a retired key", replacement)
		}
	}
	if len(host.sent()) != 0 {
		t.Fatal("a request was sent for a retired key")
	}
}

// End to end through configure and the real writer: adding the key starts
// polling on the running cache, and removing it retires the entry, with no
// restart either way. The key itself never reaches the snapshot.
func TestOpenRouterKeyIsLiveAndNeverPersisted(t *testing.T) {
	host := &openRouterHost{}
	p := New(host)
	defer p.Shutdown()
	path := filepath.Join(t.TempDir(), "cache", "snapshot.json")
	secret := "sk-or-v1-SECRET-MANAGEMENT-KEY"
	configure := func(method, key string) {
		t.Helper()
		yaml := "cache-path: " + path + "\nrequest-spacing: 1s\n"
		if key != "" {
			yaml += "openrouter-management-key: " + key + "\n"
		}
		raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte(yaml)})
		if _, err := p.Handle(method, raw); err != nil {
			t.Fatal(err)
		}
	}
	configure(protocol.MethodPluginRegister, "")
	writer := p.cache
	configure(protocol.MethodPluginReconfigure, secret)
	if p.cache != writer {
		t.Fatal("adding the key restarted the writer")
	}
	key := client.Key(quota.OpenRouterProvider, openRouterAccount(secret))
	snapshot := waitFor(t, path, func(s client.Snapshot) bool {
		entry, ok := s.Entries[key]
		return ok && entry.Quota != nil && entry.LastError == ""
	})
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), secret) {
		t.Fatal("the management key reached the snapshot")
	}
	if snapshot.Entries[key].Quota.Balances["credits"].Used != "7.5" {
		t.Fatalf("entry=%+v", snapshot.Entries[key])
	}
	configure(protocol.MethodPluginReconfigure, "")
	waitFor(t, path, func(s client.Snapshot) bool {
		_, ok := s.Entries[key]
		return !ok
	})
}

func TestOpenRouterKeyIsAConfigField(t *testing.T) {
	p := New(&testHost{})
	defer p.Shutdown()
	raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("enabled: false\n")})
	result, err := p.Handle(protocol.MethodPluginRegister, raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range result.(protocol.Registration).Metadata.ConfigFields {
		if field.Name == "openrouter-management-key" && field.Type == "string" {
			return
		}
	}
	t.Fatal("openrouter-management-key is not offered in the configuration panel")
}

func waitFor(t *testing.T, path string, done func(client.Snapshot) bool) client.Snapshot {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s, err := client.Load(path); err == nil && done(s) {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("snapshot never reached the expected state")
	return client.Snapshot{}
}

var _ cache.Fetcher = hostFetcher{}
