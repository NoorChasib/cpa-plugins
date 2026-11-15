package mask

import (
	"strings"
	"testing"
)

// TestDetectionImpliesRedaction: for a synthesized positive of every rule, in the forms that can make a
// detector and a masker disagree (adjacent and overlapping matches, JSON and percent escapes, escaped
// quotes, token runs far longer than a scan window, and the placeholder next to its own neighbour), the
// server's Detect of the masked string finds nothing and masking is idempotent. A string the re-check
// flags but String leaves as is would make its record a permanent mask_violation dead letter.
func TestDetectionImpliesRedaction(t *testing.T) {
	var frags []string
	var ids []string
	for _, sp := range append(append([]ruleSpec(nil), caRules...), gitleaksRules...) {
		if text, _, ok := synthPositive(t, sp); ok {
			frags = append(frags, strings.TrimSuffix(strings.TrimPrefix(text, "synthetic line before\n"), "\nsynthetic line after"))
			ids = append(ids, sp.ID)
		}
	}
	exact := "Q7v0mZr3Lk2Pw9Xs4Nd8Hc1Bt6Yg5Fa0"
	m, err := New(Config{ExactFPs: [][32]byte{prefixedSum(exactFPPrefix, exact)}})
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("A1b2C3d4", (maxGrow+4096)/8) // longer than maxGrow, so windows stop growing inside it
	longForms := map[string]bool{"ca-xai": true, "ca-sk": true, "ca-github": true, "ca-aws-secret": true, "generic-api-key": true, "jwt": true}
	check := func(id, form, x string) {
		t.Helper()
		once, _ := m.String(x)
		if f := m.Detect(once); len(f) != 0 {
			t.Errorf("%s, %s: Detect(Mask(x)) finds %d (%s)", id, form, len(f), f[0].RuleID)
		}
		if twice, _ := m.String(once); twice != once {
			t.Errorf("%s, %s: Mask(Mask(x)) != Mask(x)", id, form)
		}
	}
	for i, p := range frags {
		id, next := ids[i], frags[(i+1)%len(frags)]
		check(id, "adjacent twice", p+p)
		check(id, "adjacent to the next rule", p+next)
		check(id, "overlapping the next rule", p[:len(p)-min(4, len(p))]+next)
		check(id, "next to an exact value", exact+p+exact)
		check(id, "json escapes", strings.NewReplacer("-", `\u002d`, "_", `\u005f`, `"`, `\"`, "/", `\/`).Replace(p))
		check(id, "escaped quotes", `{\"value\": \"`+p+`\"}`)
		check(id, "percent escapes", strings.NewReplacer("-", "%2D", "_", "%5F", "=", "%3D", ":", "%3A").Replace(p))
		check(id, "placeholder neighbour", "[masked:"+id+"]"+p+"[masked:exact:0123456789ab]")
		// The window growth is per engine, not per rule, and each long form costs a scan of 68 KiB.
		if longForms[id] {
			check(id, "long token run after", p+long)
			check(id, "long token run before", long+p)
		}
	}
}
