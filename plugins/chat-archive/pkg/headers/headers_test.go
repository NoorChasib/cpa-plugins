package headers_test

// These tests need a working pkg/mask (they call mask.New and Masker.String). All values are
// synthetic, generated at test time; failures name headers, never values.

import (
	"crypto/sha256"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/fingerprint"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/headers"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

const alnum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func rnd(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alnum[rand.IntN(len(alnum))]
	}
	b[0] = '7' // gates that need a digit
	return string(b)
}

func newMasker(t *testing.T) *mask.Masker {
	t.Helper()
	m, err := mask.New(mask.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

var hmacKey = []byte("synthetic-test-hmac-key")

func byName(hs []wire.Header) map[string]wire.Header {
	out := map[string]wire.Header{}
	for _, h := range hs {
		out[h.Name] = h
	}
	return out
}

// noSecret fails if any secret appears anywhere in v's printed form.
func noSecret(t *testing.T, what string, v any, secrets ...string) {
	t.Helper()
	s := fmt.Sprintf("%+v", v)
	for i, sec := range secrets {
		if strings.Contains(s, sec) {
			t.Errorf("%s: secret %d present", what, i)
		}
	}
}

// TestRequestCanaries: credential headers (bearer, API keys, proxy auth, cookies, custom tokens) are
// dropped, a kept header's secret is masked, and X-Archive-* hints are masked, counted when unknown and
// listed for clearing, never kept.
func TestRequestCanaries(t *testing.T) {
	m := newMasker(t)
	c := map[string]string{
		"h1": rnd(40), "h2": rnd(40), "h3": "AIza" + rnd(35), "h4": rnd(40), "h5": rnd(32),
		"h6": rnd(40), "h7": "sk-ant-oat01-" + rnd(60), "h8": "ghp_" + rnd(36), "foo": rnd(30),
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+c["h1"])          // dropped
	h.Set("X-Api-Key", c["h2"])                        // dropped
	h.Set("X-Goog-Api-Key", c["h3"])                   // dropped
	h.Set("Proxy-Authorization", "Basic "+c["h4"])     // dropped
	h.Set("Cookie", "session="+c["h5"])                // dropped
	h.Set("X-Custom-Token", c["h6"])                   // dropped
	h.Set("X-Claude-Code-Prompt-Id", c["h7"])          // kept, masked
	h.Set("X-Archive-Machine", "laptop-"+c["h8"])      // hint, masked
	h.Set("X-Archive-Foo", c["foo"])                   // unknown hint
	h.Set("Anthropic-Version", "2023-06-01")           // kept
	h.Set("User-Agent", "claude-cli/2.1.0 (external)") // kept
	kept, hints, clear := headers.FilterRequest(h, m, "k1", hmacKey)

	got := byName(kept)
	for _, name := range []string{"authorization", "x-api-key", "x-goog-api-key", "proxy-authorization", "cookie", "x-custom-token"} {
		if _, ok := got[name]; ok {
			t.Errorf("%s must be dropped", name)
		}
	}
	for _, hd := range kept {
		if strings.HasPrefix(hd.Name, "x-archive-") {
			t.Errorf("%s must not be kept", hd.Name)
		}
	}
	if v := got["x-claude-code-prompt-id"].Values; len(v) != 1 || !strings.HasPrefix(v[0], "[masked:ca-anthropic:") {
		t.Errorf("x-claude-code-prompt-id must be kept masked")
	}
	if got["anthropic-version"].Values[0] != "2023-06-01" || got["user-agent"].Values[0] != "claude-cli/2.1.0 (external)" {
		t.Error("ordinary kept headers changed")
	}
	if !strings.HasPrefix(hints.Machine, "laptop-[masked:ca-github:") || hints.Unknown != 1 {
		t.Errorf("hints: machine masked and one unknown, got unknown=%d", hints.Unknown)
	}
	if strings.Join(clear, ",") != "X-Archive-Foo,X-Archive-Machine" {
		t.Errorf("clear = %v", clear)
	}
	var all []string
	for _, v := range c {
		all = append(all, v)
	}
	noSecret(t, "kept", kept, all...)
	noSecret(t, "hints", hints, all...)
}

func TestRequestDroppedNames(t *testing.T) {
	m := newMasker(t)
	h := http.Header{}
	for _, n := range []string{"Host", "Forwarded", "X-Forwarded-For", "X-Forwarded-Proto", "X-Real-Ip", "Via", "Cf-Connecting-Ip", "True-Client-Ip", "Anthropic-Workspace-Id", "Api-Key", "X-Unknown"} {
		h.Set(n, "v")
	}
	// Deny overrides prefix allow; exact names win over the deny pattern.
	h.Set("X-Stainless-Api-Key", "v")
	h.Set("X-Claude-Code-Auth-Token", "v")
	h.Set("X-Claude-Code-Signature", "v")
	h.Set("X-Stainless-Lang", "js")
	h.Set("X-Claude-Code-Session-Id", "sess-1")
	h.Set("Idempotency-Key", "idem-1")
	kept, _, clear := headers.FilterRequest(h, m, "k1", hmacKey)
	var names []string
	for _, k := range kept {
		names = append(names, k.Name)
	}
	if got := strings.Join(names, ","); got != "idempotency-key,x-claude-code-session-id,x-stainless-lang" {
		t.Errorf("kept = %s", got)
	}
	if clear != nil {
		t.Errorf("clear = %v", clear)
	}
}

func TestRequestExactNames(t *testing.T) {
	m := newMasker(t)
	exact := []string{"anthropic-version", "anthropic-beta", "content-type", "accept", "user-agent", "x-app",
		"idempotency-key", "originator", "version", "openai-beta", "session_id", "session-id", "thread_id",
		"thread-id", "conversation_id", "x-session-id", "x-conversation-id", "x-client-request-id",
		"x-request-id", "x-thread-id", "x-parent-session-id", "x-session-affinity", "x-slot-session-id",
		"x-task-id", "x-http-session-id", "x-codex-turn-metadata", "x-codex-beta-features",
		"x-codex-window-id", "x-codex-parent-thread-id", "x-openai-subagent",
		"x-openai-internal-codex-responses-lite"}
	h := http.Header{}
	for _, n := range exact {
		h[n] = []string{"v-" + n} // non-canonical keys are lowered too
	}
	h["X-Codex-Other"] = []string{"v"} // x-codex- is not a request prefix
	kept, _, _ := headers.FilterRequest(h, m, "k1", hmacKey)
	if len(kept) != len(exact) {
		t.Fatalf("kept %d of %d exact names", len(kept), len(exact))
	}
	for i := 1; i < len(kept); i++ {
		if kept[i-1].Name >= kept[i].Name {
			t.Fatal("kept is not sorted by name")
		}
	}
	for _, k := range kept {
		if k.Values[0] != "v-"+k.Name {
			t.Errorf("%s value changed", k.Name)
		}
	}
}

func TestHashOnlyAndHMAC(t *testing.T) {
	m := newMasker(t)
	state, hint, acct := rnd(80), rnd(30), "acct-"+rnd(20)
	h := http.Header{}
	h.Set("X-Codex-Turn-State", state)
	h.Set("X-Codex-Routing-Hint", hint)
	h.Set("Chatgpt-Account-Id", acct)
	kept, _, _ := headers.FilterRequest(h, m, "k1", hmacKey)
	got := byName(kept)
	for name, v := range map[string]string{"x-codex-turn-state": state, "x-codex-routing-hint": hint} {
		hd := got[name]
		want := sha256.Sum256([]byte(v))
		if hd.SHA256 == nil || [32]byte(*hd.SHA256) != want || hd.Len != len(v) || hd.Values != nil || hd.FP != "" {
			t.Errorf("%s must be sha256 and len only", name)
		}
	}
	hd := got["chatgpt-account-id"]
	if hd.FP != fingerprint.HMAC("k1", hmacKey, acct) || !strings.HasPrefix(hd.FP, "k1:") || hd.Values != nil || hd.SHA256 != nil {
		t.Error("chatgpt-account-id must be the k1 HMAC only")
	}
	noSecret(t, "kept", kept, state, hint, acct)

	resp := http.Header{}
	resp.Set("X-Codex-Turn-State", state)
	rk := byName(headers.FilterResponse(resp, m))["x-codex-turn-state"]
	if rk.SHA256 == nil || rk.Len != len(state) || rk.Values != nil {
		t.Error("response x-codex-turn-state must be hash only")
	}
}

func TestLimits(t *testing.T) {
	m := newMasker(t)
	h := http.Header{}
	for i := 0; i < 70; i++ {
		h.Set(fmt.Sprintf("X-Stainless-H%02d", i), "v")
	}
	kept, _, _ := headers.FilterRequest(h, m, "k1", hmacKey)
	if len(kept) != 64 || kept[63].Name != "x-stainless-h63" {
		t.Errorf("kept %d headers, want the first 64 by name", len(kept))
	}

	h = http.Header{}
	for i := 0; i < 40; i++ {
		h.Add("Accept", fmt.Sprintf("v%d", i))
	}
	h.Set("User-Agent", strings.Repeat("é", 700)) // 1,400 bytes
	got := byName(func() []wire.Header { k, _, _ := headers.FilterRequest(h, m, "k1", hmacKey); return k }())
	if n := len(got["accept"].Values); n != 32 {
		t.Errorf("accept kept %d values, want 32", n)
	}
	ua := got["user-agent"].Values[0]
	if len(ua) > 1024 || len(ua) < 1022 || !utf8.ValidString(ua) {
		t.Errorf("user-agent is %d bytes (valid UTF-8 %v), want at most 1 KiB", len(ua), utf8.ValidString(ua))
	}
}

func TestHintSanitizing(t *testing.T) {
	m := newMasker(t)
	h := http.Header{}
	h.Set("X-Archive-Origin", "ci\x00\x1f-é-"+strings.Repeat("a", 300))
	h.Set("X-Archive-Ci-Run", "123")
	h.Set("X-Archive-Ci-Job", "build")
	h.Set("X-Archive-Ci-Workflow", "release.yml")
	h.Set("X-Archive-Runner", "gh-runner-1")
	h.Set("X-Archive-Canary", "canary-0001")
	h.Set("x-archive-machine", "mbp") // non-canonical key
	_, hints, clear := headers.FilterRequest(h, m, "k1", hmacKey)
	if len(hints.Origin) != 256 || !strings.HasPrefix(hints.Origin, "ci--a") {
		t.Errorf("origin must be printable ASCII, at most 256 bytes; got %d bytes", len(hints.Origin))
	}
	want := wire.ArchiveHints{Machine: "mbp", Origin: hints.Origin, CIRun: "123", CIJob: "build", CIWorkflow: "release.yml", Runner: "gh-runner-1", Canary: "canary-0001"}
	if hints != want {
		t.Errorf("hints = %+v", hints)
	}
	if len(clear) != 7 {
		t.Errorf("clear has %d names, want 7", len(clear))
	}

	// A token split by a TAB, DEL or obs-text byte is masked after the byte is dropped.
	tok := "ghp_" + rnd(36)
	for _, sep := range []string{"\t", "\x7f", "\x80"} {
		h := http.Header{}
		h.Set("X-Archive-Machine", tok[:22]+sep+tok[22:])
		h.Set("X-Archive-Origin", "o "+tok[:16]+sep+tok[16:])
		_, hints, _ := headers.FilterRequest(h, m, "k1", hmacKey)
		noSecret(t, "glued hint", hints, tok)
		for _, v := range []string{hints.Machine, hints.Origin} {
			if !strings.Contains(v, "[masked:ca-github:") || len(m.Detect(v)) != 0 {
				t.Errorf("glued hint (sep %q) must be masked whole", sep)
			}
		}
	}
}

// TestResponse: the response allowlist keeps rate-limit and request-id headers, drops cookies,
// echoed keys and denied names under a kept prefix, and masks the values it keeps.
func TestResponse(t *testing.T) {
	m := newMasker(t)
	echoed, cookie := rnd(40), rnd(32)
	h := http.Header{}
	h.Set("Set-Cookie", "sid="+cookie)
	h.Set("X-Api-Key", echoed)
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.42")
	h.Set("Anthropic-Ratelimit-Unified-Reset", "1767225600")
	h.Set("Anthropic-Ratelimit-Tokens-Remaining", "1000") // deny pattern (token) applies under the prefix
	h.Set("X-Ratelimit-Remaining-Requests", "99")
	h.Set("X-Codex-Primary-Used-Percent", "12")
	h.Set("X-Codex-Auth-Thing", "v") // deny under prefix
	for _, n := range []string{"Content-Type", "Request-Id", "X-Request-Id", "Retry-After", "X-Should-Retry", "Openai-Processing-Ms", "Openai-Version"} {
		h.Set(n, "v")
	}
	h.Set("Server", "x")
	h.Set("Cf-Ray", "x")
	kept := headers.FilterResponse(h, m)
	var names []string
	for _, k := range kept {
		names = append(names, k.Name)
	}
	want := "anthropic-ratelimit-unified-5h-utilization,anthropic-ratelimit-unified-reset,content-type,openai-processing-ms,openai-version,request-id,retry-after,x-codex-primary-used-percent,x-ratelimit-remaining-requests,x-request-id,x-should-retry"
	if got := strings.Join(names, ","); got != want {
		t.Errorf("kept = %s", got)
	}
	noSecret(t, "response", kept, echoed, cookie)

	// Values are masked.
	h = http.Header{}
	tok := "ghp_" + rnd(36)
	h.Set("X-Request-Id", "req "+tok)
	if v := headers.FilterResponse(h, m)[0].Values[0]; !strings.HasPrefix(v, "req [masked:ca-github:") {
		t.Error("response values must be masked")
	}
}

// TestStripURL: userinfo, query strings (signed media URLs, API keys) and fragments are removed, a
// non-URL is "", and the result is ASCII.
func TestStripURL(t *testing.T) {
	m := newMasker(t)
	pw, key, sig, tok := rnd(20), "AIza"+rnd(35), rnd(40), "ghp_"+rnd(36)
	for _, tc := range []struct{ in, want string }{
		{"https://user:" + pw + "@api.example.invalid:8443/v1/models?key=" + key + "#frag", "https://api.example.invalid:8443/v1/models"},
		{"https://generativelanguage.example.invalid/v1beta/models/x:generateContent?key=" + key, "https://generativelanguage.example.invalid/v1beta/models/x:generateContent"},
		{"https://bucket.example.invalid/img/a.png?X-Amz-Signature=" + sig + "&X-Amz-Credential=c", "https://bucket.example.invalid/img/a.png"},
		{"HTTP://Host.Example.Invalid", "http://Host.Example.Invalid"},
		{"https://h.example.invalid/a%20b/c", "https://h.example.invalid/a%20b/c"},
		{"/relative/path?x=1", ""},
		{"not a url", ""},
		{"mailto:someone@example.invalid", ""},
		{"", ""},
		{"https://[::1", ""},
		// Host escapes stay escaped, so the result is ASCII and valid UTF-8.
		{"http://u@h%80x.example.invalid/p%80q?s=1", "http://h%80x.example.invalid/p%80q"},
		{"https://bücher.example.invalid/p", "https://b%C3%BCcher.example.invalid/p"},
		{"http://[fe80::1%25en0]:8080/p", "http://[fe80::1%25en0]:8080/p"},
	} {
		got := headers.StripURL(tc.in, m)
		if got != tc.want {
			t.Errorf("StripURL case %q mismatch: got %q", tc.want, got)
		}
		for i := 0; i < len(got); i++ {
			if got[i] >= 0x80 {
				t.Errorf("StripURL case %q: non-ASCII byte in output", tc.want)
				break
			}
		}
	}
	// A percent-escape inside a token, and a token in the host name, are masked too.
	for _, in := range []string{
		"https://h.example.invalid/files/ghp%5F" + tok[4:],
		"https://h.example.invalid/files/%67hp_" + tok[4:],
		"https://h.example.invalid/files/" + tok[:10] + "%" + fmt.Sprintf("%02X", tok[10]) + tok[11:],
	} {
		got := headers.StripURL(in+"?q=1", m)
		if !strings.HasPrefix(got, "https://h.example.invalid/files/[masked:ca-github:") || !strings.HasSuffix(got, "]") {
			t.Errorf("escaped path token must be masked, got %d bytes", len(got))
		}
		noSecret(t, "StripURL escaped", got, tok, tok[4:])
	}
	if got := headers.StripURL("https://"+tok+".example.invalid:8443/x", m); !strings.HasPrefix(got, "https://[masked:ca-github:") || !strings.HasSuffix(got, "].example.invalid:8443/x") {
		t.Errorf("host token must be masked, got %d bytes", len(got))
	} else {
		noSecret(t, "StripURL host", got, tok)
	}
	got := headers.StripURL("https://h.example.invalid/repos/"+tok+"/x?y=1", m)
	if !strings.HasPrefix(got, "https://h.example.invalid/repos/[masked:ca-github:") || !strings.HasSuffix(got, "]/x") {
		t.Errorf("path must be masked, got %d bytes", len(got))
	}
	noSecret(t, "StripURL", got, tok)
}

func TestArchiveHeaderNames(t *testing.T) {
	h := http.Header{"X-Archive-Machine": {"a"}, "x-archive-runner": {"b"}, "X-ARCHIVE-CANARY": {"c"}, "X-Archive": {"d"}, "X-Archivex-Foo": {"e"}, "Accept": {"f"}}
	if got := strings.Join(headers.ArchiveHeaderNames(h), ","); got != "X-ARCHIVE-CANARY,X-Archive-Machine,x-archive-runner" {
		t.Errorf("ArchiveHeaderNames = %s", got)
	}
	if headers.ArchiveHeaderNames(http.Header{"Accept": {"x"}}) != nil {
		t.Error("no archive headers must give nil")
	}
}
