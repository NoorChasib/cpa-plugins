package mask

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"testing"
)

func TestRulesetCounts(t *testing.T) {
	if len(gitleaksRules) != 217 {
		t.Errorf("gitleaks rules = %d, want 217", len(gitleaksRules))
	}
	if len(caRules) != 18 {
		t.Errorf("ca rules = %d, want 18", len(caRules))
	}
	rs, err := rulesetCompiled()
	if err != nil {
		t.Fatal(err)
	}
	if len(rs.rules) != 235 {
		t.Errorf("compiled rules = %d, want 235", len(rs.rules))
	}
	for i, r := range rs.rules[:18] {
		if !strings.HasPrefix(r.id, "ca-") || r.order != i {
			t.Errorf("rule %d is %s (order %d); explicit rules must come first", i, r.id, r.order)
		}
	}
	ids := map[string]bool{}
	for _, r := range rs.rules {
		ids[r.id] = true
	}
	for id := range taggedRules {
		if !ids[id] {
			t.Errorf("tagged rule %q does not exist", id)
		}
	}
	for id := range ruleWindows {
		if !ids[id] {
			t.Errorf("window for unknown rule %q", id)
		}
	}
	if rs.rules[17].id != "ca-archive-token" || rs.rules[17].re.String() != `\b(cha_(?:src|enr)_[A-Za-z0-9]{32,})` {
		t.Errorf("ca-archive-token must match only cha_src_ and cha_enr_ tokens, got %s", rs.rules[17].re)
	}
}

// TestAllPatternsCompile compiles every rule and allowlist pattern with the standard library, as
// gitleaks does without the gore2regex tag.
func TestAllPatternsCompile(t *testing.T) {
	n := 0
	for _, sp := range append(append([]ruleSpec(nil), caRules...), gitleaksRules...) {
		if _, err := regexp.Compile(sp.Regex); err != nil {
			t.Errorf("%s: %v", sp.ID, err)
		}
		n++
		for _, a := range sp.Allow {
			for _, p := range a.Regexes {
				if _, err := regexp.Compile(p); err != nil {
					t.Errorf("%s allowlist: %v", sp.ID, err)
				}
			}
		}
		for _, k := range sp.Keywords {
			if k != strings.ToLower(k) || k == "" {
				t.Errorf("%s: keyword %q must be non-empty lowercase", sp.ID, k)
			}
		}
	}
	for _, a := range gitleaksGlobalAllow {
		for _, p := range a.Regexes {
			if _, err := regexp.Compile(p); err != nil {
				t.Errorf("global allowlist: %v", err)
			}
		}
	}
	if n != 235 {
		t.Errorf("compiled %d rule patterns, want 235", n)
	}
}

func TestVendoredTOMLPinned(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "gitleaks-v8.30.1.toml"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	const pinned = "e163e53b9e7e8a8511e77271e2b323ed057759542a6d988258afe3a1fa329caf" // gitleaks v8.30.1 config/gitleaks.toml
	if got := hex.EncodeToString(sum[:]); got != pinned {
		t.Errorf("vendored toml sha256 %s, want %s", got, pinned)
	}
	if gitleaksTOMLSHA256 != pinned {
		t.Errorf("generated header names sha256 %s", gitleaksTOMLSHA256)
	}
	if len(data) != 97731 {
		t.Errorf("vendored toml is %d bytes, want 97731", len(data))
	}
	if gitleaksTag != "v8.30.1" || gitleaksCommit != "83d9cd684c87d95d656c1458ef04895a7f1cbd8e" {
		t.Errorf("generated header pins %s %s", gitleaksTag, gitleaksCommit)
	}
	if _, err := os.Stat("LICENSE-gitleaks"); err != nil {
		t.Error(err)
	}
}

// TestGeneratorReproduces runs scripts/gen_mask_rules.py --check (Python 3.11+ stdlib), which
// regenerates the rules file in memory and compares it byte for byte with the committed one.
func TestGeneratorReproduces(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	cmd := exec.Command(py, "-I", filepath.Join("..", "..", "scripts", "gen_mask_rules.py"), "--check")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gen_mask_rules.py --check: %v\n%s", err, out)
	}
}

func TestRulesetSHA256MatchesFile(t *testing.T) {
	data, err := os.ReadFile("rules_gitleaks_v8_30_1.go")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if got := RulesetSHA256(); got != hex.EncodeToString(sum[:]) {
		t.Errorf("RulesetSHA256 = %s, file sha256 = %x", got, sum)
	}
}

// TestTaggedRulesReviewed checks the reviewed tag list: every tagged gitleaks rule (other than the
// four reviewed by name) meets the vendor-prefix and 80-bit criterion, every gitleaks rule
// that meets it is tagged unless reviewed out, and no forbidden rule is tagged.
func TestTaggedRulesReviewed(t *testing.T) {
	named := map[string]bool{"aws-access-token": true, "private-key": true, "jwt": true, "jwt-base64": true}
	// Reviewed out: planetscale-password is password-like; microsoft-teams-webhook masks its whole
	// match, a URL, and URL rules never carry a tag.
	reviewedOut := map[string]bool{"planetscale-password": true, "microsoft-teams-webhook": true}
	for _, sp := range gitleaksRules {
		_, _, ok := tagEligible(t, sp)
		tagged := taggedRules[sp.ID]
		switch {
		case named[sp.ID]:
			if !tagged {
				t.Errorf("%s is named Tag: yes and must be tagged", sp.ID)
			}
		case tagged && !ok:
			t.Errorf("%s is tagged but fails the vendor-prefix/80-bit criterion", sp.ID)
		case !tagged && ok && !reviewedOut[sp.ID]:
			t.Errorf("%s meets the criterion but is not tagged or reviewed out", sp.ID)
		}
	}
	never := regexp.MustCompile(`^(generic-api-key|curl-auth-.*|ca-auth-header|ca-api-key-header|ca-url-cred|ca-url-userinfo|.*-passw.*)$`)
	for id := range taggedRules {
		if never.MatchString(id) {
			t.Errorf("%s must never carry a tag", id)
		}
	}
	for _, id := range []string{"ca-anthropic", "ca-sk", "ca-stripe", "ca-xai", "ca-google-api-key", "ca-google-oauth", "ca-aws-secret", "ca-github", "ca-gitlab", "ca-hf", "ca-npm", "ca-slack", "ca-jwt", "ca-archive-token"} {
		if !taggedRules[id] {
			t.Errorf("%s is a vendor-token rule and must be tagged", id)
		}
	}
	t.Logf("%d tagged rules", len(taggedRules))
}

func TestTag12(t *testing.T) {
	// Independent vector: Python hashlib.sha256(b"chat-archive/mask/v1/tag\x00ghp_synthetic").hexdigest()[:12].
	if got := tag12("ghp_synthetic"); got != "32f826e18270" {
		t.Errorf("tag12 = %s", got)
	}
}

func TestShannonEntropyMatchesGitleaks(t *testing.T) {
	for _, tc := range []struct {
		s    string
		want float64
	}{
		{"", 0},
		{"aaaa", 0},
		{"ab", 1},
		{"abcd", 2},
	} {
		if got := shannonEntropy(tc.s); got != tc.want {
			t.Errorf("entropy(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
	// gitleaks divides rune counts by the byte length: one rune seen twice in 4 bytes has frequency
	// 0.5, so the entropy is 0.5 (it would be 0 over runes alone).
	if got := shannonEntropy("éé"); got != 0.5 {
		t.Errorf("entropy(éé) = %v, want 0.5 (gitleaks' byte-length divisor)", got)
	}
}

// TestWeakFirstGroupRules finds the rules whose gitleaks secret (SecretGroup, else the first
// non-empty capture group) carries far less randomness than the whole match, so masking only that
// group would leave most of the token readable. Those rules must be exactly wholeMatchRules, which
// mask the whole match instead, and with that no rule's masked span is weak. A regenerated ruleset
// that adds such a rule fails here until the rule joins the list.
func TestWeakFirstGroupRules(t *testing.T) {
	var weak, list []string
	for _, sp := range append(append([]ruleSpec(nil), caRules...), gitleaksRules...) {
		re, err := syntax.Parse(sp.Regex, syntax.Perl)
		if err != nil {
			t.Fatal(err)
		}
		if re.MaxCap() == 0 {
			continue
		}
		if g := minBits(secretNode(t, sp.Regex, sp.SecretGroup)); g < 40 && g < 0.5*minBits(re) {
			weak = append(weak, sp.ID)
		}
		if g := minBits(ruleSecretNode(t, sp)); g < 40 && g < 0.5*minBits(re) {
			t.Errorf("%s: the masked span is still a weak fragment of the match", sp.ID)
		}
	}
	for id := range wholeMatchRules {
		list = append(list, id)
	}
	slices.Sort(list)
	if strings.Join(weak, ",") != "jwt-base64,microsoft-teams-webhook" || !slices.Equal(weak, list) {
		t.Errorf("rules with a weak first group: %v; wholeMatchRules: %v", weak, list)
	}
	rs, err := rulesetCompiled()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs.rules {
		if r.whole != wholeMatchRules[r.id] {
			t.Errorf("%s: compiled whole-match flag %v", r.id, r.whole)
		}
	}
}
