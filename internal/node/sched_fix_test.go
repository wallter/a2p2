package node

// Step 5b fix round, items 1, 2, 5 and 6: the scheduler's loops, wakes,
// refusals and pacing. In a synctest bubble, as the other scheduler tests.

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// scheduleWith is schedule with hooks set on each scheduler before it
// runs.
func scheduleWith(t *testing.T, set func(*Scheduler), ns ...*snode) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, n := range ns {
		s := NewScheduler(n.x, SchedOpts{Jitter: func() float64 { return 0.5 }})
		set(s)
		wg.Add(1)
		go func() { defer wg.Done(); s.Run(ctx) }()
	}
	t.Cleanup(func() { cancel(); wg.Wait() })
}

// rejoin pins room on n again from what from holds (as a join would).
func (n *snode) rejoin(room string, from *snode) {
	n.t.Helper()
	ctx := context.Background()
	ws, err := from.r.Offer(room)
	if err != nil {
		n.t.Fatal(err)
	}
	if _, err := n.r.PinRoom(ctx, ws[0]); err != nil {
		n.t.Fatal(err)
	}
	if _, err := n.r.Receive(ctx, room, 0, ws...); err != nil {
		n.t.Fatal(err)
	}
}

// [item 1] A room left and joined again before the scheduler looks at the
// rooms held: the dead loop of the old generation is replaced, and the
// room is pulled again.
func TestARoomLeftAndJoinedAgainBeforeTheSchedulerLooksIsPulledAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net := newRecNet()
		a, b := newSNode(t, "a", net), newSNode(t, "b", net)
		room := sgroup(t, a, b)
		rejoined := make(chan struct{})
		looks := 0
		scheduleWith(t, func(s *Scheduler) {
			s.beforeLook = func() {
				if looks++; looks == 2 {
					<-rejoined // the look after the leave waits for the rejoin
				}
			}
			var once sync.Once
			s.loopDone = func(r string) {
				once.Do(func() {
					a.rejoin(room, b) // the old loop ended: join again before Run looks
					close(rejoined)
				})
			}
		}, a)
		net.settle(t)
		if err := a.r.Leave(context.Background(), room); err != nil {
			t.Fatal(err)
		}
		net.settle(t)
		n := len(from(net.sent(), a.id))
		id := b.send(room, "after the rejoin")
		net.run(t, 2*time.Minute)
		if !a.holds(room, id) || len(from(net.sent(), a.id)) == n {
			t.Fatalf("not pulled after the rejoin: holds %v, requests %d", a.holds(room, id), len(from(net.sent(), a.id))-n)
		}
	})
}

// [item 2] A local write between an anchor's look and its waiting pull is
// offered at once, not after the wait.
func TestAWriteBetweenTheLookAndTheWaitIsOfferedAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net := newRecNet()
		a, b := newSNode(t, "a", net), newSNode(t, "b", net)
		room := sgroup(t, a, b)
		var id string
		var mu sync.Mutex
		armed := false
		scheduleWith(t, func(s *Scheduler) {
			s.beforePull = func(r string, slot int) {
				mu.Lock()
				defer mu.Unlock()
				if armed {
					armed = false
					id = a.send(room, "in the gap")
				}
			}
		}, a)
		net.settle(t)
		mu.Lock()
		armed = true
		mu.Unlock()
		net.run(t, SchedWait+time.Second) // the waiting pull ends; the next one is picked
		at := time.Now()
		mu.Lock()
		defer mu.Unlock()
		if id == "" {
			t.Fatal("no pick after the wait")
		}
		net.settle(t)
		if !b.holds(room, id) {
			t.Fatal("the write in the gap was not offered")
		}
		if d := time.Since(at); d != 0 {
			t.Fatalf("offered after %v", d)
		}
	})
}

// [item 5] A peer muted after two refusals is tried again as soon as it
// sends a request, as a current member, with changed log heads; the
// room's loops are woken.
func TestAMutedPeerThatCatchesUpIsTriedAgainWhenItsRequestShowsNewHeads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net := newRecNet()
		a, b := newSNode(t, "a", net), newSNode(t, "b", net)
		room := sgroup(t, a, b)
		ctx := context.Background()
		s := NewScheduler(a.x, SchedOpts{Jitter: func() float64 { return 0.5 }})
		has := func() bool { return slices.Contains(s.eligible(room), b.id) }
		gen, err := a.r.Gen(room)
		if err != nil {
			t.Fatal(err)
		}
		_, heads := s.roomNow(room)
		q := &PullRequest{Room: room, Log: PullLog{Heads: heads}}
		for range 2 {
			if err := a.x.answered(ctx, room, b.id, gen, q, notMember()); err != nil {
				t.Fatal(err)
			}
			s.failed(room, b.id, notMember())
		}
		if has() {
			t.Fatal("not muted after two refusals")
		}
		wake := a.m.wakes.wait(room)
		if _, err := b.x.Pull(ctx, room, a.id, PullOpts{}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-wake:
		default:
			t.Fatal("the room's loops were not woken")
		}
		if !has() {
			t.Fatal("still muted after its request showed new heads")
		}
	})
}

// [item 6] A peer that answers every waiting pull at once with same, and
// stores nothing either way, is backed off as a failing one: 1 s doubling
// to 5 min.
func TestAPeerThatAnswersEveryWaitAtOnceIsBackedOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net := newRecNet()
		net.limit = 200
		a, b := newSNode(t, "a", net), newSNode(t, "b", net)
		sgroup(t, a, b)
		b.x.after = func(time.Duration) <-chan time.Time { // b's waits end at once
			c := make(chan time.Time, 1)
			c <- time.Now()
			return c
		}
		schedule(t, net, a)
		net.run(t, 10*time.Minute)
		got := len(from(net.sent(), a.id))
		t.Logf("requests in 10 minutes to a peer that answers at once: %d", got)
		// 2 start pulls, then waits at 0, 1, 3, 7, 15, 31, 63, 127, 255,
		// 511 s.
		if got > 13 {
			t.Fatalf("%d requests in 10 minutes", got)
		}
	})
}
