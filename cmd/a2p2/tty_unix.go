//go:build darwin || linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// isTerminal asks the terminal driver whether stdin is a terminal.
// ioctlGetTermios is in tty_darwin.go and tty_linux.go.
func isTerminal() bool { return isTerminalFile(os.Stdin) }

// isTerminalFile asks the terminal driver whether f is a terminal.
func isTerminalFile(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(ioctlGetTermios), uintptr(unsafe.Pointer(&t)))
	return errno == 0
}

// openBrowser opens url in the default browser. It says why when the opener
// cannot start or fails within two seconds; one still running then (an
// xdg-open that waits for the browser) is left to run.
func openBrowser(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	cmd := exec.Command(name, url)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	case <-time.After(2 * time.Second):
	}
	return nil
}
