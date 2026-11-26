//go:build linux

package store

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// atNoAutomount: the walk's look at an entry does not mount an automount
// point there (its open still does; the name rule is the guard).
const atNoAutomount = unix.AT_NO_AUTOMOUNT

// handlePath is the path the system reports for the open file f.
func handlePath(f *os.File) (string, error) {
	return os.Readlink("/proc/self/fd/" + strconv.FormatUint(uint64(f.Fd()), 10))
}

// automountRoots are automount points refused besides /net: none by
// default on Linux, where /home holds people's files.
var automountRoots []string
