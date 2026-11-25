package store

import (
	"context"
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
	// the gate says yes when the name is reserved and no at publication
	calls := 0
	gate := func() Gate {
		return func(*Reader) (bool, error) { calls++; return calls == 1, nil }
	}
	_, err := s.PublishCopy(ctx, CopyReq{Room: testRoom, Msg: tf.msg.ID(), SHA256: tf.sha, Agent: "alice", Name: "a.txt",
		Size: int64(len(tf.data))}, gate, nil)
	if !errors.Is(err, ErrCopyRefused) || calls != 2 {
		t.Fatalf("publish: %v after %d asks", err, calls)
	}
	dir := filepath.Join(s.FileRoot(), "files", testRoom)
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Fatalf("left in the files directory: %v", ents)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM file_copies`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("copy rows %d %v", n, err)
	}
}

// TestAStartDropsEveryPartSoATransferCutByACrashStartsFromZero (finding 6,
// B5): parts recorded complete but not promoted, with or without their
// file (a crash after the rename), or short, are all gone after a start,
// row and file, so the next transfer starts from 0 and can complete.
// Requirements: L1
func TestAStartDropsEveryPartSoATransferCutByACrashStartsFromZero(t *testing.T) {
	s, path := openTest(t)
	ctx := context.Background()
	tf := newTestFiles(t, s, 1, "", "a part of a file")
	other := newTestFiles(t, s, 2, tf.msg.ID(), "another part")
	short := newTestFiles(t, s, 3, other.msg.ID(), "a short part")
	var paths []string
	for i, x := range []testFiles{tf, other, short} {
		p, err := s.ReservePart(ctx, testRoom, x.msg.ID(), x.sha, int64(len(x.data)), t0)
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 { // the bytes are on disk
			f, err := s.OpenPart(p, 0)
			if err != nil {
				t.Fatal(err)
			}
			n := len(x.data)
			if i == 2 {
				n = 3
			}
			f.Write(x.data[:n])
			f.Close()
		}
		// the row says complete (or, for the short one, more than is there)
		if err := s.PartHave(ctx, p, int64(len(x.data))); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, s.abs(partRel(testRoom, x.msg.ID(), x.sha)))
	}
	s.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.ReconcileFiles(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM file_parts`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("parts after the start: %d %v", n, err)
	}
	for _, p := range paths {
		if exists(p) {
			t.Fatalf("a part file survived the start: %s", p)
		}
	}
	receive(t, s, other.msg.ID(), other.sha, other.data, nil) // from zero, to the end
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
