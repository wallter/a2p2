//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// isTerminal asks the console whether stdin is one. A pipe (an agent's
// shell, or a terminal such as mintty that is not a Windows console) is
// not, so decisions are not taken there; use Windows Terminal, the console
// or PowerShell.
func isTerminal() bool {
	var mode uint32
	return syscall.GetConsoleMode(syscall.Handle(os.Stdin.Fd()), &mode) == nil
}

// openURL opens url in the default browser. rundll32 hands it to the
// registered handler as is; "cmd /c start" would cut it at characters cmd
// treats as special.
func openURL(url string) {
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}
