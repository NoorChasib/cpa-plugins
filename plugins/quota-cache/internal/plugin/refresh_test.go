package plugin

import (
	"context"
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
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/cache"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

// Placeholder CPA auth indexes, 16 lower-case hex digits each.
var refreshAuths = []string{"00000000000000a1", "00000000000000b2", "00000000000000c3"}

const refreshMessage = "quota-cache refreshing every account after a banked reset"

// refreshHost is claudeHost with three synthetic Claude credentials, keeping
// every line logged to it.
type refreshHost struct {
	*claudeHost
	mu    sync.Mutex
	lines []logLine
}

func newRefreshHost() *refreshHost { return &refreshHost{claudeHost: newClaudeHost()} }

func (*refreshHost) ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error) {
	var roster []protocol.HostAuthFileEntry
	for _, index := range refreshAuths {
		roster = append(roster, protocol.HostAuthFileEntry{AuthIndex: index, Provider: "claude"})
	}
	return roster, nil
}
func (h *refreshHost) Log(_ context.Context, level, message string, fields map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lines = append(h.lines, logLine{level, message, fields})
}

// logged is every line with message.
func (h *refreshHost) logged(message string) []logLine {
	h.mu.Lock()
	defer h.mu.Unlock()
	var found []logLine
	for _, line := range h.lines {
		if line.message == message {
			found = append(found, line)
		}
	}
	return found
}

func refreshID(n int) string { return fmt.Sprintf("%016x", n) }

func refreshFor(id int, authIndex string, at time.Time) client.RefreshRequest {
	return client.RefreshRequest{ID: refreshID(id), Provider: "claude", AuthIndex: authIndex, Reason: client.RefreshReasonReset, At: at}
}

// placeRefresh writes the request file as Quota Glance does, by rename, so
// each write is a new file even when its bytes and size repeat. raw, when
// given, is written in place of requests.
func placeRefresh(t *testing.T, snapshotPath, raw string, requests ...client.RefreshRequest) {
	t.Helper()
	if raw == "" {
		encoded, err := json.Marshal(map[string]any{"schema": 1, "requests": requests})
		if err != nil {
			t.Fatal(err)
		}
		raw = string(encoded)
	}
	path := client.RefreshRequestPath(snapshotPath)
	if err := os.WriteFile(path+".tmp", []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Fatal(err)
	}
}

func idsOf(requests []client.RefreshRequest) []string {
	ids := []string{}
	for _, r := range requests {
		ids = append(ids, r.ID)
	}
	return ids
}

var refreshNow = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

// As the poller starts, a request at least a minute old has had its round, or
// is too old for one, and is passed over; a younger one is a reset spent just
// before the restart, and is acted on. A request too old for a round, either
// way, never has one. Quota Glance keeps every live request when it adds the
// next, so the ones passed over are in each file it writes after the restart,
// and stay passed over.
func TestRefreshStartupPassesOverOldRequests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.json")
	found := []client.RefreshRequest{
		refreshFor(1, refreshAuths[0], refreshNow.Add(-2*time.Minute)),
		refreshFor(2, refreshAuths[0], refreshNow.Add(-time.Minute)),
		refreshFor(3, refreshAuths[1], refreshNow.Add(-59*time.Second)),
		refreshFor(4, refreshAuths[2], refreshNow.Add(-30*time.Second)),
		refreshFor(5, refreshAuths[2], refreshNow.Add(11*time.Minute)),
	}
	placeRefresh(t, path, "", found...)
	w := newRefreshWatch(path)
	due, failed := w.due(refreshNow)
	if failed != nil || !reflect.DeepEqual(idsOf(due), []string{refreshID(3), refreshID(4)}) {
		t.Fatalf("due %v, failed %v", idsOf(due), failed)
	}
	// Nothing changed, so nothing is read.
	if due, _ = w.due(refreshNow.Add(time.Second)); len(due) != 0 {
		t.Fatalf("an unchanged file gave %v", idsOf(due))
	}
	// The next banked reset: the file is written again with every request
	// above and a new one, and only the new one is acted on.
	now := refreshNow.Add(30 * time.Second)
	found = append(found, refreshFor(6, refreshAuths[1], now))
	placeRefresh(t, path, "", found...)
	if due, _ = w.due(now); !reflect.DeepEqual(idsOf(due), []string{refreshID(6)}) {
		t.Fatalf("after a rewrite due %v", idsOf(due))
	}
	// The request dated too far ahead comes within RefreshRequestMaxAge of
	// the clock, and still has no round.
	now = refreshNow.Add(2 * time.Minute)
	if !found[4].Live(now) {
		t.Fatal("request 5 should be live by now")
	}
	placeRefresh(t, path, "", found[4])
	if due, _ = w.due(now); len(due) != 0 {
		t.Fatalf("once live, due %v", idsOf(due))
	}
}

// Each request is acted on once, oldest first, however often the file is
// rewritten, and one older than RefreshRequestMaxAge never is.
func TestRefreshEachRequestIsActedOnOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.json")
	w := newRefreshWatch(path)
	if due, failed := w.due(refreshNow); len(due) != 0 || failed != nil {
		t.Fatalf("no file: %v %v", idsOf(due), failed)
	}
	first := refreshFor(1, refreshAuths[0], refreshNow)
	placeRefresh(t, path, "", first)
	if due, _ := w.due(refreshNow.Add(time.Second)); !reflect.DeepEqual(idsOf(due), []string{refreshID(1)}) {
		t.Fatalf("due %v", idsOf(due))
	}
	// Rewritten with two more, out of order, and one too old for a round.
	now := refreshNow.Add(20 * time.Second)
	placeRefresh(t, path, "", first, refreshFor(3, refreshAuths[2], now), refreshFor(2, refreshAuths[1], now.Add(-time.Second)),
		refreshFor(4, refreshAuths[2], now.Add(-client.RefreshRequestMaxAge-time.Second)))
	if due, _ := w.due(now); !reflect.DeepEqual(idsOf(due), []string{refreshID(2), refreshID(3)}) {
		t.Fatalf("due %v", idsOf(due))
	}
	// The same requests again, in a new file.
	placeRefresh(t, path, "", first, refreshFor(3, refreshAuths[2], now), refreshFor(2, refreshAuths[1], now.Add(-time.Second)))
	if due, _ := w.due(now.Add(time.Second)); len(due) != 0 {
		t.Fatalf("acted on again: %v", idsOf(due))
	}
	// Quota Glance keeps every live request when it adds the next, so the
	// file is written again with these while they are still live, up to the
	// last second of their life, and only the new request is acted on.
	last := now.Add(client.RefreshRequestMaxAge - time.Second)
	for i, at := range []time.Time{now.Add(client.RefreshRequestMaxAge * 3 / 4), last} {
		next := refreshFor(5+i, refreshAuths[0], at)
		placeRefresh(t, path, "", refreshFor(3, refreshAuths[2], now), refreshFor(2, refreshAuths[1], now.Add(-time.Second)), next)
		if due, _ := w.due(at); !reflect.DeepEqual(idsOf(due), []string{next.ID}) {
			t.Fatalf("%s on: due %v", at.Sub(now), idsOf(due))
		}
	}
	// A call forgets ids only after it has looked for new ones, so the call
	// at the last second shows nothing it forgot. Written once more at that
	// second, the file holds request 2, exactly RefreshRequestMaxAge old, and
	// request 3, a second younger: both still live, so both must still be
	// remembered.
	placeRefresh(t, path, "", refreshFor(3, refreshAuths[2], now), refreshFor(2, refreshAuths[1], now.Add(-time.Second)), refreshFor(6, refreshAuths[0], last))
	if due, _ := w.due(last); len(due) != 0 {
		t.Fatalf("forgotten while still live: due %v", idsOf(due))
	}
	// Once a request is too old to act on, its id is forgotten, and not
	// before.
	placeRefresh(t, path, `{"schema":1,"requests":[]}`)
	w.due(now.Add(client.RefreshRequestMaxAge + time.Minute))
	if _, kept := w.applied[refreshID(6)]; len(w.applied) != 2 || !kept {
		t.Fatalf("remembered: %v", w.applied)
	}
	placeRefresh(t, path, `{"schema":1,"requests":[]}`)
	w.due(last.Add(client.RefreshRequestMaxAge + time.Minute))
	if len(w.applied) != 0 {
		t.Fatalf("still remembered: %v", w.applied)
	}
}

// refreshCache is a writer over refreshHost's three credentials, each polled
// once, one second apart from refreshNow.
func refreshCache(t *testing.T, host *refreshHost) (*cache.Cache, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cache", "snapshot.json")
	current, err := cache.Open(cache.Options{Path: path, Interval: 15 * time.Minute, Spacing: time.Second}, fetcherWith(host, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(current.Close)
	for i := range refreshAuths {
		if err := current.Step(context.Background(), refreshNow.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	return current, path
}

// A request for a polled credential starts a round and is logged once, with
// its provider and how many credentials the round polls, never the auth
// index. One for a credential the cache does not poll changes nothing and is
// not logged.
func TestFollowRefreshStartsARoundAndLogsIt(t *testing.T) {
	host := newRefreshHost()
	current, path := refreshCache(t, host)
	p := New(host)
	w := newRefreshWatch(path)
	now := refreshNow.Add(5 * time.Minute)
	p.followRefresh(context.Background(), current, w, now)
	request := refreshFor(1, refreshAuths[1], now.Add(-10*time.Second))
	placeRefresh(t, path, "", request, refreshFor(2, "00000000000000d4", now))
	p.followRefresh(context.Background(), current, w, now)
	p.followRefresh(context.Background(), current, w, now.Add(time.Second))
	lines := host.logged(refreshMessage)
	if len(lines) != 1 || lines[0].level != "info" || !reflect.DeepEqual(lines[0].fields, map[string]any{"provider": "claude", "accounts": 3}) {
		t.Fatalf("logged %+v", host.lines)
	}
	if status := refreshStatusOf(current, w); status.Remaining != 3 || status.RequestedAt == nil || !status.RequestedAt.Equal(request.At) {
		t.Fatalf("status %+v", status)
	}
	for _, line := range host.lines {
		if strings.Contains(fmt.Sprint(line), refreshAuths[1]) {
			t.Fatalf("a line names the auth index: %+v", line)
		}
	}
}

// A file that cannot be read changes nothing and is warned about once per
// change. No file is no warning.
func TestAnUnreadableRefreshFileIsWarnedOncePerChange(t *testing.T) {
	host := newRefreshHost()
	current, path := refreshCache(t, host)
	p := New(host)
	w := newRefreshWatch(path)
	warnings := func() int {
		n := 0
		for _, line := range host.lines {
			if line.level == "warn" {
				n++
			}
		}
		return n
	}
	p.followRefresh(context.Background(), current, w, refreshNow)
	placeRefresh(t, path, `{"schema":2,"requests":[]}`)
	for i := range 3 {
		p.followRefresh(context.Background(), current, w, refreshNow.Add(time.Duration(i)*time.Second))
	}
	placeRefresh(t, path, `not json`)
	p.followRefresh(context.Background(), current, w, refreshNow.Add(5*time.Second))
	p.followRefresh(context.Background(), current, w, refreshNow.Add(6*time.Second))
	if n := warnings(); n != 2 || current.RoundRemaining() != 0 {
		t.Fatalf("%d warnings, remaining %d; want 2 and 0: %+v", n, current.RoundRemaining(), host.lines)
	}
}

// notOpened is what LoadRefreshRequests returns for a file it could not open.
var notOpened = fmt.Errorf("%w: %w", client.ErrUnavailable, client.ErrRefreshNotRead)

// failReads makes w's reads fail as one of a file that could not be opened
// does while *fail is set, and counts every read.
func failReads(w *refreshWatch, fail *bool, reads *int) {
	w.load = func(path string) ([]client.RefreshRequest, error) {
		*reads++
		if *fail {
			return []client.RefreshRequest{}, notOpened
		}
		return client.LoadRefreshRequests(path)
	}
}

// A read that did not settle, as when CPA is out of file descriptors, says
// nothing of the file, so the next check reads it again though it has not
// changed, and reports that failure once. The first read to settle is the
// one the startup rule applies to.
func TestARefreshFileThatCouldNotBeOpenedIsReadAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.json")
	placeRefresh(t, path, "", refreshFor(1, refreshAuths[0], refreshNow.Add(-2*time.Minute)), refreshFor(2, refreshAuths[1], refreshNow.Add(-30*time.Second)))
	w := newRefreshWatch(path)
	reads, fail := 0, true
	failReads(w, &fail, &reads)
	if due, failed := w.due(refreshNow); len(due) != 0 || !errors.Is(failed, client.ErrRefreshNotRead) {
		t.Fatalf("a failed open: %v %v", idsOf(due), failed)
	}
	if due, failed := w.due(refreshNow.Add(2 * time.Second)); len(due) != 0 || failed != nil || reads != 2 {
		t.Fatalf("failing again: %v %v, %d reads", idsOf(due), failed, reads)
	}
	fail = false
	if due, failed := w.due(refreshNow.Add(4 * time.Second)); failed != nil || !reflect.DeepEqual(idsOf(due), []string{refreshID(2)}) {
		t.Fatalf("once it opens: %v %v", idsOf(due), failed)
	}
	if due, _ := w.due(refreshNow.Add(6 * time.Second)); len(due) != 0 || reads != 3 {
		t.Fatalf("a settled read was read again: %v, %d reads", idsOf(due), reads)
	}

	// Later, a request whose first read fails is acted on at the next check.
	now := refreshNow.Add(time.Minute)
	placeRefresh(t, path, "", refreshFor(2, refreshAuths[1], refreshNow.Add(-30*time.Second)), refreshFor(3, refreshAuths[2], now))
	fail = true
	if due, failed := w.due(now); len(due) != 0 || !errors.Is(failed, client.ErrRefreshNotRead) {
		t.Fatalf("a failed open of a new file: %v %v", idsOf(due), failed)
	}
	fail = false
	if due, failed := w.due(now.Add(2 * time.Second)); failed != nil || !reflect.DeepEqual(idsOf(due), []string{refreshID(3)}) {
		t.Fatalf("once the new file opens: %v %v", idsOf(due), failed)
	}
}

// A file that could not be opened is warned about once per change, saying it
// is tried again, however many checks fail on it, and its request starts a
// round as soon as it opens. One that then opens but fails a rule is warned
// about as ignored until it changes.
func TestARefreshFileThatCouldNotBeOpenedIsWarnedOncePerChange(t *testing.T) {
	const notOpenedMessage = "quota-cache cannot open the refresh request file Quota Glance writes; it tries again every two seconds"
	const notReadMessage = "quota-cache cannot read the refresh request file Quota Glance writes; it is ignored until it changes"
	host := newRefreshHost()
	current, path := refreshCache(t, host)
	p := New(host)
	w := newRefreshWatch(path)
	now := refreshNow.Add(5 * time.Minute)
	p.followRefresh(context.Background(), current, w, now)
	reads, fail := 0, true
	failReads(w, &fail, &reads)
	placeRefresh(t, path, "", refreshFor(1, refreshAuths[1], now))
	for i := range 3 {
		if p.followRefresh(context.Background(), current, w, now.Add(time.Duration(i)*refreshCheckEvery)) {
			t.Fatal("acted on a file that could not be opened")
		}
	}
	fail = false
	if !p.followRefresh(context.Background(), current, w, now.Add(3*refreshCheckEvery)) || current.RoundRemaining() != 3 {
		t.Fatalf("once it opens: remaining %d", current.RoundRemaining())
	}
	placeRefresh(t, path, `not json`)
	fail = true
	p.followRefresh(context.Background(), current, w, now.Add(4*refreshCheckEvery))
	fail = false
	p.followRefresh(context.Background(), current, w, now.Add(5*refreshCheckEvery))
	p.followRefresh(context.Background(), current, w, now.Add(6*refreshCheckEvery))
	if opened, read, acted := host.logged(notOpenedMessage), host.logged(notReadMessage), host.logged(refreshMessage); len(opened) != 2 || len(read) != 1 || len(acted) != 1 || reads != 6 {
		t.Fatalf("%d not opened, %d not read, %d acted on, %d reads; want 2, 1, 1 and 6: %+v", len(opened), len(read), len(acted), reads, host.lines)
	}
}

// refreshPlugin is a registered plugin polling refreshHost's credentials one
// second apart.
func refreshPlugin(t *testing.T) (*Plugin, *refreshHost, string) {
	t.Helper()
	host := newRefreshHost()
	p := New(host)
	t.Cleanup(p.Shutdown)
	path := filepath.Join(t.TempDir(), "cache", "snapshot.json")
	raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("cache-path: " + path + "\nrequest-spacing: 1s\n")})
	if _, err := p.Handle(protocol.MethodPluginRegister, raw); err != nil {
		t.Fatal(err)
	}
	return p, host, path
}

func refreshBlock(t *testing.T, p *Plugin) map[string]any {
	t.Helper()
	status := statusOf(t, p)
	block, ok := status["refresh"].(map[string]any)
	if !ok {
		t.Fatalf("status has no refresh block: %v", status["refresh"])
	}
	return block
}

// statusOf is what the status route answers, called as CPA calls it.
func statusOf(t *testing.T, p *Plugin) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(protocol.ManagementRequest{Method: "GET", Path: "/v0/management/plugins/quota-cache/status"})
	result, err := p.Handle(protocol.MethodManagementHandle, raw)
	if err != nil {
		t.Fatal(err)
	}
	response := result.(protocol.ManagementResponse)
	var status map[string]any
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &status) != nil {
		t.Fatalf("status %d: %s", response.StatusCode, response.Body)
	}
	return status
}

// waitForSnapshot polls the snapshot until ready is true of it.
func waitForSnapshot(t *testing.T, path string, within time.Duration, what string, ready func(client.Snapshot) bool) client.Snapshot {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if snapshot, err := client.Load(path); err == nil && ready(snapshot) {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s within %s", what, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// End to end through the real poller: a request Quota Glance appends polls
// its credential at the next tick, then every other one, each a spacing
// apart, and the status says so.
func TestABankedResetRefreshesEveryAccountThroughThePoller(t *testing.T) {
	p, host, path := refreshPlugin(t)
	polled := waitForSnapshot(t, path, 10*time.Second, "the first round", func(s client.Snapshot) bool {
		for _, index := range refreshAuths {
			if s.Entries[client.Key("claude", index)].ObservedAt.IsZero() {
				return false
			}
		}
		return true
	})
	if block := refreshBlock(t, p); !reflect.DeepEqual(block, map[string]any{"requested_at": nil, "remaining": 0.0}) {
		t.Fatalf("refresh before any request: %v", block)
	}
	target := refreshAuths[2]
	at := time.Now().UTC()
	if err := client.AppendRefreshRequest(client.RefreshRequestPath(path), client.RefreshRequest{Provider: "claude", AuthIndex: target, Reason: client.RefreshReasonReset}, at); err != nil {
		t.Fatal(err)
	}
	moved := func(s client.Snapshot, index string) bool {
		key := client.Key("claude", index)
		return s.Entries[key].LastAttempt.After(polled.Entries[key].LastAttempt) && s.Entries[key].LastError == ""
	}
	// The poller ticks once a second and reads the file before each Step, so
	// the target is polled within two spacings, here with room for a slow
	// machine. The file's own two-second check can only bring that forward.
	first := waitForSnapshot(t, path, 3*time.Second, "the target polled", func(s client.Snapshot) bool { return moved(s, target) })
	if moved(first, refreshAuths[0]) || moved(first, refreshAuths[1]) {
		t.Fatal("another credential was polled before the target")
	}
	after := waitForSnapshot(t, path, 5*time.Second, "every credential polled", func(s client.Snapshot) bool {
		return moved(s, refreshAuths[0]) && moved(s, refreshAuths[1])
	})
	var order []string
	for _, poll := range after.History[len(after.History)-3:] {
		order = append(order, poll.AuthIndex)
	}
	if !reflect.DeepEqual(order, []string{target, refreshAuths[0], refreshAuths[1]}) {
		t.Fatalf("round order %v", order)
	}
	block := refreshBlock(t, p)
	if block["requested_at"] != at.Truncate(time.Second).Format(time.RFC3339) || block["remaining"] != 0.0 {
		t.Fatalf("refresh after the round: %v", block)
	}
	if lines := host.logged(refreshMessage); len(lines) != 1 || !reflect.DeepEqual(lines[0].fields, map[string]any{"provider": "claude", "accounts": 3}) {
		t.Fatalf("logged %+v", lines)
	}
}

// A request is seen within seconds however long the spacing is, not at the
// next tick, where it could be older than RefreshRequestMaxAge and never
// acted on. Its credential is polled at once when the spacing allows, here
// because the last poll was more than a spacing ago, and the others wait for
// the spacing.
func TestALongSpacingStillActsOnARequestAtOnce(t *testing.T) {
	host := newRefreshHost()
	path := filepath.Join(t.TempDir(), "cache", "snapshot.json")
	// Every credential was polled 40 minutes ago at an hourly interval, so
	// none is due and a 30-minute spacing is over.
	last := time.Now().UTC().Add(-40 * time.Minute)
	seed, err := cache.Open(cache.Options{Path: path, Interval: time.Hour, Spacing: time.Second}, fetcherWith(host, ""))
	if err != nil {
		t.Fatal(err)
	}
	for i := range refreshAuths {
		if err := seed.Step(context.Background(), last.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	seed.Close()
	seeded, err := client.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := New(host)
	t.Cleanup(p.Shutdown)
	raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("cache-path: " + path + "\npoll-interval: 1h\nrequest-spacing: 30m\n")})
	if _, err := p.Handle(protocol.MethodPluginRegister, raw); err != nil {
		t.Fatal(err)
	}
	target := refreshAuths[1]
	at := time.Now().UTC()
	if err := client.AppendRefreshRequest(client.RefreshRequestPath(path), client.RefreshRequest{Provider: "claude", AuthIndex: target, Reason: client.RefreshReasonReset}, at); err != nil {
		t.Fatal(err)
	}
	moved := func(s client.Snapshot, index string) bool {
		key := client.Key("claude", index)
		return s.Entries[key].LastAttempt.After(seeded.Entries[key].LastAttempt)
	}
	polled := waitForSnapshot(t, path, 3*refreshCheckEvery, "the target polled", func(s client.Snapshot) bool { return moved(s, target) })
	if moved(polled, refreshAuths[0]) || moved(polled, refreshAuths[2]) {
		t.Fatal("another credential was polled inside the spacing")
	}
	block := refreshBlock(t, p)
	if block["requested_at"] != at.Truncate(time.Second).Format(time.RFC3339) || block["remaining"] != 2.0 {
		t.Fatalf("refresh after the target's poll: %v", block)
	}
	if lines := host.logged(refreshMessage); len(lines) != 1 || !reflect.DeepEqual(lines[0].fields, map[string]any{"provider": "claude", "accounts": 3}) {
		t.Fatalf("logged %+v", lines)
	}
}

// The round goes on one spacing at a time after a credential polled between
// ticks. The poll leaves the next slot one spacing after it, and the ticker
// keeps time from there. On its old beat, the next tick would come before that
// slot and be turned away, and the round's next poll would wait for the tick
// after, up to two spacings away: six seconds here.
func TestARoundKeepsItsSpacingAfterAPollBetweenTicks(t *testing.T) {
	const spacing = 4 * time.Second
	host := newRefreshHost()
	path := filepath.Join(t.TempDir(), "cache", "snapshot.json")
	// Every credential was polled 40 minutes ago at an hourly interval, so
	// none is due and the spacing is over.
	last := time.Now().UTC().Add(-40 * time.Minute)
	seed, err := cache.Open(cache.Options{Path: path, Interval: time.Hour, Spacing: time.Second}, fetcherWith(host, ""))
	if err != nil {
		t.Fatal(err)
	}
	for i := range refreshAuths {
		if err := seed.Step(context.Background(), last.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	seed.Close()
	seeded, err := client.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := New(host)
	t.Cleanup(p.Shutdown)
	raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("cache-path: " + path + "\npoll-interval: 1h\nrequest-spacing: 4s\n")})
	if _, err := p.Handle(protocol.MethodPluginRegister, raw); err != nil {
		t.Fatal(err)
	}
	// The poller's first Step has read the request file and polled nothing.
	// Its ticks then come four and eight seconds after it, and the file's
	// checks two, four and six seconds after it, so a request written now is
	// found by the check at two seconds, between ticks.
	for deadline := time.Now().Add(time.Second); ; time.Sleep(10 * time.Millisecond) {
		if activity, _ := statusOf(t, p)["activity"].(map[string]any); activity != nil && activity["last_scan"] != "0001-01-01T00:00:00Z" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the poller's first Step within a second")
		}
	}
	target := refreshAuths[1]
	if err := client.AppendRefreshRequest(client.RefreshRequestPath(path), client.RefreshRequest{Provider: "claude", AuthIndex: target, Reason: client.RefreshReasonReset}, time.Now()); err != nil {
		t.Fatal(err)
	}
	moved := func(s client.Snapshot, index string) bool {
		key := client.Key("claude", index)
		return s.Entries[key].LastAttempt.After(seeded.Entries[key].LastAttempt)
	}
	waitForSnapshot(t, path, 2*refreshCheckEvery, "the target polled", func(s client.Snapshot) bool { return moved(s, target) })
	// A poll moves its entry's last attempt in the snapshot saved before the
	// request, and is in the history only from the one saved after it, so the
	// wait is for the history.
	after := waitForSnapshot(t, path, 3*spacing, "the round's next poll", func(s client.Snapshot) bool {
		return len(s.History) >= len(seeded.History)+2
	})
	polls := after.History[len(seeded.History) : len(seeded.History)+2]
	if polls[0].AuthIndex != target {
		t.Fatalf("polled %s first, not the target", polls[0].AuthIndex)
	}
	// Never closer than the spacing less its tolerance, a tenth of it.
	if gap := polls[1].StartedAt.Sub(polls[0].StartedAt); gap < spacing-spacing/10 || gap > spacing+spacing/4 {
		t.Fatalf("the round's next poll came %s after the target's, want one spacing, %s", gap, spacing)
	}
}

// Requests appended while the poller ticks, status reads and a schedule
// change race neither one another nor a poll in progress.
func TestRefreshRacesNeitherTicksNorStatus(t *testing.T) {
	p, _, path := refreshPlugin(t)
	// The status route answers 503 until the first snapshot is written.
	waitForSnapshot(t, path, 5*time.Second, "the first snapshot", func(client.Snapshot) bool { return true })
	stop := time.Now().Add(2500 * time.Millisecond)
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; time.Now().Before(stop); i++ {
			request := client.RefreshRequest{Provider: "claude", AuthIndex: refreshAuths[i%len(refreshAuths)], Reason: client.RefreshReasonReset}
			if err := client.AppendRefreshRequest(client.RefreshRequestPath(path), request, time.Now()); err != nil {
				t.Error(err)
				return
			}
			time.Sleep(40 * time.Millisecond)
		}
	})
	wg.Go(func() {
		for time.Now().Before(stop) {
			if block := refreshBlock(t, p); block["remaining"].(float64) < 0 {
				t.Errorf("refresh %v", block)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
	wg.Go(func() {
		time.Sleep(time.Second)
		raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("cache-path: " + path + "\npoll-interval: 30m\nrequest-spacing: 1s\n")})
		if _, err := p.Handle(protocol.MethodPluginReconfigure, raw); err != nil {
			t.Error(err)
		}
	})
	wg.Wait()
	if block := refreshBlock(t, p); block["requested_at"] == nil {
		t.Fatalf("no request was acted on: %v", block)
	}
}
