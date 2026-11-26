package node

// Security group H after the second review (D63): what a send with files
// does before any path is touched, and what its refusals have in common.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// countHooks counts the times each test point is reached until the test
// ends.
func countHooks(t *testing.T) map[string]int {
	t.Helper()
	n := map[string]int{}
	testHook = func(p string) error { n[p]++; return nil }
	t.Cleanup(func() { testHook = nil })
	return n
}

// TestAnInvalidEntryBesideAPathIsRefusedBeforeAnyPathIsOpened (D63, after
// the second review): a send naming a readable file beside an entry that
// can never be staged (a sha256: entry with nothing replaced, an unlisted
// hash, a relative path) used to open, size, copy and hash the file
// before it was refused, with no request raised, so an agent could learn
// whether a path was readable and how large it was. It is now refused
// before any path is opened, and its answer through a surface is the one
// staging refusal, the same as for a path that cannot be read.
// Requirements: L1, P3
func TestAnInvalidEntryBesideAPathIsRefusedBeforeAnyPathIsOpened(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	secret := writeSource(t, "secret.txt", strings.Repeat("s", 4096))
	missing := filepath.Join(t.TempDir(), "missing.txt")
	hits := countHooks(t)
	for _, bad := range []string{KeptPrefix + strings.Repeat("0", 64), KeptPrefix + "not-a-hash", "relative.txt"} {
		paths := []string{secret, bad}
		_, err := a.askSend(room, paths...)
		var ue *UserError
		if !errors.As(err, &ue) || ue.Code != ErrorInvalid {
			t.Fatalf("%v: %v", paths, err)
		}
		if hits["files:open"] != 0 || hits["files:reserve"] != 0 {
			t.Fatalf("%v: %d paths opened, %d reservations, before the refusal", paths, hits["files:open"], hits["files:reserve"])
		}
		// the answer does not depend on the path: the same for one that
		// does not exist, in either order
		for _, other := range [][]string{{missing, bad}, {bad, secret}} {
			if _, err2 := a.askSend(room, other...); err2 == nil || err2.Error() != err.Error() {
				t.Fatalf("%v: %v; %v: %v", paths, err, other, err2)
			}
		}
		got := cannotAttachErr(err, paths)
		want := cannotAttachErr(func() error { _, err := a.askSend(room, missing); return err }(), []string{missing})
		if got.Error() != want.Error() {
			t.Fatalf("%v: %q, want the words of any staging refusal %q", paths, got, want)
		}
		delete(hits, "files:open")
		delete(hits, "files:reserve")
	}
	if n := stagingLeft(t, a); n != 0 {
		t.Fatalf("%d staged", n)
	}
	if qs, err := a.st.Requests(context.Background(), room, store.RequestPending); err != nil || len(qs) != 0 {
		t.Fatalf("requests %d %v", len(qs), err)
	}
	// the honest case: the same file alone is staged and asked for
	res, err := a.askSend(room, secret)
	if err != nil || res.State != "waiting" || len(res.Files) != 1 || res.Files[0].Size != 4096 {
		t.Fatalf("an honest send: %+v %v", res, err)
	}
}

// TestEveryRefusalBeforeCopyingTakesTheSameSteps (D63, after the second
// review): a send refused before any byte is copied, because a path
// cannot be read, a file is over the size limit or the files do not fit
// the budget, opens every path it names and reads the budget before it
// answers, and answers with the same words, so neither the words nor the
// steps tell which. Before, an unreadable path was refused at once, and
// a readable one only after the budget was read.
// Requirements: L1, P3
func TestEveryRefusalBeforeCopyingTakesTheSameSteps(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	small := writeSource(t, "small.txt", "small")
	missing := filepath.Join(t.TempDir(), "missing.txt")
	big := filepath.Join(t.TempDir(), "big.bin")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(core.MaxFileSize + 1); err != nil { // sparse: nothing is written
		t.Fatal(err)
	}
	f.Close()
	overBudget := writeSource(t, "over.txt", strings.Repeat("o", 3000))
	a.st.SetFileBudgets(2000, 20000)
	hits := countHooks(t)
	var words []string
	for _, paths := range [][]string{{missing, small}, {big, small}, {overBudget, small}, {small, missing}} {
		_, err := a.askSend(room, paths...)
		if err == nil {
			t.Fatalf("%v: staged", paths)
		}
		if hits["files:open"] != 2 || hits["files:reserve"] != 1 {
			t.Fatalf("%v: %d paths opened, %d budget reads, want 2 and 1", paths, hits["files:open"], hits["files:reserve"])
		}
		words = append(words, cannotAttachErr(err, paths).Error())
		delete(hits, "files:open")
		delete(hits, "files:reserve")
	}
	for _, w := range words[1:] {
		if w != words[0] {
			t.Fatalf("answers differ: %q", words)
		}
	}
	if n := stagingLeft(t, a); n != 0 {
		t.Fatalf("%d staged", n)
	}
	// the honest case: what fits is asked for
	if res, err := a.askSend(room, small); err != nil || res.State != "waiting" {
		t.Fatalf("an honest send: %+v %v", res, err)
	}
}
