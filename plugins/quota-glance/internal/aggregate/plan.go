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
// Providers report a plan in whatever form suits them. Claude sends a name that
// is already presentable ("Max", "Team") and Grok sends its tier ("SuperGrok
// Heavy"); Codex sends an enum token ("pro"), which is not what the tier is
// sold as. Turning that token into a name is a display concern, so it happens
// here rather than in quota-cache, which owns quota and not presentation.
//
// The names below are derived, not guessed: the Codex plan enum distinguishes
// prolite from pro, which is exactly the Pro 5x / Pro 20x split the tiers are
// sold under. An operator can still override any of it, because marketing names
// change more often than enum values do.
var defaultPlanLabels = map[string]string{
	"free":       "Free",
	"go":         "Go",
	"plus":       "Plus",
	"prolite":    "Pro 5x",
	"pro":        "Pro 20x",
	"team":       "Team",
	"business":   "Business",
	"enterprise": "Enterprise",
	"edu":        "Edu",
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
// matched case-insensitively against the provider's reported value.
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
// A value that already reads as a name is passed through untouched, so a
// provider that does the right thing is never second-guessed.
func planLabelOf(provider, raw string, overrides map[string]string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	key := strings.ToLower(trimmed)
	if label, ok := overrides[key]; ok {
		return label
	}
	// The enum table is Codex's. Applying it to every provider would rename
	// Claude's own "Pro" tier to OpenAI's "Pro 20x".
	if provider == "codex" {
		if label, ok := defaultPlanLabels[key]; ok {
			return label
		}
	}
	if enumToken(trimmed) {
		return prettify(trimmed)
	}
	return trimmed
}
