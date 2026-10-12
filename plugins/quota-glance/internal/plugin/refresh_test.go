package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
)

// refreshAuth is a Codex credential's auth index in the form current CPA
// reports, and refreshLegacyAuth one in an older CPA's file-name form, which
// Quota Cache's refresh request file does not accept.
const (
	refreshAuth       = "0123456789abcdef"
	refreshLegacyAuth = "codex-older@example.com.json"
)

// consumeHost answers the two Codex requests a press makes: the credit
// inventory, with one credit, and the consume, with consume.
type consumeHost struct {
	*fakeHost
	consume protocol.HostHTTPResponse
}

func (h *consumeHost) HTTPDo(ctx context.Context, request protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	if _, err := h.fakeHost.HTTPDo(ctx, request); err != nil {
		return protocol.HostHTTPResponse{}, err
	}
	if request.Method == http.MethodPost {
		return h.consume, nil
	}
	expires := time.Now().Add(20 * 24 * time.Hour).UTC().Format(time.RFC3339)
	return protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"credits":[{"id":"credit-a","status":"available","expires_at":"` + expires + `"}]}`)}, nil
}

// publishCredits writes the credit state a Quota Cache 0.2.0 or newer
// publishes beside the snapshot, computed at computedAt, with no organization
// listed. Quota Cache running renews it at least every minute.
func publishCredits(t *testing.T, cachePath string, computedAt time.Time) {
	t.Helper()
	raw, err := json.Marshal(qc.CreditState{
		Schema: qc.CreditsSchema, QuotaCacheVersion: "0.2.0", ComputedAt: computedAt.UTC().Truncate(time.Second),
		Renewals: map[string]qc.RenewalSetting{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(qc.CreditsPath(cachePath), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeRefreshSnapshot writes a snapshot holding one Codex credential, with two
// banked resets, under authIndex. Quota Cache last polled it lastPoll from now,
// and its next free slot, next_request, is nextRequest from now.
func writeRefreshSnapshot(t *testing.T, cachePath, authIndex string, lastPoll, nextRequest time.Duration) {
	t.Helper()
	now := time.Now().UTC()
	stamp := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	snapshot := map[string]any{
		"schema": 1, "written_at": stamp(0), "next_request": stamp(nextRequest), "provider_cooldown": map[string]any{},
		"entries": map[string]any{
			"codex:" + authIndex: map[string]any{
				"provider": "codex", "auth_index": authIndex, "used_percent": 100, "reset_at": stamp(48 * time.Hour),
				"observed_at": stamp(lastPoll), "last_attempt": stamp(lastPoll), "next_attempt": stamp(lastPoll + 15*time.Minute),
				"failures": 0,
				"quota":    map[string]any{"schema": 1, "observed_at": stamp(lastPoll), "reset_credits": map[string]any{"available_count": 2}},
			},
		},
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// refreshPlugin serves a snapshot holding one Codex credential, with two banked
// resets, under authIndex, and answers its consume with consume. A Quota Cache
// that reads refresh requests is running beside it: its credit state is
// current, and at the default 10-second spacing it last polled a minute ago,
// so it is free to poll again at once.
func refreshPlugin(t *testing.T, authIndex string, consume protocol.HostHTTPResponse) (*Plugin, *consumeHost, string) {
	t.Helper()
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "snapshot.json")
	publishCredits(t, cachePath, time.Now())
	writeRefreshSnapshot(t, cachePath, authIndex, -time.Minute, -50*time.Second)
	host := &consumeHost{
		fakeHost: &fakeHost{files: []protocol.HostAuthFileEntry{{AuthIndex: authIndex, Provider: "codex"}}},
		consume:  consume,
	}
	p := New(host)
	t.Cleanup(p.Shutdown)
	cfg := "cache-path: " + cachePath + "\ndata-dir: " + filepath.Join(dir, "data") + "\nweb-token: test-token\n"
	if _, err := configure(t, p, cfg); err != nil {
		t.Fatal(err)
	}
	waitForDocument(t, p, func(doc map[string]any) bool {
		credentials, _ := doc["credentials"].([]any)
		for _, item := range credentials {
			credential, _ := item.(map[string]any)
			credits, _ := credential["resetCredits"].(map[string]any)
			if credential["id"] == authIndex && credits["redeemable"] == true {
				return true
			}
		}
		return false
	})
	return p, host, cachePath
}

// pressReset sends one confirmed press for authIndex down the console door and
// returns the answer.
func pressReset(t *testing.T, p *Plugin, authIndex string) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(protocol.ManagementRequest{
		Method: "POST", Path: "/v0/management/plugins/quota-glance/redeem",
		Headers: http.Header{"Content-Type": {"application/json"}},
		Body:    []byte(`{"credentialId":"` + authIndex + `","confirmed":true}`),
	})
	result, err := p.Handle(protocol.MethodManagementHandle, raw)
	if err != nil {
		t.Fatal(err)
	}
	res := result.(protocol.ManagementResponse)
	var body map[string]any
	if err := json.Unmarshal(res.Body, &body); err != nil {
		t.Fatalf("answer is not JSON: %s", res.Body)
	}
	return res.StatusCode, body
}

// refreshRequests is what Quota Cache would read beside the snapshot.
func refreshRequests(t *testing.T, cachePath string) []qc.RefreshRequest {
	t.Helper()
	requests, err := qc.LoadRefreshRequests(qc.RefreshRequestPath(cachePath))
	if err != nil {
		t.Fatalf("the refresh request file cannot be read: %v", err)
	}
	return requests
}

func refreshLogLines(host *consumeHost) []logLine {
	var lines []logLine
	for _, line := range host.logged() {
		if line.message == logRefreshNotRequested {
			lines = append(lines, line)
		}
	}
	return lines
}

var sixteenHex = regexp.MustCompile(`^[0-9a-f]{16}$`)

// A spent reset leaves one request beside the snapshot, for this credential's
// provider and auth index, written as Quota Cache reads it.
func TestASpentResetAsksQuotaCacheToRefreshThatAccount(t *testing.T) {
	p, host, cachePath := refreshPlugin(t, refreshAuth, protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"code":"reset","windows_reset":2}`)})
	before := time.Now().UTC().Truncate(time.Second)
	status, body := pressReset(t, p, refreshAuth)
	if status != http.StatusOK || body["outcome"] != "reset" || body["refreshRequested"] != true {
		t.Fatalf("answer = %d %v, want a reset with refreshRequested true", status, body)
	}
	requests := refreshRequests(t, cachePath)
	if len(requests) != 1 {
		t.Fatalf("refresh requests = %+v, want one", requests)
	}
	got := requests[0]
	if got.Provider != "codex" || got.AuthIndex != refreshAuth || got.Reason != qc.RefreshReasonReset || !sixteenHex.MatchString(got.ID) {
		t.Fatalf("request = %+v", got)
	}
	if got.At.Before(before) || got.At.After(time.Now().UTC()) {
		t.Fatalf("request made at %s, want during the press", got.At)
	}
	// The answer names the request's own instant, which Quota Cache's poll of
	// the account cannot precede.
	if body["refreshRequestedAtEpoch"] != float64(got.At.Unix()) {
		t.Fatalf("refreshRequestedAtEpoch = %v, want the request's %d", body["refreshRequestedAtEpoch"], got.At.Unix())
	}
	info, err := os.Stat(qc.RefreshRequestPath(cachePath))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("refresh request file: %v, mode %v", err, info.Mode())
	}
	if lines := refreshLogLines(host); len(lines) != 0 {
		t.Fatalf("logged %v after a request that was written", lines)
	}

	// A second press adds a second request; the first is kept.
	if _, body := pressReset(t, p, refreshAuth); body["refreshRequested"] != true {
		t.Fatalf("second answer = %v", body)
	}
	if requests := refreshRequests(t, cachePath); len(requests) != 2 || requests[0].ID == requests[1].ID {
		t.Fatalf("after two presses: %+v", requests)
	}
}

// A press the provider refused, or that found nothing to spend, writes nothing.
func TestAPressThatSpentNothingWritesNoRefreshRequest(t *testing.T) {
	for name, consume := range map[string]protocol.HostHTTPResponse{
		"rate limited": {StatusCode: 429},
		"refused":      {StatusCode: 403},
		"no credit":    {StatusCode: 200, Body: []byte(`{"code":"no_credit"}`)},
	} {
		p, host, cachePath := refreshPlugin(t, refreshAuth, consume)
		status, body := pressReset(t, p, refreshAuth)
		if body["refreshRequested"] != false {
			t.Fatalf("%s: answer = %d %v, want refreshRequested false", name, status, body)
		}
		if _, err := os.Lstat(qc.RefreshRequestPath(cachePath)); !os.IsNotExist(err) {
			t.Fatalf("%s: a refresh request file exists after a press that spent nothing (%v)", name, err)
		}
		if len(host.providerRequests()) != 2 {
			t.Fatalf("%s: provider requests = %d, want the inventory and the consume", name, len(host.providerRequests()))
		}
		if lines := refreshLogLines(host); len(lines) != 0 {
			t.Fatalf("%s: logged %v", name, lines)
		}
	}
}

// An unknown outcome may have spent the reset, so it asks as a spend does.
func TestAnUnknownOutcomeWritesOneRefreshRequest(t *testing.T) {
	p, _, cachePath := refreshPlugin(t, refreshAuth, protocol.HostHTTPResponse{StatusCode: 502})
	status, body := pressReset(t, p, refreshAuth)
	if status != http.StatusBadGateway || body["error"] != "outcome_unknown" || body["refreshRequested"] != true {
		t.Fatalf("answer = %d %v", status, body)
	}
	if requests := refreshRequests(t, cachePath); len(requests) != 1 || requests[0].AuthIndex != refreshAuth {
		t.Fatalf("refresh requests = %+v, want one for this credential", requests)
	}
}

// A request that cannot be written leaves the answer as it was but for
// refreshRequested, and says so once in CPA's log, naming neither the file
// nor the credential.
func TestARefreshThatCannotBeWrittenLeavesTheAnswerAndLogsOnce(t *testing.T) {
	spent := protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"code":"reset","windows_reset":2}`)}
	want := map[string]any{
		"provider": "codex", "outcome": "reset", "windowsReset": 2.0, "remainingCount": 0.0,
		"snapshotPending": true, "cooldown": "cleared", "refreshRequested": false,
	}
	for name, setup := range map[string]struct {
		authIndex string
		breakIt   func(t *testing.T, cachePath string)
	}{
		// A rename onto a directory fails, which is a write error like any
		// other: a read-only directory, a full disk.
		"write fails": {refreshAuth, func(t *testing.T, cachePath string) {
			if err := os.Mkdir(qc.RefreshRequestPath(cachePath), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		"older CPA's auth index": {refreshLegacyAuth, func(*testing.T, string) {}},
	} {
		p, host, cachePath := refreshPlugin(t, setup.authIndex, spent)
		setup.breakIt(t, cachePath)
		status, body := pressReset(t, p, setup.authIndex)
		if status != http.StatusOK || fmt.Sprint(body) != fmt.Sprint(want) {
			t.Fatalf("%s: answer = %d %v, want %v", name, status, body, want)
		}
		lines := refreshLogLines(host)
		if len(lines) != 1 || lines[0].level != "info" || len(lines[0].fields) != 0 {
			t.Fatalf("%s: log lines = %+v, want one info line with no fields", name, lines)
		}
		for _, line := range host.logged() {
			text := line.message + fmt.Sprint(line.fields)
			if strings.Contains(text, filepath.Dir(cachePath)) || strings.Contains(text, setup.authIndex) {
				t.Fatalf("%s: a log line names the file or the credential: %+v", name, line)
			}
		}
	}
}

// Presses that finish together each keep their request: writes are taken one
// at a time, and each reads the file the one before it wrote.
func TestRefreshRequestsMadeTogetherAreAllKept(t *testing.T) {
	p, _, cachePath := refreshPlugin(t, refreshAuth, protocol.HostHTTPResponse{StatusCode: 429})
	const presses = qc.MaxRefreshRequests - 1
	var wg sync.WaitGroup
	for i := range presses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := (quotaCacheRefresh{p}).RequestRefresh("claude", fmt.Sprintf("%016x", i+1)); !ok {
				t.Error("a refresh request was not written")
			}
		}()
	}
	wg.Wait()
	if requests := refreshRequests(t, cachePath); len(requests) != presses {
		t.Fatalf("kept %d of %d requests made together: %+v", len(requests), presses, requests)
	}
}

// The request is written whatever reads it, but the answer promises a refresh
// only while a Quota Cache that acts on it runs beside the snapshot. One older
// than 0.2.0 publishes no credit state and never reads the file; one stopped
// leaves its last state to age. Either way the card waits for the next
// scheduled poll, and the page must not say within a minute. Neither is a
// failure to log.
func TestARefreshIsPromisedOnlyWhileQuotaCacheReadsIt(t *testing.T) {
	spent := protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"code":"reset","windows_reset":2}`)}
	for name, setup := range map[string]func(t *testing.T, cachePath string){
		"older than 0.2.0": func(t *testing.T, cachePath string) {
			if err := os.Remove(qc.CreditsPath(cachePath)); err != nil {
				t.Fatal(err)
			}
		},
		"stopped": func(t *testing.T, cachePath string) {
			publishCredits(t, cachePath, time.Now().Add(-refreshReaderMaxAge-time.Minute))
		},
	} {
		p, host, cachePath := refreshPlugin(t, refreshAuth, spent)
		setup(t, cachePath)
		status, body := pressReset(t, p, refreshAuth)
		if status != http.StatusOK || body["outcome"] != "reset" || body["refreshRequested"] != false {
			t.Fatalf("%s: answer = %d %v, want a reset with refreshRequested false", name, status, body)
		}
		if _, present := body["refreshRequestedAtEpoch"]; present {
			t.Fatalf("%s: answer %v names an instant for a refresh it does not promise", name, body)
		}
		if requests := refreshRequests(t, cachePath); len(requests) != 1 || requests[0].AuthIndex != refreshAuth {
			t.Fatalf("%s: refresh requests = %+v, want the one written all the same", name, requests)
		}
		if lines := refreshLogLines(host); len(lines) != 0 {
			t.Fatalf("%s: logged %v after a request that was written", name, lines)
		}

		// Quota Cache publishing again is all it takes.
		publishCredits(t, cachePath, time.Now())
		if _, body := pressReset(t, p, refreshAuth); body["refreshRequested"] != true {
			t.Fatalf("%s: with Quota Cache publishing again, answer = %v", name, body)
		}
	}
}

// Quota Cache polls the account at its next free slot, which a request does
// not move. With a five-minute request-spacing and a reset spent 30 seconds
// after its last poll, that is four and a half minutes off: the answer must
// not promise a refresh the page would say comes within a minute. The request
// is written all the same, and once the slot is free a press is promised one.
func TestARefreshIsPromisedOnlyWhenQuotaCacheCanPollSoon(t *testing.T) {
	spent := protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"code":"reset","windows_reset":2}`)}
	p, host, cachePath := refreshPlugin(t, refreshAuth, spent)
	writeRefreshSnapshot(t, cachePath, refreshAuth, -30*time.Second, 4*time.Minute+30*time.Second)
	status, body := pressReset(t, p, refreshAuth)
	if status != http.StatusOK || body["outcome"] != "reset" || body["refreshRequested"] != false {
		t.Fatalf("answer = %d %v, want a reset with refreshRequested false", status, body)
	}
	if _, present := body["refreshRequestedAtEpoch"]; present {
		t.Fatalf("answer %v names an instant for a refresh it does not promise", body)
	}
	if requests := refreshRequests(t, cachePath); len(requests) != 1 || requests[0].AuthIndex != refreshAuth {
		t.Fatalf("refresh requests = %+v, want the one written all the same", requests)
	}
	if lines := refreshLogLines(host); len(lines) != 0 {
		t.Fatalf("logged %v after a request that was written", lines)
	}

	writeRefreshSnapshot(t, cachePath, refreshAuth, -6*time.Minute, -time.Minute)
	if _, body := pressReset(t, p, refreshAuth); body["refreshRequested"] != true {
		t.Fatalf("with Quota Cache's slot free, answer = %v", body)
	}
}

// Quota Cache can poll a Codex account within a minute only when its next
// free slot and any 429 pause on Codex end within it, and, when they are
// still ahead, its poller wakes at least once a minute.
func TestRefreshPollSoon(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return now.Add(d) }
	for name, c := range map[string]struct {
		polled, next time.Duration
		paused       map[string]time.Time
		unpolled     bool
		want         bool
	}{
		"the default spacing, just after a poll":    {polled: -time.Second, next: 9 * time.Second, want: true},
		"a 50-second spacing, just after a poll":    {polled: -2 * time.Second, next: 48 * time.Second, want: true},
		"a five-minute spacing, its slot free":      {polled: -6 * time.Minute, next: -time.Minute, want: true},
		"a five-minute spacing, its slot ahead":     {polled: -30 * time.Second, next: 270 * time.Second},
		"a five-minute spacing, its slot 30s ahead": {polled: -270 * time.Second, next: 30 * time.Second},
		"Codex paused after a 429":                  {polled: -time.Minute, next: -50 * time.Second, paused: map[string]time.Time{"codex": at(10 * time.Minute)}},
		"Codex's pause ending within a minute":      {polled: -time.Minute, next: -50 * time.Second, paused: map[string]time.Time{"codex": at(30 * time.Second)}, want: true},
		"Codex's pause over":                        {polled: -time.Minute, next: -50 * time.Second, paused: map[string]time.Time{"codex": at(-time.Minute)}, want: true},
		"Claude paused, not Codex":                  {polled: -time.Minute, next: -50 * time.Second, paused: map[string]time.Time{"claude": at(10 * time.Minute)}, want: true},
		"no poll recorded, its slot ahead":          {next: 5 * time.Second, unpolled: true},
		"no poll recorded, its slot free":           {next: -5 * time.Second, unpolled: true, want: true},
	} {
		snapshot := qc.Snapshot{
			Schema: 1, NextRequest: at(c.next), ProviderCooldown: map[string]time.Time{},
			Entries: map[string]qc.Entry{"codex:" + refreshAuth: {Provider: "codex", AuthIndex: refreshAuth, LastAttempt: at(c.polled)}},
		}
		// No older poll may stand for the latest: each would make the spacing
		// look ten minutes or more longer than it is. Map order is random, so
		// there are many of them and each case is checked many times: a
		// refreshPollSoon that took whichever entry it met first, or last,
		// then fails every run rather than now and then.
		for i := range 15 {
			provider, authIndex := []string{"claude", "codex"}[i%2], fmt.Sprintf("%016x", i+1)
			snapshot.Entries[provider+":"+authIndex] = qc.Entry{Provider: provider, AuthIndex: authIndex, LastAttempt: at(c.polled - time.Duration(10+i)*time.Minute)}
		}
		if c.unpolled {
			snapshot.Entries = map[string]qc.Entry{}
		}
		for provider, until := range c.paused {
			snapshot.ProviderCooldown[provider] = until
		}
		for range 32 {
			if got := refreshPollSoon(snapshot, "codex", now); got != c.want {
				t.Errorf("%s: refreshPollSoon = %v, want %v", name, got, c.want)
				break
			}
		}
	}
}
