package aggregate

import (
	"sort"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// Why there is no usable credit state from Quota Cache: Input.CreditsError.
const (
	// CreditsMissing: there is no credit state file. The Quota Cache writing
	// the snapshot predates 0.2.0, or has not published yet.
	CreditsMissing = "missing"
	// CreditsUnreadable: the file is there and does not load. It is not a
	// regular file, is too large, is not JSON, or has a schema this build does
	// not read.
	CreditsUnreadable = "unreadable"
	// CreditsAbandoned: the snapshot was written more than
	// qc.CreditPublishLagLimit after the state was computed. Quota Cache's
	// snapshot writer runs and its publisher does not, as after a rollback to
	// a Quota Cache older than 0.2.0, so the file is ignored rather than shown
	// as current.
	CreditsAbandoned = "abandoned"
)

// apiCreditsTitle is the card's heading, Quota Cache's own.
const apiCreditsTitle = "Monthly API Credit"

// fallbackIssues is the sentence every organization carries when there is no
// usable state, by why there is none.
var fallbackIssues = map[string]string{
	CreditsMissing:    "Quota Cache has not published this organization's credit. Update Quota Cache to 0.2.0 or newer and check that it is enabled.",
	CreditsUnreadable: "Quota Glance cannot read Quota Cache's published credit state; check Quota Cache's page.",
	CreditsAbandoned:  "Quota Cache is running but no longer publishes API credit figures. Update it to 0.2.0 or newer, or check its page.",
}

// usableCredits is Quota Cache's published state when it can be shown, and
// nil when there is none or CreditsError says why not.
func usableCredits(in Input) *qc.CreditState {
	if in.CreditsError != "" {
		return nil
	}
	return in.Credits
}

// creditsCause is why there is no usable state. An input that carries neither
// a state nor a reason has nothing from Quota Cache, which is missing.
func creditsCause(in Input) string {
	if in.CreditsError != "" {
		return in.CreditsError
	}
	return CreditsMissing
}

// apiCreditsOf is the API credit card. Every figure in it is Quota Cache's:
// the published state judged at now with this plugin's stale-after
// (qc.CreditsAt), and marked read-only. Without a usable state it is a card
// that still lists every organization in the snapshot and says why it has no
// figures. Nil when there is nothing to list.
func apiCreditsOf(in Input, now time.Time) *APICredits {
	if state := usableCredits(in); state != nil {
		return readOnly(qc.CreditsAt(state, now, in.StaleAfter))
	}
	return fallbackCreditsOf(in)
}

// structuralReasons are the reasons an account could not be edited anywhere,
// Quota Cache's page included. They are kept; every other reason becomes why
// it is not edited here.
var structuralReasons = map[string]bool{
	qc.CreditNotEditableNoOrganization:        true,
	qc.CreditNotEditableDuplicateOrganization: true,
	qc.CreditNotEditableOverLimit:             true,
	qc.CreditNotEditableCacheTooOld:           true,
}

// readOnly is the published card as quota-glance shows it: nothing on it is
// editable here. The reason is Quota Cache's own while it cannot read its
// saved settings, settingsUnreadable, so the page points at that file;
// otherwise it is quotaCache, edited on Quota Cache's page. state is
// CreditsAt's copy, which this changes in place.
func readOnly(state *qc.CreditState) *APICredits {
	d := state.APICredits
	if d == nil {
		return nil
	}
	reason := qc.CreditEditingQuotaCache
	if state.Settings.State != qc.CreditSettingsOK && d.Editing.Reason != "" {
		reason = d.Editing.Reason
	}
	d.Editing = APICreditEditing{Reason: reason}
	for i := range d.Accounts {
		settings := &d.Accounts[i].Settings
		settings.Editable = false
		if !structuralReasons[settings.NotEditableReason] {
			settings.NotEditableReason = reason
		}
	}
	return d
}

// fallbackCreditsOf is the card without a usable state, built from the
// snapshot's anthropic-api entries alone: every organization with what its
// entry says of it, none counted, each saying why, and no figure that only
// Quota Cache could work out. Nil when the snapshot has no such entry.
func fallbackCreditsOf(in Input) *APICredits {
	entries := []qc.Entry{}
	for _, entry := range in.Snapshot.Entries {
		if entry.Provider == qc.ProviderAnthropicAPI {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		return nil
	}
	accounts := make([]APICreditAccount, 0, len(entries))
	for _, entry := range entries {
		accounts = append(accounts, fallbackAccountOf(entry, fallbackIssues[creditsCause(in)]))
	}
	// Quota Cache's order: position, then label, then id. An entry without
	// api_credit has no position and goes last.
	sort.Slice(accounts, func(i, j int) bool {
		a, b := accounts[i], accounts[j]
		if (a.Order < 0) != (b.Order < 0) {
			return b.Order < 0
		}
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		if a.Label != b.Label {
			return a.Label < b.Label
		}
		return a.ID < b.ID
	})
	return &APICredits{
		Title:    apiCreditsTitle,
		Currency: "USD",
		Editing:  APICreditEditing{Reason: qc.CreditEditingQuotaCache},
		Pool:     APICreditPool{AccountCount: len(accounts), MissingCount: len(accounts)},
		Accounts: accounts,
		Unlinked: []APICreditUnlinked{},
		Orphans:  []APICreditOrphan{},
	}
}

// fallbackAccountOf is one organization with no figures: its name, place and
// organization, and, from a Quota Cache older than 0.2.0, the credit and
// refill date its configuration gave, in the form Quota Cache's own state
// carries them. Quota Cache 0.2.0 writes neither into the snapshot.
func fallbackAccountOf(entry qc.Entry, issue string) APICreditAccount {
	account := APICreditAccount{
		ID:                  entry.AuthIndex,
		Order:               -1,
		State:               qc.CreditStateCacheTooOld,
		MonthlyCreditSource: qc.CreditSourceNone,
		RenewsSource:        qc.CreditSourceNone,
		Unpriced:            []APICreditUnpriced{},
		DataIssues:          []string{qc.CreditIssueCacheTooOld},
		Issue:               issue,
		Settings:            APICreditSettings{NotEditableReason: qc.CreditEditingQuotaCache},
	}
	c := entry.APICredit
	if c == nil {
		return account
	}
	account.Label, account.Order, account.OrganizationID = c.Label, c.Position, c.OrganizationID
	if qc.ValidMonthlyUSD(c.MonthlyUSD) {
		account.Settings.ConfigMonthlyUSD = c.MonthlyUSD
	}
	if day, err := qc.ParseRenewal(c.Renews); err == nil {
		account.Settings.ConfigRenews = day.Format(time.DateOnly)
	}
	account.Settings.ConfigMonthlyUSDInvalid, account.Settings.ConfigRenewsInvalid = c.MonthlyUSDInvalid, c.RenewsInvalid
	return account
}
