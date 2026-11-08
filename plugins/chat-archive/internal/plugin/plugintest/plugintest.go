// Package plugintest builds synthetic CPA hook RPCs and a configured plugin against the fake
// archive (pkg/client/clienttest), for tests only. Every value is invented.
package plugintest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/labels"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/plugin"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/client/clienttest"
)

// HMACKey is the synthetic HMAC key the harness configures.
const HMACKey = "synthetic-hmac-key-0123456789abcdefghijklmnop"

// Harness is a plugin configured against a fake archive.
type Harness struct {
	Srv      *clienttest.Server
	SourceID string
	Token    string
	Plugin   *plugin.Plugin
	Dir      string
	YAML     string
}

// New starts a fake archive and registers a plugin with extra YAML appended to the config.
func New(t testing.TB, host labels.Host, extra string) *Harness {
	t.Helper()
	h := &Harness{Srv: clienttest.NewServer(clienttest.Options{}), Dir: t.TempDir()}
	t.Cleanup(h.Srv.Close)
	h.SourceID, h.Token = h.Srv.AddSource("cpa-plugin", "test")
	os.Setenv("CA_TEST_SOURCE_TOKEN", h.Token)
	os.Setenv("CA_TEST_HMAC_KEY", HMACKey)
	h.YAML = fmt.Sprintf("enabled: true\npriority: 100\narchive-url: %s\nsource-token-env: CA_TEST_SOURCE_TOKEN\nhmac-key-env: CA_TEST_HMAC_KEY\ndata-dir: %s\n%s",
		h.Srv.URL, filepath.Join(h.Dir, "data"), extra)
	h.Plugin = plugin.New(host)
	if _, err := h.Plugin.Handle(protocol.MethodPluginRegister, Lifecycle(h.YAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(h.Plugin.Shutdown)
	return h
}

// Lifecycle returns a plugin.register / reconfigure body.
func Lifecycle(yaml string) []byte {
	b, _ := json.Marshal(protocol.LifecycleRequest{ConfigYAML: []byte(yaml), SchemaVersion: 10})
	return b
}

func enc(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// Meta is a synthetic Metadata map.
func Meta(path string) map[string]any {
	return map[string]any{"request_path": path, "generate": true, "reasoning_effort": "high", "service_tier": "auto", "caller_scope": strings.Repeat("c", 64)}
}

// BeforeAuth builds request.intercept_before.
func BeforeAuth(rid, format string, body []byte, headers http.Header, meta map[string]any) []byte {
	return enc(map[string]any{"RequestID": rid, "TraceID": "trace-" + rid, "SourceFormat": format, "ToFormat": "", "Model": "model-x",
		"RequestedModel": "model-x", "Stream": false, "Headers": headers, "Body": body, "CallerScope": strings.Repeat("c", 64), "Metadata": meta,
		"host_callback_id": "cb"})
}

// AfterAuth builds request.intercept_after (body omitted).
func AfterAuth(rid string) []byte {
	m := Meta("/v1/messages")
	m["selected_auth_id"] = "auths/synthetic-user.json"
	m["selected_auth_index"] = "0123456789abcdef"
	m["canonical_session_id"] = "claude:synthetic-session"
	return enc(map[string]any{"RequestID": rid, "ToFormat": "claude", "Model": "model-x", "Body": nil, "Metadata": m,
		"Headers": map[string][]string{"Authorization": {"Bearer upstream-synthetic-secret"}}})
}

// Response builds response.intercept_after; the executed RequestHeaders carry a canary.
func Response(rid, format string, body []byte, meta map[string]any) []byte {
	return enc(map[string]any{"RequestID": rid, "SourceFormat": format, "Model": "model-x", "RequestedModel": "model-x", "Stream": false,
		"RequestHeaders":  map[string][]string{"X-Api-Key": {"upstream-request-header-canary"}},
		"ResponseHeaders": map[string][]string{"Content-Type": {"application/json"}, "Request-Id": {"req_synthetic"}},
		"OriginalRequest": []byte("{}"), "RequestBody": []byte("{}"), "Body": body, "StatusCode": 200, "Metadata": meta})
}

// ModelList builds the response hook call of a model-list GET.
func ModelList(rid string) []byte {
	return enc(map[string]any{"RequestID": rid, "SourceFormat": "openai", "Model": "", "RequestedModel": "", "Stream": false,
		"OriginalRequest": nil, "RequestBody": nil, "Body": []byte(`{"data":[]}`), "StatusCode": 200, "Metadata": nil})
}

// Chunk builds response.intercept_stream_chunk (index -1 is header-init).
func Chunk(rid, format string, index int, body []byte, meta map[string]any) []byte {
	m := map[string]any{"RequestID": rid, "SourceFormat": format, "Model": "model-x", "RequestedModel": "model-x", "ChunkIndex": index,
		"RequestHeaders": map[string][]string{"Authorization": {"Bearer upstream-request-header-canary"}}, "Metadata": meta}
	if index < 0 {
		m["ResponseHeaders"] = map[string][]string{"Content-Type": {"text/event-stream"}}
		m["OriginalRequest"], m["RequestBody"] = []byte("{}"), []byte("{}")
	} else {
		m["Body"] = body
	}
	return enc(m)
}

// Complete builds request.complete.
func Complete(rid, outcome string, meta map[string]any) []byte {
	return CompleteStatus(rid, outcome, 200, "", meta)
}

// CompleteStatus builds request.complete with an explicit status code and error text.
func CompleteStatus(rid, outcome string, status int, errText string, meta map[string]any) []byte {
	now := time.Now().UTC()
	return enc(map[string]any{"RequestID": rid, "TraceID": "trace-" + rid, "SourceFormat": "claude", "Model": "model-x", "RequestedModel": "model-x",
		"Stream": false, "Outcome": outcome, "StatusCode": status, "Error": errText, "StartedAt": now.Add(-time.Second), "CompletedAt": now, "Metadata": meta})
}

// Usage builds usage.handle; apiKey and source are the secret fields.
func Usage(urid, apiKey, source string, failureBody string) []byte {
	return enc(map[string]any{"RequestID": urid, "TraceID": "trace", "Provider": "claude", "BaseURL": "https://api.invalid/v1?key=q",
		"ExecutorType": "claude", "Model": "model-x", "APIKey": apiKey, "IsNativeKey": true, "AccessProvider": "config-inline",
		"AuthID": "auths/synthetic-user.json", "AuthIndex": "0123456789abcdef", "AuthType": "oauth", "Source": source,
		"Generate": true, "Stream": false, "RequestedAt": time.Now().UTC(), "Latency": int64(time.Second), "TTFT": int64(time.Millisecond),
		"Failed": failureBody != "", "Failure": map[string]any{"StatusCode": 500, "Body": failureBody},
		"Detail": map[string]any{"InputTokens": 10, "OutputTokens": 5, "TotalTokens": 15}, "ResponseHeaders": map[string][]string{"Retry-After": {"1"}}})
}

// ClaudeBody is a synthetic Claude Messages request.
func ClaudeBody(text string) []byte {
	return enc(map[string]any{"model": "model-x", "max_tokens": 16, "system": "synthetic system prompt",
		"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}}}})
}

// WaitFor polls cond for up to 15 s.
func WaitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
