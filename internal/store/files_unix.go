//go:build !windows

package store

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// syncDir makes a rename or a removal in dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// dirSyncDurable: syncDir makes a removal durable here.
const dirSyncDurable = true

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

// openSource opens a path a person named for reading, once. Non-blocking,
// so a FIFO or a device does not hang the open; the caller refuses
// anything but a regular file from the handle's own information.
func openSource(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

// remoteOrDevice: on Unix every path is opened non-blocking and its type
// checked on the handle; nothing is refused by its name alone.
func remoteOrDevice(string) bool { return false }
