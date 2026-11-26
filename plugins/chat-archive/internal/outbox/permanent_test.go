package outbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"syscall"
	"testing"

	sqlite "github.com/mattn/go-sqlite3"
)

// TestPermanent: only errors that no retry of the same envelopes can fix are permanent; a timeout,
// busy, an I/O error, a full disk or a blob file error is not, so the capture writer keeps the group.
func TestPermanent(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"closed", ErrClosed, true},
		{"encode", fmt.Errorf("%w: %w", errEncode, errors.New("synthetic")), true},
		{"constraint", sqlite.Error{Code: sqlite.ErrConstraint}, true},
		{"mismatch", sqlite.Error{Code: sqlite.ErrMismatch}, true},
		{"too_big", sqlite.Error{Code: sqlite.ErrTooBig}, true},
		{"busy", sqlite.Error{Code: sqlite.ErrBusy}, false},
		{"locked", sqlite.Error{Code: sqlite.ErrLocked}, false},
		{"io", sqlite.Error{Code: sqlite.ErrIoErr}, false},
		{"full", sqlite.Error{Code: sqlite.ErrFull}, false},
		{"interrupt", sqlite.Error{Code: sqlite.ErrInterrupt}, false},
		{"deadline", context.DeadlineExceeded, false},
		{"blob_enospc", &fs.PathError{Op: "write", Path: "blob", Err: syscall.ENOSPC}, false},
	} {
		if got := Permanent(c.err); got != c.want {
			t.Errorf("Permanent(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}
