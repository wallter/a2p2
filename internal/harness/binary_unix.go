//go:build !windows

package harness

import (
	"os"
	"syscall"
)

// fileOwner returns the user id that owns fi.
func fileOwner(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
