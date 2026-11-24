package store

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/wallter/a2p2/internal/core"
)

// [D9] After a message is withdrawn and the store is scrubbed, its text is
// in none of the store's files: not in the database (secure_delete) and
// not in the write-ahead log (checkpoint and truncate). Found by the
// container fault tests, which grep the files of every member.
func TestWithdrawnTextIsInNoFileOfTheStoreAfterAScrub(t *testing.T) {
	s, path := openTest(t)
	a := testKey("a")
	secret := "sentinel-7f3a9c-withdrawn-text-must-not-stay-on-disk"
	m := mk(t, a, core.RecordFields{Text: secret})
	wantOutcome(t, put(t, s, m, t0), OutcomeStored)
	if !storeFilesContain(t, path, secret) {
		t.Fatal("the test cannot see the text on disk before the withdraw, so it proves nothing")
	}
	w := mk(t, a, core.RecordFields{Seq: 2, Prev: m.ID(), Kind: core.KindWithdraw, Ref: m.ID()})
	wantOutcome(t, put(t, s, w, t0), OutcomeStored)
	wantReason(t, s, m.ID(), ReasonWithdrawn)
	if err := s.Scrub(context.Background()); err != nil {
		t.Fatal(err)
	}
	if storeFilesContain(t, path, secret) {
		t.Fatal("withdrawn text is still in a file of the store after a scrub")
	}
}

// storeFilesContain reports whether any file beside the database (the
// database itself, its write-ahead log, its shared-memory file) holds needle.
func storeFilesContain(t *testing.T, dbPath, needle string) bool {
	t.Helper()
	files, err := filepath.Glob(dbPath + "*")
	if err != nil || len(files) == 0 {
		t.Fatalf("no store files at %s: %v", dbPath, err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(needle)) {
			return true
		}
	}
	return false
}
