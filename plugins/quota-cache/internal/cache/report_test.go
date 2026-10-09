package cache

import (
	"errors"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// The snapshot keeps only the last hundred polls, so a failure older than that
// cannot be dated from it. Every failed poll is therefore reported once, with
// the schedule it earned exactly as the snapshot now holds it; a successful
// poll reports nothing.
func TestEveryFailedPollIsReportedWithTheScheduleItEarned(t *testing.T) {
	c, f, opts := fixture(t)
	f.accounts = append(f.accounts, Account{Provider: "codex", AuthIndex: "two"})
	same := func(got, want Failure) bool {
		return got.Provider == want.Provider && got.AuthIndex == want.AuthIndex && got.HTTPStatus == want.HTTPStatus &&
			got.RateLimited == want.RateLimited && got.RetryAfter.Equal(want.RetryAfter) &&
			got.NextAttempt.Equal(want.NextAttempt) && got.ProviderPause.Equal(want.ProviderPause)
	}

	step(t, c, f.now)
	if len(f.reports) != 0 {
		t.Fatalf("a successful poll was reported: %+v", f.reports)
	}

	at := f.now.Add(opts.Spacing)
	f.failures = map[string]error{"two": RateLimited{RetryAfter: at.Add(time.Hour)}}
	step(t, c, at)
	s := load(t, opts.Path)
	want := Failure{Provider: "codex", AuthIndex: "two", HTTPStatus: 200, RateLimited: true, RetryAfter: at.Add(time.Hour),
		NextAttempt: s.Entries[client.Key("codex", "two")].NextAttempt, ProviderPause: s.ProviderCooldown["codex"]}
	if len(f.reports) != 1 || !same(f.reports[0], want) || want.NextAttempt.IsZero() || want.ProviderPause.IsZero() {
		t.Fatalf("reports = %+v; want %+v", f.reports, want)
	}

	// Any other failure sets no pause and carries no Retry-After.
	at = f.now.Add(opts.Interval)
	f.failures, f.failure = nil, errors.New("synthetic failure")
	step(t, c, at)
	want = Failure{Provider: "claude", AuthIndex: "one", HTTPStatus: 200,
		NextAttempt: load(t, opts.Path).Entries[client.Key("claude", "one")].NextAttempt}
	if len(f.reports) != 2 || !same(f.reports[1], want) || !want.NextAttempt.Equal(at.Add(opts.Interval)) {
		t.Fatalf("reports = %+v; want %+v last", f.reports, want)
	}
}
