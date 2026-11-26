package harness

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// realBinary makes an a2p2 file in a fresh directory that checkBinary
// treats as outside any temporary directory, and stops its walk at that
// directory's parent so the test does not depend on /tmp's mode.
func realBinary(t *testing.T) (dir, bin string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix file modes")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil { // others may enter, as a home often allows
		t.Fatal(err)
	}
	dir = filepath.Join(root, "bin")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(dir, "a2p2")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldRoots, oldStop := tempRoots, stopAt
	tempRoots = func() []string { return nil }
	stopAt = root
	t.Cleanup(func() { tempRoots, stopAt = oldRoots, oldStop })
	return dir, bin
}

// F50: install refuses to write a binary path into a harness that another
// user could later fill with a program of their own, and writes nothing.
func TestInstallRefusesABinaryInATemporaryDirectory(t *testing.T) {
	dir, bin := realBinary(t)
	tempRoots = func() []string { return []string{dir} }
	for _, name := range []string{"claude", "pi"} {
		o, _ := setup(t)
		o.Binary = bin
		before := tree(t, o.HomeDir)
		_, err := Install(name, o)
		if err == nil || !strings.Contains(err.Error(), "temporary directory") {
			t.Fatalf("%s: %v", name, err)
		}
		if after := tree(t, o.HomeDir); len(after) != len(before) {
			t.Fatalf("%s: a refused install wrote files: %v", name, after)
		}
		if r, _ := loadRecord(name, o); r != nil {
			t.Fatalf("%s: a refused install left a record", name)
		}
	}
}

func TestInstallRefusesABinaryInGoBuildCache(t *testing.T) {
	_, bin := realBinary(t)
	cache := filepath.Join(filepath.Dir(filepath.Dir(bin)), "go-build123", "b001", "exe")
	os.MkdirAll(cache, 0o755)
	o, _ := setup(t)
	o.Binary = filepath.Join(cache, "a2p2")
	os.WriteFile(o.Binary, nil, 0o755)
	if _, err := Install("claude", o); err == nil || !strings.Contains(err.Error(), "build cache") {
		t.Fatalf("%v", err)
	}
}

func TestInstallRefusesABinaryOthersCanReplace(t *testing.T) {
	dir, bin := realBinary(t)
	o, _ := setup(t)
	o.Binary = bin
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := Install("claude", o); err == nil || !strings.Contains(err.Error(), "every user") {
		t.Fatalf("world-writable directory: %v", err)
	}
	os.Chmod(dir, 0o755)
	os.Chmod(bin, 0o757)
	if _, err := Install("claude", o); err == nil || !strings.Contains(err.Error(), "every user") {
		t.Fatalf("world-writable binary: %v", err)
	}
	os.Chmod(bin, 0o755)
	// Doctor says the same about a connected harness.
	o.Run = fakeCLI(o.HomeDir)
	if _, err := Install("claude", o); err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o777)
	defer os.Chmod(dir, 0o755)
	var found bool
	for _, c := range Doctor(o) {
		if c.Name == "a2p2 binary" {
			found = !c.OK && strings.Contains(c.Info, "every user") && c.Fix != ""
		}
	}
	if !found {
		t.Fatalf("doctor: %+v", Doctor(o))
	}
}

// A directory every user may change is fine below one they may not enter,
// such as a projects folder made 0777 inside a private ~/Documents.
func TestInstallAcceptsAWritableDirectoryInsideOneOthersCannotEnter(t *testing.T) {
	dir, _ := realBinary(t)
	open := filepath.Join(dir, "open")
	os.Mkdir(open, 0o777)
	os.Chmod(open, 0o777)
	inner := filepath.Join(open, "a2p2")
	os.WriteFile(inner, nil, 0o755)
	o, _ := setup(t)
	o.Binary = inner
	o.Run = fakeCLI(o.HomeDir)
	if _, err := Install("claude", o); err == nil {
		t.Fatal("a directory every user can change, reachable by them, was accepted")
	}
	os.Chmod(dir, 0o700)
	defer os.Chmod(dir, 0o755)
	if rep, err := Install("claude", o); err != nil || strings.Contains(rep, "warning") {
		t.Fatalf("below a private directory: %v %s", err, rep)
	}
}

// A directory its group can change only warns: on some systems that group
// is the administrators.
func TestInstallWarnsAboutAGroupWritableBinaryDirectory(t *testing.T) {
	dir, bin := realBinary(t)
	os.Chmod(dir, 0o775)
	o, _ := setup(t)
	o.Binary = bin
	o.Run = fakeCLI(o.HomeDir)
	rep, err := Install("claude", o)
	if err != nil || !strings.Contains(rep, "warning: "+dir+" can be changed by its group") {
		t.Fatalf("%v %s", err, rep)
	}
}

// The honest case: a binary in a directory only its user can change is
// installed, and doctor has nothing to say about it.
func TestInstallAcceptsABinaryOnlyItsUserCanChange(t *testing.T) {
	_, bin := realBinary(t)
	o, _ := setup(t)
	o.Binary = bin
	o.Run = fakeCLI(o.HomeDir)
	rep, err := Install("claude", o)
	if err != nil || strings.Contains(rep, "warning") {
		t.Fatalf("%v %s", err, rep)
	}
	for _, c := range Doctor(o) {
		if c.Name == "a2p2 binary" || !c.OK {
			t.Fatalf("doctor: %+v", c)
		}
	}
}

// A control character in the binary's path would end up inside Pi's skill
// text and config lines; it is refused on every system.
func TestInstallRefusesABinaryPathWithAControlCharacter(t *testing.T) {
	for _, target := range []string{"linux", "windows"} {
		o, _ := setup(t)
		o.OS = target
		o.Binary = "/opt/x\n```\nrun this\n/a2p2"
		if _, err := Install("opencode", o); err == nil || !strings.Contains(err.Error(), "control character") {
			t.Fatalf("%s: %v", target, err)
		}
	}
}

// The directories install creates for a harness's configuration (for
// example ~/.hermes, where Hermes later keeps its keys) are the user's
// alone, like the files a2p2 writes in them.
func TestDirectoriesInstallCreatesAreOnlyTheUsers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file modes")
	}
	for _, name := range []string{"claude", "opencode", "hermes", "pi"} {
		o, _ := setup(t)
		o.Run = fakeCLI(o.HomeDir)
		before := tree(t, o.HomeDir)
		if _, err := Install(name, o); err != nil {
			t.Fatal(err)
		}
		for p, v := range tree(t, o.HomeDir) {
			if _, ok := before[p]; ok || !strings.HasPrefix(v, "d") {
				continue
			}
			if fi, err := os.Stat(filepath.Join(o.HomeDir, p)); err != nil || fi.Mode().Perm()&0o077 != 0 {
				t.Errorf("%s: install created %s as %s", name, p, v)
			}
		}
	}
}

// F50: the harness runs the path as written, so a symlink to an honest
// binary is refused when the link, or a directory on the way to it, is in
// a temporary directory or one every user can change. Doctor says the same.
func TestInstallChecksThePathAsWrittenNotOnlyWhereASymlinkLeads(t *testing.T) {
	dir, bin := realBinary(t)
	open := filepath.Join(filepath.Dir(dir), "open")
	os.Mkdir(open, 0o777)
	os.Chmod(open, 0o777)
	link := filepath.Join(open, "a2p2")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	o, _ := setup(t)
	o.Binary = link
	o.Run = fakeCLI(o.HomeDir)
	if _, err := Install("claude", o); err == nil || !strings.Contains(err.Error(), "every user") {
		t.Fatalf("symlink in a directory every user can change: %v", err)
	}
	os.Chmod(open, 0o755)
	if _, err := Install("claude", o); err != nil {
		t.Fatalf("symlink in a directory only its user can change: %v", err)
	}
	os.Chmod(open, 0o777)
	var found bool
	for _, c := range Doctor(o) {
		found = found || c.Name == "a2p2 binary" && !c.OK && strings.Contains(c.Info, "every user")
	}
	if !found {
		t.Fatalf("doctor: %+v", Doctor(o))
	}
	os.Chmod(open, 0o755)
	// A link in a temporary directory, and a link to a directory there.
	tempRoots = func() []string { return []string{open} }
	o2, _ := setup(t)
	o2.Binary = link
	if _, err := Install("claude", o2); err == nil || !strings.Contains(err.Error(), "temporary directory") {
		t.Fatalf("symlink in a temporary directory: %v", err)
	}
	tempRoots = func() []string { return []string{dir} }
	viaDir := filepath.Join(open, "bin")
	os.Symlink(dir, viaDir)
	o2.Binary = filepath.Join(viaDir, "a2p2")
	if _, err := Install("claude", o2); err == nil || !strings.Contains(err.Error(), "temporary directory") {
		t.Fatalf("path through a symlink to a temporary directory: %v", err)
	}
}

// F50: a binary path that does not exist yet is checked as far as it
// exists: another user who may create the missing part could put a
// program there.
func TestInstallChecksAMissingBinaryPathAsFarAsItExists(t *testing.T) {
	dir, _ := realBinary(t)
	open := filepath.Join(dir, "open")
	os.Mkdir(open, 0o777)
	os.Chmod(open, 0o777)
	o, _ := setup(t)
	o.Binary = filepath.Join(open, "later", "a2p2")
	if _, err := Install("claude", o); err == nil || !strings.Contains(err.Error(), "every user") {
		t.Fatalf("missing binary below a directory every user can change: %v", err)
	}
	tempRoots = func() []string { return []string{dir} }
	o.Binary = filepath.Join(dir, "gone", "a2p2")
	if _, err := Install("claude", o); err == nil || !strings.Contains(err.Error(), "temporary directory") {
		t.Fatalf("missing binary in a temporary directory: %v", err)
	}
}
