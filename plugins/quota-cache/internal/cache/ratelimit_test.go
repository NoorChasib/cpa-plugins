package cache

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

func step(t *testing.T, c *Cache, at time.Time) {
	t.Helper()
	if err := c.Step(context.Background(), at); err != nil {
		t.Fatal(err)
	}
}

func load(t *testing.T, path string) client.Snapshot {
	t.Helper()
	s, err := client.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// One credential that keeps drawing 429s while its siblings answer must not
// lend the whole provider its own growing backoff. Every pause it causes lasts
// one interval, its own retries settle at one an hour, and its siblings keep
// being read in between.
func TestOneRepeatedlyRateLimitedCredentialPausesItsProviderOneIntervalAtATime(t *testing.T) {
	c, f, opts := fixture(t)
	f.accounts = []Account{{Provider: "claude", AuthIndex: "bad"}, {Provider: "claude", AuthIndex: "one"}, {Provider: "claude", AuthIndex: "two"}}
	f.failures = map[string]error{"bad": RateLimited{}}
	bad, end := client.Key("claude", "bad"), f.now.Add(12*time.Hour)
	lastRead := map[string]time.Time{}
	var limits uint64
	for at := f.now; at.Before(end); at = at.Add(time.Minute) {
		step(t, c, at)
		s := load(t, opts.Path)
		for _, id := range []string{"one", "two"} {
			entry := s.Entries[client.Key("claude", id)]
			if entry.LastError != "" || entry.LastAttempt.Equal(lastRead[id]) {
				continue
			}
			// A sibling waits out its own interval and at most one pause.
			if previous := lastRead[id]; !previous.IsZero() && entry.LastAttempt.Sub(previous) > 2*opts.Interval+3*time.Minute {
				t.Fatalf("claude:%s went %s without a reading", id, entry.LastAttempt.Sub(previous))
			}
			lastRead[id] = entry.LastAttempt
		}
		if s.Totals.RateLimits == limits {
			continue
		}
		limits = s.Totals.RateLimits
		if pause := s.ProviderCooldown["claude"].Sub(at); pause > opts.Interval {
			t.Fatalf("429 number %d paused every claude credential for %s; want at most one %s interval", limits, pause, opts.Interval)
		}
		if wait := s.Entries[bad].NextAttempt.Sub(at); wait > time.Hour {
			t.Fatalf("429 number %d backed the credential off %s; want at most an hour", limits, wait)
		}
	}
	// It is still probed, about once an hour once its backoff has settled.
	if limits < 12 {
		t.Fatalf("%d rate limits in twelve hours; want the credential probed hourly", limits)
	}
}

// A credential that has been failing for some other reason, such as a revoked
// token answering 401, has backed itself off for hours. When it then draws a
// 429, its siblings pause for one interval, not for the backoff it had earned.
func TestARateLimitAfterOtherFailuresPausesTheProviderOneInterval(t *testing.T) {
	c, f, opts := fixture(t)
	f.accounts = []Account{{Provider: "claude", AuthIndex: "dead"}, {Provider: "claude", AuthIndex: "one"}}
	f.failures = map[string]error{"dead": errors.New("synthetic 401")}
	dead := client.Key("claude", "dead")
	at := f.now
	// Four failures in a row: it backed off 15m, 30m, 1h and then 2h.
	for done := false; !done; at = at.Add(time.Minute) {
		step(t, c, at)
		done = load(t, opts.Path).Entries[dead].Failures == 4
	}
	f.failures["dead"] = RateLimited{}
	var limitedAt time.Time
	for ; limitedAt.IsZero(); at = at.Add(time.Minute) {
		step(t, c, at)
		if load(t, opts.Path).Totals.RateLimits == 1 {
			limitedAt = at
		}
	}
	s := load(t, opts.Path)
	if pause := s.ProviderCooldown["claude"]; !pause.Equal(limitedAt.Add(opts.Interval)) {
		t.Fatalf("provider paused until %s, %s after the 429; want one %s interval", pause, pause.Sub(limitedAt), opts.Interval)
	}
	if next := s.Entries[dead].NextAttempt; !next.Equal(limitedAt.Add(time.Hour)) {
		t.Fatalf("credential next attempt %s after the 429; want an hour", next.Sub(limitedAt))
	}
	// The sibling is read as soon as the pause ends.
	for ; !at.After(limitedAt.Add(opts.Interval)); at = at.Add(time.Minute) {
		step(t, c, at)
	}
	if read := load(t, opts.Path).Entries[client.Key("claude", "one")]; read.LastAttempt.Before(limitedAt.Add(opts.Interval)) || read.LastError != "" {
		t.Fatalf("sibling last read %s, error %q; want a reading when the pause ended", read.LastAttempt, read.LastError)
	}
}

// Retry-After is the provider's own instruction, so when it asks for longer
// than the pause or the capped backoff, it wins for both; when it asks for
// less, the pause and the backoff stand.
func TestRetryAfterWinsOnlyWhenLonger(t *testing.T) {
	for _, tc := range []struct {
		name               string
		retryAfter         time.Duration
		pause, credentials time.Duration
	}{
		{"none", 0, 15 * time.Minute, time.Hour},
		{"shorter than the interval", 5 * time.Minute, 15 * time.Minute, time.Hour},
		{"between the pause and the backoff", 30 * time.Minute, 30 * time.Minute, time.Hour},
		{"longer than both", 3 * time.Hour, 3 * time.Hour, 3 * time.Hour},
		{"longer than six hours", 10 * time.Hour, 10 * time.Hour, 10 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f, opts := fixture(t)
			key := client.Key("claude", "one")
			// Four failures first, so the backoff it would otherwise inherit is
			// four hours.
			at := f.now
			f.failure = errors.New("synthetic failure")
			for i := 0; i < 4; i++ {
				step(t, c, at)
				at = load(t, opts.Path).Entries[key].NextAttempt
			}
			limited := RateLimited{}
			if tc.retryAfter > 0 {
				limited.RetryAfter = at.Add(tc.retryAfter)
			}
			f.failure = limited
			step(t, c, at)
			s := load(t, opts.Path)
			if pause := s.ProviderCooldown["claude"].Sub(at); pause != tc.pause {
				t.Fatalf("provider paused %s; want %s", pause, tc.pause)
			}
			if wait := s.Entries[key].NextAttempt.Sub(at); wait != tc.credentials {
				t.Fatalf("credential backed off %s; want %s", wait, tc.credentials)
			}
		})
	}
}

// After repeated 429s a credential's own backoff settles at an hour, or at the
// interval when that is longer, instead of climbing to six hours: the provider
// pause already covers its siblings, and an hourly probe is what finds the
// limit lifted. Every other failure still climbs to six hours.
func TestRateLimitBackoffCapsAtAnHourOrTheInterval(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval time.Duration
		failure  error
		waits    []time.Duration
	}{
		// At a short interval the doubling takes several 429s to reach the
		// hour, so a long outage costs a few more than one an hour at first.
		{"429 at a 5m interval", 5 * time.Minute, RateLimited{},
			[]time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour}},
		{"429 at a 15m interval", 15 * time.Minute, RateLimited{},
			[]time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, time.Hour, time.Hour}},
		{"429 at a 2h interval", 2 * time.Hour, RateLimited{},
			[]time.Duration{2 * time.Hour, 2 * time.Hour, 2 * time.Hour}},
		{"other failures", 15 * time.Minute, errors.New("synthetic failure"),
			[]time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour, 6 * time.Hour, 6 * time.Hour}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f, opts := fixture(t)
			c.SetSchedule(tc.interval, opts.Spacing)
			f.failure = tc.failure
			key := client.Key("claude", "one")
			at := f.now
			for i, want := range tc.waits {
				step(t, c, at)
				next := load(t, opts.Path).Entries[key].NextAttempt
				if got := next.Sub(at); got != want {
					t.Fatalf("failure %d backed off %s; want %s", i+1, got, want)
				}
				at = next
			}
		})
	}
}

// When every credential of a provider is rate limited for hours, each backs
// itself off to an hour, and once there draws at most one 429 an hour. On the
// way, while its backoff is still doubling, a short interval lets it draw more,
// but each 429 pauses the provider for an interval, so the provider never
// draws more than one per interval. A sibling's success does not bring the
// others forward, so once the limit lifts each is read again when its own
// backoff runs out: the last of them can wait up to an hour, but no longer.
func TestALongProviderOutageSettlesAtHourlyProbesAndRecoversWithinAnHour(t *testing.T) {
	for _, tc := range []struct{ interval, spacing time.Duration }{
		{5 * time.Minute, 10 * time.Second},
		{15 * time.Minute, 30 * time.Second},
	} {
		t.Run(tc.interval.String(), func(t *testing.T) {
			c, f, opts := fixture(t)
			c.SetSchedule(tc.interval, tc.spacing)
			f.accounts, f.failures = nil, map[string]error{}
			for i := 0; i < 6; i++ {
				f.accounts = append(f.accounts, Account{Provider: "claude", AuthIndex: "claude-" + strconv.Itoa(i)})
			}
			at := f.now
			for ; f.calls < len(f.accounts); at = at.Add(tc.spacing) {
				step(t, c, at)
			}
			for _, a := range f.accounts {
				f.failures[a.AuthIndex] = RateLimited{}
			}
			outage := at
			// settled is when each credential's backoff first reached the hour.
			settled, last := map[string]time.Time{}, map[string]time.Time{}
			var previous time.Time
			for end := outage.Add(12 * time.Hour); at.Before(end); at = at.Add(tc.spacing) {
				before := len(f.reports)
				step(t, c, at)
				if len(f.reports) == before {
					continue
				}
				if gap := at.Sub(previous); !previous.IsZero() && gap < tc.interval {
					t.Fatalf("claude drew 429s %s apart; want at least one %s interval", gap, tc.interval)
				}
				previous = at
				id := f.reports[before].AuthIndex
				if !settled[id].IsZero() {
					if gap := at.Sub(last[id]); gap < time.Hour {
						t.Fatalf("claude:%s drew 429s %s apart after its backoff reached an hour", id, gap)
					}
				} else if f.reports[before].NextAttempt.Sub(at) == time.Hour {
					settled[id] = at
				}
				last[id] = at
			}
			for _, a := range f.accounts {
				if settled[a.AuthIndex].IsZero() {
					t.Fatalf("claude:%s backoff never reached an hour in a twelve-hour outage", a.AuthIndex)
				}
			}
			f.failures = nil
			recovered := at
			// The hour, and a tick for each credential that may fall due at once.
			for end := recovered.Add(time.Hour + time.Duration(len(f.accounts)+1)*tc.spacing); !at.After(end); at = at.Add(tc.spacing) {
				step(t, c, at)
			}
			s := load(t, opts.Path)
			for _, a := range f.accounts {
				if entry := s.Entries[client.Key(a.Provider, a.AuthIndex)]; entry.LastError != "" || entry.LastAttempt.Before(recovered) {
					t.Fatalf("claude:%s not read within an hour of the limit lifting at %s: last attempt %s, error %q",
						a.AuthIndex, recovered, entry.LastAttempt, entry.LastError)
				}
			}
		})
	}
}
