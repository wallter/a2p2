//go:build unix

package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/localapi"
)

// fakeDir is a directory's mode and owner, as Lstat would give them.
type fakeDir struct {
	mode     os.FileMode
	uid, gid uint32
}

func (f fakeDir) Name() string       { return "d" }
func (f fakeDir) Size() int64        { return 0 }
func (f fakeDir) Mode() os.FileMode  { return f.mode | os.ModeDir }
func (f fakeDir) ModTime() time.Time { return time.Time{} }
func (f fakeDir) IsDir() bool        { return true }
func (f fakeDir) Sys() any           { return &syscall.Stat_t{Uid: f.uid, Gid: f.gid} }

// GPT, second review: a directory above the home that its group can write
// stopped a stock macOS account (group staff) from starting. The rule is
// now: refused where another user can replace the home (another owner,
// others' write without the sticky bit, a group this user is not in);
// warned about where only a group this user is in can (a2p2 cannot tell
// whether anyone else is in it); silent for this user's own private group
// and for a sticky directory.
func TestADirectoryAboveTheHomeIsRefusedOnlyWhereAnotherUserCanReplaceIt(t *testing.T) {
	const me, mine, team, other = 501, 20, 30, 40
	for _, tc := range []struct {
		name    string
		dir     fakeDir
		private bool
		refuse  bool
		warn    bool
	}{
		{"mine, 0755", fakeDir{0o755, me, mine}, false, false, false},
		{"root's, 0755", fakeDir{0o755, 0, other}, false, false, false},
		{"another user's", fakeDir{0o755, 502, mine}, false, true, false},
		{"0777 without the sticky bit", fakeDir{0o777, me, mine}, false, true, false},
		{"0777 with the sticky bit", fakeDir{0o777 | os.ModeSticky, 0, 0}, false, false, false},
		{"my primary group's, shared (staff)", fakeDir{0o775, me, mine}, false, false, true},
		{"my primary group's, private", fakeDir{0o775, me, mine}, true, false, false},
		{"a supplementary group of mine", fakeDir{0o775, me, team}, true, false, true},
		{"a group I am not in", fakeDir{0o775, 0, other}, false, true, false},
	} {
		w := &walk{home: "/h", uid: me, gid: mine, groups: []int{team}, groupOK: tc.private}
		err := w.check("/d", tc.dir)
		if (err != nil) != tc.refuse || (len(w.warns) > 0) != tc.warn {
			t.Errorf("%s: err %v, warnings %q", tc.name, err, w.warns)
		}
	}
}

// Verifier, third review: a directory above the home counts only if
// another user can reach it. Under a directory of this user's or root's
// that gives others no search bit (~/Documents on macOS is 0700), a 0777
// directory cannot be changed by anyone but this user and root, and the
// home under it starts with no refusal and no warning. Each class of user
// is followed on its own: a group's members pass a directory of that group
// by its group bit, everyone else by its other bit.
func TestADirectoryAboveTheHomeCountsOnlyIfAnotherUserCanReachIt(t *testing.T) {
	const me, mine, team, other = 501, 20, 30, 40
	open := step{perm: 0o755, gid: mine}
	for _, tc := range []struct {
		name   string
		chain  []step
		dir    fakeDir
		refuse bool
		warn   bool
	}{
		{"0777 under a 0700 directory of mine", []step{open, {perm: 0o700, gid: mine}}, fakeDir{0o777, me, mine}, false, false},
		{"0777 under a 0711 directory of mine", []step{open, {perm: 0o711, gid: mine}}, fakeDir{0o777, me, mine}, true, false},
		{"0777 under 0755 directories", []step{open, open}, fakeDir{0o777, me, mine}, true, false},
		{"0777 under 0710 of its own group", []step{{perm: 0o710, gid: team}}, fakeDir{0o777, me, team}, false, true},
		{"0777 under 0710 of another group", []step{{perm: 0o710, gid: other}}, fakeDir{0o777, me, team}, true, false},
		{"0775 of a group I am not in, under 0701 of that group", []step{{perm: 0o701, gid: other}}, fakeDir{0o775, 0, other}, false, false},
		{"0775 of a group I am not in, under 0750 of that group", []step{{perm: 0o750, gid: other}}, fakeDir{0o775, 0, other}, true, false},
		{"another user's, under a 0700 directory of mine", []step{{perm: 0o700, gid: mine}}, fakeDir{0o755, 502, mine}, false, false},
		{"another user's, under 0755", []step{open}, fakeDir{0o755, 502, mine}, true, false},
		{"another user's, under another user's 0700", []step{{perm: 0o700, gid: other, foreign: true}}, fakeDir{0o755, 502, mine}, true, false},
	} {
		w := &walk{home: "/h", uid: me, gid: mine, groups: []int{team}, groupOK: false, chain: tc.chain}
		err := w.check("/d", tc.dir)
		if (err != nil) != tc.refuse || (len(w.warns) > 0) != tc.warn {
			t.Errorf("%s: err %v, warnings %q", tc.name, err, w.warns)
		}
	}
}

// The same on real directories: a 0700 directory of this user's, a 0777
// directory with no sticky bit in it, and a home there. It starts with no
// warning, and keeps working with the 0777 directory sticky. Once others
// can search the 0700 directory, the home is refused.
func TestAHomeUnderASharedDirectoryNoOtherUserCanReachStarts(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("as root every directory is root's")
	}
	parent := reachableTemp(t)
	private := filepath.Join(parent, "Documents")
	shared := filepath.Join(private, "programing")
	home := filepath.Join(shared, "a2p2", ".a2p2")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(private, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := PrepareHome(home); err != nil {
		t.Fatalf("under a 0777 directory no one else can reach: %v", err)
	}
	if w := HomeWarnings(home); len(w) != 0 {
		t.Fatalf("warnings: %q", w)
	}
	if err := os.Chmod(private, 0o711); err != nil {
		t.Fatal(err)
	}
	err := PrepareHome(home)
	if err == nil || !strings.Contains(err.Error(), shared+",") || !strings.Contains(err.Error(), "chmod o-w") {
		t.Fatalf("under a 0777 directory others can reach: %v", err)
	}
	if err := os.Chmod(shared, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := PrepareHome(home); err != nil {
		t.Fatalf("under a sticky directory others can reach: %v", err)
	}
}

// GPT, second review: the database of a user upgrading from 0.2.3 must
// stay reachable whatever a directory above it allows. Under a directory
// its group can change (staff on macOS), the daemon starts on the existing
// home and its messages, and status says why it warns. Under one every
// user can change, it refuses to start, naming the directory and the
// chmod, and changes nothing: after the chmod the same database opens
// with its messages.
func TestAnExistingHomeUnderAGroupWritableDirectoryStillStarts(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("as root every directory is root's")
	}
	old := privateGroup
	t.Cleanup(func() { privateGroup = old })
	privateGroup = func() bool { return false } // a shared primary group, as staff on macOS

	m := newMachineWith(t, "a", nil)
	room := call[localapi.Room](m, "room/create", localapi.RoomReq{Room: "team"}).ID
	call[localapi.SendResp](m, "send", localapi.SendReq{Agent: m.agent("ann"), Room: room, Text: "written before the upgrade"})
	m.stop()

	parent, err := os.MkdirTemp(sockTemp(), "a2p2g")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(parent) })
	if err := os.Chown(parent, -1, os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o775); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(parent, "h")
	if err := os.Rename(m.home, home); err != nil {
		t.Fatal(err)
	}
	m.home = home
	m.start()
	raw := call[json.RawMessage](m, "history", localapi.HistoryReq{Room: room})
	if !strings.Contains(string(raw), "written before the upgrade") {
		t.Fatalf("history after the start: %s", raw)
	}
	if w := HomeWarnings(home); len(w) != 1 || !strings.Contains(w[0], parent) || !strings.Contains(w[0], "chmod g-w") {
		t.Fatalf("warnings: %q", w)
	}
	m.stop()

	if err := os.Chmod(parent, 0o777); err != nil {
		t.Fatal(err)
	}
	err = RunWith(context.Background(), home, "test", RunOpts{})
	if err == nil || !strings.Contains(err.Error(), parent) || !strings.Contains(err.Error(), "chmod o-w") {
		t.Fatalf("a start under a directory every user can change: %v", err)
	}
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	m.start()
	raw = call[json.RawMessage](m, "history", localapi.HistoryReq{Room: room})
	if !strings.Contains(string(raw), "written before the upgrade") || len(HomeWarnings(home)) != 0 {
		t.Fatalf("history after the chmod: %s", raw)
	}
}
