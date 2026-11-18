package credits

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// The OpenAI price table's provenance, emitted in the OpenAI credit document.
const (
	openAIPricingAsOf   = "2026-11-18"
	openAIPricingSource = "https://developers.openai.com/api/docs/pricing"
	// openAIPricingCacheWrites is the cache-write rate every OpenAI amount
	// uses. CPA's record does not split writes by retention, its
	// openai-compatibility executor sends Chat Completions but for
	// /responses/compact, and only the Responses API offers 12-hour
	// retention, so a 12-hour figure would almost never be real. With one
	// rate there is no cache-write difference to report.
	openAIPricingCacheWrites = "30m"
)

// openAIPriceRow is one row of OpenAI's pricing page on openAIPricingAsOf, in
// US dollars per million tokens, as the page prints it. "" is the page's "–".
// A row whose input and output are both "" is a long-context line the page
// prints with no figures: it prices nothing, and marks the model as one the
// page prices by context length. Otherwise cached input and cache writes
// marked "–" are billed at the input rate: GPT-5.5 and earlier have no
// cache-write charge, and a model without caching reports no cached tokens.
// gpt-6-astra-law prints no write rate at all, so its writes are understated
// if OpenAI charges them.
type openAIPriceRow struct {
	// model is the canonical id openAICanonicalModel reduces a request's model
	// to.
	model string
	// tier is the meter's service tier the row prices: "" (Standard),
	// client.MeterTierFlex, client.MeterTierFast or client.MeterTierUltrafast.
	tier string
	// prompt is "" for the page's short context, at most 272K input tokens,
	// or over272K for its long context, more than that.
	prompt                                    string
	input, cached, write30m, write12h, output string
}

// over272K is the prompt class of the table's long-context rows.
const over272K = client.MeterPromptOver272K

// openAIPriceTable is the pricing page on 2026-11-18, every tier a request
// through CPA can be served at. Batch is left out, since the Batch API does
// not pass through CPA, and so are image, audio, realtime, transcription and
// embedding models, whose usage the meter has no columns for, and fine-tuned
// models, which fine-tuning closed to new users and which bill at rates of
// their own: their usage is listed unpriced. Ultrafast GPT-6.2 is the
// promotion price, 3x Standard through 2027-02-28.
var openAIPriceTable = []openAIPriceRow{
	// Standard, priced by context length.
	{model: "gpt-6.2-astra", input: "8.00", cached: "0.40", write30m: "10.00", write12h: "20.00", output: "40.00"},
	{model: "gpt-6.2-astra", prompt: over272K, input: "16.00", cached: "0.80", write30m: "20.00", write12h: "40.00", output: "60.00"},
	{model: "gpt-6.2-sol", input: "2.00", cached: "0.10", write30m: "2.50", write12h: "5.00", output: "10.00"},
	{model: "gpt-6.2-sol", prompt: over272K, input: "4.00", cached: "0.20", write30m: "5.00", write12h: "10.00", output: "15.00"},
	{model: "gpt-6-astra", input: "10.00", cached: "1.00", write30m: "12.50", write12h: "25.00", output: "50.00"},
	{model: "gpt-6-astra", prompt: over272K, input: "20.00", cached: "2.00", write30m: "25.00", write12h: "50.00", output: "75.00"},
	{model: "gpt-6.1-sol", input: "2.00", cached: "0.10", write30m: "2.50", write12h: "5.00", output: "10.00"},
	{model: "gpt-6.1-sol", prompt: over272K, input: "4.00", cached: "0.20", write30m: "5.00", write12h: "10.00", output: "15.00"},
	{model: "gpt-6.1-terra", input: "1.00", cached: "0.05", write30m: "1.25", write12h: "2.50", output: "5.00"},
	{model: "gpt-6.1-terra", prompt: over272K, input: "2.00", cached: "0.10", write30m: "2.50", write12h: "5.00", output: "7.50"},
	{model: "gpt-6-luna", input: "0.10", cached: "0.01", write30m: "0.125", write12h: "0.25", output: "0.50"},
	{model: "gpt-6-luna", prompt: over272K, input: "0.20", cached: "0.02", write30m: "0.25", write12h: "0.50", output: "0.75"},
	{model: "gpt-6-sol", input: "2.00", cached: "0.20", write30m: "2.50", write12h: "5.00", output: "10.00"},
	{model: "gpt-6-sol", prompt: over272K, input: "4.00", cached: "0.40", write30m: "5.00", write12h: "10.00", output: "15.00"},
	{model: "gpt-5.6-sol", input: "4.00", cached: "0.40", write30m: "5.00", write12h: "10.00", output: "20.00"},
	{model: "gpt-5.6-sol", prompt: over272K, input: "8.00", cached: "0.80", write30m: "10.00", write12h: "20.00", output: "30.00"},
	{model: "gpt-5.6-terra", input: "2.00", cached: "0.20", write30m: "2.50", write12h: "5.00", output: "12.00"},
	{model: "gpt-5.6-terra", prompt: over272K, input: "4.00", cached: "0.40", write30m: "5.00", write12h: "10.00", output: "18.00"},
	{model: "gpt-5.6-luna", input: "0.20", cached: "0.02", write30m: "0.25", write12h: "0.50", output: "1.20"},
	{model: "gpt-5.6-luna", prompt: over272K, input: "0.40", cached: "0.04", write30m: "0.50", write12h: "1.00", output: "1.80"},
	{model: "gpt-5.5", input: "5.00", cached: "0.50", output: "30.00"},
	{model: "gpt-5.5", prompt: over272K, input: "10.00", cached: "1.00", output: "45.00"},
	{model: "gpt-5.5-pro", input: "30.00", output: "180.00"},
	{model: "gpt-5.5-pro", prompt: over272K, input: "60.00", output: "270.00"},
	{model: "gpt-5.4", input: "2.50", cached: "0.25", output: "15.00"},
	{model: "gpt-5.4", prompt: over272K, input: "5.00", cached: "0.50", output: "22.50"},
	{model: "gpt-5.4-pro", input: "30.00", output: "180.00"},
	{model: "gpt-5.4-pro", prompt: over272K, input: "60.00", output: "270.00"},

	// Standard, one rate whatever the length.
	{model: "gpt-5.4-mini", input: "0.75", cached: "0.075", output: "4.50"},
	{model: "gpt-5.4-nano", input: "0.20", cached: "0.02", output: "1.25"},
	{model: "gpt-5.2", input: "1.75", cached: "0.175", output: "14.00"},
	{model: "gpt-5.2-pro", input: "21.00", output: "168.00"},
	{model: "gpt-5.1", input: "1.25", cached: "0.125", output: "10.00"},
	{model: "gpt-5", input: "1.25", cached: "0.125", output: "10.00"},
	{model: "gpt-5-mini", input: "0.25", cached: "0.025", output: "2.00"},
	{model: "gpt-5-nano", input: "0.05", cached: "0.005", output: "0.40"},
	{model: "gpt-5-pro", input: "15.00", output: "120.00"},
	{model: "gpt-4.1", input: "2.00", cached: "0.50", output: "8.00"},
	{model: "gpt-4.1-mini", input: "0.40", cached: "0.10", output: "1.60"},
	{model: "gpt-4o", input: "2.50", cached: "1.25", output: "10.00"},
	{model: "gpt-4o-mini", input: "0.15", cached: "0.075", output: "0.60"},
	{model: "o3-pro", input: "20.00", output: "80.00"},
	{model: "o3", input: "2.00", cached: "0.50", output: "8.00"},

	// Standard, specialized models.
	{model: "gpt-6-cyber", input: "20.00", cached: "2.00", write30m: "25.00", write12h: "50.00", output: "100.00"},
	// The page prints no long-context write rates; openAIDerivedWrites gives
	// them.
	{model: "gpt-6-cyber", prompt: over272K, input: "40.00", cached: "4.00", output: "150.00"},
	{model: "gpt-5.6-cyber", input: "12.50", cached: "1.25", write30m: "15.625", write12h: "31.25", output: "75.00"},
	{model: "gpt-5.6-cyber", prompt: over272K},
	{model: "gpt-5.5-cyber-preview", input: "12.50", cached: "1.25", output: "75.00"},
	{model: "gpt-5.5-cyber-preview", prompt: over272K},
	{model: "gpt-6-rosalind", input: "10.00", cached: "1.00", write30m: "12.50", write12h: "25.00", output: "50.00"},
	{model: "gpt-6-rosalind", prompt: over272K, input: "20.00", cached: "2.00", write30m: "25.00", write12h: "50.00", output: "75.00"},
	{model: "gpt-rosalind-research", input: "5.00", cached: "0.50", output: "25.00"},
	{model: "gpt-rosalind-research", prompt: over272K},
	{model: "gpt-rosalind-discovery", input: "5.00", cached: "0.50", output: "25.00"},
	{model: "gpt-rosalind-discovery", prompt: over272K},
	{model: "gpt-6.2-astra-law", input: "8.00", cached: "0.40", write30m: "10.00", write12h: "20.00", output: "40.00"},
	{model: "gpt-6.2-astra-law", prompt: over272K, input: "16.00", cached: "0.80", write30m: "20.00", write12h: "40.00", output: "60.00"},
	{model: "gpt-6-astra-law", input: "10.00", cached: "1.00", output: "50.00"},
	{model: "gpt-6-astra-law", prompt: over272K, input: "20.00", cached: "2.00", output: "75.00"},
	{model: "chat-latest", input: "5.00", cached: "0.50", output: "30.00"},
	{model: "gpt-5.3-codex", input: "1.75", cached: "0.175", output: "14.00"},
	// Not on the pricing page: the comparison table on gpt-5.3-codex's model
	// page (https://developers.openai.com/api/docs/models/gpt-5.3-codex)
	// prices GPT-5.2-Codex the same. That page says Chat Completions is not
	// supported, so this traffic is unlikely through CPA.
	{model: "gpt-5.2-codex", input: "1.75", cached: "0.175", output: "14.00"},
	{model: "gpt-5-search-api", input: "1.25", cached: "0.125", output: "10.00"},

	// Flex, priced by context length.
	{model: "gpt-6-astra", tier: client.MeterTierFlex, input: "5.00", cached: "0.50", write30m: "6.25", write12h: "12.50", output: "25.00"},
	{model: "gpt-6-astra", tier: client.MeterTierFlex, prompt: over272K, input: "10.00", cached: "1.00", write30m: "12.50", write12h: "25.00", output: "37.50"},
	{model: "gpt-6.1-sol", tier: client.MeterTierFlex, input: "1.00", cached: "0.05", write30m: "1.25", write12h: "2.50", output: "5.00"},
	{model: "gpt-6.1-sol", tier: client.MeterTierFlex, prompt: over272K, input: "2.00", cached: "0.10", write30m: "2.50", write12h: "5.00", output: "7.50"},
	{model: "gpt-6.1-terra", tier: client.MeterTierFlex, input: "0.50", cached: "0.025", write30m: "0.625", write12h: "1.25", output: "2.50"},
	{model: "gpt-6.1-terra", tier: client.MeterTierFlex, prompt: over272K, input: "1.00", cached: "0.05", write30m: "1.25", write12h: "2.50", output: "3.75"},
	{model: "gpt-6-luna", tier: client.MeterTierFlex, input: "0.05", cached: "0.005", write30m: "0.0625", write12h: "0.125", output: "0.25"},
	{model: "gpt-6-luna", tier: client.MeterTierFlex, prompt: over272K, input: "0.10", cached: "0.01", write30m: "0.125", write12h: "0.25", output: "0.375"},
	{model: "gpt-6-sol", tier: client.MeterTierFlex, input: "1.00", cached: "0.10", write30m: "1.25", write12h: "2.50", output: "5.00"},
	{model: "gpt-6-sol", tier: client.MeterTierFlex, prompt: over272K, input: "2.00", cached: "0.20", write30m: "2.50", write12h: "5.00", output: "7.50"},
	{model: "gpt-5.6-sol", tier: client.MeterTierFlex, input: "2.00", cached: "0.20", write30m: "2.50", write12h: "5.00", output: "10.00"},
	{model: "gpt-5.6-sol", tier: client.MeterTierFlex, prompt: over272K, input: "4.00", cached: "0.40", write30m: "5.00", write12h: "10.00", output: "15.00"},
	{model: "gpt-5.6-terra", tier: client.MeterTierFlex, input: "1.00", cached: "0.10", write30m: "1.25", write12h: "2.50", output: "6.00"},
	{model: "gpt-5.6-terra", tier: client.MeterTierFlex, prompt: over272K, input: "2.00", cached: "0.20", write30m: "2.50", write12h: "5.00", output: "9.00"},
	{model: "gpt-5.6-luna", tier: client.MeterTierFlex, input: "0.10", cached: "0.01", write30m: "0.125", write12h: "0.25", output: "0.60"},
	{model: "gpt-5.6-luna", tier: client.MeterTierFlex, prompt: over272K, input: "0.20", cached: "0.02", write30m: "0.25", write12h: "0.50", output: "0.90"},
	{model: "gpt-5.5", tier: client.MeterTierFlex, input: "2.50", cached: "0.25", output: "15.00"},
	{model: "gpt-5.5", tier: client.MeterTierFlex, prompt: over272K, input: "5.00", cached: "0.50", output: "22.50"},
	{model: "gpt-5.5-pro", tier: client.MeterTierFlex, input: "15.00", output: "90.00"},
	{model: "gpt-5.5-pro", tier: client.MeterTierFlex, prompt: over272K},
	// The page prints 0.13 for this cached rate, its rounding of 0.125; it
	// is kept as printed.
	{model: "gpt-5.4", tier: client.MeterTierFlex, input: "1.25", cached: "0.13", output: "7.50"},
	{model: "gpt-5.4", tier: client.MeterTierFlex, prompt: over272K, input: "2.50", cached: "0.25", output: "11.25"},
	{model: "gpt-5.4-pro", tier: client.MeterTierFlex, input: "15.00", output: "90.00"},
	{model: "gpt-5.4-pro", tier: client.MeterTierFlex, prompt: over272K, input: "30.00", output: "135.00"},

	// Flex, one rate whatever the length.
	{model: "gpt-5.4-mini", tier: client.MeterTierFlex, input: "0.375", cached: "0.0375", output: "2.25"},
	{model: "gpt-5.4-nano", tier: client.MeterTierFlex, input: "0.10", cached: "0.01", output: "0.625"},
	{model: "gpt-5.2", tier: client.MeterTierFlex, input: "0.875", cached: "0.0875", output: "7.00"},
	{model: "gpt-5.1", tier: client.MeterTierFlex, input: "0.625", cached: "0.0625", output: "5.00"},
	{model: "gpt-5", tier: client.MeterTierFlex, input: "0.625", cached: "0.0625", output: "5.00"},
	{model: "gpt-5-mini", tier: client.MeterTierFlex, input: "0.125", cached: "0.0125", output: "1.00"},
	{model: "gpt-5-nano", tier: client.MeterTierFlex, input: "0.025", cached: "0.0025", output: "0.20"},
	{model: "o3", tier: client.MeterTierFlex, input: "1.00", cached: "0.25", output: "4.00"},

	// Fast, priced by context length.
	{model: "gpt-6.2-astra", tier: client.MeterTierFast, input: "16.00", cached: "0.80", write30m: "20.00", write12h: "40.00", output: "80.00"},
	{model: "gpt-6.2-astra", tier: client.MeterTierFast, prompt: over272K, input: "32.00", cached: "1.60", write30m: "40.00", write12h: "80.00", output: "120.00"},
	{model: "gpt-6.2-sol", tier: client.MeterTierFast, input: "4.00", cached: "0.20", write30m: "5.00", write12h: "10.00", output: "20.00"},
	{model: "gpt-6.2-sol", tier: client.MeterTierFast, prompt: over272K, input: "8.00", cached: "0.40", write30m: "10.00", write12h: "20.00", output: "30.00"},
	{model: "gpt-6-astra", tier: client.MeterTierFast, input: "20.00", cached: "2.00", write30m: "25.00", write12h: "50.00", output: "100.00"},
	{model: "gpt-6-astra", tier: client.MeterTierFast, prompt: over272K, input: "40.00", cached: "4.00", write30m: "50.00", write12h: "100.00", output: "150.00"},
	{model: "gpt-6.1-sol", tier: client.MeterTierFast, input: "4.00", cached: "0.20", write30m: "5.00", write12h: "10.00", output: "20.00"},
	{model: "gpt-6.1-sol", tier: client.MeterTierFast, prompt: over272K, input: "8.00", cached: "0.40", write30m: "10.00", write12h: "20.00", output: "30.00"},
	{model: "gpt-6.1-terra", tier: client.MeterTierFast, input: "2.00", cached: "0.10", write30m: "2.50", write12h: "5.00", output: "10.00"},
	{model: "gpt-6.1-terra", tier: client.MeterTierFast, prompt: over272K, input: "4.00", cached: "0.20", write30m: "5.00", write12h: "10.00", output: "15.00"},
	{model: "gpt-6-luna", tier: client.MeterTierFast, input: "0.20", cached: "0.02", write30m: "0.25", write12h: "0.50", output: "1.00"},
	{model: "gpt-6-luna", tier: client.MeterTierFast, prompt: over272K, input: "0.40", cached: "0.04", write30m: "0.50", write12h: "1.00", output: "1.50"},
	{model: "gpt-6-sol", tier: client.MeterTierFast, input: "4.00", cached: "0.40", write30m: "5.00", write12h: "10.00", output: "20.00"},
	{model: "gpt-6-sol", tier: client.MeterTierFast, prompt: over272K, input: "8.00", cached: "0.80", write30m: "10.00", write12h: "20.00", output: "30.00"},
	{model: "gpt-5.6-sol", tier: client.MeterTierFast, input: "8.00", cached: "0.80", write30m: "10.00", write12h: "20.00", output: "40.00"},
	{model: "gpt-5.6-sol", tier: client.MeterTierFast, prompt: over272K, input: "16.00", cached: "1.60", write30m: "20.00", write12h: "40.00", output: "60.00"},
	{model: "gpt-5.6-terra", tier: client.MeterTierFast, input: "4.00", cached: "0.40", write30m: "5.00", write12h: "10.00", output: "24.00"},
	{model: "gpt-5.6-terra", tier: client.MeterTierFast, prompt: over272K, input: "8.00", cached: "0.80", write30m: "10.00", write12h: "20.00", output: "36.00"},
	{model: "gpt-5.6-luna", tier: client.MeterTierFast, input: "0.40", cached: "0.04", write30m: "0.50", write12h: "1.00", output: "2.40"},
	{model: "gpt-5.6-luna", tier: client.MeterTierFast, prompt: over272K, input: "0.80", cached: "0.08", write30m: "1.00", write12h: "2.00", output: "3.60"},
	{model: "gpt-5.5", tier: client.MeterTierFast, input: "12.50", cached: "1.25", output: "75.00"},
	{model: "gpt-5.5", tier: client.MeterTierFast, prompt: over272K},
	{model: "gpt-5.4", tier: client.MeterTierFast, input: "5.00", cached: "0.50", output: "30.00"},
	{model: "gpt-5.4", tier: client.MeterTierFast, prompt: over272K},

	// Fast, one rate whatever the length.
	{model: "gpt-5.4-mini", tier: client.MeterTierFast, input: "1.50", cached: "0.15", output: "9.00"},
	{model: "gpt-5.2", tier: client.MeterTierFast, input: "3.50", cached: "0.35", output: "28.00"},
	{model: "gpt-5.1", tier: client.MeterTierFast, input: "2.50", cached: "0.25", output: "20.00"},
	{model: "gpt-5", tier: client.MeterTierFast, input: "2.50", cached: "0.25", output: "20.00"},
	{model: "gpt-5-mini", tier: client.MeterTierFast, input: "0.45", cached: "0.045", output: "3.60"},
	{model: "gpt-4.1", tier: client.MeterTierFast, input: "3.50", cached: "0.875", output: "14.00"},
	{model: "gpt-4.1-mini", tier: client.MeterTierFast, input: "0.70", cached: "0.175", output: "2.80"},
	{model: "gpt-4o", tier: client.MeterTierFast, input: "4.25", cached: "2.125", output: "17.00"},
	{model: "gpt-4o-mini", tier: client.MeterTierFast, input: "0.25", cached: "0.125", output: "1.00"},
	{model: "o3", tier: client.MeterTierFast, input: "3.50", cached: "0.875", output: "14.00"},
	{model: "gpt-5.3-codex", tier: client.MeterTierFast, input: "3.50", cached: "0.35", output: "28.00"},

	// Ultrafast, priced by context length.
	{model: "gpt-6-astra", tier: client.MeterTierUltrafast, input: "60.00", cached: "6.00", write30m: "75.00", write12h: "150.00", output: "300.00"},
	{model: "gpt-6-astra", tier: client.MeterTierUltrafast, prompt: over272K, input: "120.00", cached: "12.00", write30m: "150.00", write12h: "300.00", output: "450.00"},
	{model: "gpt-6.1-sol", tier: client.MeterTierUltrafast, input: "12.00", cached: "0.60", write30m: "15.00", write12h: "30.00", output: "60.00"},
	{model: "gpt-6.1-sol", tier: client.MeterTierUltrafast, prompt: over272K, input: "24.00", cached: "1.20", write30m: "30.00", write12h: "60.00", output: "90.00"},
	{model: "gpt-6.2-astra", tier: client.MeterTierUltrafast, input: "24.00", cached: "1.20", write30m: "30.00", write12h: "60.00", output: "120.00"},
	{model: "gpt-6.2-astra", tier: client.MeterTierUltrafast, prompt: over272K, input: "48.00", cached: "2.40", write30m: "60.00", write12h: "120.00", output: "180.00"},
	{model: "gpt-6.2-sol", tier: client.MeterTierUltrafast, input: "6.00", cached: "0.30", write30m: "7.50", write12h: "15.00", output: "30.00"},
	{model: "gpt-6.2-sol", tier: client.MeterTierUltrafast, prompt: over272K, input: "12.00", cached: "0.60", write30m: "15.00", write12h: "30.00", output: "45.00"},
}

// openAIDerivedWrites are write rates the page leaves "–" where the model's
// own short-context rates and the page's long-context rule (2x input and
// cache) give them: gpt-6-cyber's long-context writes. Each fills only the
// row's "–" cells, 30-minute then 12-hour.
var openAIDerivedWrites = map[openAIPriceKey][2]string{
	{"gpt-6-cyber", "", over272K}: {"50.00", "100.00"},
}

type openAIPriceKey struct{ model, tier, prompt string }

// openAIPrices is openAIPriceTable parsed once, and openAILongContextModels
// every model it has a long-context row for, priced or not. A string that
// does not parse leaves its row out and is reported in openAIPricesErr, which
// a test requires to be nil, as pricesErr is.
var openAIPrices, openAILongContextModels, openAIPricesErr = parseOpenAIPrices(openAIPriceTable, openAIDerivedWrites)

func parseOpenAIPrices(table []openAIPriceRow, derived map[openAIPriceKey][2]string) (map[openAIPriceKey]rates, map[string]bool, error) {
	parsed := make(map[openAIPriceKey]rates, len(table))
	longContext := map[string]bool{}
	var failed []string
	for _, row := range table {
		key := openAIPriceKey{row.model, row.tier, row.prompt}
		if row.prompt == over272K {
			longContext[row.model] = true
		}
		if row.input == "" && row.output == "" {
			continue
		}
		write30m, write12h := row.write30m, row.write12h
		if d, ok := derived[key]; ok {
			if write30m == "" {
				write30m = d[0]
			}
			if write12h == "" {
				write12h = d[1]
			}
		}
		input, okInput := openAIRateOf(row.input, "")
		out, okOut := openAIRateOf(row.output, "")
		read, okRead := openAIRateOf(row.cached, row.input)
		write, okWrite := openAIRateOf(write30m, row.input)
		// The 12-hour rate is never priced, but a cell that does not parse
		// is still a row read wrong.
		_, ok12h := openAIRateOf(write12h, row.input)
		if !okInput || !okOut || !okRead || !okWrite || !ok12h {
			failed = append(failed, fmt.Sprint(key))
			continue
		}
		parsed[key] = rates{input: input, write5m: write, write1h: write, read: read, out: out}
	}
	if len(failed) > 0 {
		return parsed, longContext, errors.New("OpenAI price table rows do not parse: " + strings.Join(failed, ", "))
	}
	return parsed, longContext, nil
}

// openAIRateOf is one cell as dollars per token, the cell standing in for a
// "–" one when given ("" stands in for nothing, so the cell must parse).
func openAIRateOf(text, standIn string) (*big.Rat, bool) {
	if text == "" {
		text = standIn
	}
	perMillion, valid := amountOf(text)
	if !valid || !priceShape.MatchString(text) {
		return nil, false
	}
	return perMillion.Quo(perMillion, tokensPerMillion), true
}

// openAIDateSuffix is a dated snapshot, "-2026-08-01".
var openAIDateSuffix = regexp.MustCompile(`-[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

// openAICanonicalModel is the id lower-cased, trimmed, without a dated
// snapshot suffix: gpt-6-sol-2026-08-01 is gpt-6-sol.
func openAICanonicalModel(model string) string {
	return openAIDateSuffix.ReplaceAllString(strings.ToLower(strings.TrimSpace(model)), "")
}

// openAIRatesOf finds the row for one usage entry: the entry's tier at its
// prompt class. A model the page prices at one rate whatever the length
// falls back to that tier's short-context row; a model the page prices by
// length (openAILongContextModels) does not, so a long-context request in a
// tier the page gives no long figures for is unpriced. A tier with no row
// for the model at all is unpriced, and no tier falls back to Standard. ok is
// false when unpriced.
func openAIRatesOf(model, tier, prompt string) (rates, bool) {
	canonical := openAICanonicalModel(model)
	if r, ok := openAIPrices[openAIPriceKey{canonical, tier, prompt}]; ok {
		return r, true
	}
	if prompt == over272K && !openAILongContextModels[canonical] {
		r, ok := openAIPrices[openAIPriceKey{canonical, tier, ""}]
		return r, ok
	}
	return rates{}, false
}

// openAIPriceUsage prices a window as priceUsage does, with openAIRatesOf,
// and at1h equal to at5m: every write is priced at the 30-minute rate, so
// there is no 1-hour difference to report. An unpriced entry is listed under
// openAIUnpricedLabel, and (other) is left out without being listed.
func openAIPriceUsage(usage []client.MeterUsage) costs {
	c := costs{at5m: new(big.Rat), unpriced: map[string]uint64{}}
	for _, u := range usage {
		if u.Model == client.MeterOtherModel {
			continue
		}
		r, ok := openAIRatesOf(u.Model, u.Tier, u.Prompt)
		if !ok {
			label := openAIUnpricedLabel(u)
			c.unpriced[label] = addTokens(c.unpriced[label], tokensOf(u))
			continue
		}
		c.at5m.Add(c.at5m, tokenCost(u.Input, r.input))
		c.at5m.Add(c.at5m, tokenCost(u.Output, r.out))
		c.at5m.Add(c.at5m, tokenCost(u.CacheRead, r.read))
		c.at5m.Add(c.at5m, tokenCost(u.CacheWrite, r.write5m))
	}
	c.at1h = new(big.Rat).Set(c.at5m)
	return c
}

// openAIUnpricedLabel names an unpriced entry: its meter model; then " " and
// the tier when it is not Standard ("flex", "fast", "ultrafast", or "unknown
// tier"); then " over 272K" when the prompt is over_272k and the tier has a
// short-context row for the model, so the long row is what is missing.
// "gpt-5.6-sol ultrafast", "gpt-5.5 fast over 272K", "gpt-6-sol unknown tier".
// It has no parentheses: unpricedSentenceOf adds the token count in them.
func openAIUnpricedLabel(u client.MeterUsage) string {
	label := u.Model
	switch u.Tier {
	case "":
	case client.MeterTierUnknown:
		label += " unknown tier"
	default:
		label += " " + u.Tier
	}
	if u.Prompt == over272K {
		if _, short := openAIPrices[openAIPriceKey{openAICanonicalModel(u.Model), u.Tier, ""}]; short {
			label += " over 272K"
		}
	}
	return label
}
