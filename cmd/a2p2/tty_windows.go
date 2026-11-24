//go:build windows

package main

// isTerminal is never asked on Windows in practice: every command that
// could ask a person stops first with daemon.ErrUnsupported.
func isTerminal() bool { return false }
