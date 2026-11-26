//go:build unix

package daemon

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
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

// checkAncestors refuses a home with a directory above it that another
// user could change: one owned by a user other than this one or root, or
// one that others may write without the sticky bit (as /tmp has). Such a
// user could move the home aside and put a directory with a socket of
// their own at its path, which a client already running would then dial.
// A directory writable by its group is accepted only when the group is
// this user's own private group: this user's primary group, named as the
// user is (the usual case on Linux); a shared primary group, such as staff
// on macOS, holds other users. The path is walked one name at a time
// without following links; each link must be this user's or root's (one
// in a sticky directory could be replaced by its owner), and its target is
// walked the same way from the directory that holds it, so every
// directory a lookup of the path passes through is checked. The path is
// walked as given, not cleaned: a ".." after a link leads where the link's
// target leads, which a cleaned path does not say. The home itself is
// walked too (CheckHome has checked it already, more strictly), since a
// home given as "a/l/.." is not under its textual parent.
func checkAncestors(home string) error {
	abs := home
	if !filepath.IsAbs(home) {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		abs = wd + "/" + home
	}
	w := &walk{home: home, uid: os.Getuid(), gid: os.Getgid(), groupOK: ownPrivateGroup()}
	_, err := w.resolve("/", abs)
	return err
}

// ownPrivateGroup reports whether this user's primary group is a private
// one, named as the user is. Without a name for either, it is not.
func ownPrivateGroup() bool {
	u, err := user.Current()
	if err != nil || u.Username == "" {
		return false
	}
	g, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	return err == nil && g.Name == u.Username
}

type walk struct {
	home     string
	uid, gid int
	groupOK  bool
	links    int
}

// maxLinks bounds the links followed in one walk, as the kernel does.
const maxLinks = 40

// resolve walks p from cur, a directory with no link in its path that was
// checked already, and returns the directory p leads to.
func (w *walk) resolve(cur, p string) (string, error) {
	if filepath.IsAbs(p) {
		cur = "/"
		if err := w.checkDir(cur); err != nil {
			return "", err
		}
	}
	for _, name := range strings.Split(p, "/") {
		switch name {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur) // checked already, on the way down
			continue
		}
		next := filepath.Join(cur, name)
		fi, err := os.Lstat(next)
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			if w.links++; w.links > maxLinks {
				return "", fmt.Errorf("%s: too many links above the a2p2 home %s", next, w.home)
			}
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != w.uid && st.Uid != 0 {
				return "", fmt.Errorf("%s, a link above the a2p2 home %s, is owned by another user; "+
					"keep the home under directories and links only you and root control", next, w.home)
			}
			target, err := os.Readlink(next)
			if err != nil {
				return "", err
			}
			if cur, err = w.resolve(cur, target); err != nil {
				return "", err
			}
			continue
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("%s, above the a2p2 home %s, is not a directory", next, w.home)
		}
		if err := w.check(next, fi); err != nil {
			return "", err
		}
		cur = next
	}
	return cur, nil
}

func (w *walk) checkDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	return w.check(dir, fi)
}

// check refuses the directory dir, above the home, if another user could
// change what is in it.
func (w *walk) check(dir string, fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	mode, sticky := fi.Mode().Perm(), fi.Mode()&os.ModeSticky != 0
	switch {
	case int(st.Uid) != w.uid && st.Uid != 0:
		return fmt.Errorf("%s, a directory above the a2p2 home %s, is owned by another user; "+
			"keep the home under directories only you and root control", dir, w.home)
	case !sticky && (mode&0o002 != 0 || mode&0o020 != 0 && (int(st.Gid) != w.gid || !w.groupOK)):
		return fmt.Errorf("%s, a directory above the a2p2 home %s, can be changed by other users (mode %o); "+
			"run: chmod go-w %s, or move the home", dir, w.home, mode, dir)
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
