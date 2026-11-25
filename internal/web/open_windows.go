//go:build windows

package web

import (
	"errors"
	"os"
)

// openFile opens path for reading only if it is a regular file and not a
// link (a reparse point), checked before the open, and only if the file
// opened is the one checked; the caller checks the type on the handle.
func openFile(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if after, err := f.Stat(); err != nil || !os.SameFile(before, after) {
		f.Close()
		return nil, errors.New("the file changed while it was opened")
	}
	return f, nil
}
