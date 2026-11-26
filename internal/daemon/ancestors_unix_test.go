//go:build unix

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// F53: a user who can change a directory above the home can move the home
// aside and put a directory of their own, with a socket, at its path; a
// client that checked the home once would then dial that socket. A home
// under such a directory is refused, by the client and the daemon.
func TestAHomeUnderADirectoryOthersCanChangeIsRefused(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("as root every directory is root's")
	}
	shared := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777); err != nil { // no sticky bit
		t.Fatal(err)
	}
	home := filepath.Join(shared, "team", ".a2p2")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]func(string) error{"CheckHome": CheckHome, "PrepareHome": PrepareHome} {
		err := f(home)
		if err == nil || !strings.Contains(err.Error(), shared) || !strings.Contains(err.Error(), "other users") {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// The honest cases: a directory others can write with the sticky bit (as
// /tmp), and a home reached through a link, still work. A directory
// writable by this user's primary group works only when that group is
// private to the user (named as the user is, as on most Linux systems); a
// shared one, such as staff on macOS, holds other users and is refused.
func TestAHomeUnderAStickyOrOwnGroupDirectoryWorks(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("as root every directory is root's")
	}
	base := t.TempDir()
	sticky := filepath.Join(base, "tmp")
	own := filepath.Join(sticky, "group")
	if err := os.MkdirAll(own, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sticky, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(own, -1, os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(own, 0o770); err != nil { // the group is this user's primary group
		t.Fatal(err)
	}
	home := filepath.Join(own, ".a2p2")
	err := PrepareHome(home)
	if ownPrivateGroup() {
		if err != nil {
			t.Fatal(err)
		}
	} else if err == nil || !strings.Contains(err.Error(), "other users") {
		t.Fatalf("a home under a directory a shared group can change: %v", err)
	}
	if err := os.Chmod(own, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := PrepareHome(home); err != nil {
		t.Fatalf("under a sticky directory: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(own, link); err != nil {
		t.Fatal(err)
	}
	if err := CheckHome(filepath.Join(link, ".a2p2")); err != nil {
		t.Fatalf("through a link: %v", err)
	}
	rel := filepath.Join(base, "rel")
	if err := os.Symlink("tmp/./x/../group", rel); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(sticky, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := CheckHome(filepath.Join(rel, ".a2p2")); err != nil {
		t.Fatalf("through a relative link: %v", err)
	}
}

// A link into a directory others can change is refused too, wherever the
// link or the directory is: the walk follows each link from the directory
// that holds it. Each case puts a 0777 directory (no sticky bit) only on
// a path that a lookup of the home passes through and that neither the
// home's path as given nor its fully resolved path names.
func TestAHomeReachedThroughALinkIntoASharedDirectoryIsRefused(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("as root every directory is root's")
	}
	mkdir := func(p string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	link := func(target, at string) {
		t.Helper()
		if err := os.Symlink(target, at); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("the link's target is under a shared directory", func(t *testing.T) {
		// mine/work -> shared/x: the given path has mine, the resolved one shared.
		base := t.TempDir()
		mkdir(filepath.Join(base, "shared", "x", ".a2p2"), 0o700)
		mkdir(filepath.Join(base, "shared"), 0o777)
		mkdir(filepath.Join(base, "mine"), 0o700)
		link(filepath.Join(base, "shared", "x"), filepath.Join(base, "mine", "work"))
		home := filepath.Join(base, "mine", "work", ".a2p2")
		if err := CheckHome(home); err == nil || !strings.Contains(err.Error(), "shared") {
			t.Fatalf("CheckHome: %v", err)
		}
	})
	t.Run("a link in a chain is held by a shared directory", func(t *testing.T) {
		// a/l1 -> b/l2 -> c/d: b is on neither the given nor the resolved path,
		// and whoever can write b can point l2 elsewhere.
		base := t.TempDir()
		mkdir(filepath.Join(base, "c", "d", ".a2p2"), 0o700)
		mkdir(filepath.Join(base, "b"), 0o777)
		mkdir(filepath.Join(base, "a"), 0o700)
		link(filepath.Join(base, "c", "d"), filepath.Join(base, "b", "l2"))
		link(filepath.Join(base, "b", "l2"), filepath.Join(base, "a", "l1"))
		home := filepath.Join(base, "a", "l1", ".a2p2")
		err := CheckHome(home)
		if err == nil || !strings.Contains(err.Error(), filepath.Join("b")+",") {
			t.Fatalf("CheckHome: %v", err)
		}
		// The same chain with b private works.
		if err := os.Chmod(filepath.Join(base, "b"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := CheckHome(home); err != nil {
			t.Fatalf("a private chain: %v", err)
		}
	})
	t.Run("a '..' after a link leads into a shared directory", func(t *testing.T) {
		// mine/l -> shared/x/sub: mine/l/../.a2p2 is shared/x/.a2p2 to the
		// kernel, while the path cleaned as text is mine/.a2p2.
		base := t.TempDir()
		mkdir(filepath.Join(base, "shared", "x", "sub"), 0o700)
		mkdir(filepath.Join(base, "shared", "x", ".a2p2"), 0o700)
		mkdir(filepath.Join(base, "shared"), 0o777)
		mkdir(filepath.Join(base, "mine"), 0o700)
		link(filepath.Join(base, "shared", "x", "sub"), filepath.Join(base, "mine", "l"))
		home := filepath.Join(base, "mine", "l") + "/../.a2p2"
		if err := CheckHome(home); err == nil || !strings.Contains(err.Error(), "shared") {
			t.Fatalf("CheckHome: %v", err)
		}
		// The same home given relative to the working directory.
		t.Chdir(base)
		if err := CheckHome("mine/l/../.a2p2"); err == nil || !strings.Contains(err.Error(), "shared") {
			t.Fatalf("CheckHome, relative: %v", err)
		}
		// With shared private, the same path works.
		if err := os.Chmod(filepath.Join(base, "shared"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := CheckHome(home); err != nil {
			t.Fatalf("a private target: %v", err)
		}
	})
}

// A loop of links above the home is an error, not a hang.
func TestALoopOfLinksAboveTheHomeIsRefused(t *testing.T) {
	base := t.TempDir()
	if err := os.Symlink(filepath.Join(base, "y"), filepath.Join(base, "x")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "x"), filepath.Join(base, "y")); err != nil {
		t.Fatal(err)
	}
	if err := CheckHome(filepath.Join(base, "x", ".a2p2")); err == nil {
		t.Fatal("a home under a loop of links was accepted")
	}
}
