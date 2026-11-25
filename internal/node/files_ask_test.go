package node

import (
	"context"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

// Requirements: L1, F2. K1: a request names the file's hash in its path,
// so a fetch goes only to a machine that may read the message. B holds
// the list, C joined without history, the author is offline: B's agent
// fetches, no request reaches C and the reader is told the file is
// coming from its author, not here.
func TestAFetchIsNeverSentToAMemberThatMayNotReadTheMessage(t *testing.T) {
	ns := filesNet(t, "a", "b", "c")
	a, b, c := ns[0], ns[1], ns[2]
	room := fileRoom(t, a, b)
	msg, res := a.sendFiles(room, "alice", "before c", writeSource(t, "x.txt", "for b only"))
	sha := res.Files[0].SHA256
	b.pullAll(room, a)
	b.tickFiles()
	if !listHere(t, b, room, msg) {
		t.Fatal("b has no list")
	}
	a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "c"}) // without history
	c.pin(room, a.lnode)
	c.pull(room, a.lnode)
	b.pullAll(room, a)
	if st, err := b.r.State(room); err != nil || len(st.Members) != 3 {
		t.Fatalf("b's view of the room: %+v %v", st, err)
	}
	entryOf(t, b.inbox(room, "agent-b"), msg)
	a.net.down[a.id] = true
	r, err := b.fetch(room, "agent-b", sha)
	if err != nil {
		t.Fatal(err)
	}
	b.fn.mu.Lock()
	asked := append([][3]string(nil), b.fn.asked...)
	b.fn.mu.Unlock()
	for _, q := range asked {
		if q[1] == c.id {
			t.Fatalf("a request for %s reached c, which may not read the message", q[2])
		}
	}
	// the author is the only one asked: it uploads when it next connects
	// (coming), or nobody serves (unavailable); never here.
	if r.State != FetchComing && r.State != FetchUnavailable {
		t.Fatalf("fetch %+v", r)
	}
}

// Requirements: F9. S6: a want past its lifetime is not advertised in a
// pull answer, even before the sweep marks it lapsed.
func TestAWantPastItsLifeIsNotAdvertisedBeforeTheSweep(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	a.net.noInbound[a.id] = true
	room := fileRoom(t, a, b)
	msg, _ := a.sendFiles(room, "alice", "from behind a wall", writeSource(t, "w.txt", "uploaded bytes"))
	a.pullAll(room, b)
	b.tickFiles() // the list: a want to the author
	ctx := context.Background()
	if ws := b.f.wantsFor(ctx, room, a.id, 1<<20); len(ws) != 1 || ws[0].M != msg {
		t.Fatalf("a live want: %+v", ws)
	}
	*b.clock = b.clock.Add(core.WantLife + time.Minute) // no sweep runs
	if ws := b.f.wantsFor(ctx, room, a.id, 1<<20); len(ws) != 0 {
		t.Fatalf("a want past its life was advertised: %+v", ws)
	}
}
