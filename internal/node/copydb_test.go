package node

import (
	"errors"
	"io"
	"os"
	"testing"
)

// copyDB copies a closed SQLite database and its WAL, as a backup would.
func copyDB(t testing.TB, src, dst string) {
	t.Helper()
	for _, suf := range []string{"", "-wal"} {
		in, err := os.Open(src + suf)
		if errors.Is(err, os.ErrNotExist) && suf != "" {
			os.Remove(dst + suf)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		out, err := os.Create(dst + suf)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(out, in); err != nil {
			t.Fatal(err)
		}
		in.Close()
		if err := out.Close(); err != nil {
			t.Fatal(err)
		}
	}
	os.Remove(dst + "-shm")
}
