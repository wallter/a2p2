package node

import (
	"container/list"
	"net"
	"net/netip"
)

// SourceGroup is the key under which requests from a network source are
// counted: an IPv4 address, or the /64 prefix of an IPv6 address, since one
// IPv6 host is usually given a whole /64. An IPv4-mapped IPv6 address
// counts as its IPv4 address, and a zone is dropped. addr is a host or a
// host:port; anything that is not an IP address is its own group.
func SourceGroup(addr string) string {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return addr
	}
	ip = ip.WithZone("").Unmap()
	if ip.Is4() {
		return ip.String()
	}
	p, err := ip.Prefix(64)
	if err != nil {
		return addr
	}
	return p.String()
}

// Recent keeps a value per key for at most max keys. While it is full, a
// new key pushes out the key used least recently. Every operation takes
// constant time, so a full table costs no scan. It is not safe for
// concurrent use: callers hold their own lock.
type Recent[V any] struct {
	max int
	m   map[string]*list.Element
	l   list.List // front: used most recently
}

type recentItem[V any] struct {
	key string
	v   V
}

// NewRecent makes a table of at most size keys (at least one).
func NewRecent[V any](size int) *Recent[V] {
	return &Recent[V]{max: max(1, size), m: map[string]*list.Element{}}
}

// Use returns key's value, a zero value if key is new, and marks key as
// used most recently.
func (r *Recent[V]) Use(key string) *V {
	if e := r.m[key]; e != nil {
		r.l.MoveToFront(e)
		return &e.Value.(*recentItem[V]).v
	}
	if r.l.Len() >= r.max {
		old := r.l.Back()
		r.l.Remove(old)
		delete(r.m, old.Value.(*recentItem[V]).key)
	}
	it := &recentItem[V]{key: key}
	r.m[key] = r.l.PushFront(it)
	return &it.v
}

// Get returns key's value without marking it used, or nil.
func (r *Recent[V]) Get(key string) *V {
	if e := r.m[key]; e != nil {
		return &e.Value.(*recentItem[V]).v
	}
	return nil
}

// Delete forgets key.
func (r *Recent[V]) Delete(key string) {
	if e := r.m[key]; e != nil {
		r.l.Remove(e)
		delete(r.m, key)
	}
}

// Len is the number of keys held.
func (r *Recent[V]) Len() int { return r.l.Len() }
