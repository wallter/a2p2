package node

// Waiting pulls (PROTOCOL-next 4.4 steps 9 and 10, 4.6): an opening
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
	if old := x.waiting[key]; old != nil {
		close(old) // replaced: the older one is released now
	}
	x.waiting[key] = mine
	x.mu.Unlock()
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
		case <-ctx.Done():
			return nil, ctx.Err()
		}
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
	return nb, nb.resp.Same, nil
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
		if !core.ValidNode(p.Node) || p.Node == x.r.m.ID || p.Node == from || !slices.Contains(members, p.Node) ||
			len(p.Addrs) > MaxAddrs {
			continue
		}
		var seen time.Time
		if t, err := time.Parse(timeFormat, p.Seen); err == nil && len(p.Seen) == len(timeFormat) && !t.After(now) {
			seen = t
		}
		x.book.Learn(p.Node, p.Addrs, seen)
	}
}
