//go:build unix

package fsafe

import (
	"errors"
	"os"
	"syscall"
)

// openNoFollow refuses a symlink at the last element (O_NOFOLLOW fails with
// ELOOP, or EMLINK on FreeBSD) and doesn't wait for a writer on a FIFO
// (O_NONBLOCK). O_NONBLOCK has no effect on reads from regular files.
func openNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK) {
		return nil, notRegular(path)
	}
	return f, err
}
