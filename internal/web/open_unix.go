//go:build !windows

package web

import (
	"os"
	"syscall"
)

// openFile opens path for reading without following a final symbolic
// link and without blocking if it is a FIFO or a device; the caller checks
// the type on the handle.
func openFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
