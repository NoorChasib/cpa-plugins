package importer

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// acquireLock takes the exclusive flock on path, so only one run, backfill, enroll or requeue works on the
// state at a time. With wait false it returns busy=true at once when another process holds it. release
// closes the file, which drops the lock; the kernel also drops it when the process exits.
func acquireLock(path string, wait bool) (release func(), busy bool, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, err
	}
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	for {
		err = syscall.Flock(int(f.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return func() { f.Close() }, false, nil
}
