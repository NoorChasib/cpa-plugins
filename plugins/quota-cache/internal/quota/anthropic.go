package quota

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

// A Claude Console organization's monthly API credit is not a CPA credential.
// Anthropic reports neither the credit nor its renewal, so the operator
// configures both, and this reads the one thing Anthropic does report: what the
// organization was charged each UTC day, with that organization's Admin API key.
//
// Verified against Anthropic's Usage and Cost API documentation on 2026-10-09:
//
//	GET https://api.anthropic.com/v1/organizations/cost_report
//	    ?starting_at=<RFC 3339>&ending_at=<RFC 3339>&bucket_width=1d&limit=31
//	    -> data[] daily buckets oldest first, one per day including days with no
//	       cost, each {starting_at, ending_at, results[{amount, currency}]};
//	       amount is a decimal string in the currency's lowest unit (cents);
//	       has_more and next_page page the buckets. Header x-api-key with an
//	       Admin API key, and anthropic-version: 2023-06-01.
//	GET https://api.anthropic.com/v1/organizations/me
//	    -> {id, name, type: "organization"}, asked only when the cost report's
//	       response lacks its anthropic-organization-id header.
const (
	anthropicCostReportURL   = "https://api.anthropic.com/v1/organizations/cost_report"
	anthropicOrganizationURL = "https://api.anthropic.com/v1/organizations/me"
	anthropicVersion         = "2023-06-01"

	// maxCostReportPages bounds one poll. The window is at most 31 daily
	// buckets and a page holds 31, so one page is expected; paging exists to
	// honour the contract, not because a second page should ever come.
	maxCostReportPages = 3
	maxCostReportDays  = 62
	maxCostResults     = 32
	maxNextPageLength  = 1024
)

var (
	organizationIDValue = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	costAmountValue     = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)
	currencyCodeValue   = regexp.MustCompile(`^[A-Z]{3}$`)
)

// FetchAPICredit reads the organization's daily spend for the credit cycle that
// contains now, renews being the configured renewal date. fingerprint is the
// key's, recorded beside the reading so a consumer can tell a reading made with
// a since-rotated key. userAgent identifies this plugin honestly: these are the
// operator's own reads, and Anthropic asks integrations to name themselves.
//
// Nothing is added up. Each day's amounts are kept as the strings Anthropic
// sent, and the consumer owns every sum. A response that breaks any rule below
// fails the whole poll, so the last good reading stays up, marked failed,
// rather than being replaced by a partial one.
//
// The key goes into the x-api-key header and nowhere else.
func FetchAPICredit(ctx context.Context, doer Doer, key, renews, fingerprint, userAgent string, now time.Time) (Observation, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return Observation{}, ErrNoAccessToken
	}
	now = now.UTC()
	cycle, err := client.CreditCycleAt(renews, now)
	if err != nil {
		return Observation{}, err
	}
	start, end := cycle.Start, client.CostReportEndingAt(now)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	headers := func() map[string][]string {
		return map[string][]string{
			"X-Api-Key":         {key},
			"Anthropic-Version": {anthropicVersion},
			"Accept":            {"application/json"},
			"User-Agent":        {userAgent},
		}
	}
	query := "?starting_at=" + start.Format(time.RFC3339) + "&ending_at=" + end.Format(time.RFC3339) + "&bucket_width=1d&limit=31"

	days := []client.CostDay{}
	organizationID := ""
	buckets, page := 0, ""
pages:
	for request := 0; ; request++ {
		if request == maxCostReportPages {
			return Observation{}, ErrInvalidResponse
		}
		target := anthropicCostReportURL + query
		if page != "" {
			target += "&page=" + url.QueryEscape(page)
		}
		response, err := doer.HTTPDo(ctx, protocol.HostHTTPRequest{Method: "GET", URL: target, Headers: headers()})
		if err != nil {
			return Observation{}, err
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return Observation{}, HTTPStatusError{StatusCode: response.StatusCode}
		}
		if request == 0 {
			organizationID = organizationHeader(response.Headers)
		}
		if len(response.Body) > maxResponseBytes {
			return Observation{}, ErrInvalidResponse
		}
		root, err := decodeObject(response.Body)
		if err != nil {
			return Observation{}, err
		}
		data, ok := root["data"].([]any)
		if !ok {
			return Observation{}, ErrInvalidResponse
		}
		if buckets += len(data); buckets > maxCostReportDays {
			return Observation{}, ErrInvalidResponse
		}
		for _, raw := range data {
			bucket, ok := raw.(map[string]any)
			if !ok {
				return Observation{}, ErrInvalidResponse
			}
			startsAt, ok1 := bucketInstant(bucket["starting_at"])
			endsAt, ok2 := bucketInstant(bucket["ending_at"])
			if !ok1 || !ok2 {
				return Observation{}, ErrInvalidResponse
			}
			// A bucket that starts at or after the ending_at this poll sent is
			// one Anthropic should not have returned. It and everything after
			// it are dropped rather than failing the poll, so no bucket kept
			// can lie in the future.
			if !startsAt.Before(end) {
				break pages
			}
			// Days run without a gap from the cycle's first, so each bucket
			// must start exactly where the previous one ended, at 00:00 UTC,
			// and last one day.
			expected := start.Add(time.Duration(len(days)) * 24 * time.Hour)
			if !startsAt.Equal(expected) || !endsAt.Equal(startsAt.Add(24*time.Hour)) {
				return Observation{}, ErrInvalidResponse
			}
			amounts, ok := costAmounts(bucket["results"])
			if !ok {
				return Observation{}, ErrInvalidResponse
			}
			days = append(days, client.CostDay{StartingAt: startsAt, Amounts: amounts})
		}
		more, _ := root["has_more"].(bool)
		if !more {
			break
		}
		next, ok := root["next_page"].(string)
		if !ok || next == "" || len(next) > maxNextPageLength {
			return Observation{}, ErrInvalidResponse
		}
		page = next
	}
	// Every complete day of the cycle must be there. Only today's bucket, which
	// is still in progress, may be missing: a consumer can tell that from
	// CoveredUntil and say so, but a missing earlier day would read as a day
	// with no spend.
	if start.Add(time.Duration(len(days)) * 24 * time.Hour).Before(today) {
		return Observation{}, ErrInvalidResponse
	}
	if organizationID == "" {
		if organizationID, err = fetchOrganizationID(ctx, doer, headers()); err != nil {
			return Observation{}, err
		}
	}
	return Observation{
		Provider: client.ProviderAnthropicAPI,
		Quota: &client.Quota{Schema: 1, ObservedAt: now, CostReport: &client.CostReport{
			OrganizationID: organizationID, KeyFingerprint: fingerprint,
			StartingAt: start, EndingAt: end, Days: days,
		}},
	}, nil
}

// organizationHeader is the anthropic-organization-id response header when it
// holds a usable id, else empty. CPA returns Go's canonical header names, but
// the lookup ignores case so that no spelling of it is missed.
func organizationHeader(headers map[string][]string) string {
	for name, values := range headers {
		if strings.EqualFold(name, "anthropic-organization-id") && len(values) > 0 && organizationIDValue.MatchString(values[0]) {
			return values[0]
		}
	}
	return ""
}

// fetchOrganizationID asks /v1/organizations/me for the key's organization. It
// is the fallback for a cost report that came back without the header, so a
// normal poll never sends it.
func fetchOrganizationID(ctx context.Context, doer Doer, headers map[string][]string) (string, error) {
	response, err := doer.HTTPDo(ctx, protocol.HostHTTPRequest{Method: "GET", URL: anthropicOrganizationURL, Headers: headers})
	if err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", HTTPStatusError{StatusCode: response.StatusCode}
	}
	if len(response.Body) > maxResponseBytes {
		return "", ErrInvalidResponse
	}
	root, err := decodeObject(response.Body)
	if err != nil {
		return "", err
	}
	id, _ := root["id"].(string)
	if kind, present := root["type"]; !organizationIDValue.MatchString(id) || (present && kind != "organization") {
		return "", ErrInvalidResponse
	}
	return id, nil
}

// bucketInstant reads a bucket boundary, an RFC 3339 string, in UTC.
func bucketInstant(value any) (time.Time, bool) {
	text, ok := value.(string)
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// costAmounts copies a bucket's results verbatim: each amount and its currency,
// and nothing else Anthropic sends. A day with no cost is an empty list, never
// nil, so the snapshot says "nothing spent" rather than "not reported".
func costAmounts(value any) ([]client.CostAmount, bool) {
	results, ok := value.([]any)
	if !ok || len(results) > maxCostResults {
		return nil, false
	}
	amounts := make([]client.CostAmount, 0, len(results))
	for _, raw := range results {
		result, ok := raw.(map[string]any)
		if !ok {
			return nil, false
		}
		amount, ok1 := result["amount"].(string)
		currency, ok2 := result["currency"].(string)
		if !ok1 || !ok2 || len(amount) > 64 || !costAmountValue.MatchString(amount) || !currencyCodeValue.MatchString(currency) {
			return nil, false
		}
		amounts = append(amounts, client.CostAmount{Amount: amount, Currency: currency})
	}
	return amounts, true
}
