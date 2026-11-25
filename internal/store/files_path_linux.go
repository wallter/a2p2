//go:build linux

package store

import (
	"os"
	"strconv"
)

// handlePath is the path the system reports for the open file f.
func handlePath(f *os.File) (string, error) {
	return os.Readlink("/proc/self/fd/" + strconv.FormatUint(uint64(f.Fd()), 10))
}
