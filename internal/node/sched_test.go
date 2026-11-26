package node

// The scheduler (sched.go) over the in-memory transport with a fake
// clock: every timer of the scheduler and every hold of a waiting pull at
// a server runs on it, so fake minutes pass in real milliseconds.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// The scheduler tests run in a synctest bubble: time.Now and time.After
// are the bubble's fake clock, which moves only when every goroutine of the
// test is blocked, so fake minutes pass in real milliseconds and "nothing
// is moving" is synctest.Wait, not a wall-clock pause.
type fakeClock struct{}

func newFakeClock() *fakeClock                            { return &fakeClock{} }
func (*fakeClock) Now() time.Time                         { return time.Now() }
func (*fakeClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// sent is one request as a requester sent it.
type sent struct {
	from, to, room string
	wait           int64
	offer          int
	at             time.Time
}

// recNet records every request on a memNet before passing it on.
type recNet struct {
	*memNet
	clock *fakeClock
	mu    sync.Mutex
	log   []sent
	done  int
	// limit, above 0, stalls every request after that many (until its
	// context ends), so a scheduler that spins cannot spin for ever.
	limit int
	// oneWay, above 0, is each leg's delay: fake time passes before a
	// request reaches the server and again before its answer reaches the
	// requester (an answer whose context ends on the way back is lost,
	// though the server applied its request).
	oneWay time.Duration
	// refuse, if set and it returns an error, refuses a request with it
	// before it reaches the server.
	refuse func(from, to string, q *PullRequest) error
	// inflight and peak count, per requester and peer, requests without
	// wait_ms sent and not yet answered, and the most at once.
	inflight, peak map[[2]string]int
}

func (n *recNet) link(from string) PullTransport { return &recLink{n, n.memNet.link(from), from} }

type recLink struct {
	n    *recNet
	l    PullTransport
	from string
}

func (l *recLink) Pull(ctx context.Context, peer string, q *PullRequest) (*PullResponse, error) {
	l.n.mu.Lock()
	l.n.log = append(l.n.log, sent{from: l.from, to: peer, room: q.Room, wait: q.WaitMS, offer: len(q.Offer), at: l.n.clock.Now()})
	over := l.n.limit > 0 && len(l.n.log) > l.n.limit
	delay, refuse := l.n.oneWay, l.n.refuse
	k := [2]string{l.from, peer}
	if q.WaitMS == 0 {
		if l.n.inflight == nil {
			l.n.inflight, l.n.peak = map[[2]string]int{}, map[[2]string]int{}
		}
		l.n.inflight[k]++
		l.n.peak[k] = max(l.n.peak[k], l.n.inflight[k])
		defer func() {
			l.n.mu.Lock()
			l.n.inflight[k]--
			l.n.mu.Unlock()
		}()
	}
	l.n.mu.Unlock()
	if over {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	leg := func() error {
		if delay <= 0 {
			return nil
		}
		select {
		case <-time.After(delay):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := leg(); err != nil {
		return nil, err
	}
	if refuse != nil {
		if err := refuse(l.from, peer, q); err != nil {
			return nil, err
		}
	}
	defer func() {
		l.n.mu.Lock()
		l.n.done++
		l.n.mu.Unlock()
	}()
	resp, err := l.l.Pull(ctx, peer, q)
	if lerr := leg(); lerr != nil {
		return nil, lerr
	}
	return resp, err
}

func (n *recNet) sent() []sent {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.log)
}

// settle waits until every goroutine of the test is blocked: nothing can
// move without the clock.
func (n *recNet) settle(t *testing.T) {
	t.Helper()
	synctest.Wait()
}

// run lets d of fake time pass, every timer due in it firing in order.
func (n *recNet) run(t *testing.T, d time.Duration) {
	t.Helper()
	synctest.Wait()
	time.Sleep(d)
	synctest.Wait()
}

// snode is a machine on the fake clock with its exchange on a recNet.
type snode struct {
	t    *testing.T
	name string
	id   string
	st   *store.Store
	m    *Machine
	r    *Rooms
	x    *Exchange
}

func newSNode(t *testing.T, name string, net *recNet) *snode {
	t.Helper()
	seed := sha256.Sum256([]byte("sched test " + name))
	key := ed25519.NewKeyFromSeed(seed[:])
	st, err := store.Open(filepath.Join(t.TempDir(), "a2p2.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := NewMachine(st, key, MachineOpts{Now: net.clock.Now})
	r, err := OpenRooms(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	n := &snode{t: t, name: name, id: hex.EncodeToString(key.Public().(ed25519.PublicKey)), st: st, m: m, r: r}
	n.x = NewExchange(r, net.link(n.id), ExchangeOpts{After: net.clock.After})
	net.attach(n.id, n.x)
	if _, err := m.Register(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	return n
}

// sgroup makes a room on admin with the others admitted with history and
// handed the log (as a join would).
func sgroup(t *testing.T, admin *snode, others ...*snode) string {
	t.Helper()
	ctx := context.Background()
	room, err := admin.r.CreateRoom(ctx, "team")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range others {
		q, err := admin.r.proposeAny(ctx, room, Change{Kind: core.EntryAdd, Target: o.id, Name: o.name, History: true}) // test only: no join exchange
		if err != nil {
			t.Fatal(err)
		}
		if c, err := admin.r.Confirm(ctx, q.ID, true); err != nil || c.State != store.RequestApproved {
			t.Fatalf("confirm: %+v %v", c, err)
		}
	}
	ws, err := admin.r.Offer(room)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range others {
		if _, err := o.r.PinRoom(ctx, ws[0]); err != nil {
			t.Fatal(err)
		}
		if _, err := o.r.Receive(ctx, room, 0, ws...); err != nil {
			t.Fatal(err)
		}
	}
	return room
}

func newRecNet() *recNet { return &recNet{memNet: newMemNet(), clock: newFakeClock()} }

// schedule runs the scheduler of each of ns on the fake clock, with jitter
// fixed at its middle (no jitter), until the test ends.
func schedule(t *testing.T, net *recNet, ns ...*snode) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, n := range ns {
		s := NewScheduler(n.x, SchedOpts{Now: net.clock.Now, After: net.clock.After, Jitter: func() float64 { return 0.5 }})
		wg.Add(1)
		go func() { defer wg.Done(); s.Run(ctx) }()
	}
	t.Cleanup(func() { cancel(); wg.Wait() })
}

func (n *snode) send(room, text string) string {
	n.t.Helper()
	s, err := n.m.Send(context.Background(), Draft{Room: room, Agent: n.name, Text: text})
	if err != nil {
		n.t.Fatal(err)
	}
	return s.ID
}

func (n *snode) holds(room, id string) bool {
	n.t.Helper()
	found := false
	if err := n.st.View(context.Background(), func(rd *store.Reader) error {
		_, ok, err := rd.Record(room, id)
		found = ok
		return err
	}); err != nil {
		n.t.Fatal(err)
	}
	return found
}

func from(log []sent, id string) []sent {
	var out []sent
	for _, s := range log {
		if s.from == id {
			out = append(out, s)
		}
	}
	return out
}

// DESIGN 4, start: short pulls of the room from two members, then
// waiting pulls.
//
// Requirements: D2, D6
func TestSchedulerStartsWithShortPullsOfEveryRoomFromTwoMembers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net := newRecNet()
		a, b, c, d := newSNode(t, "a", net), newSNode(t, "b", net), newSNode(t, "c", net), newSNode(t, "d", net)
		room := sgroup(t, a, b, c, d)
		schedule(t, net, a)
		net.settle(t)
		log := from(net.sent(), a.id)
		first := slices.IndexFunc(log, func(s sent) bool { return s.wait > 0 })
		if first < 0 {
			t.Fatalf("no waiting pull: %+v", log)
		}
		peers := map[string]bool{}
		for _, s := range log[:first] {
			if s.room != room || s.wait != 0 {
				t.Fatalf("start request: %+v", s)
			}
			peers[s.to] = true
		}
		if len(peers) != 2 {
			t.Fatalf("start pulls went to %d members, want 2: %+v", len(peers), log[:first])
		}
		waits := map[string]int64{}
		for _, s := range log[first:] {
			waits[s.to] = s.wait
		}
		if len(waits) != 2 {
			t.Fatalf("waiting pulls to %d anchors, want 2: %+v", len(waits), log[first:])
		}
	})
}

// DESIGN 4, steady state: the rotating anchor goes through every
// member, past one that cannot be reached, each newly chosen one probed
// with a 10 s wait first; the stable anchor stays.
//
// Requirements: D2, D6, N2
func TestSchedulerRotatesTheSecondAnchorThroughEveryMemberPastAFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net := newRecNet()
		a := newSNode(t, "a", net)
		var others []*snode
		for _, name := range []string{"b", "c", "d", "e"} {
			others = append(others, newSNode(t, name, net))
		}
		sgroup(t, a, others...)
		down := others[2]
		net.memNet.mu.Lock()
		net.down[down.id] = true
		net.memNet.mu.Unlock()
		schedule(t, net, a)
		net.run(t, 10*time.Minute)
		log := from(net.sent(), a.id)
		probed := map[string]bool{}
		stable := map[string]int{}
		tried := map[string]bool{}
		for _, s := range log {
			tried[s.to] = true
			switch s.wait {
			case SchedProbe.Milliseconds():
				probed[s.to] = true
			case SchedWait.Milliseconds():
				stable[s.to]++
			}
		}
		// A first request that gives the peer log entries (it has never
		// answered, so its log is not known) carries no wait: the
		// unreachable member is tried, never with a wait.
		for _, o := range others {
			if o != down && !probed[o.id] && stable[o.id] == 0 {
				t.Errorf("%s was never an anchor", o.name)
			}
		}
		if !tried[down.id] {
			t.Error("the unreachable member was never tried")
		}
		var downAt []time.Time
		for _, s := range log {
			if s.to == down.id {
				downAt = append(downAt, s.at)
			}
		}
		for i := 1; i < len(downAt); i++ {
			if downAt[i].Sub(downAt[i-1]) < time.Second {
				t.Fatalf("the unreachable member was tried again after %v", downAt[i].Sub(downAt[i-1]))
			}
		}
		t.Logf("requests from a over 10 idle minutes with one member down: %d; probes %d", len(log), len(probed))
	})
}

// PROTOCOL 4.6: a sleeping peer is tried 1 s, 2 s, 4 s ... apart up
// to 5 min, and no more often; once it answers the back-off starts again
// from 1 s.
//
// Requirements: P5, D6
func TestSchedulerBacksOffASleepingPeerAndStartsAgainWhenItAnswers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net := newRecNet()
		a, b := newSNode(t, "a", net), newSNode(t, "b", net)
		sgroup(t, a, b)
		setDown := func(v bool) {
			net.memNet.mu.Lock()
			net.down[b.id] = v
			net.memNet.mu.Unlock()
		}
		setDown(true)
		schedule(t, net, a)
		net.run(t, 30*time.Minute)
		log := from(net.sent(), a.id)
		want := []time.Duration{1, 2, 4, 8, 16, 32, 64, 128, 256, 300, 300, 300, 300}
		if len(log) != len(want)+1 {
			t.Fatalf("%d attempts in 30 minutes, the schedule allows %d: %+v", len(log), len(want)+1, log)
		}
		for i, w := range want {
			if got := log[i+1].at.Sub(log[i].at); got != w*time.Second {
				t.Fatalf("gap %d: %v, want %v", i, got, w*time.Second)
			}
		}
		setDown(false)
		net.run(t, 5*time.Minute+time.Second)
		n := len(from(net.sent(), a.id))
		if n <= len(log) {
			t.Fatal("not tried again after waking")
		}
		setDown(true)
		// The waiting pull in progress ends; the next attempt fails, and the
		// one after it is 1 s later.
		net.run(t, 2*time.Minute)
		after := from(net.sent(), a.id)[n:]
		if len(after) < 2 {
			t.Fatalf("attempts after it slept again: %+v", after)
		}
		if g := after[1].at.Sub(after[0].at); g != time.Second {
			t.Fatalf("the back-off did not start again: first gap %v", g)
		}
	})
}

// DESIGN 4: a local write abandons the waiting pulls and issues them
// again at once, with an offer that carries the write, without any time
// passing.
//
// Requirements: D2, N1
func TestALocalWriteReissuesTheWaitingPullsAtOnceWithAnOffer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net := newRecNet()
		a, b, c := newSNode(t, "a", net), newSNode(t, "b", net), newSNode(t, "c", net)
		room := sgroup(t, a, b, c)
		schedule(t, net, a)
		net.settle(t)
		before := len(net.sent())
		at := net.clock.Now()
		id := a.send(room, "news")
		net.settle(t)
		if !b.holds(room, id) || !c.holds(room, id) {
			t.Fatalf("offered on: b %v c %v", b.holds(room, id), c.holds(room, id))
		}
		offered := map[string]bool{}
		for _, s := range net.sent()[before:] {
			if s.offer > 0 {
				offered[s.to] = true
			}
		}
		if !offered[b.id] || !offered[c.id] || !net.clock.Now().Equal(at) {
			t.Fatalf("offers to b %v c %v; time passed %v", offered[b.id], offered[c.id], net.clock.Now().Sub(at))
		}
	})
}

// No pull of a room this machine has left, ever again.
//
// Requirements: P5
func TestNoPullsAtAllForARoomThisMachineHasLeft(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net := newRecNet()
		a, b := newSNode(t, "a", net), newSNode(t, "b", net)
		room := sgroup(t, a, b)
		schedule(t, net, a)
		net.run(t, 3*time.Minute)
		if len(from(net.sent(), a.id)) == 0 {
			t.Fatal("no pulls before leaving")
		}
		if err := a.r.Leave(context.Background(), room); err != nil {
			t.Fatal(err)
		}
		net.settle(t)
		n := len(net.sent())
		net.run(t, 30*time.Minute)
		if extra := net.sent()[n:]; len(extra) > 0 {
			t.Fatalf("%d pulls after leaving: %+v", len(extra), extra)
		}
	})
}

// What an idle room costs: three machines, each with its two anchors,
// for 30 fake minutes.
//
// Requirements: P5
func TestAnIdleRoomCostsAFewRequestsPerMachinePerMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net := newRecNet()
		a, b, c := newSNode(t, "a", net), newSNode(t, "b", net), newSNode(t, "c", net)
		sgroup(t, a, b, c)
		schedule(t, net, a, b, c)
		net.run(t, time.Minute)
		n := len(net.sent())
		net.run(t, 30*time.Minute)
		per := float64(len(net.sent())-n) / 3 / 30
		t.Logf("requests per idle minute per room per machine: %.2f", per)
		if per > 4 {
			t.Fatalf("%.2f requests per idle minute", per)
		}
	})
}

// DESIGN 3: a daemon restored from an older copy signs nothing for a
// room until it has pulled the room's log from another member; then it
// can, on the log it pulled.
//
// Requirements: D6, T9
func TestARestoredDaemonSignsNothingForARoomUntilItHasPulledItsLog(t *testing.T) {
	clock := lclock()
	net := newMemNet()
	a := newPNode(t, net, "a", newLNode(t, "a", clock))
	b := newPNode(t, net, "b", newLNode(t, "b", clock))
	c := newPNode(t, net, "c", newLNode(t, "c", clock))
	room := a.create("team")
	a.change(room, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee", History: true})
	b.pin(room, a.lnode)
	b.pull(room, a.lnode)
	old := snapshot(t, a) // a restart too: a pulls before it signs again
	a.mustPull(room, b, PullOpts{})
	a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "cee", History: true})
	b.mustPull(room, a, PullOpts{}) // b holds the add of c
	restoreFrom(t, a, old)
	q := a.propose(room, Change{Kind: core.EntryAdd, Target: junkNode(1), Name: "j", History: true})
	signs := a.r.signs
	_, err := a.r.Confirm(context.Background(), q.ID, true)
	if !errors.Is(err, ErrNotCaughtUp) || a.r.signs != signs {
		t.Fatalf("before pulling: %v, signed %d", err, a.r.signs-signs)
	}
	a.mustPull(room, b, PullOpts{})
	if st := a.state(room); !slices.Contains(st.Members, c.id) {
		t.Fatal("the pull did not bring back the add of c")
	}
	if cf := a.confirm(q.ID); cf.State != store.RequestApproved {
		t.Fatalf("after pulling: %+v", cf)
	}
}

// PROTOCOL 4.5 as the scheduler applies it: a first 403 is tried
// again at once (the exchange resends its whole log), a second mutes the
// peer until this machine's log of the room changes; 413 halves the offer
// and retries at once; 429 and 503 wait their Retry-After; anything else
// backs off.
//
// Requirements: T5, P5
func TestTheSchedulerFollowsEachRefusalAsItsStatusSays(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net := newRecNet()
		a, b, c := newSNode(t, "a", net), newSNode(t, "b", net), newSNode(t, "c", net)
		room := sgroup(t, a, b, c)
		s := NewScheduler(a.x, SchedOpts{Now: net.clock.Now, After: net.clock.After, Jitter: func() float64 { return 0.5 }})
		has := func(p string) bool { return slices.Contains(s.eligible(room), p) }
		// As an exchange that b refuses: the exchange keeps the refusal.
		gen, _ := a.r.Gen(room)
		_, heads := s.roomNow(room)
		refuse := func() {
			q := &PullRequest{Room: room, Log: PullLog{Heads: heads}}
			if err := a.x.answered(context.Background(), room, b.id, gen, q, notMember()); err != nil {
				t.Fatal(err)
			}
			s.failed(room, b.id, notMember())
		}
		refuse()
		if !has(b.id) {
			t.Fatal("not tried again after the first 403")
		}
		refuse()
		if has(b.id) {
			t.Fatal("tried again after the second 403")
		}
		// The room log changes here: b is tried again.
		q, err := a.r.Propose(context.Background(), room, Change{Kind: core.EntryAdmin, Target: c.id})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.r.Confirm(context.Background(), q.ID, true); err != nil {
			t.Fatal(err)
		}
		if !has(b.id) {
			t.Fatal("still muted after the log changed")
		}
		s.failed(room, c.id, pullErr(413, CodeTooLarge, ""))
		if got := s.offerOf(room, c.id); got != MaxOfferRecords/2 || !has(c.id) {
			t.Fatalf("after a 413: offer %d, eligible %v", got, has(c.id))
		}
		s.failed(room, c.id, pullErr(413, CodeTooLarge, ""))
		if got := s.offerOf(room, c.id); got != MaxOfferRecords/4 {
			t.Fatalf("after a second 413: offer %d", got)
		}
		busy := pullErr(429, CodeRateLimited, "")
		busy.RetryAfter = 7 * time.Second
		s.failed(room, c.id, busy)
		if until := s.peers[[2]string{room, c.id}].until.Sub(net.clock.Now()); until != 7*time.Second {
			t.Fatalf("429 waits %v", until)
		}
		s.failed(room, b.id, errUnreachable)
		if until := s.peers[[2]string{room, b.id}].until.Sub(net.clock.Now()); until < time.Second {
			t.Fatalf("an unreachable peer backs off %v", until)
		}
	})
}
