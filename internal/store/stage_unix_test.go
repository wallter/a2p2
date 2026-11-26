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
	if _, err := s.StageFile(req, 0, dir, core.MaxFileSize); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("%s: %v", dir, err)
	}
	// a device under /dev is refused by its name, before it is opened
	if _, err := s.StageFile(req, 0, "/dev/null", core.MaxFileSize); !errors.Is(err, ErrRemoteOrDevice) {
		t.Fatalf("/dev/null: %v", err)
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

// TestThePathShownIsTheOpenedFilesNotTheNameAskedFor (fix-files-b 7): a
// file moved right after it was opened is shown where it now is, with
// the bytes read from the handle.
// Requirements: L1, P3
func TestThePathShownIsTheOpenedFilesNotTheNameAskedFor(t *testing.T) {
	s, _ := openTest(t)
	dir := t.TempDir()
	asked, moved := filepath.Join(dir, "asked.txt"), filepath.Join(dir, "moved.txt")
	if err := os.WriteFile(asked, []byte("opened bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	stageOpened = func() {
		if err := os.Rename(asked, moved); err != nil {
			t.Error(err)
		}
	}
	defer func() { stageOpened = nil }()
	st, err := s.StageFile("q_"+strings.Repeat("4", 32), 0, asked, core.MaxFileSize)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(moved)
	if st.Path != want || st.SHA256 != core.SHA256Hex([]byte("opened bytes")) {
		t.Fatalf("staged %+v, want path %s", st, want)
	}
}

// TestAFileErrorNamesItsOperationAndCodeNeverThePath (fix-files-b 8).
// Requirements: L1
func TestAFileErrorNamesItsOperationAndCodeNeverThePath(t *testing.T) {
	s, _ := openTest(t)
	dir := t.TempDir()
	missing := filepath.Join(dir, "secret-project-name.txt")
	_, err := s.StageFile("q_"+strings.Repeat("5", 32), 0, missing, core.MaxFileSize)
	var fe *FileError
	if !errors.As(err, &fe) || fe.Op != "open" || fe.Code != "not_found" || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing: %#v", err)
	}
	if strings.Contains(err.Error(), "secret-project-name") || strings.Contains(err.Error(), dir) {
		t.Fatalf("the error names the path: %v", err)
	}
}
