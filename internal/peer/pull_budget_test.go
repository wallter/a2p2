package peer

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/node"
)

// refusals counts the 429s a machine's pull client gets.
type refusals struct {
	node.PullTransport
	n atomic.Int64
}

func (c *refusals) Pull(ctx context.Context, peer string, q *node.PullRequest) (*node.PullResponse, error) {
	resp, err := c.PullTransport.Pull(ctx, peer, q)
	var pe *node.PullError
	if errors.As(err, &pe) && pe.Status == http.StatusTooManyRequests {
		c.n.Add(1)
	}
	return resp, err
}

// counted gives p an exchange whose client counts the 429s it gets.
func counted(p *pm) *refusals {
	c := &refusals{PullTransport: p.cl}
	p.x = node.NewExchange(p.r, c, node.ExchangeOpts{Book: p.book, Addrs: []string{p.addr}})
	p.h.x.Store(p.x)
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
