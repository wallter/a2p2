//go:build !windows

package node

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestADeletionThatFailedIsCompletedByTheNextIdleTick (B4): a withdraw's
// deletion of a copy fails (its directory refuses it); with nothing else
// happening in the room, the next file tick completes it.
// Requirements: L1, T5
func TestADeletionThatFailedIsCompletedByTheNextIdleTick(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root a read-only directory does not refuse a removal")
	}
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	msg, res := a.sendFiles(room, "alice", "x", writeSource(t, "x.txt", "deleted later"))
	b.pullAll(room, a)
	b.tickFiles()
	b.inbox(room, "agent-b")
	r, err := b.fetch(room, "agent-b", res.Files[0].SHA256)
	if err != nil || r.State != FetchHere {
		t.Fatalf("%+v %v", r, err)
	}
	dir := filepath.Dir(r.Path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	a.tick()
	if _, err := a.m.Withdraw(context.Background(), room, "alice", msg); err != nil {
		t.Fatal(err)
	}
	b.pullAll(room, a)
	b.tickFiles()
	if _, err := os.Lstat(r.Path); err != nil {
		t.Fatalf("the refused deletion happened anyway: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b.tickFiles() // nothing new in the room
	if _, err := os.Lstat(r.Path); !os.IsNotExist(err) {
		t.Fatalf("the idle tick did not complete the deletion: %v", err)
	}
}
