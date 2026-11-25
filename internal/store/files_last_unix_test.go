//go:build !windows

package store

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestAHeldBlobAStartCouldNotExamineIsNotServedUntilVerified (W1): the
// room's blob directory cannot be read at a start, so its held blob keeps
// its row unexamined. Its bytes were changed meanwhile: the first open
// hashes them and refuses; bytes that match are served after that hash.
// Requirements: F10
func TestAHeldBlobAStartCouldNotExamineIsNotServedUntilVerified(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	s, _ := openTest(t)
	ctx := context.Background()
	tf := newTestFiles(t, s, 1, "", "the bytes held")
	receive(t, s, tf.msg.ID(), tf.sha, tf.data, nil)
	blob := s.blobPath(testRoom, tf.sha)
	dir := filepath.Dir(blob)
	start := func() {
		t.Helper()
		if err := os.Chmod(dir, 0); err != nil {
			t.Fatal(err)
		}
		err := s.ReconcileFiles(ctx)
		if cerr := os.Chmod(dir, 0o700); cerr != nil {
			t.Fatal(cerr)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(blob, []byte("THE BYTES HELD"), 0o600); err != nil {
		t.Fatal(err)
	}
	start()
	if f, err := s.OpenBlob(testRoom, tf.sha); err == nil {
		b, _ := io.ReadAll(f)
		f.Close()
		t.Fatalf("a blob no start verified was served: %q", b)
	}
	if err := os.WriteFile(blob, tf.data, 0o600); err != nil {
		t.Fatal(err)
	}
	start()
	f, err := s.OpenBlob(testRoom, tf.sha)
	if err != nil {
		t.Fatalf("bytes that match their name, hashed at first open: %v", err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != string(tf.data) {
		t.Fatalf("served %q", b)
	}
}
