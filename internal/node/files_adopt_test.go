package node

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// servedBy reads, chunk by chunk, what n serves peer for sha of msg (""
// when refused).
func servedBy(t *testing.T, n *fnode, peer, room, msg, sha string) string {
	t.Helper()
	var out []byte
	for {
		fs, err := n.f.ServeGet(context.Background(), peer, FileGet{Room: room, Msg: msg, SHA256: sha, Offset: int64(len(out))})
		if err != nil || fs.Body == nil {
			return string(out)
		}
		b, err := io.ReadAll(fs.Body)
		fs.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b...)
		if len(b) == 0 || int64(len(out)) >= fs.Total {
			return string(out)
		}
	}
}

// Requirements: F3, F10. K2: bytes are never adopted unverified. A wrong
// regular file is planted at the blob's path before a download is made
// live: the verified part replaces it, and what is copied for the agent
// and served to a member are the verified bytes. At a start, a held blob
// whose content no longer hashes to its name is removed, not served.
func TestAFilePlantedAtTheBlobPathIsNeverTakenForTheBlob(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	data := "the verified bytes"
	msg, res := a.sendFiles(room, "alice", "x", writeSource(t, "x.txt", data))
	sha := res.Files[0].SHA256
	b.pullAll(room, a)
	b.tickFiles()
	b.inbox(room, "agent-b")
	blob := filepath.Join(b.st.FileRoot(), "blobs", room, sha)
	testHook = func(p string) error {
		if p == "files:commit" {
			if err := os.MkdirAll(filepath.Dir(blob), 0o700); err != nil {
				t.Error(err)
			}
			if err := os.WriteFile(blob, []byte("planted, not verified"), 0o600); err != nil {
				t.Error(err)
			}
		}
		return nil
	}
	t.Cleanup(func() { testHook = nil }) // after the test: a lease's watcher may still read it
	r, err := b.fetch(room, "agent-b", sha)
	if err != nil || r.State != FetchHere {
		t.Fatalf("fetch %+v %v", r, err)
	}
	if got, _ := os.ReadFile(r.Path); string(got) != data {
		t.Fatalf("the agent's copy holds %q", got)
	}
	if got := servedBy(t, b, a.id, room, msg, sha); got != data {
		t.Fatalf("served %q", got)
	}
	// a held blob changed on disk is caught by the next start
	if err := os.WriteFile(blob, []byte("changed while stopped"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.f.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := servedBy(t, b, a.id, room, msg, sha); got != "" {
		t.Fatalf("served %q after a start", got)
	}
	if how := heldFor(t, b, room, sha, msg); how != "" {
		t.Fatalf("still held (%s) after a start found the blob changed", how)
	}
}
