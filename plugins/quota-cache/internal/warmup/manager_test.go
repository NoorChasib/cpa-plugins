package warmup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

const accountA = "0000000000000001"
const accountB = "0000000000000002"

func fixture(t *testing.T, runner Runner) (*Manager, time.Time, string) {
	t.Helper()
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "snapshot.json")
	options := Defaults()
	options.Enabled = true
	options, err := options.Validate()
	if err != nil {
		t.Fatal(err)
	}
	roster := func(context.Context) ([]Identity, error) {
		return []Identity{{AuthIndex: accountA, Email: "one@example.com"}}, nil
	}
	manager := New(path, options, runner, roster, nil)
	writeSnapshot(t, path, now, idle(now))
	return manager, now, path
}

func idle(now time.Time) client.Entry {
	return client.Entry{Provider: "claude", AuthIndex: accountA, ObservedAt: now, Windows: []client.EntryWindow{
		{Key: client.WindowSession, ObservedAt: now},
		{Key: client.WindowWeekly, UsedPercent: 10, ObservedAt: now, ResetAt: now.Add(24 * time.Hour)},
	}}
}

func writeSnapshot(t *testing.T, path string, now time.Time, entries ...client.Entry) {
	t.Helper()
	snapshot := client.Snapshot{Schema: 1, WrittenAt: now, Entries: map[string]client.Entry{}, ProviderCooldown: map[string]time.Time{}}
	for _, entry := range entries {
		snapshot.Entries[client.Key("claude", entry.AuthIndex)] = entry
	}
	raw, _ := json.Marshal(snapshot)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestEligibilityDoesNotSpendAgainstUnknownOrExhaustedQuota(t *testing.T) {
	for _, kind := range []string{"active", "weekly_spent", "stale", "error", "missing_session", "turning_over", "ambiguous"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			m, now, path := fixture(t, RunnerFunc(func(context.Context, Options, Identity) error { calls++; return nil }))
			entry := idle(now)
			switch kind {
			case "active":
				entry.Windows[0].ResetAt = now.Add(time.Hour)
			case "weekly_spent":
				entry.Windows[1].UsedPercent = 100
			case "stale":
				entry.Windows[0].ObservedAt = now.Add(-time.Hour)
			case "error":
				entry.LastError = "HTTP 429"
			case "missing_session":
				entry.Windows = entry.Windows[1:]
			case "turning_over":
				entry.Windows[0].ResetAt = now.Add(-time.Minute)
				entry.Windows[0].ObservedAt = now.Add(-2 * time.Minute)
			case "ambiguous":
				entry.Windows[0].UsedPercent = 20
			}
			writeSnapshot(t, path, now, entry)
			m.WarmAll()
			m.Step(context.Background(), now)
			if calls != 0 {
				t.Fatal("warm-up ignored quota guard")
			}
		})
	}
}

func TestAttemptIsDurableBeforeRunnerAndSurvivesRestart(t *testing.T) {
	calls := 0
	var path string
	runner := RunnerFunc(func(_ context.Context, options Options, identity Identity) error {
		calls++
		state, err := client.LoadWarmup(path)
		if err != nil || state.Accounts[0].State != "warming" || state.Accounts[0].NextAttempt.IsZero() {
			t.Fatal("attempt was not durable before inference")
		}
		if options.Model != DefaultModel || options.Effort != "low" || identity.AuthIndex != accountA {
			t.Fatal("wrong configured model or account")
		}
		return errors.New("outcome_unknown")
	})
	m, now, snapshotPath := fixture(t, runner)
	path = snapshotPath
	m.Step(context.Background(), now)
	m = New(path, m.opts, runner, m.roster, nil)
	for range 3 {
		m.WarmAll()
		m.Step(context.Background(), now.Add(time.Minute))
	}
	if calls != 1 {
		t.Fatalf("runner called %d times after an ambiguous result", calls)
	}
	if state := m.Status(now); state.Accounts[0].Error != "outcome_unknown" {
		t.Fatalf("state: %+v", state)
	}
}

func TestFreshConfirmationUsesProviderResetAndRewarmsAfterIt(t *testing.T) {
	calls := 0
	m, now, path := fixture(t, RunnerFunc(func(context.Context, Options, Identity) error { calls++; return nil }))
	m.Step(context.Background(), now)
	if m.Status(now).Accounts[0].State != "awaiting_confirmation" {
		t.Fatal("claimed a warm window before observing one")
	}
	confirmed := idle(now.Add(time.Minute))
	confirmed.Windows[0].ResetAt = now.Add(2 * time.Hour)
	writeSnapshot(t, path, now.Add(time.Minute), confirmed)
	m.Step(context.Background(), now.Add(time.Minute))
	account := m.Status(now).Accounts[0]
	if account.State != "active" || !account.NextAttempt.Equal(confirmed.Windows[0].ResetAt) || account.LastSuccess.IsZero() {
		t.Fatalf("confirmation = %+v", account)
	}
	later := now.Add(2*time.Hour + time.Minute)
	writeSnapshot(t, path, later, idle(later))
	m.Step(context.Background(), later)
	if calls != 2 {
		t.Fatalf("did not rewarm after the provider reset: %d", calls)
	}
}

func TestWarmAllQueuesEveryAccountOutsideHoursWithSpacing(t *testing.T) {
	calls := 0
	m, now, path := fixture(t, RunnerFunc(func(context.Context, Options, Identity) error { calls++; return nil }))
	o := m.opts
	o.Hours = "18:00-20:00"
	o, _ = o.Validate()
	m.Configure(o)
	m.roster = func(context.Context) ([]Identity, error) {
		return []Identity{{AuthIndex: accountA, Email: "one@example.com"}, {AuthIndex: accountB, Email: "two@example.com"}}, nil
	}
	b := idle(now)
	b.AuthIndex = accountB
	writeSnapshot(t, path, now, idle(now), b)
	m.Step(context.Background(), now)
	if calls != 0 {
		t.Fatal("ignored automatic working hours")
	}
	m.WarmAll()
	m.Step(context.Background(), now)
	m.Step(context.Background(), now.Add(2*time.Second))
	if calls != 1 {
		t.Fatal("ignored request spacing")
	}
	m.Step(context.Background(), now.Add(o.Spacing))
	if calls != 2 {
		t.Fatal("manual queue lost its second account outside working hours")
	}
}

func TestDisabledAndCorruptJournalNeverInvokeRunner(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		calls := 0
		m, now, path := fixture(t, RunnerFunc(func(context.Context, Options, Identity) error { calls++; return nil }))
		if corrupt {
			if err := os.WriteFile(client.WarmupPath(path), []byte("bad journal"), 0600); err != nil {
				t.Fatal(err)
			}
			m = New(path, m.opts, m.runner, m.roster, nil)
		} else {
			o := m.opts
			o.Enabled = false
			m.Configure(o)
		}
		if m.WarmAll() {
			t.Fatal("queued disabled or unsafe warm-up")
		}
		m.Step(context.Background(), now)
		if calls != 0 {
			t.Fatal("invoked runner")
		}
	}
}

func TestConfigureAppliesTheNewModelToTheNextRequest(t *testing.T) {
	var got Options
	m, now, _ := fixture(t, RunnerFunc(func(_ context.Context, o Options, _ Identity) error { got = o; return nil }))
	o := m.opts
	o.Model, o.Effort = "claude-next-model", "medium"
	o, _ = o.Validate()
	m.Configure(o)
	m.Step(context.Background(), now)
	if got.Model != o.Model || got.Effort != "medium" {
		t.Fatalf("stale runner config: %+v", got)
	}
}

func TestDuplicateCredentialsShareTheAccountAttemptGuard(t *testing.T) {
	calls := 0
	m, now, path := fixture(t, RunnerFunc(func(context.Context, Options, Identity) error { calls++; return nil }))
	m.roster = func(context.Context) ([]Identity, error) {
		return []Identity{{AuthIndex: accountA, Email: "one@example.com"}, {AuthIndex: accountB, Email: "ONE@example.com"}}, nil
	}
	b := idle(now)
	b.AuthIndex = accountB
	writeSnapshot(t, path, now, idle(now), b)
	m.Step(context.Background(), now)
	m.Step(context.Background(), now.Add(m.opts.Spacing))
	if calls != 1 {
		t.Fatalf("duplicate account was warmed %d times", calls)
	}
}

func TestProviderCooldownAndDisabledCredentialSkipWarmup(t *testing.T) {
	calls := 0
	m, now, path := fixture(t, RunnerFunc(func(context.Context, Options, Identity) error { calls++; return nil }))
	snapshot, err := client.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.ProviderCooldown["claude"] = now.Add(time.Hour)
	raw, _ := json.Marshal(snapshot)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	m.WarmAll()
	m.Step(context.Background(), now)
	if calls != 0 || m.Status(now).Accounts[0].State != "provider_cooldown" {
		t.Fatal("provider cooldown ignored")
	}
	writeSnapshot(t, path, now, idle(now))
	m.roster = func(context.Context) ([]Identity, error) {
		return []Identity{{AuthIndex: accountA, Email: "one@example.com", Disabled: true}}, nil
	}
	m.WarmAll()
	m.Step(context.Background(), now)
	if calls != 0 || m.Status(now).Accounts[0].State != "disabled" {
		t.Fatal("disabled credential warmed")
	}
}

func TestTurnoverBringsForwardOnlyTheNeededQuotaPoll(t *testing.T) {
	calls, refreshes := 0, 0
	m, now, path := fixture(t, RunnerFunc(func(context.Context, Options, Identity) error { calls++; return nil }))
	o := m.opts
	o.Hours = "18:00-20:00"
	o, _ = o.Validate()
	m.Configure(o)
	m.WarmAll()
	entry := idle(now.Add(-time.Minute))
	entry.Windows[0].ResetAt = now.Add(-time.Second)
	writeSnapshot(t, path, now, entry)
	m.refresh = func(id string, at time.Time) {
		if id != accountA || !at.Equal(now) {
			t.Error("wrong turnover refresh")
		}
		refreshes++
	}
	m.Step(context.Background(), now)
	if calls != 0 || refreshes != 1 {
		t.Fatal("turnover did not wait for a targeted fresh observation")
	}
	writeSnapshot(t, path, now, idle(now))
	m.refresh = nil
	m.Step(context.Background(), now)
	if calls != 1 {
		t.Fatal("fresh idle window did not warm")
	}
}

func TestDisableCancelsTheInFlightRunner(t *testing.T) {
	started, done := make(chan struct{}), make(chan struct{})
	m, now, _ := fixture(t, RunnerFunc(func(ctx context.Context, _ Options, _ Identity) error {
		close(started)
		<-ctx.Done()
		return errors.New("outcome_unknown")
	}))
	go func() { defer close(done); m.Step(context.Background(), now) }()
	<-started
	o := m.opts
	o.Enabled = false
	m.Configure(o)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disable did not cancel runner")
	}
}

// CPA cannot stop a model request once it is sent. Disabling warm-up still
// returns at once and keeps the five-hour guard; the plan the cache recorded
// is what lets the request go out at all.
func TestDisableDoesNotWaitForAModelRequestCPACannotStop(t *testing.T) {
	host, entered, release := newFakeHost(), make(chan struct{}), make(chan struct{})
	host.execute = func() (protocol.HostModelExecutionResponse, error) {
		close(entered)
		<-release
		return protocol.HostModelExecutionResponse{StatusCode: 200, Body: []byte(`{"type":"message"}`)}, nil
	}
	defer close(release)
	m, now, path := fixture(t, NewCPA(host))
	entry := idle(now)
	entry.Plan = "max"
	writeSnapshot(t, path, now, entry)
	done := make(chan struct{})
	go func() { defer close(done); m.Step(context.Background(), now) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatalf("no model request was sent: %+v", m.Status(now).Accounts)
	}
	m.Disable()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disable waited for the model request")
	}
	account := m.Status(now).Accounts[0]
	if account.Error != "outcome_unknown" || !account.NextAttempt.Equal(now.Add(5*time.Hour)) {
		t.Fatalf("an uncertain request lost its guard: %+v", account)
	}
}

// Only a request that may have been sent keeps the five-hour guard. A code
// that proves nothing was sent retries after 15 minutes; anything unknown is
// treated as possibly sent.
func TestEveryRunnerCodeKeepsTheRightGuard(t *testing.T) {
	for code, wait := range map[string]time.Duration{
		"identity_unknown": 15 * time.Minute, "subscription_login_required": 15 * time.Minute,
		"account_unavailable": 15 * time.Minute, "host_unsupported": 15 * time.Minute,
		"runner_unavailable": 15 * time.Minute, "runner_busy": 15 * time.Minute,
		"outcome_unknown": 5 * time.Hour, "request_failed": 5 * time.Hour,
		"context deadline exceeded": 5 * time.Hour, "provider said no": 5 * time.Hour,
	} {
		t.Run(code, func(t *testing.T) {
			m, now, _ := fixture(t, RunnerFunc(func(context.Context, Options, Identity) error { return errors.New(code) }))
			m.Step(context.Background(), now)
			account := m.Status(now).Accounts[0]
			stored := code
			if wait == 5*time.Hour && code != "outcome_unknown" {
				stored = "request_failed"
			}
			if account.State != "failed" || account.Error != stored || !account.NextAttempt.Equal(now.Add(wait)) {
				t.Fatalf("%s: %+v", code, account)
			}
		})
	}
}

// A warm-up CPA answered that never showed up as an active session is not
// repeated automatically; only Warm all now tries that account again.
func TestAnsweredWarmupWithoutASessionPausesAutomaticRetries(t *testing.T) {
	calls := 0
	m, now, path := fixture(t, RunnerFunc(func(context.Context, Options, Identity) error { calls++; return nil }))
	m.Step(context.Background(), now)
	later := now.Add(5*time.Hour + time.Minute)
	writeSnapshot(t, path, later, idle(later))
	for range 2 {
		m.Step(context.Background(), later)
	}
	if account := m.Status(later).Accounts[0]; calls != 1 || account.Error != "unconfirmed" || account.State != "failed" {
		t.Fatalf("calls = %d, account = %+v", calls, account)
	}
	m.WarmAll()
	m.Step(context.Background(), later)
	if account := m.Status(later).Accounts[0]; calls != 2 || account.Error != "" || account.State != "awaiting_confirmation" {
		t.Fatalf("Warm all now did not retry: calls = %d, account = %+v", calls, account)
	}
}

func TestSetupFailuresBeforeTheRequestRetrySooner(t *testing.T) {
	host := newFakeHost()
	m, now, _ := fixture(t, NewCPA(host))
	m.Step(context.Background(), now)
	account := m.Status(now).Accounts[0]
	if host.sent() != 0 || account.Error != "subscription_login_required" || !account.NextAttempt.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("an account with no recorded plan: %+v", account)
	}
}

func TestHoursTimezoneAndOvernightRanges(t *testing.T) {
	o := Defaults()
	o.Hours, o.Timezone = "22:00-06:00", "Australia/Sydney"
	o, err := o.Validate()
	if err != nil {
		t.Fatal(err)
	}
	for hour, want := range map[int]bool{2: true, 5: true, 12: false, 22: true} {
		now := time.Date(2026, 10, 14, hour, 0, 0, 0, o.location)
		if o.withinHours(now) != want {
			t.Fatalf("hour %d", hour)
		}
	}
	for _, hours := range []string{"25:00-26:00", "9:00-17:00", "12:00-12:00", "oops"} {
		o.Hours = hours
		if _, err := o.Validate(); err == nil {
			t.Fatalf("accepted %q", hours)
		}
	}
}
