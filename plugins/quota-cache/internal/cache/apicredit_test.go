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
}

func (f *creditFetcher) List(context.Context) ([]Account, error) { return f.accounts, nil }
func (f *creditFetcher) Fetch(_ context.Context, a Account, _ *client.AccountDetails) (Observation, error) {
	f.fetched = append(f.fetched, a.AuthIndex)
	if f.result != nil {
		return f.result(a)
	}
	return Observation{Quota: &client.Quota{Schema: 1}, RequestSent: true, HTTPStatus: 200}, nil
}

const fakeOrg = "00000000-0000-4000-8000-00000000000a"

func creditAccount(id, org string) Account {
	return Account{Provider: client.ProviderAnthropicAPI, AuthIndex: id, Credit: &client.APICredit{
		Label: id, OrganizationID: org, MonthlyUSD: "200", Renews: "2026-10-29",
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

// Every credit item is in the snapshot from the first scan, carrying its
// configuration, and is never fetched, valid or not, due or not: what its
// organization spent comes from the meter. A credential beside it is polled
// as usual.
func TestCreditAccountsAreWrittenAtScanAndNeverFetched(t *testing.T) {
	good := creditAccount(client.APICreditOrgAccount(fakeOrg), fakeOrg)
	broken := creditAccount("item-2", "")
	broken.Credit.Problem = client.CreditProblemOrganizationIDMissing
	f := &creditFetcher{accounts: []Account{good, broken}}
	c, path := creditCache(t, f)
	for i := 0; i < 4; i++ {
		step(t, c, creditStart.Add(time.Duration(i)*time.Hour))
	}
	for _, a := range []Account{good, broken} {
		entry := entryOf(t, path, a.AuthIndex)
		if entry.APICredit == nil || *entry.APICredit != *a.Credit || entry.Failures != 0 || entry.LastError != "" ||
			!entry.LastAttempt.IsZero() || !entry.NextAttempt.IsZero() || entry.Quota != nil {
			t.Fatalf("%s: entry=%+v", a.AuthIndex, entry)
		}
	}
	// Made due by hand, it is still skipped.
	key := client.Key(client.ProviderAnthropicAPI, good.AuthIndex)
	e := c.data.Entries[key]
	e.NextAttempt = creditStart
	c.data.Entries[key] = e
	f.accounts = append(f.accounts, Account{Provider: "claude", AuthIndex: "one"})
	step(t, c, creditStart.Add(5*time.Hour))
	if len(f.fetched) != 1 || f.fetched[0] != "one" {
		t.Fatalf("fetched=%v", f.fetched)
	}
	// A changed configuration reaches the entry at the next scan.
	changed := *good.Credit
	changed.MonthlyUSD, changed.Renews = "260.50", "2026-10-31"
	f.accounts[0].Credit = &changed
	step(t, c, creditStart.Add(6*time.Hour))
	if entry := entryOf(t, path, good.AuthIndex); *entry.APICredit != changed {
		t.Fatalf("entry=%+v", entry.APICredit)
	}
	// Removing an item retires its entry.
	f.accounts = f.accounts[2:]
	step(t, c, creditStart.Add(7*time.Hour))
	if snapshot, _ := client.Load(path); len(snapshot.Entries) != 1 {
		t.Fatalf("entries=%v", snapshot.Entries)
	}
	for _, fetched := range f.fetched {
		if fetched != "one" {
			t.Fatalf("fetched=%v; a credit account was polled", f.fetched)
		}
	}
}

// An entry quota-cache 0.1.13 polled carries a reading, failures and a
// schedule. Adopting the configuration clears them all, once, and leaves the
// entry quiet after that.
func TestAdoptingACreditClearsWhatAnEarlierPollLeft(t *testing.T) {
	account := creditAccount(client.APICreditOrgAccount(fakeOrg), fakeOrg)
	f := &creditFetcher{accounts: []Account{account}}
	c, path := creditCache(t, f)
	key := client.Key(client.ProviderAnthropicAPI, account.AuthIndex)
	old := *account.Credit
	old.KeyFingerprint, old.OrganizationID = "key-0123456789ab", ""
	c.data.Entries[key] = client.Entry{Provider: account.Provider, AuthIndex: account.AuthIndex, APICredit: &old,
		Quota:    &client.Quota{Schema: 1, ObservedAt: creditStart, CostReport: &client.CostReport{OrganizationID: fakeOrg}},
		Failures: 3, LastError: client.CreditErrorKeyRejected, LastAttempt: creditStart.Add(-time.Hour), NextAttempt: creditStart.Add(time.Hour)}
	step(t, c, creditStart)
	entry := entryOf(t, path, account.AuthIndex)
	if *entry.APICredit != *account.Credit || entry.Quota != nil || entry.Failures != 0 || entry.LastError != "" || !entry.LastAttempt.IsZero() || !entry.NextAttempt.IsZero() {
		t.Fatalf("entry=%+v", entry)
	}
	written := load(t, path).WrittenAt
	step(t, c, creditStart.Add(time.Minute))
	if load(t, path).WrittenAt != written || len(f.fetched) != 0 {
		t.Fatalf("an unchanged item rewrote the snapshot, or was fetched (%v)", f.fetched)
	}
	// The leftovers are cleared even when the configuration itself is the
	// same as before.
	e := c.data.Entries[key]
	e.Failures, e.LastError = 2, "quota fetch failed"
	c.data.Entries[key] = e
	step(t, c, creditStart.Add(2*time.Minute))
	if entry := entryOf(t, path, account.AuthIndex); entry.Failures != 0 || entry.LastError != "" {
		t.Fatalf("entry=%+v", entry)
	}
}

// A PollError's fixed message reaches the entry and the history; an empty
// one is no message. No provider returns one today; the mechanism stays.
func TestPollErrorMessageIsRecorded(t *testing.T) {
	f := &creditFetcher{accounts: []Account{{Provider: "claude", AuthIndex: "one"}}, result: func(Account) (Observation, error) {
		return Observation{RequestSent: true, HTTPStatus: 403}, PollError{Message: "credential not permitted"}
	}}
	c, path := creditCache(t, f)
	step(t, c, creditStart)
	snapshot := load(t, path)
	entry := snapshot.Entries[client.Key("claude", "one")]
	if entry.LastError != "credential not permitted" || entry.Failures != 1 || len(snapshot.History) != 1 ||
		snapshot.History[0].Error != "credential not permitted" || snapshot.History[0].Outcome != "failed" {
		t.Fatalf("entry=%+v history=%+v", entry, snapshot.History)
	}
	f.result = func(Account) (Observation, error) { return Observation{RequestSent: true}, PollError{} }
	step(t, c, creditStart.Add(time.Hour))
	if entry := load(t, path).Entries[client.Key("claude", "one")]; entry.LastError != "quota fetch failed" {
		t.Fatalf("last_error=%q", entry.LastError)
	}
}
