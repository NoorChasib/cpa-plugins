package source

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/aggregate"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
)

type fakeHost struct {
	files []protocol.HostAuthFileEntry
	err   error
}

func (f fakeHost) ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error) {
	return f.files, f.err
}

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snapshot.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A quota-cache schema bump blinds this plugin too. It has to surface as itself
// rather than as a generic read failure, or the cause is invisible in health.
func TestSchemaBumpIsDistinguishedFromAMissingFile(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		reason string
	}{
		{"schema 2", `{"schema":2,"entries":{},"provider_cooldown":{}}`, aggregate.ReasonSchemaUnsupported},
		{"schema 0", `{"entries":{},"provider_cooldown":{}}`, aggregate.ReasonCacheMissing},
		{"corrupt", `{"schema":1,`, aggregate.ReasonCacheMissing},
		{"valid", `{"schema":1,"entries":{},"provider_cooldown":{}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Read(context.Background(), fakeHost{}, write(t, tc.body))
			if got.Reason != tc.reason {
				t.Fatalf("reason = %q; want %q", got.Reason, tc.reason)
			}
		})
	}

	missing := filepath.Join(t.TempDir(), "absent.json")
	if got := Read(context.Background(), fakeHost{}, missing); got.Reason != aggregate.ReasonCacheMissing {
		t.Fatalf("reason = %q", got.Reason)
	}
}

func TestReadableRefusesPlainly(t *testing.T) {
	if err := Readable(filepath.Join(t.TempDir(), "absent.json")); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("err = %v", err)
	}
	if err := Readable(t.TempDir()); !errors.Is(err, ErrUnreadable) {
		t.Fatal("a directory was accepted as the snapshot")
	}
	if err := Readable(write(t, `{"schema":1,"entries":{},"provider_cooldown":{}}`)); err != nil {
		t.Fatal(err)
	}
}

func TestRosterIsConvertedAndRuntimeOnlyEntriesSkipped(t *testing.T) {
	host := fakeHost{files: []protocol.HostAuthFileEntry{
		{AuthIndex: "claude-a@example.com.json", Provider: "Claude ", Email: "a@example.com"},
		// Provider falls back to Type, which is how some rosters report it.
		{AuthIndex: "codex-b@example.com.json", Type: "CODEX"},
		{AuthIndex: "claude-c@example.com.json", Provider: "claude", Disabled: true, Unavailable: true},
		{AuthIndex: "", Provider: "claude"},
		{AuthIndex: "claude-d@example.com.json", Provider: "claude", RuntimeOnly: true},
	}}
	got := Read(context.Background(), host, write(t, `{"schema":1,"entries":{},"provider_cooldown":{}}`))
	if len(got.Identities) != 3 {
		t.Fatalf("identities = %+v", got.Identities)
	}
	if got.Identities[0].Provider != "claude" || got.Identities[1].Provider != "codex" {
		t.Fatalf("provider normalization failed: %+v", got.Identities)
	}
	if !got.Identities[2].Disabled || !got.Identities[2].Unavailable {
		t.Fatalf("flags lost: %+v", got.Identities[2])
	}
}

// Load, not ReadFresh: a stale or failed entry must still reach the dashboard,
// which shows it with a badge rather than a blank.
func TestStaleAndFailedEntriesAreStillReturned(t *testing.T) {
	body := `{"schema":1,"provider_cooldown":{},"entries":{"claude:claude-a@example.com.json":{` +
		`"provider":"claude","auth_index":"claude-a@example.com.json","used_percent":76,` +
		`"reset_at":"2020-01-01T00:00:00Z","observed_at":"2020-01-01T00:00:00Z",` +
		`"last_error":"quota fetch failed","failures":3}}}`
	got := Read(context.Background(), fakeHost{}, write(t, body))
	if got.Reason != "" {
		t.Fatalf("reason = %q", got.Reason)
	}
	entry, ok := got.Snapshot.Entries["claude:claude-a@example.com.json"]
	if !ok || entry.Percent != 76 || entry.Failures != 3 {
		t.Fatalf("a stale, failed entry was dropped: %+v", got.Snapshot.Entries)
	}
}

// A roster the host could not supply is a source failure. Reporting it as a
// successful read of an empty roster lets the caller publish a blank document
// and latch that emptiness as its last good state, which is exactly the
// failure "serve stale over empty" exists to prevent.
func TestRosterFailureIsReportedAsASourceFailure(t *testing.T) {
	path := write(t, `{"schema":1,"entries":{},"provider_cooldown":{}}`)

	got := Read(context.Background(), fakeHost{err: errors.New("unavailable")}, path)
	if got.Reason != aggregate.ReasonRosterUnavailable {
		t.Fatalf("reason = %q; want %q", got.Reason, aggregate.ReasonRosterUnavailable)
	}
	if got.Identities == nil {
		t.Fatal("identities must be an empty slice, never nil")
	}

	// A host that genuinely has no credentials is not a failure.
	if got := Read(context.Background(), fakeHost{}, path); got.Reason != "" {
		t.Fatalf("reason = %q; an empty roster is legitimately empty", got.Reason)
	}

	// A missing snapshot is the more specific problem and keeps the reason.
	missing := filepath.Join(t.TempDir(), "absent.json")
	if got := Read(context.Background(), fakeHost{err: errors.New("x")}, missing); got.Reason != aggregate.ReasonCacheMissing {
		t.Fatalf("reason = %q", got.Reason)
	}
}

// A path that cannot be opened without blocking must never be opened. This runs
// on the watcher goroutine, which the shutdown path waits for while holding the
// plugin's lifecycle lock, so a blocking open wedges the plugin permanently and
// stops CPA unloading it.
func TestUnopenablePathsAreRejectedWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "snapshot.json")
	if out, err := exec.Command("mkfifo", fifo).CombinedOutput(); err != nil {
		t.Skipf("mkfifo unavailable: %v %s", err, out)
	}
	done := make(chan Result, 1)
	go func() { done <- Read(context.Background(), fakeHost{}, fifo) }()
	select {
	case got := <-done:
		if got.Reason != aggregate.ReasonCacheMissing {
			t.Fatalf("reason = %q", got.Reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Read blocked on a FIFO; this deadlocks the plugin and CPA's unload")
	}
	if err := Readable(fifo); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("Readable accepted a FIFO: %v", err)
	}
}

// The roster is the only place the routing story comes from, and the fields
// that carry it are the ones that change between reads of an unchanged
// snapshot. Dropping them on the way through was the shape of this plugin
// before the strip existed, and nothing else in the pipeline would have noticed.
func TestRecentRequestsAreCarriedThroughInOrder(t *testing.T) {
	host := fakeHost{files: []protocol.HostAuthFileEntry{{
		AuthIndex: "claude-agency@example.com.json",
		Provider:  "claude",
		RecentRequests: []protocol.HostRecentRequestEntry{
			{Time: "14:40-14:50", Success: 3},
			{Time: "14:50-15:00", Success: 7, Failed: 1},
			{Time: "15:00-15:10"},
		},
	}}}

	got := Read(context.Background(), host, write(t, `{"schema":1,"entries":{},"provider_cooldown":{}}`))
	if len(got.Identities) != 1 {
		t.Fatalf("identities = %d; want 1", len(got.Identities))
	}
	want := []aggregate.RecentRequest{
		{Label: "14:40-14:50", Success: 3},
		{Label: "14:50-15:00", Success: 7, Failed: 1},
		{Label: "15:00-15:10"},
	}
	ring := got.Identities[0].Recent
	if len(ring) != len(want) {
		t.Fatalf("ring has %d buckets; want %d", len(ring), len(want))
	}
	for i := range want {
		if ring[i] != want[i] {
			t.Fatalf("bucket %d = %+v; want %+v — the ring is reordered or rewritten", i, ring[i], want[i])
		}
	}
}

// A host that reports no ring must produce no ring, rather than an empty one:
// the aggregate publishes those as different facts, and only this conversion
// knows which it was given.
func TestAnAbsentRingStaysAbsent(t *testing.T) {
	host := fakeHost{files: []protocol.HostAuthFileEntry{
		{AuthIndex: "claude-quiet@example.com.json", Provider: "claude"},
	}}
	got := Read(context.Background(), host, write(t, `{"schema":1,"entries":{},"provider_cooldown":{}}`))
	if ring := got.Identities[0].Recent; ring != nil {
		t.Fatalf("a host that reported no ring produced %d buckets", len(ring))
	}
}

// creditsBody is a minimal credit state, computed at computed.
func creditsBody(computed string) string {
	return `{"schema":1,"quota_cache_version":"0.2.0","computed_at":"` + computed + `","valid_until":null,` +
		`"inputs":{},"settings":{"state":"ok","editable":true,"revision":""},"api_credits":null,"renewals":{},` +
		`"organizations":{"state":"missing","revision":"","config_items_ignored":0}}`
}

// The credit state is read from beside the snapshot on every read. A missing
// or unreadable one is named, and never makes the snapshot a source failure:
// only the API credit card and the renewal dates depend on it.
func TestTheCreditStateIsReadBesideTheSnapshot(t *testing.T) {
	const snapshot = `{"schema":1,"written_at":"2026-10-09T11:55:00Z","entries":{},"provider_cooldown":{}}`
	path := write(t, snapshot)
	creditsPath := filepath.Join(filepath.Dir(path), "snapshot.credits.json")

	got := Read(context.Background(), fakeHost{}, path)
	if got.Reason != "" || got.Credits != nil || got.CreditsError != aggregate.CreditsMissing {
		t.Fatalf("no state: reason %q state %v error %q", got.Reason, got.Credits, got.CreditsError)
	}

	for name, body := range map[string]string{"corrupt": `{"schema":1,`, "schema 2": `{"schema":2}`} {
		if err := os.WriteFile(creditsPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		got = Read(context.Background(), fakeHost{}, path)
		if got.Reason != "" || got.Credits != nil || got.CreditsError != aggregate.CreditsUnreadable {
			t.Fatalf("%s state: reason %q state %v error %q", name, got.Reason, got.Credits, got.CreditsError)
		}
	}

	if err := os.WriteFile(creditsPath, []byte(creditsBody("2026-10-09T11:55:10Z")), 0o600); err != nil {
		t.Fatal(err)
	}
	got = Read(context.Background(), fakeHost{}, path)
	if got.CreditsError != "" || got.Credits == nil || got.Credits.QuotaCacheVersion != "0.2.0" || got.Credits.Renewals == nil {
		t.Fatalf("state = %+v, error %q", got.Credits, got.CreditsError)
	}

	// A state beside a snapshot that cannot be read is still read: the
	// snapshot's failure is reported on its own.
	if err := os.WriteFile(path, []byte(`{"schema":1,`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got = Read(context.Background(), fakeHost{}, path); got.Reason != aggregate.ReasonCacheMissing || got.Credits == nil || got.CreditsError != "" {
		t.Fatalf("broken snapshot: reason %q state %v error %q", got.Reason, got.Credits, got.CreditsError)
	}
}

// A FIFO where the credit state belongs must not block the read either.
func TestAnUnopenableCreditStateDoesNotBlock(t *testing.T) {
	path := write(t, `{"schema":1,"entries":{},"provider_cooldown":{}}`)
	fifo := filepath.Join(filepath.Dir(path), "snapshot.credits.json")
	if out, err := exec.Command("mkfifo", fifo).CombinedOutput(); err != nil {
		t.Skipf("mkfifo unavailable: %v %s", err, out)
	}
	done := make(chan Result, 1)
	go func() { done <- Read(context.Background(), fakeHost{}, path) }()
	select {
	case got := <-done:
		if got.Reason != "" || got.CreditsError != aggregate.CreditsUnreadable {
			t.Fatalf("reason %q credits error %q", got.Reason, got.CreditsError)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Read blocked on a FIFO in the credit state's place")
	}
}

// When a credit state counts as abandoned. A snapshot written more than five
// minutes after the state was computed means no publisher keeps the state up:
// it is abandoned, and kept only for health. Five minutes exactly is still a
// running publisher's. A snapshot with no written_at says nothing either way.
func TestAnAbandonedStateIsToldApart(t *testing.T) {
	for _, tc := range []struct {
		name, written string
		want          string
	}{
		{"5m1s after", "2026-10-09T12:05:01Z", aggregate.CreditsAbandoned},
		{"5m after", "2026-10-09T12:05:00Z", ""},
		{"written first", "2026-10-09T11:59:00Z", ""},
		{"no written_at", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			written := ""
			if tc.written != "" {
				written = `"written_at":"` + tc.written + `",`
			}
			path := write(t, `{"schema":1,`+written+`"entries":{},"provider_cooldown":{}}`)
			if err := os.WriteFile(filepath.Join(filepath.Dir(path), "snapshot.credits.json"), []byte(creditsBody("2026-10-09T12:00:00Z")), 0o600); err != nil {
				t.Fatal(err)
			}
			got := Read(context.Background(), fakeHost{}, path)
			if got.Reason != "" || got.CreditsError != tc.want || got.Credits == nil {
				t.Fatalf("reason %q error %q state %v; want error %q with the state kept", got.Reason, got.CreditsError, got.Credits, tc.want)
			}
		})
	}
}
