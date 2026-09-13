package cache

import (
	"context"
	"errors"
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
}

func (f *fakeFetcher) List(context.Context) ([]Account, error) { return f.accounts, nil }
func (f *fakeFetcher) Fetch(context.Context, Account) (Observation, error) {
	f.calls++
	return Observation{Percent: 95, ResetAt: f.now.Add(7 * 24 * time.Hour), ObservedAt: f.now, RequestSent: true, HTTPStatus: 200}, f.failure
}
func fixture(t *testing.T) (*Cache, *fakeFetcher, Options) {
	t.Helper()
	opts := Options{Path: filepath.Join(t.TempDir(), "cache", "snapshot.json"), Interval: 15 * time.Minute, Spacing: 10 * time.Second}
	f := &fakeFetcher{accounts: []Account{{"claude", "one"}}, now: time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)}
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
	f.accounts = append(f.accounts, Account{"claude", "two"}, Account{"codex", "three"})
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
	return []Account{{"claude", "one"}}, nil
}
func (f *blockingFetcher) Fetch(context.Context, Account) (Observation, error) {
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
	f.accounts = append(f.accounts, Account{"codex", "two"})
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

func (f *detailsFetcher) Fetch(ctx context.Context, a Account) (Observation, error) {
	o, err := f.fakeFetcher.Fetch(ctx, a)
	o.Quota = f.quota
	return o, err
}
