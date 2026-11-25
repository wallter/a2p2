package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestAStartRemovesOnlyFilesWithNamesA2p2Generates: K3. Files under
// blobs/ whose names a2p2 does not generate (a note in a room's
// directory, one at the top, one in a directory under .staging that is
// not a request's) survive a start and are counted; a blob-named file no
// row names and a part still go.
// Requirements: F10
func TestAStartRemovesOnlyFilesWithNamesA2p2Generates(t *testing.T) {
	s, _ := openTest(t)
	base := filepath.Join(s.FileRoot(), "blobs")
	sha := "a000000000000000000000000000000000000000000000000000000000000000"
	keep := []string{
		filepath.Join(base, testRoom, "notes.txt"),
		filepath.Join(base, "README"),
		filepath.Join(base, ".staging", "mine", "a"),
	}
	gone := []string{
		s.blobPath(testRoom, sha),
		s.abs(partRel(testRoom, "0123456789abcdef0123456789abcdef", sha)),
	}
	for _, p := range append(append([]string{}, keep...), gone...) {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ReconcileFiles(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, p := range keep {
		if !exists(p) {
			t.Errorf("a start removed %s, a name a2p2 does not generate", p)
		}
	}
	for _, p := range gone {
		if exists(p) {
			t.Errorf("a start left %s, a2p2's own name with no row", p)
		}
	}
	if n := s.UnknownFiles(); n != int64(len(keep)) {
		t.Errorf("unknown files = %d, want %d", n, len(keep))
	}
}
