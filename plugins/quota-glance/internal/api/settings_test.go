package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/aggregate"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/overrides"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
)

const (
	settingsPath     = "/v0/management/plugins/quota-glance/settings"
	saveSettingsPath = "/v0/resource/plugins/quota-glance/save-settings"
	settingsOrg      = "00000000-0000-4000-8000-00000000000a"
	claudeHexID      = "0123456789abcdef"
	orphanAccount    = "org-3f2a9c1d0b7e"
	orphanRenewal    = "fedcba9876543210"
)

var (
	settingsNow     = time.Date(2026, time.October, 9, 14, 0, 0, 0, time.UTC)
	editableAccount = qc.APICreditOrgAccount(settingsOrg)
)

// testSaver is the runtime's saver in miniature: the real store, and a
// rebuild that publishes the document from what it now holds.
type testSaver struct {
	store *overrides.Store
	api   *API
	in    aggregate.Input
	saves atomic.Int32
	fail  error
}

func (s *testSaver) Current() overrides.Values { return s.store.Current() }

func (s *testSaver) Save(b overrides.Batch, now time.Time) (overrides.Result, error) {
	s.saves.Add(1)
	if s.fail != nil {
		return overrides.Result{}, s.fail
	}
	result, err := s.store.Apply(b, s.in.Meter, now)
	if err == nil && !result.Unchanged {
		s.rebuild()
	}
	return result, err
}

func (s *testSaver) rebuild() {
	in := s.in
	in.Overrides = s.store.Current()
	s.api.Publish(aggregate.Build(in, settingsNow), Health{Version: "test"})
}

// settingsAPI serves a document with one editable organization, one item
// quota-cache could not link, a Claude credential whose renewal date may be
// set, a Codex credential whose may not, and an orphan of each kind.
func settingsAPI(t *testing.T) (*API, *testSaver) {
	t.Helper()
	dir := t.TempDir()
	raw, _ := json.Marshal(map[string]any{
		"schema": 1, "revision": 2,
		"apiCredits": map[string]any{orphanAccount: map[string]any{"monthlyUsd": "10", "rev": 1, "updatedAt": "2026-10-01T00:00:00Z"}},
		"renewals":   map[string]any{orphanRenewal: map[string]any{"date": "2026-10-29", "rev": 2, "updatedAt": "2026-10-02T00:00:00Z"}},
	})
	if err := os.WriteFile(filepath.Join(dir, overrides.FileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	linked := qc.Entry{Provider: qc.ProviderAnthropicAPI, AuthIndex: editableAccount, APICredit: &qc.APICredit{
		Label: "alpha", MonthlyUSD: "200", Renews: "2026-10-05", OrganizationID: settingsOrg,
	}}
	unlinked := qc.Entry{Provider: qc.ProviderAnthropicAPI, AuthIndex: "item-2", APICredit: &qc.APICredit{
		Label: "bravo", Position: 1, Problem: qc.CreditProblemOrganizationIDMissing,
	}}
	day := qc.MeterDayStart(settingsNow)
	in := aggregate.Input{
		Snapshot: qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{
			qc.Key(linked.Provider, linked.AuthIndex): linked, qc.Key(unlinked.Provider, unlinked.AuthIndex): unlinked,
		}},
		Identities: []aggregate.Identity{
			{AuthIndex: claudeHexID, Provider: "claude"},
			{AuthIndex: "codex-a@example.com.json", Provider: "codex"},
		},
		Meter: &qc.APIMeter{Schema: 1, FlushedAt: settingsNow.Add(-time.Minute), Gaps: []qc.MeterGap{}, Unlinked: []qc.MeterUnlinked{},
			Auths: map[string]qc.MeterAuth{}, Organizations: map[string]qc.MeterOrganization{settingsOrg: {
				Since: day.Add(-30 * 24 * time.Hour), Days: []qc.MeterBucket{},
				Hours: []qc.MeterBucket{{Start: day.Add(9 * time.Hour), Usage: []qc.MeterUsage{{Model: "claude-sonnet-5-5", Input: 1_000_000}}}},
			}}},
		AllowEdit: true,
	}
	a := New("quota-glance", testToken)
	saver := &testSaver{store: overrides.Open(dir), api: a, in: in}
	saver.rebuild()
	a.SetSaver(saver)
	return a, saver
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

func postSettings(a *API, body any) protocol.ManagementResponse {
	raw, _ := json.Marshal(body)
	return a.Handle(protocol.ManagementRequest{Method: "POST", Path: settingsPath,
		Headers: http.Header{"Content-Type": {"application/json"}}, Body: raw}, settingsNow)
}

func encodeSettings(body any) string {
	raw, _ := json.Marshal(body)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func getSettings(a *API, headers http.Header) protocol.ManagementResponse {
	return a.Handle(protocol.ManagementRequest{Method: "GET", Path: saveSettingsPath, Headers: headers}, settingsNow)
}

func settingsHeaders(body any) http.Header {
	return http.Header{"Authorization": {"Bearer " + testToken}, settingsHeader: {encodeSettings(body)}}
}

// door sends one batch down one door.
type door struct {
	name string
	send func(*API, any) protocol.ManagementResponse
}

var doors = []door{
	{DoorConsole, postSettings},
	{DoorPassword, func(a *API, body any) protocol.ManagementResponse { return getSettings(a, settingsHeaders(body)) }},
}

func wantError(t *testing.T, name string, res protocol.ManagementResponse, status int, code string) map[string]any {
	t.Helper()
	if res.StatusCode != status {
		t.Fatalf("%s: status %d, want %d: %s", name, res.StatusCode, status, res.Body)
	}
	body := decodeBody(t, res)
	if body["error"] != code {
		t.Fatalf("%s: error %v, want %s", name, body["error"], code)
	}
	return body
}

// Both doors save the same batch the same way: one commit, a rebuilt
// document, and the rows' new blocks in the answer. Resent, it is a no-op.
func TestBothDoorsSave(t *testing.T) {
	for _, d := range doors {
		a, saver := settingsAPI(t)
		res := d.send(a, batchOf(overrides.KindAPICredits, creditRow(editableAccount, "", "300", nil, nil)))
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", d.name, res.StatusCode, res.Body)
		}
		body := decodeBody(t, res)
		block, _ := body["settings"].(map[string]any)[editableAccount].(map[string]any)
		if body["ok"] != true || body["unchanged"] != false || body["revision"] != "3" ||
			block["monthlyUsd"] != "300" || block["configMonthlyUsd"] != "200" || block["revision"] != "3" || block["editable"] != true {
			t.Fatalf("%s: %s", d.name, res.Body)
		}
		// The document served now is the rebuilt one.
		if account := a.Document().APICredits.Accounts[0]; account.MonthlyCreditText != "$300.00" || account.MonthlyCreditSource != "dashboard" {
			t.Fatalf("%s: served %+v", d.name, account)
		}
		again := d.send(a, batchOf(overrides.KindAPICredits, creditRow(editableAccount, "", "300", nil, nil)))
		if body := decodeBody(t, again); again.StatusCode != http.StatusOK || body["unchanged"] != true || body["revision"] != "3" {
			t.Fatalf("%s: a resent batch: %d %s", d.name, again.StatusCode, again.Body)
		}
		if saver.store.Current().Revision != 3 {
			t.Fatalf("%s: revision %d", d.name, saver.store.Current().Revision)
		}

		res = d.send(a, batchOf(overrides.KindRenewals, renewalRow(claudeHexID, "", "2026-10-29")))
		block, _ = decodeBody(t, res)["settings"].(map[string]any)[claudeHexID].(map[string]any)
		if res.StatusCode != http.StatusOK || block["date"] != "2026-10-29" || block["revision"] != "4" {
			t.Fatalf("%s: renewal: %d %s", d.name, res.StatusCode, res.Body)
		}
		res = d.send(a, batchOf(overrides.KindRenewals, renewalRow(claudeHexID, "4", nil)))
		if settings := decodeBody(t, res)["settings"].(map[string]any); res.StatusCode != http.StatusOK || settings[claudeHexID] != nil {
			t.Fatalf("%s: use estimate: %d %s", d.name, res.StatusCode, res.Body)
		}
	}
}

// The disabled plugin, then the GET fences, then the token, then whether
// editing is on at all, exactly as for a press: an unauthenticated caller
// learns nothing about editing.
func TestTheOrderOfChecksOnTheGETDoor(t *testing.T) {
	a, saver := settingsAPI(t)
	good := encodeSettings(batchOf(overrides.KindAPICredits, creditRow(editableAccount, "", "300", nil, nil)))

	a.Disable()
	wantError(t, "disabled", getSettings(a, http.Header{settingsHeader: {good}}), http.StatusServiceUnavailable, "disabled")
	a.Enable()

	// The fences answer before the token is looked at, and cost the limiter
	// nothing.
	for range failureLimit + 5 {
		wantError(t, "cross site", getSettings(a, http.Header{"Sec-Fetch-Site": {"cross-site"}, settingsHeader: {good}}), http.StatusForbidden, "cross_site")
		wantError(t, "early data", getSettings(a, http.Header{"Early-Data": {"1"}, settingsHeader: {good}}), http.StatusTooEarly, "too_early")
	}
	if res := getSettings(a, http.Header{settingsHeader: {good}}); res.StatusCode != http.StatusUnauthorized || len(res.Body) != 0 {
		t.Fatalf("no token: %d %s; the fences must not have spent the limiter", res.StatusCode, res.Body)
	}
	// Without the token, nothing says whether editing is on.
	a.SetSaver(nil)
	if res := getSettings(a, http.Header{"Authorization": {"Bearer wrong"}, settingsHeader: {good}}); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a wrong token with editing off: %d", res.StatusCode)
	}
	wantError(t, "editing off", getSettings(a, settingsHeaders(batchOf(overrides.KindAPICredits))), http.StatusNotFound, "not_found")
	a.SetSaver(saver)
	if saver.saves.Load() != 0 {
		t.Fatal("a refused request reached the store")
	}
}

// Failed token attempts count against the shared limiter; the operator's own
// token is never throttled.
func TestABadTokenReachesTheLimiterAndAGoodOneNever(t *testing.T) {
	a, _ := settingsAPI(t)
	good := encodeSettings(batchOf(overrides.KindAPICredits, creditRow(editableAccount, "", "300", nil, nil)))
	throttled := false
	for range failureLimit + 5 {
		res := getSettings(a, http.Header{"Authorization": {"Bearer wrong"}, settingsHeader: {good}})
		switch res.StatusCode {
		case http.StatusTooManyRequests:
			throttled = true
			if res.Headers.Get("Retry-After") != "60" {
				t.Fatalf("Retry-After = %q", res.Headers.Get("Retry-After"))
			}
		case http.StatusUnauthorized:
		default:
			t.Fatalf("status %d", res.StatusCode)
		}
	}
	if !throttled {
		t.Fatal("failed attempts are never throttled")
	}
	if res := getSettings(a, settingsHeaders(batchOf(overrides.KindAPICredits, creditRow(editableAccount, "", "300", nil, nil)))); res.StatusCode != http.StatusOK {
		t.Fatalf("the right token was throttled: %d", res.StatusCode)
	}
	// The same limiter guards the summary: the settings door spent it.
	if res := a.Handle(protocol.ManagementRequest{Method: "GET", Path: tokenPath, Headers: bearer("wrong")}, settingsNow); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("summary after settings failures: %d", res.StatusCode)
	}
}

func TestTheOrderOfChecksOnThePOSTDoor(t *testing.T) {
	a, saver := settingsAPI(t)
	body := batchOf(overrides.KindAPICredits, creditRow(editableAccount, "", "300", nil, nil))
	a.Disable()
	wantError(t, "disabled", postSettings(a, body), http.StatusServiceUnavailable, "disabled")
	a.Enable()
	a.SetSaver(nil)
	wantError(t, "editing off", postSettings(a, body), http.StatusNotFound, "not_found")
	a.SetSaver(saver)
	raw, _ := json.Marshal(body)
	for _, media := range []string{"application/x-www-form-urlencoded", "text/plain", "multipart/form-data; boundary=x"} {
		res := a.Handle(protocol.ManagementRequest{Method: "POST", Path: settingsPath, Headers: http.Header{"Content-Type": {media}}, Body: raw}, settingsNow)
		wantError(t, media, res, http.StatusUnsupportedMediaType, "unsupported_media_type")
	}
	if saver.saves.Load() != 0 {
		t.Fatal("a refused request reached the store")
	}
}

// settings.json that cannot be read answers 503 on both doors, and is not
// written over.
func TestAnUnreadableSettingsFileRefusesToSave(t *testing.T) {
	for _, d := range doors {
		dir := t.TempDir()
		path := filepath.Join(dir, overrides.FileName)
		if err := os.WriteFile(path, []byte(`{"schema":2}`), 0o600); err != nil {
			t.Fatal(err)
		}
		a, saver := settingsAPI(t)
		saver.store = overrides.Open(dir)
		wantError(t, d.name, d.send(a, batchOf(overrides.KindAPICredits, creditRow(editableAccount, "", "300", nil, nil))),
			http.StatusServiceUnavailable, "settings_unavailable")
		if raw, _ := os.ReadFile(path); string(raw) != `{"schema":2}` {
			t.Fatalf("%s: overwritten", d.name)
		}
	}
}

// Everything that is not exactly a batch is invalid_request, on both doors.
func TestMalformedBatchesAreRefused(t *testing.T) {
	a, saver := settingsAPI(t)
	row := creditRow(editableAccount, "", "300", nil, nil)
	missing := creditRow(editableAccount, "", "300", nil, nil)
	delete(missing, "reading")
	unknown := creditRow(editableAccount, "", "300", nil, nil)
	unknown["note"] = "x"
	big := creditRow(editableAccount, "", strings.Repeat("9", 4100), nil, nil)
	cases := map[string]any{
		"null":           nil,
		"an array":       []any{row},
		"unknown kind":   batchOf("budgets", row),
		"no items":       batchOf(overrides.KindAPICredits),
		"a missing key":  batchOf(overrides.KindAPICredits, missing),
		"an unknown key": batchOf(overrides.KindAPICredits, unknown),
		"a repeated id":  batchOf(overrides.KindAPICredits, row, row),
		"over 4096":      batchOf(overrides.KindAPICredits, big),
	}
	seventeen := []map[string]any{}
	for i := range 17 {
		seventeen = append(seventeen, creditRow("org-00000000000"+string(rune('a'+i%6))+string(rune('a'+i/6)), "", nil, nil, nil))
	}
	cases["seventeen rows"] = batchOf(overrides.KindAPICredits, seventeen...)
	for _, d := range doors {
		for name, body := range cases {
			wantError(t, d.name+": "+name, d.send(a, body), http.StatusBadRequest, "invalid_request")
		}
	}
	// The GET door's own shapes: exactly one header, base64url without
	// padding, and an object.
	good := encodeSettings(row)
	for name, headers := range map[string]http.Header{
		"no header":   {"Authorization": {"Bearer " + testToken}},
		"two headers": {"Authorization": {"Bearer " + testToken}, settingsHeader: {good, good}},
		"padded":      {"Authorization": {"Bearer " + testToken}, settingsHeader: {base64.URLEncoding.EncodeToString([]byte(`{"kind":"renewals","items":[]}`))}},
		"not base64":  {"Authorization": {"Bearer " + testToken}, settingsHeader: {"!!!"}},
		"oversized":   {"Authorization": {"Bearer " + testToken}, settingsHeader: {strings.Repeat("A", 5500)}},
	} {
		wantError(t, name, getSettings(a, headers), http.StatusBadRequest, "invalid_request")
	}
	if saver.saves.Load() != 0 {
		t.Fatal("a malformed batch reached the store")
	}
}

// Each value rule answers with its code, the row and the field.
func TestEachValueRuleNamesItsField(t *testing.T) {
	a, saver := settingsAPI(t)
	reading := func(amount, at string) map[string]any { return map[string]any{"remainingUsd": amount, "at": at} }
	for name, tc := range map[string]struct {
		body        any
		code, field string
		id          string
	}{
		"monthly":       {batchOf(overrides.KindAPICredits, creditRow(editableAccount, "", "200.123", nil, nil)), "invalid_monthly_usd", "monthlyUsd", editableAccount},
		"renews":        {batchOf(overrides.KindAPICredits, creditRow(editableAccount, "", nil, "2026-11-31", nil)), "invalid_renews", "renews", editableAccount},
		"amount":        {batchOf(overrides.KindAPICredits, creditRow(editableAccount, "", nil, nil, reading("1e3", "2026-10-09T13:00:00Z"))), "invalid_reading_amount", "reading.remainingUsd", editableAccount},
		"time":          {batchOf(overrides.KindAPICredits, creditRow(editableAccount, "", nil, nil, reading("100.00", "2026-10-07T13:59:59Z"))), "invalid_reading_time", "reading.at", editableAccount},
		"before refill": {batchOf(overrides.KindAPICredits, creditRow(editableAccount, "", nil, "2026-10-09", reading("100.00", "2026-10-08T20:00:00Z"))), "reading_before_refill", "reading.at", editableAccount},
		"date":          {batchOf(overrides.KindRenewals, renewalRow(claudeHexID, "", "2026-02-30")), "invalid_date", "date", claudeHexID},
	} {
		for _, d := range doors {
			body := wantError(t, d.name+": "+name, d.send(a, tc.body), http.StatusBadRequest, tc.code)
			if body["id"] != tc.id || body["field"] != tc.field {
				t.Errorf("%s %s: %v", d.name, name, body)
			}
		}
	}
	// A value rule is judged before whether the row may be edited at all.
	body := wantError(t, "both", postSettings(a, batchOf(overrides.KindAPICredits, creditRow("item-2", "", "x", nil, nil))), http.StatusBadRequest, "invalid_monthly_usd")
	if body["id"] != "item-2" {
		t.Errorf("%v", body)
	}
	if saver.saves.Load() != 0 {
		t.Fatal("an invalid value reached the store")
	}
}

// Only rows the served document offers for editing may be saved, all of them
// or none.
func TestOnlyWhatTheDocumentOffersIsSaved(t *testing.T) {
	a, saver := settingsAPI(t)
	for name, tc := range map[string]struct {
		body any
		ids  []any
	}{
		"an unlinked item":         {batchOf(overrides.KindAPICredits, creditRow("item-2", "", "300", nil, nil)), []any{"item-2"}},
		"an unknown account":       {batchOf(overrides.KindAPICredits, creditRow("org-000000000000", "", "300", nil, nil)), []any{"org-000000000000"}},
		"one bad row of two":       {batchOf(overrides.KindAPICredits, creditRow(editableAccount, "", "300", nil, nil), creditRow("item-2", "", "1", nil, nil)), []any{"item-2"}},
		"a Codex renewal":          {batchOf(overrides.KindRenewals, renewalRow("codex-a@example.com.json", "", "2026-10-29")), []any{"codex-a@example.com.json"}},
		"setting an orphan":        {batchOf(overrides.KindAPICredits, creditRow(orphanAccount, "1", "20", nil, nil)), []any{orphanAccount}},
		"setting a renewal orphan": {batchOf(overrides.KindRenewals, renewalRow(orphanRenewal, "2", "2026-11-29")), []any{orphanRenewal}},
	} {
		body := wantError(t, name, postSettings(a, tc.body), http.StatusConflict, "not_editable")
		if !reflect.DeepEqual(body["ids"], tc.ids) {
			t.Errorf("%s: ids %v", name, body["ids"])
		}
	}
	if saver.saves.Load() != 0 {
		t.Fatal("a row the document does not offer reached the store")
	}
	// Clearing either orphan is allowed, and removes it.
	if res := postSettings(a, batchOf(overrides.KindAPICredits, creditRow(orphanAccount, "1", nil, nil, nil))); res.StatusCode != http.StatusOK ||
		decodeBody(t, res)["settings"].(map[string]any)[orphanAccount] != nil {
		t.Fatalf("clearing an orphan: %d %s", res.StatusCode, res.Body)
	}
	if res := postSettings(a, batchOf(overrides.KindRenewals, renewalRow(orphanRenewal, "2", nil))); res.StatusCode != http.StatusOK {
		t.Fatalf("clearing a renewal orphan: %d %s", res.StatusCode, res.Body)
	}
	if doc := a.Document(); len(doc.APICredits.Orphans) != 0 || len(doc.RenewalOrphans) != 0 {
		t.Fatalf("orphans left: %+v %+v", doc.APICredits.Orphans, doc.RenewalOrphans)
	}
}

// A row another device saved since the page read it is a conflict, with what
// is there now; sending what is already there is not.
func TestConflictsCarryTheCurrentValues(t *testing.T) {
	a, saver := settingsAPI(t)
	if res := postSettings(a, batchOf(overrides.KindAPICredits, creditRow(editableAccount, "", "300", nil, nil))); res.StatusCode != http.StatusOK {
		t.Fatal(string(res.Body))
	}
	for _, d := range doors {
		body := wantError(t, d.name, d.send(a, batchOf(overrides.KindAPICredits, creditRow(editableAccount, "", "250", nil, nil))), http.StatusConflict, "conflict")
		block, _ := body["current"].(map[string]any)[editableAccount].(map[string]any)
		if body["revision"] != "3" || !reflect.DeepEqual(body["conflicts"], []any{editableAccount}) || block["monthlyUsd"] != "300" || block["revision"] != "3" {
			t.Fatalf("%s: %v", d.name, body)
		}
		if res := d.send(a, batchOf(overrides.KindAPICredits, creditRow(editableAccount, "", "300", nil, nil))); res.StatusCode != http.StatusOK || decodeBody(t, res)["unchanged"] != true {
			t.Fatalf("%s: an identical replay: %d %s", d.name, res.StatusCode, res.Body)
		}
	}
	if saver.store.Current().Revision != 3 {
		t.Fatal("a conflict wrote")
	}
	if res := postSettings(a, batchOf(overrides.KindRenewals, renewalRow(claudeHexID, "", "2026-10-29"))); res.StatusCode != http.StatusOK {
		t.Fatal(string(res.Body))
	}
	body := wantError(t, "renewal", postSettings(a, batchOf(overrides.KindRenewals, renewalRow(claudeHexID, "", "2026-11-01"))), http.StatusConflict, "conflict")
	if block, _ := body["current"].(map[string]any)[claudeHexID].(map[string]any); block["date"] != "2026-10-29" || block["revision"] != "4" {
		t.Fatalf("renewal conflict: %v", body)
	}
}

// A week-old reading resent unchanged beside a new credit saves; the clock
// only judges a new reading.
func TestAnUnchangedOldReadingResentWithANewCreditSaves(t *testing.T) {
	a, saver := settingsAPI(t)
	at := "2026-10-09T13:20:00Z"
	if res := postSettings(a, batchOf(overrides.KindAPICredits, creditRow(editableAccount, "", nil, nil, map[string]any{"remainingUsd": "150.00", "at": at}))); res.StatusCode != http.StatusOK {
		t.Fatal(string(res.Body))
	}
	stored := saver.store.Current().APICredits[editableAccount].Reading
	if stored.OrganizationID != settingsOrg || len(stored.Baseline.Usage) != 1 || stored.Baseline.Usage[0].Input != 1_000_000 {
		t.Fatalf("baseline: %+v", stored)
	}
	// A week on.
	later := settingsNow.Add(7 * 24 * time.Hour)
	raw, _ := json.Marshal(batchOf(overrides.KindAPICredits, creditRow(editableAccount, "3", "300", nil, map[string]any{"remainingUsd": "150.00", "at": at})))
	res := a.Handle(protocol.ManagementRequest{Method: "POST", Path: settingsPath, Body: raw, Headers: http.Header{}}, later)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", res.StatusCode, res.Body)
	}
	if again := saver.store.Current().APICredits[editableAccount]; again.Reading != stored || again.MonthlyUSD != "300" {
		t.Fatalf("stored: %+v", again)
	}
}

// A meter that cannot cover the reading's baseline refuses it.
func TestAnUncoveredBaselineIsAReadingTimeError(t *testing.T) {
	a, saver := settingsAPI(t)
	saver.in.Meter.FlushedAt = settingsNow.Add(96 * time.Hour)
	body := wantError(t, "uncovered", postSettings(a, batchOf(overrides.KindAPICredits,
		creditRow(editableAccount, "", nil, nil, map[string]any{"remainingUsd": "1.00", "at": "2026-10-09T13:20:00Z"}))),
		http.StatusBadRequest, "invalid_reading_time")
	if body["id"] != editableAccount || body["field"] != "reading.at" {
		t.Fatalf("%v", body)
	}
}

// A meter stopped hours ago is when the page asks for a reading, so a reading
// saves then, and the rest of its batch with it.
func TestAReadingSavesWhileTheMeterIsStopped(t *testing.T) {
	a, saver := settingsAPI(t)
	stopped := settingsNow.Add(-4 * time.Hour)
	saver.in.Meter.FlushedAt, saver.in.Meter.StoppedAt, saver.in.Meter.StopReason = stopped, &stopped, "disabled"
	res := postSettings(a, batchOf(overrides.KindAPICredits,
		creditRow(editableAccount, "", "300", nil, map[string]any{"remainingUsd": "143.20", "at": "2026-10-09T13:55:00Z"})))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", res.StatusCode, res.Body)
	}
	stored := saver.store.Current().APICredits[editableAccount]
	if stored.MonthlyUSD != "300" || stored.Reading == nil || len(stored.Reading.Baseline.Usage) != 1 || stored.Reading.Baseline.Usage[0].Input != 1_000_000 {
		t.Fatalf("stored: %+v %+v", stored, stored.Reading)
	}
}

// A new reading stores the account's organization. One the document gives in
// a form a stored reading may not hold is refused as not editable, and nothing
// in the batch is saved.
func TestAReadingWithoutAUsableOrganizationIsNotEditable(t *testing.T) {
	a, saver := settingsAPI(t)
	key := qc.Key(qc.ProviderAnthropicAPI, editableAccount)
	entry := saver.in.Snapshot.Entries[key]
	credit := *entry.APICredit
	credit.OrganizationID = strings.ToUpper(settingsOrg)
	entry.APICredit = &credit
	saver.in.Snapshot.Entries[key] = entry
	saver.rebuild()
	body := wantError(t, "unnormalized", postSettings(a, batchOf(overrides.KindAPICredits,
		creditRow(editableAccount, "", "300", nil, map[string]any{"remainingUsd": "1.00", "at": "2026-10-09T13:20:00Z"}))),
		http.StatusConflict, "not_editable")
	if !reflect.DeepEqual(body["ids"], []any{editableAccount}) {
		t.Fatalf("ids %v", body["ids"])
	}
	if saver.store.Current().Revision != 2 {
		t.Fatal("part of the batch was saved")
	}
}

// Thirty committed saves a minute across both doors, then 429.
func TestCommittedSavesAreThrottled(t *testing.T) {
	a, _ := settingsAPI(t)
	revision := 2
	for i := range 30 {
		d := doors[i%2]
		res := d.send(a, batchOf(overrides.KindRenewals, renewalRow(claudeHexID, revisionOrEmpty(i, revision), fmt.Sprintf("2026-10-%02d", i%28+1))))
		if res.StatusCode != http.StatusOK {
			t.Fatalf("save %d: %d %s", i, res.StatusCode, res.Body)
		}
		revision++
	}
	res := postSettings(a, batchOf(overrides.KindRenewals, renewalRow(claudeHexID, revisionOrEmpty(30, revision), "2026-11-01")))
	wantError(t, "past the limit", res, http.StatusTooManyRequests, "too_many_writes")
	if res.Headers.Get("Retry-After") != "60" {
		t.Fatalf("Retry-After = %q", res.Headers.Get("Retry-After"))
	}
}

// revisionOrEmpty is the base revision of the row on its i-th save: none
// stored before the first.
func revisionOrEmpty(i, revision int) string {
	if i == 0 {
		return ""
	}
	return strconv.Itoa(revision)
}

// The store's own failures each have their answer.
func TestStoreFailuresAreAnswered(t *testing.T) {
	for err, want := range map[error]struct {
		status int
		code   string
	}{
		overrides.ErrUnwritable: {http.StatusServiceUnavailable, "settings_unwritable"},
		overrides.ErrFull:       {http.StatusConflict, "settings_full"},
		errors.New("anything"):  {http.StatusServiceUnavailable, "settings_unwritable"},
	} {
		a, saver := settingsAPI(t)
		saver.fail = err
		wantError(t, err.Error(), postSettings(a, batchOf(overrides.KindAPICredits, creditRow(editableAccount, "", "300", nil, nil))), want.status, want.code)
	}
}

// The doors are exact: the console door takes only POST, the password door
// only GET, and neither exists on the other tree.
func TestSettingsRoutesAreExact(t *testing.T) {
	a, saver := settingsAPI(t)
	body := encodeSettings(batchOf(overrides.KindAPICredits, creditRow(editableAccount, "", "300", nil, nil)))
	for _, req := range []protocol.ManagementRequest{
		{Method: "GET", Path: settingsPath, Headers: settingsHeaders(nil)},
		{Method: "POST", Path: saveSettingsPath, Headers: http.Header{"Authorization": {"Bearer " + testToken}, settingsHeader: {body}}},
		{Method: "POST", Path: "/v0/resource/plugins/quota-glance/settings", Headers: bearer(testToken)},
		{Method: "GET", Path: "/v0/management/plugins/quota-glance/save-settings", Headers: settingsHeaders(nil)},
		{Method: "PUT", Path: settingsPath},
		{Method: "GET", Path: saveSettingsPath + "/", Headers: http.Header{"Authorization": {"Bearer " + testToken}, settingsHeader: {body}}},
	} {
		if res := a.Handle(req, settingsNow); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s: %d", req.Method, req.Path, res.StatusCode)
		}
	}
	if saver.saves.Load() != 0 {
		t.Fatal("a wrong route saved")
	}
}

// No answer carries the token or echoes the batch it was sent.
func TestSettingsEchoNeitherTheTokenNorTheBatch(t *testing.T) {
	a, _ := settingsAPI(t)
	for _, res := range []protocol.ManagementResponse{
		getSettings(a, settingsHeaders(batchOf(overrides.KindAPICredits, creditRow(editableAccount, "", "300", nil, nil)))),
		getSettings(a, http.Header{"Authorization": {"Bearer " + testToken}, settingsHeader: {"!!!"}}),
		getSettings(a, http.Header{"Authorization": {"Bearer wrong-" + testToken}, settingsHeader: {"x"}}),
	} {
		if strings.Contains(string(res.Body), testToken) {
			t.Fatalf("an answer carries the token: %s", res.Body)
		}
		for name := range res.Headers {
			if strings.Contains(strings.Join(res.Headers.Values(name), ","), testToken) || name == settingsHeader {
				t.Fatalf("a header echoes the request: %s", name)
			}
		}
	}
}
