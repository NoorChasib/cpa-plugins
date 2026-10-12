package settings

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

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

const (
	orgOne   = "00000000-0000-4000-8000-0000000000a1"
	orgTwo   = "00000000-0000-4000-8000-0000000000b2"
	orgThree = "00000000-0000-4000-8000-0000000000c3"
)

var orgNow = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

// orgFirst is the revision of a file written at orgNow where there was none:
// the time in Unix seconds.
var orgFirst = uint64(orgNow.Unix())

// orgRev is the revision n batches after the first, as a page sends it.
func orgRev(n uint64) string { return RevisionText(orgFirst + n) }

// validOrganizations is an organizations file with one of everything.
func validOrganizations() map[string]any {
	return map[string]any{
		"schema":   1,
		"revision": "4",
		"organizations": []any{
			map[string]any{"label": "personal-1", "organization_id": orgOne, "rev": "3", "updated_at": "2026-10-12T09:00:00Z"},
			map[string]any{"label": "team", "organization_id": orgTwo, "rev": "4", "updated_at": "2026-10-12T10:00:00Z"},
		},
	}
}

func writeOrganizations(t *testing.T, snapshot string, value any) []byte {
	t.Helper()
	raw, ok := value.([]byte)
	if !ok {
		var err error
		if raw, err = json.Marshal(value); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(client.OrganizationsPath(snapshot), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return raw
}

func newOrganizationStore(t *testing.T) (*OrganizationStore, *int) {
	t.Helper()
	s := OpenOrganizations(snapshotIn(t.TempDir()))
	synced := 0
	s.syncDir = func(dir string) error {
		synced++
		return syncDirectory(dir)
	}
	return s, &synced
}

func addOp(label, org string) OrganizationOp {
	return OrganizationOp{Op: OpAdd, Label: label, OrganizationID: org}
}

func renameOp(org, label string) OrganizationOp {
	return OrganizationOp{Op: OpRename, ID: client.APICreditOrgAccount(org), Label: label}
}

func removeOp(org string) OrganizationOp {
	return OrganizationOp{Op: OpRemove, ID: client.APICreditOrgAccount(org)}
}

func TestOrganizationsFileLoads(t *testing.T) {
	snapshot := snapshotIn(t.TempDir())
	writeOrganizations(t, snapshot, validOrganizations())
	got := OpenOrganizations(snapshot).Current()
	want := Organizations{
		State: client.OrganizationsStateOK, Revision: 4,
		Items: []Organization{
			{Label: "personal-1", OrganizationID: orgOne, Rev: 3, UpdatedAt: orgNow},
			{Label: "team", OrganizationID: orgTwo, Rev: 4, UpdatedAt: orgNow.Add(time.Hour)},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loaded\n%+v\nwant\n%+v", got, want)
	}
	// It encodes back to a file that loads the same.
	raw, err := encodeOrganizations(got)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := decodeOrganizations(raw); err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("round trip: %+v, %v", again, err)
	}
	// A label with inner spaces and other scripts loads, and so does an empty
	// list.
	f := validOrganizations()
	f["organizations"].([]any)[1].(map[string]any)["label"] = "Équipe Two 二"
	writeOrganizations(t, snapshot, f)
	if o := LoadOrganizations(client.OrganizationsPath(snapshot)); o.State != client.OrganizationsStateOK || o.Items[1].Label != "Équipe Two 二" {
		t.Fatalf("loaded %+v", o)
	}
	writeOrganizations(t, snapshot, []byte(`{"schema":1,"revision":"0","organizations":[]}`))
	if o := LoadOrganizations(client.OrganizationsPath(snapshot)); o.State != client.OrganizationsStateOK || o.Items == nil || len(o.Items) != 0 {
		t.Fatalf("empty list: %+v", o)
	}
	// As the snapshot's entries carry them: the label, the organization and
	// the position, nothing else.
	credits := want.APICredits()
	if len(credits) != 2 || credits[0] != (client.APICredit{Label: "personal-1", OrganizationID: orgOne}) ||
		credits[1] != (client.APICredit{Label: "team", Position: 1, OrganizationID: orgTwo}) || want.Items[1].ID() != client.APICreditOrgAccount(orgTwo) {
		t.Fatalf("credits %+v", credits)
	}
}

// Every load rule, one at a time: any failure makes the whole file
// unreadable, applies nothing, and is never written over.
func TestEachOrganizationsLoadRule(t *testing.T) {
	entry := func(f map[string]any, i int) map[string]any { return f["organizations"].([]any)[i].(map[string]any) }
	for name, break_ := range map[string]func(map[string]any){
		"schema 2":                   func(f map[string]any) { f["schema"] = 2 },
		"no schema":                  func(f map[string]any) { delete(f, "schema") },
		"no revision":                func(f map[string]any) { delete(f, "revision") },
		"a numeric revision":         func(f map[string]any) { f["revision"] = 4 },
		"an empty revision":          func(f map[string]any) { f["revision"] = "" },
		"a padded revision":          func(f map[string]any) { f["revision"] = "04" },
		"a negative revision":        func(f map[string]any) { f["revision"] = "-4" },
		"no list":                    func(f map[string]any) { delete(f, "organizations") },
		"a null list":                func(f map[string]any) { f["organizations"] = nil },
		"an unknown top-level key":   func(f map[string]any) { f["extra"] = true },
		"an unknown entry key":       func(f map[string]any) { entry(f, 0)["note"] = "x" },
		"no label":                   func(f map[string]any) { delete(entry(f, 0), "label") },
		"an empty label":             func(f map[string]any) { entry(f, 0)["label"] = "" },
		"a label with outer spaces":  func(f map[string]any) { entry(f, 0)["label"] = " personal-1" },
		"a label of 65 characters":   func(f map[string]any) { entry(f, 0)["label"] = strings.Repeat("é", 65) },
		"a label with a control":     func(f map[string]any) { entry(f, 0)["label"] = "a\tb" },
		"a label with a joiner":      func(f map[string]any) { entry(f, 0)["label"] = "a‍b" },
		"a label with a no-break":    func(f map[string]any) { entry(f, 0)["label"] = "a b" },
		"a label repeated in a case": func(f map[string]any) { entry(f, 1)["label"] = "PERSONAL-1" },
		"no organization":            func(f map[string]any) { delete(entry(f, 0), "organization_id") },
		"an upper-case organization": func(f map[string]any) { entry(f, 0)["organization_id"] = strings.ToUpper(orgOne) },
		"a padded organization":      func(f map[string]any) { entry(f, 0)["organization_id"] = " " + orgOne },
		"the nil organization":       func(f map[string]any) { entry(f, 0)["organization_id"] = "00000000-0000-0000-0000-000000000000" },
		"not an organization":        func(f map[string]any) { entry(f, 0)["organization_id"] = "personal" },
		"an organization twice":      func(f map[string]any) { entry(f, 1)["organization_id"] = orgOne },
		"rev zero":                   func(f map[string]any) { entry(f, 0)["rev"] = "0" },
		"rev past revision":          func(f map[string]any) { entry(f, 0)["rev"] = "5" },
		"a numeric rev":              func(f map[string]any) { entry(f, 0)["rev"] = 3 },
		"no rev":                     func(f map[string]any) { delete(entry(f, 0), "rev") },
		"updated_at with a fraction": func(f map[string]any) { entry(f, 0)["updated_at"] = "2026-10-12T09:00:00.5Z" },
		"updated_at with an offset":  func(f map[string]any) { entry(f, 0)["updated_at"] = "2026-10-12T09:00:00+00:00" },
		"no updated_at":              func(f map[string]any) { delete(entry(f, 0), "updated_at") },
		// No credit, refill date or record of a move from the configuration:
		// the credit and refill date are the settings file's.
		"a default credit": func(f map[string]any) { entry(f, 0)["default_monthly_usd"] = "200" },
		"a default refill": func(f map[string]any) { entry(f, 0)["default_renews"] = "2026-10-29" },
		"a migration": func(f map[string]any) {
			f["migration"] = map[string]any{"at": "2026-10-12T09:00:00Z", "moved": 1, "skipped": 0}
		},
		"a null migration": func(f map[string]any) { f["migration"] = nil },
		"seventeen organizations": func(f map[string]any) {
			items := []any{}
			for i := range MaxOrganizations + 1 {
				items = append(items, map[string]any{"label": fmt.Sprintf("org-%02d", i), "organization_id": fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1),
					"rev": "1", "updated_at": "2026-10-12T09:00:00Z"})
			}
			f["organizations"] = items
		},
		"trailing data": nil,
	} {
		snapshot := snapshotIn(t.TempDir())
		f := validOrganizations()
		var raw []byte
		if break_ == nil {
			raw, _ = json.Marshal(f)
			raw = append(raw, []byte(` {}`)...)
		} else {
			break_(f)
			raw, _ = json.Marshal(f)
		}
		writeOrganizations(t, snapshot, raw)
		checkUnreadableOrganizations(t, name, snapshot, raw)
	}
	big := append([]byte(`{"schema":1,"revision":"0","organizations":[]}`), bytes.Repeat([]byte(" "), MaxOrganizationsBytes)...)
	for name, raw := range map[string][]byte{"oversize": big, "not JSON": []byte("schema: 1\n"), "null": []byte("null")} {
		snapshot := snapshotIn(t.TempDir())
		writeOrganizations(t, snapshot, raw)
		checkUnreadableOrganizations(t, name, snapshot, raw)
	}
	// Not a regular file.
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere.json")
	good, _ := json.Marshal(validOrganizations())
	if err := os.WriteFile(target, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, client.OrganizationsPath(snapshotIn(dir))); err != nil {
		t.Fatal(err)
	}
	if OpenOrganizations(snapshotIn(dir)).Current().State != client.OrganizationsStateUnreadable {
		t.Error("a symlink loaded")
	}
	dir = t.TempDir()
	if err := os.Mkdir(client.OrganizationsPath(snapshotIn(dir)), 0o700); err != nil {
		t.Fatal(err)
	}
	if OpenOrganizations(snapshotIn(dir)).Current().State != client.OrganizationsStateUnreadable {
		t.Error("a directory loaded")
	}
}

func checkUnreadableOrganizations(t *testing.T, name, snapshot string, raw []byte) {
	t.Helper()
	s := OpenOrganizations(snapshot)
	if o := s.Current(); o.State != client.OrganizationsStateUnreadable || len(o.Items) != 0 || o.Revision != 0 {
		t.Errorf("%s: loaded as %+v", name, o)
		return
	}
	if _, err := s.Apply(OrganizationsBatch{Ops: []OrganizationOp{addOp("a", orgThree)}}, orgNow); !errors.Is(err, ErrOrganizationsUnavailable) {
		t.Errorf("%s: a save answered %v", name, err)
	}
	if after, _ := os.ReadFile(s.Path()); !bytes.Equal(after, raw) {
		t.Errorf("%s: overwritten", name)
	}
}

// With no file the list is empty and editable; the first add creates the
// file, atomically, at its first revision.
func TestOrganizationsFirstAddCreatesTheFile(t *testing.T) {
	s, synced := newOrganizationStore(t)
	if o := s.Current(); o.State != client.OrganizationsStateMissing || o.Revision != 0 || o.Items == nil || len(o.Items) != 0 {
		t.Fatalf("missing: %+v", o)
	}
	result, err := s.Apply(OrganizationsBatch{Ops: []OrganizationOp{addOp("  team  ", " "+strings.ToUpper(orgOne)+" ")}}, orgNow)
	if err != nil || result != (OrganizationsResult{Revision: orgFirst, Added: 1}) {
		t.Fatalf("result %+v, %v", result, err)
	}
	want := Organizations{State: client.OrganizationsStateOK, Revision: orgFirst, Items: []Organization{
		{Label: "team", OrganizationID: orgOne, Rev: orgFirst, UpdatedAt: orgNow},
	}}
	if got := s.Current(); !reflect.DeepEqual(got, want) {
		t.Fatalf("current %+v", got)
	}
	if got := LoadOrganizations(s.Path()); !reflect.DeepEqual(got, want) {
		t.Fatalf("file %+v", got)
	}
	info, err := os.Stat(s.Path())
	if err != nil || info.Mode().Perm() != 0o600 || *synced != 1 {
		t.Fatalf("file %v %v, synced %d", info, err, *synced)
	}
	if entries, _ := os.ReadDir(filepath.Dir(s.Path())); len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
	// Current is a copy.
	s.Current().Items[0].Label = "changed"
	if s.Current().Items[0].Label != "team" {
		t.Fatal("Current shares its list")
	}
}

// Each committed batch counts the revision, and each entry records the
// revision that last changed it.
func TestOrganizationsBatchRevisions(t *testing.T) {
	s, _ := newOrganizationStore(t)
	if _, err := s.Apply(OrganizationsBatch{Ops: []OrganizationOp{addOp("one", orgOne), addOp("two", orgTwo)}}, orgNow); err != nil {
		t.Fatal(err)
	}
	later := orgNow.Add(time.Minute)
	result, err := s.Apply(OrganizationsBatch{BaseRevision: orgRev(0), Ops: []OrganizationOp{renameOp(orgOne, "One")}}, later)
	if err != nil || result != (OrganizationsResult{Revision: orgFirst + 1, Renamed: 1}) {
		t.Fatalf("rename: %+v, %v", result, err)
	}
	o := s.Current()
	if o.Revision != orgFirst+1 || o.Items[0] != (Organization{Label: "One", OrganizationID: orgOne, Rev: orgFirst + 1, UpdatedAt: later}) ||
		o.Items[1] != (Organization{Label: "two", OrganizationID: orgTwo, Rev: orgFirst, UpdatedAt: orgNow}) {
		t.Fatalf("after rename: %+v", o)
	}
	result, err = s.Apply(OrganizationsBatch{BaseRevision: orgRev(1), Ops: []OrganizationOp{removeOp(orgOne), addOp("three", orgThree)}}, later.Add(time.Minute))
	if err != nil || result != (OrganizationsResult{Revision: orgFirst + 2, Added: 1, Removed: 1}) {
		t.Fatalf("remove: %+v, %v", result, err)
	}
	o = s.Current()
	if o.Revision != orgFirst+2 || len(o.Items) != 2 || o.Items[0].OrganizationID != orgTwo || o.Items[1] != (Organization{Label: "three", OrganizationID: orgThree, Rev: orgFirst + 2, UpdatedAt: later.Add(time.Minute)}) {
		t.Fatalf("after remove: %+v", o)
	}
	if again := OpenOrganizations(snapshotIn(filepath.Dir(s.Path()))).Current(); !reflect.DeepEqual(again, o) {
		t.Fatalf("reopened %+v", again)
	}
	// A batch that leaves the list as it was writes nothing.
	before := readFile(t, s.Path())
	result, err = s.Apply(OrganizationsBatch{BaseRevision: orgRev(2), Ops: []OrganizationOp{renameOp(orgTwo, "two")}}, later.Add(2*time.Minute))
	if err != nil || result != (OrganizationsResult{Revision: orgFirst + 2, Unchanged: true}) || !bytes.Equal(readFile(t, s.Path()), before) {
		t.Fatalf("unchanged: %+v, %v", result, err)
	}
	// A removed organization added back keeps its account id, so the values
	// saved for it apply again.
	if _, err := s.Apply(OrganizationsBatch{BaseRevision: orgRev(2), Ops: []OrganizationOp{removeOp(orgTwo)}}, later); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(OrganizationsBatch{BaseRevision: orgRev(3), Ops: []OrganizationOp{addOp("two again", strings.ToUpper(orgTwo))}}, later); err != nil {
		t.Fatal(err)
	}
	if items := s.Current().Items; items[len(items)-1].ID() != client.APICreditOrgAccount(orgTwo) {
		t.Fatalf("added back as %+v", items)
	}
}

// Every error code, with the op's index and field, and a batch with one bad
// op writes none of its ops.
func TestOrganizationsBatchErrors(t *testing.T) {
	s, _ := newOrganizationStore(t)
	if _, err := s.Apply(OrganizationsBatch{Ops: []OrganizationOp{addOp("one", orgOne), addOp("two", orgTwo)}}, orgNow); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		ops  []OrganizationOp
		want OpError
	}{
		"an empty label":            {[]OrganizationOp{addOp("   ", orgThree)}, OpError{CodeInvalidLabel, 0, "label"}},
		"a long label":              {[]OrganizationOp{addOp(strings.Repeat("x", 65), orgThree)}, OpError{CodeInvalidLabel, 0, "label"}},
		"a label with a newline":    {[]OrganizationOp{addOp("a\nb", orgThree)}, OpError{CodeInvalidLabel, 0, "label"}},
		"a label not UTF-8":         {[]OrganizationOp{addOp("a\xffb", orgThree)}, OpError{CodeInvalidLabel, 0, "label"}},
		"a label taken":             {[]OrganizationOp{addOp("ONE", orgThree)}, OpError{CodeDuplicateLabel, 0, "label"}},
		"a label taken in a batch":  {[]OrganizationOp{addOp("three", orgThree), renameOp(orgOne, "Three")}, OpError{CodeDuplicateLabel, 1, "label"}},
		"a rename to a taken label": {[]OrganizationOp{renameOp(orgOne, "two")}, OpError{CodeDuplicateLabel, 0, "label"}},
		"a bad rename label":        {[]OrganizationOp{renameOp(orgOne, "")}, OpError{CodeInvalidLabel, 0, "label"}},
		"not an organization":       {[]OrganizationOp{addOp("three", "org-123")}, OpError{CodeInvalidOrganizationID, 0, "organizationId"}},
		"the nil organization":      {[]OrganizationOp{addOp("three", "00000000-0000-0000-0000-000000000000")}, OpError{CodeInvalidOrganizationID, 0, "organizationId"}},
		"an organization listed":    {[]OrganizationOp{addOp("three", strings.ToUpper(orgOne))}, OpError{CodeDuplicateOrganization, 0, "organizationId"}},
		"an organization twice":     {[]OrganizationOp{addOp("three", orgThree), addOp("four", " "+orgThree)}, OpError{CodeDuplicateOrganization, 1, "organizationId"}},
		"an unknown rename":         {[]OrganizationOp{renameOp(orgThree, "three")}, OpError{CodeUnknownID, 0, "id"}},
		"an unknown remove":         {[]OrganizationOp{{Op: OpRemove, ID: "org-000000000000"}}, OpError{CodeUnknownID, 0, "id"}},
		"a rename after a remove":   {[]OrganizationOp{removeOp(orgOne), renameOp(orgOne, "uno")}, OpError{CodeUnknownID, 1, "id"}},
		"an unknown op":             {[]OrganizationOp{{Op: "move", ID: client.APICreditOrgAccount(orgOne)}}, OpError{"invalid_request", 0, "op"}},
	} {
		before := readFile(t, s.Path())
		_, err := s.Apply(OrganizationsBatch{BaseRevision: orgRev(0), Ops: tc.ops}, orgNow)
		var fail *OpError
		if !errors.As(err, &fail) || *fail != tc.want {
			t.Errorf("%s: %v, want %+v", name, err, tc.want)
		}
		if !bytes.Equal(readFile(t, s.Path()), before) || s.Current().Revision != orgFirst || len(s.Current().Items) != 2 {
			t.Errorf("%s: wrote", name)
		}
	}
	// Past MaxOrganizations: the add that would make the seventeenth.
	ops := []OrganizationOp{}
	for i := 3; i <= MaxOrganizations+1; i++ {
		ops = append(ops, addOp(fmt.Sprintf("org %d", i), fmt.Sprintf("00000000-0000-4000-8000-%012d", i)))
	}
	_, err := s.Apply(OrganizationsBatch{BaseRevision: orgRev(0), Ops: ops[:MaxOrganizations-1]}, orgNow)
	var fail *OpError
	if !errors.As(err, &fail) || *fail != (OpError{CodeTooManyOrganizations, MaxOrganizations - 2, ""}) {
		t.Fatalf("seventeen: %v", err)
	}
	if _, err := s.Apply(OrganizationsBatch{BaseRevision: orgRev(0), Ops: ops[:MaxOrganizations-2]}, orgNow); err != nil || len(s.Current().Items) != MaxOrganizations {
		t.Fatalf("sixteen: %v", err)
	}
	// A page on an older revision: conflict, with the list as it is now.
	_, err = s.Apply(OrganizationsBatch{BaseRevision: orgRev(0), Ops: []OrganizationOp{removeOp(orgOne)}}, orgNow)
	var conflict *OrganizationsConflictError
	if !errors.As(err, &conflict) || conflict.Revision != orgRev(1) || !reflect.DeepEqual(conflict.Organizations, s.Current()) {
		t.Fatalf("conflict: %v", err)
	}
	if _, err := OpenOrganizations(snapshotIn(t.TempDir())).Apply(OrganizationsBatch{BaseRevision: "1", Ops: []OrganizationOp{addOp("a", orgOne)}}, orgNow); !errors.As(err, &conflict) || conflict.Revision != "" {
		t.Fatalf("no file: %v", err)
	}
}

// Thirty committed batches a minute, its own count apart from the settings
// file's, then 429 until the minute has passed.
func TestOrganizationsWritesAreThrottled(t *testing.T) {
	s, _ := newOrganizationStore(t)
	if _, err := s.Apply(OrganizationsBatch{Ops: []OrganizationOp{addOp("one", orgOne)}}, orgNow); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < writeLimit; i++ {
		base := RevisionText(s.Current().Revision)
		if _, err := s.Apply(OrganizationsBatch{BaseRevision: base, Ops: []OrganizationOp{renameOp(orgOne, fmt.Sprintf("one %d", i))}}, orgNow.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	base := RevisionText(s.Current().Revision)
	if _, err := s.Apply(OrganizationsBatch{BaseRevision: base, Ops: []OrganizationOp{renameOp(orgOne, "late")}}, orgNow.Add(40*time.Second)); !errors.Is(err, ErrTooManyWrites) {
		t.Fatalf("past the limit: %v", err)
	}
	// A clock set back forgets the commits it put ahead of now.
	if _, err := s.Apply(OrganizationsBatch{BaseRevision: base, Ops: []OrganizationOp{renameOp(orgOne, "earlier")}}, orgNow.Add(-time.Hour)); err != nil {
		t.Fatalf("a clock set back: %v", err)
	}
	base = RevisionText(s.Current().Revision)
	if _, err := s.Apply(OrganizationsBatch{BaseRevision: base, Ops: []OrganizationOp{renameOp(orgOne, "later")}}, orgNow.Add(61*time.Second)); err != nil {
		t.Fatalf("after the minute: %v", err)
	}
}

// A commit decodes the bytes it is about to write, and a failed one changes
// nothing, on disk or in memory.
func TestOrganizationsCommitIsCheckedAndAtomic(t *testing.T) {
	s, _ := newOrganizationStore(t)
	written := false
	s.write = func(path string, raw []byte) error {
		written = true
		return writeFile(path, raw)
	}
	bad := Organizations{State: client.OrganizationsStateOK, Revision: 1, Items: []Organization{{Label: "a\tb", OrganizationID: orgOne, Rev: 1, UpdatedAt: orgNow}}}
	if err := s.commit(bad); !errors.Is(err, ErrUnwritable) || written {
		t.Fatalf("an unloadable file: %v, written %v", err, written)
	}
	if _, err := s.Apply(OrganizationsBatch{Ops: []OrganizationOp{addOp("one", orgOne)}}, orgNow); err != nil || !written {
		t.Fatalf("a loadable file: %v", err)
	}
	before := readFile(t, s.Path())
	s.write = func(string, []byte) error { return errors.New("disk full") }
	if _, err := s.Apply(OrganizationsBatch{BaseRevision: orgRev(0), Ops: []OrganizationOp{addOp("two", orgTwo)}}, orgNow); !errors.Is(err, ErrUnwritable) {
		t.Fatalf("a failed commit: %v", err)
	}
	if o := s.Current(); o.Revision != orgFirst || len(o.Items) != 1 || !bytes.Equal(readFile(t, s.Path()), before) {
		t.Fatalf("a failed commit moved something: %+v", o)
	}
}

// Close waits for a save in flight, then refuses every later write.
func TestOrganizationsCloseWaitsAndRefuses(t *testing.T) {
	s, _ := newOrganizationStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	s.write = func(path string, raw []byte) error {
		close(entered)
		<-release
		return writeFile(path, raw)
	}
	saved := make(chan error, 1)
	go func() {
		_, err := s.Apply(OrganizationsBatch{Ops: []OrganizationOp{addOp("one", orgOne)}}, orgNow)
		saved <- err
	}()
	<-entered
	closed := make(chan struct{})
	go func() {
		s.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while a save was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
	<-closed
	if _, err := s.Apply(OrganizationsBatch{BaseRevision: "1", Ops: []OrganizationOp{addOp("two", orgTwo)}}, orgNow); !errors.Is(err, ErrClosed) {
		t.Fatalf("a save after Close: %v", err)
	}
	if err := os.Remove(s.Path()); err != nil {
		t.Fatal(err)
	}
	if s.Sync() || len(s.Current().Items) != 1 {
		t.Fatal("a closed store read its file again")
	}
}

func TestParseOrganizationsBatch(t *testing.T) {
	b, err := ParseOrganizationsBatch([]byte(`{"kind":"organizations","baseRevision":"4","ops":[
		{"op":"add","label":"team","organizationId":"` + orgTwo + `"},
		{"op":"rename","id":"org-1a2b3c4d5e6f","label":"personal-one"},
		{"op":"remove","id":"org-0f9e8d7c6b5a"}]}`))
	want := OrganizationsBatch{BaseRevision: "4", Ops: []OrganizationOp{
		{Op: OpAdd, Label: "team", OrganizationID: orgTwo},
		{Op: OpRename, ID: "org-1a2b3c4d5e6f", Label: "personal-one"},
		{Op: OpRemove, ID: "org-0f9e8d7c6b5a"},
	}}
	if err != nil || !reflect.DeepEqual(b, want) {
		t.Fatalf("%+v %v", b, err)
	}
	add := `{"op":"add","label":"team","organizationId":"` + orgTwo + `"}`
	body := func(ops ...string) string {
		return `{"kind":"organizations","baseRevision":"","ops":[` + strings.Join(ops, ",") + `]}`
	}
	many := make([]string, MaxOrganizationOps+1)
	for i := range many {
		many[i] = add
	}
	if _, err := ParseOrganizationsBatch([]byte(body(many[:MaxOrganizationOps]...))); err != nil {
		t.Errorf("sixteen ops: %v", err)
	}
	for name, raw := range map[string]string{
		"empty":                ``,
		"null":                 `null`,
		"an array":             `[` + add + `]`,
		"no kind":              `{"baseRevision":"","ops":[` + add + `]}`,
		"another kind":         `{"kind":"apiCredits","baseRevision":"","ops":[` + add + `]}`,
		"no baseRevision":      `{"kind":"organizations","ops":[` + add + `]}`,
		"a numeric base":       `{"kind":"organizations","baseRevision":4,"ops":[` + add + `]}`,
		"a padded base":        `{"kind":"organizations","baseRevision":"04","ops":[` + add + `]}`,
		"no ops":               `{"kind":"organizations","baseRevision":""}`,
		"null ops":             `{"kind":"organizations","baseRevision":"","ops":null}`,
		"zero ops":             body(),
		"seventeen ops":        body(many...),
		"an unknown top key":   `{"kind":"organizations","baseRevision":"","ops":[` + add + `],"force":true}`,
		"an unknown op":        body(`{"op":"move","id":"org-1a2b3c4d5e6f"}`),
		"no op":                body(`{"label":"team","organizationId":"` + orgTwo + `"}`),
		"a null op":            body(`{"op":null,"id":"org-1a2b3c4d5e6f"}`),
		"an op not an object":  body(`"add"`),
		"add without a label":  body(`{"op":"add","organizationId":"` + orgTwo + `"}`),
		"add with a null one":  body(`{"op":"add","label":null,"organizationId":"` + orgTwo + `"}`),
		"add with an id":       body(`{"op":"add","id":"org-1a2b3c4d5e6f","label":"team","organizationId":"` + orgTwo + `"}`),
		"add with a number":    body(`{"op":"add","label":"team","organizationId":2}`),
		"rename without label": body(`{"op":"rename","id":"org-1a2b3c4d5e6f"}`),
		"rename with an org":   body(`{"op":"rename","id":"org-1a2b3c4d5e6f","label":"x","organizationId":"` + orgTwo + `"}`),
		"remove without an id": body(`{"op":"remove"}`),
		"remove with a label":  body(`{"op":"remove","id":"org-1a2b3c4d5e6f","label":"x"}`),
		"trailing data":        body(add) + ` {}`,
	} {
		if _, err := ParseOrganizationsBatch([]byte(raw)); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A batch judges the organizations file as it is on disk, read again when it
// changed since the last tick: one deleted conflicts and stays deleted, with
// nothing put back from the configuration, and one replaced by a file that
// fails its rules is unavailable and left as it is.
func TestAnOrganizationsBatchSeesTheFileAsItIsOnDisk(t *testing.T) {
	s, _ := newOrganizationStore(t)
	if _, err := s.Apply(OrganizationsBatch{Ops: []OrganizationOp{addOp("one", orgOne)}}, orgNow); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.Path()); err != nil {
		t.Fatal(err)
	}
	var conflict *OrganizationsConflictError
	if _, err := s.Apply(OrganizationsBatch{BaseRevision: orgRev(0), Ops: []OrganizationOp{renameOp(orgOne, "uno")}}, orgNow); !errors.As(err, &conflict) ||
		conflict.Revision != "" || len(conflict.Organizations.Items) != 0 {
		t.Fatalf("a rename after the file was deleted: %v", err)
	}
	if exists(s.Path()) || s.Current().State != client.OrganizationsStateMissing {
		t.Fatalf("the deleted file was written again, or is listed: %+v", s.Current())
	}
	// A page that read the empty list adds the organization again. The new
	// file, in the same second, starts past the deleted file's revision, so
	// the page that read that file still conflicts.
	if _, err := s.Apply(OrganizationsBatch{Ops: []OrganizationOp{addOp("one", orgOne)}}, orgNow); err != nil {
		t.Fatalf("an add after the delete: %v", err)
	}
	if _, err := s.Apply(OrganizationsBatch{BaseRevision: orgRev(0), Ops: []OrganizationOp{renameOp(orgOne, "uno")}}, orgNow); !errors.As(err, &conflict) ||
		conflict.Revision != orgRev(1) || conflict.Organizations.Items[0].Label != "one" {
		t.Fatalf("a rename against the deleted file, after the add: %v", err)
	}

	bad := []byte(`{"schema":1}`)
	if err := os.WriteFile(s.Path(), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(OrganizationsBatch{BaseRevision: orgRev(1), Ops: []OrganizationOp{renameOp(orgOne, "uno")}}, orgNow); !errors.Is(err, ErrOrganizationsUnavailable) {
		t.Fatalf("a rename over an unreadable file: %v", err)
	}
	if after, _ := os.ReadFile(s.Path()); !bytes.Equal(after, bad) || s.Current().State != client.OrganizationsStateUnreadable {
		t.Fatalf("overwritten, or the list missed it: %q %+v", after, s.Current())
	}
}

// A batch that read the file again leaves the next Sync to report it, once,
// whatever the batch came to: the stamp no longer says the file changed, and
// the caller's item list has not followed it. A batch that read nothing again
// leaves Sync nothing to report, a saved one included.
func TestSyncReportsWhatABatchReadAgain(t *testing.T) {
	s, _ := newOrganizationStore(t)
	if _, err := s.Apply(OrganizationsBatch{Ops: []OrganizationOp{addOp("one", orgOne)}}, orgNow); err != nil {
		t.Fatal(err)
	}
	if s.Sync() {
		t.Fatal("Sync reported a batch that read nothing again")
	}
	if err := os.Remove(s.Path()); err != nil {
		t.Fatal(err)
	}
	var conflict *OrganizationsConflictError
	if _, err := s.Apply(OrganizationsBatch{BaseRevision: orgRev(0), Ops: []OrganizationOp{renameOp(orgOne, "uno")}}, orgNow); !errors.As(err, &conflict) {
		t.Fatalf("a rename after the file was deleted: %v", err)
	}
	if !s.Sync() || s.Sync() || s.Current().State != client.OrganizationsStateMissing {
		t.Fatalf("after a conflict with the deleted file: %+v", s.Current())
	}
	raw, err := json.Marshal(validOrganizations())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := s.Apply(OrganizationsBatch{BaseRevision: "4", Ops: []OrganizationOp{renameOp(orgOne, "personal-1")}}, orgNow)
	if err != nil || !result.Unchanged {
		t.Fatalf("an unchanged batch over a file put back: %+v %v", result, err)
	}
	if !s.Sync() || s.Sync() || s.Current().Revision != 4 || len(s.Current().Items) != 2 {
		t.Fatalf("after an unchanged batch over a file put back: %+v", s.Current())
	}
	if _, err := s.Apply(OrganizationsBatch{BaseRevision: "4", Ops: []OrganizationOp{addOp("three", orgThree)}}, orgNow); err != nil {
		t.Fatal(err)
	}
	if s.Sync() {
		t.Fatal("Sync reported a batch saved over the file it had read")
	}
}

// A file written where there was none never takes a revision an earlier file
// had, so a page that read the earlier one conflicts rather than changing a
// list it never showed: not after a delete and an add in the same second, and
// not after a restart that forgot the deleted file.
func TestARecreatedOrganizationsFileStartsPastEveryEarlierRevision(t *testing.T) {
	add := func(s *OrganizationStore, now time.Time) {
		t.Helper()
		if _, err := s.Apply(OrganizationsBatch{Ops: []OrganizationOp{addOp("one", orgOne)}}, now); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := snapshotIn(t.TempDir())
	s := OpenOrganizations(snapshot)
	add(s, orgNow)
	if s.Current().Revision != orgFirst {
		t.Fatalf("the first add: %+v", s.Current())
	}
	// What a page read before any delete, and holds while its card is open.
	read := RevisionText(s.Current().Revision)
	stale := func(step string, s *OrganizationStore, want uint64) {
		t.Helper()
		var conflict *OrganizationsConflictError
		if _, err := s.Apply(OrganizationsBatch{BaseRevision: read, Ops: []OrganizationOp{removeOp(orgOne)}}, orgNow); !errors.As(err, &conflict) || conflict.Revision != RevisionText(want) {
			t.Fatalf("%s: %v, want a conflict at %d", step, err, want)
		}
		if len(s.Current().Items) == 0 {
			t.Fatalf("%s: the stale remove applied", step)
		}
	}
	remove := func() {
		t.Helper()
		if err := os.Remove(s.Path()); err != nil {
			t.Fatal(err)
		}
		s.Sync()
	}

	// Deleted and added again within the second.
	remove()
	add(s, orgNow)
	stale("added again", s, orgFirst+1)

	// Deleted while stopped: a new store has never seen the old file, and the
	// time is past it.
	remove()
	s = OpenOrganizations(snapshot)
	add(s, orgNow.Add(time.Minute))
	stale("added after a restart", s, orgFirst+60)

	// Deleted again, and added again with the clock behind.
	remove()
	add(s, orgNow)
	stale("added with the clock behind", s, orgFirst+61)
	if o := LoadOrganizations(s.Path()); o.Revision != orgFirst+61 || o.Items[0].Rev != orgFirst+61 {
		t.Fatalf("the file %+v", o)
	}
}

// A good copy put back over a broken file, as the docs advise, brings back a
// lower revision. The next batch goes past the highest revision the store has
// seen, not past the copy's, so a page that read a later file and holds its
// card open conflicts rather than matching a revision given out again.
func TestABatchOverAnOlderCopyGoesPastEveryRevisionSeen(t *testing.T) {
	snapshot := snapshotIn(t.TempDir())
	writeOrganizations(t, snapshot, validOrganizations())
	s := OpenOrganizations(snapshot)
	backup := readFile(t, s.Path())
	// What a page read at revision 5, and holds while its card is open.
	if _, err := s.Apply(OrganizationsBatch{BaseRevision: "4", Ops: []OrganizationOp{renameOp(orgOne, "uno")}}, orgNow); err != nil {
		t.Fatal(err)
	}
	held := RevisionText(s.Current().Revision)
	for i, label := range []string{"prod", "staging", "prod again", "prod"} {
		if _, err := s.Apply(OrganizationsBatch{BaseRevision: RevisionText(s.Current().Revision), Ops: []OrganizationOp{renameOp(orgOne, label)}}, orgNow); err != nil {
			t.Fatalf("batch %d: %v", i, err)
		}
	}
	if s.Current().Revision != 9 {
		t.Fatalf("after the batches: %+v", s.Current())
	}

	// Broken by a hand edit, then the revision-4 copy put back.
	place(t, s.Path(), []byte(`{"schema":1}`))
	if !s.Sync() || s.Current().State != client.OrganizationsStateUnreadable {
		t.Fatalf("broken: %+v", s.Current())
	}
	place(t, s.Path(), backup)
	if !s.Sync() || s.Current().Revision != 4 {
		t.Fatalf("put back: %+v", s.Current())
	}

	// A page that read the copy saves, past every revision seen.
	result, err := s.Apply(OrganizationsBatch{BaseRevision: "4", Ops: []OrganizationOp{addOp("three", orgThree)}}, orgNow)
	if err != nil || result.Revision != 10 {
		t.Fatalf("a batch over the copy: %+v, %v", result, err)
	}
	if o := LoadOrganizations(s.Path()); o.Revision != 10 || o.Items[len(o.Items)-1].Rev != 10 {
		t.Fatalf("the file %+v", o)
	}

	// The page still holding revision 5 conflicts.
	var conflict *OrganizationsConflictError
	if _, err := s.Apply(OrganizationsBatch{BaseRevision: held, Ops: []OrganizationOp{renameOp(orgOne, "staging")}}, orgNow); !errors.As(err, &conflict) || conflict.Revision != "10" {
		t.Fatalf("a batch held across the put back: %v, want a conflict at 10", err)
	}
	if label := s.Current().Items[0].Label; label == "staging" {
		t.Fatalf("the held rename applied: %q", label)
	}
}

// A file that could not be opened or read, as one owned by another user, is
// read again at every tick, and by a save, until a read settles, though its
// stamp never changed: fixing its owner changes none of what the stamp
// compares. One that fails a load rule is read again only once it changes.
func TestAnOrganizationsFileThatCouldNotBeOpenedIsReadAgain(t *testing.T) {
	snapshot := snapshotIn(t.TempDir())
	writeOrganizations(t, snapshot, validOrganizations())
	s := OpenOrganizations(snapshot)
	reads, fail := 0, true
	s.read = func(path string, stamp fileStamp, limit int64) ([]byte, bool, bool) {
		reads++
		if fail {
			return nil, false, false
		}
		return readStamped(path, stamp, limit)
	}
	touched := orgNow.Add(time.Hour)
	if err := os.Chtimes(s.Path(), touched, touched); err != nil {
		t.Fatal(err)
	}
	if !s.Sync() || s.Current().State != client.OrganizationsStateUnreadable {
		t.Fatalf("a failed open: %+v", s.Current())
	}
	// Still failing: read again, and nothing to report.
	if s.Sync() || s.Sync() || reads != 3 || s.Current().State != client.OrganizationsStateUnreadable {
		t.Fatalf("still failing: %d reads, %+v", reads, s.Current())
	}
	fail = false
	if !s.Sync() || s.Current().State != client.OrganizationsStateOK || s.Current().Revision != 4 {
		t.Fatalf("fixed: %+v", s.Current())
	}
	if s.Sync() || reads != 4 {
		t.Fatalf("a settled read was read again: %d reads", reads)
	}

	// A save reads it again first.
	fail = true
	if err := os.Chtimes(s.Path(), touched.Add(time.Hour), touched.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(OrganizationsBatch{BaseRevision: "4", Ops: []OrganizationOp{removeOp(orgTwo)}}, orgNow); !errors.Is(err, ErrOrganizationsUnavailable) {
		t.Fatalf("a save over a failed open: %v", err)
	}
	fail = false
	if _, err := s.Apply(OrganizationsBatch{BaseRevision: "4", Ops: []OrganizationOp{removeOp(orgTwo)}}, orgNow); err != nil || len(s.Current().Items) != 1 {
		t.Fatalf("a save once it opens: %v", err)
	}

	// A rule failure settles: no tick reads it again until it changes.
	writeOrganizations(t, snapshot, []byte(`{"schema":1}`))
	reads = 0
	if !s.Sync() || s.Sync() || s.Sync() || reads != 1 || s.Current().State != client.OrganizationsStateUnreadable {
		t.Fatalf("a rule failure: %d reads, %+v", reads, s.Current())
	}
}

func TestUnreadableOrganizationsReopenOnTheNextTick(t *testing.T) {
	snapshot := snapshotIn(t.TempDir())
	writeOrganizations(t, snapshot, []byte(`{"schema":1}`))
	s := OpenOrganizations(snapshot)
	if s.Current().State != client.OrganizationsStateUnreadable || s.Sync() {
		t.Fatalf("open: %+v", s.Current())
	}
	if err := os.Rename(s.Path(), s.Path()+".aside"); err != nil {
		t.Fatal(err)
	}
	if !s.Sync() || s.Current().State != client.OrganizationsStateMissing {
		t.Fatalf("moved aside: %+v", s.Current())
	}
	if _, err := s.Apply(OrganizationsBatch{Ops: []OrganizationOp{addOp("one", orgOne)}}, orgNow); err != nil || s.Current().State != client.OrganizationsStateOK {
		t.Fatalf("a save once moved aside: %v", err)
	}
	// A good file put back in place of a bad one is read too.
	writeOrganizations(t, snapshot, []byte(`{"schema":1}`))
	if !s.Sync() || s.Current().State != client.OrganizationsStateUnreadable {
		t.Fatalf("broken again: %+v", s.Current())
	}
	writeOrganizations(t, snapshot, validOrganizations())
	if !s.Sync() || s.Current().State != client.OrganizationsStateOK || s.Current().Revision != 4 {
		t.Fatalf("fixed: %+v", s.Current())
	}
}
