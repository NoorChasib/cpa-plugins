package quota

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

func TestOpenRouterKeepsBothTotalsVerbatimInDollars(t *testing.T) {
	body := `{"data":{"total_credits":100.5,"total_usage":25.123456789}}`
	doer := &fakeDoer{response: protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(body)}}
	observation, err := FetchOpenRouter(context.Background(), doer, " sk-or-v1-management ", now)
	if err != nil {
		t.Fatal(err)
	}
	if doer.request.Method != "GET" || doer.request.URL != openRouterCreditsURL ||
		doer.request.Headers["Authorization"][0] != "Bearer sk-or-v1-management" {
		t.Fatalf("request=%+v", doer.request)
	}
	if observation.Provider != OpenRouterProvider || observation.Quota == nil || !observation.Quota.ObservedAt.Equal(now) {
		t.Fatalf("observation=%+v", observation)
	}
	balance, ok := observation.Quota.Balances["credits"]
	if !ok || balance.Unit != "usd" || balance.Limit != "100.5" || balance.Used != "25.123456789" {
		t.Fatalf("balance=%+v", balance)
	}
	// The subtraction belongs to whoever displays a balance, exactly as for
	// every other provider's amounts.
	if balance.Remaining != "" {
		t.Fatalf("remaining was inferred: %q", balance.Remaining)
	}
}

// OpenRouter has no weekly window. The compatibility fields must not describe
// one, or Account Health and Reset Priority would read an account at 0% weekly
// use; the nested observation carries the date instead.
func TestOpenRouterLeavesTheWeeklyCompatibilityFieldsEmpty(t *testing.T) {
	doer := &fakeDoer{response: protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"data":{"total_credits":10,"total_usage":0}}`)}}
	observation, err := FetchOpenRouter(context.Background(), doer, "key", now)
	if err != nil {
		t.Fatal(err)
	}
	if !observation.ObservedAt.IsZero() || !observation.ResetAt.IsZero() || observation.Percent != 0 || len(observation.Windows) != 0 {
		t.Fatalf("observation claims a weekly window: %+v", observation)
	}
}

// A negative balance is a real OpenRouter state, and a zero total is an
// explicit value rather than a missing one.
func TestOpenRouterKeepsZeroAndOverspentTotals(t *testing.T) {
	doer := &fakeDoer{response: protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"data":{"total_credits":0,"total_usage":1.25}}`)}}
	observation, err := FetchOpenRouter(context.Background(), doer, "key", now)
	if err != nil {
		t.Fatal(err)
	}
	if balance := observation.Quota.Balances["credits"]; balance.Limit != "0" || balance.Used != "1.25" {
		t.Fatalf("balance=%+v", balance)
	}
}

func TestOpenRouterErrorsAreStaticAndNeverEchoTheKey(t *testing.T) {
	secret := "sk-or-v1-SECRET-MANAGEMENT-KEY"
	cases := []struct {
		name     string
		response protocol.HostHTTPResponse
		err      error
		want     error
	}{
		{"inference key refused", protocol.HostHTTPResponse{StatusCode: 403, Body: []byte(`{"error":{"code":403,"message":"Only management keys can perform this operation ` + secret + `"}}`)}, nil, HTTPStatusError{StatusCode: 403}},
		{"missing usage", protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"data":{"total_credits":10}}`)}, nil, ErrInvalidResponse},
		{"missing data", protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"error":"` + secret + `"}`)}, nil, ErrInvalidResponse},
		{"non numeric", protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"data":{"total_credits":"lots","total_usage":1}}`)}, nil, ErrInvalidResponse},
		{"invalid json", protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`not json ` + secret)}, nil, ErrInvalidResponse},
		{"transport", protocol.HostHTTPResponse{}, errors.New("dial failed"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &fakeDoer{response: tc.response, err: tc.err}
			_, err := FetchOpenRouter(context.Background(), doer, secret, now)
			if err == nil {
				t.Fatal("expected error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) && err.Error() != tc.want.Error() {
				t.Fatalf("err=%v want %v", err, tc.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaked the key: %v", err)
			}
		})
	}
}

func TestOpenRouterWithoutAKeySendsNothing(t *testing.T) {
	doer := &fakeDoer{}
	if _, err := FetchOpenRouter(context.Background(), doer, "  ", now); !errors.Is(err, ErrNoAccessToken) {
		t.Fatalf("err=%v", err)
	}
	if doer.request.URL != "" {
		t.Fatal("a request was made without a key")
	}
}

// The snapshot is written to disk and served on the status route. Neither the
// key nor anything else from the response beyond the two totals may reach it.
func TestOpenRouterObservationCarriesNothingButTheTotals(t *testing.T) {
	secret := "sk-or-v1-SECRET-MANAGEMENT-KEY"
	body := `{"data":{"total_credits":5,"total_usage":1,"label":"` + secret + `","email":"owner@example.com"}}`
	doer := &fakeDoer{response: protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(body)}}
	observation, err := FetchOpenRouter(context.Background(), doer, secret, now)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(observation.Quota)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "owner@example.com") {
		t.Fatalf("observation carries response fields it should not: %s", raw)
	}
}
