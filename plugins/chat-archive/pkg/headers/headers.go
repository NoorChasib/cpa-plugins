// Package headers implements the request and response header allowlists, the X-Archive-* hints and
// URL stripping. It is pure Go; it imports pkg/mask, pkg/fingerprint
// and pkg/wire only.
//
// The exported API is frozen: api_test.go pins every signature, and an exported identifier, field
// or JSON tag is never changed or removed, only added.
package headers

import (
	"crypto/sha256"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/fingerprint"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// Limits on the kept headers and hints.
const (
	maxHeaders    = 64
	maxValues     = 32
	maxValueBytes = 1 << 10
	maxHintBytes  = 256
	archivePrefix = "x-archive-"
)

// requestExact are the request header names kept by exact name. An exact name is kept
// even when it matches the deny pattern (idempotency-key).
var requestExact = setOf(
	"anthropic-version", "anthropic-beta", "content-type", "accept", "user-agent", "x-app",
	"idempotency-key", "originator", "version", "openai-beta", "session_id", "session-id", "thread_id",
	"thread-id", "conversation_id", "x-session-id", "x-conversation-id", "x-client-request-id",
	"x-request-id", "x-thread-id", "x-parent-session-id", "x-session-affinity", "x-slot-session-id",
	"x-task-id", "x-http-session-id", "x-codex-turn-metadata", "x-codex-beta-features",
	"x-codex-window-id", "x-codex-parent-thread-id", "x-openai-subagent",
	"x-openai-internal-codex-responses-lite",
)

var requestPrefixes = []string{"x-stainless-", "x-claude-code-"}

// requestHashOnly headers are stored as sha256 and length only; requestHMAC as a k1 fingerprint.
var (
	requestHashOnly = setOf("x-codex-turn-state", "x-codex-routing-hint")
	requestHMAC     = setOf("chatgpt-account-id")
)

var (
	responseExact    = setOf("content-type", "request-id", "x-request-id", "retry-after", "x-should-retry", "openai-processing-ms", "openai-version")
	responsePrefixes = []string{"anthropic-ratelimit-", "x-ratelimit-", "x-codex-"}
	responseHashOnly = setOf("x-codex-turn-state")
)

// denyRE drops a prefix-allowed name: deny overrides a prefix allow, never an exact name.
var denyRE = regexp.MustCompile(`(?i)(key|token|secret|passw|sig|credential|auth|cookie)`)

// hintSetters map the seven X-Archive-* hint names to their ArchiveHints field.
var hintSetters = map[string]func(*wire.ArchiveHints, string){
	"x-archive-machine":     func(h *wire.ArchiveHints, v string) { h.Machine = v },
	"x-archive-origin":      func(h *wire.ArchiveHints, v string) { h.Origin = v },
	"x-archive-ci-run":      func(h *wire.ArchiveHints, v string) { h.CIRun = v },
	"x-archive-ci-job":      func(h *wire.ArchiveHints, v string) { h.CIJob = v },
	"x-archive-ci-workflow": func(h *wire.ArchiveHints, v string) { h.CIWorkflow = v },
	"x-archive-runner":      func(h *wire.ArchiveHints, v string) { h.Runner = v },
	"x-archive-canary":      func(h *wire.ArchiveHints, v string) { h.Canary = v },
}

// FilterRequest applies the request header rules to h:
//   - kept: the exact-name and prefix allowlist, where a name matching the deny pattern is dropped
//     unless it is an exact name; lowercase names,
//     values masked with m, each at most 1 KiB, at most 64 headers x 32 values; x-codex-turn-state and
//     x-codex-routing-hint as SHA256+Len only; chatgpt-account-id as FP = fingerprint.HMAC(hmacKeyID,
//     hmacKey, value);
//   - hints: the seven X-Archive-* hint headers, masked, at most 256 printable ASCII each, with every
//     other x-archive-* name counted in Unknown;
//   - clear: the original names of every x-archive-* header present, for the before-auth ClearHeaders.
func FilterRequest(h http.Header, m *mask.Masker, hmacKeyID string, hmacKey []byte) (kept []wire.Header, hints wire.ArchiveHints, clear []string) {
	clear = ArchiveHeaderNames(h)
	unknown := map[string]bool{}
	for _, g := range groups(h) {
		switch {
		case strings.HasPrefix(g.name, archivePrefix):
			if set, ok := hintSetters[g.name]; ok {
				set(&hints, hintValue(g.values[0], m))
			} else {
				unknown[g.name] = true
			}
		case requestHashOnly[g.name]:
			kept = append(kept, hashOnly(g.name, g.values[0]))
		case requestHMAC[g.name]:
			kept = append(kept, wire.Header{Name: g.name, FP: fingerprint.HMAC(hmacKeyID, hmacKey, g.values[0])})
		case requestExact[g.name], hasPrefix(g.name, requestPrefixes) && !denyRE.MatchString(g.name):
			kept = append(kept, maskedHeader(g, m))
		}
	}
	hints.Unknown = len(unknown)
	return capHeaders(kept), hints, clear
}

// FilterResponse applies the response header rules (also used for UsageRecord.ResponseHeaders):
// the kept names and prefixes, the deny pattern, x-codex-turn-state as hash only, values masked.
func FilterResponse(h http.Header, m *mask.Masker) []wire.Header {
	var kept []wire.Header
	for _, g := range groups(h) {
		switch {
		case responseHashOnly[g.name]:
			kept = append(kept, hashOnly(g.name, g.values[0]))
		case responseExact[g.name], hasPrefix(g.name, responsePrefixes) && !denyRE.MatchString(g.name):
			kept = append(kept, maskedHeader(g, m))
		}
	}
	return capHeaders(kept)
}

// StripURL returns raw as scheme://host/path with userinfo, query and fragment removed and the host
// and path masked with m, for request URLs and provider media URLs. A raw value that does not parse
// as an absolute URL with a scheme and host returns "". The path is masked in
// its decoded form, so a percent-escape inside a token ("ghp%5F…", "%67hp_…") cannot hide it; a path
// masking leaves unchanged keeps its original escaping, a masked one is re-escaped with its
// placeholders' brackets left as written. A host name is masked too (an IP literal is kept). Host and
// path are in their escaped forms, so the result is ASCII: url.Parse decodes a host escape such as
// %80 to a raw byte, and that byte must not reach a canon/v1 piece as invalid UTF-8.
func StripURL(raw string, m *mask.Masker) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	host := u.Host
	if !strings.HasPrefix(host, "[") {
		name, _ := m.String(u.Hostname())
		host = name
		if p := u.Port(); p != "" {
			host += ":" + p
		}
	}
	path := u.EscapedPath()
	if masked, _ := m.String(u.Path); masked != u.Path {
		path = placeholderBrackets.Replace((&url.URL{Path: masked}).EscapedPath())
	}
	return (&url.URL{Scheme: u.Scheme, Host: host}).String() + path
}

// placeholderBrackets undoes the escaping of '[' and ']' in a re-escaped path, so a placeholder
// stays readable ("[masked:…]"); both are valid in a path (net/url validEncoded).
var placeholderBrackets = strings.NewReplacer("%5B", "[", "%5D", "]")

// ArchiveHeaderNames returns the original names of every x-archive-* header in h (case-insensitive),
// without masking or allocation beyond the result, so the synchronous before-auth hook can fill
// ClearHeaders on the hot path.
func ArchiveHeaderNames(h http.Header) []string {
	var names []string
	for k := range h {
		if len(k) > len(archivePrefix) && strings.EqualFold(k[:len(archivePrefix)], archivePrefix) {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	return names
}

// group is one lowercase header name with the values of every original key that lowers to it, in
// sorted original-key order.
type group struct {
	name   string
	values []string
}

// groups returns h's non-empty headers by lowercase name, sorted by name.
func groups(h http.Header) []group {
	keys := make([]string, 0, len(h))
	for k, v := range h {
		if len(v) > 0 {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	idx := map[string]int{}
	var out []group
	for _, k := range keys {
		name := strings.ToLower(k)
		i, ok := idx[name]
		if !ok {
			i = len(out)
			idx[name] = i
			out = append(out, group{name: name})
		}
		out[i].values = append(out[i].values, h[k]...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func maskedHeader(g group, m *mask.Masker) wire.Header {
	vals := g.values
	if len(vals) > maxValues {
		vals = vals[:maxValues]
	}
	out := wire.Header{Name: g.name, Values: make([]string, len(vals))}
	for i, v := range vals {
		masked, _ := m.String(v)
		out.Values[i] = truncateUTF8(masked, maxValueBytes)
	}
	return out
}

// hashOnly keeps sha256 and length of the header's first value.
func hashOnly(name, value string) wire.Header {
	sum := wire.Hash(sha256.Sum256([]byte(value)))
	return wire.Header{Name: name, SHA256: &sum, Len: len(value)}
}

// hintValue keeps v's printable ASCII bytes (0x20-0x7E), masks the result and caps it at 256 bytes.
// Filtering first means a secret split by a TAB, DEL or obs-text byte is masked whole: masking first
// and dropping the byte afterwards would join two halves the masker never saw as one token.
func hintValue(v string, m *mask.Masker) string {
	b := make([]byte, 0, len(v))
	for i := 0; i < len(v); i++ {
		if c := v[i]; c >= 0x20 && c < 0x7f {
			b = append(b, c)
		}
	}
	masked, _ := m.String(string(b))
	if len(masked) > maxHintBytes {
		masked = masked[:maxHintBytes]
	}
	return masked
}

func capHeaders(kept []wire.Header) []wire.Header {
	if len(kept) > maxHeaders {
		kept = kept[:maxHeaders]
	}
	return kept
}

// truncateUTF8 cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func hasPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func setOf(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}
