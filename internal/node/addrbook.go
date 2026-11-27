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
//
// Addresses from other machines (Heard, Listed) are kept only if
// RemoteAddrOK holds, and checked again when dialled (Rule, DialTargets);
// addresses this machine's person gave (Learn and Replace: an accepted
// invite, the joins recorded since) are dialled as given. Which addresses
// were given is kept in memory only: an address loaded from the store
// counts as another machine's until a person's invite gives it again
// (Surface.SeedBook, at start), unless it is a literal address that
// completed a pinned handshake with the node; so one a past version kept
// that the rule refuses is held, not dialled (Held says why), and ages
// out as a failing address does.

import (
	"context"
	"net/netip"
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
	// given are the addresses this machine's person gave (Learn,
	// Replace). In memory only.
	given map[string]bool
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

// Learn adds addresses this machine's person gave for node, as a third
// party's (an invite names the admin machine's): only into free slots,
// dialled as given.
func (b *AddrBook) Learn(node string, addrs []string, seen time.Time) {
	b.add(node, addrs, store.AddrPeer, seen, true, false)
}

// Replace sets addresses for node as its own, dialled as given: what it
// no longer lists of its own is dropped, and its own take slots before
// third parties'. For addresses this machine was given; another
// machine's word goes through Heard.
func (b *AddrBook) Replace(node string, addrs []string, seen time.Time) {
	b.add(node, addrs, store.AddrSelf, seen, true, false)
}

// Heard sets the addresses node gave for itself in its own authenticated
// request, as Replace does, keeping only those RemoteAddrOK accepts. A
// list of which none is kept still replaces node's own earlier list.
func (b *AddrBook) Heard(node string, addrs []string, seen time.Time) {
	if len(addrs) == 0 {
		return
	}
	b.add(node, remoteAddrs(addrs), store.AddrSelf, seen, false, true)
}

// Listed adds addresses a member listed for another member (peers in a
// response), as Learn does, keeping only those RemoteAddrOK accepts.
func (b *AddrBook) Listed(node string, addrs []string, seen time.Time) {
	b.add(node, remoteAddrs(addrs), store.AddrPeer, seen, false, false)
}

func remoteAddrs(addrs []string) []string {
	var out []string
	for _, a := range addrs {
		if RemoteAddrOK(a) {
			out = append(out, a)
		}
	}
	return out
}

// Rule is the one answer to "would this machine dial addr of node", for
// pulls, dial-back requests, file transfers, net check and status. given:
// dial it as written, because this machine's person gave it (an accepted
// invite, Learn and Replace) or it is a literal IP address that completed
// a pinned handshake with node (node's own key answered there, so a dial
// reaches node and nothing else; a name could resolve elsewhere since).
// refused: why it is not dialled at all (AddrRefusal); empty when it is.
// A name that is not given passes here and is checked again, address by
// address, when it is looked up (DialTargets).
func (b *AddrBook) Rule(node, addr string) (given bool, refused string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rule(node, addr)
}

func (b *AddrBook) rule(node, addr string) (bool, string) {
	e := b.m[node]
	if e != nil {
		if e.given[addr] {
			return true, ""
		}
		if i := slices.IndexFunc(e.addrs, func(a store.PeerAddr) bool { return a.Addr == addr }); i >= 0 && e.addrs[i].Good {
			if ap, err := netip.ParseAddrPort(addr); err == nil && ap.Addr().Zone() == "" {
				return true, ""
			}
		}
	}
	return false, AddrRefusal(addr)
}

// HeldAddr is an address held for a node that is not dialled, and why.
type HeldAddr struct {
	Addr string `json:"addr"`
	Why  string `json:"why"`
}

// Held lists node's addresses that are kept but not dialled (Rule), in
// the order they are kept: stored by an earlier version, they go once
// they have failed for a day.
func (b *AddrBook) Held(node string) []HeldAddr {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.m[node]
	if e == nil {
		return nil
	}
	var out []HeldAddr
	for _, a := range e.addrs {
		if _, why := b.rule(node, a.Addr); why != "" {
			out = append(out, HeldAddr{Addr: a.Addr, Why: why})
		}
	}
	return out
}

// add puts addrs for node from source; given marks them as this
// machine's person's. empty: an empty addrs still replaces node's own.
func (b *AddrBook) add(node string, addrs []string, source string, seen time.Time, given, empty bool) {
	if len(addrs) == 0 && !empty {
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
	if given && e.given == nil {
		e.given = map[string]bool{}
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
		if given {
			e.given[a] = true
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
	e.trim()
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
	e.trim()
	rows := slices.Clone(e.addrs)
	b.mu.Unlock()
	b.save(node, rows)
}

// trim forgets which addresses were given once they are no longer kept.
func (e *bookEntry) trim() {
	for a := range e.given {
		if !slices.ContainsFunc(e.addrs, func(p store.PeerAddr) bool { return p.Addr == a }) {
			delete(e.given, a)
		}
	}
}

// peers lists, for a response, the nodes of members with addresses, most
// recently heard from first, at most MaxPeersListed; of each, only the
// addresses the asker would keep (Listed).
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
			if RemoteAddrOK(a.Addr) { // what the asker would keep (Listed)
				p.Addrs = append(p.Addrs, a.Addr)
			}
		}
		if len(p.Addrs) == 0 {
			continue
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
