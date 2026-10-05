package aggregate

import (
	"math/big"
	"strings"
)

// Units a Credits figure is reported in.
const (
	creditsUnitCredits = "credits"
	creditsUnitUSD     = "usd"
)

// accountCreditsOf reads the one balance each provider reports as money the
// account can spend beyond its windows, and is nil for every other provider and
// whenever that balance is absent.
//
// Each provider names a different entry, and only that entry: Codex reports its
// credit balance under "credits", and Grok its prepaid dollars under "prepaid".
// Grok's other balances are monthly allowance and an on-demand cap, which are
// limits rather than money held, and printing one of them as "credit" would be
// a figure with the wrong meaning.
//
// It reads the last successful observation, stale or not, as the reset count
// does: a balance moves when the operator spends, and the credential's status
// already says how old the poll is.
func accountCreditsOf(r record) *Credits {
	if !r.hasEntry || r.entry.Quota == nil {
		return nil
	}
	switch r.identity.Provider {
	case "codex":
		balance, ok := r.entry.Quota.Balances["credits"]
		if !ok || balance.Unit != creditsUnitCredits {
			return nil
		}
		if balance.Unlimited != nil && *balance.Unlimited {
			return &Credits{Display: "Unlimited", Unlimited: true, Unit: creditsUnitCredits}
		}
		amount, ok := amountOf(balance.Remaining)
		if !ok {
			return nil
		}
		return &Credits{Display: creditsTextOf(amount), Amount: exactDecimalOf(amount, 0), Unit: creditsUnitCredits}
	case "xai":
		balance, ok := r.entry.Quota.Balances["prepaid"]
		if !ok || balance.Unit != "usd_cents" {
			return nil
		}
		cents, ok := amountOf(balance.Remaining)
		if !ok {
			return nil
		}
		dollars := new(big.Rat).Quo(cents, big.NewRat(100, 1))
		return &Credits{Display: moneyOf(dollars), Amount: exactDecimalOf(dollars, 2), Unit: creditsUnitUSD}
	}
	return nil
}

// creditsTextOf prints a credit balance the way the dashboard shows it:
// thousands grouped, at most two decimals rounded half away from zero, and
// trailing zeros dropped, so 57706.149 is "57,706.15", 120.50 is "120.5", and
// anything that rounds to nothing is "0" rather than "0.00" or "-0".
//
// Credits are not money and carry no currency symbol. They are counted rather
// than priced, so a balance of 100 reads "100", not "100.00".
func creditsTextOf(amount *big.Rat) string {
	text := amount.FloatString(2)
	negative := strings.HasPrefix(text, "-")
	whole, fraction, _ := strings.Cut(strings.TrimPrefix(text, "-"), ".")
	fraction = strings.TrimRight(fraction, "0")
	text = groupThousands(whole)
	if fraction != "" {
		text += "." + fraction
	}
	if negative && text != "0" {
		return "-" + text
	}
	return text
}

// groupThousands puts a comma between every three digits of an unsigned whole
// number, counting from the right.
func groupThousands(whole string) string {
	grouped := make([]byte, 0, len(whole)+len(whole)/3)
	for i := range len(whole) {
		if i > 0 && (len(whole)-i)%3 == 0 {
			grouped = append(grouped, ',')
		}
		grouped = append(grouped, whole[i])
	}
	return string(grouped)
}

// exactDecimalOf writes an amount as a plain decimal, with no exponent and no
// grouping, carrying every digit it has and at least minDigits after the point.
// Every amount here was parsed from a terminating decimal, so the expansion
// ends; the cap is only there so a pathological one cannot run on.
func exactDecimalOf(amount *big.Rat, minDigits int) string {
	const maxDigits = 18
	scaled := new(big.Rat)
	ten := big.NewRat(10, 1)
	digits := minDigits
	scaled.Set(amount)
	for range digits {
		scaled.Mul(scaled, ten)
	}
	for !scaled.IsInt() && digits < maxDigits {
		scaled.Mul(scaled, ten)
		digits++
	}
	return amount.FloatString(digits)
}
