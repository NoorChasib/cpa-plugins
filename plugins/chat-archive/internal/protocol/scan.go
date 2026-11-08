package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"unicode/utf16"
	"unicode/utf8"
)

// ErrSyntax is returned for an RPC body that is not the expected JSON shape.
var ErrSyntax = errors.New("protocol: malformed rpc body")

// Fields iterates the members of the top-level JSON object in raw. fn receives the key bytes
// (unescaped only when the key contains no escape, which is always the case for CPA's Go field
// names; an escaped key is passed as written) and the raw value span. Both alias raw: callers
// copy what they keep. Large values are skipped with memchr-speed string scanning, so a
// 160 MiB base64 field costs no allocation.
func Fields(raw []byte, fn func(key, val []byte) error) error {
	i := skipWS(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return ErrSyntax
	}
	i = skipWS(raw, i+1)
	if i < len(raw) && raw[i] == '}' {
		return nil
	}
	for {
		if i >= len(raw) || raw[i] != '"' {
			return ErrSyntax
		}
		end, err := skipString(raw, i)
		if err != nil {
			return err
		}
		key := raw[i+1 : end-1]
		i = skipWS(raw, end)
		if i >= len(raw) || raw[i] != ':' {
			return ErrSyntax
		}
		i = skipWS(raw, i+1)
		vend, err := skipValue(raw, i)
		if err != nil {
			return err
		}
		if err := fn(key, raw[i:vend]); err != nil {
			return err
		}
		i = skipWS(raw, vend)
		if i >= len(raw) {
			return ErrSyntax
		}
		switch raw[i] {
		case ',':
			i = skipWS(raw, i+1)
		case '}':
			return nil
		default:
			return ErrSyntax
		}
	}
}

func skipWS(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// skipString returns the index just past the closing quote of the string starting at b[i]=='"'.
func skipString(b []byte, i int) (int, error) {
	j := i + 1
	for {
		k := bytes.IndexByte(b[j:], '"')
		if k < 0 {
			return 0, ErrSyntax
		}
		k += j
		n := 0
		for p := k - 1; p > i && b[p] == '\\'; p-- {
			n++
		}
		if n%2 == 0 {
			return k + 1, nil
		}
		j = k + 1
	}
}

func skipValue(b []byte, i int) (int, error) {
	if i >= len(b) {
		return 0, ErrSyntax
	}
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(b); j++ {
			switch b[j] {
			case '"':
				end, err := skipString(b, j)
				if err != nil {
					return 0, err
				}
				j = end - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1, nil
				}
			}
		}
		return 0, ErrSyntax
	default:
		j := i
		for j < len(b) {
			c := b[j]
			if c == ',' || c == '}' || c == ']' || c == ' ' || c == '\t' || c == '\n' || c == '\r' {
				break
			}
			j++
		}
		if j == i {
			return 0, ErrSyntax
		}
		return j, nil
	}
}

// IsNull reports whether a value span is JSON null.
func IsNull(val []byte) bool { return string(val) == "null" }

// Base64Len returns the length of the base64 text in a JSON string span, 0 for null.
func Base64Len(val []byte) int {
	if len(val) < 2 || val[0] != '"' {
		return 0
	}
	return len(val) - 2
}

// DecodeBase64 decodes a JSON string span holding base64 ([]byte on the RPC wire) straight into
// a new slice of the decoded size; the base64 text itself is never copied into the Go heap.
// null and "" give nil.
func DecodeBase64(val []byte) ([]byte, error) {
	if IsNull(val) {
		return nil, nil
	}
	if len(val) < 2 || val[0] != '"' || val[len(val)-1] != '"' {
		return nil, ErrSyntax
	}
	inner := val[1 : len(val)-1]
	if len(inner) == 0 {
		return nil, nil
	}
	if bytes.IndexByte(inner, '\\') >= 0 {
		// Never emitted by encoding/json for base64; fall back to the full decoder.
		var out []byte
		if err := json.Unmarshal(val, &out); err != nil {
			return nil, ErrSyntax
		}
		return out, nil
	}
	dst := make([]byte, base64.StdEncoding.DecodedLen(len(inner)))
	n, err := base64.StdEncoding.Decode(dst, inner)
	if err != nil {
		return nil, ErrSyntax
	}
	return dst[:n], nil
}

// String decodes a small JSON string span. null gives "".
func String(val []byte) (string, error) {
	if IsNull(val) {
		return "", nil
	}
	if len(val) >= 2 && val[0] == '"' && bytes.IndexByte(val, '\\') < 0 && utf8.Valid(val) {
		return string(val[1 : len(val)-1]), nil
	}
	var s string
	if err := json.Unmarshal(val, &s); err != nil {
		return "", ErrSyntax
	}
	return s, nil
}

// UnquoteInto appends the unescaped bytes of the JSON string span val to dst without creating
// any Go string, so a caller can zero dst afterwards (secret fields). null appends
// nothing.
func UnquoteInto(dst, val []byte) ([]byte, error) {
	if len(val) == 0 || IsNull(val) {
		return dst, nil
	}
	if len(val) < 2 || val[0] != '"' || val[len(val)-1] != '"' {
		return dst, ErrSyntax
	}
	s := val[1 : len(val)-1]
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			dst = append(dst, c)
			continue
		}
		i++
		if i >= len(s) {
			return dst, ErrSyntax
		}
		switch s[i] {
		case '"', '\\', '/':
			dst = append(dst, s[i])
		case 'b':
			dst = append(dst, '\b')
		case 'f':
			dst = append(dst, '\f')
		case 'n':
			dst = append(dst, '\n')
		case 'r':
			dst = append(dst, '\r')
		case 't':
			dst = append(dst, '\t')
		case 'u':
			r, n, ok := hexRune(s[i+1:])
			if !ok {
				return dst, ErrSyntax
			}
			i += n
			if utf16.IsSurrogate(r) {
				if i+2 < len(s) && s[i+1] == '\\' && s[i+2] == 'u' {
					if r2, n2, ok := hexRune(s[i+3:]); ok {
						if d := utf16.DecodeRune(r, r2); d != utf8.RuneError {
							r = d
							i += 2 + n2
						} else {
							r = utf8.RuneError
						}
					} else {
						r = utf8.RuneError
					}
				} else {
					r = utf8.RuneError
				}
			}
			dst = utf8.AppendRune(dst, r)
		default:
			return dst, ErrSyntax
		}
	}
	return dst, nil
}

func hexRune(b []byte) (rune, int, bool) {
	if len(b) < 4 {
		return 0, 0, false
	}
	var r rune
	for _, c := range b[:4] {
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			r |= rune(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			r |= rune(c - 'A' + 10)
		default:
			return 0, 0, false
		}
	}
	return r, 4, true
}
