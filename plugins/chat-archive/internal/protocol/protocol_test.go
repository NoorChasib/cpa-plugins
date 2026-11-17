package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFieldsSkipsLargeValues(t *testing.T) {
	big := strings.Repeat("QUFB", 1<<18)
	raw := []byte(`{"A":"x\"y","B":{"n":[1,{"s":"}"}]},"Body":"` + big + `","C":null,"D":true}`)
	var keys []string
	err := Fields(raw, func(k, v []byte) error {
		keys = append(keys, string(k))
		if string(k) == "Body" && len(v) != len(big)+2 {
			t.Fatalf("Body span = %d", len(v))
		}
		return nil
	})
	if err != nil || strings.Join(keys, ",") != "A,B,Body,C,D" {
		t.Fatalf("keys = %v err %v", keys, err)
	}
	if err := Fields([]byte(`{"A":"unterminated}`), func(k, v []byte) error { return nil }); err == nil {
		t.Fatal("malformed body accepted")
	}
}

func TestDecodeBase64AndUnquote(t *testing.T) {
	payload := []byte("synthetic body \x00\xff")
	val, _ := json.Marshal(payload)
	got, err := DecodeBase64(val)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("got %q err %v", got, err)
	}
	if b, _ := DecodeBase64([]byte("null")); b != nil {
		t.Fatal("null decoded")
	}
	s, _ := json.Marshal("a\"b\\c\né\U0001F600<>&")
	out, err := UnquoteInto(nil, s)
	if err != nil || string(out) != "a\"b\\c\né\U0001F600<>&" {
		t.Fatalf("unquote = %q err %v", out, err)
	}
}

func TestScanResponseSkipsRequestHeaders(t *testing.T) {
	body := base64.StdEncoding.EncodeToString([]byte(`{"id":"x"}`))
	raw := []byte(`{"RequestID":"r1","RequestHeaders":{"X-Api-Key":["upstream-secret-canary"]},"ResponseHeaders":{"Content-Type":["application/json"]},` +
		`"OriginalRequest":"` + body + `","RequestBody":null,"Body":"` + body + `","StatusCode":200,"Metadata":{"request_path":"/v1/messages","generate":true,"other":{"deep":[1]}}}`)
	r, sp, err := ScanResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := json.Marshal(r)
	if strings.Contains(string(enc), "upstream-secret-canary") {
		t.Fatal("RequestHeaders materialized")
	}
	if r.RequestID != "r1" || r.StatusCode != 200 || r.OriginalAbsent || !r.Meta.Present || r.Meta.RequestPath != "/v1/messages" || r.Meta.Generate == nil || !*r.Meta.Generate {
		t.Fatalf("decoded = %+v", r)
	}
	b, _ := DecodeBase64(sp.Body)
	if string(b) != `{"id":"x"}` {
		t.Fatalf("body = %s", b)
	}
}

// Execution is set by a top-level requested_model key of any value, never by a nested one.
func TestDecodeMetaExecution(t *testing.T) {
	for _, c := range []struct {
		raw              string
		present, execute bool
	}{
		{`null`, false, false},
		{`{"caller_scope":"c","access_provider":"config-inline","frontend_auth_metadata":{"requested_model":"x"}}`, true, false},
		{`{"requested_model":""}`, true, true},
		{`{"requested_model":"model-x","request_path":"/v1/messages"}`, true, true},
		{`{"requested_model":null}`, true, true},
	} {
		m, err := DecodeMeta([]byte(c.raw))
		if err != nil || m.Present != c.present || m.Execution != c.execute {
			t.Fatalf("%s: present %v execution %v err %v", c.raw, m.Present, m.Execution, err)
		}
	}
}

func TestDecodeUsageZeroesScratch(t *testing.T) {
	key := "sk-cpa-synthetic-canary-0123456789"
	src := "user@example.invalid"
	raw := []byte(`{"RequestID":"u1","Provider":"claude","APIKey":"` + key + `","Source":"` + src + `","AuthID":"auth/file.json","AuthIndex":"abcd",` +
		`"Latency":1500000000,"TTFT":250000000,"Failed":true,"Failure":{"StatusCode":500,"Body":"oops"},"Detail":{"InputTokens":5},"ResponseHeaders":{"X-Request-Id":["q"]}}`)
	var seen UsageSecrets
	var keep []byte
	r, err := DecodeUsage(raw, func(u *Usage, s UsageSecrets) {
		seen = s
		keep = s.APIKey
		if string(s.APIKey) != key || string(s.Source) != src || string(s.AuthID) != "auth/file.json" || u.Provider != "claude" || u.AuthIndex != "abcd" {
			t.Fatalf("secrets not passed: %+v", u)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(keep, []byte("sk-cpa")) || len(seen.Source) > 0 && bytes.Contains(seen.Source, []byte("example")) {
		t.Fatal("scratch buffer not zeroed")
	}
	enc, _ := json.Marshal(r)
	for _, s := range []string{key, src, "auth/file.json"} {
		if strings.Contains(string(enc), s) {
			t.Fatalf("decoded usage holds a secret field")
		}
	}
	if r.Latency.Seconds() != 1.5 || r.FailureStatus != 500 || r.FailureBody != "oops" || r.Tokens.InputTokens != 5 {
		t.Fatalf("usage = %+v", r)
	}
	// A long secret takes the heap path and is zeroed as well.
	long := strings.Repeat("k", 2000)
	_, err = DecodeUsage([]byte(`{"APIKey":"`+long+`"}`), func(_ *Usage, s UsageSecrets) { keep = s.APIKey })
	if err != nil || bytes.Contains(keep, []byte("kkkk")) {
		t.Fatalf("long scratch not zeroed (err %v)", err)
	}
}

// moegoWire reads a request CPA v8.0.76 encoded with the moego JSON library (testdata/
// cpa-v8.0.76-moego, captured from callPlugin by capture_test.go.txt there).
func moegoWire(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "cpa-v8.0.76-moego", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// moegoBody is the []byte value the capture gave every body field: HTML characters, U+2028,
// invalid UTF-8 and the bytes 0x00..0xff, so its base64 is padded.
func moegoBody() []byte {
	body := []byte("{\"model\":\"ca-claude\",\"text\":\"<b>&amp;</b>   café \xff\xfe\"}")
	for i := 0; i < 256; i++ {
		body = append(body, byte(i))
	}
	return body
}

// CPA v8.0.74 and later encode plugin RPC requests with the moego JSON library instead of
// encoding/json. Every call the plugin decodes, as v8.0.76 really encodes it, decodes to the
// values CPA sent: []byte as padded standard base64, time.Time as RFC 3339 with nanoseconds and
// offset, durations as integer nanoseconds, HTML characters escaped, invalid UTF-8 replaced.
func TestDecodeCPAMoegoWire(t *testing.T) {
	started := time.Date(2026, 11, 17, 16, 4, 5, 123456789, time.FixedZone("", 5*3600+30*60))
	completed := time.Date(2026, 11, 17, 16, 10, 2, 385758544, time.UTC)
	execMeta := func(name string, m Meta) {
		t.Helper()
		if !m.Present || !m.Execution || m.RequestPath != "/v1/messages" || m.CallerScope != "c0ffee" ||
			m.ExecutionSessionID != "sess-1" || m.Generate == nil || !*m.Generate {
			t.Fatalf("%s: meta = %+v", name, m)
		}
	}
	body := func(name string, span, want []byte) {
		t.Helper()
		got, err := DecodeBase64(span)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s: body %q err %v", name, got, err)
		}
	}

	ba, sp, err := ScanBeforeAuth(moegoWire(t, "before-auth"))
	if err != nil || ba.RequestID != "rid-exec" || ba.TraceID != "trace-1" || ba.SourceFormat != "claude" || ba.Model != "ca-claude" ||
		ba.RequestedModel != "ca-claude" || !ba.Stream || ba.CallerScope != "c0ffee" || ba.BodyLen < len(moegoBody()) ||
		strings.Join(ba.Headers.Values("X-Multi"), "|") != "a<b|c&d" {
		t.Fatalf("before-auth = %+v err %v", ba, err)
	}
	execMeta("before-auth", ba.Meta)
	body("before-auth", sp.Body, moegoBody())

	aa, err := DecodeAfterAuth(moegoWire(t, "after-auth"))
	if err != nil || aa.RequestID != "rid-exec" || aa.ToFormat != "claude" || aa.Model != "ca-claude-upstream" {
		t.Fatalf("after-auth = %+v err %v", aa, err)
	}
	execMeta("after-auth", aa.Meta)

	r, sp, err := ScanResponse(moegoWire(t, "response"))
	if err != nil || r.RequestID != "rid-exec" || r.Model != "ca-claude" || r.RequestedModel != "ca-claude" || r.StatusCode != 200 ||
		r.OriginalAbsent || r.ResponseHeaders.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("response = %+v err %v", r, err)
	}
	execMeta("response", r.Meta)
	body("response", sp.Body, moegoBody())

	ml, sp, err := ScanResponse(moegoWire(t, "model-list-response"))
	if err != nil || ml.RequestID != "rid-list" || ml.Model != "" || ml.RequestedModel != "" || !ml.OriginalAbsent ||
		!ml.Meta.Present || ml.Meta.Execution || ml.Meta.CallerScope != "c0ffee" || ml.StatusCode != 200 {
		t.Fatalf("model-list response = %+v err %v", ml, err)
	}
	body("model-list response", sp.Body, []byte(`{"object":"list","data":[{"id":"ca-claude"}]}`))

	mc, err := DecodeCompletion(moegoWire(t, "model-list-complete"))
	if err != nil || mc.RequestID != "rid-list" || mc.Model != "" || mc.RequestedModel != "" || mc.Outcome != "succeeded" ||
		!mc.Meta.Present || mc.Meta.Execution || !mc.StartedAt.Equal(started) || !mc.CompletedAt.Equal(completed) {
		t.Fatalf("model-list completion = %+v err %v", mc, err)
	}

	hi, sp, err := ScanStreamChunk(moegoWire(t, "chunk-header-init"))
	if err != nil || hi.ChunkIndex != StreamHeaderInit || hi.ResponseHeaders.Get("Content-Type") != "text/event-stream" || !IsNull(sp.Body) {
		t.Fatalf("header-init = %+v body %q err %v", hi, sp.Body, err)
	}
	execMeta("header-init", hi.Meta)
	ch, sp, err := ScanStreamChunk(moegoWire(t, "chunk-payload"))
	if err != nil || ch.RequestID != "rid-exec" || ch.ChunkIndex != 0 || ch.ResponseHeaders != nil {
		t.Fatalf("chunk = %+v err %v", ch, err)
	}
	body("chunk", sp.Body, []byte("event: ping\ndata: {\"x\":\"<&>\"}\n\n"))

	c, err := DecodeCompletion(moegoWire(t, "complete"))
	if err != nil || c.RequestID != "rid-exec" || c.TraceID != "trace-1" || !c.Stream || c.Outcome != "failed" || c.StatusCode != 502 ||
		c.Error != "upstream <b>&</b> �   failed" || !c.StartedAt.Equal(started) || !c.CompletedAt.Equal(completed) {
		t.Fatalf("completion = %+v err %v", c, err)
	}
	execMeta("completion", c.Meta)
	z, err := DecodeCompletion(moegoWire(t, "complete-zero-times"))
	if err != nil || z.Outcome != "canceled" || !z.StartedAt.IsZero() || !z.CompletedAt.IsZero() || z.Meta.Present {
		t.Fatalf("zero-time completion = %+v err %v", z, err)
	}

	var apiKey, source, authID string
	u, err := DecodeUsage(moegoWire(t, "usage"), func(_ *Usage, s UsageSecrets) {
		apiKey, source, authID = string(s.APIKey), string(s.Source), string(s.AuthID)
	})
	if err != nil || u.RequestID != "rid-exec" || u.BaseURL != "http://127.0.0.1:9/v1?a=1&b=<2>" || !u.Generate || !u.Stream ||
		!u.RequestedAt.Equal(time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)) || u.Latency != 1500*time.Millisecond ||
		u.TTFT != 250*time.Millisecond || !u.Failed || u.FailureStatus != 502 || u.FailureBody != "bad � <gateway>" {
		t.Fatalf("usage = %+v err %v", u, err)
	}
	if apiKey != "sk-synthetic-<&>" || source != "ops@example.invalid" || authID != "auth-a" {
		t.Fatalf("usage secrets = %q %q %q", apiKey, source, authID)
	}
}
