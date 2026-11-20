package aggregate

import (
	"sort"
	"strings"
	"unicode"
)

// printableLabel rejects a configured label carrying anything that is not
// printable. Operator-supplied, but it lands verbatim in a served document, so
// control characters, format characters, and the line and paragraph separators
// that terminate a JavaScript string literal are all excluded. IsGraphic admits
// ordinary spaces and every printable script.
func printableLabel(label string) bool {
	for _, r := range label {
		if !unicode.IsGraphic(r) {
			return false
		}
	}
	return true
}

// Plan display names.
//
// Providers report a plan in whatever form suits them, and the same token means
// different tiers at different providers, so each provider that sends tokens has
// a table of its own. Turning a token into a name is a display concern, so it
// happens here rather than in quota-cache, which owns quota and not
// presentation.
//
// Codex sends its plan enum ("pro"), which is not what the tier is sold as. The
// names below follow the CPA console, which renamed the two Pro tiers after
// their price — prolite is Pro 100 and pro is Pro 200 — and which reads
// self_serve_business_prolite as Business Premium. The console accepts prolite
// spelt three ways, so this does too.
var codexPlanLabels = map[string]string{
	"free":                        "Free",
	"go":                          "Go",
	"plus":                        "Plus",
	"prolite":                     "Pro 100",
	"pro_lite":                    "Pro 100",
	"pro-lite":                    "Pro 100",
	"pro":                         "Pro 200",
	"team":                        "Team",
	"business":                    "Business",
	"self_serve_business_prolite": "Business Premium",
	"enterprise":                  "Enterprise",
	"edu":                         "Edu",
}

// claudePlanLabels names the tokens quota-cache derives for a Claude account
// from its profile: the organization type, and for Max the rate-limit tier that
// separates the 5x plan from the 20x one. "max" alone is a Max account whose
// tier the profile did not state. A presentable name from an older snapshot
// ("Max", "Team") lands on the same entries case-insensitively.
//
// Kept apart from the Codex table on purpose: both providers sell a "Pro", and
// they are not the same tier.
var claudePlanLabels = map[string]string{
	"team":       "Team",
	"enterprise": "Enterprise",
	"max_20x":    "Max 20x",
	"max_5x":     "Max 5x",
	"max":        "Max",
	"pro":        "Pro",
	"free":       "Free",
}

// planTables holds every provider's token table, by provider id. Grok is
// absent deliberately: quota-cache reports the subscription's own display name
// ("SuperGrok Heavy"), which needs no table.
var planTables = map[string]map[string]string{
	"codex":  codexPlanLabels,
	"claude": claudePlanLabels,
}

// planLabelsFor is the token table for one provider, or nil.
func planLabelsFor(provider string) map[string]string { return planTables[provider] }

// bareKeyReaches reports whether an override keyed by the token alone applies
// to this provider's token.
//
// A bare key is the older form, written when Codex was the only provider that
// sent tokens, and it keeps the reach it had — every provider — with one
// exception. A token two tables hold — "pro", "team" — is a different tier at
// each, so a bare key for it is taken to mean Codex's, which is all it could
// have meant when it was written, and it does not reach the other table's
// provider: today that is Claude. Renaming Claude's needs the prefix:
// "claude:pro". A provider with no table of its own, Grok among them, is
// reached by every bare key exactly as before, shared tokens included. Without
// the exception, an operator who kept "pro: Pro 20x" for Codex would find every
// Claude Pro account renamed after an upgrade.
func bareKeyReaches(provider, key string) bool {
	if _, own := planTables[provider][key]; !own {
		return true
	}
	for other, table := range planTables {
		if _, shared := table[key]; shared && other != provider {
			return provider == "codex"
		}
	}
	return true
}

// maxPlanLabels bounds the configured override map. Operator-supplied, so this
// is a sanity limit rather than a security boundary.
const (
	maxPlanLabels    = 64
	maxPlanLabelRune = 64
)

// enumToken reports whether a value looks like a machine identifier rather than
// something written for a person to read: lowercase letters, digits and
// underscores only.
func enumToken(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}

// prettify renders an unrecognized enum token readably: self_serve_business
// becomes "Self Serve Business". Better than showing the raw token, and far
// better than showing nothing.
func prettify(token string) string {
	words := strings.Split(token, "_")
	for i, word := range words {
		if word == "" {
			continue
		}
		words[i] = strings.ToUpper(word[:1]) + word[1:]
	}
	return strings.Join(words, " ")
}

// NormalizePlanLabels prepares a configured override map for lookup. Keys are
// matched case-insensitively against the provider's reported value, either
// bare ("prolite") or scoped to one provider ("codex:pro").
func NormalizePlanLabels(configured map[string]string) map[string]string {
	if len(configured) == 0 {
		return nil
	}
	// Sorted, so which overrides survive the bound does not change between
	// process starts.
	keys := make([]string, 0, len(configured))
	for key := range configured {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(configured))
	for _, raw := range keys {
		key := strings.ToLower(strings.TrimSpace(raw))
		// "codex : pro" means what "codex:pro" means.
		if provider, token, scoped := strings.Cut(key, ":"); scoped {
			key = strings.TrimSpace(provider) + ":" + strings.TrimSpace(token)
		}
		label := strings.TrimSpace(configured[raw])
		if key == "" || label == "" || len(out) >= maxPlanLabels {
			continue
		}
		if len([]rune(key)) > maxPlanLabelRune || len([]rune(label)) > maxPlanLabelRune || !printableLabel(label) {
			continue
		}
		out[key] = label
	}
	return out
}

// planLabelOf resolves the name shown beside a credential.
//
// An override scoped to the provider wins outright; a bare one wins wherever
// it reaches (see bareKeyReaches). A value that already reads as a name is
// passed through untouched, so a provider that does the right thing is never
// second-guessed.
func planLabelOf(provider, raw string, overrides map[string]string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	key := strings.ToLower(trimmed)
	if label, ok := overrides[provider+":"+key]; ok {
		return label
	}
	if label, ok := overrides[key]; ok && bareKeyReaches(provider, key) {
		return label
	}
	// Each provider's own table and no other's. Applying the Codex table to
	// Claude would rename Claude's "Pro" tier to OpenAI's "Pro 200".
	if label, ok := planLabelsFor(provider)[key]; ok {
		return label
	}
	if enumToken(trimmed) {
		return prettify(trimmed)
	}
	return trimmed
}
