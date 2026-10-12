package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/settings"
)

// The route fixture's ids: a Claude credential whose renewal date may be set,
// and an orphan of each kind already in the settings file.
const (
	claudeHexID = "0123456789abcdef"
	claudeEmail = "user@example.com"
	// claudeFileID is a credential's index as an older CPA formed it: a
	// file name, which cannot key a renewal date.
	claudeFileID  = "claude-two@example.com.json"
	orphanAccount = "org-3f2a9c1d0b7e"
	orphanRenewal = "fedcba9876543210"
)

var alphaAccount = client.APICreditOrgAccount(fakeOrgA)

// routeHost lists one Claude credential, disabled so that it is never polled,
// and keeps every log line.
type routeHost struct {
	*meterHost
	roster []protocol.HostAuthFileEntry
}

func (h routeHost) ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error) {
	return h.roster, nil
}

// alphaCredit is claude-api-credits as quota-cache 0.1.x read it, listing
// alpha with a credit and a refill date. This quota-cache ignores it.
const alphaCredit = "claude-api-credits:\n  - label: alpha\n    organization-id: " + fakeOrgA + "\n    monthly-usd: \"200\"\n    renews: \"2026-10-05\"\n"

// newRoutePlugin is a running plugin whose page can save: the credential
// above on the roster, alpha on the page, the settings file holding an orphan
// of each kind at revision 2, and a published state. setup runs before the
// first configure.
func newRoutePlugin(t *testing.T, setup ...func(p *meterPlugin)) *meterPlugin {
	t.Helper()
	return newRoutePluginListing(t, routeRoster(), setup...)
}

// routeRoster is the route fixture's roster: the two Claude credentials
// above, disabled so that neither is ever polled.
func routeRoster() []protocol.HostAuthFileEntry {
	return []protocol.HostAuthFileEntry{
		{AuthIndex: claudeHexID, Provider: "claude", Email: claudeEmail, Disabled: true},
		{AuthIndex: claudeFileID, Type: "Claude", Disabled: true},
	}
}

// newRoutePluginListing is newRoutePlugin with roster as what CPA lists.
func newRoutePluginListing(t *testing.T, roster []protocol.HostAuthFileEntry, setup ...func(p *meterPlugin)) *meterPlugin {
	t.Helper()
	host := &meterHost{}
	p := &meterPlugin{Plugin: New(routeHost{meterHost: host, roster: roster}), t: t, host: host, path: filepath.Join(t.TempDir(), "cache", "snapshot.json")}
	t.Cleanup(p.Shutdown)
	p.writeSettings(map[string]any{
		"schema": 1, "revision": 2,
		"apiCredits": map[string]any{orphanAccount: map[string]any{"monthlyUsd": "10", "rev": 1, "updatedAt": "2026-10-01T00:00:00Z"}},
		"renewals":   map[string]any{orphanRenewal: map[string]any{"date": "2026-10-29", "rev": 2, "updatedAt": "2026-10-02T00:00:00Z"}},
	})
	p.writeOrganizations([2]string{"alpha", fakeOrgA})
	for _, f := range setup {
		f(p)
	}
	p.configure(protocol.MethodPluginRegister, "")
	p.published()
	return p
}

// writeOrganizations writes the organizations file beside the snapshot, as
// the page saves it: each organization a label and an Organization ID, at
// revision 1.
func (p *meterPlugin) writeOrganizations(organizations ...[2]string) {
	p.t.Helper()
	items := []any{}
	for _, o := range organizations {
		items = append(items, map[string]any{"label": o[0], "organization_id": o[1], "rev": "1", "updated_at": "2026-10-01T00:00:00Z"})
	}
	raw, _ := json.Marshal(map[string]any{"schema": 1, "revision": "1", "organizations": items})
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		p.t.Fatal(err)
	}
	writeFile(p.t, client.OrganizationsPath(p.path), raw)
}

// writeSettings writes the settings file beside the snapshot.
func (p *meterPlugin) writeSettings(doc map[string]any) {
	p.t.Helper()
	raw, _ := json.Marshal(doc)
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(settings.FilePath(p.path), raw, 0o600); err != nil {
		p.t.Fatal(err)
	}
}

// published waits for the roster and the snapshot, then publishes, and
// returns the credit state file.
func (p *meterPlugin) published() *client.CreditState {
	p.t.Helper()
	waitFor(p.t, p.path, func(s client.Snapshot) bool { return !s.WrittenAt.IsZero() })
	deadline := time.Now().Add(10 * time.Second)
	for p.roster.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	p.mu.Lock()
	publisher := p.publisher
	p.mu.Unlock()
	publisher.PublishNow()
	return p.creditsFile()
}

func (p *meterPlugin) creditsFile() *client.CreditState {
	p.t.Helper()
	state, err := client.LoadCredits(client.CreditsPath(p.path))
	if err != nil {
		p.t.Fatalf("credits file: %v", err)
	}
	return state
}

// post sends body to the save route as the page does: JSON, same-origin.
func (p *meterPlugin) post(body any) (int, map[string]any, protocol.ManagementResponse) {
	p.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		p.t.Fatal(err)
	}
	return p.postRaw(raw, http.Header{"Content-Type": {"application/json"}, "Sec-Fetch-Site": {"same-origin"}})
}

func (p *meterPlugin) postRaw(raw []byte, headers http.Header) (int, map[string]any, protocol.ManagementResponse) {
	p.t.Helper()
	request, _ := json.Marshal(protocol.ManagementRequest{Method: "POST", Path: settingsPath, Headers: headers, Body: raw})
	result, err := p.Handle(protocol.MethodManagementHandle, request)
	if err != nil {
		p.t.Fatal(err)
	}
	res := result.(protocol.ManagementResponse)
	if res.Headers.Get("Content-Type") != "application/json" || res.Headers.Get("Cache-Control") != "no-store" {
		p.t.Fatalf("headers %v", res.Headers)
	}
	body := map[string]any{}
	if err := json.Unmarshal(res.Body, &body); err != nil {
		p.t.Fatalf("body %s: %v", res.Body, err)
	}
	return res.StatusCode, body, res
}

// wantError requires one refusal and returns its body.
func (p *meterPlugin) wantError(name string, status int, code string, body any) map[string]any {
	p.t.Helper()
	got, answer, _ := p.post(body)
	if got != status || answer["error"] != code {
		p.t.Fatalf("%s: %d %v, want %d %s", name, got, answer, status, code)
	}
	return answer
}

// revision is the settings file's revision now.
func (p *meterPlugin) revision() uint64 {
	p.t.Helper()
	p.mu.Lock()
	store := p.store
	p.mu.Unlock()
	return store.Current().Revision
}

// organizationsRevision is the organizations file's revision as the page
// reads it.
func (p *meterPlugin) organizationsRevision() string {
	p.t.Helper()
	p.mu.Lock()
	store := p.organizations
	p.mu.Unlock()
	if store == nil {
		p.t.Fatal("the cache is not running")
	}
	return settings.RevisionText(store.Current().Revision)
}

// organizationsAfter is the organizations revision n batches after base.
func organizationsAfter(t *testing.T, base string, n uint64) string {
	t.Helper()
	revision, err := strconv.ParseUint(base, 10, 64)
	if err != nil {
		t.Fatalf("revision %q: %v", base, err)
	}
	return settings.RevisionText(revision + n)
}

// organizationOps sends one organizations batch from the current revision and
// requires it to be accepted.
func (p *meterPlugin) organizationOps(ops ...map[string]any) map[string]any {
	p.t.Helper()
	code, body, _ := p.post(map[string]any{"kind": settings.KindOrganizations, "baseRevision": p.organizationsRevision(), "ops": ops})
	if code != http.StatusOK {
		p.t.Fatalf("organizations batch %v: %d %v", ops, code, body)
	}
	return body
}

func addOp(label, organizationID string) map[string]any {
	return map[string]any{"op": settings.OpAdd, "label": label, "organizationId": organizationID}
}

func removeOp(organizationID string) map[string]any {
	return map[string]any{"op": settings.OpRemove, "id": client.APICreditOrgAccount(organizationID)}
}

// removeAll removes every organization on the page, which leaves nothing for
// the meter to count.
func (p *meterPlugin) removeAll() {
	p.t.Helper()
	p.mu.Lock()
	current := p.organizations.Current()
	p.mu.Unlock()
	ops := []map[string]any{}
	for _, item := range current.Items {
		ops = append(ops, removeOp(item.OrganizationID))
	}
	if len(ops) == 0 {
		p.t.Fatal("no organization to remove")
	}
	p.organizationOps(ops...)
}

// breakOrganizations makes the organizations file one its load rules refuse,
// so no organization is listed and none can be saved until it is fixed.
func (p *meterPlugin) breakOrganizations() {
	p.t.Helper()
	writeFile(p.t, client.OrganizationsPath(p.path), []byte(`{`))
}

func batchOf(kind string, items ...map[string]any) map[string]any {
	list := make([]any, len(items))
	for i, item := range items {
		list[i] = item
	}
	return map[string]any{"kind": kind, "items": list}
}

func creditRow(id, base string, monthly, renews, reading any) map[string]any {
	return map[string]any{"id": id, "baseRevision": base, "monthlyUsd": monthly, "renews": renews, "reading": reading}
}

func renewalRow(id, base string, date any) map[string]any {
	return map[string]any{"id": id, "baseRevision": base, "date": date}
}

func readingOf(amount string, at time.Time) map[string]any {
	return map[string]any{"remainingUsd": amount, "at": at.UTC().Format(time.RFC3339)}
}

// accountIn is the account id in state.
func accountIn(t *testing.T, state *client.CreditState, id string) client.CreditAccount {
	t.Helper()
	if state == nil || state.APICredits == nil {
		t.Fatalf("no api_credits in %+v", state)
	}
	for _, a := range state.APICredits.Accounts {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("no account %s", id)
	return client.CreditAccount{}
}

// noValueLogged fails when a log line carries one of values.
func (p *meterPlugin) noValueLogged(values ...string) {
	p.t.Helper()
	text := fmt.Sprintf("%+v", p.host.logged())
	for _, value := range values {
		if strings.Contains(text, value) {
			p.t.Fatalf("the log carries %q: %s", value, text)
		}
	}
}

// A save is committed, published before the answer, logged with what changed
// and never a value, and answered with the rows' newly published blocks.
// Resent, it is a no-op.
func TestASaveIsCommittedPublishedAndLogged(t *testing.T) {
	p := newRoutePlugin(t)
	code, body, _ := p.post(batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", "300.25", nil, nil)))
	block, _ := body["settings"].(map[string]any)[alphaAccount].(map[string]any)
	// Past the file's revision 2, at the time.
	saved := strconv.FormatUint(p.revision(), 10)
	if code != http.StatusOK || body["ok"] != true || body["unchanged"] != false || body["revision"] != saved || p.revision() <= 2 ||
		block["monthlyUsd"] != "300.25" || block["configMonthlyUsd"] != "" || block["revision"] != saved || block["editable"] != true {
		t.Fatalf("%d %v", code, body)
	}
	// Published before the route answered: the file already shows it.
	if account := accountIn(t, p.creditsFile(), alphaAccount); account.Settings.MonthlyUSD != "300.25" || account.MonthlyCreditSource != client.CreditSourceDashboard {
		t.Fatalf("published %+v", account)
	}
	code, body, _ = p.post(batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", "300.25", nil, nil)))
	if code != http.StatusOK || body["unchanged"] != true || body["revision"] != saved {
		t.Fatalf("a resent batch: %d %v", code, body)
	}
	code, body, _ = p.post(batchOf(settings.KindRenewals, renewalRow(claudeHexID, "", "2026-09-17")))
	block, _ = body["settings"].(map[string]any)[claudeHexID].(map[string]any)
	renewed := strconv.FormatUint(p.revision(), 10)
	if code != http.StatusOK || block["date"] != "2026-09-17" || block["revision"] != renewed || renewed == saved {
		t.Fatalf("renewal: %d %v", code, body)
	}
	if renewal := p.creditsFile().Renewals[claudeHexID]; renewal.Date != "2026-09-17" || renewal.Revision != renewed {
		t.Fatalf("published renewal %+v", renewal)
	}
	// Use estimate.
	code, body, _ = p.post(batchOf(settings.KindRenewals, renewalRow(claudeHexID, renewed, nil)))
	if saved := body["settings"].(map[string]any); code != http.StatusOK || saved[claudeHexID] != nil {
		t.Fatalf("use estimate: %d %v", code, body)
	}
	changed := []logLine{}
	for _, line := range p.host.logged() {
		if line.message == "quota-cache settings changed" {
			changed = append(changed, line)
		}
	}
	if len(changed) != 3 || changed[0].level != "info" || changed[0].fields["kind"] != settings.KindAPICredits ||
		!reflect.DeepEqual(changed[0].fields["ids"], []string{alphaAccount}) || !reflect.DeepEqual(changed[0].fields["fields"], []string{"monthlyUsd"}) {
		t.Fatalf("logged %+v", changed)
	}
	p.noValueLogged("300.25", "2026-09-17", claudeEmail)
}

// The checks run in their fixed order: a refusal names the first rule the
// request breaks, and nothing refused reaches the store.
func TestTheOrderOfChecks(t *testing.T) {
	good, _ := json.Marshal(batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", "300", nil, nil)))
	big := []byte(`{"kind":"apiCredits","items":[` + strings.Repeat(" ", maxSettingsBytes) + `]}`)
	page := http.Header{"Content-Type": {"application/json"}, "Sec-Fetch-Site": {"same-origin"}}
	stopped := newMeterPlugin(t)
	stopped.configure(protocol.MethodPluginRegister, "enabled: false\n")
	for name, headers := range map[string]http.Header{"from the page": page, "cross-site and not JSON": {"Sec-Fetch-Site": {"cross-site"}, "Content-Type": {"text/plain"}}} {
		if code, body, _ := stopped.postRaw(good, headers); code != http.StatusServiceUnavailable || body["error"] != "cache_stopped" {
			t.Fatalf("stopped, %s: %d %v", name, code, body)
		}
	}

	p := newRoutePlugin(t)
	for _, tc := range []struct {
		name    string
		raw     []byte
		headers http.Header
		status  int
		code    string
	}{
		{"cross-site before the media type", big, http.Header{"Sec-Fetch-Site": {"cross-site"}, "Content-Type": {"text/plain"}}, http.StatusForbidden, "cross_site"},
		{"same-site", good, http.Header{"Sec-Fetch-Site": {"same-site"}, "Content-Type": {"application/json"}}, http.StatusForbidden, "cross_site"},
		{"one value of two cross-site", good, http.Header{"Sec-Fetch-Site": {"same-origin", "cross-site"}, "Content-Type": {"application/json"}}, http.StatusForbidden, "cross_site"},
		{"a form before the size", big, http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"text", good, http.Header{"Content-Type": {"text/plain"}}, http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"multipart", good, http.Header{"Content-Type": {"multipart/form-data; boundary=x"}}, http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"no media type", good, http.Header{}, http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"over 4096 bytes", big, page, http.StatusBadRequest, "invalid_request"},
	} {
		if code, body, _ := p.postRaw(tc.raw, tc.headers); code != tc.status || body["error"] != tc.code {
			t.Errorf("%s: %d %v, want %d %s", tc.name, code, body, tc.status, tc.code)
		}
	}
	if p.revision() != 2 {
		t.Fatal("a refused request reached the store")
	}
	// No Sec-Fetch-Site at all, as from curl, and a media type with a
	// parameter, pass.
	if code, body, _ := p.postRaw(good, http.Header{"Content-Type": {"Application/JSON; charset=utf-8"}}); code != http.StatusOK {
		t.Fatalf("curl: %d %v", code, body)
	}

	// A settings file that cannot be read refuses every settings kind after
	// the size check, and is never written over; the organizations card
	// still saves, since it needs only its own file.
	unreadable := newRoutePlugin(t, func(p *meterPlugin) {
		if err := os.WriteFile(settings.FilePath(p.path), []byte(`{"schema":2}`), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if code, body, _ := unreadable.postRaw(big, page); code != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("size before state: %d %v", code, body)
	}
	for name, body := range map[string]any{
		"a batch":   batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", "300", nil, nil)),
		"malformed": map[string]any{"kind": "budgets"},
		"no kind":   []any{1},
	} {
		unreadable.wantError(name, http.StatusServiceUnavailable, "settings_unavailable", body)
	}
	if raw, _ := os.ReadFile(settings.FilePath(unreadable.path)); string(raw) != `{"schema":2}` {
		t.Fatal("an unreadable settings file was written over")
	}
	unreadable.organizationOps(addOp("bravo", fakeOrgB))
}

// Everything that is not exactly a batch is invalid_request.
func TestMalformedBatchesAreRefused(t *testing.T) {
	p := newRoutePlugin(t)
	read := p.organizationsRevision()
	row := creditRow(alphaAccount, "", "300", nil, nil)
	missing := creditRow(alphaAccount, "", "300", nil, nil)
	delete(missing, "reading")
	unknown := creditRow(alphaAccount, "", "300", nil, nil)
	unknown["note"] = "x"
	cases := map[string]any{
		"null":             nil,
		"an array":         []any{row},
		"unknown kind":     batchOf("budgets", row),
		"no items":         batchOf(settings.KindAPICredits),
		"a missing key":    batchOf(settings.KindAPICredits, missing),
		"an unknown key":   batchOf(settings.KindAPICredits, unknown),
		"a repeated id":    batchOf(settings.KindAPICredits, row, row),
		"a bad revision":   batchOf(settings.KindAPICredits, creditRow(alphaAccount, "01", "300", nil, nil)),
		"importLegacy":     map[string]any{"kind": "importLegacy", "baseRevision": "2"},
		"an op with no id": map[string]any{"kind": settings.KindOrganizations, "baseRevision": read, "ops": []any{map[string]any{"op": "remove"}}},
		"no op":            map[string]any{"kind": settings.KindOrganizations, "baseRevision": read, "ops": []any{}},
	}
	seventeen := []map[string]any{}
	for i := range 17 {
		seventeen = append(seventeen, creditRow(fmt.Sprintf("org-%012x", i), "", nil, nil, nil))
	}
	cases["seventeen rows"] = batchOf(settings.KindAPICredits, seventeen...)
	for name, body := range cases {
		p.wantError(name, http.StatusBadRequest, "invalid_request", body)
	}
	if p.revision() != 2 || p.organizationsRevision() != read {
		t.Fatal("a malformed batch reached a store")
	}
}

// Each value rule answers with its code, the row and the field.
func TestEachValueRuleNamesItsField(t *testing.T) {
	p := newRoutePlugin(t)
	now := time.Now().UTC()
	today := client.MeterDayStart(now)
	for name, tc := range map[string]struct {
		body        any
		code, field string
		id          string
	}{
		"monthly":       {batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", "200.123", nil, nil)), "invalid_monthly_usd", "monthlyUsd", alphaAccount},
		"renews":        {batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", nil, "2026-11-31", nil)), "invalid_renews", "renews", alphaAccount},
		"amount":        {batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", nil, nil, readingOf("1e3", now))), "invalid_reading_amount", "reading.remainingUsd", alphaAccount},
		"too old":       {batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", nil, nil, readingOf("100.00", now.Add(-48*time.Hour-time.Minute)))), "invalid_reading_time", "reading.at", alphaAccount},
		"ahead":         {batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", nil, nil, readingOf("100.00", now.Add(10*time.Minute)))), "invalid_reading_time", "reading.at", alphaAccount},
		"before refill": {batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", nil, today.Format(time.DateOnly), readingOf("100.00", today.Add(-time.Hour)))), "reading_before_refill", "reading.at", alphaAccount},
		"date":          {batchOf(settings.KindRenewals, renewalRow(claudeHexID, "", "2026-02-30")), "invalid_date", "date", claudeHexID},
	} {
		body := p.wantError(name, http.StatusBadRequest, tc.code, tc.body)
		if body["id"] != tc.id || body["field"] != tc.field {
			t.Errorf("%s: %v", name, body)
		}
	}
	// A value rule is judged before whether the row may be edited at all.
	if body := p.wantError("both", http.StatusBadRequest, "invalid_monthly_usd", batchOf(settings.KindAPICredits, creditRow("org-000000000000", "", "x", nil, nil))); body["id"] != "org-000000000000" {
		t.Errorf("%v", body)
	}
	if p.revision() != 2 {
		t.Fatal("an invalid value reached the store")
	}
}

// A new reading is checked against the refill date its row keeps, and only
// that one: a row that keeps today's refill date refuses a reading from before
// it, and a row that keeps none takes it, though the configuration lists
// today's for the organization.
func TestAReadingIsCheckedAgainstTheRowsRefillDate(t *testing.T) {
	now := time.Now().UTC()
	today := client.MeterDayStart(now)
	p := newRoutePlugin(t)
	p.configure(protocol.MethodPluginReconfigure, "claude-api-credits:\n  - label: alpha\n    organization-id: "+fakeOrgA+"\n    renews: \""+today.Format(time.DateOnly)+"\"\n")
	reading := readingOf("100.00", today.Add(-time.Hour))
	body := p.wantError("before the row's refill", http.StatusBadRequest, "reading_before_refill",
		batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", nil, today.Format(time.DateOnly), reading)))
	if body["id"] != alphaAccount || body["field"] != "reading.at" || p.revision() != 2 {
		t.Fatalf("%v", body)
	}
	if code, body, _ := p.post(batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", nil, nil, reading))); code != http.StatusOK {
		t.Fatalf("no refill date kept: %d %v", code, body)
	}
}

// Only rows the published state offers may be saved, all of them or none;
// an orphan of either kind may only be cleared.
func TestOnlyWhatTheStateOffersIsSaved(t *testing.T) {
	p := newRoutePlugin(t)
	for name, tc := range map[string]struct {
		body any
		ids  []any
	}{
		"an unknown account":       {batchOf(settings.KindAPICredits, creditRow("org-000000000000", "", "300", nil, nil)), []any{"org-000000000000"}},
		"one bad row of two":       {batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", "300", nil, nil), creditRow("org-000000000000", "", "1", nil, nil)), []any{"org-000000000000"}},
		"a credential not listed":  {batchOf(settings.KindRenewals, renewalRow("00000000000000aa", "", "2026-10-29")), []any{"00000000000000aa"}},
		"an index that is no key":  {batchOf(settings.KindRenewals, renewalRow(claudeFileID, "", "2026-10-29")), []any{claudeFileID}},
		"setting an orphan":        {batchOf(settings.KindAPICredits, creditRow(orphanAccount, "1", "20", nil, nil)), []any{orphanAccount}},
		"setting a renewal orphan": {batchOf(settings.KindRenewals, renewalRow(orphanRenewal, "2", "2026-11-29")), []any{orphanRenewal}},
	} {
		body := p.wantError(name, http.StatusConflict, "not_editable", tc.body)
		if !reflect.DeepEqual(body["ids"], tc.ids) || body["id"] != tc.ids[0] {
			t.Errorf("%s: %v", name, body)
		}
	}
	if p.revision() != 2 {
		t.Fatal("a row the state does not offer reached the store")
	}
	// Clearing either orphan is allowed, and removes it.
	if code, body, _ := p.post(batchOf(settings.KindAPICredits, creditRow(orphanAccount, "1", nil, nil, nil))); code != http.StatusOK || body["settings"].(map[string]any)[orphanAccount] != nil {
		t.Fatalf("clearing an orphan: %d %v", code, body)
	}
	if code, body, _ := p.post(batchOf(settings.KindRenewals, renewalRow(orphanRenewal, "2", nil))); code != http.StatusOK {
		t.Fatalf("clearing a renewal orphan: %d %v", code, body)
	}
	if state := p.creditsFile(); len(state.APICredits.Orphans) != 0 || len(state.Renewals) != 0 {
		t.Fatalf("orphans left: %+v %+v", state.APICredits.Orphans, state.Renewals)
	}
}

// A row another device saved since the page read it is a conflict, with what
// is there now; sending what is already there is not.
func TestConflictsCarryTheCurrentValues(t *testing.T) {
	p := newRoutePlugin(t)
	if code, body, _ := p.post(batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", "300", nil, nil))); code != http.StatusOK {
		t.Fatal(body)
	}
	saved := strconv.FormatUint(p.revision(), 10)
	body := p.wantError("credit", http.StatusConflict, "conflict", batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", "250", nil, nil)))
	block, _ := body["current"].(map[string]any)[alphaAccount].(map[string]any)
	if body["revision"] != saved || !reflect.DeepEqual(body["conflicts"], []any{alphaAccount}) || block["monthlyUsd"] != "300" || block["revision"] != saved || block["configMonthlyUsd"] != "" {
		t.Fatalf("%v", body)
	}
	if code, body, _ := p.post(batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", "300", nil, nil))); code != http.StatusOK || body["unchanged"] != true {
		t.Fatalf("an identical replay: %d %v", code, body)
	}
	if code, body, _ := p.post(batchOf(settings.KindRenewals, renewalRow(claudeHexID, "", "2026-10-29"))); code != http.StatusOK {
		t.Fatal(body)
	}
	renewed := p.revision()
	body = p.wantError("renewal", http.StatusConflict, "conflict", batchOf(settings.KindRenewals, renewalRow(claudeHexID, "", "2026-11-01")))
	if block, _ := body["current"].(map[string]any)[claudeHexID].(map[string]any); block["date"] != "2026-10-29" || block["revision"] != strconv.FormatUint(renewed, 10) {
		t.Fatalf("renewal conflict: %v", body)
	}
	if p.revision() != renewed {
		t.Fatal("a conflict wrote")
	}
}

// A new reading stores the account's organization and a baseline taken from
// the meter file the publisher last read.
func TestAReadingTakesItsBaselineFromThePublishedMeter(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	day := client.MeterDayStart(now)
	p := newRoutePlugin(t, func(p *meterPlugin) {
		usage := []client.MeterUsage{{Model: "claude-sonnet-5-5", Requests: 1, Input: 1_000_000}}
		meter := client.APIMeter{Schema: client.MeterSchema, Since: day.Add(-24 * time.Hour), StartedAt: day.Add(-24 * time.Hour), FlushedAt: now,
			Gaps: []client.MeterGap{}, Unlinked: []client.MeterUnlinked{}, Auths: map[string]client.MeterAuth{}, AuthIDs: map[string]client.MeterAuthID{},
			Organizations: map[string]client.MeterOrganization{fakeOrgA: {Since: day.Add(-24 * time.Hour),
				Days: []client.MeterBucket{{Start: day, Usage: usage}}, Hours: []client.MeterBucket{{Start: day, Usage: usage}}}}}
		raw, _ := json.Marshal(meter)
		if err := os.WriteFile(client.MeterPath(p.path), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	})
	// A minute back, but never before today's start: on the previous day the
	// baseline would start a day earlier.
	at := now.Add(-time.Minute)
	if at.Before(day) {
		at = day
	}
	if code, body, _ := p.post(batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", nil, nil, readingOf("150.00", at)))); code != http.StatusOK {
		t.Fatalf("%d %v", code, body)
	}
	stored := p.store.Current().APICredits[alphaAccount].Reading
	want := 0
	if client.MeterHourStart(at).After(day) {
		// The reading's day has an hour before the reading's own.
		want = 1
	}
	if stored == nil || stored.OrganizationID != fakeOrgA || !stored.Baseline.DayStart.Equal(day) || len(stored.Baseline.Usage) != want {
		t.Fatalf("stored %+v", stored)
	}
	if account := accountIn(t, p.creditsFile(), alphaAccount); account.Settings.Reading == nil || account.Settings.Reading.RemainingUSD != "150.00" {
		t.Fatalf("published %+v", account.Settings)
	}
}

// A reading saved days ago is resent unchanged beside a new credit, and the
// save goes through: the clock judges only a new reading.
func TestAnUnchangedOldReadingResentWithANewCreditSaves(t *testing.T) {
	at := time.Now().UTC().Add(-5 * 24 * time.Hour).Truncate(time.Hour).Add(20 * time.Minute)
	p := newRoutePlugin(t, func(p *meterPlugin) {
		p.writeSettings(map[string]any{"schema": 1, "revision": 3, "apiCredits": map[string]any{alphaAccount: map[string]any{
			"reading": map[string]any{
				"remainingUsd": "150.00", "at": at.Format(time.RFC3339), "enteredAt": at.Add(time.Minute).Format(time.RFC3339), "organizationId": fakeOrgA,
				"baseline": map[string]any{"dayStart": client.MeterDayStart(at).Format(time.RFC3339), "until": client.MeterHourStart(at).Format(time.RFC3339), "usage": []any{}},
			},
			"rev": 3, "updatedAt": at.Add(time.Minute).Format(time.RFC3339),
		}}})
	})
	stored := p.store.Current().APICredits[alphaAccount].Reading
	if code, body, _ := p.post(batchOf(settings.KindAPICredits, creditRow(alphaAccount, "3", "300", nil, readingOf("150.00", at)))); code != http.StatusOK {
		t.Fatalf("%d %v", code, body)
	}
	if again := p.store.Current().APICredits[alphaAccount]; again.Reading != stored || again.MonthlyUSD != "300" {
		t.Fatalf("stored %+v", again)
	}
}

// Thirty committed saves a minute, then 429.
func TestCommittedSavesAreThrottled(t *testing.T) {
	p := newRoutePlugin(t)
	base := ""
	for i := range 30 {
		code, body, _ := p.post(batchOf(settings.KindRenewals, renewalRow(claudeHexID, base, fmt.Sprintf("2026-10-%02d", i%28+1))))
		if code != http.StatusOK {
			t.Fatalf("save %d: %d %v", i, code, body)
		}
		base = body["revision"].(string)
	}
	_, _, res := p.post(batchOf(settings.KindRenewals, renewalRow(claudeHexID, base, "2026-11-01")))
	if res.StatusCode != http.StatusTooManyRequests || res.Headers.Get("Retry-After") != "60" || !strings.Contains(string(res.Body), `"too_many_writes"`) {
		t.Fatalf("past the limit: %d %v %s", res.StatusCode, res.Headers, res.Body)
	}
}

// The store's own failures each have their answer. settings_full is also
// reached for real: 64 stored credits, and one more.
func TestStoreFailuresAreAnswered(t *testing.T) {
	for err, want := range map[error]struct {
		status int
		code   string
	}{
		settings.ErrClosed:        {http.StatusServiceUnavailable, "cache_stopped"},
		settings.ErrUnavailable:   {http.StatusServiceUnavailable, "settings_unavailable"},
		settings.ErrTooManyWrites: {http.StatusTooManyRequests, "too_many_writes"},
		settings.ErrFull:          {http.StatusConflict, "settings_full"},
		settings.ErrUnwritable:    {http.StatusServiceUnavailable, "settings_unwritable"},
		errors.New("anything"):    {http.StatusServiceUnavailable, "settings_unwritable"},
	} {
		res := storeRefusal(err)
		body := map[string]string{}
		_ = json.Unmarshal(res.Body, &body)
		if res.StatusCode != want.status || body["error"] != want.code || res.Headers.Get("Cache-Control") != "no-store" {
			t.Errorf("%v: %d %s", err, res.StatusCode, res.Body)
		}
	}
	p := newRoutePlugin(t, func(p *meterPlugin) {
		credits := map[string]any{}
		for i := range settings.MaxAPICredits {
			credits[fmt.Sprintf("org-%012x", i)] = map[string]any{"monthlyUsd": "1", "rev": 1, "updatedAt": "2026-10-01T00:00:00Z"}
		}
		p.writeSettings(map[string]any{"schema": 1, "revision": 1, "apiCredits": credits})
	})
	p.wantError("full", http.StatusConflict, "settings_full", batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", "300", nil, nil)))
}

// A save tells the store every Claude credential CPA lists, so a settings
// file at its bound of renewal dates makes room by dropping the oldest date of
// a credential CPA no longer lists, never one it lists, however old. When
// every date stored belongs to a listed credential, the save is refused.
func TestAFullSettingsFileKeepsTheDatesOfListedCredentials(t *testing.T) {
	credential := func(i int) string { return fmt.Sprintf("00000000000a%04x", i) }
	listed := func(n int) []protocol.HostAuthFileEntry {
		roster := routeRoster()
		for i := range n {
			roster = append(roster, protocol.HostAuthFileEntry{AuthIndex: credential(i), Provider: "claude", Disabled: true})
		}
		return roster
	}
	date := func(updatedAt string) map[string]any {
		return map[string]any{"date": "2026-10-29", "rev": 1, "updatedAt": updatedAt}
	}
	renewalsOf := func(p *meterPlugin) map[string]settings.Renewal {
		p.mu.Lock()
		store := p.store
		p.mu.Unlock()
		return store.Current().Renewals
	}

	// The oldest date is a listed credential's; every other is an orphan's.
	orphan := func(i int) string { return fmt.Sprintf("00000000000b%04x", i) }
	p := newRoutePluginListing(t, listed(2), func(p *meterPlugin) {
		renewals := map[string]any{credential(0): date("2026-01-01T00:00:00Z")}
		for i := range settings.MaxRenewals - 1 {
			renewals[orphan(i)] = date(time.Date(2026, time.February, 1, 0, 0, i, 0, time.UTC).Format(time.RFC3339))
		}
		p.writeSettings(map[string]any{"schema": 1, "revision": 1, "renewals": renewals})
	})
	if code, body, _ := p.post(batchOf(settings.KindRenewals, renewalRow(credential(1), "", "2026-10-29"))); code != http.StatusOK {
		t.Fatalf("a date for a new credential: %d %v", code, body)
	}
	renewals := renewalsOf(p)
	_, listedKept := renewals[credential(0)]
	_, orphanKept := renewals[orphan(0)]
	if !listedKept || orphanKept || len(renewals) != settings.MaxRenewals {
		t.Fatalf("listed date kept %v, oldest orphan's kept %v, %d dates", listedKept, orphanKept, len(renewals))
	}

	// Every date stored is a listed credential's: nothing can go.
	full := newRoutePluginListing(t, listed(settings.MaxRenewals+1), func(p *meterPlugin) {
		renewals := map[string]any{}
		for i := range settings.MaxRenewals {
			renewals[credential(i)] = date(time.Date(2026, time.February, 1, 0, 0, i, 0, time.UTC).Format(time.RFC3339))
		}
		p.writeSettings(map[string]any{"schema": 1, "revision": 1, "renewals": renewals})
	})
	full.wantError("full of listed dates", http.StatusConflict, "settings_full", batchOf(settings.KindRenewals, renewalRow(credential(settings.MaxRenewals), "", "2026-10-29")))
	if renewals := renewalsOf(full); len(renewals) != settings.MaxRenewals || full.revision() != 1 {
		t.Fatalf("a refused save changed the file: %d dates at revision %d", len(renewals), full.revision())
	}
}

// The organizations card's batch: add, rename and remove, all or nothing,
// against the file's revision, published before the answer, and logged by
// count only.
func TestOrganizationsBatchThroughTheRoute(t *testing.T) {
	p := newRoutePlugin(t)
	read := p.organizationsRevision()
	saved := organizationsAfter(t, read, 1)
	body := p.organizationOps(addOp(" bravo ", strings.ToUpper(fakeOrgB)), map[string]any{"op": settings.OpRename, "id": alphaAccount, "label": "alpha-one"})
	items, _ := body["organizations"].([]any)
	if body["revision"] != saved || body["unchanged"] != false || len(items) != 2 {
		t.Fatalf("%v", body)
	}
	if second := items[1].(map[string]any); second["label"] != "bravo" || second["organization_id"] != fakeOrgB || second["id"] != client.APICreditOrgAccount(fakeOrgB) ||
		!slices.Equal(slices.Sorted(maps.Keys(second)), []string{"id", "label", "organization_id"}) {
		t.Fatalf("%v", items)
	}
	// Published before the answer: both are accounts already.
	state := p.creditsFile()
	if accountIn(t, state, alphaAccount).Label != "alpha-one" || accountIn(t, state, client.APICreditOrgAccount(fakeOrgB)).Label != "bravo" || state.Organizations.Revision != saved {
		t.Fatalf("published %+v", state.Organizations)
	}
	if body := p.wantError("stale", http.StatusConflict, "conflict", map[string]any{"kind": settings.KindOrganizations, "baseRevision": read, "ops": []any{removeOp(fakeOrgB)}}); body["revision"] != saved || len(body["organizations"].([]any)) != 2 {
		t.Fatalf("%v", body)
	}
	for name, tc := range map[string]struct {
		op          map[string]any
		code, field string
	}{
		"a duplicate label": {addOp("ALPHA-ONE", fakeOrgC), settings.CodeDuplicateLabel, "label"},
		"an invalid id":     {addOp("charlie", "nope"), settings.CodeInvalidOrganizationID, "organizationId"},
		"a duplicate org":   {addOp("charlie", fakeOrgB), settings.CodeDuplicateOrganization, "organizationId"},
		"an unknown id":     {removeOp(fakeOrgC), settings.CodeUnknownID, "id"},
		"an invalid label":  {addOp("a\tb", fakeOrgC), settings.CodeInvalidLabel, "label"},
	} {
		// The first op is fine; the batch is refused whole, naming the second.
		body := p.wantError(name, http.StatusBadRequest, tc.code, map[string]any{"kind": settings.KindOrganizations, "baseRevision": saved, "ops": []any{addOp("delta", "00000000-0000-4000-8000-00000000000d"), tc.op}})
		if body["op"] != 1.0 || body["field"] != tc.field {
			t.Errorf("%s: %v", name, body)
		}
	}
	if p.organizationsRevision() != saved {
		t.Fatal("a refused batch wrote")
	}
	if code, body, _ := p.post(map[string]any{"kind": settings.KindOrganizations, "baseRevision": saved, "ops": []any{map[string]any{"op": settings.OpRename, "id": alphaAccount, "label": "alpha-one"}}}); code != http.StatusOK || body["unchanged"] != true || body["revision"] != saved {
		t.Fatalf("a no-op: %d %v", code, body)
	}
	changed := 0
	for _, line := range p.host.logged() {
		if line.message == "quota-cache organizations changed" {
			changed++
			if line.fields["added"] != 1 || line.fields["renamed"] != 1 || line.fields["removed"] != 0 {
				t.Fatalf("logged %+v", line)
			}
		}
	}
	if changed != 1 {
		t.Fatalf("logged %d changes", changed)
	}
	p.noValueLogged("bravo", "alpha-one", fakeOrgB, fakeOrgA)

	// An organizations file that cannot be read refuses the card's batch, and
	// no organization is listed meanwhile, though the configuration lists
	// one.
	unreadable := newRoutePlugin(t, func(p *meterPlugin) { p.breakOrganizations() })
	unreadable.configure(protocol.MethodPluginReconfigure, alphaCredit)
	unreadable.wantError("unreadable", http.StatusServiceUnavailable, "organizations_unavailable", map[string]any{"kind": settings.KindOrganizations, "baseRevision": "", "ops": []any{addOp("bravo", fakeOrgB)}})
	if state := unreadable.creditsFile(); state.APICredits != nil || state.Organizations != (client.CreditOrganizations{State: client.OrganizationsStateUnreadable, ConfigItemsIgnored: 1}) {
		t.Fatalf("%+v %+v", state.Organizations, state.APICredits)
	}
}

// The meter follows the page's organizations with no configure: removing the
// last stops it with no_items, adding one opens it from its file with the
// gap, and it counts the next record. A removed organization's values stay
// as an orphan, and adding the same Organization ID back restores them.
func TestOrganizationsChangeTheMeterWithoutAConfigure(t *testing.T) {
	p := newRoutePlugin(t)
	if code, body, _ := p.post(batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", "300", nil, nil))); code != http.StatusOK {
		t.Fatal(body)
	}
	p.removeAll()
	if p.meter.Load() != nil {
		t.Fatal("the meter outlived the last organization")
	}
	if file := p.meterFile(); file.StopReason != client.MeterStopNoItems {
		t.Fatalf("file %+v", file)
	}
	state := p.creditsFile()
	if state.APICredits != nil || state.Organizations.State != client.OrganizationsStateOK {
		t.Fatalf("published %+v %+v", state.APICredits, state.Organizations)
	}
	p.organizationOps(addOp("alpha", fakeOrgA))
	if p.meter.Load() == nil {
		t.Fatal("adding an organization did not open the meter")
	}
	p.usage(goldenRecord(t))
	if account := accountIn(t, p.creditsFile(), alphaAccount); account.Settings.MonthlyUSD != "300" || account.MonthlyCreditSource != client.CreditSourceDashboard {
		t.Fatalf("the value saved before the removal: %+v", account)
	}
	p.meter.Load().Stop(client.MeterStopQuiesce, time.Now().UTC())
	if file := p.meterFile(); file.Counted != 1 || len(file.Gaps) != 1 || file.Gaps[0].Reason != client.MeterStopNoItems {
		t.Fatalf("file %+v", file)
	}
}

// While another organization keeps the meter running, the same meter follows
// the page's organizations with no configure: one added is linked and counts
// its first record, and one removed is unlinked with its history kept.
func TestOrganizationsRelinkTheRunningMeter(t *testing.T) {
	p := newRoutePlugin(t)
	running := p.meter.Load()
	if running == nil {
		t.Fatal("no meter for alpha")
	}
	p.organizationOps(addOp("bravo", fakeOrgB))
	p.usage([]byte(strings.Replace(string(goldenRecord(t)), strings.ToUpper(fakeOrgA), fakeOrgB, 1)))
	p.organizationOps(removeOp(fakeOrgA))
	if p.meter.Load() != running {
		t.Fatal("the meter was replaced, not relinked")
	}
	running.Stop(client.MeterStopQuiesce, time.Now().UTC())
	file := p.meterFile()
	bravo, known := file.Organizations[fakeOrgB]
	if !known || bravo.UnlinkedAt != nil || bravo.LastSeenAt == nil || file.Counted != 1 || len(file.Unlinked) != 0 {
		t.Fatalf("bravo: %+v", file)
	}
	if alpha, known := file.Organizations[fakeOrgA]; !known || alpha.UnlinkedAt == nil {
		t.Fatalf("alpha: %+v", alpha)
	}
}

// A batch sent after the organizations file was deleted, before the next tick,
// meets the file as it is now: no list, and nothing put back from the
// configuration, though it lists an organization. The batch conflicts with the
// empty list, and the page's next add starts a file past the deleted one's
// revision.
func TestABatchAfterTheOrganizationsFileIsDeletedConflicts(t *testing.T) {
	p := newRoutePlugin(t)
	p.configure(protocol.MethodPluginReconfigure, alphaCredit)
	p.organizationOps(addOp("bravo", fakeOrgB))
	read := p.organizationsRevision()
	if err := os.Remove(client.OrganizationsPath(p.path)); err != nil {
		t.Fatal(err)
	}
	rename := map[string]any{"op": settings.OpRename, "id": alphaAccount, "label": "alpha-one"}
	body := p.wantError("deleted", http.StatusConflict, "conflict", map[string]any{"kind": settings.KindOrganizations, "baseRevision": read, "ops": []any{rename}})
	if items, _ := body["organizations"].([]any); body["revision"] != "" || len(items) != 0 {
		t.Fatalf("%v", body)
	}
	if _, err := os.Lstat(client.OrganizationsPath(p.path)); !os.IsNotExist(err) {
		t.Fatal("the deleted file was written again")
	}
	state := p.creditsFile()
	if state.APICredits != nil || state.Organizations != (client.CreditOrganizations{State: client.OrganizationsStateMissing, ConfigItemsIgnored: 1}) {
		t.Fatalf("published %+v %+v", state.Organizations, state.APICredits)
	}
	body = p.organizationOps(addOp("alpha", fakeOrgA))
	revision, _ := strconv.ParseUint(fmt.Sprint(body["revision"]), 10, 64)
	if before, _ := strconv.ParseUint(read, 10, 64); revision <= before || len(body["organizations"].([]any)) != 1 {
		t.Fatalf("the add after the delete: %v, read %s", body, read)
	}
}

// An organizations batch sent before the next tick reads an organizations
// file that could not be opened again, as that tick would. Once its owner is
// fixed, which changes none of the stamps the publisher compares, the page's
// list is published before the answer whatever the batch comes to, since the
// tick has nothing left to follow.
func TestAnOrganizationsBatchPublishesTheFileItOpenedAgain(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a file whatever its mode")
	}
	bravo := client.APICreditOrgAccount(fakeOrgB)
	for _, tc := range []struct {
		name, code string
		status     int
		body       func(read, saved string) map[string]any
	}{
		{"invalid", "invalid_request", http.StatusBadRequest, func(_, saved string) map[string]any {
			return map[string]any{"kind": settings.KindOrganizations, "baseRevision": saved, "ops": []any{}}
		}},
		{"stale", "conflict", http.StatusConflict, func(read, _ string) map[string]any {
			return map[string]any{"kind": settings.KindOrganizations, "baseRevision": read, "ops": []any{removeOp(fakeOrgB)}}
		}},
		{"refused", settings.CodeDuplicateOrganization, http.StatusBadRequest, func(_, saved string) map[string]any {
			return map[string]any{"kind": settings.KindOrganizations, "baseRevision": saved, "ops": []any{addOp("charlie", fakeOrgB)}}
		}},
		{"unchanged", "", http.StatusOK, func(_, saved string) map[string]any {
			return map[string]any{"kind": settings.KindOrganizations, "baseRevision": saved, "ops": []any{map[string]any{"op": settings.OpRename, "id": bravo, "label": "bravo"}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newRoutePlugin(t)
			read := p.organizationsRevision()
			p.organizationOps(addOp("bravo", fakeOrgB))
			saved := p.organizationsRevision()
			_, organizations := p.storesOf()
			fixed := unopenable(t, client.OrganizationsPath(p.path))
			p.followOrganizations(organizations, time.Now().UTC())
			fixed()
			p.publisher.PublishNow()
			state := p.creditsFile()
			if _, listed := accountOf(state, bravo); listed || state.Organizations.State != client.OrganizationsStateUnreadable {
				t.Fatalf("before the batch, published %+v with bravo listed %v", state.Organizations, listed)
			}
			code, body, _ := p.post(tc.body(read, saved))
			if code != tc.status || tc.code != "" && body["error"] != tc.code || tc.code == "" && body["unchanged"] != true {
				t.Fatalf("%d %v", code, body)
			}
			state = p.creditsFile()
			if _, listed := accountOf(state, bravo); !listed || len(unreadFiles(state)) != 0 || state.Organizations.Revision != saved {
				t.Fatalf("after the batch, published %+v with bravo listed %v", state.Organizations, listed)
			}
		})
	}
}

// A batch reads the organizations file again when it changed after the route
// followed it and before Apply judged the batch, and the store then holds what
// it read whatever the batch comes to. The item list, the meter and the
// published state follow that before the route answers, with no tick, and so
// does a tick that comes between Apply and the route's own follow. Replaced by
// a file with the same revision, the list differs in nothing but its
// organizations.
func TestAFileABatchReadAgainIsFollowed(t *testing.T) {
	bravo := client.APICreditOrgAccount(fakeOrgB)
	for _, tc := range []struct {
		name   string
		change func(p *meterPlugin)
		op     map[string]any
		status int
		// code is the route's error, "" for an unchanged batch.
		code   string
		state  string
		listed []string
	}{
		{"moved aside", func(p *meterPlugin) {
			if err := os.Rename(client.OrganizationsPath(p.path), client.OrganizationsPath(p.path)+".aside"); err != nil {
				p.t.Fatal(err)
			}
		}, addOp("charlie", fakeOrgC), http.StatusConflict, "conflict", client.OrganizationsStateMissing, nil},
		{"unreadable", (*meterPlugin).breakOrganizations, addOp("charlie", fakeOrgC),
			http.StatusServiceUnavailable, "organizations_unavailable", client.OrganizationsStateUnreadable, nil},
		{"replaced", func(p *meterPlugin) { p.writeOrganizations([2]string{"bravo-one", fakeOrgB}) },
			map[string]any{"op": settings.OpRename, "id": bravo, "label": "bravo-one"}, http.StatusOK, "", client.OrganizationsStateOK, []string{bravo}},
	} {
		body := map[string]any{"kind": settings.KindOrganizations, "baseRevision": "1", "ops": []any{tc.op}}
		t.Run(tc.name+", before the answer", func(t *testing.T) {
			p := newRoutePlugin(t)
			// The route has followed the file, and it changes before Apply.
			p.trace = func(step string) {
				if step == "apply organizations" {
					tc.change(p)
				}
			}
			code, answer, _ := p.post(body)
			if code != tc.status || tc.code != "" && answer["error"] != tc.code || tc.code == "" && answer["unchanged"] != true {
				t.Fatalf("%d %v", code, answer)
			}
			p.wantFollowed(tc.state, tc.listed)
		})
		t.Run(tc.name+", at a tick", func(t *testing.T) {
			p := newRoutePlugin(t)
			_, organizations := p.storesOf()
			raw, _ := json.Marshal(body)
			batch, err := settings.ParseOrganizationsBatch(raw)
			if err != nil {
				t.Fatal(err)
			}
			tc.change(p)
			result, err := organizations.Apply(batch, time.Now().UTC())
			var conflict *settings.OrganizationsConflictError
			if applied := map[string]bool{
				"conflict":                  errors.As(err, &conflict),
				"organizations_unavailable": errors.Is(err, settings.ErrOrganizationsUnavailable),
				"":                          err == nil && result.Unchanged,
			}; !applied[tc.code] {
				t.Fatalf("Apply: %+v %v", result, err)
			}
			if current := organizations.Current(); current.State != tc.state {
				t.Fatalf("the store holds %+v", current)
			}
			p.publisher.tick(time.Now().UTC())
			p.wantFollowed(tc.state, tc.listed)
		})
	}
}

// wantFollowed requires the item list in force, the meter and the published
// state to follow an organizations file in state that lists the accounts
// listed.
func (p *meterPlugin) wantFollowed(state string, listed []string) {
	p.t.Helper()
	items := []string{}
	for _, item := range *p.apiCredits.Load() {
		items = append(items, item.id)
	}
	if !slices.Equal(items, listed) {
		p.t.Fatalf("in force %v, want %v", items, listed)
	}
	if running := p.meter.Load() != nil; running != (len(listed) > 0) {
		p.t.Fatalf("meter running %v", running)
	}
	published := p.creditsFile()
	accounts := []string{}
	if published.APICredits != nil {
		for _, account := range published.APICredits.Accounts {
			accounts = append(accounts, account.ID)
		}
	}
	if published.Organizations.State != state || !slices.Equal(accounts, listed) {
		p.t.Fatalf("published %+v listing %v", published.Organizations, accounts)
	}
}

// With the last organization removed api_credits is null, yet every saved API
// credit value is still an orphan the page shows and may clear: the status
// route lists them in editor.api_credit_orphans, a batch that clears one is
// saved, and one that sets it is refused.
func TestOrphansOutliveTheLastOrganization(t *testing.T) {
	p := newRoutePlugin(t)
	if code, body, _ := p.post(batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", "300", nil, nil))); code != http.StatusOK {
		t.Fatal(body)
	}
	editorOrphans := func() []map[string]any {
		t.Helper()
		listed, ok := p.statusOf()["editor"].(map[string]any)["api_credit_orphans"].([]any)
		if !ok {
			t.Fatal("no api_credit_orphans in the editor")
		}
		out := []map[string]any{}
		for _, orphan := range listed {
			out = append(out, orphan.(map[string]any))
		}
		return out
	}
	// While api_credits is published it carries the orphans itself.
	if orphans := editorOrphans(); len(orphans) != 0 {
		t.Fatalf("with an organization listed: %v", orphans)
	}
	p.removeAll()
	if state := p.creditsFile(); state.APICredits != nil {
		t.Fatalf("published %+v", state.APICredits)
	}
	orphans := editorOrphans()
	saved := strconv.FormatUint(p.revision(), 10)
	if len(orphans) != 2 || orphans[0]["id"] != orphanAccount || orphans[1]["id"] != alphaAccount || orphans[1]["monthlyUsd"] != "300" || orphans[1]["revision"] != saved {
		t.Fatalf("orphans %v", orphans)
	}
	p.wantError("an orphan set", http.StatusConflict, "not_editable", batchOf(settings.KindAPICredits, creditRow(alphaAccount, saved, "400", nil, nil)))
	code, body, _ := p.post(batchOf(settings.KindAPICredits, creditRow(alphaAccount, saved, nil, nil, nil)))
	if saved, _ := body["settings"].(map[string]any); code != http.StatusOK || saved == nil || saved[alphaAccount] != nil {
		t.Fatalf("an orphan cleared: %d %v", code, body)
	}
	if _, has := p.store.Current().APICredits[alphaAccount]; has {
		t.Fatal("the cleared orphan is still saved")
	}
	if orphans := editorOrphans(); len(orphans) != 1 || orphans[0]["id"] != orphanAccount {
		t.Fatalf("after the clear: %v", orphans)
	}
}

// While the organizations file cannot be read, no organization is listed, and
// any value saved here may belong to one in the file: none is an orphan, in
// the published state or the editor, and the save route refuses to clear or
// set any. Once the file reads again, nothing was lost.
func TestSavedValuesAreHeldWhileTheOrganizationsFileCannotBeRead(t *testing.T) {
	p := newRoutePlugin(t)
	bravo := client.APICreditOrgAccount(fakeOrgB)
	p.organizationOps(addOp("bravo", fakeOrgB))
	if code, body, _ := p.post(batchOf(settings.KindAPICredits, creditRow(bravo, "", "300", nil, nil))); code != http.StatusOK {
		t.Fatalf("bravo's credit: %d %v", code, body)
	}
	bravoRev := strconv.FormatUint(p.revision(), 10)
	good, err := os.ReadFile(client.OrganizationsPath(p.path))
	if err != nil {
		t.Fatal(err)
	}
	editorKey := func(key string) []string {
		t.Helper()
		ids := []string{}
		for _, row := range p.statusOf()["editor"].(map[string]any)[key].([]any) {
			ids = append(ids, row.(map[string]any)["id"].(string))
		}
		return ids
	}

	p.breakOrganizations()
	p.publisher.tick(time.Now().UTC())
	if state := p.creditsFile(); state.Organizations.State != client.OrganizationsStateUnreadable || state.APICredits != nil {
		t.Fatalf("published %+v %+v", state.Organizations, state.APICredits)
	}
	if orphans, kept := editorKey("api_credit_orphans"), editorKey("held_api_credits"); len(orphans) != 0 || !reflect.DeepEqual(kept, []string{orphanAccount, bravo}) {
		t.Fatalf("editor orphans %v, held %v", orphans, kept)
	}
	for name, row := range map[string]map[string]any{
		"clearing bravo":         creditRow(bravo, bravoRev, nil, nil, nil),
		"clearing an old orphan": creditRow(orphanAccount, "1", nil, nil, nil),
		"setting bravo":          creditRow(bravo, bravoRev, "400", nil, nil),
		"setting alpha":          creditRow(alphaAccount, "", "250", nil, nil),
	} {
		p.wantError(name, http.StatusServiceUnavailable, "organizations_unavailable", batchOf(settings.KindAPICredits, row))
	}
	if stored := p.store.Current().APICredits; len(stored) != 2 || stored[bravo].MonthlyUSD != "300" || stored[orphanAccount].MonthlyUSD != "10" {
		t.Fatalf("stored %+v", stored)
	}

	// The file read again: bravo is listed with its credit, and only the old
	// orphan is one.
	writeFile(t, client.OrganizationsPath(p.path), good)
	p.publisher.tick(time.Now().UTC())
	state := p.creditsFile()
	if accountIn(t, state, bravo).Settings.MonthlyUSD != "300" || len(state.APICredits.Orphans) != 1 || state.APICredits.Orphans[0].ID != orphanAccount {
		t.Fatalf("after the fix: %+v", state.APICredits)
	}
	if kept := editorKey("held_api_credits"); len(kept) != 0 {
		t.Fatalf("held after the fix: %v", kept)
	}
}

// While the organizations file cannot be read, every saved API credit row is
// held and a batch naming any of them is refused, even in a state that still
// lists an account: a publish can pair the item list from before the file
// broke with the organizations block from after. A batch with no API credit
// row is not held.
func TestEveryAPICreditRowIsHeldWhileTheOrganizationsFileCannotBeRead(t *testing.T) {
	bravo := client.APICreditOrgAccount(fakeOrgB)
	values := settings.Values{APICredits: map[string]settings.APICredit{
		alphaAccount: {MonthlyUSD: "200", Rev: 1, UpdatedAt: time.Unix(1, 0)},
		bravo:        {MonthlyUSD: "300", Rev: 2, UpdatedAt: time.Unix(2, 0)},
	}}
	state := &client.CreditState{
		Organizations: client.CreditOrganizations{State: client.OrganizationsStateUnreadable},
		APICredits:    &client.CreditDocument{Accounts: []client.CreditAccount{{ID: alphaAccount}}},
	}
	rowBatch := func(id string) settings.Batch {
		return settings.Batch{Kind: settings.KindAPICredits, APICredits: []settings.APICreditItem{{ID: id}}}
	}
	held := []string{}
	for _, row := range heldAPICreditsOf(state, values) {
		held = append(held, row.ID)
	}
	if !reflect.DeepEqual(held, []string{alphaAccount, bravo}) {
		t.Fatalf("held %v", held)
	}
	for _, id := range []string{alphaAccount, bravo} {
		if !heldByOrganizations(rowBatch(id), state) {
			t.Fatalf("a batch naming %s is not held", id)
		}
	}
	if heldByOrganizations(settings.Batch{Kind: settings.KindRenewals}, state) {
		t.Fatal("a batch with no API credit row is held")
	}

	state.Organizations.State = client.OrganizationsStateOK
	if held := heldAPICreditsOf(state, values); len(held) != 0 || heldByOrganizations(rowBatch(bravo), state) {
		t.Fatalf("held while the file reads: %v", held)
	}
}

// Saves of both kinds, ticks, publishes, status reads, usage records and
// reconfigures, all at once: every one returns, and the race detector sees
// nothing. The save route holds one lock at a time (each store's own, then
// linkMu, then the publisher's), and a tick never takes the plugin's mu.
func TestConcurrentSavesTicksAndConfiguresComplete(t *testing.T) {
	p := newRoutePlugin(t)
	record := goldenRecord(t)
	finished := make(chan struct{})
	var wg sync.WaitGroup
	deadline := time.Now().Add(1500 * time.Millisecond)
	run := func(f func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; time.Now().Before(deadline); i++ {
				f(i)
			}
		}()
	}
	post := func(body any) {
		raw, _ := json.Marshal(body)
		request, _ := json.Marshal(protocol.ManagementRequest{Method: "POST", Path: settingsPath, Headers: http.Header{"Content-Type": {"application/json"}}, Body: raw})
		if _, err := p.Handle(protocol.MethodManagementHandle, request); err != nil {
			t.Error(err)
		}
	}
	base := ""
	run(func(i int) {
		raw, _ := json.Marshal(batchOf(settings.KindRenewals, renewalRow(claudeHexID, base, fmt.Sprintf("2026-10-%02d", i%28+1))))
		request, _ := json.Marshal(protocol.ManagementRequest{Method: "POST", Path: settingsPath, Headers: http.Header{"Content-Type": {"application/json"}}, Body: raw})
		result, err := p.Handle(protocol.MethodManagementHandle, request)
		if err != nil {
			t.Error(err)
			return
		}
		answer := map[string]any{}
		_ = json.Unmarshal(result.(protocol.ManagementResponse).Body, &answer)
		if revision, ok := answer["revision"].(string); ok {
			base = revision
		}
	})
	run(func(i int) {
		op := addOp("bravo", fakeOrgB)
		if i%2 == 1 {
			op = removeOp(fakeOrgB)
		}
		p.mu.Lock()
		store := p.organizations
		p.mu.Unlock()
		if store != nil {
			post(map[string]any{"kind": settings.KindOrganizations, "baseRevision": settings.RevisionText(store.Current().Revision), "ops": []any{op}})
		}
	})
	run(func(int) {
		p.mu.Lock()
		publisher := p.publisher
		p.mu.Unlock()
		if publisher != nil {
			publisher.tick(time.Now().UTC())
			publisher.PublishNow()
		}
	})
	run(func(int) {
		raw, _ := json.Marshal(protocol.ManagementRequest{Method: "GET", Path: "/v0/management/plugins/quota-cache/status"})
		if _, err := p.Handle(protocol.MethodManagementHandle, raw); err != nil {
			t.Error(err)
		}
	})
	run(func(int) { _, _ = p.Handle(protocol.MethodUsageHandle, record) })
	run(func(int) {
		time.Sleep(50 * time.Millisecond)
		raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("cache-path: " + p.path + "\nrequest-spacing: 1s\n" + alphaCredit)})
		if _, err := p.Handle(protocol.MethodPluginReconfigure, raw); err != nil {
			t.Error(err)
		}
	})
	go func() {
		wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("deadlock: a save, tick or configure never returned")
	}
	// Everything still answers, and the state still publishes.
	if code, status := p.status(); code != http.StatusOK || !strings.Contains(status, `"editor":{`) {
		t.Fatalf("status after: %d %s", code, status)
	}
	if state := p.published(); state.Settings.State != client.CreditSettingsOK {
		t.Fatalf("published after: %+v", state.Settings)
	}
}
