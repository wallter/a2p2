package node

// Exchanges excused from counting as spins (sched.go, excused): a machine
// that only connects out delivers its own changes as offers in its own
// pulls, which return at once, and must not be backed off for it; a peer
// that never holds a wait must still be. In a synctest bubble, as the
// other scheduler tests; recNet.delay gives requests a round trip.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// outboundPair is a listening machine a and an outbound-only machine b
// sharing a room, both scheduled, requests taking rtt; a cannot reach b.
func outboundPair(t *testing.T, rtt time.Duration) (net *recNet, a, b *snode, room string) {
	t.Helper()
	net = newRecNet()
	net.delay = rtt
	a, b = newSNode(t, "a", net), newSNode(t, "b", net)
	room = sgroup(t, a, b)
	net.memNet.mu.Lock()
	net.noInbound[b.id] = true
	net.memNet.mu.Unlock()
	schedule(t, net, a, b)
	net.run(t, time.Minute) // start pulls done; a has backed off b
	return net, a, b, room
}

// sendEvery has b send n messages, every gap of fake time, and fails
// unless each is held at a within bound of its send.
func sendEvery(t *testing.T, net *recNet, a, b *snode, room string, n int, gap, bound time.Duration) {
	t.Helper()
	for i := range n {
		at := net.clock.Now()
		id := b.send(room, "message")
		awaitAt(t, net, a, room, id, at, bound, fmt.Sprintf("message %d of %d (every %v)", i+1, n, gap))
		if rest := gap - net.clock.Now().Sub(at); rest > 0 {
			net.run(t, rest)
		}
	}
}

// awaitAt fails unless a holds record id within bound of at.
func awaitAt(t *testing.T, net *recNet, a *snode, room, id string, at time.Time, bound time.Duration, what string) {
	t.Helper()
	for !a.holds(room, id) {
		if took := net.clock.Now().Sub(at); took > bound {
			t.Fatalf("%s not at %s %v after it was sent", what, a.name, took)
		}
		net.run(t, 10*time.Millisecond)
	}
}

// requests counts the requests from one machine to another sent in
// [from, to), continuations included, and those with an offer.
func requests(log []sent, src, dst string, from, to time.Time) (n, offers int) {
	for _, s := range log {
		if s.from == src && s.to == dst && !s.at.Before(from) && s.at.Before(to) {
			n++
			if s.offer > 0 {
				offers++
			}
		}
	}
	return n, offers
}

// Every message of an outbound-only machine reaches the machine it pulls
// from at once: in a burst 5 ms apart, at human pace (one every 2 s), past
// a whole wait, and one after a quiet hour; with no round trip and with
// 100 ms requests.
//
// Requirements: D2, N1
func TestAnOutboundOnlyMachinesMessagesReachTheListenerAtOnceAtAnyPace(t *testing.T) {
	for _, c := range []struct {
		name       string
		rtt        time.Duration
		n          int
		gap, bound time.Duration
	}{
		{"burst 5 ms apart", 0, 50, 5 * time.Millisecond, 50 * time.Millisecond},
		{"human pace, every 2 s", 0, 30, 2 * time.Second, 50 * time.Millisecond},
		{"one every 70 s, past a whole wait", 0, 5, 70 * time.Second, 50 * time.Millisecond},
		// two requests of the offer exchange; a write during the request
		// before it waits for that too
		{"human pace, 100 ms round trip", 100 * time.Millisecond, 30, 2 * time.Second, 300 * time.Millisecond},
		// writes faster than an exchange: a write waits for the exchange
		// under way (its offer, then its closing request held 200 ms) and
		// then the next one's offer
		{"burst 20 ms apart, 100 ms round trip", 100 * time.Millisecond, 50, 20 * time.Millisecond, 800 * time.Millisecond},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				net, a, b, room := outboundPair(t, c.rtt)
				sendEvery(t, net, a, b, room, c.n, c.gap, c.bound)
				net.run(t, time.Hour)
				sendEvery(t, net, a, b, room, 1, 0, c.bound)
			})
		})
	}
}

// Two machines that both listen, writing in turn every 2 s as a
// conversation does: each write reaches the other at once. Each side's own
// offers must not back it off, or both are backed off together and a write
// has no path until one back-off ends.
//
// Requirements: D2
func TestTwoListeningMachinesTakingTurnsEveryTwoSecondsReachEachOtherAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net := newRecNet()
		a, b := newSNode(t, "a", net), newSNode(t, "b", net)
		room := sgroup(t, a, b)
		schedule(t, net, a, b)
		net.run(t, time.Minute)
		for i := range 30 {
			w, r := a, b
			if i%2 == 1 {
				w, r = b, a
			}
			sendEvery(t, net, r, w, room, 1, 2*time.Second, 50*time.Millisecond)
		}
	})
}

// Three members, b only connecting out: with c offline, b's messages every
// 2 s reach a at once; with c listening and writing too, b's reach a and
// c, and c's reach b, at once.
//
// Requirements: D2, N1
func TestInAThreeMemberRoomAnOutboundOnlyMachinesMessagesArriveAtOnce(t *testing.T) {
	setup := func(t *testing.T) (*recNet, *snode, *snode, *snode, string) {
		net := newRecNet()
		a, b, c := newSNode(t, "a", net), newSNode(t, "b", net), newSNode(t, "c", net)
		room := sgroup(t, a, b, c)
		net.memNet.mu.Lock()
		net.noInbound[b.id] = true
		net.memNet.mu.Unlock()
		return net, a, b, c, room
	}
	t.Run("one member offline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			net, a, b, c, room := setup(t)
			net.memNet.mu.Lock()
			net.down[c.id] = true
			net.memNet.mu.Unlock()
			schedule(t, net, a, b)
			net.run(t, time.Minute)
			sendEvery(t, net, a, b, room, 30, 2*time.Second, 50*time.Millisecond)
		})
	})
	t.Run("another member writing too", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			net, a, b, c, room := setup(t)
			schedule(t, net, a, b, c)
			net.run(t, time.Minute)
			for i := range 30 {
				if i%3 == 2 {
					at := net.clock.Now()
					id := c.send(room, "from c")
					awaitAt(t, net, b, room, id, at, 50*time.Millisecond, fmt.Sprintf("c's message %d", i))
				} else {
					at := net.clock.Now()
					id := b.send(room, "from b")
					awaitAt(t, net, a, room, id, at, 50*time.Millisecond, fmt.Sprintf("b's message %d", i))
					awaitAt(t, net, c, room, id, at, 50*time.Millisecond, fmt.Sprintf("b's message %d", i))
				}
				net.run(t, 2*time.Second)
			}
		})
	})
}

// A write made while the peer is backed off goes at the end of the
// back-off, exactly: 1 s after a failed request (jitter fixed at its
// middle), and the Retry-After of a 429; and then the next writes go at
// once again.
//
// Requirements: D2, P5
func TestAWriteDuringABackOffGoesWhenItEndsAndTheNextAtOnce(t *testing.T) {
	for _, c := range []struct {
		name  string
		err   func() error
		retry time.Duration
	}{
		{"after a failure", func() error { return errUnreachable }, time.Second},
		{"after a 429", func() error {
			e := pullErr(429, CodeRateLimited, "")
			e.RetryAfter = 3 * time.Second
			return e
		}, 3 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				net, a, b, room := outboundPair(t, 0)
				refusing := true
				net.mu.Lock()
				net.refuse = func(from, _ string, q *PullRequest) error {
					if from == b.id && refusing && len(q.Offer) > 0 {
						refusing = false
						return c.err()
					}
					return nil
				}
				net.mu.Unlock()
				n := len(net.sent())
				at := net.clock.Now()
				id := b.send(room, "during a back-off")
				net.settle(t)
				if a.holds(room, id) {
					t.Fatal("delivered through the refusal")
				}
				awaitAt(t, net, a, room, id, at, c.retry+20*time.Millisecond, "the write")
				log := from(net.sent()[n:], b.id)
				if len(log) < 2 || log[1].at.Sub(log[0].at) != c.retry {
					t.Fatalf("retry after %v, want %v: %+v", log[1].at.Sub(log[0].at), c.retry, log)
				}
				sendEvery(t, net, a, b, room, 10, 2*time.Second, 50*time.Millisecond)
			})
		})
	}
}

// An outbound-only admin changes the room's membership several times in
// a minute, and messages follow: every log entry and message reaches the
// listener at once. Its offer is of log entries, so the peer's progress is
// inferred from its log heads, not its digests.
//
// Requirements: D2, N1
func TestAnOutboundOnlyAdminsMembershipChangesReachTheListenerAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net := newRecNet()
		b, a := newSNode(t, "b", net), newSNode(t, "a", net)
		room := sgroup(t, b, a) // b created it: the admin
		net.memNet.mu.Lock()
		net.noInbound[b.id] = true
		net.memNet.mu.Unlock()
		schedule(t, net, a, b)
		net.run(t, time.Minute)
		ctx := context.Background()
		for i := range 6 {
			at := net.clock.Now()
			q, err := b.r.proposeAny(ctx, room, Change{Kind: core.EntryAdd, Target: junkNode(i), Name: fmt.Sprintf("j%d", i), History: true})
			if err != nil {
				t.Fatal(err)
			}
			if c, err := b.r.Confirm(ctx, q.ID, true); err != nil || c.State != store.RequestApproved {
				t.Fatalf("confirm: %+v %v", c, err)
			}
			for {
				st, err := a.r.State(room)
				if err != nil {
					t.Fatal(err)
				}
				if slices.Contains(st.Members, junkNode(i)) {
					break
				}
				if took := net.clock.Now().Sub(at); took > 50*time.Millisecond {
					t.Fatalf("change %d not at a %v after it was signed", i+1, took)
				}
				net.run(t, 10*time.Millisecond)
			}
			net.run(t, 3*time.Second)
			sendEvery(t, net, a, b, room, 1, 7*time.Second, 50*time.Millisecond)
		}
	})
}

// After a refusal as not a member, an outbound-only admin's next request
// sends its whole log, knowing nothing of the peer's (PROTOCOL 4.4): the
// peer's progress is inferred from its heads naming the new entry.
//
// Requirements: D2
func TestAfterARefusalAWholeLogResentIsSeenAsDeliveredByTheHeadsItNames(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net := newRecNet()
		b, a := newSNode(t, "b", net), newSNode(t, "a", net)
		room := sgroup(t, b, a)
		ctx := context.Background()
		if _, err := b.x.Pull(ctx, room, a.id, PullOpts{}); err != nil {
			t.Fatal(err)
		}
		gen, _ := b.r.Gen(room)
		if err := b.x.answered(ctx, room, a.id, gen, &PullRequest{Room: room}, notMember()); err != nil {
			t.Fatal(err)
		}
		q, err := b.r.proposeAny(ctx, room, Change{Kind: core.EntryAdd, Target: junkNode(1), Name: "j", History: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.r.Confirm(ctx, q.ID, true); err != nil {
			t.Fatal(err)
		}
		st, err := b.x.Pull(ctx, room, a.id, PullOpts{Offer: true})
		if err != nil {
			t.Fatal(err)
		}
		if ast, _ := a.r.State(room); !slices.Contains(ast.Members, junkNode(1)) {
			t.Fatal("the entry was not delivered")
		}
		if st.Given == 0 || st.Same == 0 {
			t.Fatalf("delivered, but not seen to be: %+v", st)
		}
	})
}

// hostileRun counts a's requests to h, a peer whose waits end at once,
// over one minute in which the room changes every gap, each request
// taking rtt: from a's own writes, or from those of another member c that
// a pulls. With warm, the minute follows a quiet minute (h has spun by
// then); without, it starts with the scheduler. It returns requests and
// those with offers.
func hostileRun(t *testing.T, rtt, gap time.Duration, byOther, warm bool) (int, int) {
	return hostileRunHeld(t, rtt, gap, byOther, warm, false)
}

// hostileRunHeld is hostileRun; with heldFirst, h holds waits as an
// honest peer does through the quiet minute (warm), and answers them at
// once from the minute counted on.
func hostileRunHeld(t *testing.T, rtt, gap time.Duration, byOther, warm, heldFirst bool) (int, int) {
	var n, offers int
	synctest.Test(t, func(t *testing.T) {
		net := newRecNet()
		net.delay = rtt
		a, h := newSNode(t, "a", net), newSNode(t, "h", net)
		ns := []*snode{a}
		writer := a
		var room string
		if byOther {
			c := newSNode(t, "c", net)
			room = sgroup(t, a, h, c)
			ns, writer = append(ns, c), c
		} else {
			room = sgroup(t, a, h)
		}
		var honest atomic.Bool
		honest.Store(heldFirst)
		h.x.after = func(d time.Duration) <-chan time.Time { // h's waits end at once
			if honest.Load() {
				return time.After(d)
			}
			c := make(chan time.Time, 1)
			c <- time.Now()
			return c
		}
		start := net.clock.Now()
		schedule(t, net, ns...)
		if warm {
			if heldFirst {
				// a held wait ended by a change: h has shown it holds
				net.run(t, 30*time.Second)
				writer.send(room, "while h holds")
			}
			net.run(t, time.Minute)
			honest.Store(false)
			start = net.clock.Now()
		}
		for net.clock.Now().Sub(start) < time.Minute {
			writer.send(room, "tick")
			net.run(t, gap)
		}
		n, offers = requests(net.sent(), a.id, h.id, start, start.Add(time.Minute))
	})
	return n, offers
}

// A peer that stores every offer but answers every wait at once is still
// backed off while the room changes, from this machine's writes or from
// another member's, every 1 s, 200 ms or 50 ms, with 100 ms requests,
// from the scheduler's start or after a quiet minute: it never holds a
// wait, so every excused exchange with it counts as a spin. Ceiling: 20
// requests to it in the minute, continuations included.
//
// Requirements: P5, D6
func TestAPeerThatStoresEveryOfferButAnswersEveryWaitAtOnceIsStillBackedOff(t *testing.T) {
	const ceiling = 20
	for _, c := range []struct {
		gap           time.Duration
		byOther, warm bool
	}{
		{time.Second, false, false}, {200 * time.Millisecond, false, false}, {50 * time.Millisecond, false, false},
		{200 * time.Millisecond, true, false}, {50 * time.Millisecond, true, false},
		{200 * time.Millisecond, false, true}, {50 * time.Millisecond, false, true},
		{50 * time.Millisecond, true, true},
	} {
		who, when := "this machine", "from the start"
		if c.byOther {
			who = "another member"
		}
		if c.warm {
			when = "after a quiet minute"
		}
		t.Run(fmt.Sprintf("changes every %v from %s, %s", c.gap, who, when), func(t *testing.T) {
			n, offers := hostileRun(t, 100*time.Millisecond, c.gap, c.byOther, c.warm)
			t.Logf("requests to the peer in a minute: %d, %d with offers", n, offers)
			if n > ceiling {
				t.Fatalf("%d requests in a minute, ceiling %d", n, ceiling)
			}
		})
	}
}

// A peer that held this machine's waits for a minute and then answers
// every wait at once, while the room changes every 200 ms or 50 ms with
// 100 ms requests. Its last hold excuses one exchange; after that, while
// changes keep coming faster than an exchange, so that no wait can show a
// hold, one exchange per SchedPace (at least 0.8 s with jitter) of two
// requests: ceiling 2 × 60 / 0.8 + 10 = 160 requests to it in the minute,
// continuations included. Once a wait goes without a change pending, it
// is answered at once and the peer backs off as any other.
//
// Requirements: P5, D6
func TestAPeerThatHeldWaitsAndThenAnswersThemAtOnceIsBackedOff(t *testing.T) {
	const ceiling = 160
	for _, gap := range []time.Duration{200 * time.Millisecond, 50 * time.Millisecond} {
		for _, byOther := range []bool{false, true} {
			who := "this machine"
			if byOther {
				who = "another member"
			}
			t.Run(fmt.Sprintf("changes every %v from %s", gap, who), func(t *testing.T) {
				n, offers := hostileRunHeld(t, 100*time.Millisecond, gap, byOther, true, true)
				t.Logf("requests to the peer in a minute: %d, %d with offers", n, offers)
				if n > ceiling {
					t.Fatalf("%d requests in a minute, ceiling %d", n, ceiling)
				}
			})
		}
	}
}

// A listener that stores every offer but stalls the exchange: it marks
// its answer of digests log more, answers the next opening with the same
// digests and log more again, and only then same. With 100 ms requests
// and a write every 50 ms, so that no wait goes without a change pending,
// the offer exchanges end same but stalled, are not excused, and the
// outbound-only machine backs off it. And the peer's log more alone does
// not keep one exchange going while each request carries the records
// written since (one request every round trip, for as long as the writes
// went on). Ceiling: 30 requests in the minute (excused, about 100: two
// requests per SchedPace; with log more alone kept going, 600).
//
// Requirements: P5
func TestAnOfferExchangeThatStallsIsNotExcused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net, a, b, room := outboundPair(t, 100*time.Millisecond)
		var last *PullResponse // the digests answer to replay
		net.memNet.mu.Lock()
		net.tamper = func(from, _ string, resp *PullResponse) {
			if from != b.id || resp.Detail != nil || resp.Page != nil {
				return
			}
			switch {
			case !resp.Same:
				resp.Log.More = true
				c := *resp
				last = &c
			case last != nil:
				resp.Same, resp.Authors, resp.Digests, resp.Sum = false, last.Authors, last.Digests, last.Sum
				resp.Log.More, last = true, nil
			}
		}
		net.memNet.mu.Unlock()
		start := net.clock.Now()
		for net.clock.Now().Sub(start) < time.Minute {
			b.send(room, "tick")
			net.run(t, 50*time.Millisecond)
		}
		n, offers := requests(net.sent(), b.id, a.id, start, start.Add(time.Minute))
		t.Logf("requests in a minute: %d, %d with offers", n, offers)
		if n > 30 {
			t.Fatalf("%d requests in a minute, ceiling 30", n)
		}
	})
}

// A peer that keeps answering with log more and new log heads, never
// taking the entries it is given: the exchange gives each entry once,
// then ends as stalled instead of resending them for ever.
//
// Requirements: P5
func TestAPeerThatKeepsAnsweringWithNewHeadsDoesNotHoldAnExchangeInALoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net := newRecNet()
		net.limit = 1000 // a loop stalls here instead of running for ever
		b, a := newSNode(t, "b", net), newSNode(t, "a", net)
		room := sgroup(t, b, a)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := b.x.Pull(ctx, room, a.id, PullOpts{}); err != nil {
			t.Fatal(err)
		}
		q, err := b.r.proposeAny(ctx, room, Change{Kind: core.EntryAdd, Target: junkNode(1), Name: "j", History: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.r.Confirm(ctx, q.ID, true); err != nil {
			t.Fatal(err)
		}
		k := 0
		net.memNet.mu.Lock()
		net.tamper = func(_, _ string, resp *PullResponse) {
			if resp.Same { // never same: an answer of digests that differ
				resp.Same, resp.Authors, resp.Digests = false, []string{b.id}, map[string]string{}
				resp.Sum = sumOf(resp.Digests)
			}
			k++
			sum := sha256.Sum256(fmt.Appendf(nil, "head %d", k))
			resp.Log.Heads, resp.Log.More = []string{hex.EncodeToString(sum[:16])}, true
		}
		net.memNet.mu.Unlock()
		n := len(net.sent())
		st, err := b.x.Pull(ctx, room, a.id, PullOpts{})
		got := len(net.sent()) - n
		t.Logf("requests: %d, stats %+v, err %v", got, st, err)
		if got > 10 {
			t.Fatalf("%d requests in one exchange", got)
		}
		if err == nil && st.Stalled == 0 {
			t.Fatalf("not ended as stalled: %+v", st)
		}
	})
}

// A peer that keeps answering an exchange that offered a record with log
// more and a new digest of the record's author, as if it stored something
// each time: one request that gives something earns one such answer, so
// the exchange ends after a few requests instead of looping.
//
// Requirements: P5
func TestAPeerWhoseDigestsKeepChangingAfterAnOfferDoesNotHoldAnExchangeInALoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net := newRecNet()
		net.limit = 1000 // a loop stalls here instead of running for ever
		b, a := newSNode(t, "b", net), newSNode(t, "a", net)
		room := sgroup(t, b, a)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := b.x.Pull(ctx, room, a.id, PullOpts{}); err != nil {
			t.Fatal(err)
		}
		b.send(room, "offered")
		k := 0
		net.memNet.mu.Lock()
		net.tamper = func(_, _ string, resp *PullResponse) {
			if resp.Detail != nil || resp.Page != nil || resp.More {
				return // a continuation's answer: as it is
			}
			k++
			sum := sha256.Sum256(fmt.Appendf(nil, "digest %d", k))
			resp.Same, resp.Authors = false, []string{b.id}
			resp.Digests = map[string]string{b.id: hex.EncodeToString(sum[:])}
			resp.Sum, resp.Log.More = sumOf(resp.Digests), true
		}
		net.memNet.mu.Unlock()
		n := len(net.sent())
		st, err := b.x.Pull(ctx, room, a.id, PullOpts{Offer: true})
		got := len(net.sent()) - n
		t.Logf("requests: %d, stats %+v, err %v", got, st, err)
		if st.Offered == 0 {
			t.Fatal("nothing offered: the test shows nothing")
		}
		if got > 2*pullRoundsPerPull {
			t.Fatalf("%d requests in one exchange", got)
		}
	})
}
