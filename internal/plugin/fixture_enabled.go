//go:build nativefixture

package plugin

import (
	"sync"

	"github.com/NoorChasib/cpa-plugin-token-usage/internal/protocol"
	"github.com/NoorChasib/cpa-plugin-token-usage/internal/usage"
)

// Only synthetic native-spike builds retain bounded, sanitized observations.
type fixtureRecorder struct {
	mu      sync.Mutex
	records []protocol.UsageRecord
}

func (f *fixtureRecorder) record(e usage.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.records) == 64 {
		copy(f.records, f.records[1:])
		f.records = f.records[:63]
	}
	f.records = append(f.records, e.WireRecord())
}

func (f *fixtureRecorder) addStatus(value, snapshot map[string]any) {
	f.mu.Lock()
	value["fixture_records"] = append([]protocol.UsageRecord{}, f.records...)
	f.mu.Unlock()
	value["schema6_probe"] = "<model>&literal"
	diag := snapshot["diagnostics"].(map[string]string)
	value["observed_events"] = diag["observed_events"]
	value["rejected_events"] = diag["rejected_events"]
}
