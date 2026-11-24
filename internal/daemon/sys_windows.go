//go:build windows

package daemon

import (
	"errors"
	"os"
	"os/exec"
)

// ErrUnsupported is what a2p2 returns on Windows, where it builds but does
// not run: the home's permission checks, the lock and the socket are Unix.
var ErrUnsupported = errors.New("a2p2 does not support Windows yet; see the README")

// checkOwner fails on Windows, so every command that needs a home (all of
// them reach CheckHome) stops with ErrUnsupported before doing anything.
func checkOwner(os.FileInfo, string) error { return ErrUnsupported }

func tryLock(*os.File) error { return ErrUnsupported }

func unlock(*os.File) {}

func umask(int) int { return 0 }

func detach(*exec.Cmd) {}
