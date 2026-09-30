package aggregate

import (
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// providerOpenRouter is quota-cache's provider name for the OpenRouter account.
// It is not a CPA credential: quota-cache polls it with a management key from
// its own configuration and drops the entry when that key is removed, so the
// snapshot entry — not the roster — is what says the account exists.
const providerOpenRouter = "openrouter"

// balancesOf renders every prepaid balance in the snapshot, ordered by provider
// and then by id so a document built twice is byte-identical.
func balancesOf(in Input, now time.Time) []Balance {
	keys := []string{}
	for key, entry := range in.Snapshot.Entries {
		if entry.Provider == providerOpenRouter {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	balances := make([]Balance, 0, len(keys))
	for _, key := range keys {
		balances = append(balances, balanceOf(in.Snapshot.Entries[key], in, now))
	}
	return balances
}

func balanceOf(entry qc.Entry, in Input, now time.Time) Balance {
	b := Balance{
		ID:               entry.AuthIndex,
		Provider:         entry.Provider,
		Title:            "OpenRouter",
		Order:            10,
		Currency:         "USD",
		WarnBelow:        in.BalanceWarnBelow,
		NextAttemptEpoch: epochOf(entry.NextAttempt),
		DataIssues:       []string{},
		State:            StatusPending,
	}
	purchased, used, observed, ok := creditsOf(entry)
	if ok {
		remaining := new(big.Rat).Sub(purchased, used)
		b.HasReading, b.State = true, StatusOK
		b.Remaining, _ = remaining.Float64()
		b.Purchased, _ = purchased.Float64()
		b.Used, _ = used.Float64()
		b.RemainingText = moneyOf(remaining)
		b.Level = balanceLevelOf(remaining, in.BalanceWarnBelow)
		b.ObservedAtEpoch = observed.Unix()
	}
	// The same judgements a window entry gets, in the same order: a failed poll
	// outranks one in flight, and staleness is about the reading, so a balance
	// that has none cannot be stale.
	switch {
	case entry.Failures > 0:
		b.DataIssues = append(b.DataIssues, issueObserveError)
		b.State = StatusError
	case entry.LastError != "":
		b.DataIssues = append(b.DataIssues, issueRefreshPending)
	}
	if b.HasReading && in.StaleAfter > 0 && now.Sub(observed) > in.StaleAfter {
		b.DataIssues = append(b.DataIssues, issueStale)
		if b.State == StatusOK {
			b.State = StateStale
		}
	}
	b.Subtext = balanceSubtextOf(b, purchased, used)
	return b
}

// creditsOf reads OpenRouter's two all-time totals off the entry.
//
// Both must be present and in dollars, or there is no reading: half a balance
// would be displayed as the other half's absence, and an amount in any other
// unit printed behind a dollar sign would be wrong by however much the units
// differ. Parsed exactly, because the difference of two amounts carrying nine
// decimal places is exactly where float subtraction prints $74.74999999.
func creditsOf(entry qc.Entry) (purchased, used *big.Rat, observed time.Time, ok bool) {
	if entry.Quota == nil || entry.Quota.ObservedAt.IsZero() {
		return nil, nil, time.Time{}, false
	}
	credits, found := entry.Quota.Balances["credits"]
	if !found || credits.Unit != "usd" {
		return nil, nil, time.Time{}, false
	}
	purchased, okPurchased := amountOf(credits.Limit)
	used, okUsed := amountOf(credits.Used)
	if !okPurchased || !okUsed {
		return nil, nil, time.Time{}, false
	}
	return purchased, used, entry.Quota.ObservedAt, true
}

// amountOf accepts a finite decimal and nothing else. big.Rat alone would also
// take "1/3", which is not an amount any provider reports.
func amountOf(text string) (*big.Rat, bool) {
	if text == "" || len(text) > 64 || strings.Contains(text, "/") {
		return nil, false
	}
	if value, err := strconv.ParseFloat(text, 64); err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil, false
	}
	return new(big.Rat).SetString(text)
}

// balanceLevelOf is the balance's counterpart to levelOf. Nothing left is
// critical whatever the threshold says, because at that point requests fail;
// below the threshold is low. A threshold of zero therefore means "tell me
// only when it has run out".
func balanceLevelOf(remaining *big.Rat, warnBelow float64) string {
	switch {
	case remaining.Sign() <= 0:
		return LevelCritical
	case remaining.Cmp(decimalOf(warnBelow)) < 0:
		return LevelLow
	}
	return LevelOK
}

// decimalOf is the threshold as the operator typed it. SetFloat64 would give
// the binary expansion instead — 2.1 is a hair above 2.1 — and put a balance
// of exactly $2.10 below a $2.10 warning.
func decimalOf(value float64) *big.Rat {
	r, _ := new(big.Rat).SetString(strconv.FormatFloat(value, 'f', -1, 64))
	return r
}

// balanceSubtextOf writes the line under the amount. It is timeless on
// purpose: the document is cached between rebuilds, so anything relative to
// now — "3h ago" — is the client's to tick, from observedAtEpoch.
func balanceSubtextOf(b Balance, purchased, used *big.Rat) string {
	if !b.HasReading {
		if b.State == StatusError {
			return "The balance could not be read. Check the OpenRouter management key in Quota Cache's settings."
		}
		return "Waiting for Quota Cache to read the balance."
	}
	threshold := moneyOf(decimalOf(b.WarnBelow))
	totals := moneyOf(used) + " spent of " + moneyOf(purchased) + " purchased"
	switch b.Level {
	case LevelCritical:
		return "Out of credit · " + totals
	case LevelLow:
		return "Below your " + threshold + " warning · " + totals
	}
	if b.WarnBelow > 0 {
		return totals + " · warns below " + threshold
	}
	return totals
}

// moneyOf prints an amount the way the dashboard shows it: "$1,234.56", and
// "-$1.20" when overdrawn. Rounded to the cent half away from zero, on the exact
// value; an amount that rounds to nothing is "$0.00" rather than "-$0.00".
func moneyOf(amount *big.Rat) string {
	text := amount.FloatString(2)
	negative := strings.HasPrefix(text, "-")
	text = strings.TrimPrefix(text, "-")
	whole, cents, _ := strings.Cut(text, ".")
	grouped := make([]byte, 0, len(whole)+len(whole)/3)
	for i := range len(whole) {
		if i > 0 && (len(whole)-i)%3 == 0 {
			grouped = append(grouped, ',')
		}
		grouped = append(grouped, whole[i])
	}
	text = "$" + string(grouped) + "." + cents
	if negative && text != "$0.00" {
		return "-" + text
	}
	return text
}
