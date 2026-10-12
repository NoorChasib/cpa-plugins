package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// The import from Quota Glance's settings.json, the legacy file.
//
// Quota Glance 0.7 and older kept these values in <data-dir>/settings.json,
// in exactly this package's schema. The legacy file is found by convention
// (client.LegacySettingsCandidates) and is only ever read: nothing here
// creates, writes, renames, chmods or deletes it. The automatic import copies
// its bytes verbatim, so every revision, rev, updatedAt and reading baseline
// survives bit for bit, and so does a follow while the record says the
// settings file is a byte copy of the legacy file, no later revision was given
// out, and the legacy file goes on from that copy. Import Quota Glance's
// settings, and a follow otherwise, give every row a revision past both
// files' instead (raised). A rollback can copy any of these settings files
// back over the legacy file.
//
// Each import writes the settings file first and then the import record,
// which says what was imported. A record that could not be written is written
// before anything is judged against it again (writePending). After a crash
// between the two, the next Sync completes a verbatim copy, which left the
// files equal (matched), and a raised one, which left the settings file
// holding the legacy file's rows, each at the settings file's revision
// (raisedCopy). The record also keeps the settings file's lineage:
// the revisions from which files hold what was imported, and those from which
// they do not (holdsImport). A file from before the import, or one a save
// started after the imported file went missing, holds none of it and is never
// reported as the import, whichever file is put back later. A save that
// crosses from one to the other writes the record first (saveRevision); a
// tick never writes it for that.

// RecordPath is where the import record lives for the snapshot at
// snapshotPath: the same path with a final ".json" replaced by
// ".settings-import.json", else with it appended. Like the settings file, it
// is operator data and must move with the snapshot.
func RecordPath(snapshotPath string) string {
	return strings.TrimSuffix(snapshotPath, ".json") + ".settings-import.json"
}

// The import record's limits. maxLineage is even, so dropping the oldest pair
// keeps the first revision one from which files hold the import.
const (
	recordSchema   = 1
	maxRecordBytes = 4 << 10
	maxLineage     = 16
)

// ErrNotImportable: the legacy file is missing or cannot be read, so there is
// nothing to import. Answered 409 not_editable.
var ErrNotImportable = errors.New("not_editable")

// Imported is an import that changed the settings file, for the log line.
type Imported struct {
	// Kind is client.CreditImportKindAutomatic, …Follow or …Manual. A matched
	// record changes no value and is never reported.
	Kind string
	// Counts count what the legacy file held, as the record does.
	Counts client.CreditImportCounts
	// Revision is the settings file's revision after the import.
	Revision uint64
}

// SyncResult is what one Sync did.
type SyncResult struct {
	// Reloaded is set when the settings file was read again: it had changed
	// on disk or gone, or the last read of it did not settle.
	Reloaded bool
	// Imported is the import Sync wrote, nil when there was none.
	Imported *Imported
	// LegacyUnreadable is set while there is neither a settings file nor an
	// import record and the legacy file cannot be read: nothing was imported,
	// nothing stored applies, and nothing may be saved until it is fixed or
	// moved aside. Configure logs it once.
	LegacyUnreadable bool
}

// record is the import record: what the last import read and wrote.
type record struct {
	// Kind is a client.CreditImportKind value. A matched or follow record
	// written while nothing was saved here since an automatic or manual
	// import keeps that import's kind (continued). One that completes a
	// raised copy (raisedCopy) is manual, or automatic when the record it
	// replaces is.
	Kind       string
	SourcePath string
	// SHA256 is the hex SHA-256 of the legacy bytes imported, and
	// SettingsSHA256 that of the settings file then written. They differ when
	// the import raised the revisions past the legacy file's: a manual
	// import, and a follow that raised them.
	SHA256          string
	SettingsSHA256  string
	LegacyRevision  uint64
	LegacyUpdatedAt *time.Time
	Counts          client.CreditImportCounts
	ImportedAt      time.Time
	// Lineage is where the settings file's revisions switch, ascending,
	// between files that hold what was imported and files that do not
	// (holds): from the first, saves over the file the import wrote; from
	// the second, a file a save started after that one went missing; from
	// the third, saves over a file that holds the import put back; and so
	// on. Each import writes the first, past every revision the store has
	// seen and at least the time, so a file from before the import is below
	// it. A save that crosses writes the next first (saveRevision).
	Lineage []uint64
}

// recordFile is the import record as the store last read or wrote it.
type recordFile struct {
	stamp fileStamp
	// settled is set when what follows is the file's as stamped: there is
	// none, or it was read; not after a failure to open or read it, which a
	// later read may get past.
	settled bool
	// rec is nil when there is no record or it cannot be read.
	rec *record
}

// pendingRecord is the record of an import that wrote the settings file but
// could not write the record, with the stamps of the settings file it wrote
// and of the import record it was to replace.
type pendingRecord struct {
	rec            record
	file, replaces fileStamp
}

// legacyFile is the legacy file as the store last read it.
type legacyFile struct {
	// path is "" when there is none to read.
	path  string
	stamp fileStamp
	// exists is set when Lstat succeeded on path.
	exists bool
	// settled is set when what follows is the file's as stamped, so it
	// stands until the stamp changes; not after a failure to open or read it.
	settled bool
	// ok is set when it decodes under the settings rules; raw, sha and values
	// are then its bytes, their hash and what they hold.
	ok     bool
	raw    []byte
	sha    string
	values Values
}

// The record as JSON, keys in the order they are written.
type recordDoc struct {
	Schema          *int       `json:"schema"`
	Kind            *string    `json:"kind"`
	SourcePath      *string    `json:"source_path"`
	SHA256          *string    `json:"sha256"`
	SettingsSHA256  *string    `json:"settings_sha256"`
	LegacyRevision  *string    `json:"legacy_revision"`
	LegacyUpdatedAt *string    `json:"legacy_updated_at"`
	Counts          *countsDoc `json:"counts"`
	ImportedAt      *string    `json:"imported_at"`
	Lineage         []string   `json:"lineage"`
}

type countsDoc struct {
	MonthlyCredits *int `json:"monthly_credits"`
	RefillDates    *int `json:"refill_dates"`
	Readings       *int `json:"readings"`
	RenewalDates   *int `json:"renewal_dates"`
}

var (
	sha256Shape   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	revisionDigit = regexp.MustCompile(`^(0|[1-9][0-9]{0,19})$`)
)

// decodeRecord reads an import record: at most maxRecordBytes, no unknown key,
// no trailing data, schema 1, every key present, and each value in the form
// encodeRecord writes. legacy_updated_at may be null; lineage is 1 to
// maxLineage revisions from 1, ascending.
func decodeRecord(raw []byte) (record, error) {
	if len(raw) > maxRecordBytes {
		return record{}, errShape
	}
	var doc recordDoc
	if strictDecode(raw, &doc) != nil {
		return record{}, errShape
	}
	if doc.Schema == nil || *doc.Schema != recordSchema || doc.Kind == nil || doc.SourcePath == nil || doc.SHA256 == nil ||
		doc.SettingsSHA256 == nil || doc.LegacyRevision == nil || doc.Counts == nil || doc.ImportedAt == nil {
		return record{}, errShape
	}
	switch *doc.Kind {
	case client.CreditImportKindAutomatic, client.CreditImportKindMatched, client.CreditImportKindFollow, client.CreditImportKindManual:
	default:
		return record{}, errShape
	}
	if !filepath.IsAbs(*doc.SourcePath) || !sha256Shape.MatchString(*doc.SHA256) || !sha256Shape.MatchString(*doc.SettingsSHA256) ||
		!revisionDigit.MatchString(*doc.LegacyRevision) {
		return record{}, errShape
	}
	revision, err := strconv.ParseUint(*doc.LegacyRevision, 10, 64)
	if err != nil {
		return record{}, errShape
	}
	imported, ok := parseStamp(*doc.ImportedAt)
	if !ok {
		return record{}, errShape
	}
	c := doc.Counts
	if c.MonthlyCredits == nil || c.RefillDates == nil || c.Readings == nil || c.RenewalDates == nil ||
		*c.MonthlyCredits < 0 || *c.RefillDates < 0 || *c.Readings < 0 || *c.RenewalDates < 0 {
		return record{}, errShape
	}
	r := record{
		Kind: *doc.Kind, SourcePath: *doc.SourcePath, SHA256: *doc.SHA256, SettingsSHA256: *doc.SettingsSHA256,
		LegacyRevision: revision, ImportedAt: imported,
		Counts: client.CreditImportCounts{MonthlyCredits: *c.MonthlyCredits, RefillDates: *c.RefillDates, Readings: *c.Readings, RenewalDates: *c.RenewalDates},
	}
	if doc.LegacyUpdatedAt != nil {
		updated, ok := parseStamp(*doc.LegacyUpdatedAt)
		if !ok {
			return record{}, errShape
		}
		r.LegacyUpdatedAt = &updated
	}
	if len(doc.Lineage) == 0 || len(doc.Lineage) > maxLineage {
		return record{}, errShape
	}
	for _, text := range doc.Lineage {
		if !revisionDigit.MatchString(text) {
			return record{}, errShape
		}
		revision, err := strconv.ParseUint(text, 10, 64)
		if err != nil || revision == 0 || (len(r.Lineage) > 0 && revision <= r.Lineage[len(r.Lineage)-1]) {
			return record{}, errShape
		}
		r.Lineage = append(r.Lineage, revision)
	}
	return r, nil
}

func encodeRecord(r record) ([]byte, error) {
	schema, revision, imported := recordSchema, strconv.FormatUint(r.LegacyRevision, 10), stampOf(r.ImportedAt)
	doc := recordDoc{
		Schema: &schema, Kind: &r.Kind, SourcePath: &r.SourcePath, SHA256: &r.SHA256, SettingsSHA256: &r.SettingsSHA256,
		LegacyRevision: &revision, ImportedAt: &imported,
		Counts: &countsDoc{MonthlyCredits: &r.Counts.MonthlyCredits, RefillDates: &r.Counts.RefillDates, Readings: &r.Counts.Readings, RenewalDates: &r.Counts.RenewalDates},
	}
	if r.LegacyUpdatedAt != nil {
		updated := stampOf(*r.LegacyUpdatedAt)
		doc.LegacyUpdatedAt = &updated
	}
	doc.Lineage = make([]string, 0, len(r.Lineage))
	for _, revision := range r.Lineage {
		doc.Lineage = append(doc.Lineage, strconv.FormatUint(revision, 10))
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// readRecord reads the import record with its stamp.
func (s *Store) readRecord() recordFile {
	stamp := stampOfFile(s.recordPath)
	raw, settled, ok := s.read(s.recordPath, stamp, maxRecordBytes)
	f := recordFile{stamp: stamp, settled: settled}
	if !ok {
		return f
	}
	if r, err := decodeRecord(raw); err == nil {
		f.rec = &r
	}
	return f
}

// writeRecord commits r as the import record, which replaces a pending one.
// On a failure the record on disk, and the store's idea of it, stay as they
// were.
func (s *Store) writeRecord(r record) error {
	raw, err := encodeRecord(r)
	if err != nil {
		return ErrUnwritable
	}
	if err := commitFile(s.recordPath, raw, func(raw []byte) error {
		_, err := decodeRecord(raw)
		return err
	}, s.write, s.syncDir); err != nil {
		return err
	}
	s.record = recordFile{stamp: stampOfFile(s.recordPath), settled: true, rec: &r}
	s.pending = nil
	return nil
}

// recordImport writes r, the record of an import that has just written the
// settings file. One that cannot be written is kept pending: left to the files
// alone, a save here or Quota Glance's next save could come before the next
// tick, after which nothing would show that the settings file holds the
// import. The caller holds s.mu.
func (s *Store) recordImport(r record) {
	if s.writeRecord(r) != nil {
		s.pending = &pendingRecord{rec: r, file: s.file.stamp, replaces: s.record.stamp}
	}
}

// writePending writes the pending record while the settings file is still the
// one its import wrote and the import record the one it was to replace, and
// drops it once either changed: the files then say what holds. ErrUnwritable
// when it cannot be written, which keeps it. While the record cannot be
// opened or read, it waits for a read to settle. The caller holds s.mu and
// has read the settings file and the record again.
func (s *Store) writePending() error {
	p := s.pending
	switch {
	case p == nil || s.recordUnread():
		return nil
	case !s.file.stamp.same(p.file) || !s.record.stamp.same(p.replaces):
		s.pending = nil
		return nil
	}
	return s.writeRecord(p.rec)
}

// legacyPath is the legacy file, in this order: the drop-in when it is there,
// since placing it is an explicit act; else the readable record's file,
// whether it is still there or not, so the legacy file never silently
// switches to another candidate; else the first other candidate that is
// there. "" when there is none. A candidate is there when Lstat succeeds on
// it. The file is identified by its path, never by its inode: Quota Glance
// saves by rename. stamp is the file's Lstat.
func (s *Store) legacyPath(rec *record) (string, fileStamp) {
	if len(s.candidates) > 0 {
		if stamp := stampOfFile(s.candidates[0]); stamp.info != nil {
			return s.candidates[0], stamp
		}
	}
	if rec != nil {
		return rec.SourcePath, stampOfFile(rec.SourcePath)
	}
	for _, path := range s.candidates[min(1, len(s.candidates)):] {
		if stamp := stampOfFile(path); stamp.info != nil {
			return path, stamp
		}
	}
	return "", fileStamp{}
}

// readLegacy reads the legacy file at path, whose Lstat gave stamp, under the
// settings rules. While the stamp is the one the last settled read saw, the
// file is not read again: its bytes, their hash and what they hold still
// stand, so an unchanged file costs a tick its Lstat alone, never a read and
// a hash of up to MaxBytes. The caller holds s.mu.
func (s *Store) readLegacy(path string, stamp fileStamp) legacyFile {
	if path == "" {
		return legacyFile{}
	}
	if last := s.legacy; last.settled && last.path == path && last.stamp.same(stamp) {
		return last
	}
	f := legacyFile{path: path, stamp: stamp, exists: stamp.info != nil}
	raw, settled, ok := s.read(path, stamp, MaxBytes)
	f.settled = settled
	if !ok {
		return f
	}
	v, err := decode(raw)
	if err != nil {
		return f
	}
	f.ok, f.raw, f.sha, f.values = true, raw, hashOf(raw), v
	return f
}

// recordUnread reports whether the import record is there but its last read
// could not open or read it, which a later read may get past. The caller
// holds s.mu.
func (s *Store) recordUnread() bool {
	return s.record.stamp.exists && !s.record.settled
}

// observe reads the import record and the legacy file, and judges whether
// the legacy file blocks the first import. The caller holds s.mu.
func (s *Store) observe() {
	s.record = s.readRecord()
	s.legacy = s.readLegacy(s.legacyPath(s.record.rec))
	s.blocked = !s.file.stamp.exists && !s.record.stamp.exists && s.legacy.exists && !s.legacy.ok
}

// Sync brings the store in line with its files, once per publisher tick and
// at every configure:
//
//   - The settings file is read again when it changed on disk or is gone:
//     moving an unreadable file aside makes the store empty and editable once
//     an import record exists, and is a first start (below) while none does,
//     and putting a good copy back makes it apply, with no configure. One that
//     could not be opened or read is read again at every tick until a read
//     settles, so fixing its owner or permissions is enough.
//   - The first import: while there is neither a settings file nor an import
//     record (one that cannot be read counts), a legacy file that decodes is
//     copied verbatim, then recorded as automatic. One that cannot be read
//     blocks: nothing applies and nothing may be saved until it is fixed or
//     moved aside, so a first save cannot shut the import out for good.
//   - The record of an import, this one or an earlier one, that could not be
//     written is written while neither file changed since (writePending).
//   - Matching files complete an import: when the settings file and the
//     legacy file hold the same bytes, and there is no import record, or one
//     that fails its rules or does not say the one is a copy of the other, a
//     matched record is written (continued), unless an import's record still
//     waits to be written. That finishes an import a crash interrupted
//     between its two writes, and re-arms follow after a rollback copied the
//     settings file over Quota Glance's, after a manual import too, whose
//     record says its file was raised past Quota Glance's. A record that
//     cannot be opened or read is left as it is, as a save leaves it, until a
//     read settles: what it says is not known.
//   - A raised copy completes an import too: when the settings file holds
//     exactly the legacy file's rows, each at the settings file's revision,
//     which is past the legacy file's and the record's, and the record names
//     the legacy file but not the settings file (raisedCopy), the record is
//     written, unless one still waits to be written. That finishes Import Quota Glance's settings, or a follow that
//     raised the revisions, after a crash or a restart between its two
//     writes. The files cannot tell the one from the other, and a follow
//     keeps the kind of the import it continues, so the record keeps an
//     automatic import's kind and is otherwise manual: either note says what
//     may need entering again.
//   - Follow: while nothing was saved here since the import (the settings
//     file's hash is the record's) and the legacy file is the record's, a
//     legacy file that changed, decodes, and has a higher revision than the
//     record's is copied, verbatim unless a page may hold one of its
//     revisions for another value (follow), then recorded as follow
//     (continued). The first save here ends it.
//
// Then the import status is worked out again (Status). After Close, Sync does
// nothing.
func (s *Store) Sync(now time.Time) SyncResult {
	now = now.UTC().Truncate(time.Second)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return SyncResult{}
	}
	var result SyncResult
	if s.fileChanged() {
		s.setFile(s.readFile())
		result.Reloaded = true
	}
	s.observe()
	result.LegacyUnreadable = s.blocked

	if !s.file.stamp.exists && !s.record.stamp.exists && s.legacy.ok {
		result.Imported = s.copyLegacy(client.CreditImportKindAutomatic, now)
	}
	_ = s.writePending()
	if rec := s.record.rec; s.pending == nil && !s.recordUnread() && s.file.sha != "" && s.legacy.ok && s.file.sha == s.legacy.sha && (rec == nil || rec.SettingsSHA256 != s.file.sha || rec.SHA256 != s.file.sha) {
		_ = s.writeRecord(s.recordOf(s.continued(client.CreditImportKindMatched), s.legacy.sha, now))
	}
	if rec := s.record.rec; s.pending == nil && !s.recordUnread() && s.raisedCopy() {
		kind := client.CreditImportKindManual
		if rec.Kind == client.CreditImportKindAutomatic {
			kind = rec.Kind
		}
		_ = s.writeRecord(s.recordOf(kind, s.file.sha, now))
	}
	if rec := s.record.rec; s.untouched() && s.legacy.ok && s.legacy.path == rec.SourcePath && !s.legacy.stamp.same(s.followSeen) {
		if s.legacy.values.Revision <= rec.LegacyRevision {
			// An older copy put back is reported as changed, never imported.
			s.followSeen = s.legacy.stamp
		} else if imported := s.follow(now); imported != nil {
			s.followSeen = s.legacy.stamp
			result.Imported = imported
		}
	}
	s.publish()
	return result
}

// follow copies a newer legacy file while the store is untouched, then
// records it as follow, or with the kind of the import it continues
// (continued); the result reports a follow either way. While the settings
// file is a byte copy of the legacy file the record names, which the record
// says by its two equal hashes, no revision past that file's was ever given
// out, and the legacy file goes on from that copy (continuesCopy), the copy is
// verbatim. Otherwise it is not: after a manual import, whose revisions were
// raised past both files', or after saves here and the imported file put
// back, a page may hold revisions past the legacy file's, while Quota
// Glance's file goes on from its own lower ones; and a legacy file that does
// not go on from the copy may give a row a revision the copy gave it, for
// another value. Copied verbatim, a row's rev could equal a revision a page
// holds for it, so that page's save would overwrite the copy without a
// conflict. The values are raised as the manual import raised them instead,
// every time until the next save here or the files match again, and the
// record's settings_sha256 is that of the bytes written. Nil when nothing
// could be written, as when no revision is past both files' (raised); the
// next tick tries again. The caller holds s.mu.
func (s *Store) follow(now time.Time) *Imported {
	kind := s.continued(client.CreditImportKindFollow)
	if rec := s.record.rec; rec.SettingsSHA256 == rec.SHA256 && s.seen <= rec.LegacyRevision && s.continuesCopy() {
		if s.copyLegacy(kind, now) == nil {
			return nil
		}
	} else {
		next, raw, err := s.raised(now)
		if err != nil || s.commit(raw) != nil {
			return nil
		}
		s.setFile(settingsFile{stamp: stampOfFile(s.path), values: next, sha: hashOf(raw)})
		s.recordImport(s.recordOf(kind, s.file.sha, now))
	}
	return &Imported{Kind: client.CreditImportKindFollow, Counts: countsOf(s.legacy.values), Revision: s.file.values.Revision}
}

// continuesCopy reports whether the legacy file goes on from the settings
// file, a byte copy of the one the record names: each of its rows at or below
// the record's revision is the settings file's row, at the same rev with the
// same values. Quota Glance numbers each save one past its file's revision, so
// once an older copy of its file is put back, or another file is placed as
// the drop-in, its newer file can hold such a rev for another value. A row it
// no longer holds needs no check: a page's save over it meets nothing stored,
// a conflict. The caller holds s.mu, and has found the settings file a byte
// copy of the legacy file the record names.
func (s *Store) continuesCopy() bool {
	limit, copied := s.record.rec.LegacyRevision, s.file.values
	for id, credit := range s.legacy.values.APICredits {
		if credit.Rev <= limit && !sameCreditRow(copied.APICredits[id], credit) {
			return false
		}
	}
	for id, renewal := range s.legacy.values.Renewals {
		if renewal.Rev <= limit && !sameRenewalRow(copied.Renewals[id], renewal) {
			return false
		}
	}
	return true
}

// sameCreditRow and sameRenewalRow report whether two stored rows are the
// same: the same rev, updatedAt and values.
func sameCreditRow(a, b APICredit) bool {
	return a.Rev == b.Rev && a.UpdatedAt.Equal(b.UpdatedAt) && a.MonthlyUSD == b.MonthlyUSD && a.Renews == b.Renews &&
		sameStoredReading(a.Reading, b.Reading)
}

func sameRenewalRow(a, b Renewal) bool {
	return a.Rev == b.Rev && a.UpdatedAt.Equal(b.UpdatedAt) && a.Date == b.Date
}

func sameStoredReading(a, b *Reading) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.RemainingUSD == b.RemainingUSD && a.At.Equal(b.At) && a.EnteredAt.Equal(b.EnteredAt) && a.OrganizationID == b.OrganizationID &&
		a.Baseline.DayStart.Equal(b.Baseline.DayStart) && a.Baseline.Until.Equal(b.Baseline.Until) && slices.Equal(a.Baseline.Usage, b.Baseline.Usage)
}

// continued is the kind of a matched or follow record. While nothing was
// saved here since the import on record, the new record continues that
// import, and keeps its kind when it is automatic or manual: their notes say
// a value saved here before the import, or only here, is not among what it
// brought, and a later copy of Quota Glance's file brings none of that back.
// Nothing was saved here while the settings file is what the record says was
// written (untouched), or the copy of a follow cut short before its record
// (followCopied). Otherwise kind. The caller holds s.mu, and has not yet
// written the settings file the new record describes, or has found it a byte
// copy of the legacy file.
func (s *Store) continued(kind string) string {
	if rec := s.record.rec; (s.untouched() || s.followCopied()) && (rec.Kind == client.CreditImportKindAutomatic || rec.Kind == client.CreditImportKindManual) {
		return rec.Kind
	}
	return kind
}

// followCopied reports whether the settings file is what a verbatim follow
// copies, as one cut short before its record leaves it: the readable record
// says the file it wrote was a byte copy of the legacy file it names, and the
// settings file is a byte copy of that file at a revision past the record's
// and below the lineage's first, whose every row past the record's revision
// was saved at or after the record's import. Under a clock that does not go
// back, every save here since the import writes at or past the lineage's
// first, so nothing saved here since is in such a file. A file from before a
// manual import, put back, is not past the revision that import raised Quota
// Glance's file to, nor does that import's own record say it wrote a byte
// copy. A file saved here before an automatic import, whose revision is
// Quota Glance's own, can be past it, but each row saved here is stamped
// before the import, while Quota Glance stamps each row it saves with the
// time, on CPA's clock, and saved past the revision the import read only
// after reading it. The caller holds s.mu.
func (s *Store) followCopied() bool {
	rec := s.record.rec
	return rec != nil && rec.SHA256 == rec.SettingsSHA256 && s.legacy.ok && s.legacy.path == rec.SourcePath &&
		s.file.sha != "" && s.file.sha == s.legacy.sha &&
		s.file.values.Revision > rec.LegacyRevision && s.file.values.Revision < rec.Lineage[0] &&
		savedSince(s.file.values, rec.LegacyRevision, rec.ImportedAt)
}

// savedSince reports whether every row of v past revision was saved at or
// after at.
func savedSince(v Values, revision uint64, at time.Time) bool {
	for _, credit := range v.APICredits {
		if credit.Rev > revision && credit.UpdatedAt.Before(at) {
			return false
		}
	}
	for _, renewal := range v.Renewals {
		if renewal.Rev > revision && renewal.UpdatedAt.Before(at) {
			return false
		}
	}
	return true
}

// raisedCopy reports whether the settings file is what a copy that raised the
// legacy file's revisions writes, Import Quota Glance's settings or a follow
// (raised), as one cut short before its record leaves it: the readable record
// names the legacy file but not the settings file (untouched), and the
// settings file holds exactly the legacy file's rows, each at the settings
// file's revision, which is past the record's and the legacy file's. A save
// here gives that revision only to the rows it changes, each stamped with the
// time of the save, so it leaves no such file unless it changed every row
// still held to what Quota Glance holds, in the second Quota Glance saved
// each of them. The caller holds s.mu.
func (s *Store) raisedCopy() bool {
	rec, f, l := s.record.rec, s.file.values, s.legacy.values
	if rec == nil || !s.legacy.ok || s.legacy.path != rec.SourcePath || s.file.sha == "" || s.untouched() ||
		f.Revision <= rec.LegacyRevision || f.Revision <= l.Revision ||
		len(f.APICredits) != len(l.APICredits) || len(f.Renewals) != len(l.Renewals) {
		return false
	}
	for id, credit := range l.APICredits {
		credit.Rev = f.Revision
		if !sameCreditRow(f.APICredits[id], credit) {
			return false
		}
	}
	for id, renewal := range l.Renewals {
		renewal.Rev = f.Revision
		if !sameRenewalRow(f.Renewals[id], renewal) {
			return false
		}
	}
	return true
}

// copyLegacy writes the legacy file's bytes verbatim as the settings file,
// then a record of kind for them, kept pending when it cannot be written
// (recordImport). Nil when the settings file could not be written: nothing
// changed then. A crash before the record leaves the files equal and the old
// record, which a later Sync completes as matched, or as the import a follow
// continued (continued). The caller holds s.mu and has read a legacy file
// that decodes.
func (s *Store) copyLegacy(kind string, now time.Time) *Imported {
	if err := s.commit(s.legacy.raw); err != nil {
		return nil
	}
	s.setFile(settingsFile{stamp: stampOfFile(s.path), values: s.legacy.values, sha: s.legacy.sha})
	s.recordImport(s.recordOf(kind, s.legacy.sha, now))
	return &Imported{Kind: kind, Counts: countsOf(s.legacy.values), Revision: s.legacy.values.Revision}
}

// recordOf is the record of an import of the legacy file just read, which
// wrote a settings file whose hash is settingsSHA. Its lineage starts where a
// file written now where there is none would: past every revision the store
// has seen and at least the time, so every file from before the import is
// below it, and saves over the imported file go on from there. Once the store
// has seen the highest revision there is, it starts there, since no save
// writes past it (nextRevision). The caller holds s.mu and has set the file
// the import wrote.
func (s *Store) recordOf(kind, settingsSHA string, now time.Time) record {
	start, ok := s.nextRevision(now)
	if !ok {
		start = math.MaxUint64
	}
	return record{
		Kind: kind, SourcePath: s.legacy.path, SHA256: s.legacy.sha, SettingsSHA256: settingsSHA,
		LegacyRevision: s.legacy.values.Revision, LegacyUpdatedAt: newestUpdate(s.legacy.values),
		Counts: countsOf(s.legacy.values), ImportedAt: now, Lineage: []uint64{start},
	}
}

// untouched reports whether nothing was saved here since the last import:
// the settings file is there and is what the readable record says was
// written.
func (s *Store) untouched() bool {
	return s.file.sha != "" && s.record.rec != nil && s.file.sha == s.record.rec.SettingsSHA256
}

// holds reports whether a settings file at revision, other than the one the
// import wrote, holds what was imported: an odd number of the lineage's
// revisions are at or below it, so it is in a span that saves over a file
// holding the import started.
func (r *record) holds(revision uint64) bool {
	return sort.Search(len(r.Lineage), func(i int) bool { return r.Lineage[i] > revision })%2 == 1
}

// holdsImport reports whether the settings file holds what the import record
// says was imported: it is the file the import wrote, or a save here wrote it
// over one that holds it (record.holds). A file from before the import, and
// one a save started after the imported file went missing, hold none of it,
// whichever is put back later. The caller holds s.mu.
func (s *Store) holdsImport() bool {
	rec := s.record.rec
	return rec != nil && s.file.sha != "" && (s.file.sha == rec.SettingsSHA256 || rec.holds(s.file.values.Revision))
}

// saveRevision is the revision a save writes, and the import record to write
// before it, nil when there is none to write. The revision is nextRevision
// unless the record's lineage gives it the other side from the file it saves
// over: a file that holds the import, or one that does not, or none, which
// holds nothing. The save then writes past the last switch, every revision
// the store has seen and the time, so no file there is now changes side, and
// when the last span is the other side too, the record goes first with the
// switch added, the oldest pair dropped past maxLineage. ErrFull when no
// revision is past those it must pass. The caller holds s.mu and has read the
// record and the settings file again.
func (s *Store) saveRevision(now time.Time) (uint64, *record, error) {
	revision, ok := s.nextRevision(now)
	if !ok {
		return 0, nil, ErrFull
	}
	rec := s.record.rec
	if rec == nil {
		return revision, nil, nil
	}
	held := s.holdsImport()
	if rec.holds(revision) == held {
		return revision, nil, nil
	}
	last := rec.Lineage[len(rec.Lineage)-1]
	if last == math.MaxUint64 {
		return 0, nil, ErrFull
	}
	revision = max(revision, last+1)
	if rec.holds(revision) == held {
		return revision, nil, nil
	}
	next := *rec
	next.Lineage = append(slices.Clone(rec.Lineage), revision)
	if len(next.Lineage) > maxLineage {
		next.Lineage = next.Lineage[2:]
	}
	return revision, &next, nil
}

// importStatus is published as settings.import. The first rule that matches
// gives the state. A record counts as none while the settings file holds
// none of what it says was imported (holdsImport): Quota Glance's values are
// then not in the store, so the state reads notImported and offers the
// import, or null with no Quota Glance file to import. The caller holds s.mu.
func (s *Store) importStatus() *client.CreditImport {
	rec, l := s.record.rec, s.legacy
	if s.file.sha != "" && !s.holdsImport() {
		rec = nil
	}
	imp := &client.CreditImport{Importable: l.ok}
	switch {
	case l.exists && !l.ok:
		imp.State = client.CreditImportStateLegacyUnreadable
	case s.record.stamp.exists && !s.file.stamp.exists:
		imp.State = client.CreditImportStateSettingsMissing
	case !l.exists && rec == nil:
		return nil
	case l.ok && (rec == nil || rec.SourcePath != l.path):
		imp.State = client.CreditImportStateNotImported
	case !l.exists || rec.SHA256 == l.sha:
		imp.State = client.CreditImportStateImported
	default:
		imp.State = client.CreditImportStateChangedSinceImport
	}
	if rec != nil {
		revision, counts, imported := strconv.FormatUint(rec.LegacyRevision, 10), rec.Counts, rec.ImportedAt
		imp.Kind, imp.SourcePath, imp.SHA256, imp.SettingsSHA256 = &rec.Kind, &rec.SourcePath, &rec.SHA256, &rec.SettingsSHA256
		imp.LegacyRevision, imp.LegacyUpdatedAt, imp.Counts, imp.ImportedAt = &revision, copyOf(rec.LegacyUpdatedAt), &counts, &imported
		// A drop-in is a copy that Quota Glance never saves to, so Quota
		// Glance's later saves are never followed from it, even while follow
		// would copy the drop-in placed again.
		imp.Following = s.untouched() && l.path == rec.SourcePath && !isDropIn(rec.SourcePath)
	}
	return cloneImport(imp)
}

// isDropIn reports whether path is a drop-in: client.LegacySettingsDropIn,
// which Quota Glance's own file, settings.json, never is.
func isDropIn(path string) bool {
	return filepath.Base(path) == client.LegacySettingsDropIn
}

// ParseImportLegacy reads an import request: exactly kind "importLegacy" and
// baseRevision, the settings file's revision the page read, "" when there was
// no file. Anything else is ErrInvalidRequest. It never carries a path: the
// legacy file is found the way Sync finds it.
func ParseImportLegacy(raw []byte) (string, error) {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return "", ErrInvalidRequest
	}
	var request struct {
		Kind         required `json:"kind"`
		BaseRevision required `json:"baseRevision"`
	}
	if strictDecode(raw, &request) != nil || !request.Kind.set || request.Kind.value != KindImportLegacy ||
		!request.BaseRevision.set || !revisionShape.MatchString(request.BaseRevision.value) {
		return "", ErrInvalidRequest
	}
	return request.BaseRevision.value, nil
}

// ImportLegacy is Import Quota Glance's settings, pressed on the page. It
// replaces every stored value with the legacy file's, sets the revision past
// the legacy file's and every revision the store has seen, as raised does,
// and every row's rev to it, so no open page's baseRevision can match a
// replaced row, and writes the settings file, then a manual record. It counts
// toward the write limit.
//
// ErrClosed, ErrUnavailable while nothing may be saved, as Apply judges it
// with the import record read again first, ErrNotImportable when
// the legacy file is missing or cannot be read, a *ConflictError when
// baseRevision is not RevisionText of the file's revision, read again first
// if the file changed on disk,
// ErrTooManyWrites, ErrFull, or ErrUnwritable, and nothing changed. A record
// that cannot be written after the settings file was is kept pending, as an
// automatic import's is (recordImport): the values are imported, and the
// record is written before anything is judged against it again. After a crash
// or a restart in between, the next Sync records it from the files while the
// old record names the legacy file (raisedCopy); otherwise the old record
// stands, the status never reports this import, and importing again is
// harmless.
func (s *Store) ImportLegacy(baseRevision string, now time.Time) (Imported, error) {
	now = now.UTC().Truncate(time.Second)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Imported{}, ErrClosed
	}
	s.reloadChanged()
	current := s.file.values
	if current.Unreadable || s.blocked {
		return Imported{}, ErrUnavailable
	}
	defer s.publish()
	s.observe()
	if s.recordUnread() {
		return Imported{}, ErrUnavailable
	}
	if !s.legacy.ok {
		return Imported{}, ErrNotImportable
	}
	if baseRevision != RevisionText(current.Revision) {
		return Imported{}, &ConflictError{Kind: KindImportLegacy, Revision: RevisionText(current.Revision)}
	}
	if !s.writes.allows(now) {
		return Imported{}, ErrTooManyWrites
	}
	next, raw, err := s.raised(now)
	if err != nil {
		return Imported{}, err
	}
	if err := s.commit(raw); err != nil {
		return Imported{}, err
	}
	s.setFile(settingsFile{stamp: stampOfFile(s.path), values: next, sha: hashOf(raw)})
	s.writes = append(s.writes, now)
	s.recordImport(s.recordOf(client.CreditImportKindManual, s.file.sha, now))
	return Imported{Kind: client.CreditImportKindManual, Counts: countsOf(s.legacy.values), Revision: next.Revision}, nil
}

// raised is the legacy file's values with the revision past the legacy
// file's and nextRevision, and every row's rev set to it, and their bytes:
// what replaces the stored values when a page may hold revisions this store
// gave out, so no open page's baseRevision can match a replaced row, even
// after the settings file was moved aside. ErrUnwritable or ErrFull when they
// cannot be written, ErrFull also when either file's revision is the highest
// there is, which none is past. The caller holds s.mu and has read a legacy
// file that decodes.
func (s *Store) raised(now time.Time) (Values, []byte, error) {
	revision, ok := s.nextRevision(now)
	if !ok || s.legacy.values.Revision == math.MaxUint64 {
		return Values{}, nil, ErrFull
	}
	next := s.legacy.values.clone()
	next.Revision = max(revision, s.legacy.values.Revision+1)
	for id, credit := range next.APICredits {
		credit.Rev = next.Revision
		next.APICredits[id] = credit
	}
	for id, renewal := range next.Renewals {
		renewal.Rev = next.Revision
		next.Renewals[id] = renewal
	}
	raw, err := encode(next)
	switch {
	case err != nil:
		return Values{}, nil, ErrUnwritable
	case len(raw) > MaxBytes:
		return Values{}, nil, ErrFull
	}
	return next, raw, nil
}

// countsOf counts what v holds: accounts with a monthly credit, with a refill
// date and with a Console reading, and renewal dates.
func countsOf(v Values) client.CreditImportCounts {
	var c client.CreditImportCounts
	for _, credit := range v.APICredits {
		if credit.MonthlyUSD != "" {
			c.MonthlyCredits++
		}
		if credit.Renews != "" {
			c.RefillDates++
		}
		if credit.Reading != nil {
			c.Readings++
		}
	}
	c.RenewalDates = len(v.Renewals)
	return c
}

// newestUpdate is the latest updatedAt in v, nil when it holds no row: how
// old an import's values are.
func newestUpdate(v Values) *time.Time {
	var newest *time.Time
	consider := func(t time.Time) {
		if newest == nil || t.After(*newest) {
			newest = &t
		}
	}
	for _, credit := range v.APICredits {
		consider(credit.UpdatedAt)
	}
	for _, renewal := range v.Renewals {
		consider(renewal.UpdatedAt)
	}
	return newest
}
