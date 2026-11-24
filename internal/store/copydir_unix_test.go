//go:build !windows

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

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
			c, _, err := s.ReserveCopy(ctx, testRoom, tf.msg.ID(), tf.sha, "alice", "a.txt", int64(len(tf.data)))
			if err != nil {
				t.Fatal(err)
			}
			if err := s.PublishCopy(ctx, c, nil); !errors.Is(err, ErrBadPath) {
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
