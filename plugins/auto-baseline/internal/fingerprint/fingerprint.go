package fingerprint

import (
	"fmt"
	"net/textproto"
	"regexp"
	"strings"
)

// Compiled-in baseline defaults of the audited CPA build
// (v7.2.146-3-g81e1b53). When config.yaml omits a field, CPA falls back to
// these values (internal/runtime/executor/helps/claude_device_profile.go:22-27,
// internal/runtime/executor/codex_executor_request.go:26). They are the
// plugin's assumption about the "effective" baseline for an unconfigured
// file; a newer CPA release may raise them, which only makes the plugin's
// floor conservative, never harmful.
const (
	compiledClaudeVersion = "2.1.220"
	compiledCodexVersion  = "0.146.0"

	CompiledClaudeUserAgent      = "claude-cli/" + compiledClaudeVersion + " (external, cli)"
	CompiledClaudePackageVersion = "0.94.0"
	CompiledClaudeRuntimeVersion = "v26.3.0"
	CompiledClaudeOS             = "MacOS"
	CompiledClaudeArch           = "arm64"
	CompiledCodexUserAgent       = "codex-tui/" + compiledCodexVersion + " (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 (codex-tui; " + compiledCodexVersion + ")"
	CompiledCPAVersion           = "v7.2.146-3-g81e1b53"
)

// Compiled baseline versions parsed from the constants above.
var (
	CompiledClaudeBaselineVersion = MustParseVersion(compiledClaudeVersion)
	CompiledCodexBaselineVersion  = MustParseVersion(compiledCodexVersion)
)

// Header names consulted by the classifier.
const (
	HeaderUserAgent               = "User-Agent"
	HeaderXApp                    = "X-App"
	HeaderAnthropicVersion        = "Anthropic-Version"
	HeaderAnthropicBeta           = "Anthropic-Beta"
	HeaderStainlessLang           = "X-Stainless-Lang"
	HeaderStainlessRuntime        = "X-Stainless-Runtime"
	HeaderStainlessPackageVersion = "X-Stainless-Package-Version"
	HeaderStainlessRuntimeVersion = "X-Stainless-Runtime-Version"
	HeaderStainlessOS             = "X-Stainless-Os"
	HeaderStainlessArch           = "X-Stainless-Arch"
	HeaderClaudeSessionID         = "X-Claude-Code-Session-Id"
	HeaderOriginator              = "Originator"
	HeaderCodexSessionID          = "Session_id"
	HeaderCodexSessionIDAlt       = "Session-Id"
	HeaderCodexThreadID           = "Thread-Id"

	// ClaudeCodeBeta is the beta flag every Claude Code /v1/messages request
	// carries; count_tokens and helper requests may omit it.
	ClaudeCodeBeta = "claude-code-20250219"

	// RequiredAnthropicVersion is the API version Claude Code pins.
	RequiredAnthropicVersion = "2023-06-01"

	maxUserAgentLength = 512
	maxSessionIDLength = 128
)

var (
	// claudeUserAgentPattern mirrors CPA's claudeCodeNativeUserAgentPattern
	// (helps/claude_client_detection.go:32) with capture groups. It is
	// case-insensitive like CPA's.
	claudeUserAgentPattern = regexp.MustCompile(`(?i)^claude-cli/(\d+)\.(\d+)\.(\d+)\s+\(external,\s*([^,)\s]+)(?:,\s*agent-sdk/(\d+\.\d+\.\d+))?\)$`)
	// codexUserAgentPattern accepts the two Codex CLI UA products seen in the
	// wild (codex_cli_rs before ~0.145, codex-tui since).
	codexUserAgentPattern = regexp.MustCompile(`^(codex_cli_rs|codex-tui)/(\d+)\.(\d+)\.(\d+)\b`)
	packageVersionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	runtimeVersionPattern = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	sessionIDPattern      = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	entrypointPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	agentSDKPattern       = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	safeFieldPattern      = regexp.MustCompile(`^[A-Za-z0-9 ._:+()/;-]{1,64}$`)
)

// Candidate is one internally consistent fingerprint tuple observed on a
// single request. For Claude the UserAgent is the canonical CLI form
// (claude-cli/<version> (external, cli)); for Codex it is the full observed
// UA string (which carries OS/terminal details CPA writes verbatim).
type Candidate struct {
	Provider       Provider `json:"provider"`
	Version        Version  `json:"version"`
	UserAgent      string   `json:"user_agent"`
	PackageVersion string   `json:"package_version,omitempty"`
	RuntimeVersion string   `json:"runtime_version,omitempty"`
	OS             string   `json:"os,omitempty"`
	Arch           string   `json:"arch,omitempty"`
	Entrypoint     string   `json:"entrypoint,omitempty"`
	AgentSDK       string   `json:"agent_sdk,omitempty"`
}

// Key identifies the tuple that would be written to config.yaml. Two
// observations with the same Key are counted together toward quorum.
func (c Candidate) Key() string {
	switch c.Provider {
	case ProviderClaude:
		return "claude|" + c.Version.String() + "|" + c.PackageVersion + "|" + c.RuntimeVersion
	case ProviderCodex:
		return "codex|" + c.UserAgent
	default:
		return string(c.Provider) + "|" + c.UserAgent
	}
}

// CanonicalClaudeUserAgent renders the CLI-form UA CPA expects as a baseline.
func CanonicalClaudeUserAgent(v Version) string {
	return "claude-cli/" + v.String() + " (external, cli)"
}

// Rules are the operator-configurable classification knobs.
type Rules struct {
	// ClaudeEntrypoints is a lowercase allowlist; empty means none allowed.
	ClaudeEntrypoints []string
	// RequireClaudeCodeBeta rejects Claude requests without the
	// claude-code-20250219 beta in Anthropic-Beta.
	RequireClaudeCodeBeta bool
}

func (r Rules) entrypointAllowed(entrypoint string) bool {
	for _, allowed := range r.ClaudeEntrypoints {
		if allowed == entrypoint {
			return true
		}
	}
	return false
}

// Rejection reason buckets surfaced in status counters.
const (
	ReasonNotClient           = "not_client"
	ReasonEntrypointDenied    = "claude_entrypoint_not_allowed"
	ReasonMissingXApp         = "claude_missing_x_app_cli"
	ReasonAnthropicVersion    = "claude_anthropic_version_mismatch"
	ReasonStainlessLang       = "claude_stainless_lang_not_js"
	ReasonStainlessRuntime    = "claude_stainless_runtime_not_node"
	ReasonPackageVersion      = "claude_package_version_malformed"
	ReasonRuntimeVersion      = "claude_runtime_version_malformed"
	ReasonStainlessOSArch     = "claude_stainless_os_or_arch_missing"
	ReasonBetaMissing         = "claude_code_beta_missing"
	ReasonCodexOriginator     = "codex_originator_missing"
	ReasonUserAgentTooLong    = "user_agent_too_long"
	ReasonUnsafeFieldContents = "unsafe_field_contents"
)

// Result is the outcome of classifying one request.
type Result struct {
	// OK is true when Candidate and SessionID are valid observations.
	OK bool
	// Provider is the detected provider, set whenever the User-Agent looked
	// like a managed client, even when the request was rejected.
	Provider Provider
	// Reason is the rejection bucket when OK is false.
	Reason    string
	Candidate Candidate
	// SessionID is the client session identifier, or "" when anonymous.
	SessionID string
}

// Classify inspects request headers only. Bodies are never parsed: the
// interceptor runs on every model request, and the local-trust threat model
// (see docs/architecture.md) does not justify the cost.
func Classify(headers map[string][]string, rules Rules) Result {
	ua := strings.TrimSpace(headerValue(headers, HeaderUserAgent))
	if ua == "" {
		return Result{Reason: ReasonNotClient}
	}
	if len(ua) > maxUserAgentLength {
		return Result{Reason: ReasonUserAgentTooLong}
	}
	if m := claudeUserAgentPattern.FindStringSubmatch(ua); m != nil {
		return classifyClaude(headers, m, rules)
	}
	if m := codexUserAgentPattern.FindStringSubmatch(ua); m != nil {
		return classifyCodex(headers, ua, m)
	}
	return Result{Reason: ReasonNotClient}
}

func classifyClaude(headers map[string][]string, m []string, rules Rules) Result {
	reject := func(reason string) Result {
		return Result{Provider: ProviderClaude, Reason: reason}
	}
	version, err := newVersion(m[1], m[2], m[3])
	if err != nil {
		return reject(ReasonNotClient)
	}
	entrypoint := strings.ToLower(m[4])
	if !rules.entrypointAllowed(entrypoint) {
		return reject(ReasonEntrypointDenied)
	}
	if !strings.EqualFold(strings.TrimSpace(headerValue(headers, HeaderXApp)), "cli") {
		return reject(ReasonMissingXApp)
	}
	if strings.TrimSpace(headerValue(headers, HeaderAnthropicVersion)) != RequiredAnthropicVersion {
		return reject(ReasonAnthropicVersion)
	}
	if !strings.EqualFold(strings.TrimSpace(headerValue(headers, HeaderStainlessLang)), "js") {
		return reject(ReasonStainlessLang)
	}
	if !strings.EqualFold(strings.TrimSpace(headerValue(headers, HeaderStainlessRuntime)), "node") {
		return reject(ReasonStainlessRuntime)
	}
	packageVersion := strings.TrimSpace(headerValue(headers, HeaderStainlessPackageVersion))
	if !packageVersionPattern.MatchString(packageVersion) {
		return reject(ReasonPackageVersion)
	}
	runtimeVersion := strings.TrimSpace(headerValue(headers, HeaderStainlessRuntimeVersion))
	if !runtimeVersionPattern.MatchString(runtimeVersion) {
		return reject(ReasonRuntimeVersion)
	}
	osName := strings.TrimSpace(headerValue(headers, HeaderStainlessOS))
	arch := strings.TrimSpace(headerValue(headers, HeaderStainlessArch))
	if osName == "" || arch == "" {
		return reject(ReasonStainlessOSArch)
	}
	if !safeFieldPattern.MatchString(osName) || !safeFieldPattern.MatchString(arch) {
		return reject(ReasonUnsafeFieldContents)
	}
	if rules.RequireClaudeCodeBeta && !headerListContains(headers, HeaderAnthropicBeta, ClaudeCodeBeta) {
		return reject(ReasonBetaMissing)
	}
	return Result{
		OK:       true,
		Provider: ProviderClaude,
		Candidate: Candidate{
			Provider:       ProviderClaude,
			Version:        version,
			UserAgent:      CanonicalClaudeUserAgent(version),
			PackageVersion: packageVersion,
			RuntimeVersion: runtimeVersion,
			OS:             osName,
			Arch:           arch,
			Entrypoint:     entrypoint,
			AgentSDK:       m[5],
		},
		SessionID: sessionID(headers, HeaderClaudeSessionID),
	}
}

func classifyCodex(headers map[string][]string, ua string, m []string) Result {
	reject := func(reason string) Result {
		return Result{Provider: ProviderCodex, Reason: reason}
	}
	version, err := newVersion(m[2], m[3], m[4])
	if err != nil {
		return reject(ReasonNotClient)
	}
	if strings.TrimSpace(headerValue(headers, HeaderOriginator)) == "" {
		return reject(ReasonCodexOriginator)
	}
	if !safeUserAgent(ua) {
		return reject(ReasonUnsafeFieldContents)
	}
	return Result{
		OK:       true,
		Provider: ProviderCodex,
		Candidate: Candidate{
			Provider:  ProviderCodex,
			Version:   version,
			UserAgent: ua,
		},
		// X-Client-Request-Id is deliberately absent: it is per request and
		// would make every observation look like a distinct session.
		SessionID: sessionID(headers, HeaderCodexSessionID, HeaderCodexSessionIDAlt, HeaderCodexThreadID),
	}
}

// safeUserAgent rejects control characters and YAML-hostile line breaks so
// the value can be written into config.yaml as a plain scalar.
func safeUserAgent(ua string) bool {
	for _, r := range ua {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// sessionID returns the first usable session header value, or "" for an
// anonymous request. Values that do not look like identifiers are treated as
// anonymous rather than rejected so a client with an odd session format still
// contributes observations; anonymous observations count toward
// min-observations but never toward min-distinct-sessions.
func sessionID(headers map[string][]string, names ...string) string {
	for _, name := range names {
		value := strings.TrimSpace(headerValue(headers, name))
		if value == "" {
			continue
		}
		if len(value) <= maxSessionIDLength && sessionIDPattern.MatchString(value) {
			return value
		}
	}
	return ""
}

// headerValue performs a case-insensitive single-value lookup. The audited
// host sends canonical MIME keys (a gin request header clone), so the
// canonical lookup hits first and the scan is only a fallback.
func headerValue(headers map[string][]string, name string) string {
	if len(headers) == 0 {
		return ""
	}
	if values, ok := headers[textproto.CanonicalMIMEHeaderKey(name)]; ok {
		if len(values) > 0 {
			return values[0]
		}
		return ""
	}
	if values, ok := headers[name]; ok && len(values) > 0 {
		return values[0]
	}
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

// headerListContains reports whether any comma-separated token of any value
// of the header equals token (case-insensitive).
func headerListContains(headers map[string][]string, name, token string) bool {
	if len(headers) == 0 {
		return false
	}
	canonical := textproto.CanonicalMIMEHeaderKey(name)
	for key, values := range headers {
		if key != canonical && !strings.EqualFold(key, name) {
			continue
		}
		for _, value := range values {
			for _, part := range strings.Split(value, ",") {
				if strings.EqualFold(strings.TrimSpace(part), token) {
					return true
				}
			}
		}
	}
	return false
}

// claudeCLIVersionPrefix mirrors CPA's lenient claudeCLIVersionPattern
// (helps/claude_device_profile.go:33), which only requires the prefix. It is
// used to read the version out of an operator-supplied baseline UA.
var claudeCLIVersionPrefix = regexp.MustCompile(`^claude-cli/(\d+)\.(\d+)\.(\d+)`)

// ParseClaudeUserAgentVersion extracts the CLI version from a baseline UA the
// same way CPA does when computing the effective baseline.
func ParseClaudeUserAgentVersion(ua string) (Version, bool) {
	m := claudeCLIVersionPrefix.FindStringSubmatch(strings.TrimSpace(ua))
	if m == nil {
		return Version{}, false
	}
	v, err := newVersion(m[1], m[2], m[3])
	return v, err == nil
}

// ParseCodexUserAgentVersion extracts the CLI version from a Codex UA.
func ParseCodexUserAgentVersion(ua string) (Version, bool) {
	m := codexUserAgentPattern.FindStringSubmatch(strings.TrimSpace(ua))
	if m == nil {
		return Version{}, false
	}
	v, err := newVersion(m[2], m[3], m[4])
	return v, err == nil
}

// ValidSessionID reports whether s is acceptable as a session identifier:
// the same predicate Classify applies to live session headers. Empty means
// anonymous and is valid.
func ValidSessionID(s string) bool {
	return s == "" || (len(s) <= maxSessionIDLength && sessionIDPattern.MatchString(s))
}

// ValidateCandidate checks that a candidate (typically restored from the
// state file) is structurally sound and would still be accepted under the
// current rules: provider known, version set, user agent consistent with the
// version and in the canonical form, Claude package/runtime/os/arch present
// and well-formed, entrypoint in the current allowlist, agent-sdk bounded.
// Restored entries that fail are dropped.
func ValidateCandidate(c Candidate, rules Rules) error {
	if c.Version.IsZero() {
		return fmt.Errorf("version is unset")
	}
	if len(c.UserAgent) == 0 || len(c.UserAgent) > maxUserAgentLength || !safeUserAgent(c.UserAgent) {
		return fmt.Errorf("user agent is empty, too long, or unsafe")
	}
	switch c.Provider {
	case ProviderClaude:
		if c.UserAgent != CanonicalClaudeUserAgent(c.Version) {
			return fmt.Errorf("claude user agent %q is not the canonical form for %s", c.UserAgent, c.Version)
		}
		if !packageVersionPattern.MatchString(c.PackageVersion) {
			return fmt.Errorf("claude package version %q is malformed", c.PackageVersion)
		}
		if !runtimeVersionPattern.MatchString(c.RuntimeVersion) {
			return fmt.Errorf("claude runtime version %q is malformed", c.RuntimeVersion)
		}
		if !safeFieldPattern.MatchString(c.OS) {
			return fmt.Errorf("claude os %q is missing or unsafe", c.OS)
		}
		if !safeFieldPattern.MatchString(c.Arch) {
			return fmt.Errorf("claude arch %q is missing or unsafe", c.Arch)
		}
		if !entrypointPattern.MatchString(c.Entrypoint) {
			return fmt.Errorf("claude entrypoint %q is missing or malformed", c.Entrypoint)
		}
		if !rules.entrypointAllowed(c.Entrypoint) {
			return fmt.Errorf("claude entrypoint %q is not in the current allowlist", c.Entrypoint)
		}
		if c.AgentSDK != "" && (len(c.AgentSDK) > 64 || !agentSDKPattern.MatchString(c.AgentSDK)) {
			return fmt.Errorf("claude agent-sdk %q is malformed", c.AgentSDK)
		}
	case ProviderCodex:
		v, ok := ParseCodexUserAgentVersion(c.UserAgent)
		if !ok || v != c.Version {
			return fmt.Errorf("codex user agent %q does not carry version %s", c.UserAgent, c.Version)
		}
		if c.PackageVersion != "" || c.RuntimeVersion != "" || c.OS != "" || c.Arch != "" || c.Entrypoint != "" || c.AgentSDK != "" {
			return fmt.Errorf("codex candidate must not carry claude-only fields")
		}
	default:
		return fmt.Errorf("unknown provider %q", c.Provider)
	}
	return nil
}
