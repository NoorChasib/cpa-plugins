package aggregate

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// The API credit figures are Quota Cache's, and their rules are tested there.
// These test what quota-glance does with the published state: show it as a
// reader at now sees it, read-only, and say why when there is none.

func jsonOf(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// workedAt is the instant the api-credits and meter-gaps fixtures describe.
var workedAt = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// cardAt is the card built from state at now, with stale-after.
func cardAt(state *qc.CreditState, now time.Time, staleAfter time.Duration) *APICredits {
	return Build(Input{Credits: state, StaleAfter: staleAfter}, now).APICredits
}

// readOnlyMasked is a card with the fields quota-glance maps set as Quota
// Cache published them, so the rest can be compared whole.
func readOnlyMasked(card, published *APICredits) APICredits {
	out := *card
	out.Editing = published.Editing
	out.Accounts = slices.Clone(card.Accounts)
	for i := range out.Accounts {
		out.Accounts[i].Settings.Editable = published.Accounts[i].Settings.Editable
		out.Accounts[i].Settings.NotEditableReason = published.Accounts[i].Settings.NotEditableReason
	}
	return out
}

// Every published field is shown as the overlay leaves it, except
// editing and each account's editable and reason, which say the values are
// edited on Quota Cache's page, or, while Quota Cache cannot read its saved
// settings, say so. A reason no page could lift is kept. The published state
// itself is never changed.
func TestCreditsArePassedThroughReadOnly(t *testing.T) {
	for _, name := range []string{"api-credits", "degraded-states"} {
		state := fixtureCredits(t, name)
		before := jsonOf(t, state)
		now := state.ComputedAt.Add(20 * time.Minute)
		card := cardAt(state, now, 45*time.Minute)
		judged := qc.CreditsAt(state, now, 45*time.Minute).APICredits
		if got, want := jsonOf(t, readOnlyMasked(card, judged)), jsonOf(t, judged); got != want {
			t.Errorf("%s: the card differs from the published state outside the read-only fields:\n got %s\nwant %s", name, got, want)
		}
		if card.Editing != (APICreditEditing{Reason: qc.CreditEditingQuotaCache}) {
			t.Errorf("%s: editing = %+v", name, card.Editing)
		}
		for i, account := range card.Accounts {
			published := judged.Accounts[i].Settings.NotEditableReason
			want := qc.CreditEditingQuotaCache
			if structuralReasons[published] {
				want = published
			}
			if account.Settings.Editable || account.Settings.NotEditableReason != want {
				t.Errorf("%s %s: editable %v reason %q, want false %q", name, account.ID, account.Settings.Editable, account.Settings.NotEditableReason, want)
			}
		}
		if jsonOf(t, state) != before {
			t.Errorf("%s: building the card changed the published state", name)
		}
	}

	// The degraded fixture carries every reason no page can lift.
	reasons := map[string]bool{}
	for _, account := range buildDegraded(t).APICredits.Accounts {
		reasons[account.Settings.NotEditableReason] = true
	}
	for _, want := range []string{qc.CreditNotEditableNoOrganization, qc.CreditNotEditableDuplicateOrganization,
		qc.CreditNotEditableOverLimit, qc.CreditNotEditableCacheTooOld, qc.CreditEditingQuotaCache} {
		if !reasons[want] {
			t.Errorf("no degraded account not editable for %q", want)
		}
	}

	// Quota Cache that cannot read its saved settings says why, and so does
	// every account it would otherwise let the operator edit.
	const reason = qc.CreditEditingSettingsUnreadable
	unreadable := fixtureCredits(t, "degraded-states")
	unreadable.Settings = qc.CreditSettingsState{State: qc.CreditSettingsUnreadable}
	unreadable.APICredits.Editing = APICreditEditing{Reason: reason}
	for i := range unreadable.APICredits.Accounts {
		if s := &unreadable.APICredits.Accounts[i].Settings; !structuralReasons[s.NotEditableReason] {
			s.Editable, s.NotEditableReason = false, reason
		}
	}
	card := cardAt(unreadable, unreadable.ComputedAt, 45*time.Minute)
	if card.Editing != (APICreditEditing{Reason: reason}) {
		t.Errorf("unreadable settings: editing = %+v", card.Editing)
	}
	for _, account := range card.Accounts {
		if s := account.Settings; s.Editable || (s.NotEditableReason != reason && !structuralReasons[s.NotEditableReason]) {
			t.Errorf("unreadable settings %s: %+v", account.ID, s)
		}
	}

	// A Quota Cache that lists no organization publishes none, and there is
	// no card, whatever the snapshot holds.
	state := fixtureCredits(t, "api-credits")
	state.APICredits = nil
	if card := Build(Input{Snapshot: loadSnapshot(t, "api-credits.json"), Credits: state}, workedAt).APICredits; card != nil {
		t.Errorf("apiCredits = %+v; want null", card)
	}
}

// Without a usable state, each anthropic-api entry in the snapshot is one
// account with no figures, cacheTooOld, not counted, and saying why; nothing
// only Quota Cache could know is shown, and no renewal date entered on its
// page is known. Which file is behind each cause is source's (source_test.go).
func TestFallbackWithoutQuotaCacheState(t *testing.T) {
	snapshot := loadSnapshot(t, "seven-credentials.json")
	configured := map[string]*qc.APICredit{}
	for _, entry := range snapshot.Entries {
		if entry.Provider == qc.ProviderAnthropicAPI {
			configured[entry.AuthIndex] = entry.APICredit
		}
	}
	entries := len(configured)
	if entries == 0 {
		t.Fatal("the fixture has no anthropic-api entry; the test asserts nothing")
	}
	published := fixtureCredits(t, "seven-credentials")
	for cause, sentence := range fallbackIssues {
		in := Input{
			Snapshot: snapshot, Identities: fixtureRoster(), StaleAfter: 45 * time.Minute,
			CreditsError: cause,
		}
		if cause == CreditsAbandoned {
			// An abandoned file is loaded, and ignored.
			in.Credits = published
		}
		doc := Build(in, at(t, 0))
		card := doc.APICredits
		if card == nil {
			t.Fatalf("%s: apiCredits is null", cause)
		}
		if card.Title != apiCreditsTitle || card.Currency != "USD" || card.Pricing != (APICreditPricing{}) || card.Meter != nil ||
			card.Editing != (APICreditEditing{Reason: qc.CreditEditingQuotaCache}) ||
			card.Pool != (APICreditPool{AccountCount: entries, MissingCount: entries}) ||
			card.Unlinked == nil || len(card.Unlinked) != 0 || card.Orphans == nil || len(card.Orphans) != 0 || len(card.Accounts) != entries {
			t.Errorf("%s: card %s", cause, jsonOf(t, card))
		}
		for i, account := range card.Accounts {
			// The published order, and the credit and refill date the
			// snapshot's entry carries, as a Quota Cache older than 0.2.0
			// wrote them from its configuration.
			want, c := published.APICredits.Accounts[i], configured[account.ID]
			if c == nil || c.MonthlyUSD == "" || c.Renews == "" || account.ID != want.ID || account.Label != want.Label ||
				account.Order != want.Order || account.OrganizationID != want.OrganizationID ||
				account.Settings.ConfigMonthlyUSD != c.MonthlyUSD || account.Settings.ConfigRenews != c.Renews {
				t.Errorf("%s: account %d = %s, want %s with the snapshot's %s", cause, i, jsonOf(t, account), jsonOf(t, want), jsonOf(t, c))
			}
			if account.State != qc.CreditStateCacheTooOld || account.Counted || account.HasEstimate || account.Basis != "" ||
				!reflect.DeepEqual(account.DataIssues, []string{qc.CreditIssueCacheTooOld}) || account.Issue != sentence ||
				account.LeftText != "" || account.Level != "" || account.RenewsAtEpoch != nil || account.Reading != nil ||
				account.Unpriced == nil || account.Settings.Editable || account.Settings.NotEditableReason != qc.CreditEditingQuotaCache ||
				account.Settings.Reading != nil || account.Settings.MonthlyUSD != "" || account.Settings.Revision != "" {
				t.Errorf("%s: account %s", cause, jsonOf(t, account))
			}
		}
		for _, c := range doc.Credentials {
			if c.RenewalSetting != nil || (c.RenewalSource != nil && *c.RenewalSource == qc.RenewalDashboard) {
				t.Errorf("%s: %s renews from a date no one can know: %v %v", cause, c.ID, c.RenewalSetting, *c.RenewalSource)
			}
		}
	}

	// Nothing at all from Quota Cache reads as missing.
	if card := Build(Input{Snapshot: snapshot}, at(t, 0)).APICredits; card == nil || card.Accounts[0].Issue != fallbackIssues[CreditsMissing] {
		t.Errorf("no state and no reason: %s", jsonOf(t, card))
	}
	// Without an anthropic-api entry there is no card, as before.
	if card := Build(Input{Snapshot: loadSnapshot(t, "degraded-states.json"), CreditsError: CreditsMissing}, at(t, 0)).APICredits; card == nil {
		t.Error("the degraded snapshot has anthropic-api entries; the next case asserts nothing")
	}
	empty := qc.Snapshot{Schema: 1, Entries: map[string]qc.Entry{}}
	for _, cause := range []string{CreditsMissing, CreditsUnreadable, CreditsAbandoned} {
		if card := Build(Input{Snapshot: empty, CreditsError: cause}, at(t, 0)).APICredits; card != nil {
			t.Errorf("%s with no entry: %s", cause, jsonOf(t, card))
		}
	}
}

// stateWithOK is the api-credits fixture's state with the account named
// label, whose state is ok and which is anchored, so the overlay's rules all
// reach it.
func stateWithOK(t *testing.T, label string) (*qc.CreditState, int) {
	t.Helper()
	state := fixtureCredits(t, "api-credits")
	for i, account := range state.APICredits.Accounts {
		if account.Label == label {
			if account.State != qc.CreditStateOK || account.SpentSinceEpoch == nil {
				t.Fatalf("%s: state %s spentSince %v; the test needs an anchored ok account", label, account.State, account.SpentSinceEpoch)
			}
			return state, i
		}
	}
	t.Fatalf("no account %s", label)
	return nil, 0
}

// The published state is judged with quota-glance's own stale-after, by
// CreditsAt: the meter's age, the state's age and valid_until each turn ok
// into stale, exactly at their boundaries. A state written as Quota Cache
// stopped shows the gap and lower bounds on anchored accounts from five
// minutes after the stop, and not a second before.
func TestStaleAfterAppliesToPublishedState(t *testing.T) {
	const staleAfter = 45 * time.Minute
	state, i := stateWithOK(t, "alpha")
	computed := state.ComputedAt
	stateAt := func(s *qc.CreditState, now time.Time) APICreditAccount {
		t.Helper()
		return cardAt(s, now, staleAfter).Accounts[i]
	}
	if got := stateAt(state, computed); got.State != qc.CreditStateOK || got.LowerBound {
		t.Fatalf("at computed_at: %s", jsonOf(t, got))
	}

	// The meter's age: it was saved a minute before the state.
	saved := time.Unix(state.APICredits.Meter.UpdatedAtEpoch, 0)
	if got := stateAt(state, saved.Add(staleAfter)); got.State != qc.CreditStateOK {
		t.Errorf("meter exactly stale-after old: %s", got.State)
	}
	got := stateAt(state, saved.Add(staleAfter+time.Second))
	if got.State != qc.CreditStateStale || !got.LowerBound || got.DataIssues[0] != qc.CreditIssueStale || got.Issue != qc.CreditSentenceStale {
		t.Errorf("meter a second past stale-after: %s", jsonOf(t, got))
	}
	if card := cardAt(state, saved.Add(staleAfter+time.Second), staleAfter); !card.Meter.Stale || !card.Pool.LowerBound {
		t.Errorf("meter stale %v pool lower bound %v", card.Meter.Stale, card.Pool.LowerBound)
	}

	// The state's own age, with a meter saved just now.
	fresh, _ := stateWithOK(t, "alpha")
	at := computed.Add(staleAfter + time.Second)
	fresh.APICredits.Meter.UpdatedAtEpoch = at.Add(-time.Minute).Unix()
	if got := stateAt(fresh, computed.Add(staleAfter)); got.State != qc.CreditStateOK {
		t.Errorf("state exactly stale-after old: %s", got.State)
	}
	if got := stateAt(fresh, at); got.State != qc.CreditStateStale {
		t.Errorf("state a second past stale-after: %s", got.State)
	}

	// valid_until, with its grace for the publisher to write first.
	bounded, _ := stateWithOK(t, "alpha")
	until := computed.Add(10 * time.Minute)
	bounded.ValidUntil = &until
	if got := stateAt(bounded, until.Add(qc.CreditBoundaryGrace-time.Second)); got.State != qc.CreditStateOK {
		t.Errorf("a second before valid_until's grace ends: %s", got.State)
	}
	if got := stateAt(bounded, until.Add(qc.CreditBoundaryGrace)); got.State != qc.CreditStateStale {
		t.Errorf("at valid_until's grace: %s", got.State)
	}

	// The final state of a Quota Cache that stopped.
	stopped, _ := stateWithOK(t, "alpha")
	stop := computed.Unix()
	stopped.APICredits.Meter.StoppedAtEpoch = &stop
	if got := stateAt(stopped, computed.Add(qc.MeterBriefGap-time.Second)); slices.Contains(got.DataIssues, qc.CreditIssueMeterGap) || got.LowerBound {
		t.Errorf("a second before the gap counts: %s", jsonOf(t, got))
	}
	card := cardAt(stopped, computed.Add(qc.MeterBriefGap), staleAfter)
	for _, account := range card.Accounts {
		anchored := account.SpentSinceEpoch != nil
		if (account.State == qc.CreditStateOK || account.State == qc.CreditStateOut) && anchored &&
			(!slices.Contains(account.DataIssues, qc.CreditIssueMeterGap) || !account.LowerBound) {
			t.Errorf("five minutes after the stop, %s: %s", account.Label, jsonOf(t, account))
		}
	}
	if !card.Pool.LowerBound {
		t.Error("five minutes after the stop the pool is not a lower bound")
	}
}

// Quota Cache judges the API credit card's levels by the thresholds the
// rows here use, under the same names.
func TestCreditLevelsMatchTheRows(t *testing.T) {
	if qc.CreditLevelCriticalAtOrBelow != criticalAtOrBelow || qc.CreditLevelLowAtOrBelow != lowAtOrBelow {
		t.Fatalf("Quota Cache's levels %d and %d; the rows' %d and %d",
			qc.CreditLevelCriticalAtOrBelow, qc.CreditLevelLowAtOrBelow, criticalAtOrBelow, lowAtOrBelow)
	}
	if qc.LevelOK != LevelOK || qc.LevelLow != LevelLow || qc.LevelCritical != LevelCritical {
		t.Fatal("Quota Cache names the levels differently")
	}
}

// The OpenAI API credit card is Quota Cache's published OpenAI state, judged
// at now with quota-glance's stale-after and marked read-only exactly as the
// Claude card is, under its own key. Neither card is built from the other's
// state, and the published state itself is never changed.
func TestOpenAICreditsCardIsQuotaCachesStateReadOnly(t *testing.T) {
	state := fixtureOpenAICredits(t, "degraded-states")
	before := jsonOf(t, state)
	now := state.ComputedAt.Add(20 * time.Minute)
	doc := Build(Input{OpenAICredits: state, StaleAfter: 45 * time.Minute}, now)
	card := doc.OpenAIAPICredits
	if card == nil {
		t.Fatal("openaiApiCredits is absent")
	}
	judged := qc.CreditsAt(state, now, 45*time.Minute).APICredits
	if got, want := jsonOf(t, readOnlyMasked(card, judged)), jsonOf(t, judged); got != want {
		t.Errorf("the card differs from the published state outside the read-only fields:\n got %s\nwant %s", got, want)
	}
	if card.Provider != qc.ProviderOpenAIAPI || card.Title != "Monthly OpenAI API Credit" {
		t.Errorf("provider %q title %q", card.Provider, card.Title)
	}
	if card.Editing != (APICreditEditing{Reason: qc.CreditEditingQuotaCache}) {
		t.Errorf("editing = %+v", card.Editing)
	}
	for _, account := range card.Accounts {
		if account.Settings.Editable || account.Settings.NotEditableReason != qc.CreditEditingQuotaCache {
			t.Errorf("%s: editable %v reason %q", account.ID, account.Settings.Editable, account.Settings.NotEditableReason)
		}
	}
	if jsonOf(t, state) != before {
		t.Error("building the card changed the published state")
	}

	// The OpenAI state is not the Claude card's, and the Claude state is not
	// the OpenAI card's.
	if doc.APICredits != nil {
		t.Errorf("an OpenAI state alone built a Claude card: %s", jsonOf(t, doc.APICredits))
	}
	claude := Input{Snapshot: loadSnapshot(t, "degraded-states.json"), Credits: fixtureCredits(t, "degraded-states"), StaleAfter: 45 * time.Minute}
	alone := Build(claude, now)
	if alone.OpenAIAPICredits != nil {
		t.Errorf("a Claude state alone built an OpenAI card: %s", jsonOf(t, alone.OpenAIAPICredits))
	}
	both := claude
	both.OpenAICredits = state
	if got, want := jsonOf(t, Build(both, now).APICredits), jsonOf(t, alone.APICredits); got != want {
		t.Error("the Claude card changed with an OpenAI state beside it")
	}
}

// Without a usable OpenAI state there is no OpenAI card, whatever the cause
// and whatever the snapshot holds: no snapshot entry names an OpenAI
// organization, so there is nothing a fallback could list. The key is then
// absent from the document, not null, and the Claude card is as it would be.
func TestNoOpenAICardWithoutAUsableState(t *testing.T) {
	published := fixtureOpenAICredits(t, "degraded-states")
	snapshot := loadSnapshot(t, "seven-credentials.json")
	for _, cause := range []string{CreditsMissing, CreditsUnreadable, CreditsAbandoned, ""} {
		in := Input{Snapshot: snapshot, Identities: fixtureRoster(), StaleAfter: 45 * time.Minute, OpenAICreditsError: cause}
		if cause == CreditsAbandoned {
			// An abandoned file is loaded, and ignored.
			in.OpenAICredits = published
		}
		doc := Build(in, at(t, 0))
		if doc.OpenAIAPICredits != nil {
			t.Errorf("%q: openaiApiCredits = %s", cause, jsonOf(t, doc.OpenAIAPICredits))
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal([]byte(jsonOf(t, doc)), &keys); err != nil {
			t.Fatal(err)
		}
		if _, ok := keys["openaiApiCredits"]; ok {
			t.Errorf("%q: the document carries openaiApiCredits: %s", cause, keys["openaiApiCredits"])
		}
		if doc.APICredits == nil {
			t.Errorf("%q: the Claude fallback card is gone", cause)
		}
	}

	// A Quota Cache that lists no OpenAI organization publishes none.
	empty := fixtureOpenAICredits(t, "degraded-states")
	empty.APICredits = nil
	if card := Build(Input{OpenAICredits: empty}, at(t, 0)).OpenAIAPICredits; card != nil {
		t.Errorf("an empty OpenAI state built a card: %s", jsonOf(t, card))
	}
}

// A document served again while its source fails takes the OpenAI card from
// the fresh build too, as it takes the Claude one, so a value entered on
// Quota Cache's page meanwhile shows, and a state that has gone takes its
// card with it.
func TestWithCreditsCarriesTheOpenAICard(t *testing.T) {
	without := Build(Input{StaleAfter: 45 * time.Minute}, at(t, 0))
	with := Build(Input{OpenAICredits: fixtureOpenAICredits(t, "degraded-states"), StaleAfter: 45 * time.Minute}, at(t, 0))
	if with.OpenAIAPICredits == nil {
		t.Fatal("the fixture built no OpenAI card")
	}
	if got := WithCredits(without, with); got.OpenAIAPICredits != with.OpenAIAPICredits {
		t.Errorf("WithCredits kept %v; want the fresh card", got.OpenAIAPICredits)
	}
	if without.OpenAIAPICredits != nil {
		t.Error("WithCredits changed the document it was given")
	}
	if got := WithCredits(with, without); got.OpenAIAPICredits != nil {
		t.Errorf("WithCredits kept a card whose state has gone: %s", jsonOf(t, got.OpenAIAPICredits))
	}
}
