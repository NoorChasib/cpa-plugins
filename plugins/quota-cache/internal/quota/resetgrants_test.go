package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

// claudeGrants polls a Claude usage response carrying the given cedar_ember
// block, raw, or none when block is empty, and returns what reached the
// snapshot. The block is an ornament on the poll, so every case also checks
// that the poll itself came through whole.
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
	return o.Quota
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
// from the block may reach the snapshot except the count, the date and the hold.
func TestClaudeResetGrantsLeakNothingIntoTheSnapshot(t *testing.T) {
	q := claudeGrants(t, `{"eligible":true,"next_grant_id":"synthetic-grant-id",`+
		`"event_props":{"tier":"claude_max_20x","surface":"synthetic-surface"},`+
		`"grants":[{"id":"synthetic-grant-id","label":"synthetic-label","resets_total":1,"resets_left":1,`+
		`"ends_at":"2026-09-20T00:00:00Z","clears":["five_hour"],"percent_used":{"five_hour":100}}]}`)
	if q.ResetCredits == nil || q.ResetCredits.AvailableCount != 1 {
		t.Fatalf("reset credits = %+v", q.ResetCredits)
	}
	raw, _ := json.Marshal(q)
	for _, secret := range []string{"synthetic-grant-id", "synthetic-label", "synthetic-surface", "claude_max_20x", "cedar"} {
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
