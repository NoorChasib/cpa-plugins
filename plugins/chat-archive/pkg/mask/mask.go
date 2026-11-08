// Package mask is the mask/v1 engine: the 217 non-path-scoped gitleaks v8.30.1 rules (generated into
// rules_gitleaks_v8_30_1.go by scripts/gen_mask_rules.py), the 18 ca-* rules, exact-value masking and
// the server's detect mode. It is pure Go and imports nothing from this
// module.
//
// The exported API is frozen: api_test.go pins every signature, and an exported identifier, field
// or JSON tag is never changed or removed, only added.
package mask

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"strings"
)

// Version is the masking semantics this package implements.
const Version = "mask/v1"

//go:embed rules_gitleaks_v8_30_1.go
var generatedRules []byte

var rulesetSHA256 = func() string {
	sum := sha256.Sum256(generatedRules)
	return hex.EncodeToString(sum[:])
}()

// RulesetSHA256 returns the sha256 (64 lowercase hex) of the generated rules file contents, reported
// in heartbeats as ruleset_sha256.
func RulesetSHA256() string {
	return rulesetSHA256
}

// Config is the exact-value set a Masker matches.
type Config struct {
	ExactFPs  [][32]byte // fingerprint.ExactFP of archive tokens, CH passwords, ARCHIVE_EXTRA_EXACT_FPS, HMAC key, own source token
	CallerFPs [][32]byte // raw 32-byte CallerScope values
	ExactRev  uint32
}

// Masker applies mask/v1. It is immutable after New and safe for concurrent use.
type Masker struct {
	rs      *ruleset
	exact   map[[32]byte]struct{}
	callers map[[32]byte]struct{}
	rev     uint32
}

// New builds a Masker for cfg.
func New(cfg Config) (*Masker, error) {
	rs, err := rulesetCompiled()
	if err != nil {
		return nil, err
	}
	m := &Masker{rs: rs, rev: cfg.ExactRev}
	if len(cfg.ExactFPs) > 0 {
		m.exact = make(map[[32]byte]struct{}, len(cfg.ExactFPs))
		for _, fp := range cfg.ExactFPs {
			m.exact[fp] = struct{}{}
		}
	}
	if len(cfg.CallerFPs) > 0 {
		m.callers = make(map[[32]byte]struct{}, len(cfg.CallerFPs))
		for _, fp := range cfg.CallerFPs {
			m.callers[fp] = struct{}{}
		}
	}
	return m, nil
}

// String masks one decoded string value. Only each secret group is replaced, by [masked:<rule-id>],
// [masked:<rule-id>:<tag12>] or [masked:exact:<fp12>]; hits counts replacements per rule id.
// Mask(Mask(x)) == Mask(x).
func (m *Masker) String(s string) (out string, hits Hits) {
	out, hits, _, _ = m.run(context.Background(), s, false)
	return out, hits
}

// StringContext is String that stops when ctx ends: it checks ctx between passes and every 64 scan
// windows, and returns "", nil and ctx.Err() then, so nothing unmasked comes back. This lets a
// masking deadline bound one large string.
func (m *Masker) StringContext(ctx context.Context, s string) (string, Hits, error) {
	out, hits, _, err := m.run(ctx, s, false)
	if err != nil {
		return "", nil, err
	}
	return out, hits, nil
}

// Detect is the server's detect-only mode: one Finding per secret String would replace, never the
// secret itself. Placeholders count as clean.
func (m *Masker) Detect(s string) []Finding {
	_, _, f, _ := m.run(context.Background(), s, true)
	return f
}

// maxPasses bounds the passes of run. A pass only follows a pass that replaced something; a string
// that still has secrets after maxPasses passes becomes one [masked:overflow] placeholder.
const maxPasses = 32

// run masks s in passes until a pass finds nothing. One pass can miss a secret whose match depends
// on a neighbour another rule replaces in the same pass: "<token>sk-..." has no \b before "sk-" until
// <token> becomes a placeholder ending in "]". Repeating until nothing changes makes the output a
// fixed point, so Mask(Mask(x)) == Mask(x) and the server's Detect of masked content finds nothing.
// Only crafted input (one back-to-back fixed-length token per pass) reaches maxPasses; it is then
// replaced whole, counted under "overflow", rather than left partly unmasked.
func (m *Masker) run(ctx context.Context, s string, detect bool) (string, Hits, []Finding, error) {
	var hits Hits
	var findings []Finding
	var lower []byte
	for pass := 0; ; pass++ {
		if err := ctx.Err(); err != nil {
			return "", nil, nil, err
		}
		spans, err := m.scanCtx(ctx, s, &lower)
		if err != nil {
			return "", nil, nil, err
		}
		if len(spans) == 0 {
			break
		}
		if hits == nil {
			hits = make(Hits, len(spans))
		}
		if pass == maxPasses {
			hits[overflowRuleID]++
			if detect {
				findings = append(findings, Finding{RuleID: overflowRuleID, Span: len(s)})
			}
			s = "[masked:" + overflowRuleID + "]"
			break
		}
		var b strings.Builder
		b.Grow(len(s))
		prev := 0
		for _, sp := range spans {
			text, id := m.placeholder(s, sp)
			b.WriteString(s[prev:sp.start])
			b.WriteString(text)
			prev = sp.end
			hits[id]++
			if detect {
				findings = append(findings, Finding{RuleID: id, Span: sp.end - sp.start})
			}
		}
		b.WriteString(s[prev:])
		s = b.String()
	}
	return s, hits, findings, nil
}

// ExactRev returns Config.ExactRev, recorded in every envelope as exact_rev.
func (m *Masker) ExactRev() uint32 {
	return m.rev
}

// Hits maps a rule id to a replacement count; counts only. Exact-value replacements
// count under "exact", and a string replaced whole after maxPasses passes under "overflow".
type Hits map[string]uint32

// Finding is one detect-mode match: the rule id ("exact" for an exact value) and the length in bytes
// of the secret span.
type Finding struct {
	RuleID string
	Span   int
}

// Get is the version registry for the server: Get("mask/v1") returns New and true; any other version
// returns nil and false.
func Get(version string) (func(Config) (*Masker, error), bool) {
	if version == Version {
		return New, true
	}
	return nil, false
}
