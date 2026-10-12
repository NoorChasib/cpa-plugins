package settings

import (
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// writeLimit bounds committed saves per minute. It is a brake on a page or script stuck in a loop, not an authentication limit:
// every save that reaches it was already authenticated.
const (
	writeLimit  = 30
	writeWindow = time.Minute
)

// Errors Apply returns, each answered with its own code.
var (
	// ErrUnavailable: the settings file could not be read. Nothing may be
	// saved until it is fixed or moved aside.
	ErrUnavailable = errors.New("settings_unavailable")
	// ErrUnwritable: the commit failed. Nothing changed, on disk or here.
	ErrUnwritable = errors.New("settings_unwritable")
	// ErrFull: a bound would be passed even after evicting every renewal date
	// whose credential is no longer listed, or the write needs a revision
	// past the highest there is, which only a file edited by hand holds.
	ErrFull = errors.New("settings_full")
	// ErrTooManyWrites: writeLimit saves were committed in the last minute.
	ErrTooManyWrites = errors.New("too_many_writes")
	// ErrClosed: the store was closed because the cache stopped, so nothing
	// may be written without its writer lock.
	ErrClosed = errors.New("cache_stopped")
)

// ConflictError is a batch with a row another save changed since the page
// read it. Nothing was written.
type ConflictError struct {
	// Kind is the request's, which says whether IDs are API credit or renewal
	// rows.
	Kind string
	// Revision is the file's revision now, in decimal, as Quota Glance
	// answered it.
	Revision string
	// IDs are the conflicting rows, in request order; APICredits or Renewals
	// holds what is stored for each now, absent for a row with nothing stored.
	IDs        []string
	APICredits map[string]APICredit
	Renewals   map[string]Renewal
}

func (e *ConflictError) Error() string { return "conflict" }

// NotEditableError is a batch with a row the store cannot take as sent: a new
// reading for an account whose organization, as the published state gives
// it, is not in the form a stored reading must hold. Nothing was written.
type NotEditableError struct {
	IDs []string
}

func (e *NotEditableError) Error() string { return "not_editable" }

// Result is a batch Apply accepted.
type Result struct {
	// Revision is the file's revision after the batch.
	Revision uint64
	// Unchanged is true when every row already held what was sent, and
	// nothing was written.
	Unchanged bool
	// IDs and Fields name what changed, for the log: the rows, and the JSON
	// names of the values that moved among them.
	IDs    []string
	Fields []string
}

// Status is what the published state says about the saved values.
type Status struct {
	// State is client.CreditSettingsOK, or client.CreditSettingsUnreadable
	// while the settings file fails a load rule or could not be read.
	State string
}

// Editable reports whether a save may reach the store: the saved values can
// be read.
func (s Status) Editable() bool { return s.State == client.CreditSettingsOK }

// Store is the settings file beside one snapshot.
//
// Its mutex is a leaf lock: nothing else is acquired while it is held, and no
// caller may publish, log or call into the host while holding it. Under it
// the store does nothing but read and write its own file. Readers never take
// it: Current and Status return what the last commit or Sync swapped in.
type Store struct {
	dir, path string

	mu     sync.Mutex
	writes writeLog
	closed bool
	file   settingsFile
	// seen is the highest revision this store has read or written, kept
	// across a delete, a move aside and a file put back with a lower
	// revision, which nextRevision starts past. It is not kept across a
	// reopen; the time in firstRevision covers that.
	seen uint64

	view atomic.Pointer[view]

	// write and syncDir commit the file; tests replace them to fail a commit
	// or to see the directory synced. read reads the settings file once its
	// stamp changed or its last read did not settle; tests replace it to count
	// the reads or to fail one.
	write   func(path string, raw []byte) error
	syncDir func(dir string) error
	read    func(path string, stamp fileStamp, limit int64) (raw []byte, settled, ok bool)
}

// view is one consistent pair of values and status for readers.
type view struct {
	values Values
	status Status
}

// settingsFile is the settings file as the store last read or wrote it.
type settingsFile struct {
	stamp  fileStamp
	values Values
	// retry is set when the last read failed for a reason other than what
	// the file holds: Lstat failed, or the file could not be opened or read,
	// as when it belongs to another user or the process ran out of file
	// descriptors. Unreadable stands only until the next read, which comes
	// whether or not the stamp changed, so fixing the file's owner is enough.
	retry bool
}

// Open loads the settings beside the snapshot at snapshotPath. It never fails
// for the file's sake: a missing file is empty and editable, and one that
// cannot be read leaves an unreadable store that applies nothing and saves
// nothing, and never overwrites what is there. It writes nothing.
func Open(snapshotPath string) *Store {
	s := &Store{
		dir:     filepath.Dir(snapshotPath),
		path:    FilePath(snapshotPath),
		write:   writeFile,
		syncDir: syncDirectory,
		read:    readStamped,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setFile(s.readFile())
	s.publish()
	return s
}

// Load reads one settings file: empty when there is none, and Unreadable when
// it fails a shape rule.
func Load(path string) Values {
	raw, stamp, ok := readRegular(path, MaxBytes)
	switch {
	case !stamp.exists:
		return empty()
	case !ok:
		return unreadable()
	}
	v, err := decode(raw)
	if err != nil {
		return unreadable()
	}
	return v
}

// readFile reads the settings file with its stamp.
func (s *Store) readFile() settingsFile {
	stamp := stampOfFile(s.path)
	f := settingsFile{stamp: stamp, values: empty()}
	if !stamp.exists {
		return f
	}
	raw, settled, ok := s.read(s.path, stamp, MaxBytes)
	f.retry = !settled
	v, err := decode(raw)
	if !ok || err != nil {
		f.values = unreadable()
		return f
	}
	f.values = v
	return f
}

// setFile makes f the settings file as the store last read or wrote it. The
// caller holds s.mu.
func (s *Store) setFile(f settingsFile) {
	s.file, s.seen = f, max(s.seen, f.values.Revision)
}

// nextRevision is the revision the next commit writes, firstRevision: one
// past every revision this store has seen, and at least the time, so a page
// that read a file since deleted or moved aside, or since replaced by an older
// copy, holds no row revision a new write can match, even when that happened
// while the store was closed. False once the store has seen the highest revision there
// is, which only a file edited by hand holds: none is past it, so nothing is
// written (ErrFull), rather than a revision wrapped round below one a page may
// hold. The caller holds s.mu and has read the file again if it changed.
func (s *Store) nextRevision(now time.Time) (uint64, bool) {
	if s.seen == math.MaxUint64 {
		return 0, false
	}
	return firstRevision(s.seen, now), true
}

// firstRevision is the revision a commit writes: one past seen, the highest
// revision its store has read or written, and at least now in Unix seconds.
// The time covers revisions no store open now saw: those of a file deleted,
// moved aside or replaced by an older copy while CPA was stopped or the cache
// disabled.
// Each was at least the time it was given out, then one more per commit, at
// most writeLimit a minute, so none is past the time by more than a minute's
// commits: a store opened a minute after the last gives none out again.
func firstRevision(seen uint64, now time.Time) uint64 {
	return max(seen+1, uint64(max(now.Unix(), 0)))
}

// fileChanged reports whether the settings file must be read again: it
// changed on disk since the store last read or wrote it, or is gone, or the
// last read did not settle. The caller holds s.mu.
func (s *Store) fileChanged() bool {
	return s.file.retry || !stampOfFile(s.path).same(s.file.stamp)
}

// Dir is the snapshot's directory, where the settings file is.
func (s *Store) Dir() string { return s.dir }

// Path is the settings file's path.
func (s *Store) Path() string { return s.path }

// Current is the committed values. They are never modified: a save swaps in
// new ones, so a caller may hold these as long as it likes.
func (s *Store) Current() Values { return s.view.Load().values }

// Status is the state of the saved values, as the last commit or Sync left
// it.
func (s *Store) Status() Status { return s.view.Load().status }

// Snapshot is Current and Status as one consistent pair.
func (s *Store) Snapshot() (Values, Status) {
	v := s.view.Load()
	return v.values, v.status
}

// Close stops every later write: Apply returns ErrClosed and Sync does
// nothing. It waits for a save in flight to finish first. What the store last
// held stays readable.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

// publish swaps in the values and status the settings file now gives. The
// caller holds s.mu.
func (s *Store) publish() {
	status := Status{State: client.CreditSettingsOK}
	if s.file.values.Unreadable {
		status.State = client.CreditSettingsUnreadable
	}
	s.view.Store(&view{values: s.file.values, status: status})
}

// Sync brings the store in line with its file, once per publisher tick and at
// every configure. The settings file is read again when it changed on disk or
// is gone: moving an unreadable file aside makes the store empty and
// editable, and putting a good copy back makes it apply, with no configure.
// One that could not be opened or read is read again at every tick until a
// read settles, so fixing its owner or permissions is enough. It reports
// whether what the file gives may have changed: a read again of a changed
// file, or one that settled. After Close, Sync does nothing.
func (s *Store) Sync() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed && s.reloadChanged()
}

// reloadChanged reads the settings file again when it changed on disk since
// the store last read or wrote it, or its last read did not settle, and
// reports whether what it gives may have changed: the file changed, or the
// read settled. A save is judged against what is there now: a good copy put
// back, a file deleted or moved aside, or one that fails its rules is seen at
// once, not at the next tick, and never written over from memory. The caller
// holds s.mu.
func (s *Store) reloadChanged() bool {
	if !s.fileChanged() {
		return false
	}
	moved := !stampOfFile(s.path).same(s.file.stamp)
	s.setFile(s.readFile())
	s.publish()
	return moved || !s.file.retry
}

// Apply saves one batch, all or nothing: a conflict, the write limit, a
// baseline the meter cannot cover, a reading with no organization to store, a
// bound, or a failed commit leaves the file and the values exactly as they
// were. meter is the one the publisher last loaded, nil when there was none;
// it gives a new reading its baseline. Once the store has seen the highest
// revision there is, nothing is saved (ErrFull).
//
// The batch must have passed Check against Current; Apply checks again under
// its lock, against what is stored then, read again first if the file changed
// on disk, so neither a save racing another nor a file replaced by hand can
// slip past a rule.
//
// The lock is released when Apply returns. The caller publishes after that,
// never during.
func (s *Store) Apply(b Batch, meter *client.APIMeter, now time.Time) (Result, error) {
	now = now.UTC().Truncate(time.Second)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Result{}, ErrClosed
	}
	s.reloadChanged()
	current := s.file.values
	if current.Unreadable {
		return Result{}, ErrUnavailable
	}
	if fail := b.Check(current, now); fail != nil {
		return Result{}, fail
	}

	// A row whose page read an older revision conflicts only when it asks for
	// something other than what is stored now. Sending what another device
	// already saved is agreement, not a conflict.
	conflict := &ConflictError{Kind: b.Kind, Revision: strconv.FormatUint(current.Revision, 10), APICredits: map[string]APICredit{}, Renewals: map[string]Renewal{}}
	changed := false
	for _, item := range b.APICredits {
		stored, has := current.APICredits[item.ID]
		if sameAPICredit(stored, item) {
			continue
		}
		changed = true
		if item.BaseRevision != RevisionText(stored.Rev) {
			conflict.IDs = append(conflict.IDs, item.ID)
			if has {
				conflict.APICredits[item.ID] = stored
			}
		}
	}
	for _, item := range b.Renewals {
		stored, has := current.Renewals[item.ID]
		if sameRenewal(stored, item) {
			continue
		}
		changed = true
		if item.BaseRevision != RevisionText(stored.Rev) {
			conflict.IDs = append(conflict.IDs, item.ID)
			if has {
				conflict.Renewals[item.ID] = stored
			}
		}
	}
	if len(conflict.IDs) > 0 {
		return Result{}, conflict
	}
	if !changed {
		return Result{Revision: current.Revision, Unchanged: true}, nil
	}
	if !s.writes.allows(now) {
		return Result{}, ErrTooManyWrites
	}
	revision, ok := s.nextRevision(now)
	if !ok {
		return Result{}, ErrFull
	}

	next := current.clone()
	next.Revision = revision
	result := Result{Revision: next.Revision}
	fields := map[string]bool{}
	for _, item := range b.APICredits {
		stored := current.APICredits[item.ID]
		if sameAPICredit(stored, item) {
			continue
		}
		result.IDs = append(result.IDs, item.ID)
		credit := APICredit{Rev: next.Revision, UpdatedAt: now}
		if item.MonthlyUSD != nil {
			credit.MonthlyUSD = *item.MonthlyUSD
		}
		if item.Renews != nil {
			credit.Renews = *item.Renews
		}
		if credit.MonthlyUSD != stored.MonthlyUSD {
			fields["monthlyUsd"] = true
		}
		if credit.Renews != stored.Renews {
			fields["renews"] = true
		}
		switch r := item.Reading; {
		case r == nil:
		case sameReading(stored.Reading, r):
			credit.Reading = stored.Reading
		default:
			if org, ok := client.NormalizeOrganizationID(item.OrganizationID); !ok || org != item.OrganizationID {
				return Result{}, &NotEditableError{IDs: []string{item.ID}}
			}
			reading, ok := newReading(item, meter, now)
			if !ok {
				return Result{}, &FieldError{Code: CodeReadingTime, ID: item.ID, Field: "reading.at"}
			}
			credit.Reading = reading
		}
		if !sameReading(stored.Reading, item.Reading) {
			fields["reading"] = true
		}
		if credit.MonthlyUSD == "" && credit.Renews == "" && credit.Reading == nil {
			delete(next.APICredits, item.ID)
		} else {
			next.APICredits[item.ID] = credit
		}
	}
	for _, item := range b.Renewals {
		if sameRenewal(current.Renewals[item.ID], item) {
			continue
		}
		result.IDs = append(result.IDs, item.ID)
		fields["date"] = true
		if item.Date == nil {
			delete(next.Renewals, item.ID)
		} else {
			next.Renewals[item.ID] = Renewal{Date: *item.Date, Rev: next.Revision, UpdatedAt: now}
		}
	}
	for name := range fields {
		result.Fields = append(result.Fields, name)
	}
	sort.Strings(result.Fields)

	raw, err := s.fit(&next, b)
	if err != nil {
		return Result{}, err
	}
	if err := s.commit(raw); err != nil {
		return Result{}, err
	}
	s.setFile(settingsFile{stamp: stampOfFile(s.path), values: next})
	s.writes = append(s.writes, now)
	s.publish()
	return result, nil
}

// writeLog is a store's commits in the last writeWindow.
type writeLog []time.Time

// allows reports whether a commit at now stays within writeLimit, and forgets
// the commits that left the window, or that a clock set back put ahead of now.
func (w *writeLog) allows(now time.Time) bool {
	cutoff := now.Add(-writeWindow)
	*w = slices.DeleteFunc(*w, func(at time.Time) bool { return !at.After(cutoff) || at.After(now) })
	return len(*w) < writeLimit
}

// newReading is a reading as first saved: the account's organization, and
// its baseline from the meter. With no meter, or the organization not in it,
// the baseline is empty, which stays consistent: what was not counted then is
// not in the daily buckets either.
//
// False only when the meter no longer keeps the first hour the baseline
// needs, which a reading in the 48 hours Check allows meets only on a meter
// whose clock is ahead. A meter that has not saved since before the reading's
// hour, because it is stopped or stale, is not refused, though
// UsageInHours's covered says it lacks the later hours: the reading is
// exactly what the page asks for then. A stopped meter counted nothing in
// those hours, so the baseline from the hours it has is complete; one still
// counting without saving gives a baseline short of the truth, which
// overstates the spend since the reading, the safe direction.
func newReading(item APICreditItem, meter *client.APIMeter, now time.Time) (*Reading, bool) {
	at, _ := parseStamp(item.Reading.At)
	baseline := Baseline{DayStart: client.MeterDayStart(at), Until: client.MeterHourStart(at), Usage: []client.MeterUsage{}}
	if meter != nil {
		if org, ok := meter.Organizations[item.OrganizationID]; ok {
			kept := client.MeterHourStart(meter.FlushedAt).Add(-client.MeterHours * time.Hour)
			if baseline.DayStart.Before(kept) {
				return nil, false
			}
			usage, _ := org.UsageInHours(baseline.DayStart, baseline.Until, meter.FlushedAt)
			baseline.Usage = capBaseline(loadable(usage))
		}
	}
	return &Reading{
		RemainingUSD:   item.Reading.RemainingUSD,
		At:             at,
		EnteredAt:      now,
		OrganizationID: item.OrganizationID,
		Baseline:       baseline,
	}, true
}

// loadable puts the meter's usage in the form the settings file's load rules
// accept. An entry whose model is not a NormalizeMeterModel result, or whose
// prompt class this build does not know, such as one a newer meter adds, is
// summed under client.MeterOtherModel, which is never priced: the baseline
// then reads low and the spend since the reading high, the safe direction.
// Stored as it came, it would make the file fail those rules at the next
// open, and every value in it would stop applying.
func loadable(usage []client.MeterUsage) []client.MeterUsage {
	out := make([]client.MeterUsage, 0, len(usage))
	for _, u := range usage {
		knownModel := u.Model == client.MeterOtherModel || client.NormalizeMeterModel(u.Model, "") == u.Model
		knownPrompt := u.Prompt == "" || u.Prompt == client.MeterPromptOver100K
		if !knownModel || !knownPrompt {
			u.Model, u.Prompt = client.MeterOtherModel, ""
		}
		out = client.MergeMeterUsage(out, u)
	}
	return out
}

// capBaseline keeps the MaxBaseline-1 entries with the most tokens and sums
// the rest under client.MeterOtherModel.
func capBaseline(usage []client.MeterUsage) []client.MeterUsage {
	if len(usage) <= MaxBaseline {
		return usage
	}
	byTokens := slices.Clone(usage)
	sort.SliceStable(byTokens, func(i, j int) bool { return tokens(byTokens[i]) > tokens(byTokens[j]) })
	kept := []client.MeterUsage{}
	for i, u := range byTokens {
		if i >= MaxBaseline-1 {
			u.Model, u.Prompt = client.MeterOtherModel, ""
		}
		kept = client.MergeMeterUsage(kept, u)
	}
	return kept
}

func tokens(u client.MeterUsage) uint64 {
	total := uint64(0)
	for _, n := range []uint64{u.Input, u.Output, u.CacheRead, u.CacheWrite} {
		if total+n < total {
			return ^uint64(0)
		}
		total += n
	}
	return total
}

func sameAPICredit(stored APICredit, item APICreditItem) bool {
	return stored.MonthlyUSD == valueOf(item.MonthlyUSD) && stored.Renews == valueOf(item.Renews) &&
		sameReading(stored.Reading, item.Reading)
}

func sameRenewal(stored Renewal, item RenewalItem) bool { return stored.Date == valueOf(item.Date) }

func valueOf(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// fit encodes next within the bounds, first evicting renewal dates for
// credentials no longer listed, the oldest first, and never one the batch
// itself writes. ErrFull when that is not enough.
func (s *Store) fit(next *Values, b Batch) ([]byte, error) {
	keep := map[string]bool{}
	for id := range b.Credentials {
		keep[id] = true
	}
	for _, item := range b.Renewals {
		keep[item.ID] = true
	}
	for {
		raw, err := encode(*next)
		if err != nil {
			return nil, ErrUnwritable
		}
		if len(next.APICredits) > MaxAPICredits {
			return nil, ErrFull
		}
		if len(next.Renewals) <= MaxRenewals && len(raw) <= MaxBytes {
			return raw, nil
		}
		evict, found := "", false
		var oldest time.Time
		for id, renewal := range next.Renewals {
			if keep[id] {
				continue
			}
			if !found || renewal.UpdatedAt.Before(oldest) || (renewal.UpdatedAt.Equal(oldest) && id < evict) {
				evict, oldest, found = id, renewal.UpdatedAt, true
			}
		}
		if !found {
			return nil, ErrFull
		}
		delete(next.Renewals, evict)
	}
}

// commit writes raw over the settings file: a temporary file in the
// directory, written, synced, renamed over the old one, and the directory
// synced. A reader never sees half a file.
//
// raw must pass the load rules first. A file this build could not read back
// would work from memory until the next open and then apply nothing and lock
// editing until it was moved aside, so a save that would write one is
// refused instead.
func (s *Store) commit(raw []byte) error {
	return commitFile(s.path, raw, func(raw []byte) error {
		_, err := decode(raw)
		return err
	}, s.write, s.syncDir)
}

// commitFile writes raw over path the way every file here is written, after
// check, the file's own load rules, accepts it. Any failure is ErrUnwritable,
// and leaves the file as it was.
func commitFile(path string, raw []byte, check func([]byte) error, write func(string, []byte) error, syncDir func(string) error) error {
	if check(raw) != nil {
		return ErrUnwritable
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ErrUnwritable
	}
	if err := write(path, raw); err != nil {
		return ErrUnwritable
	}
	// The rename is visible from here on, so the values must follow it even
	// if the directory cannot be synced: some filesystems cannot sync one at
	// all, and refusing would leave the store disagreeing with its file.
	_ = syncDir(dir)
	return nil
}

// writeFile writes raw to a temporary file named ".<name>-*" beside path,
// at 0600, synced, then renamed over path.
func writeFile(path string, raw []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(0o600); err == nil {
		if _, err = file.Write(raw); err == nil {
			err = file.Sync()
		}
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

func syncDirectory(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// fileStamp is what a tick compares to tell whether a file changed: whether
// it is there, and its identity, size, modification time and mode.
type fileStamp struct {
	// exists is false only when Lstat says there is no file. A file Lstat
	// fails on for another reason counts as there, so nothing is written in
	// its place.
	exists bool
	// info is Lstat's answer, nil when it failed.
	info fs.FileInfo
}

func stampOfFile(path string) fileStamp {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fileStamp{}
	}
	if err != nil {
		return fileStamp{exists: true}
	}
	return fileStamp{exists: true, info: info}
}

// same reports whether a and b describe the same file, unchanged.
func (a fileStamp) same(b fileStamp) bool {
	if a.exists != b.exists || (a.info == nil) != (b.info == nil) {
		return false
	}
	if a.info == nil {
		return true
	}
	return os.SameFile(a.info, b.info) && a.info.Size() == b.info.Size() &&
		a.info.ModTime().Equal(b.info.ModTime()) && a.info.Mode() == b.info.Mode()
}

// readRegular reads a file the way every file here is read: Lstat, a regular
// file of at most limit bytes, read through a LimitReader. ok is false for
// anything else, and for a file that is not there.
func readRegular(path string, limit int64) ([]byte, fileStamp, bool) {
	stamp := stampOfFile(path)
	raw, _, ok := readStamped(path, stamp, limit)
	return raw, stamp, ok
}

// readStamped is readRegular for a file Lstat already gave stamp. settled is
// false when the outcome is not the file's as stamped: Lstat failed for a
// reason other than its absence, the file could not be opened or read, or it
// grew while being read. A later read may then fare differently.
func readStamped(path string, stamp fileStamp, limit int64) (raw []byte, settled, ok bool) {
	switch {
	case !stamp.exists:
		return nil, true, false
	case stamp.info == nil:
		return nil, false, false
	case !stamp.info.Mode().IsRegular() || stamp.info.Size() > limit:
		return nil, true, false
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false, false
	}
	defer file.Close()
	raw, err = io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, false, false
	}
	return raw, true, true
}
