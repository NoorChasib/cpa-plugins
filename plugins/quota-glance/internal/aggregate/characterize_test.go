package aggregate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// testdata/characterization holds each fixture's apiCredits block as
// quota-glance's own pricing built it, written by this test before that
// pricing moved to Quota Cache, and never rewritten since. It is the oracle
// the move is held to. Quota Cache's TestCharacterizationMatchesGlance holds
// its computation to the same files, through its copies, which
// TestCharacterizationMatchesQuotaCache keeps byte-identical; this test holds
// what quota-glance shows of Quota Cache's published state to them.
//
// Every fixture is read at the instant it describes, with the stale-after a
// default install runs.
var characterizations = []struct {
	name string
	at   time.Time
}{
	{"seven-credentials", time.Unix(fixtureNow, 0).UTC()},
	{"degraded-states", time.Unix(fixtureNow, 0).UTC()},
	{"api-credits", time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)},
	{"meter-gaps", time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)},
	// Reset Priority's routing fixture, with no stored settings.
	{"api-credits-routing", time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)},
}

func characterizationPath(name string) string {
	return filepath.Join("..", "..", "testdata", "characterization", name+".json")
}

// withoutEditing is an apiCredits block as generic JSON with the parts
// quota-glance maps to read-only taken out: editing, and each account's
// settings.editable and settings.notEditableReason. Nothing else may differ.
func withoutEditing(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var block map[string]any
	if err := json.Unmarshal(raw, &block); err != nil {
		t.Fatal(err)
	}
	delete(block, "editing")
	accounts, _ := block["accounts"].([]any)
	for _, account := range accounts {
		settings := account.(map[string]any)["settings"].(map[string]any)
		delete(settings, "editable")
		delete(settings, "notEditableReason")
	}
	return block
}

// configIssues are the issues about the values quota-glance 0.7 read from
// Quota Cache's configuration, which Quota Cache 0.2.0 never raises.
var configIssues = []string{qc.CreditIssueConfigMonthlyUSDInvalid, qc.CreditIssueConfigRenewsInvalid, qc.CreditIssueAdminKeyIgnored}

// enteredOnPage turns a characterization into the card for the same values
// entered on Quota Cache's page, which is where Quota Cache 0.2.0 takes them
// from; it reads none from its configuration. A linked organization's
// configured credit or refill date is the page's, stamped with the revision
// and time got's entry carries. Any other account's is none, so a refill date
// gives it no cycle. The configured values and the issues about them are
// gone, which changes no sentence: none was an account's first issue.
func enteredOnPage(t *testing.T, want, got map[string]any) {
	t.Helper()
	gotAccounts, _ := got["accounts"].([]any)
	for i, raw := range want["accounts"].([]any) {
		account := raw.(map[string]any)
		stored := account["settings"].(map[string]any)
		linked, entered := strings.HasPrefix(account["id"].(string), "org-"), false
		for _, field := range [][3]string{{"monthlyCreditSource", "monthlyUsd", "configMonthlyUsd"}, {"renewsSource", "renews", "configRenews"}} {
			source, value, configured := field[0], field[1], field[2]
			switch {
			case account[source] != qc.CreditSourceConfig:
			case linked:
				account[source], stored[value], entered = qc.CreditSourceDashboard, stored[configured], true
			default:
				account[source] = qc.CreditSourceNone
				if source == "renewsSource" {
					account["cycleStartEpoch"], account["renewsAtEpoch"], account["renewsInSeconds"] = nil, nil, nil
				}
			}
			stored[configured] = ""
		}
		stored["configMonthlyUsdInvalid"], stored["configRenewsInvalid"] = false, false
		if entered && stored["revision"] == "" && i < len(gotAccounts) {
			page := gotAccounts[i].(map[string]any)["settings"].(map[string]any)
			if page["revision"] == "" || page["updatedAtEpoch"] == nil {
				t.Errorf("%s: a value entered on the page has no revision or time: %v", account["id"], page)
			}
			stored["revision"], stored["updatedAtEpoch"] = page["revision"], page["updatedAtEpoch"]
		}
		issues := []any{}
		for j, issue := range account["dataIssues"].([]any) {
			if slices.Contains(configIssues, issue.(string)) {
				if j == 0 {
					t.Fatalf("%s: %s is the first issue, so the sentence changes", account["id"], issue)
				}
				continue
			}
			issues = append(issues, issue)
		}
		account["dataIssues"] = issues
	}
}

// After the move to Quota Cache, each fixture's card, built from Quota Cache's
// published state for it, is the characterization byte for byte once the
// read-only fields are set aside, and those are read-only, and once the values
// quota-glance 0.7 read from Quota Cache's configuration are entered on its
// page instead.
func TestPublishedCreditsMatchTheCharacterization(t *testing.T) {
	for _, c := range characterizations {
		t.Run(c.name, func(t *testing.T) {
			credits := Build(Input{Credits: fixtureCredits(t, c.name), StaleAfter: 45 * time.Minute}, c.at).APICredits
			if credits == nil {
				t.Fatal("apiCredits is null")
			}
			raw, err := json.MarshalIndent(credits, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			characterization, err := os.ReadFile(characterizationPath(c.name))
			if err != nil {
				t.Fatal(err)
			}
			got, want := withoutEditing(t, raw), withoutEditing(t, characterization)
			enteredOnPage(t, want, got)
			if !reflect.DeepEqual(got, want) {
				gotText, _ := json.MarshalIndent(got, "", "  ")
				wantText, _ := json.MarshalIndent(want, "", "  ")
				t.Fatalf("%s: the published card differs from the characterization outside the read-only fields:\n got %s\nwant %s", c.name, gotText, wantText)
			}
			if credits.Editing != (APICreditEditing{Reason: qc.CreditEditingQuotaCache}) {
				t.Errorf("editing = %+v", credits.Editing)
			}
			for _, account := range credits.Accounts {
				if s := account.Settings; s.Editable || (s.NotEditableReason != qc.CreditEditingQuotaCache && !structuralReasons[s.NotEditableReason]) {
					t.Errorf("%s: settings %+v", account.ID, s)
				}
			}
		})
	}
}

// Quota Cache holds its computation to its own copies of the
// characterization; they must be these, byte for byte.
func TestCharacterizationMatchesQuotaCache(t *testing.T) {
	for _, c := range characterizations {
		ours, err := os.ReadFile(characterizationPath(c.name))
		if err != nil {
			t.Fatal(err)
		}
		theirs, err := os.ReadFile(filepath.Join(quotaCacheTestdata, "characterization", c.name+".json"))
		if err != nil {
			t.Fatalf("Quota Cache has no characterization of %s: %v", c.name, err)
		}
		if !bytes.Equal(ours, theirs) {
			t.Errorf("%s differs from Quota Cache's copy", c.name)
		}
	}
}
