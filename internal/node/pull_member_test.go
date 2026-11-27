package node

// A pull's answer from a peer removed while it was on the way, and one
// whose pages remove the peer and then admit it again (T4 on the pull
// path, Exchange.apply).

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

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

// requestsFrom records every request from src that reaches a server, as the
// server got it.
func requestsFrom(net *recNet, src string) func() []PullRequest {
	var mu sync.Mutex
	var qs []PullRequest
	net.memNet.mu.Lock()
	net.lose = func(from, _ string, q *PullRequest, _ *PullResponse) bool {
		if from == src {
			mu.Lock()
			qs = append(qs, *q)
			mu.Unlock()
		}
		return false
	}
	net.memNet.mu.Unlock()
	return func() []PullRequest {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(qs)
	}
}

// The log this machine lacks removes the peer, then a whole page of other
// entries (the intervening page), then admits it again. The page that
// removes it is taken; the request after it, to a peer no longer a member,
// carries only the read: the sum of an empty summary and this machine's
// log heads and extra, with no seen, addresses, peers, log entries, detail
// or offer, although this machine has an address and a record the peer
// lacks. The intervening page, from a non-member, is not taken, so the
// re-admission is not reached through it; the pull ends "not a member".
// The rest of the log then comes from another member, and the next pull
// from the peer, a member again, takes its record.
//
// Requirements: T4, T5
func TestARemovedPeerIsSentOnlyTheReadAndAnInterveningPageIsNotTaken(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		net := newRecNet()
		z, a, b := newSNode(t, "z", net), newSNode(t, "a", net), newSNode(t, "b", net)
		b.x.addrs = []string{"192.0.2.7:4100"}
		room := sgroup(t, z, a, b)
		if _, err := b.x.Pull(ctx, room, a.id, PullOpts{}); err != nil {
			t.Fatal(err)
		}
		before := len(offerOf(t, z, room))
		change(t, z, room, Change{Kind: core.EntryRemove, Target: a.id})
		for i := range 2*MaxLogEntries + 44 {
			change(t, z, room, Change{Kind: core.EntryAdd, Target: junkNode(100 + i), Name: "filler", History: true})
		}
		change(t, z, room, Change{Kind: core.EntryAdd, Target: a.id, Name: "again", History: true})
		whole := offerOf(t, z, room)
		if _, err := a.r.Receive(ctx, room, 0, whole...); err != nil {
			t.Fatal(err)
		}
		fromA := a.send(room, "after the re-admission")
		mine := b.send(room, "b's own, which a lacks")
		got := requestsFrom(net, b.id)
		_, err := b.x.Pull(ctx, room, a.id, PullOpts{Offer: true})
		if !errors.Is(err, ErrPeerNotMember) {
			t.Fatalf("pull: %v, want ErrPeerNotMember", err)
		}
		qs := got()
		if len(qs) != 2 {
			t.Fatalf("%d requests, want the opening and one read", len(qs))
		}
		if q := qs[0]; len(q.Offer) == 0 || len(q.Addrs) == 0 {
			t.Fatalf("the opening carried no offer or addresses: the test shows nothing (%+v)", q)
		}
		heads, extra := func() ([]string, []string) {
			b.r.mu.Lock()
			defer b.r.mu.Unlock()
			return b.r.rooms[room].logHeads()
		}()
		want := PullRequest{V: PullVersion, Room: room, Sum: sumOf(nil), Log: PullLog{Heads: heads, Extra: extra}}
		if q := qs[1]; !reflect.DeepEqual(q, want) {
			t.Fatalf("the request to the removed peer:\n got %+v\nwant %+v", q, want)
		}
		if n := len(offerOf(t, b, room)); n != before+MaxLogEntries {
			t.Fatalf("b holds %d log entries, want %d: the intervening page was taken", n, before+MaxLogEntries)
		}
		if st, _ := b.r.State(room); slices.Contains(st.Members, a.id) {
			t.Fatal("a is a member at b through a page it sent as a non-member")
		}
		for range 4 { // the rest of the log from z, a page at a time
			if _, err := b.x.Pull(ctx, room, z.id, PullOpts{}); err != nil {
				t.Fatal(err)
			}
		}
		if n := len(offerOf(t, b, room)); n != len(whole) {
			t.Fatalf("b holds %d log entries after pulling from z, want %d", n, len(whole))
		}
		if _, err := b.x.Pull(ctx, room, a.id, PullOpts{Offer: true}); err != nil {
			t.Fatal(err)
		}
		if !b.holds(room, fromA) || !a.holds(room, mine) {
			t.Fatal("after the re-admission the records did not cross")
		}
	})
}

// A request waiting for a slot of the peer's budget when the peer is
// removed here is never sent: membership is checked when it is prepared,
// after the slot and the token, and the token goes back.
//
// Requirements: T5, N1
func TestARequestQueuedWhenItsPeerIsRemovedIsNeverSent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		net := newRecNet()
		z, a, b := newSNode(t, "z", net), newSNode(t, "a", net), newSNode(t, "b", net)
		room := sgroup(t, z, a, b)
		b.send(room, "an offer for a")
		var held []*pullTicket
		for range PullBudgetSlots {
			tk, err := b.x.budget.admit(ctx, a.id, nil)
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, tk)
		}
		done := make(chan error, 1)
		go func() {
			_, err := b.x.Pull(ctx, room, a.id, PullOpts{Offer: true})
			done <- err
		}()
		synctest.Wait()
		change(t, z, room, Change{Kind: core.EntryRemove, Target: a.id})
		if _, err := b.r.Receive(ctx, room, 0, offerOf(t, z, room)...); err != nil {
			t.Fatal(err)
		}
		for _, tk := range held {
			tk.done()
		}
		if err := <-done; !errors.Is(err, ErrPeerNotMember) {
			t.Fatalf("pull: %v, want ErrPeerNotMember", err)
		}
		if n, _ := requests(net.sent(), b.id, a.id, time.Time{}, time.Now().Add(time.Hour)); n != 0 {
			t.Fatalf("%d requests sent to the removed peer", n)
		}
		if got := b.x.budget.takenBy(a.id); got != int64(PullBudgetSlots) {
			t.Fatalf("%d tokens kept, want %d (the queued request's went back)", got, PullBudgetSlots)
		}
	})
}
