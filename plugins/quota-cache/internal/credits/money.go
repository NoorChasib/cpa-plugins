package credits

import (
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

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

func percentOf(fraction float64) int { return int(math.Round(fraction * 100)) }

// levelOf judges the rounded percent rather than the raw fraction, so the
// colour always agrees with the figure printed beside it: a pool reading
// 40.4% is printed "40%" and is amber, never a green "40".
func levelOf(remaining float64) string {
	switch percent := percentOf(remaining); {
	case percent <= client.CreditLevelCriticalAtOrBelow:
		return client.LevelCritical
	case percent <= client.CreditLevelLowAtOrBelow:
		return client.LevelLow
	}
	return client.LevelOK
}

// epochPointerOf is the nullable form, for the document fields that report
// "there is nothing to say yet" rather than a zero instant.
func epochPointerOf(t time.Time) *int64 {
	if t.IsZero() {
		return nil
	}
	epoch := t.Unix()
	return &epoch
}
