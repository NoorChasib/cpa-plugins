package client

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// A refresh request asks quota-cache to poll a credential now rather than at
// its next scheduled poll, then every other credential after it. Quota Glance
// writes one after a redeem that spent, or may have spent, a banked reset, so
// the new limits show within a minute instead of at the next poll, up to a
// poll interval away. CPA gives plugins no way to call one another, so the
// request is a small file beside the snapshot (RefreshRequestPath): the one
// file in quota-cache's directory that another plugin writes. Quota Glance
// writes it with AppendRefreshRequest and quota-cache reads it with
// LoadRefreshRequests; quota-cache never writes, renames or deletes it.

// Refresh request file limits and values.
const (
	RefreshRequestSchema   = 1       // the only schema LoadRefreshRequests accepts
	MaxRefreshRequestBytes = 4 << 10 // the largest refresh request file LoadRefreshRequests reads
	MaxRefreshRequests     = 8       // requests AppendRefreshRequest keeps, the newest
	// RefreshRequestMaxAge is how long a request stays live. quota-cache never
	// acts on one dated further from its clock than this, either way, and
	// AppendRefreshRequest drops such requests from the file.
	RefreshRequestMaxAge = 10 * time.Minute
	// RefreshReasonReset is a request's reason after a banked reset was spent,
	// the only reason there is.
	RefreshReasonReset = "reset"
)

// ErrRefreshRequestInvalid is AppendRefreshRequest's error for a request that
// LoadRefreshRequests would skip, which is therefore not written.
var ErrRefreshRequestInvalid = errors.New("invalid refresh request")

// errRefreshNotWritten is AppendRefreshRequest's error when the file could not
// be written. It names no path, so a caller may log it as it is.
var errRefreshNotWritten = errors.New("refresh request cannot be written")

// ErrRefreshNotRead is wrapped, with ErrUnavailable, by LoadRefreshRequests'
// error for a file that is there but could not be looked at, opened or read,
// as when the process is out of file descriptors. It says nothing of what the
// file holds, so a later read may fare differently.
var ErrRefreshNotRead = errors.New("refresh request file not read")

// errRefreshUnread is LoadRefreshRequests' error for such a file.
var errRefreshUnread = fmt.Errorf("%w: %w", ErrUnavailable, ErrRefreshNotRead)

// refreshID is a request's id and a CPA auth index: 16 lower-case hex digits.
var refreshID = renewalKey

// RefreshRequestPath is where quota-cache looks for refresh requests for the
// snapshot at snapshotPath: the same path with a final ".json" replaced by
// ".refresh.json", else with ".refresh.json" appended. For DefaultPath it is
// plugins/data/quota-cache/snapshot.refresh.json.
func RefreshRequestPath(snapshotPath string) string {
	return strings.TrimSuffix(snapshotPath, ".json") + ".refresh.json"
}

// RefreshRequest is one request: poll the credential Provider and AuthIndex
// name first, then every other one.
type RefreshRequest struct {
	// ID is random, 16 lower-case hex digits. quota-cache acts on each id
	// once.
	ID string `json:"id"`
	// Provider is "codex" or "claude", the providers with banked resets.
	Provider string `json:"provider"`
	// AuthIndex is the credential's CPA auth index, 16 lower-case hex digits.
	AuthIndex string `json:"auth_index"`
	// Reason is RefreshReasonReset.
	Reason string `json:"reason"`
	// At is when the request was made, RFC 3339 in UTC.
	At time.Time `json:"at"`
}

// valid reports whether LoadRefreshRequests keeps r.
func (r RefreshRequest) valid() bool {
	return refreshID.MatchString(r.ID) && (r.Provider == "codex" || r.Provider == "claude") &&
		refreshID.MatchString(r.AuthIndex) && r.Reason == RefreshReasonReset && !r.At.IsZero()
}

// Live reports whether r is dated within RefreshRequestMaxAge of now, either
// way: one older has had its chance, and one dated further ahead was made by
// a clock quota-cache does not share.
func (r RefreshRequest) Live(now time.Time) bool {
	age := now.Sub(r.At)
	return age <= RefreshRequestMaxAge && age >= -RefreshRequestMaxAge
}

type refreshFile struct {
	Schema   int               `json:"schema"`
	Requests []json.RawMessage `json:"requests"`
}

// LoadRefreshRequests reads a refresh request file: a regular file (Lstat), at
// most MaxRefreshRequestBytes, valid JSON, Schema == RefreshRequestSchema. A
// request that is not valid, or repeats an earlier id, is skipped on its own:
// its id must be 16 lower-case hex digits, its provider "codex" or "claude",
// its auth_index 16 lower-case hex digits, its reason "reset", and its at an
// RFC 3339 time. The rest are returned oldest first, never nil.
//
// A missing file holds no requests and is no error. Any other file that fails
// a rule holds none either, and the error is ErrUnavailable. So does one that
// could not be looked at, opened or read, and its error also wraps
// ErrRefreshNotRead.
func LoadRefreshRequests(path string) ([]RefreshRequest, error) {
	requests := []RefreshRequest{}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return requests, nil
	}
	if err != nil {
		return requests, errRefreshUnread
	}
	if !info.Mode().IsRegular() {
		return requests, ErrUnavailable
	}
	file, err := os.Open(path)
	if err != nil {
		return requests, errRefreshUnread
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, MaxRefreshRequestBytes+1))
	if err != nil {
		return requests, errRefreshUnread
	}
	if len(raw) > MaxRefreshRequestBytes {
		return requests, ErrUnavailable
	}
	var decoded refreshFile
	if json.Unmarshal(raw, &decoded) != nil || decoded.Schema != RefreshRequestSchema {
		return requests, ErrUnavailable
	}
	seen := map[string]bool{}
	for _, item := range decoded.Requests {
		var request RefreshRequest
		if json.Unmarshal(item, &request) != nil || !request.valid() || seen[request.ID] {
			continue
		}
		seen[request.ID] = true
		requests = append(requests, request)
	}
	sortRefreshRequests(requests)
	return requests, nil
}

// sortRefreshRequests puts requests oldest first, keeping the file's order
// between requests made at the same instant.
func sortRefreshRequests(requests []RefreshRequest) {
	slices.SortStableFunc(requests, func(a, b RefreshRequest) int { return a.At.Compare(b.At) })
}

// refreshWrites serializes AppendRefreshRequest within one process, so two
// redeems that finish together cannot each drop the other's request.
var refreshWrites sync.Mutex

// AppendRefreshRequest adds request to the refresh request file at path, for
// quota-cache to act on at its next tick. An empty ID is filled with a random
// one, and a zero At with now; At is stored in UTC, whole seconds. A request
// that is still not valid is not written, and the error is
// ErrRefreshRequestInvalid.
//
// The file as it is now is read with LoadRefreshRequests; a missing or
// unreadable one counts as empty and is replaced. Requests that are no longer
// Live at now are dropped, and so is one with the same id as request. Of the
// rest, the newest MaxRefreshRequests-1 are kept, then request is added, so
// the file stays a few hundred bytes however often it is written and request
// is always in it.
//
// The write is atomic: a temporary file named ".<name>-*" in the same
// directory, at 0600, synced, renamed over path, and the directory synced. The
// directory is never created: quota-cache creates it, and without it the
// error says the file could not be written. The error names no path.
func AppendRefreshRequest(path string, request RefreshRequest, now time.Time) error {
	if request.ID == "" {
		request.ID = newRefreshRequestID()
	}
	if request.At.IsZero() {
		request.At = now
	}
	request.At = request.At.UTC().Truncate(time.Second)
	if !request.valid() {
		return ErrRefreshRequestInvalid
	}
	refreshWrites.Lock()
	defer refreshWrites.Unlock()
	existing, _ := LoadRefreshRequests(path)
	kept := make([]RefreshRequest, 0, MaxRefreshRequests)
	for _, r := range existing {
		if r.Live(now) && r.ID != request.ID {
			kept = append(kept, r)
		}
	}
	kept = append(kept[max(0, len(kept)-(MaxRefreshRequests-1)):], request)
	sortRefreshRequests(kept)
	raw, err := json.Marshal(struct {
		Schema   int              `json:"schema"`
		Requests []RefreshRequest `json:"requests"`
	}{RefreshRequestSchema, kept})
	if err != nil || len(raw) > MaxRefreshRequestBytes {
		return errRefreshNotWritten
	}
	if writeRefreshFile(path, raw) != nil {
		return errRefreshNotWritten
	}
	return nil
}

// newRefreshRequestID is a random request id: 16 lower-case hex digits from
// crypto/rand, which never fails.
func newRefreshRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// writeRefreshFile writes raw over path the way quota-cache writes its own
// files: a temporary file named ".<name>-*" beside it at 0600, written,
// synced, renamed over it, and the directory synced. A reader never sees half
// a file.
func writeRefreshFile(path string, raw []byte) error {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
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
	if err := os.Rename(name, path); err != nil {
		return err
	}
	// The rename is visible from here on; a directory that cannot be synced
	// is no reason to report the request lost.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
