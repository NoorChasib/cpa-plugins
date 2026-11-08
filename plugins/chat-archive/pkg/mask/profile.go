package mask

import (
	"context"
	"time"
)

// Keys of ProfileRules other than rule ids.
const (
	profilePrefilter = "(prefilter)"
	profileExact     = "(exact)"
)

// ProfileRules adds to acc the wall-clock time that one scan pass over s spends in each part of the
// engine, for mask-dry-run's MB/s per rule. A rule's time, under its id, is its regex and checks
// inside its windows, over s and over its decoded view; a rule whose keyword does not occur costs
// nothing and is not added. The shared work is under "(prefilter)": building the decoded view, the
// lowercase copy, the keyword automaton, the windows and the placeholder index, and resolving
// overlaps. Exact-value matching is under "(exact)", added only when the Masker has exact values.
// acc must not be nil.
//
// ProfileRules masks nothing and returns nothing. It times one pass, where String repeats passes
// until one finds nothing, so it measures scan cost, not a full String call. Replacement counts per
// rule are String's Hits. It is a separate code path: String, StringContext and Detect pay nothing
// for it.
func (m *Masker) ProfileRules(s string, acc map[string]time.Duration) {
	rs := m.rs
	t0 := time.Now()
	v := decodeView(s)
	acc[profilePrefilter] += time.Since(t0)
	cands := m.profileRules(s, acc)
	if v != nil {
		for _, c := range m.profileRules(v.text, acc) {
			c.start, c.end = v.rawStart(c.start), v.rawEnd(c.end)
			cands = append(cands, c)
		}
	}
	t0 = time.Now()
	accepted := resolve(rs, cands)
	acc[profilePrefilter] += time.Since(t0)
	if m.exactEnabled() {
		t0 = time.Now()
		if len(s) >= exactMinLen {
			accepted = m.addExact(s, accepted)
		}
		if v != nil && len(v.text) >= exactMinLen {
			m.addExactMapped(v.text, accepted, v)
		}
		acc[profileExact] += time.Since(t0)
	}
}

// profileRules is ruleCandidates for one text, timed per rule.
func (m *Masker) profileRules(text string, acc map[string]time.Duration) []span {
	rs := m.rs
	if len(text) < rs.minKW {
		return nil
	}
	t0 := time.Now()
	wins := rs.windowsInto(text, nil) // sorted by rule, then start
	t := newText(text)
	acc[profilePrefilter] += time.Since(t0)
	var cands []span
	for i := 0; i < len(wins); {
		j := i + 1
		for j < len(wins) && wins[j].rule == wins[i].rule {
			j++
		}
		t1 := time.Now()
		c, _ := rs.candidatesCtx(context.Background(), t, wins[i:j])
		acc[rs.rules[wins[i].rule].id] += time.Since(t1)
		cands = append(cands, c...)
		i = j
	}
	return cands
}
