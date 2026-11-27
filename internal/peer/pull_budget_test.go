package peer

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/node"
)

// refusals counts the 429s a machine's pull client gets, in all (n) and
// those for requests in flight (concurrent; the rest are for the rate).
type refusals struct {
	node.PullTransport
	n, concurrent atomic.Int64
}

func (c *refusals) Pull(ctx context.Context, peer string, q *node.PullRequest) (*node.PullResponse, error) {
	resp, err := c.PullTransport.Pull(ctx, peer, q)
	var pe *node.PullError
	if errors.As(err, &pe) && pe.Status == http.StatusTooManyRequests {
		c.n.Add(1)
		if pe.Detail == "concurrent" {
			c.concurrent.Add(1)
		}
	}
	return resp, err
}

// counted gives p an exchange whose client counts the 429s it gets.
func counted(p *pm) *refusals {
	c := &refusals{PullTransport: p.cl}
	var addrs []string
	if p.addr != "" {
		addrs = []string{p.addr}
	}
	p.x = node.NewExchange(p.r, c, node.ExchangeOpts{Book: p.book, Addrs: addrs})
	if p.h != nil {
		p.h.x.Store(p.x)
	}
	return c
}

// Two listening machines over TLS on loopback, each writing every 50 ms
// for a minute of real time, through the listener's real limiter (20 a
// second per peer, burst 40): the requester's budget (15 a second, burst
// 30, a 25 % margin) keeps every request under it, so neither gets a single 429, and
// every message arrives.
//
// Requirements: D2, P5
func TestOverTLSAnHonestPairWritingEvery50MsForAMinuteDrawsNo429(t *testing.T) {
	a, b := newPM(t, "alice", true), newPM(t, "bob", true)
	ca, cb := counted(a), counted(b)
	room := group(t, a, b)
	a.knows(b)
	b.knows(a)
	runSched(t, a, b)
	var toA, toB []string
	end := time.Now().Add(time.Minute)
	for tick := time.NewTicker(50 * time.Millisecond); time.Now().Before(end); <-tick.C {
		toB = append(toB, a.send(room, "from alice", false))
		toA = append(toA, b.send(room, "from bob", false))
	}
	within(t, "every message at the other machine", func() bool {
		for len(toB) > 0 && b.holds(room, toB[0]) {
			toB = toB[1:]
		}
		for len(toA) > 0 && a.holds(room, toA[0]) {
			toA = toA[1:]
		}
		return len(toA)+len(toB) == 0
	})
	t.Logf("requests served: by alice %d, by bob %d; 429s: to alice %d, to bob %d",
		a.h.pulls.Load(), b.h.pulls.Load(), ca.n.Load(), cb.n.Load())
	if n := ca.n.Load() + cb.n.Load(); n > 0 {
		t.Fatalf("%d requests refused 429", n)
	}
}

// Twelve rooms shared by an outbound-only machine and the listening one it
// pulls from, all scheduled at once, the listener writing in every room at
// the same moment every 500 ms for 15 s (each write releases the twelve
// waiting pulls together, and they are asked again together) and the
// outbound one every 300 ms: over TLS through the real listener, whose
// limits are 20 a second and 4 requests in flight per peer, counting a
// waiting pull until it is held. A waiting pull keeps one of the
// requester's 3 slots until it is answered or a second has passed, so no
// request is refused 429, for the rate or for requests in flight, and
// every message arrives.
//
// Requirements: D2, P5, N1
func TestOverTLSManyRoomsWithOnePeerStartingAndWritingTogetherDrawNo429(t *testing.T) {
	a, b := newPM(t, "alice", true), newPM(t, "bob", false)
	cb := counted(b)
	var rooms []string
	for range 12 {
		rooms = append(rooms, group(t, a, b))
	}
	b.knows(a)
	runSched(t, a, b)
	var toA, toB []string
	start := time.Now()
	end := start.Add(15 * time.Second)
	lastA := start
	for tick := time.NewTicker(300 * time.Millisecond); time.Now().Before(end); <-tick.C {
		toA = append(toA, b.send(rooms[len(toA)%len(rooms)], "from bob", false))
		if time.Since(lastA) >= 500*time.Millisecond {
			lastA = time.Now()
			for _, r := range rooms {
				toB = append(toB, a.send(r, "from alice", false))
			}
		}
	}
	at := func(p *pm, ids []string) []string {
		var rest []string
		for i, id := range ids {
			found := false
			for _, r := range rooms {
				if p.holds(r, id) {
					found = true
					break
				}
			}
			if !found {
				rest = append(rest, ids[i])
			}
		}
		return rest
	}
	within(t, "every message at the other machine", func() bool {
		toA, toB = at(a, toA), at(b, toB)
		return len(toA)+len(toB) == 0
	})
	t.Logf("requests served by alice %d in %v; 429s to bob: %d (%d for requests in flight)",
		a.h.pulls.Load(), time.Since(start).Round(time.Second), cb.n.Load(), cb.concurrent.Load())
	if n := cb.n.Load(); n > 0 {
		t.Fatalf("%d requests refused 429 (%d for requests in flight)", n, cb.concurrent.Load())
	}
}

// A request that tries a dead address first and then the live one pays a
// token for each attempt: what the requester was charged is every request
// the listener got, and the one attempt at the dead address.
//
// Requirements: N1
func TestOverTLSEachAddressAPullTriesIsChargedToThePeersBudget(t *testing.T) {
	a, b := newPM(t, "alice", true), newPM(t, "bob", false)
	room := group(t, a, b)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()
	forget(b.book, a.node())
	b.book.Replace(a.node(), []string{dead}, time.Now())
	b.book.Learn(a.node(), []string{a.addr}, time.Now())
	a.send(room, "hello", false)
	before, charged := a.h.pulls.Load(), b.x.RequestsCharged(a.node())
	b.mustPull(room, a, node.PullOpts{})
	served := a.h.pulls.Load() - before
	paid := b.x.RequestsCharged(a.node()) - charged
	t.Logf("served %d, charged %d", served, paid)
	if served == 0 || paid != served+1 {
		t.Fatalf("charged %d for %d requests served and one dead address", paid, served)
	}
}
