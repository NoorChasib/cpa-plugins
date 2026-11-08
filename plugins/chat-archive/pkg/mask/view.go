package mask

import (
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// view is a decoded copy of a string for the rules and exact runs. Strings that
// are themselves JSON text (a tool output holding a JSON document, JSON from a Go program) or that are
// percent-encoded (URLs, form bodies) carry escapes next to or inside a secret: "\n<token>" has no
// \b before the token, "\/" breaks a base64 class, "&" hides the "&" of a URL parameter, and
// "%3D<value>" glues the value to its name. The view decodes JSON string escapes
// (\n \r \t \b \f \/ \" \\ and \uXXXX outside the surrogate range) and %XX triplets, each once, left
// to right. A match in the view is mapped back to the smallest byte range of the original that covers
// it; the original is scanned as well, so a secret that only the raw form matches is still found.
type view struct {
	text string
	orig string // the original, to recompute sequence lengths
	segs []vseg // one per decoded sequence, in order
}

// vseg is one decoded sequence: its offset in the view and in the original. The two lengths are
// recomputed from the original with decodeSeq.
type vseg struct{ v, r int32 }

// decodeView returns the decoded view of s, or nil when s holds no sequence to decode.
func decodeView(s string) *view {
	if strings.IndexByte(s, '\\') < 0 && strings.IndexByte(s, '%') < 0 {
		return nil
	}
	var b []byte
	var segs []vseg
	last := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c != '\\' && c != '%' {
			i++
			continue
		}
		var out [utf8.UTFMax]byte
		n, rn := decodeSeq(s, i, out[:])
		if rn == 0 {
			i++
			continue
		}
		if b == nil {
			b = make([]byte, 0, len(s))
		}
		b = append(b, s[last:i]...)
		segs = append(segs, vseg{v: int32(len(b)), r: int32(i)})
		b = append(b, out[:n]...)
		i += rn
		last = i
	}
	if segs == nil {
		return nil
	}
	b = append(b, s[last:]...)
	return &view{text: string(b), orig: s, segs: segs}
}

// decodeSeq decodes the sequence starting at s[i] into out: it returns the decoded length n and the
// sequence length rn, or rn = 0 when s[i] starts no sequence.
func decodeSeq(s string, i int, out []byte) (n, rn int) {
	switch s[i] {
	case '\\':
		if i+1 >= len(s) {
			return 0, 0
		}
		var c byte
		switch s[i+1] {
		case 'n':
			c = '\n'
		case 'r':
			c = '\r'
		case 't':
			c = '\t'
		case 'b':
			c = '\b'
		case 'f':
			c = '\f'
		case '/', '"', '\\':
			c = s[i+1]
		case 'u':
			if i+6 > len(s) {
				return 0, 0
			}
			var r rune
			for _, h := range []byte(s[i+2 : i+6]) {
				d, ok := unhex(h)
				if !ok {
					return 0, 0
				}
				r = r<<4 | rune(d)
			}
			if utf16.IsSurrogate(r) {
				return 0, 0
			}
			return utf8.EncodeRune(out, r), 6
		default:
			return 0, 0
		}
		out[0] = c
		return 1, 2
	case '%':
		if i+2 >= len(s) {
			return 0, 0
		}
		hi, ok1 := unhex(s[i+1])
		lo, ok2 := unhex(s[i+2])
		if !ok1 || !ok2 {
			return 0, 0
		}
		out[0] = hi<<4 | lo
		return 1, 3
	}
	return 0, 0
}

func unhex(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// seg returns the decoded sequence holding or preceding view offset p, with its view length and its
// length in the original; i < 0 when p comes before every sequence.
func (v *view) seg(p int) (i, vn, rn int) {
	i = sort.Search(len(v.segs), func(k int) bool { return int(v.segs[k].v) > p }) - 1
	if i < 0 {
		return -1, 0, 0
	}
	var out [utf8.UTFMax]byte
	vn, rn = decodeSeq(v.orig, int(v.segs[i].r), out[:])
	return i, vn, rn
}

// rawStart maps the view offset of a span's first byte to the original.
func (v *view) rawStart(p int) int {
	i, vn, rn := v.seg(p)
	if i < 0 {
		return p
	}
	sg := v.segs[i]
	if p < int(sg.v)+vn {
		return int(sg.r) // inside a decoded sequence: cover all of it
	}
	return int(sg.r) + rn + p - (int(sg.v) + vn)
}

// rawEnd maps a span's exclusive view end to the original.
func (v *view) rawEnd(p int) int {
	if p == 0 {
		return 0
	}
	i, vn, rn := v.seg(p - 1)
	if i < 0 {
		return p
	}
	sg := v.segs[i]
	if p-1 < int(sg.v)+vn {
		return int(sg.r) + rn
	}
	return int(sg.r) + rn + p - (int(sg.v) + vn)
}
