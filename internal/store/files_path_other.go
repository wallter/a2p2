//go:build !darwin && !linux && !windows

package store

import (
	"errors"
	"os"
	"path/filepath"
)

// handlePath, where the system cannot report the path of an open file:
// the name walkSource opened it by. That name has no link in it (the walk
// read every one and went on from its target) and each of its components
// was opened from the one before and checked to be the entry looked at,
// so the name is not looked up again: a second lookup could follow a link
// swapped in since, and reach a path the walk refused.
func handlePath(f *os.File) (string, error) {
	p := filepath.Clean(f.Name())
	if !filepath.IsAbs(p) {
		return "", errors.New("files: the opened file has no absolute name")
	}
	return p, nil
}

// automountRoots are automount points refused besides /net: none known.
var automountRoots []string
