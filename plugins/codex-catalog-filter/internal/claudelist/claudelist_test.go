package claudelist

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

// fixture is CPA v8.0.73's answer to Claude Code's discovery request
// (GET /v1/models?limit=1000 with anthropic-version) on 2026-11-19: 89
// models, 68 of them cloaked. Model lists carry no credentials.
func fixture(t testing.TB) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/cpa-claude-models.json")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCloakRoundTripsAsCPADoes(t *testing.T) {
	for real, cloaked := range map[string]string{
		"gpt-6-sol":         "claude-fable-5-dd-los-6-tpg",
		"or-glm-5-3-flash":  "claude-fable-5-dd-hsalf-3-5-mlg-ro",
		"claude-opus-5-5":   "claude-opus-5-5",
		"credits-claude-x":  "claude-fable-5-dd-x-edualc-stiderc",
		"gemini-4-argon-é":  "claude-fable-5-dd-é-nogra-4-inimeg",
		"":                  "",
		"claude-fable-5-dd": "claude-fable-5-dd",
	} {
		if got := Cloak(real); got != cloaked {
			t.Errorf("Cloak(%q) = %q, want %q", real, got, cloaked)
		}
		if got := Decloak(cloaked); got != real {
			t.Errorf("Decloak(%q) = %q, want %q", cloaked, got, real)
		}
	}
	// CPA keeps a "(level)" suffix when resolving a cloaked id.
	if got := Decloak("claude-fable-5-dd-los-6-tpg(high)"); got != "gpt-6-sol(high)" {
		t.Fatalf("suffix = %q", got)
	}
	if got := Decloak("claude-fable-5-dd-"); got != "claude-fable-5-dd-" {
		t.Fatalf("empty = %q", got)
	}
}

func TestSummarizeFixture(t *testing.T) {
	entries, ok := Summarize(fixture(t))
	if !ok || len(entries) != 89 {
		t.Fatalf("summary %v, %d entries", ok, len(entries))
	}
	cloaked := 0
	ids := map[string]bool{}
	for _, e := range entries {
		if e.CloakedID != "" {
			cloaked++
			if Cloak(e.ID) != e.CloakedID || strings.HasPrefix(e.ID, "claude-fable-5-dd-") {
				t.Errorf("entry %+v", e)
			}
		}
		ids[e.ID] = true
	}
	if cloaked != 68 || !ids["gpt-6-sol"] || !ids["gemini-4-argon-high"] || !ids["claude-opus-5-5"] {
		t.Fatalf("cloaked %d, ids %v", cloaked, len(ids))
	}
}

func TestRewriteKeepsEntryBytesAndRecomputesEnds(t *testing.T) {
	body := fixture(t)
	allowed := map[string]bool{"claude-opus-5-5": true, "gpt-6-sol": true, "gemini-4-argon-high": true}
	summary, out, ok := Rewrite(body, func(id string) bool { return allowed[id] })
	if !ok || len(summary) != 89 || out == nil {
		t.Fatalf("rewrite %v %d %v", ok, len(summary), out == nil)
	}
	var original, got struct {
		Data    []json.RawMessage `json:"data"`
		FirstID string            `json:"first_id"`
		LastID  string            `json:"last_id"`
		HasMore bool              `json:"has_more"`
	}
	if json.Unmarshal(body, &original) != nil || json.Unmarshal(out, &got) != nil {
		t.Fatalf("invalid json: %s", out)
	}
	var want []json.RawMessage
	for i, raw := range original.Data {
		if allowed[summary[i].ID] {
			want = append(want, raw)
		}
	}
	if len(got.Data) != 3 || !slices.EqualFunc(got.Data, want, func(a, b json.RawMessage) bool { return string(a) == string(b) }) {
		t.Fatalf("data = %s", out)
	}
	var first, last struct{ ID string }
	_ = json.Unmarshal(want[0], &first)
	_ = json.Unmarshal(want[2], &last)
	if got.FirstID != first.ID || got.LastID != last.ID || got.HasMore {
		t.Fatalf("ends = %s %s %v", got.FirstID, got.LastID, got.HasMore)
	}
	// Everything but data, first_id, and last_id is byte-identical.
	if !strings.HasPrefix(string(out), `{"data":[`) || !strings.Contains(string(out), `"has_more":false`) {
		t.Fatalf("shape = %.200s", out)
	}
}

func TestRewriteExactBytes(t *testing.T) {
	body := `{ "data" : [ {"id":"claude-a","display_name":"A"} , {"id":"claude-fable-5-dd-b-tpg","x":[1,2]}, {"id":"claude-c"} ], "first_id":"claude-a", "has_more":false, "last_id":"claude-c", "extra":{"k":"<&>"} }`
	_, out, ok := Rewrite([]byte(body), func(id string) bool { return id != "claude-a" })
	want := `{ "data" : [{"id":"claude-fable-5-dd-b-tpg","x":[1,2]},{"id":"claude-c"}], "first_id":"claude-fable-5-dd-b-tpg", "has_more":false, "last_id":"claude-c", "extra":{"k":"<&>"} }`
	if !ok || string(out) != want {
		t.Fatalf("got  %s\nwant %s", out, want)
	}
	_, out, _ = Rewrite([]byte(body), func(id string) bool { return id == "gpt-b" })
	if string(out) != `{ "data" : [{"id":"claude-fable-5-dd-b-tpg","x":[1,2]}], "first_id":"claude-fable-5-dd-b-tpg", "has_more":false, "last_id":"claude-fable-5-dd-b-tpg", "extra":{"k":"<&>"} }` {
		t.Fatalf("single = %s", out)
	}
}

func TestRewritePassesThrough(t *testing.T) {
	body := fixture(t)
	if _, out, ok := Rewrite(body, func(string) bool { return true }); !ok || out != nil {
		t.Fatal("nothing removed must pass through")
	}
	// Claude Code ignores an empty list; never send one.
	if _, out, ok := Rewrite(body, func(string) bool { return false }); !ok || out != nil {
		t.Fatal("nothing left must pass through")
	}
	for name, body := range map[string]string{
		"openai list":      `{"object":"list","data":[{"id":"gpt-6-sol"}]}`,
		"codex catalog":    `{"models":[{"slug":"gpt-6-sol"}]}`,
		"no first_id":      `{"data":[{"id":"a"}],"has_more":false,"last_id":"a"}`,
		"empty data":       `{"data":[],"first_id":"","has_more":false,"last_id":""}`,
		"entry without id": `{"data":[{"name":"a"}],"first_id":"a","has_more":false,"last_id":"a"}`,
		"numeric id":       `{"data":[{"id":1}],"first_id":"a","has_more":false,"last_id":"a"}`,
		"entry not object": `{"data":["a"],"first_id":"a","has_more":false,"last_id":"a"}`,
		"duplicate data":   `{"data":[{"id":"a"}],"data":[{"id":"b"}],"first_id":"a","has_more":false,"last_id":"a"}`,
		"trailing":         `{"data":[{"id":"a"}],"first_id":"a","has_more":false,"last_id":"a"} {}`,
		"truncated":        `{"data":[{"id":"a"}],"first_id":"a"`,
		"array":            `[{"id":"a"}]`,
		"inference":        `{"id":"msg_1","type":"message","content":[{"type":"text","text":"hi"}]}`,
	} {
		if _, out, ok := Rewrite([]byte(body), func(string) bool { return false }); ok || out != nil {
			t.Errorf("%s: rewritten", name)
		}
	}
}

func FuzzRewrite(f *testing.F) {
	f.Add(fixture(f), "gpt")
	f.Add([]byte(`{"data":[{"id":"claude-a"},{"id":"b"}],"first_id":"claude-a","has_more":false,"last_id":"b"}`), "claude")
	f.Fuzz(func(t *testing.T, body []byte, keep string) {
		allows := func(id string) bool { return strings.Contains(id, keep) }
		summary, out, ok := Rewrite(body, allows)
		if !ok || out == nil {
			return
		}
		var got struct {
			Data    []struct{ ID string } `json:"data"`
			FirstID string                `json:"first_id"`
			LastID  string                `json:"last_id"`
		}
		if err := json.Unmarshal(out, &got); err != nil || len(got.Data) == 0 {
			t.Fatalf("invalid output %s: %v", out, err)
		}
		var want []string
		for _, e := range summary {
			if allows(e.ID) {
				want = append(want, e.ID)
			}
		}
		var gotIDs []string
		for _, e := range got.Data {
			gotIDs = append(gotIDs, Decloak(e.ID))
		}
		if !slices.Equal(gotIDs, want) || got.FirstID != got.Data[0].ID || got.LastID != got.Data[len(got.Data)-1].ID {
			t.Fatalf("kept %v, want %v; ends %s %s", gotIDs, want, got.FirstID, got.LastID)
		}
	})
}
