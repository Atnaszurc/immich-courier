// Package fsafe opens files found in watched folders without trusting the
// path: anyone who can write to a watched folder chooses what sits there.
// A symlink must not make the app read a file outside the folder, and a
// FIFO or device must not block a worker forever.
package fsafe

import (
	"errors"
	"io/fs"
	"os"
)

// ErrNotRegular means the path is a symlink, FIFO, device, directory or
// anything else that isn't a plain file.
var ErrNotRegular = errors.New("not a regular file")

// OpenRegular opens path for reading only if it is a regular file, without
// following a symlink at the last element and without blocking on special
// files. The FileInfo describes the opened file itself, so callers can check
// it against what they expect before reading.
func OpenRegular(path string) (*os.File, fs.FileInfo, error) {
	f, err := openNoFollow(path)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, nil, notRegular(path)
	}
	return f, st, nil
}

// IsRegular reports whether path itself (not a symlink target) is a regular
// file, and returns its info.
func IsRegular(path string) (fs.FileInfo, bool) {
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() {
		return nil, false
	}
	return st, true
}

func notRegular(path string) error {
	return &fs.PathError{Op: "open", Path: path, Err: ErrNotRegular}
}
