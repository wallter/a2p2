package node

// What the pull exchange keeps per room and peer, and how it is bounded
// (F3, F18, F20, F21, F22 of the 2026-11-25 review): nothing for a key
// that is not a member of the room here; for members, entries dropped
// once the peer is no longer one (tidy, from the scheduler); and when each
// member was last exchanged with, for the warning before its removal.

import (
	"context"
	"net/netip"
	"slices"
	"time"

	"github.com/wallter/a2p2/internal/store"
)

// isMember says whether peer is a member of room as its membership was
// last published (State), without the rooms lock. A stale answer only
// refuses a member admitted a moment ago (it asks again) or lets a key
// removed a moment ago through to the check under the lock.
func (r *Rooms) isMember(room, peer string) bool {
	st, err := r.State(room)
	return err == nil && slices.Contains(st.Members, peer)
}

type sourceKey struct{}

// WithSource returns ctx carrying the address a peer's request came from
// (the TCP peer), for Serve: a member that connected over loopback may
// give loopback addresses for itself (RemoteAddrOK).
func WithSource(ctx context.Context, remoteAddr string) context.Context {
	return context.WithValue(ctx, sourceKey{}, remoteAddr)
}

// sourceLoopback says whether ctx's request came over loopback.
func sourceLoopback(ctx context.Context) bool {
	s, _ := ctx.Value(sourceKey{}).(string)
	ap, err := netip.ParseAddrPort(s)
	return err == nil && ap.Addr().Unmap().IsLoopback()
}

// Requests with log entries from keys that are not members of the room,
// for the whole machine: they are verified under the rooms lock, and only
// a machine admitted by entries it carries itself sends one honestly.
const (
	strangerEntriesBurst = 8
	strangerEntriesEvery = 250 * time.Millisecond // 4 a second
)

// strangerEntries takes a token of that budget, or says how long to wait.
func (x *Exchange) strangerEntries() (bool, time.Duration) {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.strangers.take(x.r.m.now(), strangerEntriesBurst, strangerEntriesEvery)
}

// forget drops what the server side keeps of peer for room (its heads
// heard, the evidence sent it, its last page): peer is not a member there.
func (x *Exchange) forget(room, peer string) {
	key := [2]string{room, peer}
	x.mu.Lock()
	defer x.mu.Unlock()
	delete(x.heard, key)
	delete(x.evidence, key)
	delete(x.pages, key)
}

// ReachedFor is how recent an exchange with a member must be for it to
// count as reached, for the warning before a removal (OnlyReached).
const ReachedFor = time.Hour

// exchanged records an exchange with peer for room that ended without an
// error, either way: this machine's pull, or a request it answered.
func (x *Exchange) exchanged(room, peer string) {
	now := x.r.m.now()
	x.mu.Lock()
	defer x.mu.Unlock()
	x.lastEx[[2]string{room, peer}] = now
}

// exchangedIn is, per peer, when room was last exchanged with it in this
// run (in memory).
func (x *Exchange) exchangedIn(room string) map[string]time.Time {
	x.mu.Lock()
	defer x.mu.Unlock()
	out := map[string]time.Time{}
	for k, at := range x.lastEx {
		if k[0] == room {
			out[k[1]] = at
		}
	}
	return out
}

// OnlyReached says whether node is the one member of room other than
// this machine that it exchanged with within ReachedFor: removing it may
// leave this machine reaching no member of the room, and the removal
// reaching no one else (F21). False when the exchange is not running.
func (r *Rooms) OnlyReached(room, node string) bool {
	if r.reached == nil {
		return false
	}
	st, err := r.State(room)
	if err != nil || !slices.Contains(st.Members, node) {
		return false
	}
	now := r.m.now()
	var recent []string
	for p, at := range r.reached(room) {
		if p != r.m.ID && slices.Contains(st.Members, p) && now.Sub(at) < ReachedFor && !at.After(now) {
			recent = append(recent, p)
		}
	}
	return len(recent) == 1 && recent[0] == node
}

// tidyEvery is how often the scheduler drops what is kept of peers that
// are no longer members (tidy), besides when the rooms held change.
const tidyEvery = 10 * time.Minute

// tidy drops what the exchange keeps, in memory and in the store, of
// peers that are not members of a room held here: the per-peer maps, and
// the peer's observed reports and the stream hints only its reports keep
// (F9); a room no longer held loses all of it. Hints of a member not
// observed within hintLife go too: its next report sets them again.
func (x *Exchange) tidy(ctx context.Context) {
	r := x.r
	r.mu.Lock()
	members := map[string][]string{}
	gens := map[string]int64{}
	for id, rl := range r.rooms {
		members[id], gens[id] = slices.Clone(rl.st.Members), rl.gen
	}
	r.mu.Unlock()
	keep := func(k [2]string) bool {
		ms, held := members[k[0]]
		return held && slices.Contains(ms, k[1])
	}
	x.mu.Lock()
	for k := range x.heard {
		if !keep(k) {
			delete(x.heard, k)
		}
	}
	for k := range x.evidence {
		if !keep(k) {
			delete(x.evidence, k)
		}
	}
	for k := range x.pages {
		if !keep(k) {
			delete(x.pages, k)
		}
	}
	for k := range x.refused {
		if !keep(k) {
			delete(x.refused, k)
		}
	}
	for k := range x.seen {
		if !keep(k) {
			delete(x.seen, k)
		}
	}
	for k := range x.resume {
		if !keep(k) {
			delete(x.resume, k)
		}
	}
	for k := range x.carries {
		if !keep(k) {
			delete(x.carries, k)
		}
	}
	for k := range x.asked {
		if !keep(k) {
			delete(x.asked, k)
		}
	}
	for k := range x.lastEx {
		if !keep(k) {
			delete(x.lastEx, k)
		}
	}
	for k := range x.latest {
		if _, held := members[k[0]]; !held {
			delete(x.latest, k) // the order of observations matters within a generation only
		}
	}
	x.mu.Unlock()
	now := r.m.now()
	for _, room := range sortedIDs(members) {
		// One transaction per room, under the rooms lock as every write
		// of observations is, so a peer admitted meanwhile is not touched.
		r.mu.Lock()
		rl := r.rooms[room]
		if rl != nil && rl.gen == gens[room] {
			_, _ = r.m.st.Exchange(ctx, room, rl.gen, func(tx *store.Tx) error {
				return tx.DropReports(room, rl.st.Members, now.Add(-hintLife))
			})
		}
		r.mu.Unlock()
	}
}

// hintLife is how long the stream hints of a member not observed since
// are kept.
const hintLife = 24 * time.Hour
