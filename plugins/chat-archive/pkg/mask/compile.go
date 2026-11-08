package mask

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	ahocorasick "github.com/BobuSumisu/aho-corasick"
)

// rule is one compiled ruleSpec.
type rule struct {
	id      string
	order   int // position in caRules then gitleaksRules; lower wins ties
	re      *regexp.Regexp
	entropy float64
	group   int
	whole   bool // the secret is the whole match (wholeMatchRules), not a capture group
	allow   []allowlist
	gate    func(string) bool
	tag     bool
	win     window
	line    bool // default line window
}

// allowlist is one compiled allowSpec. The regexes are kept separate; any match allows, which is
// what gitleaks' joined pattern (config/allowlist.go joinRegexOr) computes.
type allowlist struct {
	target string // "", "match" or "line"
	res    []*regexp.Regexp
	stops  *ahocorasick.Trie // nil when there are no stopwords
	and    bool
}

// ruleset is the immutable compiled mask/v1 ruleset, shared by every Masker.
type ruleset struct {
	rules  []rule
	global []allowlist
	trie   *ahocorasick.Trie
	byKW   [][]int // keyword pattern index -> rule indices
	noKW   []int   // rules without keywords run over the whole string
	minKW  int     // shortest keyword length
}

var (
	compiledOnce sync.Once
	compiled     *ruleset
	compileErr   error
)

// rulesetCompiled compiles the 235 rules once per process.
func rulesetCompiled() (*ruleset, error) {
	compiledOnce.Do(func() {
		compiled, compileErr = compileRules(append(append([]ruleSpec(nil), caRules...), gitleaksRules...), gitleaksGlobalAllow)
	})
	return compiled, compileErr
}

func compileRules(specs []ruleSpec, global []allowSpec) (*ruleset, error) {
	rs := &ruleset{minKW: -1}
	var err error
	if rs.global, err = compileAllow(global); err != nil {
		return nil, fmt.Errorf("mask: global allowlist: %w", err)
	}
	kwIndex := map[string]int{}
	var kws []string
	seen := map[string]bool{}
	for i, sp := range specs {
		if seen[sp.ID] {
			return nil, fmt.Errorf("mask: duplicate rule id %q", sp.ID)
		}
		seen[sp.ID] = true
		re, err := regexp.Compile(sp.Regex)
		if err != nil {
			return nil, fmt.Errorf("mask: rule %s: %w", sp.ID, err)
		}
		if sp.SecretGroup > re.NumSubexp() {
			return nil, fmt.Errorf("mask: rule %s: secret group %d > %d groups", sp.ID, sp.SecretGroup, re.NumSubexp())
		}
		al, err := compileAllow(sp.Allow)
		if err != nil {
			return nil, fmt.Errorf("mask: rule %s: %w", sp.ID, err)
		}
		r := rule{
			id: sp.ID, order: i, re: re, entropy: sp.Entropy, group: sp.SecretGroup, whole: wholeMatchRules[sp.ID],
			allow: al, gate: sp.gate, tag: taggedRules[sp.ID],
		}
		if w, ok := ruleWindows[sp.ID]; ok {
			r.win = w
		} else {
			r.line = true
			r.win = window{before: defaultLineRadius, after: defaultLineRadius}
		}
		rs.rules = append(rs.rules, r)
		if len(sp.Keywords) == 0 {
			rs.noKW = append(rs.noKW, i)
			continue
		}
		for _, k := range sp.Keywords {
			k = strings.ToLower(k)
			idx, ok := kwIndex[k]
			if !ok {
				idx = len(kws)
				kwIndex[k] = idx
				kws = append(kws, k)
				rs.byKW = append(rs.byKW, nil)
				if rs.minKW < 0 || len(k) < rs.minKW {
					rs.minKW = len(k)
				}
			}
			if l := rs.byKW[idx]; len(l) == 0 || l[len(l)-1] != i {
				rs.byKW[idx] = append(l, i)
			}
		}
	}
	rs.trie = ahocorasick.NewTrieBuilder().AddStrings(kws).Build()
	if rs.minKW < 1 {
		rs.minKW = 1
	}
	return rs, nil
}

func compileAllow(specs []allowSpec) ([]allowlist, error) {
	var out []allowlist
	for _, a := range specs {
		al := allowlist{target: a.RegexTarget}
		switch a.RegexTarget {
		case "", "match", "line":
		default:
			return nil, fmt.Errorf("unknown regex target %q", a.RegexTarget)
		}
		switch strings.ToUpper(a.Condition) {
		case "", "OR":
		case "AND":
			al.and = true
		default:
			return nil, fmt.Errorf("unknown allowlist condition %q", a.Condition)
		}
		for _, p := range a.Regexes {
			re, err := regexp.Compile(p)
			if err != nil {
				return nil, fmt.Errorf("allowlist regex: %w", err)
			}
			al.res = append(al.res, re)
		}
		if len(a.StopWords) > 0 {
			words := make([]string, 0, len(a.StopWords))
			dedup := map[string]bool{}
			for _, w := range a.StopWords {
				w = strings.ToLower(w)
				if !dedup[w] {
					dedup[w] = true
					words = append(words, w)
				}
			}
			al.stops = ahocorasick.NewTrieBuilder().AddStrings(words).Build()
		}
		if len(al.res) == 0 && al.stops == nil {
			return nil, fmt.Errorf("empty allowlist")
		}
		out = append(out, al)
	}
	return out, nil
}
