package aggregate

import (
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// oneCredential builds a document for a single rostered credential whose
// snapshot entry carries quota.
func oneCredential(t *testing.T, identity Identity, entry qc.Entry, redeemable bool) Credential {
	t.Helper()
	now := at(t, 0)
	entry.Provider, entry.AuthIndex = identity.Provider, identity.AuthIndex
	if entry.ObservedAt.IsZero() {
		entry.ObservedAt = now.Add(-2 * time.Minute)
	}
	if entry.Quota != nil && entry.Quota.ObservedAt.IsZero() {
		entry.Quota.ObservedAt = entry.ObservedAt
	}
	doc := Build(Input{
		Snapshot: qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{
			qc.Key(identity.Provider, identity.AuthIndex): entry,
		}},
		Identities: []Identity{identity},
		StaleAfter: 45 * time.Minute,
		Redeemable: redeemable,
	}, now)
	if len(doc.Credentials) != 1 {
		t.Fatalf("credentials = %+v", doc.Credentials)
	}
	return doc.Credentials[0]
}

// banked is count resets, all of them full resets as quota-cache reports an
// account whose grants predate kinds. Codex ignores the breakdown.
func banked(count int) *qc.Quota {
	return &qc.Quota{Schema: 1, ResetCredits: &qc.ResetCredits{
		AvailableCount: count,
		Kinds:          []qc.ResetKindCredits{{Kind: qc.ResetKindFull, AvailableCount: count}},
	}}
}

// offered reports whether a client may put a button anywhere on these
// credits: on the count itself for Codex, and on a kind for Claude.
func offered(credits *ResetCredits) bool {
	if credits == nil {
		return false
	}
	if credits.Redeemable {
		return true
	}
	for _, kind := range credits.Kinds {
		if kind.Redeemable {
			return true
		}
	}
	return false
}

// Every account holding a banked reset gets its own button, so the operator
// chooses which account to spend on — including one CPA has parked in a
// cooldown. Unavailable is a statement about routing: CPA still hands out the
// token, quota-cache still polls with it, and a cooldown is exactly when a
// reset is wanted. Hiding the button there removed it from the one account
// that needed it.
func TestACredentialInCooldownKeepsItsButton(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		identity := Identity{AuthIndex: provider + "-cooling@example.com.json", Provider: provider, Unavailable: true}
		got := oneCredential(t, identity, qc.Entry{Quota: banked(2)}, true)
		if got.Status != StatusUnavailable {
			t.Fatalf("%s: status = %q; the fixture should be in cooldown", provider, got.Status)
		}
		if !offered(got.ResetCredits) {
			t.Fatalf("%s: a credential in cooldown lost its button: %+v", provider, got.ResetCredits)
		}
	}
}

// The things that do take the button away, and only those: the operator
// switched the credential off, switched redemption off, or the provider is one
// this plugin cannot redeem on. The count still shows in every case.
func TestTheButtonGoesOnlyWhereItCannotWork(t *testing.T) {
	for name, one := range map[string]struct {
		identity   Identity
		redeemable bool
		want       bool
	}{
		"codex":                          {Identity{AuthIndex: "codex-a@example.com.json", Provider: "codex"}, true, true},
		"claude":                         {Identity{AuthIndex: "claude-a@northwind.dev.json", Provider: "claude"}, true, true},
		"disabled codex":                 {Identity{AuthIndex: "codex-a@example.com.json", Provider: "codex", Disabled: true}, true, false},
		"disabled claude":                {Identity{AuthIndex: "claude-a@northwind.dev.json", Provider: "claude", Disabled: true}, true, false},
		"disabled and parked":            {Identity{AuthIndex: "claude-a@northwind.dev.json", Provider: "claude", Disabled: true, Unavailable: true}, true, false},
		"redemption switched off":        {Identity{AuthIndex: "claude-a@northwind.dev.json", Provider: "claude"}, false, false},
		"a provider with no redeem path": {Identity{AuthIndex: "xai-a@example.com.json", Provider: "xai"}, true, false},
	} {
		got := oneCredential(t, one.identity, qc.Entry{Quota: banked(1)}, one.redeemable)
		if got.ResetCredits == nil || got.ResetCredits.AvailableCount != 1 {
			t.Fatalf("%s: the count went with the button: %+v", name, got.ResetCredits)
		}
		if offered(got.ResetCredits) != one.want {
			t.Fatalf("%s: offered = %v, want %v: %+v", name, offered(got.ResetCredits), one.want, got.ResetCredits)
		}
	}
}

// A Claude press names the kind it spends, so the count itself never carries
// the button there: each kind does. Codex keeps its one button on the count
// and has no kinds, whatever the snapshot says.
func TestClaudesButtonsAreOnItsKindsAndCodexsOnItsCount(t *testing.T) {
	claude := oneCredential(t, Identity{AuthIndex: "claude-a@northwind.dev.json", Provider: "claude"}, qc.Entry{Quota: banked(2)}, true).ResetCredits
	if claude.Redeemable || len(claude.Kinds) != 1 || !claude.Kinds[0].Redeemable || claude.Kinds[0].Kind != qc.ResetKindFull {
		t.Fatalf("claude = %+v", claude)
	}
	codex := oneCredential(t, Identity{AuthIndex: "codex-a@example.com.json", Provider: "codex"}, qc.Entry{Quota: banked(2)}, true).ResetCredits
	if !codex.Redeemable || codex.Kinds == nil || len(codex.Kinds) != 0 {
		t.Fatalf("codex = %+v, want its button on the count and an empty, non-nil kinds", codex)
	}
}

// Quota Cache's breakdown by kind, carried onto the credential: dropped where
// empty, in rank order whatever order it arrived in, each dated and held as the
// account is, and each offered only where a press could name it.
func TestClaudeResetKindsAreCarriedKindByKind(t *testing.T) {
	now := at(t, 0)
	soon, later, past := now.Add(26*time.Hour), now.Add(9*24*time.Hour), now.Add(-time.Hour)
	cooling, cooled := now.Add(90*time.Minute), now.Add(-time.Minute)
	quota := &qc.Quota{Schema: 1, ResetCredits: &qc.ResetCredits{
		AvailableCount: 5, SoonestExpiry: &soon,
		Kinds: []qc.ResetKindCredits{
			{Kind: qc.ResetKindUnknown, AvailableCount: 1, SoonestExpiry: &later},
			{Kind: qc.ResetKindSession, AvailableCount: 0},
			{Kind: qc.ResetKindFable, AvailableCount: 2, SoonestExpiry: &soon, Hold: qc.HoldCooldown, HoldUntil: &cooling},
			{Kind: "monthly", AvailableCount: 1},
			{Kind: qc.ResetKindFull, AvailableCount: 1, SoonestExpiry: &past, Hold: qc.HoldCooldown, HoldUntil: &cooled},
		},
	}}
	identity := Identity{AuthIndex: "claude-a@northwind.dev.json", Provider: "claude"}
	got := oneCredential(t, identity, qc.Entry{Quota: quota}, true).ResetCredits
	if got.AvailableCount != 5 || got.Redeemable || got.ExpiresAtEpoch == nil || *got.ExpiresAtEpoch != soon.Unix() {
		t.Fatalf("account-wide = %+v", got)
	}
	want := []ResetKind{
		// Its expiry and its cooldown both already past: dropped, as the
		// account's would be.
		{Kind: qc.ResetKindFull, AvailableCount: 1, Redeemable: true},
		{Kind: qc.ResetKindFable, AvailableCount: 2, Redeemable: true, Hold: "cooldown",
			ExpiresAtEpoch: ptr(soon.Unix()), ExpiresInSeconds: ptr(int64(26 * 3600)), HoldUntilEpoch: ptr(cooling.Unix())},
		// Counted, never offered: neither is a kind a press may name.
		{Kind: qc.ResetKindUnknown, AvailableCount: 1, ExpiresAtEpoch: ptr(later.Unix()), ExpiresInSeconds: ptr(int64(9 * 24 * 3600))},
		{Kind: "monthly", AvailableCount: 1},
	}
	if len(got.Kinds) != len(want) {
		t.Fatalf("kinds = %+v, want %d entries", got.Kinds, len(want))
	}
	for i := range want {
		if !sameKind(got.Kinds[i], want[i]) {
			t.Fatalf("kinds[%d] = %s, want %s", i, kindString(got.Kinds[i]), kindString(want[i]))
		}
	}

	// The credential's own condition takes every kind's button with it, and
	// the counts stay.
	for name, one := range map[string]struct {
		identity   Identity
		redeemable bool
	}{
		"disabled":                {Identity{AuthIndex: identity.AuthIndex, Provider: "claude", Disabled: true}, true},
		"redemption switched off": {identity, false},
	} {
		got := oneCredential(t, one.identity, qc.Entry{Quota: quota}, one.redeemable).ResetCredits
		if len(got.Kinds) != 4 {
			t.Fatalf("%s: kinds = %+v", name, got.Kinds)
		}
		for _, kind := range got.Kinds {
			if kind.Redeemable {
				t.Fatalf("%s: %s is offered", name, kind.Kind)
			}
		}
	}
}

// A Quota Cache older than the breakdown gives a Claude count with no kinds.
// The count still shows, with no button anywhere, and kinds is an empty array
// rather than null so a client has one shape to read.
func TestAClaudeCountWithoutKindsOffersNoButton(t *testing.T) {
	quota := &qc.Quota{Schema: 1, ResetCredits: &qc.ResetCredits{AvailableCount: 2, Hold: qc.HoldNotLimited}}
	got := oneCredential(t, Identity{AuthIndex: "claude-a@northwind.dev.json", Provider: "claude"}, qc.Entry{Quota: quota}, true)
	if got.ResetCredits == nil || got.ResetCredits.AvailableCount != 2 || got.ResetCredits.Hold != "notLimited" || offered(got.ResetCredits) {
		t.Fatalf("resetCredits = %+v", got.ResetCredits)
	}
	for _, credential := range []Credential{got, oneCredential(t, Identity{AuthIndex: "codex-a@example.com.json", Provider: "codex"}, qc.Entry{Quota: quota}, true)} {
		raw, err := json.Marshal(credential.ResetCredits)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"kinds":[]`) {
			t.Fatalf("%s: %s, want kinds as an empty array", credential.Provider, raw)
		}
	}
}

func sameKind(a, b ResetKind) bool {
	same := func(x, y *int64) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return a.Kind == b.Kind && a.AvailableCount == b.AvailableCount && a.Redeemable == b.Redeemable && a.Hold == b.Hold &&
		same(a.ExpiresAtEpoch, b.ExpiresAtEpoch) && same(a.ExpiresInSeconds, b.ExpiresInSeconds) && same(a.HoldUntilEpoch, b.HoldUntilEpoch)
}

func kindString(k ResetKind) string {
	raw, _ := json.Marshal(k)
	return string(raw)
}

// quota-cache's hold arrives in this document's vocabulary. A cooldown already
// over is dropped with its instant, as an expired expiry is; a hold this build
// does not know is dropped rather than shown raw.
func TestHoldsAreTranslatedAndExpire(t *testing.T) {
	now := at(t, 0)
	later, earlier := now.Add(90*time.Minute), now.Add(-time.Minute)
	for name, one := range map[string]struct {
		hold      string
		until     *time.Time
		want      string
		wantUntil *int64
	}{
		"not limited":        {hold: "not_limited", want: "notLimited"},
		"not limited, dated": {hold: "not_limited", until: &later, want: "notLimited"},
		"cooldown":           {hold: "cooldown", until: &later, want: "cooldown", wantUntil: ptr(later.Unix())},
		"cooldown, undated":  {hold: "cooldown", want: "cooldown"},
		"cooldown over":      {hold: "cooldown", until: &earlier, want: ""},
		"cooldown ends now":  {hold: "cooldown", until: &now, want: ""},
		"paused":             {hold: "paused", want: "paused"},
		"ineligible":         {hold: "ineligible", want: "ineligible"},
		"none":               {hold: "", want: ""},
		"unknown":            {hold: "on_vacation", until: &later, want: ""},
	} {
		quota := banked(1)
		quota.ResetCredits.Hold, quota.ResetCredits.HoldUntil = one.hold, one.until
		quota.ResetCredits.Kinds[0].Hold, quota.ResetCredits.Kinds[0].HoldUntil = one.hold, one.until
		got := oneCredential(t, Identity{AuthIndex: "claude-a@northwind.dev.json", Provider: "claude"}, qc.Entry{Quota: quota}, true).ResetCredits
		// The account's hold and its kind's are read by the same rule.
		for where, hold := range map[string]struct {
			hold  string
			until *int64
		}{"account": {got.Hold, got.HoldUntilEpoch}, "kind": {got.Kinds[0].Hold, got.Kinds[0].HoldUntilEpoch}} {
			if hold.hold != one.want {
				t.Fatalf("%s: %s hold = %q, want %q", name, where, hold.hold, one.want)
			}
			if (hold.until == nil) != (one.wantUntil == nil) || (hold.until != nil && *hold.until != *one.wantUntil) {
				t.Fatalf("%s: %s holdUntilEpoch = %v, want %v", name, where, hold.until, one.wantUntil)
			}
		}
		// A hold is a hint beside the button, never a reason to remove it.
		if !got.Kinds[0].Redeemable {
			t.Fatalf("%s: a hold took the button away", name)
		}
	}
}

func ptr(v int64) *int64 { return &v }

// The renewal instant is carried while it is ahead of us and dropped once it
// is not: a renewal behind us is a poll that has not yet seen the next one.
func TestRenewalIsCarriedOnlyWhileItIsAhead(t *testing.T) {
	now := at(t, 0)
	ahead, behind := now.Add(24*time.Hour), now.Add(-time.Hour)
	for name, one := range map[string]struct {
		renewal *time.Time
		want    *int64
	}{
		"ahead":   {&ahead, ptr(ahead.Unix())},
		"behind":  {&behind, nil},
		"now":     {&now, nil},
		"unknown": {nil, nil},
	} {
		got := oneCredential(t, Identity{AuthIndex: "codex-a@example.com.json", Provider: "codex"}, qc.Entry{RenewalAt: one.renewal}, true)
		if (got.RenewalAtEpoch == nil) != (one.want == nil) || (got.RenewalAtEpoch != nil && *got.RenewalAtEpoch != *one.want) {
			t.Fatalf("%s: renewalAtEpoch = %v, want %v", name, got.RenewalAtEpoch, one.want)
		}
	}
	// A credential quota-cache has never heard of has no renewal either.
	doc := Build(Input{
		Snapshot:   qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{}},
		Identities: []Identity{{AuthIndex: "codex-a@example.com.json", Provider: "codex"}},
	}, now)
	if doc.Credentials[0].RenewalAtEpoch != nil || doc.Credentials[0].Credits != nil {
		t.Fatalf("an unpolled credential carries %+v", doc.Credentials[0])
	}
}

// Each provider's spendable balance, and only that one, written out for direct
// rendering.
func TestCreditsAreReadFromEachProvidersOwnBalance(t *testing.T) {
	yes, no := true, false
	for name, one := range map[string]struct {
		provider string
		balances map[string]qc.Balance
		want     *Credits
	}{
		"codex credits": {"codex", map[string]qc.Balance{"credits": {Unit: "credits", Remaining: "57706.149", HasCredits: &yes, Unlimited: &no}},
			&Credits{Display: "57,706.15", Amount: "57706.149", Unit: "credits"}},
		"codex none left": {"codex", map[string]qc.Balance{"credits": {Unit: "credits", Remaining: "0", HasCredits: &no}},
			&Credits{Display: "0", Amount: "0", Unit: "credits"}},
		"codex unlimited": {"codex", map[string]qc.Balance{"credits": {Unit: "credits", Remaining: "120", Unlimited: &yes}},
			&Credits{Display: "Unlimited", Unlimited: true, Unit: "credits"}},
		"codex exponent": {"codex", map[string]qc.Balance{"credits": {Unit: "credits", Remaining: "1.5e3"}},
			&Credits{Display: "1,500", Amount: "1500", Unit: "credits"}},
		"codex no amount":    {"codex", map[string]qc.Balance{"credits": {Unit: "credits", HasCredits: &yes}}, nil},
		"codex wrong unit":   {"codex", map[string]qc.Balance{"credits": {Unit: "usd", Remaining: "5"}}, nil},
		"codex not reported": {"codex", map[string]qc.Balance{"spend_control": {Unit: "usd", Remaining: "5"}}, nil},
		"grok prepaid": {"xai", map[string]qc.Balance{"prepaid": {Unit: "usd_cents", Remaining: "1240"}},
			&Credits{Display: "$12.40", Amount: "12.40", Unit: "usd"}},
		"grok large": {"xai", map[string]qc.Balance{"prepaid": {Unit: "usd_cents", Remaining: "123456789"}},
			&Credits{Display: "$1,234,567.89", Amount: "1234567.89", Unit: "usd"}},
		"grok sub-cent": {"xai", map[string]qc.Balance{"prepaid": {Unit: "usd_cents", Remaining: "1240.5"}},
			&Credits{Display: "$12.41", Amount: "12.405", Unit: "usd"}},
		"grok empty": {"xai", map[string]qc.Balance{"prepaid": {Unit: "usd_cents", Remaining: "0"}},
			&Credits{Display: "$0.00", Amount: "0.00", Unit: "usd"}},
		// Grok's allowance and on-demand cap are limits, not money held.
		"grok allowance only": {"xai", map[string]qc.Balance{"included": {Unit: "usd_cents", Used: "500", Limit: "3000"}}, nil},
		"grok wrong unit":     {"xai", map[string]qc.Balance{"prepaid": {Unit: "usd", Remaining: "12"}}, nil},
		"grok not a number":   {"xai", map[string]qc.Balance{"prepaid": {Unit: "usd_cents", Remaining: "lots"}}, nil},
		"claude":              {"claude", map[string]qc.Balance{"credits": {Unit: "credits", Remaining: "10"}}, nil},
	} {
		identity := Identity{AuthIndex: one.provider + "-a@example.com.json", Provider: one.provider}
		got := oneCredential(t, identity, qc.Entry{Quota: &qc.Quota{Schema: 1, Balances: one.balances}}, true).Credits
		if (got == nil) != (one.want == nil) || (got != nil && *got != *one.want) {
			t.Fatalf("%s: credits = %+v, want %+v", name, got, one.want)
		}
	}
	// No observation at all is no balance at all.
	if got := oneCredential(t, Identity{AuthIndex: "codex-a@example.com.json", Provider: "codex"}, qc.Entry{}, true).Credits; got != nil {
		t.Fatalf("credits without a quota = %+v", got)
	}
}

// Credits are counted, not priced: grouped, at most two places, trailing zeros
// dropped, and never "-0".
func TestCreditsTextMatchesTheConsole(t *testing.T) {
	for raw, want := range map[string]string{
		"57706.149":   "57,706.15",
		"57706.15":    "57,706.15",
		"0":           "0",
		"100":         "100",
		"120.50":      "120.5",
		"999.999":     "1,000",
		"1234567.005": "1,234,567.01",
		"0.004":       "0",
		"-0.004":      "0",
		"-12.5":       "-12.5",
		"-1234.5":     "-1,234.5",
	} {
		amount, ok := new(big.Rat).SetString(raw)
		if !ok {
			t.Fatalf("fixture %q", raw)
		}
		if got := creditsTextOf(amount); got != want {
			t.Errorf("creditsTextOf(%s) = %q, want %q", raw, got, want)
		}
	}
}

// The two committed contracts are what the web app develops against, so each
// shape the new fields can take has to appear in one of them, or the app is
// left to invent it.
func TestGoldenContractsCoverTheCredentialExtras(t *testing.T) {
	byID := map[string]Credential{}
	for _, doc := range []Document{buildFixture(t), buildDegraded(t)} {
		for _, c := range doc.Credentials {
			byID[c.ID] = c
		}
	}
	credential := func(id string) Credential {
		c, ok := byID[id]
		if !ok {
			t.Fatalf("no credential %s in either contract", id)
		}
		return c
	}

	// kinds is a Claude credential's kinds as "kind:count:redeemable:hold".
	kinds := func(c Credential) []string {
		out := []string{}
		for _, kind := range c.ResetCredits.Kinds {
			out = append(out, fmt.Sprintf("%s:%d:%v:%s", kind.Kind, kind.AvailableCount, kind.Redeemable, kind.Hold))
		}
		return out
	}
	// Claude holding resets of two kinds, each with its own button and the
	// Fable one with its own hold, and none on the count; and an account whose
	// grants predate kinds, which are all full resets, with a hold beside its
	// one button.
	if c := credential("claude-sierra@example.com.json"); c.ResetCredits == nil || c.ResetCredits.Redeemable || c.ResetCredits.Hold != "" ||
		!slices.Equal(kinds(c), []string{"full:1:true:", "fable:2:true:notLimited"}) {
		t.Errorf("Claude holding full and Fable resets = %+v", c.ResetCredits)
	}
	if c := credential("claude-agency@example.com.json"); c.ResetCredits == nil || c.ResetCredits.Hold != "notLimited" ||
		!slices.Equal(kinds(c), []string{"full:3:true:notLimited"}) {
		t.Errorf("held Claude reset = %+v", c.ResetCredits)
	}
	// A Claude credential CPA has in cooldown, still offered, with its hold
	// dated on every kind, and one kind this build cannot name, counted and
	// never offered.
	if c := credential("claude-unavailable@example.com.json"); c.Status != StatusUnavailable || c.ResetCredits == nil ||
		c.ResetCredits.Hold != "cooldown" || c.ResetCredits.HoldUntilEpoch == nil ||
		!slices.Equal(kinds(c), []string{"full:1:true:cooldown", "unknown:1:false:cooldown"}) {
		t.Errorf("cooling Claude credential = status %q, %+v", c.Status, c.ResetCredits)
	}
	// A cooldown already over, dropped, on an entry from a Quota Cache too old
	// to say which kind each reset is; and a disabled credential's count
	// without its button.
	if c := credential("claude-stale@example.com.json"); c.ResetCredits == nil || c.ResetCredits.Hold != "" || c.ResetCredits.HoldUntilEpoch != nil ||
		c.ResetCredits.Kinds == nil || len(c.ResetCredits.Kinds) != 0 {
		t.Errorf("expired hold, no kinds = %+v", c.ResetCredits)
	}
	if c := credential("claude-disabled@example.com.json"); c.ResetCredits == nil || offered(c.ResetCredits) ||
		!slices.Equal(kinds(c), []string{"fable:1:false:"}) {
		t.Errorf("disabled Claude reset = %+v", c.ResetCredits)
	}
	// Codex's credits are one sort: the button on the count, and no kinds.
	if c := credential("codex-noor@example.com.json"); c.ResetCredits == nil || !c.ResetCredits.Redeemable || len(c.ResetCredits.Kinds) != 0 {
		t.Errorf("codex resets = %+v", c.ResetCredits)
	}
	// Codex credits and renewal, and the unlimited and lapsed shapes.
	if c := credential("codex-noor@example.com.json"); c.Credits == nil || c.Credits.Display != "57,706.15" || c.RenewalAtEpoch == nil {
		t.Errorf("codex extras = %+v, renewal %v", c.Credits, c.RenewalAtEpoch)
	}
	if c := credential("codex-model@example.com.json"); c.Credits == nil || !c.Credits.Unlimited || c.RenewalAtEpoch != nil || c.Plan != "Pro 100" {
		t.Errorf("unlimited codex = %+v, renewal %v, plan %q", c.Credits, c.RenewalAtEpoch, c.Plan)
	}
	// Grok prepaid credits beside the subscription's own display name.
	if c := credential("xai-noor@example.com.json"); c.Credits == nil || c.Credits.Display != "$12.40" || c.Plan != "SuperGrok Heavy" {
		t.Errorf("grok = %+v, plan %q", c.Credits, c.Plan)
	}
	if c := credential("xai-raw@example.com.json"); c.Credits == nil || c.Credits.Display != "$0.00" || c.Plan != "SuperGrok" {
		t.Errorf("empty grok = %+v, plan %q", c.Credits, c.Plan)
	}
	// And a credential with none of it: null, not empty.
	if c := credential("claude-pending@example.com.json"); c.Credits != nil || c.RenewalAtEpoch != nil || c.ResetCredits != nil {
		t.Errorf("pending credential = %+v", c)
	}
}
