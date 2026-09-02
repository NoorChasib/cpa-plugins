package fingerprint

import (
	"encoding/json"
	"strings"
	"testing"
)

func defaultRules() Rules {
	return Rules{
		ClaudeEntrypoints:     []string{"cli", "sdk-cli", "claude-vscode", "sdk-ts", "sdk-py"},
		RequireClaudeCodeBeta: true,
	}
}

// claudeHeaders reproduces the measured Claude Code 2.1.258 request shape
// captured through an Agent SDK (sdk-ts) host.
func claudeHeaders() map[string][]string {
	return map[string][]string{
		"User-Agent":                  {"claude-cli/2.1.258 (external, sdk-ts, agent-sdk/0.3.170)"},
		"X-App":                       {"cli"},
		"Anthropic-Version":           {"2023-06-01"},
		"Anthropic-Beta":              {"claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14"},
		"X-Stainless-Lang":            {"js"},
		"X-Stainless-Runtime":         {"node"},
		"X-Stainless-Package-Version": {"0.112.1"},
		"X-Stainless-Runtime-Version": {"v26.3.0"},
		"X-Stainless-Os":              {"Linux"},
		"X-Stainless-Arch":            {"x64"},
		"X-Stainless-Timeout":         {"600"},
		"X-Claude-Code-Session-Id":    {"3f6c1a1e-7b6d-4c1e-9a1f-2b3c4d5e6f70"},
		"Authorization":               {"Bearer sk-ant-secret"},
	}
}

func codexHeaders(ua string) map[string][]string {
	return map[string][]string{
		"User-Agent": {ua},
		"Originator": {"codex_cli_rs"},
		"Session_id": {"019a1b2c-3d4e-7f80-9a1b-2c3d4e5f6a7b"},
	}
}

func TestClassifyClaudeMeasuredShape(t *testing.T) {
	res := Classify(claudeHeaders(), defaultRules())
	if !res.OK {
		t.Fatalf("rejected: %s", res.Reason)
	}
	c := res.Candidate
	if c.Provider != ProviderClaude {
		t.Errorf("provider = %s", c.Provider)
	}
	if c.Version.String() != "2.1.258" {
		t.Errorf("version = %s", c.Version)
	}
	if c.UserAgent != "claude-cli/2.1.258 (external, cli)" {
		t.Errorf("canonical UA = %q; the sdk-ts/agent-sdk suffix must never reach the baseline", c.UserAgent)
	}
	if c.PackageVersion != "0.112.1" || c.RuntimeVersion != "v26.3.0" {
		t.Errorf("package/runtime = %s/%s", c.PackageVersion, c.RuntimeVersion)
	}
	if c.OS != "Linux" || c.Arch != "x64" {
		t.Errorf("os/arch = %s/%s", c.OS, c.Arch)
	}
	if c.Entrypoint != "sdk-ts" || c.AgentSDK != "0.3.170" {
		t.Errorf("entrypoint/agent-sdk = %s/%s", c.Entrypoint, c.AgentSDK)
	}
	if res.SessionID != "3f6c1a1e-7b6d-4c1e-9a1f-2b3c4d5e6f70" {
		t.Errorf("session = %q", res.SessionID)
	}
	if c.Key() != "claude|2.1.258|0.112.1|v26.3.0" {
		t.Errorf("key = %q", c.Key())
	}
}

func TestClassifyClaudeCLIEntrypointWithoutAgentSDK(t *testing.T) {
	h := claudeHeaders()
	h["User-Agent"] = []string{"claude-cli/2.1.260 (external, cli)"}
	delete(h, "X-Claude-Code-Session-Id")
	res := Classify(h, defaultRules())
	if !res.OK {
		t.Fatalf("rejected: %s", res.Reason)
	}
	if res.Candidate.Entrypoint != "cli" || res.Candidate.AgentSDK != "" {
		t.Errorf("entrypoint/agent-sdk = %s/%q", res.Candidate.Entrypoint, res.Candidate.AgentSDK)
	}
	if res.SessionID != "" {
		t.Errorf("session = %q, want anonymous (empty)", res.SessionID)
	}
}

func TestClassifyClaudeCaseInsensitiveUserAgent(t *testing.T) {
	h := claudeHeaders()
	h["User-Agent"] = []string{"Claude-CLI/2.1.258 (External, SDK-TS, agent-sdk/0.3.170)"}
	res := Classify(h, defaultRules())
	if !res.OK {
		t.Fatalf("rejected: %s", res.Reason)
	}
	if res.Candidate.Entrypoint != "sdk-ts" {
		t.Errorf("entrypoint = %q", res.Candidate.Entrypoint)
	}
}

func TestClassifyClaudeLowercaseHeaderKeysFallback(t *testing.T) {
	lower := map[string][]string{}
	for k, v := range claudeHeaders() {
		lower[strings.ToLower(k)] = v
	}
	res := Classify(lower, defaultRules())
	if !res.OK {
		t.Fatalf("rejected with lowercase keys: %s", res.Reason)
	}
}

func TestClassifyClaudeRejections(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string][]string)
		reason string
	}{
		{"entrypoint denied", func(h map[string][]string) {
			h["User-Agent"] = []string{"claude-cli/2.1.258 (external, mcp)"}
		}, ReasonEntrypointDenied},
		{"x-app missing", func(h map[string][]string) { delete(h, "X-App") }, ReasonMissingXApp},
		{"x-app wrong", func(h map[string][]string) { h["X-App"] = []string{"web"} }, ReasonMissingXApp},
		{"anthropic-version wrong", func(h map[string][]string) { h["Anthropic-Version"] = []string{"2024-01-01"} }, ReasonAnthropicVersion},
		{"stainless lang", func(h map[string][]string) { h["X-Stainless-Lang"] = []string{"python"} }, ReasonStainlessLang},
		{"stainless runtime", func(h map[string][]string) { h["X-Stainless-Runtime"] = []string{"bun"} }, ReasonStainlessRuntime},
		{"package version malformed", func(h map[string][]string) { h["X-Stainless-Package-Version"] = []string{"0.112"} }, ReasonPackageVersion},
		{"package version missing", func(h map[string][]string) { delete(h, "X-Stainless-Package-Version") }, ReasonPackageVersion},
		{"runtime version missing v", func(h map[string][]string) { h["X-Stainless-Runtime-Version"] = []string{"26.3.0"} }, ReasonRuntimeVersion},
		{"os missing", func(h map[string][]string) { delete(h, "X-Stainless-Os") }, ReasonStainlessOSArch},
		{"arch empty", func(h map[string][]string) { h["X-Stainless-Arch"] = []string{"  "} }, ReasonStainlessOSArch},
		{"os unsafe", func(h map[string][]string) { h["X-Stainless-Os"] = []string{"Linux\nfoo: bar"} }, ReasonUnsafeFieldContents},
		{"beta missing", func(h map[string][]string) { delete(h, "Anthropic-Beta") }, ReasonBetaMissing},
		{"beta without claude code", func(h map[string][]string) { h["Anthropic-Beta"] = []string{"oauth-2025-04-20"} }, ReasonBetaMissing},
		{"ua trailing junk", func(h map[string][]string) {
			h["User-Agent"] = []string{"claude-cli/2.1.258 (external, cli) extra"}
		}, ReasonNotClient},
		{"ua missing external", func(h map[string][]string) {
			h["User-Agent"] = []string{"claude-cli/2.1.258 (internal, cli)"}
		}, ReasonNotClient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := claudeHeaders()
			tc.mutate(h)
			res := Classify(h, defaultRules())
			if res.OK {
				t.Fatalf("accepted, want rejection %s", tc.reason)
			}
			if res.Reason != tc.reason {
				t.Errorf("reason = %s, want %s", res.Reason, tc.reason)
			}
			if tc.reason != ReasonNotClient && res.Provider != ProviderClaude {
				t.Errorf("provider = %q, want claude for a claude-shaped rejection", res.Provider)
			}
		})
	}
}

func TestClassifyClaudeBetaOptionalWhenNotRequired(t *testing.T) {
	h := claudeHeaders()
	delete(h, "Anthropic-Beta")
	rules := defaultRules()
	rules.RequireClaudeCodeBeta = false
	if res := Classify(h, rules); !res.OK {
		t.Fatalf("rejected: %s", res.Reason)
	}
}

func TestClassifyClaudeBetaTokenSpacing(t *testing.T) {
	h := claudeHeaders()
	h["Anthropic-Beta"] = []string{"oauth-2025-04-20 , CLAUDE-CODE-20250219"}
	if res := Classify(h, defaultRules()); !res.OK {
		t.Fatalf("rejected: %s", res.Reason)
	}
	h["Anthropic-Beta"] = []string{"oauth-2025-04-20", "claude-code-20250219"}
	if res := Classify(h, defaultRules()); !res.OK {
		t.Fatalf("multi-value header rejected: %s", res.Reason)
	}
}

func TestClassifyCodexShapes(t *testing.T) {
	cases := []struct {
		ua      string
		version string
	}{
		{"codex_cli_rs/0.144.1 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9", "0.144.1"},
		{"codex-tui/0.145.0 (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 (codex-tui; 0.145.0)", "0.145.0"},
		{"codex-tui/0.152.1 (Ubuntu 24.4.0; x86_64) WezTerm/20240203-110809-5046fc22", "0.152.1"},
	}
	for _, tc := range cases {
		res := Classify(codexHeaders(tc.ua), defaultRules())
		if !res.OK {
			t.Fatalf("%q rejected: %s", tc.ua, res.Reason)
		}
		if res.Candidate.Provider != ProviderCodex || res.Candidate.Version.String() != tc.version {
			t.Errorf("%q -> %s %s", tc.ua, res.Candidate.Provider, res.Candidate.Version)
		}
		if res.Candidate.UserAgent != tc.ua {
			t.Errorf("Codex must keep the full UA, got %q", res.Candidate.UserAgent)
		}
		if res.Candidate.Key() != "codex|"+tc.ua {
			t.Errorf("key = %q", res.Candidate.Key())
		}
		if res.SessionID != "019a1b2c-3d4e-7f80-9a1b-2c3d4e5f6a7b" {
			t.Errorf("session = %q", res.SessionID)
		}
	}
}

func TestClassifyCodexSessionFallbacks(t *testing.T) {
	h := codexHeaders("codex-tui/0.150.0 (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 (codex-tui; 0.150.0)")
	delete(h, "Session_id")
	h["Thread-Id"] = []string{"thread-123"}
	if res := Classify(h, defaultRules()); res.SessionID != "thread-123" {
		t.Errorf("session = %q", res.SessionID)
	}
	delete(h, "Thread-Id")
	h["Session-Id"] = []string{"sess 1"} // spaces are not identifier-safe
	if res := Classify(h, defaultRules()); res.SessionID != "" {
		t.Errorf("session = %q, want anonymous", res.SessionID)
	}
	delete(h, "Session-Id")
	// X-Client-Request-Id is per request and must never act as a session.
	h["X-Client-Request-Id"] = []string{"req-1"}
	if res := Classify(h, defaultRules()); res.SessionID != "" {
		t.Errorf("X-Client-Request-Id used as session: %q", res.SessionID)
	}
}

func TestClassifyCodexRejections(t *testing.T) {
	h := codexHeaders("codex-tui/0.150.0 (Mac OS 26.5.0; arm64)")
	delete(h, "Originator")
	res := Classify(h, defaultRules())
	if res.OK || res.Reason != ReasonCodexOriginator || res.Provider != ProviderCodex {
		t.Errorf("result = %+v", res)
	}
	h = codexHeaders("codex-tui/0.150.0 (Mac OS\n26.5.0; arm64)")
	res = Classify(h, defaultRules())
	if res.OK || res.Reason != ReasonUnsafeFieldContents {
		t.Errorf("control characters accepted: %+v", res)
	}
	h = codexHeaders("codex-tui/0.150 (Mac OS 26.5.0; arm64)")
	if res = Classify(h, defaultRules()); res.OK || res.Reason != ReasonNotClient {
		t.Errorf("two-part version accepted: %+v", res)
	}
}

func TestClassifyIgnoresOtherClients(t *testing.T) {
	for _, ua := range []string{"", "curl/8.5.0", "Mozilla/5.0", "anthropic-sdk-python/0.40.0", "claude-cli", "codex"} {
		res := Classify(map[string][]string{"User-Agent": {ua}}, defaultRules())
		if res.OK || res.Reason != ReasonNotClient || res.Provider != "" {
			t.Errorf("%q -> %+v", ua, res)
		}
	}
	if res := Classify(nil, defaultRules()); res.OK || res.Reason != ReasonNotClient {
		t.Errorf("nil headers -> %+v", res)
	}
}

func TestClassifyRejectsHugeUserAgent(t *testing.T) {
	h := claudeHeaders()
	h["User-Agent"] = []string{"claude-cli/2.1.258 (external, " + strings.Repeat("x", 600) + ")"}
	if res := Classify(h, defaultRules()); res.OK || res.Reason != ReasonUserAgentTooLong {
		t.Errorf("result = %+v", res)
	}
}

func TestVersionParseCompareAndText(t *testing.T) {
	v, err := ParseVersion("2.1.258")
	if err != nil {
		t.Fatal(err)
	}
	if v.String() != "2.1.258" {
		t.Errorf("String = %q", v.String())
	}
	if w, _ := ParseVersion("v0.152.1"); w.String() != "0.152.1" {
		t.Errorf("leading v not tolerated: %s", w)
	}
	for _, bad := range []string{"", "2.1", "2.1.x", "2.1.258.1", "a.b.c", "2.1.2585555555"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	a := MustParseVersion("2.1.258")
	b := MustParseVersion("2.1.220")
	c := MustParseVersion("2.2.0")
	d := MustParseVersion("3.0.0")
	if !a.Newer(b) || b.Newer(a) || !c.Newer(a) || !d.Newer(c) || a.Newer(a) {
		t.Errorf("ordering broken")
	}
	if a.Compare(a) != 0 || a.Compare(b) != 1 || b.Compare(a) != -1 {
		t.Errorf("Compare broken")
	}
	if !(Version{}).IsZero() || a.IsZero() {
		t.Errorf("IsZero broken")
	}

	raw, err := json.Marshal(struct{ V Version }{a})
	if err != nil || string(raw) != `{"V":"2.1.258"}` {
		t.Errorf("marshal = %s, %v", raw, err)
	}
	var back struct{ V Version }
	if err := json.Unmarshal([]byte(`{"V":"1.2.3"}`), &back); err != nil || back.V.String() != "1.2.3" {
		t.Errorf("unmarshal = %+v, %v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"V":""}`), &back); err != nil || !back.V.IsZero() {
		t.Errorf("empty unmarshal = %+v, %v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"V":"nope"}`), &back); err == nil {
		t.Errorf("malformed version unmarshalled")
	}
}

func TestCompiledDefaultsParse(t *testing.T) {
	res := Classify(map[string][]string{
		"User-Agent":                  {CompiledClaudeUserAgent},
		"X-App":                       {"cli"},
		"Anthropic-Version":           {"2023-06-01"},
		"Anthropic-Beta":              {ClaudeCodeBeta},
		"X-Stainless-Lang":            {"js"},
		"X-Stainless-Runtime":         {"node"},
		"X-Stainless-Package-Version": {CompiledClaudePackageVersion},
		"X-Stainless-Runtime-Version": {CompiledClaudeRuntimeVersion},
		"X-Stainless-Os":              {CompiledClaudeOS},
		"X-Stainless-Arch":            {CompiledClaudeArch},
	}, defaultRules())
	if !res.OK || res.Candidate.Version != CompiledClaudeBaselineVersion {
		t.Errorf("compiled Claude defaults do not classify: %+v", res)
	}
	res = Classify(map[string][]string{"User-Agent": {CompiledCodexUserAgent}, "Originator": {"codex-tui"}}, defaultRules())
	if !res.OK || res.Candidate.Version != CompiledCodexBaselineVersion {
		t.Errorf("compiled Codex default does not classify: %+v", res)
	}
}

func TestParseBaselineUserAgentVersions(t *testing.T) {
	if v, ok := ParseClaudeUserAgentVersion(CompiledClaudeUserAgent); !ok || v.String() != "2.1.220" {
		t.Errorf("claude compiled = %s %v", v, ok)
	}
	if v, ok := ParseClaudeUserAgentVersion("claude-cli/2.1.258"); !ok || v.String() != "2.1.258" {
		t.Errorf("claude prefix-only = %s %v", v, ok)
	}
	if _, ok := ParseClaudeUserAgentVersion("my-proxy/1.0"); ok {
		t.Error("non-claude UA parsed")
	}
	if v, ok := ParseCodexUserAgentVersion(CompiledCodexUserAgent); !ok || v.String() != "0.146.0" {
		t.Errorf("codex compiled = %s %v", v, ok)
	}
	if v, ok := ParseCodexUserAgentVersion("codex_cli_rs/0.114.0"); !ok || v.String() != "0.114.0" {
		t.Errorf("codex placeholder = %s %v", v, ok)
	}
	if _, ok := ParseCodexUserAgentVersion("my-codex-client/1.0"); ok {
		t.Error("non-codex UA parsed")
	}
}

func TestValidateCandidate(t *testing.T) {
	v := MustParseVersion("2.1.258")
	rules := defaultRules()
	good := Candidate{Provider: ProviderClaude, Version: v, UserAgent: CanonicalClaudeUserAgent(v), PackageVersion: "0.112.1", RuntimeVersion: "v26.3.0", OS: "Linux", Arch: "x64", Entrypoint: "sdk-ts", AgentSDK: "0.3.170"}
	if err := ValidateCandidate(good, rules); err != nil {
		t.Fatalf("good claude candidate rejected: %v", err)
	}
	codexUA := "codex-tui/0.152.1 (Ubuntu 24.4.0; x86_64) WezTerm/1 (codex-tui; 0.152.1)"
	if err := ValidateCandidate(Candidate{Provider: ProviderCodex, Version: MustParseVersion("0.152.1"), UserAgent: codexUA}, rules); err != nil {
		t.Fatalf("good codex candidate rejected: %v", err)
	}
	with := func(mutate func(*Candidate)) Candidate { c := good; mutate(&c); return c }
	bad := map[string]Candidate{
		"zero version":        with(func(c *Candidate) { c.Version = Version{} }),
		"non-canonical ua":    with(func(c *Candidate) { c.UserAgent = "claude-cli/2.1.258 (external, sdk-ts)" }),
		"version mismatch":    with(func(c *Candidate) { c.UserAgent = CanonicalClaudeUserAgent(MustParseVersion("2.1.200")) }),
		"bad package":         with(func(c *Candidate) { c.PackageVersion = "x" }),
		"bad runtime":         with(func(c *Candidate) { c.RuntimeVersion = "1.0.0" }),
		"missing os":          with(func(c *Candidate) { c.OS = "" }),
		"unsafe os":           with(func(c *Candidate) { c.OS = "a\nb" }),
		"missing arch":        with(func(c *Candidate) { c.Arch = "" }),
		"missing entrypoint":  with(func(c *Candidate) { c.Entrypoint = "" }),
		"entrypoint denied":   with(func(c *Candidate) { c.Entrypoint = "mcp" }),
		"agent sdk malformed": with(func(c *Candidate) { c.AgentSDK = "0.3" }),
		"agent sdk too long": with(func(c *Candidate) {
			c.AgentSDK = strings.Repeat("1", 30) + "." + strings.Repeat("1", 30) + "." + strings.Repeat("1", 30)
		}),
		"codex version drift": {Provider: ProviderCodex, Version: MustParseVersion("0.150.0"), UserAgent: codexUA},
		"codex with package":  {Provider: ProviderCodex, Version: MustParseVersion("0.152.1"), UserAgent: codexUA, PackageVersion: "1.0.0"},
		"codex with os":       {Provider: ProviderCodex, Version: MustParseVersion("0.152.1"), UserAgent: codexUA, OS: "Linux"},
		"unknown provider":    {Provider: "gemini", Version: v, UserAgent: "x"},
		"control chars":       {Provider: ProviderCodex, Version: MustParseVersion("0.152.1"), UserAgent: "codex-tui/0.152.1\x01"},
	}
	for name, c := range bad {
		if err := ValidateCandidate(c, rules); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The allowlist is the CURRENT one.
	narrow := Rules{ClaudeEntrypoints: []string{"cli"}}
	if err := ValidateCandidate(good, narrow); err == nil {
		t.Error("sdk-ts accepted under a cli-only allowlist")
	}
}

func TestValidSessionID(t *testing.T) {
	for _, ok := range []string{"", "3f6c1a1e-7b6d-4c1e-9a1f-2b3c4d5e6f70", "thread_1:2.3", strings.Repeat("a", 128)} {
		if !ValidSessionID(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"has space", "a/b", strings.Repeat("a", 129), "x\n"} {
		if ValidSessionID(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
