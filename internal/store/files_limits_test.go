package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

// Tests for the known limits of attachments: each fails without its fix.

// stagedSend raises a pending send request in testRoom with one staged
// file, created at created, and returns what its approval publishes.
func stagedSend(t *testing.T, s *Store, id, body string, created time.Time) SendFiles {
	t.Helper()
	src := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := s.StageFile(id, 0, src, core.MaxFileSize)
	if err != nil {
		t.Fatal(err)
	}
	f := StagedFile{N: 0, SHA256: st.SHA256, Size: st.Size}
	draft, _ := json.Marshal(draftFiles{Files: []StagedFile{f}})
	q := Request{ID: id, Kind: RequestSendFiles, Room: testRoom, Created: created, State: RequestPending, Draft: string(draft)}
	if err := s.CreateSendRequest(context.Background(), q, []StagedFile{f}, 0); err != nil {
		t.Fatal(err)
	}
	return SendFiles{Request: id, Room: testRoom, Manifest: []byte(`{"the":"manifest"}`), Files: []StagedFile{f}}
}

// usageOf is the room's and the machine's file usage.
func usageOf(t *testing.T, s *Store) (int64, int64) {
	t.Helper()
	var r, m int64
	if err := s.View(context.Background(), func(rd *Reader) (err error) {
		r, m, err = rd.FileUsage(testRoom)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return r, m
}

// roomBlobs lists the files in testRoom's blob directory.
func roomBlobs(t *testing.T, s *Store) []string {
	t.Helper()
	ents, err := os.ReadDir(s.abs("blobs/" + testRoom))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// TestARequestThatExpiresWhileItsBytesArePlacedIsRefusedByThePublication
// (B6, F3): the request is live when its bytes start to be put in place
// and past its life when the publishing transaction reads the clock; the
// transaction refuses, the blobs written go, and nothing more counts.
// Requirements: L1, F7
func TestARequestThatExpiresWhileItsBytesArePlacedIsRefusedByThePublication(t *testing.T) {
	s, _ := openTest(t)
	sf := stagedSend(t, s, "q_"+strings.Repeat("6", 32), "copied while the clock runs out", t0)
	r0, m0 := usageOf(t, s)
	reads := 0
	sf.Clock = func() time.Time { // live before the copy, expired after it
		reads++
		if reads == 1 {
			return t0
		}
		return t0.Add(core.StagingLife)
	}
	built := false
	_, err := s.WriteWithFiles(context.Background(), t0, sf, core.Manifest{}, func(*Reader) (*core.VerifiedRecord, error) {
		built = true
		return nil, errors.New("not reached")
	})
	if !errors.Is(err, ErrSendExpired) || built || reads != 2 {
		t.Fatalf("expired inside the publication: %v, built %v, clock read %d times", err, built, reads)
	}
	if left := roomBlobs(t, s); len(left) != 0 {
		t.Errorf("blobs left on disk: %v", left)
	}
	if r, m := usageOf(t, s); r != r0 || m != m0 {
		t.Errorf("usage %d, %d, want %d, %d", r, m, r0, m0)
	}
}

// TestAStartLeavesAndCountsNearMissesOfTheNamesA2p2Generates (F4): an
// orphan staging directory holds a file number and a temporary file
// exactly as a2p2 names them, beside names that only resemble a temporary
// file's (wrong length, upper case, wrong prefix, an extra suffix) and a
// directory. A start removes only the two; every near miss and the inner
// directory stay, and each is counted, with the staging directory kept
// for them.
// Requirements: F10
func TestAStartLeavesAndCountsNearMissesOfTheNamesA2p2Generates(t *testing.T) {
	s, _ := openTest(t)
	dir := s.abs(stageDirRel("q_" + strings.Repeat("7", 32)))
	hex24 := strings.Repeat("0123456789abcdef", 2)[:24]
	ours := []string{"0", tmpPrefix + hex24}
	near := []string{
		tmpPrefix + hex24[:23],                     // too short
		tmpPrefix + hex24 + "f",                    // too long
		tmpPrefix + "FEDCBA9876543210FEDCBA98",     // upper case (not ours folded: some file systems ignore case)
		".a2p2-tmq-" + hex24,                       // another prefix
		"a2p2-tmp-" + hex24,                        // the prefix without its dot
		tmpPrefix + hex24 + ".bak",                 // an extra suffix
		tmpPrefix + hex24[:22] + "g0",              // not hexadecimal
		strings.TrimSuffix(tmpPrefix, "-") + hex24, // the prefix cut short
	}
	if err := os.MkdirAll(filepath.Join(dir, "kept-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range append(append([]string{}, ours...), near...) {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if !stagingName(tmpName()) {
		t.Fatal("a name the generator makes is not recognised")
	}
	if err := s.ReconcileFiles(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, n := range ours {
		if exists(filepath.Join(dir, n)) {
			t.Errorf("a start left %q, a name a2p2 generates", n)
		}
	}
	for _, n := range append(near, "kept-dir") {
		if !exists(filepath.Join(dir, n)) {
			t.Errorf("a start removed %q, a name a2p2 does not generate", n)
		}
	}
	// each near miss, the inner directory, and the staging directory kept
	if got, want := s.UnknownFiles(), int64(len(near)+2); got != want {
		t.Errorf("unknown files = %d, want %d", got, want)
	}
}

// TestBlobsRemovedWhereADirectoryCannotBeSyncedStopCountingAtOnce: a
// publication fails after its bytes are placed. Where a removal is not
// known durable until a start (Windows, taken here by dirSyncDurable), the
// blobs it wrote are removed and their queue entries stay, marked
// unlinked; their bytes stop counting against the room's and the
// machine's budget at once, without waiting for a start.
// Requirements: F7, F10
func TestBlobsRemovedWhereADirectoryCannotBeSyncedStopCountingAtOnce(t *testing.T) {
	for _, durable := range []bool{true, false} {
		was := dirSyncDurable
		dirSyncDurable = durable
		s, _ := openTest(t)
		sf := stagedSend(t, s, "q_"+strings.Repeat("3", 32), "bytes put in place, then undone", t0)
		r0, m0 := usageOf(t, s) // the request's staged bytes: it still waits
		_, err := s.WriteWithFiles(context.Background(), t0, sf, core.Manifest{}, func(*Reader) (*core.VerifiedRecord, error) {
			return nil, errors.New("the record cannot be built (test)")
		})
		if err == nil {
			t.Fatal("a publication whose record cannot be built succeeded")
		}
		if left := roomBlobs(t, s); len(left) != 0 {
			t.Errorf("durable %v: blobs left on disk: %v", durable, left)
		}
		var unlinked int
		if err := s.db.QueryRow(`SELECT count(*) FROM file_trash WHERE kind = 'blob' AND unlinked = 1`).Scan(&unlinked); err != nil {
			t.Fatal(err)
		}
		if !durable && unlinked != 2 {
			t.Fatalf("%d blob removals marked unlinked, want 2: the Windows path was not taken", unlinked)
		}
		if r, m := usageOf(t, s); r != r0 || m != m0 {
			t.Errorf("durable %v: removed bytes still count: room %d, machine %d, want %d, %d", durable, r, m, r0, m0)
		}
		dirSyncDurable = was
	}
}
