package credits

import "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"

// Provider is which monthly API credit a state is computed for: the document
// it is published as, its price table and the words its sentences use. The
// rules, the order they apply in and the pool are the same for every
// provider, so one state reads like the other.
type Provider struct {
	// id is the document's CreditDocument.Provider: "" for Claude, whose
	// api_credits has never carried one.
	id    string
	title string
	// accountPrefix begins every account id an organization of this provider
	// has; an id without it is not one whose stored values can be edited
	// (settingsOf's cacheTooOld).
	accountPrefix string
	// entryProvider is the provider of the snapshot entry each item is taken
	// as.
	entryProvider string
	pricing       client.CreditPricing
	price         func([]client.MeterUsage) costs
	// sentence, when set, is the provider's own sentence for an issue; false
	// falls back to sentenceOf's, whose words fit every provider.
	sentence func(a *creditAccount, issue string) (string, bool)
}

// Claude is the monthly Claude API credit of the Console organizations
// quota-cache lists, published in client.CreditsPath. Input.Provider nil is
// Claude.
var Claude = &Provider{
	id:            "",
	title:         "Monthly API Credit",
	accountPrefix: "org-",
	entryProvider: client.ProviderAnthropicAPI,
	pricing:       client.CreditPricing{AsOf: pricingAsOf, Source: pricingSource, CacheWrites: pricingCacheWrites},
	price:         priceUsage,
}

// OpenAI is the monthly OpenAI API credit of the OpenAI API organizations
// quota-cache lists, published in client.OpenAICreditsPath and metered from
// CPA's openai-compatibility traffic.
var OpenAI = &Provider{
	id:            client.ProviderOpenAIAPI,
	title:         "Monthly OpenAI API Credit",
	accountPrefix: client.OpenAICreditAccountPrefix,
	entryProvider: client.ProviderOpenAIAPI,
	pricing:       client.CreditPricing{AsOf: openAIPricingAsOf, Source: openAIPricingSource, CacheWrites: openAIPricingCacheWrites},
	price:         openAIPriceUsage,
	sentence:      openAISentenceOf,
}
