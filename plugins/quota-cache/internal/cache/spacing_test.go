package cache

import (
	"math/rand/v2"
	"strconv"
	"testing"
	"time"
)

// manyAccounts is a deployment the size of a real one: six Claude, two Codex,
// one Grok and one OpenRouter credential.
func manyAccounts() []Account {
	var accounts []Account
	for i := 0; i < 6; i++ {
		accounts = append(accounts, Account{"claude", "claude-" + strconv.Itoa(i)})
	}
	return append(accounts, Account{"codex", "codex-0"}, Account{"codex", "codex-1"},
		Account{"xai", "xai-0"}, Account{"openrouter", "key-0"})
}

// The poller ticks once per request spacing, and no tick wakes at quite the
// offset the last one did: each lands a few milliseconds either side of it. A
// tick that wakes a moment earlier than the one before must still send, or
// about half the ticks are lost and every round of polls takes twice as long.
func TestEveryOnTimeTickSendsDespiteJitter(t *testing.T) {
	for _, spacing := range []time.Duration{time.Second, 10 * time.Second, 30 * time.Second} {
		t.Run(spacing.String(), func(t *testing.T) {
			c, f, opts := fixture(t)
			c.SetSchedule(opts.Interval, spacing)
			f.accounts = manyAccounts()
			rng := rand.New(rand.NewPCG(1, uint64(spacing)))
			for tick := 0; tick < len(f.accounts); tick++ {
				// Woken up to 20ms late, in no particular order.
				jitter := time.Duration(rng.Int64N(int64(20 * time.Millisecond)))
				before := f.calls
				step(t, c, f.now.Add(time.Duration(tick)*spacing+jitter))
				if f.calls != before+1 {
					t.Fatalf("tick %d, %s late, sent nothing; %d of %d credentials read", tick, jitter, f.calls, len(f.accounts))
				}
			}
		})
	}
}

// However often Step is called, two requests are never closer together than
// the spacing less a small tolerance, and on average never closer than the
// spacing itself: admitting a request a moment early must not let the next
// one come early too.
func TestRequestsNeverOutpaceTheSpacing(t *testing.T) {
	c, f, opts := fixture(t)
	// Ten credentials at a one-minute interval and ten-second spacing always
	// have one due, so only the spacing holds requests back.
	c.SetSchedule(time.Minute, opts.Spacing)
	f.accounts = manyAccounts()
	rng := rand.New(rand.NewPCG(2, 3))
	var sent []time.Time
	end := f.now.Add(30 * time.Minute)
	for at := f.now; at.Before(end); at = at.Add(time.Duration(50+rng.Int64N(100)) * time.Millisecond) {
		before := f.calls
		step(t, c, at)
		if f.calls != before {
			sent = append(sent, at)
		}
	}
	tolerance := min(time.Second, opts.Spacing/10)
	for i := 1; i < len(sent); i++ {
		if gap := sent[i].Sub(sent[i-1]); gap < opts.Spacing-tolerance {
			t.Fatalf("requests %d and %d were %s apart; want at least %s", i-1, i, gap, opts.Spacing-tolerance)
		}
	}
	if most := int(end.Sub(f.now)/opts.Spacing) + 1; len(sent) > most {
		t.Fatalf("%d requests in %s; want at most %d at %s spacing", len(sent), end.Sub(f.now), most, opts.Spacing)
	}
	// And the spacing is not wasted either: nearly every slot is used.
	if least := int(end.Sub(f.now)/opts.Spacing) * 9 / 10; len(sent) < least {
		t.Fatalf("%d requests in %s; want at least %d", len(sent), end.Sub(f.now), least)
	}
}
