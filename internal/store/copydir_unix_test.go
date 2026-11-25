//go:build !windows

package store

import (
	"context"
	"database/sql"
	"errors"
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
