//go:build nativefixture

package fixture

import "sync"

// Enabled reports whether this build keeps fixture records.
const Enabled = true

// Recorder keeps the last 64 records in a FIFO ring.
type Recorder struct {
	mu      sync.Mutex
	records []Record
}

// Add appends one record.
func (r *Recorder) Add(rec Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.records) == ringSize {
		copy(r.records, r.records[1:])
		r.records = r.records[:ringSize-1]
	}
	r.records = append(r.records, rec)
}

// Snapshot returns a copy of the ring.
func (r *Recorder) Snapshot() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Record{}, r.records...)
}
