//go:build darwin || linux

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// isTerminal asks the terminal driver whether stdin is a terminal.
// ioctlGetTermios is in tty_darwin.go and tty_linux.go.
func isTerminal() bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, os.Stdin.Fd(), uintptr(ioctlGetTermios), uintptr(unsafe.Pointer(&t)))
	return errno == 0
}
