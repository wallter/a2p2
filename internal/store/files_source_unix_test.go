//go:build !windows

package store

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/core"
)

// TestASendCannotAttachTheMachineKeyOrDatabaseEvenThroughAHardLink (F33,
// F40): a2p2's own files are not staged: the machine key, the local key,
// the database and its journal by name, through a symbolic link or a hard
// link elsewhere (to a file directly in the home or a nested blob), and
// anything else under the home but agents' copies.
// An agent's copy under files/ may be sent on, and so may any other file.
// Requirements: L1, P3
func TestASendCannotAttachTheMachineKeyOrDatabaseEvenThroughAHardLink(t *testing.T) {
	s, dbPath := openTest(t)
	home := filepath.Dir(dbPath)
	for _, n := range []string{"identity.key", "local.key"} {
		if err := os.WriteFile(filepath.Join(home, n), []byte("secret "+n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(home, "blobs", testRoom), 0o700); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(home, "blobs", testRoom, strings.Repeat("a", 64))
	if err := os.WriteFile(blob, []byte("held bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "files", testRoom), 0o700); err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(home, "files", testRoom, "report.txt")
	if err := os.WriteFile(copied, []byte("a copy for an agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	hard := filepath.Join(elsewhere, "build.log")
	if err := os.Link(filepath.Join(home, "identity.key"), hard); err != nil {
		t.Fatal(err)
	}
	hardBlob := filepath.Join(elsewhere, "data.bin") // a nested file of the home
	if err := os.Link(blob, hardBlob); err != nil {
		t.Fatal(err)
	}
	soft := filepath.Join(elsewhere, "notes.txt")
	if err := os.Symlink(filepath.Join(home, "local.key"), soft); err != nil {
		t.Fatal(err)
	}
	refused := []string{filepath.Join(home, "identity.key"), filepath.Join(home, "local.key"), dbPath, hard, hardBlob, soft, blob,
		filepath.Join(home, ".", "identity.key")}
	if _, err := os.Stat(dbPath + "-wal"); err == nil {
		refused = append(refused, dbPath+"-wal")
	}
	req := "q_" + strings.Repeat("6", 32)
	for i, p := range refused {
		if _, err := s.StageFile(req, i%core.MaxFiles, p, core.MaxFileSize); !errors.Is(err, ErrHomeFile) {
			t.Errorf("%s: %v, want the home refusal", p, err)
		}
	}
	plain := filepath.Join(elsewhere, "plain.txt")
	if err := os.WriteFile(plain, []byte("plain bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i, p := range []string{copied, plain} {
		st, err := s.StageFile(req, i, p, core.MaxFileSize)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if st.Links != 0 {
			t.Fatalf("%s: %d links", p, st.Links)
		}
	}
}

// TestAPathUnderAnAutomountOrDevicePointIsRefusedBeforeAnyIO (F23 on
// Unix, audit-files 5): /dev, /proc, /sys, /net and, on macOS, the other
// automount points are refused by name; so is a link that leads there,
// before the system looks at its target.
// Requirements: L1, P3
func TestAPathUnderAnAutomountOrDevicePointIsRefusedBeforeAnyIO(t *testing.T) {
	cases := map[string]bool{
		"/net/evil.example/s/log.txt": true, "/NET/evil.example/x": true, "/net": true, "/dev/fd/3": true,
		"/proc/self/fd/3": true, "/sys/kernel/x": true, "//dev/null": true, "/tmp/../dev/zero": true,
		"/network2/a.txt": false, "/devices/a.txt": false, "/tmp/a.txt": false,
	}
	if runtime.GOOS == "darwin" {
		cases["/Network/Servers/evil/x"] = true
		cases["/home/someone/x"] = true
		cases["/System/Volumes/Data/net/evil/x"] = true
	}
	for p, want := range cases {
		if got := RemoteOrDevice(p); got != want {
			t.Errorf("RemoteOrDevice(%q) = %v, want %v", p, got, want)
		}
	}
	s, _ := openTest(t)
	dir := t.TempDir()
	viaDir := filepath.Join(dir, "share")
	if err := os.Symlink("/net/evil.example/s", viaDir); err != nil {
		t.Fatal(err)
	}
	last := filepath.Join(dir, "log.txt")
	if err := os.Symlink("/dev/fd/0", last); err != nil {
		t.Fatal(err)
	}
	var looked []string
	sourceLooked = func(p string) { looked = append(looked, p) }
	defer func() { sourceLooked = nil }()
	for i, p := range []string{filepath.Join(viaDir, "log.txt"), last} {
		if _, err := s.StageFile("q_"+strings.Repeat("7", 32), i, p, core.MaxFileSize); !errors.Is(err, ErrRemoteOrDevice) {
			t.Errorf("%s: %v, want remote_or_device", p, err)
		}
	}
	for _, p := range looked {
		if RemoteOrDevice(p) {
			t.Errorf("looked at %s", p)
		}
	}
}

// TestStagingNeverFollowsALinkAtTheLastComponentWhenItOpens (PROTOCOL
// 10.11): links are read and checked one by one, and the file staged is
// the one that was checked. A link is followed by its checked target, and
// a link or another file put at the last component after its check is
// refused, through the whole of StageFile.
// Requirements: L1, P3
func TestStagingNeverFollowsALinkAtTheLastComponentWhenItOpens(t *testing.T) {
	s, _ := openTest(t)
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(real, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other.txt")
	if err := os.WriteFile(other, []byte("other bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	req := "q_" + strings.Repeat("9", 32)
	st, err := s.StageFile(req, 0, link, core.MaxFileSize)
	want, _ := filepath.EvalSymlinks(real)
	if err != nil || st.Path != want || st.SHA256 != core.SHA256Hex([]byte("bytes")) {
		t.Fatalf("staged %+v %v, want %q", st, err, want)
	}
	for i, swap := range []func(p string) error{
		func(p string) error { return os.Symlink("other.txt", p) },
		func(p string) error { return os.Link(other, p) },
	} {
		asked := filepath.Join(dir, "asked"+strings.Repeat("x", i)+".txt")
		if err := os.WriteFile(asked, []byte("checked bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		walked, _ := filepath.EvalSymlinks(asked)
		sourceChecked = func(p string) {
			if p != walked {
				return
			}
			if err := os.Remove(asked); err != nil {
				t.Error(err)
			}
			if err := swap(asked); err != nil {
				t.Error(err)
			}
		}
		_, err := s.StageFile(req, i+1, asked, core.MaxFileSize)
		sourceChecked = nil
		if !errors.Is(err, errSwapped) {
			t.Errorf("swap %d after the check: %v, want refused", i, err)
		}
	}
}

// TestADirectorySwappedOnTheWayAfterItsCheckIsRefused (F23, F37): each
// component is looked up in the directory opened before it, so a
// directory replaced by a link after its check, to another directory or
// to an automount point, is refused and never followed.
// Requirements: L1, P3
func TestADirectorySwappedOnTheWayAfterItsCheckIsRefused(t *testing.T) {
	s, _ := openTest(t)
	base := t.TempDir()
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(elsewhere, "f.txt"), []byte("elsewhere"), 0o600); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(base, "sibling")
	if err := os.Mkdir(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "f.txt"), []byte("sibling"), 0o600); err != nil {
		t.Fatal(err)
	}
	// a relative link stays in the directory checked and is followed by
	// the lookup, so the identity check refuses it; an absolute one is
	// never followed by a lookup at all
	for i, target := range []string{"sibling", elsewhere, "/net/evil.example/s"} {
		sub := filepath.Join(base, "sub"+strings.Repeat("x", i))
		if err := os.Mkdir(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "f.txt"), []byte("checked"), 0o600); err != nil {
			t.Fatal(err)
		}
		walked, _ := filepath.EvalSymlinks(sub)
		sourceChecked = func(p string) {
			if p != walked {
				return
			}
			if err := os.RemoveAll(sub); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(target, sub); err != nil {
				t.Error(err)
			}
		}
		_, err := s.StageFile("q_"+strings.Repeat("5", 32), i, filepath.Join(sub, "f.txt"), core.MaxFileSize)
		sourceChecked = nil
		if err == nil || i == 0 && !errors.Is(err, errSwapped) {
			t.Errorf("%s swapped in: %v, want refused", target, err)
		}
	}
}

// TestAFileWithSeveralNamesIsStagedWithItsLinkCount (F36): the count goes
// with the staged file so that the person is told.
// Requirements: L1, P3
func TestAFileWithSeveralNamesIsStagedWithItsLinkCount(t *testing.T) {
	s, _ := openTest(t)
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(a, []byte("two names"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(a, filepath.Join(dir, "b.txt")); err != nil {
		t.Fatal(err)
	}
	st, err := s.StageFile("q_"+strings.Repeat("8", 32), 0, a, core.MaxFileSize)
	if err != nil || st.Links != 2 {
		t.Fatalf("%+v %v", st, err)
	}
}
