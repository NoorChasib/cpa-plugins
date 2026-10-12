package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// The names, limits and sentences are written into the credit state file and
// read by other plugins at their own client tag, so their values are the
// contract.
func TestCreditConstants(t *testing.T) {
	for _, tc := range []struct {
		name      string
		got, want any
	}{
		{"CreditsSchema", CreditsSchema, 1},
		{"MaxCreditsBytes", MaxCreditsBytes, 1 << 20},
		{"DefaultCreditStaleAfter", DefaultCreditStaleAfter, 45 * time.Minute},
		{"CreditBoundaryGrace", CreditBoundaryGrace, 2 * time.Minute},
		{"CreditPublishLagLimit", CreditPublishLagLimit, 5 * time.Minute},
		{"CreditLevelCriticalAtOrBelow", CreditLevelCriticalAtOrBelow, 10},
		{"CreditLevelLowAtOrBelow", CreditLevelLowAtOrBelow, 40},
		{"CreditSentenceStale", CreditSentenceStale, "Quota Cache has not saved its meter recently, so recent spend may be missing."},
		{"CreditSentenceMeterGap", CreditSentenceMeterGap, "Quota Cache was not counting for part of this period, for example while it was off or reloading, so some spend may be missing. Enter a Console reading to correct it."},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
	want := []string{
		"misconfigured", "cacheTooOld", "meterMissing", "orgNotCounted", "needsCredit", "needsRefillDate", "refused",
		"refusedNearlySpent", "stale", "meterStartedLate", "meterGap", "meterDropped", "meterFull", "meterUnattributed",
		"meterRejected", "unpricedModel", "zeroCredit", "overCredit", "readingAboveCredit", "readingOnRefillDay",
		"readingUnused", "claudeCodeRefused", "noTraffic", "configMonthlyUsdInvalid", "configRenewsInvalid", "adminKeyIgnored",
	}
	if !slices.Equal(CreditIssueOrder, want) {
		t.Errorf("CreditIssueOrder = %v", CreditIssueOrder)
	}
}

func TestCreditsPaths(t *testing.T) {
	for in, want := range map[string][2]string{
		DefaultPath:                          {"plugins/data/quota-cache/snapshot.credits.json", "plugins/data/quota-cache/snapshot.organizations.json"},
		"/data/cache.json":                   {"/data/cache.credits.json", "/data/cache.organizations.json"},
		"/data/snapshot":                     {"/data/snapshot.credits.json", "/data/snapshot.organizations.json"},
		"/data/snapshot.json.bak":            {"/data/snapshot.json.bak.credits.json", "/data/snapshot.json.bak.organizations.json"},
		"/CLIProxyAPI/plugins/data/q/s.json": {"/CLIProxyAPI/plugins/data/q/s.credits.json", "/CLIProxyAPI/plugins/data/q/s.organizations.json"},
	} {
		if got := CreditsPath(in); got != want[0] {
			t.Errorf("CreditsPath(%q) = %q, want %q", in, got, want[0])
		}
		if got := OrganizationsPath(in); got != want[1] {
			t.Errorf("OrganizationsPath(%q) = %q, want %q", in, got, want[1])
		}
	}
}

func epochOf(t time.Time) *int64 {
	e := t.Unix()
	return &e
}

func textOf(s string) *string { return &s }

// creditT is the instant sampleState is computed at.
var creditT = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// sampleState is a published state at creditT, valid for two days, with one
// account of each kind the overlay treats differently: anchored and ok with no
// issue, anchored and ok already a lower bound, needing settings, pending, and
// out.
func sampleState() *CreditState {
	until := creditT.Add(48 * time.Hour)
	flushed := creditT.Add(-time.Minute)
	anchor := epochOf(creditT.Add(-10 * 24 * time.Hour))
	renews := epochOf(creditT.Add(24 * time.Hour))
	in := int64(24 * 3600)
	account := func(id, state string, anchored, counted bool, issues ...string) CreditAccount {
		a := CreditAccount{ID: id, Label: id, State: state, Counted: counted, DataIssues: append([]string{}, issues...), Unpriced: []CreditUnpriced{},
			RenewsAtEpoch: renews, RenewsInSeconds: &in}
		if anchored {
			a.SpentSinceEpoch = anchor
		}
		if len(issues) > 0 {
			a.Issue = "sentence for " + issues[0]
		}
		return a
	}
	lowerBound := account("org-00000000000b", CreditStateOK, true, true, CreditIssueMeterStartedLate)
	lowerBound.LowerBound = true
	full := epochOf(creditT.Add(72 * time.Hour))
	fullIn := int64(72 * 3600)
	return &CreditState{
		Schema: CreditsSchema, QuotaCacheVersion: "0.2.0", ComputedAt: creditT, ValidUntil: &until,
		Inputs:   CreditInputs{SnapshotWrittenAt: &creditT, MeterFlushedAt: &flushed},
		Settings: CreditSettingsState{State: CreditSettingsOK, Editable: true, Revision: "3"},
		APICredits: &CreditDocument{
			Title: "Monthly API Credit", Currency: "USD",
			Meter: &CreditMeter{SinceEpoch: creditT.Add(-40 * 24 * time.Hour).Unix(), UpdatedAtEpoch: flushed.Unix(), Gaps: []CreditGap{}},
			Pool: CreditPool{HasEstimate: true, LowerBound: true, AccountCount: 5, CountedCount: 3, MissingCount: 2,
				NextRefill:  &CreditRefill{AccountIDs: []string{"org-00000000000a"}, RefillAtEpoch: *renews, RefillInSeconds: in},
				FullAtEpoch: full, FullInSeconds: &fullIn},
			Accounts: []CreditAccount{
				account("org-00000000000a", CreditStateOK, true, true),
				lowerBound,
				account("org-00000000000c", CreditStateNeedsSettings, false, false, CreditIssueNeedsCredit),
				account("org-00000000000d", CreditStatePending, false, false, CreditIssueOrgNotCounted),
				account("org-00000000000e", CreditStateOut, true, true, CreditIssueRefused),
			},
			Unlinked: []CreditUnlinked{}, Orphans: []CreditOrphan{},
		},
		Renewals:      map[string]RenewalSetting{},
		Organizations: CreditOrganizations{State: OrganizationsStateOK, Source: OrganizationsSourcePage},
	}
}

func encoded(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// sameAccount compares two accounts but for the countdown, which CreditsAt
// always runs to its instant.
func sameAccount(a, b CreditAccount) bool {
	a.RenewsInSeconds, b.RenewsInSeconds = nil, nil
	return reflect.DeepEqual(a, b)
}

// creditsAt judges s at now and checks that s itself was left alone.
func creditsAt(t *testing.T, s *CreditState, now time.Time, staleAfter time.Duration) *CreditDocument {
	t.Helper()
	before := encoded(t, s)
	got := CreditsAt(s, now, staleAfter)
	if encoded(t, s) != before {
		t.Fatal("CreditsAt modified the state it was given")
	}
	return got.APICredits
}

// Each rule of the overlay alone.
func TestCreditsAtRules(t *testing.T) {
	// At the instant it was computed, nothing but the countdowns is judged,
	// and they agree.
	if got := creditsAt(t, sampleState(), creditT, DefaultCreditStaleAfter); !reflect.DeepEqual(got, sampleState().APICredits) {
		t.Errorf("fresh: %s", encoded(t, got))
	}

	staleAccounts := func(name string, got *CreditDocument) {
		t.Helper()
		a, b, c, d, e := got.Accounts[0], got.Accounts[1], got.Accounts[2], got.Accounts[3], got.Accounts[4]
		if !got.Meter.Stale {
			t.Errorf("%s: the meter is not marked stale", name)
		}
		if a.State != CreditStateStale || !a.LowerBound || !slices.Equal(a.DataIssues, []string{CreditIssueStale}) || a.Issue != CreditSentenceStale {
			t.Errorf("%s: an ok account: %s", name, encoded(t, a))
		}
		if b.State != CreditStateStale || !slices.Equal(b.DataIssues, []string{CreditIssueStale, CreditIssueMeterStartedLate}) || b.Issue != CreditSentenceStale {
			t.Errorf("%s: stale placed first: %s", name, encoded(t, b))
		}
		if c.State != CreditStateNeedsSettings || c.LowerBound || !slices.Equal(c.DataIssues, []string{CreditIssueNeedsCredit, CreditIssueStale}) ||
			c.Issue != "sentence for needsCredit" {
			t.Errorf("%s: an account that needs settings: %s", name, encoded(t, c))
		}
		if !sameAccount(d, sampleState().APICredits.Accounts[3]) {
			t.Errorf("%s: a pending account changed: %s", name, encoded(t, d))
		}
		if e.State != CreditStateOut || !e.LowerBound || !slices.Equal(e.DataIssues, []string{CreditIssueRefused, CreditIssueStale}) || e.Issue != "sentence for refused" {
			t.Errorf("%s: an out account: %s", name, encoded(t, e))
		}
		if !got.Pool.LowerBound || got.Pool.Left != 0 || got.Pool.CountedCount != 3 {
			t.Errorf("%s: pool %s", name, encoded(t, got.Pool))
		}
	}

	// Stale by the meter's age.
	staleAccounts("meter age", creditsAt(t, sampleState(), creditT.Add(44*time.Minute+time.Second), DefaultCreditStaleAfter))
	if got := creditsAt(t, sampleState(), creditT.Add(44*time.Minute), DefaultCreditStaleAfter); got.Meter.Stale || got.Accounts[0].State != CreditStateOK {
		t.Errorf("a meter saved exactly stale-after ago is stale: %s", encoded(t, got.Accounts[0]))
	}
	// now is taken to the second, as the publisher takes it.
	if got := creditsAt(t, sampleState(), creditT.Add(44*time.Minute+999*time.Millisecond), DefaultCreditStaleAfter); got.Meter.Stale {
		t.Error("a fraction of a second past stale-after is stale")
	}

	// Stale by the state's own age, with a meter saved since.
	s := sampleState()
	s.APICredits.Meter.UpdatedAtEpoch = creditT.Add(40 * time.Minute).Unix()
	staleAccounts("computed_at age", creditsAt(t, s, creditT.Add(45*time.Minute+time.Second), DefaultCreditStaleAfter))
	if got := creditsAt(t, s, creditT.Add(45*time.Minute), DefaultCreditStaleAfter); got.Meter.Stale {
		t.Error("a state exactly stale-after old is stale")
	}

	// Stale by valid_until plus the grace, even with stale-after off.
	staleAccounts("valid_until", creditsAt(t, sampleState(), sampleState().ValidUntil.Add(CreditBoundaryGrace), 0))
	if got := creditsAt(t, sampleState(), sampleState().ValidUntil.Add(CreditBoundaryGrace-time.Second), 0); got.Meter.Stale {
		t.Error("stale before valid_until plus the grace")
	}

	// stale-after 0 turns both age rules off.
	if got := creditsAt(t, sampleState(), creditT.Add(40*time.Hour), 0); got.Meter.Stale || got.Accounts[0].State != CreditStateOK {
		t.Errorf("stale-after 0: %s", encoded(t, got.Accounts[0]))
	}

	// Expired with no meter: the accounts that reach the stale rule are stale.
	s = sampleState()
	s.APICredits.Meter = nil
	if got := creditsAt(t, s, creditT.Add(46*time.Minute), DefaultCreditStaleAfter); got.Meter != nil || got.Accounts[0].State != CreditStateStale {
		t.Errorf("expired without a meter: %s", encoded(t, got.Accounts[0]))
	}

	// A meter stopped five minutes ago leaves a gap in every anchored account.
	s = sampleState()
	s.APICredits.Meter.StoppedAtEpoch, s.APICredits.Meter.StopReason = epochOf(creditT), MeterStopQuiesce
	if got := creditsAt(t, s, creditT.Add(MeterBriefGap-time.Second), 0); !slices.EqualFunc(got.Accounts, s.APICredits.Accounts, sameAccount) {
		t.Errorf("a gap before five minutes: %s", encoded(t, got.Accounts))
	}
	got := creditsAt(t, s, creditT.Add(MeterBriefGap), 0)
	a, b, c, e := got.Accounts[0], got.Accounts[1], got.Accounts[2], got.Accounts[4]
	if a.State != CreditStateOK || !a.LowerBound || !slices.Equal(a.DataIssues, []string{CreditIssueMeterGap}) || a.Issue != CreditSentenceMeterGap {
		t.Errorf("gap, first: %s", encoded(t, a))
	}
	if !slices.Equal(b.DataIssues, []string{CreditIssueMeterStartedLate, CreditIssueMeterGap}) || b.Issue != "sentence for meterStartedLate" {
		t.Errorf("gap, not first: %s", encoded(t, b))
	}
	if !sameAccount(c, s.APICredits.Accounts[2]) {
		t.Errorf("gap on an account with no anchor: %s", encoded(t, c))
	}
	if !e.LowerBound || !slices.Equal(e.DataIssues, []string{CreditIssueRefused, CreditIssueMeterGap}) || e.State != CreditStateOut {
		t.Errorf("gap, out: %s", encoded(t, e))
	}
	if got.Meter.Stale || !got.Pool.LowerBound {
		t.Errorf("gap: meter %s pool %s", encoded(t, got.Meter), encoded(t, got.Pool))
	}

	// Both: stale goes first, and its sentence.
	got = creditsAt(t, s, creditT.Add(time.Hour), DefaultCreditStaleAfter)
	if a := got.Accounts[0]; !slices.Equal(a.DataIssues, []string{CreditIssueStale, CreditIssueMeterGap}) || a.Issue != CreditSentenceStale {
		t.Errorf("stale and gap: %s", encoded(t, a))
	}

	// The pool is a lower bound when a counted account is, and only then.
	s = sampleState()
	s.APICredits.Accounts[1].LowerBound = false
	if got := creditsAt(t, s, creditT, 0); got.Pool.LowerBound {
		t.Error("the pool is a lower bound with no counted account one")
	}
	s.APICredits.Accounts[2].LowerBound = true // not counted
	if got := creditsAt(t, s, creditT, 0); got.Pool.LowerBound {
		t.Error("an account that is not counted made the pool a lower bound")
	}

	// The countdowns run down to now and stop at zero.
	got = creditsAt(t, sampleState(), creditT.Add(25*time.Hour), 0)
	if *got.Accounts[0].RenewsInSeconds != 0 || got.Pool.NextRefill.RefillInSeconds != 0 || *got.Pool.FullInSeconds != 47*3600 {
		t.Errorf("countdowns: renews %d refill %d full %d", *got.Accounts[0].RenewsInSeconds, got.Pool.NextRefill.RefillInSeconds, *got.Pool.FullInSeconds)
	}
	got = creditsAt(t, sampleState(), creditT.Add(90*time.Minute+500*time.Millisecond), 0)
	if *got.Accounts[0].RenewsInSeconds != 24*3600-5400 || *got.Accounts[0].RenewsAtEpoch != *sampleState().APICredits.Accounts[0].RenewsAtEpoch {
		t.Errorf("countdown: %d", *got.Accounts[0].RenewsInSeconds)
	}
	s = sampleState()
	s.APICredits.Accounts[0].RenewsAtEpoch, s.APICredits.Accounts[0].RenewsInSeconds = nil, nil
	s.APICredits.Pool.NextRefill, s.APICredits.Pool.FullAtEpoch, s.APICredits.Pool.FullInSeconds = nil, nil, nil
	if got := creditsAt(t, s, creditT.Add(time.Hour), 0); got.Accounts[0].RenewsInSeconds != nil || got.Pool.FullInSeconds != nil {
		t.Error("a countdown appeared with no epoch to count down to")
	}

	// Nothing to judge.
	if CreditsAt(nil, creditT, 0) != nil {
		t.Error("CreditsAt(nil) is not nil")
	}
	s = sampleState()
	s.APICredits = nil
	if got := CreditsAt(s, creditT.Add(100*time.Hour), DefaultCreditStaleAfter); got == s || encoded(t, got) != encoded(t, s) {
		t.Errorf("no api_credits: %s", encoded(t, got))
	}
}

// A code this reader does not know is kept where it was: never compared,
// moved or dropped. The issue sentence changes only when the added code lands
// first.
func TestCreditsAtInsertionRule(t *testing.T) {
	const unknown, other = "fromANewerQuotaCache", "alsoNew"
	for _, tc := range []struct {
		issues, want []string
		replaced     bool
	}{
		{[]string{unknown, CreditIssueNeedsCredit}, []string{unknown, CreditIssueNeedsCredit, CreditIssueStale}, false},
		{[]string{unknown, CreditIssueMeterStartedLate}, []string{unknown, CreditIssueStale, CreditIssueMeterStartedLate}, false},
		{[]string{CreditIssueMeterStartedLate, unknown}, []string{CreditIssueStale, CreditIssueMeterStartedLate, unknown}, true},
		{[]string{CreditIssueNeedsCredit, unknown, CreditIssueNoTraffic, other}, []string{CreditIssueNeedsCredit, unknown, CreditIssueStale, CreditIssueNoTraffic, other}, false},
		{[]string{unknown}, []string{unknown, CreditIssueStale}, false},
		{[]string{CreditIssueStale, unknown}, []string{CreditIssueStale, unknown}, false},
		{[]string{}, []string{CreditIssueStale}, true},
	} {
		s := sampleState()
		s.APICredits.Accounts[0].DataIssues = tc.issues
		s.APICredits.Accounts[0].Issue = "the published sentence"
		got := creditsAt(t, s, creditT.Add(time.Hour), DefaultCreditStaleAfter).Accounts[0]
		want := "the published sentence"
		if tc.replaced {
			want = CreditSentenceStale
		}
		if !slices.Equal(got.DataIssues, tc.want) || got.Issue != want {
			t.Errorf("%v: got %v %q, want %v %q", tc.issues, got.DataIssues, got.Issue, tc.want, want)
		}
	}
}

// fullState has every pointer, slice and map of a state set, so a copy that
// shares any of them is caught.
func fullState() *CreditState {
	s := sampleState()
	at := creditT.Add(-time.Hour)
	s.Settings.Import = &CreditImport{State: CreditImportStateImported, Kind: textOf(CreditImportKindAutomatic), SourcePath: textOf("/data/settings.json"),
		SHA256: textOf("aa"), SettingsSHA256: textOf("bb"), LegacyRevision: textOf("12"), LegacyUpdatedAt: &at,
		Counts: &CreditImportCounts{MonthlyCredits: 1}, ImportedAt: &at}
	s.Renewals["5f2b8c41d09e7a36"] = RenewalSetting{Date: "2026-09-10", Revision: "3", UpdatedAtEpoch: at.Unix()}
	s.Organizations.Migration = &OrganizationsMigration{At: at, Moved: 5}
	d := s.APICredits
	d.Meter.StoppedAtEpoch, d.Meter.LastDroppedEpoch, d.Meter.LastUnattributedEpoch, d.Meter.LastRejectedEpoch = epochOf(creditT), epochOf(at), epochOf(at), epochOf(at)
	d.Meter.Gaps = []CreditGap{{FromEpoch: 1, ToEpoch: 2, Reason: MeterStopShutdown}}
	d.Unlinked = []CreditUnlinked{{OrganizationID: "x"}}
	d.Orphans = []CreditOrphan{{ID: "org-000000000001"}}
	for i := range d.Accounts {
		a := &d.Accounts[i]
		a.SpentSinceEpoch, a.CycleStartEpoch, a.MeterSinceEpoch, a.LastSeenEpoch = epochOf(at), epochOf(at), epochOf(at), epochOf(at)
		a.Reading = &CreditReading{Remaining: 1}
		a.Unpriced = []CreditUnpriced{{Model: "m", Tokens: 1}}
		a.Refusals.LastAtEpoch, a.Refusals.ClaudeCodeLastAtEpoch = epochOf(at), epochOf(at)
		a.Settings.Reading, a.Settings.UpdatedAtEpoch = &StoredReading{RemainingUSD: "1"}, epochOf(at)
		a.DataIssues = append(a.DataIssues, CreditIssueNoTraffic)
	}
	return s
}

// shared walks a and b together and reports the first pointer, slice or map
// they share, by path.
func shared(a, b reflect.Value, path string) string {
	if a.Type() == reflect.TypeFor[time.Time]() {
		return ""
	}
	switch a.Kind() {
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return ""
		}
		if a.Pointer() == b.Pointer() {
			return path
		}
		return shared(a.Elem(), b.Elem(), path)
	case reflect.Slice:
		if a.Len() > 0 && b.Len() > 0 && a.Pointer() == b.Pointer() {
			return path
		}
		for i := range min(a.Len(), b.Len()) {
			if p := shared(a.Index(i), b.Index(i), path+"[]"); p != "" {
				return p
			}
		}
	case reflect.Map:
		if !a.IsNil() && !b.IsNil() && a.Pointer() == b.Pointer() {
			return path
		}
		for _, key := range a.MapKeys() {
			if bv := b.MapIndex(key); bv.IsValid() {
				if p := shared(a.MapIndex(key), bv, path+"{}"); p != "" {
					return p
				}
			}
		}
	case reflect.Struct:
		for i := range a.NumField() {
			if p := shared(a.Field(i), b.Field(i), path+"."+a.Type().Field(i).Name); p != "" {
				return p
			}
		}
	}
	return ""
}

// CreditsAt's result shares nothing with its input, so a reader may change it
// freely and a cached state stays as published.
func TestCreditsAtReturnsADeepCopy(t *testing.T) {
	s := fullState()
	got := CreditsAt(s, creditT, 0)
	if path := shared(reflect.ValueOf(s), reflect.ValueOf(got), "state"); path != "" {
		t.Fatalf("the copy shares %s with the state", path)
	}
	if !reflect.DeepEqual(got, s) {
		t.Fatalf("the copy differs:\n got %s\nwant %s", encoded(t, got), encoded(t, s))
	}
	// Every field of the state is set here, so a field added to a type and
	// forgotten in the copy is caught.
	var unset func(v reflect.Value, path string)
	unset = func(v reflect.Value, path string) {
		if v.Type() == reflect.TypeFor[time.Time]() {
			return
		}
		switch v.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Map:
			if v.IsNil() {
				t.Errorf("fullState leaves %s nil", path)
				return
			}
		}
		switch v.Kind() {
		case reflect.Pointer:
			unset(v.Elem(), path)
		case reflect.Slice:
			for i := range v.Len() {
				unset(v.Index(i), path+"[]")
			}
		case reflect.Struct:
			for i := range v.NumField() {
				unset(v.Field(i), path+"."+v.Type().Field(i).Name)
			}
		}
	}
	unset(reflect.ValueOf(s), "state")
}

func TestCreditStateCurrent(t *testing.T) {
	s := sampleState()
	for _, tc := range []struct {
		now    time.Time
		maxAge time.Duration
		want   bool
	}{
		{creditT, 3 * time.Minute, true},
		{creditT.Add(3 * time.Minute), 3 * time.Minute, true},
		{creditT.Add(3*time.Minute + time.Second), 3 * time.Minute, false},
		{s.ValidUntil.Add(-time.Second), 72 * time.Hour, true},
		{*s.ValidUntil, 72 * time.Hour, false},
	} {
		if got := s.Current(tc.now, tc.maxAge); got != tc.want {
			t.Errorf("Current(%s, %v) = %v", tc.now, tc.maxAge, got)
		}
	}
	s.ValidUntil = nil
	if !s.Current(creditT.Add(100*time.Hour), 101*time.Hour) {
		t.Error("a state with no valid_until expired")
	}
	if (*CreditState)(nil).Current(creditT, time.Hour) {
		t.Error("a nil state is current")
	}
}

func writeCredits(t *testing.T, raw []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snapshot.credits.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadCredits(t *testing.T) {
	if _, err := LoadCredits(filepath.Join(t.TempDir(), "absent.json")); !errors.Is(err, ErrCreditsMissing) {
		t.Errorf("missing: %v", err)
	}
	full := fullState()
	raw, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadCredits(writeCredits(t, raw))
	if err != nil || encoded(t, got) != string(raw) {
		t.Errorf("round trip: %v\n got %s\nwant %s", err, encoded(t, got), raw)
	}

	target := writeCredits(t, raw)
	link := filepath.Join(t.TempDir(), "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	padded := append(bytes.Repeat([]byte(" "), MaxCreditsBytes), raw...)
	for name, path := range map[string]string{
		"a symlink":     link,
		"a directory":   t.TempDir(),
		"over 1 MiB":    writeCredits(t, padded),
		"not JSON":      writeCredits(t, []byte("{")),
		"trailing data": writeCredits(t, append(append([]byte{}, raw...), []byte(" {}")...)),
		"schema 2":      writeCredits(t, []byte(`{"schema":2}`)),
		"no schema":     writeCredits(t, []byte(`{}`)),
		"wrong type":    writeCredits(t, []byte(`{"schema":1,"api_credits":[]}`)),
	} {
		if _, err := LoadCredits(path); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Exactly the limit still loads.
	exact := append(bytes.Repeat([]byte(" "), MaxCreditsBytes-len(raw)), raw...)
	if _, err := LoadCredits(writeCredits(t, exact)); err != nil {
		t.Errorf("a file of exactly MaxCreditsBytes: %v", err)
	}
	// Unknown keys are a later quota-cache's additions, and are ignored.
	if _, err := LoadCredits(writeCredits(t, []byte(`{"schema":1,"a_later_field":{"x":1},"renewals":{}}`))); err != nil {
		t.Errorf("an additive field: %v", err)
	}
}

// A file that holds null where a reader ranges is filled, so a reader never
// checks.
func TestLoadCreditsFillsNilMapsAndSlices(t *testing.T) {
	raw := []byte(`{"schema":1,"renewals":null,"api_credits":{"meter":{"gaps":null},"pool":{"nextRefill":{"accountIds":null}},` +
		`"accounts":[{"id":"org-00000000000a","dataIssues":null,"unpriced":null}],"unlinked":null,"orphans":null}}`)
	got, err := LoadCredits(writeCredits(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	d := got.APICredits
	if got.Renewals == nil || d.Meter.Gaps == nil || d.Pool.NextRefill.AccountIDs == nil || d.Accounts[0].DataIssues == nil ||
		d.Accounts[0].Unpriced == nil || d.Unlinked == nil || d.Orphans == nil {
		t.Errorf("not filled: %s", encoded(t, got))
	}
	got, err = LoadCredits(writeCredits(t, []byte(`{"schema":1,"api_credits":{}}`)))
	if err != nil || got.Renewals == nil || got.APICredits.Accounts == nil || got.APICredits.Meter != nil {
		t.Errorf("an empty card: %v %s", err, encoded(t, got))
	}
}

// keysOf lists the keys of the JSON object at path in raw, in order.
func keysOf(t *testing.T, raw []byte, path ...string) []string {
	t.Helper()
	var object json.RawMessage = raw
	for _, key := range path {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(object, &fields); err != nil {
			t.Fatal(err)
		}
		object = fields[key]
	}
	decoder := json.NewDecoder(bytes.NewReader(object))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		t.Fatalf("%v is not an object: %s", path, object)
	}
	keys := []string{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key.(string))
		var skip json.RawMessage
		if err := decoder.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

// The credit state's own keys are snake_case and in this order, like the
// snapshot's and the meter's; api_credits and renewals keep the summary
// contract's camelCase.
func TestCreditStateWireNames(t *testing.T) {
	raw, err := json.Marshal(fullState())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path []string
		want string
	}{
		{nil, "schema quota_cache_version computed_at valid_until inputs settings api_credits renewals organizations"},
		{[]string{"inputs"}, "snapshot_written_at meter_flushed_at meter_error settings_revision"},
		{[]string{"settings"}, "state editable revision import"},
		{[]string{"settings", "import"}, "state kind source_path sha256 settings_sha256 legacy_revision legacy_updated_at counts imported_at following importable"},
		{[]string{"settings", "import", "counts"}, "monthly_credits refill_dates readings renewal_dates"},
		{[]string{"organizations"}, "state revision source migration config_items_ignored"},
		{[]string{"organizations", "migration"}, "at moved skipped"},
		{[]string{"renewals", "5f2b8c41d09e7a36"}, "date revision updatedAtEpoch"},
		{[]string{"api_credits"}, "title currency pricing meter editing pool accounts unlinked orphans"},
		{[]string{"api_credits", "editing"}, "available reason migration"},
	} {
		if got := strings.Join(keysOf(t, raw, tc.path...), " "); got != tc.want {
			t.Errorf("%v:\n got %s\nwant %s", tc.path, got, tc.want)
		}
	}
	// Nothing absent is omitted: null is written.
	raw, err = json.Marshal(&CreditState{Schema: 1, Renewals: map[string]RenewalSetting{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"valid_until":null`, `"api_credits":null`, `"import":null`, `"migration":null`, `"snapshot_written_at":null`, `"renewals":{}`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("%s missing from %s", want, raw)
		}
	}
}
