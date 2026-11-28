//go:build unix

package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// Every command that talks to the daemon checks the directories above its
// home. Whether this user's primary group is private to it is asked of
// the directory service (about 1.4 ms on macOS), and the answer matters
// only for a directory of that group that the group can write; it is
// asked then, once per walk. Each reachable group-writable ancestor
// still gives its own warning.
func TestTheHomeCheckAsksForTheUsersGroupOnlyWhenADirectoryNeedsIt(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("as root every directory is root's")
	}
	old := privateGroup
	t.Cleanup(func() { privateGroup = old })
	asked := 0
	privateGroup = func() bool { asked++; return false } // a shared primary group, as staff on macOS

	parent, err := os.MkdirTemp(sockTemp(), "a2p2g")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(parent) })
	child := filepath.Join(parent, "child")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(child, "h")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{parent, child} {
		if err := os.Chown(dir, -1, os.Getgid()); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := CheckHome(home); err != nil {
		t.Fatal(err)
	}
	if w := HomeWarnings(home); len(w) != 0 || asked != 0 {
		t.Fatalf("under directories no group can write: warnings %q, the user's group asked for %d times, want none", w, asked)
	}
	for _, dir := range []string{parent, child} {
		if err := os.Chmod(dir, 0o775); err != nil {
			t.Fatal(err)
		}
	}
	if w, err := checkAncestors(home); err != nil || len(w) != 2 || asked != 1 {
		t.Fatalf("under two directories their group can write: warnings %q (%v), the user's group asked for %d times, want 2 and 1", w, err, asked)
	}
}
