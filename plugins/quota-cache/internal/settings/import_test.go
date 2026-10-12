package settings

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

var (
	importNow = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	// fiveOrgA is the five-organization fixture's first account, with a
	// monthly credit of 200.
	fiveOrgA = client.APICreditOrgAccount("00000000-0000-4000-8000-0000000000c1")
)

// importEnv is a data layout for the import tests: the snapshot in <root>/qc,
// so the drop-in is <root>/qc/quota-glance-settings.json and Quota Glance's
// sibling data-dir file is <root>/quota-glance/settings.json.
type importEnv struct {
	root, snapshot, dropIn, sibling string
}

func newImportEnv(t *testing.T) importEnv {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "qc"), 0o700); err != nil {
		t.Fatal(err)
	}
	return importEnv{
		root:     root,
		snapshot: filepath.Join(root, "qc", "snapshot.json"),
		dropIn:   filepath.Join(root, "qc", client.LegacySettingsDropIn),
		sibling:  filepath.Join(root, "quota-glance", "settings.json"),
	}
}

// place writes raw at path the way Quota Glance saves: a new file renamed over
// the old one.
func place(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".new", raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
}

func legacyFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "legacy", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// atRevision is the settings file raw at revision, with fiveOrgA's monthly
// credit set to monthly by that revision, and no rev past it.
func atRevision(t *testing.T, raw []byte, revision uint64, monthly string) []byte {
	t.Helper()
	return savedAt(t, raw, revision, monthly, importNow)
}

// savedAt is atRevision with fiveOrgA's row saved at at, as Quota Glance
// stamps each row it saves with the time.
func savedAt(t *testing.T, raw []byte, revision uint64, monthly string, at time.Time) []byte {
	t.Helper()
	v, err := decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	next := v.clone()
	next.Revision = revision
	for id, credit := range next.APICredits {
		credit.Rev = min(credit.Rev, revision)
		next.APICredits[id] = credit
	}
	for id, renewal := range next.Renewals {
		renewal.Rev = min(renewal.Rev, revision)
		next.Renewals[id] = renewal
	}
	credit := next.APICredits[fiveOrgA]
	credit.MonthlyUSD, credit.Rev, credit.UpdatedAt = monthly, revision, at
	next.APICredits[fiveOrgA] = credit
	out, err := encode(next)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func recordAt(t *testing.T, snapshot string) record {
	t.Helper()
	r, err := decodeRecord(readFile(t, RecordPath(snapshot)))
	if err != nil {
		t.Fatalf("the import record does not load: %v", err)
	}
	return r
}

// recordWrites records the base name of every file the store writes, in
// order, and fails a record write while failRecord says so.
func recordWrites(s *Store, failRecord *bool) *[]string {
	order := &[]string{}
	s.write = func(path string, raw []byte) error {
		if failRecord != nil && *failRecord && path == s.RecordPath() {
			return errors.New("injected: the record write fails")
		}
		*order = append(*order, filepath.Base(path))
		return writeFile(path, raw)
	}
	return order
}

func renewalSave(id, base, date string) Batch {
	return Batch{Kind: KindRenewals, Renewals: []RenewalItem{{ID: id, BaseRevision: base, Date: ptr(date)}}}
}

func importState(s *Store) string {
	if imp := s.Status().Import; imp != nil {
		return imp.State
	}
	return "null"
}

// The first start copies Quota Glance's file verbatim, records it, and leaves
// it exactly as it was.
func TestImportCopiesTheLegacyBytes(t *testing.T) {
	e := newImportEnv(t)
	legacy := legacyFixture(t, "five-orgs")
	place(t, e.sibling, legacy)
	if err := os.Chmod(e.sibling, 0o640); err != nil {
		t.Fatal(err)
	}
	modified := time.Date(2026, 10, 10, 18, 20, 30, 0, time.UTC)
	if err := os.Chtimes(e.sibling, modified, modified); err != nil {
		t.Fatal(err)
	}
	s := Open(e.snapshot)
	result := s.Sync(importNow)
	counts := client.CreditImportCounts{MonthlyCredits: 2, RefillDates: 2, Readings: 2, RenewalDates: 3}
	if result.Imported == nil || *result.Imported != (Imported{Kind: client.CreditImportKindAutomatic, Counts: counts, Revision: 12}) || result.LegacyUnreadable {
		t.Fatalf("result %+v, imported %+v", result, result.Imported)
	}
	if got := readFile(t, s.Path()); !bytes.Equal(got, legacy) {
		t.Fatalf("the settings file is not the legacy bytes:\n%s", got)
	}
	updated := time.Date(2026, 10, 10, 18, 20, 0, 0, time.UTC)
	// The lineage starts at the time: a file from before the import, saved
	// here, is below it, and a save over the imported file goes on from it.
	want := record{
		Kind: client.CreditImportKindAutomatic, SourcePath: e.sibling, SHA256: hashOf(legacy), SettingsSHA256: hashOf(legacy),
		LegacyRevision: 12, LegacyUpdatedAt: &updated, Counts: counts, ImportedAt: importNow, Lineage: []uint64{uint64(importNow.Unix())},
	}
	if got := recordAt(t, e.snapshot); !reflect.DeepEqual(got, want) {
		t.Fatalf("record\n got %+v\nwant %+v", got, want)
	}
	for _, path := range []string{s.Path(), s.RecordPath()} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", path, info, err)
		}
	}
	// Quota Glance's file is untouched: its bytes, its mode and its time.
	info, err := os.Stat(e.sibling)
	if err != nil || !bytes.Equal(readFile(t, e.sibling), legacy) || info.Mode().Perm() != 0o640 || !info.ModTime().Equal(modified) {
		t.Fatalf("the legacy file changed: %v %v", info, err)
	}
	if v, err := decode(legacy); err != nil || !reflect.DeepEqual(s.Current(), v) {
		t.Fatalf("values %+v, %v", s.Current(), err)
	}
	status := s.Status()
	imp := status.Import
	if status.State != client.CreditSettingsOK || imp == nil || imp.State != client.CreditImportStateImported || !imp.Following || !imp.Importable ||
		*imp.Kind != client.CreditImportKindAutomatic || *imp.SourcePath != e.sibling || *imp.LegacyRevision != "12" ||
		!imp.LegacyUpdatedAt.Equal(updated) || *imp.Counts != counts || !imp.ImportedAt.Equal(importNow) {
		t.Fatalf("status %+v, import %+v", status, imp)
	}
	if status.LegacyCounts == nil || *status.LegacyCounts != counts || status.LegacyUpdatedAt == nil || !status.LegacyUpdatedAt.Equal(updated) {
		t.Fatalf("legacy counts %+v, updated %v", status.LegacyCounts, status.LegacyUpdatedAt)
	}
	// The store hands out copies.
	*s.Status().Import.Kind = "changed"
	s.Status().LegacyCounts.Readings = 9
	if *s.Status().Import.Kind != client.CreditImportKindAutomatic || s.Status().LegacyCounts.Readings != 2 {
		t.Fatal("Status shares its import with callers")
	}
}

// The drop-in beats the sibling data-dir, which beats Quota Glance's default
// against the working directory; a drop-in that cannot be read blocks rather
// than falling through; and a path listed twice is one candidate.
func TestImportCandidatesInOrder(t *testing.T) {
	a, b, c := legacyFixture(t, "api-credits"), legacyFixture(t, "seven-credentials"), legacyFixture(t, "degraded-states")
	setup := func(t *testing.T) (importEnv, string) {
		t.Chdir(t.TempDir())
		l2, err := filepath.Abs(filepath.Join("plugins", "data", "quota-glance", "settings.json"))
		if err != nil {
			t.Fatal(err)
		}
		return newImportEnv(t), l2
	}
	t.Run("the drop-in first", func(t *testing.T) {
		e, l2 := setup(t)
		place(t, e.dropIn, a)
		place(t, e.sibling, b)
		place(t, l2, c)
		s := Open(e.snapshot)
		s.Sync(importNow)
		if !bytes.Equal(readFile(t, s.Path()), a) || recordAt(t, e.snapshot).SourcePath != e.dropIn {
			t.Fatal("did not import the drop-in")
		}
	})
	t.Run("then the sibling", func(t *testing.T) {
		e, l2 := setup(t)
		place(t, e.sibling, b)
		place(t, l2, c)
		s := Open(e.snapshot)
		s.Sync(importNow)
		if !bytes.Equal(readFile(t, s.Path()), b) || recordAt(t, e.snapshot).SourcePath != e.sibling {
			t.Fatal("did not import the sibling")
		}
	})
	t.Run("then the working directory's", func(t *testing.T) {
		e, l2 := setup(t)
		place(t, l2, c)
		s := Open(e.snapshot)
		s.Sync(importNow)
		if !bytes.Equal(readFile(t, s.Path()), c) || recordAt(t, e.snapshot).SourcePath != l2 {
			t.Fatal("did not import Quota Glance's default")
		}
	})
	t.Run("a drop-in that cannot be read blocks", func(t *testing.T) {
		e, l2 := setup(t)
		place(t, e.dropIn, []byte("not json"))
		place(t, e.sibling, b)
		place(t, l2, c)
		s := Open(e.snapshot)
		if s.Status().State != client.CreditSettingsLegacyUnreadable {
			t.Fatalf("open: %+v", s.Status())
		}
		result := s.Sync(importNow)
		if !result.LegacyUnreadable || result.Imported != nil || exists(s.Path()) || exists(s.RecordPath()) ||
			importState(s) != client.CreditImportStateLegacyUnreadable {
			t.Fatalf("result %+v, status %+v", result, s.Status())
		}
	})
	t.Run("a path listed twice is one candidate", func(t *testing.T) {
		t.Chdir(t.TempDir())
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		snapshot := filepath.Join(cwd, "plugins", "data", "quota-cache", "snapshot.json")
		s := Open(snapshot)
		want := []string{filepath.Join(cwd, "plugins", "data", "quota-cache", client.LegacySettingsDropIn), filepath.Join(cwd, "plugins", "data", "quota-glance", "settings.json")}
		if !reflect.DeepEqual(s.candidates, want) {
			t.Fatalf("candidates %v, want %v", s.candidates, want)
		}
		place(t, want[1], b)
		s.Sync(importNow)
		if !bytes.Equal(readFile(t, s.Path()), b) {
			t.Fatal("did not import")
		}
	})
}

// A legacy file that cannot be read blocks the first import: nothing is
// written, nothing stored applies, and nothing may be saved, until it is moved
// aside, which the next tick picks up.
func TestUnreadableLegacyBlocksAndWritesNothing(t *testing.T) {
	good := legacyFixture(t, "five-orgs")
	for name, write := range map[string]func(t *testing.T, e importEnv){
		"a symlink": func(t *testing.T, e importEnv) {
			target := filepath.Join(e.root, "elsewhere.json")
			place(t, target, good)
			if err := os.MkdirAll(filepath.Dir(e.sibling), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, e.sibling); err != nil {
				t.Fatal(err)
			}
		},
		"over 1 MiB": func(t *testing.T, e importEnv) {
			place(t, e.sibling, append([]byte(`{"schema":1,"revision":0}`), bytes.Repeat([]byte(" "), MaxBytes)...))
		},
		"an unknown field": func(t *testing.T, e importEnv) {
			place(t, e.sibling, bytes.Replace(good, []byte("{"), []byte(`{"extra": true,`), 1))
		},
		"schema 2": func(t *testing.T, e importEnv) {
			place(t, e.sibling, bytes.Replace(good, []byte(`"schema": 1`), []byte(`"schema": 2`), 1))
		},
		"a bad baseline": func(t *testing.T, e importEnv) {
			place(t, e.sibling, bytes.Replace(good, []byte(`"dayStart": "2026-10-09T00:00:00Z"`), []byte(`"dayStart": "2026-10-08T00:00:00Z"`), 1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newImportEnv(t)
			write(t, e)
			before, _ := os.ReadFile(e.sibling)
			s := Open(e.snapshot)
			order := recordWrites(s, nil)
			result := s.Sync(importNow)
			status := s.Status()
			if !result.LegacyUnreadable || result.Imported != nil || status.State != client.CreditSettingsLegacyUnreadable ||
				status.Import == nil || status.Import.State != client.CreditImportStateLegacyUnreadable || status.Import.Importable ||
				status.Import.Kind != nil || status.LegacyCounts != nil {
				t.Fatalf("result %+v, status %+v, import %+v", result, status, status.Import)
			}
			if v := s.Current(); v.Unreadable || len(v.APICredits) != 0 || len(v.Renewals) != 0 {
				t.Fatalf("values %+v", v)
			}
			if len(*order) != 0 || exists(s.Path()) || exists(s.RecordPath()) {
				t.Fatalf("wrote %v", *order)
			}
			if _, err := s.Apply(renewalSave(renewalA, "", "2026-11-01"), nil, importNow); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("a save: %v", err)
			}
			if _, err := s.ImportLegacy("", importNow); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("an import: %v", err)
			}
			if after, _ := os.ReadFile(e.sibling); !bytes.Equal(after, before) {
				t.Fatal("the legacy file changed")
			}

			if err := os.Rename(e.sibling, e.sibling+".aside"); err != nil {
				t.Fatal(err)
			}
			result = s.Sync(importNow.Add(10 * time.Second))
			if result.LegacyUnreadable || s.Status().State != client.CreditSettingsOK || s.Status().Import != nil {
				t.Fatalf("moved aside: %+v, %+v", result, s.Status())
			}
			if _, err := s.Apply(renewalSave(renewalA, "", "2026-11-01"), nil, importNow); err != nil {
				t.Fatalf("a save once moved aside: %v", err)
			}
		})
	}
}

// The import runs once, settings file first and record second, and never
// replaces a settings file that is already there.
func TestImportIsIdempotent(t *testing.T) {
	e := newImportEnv(t)
	legacy := legacyFixture(t, "five-orgs")
	place(t, e.sibling, legacy)
	s := Open(e.snapshot)
	order := recordWrites(s, nil)
	first := s.Sync(importNow)
	second := s.Sync(importNow.Add(10 * time.Second))
	if first.Imported == nil || second.Imported != nil {
		t.Fatalf("first %+v, second %+v", first, second)
	}
	if want := []string{"snapshot.settings.json", "snapshot.settings-import.json"}; !reflect.DeepEqual(*order, want) {
		t.Fatalf("writes %v, want %v", *order, want)
	}
	r := recordAt(t, e.snapshot)
	updated := time.Date(2026, 10, 10, 18, 20, 0, 0, time.UTC)
	if r.SettingsSHA256 != hashOf(legacy) || r.LegacyUpdatedAt == nil || !r.LegacyUpdatedAt.Equal(updated) ||
		r.Counts != (client.CreditImportCounts{MonthlyCredits: 2, RefillDates: 2, Readings: 2, RenewalDates: 3}) {
		t.Fatalf("record %+v", r)
	}
	// A fresh start finds the record and imports nothing.
	again := Open(e.snapshot)
	order = recordWrites(again, nil)
	if result := again.Sync(importNow.Add(time.Minute)); result.Imported != nil || len(*order) != 0 {
		t.Fatalf("reopened: %+v, wrote %v", result, *order)
	}

	// A settings file of its own, and no record: never replaced
	// automatically.
	e = newImportEnv(t)
	own := legacyFixture(t, "api-credits")
	if err := os.WriteFile(FilePath(e.snapshot), own, 0o600); err != nil {
		t.Fatal(err)
	}
	place(t, e.sibling, legacy)
	s = Open(e.snapshot)
	order = recordWrites(s, nil)
	if result := s.Sync(importNow); result.Imported != nil || len(*order) != 0 || !bytes.Equal(readFile(t, s.Path()), own) {
		t.Fatalf("own file: %+v, wrote %v", result, *order)
	}
	if imp := s.Status().Import; imp == nil || imp.State != client.CreditImportStateNotImported || !imp.Importable || imp.Following {
		t.Fatalf("own file: import %+v", imp)
	}
}

// While nothing is saved here, a newer save in Quota Glance is copied over;
// the first save here ends that; an older file put back is never copied; and a
// save racing the copy either wins or builds on it.
func TestFollowWhileUntouched(t *testing.T) {
	legacy := legacyFixture(t, "five-orgs")
	e := newImportEnv(t)
	place(t, e.sibling, legacy)
	s := Open(e.snapshot)
	s.Sync(importNow)

	v13 := atRevision(t, legacy, 13, "300")
	place(t, e.sibling, v13)
	result := s.Sync(importNow.Add(10 * time.Second))
	if result.Imported == nil || result.Imported.Kind != client.CreditImportKindFollow || result.Imported.Revision != 13 {
		t.Fatalf("follow: %+v", result)
	}
	if !bytes.Equal(readFile(t, s.Path()), v13) || s.Current().APICredits[fiveOrgA].MonthlyUSD != "300" {
		t.Fatal("follow did not copy verbatim")
	}
	// The record describes the copy, and keeps the automatic import's kind,
	// which it continues.
	if r := recordAt(t, e.snapshot); r.Kind != client.CreditImportKindAutomatic || r.LegacyRevision != 13 || r.SHA256 != hashOf(v13) {
		t.Fatalf("record %+v", r)
	}
	if imp := s.Status().Import; imp.State != client.CreditImportStateImported || !imp.Following {
		t.Fatalf("import %+v", imp)
	}
	// Unchanged, it is not copied again.
	if result := s.Sync(importNow.Add(20 * time.Second)); result.Imported != nil {
		t.Fatalf("copied twice: %+v", result)
	}

	// One save here ends follow.
	if _, err := s.Apply(renewalSave("aaaaaaaaaaaaaaaa", "", "2026-12-01"), nil, importNow.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if imp := s.Status().Import; imp.Following {
		t.Fatal("still following after a save")
	}
	saved := readFile(t, s.Path())
	place(t, e.sibling, atRevision(t, v13, 14, "400"))
	if result := s.Sync(importNow.Add(time.Minute)); result.Imported != nil || !bytes.Equal(readFile(t, s.Path()), saved) {
		t.Fatalf("followed after a save: %+v", result)
	}
	if imp := s.Status().Import; imp.State != client.CreditImportStateChangedSinceImport || imp.Following {
		t.Fatalf("after a save: %+v", imp)
	}

	// An older revision put back is reported, never copied.
	e = newImportEnv(t)
	place(t, e.sibling, legacy)
	s = Open(e.snapshot)
	s.Sync(importNow)
	place(t, e.sibling, atRevision(t, legacy, 11, "100"))
	if result := s.Sync(importNow.Add(10 * time.Second)); result.Imported != nil || !bytes.Equal(readFile(t, s.Path()), legacy) {
		t.Fatalf("an older file was copied: %+v", result)
	}
	if imp := s.Status().Import; imp.State != client.CreditImportStateChangedSinceImport || !imp.Following {
		t.Fatalf("an older file: %+v", imp)
	}

	// A save racing a copy: the save is never lost, and the copy happened
	// exactly when the record says so.
	for i := range 25 {
		e := newImportEnv(t)
		place(t, e.sibling, legacy)
		s := Open(e.snapshot)
		s.Sync(importNow)
		place(t, e.sibling, v13)
		var wg sync.WaitGroup
		var saveErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, saveErr = s.Apply(renewalSave("aaaaaaaaaaaaaaaa", "", "2026-12-01"), nil, importNow.Add(10*time.Second))
		}()
		go func() {
			defer wg.Done()
			s.Sync(importNow.Add(10 * time.Second))
		}()
		wg.Wait()
		v := s.Current()
		followed := recordAt(t, e.snapshot).LegacyRevision == 13
		if saveErr != nil || v.Renewals["aaaaaaaaaaaaaaaa"].Date != "2026-12-01" {
			t.Fatalf("race %d: the save was lost: %v, %+v", i, saveErr, v)
		}
		if followed != (v.APICredits[fiveOrgA].MonthlyUSD == "300") || s.Status().Import.Following {
			t.Fatalf("race %d: followed %v, values %+v, import %+v", i, followed, v, s.Status().Import)
		}
		if result := s.Sync(importNow.Add(20 * time.Second)); result.Imported != nil || Load(s.Path()).Renewals["aaaaaaaaaaaaaaaa"].Date != "2026-12-01" {
			t.Fatalf("race %d: copied over the save: %+v", i, result)
		}
	}
}

// Import Quota Glance's settings replaces the values, past every revision
// either file had, after checking the page's revision.
func TestExplicitReimport(t *testing.T) {
	legacy := legacyFixture(t, "five-orgs")
	// The first save writes a file where there was none, at the time in Unix
	// seconds.
	first := uint64(importNow.Unix())
	e := newImportEnv(t)
	s := Open(e.snapshot)
	s.Sync(importNow)
	if _, err := s.ImportLegacy("", importNow); !errors.Is(err, ErrNotImportable) {
		t.Fatalf("with no legacy file: %v", err)
	}
	if _, err := s.Apply(renewalSave("aaaaaaaaaaaaaaaa", "", "2026-12-01"), nil, importNow); err != nil {
		t.Fatal(err)
	}
	if s.Status().LegacyCounts != nil {
		t.Fatalf("counts with no legacy file: %+v", s.Status().LegacyCounts)
	}
	place(t, e.sibling, legacy)
	s.Sync(importNow.Add(10 * time.Second))
	if imp := s.Status().Import; imp == nil || imp.State != client.CreditImportStateNotImported || !imp.Importable {
		t.Fatalf("before: %+v", imp)
	}
	// What the import would bring, with no record yet to say what one did.
	if counts := s.Status().LegacyCounts; counts == nil || *counts != (client.CreditImportCounts{MonthlyCredits: 2, RefillDates: 2, Readings: 2, RenewalDates: 3}) {
		t.Fatalf("legacy counts before: %+v", counts)
	}
	for _, stale := range []string{"", "7"} {
		var conflict *ConflictError
		if _, err := s.ImportLegacy(stale, importNow); !errors.As(err, &conflict) || conflict.Kind != KindImportLegacy || conflict.Revision != RevisionText(first) {
			t.Fatalf("baseRevision %q: %v", stale, err)
		}
	}
	if Load(s.Path()).Revision != first {
		t.Fatal("a conflict wrote")
	}
	// A minute later, the import writes at the time, past both files'
	// revisions.
	manual := first + 60
	imported, err := s.ImportLegacy(RevisionText(first), importNow.Add(time.Minute))
	if err != nil || imported.Kind != client.CreditImportKindManual || imported.Revision != manual ||
		imported.Counts != (client.CreditImportCounts{MonthlyCredits: 2, RefillDates: 2, Readings: 2, RenewalDates: 3}) {
		t.Fatalf("import: %+v, %v", imported, err)
	}
	want, _ := decode(legacy)
	want = want.clone()
	want.Revision = manual
	for id, credit := range want.APICredits {
		credit.Rev = manual
		want.APICredits[id] = credit
	}
	for id, renewal := range want.Renewals {
		renewal.Rev = manual
		want.Renewals[id] = renewal
	}
	written := readFile(t, s.Path())
	if !reflect.DeepEqual(s.Current(), want) || !reflect.DeepEqual(Load(s.Path()), want) {
		t.Fatalf("values %+v", s.Current())
	}
	if r := recordAt(t, e.snapshot); r.Kind != client.CreditImportKindManual || r.SHA256 != hashOf(legacy) || r.SettingsSHA256 != hashOf(written) ||
		r.SHA256 == r.SettingsSHA256 || r.LegacyRevision != 12 || r.SourcePath != e.sibling {
		t.Fatalf("record %+v", r)
	}
	if imp := s.Status().Import; imp.State != client.CreditImportStateImported || !imp.Following {
		t.Fatalf("after: %+v", imp)
	}

	// With no settings file, baseRevision "" is the one that matches, and the
	// file written starts past every revision the one moved aside had.
	if err := os.Rename(s.Path(), s.Path()+".aside"); err != nil {
		t.Fatal(err)
	}
	s.Sync(importNow.Add(2 * time.Minute))
	if _, err := s.ImportLegacy(RevisionText(manual), importNow); err == nil {
		t.Fatal("a missing file's revision matched the one moved aside")
	}
	refilled := first + 120
	if imported, err := s.ImportLegacy("", importNow.Add(2*time.Minute)); err != nil || imported.Revision != refilled || s.Current().APICredits[fiveOrgA].Rev != refilled {
		t.Fatalf("no settings file: %+v, %v", imported, err)
	}

	// A legacy file that cannot be read, beside a settings file, is not
	// importable; that is a warning, not a block.
	place(t, e.sibling, []byte("{"))
	s.Sync(importNow.Add(3 * time.Minute))
	if s.Status().State != client.CreditSettingsOK || importState(s) != client.CreditImportStateLegacyUnreadable || s.Status().LegacyCounts != nil {
		t.Fatalf("unreadable legacy: %+v", s.Status())
	}
	if _, err := s.ImportLegacy(RevisionText(refilled), importNow); !errors.Is(err, ErrNotImportable) {
		t.Fatalf("unreadable legacy: %v", err)
	}

	// It counts toward the write limit.
	place(t, e.sibling, legacy)
	s.Sync(importNow.Add(4 * time.Minute))
	start := importNow.Add(5 * time.Minute)
	for i := range writeLimit {
		if _, err := s.Apply(renewalSave(renewalA, RevisionText(s.Current().Renewals[renewalA].Rev), []string{"2026-11-01", "2026-11-02"}[i%2]), nil, start); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	if _, err := s.ImportLegacy(RevisionText(s.Current().Revision), start); !errors.Is(err, ErrTooManyWrites) {
		t.Fatalf("past the limit: %v", err)
	}
}

// Import Quota Glance's settings into a store whose file was moved aside, the
// remedy for one that cannot be read, gives every row a revision no page can
// hold: one still holding a row saved after the automatic import conflicts
// rather than saving over the value just imported.
func TestAReimportIntoAMovedAsideStoreConflictsWithAnOpenPage(t *testing.T) {
	e := newImportEnv(t)
	place(t, e.sibling, legacyFixture(t, "five-orgs"))
	s := Open(e.snapshot)
	s.Sync(importNow)
	credit := func(base, monthly string) Batch {
		return Batch{Kind: KindAPICredits, APICredits: []APICreditItem{{ID: fiveOrgA, BaseRevision: base, MonthlyUSD: ptr(monthly)}}}
	}
	if _, err := s.Apply(credit(RevisionText(s.Current().APICredits[fiveOrgA].Rev), "300"), nil, importNow); err != nil {
		t.Fatal(err)
	}
	// Another device holds the card open from here: the time of the save,
	// where the import's lineage starts.
	held := RevisionText(s.Current().APICredits[fiveOrgA].Rev)
	if held != RevisionText(uint64(importNow.Unix())) {
		t.Fatalf("saved at %s", held)
	}
	if err := os.Rename(s.Path(), s.Path()+".aside"); err != nil {
		t.Fatal(err)
	}
	s.Sync(importNow.Add(10 * time.Second))
	imported, err := s.ImportLegacy("", importNow.Add(20*time.Second))
	if err != nil || imported.Revision != uint64(importNow.Unix())+20 {
		t.Fatalf("import: %+v, %v", imported, err)
	}
	var conflict *ConflictError
	if _, err := s.Apply(credit(held, "999"), nil, importNow.Add(30*time.Second)); !errors.As(err, &conflict) || conflict.APICredits[fiveOrgA].MonthlyUSD != "200" {
		t.Fatalf("the stale save: %v, want a conflict", err)
	}
	if got := Load(s.Path()).APICredits[fiveOrgA]; got.MonthlyUSD != "200" || got.Rev != imported.Revision {
		t.Fatalf("stored %+v", got)
	}
}

// Follow after a manual import keeps every revision past what a page could
// hold. Quota Glance's file goes on from its own lower revisions, so a
// verbatim copy would hand a row back a rev this store gave out before the
// import, and a page still holding it would save over the copy without a
// conflict.
func TestFollowAfterAManualImportKeepsRevisionsAhead(t *testing.T) {
	legacy := legacyFixture(t, "five-orgs")
	// A row nobody edits here, at rev 5 in both files after the first import.
	const other = "org-17629bebcd24"
	e := newImportEnv(t)
	place(t, e.sibling, legacy)
	s := Open(e.snapshot)
	s.Sync(importNow)
	held := RevisionText(s.Current().APICredits[other].Rev)
	credit := func(id, base, monthly string) Batch {
		return Batch{Kind: KindAPICredits, APICredits: []APICreditItem{{ID: id, BaseRevision: base, MonthlyUSD: ptr(monthly)}}}
	}
	// Saved here, past the time of the import, then in Quota Glance at its
	// revision 13.
	saved, err := s.Apply(credit(fiveOrgA, RevisionText(s.Current().APICredits[fiveOrgA].Rev), "210"), nil, importNow.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	v13 := atRevision(t, legacy, 13, "220")
	place(t, e.sibling, v13)
	s.Sync(importNow.Add(10 * time.Second))
	if importState(s) != client.CreditImportStateChangedSinceImport {
		t.Fatalf("before the import: %+v", s.Status().Import)
	}
	imported, err := s.ImportLegacy(RevisionText(saved.Revision), importNow.Add(20*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if imported.Revision <= saved.Revision || s.Current().Revision != imported.Revision || !s.Status().Import.Following {
		t.Fatalf("the import: %d %+v", s.Current().Revision, s.Status().Import)
	}

	// Quota Glance saves again, at its revision 14, where the other row is
	// still at rev 5. It is followed, past both files' revisions, at the
	// time.
	v14 := atRevision(t, v13, 14, "230")
	place(t, e.sibling, v14)
	result := s.Sync(importNow.Add(30 * time.Second))
	followed := uint64(importNow.Add(30 * time.Second).Unix())
	if result.Imported == nil || result.Imported.Kind != client.CreditImportKindFollow || result.Imported.Revision != followed || followed <= imported.Revision {
		t.Fatalf("follow: %+v", result)
	}
	v := s.Current()
	if v.Revision != followed || v.APICredits[fiveOrgA].MonthlyUSD != "230" || !reflect.DeepEqual(Load(s.Path()), v) {
		t.Fatalf("values %+v", v)
	}
	for id, credit := range v.APICredits {
		if credit.Rev != followed {
			t.Fatalf("%s at rev %d", id, credit.Rev)
		}
	}
	for id, renewal := range v.Renewals {
		if renewal.Rev != followed {
			t.Fatalf("renewal %s at rev %d", id, renewal.Rev)
		}
	}
	if r := recordAt(t, e.snapshot); r.Kind != client.CreditImportKindManual || r.SHA256 != hashOf(v14) || r.SettingsSHA256 != hashOf(readFile(t, s.Path())) || r.LegacyRevision != 14 {
		t.Fatalf("record %+v", r)
	}
	if imp := s.Status().Import; imp.State != client.CreditImportStateImported || !imp.Following {
		t.Fatalf("after follow: %+v", imp)
	}
	// A page opened before the import still holds the other row's old rev:
	// its save conflicts.
	var conflict *ConflictError
	if _, err := s.Apply(credit(other, held, "999"), nil, importNow.Add(40*time.Second)); !errors.As(err, &conflict) || !reflect.DeepEqual(conflict.IDs, []string{other}) {
		t.Fatalf("a page holding rev %s: %v", held, err)
	}
	// Follow goes on, raised each time, until a save here.
	place(t, e.sibling, atRevision(t, v14, 15, "240"))
	next := uint64(importNow.Add(50 * time.Second).Unix())
	if result := s.Sync(importNow.Add(50 * time.Second)); result.Imported == nil || result.Imported.Revision != next || s.Current().APICredits[other].Rev != next {
		t.Fatalf("the next follow: %+v", result)
	}
}

// A follow continues the import on record, so it keeps the kind of an
// automatic or manual import, whose note says what may need entering again: a
// later copy of Quota Glance's file brings none of that back, and nothing can
// have been entered here since, or follow would have ended. The log still
// reports a follow, and the record otherwise describes the copy.
func TestAFollowKeepsTheKindOfTheImportItContinues(t *testing.T) {
	legacy := legacyFixture(t, "five-orgs")
	followed := func(t *testing.T, e importEnv, s *Store, raw []byte, at time.Time, kind string) {
		t.Helper()
		place(t, e.sibling, raw)
		if result := s.Sync(at); result.Imported == nil || result.Imported.Kind != client.CreditImportKindFollow {
			t.Fatalf("not followed: %+v", result)
		}
		if r := recordAt(t, e.snapshot); r.Kind != kind || r.SHA256 != hashOf(raw) || !r.ImportedAt.Equal(at) {
			t.Fatalf("record %+v, want kind %s", r, kind)
		}
		if imp := s.Status().Import; imp.State != client.CreditImportStateImported || !imp.Following || *imp.Kind != kind {
			t.Fatalf("import %+v", imp)
		}
	}
	t.Run("automatic", func(t *testing.T) {
		e := newImportEnv(t)
		place(t, e.sibling, legacy)
		s := Open(e.snapshot)
		s.Sync(importNow)
		v13 := atRevision(t, legacy, 13, "300")
		followed(t, e, s, v13, importNow.Add(10*time.Second), client.CreditImportKindAutomatic)
		followed(t, e, s, atRevision(t, v13, 14, "400"), importNow.Add(20*time.Second), client.CreditImportKindAutomatic)
	})
	t.Run("manual", func(t *testing.T) {
		e := newImportEnv(t)
		place(t, e.sibling, legacy)
		s := Open(e.snapshot)
		s.Sync(importNow)
		if _, err := s.Apply(renewalSave("aaaaaaaaaaaaaaaa", "", "2026-12-01"), nil, importNow); err != nil {
			t.Fatal(err)
		}
		v13 := atRevision(t, legacy, 13, "300")
		place(t, e.sibling, v13)
		s.Sync(importNow.Add(10 * time.Second))
		if _, err := s.ImportLegacy(RevisionText(s.Current().Revision), importNow.Add(20*time.Second)); err != nil {
			t.Fatal(err)
		}
		v14 := atRevision(t, v13, 14, "400")
		followed(t, e, s, v14, importNow.Add(30*time.Second), client.CreditImportKindManual)
		followed(t, e, s, atRevision(t, v14, 15, "500"), importNow.Add(40*time.Second), client.CreditImportKindManual)
	})
	// A matched or follow import says nothing needs entering again, and so
	// does the follow after it.
	t.Run("matched", func(t *testing.T) {
		e := newImportEnv(t)
		place(t, e.sibling, legacy)
		s := Open(e.snapshot)
		s.Sync(importNow)
		saved, err := s.Apply(renewalSave("aaaaaaaaaaaaaaaa", "", "2026-12-01"), nil, importNow)
		if err != nil {
			t.Fatal(err)
		}
		ours := readFile(t, s.Path())
		place(t, e.sibling, ours)
		s.Sync(importNow.Add(10 * time.Second))
		next := atRevision(t, ours, saved.Revision+1, "300")
		followed(t, e, s, next, importNow.Add(20*time.Second), client.CreditImportKindFollow)
		followed(t, e, s, atRevision(t, next, saved.Revision+2, "400"), importNow.Add(30*time.Second), client.CreditImportKindFollow)
	})
}

// A crash after the settings file and before the record. Automatic and a
// verbatim follow: the next tick finds the files equal and records them as
// matched, or, for a follow that continued an automatic or manual import, as
// that import, since nothing was saved here in between. Manual: with a record
// of Quota Glance's file, the next tick finds its rows at the settings file's
// revision and records them, as it does a follow that raised them; with none,
// the state never says imported, and importing again is harmless.
func TestCrashBetweenSettingsAndRecord(t *testing.T) {
	legacy := legacyFixture(t, "five-orgs")
	t.Run("automatic", func(t *testing.T) {
		e := newImportEnv(t)
		place(t, e.sibling, legacy)
		s := Open(e.snapshot)
		fail := true
		recordWrites(s, &fail)
		if result := s.Sync(importNow); result.Imported == nil {
			t.Fatalf("result %+v", result)
		}
		if !bytes.Equal(readFile(t, s.Path()), legacy) || exists(s.RecordPath()) {
			t.Fatal("the crash left the wrong files")
		}
		// The process ends; the next one starts from the files.
		s = Open(e.snapshot)
		order := recordWrites(s, nil)
		if result := s.Sync(importNow.Add(10 * time.Second)); result.Imported != nil {
			t.Fatalf("imported again: %+v", result)
		}
		if !reflect.DeepEqual(*order, []string{"snapshot.settings-import.json"}) || recordAt(t, e.snapshot).Kind != client.CreditImportKindMatched {
			t.Fatalf("writes %v", *order)
		}
		if imp := s.Status().Import; imp.State != client.CreditImportStateImported || !imp.Following {
			t.Fatalf("import %+v", imp)
		}
	})
	t.Run("follow", func(t *testing.T) {
		e := newImportEnv(t)
		place(t, e.sibling, legacy)
		s := Open(e.snapshot)
		s.Sync(importNow)
		v13 := atRevision(t, legacy, 13, "300")
		place(t, e.sibling, v13)
		fail := true
		recordWrites(s, &fail)
		s.Sync(importNow.Add(10 * time.Second))
		if !bytes.Equal(readFile(t, s.Path()), v13) || recordAt(t, e.snapshot).Kind != client.CreditImportKindAutomatic {
			t.Fatal("the crash left the wrong files")
		}
		fail = false
		if result := s.Sync(importNow.Add(20 * time.Second)); result.Imported != nil {
			t.Fatalf("copied again: %+v", result)
		}
		if r := recordAt(t, e.snapshot); r.Kind != client.CreditImportKindAutomatic || r.LegacyRevision != 13 || r.SHA256 != hashOf(v13) || r.SettingsSHA256 != hashOf(v13) {
			t.Fatalf("record %+v", r)
		}
		if imp := s.Status().Import; imp.State != client.CreditImportStateImported || !imp.Following || *imp.Kind != client.CreditImportKindAutomatic {
			t.Fatalf("import %+v", imp)
		}
	})
	t.Run("follow after the rollback copy of a manual import", func(t *testing.T) {
		e := newImportEnv(t)
		place(t, e.sibling, legacy)
		s := Open(e.snapshot)
		s.Sync(importNow)
		if _, err := s.Apply(renewalSave("aaaaaaaaaaaaaaaa", "", "2026-12-01"), nil, importNow); err != nil {
			t.Fatal(err)
		}
		place(t, e.sibling, atRevision(t, legacy, 13, "300"))
		s.Sync(importNow.Add(10 * time.Second))
		imported, err := s.ImportLegacy(RevisionText(s.Current().Revision), importNow.Add(20*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		ours := readFile(t, s.Path())
		place(t, e.sibling, ours)
		s.Sync(importNow.Add(30 * time.Second))
		if r := recordAt(t, e.snapshot); r.Kind != client.CreditImportKindManual || r.SHA256 != hashOf(ours) || r.SettingsSHA256 != hashOf(ours) {
			t.Fatalf("the rollback copy: record %+v", r)
		}
		// Quota Glance saves after the copy was recorded, and stamps its row
		// with the time.
		next := savedAt(t, ours, imported.Revision+1, "321", importNow.Add(35*time.Second))
		place(t, e.sibling, next)
		fail := true
		recordWrites(s, &fail)
		s.Sync(importNow.Add(40 * time.Second))
		if !bytes.Equal(readFile(t, s.Path()), next) || recordAt(t, e.snapshot).LegacyRevision != imported.Revision {
			t.Fatal("the crash left the wrong files")
		}
		// The process ends; the next one starts from the files.
		s = Open(e.snapshot)
		if result := s.Sync(importNow.Add(50 * time.Second)); result.Imported != nil {
			t.Fatalf("copied again: %+v", result)
		}
		if r := recordAt(t, e.snapshot); r.Kind != client.CreditImportKindManual || r.LegacyRevision != imported.Revision+1 ||
			r.SHA256 != hashOf(next) || r.SettingsSHA256 != hashOf(next) {
			t.Fatalf("record %+v", r)
		}
		if imp := s.Status().Import; imp.State != client.CreditImportStateImported || !imp.Following || *imp.Kind != client.CreditImportKindManual {
			t.Fatalf("import %+v", imp)
		}
		// Follow goes on as it is, still as the manual import.
		later := atRevision(t, next, imported.Revision+2, "322")
		place(t, e.sibling, later)
		if result := s.Sync(importNow.Add(60 * time.Second)); result.Imported == nil || result.Imported.Kind != client.CreditImportKindFollow {
			t.Fatalf("not followed: %+v", result)
		}
		if !bytes.Equal(readFile(t, s.Path()), later) || recordAt(t, e.snapshot).Kind != client.CreditImportKindManual {
			t.Fatal("follow did not copy Quota Glance's save as it is")
		}
	})
	// After Import Quota Glance's settings, follow raises every row past what
	// a page could hold, so the files never match: the next tick finds Quota
	// Glance's rows at the settings file's revision and records them, as the
	// manual import the follow continued, and follow goes on. A save here
	// first ends it, as it ends follow.
	t.Run("follow that raised the revisions", func(t *testing.T) {
		raisedFollow := func(t *testing.T) (importEnv, *Store, []byte) {
			t.Helper()
			e := newImportEnv(t)
			place(t, e.sibling, legacy)
			s := Open(e.snapshot)
			s.Sync(importNow)
			if _, err := s.Apply(renewalSave("aaaaaaaaaaaaaaaa", "", "2026-12-01"), nil, importNow); err != nil {
				t.Fatal(err)
			}
			v13 := atRevision(t, legacy, 13, "300")
			place(t, e.sibling, v13)
			s.Sync(importNow.Add(10 * time.Second))
			if _, err := s.ImportLegacy(RevisionText(s.Current().Revision), importNow.Add(20*time.Second)); err != nil {
				t.Fatal(err)
			}
			v14 := savedAt(t, v13, 14, "314", importNow.Add(25*time.Second))
			place(t, e.sibling, v14)
			fail := true
			recordWrites(s, &fail)
			if result := s.Sync(importNow.Add(30 * time.Second)); result.Imported == nil || s.Current().APICredits[fiveOrgA].MonthlyUSD != "314" {
				t.Fatalf("follow: %+v", result)
			}
			if r := recordAt(t, e.snapshot); r.Kind != client.CreditImportKindManual || r.LegacyRevision != 13 {
				t.Fatalf("the crash left the wrong record: %+v", r)
			}
			return e, Open(e.snapshot), v14
		}
		e, s, v14 := raisedFollow(t)
		if result := s.Sync(importNow.Add(40 * time.Second)); result.Imported != nil {
			t.Fatalf("copied again: %+v", result)
		}
		if r := recordAt(t, e.snapshot); r.Kind != client.CreditImportKindManual || r.LegacyRevision != 14 || r.SHA256 != hashOf(v14) ||
			r.SettingsSHA256 != hashOf(readFile(t, s.Path())) {
			t.Fatalf("record %+v", r)
		}
		if imp := s.Status().Import; imp.State != client.CreditImportStateImported || !imp.Following || *imp.Kind != client.CreditImportKindManual {
			t.Fatalf("import %+v", imp)
		}
		place(t, e.sibling, atRevision(t, v14, 15, "315"))
		next := uint64(importNow.Add(50 * time.Second).Unix())
		if result := s.Sync(importNow.Add(50 * time.Second)); result.Imported == nil || result.Imported.Revision != next ||
			s.Current().APICredits[fiveOrgA].MonthlyUSD != "315" || recordAt(t, e.snapshot).Kind != client.CreditImportKindManual {
			t.Fatalf("not followed: %+v", result)
		}

		e, s, _ = raisedFollow(t)
		page := RevisionText(s.Current().Renewals[renewalA].Rev)
		if _, err := s.Apply(renewalSave(renewalA, page, "2026-11-20"), nil, importNow.Add(35*time.Second)); err != nil {
			t.Fatal(err)
		}
		s.Sync(importNow.Add(40 * time.Second))
		if r := recordAt(t, e.snapshot); r.LegacyRevision != 13 || importState(s) != client.CreditImportStateChangedSinceImport || s.Status().Import.Following {
			t.Fatalf("saved first: record %+v, import %+v", r, s.Status().Import)
		}
	})
	t.Run("manual with no record", func(t *testing.T) {
		e := newImportEnv(t)
		s := Open(e.snapshot)
		s.Sync(importNow)
		if _, err := s.Apply(renewalSave("aaaaaaaaaaaaaaaa", "", "2026-12-01"), nil, importNow); err != nil {
			t.Fatal(err)
		}
		place(t, e.sibling, legacy)
		s.Sync(importNow.Add(10 * time.Second))
		fail := true
		recordWrites(s, &fail)
		if _, err := s.ImportLegacy(RevisionText(s.Current().Revision), importNow.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		first := s.Current()
		// The process ends; the next one starts from the files.
		s = Open(e.snapshot)
		s.Sync(importNow.Add(2 * time.Minute))
		if state := importState(s); state != client.CreditImportStateNotImported || exists(s.RecordPath()) {
			t.Fatalf("after the crash: %s", state)
		}
		if _, err := s.ImportLegacy(RevisionText(first.Revision), importNow.Add(3*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if state := importState(s); state != client.CreditImportStateImported {
			t.Fatalf("pressed again: %s", state)
		}
		if again := s.Current(); again.Revision != uint64(importNow.Add(3*time.Minute).Unix()) || len(again.APICredits) != len(first.APICredits) ||
			again.APICredits[fiveOrgA].MonthlyUSD != first.APICredits[fiveOrgA].MonthlyUSD || len(again.Renewals) != len(first.Renewals) {
			t.Fatalf("pressed again: %+v", again)
		}
	})
	t.Run("manual with an older record", func(t *testing.T) {
		e := newImportEnv(t)
		place(t, e.sibling, legacy)
		s := Open(e.snapshot)
		s.Sync(importNow)
		if _, err := s.Apply(renewalSave("aaaaaaaaaaaaaaaa", "", "2026-12-01"), nil, importNow); err != nil {
			t.Fatal(err)
		}
		v13 := atRevision(t, legacy, 13, "300")
		place(t, e.sibling, v13)
		s.Sync(importNow.Add(10 * time.Second))
		fail := true
		recordWrites(s, &fail)
		if _, err := s.ImportLegacy(RevisionText(s.Current().Revision), importNow.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if r := recordAt(t, e.snapshot); r.Kind != client.CreditImportKindAutomatic || r.LegacyRevision != 12 {
			t.Fatalf("the crash left the wrong record: %+v", r)
		}
		// The process ends; the next one starts from the files, which hold
		// Quota Glance's rows at the settings file's revision. The record
		// keeps the automatic import's kind, whose note says a value saved
		// here before is not among them.
		s = Open(e.snapshot)
		if state := importState(s); state != client.CreditImportStateChangedSinceImport {
			t.Fatalf("reopened: %s", state)
		}
		if result := s.Sync(importNow.Add(2 * time.Minute)); result.Imported != nil {
			t.Fatalf("imported again: %+v", result)
		}
		written := readFile(t, s.Path())
		if r := recordAt(t, e.snapshot); r.Kind != client.CreditImportKindAutomatic || r.SHA256 != hashOf(v13) || r.SettingsSHA256 != hashOf(written) ||
			r.LegacyRevision != 13 || !r.ImportedAt.Equal(importNow.Add(2*time.Minute)) {
			t.Fatalf("record %+v", r)
		}
		if imp := s.Status().Import; imp.State != client.CreditImportStateImported || !imp.Following || *imp.Kind != client.CreditImportKindAutomatic {
			t.Fatalf("import %+v", imp)
		}
		if _, err := s.ImportLegacy(RevisionText(s.Current().Revision), importNow.Add(3*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if imp := s.Status().Import; imp.State != client.CreditImportStateImported || *imp.Kind != client.CreditImportKindManual || s.Current().APICredits[fiveOrgA].MonthlyUSD != "300" {
			t.Fatalf("pressed again: %+v", imp)
		}
	})
}

// With a completed import, a settings file moved aside leaves the store empty
// and editable, offers the import, and writes nothing. A record that cannot be
// read blocks the automatic import too.
func TestMissingSettingsAfterImportStartsEmpty(t *testing.T) {
	legacy := legacyFixture(t, "five-orgs")
	e := newImportEnv(t)
	place(t, e.sibling, legacy)
	s := Open(e.snapshot)
	s.Sync(importNow)
	if err := os.Rename(s.Path(), s.Path()+".aside"); err != nil {
		t.Fatal(err)
	}
	record := readFile(t, s.RecordPath())
	check := func(name string, s *Store, result SyncResult, order []string) {
		t.Helper()
		v, status := s.Snapshot()
		imp := status.Import
		if v.Unreadable || v.Revision != 0 || len(v.APICredits) != 0 || len(v.Renewals) != 0 || status.State != client.CreditSettingsOK ||
			imp == nil || imp.State != client.CreditImportStateSettingsMissing || !imp.Importable || imp.Following || result.Imported != nil {
			t.Fatalf("%s: values %+v, status %+v, import %+v", name, v, status, imp)
		}
		if len(order) != 0 || exists(s.Path()) || !bytes.Equal(readFile(t, s.RecordPath()), record) {
			t.Fatalf("%s: wrote %v", name, order)
		}
	}
	// The same store, at its next tick.
	order := recordWrites(s, nil)
	result := s.Sync(importNow.Add(10 * time.Second))
	if !result.Reloaded {
		t.Fatal("the missing file was not noticed")
	}
	check("next tick", s, result, *order)
	// A configure that opens the store again.
	again := Open(e.snapshot)
	order = recordWrites(again, nil)
	check("reopened", again, again.Sync(importNow.Add(20*time.Second)), *order)
	// It is editable, and the file it writes starts past the one moved aside,
	// and past where the import's lineage starts, the time of the import.
	if result, err := again.Apply(renewalSave(renewalA, "", "2026-11-01"), nil, importNow); err != nil || result.Revision != uint64(importNow.Unix())+1 {
		t.Fatalf("the first save: %+v, %v", result, err)
	}

	// A record that cannot be read, and no settings file.
	e = newImportEnv(t)
	place(t, e.sibling, legacy)
	if err := os.WriteFile(RecordPath(e.snapshot), []byte(`{"schema":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s = Open(e.snapshot)
	order = recordWrites(s, nil)
	result = s.Sync(importNow)
	if imp := s.Status().Import; result.Imported != nil || len(*order) != 0 || imp == nil || imp.State != client.CreditImportStateSettingsMissing ||
		imp.Kind != nil || imp.Following || s.Status().State != client.CreditSettingsOK {
		t.Fatalf("unreadable record: %+v, wrote %v, import %+v", result, *order, imp)
	}

	// After an import, a Quota Glance file that cannot be read is only a
	// warning, with the settings file moved aside too: the store stays
	// editable, at its next tick and opened again.
	e = newImportEnv(t)
	place(t, e.sibling, legacy)
	s = Open(e.snapshot)
	s.Sync(importNow)
	if err := os.Rename(s.Path(), s.Path()+".aside"); err != nil {
		t.Fatal(err)
	}
	place(t, e.sibling, []byte("{"))
	editable := func(name string, s *Store, result SyncResult) {
		t.Helper()
		if status := s.Status(); result.LegacyUnreadable || status.State != client.CreditSettingsOK || importState(s) != client.CreditImportStateLegacyUnreadable {
			t.Fatalf("%s: %+v, status %+v, import %+v", name, result, status, status.Import)
		}
	}
	editable("next tick", s, s.Sync(importNow.Add(10*time.Second)))
	s = Open(e.snapshot)
	editable("reopened", s, s.Sync(importNow.Add(20*time.Second)))
	if _, err := s.Apply(renewalSave(renewalA, "", "2026-11-01"), nil, importNow.Add(30*time.Second)); err != nil {
		t.Fatalf("a save: %v", err)
	}
}

// A save into a store whose settings file went missing after an import starts
// a file that holds none of the imported values. It adds that file's revision
// to the record's lineage first, and the record then counts as none: the
// import is offered as notImported, through a reopen and later saves, and is
// null while there is no Quota Glance file. The import's own settings file
// put back reads as imported again, and an import starts the lineage afresh.
func TestASaveAfterTheSettingsFileWentMissingIsNotImported(t *testing.T) {
	const saved = "aaaaaaaaaaaaaaaa"
	legacy := legacyFixture(t, "five-orgs")
	e := newImportEnv(t)
	place(t, e.sibling, legacy)
	s := Open(e.snapshot)
	s.Sync(importNow)
	if err := os.Rename(s.Path(), s.Path()+".aside"); err != nil {
		t.Fatal(err)
	}
	s.Sync(importNow.Add(10 * time.Second))
	if state := importState(s); state != client.CreditImportStateSettingsMissing {
		t.Fatalf("moved aside: %s", state)
	}
	notImported := func(name string, s *Store) {
		t.Helper()
		v, status := s.Snapshot()
		imp := status.Import
		if imp == nil || imp.State != client.CreditImportStateNotImported || !imp.Importable || imp.Following || imp.Kind != nil || imp.SHA256 != nil ||
			len(v.APICredits) != 0 || v.Renewals[saved].Date == "" {
			t.Fatalf("%s: values %+v, import %+v", name, v, imp)
		}
	}

	// A record that cannot be written refuses the save, and writes nothing.
	failRecord := true
	order := recordWrites(s, &failRecord)
	if _, err := s.Apply(renewalSave(saved, "", "2026-11-01"), nil, importNow.Add(20*time.Second)); !errors.Is(err, ErrUnwritable) ||
		exists(s.Path()) || len(recordAt(t, e.snapshot).Lineage) != 1 || len(*order) != 0 {
		t.Fatalf("record unwritable: %v, wrote %v", err, *order)
	}
	failRecord = false
	first, err := s.Apply(renewalSave(saved, "", "2026-11-01"), nil, importNow.Add(20*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"snapshot.settings-import.json", "snapshot.settings.json"}; !reflect.DeepEqual(*order, want) {
		t.Fatalf("wrote %v, want the record first", *order)
	}
	if r := recordAt(t, e.snapshot); len(r.Lineage) != 2 || r.Lineage[1] != first.Revision || r.Kind != client.CreditImportKindAutomatic {
		t.Fatalf("record %+v", r)
	}
	notImported("saved", s)
	s.Sync(importNow.Add(30 * time.Second))
	notImported("next tick", s)
	again := Open(e.snapshot)
	again.Sync(importNow.Add(40 * time.Second))
	notImported("reopened", again)
	*order = nil
	if _, err := s.Apply(renewalSave(saved, RevisionText(first.Revision), "2026-11-02"), nil, importNow.Add(50*time.Second)); err != nil || !reflect.DeepEqual(*order, []string{"snapshot.settings.json"}) {
		t.Fatalf("a later save: %v, wrote %v", err, *order)
	}
	notImported("a later save", s)

	// With no Quota Glance file there is nothing to import, and nothing to
	// report as imported.
	if err := os.Rename(e.sibling, e.sibling+".aside"); err != nil {
		t.Fatal(err)
	}
	s.Sync(importNow.Add(60 * time.Second))
	if imp := s.Status().Import; imp != nil {
		t.Fatalf("no legacy file: %+v", imp)
	}
	if err := os.Rename(e.sibling+".aside", e.sibling); err != nil {
		t.Fatal(err)
	}

	// The import's own file put back: imported, and followed, since nothing
	// was saved over it.
	if err := os.Rename(s.Path()+".aside", s.Path()); err != nil {
		t.Fatal(err)
	}
	s.Sync(importNow.Add(70 * time.Second))
	if imp := s.Status().Import; imp == nil || imp.State != client.CreditImportStateImported || !imp.Following {
		t.Fatalf("put back: %+v", imp)
	}

	// Moved aside and saved over again, which joins the span the first file
	// started, then imported: the import starts the lineage afresh.
	if err := os.Remove(s.Path()); err != nil {
		t.Fatal(err)
	}
	s.Sync(importNow.Add(80 * time.Second))
	second, err := s.Apply(renewalSave(saved, "", "2026-11-03"), nil, importNow.Add(90*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	notImported("saved over again", s)
	if r := recordAt(t, e.snapshot); len(r.Lineage) != 2 {
		t.Fatalf("a second start wrote the record: %+v", r)
	}
	if _, err := s.ImportLegacy(RevisionText(second.Revision), importNow.Add(100*time.Second)); err != nil {
		t.Fatal(err)
	}
	if r := recordAt(t, e.snapshot); len(r.Lineage) != 1 || r.Lineage[0] <= second.Revision+1 || r.Kind != client.CreditImportKindManual {
		t.Fatalf("record %+v", r)
	}
	if v, imp := s.Current(), s.Status().Import; imp.State != client.CreditImportStateImported || len(v.APICredits) != 5 || v.Renewals[saved].Date != "" {
		t.Fatalf("imported: %+v, %+v", v, imp)
	}
}

// A good copy put back over a settings file a save here started is the
// import's again, through every later save. The store has seen the started
// file's revisions, so a save over the copy writes past them, into the span
// that holds none of the import, and would read as started here and offer
// the import again, whose values it still holds. So the save adds a switch
// back to the record's lineage first, and is refused when it cannot. A tick
// writes nothing: whatever is put back reads by its revision.
func TestAGoodCopyPutBackStaysImportedThroughLaterSaves(t *testing.T) {
	const saved = "aaaaaaaaaaaaaaaa"
	legacy := legacyFixture(t, "five-orgs")
	e := newImportEnv(t)
	place(t, e.sibling, legacy)
	s := Open(e.snapshot)
	s.Sync(importNow)
	// A date entered here after the import, then a copy kept of the file.
	edited, err := s.Apply(renewalSave(saved, "", "2026-11-01"), nil, importNow.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	backup := readFile(t, s.Path())
	imported := func(name string, s *Store, date string) {
		t.Helper()
		v, status := s.Snapshot()
		imp := status.Import
		if imp == nil || imp.State != client.CreditImportStateImported || imp.Kind == nil || *imp.Kind != client.CreditImportKindAutomatic ||
			len(v.APICredits) != 5 || v.Renewals[saved].Date != date {
			t.Fatalf("%s: values %+v, import %+v", name, v, imp)
		}
	}
	// The file goes missing, and a date entered again starts a new one: the
	// record's lineage gains its revision, and the import is offered.
	startOver := func(at time.Time) uint64 {
		t.Helper()
		if err := os.Remove(s.Path()); err != nil {
			t.Fatal(err)
		}
		s.Sync(at)
		started, err := s.Apply(renewalSave(saved, "", "2026-12-01"), nil, at.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if r := recordAt(t, e.snapshot); r.Lineage[len(r.Lineage)-1] != started.Revision || importState(s) != client.CreditImportStateNotImported {
			t.Fatalf("started over: record %+v, state %s", r, importState(s))
		}
		return started.Revision
	}
	first := startOver(importNow.Add(20 * time.Second))

	// The copy put back is below the started file, so it reads as imported
	// at once. A save over it writes the switch back first, and is refused
	// when the record cannot be written, with nothing written.
	place(t, s.Path(), backup)
	failRecord := true
	order := recordWrites(s, &failRecord)
	over := renewalSave(saved, RevisionText(edited.Revision), "2026-11-02")
	if _, err := s.Apply(over, nil, importNow.Add(30*time.Second)); !errors.Is(err, ErrUnwritable) || len(*order) != 0 ||
		!bytes.Equal(readFile(t, s.Path()), backup) || len(recordAt(t, e.snapshot).Lineage) != 2 {
		t.Fatalf("record unwritable: %v, wrote %v", err, *order)
	}
	imported("put back", s, "2026-11-01")
	failRecord = false
	later, err := s.Apply(over, nil, importNow.Add(40*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"snapshot.settings-import.json", "snapshot.settings.json"}; !reflect.DeepEqual(*order, want) || later.Revision <= first {
		t.Fatalf("saved at %d, not past the started file's %d, wrote %v, want the record first", later.Revision, first, *order)
	}
	if r := recordAt(t, e.snapshot); !reflect.DeepEqual(r.Lineage[1:], []uint64{first, later.Revision}) || r.Kind != client.CreditImportKindAutomatic || r.SHA256 != hashOf(legacy) {
		t.Fatalf("record %+v", r)
	}
	imported("saved over", s, "2026-11-02")
	s.Sync(importNow.Add(50 * time.Second))
	imported("next tick", s, "2026-11-02")
	again := Open(e.snapshot)
	again.Sync(importNow.Add(60 * time.Second))
	imported("reopened", again, "2026-11-02")
	*order = nil
	if _, err := s.Apply(renewalSave(saved, RevisionText(later.Revision), "2026-11-03"), nil, importNow.Add(70*time.Second)); err != nil ||
		!reflect.DeepEqual(*order, []string{"snapshot.settings.json"}) {
		t.Fatalf("a later save: %v, wrote %v", err, *order)
	}
	imported("a later save", s, "2026-11-03")

	// Put back with a tick before the next save: the tick writes nothing.
	startOver(importNow.Add(80 * time.Second))
	place(t, s.Path(), backup)
	*order = nil
	s.Sync(importNow.Add(90 * time.Second))
	if r := recordAt(t, e.snapshot); len(r.Lineage) != 4 || len(*order) != 0 {
		t.Fatalf("at the tick: record %+v, wrote %v", r, *order)
	}
	imported("put back again", s, "2026-11-01")
	if _, err := s.Apply(over, nil, importNow.Add(100*time.Second)); err != nil {
		t.Fatal(err)
	}
	imported("saved over again", s, "2026-11-02")
}

// notImportedHolding checks that s offers the import as notImported, with no
// record fields and none of the imported values, and holds date for renewalA.
func notImportedHolding(t *testing.T, name string, s *Store, date string) {
	t.Helper()
	v, status := s.Snapshot()
	imp := status.Import
	if imp == nil || imp.State != client.CreditImportStateNotImported || !imp.Importable || imp.Following || imp.Kind != nil ||
		len(v.APICredits) != 0 || v.Renewals[renewalA].Date != date {
		t.Fatalf("%s: values %+v, import %+v", name, v, imp)
	}
}

// importedHolding checks that s reads as imported, with the five-orgs
// fixture's credits, and holds date for renewalA.
func importedHolding(t *testing.T, name string, s *Store, date string) {
	t.Helper()
	v, status := s.Snapshot()
	imp := status.Import
	if imp == nil || imp.State != client.CreditImportStateImported || imp.Kind == nil || len(v.APICredits) != 5 || v.Renewals[renewalA].Date != date {
		t.Fatalf("%s: values %+v, import %+v", name, v, imp)
	}
}

// A settings file from before the import, put back, holds none of what was
// imported: it reads as notImported and offers the import, through later
// saves and a reopen, whether the import was the automatic one, which ran
// once the file was moved aside, or one pressed on the page over it.
func TestASettingsFileFromBeforeTheImportIsNotImported(t *testing.T) {
	legacy := legacyFixture(t, "five-orgs")
	t.Run("automatic", func(t *testing.T) {
		e := newImportEnv(t)
		s := Open(e.snapshot)
		s.Sync(importNow)
		// A date saved here before Quota Glance's file was found, and a copy
		// kept of the file.
		first, err := s.Apply(renewalSave(renewalA, "", "2026-11-01"), nil, importNow)
		if err != nil {
			t.Fatal(err)
		}
		before := readFile(t, s.Path())
		place(t, e.sibling, legacy)
		s.Sync(importNow.Add(10 * time.Second))
		notImportedHolding(t, "found", s, "2026-11-01")
		// The file fails a rule and is moved aside: a first start, which
		// imports Quota Glance's file.
		if err := os.WriteFile(s.Path(), []byte(`{"schema":1,"revision":"x"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		s.Sync(importNow.Add(20 * time.Second))
		if err := os.Rename(s.Path(), s.Path()+".aside"); err != nil {
			t.Fatal(err)
		}
		if result := s.Sync(importNow.Add(30 * time.Second)); result.Imported == nil {
			t.Fatalf("moved aside: %+v", result)
		}
		importedHolding(t, "imported", s, "2026-10-29")
		// The good copy put back, saved over, and reopened.
		place(t, s.Path(), before)
		s.Sync(importNow.Add(40 * time.Second))
		notImportedHolding(t, "put back", s, "2026-11-01")
		if _, err := s.Apply(renewalSave(renewalA, RevisionText(first.Revision), "2026-11-02"), nil, importNow.Add(50*time.Second)); err != nil {
			t.Fatal(err)
		}
		notImportedHolding(t, "saved over", s, "2026-11-02")
		again := Open(e.snapshot)
		again.Sync(importNow.Add(60 * time.Second))
		notImportedHolding(t, "reopened", again, "2026-11-02")
		// The import's own file is still the import's.
		place(t, s.Path(), legacy)
		s.Sync(importNow.Add(70 * time.Second))
		importedHolding(t, "the import's file back", s, "2026-10-29")
	})
	t.Run("manual", func(t *testing.T) {
		e := newImportEnv(t)
		s := Open(e.snapshot)
		s.Sync(importNow)
		first, err := s.Apply(renewalSave(renewalA, "", "2026-11-01"), nil, importNow)
		if err != nil {
			t.Fatal(err)
		}
		before := readFile(t, s.Path())
		place(t, e.sibling, legacy)
		s.Sync(importNow.Add(10 * time.Second))
		if _, err := s.ImportLegacy(RevisionText(first.Revision), importNow.Add(20*time.Second)); err != nil {
			t.Fatal(err)
		}
		importedHolding(t, "imported", s, "2026-10-29")
		place(t, s.Path(), before)
		s.Sync(importNow.Add(30 * time.Second))
		notImportedHolding(t, "put back", s, "2026-11-01")
	})
}

// A settings file a save here started after the imported one went missing
// holds none of what was imported, and goes on reading so whatever was put
// back in between: the import's own file, a copy saved over since, or a file
// another start began. Each file that holds the import goes on reading as
// imported too.
func TestAFileStartedHereStaysNotImportedWhateverIsPutBack(t *testing.T) {
	e := newImportEnv(t)
	place(t, e.sibling, legacyFixture(t, "five-orgs"))
	s := Open(e.snapshot)
	tick := importNow
	s.Sync(tick)
	next := func() time.Time {
		tick = tick.Add(10 * time.Second)
		return tick
	}
	putBack := func(raw []byte) {
		t.Helper()
		place(t, s.Path(), raw)
		s.Sync(next())
	}
	// The file goes missing, and a date entered here starts a new one.
	start := func(date string) []byte {
		t.Helper()
		if err := os.Remove(s.Path()); err != nil {
			t.Fatal(err)
		}
		s.Sync(next())
		if _, err := s.Apply(renewalSave(renewalA, "", date), nil, next()); err != nil {
			t.Fatal(err)
		}
		return readFile(t, s.Path())
	}
	imported := readFile(t, s.Path())
	started := start("2026-12-01")
	notImportedHolding(t, "started", s, "2026-12-01")
	putBack(imported)
	importedHolding(t, "the import's file back", s, "2026-10-29")
	putBack(started)
	notImportedHolding(t, "the started file back", s, "2026-12-01")
	again := Open(e.snapshot)
	again.Sync(next())
	notImportedHolding(t, "reopened", again, "2026-12-01")

	// The import's file back and saved over, then another start.
	putBack(imported)
	if _, err := s.Apply(renewalSave(renewalA, "2", "2026-11-15"), nil, next()); err != nil {
		t.Fatal(err)
	}
	savedOver := readFile(t, s.Path())
	importedHolding(t, "saved over", s, "2026-11-15")
	second := start("2026-12-02")
	notImportedHolding(t, "started again", s, "2026-12-02")
	for _, back := range []struct {
		name     string
		raw      []byte
		imported bool
		date     string
	}{
		{"the first started file", started, false, "2026-12-01"},
		{"the copy saved over", savedOver, true, "2026-11-15"},
		{"the second started file", second, false, "2026-12-02"},
		{"the import's file", imported, true, "2026-10-29"},
		{"the first started file again", started, false, "2026-12-01"},
	} {
		putBack(back.raw)
		if back.imported {
			importedHolding(t, back.name+" back", s, back.date)
		} else {
			notImportedHolding(t, back.name+" back", s, back.date)
		}
	}
}

// Every file a save starts after the imported one went missing reads as not
// imported, however the starts fall. A second start, after the first file
// went missing too, leaves the first reading so once it is put back. A start
// whose clock is behind an earlier one, as after a settings commit that
// failed, still lands where files are not imported.
func TestEveryStartAfterTheImportIsNotImported(t *testing.T) {
	legacy := legacyFixture(t, "five-orgs")
	startAt := func(t *testing.T, s *Store, at time.Time, date string) error {
		t.Helper()
		if exists(s.Path()) {
			if err := os.Remove(s.Path()); err != nil {
				t.Fatal(err)
			}
		}
		s.Sync(at)
		_, err := s.Apply(renewalSave(renewalA, "", date), nil, at)
		return err
	}
	t.Run("a second start", func(t *testing.T) {
		e := newImportEnv(t)
		place(t, e.sibling, legacy)
		s := Open(e.snapshot)
		s.Sync(importNow)
		if err := startAt(t, s, importNow.Add(time.Hour), "2026-12-01"); err != nil {
			t.Fatal(err)
		}
		first := readFile(t, s.Path())
		if err := startAt(t, s, importNow.Add(2*time.Hour), "2026-12-02"); err != nil {
			t.Fatal(err)
		}
		notImportedHolding(t, "the second start", s, "2026-12-02")
		place(t, s.Path(), first)
		s.Sync(importNow.Add(3 * time.Hour))
		notImportedHolding(t, "the first file back", s, "2026-12-01")
		place(t, s.Path(), legacy)
		s.Sync(importNow.Add(4 * time.Hour))
		importedHolding(t, "the import's file back", s, "2026-10-29")
	})
	t.Run("a clock set back", func(t *testing.T) {
		e := newImportEnv(t)
		place(t, e.sibling, legacy)
		s := Open(e.snapshot)
		s.Sync(importNow)
		failSettings := true
		s.write = func(path string, raw []byte) error {
			if failSettings && path == s.Path() {
				return errors.New("injected: the settings write fails")
			}
			return writeFile(path, raw)
		}
		if err := startAt(t, s, importNow.Add(time.Hour), "2026-12-01"); !errors.Is(err, ErrUnwritable) || exists(s.Path()) {
			t.Fatalf("the failed start: %v", err)
		}
		failSettings = false
		if err := startAt(t, s, importNow.Add(10*time.Minute), "2026-12-02"); err != nil {
			t.Fatal(err)
		}
		notImportedHolding(t, "started behind", s, "2026-12-02")
		s.Sync(importNow.Add(11 * time.Minute))
		notImportedHolding(t, "next tick", s, "2026-12-02")
		started := readFile(t, s.Path())
		place(t, s.Path(), legacy)
		s.Sync(importNow.Add(12 * time.Minute))
		importedHolding(t, "the import's file back", s, "2026-10-29")
		place(t, s.Path(), started)
		s.Sync(importNow.Add(13 * time.Minute))
		notImportedHolding(t, "the started file back", s, "2026-12-02")
	})
}

// The record keeps at most maxLineage switches. A save that would add one past
// that drops the oldest pair, so the first switch left is still one from
// which files hold the import, and the newest files read as they did: the
// newest saved over a file that holds the import as imported, the newest
// started after the imported file went missing as not.
func TestTheLineageDropsItsOldestPairPastItsBound(t *testing.T) {
	e := newImportEnv(t)
	place(t, e.sibling, legacyFixture(t, "five-orgs"))
	s := Open(e.snapshot)
	tick := importNow
	s.Sync(tick)
	next := func() time.Time {
		tick = tick.Add(10 * time.Second)
		return tick
	}
	imported := readFile(t, s.Path())
	switches := recordAt(t, e.snapshot).Lineage
	save := func(date string) uint64 {
		t.Helper()
		saved, err := s.Apply(renewalSave(renewalA, RevisionText(s.Current().Renewals[renewalA].Rev), date), nil, next())
		if err != nil {
			t.Fatalf("saving %s: %v", date, err)
		}
		return saved.Revision
	}
	check := func(name, state string) {
		t.Helper()
		want := switches
		for len(want) > maxLineage {
			want = want[2:]
		}
		if r := recordAt(t, e.snapshot); !reflect.DeepEqual(r.Lineage, want) || len(r.Lineage) > maxLineage || importState(s) != state {
			t.Fatalf("%s: lineage %v, want %v; state %s, want %s", name, r.Lineage, want, importState(s), state)
		}
	}
	var held, started []byte
	for i := range maxLineage {
		// The file goes missing, and a save starts another.
		if err := os.Remove(s.Path()); err != nil {
			t.Fatal(err)
		}
		s.Sync(next())
		date := fmt.Sprintf("2026-11-%02d", i+1)
		switches = append(switches, save(date))
		started = readFile(t, s.Path())
		check("started "+date, client.CreditImportStateNotImported)
		// The imported file put back, and saved over.
		place(t, s.Path(), imported)
		s.Sync(next())
		date = fmt.Sprintf("2026-12-%02d", i+1)
		switches = append(switches, save(date))
		held = readFile(t, s.Path())
		check("saved over "+date, client.CreditImportStateImported)
	}
	if len(switches) <= maxLineage+2 {
		t.Fatalf("only %d switches: the bound was not passed twice", len(switches))
	}
	place(t, s.Path(), started)
	s.Sync(next())
	notImportedHolding(t, "the newest started file back", s, fmt.Sprintf("2026-11-%02d", maxLineage))
	place(t, s.Path(), held)
	s.Sync(next())
	importedHolding(t, "the newest file saved over back", s, fmt.Sprintf("2026-12-%02d", maxLineage))
	again := Open(e.snapshot)
	again.Sync(next())
	importedHolding(t, "reopened", again, fmt.Sprintf("2026-12-%02d", maxLineage))
}

// An import starts the lineage past every revision the store has seen, not at
// the time alone. While revisions are ahead of the clock, as in files saved
// on a host whose clock ran ahead, a copy from before the import whose
// revision lies between the time and the highest seen holds none of what was
// imported, and reads as not imported once put back.
func TestALineageStartsPastEveryRevisionSeen(t *testing.T) {
	legacy := legacyFixture(t, "five-orgs")
	clock := uint64(importNow.Unix())
	// ahead returns a store that has seen a settings file at clock+1000, and
	// a copy kept at clock+100, both from before Quota Glance's file is
	// placed.
	ahead := func(t *testing.T) (importEnv, *Store, []byte) {
		t.Helper()
		e := newImportEnv(t)
		s := Open(e.snapshot)
		s.Sync(importNow)
		early := atRevision(t, legacy, clock+100, "111")
		place(t, s.Path(), early)
		s.Sync(importNow)
		place(t, s.Path(), atRevision(t, legacy, clock+1000, "222"))
		s.Sync(importNow)
		if v := s.Current(); v.Revision != clock+1000 || exists(s.RecordPath()) {
			t.Fatalf("ahead: revision %d", v.Revision)
		}
		return e, s, early
	}
	notImported := func(t *testing.T, name string, s *Store, revision uint64, monthly string) {
		t.Helper()
		if v := s.Current(); importState(s) != client.CreditImportStateNotImported || v.Revision != revision || v.APICredits[fiveOrgA].MonthlyUSD != monthly {
			t.Fatalf("%s: state %s, revision %d, credit %+v", name, importState(s), v.Revision, v.APICredits[fiveOrgA])
		}
	}
	t.Run("manual", func(t *testing.T) {
		e, s, early := ahead(t)
		place(t, e.sibling, legacy)
		s.Sync(importNow)
		result, err := s.ImportLegacy(RevisionText(clock+1000), importNow)
		if err != nil {
			t.Fatal(err)
		}
		if r := recordAt(t, e.snapshot); r.Kind != client.CreditImportKindManual || len(r.Lineage) != 1 || r.Lineage[0] <= result.Revision {
			t.Fatalf("imported at %d: record %+v", result.Revision, r)
		}
		ours := readFile(t, s.Path())
		place(t, s.Path(), early)
		s.Sync(importNow.Add(10 * time.Second))
		notImported(t, "the copy back", s, clock+100, "111")
		place(t, s.Path(), atRevision(t, legacy, clock+1000, "222"))
		s.Sync(importNow.Add(20 * time.Second))
		notImported(t, "the newest file from before back", s, clock+1000, "222")
		place(t, s.Path(), ours)
		s.Sync(importNow.Add(30 * time.Second))
		importedHolding(t, "the import's file back", s, "2026-10-29")
		if _, err := s.Apply(renewalSave(renewalA, RevisionText(result.Revision), "2026-11-01"), nil, importNow.Add(40*time.Second)); err != nil {
			t.Fatal(err)
		}
		importedHolding(t, "saved over", s, "2026-11-01")
	})
	// The automatic import, once the file ahead of the clock went missing
	// before any record was written.
	t.Run("automatic", func(t *testing.T) {
		e, s, early := ahead(t)
		if err := os.Remove(s.Path()); err != nil {
			t.Fatal(err)
		}
		place(t, e.sibling, legacy)
		if result := s.Sync(importNow); result.Imported == nil || result.Imported.Kind != client.CreditImportKindAutomatic {
			t.Fatalf("not imported: %+v", result)
		}
		if r := recordAt(t, e.snapshot); len(r.Lineage) != 1 || r.Lineage[0] <= clock+1000 {
			t.Fatalf("record %+v", r)
		}
		place(t, s.Path(), early)
		s.Sync(importNow.Add(10 * time.Second))
		notImported(t, "the copy back", s, clock+100, "111")
		place(t, s.Path(), legacy)
		s.Sync(importNow.Add(20 * time.Second))
		importedHolding(t, "the import's file back", s, "2026-10-29")
	})
}

// Follow copies Quota Glance's next save verbatim only while no revision past
// the imported file's was given out. Once a save here gave one out and the
// imported file was put back, a page may still hold that revision for a row,
// and Quota Glance's next save can reach it: copied verbatim, the page's save
// would overwrite Quota Glance's value without a conflict. Followed past
// every revision given out instead, the page's save conflicts.
func TestFollowAfterTheImportedFileIsPutBackKeepsRevisionsAhead(t *testing.T) {
	legacy := legacyFixture(t, "five-orgs")
	credit := func(base, monthly string) Batch {
		return Batch{Kind: KindAPICredits, APICredits: []APICreditItem{{ID: fiveOrgA, BaseRevision: base, MonthlyUSD: ptr(monthly)}}}
	}
	// ours is the imported file; a card saves fiveOrgA over it at saveAt and
	// stays open; ours is put back; Quota Glance saves fiveOrgA at revision
	// glance. The card's save then meets Quota Glance's value as a conflict.
	check := func(t *testing.T, e importEnv, s *Store, ours []byte, saveAt time.Time, glance uint64) {
		t.Helper()
		saved, err := s.Apply(credit(RevisionText(s.Current().APICredits[fiveOrgA].Rev), "210"), nil, saveAt)
		if err != nil {
			t.Fatal(err)
		}
		held := RevisionText(saved.Revision)
		place(t, s.Path(), ours)
		s.Sync(saveAt.Add(10 * time.Second))
		if imp := s.Status().Import; imp.State != client.CreditImportStateImported || !imp.Following {
			t.Fatalf("put back: %+v", imp)
		}
		place(t, e.sibling, atRevision(t, ours, glance, "999"))
		if result := s.Sync(saveAt.Add(20 * time.Second)); result.Imported == nil || result.Imported.Kind != client.CreditImportKindFollow {
			t.Fatalf("not followed: %+v", result)
		}
		var conflict *ConflictError
		if _, err := s.Apply(credit(held, "220"), nil, saveAt.Add(30*time.Second)); !errors.As(err, &conflict) || conflict.APICredits[fiveOrgA].MonthlyUSD != "999" {
			t.Fatalf("the card holding rev %s: %v, want a conflict", held, err)
		}
		if got := Load(s.Path()).APICredits[fiveOrgA]; got.MonthlyUSD != "999" {
			t.Fatalf("stored %+v", got)
		}
		if imp := s.Status().Import; imp.State != client.CreditImportStateImported || !imp.Following {
			t.Fatalf("followed: %+v", imp)
		}
	}
	t.Run("automatic", func(t *testing.T) {
		e := newImportEnv(t)
		place(t, e.sibling, legacy)
		s := Open(e.snapshot)
		s.Sync(importNow)
		check(t, e, s, legacy, importNow.Add(10*time.Second), 13)
	})
	// A file started here at the time, copied over Quota Glance's in the
	// same second and matched: the save over it gives out the revision one
	// past it, which is Quota Glance's next.
	t.Run("matched", func(t *testing.T) {
		e := newImportEnv(t)
		s := Open(e.snapshot)
		s.Sync(importNow)
		first, err := s.Apply(credit("", "200"), nil, importNow)
		if err != nil {
			t.Fatal(err)
		}
		ours := readFile(t, s.Path())
		place(t, e.sibling, ours)
		s.Sync(importNow)
		if r := recordAt(t, e.snapshot); r.Kind != client.CreditImportKindMatched {
			t.Fatalf("record %+v", r)
		}
		check(t, e, s, ours, importNow, first.Revision+1)
	})
}

// withRenewal is the settings file raw at revision, with the renewal id set to
// date by that revision, as a save in Quota Glance writes it.
func withRenewal(t *testing.T, raw []byte, revision uint64, id, date string) []byte {
	t.Helper()
	v, err := decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	next := v.clone()
	next.Revision = revision
	next.Renewals[id] = Renewal{Date: date, Rev: revision, UpdatedAt: importNow}
	out, err := encode(next)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Follow copies Quota Glance's save verbatim only when it goes on from the
// file copied. Quota Glance numbers each save one past its file's revision, so
// once an older copy of its file is put back, or another file is placed as
// the drop-in, a newer file can hold a revision the copy gave a row, for
// another value. Copied verbatim, a card holding that revision would save
// over Quota Glance's value without a conflict; raised past it, as after a
// manual import, the card's save conflicts.
func TestFollowRaisesAFileThatDoesNotGoOnFromTheCopy(t *testing.T) {
	legacy := legacyFixture(t, "five-orgs")
	// Renewals at revisions 2 and 9 in the fixture.
	const a, b = "0123456789abcdef", "fedcba9876543210"
	// raisedPast checks a follow at at that raised every row, with b's value
	// from Quota Glance's file, then that a card holding held for b meets that
	// value as a conflict.
	raisedPast := func(t *testing.T, e importEnv, s *Store, result SyncResult, at time.Time, held, date string) {
		t.Helper()
		followed := uint64(at.Unix())
		if result.Imported == nil || result.Imported.Kind != client.CreditImportKindFollow || result.Imported.Revision != followed {
			t.Fatalf("follow: %+v", result)
		}
		for id, renewal := range s.Current().Renewals {
			if renewal.Rev != followed {
				t.Fatalf("renewal %s at rev %d, want %d", id, renewal.Rev, followed)
			}
		}
		if got := s.Current().Renewals[b]; got.Date != date {
			t.Fatalf("b holds %+v, want %s", got, date)
		}
		if r := recordAt(t, e.snapshot); r.SHA256 == r.SettingsSHA256 || r.SettingsSHA256 != hashOf(readFile(t, s.Path())) {
			t.Fatalf("record %+v", r)
		}
		if imp := s.Status().Import; imp.State != client.CreditImportStateImported {
			t.Fatalf("import %+v", imp)
		}
		var conflict *ConflictError
		if _, err := s.Apply(renewalSave(b, held, "2026-11-25"), nil, at.Add(10*time.Second)); !errors.As(err, &conflict) || conflict.Renewals[b].Date != date {
			t.Fatalf("the card holding rev %s: %v, want a conflict", held, err)
		}
		if got := Load(s.Path()).Renewals[b]; got.Date != date {
			t.Fatalf("stored %+v", got)
		}
	}
	// Quota Glance's file at revision 12 is imported, and its save at 13
	// followed; a card opens on b. The file at 12 is put back, and Quota
	// Glance saves b at 13 again, for another date, then a at 14.
	t.Run("an older copy put back", func(t *testing.T) {
		e := newImportEnv(t)
		place(t, e.sibling, legacy)
		s := Open(e.snapshot)
		s.Sync(importNow)
		v13 := withRenewal(t, legacy, 13, b, "2026-11-20")
		place(t, e.sibling, v13)
		if result := s.Sync(importNow.Add(10 * time.Second)); result.Imported == nil || !bytes.Equal(readFile(t, s.Path()), v13) {
			t.Fatalf("not followed as it is: %+v", result)
		}
		held := RevisionText(s.Current().Renewals[b].Rev)
		if held != "13" {
			t.Fatalf("b at rev %s", held)
		}
		place(t, e.sibling, legacy)
		s.Sync(importNow.Add(20 * time.Second))
		reused := withRenewal(t, legacy, 13, b, "2026-11-15")
		place(t, e.sibling, reused)
		if result := s.Sync(importNow.Add(30 * time.Second)); result.Imported != nil || importState(s) != client.CreditImportStateChangedSinceImport {
			t.Fatalf("a file not past the record's revision was followed: %+v", result)
		}
		place(t, e.sibling, withRenewal(t, reused, 14, a, "2026-12-01"))
		at := importNow.Add(40 * time.Second)
		raisedPast(t, e, s, s.Sync(at), at, held, "2026-11-15")
	})
	// The drop-in at revision 12 is imported, and a card opens on b, at rev
	// 9. Another Quota Glance's file is placed as the drop-in, at 13, where b
	// is at rev 9 too, with another date.
	t.Run("another file placed as the drop-in", func(t *testing.T) {
		e := newImportEnv(t)
		place(t, e.dropIn, legacy)
		s := Open(e.snapshot)
		s.Sync(importNow)
		held := RevisionText(s.Current().Renewals[b].Rev)
		other, err := decode(withRenewal(t, legacy, 13, a, "2026-12-01"))
		if err != nil {
			t.Fatal(err)
		}
		other.Renewals[b] = Renewal{Date: "2027-02-05", Rev: 9, UpdatedAt: importNow}
		raw, err := encode(other)
		if err != nil {
			t.Fatal(err)
		}
		place(t, e.dropIn, raw)
		at := importNow.Add(10 * time.Second)
		raisedPast(t, e, s, s.Sync(at), at, held, "2027-02-05")
	})
	// Quota Glance's next saves, one that clears a row among them, go on from
	// the copy and are copied as they are.
	t.Run("Quota Glance's next saves", func(t *testing.T) {
		e := newImportEnv(t)
		place(t, e.sibling, legacy)
		s := Open(e.snapshot)
		s.Sync(importNow)
		v13 := withRenewal(t, legacy, 13, b, "2026-11-20")
		cleared, err := decode(v13)
		if err != nil {
			t.Fatal(err)
		}
		cleared.Revision = 14
		delete(cleared.Renewals, a)
		v14, err := encode(cleared)
		if err != nil {
			t.Fatal(err)
		}
		for i, raw := range [][]byte{v13, v14} {
			place(t, e.sibling, raw)
			if result := s.Sync(importNow.Add(time.Duration(i+1) * 10 * time.Second)); result.Imported == nil || !bytes.Equal(readFile(t, s.Path()), raw) {
				t.Fatalf("save %d not copied as it is: %+v", i, result)
			}
		}
		if r := recordAt(t, e.snapshot); r.SHA256 != r.SettingsSHA256 || r.LegacyRevision != 14 {
			t.Fatalf("record %+v", r)
		}
	})
}

// An import that wrote the settings file but not its record writes the record
// before anything is judged against it again: at the next tick, or before a
// save on the page, which is refused while it cannot be written. A save on the
// page first, or Quota Glance's next save, would otherwise leave a settings
// file that holds the import with a record that says it does not, for good.
// Once the settings file or the record changed, it is not written.
func TestAnImportRecordThatCouldNotBeWrittenIsWrittenLater(t *testing.T) {
	legacy := legacyFixture(t, "five-orgs")
	v13 := atRevision(t, legacy, 13, "300")
	// followed is a store that imported Quota Glance's file and followed its
	// save at 13, whose record could not be written; fail fails record writes.
	followed := func(t *testing.T) (importEnv, *Store, *bool) {
		t.Helper()
		e := newImportEnv(t)
		place(t, e.sibling, legacy)
		s := Open(e.snapshot)
		s.Sync(importNow)
		place(t, e.sibling, v13)
		fail := true
		recordWrites(s, &fail)
		if result := s.Sync(importNow.Add(10 * time.Second)); result.Imported == nil || !bytes.Equal(readFile(t, s.Path()), v13) || recordAt(t, e.snapshot).LegacyRevision != 12 {
			t.Fatalf("follow: %+v", result)
		}
		return e, s, &fail
	}
	recorded := func(t *testing.T, name string, e importEnv, s *Store, kind string, raw []byte, following bool) {
		t.Helper()
		if r := recordAt(t, e.snapshot); r.Kind != kind || r.SHA256 != hashOf(raw) || len(r.Lineage) != 1 {
			t.Fatalf("%s: record %+v", name, r)
		}
		if imp := s.Status().Import; imp == nil || imp.State != client.CreditImportStateImported || imp.Following != following || *imp.Kind != kind {
			t.Fatalf("%s: import %+v", name, imp)
		}
	}
	t.Run("a save on the page first", func(t *testing.T) {
		e, s, fail := followed(t)
		if _, err := s.Apply(renewalSave(renewalA, "2", "2026-11-01"), nil, importNow.Add(12*time.Second)); !errors.Is(err, ErrUnwritable) || !bytes.Equal(readFile(t, s.Path()), v13) {
			t.Fatalf("a save while the record cannot be written: %v", err)
		}
		*fail = false
		if _, err := s.Apply(renewalSave(renewalA, "2", "2026-11-01"), nil, importNow.Add(14*time.Second)); err != nil {
			t.Fatal(err)
		}
		recorded(t, "saved", e, s, client.CreditImportKindAutomatic, v13, false)
		s.Sync(importNow.Add(20 * time.Second))
		recorded(t, "next tick", e, s, client.CreditImportKindAutomatic, v13, false)
		again := Open(e.snapshot)
		again.Sync(importNow.Add(30 * time.Second))
		recorded(t, "reopened", e, again, client.CreditImportKindAutomatic, v13, false)
	})
	t.Run("Quota Glance's next save first", func(t *testing.T) {
		e, s, fail := followed(t)
		v14 := atRevision(t, v13, 14, "400")
		place(t, e.sibling, v14)
		*fail = false
		if result := s.Sync(importNow.Add(20 * time.Second)); result.Imported == nil || !bytes.Equal(readFile(t, s.Path()), v14) {
			t.Fatalf("not followed: %+v", result)
		}
		recorded(t, "followed", e, s, client.CreditImportKindAutomatic, v14, true)
	})
	t.Run("the automatic import", func(t *testing.T) {
		e := newImportEnv(t)
		place(t, e.sibling, legacy)
		s := Open(e.snapshot)
		fail := true
		recordWrites(s, &fail)
		if result := s.Sync(importNow); result.Imported == nil || exists(s.RecordPath()) {
			t.Fatalf("import: %+v", result)
		}
		fail = false
		if _, err := s.Apply(renewalSave(renewalA, "2", "2026-11-01"), nil, importNow.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		recorded(t, "saved", e, s, client.CreditImportKindAutomatic, legacy, false)
	})
	// After a manual import, Quota Glance's next save is followed raised.
	t.Run("a raised follow", func(t *testing.T) {
		e := newImportEnv(t)
		place(t, e.sibling, legacy)
		s := Open(e.snapshot)
		s.Sync(importNow)
		if _, err := s.ImportLegacy(RevisionText(12), importNow.Add(10*time.Second)); err != nil {
			t.Fatal(err)
		}
		place(t, e.sibling, v13)
		fail := true
		recordWrites(s, &fail)
		result := s.Sync(importNow.Add(20 * time.Second))
		if result.Imported == nil || s.Current().APICredits[fiveOrgA].MonthlyUSD != "300" {
			t.Fatalf("follow: %+v", result)
		}
		fail = false
		if _, err := s.Apply(renewalSave(renewalA, RevisionText(result.Imported.Revision), "2026-11-01"), nil, importNow.Add(22*time.Second)); err != nil {
			t.Fatal(err)
		}
		recorded(t, "saved", e, s, client.CreditImportKindManual, v13, false)
	})
	// Import Quota Glance's settings into a store whose settings file was
	// moved aside. The old record names the same file, which has not changed,
	// and the file the import writes is on a side of its lineage that holds
	// it, so without its own record the import would read as the automatic
	// one, never followed.
	t.Run("Import Quota Glance's settings", func(t *testing.T) {
		e := newImportEnv(t)
		place(t, e.sibling, legacy)
		s := Open(e.snapshot)
		s.Sync(importNow)
		if err := os.Remove(s.Path()); err != nil {
			t.Fatal(err)
		}
		s.Sync(importNow.Add(10 * time.Second))
		fail := true
		recordWrites(s, &fail)
		imported, err := s.ImportLegacy("", importNow.Add(20*time.Second))
		if err != nil || recordAt(t, e.snapshot).Kind != client.CreditImportKindAutomatic {
			t.Fatalf("import: %+v, %v", imported, err)
		}
		if _, err := s.Apply(renewalSave(renewalA, RevisionText(imported.Revision), "2026-11-01"), nil, importNow.Add(22*time.Second)); !errors.Is(err, ErrUnwritable) {
			t.Fatalf("a save while the record cannot be written: %v", err)
		}
		fail = false
		s.Sync(importNow.Add(30 * time.Second))
		if r := recordAt(t, e.snapshot); r.Kind != client.CreditImportKindManual || r.SHA256 != hashOf(legacy) || r.SettingsSHA256 != hashOf(readFile(t, s.Path())) ||
			!r.ImportedAt.Equal(importNow.Add(20*time.Second)) {
			t.Fatalf("record %+v", r)
		}
		if imp := s.Status().Import; imp.State != client.CreditImportStateImported || !imp.Following || *imp.Kind != client.CreditImportKindManual {
			t.Fatalf("import %+v", imp)
		}
		place(t, e.sibling, v13)
		if result := s.Sync(importNow.Add(40 * time.Second)); result.Imported == nil || s.Current().APICredits[fiveOrgA].MonthlyUSD != "300" {
			t.Fatalf("not followed: %+v", result)
		}
	})
	t.Run("not once the settings file changed", func(t *testing.T) {
		e, s, fail := followed(t)
		if err := os.Remove(s.Path()); err != nil {
			t.Fatal(err)
		}
		*fail = false
		s.Sync(importNow.Add(20 * time.Second))
		if r := recordAt(t, e.snapshot); r.LegacyRevision != 12 || importState(s) != client.CreditImportStateSettingsMissing {
			t.Fatalf("record %+v, state %s", r, importState(s))
		}
	})
	// The record removed by hand: the files match, which a tick records as
	// such, not as the import that could not write it.
	t.Run("not once the record changed", func(t *testing.T) {
		e, s, fail := followed(t)
		if err := os.Remove(s.RecordPath()); err != nil {
			t.Fatal(err)
		}
		*fail = false
		s.Sync(importNow.Add(20 * time.Second))
		if r := recordAt(t, e.snapshot); r.Kind != client.CreditImportKindMatched || r.SHA256 != hashOf(v13) {
			t.Fatalf("record %+v", r)
		}
	})
}

// A revision at the top of the range, which only a file edited by hand holds,
// is never wrapped round to a lower one: nothing is written that would need a
// revision past it, and the settings file's revision never goes backwards.
func TestNoRevisionIsWrittenPastTheHighest(t *testing.T) {
	const top = math.MaxUint64
	const a, b = "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"
	atTop, err := encode(Values{Revision: top, APICredits: map[string]APICredit{}, Renewals: map[string]Renewal{
		a: {Date: "2026-11-01", Rev: top, UpdatedAt: importNow},
		b: {Date: "2026-11-02", Rev: 1, UpdatedAt: importNow},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decode(atTop); err != nil {
		t.Fatalf("a file at the top revision does not load: %v", err)
	}
	unchanged := func(t *testing.T, s *Store, raw []byte) {
		t.Helper()
		if got := readFile(t, s.Path()); !bytes.Equal(got, raw) || s.Current().Revision != Load(s.Path()).Revision {
			t.Fatalf("the settings file changed:\n%s", got)
		}
	}
	// Imported as it is, its record's lineage starts at the top, and neither a
	// save nor an import can write past it.
	t.Run("the settings file", func(t *testing.T) {
		e := newImportEnv(t)
		place(t, e.sibling, atTop)
		s := Open(e.snapshot)
		if result := s.Sync(importNow); result.Imported == nil || s.Current().Revision != top {
			t.Fatalf("import: %+v", result)
		}
		if r := recordAt(t, e.snapshot); !reflect.DeepEqual(r.Lineage, []uint64{top}) {
			t.Fatalf("lineage %v", r.Lineage)
		}
		if _, err := s.Apply(renewalSave(b, "1", "2026-12-01"), nil, importNow.Add(time.Second)); !errors.Is(err, ErrFull) {
			t.Fatalf("a save: %v, want ErrFull", err)
		}
		if _, err := s.ImportLegacy(RevisionText(top), importNow.Add(2*time.Second)); !errors.Is(err, ErrFull) {
			t.Fatalf("an import: %v, want ErrFull", err)
		}
		unchanged(t, s, atTop)
	})
	// A store with a file of its own: Import Quota Glance's settings cannot
	// write past Quota Glance's file.
	t.Run("Quota Glance's file", func(t *testing.T) {
		e := newImportEnv(t)
		s := Open(e.snapshot)
		s.Sync(importNow)
		saved, err := s.Apply(renewalSave(a, "", "2026-12-01"), nil, importNow)
		if err != nil {
			t.Fatal(err)
		}
		ours := readFile(t, s.Path())
		place(t, e.sibling, atTop)
		s.Sync(importNow.Add(10 * time.Second))
		if _, err := s.ImportLegacy(RevisionText(saved.Revision), importNow.Add(20*time.Second)); !errors.Is(err, ErrFull) {
			t.Fatalf("an import: %v, want ErrFull", err)
		}
		unchanged(t, s, ours)
	})
	// A record whose lineage ends at the top: a save that would cross it
	// cannot.
	t.Run("the lineage", func(t *testing.T) {
		e := newImportEnv(t)
		ours := legacyFixture(t, "five-orgs")
		if err := os.WriteFile(FilePath(e.snapshot), ours, 0o600); err != nil {
			t.Fatal(err)
		}
		raw, err := encodeRecord(record{
			Kind: client.CreditImportKindAutomatic, SourcePath: e.sibling, SHA256: hashOf(ours), SettingsSHA256: hashOf(ours),
			LegacyRevision: 12, ImportedAt: importNow, Lineage: []uint64{top},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(RecordPath(e.snapshot), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		s := Open(e.snapshot)
		s.Sync(importNow)
		if _, err := s.Apply(renewalSave(renewalA, "2", "2026-11-01"), nil, importNow.Add(time.Second)); !errors.Is(err, ErrFull) {
			t.Fatalf("a save: %v, want ErrFull", err)
		}
		unchanged(t, s, ours)
	})
}

// While the import record is there but cannot be opened or read, as one
// restored as another user, nothing is saved: a save could not tell whether
// the file it writes holds the import, and a file started where the imported
// one went missing would read as the import once the record could be read
// again. A record that fails its rules counts as none, as before, and saves
// go on.
func TestASaveWaitsForAnImportRecordThatCannotBeRead(t *testing.T) {
	e := newImportEnv(t)
	place(t, e.sibling, legacyFixture(t, "five-orgs"))
	s := Open(e.snapshot)
	s.Sync(importNow)
	if err := os.Remove(s.Path()); err != nil {
		t.Fatal(err)
	}
	s.Sync(importNow.Add(10 * time.Second))
	fail := true
	s.read = func(path string, stamp fileStamp, limit int64) ([]byte, bool, bool) {
		if fail && path == s.RecordPath() {
			return nil, false, false
		}
		return readStamped(path, stamp, limit)
	}
	if _, err := s.Apply(renewalSave(renewalA, "", "2026-11-01"), nil, importNow.Add(20*time.Second)); !errors.Is(err, ErrUnavailable) || exists(s.Path()) {
		t.Fatalf("a start with the record unread: %v", err)
	}
	fail = false
	first, err := s.Apply(renewalSave(renewalA, "", "2026-11-01"), nil, importNow.Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	notImportedHolding(t, "started", s, "2026-11-01")
	again := Open(e.snapshot)
	again.Sync(importNow.Add(40 * time.Second))
	notImportedHolding(t, "reopened", again, "2026-11-01")
	// A save over a file waits too, and goes once the record reads.
	fail = true
	over := renewalSave(renewalA, RevisionText(first.Revision), "2026-11-02")
	if _, err := s.Apply(over, nil, importNow.Add(50*time.Second)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a save with the record unread: %v", err)
	}
	fail = false
	if _, err := s.Apply(over, nil, importNow.Add(60*time.Second)); err != nil {
		t.Fatal(err)
	}
	notImportedHolding(t, "saved", s, "2026-11-02")
	// A record that fails a rule counts as none, and does not hold a save.
	place(t, s.RecordPath(), []byte(`{"schema":1}`))
	if _, err := s.Apply(renewalSave(renewalA, RevisionText(s.Current().Renewals[renewalA].Rev), "2026-11-03"), nil, importNow.Add(70*time.Second)); err != nil {
		t.Fatalf("with a record that fails a rule: %v", err)
	}
}

// While the import record is there but cannot be opened or read, the status
// says so, so the page and the save route hold before a save meets it, while
// the saved values go on applying. What the record says is not known, so the
// import stands as last published rather than reading as not imported, which
// would offer an import that replaces what was saved here; and an import is
// refused as a save is. Once the record reads, editing is back.
func TestAnImportRecordThatCannotBeReadIsPublished(t *testing.T) {
	e := newImportEnv(t)
	place(t, e.sibling, legacyFixture(t, "five-orgs"))
	s := Open(e.snapshot)
	s.Sync(importNow)
	saved, err := s.Apply(renewalSave(renewalA, RevisionText(s.Current().Renewals[renewalA].Rev), "2026-11-01"), nil, importNow.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	importedHolding(t, "saved", s, "2026-11-01")
	if status := s.Status(); status.RecordUnread || !status.Editable() {
		t.Fatalf("the record reads: %+v", status)
	}
	last := s.Status().Import
	fail := true
	s.read = func(path string, stamp fileStamp, limit int64) ([]byte, bool, bool) {
		if fail && path == s.RecordPath() {
			return nil, false, false
		}
		return readStamped(path, stamp, limit)
	}
	unread := func(name string) {
		t.Helper()
		status := s.Status()
		if status.State != client.CreditSettingsOK || !status.RecordUnread || status.Editable() || !reflect.DeepEqual(status.Import, last) {
			t.Fatalf("%s: %+v, import %+v, want %+v", name, status, status.Import, last)
		}
		importedHolding(t, name, s, "2026-11-01")
	}
	s.Sync(importNow.Add(20 * time.Second))
	unread("at the tick")
	if _, err := s.ImportLegacy(RevisionText(saved.Revision), importNow.Add(30*time.Second)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("an import with the record unread: %v", err)
	}
	unread("after the import")
	if _, err := s.Apply(renewalSave(renewalA, RevisionText(saved.Revision), "2026-11-02"), nil, importNow.Add(40*time.Second)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a save with the record unread: %v", err)
	}
	unread("after the save")
	if v := Load(s.Path()); v.Revision != saved.Revision || v.Renewals[renewalA].Date != "2026-11-01" {
		t.Fatalf("written while the record was unread: %+v", v)
	}

	fail = false
	s.Sync(importNow.Add(50 * time.Second))
	if status := s.Status(); status.RecordUnread || !status.Editable() || !reflect.DeepEqual(status.Import, last) {
		t.Fatalf("the record reads again: %+v, import %+v", status, status.Import)
	}
	if _, err := s.Apply(renewalSave(renewalA, RevisionText(saved.Revision), "2026-11-02"), nil, importNow.Add(60*time.Second)); err != nil {
		t.Fatal(err)
	}
	importedHolding(t, "saved again", s, "2026-11-02")

	// A store opened while the record cannot be opened has published nothing
	// to keep: no import note, and no save or import.
	t.Run("at open", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root opens a file whatever its mode")
		}
		if err := os.Chmod(s.RecordPath(), 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(s.RecordPath(), 0o600) })
		again := Open(e.snapshot)
		again.Sync(importNow.Add(70 * time.Second))
		if status := again.Status(); status.State != client.CreditSettingsOK || !status.RecordUnread || status.Editable() || status.Import != nil {
			t.Fatalf("opened with the record unread: %+v, import %+v", status, status.Import)
		}
		if _, err := again.ImportLegacy(RevisionText(again.Current().Revision), importNow.Add(80*time.Second)); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("an import with the record unread: %v", err)
		}
		if err := os.Chmod(s.RecordPath(), 0o600); err != nil {
			t.Fatal(err)
		}
		again.Sync(importNow.Add(90 * time.Second))
		if status := again.Status(); status.RecordUnread || !status.Editable() || status.Import == nil || status.Import.State != client.CreditImportStateImported {
			t.Fatalf("the record reads again: %+v, import %+v", status, status.Import)
		}
	})
}

// An import record that cannot be opened or read is not replaced when the
// files match: what it says is not known, so the tick leaves it, and the
// import as last published, until a read settles. Fixing its owner or
// permissions is then enough: an automatic import, or the rollback copy after
// Import Quota Glance's settings, keeps its kind, imported_at and lineage, and
// with them the note's line about what to enter again.
func TestAnImportRecordThatCannotBeReadIsNotReplacedWhenTheFilesMatch(t *testing.T) {
	legacy := legacyFixture(t, "five-orgs")
	// failRead fails every read of the record while fail says so, as one that
	// cannot be opened does.
	failRead := func(s *Store) func(bool) {
		fail := true
		s.read = func(path string, stamp fileStamp, limit int64) ([]byte, bool, bool) {
			if fail && path == s.RecordPath() {
				return nil, false, false
			}
			return readStamped(path, stamp, limit)
		}
		return func(on bool) { fail = on }
	}
	// chmodRecord makes the record unopenable as one restored as another user
	// is, until it is given back.
	chmodRecord := func(t *testing.T, s *Store) func(bool) {
		if os.Geteuid() == 0 {
			t.Skip("root opens a file whatever its mode")
		}
		t.Cleanup(func() { _ = os.Chmod(s.RecordPath(), 0o600) })
		return func(on bool) {
			mode := os.FileMode(0o600)
			if on {
				mode = 0
			}
			if err := os.Chmod(s.RecordPath(), mode); err != nil {
				t.Fatal(err)
			}
		}
	}
	held := func(t *testing.T, s *Store, kind string, at time.Time, unreadable func(bool)) {
		t.Helper()
		raw := readFile(t, s.RecordPath())
		before, err := decodeRecord(raw)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(s.RecordPath())
		if err != nil {
			t.Fatal(err)
		}
		last := s.Status().Import
		if last == nil || last.State != client.CreditImportStateImported || *last.Kind != kind || !bytes.Equal(readFile(t, s.Path()), readFile(t, before.SourcePath)) {
			t.Fatalf("before: import %+v", last)
		}
		unreadable(true)
		if result := s.Sync(at); result.Imported != nil {
			t.Fatalf("imported with the record unread: %+v", result)
		}
		if status := s.Status(); !status.RecordUnread || status.Editable() || !reflect.DeepEqual(status.Import, last) {
			t.Fatalf("with the record unread: %+v, import %+v, want %+v", status, status.Import, last)
		}
		unreadable(false)
		if after, err := os.Lstat(s.RecordPath()); err != nil || !os.SameFile(info, after) || !bytes.Equal(readFile(t, s.RecordPath()), raw) {
			t.Fatalf("the record was replaced: %s", readFile(t, s.RecordPath()))
		}
		s.Sync(at.Add(10 * time.Second))
		if status := s.Status(); status.RecordUnread || !status.Editable() || !reflect.DeepEqual(status.Import, last) {
			t.Fatalf("the record reads again: %+v, import %+v, want %+v", status, status.Import, last)
		}
		if after, err := decodeRecord(readFile(t, s.RecordPath())); err != nil || !reflect.DeepEqual(after, before) {
			t.Fatalf("record %+v, want %+v", after, before)
		}
	}
	automatic := func(t *testing.T) *Store {
		e := newImportEnv(t)
		place(t, e.sibling, legacy)
		s := Open(e.snapshot)
		if result := s.Sync(importNow); result.Imported == nil || result.Imported.Kind != client.CreditImportKindAutomatic {
			t.Fatalf("the first import: %+v", result)
		}
		return s
	}
	t.Run("automatic", func(t *testing.T) {
		s := automatic(t)
		held(t, s, client.CreditImportKindAutomatic, importNow.Add(10*time.Second), failRead(s))
	})
	t.Run("automatic, restored as another user", func(t *testing.T) {
		s := automatic(t)
		held(t, s, client.CreditImportKindAutomatic, importNow.Add(10*time.Second), chmodRecord(t, s))
	})
	t.Run("manual, after the rollback copy", func(t *testing.T) {
		e := newImportEnv(t)
		place(t, e.sibling, legacy)
		s := Open(e.snapshot)
		s.Sync(importNow)
		if _, err := s.Apply(renewalSave("aaaaaaaaaaaaaaaa", "", "2026-12-01"), nil, importNow); err != nil {
			t.Fatal(err)
		}
		place(t, e.sibling, atRevision(t, legacy, 13, "300"))
		s.Sync(importNow.Add(10 * time.Second))
		if _, err := s.ImportLegacy(RevisionText(s.Current().Revision), importNow.Add(20*time.Second)); err != nil {
			t.Fatal(err)
		}
		place(t, e.sibling, readFile(t, s.Path()))
		s.Sync(importNow.Add(30 * time.Second))
		held(t, s, client.CreditImportKindManual, importNow.Add(40*time.Second), failRead(s))
	})
}

// Once a record exists, the legacy file is the record's unless a drop-in is
// placed: removing an imported drop-in never switches to another candidate,
// and a drop-in placed later is what the import button imports. A drop-in is
// a copy Quota Glance never saves to, so an import of it never reports
// following Quota Glance, though the drop-in placed again with a newer
// revision is still copied while nothing was saved here.
func TestLegacyFileFollowsTheRecord(t *testing.T) {
	five, other := legacyFixture(t, "five-orgs"), legacyFixture(t, "api-credits")
	e := newImportEnv(t)
	place(t, e.dropIn, five)
	place(t, e.sibling, other)
	s := Open(e.snapshot)
	s.Sync(importNow)
	if r := recordAt(t, e.snapshot); r.SourcePath != e.dropIn {
		t.Fatalf("record %+v", r)
	}
	if imp := s.Status().Import; imp.State != client.CreditImportStateImported || !imp.Importable || imp.Following {
		t.Fatalf("drop-in imported: %+v", imp)
	}
	again := atRevision(t, five, 13, "300")
	place(t, e.dropIn, again)
	if result := s.Sync(importNow.Add(5 * time.Second)); result.Imported == nil || result.Imported.Kind != client.CreditImportKindFollow || !bytes.Equal(readFile(t, s.Path()), again) {
		t.Fatalf("the drop-in placed again was not copied: %+v", result)
	}
	if imp := s.Status().Import; imp.State != client.CreditImportStateImported || imp.Following {
		t.Fatalf("drop-in placed again: %+v", imp)
	}
	if err := os.Remove(e.dropIn); err != nil {
		t.Fatal(err)
	}
	s.Sync(importNow.Add(10 * time.Second))
	imp := s.Status().Import
	if imp.State != client.CreditImportStateImported || imp.Importable || imp.Following || *imp.SourcePath != e.dropIn {
		t.Fatalf("drop-in removed: %+v", imp)
	}
	// The sibling saves again; nothing follows it, and the button has
	// nothing to import.
	place(t, e.sibling, atRevision(t, five, 40, "999"))
	if result := s.Sync(importNow.Add(20 * time.Second)); result.Imported != nil || !bytes.Equal(readFile(t, s.Path()), again) {
		t.Fatalf("followed the sibling: %+v", result)
	}
	if _, err := s.ImportLegacy("12", importNow); !errors.Is(err, ErrNotImportable) {
		t.Fatalf("imported the sibling: %v", err)
	}

	// A drop-in placed after an import of Quota Glance's file, with nothing
	// saved here since, is never followed, even at a revision past the
	// record's: it is another file.
	e = newImportEnv(t)
	place(t, e.sibling, five)
	s = Open(e.snapshot)
	s.Sync(importNow)
	later := atRevision(t, other, 40, "999")
	place(t, e.dropIn, later)
	if result := s.Sync(importNow.Add(10 * time.Second)); result.Imported != nil || !bytes.Equal(readFile(t, s.Path()), five) {
		t.Fatalf("a later drop-in was imported automatically: %+v", result)
	}
	if imp := s.Status().Import; imp.State != client.CreditImportStateNotImported || !imp.Importable || imp.Following {
		t.Fatalf("later drop-in: %+v", imp)
	}
	imported, err := s.ImportLegacy("12", importNow.Add(time.Minute))
	if err != nil || imported.Revision != uint64(importNow.Add(time.Minute).Unix()) {
		t.Fatalf("import: %+v, %v", imported, err)
	}
	want, _ := decode(later)
	if got := s.Current(); len(got.APICredits) != len(want.APICredits) || got.APICredits["org-752c8e5b3c4d"].MonthlyUSD != "300" ||
		got.APICredits[fiveOrgA].MonthlyUSD != "999" {
		t.Fatalf("values %+v", got)
	}
	if r := recordAt(t, e.snapshot); r.SourcePath != e.dropIn || r.Kind != client.CreditImportKindManual {
		t.Fatalf("record %+v", r)
	}
}

// A save before any legacy file, then a drop-in: offered, never imported
// automatically, and the button takes the drop-in's values.
func TestSaveFirstThenDropIn(t *testing.T) {
	e := newImportEnv(t)
	s := Open(e.snapshot)
	s.Sync(importNow)
	if _, err := s.Apply(renewalSave("aaaaaaaaaaaaaaaa", "", "2026-12-01"), nil, importNow); err != nil {
		t.Fatal(err)
	}
	place(t, e.dropIn, legacyFixture(t, "five-orgs"))
	if result := s.Sync(importNow.Add(10 * time.Second)); result.Imported != nil {
		t.Fatalf("imported automatically: %+v", result)
	}
	if imp := s.Status().Import; imp.State != client.CreditImportStateNotImported || !imp.Importable || imp.Kind != nil {
		t.Fatalf("import %+v", imp)
	}
	if _, err := s.ImportLegacy(RevisionText(s.Current().Revision), importNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if v := s.Current(); v.APICredits[fiveOrgA].MonthlyUSD != "200" || len(v.Renewals) != 3 || v.Renewals["aaaaaaaaaaaaaaaa"].Date != "" {
		t.Fatalf("values %+v", v)
	}
}

// A rollback copies the settings file over Quota Glance's; the files match, a
// matched record re-arms follow, and Quota Glance's next save is followed.
func TestRollbackCopyRearmsFollow(t *testing.T) {
	e := newImportEnv(t)
	place(t, e.sibling, legacyFixture(t, "five-orgs"))
	s := Open(e.snapshot)
	s.Sync(importNow)
	saved, err := s.Apply(renewalSave("aaaaaaaaaaaaaaaa", "", "2026-12-01"), nil, importNow)
	if err != nil {
		t.Fatal(err)
	}
	ours := readFile(t, s.Path())
	place(t, e.sibling, ours)
	if result := s.Sync(importNow.Add(10 * time.Second)); result.Imported != nil {
		t.Fatalf("result %+v", result)
	}
	if r := recordAt(t, e.snapshot); r.Kind != client.CreditImportKindMatched || r.SHA256 != hashOf(ours) || r.SettingsSHA256 != hashOf(ours) || r.LegacyRevision != saved.Revision {
		t.Fatalf("record %+v", r)
	}
	if imp := s.Status().Import; imp.State != client.CreditImportStateImported || !imp.Following {
		t.Fatalf("import %+v", imp)
	}
	next := atRevision(t, ours, saved.Revision+1, "321")
	place(t, e.sibling, next)
	if result := s.Sync(importNow.Add(20 * time.Second)); result.Imported == nil || result.Imported.Kind != client.CreditImportKindFollow {
		t.Fatalf("not followed: %+v", result)
	}
	if !bytes.Equal(readFile(t, s.Path()), next) || s.Current().Renewals["aaaaaaaaaaaaaaaa"].Date != "2026-12-01" {
		t.Fatal("follow did not copy the rolled-back file's save")
	}
}

// The rollback copy after Import Quota Glance's settings, with nothing saved
// here since: the import raised the settings file's revisions, so its record
// says it is no copy of Quota Glance's file, and the copy makes it one. The
// files are found equal and recorded as such, keeping the import's kind, whose
// note says what to enter again; nothing is rewritten, so a page open on the
// imported file goes on saving, and Quota Glance 0.7.2's later saves are
// copied as they are.
func TestRollbackCopyAfterAnImportRearmsFollow(t *testing.T) {
	legacy := legacyFixture(t, "five-orgs")
	e := newImportEnv(t)
	place(t, e.sibling, legacy)
	s := Open(e.snapshot)
	s.Sync(importNow)
	// Saved here, then in Quota Glance: the note reads Changed in Quota
	// Glance, and the operator imports before the copy.
	if _, err := s.Apply(renewalSave("aaaaaaaaaaaaaaaa", "", "2026-12-01"), nil, importNow); err != nil {
		t.Fatal(err)
	}
	place(t, e.sibling, atRevision(t, legacy, 13, "300"))
	s.Sync(importNow.Add(10 * time.Second))
	imported, err := s.ImportLegacy(RevisionText(s.Current().Revision), importNow.Add(20*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	ours := readFile(t, s.Path())
	place(t, e.sibling, ours)
	if result := s.Sync(importNow.Add(30 * time.Second)); result.Imported != nil || !bytes.Equal(readFile(t, s.Path()), ours) {
		t.Fatalf("the copy was imported again: %+v", result)
	}
	if r := recordAt(t, e.snapshot); r.Kind != client.CreditImportKindManual || r.SHA256 != hashOf(ours) || r.SettingsSHA256 != hashOf(ours) ||
		r.LegacyRevision != imported.Revision {
		t.Fatalf("record %+v", r)
	}
	if imp := s.Status().Import; imp.State != client.CreditImportStateImported || !imp.Following || *imp.Kind != client.CreditImportKindManual {
		t.Fatalf("import %+v", imp)
	}
	// A page that read the imported file saves a row Quota Glance has not
	// changed since.
	page := RevisionText(s.Current().Renewals[renewalA].Rev)
	next := atRevision(t, ours, imported.Revision+1, "321")
	place(t, e.sibling, next)
	if result := s.Sync(importNow.Add(40 * time.Second)); result.Imported == nil || result.Imported.Kind != client.CreditImportKindFollow || result.Imported.Revision != imported.Revision+1 {
		t.Fatalf("not followed: %+v", result)
	}
	if !bytes.Equal(readFile(t, s.Path()), next) || recordAt(t, e.snapshot).Kind != client.CreditImportKindManual {
		t.Fatal("follow did not copy Quota Glance's save as it is")
	}
	if _, err := s.Apply(renewalSave(renewalA, page, "2026-11-20"), nil, importNow.Add(50*time.Second)); err != nil {
		t.Fatalf("a page open on the imported file: %v", err)
	}
}

// Undoing Import Quota Glance's settings: the settings file from before it,
// put back and then copied over Quota Glance's for the rollback, holds what
// was saved here, so the files are recorded as matched, whose note says
// nothing needs entering again, not as the import, whose note says to enter
// those values again. It is no later save of a file the import copied as it
// is, whether the import's own record is there or the one the rollback copy
// of its file wrote.
func TestAFileFromBeforeAManualImportPutBackIsMatched(t *testing.T) {
	legacy := legacyFixture(t, "five-orgs")
	for _, rearmed := range []bool{false, true} {
		t.Run(fmt.Sprintf("rearmed=%v", rearmed), func(t *testing.T) {
			e := newImportEnv(t)
			place(t, e.sibling, legacy)
			s := Open(e.snapshot)
			s.Sync(importNow)
			if _, err := s.Apply(renewalSave("aaaaaaaaaaaaaaaa", "", "2026-12-01"), nil, importNow); err != nil {
				t.Fatal(err)
			}
			before := readFile(t, s.Path())
			place(t, e.sibling, atRevision(t, legacy, 13, "300"))
			s.Sync(importNow.Add(10 * time.Second))
			if _, err := s.ImportLegacy(RevisionText(s.Current().Revision), importNow.Add(20*time.Second)); err != nil {
				t.Fatal(err)
			}
			if rearmed {
				place(t, e.sibling, readFile(t, s.Path()))
				s.Sync(importNow.Add(30 * time.Second))
				if r := recordAt(t, e.snapshot); r.Kind != client.CreditImportKindManual || r.SHA256 != r.SettingsSHA256 {
					t.Fatalf("the rollback copy: record %+v", r)
				}
			}
			place(t, s.Path(), before)
			s.Sync(importNow.Add(40 * time.Second))
			if state := importState(s); state != client.CreditImportStateNotImported {
				t.Fatalf("put back: %s", state)
			}
			place(t, e.sibling, before)
			if result := s.Sync(importNow.Add(50 * time.Second)); result.Imported != nil {
				t.Fatalf("imported: %+v", result)
			}
			if r := recordAt(t, e.snapshot); r.Kind != client.CreditImportKindMatched || r.SHA256 != hashOf(before) || r.SettingsSHA256 != hashOf(before) {
				t.Fatalf("record %+v", r)
			}
			if imp := s.Status().Import; imp.State != client.CreditImportStateImported || !imp.Following || *imp.Kind != client.CreditImportKindMatched {
				t.Fatalf("import %+v", imp)
			}
			if !bytes.Equal(readFile(t, s.Path()), before) || s.Current().Renewals["aaaaaaaaaaaaaaaa"].Date != "2026-12-01" {
				t.Fatal("the settings file put back was rewritten")
			}
		})
	}
}

// The same after an automatic import: a settings file saved here before
// Quota Glance's file was found, moved aside, which is a first start, then
// put back and copied over Quota Glance's for the rollback, holds every value
// saved here before the import. Its revision is past the import's, which is
// Quota Glance's own, and below where the import's lineage starts, as a
// verbatim follow's copy is, but each row saved here is stamped before the
// import, so the files are recorded as matched, not as the import.
func TestAFileFromBeforeAnAutomaticImportPutBackIsMatched(t *testing.T) {
	e := newImportEnv(t)
	s := Open(e.snapshot)
	s.Sync(importNow)
	if _, err := s.Apply(renewalSave("aaaaaaaaaaaaaaaa", "", "2026-12-01"), nil, importNow); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, s.Path())
	if err := os.Rename(s.Path(), s.Path()+".aside"); err != nil {
		t.Fatal(err)
	}
	place(t, e.sibling, legacyFixture(t, "five-orgs"))
	if result := s.Sync(importNow.Add(time.Minute)); result.Imported == nil || result.Imported.Kind != client.CreditImportKindAutomatic {
		t.Fatalf("the first start: %+v", result)
	}
	if r := recordAt(t, e.snapshot); r.LegacyRevision >= Load(s.Path()+".aside").Revision || r.Lineage[0] <= Load(s.Path()+".aside").Revision {
		t.Fatalf("record %+v", r)
	}
	place(t, s.Path(), before)
	s.Sync(importNow.Add(2 * time.Minute))
	if state := importState(s); state != client.CreditImportStateNotImported {
		t.Fatalf("put back: %s", state)
	}
	place(t, e.sibling, before)
	if result := s.Sync(importNow.Add(3 * time.Minute)); result.Imported != nil {
		t.Fatalf("imported: %+v", result)
	}
	if r := recordAt(t, e.snapshot); r.Kind != client.CreditImportKindMatched || r.SHA256 != hashOf(before) || r.SettingsSHA256 != hashOf(before) {
		t.Fatalf("record %+v", r)
	}
	if imp := s.Status().Import; imp.State != client.CreditImportStateImported || !imp.Following || *imp.Kind != client.CreditImportKindMatched {
		t.Fatalf("import %+v", imp)
	}
	if !bytes.Equal(readFile(t, s.Path()), before) || s.Current().Renewals["aaaaaaaaaaaaaaaa"].Date != "2026-12-01" {
		t.Fatal("the settings file put back was rewritten")
	}
}

// After Import Quota Glance's settings, Quota Glance's file copied over the
// settings file makes the two equal, though the record's sha256 alone is
// already that file's: its settings_sha256 is the raised file's. The files
// are recorded as matched, so follow goes on, raised past the revisions the
// import gave out, since a page may still hold one.
func TestQuotaGlancesFileCopiedOverAManualImportIsMatched(t *testing.T) {
	legacy := legacyFixture(t, "five-orgs")
	e := newImportEnv(t)
	place(t, e.sibling, legacy)
	s := Open(e.snapshot)
	s.Sync(importNow)
	imported, err := s.ImportLegacy(RevisionText(12), importNow.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if r := recordAt(t, e.snapshot); r.SHA256 != hashOf(legacy) || r.SettingsSHA256 == r.SHA256 {
		t.Fatalf("the import: record %+v", r)
	}
	place(t, s.Path(), legacy)
	if result := s.Sync(importNow.Add(20 * time.Second)); result.Imported != nil {
		t.Fatalf("imported: %+v", result)
	}
	if r := recordAt(t, e.snapshot); r.Kind != client.CreditImportKindMatched || r.SHA256 != hashOf(legacy) || r.SettingsSHA256 != hashOf(legacy) || r.LegacyRevision != 12 {
		t.Fatalf("record %+v", r)
	}
	if imp := s.Status().Import; imp.State != client.CreditImportStateImported || !imp.Following {
		t.Fatalf("import %+v", imp)
	}
	place(t, e.sibling, atRevision(t, legacy, 13, "300"))
	result := s.Sync(importNow.Add(30 * time.Second))
	followed := uint64(importNow.Add(30 * time.Second).Unix())
	if result.Imported == nil || result.Imported.Kind != client.CreditImportKindFollow || result.Imported.Revision != followed || followed <= imported.Revision {
		t.Fatalf("follow: %+v", result)
	}
	if v := s.Current(); v.APICredits[fiveOrgA].MonthlyUSD != "300" || v.APICredits[fiveOrgA].Rev != followed || v.Renewals[renewalA].Rev != followed {
		t.Fatalf("values %+v", v)
	}
}

// Moving cache-path. With both files moved nothing is imported; with the
// record but not the settings file the store starts empty; with neither, the
// move looks like a first start and imports again, and the record says how old
// the values are.
func TestCachePathMove(t *testing.T) {
	legacy := legacyFixture(t, "five-orgs")
	e := newImportEnv(t)
	place(t, e.sibling, legacy)
	s := Open(e.snapshot)
	s.Sync(importNow)
	moved := func(name string, files ...string) string {
		snapshot := filepath.Join(e.root, name, "snapshot.json")
		if err := os.Mkdir(filepath.Dir(snapshot), 0o700); err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			from := strings.TrimSuffix(e.snapshot, "snapshot.json") + file
			if err := os.WriteFile(strings.TrimSuffix(snapshot, "snapshot.json")+file, readFile(t, from), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return snapshot
	}

	both := Open(moved("both", "snapshot.settings.json", "snapshot.settings-import.json"))
	order := recordWrites(both, nil)
	if result := both.Sync(importNow.Add(time.Hour)); result.Imported != nil || len(*order) != 0 || importState(both) != client.CreditImportStateImported {
		t.Fatalf("both moved: %+v, wrote %v, %s", result, *order, importState(both))
	}
	if want, _ := decode(legacy); !reflect.DeepEqual(both.Current(), want) {
		t.Fatal("both moved: values lost")
	}

	recordOnly := Open(moved("record", "snapshot.settings-import.json"))
	order = recordWrites(recordOnly, nil)
	if result := recordOnly.Sync(importNow.Add(time.Hour)); result.Imported != nil || len(*order) != 0 ||
		importState(recordOnly) != client.CreditImportStateSettingsMissing || len(recordOnly.Current().APICredits) != 0 {
		t.Fatalf("record moved: %+v, wrote %v, %s", result, *order, importState(recordOnly))
	}

	neither := Open(moved("neither"))
	result := neither.Sync(importNow.Add(time.Hour))
	if result.Imported == nil || result.Imported.Kind != client.CreditImportKindAutomatic {
		t.Fatalf("neither moved: %+v", result)
	}
	updated := time.Date(2026, 10, 10, 18, 20, 0, 0, time.UTC)
	if r := recordAt(t, filepath.Join(e.root, "neither", "snapshot.json")); r.LegacyUpdatedAt == nil || !r.LegacyUpdatedAt.Equal(updated) ||
		!r.ImportedAt.Equal(importNow.Add(time.Hour)) {
		t.Fatalf("record %+v", r)
	}
	if imp := neither.Status().Import; !imp.LegacyUpdatedAt.Equal(updated) {
		t.Fatalf("import %+v", imp)
	}
}

// A settings file that cannot be read is read again at the tick after it is
// moved aside or fixed, with no configure.
func TestUnreadableSettingsReopenOnTheNextTick(t *testing.T) {
	e := newImportEnv(t)
	if err := os.WriteFile(FilePath(e.snapshot), []byte(`{"schema":1,"revision":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := Open(e.snapshot)
	if s.Status().State != client.CreditSettingsUnreadable || !s.Current().Unreadable {
		t.Fatalf("open: %+v", s.Status())
	}
	if result := s.Sync(importNow); result.Reloaded || s.Status().State != client.CreditSettingsUnreadable {
		t.Fatalf("unchanged: %+v", result)
	}
	if err := os.Rename(s.Path(), s.Path()+".aside"); err != nil {
		t.Fatal(err)
	}
	if result := s.Sync(importNow.Add(10 * time.Second)); !result.Reloaded || s.Status().State != client.CreditSettingsOK || s.Current().Unreadable {
		t.Fatalf("moved aside: %+v, %+v", result, s.Status())
	}
	if _, err := s.Apply(renewalSave(renewalA, "", "2026-11-01"), nil, importNow); err != nil {
		t.Fatalf("a save once moved aside: %v", err)
	}

	// Fixed in place, the same file now reads.
	e = newImportEnv(t)
	if err := os.WriteFile(FilePath(e.snapshot), []byte(`{"schema":1,"revision":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s = Open(e.snapshot)
	good := legacyFixture(t, "api-credits")
	if err := os.WriteFile(FilePath(e.snapshot), good, 0o600); err != nil {
		t.Fatal(err)
	}
	if result := s.Sync(importNow); !result.Reloaded || s.Status().State != client.CreditSettingsOK || s.Current().Revision != 2 {
		t.Fatalf("fixed: %+v, %+v", result, s.Current())
	}
}

// With no import record, as after a save here before Quota Glance's file was
// found, an unreadable settings file moved aside is a first start, not an
// empty store: Quota Glance's file is imported automatically, though the page
// offered it and nothing was imported, and one that cannot be read blocks
// editing. The page's remedy and the docs say so.
func TestUnreadableSettingsMovedAsideWithNoRecordIsAFirstStart(t *testing.T) {
	for _, tc := range []struct {
		name     string
		legacy   []byte
		readable bool
	}{
		{"readable", legacyFixture(t, "five-orgs"), true},
		{"unreadable", []byte(`{"schema":1,"revision":"x"}`), false},
	} {
		e := newImportEnv(t)
		s := Open(e.snapshot)
		s.Sync(importNow)
		if _, err := s.Apply(renewalSave(renewalA, "", "2026-11-01"), nil, importNow); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		place(t, e.dropIn, tc.legacy)
		if result := s.Sync(importNow.Add(10 * time.Second)); result.Imported != nil || result.LegacyUnreadable {
			t.Fatalf("%s: before the move: %+v", tc.name, result)
		}
		// A hand edit that fails a load rule.
		if err := os.WriteFile(s.Path(), []byte(`{"schema":1,"revision":"x"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if result := s.Sync(importNow.Add(20 * time.Second)); !result.Reloaded || result.Imported != nil || s.Status().State != client.CreditSettingsUnreadable || exists(s.RecordPath()) {
			t.Fatalf("%s: unreadable: %+v, %+v", tc.name, result, s.Status())
		}
		if err := os.Rename(s.Path(), s.Path()+".aside"); err != nil {
			t.Fatal(err)
		}
		result := s.Sync(importNow.Add(30 * time.Second))
		if !tc.readable {
			if !result.LegacyUnreadable || result.Imported != nil || s.Status().State != client.CreditSettingsLegacyUnreadable || exists(s.Path()) || exists(s.RecordPath()) {
				t.Fatalf("%s: moved aside: %+v, %+v", tc.name, result, s.Status())
			}
			if _, err := s.Apply(renewalSave(renewalA, "", "2026-11-01"), nil, importNow); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("%s: a save: %v", tc.name, err)
			}
			continue
		}
		if result.Imported == nil || result.Imported.Kind != client.CreditImportKindAutomatic || !bytes.Equal(readFile(t, s.Path()), tc.legacy) {
			t.Fatalf("%s: moved aside: %+v", tc.name, result)
		}
		// The date saved here gives way to the file's.
		if v := s.Current(); v.APICredits[fiveOrgA].MonthlyUSD != "200" || v.Renewals[renewalA].Date != "2026-10-29" {
			t.Fatalf("%s: values %+v", tc.name, v)
		}
		if r := recordAt(t, e.snapshot); r.Kind != client.CreditImportKindAutomatic || r.SourcePath != e.dropIn {
			t.Fatalf("%s: record %+v", tc.name, r)
		}
	}
}

// A settings file that could not be opened or read, as one restored as
// another user, is read again at every tick, and by a save, until a read
// settles, though its stamp never changed: fixing its owner changes none of
// what the stamp compares. One that fails a load rule is read again only once
// it changes.
func TestASettingsFileThatCouldNotBeOpenedIsReadAgain(t *testing.T) {
	e := newImportEnv(t)
	if err := os.WriteFile(FilePath(e.snapshot), legacyFixture(t, "api-credits"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := Open(e.snapshot)
	reads, fail := 0, true
	s.read = func(path string, stamp fileStamp, limit int64) ([]byte, bool, bool) {
		if path != s.Path() {
			return readStamped(path, stamp, limit)
		}
		reads++
		if fail {
			return nil, false, false
		}
		return readStamped(path, stamp, limit)
	}
	touch := func(at time.Time) {
		t.Helper()
		if err := os.Chtimes(s.Path(), at, at); err != nil {
			t.Fatal(err)
		}
	}
	touch(importNow.Add(time.Hour))
	if result := s.Sync(importNow); !result.Reloaded || s.Status().State != client.CreditSettingsUnreadable {
		t.Fatalf("a failed open: %+v, %+v", result, s.Status())
	}
	for range 2 {
		if result := s.Sync(importNow); !result.Reloaded || s.Status().State != client.CreditSettingsUnreadable {
			t.Fatalf("still failing: %+v, %+v", result, s.Status())
		}
	}
	fail = false
	if result := s.Sync(importNow); !result.Reloaded || reads != 4 || s.Status().State != client.CreditSettingsOK || s.Current().Revision != 2 {
		t.Fatalf("fixed: %+v, %d reads, %+v", result, reads, s.Status())
	}
	if result := s.Sync(importNow); result.Reloaded || reads != 4 {
		t.Fatalf("a settled read was read again: %+v, %d reads", result, reads)
	}

	// A save reads it again first.
	fail = true
	touch(importNow.Add(2 * time.Hour))
	if _, err := s.Apply(renewalSave(renewalA, "", "2026-11-01"), nil, importNow); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a save over a failed open: %v", err)
	}
	fail = false
	if _, err := s.Apply(renewalSave(renewalA, "", "2026-11-01"), nil, importNow); err != nil || s.Current().APICredits["org-752c8e5b3c4d"].MonthlyUSD != "300" {
		t.Fatalf("a save once it opens: %v", err)
	}

	// A rule failure settles: no tick reads it again until it changes.
	if err := os.WriteFile(s.Path(), []byte(`{"schema":1,"revision":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reads = 0
	for i := range 3 {
		if result := s.Sync(importNow); result.Reloaded != (i == 0) || s.Status().State != client.CreditSettingsUnreadable {
			t.Fatalf("a rule failure, tick %d: %+v", i, result)
		}
	}
	if reads != 1 {
		t.Fatalf("a rule failure read %d times", reads)
	}
}

// Each tick takes Quota Glance's file's Lstat, and reads and hashes it again
// only when the stamp (inode, size, modification time and mode) changed or
// it is another file. Unchanged, it costs one Lstat. A read that failed is
// tried again at the next tick, never kept.
func TestAnUnchangedLegacyFileIsNotReadAgain(t *testing.T) {
	legacy := legacyFixture(t, "five-orgs")
	e := newImportEnv(t)
	place(t, e.sibling, legacy)
	s := Open(e.snapshot)
	var reads []string
	fail := false
	s.read = func(path string, stamp fileStamp, limit int64) ([]byte, bool, bool) {
		if path == s.RecordPath() {
			return readStamped(path, stamp, limit)
		}
		reads = append(reads, filepath.Base(filepath.Dir(path))+"/"+filepath.Base(path))
		if fail {
			return nil, false, false
		}
		return readStamped(path, stamp, limit)
	}
	tick := importNow
	sync := func() SyncResult {
		tick = tick.Add(10 * time.Second)
		return s.Sync(tick)
	}
	want := func(step string, n int, state string) {
		t.Helper()
		if len(reads) != n || importState(s) != state {
			t.Fatalf("%s: reads %v, want %d; state %s, want %s", step, reads, n, importState(s), state)
		}
	}

	// Open read it; the first import and later ticks use what it read.
	if result := sync(); result.Imported == nil || result.Imported.Kind != client.CreditImportKindAutomatic {
		t.Fatalf("first import: %+v", result)
	}
	for range 5 {
		if result := sync(); result.Imported != nil {
			t.Fatalf("unchanged: %+v", result)
		}
	}
	want("unchanged", 0, client.CreditImportStateImported)

	// A new modification time is read again, though the bytes are the same.
	touched := importNow.Add(time.Hour)
	if err := os.Chtimes(e.sibling, touched, touched); err != nil {
		t.Fatal(err)
	}
	sync()
	sync()
	want("touched", 1, client.CreditImportStateImported)

	// A save in Quota Glance, by rename, is read once and followed.
	v13 := atRevision(t, legacy, 13, "300")
	place(t, e.sibling, v13)
	if result := sync(); result.Imported == nil || result.Imported.Kind != client.CreditImportKindFollow || !bytes.Equal(readFile(t, s.Path()), v13) {
		t.Fatalf("follow: %+v", result)
	}
	sync()
	want("followed", 2, client.CreditImportStateImported)

	// A read that fails is not kept: the next tick reads again, and follows.
	v14 := atRevision(t, v13, 14, "400")
	place(t, e.sibling, v14)
	fail = true
	if result := sync(); result.Imported != nil || s.Status().State != client.CreditSettingsOK {
		t.Fatalf("a failed read: %+v, %+v", result, s.Status())
	}
	want("a failed read", 3, client.CreditImportStateLegacyUnreadable)
	fail = false
	if result := sync(); result.Imported == nil || result.Imported.Revision != 14 {
		t.Fatalf("after a failed read: %+v", result)
	}
	sync()
	want("read again", 4, client.CreditImportStateImported)

	// Another file is read: a drop-in placed later, then kept as read.
	dropIn := legacyFixture(t, "api-credits")
	place(t, e.dropIn, dropIn)
	sync()
	sync()
	want("a drop-in", 5, client.CreditImportStateNotImported)
	if reads[4] != "qc/"+client.LegacySettingsDropIn {
		t.Fatalf("reads %v", reads)
	}
	if counts := s.Status().LegacyCounts; counts == nil || *counts != (client.CreditImportCounts{MonthlyCredits: 1, Readings: 1}) {
		t.Fatalf("drop-in counts %+v", counts)
	}

	// Import Quota Glance's settings takes what the tick read, unchanged.
	imported, err := s.ImportLegacy(RevisionText(s.Current().Revision), tick)
	if err != nil || imported.Revision != uint64(tick.Unix()) || imported.Counts != (client.CreditImportCounts{MonthlyCredits: 1, Readings: 1}) {
		t.Fatalf("import: %+v, %v", imported, err)
	}
	if r := recordAt(t, e.snapshot); r.Kind != client.CreditImportKindManual || r.SourcePath != e.dropIn || r.SHA256 != hashOf(dropIn) {
		t.Fatalf("record %+v", r)
	}
	sync()
	want("imported", 5, client.CreditImportStateImported)
}

// The import record loads with the meter's pattern, and every key in the form
// it is written: anything else is a record that cannot be read, which blocks
// the automatic import and ends follow until a later import replaces it.
func TestImportRecordLoadRules(t *testing.T) {
	updated := time.Date(2026, 10, 10, 18, 20, 0, 0, time.UTC)
	valid := record{
		Kind: client.CreditImportKindAutomatic, SourcePath: "/CLIProxyAPI/plugins/data/quota-glance/settings.json",
		SHA256: strings.Repeat("a", 64), SettingsSHA256: strings.Repeat("b", 64), LegacyRevision: 12, LegacyUpdatedAt: &updated,
		Counts: client.CreditImportCounts{MonthlyCredits: 2, RefillDates: 2, Readings: 2, RenewalDates: 3}, ImportedAt: importNow,
		Lineage: []uint64{1791795600},
	}
	raw, err := encodeRecord(valid)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := decodeRecord(raw); err != nil || !reflect.DeepEqual(got, valid) {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	empty := valid
	empty.LegacyUpdatedAt, empty.LegacyRevision = nil, 0
	if raw, _ := encodeRecord(empty); !bytes.Contains(raw, []byte(`"legacy_updated_at": null`)) || !bytes.Contains(raw, []byte(`"legacy_revision": "0"`)) {
		t.Fatalf("no rows: %s", raw)
	} else if got, err := decodeRecord(raw); err != nil || !reflect.DeepEqual(got, empty) {
		t.Fatalf("no rows: %+v, %v", got, err)
	}
	// The lineage: 1 to maxLineage revisions from 1, each past the one
	// before.
	switched := valid
	switched.Lineage = []uint64{1791795600, 1791795620, 1791795640}
	switchedRaw, _ := encodeRecord(switched)
	if !bytes.Contains(switchedRaw, []byte("\"lineage\": [\n    \"1791795600\",\n    \"1791795620\",\n    \"1791795640\"\n  ]")) {
		t.Fatalf("switched: %s", switchedRaw)
	} else if got, err := decodeRecord(switchedRaw); err != nil || !reflect.DeepEqual(got, switched) {
		t.Fatalf("switched: %+v, %v", got, err)
	}
	longest := valid
	for i := range maxLineage + 1 {
		longest.Lineage = append(longest.Lineage[:i:i], 1791795600+uint64(i))
		raw, _ := encodeRecord(longest)
		if _, err := decodeRecord(raw); (err == nil) != (i < maxLineage) {
			t.Fatalf("%d revisions: %v", i+1, err)
		}
	}
	for name, edit := range map[string]func(string) string{
		"no lineage": func(s string) string {
			return strings.Replace(s, ",\n  \"lineage\": [\n    \"1791795600\",\n    \"1791795620\",\n    \"1791795640\"\n  ]", "", 1)
		},
		"an empty lineage": func(s string) string {
			return strings.Replace(s, "[\n    \"1791795600\",\n    \"1791795620\",\n    \"1791795640\"\n  ]", "[]", 1)
		},
		"revision 0":                      func(s string) string { return strings.Replace(s, `"1791795600"`, `"0"`, 1) },
		"a revision below the one before": func(s string) string { return strings.Replace(s, `"1791795620"`, `"1791795599"`, 1) },
		"a revision repeated":             func(s string) string { return strings.Replace(s, `"1791795620"`, `"1791795600"`, 1) },
		"a padded revision":               func(s string) string { return strings.Replace(s, `"1791795620"`, `"01791795620"`, 1) },
		"a numeric revision":              func(s string) string { return strings.Replace(s, `"1791795620"`, `1791795620`, 1) },
		"a null lineage": func(s string) string {
			return strings.Replace(s, "[\n    \"1791795600\",\n    \"1791795620\",\n    \"1791795640\"\n  ]", "null", 1)
		},
	} {
		broken := edit(string(switchedRaw))
		if broken == string(switchedRaw) {
			t.Fatalf("%s: the edit changed nothing", name)
		}
		if _, err := decodeRecord([]byte(broken)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
	for name, edit := range map[string]func(string) string{
		"an unknown key":     func(s string) string { return strings.Replace(s, `"schema": 1,`, `"schema": 1, "note": "x",`, 1) },
		"schema 2":           func(s string) string { return strings.Replace(s, `"schema": 1`, `"schema": 2`, 1) },
		"no kind":            func(s string) string { return strings.Replace(s, `"kind": "automatic",`, ``, 1) },
		"another kind":       func(s string) string { return strings.Replace(s, `"automatic"`, `"copied"`, 1) },
		"a relative source":  func(s string) string { return strings.Replace(s, `"/CLIProxyAPI/`, `"CLIProxyAPI/`, 1) },
		"a short hash":       func(s string) string { return strings.Replace(s, strings.Repeat("a", 64), "abc", 1) },
		"an upper-case hash": func(s string) string { return strings.Replace(s, strings.Repeat("b", 64), strings.Repeat("B", 64), 1) },
		"a padded revision":  func(s string) string { return strings.Replace(s, `"12"`, `"012"`, 1) },
		"a numeric revision": func(s string) string { return strings.Replace(s, `"12"`, `12`, 1) },
		"imported_at with a frac": func(s string) string {
			return strings.Replace(s, `"2026-10-12T09:00:00Z"`, `"2026-10-12T09:00:00.5Z"`, 1)
		},
		"no imported_at": func(s string) string {
			return strings.Replace(s, ",\n  \"imported_at\": \"2026-10-12T09:00:00Z\"", "", 1)
		},
		"a negative count":         func(s string) string { return strings.Replace(s, `"readings": 2`, `"readings": -2`, 1) },
		"a missing count":          func(s string) string { return strings.Replace(s, `"readings": 2,`, ``, 1) },
		"an unknown count":         func(s string) string { return strings.Replace(s, `"readings": 2,`, `"readings": 2, "orgs": 5,`, 1) },
		"legacy_updated_at a date": func(s string) string { return strings.Replace(s, `"2026-10-10T18:20:00Z"`, `"2026-10-10"`, 1) },
		"trailing data":            func(s string) string { return s + "{}" },
		"over 4 KiB":               func(s string) string { return s + strings.Repeat(" ", maxRecordBytes) },
	} {
		broken := edit(string(raw))
		if broken == string(raw) {
			t.Fatalf("%s: the edit changed nothing", name)
		}
		if _, err := decodeRecord([]byte(broken)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}

	// An unreadable record ends follow: a newer legacy file is not copied
	// over the settings file it would describe.
	legacy := legacyFixture(t, "five-orgs")
	e := newImportEnv(t)
	place(t, e.sibling, legacy)
	s := Open(e.snapshot)
	s.Sync(importNow)
	if err := os.WriteFile(s.RecordPath(), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	place(t, e.sibling, atRevision(t, legacy, 13, "300"))
	if result := s.Sync(importNow.Add(10 * time.Second)); result.Imported != nil || !bytes.Equal(readFile(t, s.Path()), legacy) {
		t.Fatalf("followed with an unreadable record: %+v", result)
	}
	if imp := s.Status().Import; imp.State != client.CreditImportStateNotImported || imp.Following || imp.Kind != nil {
		t.Fatalf("import %+v", imp)
	}
}

// An import record that cannot be read counts as none, and is the one
// operator file that is written over: with the settings file still a byte
// copy of Quota Glance's, the next tick writes a matched record in its place,
// as it would where there was no record. The settings file is not touched.
func TestAnUnreadableRecordIsReplacedWhenTheFilesMatch(t *testing.T) {
	legacy := legacyFixture(t, "five-orgs")
	e := newImportEnv(t)
	place(t, e.sibling, legacy)
	s := Open(e.snapshot)
	if result := s.Sync(importNow); result.Imported == nil || recordAt(t, e.snapshot).Kind != client.CreditImportKindAutomatic {
		t.Fatalf("the first import: %+v", result)
	}
	place(t, s.RecordPath(), []byte(`{"schema":1,"kind":"automatic","note":"hand edited"}`))
	later := importNow.Add(10 * time.Second)
	if result := s.Sync(later); result.Imported != nil || !bytes.Equal(readFile(t, s.Path()), legacy) {
		t.Fatalf("imported again: %+v", result)
	}
	if r := recordAt(t, e.snapshot); r.Kind != client.CreditImportKindMatched || !r.ImportedAt.Equal(later) || r.SHA256 != hashOf(legacy) {
		t.Fatalf("record %+v", r)
	}
	if imp := s.Status().Import; imp.State != client.CreditImportStateImported || !imp.Following {
		t.Fatalf("import %+v", imp)
	}
}
