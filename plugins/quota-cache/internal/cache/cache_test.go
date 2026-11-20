package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

type fakeFetcher struct {
	accounts []Account
	calls    int
	now      time.Time
	failure  error
	// details is returned with every observation, and known records the
	// account details each call was handed.
	details *client.AccountDetails
	known   []*client.AccountDetails
	// listErr is returned by List in place of the accounts.
	listErr error
	// failures overrides failure for the auth indexes it names, so one
	// credential can fail while its siblings answer.
	failures map[string]error
	// reports records every failed poll Step reported.
	reports []Failure
}

func (f *fakeFetcher) ReportFailure(_ context.Context, failure Failure) {
	f.reports = append(f.reports, failure)
}

func (f *fakeFetcher) List(context.Context) ([]Account, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.accounts, nil
}
func (f *fakeFetcher) Fetch(_ context.Context, a Account, known *client.AccountDetails) (Observation, error) {
	f.calls++
	f.known = append(f.known, known)
	failure := f.failure
	if err, ok := f.failures[a.AuthIndex]; ok {
		failure = err
	}
	return Observation{Percent: 95, ResetAt: f.now.Add(7 * 24 * time.Hour), ObservedAt: f.now, AccountDetails: f.details, RequestSent: true, HTTPStatus: 200}, failure
}
func fixture(t *testing.T) (*Cache, *fakeFetcher, Options) {
	t.Helper()
	opts := Options{Path: filepath.Join(t.TempDir(), "cache", "snapshot.json"), Interval: 15 * time.Minute, Spacing: 10 * time.Second}
	f := &fakeFetcher{accounts: []Account{{Provider: "claude", AuthIndex: "one"}}, now: time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)}
	c, err := Open(opts, f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c, f, opts
}
func TestConcurrentRequestsAndRepeatedConsumerReadsMakeOneProviderCall(t *testing.T) {
	c, f, opts := fixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Step(context.Background(), f.now); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for i := 0; i < 100; i++ {
		entry, err := client.ReadFresh(opts.Path, "claude", "one", f.now, 30*time.Minute)
		if err != nil || entry.Percent != 95 {
			t.Fatalf("entry=%+v err=%v", entry, err)
		}
	}
	if f.calls != 1 {
		t.Fatalf("provider requests=%d; want 1", f.calls)
	}
	if err := c.Step(context.Background(), f.now.Add(14*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 {
		t.Fatal("TTL was bypassed")
	}
}
func TestRateLimitPausesSiblingAccountsAndSurvivesRestart(t *testing.T) {
	c, f, opts := fixture(t)
	f.accounts = append(f.accounts, Account{Provider: "claude", AuthIndex: "two"}, Account{Provider: "codex", AuthIndex: "three"})
	f.failure = RateLimited{RetryAfter: f.now.Add(time.Hour)}
	if err := c.Step(context.Background(), f.now); err != nil {
		t.Fatal(err)
	}
	c.Close()
	f.failure = nil
	c2, err := Open(opts, f)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if err := c2.Step(context.Background(), f.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Load(opts.Path)
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != 2 {
		t.Fatalf("other provider should still refresh, calls=%d", f.calls)
	}
	if !snapshot.Entries[client.Key("claude", "two")].ObservedAt.IsZero() {
		t.Fatal("sibling bypassed provider cooldown")
	}
	if err := c2.Step(context.Background(), f.now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if f.calls != 2 {
		t.Fatal("restart or sibling bypassed Retry-After")
	}
}
func TestFailedRefreshRetainsButDoesNotServeOldObservationAsFresh(t *testing.T) {
	c, f, opts := fixture(t)
	if err := c.Step(context.Background(), f.now); err != nil {
		t.Fatal(err)
	}
	f.failure = errors.New("private upstream failure body")
	if err := c.Step(context.Background(), f.now.Add(15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Load(opts.Path)
	if err != nil {
		t.Fatal(err)
	}
	entry := snapshot.Entries[client.Key("claude", "one")]
	if entry.Percent != 95 || !entry.ObservedAt.Equal(f.now) || entry.LastError != "quota fetch failed" {
		t.Fatalf("%+v", entry)
	}
	if _, err := client.ReadFresh(opts.Path, "claude", "one", f.now.Add(15*time.Minute), time.Hour); err == nil {
		t.Fatal("failed refresh became fresh")
	}
}
func TestSingleWriterAndCorruptStateFailClosed(t *testing.T) {
	c, f, opts := fixture(t)
	if other, err := Open(opts, f); err == nil {
		other.Close()
		t.Fatal("second writer admitted")
	}
	c.Close()
	if err := os.WriteFile(opts.Path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if other, err := Open(opts, f); err == nil {
		other.Close()
		t.Fatal("corrupt persisted cooldown discarded")
	}
}
func TestRemovedAccountIsNotServed(t *testing.T) {
	c, f, opts := fixture(t)
	if err := c.Step(context.Background(), f.now); err != nil {
		t.Fatal(err)
	}
	f.accounts = nil
	if err := c.Step(context.Background(), f.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReadFresh(opts.Path, "claude", "one", f.now.Add(time.Minute), time.Hour); err == nil {
		t.Fatal("removed account retained")
	}
}

// A host that is still starting lists no credentials it can name. That is not
// every account being removed: the snapshot, its schedule and its file stay
// exactly as they were, consumers keep reading them, and the first complete
// roster carries on from where the cache left off.
func TestRosterNotReadyLeavesTheSnapshotUntouched(t *testing.T) {
	c, f, opts := fixture(t)
	f.accounts = append(f.accounts, Account{Provider: "codex", AuthIndex: "two"})
	if err := c.Step(context.Background(), f.now); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(opts.Path)
	if err != nil {
		t.Fatal(err)
	}
	written, err := client.Load(opts.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(written.Entries) != 2 || f.calls != 1 {
		t.Fatalf("setup: entries=%d calls=%d", len(written.Entries), f.calls)
	}

	// Past the request spacing, so only the roster stands between this Step
	// and a provider request. Wrapped, as a host error may be.
	at := f.now.Add(time.Minute)
	f.listErr = fmt.Errorf("host: %w", ErrRosterNotReady)
	if err := c.Step(context.Background(), at); !errors.Is(err, ErrRosterNotReady) {
		t.Fatalf("err=%v; want ErrRosterNotReady", err)
	}
	after, err := os.ReadFile(opts.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("a roster that was not ready rewrote the snapshot:\nbefore %s\nafter  %s", before, after)
	}
	if s, err := client.Load(opts.Path); err != nil || !s.WrittenAt.Equal(written.WrittenAt) {
		t.Fatalf("written_at moved: %v err=%v", s.WrittenAt, err)
	}
	if entry, err := client.ReadFresh(opts.Path, "claude", "one", at, 30*time.Minute); err != nil || entry.Percent != 95 {
		t.Fatalf("observation stopped being served: %+v err=%v", entry, err)
	}
	if a := c.Activity(); a.Error != ErrRosterNotReady.Error() || !a.Waiting || !a.LastScan.Equal(f.now) || a.Accounts != 2 {
		t.Fatalf("activity=%+v; want the wait reported and the last real scan kept", a)
	}
	if f.calls != 1 {
		t.Fatalf("provider requests=%d; want 1", f.calls)
	}

	// The real roster: the account still due is polled at once, since the
	// wait admitted no request and moved no schedule.
	f.listErr = nil
	if err := c.Step(context.Background(), at); err != nil {
		t.Fatal(err)
	}
	if f.calls != 2 {
		t.Fatalf("provider requests=%d; want 2", f.calls)
	}
	s, err := client.Load(opts.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{client.Key("claude", "one"), client.Key("codex", "two")} {
		if entry := s.Entries[key]; entry.ObservedAt.IsZero() || entry.LastError != "" {
			t.Fatalf("%s=%+v", key, entry)
		}
	}
	if a := c.Activity(); a.Error != "" || a.Waiting || !a.LastScan.Equal(at) || a.Accounts != 2 {
		t.Fatalf("activity=%+v", a)
	}
}

// A new install has no snapshot yet. Waiting for CPA's credentials still writes
// the empty one a first scan would, so a consumer that checks the path while
// CPA starts finds it, and writes it only once.
func TestRosterNotReadyStillWritesTheFirstSnapshot(t *testing.T) {
	c, f, opts := fixture(t)
	f.listErr = ErrRosterNotReady
	if _, err := os.Lstat(opts.Path); !os.IsNotExist(err) {
		t.Fatalf("setup: snapshot already exists: %v", err)
	}
	if err := c.Step(context.Background(), f.now); !errors.Is(err, ErrRosterNotReady) {
		t.Fatalf("err=%v; want ErrRosterNotReady", err)
	}
	written, err := client.Load(opts.Path)
	if err != nil {
		t.Fatal(err)
	}
	if written.Schema != 1 || len(written.Entries) != 0 || !written.WrittenAt.Equal(f.now) {
		t.Fatalf("snapshot=%+v; want an empty schema-1 snapshot", written)
	}
	if a := c.Activity(); !a.Waiting || !a.LastScan.IsZero() || a.Accounts != 0 {
		t.Fatalf("activity=%+v; want the wait reported and no scan", a)
	}
	if err := c.Step(context.Background(), f.now.Add(time.Minute)); !errors.Is(err, ErrRosterNotReady) {
		t.Fatalf("err=%v; want ErrRosterNotReady", err)
	}
	if s, err := client.Load(opts.Path); err != nil || !s.WrittenAt.Equal(f.now) {
		t.Fatalf("written_at=%v err=%v; want the first write only", s.WrittenAt, err)
	}
	if f.calls != 0 {
		t.Fatalf("provider requests=%d; want 0", f.calls)
	}
}

func TestPollingHistoryIsBoundedRedactedAndPreservedOnRestart(t *testing.T) {
	c, f, opts := fixture(t)
	for i := 0; i < 105; i++ {
		f.now = f.now.Add(opts.Interval)
		if err := c.Step(context.Background(), f.now); err != nil {
			t.Fatal(err)
		}
	}
	f.failure = errors.New("private upstream canary")
	if err := c.Step(context.Background(), f.now.Add(opts.Interval)); err != nil {
		t.Fatal(err)
	}
	c.Close()
	restarted, err := Open(opts, f)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	s, err := client.Load(opts.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.History) != 100 || s.Totals.Attempts != 106 || s.Totals.Requests != 106 || s.Totals.Successes != 105 || s.Totals.Failures != 1 {
		t.Fatalf("incorrect bounded history or totals: %d %+v", len(s.History), s.Totals)
	}
	last := s.History[len(s.History)-1]
	if last.Outcome != "failed" || last.Error != "quota fetch failed" || last.HTTPStatus != 200 {
		t.Fatalf("%+v", last)
	}
	if err := restarted.Step(context.Background(), f.now.Add(opts.Interval+time.Second)); err != nil {
		t.Fatal(err)
	}
	if f.calls != 106 {
		t.Fatal("history caused an extra provider request")
	}
}

type blockingFetcher struct{ entered, finish chan struct{} }

func (f *blockingFetcher) List(context.Context) ([]Account, error) {
	return []Account{{Provider: "claude", AuthIndex: "one"}}, nil
}
func (f *blockingFetcher) Fetch(context.Context, Account, *client.AccountDetails) (Observation, error) {
	close(f.entered)
	<-f.finish
	return Observation{}, nil
}

func TestStatusRemainsReadableDuringAProviderCall(t *testing.T) {
	f := &blockingFetcher{make(chan struct{}), make(chan struct{})}
	opts := Options{Path: filepath.Join(t.TempDir(), "cache", "snapshot.json"), Interval: time.Minute, Spacing: time.Second}
	c, err := Open(opts, f)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	done := make(chan error, 1)
	go func() { done <- c.Step(context.Background(), time.Now()) }()
	<-f.entered
	// Disk status and the activity mutex must not wait behind the HTTP callback.
	read := make(chan bool, 1)
	go func() {
		s, err := client.Load(opts.Path)
		a := c.Activity()
		read <- err == nil && a.Accounts == 1 && s.Entries["claude:one"].LastError == "refresh pending"
	}()
	select {
	case ok := <-read:
		if !ok {
			t.Error("pending poll not visible")
		}
	case <-time.After(time.Second):
		t.Error("status blocked on provider callback")
	}
	close(f.finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestScheduleChangesPreserveHistoryAndCooldowns(t *testing.T) {
	c, f, opts := fixture(t)
	start := f.now
	if err := c.Step(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	c.SetSchedule(5*time.Minute, time.Second)
	if err := c.Step(context.Background(), start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	s, err := client.Load(opts.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Entries[client.Key("claude", "one")].NextAttempt.Equal(start.Add(5*time.Minute)) || f.calls != 1 || len(s.History) != 1 {
		t.Fatal("shorter interval was not persisted without losing history or making an early request")
	}
	f.now = start.Add(5 * time.Minute)
	f.failure = RateLimited{RetryAfter: start.Add(time.Hour)}
	if err := c.Step(context.Background(), f.now); err != nil {
		t.Fatal(err)
	}
	c.SetSchedule(time.Minute, 20*time.Second)
	if err := c.Step(context.Background(), start.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	s, err = client.Load(opts.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.ProviderCooldown["claude"].Equal(start.Add(time.Hour)) || f.calls != 2 || len(s.History) != 2 || s.Totals.RateLimits != 1 {
		t.Fatal("schedule edit lost history or shortened rate limit cooldown")
	}
	c.Close()
	opts.Interval, opts.Spacing = time.Minute, 20*time.Second
	restarted, err := Open(opts, f)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.Step(context.Background(), start.Add(7*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if f.calls != 2 {
		t.Fatal("restart bypassed cooldown after schedule edit")
	}
}

func TestIncreasingScheduleDefersAdmission(t *testing.T) {
	c, f, opts := fixture(t)
	f.accounts = append(f.accounts, Account{Provider: "codex", AuthIndex: "two"})
	if err := c.Step(context.Background(), f.now); err != nil {
		t.Fatal(err)
	}
	c.SetSchedule(30*time.Minute, time.Minute)
	if err := c.Step(context.Background(), f.now.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	s, err := client.Load(opts.Path)
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 || !s.NextRequest.Equal(f.now.Add(time.Minute)) || !s.Entries[client.Key("claude", "one")].NextAttempt.Equal(f.now.Add(30*time.Minute)) {
		t.Fatal("longer interval or spacing did not defer admission")
	}
	if err := c.Step(context.Background(), f.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if f.calls != 2 {
		t.Fatal("new spacing did not admit queued account")
	}
}

func TestExtendedSnapshotPersistsRetainsOnFailureAndReplacesOnSuccess(t *testing.T) {
	c, f, opts := fixture(t)
	value := float64(20)
	// Exercise actual snapshot persistence and admission with extended data.
	detailed := &detailsFetcher{fakeFetcher: f, quota: &client.Quota{Schema: 1, ObservedAt: f.now, Windows: map[string]client.Window{"five_hour": {UsedPercent: &value}}}}
	c.fetcher = detailed
	if err := c.Step(context.Background(), f.now); err != nil {
		t.Fatal(err)
	}
	c.Close()
	restarted, err := Open(opts, detailed)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if q, err := client.ReadQuota(opts.Path, "claude", "one", f.now, time.Hour); err != nil || len(q.Windows) != 1 {
		t.Fatalf("restart lost extended data: %v", err)
	}
	f.failure = RateLimited{RetryAfter: f.now.Add(time.Hour)}
	if err := restarted.Step(context.Background(), f.now.Add(opts.Interval)); err != nil {
		t.Fatal(err)
	}
	s, err := client.Load(opts.Path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Entries["claude:one"].Quota == nil {
		t.Fatal("failure discarded last known extended values")
	}
	if _, err := client.ReadQuota(opts.Path, "claude", "one", f.now.Add(opts.Interval), time.Hour); err == nil {
		t.Fatal("failed refresh treated as usable")
	}
	f.failure = nil
	f.now = f.now.Add(2 * time.Hour)
	detailed.quota = &client.Quota{Schema: 1, ObservedAt: f.now}
	if err := restarted.Step(context.Background(), f.now); err != nil {
		t.Fatal(err)
	}
	q, err := client.ReadQuota(opts.Path, "claude", "one", f.now, time.Hour)
	if err != nil || len(q.Windows) != 0 {
		t.Fatal("successful response retained fields omitted by provider")
	}
}

type detailsFetcher struct {
	*fakeFetcher
	quota *client.Quota
}

func (f *detailsFetcher) Fetch(ctx context.Context, a Account, known *client.AccountDetails) (Observation, error) {
	o, err := f.fakeFetcher.Fetch(ctx, a, known)
	o.Quota = f.quota
	return o, err
}

type windowFetcher struct {
	accounts []Account
	now      time.Time
	windows  []client.EntryWindow
}

func (f *windowFetcher) List(context.Context) ([]Account, error) { return f.accounts, nil }
func (f *windowFetcher) Fetch(context.Context, Account, *client.AccountDetails) (Observation, error) {
	return Observation{
		Percent: 76, ResetAt: f.now.Add(62 * time.Hour), ObservedAt: f.now,
		Windows: f.windows, Plan: "Max", TierName: "max_20x",
		RequestSent: true, HTTPStatus: 200,
	}, nil
}

// The dashboard reads the file, not the writer's memory. Windows and plan have
// to survive the atomic-rename commit and come back in the same order, since an
// unstable order would rewrite the file on every poll.
func TestCanonicalWindowsAndPlanSurviveTheWriteAndReload(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	f := &windowFetcher{accounts: []Account{{Provider: "claude", AuthIndex: "one"}}, now: now, windows: []client.EntryWindow{
		{Key: client.WindowSession, Title: "Session", UsedPercent: 31, ResetAt: now.Add(75 * time.Minute), ObservedAt: now},
		{Key: client.WindowWeekly, Title: "Weekly", UsedPercent: 76, ResetAt: now.Add(62 * time.Hour), ObservedAt: now},
		{Key: client.WindowWeeklyFable, UsedPercent: 100, ResetAt: now.Add(61 * time.Hour), ObservedAt: now},
	}}
	opts := Options{Path: filepath.Join(t.TempDir(), "cache", "snapshot.json"), Interval: 15 * time.Minute, Spacing: 10 * time.Second}
	c, err := Open(opts, f)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Step(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Load(opts.Path)
	if err != nil {
		t.Fatal(err)
	}
	entry := snapshot.Entries[client.Key("claude", "one")]
	if len(entry.Windows) != 3 {
		t.Fatalf("windows=%+v", entry.Windows)
	}
	for i, want := range []string{client.WindowSession, client.WindowWeekly, client.WindowWeeklyFable} {
		if entry.Windows[i].Key != want {
			t.Fatalf("window %d=%q want %q", i, entry.Windows[i].Key, want)
		}
		if !entry.Windows[i].ObservedAt.Equal(now) {
			t.Fatalf("window %d lost its observation time", i)
		}
	}
	if entry.Plan != "Max" || entry.TierName != "max_20x" {
		t.Fatalf("identity=%q/%q", entry.Plan, entry.TierName)
	}
	// The compatibility surface is unchanged, so existing consumers still read
	// the regular weekly window exactly as before.
	fresh, err := client.ReadFresh(opts.Path, "claude", "one", now, 30*time.Minute)
	if err != nil || fresh.Percent != 76 {
		t.Fatalf("fresh=%+v err=%v", fresh, err)
	}
}

// Account details are read on a slower schedule than the poll, so the cache
// carries them: through a poll that did not read them, through a failed poll,
// and through a restart, after which the fetcher is handed the persisted copy
// and can tell that nothing is due yet.
func TestAccountDetailsCarryForwardAndReachTheFetcherAfterRestart(t *testing.T) {
	c, f, opts := fixture(t)
	start := f.now
	// The subscription start rides along with the plan: both are read from
	// one profile, and both have to survive the file and the restart.
	started := time.Date(2025, time.January, 31, 9, 15, 0, 0, time.UTC)
	read := &client.AccountDetails{CheckedAt: start, Plan: "team", SubscriptionStartedAt: &started}
	f.details = read
	if err := c.Step(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	entry := func() client.Entry {
		t.Helper()
		s, err := client.Load(opts.Path)
		if err != nil {
			t.Fatal(err)
		}
		return s.Entries[client.Key("claude", "one")]
	}
	if got := entry().AccountDetails; got == nil || got.Plan != "team" || !got.CheckedAt.Equal(start) || f.known[0] != nil {
		t.Fatalf("details=%+v handed=%+v", got, f.known)
	}

	// A successful poll that did not read them leaves them as they were.
	f.details = nil
	if err := c.Step(context.Background(), start.Add(opts.Interval)); err != nil {
		t.Fatal(err)
	}
	if got := entry().AccountDetails; got == nil || got.Plan != "team" {
		t.Fatalf("a poll without a read erased the details: %+v", got)
	}
	if f.known[1] == nil || f.known[1].Plan != "team" {
		t.Fatalf("the fetcher was not handed what the snapshot holds: %+v", f.known[1])
	}

	// A failed poll keeps them too, like every other last-known value.
	f.failure = errors.New("synthetic failure")
	if err := c.Step(context.Background(), start.Add(2*opts.Interval)); err != nil {
		t.Fatal(err)
	}
	if got := entry().AccountDetails; got == nil || got.Plan != "team" || got.SubscriptionStartedAt == nil || !got.SubscriptionStartedAt.Equal(started) {
		t.Fatalf("a failed poll erased the details: %+v", got)
	}

	c.Close()
	f.failure = nil
	restarted, err := Open(opts, f)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	// The failure backed the credential off one interval further.
	if err := restarted.Step(context.Background(), start.Add(4*opts.Interval)); err != nil {
		t.Fatal(err)
	}
	if len(f.known) != 4 {
		t.Fatalf("calls=%d", len(f.known))
	}
	if handed := f.known[3]; handed == nil || handed.Plan != "team" || !handed.CheckedAt.Equal(start) ||
		handed.SubscriptionStartedAt == nil || !handed.SubscriptionStartedAt.Equal(started) {
		t.Fatalf("after a restart the fetcher was handed %+v; it would ask every credential again", handed)
	}

	// A fresh read replaces them.
	f.details = &client.AccountDetails{CheckedAt: start.Add(5 * opts.Interval), Plan: "enterprise"}
	if err := restarted.Step(context.Background(), start.Add(5*opts.Interval)); err != nil {
		t.Fatal(err)
	}
	if got := entry(); got.AccountDetails == nil || got.AccountDetails.Plan != "enterprise" {
		t.Fatalf("a fresh read did not replace the details: %+v", got.AccountDetails)
	}
}
