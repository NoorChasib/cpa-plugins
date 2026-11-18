package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/settings"
)

// The OpenAI organizations these tests list: the personal organization the
// meter's synthetic success names, the named one its credit_balance_exhausted
// refusal names, and one no record names.
const (
	proSlug  = "user-AbCdEfGhIjKlMnOpQrStUvWx"
	acmeSlug = "acme-x1wegt"
	sideSlug = "side-k2m9qz"
)

var proAccount = client.OpenAICreditOrgAccount(proSlug)

const openAICounting = "quota-cache OpenAI API meter counting openai-compatibility traffic"

// openAIRecord is the meter's synthetic openai-compatibility success, which
// names proSlug, and openAIRefusal its credit_balance_exhausted 429, which
// names acmeSlug. Both come from the same auth index.
func openAIRecord(t *testing.T) []byte {
	return meterFixture(t, "usage-record-openai-compat-v8.0.77.json")
}

func openAIRefusal(t *testing.T) []byte {
	return meterFixture(t, "usage-record-openai-compat-429-v8.0.77.json")
}

func meterFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "meter", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

var organizationHeader = regexp.MustCompile(`"Openai-Organization":\["[^"]*"\],`)

// headerless is record without its openai-organization header, as an
// openai-compatibility entry pointed at another upstream answers.
func headerless(t *testing.T, record []byte) []byte {
	t.Helper()
	out := organizationHeader.ReplaceAll(record, nil)
	if bytes.Equal(out, record) {
		t.Fatal("the record names no organization")
	}
	return out
}

// writeOpenAIOrganizations writes the OpenAI organizations file beside the
// snapshot, as the page saves it: each organization a label and the slug
// OpenAI's responses name it by, at revision 1.
func (p *meterPlugin) writeOpenAIOrganizations(organizations ...[2]string) {
	p.t.Helper()
	items := []any{}
	for _, o := range organizations {
		items = append(items, map[string]any{"label": o[0], "organization_id": o[1], "rev": "1", "updated_at": "2026-10-01T00:00:00Z"})
	}
	raw, _ := json.Marshal(map[string]any{"schema": 1, "revision": "1", "organizations": items})
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		p.t.Fatal(err)
	}
	writeFile(p.t, client.OpenAIOrganizationsPath(p.path), raw)
}

// openAIParts is the running cache's OpenAI lane.
func (p *meterPlugin) openAIParts() laneParts {
	p.t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	parts := p.openai.partsLocked()
	if parts.store == nil || parts.organizations == nil || parts.publisher == nil {
		p.t.Fatal("the cache is not running")
	}
	return parts
}

// openAIOrganizationOps sends one OpenAI organizations batch from the current
// revision and requires it to be accepted.
func (p *meterPlugin) openAIOrganizationOps(ops ...map[string]any) map[string]any {
	p.t.Helper()
	revision := settings.RevisionText(p.openAIParts().organizations.Current().Revision)
	code, body, _ := p.post(map[string]any{"kind": settings.KindOpenAIOrganizations, "baseRevision": revision, "ops": ops})
	if code != http.StatusOK {
		p.t.Fatalf("OpenAI organizations batch %v: %d %v", ops, code, body)
	}
	return body
}

func openAIRemoveOp(slug string) map[string]any {
	return map[string]any{"op": settings.OpRemove, "id": client.OpenAICreditOrgAccount(slug)}
}

// openAIMeterFile is the OpenAI meter file, nil when there is none.
func (p *meterPlugin) openAIMeterFile() *client.APIMeter {
	p.t.Helper()
	meter, err := client.LoadMeter(client.OpenAIMeterPath(p.path))
	if errors.Is(err, client.ErrMeterMissing) {
		return nil
	}
	if err != nil {
		p.t.Fatalf("OpenAI meter file: %v", err)
	}
	return meter
}

// openAICreditsFile is the OpenAI credit state file, nil when there is none.
func (p *meterPlugin) openAICreditsFile() *client.CreditState {
	p.t.Helper()
	state, err := client.LoadCredits(client.OpenAICreditsPath(p.path))
	if errors.Is(err, client.ErrCreditsMissing) {
		return nil
	}
	if err != nil {
		p.t.Fatalf("OpenAI credits file: %v", err)
	}
	return state
}

// openAIPublished publishes the OpenAI lane now and returns its credit state
// file, nil when there is none.
func (p *meterPlugin) openAIPublished() *client.CreditState {
	p.t.Helper()
	p.openAIParts().publisher.PublishNow()
	return p.openAICreditsFile()
}

// openAIFiles names every snapshot.openai-*.json file beside the snapshot.
func (p *meterPlugin) openAIFiles() []string {
	p.t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(p.path), "snapshot.openai-*"))
	if err != nil {
		p.t.Fatal(err)
	}
	names := []string{}
	for _, match := range matches {
		names = append(names, filepath.Base(match))
	}
	return names
}

func (p *meterPlugin) quiesce() {
	p.t.Helper()
	if _, err := p.Handle(protocol.MethodPluginQuiesce, nil); err != nil {
		p.t.Fatal(err)
	}
}

// said is how many times the plugin has logged message.
func (p *meterPlugin) said(message string) int {
	said := 0
	for _, line := range p.host.logged() {
		if line.message == message {
			said++
		}
	}
	return said
}

// wantOpenAIIDs requires an answer's organizations to be the slugs given, in
// order, each with its OpenAI account id and never a Claude one.
func wantOpenAIIDs(t *testing.T, name string, organizations any, slugs ...string) {
	t.Helper()
	listed, _ := organizations.([]any)
	if len(listed) != len(slugs) {
		t.Fatalf("%s: organizations %v, want %v", name, organizations, slugs)
	}
	for i, slug := range slugs {
		item, _ := listed[i].(map[string]any)
		id, _ := item["id"].(string)
		if item["organization_id"] != slug || id != client.OpenAICreditOrgAccount(slug) || !regexp.MustCompile(`^oai-[0-9a-f]{12}$`).MatchString(id) {
			t.Fatalf("%s: organization %d %v, want %s as %s", name, i, item, slug, client.OpenAICreditOrgAccount(slug))
		}
	}
}

// Every usage record CPA sends is offered to both meters, and each keeps only
// its own source's: Claude API-key traffic for the Claude meter, and
// openai-compatibility traffic that names an OpenAI organization for the
// OpenAI one. Each counts as received every record it was offered.
func TestUsageRecordsReachBothMeters(t *testing.T) {
	p := newMeterPlugin(t)
	p.writeOrganizations([2]string{"sierra", fakeOrgA})
	p.writeOpenAIOrganizations([2]string{"pro", proSlug})
	p.configure(protocol.MethodPluginRegister, "")
	p.usage(goldenRecord(t))
	p.usage(openAIRecord(t))
	p.usage(openAIRefusal(t))
	p.Shutdown()
	claude := p.meterFile()
	if claude.Received != 3 || claude.Counted != 1 || len(claude.Unlinked) != 0 || !slices.Equal(slices.Sorted(maps.Keys(claude.Organizations)), []string{fakeOrgA}) {
		t.Fatalf("the Claude meter: %+v", claude)
	}
	openAI := p.openAIMeterFile()
	if openAI == nil || openAI.Received != 3 || openAI.Counted != 1 || !slices.Equal(slices.Sorted(maps.Keys(openAI.Organizations)), []string{proSlug}) {
		t.Fatalf("the OpenAI meter: %+v", openAI)
	}
	pro := openAI.Organizations[proSlug]
	if len(pro.Days) != 1 || len(pro.Days[0].Usage) != 1 {
		t.Fatalf("pro: %+v", pro)
	}
	if usage := pro.Days[0].Usage[0]; usage.Model != "gpt-6-sol-2026-08-01" || usage.Prompt != client.MeterPromptOver272K || usage.Tier != client.MeterTierFlex || usage.Requests != 1 || usage.Input != 50000 {
		t.Fatalf("pro's usage: %+v", usage)
	}
	if len(openAI.Unlinked) != 1 || openAI.Unlinked[0].OrganizationID != acmeSlug || openAI.Unlinked[0].Requests != 1 {
		t.Fatalf("the OpenAI meter's unlinked: %+v", openAI.Unlinked)
	}
	if len(openAI.AuthIDs) != 0 || openAI.AuthIDsSince != nil || openAI.Foreign != 0 || openAI.Unattributed != 0 {
		t.Fatalf("the OpenAI meter kept what only Claude's keeps: %+v", openAI)
	}
}

// With no OpenAI organization listed the OpenAI meter still runs while the
// cache does, whatever Claude's list holds: it counts nothing, notes each
// organization OpenAI's responses name, and writes no file until it has noted
// one, so the page can offer the first organization to add. Removing the last
// OpenAI organization leaves it watching, never stopped with no_items; only
// the cache's own stops stop it. Claude's meter keeps its own rule.
func TestTheOpenAIMeterWatchesWithNoOrganizationListed(t *testing.T) {
	p := newMeterPlugin(t)
	p.configure(protocol.MethodPluginRegister, "")
	if p.openai.meter.Load() == nil || p.claude.meter.Load() != nil {
		t.Fatalf("with nothing listed: OpenAI meter %v, Claude meter %v", p.openai.meter.Load(), p.claude.meter.Load())
	}
	if said := p.said(openAICounting); said != 1 {
		t.Fatalf("said %d times that the OpenAI meter is counting", said)
	}
	// Claude traffic, and openai-compatibility answers that name no
	// organization, as another upstream's do, are nothing to keep.
	p.usage(goldenRecord(t))
	p.usage(headerless(t, openAIRecord(t)))
	p.usage(headerless(t, openAIRefusal(t)))
	p.quiesce()
	if files := p.openAIFiles(); len(files) != 0 {
		t.Fatalf("with nothing seen, wrote %v", files)
	}

	// A response that names an organization is noted, and nothing is
	// counted.
	p.configure(protocol.MethodPluginRegister, "")
	p.usage(openAIRecord(t))
	p.quiesce()
	file := p.openAIMeterFile()
	if file == nil || file.Counted != 0 || len(file.Organizations) != 0 || file.StopReason != client.MeterStopQuiesce ||
		len(file.Unlinked) != 1 || file.Unlinked[0].OrganizationID != proSlug || file.Unlinked[0].Requests != 1 {
		t.Fatalf("after a response named pro: %+v", file)
	}
	// Nothing is listed or saved, so there is no OpenAI state to publish.
	if files := p.openAIFiles(); !slices.Equal(files, []string{"snapshot.openai-meter.json"}) {
		t.Fatalf("files %v", files)
	}

	p.configure(protocol.MethodPluginRegister, "")
	watching := p.openai.meter.Load()
	if watching == nil {
		t.Fatal("no OpenAI meter")
	}
	// Claude's list starts and stops Claude's meter, and leaves this one be.
	p.organizationOps(addOp("alpha", fakeOrgA))
	if p.claude.meter.Load() == nil || p.openai.meter.Load() != watching {
		t.Fatal("adding a Claude organization")
	}
	p.removeAll()
	if p.claude.meter.Load() != nil || p.openai.meter.Load() != watching {
		t.Fatal("removing the last Claude organization")
	}
	// Listing an OpenAI organization links it in the same meter, and
	// removing the last one leaves that meter watching.
	p.openAIOrganizationOps(addOp("pro", proSlug))
	if p.openai.meter.Load() != watching {
		t.Fatal("adding an OpenAI organization replaced the meter")
	}
	p.openAIOrganizationOps(openAIRemoveOp(proSlug))
	if p.openai.meter.Load() != watching {
		t.Fatal("removing the last OpenAI organization stopped the meter")
	}
	p.usage(openAIRefusal(t))
	p.configure(protocol.MethodPluginReconfigure, "enabled: false\n")
	if p.openai.meter.Load() != nil {
		t.Fatal("the OpenAI meter outlived the cache")
	}
	file = p.openAIMeterFile()
	if file.StopReason != client.MeterStopDisabled || len(file.Gaps) != 1 || file.Gaps[0].Reason != client.MeterStopQuiesce || file.Counted != 0 {
		t.Fatalf("after disabled: %+v", file)
	}
	if pro, kept := file.Organizations[proSlug]; !kept || pro.UnlinkedAt == nil {
		t.Fatalf("pro after its removal: %+v", file.Organizations)
	}
	if len(file.Unlinked) != 1 || file.Unlinked[0].OrganizationID != acmeSlug {
		t.Fatalf("unlinked after the refusal: %+v", file.Unlinked)
	}
	if claude := p.meterFile(); claude.StopReason != client.MeterStopNoItems {
		t.Fatalf("the Claude meter: %+v", claude)
	}
}

// An OpenAI organization added while the meter is already watching is counted
// from its add, as a Claude organization added while the Claude meter runs
// for others is: the traffic it sent before is not in the meter, the meter
// keeps its own start, and the credit says counting began late.
func TestAnOpenAIOrganizationAddedWhileWatchingIsCountedFromItsAdd(t *testing.T) {
	p := newMeterPlugin(t)
	now := time.Now().UTC().Truncate(time.Second)
	since, stopped := now.Add(-72*time.Hour), now.Add(-time.Hour)
	// The file a watching run left: it began three days ago and saw pro.
	raw, _ := json.Marshal(client.APIMeter{
		Schema: client.MeterSchema, Since: since, StartedAt: since, FlushedAt: stopped, StoppedAt: &stopped, StopReason: client.MeterStopQuiesce,
		Gaps: []client.MeterGap{}, Organizations: map[string]client.MeterOrganization{},
		Unlinked: []client.MeterUnlinked{{OrganizationID: proSlug, FirstSeenAt: since, LastSeenAt: stopped, Requests: 3}},
		Auths:    map[string]client.MeterAuth{}, AuthIDs: map[string]client.MeterAuthID{},
	})
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, client.OpenAIMeterPath(p.path), raw)
	p.configure(protocol.MethodPluginRegister, "")
	waitFor(t, p.path, func(s client.Snapshot) bool { return !s.WrittenAt.IsZero() })
	// Before the add: noted, not counted. Quiescing drains the meter's intake,
	// so the record is applied before the add, and registering watches again.
	p.usage(openAIRecord(t))
	p.quiesce()
	p.configure(protocol.MethodPluginRegister, "")
	waitFor(t, p.path, func(s client.Snapshot) bool { return !s.WrittenAt.IsZero() })
	added := time.Now().UTC().Truncate(time.Second)
	p.openAIOrganizationOps(addOp("pro", proSlug))
	p.usage(openAIRecord(t))
	renews := added.AddDate(0, 0, -2).Format(time.DateOnly)
	if code, body, _ := p.post(batchOf(settings.KindOpenAIAPICredits, creditRow(proAccount, "", "200", renews, nil))); code != http.StatusOK {
		t.Fatalf("pro's credit: %d %v", code, body)
	}
	p.quiesce()
	file := p.openAIMeterFile()
	pro, listed := file.Organizations[proSlug]
	if !listed || !file.Since.Equal(since) || pro.Since.Before(added) || pro.UnlinkedAt != nil || file.Counted != 1 || len(file.Unlinked) != 0 {
		t.Fatalf("meter %+v, pro %+v, added at %v", file, pro, added)
	}
	requests := uint64(0)
	for _, day := range pro.Days {
		for _, usage := range day.Usage {
			requests += usage.Requests
		}
	}
	if requests != 1 {
		t.Fatalf("pro has %d requests, want only the one after its add", requests)
	}
	account := accountIn(t, p.openAICreditsFile(), proAccount)
	if account.Basis != client.CreditBasisCredit || !slices.Contains(account.DataIssues, client.CreditIssueMeterStartedLate) {
		t.Fatalf("pro's account: %+v", account)
	}
}

// The save route takes the OpenAI cards' kinds with the same checks, codes and
// answers as Claude's, on the OpenAI files: the organizations card's answers
// carry OpenAI account ids, never Claude's; a row of one lane is not editable
// on the other's card; and a file of one lane that cannot be read refuses only
// the kinds that need it.
func TestOpenAISaveRoute(t *testing.T) {
	p := newRoutePlugin(t)
	openAIOrganizations := func(base string, ops ...map[string]any) map[string]any {
		return map[string]any{"kind": settings.KindOpenAIOrganizations, "baseRevision": base, "ops": ops}
	}

	// The organizations card.
	body := p.openAIOrganizationOps(addOp("pro", proSlug), addOp("acme", acmeSlug))
	wantOpenAIIDs(t, "the add", body["organizations"], proSlug, acmeSlug)
	read, _ := body["revision"].(string)
	for _, tc := range []struct {
		name, code, field string
		op                map[string]any
	}{
		{"a slug with a space", settings.CodeInvalidOrganizationID, "organizationId", addOp("corp", "acme corp")},
		{"a slug that starts with a dash", settings.CodeInvalidOrganizationID, "organizationId", addOp("dash", "-acme")},
		{"a slug listed already", settings.CodeDuplicateOrganization, "organizationId", addOp("again", proSlug)},
		{"a label listed already", settings.CodeDuplicateLabel, "label", addOp("pro", sideSlug)},
		{"a Claude account id", settings.CodeUnknownID, "id", removeOp(fakeOrgA)},
	} {
		if answer := p.wantError(tc.name, http.StatusBadRequest, tc.code, openAIOrganizations(read, tc.op)); answer["op"] != 0.0 || answer["field"] != tc.field {
			t.Fatalf("%s: %v", tc.name, answer)
		}
	}
	// Two slugs that differ only in case are two organizations.
	p.openAIOrganizationOps(addOp("pro upper", strings.ToUpper(proSlug)))
	body = p.openAIOrganizationOps(openAIRemoveOp(strings.ToUpper(proSlug)), openAIRemoveOp(acmeSlug))
	wantOpenAIIDs(t, "the removal", body["organizations"], proSlug)
	conflict := p.wantError("a stale batch", http.StatusConflict, "conflict", openAIOrganizations(read, addOp("side", sideSlug)))
	wantOpenAIIDs(t, "the conflict", conflict["organizations"], proSlug)
	// Each organizations card takes only its own lane's form.
	p.wantError("a slug on Claude's card", http.StatusBadRequest, settings.CodeInvalidOrganizationID,
		map[string]any{"kind": settings.KindOrganizations, "baseRevision": p.organizationsRevision(), "ops": []any{addOp("pro", proSlug)}})
	if items := p.statusOf()["editor"].(map[string]any)["organizations"].(map[string]any)["items"].([]any); len(items) != 1 {
		t.Fatalf("Claude's organizations %v", items)
	}

	// The API credits card.
	p.openAIPublished()
	renews := time.Now().UTC().AddDate(0, 0, -2).Format(time.DateOnly)
	code, body, _ := p.post(batchOf(settings.KindOpenAIAPICredits, creditRow(proAccount, "", "200", renews, nil)))
	block, _ := body["settings"].(map[string]any)[proAccount].(map[string]any)
	saved, _ := body["revision"].(string)
	if code != http.StatusOK || block["monthlyUsd"] != "200" || block["revision"] != saved {
		t.Fatalf("pro's credit: %d %v", code, body)
	}
	if account := accountIn(t, p.openAICreditsFile(), proAccount); account.Settings.MonthlyUSD != "200" || account.OrganizationID != proSlug {
		t.Fatalf("published %+v", account)
	}
	if raw, _ := os.ReadFile(settings.FilePath(p.path)); strings.Contains(string(raw), "oai-") {
		t.Fatal("an OpenAI value reached Claude's settings file")
	}
	p.wantError("a monthly credit below zero", http.StatusBadRequest, settings.CodeMonthlyUSD,
		batchOf(settings.KindOpenAIAPICredits, creditRow(proAccount, saved, "-1", nil, nil)))
	stale := p.wantError("a stale row", http.StatusConflict, "conflict", batchOf(settings.KindOpenAIAPICredits, creditRow(proAccount, "", "300", nil, nil)))
	if current, _ := stale["current"].(map[string]any)[proAccount].(map[string]any); !slices.Equal(anyStrings(stale["conflicts"]), []string{proAccount}) || current["monthlyUsd"] != "200" {
		t.Fatalf("the conflict: %v", stale)
	}
	p.wantError("Claude's account on the OpenAI card", http.StatusConflict, "not_editable",
		batchOf(settings.KindOpenAIAPICredits, creditRow(alphaAccount, "", "300", nil, nil)))
	p.wantError("the OpenAI account on Claude's card", http.StatusConflict, "not_editable",
		batchOf(settings.KindAPICredits, creditRow(proAccount, "", "300", nil, nil)))
	// A reading keeps the organization as the published state names it.
	at := time.Now().UTC().Add(-time.Minute)
	if code, body, _ := p.post(batchOf(settings.KindOpenAIAPICredits, creditRow(proAccount, saved, "200", renews, readingOf("150.00", at)))); code != http.StatusOK {
		t.Fatalf("pro's reading: %d %v", code, body)
	}
	if reading := p.openAIParts().store.Current().APICredits[proAccount].Reading; reading == nil || reading.OrganizationID != proSlug || reading.RemainingUSD != "150.00" {
		t.Fatalf("the stored reading %+v", reading)
	}
	if account := accountIn(t, p.openAICreditsFile(), proAccount); account.Settings.Reading == nil || account.Settings.Reading.RemainingUSD != "150.00" {
		t.Fatalf("published %+v", account)
	}
	saved = settings.RevisionText(p.openAIParts().store.Current().Revision)

	// An OpenAI organizations file that cannot be read refuses both OpenAI
	// cards' rows, and leaves Claude's cards be.
	good, err := os.ReadFile(client.OpenAIOrganizationsPath(p.path))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, client.OpenAIOrganizationsPath(p.path), []byte(`{`))
	p.openAIParts().publisher.tick(time.Now().UTC())
	p.wantError("the OpenAI organizations unreadable", http.StatusServiceUnavailable, "organizations_unavailable", openAIOrganizations("", addOp("side", sideSlug)))
	p.wantError("a held OpenAI row", http.StatusServiceUnavailable, "organizations_unavailable",
		batchOf(settings.KindOpenAIAPICredits, creditRow(proAccount, saved, "300", nil, nil)))
	p.organizationOps(addOp("bravo", fakeOrgB))
	writeFile(t, client.OpenAIOrganizationsPath(p.path), good)
	p.openAIParts().publisher.tick(time.Now().UTC())

	// An OpenAI settings file that cannot be read refuses only the OpenAI
	// API credits card.
	writeFile(t, client.OpenAISettingsPath(p.path), []byte(`{"schema":2}`))
	p.openAIParts().publisher.tick(time.Now().UTC())
	p.wantError("the OpenAI settings unreadable", http.StatusServiceUnavailable, "settings_unavailable",
		batchOf(settings.KindOpenAIAPICredits, creditRow(proAccount, saved, "300", nil, nil)))
	if editor := p.statusOf()["openai_editor"].(map[string]any); editor["available"] != false || editor["reason"] != client.CreditEditingSettingsUnreadable {
		t.Fatalf("the OpenAI editor %v", editor)
	}
	if code, body, _ := p.post(batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", "300", nil, nil))); code != http.StatusOK {
		t.Fatalf("Claude's credit: %d %v", code, body)
	}
	if code, body, _ := p.post(batchOf(settings.KindRenewals, renewalRow(claudeHexID, "", "2026-09-17"))); code != http.StatusOK {
		t.Fatalf("a renewal date: %d %v", code, body)
	}
	p.openAIOrganizationOps(addOp("side", sideSlug))
	p.configure(protocol.MethodPluginReconfigure, "")
	if said := p.said("quota-cache cannot read its saved OpenAI API credit settings; none apply until the file is moved aside"); said != 1 {
		t.Fatalf("warned %d times about the OpenAI settings file", said)
	}
	if said := p.said("quota-cache cannot read its saved API credit settings; none apply until the file is moved aside"); said != 0 {
		t.Fatalf("warned %d times about Claude's settings file", said)
	}

	// Each lane logs its own lines, with kinds, account ids and counts, and
	// never a slug, label or amount.
	changes := []map[string]any{}
	kinds := []any{}
	for _, line := range p.host.logged() {
		switch line.message {
		case "quota-cache OpenAI organizations changed":
			changes = append(changes, line.fields)
		case "quota-cache settings changed":
			kinds = append(kinds, line.fields["kind"])
		}
	}
	if len(changes) != 4 || changes[0]["added"] != 2 || changes[2]["removed"] != 2 {
		t.Fatalf("OpenAI organizations changes %v", changes)
	}
	if want := []any{settings.KindOpenAIAPICredits, settings.KindOpenAIAPICredits, settings.KindAPICredits, settings.KindRenewals}; !slices.Equal(kinds, want) {
		t.Fatalf("settings changes %v, want %v", kinds, want)
	}
	p.noValueLogged(proSlug, strings.ToUpper(proSlug), acmeSlug, sideSlug, "pro upper", "150.00")
}

func anyStrings(v any) []string {
	out := []string{}
	list, _ := v.([]any)
	for _, item := range list {
		s, _ := item.(string)
		out = append(out, s)
	}
	return out
}

// A Claude-only install writes no file of the OpenAI API credit: with no
// OpenAI organization listed, no OpenAI value saved and no response naming an
// OpenAI organization, nothing is added beside the snapshot through traffic,
// publishes, status reads, saves and every kind of stop, and the status shows
// no OpenAI meter and no OpenAI state.
func TestAClaudeOnlyInstallWritesNoOpenAIFile(t *testing.T) {
	p := newRoutePlugin(t)
	p.usage(goldenRecord(t))
	p.usage(headerless(t, openAIRecord(t)))
	p.usage(headerless(t, openAIRefusal(t)))
	if code, body, _ := p.post(batchOf(settings.KindAPICredits, creditRow(alphaAccount, "", "300", nil, nil))); code != http.StatusOK {
		t.Fatalf("%d %v", code, body)
	}
	if state := p.openAIPublished(); state != nil {
		t.Fatalf("published %+v", state)
	}
	p.openAIParts().publisher.tick(time.Now().UTC())
	status := p.statusOf()
	if _, has := status["openai_meter"]; has || status["openai_credits"] != nil {
		t.Fatalf("status openai_meter %v, openai_credits %v", status["openai_meter"], status["openai_credits"])
	}
	p.configure(protocol.MethodPluginReconfigure, "enabled: false\n")
	p.configure(protocol.MethodPluginReconfigure, "")
	p.published()
	p.quiesce()
	p.configure(protocol.MethodPluginRegister, "")
	p.published()
	p.Shutdown()
	if files := p.openAIFiles(); len(files) != 0 {
		t.Fatalf("wrote %v", files)
	}
	raw, err := os.ReadFile(client.CreditsPath(p.path))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"provider"`) || strings.Contains(string(raw), "oai-") {
		t.Fatalf("the Claude credit state names a provider: %s", raw)
	}
}

// The OpenAI credit state is a file of its own beside the snapshot, in the
// credit state's schema, naming its provider. Claude's state is the same with
// OpenAI organizations listed, saved and counted as without.
func TestOpenAIStateIsPublishedBesideTheSnapshot(t *testing.T) {
	p := newRoutePlugin(t)
	p.configure(protocol.MethodPluginReconfigure, alphaCredit)
	// Claude's meter file holds still, so only OpenAI's lane changes.
	p.claude.meter.Load().Stop(client.MeterStopQuiesce, time.Now().UTC())
	claude := func() []byte {
		t.Helper()
		raw, err := json.Marshal(comparableOf(p.published()))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	before := claude()
	p.openAIOrganizationOps(addOp("pro", proSlug))
	if code, body, _ := p.post(batchOf(settings.KindOpenAIAPICredits, creditRow(proAccount, "", "200", nil, nil))); code != http.StatusOK {
		t.Fatalf("%d %v", code, body)
	}
	p.usage(openAIRecord(t))
	state := p.openAIPublished()
	if state == nil || state.Schema != 1 || state.APICredits == nil {
		t.Fatalf("published %+v", state)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(p.path), "snapshot.openai-credits.json")); err != nil {
		t.Fatal(err)
	}
	d := state.APICredits
	if d.Provider != client.ProviderOpenAIAPI || d.Title != "Monthly OpenAI API Credit" || d.Pricing.CacheWrites != "30m" ||
		len(d.Accounts) != 1 || d.Accounts[0].ID != proAccount || d.Accounts[0].OrganizationID != proSlug {
		t.Fatalf("the OpenAI document %+v", d)
	}
	if state.Organizations.ConfigItemsIgnored != 0 || state.Organizations.State != client.OrganizationsStateOK || len(state.Renewals) != 0 || state.Settings.State != client.CreditSettingsOK {
		t.Fatalf("the OpenAI state %+v %+v %+v", state.Organizations, state.Settings, state.Renewals)
	}
	if raw, _ := os.ReadFile(client.OpenAICreditsPath(p.path)); !strings.Contains(string(raw), `"renewals":{}`) || !strings.Contains(string(raw), `"api_credits":{"provider":"openai-api",`) {
		t.Fatalf("the OpenAI state file %s", raw)
	}
	if after := claude(); !bytes.Equal(before, after) {
		t.Fatalf("Claude's state changed:\n%s\n%s", before, after)
	}
	if state := p.creditsFile(); state.Organizations.ConfigItemsIgnored != 1 || state.APICredits.Provider != "" {
		t.Fatalf("Claude's state %+v %+v", state.Organizations, state.APICredits.Provider)
	}
}

// The status route carries the OpenAI meter file once there is one, the OpenAI
// state as a reader sees it, null while the lane publishes nothing, and the
// OpenAI editor, with the same keys as Claude's and no credential or renewal
// date.
func TestStatusCarriesOpenAI(t *testing.T) {
	p := newRoutePlugin(t)
	status := p.statusOf()
	if _, has := status["openai_meter"]; has {
		t.Fatalf("an OpenAI meter with no file: %v", status["openai_meter"])
	}
	if state, has := status["openai_credits"]; !has || state != nil {
		t.Fatalf("openai_credits %v, present %v", state, has)
	}
	editor, _ := status["editor"].(map[string]any)
	openAI, _ := status["openai_editor"].(map[string]any)
	if keys, want := slices.Sorted(maps.Keys(openAI)), slices.Sorted(maps.Keys(editor)); len(want) == 0 || !slices.Equal(keys, want) {
		t.Fatalf("openai_editor keys %v, editor keys %v", keys, want)
	}
	for key, want := range map[string]any{"available": true, "reason": "", "revision": "", "max_api_credit_items": 16.0, "max_renewal_items": 0.0, "max_batch_bytes": 4096.0} {
		if openAI[key] != want {
			t.Errorf("openai_editor %s %v, want %v", key, openAI[key], want)
		}
	}
	for _, key := range []string{"credentials", "renewal_orphans", "api_credit_orphans", "held_api_credits"} {
		if list, ok := openAI[key].([]any); !ok || len(list) != 0 {
			t.Errorf("openai_editor %s %v", key, openAI[key])
		}
	}
	if organizations := openAI["organizations"].(map[string]any); organizations["available"] != true || organizations["revision"] != "" || len(organizations["items"].([]any)) != 0 {
		t.Fatalf("openai_editor organizations %v", organizations)
	}

	p.openAIOrganizationOps(addOp("pro", proSlug))
	p.usage(openAIRecord(t))
	p.quiesce()
	p.configure(protocol.MethodPluginRegister, "")
	status = p.statusOf()
	meter, _ := status["openai_meter"].(map[string]any)
	if meter["schema"] != 1.0 || meter["organizations"].(map[string]any)[proSlug] == nil {
		t.Fatalf("openai_meter %v", status["openai_meter"])
	}
	if _, has := meter["auth_ids_since"]; has {
		t.Fatalf("openai_meter %v", meter)
	}
	state, _ := status["openai_credits"].(map[string]any)
	document, _ := state["api_credits"].(map[string]any)
	if document["provider"] != client.ProviderOpenAIAPI || len(document["accounts"].([]any)) != 1 || document["accounts"].([]any)[0].(map[string]any)["id"] != proAccount {
		t.Fatalf("openai_credits %v", status["openai_credits"])
	}
	wantOpenAIIDs(t, "openai_editor", status["openai_editor"].(map[string]any)["organizations"].(map[string]any)["items"], proSlug)
	// Claude's keys are as they were.
	if items := status["editor"].(map[string]any)["organizations"].(map[string]any)["items"].([]any); len(items) != 1 || items[0].(map[string]any)["id"] != alphaAccount {
		t.Fatalf("editor organizations %v", items)
	}
	if _, has := status["credit_state"].(map[string]any)["api_credits"].(map[string]any)["provider"]; has {
		t.Fatal("credit_state names a provider")
	}
}

// A stop stops both lanes in the same steps, each on Claude's lane and then
// OpenAI's: both meters record the stop's reason, both lanes' stores are
// closed before the poller stops, and both credit states written last say
// their meter stopped, before the cache lets go of its writer lock.
func TestStopStopsBothLanes(t *testing.T) {
	p := newRoutePlugin(t)
	p.openAIOrganizationOps(addOp("pro", proSlug))
	p.openAIPublished()
	for _, stop := range []struct {
		reason  string
		do      func()
		restart func()
	}{
		{client.MeterStopDisabled, func() { p.configure(protocol.MethodPluginReconfigure, "enabled: false\n") }, func() { p.configure(protocol.MethodPluginReconfigure, "") }},
		{client.MeterStopQuiesce, p.quiesce, func() { p.configure(protocol.MethodPluginRegister, "") }},
		{client.MeterStopShutdown, p.Shutdown, nil},
	} {
		claudeStore, claudeOrganizations := p.storesOf()
		openAI := p.openAIParts()
		steps := p.traceStops()
		stop.do()
		got := steps()
		order := []string{}
		for _, step := range got {
			if step != "write" {
				order = append(order, step)
			}
		}
		if !slices.Equal(order, []string{"meter", "settings", "stores closed", "poller", "publisher", "cache"}) || !slices.Equal(got[len(got)-3:], []string{"publisher", "write", "cache"}) {
			t.Fatalf("%s: steps %v", stop.reason, got)
		}
		storesClosed(t, stop.reason, claudeStore, claudeOrganizations)
		storesClosed(t, stop.reason+", OpenAI", openAI.store, openAI.organizations)
		if p.claude.meter.Load() != nil || p.openai.meter.Load() != nil {
			t.Fatalf("%s: a meter outlived the stop", stop.reason)
		}
		if file := p.meterFile(); file.StopReason != stop.reason {
			t.Fatalf("%s: the Claude meter %+v", stop.reason, file)
		}
		if file := p.openAIMeterFile(); file == nil || file.StopReason != stop.reason {
			t.Fatalf("%s: the OpenAI meter %+v", stop.reason, file)
		}
		for name, state := range map[string]*client.CreditState{"Claude": p.creditsFile(), "OpenAI": p.openAICreditsFile()} {
			if m := state.APICredits.Meter; m == nil || m.StoppedAtEpoch == nil || m.StopReason != stop.reason {
				t.Fatalf("%s: %s's last state says the meter %+v", stop.reason, name, m)
			}
		}
		if stop.restart != nil {
			stop.restart()
			p.published()
			p.openAIPublished()
		}
	}
}
