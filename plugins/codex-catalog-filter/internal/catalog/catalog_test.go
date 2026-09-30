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

// offOthers switches off everything but the GPT target set and auto review,
// leaving new models enabled: the setup the settings page produces.
var offOthers = map[string]bool{
	"claude-fable-5-1": false, "gpt-reserve": false, "grok-4.7": false, "or-kimi-k2": false,
	"cpa-sonnet": false, "gpt-image-2": false, "claude-opus-5-5": false,
}

func fixtureBody(keep func(slug string) (raw string, ok bool)) string {
	var parts []string
	for _, e := range fixture {
		if raw, ok := keep(e.slug); ok {
			parts = append(parts, raw)
		}
	}
	return `{"models":[` + strings.Join(parts, ",") + `]}`
}

func fullBody() []byte {
	return []byte(fixtureBody(func(slug string) (string, bool) { return rawOf(slug), true }))
}

func rawOf(slug string) string {
	for _, e := range fixture {
		if e.slug == slug {
			return e.raw
		}
	}
	panic(slug)
}

func mustRules(t *testing.T, models map[string]bool, enableNew bool, action Action) *Rules {
	t.Helper()
	r, err := NewRules(models, enableNew, action)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

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

func TestRemoveDropsSwitchedOffEntriesAndKeepsTheRestVerbatim(t *testing.T) {
	r := mustRules(t, offOthers, true, Remove)
	got := r.Rewrite(fullBody())
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

func TestHideRewritesOnlyVisibilityOfSwitchedOffEntries(t *testing.T) {
	r := mustRules(t, offOthers, true, Hide)
	got := r.Rewrite(fullBody())
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

func TestHideAddsMissingVisibility(t *testing.T) {
	r := mustRules(t, map[string]bool{"claude-x": false}, true, Hide)
	got := r.Rewrite([]byte(`{"models":[{"slug":"gpt-6-sol","visibility":"list"},{"slug":"claude-x" ,"n":1 }]}`))
	want := `{"models":[{"slug":"gpt-6-sol","visibility":"list"},{"slug":"claude-x" ,"n":1 ,"visibility":"hide"}]}`
	if string(got) != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func TestSwitchesOverrideTheNewModelSetting(t *testing.T) {
	cases := []struct {
		name        string
		models      map[string]bool
		enableNew   bool
		listed, all []string
	}{
		{
			name: "new models enabled, some switched off", models: map[string]bool{"claude-fable-5-1": false, "grok-4.7": false, "gpt-5.5": false}, enableNew: true,
			listed: []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "or-kimi-k2", "gpt-5.6-luna", "cpa-sonnet", "claude-opus-5-5"},
			all:    []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-reserve", "gpt-5.6-sol", "gpt-5.6-terra", "or-kimi-k2", "gpt-5.6-luna", "cpa-sonnet", "gpt-image-2", "codex-auto-review", "claude-opus-5-5"},
		},
		{
			name: "new models disabled, some switched on", models: map[string]bool{"gpt-6-sol": true, "gpt-5.5": true, "codex-auto-review": true}, enableNew: false,
			listed: []string{"gpt-6-sol", "gpt-5.5"},
			all:    []string{"gpt-6-sol", "gpt-5.5", "codex-auto-review"},
		},
		{
			name: "an explicit on is kept even when it matches the default", models: map[string]bool{"gpt-6-sol": true, "claude-fable-5-1": false}, enableNew: true,
			listed: []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-5.6-sol", "grok-4.7", "gpt-5.6-terra", "or-kimi-k2", "gpt-5.6-luna", "cpa-sonnet", "gpt-5.5", "claude-opus-5-5"},
			all:    []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-reserve", "gpt-5.6-sol", "grok-4.7", "gpt-5.6-terra", "or-kimi-k2", "gpt-5.6-luna", "cpa-sonnet", "gpt-5.5", "gpt-image-2", "codex-auto-review", "claude-opus-5-5"},
		},
		{
			name: "new models disabled drops auto review unless switched on", models: map[string]bool{"gpt-6-sol": true}, enableNew: false,
			listed: []string{"gpt-6-sol"}, all: []string{"gpt-6-sol"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mustRules(t, tc.models, tc.enableNew, Remove).Rewrite(fullBody())
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

func TestSwitchesForModelsNotInTheCatalogAreHarmless(t *testing.T) {
	r := mustRules(t, map[string]bool{"retired-model": false, "claude-fable-5-1": false}, true, Remove)
	d := decode(t, r.Rewrite(fullBody()))
	if slices.Contains(d.slugs(""), "claude-fable-5-1") || len(d.slugs("")) != len(fixture)-1 {
		t.Fatalf("entries = %v", d.slugs(""))
	}
}

func TestPreservesFormattingAndOtherTopLevelMembers(t *testing.T) {
	input := "\n{ \"etag\": \"abc\",\n  \"models\" : [\n    {\"slug\": \"claude-x\", \"visibility\": \"list\"},\n    {\"slug\": \"gpt-6-sol\", \"visibility\": \"list\"} ,\n    {\"slug\": \"gpt-5.5\",\n     \"visibility\": \"list\"}\n  ],\n  \"z\": [1, 2]\n}\n"
	off := map[string]bool{"claude-x": false}
	removeWant := "\n{ \"etag\": \"abc\",\n  \"models\" : [\n    {\"slug\": \"gpt-6-sol\", \"visibility\": \"list\"} ,\n    {\"slug\": \"gpt-5.5\",\n     \"visibility\": \"list\"}\n  ],\n  \"z\": [1, 2]\n}\n"
	if got := mustRules(t, off, true, Remove).Rewrite([]byte(input)); string(got) != removeWant {
		t.Fatalf("remove:\n%q\nwant\n%q", got, removeWant)
	}
	hideWant := strings.Replace(input, `{"slug": "claude-x", "visibility": "list"}`, `{"slug": "claude-x", "visibility": "hide"}`, 1)
	if got := mustRules(t, off, true, Hide).Rewrite([]byte(input)); string(got) != hideWant {
		t.Fatalf("hide:\n%q\nwant\n%q", got, hideWant)
	}
}

func TestRemovingTheLastEntryKeepsValidSeparators(t *testing.T) {
	got := mustRules(t, map[string]bool{"x": false}, true, Remove).Rewrite([]byte(`{"models":[{"slug":"gpt-a","visibility":"list"}, {"slug":"x","visibility":"list"}]}`))
	if string(got) != `{"models":[{"slug":"gpt-a","visibility":"list"}]}` {
		t.Fatalf("got %s", got)
	}
}

func TestPassesThroughAnythingElse(t *testing.T) {
	r := mustRules(t, map[string]bool{"claude-x": false, "grok-4.7": false, "claude-fable-5-1": false}, true, Remove)
	full := string(fullBody())
	cases := map[string]string{
		"openai list":             `{"object":"list","data":[{"id":"gpt-6-sol","object":"model"},{"id":"claude-fable-5-1","object":"model"}]}`,
		"claude list":             `{"data":[{"id":"claude-fable-5-1","type":"model","display_name":"Claude"}],"has_more":false,"first_id":"claude-fable-5-1","last_id":"claude-fable-5-1"}`,
		"gemini list":             `{"models":[{"name":"models/gemini-3-pro","displayName":"Gemini 3 Pro"},{"name":"models/claude-x"}]}`,
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
	if got := mustRules(t, map[string]bool{"claude-x": false}, true, Hide).Rewrite([]byte(`{"models":[{"slug":"gpt-6-sol","visibility":"list"},{"slug":"claude-x","visibility":"hide"}]}`)); got != nil {
		t.Errorf("hide of an already hidden entry rewrote to %s", got)
	}
	for name, idle := range map[string]*Rules{
		"no switches":        mustRules(t, nil, true, Remove),
		"only switches on":   mustRules(t, map[string]bool{"gpt-6-sol": true}, true, Remove),
		"nil rules (unused)": nil,
	} {
		if got := idle.Rewrite([]byte(full)); got != nil || !idle.Idle() {
			t.Errorf("%s rewrote to %s", name, got)
		}
	}
	if mustRules(t, nil, false, Remove).Idle() {
		t.Error("rules disabling every new model reported idle")
	}
}

func TestNewRulesRejectsAnUnknownAction(t *testing.T) {
	if _, err := NewRules(nil, true, "drop"); err == nil {
		t.Error("unknown action accepted")
	}
}

func TestRulesExposeTheirSettings(t *testing.T) {
	r := mustRules(t, map[string]bool{"a": false, "b": true}, false, Hide)
	switches := r.Switches()
	switches["a"] = true // a copy: the rules must not change
	if enabled, ok := r.Switched("a"); !ok || enabled || r.EnableNew() || r.Action() != Hide {
		t.Fatalf("settings = %v %v %v %v", enabled, ok, r.EnableNew(), r.Action())
	}
	if _, ok := r.Switched("c"); ok {
		t.Fatal("unswitched slug reported as switched")
	}
}

func TestSummarizeListsEntriesInOrder(t *testing.T) {
	entries, ok := Summarize(fullBody())
	if !ok || len(entries) != len(fixture) {
		t.Fatalf("summary = %v, %v", entries, ok)
	}
	if entries[0] != (Entry{Slug: "gpt-6-astra", DisplayName: "GPT-6 Astra", Visibility: "list"}) ||
		entries[3] != (Entry{Slug: "gpt-6-luna", Visibility: "list"}) ||
		entries[13] != (Entry{Slug: "codex-auto-review", DisplayName: "Codex Auto Review", Visibility: "hide"}) {
		t.Fatalf("summary = %+v", entries)
	}
	for _, body := range []string{`{"object":"list","data":[]}`, `{"models":[{"name":"models/x"}]}`, `{"models":[`, ``} {
		if _, ok := Summarize([]byte(body)); ok {
			t.Errorf("summarized %q", body)
		}
	}
	odd, ok := Summarize([]byte(`{"models":[{"slug":"x","display_name":7,"visibility":null}]}`))
	if !ok || odd[0] != (Entry{Slug: "x"}) {
		t.Fatalf("odd field types = %+v, %v", odd, ok)
	}
}

func BenchmarkRewriteRemove(b *testing.B) {
	r, _ := NewRules(offOthers, true, Remove)
	body := fullBody()
	for b.Loop() {
		if r.Rewrite(body) == nil {
			b.Fatal("no rewrite")
		}
	}
}
