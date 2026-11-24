package outbox

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"
	"testing"

	sqlite "github.com/mattn/go-sqlite3"
)

// TestTransientWrite: the capture writer retries busy, locked and I/O failures, and nothing that
// the same write cannot pass later.
func TestTransientWrite(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{sqlite.Error{Code: sqlite.ErrBusy}, true},
		{sqlite.Error{Code: sqlite.ErrLocked}, true},
		{sqlite.Error{Code: sqlite.ErrIoErr, ExtendedCode: sqlite.ErrIoErrWrite}, true},
		{fmt.Errorf("commit: %w", sqlite.Error{Code: sqlite.ErrIoErr}), true},
		{&fs.PathError{Op: "write", Path: "blob", Err: syscall.EIO}, true},
		{&fs.PathError{Op: "open", Path: "blob", Err: syscall.EAGAIN}, true},
		{&fs.PathError{Op: "sync", Path: "blob", Err: syscall.EINTR}, true},
		{sqlite.Error{Code: sqlite.ErrConstraint}, false},
		{sqlite.Error{Code: sqlite.ErrCorrupt}, false},
		{ErrClosed, false},
		{&fs.PathError{Op: "open", Path: "blob", Err: syscall.EACCES}, false},
		{errors.New("other"), false},
	} {
		if got := TransientWrite(tc.err); got != tc.want {
			t.Errorf("TransientWrite(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
