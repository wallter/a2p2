package node

import (
	"context"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

// One waiting pull per room per peer: a second from the same peer for the
// same room releases the first at once (same, no error).
//
// Requirements: P5
func TestASecondWaitingPullFromOnePeerReleasesTheFirst(t *testing.T) {
	a, b, _, room := pairWithHistory(t)
	first := make(chan error, 1)
	go func() {
		_, err := b.x.Pull(context.Background(), room, a.id, PullOpts{WaitMS: 60000})
		first <- err
	}()
	time.Sleep(30 * time.Millisecond)
	select {
	case err := <-first:
		t.Fatalf("returned before the second: %v", err)
	default:
	}
	ctx, cancel := context.WithCancel(context.Background())
	second := make(chan struct{})
	defer func() { cancel(); <-second }()
	go func() {
		defer close(second)
		_, _ = b.x.Pull(ctx, room, a.id, PullOpts{WaitMS: 60000})
	}()
	select {
	case err := <-first:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
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
