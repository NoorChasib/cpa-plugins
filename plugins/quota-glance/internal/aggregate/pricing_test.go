package aggregate

import (
	"math/big"
	"testing"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// Every row of the table parses, exactly, and none is lost: a price that
// failed to parse would leave its model unpriced without a word.
func TestEveryPriceParses(t *testing.T) {
	if pricesErr != nil {
		t.Fatal(pricesErr)
	}
	if len(prices) != len(priceTable) {
		t.Fatalf("%d rows parsed from %d", len(prices), len(priceTable))
	}
	if _, err := parsePrices([]priceRow{{model: "x", input: "1e3", write5m: "1", write1h: "1", read: "1", out: "1"}}); err == nil {
		t.Error("an exponent parsed as a price")
	}
	if _, err := parsePrices([]priceRow{{model: "x", input: "1/3", write5m: "1", write1h: "1", read: "1", out: "1"}}); err == nil {
		t.Error("a fraction parsed as a price")
	}
}

// The table as the pricing page read on 2026-10-09, row by row.
func TestPriceTableMatchesThePricingPage(t *testing.T) {
	type row = [5]string // input, 5m write, 1h write, cache read, output
	want := map[priceKey]row{
		{"claude-fable-5-1", ""}:                     {"10", "12.50", "20", "0.25", "50"},
		{"claude-mythos-5-1", ""}:                    {"10", "12.50", "20", "0.25", "50"},
		{"claude-fable-5", ""}:                       {"10", "12.50", "20", "1", "50"},
		{"claude-mythos-5", ""}:                      {"10", "12.50", "20", "1", "50"},
		{"claude-opus-5-5", ""}:                      {"4", "5", "8", "0.20", "20"},
		{"claude-opus-5", ""}:                        {"5", "6.25", "10", "0.50", "25"},
		{"claude-opus-4-8", ""}:                      {"5", "6.25", "10", "0.50", "25"},
		{"claude-opus-4-7", ""}:                      {"5", "6.25", "10", "0.50", "25"},
		{"claude-opus-4-6", ""}:                      {"5", "6.25", "10", "0.50", "25"},
		{"claude-opus-4-5", ""}:                      {"5", "6.25", "10", "0.50", "25"},
		{"claude-opus-4-1", ""}:                      {"15", "18.75", "30", "1.50", "75"},
		{"claude-opus-4", ""}:                        {"15", "18.75", "30", "1.50", "75"},
		{"claude-sonnet-5-5", ""}:                    {"2", "2.50", "4", "0.10", "10"},
		{"claude-sonnet-5", ""}:                      {"2", "2.50", "4", "0.20", "10"},
		{"claude-sonnet-4-6", ""}:                    {"3", "3.75", "6", "0.30", "15"},
		{"claude-sonnet-4-5", ""}:                    {"3", "3.75", "6", "0.30", "15"},
		{"claude-sonnet-4", ""}:                      {"3", "3.75", "6", "0.30", "15"},
		{"claude-haiku-5-5", ""}:                     {"0.10", "0.125", "0.20", "0.01", "0.50"},
		{"claude-haiku-5-5", qc.MeterPromptOver100K}: {"0.50", "0.625", "1", "0.05", "2.50"},
		{"claude-haiku-4-5", ""}:                     {"1", "1.25", "2", "0.10", "5"},
		{"claude-3-5-haiku", ""}:                     {"0.80", "1", "1.60", "0.08", "4"},
	}
	if len(want) != len(prices) {
		t.Fatalf("the table has %d rows; the page lists %d", len(prices), len(want))
	}
	for key, perMillion := range want {
		r, ok := prices[key]
		if !ok {
			t.Errorf("%v is missing", key)
			continue
		}
		for i, got := range []*big.Rat{r.input, r.write5m, r.write1h, r.read, r.out} {
			expected, _ := new(big.Rat).SetString(perMillion[i])
			if new(big.Rat).Mul(got, tokensPerMillion).Cmp(expected) != 0 {
				t.Errorf("%v column %d = %s per million, want %s", key, i, new(big.Rat).Mul(got, tokensPerMillion).FloatString(4), perMillion[i])
			}
		}
	}
}

func TestCanonicalModel(t *testing.T) {
	for in, want := range map[string]string{
		"claude-opus-4-20250514":                  "claude-opus-4",
		"claude-3-5-haiku-20241022":               "claude-3-5-haiku",
		"claude-haiku-4-5@20251001":               "claude-haiku-4-5",
		"anthropic.claude-opus-4-20250514-v1:0":   "claude-opus-4",
		"anthropic.claude-sonnet-4-5-20250929-v1": "claude-sonnet-4-5",
		"  Claude-Sonnet-5-5  ":                   "claude-sonnet-5-5",
		"claude-opus-4-5-20251101":                "claude-opus-4-5",
		"claude-mythos-preview":                   "claude-mythos-preview",
	} {
		if got := canonicalModel(in); got != want {
			t.Errorf("canonicalModel(%q) = %q, want %q", in, got, want)
		}
	}
}

// Money is exact: one Haiku 5.5 cache-read token is a hundred-millionth of a
// dollar, and a million of them a cent.
func TestOneTokenIsExact(t *testing.T) {
	one := priceUsage([]qc.MeterUsage{{Model: "claude-haiku-5-5", CacheRead: 1}})
	if want := big.NewRat(1, 100_000_000); one.at5m.Cmp(want) != 0 {
		t.Errorf("one cache-read token = %s, want %s", one.at5m.RatString(), want.RatString())
	}
	million := priceUsage([]qc.MeterUsage{{Model: "claude-haiku-5-5", CacheRead: 1_000_000}})
	if million.at5m.Cmp(big.NewRat(1, 100)) != 0 || moneyOf(million.at5m) != "$0.01" {
		t.Errorf("a million cache-read tokens = %s", million.at5m.RatString())
	}
}

// Haiku 5.5 bills a prompt over 100,000 tokens at its higher tier; the class
// on any other model is priced at that model's base rate.
func TestPromptClasses(t *testing.T) {
	short := priceUsage([]qc.MeterUsage{{Model: "claude-haiku-5-5", Input: 1_000_000, Output: 1_000_000}})
	long := priceUsage([]qc.MeterUsage{{Model: "claude-haiku-5-5", Prompt: qc.MeterPromptOver100K, Input: 1_000_000, Output: 1_000_000}})
	if moneyOf(short.at5m) != "$0.60" || moneyOf(long.at5m) != "$3.00" {
		t.Errorf("haiku 5.5: short %s long %s", moneyOf(short.at5m), moneyOf(long.at5m))
	}
	sonnet := priceUsage([]qc.MeterUsage{{Model: "claude-sonnet-5-5", Prompt: qc.MeterPromptOver100K, Input: 1_000_000}})
	if moneyOf(sonnet.at5m) != "$2.00" || len(sonnet.unpriced) != 0 {
		t.Errorf("sonnet 5.5 over_100k: %s, unpriced %v", moneyOf(sonnet.at5m), sonnet.unpriced)
	}
}

// A model with no row is left out and listed by the name the meter gave it;
// (other) is left out and never listed.
func TestUnpricedAndOther(t *testing.T) {
	c := priceUsage([]qc.MeterUsage{
		{Model: "claude-mythos-preview", Input: 1_000_000},
		{Model: "claude-3-7-sonnet-20250219", Output: 10},
		{Model: "claude-3-haiku", CacheRead: 3},
		{Model: qc.MeterOtherModel, Input: 50_000_000},
		{Model: "claude-sonnet-5-5", CacheWrite: 1_000_000},
	})
	if moneyOf(c.at5m) != "$2.50" || moneyOf(c.at1h) != "$4.00" {
		t.Errorf("priced: %s at 5m, %s at 1h", moneyOf(c.at5m), moneyOf(c.at1h))
	}
	list := c.unpricedList()
	if len(list) != 3 || list[0] != (APICreditUnpriced{Model: "claude-mythos-preview", Tokens: 1_000_000}) ||
		list[1].Model != "claude-3-7-sonnet-20250219" || list[2].Model != "claude-3-haiku" {
		t.Errorf("unpriced = %+v", list)
	}
	if _, listed := c.unpriced[qc.MeterOtherModel]; listed {
		t.Error("(other) was listed as an unpriced model")
	}
}

// The table's provenance is in the document.
func TestPricingIsEmitted(t *testing.T) {
	pricing := buildFixture(t).APICredits.Pricing
	if pricing.AsOf != "2026-10-09" || pricing.Source != "https://platform.claude.com/docs/en/about-claude/pricing" || pricing.CacheWrites != "5m" {
		t.Errorf("pricing = %+v", pricing)
	}
}
