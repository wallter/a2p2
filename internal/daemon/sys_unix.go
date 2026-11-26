//go:build unix

package daemon

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
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
// user can change: one owned by a user other than this one or root, one
// that others may write without the sticky bit (as /tmp has), or one its
// group may write when this user is not in that group. Such a user could
// move the home aside and put a directory with a socket of their own at
// its path, which a client already running would then dial. A directory
// writable by a group this user is in (staff on macOS, a team's group on
// Linux) is not refused, since a2p2 cannot tell whether anyone else is in
// it: it is returned as a warning, which the daemon logs and status and
// doctor print. This user's own private group (the primary group, named as
// the user is, the usual case on Linux) gives no warning. The path is
// walked one name at a time without following links; each link must be
// this user's or root's (one in a sticky directory could be replaced by
// its owner), and its target is walked the same way from the directory
// that holds it, so every directory a lookup of the path passes through is
// checked. The path is walked as given, not cleaned: a ".." after a link
// leads where the link's target leads, which a cleaned path does not say.
// The home itself is walked too (CheckHome has checked it already, more
// strictly), since a home given as "a/l/.." is not under its textual
// parent. A directory another user cannot reach is not refused or warned
// about: one under a directory of this user's or root's that gives that
// user no search bit (~/Documents on macOS is 0700) cannot be changed by
// them, whatever its own mode says. The walk keeps, for each directory on
// the way down, the bits that say who may pass through it (see reachable).
func checkAncestors(home string) (warns []string, err error) {
	abs := home
	if !filepath.IsAbs(home) {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		abs = wd + "/" + home
	}
	groups, _ := os.Getgroups() // none known: a directory only another group can write is refused
	w := &walk{home: home, uid: os.Getuid(), gid: os.Getgid(), groups: groups, groupOK: privateGroup()}
	if _, err := w.resolve("/", abs); err != nil {
		return nil, err
	}
	return w.warns, nil
}

// privateGroup is ownPrivateGroup; tests replace it.
var privateGroup = ownPrivateGroup

// ownPrivateGroup reports whether this user's primary group is a private
// one, named as the user is. Without a name for either (a static binary
// and an account from a directory service, say), it is not, and a
// directory that group can write is only warned about.
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
	groups   []int // this user's supplementary groups
	groupOK  bool  // the primary group is private to this user
	links    int
	warns    []string
	chain    []step // the directories of the current one's path, from the root, with no link
}

// step is a directory on the way down, as far as passing through it goes.
type step struct {
	perm    os.FileMode
	gid     int
	foreign bool // owned by a user other than this one and root
}

// reachable reports whether a user other than this one and root could
// search every directory of the current path. With h at or above zero it
// asks about such a user in group h (in) or not in it (!in); with h below
// zero, about any such user. Where a directory's group is another one, the
// user may or may not be in it, so either its group or its other search
// bit lets them pass; the owner of a directory owned by another user may
// pass by its owner bit too.
func (w *walk) reachable(h int, in bool) bool {
	for _, s := range w.chain {
		var x os.FileMode
		switch {
		case h >= 0 && s.gid == h && in:
			x = 0o010
		case h >= 0 && s.gid == h:
			x = 0o001
		default:
			x = 0o011
		}
		if s.foreign {
			x |= 0o100
		}
		if s.perm&x == 0 {
			return false
		}
	}
	return true
}

// push records dir, just checked, as the next directory of the path.
func (w *walk) push(fi os.FileInfo) {
	s := step{perm: fi.Mode().Perm()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		s.gid = int(st.Gid)
		s.foreign = int(st.Uid) != w.uid && st.Uid != 0
	}
	w.chain = append(w.chain, s)
}

// inGroup reports whether this user is in group gid.
func (w *walk) inGroup(gid int) bool {
	return gid == w.gid || slices.Contains(w.groups, gid)
}

// maxLinks bounds the links followed in one walk, as the kernel does.
const maxLinks = 40

// resolve walks p from cur, a directory with no link in its path that was
// checked already, and returns the directory p leads to.
func (w *walk) resolve(cur, p string) (string, error) {
	if filepath.IsAbs(p) {
		cur = "/"
		w.chain = w.chain[:0]
		if err := w.checkDir(cur); err != nil {
			return "", err
		}
	}
	for _, name := range strings.Split(p, "/") {
		switch name {
		case "", ".":
			continue
		case "..":
			if cur != "/" {
				cur = filepath.Dir(cur) // checked already, on the way down
				w.chain = w.chain[:len(w.chain)-1]
			}
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
		w.push(fi)
		cur = next
	}
	return cur, nil
}

func (w *walk) checkDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if err := w.check(dir, fi); err != nil {
		return err
	}
	w.push(fi)
	return nil
}

// check refuses the directory dir, above the home, if another user can
// change what is in it, and notes it if a group this user is in can. Only
// users who can reach dir count (see reachable); w.chain holds the
// directories above dir.
func (w *walk) check(dir string, fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	mode, sticky := fi.Mode().Perm(), fi.Mode()&os.ModeSticky != 0
	gid := int(st.Gid)
	if int(st.Uid) != w.uid && st.Uid != 0 {
		if !w.reachable(-1, false) {
			return nil // its owner cannot reach it either
		}
		return fmt.Errorf("%s, a directory above the a2p2 home %s, is owned by another user; "+
			"keep the home under directories only you and root control", dir, w.home)
	}
	if sticky || mode&0o022 == 0 {
		return nil
	}
	if mode&0o002 != 0 && w.reachable(gid, false) {
		return fmt.Errorf("%s, a directory above the a2p2 home %s, can be changed by other users (mode %o); "+
			"run: chmod o-w %s, or move the home", dir, w.home, mode, dir)
	}
	switch {
	case mode&0o020 == 0 || !w.reachable(gid, true):
	case !w.inGroup(gid):
		return fmt.Errorf("%s, a directory above the a2p2 home %s, can be changed by other users: "+
			"its group %s, which you are not in, can write it (mode %o); run: chmod g-w %s, or move the home",
			dir, w.home, groupName(gid), mode, dir)
	case gid == w.gid && w.groupOK:
	default:
		w.warns = append(w.warns, fmt.Sprintf("%s, a directory above the a2p2 home, can be changed by every member of its group %s (mode %o); "+
			"anyone else in that group could replace the home. To close this, run: chmod g-w %s", dir, groupName(gid), mode, dir))
	}
	return nil
}

// groupName is the group's name and id, or its id.
func groupName(gid int) string {
	if g, err := user.LookupGroupId(strconv.Itoa(gid)); err == nil && g.Name != "" {
		return g.Name + " (" + strconv.Itoa(gid) + ")"
	}
	return strconv.Itoa(gid)
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
