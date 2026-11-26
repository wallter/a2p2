package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

// TestAWindowsPathOfAnyFormButADriveAndPlainNamesIsRefusedByNameBeforeAnyIO
// (F23): the Windows rule, run here on every system. \??\UNC\ reaches a
// remote host on Windows; a stream after a colon, NUL: and the newer
// device names open a device; none of them is the plain form.
// Requirements: L1, P3
func TestAWindowsPathOfAnyFormButADriveAndPlainNamesIsRefusedByNameBeforeAnyIO(t *testing.T) {
	for p, want := range map[string]bool{
		`\??\UNC\evil.example\s\report.txt`: true, `\??\C:\a.txt`: true, `\\server\share\a.txt`: true,
		`\\?\C:\a.txt`: true, `\\.\PhysicalDrive0`: true, `//server/share/a.txt`: true, `\/server\a`: true,
		`C:a.txt`: true, `\a.txt`: true, `C:\t\COM0`: true, `C:\t\LPT0.log`: true, `C:\t\COM¹`: true,
		`C:\t\CONIN$`: true, `C:\t\conout$.txt`: true, `C:\t\NUL:`: true, `C:\t\a.txt:s`: true,
		`C:\t\a.txt::$DATA`: true, `C:\NUL\a.txt`: true, `C:\tmp\con.txt`: true, `C:\tmp\COM1 .log`: true,
		`C:\t\a*.txt`: true, `C:\t\a?.txt`: true, `1:\a.txt`: true,
		`C:\tmp\a.txt`: false, `c:/tmp/a.txt`: false, `D:\work\console.txt`: false, `C:\t\COM10.txt`: false,
		`C:\Users\me\report (1).pdf`: false,
	} {
		if got := windowsRemoteOrDevice(p); got != want {
			t.Errorf("windowsRemoteOrDevice(%q) = %v, want %v", p, got, want)
		}
	}
}

// TestOneAuthorsUnansweredWantsReserveAtMostAQuarterOfTheRoomBudget (F10):
// carry and keep wants of one author with no byte yet reserve at most a
// quarter of the room budget, so its author, who never uploads, cannot
// hold the rest for a day. Another author, and a want this machine's own
// agent asked for, still fit.
// Requirements: L1
func TestOneAuthorsUnansweredWantsReserveAtMostAQuarterOfTheRoomBudget(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	s.SetFileBudgets(400, 4000)
	author, other := strings.Repeat("a", 64), strings.Repeat("b", 64)
	want := func(msg, sha, by string, size int64, capped bool) error {
		_, err := s.IssueWant(ctx, FileWant{Room: testRoom, Msg: strings.Repeat(msg, 32), Version: strings.Repeat(msg, 32),
			SHA256: strings.Repeat(sha, 64), Author: by, Size: size, At: time.Unix(0, 0), Capped: capped})
		return err
	}
	if err := want("1", "1", author, 100, true); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := want("2", "2", author, 1, true); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("over the author's quarter: %v", err)
	}
	if err := want("3", "3", other, 100, true); err != nil {
		t.Fatalf("another author: %v", err)
	}
	if err := want("4", "4", author, 100, false); err != nil {
		t.Fatalf("this machine's own fetch: %v", err)
	}
	// what is left fits this machine's own send
	if ok, err := s.FileFits(ctx, testRoom, 100); err != nil || !ok {
		t.Fatalf("own send: %v %v", ok, err)
	}
	if core.AuthorWantShare != 4 {
		t.Fatalf("share %d", core.AuthorWantShare)
	}
}

// TestWantsAnOlderBinaryReservedOverTheAuthorsShareStillLoadAndLapse
// (F10, compatibility): the cap is a rule for new wants only. A database
// whose wants of one author hold more than the share (written by 0.2.3)
// opens and lists them; only a new carry job for that author waits for
// room. Their messages are not in this store, so the sweep drops them as
// it always did; node's
// TestLiveWantsAnOlderBinaryIssuedOverTheAuthorsShareStillWorkAndLapse
// shows such wants of a message that is here living and lapsing.
// Requirements: L1, I5
func TestWantsAnOlderBinaryReservedOverTheAuthorsShareStillLoadAndLapse(t *testing.T) {
	s, path := openTest(t)
	ctx := context.Background()
	author := strings.Repeat("a", 64)
	at := time.Unix(1000, 0)
	for _, x := range []string{"1", "2", "3"} {
		if _, err := s.IssueWant(ctx, FileWant{Room: testRoom, Msg: strings.Repeat(x, 32), Version: strings.Repeat(x, 32),
			SHA256: strings.Repeat(x, 64), Author: author, Size: 100, At: at}); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetFileBudgets(400, 4000)
	var ws []FileWant
	if err := s.View(ctx, func(rd *Reader) (err error) { ws, err = rd.FileWants(testRoom); return err }); err != nil || len(ws) != 3 {
		t.Fatalf("wants %d %v", len(ws), err)
	}
	if _, err := s.IssueWant(ctx, FileWant{Room: testRoom, Msg: strings.Repeat("4", 32), Version: strings.Repeat("4", 32),
		SHA256: strings.Repeat("4", 64), Author: author, Size: 1, At: at, Capped: true}); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("a new carry job: %v", err)
	}
	if _, err := s.SweepFiles(ctx, testRoom, strings.Repeat("c", 64), at.Add(core.WantLife)); err != nil {
		t.Fatal(err)
	}
	if err := s.View(ctx, func(rd *Reader) (err error) { ws, err = rd.FileWants(testRoom); return err }); err != nil || len(ws) != 0 {
		t.Fatalf("wants after their life %d %v", len(ws), err)
	}
}

// TestAnAuthorsUnfinishedUploadsStillCountTheirWholeSizeInItsShare (F10;
// D63, after the second review): bytes uploaded are checked only once a
// file is whole, so an author that uploads all but one byte of garbage
// for each want frees nothing of its share, which was the way past the
// quarter to nearly the whole room budget. Wants issued at once never
// pass the share together; a part goes with its want when the want
// lapses; and once a want is gone its share is free again (the honest
// case: a file received in full drops its want).
// Requirements: L1
func TestAnAuthorsUnfinishedUploadsStillCountTheirWholeSizeInItsShare(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	s.SetFileBudgets(4000, 40000)
	author := strings.Repeat("a", 64)
	key := func(i int) (string, string) {
		x := string(rune('0' + i))
		return strings.Repeat(x, 32), strings.Repeat(x, 64)
	}
	want := func(i int, size int64) error {
		msg, sha := key(i)
		_, err := s.IssueWant(ctx, FileWant{Room: testRoom, Msg: msg, Version: msg, SHA256: sha, Author: author,
			Size: size, At: time.Unix(0, 0), Capped: true})
		return err
	}
	if err := want(1, 900); err != nil {
		t.Fatal(err)
	}
	msg, sha := key(1)
	p, err := s.ReservePart(ctx, testRoom, msg, sha, 900, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PartHave(ctx, p, 899); err != nil {
		t.Fatal(err)
	}
	// all 900 still counted of the 1000 share, as the room's usage counts
	// the part: 899 bytes received free nothing
	if err := want(2, 101); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("bytes received freed the want's share: %v", err)
	}
	if err := want(2, 100); err != nil {
		t.Fatalf("what is left of the share: %v", err)
	}
	// another author's wants, issued at once: each fits alone, three fit together
	other := strings.Repeat("b", 64)
	var wg sync.WaitGroup
	for i := 3; i < 9; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, h := key(i)
			_, _ = s.IssueWant(ctx, FileWant{Room: testRoom, Msg: m, Version: m, SHA256: h, Author: other,
				Size: 300, At: time.Unix(0, 0), Capped: true})
		}()
	}
	wg.Wait()
	var ws []FileWant
	if err := s.View(ctx, func(rd *Reader) (err error) { ws, err = rd.FileWants(testRoom); return err }); err != nil || len(ws) != 5 {
		t.Fatalf("wants: %d %v, want 2 and 3 issued at once", len(ws), err)
	}
	if err := s.LapseWant(ctx, testRoom, msg, sha, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	var has bool
	if err := s.View(ctx, func(rd *Reader) (err error) { _, has, err = rd.FilePart(testRoom, msg, sha); return err }); err != nil || has {
		t.Fatalf("the part outlived its want: %v %v", has, err)
	}
	if err := want(9, 900); err != nil {
		t.Fatalf("the share of a want that went: %v", err)
	}
}

// TestAFileOnADriveLetterMappedToAShareIsStagedAsALocalFile (F23,
// compatibility with 0.2.3): on Windows the handle of a file on a mapped
// drive reports \\server\share\...; that path is taken only when the path
// asked began with a drive letter that names a share the person mapped.
// A local drive whose handle path is a share (a link swapped in), a
// device path, and a UNC path asked directly are still refused. The rule
// runs here on every system; GetDriveTypeW is unverified.
// Requirements: L1, P3
func TestAFileOnADriveLetterMappedToAShareIsStagedAsALocalFile(t *testing.T) {
	mapped := func(root string) bool { return root == `Z:\` }
	for _, c := range []struct {
		resolved, shown string
		want            bool
	}{
		{`Z:\work\a.txt`, `\\server\share\work\a.txt`, true},
		{`C:\work\a.txt`, `\\server\share\work\a.txt`, false},
		{`\\server\share\a.txt`, `\\server\share\a.txt`, false},
		{`Z:\work\a.txt`, `\\?\GLOBALROOT\Device\x`, false},
		{`Z:\work\a.txt`, `\\.\PhysicalDrive0`, false},
		{`Z:\work\NUL`, `\\server\share\work\NUL`, false},
		{`Z:\work\a.txt`, `\\server\share\a.txt:s`, false},
	} {
		if got := mappedShare(c.resolved, c.shown, mapped); got != c.want {
			t.Errorf("mappedShare(%q, %q) = %v, want %v", c.resolved, c.shown, got, c.want)
		}
	}
}

// TestAStagedFileFromTheHomeIsKnownByItsPathOrItsKeyBytes (F33): what
// approval checks again for a request an older binary staged.
// Requirements: L1, P3
func TestAStagedFileFromTheHomeIsKnownByItsPathOrItsKeyBytes(t *testing.T) {
	s, dbPath := openTest(t)
	home := filepath.Dir(dbPath)
	if err := os.WriteFile(filepath.Join(home, "local.key"), []byte("local key bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := core.SHA256Hex([]byte("other"))
	for p, want := range map[string]bool{
		dbPath:                                         true,
		filepath.Join(home, "identity.key"):            true,
		filepath.Join(home, "blobs", "r", "x"):         true,
		strings.ToUpper(home[:2]) + home[2:] + "/x.db": true, // case is ignored
		filepath.Join(home, "files", "r", "copy.txt"):  false,
		filepath.Join(t.TempDir(), "report.txt"):       false,
		home + "-other/x":                              false,
	} {
		if got := s.StagedHomeFile(p, other); got != want {
			t.Errorf("StagedHomeFile(%q) = %v, want %v", p, got, want)
		}
	}
	if !s.StagedHomeFile(filepath.Join(t.TempDir(), "copy.bin"), core.SHA256Hex([]byte("local key bytes"))) {
		t.Error("the local key's bytes from another path were not known")
	}
}
