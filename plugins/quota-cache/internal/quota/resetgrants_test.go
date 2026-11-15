package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

// claudeGrants polls a Claude usage response carrying the given cedar_ember
// block, raw, or none when block is empty, and returns what reached the
// snapshot. The block is an ornament on the poll, so every case also checks
// that the poll itself came through whole, and that whatever banked resets it
// found keep the promises their kinds make (checkResetKinds).
func claudeGrants(t *testing.T, block string) *client.Quota {
	t.Helper()
	body := `{"seven_day":{"utilization":10,"resets_at":"2026-09-09T10:00:00Z"}`
	if block != "" {
		body += `,"cedar_ember":` + block
	}
	o := fetchDetails(t, "claude", body+`}`)
	if o.Percent != 10 || !o.ObservedAt.Equal(now) {
		t.Fatalf("the usage observation was damaged by the grants block: %+v", o)
	}
	checkResetKinds(t, o.Quota.ResetCredits)
	return o.Quota
}

// checkResetKinds holds a Claude reading to what ResetCredits.Kinds promises a
// reader: one entry per kind, in ResetKindRank order, each holding at least
// one reset, the counts adding up to the total, the soonest expiry among them
// the account's, and an account-wide hold (ineligible, a running cooldown) on
// every kind alike. A Claude reading with resets and no kinds would read as an
// older Quota Cache, which the dashboard takes as "cannot tell", so it fails.
func checkResetKinds(t *testing.T, credits *client.ResetCredits) {
	t.Helper()
	if credits == nil {
		return
	}
	if len(credits.Kinds) == 0 {
		t.Fatalf("%d banked resets and no kinds: %+v", credits.AvailableCount, credits)
	}
	sum := 0
	var soonest *time.Time
	for i, k := range credits.Kinds {
		switch k.Kind {
		case client.ResetKindFull, client.ResetKindFable, client.ResetKindWeekly, client.ResetKindSession, client.ResetKindUnknown:
		default:
			t.Fatalf("kind %q is outside the vocabulary: %s", k.Kind, describeKinds(credits.Kinds))
		}
		if i > 0 && client.ResetKindRank(credits.Kinds[i-1].Kind) >= client.ResetKindRank(k.Kind) {
			t.Fatalf("kinds out of order or repeated: %s", describeKinds(credits.Kinds))
		}
		if k.AvailableCount < 1 {
			t.Fatalf("a kind with nothing to spend was listed: %s", describeKinds(credits.Kinds))
		}
		sum += k.AvailableCount
		if k.SoonestExpiry != nil && (soonest == nil || k.SoonestExpiry.Before(*soonest)) {
			soonest = k.SoonestExpiry
		}
		if (credits.Hold == client.HoldIneligible || credits.Hold == client.HoldCooldown) &&
			(k.Hold != credits.Hold || !sameInstant(k.HoldUntil, credits.HoldUntil)) {
			t.Fatalf("the account-wide hold %q did not reach kind %q: %s", credits.Hold, k.Kind, describeKinds(credits.Kinds))
		}
	}
	if sum != credits.AvailableCount {
		t.Fatalf("kinds add up to %d, the total is %d: %s", sum, credits.AvailableCount, describeKinds(credits.Kinds))
	}
	if !sameInstant(soonest, credits.SoonestExpiry) {
		t.Fatalf("the soonest kind expiry %v is not the account's %v", soonest, credits.SoonestExpiry)
	}
}

func sameInstant(a, b *time.Time) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && a.Equal(*b))
}

// describeKinds renders kinds one per entry, "kind:count ends <time> <hold>
// until <time>", so a table can state a whole breakdown in one string.
func describeKinds(kinds []client.ResetKindCredits) string {
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		s := fmt.Sprintf("%s:%d", k.Kind, k.AvailableCount)
		if k.SoonestExpiry != nil {
			s += " ends " + k.SoonestExpiry.UTC().Format(time.RFC3339)
		}
		if k.Hold != "" {
			s += " " + k.Hold
		}
		if k.HoldUntil != nil {
			s += " until " + k.HoldUntil.UTC().Format(time.RFC3339)
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "; ")
}

// The block rides on the usage request that was always made, so the request
// is the same one with the program named. skip_spend, which Claude Code sends
// beside it, would empty extra_usage, which this same response supplies.
func TestClaudeUsageRequestAsksForResetGrantsButNotToSkipSpend(t *testing.T) {
	d := &fakeDoer{response: protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"seven_day":{"utilization":10},` +
		`"extra_usage":{"is_enabled":true,"monthly_limit":5000,"used_credits":1250,"utilization":25},` +
		`"cedar_ember":{"eligible":true,"grants":[{"id":"g1","resets_left":1}]}}`)}}
	o, err := Fetch(context.Background(), d, "claude", []byte(`{"access_token":"synthetic-secret"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if d.request.URL != "https://api.anthropic.com/api/oauth/usage?cedar_ember=1" {
		t.Fatalf("usage URL = %q", d.request.URL)
	}
	if strings.Contains(d.request.URL, "skip_spend") {
		t.Fatal("skip_spend would drop the extra_usage spend this response is read for")
	}
	extra := o.Quota.Balances["extra_usage"]
	if extra.Used != "1250" || extra.Limit != "5000" || extra.UsedPercent == nil || *extra.UsedPercent != 25 || extra.Enabled == nil || !*extra.Enabled {
		t.Fatalf("extra_usage was not read beside the grants block: %+v", extra)
	}
	if o.Quota.ResetCredits == nil || o.Quota.ResetCredits.AvailableCount != 1 {
		t.Fatalf("reset credits = %+v", o.Quota.ResetCredits)
	}
}

// Anything wrong with the block costs the block and nothing else, and an
// account with nothing to spend says nothing at all.
func TestClaudeResetGrantsAbsentMalformedOrEmptyYieldNothing(t *testing.T) {
	for name, block := range map[string]string{
		"absent":              ``,
		"null":                `null`,
		"not an object":       `[{"id":"g1","resets_left":1}]`,
		"eligible missing":    `{"grants":[{"id":"g1","resets_left":1}]}`,
		"eligible not a bool": `{"eligible":"true","grants":[{"id":"g1","resets_left":1}]}`,
		"grants not a list":   `{"eligible":true,"grants":{"id":"g1","resets_left":1}}`,
		"no grants":           `{"eligible":true,"at_limit":true}`,
		"every grant malformed": `{"eligible":true,"grants":[` +
			`{"id":"UPPER","resets_left":1},` +
			`{"id":"` + strings.Repeat("a", 41) + `","resets_left":1},` +
			`{"id":"no-count"},` +
			`{"id":"negative","resets_left":-1},` +
			`{"id":"fraction","resets_left":1.5},` +
			`{"id":"text","resets_left":"1"},` +
			`{"id":"absurd","resets_left":1000},` +
			`"g1",null]}`,
		"every grant spent":   `{"eligible":true,"grants":[{"id":"g1","resets_total":2,"resets_left":0}]}`,
		"every grant expired": `{"eligible":true,"grants":[{"id":"g1","resets_left":3,"ends_at":"2026-09-04T21:59:59Z"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if credits := claudeGrants(t, block).ResetCredits; credits != nil {
				t.Fatalf("reset credits = %+v, want nil", credits)
			}
		})
	}
}

// The count is every reset still banked, the expiry is the soonest deadline
// among them that is still ahead, and nothing spent, expired, malformed or
// repeated contributes to either.
func TestClaudeResetGrantsSumTheLiveGrantsAndDateTheSoonest(t *testing.T) {
	q := claudeGrants(t, `{"eligible":true,"at_limit":true,"grants":[`+
		`{"id":"g1","resets_total":2,"resets_left":2,"ends_at":"2026-09-20T00:00:00Z"},`+
		`{"id":"g2","resets_total":1,"resets_left":1,"ends_at":"2026-09-11T00:00:00Z"},`+
		`{"id":"g3","resets_total":5,"resets_left":5,"ends_at":"2026-09-01T00:00:00Z"},`+
		`{"id":"g4","resets_total":1,"resets_left":0,"ends_at":"2026-09-06T00:00:00Z"},`+
		`{"id":"BAD","resets_left":3,"ends_at":"2026-09-05T00:00:00Z"},`+
		`{"id":"g1","resets_left":4,"ends_at":"2026-09-05T00:00:00Z"},`+
		`{"id":"g5","resets_left":1},`+
		`{"id":"g6","resets_left":1,"ends_at":"next tuesday"},`+
		`{"id":"g7","resets_left":1,"starts_at":"2026-09-10T00:00:00Z","ends_at":"2026-09-25T00:00:00Z"}]}`)
	credits := q.ResetCredits
	// g1 2 + g2 1 + g5 1 + g6 1 + g7 1. g3 has lapsed, g4 is spent, BAD fails
	// the id shape, and the second g1 is the first reported twice. g6's
	// unreadable date is no date, as Claude Code reads it; g7 has not started
	// but is held all the same.
	if credits == nil || credits.AvailableCount != 6 {
		t.Fatalf("reset credits = %+v, want 6", credits)
	}
	want := time.Date(2026, time.September, 11, 0, 0, 0, 0, time.UTC)
	if credits.SoonestExpiry == nil || !credits.SoonestExpiry.Equal(want) {
		t.Fatalf("soonest expiry = %v, want %s", credits.SoonestExpiry, want)
	}
	if q.Truncated {
		t.Fatal("a short list was marked truncated")
	}
}

// The hold is the provider's reason none can be spent now, judged by the claim
// endpoint's own rules on the grant a redemption would reach for first.
func TestClaudeResetGrantHold(t *testing.T) {
	cooldown := time.Date(2026, time.September, 5, 3, 0, 0, 0, time.UTC)
	const usable = `{"id":"g1","resets_left":1,"usable_now":true,"ends_at":"2026-09-20T00:00:00Z"}`
	cases := []struct {
		name      string
		block     string
		count     int
		hold      string
		holdUntil *time.Time
	}{
		{"usable", `{"eligible":true,"at_limit":true,"grants":[` + usable + `]}`, 1, "", nil},
		// at_limit defaults false and use_requires_limit defaults true: an
		// account that is not at a limit cannot spend one, the common case.
		{"not limited by default", `{"eligible":true,"grants":[{"id":"g1","resets_left":2,"usable_now":true}]}`, 2, client.HoldNotLimited, nil},
		{"limit not required", `{"eligible":true,"grants":[{"id":"g1","resets_left":1,"usable_now":true,"use_requires_limit":false}]}`, 1, "", nil},
		{"ineligible", `{"eligible":false,"ineligible_reason":"tier","at_limit":true,"grants":[` + usable + `]}`, 1, client.HoldIneligible, nil},
		{"ineligible outranks cooldown", `{"eligible":false,"cooldown_until":"2026-09-05T03:00:00Z","grants":[` + usable + `]}`, 1, client.HoldIneligible, nil},
		{"cooldown", `{"eligible":true,"at_limit":true,"cooldown_until":"2026-09-05T03:00:00Z","grants":[` + usable + `]}`, 1, client.HoldCooldown, &cooldown},
		{"cooldown already over", `{"eligible":true,"at_limit":true,"cooldown_until":"2026-09-04T21:00:00Z","grants":[` + usable + `]}`, 1, "", nil},
		{"every grant paused", `{"eligible":true,"at_limit":true,"grants":[` +
			`{"id":"g1","resets_left":1,"usable_now":true,"paused":true},` +
			`{"id":"g2","resets_left":1,"usable_now":true,"paused":true}]}`, 2, client.HoldPaused, nil},
		// The server's preferred grant is paused and the other is waiting on a
		// limit. The preferred grant is the one a redemption reaches for.
		{"preferred grant paused", `{"eligible":true,"next_grant_id":"g2","grants":[` +
			`{"id":"g1","resets_left":1,"usable_now":true,"ends_at":"2026-09-10T00:00:00Z"},` +
			`{"id":"g2","resets_left":1,"usable_now":true,"paused":true,"ends_at":"2026-09-20T00:00:00Z"}]}`, 2, client.HoldPaused, nil},
		// No preference: the soonest to expire explains, not response order.
		{"soonest grant explains", `{"eligible":true,"grants":[` +
			`{"id":"g1","resets_left":1,"usable_now":true,"paused":true,"ends_at":"2026-09-20T00:00:00Z"},` +
			`{"id":"g2","resets_left":1,"usable_now":true,"ends_at":"2026-09-10T00:00:00Z"}]}`, 2, client.HoldNotLimited, nil},
		// One grant that can be spent is enough, whichever the server prefers.
		{"one usable among blocked", `{"eligible":true,"at_limit":true,"next_grant_id":"g2","grants":[` +
			usable + `,{"id":"g2","resets_left":1,"usable_now":true,"paused":true}]}`, 2, "", nil},
		// Refusals outside the vocabulary are not guessed at.
		{"not started yet", `{"eligible":true,"at_limit":true,"grants":[` +
			`{"id":"g1","resets_left":1,"usable_now":true,"starts_at":"2026-09-10T00:00:00Z"}]}`, 1, "", nil},
		{"usable_now unstated", `{"eligible":true,"at_limit":true,"grants":[{"id":"g1","resets_left":1}]}`, 1, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			credits := claudeGrants(t, tc.block).ResetCredits
			if credits == nil || credits.AvailableCount != tc.count || credits.Hold != tc.hold {
				t.Fatalf("reset credits = %+v, want count %d hold %q", credits, tc.count, tc.hold)
			}
			switch {
			case tc.holdUntil == nil && credits.HoldUntil != nil:
				t.Fatalf("hold until = %s, want none", credits.HoldUntil)
			case tc.holdUntil != nil && (credits.HoldUntil == nil || !credits.HoldUntil.Equal(*tc.holdUntil)):
				t.Fatalf("hold until = %v, want %s", credits.HoldUntil, tc.holdUntil)
			}
		})
	}
}

// The grant list is bounded like every other list read from a response.
func TestClaudeResetGrantsAreBounded(t *testing.T) {
	grants := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		grants = append(grants, fmt.Sprintf(`{"id":"g%03d","resets_left":1}`, i))
	}
	q := claudeGrants(t, `{"eligible":true,"grants":[`+strings.Join(grants, ",")+`]}`)
	if q.ResetCredits == nil || q.ResetCredits.AvailableCount != maxDetails || !q.Truncated {
		t.Fatalf("reset credits = %+v truncated=%v; want %d and truncated", q.ResetCredits, q.Truncated, maxDetails)
	}
}

// Grant ids and labels identify the account and have no display use. Nothing
// from the block may reach the snapshot except the count, the date, the hold,
// the kind each is classified as, and the billing period
// (TestOnlyTheBillingPeriodLeavesEventProps). The kind is a word this code
// chose, never the grant's own clears or type copied across.
func TestClaudeResetGrantsLeakNothingIntoTheSnapshot(t *testing.T) {
	q := claudeGrants(t, `{"eligible":true,"next_grant_id":"synthetic-grant-id",`+
		`"event_props":{"tier":"claude_max_20x","surface":"synthetic-surface"},`+
		`"grants":[{"id":"synthetic-grant-id","label":"synthetic-label","resets_total":1,"resets_left":1,`+
		`"ends_at":"2026-09-20T00:00:00Z","clears":["five_hour"],"percent_used":{"five_hour":100}}]}`)
	if q.ResetCredits == nil || q.ResetCredits.AvailableCount != 1 {
		t.Fatalf("reset credits = %+v", q.ResetCredits)
	}
	if got := describeKinds(q.ResetCredits.Kinds); got != "session:1 ends 2026-09-20T00:00:00Z not_limited" {
		t.Fatalf("kinds = %s", got)
	}
	raw, _ := json.Marshal(q)
	for _, secret := range []string{"synthetic-grant-id", "synthetic-label", "synthetic-surface", "claude_max_20x", "cedar", "five_hour", "clears", "percent_used"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("the snapshot carries %q from the grants block: %s", secret, raw)
		}
	}
}

// Unlike a Codex credit, a Claude grant is dated by its own ends_at in the
// usage response that counted it, so an account holding grants is still one
// request per poll. Quota Glance's README and summary contract say so.
func TestClaudeResetGrantsAreDatedWithoutASecondRequest(t *testing.T) {
	doer := &routingDoer{bodies: map[string]string{claudeUsageURL: `{"seven_day":{"utilization":10},` +
		`"cedar_ember":{"eligible":true,"grants":[{"id":"g1","resets_left":2,"starts_at":"2026-09-01T00:00:00Z","ends_at":"2026-09-12T00:00:00Z"}]}}`}}
	o, err := Fetch(context.Background(), doer, "claude", []byte(`{"access_token":"synthetic-secret"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(doer.urls) != 1 || doer.urls[0] != claudeUsageURL {
		t.Fatalf("requests = %q; want the usage request alone", doer.urls)
	}
	want := time.Date(2026, time.September, 12, 0, 0, 0, 0, time.UTC)
	if c := o.Quota.ResetCredits; c == nil || c.AvailableCount != 2 || c.SoonestExpiry == nil || !c.SoonestExpiry.Equal(want) {
		t.Fatalf("reset credits = %+v; want 2 expiring %s", c, want)
	}
}

// Codex has no hold concept; its banked count must not gain one by accident.
func TestCodexResetCreditsNeverReportAHold(t *testing.T) {
	doer := &routingDoer{bodies: map[string]string{codexUsageURL: codexUsageWithBankedResets}}
	o, err := Fetch(context.Background(), doer, "codex", []byte(`{"access_token":"tok","account_id":"acct-1"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if c := o.Quota.ResetCredits; c == nil || c.Hold != "" || c.HoldUntil != nil {
		t.Fatalf("reset credits = %+v", c)
	}
}

// How often the subscription bills, read off the cedar_ember block's
// event_props on every poll. Only the two cadences a reader can schedule are
// kept; anything else — "unknown", a cadence this code was not built for, the
// wrong type, a missing or null event_props — stores nothing.
func TestClaudeBillingPeriodFromEventProps(t *testing.T) {
	const grant = `"grants":[{"id":"g1","resets_left":1,"usable_now":true,"ends_at":"2026-09-20T00:00:00Z"}]`
	for name, one := range map[string]struct{ block, want string }{
		"monthly": {`{"eligible":true,"at_limit":true,` + grant + `,"event_props":{"tier":"claude_max_20x","billing_period":"monthly"}}`, client.BillingMonthly},
		"annual":  {`{"eligible":true,"at_limit":true,` + grant + `,"event_props":{"billing_period":"annual"}}`, client.BillingAnnual},
		// The cadence is a fact about the subscription, not the grants: an
		// account that holds none, or is not eligible for them, still says it.
		"ineligible, no grants": {`{"eligible":false,"ineligible_reason":"tier","event_props":{"billing_period":"annual"}}`, client.BillingAnnual},
		"eligible missing":      {`{"event_props":{"billing_period":"monthly"}}`, client.BillingMonthly},
		"unknown":               {`{"eligible":true,` + grant + `,"event_props":{"billing_period":"unknown"}}`, ""},
		"another cadence":       {`{"eligible":true,` + grant + `,"event_props":{"billing_period":"weekly"}}`, ""},
		"another spelling":      {`{"eligible":true,` + grant + `,"event_props":{"billing_period":"Monthly"}}`, ""},
		"garbage":               {`{"eligible":true,` + grant + `,"event_props":{"billing_period":"monthly\nannual"}}`, ""},
		"empty":                 {`{"eligible":true,` + grant + `,"event_props":{"billing_period":""}}`, ""},
		"a number":              {`{"eligible":true,` + grant + `,"event_props":{"billing_period":12}}`, ""},
		"a boolean":             {`{"eligible":true,` + grant + `,"event_props":{"billing_period":true}}`, ""},
		"an object":             {`{"eligible":true,` + grant + `,"event_props":{"billing_period":{"value":"monthly"}}}`, ""},
		"a list":                {`{"eligible":true,` + grant + `,"event_props":{"billing_period":["monthly"]}}`, ""},
		"null":                  {`{"eligible":true,` + grant + `,"event_props":{"billing_period":null}}`, ""},
		"field absent":          {`{"eligible":true,` + grant + `,"event_props":{"tier":"claude_pro"}}`, ""},
		"event_props null":      {`{"eligible":true,` + grant + `,"event_props":null}`, ""},
		"event_props absent":    {`{"eligible":true,` + grant + `}`, ""},
		"event_props a string":  {`{"eligible":true,` + grant + `,"event_props":"monthly"}`, ""},
		"event_props a list":    {`{"eligible":true,` + grant + `,"event_props":[{"billing_period":"monthly"}]}`, ""},
		// Read from event_props only, never from a key that merely looks right.
		"beside event_props": {`{"eligible":true,` + grant + `,"billing_period":"monthly"}`, ""},
		"block absent":       {``, ""},
		"block null":         {`null`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := claudeGrants(t, one.block).BillingPeriod; got != one.want {
				t.Fatalf("billing period = %q, want %q", got, one.want)
			}
		})
	}
}

// A malformed event_props costs the cadence and nothing else: the grants
// beside it are counted and held exactly as they would be without it, and the
// poll itself (checked inside claudeGrants) comes through whole.
func TestAMalformedEventPropsLeavesTheGrantsAlone(t *testing.T) {
	const grants = `"eligible":true,"cooldown_until":"2026-09-05T03:00:00Z","grants":[` +
		`{"id":"g1","resets_left":2,"usable_now":true,"ends_at":"2026-09-20T00:00:00Z"}]`
	want := claudeGrants(t, `{`+grants+`}`).ResetCredits
	for name, props := range map[string]string{
		"a string":  `"monthly"`,
		"a list":    `[1,2,3]`,
		"a number":  `7`,
		"nested":    `{"billing_period":{"billing_period":"monthly"},"tier":[null]}`,
		"oversized": `{"billing_period":"` + strings.Repeat("m", 4096) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			q := claudeGrants(t, `{`+grants+`,"event_props":`+props+`}`)
			got := q.ResetCredits
			if got == nil || got.AvailableCount != want.AvailableCount || got.Hold != want.Hold ||
				!got.SoonestExpiry.Equal(*want.SoonestExpiry) || !got.HoldUntil.Equal(*want.HoldUntil) {
				t.Fatalf("reset credits = %+v, want %+v", got, want)
			}
			if q.BillingPeriod != "" || q.Truncated {
				t.Fatalf("billing period = %q truncated = %v", q.BillingPeriod, q.Truncated)
			}
		})
	}
}

// billing_period is the one event_props field that reaches the snapshot.
// The rest is analytics context about the account, and is left behind.
func TestOnlyTheBillingPeriodLeavesEventProps(t *testing.T) {
	q := claudeGrants(t, `{"eligible":true,"grants":[{"id":"g1","resets_left":1}],"event_props":{`+
		`"billing_period":"annual","tier":"claude_max_20x","surface":"synthetic-surface",`+
		`"org_uuid":"synthetic-org","seat_tier":"synthetic-seat","nested":{"billing_period":"monthly"}}}`)
	raw, _ := json.Marshal(q)
	if !strings.Contains(string(raw), `"billing_period":"annual"`) {
		t.Fatalf("the billing period did not reach the snapshot: %s", raw)
	}
	for _, leaked := range []string{"claude_max_20x", "synthetic-surface", "synthetic-org", "synthetic-seat", "monthly", "event_props", "tier"} {
		if strings.Contains(string(raw), leaked) {
			t.Fatalf("the snapshot carries %q from event_props: %s", leaked, raw)
		}
	}
	// And a provider without the block never gains the key at all.
	codex := fetchDetails(t, "codex", codexUsageWithBankedResets).Quota
	if raw, _ := json.Marshal(codex); strings.Contains(string(raw), "billing_period") {
		t.Fatalf("a Codex observation carries a billing period: %s", raw)
	}
}

// Grants for the kind tests below. full-b has the shape every grant had before
// kinds existed — no type, no gives_back_weekly, no clears — so it is a full
// reset too, and the full kind holds three.
const (
	kindFullA       = `{"id":"full-a","type":"full","resets_left":2,"usable_now":true,"ends_at":"2026-09-20T00:00:00Z"}`
	kindFullAPaused = `{"id":"full-a","type":"full","resets_left":2,"usable_now":true,"paused":true,"ends_at":"2026-09-20T00:00:00Z"}`
	kindFullB       = `{"id":"full-b","resets_left":1,"usable_now":true,"paused":true,"ends_at":"2026-09-10T00:00:00Z"}`
	kindFable       = `{"id":"fable-a","gives_back_weekly":true,"resets_left":1,"usable_now":true,"ends_at":"2026-09-08T00:00:00Z"}`
	kindFablePaused = `{"id":"fable-a","gives_back_weekly":true,"resets_left":1,"usable_now":true,"paused":true,"ends_at":"2026-09-08T00:00:00Z"}`
)

// An account holding full and Fable resets is two kinds, each judged as a
// spend of that kind would judge it: only its own grants, next_grant_id only
// when it names one of them. The account-wide fields keep the meaning they
// always had, over every grant.
func TestClaudeResetKindsJudgeEachKindOnItsOwn(t *testing.T) {
	cases := []struct {
		name, block, hold, kinds string
	}{
		// The full resets can be spent, the Fable one is paused, and the server
		// would reach for the Fable one first. The account has one to spend;
		// the Fable kind has none, and must not borrow the full kind's "".
		{"full spendable, Fable paused and preferred",
			`{"eligible":true,"at_limit":true,"next_grant_id":"fable-a","grants":[` + kindFullA + `,` + kindFullB + `,` + kindFablePaused + `]}`,
			"", "full:3 ends 2026-09-10T00:00:00Z; fable:1 ends 2026-09-08T00:00:00Z paused"},
		{"Fable spendable, every full paused",
			`{"eligible":true,"at_limit":true,"grants":[` + kindFullAPaused + `,` + kindFullB + `,` + kindFable + `]}`,
			"", "full:3 ends 2026-09-10T00:00:00Z paused; fable:1 ends 2026-09-08T00:00:00Z"},
		// next_grant_id names the Fable grant, so it explains the account's
		// hold. It says nothing about the full grant a full spend reaches for,
		// which is that kind's soonest, full-b, and full-b is paused.
		{"next_grant_id names another kind",
			`{"eligible":true,"next_grant_id":"fable-a","grants":[` + kindFullA + `,` + kindFullB + `,` + kindFable + `]}`,
			client.HoldNotLimited, "full:3 ends 2026-09-10T00:00:00Z paused; fable:1 ends 2026-09-08T00:00:00Z not_limited"},
		// The same grants with the server preferring full-a: now full-a, not
		// the soonest, explains the full kind.
		{"next_grant_id names this kind",
			`{"eligible":true,"next_grant_id":"full-a","grants":[` + kindFullA + `,` + kindFullB + `,` + kindFable + `]}`,
			client.HoldNotLimited, "full:3 ends 2026-09-10T00:00:00Z not_limited; fable:1 ends 2026-09-08T00:00:00Z not_limited"},
		// No choice of grant gets past an account-wide refusal, so every kind
		// carries it.
		{"ineligible holds every kind",
			`{"eligible":false,"at_limit":true,"grants":[` + kindFullA + `,` + kindFable + `]}`,
			client.HoldIneligible, "full:2 ends 2026-09-20T00:00:00Z ineligible; fable:1 ends 2026-09-08T00:00:00Z ineligible"},
		{"a cooldown holds every kind",
			`{"eligible":true,"at_limit":true,"cooldown_until":"2026-09-05T03:00:00Z","grants":[` + kindFullA + `,` + kindFable + `]}`,
			client.HoldCooldown, "full:2 ends 2026-09-20T00:00:00Z cooldown until 2026-09-05T03:00:00Z; " +
				"fable:1 ends 2026-09-08T00:00:00Z cooldown until 2026-09-05T03:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			credits := claudeGrants(t, tc.block).ResetCredits
			if credits == nil || credits.Hold != tc.hold {
				t.Fatalf("reset credits = %+v, want hold %q", credits, tc.hold)
			}
			if got := describeKinds(credits.Kinds); got != tc.kinds {
				t.Fatalf("kinds = %s\nwant    %s", got, tc.kinds)
			}
		})
	}
}

// A grant this build cannot classify is still banked: it counts toward the
// total and is listed, last, as "unknown", which no reader may offer to spend.
// An unrecognised type and a malformed gives_back_weekly or clears are each
// unknown, never a guess at full.
func TestClaudeResetKindsCountWhatTheyCannotClassify(t *testing.T) {
	q := claudeGrants(t, `{"eligible":true,"at_limit":true,"grants":[`+
		`{"id":"monthly","type":"monthly","clears":["five_hour","seven_day"],"resets_left":1,"usable_now":true,"ends_at":"2026-09-12T00:00:00Z"},`+
		`{"id":"garbled","gives_back_weekly":"yes","type":"full","resets_left":2,"paused":true,"ends_at":"2026-09-07T00:00:00Z"},`+
		`{"id":"shapeless","clears":"five_hour","resets_left":1},`+
		`{"id":"full","type":"full","resets_left":1,"ends_at":"2026-09-30T00:00:00Z"}]}`)
	credits := q.ResetCredits
	if credits == nil || credits.AvailableCount != 5 {
		t.Fatalf("reset credits = %+v, want 5", credits)
	}
	if got, want := describeKinds(credits.Kinds), "full:1 ends 2026-09-30T00:00:00Z; unknown:4 ends 2026-09-07T00:00:00Z"; got != want {
		t.Fatalf("kinds = %s\nwant    %s", got, want)
	}
}

// Grants from before kinds existed name no type, no gives_back_weekly and no
// clears (or name them empty or null), and cleared everything: an account
// holding only those is one full entry that says exactly what the
// account-wide fields say.
func TestClaudeLegacyResetGrantsAreOneFullKind(t *testing.T) {
	for name, block := range map[string]string{
		"spendable": `{"eligible":true,"at_limit":true,"grants":[` +
			`{"id":"g1","resets_left":2,"usable_now":true,"ends_at":"2026-09-20T00:00:00Z"},` +
			`{"id":"g2","resets_left":1,"clears":[]},` +
			`{"id":"g3","resets_left":1,"type":null,"gives_back_weekly":null,"clears":null,"ends_at":"2026-09-11T00:00:00Z"}]}`,
		"not limited": `{"eligible":true,"grants":[{"id":"g1","resets_left":2,"usable_now":true}]}`,
		"paused": `{"eligible":true,"next_grant_id":"g2","grants":[` +
			`{"id":"g1","resets_left":1,"usable_now":true,"ends_at":"2026-09-10T00:00:00Z"},` +
			`{"id":"g2","resets_left":1,"usable_now":true,"paused":true,"ends_at":"2026-09-20T00:00:00Z"}]}`,
		"cooldown":   `{"eligible":true,"at_limit":true,"cooldown_until":"2026-09-05T03:00:00Z","grants":[{"id":"g1","resets_left":1,"usable_now":true}]}`,
		"ineligible": `{"eligible":false,"grants":[{"id":"g1","resets_left":3,"ends_at":"2026-09-12T00:00:00Z"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			credits := claudeGrants(t, block).ResetCredits
			if credits == nil || len(credits.Kinds) != 1 {
				t.Fatalf("reset credits = %+v, want one kind", credits)
			}
			k := credits.Kinds[0]
			if k.Kind != client.ResetKindFull || k.AvailableCount != credits.AvailableCount || k.Hold != credits.Hold ||
				!sameInstant(k.SoonestExpiry, credits.SoonestExpiry) || !sameInstant(k.HoldUntil, credits.HoldUntil) {
				t.Fatalf("kind %+v does not say what the account says: %+v", k, credits)
			}
		})
	}
}

// A repeated id is one grant reported twice, whatever kind the repeat claims:
// the first reading stands, and no kind gains a reset the account does not
// have. Several grants of one kind are one entry, never one each.
func TestClaudeResetKindsCountARepeatedGrantOnce(t *testing.T) {
	q := claudeGrants(t, `{"eligible":true,"grants":[`+
		`{"id":"g1","gives_back_weekly":true,"resets_left":1,"ends_at":"2026-09-12T00:00:00Z"},`+
		`{"id":"g1","type":"full","resets_left":3,"ends_at":"2026-09-06T00:00:00Z"},`+
		`{"id":"g2","gives_back_weekly":true,"resets_left":2,"ends_at":"2026-09-09T00:00:00Z"},`+
		`{"id":"g2","gives_back_weekly":true,"resets_left":2}]}`)
	credits := q.ResetCredits
	if credits == nil || credits.AvailableCount != 3 {
		t.Fatalf("reset credits = %+v, want 3", credits)
	}
	if got, want := describeKinds(credits.Kinds), "fable:3 ends 2026-09-09T00:00:00Z not_limited"; got != want {
		t.Fatalf("kinds = %s\nwant    %s", got, want)
	}
}

// Kinds come in ResetKindRank order whatever order the response lists them
// in, and a grant with no type is classified by its clears as Claude Code
// classifies it.
func TestClaudeResetKindsAreOrderedByRank(t *testing.T) {
	q := claudeGrants(t, `{"eligible":true,"at_limit":true,"grants":[`+
		`{"id":"u","type":"monthly","resets_left":1},`+
		`{"id":"s","clears":["five_hour"],"resets_left":1},`+
		`{"id":"w","clears":["seven_day","seven_day_opus"],"resets_left":2},`+
		`{"id":"b","gives_back_weekly":true,"clears":["five_hour","seven_day"],"resets_left":1},`+
		`{"id":"f","clears":["five_hour","seven_day"],"resets_left":3}]}`)
	if got, want := describeKinds(q.ResetCredits.Kinds), "full:3; fable:1; weekly:2; session:1; unknown:1"; got != want {
		t.Fatalf("kinds = %s\nwant    %s", got, want)
	}
}

// Across many mixes of kinds, spent and lapsed grants, and lists long enough
// to be cut at the bound, each kind's count is exactly its own live resets,
// tallied here independently of the code under test. checkResetKinds, run on
// every reading, holds the sum to the total.
func TestClaudeResetKindsAddUpToTheTotal(t *testing.T) {
	shapes := []struct{ fields, kind string }{
		{`"type":"full"`, client.ResetKindFull},
		{`"gives_back_weekly":true`, client.ResetKindFable},
		{`"clears":["seven_day"]`, client.ResetKindWeekly},
		{`"type":"session"`, client.ResetKindSession},
		{`"type":"monthly"`, client.ResetKindUnknown},
		{`"clears":[]`, client.ResetKindFull},
	}
	for n := 1; n <= maxDetails+8; n++ {
		grants := make([]string, 0, n)
		want := map[string]int{}
		for i := 0; i < n; i++ {
			shape := shapes[(i*5+n)%len(shapes)]
			left := (i + n) % 4
			// Days 1 to 4 have lapsed by now, 4 September at 22:00.
			day := 1 + (i*7+n)%28
			grants = append(grants, fmt.Sprintf(`{"id":"g%03d",%s,"resets_left":%d,"ends_at":"2026-09-%02dT00:00:00Z"}`, i, shape.fields, left, day))
			if i < maxDetails && left > 0 && day > 4 {
				want[shape.kind] += left
			}
		}
		credits := claudeGrants(t, `{"eligible":true,"grants":[`+strings.Join(grants, ",")+`]}`).ResetCredits
		got := map[string]int{}
		if credits != nil {
			for _, k := range credits.Kinds {
				got[k.Kind] = k.AvailableCount
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%d grants: kinds = %v, want %v", n, got, want)
		}
	}
}

// Codex credits are all one sort: a Codex reading never gains kinds, with or
// without the inventory that dates them, so no reader can take a Codex
// account for one whose resets must be told apart.
func TestCodexResetCreditsHaveNoKinds(t *testing.T) {
	undated := fetchDetails(t, "codex", codexUsageWithBankedResets).Quota
	doer := &routingDoer{bodies: map[string]string{codexUsageURL: codexUsageWithBankedResets,
		codexResetCreditsURL: `{"credits":[{"id":"c1","status":"available","expires_at":"2026-09-20T00:00:00Z"}]}`}}
	o, err := Fetch(context.Background(), doer, "codex", []byte(`{"access_token":"tok","account_id":"acct-1"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	for name, q := range map[string]*client.Quota{"undated": undated, "dated": o.Quota} {
		if c := q.ResetCredits; c == nil || c.AvailableCount != 2 || c.Kinds != nil {
			t.Fatalf("%s: reset credits = %+v, want 2 and no kinds", name, c)
		}
		if raw, _ := json.Marshal(q); strings.Contains(string(raw), `"kinds"`) {
			t.Fatalf("%s: a Codex observation carries kinds: %s", name, raw)
		}
	}
}

// Kinds reach a reader through the snapshot file as they left the poll, under
// the documented names. A snapshot written before they existed reads back with
// none, which a reader must take as "not read", never as "all full".
func TestClaudeResetKindsSurviveTheSnapshot(t *testing.T) {
	q := claudeGrants(t, `{"eligible":true,"at_limit":true,"cooldown_until":"2026-09-05T03:00:00Z","grants":[`+
		kindFullA+`,`+kindFable+`,{"id":"session-a","type":"session","resets_left":1}]}`)
	raw, err := json.Marshal(client.Snapshot{Schema: 1, WrittenAt: now, ProviderCooldown: map[string]time.Time{},
		Entries: map[string]client.Entry{client.Key("claude", "a1"): {Provider: "claude", AuthIndex: "a1", ObservedAt: now, Quota: q}}})
	if err != nil {
		t.Fatal(err)
	}
	const wire = `"kinds":[` +
		`{"kind":"full","available_count":2,"soonest_expiry":"2026-09-20T00:00:00Z","hold":"cooldown","hold_until":"2026-09-05T03:00:00Z"},` +
		`{"kind":"fable","available_count":1,"soonest_expiry":"2026-09-08T00:00:00Z","hold":"cooldown","hold_until":"2026-09-05T03:00:00Z"},` +
		`{"kind":"session","available_count":1,"hold":"cooldown","hold_until":"2026-09-05T03:00:00Z"}]`
	if !strings.Contains(string(raw), wire) {
		t.Fatalf("snapshot = %s\nwant it to carry %s", raw, wire)
	}
	path := filepath.Join(t.TempDir(), "snapshot.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	read, err := client.ReadQuota(path, "claude", "a1", now, time.Hour)
	if err != nil || read.ResetCredits == nil {
		t.Fatalf("read back %+v, %v", read.ResetCredits, err)
	}
	if got, want := describeKinds(read.ResetCredits.Kinds), describeKinds(q.ResetCredits.Kinds); got != want {
		t.Fatalf("kinds read back = %s\nwant          %s", got, want)
	}
	var older client.ResetCredits
	if err := json.Unmarshal([]byte(`{"available_count":2,"soonest_expiry":"2026-09-20T00:00:00Z","hold":"not_limited"}`), &older); err != nil {
		t.Fatal(err)
	}
	if older.AvailableCount != 2 || older.Kinds != nil {
		t.Fatalf("an older snapshot's reset credits read back as %+v, want no kinds", older)
	}
}
