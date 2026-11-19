package monitor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/account-health-pushover/internal/config"
	"github.com/NoorChasib/cpa-plugins/plugins/account-health-pushover/internal/health"
	"github.com/NoorChasib/cpa-plugins/plugins/account-health-pushover/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/account-health-pushover/internal/state"
	quotaclient "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

func tosSuspendedEntry(index, email string) protocol.HostAuthFileEntry {
	entry := oauthEntry(index, "antigravity", email, "disabled", "tos_violation", true)
	entry.Disabled = true
	return entry
}

func TestAntigravityTOSSuspensionAlertsEvenWithNotifyDisabledOff(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("ag", "antigravity", "ag@example.com", "active", "", false))
	mock := newMockPushover(t)
	monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.ahp"))
	if monitor.cfg.NotifyDisabled {
		t.Fatal("test requires notify-disabled off")
	}
	ctx := context.Background()
	if err := monitor.Reconcile(ctx, "baseline"); err != nil {
		t.Fatal(err)
	}

	host.set(tosSuspendedEntry("ag", "ag@example.com"))
	if err := monitor.Reconcile(ctx, "suspended"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
	all := mock.all()
	for _, want := range []string{"CLIProxyAPI: Antigravity account suspended", "ag@example.com was suspended by Google for a terms-of-service violation", "Appeal the suspension", "Incident: credential_down", "Reason: tos_suspended"} {
		if !strings.Contains(all, want) {
			t.Fatalf("message missing %q: %s", want, all)
		}
	}
	row := monitor.Snapshot().Accounts[0]
	if row.Health != health.CredentialDown || row.ReasonCode != string(health.ReasonTOSSuspended) {
		t.Fatalf("row=%+v", row)
	}
	waitFor(t, 5*time.Second, func() bool { return !monitor.Snapshot().Accounts[0].LastAlertAt.IsZero() })
	if err := monitor.Reconcile(ctx, "repeat"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if mock.count() != 1 {
		t.Fatalf("suspension alert repeated: %s", mock.all())
	}

	// The operator re-enables the credential once Google reinstates it.
	host.set(oauthEntry("ag", "antigravity", "ag@example.com", "active", "", false))
	if err := monitor.Reconcile(ctx, "reinstated"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 2 })
	if !strings.Contains(mock.all(), "CLIProxyAPI: Antigravity account recovered") {
		t.Fatalf("recovery missing: %s", mock.all())
	}
}

// A suspension that lands inside an incident that already alerted (here a
// reauth_required one, whose alert wrongly tells the user to sign in again)
// still gets its own message with the appeal guidance, once.
func TestAntigravityTOSSuspensionAlertsInsideAnAlertedIncident(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("ag", "antigravity", "ag@example.com", "error", "invalid_grant", true))
	mock := newMockPushover(t)
	monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.ahp"))
	ctx := context.Background()
	if err := monitor.Reconcile(ctx, "reauth"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
	waitFor(t, 5*time.Second, func() bool { return !monitor.Snapshot().Accounts[0].LastAlertAt.IsZero() })
	if row := monitor.Snapshot().Accounts[0]; row.Health != health.ReauthRequired {
		t.Fatalf("setup row=%+v", row)
	}

	host.set(tosSuspendedEntry("ag", "ag@example.com"))
	if err := monitor.Reconcile(ctx, "suspended"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 2 })
	for _, want := range []string{"CLIProxyAPI: Antigravity account suspended", "Appeal the suspension", "Reason: tos_suspended"} {
		if !strings.Contains(mock.all(), want) {
			t.Fatalf("message missing %q: %s", want, mock.all())
		}
	}
	waitFor(t, 5*time.Second, func() bool { return monitor.Snapshot().Accounts[0].ReasonCode == string(health.ReasonTOSSuspended) })

	// Staying suspended does not repeat it.
	if err := monitor.Reconcile(ctx, "repeat"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if mock.count() != 2 {
		t.Fatalf("suspension alert repeated: %s", mock.all())
	}
}

// Other providers keep reading a disabled credential as an operator action,
// whatever its message, and stay silent with notify-disabled off.
func TestDisabledNonAntigravityCredentialStaysSilent(t *testing.T) {
	for _, provider := range []string{"claude", "codex", "xai"} {
		t.Run(provider, func(t *testing.T) {
			host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
			entry := oauthEntry("one", provider, "a@example.com", "disabled", "tos_violation", true)
			entry.Disabled = true
			host.set(entry)
			mock := newMockPushover(t)
			monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.ahp"))
			if err := monitor.Reconcile(context.Background(), "test"); err != nil {
				t.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond)
			if mock.count() != 0 {
				t.Fatalf("disabled %s credential alerted: %s", provider, mock.all())
			}
			if row := monitor.Snapshot().Accounts[0]; row.Health != health.Disabled || row.ReasonCode != string(health.ReasonOperatorDisabled) {
				t.Fatalf("row=%+v", row)
			}
		})
	}
}

type agWindow struct {
	key     string
	model   string
	percent float64
	reset   time.Time
}

// writeAntigravityCache writes a quota-cache snapshot holding one Antigravity
// entry. Entry.ObservedAt stays zero, as quota-cache leaves it when the
// summary carries no default Gemini weekly window, so only ReadFreshWindows
// can serve the entry.
func writeAntigravityCache(t *testing.T, path, authIndex string, observedAt time.Time, windows ...agWindow) {
	t.Helper()
	entry := quotaclient.Entry{Provider: quotaclient.ProviderAntigravity, AuthIndex: authIndex}
	for _, w := range windows {
		entry.Windows = append(entry.Windows, quotaclient.EntryWindow{Key: w.key, Model: w.model, Title: w.key + "/" + w.model, UsedPercent: w.percent, ResetAt: w.reset, ObservedAt: observedAt})
	}
	snapshot := quotaclient.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]quotaclient.Entry{
		quotaclient.Key(quotaclient.ProviderAntigravity, authIndex): entry,
	}}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func familyStatus(t *testing.T, monitor *Monitor, family string) QuotaFamilyStatus {
	t.Helper()
	for _, row := range monitor.Snapshot().Accounts {
		if row.Quota == nil {
			continue
		}
		for _, status := range row.Quota.Families {
			if status.Family == family {
				return status
			}
		}
	}
	return QuotaFamilyStatus{}
}

func TestAntigravityFamiliesAlertAndRecoverIndependently(t *testing.T) {
	base := time.Date(2026, time.November, 19, 12, 0, 0, 0, time.UTC)
	reset := base.Add(72 * time.Hour)
	observed := base.Add(-time.Minute)
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "snapshot.json")
	statePath := filepath.Join(dir, "state.ahp")
	host := newQuotaHost()
	host.set(oauthEntry("ag", "antigravity", "ag@example.com", "active", "", false))
	mock := newMockPushover(t)
	configure := func(cfg *config.Config) {
		cfg.StateFile = statePath
		cfg.QuotaCachePath = cachePath
	}
	monitor := newQuotaMonitor(t, host, mock, configure)
	monitor.now = func() time.Time { return base }
	ctx := context.Background()
	poll := func(m *Monitor) {
		t.Helper()
		if err := m.PollQuota(ctx, "test"); err != nil {
			t.Fatal(err)
		}
	}

	gemini, shared, argon := 40.0, 96.0, 100.0
	argonReset := reset
	write := func() {
		writeAntigravityCache(t, cachePath, "ag", observed,
			agWindow{key: quotaclient.WindowSession, percent: 100, reset: base.Add(2 * time.Hour)},
			agWindow{key: quotaclient.WindowWeekly, percent: gemini, reset: reset},
			agWindow{key: quotaclient.WindowModelWeekly, model: quotaclient.AntigravityModelShared, percent: shared, reset: reset},
			agWindow{key: quotaclient.WindowModelSession, model: quotaclient.AntigravityModelShared, percent: 100, reset: base.Add(time.Hour)},
			agWindow{key: quotaclient.WindowModelWeekly, model: quotaclient.AntigravityModelArgon, percent: argon, reset: argonReset},
			agWindow{key: quotaclient.WindowModelWeekly, model: "neon", percent: 100, reset: reset},
			agWindow{key: quotaclient.WindowRawPrefix + "antigravity:gemini-other-weekly", percent: 100, reset: reset},
		)
	}
	write()
	if err := monitor.Reconcile(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		poll(monitor)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 2 })
	all := mock.all()
	for _, want := range []string{
		"CLIProxyAPI: Antigravity (Claude & GPT) weekly limit almost used",
		"ag@example.com has used 96% of its Claude & GPT weekly limit (4% remaining).",
		"CLIProxyAPI: Antigravity (Gemini Argon) weekly limit reached",
		"ag@example.com has used 100% of its Gemini Argon weekly limit.",
		"Its other model families have separate limits",
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("messages missing %q: %s", want, all)
		}
	}
	for _, unwanted := range []string{"Antigravity (Gemini) weekly", "neon", "Session", "raw:"} {
		if strings.Contains(all, unwanted) {
			t.Fatalf("messages contain %q: %s", unwanted, all)
		}
	}
	waitFor(t, 5*time.Second, func() bool {
		return !familyStatus(t, monitor, "shared").WarningSentAt.IsZero() && !familyStatus(t, monitor, "argon").ExhaustedSentAt.IsZero()
	})
	row := monitor.Snapshot().Accounts[0]
	if row.Quota == nil || row.Quota.Percent != nil || row.Quota.LastError != "" || len(row.Quota.Families) != 3 {
		t.Fatalf("quota row=%+v", row.Quota)
	}
	for i, want := range []string{"gemini", "shared", "argon"} {
		if row.Quota.Families[i].Family != want {
			t.Fatalf("family order=%+v", row.Quota.Families)
		}
	}
	if got := familyStatus(t, monitor, "gemini"); got.Name != "Gemini" || got.Percent == nil || *got.Percent != 40 || !got.ResetAt.Equal(reset) || !got.ObservedAt.Equal(observed) {
		t.Fatalf("gemini=%+v", got)
	}

	// The Claude & GPT pool drops back under the warning level within the same
	// window: only its latches re-arm. Argon stays latched.
	shared = 10
	write()
	poll(monitor)
	if got := familyStatus(t, monitor, "shared"); !got.WarningSentAt.IsZero() {
		t.Fatalf("shared latch not re-armed: %+v", got)
	}
	if got := familyStatus(t, monitor, "argon"); got.ExhaustedSentAt.IsZero() {
		t.Fatalf("argon latch cleared by another family: %+v", got)
	}
	shared = 97
	write()
	poll(monitor)
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 3 })
	// Gemini crosses the warning level on its own latch.
	gemini = 96
	write()
	poll(monitor)
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 4 })
	messages := strings.Split(mock.all(), "\n---\n")
	if !strings.Contains(messages[2], "Antigravity (Claude & GPT) weekly limit almost used") || !strings.Contains(messages[3], "Antigravity (Gemini) weekly limit almost used") || !strings.Contains(messages[3], "of its Gemini weekly limit") {
		t.Fatalf("messages=%q", messages)
	}
	waitFor(t, 5*time.Second, func() bool {
		return !familyStatus(t, monitor, "shared").WarningSentAt.IsZero() && !familyStatus(t, monitor, "gemini").WarningSentAt.IsZero()
	})

	// Each family's latch is persisted under a family-qualified window key.
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted state.Data
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	families := persisted.Accounts["antigravity:ag"].Quota.Families
	if families["shared"].WindowKey != "shared@"+reset.Format(time.RFC3339) || families["gemini"].WindowKey != "gemini@"+reset.Format(time.RFC3339) || len(families) != 3 {
		t.Fatalf("persisted families=%+v", families)
	}
	if persisted.Accounts["antigravity:ag"].Quota.WindowKey != "" {
		t.Fatalf("family alert wrote the single-window key: %+v", persisted.Accounts["antigravity:ag"].Quota)
	}

	// A restart in the same windows repeats nothing.
	monitor.Stop()
	restarted := newQuotaMonitor(t, host, mock, configure)
	restarted.now = func() time.Time { return base }
	if err := restarted.Reconcile(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	poll(restarted)
	time.Sleep(50 * time.Millisecond)
	if mock.count() != 4 {
		t.Fatalf("restart re-alerted: %s", mock.all())
	}

	// A new Argon window alerts Argon again and nothing else.
	argonReset = reset.Add(7 * 24 * time.Hour)
	write()
	poll(restarted)
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 5 })
	poll(restarted)
	time.Sleep(50 * time.Millisecond)
	if mock.count() != 5 || !strings.Contains(strings.Split(mock.all(), "\n---\n")[4], "Antigravity (Gemini Argon) weekly limit reached") {
		t.Fatalf("messages=%s", mock.all())
	}
	if host.authCalls != 0 || host.requestCount() != 0 {
		t.Fatalf("cache mode read credentials (%d) or sent requests (%d)", host.authCalls, host.requestCount())
	}
}

func TestAntigravityFamilyAbsentFromOneReadKeepsItsLatchUntilReset(t *testing.T) {
	base := time.Date(2026, time.November, 19, 12, 0, 0, 0, time.UTC)
	sharedReset := base.Add(time.Hour)
	geminiReset := base.Add(72 * time.Hour)
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "snapshot.json")
	host := newQuotaHost()
	host.set(oauthEntry("ag", "antigravity", "ag@example.com", "active", "", false))
	mock := newMockPushover(t)
	monitor := newQuotaMonitor(t, host, mock, func(cfg *config.Config) {
		cfg.StateFile = filepath.Join(dir, "state.ahp")
		cfg.QuotaCachePath = cachePath
	})
	now := base
	monitor.now = func() time.Time { return now }
	ctx := context.Background()
	if err := monitor.Reconcile(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	gemini := agWindow{key: quotaclient.WindowWeekly, percent: 5, reset: geminiReset}
	shared := agWindow{key: quotaclient.WindowModelWeekly, model: quotaclient.AntigravityModelShared, percent: 96, reset: sharedReset}

	writeAntigravityCache(t, cachePath, "ag", now.Add(-time.Minute), gemini, shared)
	if err := monitor.PollQuota(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
	waitFor(t, 5*time.Second, func() bool { return !familyStatus(t, monitor, "shared").WarningSentAt.IsZero() })

	// One read without the pool (a fallback answer, say) keeps its latch...
	writeAntigravityCache(t, cachePath, "ag", now.Add(-time.Minute), gemini)
	if err := monitor.PollQuota(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if got := familyStatus(t, monitor, "shared"); got.WarningSentAt.IsZero() {
		t.Fatalf("absent family lost its latch: %+v", got)
	}
	// ...so its return in the same window does not alert twice.
	writeAntigravityCache(t, cachePath, "ag", now.Add(-time.Minute), gemini, shared)
	if err := monitor.PollQuota(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if mock.count() != 1 {
		t.Fatalf("returning family re-alerted: %s", mock.all())
	}

	// Once its reset has passed, an absent family is dropped.
	now = sharedReset.Add(time.Minute)
	writeAntigravityCache(t, cachePath, "ag", now.Add(-time.Minute), gemini)
	if err := monitor.PollQuota(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if got := familyStatus(t, monitor, "shared"); got.Family != "" {
		t.Fatalf("finished family kept: %+v", got)
	}
	if got := familyStatus(t, monitor, "gemini"); got.Percent == nil || *got.Percent != 5 {
		t.Fatalf("gemini=%+v", got)
	}
}

func TestAntigravityQuotaIsSkippedInStandaloneMode(t *testing.T) {
	host := newQuotaHost()
	claude := oauthEntry("c1", "claude", "a@example.com", "active", "", false)
	antigravity := oauthEntry("ag", "antigravity", "ag@example.com", "active", "", false)
	host.roster = []protocol.HostAuthFileEntry{claude, antigravity}
	for _, entry := range host.roster {
		host.runtime[entry.AuthIndex] = entry
	}
	host.setQuota("c1", 10, weekReset)
	mock := newMockPushover(t)
	monitor := newQuotaMonitor(t, host, mock, nil)
	if monitor.cfg.QuotaCachePath != "" {
		t.Fatal("test requires standalone mode")
	}
	ctx := context.Background()
	if err := monitor.Reconcile(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if err := monitor.PollQuota(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	status := monitor.Snapshot()
	if status.LastQuotaPollError != "" || status.LastQuotaPoll.IsZero() {
		t.Fatalf("status=%+v", status)
	}
	byIndex := make(map[string]AccountStatus)
	for _, row := range status.Accounts {
		byIndex[row.AuthIndex] = row
	}
	if row := byIndex["ag"]; row.Health != health.Healthy || row.Quota != nil {
		t.Fatalf("antigravity row=%+v quota=%+v", row, row.Quota)
	}
	if q := byIndex["c1"].Quota; q == nil || q.Percent == nil || *q.Percent != 10 || len(q.Families) != 0 {
		t.Fatalf("claude quota=%+v", q)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.authCalls != 1 || len(host.requests) != 1 || strings.Contains(host.requests[0].URL, "googleapis") {
		t.Fatalf("auth reads=%d requests=%+v", host.authCalls, host.requests)
	}
}

func TestAntigravityCacheReadsNeverFallBackAndReportStaticErrors(t *testing.T) {
	now := time.Date(2026, time.November, 19, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "snapshot.json")
	host := newQuotaHost()
	m := &Monitor{cfg: config.Config{QuotaCachePath: path}, quotaHost: host}
	entry := protocol.HostAuthFileEntry{Provider: "antigravity", AuthIndex: "ag"}
	if !m.quotaTracked("antigravity") || (&Monitor{}).quotaTracked("antigravity") {
		t.Fatal("antigravity must be tracked in cache mode only")
	}
	for _, provider := range []string{"claude", "codex", "xai"} {
		if !m.quotaTracked(provider) || !(&Monitor{}).quotaTracked(provider) {
			t.Fatalf("%s tracking changed", provider)
		}
	}

	if _, err := m.readQuotaFamilies(entry, now); err == nil || sanitizeQuotaError(err) != "usage request failed" {
		t.Fatalf("missing cache: %v", err)
	}
	// Only five-hour windows: no weekly family to alert on.
	writeAntigravityCache(t, path, "ag", now.Add(-time.Minute),
		agWindow{key: quotaclient.WindowSession, percent: 100, reset: now.Add(time.Hour)},
		agWindow{key: quotaclient.WindowModelSession, model: quotaclient.AntigravityModelShared, percent: 100, reset: now.Add(time.Hour)},
	)
	if _, err := m.readQuotaFamilies(entry, now); err == nil || sanitizeQuotaError(err) != "usage endpoint did not report a weekly window" {
		t.Fatalf("session-only entry: %v", err)
	}
	// Windows older than the 30-minute freshness bound are unavailable.
	writeAntigravityCache(t, path, "ag", now.Add(-31*time.Minute),
		agWindow{key: quotaclient.WindowWeekly, percent: 99, reset: now.Add(time.Hour)},
	)
	if _, err := m.readQuotaFamilies(entry, now); err == nil {
		t.Fatal("stale window accepted")
	}
	// A family reported twice keeps its first reading.
	writeAntigravityCache(t, path, "ag", now.Add(-time.Minute),
		agWindow{key: quotaclient.WindowModelWeekly, model: quotaclient.AntigravityModelCarbon, percent: 20, reset: now.Add(time.Hour)},
		agWindow{key: quotaclient.WindowModelWeekly, model: quotaclient.AntigravityModelCarbon, percent: 99, reset: now.Add(time.Hour)},
		agWindow{key: quotaclient.WindowModelWeekly, model: quotaclient.AntigravityModelHydrogen, percent: 30, reset: now.Add(time.Hour)},
		agWindow{key: quotaclient.WindowModelWeekly, model: familyGemini, percent: 99, reset: now.Add(time.Hour)},
	)
	families, err := m.readQuotaFamilies(entry, now)
	if err != nil || len(families) != 2 || families[0].Family != "carbon" || families[0].Percent != 20 || families[1].Family != "hydrogen" {
		t.Fatalf("families=%+v err=%v", families, err)
	}
	if host.authCalls != 0 || host.requestCount() != 0 {
		t.Fatal("cache-only Antigravity read accessed credentials or HTTP")
	}
}

func TestAntigravityDisplayNamesAndWindowKeys(t *testing.T) {
	if got := titleProvider("antigravity"); got != "Antigravity" {
		t.Fatalf("titleProvider=%q", got)
	}
	for provider, want := range map[string]string{"claude": "Claude", "codex": "Codex", "xai": "Grok", "": "OAuth"} {
		if got := titleProvider(provider); got != want {
			t.Fatalf("titleProvider(%q)=%q want %q", provider, got, want)
		}
	}
	for token, want := range map[string]string{"gemini": "Gemini", "shared": "Claude & GPT", "argon": "Gemini Argon", "carbon": "Gemini Carbon", "hydrogen": "Gemini Hydrogen"} {
		got, _, ok := familyName(token)
		if !ok || got != want {
			t.Fatalf("familyName(%q)=%q", token, got)
		}
		// Coalesced Pushover bodies bound the provider label to 32 runes.
		if label := "Antigravity (" + got + ")"; len([]rune(label)) > 32 {
			t.Fatalf("label %q exceeds the coalesced provider bound", label)
		}
	}
	reset := time.Date(2026, time.November, 22, 12, 0, 0, 0, time.FixedZone("x", 3600))
	if got := quotaWindowKey("", reset); got != "2026-11-22T11:00:00Z" {
		t.Fatalf("single-window key changed: %q", got)
	}
	if got := quotaWindowKey("", time.Time{}); got != "" {
		t.Fatalf("unknown-reset key changed: %q", got)
	}
	if got := quotaWindowKey("shared", reset); got != "shared@2026-11-22T11:00:00Z" {
		t.Fatalf("family key=%q", got)
	}
	if got := quotaWindowKey("shared", time.Time{}); got != "shared@" {
		t.Fatalf("family unknown-reset key=%q", got)
	}
}
