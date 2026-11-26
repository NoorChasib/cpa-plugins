//go:build cgo

// Package diskstall stalls or fails SQLite's disk I/O at chosen points, for tests only: the
// stall-injection harness for the outbox (K10). Install registers a pass-through VFS (vfs.c) as
// the process's default SQLite VFS, so every database mattn/go-sqlite3 opens afterwards goes
// through it; Arm then makes the reads, writes, syncs or truncates of files under one directory
// wait (a stalled disk) or return an SQLite I/O error (a failing disk). Operations under other
// directories, and every operation while no rule matches, pass straight through.
//
// The VFS sees only SQLite's own files (the database, its WAL and journal). The outbox's blob
// files are written with package os and are not covered.
//
// No production code imports this package; it links into test binaries only.
package diskstall

/*
#include "vfs.h"
*/
import "C"

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Op is a bit set of file operations.
type Op int

// File operations the VFS gates.
const (
	Read     Op = C.DISKSTALL_READ
	Write    Op = C.DISKSTALL_WRITE
	Sync     Op = C.DISKSTALL_SYNC
	Truncate Op = C.DISKSTALL_TRUNCATE
)

// File is a bit set of SQLite file kinds.
type File int

// File kinds, from the flags SQLite opened the file with.
const (
	MainDB  File = C.DISKSTALL_MAIN_DB
	WAL     File = C.DISKSTALL_WAL
	Journal File = C.DISKSTALL_JOURNAL
	Other   File = C.DISKSTALL_OTHER
)

// Code is an SQLite result code a failing operation returns.
type Code int

// Result codes for Rule.Fail (sqlite3.h primary and extended codes).
const (
	IOErrRead  Code = 10 | 1<<8  // SQLITE_IOERR_READ
	IOErrWrite Code = 10 | 3<<8  // SQLITE_IOERR_WRITE
	IOErrFsync Code = 10 | 4<<8  // SQLITE_IOERR_FSYNC
	IOErrTrunc Code = 10 | 6<<8  // SQLITE_IOERR_TRUNCATE
	Full       Code = 13         // SQLITE_FULL
	IOErr      Code = 10         // SQLITE_IOERR
	noFailure  Code = C.SQLITE_OK // the operation runs after its wait
)

// Rule picks the operations it affects and what happens to them. An affected operation first
// waits (Hold, else Delay and Until), then runs, or returns Fail when Fail is set. A rule with no
// wait and no Fail only counts what it matches.
type Rule struct {
	Dir   string // files whose path is under this directory
	Ops   Op     // operations (0: all)
	Files File   // file kinds (0: all)
	Skip  int    // matching operations let through before the rule affects any
	Count int    // matching operations affected after Skip (0: every one until Disarm)

	Hold  <-chan struct{} // wait until this closes (Delay and Until are ignored)
	Delay time.Duration   // else wait this long
	Until time.Time       // and at least until this time (a disk frozen until then)
	Fail  Code            // then return this code instead of running the operation
}

// Stats counts what one rule saw.
type Stats struct {
	Matched    int           // operations that matched Dir, Ops and Files
	Affected   int           // operations the rule held, delayed or failed (or counted)
	Waiting    int           // operations waiting now
	MaxWaiting int           // most operations waiting at once
	Waited     time.Duration // total time operations waited
	MaxWait    time.Duration // longest single wait
}

// Handle is one armed rule.
type Handle struct {
	rule     Rule
	released chan struct{}
	once     sync.Once
	stats    Stats
	changed  chan struct{} // closed and replaced on every Stats change
}

type openFile struct {
	path string
	kind File
}

var (
	installOnce sync.Once
	installErr  error

	mu     sync.Mutex
	files  = map[int]openFile{}
	nextID int
	rules  []*Handle
)

// Install registers the VFS as the default for databases opened from now on. It is idempotent.
func Install() error {
	installOnce.Do(func() {
		if rc := C.diskstall_register(); rc != C.SQLITE_OK {
			installErr = errors.New("diskstall: VFS registration failed")
		}
	})
	return installErr
}

// Arm adds r. Rules apply in the order armed; every matching rule counts an operation, their waits
// run one after another, and the first Fail set wins.
func Arm(r Rule) *Handle {
	if real, err := filepath.EvalSymlinks(r.Dir); err == nil {
		r.Dir = real // SQLite opens files by their resolved path
	}
	h := &Handle{rule: r, released: make(chan struct{}), changed: make(chan struct{})}
	mu.Lock()
	rules = append(rules, h)
	mu.Unlock()
	return h
}

// Disarm removes the rule and releases every operation it holds or delays.
func (h *Handle) Disarm() {
	h.once.Do(func() { close(h.released) })
	mu.Lock()
	defer mu.Unlock()
	for i, r := range rules {
		if r == h {
			rules = append(rules[:i], rules[i+1:]...)
			break
		}
	}
}

// Stats returns the rule's counters.
func (h *Handle) Stats() Stats {
	mu.Lock()
	defer mu.Unlock()
	return h.stats
}

// WaitFor waits until cond holds for the rule's counters or timeout passes, and reports whether it
// held.
func (h *Handle) WaitFor(timeout time.Duration, cond func(Stats) bool) bool {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		mu.Lock()
		ok, ch := cond(h.stats), h.changed
		mu.Unlock()
		if ok {
			return true
		}
		select {
		case <-ch:
		case <-t.C:
			return false
		}
	}
}

// bump signals a Stats change; mu is held.
func (h *Handle) bump() {
	close(h.changed)
	h.changed = make(chan struct{})
}

func (r *Rule) matches(f openFile, op Op) bool {
	if r.Ops != 0 && r.Ops&op == 0 {
		return false
	}
	if r.Files != 0 && r.Files&f.kind == 0 {
		return false
	}
	dir := strings.TrimSuffix(r.Dir, "/") + "/"
	return strings.HasPrefix(f.path, dir)
}

//export diskstallOpen
func diskstallOpen(path *C.char, kind C.int) C.int {
	mu.Lock()
	defer mu.Unlock()
	nextID++
	files[nextID] = openFile{path: C.GoString(path), kind: File(kind)}
	return C.int(nextID)
}

//export diskstallClose
func diskstallClose(id C.int) {
	mu.Lock()
	delete(files, int(id))
	mu.Unlock()
}

//export diskstallGate
func diskstallGate(id C.int, op C.int) C.int {
	mu.Lock()
	f, ok := files[int(id)]
	if !ok || len(rules) == 0 {
		mu.Unlock()
		return C.SQLITE_OK
	}
	var hit []*Handle
	for _, h := range rules {
		if !h.rule.matches(f, Op(op)) {
			continue
		}
		h.stats.Matched++
		n := h.stats.Matched - h.rule.Skip
		if n <= 0 || (h.rule.Count > 0 && n > h.rule.Count) {
			h.bump()
			continue
		}
		h.stats.Affected++
		h.bump()
		hit = append(hit, h)
	}
	mu.Unlock()
	fail := noFailure
	for _, h := range hit {
		h.wait()
		if fail == noFailure && h.rule.Fail != noFailure {
			fail = h.rule.Fail
		}
	}
	return C.int(fail)
}

// wait blocks one operation as the rule says and records the wait.
func (h *Handle) wait() {
	r := &h.rule
	var timer <-chan time.Time
	switch {
	case r.Hold != nil:
	case r.Delay > 0 || !r.Until.IsZero():
		d := max(r.Delay, time.Until(r.Until))
		if d <= 0 {
			return
		}
		t := time.NewTimer(d)
		defer t.Stop()
		timer = t.C
	default:
		return
	}
	start := time.Now()
	mu.Lock()
	h.stats.Waiting++
	h.stats.MaxWaiting = max(h.stats.MaxWaiting, h.stats.Waiting)
	h.bump()
	mu.Unlock()
	select {
	case <-r.Hold:
	case <-timer:
	case <-h.released:
	}
	d := time.Since(start)
	mu.Lock()
	h.stats.Waiting--
	h.stats.Waited += d
	h.stats.MaxWait = max(h.stats.MaxWait, d)
	h.bump()
	mu.Unlock()
}
