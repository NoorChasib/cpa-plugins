package overrides

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

const (
	orgA     = "00000000-0000-4000-8000-00000000000a"
	orgB     = "00000000-0000-4000-8000-00000000000b"
	renewalA = "0123456789abcdef"
)

var accountA = qc.APICreditOrgAccount(orgA)

func at(t *testing.T, text string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func ptr(s string) *string { return &s }

// validFile is a settings.json with one of everything, as E.1 shows it.
func validFile() map[string]any {
	return map[string]any{
		"schema":   1,
		"revision": 7,
		"apiCredits": map[string]any{
			accountA: map[string]any{
				"monthlyUsd": "260.50",
				"renews":     "2026-10-29",
				"reading": map[string]any{
					"remainingUsd":   "143.20",
					"at":             "2026-10-09T13:20:00Z",
					"enteredAt":      "2026-10-09T13:24:10Z",
					"organizationId": orgA,
					"baseline": map[string]any{
						"dayStart": "2026-10-09T00:00:00Z",
						"until":    "2026-10-09T13:00:00Z",
						"usage":    []any{map[string]any{"model": "claude-sonnet-5-5", "requests": 3, "input": 1200, "output": 800}},
					},
				},
				"rev":       7,
				"updatedAt": "2026-10-09T13:24:10Z",
			},
		},
		"renewals": map[string]any{
			renewalA: map[string]any{"date": "2026-10-29", "rev": 5, "updatedAt": "2026-10-09T13:00:00Z"},
		},
	}
}

func writeSettings(t *testing.T, dir string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAMissingFileIsEmptyAndEditable(t *testing.T) {
	s := Open(t.TempDir())
	v := s.Current()
	if v.Unreadable || v.Revision != 0 || len(v.APICredits) != 0 || len(v.Renewals) != 0 || s.LastError() != "" {
		t.Fatalf("values = %+v, error %q", v, s.LastError())
	}
}

func TestAValidFileLoads(t *testing.T) {
	dir := t.TempDir()
	writeSettings(t, dir, validFile())
	v := Open(dir).Current()
	credit := v.APICredits[accountA]
	if v.Unreadable || v.Revision != 7 || credit.MonthlyUSD != "260.50" || credit.Renews != "2026-10-29" || credit.Rev != 7 ||
		credit.Reading == nil || credit.Reading.RemainingUSD != "143.20" || !credit.Reading.At.Equal(at(t, "2026-10-09T13:20:00Z")) ||
		credit.Reading.OrganizationID != orgA || len(credit.Reading.Baseline.Usage) != 1 || credit.Reading.Baseline.Usage[0].Input != 1200 ||
		v.Renewals[renewalA] != (Renewal{Date: "2026-10-29", Rev: 5, UpdatedAt: at(t, "2026-10-09T13:00:00Z")}) {
		t.Fatalf("values = %+v", v)
	}
	// The committed fixtures load too.
	for _, name := range []string{"api-credits", "seven-credentials", "degraded-states"} {
		if Load(filepath.Join("..", "..", "testdata", "overrides", name+".json")).Unreadable {
			t.Errorf("testdata/overrides/%s.json does not load", name)
		}
	}
}

// Every shape rule, one at a time: any failure makes the whole file
// unreadable, applies nothing, and is never written over.
func TestEachShapeRule(t *testing.T) {
	credit := func(f map[string]any) map[string]any {
		return f["apiCredits"].(map[string]any)[accountA].(map[string]any)
	}
	reading := func(f map[string]any) map[string]any { return credit(f)["reading"].(map[string]any) }
	baseline := func(f map[string]any) map[string]any { return reading(f)["baseline"].(map[string]any) }
	renewal := func(f map[string]any) map[string]any {
		return f["renewals"].(map[string]any)[renewalA].(map[string]any)
	}
	for name, break_ := range map[string]func(map[string]any){
		"schema 2":              func(f map[string]any) { f["schema"] = 2 },
		"no schema":             func(f map[string]any) { delete(f, "schema") },
		"no revision":           func(f map[string]any) { delete(f, "revision") },
		"negative revision":     func(f map[string]any) { f["revision"] = -1 },
		"fractional revision":   func(f map[string]any) { f["revision"] = 1.5 },
		"unknown top-level key": func(f map[string]any) { f["extra"] = true },
		"unknown entry key":     func(f map[string]any) { credit(f)["note"] = "x" },
		"unknown reading key":   func(f map[string]any) { reading(f)["source"] = "x" },
		"unknown baseline key":  func(f map[string]any) { baseline(f)["from"] = "x" },
		"unknown usage key": func(f map[string]any) {
			baseline(f)["usage"] = []any{map[string]any{"model": "claude-sonnet-5-5", "cost": 1}}
		},
		"unknown renewal key":        func(f map[string]any) { renewal(f)["note"] = "x" },
		"bad apiCredits key":         func(f map[string]any) { f["apiCredits"] = map[string]any{"label-1234": credit(f)} },
		"upper-case apiCredits key":  func(f map[string]any) { f["apiCredits"] = map[string]any{strings.ToUpper(accountA): credit(f)} },
		"bad renewals key":           func(f map[string]any) { f["renewals"] = map[string]any{"claude-a@example.com.json": renewal(f)} },
		"monthlyUsd with 3 decimals": func(f map[string]any) { credit(f)["monthlyUsd"] = "1.234" },
		"monthlyUsd empty":           func(f map[string]any) { credit(f)["monthlyUsd"] = "" },
		"monthlyUsd a number":        func(f map[string]any) { credit(f)["monthlyUsd"] = 200 },
		"renews impossible":          func(f map[string]any) { credit(f)["renews"] = "2026-02-30" },
		"renews as an instant":       func(f map[string]any) { credit(f)["renews"] = "2026-10-29T00:00:00Z" },
		"renews in 2100":             func(f map[string]any) { credit(f)["renews"] = "2100-01-01" },
		"date impossible":            func(f map[string]any) { renewal(f)["date"] = "2026-11-31" },
		"remainingUsd negative":      func(f map[string]any) { reading(f)["remainingUsd"] = "-1" },
		"at with an offset":          func(f map[string]any) { reading(f)["at"] = "2026-10-09T13:20:00+00:00" },
		"at with a fraction":         func(f map[string]any) { reading(f)["at"] = "2026-10-09T13:20:00.5Z" },
		"enteredAt missing":          func(f map[string]any) { delete(reading(f), "enteredAt") },
		"updatedAt not a time":       func(f map[string]any) { credit(f)["updatedAt"] = "yesterday" },
		"organizationId upper case":  func(f map[string]any) { reading(f)["organizationId"] = strings.ToUpper(orgA) },
		"organizationId all zeros":   func(f map[string]any) { reading(f)["organizationId"] = "00000000-0000-0000-0000-000000000000" },
		"dayStart not the day":       func(f map[string]any) { baseline(f)["dayStart"] = "2026-10-08T00:00:00Z" },
		"until not the hour":         func(f map[string]any) { baseline(f)["until"] = "2026-10-09T12:00:00Z" },
		"model not normalized":       func(f map[string]any) { baseline(f)["usage"] = []any{map[string]any{"model": "Claude-Sonnet-5-5"}} },
		"prompt unknown": func(f map[string]any) {
			baseline(f)["usage"] = []any{map[string]any{"model": "claude-haiku-5-5", "prompt": "over_200k"}}
		},
		"too many baseline entries": func(f map[string]any) {
			usage := []any{}
			for i := range MaxBaseline + 1 {
				usage = append(usage, map[string]any{"model": fmt.Sprintf("model-%02d", i)})
			}
			baseline(f)["usage"] = usage
		},
		"rev zero":            func(f map[string]any) { credit(f)["rev"] = 0 },
		"rev past revision":   func(f map[string]any) { renewal(f)["rev"] = 8 },
		"rev missing":         func(f map[string]any) { delete(renewal(f), "rev") },
		"trailing data":       nil,
		"too many apiCredits": func(f map[string]any) { fill(f, "apiCredits", MaxAPICredits+1) },
		"too many renewals":   func(f map[string]any) { fill(f, "renewals", MaxRenewals+1) },
	} {
		dir := t.TempDir()
		f := validFile()
		var raw []byte
		if break_ == nil {
			raw, _ = json.Marshal(f)
			raw = append(raw, []byte(` {}`)...)
		} else {
			break_(f)
			raw, _ = json.Marshal(f)
		}
		if err := os.WriteFile(filepath.Join(dir, FileName), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		s := Open(dir)
		v := s.Current()
		if !v.Unreadable || len(v.APICredits) != 0 || len(v.Renewals) != 0 || s.LastError() != "unreadable" {
			t.Errorf("%s: loaded as %+v", name, v)
			continue
		}
		if _, err := s.Apply(Batch{Kind: KindRenewals, Renewals: []RenewalItem{{ID: renewalA, Date: ptr("2026-11-01")}}}, nil, at(t, "2026-10-09T14:00:00Z")); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: a save over an unreadable file answered %v", name, err)
		}
		if after, _ := os.ReadFile(filepath.Join(dir, FileName)); !bytes.Equal(after, raw) {
			t.Errorf("%s: the unreadable file was overwritten", name)
		}
	}
}

func fill(f map[string]any, kind string, n int) {
	entries := map[string]any{}
	for i := range n {
		if kind == "apiCredits" {
			entries[fmt.Sprintf("org-%012x", i)] = map[string]any{"monthlyUsd": "1", "rev": 1, "updatedAt": "2026-10-09T13:00:00Z"}
		} else {
			entries[fmt.Sprintf("%016x", i)] = map[string]any{"date": "2026-10-29", "rev": 1, "updatedAt": "2026-10-09T13:00:00Z"}
		}
	}
	f[kind] = entries
}

// Oversize, not a regular file, or not JSON at all: unreadable, never
// overwritten.
func TestAFileThatCannotBeReadIsNeverOverwritten(t *testing.T) {
	big := bytes.Repeat([]byte(" "), MaxBytes+1)
	for name, raw := range map[string][]byte{
		"oversize":  append([]byte(`{"schema":1,"revision":0}`), big...),
		"not JSON":  []byte("schema: 1\n"),
		"truncated": []byte(`{"schema":1,"revision":0,"apiCredits":{`),
		"null":      []byte(`null`),
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, FileName)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		s := Open(dir)
		if !s.Current().Unreadable {
			t.Errorf("%s: loaded", name)
		}
		if _, err := s.Apply(Batch{Kind: KindRenewals, Renewals: []RenewalItem{{ID: renewalA, Date: ptr("2026-11-01")}}}, nil, time.Now()); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: save answered %v", name, err)
		}
		if after, _ := os.ReadFile(path); !bytes.Equal(after, raw) {
			t.Errorf("%s: overwritten", name)
		}
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, FileName), 0o700); err != nil {
		t.Fatal(err)
	}
	if !Open(dir).Current().Unreadable {
		t.Error("a directory in settings.json's place loaded")
	}
}

// An entry with no value at all is what a save deletes; one left in a file
// loads as absent.
func TestAnEntryWithNoValueLoadsAsAbsent(t *testing.T) {
	dir := t.TempDir()
	f := validFile()
	f["apiCredits"] = map[string]any{accountA: map[string]any{"rev": 7, "updatedAt": "2026-10-09T13:24:10Z"}}
	f["renewals"] = map[string]any{renewalA: map[string]any{"rev": 5, "updatedAt": "2026-10-09T13:00:00Z"}}
	writeSettings(t, dir, f)
	if v := Open(dir).Current(); v.Unreadable || len(v.APICredits) != 0 || len(v.Renewals) != 0 {
		t.Fatalf("values = %+v", v)
	}
}

func newStore(t *testing.T) (*Store, *int) {
	t.Helper()
	s := Open(t.TempDir())
	synced := 0
	s.syncDir = func(dir string) error {
		synced++
		return syncDirectory(dir)
	}
	return s, &synced
}

func creditItem(id, base string, monthly, renews *string, reading *ReadingInput) APICreditItem {
	return APICreditItem{ID: id, BaseRevision: base, MonthlyUSD: monthly, Renews: renews, Reading: reading, OrganizationID: orgA}
}

// A save writes the whole file by rename, synced, directory included, and
// counts the revision; each row records the revision that wrote it.
func TestApplyCommitsAtomicallyAndCountsRevisions(t *testing.T) {
	s, synced := newStore(t)
	now := at(t, "2026-10-09T14:00:00Z")
	result, err := s.Apply(Batch{Kind: KindAPICredits, APICredits: []APICreditItem{creditItem(accountA, "", ptr("260.50"), nil, nil)}}, nil, now)
	if err != nil || result.Revision != 1 || result.Unchanged || !reflect.DeepEqual(result.IDs, []string{accountA}) ||
		!reflect.DeepEqual(result.Fields, []string{"monthlyUsd"}) {
		t.Fatalf("result %+v, %v", result, err)
	}
	if *synced != 1 {
		t.Fatalf("directory synced %d times", *synced)
	}
	info, err := os.Stat(s.Path())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("settings.json: %v %v", info, err)
	}
	entries, _ := os.ReadDir(s.Dir())
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}

	result, err = s.Apply(Batch{Kind: KindRenewals, Renewals: []RenewalItem{{ID: renewalA, Date: ptr("2026-10-29")}}}, nil, now.Add(time.Second))
	if err != nil || result.Revision != 2 {
		t.Fatalf("second: %+v %v", result, err)
	}
	v := s.Current()
	if v.Revision != 2 || v.APICredits[accountA].Rev != 1 || v.Renewals[renewalA].Rev != 2 || !v.Renewals[renewalA].UpdatedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("values = %+v", v)
	}
	// Reopened, the file says the same.
	if again := Open(s.Dir()).Current(); !reflect.DeepEqual(again, v) {
		t.Fatalf("reopened:\n%+v\nwant\n%+v", again, v)
	}
	// Clearing every value deletes the row.
	if _, err := s.Apply(Batch{Kind: KindAPICredits, APICredits: []APICreditItem{creditItem(accountA, "1", nil, nil, nil)}}, nil, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Current().APICredits[accountA]; ok {
		t.Fatal("a row with every value cleared was kept")
	}
}

// What every row already holds is a no-op; a row another device changed is a
// conflict unless it asks for what is already there.
func TestConflictsAndReplays(t *testing.T) {
	s, _ := newStore(t)
	now := at(t, "2026-10-09T14:00:00Z")
	set := func(base, monthly string) (Result, error) {
		return s.Apply(Batch{Kind: KindAPICredits, APICredits: []APICreditItem{creditItem(accountA, base, ptr(monthly), nil, nil)}}, nil, now)
	}
	if _, err := set("", "200"); err != nil {
		t.Fatal(err)
	}
	// The same press again: unchanged, nothing written.
	before, _ := os.ReadFile(s.Path())
	result, err := set("", "200")
	if err != nil || !result.Unchanged || result.Revision != 1 {
		t.Fatalf("replay: %+v %v", result, err)
	}
	// Another device, still on revision "", asks for something else.
	_, err = set("", "300")
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.Revision != "1" || !reflect.DeepEqual(conflict.IDs, []string{accountA}) ||
		conflict.APICredits[accountA].MonthlyUSD != "200" {
		t.Fatalf("conflict: %+v %v", conflict, err)
	}
	if after, _ := os.ReadFile(s.Path()); !bytes.Equal(after, before) || s.Current().Revision != 1 {
		t.Fatal("a conflict wrote something")
	}
	// On the current revision it saves.
	if result, err := set("1", "300"); err != nil || result.Revision != 2 {
		t.Fatalf("current base: %+v %v", result, err)
	}
	// A batch with one conflicting row writes none of its rows.
	_, err = s.Apply(Batch{Kind: KindAPICredits, APICredits: []APICreditItem{
		creditItem(qc.APICreditOrgAccount(orgB), "", ptr("10"), nil, nil),
		creditItem(accountA, "1", ptr("400"), nil, nil),
	}}, nil, now)
	if !errors.As(err, &conflict) || len(s.Current().APICredits) != 1 {
		t.Fatalf("a partial batch: %v, %+v", err, s.Current())
	}
}

// A failed commit changes nothing, on disk or in memory.
func TestAFailedCommitLeavesEverythingAsItWas(t *testing.T) {
	s, _ := newStore(t)
	now := at(t, "2026-10-09T14:00:00Z")
	if _, err := s.Apply(Batch{Kind: KindRenewals, Renewals: []RenewalItem{{ID: renewalA, Date: ptr("2026-10-29")}}}, nil, now); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(s.Path())
	s.write = func(string, []byte) error { return errors.New("disk full") }
	_, err := s.Apply(Batch{Kind: KindRenewals, Renewals: []RenewalItem{{ID: renewalA, BaseRevision: "1", Date: ptr("2026-11-29")}}}, nil, now)
	if !errors.Is(err, ErrUnwritable) {
		t.Fatalf("err = %v", err)
	}
	if v := s.Current(); v.Revision != 1 || v.Renewals[renewalA].Date != "2026-10-29" {
		t.Fatalf("memory moved: %+v", v)
	}
	if after, _ := os.ReadFile(s.Path()); !bytes.Equal(after, before) {
		t.Fatal("the file moved")
	}
}

// Thirty committed saves a minute, then 429 until the minute has passed.
func TestCommittedWritesAreThrottled(t *testing.T) {
	s, _ := newStore(t)
	now := at(t, "2026-10-09T14:00:00Z")
	save := func(i int, when time.Time) error {
		_, err := s.Apply(Batch{Kind: KindRenewals, Renewals: []RenewalItem{{ID: renewalA, BaseRevision: RevisionText(s.Current().Revision), Date: ptr(fmt.Sprintf("2026-10-%02d", i%28+1))}}}, nil, when)
		return err
	}
	for i := range writeLimit {
		if err := save(i, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	if err := save(writeLimit, now.Add(40*time.Second)); !errors.Is(err, ErrTooManyWrites) {
		t.Fatalf("save past the limit: %v", err)
	}
	// An unchanged batch is not a write, and is still answered.
	if _, err := s.Apply(Batch{Kind: KindRenewals, Renewals: []RenewalItem{{ID: renewalA, Date: ptr(s.Current().Renewals[renewalA].Date)}}}, nil, now.Add(41*time.Second)); err != nil {
		t.Fatalf("an unchanged batch: %v", err)
	}
	if err := save(writeLimit+1, now.Add(61*time.Second)); err != nil {
		t.Fatalf("after the minute: %v", err)
	}
}

// Past a bound, renewal dates for credentials the dashboard no longer lists go
// first, the oldest first; when none can go, the save is refused.
func TestBoundsEvictOrphanRenewalsFirst(t *testing.T) {
	dir := t.TempDir()
	f := validFile()
	fill(f, "renewals", MaxRenewals)
	f["apiCredits"] = map[string]any{}
	renewals := f["renewals"].(map[string]any)
	renewals[fmt.Sprintf("%016x", 3)].(map[string]any)["updatedAt"] = "2026-10-01T00:00:00Z"
	writeSettings(t, dir, f)
	s := Open(dir)
	now := at(t, "2026-10-09T14:00:00Z")
	listed := map[string]bool{}
	_, err := s.Apply(Batch{Kind: KindRenewals, Renewals: []RenewalItem{{ID: "ffffffffffffffff", Date: ptr("2026-10-29")}}, Credentials: listed}, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	v := s.Current()
	if _, kept := v.Renewals[fmt.Sprintf("%016x", 3)]; kept || len(v.Renewals) != MaxRenewals {
		t.Fatalf("evicted the wrong one: %d renewals", len(v.Renewals))
	}
	// Every stored date belongs to a listed credential: nothing can go.
	for id := range v.Renewals {
		listed[id] = true
	}
	_, err = s.Apply(Batch{Kind: KindRenewals, Renewals: []RenewalItem{{ID: "eeeeeeeeeeeeeeee", Date: ptr("2026-10-29")}}, Credentials: listed}, nil, now.Add(time.Second))
	if !errors.Is(err, ErrFull) || len(s.Current().Renewals) != MaxRenewals {
		t.Fatalf("err = %v", err)
	}
	// API credit rows are never evicted.
	dir = t.TempDir()
	f = validFile()
	fill(f, "apiCredits", MaxAPICredits)
	f["renewals"] = map[string]any{}
	writeSettings(t, dir, f)
	s = Open(dir)
	_, err = s.Apply(Batch{Kind: KindAPICredits, APICredits: []APICreditItem{creditItem("org-ffffffffffff", "", ptr("1"), nil, nil)}}, nil, now)
	if !errors.Is(err, ErrFull) {
		t.Fatalf("a 65th row: %v", err)
	}
}

// meterFor is a meter flushed at flushed holding one organization's hours.
func meterFor(t *testing.T, flushed string, hours ...qc.MeterBucket) *qc.APIMeter {
	return &qc.APIMeter{Schema: 1, FlushedAt: at(t, flushed), Organizations: map[string]qc.MeterOrganization{
		orgA: {Since: at(t, "2026-09-01T00:00:00Z"), Days: []qc.MeterBucket{}, Hours: hours},
	}}
}

// A new reading takes its baseline from the meter at save: its day's hours up
// to its own. A reading resent unchanged keeps the one it has.
func TestANewReadingTakesItsBaseline(t *testing.T) {
	s, _ := newStore(t)
	now := at(t, "2026-10-09T14:00:00Z")
	meter := meterFor(t, "2026-10-09T13:59:00Z",
		qc.MeterBucket{Start: at(t, "2026-10-08T23:00:00Z"), Usage: []qc.MeterUsage{{Model: "claude-sonnet-5-5", Input: 999}}},
		qc.MeterBucket{Start: at(t, "2026-10-09T09:00:00Z"), Usage: []qc.MeterUsage{{Model: "claude-sonnet-5-5", Requests: 3, Input: 1200, Output: 800}}},
		qc.MeterBucket{Start: at(t, "2026-10-09T13:00:00Z"), Usage: []qc.MeterUsage{{Model: "claude-sonnet-5-5", Input: 50}}},
	)
	reading := &ReadingInput{RemainingUSD: "143.20", At: "2026-10-09T13:20:00Z"}
	if _, err := s.Apply(Batch{Kind: KindAPICredits, APICredits: []APICreditItem{creditItem(accountA, "", nil, nil, reading)}}, meter, now); err != nil {
		t.Fatal(err)
	}
	got := s.Current().APICredits[accountA].Reading
	want := &Reading{
		RemainingUSD: "143.20", At: at(t, "2026-10-09T13:20:00Z"), EnteredAt: now, OrganizationID: orgA,
		Baseline: Baseline{DayStart: at(t, "2026-10-09T00:00:00Z"), Until: at(t, "2026-10-09T13:00:00Z"),
			Usage: []qc.MeterUsage{{Model: "claude-sonnet-5-5", Requests: 3, Input: 1200, Output: 800}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reading:\n got %+v\nwant %+v", got, want)
	}
	// Resent unchanged with a new credit, a day later and with the meter
	// moved on: the baseline and the time it was entered stay.
	later := now.Add(24 * time.Hour)
	if _, err := s.Apply(Batch{Kind: KindAPICredits, APICredits: []APICreditItem{creditItem(accountA, "1", ptr("300"), nil, reading)}},
		meterFor(t, "2026-10-10T13:59:00Z"), later); err != nil {
		t.Fatal(err)
	}
	if again := s.Current().APICredits[accountA].Reading; again != got {
		t.Fatalf("an unchanged reading was taken again: %+v", again)
	}
	// Without a meter, or the organization not in it, the baseline is empty.
	s2, _ := newStore(t)
	if _, err := s2.Apply(Batch{Kind: KindAPICredits, APICredits: []APICreditItem{creditItem(accountA, "", nil, nil, reading)}}, nil, now); err != nil {
		t.Fatal(err)
	}
	if b := s2.Current().APICredits[accountA].Reading.Baseline; b.Usage == nil || len(b.Usage) != 0 || !b.Until.Equal(at(t, "2026-10-09T13:00:00Z")) {
		t.Fatalf("no meter: %+v", b)
	}
}

// A meter that does not cover the baseline's hours refuses the reading.
func TestAnUncoveredBaselineIsRefused(t *testing.T) {
	s, _ := newStore(t)
	now := at(t, "2026-10-09T14:00:00Z")
	// A meter whose clock ran three days ahead keeps no hour of Oct 9.
	meter := meterFor(t, "2026-10-12T14:00:00Z")
	_, err := s.Apply(Batch{Kind: KindAPICredits, APICredits: []APICreditItem{
		creditItem(accountA, "", nil, nil, &ReadingInput{RemainingUSD: "1.00", At: "2026-10-09T13:20:00Z"}),
	}}, meter, now)
	var field *FieldError
	if !errors.As(err, &field) || *field != (FieldError{Code: CodeReadingTime, ID: accountA, Field: "reading.at"}) {
		t.Fatalf("err = %v", err)
	}
	if s.Current().Revision != 0 {
		t.Fatal("an uncovered reading was saved")
	}
}

// Past MaxBaseline pairs, the smallest are summed under (other).
func TestABaselineIsCapped(t *testing.T) {
	usage := []qc.MeterUsage{}
	for i := range 20 {
		usage = qc.MergeMeterUsage(usage, qc.MeterUsage{Model: fmt.Sprintf("model-%02d", i), Input: uint64(100 + i)})
	}
	capped := capBaseline(usage)
	if len(capped) != MaxBaseline {
		t.Fatalf("%d entries", len(capped))
	}
	var other qc.MeterUsage
	for _, u := range capped {
		if u.Model == qc.MeterOtherModel {
			other = u
		}
	}
	// model-00 to model-04 are the five smallest.
	if other.Input != 100+101+102+103+104 {
		t.Fatalf("(other) = %+v", other)
	}
}

// A meter that has not saved since before the reading's hour, stopped or
// stale, still gives the reading a baseline: the hours it has. It counted
// nothing after its last save, so the baseline is complete, or short, which
// overstates the spend since the reading, the safe direction. That is the
// state the page asks for a reading in, so it must save, and with it the rest
// of the batch.
func TestAStoppedMeterTakesAReading(t *testing.T) {
	s, _ := newStore(t)
	now := at(t, "2026-10-09T13:00:00Z")
	stopped := at(t, "2026-10-09T09:00:00Z")
	meter := meterFor(t, "2026-10-09T09:00:00Z",
		qc.MeterBucket{Start: at(t, "2026-10-09T08:00:00Z"), Usage: []qc.MeterUsage{{Model: "claude-sonnet-5-5", Requests: 2, Input: 700}}},
		qc.MeterBucket{Start: at(t, "2026-10-09T09:00:00Z"), Usage: []qc.MeterUsage{{Model: "claude-sonnet-5-5", Input: 5}}},
	)
	meter.StoppedAt, meter.StopReason = &stopped, "disabled"
	reading := &ReadingInput{RemainingUSD: "143.20", At: "2026-10-09T12:55:00Z"}
	result, err := s.Apply(Batch{Kind: KindAPICredits, APICredits: []APICreditItem{creditItem(accountA, "", ptr("300"), nil, reading)}}, meter, now)
	if err != nil || result.Revision != 1 {
		t.Fatalf("a reading on a stopped meter: %+v, %v", result, err)
	}
	credit := s.Current().APICredits[accountA]
	want := Baseline{DayStart: at(t, "2026-10-09T00:00:00Z"), Until: at(t, "2026-10-09T12:00:00Z"),
		Usage: []qc.MeterUsage{{Model: "claude-sonnet-5-5", Requests: 2, Input: 705}}}
	if credit.MonthlyUSD != "300" || credit.Reading == nil || !reflect.DeepEqual(credit.Reading.Baseline, want) {
		t.Fatalf("saved %+v, reading %+v", credit, credit.Reading)
	}
	// A meter that stopped the day before has none of the reading's day.
	s2, _ := newStore(t)
	yesterday := meterFor(t, "2026-10-08T20:00:00Z",
		qc.MeterBucket{Start: at(t, "2026-10-08T19:00:00Z"), Usage: []qc.MeterUsage{{Model: "claude-sonnet-5-5", Input: 9}}})
	if _, err := s2.Apply(Batch{Kind: KindAPICredits, APICredits: []APICreditItem{creditItem(accountA, "", nil, nil, reading)}}, yesterday, now); err != nil {
		t.Fatal(err)
	}
	if b := s2.Current().APICredits[accountA].Reading.Baseline; len(b.Usage) != 0 {
		t.Fatalf("a baseline from another day: %+v", b)
	}
}

// What a newer quota-cache may write in a bucket, a prompt class or a model
// name this build does not know, goes into the baseline as (other), which is
// never priced: the baseline reads low and the spend since high, the safe
// direction. Stored verbatim it would make settings.json fail its own load
// rules at the next open, and every value in it would stop applying.
func TestABaselineIsStoredInAFormThisBuildLoads(t *testing.T) {
	s, _ := newStore(t)
	now := at(t, "2026-10-09T14:00:00Z")
	meter := meterFor(t, "2026-10-09T13:59:00Z",
		qc.MeterBucket{Start: at(t, "2026-10-09T09:00:00Z"), Usage: []qc.MeterUsage{
			{Model: "Claude-Sonnet-5-5", Requests: 1, Input: 10},
			{Model: "claude-haiku-5-5", Prompt: qc.MeterPromptOver100K, Requests: 1, Input: 200000},
			{Model: "claude-sonnet-5-5", Requests: 3, Input: 1200, Output: 800},
			{Model: "claude-sonnet-5-5", Prompt: "over_200k", Requests: 2, Input: 300000},
		}},
	)
	reading := &ReadingInput{RemainingUSD: "143.20", At: "2026-10-09T13:20:00Z"}
	if _, err := s.Apply(Batch{Kind: KindAPICredits, APICredits: []APICreditItem{creditItem(accountA, "", ptr("260.50"), nil, reading)}}, meter, now); err != nil {
		t.Fatal(err)
	}
	want := []qc.MeterUsage{
		{Model: qc.MeterOtherModel, Requests: 3, Input: 300010},
		{Model: "claude-haiku-5-5", Prompt: qc.MeterPromptOver100K, Requests: 1, Input: 200000},
		{Model: "claude-sonnet-5-5", Requests: 3, Input: 1200, Output: 800},
	}
	if got := s.Current().APICredits[accountA].Reading.Baseline.Usage; !reflect.DeepEqual(got, want) {
		t.Fatalf("baseline:\n got %+v\nwant %+v", got, want)
	}
	again := Open(s.Dir()).Current()
	if again.Unreadable || again.APICredits[accountA].MonthlyUSD != "260.50" || !reflect.DeepEqual(again, s.Current()) {
		t.Fatalf("reopened: %+v", again)
	}
}

// A new reading stores the account's organization, so one the document gives
// in a form the load rules refuse is not taken. Nothing is written.
func TestAReadingNeedsANormalizedOrganization(t *testing.T) {
	for _, org := range []string{"", "00000000-0000-4000-8000-00000000000A", "not-an-organization"} {
		s, _ := newStore(t)
		item := creditItem(accountA, "", ptr("300"), nil, &ReadingInput{RemainingUSD: "1.00", At: "2026-10-09T13:20:00Z"})
		item.OrganizationID = org
		_, err := s.Apply(Batch{Kind: KindAPICredits, APICredits: []APICreditItem{item}}, nil, at(t, "2026-10-09T14:00:00Z"))
		var refused *NotEditableError
		if !errors.As(err, &refused) || !reflect.DeepEqual(refused.IDs, []string{accountA}) {
			t.Fatalf("organization %q: err = %v", org, err)
		}
		if s.Current().Revision != 0 {
			t.Fatalf("organization %q: saved", org)
		}
		if _, statErr := os.Stat(s.Path()); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("organization %q: a file was written", org)
		}
	}
}

// A commit never writes a file this build could not load back.
func TestACommitThisBuildCouldNotLoadIsRefused(t *testing.T) {
	s, _ := newStore(t)
	written := false
	s.write = func(path string, raw []byte) error {
		written = true
		return writeFile(path, raw)
	}
	for _, raw := range [][]byte{[]byte(`{"schema":2,"revision":0,"apiCredits":{},"renewals":{}}`), []byte(`not json`)} {
		if err := s.commit(raw); !errors.Is(err, ErrUnwritable) {
			t.Fatalf("%s: err = %v", raw, err)
		}
	}
	if written {
		t.Fatal("an unloadable file reached the disk")
	}
	good, err := encode(empty())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.commit(good); err != nil || !written {
		t.Fatalf("a loadable file: %v, written %v", err, written)
	}
}
