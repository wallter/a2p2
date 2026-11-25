//go:build !windows

package store

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAQueuedDeletionThatFailsIsKeptAndTriedAgain (finding 5, B4): a
// removal that fails (here its directory refuses it) keeps the entry, idle
// runs try again, and the entry goes once the path is gone.
// Requirements: L1
func TestAQueuedDeletionThatFailsIsKeptAndTriedAgain(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root a read-only directory does not refuse a removal")
	}
	s, _ := openTest(t)
	ctx := context.Background()
	msg, sha := strings.Repeat("1", 32), strings.Repeat("f", 64)
	rel := partRel(testRoom, msg, sha)
	p := s.abs(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("part"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(p), 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Dir(p), 0o700)
	if err := s.tx(ctx, func(tx *sql.Tx) error {
		return trash(tx, rel, "part", testRoom, msg, sha, 0, "", t0)
	}); err != nil {
		t.Fatal(err)
	}
	s.RunTrash(ctx)
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM file_trash WHERE path = ?`, rel).Scan(&n); err != nil || n != 1 || !exists(p) {
		t.Fatalf("a failed deletion was forgotten: %d %v", n, err)
	}
	if err := os.Chmod(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	s.RunTrash(ctx) // an idle run, as the timer does
	if err := s.db.QueryRow(`SELECT count(*) FROM file_trash WHERE path = ?`, rel).Scan(&n); err != nil || n != 0 || exists(p) {
		t.Fatalf("the retry did not complete: %d %v", n, err)
	}
}

// TestARemovalWhoseDirectorySyncFailedStaysQueuedUntilASyncSucceeds (S5):
// the file is removed but its directory sync fails, so the entry stays;
// the next run finds the path absent and still keeps the entry while the
// sync fails, and drops it once a sync succeeds.
// Requirements: F10
func TestARemovalWhoseDirectorySyncFailedStaysQueuedUntilASyncSucceeds(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	msg, sha := strings.Repeat("2", 32), strings.Repeat("e", 64)
	rel := partRel(testRoom, msg, sha)
	p := s.abs(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("part"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.tx(ctx, func(tx *sql.Tx) error {
		return trash(tx, rel, "part", testRoom, msg, sha, 0, "", t0)
	}); err != nil {
		t.Fatal(err)
	}
	fail := true
	syncDirFails = func(string) bool { return fail }
	defer func() { syncDirFails = nil }()
	queued := func() int {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM file_trash WHERE path = ?`, rel).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	s.RunTrash(ctx)
	if exists(p) || queued() != 1 {
		t.Fatalf("after a removal whose sync failed: exists %v, queued %d", exists(p), queued())
	}
	s.RunTrash(ctx) // the path is absent now; the sync still fails
	if queued() != 1 {
		t.Fatal("an absent path left the queue without a successful sync")
	}
	fail = false
	s.RunTrash(ctx)
	if queued() != 0 {
		t.Fatal("the entry stayed after a sync succeeded")
	}
}

// TestAnApprovalWhoseBytesCannotBeWrittenFailsWithoutAPath (S3): putting
// approved bytes in place fails (the room's blob directory refuses a
// write); the error is a FileError with an operation and a code, and its
// text names no path.
// Requirements: T3
func TestAnApprovalWhoseBytesCannotBeWrittenFailsWithoutAPath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root a read-only directory does not refuse a write")
	}
	s, _ := openTest(t)
	dir := s.abs("blobs/" + testRoom)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	err := s.putBlob(testRoom, strings.Repeat("c", 64), func(w io.Writer) error { _, err := w.Write([]byte("x")); return err })
	var fe *FileError
	if !errors.As(err, &fe) || fe.Op == "" || fe.Code == "" || strings.Contains(err.Error(), s.FileRoot()) {
		t.Fatalf("putBlob refused with %v", err)
	}
}

// TestACopyIsNeverWrittenThroughALinkInTheFilesDirectory (finding 4): a
// link at files/<room> or at files is refused, and nothing is written
// where it points.
// Requirements: L1, T3
func TestACopyIsNeverWrittenThroughALinkInTheFilesDirectory(t *testing.T) {
	for _, at := range []string{"room", "files"} {
		t.Run(at, func(t *testing.T) {
			s, _ := openTest(t)
			ctx := context.Background()
			tf := newTestFiles(t, s, 1, "", "linked away")
			receive(t, s, tf.msg.ID(), tf.sha, tf.data, nil)
			outside := t.TempDir()
			link := filepath.Join(s.FileRoot(), "files", testRoom)
			if at == "files" {
				link = filepath.Join(s.FileRoot(), "files")
				if err := os.MkdirAll(filepath.Join(outside, testRoom), 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			if _, err := s.PublishCopy(ctx, CopyReq{Room: testRoom, Msg: tf.msg.ID(), SHA256: tf.sha, Agent: "alice", Name: "a.txt",
				Size: int64(len(tf.data))}, nil, nil); !errors.Is(err, ErrBadPath) {
				t.Fatalf("through a link: %v", err)
			}
			_ = filepath.Walk(outside, func(p string, fi os.FileInfo, err error) error {
				if err == nil && !fi.IsDir() {
					t.Errorf("written outside: %s", p)
				}
				return nil
			})
		})
	}
}
