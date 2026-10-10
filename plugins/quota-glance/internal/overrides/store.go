package overrides

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// writeLimit bounds committed saves, both doors together, per minute. It is a
// brake on a page or script stuck in a loop, not an authentication limit:
// every save that reaches it was already authenticated.
const (
	writeLimit  = 30
	writeWindow = time.Minute
)

// Errors Apply returns, each answered with its own code.
var (
	// ErrUnavailable: settings.json could not be read, so nothing may be
	// saved over it.
	ErrUnavailable = errors.New("settings_unavailable")
	// ErrUnwritable: the commit failed. Nothing changed, on disk or here.
	ErrUnwritable = errors.New("settings_unwritable")
	// ErrFull: a bound would be passed even after evicting every renewal date
	// whose credential the dashboard no longer lists.
	ErrFull = errors.New("settings_full")
	// ErrTooManyWrites: writeLimit saves were committed in the last minute.
	ErrTooManyWrites = errors.New("too_many_writes")
)

// ConflictError is a batch with a row another save changed since the page
// read it. Nothing was written.
type ConflictError struct {
	// Kind is the batch's, which says whether IDs are API credit or renewal
	// rows.
	Kind     string
	Revision string
	// IDs are the conflicting rows, in request order; APICredits or Renewals
	// holds what is stored for each now, absent for a row with nothing stored.
	IDs        []string
	APICredits map[string]APICredit
	Renewals   map[string]Renewal
}

func (e *ConflictError) Error() string { return "conflict" }

// NotEditableError is a batch with a row the store cannot take as sent: a new
// reading for an account whose organization, as the served document gives it,
// is not in the form a stored reading must hold. Nothing was written.
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

// Store is settings.json in one data directory.
//
// Its mutex is a leaf lock: nothing else is acquired while it is held, and no
// caller may rebuild, publish or call into the host while holding it. Readers
// never take it: Current returns the values the last commit swapped in.
type Store struct {
	dir, path string

	mu     sync.Mutex
	writes []time.Time

	values atomic.Pointer[Values]

	// write and syncDir commit a file; tests replace them to fail a commit or
	// to see the directory synced.
	write   func(path string, raw []byte) error
	syncDir func(dir string) error
}

// Open loads <dir>/settings.json. It never fails for the file's sake: a
// missing file is empty and editable, and one that cannot be read leaves an
// unreadable store that applies nothing and saves nothing, and never
// overwrites what is there.
func Open(dir string) *Store {
	s := &Store{dir: dir, path: filepath.Join(dir, FileName), write: writeFile, syncDir: syncDirectory}
	v := Load(s.path)
	s.values.Store(&v)
	return s
}

// Load reads one settings file: empty when there is none, and Unreadable when
// it fails a shape rule. Open uses it; so do the fixtures.
func Load(path string) Values {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return empty()
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxBytes {
		return unreadable()
	}
	file, err := os.Open(path)
	if err != nil {
		return unreadable()
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, MaxBytes+1))
	if err != nil {
		return unreadable()
	}
	v, err := decode(raw)
	if err != nil {
		return unreadable()
	}
	return v
}

func unreadable() Values {
	v := empty()
	v.Unreadable = true
	return v
}

// Dir is the data directory the store was opened in.
func (s *Store) Dir() string { return s.dir }

// Path is settings.json's path.
func (s *Store) Path() string { return s.path }

// Current is the committed values. They are never modified: a save swaps in
// new ones, so a caller may hold these as long as it likes.
func (s *Store) Current() Values { return *s.values.Load() }

// LastError is "" or "unreadable", for health.
func (s *Store) LastError() string {
	if s.Current().Unreadable {
		return "unreadable"
	}
	return ""
}

// Apply saves one batch, all or nothing: a conflict, the write limit, a
// baseline the meter cannot cover, a reading with no organization to store, a
// bound, or a failed commit leaves the file and the values exactly as they
// were. meter is the one the last rebuild read, nil when there was none; it
// gives a new reading its baseline.
//
// The batch must have passed Check against Current; Apply checks again under
// its lock, against what is stored then, so a save racing another cannot slip
// past a rule.
//
// The lock is released when Apply returns. The caller rebuilds after that,
// never during.
func (s *Store) Apply(b Batch, meter *qc.APIMeter, now time.Time) (Result, error) {
	now = now.UTC().Truncate(time.Second)
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.Current()
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

	cutoff := now.Add(-writeWindow)
	s.writes = slices.DeleteFunc(s.writes, func(at time.Time) bool { return !at.After(cutoff) || at.After(now) })
	if len(s.writes) >= writeLimit {
		return Result{}, ErrTooManyWrites
	}

	next := current.clone()
	next.Revision++
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
			if org, ok := qc.NormalizeOrganizationID(item.OrganizationID); !ok || org != item.OrganizationID {
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
	s.values.Store(&next)
	s.writes = append(s.writes, now)
	return result, nil
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
func newReading(item APICreditItem, meter *qc.APIMeter, now time.Time) (*Reading, bool) {
	at, _ := parseStamp(item.Reading.At)
	baseline := Baseline{DayStart: qc.MeterDayStart(at), Until: qc.MeterHourStart(at), Usage: []qc.MeterUsage{}}
	if meter != nil {
		if org, ok := meter.Organizations[item.OrganizationID]; ok {
			kept := qc.MeterHourStart(meter.FlushedAt).Add(-qc.MeterHours * time.Hour)
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

// loadable puts the meter's usage in the form settings.json's load rules
// accept. An entry whose model is not a NormalizeMeterModel result, or whose
// prompt class this build does not know, such as one a newer quota-cache
// adds, is summed under qc.MeterOtherModel, which is never priced: the
// baseline then reads low and the spend since the reading high, the safe
// direction. Stored as it came, it would make the file fail those rules at
// the next open, and every value in it would stop applying.
func loadable(usage []qc.MeterUsage) []qc.MeterUsage {
	out := make([]qc.MeterUsage, 0, len(usage))
	for _, u := range usage {
		knownModel := u.Model == qc.MeterOtherModel || qc.NormalizeMeterModel(u.Model, "") == u.Model
		knownPrompt := u.Prompt == "" || u.Prompt == qc.MeterPromptOver100K
		if !knownModel || !knownPrompt {
			u.Model, u.Prompt = qc.MeterOtherModel, ""
		}
		out = qc.MergeMeterUsage(out, u)
	}
	return out
}

// capBaseline keeps the MaxBaseline-1 entries with the most tokens and sums
// the rest under qc.MeterOtherModel.
func capBaseline(usage []qc.MeterUsage) []qc.MeterUsage {
	if len(usage) <= MaxBaseline {
		return usage
	}
	byTokens := slices.Clone(usage)
	sort.SliceStable(byTokens, func(i, j int) bool { return tokens(byTokens[i]) > tokens(byTokens[j]) })
	kept := []qc.MeterUsage{}
	for i, u := range byTokens {
		if i >= MaxBaseline-1 {
			u.Model, u.Prompt = qc.MeterOtherModel, ""
		}
		kept = qc.MergeMeterUsage(kept, u)
	}
	return kept
}

func tokens(u qc.MeterUsage) uint64 {
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
// credentials the dashboard no longer lists, the oldest first, and never one
// the batch itself writes. ErrFull when that is not enough.
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

// commit writes raw over settings.json: a temporary file in the directory,
// written, synced, renamed over the old one, and the directory synced. A
// reader never sees half a file.
//
// raw must pass the load rules first. A file this build could not read back
// would work from memory until the next open and then apply nothing and lock
// editing until it was moved aside, so a save that would write one is
// refused instead.
func (s *Store) commit(raw []byte) error {
	if _, err := decode(raw); err != nil {
		return ErrUnwritable
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return ErrUnwritable
	}
	if err := s.write(s.path, raw); err != nil {
		return ErrUnwritable
	}
	// The rename is visible from here on, so the values must follow it even
	// if the directory cannot be synced: some filesystems cannot sync one at
	// all, and refusing would leave this store disagreeing with its file.
	_ = s.syncDir(s.dir)
	return nil
}

func writeFile(path string, raw []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".settings-*")
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
