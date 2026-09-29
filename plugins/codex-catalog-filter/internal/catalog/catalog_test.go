package catalog

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// Entries shaped like CPA v8.0.4's compact Codex catalog. Instructions carry
// characters CPA deliberately leaves unescaped, so byte comparison catches any
// accidental re-encoding.
var fixture = []struct{ slug, raw string }{
	{"gpt-6-astra", `{"slug":"gpt-6-astra","display_name":"GPT-6 Astra","visibility":"list","priority":1,"base_instructions":"Use <tags> & keep \"quotes\" — ünïcode","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}]}`},
	{"gpt-6-sol", `{"slug":"gpt-6-sol","display_name":"GPT-6 Sol","visibility":"list","context_window":400000}`},
	{"claude-fable-5-1", `{"slug":"claude-fable-5-1","display_name":"Claude Fable 5.1","visibility":"list","context_window":1000000}`},
	{"gpt-6-luna", `{"slug":"gpt-6-luna","visibility":"list"}`},
	{"gpt-reserve", `{"slug":"gpt-reserve","visibility":"hide"}`},
	{"gpt-5.6-sol", `{"slug":"gpt-5.6-sol","visibility":"list"}`},
	{"grok-4.7", `{"slug":"grok-4.7","visibility":"list"}`},
	{"gpt-5.6-terra", `{"slug":"gpt-5.6-terra","visibility":"list"}`},
	{"or-kimi-k2", `{"slug":"or-kimi-k2","visibility":"list"}`},
	{"gpt-5.6-luna", `{"slug":"gpt-5.6-luna","visibility":"list"}`},
	{"cpa-sonnet", `{"slug":"cpa-sonnet","visibility":"list"}`},
	{"gpt-5.5", `{"slug":"gpt-5.5","visibility":"list"}`},
	{"gpt-image-2", `{"slug":"gpt-image-2","visibility":"hide"}`},
	{"codex-auto-review", `{"slug":"codex-auto-review","display_name":"Codex Auto Review","visibility":"hide"}`},
	{"claude-opus-5-5", `{"visibility":"list","slug":"claude-opus-5-5"}`},
}

var targetSet = []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5"}

func fixtureBody(keep func(slug string) (raw string, ok bool)) string {
	var parts []string
	for _, e := range fixture {
		if raw, ok := keep(e.slug); ok {
			parts = append(parts, raw)
		}
	}
	return `{"models":[` + strings.Join(parts, ",") + `]}`
}

func mustRules(t *testing.T, include, exclude []string, action Action) *Rules {
	t.Helper()
	r, err := NewRules(include, exclude, action)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var codexOnly = []string{"gpt-[0-9]*", "codex-*"}

type decoded struct {
	Models []struct {
		Slug       string `json:"slug"`
		Visibility string `json:"visibility"`
	} `json:"models"`
}

func decode(t *testing.T, body []byte) decoded {
	t.Helper()
	var d decoded
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatalf("invalid output %s: %v", body, err)
	}
	return d
}

func (d decoded) slugs(visibility string) []string {
	var out []string
	for _, m := range d.Models {
		if visibility == "" || m.Visibility == visibility {
			out = append(out, m.Slug)
		}
	}
	return out
}

func TestRemoveKeepsOnlyMatchingEntriesVerbatim(t *testing.T) {
	r := mustRules(t, codexOnly, nil, Remove)
	input := fixtureBody(func(slug string) (string, bool) { return rawOf(slug), true })
	got := r.Rewrite([]byte(input))
	want := fixtureBody(func(slug string) (string, bool) { return rawOf(slug), r.Allows(slug) })
	if string(got) != want {
		t.Fatalf("remove output differs\n got: %s\nwant: %s", got, want)
	}
	d := decode(t, got)
	if listed := d.slugs("list"); !slices.Equal(listed, targetSet) {
		t.Fatalf("listed = %v, want %v", listed, targetSet)
	}
	if hidden := d.slugs("hide"); !slices.Equal(hidden, []string{"codex-auto-review"}) {
		t.Fatalf("hidden = %v; codex-auto-review must survive as a hidden entry", hidden)
	}
}

func TestHideRewritesOnlyVisibilityOfOtherEntries(t *testing.T) {
	r := mustRules(t, codexOnly, nil, Hide)
	input := fixtureBody(func(slug string) (string, bool) { return rawOf(slug), true })
	got := r.Rewrite([]byte(input))
	want := fixtureBody(func(slug string) (string, bool) {
		raw := rawOf(slug)
		if !r.Allows(slug) {
			raw = strings.Replace(raw, `"visibility":"list"`, `"visibility":"hide"`, 1)
		}
		return raw, true
	})
	if string(got) != want {
		t.Fatalf("hide output differs\n got: %s\nwant: %s", got, want)
	}
	d := decode(t, got)
	if listed := d.slugs("list"); !slices.Equal(listed, targetSet) {
		t.Fatalf("listed = %v, want %v", listed, targetSet)
	}
	if all := d.slugs(""); len(all) != len(fixture) {
		t.Fatalf("hide dropped entries: %v", all)
	}
}

func rawOf(slug string) string {
	for _, e := range fixture {
		if e.slug == slug {
			return e.raw
		}
	}
	panic(slug)
}

func TestHideAddsMissingVisibility(t *testing.T) {
	r := mustRules(t, []string{"gpt-*"}, nil, Hide)
	got := r.Rewrite([]byte(`{"models":[{"slug":"gpt-6-sol","visibility":"list"},{"slug":"claude-x" ,"n":1 }]}`))
	want := `{"models":[{"slug":"gpt-6-sol","visibility":"list"},{"slug":"claude-x" ,"n":1 ,"visibility":"hide"}]}`
	if string(got) != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func TestIncludeExcludeActionCombinations(t *testing.T) {
	input := []byte(fixtureBody(func(slug string) (string, bool) { return rawOf(slug), true }))
	cases := []struct {
		name             string
		include, exclude []string
		action           Action
		listed, all      []string
	}{
		{
			name: "exclude narrows include", include: codexOnly, exclude: []string{"gpt-5.5", "gpt-6-luna"}, action: Remove,
			listed: []string{"gpt-6-astra", "gpt-6-sol", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"},
			all:    []string{"gpt-6-astra", "gpt-6-sol", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "codex-auto-review"},
		},
		{
			name: "exclude wins over an exact include", include: []string{"gpt-5.5", "gpt-6-sol"}, exclude: []string{"gpt-5.*"}, action: Remove,
			listed: []string{"gpt-6-sol"}, all: []string{"gpt-6-sol"},
		},
		{
			name: "exclude glob with hide", include: []string{"*"}, exclude: []string{"claude-*", "grok-*", "or-*", "cpa-*"}, action: Hide,
			listed: targetSet,
			all:    fixtureSlugs(),
		},
		{
			name: "include without codex-* drops auto review", include: []string{"gpt-[0-9]*"}, action: Remove,
			listed: targetSet, all: targetSet,
		},
		{
			name: "several include patterns", include: []string{"gpt-6-*", "claude-*"}, action: Remove,
			listed: []string{"gpt-6-astra", "gpt-6-sol", "claude-fable-5-1", "gpt-6-luna", "claude-opus-5-5"},
			all:    []string{"gpt-6-astra", "gpt-6-sol", "claude-fable-5-1", "gpt-6-luna", "claude-opus-5-5"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mustRules(t, tc.include, tc.exclude, tc.action).Rewrite(input)
			if got == nil {
				t.Fatal("expected a rewrite")
			}
			d := decode(t, got)
			if listed := d.slugs("list"); !slices.Equal(listed, tc.listed) {
				t.Errorf("listed = %v, want %v", listed, tc.listed)
			}
			if all := d.slugs(""); !slices.Equal(all, tc.all) {
				t.Errorf("entries = %v, want %v", all, tc.all)
			}
		})
	}
}

func fixtureSlugs() []string {
	var out []string
	for _, e := range fixture {
		out = append(out, e.slug)
	}
	return out
}

func TestPreservesFormattingAndOtherTopLevelMembers(t *testing.T) {
	input := "\n{ \"etag\": \"abc\",\n  \"models\" : [\n    {\"slug\": \"claude-x\", \"visibility\": \"list\"},\n    {\"slug\": \"gpt-6-sol\", \"visibility\": \"list\"} ,\n    {\"slug\": \"gpt-5.5\",\n     \"visibility\": \"list\"}\n  ],\n  \"z\": [1, 2]\n}\n"
	removeWant := "\n{ \"etag\": \"abc\",\n  \"models\" : [\n    {\"slug\": \"gpt-6-sol\", \"visibility\": \"list\"} ,\n    {\"slug\": \"gpt-5.5\",\n     \"visibility\": \"list\"}\n  ],\n  \"z\": [1, 2]\n}\n"
	if got := mustRules(t, codexOnly, nil, Remove).Rewrite([]byte(input)); string(got) != removeWant {
		t.Fatalf("remove:\n%q\nwant\n%q", got, removeWant)
	}
	hideWant := strings.Replace(input, `{"slug": "claude-x", "visibility": "list"}`, `{"slug": "claude-x", "visibility": "hide"}`, 1)
	if got := mustRules(t, codexOnly, nil, Hide).Rewrite([]byte(input)); string(got) != hideWant {
		t.Fatalf("hide:\n%q\nwant\n%q", got, hideWant)
	}
}

func TestRemovingTheLastEntryKeepsValidSeparators(t *testing.T) {
	got := mustRules(t, []string{"gpt-*"}, nil, Remove).Rewrite([]byte(`{"models":[{"slug":"gpt-a","visibility":"list"}, {"slug":"x","visibility":"list"}]}`))
	if string(got) != `{"models":[{"slug":"gpt-a","visibility":"list"}]}` {
		t.Fatalf("got %s", got)
	}
}

func TestPassesThroughAnythingElse(t *testing.T) {
	r := mustRules(t, codexOnly, nil, Remove)
	full := fixtureBody(func(slug string) (string, bool) { return rawOf(slug), true })
	cases := map[string]string{
		"openai list":             `{"object":"list","data":[{"id":"gpt-6-sol","object":"model"},{"id":"claude-fable-5-1","object":"model"}]}`,
		"claude list":             `{"data":[{"id":"claude-fable-5-1","type":"model","display_name":"Claude"}],"has_more":false,"first_id":"claude-fable-5-1","last_id":"claude-fable-5-1"}`,
		"gemini list":             `{"models":[{"name":"models/gemini-3-pro","displayName":"Gemini 3 Pro"},{"name":"models/claude-x"}]}`,
		"grok list":               `{"object":"list","data":[{"id":"grok-4.7","model":"grok-4.7","name":"Grok"}]}`,
		"malformed":               `{"models":[{"slug":"gpt-6-sol","visibility":"list"},{"slug":"claude-x"`,
		"trailing garbage":        full + `x`,
		"two documents":           full + ` {}`,
		"not an object":           `[{"slug":"claude-x"}]`,
		"empty":                   ``,
		"empty models":            `{"models":[]}`,
		"models not an array":     `{"models":{"slug":"claude-x"}}`,
		"entry without slug":      `{"models":[{"slug":"gpt-6-sol","visibility":"list"},{"name":"claude-x"}]}`,
		"entry not an object":     `{"models":[{"slug":"gpt-6-sol","visibility":"list"},"claude-x"]}`,
		"non-string slug":         `{"models":[{"slug":"gpt-6-sol","visibility":"list"},{"slug":7}]}`,
		"empty slug":              `{"models":[{"slug":"gpt-6-sol","visibility":"list"},{"slug":""}]}`,
		"duplicate slug":          `{"models":[{"slug":"gpt-6-sol","visibility":"list"},{"slug":"claude-x","slug":"gpt-5.5"}]}`,
		"duplicate visibility":    `{"models":[{"slug":"gpt-6-sol","visibility":"list"},{"slug":"claude-x","visibility":"list","visibility":"hide"}]}`,
		"duplicate models":        `{"models":[{"slug":"claude-x","visibility":"list"}],"models":[{"slug":"gpt-6-sol","visibility":"list"}]}`,
		"nothing to change":       `{"models":[{"slug":"gpt-6-sol","visibility":"list"},{"slug":"codex-auto-review","visibility":"hide"}]}`,
		"nothing would be listed": `{"models":[{"slug":"claude-x","visibility":"list"},{"slug":"codex-auto-review","visibility":"hide"}]}`,
		"nothing would remain":    `{"models":[{"slug":"claude-x","visibility":"list"},{"slug":"grok-4.7","visibility":"list"}]}`,
	}
	for name, body := range cases {
		if got := r.Rewrite([]byte(body)); got != nil {
			t.Errorf("%s: rewrote to %s", name, got)
		}
	}
	if got := mustRules(t, codexOnly, nil, Hide).Rewrite([]byte(`{"models":[{"slug":"gpt-6-sol","visibility":"list"},{"slug":"claude-x","visibility":"hide"}]}`)); got != nil {
		t.Errorf("hide of an already hidden entry rewrote to %s", got)
	}
	if got := mustRules(t, nil, nil, Remove).Rewrite([]byte(full)); got != nil {
		t.Errorf("idle rules rewrote to %s", got)
	}
	var nilRules *Rules
	if got := nilRules.Rewrite([]byte(full)); got != nil || !nilRules.Idle() {
		t.Errorf("nil rules rewrote to %s", got)
	}
}

func TestNewRulesRejectsBadInput(t *testing.T) {
	if _, err := NewRules([]string{"gpt-*"}, nil, "drop"); err == nil {
		t.Error("unknown action accepted")
	}
	if _, err := NewRules([]string{"gpt-[0-9"}, nil, Remove); err == nil {
		t.Error("malformed include accepted")
	}
	if _, err := NewRules([]string{"gpt-*"}, []string{"["}, Remove); err == nil {
		t.Error("malformed exclude accepted")
	}
}

func BenchmarkRewriteRemove(b *testing.B) {
	r, _ := NewRules(codexOnly, nil, Remove)
	body := []byte(fixtureBody(func(slug string) (string, bool) { return rawOf(slug), true }))
	for b.Loop() {
		if r.Rewrite(body) == nil {
			b.Fatal("no rewrite")
		}
	}
}
