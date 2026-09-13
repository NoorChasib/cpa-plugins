package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
	"strings"
	"testing"
	"time"
)

func fetchDetails(t *testing.T, provider, body string) Observation {
	t.Helper()
	d := &fakeDoer{response: protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(body)}}
	o, err := Fetch(context.Background(), d, provider, []byte(`{"access_token":"synthetic-secret","account_id":"synthetic-account"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if o.Quota == nil || !o.Quota.ObservedAt.Equal(now) || o.Quota.Schema != 1 {
		t.Fatal("missing extended observation")
	}
	return o
}
func TestClaudeExtendedWindowsAndOptionalValues(t *testing.T) {
	o := fetchDetails(t, "claude", `{"five_hour":{"utilization":0,"resets_at":"2026-09-04T23:00:00Z"},"seven_day":{"utilization":14},"seven_day_sonnet":{"utilization":125},"seven_day_opus":null,"extra_usage":{"is_enabled":false,"monthly_limit":0,"used_credits":0,"utilization":null},"access_token":"synthetic-secret"}`)
	q := o.Quota
	if o.Percent != 14 || !o.ObservedAt.Equal(now) || len(q.Windows) != 3 || *q.Windows["five_hour"].UsedPercent != 0 || *q.Windows["seven_day_sonnet"].UsedPercent != 125 {
		t.Fatalf("windows=%+v", q.Windows)
	}
	b := q.Balances["extra_usage"]
	if b.Enabled == nil || *b.Enabled || b.Used != "0" || b.Limit != "0" || b.UsedPercent != nil {
		t.Fatalf("balance=%+v", b)
	}
	raw, _ := json.Marshal(q)
	if strings.Contains(string(raw), "synthetic-secret") {
		t.Fatal("unselected credential field persisted")
	}
}
func TestCodexExtendedGroupsCreditsAndNoFalseWeeklyObservation(t *testing.T) {
	o := fetchDetails(t, "codex", `{"plan_type":"pro","rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_after_seconds":60}},"code_review_rate_limit":{"allowed":true,"primary_window":{"used_percent":4,"limit_window_seconds":604800}},"additional_rate_limits":[{"limit_name":"spark","rate_limit":{"secondary_window":{"used_percent":12,"limit_window_seconds":604800}}}],"credits":{"has_credits":false,"unlimited":false,"balance":"0.00000000000000000001"}}`)
	q := o.Quota
	if !o.ObservedAt.IsZero() || o.Percent != 0 || !o.ResetAt.IsZero() {
		t.Fatal("short-only account invented weekly data")
	}
	if q.Plan != "pro" || len(q.Windows) != 3 || *q.Limits["regular"].Allowed || !*q.Limits["regular"].Reached || q.Balances["credits"].Remaining != "0.00000000000000000001" || *q.Balances["credits"].HasCredits {
		t.Fatalf("quota=%+v", q)
	}
	if !q.Windows["regular/primary"].ResetsAt.Equal(now.Add(time.Minute)) {
		t.Fatal("relative reset not anchored to observation")
	}
	o = fetchDetails(t, "codex", `{"additionalRateLimits":{"spark":{"allowed":true,"primary":{"usedPercent":2,"windowMinutes":300}}}}`)
	if len(o.Quota.Windows) != 1 || *o.Quota.Windows["additional/spark/primary"].DurationSeconds != 18000 {
		t.Fatal("object/camelCase additional group missing")
	}
}
func TestGrokBalancesUnitsAndProductWindows(t *testing.T) {
	o := fetchDetails(t, "xai", `{"subscriptionTier":"SuperGrok Heavy","onDemandEnabled":false,"config":{"creditUsagePercent":28,"isUnifiedBillingUser":true,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_MONTHLY","start":"2026-09-01T00:00:00Z","end":"2026-10-01T00:00:00Z"},"monthlyLimit":{"val":"9007199254740993"},"used":{"val":12},"prepaidBalance":{},"onDemandUsed":{"val":0},"productUsage":[{"product":"BUILD","usagePercent":22}]}}`)
	q := o.Quota
	if len(q.Windows) != 2 || q.Windows["shared"].Period != "USAGE_PERIOD_TYPE_MONTHLY" || q.Windows["shared"].StartsAt == nil || q.Plan != "SuperGrok Heavy" || !*q.UnifiedBilling {
		t.Fatalf("quota=%+v", q)
	}
	if q.Balances["included"].Limit != "9007199254740993" || q.Balances["included"].Unit != "usd_cents" || q.Balances["prepaid"].Remaining != "0" || q.Balances["on_demand"].Limit != "" || *q.Balances["on_demand"].Enabled {
		t.Fatalf("balances=%+v", q.Balances)
	}
}
func TestExtendedMetadataIsBoundedAndMalformedOptionalValuesAreOmitted(t *testing.T) {
	items := []string{}
	for i := 0; i < 100; i++ {
		items = append(items, fmt.Sprintf(`{"limit_name":"limit-%03d","rate_limit":{"allowed":false,"primary_window":{"used_percent":2}}}`, i))
	}
	o := fetchDetails(t, "codex", `{"rate_limit":{"primary_window":{"used_percent":3,"limit_window_seconds":604800}},"plan_type":"bad\nvalue","credits":{"balance":"NaN","has_credits":"false"},"additional_rate_limits":`+"["+strings.Join(items, ",")+`]}`)
	if !o.Quota.Truncated || len(o.Quota.Windows) > maxDetails || len(o.Quota.Limits) > maxDetails || o.Quota.Plan != "" || len(o.Quota.Balances) != 0 {
		t.Fatal("bounds or optional field validation failed")
	}
	raw, _ := json.Marshal(o.Quota)
	if strings.Contains(string(raw), "bad") || strings.Contains(string(raw), "NaN") {
		t.Fatal("malformed metadata retained")
	}
}

func TestCodexSpendControlsAndFeatureIdentity(t *testing.T) {
	o := fetchDetails(t, "codex", `{"metered_limit_name":"spark","rate_limit_reached_type":{"type":"workspace_member_usage_limit_reached"},"spend_control":{"reached":true,"individual_limit":{"source":"workspace","limit":"50.00","used":"50.25","remaining":"-0.25","used_percent":101,"remaining_percent":0,"reset_after_seconds":2592000}},"additional_rate_limits":[{"limit_name":"spark","metered_feature":"codex_spark","rate_limit":{"primary_window":{"used_percent":9}}}]}`)
	q := o.Quota
	b := q.Balances["spend_control"]
	if q.ActiveLimit != "spark" || q.LimitReachedReason != "workspace_member_usage_limit_reached" || !*q.Limits["spend_control"].Reached || q.Limits["additional/spark"].MeteredFeature != "codex_spark" || b.Remaining != "-0.25" || b.Limit != "50.00" || b.Source != "workspace" || !b.ResetsAt.Equal(now.Add(30*24*time.Hour)) {
		t.Fatalf("quota=%+v balance=%+v", q, b)
	}
}
