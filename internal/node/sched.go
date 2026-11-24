package node

// The scheduler of the pull exchange: when to pull and from whom
// (DESIGN-next section 4, the trigger table; PROTOCOL-next 4.5 and 4.6).
//
//   - Start: short pulls of every room from two members.
//   - Steady state: a waiting pull per room to each of two anchors: the
//     most reliable member (stable), and one that rotates at every end,
//     cancellation or failure of a wait through every eligible member
//     (rotating); a newly chosen rotating anchor is first probed with a
//     short wait (10 s). The transport tries each of a member's addresses
//     in turn (N2).
//   - A local write or newly stored records: the waiting pulls of the room
//     are abandoned and issued again at once, with offers.
//   - Failure: back off 1 s doubling to 5 min, with ±20 % jitter, and move
//     on to the next member. 403 twice running (the exchange resends its
//     whole log after the first) stops pulling the room from that peer
//     until this machine's log of the room changes; 413 halves the offer;
//     429 and 503 wait their Retry-After.
//
// Clock, timers, jitter and transport are injected, so tests are
// deterministic. Peers' addresses are the Exchange's AddrBook, in memory.
// Nothing in the daemon starts a Scheduler yet: that happens at the switch.

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"sync"
	"time"
)

// Timing of DESIGN-next 4 and PROTOCOL-next 4.6.
const (
	SchedWait       = 60 * time.Second // a waiting pull
	SchedProbe      = 10 * time.Second // the probe of a newly chosen rotating anchor
	SchedBackoffMin = time.Second
	SchedBackoffMax = 5 * time.Minute
)

// SchedOpts injects the clock, timers and jitter; zero values are the
// real ones and the timing above.
type SchedOpts struct {
	Now    func() time.Time
	After  func(time.Duration) <-chan time.Time
	Jitter func() float64 // uniform in [0, 1)
	Wait   time.Duration
	Probe  time.Duration
}

// Scheduler decides when this machine pulls each room, and from whom.
type Scheduler struct {
	x *Exchange
	o SchedOpts

	mu    sync.Mutex
	peers map[[2]string]*peerState // per room and peer
	rooms map[string]*roomSched
}

// peerState is what the scheduler knows of one peer for one room.
type peerState struct {
	fails   int       // failures since the last success
	ok      int       // successes
	until   time.Time // not before
	refused int       // 403s running
	muted   []string  // set: this machine's log heads when the peer refused twice
	offer   int       // offer limit after a 413; 0: the exchange's
	spins   int       // waiting pulls running that neither waited nor stored anything
}

// roomSched is one room's anchors: the peer each is pulling from, and the
// rotation.
type roomSched struct {
	cur [2]string
	rot int
}

const (
	slotStable = iota
	slotRotating
)

// NewScheduler schedules the pulls of x.
func NewScheduler(x *Exchange, o SchedOpts) *Scheduler {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.After == nil {
		o.After = time.After
	}
	if o.Jitter == nil {
		o.Jitter = rand.Float64
	}
	if o.Wait <= 0 {
		o.Wait = SchedWait
	}
	if o.Probe <= 0 {
		o.Probe = SchedProbe
	}
	return &Scheduler{x: x, o: o, peers: map[[2]string]*peerState{}, rooms: map[string]*roomSched{}}
}

// Run pulls every room held until ctx ends: one loop per room, started
// when the room is held and stopped when it is left.
func (s *Scheduler) Run(ctx context.Context) {
	loops := map[string]context.CancelFunc{}
	var wg sync.WaitGroup
	defer func() {
		for _, stop := range loops {
			stop()
		}
		wg.Wait()
	}()
	for {
		wake := s.x.r.m.wakes.wait(roomSet)
		held := s.held()
		for _, room := range held {
			if loops[room] == nil {
				rctx, stop := context.WithCancel(ctx)
				loops[room] = stop
				wg.Add(1)
				go func() {
					defer wg.Done()
					s.room(rctx, room)
				}()
			}
		}
		for room, stop := range loops {
			if !slices.Contains(held, room) {
				stop()
				delete(loops, room)
			}
		}
		select {
		case <-wake:
		case <-ctx.Done():
			return
		}
	}
}

func (s *Scheduler) held() []string {
	r := s.x.r
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.rooms))
	for id := range r.rooms {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// room is one room's loop: the start pulls, then the two anchors.
func (s *Scheduler) room(ctx context.Context, room string) {
	s.start(ctx, room)
	var wg sync.WaitGroup
	for _, slot := range []int{slotStable, slotRotating} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.anchor(ctx, room, slot)
		}()
	}
	wg.Wait()
	s.mu.Lock()
	delete(s.rooms, room)
	for k := range s.peers {
		if k[0] == room {
			delete(s.peers, k)
		}
	}
	s.mu.Unlock()
}

// start is the short pulls of a room from two members, the most reliable
// first; a failure moves on to the next.
func (s *Scheduler) start(ctx context.Context, room string) {
	done := 0
	tried := map[string]bool{}
	for done < 2 && ctx.Err() == nil {
		var peer string
		for _, p := range s.eligible(room) {
			if !tried[p] {
				peer = p
				break
			}
		}
		if peer == "" {
			return
		}
		tried[peer] = true
		if _, err := s.x.Pull(ctx, room, peer, PullOpts{Offer: true, OfferLimit: s.offerOf(room, peer)}); err != nil {
			s.failed(room, peer, err)
			continue
		}
		s.succeeded(room, peer)
		done++
	}
}

// anchor is one anchor's loop of waiting pulls.
func (s *Scheduler) anchor(ctx context.Context, room string, slot int) {
	offer, probed := true, false
	cur := ""
	for ctx.Err() == nil {
		if _, err := s.x.r.Gen(room); err != nil {
			return // left: no more pulls of it
		}
		peer, idle := s.pick(room, slot)
		if peer == "" {
			s.sleep(ctx, room, idle)
			continue
		}
		if peer != cur {
			cur, probed = peer, slot == slotStable
		}
		wait := s.o.Wait
		if !probed {
			wait = s.o.Probe
		}
		stop := s.x.r.m.wakes.wait(room)
		st, err := s.x.Pull(ctx, room, peer, PullOpts{Offer: offer, OfferLimit: s.offerOf(room, peer),
			WaitMS: wait.Milliseconds(), Stop: stop})
		select {
		case <-stop:
			offer = true // changed while it ran: the next request offers
		default:
			offer = false
		}
		switch {
		case ctx.Err() != nil:
			return
		case err == nil:
			s.succeeded(room, peer)
			// An exchange that ended without a same did not wait; if it
			// also stored nothing (a lasting difference whose round was not
			// clean, so no seen), it is not repeated at once.
			s.spun(room, peer, st.Same == 0 && st.Stored+st.Repaired+st.Entries+st.Offered == 0)
			if slot == slotRotating {
				if probed {
					s.rotate(room) // the end of a wait
				}
				probed = true
			}
		case errors.Is(err, ErrWaitStopped):
			offer = true
			if slot == slotRotating {
				s.rotate(room) // a cancellation
			}
		case errors.Is(err, ErrStaleGeneration), errors.Is(err, ErrNoRoom), errors.Is(err, ErrPeerNotMember):
			// The room or its membership moved: pick again.
		default:
			s.failed(room, peer, err)
			offer = true
			if slot == slotRotating {
				s.rotate(room)
			}
		}
	}
}

// sleep waits d, or for a change to the room.
func (s *Scheduler) sleep(ctx context.Context, room string, d time.Duration) {
	wake := s.x.r.m.wakes.wait(room)
	select {
	case <-s.o.After(d):
	case <-wake:
	case <-ctx.Done():
	}
}

// eligible is the room's other members that may be pulled from now, most
// reliable first: fewest failures, then most successes, then node id.
func (s *Scheduler) eligible(room string) []string {
	members, heads := s.roomNow(room)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.eligibleLocked(room, members, heads)
}

// roomNow is the room's members and this machine's log heads of it.
func (s *Scheduler) roomNow(room string) (members, heads []string) {
	r := s.x.r
	r.mu.Lock()
	defer r.mu.Unlock()
	if rl := r.rooms[room]; rl != nil {
		members = slices.Clone(rl.st.Members)
		heads, _ = rl.logHeads()
	}
	return members, heads
}

// eligibleLocked is eligible with s.mu held.
func (s *Scheduler) eligibleLocked(room string, members, heads []string) []string {
	r := s.x.r
	now := s.o.Now()
	var out []string
	for _, p := range members {
		if p == r.m.ID {
			continue
		}
		ps := s.peers[[2]string{room, p}]
		if ps != nil {
			if ps.muted != nil {
				if sameSet(ps.muted, heads) {
					continue
				}
				ps.muted, ps.refused = nil, 0 // the log changed: try it again
			}
			if now.Before(ps.until) {
				continue
			}
		}
		out = append(out, p)
	}
	slices.SortStableFunc(out, func(a, b string) int {
		pa, pb := s.peers[[2]string{room, a}], s.peers[[2]string{room, b}]
		fa, fb, oa, ob := 0, 0, 0, 0
		if pa != nil {
			fa, oa = pa.fails, pa.ok
		}
		if pb != nil {
			fb, ob = pb.fails, pb.ok
		}
		switch {
		case fa != fb:
			return fa - fb
		case oa != ob:
			return ob - oa
		}
		return compareStr(a, b)
	})
	return out
}

// pick is the peer for slot to pull room from now, or none and how long
// to wait before looking again.
func (s *Scheduler) pick(room string, slot int) (string, time.Duration) {
	members, heads := s.roomNow(room)
	s.mu.Lock()
	defer s.mu.Unlock()
	// Chosen in the same critical section as the other anchor's choice
	// and every failure noted, so two anchors never take one peer and a
	// peer backing off is never taken.
	el := s.eligibleLocked(room, members, heads)
	rs := s.rooms[room]
	if rs == nil {
		rs = &roomSched{}
		s.rooms[room] = rs
	}
	var peer string
	switch slot {
	case slotStable:
		// The most reliable, unless the rotating anchor holds it: two
		// waiting pulls to one peer would release each other.
		for _, p := range el {
			if p != rs.cur[slotRotating] {
				peer = p
				break
			}
		}
	default:
		var rot []string
		for _, p := range el {
			if p != rs.cur[slotStable] {
				rot = append(rot, p)
			}
		}
		slices.Sort(rot)
		if len(rot) > 0 {
			peer = rot[rs.rot%len(rot)]
		}
	}
	rs.cur[slot] = peer
	if peer != "" {
		return peer, 0
	}
	// None now: until the first back-off ends, or a wait's length.
	idle := s.o.Wait
	now := s.o.Now()
	for k, ps := range s.peers {
		if k[0] == room && ps.until.After(now) && ps.until.Sub(now) < idle {
			idle = ps.until.Sub(now)
		}
	}
	return "", idle
}

func (s *Scheduler) rotate(room string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rs := s.rooms[room]; rs != nil {
		rs.rot++
	}
}

func (s *Scheduler) state(room, peer string) *peerState {
	k := [2]string{room, peer}
	ps := s.peers[k]
	if ps == nil {
		ps = &peerState{}
		s.peers[k] = ps
	}
	return ps
}

func (s *Scheduler) offerOf(room, peer string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ps := s.peers[[2]string{room, peer}]; ps != nil {
		return ps.offer
	}
	return 0
}

// spun counts an exchange that neither waited nor stored anything, and
// backs the next one off as a failure would; any other ends the count.
func (s *Scheduler) spun(room, peer string, spun bool) {
	now := s.o.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	ps := s.state(room, peer)
	if !spun {
		ps.spins = 0
		return
	}
	ps.spins++
	ps.until = now.Add(s.backoff(ps.spins))
}

func (s *Scheduler) succeeded(room, peer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps := s.state(room, peer)
	ps.fails, ps.until, ps.refused, ps.muted = 0, time.Time{}, 0, nil
	ps.ok++
}

// failed applies PROTOCOL-next 4.5 to a failed exchange with peer.
func (s *Scheduler) failed(room, peer string, err error) {
	var heads []string
	s.x.r.mu.Lock()
	if rl := s.x.r.rooms[room]; rl != nil {
		heads, _ = rl.logHeads()
	}
	s.x.r.mu.Unlock()
	now := s.o.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	ps := s.state(room, peer)
	var pe *PullError
	errors.As(err, &pe)
	switch {
	case pe != nil && pe.Status == 403:
		// The first: the exchange sends its whole log next time, at once.
		// The second running: none until this machine's log changes.
		if ps.refused++; ps.refused >= 2 {
			ps.muted = heads
			if ps.muted == nil {
				ps.muted = []string{}
			}
		}
		ps.fails++
		return
	case pe != nil && pe.Status == 413 && ps.offer != 1:
		if ps.offer == 0 {
			ps.offer = s.x.offerLimit
		}
		ps.offer = max(1, ps.offer/2) // and retry at once
		return
	case pe != nil && (pe.Status == 429 || pe.Status == 503):
		ps.fails++
		ps.until = now.Add(max(pe.RetryAfter, SchedBackoffMin))
		return
	}
	ps.fails++
	ps.until = now.Add(s.backoff(ps.fails))
}

// backoff is 1 s doubling to 5 min, ±20 %.
func (s *Scheduler) backoff(fails int) time.Duration {
	d := SchedBackoffMin
	for i := 1; i < fails && d < SchedBackoffMax; i++ {
		d *= 2
	}
	d = min(d, SchedBackoffMax)
	return time.Duration(float64(d) * (0.8 + 0.4*s.o.Jitter()))
}
