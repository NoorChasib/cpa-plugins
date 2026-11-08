// Package fixture is the nativefixture recorder used by native acceptance. Only builds with
// the nativefixture tag keep records; production builds compile the no-op in fixture_disabled.go,
// and release validation rejects any library built with that tag. A record holds a hook name,
// a chunk index, a byte length and fixed reason or counter codes only, never a hook field value
// (no request ids, headers, bodies or keys).
package fixture

// Record is one sanitized hook observation.
type Record struct {
	Hook   string `json:"hook"`
	Chunk  int    `json:"chunk,omitempty"`
	Len    int    `json:"len,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Size of the FIFO ring.
const ringSize = 64
