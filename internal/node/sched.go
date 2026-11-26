package node

// The scheduler of the pull exchange: when to pull and from whom
// (DESIGN section 4, the trigger table; PROTOCOL 4.5 and 4.6).
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
//     until this machine's log of the room changes or a request from the
//     peer, a current member, shows changed log heads (the Exchange keeps
//     that state; refusalOf); 413 halves the offer; 429 and 503 wait their
//     Retry-After.
//   - Spins: an exchange that stored nothing here and did not wait (a same
//     well before its wait, or a difference whose round was not clean) is
//     backed off as a failure; against a peer that answers at once, after
//     1 s doubling to 5 min, one exchange per room per 4 to 6 min. A wait
//     held at least holdProof (twice the round trip, at most 1 s) and then
//     answered same or stopped by a change ends the count (held).
//   - Offers: an exchange that offered room changes, ended clean, and from
//     whose answers the peer's progress is inferred (PullStats.Given) is
//     excused (excused). Its closing request waits holdProof; a peer that
//     holds it has held a wait. Otherwise, after an earlier held wait the
//     exchange is not a spin and the next may follow at once, a further
//     one before another held wait waits SchedPace, and with no held wait
//     since the last spin it is a spin. That is how a machine that only
//     connects out delivers its writes, at once to a peer that holds
//     waits, and to one that does not, no faster than the back-off or one
//     exchange per SchedPace.
//
// Clock, timers, jitter and transport are injected, so tests are
// deterministic. Peers' addresses are the Exchange's AddrBook; Run keeps
// it to the members of the rooms held. The daemon starts one Scheduler
// (internal/daemon) unless networking is off.

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"sync"
	"time"
)

// Timing of DESIGN 4 and PROTOCOL 4.6.
const (
	SchedWait       = 60 * time.Second // a waiting pull
	SchedProbe      = 10 * time.Second // the probe of a newly chosen rotating anchor
	SchedBackoffMin = time.Second
	SchedBackoffMax = 5 * time.Minute
	SchedPace       = time.Second // between excused exchanges with no hold between them (excused)
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

	// Test hooks: beforeLook runs before Run looks at the rooms held;
	// beforePull after an anchor has picked its peer, before it pulls;
	// loopDone when a room's loop has ended.
	beforeLook func()
	beforePull func(room string, slot int)
	loopDone   func(room string)
}

// peerState is what the scheduler knows of one peer for one room.
type peerState struct {
	fails int       // failures since the last success
	ok    int       // successes
	until time.Time // not before
	offer int       // offer limit after a 413; 0: the exchange's
	spins int       // waiting pulls running that neither waited nor stored anything
	// held: since its last spin the peer held a waiting pull of this
	// machine's longer than an answer takes (holdProof); paced: an
	// exchange was excused since that hold (excused).
	held  bool
	paced bool
	rtt   time.Duration // per request, of its last exchange answered early
	timed bool          // rtt was measured
}

// holdProof is how long a waiting pull must have lasted to show the peer
// held it rather than answered it: more than twice the round trip last
// measured, at most 1 s, and 1 s before one is measured.
func (ps *peerState) holdProof() time.Duration {
	if !ps.timed {
		return SchedBackoffMin
	}
	return min(2*ps.rtt, SchedBackoffMin)
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

// Run pulls every room held until ctx ends: one loop per room and
// generation, started when the room is held and stopped when it is left
// or joined again; a loop of an earlier generation has ended before the
// loop of the next starts. Its context reaches every request of every
// exchange the loop runs.
func (s *Scheduler) Run(ctx context.Context) {
	type loop struct {
		gen  int64
		stop context.CancelFunc
		done chan struct{}
	}
	loops := map[string]*loop{}
	defer func() {
		for _, l := range loops {
			l.stop()
		}
		for _, l := range loops {
			<-l.done
		}
	}()
	for {
		wake := s.x.r.m.wakes.wait(roomSet)
		if s.beforeLook != nil {
			s.beforeLook()
		}
		held := s.held()
		for room, l := range loops {
			if g, ok := held[room]; !ok || g != l.gen {
				l.stop()
				<-l.done
				delete(loops, room)
			}
		}
		for room, gen := range held {
			if l := loops[room]; l != nil {
				select {
				case <-l.done: // ended by itself (the room moved on): replaced
					delete(loops, room)
				default:
					continue
				}
			}
			rctx, stop := context.WithCancel(ctx)
			l := &loop{gen: gen, stop: stop, done: make(chan struct{})}
			loops[room] = l
			go func() {
				defer close(l.done)
				s.room(rctx, room, gen)
			}()
		}
		s.x.keepBook()
		select {
		case <-wake:
		case <-ctx.Done():
			return
		}
	}
}

// held is the rooms held and their generations.
func (s *Scheduler) held() map[string]int64 {
	r := s.x.r
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int64, len(r.rooms))
	for id, rl := range r.rooms {
		out[id] = rl.gen
	}
	return out
}

// room is one room's loop: the start pulls, then the two anchors.
func (s *Scheduler) room(ctx context.Context, room string, gen int64) {
	s.start(ctx, room, gen)
	var wg sync.WaitGroup
	for _, slot := range []int{slotStable, slotRotating} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.anchor(ctx, room, gen, slot)
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
	if s.loopDone != nil {
		s.loopDone(room)
	}
}

// start is the short pulls of a room from two members, the most reliable
// first; a failure moves on to the next.
func (s *Scheduler) start(ctx context.Context, room string, gen int64) {
	done := 0
	tried := map[string]bool{}
	for done < 2 && ctx.Err() == nil {
		if g, err := s.x.r.Gen(room); err != nil || g != gen {
			return
		}
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
		began := s.o.Now()
		wake, _ := s.x.r.m.wakes.sub(room) // a change ends it early (Pull, Superseded); no wait to stop
		st, err := s.x.Pull(ctx, room, peer, PullOpts{Offer: true, OfferLimit: s.offerOf(room, peer), Stop: wake})
		if err != nil {
			s.failed(room, peer, err)
			continue
		}
		if st.Requests > 0 {
			s.timed(room, peer, s.o.Now().Sub(began)/time.Duration(st.Requests))
		}
		s.succeeded(room, peer)
		done++
	}
}

// anchor is one anchor's loop of waiting pulls, for generation gen of
// the room.
func (s *Scheduler) anchor(ctx context.Context, room string, gen int64, slot int) {
	// offered is the count of the room's touches (writes) covered by the
	// last exchange that could carry an offer and ran; until one has run
	// for every touch, the next request offers.
	var offered uint64
	first, probed := true, false
	cur := ""
	for ctx.Err() == nil {
		// Taken before anything is looked at: a write after this point
		// both wakes the wait below and shows in seq.
		wake, seq := s.x.r.m.wakes.sub(room)
		if g, err := s.x.r.Gen(room); err != nil || g != gen {
			return // left, or joined again (a new loop): no more pulls of this generation
		}
		peer, idle := s.pick(room, slot)
		if peer == "" {
			s.sleep(ctx, wake, idle)
			continue
		}
		if s.beforePull != nil {
			s.beforePull(room, slot)
		}
		if peer != cur {
			cur, probed = peer, slot == slotStable
		}
		wait := s.o.Wait
		if !probed {
			wait = s.o.Probe
		}
		offer := first || seq != offered
		began := s.o.Now()
		// A change stops the wait only once it has lasted holdProof, when
		// an answer would be back: a request with an offer abandoned in
		// flight, again at every change, would never deliver it, and a
		// wait stopped then shows the peer held it (heldFor).
		pctx, done := context.WithCancel(ctx)
		proof := s.proofOf(room, peer)
		stop := s.stopAfter(pctx, wake, began, proof)
		var confirm int64 // the closing request of an offer exchange waits as long as a hold must last
		if offer {
			confirm = max(proof.Milliseconds(), 1)
		}
		st, err := s.x.Pull(pctx, room, peer, PullOpts{Offer: offer, OfferLimit: s.offerOf(room, peer),
			WaitMS: wait.Milliseconds(), ConfirmWaitMS: confirm, Stop: stop})
		done()
		switch {
		case ctx.Err() != nil:
			return
		case err == nil:
			if offer {
				first, offered = false, seq
			}
			s.succeeded(room, peer)
			// Progress is what was stored here. An exchange that stored
			// nothing and did not wait (a same well before its wait, or a
			// lasting difference whose round was not clean) is backed off
			// as a failure: a spin. An exchange that offered new room
			// changes, ended clean (same or superseded, not capped, nothing
			// stalled or too large), and from whose responses the peer's
			// progress is inferred (st.Given) goes to excused instead,
			// which bounds it. That is how a machine that only connects
			// out delivers its writes.
			elapsed := s.o.Now().Sub(began)
			stored := st.Stored+st.Repaired+st.Entries > 0
			early := elapsed < wait/2
			clean := (st.Same > 0 || st.Superseded) && !st.Capped && st.Stalled == 0 && st.TooLarge == 0
			// The round trip per request, leaving out a closing request
			// that carried a wait (it may have been held).
			if n := st.Requests; st.ConfirmHeld > 0 && n > 1 && early {
				s.timed(room, peer, (elapsed-st.ConfirmHeld)/time.Duration(n-1))
			} else if early && n > 0 && st.ConfirmHeld == 0 {
				s.timed(room, peer, elapsed/time.Duration(n))
			}
			switch {
			case !stored && offer && st.Given > 0 && clean:
				s.excused(room, peer, st.ConfirmHeld > 0 && st.ConfirmHeld >= proof)
			default:
				s.spun(room, peer, !stored && (st.Same == 0 || early))
				if !early && st.Same > 0 {
					s.heldFor(room, peer, elapsed)
				}
			}
			if slot == slotRotating {
				if probed {
					s.rotate(room) // the end of a wait
				}
				probed = true
			}
		case errors.Is(err, ErrWaitStopped):
			s.heldFor(room, peer, s.o.Now().Sub(began))
			if slot == slotRotating {
				s.rotate(room) // a cancellation
			}
		case errors.Is(err, ErrStaleGeneration), errors.Is(err, ErrNoRoom), errors.Is(err, ErrPeerNotMember):
			// The room or its membership moved: look again.
		default:
			s.failed(room, peer, err)
			if slot == slotRotating {
				s.rotate(room)
			}
		}
	}
}

// sleep waits d, or for wake (the subscription taken before the look).
func (s *Scheduler) sleep(ctx context.Context, wake <-chan struct{}, d time.Duration) {
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
		if s.x.muted(room, p, heads) {
			continue // refused twice; until either side's log heads change
		}
		if ps := s.peers[[2]string{room, p}]; ps != nil && now.Before(ps.until) {
			continue
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
	ps.held = false
	ps.until = now.Add(s.backoff(ps.spins))
}

// excused notes an exchange that offered new room changes and from which
// the peer's progress was inferred (anchor). Its closing request waited
// as long as a hold must last (PullOpts.ConfirmWaitMS); if the peer held
// it (confirmed), that is a held wait: not a spin, and the count ends.
// Otherwise, with a peer that has held a wait since its last spin (held),
// the first such exchange is not a spin and may be followed at once, and
// each further one before the peer holds a wait again puts the next
// SchedPace later. Without such a hold it counts as a spin and backs off.
// So a peer that answers every wait at once gets no more exchanges than
// the back-off allows however often the room changes, one that held a
// wait before, one exchange per SchedPace while the room keeps changing,
// and one that holds waits is never held back.
func (s *Scheduler) excused(room, peer string, confirmed bool) {
	now := s.o.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	ps := s.state(room, peer)
	switch {
	case confirmed:
		ps.held, ps.paced, ps.spins = true, false, 0
	case ps.held && !ps.paced:
		ps.paced = true
	case ps.held:
		ps.until = now.Add(time.Duration(float64(SchedPace) * (0.8 + 0.4*s.o.Jitter())))
	default:
		ps.spins++
		ps.until = now.Add(s.backoff(ps.spins))
	}
}

// heldFor notes a waiting pull to peer that lasted d, answered same or
// stopped: as long as holdProof shows the peer held it, which ends the
// count of spins.
func (s *Scheduler) heldFor(room, peer string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ps := s.state(room, peer); d >= ps.holdProof() {
		ps.held, ps.paced, ps.spins = true, false, 0
	}
}

// proofOf is peer's holdProof.
func (s *Scheduler) proofOf(room, peer string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state(room, peer).holdProof()
}

// stopAfter is a channel closed when wake is and at least d has passed
// since began, or never if ctx ends first.
func (s *Scheduler) stopAfter(ctx context.Context, wake <-chan struct{}, began time.Time, d time.Duration) <-chan struct{} {
	c := make(chan struct{})
	go func() {
		select {
		case <-wake:
		case <-ctx.Done():
			return
		}
		if rest := began.Add(d).Sub(s.o.Now()); rest > 0 {
			select {
			case <-s.o.After(rest):
			case <-ctx.Done():
				return
			}
		}
		close(c)
	}()
	return c
}

// timed notes the round trip per request of an exchange answered early.
func (s *Scheduler) timed(room, peer string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps := s.state(room, peer)
	ps.rtt, ps.timed = d, true
}

func (s *Scheduler) succeeded(room, peer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps := s.state(room, peer)
	ps.fails, ps.until = 0, time.Time{}
	ps.ok++
}

// failed applies PROTOCOL 4.5 to a failed exchange with peer.
func (s *Scheduler) failed(room, peer string, err error) {
	now := s.o.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	ps := s.state(room, peer)
	var pe *PullError
	errors.As(err, &pe)
	switch {
	case pe != nil && pe.Status == 403:
		// The first: the exchange sends its whole log next time, at once.
		// The second running mutes the peer (Exchange.answered; eligible).
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
