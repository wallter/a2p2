package node

import (
	"context"
	"sync"
	"testing"
)

// A room left while an agent's fetch downloads: the re-check after the
// download finds no list, and the fetch ends with an answer instead of
// dereferencing a list it never obtained (F1 of the fifth review).
//
// Requirements: L1
func TestARoomLeftDuringAFetchEndsTheFetchWithAnAnswer(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	_, res := a.sendFiles(room, "alice", "with a file", writeSource(t, "x.txt", "left while fetching"))
	b.pullAll(room, a)
	b.tickFiles()
	b.inbox(room, "agent-b")
	var once sync.Once
	testHook = func(p string) error {
		if p == "files:downloaded" {
			once.Do(func() {
				if err := b.r.Leave(context.Background(), room); err != nil {
					t.Errorf("leave: %v", err)
				}
			})
		}
		return nil
	}
	t.Cleanup(func() { testHook = nil })
	r, err := b.fetch(room, "agent-b", res.Files[0].SHA256)
	if err == nil && r.State == "" {
		t.Fatalf("the fetch ended without a state or an error: %+v", r)
	}
	if r.State == FetchHere {
		t.Fatalf("a copy was made for a room that was left: %+v", r)
	}
}
