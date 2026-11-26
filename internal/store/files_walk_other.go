//go:build !unix

package store

// The directories walkSource walks where there is no openat (Windows):
// os.Root, each entry looked at with Lstat and opened only if the handle
// is still that entry (os.SameFile). os.Root follows a link that stays
// inside its directory by itself, so a link swapped in after the look is
// opened before the identity check refuses it. Unverified on Windows: the
// open there is not checked before it happens, unlike on Unix (D63).

import (
	"io/fs"
	"os"
)

// walkEntry is what a look at one entry found.
type walkEntry struct {
	mode fs.FileMode
	fi   fs.FileInfo
}

type rootDir struct{ r *os.Root }

func openWalkRoot(p string) (walkDir, error) {
	r, err := os.OpenRoot(p)
	if err != nil {
		return nil, err
	}
	return rootDir{r}, nil
}

func (d rootDir) lstat(name string) (walkEntry, error) {
	fi, err := d.r.Lstat(name)
	if err != nil {
		return walkEntry{}, err
	}
	return walkEntry{mode: fi.Mode() & (fs.ModeType | fs.ModeIrregular), fi: fi}, nil
}

func (d rootDir) readlink(name string) (string, error) { return d.r.Readlink(name) }

func (d rootDir) openFile(name, _ string, e walkEntry) (*os.File, error) {
	f, err := d.r.OpenFile(name, os.O_RDONLY|oNonBlock, 0)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err != nil || !os.SameFile(st, e.fi) {
		f.Close()
		return nil, errSwapped
	}
	return f, nil
}

func (d rootDir) openDir(name, _ string, e walkEntry) (walkDir, error) {
	sub, err := d.r.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	if st, err := sub.Stat("."); err != nil || !os.SameFile(st, e.fi) {
		sub.Close()
		return nil, errSwapped
	}
	return rootDir{sub}, nil
}

func (d rootDir) info() (fs.FileInfo, error) { return d.r.Stat(".") }
func (d rootDir) close()                     { d.r.Close() }
