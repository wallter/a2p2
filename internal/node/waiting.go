package node

// Waiting pulls (PROTOCOL 4.4 steps 9 and 10, 4.6): an opening
// request with wait_ms whose answer is same is held until the answer would
// no longer be same (this machine's sum for the room, or its log, changed)
// or wait_ms passes, then released through the release point (Exchange.
// release: generation, membership, rebuild). One waiting pull per room per
// peer: a new one releases the old. Waiting holds neither the rooms lock
// nor a database transaction.

import (
	"context"
	"slices"
	"sync/atomic"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

// hold waits for b, a same answer to q from peer, to stop being same. It
// returns the answer to release: b, or one built again after a change.
func (x *Exchange) hold(ctx context.Context, peer string, q *PullRequest, b *built) (*built, error) {
	key := [2]string{q.Room, peer}
	mine := make(chan struct{})
	x.mu.Lock()
	if x.isClosed() {
		x.mu.Unlock()
		return nil, pullErr(503, CodeUnavailable, "closing")
	}
	if old := x.waiting[key]; old != nil {
		close(old) // replaced: the older one is released now
	}
	x.waiting[key] = mine
	notify := x.nextHold[key]
	delete(x.nextHold, key)
	x.mu.Unlock()
	if f, _ := ctx.Value(holdNoticeKey{}).(func()); f != nil {
		f() // the HTTP layer gives up this request's concurrency slot
	}
	// NextHold's watchers learn of the hold only once all of it is done,
	// the slot given up included.
	for _, c := range notify {
		close(c)
	}
	defer func() {
		x.mu.Lock()
		if x.waiting[key] == mine {
			delete(x.waiting, key)
		}
		x.mu.Unlock()
	}()
	timeout := x.after(time.Duration(q.WaitMS) * time.Millisecond)
	for {
		// Taken before the look, so a change committed after it wakes.
		wake := x.r.m.wakes.wait(q.Room)
		nb, still, err := x.stillSame(ctx, peer, q, b)
		if err != nil {
			return nil, err
		}
		b = nb
		if !still {
			return b, nil
		}
		select {
		case <-wake:
		case <-timeout:
			return b, nil
		case <-mine:
			return b, nil
		case <-x.closed:
			return nil, pullErr(503, CodeUnavailable, "closing")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

type holdNoticeKey struct{}

// WithHoldNotice returns ctx carrying f, which Serve calls once if the
// request it serves enters a hold (a waiting pull held at this server):
// the HTTP layer counts a request against its concurrency limit until
// then only.
func WithHoldNotice(ctx context.Context, f func()) context.Context {
	return context.WithValue(ctx, holdNoticeKey{}, f)
}

// NextHold returns a channel closed when the next hold of a request from
// peer for room is registered and its concurrency slot given up: what a
// test waits on instead of sleeping.
func (x *Exchange) NextHold(room, peer string) <-chan struct{} {
	c := make(chan struct{})
	x.mu.Lock()
	defer x.mu.Unlock()
	key := [2]string{room, peer}
	x.nextHold[key] = append(x.nextHold[key], c)
	return c
}

// Close releases every held request at once, refused 503 unavailable,
// and refuses every request after it the same way. The daemon calls it
// when it begins to shut down, before it waits for the listener's
// requests to end (a held request would otherwise last its whole wait).
func (x *Exchange) Close() {
	x.closing.Do(func() { close(x.closed) })
}

func (x *Exchange) isClosed() bool {
	select {
	case <-x.closed:
		return true
	default:
		return false
	}
}

// stillSame reports whether the answer to q is still same, building it
// again if anything was written since b was built. A room left or joined
// again, or a peer no longer a member, ends the wait: release refuses it.
func (x *Exchange) stillSame(ctx context.Context, peer string, q *PullRequest, b *built) (*built, bool, error) {
	r := x.r
	r.mu.Lock()
	defer r.mu.Unlock()
	rl := r.rooms[q.Room]
	if rl == nil || rl != b.rl || rl.gen != b.gen || !slices.Contains(rl.st.Members, peer) {
		return b, false, nil
	}
	if r.m.st.Changes() == b.ver {
		return b, true, nil
	}
	nb, err := x.respond(ctx, rl, peer, q, served{stored: b.sv.stored})
	if err != nil {
		return nil, false, unavailable(err)
	}
	// A want for the puller ends the hold: release adds it (PROTOCOL 10.4).
	return nb, nb.resp.Same && (x.files == nil || !x.files.wantsDue(ctx, q.Room, peer)), nil
}

// pullStoppable sends a waiting request and abandons it if stop closes
// first.
func (x *Exchange) pullStoppable(ctx context.Context, peer string, q *PullRequest, stop <-chan struct{}) (*PullResponse, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stopped atomic.Bool
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-stop:
			stopped.Store(true)
			cancel()
		case <-done:
		}
	}()
	resp, err := x.t.Pull(ctx, peer, q)
	if err != nil && stopped.Load() {
		return nil, ErrWaitStopped
	}
	return resp, err
}

// learnPeers takes the addresses a response lists for members of room
// (other than this machine and the peer that answered, whose own word in
// its requests counts instead) into the book.
func (x *Exchange) learnPeers(room, from string, ps []PullPeer) {
	if x.book == nil || len(ps) == 0 {
		return
	}
	x.r.mu.Lock()
	var members []string
	if rl := x.r.rooms[room]; rl != nil {
		members = rl.st.Members
	}
	x.r.mu.Unlock()
	now := x.r.m.now()
	for _, p := range ps {
		if p.Node == x.r.m.ID || p.Node == from || !slices.Contains(members, p.Node) {
			continue
		}
		seen, _ := peerSeen(p.Seen)
		if seen.After(now) {
			seen = now
		}
		x.book.Learn(p.Node, p.Addrs, seen)
	}
}

// askPeersEvery bounds how often one peer is asked for the room's peers
// while this machine has an address for every current member.
const askPeersEvery = time.Hour

// wantPeers says whether an opening request to peer for rl asks for the
// room's peers: whenever this machine has no address for some other
// current member, and otherwise at most once an hour per peer. A member
// that does not listen never has an address, so a room with one is asked
// about on every opening request: the cost is the bytes of the list
// (at most MaxPeersListed entries). r.mu is held.
func (x *Exchange) wantPeers(rl *roomLog, peer string, now time.Time) bool {
	if x.book == nil {
		return false
	}
	missing := false
	for _, n := range rl.st.Members {
		if n != x.r.m.ID && n != peer && len(x.book.Addrs(n)) == 0 {
			missing = true
		}
	}
	key := [2]string{rl.id, peer}
	x.mu.Lock()
	defer x.mu.Unlock()
	last, ok := x.asked[key]
	if !missing && ok && now.Sub(last) < askPeersEvery && !now.Before(last) {
		return false
	}
	x.asked[key] = now
	return true
}

// validPeer checks one entry of a response's peers (PROTOCOL 4.2):
// a node id, 1 to MaxAddrs addresses of 1 to MaxAddrBytes bytes, and a
// time of section 0 if seen is there. A malformed one refuses the
// response.
func validPeer(p PullPeer) bool {
	if !core.ValidNode(p.Node) || len(p.Addrs) == 0 || len(p.Addrs) > MaxAddrs {
		return false
	}
	for _, a := range p.Addrs {
		if a == "" || len(a) > MaxAddrBytes {
			return false
		}
	}
	_, ok := peerSeen(p.Seen)
	return ok
}

// peerSeen parses a peer's seen; empty is the zero time.
func peerSeen(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, true
	}
	t, err := time.Parse(timeFormat, s)
	return t, err == nil && t.UTC().Format(timeFormat) == s
}
