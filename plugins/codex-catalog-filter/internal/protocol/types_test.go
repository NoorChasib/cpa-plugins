package protocol

import (
	"bytes"
	"encoding/json"
	"net/http"
	"runtime"
	"strings"
	"testing"
)

const claudeList = `{"data":[{"id":"claude-a"}],"first_id":"claude-a","has_more":false,"last_id":"claude-a"}`

// listRequest is response.intercept_after as CPA's WriteModelListResponse
// sends it, fields in CPA's struct order, with overrides applied.
func listRequest(t *testing.T, overrides map[string]any) []byte {
	t.Helper()
	fields := []struct {
		key   string
		value any
	}{
		{"RequestID", "req-1"},
		{"SourceFormat", "claude"},
		{"Model", ""},
		{"RequestedModel", ""},
		{"Stream", false},
		{"RequestHeaders", http.Header{"User-Agent": {"claude-code/2.1.327"}, "Anthropic-Version": {"2023-06-01"}, "Authorization": {"Bearer secret"}}},
		{"ResponseHeaders", http.Header{"Content-Type": {"application/json; charset=utf-8"}}},
		{"OriginalRequest", nil},
		{"RequestBody", nil},
		{"Body", []byte(claudeList)},
		{"StatusCode", 200},
		{"Metadata", map[string]any{"caller_scope": "abc"}},
		{"host_callback_id", "cb-1"},
	}
	var out bytes.Buffer
	out.WriteByte('{')
	for i, f := range fields {
		value := f.value
		if v, ok := overrides[f.key]; ok {
			value = v
		}
		if i > 0 {
			out.WriteByte(',')
		}
		k, _ := json.Marshal(f.key)
		v, _ := json.Marshal(value)
		out.Write(k)
		out.WriteByte(':')
		out.Write(v)
	}
	out.WriteByte('}')
	return out.Bytes()
}

func TestScanAcceptsOnlyClaudeCodeModelLists(t *testing.T) {
	got, ok := ScanClaudeListIntercept(listRequest(t, nil), "codex-catalog-filter/")
	if !ok || string(got.Body) != claudeList || got.UserAgent != "claude-code/2.1.327" {
		t.Fatalf("list = %+v %v", got, ok)
	}
	if _, ok := ScanClaudeListIntercept(listRequest(t, map[string]any{"RequestHeaders": http.Header{"User-Agent": {"claude-cli/2.1.327 (external, sdk-cli)"}}}), ""); !ok {
		t.Fatal("claude-cli agent rejected")
	}
	if _, ok := ScanClaudeListIntercept(listRequest(t, map[string]any{"OriginalRequest": []byte{}, "RequestBody": ""}), ""); !ok {
		t.Fatal("empty bodies rejected")
	}
	for name, overrides := range map[string]map[string]any{
		"openai format":    {"SourceFormat": "openai"},
		"gemini format":    {"SourceFormat": "gemini"},
		"inference":        {"Model": "claude-opus-5-5"},
		"requested model":  {"RequestedModel": "claude-opus-5-5"},
		"stream":           {"Stream": true},
		"request body":     {"RequestBody": []byte(`{"model":"x"}`)},
		"original request": {"OriginalRequest": []byte(`{"model":"x"}`)},
		"status":           {"StatusCode": 201},
		"other agent":      {"RequestHeaders": http.Header{"User-Agent": {"Mozilla/5.0"}}},
		"no agent":         {"RequestHeaders": http.Header{}},
		"own agent":        {"RequestHeaders": http.Header{"User-Agent": {"codex-catalog-filter/0.2.0"}}},
		"null body":        {"Body": nil},
		"empty body":       {"Body": []byte{}},
		"no status":        {"StatusCode": "200"},
	} {
		if _, ok := ScanClaudeListIntercept(listRequest(t, overrides), "codex-catalog-filter/"); ok {
			t.Errorf("%s accepted", name)
		}
	}
	// The plugin's own agent is refused even when it looks like Claude Code.
	if _, ok := ScanClaudeListIntercept(listRequest(t, map[string]any{"RequestHeaders": http.Header{"User-Agent": {"claude-code/x"}}}), "claude-code/"); ok {
		t.Fatal("own agent accepted")
	}
	for _, raw := range []string{"", "null", "[]", `{"SourceFormat":"claude"`, `{"SourceFormat":1}`} {
		if _, ok := ScanClaudeListIntercept([]byte(raw), ""); ok {
			t.Errorf("%q accepted", raw)
		}
	}
	missing := strings.Replace(string(listRequest(t, nil)), `"StatusCode":200,`, ``, 1)
	if _, ok := ScanClaudeListIntercept([]byte(missing), ""); ok {
		t.Fatal("missing status accepted")
	}
}

// An inference response is rejected from its leading fields: its body is
// never read, let alone decoded.
func TestScanDoesNotReadInferenceBodies(t *testing.T) {
	big := bytes.Repeat([]byte("A"), 32<<20)
	raw := listRequest(t, map[string]any{"Model": "claude-opus-5-5", "Body": nil})
	raw = bytes.Replace(raw, []byte(`"Body":null`), append(append([]byte(`"Body":"`), big...), '"'), 1)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if _, ok := ScanClaudeListIntercept(raw, ""); ok {
		t.Fatal("inference accepted")
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Fatalf("allocated %d bytes for a rejected inference response", allocated)
	}
}

func TestScanBoundsListBodies(t *testing.T) {
	big := bytes.Repeat([]byte(" "), MaxListBodyBytes+1)
	if _, ok := ScanClaudeListIntercept(listRequest(t, map[string]any{"Body": big}), ""); ok {
		t.Fatal("oversized list accepted")
	}
}
