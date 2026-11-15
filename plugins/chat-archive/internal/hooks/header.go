package hooks

import (
	"net/http"
	"sort"
	"strings"
)

// HeaderGet returns the first value of header name in h under any key case. http.Header.Get finds
// only the canonical key, and a host may pass keys as written.
func HeaderGet(h http.Header, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

// HeaderList returns every value of a list header (any key case), joined with ", " as RFC 9110 5.3
// combines repeated field lines: a body sent with Content-Encoding twice ("gzip" then "br") names both
// codings, and decoding only the first would leave it still encoded.
func HeaderList(h http.Header, name string) string {
	var keys, vs []string
	for k := range h {
		if strings.EqualFold(k, name) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys) // one key in practice; a fixed order if a map holds two spellings
	for _, k := range keys {
		vs = append(vs, h[k]...)
	}
	return strings.Join(vs, ", ")
}
