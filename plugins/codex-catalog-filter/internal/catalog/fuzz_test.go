package catalog

import (
	"encoding/json"
	"reflect"
	"testing"
)

// checkRewrite decodes input and output independently of the splicer and fails
// unless a rewrite is exactly the filtered catalog. Seeds run with go test;
// explore further with go test -fuzz=FuzzRewrite ./internal/catalog/.
func checkRewrite(t *testing.T, r *Rules, body []byte) {
	out := r.Rewrite(body)
	if out == nil {
		return
	}
	if !json.Valid(out) {
		t.Fatalf("invalid output %q", out)
	}
	var in, got map[string]any
	if err := json.Unmarshal(body, &in); err != nil {
		t.Fatalf("rewrote a body Go cannot decode: %q", body)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not an object: %q", out)
	}
	if len(got) != len(in) {
		t.Fatalf("top-level members changed: in %q out %q", body, out)
	}
	for k, v := range in {
		if k != "models" && !reflect.DeepEqual(v, got[k]) {
			t.Fatalf("top-level %q changed: in %q out %q", k, body, out)
		}
	}
	gotModels, ok := got["models"].([]any)
	if !ok {
		t.Fatalf("no models in output %q", out)
	}
	var want []any
	listed := 0
	for _, m := range in["models"].([]any) {
		entry, ok := m.(map[string]any)
		slug, _ := entry["slug"].(string)
		if !ok || slug == "" {
			t.Fatalf("rewrote a catalog with an invalid entry: %q", body)
		}
		if r.Allows(slug) {
			want = append(want, entry)
			if entry["visibility"] == "list" {
				listed++
			}
			continue
		}
		if r.action == Hide {
			hidden := map[string]any{"visibility": "hide"}
			for k, v := range entry {
				if k != "visibility" {
					hidden[k] = v
				}
			}
			want = append(want, hidden)
		}
	}
	if listed == 0 {
		t.Fatalf("rewrite left no listed entry: %q", out)
	}
	if !reflect.DeepEqual(want, gotModels) {
		t.Fatalf("models mismatch:\nin  %q\nout %q", body, out)
	}
}

func FuzzRewrite(f *testing.F) {
	for _, seed := range []string{
		`{"models":[{"slug":"gpt-6","visibility":"list"},{"slug":"other","visibility":"list"}]}`,
		`{ "etag" : 1 , "models" : [ {"slug":"a","visibility":"list"} , {"visibility":"list","slug":"gpt-1"}, {"slug":"z"} ] , "x":[1,{"y":null}] }`,
		`{"models":[{"slug":"gpt-6","visibility":"list"},{"slug":"b","visibility":null}]}`,
		"{\"models\":[\n  {\"slug\": \"gpt-6\" ,\"visibility\" : \"list\"},\n  {\"slug\":\"b\"}\n]}\n",
		`{"models":[{"slug":"b","visibility":"hide"},{"slug":"gpt-x","visibility":"list"},{"slug":"c","visibility":123}]}`,
		`{"models":[{"slug":"gpt-6-mini","visibility":"list"},{"slug":"gpt-é","visibility":"list","d":"<&>"}]}`,
	} {
		f.Add([]byte(seed), false)
		f.Add([]byte(seed), true)
	}
	// Some slugs switched off with new models enabled, and the reverse.
	remove, _ := NewRules(map[string]bool{"other": false, "b": false, "gpt-6-mini": false}, true, Remove)
	hide, _ := NewRules(map[string]bool{"gpt-6": true, "gpt-x": true, "gpt-1": true, "gpt-\u00e9": true}, false, Hide)
	f.Fuzz(func(t *testing.T, body []byte, useHide bool) {
		if useHide {
			checkRewrite(t, hide, body)
		} else {
			checkRewrite(t, remove, body)
		}
	})
}
