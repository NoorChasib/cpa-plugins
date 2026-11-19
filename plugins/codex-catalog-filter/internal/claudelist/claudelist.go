// Package claudelist filters CPA's Claude-format model list, the answer to
// GET /v1/models with an anthropic-version header:
//
//	{"data":[{"id":"claude-...","display_name":"...",...},...],"first_id":"...","has_more":false,"last_id":"..."}
//
// CPA cloaks every id that does not start with "claude-" as
// "claude-fable-5-dd-" plus the id reversed, so Claude Code keeps it
// (internal/client/claude/models/models.go). Switches are keyed by real ids:
// each entry is de-cloaked for matching, and kept entries are copied as CPA
// sent them.
package claudelist

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

const cloakPrefix = "claude-fable-5-dd-"

// Cloak is CPA's EnsureClaudeModelIDPrefix: the id CPA lists to Claude Code.
func Cloak(id string) string {
	if id == "" || strings.HasPrefix(id, "claude-") {
		return id
	}
	return cloakPrefix + reverse(id)
}

func reverse(s string) string {
	runes := []rune(s)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}

// Decloak reverses CPA's EnsureClaudeModelIDPrefix, as CPA's own
// ResolveClaudeModelIDPrefix does for routing, including a "(level)" suffix.
func Decloak(id string) string {
	base, suffix := id, ""
	if open := strings.LastIndex(id, "("); open >= 0 && strings.HasSuffix(id, ")") {
		base, suffix = id[:open], id[open:]
	}
	encoded, ok := strings.CutPrefix(base, cloakPrefix)
	if !ok || encoded == "" {
		return id
	}
	return reverse(encoded) + suffix
}

// Entry is one model of a Claude-format list.
type Entry struct {
	// ID is the real model id, de-cloaked.
	ID string `json:"id"`
	// CloakedID is the id as CPA listed it, when it differs from ID.
	CloakedID   string `json:"cloaked_id,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

type entry struct {
	start, end int
	id         string // as listed
	display    string
}

type member struct {
	key        string
	start, end int // value bytes are body[start:end]
}

type document struct {
	members []member
	data    int // index of the data member
	entries []entry
}

var errNotList = errors.New("not a claude model list")

const maxEntries = 5000

// parse accepts exactly a JSON object with a non-empty data array of objects
// that each have one non-empty string id, plus first_id, last_id, and
// has_more members, and nothing after it.
func parse(body []byte) (document, error) {
	doc := document{data: -1}
	dec := json.NewDecoder(bytes.NewReader(body))
	if !expectDelim(dec, '{') {
		return doc, errNotList
	}
	keys := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return doc, errNotList
		}
		key, _ := tok.(string)
		if keys[key] {
			return doc, errNotList
		}
		keys[key] = true
		if key != "data" {
			var value json.RawMessage
			if err := dec.Decode(&value); err != nil {
				return doc, errNotList
			}
			end := int(dec.InputOffset())
			start := end - len(value)
			if start < 0 || !bytes.Equal(body[start:end], value) {
				return doc, errNotList
			}
			doc.members = append(doc.members, member{key: key, start: start, end: end})
			continue
		}
		if !expectDelim(dec, '[') {
			return doc, errNotList
		}
		start := int(dec.InputOffset()) - 1
		for dec.More() {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return doc, errNotList
			}
			end := int(dec.InputOffset())
			e, err := parseEntry(raw)
			if err != nil || len(doc.entries) >= maxEntries {
				return doc, errNotList
			}
			e.start, e.end = end-len(raw), end
			if e.start < 0 || !bytes.Equal(body[e.start:e.end], raw) {
				return doc, errNotList
			}
			doc.entries = append(doc.entries, e)
		}
		if !expectDelim(dec, ']') {
			return doc, errNotList
		}
		doc.data = len(doc.members)
		doc.members = append(doc.members, member{key: key, start: start, end: int(dec.InputOffset())})
	}
	if !expectDelim(dec, '}') {
		return doc, errNotList
	}
	if _, err := dec.Token(); err != io.EOF {
		return doc, errNotList
	}
	if doc.data < 0 || len(doc.entries) == 0 || !keys["first_id"] || !keys["last_id"] || !keys["has_more"] {
		return doc, errNotList
	}
	for _, m := range doc.members {
		if m.start < 0 || m.end > len(body) || m.start >= m.end {
			return doc, errNotList
		}
	}
	return doc, nil
}

func parseEntry(raw []byte) (entry, error) {
	var fields struct {
		ID          *string `json:"id"`
		DisplayName any     `json:"display_name"`
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || json.Unmarshal(raw, &fields) != nil || fields.ID == nil || *fields.ID == "" {
		return entry{}, errNotList
	}
	e := entry{id: *fields.ID}
	if name, ok := fields.DisplayName.(string); ok {
		e.display = name
	}
	return e, nil
}

func expectDelim(dec *json.Decoder, want json.Delim) bool {
	tok, err := dec.Token()
	return err == nil && tok == want
}

// Summarize lists a Claude-format model list's entries in order, de-cloaked.
func Summarize(body []byte) ([]Entry, bool) {
	doc, err := parse(body)
	if err != nil {
		return nil, false
	}
	return summarize(doc), true
}

func summarize(doc document) []Entry {
	out := make([]Entry, 0, len(doc.entries))
	for _, e := range doc.entries {
		item := Entry{ID: Decloak(e.id), DisplayName: e.display}
		if item.ID != e.id {
			item.CloakedID = e.id
		}
		out = append(out, item)
	}
	return out
}

// Rewrite keeps the entries whose real id allows accepts, copying each kept
// entry and every member other than data, first_id, and last_id as CPA sent
// them, and recomputes first_id and last_id. It returns the summary of the
// original list and the rewritten body, or a nil body when the list must pass
// through unchanged: it is not a Claude-format list, nothing would be
// removed, or nothing would remain (Claude Code ignores an empty list and
// keeps its previous one, so an empty list would be a confusing no-op).
func Rewrite(body []byte, allows func(realID string) bool) ([]Entry, []byte, bool) {
	doc, err := parse(body)
	if err != nil {
		return nil, nil, false
	}
	summary := summarize(doc)
	var kept []entry
	for i, e := range doc.entries {
		if allows(summary[i].ID) {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(doc.entries) || len(kept) == 0 {
		return summary, nil, true
	}
	var out bytes.Buffer
	out.Grow(len(body))
	cursor := 0
	for _, m := range doc.members {
		out.Write(body[cursor:m.start])
		switch m.key {
		case "data":
			out.WriteByte('[')
			for i, e := range kept {
				if i > 0 {
					out.WriteByte(',')
				}
				out.Write(body[e.start:e.end])
			}
			out.WriteByte(']')
		case "first_id":
			writeString(&out, kept[0].id)
		case "last_id":
			writeString(&out, kept[len(kept)-1].id)
		default:
			out.Write(body[m.start:m.end])
		}
		cursor = m.end
	}
	out.Write(body[cursor:])
	result := out.Bytes()
	if !json.Valid(result) {
		return summary, nil, true
	}
	return summary, result, true
}

func writeString(out *bytes.Buffer, value string) {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
	out.Truncate(out.Len() - 1) // Encode appends a newline
}
