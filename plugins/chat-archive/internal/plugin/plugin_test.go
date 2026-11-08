package plugin_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/plugin"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/plugin/plugintest"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/safego"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/fingerprint"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

func mgmt(t *testing.T, p *plugin.Plugin, method, path, query string) protocol.ManagementResponse {
	t.Helper()
	q := map[string][]string{}
	for _, kv := range strings.Split(query, "&") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			q[k] = append(q[k], v)
		}
	}
	raw, _ := json.Marshal(map[string]any{"Method": method, "Path": path, "Query": q,
		"Headers": map[string][]string{"Authorization": {"Bearer management-key-canary"}}, "Body": nil})
	r, err := p.Handle(protocol.MethodManagementHandle, raw)
	if err != nil {
		t.Fatal(err)
	}
	return r.(protocol.ManagementResponse)
}

func statusJSON(t *testing.T, p *plugin.Plugin) map[string]any {
	r := mgmt(t, p, http.MethodGet, "/v0/management/plugins/chat-archive/status", "")
	if r.StatusCode != 200 {
		t.Fatalf("status %d %s", r.StatusCode, r.Body)
	}
	var m map[string]any
	json.Unmarshal(r.Body, &m)
	return m
}

func randomToken(prefix string, n int) string {
	b := make([]byte, n)
	rand.Read(b)
	const al = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	for i := range b {
		b[i] = al[int(b[i])%len(al)]
	}
	return prefix + string(b)
}

func TestRegistrationReconfigureQuiesce(t *testing.T) {
	h := plugintest.New(t, nil, "")
	p := h.Plugin
	reg, err := p.Handle(protocol.MethodPluginReconfigure, plugintest.Lifecycle(h.YAML))
	if err != nil {
		t.Fatal(err)
	}
	r := reg.(protocol.Registration)
	if r.SchemaVersion != 7 || r.Metadata.Name != "Chat Archive" || !r.Capabilities.RequestInterceptorOmitBodyAfter || !r.Capabilities.UsagePlugin {
		t.Fatalf("registration = %+v", r)
	}
	ob1, _, _ := p.Runtime()
	// Identical config: idempotent, same runtime.
	p.Handle(protocol.MethodPluginReconfigure, plugintest.Lifecycle(h.YAML))
	if ob2, _, _ := p.Runtime(); ob2 != ob1 {
		t.Fatal("identical reconfigure restarted")
	}
	// Changed config: stop and start.
	p.Handle(protocol.MethodPluginReconfigure, plugintest.Lifecycle(h.YAML+"workers: 1\n"))
	ob3, _, _ := p.Runtime()
	if ob3 == nil || ob3 == ob1 {
		t.Fatal("changed reconfigure did not restart")
	}
	// Quiesce then reconfigure (rollback) restarts cleanly.
	p.Handle(protocol.MethodPluginQuiesce, nil)
	if ob, _, _ := p.Runtime(); ob != nil {
		t.Fatal("quiesce left the runtime running")
	}
	if st, _ := p.State(); st != plugin.StateStopped {
		t.Fatalf("state after quiesce = %s", st)
	}
	if out := p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth("q1", "claude", plugintest.ClaudeBody("x"), http.Header{"X-Archive-Machine": {"m"}}, nil)); !strings.Contains(string(out), "X-Archive-Machine") {
		t.Fatalf("quiesced hook did not clear X-Archive-*: %s", out)
	}
	p.Handle(protocol.MethodPluginReconfigure, plugintest.Lifecycle(h.YAML+"workers: 1\n"))
	if st, _ := p.State(); st != plugin.StateRunning {
		t.Fatalf("state after rollback = %s", st)
	}
	// Bad config: error names the key, never the value.
	if _, err := p.Handle(protocol.MethodPluginReconfigure, plugintest.Lifecycle(h.YAML+"workers: 99\n")); err == nil || strings.Contains(err.Error(), "99") {
		t.Fatalf("bad config err = %v", err)
	}
	if _, err := p.Handle(protocol.MethodPluginRegister, []byte(`{"config_yaml":"","schema_version":6}`)); err == nil {
		t.Fatal("schema 6 accepted")
	}
}

// TestOutboxOpenRetriedInBackground: a failed outbox open leaves the plugin storage_unavailable
// (hooks pass through) and a background loop retries it, so capture comes back without a config
// change. A lifecycle call during the retry cancels it without blocking.
func TestOutboxOpenRetriedInBackground(t *testing.T) {
	old := plugin.OpenRetryMin
	plugin.OpenRetryMin = 20 * time.Millisecond
	defer func() { plugin.OpenRetryMin = old }()
	h := plugintest.New(t, nil, "")
	p := h.Plugin
	data := filepath.Join(h.Dir, "data")
	p.Handle(protocol.MethodPluginQuiesce, nil)
	if err := os.Chmod(data, 0o755); err != nil { // the hardened open refuses a group-readable directory
		t.Fatal(err)
	}
	start := time.Now()
	p.Handle(protocol.MethodPluginReconfigure, plugintest.Lifecycle(h.YAML))
	if st, why := p.State(); st != plugin.StateUnavailable || why != "outbox_unavailable" {
		t.Fatalf("state %s %s", st, why)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("lifecycle call took %v", time.Since(start))
	}
	// An identical reconfigure while the retry runs is idempotent and returns at once; a changed
	// one cancels the retry and starts a new one without deadlock.
	start = time.Now()
	p.Handle(protocol.MethodPluginReconfigure, plugintest.Lifecycle(h.YAML))
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("identical reconfigure during the retry took %v", time.Since(start))
	}
	p.Handle(protocol.MethodPluginReconfigure, plugintest.Lifecycle(h.YAML+"workers: 1\n"))
	time.Sleep(100 * time.Millisecond)
	if st, _ := p.State(); st != plugin.StateUnavailable {
		t.Fatalf("state while the directory is refused = %s", st)
	}
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	plugintest.WaitFor(t, "background open", func() bool { st, _ := p.State(); return st == plugin.StateRunning })
	p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth("retry-1", "claude", plugintest.ClaudeBody("x"), nil, nil))
	plugintest.WaitFor(t, "capture after the retry", func() bool { return len(h.Srv.Envelopes(h.SourceID)) >= 1 })
	// Shutdown during a retry cancels it (t.Cleanup runs Shutdown; a leaked loop would race).
	p.Handle(protocol.MethodPluginQuiesce, nil)
	os.Chmod(data, 0o755)
	p.Handle(protocol.MethodPluginReconfigure, plugintest.Lifecycle(h.YAML))
	p.Shutdown()
	os.Chmod(data, 0o700)
	time.Sleep(100 * time.Millisecond)
	if ob, _, _ := p.Runtime(); ob != nil {
		t.Fatal("a cancelled retry started the runtime")
	}
}

func TestUnconfiguredPassThrough(t *testing.T) {
	p := plugin.New(nil)
	defer p.Shutdown()
	if _, err := p.Handle(protocol.MethodPluginRegister, plugintest.Lifecycle("enabled: true\nstore:\n  source: x\n")); err != nil {
		t.Fatal(err)
	}
	if st, why := p.State(); st != plugin.StateUnconfigured || why != "archive_url_missing" {
		t.Fatalf("state %s %s", st, why)
	}
	out := p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth("r", "claude", []byte("{}"), http.Header{"X-Archive-Canary": {"n"}}, nil))
	var env protocol.Envelope
	json.Unmarshal(out, &env)
	if !env.OK || !strings.Contains(string(env.Result), `"ClearHeaders":["X-Archive-Canary"]`) {
		t.Fatalf("unconfigured hook = %s", out)
	}
}

func TestEndToEndCaptureMasksSecrets(t *testing.T) {
	h := plugintest.New(t, nil, "")
	p := h.Plugin
	ghp := randomToken("ghp_", 36)
	clientKey := randomToken("sk-cpa-", 40)
	marker := "ordinary-marker-" + hex.EncodeToString([]byte("zeta"))
	hdr := http.Header{"X-Archive-Machine": {"vps-synthetic"}, "Authorization": {"Bearer " + clientKey}, "Anthropic-Version": {"2023-06-01"},
		"X-Claude-Code-Session-Id": {"sess-1"}, "Content-Type": {"application/json"}}
	out := p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth("rid-1", "claude", plugintest.ClaudeBody("token "+ghp+" "+marker), hdr, plugintest.Meta("/v1/messages")))
	if !strings.Contains(string(out), "X-Archive-Machine") {
		t.Fatalf("ClearHeaders missing: %s", out)
	}
	p.Hook(protocol.MethodRequestInterceptAfter, plugintest.AfterAuth("rid-1"))
	resp := []byte(`{"id":"msg_synthetic","type":"message","role":"assistant","content":[{"type":"text","text":"reply ` + marker + `"}],"usage":{"input_tokens":1}}`)
	p.Hook(protocol.MethodResponseInterceptAfter, plugintest.Response("rid-1", "claude", resp, plugintest.Meta("/v1/messages")))
	p.Hook(protocol.MethodRequestComplete, plugintest.Complete("rid-1", "succeeded", plugintest.Meta("/v1/messages")))
	p.Hook(protocol.MethodUsageHandle, plugintest.Usage("usage-1", clientKey, "user@example.invalid", "upstream said "+ghp))
	plugintest.WaitFor(t, "five envelopes", func() bool { return len(h.Srv.Envelopes(h.SourceID)) >= 5 })
	kinds := map[wire.Kind]wire.Envelope{}
	for _, r := range h.Srv.Envelopes(h.SourceID) {
		if r.Status != wire.StatusOK {
			t.Fatalf("envelope %s got %s/%s", r.Envelope.Key, r.Status, r.Reason)
		}
		kinds[r.Envelope.Kind] = r.Envelope
	}
	for _, k := range []wire.Kind{wire.KindExchangeBegin, wire.KindExchangeAttempt, wire.KindExchangeResponse, wire.KindExchangeDone, wire.KindUsage} {
		if _, ok := kinds[k]; !ok {
			t.Fatalf("missing %s", k)
		}
	}
	var begin wire.ExchangeBegin
	json.Unmarshal(kinds[wire.KindExchangeBegin].Body, &begin)
	if begin.Hints.Machine != "vps-synthetic" || begin.CallerFP != strings.Repeat("c", 64) || begin.Body.N == 0 || begin.Kind != "chat" {
		t.Fatalf("begin = %+v", begin)
	}
	var comp wire.ExchangeCompletion
	json.Unmarshal(kinds[wire.KindExchangeDone].Body, &comp)
	if !comp.BeginWritten || comp.Responses != 1 || comp.HookMicros["before_auth"] == 0 {
		t.Fatalf("completion = %+v", comp)
	}
	var ua wire.UsageAttempt
	json.Unmarshal(kinds[wire.KindUsage].Body, &ua)
	if ua.CallerFP != fingerprint.CallerScope(clientKey) || ua.AccountFP != fingerprint.HMAC("k1", []byte(plugintest.HMACKey), "claude", "user@example.invalid") ||
		ua.SourceKind != "hashed" || ua.FailureBody == nil || strings.Contains(ua.BaseURL, "key=") {
		t.Fatalf("usage = %+v", ua)
	}
	var all strings.Builder
	for _, b := range h.Srv.RequestBodies() {
		all.Write(b)
	}
	for _, r := range h.Srv.Envelopes(h.SourceID) {
		all.Write(r.Raw)
	}
	// Outbox side: decompressed pieces and the raw database files.
	ob, _, _ := p.Runtime()
	for _, r := range h.Srv.Envelopes(h.SourceID) {
		for _, ref := range r.Envelope.Refs {
			if _, _, raw, err := ob.Piece(context.Background(), ref); err == nil {
				all.Write(raw)
			}
		}
	}
	for _, suffix := range []string{"", "-wal"} {
		if b, err := os.ReadFile(ob.Path() + suffix); err == nil {
			all.Write(b)
		}
	}
	blob := all.String()
	for _, secret := range []string{ghp, clientKey, "upstream-request-header-canary", "upstream-synthetic-secret", "auths/synthetic-user.json", "user@example.invalid"} {
		for _, form := range []string{secret, base64.StdEncoding.EncodeToString([]byte(secret))} {
			if strings.Contains(blob, form) {
				t.Errorf("secret reached the archive (%d bytes)", len(secret))
			}
		}
	}
	// Ordinary markers reach the archive (in pieces), and a placeholder sits where the secret was.
	if !piecesContain(h, marker) {
		t.Fatal("ordinary marker missing from uploaded pieces")
	}
	if !piecesContain(h, "token [masked:") {
		t.Fatal("mask placeholder missing where the secret was")
	}
	st, _ := json.Marshal(statusJSON(t, p))
	for _, s := range []string{ghp, clientKey, marker, "management-key-canary", h.Token, plugintest.HMACKey} {
		if strings.Contains(string(st), s) {
			t.Fatal("status leaks a secret or content")
		}
	}
}

func piecesContain(h *plugintest.Harness, s string) bool {
	for _, r := range h.Srv.Envelopes(h.SourceID) {
		for _, ref := range r.Envelope.Refs {
			if _, raw, ok := h.Srv.Piece(ref); ok && strings.Contains(string(raw), s) {
				return true
			}
		}
	}
	return false
}

func TestStatusNeverTouchesSQLite(t *testing.T) {
	plugin.SnapshotEvery = time.Hour
	defer func() { plugin.SnapshotEvery = 5 * time.Second }()
	h := plugintest.New(t, nil, "")
	ob, _, _ := h.Plugin.Runtime()
	time.Sleep(50 * time.Millisecond)
	before := ob.StatsCalls.Load()
	for i := 0; i < 50; i++ {
		statusJSON(t, h.Plugin)
	}
	if after := ob.StatsCalls.Load(); after != before {
		t.Fatalf("status route ran %d SQLite stats queries", after-before)
	}
	if r := mgmt(t, h.Plugin, http.MethodGet, "/v0/management/plugins/chat-archive/status", "x=1"); r.StatusCode != 400 {
		t.Fatalf("status with query = %d", r.StatusCode)
	}
	if r := mgmt(t, h.Plugin, http.MethodGet, "/v0/resource/plugins/chat-archive/status", ""); r.StatusCode != 200 || !strings.Contains(r.Headers.Get("Content-Security-Policy"), "sha256-") {
		t.Fatalf("resource = %d %v", r.StatusCode, r.Headers)
	}
	for _, path := range []string{"/v0/management/plugins/chat-archive/config", "/v0/management/plugins/chat-archive/enabled"} {
		if r := mgmt(t, h.Plugin, http.MethodGet, path, ""); r.StatusCode != 404 {
			t.Fatalf("%s = %d", path, r.StatusCode)
		}
	}
}

func TestRequeueReturns202AndRequeues(t *testing.T) {
	plugin.SnapshotEvery = 50 * time.Millisecond
	defer func() { plugin.SnapshotEvery = 5 * time.Second }()
	h := plugintest.New(t, nil, "")
	p := h.Plugin
	key := wire.ExchangeAttemptKey("rq-1", 1)
	h.Srv.RejectKey(key, wire.ReasonMaskViolation)
	p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth("rq-1", "claude", plugintest.ClaudeBody("x"), nil, nil))
	p.Hook(protocol.MethodRequestInterceptAfter, plugintest.AfterAuth("rq-1"))
	ob, _, _ := p.Runtime()
	plugintest.WaitFor(t, "dead letter", func() bool {
		d, _ := ob.DeadLetters(context.Background(), "mask_violation", 0, 10)
		return len(d) == 1
	})
	for _, q := range []string{"", "reason=nope", "reason=mask_violation&remask=2", "reason=mask_violation&full=1", "reason=mask_violation&extra=1"} {
		if r := mgmt(t, p, http.MethodPost, "/v0/management/plugins/chat-archive/requeue", q); r.StatusCode != 400 {
			t.Fatalf("query %q = %d", q, r.StatusCode)
		}
	}
	h.Srv.AcceptKey(key)
	start := time.Now()
	r := mgmt(t, p, http.MethodPost, "/v0/management/plugins/chat-archive/requeue", "reason=mask_violation&remask=1")
	if r.StatusCode != 202 || !strings.Contains(string(r.Body), `"job":"rq-`) || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("requeue = %d %s in %v", r.StatusCode, r.Body, time.Since(start))
	}
	if r2 := mgmt(t, p, http.MethodPost, "/v0/management/plugins/chat-archive/requeue", "reason=mask_violation"); r2.StatusCode != 409 {
		t.Logf("second requeue = %d (job may have finished already)", r2.StatusCode)
	}
	plugintest.WaitFor(t, "requeued envelope accepted", func() bool {
		for _, e := range h.Srv.Envelopes(h.SourceID) {
			if e.Envelope.Key == key && e.Status == wire.StatusOK {
				return true
			}
		}
		return false
	})
	jobs := statusJSON(t, p)["requeue_jobs"].([]any)
	if len(jobs) == 0 || jobs[len(jobs)-1].(map[string]any)["requeued"].(float64) != 1 {
		t.Fatalf("jobs = %v", jobs)
	}
}

func TestForcedPanicInEachGoroutine(t *testing.T) {
	safego.MinBackoff = 5 * time.Millisecond
	defer func() { safego.MinBackoff = time.Second }()
	names := []string{"worker", "stream", "writer", "uploader", "heartbeat", "media_fetch", "maintenance", "labels", "push_alert"}
	for _, n := range names {
		safego.Inject(n, 1)
	}
	h := plugintest.New(t, nil, "")
	p := h.Plugin
	plugintest.WaitFor(t, "every goroutine panicked once", func() bool {
		got := p.Engine().Stats.Panics()
		for _, n := range names {
			if got[n] == 0 {
				return false
			}
		}
		return true
	})
	st := statusJSON(t, p)
	faults, _ := json.Marshal(st["faults"])
	panics, _ := json.Marshal(st["recovered_panics"])
	types, _ := json.Marshal(st["recovered_panic_types"])
	if !strings.Contains(string(faults), "recovered_panic") || strings.Contains(string(panics), "InjectedPanic") ||
		!strings.Contains(string(types), `"worker":"safego.InjectedPanic"`) {
		t.Fatalf("faults %s panics %s types %s", faults, panics, types)
	}
	// Still capturing after the restarts.
	p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth("after-panic", "claude", plugintest.ClaudeBody("x"), nil, nil))
	plugintest.WaitFor(t, "capture after panics", func() bool { return len(h.Srv.Envelopes(h.SourceID)) >= 1 })
}

// TestStreamSpanningRestart: a stream whose chunks straddle an in-place restart (changed
// reconfigure) or a stream-goroutine panic restart still gets its completion written, and no
// reservation stays held while the runtime runs again.
func TestStreamSpanningRestart(t *testing.T) {
	safego.MinBackoff = 5 * time.Millisecond
	t.Cleanup(func() { safego.MinBackoff = time.Second }) // after the plugin's own cleanup stopped it
	h := plugintest.New(t, nil, "")
	p := h.Plugin
	e := p.Engine()
	meta := plugintest.Meta("/v1/messages")
	delta := func(s string) []byte {
		return []byte(`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + s + `"}}` + "\n\n")
	}
	completed := func(rid string) bool {
		for _, r := range h.Srv.Envelopes(h.SourceID) {
			if r.Envelope.Key == wire.ExchangeCompletionKey(rid) {
				return true
			}
		}
		return false
	}
	for i, restart := range []func(){
		func() { p.Handle(protocol.MethodPluginReconfigure, plugintest.Lifecycle(h.YAML+"workers: 1\n")) },
		func() {
			p.Handle(protocol.MethodPluginQuiesce, nil)
			p.Handle(protocol.MethodPluginReconfigure, plugintest.Lifecycle(h.YAML))
		},
		func() {
			safego.Inject("stream", 1)
			// The next stream item makes the goroutine reach its checkpoint and panic.
			p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk("nudge", "claude", -1, nil, meta))
			plugintest.WaitFor(t, "stream goroutine panic", func() bool { return e.Stats.Panics()["stream"] > 0 })
		},
	} {
		rid := "span-" + string(rune('a'+i))
		p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth(rid, "claude", plugintest.ClaudeBody("x"), nil, meta))
		p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(rid, "claude", -1, nil, meta))
		p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(rid, "claude", 0, delta("before"), meta))
		time.Sleep(50 * time.Millisecond) // processed by the current stream goroutine
		restart()
		p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(rid, "claude", 1, delta("after"), meta))
		p.Hook(protocol.MethodRequestComplete, plugintest.Complete(rid, "canceled", meta))
		plugintest.WaitFor(t, "completion of "+rid, func() bool { return completed(rid) })
	}
	p.Hook(protocol.MethodRequestComplete, plugintest.Complete("nudge", "canceled", meta))
	plugintest.WaitFor(t, "pool released while running", func() bool { return e.Pool.Used() == 0 && e.Pool.StreamUsed() == 0 })
	if st, _ := p.State(); st != plugin.StateRunning {
		t.Fatalf("state = %s", st)
	}
}

func TestModelListIgnored(t *testing.T) {
	h := plugintest.New(t, nil, "")
	h.Plugin.Hook(protocol.MethodResponseInterceptAfter, plugintest.ModelList("ml-1"))
	h.Plugin.Hook(protocol.MethodRequestComplete, []byte(`{"RequestID":"ml-1","Model":"","Metadata":null,"Outcome":"succeeded"}`))
	if n := h.Plugin.Engine().Stats.Other()["ignored_model_list"]; n != 2 {
		t.Fatalf("ignored_model_list = %d", n)
	}
	if h.Plugin.Engine().Entries() != 0 {
		t.Fatal("model list left state")
	}
}

func TestHeartbeatReachesArchive(t *testing.T) {
	h := plugintest.New(t, nil, "")
	h.Plugin.Shutdown()
	hbs := h.Srv.Heartbeats(h.SourceID)
	if len(hbs) == 0 {
		t.Fatal("no heartbeat")
	}
	last := hbs[len(hbs)-1]
	if last.Component != "cpa-plugin" || last.State != "stopping" || last.MaskVersion != "mask/v1" || last.HostVersion != "schema:10" || last.OutboxID == "" {
		t.Fatalf("heartbeat = %+v", last)
	}
	b, _ := json.Marshal(hbs)
	if strings.Contains(string(b), h.Token) || strings.Contains(string(b), plugintest.HMACKey) {
		t.Fatal("heartbeat leaks a secret")
	}
	_ = hooks.OK
}
