package hooks

import (
	"encoding/json"
	"reflect"
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

// TestUsageQueueBudgetHeldWriter: with the writer held (nothing takes the usage queue), the 32 MiB usage budget holds for
// every record, the metadata-only fallback included. A record whose failure body does not fit keeps its response headers
// when they fit, sheds them when they do not, and is dropped whole (usage_queue) when even the bare record does not
// fit; every queued item's Bytes covers what it holds, and the queued items' Bytes add up to the reservation. Before,
// the fallback added the headers and 512 bytes without a limit check, so header-heavy records pushed the queue past
// the budget.
func TestUsageQueueBudgetHeldWriter(t *testing.T) {
	e := running(t, "full")
	hdr := func(n int) map[string][]string { return map[string][]string{"X-Synthetic": {strings.Repeat("h", n)}} }
	call := func(body string, h map[string][]string) {
		fields := map[string]any{"RequestID": "u-held", "Provider": "claude", "Model": "model-x", "Failure": map[string]any{"Body": body}}
		if h != nil {
			fields["ResponseHeaders"] = h
		}
		raw, _ := json.Marshal(fields)
		e.UsageHook(raw)
		if b := e.usageBytes.Load(); b > UsageQueueBytes {
			t.Fatalf("usage queue reservation %d past the %d budget", b, UsageQueueBytes)
		}
	}
	// A bare record's charge: its fixed size and its metadata strings (usageMetaBytes).
	call("", nil)
	first := <-e.Usage
	e.UsageDequeued(first.Bytes)
	bare := first.Bytes
	if bare != usageMetaBytes(&first.Rec) || bare < usageRecordBytes {
		t.Fatalf("bare record charged %d, metadata %d", bare, usageMetaBytes(&first.Rec))
	}
	// Header-heavy records with failure bodies: the first fill the budget, the rest fall back.
	for i := 0; i < 40; i++ {
		call(strings.Repeat("b", 1<<20), hdr(1<<20))
	}
	// Bodyless records: headers that do not fit are shed, the bare record is kept while it fits.
	for i := 0; i < 20; i++ {
		call("", hdr(1<<20))
	}
	// Leave less than a bare record's room, then send bodyless, headerless records: dropped whole.
	if rem := UsageQueueBytes - e.usageBytes.Load(); rem > bare+100 {
		call(strings.Repeat("f", int(rem-bare-100)), nil)
	}
	if rem := UsageQueueBytes - e.usageBytes.Load(); rem >= bare {
		t.Fatalf("%d bytes left, want less than a bare record", rem)
	}
	before := e.Stats.Dropped()["usage_queue"]
	call("", nil)
	call("", hdr(64))
	if d := e.Stats.Dropped()["usage_queue"] - before; d != 2 {
		t.Fatalf("usage_queue +%d for two records that cannot fit, want +2", d)
	}
	var sum int64
	var withHeaders, bareKept int
	for len(e.Usage) > 0 {
		u := <-e.Usage
		need := retainedUsage(u.Rec)
		if u.Bytes < need {
			t.Fatalf("queued item counts %d bytes, holds %d", u.Bytes, need)
		}
		if u.Rec.ResponseHeaders != nil && u.Rec.FailureBody == "" {
			withHeaders++
		}
		if u.Rec.ResponseHeaders == nil && u.Rec.FailureBody == "" {
			bareKept++
		}
		sum += u.Bytes
	}
	if sum != e.usageBytes.Load() {
		t.Fatalf("queued items count %d bytes, reservation %d", sum, e.usageBytes.Load())
	}
	if withHeaders == 0 || bareKept == 0 {
		t.Fatalf("records kept without their body: %d with headers, %d with headers shed; want both", withHeaders, bareKept)
	}
}

// retainedUsage is what a queued usage record holds, counted independently of usageMetaBytes: every string field
// (reflection, so a field added later counts too), the response headers and the fixed size.
func retainedUsage(u protocol.Usage) int64 {
	n := int64(usageRecordBytes) + headerBytes(u.ResponseHeaders)
	v := reflect.ValueOf(u)
	for i := 0; i < v.NumField(); i++ {
		if f := v.Field(i); f.Kind() == reflect.String {
			n += int64(f.Len())
		}
	}
	return n
}

// TestUsageMetaBytesCountsEveryString: usageMetaBytes charges every string field of a usage record but the failure
// body (charged on its own), so a field added to protocol.Usage cannot escape the usage budget.
func TestUsageMetaBytesCountsEveryString(t *testing.T) {
	base := usageMetaBytes(&protocol.Usage{})
	typ := reflect.TypeOf(protocol.Usage{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type.Kind() != reflect.String || f.Name == "FailureBody" {
			continue
		}
		var u protocol.Usage
		reflect.ValueOf(&u).Elem().Field(i).SetString(strings.Repeat("m", 1000))
		if got := usageMetaBytes(&u) - base; got != 1000 {
			t.Errorf("a 1000-byte %s adds %d to the charge", f.Name, got)
		}
	}
}

// TestUsageQueueBudgetLargeMetadata (review 2 of the round-4 fixes): with the writer held, records whose metadata
// fields are large (a base URL, response model and session id of 1 MiB each) stay inside the 32 MiB usage budget by
// what they hold, not by a flat 512 bytes: after the failure body and headers are shed, each queued record's charge
// covers its metadata, the queue holds at most the budget, and the records that do not fit are dropped whole
// (usage_queue). Before, 40 such records queued about 120 MiB while charging under 64 KiB.
func TestUsageQueueBudgetLargeMetadata(t *testing.T) {
	e := running(t, "full")
	big := strings.Repeat("m", 1<<20)
	for i := 0; i < 40; i++ {
		raw, _ := json.Marshal(map[string]any{"RequestID": "u-meta", "Provider": "claude", "Model": "model-x", "BaseURL": big,
			"ResponseModel": big, "SessionID": big, "ResponseHeaders": map[string][]string{"X-Synthetic": {strings.Repeat("h", 4096)}},
			"Failure": map[string]any{"Body": strings.Repeat("b", 64<<10)}})
		e.UsageHook(raw)
		if b := e.usageBytes.Load(); b > UsageQueueBytes {
			t.Fatalf("usage queue reservation %d past the %d budget", b, UsageQueueBytes)
		}
	}
	var held, charged int64
	n := 0
	for len(e.Usage) > 0 {
		u := <-e.Usage
		r := retainedUsage(u.Rec)
		if len(u.Rec.SessionID) != 1<<20 {
			t.Fatalf("queued record kept a %d-byte session id", len(u.Rec.SessionID))
		}
		if u.Bytes < r {
			t.Fatalf("queued record charged %d bytes, holds %d", u.Bytes, r)
		}
		held, charged, n = held+r, charged+u.Bytes, n+1
	}
	if held > UsageQueueBytes || charged != e.usageBytes.Load() {
		t.Fatalf("%d queued records hold %d bytes (budget %d), charged %d, reservation %d", n, held, UsageQueueBytes, charged, e.usageBytes.Load())
	}
	if n == 0 || n == 40 || e.Stats.Dropped()["usage_queue"] == 0 {
		t.Fatalf("%d of 40 records queued, usage_queue %d: want some kept and the rest dropped", n, e.Stats.Dropped()["usage_queue"])
	}
}
