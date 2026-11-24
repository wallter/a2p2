package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/core"
)

// Tests for the findings of the review of the attachments mechanism
// (fix-files-a): each fails without its fix.

// TestACopyIsNotPublishedWhenItsPermissionEndsWhileItIsWritten (finding 3):
// the permission asked again at publication says no: no file at the name,
// no temporary file, the row not written.
// Requirements: L1, T5
func TestACopyIsNotPublishedWhenItsPermissionEndsWhileItIsWritten(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	tf := newTestFiles(t, s, 1, "", "revoked copy")
	receive(t, s, tf.msg.ID(), tf.sha, tf.data, nil)
	c, _, err := s.ReserveCopy(ctx, testRoom, tf.msg.ID(), tf.sha, "alice", "a.txt", int64(len(tf.data)))
	if err != nil {
		t.Fatal(err)
	}
	err = s.PublishCopy(ctx, c, func(*Reader) (bool, error) { return false, nil })
	if !errors.Is(err, ErrCopyRefused) {
		t.Fatalf("publish: %v", err)
	}
	dir := filepath.Dir(s.CopyPath(c.Path))
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Fatalf("left in the files directory: %v", ents)
	}
	if err := s.CopyDone(ctx, c); err == nil {
		t.Fatal("the copy is marked written")
	}
}

// TestAQueuedDeletionThatFailsIsKeptAndTriedAgain (finding 5).
// Requirements: L1
func TestAQueuedDeletionThatFailsIsKeptAndTriedAgain(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	rel := partRel(testRoom, strings.Repeat("1", 32), strings.Repeat("f", 64))
	p := s.abs(rel)
	// a path that cannot be removed now: a directory with something in it
	if err := os.MkdirAll(filepath.Join(p, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.tx(ctx, func(tx *sql.Tx) error {
		return trash(tx, rel, "part", testRoom, strings.Repeat("1", 32), strings.Repeat("f", 64), 0, t0)
	}); err != nil {
		t.Fatal(err)
	}
	s.RunTrash(ctx)
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM file_trash WHERE path = ?`, rel).Scan(&n); err != nil || n != 1 {
		t.Fatalf("a failed deletion was forgotten: %d %v", n, err)
	}
	if err := os.RemoveAll(filepath.Join(p, "x")); err != nil {
		t.Fatal(err)
	}
	s.RunTrash(ctx)
	if err := s.db.QueryRow(`SELECT count(*) FROM file_trash WHERE path = ?`, rel).Scan(&n); err != nil || n != 0 || exists(p) {
		t.Fatalf("the retry did not complete: %d %v", n, err)
	}
}

// TestAStartResetsAPartWhoseFileIsMissingOrShort (finding 6).
// Requirements: L1
func TestAStartResetsAPartWhoseFileIsMissingOrShort(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	tf := newTestFiles(t, s, 1, "", "a part of a file")
	p, err := s.ReservePart(ctx, testRoom, tf.msg.ID(), tf.sha, int64(len(tf.data)), t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PartHave(ctx, p, int64(len(tf.data))); err != nil { // the row says complete; no file (a crash after a rename)
		t.Fatal(err)
	}
	if err := s.ReconcileFiles(ctx); err != nil {
		t.Fatal(err)
	}
	var got FilePart
	_ = s.View(ctx, func(rd *Reader) (err error) {
		got, _, err = rd.FilePart(testRoom, tf.msg.ID(), tf.sha)
		return err
	})
	if got.Have != 0 {
		t.Fatalf("a part with no file kept have %d", got.Have)
	}
}

// TestAWithdrawnListDoesNotSeedAnotherRecordsList (finding 8).
// Requirements: L1, T5
func TestAWithdrawnListDoesNotSeedAnotherRecordsList(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	tf := newTestFiles(t, s, 1, "", "listed once")
	receive(t, s, tf.msg.ID(), tf.msha, tf.man, &tf.m)
	wd := mk(t, fileKey, core.RecordFields{Seq: 2, Prev: tf.msg.ID(), Kind: core.KindWithdraw, Ref: tf.msg.ID()})
	wantOutcome(t, put(t, s, wd, t0), OutcomeStored)
	if _, err := s.SweepFiles(ctx, testRoom, "", t0); err != nil {
		t.Fatal(err)
	}
	var items []core.FileRef
	_ = s.View(ctx, func(rd *Reader) (err error) {
		items, err = rd.FileItems(testRoom, tf.msg.ID())
		return err
	})
	if len(items) != 0 {
		t.Fatalf("the withdrawn list kept %v", items)
	}
	other := mk(t, fileKey, core.RecordFields{Seq: 3, Prev: wd.ID(), Files: tf.msha})
	wantOutcome(t, put(t, s, other, t0), OutcomeStored)
	var ok bool
	_ = s.View(ctx, func(rd *Reader) (err error) {
		_, ok, err = rd.FileSet(testRoom, other.ID())
		return err
	})
	if ok {
		t.Fatal("a withdrawn list seeded another record's list")
	}
}

// TestStagingReservesTheBudgetBeforeCopying (finding 9): two requests
// being staged at once cannot together pass the budget.
// Requirements: L1
func TestStagingReservesTheBudgetBeforeCopying(t *testing.T) {
	s, _ := openTest(t)
	s.SetFileBudgets(100, 0)
	ctx := context.Background()
	q1, q2 := "q_"+strings.Repeat("1", 32), "q_"+strings.Repeat("2", 32)
	if err := s.ReserveStaging(ctx, testRoom, q1, 60); err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveStaging(ctx, testRoom, q2, 60); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("the second reservation: %v", err)
	}
	s.ReleaseStaging(q1)
	if err := s.ReserveStaging(ctx, testRoom, q2, 60); err != nil {
		t.Fatalf("after the first ended: %v", err)
	}
}
