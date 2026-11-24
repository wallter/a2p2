package node

import (
	"context"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

// One waiting pull per room per peer: a second from the same peer for the
// same room releases the first at once (same, no error). In an explicit
// order: the first is held, then the second is, and only then is the
// first released.
//
// Requirements: P5
func TestASecondWaitingPullFromOnePeerReleasesTheFirst(t *testing.T) {
	a, b, _, room := pairWithHistory(t)
	b.mustPull(room, a, PullOpts{})
	a.x.after = func(time.Duration) <-chan time.Time { return nil } // no wait ends on its own
	first := heldPull(t, a, b, room)
	select {
	case err := <-first:
		t.Fatalf("the first returned on its own: %v", err)
	default:
	}
	second := a.x.NextHold(room, b.id)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	defer func() { cancel(); <-stopped }()
	go func() {
		defer close(stopped)
		_, _ = b.x.Pull(ctx, room, a.id, PullOpts{WaitMS: 60000})
	}()
	<-second
	select {
	case err := <-first:
		if err != nil {
			t.Fatalf("the first, released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the first waiting pull was not released")
	}
}

// A write in one room does not wake the waiting pulls of another; a write
// in the room does, at once.
//
// Requirements: P5
func TestAWriteInOneRoomWakesOnlyThatRoomsWaitingPulls(t *testing.T) {
	var w roomWakes
	r1, r2 := w.wait("r1"), w.wait("r2")
	w.touch("r2")
	select {
	case <-r1:
		t.Fatal("r1 woken by a write in r2")
	default:
	}
	select {
	case <-r2:
	default:
		t.Fatal("r2 not woken")
	}
	clock := lclock()
	net := newMemNet()
	a := newPNode(t, net, "a", newLNode(t, "a", clock))
	b := newPNode(t, net, "b", newLNode(t, "b", clock))
	one, two := a.create("one"), a.create("two")
	for _, room := range []string{one, two} {
		a.change(room, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee", History: true})
		b.pin(room, a.lnode)
		b.pull(room, a.lnode)
		b.mustPull(room, a, PullOpts{})
	}
	a.register("alice")
	wake := a.m.wakes.wait(one)
	a.send(two, "alice", "elsewhere")
	select {
	case <-wake:
		t.Fatal("a write in room two woke room one")
	default:
	}
	a.send(one, "alice", "here")
	select {
	case <-wake:
	default:
		t.Fatal("a write in room one did not wake it")
	}
}
