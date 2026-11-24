package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

var fileKey = testKey("files")

// testFiles is a message with one file whose manifest commits to it.
type testFiles struct {
	man     []byte
	m       core.Manifest
	data    []byte
	sha     string
	msg     *core.VerifiedRecord
	msha    string
	authorN string
}

func newTestFiles(t *testing.T, s *Store, seq int64, prev string, data string) testFiles {
	t.Helper()
	tf := testFiles{data: []byte(data), sha: core.SHA256Hex([]byte(data)), authorN: nodeOf(fileKey)}
	tf.m = core.Manifest{Salt: strings.Repeat("0a", 16), Files: []core.FileRef{{Name: "a.txt", Size: int64(len(data)), SHA256: tf.sha}}}
	var err error
	if tf.man, err = core.EncodeManifest(tf.m); err != nil {
		t.Fatal(err)
	}
	tf.msha = core.SHA256Hex(tf.man)
	tf.msg = mk(t, fileKey, core.RecordFields{Seq: seq, Prev: prev, Files: tf.msha})
	wantOutcome(t, put(t, s, tf.msg, t0), OutcomeStored)
	return tf
}

// receive puts bytes for msg as a download would: reserve, write, count,
// commit.
func receive(t *testing.T, s *Store, msg, sha string, data []byte, items *core.Manifest) {
	t.Helper()
	ctx := context.Background()
	p, err := s.ReservePart(ctx, testRoom, msg, sha, int64(len(data)), t0)
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.OpenPart(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	f.Sync()
	f.Close()
	if err := s.PartHave(ctx, p, int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if got, err := s.PartSHA256(p); err != nil || got != sha {
		t.Fatalf("part hash %s %v", got, err)
	}
	if err := s.CommitPart(ctx, p, HowReceived, items, t0); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// TestMigration13FillsFileSetsFromRecordsAlreadyHeld: a record with files
// held before the upgrade gets its file_sets row; a malformed one does not.
// Requirements: L1, AGENTS.md rule 5
func TestMigration13FillsFileSetsFromRecordsAlreadyHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a2p2.db")
	saved := migrations
	migrations = saved[:12]
	s, err := Open(path)
	migrations = saved
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddRoom(context.Background(), testRoom, "team", "", t0); err != nil {
		t.Fatal(err)
	}
	good := mk(t, fileKey, core.RecordFields{Files: strings.Repeat("ab", 32)})
	for _, r := range []*core.VerifiedRecord{good} {
		f := r.Fields()
		if _, err := s.db.Exec(`INSERT INTO records (room, id, author, epoch, seq, kind, ref, parent, thread, agent, created, hdr, sig, body, reason, effect)
			VALUES (?,?,?,?,?,?,'','','',?,?,?,?,?,'live','ok')`, f.Room, r.ID(), r.Author(), f.Epoch, f.Seq, string(f.Kind), f.Agent,
			ms(f.Created), r.Header(), r.Sig(), r.Body()); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var man string
	if err := s.View(context.Background(), func(rd *Reader) (err error) {
		man, _, err = rd.FileSet(testRoom, good.ID())
		return err
	}); err != nil || man != strings.Repeat("ab", 32) {
		t.Fatalf("file set %q %v", man, err)
	}
}

// TestAStartRemovesTemporaryFilesAndFilesWithoutRows (PROTOCOL 10.11):
// a start finishes the queue and removes temporary files, blobs and parts
// no row names, staging no pending request names and copies still being
// written; what rows name stays.
// Requirements: L1
func TestAStartRemovesTemporaryFilesAndFilesWithoutRows(t *testing.T) {
	s, path := openTest(t)
	ctx := context.Background()
	tf := newTestFiles(t, s, 1, "", "kept bytes")
	receive(t, s, tf.msg.ID(), tf.sha, tf.data, nil)
	root := s.FileRoot()
	stray := []string{
		filepath.Join(root, "blobs", testRoom, strings.Repeat("e", 64)),
		filepath.Join(root, "blobs", testRoom, tmpPrefix+"x"),
		filepath.Join(root, "blobs", testRoom, "part", strings.Repeat("1", 32), strings.Repeat("f", 64)),
		filepath.Join(root, "blobs", ".staging", "q_"+strings.Repeat("2", 32), "0"),
	}
	// under files/ only what a row proves is a2p2's goes (finding 4)
	notOurs := []string{filepath.Join(root, "files", testRoom, tmpPrefix+"y"), filepath.Join(root, "files", testRoom, "b.txt")}
	for _, p := range append(stray, notOurs...) {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("stray"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// a copy that crashed between its link and its commit: the row names
	// its temporary file, and the final name is the same file
	c, _, err := s.ReserveCopy(ctx, testRoom, tf.msg.ID(), tf.sha, "alice", "a.txt", int64(len(tf.data)))
	if err != nil {
		t.Fatal(err)
	}
	tmp := tmpPrefix + "crashed"
	if _, err := s.db.Exec(`UPDATE file_copies SET tmp = ? WHERE path = ?`, tmp, c.Path); err != nil {
		t.Fatal(err)
	}
	tmpAbs := filepath.Join(filepath.Dir(s.CopyPath(c.Path)), tmp)
	if err := os.WriteFile(tmpAbs, tf.data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(tmpAbs, s.CopyPath(c.Path)); err != nil {
		t.Fatal(err)
	}
	// another reserved copy whose name a person's file took meanwhile
	c2, _, err := s.ReserveCopy(ctx, testRoom, tf.msg.ID(), tf.sha, "bob", "c.txt", int64(len(tf.data)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.CopyPath(c2.Path), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	notOurs = append(notOurs, s.CopyPath(c2.Path))
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.ReconcileFiles(ctx); err != nil {
		t.Fatal(err)
	}
	for _, p := range stray {
		if exists(p) {
			t.Errorf("%s survived the start", p)
		}
	}
	if exists(s2.CopyPath(c.Path)) || exists(tmpAbs) {
		t.Error("a copy still being written survived the start")
	}
	for _, p := range notOurs {
		if !exists(p) {
			t.Errorf("%s, not proven to be a2p2's, was removed", p)
		}
	}
	if !exists(s2.blobPath(testRoom, tf.sha)) {
		t.Error("a blob a row names was removed")
	}
}

// TestACrashBetweenWritingAFileAndItsRowLeavesNothingAfterTheRestart:
// bytes renamed into place with no row (the crash between) go at the start.
// Requirements: L1
func TestACrashBetweenWritingAFileAndItsRowLeavesNothingAfterTheRestart(t *testing.T) {
	s, _ := openTest(t)
	tf := newTestFiles(t, s, 1, "", "crash bytes")
	blob := s.blobPath(testRoom, tf.sha)
	if err := os.MkdirAll(filepath.Dir(blob), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blob, tf.data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileFiles(context.Background()); err != nil {
		t.Fatal(err)
	}
	if exists(blob) {
		t.Fatal("bytes with no row survived the start")
	}
}

// TestACrashBetweenDroppingAFileAndDeletingItCompletesTheDeletionAtTheRestart:
// the transaction that drops the row queues the path; a crash before the
// queue runs leaves the file, and the start deletes it.
// Requirements: L1
func TestACrashBetweenDroppingAFileAndDeletingItCompletesTheDeletionAtTheRestart(t *testing.T) {
	s, path := openTest(t)
	ctx := context.Background()
	tf := newTestFiles(t, s, 1, "", "dropped bytes")
	receive(t, s, tf.msg.ID(), tf.sha, tf.data, nil)
	// what a sweep's transaction does, without the queue's run after it
	if err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM file_for WHERE room = ?`, testRoom); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM files WHERE room = ?`, testRoom); err != nil {
			return err
		}
		return trash(tx, blobRel(testRoom, tf.sha), "blob", testRoom, "", tf.sha, 0, t0)
	}); err != nil {
		t.Fatal(err)
	}
	blob := s.blobPath(testRoom, tf.sha)
	if !exists(blob) {
		t.Fatal("the bytes went before the queue ran")
	}
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.ReconcileFiles(ctx); err != nil {
		t.Fatal(err)
	}
	if exists(blob) {
		t.Fatal("the queued deletion did not complete at the start")
	}
}

// TestAQueuedDeletionNeverRemovesBytesALiveRowNames: bytes dropped for one
// message and acquired again for another before the queue runs stay.
// Requirements: L1
func TestAQueuedDeletionNeverRemovesBytesALiveRowNames(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	tf := newTestFiles(t, s, 1, "", "shared bytes")
	receive(t, s, tf.msg.ID(), tf.sha, tf.data, nil)
	if err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM file_for WHERE room = ?`, testRoom); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM files WHERE room = ?`, testRoom); err != nil {
			return err
		}
		return trash(tx, blobRel(testRoom, tf.sha), "blob", testRoom, "", tf.sha, 0, t0)
	}); err != nil {
		t.Fatal(err)
	}
	other := mk(t, fileKey, core.RecordFields{Seq: 2, Prev: tf.msg.ID(), Files: tf.msha})
	wantOutcome(t, put(t, s, other, t0), OutcomeStored)
	receive(t, s, other.ID(), tf.sha, tf.data, nil)
	s.RunTrash(ctx)
	if !exists(s.blobPath(testRoom, tf.sha)) {
		t.Fatal("the queue removed bytes a live row names")
	}
}

// TestAFetchedNameCannotLeaveTheRoomsDirectoryOrOverwrite: a copy's name
// is one element under files/<room>/, sanitised, never over a file.
// Requirements: L1, T3
func TestAFetchedNameCannotLeaveTheRoomsDirectoryOrOverwrite(t *testing.T) {
	for in, want := range map[string]string{
		"../../etc/passwd": ".._.._etc_passwd", "..": "file", " . ": "file", "CON.txt": "_CON.txt",
		"a\u202eb.exe": "a_b.exe", "ok (1)+x.tar.gz": "ok (1)+x.tar.gz", "nul": "_nul",
	} {
		got := SafeName(in)
		if strings.TrimLeft(got, ".") != strings.TrimLeft(want, ".") || strings.ContainsAny(got, `/\`) || got == "." || got == ".." {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
	}
	if n := SafeName(strings.Repeat("é", 300)); len(n) > 200 {
		t.Errorf("name of %d bytes", len(n))
	}
	s, _ := openTest(t)
	ctx := context.Background()
	tf := newTestFiles(t, s, 1, "", "copy bytes")
	receive(t, s, tf.msg.ID(), tf.sha, tf.data, nil)
	dir := filepath.Join(s.FileRoot(), "files", testRoom)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, done, err := s.ReserveCopy(ctx, testRoom, tf.msg.ID(), tf.sha, "alice", "../a.txt", int64(len(tf.data)))
	if err != nil || done {
		t.Fatal(err, done)
	}
	if err := s.WriteCopy(c); err != nil {
		t.Fatal(err)
	}
	if err := s.CopyDone(ctx, c); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "mine" {
		t.Fatal("a copy overwrote a file")
	}
	if filepath.Dir(s.CopyPath(c.Path)) != dir {
		t.Fatalf("copy at %s, outside %s", s.CopyPath(c.Path), dir)
	}
	if b, _ := os.ReadFile(s.CopyPath(c.Path)); string(b) != string(tf.data) {
		t.Fatalf("copy holds %q", b)
	}
	// the exclusive create refuses a name that appeared meanwhile
	c2 := c
	c2.Path = copyRel(testRoom, "a.txt")
	if err := s.WriteCopy(c2); err == nil {
		t.Fatal("wrote over an existing file")
	}
}

// TestAFetchOverTheRoomsBudgetIsRefusedBeforeAnythingMoves: the part is
// reserved at its declared size, and one that does not fit is refused.
// Requirements: L1
func TestAFetchOverTheRoomsBudgetIsRefusedBeforeAnythingMoves(t *testing.T) {
	s, _ := openTest(t)
	s.SetFileBudgets(20, 0)
	ctx := context.Background()
	tf := newTestFiles(t, s, 1, "", "0123456789012345")
	if _, err := s.ReservePart(ctx, testRoom, tf.msg.ID(), tf.sha, 16, t0); err != nil {
		t.Fatal(err)
	}
	other := strings.Repeat("9", 64)
	if _, err := s.ReservePart(ctx, testRoom, tf.msg.ID(), other, 8, t0); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("over budget: %v", err)
	}
	if exists(filepath.Join(s.FileRoot(), "blobs", testRoom, "part", tf.msg.ID(), other)) {
		t.Fatal("a refused part was written")
	}
}

// TestWithdrawDeletesAFileItsListAndAnyPartOrCopyOnEveryMachineThatLearnsOfIt,
// store half: after a withdraw, the sweep drops the bytes, the list, the
// part and the copy, and deletes their files.
// Requirements: L1, T5
func TestWithdrawDropsTheFilesOfTheMessageInTheSweep(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	tf := newTestFiles(t, s, 1, "", "withdrawn bytes")
	receive(t, s, tf.msg.ID(), tf.msha, tf.man, &tf.m)
	receive(t, s, tf.msg.ID(), tf.sha, tf.data, nil)
	c, _, err := s.ReserveCopy(ctx, testRoom, tf.msg.ID(), tf.sha, "alice", "a.txt", int64(len(tf.data)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteCopy(c); err != nil {
		t.Fatal(err)
	}
	if err := s.CopyDone(ctx, c); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SweepFiles(ctx, testRoom, "", t0); err != nil || n != 0 {
		t.Fatalf("sweep of a live message dropped %d: %v", n, err)
	}
	wd := mk(t, fileKey, core.RecordFields{Seq: 2, Prev: tf.msg.ID(), Kind: core.KindWithdraw, Ref: tf.msg.ID()})
	wantOutcome(t, put(t, s, wd, t0), OutcomeStored)
	if _, err := s.SweepFiles(ctx, testRoom, "", t0); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{s.blobPath(testRoom, tf.sha), s.blobPath(testRoom, tf.msha), s.CopyPath(c.Path)} {
		if exists(p) {
			t.Errorf("%s survived the withdraw", p)
		}
	}
	r, m, _ := 0, 0, 0
	_ = s.View(ctx, func(rd *Reader) error {
		a, b, err := rd.FileUsage(testRoom)
		r, m = int(a), int(b)
		return err
	})
	if r != 0 || m != 0 {
		t.Fatalf("usage after withdraw %d %d", r, m)
	}
}

// TestAFileExpiresWithItsTextByEachHoldersClock, store half.
// Requirements: L1, T5
func TestAFileExpiresWithItsTextInTheSweep(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	tf := newTestFiles(t, s, 1, "", "old bytes")
	receive(t, s, tf.msg.ID(), tf.msha, tf.man, &tf.m)
	receive(t, s, tf.msg.ID(), tf.sha, tf.data, nil)
	if _, err := s.SweepFiles(ctx, testRoom, "", t0.Add(Retention-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !exists(s.blobPath(testRoom, tf.sha)) {
		t.Fatal("dropped before its text expired")
	}
	if _, err := s.SweepFiles(ctx, testRoom, "", t0.Add(Retention)); err != nil {
		t.Fatal(err)
	}
	if exists(s.blobPath(testRoom, tf.sha)) {
		t.Fatal("kept after its text expired")
	}
}
