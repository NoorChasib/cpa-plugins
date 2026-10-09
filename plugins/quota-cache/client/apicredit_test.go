package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func utc(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
}

// The one cycle rule both plugins share. Every row names the instant asked
// about and the cycle that must contain it.
func TestCreditCycleAt(t *testing.T) {
	for _, tc := range []struct {
		name       string
		renews     string
		at         time.Time
		start, end time.Time
	}{
		{"mid cycle", "2026-10-29", utc(2026, 10, 9, 12, 0), utc(2026, 9, 29, 0, 0), utc(2026, 10, 29, 0, 0)},
		{"after this month's renewal", "2026-10-29", utc(2026, 10, 30, 1, 0), utc(2026, 10, 29, 0, 0), utc(2026, 11, 29, 0, 0)},
		{"renewal instant starts the new cycle", "2026-10-29", utc(2026, 10, 29, 0, 0), utc(2026, 10, 29, 0, 0), utc(2026, 11, 29, 0, 0)},
		{"last second of the old cycle", "2026-10-29", utc(2026, 10, 29, 0, 0).Add(-time.Second), utc(2026, 9, 29, 0, 0), utc(2026, 10, 29, 0, 0)},
		{"renews today, later in the day", "2026-09-10", utc(2026, 9, 10, 4, 0), utc(2026, 9, 10, 0, 0), utc(2026, 10, 10, 0, 0)},
		{"only the day of the month matters", "2031-03-29", utc(2026, 10, 9, 12, 0), utc(2026, 9, 29, 0, 0), utc(2026, 10, 29, 0, 0)},
		{"a past anchor works alike", "2024-01-29", utc(2026, 10, 9, 12, 0), utc(2026, 9, 29, 0, 0), utc(2026, 10, 29, 0, 0)},
		{"day 1", "2026-01-01", utc(2026, 12, 31, 23, 59), utc(2026, 12, 1, 0, 0), utc(2027, 1, 1, 0, 0)},
		{"day 1 across the year", "2026-01-01", utc(2027, 1, 1, 0, 0), utc(2027, 1, 1, 0, 0), utc(2027, 2, 1, 0, 0)},
		{"day 31 clamps to Nov 30", "2026-10-31", utc(2026, 11, 15, 0, 0), utc(2026, 10, 31, 0, 0), utc(2026, 11, 30, 0, 0)},
		{"day 31 returns to Dec 31", "2026-10-31", utc(2026, 12, 1, 0, 0), utc(2026, 11, 30, 0, 0), utc(2026, 12, 31, 0, 0)},
		{"day 31 clamps to Feb 28", "2026-10-31", utc(2027, 3, 1, 0, 0), utc(2027, 2, 28, 0, 0), utc(2027, 3, 31, 0, 0)},
		{"day 31 clamps to Feb 29 in a leap year", "2026-10-31", utc(2028, 3, 1, 0, 0), utc(2028, 2, 29, 0, 0), utc(2028, 3, 31, 0, 0)},
		{"day 31 in Feb before the clamped renewal", "2026-10-31", utc(2027, 2, 27, 23, 0), utc(2027, 1, 31, 0, 0), utc(2027, 2, 28, 0, 0)},
		{"day 30 in a leap Feb", "2026-01-30", utc(2028, 2, 29, 12, 0), utc(2028, 2, 29, 0, 0), utc(2028, 3, 30, 0, 0)},
		{"day 30 before Feb's clamped renewal", "2026-01-30", utc(2027, 2, 27, 12, 0), utc(2027, 1, 30, 0, 0), utc(2027, 2, 28, 0, 0)},
		{"day 29 outside a leap year", "2026-01-29", utc(2027, 3, 1, 0, 0), utc(2027, 2, 28, 0, 0), utc(2027, 3, 29, 0, 0)},
		{"day 29 on a leap day", "2026-01-29", utc(2028, 2, 29, 0, 0), utc(2028, 2, 29, 0, 0), utc(2028, 3, 29, 0, 0)},
		{"January from a day-31 December", "2026-12-31", utc(2027, 1, 5, 0, 0), utc(2026, 12, 31, 0, 0), utc(2027, 1, 31, 0, 0)},
		{"panel round-trip form", "2026-10-29T00:00:00Z", utc(2026, 10, 9, 12, 0), utc(2026, 9, 29, 0, 0), utc(2026, 10, 29, 0, 0)},
		{"an offset form at UTC midnight", "2026-10-29T00:00:00+00:00", utc(2026, 10, 9, 12, 0), utc(2026, 9, 29, 0, 0), utc(2026, 10, 29, 0, 0)},
		{"surrounding spaces", "  2026-10-29 ", utc(2026, 10, 9, 12, 0), utc(2026, 9, 29, 0, 0), utc(2026, 10, 29, 0, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cycle, err := CreditCycleAt(tc.renews, tc.at)
			if err != nil {
				t.Fatal(err)
			}
			if !cycle.Start.Equal(tc.start) || !cycle.End.Equal(tc.end) {
				t.Fatalf("cycle = [%s, %s), want [%s, %s)", cycle.Start, cycle.End, tc.start, tc.end)
			}
			if cycle.Start.Location() != time.UTC || cycle.End.Location() != time.UTC {
				t.Fatal("cycle bounds must be in UTC")
			}
			if tc.at.Before(cycle.Start) || !tc.at.Before(cycle.End) {
				t.Fatal("cycle does not contain the instant asked about")
			}
		})
	}
}

// The calendar is UTC whatever zone the instant arrives in: 22:30 on Oct 28 in
// New York is already Oct 29 in UTC, past a renewal on the 29th.
func TestCreditCycleIgnoresTheCallersZone(t *testing.T) {
	newYork := time.FixedZone("EDT", -4*3600)
	cycle, err := CreditCycleAt("2026-10-29", time.Date(2026, 10, 28, 22, 30, 0, 0, newYork))
	if err != nil {
		t.Fatal(err)
	}
	if !cycle.Start.Equal(utc(2026, 10, 29, 0, 0)) {
		t.Fatalf("start = %s", cycle.Start)
	}
}

// Consecutive cycles tile time with no gap and no overlap, for every renewal
// day across a span with short months and a leap year.
func TestCreditCyclesTileTime(t *testing.T) {
	for day := 1; day <= 31; day++ {
		// December has every day of the month, so it can anchor all 31.
		renews := utc(2026, 12, day, 0, 0).Format(time.DateOnly)
		at := utc(2026, 1, 1, 0, 0)
		previous, err := CreditCycleAt(renews, at)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 40; i++ {
			next, err := CreditCycleAt(renews, previous.End)
			if err != nil {
				t.Fatal(err)
			}
			if !next.Start.Equal(previous.End) {
				t.Fatalf("day %d: cycle after %s starts at %s", day, previous.End, next.Start)
			}
			length := next.End.Sub(next.Start)
			if length < 28*24*time.Hour || length > 31*24*time.Hour {
				t.Fatalf("day %d: cycle %s is %s long", day, next.Start, length)
			}
			want := day
			if last := time.Date(next.Start.Year(), next.Start.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day(); want > last {
				want = last
			}
			if next.Start.Day() != want {
				t.Fatalf("day %d: cycle starts on day %d, want %d", day, next.Start.Day(), want)
			}
			previous = next
		}
	}
}

func TestParseRenewalRejects(t *testing.T) {
	for _, value := range []string{
		"", "tomorrow", "2026-02-30", "2026-13-01", "2026-10-29T00:00:01Z", "2026-10-29T00:00:00+04:00",
		"2026-10-29T12:00:00Z", "1999-12-31", "2100-01-01", "29/10/2026", "2026-10-29 00:00:00",
	} {
		if _, err := ParseRenewal(value); err == nil {
			t.Errorf("ParseRenewal(%q) accepted", value)
		}
		if _, err := CreditCycleAt(value, utc(2026, 10, 9, 0, 0)); err == nil {
			t.Errorf("CreditCycleAt(%q) accepted", value)
		}
	}
	if got, err := ParseRenewal("2028-02-29"); err != nil || !got.Equal(utc(2028, 2, 29, 0, 0)) {
		t.Fatalf("leap day = %v, %v", got, err)
	}
}

func TestValidMonthlyUSD(t *testing.T) {
	for _, ok := range []string{"0", "200", "260.5", "260.50", "500.00", "1234567"} {
		if !ValidMonthlyUSD(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "-1", "200.123", "$200", "200 ", "1e3", "12345678", ".5", "5.", "2,000"} {
		if ValidMonthlyUSD(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The account survives key rotation and case or spacing changes to the label,
// and two labels never share one.
func TestAPICreditAccount(t *testing.T) {
	a := APICreditAccount("siphorchannel")
	if a != APICreditAccount("  SiphorChannel ") {
		t.Fatal("label case or spacing changed the account")
	}
	if a == APICreditAccount("team") {
		t.Fatal("two labels share an account")
	}
	if !strings.HasPrefix(a, "label-") || len(a) != len("label-")+12 {
		t.Fatalf("account = %q", a)
	}
	if strings.Contains(a, "siphorchannel") {
		t.Fatal("the label leaked into the account id")
	}
}

// The new fields are additive: an entry without them gains no key, consumers
// built before them decode the snapshot unchanged, and the wire names are the
// contract quota-glance reads.
func TestAPICreditFieldsAreAdditive(t *testing.T) {
	observed := utc(2026, 10, 9, 12, 0)
	entry := Entry{
		Provider: ProviderAnthropicAPI, AuthIndex: APICreditAccount("team"),
		LastAttempt: observed, NextAttempt: observed.Add(15 * time.Minute),
		APICredit: &APICredit{Label: "team", Position: 4, MonthlyUSD: "500", Renews: "2026-10-03", KeyFingerprint: "key-0123456789ab"},
		Quota: &Quota{Schema: 1, ObservedAt: observed, CostReport: &CostReport{
			OrganizationID: "00000000-0000-4000-8000-000000000001",
			KeyFingerprint: "key-0123456789ab", StartingAt: utc(2026, 10, 3, 0, 0), EndingAt: utc(2026, 10, 10, 0, 0),
			Days: []CostDay{
				{StartingAt: utc(2026, 10, 3, 0, 0), Amounts: []CostAmount{{Amount: "12345.678", Currency: "USD"}}},
				{StartingAt: utc(2026, 10, 4, 0, 0), Amounts: []CostAmount{}},
			},
		}},
	}
	raw, err := json.Marshal(Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]Entry{Key(entry.Provider, entry.AuthIndex): entry}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"api_credit":{"label":"team","position":4,"monthly_usd":"500","renews":"2026-10-03","key_fingerprint":"key-0123456789ab"}`,
		`"cost_report":{"organization_id":"00000000-0000-4000-8000-000000000001","key_fingerprint":"key-0123456789ab","starting_at":"2026-10-03T00:00:00Z","ending_at":"2026-10-10T00:00:00Z","days":[`,
		`{"starting_at":"2026-10-03T00:00:00Z","amounts":[{"amount":"12345.678","currency":"USD"}]}`,
		`{"starting_at":"2026-10-04T00:00:00Z","amounts":[]}`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("snapshot is missing %s\n%s", want, raw)
		}
	}

	path := filepath.Join(t.TempDir(), "snapshot.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := snapshot.Entries[Key(entry.Provider, entry.AuthIndex)]
	if got.APICredit == nil || *got.APICredit != *entry.APICredit || got.Quota.CostReport == nil || len(got.Quota.CostReport.Days) != 2 {
		t.Fatalf("round trip lost the credit: %+v", got)
	}
	// The weekly readers stay closed to an entry that has no weekly window.
	if _, err := ReadFresh(path, entry.Provider, entry.AuthIndex, observed, time.Hour); err == nil {
		t.Fatal("ReadFresh accepted an entry with no weekly observation")
	}

	// A decoder built before these fields existed reads the entry unchanged.
	var legacy struct {
		Entries map[string]struct {
			Provider  string `json:"provider"`
			AuthIndex string `json:"auth_index"`
			Quota     *struct {
				Schema int `json:"schema"`
			} `json:"quota"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	if old := legacy.Entries[Key(entry.Provider, entry.AuthIndex)]; old.Provider != ProviderAnthropicAPI || old.Quota == nil || old.Quota.Schema != 1 {
		t.Fatalf("legacy decode = %+v", old)
	}

	// Every other provider's entry is byte-identical to before.
	bare, _ := json.Marshal(Entry{Provider: "claude", AuthIndex: "one", Quota: &Quota{Schema: 1}})
	for _, key := range []string{"api_credit", "cost_report"} {
		if strings.Contains(string(bare), key) {
			t.Fatalf("empty %s serialized: %s", key, bare)
		}
	}
}

// The window a poll asks for ends at the next 00:00 UTC: never a future
// bucket, never past the cycle, and never more buckets than one page holds.
func TestCostReportEndingAt(t *testing.T) {
	newYork := time.FixedZone("EDT", -4*3600)
	for _, tc := range []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"mid day", utc(2026, 10, 9, 12, 0), utc(2026, 10, 10, 0, 0)},
		{"midnight exactly", utc(2026, 10, 9, 0, 0), utc(2026, 10, 10, 0, 0)},
		{"last second of the day", utc(2026, 10, 10, 0, 0).Add(-time.Second), utc(2026, 10, 10, 0, 0)},
		{"month end", utc(2026, 10, 31, 23, 0), utc(2026, 11, 1, 0, 0)},
		{"year end", utc(2026, 12, 31, 23, 59), utc(2027, 1, 1, 0, 0)},
		{"leap day", utc(2028, 2, 29, 6, 0), utc(2028, 3, 1, 0, 0)},
		{"the caller's zone is ignored", time.Date(2026, 10, 9, 22, 30, 0, 0, newYork), utc(2026, 10, 11, 0, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CostReportEndingAt(tc.now); !got.Equal(tc.want) || got.Location() != time.UTC {
				t.Fatalf("CostReportEndingAt(%s) = %s, want %s", tc.now, got, tc.want)
			}
		})
	}
	// For every renewal day, every hour across a leap year: the window starts
	// at the cycle's start, ends after now and no later than the cycle's end,
	// and holds at most 31 daily buckets, the cost report's page limit.
	for day := 1; day <= 31; day++ {
		renews := utc(2026, 12, day, 0, 0).Format(time.DateOnly)
		for now := utc(2027, 12, 1, 0, 30); now.Before(utc(2029, 1, 1, 0, 0)); now = now.Add(7 * time.Hour) {
			cycle, err := CreditCycleAt(renews, now)
			if err != nil {
				t.Fatal(err)
			}
			end := CostReportEndingAt(now)
			if !end.After(now) || end.After(cycle.End) || end.Sub(cycle.Start) > 31*24*time.Hour {
				t.Fatalf("day %d at %s: window [%s, %s) against cycle [%s, %s)", day, now, cycle.Start, end, cycle.Start, cycle.End)
			}
		}
	}
}

// CoveredUntil stops at the first day that does not follow the one before,
// so a missing day is never read as a day without spend.
func TestCostReportCoveredUntil(t *testing.T) {
	start := utc(2026, 10, 3, 0, 0)
	day := func(d int) CostDay {
		return CostDay{StartingAt: start.Add(time.Duration(d) * 24 * time.Hour), Amounts: []CostAmount{}}
	}
	for _, tc := range []struct {
		name string
		days []CostDay
		want time.Time
	}{
		{"no days covers nothing", []CostDay{}, start},
		{"one day", []CostDay{day(0)}, utc(2026, 10, 4, 0, 0)},
		{"seven days", []CostDay{day(0), day(1), day(2), day(3), day(4), day(5), day(6)}, utc(2026, 10, 10, 0, 0)},
		{"a gap ends the coverage", []CostDay{day(0), day(1), day(3)}, utc(2026, 10, 5, 0, 0)},
		{"a first day after the start covers nothing", []CostDay{day(1), day(2)}, start},
		{"a repeated day ends the coverage", []CostDay{day(0), day(0), day(1)}, utc(2026, 10, 4, 0, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := &CostReport{StartingAt: start, Days: tc.days}
			if got := report.CoveredUntil(); !got.Equal(tc.want) {
				t.Fatalf("CoveredUntil = %s, want %s", got, tc.want)
			}
		})
	}
	var missing *CostReport
	if !missing.CoveredUntil().IsZero() {
		t.Fatal("a nil report covers something")
	}
}
