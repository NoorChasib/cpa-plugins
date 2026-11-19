// Package agy turns CPA's Gemini-format model list (/v1beta/models) into the
// catalog Antigravity CLI (agy) reads from models.listSource:
//
//	{"defaultModelId":"<id>","list":[{"modelId":"<id>","displayName":"..."},...]}
//
// agy accepts only that shape (or a bare array); anything else, an empty list,
// or an error makes it fall back to discovering CPA's whole list. The builder
// therefore never produces an empty list.
package agy

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
)

// Model is one entry of CPA's /v1beta/models, as kept for the settings page
// and for rebuilding the list while CPA is unavailable. It holds no secrets.
type Model struct {
	ID               string   `json:"id"`
	DisplayName      string   `json:"display_name,omitempty"`
	Description      string   `json:"description,omitempty"`
	InputTokenLimit  int      `json:"input_token_limit,omitempty"`
	OutputTokenLimit int      `json:"output_token_limit,omitempty"`
	OutputModalities []string `json:"output_modalities,omitempty"`
}

const (
	maxModels   = 5000
	maxIDBytes  = 256
	maxTextSize = 1024
)

// ParseGeminiList reads CPA's /v1beta/models body: {"models":[{"name":
// "models/<id>", "displayName", "description", "inputTokenLimit",
// "outputTokenLimit", "supportedOutputModalities"}, ...]}. It keeps CPA's
// order, drops later duplicates, and reports false for anything else.
func ParseGeminiList(body []byte) ([]Model, bool) {
	var doc struct {
		Models []json.RawMessage `json:"models"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if dec.Decode(&doc) != nil || len(doc.Models) == 0 || len(doc.Models) > maxModels {
		return nil, false
	}
	if _, err := dec.Token(); err == nil {
		return nil, false // trailing data
	}
	out := make([]Model, 0, len(doc.Models))
	seen := make(map[string]bool, len(doc.Models))
	for _, raw := range doc.Models {
		var entry struct {
			Name             string   `json:"name"`
			DisplayName      string   `json:"displayName"`
			Description      string   `json:"description"`
			InputTokenLimit  int      `json:"inputTokenLimit"`
			OutputTokenLimit int      `json:"outputTokenLimit"`
			OutputModalities []string `json:"supportedOutputModalities"`
		}
		if json.Unmarshal(raw, &entry) != nil {
			return nil, false
		}
		id := strings.TrimPrefix(strings.TrimSpace(entry.Name), "models/")
		if id == "" || len(id) > maxIDBytes || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, Model{
			ID:               id,
			DisplayName:      clip(strings.TrimSpace(entry.DisplayName)),
			Description:      clip(strings.TrimSpace(entry.Description)),
			InputTokenLimit:  max(entry.InputTokenLimit, 0),
			OutputTokenLimit: max(entry.OutputTokenLimit, 0),
			OutputModalities: entry.OutputModalities,
		})
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

func clip(s string) string {
	if len(s) <= maxTextSize {
		return s
	}
	return s[:maxTextSize]
}

// nonTextWords are id tokens that mark a model that does not answer in text.
// CPA advertises generateContent for every model, so methods cannot tell.
var nonTextWords = []string{"tts", "voice", "image", "imagine", "imagen", "dall", "video", "veo", "embedding", "embed", "audio", "whisper", "moderation"}

// NonText reports whether m is a speech, image, video, embedding, audio, or
// moderation model by name, or CPA lists output modalities without text.
// gemini-3.1-flash-image, which answers text and images, counts as non-text.
func NonText(m Model) bool {
	if len(m.OutputModalities) > 0 {
		text := false
		for _, modality := range m.OutputModalities {
			text = text || strings.EqualFold(modality, "text")
		}
		if !text {
			return true
		}
	}
	tokens := strings.FieldsFunc(strings.ToLower(m.ID), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	for _, token := range tokens {
		for _, word := range nonTextWords {
			if token == word || (len(word) >= 4 && strings.HasPrefix(token, word)) {
				return true
			}
		}
	}
	return false
}

var effortLabels = []struct{ suffix, label string }{
	{"-xhigh", "XHigh"}, {"-high", "High"}, {"-medium", "Medium"}, {"-low", "Low"},
}

// effortSuffix returns the effort suffix and label of an id such as
// gemini-4-argon-high ("-high", "High").
func effortSuffix(id string) (suffix, label string) {
	for _, effort := range effortLabels {
		if strings.HasSuffix(id, effort.suffix) && len(id) > len(effort.suffix) {
			return effort.suffix, effort.label
		}
	}
	return "", ""
}

// humanize turns an id into words: gemini-4-neon becomes Gemini 4 Neon.
func humanize(id string) string {
	words := strings.FieldsFunc(id, func(r rune) bool { return r == '-' || r == '_' || r == '/' || r == ' ' })
	for i, word := range words {
		if word[0] >= 'a' && word[0] <= 'z' {
			words[i] = string(word[0]-'a'+'A') + word[1:]
		}
	}
	return strings.Join(words, " ")
}

// Label is the displayName sent for m. agy 1.5.12's ACP (and so T3) treats a
// missing displayName, or one equal to the id after trimming, as no label and
// lists an id it has no label of its own for as an enum-style
// MODEL_PLACEHOLDER_Mnnn id, which shifts when the list changes. So the label
// always differs from the id: CPA's display name when it has a real one,
// otherwise the id in words (CPA uses the id itself as the display name of an
// alias without one). CPA's display name drops an effort suffix, so one taken
// from the id is appended.
func Label(m Model) string {
	name := strings.TrimSpace(m.DisplayName)
	suffix, effort := effortSuffix(m.ID)
	if name == "" || name == m.ID || name == "models/"+m.ID {
		name = humanize(strings.TrimSuffix(m.ID, suffix))
	}
	if effort != "" {
		lower := strings.ToLower(name)
		word := strings.ToLower(effort)
		if !strings.Contains(lower, "("+word+")") && !strings.HasSuffix(lower, " "+word) && !strings.HasSuffix(lower, "-"+word) {
			name += " (" + effort + ")"
		}
	}
	if name == "" || strings.TrimSpace(name) == m.ID {
		// An id without separators that starts with an upper-case letter or a
		// digit reads the same in words.
		name = m.ID + " (model)"
	}
	return name
}

// Token handling for one entry.
const (
	// TokensBuiltin: agy has its own limits for the id; none are sent.
	TokensBuiltin = "builtin"
	// TokensPassed: CPA's limits are sent unchanged.
	TokensPassed = "passed"
	// TokensClamped: CPA's output limit was lowered to a safe value.
	TokensClamped = "clamped"
	// TokensNone: CPA gives no limits, so none are sent.
	TokensNone = "none"

	// OutputClamp is the output limit sent when CPA's is unsafe for agy.
	OutputClamp = 65536
)

// Tokens decides maxTokens and maxOutputTokens for m. agy refuses to run a
// model whose output limit is close to its context window ("failed to
// construct executor: config.MaxTokenLimit ... cannot exceed"); CPA advertises
// output == input for gemini-4-* and several grok models. An output limit
// above half the input, or one without an input limit above OutputClamp, is
// clamped to OutputClamp (or half the input when that is smaller).
func Tokens(m Model) (maxTokens, maxOutputTokens int, kind string) {
	if _, ok := BuiltinEnum(m.ID); ok {
		return 0, 0, TokensBuiltin
	}
	in, out := m.InputTokenLimit, m.OutputTokenLimit
	if in <= 0 && out <= 0 {
		return 0, 0, TokensNone
	}
	kind = TokensPassed
	if in > 0 {
		maxTokens = in
	}
	if out > 0 {
		limit := OutputClamp
		if in > 0 && in/2 < limit {
			limit = in / 2
		}
		maxOutputTokens = out
		if (in > 0 && out > in/2) || (in <= 0 && out > OutputClamp) {
			maxOutputTokens, kind = min(out, limit), TokensClamped
		}
	}
	return maxTokens, maxOutputTokens, kind
}

// thinkingBudgets mirrors CPA's level -> budget table (internal/thinking
// levelToBudgetMap). CPA maps a budget back to the same level for level-only
// models (ConvertBudgetToLevel) and passes it on as a budget for hybrid ones.
var thinkingBudgets = map[string]int{"low": 1024, "medium": 8192, "high": 24576, "xhigh": 32768}

// ThinkingBudget returns the thinkingBudget to send for id, if any. agy sends
// an id it aliases (gemini-4-neon-xhigh) under its base name (gemini-4-neon),
// dropping the effort; for an id without built-in metadata it would then send
// a small default budget. Setting supportsThinking and the budget of the
// suffix's level keeps the effort.
func ThinkingBudget(id string) (int, bool) {
	if _, ok := BuiltinEnum(id); ok {
		return 0, false
	}
	base, ok := wireAliases[id]
	if !ok || !strings.HasPrefix(id, base+"-") {
		return 0, false
	}
	budget, ok := thinkingBudgets[strings.TrimPrefix(id, base+"-")]
	return budget, ok
}

// Selection is the antigravity client's configuration as the builder needs it.
type Selection struct {
	// Switched returns an explicit switch for id.
	Switched func(id string) (on, ok bool)
	// EnableNew decides an id without a switch.
	EnableNew    bool
	DefaultModel string
	TitleModel   string
}

func (s Selection) switched(id string) (bool, bool) {
	if s.Switched == nil {
		return false, false
	}
	return s.Switched(id)
}

// Selected reports whether id is chosen, before the fallback and slot cap: an
// explicit switch decides; otherwise the default and title models are in,
// non-text models are out, and anything else follows EnableNew.
func (s Selection) Selected(m Model) bool {
	if on, ok := s.switched(m.ID); ok {
		return on
	}
	if m.ID == s.DefaultModel || m.ID == s.TitleModel {
		return true
	}
	return s.EnableNew && !NonText(m)
}

// Slot kinds in a preview entry.
const (
	SlotBuiltin     = "builtin"
	SlotPlaceholder = "placeholder"
	SlotImage       = "image"
)

// Entry describes one model of the built list.
type Entry struct {
	ID              string `json:"id"`
	DisplayName     string `json:"display_name"`
	Slot            string `json:"slot"`
	Placeholder     int    `json:"placeholder,omitempty"`
	Tokens          string `json:"tokens"`
	MaxTokens       int    `json:"max_tokens,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
	ThinkingBudget  int    `json:"thinking_budget,omitempty"`
	NonText         bool   `json:"non_text,omitempty"`
}

// Preview describes a built list for the settings page.
type Preview struct {
	DefaultModelID string   `json:"default_model_id"`
	TitleModelID   string   `json:"title_model_id"`
	Fallback       bool     `json:"fallback"`
	SlotsUsed      int      `json:"slots_used"`
	SlotLimit      int      `json:"slot_limit"`
	Entries        []Entry  `json:"entries"`
	Skipped        []string `json:"skipped"`
	Warnings       []string `json:"warnings"`
}

// Result is a built agy catalog. Body is nil when there is nothing to serve.
type Result struct {
	Body    []byte
	Preview Preview
}

type wireEntry struct {
	ModelID          string `json:"modelId"`
	DisplayName      string `json:"displayName"`
	MaxTokens        int    `json:"maxTokens,omitempty"`
	MaxOutputTokens  int    `json:"maxOutputTokens,omitempty"`
	SupportsThinking bool   `json:"supportsThinking,omitempty"`
	ThinkingBudget   int    `json:"thinkingBudget,omitempty"`
}

type wireCatalog struct {
	DefaultModelID string      `json:"defaultModelId"`
	List           []wireEntry `json:"list"`
}

// Build selects, orders, and shapes models for agy:
//
//  1. Models are selected (Selection.Selected). If none is, or every selected
//     model is an image model agy hides, CPA's gemini-* text models are served
//     instead (then any text model), so agy never receives a list it shows
//     nothing of.
//  2. The list is sorted by id. CPA's own order is not stable: it builds its
//     list from a Go map and changes it on every configuration reload.
//  3. The default model comes first and is also defaultModelId; without one,
//     the first model by id that agy shows is the default.
//  4. The title model is moved ahead of every other id containing
//     "flash-lite": agy titles threads with the first such id in list order.
//  5. At most PlaceholderSlots ids that need a placeholder slot are kept, in
//     list order; the rest are reported in Preview.Skipped.
func Build(models []Model, sel Selection) Result {
	var list []Model
	for _, m := range models {
		if sel.Selected(m) {
			list = append(list, m)
		}
	}
	preview := Preview{SlotLimit: PlaceholderSlots, Entries: []Entry{}, Skipped: []string{}, Warnings: []string{}}
	if !slices.ContainsFunc(list, func(m Model) bool { return !HiddenAsImage(m.ID) }) {
		reason := "No model is switched on"
		if len(list) > 0 {
			reason = "Every switched-on model is an image model agy hides"
		}
		preview.Fallback, list = true, nil
		for _, gemini := range []bool{true, false} {
			for _, m := range models {
				if !NonText(m) && !HiddenAsImage(m.ID) && (!gemini || strings.HasPrefix(m.ID, "gemini-")) {
					list = append(list, m)
				}
			}
			if len(list) > 0 {
				break
			}
		}
		if len(list) > 0 {
			preview.Warnings = append(preview.Warnings, reason+", so CPA's Gemini text models are served instead.")
		}
	}
	if len(list) == 0 {
		return Result{Preview: preview}
	}
	// list is this call's own slice, so sorting never touches models.
	slices.SortFunc(list, func(a, b Model) int { return strings.Compare(a.ID, b.ID) })
	list = order(list, sel)

	used := map[int]bool{}
	kept := list[:0:0]
	for _, m := range list {
		entry := Entry{ID: m.ID, NonText: NonText(m)}
		switch enum, builtin := BuiltinEnum(m.ID); {
		case HiddenAsImage(m.ID):
			entry.Slot = SlotImage
		case builtin && !used[enum]:
			used[enum], entry.Slot = true, SlotBuiltin
		case preview.SlotsUsed >= PlaceholderSlots:
			preview.Skipped = append(preview.Skipped, m.ID)
			continue
		default:
			start := slotHash(m.ID)
			for i := range PlaceholderSlots {
				if enum := placeholderBase + (start+i)%PlaceholderSlots; !used[enum] {
					used[enum], entry.Slot, entry.Placeholder = true, SlotPlaceholder, enum-1000
					break
				}
			}
			preview.SlotsUsed++
		}
		entry.MaxTokens, entry.MaxOutputTokens, entry.Tokens = Tokens(m)
		entry.ThinkingBudget, _ = ThinkingBudget(m.ID)
		preview.Entries = append(preview.Entries, entry)
		kept = append(kept, m)
	}

	labels := map[string]bool{}
	wire := wireCatalog{DefaultModelID: kept[0].ID, List: make([]wireEntry, 0, len(kept))}
	for i, m := range kept {
		// agy drops image models, so one cannot be its default.
		if preview.Entries[i].Slot != SlotImage {
			wire.DefaultModelID = m.ID
			break
		}
	}
	if sel.DefaultModel != "" && sel.DefaultModel != wire.DefaultModelID {
		why := " is not in the list"
		if slices.ContainsFunc(kept, func(m Model) bool { return m.ID == sel.DefaultModel }) {
			why = " is an image model agy hides"
		}
		preview.Warnings = append(preview.Warnings, "The default model "+sel.DefaultModel+why+", so "+wire.DefaultModelID+" is the default.")
	}
	if len(preview.Skipped) > 0 {
		preview.Warnings = append(preview.Warnings, strconv.Itoa(len(preview.Skipped))+" switched-on models are left out: agy shows at most 32 models it has no built-in entry for.")
	}
	for i, m := range kept {
		label := Label(m)
		if labels[label] {
			label += " (" + m.ID + ")"
		}
		labels[label] = true
		entry := &preview.Entries[i]
		entry.DisplayName = label
		w := wireEntry{ModelID: m.ID, DisplayName: label, MaxTokens: entry.MaxTokens, MaxOutputTokens: entry.MaxOutputTokens}
		if entry.ThinkingBudget > 0 {
			w.SupportsThinking, w.ThinkingBudget = true, entry.ThinkingBudget
		}
		wire.List = append(wire.List, w)
	}
	preview.DefaultModelID = wire.DefaultModelID
	for _, m := range kept {
		if strings.Contains(m.ID, "flash-lite") {
			preview.TitleModelID = m.ID
			break
		}
	}
	if sel.TitleModel != "" && preview.TitleModelID != sel.TitleModel {
		preview.Warnings = append(preview.Warnings, titleWarning(sel.TitleModel, kept))
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if enc.Encode(wire) != nil {
		return Result{Preview: preview}
	}
	return Result{Body: bytes.TrimSuffix(out.Bytes(), []byte("\n")), Preview: preview}
}

// order puts the default model first and the title model ahead of the other
// flash-lite ids, keeping the id order otherwise.
func order(list []Model, sel Selection) []Model {
	out := make([]Model, 0, len(list))
	index := func(id string) int {
		for i, m := range out {
			if m.ID == id {
				return i
			}
		}
		return -1
	}
	out = append(out, list...)
	if i := index(sel.DefaultModel); sel.DefaultModel != "" && i > 0 {
		m := out[i]
		copy(out[1:i+1], out[:i])
		out[0] = m
	}
	// Only an id containing flash-lite can be agy's title model; moving any
	// other id would change nothing but its flash or pro tier.
	if strings.Contains(sel.TitleModel, "flash-lite") && sel.TitleModel != out[0].ID {
		if i := index(sel.TitleModel); i > 0 {
			first := -1
			for j := 1; j < i; j++ {
				if strings.Contains(out[j].ID, "flash-lite") {
					first = j
					break
				}
			}
			if first > 0 {
				m := out[i]
				copy(out[first+1:i+1], out[first:i])
				out[first] = m
			}
		}
	}
	return out
}

func titleWarning(title string, kept []Model) string {
	inList := false
	for _, m := range kept {
		inList = inList || m.ID == title
	}
	switch {
	case !inList:
		return "The title model " + title + " is not in the list."
	case !strings.Contains(title, "flash-lite"):
		return "agy titles threads with the first model whose id contains flash-lite, so " + title + " is not used for titles."
	default:
		return "The default model also contains flash-lite and comes first, so agy titles threads with it."
	}
}
