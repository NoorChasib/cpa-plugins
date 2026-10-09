package aggregate

import (
	"math"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// A Claude account's weekly limit sits over its other windows. Once the weekly
// is spent the account can send nothing, however much of its session or its
// Fable allowance reads as left, so those two rows count each account by what
// its weekly still lets it use rather than by their own figure alone. Every
// other row, and every other provider, keeps the plain mean: the weekly row is
// the gate itself, and no other provider's windows are nested this way.

// weeklyGate is one Claude credential's weekly reading, as its other windows
// are judged against it.
type weeklyGate struct {
	// known is false when there is no usable weekly reading: no weekly window,
	// a figure that is not a number, or a reset already behind us. That last is
	// a window that has turned over since the poll, so its figure no longer
	// holds — the same mid-turnover reading fullAgainOf waits out. An unknown
	// gate holds nothing out and caps nothing.
	known bool
	// remaining is the weekly's remaining fraction. reset is when it refills,
	// and is zero when the provider gives no reset instant: at 100% used that
	// is a limit reached without saying until when, and below it a window that
	// does not refill on a schedule, which leaves the Fable it caps with no
	// full-again instant either.
	remaining float64
	reset     time.Time
}

// spent is a weekly the Weekly card prints as 0%. It judges the rounded
// percent for the same reason a level does: an account the page calls empty on
// one card must not count as usable on the card beside it.
func (g weeklyGate) spent() bool { return g.known && percentOf(g.remaining) == 0 }

// weeklyGateOf reads a credential's weekly window. It takes the first, as
// buildRows does, so the gate is the very reading the Weekly card shows.
//
// It is decided from that reading alone. CPA's routing state, a stale or failed
// poll, usage credits and banked resets play no part: each is shown where it
// belongs, and folding any of them in here would move the session figure for a
// reason the reader cannot see on the weekly beside it.
func weeklyGateOf(windows []qc.EntryWindow, now time.Time) weeklyGate {
	for _, w := range windows {
		if rowIDOf(w) != qc.WindowWeekly {
			continue
		}
		remaining, issue := remainingOf(w.UsedPercent)
		if issue == issuePercentInvalid || (!w.ResetAt.IsZero() && !w.ResetAt.After(now)) {
			return weeklyGate{}
		}
		return weeklyGate{known: true, remaining: remaining, reset: w.ResetAt}
	}
	return weeklyGate{}
}

// gatedBy applies a Claude credential's weekly to its member of one row.
//
// Its session is held out of the mean once the weekly is spent. An idle
// session reads 100% with nothing able to use it, and counting it showed a pool
// fuller than the accounts that can still send; the entry still prints the
// reading, because the expanded card is where the actual numbers are read.
//
// Its Fable allowance is capped at what the weekly has left instead, since no
// account can spend more Fable than its weekly allows, so the row draws down as
// the weekly does. Nothing is held out of Fable: an account with no weekly left
// counts as 0%, and a pool whose every weekly is spent reads 0% there too.
func gatedBy(m member, gate weeklyGate) member {
	if m.record.identity.Provider != "claude" {
		return m
	}
	switch rowIDOf(m.window) {
	case qc.WindowSession:
		m.heldOut = gate.spent()
	case qc.WindowWeeklyFable:
		if gate.known {
			m.weekly = gate
			m.pooled = math.Min(m.remaining, gate.remaining)
		}
	}
	return m
}
