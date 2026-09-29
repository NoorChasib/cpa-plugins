package catalog

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// compileGlob turns a shell-style glob into an anchored regular expression.
//
// `*` matches any run of characters and `?` exactly one. Unlike path.Match,
// neither treats `/` specially: a slug is an identifier, not a path, and a
// prefixed slug such as "team/gpt-6-sol" should not need a second pattern.
// `[abc]`, `[a-z]`, and the negations `[!a-z]` / `[^a-z]` match one character.
// A backslash makes the next character literal. Matching is case-sensitive,
// and `*` and `?` match any character, newline included.
func compileGlob(pattern string) (*regexp.Regexp, error) {
	var out strings.Builder
	out.WriteString(`(?s)^`)
	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		switch r := runes[i]; r {
		case '*':
			for i+1 < len(runes) && runes[i+1] == '*' {
				i++
			}
			out.WriteString(`.*`)
		case '?':
			out.WriteString(`.`)
		case '\\':
			if i+1 == len(runes) {
				return nil, errors.New("trailing backslash")
			}
			i++
			out.WriteString(regexp.QuoteMeta(string(runes[i])))
		case '[':
			end, class, err := compileClass(runes, i)
			if err != nil {
				return nil, err
			}
			out.WriteString(class)
			i = end
		default:
			out.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	out.WriteString(`$`)
	return regexp.Compile(out.String())
}

// compileClass reads the bracket expression starting at runes[open] and
// returns the index of its closing bracket and the equivalent regexp class.
// Every member is written as \x{...} so no character is special inside it.
func compileClass(runes []rune, open int) (int, string, error) {
	i := open + 1
	var class strings.Builder
	class.WriteString(`[`)
	if i < len(runes) && (runes[i] == '!' || runes[i] == '^') {
		class.WriteString(`^`)
		i++
	}
	members := 0
	for ; i < len(runes); i++ {
		r := runes[i]
		if r == ']' && members > 0 {
			class.WriteString(`]`)
			return i, class.String(), nil
		}
		if r == '\\' {
			if i+1 == len(runes) {
				break
			}
			i++
			r = runes[i]
		}
		lo, hi := r, r
		if i+2 < len(runes) && runes[i+1] == '-' && runes[i+2] != ']' {
			i += 2
			hi = runes[i]
			if hi == '\\' {
				if i+1 == len(runes) {
					break
				}
				i++
				hi = runes[i]
			}
			if hi < lo {
				return 0, "", fmt.Errorf("invalid range %q-%q", lo, hi)
			}
		}
		fmt.Fprintf(&class, `\x{%x}`, lo)
		if hi != lo {
			fmt.Fprintf(&class, `-\x{%x}`, hi)
		}
		members++
	}
	return 0, "", errors.New("unterminated character class")
}
