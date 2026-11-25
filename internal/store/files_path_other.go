//go:build !darwin && !linux && !windows

package store

import (
	"errors"
	"os"
	"path/filepath"
)

// handlePath, where the system cannot report the path of an open file:
// the name resolved, accepted only if it is still the opened file.
func handlePath(f *os.File) (string, error) {
	p, err := filepath.EvalSymlinks(f.Name())
	if err == nil {
		p, err = filepath.Abs(p)
	}
	if err != nil {
		return "", err
	}
	a, err := f.Stat()
	if err != nil {
		return "", err
	}
	if b, err := os.Stat(p); err != nil || !os.SameFile(a, b) {
		return "", errors.New("files: the file changed while it was opened")
	}
	return p, nil
}
