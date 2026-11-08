package mask

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Exact-value masking.
const (
	exactMinLen  = 20
	exactMaxLen  = 512
	exactMaxSeps = 4
	exactRuleID  = "exact"

	// The two formulas are pkg/fingerprint's ExactFP and CallerScope; pkg/mask imports nothing from
	// this module, and a test checks both stay equal.
	exactFPPrefix     = "chat-archive/exact/v1\x00"
	callerScopePrefix = "cli-proxy-api:caller-scope:v1\x00"
)

// runClass is [A-Za-z0-9_\-+/=.~].
var runClass = func() (t [256]bool) {
	for c := 'a'; c <= 'z'; c++ {
		t[c] = true
	}
	for c := 'A'; c <= 'Z'; c++ {
		t[c] = true
	}
	for c := '0'; c <= '9'; c++ {
		t[c] = true
	}
	for _, c := range "_-+/=.~" {
		t[c] = true
	}
	return t
}()

func (m *Masker) exactEnabled() bool { return len(m.exact) > 0 || len(m.callers) > 0 }

// addExact tests every maximal run of at least 20 run-class bytes and merges the first matching
// sub-candidate of each run into acc. Exact wins: a rule span it overlaps is folded into one exact
// span covering both, so no part of a registered value stays visible.
func (m *Masker) addExact(s string, acc []span) []span {
	return m.addExactMapped(s, acc, nil)
}

// addExactMapped is addExact over t, the decoded view v of the scanned string when v is not nil; the
// spans found are mapped back to the original.
func (m *Masker) addExactMapped(t string, acc []span, v *view) []span {
	for i := 0; i < len(t); {
		if !runClass[t[i]] {
			i++
			continue
		}
		j := i + 1
		for j < len(t) && runClass[t[j]] {
			j++
		}
		if j-i >= exactMinLen {
			if sp, ok := m.exactRun(t, i, j); ok {
				if v != nil {
					sp.start, sp.end = v.rawStart(sp.start), v.rawEnd(sp.end)
				}
				acc = mergeInsert(acc, sp, true)
			}
		}
		i = j
	}
	return acc
}

// Trim classes of exactRun: a value ending a sentence ("V.", "V..."), on a diff line ("+V", "-V") or
// glued to punctuation.
const (
	exactTrimTrailing = ".~-_+="
	exactTrimLeading  = "+-._~"
)

// exactRun tests, in order: the whole run s[a:b]; the suffix after each of the first 4 '=' or '/';
// the suffix after the last '=' or '/'; each segment between the first 4 separators and the run's
// ends; the run with trailing ".~-_+=" removed, with leading "+-._~" removed, and with both; the last
// segment with trailing ".~-_+=" removed. Sub-candidates under 20 or over 512 bytes are skipped, so a
// run of any length is tested. At most 15 tests.
func (m *Masker) exactRun(s string, a, b int) (span, bool) {
	if sp, ok := m.exactTest(s, a, b); ok {
		return sp, true
	}
	var seps [exactMaxSeps]int
	n, last := 0, -1
	for p := a; p < b; p++ {
		if s[p] == '=' || s[p] == '/' {
			if n < exactMaxSeps {
				seps[n] = p
				n++
			}
			last = p
		}
	}
	for k := 0; k < n; k++ {
		if sp, ok := m.exactTest(s, seps[k]+1, b); ok {
			return sp, true
		}
	}
	if n > 0 && last != seps[n-1] {
		if sp, ok := m.exactTest(s, last+1, b); ok {
			return sp, true
		}
	}
	if n > 0 {
		prev := a - 1
		for k := 0; k <= n; k++ {
			end := b
			if k < n {
				end = seps[k]
			}
			if sp, ok := m.exactTest(s, prev+1, end); ok {
				return sp, true
			}
			prev = end
		}
	}
	tb := b
	for tb > a && strings.IndexByte(exactTrimTrailing, s[tb-1]) >= 0 {
		tb--
	}
	ta := a
	for ta < b && strings.IndexByte(exactTrimLeading, s[ta]) >= 0 {
		ta++
	}
	if tb != b {
		if sp, ok := m.exactTest(s, a, tb); ok {
			return sp, true
		}
	}
	if ta != a {
		if sp, ok := m.exactTest(s, ta, b); ok {
			return sp, true
		}
	}
	if ta != a && tb != b && ta < tb {
		if sp, ok := m.exactTest(s, ta, tb); ok {
			return sp, true
		}
	}
	if last >= 0 && tb != b && last+1 < tb {
		if sp, ok := m.exactTest(s, last+1, tb); ok {
			return sp, true
		}
	}
	return span{}, false
}

func (m *Masker) exactTest(s string, a, b int) (span, bool) {
	if b-a < exactMinLen || b-a > exactMaxLen {
		return span{}, false
	}
	v := s[a:b]
	if len(m.exact) > 0 {
		fp := prefixedSum(exactFPPrefix, v)
		if _, ok := m.exact[fp]; ok {
			return span{start: a, end: b, rule: -1, fp12: hex.EncodeToString(fp[:6])}, true
		}
	}
	if len(m.callers) > 0 {
		fp := prefixedSum(callerScopePrefix, v)
		if _, ok := m.callers[fp]; ok {
			return span{start: a, end: b, rule: -1, fp12: hex.EncodeToString(fp[:6])}, true
		}
	}
	return span{}, false
}

// prefixedSum is sha256(prefix + v) without allocating for v up to exactMaxLen bytes.
func prefixedSum(prefix, v string) [32]byte {
	var buf [len(callerScopePrefix) + exactMaxLen]byte
	n := copy(buf[:], prefix)
	n += copy(buf[n:], v)
	return sha256.Sum256(buf[:n])
}
