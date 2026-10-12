package settings

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// The settings file is Quota Glance's settings.json, byte for byte, so a
// rollback can copy one over the other (TestSettingsFileIsGlanceCompatible).
//
// testdata/legacy holds Quota Glance's own fixtures, copied from
// plugins/quota-glance/testdata/overrides, and files Quota Glance 0.7's
// decoder loads (a five-organization file, and one with every edge its rules
// allow: compact, keys in another order, entries with no value, (other), the
// over_100k prompt and usage to merge). testdata/legacy/glance holds what
// Quota Glance 0.7.2's overrides package made of each, through its exported
// API only:
//
//   - <name>.values.json is overrides.Load of the file, as JSON;
//   - <name>.saved.json is the file overrides' store wrote after the two
//     saves compatSaves makes, from that file ("empty": from no file).

const (
	compatOrg     = "00000000-0000-4000-8000-0000000000e1"
	compatRenewal = "ffffffffffffff01"
)

// compatNow is when compatSaves makes its first save.
var compatNow = time.Date(2026, 10, 9, 14, 0, 0, 0, time.UTC)

// compatSaves are the two saves the Quota Glance side made: a renewal date,
// then a credit with a new reading whose baseline comes from a meter.
func compatSaves(t *testing.T, s *Store) {
	t.Helper()
	now := compatNow
	if _, err := s.Apply(Batch{Kind: KindRenewals, Renewals: []RenewalItem{{ID: compatRenewal, Date: ptr("2026-11-01")}}}, nil, now); err != nil {
		t.Fatal(err)
	}
	meter := &client.APIMeter{Schema: 1, FlushedAt: at(t, "2026-10-09T13:59:00Z"), Organizations: map[string]client.MeterOrganization{
		compatOrg: {Since: at(t, "2026-09-01T00:00:00Z"), Days: []client.MeterBucket{}, Hours: []client.MeterBucket{
			{Start: at(t, "2026-10-09T09:00:00Z"), Usage: []client.MeterUsage{{Model: "claude-sonnet-5-5", Requests: 3, Input: 1200, Output: 800}}},
			{Start: at(t, "2026-10-09T13:00:00Z"), Usage: []client.MeterUsage{{Model: "claude-sonnet-5-5", Input: 50}}},
		}},
	}}
	item := APICreditItem{ID: client.APICreditOrgAccount(compatOrg), MonthlyUSD: ptr("123.45"),
		Reading: &ReadingInput{RemainingUSD: "100.00", At: "2026-10-09T13:20:00Z"}, OrganizationID: compatOrg}
	if _, err := s.Apply(Batch{Kind: KindAPICredits, APICredits: []APICreditItem{item}}, meter, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsFileIsGlanceCompatible(t *testing.T) {
	names, err := filepath.Glob(filepath.Join("testdata", "legacy", "*.json"))
	if err != nil || len(names) < 5 {
		t.Fatalf("fixtures %v, %v", names, err)
	}
	for _, path := range append(names, "") {
		name := "empty"
		dir := t.TempDir()
		snapshot := snapshotIn(dir)
		from := uint64(0)
		if path != "" {
			name = strings.TrimSuffix(filepath.Base(path), ".json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// It loads to exactly the values Quota Glance's decoder gives.
			v, err := decode(raw)
			if err != nil {
				t.Errorf("%s: does not load: %v", name, err)
				continue
			}
			got, _ := json.MarshalIndent(v, "", "  ")
			if want := readGlance(t, name+".values.json"); !bytes.Equal(append(got, '\n'), want) {
				t.Errorf("%s: loads as\n%s\nQuota Glance loads\n%s", name, got, want)
			}
			from = v.Revision
			if err := os.WriteFile(FilePath(snapshot), raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		// The same saves write the bytes Quota Glance's store writes.
		s := Open(snapshot)
		compatSaves(t, s)
		saved, err := os.ReadFile(s.Path())
		if err != nil {
			t.Fatal(err)
		}
		want := readGlance(t, name+".saved.json")
		// Each save writes at least the time in Unix seconds where Quota
		// Glance's went one past the file's revision, so a page that read a
		// file since deleted or replaced holds no revision it could match,
		// even across a restart. Counted as Quota Glance counts, the bytes
		// are Quota Glance's.
		saved = renumbered(t, saved, from)
		if !bytes.Equal(saved, want) {
			t.Errorf("%s: saved\n%s\nQuota Glance saved\n%s", name, saved, want)
		}
		// And Quota Glance's file re-encodes to itself here: the encoding is
		// Quota Glance's for the same values, and what this package commits
		// loads back under the same rules.
		v, err := decode(want)
		if err != nil {
			t.Errorf("%s: Quota Glance's saved file does not load: %v", name, err)
			continue
		}
		if again, err := encode(v); err != nil || !bytes.Equal(again, want) {
			t.Errorf("%s: re-encoded\n%s\nwant\n%s", name, again, want)
		}
		if Load(s.Path()).Unreadable {
			t.Errorf("%s: the file written does not load", name)
		}
	}
}

// renumbered is the settings file raw that compatSaves wrote over a file at
// revision from, 0 for none, with the revisions its two saves gave out
// numbered as Quota Glance's store numbers them: from+1 for the first, which
// wrote the renewal date, and from+2 for the second, the file's revision now.
// Every other rev is left as it is.
func renumbered(t *testing.T, raw []byte, from uint64) []byte {
	t.Helper()
	v, err := decode(raw)
	first := v.Renewals[compatRenewal].Rev
	if err != nil || first <= from || v.Revision <= first {
		t.Fatalf("revision %d, first save %d, from %d: %v", v.Revision, first, from, err)
	}
	glance := map[uint64]uint64{first: from + 1, v.Revision: from + 2}
	renumber := func(rev uint64) uint64 {
		if to, ok := glance[rev]; ok {
			return to
		}
		return rev
	}
	next := v.clone()
	next.Revision = renumber(next.Revision)
	for id, credit := range next.APICredits {
		credit.Rev = renumber(credit.Rev)
		next.APICredits[id] = credit
	}
	for id, renewal := range next.Renewals {
		renewal.Rev = renumber(renewal.Rev)
		next.Renewals[id] = renewal
	}
	out, err := encode(next)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func readGlance(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "legacy", "glance", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
