package aggregate

import (
	"fmt"
	"math"
	"slices"
	"time"
)

// sessionValue projects only known resets, with no further consumption. A
// weekly reset unlocks a session; it does not refill that session itself.
// An elapsed known session reset also clears that prerequisite for a future
// weekly unlock, even while its displayed reading awaits the next poll.
func sessionValue(m member, now, through time.Time) (float64, bool) {
	restored := func(reset time.Time) bool { return reset.After(now) && !reset.After(through) }
	gate := weeklyGateOf(m.record.windows, now)
	if gate.spent() {
		if !restored(gate.reset) {
			return 0, false
		}
		if !m.window.ResetAt.IsZero() && !m.window.ResetAt.After(through) {
			return 1, true
		}
	}
	if restored(m.window.ResetAt) {
		return 1, true
	}
	return m.remaining, true
}

// forecastSession keeps today's mean over weekly-eligible accounts, but
// projects recovery over every reporting account. Rejoining accounts change
// the denominator: a new usable account can add capacity while lowering the
// mean. In that case the UI names the returning account without a false +%.
func forecastSession(row *Row, members []member, now time.Time) {
	agg := &row.Aggregate
	agg.SoonestResetAtEpoch, agg.SoonestResetInSeconds = nil, nil
	agg.FullAtEpoch, agg.FullInSeconds = nil, nil
	agg.ProjectedGainFraction, agg.ProjectedGainPercent = 0, 0
	agg.Subtext = ""
	for i := range row.Entries {
		row.Entries[i].ResetsNext = false
		row.Entries[i].RecoveryShare = 0
	}
	var events []time.Time
	latest, fullKnown := now, true
	for _, m := range members {
		gate := weeklyGateOf(m.record.windows, now)
		var needed []time.Time
		if m.remaining < 1 {
			needed = append(needed, m.window.ResetAt)
		}
		if gate.spent() {
			needed = append(needed, gate.reset)
		}
		for _, reset := range needed {
			if reset.IsZero() {
				fullKnown = false
			}
			if reset.After(now) {
				events = append(events, reset)
				if reset.After(latest) {
					latest = reset
				}
			}
		}
	}
	if fullKnown && latest.After(now) {
		epoch, seconds := latest.Unix(), int64(latest.Sub(now)/time.Second)
		agg.FullAtEpoch, agg.FullInSeconds = &epoch, &seconds
	}
	slices.SortFunc(events, func(a, b time.Time) int { return a.Compare(b) })
	for _, event := range events {
		through := event.Add(time.Minute)
		beforeSum, afterSum, totalGain, afterCount := 0.0, 0.0, 0.0, 0
		gains := map[string]float64{}
		kinds := map[string]string{}
		for _, m := range members {
			before, _ := sessionValue(m, now, now)
			after, eligible := sessionValue(m, now, through)
			beforeSum += before
			if eligible {
				afterCount++
				afterSum += after
			}
			if gain := after - before; gain > 1e-9 {
				id := m.record.identity.AuthIndex
				gains[id], totalGain = gain, totalGain+gain
				kinds[id] = "session"
				if m.heldOut {
					kinds[id] = "weekly"
				}
			}
		}
		if afterSum <= beforeSum+1e-9 || afterCount == 0 {
			continue
		}
		agg.ProjectedGainFraction = math.Max(0, afterSum/float64(afterCount)-agg.RemainingFraction)
		agg.ProjectedGainPercent = percentOf(agg.ProjectedGainFraction)
		epoch, seconds := event.Unix(), int64(event.Sub(now)/time.Second)
		agg.SoonestResetAtEpoch, agg.SoonestResetInSeconds = &epoch, &seconds
		var name string
		for i := range row.Entries {
			entry := &row.Entries[i]
			gain := gains[entry.CredentialID]
			if gain <= 0 {
				continue
			}
			entry.ResetsNext, entry.RecoveryKind = true, kinds[entry.CredentialID]
			entry.RecoveryShare = agg.ProjectedGainFraction * gain / totalGain
			if entry.HeldOut {
				agg.ReturningAccountCount++
			}
			if name == "" {
				for _, m := range members {
					if m.record.identity.AuthIndex == entry.CredentialID {
						name = shortName(emailOf(m.record.identity))
						break
					}
				}
			}
		}
		verb := "resets"
		if agg.ReturningAccountCount > 0 {
			verb = "becomes available"
		}
		agg.Subtext = fmt.Sprintf("%s %s in %s", name, verb, humanDuration(event.Sub(now)))
		if agg.ProjectedGainPercent > 0 {
			agg.Subtext = fmt.Sprintf("+%d%% when %s", agg.ProjectedGainPercent, agg.Subtext)
		}
		return
	}
}
