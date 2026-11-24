//go:build !windows

package store

import (
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

// openSource opens a path a person named for reading, once. Non-blocking,
// so a FIFO or a device does not hang the open; the caller refuses
// anything but a regular file from the handle's own information.
func openSource(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
