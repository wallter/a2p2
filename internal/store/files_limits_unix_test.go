//go:build !windows

package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/core"
)

// TestAWithdrawnBlobWhoseRemovalFailsStillCounts: a withdrawn file's blob
// is queued by the sweep; its removal fails (the directory is read-only,
// as a file held open is on Windows). While its file is on disk its bytes
// count against the room's and the machine's budget; once a later run
// removes it they stop counting.
// Requirements: F7, F10
func TestAWithdrawnBlobWhoseRemovalFailsStillCounts(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	s, _ := openTest(t)
	ctx := context.Background()
	tf := newTestFiles(t, s, 1, "", "bytes withdrawn but still on disk")
	receive(t, s, tf.msg.ID(), tf.sha, tf.data, nil)
	held, _ := usageOf(t, s)
	if held < int64(len(tf.data)) {
		t.Fatalf("held bytes not counted: %d", held)
	}
	dir := s.abs("blobs/" + testRoom)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	wd := mk(t, fileKey, core.RecordFields{Seq: 2, Prev: tf.msg.ID(), Kind: core.KindWithdraw, Ref: tf.msg.ID()})
	wantOutcome(t, put(t, s, wd, t0), OutcomeStored)
	_, err := s.SweepFiles(ctx, testRoom, "", t0)
	if err == nil {
		s.RunTrash(ctx)
	}
	if cerr := os.Chmod(dir, 0o700); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !exists(s.blobPath(testRoom, tf.sha)) {
		t.Fatal("the blob was removed through a read-only directory")
	}
	if r, m := usageOf(t, s); r != held || m != held {
		t.Errorf("a queued blob on disk: usage %d, %d, want %d", r, m, held)
	}
	s.RunTrash(ctx)
	if exists(s.blobPath(testRoom, tf.sha)) {
		t.Fatal("the next run did not remove the blob")
	}
	if r, m := usageOf(t, s); r != 0 || m != 0 {
		t.Errorf("after its removal: usage %d, %d", r, m)
	}
}

// TestARemovedBlobThatComesBackAndCannotBeRemovedIsChargedAgain: where a
// directory cannot be synced (Windows, taken here by dirSyncDurable), a
// failed publication's blobs are removed and marked unlinked. A power loss
// puts their files back; at the next start their removal fails (the
// directory is read-only). Their rows are charged again and retried by
// the next ordinary run, which removes them, and then they stop counting.
// Requirements: F7, F10
func TestARemovedBlobThatComesBackAndCannotBeRemovedIsChargedAgain(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	was := dirSyncDurable
	dirSyncDurable = false
	t.Cleanup(func() { dirSyncDurable = was })
	s, _ := openTest(t)
	ctx := context.Background()
	sf := stagedSend(t, s, "q_"+strings.Repeat("8", 32), "bytes that come back", t0)
	r0, m0 := usageOf(t, s)
	if _, err := s.WriteWithFiles(ctx, t0, sf, core.Manifest{}, func(*Reader) (*core.VerifiedRecord, error) {
		return nil, errors.New("the record cannot be built (test)")
	}); err == nil {
		t.Fatal("the publication succeeded")
	}
	rows, err := s.db.Query(`SELECT path, size FROM file_trash WHERE kind = 'blob' AND unlinked = 1`)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	var back int64
	for rows.Next() {
		var p string
		var n int64
		if err := rows.Scan(&p, &n); err != nil {
			t.Fatal(err)
		}
		paths, back = append(paths, p), back+n
	}
	rows.Close()
	if len(paths) != 2 {
		t.Fatalf("%d blob removals marked unlinked, want 2", len(paths))
	}
	for _, p := range paths { // the power loss undid the removals
		if err := os.WriteFile(s.abs(p), make([]byte, 1), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dir := s.abs("blobs/" + testRoom)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	err = s.ReconcileFiles(ctx) // the start: each removal fails
	if cerr := os.Chmod(dir, 0o700); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if !exists(s.abs(p)) {
			t.Fatalf("%s was removed through a read-only directory", p)
		}
	}
	if r, m := usageOf(t, s); r != r0+back || m != m0+back {
		t.Errorf("after the start: usage %d, %d, want %d, %d: bytes on disk are not charged", r, m, r0+back, m0+back)
	}
	s.RunTrash(ctx) // an ordinary run retries them, and now succeeds
	for _, p := range paths {
		if exists(s.abs(p)) {
			t.Errorf("%s was not retried by the next run", p)
		}
	}
	if r, m := usageOf(t, s); r != r0 || m != m0 {
		t.Errorf("after the retry: usage %d, %d, want %d, %d", r, m, r0, m0)
	}
}
