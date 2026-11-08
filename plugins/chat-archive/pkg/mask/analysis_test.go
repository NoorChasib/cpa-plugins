package mask

import (
	"math"
	"os"
	"regexp/syntax"
	"sort"
	"testing"
)

// secretNode returns the regexp/syntax node of a rule's secret as gitleaks defines it: SecretGroup,
// else capture group 1, else the whole pattern. A negative group returns the whole pattern.
func secretNode(t testing.TB, pattern string, group int) *syntax.Regexp {
	t.Helper()
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		t.Fatal(err)
	}
	if group == 0 {
		group = 1
	}
	var found *syntax.Regexp
	var walk func(n *syntax.Regexp)
	walk = func(n *syntax.Regexp) {
		if found != nil {
			return
		}
		if n.Op == syntax.OpCapture && n.Cap == group {
			found = n
			return
		}
		for _, s := range n.Sub {
			walk(s)
		}
	}
	walk(re)
	if found == nil {
		return re
	}
	return found
}

// ruleSecretNode returns the node of what the engine masks for sp: the whole pattern for
// wholeMatchRules, else secretNode.
func ruleSecretNode(t testing.TB, sp ruleSpec) *syntax.Regexp {
	t.Helper()
	if wholeMatchRules[sp.ID] {
		return secretNode(t, sp.Regex, -1)
	}
	return secretNode(t, sp.Regex, sp.SecretGroup)
}

// minBits is a lower bound on the randomness of any string the node matches: literals and
// zero-width assertions add nothing, a class adds log2(size) per required repetition, an
// alternation takes its weakest branch. Any-char counts as nothing (unknown).
func minBits(n *syntax.Regexp) float64 {
	switch n.Op {
	case syntax.OpCharClass:
		var size float64
		for i := 0; i+1 < len(n.Rune); i += 2 {
			size += float64(n.Rune[i+1]-n.Rune[i]) + 1
		}
		if size <= 1 {
			return 0
		}
		return math.Log2(size)
	case syntax.OpCapture, syntax.OpPlus:
		return minBits(n.Sub[0])
	case syntax.OpRepeat:
		return float64(n.Min) * minBits(n.Sub[0])
	case syntax.OpConcat:
		var b float64
		for _, s := range n.Sub {
			b += minBits(s)
		}
		return b
	case syntax.OpAlternate:
		b := math.Inf(1)
		for _, s := range n.Sub {
			b = math.Min(b, minBits(s))
		}
		return b
	}
	return 0
}

// literalPrefix is the number of fixed characters every match of n starts with; full reports
// whether n is entirely fixed.
func literalPrefix(n *syntax.Regexp) (int, bool) {
	switch n.Op {
	case syntax.OpLiteral:
		return len(n.Rune), true
	case syntax.OpEmptyMatch, syntax.OpBeginLine, syntax.OpEndLine, syntax.OpBeginText, syntax.OpEndText,
		syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		return 0, true
	case syntax.OpCharClass:
		if len(n.Rune) == 2 && n.Rune[0] == n.Rune[1] {
			return 1, true
		}
		return 0, false
	case syntax.OpCapture:
		return literalPrefix(n.Sub[0])
	case syntax.OpRepeat:
		p, full := literalPrefix(n.Sub[0])
		if full && n.Min == n.Max {
			return p * n.Min, true
		}
		if n.Min >= 1 {
			return p, false
		}
		return 0, false
	case syntax.OpPlus:
		p, _ := literalPrefix(n.Sub[0])
		return p, false
	case syntax.OpConcat:
		total := 0
		for _, s := range n.Sub {
			p, full := literalPrefix(s)
			total += p
			if !full {
				return total, false
			}
		}
		return total, true
	case syntax.OpAlternate:
		minP, allFull := -1, true
		for _, s := range n.Sub {
			p, full := literalPrefix(s)
			if minP < 0 || p < minP {
				minP = p
			}
			allFull = allFull && full
		}
		return minP, allFull
	}
	return 0, false
}

// The criterion for a gitleaks rule on the tag list: a vendor prefix of at least 2 fixed characters
// at the start of the secret, and at least 80 bits of randomness in the secret.
const (
	tagMinPrefix = 2
	tagMinBits   = 80
)

func tagEligible(t testing.TB, sp ruleSpec) (prefix int, bits float64, ok bool) {
	n := ruleSecretNode(t, sp)
	prefix, _ = literalPrefix(n)
	bits = minBits(n)
	return prefix, bits, prefix >= tagMinPrefix && bits >= tagMinBits
}

// TestTagReport prints every gitleaks rule's prefix and bits when MASK_TAG_REPORT=1 (review aid).
func TestTagReport(t *testing.T) {
	if os.Getenv("MASK_TAG_REPORT") != "1" {
		t.Skip("set MASK_TAG_REPORT=1")
	}
	type row struct {
		id     string
		prefix int
		bits   float64
		ok     bool
	}
	var rows []row
	for _, sp := range gitleaksRules {
		p, b, ok := tagEligible(t, sp)
		rows = append(rows, row{sp.ID, p, b, ok})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].id < rows[j].id })
	for _, r := range rows {
		t.Logf("%-5v prefix=%-3d bits=%6.1f %s", r.ok, r.prefix, r.bits, r.id)
	}
}
