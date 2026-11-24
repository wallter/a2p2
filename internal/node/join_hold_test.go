package node

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

// One join at a time per invite, and a caller for another invite never
// waits behind it; a caller waiting for a busy invite stops when its
// context ends, and resuming skips the busy invite.
func TestJoinsAreSerialisedPerInviteAndAWaitingCallerIsCancellable(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	jn := newJoinNet()
	jn.serve(a)
	r1, r2 := a.create("one"), a.create("two")
	t1, t2 := a.invite(r1, "", false), a.invite(r2, "", false)
	j := b.joiner(jn)
	b.preview(j, t1)
	b.preview(j, t2)
	tok, err := core.DecodeToken(t1)
	if err != nil {
		t.Fatal(err)
	}
	// A long accept of invite 1 is in progress: it holds that invite.
	release, ok, err := j.hold(context.Background(), secretHash(tok.Secret), false)
	if err != nil || !ok {
		t.Fatal(err)
	}
	done := make(chan JoinOutcome, 1)
	go func() { done <- b.accept(j, t2, 0) }()
	select {
	case o := <-done:
		if o.State != JoinStateAccepted {
			t.Fatalf("other invite: %+v", o)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an accept of another invite waited behind a busy one")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := j.JoinAccept(ctx, t1, previewOf(j, t1), 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting for a busy invite: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("cancelled caller returned after %v", d)
	}
	if _, err := j.Resume(context.Background(), 0); err != nil {
		t.Fatalf("resume beside a busy invite: %v", err)
	}
	release()
	if o := b.accept(j, t1, 0); o.State != JoinStateAccepted {
		t.Fatalf("after release: %+v", o)
	}
}
