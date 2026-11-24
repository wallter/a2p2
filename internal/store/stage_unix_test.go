//go:build !windows

package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

// TestStagingCopiesFromTheHandleItOpenedAndRefusesAFifo (PROTOCOL 10.11):
// the path is opened once; a FIFO, a directory or a device is refused
// from the handle without blocking; a link is followed and the resolved
// path reported; the staged bytes are those read from the handle.
// Requirements: L1, P3
func TestStagingCopiesFromTheHandleItOpenedAndRefusesAFifo(t *testing.T) {
	s, _ := openTest(t)
	dir := t.TempDir()
	req := "q_" + strings.Repeat("3", 32)
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.StageFile(req, 0, fifo, core.MaxFileSize); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegular) {
			t.Fatalf("fifo: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("staging a FIFO blocked")
	}
	for _, p := range []string{dir, "/dev/null"} {
		if _, err := s.StageFile(req, 0, p, core.MaxFileSize); !errors.Is(err, ErrNotRegular) {
			t.Fatalf("%s: %v", p, err)
		}
	}
	real := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(real, []byte("the bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	st, err := s.StageFile(req, 1, link, core.MaxFileSize)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(real)
	if st.Path != want || st.Size != 9 || st.SHA256 != core.SHA256Hex([]byte("the bytes")) {
		t.Fatalf("staged %+v, want path %s", st, want)
	}
	// the source changing afterwards changes nothing staged
	if err := os.WriteFile(real, []byte("other bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckStaged(req, 1, st.SHA256, st.Size); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StageFile(req, 2, real, 3); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over the limit: %v", err)
	}
}
