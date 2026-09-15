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
