package aggregate

import (
	"math/big"
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

func banked(count int) *qc.Quota {
	return &qc.Quota{Schema: 1, ResetCredits: &qc.ResetCredits{AvailableCount: count}}
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
		if got.ResetCredits == nil || !got.ResetCredits.Redeemable {
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
		if got.ResetCredits.Redeemable != one.want {
			t.Fatalf("%s: redeemable = %v, want %v", name, got.ResetCredits.Redeemable, one.want)
		}
	}
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
		got := oneCredential(t, Identity{AuthIndex: "claude-a@northwind.dev.json", Provider: "claude"}, qc.Entry{Quota: quota}, true).ResetCredits
		if got.Hold != one.want {
			t.Fatalf("%s: hold = %q, want %q", name, got.Hold, one.want)
		}
		if (got.HoldUntilEpoch == nil) != (one.wantUntil == nil) || (got.HoldUntilEpoch != nil && *got.HoldUntilEpoch != *one.wantUntil) {
			t.Fatalf("%s: holdUntilEpoch = %v, want %v", name, got.HoldUntilEpoch, one.wantUntil)
		}
		// A hold is a hint beside the button, never a reason to remove it.
		if !got.Redeemable {
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

	// Claude holding resets: one spendable, one with a hold beside its button.
	if c := credential("claude-siphorchannel@example.com.json"); c.ResetCredits == nil || !c.ResetCredits.Redeemable || c.ResetCredits.Hold != "" {
		t.Errorf("spendable Claude reset = %+v", c.ResetCredits)
	}
	if c := credential("claude-agency@example.com.json"); c.ResetCredits == nil || c.ResetCredits.Hold != "notLimited" || !c.ResetCredits.Redeemable {
		t.Errorf("held Claude reset = %+v", c.ResetCredits)
	}
	// A Claude credential CPA has in cooldown, still offered, with its hold
	// dated.
	if c := credential("claude-unavailable@example.com.json"); c.Status != StatusUnavailable || c.ResetCredits == nil ||
		!c.ResetCredits.Redeemable || c.ResetCredits.Hold != "cooldown" || c.ResetCredits.HoldUntilEpoch == nil {
		t.Errorf("cooling Claude credential = status %q, %+v", c.Status, c.ResetCredits)
	}
	// A cooldown already over, dropped; and a disabled credential's count
	// without its button.
	if c := credential("claude-stale@example.com.json"); c.ResetCredits == nil || c.ResetCredits.Hold != "" || c.ResetCredits.HoldUntilEpoch != nil {
		t.Errorf("expired hold = %+v", c.ResetCredits)
	}
	if c := credential("claude-disabled@example.com.json"); c.ResetCredits == nil || c.ResetCredits.Redeemable {
		t.Errorf("disabled Claude reset = %+v", c.ResetCredits)
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
