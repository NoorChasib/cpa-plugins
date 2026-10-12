package credits

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/settings"
)

// fixtureNow is the instant the seven-credentials and degraded-states fixtures
// describe, the one Quota Glance builds its committed summary documents at.
const fixtureNow = 1789012800 // 2026-09-10T04:00:00Z

// fixture is one snapshot in testdata/snapshots, with the meter beside it, the
// settings it is computed with, and the instant it describes. The snapshots
// and meters are byte-for-byte Quota Glance's own fixtures.
type fixture struct {
	name string
	// settings names testdata/settings/<settings>.json, "" for none.
	settings string
	at       time.Time
}

var fixtures = []fixture{
	{"seven-credentials", "seven-credentials", time.Unix(fixtureNow, 0).UTC()},
	{"degraded-states", "degraded-states", time.Unix(fixtureNow, 0).UTC()},
	{"api-credits", "api-credits", time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)},
	// The worked example with a brief stop and a long one, and the same
	// settings.
	{"meter-gaps", "api-credits", time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)},
	// Reset Priority's routing fixture: four linked organizations and no
	// settings.
	{"api-credits-routing", "", time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)},
}

func fixtureNamed(t *testing.T, name string) fixture {
	t.Helper()
	for _, f := range fixtures {
		if f.name == name {
			return f
		}
	}
	t.Fatalf("no fixture %q", name)
	return fixture{}
}

// inputOf is what the publisher would pass for the fixture at its instant, once
// its organizations are on the page with their credits and refill dates
// (onPage): the item list, the meter, and the settings, with nothing stale.
func inputOf(t *testing.T, f fixture) Input {
	t.Helper()
	return onPage(snapshotInput(t, f))
}

// snapshotInput is the fixture's input with the snapshot's anthropic-api
// entries as the item list, as quota-cache 0.1.x wrote them: each still
// carries the credit and refill date its configuration gave it.
func snapshotInput(t *testing.T, f fixture) Input {
	t.Helper()
	path := filepath.Join("testdata", "snapshots", f.name+".json")
	snapshot, err := client.Load(path)
	if err != nil {
		t.Fatalf("fixture %s: %v", f.name, err)
	}
	meter, err := client.LoadMeter(client.MeterPath(path))
	if err != nil {
		t.Fatalf("meter beside %s: %v", f.name, err)
	}
	keys := []string{}
	for key, entry := range snapshot.Entries {
		if entry.Provider == client.ProviderAnthropicAPI {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	items := make([]Item, 0, len(keys))
	for _, key := range keys {
		entry := snapshot.Entries[key]
		items = append(items, Item{ID: entry.AuthIndex, Credit: entry.APICredit})
	}
	return Input{
		Now:           f.at,
		Snapshot:      snapshot,
		Items:         items,
		Meter:         meter,
		Settings:      loadSettings(t, f.settings),
		SettingsState: client.CreditSettingsOK,
		Organizations: client.CreditOrganizations{State: client.OrganizationsStateOK, Revision: "1"},
		Version:       "0.2.0",
	}
}

// pageEnteredAt is when onPage enters the values it moves to the page: before
// every fixture's instant.
var pageEnteredAt = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// onPage is in once each organization's monthly credit and refill date are
// entered on Quota Cache's page, the only place they are kept: the values
// quota-cache 0.1.x configured, which the fixtures' snapshot entries and
// creditEntry carry on the item. A value already stored wins, field by field,
// as it did over the configured one. Only an organization's own account takes
// values, and an invalid one is left out, as the page refuses both. A new
// entry is stamped with the settings file's revision, or 1 without one, and
// pageEnteredAt. Each item keeps only its label, position and organization,
// which is all the page's organizations give, and its problem. Only the
// degraded-states fixture has items the page never gives: item-<n> and
// label-<hex> accounts and items with a problem, which quota-cache 0.1.x
// listed from its configuration and this one never lists. They stay so that
// its golden, and the summary Quota Glance builds from it, cover the
// contract's misconfigured, cacheTooOld and overLimit values.
func onPage(in Input) Input {
	values := in.Settings
	values.APICredits = maps.Clone(values.APICredits)
	if values.APICredits == nil {
		values.APICredits = map[string]settings.APICredit{}
	}
	items := make([]Item, 0, len(in.Items))
	for _, item := range in.Items {
		c := item.Credit
		if c == nil {
			items = append(items, item)
			continue
		}
		monthly, renews := "", ""
		if client.ValidMonthlyUSD(c.MonthlyUSD) {
			monthly = c.MonthlyUSD
		}
		if day, err := client.ParseRenewal(c.Renews); err == nil {
			renews = day.Format(time.DateOnly)
		}
		if strings.HasPrefix(item.ID, "org-") && (monthly != "" || renews != "") {
			stored, has := values.APICredits[item.ID]
			if !has {
				stored = settings.APICredit{Rev: max(values.Revision, 1), UpdatedAt: pageEnteredAt}
				values.Revision = stored.Rev
			}
			if stored.MonthlyUSD == "" {
				stored.MonthlyUSD = monthly
			}
			if stored.Renews == "" {
				stored.Renews = renews
			}
			values.APICredits[item.ID] = stored
		}
		item.Credit = &client.APICredit{Label: c.Label, Position: c.Position, OrganizationID: c.OrganizationID, Problem: c.Problem}
		items = append(items, item)
	}
	in.Items, in.Settings = items, values
	return in
}

// computeFixture is the state quota-cache publishes for the fixture.
func computeFixture(t *testing.T, name string) *client.CreditState {
	t.Helper()
	return Compute(inputOf(t, fixtureNamed(t, name)))
}

// loadSettings reads testdata/settings/<name>.json, which is in Quota
// Glance's settings.json schema, into Values the way the store decodes it,
// without the store's checks: the store's own tests hold files to those.
func loadSettings(t *testing.T, name string) settings.Values {
	t.Helper()
	values := settings.Values{APICredits: map[string]settings.APICredit{}, Renewals: map[string]settings.Renewal{}}
	if name == "" {
		return values
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "settings", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Revision   uint64 `json:"revision"`
		APICredits map[string]struct {
			MonthlyUSD string `json:"monthlyUsd"`
			Renews     string `json:"renews"`
			Reading    *struct {
				RemainingUSD   string    `json:"remainingUsd"`
				At             time.Time `json:"at"`
				EnteredAt      time.Time `json:"enteredAt"`
				OrganizationID string    `json:"organizationId"`
				Baseline       struct {
					DayStart time.Time           `json:"dayStart"`
					Until    time.Time           `json:"until"`
					Usage    []client.MeterUsage `json:"usage"`
				} `json:"baseline"`
			} `json:"reading"`
			Rev       uint64    `json:"rev"`
			UpdatedAt time.Time `json:"updatedAt"`
		} `json:"apiCredits"`
		Renewals map[string]struct {
			Date      string    `json:"date"`
			Rev       uint64    `json:"rev"`
			UpdatedAt time.Time `json:"updatedAt"`
		} `json:"renewals"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("testdata/settings/%s.json: %v", name, err)
	}
	values.Revision = file.Revision
	for id, entry := range file.APICredits {
		credit := settings.APICredit{MonthlyUSD: entry.MonthlyUSD, Renews: entry.Renews, Rev: entry.Rev, UpdatedAt: entry.UpdatedAt}
		if r := entry.Reading; r != nil {
			usage := []client.MeterUsage{}
			for _, u := range r.Baseline.Usage {
				usage = client.MergeMeterUsage(usage, u)
			}
			credit.Reading = &settings.Reading{
				RemainingUSD: r.RemainingUSD, At: r.At, EnteredAt: r.EnteredAt, OrganizationID: r.OrganizationID,
				Baseline: settings.Baseline{DayStart: r.Baseline.DayStart, Until: r.Baseline.Until, Usage: usage},
			}
		}
		values.APICredits[id] = credit
	}
	for id, entry := range file.Renewals {
		values.Renewals[id] = settings.Renewal{Date: entry.Date, Rev: entry.Rev, UpdatedAt: entry.UpdatedAt}
	}
	return values
}
