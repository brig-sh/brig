//go:build !darwin && !linux

package ttytest

import (
	"errors"
	"os"
)

// open has nothing to offer on a platform brig does not ship for, and Pair
// turns that into a skip.
func open() (master, slave *os.File, err error) {
	return nil, nil, errors.New("no pseudo-terminal on this platform")
}
