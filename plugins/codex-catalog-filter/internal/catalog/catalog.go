// Package catalog filters Codex-native model catalogs:
//
//	{"models":[{"slug":"gpt-6-sol","visibility":"list",...},...]}
//
// Codex shows every entry whose visibility is "list" in its model picker. The
// rewrite splices bytes rather than re-encoding: kept entries, the separators
// between them, and everything outside the models array are copied verbatim.
package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Action selects what happens to an entry the rules do not allow.
type Action string

const (
	// Remove drops the entry from the models array.
	Remove Action = "remove"
	// Hide keeps the entry and sets its visibility to "hide", so an explicitly
	// selected model still gets CPA's metadata but is absent from the picker.
	Hide Action = "hide"
)

// Rules decides which catalog entries stay as CPA sent them. Immutable once built.
type Rules struct {
	models map[string]bool
	// enableNew decides every slug without a switch in models.
	enableNew bool
	action    Action
	idle      bool
}

// NewRules builds rules from per-model switches: true keeps a slug, false
// disables it, and a slug without a switch follows enableNew.
func NewRules(models map[string]bool, enableNew bool, action Action) (*Rules, error) {
	if action != Remove && action != Hide {
		return nil, fmt.Errorf("unsupported action %q", action)
	}
	r := &Rules{models: make(map[string]bool, len(models)), enableNew: enableNew, action: action, idle: enableNew}
	for slug, enabled := range models {
		r.models[slug] = enabled
		if !enabled {
			r.idle = false
		}
	}
	return r, nil
}

// Idle reports whether these rules can never change a catalog: every model is
// enabled unless switched off, and none is.
func (r *Rules) Idle() bool { return r == nil || r.idle }

// Allows reports whether slug stays in Codex's catalog as CPA listed it.
func (r *Rules) Allows(slug string) bool {
	if enabled, ok := r.models[slug]; ok {
		return enabled
	}
	return r.enableNew
}

// Switched reports whether slug has an explicit switch, and its value.
func (r *Rules) Switched(slug string) (enabled, ok bool) {
	enabled, ok = r.models[slug]
	return enabled, ok
}

// Switches returns a copy of the explicit per-model switches.
func (r *Rules) Switches() map[string]bool {
	out := make(map[string]bool, len(r.models))
	for slug, enabled := range r.models {
		out[slug] = enabled
	}
	return out
}

// EnableNew reports what happens to a slug without a switch.
func (r *Rules) EnableNew() bool { return r.enableNew }

// Action reports what happens to a disabled entry.
func (r *Rules) Action() Action { return r.action }

// Entry summarizes one catalog entry for the settings page.
type Entry struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name,omitempty"`
	// Visibility is CPA's own value, before any switch is applied.
	Visibility string `json:"visibility,omitempty"`
}

// maxEntries bounds a summary; CPA's catalog has tens of entries.
const maxEntries = 5000

// Summarize lists a well-formed Codex catalog's entries in order. It accepts
// exactly what Rewrite would, and reports false for anything else.
func Summarize(body []byte) ([]Entry, bool) {
	doc, err := parse(body)
	if err != nil || len(doc.entries) > maxEntries {
		return nil, false
	}
	out := make([]Entry, 0, len(doc.entries))
	for _, e := range doc.entries {
		var fields struct {
			DisplayName json.RawMessage `json:"display_name"`
			Visibility  json.RawMessage `json:"visibility"`
		}
		if json.Unmarshal(body[e.start:e.end], &fields) != nil {
			return nil, false
		}
		entry := Entry{Slug: e.slug}
		_ = json.Unmarshal(fields.DisplayName, &entry.DisplayName)
		_ = json.Unmarshal(fields.Visibility, &entry.Visibility)
		out = append(out, entry)
	}
	return out, true
}

// Rewrite returns the filtered catalog, or nil when body must pass through
// unchanged. It fails open: anything that is not a well-formed Codex catalog,
// a rewrite that would change nothing, and a rewrite that would leave no entry
// with visibility "list" all return nil. Codex treats a catalog without a listed
// model as non-authoritative and merges it into its bundled list, so emitting
// one would be worse than not filtering at all.
func (r *Rules) Rewrite(body []byte) []byte {
	if r.Idle() {
		return nil
	}
	doc, err := parse(body)
	if err != nil {
		return nil
	}
	var out bytes.Buffer
	out.Grow(len(body))
	out.Write(body[:doc.open])
	out.Write(body[doc.open:doc.entries[0].start])
	changed, listed, written := false, 0, 0
	for i, e := range doc.entries {
		raw := body[e.start:e.end]
		if !r.Allows(e.slug) {
			if r.action == Remove {
				changed = true
				continue
			}
			if hidden := e.hide(raw); hidden != nil {
				raw, changed = hidden, true
			}
		} else if e.listed {
			listed++
		}
		if written > 0 {
			// The original separator before this entry: whitespace and one comma.
			out.Write(body[doc.entries[i-1].end:e.start])
		}
		out.Write(raw)
		written++
	}
	out.Write(body[doc.entries[len(doc.entries)-1].end:])
	if !changed || listed == 0 {
		return nil
	}
	result := out.Bytes()
	if !json.Valid(result) {
		return nil
	}
	return result
}

// document locates the models array and its entries as byte offsets into body.
type document struct {
	open    int // offset of the models array's '['
	entries []entry
}

type entry struct {
	start, end int // entry bytes are body[start:end]
	slug       string
	listed     bool // visibility is the string "list"
	hidden     bool // visibility is the string "hide"
	// visibility value offsets relative to start; vStart < 0 when absent.
	vStart, vEnd int
}

// hide returns the entry with visibility "hide", or nil when it already is.
func (e entry) hide(raw []byte) []byte {
	if e.hidden {
		return nil
	}
	out := make([]byte, 0, len(raw)+len(`,"visibility":"hide"`))
	if e.vStart >= 0 {
		out = append(out, raw[:e.vStart]...)
		out = append(out, `"hide"`...)
		return append(out, raw[e.vEnd:]...)
	}
	// raw ends with the object's closing brace, and slug guarantees a member.
	out = append(out, raw[:len(raw)-1]...)
	out = append(out, `,"visibility":"hide"}`...)
	return out
}

var errNotCatalog = errors.New("not a codex model catalog")

func parse(body []byte) (document, error) {
	var doc document
	dec := json.NewDecoder(bytes.NewReader(body))
	if !expectDelim(dec, '{') {
		return doc, errNotCatalog
	}
	found := false
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return doc, err
		}
		if key != "models" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return doc, err
			}
			continue
		}
		if found || !expectDelim(dec, '[') {
			return doc, errNotCatalog
		}
		found = true
		doc.open = int(dec.InputOffset()) - 1
		if doc.open < 0 || body[doc.open] != '[' {
			return doc, errNotCatalog
		}
		for dec.More() {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return doc, err
			}
			end := int(dec.InputOffset())
			start := end - len(raw)
			if start < 0 || !bytes.Equal(body[start:end], raw) {
				return doc, errNotCatalog
			}
			e, err := parseEntry(raw)
			if err != nil {
				return doc, err
			}
			e.start, e.end = start, end
			doc.entries = append(doc.entries, e)
		}
		if !expectDelim(dec, ']') {
			return doc, errNotCatalog
		}
	}
	if !expectDelim(dec, '}') {
		return doc, errNotCatalog
	}
	if _, err := dec.Token(); err != io.EOF {
		return doc, errNotCatalog
	}
	if !found || len(doc.entries) == 0 {
		return doc, errNotCatalog
	}
	return doc, nil
}

// parseEntry requires a JSON object with one non-empty string slug. Gemini's
// catalog also has a top-level models array, but its entries carry name instead.
func parseEntry(raw []byte) (entry, error) {
	e := entry{vStart: -1}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if !expectDelim(dec, '{') {
		return e, errNotCatalog
	}
	seenSlug, seenVisibility := false, false
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return e, err
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return e, err
		}
		switch key {
		case "slug":
			if seenSlug || json.Unmarshal(value, &e.slug) != nil || e.slug == "" {
				return e, errNotCatalog
			}
			seenSlug = true
		case "visibility":
			if seenVisibility {
				return e, errNotCatalog
			}
			seenVisibility = true
			e.vEnd = int(dec.InputOffset())
			e.vStart = e.vEnd - len(value)
			if e.vStart < 0 || !bytes.Equal(raw[e.vStart:e.vEnd], value) {
				return e, errNotCatalog
			}
			var visibility string
			if json.Unmarshal(value, &visibility) == nil {
				e.listed, e.hidden = visibility == "list", visibility == "hide"
			}
		}
	}
	if !expectDelim(dec, '}') || !seenSlug {
		return e, errNotCatalog
	}
	return e, nil
}

func expectDelim(dec *json.Decoder, want json.Delim) bool {
	tok, err := dec.Token()
	return err == nil && tok == want
}
