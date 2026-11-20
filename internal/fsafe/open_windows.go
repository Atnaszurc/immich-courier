//go:build windows

package fsafe

import (
	"os"
)

// openNoFollow on Windows: there are no FIFOs in folders, and creating a
// symlink needs admin rights or Developer Mode. The path must not be a
// reparse point when checked, and the opened handle must be that same file
// (same volume and file ID), so a link swapped in between is caught.
//
// The identity check is best effort: Lstat's file ID is read by opening the
// path right after Lstat, so a link swapped in between those two calls and
// kept until the open would pass. Opening with FILE_FLAG_OPEN_REPARSE_POINT
// would close that gap but changes how cloud placeholder files (OneDrive)
// behave, so it isn't used.
func openNoFollow(path string) (*os.File, error) {
	lst, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !lst.Mode().IsRegular() {
		return nil, notRegular(path)
	}
	os.SameFile(lst, lst) // reads and caches the file ID now, before the open
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !os.SameFile(lst, st) {
		f.Close()
		return nil, notRegular(path)
	}
	return f, nil
}
