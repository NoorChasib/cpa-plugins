package cache

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

var roundStart = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

// roundCache holds four polled credentials and a Claude API credit item, each
// polled once, ten seconds apart from roundStart, in key order: claude a and
// b, codex c, xai d. Each is next due fifteen minutes after its poll, and the
// next request slot is at roundStart+40s. fail names the auth indexes whose
// first poll fails, and how.
func roundCache(t *testing.T, fail map[string]error) (*Cache, *creditFetcher, string) {
	t.Helper()
	f := &creditFetcher{accounts: []Account{
		{Provider: "claude", AuthIndex: "a"}, {Provider: "claude", AuthIndex: "b"},
		{Provider: "codex", AuthIndex: "c"}, {Provider: "xai", AuthIndex: "d"},
		creditAccount(client.APICreditOrgAccount(fakeOrg), fakeOrg),
	}}
	failed := map[string]bool{}
	f.result = func(a Account) (Observation, error) {
		if err := fail[a.AuthIndex]; err != nil && !failed[a.AuthIndex] {
			failed[a.AuthIndex] = true
			return Observation{RequestSent: true, HTTPStatus: 500}, err
		}
		return Observation{Percent: 10, ObservedAt: roundStart, RequestSent: true, HTTPStatus: 200}, nil
	}
	c, path := creditCache(t, f)
	for i := range 4 {
		step(t, c, roundStart.Add(time.Duration(i)*10*time.Second))
	}
	if strings.Join(f.fetched, ",") != "a,b,c,d" {
		t.Fatalf("setup polled %v", f.fetched)
	}
	f.fetched = nil
	return c, f, path
}

// stepEvery steps n times, one spacing apart from start, and returns what the
// round has left after each.
func stepEvery(t *testing.T, c *Cache, start time.Time, n int) []int {
	t.Helper()
	var left []int
	for i := range n {
		step(t, c, start.Add(time.Duration(i)*10*time.Second))
		left = append(left, c.RoundRemaining())
	}
	return left
}

func (c *Cache) entriesNow() map[string]client.Entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.data.Entries)
}

func (c *Cache) nextRequest() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data.NextRequest
}

var creditKey = client.Key(client.ProviderAnthropicAPI, client.APICreditOrgAccount(fakeOrg))

// The credential a reset was spent on goes first, then every other one in the
// usual order, one per spacing. The spacing is untouched, and the credit item
// is never polled and keeps its empty schedule.
func TestRequestRoundPollsTheTargetFirstThenTheRest(t *testing.T) {
	c, f, path := roundCache(t, nil)
	now := roundStart.Add(5 * time.Minute)
	before := c.nextRequest()
	if !c.RequestRound(client.Key("codex", "c"), now) {
		t.Fatal("the round did not start")
	}
	if c.RoundRemaining() != 4 || !c.nextRequest().Equal(before) {
		t.Fatalf("remaining %d, next request %v (was %v)", c.RoundRemaining(), c.nextRequest(), before)
	}
	entries := c.entriesNow()
	if target := entries[client.Key("codex", "c")]; !target.NextAttempt.Equal(now.Add(-15 * time.Minute)) {
		t.Fatalf("target next attempt %v", target.NextAttempt)
	}
	for _, key := range []string{"claude:a", "claude:b", "xai:d"} {
		if !entries[key].NextAttempt.Equal(now) {
			t.Fatalf("%s next attempt %v; want now", key, entries[key].NextAttempt)
		}
	}
	if left := stepEvery(t, c, now, 6); !reflect.DeepEqual(left, []int{3, 2, 1, 0, 0, 0}) || strings.Join(f.fetched, ",") != "c,a,b,d" {
		t.Fatalf("polled %v, remaining after each step %v", f.fetched, left)
	}
	credit := load(t, path).Entries[creditKey]
	if credit.APICredit == nil || !credit.LastAttempt.IsZero() || !credit.NextAttempt.IsZero() {
		t.Fatalf("credit entry %+v", credit)
	}
	// Each polled credential is next due one interval after its round poll.
	for key, entry := range load(t, path).Entries {
		if key != creditKey && !entry.NextAttempt.Equal(entry.LastAttempt.Add(15*time.Minute)) {
			t.Fatalf("%s next attempt %v after %v", key, entry.NextAttempt, entry.LastAttempt)
		}
	}
}

// The target goes first while a backlog drains too: credentials overdue by
// more than an interval, as after CPA was down, and one never polled yet all
// wait behind it, and a request during the round puts its own target ahead
// of them as well.
func TestATargetGoesAheadOfABacklog(t *testing.T) {
	c, f, _ := roundCache(t, nil)
	f.accounts = append(f.accounts, Account{Provider: "codex", AuthIndex: "e"}, Account{Provider: "codex", AuthIndex: "g"})
	back := roundStart.Add(45 * time.Minute)
	// The first poll after the outage takes one of the two new credentials,
	// which have never been polled, and leaves the other one due since ever.
	step(t, c, back)
	if strings.Join(f.fetched, ",") != "e" {
		t.Fatalf("catch-up polled %v", f.fetched)
	}
	f.fetched = nil
	// d is the credential overdue the least, by more than an interval.
	if !c.RequestRound(client.Key("xai", "d"), back.Add(5*time.Second)) || c.RoundRemaining() != 6 {
		t.Fatalf("remaining %d", c.RoundRemaining())
	}
	step(t, c, back.Add(10*time.Second))
	if !c.RequestRound(client.Key("codex", "e"), back.Add(15*time.Second)) || c.RoundRemaining() != 5 {
		t.Fatalf("remaining %d", c.RoundRemaining())
	}
	if left := stepEvery(t, c, back.Add(20*time.Second), 6); !reflect.DeepEqual(left, []int{4, 3, 2, 1, 0, 0}) || strings.Join(f.fetched, ",") != "d,e,g,a,b,c" {
		t.Fatalf("polled %v, remaining after each step %v", f.fetched, left)
	}
}

// Of two targets both still waiting, the later request's is polled first,
// then the earlier one's, then the rest of the round: when both requests
// come in one read of Quota Glance's file, and when both wait out their
// provider's pause.
func TestTheLatestTargetGoesAheadOfAnEarlierOneStillWaiting(t *testing.T) {
	c, f, _ := roundCache(t, nil)
	now := roundStart.Add(5 * time.Minute)
	if !c.RequestRound(client.Key("claude", "a"), now) || !c.RequestRound(client.Key("claude", "b"), now) || c.RoundRemaining() != 4 {
		t.Fatalf("remaining %d", c.RoundRemaining())
	}
	if left := stepEvery(t, c, now, 5); !reflect.DeepEqual(left, []int{3, 2, 1, 0, 0}) || strings.Join(f.fetched, ",") != "b,a,c,d" {
		t.Fatalf("one read: polled %v, remaining after each step %v", f.fetched, left)
	}

	// b's 429 pauses claude.
	c, f, path := roundCache(t, map[string]error{"b": RateLimited{}})
	pause := load(t, path).ProviderCooldown["claude"]
	if !c.RequestRound(client.Key("claude", "a"), now) || !c.RequestRound(client.Key("claude", "b"), now.Add(time.Second)) || c.RoundRemaining() != 4 {
		t.Fatalf("remaining %d", c.RoundRemaining())
	}
	if left := stepEvery(t, c, now, 3); !reflect.DeepEqual(left, []int{3, 2, 2}) || strings.Join(f.fetched, ",") != "c,d" {
		t.Fatalf("during the pause polled %v, remaining after each step %v", f.fetched, left)
	}
	f.fetched = nil
	stepEvery(t, c, pause, 2)
	if strings.Join(f.fetched, ",") != "b,a" || c.RoundRemaining() != 0 {
		t.Fatalf("after the pause polled %v, remaining %d", f.fetched, c.RoundRemaining())
	}
}

// The round waits for the next free slot: a request one second after a poll
// sends nothing until the spacing is over.
func TestRequestRoundLeavesTheSpacingAlone(t *testing.T) {
	c, f, _ := roundCache(t, nil)
	last := roundStart.Add(30 * time.Second)
	if !c.RequestRound(client.Key("claude", "b"), last.Add(time.Second)) {
		t.Fatal("the round did not start")
	}
	step(t, c, last.Add(time.Second))
	step(t, c, last.Add(5*time.Second))
	if len(f.fetched) != 0 {
		t.Fatalf("polled %v inside the spacing", f.fetched)
	}
	step(t, c, last.Add(10*time.Second))
	if strings.Join(f.fetched, ",") != "b" {
		t.Fatalf("polled %v; want the target at the next slot", f.fetched)
	}
}

// The target's own failure backoff is ignored, since the reset was just spent
// on it. Another credential backing off after a failure keeps its backoff and
// is not in the round.
func TestRequestRoundKeepsOtherFailureBackoffAndIgnoresTheTargets(t *testing.T) {
	c, f, path := roundCache(t, map[string]error{"a": errors.New("unauthorized"), "b": errors.New("unauthorized")})
	backoff := load(t, path).Entries["claude:b"].NextAttempt
	now := roundStart.Add(5 * time.Minute)
	if !c.RequestRound(client.Key("claude", "a"), now) || c.RoundRemaining() != 3 {
		t.Fatalf("remaining %d", c.RoundRemaining())
	}
	if left := stepEvery(t, c, now, 5); !reflect.DeepEqual(left, []int{2, 1, 0, 0, 0}) || strings.Join(f.fetched, ",") != "a,c,d" {
		t.Fatalf("polled %v, remaining %v", f.fetched, left)
	}
	if got := load(t, path).Entries["claude:b"]; !got.NextAttempt.Equal(backoff) || got.LastError == "" {
		t.Fatalf("the failed credential's backoff moved: %+v", got)
	}
}

// A provider inside a 429 pause stays paused. Its credentials in the round
// wait for the pause to end, and the target still goes first then.
func TestRequestRoundWaitsOutAProviderPause(t *testing.T) {
	c, f, path := roundCache(t, map[string]error{"b": RateLimited{}})
	pause := load(t, path).ProviderCooldown["claude"]
	now := roundStart.Add(5 * time.Minute)
	if !c.RequestRound(client.Key("claude", "a"), now) || c.RoundRemaining() != 3 {
		t.Fatalf("remaining %d", c.RoundRemaining())
	}
	if left := stepEvery(t, c, now, 4); !reflect.DeepEqual(left, []int{2, 1, 1, 1}) || strings.Join(f.fetched, ",") != "c,d" {
		t.Fatalf("polled %v during the pause, remaining %v", f.fetched, left)
	}
	if !load(t, path).ProviderCooldown["claude"].Equal(pause) {
		t.Fatal("the round moved the provider's pause")
	}
	f.fetched = nil
	step(t, c, pause)
	if strings.Join(f.fetched, ",") != "a" || c.RoundRemaining() != 0 {
		t.Fatalf("after the pause polled %v, remaining %d", f.fetched, c.RoundRemaining())
	}
}

// Only a polled entry of the latest scan can be a target. Anything else
// changes nothing, saves nothing and starts no round.
func TestRequestRoundIgnoresATargetThatIsNotPolled(t *testing.T) {
	c, _, _ := roundCache(t, nil)
	before := c.entriesNow()
	now := roundStart.Add(5 * time.Minute)
	for _, key := range []string{creditKey, client.Key("codex", "missing"), client.Key("claude", "c"), "c"} {
		if c.RequestRound(key, now) {
			t.Fatalf("%s started a round", key)
		}
	}
	c.mu.Lock()
	unsaved := c.unsaved
	c.mu.Unlock()
	if !reflect.DeepEqual(before, c.entriesNow()) || c.RoundRemaining() != 0 || unsaved {
		t.Fatal("an ignored request changed the schedule")
	}
	c.Close()
	if c.RequestRound(client.Key("codex", "c"), now) {
		t.Fatal("a closed cache started a round")
	}
}

// A request while a round still has credentials to poll brings only its own
// target forward: a credential polled earlier in the round is not polled
// again.
func TestARequestDuringARoundOnlyMovesItsTarget(t *testing.T) {
	c, f, _ := roundCache(t, nil)
	now := roundStart.Add(5 * time.Minute)
	c.RequestRound(client.Key("codex", "c"), now)
	stepEvery(t, c, now, 2)
	if strings.Join(f.fetched, ",") != "c,a" || c.RoundRemaining() != 2 {
		t.Fatalf("polled %v, remaining %d", f.fetched, c.RoundRemaining())
	}
	again := now.Add(20 * time.Second)
	if !c.RequestRound(client.Key("codex", "c"), again) || c.RoundRemaining() != 3 {
		t.Fatalf("remaining %d", c.RoundRemaining())
	}
	if a := c.entriesNow()["claude:a"]; !a.NextAttempt.After(again) {
		t.Fatalf("the credential already polled is due again: %v", a.NextAttempt)
	}
	f.fetched = nil
	if left := stepEvery(t, c, again, 4); !reflect.DeepEqual(left, []int{2, 1, 0, 0}) || strings.Join(f.fetched, ",") != "c,b,d" {
		t.Fatalf("polled %v, remaining %v", f.fetched, left)
	}
}

// A round held open only by a provider's pause is not under way: a request
// then starts a new round, which polls again what the first round polled
// before the pause. While the round can still poll something, a request
// still moves only its target.
func TestARequestDuringAPausedRoundRefreshesTheRest(t *testing.T) {
	c, f, path := roundCache(t, map[string]error{"b": RateLimited{}})
	pause := load(t, path).ProviderCooldown["claude"]
	now := roundStart.Add(time.Minute)
	if !c.RequestRound(client.Key("codex", "c"), now) || c.RoundRemaining() != 3 {
		t.Fatalf("remaining %d", c.RoundRemaining())
	}
	step(t, c, now)
	// d is still to poll, so the round is under way and c stays polled.
	joined := now.Add(5 * time.Second)
	if !c.RequestRound(client.Key("xai", "d"), joined) || c.RoundRemaining() != 2 {
		t.Fatalf("remaining %d", c.RoundRemaining())
	}
	if entry := c.entriesNow()["codex:c"]; !entry.NextAttempt.After(joined) {
		t.Fatalf("the credential the round polled is due again: %v", entry.NextAttempt)
	}
	step(t, c, now.Add(10*time.Second))
	if strings.Join(f.fetched, ",") != "c,d" || c.RoundRemaining() != 1 {
		t.Fatalf("polled %v, remaining %d", f.fetched, c.RoundRemaining())
	}
	// Only claude:a is left, waiting out the pause.
	again := now.Add(10 * time.Minute)
	if !c.RequestRound(client.Key("codex", "c"), again) || c.RoundRemaining() != 3 {
		t.Fatalf("remaining %d", c.RoundRemaining())
	}
	if d := c.entriesNow()["xai:d"]; !d.NextAttempt.Equal(again) {
		t.Fatalf("xai:d next attempt %v; want now", d.NextAttempt)
	}
	f.fetched = nil
	if left := stepEvery(t, c, again, 4); !reflect.DeepEqual(left, []int{2, 1, 1, 1}) || strings.Join(f.fetched, ",") != "c,d" {
		t.Fatalf("polled %v, remaining %v", f.fetched, left)
	}
	f.fetched = nil
	step(t, c, pause)
	if strings.Join(f.fetched, ",") != "a" || c.RoundRemaining() != 0 {
		t.Fatalf("after the pause polled %v, remaining %d", f.fetched, c.RoundRemaining())
	}
}

// The target of a request still waiting out its provider's pause keeps its
// place when a later request starts a new round: once the pause ends, it goes
// ahead of a credential of that provider never polled yet.
func TestAWaitingTargetKeepsItsPlaceInANewRound(t *testing.T) {
	pause := roundStart.Add(2 * time.Hour)
	c, f, _ := roundCache(t, map[string]error{"b": RateLimited{RetryAfter: pause}})
	f.accounts = append(f.accounts, Account{Provider: "claude", AuthIndex: "e"})
	now := roundStart.Add(5 * time.Minute)
	// The scan lists e, which the pause keeps from being polled.
	step(t, c, now)
	if !c.RequestRound(client.Key("claude", "a"), now) || c.RoundRemaining() != 4 {
		t.Fatalf("remaining %d", c.RoundRemaining())
	}
	stepEvery(t, c, now.Add(10*time.Second), 2)
	// Only claude's credentials are left, so this request starts a new round.
	again := now.Add(time.Minute)
	if !c.RequestRound(client.Key("codex", "c"), again) || c.RoundRemaining() != 4 {
		t.Fatalf("remaining %d", c.RoundRemaining())
	}
	stepEvery(t, c, again, 2)
	step(t, c, pause)
	step(t, c, pause.Add(10*time.Second))
	if strings.Join(f.fetched, ",") != "c,d,c,d,a,e" || c.RoundRemaining() != 0 {
		t.Fatalf("polled %v, remaining %d", f.fetched, c.RoundRemaining())
	}
}

// What a round changes is saved with the next save, even when that Step can
// poll nothing, and not before. It is saved once: a later Step that polls
// nothing writes nothing.
func TestARoundIsSavedWithTheNextSave(t *testing.T) {
	c, f, path := roundCache(t, map[string]error{"c": RateLimited{}, "d": RateLimited{}})
	// Every provider paused: claude by hand, codex and xai by their 429s.
	c.mu.Lock()
	c.data.ProviderCooldown["claude"] = roundStart.Add(time.Hour)
	c.mu.Unlock()
	now := roundStart.Add(5 * time.Minute)
	c.RequestRound(client.Key("claude", "a"), now)
	if a := load(t, path).Entries["claude:a"]; !a.NextAttempt.Equal(roundStart.Add(15 * time.Minute)) {
		t.Fatalf("saved before the next save: %v", a.NextAttempt)
	}
	step(t, c, now)
	saved := load(t, path)
	if len(f.fetched) != 0 || !saved.Entries["claude:a"].NextAttempt.Equal(now.Add(-15*time.Minute)) || !saved.Entries["claude:b"].NextAttempt.Equal(now) {
		t.Fatalf("polled %v; saved %+v", f.fetched, saved.Entries)
	}
	step(t, c, now.Add(10*time.Second))
	if again := load(t, path); len(f.fetched) != 0 || !again.WrittenAt.Equal(saved.WrittenAt) {
		t.Fatalf("polled %v; written at %v, then again at %v", f.fetched, saved.WrittenAt, again.WrittenAt)
	}
}

// A schedule change during a round leaves its credentials due.
func TestAScheduleChangeKeepsTheRoundDue(t *testing.T) {
	c, f, _ := roundCache(t, nil)
	now := roundStart.Add(5 * time.Minute)
	c.RequestRound(client.Key("codex", "c"), now)
	c.SetSchedule(30*time.Minute, 10*time.Second)
	if stepEvery(t, c, now, 4); strings.Join(f.fetched, ",") != "c,a,b,d" || c.RoundRemaining() != 0 {
		t.Fatalf("polled %v, remaining %d", f.fetched, c.RoundRemaining())
	}
}

// A credential the roster stops listing leaves the round with its entry.
func TestARemovedCredentialLeavesTheRound(t *testing.T) {
	c, f, _ := roundCache(t, nil)
	now := roundStart.Add(5 * time.Minute)
	c.RequestRound(client.Key("codex", "c"), now)
	f.accounts = f.accounts[1:]
	step(t, c, now)
	if strings.Join(f.fetched, ",") != "c" || c.RoundRemaining() != 2 {
		t.Fatalf("polled %v, remaining %d", f.fetched, c.RoundRemaining())
	}
}

// Rounds requested between ticks, and status reads of what is left, race
// neither each other nor the poll in progress.
func TestRoundsRaceNeitherTicksNorReads(t *testing.T) {
	c, _, _ := roundCache(t, nil)
	var clock sync.Mutex
	at := roundStart.Add(time.Minute)
	tick := func() time.Time {
		clock.Lock()
		defer clock.Unlock()
		at = at.Add(time.Second)
		return at
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Go(func() {
		// Every way out releases the other two, so a failing Step fails the
		// test at once rather than hanging it.
		defer close(done)
		for range 300 {
			if err := c.Step(context.Background(), tick()); err != nil {
				t.Error(err)
				return
			}
		}
	})
	keys := []string{"claude:a", "claude:b", "codex:c", "xai:d", creditKey}
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			c.RequestRound(keys[i%len(keys)], tick())
		}
	})
	wg.Go(func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			if n := c.RoundRemaining(); n < 0 || n > 4 {
				t.Errorf("remaining %d", n)
				return
			}
		}
	})
	wg.Wait()
}
