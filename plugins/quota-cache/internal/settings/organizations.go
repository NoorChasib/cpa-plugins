package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// The organizations file: the Claude Console organizations quota-cache counts
// the API credit of, each a label and an Organization ID, edited on its page.
// It sits beside the snapshot (client.OrganizationsPath), apart from the
// settings file, which must stay byte-compatible with Quota Glance's. It is
// the only list: quota-cache writes it only for a save on its page, and never
// reads claude-api-credits into it.
//
// Like the settings file, it loads all or nothing. A file that fails a load
// rule is never overwritten: no organization is listed, editing is
// unavailable, and the page says why, until the operator moves it aside.

const (
	// organizationsSchema is the only organizations schema this build reads
	// or writes.
	organizationsSchema = 1
	// MaxOrganizationsBytes bounds the file.
	MaxOrganizationsBytes = 64 << 10
	// MaxOrganizations is every organization quota-cache can link, as for
	// claude-api-credits.
	MaxOrganizations = client.MaxAPICreditItems
	// MaxOrganizationOps bounds one batch.
	MaxOrganizationOps = 16
	// MaxOrganizationLabelRunes is claude-api-credits' bound on a label.
	MaxOrganizationLabelRunes = 64
)

// The operations of an organizations batch.
const (
	OpAdd    = "add"
	OpRename = "rename"
	OpRemove = "remove"
)

// An organizations batch's error codes, each answered 400 with the op's index
// and field.
const (
	CodeInvalidLabel          = "invalid_label"
	CodeDuplicateLabel        = "duplicate_label"
	CodeInvalidOrganizationID = "invalid_organization_id"
	CodeDuplicateOrganization = "duplicate_organization"
	CodeTooManyOrganizations  = "too_many_organizations"
	CodeUnknownID             = "unknown_id"
)

// ErrOrganizationsUnavailable: the organizations file could not be read, so
// nothing may be saved over it.
var ErrOrganizationsUnavailable = errors.New("organizations_unavailable")

// Organizations is one committed state of the organizations file. A Store
// hands out copies, so a caller may keep or change what it gets.
type Organizations struct {
	// State is a client.OrganizationsState value: ok, missing (no file,
	// empty, and the first add creates it), or unreadable (nothing applies
	// and nothing may be saved until it is moved aside).
	State string
	// Revision goes up with each committed batch. A file written where there
	// was none, by the first add, starts past every revision an earlier file
	// could have had (firstRevision), and a batch after an older copy was put
	// back goes past the highest its store has seen, so a page that read a
	// file since deleted or replaced conflicts with the new one.
	// Each entry's Rev is the revision that last wrote it. 0 without a file.
	Revision uint64
	// Items are the organizations in display order, never nil.
	Items []Organization
}

// Organization is one Claude Console organization.
type Organization struct {
	// Label is the operator's display name: trimmed, at most
	// MaxOrganizationLabelRunes printable characters, unique ignoring case.
	Label string
	// OrganizationID is as client.NormalizeOrganizationID returns it, and
	// unique. It never changes: a different ID is a different organization.
	OrganizationID string
	Rev            uint64
	UpdatedAt      time.Time
}

// ID is the organization's account id, client.APICreditOrgAccount.
func (o Organization) ID() string { return client.APICreditOrgAccount(o.OrganizationID) }

// APICredits are the organizations as the snapshot's anthropic-api entries
// carry them: the label, the organization and the position in the list.
func (o Organizations) APICredits() []client.APICredit {
	out := make([]client.APICredit, 0, len(o.Items))
	for i, item := range o.Items {
		out = append(out, client.APICredit{Label: item.Label, Position: i, OrganizationID: item.OrganizationID})
	}
	return out
}

func (o Organizations) clone() Organizations {
	o.Items = slices.Clone(o.Items)
	if o.Items == nil {
		o.Items = []Organization{}
	}
	return o
}

// ValidOrganizationLabel reports whether label may name an organization, by
// claude-api-credits' rules: not empty, no surrounding space, valid UTF-8, at
// most MaxOrganizationLabelRunes characters, each printable (unicode.IsPrint:
// no control character, no space other than U+0020, no joiner).
func ValidOrganizationLabel(label string) bool {
	return label != "" && label == strings.TrimSpace(label) && utf8.ValidString(label) &&
		utf8.RuneCountInString(label) <= MaxOrganizationLabelRunes &&
		strings.IndexFunc(label, func(r rune) bool { return !unicode.IsPrint(r) }) < 0
}

// The file as JSON. Every key is a pointer so that a present key is told from
// an absent one, which the load rules care about.
type organizationsDoc struct {
	Schema        *int              `json:"schema"`
	Revision      *string           `json:"revision"`
	Organizations []organizationDoc `json:"organizations"`
}

type organizationDoc struct {
	Label          *string `json:"label"`
	OrganizationID *string `json:"organization_id"`
	Rev            *string `json:"rev"`
	UpdatedAt      *string `json:"updated_at"`
}

// decodeOrganizations reads the organizations file and applies its load
// rules, all or nothing: at most MaxOrganizationsBytes, no unknown key, no
// trailing data, schema 1, a revision and a list, at most MaxOrganizations
// entries, each with a valid label unique ignoring case, an Organization ID
// in its normalized form and unique, a rev in [1, revision], and an
// updated_at in the one instant form.
func decodeOrganizations(raw []byte) (Organizations, error) {
	if len(raw) > MaxOrganizationsBytes {
		return Organizations{}, errShape
	}
	var doc organizationsDoc
	if strictDecode(raw, &doc) != nil || doc.Schema == nil || *doc.Schema != organizationsSchema ||
		doc.Revision == nil || doc.Organizations == nil {
		return Organizations{}, errShape
	}
	revision, ok := parseRevision(*doc.Revision)
	if !ok {
		return Organizations{}, errShape
	}
	o := Organizations{State: client.OrganizationsStateOK, Revision: revision, Items: []Organization{}}
	for _, entry := range doc.Organizations {
		if entry.Label == nil || entry.OrganizationID == nil || entry.Rev == nil || entry.UpdatedAt == nil {
			return Organizations{}, errShape
		}
		rev, okRev := parseRevision(*entry.Rev)
		updated, okUpdated := parseStamp(*entry.UpdatedAt)
		if !okRev || rev < 1 || rev > revision || !okUpdated {
			return Organizations{}, errShape
		}
		item := Organization{Label: *entry.Label, OrganizationID: *entry.OrganizationID, Rev: rev, UpdatedAt: updated}
		if !fits(item, o.Items) {
			return Organizations{}, errShape
		}
		o.Items = append(o.Items, item)
	}
	return o, nil
}

// fits reports whether item may join list: a valid label no entry has,
// ignoring case; a normalized Organization ID no entry has; and room under
// MaxOrganizations.
func fits(item Organization, list []Organization) bool {
	if len(list) >= MaxOrganizations || !ValidOrganizationLabel(item.Label) {
		return false
	}
	if org, ok := client.NormalizeOrganizationID(item.OrganizationID); !ok || org != item.OrganizationID {
		return false
	}
	return labelAt(list, item.Label, -1) < 0 && organizationAt(list, item.OrganizationID) < 0
}

// labelAt is the index of the entry other than except whose label is label,
// ignoring case as claude-api-credits does, or -1.
func labelAt(list []Organization, label string, except int) int {
	for i, item := range list {
		if i != except && strings.ToLower(item.Label) == strings.ToLower(label) {
			return i
		}
	}
	return -1
}

func organizationAt(list []Organization, organizationID string) int {
	for i, item := range list {
		if item.OrganizationID == organizationID {
			return i
		}
	}
	return -1
}

func idAt(list []Organization, id string) int {
	for i, item := range list {
		if item.ID() == id {
			return i
		}
	}
	return -1
}

// revisionDigit is a revision as the organizations file writes it: decimal,
// with no leading zero.
var revisionDigit = regexp.MustCompile(`^(0|[1-9][0-9]{0,19})$`)

func parseRevision(s string) (uint64, bool) {
	if !revisionDigit.MatchString(s) {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil
}

// encodeOrganizations writes o as the organizations file.
func encodeOrganizations(o Organizations) ([]byte, error) {
	schema, revision := organizationsSchema, strconv.FormatUint(o.Revision, 10)
	doc := organizationsDoc{Schema: &schema, Revision: &revision, Organizations: make([]organizationDoc, 0, len(o.Items))}
	for _, item := range o.Items {
		label, org, rev, updated := item.Label, item.OrganizationID, strconv.FormatUint(item.Rev, 10), stampOf(item.UpdatedAt)
		doc.Organizations = append(doc.Organizations, organizationDoc{Label: &label, OrganizationID: &org, Rev: &rev, UpdatedAt: &updated})
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// LoadOrganizations reads one organizations file: missing when there is none,
// and unreadable when it fails a load rule or cannot be opened or read.
func LoadOrganizations(path string) Organizations {
	raw, stamp, ok := readRegular(path, MaxOrganizationsBytes)
	return organizationsOf(raw, stamp, ok)
}

// organizationsOf is what one read of the file gives: raw and ok are
// readStamped's for a file Lstat gave stamp.
func organizationsOf(raw []byte, stamp fileStamp, ok bool) Organizations {
	if !stamp.exists {
		return Organizations{State: client.OrganizationsStateMissing, Items: []Organization{}}
	}
	o, err := decodeOrganizations(raw)
	if !ok || err != nil {
		return Organizations{State: client.OrganizationsStateUnreadable, Items: []Organization{}}
	}
	return o
}

// OrganizationStore is the organizations file beside one snapshot.
//
// Its mutex is a leaf lock, like Store's: nothing else is acquired while it
// is held, and no caller may publish, log or call into the host while
// holding it. Readers never take it.
type OrganizationStore struct {
	path string

	mu     sync.Mutex
	writes writeLog
	closed bool
	stamp  fileStamp
	// retry is set when the last read failed for a reason other than what
	// the file holds, as settingsFile's is: the next Sync or Apply reads the
	// file again whether or not its stamp changed.
	retry bool
	// unreported is set when Apply read the file again and what it gives may
	// have changed, so the next Sync reports it: the stamp no longer says the
	// file changed, and the caller's item list has not followed it yet.
	unreported bool
	// seen is the highest revision this store has read or written, kept
	// across a delete and across a file put back with a lower revision, which
	// nextRevision starts past.
	seen uint64

	current atomic.Pointer[Organizations]

	// write and syncDir commit a file; tests replace them to fail a commit or
	// to see the directory synced. read reads the file once its stamp
	// changed; tests replace it to fail one.
	write   func(path string, raw []byte) error
	syncDir func(dir string) error
	read    func(path string, stamp fileStamp, limit int64) (raw []byte, settled, ok bool)
}

// OpenOrganizations loads the organizations file beside the snapshot at
// snapshotPath. It never fails for the file's sake, and writes nothing.
func OpenOrganizations(snapshotPath string) *OrganizationStore {
	s := &OrganizationStore{path: client.OrganizationsPath(snapshotPath), write: writeFile, syncDir: syncDirectory, read: readStamped}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	return s
}

// load reads the file again. The caller holds s.mu.
func (s *OrganizationStore) load() {
	s.stamp = stampOfFile(s.path)
	var raw []byte
	settled, ok := true, false
	if s.stamp.exists {
		raw, settled, ok = s.read(s.path, s.stamp, MaxOrganizationsBytes)
	}
	s.retry = !settled
	o := organizationsOf(raw, s.stamp, ok)
	s.seen = max(s.seen, o.Revision)
	s.current.Store(&o)
}

// changed reports whether the file must be read again: it changed on disk
// since the store last read or wrote it, or is gone, or the last read did not
// settle. The caller holds s.mu.
func (s *OrganizationStore) changed() bool {
	return s.retry || !stampOfFile(s.path).same(s.stamp)
}

// reload reads the file again and reports whether what it gives may have
// changed: the file changed on disk, or a read settled. The caller holds s.mu.
func (s *OrganizationStore) reload() bool {
	moved := !stampOfFile(s.path).same(s.stamp)
	s.load()
	return moved || !s.retry
}

// firstRevision is the revision of a file written where there is none, past
// every revision this store has seen (the package's firstRevision).
func (s *OrganizationStore) firstRevision(now time.Time) uint64 {
	return firstRevision(s.seen, now)
}

// nextRevision is the revision a batch over current writes: one past every
// revision this store has seen, not past current's, so a page that read a
// file since replaced by an older copy holds no base revision a new write can
// give out again. A file written where there is none starts at firstRevision.
// The caller holds s.mu and has read the file again if it changed.
func (s *OrganizationStore) nextRevision(current *Organizations, now time.Time) uint64 {
	if current.State == client.OrganizationsStateMissing {
		return s.firstRevision(now)
	}
	return s.seen + 1
}

// Path is the organizations file's path.
func (s *OrganizationStore) Path() string { return s.path }

// Current is the committed organizations, as a copy.
func (s *OrganizationStore) Current() Organizations { return s.current.Load().clone() }

// Close stops every later write: Apply returns ErrClosed and Sync does
// nothing. It waits for a save in flight to finish first.
func (s *OrganizationStore) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

// Sync reads the file again when it changed on disk or is gone, once per
// publisher tick: moving an unreadable file aside, or putting a good copy
// back, needs no configure. One that could not be opened or read is read
// again at every tick until a read settles, so fixing its owner or
// permissions is enough. It reports whether what the file gives may have
// changed since the last Sync: a read again of a changed file, one that
// settled, or a read again Apply made before judging a batch, whatever the
// batch came to.
func (s *OrganizationStore) Sync() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	changed := s.unreported
	s.unreported = false
	if s.changed() && s.reload() {
		changed = true
	}
	return changed
}

// commit writes next over the file, after its load rules accept the bytes,
// and swaps it in. The caller holds s.mu.
func (s *OrganizationStore) commit(next Organizations) error {
	raw, err := encodeOrganizations(next)
	if err != nil {
		return ErrUnwritable
	}
	if err := commitFile(s.path, raw, func(raw []byte) error {
		_, err := decodeOrganizations(raw)
		return err
	}, s.write, s.syncDir); err != nil {
		return err
	}
	s.stamp, s.retry, s.seen = stampOfFile(s.path), false, max(s.seen, next.Revision)
	s.current.Store(&next)
	return nil
}

// OrganizationsBatch is one press of Save on the organizations card: its ops,
// applied in order to a copy, all or nothing.
type OrganizationsBatch struct {
	// BaseRevision is the file's revision the page read, "" without a file.
	BaseRevision string
	Ops          []OrganizationOp
}

// OrganizationOp is one operation. Add uses Label and OrganizationID, as
// sent; rename uses ID and Label; remove uses ID.
type OrganizationOp struct {
	Op             string
	ID             string
	Label          string
	OrganizationID string
}

// ParseOrganizationsBatch reads an organizations batch from JSON: an object
// with exactly kind "organizations", baseRevision and ops, between one op and
// MaxOrganizationOps, each with exactly its operation's keys, every one a
// string. Anything else is ErrInvalidRequest. The values themselves are
// judged by Apply.
func ParseOrganizationsBatch(raw []byte) (OrganizationsBatch, error) {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return OrganizationsBatch{}, ErrInvalidRequest
	}
	var envelope struct {
		Kind         required          `json:"kind"`
		BaseRevision required          `json:"baseRevision"`
		Ops          []json.RawMessage `json:"ops"`
	}
	if strictDecode(raw, &envelope) != nil || !envelope.Kind.set || envelope.Kind.value != KindOrganizations ||
		!envelope.BaseRevision.set || !revisionShape.MatchString(envelope.BaseRevision.value) ||
		len(envelope.Ops) == 0 || len(envelope.Ops) > MaxOrganizationOps {
		return OrganizationsBatch{}, ErrInvalidRequest
	}
	b := OrganizationsBatch{BaseRevision: envelope.BaseRevision.value}
	for _, rawOp := range envelope.Ops {
		var head struct {
			Op *string `json:"op"`
		}
		if json.Unmarshal(rawOp, &head) != nil || head.Op == nil {
			return OrganizationsBatch{}, ErrInvalidRequest
		}
		op := OrganizationOp{Op: *head.Op}
		switch op.Op {
		case OpAdd:
			var w struct {
				Op             required `json:"op"`
				Label          required `json:"label"`
				OrganizationID required `json:"organizationId"`
			}
			if strictDecode(rawOp, &w) != nil || !w.Label.set || !w.OrganizationID.set {
				return OrganizationsBatch{}, ErrInvalidRequest
			}
			op.Label, op.OrganizationID = w.Label.value, w.OrganizationID.value
		case OpRename:
			var w struct {
				Op    required `json:"op"`
				ID    required `json:"id"`
				Label required `json:"label"`
			}
			if strictDecode(rawOp, &w) != nil || !w.ID.set || !w.Label.set {
				return OrganizationsBatch{}, ErrInvalidRequest
			}
			op.ID, op.Label = w.ID.value, w.Label.value
		case OpRemove:
			var w struct {
				Op required `json:"op"`
				ID required `json:"id"`
			}
			if strictDecode(rawOp, &w) != nil || !w.ID.set {
				return OrganizationsBatch{}, ErrInvalidRequest
			}
			op.ID = w.ID.value
		default:
			return OrganizationsBatch{}, ErrInvalidRequest
		}
		b.Ops = append(b.Ops, op)
	}
	return b, nil
}

// OpError is an op the rules refuse: the code the page shows a message for,
// the op's index in the batch, and the field's JSON name ("" for
// too_many_organizations).
type OpError struct {
	Code  string
	Op    int
	Field string
}

func (e *OpError) Error() string { return e.Code }

// OrganizationsConflictError is a batch whose page read an older revision.
// Nothing was written.
type OrganizationsConflictError struct {
	// Revision is RevisionText of the file's revision now.
	Revision string
	// Organizations is what is stored now.
	Organizations Organizations
}

func (e *OrganizationsConflictError) Error() string { return "conflict" }

// OrganizationsResult is a batch Apply accepted.
type OrganizationsResult struct {
	// Revision is the file's revision after the batch.
	Revision uint64
	// Unchanged is true when the list came out as it was, and nothing was
	// written.
	Unchanged bool
	// Added, Renamed and Removed count the ops that changed the list, for the
	// log.
	Added, Renamed, Removed int
}

// Apply saves one batch, all or nothing: ErrClosed, ErrOrganizationsUnavailable,
// a *OrganizationsConflictError when BaseRevision is not the file's, an
// *OpError for the first op the rules refuse, ErrTooManyWrites, or
// ErrUnwritable leaves the file and the list exactly as they were. An add
// appends with rev the new revision; a rename changes the label only; a
// remove deletes the entry. The values saved for a removed
// organization stay in the settings file, and adding the same Organization ID
// back brings them back.
//
// The lock is released when Apply returns. The caller rebuilds its item list
// and publishes after that, never during: after a batch it saved, and when
// the next Sync reports that Apply read the file again, whatever the batch
// came to.
func (s *OrganizationStore) Apply(b OrganizationsBatch, now time.Time) (OrganizationsResult, error) {
	now = now.UTC().Truncate(time.Second)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return OrganizationsResult{}, ErrClosed
	}
	// A file changed on disk since the last read or write is read again
	// first, as Sync would at the next tick, so a batch is judged against
	// what is there now: one deleted or replaced conflicts, and one that
	// fails its rules is unavailable, instead of either being written over.
	// The next Sync reports the read, so the caller's item list follows it.
	if s.changed() && s.reload() {
		s.unreported = true
	}
	current := s.current.Load()
	if current.State == client.OrganizationsStateUnreadable {
		return OrganizationsResult{}, ErrOrganizationsUnavailable
	}
	if b.BaseRevision != RevisionText(current.Revision) {
		return OrganizationsResult{}, &OrganizationsConflictError{Revision: RevisionText(current.Revision), Organizations: current.clone()}
	}
	revision := s.nextRevision(current, now)
	list, result, fail := b.apply(current.Items, revision, now)
	if fail != nil {
		return OrganizationsResult{}, fail
	}
	if sameOrganizations(list, current.Items) {
		return OrganizationsResult{Revision: current.Revision, Unchanged: true}, nil
	}
	if !s.writes.allows(now) {
		return OrganizationsResult{}, ErrTooManyWrites
	}
	next := Organizations{State: client.OrganizationsStateOK, Revision: revision, Items: list}
	if err := s.commit(next); err != nil {
		return OrganizationsResult{}, err
	}
	s.writes = append(s.writes, now)
	result.Revision = revision
	return result, nil
}

// apply runs the ops in order on a copy of list, stamping what they change
// with revision and now, and returns the new list, or the first op the rules
// refuse.
func (b OrganizationsBatch) apply(list []Organization, revision uint64, now time.Time) ([]Organization, OrganizationsResult, *OpError) {
	list = slices.Clone(list)
	var result OrganizationsResult
	for i, op := range b.Ops {
		fail := func(code, field string) ([]Organization, OrganizationsResult, *OpError) {
			return nil, OrganizationsResult{}, &OpError{Code: code, Op: i, Field: field}
		}
		switch op.Op {
		case OpAdd:
			label := strings.TrimSpace(op.Label)
			switch org, ok := client.NormalizeOrganizationID(op.OrganizationID); {
			case !ValidOrganizationLabel(label):
				return fail(CodeInvalidLabel, "label")
			case labelAt(list, label, -1) >= 0:
				return fail(CodeDuplicateLabel, "label")
			case !ok:
				return fail(CodeInvalidOrganizationID, "organizationId")
			case organizationAt(list, org) >= 0:
				return fail(CodeDuplicateOrganization, "organizationId")
			case len(list) >= MaxOrganizations:
				return fail(CodeTooManyOrganizations, "")
			default:
				list = append(list, Organization{Label: label, OrganizationID: org, Rev: revision, UpdatedAt: now})
				result.Added++
			}
		case OpRename:
			at, label := idAt(list, op.ID), strings.TrimSpace(op.Label)
			switch {
			case at < 0:
				return fail(CodeUnknownID, "id")
			case !ValidOrganizationLabel(label):
				return fail(CodeInvalidLabel, "label")
			case labelAt(list, label, at) >= 0:
				return fail(CodeDuplicateLabel, "label")
			case list[at].Label != label:
				list[at].Label, list[at].Rev, list[at].UpdatedAt = label, revision, now
				result.Renamed++
			}
		case OpRemove:
			at := idAt(list, op.ID)
			if at < 0 {
				return fail(CodeUnknownID, "id")
			}
			list = slices.Delete(list, at, at+1)
			result.Removed++
		default:
			return fail(ErrInvalidRequest.Error(), "op")
		}
	}
	return list, result, nil
}

// sameOrganizations reports whether a and b list the same organizations, with
// the same labels, in the same order.
func sameOrganizations(a, b []Organization) bool {
	return slices.EqualFunc(a, b, func(x, y Organization) bool {
		return x.Label == y.Label && x.OrganizationID == y.OrganizationID
	})
}
