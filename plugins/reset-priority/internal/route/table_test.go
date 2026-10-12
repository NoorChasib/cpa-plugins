package route

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// Every reason a table cannot route, alone and together: the first in order
// wins, and an inactive table holds nothing a pick could use.
func TestBuildInactiveReasonsInOrder(t *testing.T) {
	type change func(*Input)
	var (
		creditsGone   change = func(in *Input) { in.Credits, in.CreditsError = nil, "missing" }
		creditsBroken change = func(in *Input) { in.Credits, in.CreditsError = nil, "unreadable" }
		// The router loop never passes a state with an error; if it did, the
		// error would still decline.
		creditsErred change = func(in *Input) { in.CreditsError = "unreadable" }
		oldState     change = func(in *Input) { in.Credits.ComputedAt = in.Now.Add(-MaxCreditStateAge - time.Second) }
		justCurrent  change = func(in *Input) { in.Credits.ComputedAt = in.Now.Add(-MaxCreditStateAge) }
		boundary     change = func(in *Input) { in.Credits.ValidUntil = ptr(in.Now) }
		nearBoundary change = func(in *Input) { in.Credits.ValidUntil = ptr(in.Now.Add(time.Second)) }
		noSettings   change = func(in *Input) {
			in.Credits.Settings = qc.CreditSettingsState{State: qc.CreditSettingsUnreadable}
		}
		meterGone   change = func(in *Input) { in.Meter, in.MeterError = nil, "missing" }
		meterBroken change = func(in *Input) { in.Meter, in.MeterError = nil, "unreadable" }
		meterErred  change = func(in *Input) { in.MeterError = "missing" }
		oldCache    change = func(in *Input) { in.Meter.AuthIDsSince = nil }
		stopped     change = func(in *Input) { in.Meter.StoppedAt, in.Meter.StopReason = ptr(in.Now), qc.MeterStopShutdown }
		quiesced    change = func(in *Input) { in.Meter.StoppedAt, in.Meter.StopReason = ptr(in.Now), qc.MeterStopQuiesce }
		stale       change = func(in *Input) { in.Meter.FlushedAt = in.Now.Add(-MaxMeterAge - time.Second) }
		noCredits   change = func(in *Input) { in.Credits.APICredits = nil }
		noLinks     change = func(in *Input) {
			accounts := append([]qc.CreditAccount(nil), in.Credits.APICredits.Accounts...)
			for i := range accounts {
				accounts[i].OrganizationID = ""
			}
			in.Credits.APICredits = &qc.CreditDocument{Accounts: accounts}
		}
		noAccounts change = func(in *Input) { in.Credits.APICredits = &qc.CreditDocument{Accounts: []qc.CreditAccount{}} }
		justFresh  change = func(in *Input) { in.Meter.FlushedAt = in.Now.Add(-MaxMeterAge) }
	)
	cases := []struct {
		name    string
		changes []change
		want    Reason
	}{
		{"active", nil, ReasonNone},
		{"meter flushed exactly MaxMeterAge ago", []change{justFresh}, ReasonNone},
		{"state computed exactly MaxCreditStateAge ago", []change{justCurrent}, ReasonNone},
		{"state valid for one more second", []change{nearBoundary}, ReasonNone},
		{"credits missing", []change{creditsGone}, ReasonCreditsMissing},
		{"credits unreadable", []change{creditsBroken}, ReasonCreditsUnreadable},
		{"a credits error beside a state", []change{creditsErred}, ReasonCreditsUnreadable},
		{"state too old", []change{oldState}, ReasonCreditsStale},
		{"state at its valid_until", []change{boundary}, ReasonCreditsStale},
		{"settings unreadable", []change{noSettings}, ReasonSettingsUnavailable},
		{"meter missing", []change{meterGone}, ReasonMeterMissing},
		{"meter unreadable", []change{meterBroken}, ReasonMeterUnreadable},
		{"a meter error beside a meter", []change{meterErred}, ReasonMeterMissing},
		{"quota-cache older than 0.2.0", []change{oldCache}, ReasonMeterTooOld},
		{"meter stopped", []change{stopped}, ReasonMeterStopped},
		{"meter stale", []change{stale}, ReasonMeterStale},
		{"no api credits", []change{noCredits}, ReasonNoAPICredits},
		{"no linked organization", []change{noLinks}, ReasonNoLinkedOrgs},
		{"no account at all", []change{noAccounts}, ReasonNoLinkedOrgs},
		{"credits before meter", []change{noLinks, meterGone, creditsGone}, ReasonCreditsMissing},
		{"credits unreadable before stale", []change{oldState, creditsBroken}, ReasonCreditsUnreadable},
		{"stale state before settings", []change{noSettings, meterBroken, oldState}, ReasonCreditsStale},
		{"settings before meter", []change{stopped, meterBroken, noSettings}, ReasonSettingsUnavailable},
		{"meter unreadable before api credits", []change{noCredits, meterBroken}, ReasonMeterUnreadable},
		// A file Quota Cache 0.1.14 left stopped has no auth_ids_since, and
		// 0.2.0 adds it only when its meter starts: the stop is what to fix.
		{"stopped before too old", []change{oldCache, stopped}, ReasonMeterStopped},
		// Quota Cache starts no meter while it lists no organization, so the
		// file keeps the stop its last run recorded: 0.1.14's quiesce right
		// after the update, or a shutdown after a restart with none listed.
		{"0.1.14's stop before api credits", []change{noCredits, oldCache, quiesced}, ReasonMeterStopped},
		{"stopped before api credits", []change{noCredits, stopped}, ReasonMeterStopped},
		{"too old before stale", []change{stale, oldCache, noCredits}, ReasonMeterTooOld},
		{"stopped before stale", []change{noLinks, stale, stopped}, ReasonMeterStopped},
		{"stale before credits", []change{noCredits, stale}, ReasonMeterStale},
		{"no credits before links", []change{noCredits}, ReasonNoAPICredits},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := worked(t)
			in := f.input()
			for _, apply := range c.changes {
				apply(&in)
			}
			table := Build(in)
			if table.Reason != c.want {
				t.Fatalf("reason = %s; want %s", table.Reason, c.want)
			}
			if !table.BuiltAt.Equal(in.Now) {
				t.Fatalf("built at %v; want %v", table.BuiltAt, in.Now)
			}
			if c.want == ReasonNone {
				return
			}
			if len(table.keys) != 0 || len(table.Orgs) != 0 || len(table.rechecks) != 0 ||
				table.MappedKeys+table.UnmappedKeys+table.FailingKeys+table.ForeignKeys+table.UnlinkedKeys+table.StaleKeys != 0 {
				t.Fatalf("an inactive table holds data: %+v", table)
			}
			if in.Meter != nil && !table.MeterFlushedAt.Equal(in.Meter.FlushedAt) {
				t.Fatalf("meter flushed at %v; want %v", table.MeterFlushedAt, in.Meter.FlushedAt)
			}
		})
	}
}

// The table keeps the state's valid_until, in its own copy, whatever the
// state's reason; a state with none gives a table with none.
func TestBuildCopiesValidUntil(t *testing.T) {
	f := worked(t)
	in := f.input()
	if table := Build(in); table.ValidUntil != nil {
		t.Fatalf("valid until %v; want none", table.ValidUntil)
	}
	until := f.Now.Add(time.Hour)
	in.Credits.ValidUntil = &until
	for _, settings := range []string{qc.CreditSettingsOK, qc.CreditSettingsUnreadable} {
		in.Credits.Settings.State = settings
		table := Build(in)
		if table.ValidUntil == nil || !table.ValidUntil.Equal(until) || table.ValidUntil == &until {
			t.Fatalf("settings %s: valid until %v; want a copy of %v", settings, table.ValidUntil, until)
		}
	}
}

// account is an API credit account as Quota Cache publishes it, eligible
// unless changed.
func account(id, org string, order int, renews *int64) qc.CreditAccount {
	return qc.CreditAccount{
		ID: id, Label: id[len(id)-4:], Order: order, OrganizationID: org, State: qc.CreditStateOK, Counted: true,
		HasEstimate: true, Basis: "credit", Left: 10, LeftText: "$10.00", RenewsAtEpoch: renews, DataIssues: []string{},
	}
}

func epoch(t *testing.T, text string) *int64 { return ptr(at(t, text).Unix()) }

func fakeOrg(n string) string { return "00000000-0000-4000-8000-0000000000" + n }

// meterFor is a fresh 0.2.0 meter at now with no keys.
func meterFor(now time.Time) *qc.APIMeter {
	return &qc.APIMeter{
		Schema: qc.MeterSchema, Since: now.Add(-30 * 24 * time.Hour), StartedAt: now.Add(-time.Hour), FlushedAt: now.Add(-30 * time.Second),
		Gaps: []qc.MeterGap{}, Organizations: map[string]qc.MeterOrganization{}, Unlinked: []qc.MeterUnlinked{},
		Auths: map[string]qc.MeterAuth{}, AuthIDsSince: ptr(now.Add(-time.Hour)), AuthIDs: map[string]qc.MeterAuthID{},
	}
}

func orgIDs(orgs []Org) []string {
	out := []string{}
	for _, org := range orgs {
		out = append(out, org.ID)
	}
	return out
}

// Clean eligible organizations first, by soonest refill with no date last,
// then the order on Quota Cache's page, then account ID; demoted ones after
// them; the ineligible ones in document order, unranked.
func TestBuildRanksSoonestRefillFirst(t *testing.T) {
	now := at(t, "2026-10-11T12:00:00Z")
	soon, later := epoch(t, "2026-10-21T00:00:00Z"), epoch(t, "2026-11-01T00:00:00Z")

	out := account("org-000000000005", fakeOrg("05"), 0, soon)
	out.State, out.Left, out.LeftText = qc.CreditStateOut, 0, "$0.00"
	demoted := account("org-000000000002", fakeOrg("02"), 1, soon)
	demoted.DataIssues = []string{"claudeCodeRefused"}
	pending := account("org-000000000009", fakeOrg("09"), 9, nil)
	pending.State, pending.HasEstimate = qc.CreditStatePending, false
	stale := account("org-000000000006", fakeOrg("06"), 2, soon)
	stale.State = qc.CreditStateStale

	in := Input{Now: now, Meter: meterFor(now), Credits: credits(now,
		out,
		account("org-00000000000a", fakeOrg("0a"), 0, later), // clean, later refill
		demoted,
		account("org-000000000003", fakeOrg("03"), 2, nil), // clean, no refill date
		stale, // ineligible
		account("org-00000000000d", fakeOrg("0d"), 3, soon), // clean, soon, order 3
		account("org-000000000004", fakeOrg("04"), 3, soon), // clean, soon, order 3, smaller ID
		pending, // ineligible
		account("org-00000000000c", fakeOrg("0c"), 2, soon), // clean, soon, order 2
	)}
	table := Build(in)
	want := []string{
		"org-00000000000c", "org-000000000004", "org-00000000000d", "org-00000000000a", "org-000000000003",
		"org-000000000002",
		"org-000000000005", "org-000000000006", "org-000000000009",
	}
	if got := orgIDs(table.Orgs); !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v\nwant    %v", got, want)
	}
	for i, org := range table.Orgs {
		wantRank := i + 1
		if i >= 6 {
			wantRank = 0
		}
		if org.Rank != wantRank || org.Eligible != (i < 6) {
			t.Errorf("%s: rank %d eligible %v; want rank %d", org.ID, org.Rank, org.Eligible, wantRank)
		}
	}
	if !table.Orgs[5].Demoted {
		t.Fatal("the claudeCodeRefused organization is not demoted")
	}
}

// Every organization-reason row: what is eligible, demoted, re-checked, and
// what is copied for the status.
func TestBuildEligibility(t *testing.T) {
	now := at(t, "2026-10-11T12:00:00Z")
	renews := epoch(t, "2026-10-30T00:00:00Z")
	with := func(n string, change func(*qc.CreditAccount)) qc.CreditAccount {
		a := account("org-0000000000"+n, fakeOrg(n), 0, renews)
		change(&a)
		return a
	}
	cases := []struct {
		account                    qc.CreditAccount
		eligible, demoted, recheck bool
		reason, refusal            string
	}{
		{with("01", func(a *qc.CreditAccount) {}), true, false, false, "", ""},
		{with("02", func(a *qc.CreditAccount) {
			a.DataIssues, a.Issue = []string{"noTraffic", "claudeCodeRefused"}, "Anthropic refused requests from a Claude Code-based client."
		}), true, true, true, "claude_code_refused", "claudeCodeRefused"},
		{with("03", func(a *qc.CreditAccount) { a.HasEstimate, a.Left, a.LeftText = false, 0, "" }), false, false, false, "no_estimate", ""},
		{with("04", func(a *qc.CreditAccount) { a.Left, a.LeftText = 0, "$0.00" }), false, false, false, "no_credit_left", ""},
		{with("05", func(a *qc.CreditAccount) {
			a.State, a.Left, a.LeftText, a.EstimateLeftText = qc.CreditStateOut, 0, "$0.00", "$6.20"
			a.DataIssues, a.Issue = []string{"refusedNearlySpent"}, "Anthropic refused requests from a Claude Code-based client for low credit."
		}), false, false, true, "out", "refusedNearlySpent"},
		{with("06", func(a *qc.CreditAccount) {
			a.State, a.Left, a.LeftText, a.EstimateLeftText = qc.CreditStateOut, 0, "$0.00", "$0.00"
			a.DataIssues = []string{"refused", "claudeCodeRefused"}
		}), false, false, false, "out", "refused"},
		{with("07", func(a *qc.CreditAccount) {
			a.State, a.Left, a.LeftText, a.EstimateLeftText = qc.CreditStateOut, 0, "$0.00", ""
		}), false, false, false, "out", ""},
		{with("08", func(a *qc.CreditAccount) { a.State = qc.CreditStateStale }), false, false, false, "stale", ""},
		{with("09", func(a *qc.CreditAccount) { a.State = qc.CreditStateNeedsSettings }), false, false, false, "needs_settings", ""},
		{with("10", func(a *qc.CreditAccount) { a.State = qc.CreditStatePending }), false, false, false, "pending", ""},
		{with("11", func(a *qc.CreditAccount) { a.State = qc.CreditStateMisconfigured }), false, false, false, "misconfigured", ""},
		{with("12", func(a *qc.CreditAccount) { a.State = qc.CreditStateCacheTooOld }), false, false, false, "cache_too_old", ""},
		{with("13", func(a *qc.CreditAccount) { a.State = "somethingNew" }), false, false, false, "unknown_state", ""},
	}
	accounts := []qc.CreditAccount{}
	for _, c := range cases {
		accounts = append(accounts, c.account)
	}
	table := Build(Input{Now: now, Meter: meterFor(now), Credits: credits(now, accounts...)})
	byID := map[string]Org{}
	for _, org := range table.Orgs {
		byID[org.ID] = org
	}
	for _, c := range cases {
		org, ok := byID[c.account.ID]
		if !ok {
			t.Fatalf("%s is missing", c.account.ID)
		}
		if org.Eligible != c.eligible || org.Demoted != c.demoted || org.Recheck != c.recheck || org.Reason != c.reason || org.Refusal != c.refusal {
			t.Errorf("%s: eligible %v demoted %v recheck %v reason %q refusal %q; want %v %v %v %q %q", org.ID,
				org.Eligible, org.Demoted, org.Recheck, org.Reason, org.Refusal, c.eligible, c.demoted, c.recheck, c.reason, c.refusal)
		}
		if org.Issue != c.account.Issue || org.EstimateLeftText != c.account.EstimateLeftText || org.LeftText != c.account.LeftText || org.Label != c.account.Label {
			t.Errorf("%s: copied %+v from %+v", org.ID, org, c.account)
		}
		if org.RenewsAt == nil || org.RenewsAt.Unix() != *renews || org.RenewsAt.Location() != time.UTC {
			t.Errorf("%s: renews at %v", org.ID, org.RenewsAt)
		}
	}

	// One organization, one Org: of two accounts naming the same
	// organization, in any letter case, the first is kept, and an account
	// naming none is skipped.
	first := account("org-00000000002a", fakeOrg("2a"), 3, renews)
	first.State = qc.CreditStatePending
	second := account("org-00000000002b", strings.ToUpper(fakeOrg("2a")), 4, renews)
	unlinked := account("org-00000000002c", "", 5, renews)
	table = Build(Input{Now: now, Meter: meterFor(now), Credits: credits(now, first, second, unlinked)})
	if got := orgIDs(table.Orgs); !reflect.DeepEqual(got, []string{"org-00000000002a"}) {
		t.Fatalf("kept %v; want the first account", got)
	}
	if table.Orgs[0].Eligible || table.Orgs[0].Reason != "pending" {
		t.Fatalf("the kept account is %+v", table.Orgs[0])
	}
}

// keyed adds an auth ID to the meter, linked through index to org ("" for
// foreign, "-" for an index the meter does not know).
func keyed(m *qc.APIMeter, suffix, org string, seen time.Time, success *time.Time, failures uint16) {
	index := "00000000000000" + suffix
	m.AuthIDs[id(suffix)] = qc.MeterAuthID{AuthIndex: index, SeenAt: seen, LastSuccessAt: success, Failures: failures}
	if org != "-" {
		m.Auths[index] = qc.MeterAuth{OrganizationID: org, SeenAt: seen}
	}
}

func TestBuildMapsKeys(t *testing.T) {
	f := worked(t)
	m := &f.Meter
	seen := at(t, "2026-10-11T11:00:00Z")
	keyed(m, "20", fakeOrg("0b"), seen, nil, 0)                                   // bravo, never succeeded: failing
	keyed(m, "21", fakeOrg("99"), seen, &seen, 0)                                 // an organization no item names: unlinked
	keyed(m, "22", fakeOrg("0e"), f.Now.Add(-RecentKeyAge-time.Second), &seen, 0) // echo, too long ago: stale
	keyed(m, "23", fakeOrg("0e"), f.Now.Add(-RecentKeyAge), &seen, 0)             // echo, just within: counted
	keyed(m, "24", fakeOrg("0a"), seen, &seen, 2)                                 // alpha, out, two failures since: ineligible, not proven
	m.AuthIDs["claude:apikey:00000000000G"] = qc.MeterAuthID{AuthIndex: "000000000000000b", SeenAt: seen}
	m.AuthIDs["AUTHID-CANARY.json"] = qc.MeterAuthID{AuthIndex: "000000000000000b", SeenAt: seen}

	table := Build(f.input())
	// An ineligible organization's keys keep whether they are proven, which
	// its re-check reads.
	want := map[string]key{
		id("0a"): {class: classIneligible, proven: true, org: 4, seen: at(t, "2026-10-11T10:00:00Z").Unix()},
		id("0b"): {class: classRanked, proven: true, rank: 1, org: 0, seen: at(t, "2026-10-11T11:50:00Z").Unix()},
		id("0c"): {class: classIneligible, proven: true, org: 5, seen: at(t, "2026-10-11T10:00:00Z").Unix()},
		id("0d"): {class: classRanked, proven: true, rank: 2, org: 1, seen: at(t, "2026-10-11T11:40:00Z").Unix()},
		id("0f"): {class: classRanked, proven: true, rank: 3, org: 2, seen: at(t, "2026-10-11T11:30:00Z").Unix()},
		id("10"): {class: classRanked, proven: true, rank: 3, org: 2, seen: at(t, "2026-10-11T11:20:00Z").Unix()},
		id("11"): {class: classRanked, proven: true, rank: 4, org: 3, seen: at(t, "2026-10-11T11:10:00Z").Unix()},
		id("12"): {class: classUnmapped, org: -1, seen: at(t, "2026-10-11T10:30:00Z").Unix()},
		id("13"): {class: classForeign, org: -1, seen: seen.Unix()},
		id("20"): {class: classFailing, rank: 1, org: 0, seen: seen.Unix()},
		id("21"): {class: classUnlinked, org: -1, seen: seen.Unix()},
		id("22"): {class: classRanked, proven: true, rank: 3, org: 2, seen: f.Now.Add(-RecentKeyAge - time.Second).Unix()},
		id("23"): {class: classRanked, proven: true, rank: 3, org: 2, seen: f.Now.Add(-RecentKeyAge).Unix()},
		id("24"): {class: classIneligible, org: 4, seen: seen.Unix()},
	}
	if !reflect.DeepEqual(table.keys, want) {
		t.Fatalf("keys = %+v\nwant   %+v", table.keys, want)
	}
	if got := orgIDs(table.Orgs); !reflect.DeepEqual(got, []string{
		"org-bbbbbbbbbbbb", "org-dddddddddddd", "org-eeeeeeeeeeee", "org-ffffffffffff", "org-aaaaaaaaaaaa", "org-cccccccccccc",
	}) {
		t.Fatalf("orgs = %v", got)
	}
	// 0a 0b 0c 0d 0f 10 11 20 23 24 are mapped; 22 is only stale. 24 is not
	// proven, but only an eligible organization's keys count as failing.
	if table.MappedKeys != 10 || table.FailingKeys != 1 || table.UnmappedKeys != 1 || table.ForeignKeys != 1 ||
		table.UnlinkedKeys != 1 || table.StaleKeys != 1 {
		t.Fatalf("counts mapped %d failing %d unmapped %d foreign %d unlinked %d stale %d", table.MappedKeys,
			table.FailingKeys, table.UnmappedKeys, table.ForeignKeys, table.UnlinkedKeys, table.StaleKeys)
	}
	keys := map[string][2]int{}
	for _, org := range table.Orgs {
		keys[org.Label] = [2]int{org.Keys, org.FailingKeys}
	}
	if want := map[string][2]int{"alpha": {2, 0}, "bravo": {2, 1}, "charlie": {1, 0}, "delta": {1, 0}, "echo": {3, 0}, "foxtrot": {1, 0}}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("org keys = %v; want %v", keys, want)
	}
}

// Changing a key's prefix, proxy or headers gives it a new auth ID but keeps
// its auth index, so the old ID stays linked to its organization and counts
// until it has been idle for RecentKeyAge. Right after the change the counts
// still read one key, though the only ID CPA now offers is unknown and is
// explored; once the new ID serves they read two.
func TestBuildCountsAChangedKeysOldID(t *testing.T) {
	now := at(t, "2026-10-11T12:00:00Z")
	in := Input{Now: now, Meter: meterFor(now),
		Credits: credits(now, account("org-bbbbbbbbbbbb", fakeOrg("0b"), 0, epoch(t, "2026-10-21T00:00:00Z")))}
	last := now.Add(-time.Hour)
	keyed(in.Meter, "0b", fakeOrg("0b"), last, &last, 0) // its last request before the change
	counts := func() [4]int {
		table := Build(in)
		return [4]int{table.MappedKeys, table.UnmappedKeys, table.FailingKeys, table.StaleKeys}
	}
	if got := counts(); got != [4]int{1, 0, 0, 0} {
		t.Fatalf("after the change: mapped, unmapped, failing, stale = %v", got)
	}
	expect(t, routing(t, in), request(t, "", "1b"), now, explored("1b"))

	served := now.Add(time.Minute)
	in.Meter.AuthIDs[id("1b")] = qc.MeterAuthID{AuthIndex: in.Meter.AuthIDs[id("0b")].AuthIndex, SeenAt: served, LastSuccessAt: &served}
	if got := counts(); got != [4]int{2, 0, 0, 0} {
		t.Fatalf("once the new ID served: mapped, unmapped, failing, stale = %v", got)
	}
	expect(t, routing(t, in), request(t, "", "1b"), now.Add(2*time.Minute), routed("1b"))

	in.Now = last.Add(RecentKeyAge + time.Second)
	in.Meter.FlushedAt = in.Now.Add(-30 * time.Second)
	in.Credits.ComputedAt = in.Now
	if got := counts(); got != [4]int{1, 0, 0, 1} {
		t.Fatalf("7 days after the old ID's last request: mapped, unmapped, failing, stale = %v", got)
	}
}

// README's first-use steps set the credits group's prefix before the status
// check that every key is known, since a prefix change leaves the old IDs
// counting (above). They also say what else holds that check open: a key that
// fails every request, and an old ID that has not moved to stale_keys yet.
func TestReadmeSetsThePrefixBeforeTheKeyCheck(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	section, found := section(string(readme), "Spend the credit that refills soonest (optional)")
	if !found {
		t.Fatal(`README.md has no "Spend the credit that refills soonest (optional)" section`)
	}
	section = strings.Join(strings.Fields(section), " ")
	prefix, check := strings.Index(section, "`prefix`"), strings.Index(section, "`router.unmapped_keys`")
	if prefix < 0 || check < 0 || prefix > check {
		t.Errorf("README.md's first-use steps check router.unmapped_keys (at %d) before they set the prefix (at %d)", check, prefix)
	}
	for _, want := range []string{"remove it from CPA's configuration", "`router.stale_keys`"} {
		if !strings.Contains(section, want) {
			t.Errorf("README.md's first-use steps never say %q", want)
		}
	}
}

func TestBuildIsPure(t *testing.T) {
	f := worked(t)
	in := f.input()
	first, second := Build(in), Build(in)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("two builds of one input differ:\n%+v\n%+v", first, second)
	}
	if len(first.keys) == 0 || len(first.Orgs) == 0 {
		t.Fatal("the worked example built an empty table")
	}
}

// Only a key that has succeeded, and has not failed twice since, is ever the
// ranked choice. A key that answers every request with a 400 CPA does not
// fail over is mapped (the 400 carried the organization header) and never
// succeeded, so it is explored with backoff, never routed to.
func TestBuildRanksOnlyProvenKeys(t *testing.T) {
	f := worked(t)
	m := &f.Meter
	seen := at(t, "2026-10-11T11:55:00Z")
	keyed(m, "20", fakeOrg("0b"), seen, nil, 0)     // bravo, no success yet
	keyed(m, "21", fakeOrg("0b"), seen, &seen, 2)   // bravo, two failures since
	keyed(m, "22", fakeOrg("0b"), seen, &seen, 1)   // bravo, one failure since: still proven
	keyed(m, "24", fakeOrg("0b"), seen, nil, 65535) // bravo, the invalid_request_error 400 on every request
	failure := at(t, "2026-10-11T11:55:00Z")
	e := m.AuthIDs[id("24")]
	e.LastFailureAt = &failure
	m.AuthIDs[id("24")] = e

	table := Build(f.input())
	for suffix, want := range map[string]keyClass{"20": classFailing, "21": classFailing, "22": classRanked, "24": classFailing} {
		if got := table.keys[id(suffix)].class; got != want {
			t.Errorf("%s is class %d; want %d", suffix, got, want)
		}
	}

	r := New()
	r.SetEnabled(true)
	r.Publish(table, f.Now)
	now := f.Now.Add(42 * time.Second)
	// 20, 21 and 24 sort before 0d's org in rank, but are never routed:
	// each is explored once, then 22, the proven bravo key, is the choice.
	offered := []string{"0d", "20", "21", "22", "24"}
	for _, want := range []Decision{explored("20"), explored("21"), explored("24"), routed("22"), routed("22")} {
		expect(t, r, request(t, "", offered...), now, want)
	}
	// A failover pick with a ranked candidate never explores; without the
	// bravo keys the delta key is the choice.
	expect(t, r, request(t, id("22"), "0d", "20", "21", "24"), now, routed("0d"))

	// Only failing candidates: one exploration while a slot is free, then
	// no_proven.
	r = New()
	r.SetEnabled(true)
	r.Publish(table, f.Now)
	expect(t, r, request(t, "", "24"), now, explored("24"))
	expect(t, r, request(t, "", "24"), now, decline(ReasonNoProven))
	expect(t, r, request(t, id("24"), "20", "24"), now, explored("20"))
	expect(t, r, request(t, "", "20", "24"), now, decline(ReasonNoProven))
}
