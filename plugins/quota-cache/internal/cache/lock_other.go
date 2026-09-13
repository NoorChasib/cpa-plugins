//go:build !linux

package cache

import (
	"errors"
	"os"
)

func lockWriter(string) (*os.File, error) {
	return nil, errors.New("quota-cache writer currently supports Linux only")
}
func unlockWriter(*os.File) {}
