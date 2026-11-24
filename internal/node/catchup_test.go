package node

// Step 5b fix round, items 3 and 4: a room counts as caught up only after
// an exchange in which every log head the peer advertised is accepted
// here, or a completed join; Confirm refuses to sign before that unless
// the person explicitly accepts the risk.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// restoredAdmin is a (admin, restored to before it added c), b (holding
// the add of c) and a pending request on a.
func restoredAdmin(t *testing.T) (a, b *pnode, net *memNet, room string, q store.Request) {
	t.Helper()
	clock := lclock()
	net = newMemNet()
	a = newPNode(t, net, "a", newLNode(t, "a", clock))
	b = newPNode(t, net, "b", newLNode(t, "b", clock))
	c := newPNode(t, net, "c", newLNode(t, "c", clock))
	room = a.create("team")
	a.change(room, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee", History: true})
	b.pin(room, a.lnode)
	b.pull(room, a.lnode)
	old := snapshot(t, a)
	a.mustPull(room, b, PullOpts{})
	a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "cee", History: true})
	b.mustPull(room, a, PullOpts{})
	restoreFrom(t, a, old)
	q = a.propose(room, Change{Kind: core.EntryAdd, Target: junkNode(1), Name: "j", History: true})
	return a, b, net, room, q
}

func notCaughtUp(err error) bool { return errors.Is(err, ErrNotCaughtUp) }

// [item 3] A peer that advertises newer log heads but hands over no
// entries ends the exchange without error, and unlocks nothing; the real
// catch-up does.
func TestAnExchangeWhoseAdvertisedHeadsAreNotAcceptedDoesNotUnlockSigning(t *testing.T) {
	a, b, net, room, q := restoredAdmin(t)
	net.tamper = func(_, _ string, resp *PullResponse) { resp.Log.Entries, resp.Log.More = nil, false }
	if _, err := a.pullFrom(room, b, PullOpts{}); err != nil {
		t.Fatalf("the withheld exchange: %v", err)
	}
	signs := a.r.signs
	if _, err := a.r.Confirm(context.Background(), q.ID, true); !notCaughtUp(err) || a.r.signs != signs {
		t.Fatalf("after an exchange that withheld the log: %v, signed %d", err, a.r.signs-signs)
	}
	net.tamper = nil
	a.mustPull(room, b, PullOpts{})
	if cf := a.confirm(q.ID); cf.State != store.RequestApproved {
		t.Fatalf("after catching up: %+v", cf)
	}
}

// [item 3] Entries handed over through Receive never unlock signing.
func TestReceiveAloneNeverUnlocksSigning(t *testing.T) {
	a, b, _, room, q := restoredAdmin(t)
	ws, err := b.r.Offer(room)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.r.Receive(context.Background(), room, 0, ws...); err != nil {
		t.Fatal(err)
	}
	if _, err := a.r.Confirm(context.Background(), q.ID, true); !notCaughtUp(err) {
		t.Fatalf("after Receive: %v", err)
	}
}

// [item 3] A join unlocks signing only once it completes with every head
// of its last page accepted.
func TestAJoinUnlocksSigningOnlyWhenItCompletes(t *testing.T) {
	clock := lclock()
	jn := newJoinNet()
	a := newLNode(t, "a", clock)
	b := newLNode(t, "b", clock)
	other := newLNode(t, "o", clock)
	jn.serve(a)
	room := a.create("team")
	a.change(room, Change{Kind: core.EntryAdd, Target: other.id, Name: "oh", History: true})
	j := b.joiner(jn)
	tok := a.invite(room, RoleAdmin, true)
	b.preview(j, tok)
	b.accept(j, tok, 0)
	for _, q := range a.requests(room) {
		a.confirm(q.ID)
	}
	if rl := b.r.rooms[room]; rl != nil && rl.pulled {
		t.Fatal("caught up before the join completed")
	}
	os, err := j.Resume(context.Background(), time.Minute)
	if err != nil || len(os) != 1 || os[0].State != JoinStateSigned {
		t.Fatalf("resume: %+v %v", os, err)
	}
	if !b.r.rooms[room].pulled {
		t.Fatal("a completed join did not count as caught up")
	}
}

// [item 4] An admin that cannot catch up (every other member gone) can
// still sign, but only with the explicit override, which is recorded with
// the request; a rejoin approval, which signs nothing, is not gated.
func TestConfirmBeforeCatchingUpNeedsTheExplicitOverrideAndRecordsIt(t *testing.T) {
	a, _, _, room, q := restoredAdmin(t)
	ctx := context.Background()
	if _, err := a.r.Confirm(ctx, q.ID, true); !notCaughtUp(err) {
		t.Fatalf("without the override: %v", err)
	}
	if got, _ := a.st.GetRequest(ctx, q.ID); got.State != store.RequestPending {
		t.Fatalf("the refused request is %s, want pending", got.State)
	}
	cf, err := a.r.ConfirmWith(ctx, q.ID, true, ConfirmOpts{NotCaughtUpOK: true})
	if err != nil || cf.State != store.RequestApproved {
		t.Fatalf("with the override: %+v %v", cf, err)
	}
	got, err := a.st.GetRequest(ctx, q.ID)
	if err != nil || !got.NotCaughtUp {
		t.Fatalf("the override was not recorded: %+v %v", got, err)
	}
	_ = room
}

// [item 4] Approving a rejoin signs nothing and is not gated.
func TestARejoinApprovalIsNotGatedOnCatchingUp(t *testing.T) {
	clock := lclock()
	jn := newJoinNet()
	a := newLNode(t, "a", clock)
	b := newLNode(t, "b", clock)
	other := newLNode(t, "o", clock)
	jn.serve(a)
	room := a.create("team")
	a.change(room, Change{Kind: core.EntryAdd, Target: other.id, Name: "oh", History: true})
	a.change(room, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee", History: true})
	a.restart() // not caught up now
	j := b.joiner(jn)
	tok := a.invite(room, RoleMember, true)
	b.preview(j, tok)
	b.accept(j, tok, 0)
	qs := a.requests(room)
	if len(qs) != 1 || qs[0].Action != ActionRejoin {
		t.Fatalf("requests: %+v", qs)
	}
	if cf, err := a.r.Confirm(context.Background(), qs[0].ID, true); err != nil || cf.State != store.RequestApproved {
		t.Fatalf("rejoin approval: %+v %v", cf, err)
	}
}
