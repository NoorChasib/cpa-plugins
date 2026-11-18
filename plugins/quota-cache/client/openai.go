package client

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// The OpenAI API credit is the monthly credit a ChatGPT Pro or Business plan
// grants one OpenAI API organization. quota-cache tracks it beside the Claude
// API credit, in four files of its own next to the snapshot with the Claude
// files' schemas: a meter (OpenAIMeterPath, an APIMeter LoadMeter reads), the
// organizations listed on its page (OpenAIOrganizationsPath), their saved
// values (OpenAISettingsPath) and the published credit state
// (OpenAICreditsPath, a CreditState LoadCredits reads and CreditsAt judges).
// No Claude file changes, and the snapshot has no entry for an OpenAI
// organization.
//
// The meter counts only CPA's openai-compatibility API-key traffic, never a
// codex-api-key's: OpenAI says Codex usage through API keys is not covered by
// the credit. Nothing is ever sent to OpenAI. An organization is named as the
// openai-organization response header names it, not by its org-… Organization
// ID.

// OpenAIMeterPath is where quota-cache saves its OpenAI API meter for the
// snapshot at snapshotPath: the same path with a final ".json" replaced by
// ".openai-meter.json", else with ".openai-meter.json" appended. For
// DefaultPath it is plugins/data/quota-cache/snapshot.openai-meter.json.
func OpenAIMeterPath(snapshotPath string) string {
	return strings.TrimSuffix(snapshotPath, ".json") + ".openai-meter.json"
}

// OpenAICreditsPath is where quota-cache saves its OpenAI API credit state for
// the snapshot at snapshotPath: the same path with a final ".json" replaced by
// ".openai-credits.json", else with ".openai-credits.json" appended. For
// DefaultPath it is plugins/data/quota-cache/snapshot.openai-credits.json.
func OpenAICreditsPath(snapshotPath string) string {
	return strings.TrimSuffix(snapshotPath, ".json") + ".openai-credits.json"
}

// OpenAIOrganizationsPath is where quota-cache keeps the OpenAI API
// organizations the operator edits on its page, for the snapshot at
// snapshotPath: the same path with a final ".json" replaced by
// ".openai-organizations.json", else with ".openai-organizations.json"
// appended. It is operator data, not cache, and must move with the snapshot.
func OpenAIOrganizationsPath(snapshotPath string) string {
	return strings.TrimSuffix(snapshotPath, ".json") + ".openai-organizations.json"
}

// OpenAISettingsPath is where quota-cache keeps the monthly credits, refill
// dates and API Platform readings saved on its page for the OpenAI API
// organizations, for the snapshot at snapshotPath: the same path with a final
// ".json" replaced by ".openai-settings.json", else with
// ".openai-settings.json" appended. It is operator data, not cache, and must
// move with the snapshot.
func OpenAISettingsPath(snapshotPath string) string {
	return strings.TrimSuffix(snapshotPath, ".json") + ".openai-settings.json"
}

// openAIOrganizationShape is what an openai-organization header value has
// been seen to look like: a slug such as "user-" and 24 base62 characters for
// a personal organization, or a name and a suffix for a named one. OpenAI
// does not document it.
var openAIOrganizationShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// NormalizeOpenAIOrganization reads an OpenAI API organization as the
// openai-organization response header names it, or as typed on Quota
// Cache's page: trimmed, 1 to 64 bytes of ASCII letters, digits, '.', '_'
// or '-', the first a letter or digit. Its case is kept: captured values
// mix cases ("user-" and 24 base62 characters), and the meter matches the
// header exactly. False for anything else.
//
// A typed org-… Organization ID has this shape too and is accepted, in case
// OpenAI one day names organizations that way, although its responses do not
// today. Like a Console organization id, the value is not a secret, but a
// named organization's carries its name.
func NormalizeOpenAIOrganization(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if !openAIOrganizationShape.MatchString(s) {
		return "", false
	}
	return s, true
}

// OpenAICreditAccountPrefix begins every OpenAI account id. It is not "org-",
// which Claude's accounts use and OpenAI's own Organization IDs begin with.
const OpenAICreditAccountPrefix = "oai-"

// OpenAICreditOrgAccount is the account id of the OpenAI organization whose
// slug NormalizeOpenAIOrganization returned: "oai-" and the first 12 hex
// digits of the SHA-256 of the slug, trimmed, case kept. Like
// APICreditOrgAccount, it depends on nothing but the organization, so the
// values saved on Quota Cache's page follow the organization through a label
// rename. Two slugs that differ only in case are two organizations, as the
// header names them.
func OpenAICreditOrgAccount(slug string) string {
	// For a valid slug, trimming is exactly what NormalizeOpenAIOrganization
	// does.
	sum := sha256.Sum256([]byte(strings.TrimSpace(slug)))
	return OpenAICreditAccountPrefix + hex.EncodeToString(sum[:6])
}

// MeterPromptOver272K is the prompt class of an OpenAI request with more than
// 272,000 input tokens, cached and written included, which OpenAI prices as
// long context for the whole request. Only the OpenAI meter uses it;
// MeterPromptClass, Claude's, never returns it.
const MeterPromptOver272K = "over_272k"

// The OpenAI service tiers a MeterUsage keeps, from the response's
// service_tier. Standard is "", so a Claude entry, which never has one, reads
// as Standard and encodes without the key.
const (
	MeterTierFlex      = "flex"
	MeterTierFast      = "fast"      // the response's "fast" or "priority" (Fast mode, formerly Priority processing)
	MeterTierUltrafast = "ultrafast" // Ultrafast mode
	MeterTierUnknown   = "unknown"   // a service_tier this build does not know: never priced
)

// openAILongContext is the input size, in tokens, above which OpenAI prices a
// request as long context. The pricing page says "272K", read as 272,000, as
// Anthropic's "100K" is 100,000.
const openAILongContext = 272_000

// OpenAIMeterPromptClass is MeterPromptOver272K when inputTokens > 272000,
// else "". inputTokens is the request's whole input, cached and written
// tokens included.
func OpenAIMeterPromptClass(inputTokens uint64) string {
	if inputTokens > openAILongContext {
		return MeterPromptOver272K
	}
	return ""
}

// NormalizeOpenAIServiceTier maps the response's service_tier, trimmed and
// lower-cased: "", "auto", "default" and "scale" are "" (Standard); "flex" is
// MeterTierFlex; "priority" and "fast" are MeterTierFast; "ultrafast" is
// MeterTierUltrafast; anything else is MeterTierUnknown.
//
// Scale Tier is reserved capacity a subscriber's organization does not use,
// and pricing it at Standard can only overstate spend. A tier this build does
// not know may cost more than Standard, as Fast and Ultrafast do, so it is
// kept apart and left unpriced rather than read as Standard.
func NormalizeOpenAIServiceTier(tier string) string {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "", "auto", "default", "scale":
		return ""
	case "flex":
		return MeterTierFlex
	case "priority", "fast":
		return MeterTierFast
	case "ultrafast":
		return MeterTierUltrafast
	}
	return MeterTierUnknown
}
