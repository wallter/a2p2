//go:build darwin

package store

import (
	"bytes"
	"os"
	"syscall"
	"unsafe"
)

// handlePath is the path the system reports for the open file f.
func handlePath(f *os.File) (string, error) {
	buf := make([]byte, 1024) // MAXPATHLEN
	rc, err := f.SyscallConn()
	if err != nil {
		return "", err
	}
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETPATH, uintptr(unsafe.Pointer(&buf[0])))
	}); err != nil {
		return "", err
	}
	if errno != 0 {
		return "", errno
	}
	if i := bytes.IndexByte(buf, 0); i >= 0 {
		buf = buf[:i]
	}
	return string(buf), nil
}

// automountRoots are macOS's automount points (auto_master): a lookup
// under them can make the machine connect to another host. /home is
// auto_home here, not where people's files are (/Users).
var automountRoots = []string{"/Network", "/home",
	"/System/Volumes/Data/net", "/System/Volumes/Data/home", "/System/Volumes/Data/Network"}

// atNoAutomount: no such flag here; the name rule is the guard.
const atNoAutomount = 0
