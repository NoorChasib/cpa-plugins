package aggregate

import (
	"fmt"
	"strings"
	"testing"
)

// Codex reports an enum token, not the name the tier is sold under. The enum
// separates prolite from pro, which is the Pro 5x / Pro 20x split, so the
// display name is derived rather than invented. Confirmed against a live
// account: a Pro 20x subscription reports chatgpt_plan_type "pro".
func TestCodexEnumTokensBecomeTheNamesTheTiersAreSoldUnder(t *testing.T) {
	for raw, want := range map[string]string{
		"pro":     "Pro 20x",
		"prolite": "Pro 5x",
		"plus":    "Plus",
		"team":    "Team",
		"free":    "Free",
		"PRO":     "Pro 20x", // matched case-insensitively
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
		"self_serve_business_prolite": "Self Serve Business Prolite",
		"ent26":                       "Ent26",
		"edu_plus":                    "Edu Plus",
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
		"  PRO  ": " Pro 20x (2027) ",
		"max":     "Max 20x",
		"":        "ignored",
		"blank":   "   ",
	})
	if got := planLabelOf("codex", "pro", overrides); got != "Pro 20x (2027)" {
		t.Fatalf("override ignored: %q", got)
	}
	// Overrides also reach values that would otherwise pass through untouched.
	if got := planLabelOf("codex", "Max", overrides); got != "Max 20x" {
		t.Fatalf("override on a presentable name: %q", got)
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

// The enum table belongs to Codex. Claude sells its own "Pro" tier, and
// applying OpenAI's table to it would rename that credential "Pro 20x".
func TestTheCodexEnumTableIsNotAppliedToOtherProviders(t *testing.T) {
	for _, provider := range []string{"claude", "xai", "gemini", ""} {
		for _, raw := range []string{"Pro", "Team", "Max", "Plus"} {
			if got := planLabelOf(provider, raw, nil); got != raw {
				t.Errorf("planLabelOf(%q, %q) = %q; another provider's tier must not be renamed", provider, raw, got)
			}
		}
	}
	if got := planLabelOf("codex", "pro", nil); got != "Pro 20x" {
		t.Fatalf("codex pro = %q", got)
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
	if ok := NormalizePlanLabels(map[string]string{"pro": "Pro 20x (2027)"}); ok["pro"] != "Pro 20x (2027)" {
		t.Fatalf("a legitimate label was rejected: %+v", ok)
	}
}
