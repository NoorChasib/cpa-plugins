package mask

import (
	"hash/fnv"
	"math/rand/v2"
	"regexp/syntax"
	"strings"
	"testing"
	"unicode"
)

// synth builds strings from a regexp/syntax tree with random class members. Nothing here is a real secret.
type synth struct {
	r *rand.Rand
	b strings.Builder
}

func (g *synth) gen(n *syntax.Regexp) {
	switch n.Op {
	case syntax.OpLiteral:
		for _, c := range n.Rune {
			g.b.WriteRune(c)
		}
	case syntax.OpCharClass:
		g.b.WriteRune(g.classMember(n.Rune))
	case syntax.OpAnyCharNotNL, syntax.OpAnyChar:
		g.b.WriteByte('.') // patterns often write a literal dot as "."
	case syntax.OpCapture:
		g.gen(n.Sub[0])
	case syntax.OpStar:
		for k := g.r.IntN(2); k > 0; k-- {
			g.gen(n.Sub[0])
		}
	case syntax.OpPlus:
		for k := 1 + g.r.IntN(6); k > 0; k-- {
			g.gen(n.Sub[0])
		}
	case syntax.OpQuest:
		if g.r.IntN(2) == 0 {
			g.gen(n.Sub[0])
		}
	case syntax.OpRepeat:
		max := n.Max
		if max < 0 || max > n.Min+12 {
			max = n.Min + 12
		}
		for k := n.Min + g.r.IntN(max-n.Min+1); k > 0; k-- {
			g.gen(n.Sub[0])
		}
	case syntax.OpConcat:
		for _, s := range n.Sub {
			g.gen(s)
		}
	case syntax.OpAlternate:
		g.gen(n.Sub[g.r.IntN(len(n.Sub))])
	}
	// Zero-width assertions and empty matches emit nothing; the caller's context supplies them.
}

// classMember picks a member of a class, preferring ASCII letters and digits, then other printable
// ASCII, then whitespace other than newlines, then anything.
func (g *synth) classMember(ranges []rune) rune {
	var alnum, printable, space []rune
	for i := 0; i+1 < len(ranges); i += 2 {
		for c := ranges[i]; c <= ranges[i+1] && c < 128; c++ {
			switch {
			case unicode.IsLetter(c) || unicode.IsDigit(c):
				alnum = append(alnum, c)
			case c > ' ' && c < 127:
				printable = append(printable, c)
			case c == ' ' || c == '\t':
				space = append(space, c)
			}
		}
	}
	switch {
	case len(alnum) > 0 && (len(printable) == 0 || g.r.IntN(10) > 0):
		return alnum[g.r.IntN(len(alnum))]
	case len(printable) > 0:
		return printable[g.r.IntN(len(printable))]
	case len(space) > 0:
		return space[g.r.IntN(len(space))]
	}
	return ranges[0]
}

func hasKeyword(s string, kws []string) bool {
	l := strings.ToLower(s)
	for _, k := range kws {
		if strings.Contains(l, k) {
			return true
		}
	}
	return len(kws) == 0
}

func seedFor(id string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(id))
	return h.Sum64()
}

// singleRule returns a Masker that runs only sp, with the global allowlist, and no exact values.
func singleRule(t testing.TB, sp ruleSpec) *Masker {
	t.Helper()
	rs, err := compileRules([]ruleSpec{sp}, gitleaksGlobalAllow)
	if err != nil {
		t.Fatal(err)
	}
	return &Masker{rs: rs}
}

// synthPositive returns a text holding one synthesized positive for sp and the secret span the rule
// reports in it, trying up to 2000 seeds.
func synthPositive(t testing.TB, sp ruleSpec) (text, secret string, ok bool) {
	re, err := syntax.Parse(sp.Regex, syntax.Perl)
	if err != nil {
		t.Fatal(err)
	}
	one := singleRule(t, sp)
	for attempt := uint64(0); attempt < 2000; attempt++ {
		g := &synth{r: rand.New(rand.NewPCG(seedFor(sp.ID), attempt))}
		g.gen(re)
		line := g.b.String()
		if !hasKeyword(line, sp.Keywords) {
			line = sp.Keywords[0] + " " + line // the keyword is context outside the pattern
		}
		text = "synthetic line before\n" + line + "\nsynthetic line after"
		spans := one.scan(text)
		if len(spans) > 0 {
			return text, text[spans[0].start:spans[0].end], true
		}
	}
	return "", "", false
}

// TestEveryGitleaksRuleSynthesized: one positive synthesized per gitleaks rule from its
// regexp/syntax tree, masked by the full ruleset (the secret appears nowhere in the output, and a
// placeholder does).
func TestEveryGitleaksRuleSynthesized(t *testing.T) {
	m, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, sp := range gitleaksRules {
		text, secret, ok := synthPositive(t, sp)
		if !ok {
			t.Errorf("%s: no synthesized positive found", sp.ID)
			continue
		}
		out, hits := m.String(text)
		if strings.Contains(out, secret) {
			t.Errorf("%s: secret (%d bytes) survives masking; hits %v", sp.ID, len(secret), hits)
		}
		if !strings.Contains(out, "[masked:") || len(hits) == 0 {
			t.Errorf("%s: no placeholder in output", sp.ID)
		}
		if again, _ := m.String(out); again != out {
			t.Errorf("%s: not idempotent", sp.ID)
		}
	}
}

// allPlaceholders returns every placeholder form mask/v1 can emit: each rule id with and without a
// tag, and exact.
func allPlaceholders(t testing.TB) []string {
	rs, err := rulesetCompiled()
	if err != nil {
		t.Fatal(err)
	}
	ph := []string{"[masked:exact:0123456789ab]", "[masked:exact:fedcba987654]"}
	for _, r := range rs.rules {
		ph = append(ph, "[masked:"+r.id+"]", "[masked:"+r.id+":"+tag12(r.id)+"]", "[masked:"+r.id+":9f86d081884c]")
	}
	return ph
}

// placeholderContexts embed a placeholder where a secret usually sits.
var placeholderContexts = []string{
	"%s",
	"value %s end",
	`password = "%s"`,
	"api_key=%s",
	"token: %s",
	`"secret": "%s",`,
	"Authorization: Bearer %s",
	"x-api-key: %s",
	"https://api.example.invalid/v1?key=%s&x=1",
	"https://api.example.invalid/v1?sig=%s",
	"postgres://user:%s@db.example.invalid/x",
	"https://%s@gems.contribsys.com/",
	"postgres://%s@db.example.invalid/x",
	"postgres://%s:%s@db.example.invalid/x",
	"aws_secret_access_key=%s",
	"export GITHUB_TOKEN=%s",
	"curl -H 'Authorization: Bearer %s' https://example.invalid",
	`curl -u "user:%s" https://example.invalid`,
	"NAME=%s",
	"--key=%s",
	"https://h.example.invalid/%s",
	"/path/%s/x",
	"%s%s",
	"%s %s",
}

// TestPlaceholdersMatchNoRule: no compiled rule pattern matches a bare placeholder, and in every
// context Detect reports nothing and String leaves the text unchanged.
func TestPlaceholdersMatchNoRule(t *testing.T) {
	rs, err := rulesetCompiled()
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	ph := allPlaceholders(t)
	for _, p := range ph {
		for _, r := range rs.rules {
			if r.re.MatchString(p) {
				t.Errorf("rule %s matches placeholder %s", r.id, p)
			}
		}
		if !isPlaceholder(p) {
			t.Errorf("isPlaceholder(%s) = false", p)
		}
	}
	for _, p := range ph {
		for _, c := range placeholderContexts {
			text := strings.ReplaceAll(c, "%s", p)
			if f := m.Detect(text); len(f) != 0 {
				t.Errorf("Detect(%q) = %v, want clean", text, f)
			}
			if out, hits := m.String(text); out != text || hits != nil {
				t.Errorf("String(%q) = %q %v, want unchanged", text, out, hits)
			}
		}
	}
}
