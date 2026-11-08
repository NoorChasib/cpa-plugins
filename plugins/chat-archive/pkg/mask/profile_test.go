package mask

import (
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

// TestProfileRules: one profiled pass adds time under the id of every rule whose keyword occurs
// (in the string or its decoded view), under "(prefilter)" always and under "(exact)" only with
// exact values; it adds nothing else, accumulates across calls, and leaves masking unchanged.
// Values are random and synthetic.
func TestProfileRules(t *testing.T) {
	r := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 7))
	rnd := func(n int) string {
		const alpha = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
		b := make([]byte, n)
		for i := range b {
			b[i] = alpha[r.IntN(len(alpha))]
		}
		b[0] = '7'
		return string(b)
	}
	rs, err := rulesetCompiled()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{profilePrefilter: true, profileExact: true}
	for _, ru := range rs.rules {
		ids[ru.id] = true
	}
	alwaysOn := map[string]bool{profilePrefilter: true}
	for _, ri := range rs.noKW {
		alwaysOn[rs.rules[ri].id] = true
	}

	exact := rnd(32)
	text := "first ghp_" + rnd(36) + " then\\n" + "xai-" + rnd(40) + " and " + exact
	plain, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	withExact, err := New(Config{ExactFPs: [][32]byte{prefixedSum(exactFPPrefix, exact)}})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := withExact.String(text)

	for _, c := range []struct {
		name      string
		m         *Masker
		wantExact bool
	}{{"plain", plain, false}, {"exact", withExact, true}} {
		acc := map[string]time.Duration{}
		c.m.ProfileRules(text, acc)
		for k, d := range acc {
			if !ids[k] {
				t.Errorf("%s: unknown key %q", c.name, k)
			}
			if d < 0 {
				t.Errorf("%s: %s has negative time", c.name, k)
			}
		}
		for _, k := range []string{profilePrefilter, "ca-github", "ca-xai"} {
			if _, ok := acc[k]; !ok {
				t.Errorf("%s: no time for %s", c.name, k)
			}
		}
		if _, ok := acc[profileExact]; ok != c.wantExact {
			t.Errorf("%s: (exact) present = %v", c.name, ok)
		}
		first := map[string]time.Duration{}
		for k, d := range acc {
			first[k] = d
		}
		c.m.ProfileRules(text, acc)
		for k, d := range first {
			if acc[k] < d {
				t.Errorf("%s: %s did not accumulate", c.name, k)
			}
		}
	}
	if after, _ := withExact.String(text); after != before || strings.Contains(after, exact) {
		t.Error("ProfileRules changed masking")
	}

	// A string with no keyword costs only the prefilter and the rules without keywords.
	acc := map[string]time.Duration{}
	plain.ProfileRules(strings.Repeat("plain words only ", 64), acc)
	for k := range acc {
		if !alwaysOn[k] {
			t.Errorf("no keyword: unexpected key %q", k)
		}
	}
}
