package node

import (
	"context"
	"testing"

	"github.com/wallter/a2p2/internal/core"
)

// pnode is an lnode with the pull exchange over a shared memNet.
type pnode struct {
	*lnode
	x    *Exchange
	net  *memNet
	opts ExchangeOpts
}

func newPNode(t *testing.T, net *memNet, name string, l *lnode) *pnode {
	p := &pnode{lnode: l, net: net}
	p.attach()
	return p
}

func (p *pnode) attach() {
	p.x = NewExchange(p.r, p.net.link(p.id), p.opts)
	p.net.attach(p.id, p.x)
}

func (p *pnode) restartP() {
	p.restart()
	p.attach()
}

func (p *pnode) pullFrom(room string, s *pnode, o PullOpts) (PullStats, error) {
	return p.x.Pull(context.Background(), room, s.id, o)
}

func (p *pnode) mustPull(room string, s *pnode, o PullOpts) PullStats {
	p.t.Helper()
	st, err := p.pullFrom(room, s, o)
	if err != nil {
		p.t.Fatalf("%s pulls from %s: %v", p.name, s.name, err)
	}
	return st
}

func TestPullSmoke(t *testing.T) {
	clock := lclock()
	net := newMemNet()
	a := newPNode(t, net, "a", newLNode(t, "a", clock))
	b := newPNode(t, net, "b", newLNode(t, "b", clock))
	room := a.create("team")
	a.change(room, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee", History: true})
	b.pin(room, a.lnode)
	a.register("alice")
	b.register("bob")
	a.send(room, "alice", "hello")
	st := b.mustPull(room, a, PullOpts{})
	t.Logf("%+v", st)
	if es := b.inbox(room, "bob"); len(es) != 1 || es[0].Text != "hello" {
		t.Fatalf("bob: %+v", es)
	}
	b.send(room, "bob", "back")
	st = b.mustPull(room, a, PullOpts{Offer: true})
	t.Logf("%+v", st)
	if es := a.inbox(room, "alice"); len(es) != 1 || es[0].Text != "back" {
		t.Fatalf("alice: %+v", es)
	}
	st = b.mustPull(room, a, PullOpts{})
	t.Logf("%+v", st)
}
