package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// OpenAI organizations are named as the openai-organization response header
// names them: a personal organization's slug, and a named one's.
const (
	openAIPro      = "user-AbCdEfGhIjKlMnOpQrStUvWx"
	openAIBusiness = "acme-x1wegt"
)

var openAIAccount = client.OpenAICreditOrgAccount(openAIPro)

// validOpenAIFile is an OpenAI settings file with one of everything it may
// hold: a slug organization, and a baseline with service tiers and OpenAI's
// long-context class.
func validOpenAIFile() map[string]any {
	return map[string]any{
		"schema":   1,
		"revision": 7,
		"apiCredits": map[string]any{
			openAIAccount: map[string]any{
				"monthlyUsd": "200",
				"renews":     "2026-09-28",
				"reading": map[string]any{
					"remainingUsd":   "143.20",
					"at":             "2026-10-09T13:20:00Z",
					"enteredAt":      "2026-10-09T13:24:10Z",
					"organizationId": openAIPro,
					"baseline": map[string]any{
						"dayStart": "2026-10-09T00:00:00Z",
						"until":    "2026-10-09T13:00:00Z",
						"usage": []any{
							map[string]any{"model": "gpt-6-sol", "requests": 3, "input": 1200, "output": 800},
							map[string]any{"model": "gpt-6-sol", "prompt": "over_272k", "tier": "flex", "requests": 1, "input": 300000},
						},
					},
				},
				"rev":       7,
				"updatedAt": "2026-10-09T13:24:10Z",
			},
		},
		"renewals": map[string]any{},
	}
}

func marshal(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// openAIMeterFor is an OpenAI meter flushed at flushed holding one slug's
// hours.
func openAIMeterFor(t *testing.T, slug, flushed string, hours ...client.MeterBucket) *client.APIMeter {
	return &client.APIMeter{Schema: 1, FlushedAt: at(t, flushed), Organizations: map[string]client.MeterOrganization{
		slug: {Since: at(t, "2026-09-01T00:00:00Z"), Days: []client.MeterBucket{}, Hours: hours},
	}}
}

// The OpenAI settings file has the settings file's schema with OpenAI's
// rules: "oai-" account ids, slug organizations kept exactly, OpenAI's prompt
// classes and service tiers, and never a renewal date. Each file loads under
// its own provider's rules and fails the other's.
func TestOpenAISettingsFileLoadRules(t *testing.T) {
	snapshot := snapshotIn(t.TempDir())
	path := client.OpenAISettingsPath(snapshot)
	if s := OpenFor(OpenAI, snapshot); s.Path() != path || s.Path() == Open(snapshot).Path() || exists(path) {
		t.Fatalf("the OpenAI store keeps %s", s.Path())
	}
	raw := marshal(t, validOpenAIFile())
	place(t, path, raw)
	v := LoadFor(OpenAI, path)
	credit := v.APICredits[openAIAccount]
	wantUsage := []client.MeterUsage{
		{Model: "gpt-6-sol", Requests: 3, Input: 1200, Output: 800},
		{Model: "gpt-6-sol", Prompt: client.MeterPromptOver272K, Tier: client.MeterTierFlex, Requests: 1, Input: 300000},
	}
	if v.Unreadable || v.Revision != 7 || len(v.Renewals) != 0 || credit.MonthlyUSD != "200" || credit.Reading == nil ||
		credit.Reading.OrganizationID != openAIPro || !reflect.DeepEqual(credit.Reading.Baseline.Usage, wantUsage) {
		t.Fatalf("values = %+v, reading %+v", v, credit.Reading)
	}
	if store := OpenFor(OpenAI, snapshot); !reflect.DeepEqual(store.Current(), v) || !store.Status().Editable() {
		t.Fatalf("opened as %+v, %+v", store.Current(), store.Status())
	}
	// It encodes, renewals as {}, to a file that loads the same.
	encoded, err := encode(v)
	if err != nil || !bytes.Contains(encoded, []byte(`"renewals": {}`)) {
		t.Fatalf("encoded %s, %v", encoded, err)
	}
	if again, err := decodeFor(OpenAI, encoded); err != nil || !reflect.DeepEqual(again, v) {
		t.Fatalf("round trip: %+v, %v", again, err)
	}
	// Each provider's rules refuse the other's file.
	if _, err := decode(raw); err == nil {
		t.Error("the Claude rules loaded the OpenAI file")
	}
	if !Load(path).Unreadable {
		t.Error("Load read the OpenAI file as Claude's")
	}
	if _, err := decodeFor(OpenAI, marshal(t, validFile())); err == nil {
		t.Error("the OpenAI rules loaded the Claude file")
	}
	// A file with no renewals key at all loads too.
	f := validOpenAIFile()
	delete(f, "renewals")
	if _, err := decodeFor(OpenAI, marshal(t, f)); err != nil {
		t.Errorf("no renewals key: %v", err)
	}

	// Every prompt class and service tier the OpenAI meter writes loads.
	for _, prompt := range []string{"", client.MeterPromptOver272K} {
		for _, tier := range []string{"", client.MeterTierFlex, client.MeterTierFast, client.MeterTierUltrafast, client.MeterTierUnknown} {
			f := validOpenAIFile()
			baselineOf(f, openAIAccount)["usage"] = []any{map[string]any{"model": "gpt-6-sol", "prompt": prompt, "tier": tier, "input": 1}}
			if _, err := decodeFor(OpenAI, marshal(t, f)); err != nil {
				t.Errorf("prompt %q tier %q: %v", prompt, tier, err)
			}
		}
	}

	// Each rule, one at a time: the whole file is unreadable, applies
	// nothing, and is never written over.
	apiCredit := func(f map[string]any) map[string]any {
		return f["apiCredits"].(map[string]any)[openAIAccount].(map[string]any)
	}
	reading := func(f map[string]any) map[string]any { return apiCredit(f)["reading"].(map[string]any) }
	usage := func(f map[string]any, entry map[string]any) { baselineOf(f, openAIAccount)["usage"] = []any{entry} }
	for name, break_ := range map[string]func(map[string]any){
		"a Claude account id":      func(f map[string]any) { f["apiCredits"] = map[string]any{accountA: apiCredit(f)} },
		"an upper-case account id": func(f map[string]any) { f["apiCredits"] = map[string]any{strings.ToUpper(openAIAccount): apiCredit(f)} },
		"an account id too short":  func(f map[string]any) { f["apiCredits"] = map[string]any{openAIAccount[:15]: apiCredit(f)} },
		"a renewal date":           func(f map[string]any) { f["renewals"] = validFile()["renewals"] },
		"a renewal with no date": func(f map[string]any) {
			f["renewals"] = map[string]any{renewalA: map[string]any{"rev": 5, "updatedAt": "2026-10-09T13:00:00Z"}}
		},
		"an organization with space": func(f map[string]any) { reading(f)["organizationId"] = "acme corp" },
		"an organization padded":     func(f map[string]any) { reading(f)["organizationId"] = " " + openAIBusiness },
		"an organization with a dash first": func(f map[string]any) {
			reading(f)["organizationId"] = "-" + openAIBusiness
		},
		"an organization of 65 bytes": func(f map[string]any) { reading(f)["organizationId"] = "a" + strings.Repeat("b", 64) },
		"an empty organization":       func(f map[string]any) { reading(f)["organizationId"] = "" },
		"Claude's long-context class": func(f map[string]any) {
			usage(f, map[string]any{"model": "gpt-6-sol", "prompt": client.MeterPromptOver100K, "input": 1})
		},
		"a tier as the response names it": func(f map[string]any) {
			usage(f, map[string]any{"model": "gpt-6-sol", "tier": "priority", "input": 1})
		},
		"the scale tier": func(f map[string]any) { usage(f, map[string]any{"model": "gpt-6-sol", "tier": "scale", "input": 1}) },
		"an upper-case tier": func(f map[string]any) {
			usage(f, map[string]any{"model": "gpt-6-sol", "tier": "Flex", "input": 1})
		},
		"a model not normalized": func(f map[string]any) { usage(f, map[string]any{"model": "GPT-6-Sol", "input": 1}) },
	} {
		dir := t.TempDir()
		f := validOpenAIFile()
		break_(f)
		raw := marshal(t, f)
		place(t, client.OpenAISettingsPath(snapshotIn(dir)), raw)
		s := OpenFor(OpenAI, snapshotIn(dir))
		if v := s.Current(); !v.Unreadable || len(v.APICredits) != 0 || s.Status().State != client.CreditSettingsUnreadable {
			t.Errorf("%s: loaded as %+v", name, v)
			continue
		}
		save := Batch{Kind: KindOpenAIAPICredits, APICredits: []APICreditItem{{ID: openAIAccount, MonthlyUSD: ptr("100"), OrganizationID: openAIPro}}}
		if _, err := s.Apply(save, nil, at(t, "2026-10-09T14:00:00Z")); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: a save over an unreadable file answered %v", name, err)
		}
		if after := readFile(t, s.Path()); !bytes.Equal(after, raw) {
			t.Errorf("%s: the unreadable file was overwritten", name)
		}
	}
}

func baselineOf(f map[string]any, account string) map[string]any {
	credit := f["apiCredits"].(map[string]any)[account].(map[string]any)
	return credit["reading"].(map[string]any)["baseline"].(map[string]any)
}

// A save to the OpenAI store writes the OpenAI file, with the reading's slug
// kept exactly, and only what OpenAI's rules load: a reading for an
// organization in another form, or a renewal date, is never written.
func TestOpenAISettingsSaveRoundTrips(t *testing.T) {
	snapshot := snapshotIn(t.TempDir())
	s := OpenFor(OpenAI, snapshot)
	now := at(t, "2026-10-09T14:00:00Z")
	reading := &ReadingInput{RemainingUSD: "143.20", At: "2026-10-09T13:20:00Z"}
	item := APICreditItem{ID: openAIAccount, MonthlyUSD: ptr("200"), Renews: ptr("2026-09-28"), Reading: reading, OrganizationID: openAIPro}
	result, err := s.Apply(Batch{Kind: KindOpenAIAPICredits, APICredits: []APICreditItem{item}}, nil, now)
	if err != nil || result.Revision != uint64(now.Unix()) {
		t.Fatalf("saved %+v, %v", result, err)
	}
	raw := readFile(t, client.OpenAISettingsPath(snapshot))
	if !bytes.Contains(raw, []byte(`"renewals": {}`)) || !bytes.Contains(raw, []byte(`"organizationId": "`+openAIPro+`"`)) || exists(FilePath(snapshot)) {
		t.Fatalf("wrote %s", raw)
	}
	if again := LoadFor(OpenAI, s.Path()); again.Unreadable || !reflect.DeepEqual(again, s.Current()) {
		t.Fatalf("reloaded %+v, want %+v", again, s.Current())
	}
	if again := OpenFor(OpenAI, snapshot).Current(); !reflect.DeepEqual(again, s.Current()) {
		t.Fatalf("reopened %+v", again)
	}

	// A reading's organization must be a slug in its normalized form.
	for _, org := range []string{"", "acme corp", " " + openAIBusiness, "-acme"} {
		s := OpenFor(OpenAI, snapshotIn(t.TempDir()))
		bad := item
		bad.OrganizationID = org
		var refused *NotEditableError
		if _, err := s.Apply(Batch{Kind: KindOpenAIAPICredits, APICredits: []APICreditItem{bad}}, nil, now); !errors.As(err, &refused) || exists(s.Path()) {
			t.Errorf("organization %q: %v", org, err)
		}
	}

	// No save writes a renewal date into the OpenAI file: the commit's own
	// load check refuses it, and the file stays as it was.
	before := readFile(t, s.Path())
	if _, err := s.Apply(renewalSave(renewalA, "", "2026-11-01"), nil, now.Add(time.Second)); !errors.Is(err, ErrUnwritable) {
		t.Fatalf("a renewal date: %v", err)
	}
	if after := readFile(t, s.Path()); !bytes.Equal(after, before) || len(s.Current().Renewals) != 0 {
		t.Fatalf("a renewal date was written: %s", after)
	}
}

// The OpenAI organizations file has the organizations file's schema with
// OpenAI's rules: each organization a slug in NormalizeOpenAIOrganization's
// form, unique by exact match, so two that differ only in case are two, and
// each keyed by its "oai-" account id.
func TestOpenAIOrganizationsFile(t *testing.T) {
	snapshot := snapshotIn(t.TempDir())
	s := OpenOrganizationsFor(OpenAI, snapshot)
	if s.Path() != client.OpenAIOrganizationsPath(snapshot) || OpenAI.OrganizationsPath(snapshot) != s.Path() ||
		Claude.OrganizationsPath(snapshot) != client.OrganizationsPath(snapshot) || s.Current().State != client.OrganizationsStateMissing {
		t.Fatalf("the OpenAI store keeps %s, %+v", s.Path(), s.Current())
	}
	lower := strings.ToLower(openAIPro)
	typedID := "org-AbCdEfGhIjKlMnOpQrStUvWx"
	result, err := s.Apply(OrganizationsBatch{Ops: []OrganizationOp{
		addOp("Pro", openAIPro), addOp("Pro lower", lower), addOp("Business", " "+openAIBusiness+"\t"), addOp("Typed", typedID),
	}}, orgNow)
	if err != nil || result.Added != 4 || result.Revision != orgFirst {
		t.Fatalf("added %+v, %v", result, err)
	}
	items := s.Current().Items
	want := []Organization{
		{Label: "Pro", OrganizationID: openAIPro, Rev: orgFirst, UpdatedAt: orgNow},
		{Label: "Pro lower", OrganizationID: lower, Rev: orgFirst, UpdatedAt: orgNow},
		{Label: "Business", OrganizationID: openAIBusiness, Rev: orgFirst, UpdatedAt: orgNow},
		{Label: "Typed", OrganizationID: typedID, Rev: orgFirst, UpdatedAt: orgNow},
	}
	if !reflect.DeepEqual(items, want) {
		t.Fatalf("items\n%+v\nwant\n%+v", items, want)
	}
	// The account ids are OpenAI's, and differ with the case.
	seen := map[string]bool{}
	for _, item := range items {
		id := OpenAI.AccountOf(item)
		if id != client.OpenAICreditOrgAccount(item.OrganizationID) || !strings.HasPrefix(id, client.OpenAICreditAccountPrefix) ||
			!OpenAI.ValidAPICreditID(id) || Claude.ValidAPICreditID(id) || ValidAPICreditID(id) || seen[id] {
			t.Errorf("%s: account %s", item.OrganizationID, id)
		}
		seen[id] = true
		if Claude.AccountOf(item) != item.ID() || OpenAI.ValidAPICreditID(item.ID()) {
			t.Errorf("%s: Claude account %s", item.OrganizationID, item.ID())
		}
	}

	// What the rules refuse, each with its code; nothing is written.
	before := readFile(t, s.Path())
	base := RevisionText(orgFirst)
	for name, tc := range map[string]struct {
		op          OrganizationOp
		code, field string
	}{
		"the same slug":           {addOp("Again", openAIPro), CodeDuplicateOrganization, "organizationId"},
		"the same slug, padded":   {addOp("Again", " "+openAIBusiness), CodeDuplicateOrganization, "organizationId"},
		"a space inside":          {addOp("Again", "acme corp"), CodeInvalidOrganizationID, "organizationId"},
		"a dash first":            {addOp("Again", "-acme"), CodeInvalidOrganizationID, "organizationId"},
		"65 bytes":                {addOp("Again", "a"+strings.Repeat("b", 64)), CodeInvalidOrganizationID, "organizationId"},
		"blank":                   {addOp("Again", " "), CodeInvalidOrganizationID, "organizationId"},
		"not ASCII":               {addOp("Again", "acmé-x1wegt"), CodeInvalidOrganizationID, "organizationId"},
		"a rename by Claude's id": {OrganizationOp{Op: OpRename, ID: items[0].ID(), Label: "x"}, CodeUnknownID, "id"},
		"a remove by Claude's id": {OrganizationOp{Op: OpRemove, ID: items[0].ID()}, CodeUnknownID, "id"},
	} {
		_, err := s.Apply(OrganizationsBatch{BaseRevision: base, Ops: []OrganizationOp{tc.op}}, orgNow.Add(time.Second))
		var fail *OpError
		if !errors.As(err, &fail) || *fail != (OpError{Code: tc.code, Op: 0, Field: tc.field}) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if after := readFile(t, s.Path()); !bytes.Equal(after, before) {
		t.Fatal("a refused batch wrote the file")
	}
	// A rename and a remove name the entry by its OpenAI account id.
	result, err = s.Apply(OrganizationsBatch{BaseRevision: base, Ops: []OrganizationOp{
		{Op: OpRename, ID: client.OpenAICreditOrgAccount(openAIPro), Label: "Personal"},
		{Op: OpRemove, ID: client.OpenAICreditOrgAccount(lower)},
	}}, orgNow.Add(time.Second))
	if err != nil || result.Renamed != 1 || result.Removed != 1 {
		t.Fatalf("renamed and removed %+v, %v", result, err)
	}
	if items := s.Current().Items; len(items) != 3 || items[0].Label != "Personal" || items[0].OrganizationID != openAIPro || items[1].OrganizationID != openAIBusiness {
		t.Fatalf("items %+v", items)
	}

	// The file loads under OpenAI's rules, the same again, and not as Claude's.
	raw := readFile(t, s.Path())
	if o := LoadOrganizationsFor(OpenAI, s.Path()); !reflect.DeepEqual(o, s.Current()) {
		t.Fatalf("loaded %+v", o)
	}
	if again, err := decodeOrganizationsFor(OpenAI, raw); err != nil || !reflect.DeepEqual(again, s.Current()) {
		t.Fatalf("round trip %+v, %v", again, err)
	}
	if o := LoadOrganizations(s.Path()); o.State != client.OrganizationsStateUnreadable {
		t.Fatalf("Claude's rules loaded the OpenAI file: %+v", o)
	}
	// Claude's store refuses a slug, and its file is apart.
	claude := OpenOrganizations(snapshot)
	var fail *OpError
	if _, err := claude.Apply(OrganizationsBatch{Ops: []OrganizationOp{addOp("Pro", openAIPro)}}, orgNow); !errors.As(err, &fail) || fail.Code != CodeInvalidOrganizationID {
		t.Fatalf("a slug in the Claude store: %v", err)
	}
	if exists(claude.Path()) || claude.Path() == s.Path() {
		t.Fatalf("the Claude store wrote %s", claude.Path())
	}

	// The load rules, one file at a time.
	entry := func(label, org string) any {
		return map[string]any{"label": label, "organization_id": org, "rev": "1", "updated_at": "2026-10-12T09:00:00Z"}
	}
	for name, tc := range map[string]struct {
		entries []any
		loads   bool
	}{
		"slugs differing in case":  {[]any{entry("a", openAIPro), entry("b", lower)}, true},
		"the same slug twice":      {[]any{entry("a", openAIPro), entry("b", openAIPro)}, false},
		"a padded slug":            {[]any{entry("a", openAIBusiness+" ")}, false},
		"a slug with a space":      {[]any{entry("a", "acme corp")}, false},
		"labels differing in case": {[]any{entry("Team", openAIPro), entry("team", openAIBusiness)}, false},
	} {
		snapshot := snapshotIn(t.TempDir())
		place(t, client.OpenAIOrganizationsPath(snapshot), marshal(t, map[string]any{"schema": 1, "revision": "1", "organizations": tc.entries}))
		if o := OpenOrganizationsFor(OpenAI, snapshot).Current(); (o.State == client.OrganizationsStateOK) != tc.loads {
			t.Errorf("%s: %+v", name, o)
		}
	}
}

// A Claude baseline entry with a service tier fails the Claude settings file,
// as Quota Glance 0.7 and quota-cache 0.5.0 refused the key: only the OpenAI
// meter writes one. So does OpenAI's long-context class. An empty tier,
// which encodes as no key, loads.
func TestClaudeSettingsRefuseATier(t *testing.T) {
	for name, tc := range map[string]struct {
		entry map[string]any
		loads bool
	}{
		"no tier":            {map[string]any{"model": "claude-sonnet-5-5", "input": 1}, true},
		"an empty tier":      {map[string]any{"model": "claude-sonnet-5-5", "tier": "", "input": 1}, true},
		"flex":               {map[string]any{"model": "claude-sonnet-5-5", "tier": client.MeterTierFlex, "input": 1}, false},
		"unknown":            {map[string]any{"model": "claude-sonnet-5-5", "tier": client.MeterTierUnknown, "input": 1}, false},
		"OpenAI's over_272k": {map[string]any{"model": "claude-sonnet-5-5", "prompt": client.MeterPromptOver272K, "input": 1}, false},
	} {
		dir := t.TempDir()
		f := validFile()
		baselineOf(f, accountA)["usage"] = []any{tc.entry}
		writeSettings(t, dir, f)
		raw := readFile(t, FilePath(snapshotIn(dir)))
		s := Open(snapshotIn(dir))
		if got := !s.Current().Unreadable; got != tc.loads {
			t.Errorf("%s: loads %v, want %v", name, got, tc.loads)
			continue
		}
		if tc.loads {
			continue
		}
		if _, err := s.Apply(renewalSave(renewalA, "5", "2026-11-01"), nil, at(t, "2026-10-09T14:00:00Z")); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: a save answered %v", name, err)
		}
		if after := readFile(t, FilePath(snapshotIn(dir))); !bytes.Equal(after, raw) {
			t.Errorf("%s: the unreadable file was overwritten", name)
		}
	}
}

// The OpenAI API credits card's batch has exactly the API credits card's
// shape and bounds, and its kind.
func TestParseBatchOpenAIKinds(t *testing.T) {
	body := `{"kind":"openaiApiCredits","items":[{"id":"` + openAIAccount + `","baseRevision":"7","monthlyUsd":"200","renews":"2026-09-28",
		"reading":{"remainingUsd":"143.20","at":"2026-10-09T13:20:00Z"}}]}`
	b, err := ParseBatch([]byte(body))
	want := []APICreditItem{{ID: openAIAccount, BaseRevision: "7", MonthlyUSD: ptr("200"), Renews: ptr("2026-09-28"),
		Reading: &ReadingInput{RemainingUSD: "143.20", At: "2026-10-09T13:20:00Z"}}}
	if err != nil || b.Kind != KindOpenAIAPICredits || !reflect.DeepEqual(b.APICredits, want) || len(b.Renewals) != 0 {
		t.Fatalf("%+v %v", b, err)
	}
	if kind := RequestKind([]byte(body)); kind != KindOpenAIAPICredits {
		t.Fatalf("kind %q", kind)
	}
	item := func(i int) string {
		return fmt.Sprintf(`{"id":"oai-%012x","baseRevision":"","monthlyUsd":"1","renews":null,"reading":null}`, i)
	}
	batch := func(items ...string) string {
		return `{"kind":"openaiApiCredits","items":[` + strings.Join(items, ",") + `]}`
	}
	many := func(n int) string {
		items := make([]string, n)
		for i := range items {
			items[i] = item(i)
		}
		return batch(items...)
	}
	if b, err := ParseBatch([]byte(many(MaxAPICreditItems))); err != nil || len(b.APICredits) != MaxAPICreditItems {
		t.Errorf("sixteen rows: %v", err)
	}
	for name, body := range map[string]string{
		"seventeen rows":      many(MaxAPICreditItems + 1),
		"zero rows":           batch(),
		"a renewal's item":    batch(`{"id":"0123456789abcdef","baseRevision":"","date":"2026-10-29"}`),
		"a missing value key": batch(strings.Replace(item(0), `,"reading":null`, ``, 1)),
		"an unknown item key": batch(strings.Replace(item(0), `"reading":null`, `"reading":null,"renewal":null`, 1)),
		"a repeated id":       batch(item(0), item(0)),
		"an unknown top key":  `{"kind":"openaiApiCredits","items":[` + item(0) + `],"provider":"openai"}`,
		"an upper-case kind":  strings.Replace(batch(item(0)), "openaiApiCredits", "openAIApiCredits", 1),
	} {
		if _, err := ParseBatch([]byte(body)); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The OpenAI organizations card's batch has exactly the organizations card's
// shape, and parses only for OpenAI: each provider's parser refuses the
// other's kind, so a batch can never reach the other provider's file.
func TestParseOrganizationsBatchOpenAIKind(t *testing.T) {
	ops := `"ops":[
		{"op":"add","label":"Pro","organizationId":"` + openAIPro + `"},
		{"op":"rename","id":"` + openAIAccount + `","label":"Personal"},
		{"op":"remove","id":"` + openAIAccount + `"}]}`
	openAI := `{"kind":"openaiOrganizations","baseRevision":"4",` + ops
	claude := `{"kind":"organizations","baseRevision":"4",` + ops
	want := OrganizationsBatch{BaseRevision: "4", Ops: []OrganizationOp{
		{Op: OpAdd, Label: "Pro", OrganizationID: openAIPro},
		{Op: OpRename, ID: openAIAccount, Label: "Personal"},
		{Op: OpRemove, ID: openAIAccount},
	}}
	if b, err := ParseOrganizationsBatchFor(OpenAI, []byte(openAI)); err != nil || !reflect.DeepEqual(b, want) {
		t.Fatalf("%+v %v", b, err)
	}
	if b, err := ParseOrganizationsBatchFor(Claude, []byte(claude)); err != nil || !reflect.DeepEqual(b, want) {
		t.Fatalf("the same ops for Claude: %+v %v", b, err)
	}
	if kind := RequestKind([]byte(openAI)); kind != KindOpenAIOrganizations {
		t.Fatalf("kind %q", kind)
	}
	for name, parse := range map[string]func() (OrganizationsBatch, error){
		"Claude's kind for OpenAI":    func() (OrganizationsBatch, error) { return ParseOrganizationsBatchFor(OpenAI, []byte(claude)) },
		"OpenAI's kind for Claude":    func() (OrganizationsBatch, error) { return ParseOrganizationsBatchFor(Claude, []byte(openAI)) },
		"OpenAI's kind, Claude's way": func() (OrganizationsBatch, error) { return ParseOrganizationsBatch([]byte(openAI)) },
	} {
		if _, err := parse(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	add := `{"op":"add","label":"Pro","organizationId":"` + openAIPro + `"}`
	body := func(ops ...string) string {
		return `{"kind":"openaiOrganizations","baseRevision":"","ops":[` + strings.Join(ops, ",") + `]}`
	}
	many := make([]string, MaxOrganizationOps+1)
	for i := range many {
		many[i] = add
	}
	if _, err := ParseOrganizationsBatchFor(OpenAI, []byte(body(many[:MaxOrganizationOps]...))); err != nil {
		t.Errorf("sixteen ops: %v", err)
	}
	for name, raw := range map[string]string{
		"seventeen ops":      body(many...),
		"zero ops":           body(),
		"an API credit kind": strings.Replace(body(add), "openaiOrganizations", KindOpenAIAPICredits, 1),
		"an unknown top key": `{"kind":"openaiOrganizations","baseRevision":"","ops":[` + add + `],"provider":"openai"}`,
		"add with an id":     body(`{"op":"add","id":"` + openAIAccount + `","label":"Pro","organizationId":"` + openAIPro + `"}`),
		"a padded base":      `{"kind":"openaiOrganizations","baseRevision":"04","ops":[` + add + `]}`,
	} {
		if _, err := ParseOrganizationsBatchFor(OpenAI, []byte(raw)); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A reading's baseline keeps the OpenAI meter's service tiers and prompt
// classes, and sums anything the OpenAI file's rules would refuse under
// (other), with neither, which is never priced; so does a capped baseline's
// remainder. Claude's rules fold every tier.
func TestOpenAIBaselineKeepsTiers(t *testing.T) {
	usage := []client.MeterUsage{}
	for _, u := range []client.MeterUsage{
		{Model: "gpt-6-sol", Requests: 1, Input: 10},
		{Model: "gpt-6-sol", Tier: client.MeterTierFlex, Requests: 1, Input: 20},
		{Model: "gpt-6-sol", Tier: client.MeterTierUltrafast, Requests: 1, Input: 30},
		{Model: "gpt-6-sol", Tier: client.MeterTierUnknown, Requests: 1, Input: 40},
		{Model: "gpt-6-sol", Prompt: client.MeterPromptOver272K, Tier: client.MeterTierFast, Requests: 1, Input: 300000},
		// A tier and a prompt class a newer meter might add, and a model name
		// the meter never writes.
		{Model: "gpt-6-sol", Tier: "turbo", Requests: 1, Input: 5},
		{Model: "gpt-6-sol", Prompt: "over_1m", Requests: 1, Input: 6},
		{Model: "GPT-6-Sol", Requests: 1, Input: 7},
	} {
		usage = client.MergeMeterUsage(usage, u)
	}
	loads := func(p *Provider, org string, usage []client.MeterUsage) bool {
		text := func(s string) *string { return &s }
		_, ok := decodeReading(p, fileReading{
			RemainingUSD: text("1.00"), At: text("2026-10-09T13:20:00Z"), EnteredAt: text("2026-10-09T13:24:10Z"), OrganizationID: text(org),
			Baseline: &fileBaseline{DayStart: text("2026-10-09T00:00:00Z"), Until: text("2026-10-09T13:00:00Z"), Usage: usage},
		})
		return ok
	}
	if loads(OpenAI, openAIPro, usage) || loads(Claude, orgA, usage) {
		t.Fatal("usage the meter wrote loads as it is")
	}
	openAI := []client.MeterUsage{
		{Model: client.MeterOtherModel, Requests: 3, Input: 18},
		{Model: "gpt-6-sol", Requests: 1, Input: 10},
		{Model: "gpt-6-sol", Tier: client.MeterTierFlex, Requests: 1, Input: 20},
		{Model: "gpt-6-sol", Tier: client.MeterTierUltrafast, Requests: 1, Input: 30},
		{Model: "gpt-6-sol", Tier: client.MeterTierUnknown, Requests: 1, Input: 40},
		{Model: "gpt-6-sol", Prompt: client.MeterPromptOver272K, Tier: client.MeterTierFast, Requests: 1, Input: 300000},
	}
	if got := OpenAI.loadable(usage); !reflect.DeepEqual(got, openAI) || !loads(OpenAI, openAIPro, got) {
		t.Errorf("OpenAI:\n got %+v\nwant %+v", got, openAI)
	}
	claude := []client.MeterUsage{
		{Model: client.MeterOtherModel, Requests: 7, Input: 20 + 30 + 40 + 300000 + 5 + 6 + 7},
		{Model: "gpt-6-sol", Requests: 1, Input: 10},
	}
	if got := Claude.loadable(usage); !reflect.DeepEqual(got, claude) || !loads(Claude, orgA, got) {
		t.Errorf("Claude:\n got %+v\nwant %+v", got, claude)
	}

	// Capped, the smallest are summed under (other) with no tier or prompt.
	tiers := []string{"", client.MeterTierFlex, client.MeterTierFast, client.MeterTierUltrafast, client.MeterTierUnknown}
	many := []client.MeterUsage{}
	for i := range 20 {
		u := client.MeterUsage{Model: "gpt-6-sol", Tier: tiers[i%len(tiers)], Input: uint64(100 + i)}
		if i/len(tiers)%2 == 1 {
			u.Prompt = client.MeterPromptOver272K
		}
		if i >= 2*len(tiers) {
			u.Model = "gpt-6-luna"
		}
		many = client.MergeMeterUsage(many, u)
	}
	capped := capBaseline(many)
	others := 0
	for _, u := range capped {
		if u.Model == client.MeterOtherModel {
			others++
			if u.Tier != "" || u.Prompt != "" || u.Input != 100+101+102+103+104 {
				t.Errorf("(other) = %+v", u)
			}
		}
	}
	if len(capped) != MaxBaseline || others != 1 || !loads(OpenAI, openAIPro, capped) {
		t.Fatalf("capped to %d entries, %d (other): %+v", len(capped), others, capped)
	}

	// Through a save: the baseline from an OpenAI meter, keyed by the slug,
	// keeps its tiers, and the file it is saved in loads again.
	s := OpenFor(OpenAI, snapshotIn(t.TempDir()))
	meter := openAIMeterFor(t, openAIPro, "2026-10-09T13:59:00Z",
		client.MeterBucket{Start: at(t, "2026-10-09T09:00:00Z"), Usage: usage},
		client.MeterBucket{Start: at(t, "2026-10-09T13:00:00Z"), Usage: []client.MeterUsage{{Model: "gpt-6-sol", Input: 50}}},
	)
	item := APICreditItem{ID: openAIAccount, Reading: &ReadingInput{RemainingUSD: "143.20", At: "2026-10-09T13:20:00Z"}, OrganizationID: openAIPro}
	if _, err := s.Apply(Batch{Kind: KindOpenAIAPICredits, APICredits: []APICreditItem{item}}, meter, at(t, "2026-10-09T14:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if got := s.Current().APICredits[openAIAccount].Reading.Baseline.Usage; !reflect.DeepEqual(got, openAI) {
		t.Fatalf("baseline:\n got %+v\nwant %+v", got, openAI)
	}
	if again := OpenFor(OpenAI, snapshotIn(s.Dir())).Current(); again.Unreadable || !reflect.DeepEqual(again, s.Current()) {
		t.Fatalf("reopened %+v", again)
	}
}
