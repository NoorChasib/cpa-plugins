package canon

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// nodeKind is the JSON type of a parsed value.
type nodeKind uint8

const (
	kNull nodeKind = iota
	kTrue
	kFalse
	kNumber // s holds the literal token verbatim
	kString // s holds the decoded value, valid UTF-8
	kObject
	kArray
	kRaw // s holds pre-encoded canon/v1 bytes (a stub); never masked, never walked
)

type member struct {
	key string
	val *node
}

// node is one parsed JSON value. Objects keep unique keys in first-occurrence order; a duplicate key
// replaces the earlier value in place (last wins). A node is 72 bytes on 64-bit platforms, plus its
// string bytes and its share of the parent's members or elems slice.
type node struct {
	kind    nodeKind
	s       string
	members []member
	elems   []*node
}

func objNode() *node              { return &node{kind: kObject} }
func strNode(s string) *node      { return &node{kind: kString, s: s} }
func numNode(n int) *node         { return &node{kind: kNumber, s: strconv.Itoa(n)} }
func arrNode(el []*node) *node    { return &node{kind: kArray, elems: el} }
func rawNode(b []byte) *node      { return &node{kind: kRaw, s: string(b)} }
func (n *node) isObject() bool    { return n != nil && n.kind == kObject }
func (n *node) isArray() bool     { return n != nil && n.kind == kArray }
func (n *node) isString() bool    { return n != nil && n.kind == kString }
func (n *node) has(k string) bool { return n.get(k) != nil }

// get returns the value of key k, or nil when n is not an object or has no such key.
func (n *node) get(k string) *node {
	if n == nil || n.kind != kObject {
		return nil
	}
	for i := range n.members {
		if n.members[i].key == k {
			return n.members[i].val
		}
	}
	return nil
}

// del removes key k and returns its value (nil when absent).
func (n *node) del(k string) *node {
	if n == nil || n.kind != kObject {
		return nil
	}
	for i := range n.members {
		if n.members[i].key == k {
			v := n.members[i].val
			n.members = slices.Delete(n.members, i, i+1)
			return v
		}
	}
	return nil
}

// set replaces the value of key k in place, or appends it.
func (n *node) set(k string, v *node) {
	for i := range n.members {
		if n.members[i].key == k {
			n.members[i].val = v
			return
		}
	}
	n.members = append(n.members, member{k, v})
}

// strField returns the string value of key k, or "" when absent or not a string.
func (n *node) strField(k string) string {
	if v := n.get(k); v.isString() {
		return v.s
	}
	return ""
}

// takeString removes key k when its value is a string and returns that string.
func (n *node) takeString(k string) *string {
	if v := n.get(k); v.isString() {
		n.del(k)
		return &v.s
	}
	return nil
}

// without returns a new object with every member of n except key k (shallow).
func (n *node) without(k string) *node {
	out := objNode()
	for _, m := range n.members {
		if m.key != k {
			out.members = append(out.members, m)
		}
	}
	return out
}

var errTrailing = errors.New("canon: data after the top-level JSON value")

// parser decodes one JSON value with literal numbers and records the canon/v1 flags.
type parser struct {
	dec  *jsontext.Decoder
	dup  bool // a duplicate object key was seen
	bad  bool // invalid UTF-8 or a lone surrogate was replaced by U+FFFD
	left int  // values that may still be parsed; < 0 means no limit (Options.MaxNodes)
}

// parse decodes exactly one JSON value (surrounding whitespace allowed).
func parse(b []byte) (*node, parser, error) {
	return parseBudget(b, -1)
}

// parseBudget is parse with at most budget values (every object, array, string, number and literal;
// budget < 0 means no limit). Past it, it fails with ErrBodyBudget. p.left is the budget left.
func parseBudget(b []byte, budget int) (*node, parser, error) {
	p := parser{dec: jsontext.NewDecoder(bytes.NewReader(b),
		jsontext.AllowInvalidUTF8(true), jsontext.AllowDuplicateNames(true)), left: budget}
	n, err := p.value()
	if err != nil {
		return nil, p, err
	}
	if _, err := p.dec.ReadToken(); err != io.EOF {
		if err == nil {
			err = errTrailing
		}
		return nil, p, err
	}
	return n, p, nil
}

func (p *parser) value() (*node, error) {
	if p.left == 0 {
		return nil, ErrBodyBudget
	}
	if p.left > 0 {
		p.left--
	}
	switch p.dec.PeekKind() {
	case '{':
		if _, err := p.dec.ReadToken(); err != nil {
			return nil, err
		}
		obj := objNode()
		var index map[string]int
		for p.dec.PeekKind() != '}' {
			raw, err := p.dec.ReadValue()
			if err != nil {
				return nil, err
			}
			key := p.unquote(raw)
			val, err := p.value()
			if err != nil {
				return nil, err
			}
			i := -1
			if index != nil {
				if j, ok := index[key]; ok {
					i = j
				}
			} else {
				for j := range obj.members {
					if obj.members[j].key == key {
						i = j
						break
					}
				}
			}
			if i >= 0 {
				p.dup = true
				obj.members[i].val = val
				continue
			}
			obj.members = append(obj.members, member{key, val})
			if index != nil {
				index[key] = len(obj.members) - 1
			} else if len(obj.members) > 16 {
				index = make(map[string]int, 2*len(obj.members))
				for j, m := range obj.members {
					index[m.key] = j
				}
			}
		}
		if _, err := p.dec.ReadToken(); err != nil {
			return nil, err
		}
		return obj, nil
	case '[':
		if _, err := p.dec.ReadToken(); err != nil {
			return nil, err
		}
		arr := &node{kind: kArray}
		for p.dec.PeekKind() != ']' {
			el, err := p.value()
			if err != nil {
				return nil, err
			}
			arr.elems = append(arr.elems, el)
		}
		if _, err := p.dec.ReadToken(); err != nil {
			return nil, err
		}
		return arr, nil
	case '"':
		raw, err := p.dec.ReadValue()
		if err != nil {
			return nil, err
		}
		return strNode(p.unquote(raw)), nil
	case '0':
		raw, err := p.dec.ReadValue()
		if err != nil {
			return nil, err
		}
		return &node{kind: kNumber, s: string(raw)}, nil
	case 't', 'f', 'n':
		raw, err := p.dec.ReadValue()
		if err != nil {
			return nil, err
		}
		switch raw[0] {
		case 't':
			return &node{kind: kTrue}, nil
		case 'f':
			return &node{kind: kFalse}, nil
		}
		return &node{kind: kNull}, nil
	default:
		_, err := p.dec.ReadToken()
		if err == nil || err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
}

// unquote decodes a JSON string literal that jsontext has already checked for grammar. Invalid UTF-8
// bytes and unpaired surrogate escapes become U+FFFD, one per byte or escape, and set p.bad.
func (p *parser) unquote(raw []byte) string {
	b := raw[1 : len(raw)-1]
	if bytes.IndexByte(b, '\\') < 0 && utf8.Valid(b) {
		return string(b)
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == '\\':
			i++
			switch e := b[i]; e {
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'u':
				r := hex4(b[i+1 : i+5])
				i += 4
				if utf16.IsSurrogate(r) {
					r2 := rune(-1)
					if r < 0xdc00 && i+7 <= len(b) && b[i+1] == '\\' && b[i+2] == 'u' {
						r2 = hex4(b[i+3 : i+7])
					}
					if r2 >= 0xdc00 && r2 <= 0xdfff {
						r = utf16.DecodeRune(r, r2)
						i += 6
					} else {
						r = utf8.RuneError
						p.bad = true
					}
				}
				out = utf8.AppendRune(out, r)
			default: // '"', '\\', '/'
				out = append(out, e)
			}
			i++
		case c < utf8.RuneSelf:
			out = append(out, c)
			i++
		default:
			r, size := utf8.DecodeRune(b[i:])
			if r == utf8.RuneError && size == 1 {
				p.bad = true
				out = append(out, "�"...)
			} else {
				out = append(out, b[i:i+size]...)
			}
			i += size
		}
	}
	return string(out)
}

func hex4(b []byte) rune {
	var r rune
	for _, c := range b {
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			r |= rune(c - 'a' + 10)
		default:
			r |= rune(c - 'A' + 10)
		}
	}
	return r
}

// fixUTF8 replaces each invalid UTF-8 byte of s by U+FFFD, as the parser does, for strings that do not
// come from JSON (multipart names and fields). ok is false when anything was replaced.
func fixUTF8(s string) (string, bool) {
	if utf8.ValidString(s) {
		return s, true
	}
	var sb strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			sb.WriteString("�")
		} else {
			sb.WriteString(s[i : i+size])
		}
		i += size
	}
	return sb.String(), false
}

// encoder writes canon/v1. mask, when set, is applied to every string value and object key, but never
// inside kRaw stubs.
type encoder struct {
	buf  []byte
	mask func(string) string
	dup  bool // two keys of one object became equal after masking (last wins)
}

func (e *encoder) str(s string) string {
	if e.mask == nil {
		return s
	}
	return e.mask(s)
}

func (e *encoder) value(n *node) {
	switch n.kind {
	case kNull:
		e.buf = append(e.buf, "null"...)
	case kTrue:
		e.buf = append(e.buf, "true"...)
	case kFalse:
		e.buf = append(e.buf, "false"...)
	case kNumber:
		e.buf = append(e.buf, n.s...)
	case kString:
		e.buf = appendString(e.buf, e.str(n.s))
	case kRaw:
		e.buf = append(e.buf, n.s...)
	case kArray:
		e.buf = append(e.buf, '[')
		for i, el := range n.elems {
			if i > 0 {
				e.buf = append(e.buf, ',')
			}
			e.value(el)
		}
		e.buf = append(e.buf, ']')
	case kObject:
		e.object(n)
	}
}

func (e *encoder) object(n *node) {
	ms := make([]member, len(n.members))
	for i, m := range n.members {
		ms[i] = member{e.str(m.key), m.val}
	}
	slices.SortStableFunc(ms, func(a, b member) int { return strings.Compare(a.key, b.key) })
	e.buf = append(e.buf, '{')
	first := true
	for i, m := range ms {
		if i+1 < len(ms) && ms[i+1].key == m.key {
			e.dup = true // masked keys collided: the later member wins
			continue
		}
		if !first {
			e.buf = append(e.buf, ',')
		}
		first = false
		e.buf = appendString(e.buf, m.key)
		e.buf = append(e.buf, ':')
		e.value(m.val)
	}
	e.buf = append(e.buf, '}')
}

const lowerHex = "0123456789abcdef"

// appendString writes s as a canon/v1 string: escape '"', '\\', \b \f \n \r \t, other controls below
// U+0020 as lowercase \u00xx; nothing else is escaped. s must be valid UTF-8.
func appendString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		dst = append(dst, s[start:i]...)
		switch c {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			dst = append(dst, '\\', 'u', '0', '0', lowerHex[c>>4], lowerHex[c&0xf])
		}
		start = i + 1
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}
