package credits

import (
	"maps"
	"math/big"
	"slices"
	"strings"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// Every priced cell of the OpenAI table parses, the 12-hour column included,
// and a long-context line the page prints without figures has none at all: a
// price that failed to parse would leave its model unpriced without a word.
func TestEveryOpenAIPriceParses(t *testing.T) {
	if openAIPricesErr != nil {
		t.Fatal(openAIPricesErr)
	}
	priced, keys := 0, map[openAIPriceKey]bool{}
	for _, row := range openAIPriceTable {
		key := openAIPriceKey{row.model, row.tier, row.prompt}
		if keys[key] {
			t.Errorf("%v is in the table twice", key)
		}
		keys[key] = true
		cells := []string{row.input, row.cached, row.write30m, row.write12h, row.output}
		if row.input == "" && row.output == "" {
			if row.prompt != over272K || strings.Join(cells, "") != "" {
				t.Errorf("%v: a row with no input or output must be a long-context row with no figures: %q", key, cells)
			}
			continue
		}
		priced++
		if row.input == "" || row.output == "" {
			t.Errorf("%v: a priced row needs its input and output: %q", key, cells)
		}
		for _, cell := range cells {
			if _, ok := amountOf(cell); cell != "" && (!ok || !priceShape.MatchString(cell)) {
				t.Errorf("%v: %q does not parse", key, cell)
			}
		}
	}
	if len(openAIPrices) != priced {
		t.Fatalf("%d rows parsed from %d priced", len(openAIPrices), priced)
	}
	for _, row := range []openAIPriceRow{
		{model: "x", input: "1e3", output: "1"},
		{model: "x", input: "1/3", output: "1"},
		{model: "x", input: "1"},
		{model: "x", input: "1", output: "1", write12h: "-"},
	} {
		if _, _, err := parseOpenAIPrices([]openAIPriceRow{row}, nil); err == nil {
			t.Errorf("%+v parsed", row)
		}
	}
}

// The table as OpenAI's pricing page read on 2026-11-18, row by row, in the
// page's own column order: input, output, cached input, 30-minute write and
// 12-hour write, "" where the page prints "–". gpt-6-cyber's long-context
// writes stay "" here: the page prints none, and openAIDerivedWrites holds
// what the table prices them at.
func TestOpenAIPriceTableMatchesThePricingPage(t *testing.T) {
	type row = [5]string // input, output, cached input, 30m write, 12h write
	want := map[openAIPriceKey]row{
		{"gpt-6.2-astra", "", ""}:                              {"8.00", "40.00", "0.40", "10.00", "20.00"},
		{"gpt-6.2-astra", "", over272K}:                        {"16.00", "60.00", "0.80", "20.00", "40.00"},
		{"gpt-6.2-sol", "", ""}:                                {"2.00", "10.00", "0.10", "2.50", "5.00"},
		{"gpt-6.2-sol", "", over272K}:                          {"4.00", "15.00", "0.20", "5.00", "10.00"},
		{"gpt-6-astra", "", ""}:                                {"10.00", "50.00", "1.00", "12.50", "25.00"},
		{"gpt-6-astra", "", over272K}:                          {"20.00", "75.00", "2.00", "25.00", "50.00"},
		{"gpt-6.1-sol", "", ""}:                                {"2.00", "10.00", "0.10", "2.50", "5.00"},
		{"gpt-6.1-sol", "", over272K}:                          {"4.00", "15.00", "0.20", "5.00", "10.00"},
		{"gpt-6.1-terra", "", ""}:                              {"1.00", "5.00", "0.05", "1.25", "2.50"},
		{"gpt-6.1-terra", "", over272K}:                        {"2.00", "7.50", "0.10", "2.50", "5.00"},
		{"gpt-6-luna", "", ""}:                                 {"0.10", "0.50", "0.01", "0.125", "0.25"},
		{"gpt-6-luna", "", over272K}:                           {"0.20", "0.75", "0.02", "0.25", "0.50"},
		{"gpt-6-sol", "", ""}:                                  {"2.00", "10.00", "0.20", "2.50", "5.00"},
		{"gpt-6-sol", "", over272K}:                            {"4.00", "15.00", "0.40", "5.00", "10.00"},
		{"gpt-5.6-sol", "", ""}:                                {"4.00", "20.00", "0.40", "5.00", "10.00"},
		{"gpt-5.6-sol", "", over272K}:                          {"8.00", "30.00", "0.80", "10.00", "20.00"},
		{"gpt-5.6-terra", "", ""}:                              {"2.00", "12.00", "0.20", "2.50", "5.00"},
		{"gpt-5.6-terra", "", over272K}:                        {"4.00", "18.00", "0.40", "5.00", "10.00"},
		{"gpt-5.6-luna", "", ""}:                               {"0.20", "1.20", "0.02", "0.25", "0.50"},
		{"gpt-5.6-luna", "", over272K}:                         {"0.40", "1.80", "0.04", "0.50", "1.00"},
		{"gpt-5.5", "", ""}:                                    {"5.00", "30.00", "0.50", "", ""},
		{"gpt-5.5", "", over272K}:                              {"10.00", "45.00", "1.00", "", ""},
		{"gpt-5.5-pro", "", ""}:                                {"30.00", "180.00", "", "", ""},
		{"gpt-5.5-pro", "", over272K}:                          {"60.00", "270.00", "", "", ""},
		{"gpt-5.4", "", ""}:                                    {"2.50", "15.00", "0.25", "", ""},
		{"gpt-5.4", "", over272K}:                              {"5.00", "22.50", "0.50", "", ""},
		{"gpt-5.4-pro", "", ""}:                                {"30.00", "180.00", "", "", ""},
		{"gpt-5.4-pro", "", over272K}:                          {"60.00", "270.00", "", "", ""},
		{"gpt-5.4-mini", "", ""}:                               {"0.75", "4.50", "0.075", "", ""},
		{"gpt-5.4-nano", "", ""}:                               {"0.20", "1.25", "0.02", "", ""},
		{"gpt-5.2", "", ""}:                                    {"1.75", "14.00", "0.175", "", ""},
		{"gpt-5.2-pro", "", ""}:                                {"21.00", "168.00", "", "", ""},
		{"gpt-5.1", "", ""}:                                    {"1.25", "10.00", "0.125", "", ""},
		{"gpt-5", "", ""}:                                      {"1.25", "10.00", "0.125", "", ""},
		{"gpt-5-mini", "", ""}:                                 {"0.25", "2.00", "0.025", "", ""},
		{"gpt-5-nano", "", ""}:                                 {"0.05", "0.40", "0.005", "", ""},
		{"gpt-5-pro", "", ""}:                                  {"15.00", "120.00", "", "", ""},
		{"gpt-4.1", "", ""}:                                    {"2.00", "8.00", "0.50", "", ""},
		{"gpt-4.1-mini", "", ""}:                               {"0.40", "1.60", "0.10", "", ""},
		{"gpt-4o", "", ""}:                                     {"2.50", "10.00", "1.25", "", ""},
		{"gpt-4o-mini", "", ""}:                                {"0.15", "0.60", "0.075", "", ""},
		{"o3-pro", "", ""}:                                     {"20.00", "80.00", "", "", ""},
		{"o3", "", ""}:                                         {"2.00", "8.00", "0.50", "", ""},
		{"gpt-6-astra", client.MeterTierFlex, ""}:              {"5.00", "25.00", "0.50", "6.25", "12.50"},
		{"gpt-6-astra", client.MeterTierFlex, over272K}:        {"10.00", "37.50", "1.00", "12.50", "25.00"},
		{"gpt-6.1-sol", client.MeterTierFlex, ""}:              {"1.00", "5.00", "0.05", "1.25", "2.50"},
		{"gpt-6.1-sol", client.MeterTierFlex, over272K}:        {"2.00", "7.50", "0.10", "2.50", "5.00"},
		{"gpt-6.1-terra", client.MeterTierFlex, ""}:            {"0.50", "2.50", "0.025", "0.625", "1.25"},
		{"gpt-6.1-terra", client.MeterTierFlex, over272K}:      {"1.00", "3.75", "0.05", "1.25", "2.50"},
		{"gpt-6-luna", client.MeterTierFlex, ""}:               {"0.05", "0.25", "0.005", "0.0625", "0.125"},
		{"gpt-6-luna", client.MeterTierFlex, over272K}:         {"0.10", "0.375", "0.01", "0.125", "0.25"},
		{"gpt-6-sol", client.MeterTierFlex, ""}:                {"1.00", "5.00", "0.10", "1.25", "2.50"},
		{"gpt-6-sol", client.MeterTierFlex, over272K}:          {"2.00", "7.50", "0.20", "2.50", "5.00"},
		{"gpt-5.6-sol", client.MeterTierFlex, ""}:              {"2.00", "10.00", "0.20", "2.50", "5.00"},
		{"gpt-5.6-sol", client.MeterTierFlex, over272K}:        {"4.00", "15.00", "0.40", "5.00", "10.00"},
		{"gpt-5.6-terra", client.MeterTierFlex, ""}:            {"1.00", "6.00", "0.10", "1.25", "2.50"},
		{"gpt-5.6-terra", client.MeterTierFlex, over272K}:      {"2.00", "9.00", "0.20", "2.50", "5.00"},
		{"gpt-5.6-luna", client.MeterTierFlex, ""}:             {"0.10", "0.60", "0.01", "0.125", "0.25"},
		{"gpt-5.6-luna", client.MeterTierFlex, over272K}:       {"0.20", "0.90", "0.02", "0.25", "0.50"},
		{"gpt-5.5", client.MeterTierFlex, ""}:                  {"2.50", "15.00", "0.25", "", ""},
		{"gpt-5.5", client.MeterTierFlex, over272K}:            {"5.00", "22.50", "0.50", "", ""},
		{"gpt-5.5-pro", client.MeterTierFlex, ""}:              {"15.00", "90.00", "", "", ""},
		{"gpt-5.5-pro", client.MeterTierFlex, over272K}:        {"", "", "", "", ""},
		{"gpt-5.4", client.MeterTierFlex, ""}:                  {"1.25", "7.50", "0.13", "", ""},
		{"gpt-5.4", client.MeterTierFlex, over272K}:            {"2.50", "11.25", "0.25", "", ""},
		{"gpt-5.4-pro", client.MeterTierFlex, ""}:              {"15.00", "90.00", "", "", ""},
		{"gpt-5.4-pro", client.MeterTierFlex, over272K}:        {"30.00", "135.00", "", "", ""},
		{"gpt-5.4-mini", client.MeterTierFlex, ""}:             {"0.375", "2.25", "0.0375", "", ""},
		{"gpt-5.4-nano", client.MeterTierFlex, ""}:             {"0.10", "0.625", "0.01", "", ""},
		{"gpt-5.2", client.MeterTierFlex, ""}:                  {"0.875", "7.00", "0.0875", "", ""},
		{"gpt-5.1", client.MeterTierFlex, ""}:                  {"0.625", "5.00", "0.0625", "", ""},
		{"gpt-5", client.MeterTierFlex, ""}:                    {"0.625", "5.00", "0.0625", "", ""},
		{"gpt-5-mini", client.MeterTierFlex, ""}:               {"0.125", "1.00", "0.0125", "", ""},
		{"gpt-5-nano", client.MeterTierFlex, ""}:               {"0.025", "0.20", "0.0025", "", ""},
		{"o3", client.MeterTierFlex, ""}:                       {"1.00", "4.00", "0.25", "", ""},
		{"gpt-6.2-astra", client.MeterTierFast, ""}:            {"16.00", "80.00", "0.80", "20.00", "40.00"},
		{"gpt-6.2-astra", client.MeterTierFast, over272K}:      {"32.00", "120.00", "1.60", "40.00", "80.00"},
		{"gpt-6.2-sol", client.MeterTierFast, ""}:              {"4.00", "20.00", "0.20", "5.00", "10.00"},
		{"gpt-6.2-sol", client.MeterTierFast, over272K}:        {"8.00", "30.00", "0.40", "10.00", "20.00"},
		{"gpt-6-astra", client.MeterTierFast, ""}:              {"20.00", "100.00", "2.00", "25.00", "50.00"},
		{"gpt-6-astra", client.MeterTierFast, over272K}:        {"40.00", "150.00", "4.00", "50.00", "100.00"},
		{"gpt-6.1-sol", client.MeterTierFast, ""}:              {"4.00", "20.00", "0.20", "5.00", "10.00"},
		{"gpt-6.1-sol", client.MeterTierFast, over272K}:        {"8.00", "30.00", "0.40", "10.00", "20.00"},
		{"gpt-6.1-terra", client.MeterTierFast, ""}:            {"2.00", "10.00", "0.10", "2.50", "5.00"},
		{"gpt-6.1-terra", client.MeterTierFast, over272K}:      {"4.00", "15.00", "0.20", "5.00", "10.00"},
		{"gpt-6-luna", client.MeterTierFast, ""}:               {"0.20", "1.00", "0.02", "0.25", "0.50"},
		{"gpt-6-luna", client.MeterTierFast, over272K}:         {"0.40", "1.50", "0.04", "0.50", "1.00"},
		{"gpt-6-sol", client.MeterTierFast, ""}:                {"4.00", "20.00", "0.40", "5.00", "10.00"},
		{"gpt-6-sol", client.MeterTierFast, over272K}:          {"8.00", "30.00", "0.80", "10.00", "20.00"},
		{"gpt-5.6-sol", client.MeterTierFast, ""}:              {"8.00", "40.00", "0.80", "10.00", "20.00"},
		{"gpt-5.6-sol", client.MeterTierFast, over272K}:        {"16.00", "60.00", "1.60", "20.00", "40.00"},
		{"gpt-5.6-terra", client.MeterTierFast, ""}:            {"4.00", "24.00", "0.40", "5.00", "10.00"},
		{"gpt-5.6-terra", client.MeterTierFast, over272K}:      {"8.00", "36.00", "0.80", "10.00", "20.00"},
		{"gpt-5.6-luna", client.MeterTierFast, ""}:             {"0.40", "2.40", "0.04", "0.50", "1.00"},
		{"gpt-5.6-luna", client.MeterTierFast, over272K}:       {"0.80", "3.60", "0.08", "1.00", "2.00"},
		{"gpt-5.5", client.MeterTierFast, ""}:                  {"12.50", "75.00", "1.25", "", ""},
		{"gpt-5.5", client.MeterTierFast, over272K}:            {"", "", "", "", ""},
		{"gpt-5.4", client.MeterTierFast, ""}:                  {"5.00", "30.00", "0.50", "", ""},
		{"gpt-5.4", client.MeterTierFast, over272K}:            {"", "", "", "", ""},
		{"gpt-5.4-mini", client.MeterTierFast, ""}:             {"1.50", "9.00", "0.15", "", ""},
		{"gpt-5.2", client.MeterTierFast, ""}:                  {"3.50", "28.00", "0.35", "", ""},
		{"gpt-5.1", client.MeterTierFast, ""}:                  {"2.50", "20.00", "0.25", "", ""},
		{"gpt-5", client.MeterTierFast, ""}:                    {"2.50", "20.00", "0.25", "", ""},
		{"gpt-5-mini", client.MeterTierFast, ""}:               {"0.45", "3.60", "0.045", "", ""},
		{"gpt-4.1", client.MeterTierFast, ""}:                  {"3.50", "14.00", "0.875", "", ""},
		{"gpt-4.1-mini", client.MeterTierFast, ""}:             {"0.70", "2.80", "0.175", "", ""},
		{"gpt-4o", client.MeterTierFast, ""}:                   {"4.25", "17.00", "2.125", "", ""},
		{"gpt-4o-mini", client.MeterTierFast, ""}:              {"0.25", "1.00", "0.125", "", ""},
		{"o3", client.MeterTierFast, ""}:                       {"3.50", "14.00", "0.875", "", ""},
		{"gpt-6-astra", client.MeterTierUltrafast, ""}:         {"60.00", "300.00", "6.00", "75.00", "150.00"},
		{"gpt-6-astra", client.MeterTierUltrafast, over272K}:   {"120.00", "450.00", "12.00", "150.00", "300.00"},
		{"gpt-6.1-sol", client.MeterTierUltrafast, ""}:         {"12.00", "60.00", "0.60", "15.00", "30.00"},
		{"gpt-6.1-sol", client.MeterTierUltrafast, over272K}:   {"24.00", "90.00", "1.20", "30.00", "60.00"},
		{"gpt-6.2-astra", client.MeterTierUltrafast, ""}:       {"24.00", "120.00", "1.20", "30.00", "60.00"},
		{"gpt-6.2-astra", client.MeterTierUltrafast, over272K}: {"48.00", "180.00", "2.40", "60.00", "120.00"},
		{"gpt-6.2-sol", client.MeterTierUltrafast, ""}:         {"6.00", "30.00", "0.30", "7.50", "15.00"},
		{"gpt-6.2-sol", client.MeterTierUltrafast, over272K}:   {"12.00", "45.00", "0.60", "15.00", "30.00"},
		{"gpt-6-cyber", "", ""}:                                {"20.00", "100.00", "2.00", "25.00", "50.00"},
		{"gpt-6-cyber", "", over272K}:                          {"40.00", "150.00", "4.00", "", ""},
		{"gpt-5.6-cyber", "", ""}:                              {"12.50", "75.00", "1.25", "15.625", "31.25"},
		{"gpt-5.6-cyber", "", over272K}:                        {"", "", "", "", ""},
		{"gpt-5.5-cyber-preview", "", ""}:                      {"12.50", "75.00", "1.25", "", ""},
		{"gpt-5.5-cyber-preview", "", over272K}:                {"", "", "", "", ""},
		{"gpt-6-rosalind", "", ""}:                             {"10.00", "50.00", "1.00", "12.50", "25.00"},
		{"gpt-6-rosalind", "", over272K}:                       {"20.00", "75.00", "2.00", "25.00", "50.00"},
		{"gpt-rosalind-research", "", ""}:                      {"5.00", "25.00", "0.50", "", ""},
		{"gpt-rosalind-research", "", over272K}:                {"", "", "", "", ""},
		{"gpt-rosalind-discovery", "", ""}:                     {"5.00", "25.00", "0.50", "", ""},
		{"gpt-rosalind-discovery", "", over272K}:               {"", "", "", "", ""},
		{"gpt-6.2-astra-law", "", ""}:                          {"8.00", "40.00", "0.40", "10.00", "20.00"},
		{"gpt-6.2-astra-law", "", over272K}:                    {"16.00", "60.00", "0.80", "20.00", "40.00"},
		{"gpt-6-astra-law", "", ""}:                            {"10.00", "50.00", "1.00", "", ""},
		{"gpt-6-astra-law", "", over272K}:                      {"20.00", "75.00", "2.00", "", ""},
		{"chat-latest", "", ""}:                                {"5.00", "30.00", "0.50", "", ""},
		{"gpt-5.3-codex", "", ""}:                              {"1.75", "14.00", "0.175", "", ""},
		{"gpt-5-search-api", "", ""}:                           {"1.25", "10.00", "0.125", "", ""},
		{"gpt-5.3-codex", client.MeterTierFast, ""}:            {"3.50", "28.00", "0.35", "", ""},
		// Not on the pricing page: the comparison table on gpt-5.3-codex's
		// model page.
		{"gpt-5.2-codex", "", ""}: {"1.75", "14.00", "0.175", "", ""},
	}
	if len(openAIPriceTable) != len(want) {
		t.Fatalf("the table has %d rows; the page lists %d", len(openAIPriceTable), len(want))
	}
	same := func(a, b string) bool {
		if a == "" || b == "" {
			return a == b
		}
		x, okX := new(big.Rat).SetString(a)
		y, okY := new(big.Rat).SetString(b)
		return okX && okY && x.Cmp(y) == 0
	}
	for _, r := range openAIPriceTable {
		key := openAIPriceKey{r.model, r.tier, r.prompt}
		page, ok := want[key]
		if !ok {
			t.Errorf("%v is not on the page", key)
			continue
		}
		got := row{r.input, r.output, r.cached, r.write30m, r.write12h}
		for i := range got {
			if !same(got[i], page[i]) {
				t.Errorf("%v column %d = %q, the page prints %q", key, i, got[i], page[i])
			}
		}
	}

	// What each row prices at: cached input and the write at the input rate
	// where the page prints "–", every write at the 30-minute rate, and
	// nothing for a line with no figures.
	for key, page := range want {
		r, priced := openAIPrices[key]
		if page[0] == "" && page[1] == "" {
			if priced {
				t.Errorf("%v has no figures but is priced", key)
			}
			continue
		}
		if !priced {
			t.Errorf("%v is not priced", key)
			continue
		}
		orInput := func(cell string) string {
			if cell == "" {
				return page[0]
			}
			return cell
		}
		write := orInput(page[3])
		if d, ok := openAIDerivedWrites[key]; ok && page[3] == "" {
			write = d[0]
		}
		for i, c := range []struct {
			got  *big.Rat
			want string
		}{{r.input, page[0]}, {r.out, page[1]}, {r.read, orInput(page[2])}, {r.write5m, write}, {r.write1h, write}} {
			expected, _ := new(big.Rat).SetString(c.want)
			if got := new(big.Rat).Mul(c.got, tokensPerMillion); got.Cmp(expected) != 0 {
				t.Errorf("%v priced column %d = %s per million, want %s", key, i, got.FloatString(4), c.want)
			}
		}
	}
}

// The page prices a request over 272K input tokens at 2x input, cached input
// and writes and 1.5x output, for the whole request. Every cell the page
// prints in both a priced short row and a priced long row of one model and
// tier follows that rule, but one, and gpt-6-cyber's derived writes are 2x
// its short ones.
func TestOpenAILongContextRowsFollowTheRule(t *testing.T) {
	short := map[openAIPriceKey]openAIPriceRow{}
	for _, row := range openAIPriceTable {
		if row.prompt == "" {
			short[openAIPriceKey{row.model, row.tier, ""}] = row
		}
	}
	ratOf := func(text string) *big.Rat {
		r, ok := new(big.Rat).SetString(text)
		if !ok {
			t.Fatalf("%q is not a number", text)
		}
		return r
	}
	two, oneAndAHalf := big.NewRat(2, 1), big.NewRat(3, 2)
	pairs := 0
	for _, long := range openAIPriceTable {
		if long.prompt != over272K || (long.input == "" && long.output == "") {
			continue
		}
		key := openAIPriceKey{long.model, long.tier, ""}
		s, ok := short[key]
		if !ok {
			t.Errorf("%v has a long-context row and no short one", key)
			continue
		}
		pairs++
		for _, c := range []struct {
			name, short, long string
			factor            *big.Rat
		}{
			{"input", s.input, long.input, two}, {"cached", s.cached, long.cached, two},
			{"30m write", s.write30m, long.write30m, two}, {"12h write", s.write12h, long.write12h, two},
			{"output", s.output, long.output, oneAndAHalf},
		} {
			if c.short == "" || c.long == "" {
				continue
			}
			// The one exception: Flex gpt-5.4's short cached rate is printed
			// 0.13, the page's rounding of 0.125, and its long one 0.25.
			if key == (openAIPriceKey{"gpt-5.4", client.MeterTierFlex, ""}) && c.name == "cached" {
				if c.short != "0.13" || c.long != "0.25" {
					t.Errorf("Flex gpt-5.4 cached is %s and %s; the exception no longer applies", c.short, c.long)
				}
				continue
			}
			if want := new(big.Rat).Mul(ratOf(c.short), c.factor); ratOf(c.long).Cmp(want) != 0 {
				t.Errorf("%v %s: long %s, short %s times %s is %s", key, c.name, c.long, c.short, c.factor.RatString(), want.FloatString(4))
			}
		}
	}
	if pairs == 0 {
		t.Fatal("no short and long pair was checked")
	}
	for key, writes := range openAIDerivedWrites {
		s := short[openAIPriceKey{key.model, key.tier, ""}]
		if key.prompt != over272K || s.write30m == "" || s.write12h == "" {
			t.Errorf("%v: a derived write needs a long-context row and the short row's writes", key)
			continue
		}
		for i, short := range []string{s.write30m, s.write12h} {
			if want := new(big.Rat).Mul(ratOf(short), two); ratOf(writes[i]).Cmp(want) != 0 {
				t.Errorf("%v derived write %d = %s, want 2x %s", key, i, writes[i], short)
			}
		}
		for _, row := range openAIPriceTable {
			if (openAIPriceKey{row.model, row.tier, row.prompt}) == key && (row.write30m != "" || row.write12h != "") {
				t.Errorf("%v: the page now prints its writes; drop the derived ones", key)
			}
		}
	}
}

// The models the page prices by context length are exactly those with a
// long-context row, priced or not; every other priced model is single-rate.
func TestOpenAILongContextModels(t *testing.T) {
	want := []string{
		"gpt-6.2-astra", "gpt-6.2-sol", "gpt-6-astra", "gpt-6.1-sol", "gpt-6.1-terra", "gpt-6-luna", "gpt-6-sol",
		"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.5-pro", "gpt-5.4", "gpt-5.4-pro",
		"gpt-6-cyber", "gpt-5.6-cyber", "gpt-5.5-cyber-preview", "gpt-6-rosalind", "gpt-rosalind-research",
		"gpt-rosalind-discovery", "gpt-6.2-astra-law", "gpt-6-astra-law",
	}
	got := slices.Sorted(maps.Keys(openAILongContextModels))
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("long-context models\n got %v\nwant %v", got, want)
	}
}

func TestOpenAICanonicalModel(t *testing.T) {
	for in, want := range map[string]string{
		"gpt-6-sol":             "gpt-6-sol",
		"GPT-6-Sol-2026-08-01":  "gpt-6-sol",
		"  gpt-5.4-mini  ":      "gpt-5.4-mini",
		"gpt-4.1-2025-04-14":    "gpt-4.1",
		"gpt-4o-2024-08-06":     "gpt-4o",
		"o3-2025-04-16":         "o3",
		"gpt-5.2-codex":         "gpt-5.2-codex",
		"chat-latest":           "chat-latest",
		"ft:gpt-4.1-2025-04-14": "ft:gpt-4.1",
		// Only OpenAI's dated form is a snapshot.
		"gpt-6-sol-20260801": "gpt-6-sol-20260801",
		"gpt-6.1-terra-2026": "gpt-6.1-terra-2026",
	} {
		if got := openAICanonicalModel(in); got != want {
			t.Errorf("openAICanonicalModel(%q) = %q, want %q", in, got, want)
		}
	}
}

// Which row prices an entry, and what an unpriced one is listed as.
func TestOpenAILookup(t *testing.T) {
	million := uint64(1_000_000)
	for _, tc := range []struct {
		name     string
		usage    client.MeterUsage
		priced   string
		unpriced string
	}{
		{"a single-rate model over 272K is priced at its one rate",
			client.MeterUsage{Model: "gpt-5.4-mini", Prompt: over272K, Input: million}, "$0.75", ""},
		{"chat-latest over 272K", client.MeterUsage{Model: "chat-latest", Prompt: over272K, Output: million}, "$30.00", ""},
		{"a model priced by length, over 272K", client.MeterUsage{Model: "gpt-6-sol", Prompt: over272K, Input: million, Output: million}, "$19.00", ""},
		{"a dated snapshot", client.MeterUsage{Model: "gpt-6-sol-2026-08-01", Input: million}, "$2.00", ""},
		{"Fast gpt-5.5 has no long-context figures",
			client.MeterUsage{Model: "gpt-5.5", Tier: client.MeterTierFast, Prompt: over272K, Input: million}, "", "gpt-5.5 fast over 272K"},
		{"Fast gpt-5.4 has none either",
			client.MeterUsage{Model: "gpt-5.4", Tier: client.MeterTierFast, Prompt: over272K, Input: million}, "", "gpt-5.4 fast over 272K"},
		{"nor Flex gpt-5.5-pro",
			client.MeterUsage{Model: "gpt-5.5-pro", Tier: client.MeterTierFlex, Prompt: over272K, Input: million}, "", "gpt-5.5-pro flex over 272K"},
		{"nor gpt-5.6-cyber", client.MeterUsage{Model: "gpt-5.6-cyber", Prompt: over272K, Input: million}, "", "gpt-5.6-cyber over 272K"},
		{"a tier with no row for the model",
			client.MeterUsage{Model: "gpt-5.6-sol", Tier: client.MeterTierUltrafast, Input: million}, "", "gpt-5.6-sol ultrafast"},
		{"the same over 272K: the tier has no short row either",
			client.MeterUsage{Model: "gpt-5.6-sol", Tier: client.MeterTierUltrafast, Prompt: over272K, Input: million}, "", "gpt-5.6-sol ultrafast"},
		{"no tier falls back to Standard",
			client.MeterUsage{Model: "gpt-6.2-astra", Tier: client.MeterTierFlex, Input: million}, "", "gpt-6.2-astra flex"},
		{"an unknown tier", client.MeterUsage{Model: "gpt-6-sol", Tier: client.MeterTierUnknown, Input: million}, "", "gpt-6-sol unknown tier"},
		{"gpt-5.5's cache writes are billed at the input rate", client.MeterUsage{Model: "gpt-5.5", CacheWrite: million}, "$5.00", ""},
		{"so are gpt-6-astra-law's", client.MeterUsage{Model: "gpt-6-astra-law", CacheWrite: million}, "$10.00", ""},
		{"and gpt-5.5-pro's cached input", client.MeterUsage{Model: "gpt-5.5-pro", CacheRead: million}, "$30.00", ""},
		{"gpt-6-cyber's short-context writes", client.MeterUsage{Model: "gpt-6-cyber", CacheWrite: million}, "$25.00", ""},
		{"gpt-6-cyber's long-context writes are derived",
			client.MeterUsage{Model: "gpt-6-cyber", Prompt: over272K, CacheWrite: million}, "$50.00", ""},
		{"writes are at the 30-minute rate", client.MeterUsage{Model: "gpt-6-sol", CacheWrite: million}, "$2.50", ""},
		{"gpt-5.2-codex", client.MeterUsage{Model: "gpt-5.2-codex", Input: million, Output: million}, "$15.75", ""},
		{"a fine-tuned model", client.MeterUsage{Model: "ft:gpt-4.1-2025-04-14", Input: million}, "", "ft:gpt-4.1-2025-04-14"},
		{"an image model", client.MeterUsage{Model: "gpt-image-2", Input: million}, "", "gpt-image-2"},
	} {
		c := openAIPriceUsage([]client.MeterUsage{tc.usage})
		list := c.unpricedList()
		switch {
		case tc.priced != "" && (moneyOf(c.at5m) != tc.priced || len(list) != 0):
			t.Errorf("%s: %s, unpriced %v; want %s", tc.name, moneyOf(c.at5m), list, tc.priced)
		case tc.unpriced != "" && (c.at5m.Sign() != 0 || len(list) != 1 || list[0].Model != tc.unpriced || list[0].Tokens != tokensOf(tc.usage)):
			t.Errorf("%s: %s, unpriced %v; want %q", tc.name, moneyOf(c.at5m), list, tc.unpriced)
		}
		if c.at1h.Cmp(c.at5m) != 0 {
			t.Errorf("%s: at1h %s, at5m %s", tc.name, c.at1h.RatString(), c.at5m.RatString())
		}
	}

	// Every row, with tokens of every kind: the 1-hour figure is always the
	// 5-minute one, and nothing is unpriced.
	for key := range openAIPrices {
		u := client.MeterUsage{Model: key.model, Tier: key.tier, Prompt: key.prompt, Input: million, Output: million, CacheRead: million, CacheWrite: million}
		c := openAIPriceUsage([]client.MeterUsage{u})
		if c.at5m.Sign() <= 0 || c.at1h.Cmp(c.at5m) != 0 || len(c.unpriced) != 0 {
			t.Errorf("%v: at5m %s at1h %s unpriced %v", key, c.at5m.RatString(), c.at1h.RatString(), c.unpriced)
		}
	}

	// (other) is left out and never listed; entries of one label add up.
	c := openAIPriceUsage([]client.MeterUsage{
		{Model: client.MeterOtherModel, Input: 50 * million},
		{Model: "gpt-6-sol", Tier: client.MeterTierUnknown, Input: million},
		{Model: "gpt-6-sol", Tier: client.MeterTierUnknown, Prompt: over272K, Output: 200_000},
	})
	if c.at5m.Sign() != 0 || len(c.unpriced) != 1 || c.unpriced["gpt-6-sol unknown tier"] != 1_200_000 {
		t.Errorf("unpriced %v at %s", c.unpriced, moneyOf(c.at5m))
	}
}
