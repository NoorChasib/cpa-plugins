package agy

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

// fixture is CPA v8.0.73's /v1beta/models as served on 2026-11-19 (89
// models, no limits on 25 of them). Model lists carry no credentials.
func fixture(t *testing.T) []Model {
	t.Helper()
	raw, err := os.ReadFile("testdata/cpa-v1beta-models.json")
	if err != nil {
		t.Fatal(err)
	}
	models, ok := ParseGeminiList(raw)
	if !ok || len(models) != 89 {
		t.Fatalf("fixture parsed %v, %d models", ok, len(models))
	}
	return models
}

type wire struct {
	DefaultModelID string `json:"defaultModelId"`
	List           []struct {
		ModelID          string `json:"modelId"`
		DisplayName      string `json:"displayName"`
		MaxTokens        *int   `json:"maxTokens"`
		MaxOutputTokens  *int   `json:"maxOutputTokens"`
		SupportsThinking *bool  `json:"supportsThinking"`
		ThinkingBudget   *int   `json:"thinkingBudget"`
	} `json:"list"`
}

func decode(t *testing.T, body []byte) wire {
	t.Helper()
	var w wire
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		t.Fatalf("invalid agy catalog %s: %v", body, err)
	}
	return w
}

func ids(w wire) []string {
	var out []string
	for _, e := range w.List {
		out = append(out, e.ModelID)
	}
	return out
}

func switches(on map[string]bool) func(string) (bool, bool) {
	return func(id string) (bool, bool) {
		v, ok := on[id]
		return v, ok
	}
}

// The verifier ran agy 1.5.12 against mock catalogs and recorded which ids
// needed a placeholder (MODEL_PLACEHOLDER_Mnnn) and which did not. The tables
// must reproduce it: first-probe slots and built-in enums alike.
func TestBuiltinTablesMatchObservedAgyBehaviour(t *testing.T) {
	for id, want := range map[string]int{
		"gemini-4-neon-xhigh": 363, "gemini-4-neon": 335, "gemini-3.8-flash-tiered": 339,
		"gemini-3.5-flash-lite": 362, "claude-sonnet-5-5": 356,
	} {
		if _, builtin := BuiltinEnum(id); builtin || placeholderBase-1000+slotHash(id) != want {
			t.Errorf("%s: builtin %v, first slot M%d, want M%d", id, builtin, placeholderBase-1000+slotHash(id), want)
		}
	}
	for id, want := range map[string]int{
		"gemini-3-flash": 1018, "claude-opus-5-5-high": 1402, "claude-sonnet-5-5-high": 1405,
		"gemini-3.1-flash-lite": 1050, "gemini-4-argon": 1418, "gemini-4-argon-high": 1417,
		"gemini-4-hydrogen": 1462, "gemini-4-hydrogen-xhigh": 1464, "gemini-3.8-flash": 1319, "gemini-3.8-flash-high": 1318,
		"models/gemini-3.1-flash-lite": 1050, "gemini-3.1-flash-lite-preview": 1050, "gemini-3.6-flash-high": 1071,
	} {
		if enum, ok := BuiltinEnum(id); !ok || enum != want {
			t.Errorf("%s: enum %d %v, want %d", id, enum, ok, want)
		}
	}
	if len(builtinIDEnum) != 36 || len(wireAliases) != 25 {
		t.Fatalf("tables: %d ids, %d aliases", len(builtinIDEnum), len(wireAliases))
	}
	for _, id := range []string{"gemini-3.1-flash-image", "grok-imagine-image", "imagen-4.0", "dall-e-3", "gemini-3-pro-image-preview", "x-image-gen"} {
		if !HiddenAsImage(id) {
			t.Errorf("%s: agy would drop it as an image model", id)
		}
	}
	for _, id := range []string{"gpt-image-2", "grok-imagine-video", "grok-tts", "gemini-4-argon"} {
		if HiddenAsImage(id) {
			t.Errorf("%s: agy does not drop it", id)
		}
	}
	if WireID("gemini-4-neon-xhigh") != "gemini-4-neon" || WireID("grok-4.7") != "grok-4.7" {
		t.Fatal("wire ids")
	}
}

func TestParseGeminiList(t *testing.T) {
	models := fixture(t)
	if models[0].ID != "gpt-5.6-terra" || models[0].InputTokenLimit != 921000 || models[0].OutputTokenLimit != 128000 || models[0].DisplayName != "GPT 5.6 Terra" {
		t.Fatalf("first = %+v", models[0])
	}
	for name, body := range map[string]string{
		"empty":         `{"models":[]}`,
		"codex catalog": `{"models":[{"slug":"gpt-6-sol"}]}`,
		"openai":        `{"object":"list","data":[{"id":"x"}]}`,
		"not an object": `[]`,
		"trailing":      `{"models":[{"name":"models/x"}]} {}`,
		"bad entry":     `{"models":["x"]}`,
		"malformed":     `{"models":[`,
	} {
		if _, ok := ParseGeminiList([]byte(body)); ok {
			t.Errorf("%s accepted", name)
		}
	}
	got, ok := ParseGeminiList([]byte(`{"models":[{"name":"models/a"},{"name":"a"},{"name":""},{"name":"b"}]}`))
	if !ok || len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("dedupe = %+v", got)
	}
}

func TestNonTextByName(t *testing.T) {
	var nonText []string
	for _, m := range fixture(t) {
		if NonText(m) {
			nonText = append(nonText, m.ID)
		}
	}
	// CPA v8.0.73: 18 image, video, and speech models, including
	// gemini-3.1-flash-image (text and image output).
	if len(nonText) != 18 || !slices.Contains(nonText, "gemini-3.1-flash-image") || !slices.Contains(nonText, "grok-voice-tts-1.0") ||
		!slices.Contains(nonText, "grok-imagine-video-1.5-lite") {
		t.Fatalf("non-text = %v", nonText)
	}
	for _, id := range []string{"text-embedding-3-large", "gemini-embedding-001", "veo-3", "whisper-1", "omni-moderation-latest", "gpt-4o-mini-tts", "gpt-4o-audio-preview", "imagen-4.0", "dall-e-3"} {
		if !NonText(Model{ID: id}) {
			t.Errorf("%s is not text", id)
		}
	}
	for _, id := range []string{"gemini-4-argon", "gpt-6-sol", "claude-opus-5-5", "grok-4.7", "codex-auto-review"} {
		if NonText(Model{ID: id}) {
			t.Errorf("%s is text", id)
		}
	}
	if !NonText(Model{ID: "music-1", OutputModalities: []string{"audio"}}) || NonText(Model{ID: "x", OutputModalities: []string{"TEXT"}}) {
		t.Fatal("modalities")
	}
}

func TestLabelsAreReadableAndNeverEmpty(t *testing.T) {
	for _, tc := range []struct{ id, name, want string }{
		{"gemini-4-argon-high", "Gemini 4 Argon", "Gemini 4 Argon (High)"},
		{"gemini-4-hydrogen-xhigh", "Gemini 4 Hydrogen", "Gemini 4 Hydrogen (XHigh)"},
		{"gemini-4-argon", "Gemini 4 Argon", "Gemini 4 Argon"},
		{"claude-opus-5-5-high", "Claude Opus 5.5 (High)", "Claude Opus 5.5 (High)"},
		{"gpt-oss-120b-medium", "GPT-OSS 120B (Medium)", "GPT-OSS 120B (Medium)"},
		{"x-low", "X Low", "X Low"},
		// No display name, or CPA's display name equal to the id (an alias
		// without one): agy's ACP would show a MODEL_PLACEHOLDER id, so the
		// id is put into words, with the effort once.
		{"bare-model", "", "Bare Model"},
		{"thing-high", "", "Thing (High)"},
		{"mp-echo", "mp-echo", "Mp Echo"},
		{"or-kimi-k2", "or-kimi-k2", "Or Kimi K2"},
		{"mp-space", "mp-space ", "Mp Space"},
		{"gemini-4-neon-xhigh", "gemini-4-neon-xhigh", "Gemini 4 Neon (XHigh)"},
		{"gemini-4-neon-xhigh", "", "Gemini 4 Neon (XHigh)"},
		{"x", "models/x", "X"},
		{"moonshotai/kimi_k2", "", "Moonshotai Kimi K2"},
		// A name that differs from the id keeps its form.
		{"mp-case", "MP-CASE", "MP-CASE"},
		{"gemini-4-neon-xhigh", "Gemini-4-Neon-XHigh", "Gemini-4-Neon-XHigh"},
		// Ids that read the same in words.
		{"GPT4", "", "GPT4 (model)"},
		{"GPT4", "GPT4", "GPT4 (model)"},
		{"-", "", "- (model)"},
	} {
		if got := Label(Model{ID: tc.id, DisplayName: tc.name}); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.id, got, tc.want)
		}
	}
}

// agy 1.5.12's ACP lists an entry whose displayName equals its modelId (after
// trimming) under a shifting MODEL_PLACEHOLDER id, so no label may equal its id.
func TestLabelNeverEqualsTheID(t *testing.T) {
	for _, m := range fixture(t) {
		for _, name := range []string{m.DisplayName, "", m.ID, " " + m.ID + " ", "models/" + m.ID} {
			if label := Label(Model{ID: m.ID, DisplayName: name}); label == "" || strings.TrimSpace(label) == m.ID {
				t.Errorf("%s with display name %q: label %q", m.ID, name, label)
			}
		}
	}
}

func TestTokenLimits(t *testing.T) {
	for _, tc := range []struct {
		id              string
		in, out         int
		wantIn, wantOut int
		kind            string
	}{
		// agy's own limits for ids it knows: never sent.
		{"gemini-4-argon-high", 1048576, 1048576, 0, 0, TokensBuiltin},
		{"gemini-3-flash", 1048576, 65536, 0, 0, TokensBuiltin},
		{"claude-opus-5-5-high", 1000000, 128000, 0, 0, TokensBuiltin},
		// CPA advertises output == input: clamped.
		{"gemini-4-neon-xhigh", 1048576, 1048576, 1048576, 65536, TokensClamped},
		{"grok-4.7", 500000, 500000, 500000, 65536, TokensClamped},
		{"grok-build-0.1", 256000, 256000, 256000, 65536, TokensClamped},
		// 499999 of 500000 still fails in agy: anything above half is clamped.
		{"almost", 500000, 250001, 500000, 65536, TokensClamped},
		{"small-context", 100000, 80000, 100000, 50000, TokensClamped},
		// Safe limits pass unchanged.
		{"gpt-6.1-sol", 272000, 128000, 272000, 128000, TokensPassed},
		{"grok-4.5", 500000, 65536, 500000, 65536, TokensPassed},
		{"half", 500000, 250000, 500000, 250000, TokensPassed},
		// No limits from CPA: none sent; output alone is bounded.
		{"or-glm-5-3-flash", 0, 0, 0, 0, TokensNone},
		{"output-only", 0, 200000, 0, 65536, TokensClamped},
		{"output-only-small", 0, 8192, 0, 8192, TokensPassed},
		{"input-only", 128000, 0, 128000, 0, TokensPassed},
	} {
		in, out, kind := Tokens(Model{ID: tc.id, InputTokenLimit: tc.in, OutputTokenLimit: tc.out})
		if in != tc.wantIn || out != tc.wantOut || kind != tc.kind {
			t.Errorf("%s: %d/%d %s, want %d/%d %s", tc.id, in, out, kind, tc.wantIn, tc.wantOut, tc.kind)
		}
	}
}

func TestThinkingBudgetKeepsTheEffortOfAliasedIds(t *testing.T) {
	for id, want := range map[string]int{"gemini-4-neon-xhigh": 32768, "gemini-4-neon-high": 24576, "gemini-4-neon-medium": 8192, "gemini-4-neon-low": 1024} {
		if got, ok := ThinkingBudget(id); !ok || got != want {
			t.Errorf("%s: %d %v", id, got, ok)
		}
	}
	// Built-ins carry their own level; unaliased ids are sent as listed;
	// gemini-3.1-pro-high's alias does not end in a level.
	for _, id := range []string{"gemini-4-argon-high", "gemini-4-hydrogen-xhigh", "gemini-4-neon", "grok-4.7", "claude-opus-5-5-high", "gemini-3.1-pro-high"} {
		if got, ok := ThinkingBudget(id); ok {
			t.Errorf("%s: %d", id, got)
		}
	}
}

func TestBuildDefaultSelectionServesGeminiTextModels(t *testing.T) {
	r := Build(fixture(t), Selection{})
	w := decode(t, r.Body)
	// By id, so the default and title model do not change when CPA reloads.
	want := []string{"gemini-3-flash", "gemini-3.1-flash-lite", "gemini-3.5-flash-lite", "gemini-3.8-flash", "gemini-3.8-flash-high", "gemini-3.8-flash-tiered", "gemini-4-argon", "gemini-4-argon-high", "gemini-4-hydrogen", "gemini-4-hydrogen-xhigh", "gemini-4-neon", "gemini-4-neon-xhigh"}
	if !r.Preview.Fallback || !slices.Equal(ids(w), want) || w.DefaultModelID != "gemini-3-flash" {
		t.Fatalf("fallback = %v %v default %s", r.Preview.Fallback, ids(w), w.DefaultModelID)
	}
	if r.Preview.TitleModelID != "gemini-3.1-flash-lite" || r.Preview.SlotsUsed != 4 {
		t.Fatalf("preview = %+v", r.Preview)
	}
}

func TestBuildFieldsPerEntry(t *testing.T) {
	r := Build(fixture(t), Selection{Switched: switches(map[string]bool{
		"gemini-4-argon-high": true, "gemini-4-neon-xhigh": true, "grok-4.7": true, "or-glm-5-3-flash": true, "gpt-6.1-sol": true,
	})})
	w := decode(t, r.Body)
	got := map[string]string{}
	for _, e := range w.List {
		got[e.ModelID] = fmt.Sprintf("%s|%v|%v|%v|%v", e.DisplayName, deref(e.MaxTokens), deref(e.MaxOutputTokens), derefBool(e.SupportsThinking), deref(e.ThinkingBudget))
	}
	want := map[string]string{
		"gemini-4-argon-high": "Gemini 4 Argon (High)|-|-|-|-",
		"gemini-4-neon-xhigh": "Gemini 4 Neon (XHigh)|1048576|65536|true|32768",
		"grok-4.7":            "Grok 4.7|500000|65536|-|-",
		"or-glm-5-3-flash":    "z-ai/glm-5.3-flash|-|-|-|-",
		"gpt-6.1-sol":         "GPT 6.1 Sol|272000|128000|-|-",
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s = %s, want %s", id, got[id], w)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("list = %v", got)
	}
	// Sorted by id, byte for byte.
	if string(r.Body) != `{"defaultModelId":"gemini-4-argon-high","list":[{"modelId":"gemini-4-argon-high","displayName":"Gemini 4 Argon (High)"},{"modelId":"gemini-4-neon-xhigh","displayName":"Gemini 4 Neon (XHigh)","maxTokens":1048576,"maxOutputTokens":65536,"supportsThinking":true,"thinkingBudget":32768},{"modelId":"gpt-6.1-sol","displayName":"GPT 6.1 Sol","maxTokens":272000,"maxOutputTokens":128000},{"modelId":"grok-4.7","displayName":"Grok 4.7","maxTokens":500000,"maxOutputTokens":65536},{"modelId":"or-glm-5-3-flash","displayName":"z-ai/glm-5.3-flash"}]}` {
		t.Fatalf("body = %s", r.Body)
	}
}

func deref(p *int) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprint(*p)
}

func derefBool(p *bool) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprint(*p)
}

func TestBuildOrdersDefaultAndTitle(t *testing.T) {
	// CPA's order; the list is sorted by id:
	// gemini-3-flash, gemini-3.1-flash-lite, gemini-3.5-flash-lite, gemini-4-argon, gpt-6-sol.
	models := []Model{{ID: "gpt-6-sol"}, {ID: "gemini-3.5-flash-lite"}, {ID: "gemini-3-flash"}, {ID: "gemini-3.1-flash-lite"}, {ID: "gemini-4-argon"}}
	all := Selection{EnableNew: true}
	byID := []string{"gemini-3-flash", "gemini-3.1-flash-lite", "gemini-3.5-flash-lite", "gemini-4-argon", "gpt-6-sol"}
	for _, tc := range []struct {
		name, def, title string
		want             []string
		wantTitle        string
		warn             string
	}{
		{"id order", "", "", byID, "gemini-3.1-flash-lite", ""},
		{"default first", "gpt-6-sol", "", []string{"gpt-6-sol", "gemini-3-flash", "gemini-3.1-flash-lite", "gemini-3.5-flash-lite", "gemini-4-argon"}, "gemini-3.1-flash-lite", ""},
		{"title before other flash-lite", "", "gemini-3.5-flash-lite", []string{"gemini-3-flash", "gemini-3.5-flash-lite", "gemini-3.1-flash-lite", "gemini-4-argon", "gpt-6-sol"}, "gemini-3.5-flash-lite", ""},
		{"both", "gpt-6-sol", "gemini-3.5-flash-lite", []string{"gpt-6-sol", "gemini-3-flash", "gemini-3.5-flash-lite", "gemini-3.1-flash-lite", "gemini-4-argon"}, "gemini-3.5-flash-lite", ""},
		{"title already first", "", "gemini-3.1-flash-lite", byID, "gemini-3.1-flash-lite", ""},
		{"default is flash-lite", "gemini-3.1-flash-lite", "gemini-3.5-flash-lite", []string{"gemini-3.1-flash-lite", "gemini-3-flash", "gemini-3.5-flash-lite", "gemini-4-argon", "gpt-6-sol"}, "gemini-3.1-flash-lite", "default model also contains flash-lite"},
		{"title without flash-lite", "", "gemini-3-flash", byID, "gemini-3.1-flash-lite", "not used for titles"},
		{"unknown default", "nope", "", byID, "gemini-3.1-flash-lite", "default model nope is not in the list, so gemini-3-flash is the default"},
	} {
		sel := all
		sel.DefaultModel, sel.TitleModel = tc.def, tc.title
		r := Build(models, sel)
		w := decode(t, r.Body)
		if !slices.Equal(ids(w), tc.want) || w.DefaultModelID != tc.want[0] || r.Preview.TitleModelID != tc.wantTitle {
			t.Errorf("%s: %v default %s title %s", tc.name, ids(w), w.DefaultModelID, r.Preview.TitleModelID)
		}
		if tc.warn != "" && !strings.Contains(strings.Join(r.Preview.Warnings, " "), tc.warn) {
			t.Errorf("%s: warnings %v", tc.name, r.Preview.Warnings)
		}
	}
}

// CPA rebuilds its list in Go map order on every configuration reload, so the
// list agy receives, its default, title model, and slot cut-off must not
// depend on the order CPA lists models in.
func TestBuildIgnoresCPAsOrder(t *testing.T) {
	models := fixture(t)
	var many []Model
	for i := range 40 {
		many = append(many, Model{ID: fmt.Sprintf("custom-%02d", i)})
	}
	for _, sel := range []Selection{
		{},
		{EnableNew: true},
		{EnableNew: true, DefaultModel: "grok-4.7", TitleModel: "gemini-3.5-flash-lite"},
		{Switched: switches(map[string]bool{"gemini-3.1-flash-image": true, "grok-imagine-image": true})},
	} {
		for _, list := range [][]Model{models, append(slices.Clone(models), many...)} {
			want := Build(list, sel)
			for seed := range 5 {
				shuffled := slices.Clone(list)
				for i := range shuffled {
					j := (i*7 + seed*13) % len(shuffled)
					shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
				}
				slices.Reverse(shuffled)
				got := Build(shuffled, sel)
				if string(got.Body) != string(want.Body) || !slices.Equal(got.Preview.Skipped, want.Preview.Skipped) || !slices.Equal(got.Preview.Warnings, want.Preview.Warnings) {
					t.Fatalf("%+v: order %d changed the list:\n%s\n%s", sel, seed, got.Body, want.Body)
				}
			}
		}
	}
}

func TestDefaultAndTitleAreIncludedUnlessSwitchedOff(t *testing.T) {
	models := []Model{{ID: "gpt-6-sol"}, {ID: "gemini-3.1-flash-lite"}, {ID: "gemini-4-argon"}}
	sel := Selection{DefaultModel: "gemini-4-argon", TitleModel: "gemini-3.1-flash-lite", Switched: switches(map[string]bool{"gpt-6-sol": true})}
	if w := decode(t, Build(models, sel).Body); !slices.Equal(ids(w), []string{"gemini-4-argon", "gemini-3.1-flash-lite", "gpt-6-sol"}) {
		t.Fatalf("list = %v", ids(w))
	}
	sel.Switched = switches(map[string]bool{"gpt-6-sol": true, "gemini-4-argon": false})
	r := Build(models, sel)
	if w := decode(t, r.Body); !slices.Equal(ids(w), []string{"gemini-3.1-flash-lite", "gpt-6-sol"}) || w.DefaultModelID != "gemini-3.1-flash-lite" {
		t.Fatalf("list = %v", ids(w))
	}
}

func TestNonTextModelsNeedAnExplicitSwitch(t *testing.T) {
	models := []Model{{ID: "gpt-6-sol"}, {ID: "gpt-image-2"}, {ID: "grok-tts"}}
	if w := decode(t, Build(models, Selection{EnableNew: true}).Body); !slices.Equal(ids(w), []string{"gpt-6-sol"}) {
		t.Fatalf("new models = %v", ids(w))
	}
	r := Build(models, Selection{EnableNew: true, Switched: switches(map[string]bool{"gpt-image-2": true})})
	if w := decode(t, r.Body); !slices.Equal(ids(w), []string{"gpt-6-sol", "gpt-image-2"}) || !r.Preview.Entries[1].NonText {
		t.Fatalf("switched on = %v", ids(w))
	}
}

func TestSlotCapKeepsTheFirst32AndReportsTheRest(t *testing.T) {
	var models []Model
	for i := range 40 {
		models = append(models, Model{ID: fmt.Sprintf("custom-%02d", i)})
	}
	// Built-ins never use a slot, wherever they are.
	models = append(models, Model{ID: "gemini-4-argon"}, Model{ID: "gemini-3.1-flash-lite"})
	r := Build(models, Selection{EnableNew: true})
	w := decode(t, r.Body)
	if len(w.List) != 34 || r.Preview.SlotsUsed != 32 || len(r.Preview.Skipped) != 8 || r.Preview.Skipped[0] != "custom-32" || w.List[33].ModelID != "gemini-4-argon" {
		t.Fatalf("kept %d, slots %d, skipped %v", len(w.List), r.Preview.SlotsUsed, r.Preview.Skipped)
	}
	placeholders := map[int]bool{}
	for _, e := range r.Preview.Entries {
		if e.Slot == SlotPlaceholder {
			if e.Placeholder < 334 || e.Placeholder > 365 || placeholders[e.Placeholder] {
				t.Fatalf("placeholder %d for %s", e.Placeholder, e.ID)
			}
			placeholders[e.Placeholder] = true
		}
	}
}

func TestBuiltinsSharingAnEnumTakeASlot(t *testing.T) {
	// gemini-3.8-flash and gemini-3.8-flash-medium are both enum 1319.
	r := Build([]Model{{ID: "gemini-3.8-flash"}, {ID: "gemini-3.8-flash-medium"}, {ID: "gpt-image-2"}, {ID: "gemini-3.1-flash-image"}}, Selection{EnableNew: true, Switched: switches(map[string]bool{"gemini-3.1-flash-image": true})})
	slots := []string{}
	for _, e := range r.Preview.Entries {
		slots = append(slots, e.ID+":"+e.Slot)
	}
	if !slices.Equal(slots, []string{"gemini-3.1-flash-image:image", "gemini-3.8-flash:builtin", "gemini-3.8-flash-medium:placeholder"}) || r.Preview.SlotsUsed != 1 {
		t.Fatalf("slots = %v", slots)
	}
}

func TestDuplicateLabelsAreDisambiguated(t *testing.T) {
	w := decode(t, Build([]Model{{ID: "gpt-6-astra", DisplayName: "GPT 6.0 Astra"}, {ID: "cpa-gpt-6-astra", DisplayName: "GPT 6.0 Astra"}}, Selection{EnableNew: true}).Body)
	// By id, cpa-gpt-6-astra comes first and keeps the plain label.
	if w.List[0].DisplayName != "GPT 6.0 Astra" || w.List[1].DisplayName != "GPT 6.0 Astra (gpt-6-astra)" {
		t.Fatalf("labels = %+v", w.List)
	}
}

func TestImageModelIsNeverTheDefault(t *testing.T) {
	r := Build([]Model{{ID: "grok-imagine-image"}, {ID: "gpt-6-sol"}}, Selection{Switched: switches(map[string]bool{"grok-imagine-image": true, "gpt-6-sol": true})})
	if w := decode(t, r.Body); w.DefaultModelID != "gpt-6-sol" {
		t.Fatalf("default = %s", w.DefaultModelID)
	}
}

// agy drops image models itself, so a list of only those would leave it with
// nothing and send it to its random Tier 3 list: the fallback is served.
func TestOnlyImageModelsSwitchedOnServesTheFallback(t *testing.T) {
	models := []Model{{ID: "gemini-3.1-flash-image"}, {ID: "grok-imagine-image"}, {ID: "gemini-4-neon"}, {ID: "gpt-6-sol"}}
	for name, sel := range map[string]Selection{
		"switched on":   {Switched: switches(map[string]bool{"gemini-3.1-flash-image": true, "grok-imagine-image": true})},
		"default model": {DefaultModel: "grok-imagine-image"},
	} {
		r := Build(models, sel)
		w := decode(t, r.Body)
		if !r.Preview.Fallback || !slices.Equal(ids(w), []string{"gemini-4-neon"}) || w.DefaultModelID != "gemini-4-neon" {
			t.Fatalf("%s: fallback %v, list %v", name, r.Preview.Fallback, ids(w))
		}
		if !strings.Contains(strings.Join(r.Preview.Warnings, " "), "Every switched-on model is an image model agy hides") {
			t.Fatalf("%s: warnings %v", name, r.Preview.Warnings)
		}
	}
	// An image default beside a text model: the text model is the default.
	r := Build(models, Selection{DefaultModel: "grok-imagine-image", Switched: switches(map[string]bool{"gpt-6-sol": true})})
	if w := decode(t, r.Body); r.Preview.Fallback || !slices.Equal(ids(w), []string{"grok-imagine-image", "gpt-6-sol"}) || w.DefaultModelID != "gpt-6-sol" ||
		!strings.Contains(strings.Join(r.Preview.Warnings, " "), "grok-imagine-image is an image model agy hides, so gpt-6-sol is the default") {
		t.Fatalf("image default: %v %s %v", ids(w), w.DefaultModelID, r.Preview.Warnings)
	}
}

func TestBuildNeverEmitsAnEmptyList(t *testing.T) {
	// Only non-text models: nothing to serve, so no body at all.
	if r := Build([]Model{{ID: "grok-tts"}}, Selection{}); r.Body != nil {
		t.Fatalf("body = %s", r.Body)
	}
	// No Gemini text model: any text model is the fallback.
	r := Build([]Model{{ID: "grok-tts"}, {ID: "gpt-6-sol"}}, Selection{})
	if w := decode(t, r.Body); !slices.Equal(ids(w), []string{"gpt-6-sol"}) || !r.Preview.Fallback {
		t.Fatalf("fallback = %v", ids(w))
	}
	// Everything switched off: still the fallback.
	r = Build([]Model{{ID: "gemini-4-argon"}}, Selection{EnableNew: true, Switched: switches(map[string]bool{"gemini-4-argon": false})})
	if w := decode(t, r.Body); !slices.Equal(ids(w), []string{"gemini-4-argon"}) || !r.Preview.Fallback {
		t.Fatalf("all off = %v", ids(w))
	}
}

func TestBuildDoesNotEscapeHTMLInLabels(t *testing.T) {
	r := Build([]Model{{ID: "a", DisplayName: "A <b> & c"}}, Selection{EnableNew: true})
	if !strings.Contains(string(r.Body), `"displayName":"A <b> & c"`) {
		t.Fatalf("body = %s", r.Body)
	}
}

func FuzzBuild(f *testing.F) {
	raw, err := os.ReadFile("testdata/cpa-v1beta-models.json")
	if err != nil {
		f.Fatal(err)
	}
	f.Add(raw, "gemini-4-argon", "gemini-3.1-flash-lite", true)
	f.Add([]byte(`{"models":[{"name":"models/x"}]}`), "", "", false)
	f.Fuzz(func(t *testing.T, body []byte, def, title string, enableNew bool) {
		models, ok := ParseGeminiList(body)
		if !ok {
			return
		}
		r := Build(models, Selection{EnableNew: enableNew, DefaultModel: def, TitleModel: title})
		if r.Body == nil {
			return
		}
		w := decode(t, r.Body)
		if len(w.List) == 0 || w.DefaultModelID == "" || r.Preview.SlotsUsed > PlaceholderSlots {
			t.Fatalf("invalid catalog %s", r.Body)
		}
		seen := map[string]bool{}
		shown := false
		for _, e := range w.List {
			if e.ModelID == "" || e.DisplayName == "" || strings.TrimSpace(e.DisplayName) == e.ModelID || seen[e.ModelID] {
				t.Fatalf("bad entry in %s", r.Body)
			}
			seen[e.ModelID] = true
			shown = shown || !HiddenAsImage(e.ModelID)
		}
		if !seen[w.DefaultModelID] || !shown || HiddenAsImage(w.DefaultModelID) {
			t.Fatalf("default not listed, or nothing agy shows: %s", r.Body)
		}
		reversed := slices.Clone(models)
		slices.Reverse(reversed)
		if again := Build(reversed, Selection{EnableNew: enableNew, DefaultModel: def, TitleModel: title}); string(again.Body) != string(r.Body) {
			t.Fatalf("CPA's order changed the list: %s, %s", r.Body, again.Body)
		}
	})
}
