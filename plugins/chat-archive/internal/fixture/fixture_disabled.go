//go:build !nativefixture

package fixture

// Enabled reports whether this build keeps fixture records.
const Enabled = false

// Recorder is a no-op in production builds.
type Recorder struct{}

// Add does nothing.
func (*Recorder) Add(Record) {}

// Snapshot returns nil.
func (*Recorder) Snapshot() []Record { return nil }
