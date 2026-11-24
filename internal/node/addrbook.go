package node

// Peers' addresses (PROTOCOL-next section 0: "addresses are hints, learned
// from the token, a peer's addrs, and peers in responses, stored per member
// and tried most recent success first"). Kept in memory in step 5b: no
// existing table holds an address per member of a log room (room_members
// has none; invites and joins hold one machine's addresses for one join),
// and a restart relearns them from the first requests and responses. The
// switch seeds the book from joins and invites.

import (
	"slices"
	"sync"
	"time"
)

// maxBookNodes bounds the book; beyond it the node heard of longest ago
// is dropped.
const maxBookNodes = 4096

// AddrBook is where each node was last heard of, most recent success
// first. Safe for concurrent use; a key is never a credential, every
// connection is pinned to the node id.
type AddrBook struct {
	mu sync.Mutex
	m  map[string]*bookEntry
}

type bookEntry struct {
	addrs []string  // most recent success first, then most recently learned
	seen  time.Time // when the node was last heard from, directly or through a peer
}

// NewAddrBook returns an empty book.
func NewAddrBook() *AddrBook { return &AddrBook{m: map[string]*bookEntry{}} }

// Learn adds addrs for node: addresses not known yet go after the known
// ones, the list stays within MaxAddrs. seen moves forward only.
func (b *AddrBook) Learn(node string, addrs []string, seen time.Time) {
	if len(addrs) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.m[node]
	if e == nil {
		if len(b.m) >= maxBookNodes {
			b.evict()
		}
		e = &bookEntry{}
		b.m[node] = e
	}
	for _, a := range addrs {
		if a != "" && len(a) <= MaxAddrBytes && !slices.Contains(e.addrs, a) && len(e.addrs) < MaxAddrs {
			e.addrs = append(e.addrs, a)
		}
	}
	if seen.After(e.seen) {
		e.seen = seen
	}
}

// Replace sets node's addresses to addrs as the node itself listed them
// (its request's addrs): what it no longer lists is dropped; an address
// that worked last stays first if it is still listed.
func (b *AddrBook) Replace(node string, addrs []string, seen time.Time) {
	if len(addrs) == 0 {
		return
	}
	b.mu.Lock()
	e := b.m[node]
	if e != nil {
		var keep []string
		for _, a := range e.addrs {
			if slices.Contains(addrs, a) {
				keep = append(keep, a)
			}
		}
		e.addrs = keep
	}
	b.mu.Unlock()
	b.Learn(node, addrs, seen)
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

// Addrs is node's addresses, the one that worked last first.
func (b *AddrBook) Addrs(node string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e := b.m[node]; e != nil {
		return slices.Clone(e.addrs)
	}
	return nil
}

// Worked moves addr to the front of node's addresses.
func (b *AddrBook) Worked(node, addr string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.m[node]
	if e == nil {
		return
	}
	if i := slices.Index(e.addrs, addr); i > 0 {
		e.addrs = slices.Insert(slices.Delete(e.addrs, i, i+1), 0, addr)
	}
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
		p := PullPeer{Node: n, Addrs: slices.Clone(e.addrs)}
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

// timeFormat is the time format of PROTOCOL-next section 0.
const timeFormat = "2006-01-02T15:04:05.000Z"
