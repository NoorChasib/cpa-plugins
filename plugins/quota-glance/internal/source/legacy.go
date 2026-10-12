package source

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/aggregate"
)

// LegacySettingsFile is the file in data-dir where quota-glance 0.7 and older
// kept the API credit amounts, refill dates, Console readings and Claude
// renewal dates entered on its dashboard. Quota Cache keeps them now, and
// imports this file. quota-glance only reads it, to tell whether that import
// has happened, and never writes, moves or deletes it.
const LegacySettingsFile = "settings.json"

// maxLegacyBytes is the most of a settings file either plugin reads, the
// store's own limit.
const maxLegacyBytes = 1 << 20

// LegacySettings is quota-glance's own settings.json as one rebuild found it.
type LegacySettings struct {
	Path string
	// Present is true when something is at Path.
	Present bool
	// Unreadable is true when what is there is not a regular file of at most
	// 1 MiB whose revision can be read.
	Unreadable bool
	Revision   uint64
	// SHA256 is the hex digest of the file's bytes, "" when they could not be
	// read.
	SHA256 string
	// info identifies the file, so another path can be told to be the same
	// one.
	info os.FileInfo
}

// ReadLegacySettings looks at settings.json in dataDir. Only a regular file is
// opened, so a FIFO there cannot wedge the watcher goroutine this runs on.
func ReadLegacySettings(dataDir string) LegacySettings {
	legacy := LegacySettings{Path: filepath.Join(dataDir, LegacySettingsFile)}
	info, err := os.Lstat(legacy.Path)
	if err != nil {
		return legacy
	}
	legacy.Present, legacy.info = true, info
	raw, ok := readRegular(legacy.Path, info)
	if !ok {
		legacy.Unreadable = true
		return legacy
	}
	legacy.SHA256 = digestOf(raw)
	var file struct {
		Revision uint64 `json:"revision"`
	}
	if json.Unmarshal(raw, &file) != nil {
		legacy.Unreadable = true
		return legacy
	}
	legacy.Revision = file.Revision
	return legacy
}

// sameFile reports whether path names the legacy file itself.
func (l LegacySettings) sameFile(path string) bool {
	if l.info == nil {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && os.SameFile(info, l.info)
}

// readRegular reads the file at path, which info describes, when it is a
// regular file of at most maxLegacyBytes.
func readRegular(path string, info os.FileInfo) ([]byte, bool) {
	if !info.Mode().IsRegular() || info.Size() > maxLegacyBytes {
		return nil, false
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxLegacyBytes+1))
	if err != nil || len(raw) > maxLegacyBytes {
		return nil, false
	}
	return raw, true
}

func digestOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Migration is the API credit card's editing.migration: whether the values
// saved in quota-glance's own settings.json have reached Quota Cache. It is
// worked out here, on every rebuild, because Quota Cache cannot learn
// quota-glance's data-dir: each plugin receives only its own configuration.
//
// With a published state, I being its settings.import:
//
//  1. no legacy file: "", there is nothing to import;
//  2. I's sha256 is the file's: "", these exact bytes were imported, from
//     whichever path, and nothing writes the file any more;
//  3. I's source_path is this file, and Quota Cache says it changed since the
//     import or cannot read it: changedSinceImport;
//  4. otherwise notImported. That covers no import at all, and an import of a
//     different file, such as an old drop-in or a quota-glance data-dir that
//     has since moved: either way these values are not in Quota Cache.
//
// Without one, from a Quota Cache older than 0.2.0, the note is about what
// its first start will do, through the same candidates it searches
// (qc.LegacySettingsCandidates): awaitingImport when the first of them that
// exists is this file, or holds the same bytes, and notImported otherwise.
// This file being unreadable makes it notImported whatever the candidates:
// that first start imports nothing from a file it cannot read, or from a copy
// of its bytes. An unreadable or abandoned state says nothing about imports,
// which may already have happened, so its note is "".
func Migration(legacy LegacySettings, state *qc.CreditState, creditsError, cachePath string) string {
	switch {
	case !legacy.Present:
		return ""
	case creditsError == "" && state != nil:
		return migrationOf(legacy, state.Settings.Import)
	case creditsError == aggregate.CreditsUnreadable, creditsError == aggregate.CreditsAbandoned:
		return ""
	}
	return awaitingOf(legacy, cachePath)
}

// migrationOf is Migration with a published state; imported is its
// settings.import.
func migrationOf(legacy LegacySettings, imported *qc.CreditImport) string {
	switch {
	case imported == nil:
		return aggregate.MigrationNotImported
	case imported.SHA256 != nil && legacy.SHA256 != "" && *imported.SHA256 == legacy.SHA256:
		return ""
	case imported.SourcePath != nil && legacy.sameFile(*imported.SourcePath) &&
		(imported.State == qc.CreditImportStateChangedSinceImport || imported.State == qc.CreditImportStateLegacyUnreadable):
		return aggregate.MigrationChangedSinceImport
	}
	return aggregate.MigrationNotImported
}

// awaitingOf is Migration before Quota Cache publishes: whether the file its
// first start would import is this one, and can be. A candidate exists when
// Lstat finds it, as Quota Cache counts it. Only a candidate that is not this
// file is read, to compare its bytes.
func awaitingOf(legacy LegacySettings, cachePath string) string {
	// Quota Cache reads under stricter rules than the revision decoded here,
	// so it cannot read this file either, and imports nothing until the file
	// is fixed or moved aside.
	if legacy.Unreadable {
		return aggregate.MigrationNotImported
	}
	for _, candidate := range qc.LegacySettingsCandidates(cachePath) {
		info, err := os.Lstat(candidate)
		if err != nil {
			continue
		}
		if legacy.sameFile(candidate) {
			return aggregate.MigrationAwaitingImport
		}
		if raw, ok := readRegular(candidate, info); ok && legacy.SHA256 != "" && digestOf(raw) == legacy.SHA256 {
			return aggregate.MigrationAwaitingImport
		}
		return aggregate.MigrationNotImported
	}
	return aggregate.MigrationNotImported
}
