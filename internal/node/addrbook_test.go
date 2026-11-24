package node

// Step 5b fix round, items 7 and 10: the address book.

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

var bookT0 = time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)

func addrsN(prefix string, n int) []string {
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("%s%d:7443", prefix, i))
	}
	return out
}

// [item 10] What a node says about itself takes a slot before what a
// third party says; third parties never push it out; at most 8.
func TestAnAddressANodeGivesForItselfTakesASlotBeforeOneHeardFromAThirdParty(t *testing.T) {
	b := NewAddrBook()
	b.Learn(wNode, addrsN("gossip", 8), bookT0)
	b.Replace(wNode, []string{"self:7443"}, bookT0)
	got := b.Addrs(wNode)
	if len(got) != MaxAddrs || got[0] != "self:7443" {
		t.Fatalf("after its own word: %v", got)
	}
	b.Learn(wNode, addrsN("more", 8), bookT0)
	if got := b.Addrs(wNode); !slices.Contains(got, "self:7443") || len(got) != MaxAddrs {
		t.Fatalf("gossip pushed out its own address: %v", got)
	}
}

// [items 7, 10] A failure moves an address behind the others (the next
// attempt starts at the next one); a pinned handshake marks it good and
// first; one failing for 24 hours and at least 5 attempts is dropped.
func TestAFailingAddressGoesLastAndIsDroppedAfterADayAndFiveAttempts(t *testing.T) {
	b := NewAddrBook()
	b.Replace(wNode, []string{"one:1", "two:2"}, bookT0)
	b.Failed(wNode, "one:1", bookT0)
	if got := b.Addrs(wNode); got[0] != "two:2" {
		t.Fatalf("after one failure: %v", got)
	}
	b.Worked(wNode, "one:1")
	if got := b.Dial(wNode); got[0].Addr != "one:1" || !got[0].Good {
		t.Fatalf("after it worked: %+v", got)
	}
	for i := 0; i < 4; i++ {
		b.Failed(wNode, "two:2", bookT0.Add(time.Duration(i)*time.Hour*12))
	}
	if !slices.Contains(b.Addrs(wNode), "two:2") {
		t.Fatal("dropped before five attempts")
	}
	b.Failed(wNode, "two:2", bookT0.Add(25*time.Hour))
	if slices.Contains(b.Addrs(wNode), "two:2") {
		t.Fatalf("kept after a day and five attempts: %v", b.Addrs(wNode))
	}
}

// [item 10] Addresses survive a restart, for nodes that are members of a
// room this machine is in only.
func TestAddressesOfMembersSurviveARestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "a2p2.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadAddrBook(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	stranger := wNode[:63] + "d"
	b.Replace(wNode, []string{"self:1"}, bookT0)
	b.Learn(wNode, []string{"gossip:2"}, bookT0)
	b.Learn(stranger, []string{"x:3"}, bookT0)
	b.Worked(wNode, "gossip:2")
	st.Close()
	if st, err = store.Open(path); err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b, err = LoadAddrBook(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	b.Keep(func(n string) bool { return n == wNode })
	if got := b.Dial(wNode); len(got) != 2 || got[0].Addr != "gossip:2" || !got[0].Good {
		t.Fatalf("after a restart: %+v", got)
	}
	if b.Addrs(stranger) != nil {
		t.Fatal("kept an address of a node in no room here")
	}
	rows, _ := st.PeerAddrs(ctx)
	for _, r := range rows {
		if r.Node == stranger {
			t.Fatal("the non-member's address is still stored")
		}
	}
}

// [item 10] An opening request asks for the room's peers when this
// machine lacks an address for some other current member, and otherwise
// at most once an hour per peer; peers come back only when asked, in a
// same answer as in a digests one.
func TestPeersAreAskedForWhenAnAddressIsMissingOrHourlyAndListedOnlyWhenAsked(t *testing.T) {
	clock := lclock()
	net := newMemNet()
	a := newPNode(t, net, "a", newLNode(t, "a", clock))
	b := newPNode(t, net, "b", newLNode(t, "b", clock))
	c := newPNode(t, net, "c", newLNode(t, "c", clock))
	for _, p := range []*pnode{a, b, c} {
		p.opts.Book = NewAddrBook()
		p.opts.Addrs = []string{p.name + ".example:7443"}
		p.attach()
	}
	room := a.create("team")
	a.change(room, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee", History: true})
	a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "cee", History: true})
	for _, p := range []*pnode{b, c} {
		p.pin(room, a.lnode)
		p.pull(room, a.lnode)
		p.mustPull(room, a, PullOpts{}) // a learns each one's own address
	}
	var listed []bool // per response to b: peers listed
	net.tamper = func(from, _ string, resp *PullResponse) {
		if from == b.id {
			listed = append(listed, resp.Peers != nil)
		}
	}
	asked := func() []bool {
		net.mu.Lock()
		defer net.mu.Unlock()
		var out []bool
		for _, q := range net.heard {
			if q.Sum != "" {
				out = append(out, q.Peers)
			}
		}
		return out
	}
	b.opts.Book.Keep(func(n string) bool { return n == a.id }) // b knows where a is, not c
	mark, listed := len(asked()), []bool(nil)
	b.mustPull(room, a, PullOpts{})
	if got := b.opts.Book.Addrs(c.id); len(got) != 1 || got[0] != "c.example:7443" {
		t.Fatalf("b learned c at %v", got)
	}
	if q := asked()[mark:]; len(q) == 0 || !q[0] || len(listed) == 0 || !listed[0] {
		t.Fatalf("missing c: asked %v, listed %v", q, listed)
	}
	mark, listed = len(asked()), nil
	b.mustPull(room, a, PullOpts{})
	if q := asked()[mark:]; len(q) == 0 || q[0] || slices.Contains(listed, true) {
		t.Fatalf("with every address: asked %v, listed %v", q, listed)
	}
	*clock = clock.Add(askPeersEvery + time.Minute)
	mark = len(asked())
	b.mustPull(room, a, PullOpts{})
	if q := asked()[mark:]; len(q) == 0 || !q[0] {
		t.Fatalf("an hour later: asked %v", q)
	}
}

// A machine's own new list replaces what it said of itself before (a
// mapped address it no longer advertises is dropped); what a third party
// passed on stays; an empty list changes nothing, so a listener that
// briefly advertised nothing is still found where it was.
func TestAMachinesOwnNewListReplacesItsOldOneAndAnEmptyListChangesNothing(t *testing.T) {
	b := NewAddrBook()
	b.Replace("n", []string{"198.51.100.7:40001", "192.168.1.5:7423"}, bookT0)
	b.Learn("n", []string{"10.0.0.5:7423"}, bookT0)
	b.Replace("n", []string{"192.168.1.5:7423"}, bookT0.Add(time.Hour))
	got := b.Addrs("n")
	if slices.Contains(got, "198.51.100.7:40001") || !slices.Contains(got, "192.168.1.5:7423") || !slices.Contains(got, "10.0.0.5:7423") {
		t.Fatalf("after its new list: %v", got)
	}
	b.Replace("n", nil, bookT0.Add(2*time.Hour))
	if after := b.Addrs("n"); !slices.Equal(after, got) {
		t.Fatalf("an empty list changed %v to %v", got, after)
	}
}
