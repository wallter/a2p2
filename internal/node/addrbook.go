package node

// Peers' addresses (PROTOCOL section 0: "addresses are hints, learned
// from the token, a peer's addrs, and peers in responses, stored per member
// and tried most recent success first"). In memory, and with
// LoadAddrBook also in the store's peer_addrs (migration 8), so a machine
// that does not listen can reach its peers after a restart.
//
// Per node, at most MaxAddrs. An address the node gave for itself in its
// own authenticated request takes a slot before one a third party listed,
// and third parties never push one out. An address that completed a
// pinned handshake is good and goes first; one that fails goes after the
// ones that do not; one that has failed for 24 hours over at least 5
// attempts is dropped. Only nodes that are members of a room this machine
// is in are kept (Keep).

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/wallter/a2p2/internal/store"
)

// maxBookNodes bounds the book; beyond it the node heard of longest ago
// is dropped.
const maxBookNodes = 4096

// Address ageing.
const (
	addrDropAfter = 24 * time.Hour
	addrDropFails = 5
)

// AddrBook is where each node can be reached. Safe for concurrent use;
// an address is never a credential, every connection is pinned to the
// node id.
type AddrBook struct {
	mu sync.Mutex
	m  map[string]*bookEntry
	st *store.Store // nil: in memory only
}

type bookEntry struct {
	addrs []store.PeerAddr // in the order they are tried
	seen  time.Time        // when the node was last heard from
}

// DialAddr is one address to try, and whether it ever completed a pinned
// handshake.
type DialAddr struct {
	Addr string
	Good bool
}

// NewAddrBook returns an empty book kept in memory only.
func NewAddrBook() *AddrBook { return &AddrBook{m: map[string]*bookEntry{}} }

// LoadAddrBook returns the book stored in st, which it keeps up to date.
func LoadAddrBook(ctx context.Context, st *store.Store) (*AddrBook, error) {
	rows, err := st.PeerAddrs(ctx)
	if err != nil {
		return nil, err
	}
	b := &AddrBook{m: map[string]*bookEntry{}, st: st}
	for _, a := range rows {
		e := b.m[a.Node]
		if e == nil {
			e = &bookEntry{}
			b.m[a.Node] = e
		}
		e.addrs = append(e.addrs, a)
		if a.Seen.After(e.seen) {
			e.seen = a.Seen
		}
	}
	for _, e := range b.m {
		order(e.addrs)
	}
	return b, nil
}

// Keep drops every node for which member is false (not a member of a
// room this machine is in). Addresses are only ever added for members
// (Exchange.serve, learnPeers); the scheduler calls this as rooms come
// and go.
func (b *AddrBook) Keep(member func(node string) bool) {
	b.mu.Lock()
	var gone []string
	for n := range b.m {
		if !member(n) {
			delete(b.m, n)
			gone = append(gone, n)
		}
	}
	b.mu.Unlock()
	for _, n := range gone {
		b.save(n, nil)
	}
}

// order sorts addresses for dialling: not failing before failing, good
// before not, the node's own before third parties', otherwise as they
// were (the most recent success first).
func order(as []store.PeerAddr) {
	rank := func(a store.PeerAddr) int {
		r := 0
		if a.Fails > 0 {
			r += 4
		}
		if !a.Good {
			r += 2
		}
		if a.Source != store.AddrSelf {
			r++
		}
		return r
	}
	slices.SortStableFunc(as, func(x, y store.PeerAddr) int { return rank(x) - rank(y) })
}

// Learn adds addresses a third party listed for node (peers in a
// response): only into free slots.
func (b *AddrBook) Learn(node string, addrs []string, seen time.Time) {
	b.add(node, addrs, store.AddrPeer, seen)
}

// Replace sets the addresses node gave for itself in its own
// authenticated request: what it no longer lists of its own is dropped,
// and its own take slots before third parties'.
func (b *AddrBook) Replace(node string, addrs []string, seen time.Time) {
	b.add(node, addrs, store.AddrSelf, seen)
}

func (b *AddrBook) add(node string, addrs []string, source string, seen time.Time) {
	if len(addrs) == 0 {
		return
	}
	b.mu.Lock()
	e := b.m[node]
	if e == nil {
		if len(b.m) >= maxBookNodes {
			b.evict()
		}
		e = &bookEntry{}
		b.m[node] = e
	}
	before := slices.Clone(e.addrs)
	if source == store.AddrSelf {
		// Its own word replaces its own earlier word.
		e.addrs = slices.DeleteFunc(e.addrs, func(a store.PeerAddr) bool {
			return a.Source == store.AddrSelf && !slices.Contains(addrs, a.Addr)
		})
	}
	for _, a := range addrs {
		if a == "" || len(a) > MaxAddrBytes {
			continue
		}
		if i := slices.IndexFunc(e.addrs, func(p store.PeerAddr) bool { return p.Addr == a }); i >= 0 {
			if source == store.AddrSelf {
				e.addrs[i].Source = store.AddrSelf
			}
			if seen.After(e.addrs[i].Seen) {
				e.addrs[i].Seen = seen
			}
			continue
		}
		pa := store.PeerAddr{Node: node, Addr: a, Source: source, Seen: seen}
		if len(e.addrs) < MaxAddrs {
			e.addrs = append(e.addrs, pa)
			continue
		}
		if source != store.AddrSelf {
			continue // third parties fill free slots only
		}
		// Its own word pushes out the last third-party address.
		for i := len(e.addrs) - 1; i >= 0; i-- {
			if e.addrs[i].Source != store.AddrSelf {
				e.addrs[i] = pa
				break
			}
		}
	}
	order(e.addrs)
	if seen.After(e.seen) {
		e.seen = seen
	}
	changed := !slices.EqualFunc(before, e.addrs, func(x, y store.PeerAddr) bool {
		return x.Addr == y.Addr && x.Source == y.Source
	})
	rows := slices.Clone(e.addrs)
	b.mu.Unlock()
	if changed {
		b.save(node, rows)
	}
}

func (b *AddrBook) evict() {
	var oldest string
	for n, e := range b.m {
		if oldest == "" || e.seen.Before(b.m[oldest].seen) {
			oldest = n
		}
	}
	delete(b.m, oldest)
}

// save writes node's addresses through to the store, if there is one.
// A failed write leaves the copy in memory as it is: the next change
// writes again.
func (b *AddrBook) save(node string, rows []store.PeerAddr) {
	if b.st != nil {
		_ = b.st.SetPeerAddrs(context.Background(), node, rows)
	}
}

// Addrs is node's addresses in the order they are tried.
func (b *AddrBook) Addrs(node string) []string {
	var out []string
	for _, d := range b.Dial(node) {
		out = append(out, d.Addr)
	}
	return out
}

// Dial is node's addresses in the order they are tried, each with
// whether it ever completed a pinned handshake.
func (b *AddrBook) Dial(node string) []DialAddr {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.m[node]
	if e == nil {
		return nil
	}
	out := make([]DialAddr, 0, len(e.addrs))
	for _, a := range e.addrs {
		out = append(out, DialAddr{Addr: a.Addr, Good: a.Good})
	}
	return out
}

// Worked records that addr completed a pinned handshake with node: good,
// no failures, first.
func (b *AddrBook) Worked(node, addr string) {
	b.mu.Lock()
	e := b.m[node]
	if e == nil {
		b.mu.Unlock()
		return
	}
	i := slices.IndexFunc(e.addrs, func(a store.PeerAddr) bool { return a.Addr == addr })
	if i < 0 || i == 0 && e.addrs[0].Good && e.addrs[0].Fails == 0 {
		b.mu.Unlock()
		return
	}
	a := e.addrs[i]
	a.Good, a.Fails, a.Failed = true, 0, time.Time{}
	e.addrs = slices.Insert(slices.Delete(e.addrs, i, i+1), 0, a)
	rows := slices.Clone(e.addrs)
	b.mu.Unlock()
	b.save(node, rows)
}

// Failed records a failed attempt at addr (unreachable, a handshake
// failure, or a timeout): it goes after the addresses that do not fail,
// so the next attempt starts at the next one; after 24 hours of failing
// and at least 5 attempts it is dropped.
func (b *AddrBook) Failed(node, addr string, at time.Time) {
	b.mu.Lock()
	e := b.m[node]
	if e == nil {
		b.mu.Unlock()
		return
	}
	i := slices.IndexFunc(e.addrs, func(a store.PeerAddr) bool { return a.Addr == addr })
	if i < 0 {
		b.mu.Unlock()
		return
	}
	a := e.addrs[i]
	a.Fails++
	if a.Failed.IsZero() {
		a.Failed = at
	}
	e.addrs = slices.Delete(e.addrs, i, i+1)
	if !(a.Fails >= addrDropFails && at.Sub(a.Failed) >= addrDropAfter) {
		e.addrs = append(e.addrs, a)
	}
	order(e.addrs)
	rows := slices.Clone(e.addrs)
	b.mu.Unlock()
	b.save(node, rows)
}

// peers lists, for a response, the nodes of members with addresses, most
// recently heard from first, at most MaxPeersListed.
func (b *AddrBook) peers(members []string) []PullPeer {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []PullPeer
	var at []time.Time
	for _, n := range members {
		e := b.m[n]
		if e == nil || len(e.addrs) == 0 {
			continue
		}
		p := PullPeer{Node: n}
		for _, a := range e.addrs {
			p.Addrs = append(p.Addrs, a.Addr)
		}
		if !e.seen.IsZero() {
			p.Seen = e.seen.UTC().Format(timeFormat)
		}
		out, at = append(out, p), append(at, e.seen)
	}
	idx := make([]int, len(out))
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(i, j int) int { return at[j].Compare(at[i]) })
	sorted := make([]PullPeer, 0, min(len(out), MaxPeersListed))
	for _, i := range idx {
		if len(sorted) == MaxPeersListed {
			break
		}
		sorted = append(sorted, out[i])
	}
	return sorted
}

// timeFormat is the time format of PROTOCOL section 0.
const timeFormat = "2006-01-02T15:04:05.000Z"
