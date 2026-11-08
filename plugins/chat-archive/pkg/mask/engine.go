package mask

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
)

// span is one accepted replacement: s[start:end] becomes the placeholder for rule (or exact).
type span struct {
	start, end int
	rule       int    // index into ruleset.rules, or -1 for an exact value
	fp12       string // exact values: first 12 hex of the matching fingerprint
	tagText    string // the secret tag12 is computed over when it differs from s[start:end] (decoded view)
	prio       int    // rule spans: position in the resolve order; on a merge the lowest prio labels the union
}

// scanWin is a merged scan window of one rule.
type scanWin struct {
	rule       int
	start, end int
}

// maxGrow bounds how far a scan window is extended past its original edges, so a secret longer than
// the window is never cut (a window edge inside a token run, or a match ending at a window edge).
const maxGrow = 64 << 10

// ctxCheckEvery is how many windows, or candidates within one window, run between two context
// checks.
const ctxCheckEvery = 64

// maxMerged stops merging overlapping windows of one rule once the merged window is this long, so one
// regex call never spans a whole dense text and a context deadline is checked often. Overlapping
// unmerged windows may report one match twice; resolve merges duplicates.
const maxMerged = 256 << 10

// scan returns the non-overlapping spans String replaces, sorted by start: prefilter, windows, regex inside windows, gitleaks checks, explicit gates, overlap
// resolution, then exact-value candidates. The rules and exact runs see s as written and, when s
// holds JSON string escapes or %XX triplets, a decoded view of s too; a match in the view is mapped
// back to the smallest byte range of s that covers it.
func (m *Masker) scan(s string) []span {
	sp, _ := m.scanCtx(context.Background(), s, nil)
	return sp
}

func (m *Masker) scanCtx(ctx context.Context, s string, lower *[]byte) ([]span, error) {
	rs := m.rs
	var cands []span
	if len(s) >= rs.minKW {
		c, err := rs.ruleCandidates(ctx, s, lower)
		if err != nil {
			return nil, err
		}
		cands = c
	}
	v := decodeView(s)
	if v != nil && len(v.text) >= rs.minKW {
		vc, err := rs.ruleCandidates(ctx, v.text, lower)
		if err != nil {
			return nil, err
		}
		for _, c := range vc {
			secret := v.text[c.start:c.end]
			c.start, c.end = v.rawStart(c.start), v.rawEnd(c.end)
			if s[c.start:c.end] != secret {
				c.tagText = secret
			}
			cands = append(cands, c)
		}
	}
	accepted := resolve(rs, cands)
	if m.exactEnabled() {
		if len(s) >= exactMinLen {
			accepted = m.addExact(s, accepted)
		}
		if v != nil && len(v.text) >= exactMinLen {
			accepted = m.addExactMapped(v.text, accepted, v)
		}
	}
	return accepted, nil
}

// ruleCandidates runs every rule whose keyword occurs, inside its windows.
func (rs *ruleset) ruleCandidates(ctx context.Context, s string, lower *[]byte) ([]span, error) {
	return rs.candidatesCtx(ctx, newText(s), rs.windowsInto(s, lower))
}

// windows returns the merged scan windows of every rule with a keyword hit, sorted by rule then start.
func (rs *ruleset) windows(s string) []scanWin {
	return rs.windowsInto(s, nil)
}

// windowsInto is windows with a reusable buffer for the lowercase copy (nil allocates one).
func (rs *ruleset) windowsInto(s string, buf *[]byte) []scanWin {
	var lower []byte
	if buf != nil {
		lower = asciiLowerInto((*buf)[:0], s)
		*buf = lower
	} else {
		lower = asciiLowerInto(nil, s)
	}
	var wins []scanWin
	rs.trie.Walk(lower, func(end, n, pattern int64) bool {
		hs, he := int(end-n+1), int(end+1)
		for _, ri := range rs.byKW[pattern] {
			r := &rs.rules[ri]
			ws, we := hs-r.win.before, he+r.win.after
			if ws < 0 {
				ws = 0
			}
			if we > len(s) {
				we = len(s)
			}
			if r.line {
				if i := strings.LastIndexByte(s[ws:hs], '\n'); i >= 0 {
					ws += i + 1
				}
				if i := strings.IndexByte(s[he:we], '\n'); i >= 0 {
					we = he + i
				}
			}
			// Never cut a token run: a secret longer than the radius would be truncated (and a "$" in
			// the pattern would accept the cut). A run holds no newline, so line windows stay on
			// their line.
			ws, we = extendLeft(s, ws, ws-maxGrow), extendRight(s, we, we+maxGrow)
			wins = append(wins, scanWin{rule: ri, start: ws, end: we})
		}
		return true
	})
	for _, ri := range rs.noKW {
		wins = append(wins, scanWin{rule: ri, start: 0, end: len(s)})
	}
	if len(wins) == 0 {
		return nil
	}
	sort.Slice(wins, func(i, j int) bool {
		if wins[i].rule != wins[j].rule {
			return wins[i].rule < wins[j].rule
		}
		return wins[i].start < wins[j].start
	})
	// Merge overlapping windows of the same rule.
	merged := wins[:1]
	for _, w := range wins[1:] {
		last := &merged[len(merged)-1]
		if w.rule == last.rule && w.start <= last.end && last.end-last.start < maxMerged {
			if w.end > last.end {
				last.end = w.end
			}
			continue
		}
		merged = append(merged, w)
	}
	return merged
}

// tokenByte is the token-run class windows never cut: [A-Za-z0-9._~+/=%-].
var tokenByte = func() (t [256]bool) {
	for c := 'a'; c <= 'z'; c++ {
		t[c] = true
	}
	for c := 'A'; c <= 'Z'; c++ {
		t[c] = true
	}
	for c := '0'; c <= '9'; c++ {
		t[c] = true
	}
	for _, c := range "._~+/=%-" {
		t[c] = true
	}
	return t
}()

// extendRight moves a window end that falls inside a token run to the run's end, at most to lim.
func extendRight(s string, we, lim int) int {
	lim = min(lim, len(s))
	for we > 0 && we < lim && tokenByte[s[we-1]] && tokenByte[s[we]] {
		we++
	}
	return we
}

// extendLeft moves a window start that falls inside a token run to the run's start, at least to lim.
func extendLeft(s string, ws, lim int) int {
	lim = max(lim, 0)
	for ws < len(s) && ws > lim && tokenByte[s[ws-1]] && tokenByte[s[ws]] {
		ws--
	}
	return ws
}

// candidates runs each window's rule over the window and applies check to every match.
func (rs *ruleset) candidates(s string, merged []scanWin) []span {
	c, _ := rs.candidatesCtx(context.Background(), newText(s), merged)
	return c
}

func (rs *ruleset) candidatesCtx(ctx context.Context, t *text, merged []scanWin) ([]span, error) {
	var cands []span
	for wi, w := range merged {
		if wi%ctxCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		r := &rs.rules[w.rule]
		ws, ms := rs.grownMatches(t.s, r, w)
		for mj, mi := range ms {
			if mj%ctxCheckEvery == ctxCheckEvery-1 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if sp, ok := rs.check(t, r, w.rule, ws+mi[0], ws+mi[1]); ok {
				cands = append(cands, sp)
			}
		}
	}
	return cands, nil
}

// grownMatches runs r over window w and, while a match touches a window edge that is not an edge of
// s (or, for line windows, a line edge), doubles the window on that side and runs it again, up to
// maxGrow past the original edges. Regex anchors see window edges as text edges, so without this a
// long secret is cut at the edge and the cut accepted.
func (rs *ruleset) grownMatches(s string, r *rule, w scanWin) (int, [][]int) {
	ws, we := w.start, w.end
	for {
		ms := r.re.FindAllStringIndex(s[ws:we], -1)
		left, right := false, false
		for _, mi := range ms {
			if mi[0] == 0 && ws > 0 && !(r.line && s[ws-1] == '\n') {
				left = true
			}
			if mi[1] == we-ws && we < len(s) && !(r.line && s[we] == '\n') {
				right = true
			}
		}
		if !left && !right {
			return ws, ms
		}
		ext := max(we-ws, 1024)
		nws, nwe := ws, we
		if left {
			nws = max(ws-ext, w.start-maxGrow, 0)
			if r.line {
				if i := strings.LastIndexByte(s[nws:ws], '\n'); i >= 0 {
					nws += i + 1
				}
			}
		}
		if right {
			nwe = min(we+ext, w.end+maxGrow, len(s))
			if r.line {
				if i := strings.IndexByte(s[we:nwe], '\n'); i >= 0 {
					nwe = we + i
				}
			}
		}
		if nws == ws && nwe == we {
			return ws, ms
		}
		ws, we = nws, nwe
	}
}

// text is one scanned string with what its checks share: the placeholders in it, its newline index
// (built on first use) and the verdicts of line-target allowlists per line, so a long single-line
// string costs one allowlist evaluation per line, not one per candidate.
type text struct {
	s      string
	phs    [][]int
	nl     []int
	nlDone bool
	memo   map[lineKey]bool
}

type lineKey struct {
	al     *allowlist
	ls, le int
}

func newText(s string) *text {
	return &text{s: s, phs: placeholderSpans(s)}
}

// line returns the bounds of the line holding s[a:b]: after the last '\n' before a, up to the first
// '\n' at or after b.
func (t *text) line(a, b int) (int, int) {
	if !t.nlDone {
		t.nlDone = true
		for i := 0; ; {
			j := strings.IndexByte(t.s[i:], '\n')
			if j < 0 {
				break
			}
			t.nl = append(t.nl, i+j)
			i += j + 1
		}
	}
	ls := 0
	if k := sort.SearchInts(t.nl, a); k > 0 {
		ls = t.nl[k-1] + 1
	}
	le := len(t.s)
	if k := sort.SearchInts(t.nl, b); k < len(t.nl) {
		le = t.nl[k]
	}
	return ls, le
}

// check applies gitleaks' per-match semantics (detect/detect.go:520-562 at v8.30.1) to the match
// s[a:b], then the explicit gate, and returns the secret's span.
func (rs *ruleset) check(t *text, r *rule, ri, a, b int) (span, bool) {
	s := t.s
	// The match is trimmed of newlines; the regex is re-run on the trimmed match to find the groups.
	for a < b && s[a] == '\n' {
		a++
	}
	for b > a && s[b-1] == '\n' {
		b--
	}
	if a == b {
		return span{}, false
	}
	match := s[a:b]
	// The secret is the secret group, else the first non-empty capture group, else the whole match.
	// For wholeMatchRules it is always the whole match.
	ss, se := a, b
	if g := r.groups(match); len(g) >= 4 {
		if r.group > 0 {
			if g[2*r.group] < 0 {
				return span{}, false // empty secret: nothing to replace
			}
			ss, se = a+g[2*r.group], a+g[2*r.group+1]
		} else {
			for k := 1; 2*k+1 < len(g); k++ {
				if g[2*k] >= 0 && g[2*k+1] > g[2*k] {
					ss, se = a+g[2*k], a+g[2*k+1]
					break
				}
			}
		}
	}
	if ss == se {
		return span{}, false
	}
	secret := s[ss:se]
	if r.entropy != 0 && shannonEntropy(secret) <= r.entropy {
		return span{}, false
	}
	if t.allowed(rs.global, secret, match, a, b) || t.allowed(r.allow, secret, match, a, b) {
		return span{}, false
	}
	if r.gate != nil && !r.gate(secret) {
		return span{}, false
	}
	if insidePlaceholders(t.phs, ss, se) {
		return span{}, false // placeholders are clean
	}
	return span{start: ss, end: se, rule: ri}, true
}

// groups returns the submatch indexes of the rule's regex in match, or nil for a whole-match rule
// (whose secret is all of match, so the second regex run is skipped).
func (r *rule) groups(match string) []int {
	if r.whole {
		return nil
	}
	return r.re.FindStringSubmatchIndex(match)
}

// allowed is gitleaks' checkFindingAllowed for the finding-level checks (regexes and stopwords). The
// match is s[a:b] of t.
func (t *text) allowed(lists []allowlist, secret, match string, a, b int) bool {
	for i := range lists {
		al := &lists[i]
		regexOK := false
		switch al.target {
		case "line":
			ls, le := t.line(a, b)
			k := lineKey{al, ls, le}
			v, ok := t.memo[k]
			if !ok {
				v = le > ls && matchAny(al.res, t.s[ls:le])
				if t.memo == nil {
					t.memo = map[lineKey]bool{}
				}
				t.memo[k] = v
			}
			regexOK = v
		case "match":
			regexOK = match != "" && matchAny(al.res, match)
		default:
			regexOK = secret != "" && matchAny(al.res, secret)
		}
		stopOK := al.stops != nil && secret != "" && al.stops.MatchFirst(asciiLowerUnicode(secret)) != nil
		if al.and {
			ok := true
			if len(al.res) > 0 {
				ok = ok && regexOK
			}
			if al.stops != nil {
				ok = ok && stopOK
			}
			if ok {
				return true
			}
		} else if regexOK || stopOK {
			return true
		}
	}
	return false
}

func matchAny(res []*regexp.Regexp, target string) bool {
	for _, re := range res {
		if re.MatchString(target) {
			return true
		}
	}
	return false
}

// resolve resolves overlapping rule candidates: longest secret first, ties to the earlier rule, then
// the earlier position. A candidate that overlaps accepted spans is merged with them into one span
// covering the union, labelled with the accepted span that won (dropping the
// shorter candidate could leave the part of a secret outside the longer span visible).
func resolve(rs *ruleset, cands []span) []span {
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool {
		li, lj := cands[i].end-cands[i].start, cands[j].end-cands[j].start
		if li != lj {
			return li > lj
		}
		if oi, oj := rs.rules[cands[i].rule].order, rs.rules[cands[j].rule].order; oi != oj {
			return oi < oj
		}
		return cands[i].start < cands[j].start
	})
	var acc []span
	for i, c := range cands {
		c.prio = i
		acc = mergeInsert(acc, c, false)
	}
	return acc
}

// mergeInsert inserts c into acc (sorted by start, non-overlapping). Spans of acc that overlap c are
// replaced by one span covering c and all of them. The union's label is the overlapping span with the
// lowest prio when c is a rule candidate (accepted spans always rank first). When c is an exact value
// (exactWins), exact wins: an overlapping exact span keeps its label, else c's.
func mergeInsert(acc []span, c span, exactWins bool) []span {
	lo := sort.Search(len(acc), func(i int) bool { return acc[i].end > c.start })
	hi := lo
	for hi < len(acc) && acc[hi].start < c.end {
		hi++
	}
	if lo == hi {
		return slices.Insert(acc, lo, c)
	}
	win := c
	if exactWins {
		for _, a := range acc[lo:hi] {
			if a.rule < 0 {
				win = a
				break
			}
		}
	} else {
		win = acc[lo]
		for _, a := range acc[lo+1 : hi] {
			if a.prio < win.prio {
				win = a
			}
		}
	}
	u := win
	u.start = min(c.start, acc[lo].start)
	u.end = max(c.end, acc[hi-1].end)
	if u.start != win.start || u.end != win.end {
		u.tagText = ""
	}
	acc[lo] = u
	return slices.Delete(acc, lo+1, hi)
}

// overflowRuleID labels the whole-string placeholder run uses when a string still has secrets after
// maxPasses passes.
const overflowRuleID = "overflow"

// placeholder renders the replacement for one span.
func (m *Masker) placeholder(s string, sp span) (text, ruleID string) {
	if sp.rule < 0 {
		return "[masked:exact:" + sp.fp12 + "]", exactRuleID
	}
	r := &m.rs.rules[sp.rule]
	if r.tag {
		secret := sp.tagText
		if secret == "" {
			secret = s[sp.start:sp.end]
		}
		return "[masked:" + r.id + ":" + tag12(secret) + "]", r.id
	}
	return "[masked:" + r.id + "]", r.id
}

const tagPrefix = "chat-archive/mask/v1/tag\x00"

// tag12 is hex(sha256("chat-archive/mask/v1/tag\x00" + secret))[:12].
func tag12(secret string) string {
	h := sha256.New()
	h.Write([]byte(tagPrefix))
	h.Write([]byte(secret))
	var sum [32]byte
	return hex.EncodeToString(h.Sum(sum[:0])[:6])
}

// placeholderShape is the shape of a placeholder: "[masked:<id>]" or "[masked:<id>:<12 hex>]". Only
// a match whose id is a rule id, "exact" or "overflow" is a placeholder (placeholderIDs), so a secret
// written between "[masked:" and "]" is still masked. An id has no brackets, so two shape matches
// never overlap and filtering them by id finds exactly the real placeholders.
var placeholderShape = regexp.MustCompile(`\[masked:([a-z0-9]+(?:-[a-z0-9]+)*)(?::[0-9a-f]{12})?\]`)

// placeholderIDs are the ids of caRules and gitleaksRules, plus "exact" and "overflow".
var placeholderIDs = sync.OnceValue(func() map[string]bool {
	ids := make(map[string]bool, len(caRules)+len(gitleaksRules)+2)
	for _, r := range caRules {
		ids[r.ID] = true
	}
	for _, r := range gitleaksRules {
		ids[r.ID] = true
	}
	ids[exactRuleID] = true
	ids[overflowRuleID] = true
	return ids
})

// placeholderSpans returns the placeholders in s, in order; nil when there are none.
func placeholderSpans(s string) [][]int {
	if !strings.Contains(s, "[masked:") {
		return nil
	}
	ids := placeholderIDs()
	var out [][]int
	for _, m := range placeholderShape.FindAllStringSubmatchIndex(s, -1) {
		if ids[s[m[2]:m[3]]] {
			out = append(out, m[:2])
		}
	}
	return out
}

// insidePlaceholders reports whether every byte of s[a:b] lies inside placeholder text. A rule can
// otherwise read a placeholder's own characters as a secret, for example ca-url-userinfo taking
// "https://[masked" + ":" + "sidekiq-sensitive-url]" + "@" after sidekiq-sensitive-url masked
// "user:pass" as one span; masking that again would break Mask(Mask(x)) == Mask(x).
func insidePlaceholders(phs [][]int, a, b int) bool {
	k := sort.Search(len(phs), func(i int) bool { return phs[i][1] > a })
	pos := a
	for _, p := range phs[k:] {
		if p[0] > pos {
			return false
		}
		pos = p[1]
		if pos >= b {
			return true
		}
	}
	return false
}

// isPlaceholder reports whether s is exactly one placeholder.
func isPlaceholder(s string) bool {
	m := placeholderSpans(s)
	return len(m) == 1 && m[0][0] == 0 && m[0][1] == len(s)
}

// shannonEntropy is gitleaks' detect/utils.go shannonEntropy: counts over runes, divided by the byte
// length. Terms are summed in rune order so every producer and the server get the same float.
func shannonEntropy(data string) float64 {
	if data == "" {
		return 0
	}
	var ascii [128]int
	var other map[rune]int
	for _, c := range data {
		if c < 128 {
			ascii[c]++
			continue
		}
		if other == nil {
			other = map[rune]int{}
		}
		other[c]++
	}
	inv := 1.0 / float64(len(data))
	var e float64
	for _, n := range ascii {
		if n > 0 {
			f := float64(n) * inv
			e -= f * math.Log2(f)
		}
	}
	if other != nil {
		runes := make([]rune, 0, len(other))
		for c := range other {
			runes = append(runes, c)
		}
		sort.Slice(runes, func(i, j int) bool { return runes[i] < runes[j] })
		for _, c := range runes {
			f := float64(other[c]) * inv
			e -= f * math.Log2(f)
		}
	}
	return e
}

// asciiLowerInto appends s with A-Z lowered to dst[:0], byte offsets unchanged (the prefilter's copy).
func asciiLowerInto(dst []byte, s string) []byte {
	dst = append(dst[:0], s...)
	for i, c := range dst {
		if 'A' <= c && c <= 'Z' {
			dst[i] = c + ('a' - 'A')
		}
	}
	return dst
}

// asciiLowerUnicode is strings.ToLower as bytes, for stopword matching (gitleaks lowercases the
// secret with strings.ToLower).
func asciiLowerUnicode(s string) []byte {
	return []byte(strings.ToLower(s))
}
