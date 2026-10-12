package credits

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/settings"
)

var update = flag.Bool("update", false, "rewrite testdata/golden/*.credits.json")

// The committed states are what quota-cache publishes for each fixture at its
// instant. Quota Glance's and Reset Priority's tests read them as published
// input, so a change here is a change to what they are shown.
func TestGoldenCreditStates(t *testing.T) {
	for _, f := range fixtures {
		raw, err := json.MarshalIndent(computeFixture(t, f.name), "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, '\n')
		path := filepath.Join("testdata", "golden", f.name+".credits.json")
		if *update {
			if err := os.WriteFile(path, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (run go test ./internal/credits -update to write it)", err)
		}
		if !bytes.Equal(raw, want) {
			t.Errorf("%s differs from the committed state; if the change is intended, rerun with -update and review the diff", path)
		}
		// What is committed is what a reader loads.
		state, err := client.LoadCredits(path)
		if err != nil || !reflect.DeepEqual(state, computeFixture(t, f.name)) {
			t.Errorf("%s does not load back to the state it was written from: %v", path, err)
		}
	}
}

// characterization is Quota Glance's apiCredits for a fixture, as its
// aggregate package built it before the computation moved here: the snapshot,
// meter and settings of the fixture, its instant, and stale-after 45m.
func characterization(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "characterization", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// maskEditing decodes both cards and removes what differs between them by
// design: the card's editing, and the editability of each account Quota
// Glance's own allow-edit switch made read-only. Quota Glance maps both when it
// shows the published state.
func maskEditing(t *testing.T, gotRaw, wantRaw []byte) (got, want map[string]any) {
	t.Helper()
	if err := json.Unmarshal(gotRaw, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wantRaw, &want); err != nil {
		t.Fatal(err)
	}
	delete(got, "editing")
	delete(want, "editing")
	gotAccounts, wantAccounts := got["accounts"].([]any), want["accounts"].([]any)
	if len(gotAccounts) != len(wantAccounts) {
		return got, want
	}
	for i := range wantAccounts {
		w := wantAccounts[i].(map[string]any)["settings"].(map[string]any)
		g := gotAccounts[i].(map[string]any)["settings"].(map[string]any)
		if w["notEditableReason"] == "disabled" {
			for _, key := range []string{"editable", "notEditableReason"} {
				delete(w, key)
				delete(g, key)
			}
		}
	}
	return got, want
}

// The proof of the move: for every fixture, the published state as a reader
// with Quota Glance's stale-after sees it is Quota Glance's own card, field
// for field, but for editing.
func TestCharacterizationMatchesGlance(t *testing.T) {
	for _, f := range fixtures {
		seen := client.CreditsAt(computeFixture(t, f.name), f.at, 45*time.Minute)
		raw, err := json.Marshal(seen.APICredits)
		if err != nil {
			t.Fatal(err)
		}
		got, want := maskEditing(t, raw, characterization(t, f.name))
		if !reflect.DeepEqual(got, want) {
			gotText, _ := json.MarshalIndent(got, "", "  ")
			wantText, _ := json.MarshalIndent(want, "", "  ")
			t.Errorf("%s: the published card differs from Quota Glance's:\n got %s\nwant %s", f.name, gotText, wantText)
		}
	}
}

// keyPaths lists every object key path in raw, in the order they first
// appear, with array elements as "[]".
func keyPaths(t *testing.T, raw []byte) []string {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	paths, seen := []string{}, map[string]bool{}
	var walk func(prefix string)
	walk = func(prefix string) {
		token, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		switch token {
		case json.Delim('{'):
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					t.Fatal(err)
				}
				path := prefix + "." + key.(string)
				if !seen[path] {
					seen[path] = true
					paths = append(paths, path)
				}
				walk(path)
			}
			decoder.Token()
		case json.Delim('['):
			for decoder.More() {
				walk(prefix + "[]")
			}
			decoder.Token()
		}
	}
	walk("")
	return paths
}

// api_credits keeps the summary contract's names and order: every key path of
// Quota Glance's card, in order, and one more, editing.migration, which
// quota-cache always leaves empty.
func TestCreditWireNames(t *testing.T) {
	for _, f := range fixtures {
		state := computeFixture(t, f.name)
		if state.APICredits.Editing.Migration != "" {
			t.Errorf("%s: editing.migration = %q", f.name, state.APICredits.Editing.Migration)
		}
		raw, err := json.Marshal(client.CreditsAt(state, f.at, 45*time.Minute).APICredits)
		if err != nil {
			t.Fatal(err)
		}
		got := keyPaths(t, raw)
		if !slices.Contains(got, ".editing.migration") {
			t.Errorf("%s: no editing.migration", f.name)
		}
		got = slices.DeleteFunc(got, func(path string) bool { return path == ".editing.migration" })
		if want := keyPaths(t, characterization(t, f.name)); !slices.Equal(got, want) {
			t.Errorf("%s: key paths\n got %v\nwant %v", f.name, got, want)
		}
	}
}

// valid_until is the soonest instant a figure would change with nothing else
// changing, and null when there is none.
func TestValidUntil(t *testing.T) {
	now := instant(t, workedNow)
	org := map[string]string{"a": fakeOrg("a"), "b": fakeOrg("b"), "c": fakeOrg("c")}
	stored := func(id, at string) settings.APICredit { return *storedReading(t, org[id], "100.00", at) }
	for _, tc := range []struct {
		name    string
		entries []client.Entry
		stored  map[string]settings.APICredit
		noMeter bool
		want    string
	}{
		{"the cycle's end", []client.Entry{creditEntry("alpha", 0, org["a"], "200", "2026-10-29")}, nil, false, "2026-10-29T00:00:00Z"},
		{"a reading without a cycle grows too old", []client.Entry{creditEntry("alpha", 0, org["a"], "200", "")},
			map[string]settings.APICredit{"a": stored("a", "2026-10-01T09:30:00Z")}, false, "2026-11-01T09:30:00Z"},
		{"also before the meter has saved", []client.Entry{creditEntry("alpha", 0, org["a"], "200", "")},
			map[string]settings.APICredit{"a": stored("a", "2026-10-01T09:30:00Z")}, true, "2026-11-01T09:30:00Z"},
		{"a reading dated ahead comes into use", []client.Entry{creditEntry("alpha", 0, org["a"], "200", "2026-10-29")},
			map[string]settings.APICredit{"a": stored("a", "2026-10-09T13:00:00Z")}, false, "2026-10-09T13:00:00Z"},
		{"the soonest across accounts", []client.Entry{
			creditEntry("alpha", 0, org["a"], "200", "2026-10-29"),
			creditEntry("bravo", 1, org["b"], "200", "2026-10-15"),
			creditEntry("charlie", 2, org["c"], "200", ""),
		}, map[string]settings.APICredit{"c": stored("c", "2026-09-20T00:00:00Z")}, false, "2026-10-15T00:00:00Z"},
		{"a reading too old already changes nothing", []client.Entry{creditEntry("alpha", 0, org["a"], "200", "")},
			map[string]settings.APICredit{"a": stored("a", "2026-08-01T00:00:00Z")}, false, ""},
		{"a reading for another organization changes nothing", []client.Entry{creditEntry("alpha", 0, org["a"], "200", "")},
			map[string]settings.APICredit{"a": *storedReading(t, org["b"], "100.00", "2026-10-09T13:00:00Z")}, false, ""},
		{"nothing to change", []client.Entry{creditEntry("alpha", 0, org["a"], "200", "")}, nil, false, ""},
		{"no item", nil, nil, false, ""},
	} {
		in := Input{Now: now, Items: itemsOf(tc.entries...), Settings: settings.Values{APICredits: map[string]settings.APICredit{}}}
		for key, credit := range tc.stored {
			in.Settings.APICredits[client.APICreditOrgAccount(org[key])] = credit
		}
		if !tc.noMeter {
			orgs := map[string]client.MeterOrganization{}
			for _, e := range tc.entries {
				orgs[e.APICredit.OrganizationID] = healthyOrg(t)
			}
			in.Meter = meterOf(now.Add(-time.Minute), orgs)
		}
		got := Compute(in).ValidUntil
		switch {
		case tc.want == "" && got != nil:
			t.Errorf("%s: valid_until %s, want null", tc.name, got)
		case tc.want != "" && (got == nil || !got.Equal(instant(t, tc.want)) || got.Location() != time.UTC):
			t.Errorf("%s: valid_until %v, want %s", tc.name, got, tc.want)
		}
	}
}

// What surrounds api_credits: the inputs, the settings and the import as
// given, the stored renewal dates, and the organizations block.
func TestComputeEnvelope(t *testing.T) {
	f := fixtureNamed(t, "seven-credentials")
	in := inputOf(t, f)
	kind, source := client.CreditImportKindAutomatic, "/CLIProxyAPI/plugins/data/quota-glance/settings.json"
	imported := instant(t, "2026-09-01T12:00:00Z")
	in.Import = &client.CreditImport{State: client.CreditImportStateImported, Kind: &kind, SourcePath: &source,
		Counts: &client.CreditImportCounts{MonthlyCredits: 1, Readings: 1, RenewalDates: 1}, ImportedAt: &imported, Following: true, Importable: true}
	in.Organizations = client.CreditOrganizations{State: client.OrganizationsStateOK, Revision: "4", Source: client.OrganizationsSourcePage,
		Migration: &client.OrganizationsMigration{At: imported, Moved: 5}, ConfigItemsIgnored: 5}
	in.MeterError = ""
	in.Now = f.at.Add(1500 * time.Millisecond)
	state := Compute(in)

	if state.Schema != client.CreditsSchema || state.QuotaCacheVersion != "0.2.0" || !state.ComputedAt.Equal(f.at.Add(time.Second)) ||
		state.ComputedAt.Location() != time.UTC {
		t.Errorf("envelope: schema %d version %q computed_at %s", state.Schema, state.QuotaCacheVersion, state.ComputedAt)
	}
	if !state.Inputs.SnapshotWrittenAt.Equal(in.Snapshot.WrittenAt) || !state.Inputs.MeterFlushedAt.Equal(in.Meter.FlushedAt) ||
		state.Inputs.MeterError != "" || state.Inputs.SettingsRevision != "3" {
		t.Errorf("inputs: %s", jsonOf(t, state.Inputs))
	}
	if state.Settings.State != client.CreditSettingsOK || !state.Settings.Editable || state.Settings.Revision != "3" ||
		!reflect.DeepEqual(state.Settings.Import, in.Import) || state.Settings.Import == in.Import || state.Settings.Import.Kind == in.Import.Kind {
		t.Errorf("settings: %s", jsonOf(t, state.Settings))
	}
	if want := map[string]client.RenewalSetting{"5f2b8c41d09e7a36": {Date: "2026-09-10", Revision: "3", UpdatedAtEpoch: instant(t, "2026-09-09T20:00:00Z").Unix()}}; !reflect.DeepEqual(state.Renewals, want) {
		t.Errorf("renewals: %s", jsonOf(t, state.Renewals))
	}
	if !reflect.DeepEqual(state.Organizations, in.Organizations) || state.Organizations.Migration == in.Organizations.Migration {
		t.Errorf("organizations: %s", jsonOf(t, state.Organizations))
	}

	// Without a meter, and with the settings unreadable: nothing stored is
	// published, and the reason is.
	in.Meter, in.MeterError = nil, client.CreditMeterErrorUnreadable
	in.Settings = settings.Values{Unreadable: true}
	state = Compute(in)
	if state.Inputs.MeterFlushedAt != nil || state.Inputs.MeterError != client.CreditMeterErrorUnreadable || state.APICredits.Meter != nil {
		t.Errorf("no meter: %s", jsonOf(t, state.Inputs))
	}
	if state.Settings.State != client.CreditSettingsUnreadable || state.Settings.Editable || state.Settings.Revision != "" ||
		len(state.Renewals) != 0 || state.Renewals == nil || len(state.APICredits.Orphans) != 0 {
		t.Errorf("unreadable: %s renewals %v", jsonOf(t, state.Settings), state.Renewals)
	}
	in.Settings, in.SettingsState = loadSettings(t, f.settings), client.CreditSettingsLegacyUnreadable
	if state = Compute(in); state.Settings.State != client.CreditSettingsLegacyUnreadable || state.Settings.Editable || len(state.Renewals) != 0 ||
		state.APICredits.Editing.Reason != client.CreditEditingLegacyUnreadable {
		t.Errorf("legacy unreadable: %s", jsonOf(t, state.Settings))
	}

	// A snapshot that never said when it was written.
	in.Snapshot.WrittenAt = time.Time{}
	if raw := jsonOf(t, Compute(in).Inputs); !strings.Contains(raw, `"snapshot_written_at":null`) || !strings.Contains(raw, `"meter_flushed_at":null`) {
		t.Errorf("inputs: %s", raw)
	}
}

// Computing twice gives the same bytes, and leaves the input as it was.
func TestComputeIsDeterministic(t *testing.T) {
	for _, f := range fixtures {
		in := inputOf(t, f)
		before := jsonOf(t, in.Settings.APICredits)
		if a, b := jsonOf(t, Compute(in)), jsonOf(t, Compute(in)); a != b {
			t.Errorf("%s: two computes differ", f.name)
		}
		if jsonOf(t, in.Settings.APICredits) != before {
			t.Errorf("%s: Compute changed its input", f.name)
		}
	}
}
