package node

// The requester's budget and batching (budget.go, sched.go): a machine
// that only connects out delivers its own changes as offers in its own
// pulls, which return at once, and must not be backed off for it; a
// request that carries an offer is never abandoned for a newer change;
// every request to a peer takes a token of its bucket, so no peer, honest
// or not, gets more than budgetCeiling requests in a minute. In a synctest
// bubble, as the other scheduler tests; recNet.oneWay delays both legs.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// budgetCeiling is the most requests one peer can get from this machine
// in any minute: the burst, then the rate.
const budgetCeiling = int(PullBudgetBurst + 60*PullBudgetRate)

// tokenGap is how long a request waits at most for its token once the
// burst is spent.
var tokenGap = time.Duration(math.Ceil(float64(time.Second) / PullBudgetRate))

// outboundPair is a listening machine a and an outbound-only machine b
// sharing a room, both scheduled, each leg of a request taking oneWay; a
// cannot reach b.
func outboundPair(t *testing.T, oneWay time.Duration) (net *recNet, a, b *snode, room string) {
	t.Helper()
	net = newRecNet()
	net.oneWay = oneWay
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

// pending is messages sent and not yet seen at their reader.
type pending struct {
	id string
	at time.Time
}

// arrived removes from ps those r holds, failing if one took longer than
// bound, and returns the rest and the longest delay seen.
func arrived(t *testing.T, net *recNet, r *snode, room string, ps []pending, bound time.Duration, worst time.Duration) ([]pending, time.Duration) {
	t.Helper()
	now := net.clock.Now()
	out := ps[:0]
	for _, p := range ps {
		took := now.Sub(p.at)
		if r.holds(room, p.id) {
			worst = max(worst, took)
			continue
		}
		if took > bound {
			t.Fatalf("a message sent at %v not at %s after %v", p.at, r.name, took)
		}
		out = append(out, p)
	}
	return out, worst
}

// Every message of an outbound-only machine reaches the machine it pulls
// from at once: at human pace (one every 2 s), past a whole wait, and one
// after a quiet hour, within 50 ms; with no delay and with 50 ms each way.
// A write abandons the waiting pull, which carries nothing, and its offer
// goes at once; one made while an offer is on the way goes in the next
// request, once that answer is back. In a burst 5 ms apart the budget's
// burst is spent in the first few writes (each costs the abandoned wait,
// the offer and the closing request), and then a request waits up to
// tokenGap for its token: a write waits at most for the closing request
// of the exchange under way and its own offer, 2·tokenGap and 50 ms.
//
// Requirements: D2, N1
func TestAnOutboundOnlyMachinesMessagesReachTheListenerAtOnceAtAnyPace(t *testing.T) {
	for _, c := range []struct {
		name       string
		oneWay     time.Duration
		n          int
		gap, bound time.Duration
	}{
		{"burst 5 ms apart", 0, 50, 5 * time.Millisecond, 2*tokenGap + 50*time.Millisecond},
		{"human pace, every 2 s", 0, 30, 2 * time.Second, 50 * time.Millisecond},
		{"one every 70 s, past a whole wait", 0, 5, 70 * time.Second, 50 * time.Millisecond},
		// the offer's first leg
		{"human pace, 50 ms each way", 50 * time.Millisecond, 30, 2 * time.Second, 60 * time.Millisecond},
		// writes faster than an exchange: at most the round trip of the
		// request on the way, and the first leg of the next
		{"burst 20 ms apart, 50 ms each way", 50 * time.Millisecond, 50, 20 * time.Millisecond, 3*50*time.Millisecond + 20*time.Millisecond},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				net, a, b, room := outboundPair(t, c.oneWay)
				sendEvery(t, net, a, b, room, c.n, c.gap, c.bound)
				net.run(t, time.Hour)
				sendEvery(t, net, a, b, room, 1, 0, c.bound)
			})
		})
	}
}

// On a slow link, 1 s, 2 s or 3 s each way, with writes every 200 ms or
// every 2 s for two minutes: every message arrives while the writes go
// on, within three legs and a gap of its send (the request on the way
// when it was written, then the first leg of the next, which carries it),
// and b's requests stay within the budget. A request with an offer is
// never abandoned for a newer write; before the budget, each write
// abandoned the offer on the way, and nothing arrived until writes
// stopped.
//
// Requirements: D2, N1
func TestOnASlowLinkEveryMessageArrivesWhileWritesContinue(t *testing.T) {
	for _, oneWay := range []time.Duration{time.Second, 2 * time.Second, 3 * time.Second} {
		for _, gap := range []time.Duration{200 * time.Millisecond, 2 * time.Second} {
			t.Run(fmt.Sprintf("%v each way, a write every %v", oneWay, gap), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					net, a, b, room := outboundPair(t, oneWay)
					bound := 3*oneWay + gap + 100*time.Millisecond
					start := net.clock.Now()
					var ps []pending
					var worst time.Duration
					n := 0
					for net.clock.Now().Sub(start) < 2*time.Minute {
						ps = append(ps, pending{b.send(room, "slow"), net.clock.Now()})
						n++
						net.run(t, gap)
						ps, worst = arrived(t, net, a, room, ps, bound, worst)
					}
					for len(ps) > 0 {
						net.run(t, 100*time.Millisecond)
						ps, worst = arrived(t, net, a, room, ps, bound, worst)
					}
					reqs, offers := requests(net.sent(), b.id, a.id, start, net.clock.Now())
					t.Logf("%d messages, worst %v (bound %v), %d requests, %d with offers", n, worst, bound, reqs, offers)
					if per := float64(reqs) / net.clock.Now().Sub(start).Minutes(); per > float64(budgetCeiling) {
						t.Fatalf("%.0f requests a minute, budget %d", per, budgetCeiling)
					}
				})
			})
		}
	}
}

// A burst of 2,000 messages written 1 ms apart by an outbound-only
// machine, 5 ms each way, arrives within 2 s of the last write in a few
// dozen requests: writes made while a request is on the way go together in
// the next (at most 512 records an offer). Before, each write abandoned
// the request on the way.
//
// Requirements: D2, N1
func TestABurstOfTwoThousandMessagesArrivesInAFewExchanges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net, a, b, room := outboundPair(t, 5*time.Millisecond)
		start := net.clock.Now()
		ids := make([]string, 0, 2000)
		for range 2000 {
			ids = append(ids, b.send(room, "burst"))
			time.Sleep(time.Millisecond)
		}
		last := net.clock.Now()
		for _, id := range ids {
			awaitAt(t, net, a, room, id, last, 2*time.Second, "a message of the burst")
		}
		reqs, offers := requests(net.sent(), b.id, a.id, start, net.clock.Now())
		t.Logf("written in %v, all at a %v after the last, %d requests, %d with offers", last.Sub(start), net.clock.Now().Sub(last), reqs, offers)
		if reqs > 150 {
			t.Fatalf("%d requests for the burst, want at most 150", reqs)
		}
	})
}

// Two listening machines each writing every 50 ms for a minute, 5 ms each
// way: every message reaches the other within 1 s, and neither sends the
// other more than the budget, budgetCeiling requests in the minute (the
// listener's limit is twice that). Before, about 3,600.
//
// Requirements: D2, P5
func TestAnHonestPairWritingEvery50MsStaysWithinTheBudgetAndDeliversEverything(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net := newRecNet()
		net.oneWay = 5 * time.Millisecond
		a, b := newSNode(t, "a", net), newSNode(t, "b", net)
		room := sgroup(t, a, b)
		schedule(t, net, a, b)
		net.run(t, time.Minute)
		start := net.clock.Now()
		var toA, toB []pending
		var worst time.Duration
		for net.clock.Now().Sub(start) < time.Minute {
			toB = append(toB, pending{a.send(room, "from a"), net.clock.Now()})
			toA = append(toA, pending{b.send(room, "from b"), net.clock.Now()})
			net.run(t, 50*time.Millisecond)
			toB, worst = arrived(t, net, b, room, toB, time.Second, worst)
			toA, worst = arrived(t, net, a, room, toA, time.Second, worst)
		}
		for len(toA)+len(toB) > 0 {
			net.run(t, 10*time.Millisecond)
			toB, worst = arrived(t, net, b, room, toB, time.Second, worst)
			toA, worst = arrived(t, net, a, room, toA, time.Second, worst)
		}
		ab, _ := requests(net.sent(), a.id, b.id, start, start.Add(time.Minute))
		ba, _ := requests(net.sent(), b.id, a.id, start, start.Add(time.Minute))
		t.Logf("worst delay %v; requests in the minute a to b %d, b to a %d", worst, ab, ba)
		if ab > budgetCeiling || ba > budgetCeiling {
			t.Fatalf("requests in a minute: a to b %d, b to a %d; budget %d", ab, ba, budgetCeiling)
		}
	})
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
// listener at once. Its offer is of log entries, which the exchange gives
// as it gives records.
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

// hostile is a run of honest a against peer h, which answers as mode
// says, each leg taking 50 ms. With warm, a quiet minute first (with
// heldFirst h holds waits through it, as an honest peer does, and stops
// after); then a minute in which the room changes every gap (gap 0: not at
// all), from a's writes or, with byOther, from those of a member c that a
// pulls. tamper, if set, rewrites h's answers to a. It returns a's
// requests to h in that minute, all a's requests to h, and the tokens a
// took for h.
type hostile struct {
	gap                      time.Duration
	byOther, warm, heldFirst bool
	tamper                   func(resp *PullResponse)
}

func (c hostile) run(t *testing.T) (minute, all int, tokens int64) {
	synctest.Test(t, func(t *testing.T) {
		net := newRecNet()
		net.oneWay = 50 * time.Millisecond
		a, h := newSNode(t, "a", net), newSNode(t, "h", net)
		ns := []*snode{a}
		writer := a
		var room string
		if c.byOther {
			o := newSNode(t, "c", net)
			room = sgroup(t, a, h, o)
			ns, writer = append(ns, o), o
		} else {
			room = sgroup(t, a, h)
		}
		var honest atomic.Bool
		honest.Store(c.heldFirst)
		h.x.after = func(d time.Duration) <-chan time.Time { // h's waits end at once
			if honest.Load() {
				return time.After(d)
			}
			ch := make(chan time.Time, 1)
			ch <- time.Now()
			return ch
		}
		if c.tamper != nil {
			net.memNet.mu.Lock()
			net.tamper = func(from, to string, resp *PullResponse) {
				if from == a.id && to == h.id {
					c.tamper(resp)
				}
			}
			net.memNet.mu.Unlock()
		}
		start := net.clock.Now()
		schedule(t, net, ns...)
		if c.warm {
			net.run(t, time.Minute)
			honest.Store(false)
			start = net.clock.Now()
		}
		for net.clock.Now().Sub(start) < time.Minute {
			if c.gap == 0 {
				net.run(t, time.Minute)
				break
			}
			writer.send(room, "tick")
			net.run(t, c.gap)
		}
		minute, _ = requests(net.sent(), a.id, h.id, start, start.Add(time.Minute))
		all, _ = requests(net.sent(), a.id, h.id, time.Time{}, net.clock.Now().Add(time.Hour))
		tokens = a.x.budget.takenBy(h.id)
	})
	return minute, all, tokens
}

// check fails unless the minute stayed within ceiling and every request
// that went to h took exactly one token, kept.
func (c hostile) check(t *testing.T, ceiling int) {
	t.Helper()
	minute, all, tokens := c.run(t)
	t.Logf("requests to the peer in the minute: %d (ceiling %d); in all %d, tokens %d", minute, ceiling, all, tokens)
	if minute > ceiling {
		t.Fatalf("%d requests in a minute, ceiling %d", minute, ceiling)
	}
	if int64(all) != tokens {
		t.Fatalf("%d requests to the peer, %d tokens taken and kept", all, tokens)
	}
}

// A peer that answers every wait at once (it stores every offer). While
// the room changes every 200 ms or 50 ms from this machine's writes, every
// 50 ms from another member's (colluding with the peer), or every 50 ms
// after a quiet minute, every exchange carries an offer and the
// budget bounds them: at most budgetCeiling requests in the minute, every
// one with a token. With the room quiet, its waits back off as before the
// budget: at most 10 in the minute. Before, over 1,100 in a minute.
//
// Requirements: P5, D6
func TestAPeerThatAnswersEveryWaitAtOnceGetsNoMoreThanTheBudget(t *testing.T) {
	for _, c := range []hostile{
		{gap: 200 * time.Millisecond}, {gap: 50 * time.Millisecond},
		{gap: 50 * time.Millisecond, byOther: true}, {gap: 50 * time.Millisecond, warm: true},
	} {
		who, when := "this machine", "from the start"
		if c.byOther {
			who = "another member"
		}
		if c.warm {
			when = "after a quiet minute"
		}
		t.Run(fmt.Sprintf("changes every %v from %s, %s", c.gap, who, when), func(t *testing.T) {
			c.check(t, budgetCeiling)
		})
	}
	t.Run("a quiet room", func(t *testing.T) {
		hostile{warm: true}.check(t, 10)
	})
}

// A peer that held this machine's waits for a minute and then answers
// every wait at once, while the room changes every 50 ms: its
// earlier holds earn nothing; the budget bounds it.
//
// Requirements: P5, D6
func TestAPeerThatHeldWaitsAndThenAnswersThemAtOnceGetsNoMoreThanTheBudget(t *testing.T) {
	for _, gap := range []time.Duration{50 * time.Millisecond} {
		for _, byOther := range []bool{false, true} {
			who := "this machine"
			if byOther {
				who = "another member"
			}
			t.Run(fmt.Sprintf("changes every %v from %s", gap, who), func(t *testing.T) {
				hostile{gap: gap, byOther: byOther, warm: true, heldFirst: true}.check(t, budgetCeiling)
			})
		}
	}
}

// A peer whose every answer reports progress: new log heads and log more
// in each, never same; with the room changing every 50 ms and with it
// quiet. Each exchange ends at pullMaxRequests at most and the next pays
// for its requests: at most budgetCeiling in the minute.
//
// Requirements: P5
func TestAPeerThatAlwaysReportsProgressGetsNoMoreThanTheBudget(t *testing.T) {
	var k atomic.Int64
	progress := func(resp *PullResponse) {
		if resp.Same {
			resp.Same, resp.Authors, resp.Digests = false, []string{"00"}, map[string]string{}
			resp.Sum = sumOf(resp.Digests)
		}
		sum := sha256.Sum256(fmt.Appendf(nil, "head %d", k.Add(1)))
		resp.Log.Heads, resp.Log.More = []string{hex.EncodeToString(sum[:16])}, true
	}
	for _, gap := range []time.Duration{50 * time.Millisecond, 0} {
		t.Run(fmt.Sprintf("changes every %v", gap), func(t *testing.T) {
			hostile{gap: gap, warm: true, tamper: progress}.check(t, budgetCeiling)
		})
	}
}

// A peer that always says more: each answer of digests carries a new
// digest of the author offered and log more, as if it kept storing; with
// the room changing every 50 ms. At most budgetCeiling in the minute.
//
// Requirements: P5
func TestAPeerThatAlwaysSaysMoreGetsNoMoreThanTheBudget(t *testing.T) {
	var k atomic.Int64
	more := func(resp *PullResponse) {
		if resp.Detail != nil || resp.Page != nil {
			resp.More = true
			return
		}
		sum := sha256.Sum256(fmt.Appendf(nil, "digest %d", k.Add(1)))
		resp.Same = false
		if len(resp.Authors) == 0 {
			resp.Authors = []string{"00"}
		}
		resp.Digests = map[string]string{resp.Authors[0]: hex.EncodeToString(sum[:])}
		resp.Sum, resp.Log.More = sumOf(resp.Digests), true
	}
	hostile{gap: 50 * time.Millisecond, warm: true, tamper: more}.check(t, budgetCeiling)
}

// After a refusal as not a member, an outbound-only admin's next request
// sends its whole log, knowing nothing of the peer's (PROTOCOL 4.4): the
// entry is delivered in that exchange, which counts the entries it gave.
//
// Requirements: D2
func TestAfterARefusalAWholeLogResentDeliversTheNewEntry(t *testing.T) {
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
		if st.Given == 0 {
			t.Fatalf("delivered, but the exchange counts no entries given: %+v", st)
		}
	})
}

// A peer that keeps answering with log more and new log heads, never
// taking the entries it is given: the exchange gives each entry once and
// ends within pullMaxRequests. Before, 1,000 requests in one exchange (the
// limit of this test's transport).
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
		if err != nil {
			t.Fatal(err)
		}
		if got > pullMaxRequests {
			t.Fatalf("%d requests in one exchange, cap %d", got, pullMaxRequests)
		}
	})
}

// A peer that keeps answering an exchange that offered a record with log
// more and a new digest of the record's author, as if it stored something
// each time: the exchange ends within pullMaxRequests.
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
		if got > pullMaxRequests {
			t.Fatalf("%d requests in one exchange, cap %d", got, pullMaxRequests)
		}
	})
}

// Rooms and anchors that share a peer share its bucket: a machine in
// four rooms with one peer, each room writing every 50 ms, sends that
// peer at most budgetCeiling requests in the minute, and every room's
// messages still arrive within 2 s.
//
// Requirements: P5
func TestRoomsSharingAPeerShareItsBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net := newRecNet()
		net.oneWay = 5 * time.Millisecond
		a, b := newSNode(t, "a", net), newSNode(t, "b", net)
		var rooms []string
		for range 4 {
			rooms = append(rooms, sgroup(t, a, b))
		}
		schedule(t, net, a, b)
		net.run(t, time.Minute)
		start := net.clock.Now()
		ps := map[string][]pending{}
		var worst time.Duration
		for net.clock.Now().Sub(start) < time.Minute {
			for _, r := range rooms {
				ps[r] = append(ps[r], pending{a.send(r, "many rooms"), net.clock.Now()})
			}
			net.run(t, 50*time.Millisecond)
			for _, r := range rooms {
				ps[r], worst = arrived(t, net, b, r, ps[r], 2*time.Second, worst)
			}
		}
		ab, _ := requests(net.sent(), a.id, b.id, start, start.Add(time.Minute))
		t.Logf("worst delay %v; requests a to b in the minute %d", worst, ab)
		if ab > budgetCeiling {
			t.Fatalf("%d requests in a minute, budget %d", ab, budgetCeiling)
		}
	})
}

// Eight rooms with one peer start together and then write every 50 ms,
// 20 ms each way: at most PullBudgetSlots requests that do not wait are in
// flight to the peer at once, below the listener's 4, and every message
// still arrives within 2 s. Before, every room's exchange went at once.
//
// Requirements: P5
func TestRoomsStartingTogetherKeepFewerRequestsInFlightThanTheListenerAllows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net := newRecNet()
		net.oneWay = 20 * time.Millisecond
		a, b := newSNode(t, "a", net), newSNode(t, "b", net)
		var rooms []string
		for range 8 {
			rooms = append(rooms, sgroup(t, a, b))
		}
		schedule(t, net, a, b)
		net.run(t, time.Minute)
		start := net.clock.Now()
		ps := map[string][]pending{}
		var worst time.Duration
		for net.clock.Now().Sub(start) < 10*time.Second {
			for _, r := range rooms {
				ps[r] = append(ps[r], pending{a.send(r, "together"), net.clock.Now()})
			}
			net.run(t, 50*time.Millisecond)
			for _, r := range rooms {
				ps[r], worst = arrived(t, net, b, r, ps[r], 2*time.Second, worst)
			}
		}
		net.mu.Lock()
		peak := net.peak[[2]string{a.id, b.id}]
		net.mu.Unlock()
		t.Logf("most requests in flight from a to b at once: %d; worst delay %v", peak, worst)
		if peak > PullBudgetSlots || peak >= 4 {
			t.Fatalf("%d requests in flight to one peer, at most %d (the listener allows 4)", peak, PullBudgetSlots)
		}
	})
}

// An outbound-only machine writes a backlog larger than one exchange can
// carry (its offers cut to 2 records a request, 300 records: some 150
// requests against the cap of pullMaxRequests) and then nothing more. The
// exchange that ends Capped does not count the writes as offered, the
// next follows at once, and every record arrives. Before, the next
// exchange offered nothing and the rest waited for a later write.
//
// Requirements: D2, N1
func TestABacklogLargerThanOneExchangeArrivesWithNoLaterWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net := newRecNet()
		net.oneWay = 5 * time.Millisecond
		a, b := newSNode(t, "a", net), newSNode(t, "b", net)
		b.x.offerLimit = 2
		room := sgroup(t, a, b)
		net.memNet.mu.Lock()
		net.noInbound[b.id] = true
		net.memNet.mu.Unlock()
		schedule(t, net, a, b)
		net.run(t, time.Minute)
		start := net.clock.Now()
		var ids []string
		for range 300 {
			ids = append(ids, b.send(room, "backlog"))
		}
		for _, id := range ids {
			awaitAt(t, net, a, room, id, start, time.Minute, "a record of the backlog")
		}
		reqs, offers := requests(net.sent(), b.id, a.id, start, net.clock.Now())
		t.Logf("all at a after %v, %d requests, %d with offers", net.clock.Now().Sub(start), reqs, offers)
		if offers <= pullMaxRequests {
			t.Fatalf("%d requests with offers: the backlog fitted in one exchange, the test shows nothing", offers)
		}
	})
}

// The detail of an exchange that ended Capped starts, in the next one, at
// the author it was taking, and wraps round: an early author that differs
// for ever cannot keep later ones from their turn.
//
// Requirements: N1
func TestTheNextExchangeTakesDetailFromWhereACappedOneStopped(t *testing.T) {
	x := &Exchange{resume: map[[2]string]string{}}
	differ := []string{"a1", "b2", "c3", "d4"}
	check := func(what string, d []string, from, before []string) {
		t.Helper()
		f, b := x.resumed("r", "p", d)
		if !slices.Equal(f, from) || !slices.Equal(b, before) {
			t.Fatalf("%s: %v then %v, want %v then %v", what, f, b, from, before)
		}
	}
	check("with nothing to resume", differ, differ, nil)
	x.setResume("r", "p", "c3")
	check("resumed", differ, []string{"c3", "d4"}, []string{"a1", "b2"})
	check("resumed past an author no longer differing", []string{"a1", "d4"}, []string{"d4"}, []string{"a1"})
	if f, b := x.resumed("r", "q", differ); !slices.Equal(f, differ) || b != nil {
		t.Fatalf("another peer: %v %v", f, b)
	}
	x.setResume("r", "p", "")
	check("after a round that ended", differ, differ, nil)
}

// A star: one listening hub and four outbound-only spokes in one room,
// 5 ms each way, each spoke writing every 500 ms (staggered) for a minute,
// 8 messages a second in the room. Every message reaches every machine,
// a spoke's through the hub, and no spoke sends the hub more than the
// budget (budgetCeiling in the minute). The spokes run near the ceiling:
// each pays for the messages it receives (the released wait and the page)
// as well as those it sends; the budget, not the listener's limit, paces
// them, and delivery goes on by batching. The worst spoke-to-spoke delay
// is logged; a bound of 5 s fails only a stall.
//
// Requirements: D2, N1
func TestAStarOfFourOutboundOnlySpokesDeliversEverythingWithinTheBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net := newRecNet()
		net.oneWay = 5 * time.Millisecond
		hub := newSNode(t, "hub", net)
		var spokes []*snode
		for i := range 4 {
			spokes = append(spokes, newSNode(t, fmt.Sprintf("s%d", i), net))
		}
		room := sgroup(t, hub, spokes...)
		net.memNet.mu.Lock()
		for _, s := range spokes {
			net.noInbound[s.id] = true
		}
		net.memNet.mu.Unlock()
		schedule(t, net, append([]*snode{hub}, spokes...)...)
		net.run(t, time.Minute)
		type sentTo struct {
			p  pending
			to *snode
		}
		var ps []sentTo
		start := net.clock.Now()
		worst := time.Duration(0)
		check := func() {
			now := net.clock.Now()
			rest := ps[:0]
			for _, x := range ps {
				took := now.Sub(x.p.at)
				if x.to.holds(room, x.p.id) {
					worst = max(worst, took)
					continue
				}
				if took > 5*time.Second {
					t.Fatalf("a message sent at %v not at %s after %v", x.p.at.Sub(start), x.to.name, took)
				}
				rest = append(rest, x)
			}
			ps = rest
		}
		step := 125 * time.Millisecond // one spoke writes each step, each every 500 ms
		for i := 0; net.clock.Now().Sub(start) < time.Minute; i++ {
			w := spokes[i%len(spokes)]
			id := w.send(room, "spoke")
			for _, r := range spokes {
				if r != w {
					ps = append(ps, sentTo{pending{id, net.clock.Now()}, r})
				}
			}
			for range 5 {
				net.run(t, step/5)
				check()
			}
		}
		for len(ps) > 0 {
			net.run(t, 10*time.Millisecond)
			check()
		}
		for _, s := range spokes {
			n, offers := requests(net.sent(), s.id, hub.id, start, start.Add(time.Minute))
			t.Logf("%s: %d requests to the hub in the minute (%d with offers)", s.name, n, offers)
			if n > budgetCeiling {
				t.Fatalf("%s sent the hub %d requests in a minute, ceiling %d", s.name, n, budgetCeiling)
			}
		}
		t.Logf("worst spoke-to-spoke delay %v", worst)
	})
}

// A restart starts with full buckets: the budget is in memory, so a
// restarted daemon (a new Exchange) may send a peer its burst at once even
// if the last run had just spent it. That is the boundary of the stated
// bound: at most burst + 60·rate in any minute of one run of the daemon.
//
// Requirements: N1
func TestARestartStartsTheBudgetFull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net := newRecNet()
		a := newSNode(t, "a", net)
		ctx := context.Background()
		admitAll := func(x *Exchange) (at time.Duration) {
			start := time.Now()
			for range int(PullBudgetBurst) {
				tk, err := x.budget.admit(ctx, "peer", nil)
				if err != nil {
					t.Fatal(err)
				}
				tk.done()
			}
			return time.Since(start)
		}
		admitAll(a.x)
		took := admitAll(a.x)
		if took < time.Duration(PullBudgetBurst-1)*tokenGap {
			t.Fatalf("a spent bucket admitted its burst again in %v", took)
		}
		restarted := NewExchange(a.r, net.link(a.id), ExchangeOpts{After: net.clock.After})
		if took := admitAll(restarted); took != 0 {
			t.Fatalf("after a restart the burst took %v, want at once", took)
		}
	})
}

// A peer that says it holds nothing of this machine's records and changes
// its digest after every offer, in a quiet room: this machine has 80
// records and offers 1 a request, so one exchange's offer is cut by the
// cap of pullMaxRequests. After one local write, the run of cut exchanges
// goes on only while each offers records no earlier one of the run had
// offered; then the peer, which still claims nothing, is backed off. The
// requests a minute fall as the back-off grows: at most 250 in the first
// minute after the write (the run offers each record once, about 2
// exchanges), and at most 100 in the four minutes after. Before, about 600
// a minute, still at five minutes.
//
// Requirements: N1, P5
func TestAPeerThatClaimsToHoldNothingAfterEveryOfferIsBackedOffAfterOneWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net := newRecNet()
		net.oneWay = 5 * time.Millisecond
		b, a := newSNode(t, "b", net), newSNode(t, "a", net)
		b.x.offerLimit = 1
		room := sgroup(t, b, a)
		for range 80 {
			b.send(room, "history")
		}
		k := 0
		net.memNet.mu.Lock()
		net.tamper = func(from, to string, resp *PullResponse) {
			if from != b.id || to != a.id {
				return
			}
			if resp.Detail != nil {
				resp.Detail = map[string][]store.StreamDetail{b.id: {}} // holds nothing of b
				resp.Page, resp.More = nil, false
				return
			}
			k++
			sum := sha256.Sum256(fmt.Appendf(nil, "digest %d", k))
			resp.Same, resp.Authors = false, []string{b.id}
			resp.Digests = map[string]string{b.id: hex.EncodeToString(sum[:])}
			resp.Sum = sumOf(resp.Digests)
		}
		net.memNet.mu.Unlock()
		schedule(t, net, a, b)
		net.run(t, 2*time.Minute) // the first offers of the history, then quiet
		b.send(room, "one write")
		start := net.clock.Now()
		var per, offers []int
		for i := range 5 {
			net.run(t, time.Minute)
			n, o := requests(net.sent(), b.id, a.id, start.Add(time.Duration(i)*time.Minute), start.Add(time.Duration(i+1)*time.Minute))
			per, offers = append(per, n), append(offers, o)
		}
		t.Logf("requests to the peer in each minute after the write: %v (with offers %v)", per, offers)
		if offers[0] < pullMaxRequests {
			t.Fatalf("%d offers after the write: no exchange reached the cap, the test shows nothing", offers[0])
		}
		if per[0] > 150 {
			t.Fatalf("%d requests in the first minute after one write, at most 150", per[0])
		}
		if rest := per[1] + per[2] + per[3] + per[4]; rest > 50 {
			t.Fatalf("%d requests in minutes 2 to 5 after one write, at most 50: not backed off (%v)", rest, per)
		}
	})
}
