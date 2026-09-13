package cache

import (
	"errors"
	"os"
	"syscall"
)

func lockWriter(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, errors.New("cache lock unavailable")
	}
	file := os.NewFile(uintptr(fd), path)
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("another quota-cache writer owns this path")
	}
	return file, nil
}
func unlockWriter(file *os.File) {
	if file != nil {
		syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
	}
}
