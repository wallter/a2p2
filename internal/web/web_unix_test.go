//go:build !windows

package web

import (
	"bytes"
	"os"
	"syscall"
	"testing"
	"time"
)

// PROTOCOL 10.8: a FIFO or a link put in place of the copy is refused at
// once (the open neither blocks nor follows the link), never served.
func TestADownloadWithAFIFOOrALinkInPlaceOfTheCopyIsRefusedWithoutBlocking(t *testing.T) {
	e, alice := setupFiles(t)
	body := "the verified bytes"
	msg := e.approve(t, e.sendFile(t, alice, "a.txt", body))
	t.Cleanup(func() { fileHook = func(string, string) {} })
	other := writeTemp(t, body) // the same bytes, elsewhere: a link to them is still refused
	for name, swap := range map[string]func(string) error{
		"fifo": func(p string) error { return syscall.Mkfifo(p, 0o600) },
		"link": func(p string) error { return os.Symlink(other, p) },
	} {
		fileHook = func(stage, path string) {
			if stage == "fetched" {
				if err := os.Remove(path); err != nil {
					t.Error(err)
				}
				if err := swap(path); err != nil {
					t.Error(err)
				}
			}
		}
		done := make(chan struct{})
		var code int
		var b []byte
		go func() {
			defer close(done)
			resp, rb := e.get(t, fileURL("team", msg, shaOf(body)))
			code, b = resp.StatusCode, rb
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the download blocked", name)
		}
		if code != 404 || bytes.Contains(b, []byte(body)) {
			t.Errorf("%s in place of the copy: %d %s", name, code, b)
		}
	}
}

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}
