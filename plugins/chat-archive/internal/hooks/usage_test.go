package hooks

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/fingerprint"
)

// labelStub is the label loop's cache as usage.handle sees it.
type labelStub struct {
	mu      sync.Mutex
	entries map[string][2]string // AuthIndex -> email, project
	wanted  []string
}

func (l *labelStub) Lookup(idx string) (string, string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.entries[idx]
	return v[0], v[1], ok
}

func (l *labelStub) Want(idx string) {
	l.mu.Lock()
	l.wanted = append(l.wanted, idx)
	l.mu.Unlock()
}

// usageCall runs one usage.handle and returns the queued record.
func usageCall(t *testing.T, e *Engine, fields map[string]any) protocol.Usage {
	t.Helper()
	base := map[string]any{"RequestID": "u-1", "Provider": "claude", "Model": "model-x", "APIKey": "sk-cpa-synthetic-client-key",
		"AuthIndex": "idx-1", "AuthType": "oauth", "Source": "", "RequestedAt": time.Now().UTC(), "Failure": map[string]any{}}
	for k, v := range fields {
		base[k] = v
	}
	raw, _ := json.Marshal(base)
	e.UsageHook(raw)
	select {
	case u := <-e.Usage:
		e.UsageDequeued(u.Bytes)
		return u.Rec
	default:
		t.Fatal("no usage item queued")
	}
	return protocol.Usage{}
}

// TestUsageSourceRules covers the SourceClear rule (Source kept clear only when it equals the
// auth's runtime email or project) and four usage cases that need OAuth credentials the native
// mock cannot back, labelled as in the native canary list: U4 (plugin-executor Source), U5 (OAuth
// without email), U6 (Source equal to the runtime email or project, kept clear: the positive
// control) and U9 (an AuthID path that holds an email).
func TestUsageSourceRules(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	labels := &labelStub{entries: map[string][2]string{
		"idx-email":   {"person@example.invalid", "project-synthetic-1"},
		"idx-noemail": {"", ""},
	}}
	e := NewEngine()
	e.SetConfig(&Config{Configured: true, Capture: "full", MaxRPC: 8 << 20, HMACKeyID: "k1", HMACKey: key, Labels: labels})
	e.Start()
	acct := func(provider, source string) string { return fingerprint.HMAC("k1", key, provider, source) }

	cases := []struct {
		name   string
		fields map[string]any
		kind   string
		clear  string
	}{
		{"U6 email match kept clear", map[string]any{"AuthIndex": "idx-email", "Source": "person@example.invalid"}, "email", "person@example.invalid"},
		{"U6 project match kept clear", map[string]any{"AuthIndex": "idx-email", "Source": "project-synthetic-1"}, "project", "project-synthetic-1"},
		{"mismatch hashed", map[string]any{"AuthIndex": "idx-email", "Source": "other@example.invalid"}, "hashed", ""},
		{"U5 OAuth without email", map[string]any{"AuthIndex": "idx-noemail", "Source": "oauth-subject-synthetic"}, "hashed", ""},
		{"U5 empty Source", map[string]any{"AuthIndex": "idx-noemail", "Source": ""}, "empty", ""},
		{"U4 plugin executor", map[string]any{"AuthIndex": "", "ExecutorType": "plugin:synthetic-executor", "AuthType": "plugin",
			"Source": "plugin-executor-source-synthetic"}, "hashed", ""},
		{"apikey Source", map[string]any{"AuthIndex": "idx-email", "AuthType": "apikey", "Source": "sk-ant-synthetic-upstream-key"}, "hashed", ""},
	}
	for _, c := range cases {
		u := usageCall(t, e, c.fields)
		if u.SourceKind != c.kind || u.SourceClear != c.clear {
			t.Errorf("%s: kind %q clear %q, want %q %q", c.name, u.SourceKind, u.SourceClear, c.kind, c.clear)
		}
		src, _ := c.fields["Source"].(string)
		if u.AccountFP != acct("claude", src) {
			t.Errorf("%s: account_fp is not HMAC(provider, Source)", c.name)
		}
		if u.CallerFP != fingerprint.CallerScope("sk-cpa-synthetic-client-key") {
			t.Errorf("%s: caller_fp wrong", c.name)
		}
		b, _ := json.Marshal(u)
		for _, secret := range []string{"sk-cpa-synthetic-client-key", "sk-ant-synthetic-upstream-key", "plugin-executor-source-synthetic", "oauth-subject-synthetic", "other@example.invalid"} {
			if strings.Contains(string(b), secret) {
				t.Errorf("%s: a raw secret or hashed Source stayed in the record", c.name)
			}
		}
	}

	// Uncached AuthIndex: hashed, and queued for the label loop.
	u := usageCall(t, e, map[string]any{"AuthIndex": "idx-unknown", "Source": "person@example.invalid"})
	if u.SourceKind != "hashed" || u.SourceClear != "" {
		t.Fatalf("uncached: %+v", u)
	}
	if labels.wanted[len(labels.wanted)-1] != "idx-unknown" {
		t.Fatalf("uncached AuthIndex not queued: %v", labels.wanted)
	}

	// U9: an AuthID path that holds an email becomes HMAC(AuthID) only.
	authID := "auths/person@example.invalid-claude.json"
	u = usageCall(t, e, map[string]any{"AuthID": authID, "Source": ""})
	if u.AuthFP != fingerprint.HMAC("k1", key, authID) {
		t.Fatalf("auth_fp = %q", u.AuthFP)
	}
	if b, _ := json.Marshal(u); strings.Contains(string(b), "person@example.invalid") || strings.Contains(string(b), "auths/") {
		t.Fatal("AuthID path reached the record")
	}
}
