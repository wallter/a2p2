package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// [store] A database under a directory whose name holds a character that
// an SQLite URI gives a meaning ('#', '?', '%') or a space is that file:
// written, closed, reopened and read back there, and no other file is
// made beside the directory. (Before the path was escaped, "a#b/a2p2.db"
// opened a database file named "a" next to the directory.)
func TestAStoreUnderADirectoryNamedWithURICharactersIsOpenedAtItsPath(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"a#b", "a?b", "a b", "a%23b", "a%b", "a?_txlock=deferred#c", "é:ü&x=1"} {
		t.Run(name, func(t *testing.T) {
			if runtime.GOOS == "windows" && strings.ContainsAny(name, `?:`) {
				t.Skip("not a file name on Windows")
			}
			parent := t.TempDir()
			dir := filepath.Join(parent, name)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "a2p2.db")
			st, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			addrs := []PeerAddr{{Addr: "192.0.2.1:7423", Source: "test"}}
			if err := st.SetPeerAddrs(ctx, "n_peer", addrs); err != nil {
				t.Fatal(err)
			}
			var lock string
			if err := st.db.QueryRow(`PRAGMA journal_mode`).Scan(&lock); err != nil || lock != "wal" {
				t.Fatalf("journal mode %q, %v", lock, err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
				t.Fatalf("no database at %s: %v", path, err)
			}
			if es, _ := os.ReadDir(parent); len(es) != 1 {
				var names []string
				for _, e := range es {
					names = append(names, e.Name())
				}
				t.Fatalf("beside the directory: %v (a database opened elsewhere)", names)
			}
			st, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			got, err := st.PeerAddrs(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(got, func(a PeerAddr) bool { return a.Addr == addrs[0].Addr }) {
				t.Fatalf("read back %+v", got)
			}
		})
	}
}

// [store] The URI given to SQLite: the path escaped, an absolute one with
// an empty authority (even one that starts "//"), a relative one relative.
func TestTheDatabaseURIEscapesThePath(t *testing.T) {
	for in, want := range map[string]string{
		"/home/a#b/a2p2.db": "file:///home/a%23b/a2p2.db",
		"/x?y/a 1%":         "file:///x%3Fy/a%201%25",
		"//net/a2p2.db":     "file:////net/a2p2.db",
		"rel/a2p2.db":       "file:rel/a2p2.db",
	} {
		if got := fileURI(in); got != want {
			t.Errorf("fileURI(%q) = %q, want %q", in, got, want)
		}
	}
}

// [store] A home whose database 0.2.3 put elsewhere (its path cut at the
// '#') is not opened as a new, empty node: Open refuses, naming both
// paths, and creates nothing; once the file is moved, it opens with its
// data.
func TestADatabaseThatAnOlderVersionPutBesideTheHomeIsNotHiddenByANewOne(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	old := filepath.Join(parent, "a")
	st, err := Open(old) // where 0.2.3 opened parent/a#b/a2p2.db
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetPeerAddrs(ctx, "n_peer", []PeerAddr{{Addr: "192.0.2.1:7423", Source: "test"}}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	home := filepath.Join(parent, "a#b")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "a2p2.db")
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), old) || !strings.Contains(err.Error(), path) {
		t.Fatalf("open: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a new database was made: %v", err)
	}
	for _, sfx := range []string{"", "-wal", "-shm"} {
		if err := os.Rename(old+sfx, path+sfx); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got, err := st.PeerAddrs(ctx); err != nil || len(got) != 1 {
		t.Fatalf("after the move: %+v %v", got, err)
	}
}

// [store] What 0.2.3 opened for a path: SQLite's reading of it unescaped.
func TestTheMisplacedPathIsSQLitesReadingOfThePathUnescaped(t *testing.T) {
	for in, want := range map[string]string{
		"/x/a#b/a2p2.db":   "/x/a",
		"/x/a?b/a2p2.db":   "/x/a",
		"/x/a%23b/a2p2.db": "/x/a#b/a2p2.db",
		"/x/a%00b/a2p2.db": "/x/a",
		"/x/a%zz/a2p2.db":  "/x/a%zz/a2p2.db",
	} {
		got, _ := misplacedPath(filepath.FromSlash(in))
		if got != filepath.FromSlash(want) {
			t.Errorf("misplacedPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// [store] Only an a2p2 database at the misplaced path stops the open: not
// another program's SQLite database there, nor a link to one; a link to
// an a2p2 database does, and so does one at the path cut at a %00.
func TestOnlyAnA2P2DatabaseAtTheMisplacedPathStopsTheOpen(t *testing.T) {
	ctx := context.Background()
	unrelated := func(t *testing.T, path string) {
		db, err := sql.Open("sqlite", fileURI(path))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.ExecContext(ctx, `CREATE TABLE notes (t TEXT); PRAGMA user_version = 3`); err != nil {
			t.Fatal(err)
		}
	}
	ours := func(t *testing.T, path string) {
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		st.Close()
	}
	for _, c := range []struct {
		name, home string
		make       func(t *testing.T, parent string) // what is at parent/a
		refused    bool
	}{
		{"another program's SQLite database", "a#b", func(t *testing.T, p string) { unrelated(t, filepath.Join(p, "a")) }, false},
		{"a link to another program's database", "a#b", func(t *testing.T, p string) {
			unrelated(t, filepath.Join(p, "other.db"))
			if err := os.Symlink(filepath.Join(p, "other.db"), filepath.Join(p, "a")); err != nil {
				t.Skip(err)
			}
		}, false},
		{"an a2p2 database", "a#b", func(t *testing.T, p string) { ours(t, filepath.Join(p, "a")) }, true},
		{"a link to an a2p2 database", "a?b", func(t *testing.T, p string) {
			ours(t, filepath.Join(p, "real.db"))
			if err := os.Symlink(filepath.Join(p, "real.db"), filepath.Join(p, "a")); err != nil {
				t.Skip(err)
			}
		}, true},
		{"an a2p2 database at the path cut at %00", "a%00b", func(t *testing.T, p string) { ours(t, filepath.Join(p, "a")) }, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if runtime.GOOS == "windows" && strings.ContainsAny(c.home, "?") {
				t.Skip("not a file name on Windows")
			}
			parent := t.TempDir()
			c.make(t, parent)
			home := filepath.Join(parent, c.home)
			if err := os.Mkdir(home, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, "a2p2.db")
			st, err := Open(path)
			if c.refused {
				if err == nil {
					st.Close()
					t.Fatal("opened a new database beside an a2p2 database at the misplaced path")
				}
				old := filepath.Join(parent, "a")
				for _, s := range []string{strconv.Quote(old), strconv.Quote(path), strconv.Quote(old + "-wal"), "must not exist"} {
					if !strings.Contains(err.Error(), s) {
						t.Fatalf("the refusal does not say %s: %v", s, err)
					}
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("a new database was made: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("refused for %s: %v", c.name, err)
			}
			st.Close()
		})
	}
}

// [store] A relative path (A2P2_HOME set to one) opens the file relative to
// the working directory, as before.
func TestAStoreAtARelativePathIsOpenedThere(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Mkdir("home #1", 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := Open(filepath.Join("home #1", "a2p2.db"))
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if _, err := os.Stat(filepath.Join(dir, "home #1", "a2p2.db")); err != nil {
		t.Fatal(err)
	}
}
