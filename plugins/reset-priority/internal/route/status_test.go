package route

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// statusFixture is the worked example after a few picks: one exploration
// waiting out its second wait, one re-check, routed and declined picks.
func statusFixture(t *testing.T) (*Router, time.Time) {
	f := worked(t)
	f.Accounts[0].EstimateLeftText, f.Accounts[0].DataIssues = "$6.20", []string{"refusedNearlySpent"}
	f.Accounts[0].Issue = "Anthropic refused requests from a Claude Code-based client for low credit, and the estimate is nearly spent, so this credit is shown as spent. Enter a Console reading to check."
	r := routing(t, f.input())
	r.SetDeclared(true)
	now := at(t, "2026-10-11T12:00:42Z")
	expect(t, r, request(t, "", all...), now, explored("12"))
	expect(t, r, request(t, id("12"), without("12")...), now, routed("0b"))
	expect(t, r, request(t, "", all...), now, rechecked("0a"))
	expect(t, r, request(t, id("0a"), without("0a")...), now, routed("0b"))
	expect(t, r, request(t, id("0b"), "0a", "0c", "13"), now, decline(ReasonNoEligible))
	r.Pick([]byte(`{"Candidates":[{"Attributes":{"auth_kind":"oauth"}}]}`), now)
	f.Meter.FlushedAt = at(t, "2026-10-11T12:03:31Z")
	r.Publish(Build(f.inputAt(at(t, "2026-10-11T12:04:00Z"))), at(t, "2026-10-11T12:04:00Z"))
	return r, at(t, "2026-10-11T12:04:30Z")
}

func TestStatusJSON(t *testing.T) {
	r, now := statusFixture(t)
	raw, err := json.MarshalIndent(r.Status(now), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	path := filepath.Join("testdata", "status.json")
	if *update {
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("status golden rewritten")
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("status golden missing; regenerate with: go test ./internal/route/ -run TestStatusJSON -update (%v)", err)
	}
	if !bytes.Equal(raw, want) {
		t.Fatalf("router status differs from testdata/status.json; the status route serves it, so review the change.\ngot:\n%s", raw)
	}
	for _, leak := range []string{"claude:apikey:", "00000000-0000-4000-8000", "000000000000000"} {
		if bytes.Contains(raw, []byte(leak)) {
			t.Fatalf("the status carries %q", leak)
		}
	}

	// The exact key set, at every level.
	keys := func(raw []byte) []string {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for k := range object {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	var s struct {
		Orgs      []json.RawMessage `json:"orgs"`
		Exploring []json.RawMessage `json:"exploring"`
		Counters  json.RawMessage   `json:"counters"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		raw  []byte
		want string
	}{
		{raw, "active auth_ids_since built_at counters declared enabled explore_every_seconds exploring failing_keys foreign_keys last_decision last_pick_at last_routed_at last_routed_org mapped_keys max_explore_wait_seconds max_meter_age_seconds max_table_age_seconds meter_flushed_at orgs reason stale_keys table_reason unlinked_keys unmapped_keys"},
		{s.Orgs[0], "demoted eligible estimate_left_text failing_keys id issue keys label left_text rank reason recheck refusal renews_at"},
		{s.Exploring[0], "config_index kind next_at step"},
		{s.Counters, "build_panics declined demoted explored picks rechecked routed"},
	} {
		if got := strings.Join(keys(c.raw), " "); got != c.want {
			t.Errorf("keys = %s\nwant   %s", got, c.want)
		}
	}
}

// Off, the router says why it is not routing and still whether it would.
// Without a router, the status serves Idle, whose arrays are never null.
func TestStatusWhenOffAndIdle(t *testing.T) {
	r, now := statusFixture(t)
	r.SetEnabled(false)
	r.SetDeclared(false)
	s := r.Status(now)
	if s.Enabled || s.Declared || s.Active || s.Reason != "disabled" || s.TableReason != "" || len(s.Orgs) != 6 {
		t.Fatalf("off: %+v", s)
	}
	s = r.Status(now.Add(MaxTableAge))
	if s.TableReason != "table_stale" || s.Reason != "disabled" {
		t.Fatalf("off, table stale: reason %q table_reason %q", s.Reason, s.TableReason)
	}
	r.BuildFailed(now)
	if s := r.Status(now); s.TableReason != "build_failed" || s.Counters.BuildPanics != 1 || s.BuiltAt != nil {
		t.Fatalf("after a build panic: %+v", s)
	}
	r.Publish(nil, now)
	if s := r.Status(now); s.TableReason != "no_table" {
		t.Fatalf("after Publish(nil): table_reason %q", s.TableReason)
	}

	idle := Idle()
	raw, err := json.Marshal(idle)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"orgs":[]`, `"exploring":[]`, `"reason":"disabled"`, `"table_reason":"no_table"`, `"built_at":null`, `"last_pick_at":null`} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("Idle() lacks %s: %s", want, raw)
		}
	}
	fresh := New().Status(now)
	if !reflect.DeepEqual(fresh, idle) {
		t.Fatalf("a new router's status differs from Idle:\n%+v\n%+v", fresh, idle)
	}
	if len(idle.Counters.Declined) != 20 {
		t.Fatalf("declined has %d reasons; want every one of the 20", len(idle.Counters.Declined))
	}
	for reason, n := range idle.Counters.Declined {
		if n != 0 || reason == "" || reason == "routed" || reason == "build_failed" {
			t.Fatalf("declined[%q] = %d", reason, n)
		}
	}
}

// last_routed_org is the organization of the latest answer that named a key,
// so an unmapped exploration, whose key has none yet, clears it while
// last_routed_at moves on. A decline names no key and changes neither.
func TestStatusLastRoutedOrg(t *testing.T) {
	f := worked(t)
	r := routing(t, f.input())
	now := f.Now.Add(42 * time.Second)
	check := func(step string, at time.Time, org string, routedAt time.Time, decision string) {
		t.Helper()
		s := r.Status(at)
		if s.LastRoutedOrg != org || s.LastRoutedAt == nil || !s.LastRoutedAt.Equal(routedAt) || s.LastDecision != decision {
			t.Fatalf("%s: last_routed_org %q at %v, last_decision %q; want %q at %v, %q",
				step, s.LastRoutedOrg, s.LastRoutedAt, s.LastDecision, org, routedAt, decision)
		}
	}
	if s := r.Status(now); s.LastRoutedOrg != "" || s.LastRoutedAt != nil {
		t.Fatalf("before any answer: %q at %v", s.LastRoutedOrg, s.LastRoutedAt)
	}
	expect(t, r, request(t, "", "0b", "0d"), now, routed("0b"))
	check("routed", now, "org-bbbbbbbbbbbb", now, "routed")
	explore := now.Add(time.Second)
	expect(t, r, request(t, "", "0b", "12"), explore, explored("12"))
	check("an unmapped exploration", explore, "", explore, "explored")
	declined := explore.Add(time.Second)
	expect(t, r, request(t, "", "0a", "0c"), declined, decline(ReasonNoEligible))
	check("a decline", declined, "", explore, "no_eligible")
	again := declined.Add(time.Second)
	expect(t, r, request(t, "", "0d"), again, routed("0d"))
	check("routed again", again, "org-dddddddddddd", again, "routed")
}

// Publish reports the router's active edges, so the rebuild can log one line
// per change.
func TestPublishReportsActiveEdges(t *testing.T) {
	f := worked(t)
	r := New()
	table := Build(f.input())
	for i, c := range []struct {
		enabled  bool
		table    *Table
		was, now bool
	}{
		{false, table, false, false},
		{true, table, false, true},
		{true, table, true, true},
		{true, nil, true, false},
		{true, table, false, true},
		{false, table, true, false},
	} {
		r.SetEnabled(c.enabled)
		if was, now := r.Publish(c.table, f.Now); was != c.was || now != c.now {
			t.Fatalf("publish %d: was %v now %v; want %v %v", i, was, now, c.was, c.now)
		}
	}
	inactive := worked(t)
	inactive.Meter.AuthIDsSince = nil
	r.SetEnabled(true)
	r.Publish(table, f.Now)
	if was, now := r.Publish(Build(inactive.input()), f.Now); !was || now {
		t.Fatalf("an inactive table: was %v now %v", was, now)
	}
	// A table published after its state's valid_until would not route.
	bounded := f.input()
	bounded.Credits.ValidUntil = ptr(f.Now.Add(time.Minute))
	if was, now := r.Publish(Build(bounded), f.Now); was || !now {
		t.Fatalf("before valid_until: was %v now %v", was, now)
	}
	if was, now := r.Publish(Build(bounded), f.Now.Add(time.Minute)); !was || now {
		t.Fatalf("at valid_until: was %v now %v", was, now)
	}
}

// REFERENCE.md is where an operator looks up a value the status shows, so
// every value the router can put in router.table_reason,
// router.counters.declined, router.orgs[].reason, router.orgs[].refusal and
// router.exploring[].kind is there, spelled as the status spells it. The
// organization reasons are read from table.go's constants and the refusals
// from refusals, so a new one cannot ship undocumented.
func TestReferenceNamesEveryHealthValue(t *testing.T) {
	reference, err := os.ReadFile(filepath.Join("..", "..", "REFERENCE.md"))
	if err != nil {
		t.Fatal(err)
	}
	values := append([]string{}, kindNames[:]...)
	values = append(values, refusals[:]...)
	for reason := ReasonDisabled; reason < reasonCount; reason++ {
		values = append(values, reason.String())
	}
	file, err := parser.ParseFile(token.NewFileSet(), "table.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	orgReasons := 0
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			spec := spec.(*ast.ValueSpec)
			for i, name := range spec.Names {
				if !strings.HasPrefix(name.Name, "org") || i >= len(spec.Values) {
					continue
				}
				literal, ok := spec.Values[i].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatal(err)
				}
				values = append(values, value)
				orgReasons++
			}
		}
	}
	if orgReasons == 0 {
		t.Fatal("table.go declares no organization reasons")
	}
	for _, value := range values {
		if !bytes.Contains(reference, []byte("`"+value+"`")) {
			t.Errorf("REFERENCE.md never names `%s`, which the status can show", value)
		}
	}
}

// The router declines with settings_unavailable for every settings state but
// ok, and the states point at different files: Quota Cache's own settings
// file, or, before it has one, Quota Glance's settings.json, which its first
// start imports. So the reason's row in REFERENCE.md and in the
// troubleshooting guide names the file for every state Quota Cache declares,
// read from its client so a new one cannot ship unnamed.
func TestDocsNameEverySettingsCause(t *testing.T) {
	files := map[string]string{
		qc.CreditSettingsUnreadable:       "`snapshot.settings.json`",
		qc.CreditSettingsLegacyUnreadable: "Quota Glance's `settings.json`",
	}
	sources, err := filepath.Glob(filepath.Join("..", "..", "..", "quota-cache", "client", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	states := 0
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), source, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				spec := spec.(*ast.ValueSpec)
				for i, name := range spec.Names {
					if !strings.HasPrefix(name.Name, "CreditSettings") || i >= len(spec.Values) {
						continue
					}
					literal, ok := spec.Values[i].(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						continue
					}
					state, err := strconv.Unquote(literal.Value)
					if err != nil {
						t.Fatal(err)
					}
					states++
					if _, named := files[state]; !named && state != qc.CreditSettingsOK {
						t.Errorf("Quota Cache declares settings state %q (%s), which this test does not know the file for", state, name.Name)
					}
				}
			}
		}
	}
	if states == 0 {
		t.Fatal("Quota Cache's client declares no settings states")
	}
	f := worked(t)
	for state := range files {
		in := f.input()
		in.Credits.Settings.State = state
		if reason := Build(in).Reason; reason != ReasonSettingsUnavailable {
			t.Errorf("settings state %q: reason %s; want %s", state, reason, ReasonSettingsUnavailable)
		}
	}
	for _, doc := range []string{"REFERENCE.md", filepath.Join("docs", "troubleshooting.md")} {
		raw, err := os.ReadFile(filepath.Join("..", "..", doc))
		if err != nil {
			t.Fatal(err)
		}
		row := ""
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "| `settings_unavailable` |") {
				row = line
			}
		}
		if row == "" {
			t.Errorf("%s has no `settings_unavailable` row", doc)
			continue
		}
		for state, file := range files {
			if !strings.Contains(row, file) {
				t.Errorf("%s's `settings_unavailable` row never names %s, the file to fix for settings state %q", doc, file, state)
			}
		}
		if !strings.Contains(row, "Quota Cache's page") || !strings.Contains(row, "credit-settings.md#when-a-file-cannot-be-read)") {
			t.Errorf("%s's `settings_unavailable` row does not send the operator to Quota Cache's page and its guide, which name the file", doc)
		}
	}
	guide, err := os.ReadFile(filepath.Join("..", "..", "..", "quota-cache", "docs", "credit-settings.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(guide), "\n## When a file cannot be read\n") {
		t.Error("Quota Cache's credit-settings.md no longer has the When a file cannot be read section both rows link to")
	}
}

// Routing supports CPA from v8.0.4, but CPA stops a plugin that is turned off
// only from v8.0.22. An older CPA just stops sending it usage records, so
// Quota Cache keeps saving its meter and publishing a fresh credit state that
// no longer counts spend: the router never declines credits_stale or
// meter_stopped, never warns, and keeps routing on spend frozen at that
// moment. So every paragraph that states the CPA requirement, and every line
// that offers one of those reasons as what follows Quota Cache being turned
// off, says what happens before v8.0.22, and the requirements say what to do.
func TestDocsSayAnOlderCPALeavesQuotaCacheRunning(t *testing.T) {
	const (
		floor = "CPA v8.0.4 or newer"
		stops = "v8.0.22"
	)
	turnedOff := regexp.MustCompile(`(?i)\b(?:disabled|turned off|enable (?:quota cache|it))\b`)
	docs := map[string]string{
		"README.md":           "Spend the credit that refills soonest (optional)",
		"REFERENCE.md":        "Requirements",
		"troubleshooting.md":  "Routing carries on after Quota Cache is turned off",
		"config.example.yaml": "",
	}
	requirements, reasons := map[string]int{}, map[string]int{}
	for name, title := range docs {
		path := filepath.Join("..", "..", name)
		if name == "troubleshooting.md" {
			path = filepath.Join("..", "..", "docs", name)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// A YAML comment block is one paragraph too, without its comment marks.
		for _, paragraph := range strings.Split(string(raw), "\n\n") {
			text := strings.Join(strings.FieldsFunc(paragraph, func(r rune) bool { return unicode.IsSpace(r) || r == '#' }), " ")
			if !strings.Contains(text, floor) {
				continue
			}
			requirements[name]++
			if !strings.Contains(text, stops) {
				t.Errorf("%s says routing needs %s but not what turning Quota Cache off does before %s: %q", name, floor, stops, text)
			}
		}
		for n, line := range strings.Split(string(raw), "\n") {
			if !strings.Contains(line, "`credits_stale`") && !strings.Contains(line, "`meter_stopped`") || !turnedOff.MatchString(line) {
				continue
			}
			reasons[name]++
			if !strings.Contains(line, stops) {
				t.Errorf("%s:%d offers a decline as what follows Quota Cache being turned off, which holds only from CPA %s: %q", name, n+1, stops, line)
			}
		}
		if title == "" {
			continue
		}
		text, found := section(string(raw), title)
		if !found {
			t.Errorf("%s has no %q section", name, title)
			continue
		}
		text = strings.Join(strings.Fields(text), " ")
		for _, want := range []string{stops, "off before turning Quota Cache off", "Console reading"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s's %q section never says %q", name, title, want)
			}
		}
	}
	for _, name := range []string{"README.md", "REFERENCE.md", "config.example.yaml"} {
		if requirements[name] == 0 {
			t.Errorf("%s never says routing needs %s", name, floor)
		}
	}
	for _, name := range []string{"REFERENCE.md", "troubleshooting.md"} {
		if reasons[name] == 0 {
			t.Errorf("%s offers no decline as what follows Quota Cache being turned off, so this test checks nothing there", name)
		}
	}
}

// The router ranks by each organization's refill date, which is Quota Cache's
// renews: the day the linked claude.ai plan renews, not Console's expiry for
// the current credit, which may be the day before (qc.APICredit.Renews). A
// date a day early turns the ranking over a day early and lets the rest of
// that credit expire, so REFERENCE.md's "Renewal day" asks for the same date
// as Quota Cache's README, and links to it. The date is set on Quota Cache's
// page only: once the organizations are there, Quota Cache ignores
// claude-api-credits, so a renews edited in its configuration changes nothing
// and the instruction must not offer it.
func TestReferenceRenewalDayFollowsQuotaCache(t *testing.T) {
	reference, err := os.ReadFile(filepath.Join("..", "..", "REFERENCE.md"))
	if err != nil {
		t.Fatal(err)
	}
	section, found := section(string(reference), "Renewal day")
	if !found {
		t.Fatal(`REFERENCE.md has no "Renewal day" section`)
	}
	for _, want := range []string{"claude.ai plan renews", "(../quota-cache/README.md#claude-api-credits-optional)"} {
		if !strings.Contains(section, want) {
			t.Errorf("REFERENCE.md's Renewal day never says %q", want)
		}
	}
	instruction := ""
	for _, sentence := range strings.Split(strings.ReplaceAll(section, "\n", " "), ". ") {
		if strings.Contains(sentence, "claude.ai plan renews") {
			instruction = sentence
			break
		}
	}
	for _, want := range []string{"Quota Cache's page", "**Refills on, UTC**", "**Claude API credits**"} {
		if !strings.Contains(instruction, want) {
			t.Errorf("REFERENCE.md's Renewal day instruction never says %q: %q", want, instruction)
		}
	}
	if strings.Contains(instruction, "configuration") {
		t.Errorf("REFERENCE.md's Renewal day offers Quota Cache's configuration, which it ignores once the organizations are on its page: %q", instruction)
	}
	readme, err := os.ReadFile(filepath.Join("..", "..", "..", "quota-cache", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "\n## Claude API credits (optional)\n") || !strings.Contains(string(readme), "claude.ai plan renews") {
		t.Error("Quota Cache's README no longer has the renews instruction REFERENCE.md links to")
	}
	editor, err := os.ReadFile(filepath.Join("..", "..", "..", "quota-cache", "internal", "plugin", "pageassets", "editor.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"'Refills on, UTC'", "'Claude API credits'"} {
		if !strings.Contains(string(editor), label) {
			t.Errorf("Quota Cache's page no longer shows %s, which REFERENCE.md's Renewal day names", label)
		}
	}
}
