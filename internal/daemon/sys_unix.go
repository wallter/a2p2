//go:build unix

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// checkOwner refuses a home directory owned by another user.
func checkOwner(fi os.FileInfo, home string) error {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is owned by another user", home)
	}
	return nil
}

// tryLock takes an exclusive lock on f without waiting.
func tryLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlock(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

func umask(mask int) int { return syscall.Umask(mask) }

// detach starts cmd in a session of its own.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
