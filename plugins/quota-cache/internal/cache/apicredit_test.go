package cache

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// creditFetcher lists Claude API credit accounts beside whatever else it is
// given and answers each fetch with result, recording which accounts were
// fetched.
type creditFetcher struct {
	accounts []Account
	fetched  []string
	result   func(Account) (Observation, error)
	reports  []Failure
}

func (f *creditFetcher) ReportFailure(_ context.Context, failure Failure) {
	f.reports = append(f.reports, failure)
}

func (f *creditFetcher) List(context.Context) ([]Account, error) { return f.accounts, nil }
func (f *creditFetcher) Fetch(_ context.Context, a Account, _ *client.AccountDetails) (Observation, error) {
	f.fetched = append(f.fetched, a.AuthIndex)
	if f.result != nil {
		return f.result(a)
	}
	return Observation{Quota: &client.Quota{Schema: 1}, RequestSent: true, HTTPStatus: 200}, nil
}

func creditAccount(id, fingerprint, renews string) Account {
	return Account{Provider: client.ProviderAnthropicAPI, AuthIndex: id, Credit: &client.APICredit{
		Label: id, MonthlyUSD: "200", Renews: renews, KeyFingerprint: fingerprint,
	}}
}

func creditCache(t *testing.T, f *creditFetcher) (*Cache, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cache", "snapshot.json")
	c, err := Open(Options{Path: path, Interval: 15 * time.Minute, Spacing: 10 * time.Second}, f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c, path
}

func entryOf(t *testing.T, path, authIndex string) client.Entry {
	t.Helper()
	snapshot, err := client.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := snapshot.Entries[client.Key(client.ProviderAnthropicAPI, authIndex)]
	if !ok {
		t.Fatalf("no entry for %s", authIndex)
	}
	return entry
}

var creditStart = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// A misconfigured item is in the snapshot, saying what is wrong, from the
// first scan, and is never fetched: it never fails and never backs off.
func TestCreditConfigurationIsWrittenAtScanAndAProblemIsNeverPolled(t *testing.T) {
	broken := creditAccount("item-2", "", "")
	broken.Credit.Problem = client.CreditProblemRenewsMissing
	f := &creditFetcher{accounts: []Account{broken}}
	c, path := creditCache(t, f)
	for i := 0; i < 4; i++ {
		step(t, c, creditStart.Add(time.Duration(i)*time.Hour))
	}
	entry := entryOf(t, path, "item-2")
	if len(f.fetched) != 0 || entry.APICredit == nil || *entry.APICredit != *broken.Credit ||
		entry.Failures != 0 || entry.LastError != "" || !entry.LastAttempt.IsZero() || entry.Quota != nil {
		t.Fatalf("fetched=%v entry=%+v", f.fetched, entry)
	}
	// Its neighbour is polled as usual.
	f.accounts = append(f.accounts, creditAccount("label-a", "key-a", "2026-10-29"))
	step(t, c, creditStart.Add(5*time.Hour))
	if len(f.fetched) != 1 || f.fetched[0] != "label-a" {
		t.Fatalf("fetched=%v", f.fetched)
	}
	if entry := entryOf(t, path, "label-a"); entry.APICredit == nil || entry.APICredit.KeyFingerprint != "key-a" || entry.Quota == nil {
		t.Fatalf("entry=%+v", entry)
	}
}

// A new key may describe another organization and has earned none of the old
// key's failures; a moved renewal date or a fixed problem changes what a poll
// reads or whether it runs. Each makes the entry due at once. A new amount
// changes nothing that is fetched.
func TestCreditChangesThatAffectThePollMakeItDue(t *testing.T) {
	for _, tc := range []struct {
		name          string
		change        func(*client.APICredit)
		due, forgiven bool
	}{
		{"key", func(c *client.APICredit) { c.KeyFingerprint = "key-new" }, true, true},
		{"renews", func(c *client.APICredit) { c.Renews = "2026-10-05" }, true, false},
		{"problem", func(c *client.APICredit) { c.Problem = client.CreditProblemMonthlyInvalid }, true, false},
		{"monthly", func(c *client.APICredit) { c.MonthlyUSD = "260.50" }, false, false},
		{"position", func(c *client.APICredit) { c.Position = 3 }, false, false},
	} {
		account := creditAccount("label-a", "key-old", "2026-10-29")
		f := &creditFetcher{accounts: []Account{account}, result: func(Account) (Observation, error) {
			return Observation{RequestSent: true, HTTPStatus: 401}, PollError{Message: client.CreditErrorKeyRejected}
		}}
		c, path := creditCache(t, f)
		step(t, c, creditStart)
		// A provider cooldown holds the poll back, so the change is seen
		// without a poll following it. A credit 429 never sets one; it
		// stands in for anything that delays the next poll.
		c.data.ProviderCooldown[client.ProviderAnthropicAPI] = creditStart.Add(time.Hour)
		changed := *account.Credit
		tc.change(&changed)
		account.Credit = &changed
		f.accounts = []Account{account}
		step(t, c, creditStart.Add(time.Minute))
		entry := entryOf(t, path, "label-a")
		if *entry.APICredit != changed || len(f.fetched) != 1 {
			t.Fatalf("%s: credit=%+v fetched=%v", tc.name, entry.APICredit, f.fetched)
		}
		if due := entry.NextAttempt.IsZero(); due != tc.due {
			t.Fatalf("%s: next attempt %v, due=%v want %v", tc.name, entry.NextAttempt, due, tc.due)
		}
		forgiven := entry.Failures == 0 && entry.LastError == ""
		if forgiven != tc.forgiven || (!forgiven && (entry.Failures != 1 || entry.LastError != client.CreditErrorKeyRejected)) {
			t.Fatalf("%s: failures=%d last_error=%q", tc.name, entry.Failures, entry.LastError)
		}
	}
}

// A PollError's fixed message reaches the entry and the history; RateLimited
// still wins over it.
func TestCreditFailureMessageIsRecorded(t *testing.T) {
	f := &creditFetcher{accounts: []Account{creditAccount("label-a", "key-a", "2026-10-29")}, result: func(Account) (Observation, error) {
		return Observation{RequestSent: true, HTTPStatus: 403}, PollError{Message: client.CreditErrorForbidden}
	}}
	c, path := creditCache(t, f)
	step(t, c, creditStart)
	snapshot, _ := client.Load(path)
	entry := entryOf(t, path, "label-a")
	if entry.LastError != client.CreditErrorForbidden || entry.Failures != 1 || len(snapshot.History) != 1 ||
		snapshot.History[0].Error != client.CreditErrorForbidden || snapshot.History[0].Outcome != "failed" {
		t.Fatalf("entry=%+v history=%+v", entry, snapshot.History)
	}
	// An empty message is no message.
	f.result = func(Account) (Observation, error) { return Observation{RequestSent: true}, PollError{} }
	step(t, c, creditStart.Add(time.Hour))
	if entry := entryOf(t, path, "label-a"); entry.LastError != "quota fetch failed" {
		t.Fatalf("last_error=%q", entry.LastError)
	}
}

// At the end of a cycle the stored reading becomes last cycle's, so the first
// poll of the next one is never left an interval or a backoff later. An
// explicit Retry-After beyond it still wins.
func TestCreditPollIsCappedAtRenewal(t *testing.T) {
	renewal := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	beforeRenewal := renewal.Add(-10 * time.Minute)
	f := &creditFetcher{accounts: []Account{creditAccount("label-a", "key-a", "2026-10-29")}}
	c, path := creditCache(t, f)
	step(t, c, beforeRenewal)
	if entry := entryOf(t, path, "label-a"); !entry.NextAttempt.Equal(renewal) {
		t.Fatalf("after a success next attempt %v, want %v", entry.NextAttempt, renewal)
	}
	// Backing off from a run of failures is cut short at the renewal too.
	f.result = func(Account) (Observation, error) {
		return Observation{RequestSent: true, HTTPStatus: 500}, PollError{Message: client.CreditErrorUpstream}
	}
	c.data.Entries[client.Key(client.ProviderAnthropicAPI, "label-a")] = func() client.Entry {
		e := c.data.Entries[client.Key(client.ProviderAnthropicAPI, "label-a")]
		e.NextAttempt, e.Failures = time.Time{}, 5
		return e
	}()
	step(t, c, beforeRenewal.Add(time.Minute))
	if entry := entryOf(t, path, "label-a"); entry.Failures != 6 || !entry.NextAttempt.Equal(renewal) {
		t.Fatalf("after a backed-off failure: failures=%d next attempt %v", entry.Failures, entry.NextAttempt)
	}
	// The renewal poll runs at the renewal instant.
	f.result = nil
	step(t, c, renewal)
	if len(f.fetched) != 3 {
		t.Fatalf("fetched=%v; the renewal poll did not run", f.fetched)
	}
	if entry := entryOf(t, path, "label-a"); !entry.NextAttempt.Equal(renewal.Add(15 * time.Minute)) {
		t.Fatalf("in the new cycle next attempt %v", entry.NextAttempt)
	}
	// Retry-After past the renewal is the provider's word and stands, for
	// this organization alone.
	later := time.Date(2026, 11, 28, 23, 50, 0, 0, time.UTC)
	retry := later.Add(time.Hour)
	f.result = func(Account) (Observation, error) {
		return Observation{RequestSent: true, HTTPStatus: 429}, RateLimited{RetryAfter: retry}
	}
	c.data.Entries[client.Key(client.ProviderAnthropicAPI, "label-a")] = func() client.Entry {
		e := c.data.Entries[client.Key(client.ProviderAnthropicAPI, "label-a")]
		e.NextAttempt = time.Time{}
		return e
	}()
	step(t, c, later)
	snapshot, _ := client.Load(path)
	if entry := entryOf(t, path, "label-a"); !entry.NextAttempt.Equal(retry) || len(snapshot.ProviderCooldown) != 0 {
		t.Fatalf("next attempt %v cooldown %v, want %v and no provider pause", entry.NextAttempt, snapshot.ProviderCooldown, retry)
	}
}

// Each organization has its own Admin API key and its own Anthropic rate
// limits, and the spend-cap 429 has no Retry-After and keeps failing. A 429
// therefore pauses only the organization that got it: its retries settle at
// one an hour, as any 429's do since 0.1.12, the provider is never paused,
// and every other organization keeps being read each interval. Its failure
// report says it was rate limited and paused nothing else.
func TestACreditRateLimitPausesOnlyItsOwnOrganization(t *testing.T) {
	capped := creditAccount("label-capped", "key-capped", "2026-10-29")
	other := creditAccount("label-other", "key-other", "2026-10-29")
	f := &creditFetcher{accounts: []Account{capped, other}, result: func(a Account) (Observation, error) {
		if a.AuthIndex == "label-capped" {
			return Observation{RequestSent: true, HTTPStatus: 429}, RateLimited{}
		}
		return Observation{Quota: &client.Quota{Schema: 1}, RequestSent: true, HTTPStatus: 200}, nil
	}}
	c, path := creditCache(t, f)
	interval := 15 * time.Minute
	cappedKey, otherKey := client.Key(client.ProviderAnthropicAPI, "label-capped"), client.Key(client.ProviderAnthropicAPI, "label-other")
	var lastRead time.Time
	end := creditStart.Add(8 * time.Hour)
	for at := creditStart; at.Before(end); at = at.Add(time.Minute) {
		step(t, c, at)
		snapshot := load(t, path)
		if len(snapshot.ProviderCooldown) != 0 {
			t.Fatalf("%v: provider paused %v", at, snapshot.ProviderCooldown)
		}
		if entry := snapshot.Entries[otherKey]; !entry.LastAttempt.Equal(lastRead) {
			if !lastRead.IsZero() && entry.LastAttempt.Sub(lastRead) > interval+time.Minute {
				t.Fatalf("label-other went %s without a reading", entry.LastAttempt.Sub(lastRead))
			}
			lastRead = entry.LastAttempt
		}
		if entry := snapshot.Entries[cappedKey]; entry.NextAttempt.Sub(entry.LastAttempt) > time.Hour {
			t.Fatalf("%v: label-capped backs off %s after %d failures; a 429 stops at an hour", at, entry.NextAttempt.Sub(entry.LastAttempt), entry.Failures)
		}
	}
	if entry := load(t, path).Entries[cappedKey]; entry.Failures < 8 || entry.LastError != "provider rate limited" {
		t.Fatalf("label-capped: %+v", entry)
	}
	if len(f.reports) == 0 {
		t.Fatal("no failure was reported")
	}
	for _, report := range f.reports {
		if report.Provider != client.ProviderAnthropicAPI || report.AuthIndex != "label-capped" || !report.RateLimited ||
			report.HTTPStatus != 429 || !report.ProviderPause.IsZero() || report.NextAttempt.IsZero() {
			t.Fatalf("report %+v", report)
		}
	}

	// With Retry-After the organization that sent it waits until then, and
	// still nothing else does.
	at := end
	retry := at.Add(3 * time.Hour)
	f.result = func(a Account) (Observation, error) {
		if a.AuthIndex == "label-other" {
			return Observation{RequestSent: true, HTTPStatus: 429}, RateLimited{RetryAfter: retry}
		}
		return Observation{Quota: &client.Quota{Schema: 1}, RequestSent: true, HTTPStatus: 200}, nil
	}
	e := c.data.Entries[otherKey]
	e.NextAttempt = time.Time{}
	c.data.Entries[otherKey] = e
	c.data.NextRequest = time.Time{}
	step(t, c, at)
	snapshot := load(t, path)
	if entry := snapshot.Entries[otherKey]; !entry.NextAttempt.Equal(retry) || len(snapshot.ProviderCooldown) != 0 {
		t.Fatalf("next=%v cooldown=%v want %v and no provider pause", entry.NextAttempt, snapshot.ProviderCooldown, retry)
	}
	if report := f.reports[len(f.reports)-1]; report.AuthIndex != "label-other" || !report.RetryAfter.Equal(retry) || !report.ProviderPause.IsZero() {
		t.Fatalf("report %+v", report)
	}
}

// The per-organization rule is for credit accounts alone: a 429 from any
// other provider still pauses all of that provider's credentials for one
// interval, as 0.1.12 does, and a credit 429 beside it changes nothing there.
func TestOtherProvidersStillPauseOnARateLimit(t *testing.T) {
	f := &creditFetcher{accounts: []Account{{Provider: "claude", AuthIndex: "one"}, creditAccount("label-a", "key-a", "2026-10-29")},
		result: func(Account) (Observation, error) {
			return Observation{RequestSent: true, HTTPStatus: 429}, RateLimited{}
		}}
	c, path := creditCache(t, f)
	key := client.Key("claude", "one")
	step(t, c, creditStart)
	e := c.data.Entries[key]
	e.NextAttempt, e.Failures = time.Time{}, 4
	c.data.Entries[key] = e
	c.data.ProviderCooldown = map[string]time.Time{}
	c.data.NextRequest = time.Time{}
	step(t, c, creditStart.Add(time.Hour))
	snapshot := load(t, path)
	entry := snapshot.Entries[key]
	if want := creditStart.Add(time.Hour + 15*time.Minute); !snapshot.ProviderCooldown["claude"].Equal(want) || !entry.NextAttempt.Equal(creditStart.Add(2*time.Hour)) {
		t.Fatalf("next=%v cooldown=%v want the pause at %v", entry.NextAttempt, snapshot.ProviderCooldown, want)
	}
	if _, ok := snapshot.ProviderCooldown[client.ProviderAnthropicAPI]; ok {
		t.Fatalf("cooldown=%v; a credit 429 paused its provider", snapshot.ProviderCooldown)
	}
}

// A poll-interval change keeps both credit rules: a time it sets is capped at
// the renewal ending the cycle of the last attempt, and an entry a key
// rotation made due stays due while something, here a provider cooldown set
// by hand, holds it back.
func TestCreditScheduleChangeKeepsTheRenewalCapAndADueEntry(t *testing.T) {
	renewal := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	f := &creditFetcher{accounts: []Account{creditAccount("label-a", "key-a", "2026-10-29")}}
	c, path := creditCache(t, f)
	step(t, c, renewal.Add(-10*time.Minute))
	c.SetSchedule(24*time.Hour, 10*time.Second)
	step(t, c, renewal.Add(-9*time.Minute))
	if entry := entryOf(t, path, "label-a"); !entry.NextAttempt.Equal(renewal) {
		t.Fatalf("after a schedule change next attempt %v, want the renewal %v", entry.NextAttempt, renewal)
	}

	account := creditAccount("label-b", "key-old", "2026-10-29")
	f = &creditFetcher{accounts: []Account{account}, result: func(Account) (Observation, error) {
		return Observation{RequestSent: true, HTTPStatus: 401}, PollError{Message: client.CreditErrorKeyRejected}
	}}
	c, path = creditCache(t, f)
	step(t, c, creditStart)
	c.data.ProviderCooldown[client.ProviderAnthropicAPI] = creditStart.Add(time.Hour)
	rotated := *account.Credit
	rotated.KeyFingerprint = "key-new"
	f.accounts = []Account{{Provider: account.Provider, AuthIndex: account.AuthIndex, Credit: &rotated}}
	step(t, c, creditStart.Add(time.Minute))
	c.SetSchedule(30*time.Minute, 10*time.Second)
	step(t, c, creditStart.Add(2*time.Minute))
	if entry := entryOf(t, path, "label-b"); !entry.NextAttempt.IsZero() || len(f.fetched) != 1 {
		t.Fatalf("after a rotation and a schedule change next attempt %v fetched=%v; want still due", entry.NextAttempt, f.fetched)
	}
	// It is polled the moment the cooldown ends.
	f.result = nil
	step(t, c, creditStart.Add(time.Hour))
	if len(f.fetched) != 2 {
		t.Fatalf("fetched=%v; the rotated key was not polled when the cooldown ended", f.fetched)
	}
}

// A Retry-After already in the past (clock skew, or a date left from an
// earlier response) changes nothing: the organization backs off as any 429
// does, at most an hour, and the provider is not paused.
func TestCreditRateLimitWithAPastRetryAfterBacksOffOnlyTheOrganization(t *testing.T) {
	f := &creditFetcher{accounts: []Account{creditAccount("label-a", "key-a", "2026-10-29")}, result: func(Account) (Observation, error) {
		return Observation{RequestSent: true, HTTPStatus: 429}, RateLimited{RetryAfter: creditStart.Add(-time.Hour)}
	}}
	c, path := creditCache(t, f)
	key := client.Key(client.ProviderAnthropicAPI, "label-a")
	step(t, c, creditStart.Add(-time.Hour))
	e := c.data.Entries[key]
	e.NextAttempt, e.Failures = time.Time{}, 4
	c.data.Entries[key] = e
	c.data.NextRequest = time.Time{}
	step(t, c, creditStart)
	snapshot := load(t, path)
	if entry := snapshot.Entries[key]; !entry.NextAttempt.Equal(creditStart.Add(time.Hour)) || len(snapshot.ProviderCooldown) != 0 {
		t.Fatalf("next=%v cooldown=%v, want the entry at %v and no provider pause", entry.NextAttempt, snapshot.ProviderCooldown, creditStart.Add(time.Hour))
	}
}
