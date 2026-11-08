// Package stream turns the stream chunks a response_stream_interceptor sees into the equivalent
// non-stream response objects. It is pure: no goroutines, no I/O. The plugin's single
// stream goroutine owns one Assembler and one Reassembler per RequestID.
package stream

import (
	"bytes"
	"encoding/json"
)

// Event is one SSE event or bare JSON event. Data is the JSON payload (or "[DONE]"); Name is the
// SSE event: name, "" when absent. JSON "type" beats Name when both are present.
type Event struct {
	Name string
	Data []byte
}

var done = []byte("[DONE]")

// Assembler is the per-RequestID line assembler, a port of CPA's sseJSONValidationState
// (sdk/api/handlers/handlers_stream.go:784-846) extended to bare JSON chunks:
//
//  1. CR and CRLF become LF, carrying a trailing CR across chunks.
//  2. A chunk starting with data: or event: after an unterminated line gets a newline first.
//  3. A line completes at LF, or at chunk end when it is event:, id: or a comment, a data: line
//     whose payload is valid JSON or [DONE], or a bare JSON value.
//  4. Consecutive data: lines join with LF until a blank line or until the joined payload is
//     valid JSON.
//  5. A line with no prefix that holds a JSON object is a bare event; a leading [ or , and a
//     trailing ] are tolerated (Gemini arrays).
type Assembler struct {
	pending []byte
	prevCR  bool
	name    string
	data    []byte
	hasData bool
	scan    closeScan // the unterminated tail line's payload, across chunk ends
}

// closeScan tracks the bracket depth and string state of the unterminated tail line's JSON
// payload over the bytes scanned so far. The tail only grows between chunk ends, so each byte is
// scanned once, and json.Valid runs only when the payload's outermost bracket has closed: one
// large event split across many chunks costs linear work instead of a validation per chunk.
type closeScan struct {
	pos    int // payload bytes scanned
	depth  int
	inStr  bool
	esc    bool
	closed int // payload offset just past the outermost closing bracket; 0 while open
}

// advance scans p (the tail's payload, a prefix of every later payload of the same tail) and
// returns the offset just past its outermost closing bracket, or -1 while the payload is open
// or is not an object or array.
func (s *closeScan) advance(p []byte) int {
	if s.closed > 0 {
		return s.closed
	}
	if len(p) == 0 || (p[0] != '{' && p[0] != '[') {
		return -1
	}
	for ; s.pos < len(p); s.pos++ {
		c := p[s.pos]
		if s.inStr {
			switch {
			case s.esc:
				s.esc = false
			case c == '\\':
				s.esc = true
			case c == '"':
				s.inStr = false
			}
			continue
		}
		switch c {
		case '"':
			s.inStr = true
		case '{', '[':
			s.depth++
		case '}', ']':
			s.depth--
			if s.depth == 0 {
				s.pos++
				s.closed = s.pos
				return s.closed
			}
		}
	}
	return -1
}

// Pending is the number of bytes held for an incomplete line or frame.
func (a *Assembler) Pending() int { return len(a.pending) + len(a.data) }

// Add feeds one chunk and returns the events it completed.
func (a *Assembler) Add(chunk []byte) []Event {
	if len(chunk) == 0 {
		return nil
	}
	if a.prevCR {
		if chunk[0] == '\n' {
			chunk = chunk[1:]
		}
		a.prevCR = false
	}
	if len(chunk) == 0 {
		return nil
	}
	a.prevCR = chunk[len(chunk)-1] == '\r'
	if bytes.IndexByte(chunk, '\r') >= 0 {
		chunk = bytes.ReplaceAll(chunk, []byte("\r\n"), []byte("\n"))
		chunk = bytes.ReplaceAll(chunk, []byte("\r"), []byte("\n"))
	}
	// pending never holds a newline between calls (every one was consumed), so only bytes from
	// old on (an inserted newline and this chunk) need scanning.
	old := len(a.pending)
	if len(a.pending) > 0 && a.pending[len(a.pending)-1] != '\n' && chunk[0] != '\n' {
		first := bytes.TrimSpace(bytes.SplitN(chunk, []byte("\n"), 2)[0])
		if bytes.HasPrefix(first, []byte("data:")) || bytes.HasPrefix(first, []byte("event:")) {
			a.pending = append(a.pending, '\n')
		}
	}
	a.pending = append(a.pending, chunk...)
	var out []Event
	start, from := 0, old
	for {
		i := bytes.IndexByte(a.pending[from:], '\n')
		if i < 0 {
			break
		}
		end := from + i
		out = a.line(a.pending[start:end], out, true)
		start, from = end+1, end+1
	}
	rest := a.pending[start:]
	if start > 0 || old == 0 {
		a.scan = closeScan{} // a new unterminated tail line
	}
	if len(rest) > 0 && a.completeAtEnd(rest) {
		out = a.line(rest, out, false)
		rest = nil
	}
	switch {
	case len(rest) == 0:
		a.pending = nil
	case start > 0:
		// Lines were consumed: keep only the unterminated tail, which lies inside this chunk
		// (every earlier newline was consumed by an earlier Add), so the copy is linear.
		a.pending = append([]byte(nil), rest...)
	default:
		// Nothing consumed: one long line keeps growing in place (amortized append).
	}
	return out
}

// mayClose reports whether a JSON object or array payload can be complete: its last byte closes
// it. json.Valid runs only then, so one large event split across many chunks is not re-validated
// at every chunk end (that is quadratic in the event size).
func mayClose(p []byte) bool {
	return len(p) > 0 && (p[len(p)-1] == '}' || p[len(p)-1] == ']')
}

// Finish flushes what is left at stream end and returns the final events.
func (a *Assembler) Finish() []Event {
	var out []Event
	if len(bytes.TrimSpace(a.pending)) > 0 {
		out = a.line(a.pending, out, false)
	}
	a.pending = nil
	return a.dispatch(out)
}

func (a *Assembler) completeAtEnd(l []byte) bool {
	t := bytes.TrimSpace(l)
	switch {
	case len(t) == 0:
		return false
	case t[0] == ':', bytes.HasPrefix(t, []byte("event:")), bytes.HasPrefix(t, []byte("id:")), bytes.HasPrefix(t, []byte("retry:")):
		return true
	case bytes.HasPrefix(t, []byte("data:")):
		p := bytes.TrimSpace(t[5:])
		if bytes.Equal(p, done) {
			return true
		}
		if a.hasData {
			// A continuation of an already open data payload: the depth lives partly in a.data,
			// so this rare multi-line case keeps the last-byte check.
			if !mayClose(p) {
				return false
			}
			if json.Valid(p) {
				return true
			}
			joined := append(append(append([]byte(nil), a.data...), '\n'), p...)
			return json.Valid(joined)
		}
		// p is right-trimmed: anything after the closing bracket is not JSON.
		if k := a.scan.advance(p); k < 0 || k != len(p) {
			return false
		}
		return json.Valid(p)
	default:
		q := t
		if q[0] == '[' || q[0] == ',' {
			q = bytes.TrimSpace(q[1:])
		}
		k := a.scan.advance(q)
		if k < 0 || len(bytes.Trim(q[k:], " \t],")) != 0 {
			return false
		}
		_, ok := bareJSON(t)
		return ok
	}
}

func (a *Assembler) dispatch(out []Event) []Event {
	if a.hasData {
		d := bytes.TrimSpace(a.data)
		if len(d) > 0 {
			out = append(out, Event{Name: a.name, Data: append([]byte(nil), d...)})
		}
	}
	a.data, a.hasData, a.name = a.data[:0], false, ""
	return out
}

func (a *Assembler) line(l []byte, out []Event, terminated bool) []Event {
	t := bytes.TrimRight(l, " \t")
	switch {
	case len(bytes.TrimSpace(t)) == 0:
		if terminated {
			out = a.dispatch(out)
		}
		return out
	case t[0] == ':':
		return out
	case bytes.HasPrefix(t, []byte("event:")):
		if a.hasData {
			out = a.dispatch(out)
		}
		a.name = string(bytes.TrimSpace(t[6:]))
		return out
	case bytes.HasPrefix(t, []byte("id:")), bytes.HasPrefix(t, []byte("retry:")):
		return out
	case bytes.HasPrefix(t, []byte("data:")):
		p := t[5:]
		if len(p) > 0 && p[0] == ' ' {
			p = p[1:]
		}
		if a.hasData {
			a.data = append(a.data, '\n')
		}
		a.data = append(a.data, p...)
		a.hasData = true
		d := bytes.TrimSpace(a.data)
		if bytes.Equal(d, done) || (mayClose(d) && json.Valid(d)) {
			out = a.dispatch(out)
		}
		return out
	default:
		if a.hasData {
			out = a.dispatch(out)
		}
		if j, ok := bareJSON(bytes.TrimSpace(t)); ok {
			out = append(out, Event{Data: append([]byte(nil), j...)})
		}
		return out
	}
}

// bareJSON accepts a JSON object, tolerating a leading [ or , and a trailing ] or ,.
func bareJSON(t []byte) ([]byte, bool) {
	t = bytes.TrimSpace(t)
	if len(t) > 0 && (t[0] == '[' || t[0] == ',') {
		t = bytes.TrimSpace(t[1:])
	}
	for len(t) > 0 && (t[len(t)-1] == ']' || t[len(t)-1] == ',') {
		if json.Valid(t) {
			break
		}
		t = bytes.TrimSpace(t[:len(t)-1])
	}
	if len(t) == 0 || t[0] != '{' || !json.Valid(t) {
		return nil, false
	}
	return t, true
}
