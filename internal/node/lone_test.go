package node

// A lone message, sent with the requester's budget full and the reader's
// machine already waiting: what it costs in requests, and that no wait of
// the budget, a timer or a back-off is on its path, only the legs of the
// requests that carry it. In a synctest bubble, as the other scheduler
// tests; each leg of a request takes oneWay.

import (
	"fmt"
	"testing"
	"testing/synctest"
	"time"
)

// loneLeg is each leg's delay in these tests: long enough that a token
// wait (tokenGap) or any timer would show, short against a wait.
const loneLeg = 5 * time.Millisecond

// lone sends one message from w and returns how long it took to be held at
// r, and the requests each machine sent the other from the write until
// everything was quiet again (10 s).
func lone(t *testing.T, net *recNet, w, r *snode, room string, listeners ...*snode) (took time.Duration, wr, rw []sent) {
	t.Helper()
	type hold struct {
		server, peer *snode
		initial      chan struct{}
	}
	var holds []hold
	for _, server := range listeners {
		peer := r
		if server == r {
			peer = w
		}
		initial := loneHold(server, peer, room)
		if initial == nil {
			t.Fatalf("before send, %s is not holding a pull from %s", server.name, peer.name)
		}
		holds = append(holds, hold{server, peer, initial})
	}
	at := net.clock.Now()
	id := w.send(room, "lone")
	for !r.holds(room, id) {
		if net.clock.Now().Sub(at) > time.Second {
			t.Fatalf("the message is not at %s 1 s after it was sent", r.name)
		}
		net.run(t, time.Millisecond)
	}
	took = net.clock.Now().Sub(at)
	net.run(t, 10*time.Second)
	for _, h := range holds {
		if final := loneHold(h.server, h.peer, room); final == nil || final == h.initial {
			t.Fatalf("after delivery, %s is not holding a new pull from %s", h.server.name, h.peer.name)
		}
	}
	for _, s := range net.sent() {
		if s.at.Before(at) || s.at.After(at.Add(10*time.Second)) {
			continue
		}
		switch {
		case s.from == w.id && s.to == r.id:
			wr = append(wr, s)
		case s.from == r.id && s.to == w.id:
			rw = append(rw, s)
		}
	}
	return took, wr, rw
}

func loneHold(server, peer *snode, room string) chan struct{} {
	server.x.mu.Lock()
	defer server.x.mu.Unlock()
	return server.x.waiting[[2]string{room, peer.id}]
}

func describe(ss []sent) string {
	out := ""
	for _, s := range ss {
		out += fmt.Sprintf(" [wait %d ms, offer %d]", s.wait, s.offer)
	}
	return out
}

// A lone message between a listening machine a and an outbound-only
// machine b, budget full and the other side waiting, costs no token wait
// and no timer: it is held at the reader after exactly the legs of the
// requests that carry it. Pulled by b from a, it costs 4 requests: the
// waiting pull a's write ends (answered with digests), the continuation
// that brings the page, the closing opening answered same, and the next
// waiting pull; 3 of them sent after the write. Offered by b to a, it
// costs 3 requests after the write: the offer (b's waiting pull is
// abandoned for it), the closing opening, and the next waiting pull.
// Between two listening machines the writer offers it, as b does, and
// the reader's waiting pull the write ends is followed by one or two
// requests (the second when the reader, woken by storing the offer,
// offers the message back to its writer, from an observation older than
// the offer) and the next
// waiting pull: 6 or 7 requests, 5 or 6 of them sent after the write. A
// change that cuts these counts updates them here; one that raises
// them, or puts a wait on the path, fails.
//
// Requirements: N1
func TestALoneMessageCostsAFixedNumberOfRequestsAndNoTokenWait(t *testing.T) {
	t.Run("pulled by the outbound-only machine", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			net, a, b, room := outboundPair(t, loneLeg)
			for i := range 3 {
				net.run(t, 5*time.Second) // the budget full again
				took, ab, ba := lone(t, net, a, b, room, a)
				t.Logf("message %d: %v; b to a:%s", i+1, took, describe(ba))
				// the waiting pull's answer, then the continuation's two legs
				if took != 3*loneLeg {
					t.Fatalf("message %d held at b %v after it was sent, want %v (three legs)", i+1, took, 3*loneLeg)
				}
				if len(ab) != 0 || len(ba) != 3 || ba[0].wait != 0 || ba[1].wait != 0 || ba[2].wait == 0 {
					t.Fatalf("message %d: a to b %d requests, b to a%s; want none, and a continuation, a closing opening and a waiting pull",
						i+1, len(ab), describe(ba))
				}
			}
		})
	})
	t.Run("between two listening machines", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			net := newRecNet()
			net.oneWay = loneLeg
			a, b := newSNode(t, "a", net), newSNode(t, "b", net)
			room := sgroup(t, a, b)
			schedule(t, net, a, b)
			net.run(t, time.Minute)
			for i := range 3 {
				net.run(t, 5*time.Second)
				took, ab, ba := lone(t, net, a, b, room, a, b)
				t.Logf("message %d: %v; a to b:%s; b to a:%s", i+1, took, describe(ab), describe(ba))
				if took != loneLeg {
					t.Fatalf("message %d held at b %v after it was sent, want %v (the offer's first leg)", i+1, took, loneLeg)
				}
				if len(ab) != 3 || ab[0].offer != 1 || ab[1].wait != 0 || ab[1].offer != 0 || ab[2].wait == 0 ||
					len(ba) < 2 || len(ba) > 3 || ba[len(ba)-1].wait == 0 {
					t.Fatalf("message %d: a to b%s, b to a%s; want an offer, a closing opening and a waiting pull, "+
						"and at most two requests and a waiting pull", i+1, describe(ab), describe(ba))
				}
			}
		})
	})
	t.Run("offered by the outbound-only machine", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			net, a, b, room := outboundPair(t, loneLeg)
			for i := range 3 {
				net.run(t, 5*time.Second)
				took, ba, ab := lone(t, net, b, a, room, a)
				t.Logf("message %d: %v; b to a:%s", i+1, took, describe(ba))
				// the offer's first leg
				if took != loneLeg {
					t.Fatalf("message %d held at a %v after it was sent, want %v (one leg)", i+1, took, loneLeg)
				}
				if len(ab) != 0 || len(ba) != 3 || ba[0].offer != 1 || ba[1].wait != 0 || ba[1].offer != 0 || ba[2].wait == 0 {
					t.Fatalf("message %d: a to b %d requests, b to a%s; want none, and an offer, a closing opening and a waiting pull",
						i+1, len(ab), describe(ba))
				}
			}
		})
	})
}
