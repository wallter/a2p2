package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/core"
)

// Tests for the fourth review of attachments (fix-files-a4): each fails
// without its fix.

// TestAStartRemovesOnlyTheFilesA2p2GeneratesFromAnOrphanStagingDirectory
// (W2): a staging directory no pending request names loses its numbered
// files; a file with another name stays, is counted, and so the directory
// stays. One holding only a2p2's files goes.
// Requirements: F10
func TestAStartRemovesOnlyTheFilesA2p2GeneratesFromAnOrphanStagingDirectory(t *testing.T) {
	s, _ := openTest(t)
	mixed := s.abs(stageDirRel("q_" + strings.Repeat("1", 32)))
	only := s.abs(stageDirRel("q_" + strings.Repeat("2", 32)))
	for _, p := range []string{filepath.Join(mixed, "0"), filepath.Join(mixed, "notes.txt"), filepath.Join(only, "0"), filepath.Join(only, "7")} {
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
	if exists(filepath.Join(mixed, "0")) || exists(only) {
		t.Error("a start left a2p2's own staging files of no request")
	}
	if !exists(filepath.Join(mixed, "notes.txt")) {
		t.Fatal("a start removed a file a2p2 did not name, with its staging directory")
	}
	if n := s.UnknownFiles(); n != 1 {
		t.Errorf("unknown files = %d, want 1", n)
	}
}

// TestARemovalNotKnownDurableLeavesTheQueueWhenAStartFindsItGone (W6):
// where a directory cannot be synced (Windows), a removal stays queued,
// marked unlinked, and a start that finds the path gone finishes it; an
// entry whose path holds a file that is provably not a2p2's is finished
// at once, on every system. The queue never keeps an entry for ever.
// Requirements: F10
func TestARemovalNotKnownDurableLeavesTheQueueWhenAStartFindsItGone(t *testing.T) {
	for _, durable := range []bool{true, false} {
		was := dirSyncDurable
		dirSyncDurable = durable
		s, _ := openTest(t)
		ctx := context.Background()
		tf := newTestFiles(t, s, 1, "", "the same bytes")
		receive(t, s, tf.msg.ID(), tf.msha, tf.man, &tf.m)
		receive(t, s, tf.msg.ID(), tf.sha, tf.data, nil)
		c, err := s.PublishCopy(ctx, CopyReq{Room: testRoom, Msg: tf.msg.ID(), SHA256: tf.sha, Agent: "alice", Name: "a.txt",
			Size: int64(len(tf.data))}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		p := s.CopyPath(c.Path)
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, tf.data, 0o600); err != nil { // not the file a2p2 published
			t.Fatal(err)
		}
		wd := mk(t, fileKey, core.RecordFields{Seq: 2, Prev: tf.msg.ID(), Kind: core.KindWithdraw, Ref: tf.msg.ID()})
		wantOutcome(t, put(t, s, wd, t0), OutcomeStored)
		if _, err := s.SweepFiles(ctx, testRoom, "", t0); err != nil {
			t.Fatal(err)
		}
		s.RunTrash(ctx)
		count := func(where string) int {
			var n int
			if err := s.db.QueryRow(`SELECT count(*) FROM file_trash ` + where).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		if n := count(`WHERE unlinked = 0`); n != 0 {
			t.Errorf("durable %v: %d entries neither finished nor marked unlinked", durable, n)
		}
		if n := count(`WHERE kind = 'copy'`); n != 0 {
			t.Errorf("durable %v: the entry of a path holding a file a2p2 did not publish is kept", durable)
		}
		if err := s.ReconcileFiles(ctx); err != nil {
			t.Fatal(err)
		}
		if n := count(``); n != 0 {
			t.Errorf("durable %v: %d entries left after a start found their paths gone", durable, n)
		}
		if b, err := os.ReadFile(p); err != nil || string(b) != string(tf.data) {
			t.Fatalf("a file a2p2 did not publish was removed: %q %v", b, err)
		}
		dirSyncDurable = was
	}
}
