package source

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/aggregate"
)

// The legacy file's body in the tests: quota-glance 0.7's settings.json, with
// nothing stored but its revision.
const legacyBody = `{"schema":1,"revision":12,"apiCredits":{},"renewals":{}}`

// layout is the documented Docker layout under a temporary root: Quota
// Cache's snapshot in plugins/data/quota-cache, and quota-glance's data-dir
// beside it, which is one of the places Quota Cache looks for settings.json.
type layout struct {
	cachePath, dataDir, dropIn string
}

func newLayout(t *testing.T) layout {
	t.Helper()
	root := filepath.Join(t.TempDir(), "plugins", "data")
	l := layout{
		cachePath: filepath.Join(root, "quota-cache", "snapshot.json"),
		dataDir:   filepath.Join(root, "quota-glance"),
	}
	l.dropIn = filepath.Join(filepath.Dir(l.cachePath), qc.LegacySettingsDropIn)
	for _, dir := range []string{filepath.Dir(l.cachePath), l.dataDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

func (l layout) legacyPath() string { return filepath.Join(l.dataDir, LegacySettingsFile) }

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadLegacySettings(t *testing.T) {
	l := newLayout(t)
	if got := ReadLegacySettings(l.dataDir); got.Present || got.Unreadable || got.SHA256 != "" || got.Path != l.legacyPath() {
		t.Fatalf("no file: %+v", got)
	}
	writeFile(t, l.legacyPath(), legacyBody)
	if got := ReadLegacySettings(l.dataDir); !got.Present || got.Unreadable || got.Revision != 12 || got.SHA256 != digestOf([]byte(legacyBody)) {
		t.Fatalf("a readable file: %+v", got)
	}
	// Bytes it can hash and cannot decode are still hashed: they are what an
	// import would compare.
	writeFile(t, l.legacyPath(), `{"revision":`)
	if got := ReadLegacySettings(l.dataDir); !got.Present || !got.Unreadable || got.SHA256 == "" {
		t.Fatalf("not JSON: %+v", got)
	}
	writeFile(t, l.legacyPath(), `{"revision":1,"pad":"`+string(bytes.Repeat([]byte("a"), maxLegacyBytes))+`"}`)
	if got := ReadLegacySettings(l.dataDir); !got.Present || !got.Unreadable || got.SHA256 != "" {
		t.Fatalf("over 1 MiB: %+v", got)
	}
	if err := os.Remove(l.legacyPath()); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(l.dataDir, "elsewhere.json")
	writeFile(t, other, legacyBody)
	if err := os.Symlink(other, l.legacyPath()); err != nil {
		t.Fatal(err)
	}
	if got := ReadLegacySettings(l.dataDir); !got.Present || !got.Unreadable || got.SHA256 != "" {
		t.Fatalf("a symlink: %+v", got)
	}
}

// A FIFO where settings.json belongs is never opened.
func TestAnUnopenableLegacyFileDoesNotBlock(t *testing.T) {
	l := newLayout(t)
	if out, err := exec.Command("mkfifo", l.legacyPath()).CombinedOutput(); err != nil {
		t.Skipf("mkfifo unavailable: %v %s", err, out)
	}
	done := make(chan LegacySettings, 1)
	go func() { done <- ReadLegacySettings(l.dataDir) }()
	select {
	case got := <-done:
		if !got.Present || !got.Unreadable {
			t.Fatalf("%+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadLegacySettings blocked on a FIFO")
	}
}

func ptr[T any](value T) *T { return &value }

// stateWith is a published state whose settings.import is imported.
func stateWith(imported *qc.CreditImport) *qc.CreditState {
	return &qc.CreditState{Schema: qc.CreditsSchema, Settings: qc.CreditSettingsState{State: qc.CreditSettingsOK, Import: imported},
		Renewals: map[string]qc.RenewalSetting{}}
}

// The migration note with a published state, case by case, and
// quota-glance never writing the file it reads.
func TestMigrationNote(t *testing.T) {
	l := newLayout(t)
	if got := Migration(ReadLegacySettings(l.dataDir), stateWith(nil), "", l.cachePath); got != "" {
		t.Errorf("no legacy file: %q", got)
	}
	writeFile(t, l.legacyPath(), legacyBody)
	old := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(l.legacyPath(), old, old); err != nil {
		t.Fatal(err)
	}
	ours := digestOf([]byte(legacyBody))
	other := digestOf([]byte(`{"schema":1,"revision":3}`))
	stale := filepath.Join(filepath.Dir(l.dataDir), "old-quota-glance", LegacySettingsFile)

	for _, tc := range []struct {
		name     string
		imported *qc.CreditImport
		want     string
	}{
		{"no import record", nil, aggregate.MigrationNotImported},
		{"these bytes, from this file", &qc.CreditImport{State: qc.CreditImportStateImported, SourcePath: ptr(l.legacyPath()), SHA256: ptr(ours)}, ""},
		{"these bytes, from a drop-in", &qc.CreditImport{State: qc.CreditImportStateImported, SourcePath: ptr(l.dropIn), SHA256: ptr(ours)}, ""},
		{"Quota Cache's notImported", &qc.CreditImport{State: qc.CreditImportStateNotImported, Importable: true}, aggregate.MigrationNotImported},
		{"Quota Cache's settingsMissing, other bytes", &qc.CreditImport{State: qc.CreditImportStateSettingsMissing,
			SourcePath: ptr(l.legacyPath()), SHA256: ptr(other)}, aggregate.MigrationNotImported},
		{"imported from a data-dir since moved", &qc.CreditImport{State: qc.CreditImportStateImported, SourcePath: ptr(stale), SHA256: ptr(other)},
			aggregate.MigrationNotImported},
		{"imported from an old drop-in", &qc.CreditImport{State: qc.CreditImportStateImported, SourcePath: ptr(l.dropIn), SHA256: ptr(other)},
			aggregate.MigrationNotImported},
		{"this file, changed since", &qc.CreditImport{State: qc.CreditImportStateChangedSinceImport, SourcePath: ptr(l.legacyPath()),
			SHA256: ptr(other)}, aggregate.MigrationChangedSinceImport},
		{"this file, unreadable to Quota Cache", &qc.CreditImport{State: qc.CreditImportStateLegacyUnreadable,
			SourcePath: ptr(l.legacyPath())}, aggregate.MigrationChangedSinceImport},
		{"another file, changed since", &qc.CreditImport{State: qc.CreditImportStateChangedSinceImport, SourcePath: ptr(stale),
			SHA256: ptr(other)}, aggregate.MigrationNotImported},
	} {
		if got := Migration(ReadLegacySettings(l.dataDir), stateWith(tc.imported), "", l.cachePath); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}

	info, err := os.Stat(l.legacyPath())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(l.legacyPath())
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(old) || string(raw) != legacyBody {
		t.Fatalf("settings.json was written: modified %s, %s", info.ModTime(), raw)
	}
}

// The migration note before Quota Cache publishes anything says what its
// first start will do: import this file when it is the first candidate that
// exists, or byte for byte the same as it, and it can be read; otherwise not.
// An unreadable or abandoned state says nothing about imports.
func TestMigrationBeforeQuotaCachePublishes(t *testing.T) {
	note := func(l layout) string {
		t.Helper()
		return Migration(ReadLegacySettings(l.dataDir), nil, aggregate.CreditsMissing, l.cachePath)
	}
	l := newLayout(t)
	if got := note(l); got != "" {
		t.Errorf("no legacy file: %q", got)
	}
	writeFile(t, l.legacyPath(), legacyBody)
	if got := note(l); got != aggregate.MigrationAwaitingImport {
		t.Errorf("this file is the first candidate: %q", got)
	}
	for _, cause := range []string{aggregate.CreditsUnreadable, aggregate.CreditsAbandoned} {
		if got := Migration(ReadLegacySettings(l.dataDir), stateWith(nil), cause, l.cachePath); got != "" {
			t.Errorf("%s: %q", cause, got)
		}
	}
	// A drop-in beside the snapshot comes first.
	writeFile(t, l.dropIn, legacyBody)
	if got := note(l); got != aggregate.MigrationAwaitingImport {
		t.Errorf("a byte-equal drop-in: %q", got)
	}
	writeFile(t, l.dropIn, `{"schema":1,"revision":3,"apiCredits":{},"renewals":{}}`)
	if got := note(l); got != aggregate.MigrationNotImported {
		t.Errorf("a different drop-in: %q", got)
	}

	// A data-dir Quota Cache does not look in, and nothing where it does.
	moved := newLayout(t)
	moved.dataDir = filepath.Join(filepath.Dir(moved.dataDir), "custom-glance")
	if err := os.MkdirAll(moved.dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, moved.legacyPath(), legacyBody)
	if got := note(moved); got != aggregate.MigrationNotImported {
		t.Errorf("a data-dir Quota Cache cannot find: %q", got)
	}
	// And the default data-dir beside Quota Cache's holding other settings.
	writeFile(t, filepath.Join(filepath.Dir(moved.dataDir), "quota-glance", LegacySettingsFile), `{"schema":1,"revision":1}`)
	if got := note(moved); got != aggregate.MigrationNotImported {
		t.Errorf("other settings in the default data-dir: %q", got)
	}

	// This file as the first candidate, in each form it cannot be read in:
	// Quota Cache's first start imports nothing from it, so nothing is
	// awaited.
	for _, tc := range []struct {
		name  string
		place func(l layout)
	}{
		{"not JSON", func(l layout) { writeFile(t, l.legacyPath(), `{"revision":`) }},
		{"over 1 MiB", func(l layout) {
			writeFile(t, l.legacyPath(), `{"revision":1,"pad":"`+string(bytes.Repeat([]byte("a"), maxLegacyBytes))+`"}`)
		}},
		{"a symlink", func(l layout) {
			other := filepath.Join(l.dataDir, "elsewhere.json")
			writeFile(t, other, legacyBody)
			if err := os.Symlink(other, l.legacyPath()); err != nil {
				t.Fatal(err)
			}
		}},
		{"not JSON, with a byte-equal drop-in", func(l layout) {
			writeFile(t, l.legacyPath(), `{"revision":`)
			writeFile(t, l.dropIn, `{"revision":`)
		}},
	} {
		l := newLayout(t)
		tc.place(l)
		if legacy := ReadLegacySettings(l.dataDir); !legacy.Unreadable {
			t.Fatalf("%s: %+v is readable", tc.name, legacy)
		}
		if got := note(l); got != aggregate.MigrationNotImported {
			t.Errorf("%s: %q, want %q", tc.name, got, aggregate.MigrationNotImported)
		}
	}
}
