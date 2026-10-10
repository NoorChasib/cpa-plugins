package aggregate

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// openRouterEntry is quota-cache's shape for the account: no weekly window and
// an empty legacy observation, with the two totals under quota.balances.
func openRouterEntry(observed time.Time, purchased, used string) qc.Entry {
	return qc.Entry{
		Provider: "openrouter", AuthIndex: "key-000000000001",
		LastAttempt: observed, NextAttempt: observed.Add(15 * time.Minute),
		Quota: &qc.Quota{Schema: 1, ObservedAt: observed, Balances: map[string]qc.Balance{
			"credits": {Unit: "usd", Limit: purchased, Used: used},
		}},
	}
}

func buildBalance(t *testing.T, entry qc.Entry, warnBelow float64) Balance {
	t.Helper()
	snapshot := qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{
		qc.Key(entry.Provider, entry.AuthIndex): entry,
	}}
	doc := Build(Input{Snapshot: snapshot, StaleAfter: 45 * time.Minute, BalanceWarnBelow: warnBelow}, at(t, 0))
	if len(doc.Balances) != 1 {
		t.Fatalf("balances = %+v", doc.Balances)
	}
	return doc.Balances[0]
}

// The whole feature in one table: the amount left, and which side of the
// operator's threshold it is on. Nothing left is critical whatever the
// threshold, and zero turns the warning off without turning that off.
func TestBalanceLevelFollowsTheWarnBelowThreshold(t *testing.T) {
	for _, tc := range []struct {
		purchased, used string
		warnBelow       float64
		level, text     string
	}{
		{"100", "90", 5, LevelOK, "$10.00"},
		{"100", "95", 5, LevelOK, "$5.00"},
		{"100", "95.01", 5, LevelLow, "$4.99"},
		{"100", "99.99", 5, LevelLow, "$0.01"},
		{"100", "100", 5, LevelCritical, "$0.00"},
		{"100", "101.2", 5, LevelCritical, "-$1.20"},
		{"100", "99.99", 0, LevelOK, "$0.01"},
		{"100", "100", 0, LevelCritical, "$0.00"},
		{"100", "80", 25, LevelLow, "$20.00"},
		// A threshold typed as 2.1 is 2.1, not its binary expansion, so a
		// balance of exactly $2.10 is not below it.
		{"10", "7.9", 2.1, LevelOK, "$2.10"},
	} {
		b := buildBalance(t, openRouterEntry(at(t, -60), tc.purchased, tc.used), tc.warnBelow)
		if b.Level != tc.level || b.RemainingText != tc.text || !b.HasReading || b.State != StatusOK {
			t.Errorf("%s - %s below %v: level=%s text=%s hasReading=%v state=%s; want %s %s",
				tc.purchased, tc.used, tc.warnBelow, b.Level, b.RemainingText, b.HasReading, b.State, tc.level, tc.text)
		}
	}
}

// Nine decimal places is what OpenRouter reports for usage, and exactly where
// float subtraction starts printing $74.74999999.
func TestBalanceSubtractsExactly(t *testing.T) {
	b := buildBalance(t, openRouterEntry(at(t, -60), "100.5", "25.123456789"), 5)
	if b.Remaining != 75.376543211 || b.RemainingText != "$75.38" || b.Purchased != 100.5 || b.Used != 25.123456789 {
		t.Fatalf("balance = %+v", b)
	}
}

func TestMoneyIsPrintedTheWayTheDashboardShowsIt(t *testing.T) {
	for text, want := range map[string]string{
		"0":          "$0.00",
		"0.004":      "$0.00",
		"-0.004":     "$0.00",
		"0.005":      "$0.01",
		"999.995":    "$1,000.00",
		"1234.5":     "$1,234.50",
		"1000000":    "$1,000,000.00",
		"-1234.5":    "-$1,234.50",
		"12.3456789": "$12.35",
	} {
		amount, _ := new(big.Rat).SetString(text)
		if got := moneyOf(amount); got != want {
			t.Errorf("moneyOf(%s) = %q; want %q", text, got, want)
		}
	}
}

func TestBalanceSubtextIsWrittenOutAndTimeless(t *testing.T) {
	for _, tc := range []struct {
		used, want string
	}{
		{"25.75", "$25.75 spent of $100.50 purchased · warns below $5.00"},
		{"97.3", "Below your $5.00 warning · $97.30 spent of $100.50 purchased"},
		{"100.5", "Out of credit · $100.50 spent of $100.50 purchased"},
	} {
		if got := buildBalance(t, openRouterEntry(at(t, -60), "100.5", tc.used), 5).Subtext; got != tc.want {
			t.Errorf("subtext = %q; want %q", got, tc.want)
		}
	}
	if got := buildBalance(t, openRouterEntry(at(t, -60), "100.5", "25.75"), 0).Subtext; got != "$25.75 spent of $100.50 purchased" {
		t.Errorf("with the warning off, subtext = %q", got)
	}
}

// Before the first successful poll there is no balance, and $0.00 in red would
// say the opposite of the truth.
func TestUnreadBalanceIsADashNotZero(t *testing.T) {
	pending := qc.Entry{Provider: "openrouter", AuthIndex: "key-000000000001", LastError: "refresh pending", LastAttempt: at(t, -5)}
	b := buildBalance(t, pending, 5)
	if b.HasReading || b.Level != "" || b.RemainingText != "" || b.State != StatusPending ||
		len(b.DataIssues) != 1 || b.DataIssues[0] != issueRefreshPending || !strings.Contains(b.Subtext, "Waiting") {
		t.Fatalf("pending = %+v", b)
	}

	failed := qc.Entry{Provider: "openrouter", AuthIndex: "key-000000000001", LastError: "quota fetch failed", Failures: 3}
	b = buildBalance(t, failed, 5)
	if b.HasReading || b.Level != "" || b.State != StatusError || !strings.Contains(b.Subtext, "management key") {
		t.Fatalf("failed = %+v", b)
	}
}

// A failed poll keeps the last good figures up, marked; so does an old one.
func TestLastGoodBalanceSurvivesAFailureAndAges(t *testing.T) {
	entry := openRouterEntry(at(t, -3*3600), "40", "36.8")
	entry.Failures, entry.LastError = 2, "quota fetch failed"
	b := buildBalance(t, entry, 5)
	if !b.HasReading || b.RemainingText != "$3.20" || b.Level != LevelLow || b.State != StatusError ||
		strings.Join(b.DataIssues, ",") != issueObserveError+","+issueStale {
		t.Fatalf("balance = %+v", b)
	}

	b = buildBalance(t, openRouterEntry(at(t, -3*3600), "40", "10"), 5)
	if b.State != StateStale || strings.Join(b.DataIssues, ",") != issueStale {
		t.Fatalf("an hours-old reading is not stale: %+v", b)
	}
}

// Anything that is not two dollar totals is not a reading. Printing cents or
// provider units behind a dollar sign would be wrong by the conversion factor.
func TestBalanceRefusesWhatIsNotTwoDollarTotals(t *testing.T) {
	for name, balance := range map[string]qc.Balance{
		"cents":      {Unit: "usd_cents", Limit: "10000", Used: "2500"},
		"no unit":    {Limit: "100", Used: "25"},
		"half":       {Unit: "usd", Limit: "100"},
		"fraction":   {Unit: "usd", Limit: "1/3", Used: "0"},
		"not a sum":  {Unit: "usd", Limit: "lots", Used: "0"},
		"infinite":   {Unit: "usd", Limit: "1e999", Used: "0"},
		"not number": {Unit: "usd", Limit: "NaN", Used: "0"},
	} {
		entry := openRouterEntry(at(t, -60), "", "")
		entry.Quota.Balances["credits"] = balance
		if b := buildBalance(t, entry, 5); b.HasReading || b.Level != "" {
			t.Errorf("%s: read as %+v", name, b)
		}
	}
}

// The account is not a credential. It must not appear among them, inflate the
// footer's counts, or move the header's instants, all of which describe CPA's
// roster.
func TestBalanceIsNotACredential(t *testing.T) {
	plain := buildFixture(t)
	snapshot := loadSnapshot(t, "seven-credentials.json")
	meter, values := fixtureExtras(t, "seven-credentials")
	withoutBalance := Build(Input{
		Snapshot: func() qc.Snapshot {
			entries := map[string]qc.Entry{}
			for key, entry := range snapshot.Entries {
				if entry.Provider != "openrouter" {
					entries[key] = entry
				}
			}
			snapshot.Entries = entries
			return snapshot
		}(),
		Identities: fixtureRoster(), StaleAfter: 45 * time.Minute, Redeemable: true, BalanceWarnBelow: 5,
		Meter: meter, Overrides: values, AllowEdit: true,
	}, at(t, 0))
	if len(plain.Balances) != 1 || len(withoutBalance.Balances) != 0 {
		t.Fatalf("balances: with=%d without=%d", len(plain.Balances), len(withoutBalance.Balances))
	}
	plain.Balances, withoutBalance.Balances = nil, nil
	a, _ := json.Marshal(plain)
	b, _ := json.Marshal(withoutBalance)
	if string(a) != string(b) {
		t.Fatal("the OpenRouter account changed something other than balances[]")
	}
}

func TestNoBalanceIsAnEmptyArrayNotNull(t *testing.T) {
	doc := Build(Input{Snapshot: qc.Snapshot{Schema: 1, Entries: map[string]qc.Entry{}}}, at(t, 0))
	raw, _ := json.Marshal(doc)
	if !strings.Contains(string(raw), `"balances":[]`) {
		t.Fatalf("document = %s", raw)
	}
}

// The degraded contract is what the web app develops the unhappy paths
// against, so the balance card's must be in it too.
func TestDegradedContractCoversTheBalanceCard(t *testing.T) {
	doc := buildDegraded(t)
	if len(doc.Balances) == 0 {
		t.Fatal("no balance in the degraded contract")
	}
	b := doc.Balances[0]
	if !b.HasReading || b.Level != LevelLow || b.State != StatusError ||
		strings.Join(b.DataIssues, ",") != issueObserveError+","+issueStale {
		t.Fatalf("degraded balance = %+v", b)
	}
}
