package node

import (
	"context"
	"sync"
	"testing"

	"github.com/wallter/a2p2/internal/core"
)

// Several local sessions on two machines write at once; a third machine
// that pulls from both holds every message once, each author's stream
// gap-free and in order (ported from v0.2's TestConcurrentSenders).
//
// Requirements: D3, D4
func TestConcurrentWritersOnTwoMachinesReachAThirdOnceAndInOrder(t *testing.T) {
	clock := lclock()
	net := newMemNet()
	a := newPNode(t, net, "a", newLNode(t, "a", clock))
	b := newPNode(t, net, "b", newLNode(t, "b", clock))
	c := newPNode(t, net, "c", newLNode(t, "c", clock))
	room := a.create("team")
	a.change(room, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee", History: true})
	a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "cee", History: true})
	for _, p := range []*pnode{b, c} {
		p.pin(room, a.lnode)
		p.pull(room, a.lnode)
	}
	const each = 100
	var wg sync.WaitGroup
	for _, p := range []*pnode{a, b} {
		p.register("w")
		for w := 0; w < 4; w++ { // several local sessions per machine
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < each/4; i++ {
					if _, err := p.m.Send(context.Background(), Draft{Room: room, Agent: "w", Text: "x"}); err != nil {
						t.Error(err)
					}
				}
			}()
		}
	}
	wg.Wait()
	messages := func() int {
		n := 0
		for _, r := range c.records(room) {
			if r.Fields().Kind == core.KindMessage {
				n++
			}
		}
		return n
	}
	for i := 0; i < 20 && messages() < 2*each; i++ {
		c.mustPull(room, a, PullOpts{})
		c.mustPull(room, b, PullOpts{})
	}
	if n := messages(); n != 2*each {
		t.Fatalf("c holds %d messages, want %d", n, 2*each)
	}
	ids := map[string]bool{}
	last := map[string]int64{}
	for _, r := range c.records(room) { // by author, stream and position
		f := r.Fields()
		if ids[r.ID()] {
			t.Fatalf("record %s held twice", r.ID())
		}
		ids[r.ID()] = true
		key := r.Author() + "/" + f.Epoch
		if f.Seq != last[key]+1 {
			t.Fatalf("stream %s: position %d after %d", core.ShortNode(key), f.Seq, last[key])
		}
		last[key] = f.Seq
	}
}
