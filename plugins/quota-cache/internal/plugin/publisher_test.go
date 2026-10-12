package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/settings"
)

// publishAt is the publisher tests' clock: alpha's cycle runs from Sep 10 to
// Oct 10, so the state holds until then.
var (
	publishAt    = time.Date(2026, time.October, 9, 12, 0, 0, 0, time.UTC)
	publishUntil = time.Date(2026, time.October, 10, 0, 0, 0, 0, time.UTC)
)

// testPublisher is a publisher on its own snapshot, with no loop: a test ticks
// it with its own clock. It counts its writes and keeps its log.
type testPublisher struct {
	*publisher
	t      *testing.T
	mu     sync.Mutex
	writes int
	lines  []logLine
}

func newTestPublisher(t *testing.T) *testPublisher {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cache", "snapshot.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	tp := &testPublisher{t: t}
	raw, _ := json.Marshal(client.Snapshot{Schema: 1, WrittenAt: publishAt.Add(-time.Minute), Entries: map[string]client.Entry{}, ProviderCooldown: map[string]time.Time{}})
	writeFile(t, path, raw)
	// Alpha's credit and refill date, as entered on the page.
	raw, _ = json.Marshal(map[string]any{"schema": 1, "revision": 1, "apiCredits": map[string]any{
		alphaAccount: map[string]any{"monthlyUsd": "200", "renews": "2026-10-10", "rev": 1, "updatedAt": "2026-10-01T00:00:00Z"},
	}})
	writeFile(t, settings.FilePath(path), raw)
	store := settings.Open(path)
	items := []creditItem{{id: alphaAccount, credit: client.APICredit{Label: "alpha", OrganizationID: fakeOrgA}}}
	source := client.CreditOrganizations{State: client.OrganizationsStateOK, Revision: "1"}
	tp.publisher = newPublisher(path, "test", store, func() ([]creditItem, client.CreditOrganizations) { return items, source }, func(level, message string, fields map[string]any) {
		tp.mu.Lock()
		defer tp.mu.Unlock()
		tp.lines = append(tp.lines, logLine{level, message, fields})
	})
	tp.prepare = func(time.Time) bool { return store.Sync() }
	tp.write = func(path string, raw []byte) error {
		tp.mu.Lock()
		tp.writes++
		tp.mu.Unlock()
		return writeCreditsFile(path, raw)
	}
	return tp
}

func (tp *testPublisher) writeSnapshot(writtenAt time.Time) {
	tp.t.Helper()
	raw, _ := json.Marshal(client.Snapshot{Schema: 1, WrittenAt: writtenAt, Entries: map[string]client.Entry{}, ProviderCooldown: map[string]time.Time{}})
	writeFile(tp.t, tp.path, raw)
}

// writeUsedMeter writes a meter in which alpha used $4 of its credit on Oct 9:
// a million input tokens of a model at $4 a million. Only a counted account
// with credit used gives the pool its countdowns, the next refill's and the
// full refill's.
func (tp *testPublisher) writeUsedMeter(flushedAt time.Time) {
	tp.t.Helper()
	day := []client.MeterBucket{{Start: publishUntil.Add(-24 * time.Hour), Usage: []client.MeterUsage{{Model: "claude-opus-5-5", Requests: 1, Input: 1_000_000}}}}
	meter := client.APIMeter{Schema: client.MeterSchema, Since: publishAt.Add(-time.Hour), StartedAt: publishAt.Add(-time.Hour), FlushedAt: flushedAt,
		Gaps: []client.MeterGap{}, Organizations: map[string]client.MeterOrganization{fakeOrgA: {Since: publishAt.Add(-time.Hour), Days: day, Hours: []client.MeterBucket{}}},
		Unlinked: []client.MeterUnlinked{}, Auths: map[string]client.MeterAuth{}, AuthIDs: map[string]client.MeterAuthID{}}
	raw, _ := json.Marshal(meter)
	writeFile(tp.t, client.MeterPath(tp.path), raw)
}

// countdowns are the state's countdowns: alpha's renewal, the pool's next
// refill and its full refill. A countdown the state does not carry is -1.
func countdowns(state *client.CreditState) [3]int64 {
	out := [3]int64{-1, -1, -1}
	d := state.APICredits
	if len(d.Accounts) > 0 && d.Accounts[0].RenewsInSeconds != nil {
		out[0] = *d.Accounts[0].RenewsInSeconds
	}
	if d.Pool.NextRefill != nil {
		out[1] = d.Pool.NextRefill.RefillInSeconds
	}
	if d.Pool.FullInSeconds != nil {
		out[2] = *d.Pool.FullInSeconds
	}
	return out
}

func writeFile(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// wrote ticks at now and reports how many writes the tick made.
func (tp *testPublisher) wrote(now time.Time) int {
	tp.t.Helper()
	before := tp.count()
	tp.tick(now)
	return tp.count() - before
}

func (tp *testPublisher) count() int {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return tp.writes
}

func (tp *testPublisher) logged() []logLine {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return append([]logLine(nil), tp.lines...)
}

// It publishes at the first tick; then only when the snapshot, the meter or
// the settings change, when the state reaches its valid_until, or when the
// heartbeat is due; never because computed_at or a countdown moved.
func TestThePublisherWritesWhenTheStateChanges(t *testing.T) {
	tp := newTestPublisher(t)
	path := tp.path
	// Without a snapshot there is nothing to publish beside.
	if err := os.Rename(path, path+".aside"); err != nil {
		t.Fatal(err)
	}
	if n := tp.wrote(publishAt); n != 0 || tp.LastState() != nil {
		t.Fatalf("published with no snapshot: %d", n)
	}
	if err := os.Rename(path+".aside", path); err != nil {
		t.Fatal(err)
	}
	if n := tp.wrote(publishAt); n != 1 {
		t.Fatalf("first tick wrote %d", n)
	}
	state, err := client.LoadCredits(client.CreditsPath(path))
	if err != nil || !state.ComputedAt.Equal(publishAt) || state.ValidUntil == nil || !state.ValidUntil.Equal(publishUntil) || state.Inputs.MeterError != client.CreditMeterErrorMissing {
		t.Fatalf("state %+v %v", state, err)
	}
	if info, _ := os.Stat(client.CreditsPath(path)); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	// Nothing changed and nothing is due: not even a compute.
	if n := tp.wrote(publishAt.Add(10 * time.Second)); n != 0 || !tp.LastState().ComputedAt.Equal(publishAt) {
		t.Fatalf("wrote %d, computed at %v, with nothing changed", n, tp.LastState().ComputedAt)
	}
	// A forced compute moves computed_at and alpha's renewal countdown, and
	// the state in memory with them, but writes nothing.
	tp.publishMu.Lock()
	tp.publishLocked(publishAt.Add(20*time.Second), publishForced)
	tp.publishMu.Unlock()
	if tp.count() != 1 || !tp.LastState().ComputedAt.Equal(publishAt.Add(20*time.Second)) {
		t.Fatalf("writes %d, computed at %v", tp.count(), tp.LastState().ComputedAt)
	}

	// The meter, in which alpha has used some of its credit, so the pool
	// counts down to its refills too.
	tp.writeUsedMeter(publishAt.Add(25 * time.Second))
	if n := tp.wrote(publishAt.Add(30 * time.Second)); n != 1 || tp.LastMeter() == nil || tp.LastState().Inputs.MeterError != "" {
		t.Fatalf("a meter save: %d writes", n)
	}
	// Every countdown moves with a forced compute, and nothing is written.
	written := countdowns(tp.LastState())
	if written[0] < 0 || written[1] < 0 || written[2] < 0 {
		t.Fatalf("countdowns %v: alpha is not counted with credit used", written)
	}
	tp.publishMu.Lock()
	tp.publishLocked(publishAt.Add(32*time.Second), publishForced)
	tp.publishMu.Unlock()
	if moved := countdowns(tp.LastState()); tp.count() != 2 || moved != [3]int64{written[0] - 2, written[1] - 2, written[2] - 2} {
		t.Fatalf("writes %d, countdowns %v then %v", tp.count(), written, moved)
	}
	// The snapshot.
	tp.writeSnapshot(publishAt.Add(35 * time.Second))
	if n := tp.wrote(publishAt.Add(40 * time.Second)); n != 1 || !tp.LastState().Inputs.SnapshotWrittenAt.Equal(publishAt.Add(35*time.Second)) {
		t.Fatalf("a snapshot save: %d writes", n)
	}
	// The settings file, which the tick's Sync reads again.
	settingsRaw, _ := json.Marshal(map[string]any{"schema": 1, "revision": 2, "apiCredits": map[string]any{
		alphaAccount: map[string]any{"monthlyUsd": "300", "renews": "2026-10-10", "rev": 2, "updatedAt": "2026-10-09T12:00:00Z"},
	}})
	writeFile(t, settings.FilePath(path), settingsRaw)
	if n := tp.wrote(publishAt.Add(45 * time.Second)); n != 1 || accountIn(t, tp.LastState(), alphaAccount).MonthlyCreditText != "$300.00" {
		t.Fatalf("a settings save: %d writes", n)
	}
	// The heartbeat: 60 seconds after the last write, with nothing changed.
	if n := tp.wrote(publishAt.Add(104 * time.Second)); n != 0 {
		t.Fatal("the heartbeat came early")
	}
	if n := tp.wrote(publishAt.Add(105 * time.Second)); n != 1 {
		t.Fatal("no heartbeat")
	}
	// valid_until: the next cycle begins, with no other change and no
	// heartbeat due.
	if n := tp.wrote(publishUntil.Add(-30 * time.Second)); n != 1 {
		t.Fatal("no heartbeat before the boundary")
	}
	if n := tp.wrote(publishUntil); n != 1 || !tp.LastState().ValidUntil.Equal(time.Date(2026, time.November, 10, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("at valid_until: %d writes, valid until %v", n, tp.LastState().ValidUntil)
	}
	if lines := tp.logged(); len(lines) != 0 {
		t.Fatalf("logged %+v", lines)
	}
}

// A tick whose prepare reports that the settings or organizations may have
// changed computes whatever the stamps say, since fixing the owner of a file
// that could not be opened changes none of them, and writes only when the
// state changed.
func TestATickComputesWhenItsPrepareReportsAChange(t *testing.T) {
	tp := newTestPublisher(t)
	items, source := tp.items()
	unreadable := client.CreditOrganizations{State: client.OrganizationsStateUnreadable}
	tp.items = func() ([]creditItem, client.CreditOrganizations) { return []creditItem{}, unreadable }
	reported := false
	tp.prepare = func(time.Time) bool { return reported }
	if n := tp.wrote(publishAt); n != 1 || tp.LastState().Organizations.State != client.OrganizationsStateUnreadable {
		t.Fatalf("first tick wrote %d", n)
	}
	// The organizations file opens again, with no file changed on disk.
	tp.items = func() ([]creditItem, client.CreditOrganizations) { return items, source }
	if n := tp.wrote(publishAt.Add(10 * time.Second)); n != 0 || !tp.LastState().ComputedAt.Equal(publishAt) {
		t.Fatalf("computed with nothing reported: %d writes", n)
	}
	reported = true
	if n := tp.wrote(publishAt.Add(20 * time.Second)); n != 1 || tp.LastState().Organizations.State != client.OrganizationsStateOK {
		t.Fatalf("a reported change: %d writes, %+v", n, tp.LastState().Organizations)
	}
	if state, err := client.LoadCredits(client.CreditsPath(tp.path)); err != nil || state.Organizations.State != client.OrganizationsStateOK {
		t.Fatalf("written %+v %v", state, err)
	}
	// Reported again with nothing changed: computed, and nothing written.
	if n := tp.wrote(publishAt.Add(30 * time.Second)); n != 0 || !tp.LastState().ComputedAt.Equal(publishAt.Add(30*time.Second)) {
		t.Fatalf("an unchanged state: %d writes, computed at %v", n, tp.LastState().ComputedAt)
	}
}

// An encoding over the cap is not written: the previous file stays, and a
// warning says so, at most every ten minutes by the ticks' clock, and again
// at once if that clock is set back.
func TestTheCapKeepsTheOldFile(t *testing.T) {
	tp := newTestPublisher(t)
	if n := tp.wrote(publishAt); n != 1 {
		t.Fatal("no first write")
	}
	before, _ := os.ReadFile(client.CreditsPath(tp.path))
	tp.limit = 100
	tp.writeSnapshot(publishAt.Add(5 * time.Second))
	if n := tp.wrote(publishAt.Add(10 * time.Second)); n != 0 {
		t.Fatal("wrote over the cap")
	}
	if n := tp.wrote(publishAt.Add(80 * time.Second)); n != 0 {
		t.Fatal("wrote over the cap at the heartbeat")
	}
	if after, _ := os.ReadFile(client.CreditsPath(tp.path)); string(after) != string(before) {
		t.Fatal("the old file was replaced")
	}
	if lines := tp.logged(); len(lines) != 1 || lines[0].level != "warn" || lines[0].message != "quota-cache could not publish its credit state" || !reflect.DeepEqual(lines[0].fields, map[string]any{"reason": "too_large"}) {
		t.Fatalf("logged %+v", lines)
	}
	// Each heartbeat tries again; the warning comes back once ten minutes
	// have passed since the first, and not before.
	warned := publishAt.Add(10 * time.Second)
	for _, step := range []struct {
		at   time.Time
		want int
	}{
		{warned.Add(publishWarnEvery - time.Second), 1},
		{warned.Add(publishWarnEvery), 2},
		{warned.Add(publishWarnEvery + 70*time.Second), 2},
		// Set back past the last warning.
		{warned.Add(publishWarnEvery - time.Minute), 3},
	} {
		if n := tp.wrote(step.at); n != 0 {
			t.Fatalf("at %v: wrote over the cap", step.at)
		}
		if lines := tp.logged(); len(lines) != step.want || lines[len(lines)-1].fields["reason"] != "too_large" {
			t.Fatalf("at %v: logged %+v, want %d", step.at, lines, step.want)
		}
	}
}

// A panic in a tick is recovered and logged without its value, and the next
// tick publishes.
func TestAPanicIsRecoveredAndLoggedWithoutItsValue(t *testing.T) {
	tp := newTestPublisher(t)
	items := tp.items
	tp.items = func() ([]creditItem, client.CreditOrganizations) { panic("PANIC-CANARY /private/path") }
	tp.guarded(publishAt, func() { tp.tick(publishAt) })
	lines := tp.logged()
	if len(lines) != 1 || lines[0].level != "warn" || lines[0].fields["reason"] != "panic" || strings.Contains(fmt.Sprintf("%+v", lines), "CANARY") {
		t.Fatalf("logged %+v", lines)
	}
	tp.items = items
	if n := tp.wrote(publishAt.Add(time.Second)); n != 1 {
		t.Fatal("the tick after a panic did not publish")
	}
}

// Stop writes once more, whether or not the state changed, and after it
// nothing is computed or written.
func TestStopWritesOnceMoreThenNothing(t *testing.T) {
	tp := newTestPublisher(t)
	tp.start()
	tp.PublishNow()
	if tp.count() != 1 {
		t.Fatalf("PublishNow wrote %d", tp.count())
	}
	tp.Stop()
	if tp.count() != 2 {
		t.Fatalf("Stop wrote %d", tp.count()-1)
	}
	last := tp.LastState()
	tp.PublishNow()
	tp.tick(time.Now().Add(time.Hour))
	tp.Stop()
	if tp.count() != 2 || tp.LastState() != last {
		t.Fatal("published after Stop")
	}
}

// What decides a write leaves computed_at and every countdown out, alpha's
// renewal and the pool's two refills, and changes nothing in the state it
// reads.
func TestComparableLeavesTheStateAlone(t *testing.T) {
	tp := newTestPublisher(t)
	tp.writeUsedMeter(publishAt.Add(-time.Minute))
	tp.tick(publishAt)
	state := tp.LastState()
	before := countdowns(state)
	if before[0] <= 0 || before[1] <= 0 || before[2] <= 0 {
		t.Fatalf("countdowns %v: alpha is not counted with credit used", before)
	}
	comparable := comparableOf(state)
	if !comparable.ComputedAt.IsZero() || countdowns(comparable) != [3]int64{-1, 0, -1} {
		t.Fatalf("comparable keeps a countdown: %v", countdowns(comparable))
	}
	if refill := comparable.APICredits.Pool.NextRefill; refill == state.APICredits.Pool.NextRefill || refill.RefillAtEpoch != state.APICredits.Pool.NextRefill.RefillAtEpoch {
		t.Fatalf("the next refill %+v", refill)
	}
	if state.ComputedAt.IsZero() || countdowns(state) != before {
		t.Fatal("comparableOf changed the state")
	}
}

// On quiesce and shutdown the last file written says the meter stopped, and
// is written after the meter stopped and before the cache let go of its
// writer lock.
func TestTheLastStateWrittenSaysTheMeterStopped(t *testing.T) {
	p := newRoutePlugin(t)
	for _, stop := range []struct {
		reason string
		do     func()
	}{
		{client.MeterStopQuiesce, func() { _, _ = p.Handle(protocol.MethodPluginQuiesce, nil) }},
		{client.MeterStopShutdown, p.Shutdown},
	} {
		store, organizations := p.storesOf()
		steps := p.traceStops()
		stop.do()
		got := steps()
		storesClosed(t, stop.reason, store, organizations)
		order := []string{}
		for _, step := range got {
			if step != "write" {
				order = append(order, step)
			}
		}
		// A heartbeat may write before the stop; the stop's own write comes
		// after the publisher's loop ends and before the cache closes. Both
		// stores are closed by then, before the poller stops.
		if !reflect.DeepEqual(order, []string{"meter", "settings", "stores closed", "poller", "publisher", "cache"}) || !reflect.DeepEqual(got[len(got)-3:], []string{"publisher", "write", "cache"}) {
			t.Fatalf("%s: steps %v", stop.reason, got)
		}
		state := p.creditsFile()
		if m := state.APICredits.Meter; m == nil || m.StoppedAtEpoch == nil || m.StopReason != stop.reason {
			t.Fatalf("%s: meter %+v", stop.reason, m)
		}
		if stop.reason == client.MeterStopQuiesce {
			p.configure(protocol.MethodPluginRegister, alphaCredit)
			p.published()
		}
	}
}

// traceStops records each step of the next stop, and each write of the
// credit state, in order. Before each step it asks both stores for a write
// that would change nothing, and records "stores closed" before the first
// step at which both refuse it as closed: when the closes happened, which no
// step's name can say.
func (p *meterPlugin) traceStops() func() []string {
	p.t.Helper()
	var mu sync.Mutex
	steps := []string{}
	note := func(step string) {
		mu.Lock()
		defer mu.Unlock()
		steps = append(steps, step)
	}
	p.mu.Lock()
	publisher, store, organizations := p.publisher, p.store, p.organizations
	closed := false
	p.trace = func(step string) {
		if !closed {
			_, settingsErr := store.Apply(settings.Batch{}, nil, time.Now())
			_, organizationsErr := organizations.Apply(settings.OrganizationsBatch{BaseRevision: "none"}, time.Now())
			if closed = errors.Is(settingsErr, settings.ErrClosed) && errors.Is(organizationsErr, settings.ErrClosed); closed {
				note("stores closed")
			}
		}
		note(step)
	}
	p.mu.Unlock()
	publisher.publishMu.Lock()
	publisher.write = func(path string, raw []byte) error {
		note("write")
		return writeCreditsFile(path, raw)
	}
	publisher.publishMu.Unlock()
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), steps...)
	}
}
