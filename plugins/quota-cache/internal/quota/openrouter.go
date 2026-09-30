package quota

import (
	"context"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

// OpenRouter is not a CPA credential. The keys CPA routes inference with are
// refused by the endpoint that reports the account balance, so it is read with
// a management key the operator puts in this plugin's own configuration, and it
// is scheduled like any other account under OpenRouterProvider.
//
// Verified against OpenRouter's API reference on 2026-09-30:
//
//	GET https://openrouter.ai/api/v1/credits
//	    -> data.total_credits (USD purchased, all time), data.total_usage (USD
//	       spent, all time). A management key is required; an inference key is
//	       refused with 403.
const (
	OpenRouterProvider   = "openrouter"
	openRouterCreditsURL = "https://openrouter.ai/api/v1/credits"
)

// FetchOpenRouter reads the account's purchased and spent totals into
// balances.credits, in US dollars.
//
// The balance is their difference and is deliberately not computed here: like
// every other amount in the snapshot, both are kept as the decimal strings the
// provider sent, and the consumer that displays a balance owns the subtraction.
//
// The legacy ObservedAt is left zero. OpenRouter has no weekly window, and a
// consumer reading the compatibility fields must not mistake this entry for an
// account at 0% weekly use; quota.observed_at dates the observation instead.
func FetchOpenRouter(ctx context.Context, doer Doer, key string, now time.Time) (Observation, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return Observation{}, ErrNoAccessToken
	}
	response, err := doer.HTTPDo(ctx, protocol.HostHTTPRequest{
		Method: "GET",
		URL:    openRouterCreditsURL,
		Headers: map[string][]string{
			"Authorization": {"Bearer " + key},
			"Accept":        {"application/json"},
		},
	})
	if err != nil {
		return Observation{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Observation{}, HTTPStatusError{StatusCode: response.StatusCode}
	}
	if len(response.Body) > maxResponseBytes {
		return Observation{}, ErrInvalidResponse
	}
	root, err := decodeObject(response.Body)
	if err != nil {
		return Observation{}, err
	}
	data := object(root, "data")
	purchased, used := decimal(data, "total_credits"), decimal(data, "total_usage")
	// Half a balance is not a balance: either total alone would be displayed as
	// the other one's absence, so a response missing one fails the poll and the
	// last good figures stay up, marked failed.
	if purchased == "" || used == "" {
		return Observation{}, ErrInvalidResponse
	}
	return Observation{
		Provider: OpenRouterProvider,
		Quota: &client.Quota{Schema: 1, ObservedAt: now, Balances: map[string]client.Balance{
			"credits": {Unit: "usd", Limit: purchased, Used: used},
		}},
	}, nil
}
