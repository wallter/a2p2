package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// F19: another machine can make the daemon log (each refused request is a
// line), and the log used to grow without end. Past its size it is moved
// to daemon.log.1, which replaces the one before and holds only the last
// logMax bytes, so at most two files of about that size are kept; the
// daemon's handle, which appends, goes on writing at the start of the
// emptied file.
func TestTheDaemonLogIsRotatedBySizeAndOneOldFileKept(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	w, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // as Connect opens it
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	const max = 1000
	write := func(s string) {
		if _, err := w.WriteString(s); err != nil {
			t.Fatal(err)
		}
	}
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	write("z" + strings.Repeat("a", max))
	if err := rotateLog(path, max); err != nil {
		t.Fatal(err)
	}
	if read(path) != "" || read(path+".1") != strings.Repeat("a", max) {
		t.Fatal("first rotation")
	}
	write("next line\n")
	if read(path) != "next line\n" {
		t.Fatalf("the daemon's handle does not write at the start of the emptied log: %q", read(path))
	}
	write(strings.Repeat("b", max))
	if err := rotateLog(path, max); err != nil {
		t.Fatal(err)
	}
	if read(path) != "" || read(path+".1") != strings.Repeat("b", max) {
		t.Fatal("second rotation")
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 2 {
		t.Fatalf("files kept: %v", ents)
	}
	// Windows has no Unix modes (Go reports 0666 for any writable file);
	// the archive takes its directory's access control list there.
	if fi, _ := os.Stat(path + ".1"); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("daemon.log.1 mode %o", fi.Mode().Perm())
	}
}

// The honest case: a log within its size, or none, is left alone.
func TestADaemonLogWithinItsSizeIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	if err := rotateLog(path, 10); err != nil {
		t.Fatalf("no log: %v", err)
	}
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rotateLog(path, 10); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "0123456789" {
		t.Fatalf("log changed: %q", b)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("daemon.log.1 made: %v", err)
	}
}

// keepLog checks again while the daemon runs, not only at its start.
func TestKeepLogRotatesWhileRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { keepLog(ctx, path, 10, 10*time.Millisecond); close(done) }()
	time.Sleep(30 * time.Millisecond)
	if err := os.WriteFile(path, []byte(strings.Repeat("z", 11)), 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a rotation", func() bool { _, err := os.Stat(path + ".1"); return err == nil })
	cancel()
	<-done
}

// F19: when daemon.log.1 cannot be written (a full disk, say), the log is
// emptied all the same, so a remote party that makes the daemon log cannot
// fill the disk. The archive is made to fail in two ways: a directory in
// the way of daemon.log.1, which no system renames a file over (this runs
// on Windows too), and, on Unix, a directory the daemon may not create
// files in.
func TestALogThatCannotBeArchivedIsStillEmptied(t *testing.T) {
	for _, c := range []struct {
		name  string
		block func(t *testing.T, dir, path string)
	}{
		{"daemon.log.1 cannot be replaced", func(t *testing.T, dir, path string) {
			if err := os.MkdirAll(filepath.Join(path+".1", "x"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"no file can be made beside the log", func(t *testing.T, dir, path string) {
			if runtime.GOOS == "windows" {
				t.Skip("Go's Chmod sets only the read-only attribute on Windows, which does not stop new files in a directory; the other case runs there")
			}
			if os.Getuid() == 0 {
				t.Skip("root may create files in any directory")
			}
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "daemon.log")
			if err := os.WriteFile(path, []byte(strings.Repeat("x", 100)), 0o600); err != nil {
				t.Fatal(err)
			}
			c.block(t, dir, path)
			if err := rotateLog(path, 10); err == nil {
				t.Fatal("no error for a log that could not be archived")
			}
			if fi, err := os.Stat(path); err != nil || fi.Size() != 0 {
				t.Fatalf("the log was not emptied: %v %v", fi, err)
			}
			if fi, err := os.Stat(path + ".1"); err == nil && fi.Mode().IsRegular() {
				t.Fatal("daemon.log.1 was made")
			}
			ents, _ := os.ReadDir(dir)
			for _, e := range ents {
				if strings.HasSuffix(e.Name(), ".tmp") {
					t.Fatalf("a temporary file was left: %s", e.Name())
				}
			}
		})
	}
}
