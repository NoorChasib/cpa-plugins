package mask

import (
	"crypto/sha256"
	"encoding/hex"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestIdempotenceProperty: for random documents built from synthesized positives of all 235 rules,
// registered exact values, placeholders and separators, Mask(Mask(x)) == Mask(x), and the server's
// detect-mode re-check of masked content finds nothing.
func TestIdempotenceProperty(t *testing.T) {
	var frags []string
	for _, sp := range append(append([]ruleSpec(nil), caRules...), gitleaksRules...) {
		if text, _, ok := synthPositive(t, sp); ok {
			frags = append(frags, strings.TrimSuffix(strings.TrimPrefix(text, "synthetic line before\n"), "\nsynthetic line after"))
		}
	}
	exactVals := []string{"Q7v0mZr3Lk2Pw9Xs4Nd8Hc1Bt6Yg5Fa0", "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00", "opsValue_With-Dashes.and~tilde=="}
	cfg := Config{}
	for i, v := range exactVals {
		if i%2 == 0 {
			cfg.ExactFPs = append(cfg.ExactFPs, prefixedSum(exactFPPrefix, v))
		} else {
			cfg.CallerFPs = append(cfg.CallerFPs, prefixedSum(callerScopePrefix, v))
		}
	}
	frags = append(frags, exactVals...)
	frags = append(frags, "[masked:ca-github:3f9a1c2b7e4d]", "[masked:exact:0123456789ab]", "[masked:generic-api-key]")
	// Back-to-back fixed-length tokens need one pass each.
	frags = append(frags, strings.Repeat("npm_Q7v0mZr3Lk2Pw9Xs4Nd8Hc1Bt6Yg5Fa0AbCd", 10))
	seps := []string{"", " ", "\n", "=", "/", ":", "@", `"`, "'", "?key=", "&token=", "://user:", "https://h/", "NAME=", "--key=", "password = ", "Authorization: Bearer ", ";", "\\n",
		"\\t", "%3D", "%20", "%2F", "\\u0026token=", "\\/", "+", "."}
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	seed := uint64(time.Now().UnixNano())
	r := rand.New(rand.NewPCG(seed, 1))
	docs := 1500 // MASK_PROPERTY_DOCS raises it for a longer soak
	if n, err := strconv.Atoi(os.Getenv("MASK_PROPERTY_DOCS")); err == nil && n > 0 {
		docs = n
	}
	failures := 0
	for i := 0; i < docs && failures < 5; i++ {
		var b strings.Builder
		for k := 1 + r.IntN(6); k > 0; k-- {
			b.WriteString(seps[r.IntN(len(seps))])
			b.WriteString(frags[r.IntN(len(frags))])
		}
		x := b.String()
		once, _ := m.String(x)
		twice, _ := m.String(once)
		if once != twice {
			failures++
			t.Errorf("seed %d doc %d: Mask(Mask(x)) != Mask(x); input sha256 %s", seed, i, shortHash(x))
			if os.Getenv("MASK_PROPERTY_DEBUG") == "1" { // synthetic values only
				t.Logf("in    %q\nonce  %q\ntwice %q", x, once, twice)
			}
		}
		if f := m.Detect(once); len(f) != 0 {
			failures++
			t.Errorf("seed %d doc %d: Detect(Mask(x)) = %v", seed, i, f)
		}
	}
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:6])
}
