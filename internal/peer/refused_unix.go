//go:build !windows

package peer

import (
	"errors"
	"syscall"
)

// isRefused: the connection was refused (nothing listens there).
func isRefused(err error) bool { return errors.Is(err, syscall.ECONNREFUSED) }
