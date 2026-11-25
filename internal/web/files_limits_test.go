package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// snapshots lists the download snapshots under e's home: temporary files
// directly in blobs/.
func (e *env) snapshots(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(e.st.FileRoot(), "blobs"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, x := range ents {
		if !x.IsDir() && strings.HasPrefix(x.Name(), ".a2p2-tmp-") {
			out = append(out, x.Name())
		}
	}
	return out
}

// F2: a download's snapshot is written under the home with a temporary
// file's name, never in the system's temporary directory; a normal
// download removes it when it ends, and one a crash left behind is gone
// after the next start.
func TestADownloadSnapshotLeftByACrashIsGoneAfterAStart(t *testing.T) {
	e, alice := setupFiles(t)
	t.Cleanup(func() { fileHook = func(string, string) {} })
	sys := t.TempDir()
	t.Setenv("TMPDIR", sys)
	t.Setenv("TMP", sys)
	t.Setenv("TEMP", sys)
	body := "the bytes downloaded"
	msg := e.approve(t, e.sendFile(t, alice, "a.txt", body))
	e.shown(t)
	var during []string
	fileHook = func(stage, _ string) {
		if stage == "verified" {
			during = e.snapshots(t)
		}
	}
	resp, b := e.get(t, fileURL("team", msg, shaOf(body)))
	if resp.StatusCode != 200 || string(b) != body {
		t.Fatalf("download: %d %q", resp.StatusCode, b)
	}
	if len(during) != 1 {
		t.Fatalf("snapshots under the home while serving: %v", during)
	}
	if left, _ := os.ReadDir(sys); len(left) != 0 {
		t.Errorf("%d files in the system's temporary directory", len(left))
	}
	if left := e.snapshots(t); len(left) != 0 {
		t.Errorf("a normal download left its snapshot: %v", left)
	}
	// A crash between the snapshot's creation and its removal leaves it.
	f, err := e.s.SnapshotFile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if left := e.snapshots(t); len(left) != 1 {
		t.Fatalf("the snapshot a crash left: %v", left)
	}
	if err := e.st.ReconcileFiles(e.ctx); err != nil { // what a start does with files
		t.Fatal(err)
	}
	if left := e.snapshots(t); len(left) != 0 {
		t.Errorf("a snapshot a crash left outlived a start: %v", left)
	}
	if n := e.st.UnknownFiles(); n != 0 {
		t.Errorf("the start counted %d unknown files", n)
	}
}
