//go:build !windows

package store

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
)

// syncDirFails, set by a test, makes a directory sync fail.
var syncDirFails func(dir string) bool

// syncDir makes a rename or a removal in dir durable.
func syncDir(dir string) error {
	if syncDirFails != nil && syncDirFails(dir) {
		return errors.New("files: directory sync failed (test)")
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// dirSyncDurable: syncDir makes a removal durable here.
var dirSyncDurable = true // a variable so a test can take the Windows path

// oNoFollow makes an open fail on a link at the last component.
const oNoFollow = syscall.O_NOFOLLOW

// fileIdent is the identity of an open file: its device and inode.
func fileIdent(f *os.File) (string, error) {
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("files: no file identity on this system")
	}
	// the modification time too: a file system may give a new file the
	// number of one just removed, and linking or unlinking leaves it as is
	return fmt.Sprintf("%x:%x:%x", uint64(sys.Dev), uint64(sys.Ino), st.ModTime().UnixNano()), nil
}

// driveRemote: Unix has no drive letters.
func driveRemote(string) bool { return false }

// sourceRoots are the directories whose paths are refused by name, before
// any I/O: devices and kernel files (/dev/fd names another open file of
// the daemon), and automount points, where a lookup makes the machine
// connect out (automountRoots, per system).
var sourceRoots = append([]string{"/dev", "/proc", "/sys", "/net"}, automountRoots...)

// remoteOrDevice: a clean path at or under one of sourceRoots, compared
// without case (macOS volumes usually ignore it, and so can a Linux file
// system: vfat, or ext4 with casefold). On a Linux root that keeps case
// this also refuses a path such as /Net/x; that errs on the side of
// refusing.
func remoteOrDevice(p string) bool {
	for _, r := range sourceRoots {
		if len(p) >= len(r) && strings.EqualFold(p[:len(r)], r) && (len(p) == len(r) || p[len(r)] == '/') {
			return true
		}
	}
	return false
}

// linkCount is the number of names the open file f has.
func linkCount(f *os.File) int {
	st, err := f.Stat()
	if err != nil {
		return 0
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return int(sys.Nlink)
	}
	return 0
}

// linkTarget is a link's target as read: on Unix, as it is.
func linkTarget(t string) string { return t }
