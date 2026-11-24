//go:build unix

package daemon

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"syscall"
)

// localKeyRequired is false on Unix: the socket's mode and the home's
// permissions are the boundary (see localkey.go for Windows).
const localKeyRequired = false

// makeHome creates the state directory, private to this user.
func makeHome(home string) error { return os.MkdirAll(home, 0o700) }

// checkPrivate refuses a home directory owned by another user or that
// others could read.
func checkPrivate(fi os.FileInfo, home string) error {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is owned by another user", home)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is accessible to other users (mode %o); run: chmod 700 %s", home, fi.Mode().Perm(), home)
	}
	return nil
}

// tryLock takes an exclusive lock on f without waiting.
func tryLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlock(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

func umask(mask int) int { return syscall.Umask(mask) }

// startDetached starts cmd in a session of its own, so that it survives
// the caller's terminal.
func startDetached(cmd *exec.Cmd) (*exec.Cmd, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd, cmd.Start()
}

// listenLocal listens on the local API socket, readable and writable only
// by this user.
func listenLocal(sock string) (net.Listener, error) {
	old := umask(0o177)
	defer umask(old)
	return net.Listen("unix", sock)
}
