package plugin_test

// The 0.1.6 gauges the R1 scenarios assert (kept apart so the scenarios also build against 0.1.5's
// runtime, with a stub of this file, to show they fail there).

import (
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/capture"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
)

// r1Gauges returns the hold budget's and the write channel's peaks.
func r1Gauges(rt *capture.Runtime) (holdPeak int64, queuePeak int) {
	if rt == nil {
		return 0, 0
	}
	_, holdPeak = rt.HoldBytes()
	_, queuePeak = rt.WriteQueue()
	return holdPeak, queuePeak
}

// r1Slack is what the live heap may grow during a stall beyond the charged budgets' peaks: the Go
// runtime, maps, the test driver's own garbage between collections (recorded per scenario).
const r1Slack = 64 << 20

// r1CheckMemory asserts R2 in a scenario: the hold and the write channel stay within their bounds,
// and the live heap grows during the stall by at most the charged budgets' peaks (content pool, usage
// budget, hold) plus r1Slack.
func r1CheckMemory(t *testing.T, rt *capture.Runtime, holdPeak int64, queuePeak int, live, poolPeak int64) {
	t.Helper()
	if holdPeak > capture.HoldBytes || queuePeak > capture.WriteQueueEntries {
		t.Fatalf("hold peak %d (budget %d), write queue peak %d (depth %d)", holdPeak, capture.HoldBytes, queuePeak, capture.WriteQueueEntries)
	}
	if bound := poolPeak + hooks.UsageQueueBytes + holdPeak + r1Slack; live > bound {
		t.Fatalf("live heap grew %d MiB during the stall, over pool %d + usage %d + hold %d + slack %d MiB",
			live>>20, poolPeak>>20, hooks.UsageQueueBytes>>20, holdPeak>>20, r1Slack>>20)
	}
}
