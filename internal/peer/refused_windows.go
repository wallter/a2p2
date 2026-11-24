//go:build windows

package peer

import (
	"errors"
	"syscall"
)

// wsaeConnRefused is WSAECONNREFUSED, which Windows returns where Unix
// returns ECONNREFUSED.
const wsaeConnRefused = syscall.Errno(10061)

// isRefused: the connection was refused (nothing listens there).
func isRefused(err error) bool {
	return errors.Is(err, wsaeConnRefused) || errors.Is(err, syscall.ECONNREFUSED)
}
