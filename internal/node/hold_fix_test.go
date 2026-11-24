package node

// Step 5b fix round, items 8, 9 and 12: held answers are built again at
// release, a shutdown releases them at once, membership comes before a
// 503, and tests wait on the registration of a hold, not on a sleep.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/store"
)

// heldPull starts b's waiting pull of room from a and returns once a holds
// it, with a channel that gets its result.
func heldPull(t *testing.T, a, b *pnode, room string) <-chan error {
	t.Helper()
	registered := a.x.NextHold(room, b.id)
	done := make(chan error, 1)
	go func() {
		_, err := b.x.Pull(context.Background(), room, a.id, PullOpts{WaitMS: 60000})
		done <- err
	}()
	select {
	case <-registered:
	case err := <-done:
		t.Fatalf("answered without a hold: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("never held")
	}
	return done
}

// [item 8] Text that passes its retention time while an answer is held,
// with no write meanwhile, is not answered as it was when the hold began:
// the answer released is built again, as a fresh request would be.
func TestAHeldAnswerIsBuiltAgainAtReleaseEvenWithNoWrite(t *testing.T) {
	a, b, net, room := pairWithHistory(t)
	a.send(room, "alice", "soon gone")
	b.mustPull(room, a, PullOpts{})
	timeout := make(chan time.Time)
	a.x.after = func(time.Duration) <-chan time.Time { return timeout }
	var released *PullResponse
	net.tamper = func(from, to string, resp *PullResponse) {
		if from == b.id && released == nil {
			released = resp
		}
	}
	done := heldPull(t, a, b, room)
	*a.clock = a.clock.Add(store.Retention + time.Minute) // a's copy of the text expires; nothing is written
	close(timeout)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	fresh := *opening(t, net)
	fresh.WaitMS = 0
	want, err := a.x.Serve(context.Background(), b.id, &fresh)
	if err != nil {
		t.Fatal(err)
	}
	if released == nil || released.Same != want.Same || released.Sum != want.Sum {
		t.Fatalf("released %+v, a fresh answer %+v", released, want)
	}
}

// [item 8] Close releases every held request at once (503) and holds no
// more: what the daemon calls as it begins to shut down.
func TestClosingTheExchangeReleasesEveryHeldPullAtOnce(t *testing.T) {
	a, b, _, room := pairWithHistory(t)
	b.mustPull(room, a, PullOpts{})
	a.x.after = func(time.Duration) <-chan time.Time { return nil } // a wait never times out
	done := heldPull(t, a, b, room)
	a.x.Close()
	select {
	case err := <-done:
		var pe *PullError
		if !errors.As(err, &pe) || pe.Status != 503 {
			t.Fatalf("released with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("still held after Close")
	}
	_, err := b.x.Pull(context.Background(), room, a.id, PullOpts{WaitMS: 1000})
	var pe *PullError
	if !errors.As(err, &pe) || pe.Status != 503 {
		t.Fatalf("a request after Close: %v", err)
	}
}

// [item 9] A held request for a room this machine left and joined again,
// in whose new membership the peer is not, is refused as not a member,
// not as unavailable.
func TestAHeldPullForARoomJoinedAgainWithoutThePeerIsRefusedAsNotAMember(t *testing.T) {
	a, b, net, room := pairWithHistory(t)
	b.mustPull(room, a, PullOpts{})
	ws, err := a.r.Offer(room)
	if err != nil {
		t.Fatal(err)
	}
	q := opening(t, net)
	built, err := a.x.serve(context.Background(), b.id, q)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.r.Leave(context.Background(), room); err != nil {
		t.Fatal(err)
	}
	if _, err := a.r.PinRoom(context.Background(), ws[0]); err != nil { // only its creation: b is no member
		t.Fatal(err)
	}
	_, err = a.x.release(context.Background(), b.id, q, built)
	if !isNotMember(err) {
		t.Fatalf("released with %v, want not_member", err)
	}
}

// opening is the last opening request the net carried.
func opening(t *testing.T, net *memNet) *PullRequest {
	t.Helper()
	net.mu.Lock()
	defer net.mu.Unlock()
	for i := len(net.heard) - 1; i >= 0; i-- {
		if net.heard[i].Sum != "" {
			return net.heard[i]
		}
	}
	t.Fatal("no opening request")
	return nil
}
