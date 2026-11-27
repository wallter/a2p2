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

// memberOf says whether room is held here and peer is a member of it, as
// its membership was last published (State), without the rooms lock. A
// stale answer only refuses a member admitted a moment ago (it asks
// again) or lets a key removed a moment ago through to the check under
// the lock.
func (r *Rooms) memberOf(room, peer string) (held, member bool) {
	st, err := r.State(room)
	if err != nil {
		return false, false
	}
	return true, slices.Contains(st.Members, peer)
}

type sourceKey struct{}

// WithSource returns ctx carrying the address a peer's request came from
// (the TCP peer), for Serve: requests with log entries from keys that are
// not members are shared out by source (strangerEntries). It grants
// nothing: behind a tunnel or proxy on this machine every source is
// loopback.
func WithSource(ctx context.Context, remoteAddr string) context.Context {
	return context.WithValue(ctx, sourceKey{}, remoteAddr)
}

// sourceGroup is the group of ctx's source address: an IPv4 address or
// an IPv6 /64, as the listener counts connections; "" when unknown.
func sourceGroup(ctx context.Context) string {
	s, _ := ctx.Value(sourceKey{}).(string)
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		return ""
	}
	ip := ap.Addr().Unmap()
	if ip.Is6() {
		p, _ := ip.Prefix(64)
		return p.String()
	}
	return ip.String()
}

// Requests with log entries for a room held here from keys that are not
// members of it: they are verified under the rooms lock, and only a
// machine admitted by entries it carries itself sends one honestly. For
// the whole machine, a token bucket (8 at once after a quiet spell, 4 a
// second sustained) and at most 2 served at a time; each source group
// (an IPv4 address or IPv6 /64, as the listener counts connections) has a
// share of it (2 at once, one a second), so a hostile host takes at most
// that share from an honest joiner at another address. Past it: 429 with
// Retry-After.
const (
	strangerEntriesBurst    = 8
	strangerEntriesEvery    = 250 * time.Millisecond // 4 a second
	strangerEntriesInFlight = 2
	strangerSourceBurst     = 2
	strangerSourceEvery     = time.Second
	maxStrangerSources      = 1024
)

// strangerEntries admits one such request from source, returning the
// function to call when it is served; nil and how long to wait when it is
// not admitted. A refusal spends nothing.
func (x *Exchange) strangerEntries(source string) (done func(), wait time.Duration) {
	now := x.r.m.now()
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.strangerBusy >= strangerEntriesInFlight {
		return nil, strangerEntriesEvery
	}
	src := x.strangerBy[source]
	if src == nil {
		if len(x.strangerBy) >= maxStrangerSources {
			for k, t := range x.strangerBy { // forget the sources whose share is full again
				if t.full(now, strangerSourceBurst, strangerSourceEvery) {
					delete(x.strangerBy, k)
				}
			}
		}
		if len(x.strangerBy) >= maxStrangerSources {
			source = "" // the rest share one
		}
		if src = x.strangerBy[source]; src == nil {
			src = &tokens{}
			x.strangerBy[source] = src
		}
	}
	if w := src.wait(now, strangerSourceBurst, strangerSourceEvery); w > 0 {
		return nil, w
	}
	if w := x.strangers.wait(now, strangerEntriesBurst, strangerEntriesEvery); w > 0 {
		return nil, w
	}
	src.take(now, strangerSourceBurst, strangerSourceEvery)
	x.strangers.take(now, strangerEntriesBurst, strangerEntriesEvery)
	x.strangerBusy++
	return func() {
		x.mu.Lock()
		x.strangerBusy--
		x.mu.Unlock()
	}, 0
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
