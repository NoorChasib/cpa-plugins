package credits

import (
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// openAISentenceOf is an OpenAI account's sentence for the issues whose
// Claude sentence names Console or Anthropic, or says something only true of
// Claude's meter. Every other issue it leaves to sentenceOf, whose words fit
// both: false.
func openAISentenceOf(a *creditAccount, issue string) (string, bool) {
	out := &a.out
	switch issue {
	case client.CreditIssueMeterMissing:
		return "Quota Cache has not saved its OpenAI API meter yet; counting starts when it next saves.", true
	case client.CreditIssueNeedsRefillDate:
		return "Set the refill date, or enter an API Platform reading.", true
	case client.CreditIssueRefused:
		sentence := "OpenAI refused a request with credit_balance_exhausted, so this credit is spent."
		if a.estimate != nil && a.estimate.Sign() > 0 {
			sentence += " The estimate had " + moneyOf(a.estimate) + " left; enter an API Platform reading to correct it."
		}
		return sentence, true
	case client.CreditIssueMeterStartedLate:
		switch out.Basis {
		case client.CreditBasisCredit:
			return "Counting began after this cycle started, so earlier spend is missing. Enter an API Platform reading to correct it.", true
		case client.CreditBasisReading:
			return "Counting began after your API Platform reading, so spend in between is missing.", true
		}
	case client.CreditIssueMeterGap:
		return client.CreditSentenceOpenAIMeterGap, true
	case client.CreditIssueOverCredit:
		return "Spend is " + out.OverageText + " past the monthly credit. OpenAI bills purchased credits after the monthly credit; if there are none, enter an API Platform reading.", true
	case client.CreditIssueReadingAboveCredit:
		return "Your API Platform reading is more than the monthly credit; check the monthly credit.", true
	case client.CreditIssueReadingOnRefillDay:
		return "This API Platform reading was taken on the refill day. If API Platform did not show the new credit yet, enter a new reading once it does.", true
	case client.CreditIssueReadingUnused:
		return openAIReadingUnusedSentenceOf(a.unused, out.Settings.Reading), true
	case client.CreditIssueNoTraffic:
		return "No traffic from this organization has reached CPA through an openai-compatibility entry since counting began.", true
	}
	return "", false
}

// openAIReadingUnusedSentenceOf says why the stored API Platform reading is
// not used, as readingUnusedSentenceOf does for a Console reading.
func openAIReadingUnusedSentenceOf(reason string, reading *client.StoredReading) string {
	switch reason {
	case client.CreditReadingBeforeRefill:
		amount, at := "", time.Time{}
		if reading != nil {
			amount = moneyOf(exactDollars(reading.RemainingUSD))
			at = time.Unix(reading.AtEpoch, 0)
		}
		return "Your API Platform reading of " + amount + " on " + formatUTCDate(at) + " was before the last refill, so it is not used."
	case client.CreditReadingOtherOrganization:
		return "Your API Platform reading was for a different organization, so it is not used."
	case client.CreditReadingTooOld:
		return "Your API Platform reading is over 31 days old and there is no refill date, so it is not used."
	case client.CreditReadingFuture:
		return "Your API Platform reading is dated in the future, so it is not used."
	}
	return ""
}
