package node

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Requirements: D6, F10. K4: no lock order can stop pulls. A wanted upload
// is promoted (the store's file lock held, the gate about to read the
// room's membership) while the person leaves another room (the rooms
// lock held, its deletions waiting for the file lock). Both finish within
// the deadline, every time, and pulls go on.
func TestAPromotionAndALeaveAtTheSameTimeBothFinish(t *testing.T) {
	// One hook for the whole test, switched under a lock: background
	// goroutines (a lease's watcher) may read testHook after a run ends.
	var mu sync.Mutex
	var at func()
	testHook = func(p string) error {
		mu.Lock()
		f := at
		if p == "files:gate" {
			at = nil
		}
		mu.Unlock()
		if p == "files:gate" && f != nil {
			f()
		}
		return nil
	}
	t.Cleanup(func() { testHook = nil })
	for i := range 8 {
		ns := filesNet(t, "a", "b")
		a, b := ns[0], ns[1]
		a.net.noInbound[a.id] = true
		room := fileRoom(t, a, b)
		other := b.create("other")
		msg, res := a.sendFiles(room, "alice", "from behind a wall", writeSource(t, "w.txt", "uploaded bytes"))
		a.pullAll(room, b)
		b.tickFiles()
		a.pullAll(room, b) // the list, by upload
		if !listHere(t, b, room, msg) {
			t.Fatal("the list did not come by upload")
		}
		b.inbox(room, "agent-b")
		if r, err := b.fetch(room, "agent-b", res.Files[0].SHA256); err != nil || r.State != FetchComing {
			t.Fatalf("first fetch %+v %v", r, err)
		}
		left := make(chan error, 1)
		mu.Lock()
		at = func() {
			go func() { left <- b.r.Leave(context.Background(), other) }()
			time.Sleep(time.Duration(5+i*5) * time.Millisecond) // the leave holds the rooms lock and waits
		}
		mu.Unlock()
		done := make(chan struct{})
		go func() { a.pullAll(room, b); close(done) }() // the upload, promoted under the file lock
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatalf("run %d: the promotion did not finish (a lock cycle with the leave)", i)
		}
		select {
		case err := <-left:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("run %d: the leave did not finish", i)
		}
		if how := heldFor(t, b, room, res.Files[0].SHA256, msg); how == "" {
			t.Fatalf("run %d: the upload was not promoted", i)
		}
		b.send(room, "agent-b", "pulls go on")
		a.pullAll(room, b)
	}
}
