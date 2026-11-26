package store

// What a send with files may stage (DESIGN 12, PROTOCOL 10.11). An agent
// names a path; a person approves what was staged from it. So the path is
// checked by its name before anything touches it, it is walked one
// component at a time from each directory opened (os.Root), every link on
// the way is read and its target checked the same way before that is
// touched, the file is opened once and only if it is the entry checked,
// and what is shown and sent is read from that handle. The machine's own
// files are never staged: nothing under the home but agents' copies, and
// no file that is the same file as one directly in the home (a hard link
// to the machine key, for one).

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Source is a file opened for staging: Path as the system reports it for
// the handle, Size from the handle, Links the names it has on disk when
// more than one.
type Source struct {
	f     *os.File
	Path  string
	Size  int64
	Links int
}

// Close closes the source's handle.
func (src *Source) Close() error {
	if src == nil || src.f == nil {
		return nil
	}
	return src.f.Close()
}

// ErrHomeFile: the path is one of a2p2's own files.
var ErrHomeFile = &FileError{Op: "open", Code: "home"}

// errLinkLoop: too many links on the way.
var errLinkLoop = &FileError{Op: "open", Code: "links"}

// maxSourceLinks bounds the links followed while resolving one path.
const maxSourceLinks = 40

// sourceLooked, in tests, sees each path walkSource looks at;
// sourceChecked sees each entry once it was looked at, before it is opened.
var sourceLooked, sourceChecked func(path string)

// OpenSource opens an absolute path an agent named for a send, once, by
// the rules above. It refuses: a remote or device path (RemoteOrDevice),
// on the way or once a link is read, before it is touched
// (ErrRemoteOrDevice); anything but a regular file (ErrNotRegular); a2p2's
// own files (ErrHomeFile). The caller closes it.
func (s *Store) OpenSource(path string) (*Source, error) {
	if !filepath.IsAbs(path) {
		return nil, ErrBadPath
	}
	home, err := s.homeIDs()
	if err != nil {
		return nil, ErrHomeFile // the home cannot be compared: refuse
	}
	f, resolved, err := walkSource(path, home)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
		}
	}()
	if stageOpened != nil {
		stageOpened()
	}
	st, err := f.Stat()
	if err != nil {
		return nil, fileErr("stat", err)
	}
	if !st.Mode().IsRegular() {
		return nil, ErrNotRegular
	}
	// the path shown is the one the system reports for the open handle
	shown, err := handlePath(f)
	if err != nil {
		return nil, fileErr("path", err)
	}
	if RemoteOrDevice(shown) && !mappedShare(resolved, shown, driveRemote) {
		return nil, ErrRemoteOrDevice
	}
	n := linkCount(f)
	if s.linkedHomeFile(st, n) {
		return nil, ErrHomeFile
	}
	src := &Source{f: f, Path: shown, Size: st.Size()}
	if n > 1 {
		src.Links = n
	}
	ok = true
	return src, nil
}

// walkSource opens path one component at a time, each looked up in the
// directory opened before it (os.Root), never by the whole name again:
// so a directory on the way that is swapped after its check cannot send a
// later lookup somewhere else, and a lookup never leaves the directory it
// was checked in. Each path reached is checked by RemoteOrDevice before it
// is looked at; a link is read, its target checked the same way, and the
// walk starts again from that target, so a link into /net or to a UNC path
// is refused before the system follows it. Each entry is opened only if it
// is still the one looked at (os.SameFile), so a link or another file put
// in its place between the look and the open is refused. A walk that
// passes through the home (compared by identity, so neither case nor a
// link hides it) is refused unless its next directory is files/, the
// agents' copies (ErrHomeFile). It returns the open file and the path it
// was reached by.
func walkSource(path string, home homeIDs) (*os.File, string, error) {
	sep := string(os.PathSeparator)
	p := filepath.Clean(path)
	links := 0
restart:
	if RemoteOrDevice(p) {
		return nil, "", ErrRemoteOrDevice
	}
	vol := filepath.VolumeName(p)
	var parts []string
	for _, x := range strings.Split(p[len(vol):], sep) {
		if x != "" {
			parts = append(parts, x)
		}
	}
	if len(parts) == 0 {
		return nil, "", ErrNotRegular
	}
	cur := vol + sep
	dir, err := os.OpenRoot(cur)
	if err != nil {
		return nil, "", fileErr("open", err)
	}
	inHome := false // dir is the home
	if st, err := dir.Stat("."); err != nil {
		dir.Close()
		return nil, "", fileErr("open", err)
	} else {
		inHome = os.SameFile(st, home.root)
	}
	for i, part := range parts {
		next := filepath.Join(cur, part)
		if RemoteOrDevice(next) {
			dir.Close()
			return nil, "", ErrRemoteOrDevice
		}
		if sourceLooked != nil {
			sourceLooked(next)
		}
		fi, err := dir.Lstat(part)
		if err != nil {
			dir.Close()
			return nil, "", fileErr("open", err)
		}
		if sourceChecked != nil {
			sourceChecked(next)
		}
		if fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			t, err := dir.Readlink(part)
			switch {
			case err != nil && fi.Mode()&os.ModeSymlink != 0:
				dir.Close()
				return nil, "", fileErr("open", err)
			case err == nil:
				dir.Close()
				links++
				if links > maxSourceLinks {
					return nil, "", errLinkLoop
				}
				t = linkTarget(t)
				switch {
				case filepath.IsAbs(t):
				case filepath.VolumeName(t) == "" && strings.HasPrefix(t, sep):
					t = vol + t // rooted on the same volume (Windows)
				default:
					t = filepath.Join(cur, t)
				}
				p = filepath.Clean(filepath.Join(append([]string{t}, parts[i+1:]...)...))
				goto restart
			}
		}
		if i == len(parts)-1 {
			// the last component: only a regular file is opened at all, so
			// a FIFO or a device is never opened
			if inHome {
				dir.Close()
				return nil, "", ErrHomeFile
			}
			if !fi.Mode().IsRegular() {
				dir.Close()
				return nil, "", ErrNotRegular
			}
			f, err := dir.OpenFile(part, os.O_RDONLY|oNonBlock, 0)
			dir.Close()
			if err != nil {
				return nil, "", fileErr("open", err)
			}
			if st, err := f.Stat(); err != nil || !os.SameFile(st, fi) {
				f.Close()
				return nil, "", errSwapped
			}
			return f, next, nil
		}
		if !fi.IsDir() {
			dir.Close()
			return nil, "", &FileError{Op: "open", Code: "not_found", err: os.ErrNotExist}
		}
		sub, err := dir.OpenRoot(part)
		dir.Close()
		if err != nil {
			return nil, "", fileErr("open", err)
		}
		if st, err := sub.Stat("."); err != nil || !os.SameFile(st, fi) {
			sub.Close()
			return nil, "", errSwapped
		}
		if inHome && (home.copies == nil || !os.SameFile(fi, home.copies)) {
			sub.Close()
			return nil, "", ErrHomeFile
		}
		inHome = os.SameFile(fi, home.root)
		dir, cur = sub, next
	}
	panic("unreachable")
}

// errSwapped: an entry on the way changed between its check and its open.
var errSwapped = &FileError{Op: "open", Code: "changed"}

// mappedShare reports whether shown, a path the name rule refuses, is the
// handle path of a file reached through a drive letter that names a
// network share the person mapped (Windows: \\server\share\x for Z:\x).
// That share is already connected, and it is used as any local path, as a
// network file system mounted on Unix is (D63). driveRemote
// says whether a drive root is such a share.
func mappedShare(resolved, shown string, driveRemote func(root string) bool) bool {
	if len(resolved) < 3 || resolved[1] != ':' || windowsRemoteOrDevice(resolved) {
		return false
	}
	rest, ok := strings.CutPrefix(shown, `\\`)
	if !ok || strings.HasPrefix(rest, `?`) || strings.HasPrefix(rest, `.`) || strings.ContainsAny(rest, `:*?"<>|`) {
		return false
	}
	return driveRemote(resolved[:2] + `\`)
}

// windowsRemoteOrDevice is RemoteOrDevice on Windows: everything but a
// local path of the plain form, a drive letter, a colon, a separator,
// then names [fix-files-b 7]. So a UNC path, the \\?\, \\.\ and \??\
// namespaces, a path relative to a drive's current directory, a colon
// anywhere else (an alternate data stream, a device written NUL:), a
// wildcard and a reserved device name in any component (with any
// extension or trailing spaces) are refused from the name, before any
// I/O. A drive letter the person mapped to a network share is a local
// path here, as on Unix a mounted network file system is.
func windowsRemoteOrDevice(path string) bool {
	if len(path) < 3 || path[1] != ':' || !(path[0] >= 'A' && path[0] <= 'Z' || path[0] >= 'a' && path[0] <= 'z') ||
		path[2] != '\\' && path[2] != '/' {
		return true
	}
	rest := path[3:]
	if strings.ContainsAny(rest, `:*?"<>|`) {
		return true
	}
	for _, part := range strings.FieldsFunc(rest, func(r rune) bool { return r == '\\' || r == '/' }) {
		stem := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if windowsReserved[strings.TrimRight(stem, " ")] {
			return true
		}
	}
	return false
}

// homeIDs are the identities walkSource compares with: the home, and
// its files/ directory (nil when there is none yet).
type homeIDs struct{ root, copies os.FileInfo }

func (s *Store) homeIDs() (homeIDs, error) {
	root, err := os.Stat(s.root)
	if err != nil {
		return homeIDs{}, err
	}
	h := homeIDs{root: root}
	if c, err := os.Lstat(filepath.Join(s.root, copyDirRel(""))); err == nil && c.IsDir() {
		h.copies = c
	}
	return h, nil
}

// linkedHomeFile reports whether the opened file st, which has links
// names, is also a file anywhere under the home outside files/: a hard
// link to the machine key, the local key, the database, a blob or a part.
// A file with one name is not (its one name is the one walked, and the
// walk refuses the home), so the home is listed only for a file with
// several names, or when the count is unknown. The home is walked
// without following links; one it cannot list is refused.
func (s *Store) linkedHomeFile(st os.FileInfo, links int) bool {
	if links == 1 {
		return false
	}
	copies := filepath.Join(s.root, copyDirRel(""))
	found := false
	err := filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p == copies {
			return filepath.SkipDir
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if fi, err := d.Info(); err == nil && os.SameFile(st, fi) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found || err != nil
}

// StagedHomeFile reports whether a file staged for request req, as a
// draft records it (path shown, SHA-256), looks like one of a2p2's own
// files: its path is under the home but outside files/ (by name, ignoring
// case), or its bytes are those of a small file directly in the home (the
// machine key, the local key). Approval refuses such a file, so a request
// an older binary staged before staging refused them is not published.
// A copy of the database made then has other bytes now and is not
// caught; such a request expires within a day.
func (s *Store) StagedHomeFile(path, sha string) bool {
	homes := []string{filepath.Clean(s.root)}
	if r, err := filepath.EvalSymlinks(s.root); err == nil {
		homes = append(homes, r)
	}
	p := filepath.Clean(path)
	sep := string(os.PathSeparator)
	for _, h := range homes {
		if len(p) > len(h) && strings.EqualFold(p[:len(h)], h) && p[len(h):len(h)+1] == sep {
			rest := p[len(h)+1:]
			if !(len(rest) > 6 && strings.EqualFold(rest[:6], "files"+sep)) {
				return true
			}
		}
	}
	es, err := os.ReadDir(s.root)
	if err != nil {
		return true
	}
	for _, e := range es {
		fi, err := e.Info()
		if err != nil || !fi.Mode().IsRegular() || fi.Size() > homeKeyMax {
			continue
		}
		if got, err := fileSHA256(filepath.Join(s.root, e.Name())); err == nil && got == sha {
			return true
		}
	}
	return false
}

// homeKeyMax bounds the files of the home StagedHomeFile hashes: the keys
// are a few hundred bytes.
const homeKeyMax = 64 << 10
