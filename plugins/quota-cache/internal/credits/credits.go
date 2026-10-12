// Package credits works out the monthly Claude API credit of every Console
// organization quota-cache lists: the amounts, the cycle and the refill, the
// out and stale states, and the issues. Compute turns its inputs into the
// credit state quota-cache publishes beside its snapshot (client.CreditState).
//
// Everything here is pure: Compute takes the current instant as an input and
// nothing in this package calls time.Now(). That is what makes the golden
// states meaningful, and it is enforced by a test.
//
// The logic and the price table came from Quota Glance's aggregate package
// unchanged, so the published figures are the ones Quota Glance showed.
package credits

import (
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/settings"
)

// Item is one Claude Console organization quota-cache lists: its account id
// and its configuration, as the anthropic-api snapshot entry for it carries
// them.
type Item struct {
	// ID is client.APICreditOrgAccount of the organization, or "item-<n>" for
	// an item that cannot be linked to one.
	ID string
	// Credit is the item as quota-cache read it. quota-cache always sets it; a
	// nil one, like an anthropic-api entry with no api_credit, is counted as
	// misconfigured.
	Credit *client.APICredit
}

// Input is everything Compute needs. It holds no clock and no file handle: the
// caller reads the files and passes them in.
type Input struct {
	// Now is the instant to compute at, truncated to the second by Compute.
	Now time.Time
	// Snapshot is the snapshot the state is published beside. Only its
	// written_at is read.
	Snapshot client.Snapshot
	// Items is every organization quota-cache lists, in any order. The
	// accounts come from here, not from the snapshot's entries, so a change on
	// the page is published before the next scan. No item, no api_credits.
	Items []Item
	// Meter is quota-cache's API meter, nil when its file is missing or
	// unreadable; MeterError then says which (client.CreditMeterError*).
	Meter      *client.APIMeter
	MeterError string
	// Settings is what the operator saved on the page. It applies only while
	// SettingsState is client.CreditSettingsOK.
	Settings settings.Values
	// SettingsState is a client.CreditSettings value, "" for
	// client.CreditSettingsOK. Either is client.CreditSettingsUnreadable
	// when Settings.Unreadable is set.
	SettingsState string
	// Import is where the import from Quota Glance's settings.json stands,
	// published as settings.import.
	Import *client.CreditImport
	// Organizations says where Items came from, published as organizations.
	Organizations client.CreditOrganizations
	// Version is quota-cache's version, published.
	Version string
	// StaleAfter is always 0 when quota-cache publishes: stale is each
	// reader's judgement, with client.CreditsAt. A test sets it to compare a
	// published state judged later with one computed then.
	StaleAfter time.Duration
}

// Compute works out the credit state at in.Now. It is pure and deterministic:
// the same input gives the same state, byte for byte once encoded.
func Compute(in Input) *client.CreditState {
	now := in.Now.UTC().Truncate(time.Second)
	state := settingsStateOf(in)
	in.SettingsState = state
	if state != client.CreditSettingsOK {
		// Nothing stored applies, only what is configured.
		in.Settings = settings.Values{APICredits: map[string]settings.APICredit{}, Renewals: map[string]settings.Renewal{}}
	}
	revision := settings.RevisionText(in.Settings.Revision)
	out := &client.CreditState{
		Schema:            client.CreditsSchema,
		QuotaCacheVersion: in.Version,
		ComputedAt:        now,
		Inputs: client.CreditInputs{
			SnapshotWrittenAt: timePointerOf(in.Snapshot.WrittenAt),
			MeterError:        in.MeterError,
			SettingsRevision:  revision,
		},
		Settings: client.CreditSettingsState{
			State:    state,
			Editable: state == client.CreditSettingsOK,
			Revision: revision,
			Import:   cloneImport(in.Import),
		},
		APICredits:    apiCreditsOf(in, now),
		Renewals:      renewalsOf(in.Settings),
		Organizations: in.Organizations,
	}
	if in.Meter != nil {
		out.Inputs.MeterFlushedAt = timePointerOf(in.Meter.FlushedAt)
	}
	out.Organizations.Migration = copyOf(in.Organizations.Migration)
	out.ValidUntil = validUntilOf(out.APICredits)
	return out
}

// settingsStateOf is the state of the saved values Compute publishes. Values
// the store could not read are unreadable, unless the state already says why
// nothing stored applies.
func settingsStateOf(in Input) string {
	switch {
	case in.Settings.Unreadable && (in.SettingsState == "" || in.SettingsState == client.CreditSettingsOK):
		return client.CreditSettingsUnreadable
	case in.SettingsState != "":
		return in.SettingsState
	}
	return client.CreditSettingsOK
}

// renewalsOf is every stored renewal date, keyed by auth index.
func renewalsOf(v settings.Values) map[string]client.RenewalSetting {
	out := make(map[string]client.RenewalSetting, len(v.Renewals))
	for id, renewal := range v.Renewals {
		out[id] = client.RenewalSetting{Date: renewal.Date, Revision: settings.RevisionText(renewal.Rev), UpdatedAtEpoch: renewal.UpdatedAt.Unix()}
	}
	return out
}

// validUntilOf is the soonest instant at which a figure of d would change
// with nothing else changing, or nil when there is none:
//
//   - the end of an account's cycle, when the next one begins;
//   - for an account with no cycle, a used reading's time plus 31 days, when
//     it grows too old to use;
//   - a stored reading's time, while it is unused because it is dated ahead.
//
// Every other rule that reads the clock is one client.CreditsAt re-applies.
func validUntilOf(d *client.CreditDocument) *time.Time {
	if d == nil {
		return nil
	}
	var soonest *time.Time
	consider := func(t time.Time) {
		if soonest == nil || t.Before(*soonest) {
			soonest = &t
		}
	}
	for _, a := range d.Accounts {
		if a.RenewsAtEpoch != nil {
			consider(time.Unix(*a.RenewsAtEpoch, 0).UTC())
		}
		stored := a.Settings.Reading
		if stored == nil {
			continue
		}
		switch a.Settings.ReadingUnusedReason {
		case "":
			if a.RenewsAtEpoch == nil {
				consider(time.Unix(stored.AtEpoch, 0).UTC().Add(readingWithoutCycle))
			}
		case client.CreditReadingFuture:
			consider(time.Unix(stored.AtEpoch, 0).UTC())
		}
	}
	return soonest
}

// timePointerOf is the nullable form of an instant, nil for the zero time.
func timePointerOf(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}

// cloneImport is a copy of in that shares nothing with it.
func cloneImport(in *client.CreditImport) *client.CreditImport {
	if in == nil {
		return nil
	}
	out := *in
	out.Kind, out.SourcePath, out.SHA256, out.SettingsSHA256 = copyOf(in.Kind), copyOf(in.SourcePath), copyOf(in.SHA256), copyOf(in.SettingsSHA256)
	out.LegacyRevision, out.LegacyUpdatedAt = copyOf(in.LegacyRevision), copyOf(in.LegacyUpdatedAt)
	out.Counts, out.ImportedAt = copyOf(in.Counts), copyOf(in.ImportedAt)
	return &out
}

// copyOf is a new pointer to a copy of *p, and nil for nil.
func copyOf[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
