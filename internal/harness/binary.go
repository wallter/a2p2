package harness

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
)

// Install writes the a2p2 binary's path into a harness's configuration,
// which then runs that program as the person: the hook on every prompt,
// the MCP server on every session. So the binary must stay where it is,
// and nobody else may be able to put another program at that path.
//
// checkBinary refuses a path with a control character (it would end up
// inside a skill's text or a config line), a binary in a temporary
// directory or Go's build cache (it goes away, and on a shared /tmp
// another user can then create one at the same path), and a binary that
// it, or a directory above it, lets another user replace: writable by
// everyone, or owned by a user other than this one or root, unless a
// directory above it that other users may not enter keeps them out. A
// directory writable by its group gives only a warning, since on some
// systems that group is the administrators. Access control lists are not
// read.
//
// The harness runs the path as it is written, and follows a symlink in it
// only when it runs. So the path as written is checked, directory by
// directory, and so is the file it leads to now: whoever can change either
// can change what runs. A part of the path that does not exist yet is not
// there to check, but the directories above it are. On Windows only the
// control characters are checked; reading the binary's access control list
// is not done yet.
func checkBinary(o Options) (warn string, err error) {
	bin := o.Binary
	if strings.ContainsFunc(bin, unicode.IsControl) {
		return "", fmt.Errorf("the path of this a2p2 binary has a control character (%q); move it to a plain directory, then run that copy's install", bin)
	}
	// The walk below reads this machine's file modes and owners. Windows
	// has neither (Go reports every directory as 0777 and every writable
	// file as 0666), so on a Windows machine they mean nothing, whatever
	// system the configuration is written for; reading them refused every
	// path, as its drive's root looked writable by everyone.
	if o.targetOS() == "windows" || runtime.GOOS == "windows" {
		return "", nil
	}
	paths := []string{filepath.Clean(bin)}
	real, err := filepath.EvalSymlinks(bin)
	switch {
	case err == nil && real != paths[0]:
		paths = append(paths, real)
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return "", err
	}
	for _, p := range paths {
		w, err := checkPath(p, bin)
		if err != nil {
			return "", err
		}
		if warn == "" {
			warn = w
		}
	}
	return warn, nil
}

// checkPath is checkBinary's test of one path to the binary bin.
func checkPath(path, bin string) (warn string, err error) {
	fix := "; copy a2p2 to a directory only you can change, such as ~/.local/bin, and run that copy's install"
	for _, root := range tempRoots() {
		r, err := filepath.EvalSymlinks(root)
		if within(path, root) || err == nil && within(path, r) {
			return "", fmt.Errorf("%s is in a temporary directory (%s): it can be deleted, and then another program could take its path%s", bin, root, fix)
		}
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if strings.HasPrefix(part, "go-build") {
			return "", fmt.Errorf("%s is in Go's build cache (go run): it is deleted after the run%s", bin, fix)
		}
	}
	// From the top down: below a directory of this user (or root) that
	// others may not enter, nothing can be reached by them, whatever its
	// modes; the same for its group when the group may not enter either.
	var chain []string
	for p := path; ; p = filepath.Dir(p) {
		if stopAt != "" && !within(p, stopAt) {
			break
		}
		chain = append([]string{p}, chain...)
		if p == filepath.Dir(p) {
			break
		}
	}
	uid := os.Getuid()
	others, group := true, true // whether others, and the group, can still reach this far
	for _, p := range chain {
		fi, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue // not there yet: the directory above decides who may create it
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			// A link's own mode and owner do not decide who can change it:
			// the directory it is in does, and what it leads to is checked
			// on its own.
			continue
		}
		perm := fi.Mode().Perm()
		owner, known := fileOwner(fi)
		if others && perm&0o002 != 0 {
			return "", fmt.Errorf("%s can be changed by every user on this machine, so another user could replace %s%s", p, bin, fix)
		}
		if others && known && owner != 0 && owner != uid {
			return "", fmt.Errorf("%s belongs to another user (uid %d), who could replace %s%s", p, owner, bin, fix)
		}
		if group && perm&0o020 != 0 && warn == "" {
			warn = "warning: " + p + " can be changed by its group, whose members could replace " + bin + "; if that group is not only administrators, copy a2p2 to a directory only you can change"
		}
		if fi.IsDir() && known && (owner == uid || owner == 0) {
			others = others && perm&0o001 != 0
			group = group && perm&0o010 != 0
		}
	}
	return warn, nil
}

// tempRoots are the temporary directories checkBinary refuses; stopAt, when
// set, ends its walk up the directories (tests set both).
var (
	tempRoots = func() []string { return []string{os.TempDir(), "/tmp", "/var/tmp", "/dev/shm"} }
	stopAt    = ""
)

// within reports whether p is dir or inside it.
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// binaryCheck is doctor's line on the binary a harness runs, or false when
// there is nothing to report.
func binaryCheck(o Options) (Check, bool) {
	warn, err := checkBinary(o)
	switch {
	case err != nil:
		return Check{Name: "a2p2 binary", Info: err.Error(), Fix: "copy a2p2 to a directory only you can change, such as ~/.local/bin, and run that copy's install"}, true
	case warn != "":
		return Check{Name: "a2p2 binary", OK: true, Info: warn}, true
	}
	return Check{}, false
}
