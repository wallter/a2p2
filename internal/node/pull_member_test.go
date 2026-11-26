package node

// A pull's answer from a peer removed while it was on the way, and one
// whose pages remove the peer and then admit it again (T4 on the pull
// path, Exchange.apply).

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// change has admin sign and approve one change of room.
func change(t *testing.T, admin *snode, room string, c Change) {
	t.Helper()
	ctx := context.Background()
	q, err := admin.r.proposeAny(ctx, room, c)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := admin.r.Confirm(ctx, q.ID, true); err != nil || res.State != store.RequestApproved {
		t.Fatalf("confirm %+v: %+v %v", c, res, err)
	}
}

// offerOf is admin's whole log of room, as wires.
func offerOf(t *testing.T, admin *snode, room string) [][]byte {
	t.Helper()
	ws, err := admin.r.Offer(room)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

// A peer removed while its answer is on the way: the answer carries a log
// entry this machine lacks, signed after the removal, and none of it is
// stored; the pull ends as "not a member" and the room's state here is as
// the removal left it.
//
// Requirements: T4
func TestAPullsAnswerFromAPeerRemovedWhileItWasOnTheWayStoresNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		net := newRecNet()
		z, a, b := newSNode(t, "z", net), newSNode(t, "a", net), newSNode(t, "b", net)
		room := sgroup(t, z, a, b)
		if _, err := b.x.Pull(ctx, room, a.id, PullOpts{}); err != nil {
			t.Fatal(err)
		}
		change(t, z, room, Change{Kind: core.EntryRemove, Target: a.id})
		removal := offerOf(t, z, room)
		change(t, z, room, Change{Kind: core.EntryAdd, Target: junkNode(1), Name: "late", History: true})
		if _, err := a.r.Receive(ctx, room, 0, offerOf(t, z, room)...); err != nil {
			t.Fatal(err)
		}
		hit := false
		net.memNet.mu.Lock()
		net.lose = func(from, _ string, _ *PullRequest, resp *PullResponse) bool {
			if from == b.id && !hit {
				hit = true
				if len(resp.Log.Entries) == 0 {
					t.Error("the answer carries no log entries: the test shows nothing")
				}
				// the removal reaches b from z while a's answer is on the way
				if _, err := b.r.Receive(ctx, room, 0, removal...); err != nil {
					t.Error(err)
				}
			}
			return false
		}
		net.memNet.mu.Unlock()
		_, err := b.x.Pull(ctx, room, a.id, PullOpts{})
		if !errors.Is(err, ErrPeerNotMember) {
			t.Fatalf("pull: %v, want ErrPeerNotMember", err)
		}
		st, err := b.r.State(room)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(st.Members, a.id) {
			t.Fatal("the removal was not applied: the test shows nothing")
		}
		if slices.Contains(st.Members, junkNode(1)) {
			t.Fatal("the removed peer's entry was stored")
		}
		if got, want := len(offerOf(t, b, room)), len(removal); got != want {
			t.Fatalf("b holds %d log entries, want %d (the log up to the removal)", got, want)
		}
	})
}

// The log this machine lacks removes the peer it pulls from and, more than
// a page later, admits it again. The first page removes it (it was a
// member before those entries), the exchange goes on for the log's more,
// and the page that admits it again is taken, as T4 allows: the whole log
// arrives in one exchange, with the peer's records.
//
// Requirements: T4
func TestAPullWhoseLogRemovesThePeerAndAPageLaterAdmitsItAgainTakesTheWholeLog(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		net := newRecNet()
		z, a, b := newSNode(t, "z", net), newSNode(t, "a", net), newSNode(t, "b", net)
		room := sgroup(t, z, a, b)
		if _, err := b.x.Pull(ctx, room, a.id, PullOpts{}); err != nil {
			t.Fatal(err)
		}
		change(t, z, room, Change{Kind: core.EntryRemove, Target: a.id})
		for i := range MaxLogEntries + 44 {
			change(t, z, room, Change{Kind: core.EntryAdd, Target: junkNode(100 + i), Name: "filler", History: true})
		}
		change(t, z, room, Change{Kind: core.EntryAdd, Target: a.id, Name: "again", History: true})
		whole := offerOf(t, z, room)
		if _, err := a.r.Receive(ctx, room, 0, whole...); err != nil {
			t.Fatal(err)
		}
		if st, _ := a.r.State(room); !slices.Contains(st.Members, a.id) {
			t.Fatal("a is not a member again: the test shows nothing")
		}
		id := a.send(room, "after the re-admission")
		st, err := b.x.Pull(ctx, room, a.id, PullOpts{})
		if err != nil {
			t.Fatalf("pull: %v (%+v)", err, st)
		}
		if st.Entries < 2 {
			t.Fatalf("the log came in %d batch(es), want two or more: the test shows nothing", st.Entries)
		}
		if got := len(offerOf(t, b, room)); got != len(whole) {
			t.Fatalf("b holds %d log entries, want %d", got, len(whole))
		}
		if !b.holds(room, id) {
			t.Fatal("the peer's record was not taken after its re-admission")
		}
	})
}

// As above, but no later page admits the peer again: the page that removes
// it is taken (it was a member before those entries), the next one, from
// a peer that is no longer a member here, is not, and the pull ends as
// "not a member".
//
// Requirements: T4
func TestAPullWhoseLogRemovesThePeerTakesNothingAfterThePageThatRemovesIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		net := newRecNet()
		z, a, b := newSNode(t, "z", net), newSNode(t, "a", net), newSNode(t, "b", net)
		room := sgroup(t, z, a, b)
		if _, err := b.x.Pull(ctx, room, a.id, PullOpts{}); err != nil {
			t.Fatal(err)
		}
		before := len(offerOf(t, z, room))
		change(t, z, room, Change{Kind: core.EntryRemove, Target: a.id})
		for i := range MaxLogEntries + 44 {
			change(t, z, room, Change{Kind: core.EntryAdd, Target: junkNode(100 + i), Name: "filler", History: true})
		}
		if _, err := a.r.Receive(ctx, room, 0, offerOf(t, z, room)...); err != nil {
			t.Fatal(err)
		}
		_, err := b.x.Pull(ctx, room, a.id, PullOpts{})
		if !errors.Is(err, ErrPeerNotMember) {
			t.Fatalf("pull: %v, want ErrPeerNotMember", err)
		}
		if got, want := len(offerOf(t, b, room)), before+MaxLogEntries; got != want {
			t.Fatalf("b holds %d log entries, want %d (up to the first page)", got, want)
		}
	})
}
