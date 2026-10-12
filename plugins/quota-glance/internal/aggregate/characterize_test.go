package aggregate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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

// After the move to Quota Cache, each fixture's card, built from Quota Cache's
// published state for it, is the characterization byte for byte once the
// read-only fields are set aside, and those are read-only.
func TestPublishedCreditsMatchTheCharacterization(t *testing.T) {
	for _, c := range characterizations {
		t.Run(c.name, func(t *testing.T) {
			credits := Build(Input{Credits: fixtureCredits(t, c.name), StaleAfter: 45 * time.Minute}, c.at).APICredits
			if credits == nil {
				t.Fatal("apiCredits is null")
			}
			got, err := json.MarshalIndent(credits, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(characterizationPath(c.name))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(withoutEditing(t, got), withoutEditing(t, want)) {
				t.Fatalf("%s: the published card differs from the characterization outside the read-only fields", c.name)
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
