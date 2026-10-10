package aggregate

import (
	"errors"
	"math/big"
	"regexp"
	"sort"
	"strings"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// The price table's provenance, emitted in the document so a reader can see
// how old the prices behind every estimate are.
const (
	pricingAsOf   = "2026-10-09"
	pricingSource = "https://platform.claude.com/docs/en/about-claude/pricing"
	// pricingCacheWrites is the cache-write rate every amount uses. CPA does
	// not say whether a write was for 5 minutes or an hour, and 5 minutes is
	// Anthropic's default; the difference to the 1-hour rate is reported
	// beside each account as cacheWriteExtra.
	pricingCacheWrites = "5m"
)

// priceRow is one row of Anthropic's pricing page as it read on pricingAsOf,
// in US dollars per million tokens, kept as the page prints it.
type priceRow struct {
	// model is the canonical id canonicalModel reduces a request's model to.
	model string
	// prompt is the meter's prompt class the row prices: "" for every model,
	// and qc.MeterPromptOver100K for Haiku 5.5's long prompts.
	prompt                             string
	input, write5m, write1h, read, out string
}

// priceTable is the pricing page on 2026-10-09, and model ids from the model
// deprecations page and the models overview the same day. Retired models stay,
// so a meter still holding their usage prices it.
//
// Only Haiku 5.5 has a prompt-length tier: the page bills Claude 4.6 and later
// at standard prices across the full context window, except Haiku 5.5, which
// counts every input token, cache reads and writes included, toward its
// 100,000-token tier. It lists no tier for Sonnet 4.5 or Sonnet 4.
var priceTable = []priceRow{
	{model: "claude-fable-5-1", input: "10", write5m: "12.50", write1h: "20", read: "0.25", out: "50"},
	{model: "claude-mythos-5-1", input: "10", write5m: "12.50", write1h: "20", read: "0.25", out: "50"},
	{model: "claude-fable-5", input: "10", write5m: "12.50", write1h: "20", read: "1", out: "50"},
	{model: "claude-mythos-5", input: "10", write5m: "12.50", write1h: "20", read: "1", out: "50"},
	{model: "claude-opus-5-5", input: "4", write5m: "5", write1h: "8", read: "0.20", out: "20"},
	{model: "claude-opus-5", input: "5", write5m: "6.25", write1h: "10", read: "0.50", out: "25"},
	{model: "claude-opus-4-8", input: "5", write5m: "6.25", write1h: "10", read: "0.50", out: "25"},
	{model: "claude-opus-4-7", input: "5", write5m: "6.25", write1h: "10", read: "0.50", out: "25"},
	{model: "claude-opus-4-6", input: "5", write5m: "6.25", write1h: "10", read: "0.50", out: "25"},
	{model: "claude-opus-4-5", input: "5", write5m: "6.25", write1h: "10", read: "0.50", out: "25"},
	{model: "claude-opus-4-1", input: "15", write5m: "18.75", write1h: "30", read: "1.50", out: "75"},
	{model: "claude-opus-4", input: "15", write5m: "18.75", write1h: "30", read: "1.50", out: "75"},
	{model: "claude-sonnet-5-5", input: "2", write5m: "2.50", write1h: "4", read: "0.10", out: "10"},
	{model: "claude-sonnet-5", input: "2", write5m: "2.50", write1h: "4", read: "0.20", out: "10"},
	{model: "claude-sonnet-4-6", input: "3", write5m: "3.75", write1h: "6", read: "0.30", out: "15"},
	{model: "claude-sonnet-4-5", input: "3", write5m: "3.75", write1h: "6", read: "0.30", out: "15"},
	{model: "claude-sonnet-4", input: "3", write5m: "3.75", write1h: "6", read: "0.30", out: "15"},
	{model: "claude-haiku-5-5", input: "0.10", write5m: "0.125", write1h: "0.20", read: "0.01", out: "0.50"},
	{model: "claude-haiku-5-5", prompt: qc.MeterPromptOver100K, input: "0.50", write5m: "0.625", write1h: "1", read: "0.05", out: "2.50"},
	{model: "claude-haiku-4-5", input: "1", write5m: "1.25", write1h: "2", read: "0.10", out: "5"},
	{model: "claude-3-5-haiku", input: "0.80", write5m: "1", write1h: "1.60", read: "0.08", out: "4"},
}

// rates is one row parsed: exact dollars per token, so a cost is a plain sum
// of products with nothing left to divide.
type rates struct {
	input, write5m, write1h, read, out *big.Rat
}

type priceKey struct{ model, prompt string }

// prices is priceTable parsed once. A string that does not parse leaves its
// row out and is reported in pricesErr, which a test requires to be nil; a
// panic here would take CPA down with it.
var prices, pricesErr = parsePrices(priceTable)

var tokensPerMillion = big.NewRat(1_000_000, 1)

// priceShape is a price as the pricing page prints one: a plain decimal.
var priceShape = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

func parsePrices(table []priceRow) (map[priceKey]rates, error) {
	parsed := make(map[priceKey]rates, len(table))
	var failed []string
	for _, row := range table {
		var r rates
		ok := true
		for _, field := range []struct {
			into **big.Rat
			text string
		}{{&r.input, row.input}, {&r.write5m, row.write5m}, {&r.write1h, row.write1h}, {&r.read, row.read}, {&r.out, row.out}} {
			perMillion, valid := amountOf(field.text)
			if !valid || !priceShape.MatchString(field.text) {
				ok = false
				break
			}
			*field.into = perMillion.Quo(perMillion, tokensPerMillion)
		}
		if !ok {
			failed = append(failed, row.model)
			continue
		}
		parsed[priceKey{row.model, row.prompt}] = r
	}
	if len(failed) > 0 {
		return parsed, errors.New("price table rows do not parse: " + strings.Join(failed, ", "))
	}
	return parsed, nil
}

// dateSuffix is a trailing snapshot date, "-20250514".
var dateSuffix = regexp.MustCompile(`-[0-9]{8}$`)

// canonicalModel reduces a model id as CPA reports it to the table's: lower
// case and trimmed, without a leading "anthropic." (Bedrock), a trailing
// "-v1:0" or "-v1" (Bedrock), anything from "@" on (Vertex), or a trailing
// snapshot date. claude-opus-4-20250514 is claude-opus-4, and
// claude-haiku-4-5@20251001 is claude-haiku-4-5.
func canonicalModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	model = strings.TrimPrefix(model, "anthropic.")
	if trimmed := strings.TrimSuffix(model, "-v1:0"); trimmed != model {
		model = trimmed
	} else {
		model = strings.TrimSuffix(model, "-v1")
	}
	model, _, _ = strings.Cut(model, "@")
	return dateSuffix.ReplaceAllString(model, "")
}

// ratesOf finds the row that prices one usage entry. A prompt class the
// model has no tier for is priced at the model's base rate: the meter only
// classes Haiku 5.5, and an entry claiming the class on another model is not
// one Anthropic bills differently.
func ratesOf(model, prompt string) (rates, bool) {
	canonical := canonicalModel(model)
	if r, ok := prices[priceKey{canonical, prompt}]; ok {
		return r, true
	}
	r, ok := prices[priceKey{canonical, ""}]
	return r, ok
}

// costs is what one window of usage comes to: exact dollars at the 5-minute
// cache-write rate every amount uses, the same window at the 1-hour rate, and
// the tokens of every model the table has no price for, by model as the meter
// named it.
type costs struct {
	at5m, at1h *big.Rat
	unpriced   map[string]uint64
}

// priceUsage prices a meter window. (other) is left out without being listed:
// it is not a model, so there is nothing to price, and the meter marks the
// overflow that produced it on its own (LastOverflowAt, meterFull).
func priceUsage(usage []qc.MeterUsage) costs {
	c := costs{at5m: new(big.Rat), at1h: new(big.Rat), unpriced: map[string]uint64{}}
	for _, u := range usage {
		if u.Model == qc.MeterOtherModel {
			continue
		}
		r, ok := ratesOf(u.Model, u.Prompt)
		if !ok {
			c.unpriced[u.Model] = addTokens(c.unpriced[u.Model], tokensOf(u))
			continue
		}
		base := new(big.Rat)
		base.Add(base, tokenCost(u.Input, r.input))
		base.Add(base, tokenCost(u.Output, r.out))
		base.Add(base, tokenCost(u.CacheRead, r.read))
		c.at5m.Add(c.at5m, base)
		c.at5m.Add(c.at5m, tokenCost(u.CacheWrite, r.write5m))
		c.at1h.Add(c.at1h, base)
		c.at1h.Add(c.at1h, tokenCost(u.CacheWrite, r.write1h))
	}
	return c
}

func tokenCost(tokens uint64, perToken *big.Rat) *big.Rat {
	n := new(big.Rat).SetInt(new(big.Int).SetUint64(tokens))
	return n.Mul(n, perToken)
}

// tokensOf is an entry's billed tokens, every kind together, saturating as the
// meter's own counts do.
func tokensOf(u qc.MeterUsage) uint64 {
	return addTokens(addTokens(u.Input, u.Output), addTokens(u.CacheRead, u.CacheWrite))
}

func addTokens(a, b uint64) uint64 {
	if sum := a + b; sum >= a {
		return sum
	}
	return ^uint64(0)
}

// minus is a window's costs less a baseline taken inside it: the spend since a
// Console reading. Every baseline entry is also in the window (C.3), so a
// negative difference only comes from a meter that lost buckets, and is
// clamped to nothing.
func (c costs) minus(baseline costs) costs {
	out := costs{
		at5m:     nonNegative(new(big.Rat).Sub(c.at5m, baseline.at5m)),
		at1h:     nonNegative(new(big.Rat).Sub(c.at1h, baseline.at1h)),
		unpriced: map[string]uint64{},
	}
	for model, tokens := range c.unpriced {
		if before := baseline.unpriced[model]; tokens > before {
			out.unpriced[model] = tokens - before
		}
	}
	return out
}

// unpricedList is the unpriced models, most tokens first, then by name.
func (c costs) unpricedList() []APICreditUnpriced {
	list := make([]APICreditUnpriced, 0, len(c.unpriced))
	for model, tokens := range c.unpriced {
		if tokens > 0 {
			list = append(list, APICreditUnpriced{Model: model, Tokens: tokens})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Tokens != list[j].Tokens {
			return list[i].Tokens > list[j].Tokens
		}
		return list[i].Model < list[j].Model
	})
	return list
}

func nonNegative(r *big.Rat) *big.Rat {
	if r.Sign() < 0 {
		r.SetInt64(0)
	}
	return r
}
