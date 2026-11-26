//go:build unix

package store

// The directories walkSource walks, on Unix: plain descriptors, each
// entry looked at with fstatat(AT_SYMLINK_NOFOLLOW) and opened with
// openat(O_NOFOLLOW) in the directory opened before it. os.Root is not
// used here: it follows a link that stays inside its directory by itself,
// after the look and before the caller can check anything, and from the
// walk's first directory "/" every link stays inside (D63, after the
// second review). So a link put in place of an entry after its look makes
// the open fail and is never followed, and the handle is checked against
// the entry looked at before a byte is read.

import (
	"errors"
	"io/fs"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// walkEntry is what a look at one entry found: its type, and its device
// and inode, which the handle opened for it must have.
type walkEntry struct {
	mode     fs.FileMode
	dev, ino uint64
}

// fdDir is a directory open for the walk.
type fdDir struct {
	f  *os.File // owns fd
	fd int
}

func retryEINTR(f func() error) error {
	for {
		if err := f(); !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// openWalkRoot opens the walk's first directory, the root of the volume.
func openWalkRoot(p string) (walkDir, error) {
	var fd int
	err := retryEINTR(func() (err error) {
		fd, err = unix.Open(p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		return err
	})
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: p, Err: err}
	}
	return &fdDir{f: os.NewFile(uintptr(fd), p), fd: fd}, nil
}

func entryOf(st *unix.Stat_t) walkEntry {
	e := walkEntry{dev: uint64(st.Dev), ino: uint64(st.Ino)}
	switch uint32(st.Mode) & unix.S_IFMT {
	case unix.S_IFREG:
	case unix.S_IFDIR:
		e.mode = fs.ModeDir
	case unix.S_IFLNK:
		e.mode = fs.ModeSymlink
	default:
		e.mode = fs.ModeDevice // a FIFO, a socket or a device: never opened
	}
	return e
}

func (d *fdDir) lstat(name string) (walkEntry, error) {
	var st unix.Stat_t
	err := retryEINTR(func() error { return unix.Fstatat(d.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW|atNoAutomount) })
	runtime.KeepAlive(d.f)
	if err != nil {
		return walkEntry{}, &os.PathError{Op: "lstat", Path: name, Err: err}
	}
	return entryOf(&st), nil
}

func (d *fdDir) readlink(name string) (string, error) {
	defer runtime.KeepAlive(d.f)
	for n := 256; n <= 1<<16; n *= 2 {
		b := make([]byte, n)
		var got int
		err := retryEINTR(func() (err error) { got, err = unix.Readlinkat(d.fd, name, b); return err })
		if err != nil {
			return "", &os.PathError{Op: "readlink", Path: name, Err: err}
		}
		if got < n {
			return string(b[:got]), nil
		}
	}
	return "", &os.PathError{Op: "readlink", Path: name, Err: unix.ENAMETOOLONG}
}

// openAt opens name in d without following a link at it, and keeps the
// descriptor only if it is the entry e looked at. A link there now makes
// the open itself fail (ELOOP; EMLINK on some systems; ENOTDIR for a
// directory's open), so its target is never opened. O_NONBLOCK: an open
// never waits, whatever is there now; once the handle is known to be the
// entry looked at, reads block as usual.
func (d *fdDir) openAt(name string, flags int, e walkEntry) (int, error) {
	var fd int
	err := retryEINTR(func() (err error) {
		fd, err = unix.Openat(d.fd, name, flags|unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NOCTTY, 0)
		return err
	})
	runtime.KeepAlive(d.f)
	switch {
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.EMLINK), errors.Is(err, unix.ENOTDIR):
		return -1, errSwapped
	case err != nil:
		return -1, &os.PathError{Op: "open", Path: name, Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || entryOf(&st) != e {
		unix.Close(fd)
		return -1, errSwapped
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		unix.Close(fd)
		return -1, &os.PathError{Op: "open", Path: name, Err: err}
	}
	return fd, nil
}

func (d *fdDir) openFile(name, full string, e walkEntry) (*os.File, error) {
	fd, err := d.openAt(name, 0, e)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), full), nil
}

func (d *fdDir) openDir(name, full string, e walkEntry) (walkDir, error) {
	fd, err := d.openAt(name, unix.O_DIRECTORY, e)
	if err != nil {
		return nil, err
	}
	return &fdDir{f: os.NewFile(uintptr(fd), full), fd: fd}, nil
}

func (d *fdDir) info() (fs.FileInfo, error) { return d.f.Stat() }
func (d *fdDir) close()                     { d.f.Close() }
