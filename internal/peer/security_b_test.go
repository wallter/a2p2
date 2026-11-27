package peer

// Group B of the 2026-11-25 review over real TLS on loopback: the
// "connected in" record (F25) and what a member's addresses make this
// machine dial (F5, F54).

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// inRec is a machine's peer handler that knows its members and records
// every key the listener reports as connected in.
type inRec struct {
	dbH
	r    *node.Rooms
	mu   sync.Mutex
	keys map[string]int
}

func (h *inRec) SharesRoom(n string) bool { return h.r.SharesRoom(n) }
func (h *inRec) Inbound(from, _ string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.keys[from]++
}
func (h *inRec) count(n string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.keys[n]
}

// F25: keys that share no room here were recorded as connected in, and
// a flood of fresh ones pushed the members out of the record (it keeps
// 1,024). Now a stranger's request is not recorded at all; a member's is.
//
// Requirements: N1
func TestOverTLSAStrangersRequestIsNotRecordedAsConnectedIn(t *testing.T) {
	a, b := newPM(t, "a", true), newPM(t, "b", true)
	group(t, a, b)
	h := &inRec{dbH: dbH{a.h, node.NewDialBackServer(a.r, NewProber(a.id))}, r: a.r, keys: map[string]int{}}
	addr := serve(t, a.id, h, nil)
	for i := range 3 {
		x := newPM(t, "x", false)
		x.book.Replace(a.node(), []string{addr}, time.Now())
		if _, err := x.cl.DialBack(bg, a.node(), []string{"127.0.0.1:1"}); err == nil {
			t.Fatalf("stranger %d was answered", i)
		}
		if n := h.count(x.node()); n != 0 {
			t.Fatalf("stranger %d recorded as connected in %d times", i, n)
		}
	}
	forget(b.book, a.node())
	b.book.Replace(a.node(), []string{addr}, time.Now())
	if _, err := b.cl.DialBack(bg, a.node(), []string{"127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	if h.count(b.node()) == 0 {
		t.Fatal("a member's request was not recorded")
	}
}

// counter is a loopback listener that records the client address of each
// connection it accepts, and closes it.
type counter struct {
	ln   net.Listener
	mu   sync.Mutex
	from []string
}

func newCounter(t *testing.T) *counter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &counter{ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			c.mu.Lock()
			c.from = append(c.from, conn.RemoteAddr().String())
			c.mu.Unlock()
			conn.Close()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return c
}

// none says that no connection reached c before now, strictly: it makes a
// control connection, waits until that very connection is accepted (a
// connection made earlier is accepted before it), and then reports
// whether it is the only one. One forbidden connection counted before the
// call makes it false.
func (c *counter) none(t *testing.T) bool {
	t.Helper()
	conn, err := net.Dial("tcp", c.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	mine := conn.LocalAddr().String()
	conn.Close()
	within(t, "the control connection", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return slices.Contains(c.from, mine)
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.from) == 1
}

// The strict helper is strict: one connection before the call makes it
// say so (before, a count that was already 1 passed).
func TestTheNoConnectionHelperFailsAfterOneForbiddenConnection(t *testing.T) {
	c := newCounter(t)
	conn, err := net.Dial("tcp", c.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if c.none(t) {
		t.Fatal("none() passed after a connection")
	}
	if c2 := newCounter(t); !c2.none(t) {
		t.Fatal("none() failed with no connection")
	}
}

// F5, F54: an address this machine's book holds for a member on another
// machine's word (here as a past version stored it) that is on loopback
// is never dialled: the pull goes on to the member's real address, and
// the loopback port never sees a connection. A name another machine gave
// that resolves to loopback is not dialled either.
func TestOverTLSAnAddressOnLoopbackFromAnotherMachinesWordIsNeverDialled(t *testing.T) {
	a, b := newPM(t, "a", true), newPM(t, "b", true)
	room := group(t, a, b)
	probe := newCounter(t)
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/a2p2.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetPeerAddrs(ctx, a.node(), []store.PeerAddr{
		{Node: a.node(), Addr: probe.ln.Addr().String(), Source: store.AddrSelf, Seen: time.Now()},
		{Node: a.node(), Addr: "probe.example:" + portOf(probe.ln), Source: store.AddrSelf, Seen: time.Now(), Good: true},
		{Node: a.node(), Addr: a.addr, Source: store.AddrPeer, Seen: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	book, err := node.LoadAddrBook(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	book.Learn(a.node(), []string{a.addr}, time.Time{}) // the invite b's person accepted named a here
	if h := book.Held(a.node()); len(h) != 1 || h[0].Addr != probe.ln.Addr().String() {
		t.Fatalf("held and not dialled: %+v", h)
	}
	cl := NewPullClient(b.id, book)
	t.Cleanup(cl.Close)
	lookups := 0
	cl.Resolve = func(context.Context, string) ([]netip.Addr, error) {
		lookups++
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	x := node.NewExchange(b.r, cl, node.ExchangeOpts{Book: book})
	if _, err := x.Pull(ctx, room, a.node(), node.PullOpts{}); err != nil {
		t.Fatalf("b pulls from a: %v", err)
	}
	if !probe.none(t) {
		t.Fatal("a loopback address from another machine's word was dialled")
	}
	if lookups != 1 {
		t.Fatalf("%d lookups of the name, want 1", lookups)
	}
}

// The review of D71 (point 2): a member whose request arrives over
// loopback is not on this computer for that (behind a tunnel or a reverse
// proxy on this machine, every member's request arrives over loopback),
// so the loopback address it gives for itself is never dialled, nor one
// listed for it by a member reached over loopback. Public paths only, so
// it runs against the parent commit, where it fails.
func TestOverTLSAMembersOwnLoopbackAddressIsNeverDialledHoweverItConnected(t *testing.T) {
	a, b, c := newPM(t, "a", true), newPM(t, "b", false), newPM(t, "c", false)
	room := group(t, a, b, c)
	own, listed := newCounter(t), newCounter(t)
	// b gives a loopback address of its choice for itself, over loopback.
	bx := node.NewExchange(b.r, b.cl, node.ExchangeOpts{Book: b.book, Addrs: []string{own.ln.Addr().String()}})
	if _, err := bx.Pull(bg, room, a.node(), node.PullOpts{}); err != nil {
		t.Fatal(err)
	}
	// c does the same; then b, which reached a over loopback, asks a for
	// the room's peers, and a lists c there if it kept it.
	cx := node.NewExchange(c.r, c.cl, node.ExchangeOpts{Book: c.book, Addrs: []string{listed.ln.Addr().String()}})
	if _, err := cx.Pull(bg, room, a.node(), node.PullOpts{}); err != nil {
		t.Fatal(err)
	}
	forget(b.book, c.node())
	b.mustPull(room, a, node.PullOpts{})
	for range 2 {
		_, _ = a.x.Pull(bg, room, b.node(), node.PullOpts{})
		_, _ = b.x.Pull(bg, room, c.node(), node.PullOpts{})
	}
	if !own.none(t) {
		t.Fatal("a member's own loopback address was dialled")
	}
	if !listed.none(t) {
		t.Fatal("a loopback address listed for a member was dialled")
	}
}

// The honest case: two machines on one computer, as the demo runs. b
// joined through a's invite, which names a's loopback address: b dials it
// as given, and a, which never dials b's loopback address, gets b's
// messages from b's own requests. Both run the scheduler, as daemons do.
func TestOverTLSTwoMachinesOnOneComputerWorkThroughTheInvitesAddress(t *testing.T) {
	a, b := newPM(t, "a", true), newPM(t, "b", true)
	room := group(t, a, b) // b knows a from the invite
	runSched(t, a, b)
	fromB, fromA := b.send(room, "from b", false), a.send(room, "from a", false)
	within(t, "both messages on both machines", func() bool {
		return a.holds(room, fromB) && b.holds(room, fromA)
	})
	if given, why := a.book.Rule(b.node(), b.addr); given || why == "" {
		t.Fatalf("a would dial b's own loopback address: given %v, refused %q", given, why)
	}
	if got, err := node.DialTargets(bg, "192.168.1.5:7423", false, nil); err != nil || len(got) != 1 {
		t.Fatalf("a LAN address: %v %v", got, err)
	}
}

// Three machines on one computer, where only the inviter's address came
// from an invite: b and c never dial each other's loopback addresses,
// and each one's messages reach the other through a, the machine both
// joined.
func TestOverTLSThreeMachinesOnOneComputerReachEachOtherThroughTheInviter(t *testing.T) {
	a, b, c := newPM(t, "a", true), newPM(t, "b", true), newPM(t, "c", true)
	room := group(t, a, b, c)
	runSched(t, a, b, c)
	fromB, fromC := b.send(room, "from b", false), c.send(room, "from c", false)
	within(t, "each message on the other machines", func() bool {
		return a.holds(room, fromB) && c.holds(room, fromB) && a.holds(room, fromC) && b.holds(room, fromC)
	})
	if got := b.book.Addrs(c.node()); len(got) != 0 {
		t.Fatalf("b holds c at %v", got)
	}
	if got := c.book.Addrs(b.node()); len(got) != 0 {
		t.Fatalf("c holds b at %v", got)
	}
}

// The review of D71 (point 5): a file transfer asks the same question as
// a pull. A stored address the rule refuses is not one the file client
// can dial, so the listener asks the author to upload instead.
func TestAFileClientCannotDialAStoredAddressThatThePullsRefuse(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/a2p2.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	n := newID(t).NodeID()
	if err := st.SetPeerAddrs(ctx, n, []store.PeerAddr{{Node: n, Addr: "127.0.0.1:7423", Source: store.AddrSelf, Seen: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	book, err := node.LoadAddrBook(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	pc := NewPullClient(newID(t), book)
	t.Cleanup(pc.Close)
	if NewFileClient(pc).CanDial(n) {
		t.Fatal("a refused stored address counts as one the file client can dial")
	}
	book.Learn(n, []string{"127.0.0.1:7423"}, time.Time{}) // an invite gives it
	if !NewFileClient(pc).CanDial(n) {
		t.Fatal("an invite's address cannot be dialled")
	}
}

// The budget for unverified addresses is per peer too: one member's
// addresses spend at most its share, and another member's new address is
// still tried.
func TestOverTLSOneMembersUnverifiedAddressesDoNotSpendAnothersDials(t *testing.T) {
	cl := NewPullClient(newID(t), node.NewAddrBook())
	t.Cleanup(cl.Close)
	spent := 0
	for range 20 {
		if ok, _ := cl.peerDials.take("noisy"); ok {
			if ok, _ := cl.dials.take(""); ok {
				spent++
			}
		}
	}
	if spent != int(pullPeerDialBurst) {
		t.Fatalf("one peer spent %d dials at once, want %v", spent, pullPeerDialBurst)
	}
	if ok, _ := cl.peerDials.take("quiet"); !ok {
		t.Fatal("another peer had no share left")
	}
	if ok, _ := cl.dials.take(""); !ok {
		t.Fatal("the machine's budget was spent by one peer")
	}
}

func portOf(ln net.Listener) string {
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	return p
}

// A dial the machine's budget refuses gives back the token it took of the
// member's share, so that share is spent only on dials made.
func TestADialTheMachinesBudgetRefusesCostsTheMemberNothing(t *testing.T) {
	book := node.NewAddrBook()
	peer := newID(t).NodeID()
	book.Listed(peer, []string{"192.0.2.1:7423"}, time.Now()) // never verified, and never dialled here
	cl := NewPullClient(newID(t), book)
	t.Cleanup(cl.Close)
	for {
		if ok, _ := cl.dials.take(""); !ok {
			break
		}
	}
	if _, err := cl.Pull(bg, peer, &node.PullRequest{V: node.PullVersion}); err == nil {
		t.Fatal("a pull with the machine's budget spent was sent")
	}
	for i := range int(pullPeerDialBurst) {
		if ok, _ := cl.peerDials.take(peer); !ok {
			t.Fatalf("the member's share lost a token: only %d left", i)
		}
	}
}
