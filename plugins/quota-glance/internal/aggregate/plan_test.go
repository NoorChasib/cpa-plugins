package aggregate

import (
	"fmt"
	"strings"
	"testing"
)

// Codex reports an enum token, not the name the tier is sold under. The enum
// separates prolite from pro, which the CPA console now sells as Pro 100 and
// Pro 200, and it reads self_serve_business_prolite as Business Premium.
// Confirmed against a live account: the top Pro subscription reports
// chatgpt_plan_type "pro".
func TestCodexEnumTokensBecomeTheNamesTheTiersAreSoldUnder(t *testing.T) {
	for raw, want := range map[string]string{
		"pro":                         "Pro 200",
		"prolite":                     "Pro 100",
		"pro_lite":                    "Pro 100",
		"pro-lite":                    "Pro 100",
		"self_serve_business_prolite": "Business Premium",
		"plus":                        "Plus",
		"team":                        "Team",
		"business":                    "Business",
		"free":                        "Free",
		"PRO":                         "Pro 200", // matched case-insensitively
		"  ProLite ":                  "Pro 100",
	} {
		if got := planLabelOf("codex", raw, nil); got != want {
			t.Errorf("planLabelOf(%q) = %q; want %q", raw, got, want)
		}
	}
}

// A provider that already sends something presentable is not second-guessed.
func TestPresentablePlanNamesPassThroughUnchanged(t *testing.T) {
	for _, raw := range []string{"Max", "Team", "SuperGrok Heavy", "Pro (20x)", "Team+"} {
		if got := planLabelOf("codex", raw, nil); got != raw {
			t.Errorf("planLabelOf(%q) = %q; a readable name must pass through", raw, got)
		}
	}
	if got := planLabelOf("codex", "", nil); got != "" {
		t.Errorf("planLabelOf(\"\") = %q", got)
	}
	if got := planLabelOf("codex", "   ", nil); got != "" {
		t.Errorf("blank plan = %q", got)
	}
}

// An enum value this build has never seen still renders readably rather than
// showing a raw token or nothing at all.
func TestUnknownEnumTokensAreRenderedReadably(t *testing.T) {
	for raw, want := range map[string]string{
		"self_serve_business_usage_based": "Self Serve Business Usage Based",
		"ent26":                           "Ent26",
		"edu_plus":                        "Edu Plus",
	} {
		if got := planLabelOf("codex", raw, nil); got != want {
			t.Errorf("planLabelOf(%q) = %q; want %q", raw, got, want)
		}
	}
}

// Marketing names change more often than enum values, so an operator can
// override any of it without waiting for a release.
func TestConfiguredOverridesWin(t *testing.T) {
	overrides := NormalizePlanLabels(map[string]string{
		"  PRO  ":   " Pro 200 (2027) ",
		"max":       "Max 20x",
		"max_5x":    "Max (5x)",
		"SuperGrok": "SuperGrok (annual)",
		"":          "ignored",
		"blank":     "   ",
	})
	if got := planLabelOf("codex", "pro", overrides); got != "Pro 200 (2027)" {
		t.Fatalf("override ignored: %q", got)
	}
	// Overrides also reach values that would otherwise pass through untouched,
	// and win over a provider's own table for a token only that table holds.
	if got := planLabelOf("codex", "Max", overrides); got != "Max 20x" {
		t.Fatalf("override on a presentable name: %q", got)
	}
	if got := planLabelOf("claude", "max_5x", overrides); got != "Max (5x)" {
		t.Fatalf("override on a Claude token: %q", got)
	}
	if got := planLabelOf("xai", "SuperGrok", overrides); got != "SuperGrok (annual)" {
		t.Fatalf("override on a Grok display name: %q", got)
	}
	if _, ok := overrides[""]; ok {
		t.Fatal("an empty key was kept")
	}
	if _, ok := overrides["blank"]; ok {
		t.Fatal("a blank label was kept")
	}
	if NormalizePlanLabels(nil) != nil {
		t.Fatal("an absent map must stay nil")
	}
}

// "pro" is Codex's top tier and Claude's entry one. A bare override for it was
// written for Codex — the only provider that sent tokens when bare keys were
// the only form — and must not rename Claude's Pro after an upgrade. A key
// scoped to a provider reaches that provider alone, and wins over a bare one.
func TestABareOverrideForASharedTokenStaysWithCodex(t *testing.T) {
	bare := NormalizePlanLabels(map[string]string{"pro": "Pro 20x", "team": "Team (Biz)"})
	for _, one := range []struct{ provider, raw, want string }{
		{"codex", "pro", "Pro 20x"},
		{"codex", "team", "Team (Biz)"},
		{"claude", "pro", "Pro"},
		{"claude", "Pro", "Pro"},
		{"claude", "team", "Team"},
	} {
		if got := planLabelOf(one.provider, one.raw, bare); got != one.want {
			t.Errorf("bare: planLabelOf(%q, %q) = %q; want %q", one.provider, one.raw, got, one.want)
		}
	}

	scoped := NormalizePlanLabels(map[string]string{
		"pro":             "bare",
		"Codex:PRO":       "Pro 20x",
		" claude : pro ":  "Claude Pro",
		"xai:SuperGrok":   "SuperGrok (annual)",
		"claude:max_20x":  "Max 20x (team seat)",
		"gemini:whatever": "unused",
	})
	for _, one := range []struct{ provider, raw, want string }{
		{"codex", "pro", "Pro 20x"},
		{"claude", "pro", "Claude Pro"},
		{"claude", "max_20x", "Max 20x (team seat)"},
		{"codex", "max_20x", "Max 20x"},
		{"xai", "SuperGrok", "SuperGrok (annual)"},
		{"claude", "SuperGrok", "SuperGrok"},
	} {
		if got := planLabelOf(one.provider, one.raw, scoped); got != one.want {
			t.Errorf("scoped: planLabelOf(%q, %q) = %q; want %q", one.provider, one.raw, got, one.want)
		}
	}
}

// The exception for a shared token is Claude's alone. Grok has no table, so an
// unprefixed key reaches it as every bare key always has, shared tokens
// included — which is what the README and the example configuration tell an
// operator. Grok's prefix is its provider id, xai.
func TestABareOverrideForASharedTokenStillReachesGrok(t *testing.T) {
	overrides := NormalizePlanLabels(map[string]string{"free": "Codex Free", "xai:SuperGrok Heavy": "Heavy"})
	for _, one := range []struct{ provider, raw, want string }{
		{"codex", "free", "Codex Free"},
		{"xai", "Free", "Codex Free"},
		{"claude", "free", "Free"},
		{"xai", "SuperGrok Heavy", "Heavy"},
		{"codex", "SuperGrok Heavy", "SuperGrok Heavy"},
	} {
		if got := planLabelOf(one.provider, one.raw, overrides); got != one.want {
			t.Errorf("planLabelOf(%q, %q) = %q; want %q", one.provider, one.raw, got, one.want)
		}
	}
}

func TestOverrideMapIsBounded(t *testing.T) {
	oversized := map[string]string{}
	for i := 0; i < maxPlanLabels*3; i++ {
		oversized[string(rune('a'+i%26))+string(rune('a'+i/26))] = "label"
	}
	if got := len(NormalizePlanLabels(oversized)); got > maxPlanLabels {
		t.Fatalf("kept %d overrides; want at most %d", got, maxPlanLabels)
	}
	long := NormalizePlanLabels(map[string]string{"pro": string(make([]rune, maxPlanLabelRune+1))})
	if _, ok := long["pro"]; ok {
		t.Fatal("an over-long label was kept")
	}
}

// The Codex table belongs to Codex. Claude sells its own "Pro" tier, and
// applying OpenAI's table to it would rename that credential "Pro 200".
func TestTheCodexEnumTableIsNotAppliedToOtherProviders(t *testing.T) {
	for _, provider := range []string{"claude", "xai", "gemini", ""} {
		for _, raw := range []string{"Pro", "Team", "Max", "Plus"} {
			if got := planLabelOf(provider, raw, nil); got != raw {
				t.Errorf("planLabelOf(%q, %q) = %q; another provider's tier must not be renamed", provider, raw, got)
			}
		}
		if got := planLabelOf(provider, "pro", nil); got == "Pro 200" {
			t.Errorf("planLabelOf(%q, \"pro\") took the Codex name", provider)
		}
	}
	// And the other way round: Claude's table must not give Codex's top tier
	// Claude's name for its own.
	if got := planLabelOf("codex", "pro", nil); got != "Pro 200" {
		t.Fatalf("codex pro = %q", got)
	}
}

// quota-cache derives a token for a Claude account from its profile: the
// organization type, and for Max the rate-limit tier that separates the two
// Max plans. Each arrives as the name the plan is sold under.
func TestClaudePlanTokensBecomeTheirPlanNames(t *testing.T) {
	for raw, want := range map[string]string{
		"team":       "Team",
		"enterprise": "Enterprise",
		"max_20x":    "Max 20x",
		"max_5x":     "Max 5x",
		"max":        "Max",
		"pro":        "Pro",
		"free":       "Free",
		"MAX_20X":    "Max 20x",
		// What a snapshot written before the tokens existed carries.
		"Max":  "Max",
		"Team": "Team",
	} {
		if got := planLabelOf("claude", raw, nil); got != want {
			t.Errorf("planLabelOf(claude, %q) = %q; want %q", raw, got, want)
		}
	}
}

// Grok reports its subscription's own display name, which needs no table and
// is never rewritten.
func TestGrokDisplayNamesPassThrough(t *testing.T) {
	for _, raw := range []string{"SuperGrok", "SuperGrok Heavy", "SuperGrok Lite (2026)"} {
		if got := planLabelOf("xai", raw, nil); got != raw {
			t.Errorf("planLabelOf(xai, %q) = %q", raw, got)
		}
	}
}

// Which overrides survive the bound must not change between process starts.
func TestOverrideBoundIsDeterministic(t *testing.T) {
	oversized := map[string]string{}
	for i := 0; i < maxPlanLabels*3; i++ {
		oversized[fmt.Sprintf("key%03d", i)] = fmt.Sprintf("label%03d", i)
	}
	first := NormalizePlanLabels(oversized)
	for i := 0; i < 20; i++ {
		again := NormalizePlanLabels(oversized)
		if len(again) != len(first) {
			t.Fatalf("size drifted: %d vs %d", len(again), len(first))
		}
		for k, v := range first {
			if again[k] != v {
				t.Fatalf("surviving overrides differ between runs at %q", k)
			}
		}
	}
}

// A configured label lands verbatim in a served document.
func TestOverrideLabelsRejectControlAndFormattingCharacters(t *testing.T) {
	hostile := map[string]string{
		"a":                                     "Pro\n</script>",
		"b":                                     "Pro injected",
		"c":                                     "Pro\x00",
		strings.Repeat("k", maxPlanLabelRune+1): "Pro",
	}
	got := NormalizePlanLabels(hostile)
	if len(got) != 0 {
		t.Fatalf("hostile overrides accepted: %+v", got)
	}
	if ok := NormalizePlanLabels(map[string]string{"pro": "Pro 200 (2027)"}); ok["pro"] != "Pro 200 (2027)" {
		t.Fatalf("a legitimate label was rejected: %+v", ok)
	}
}
