package peer

// Group B of the 2026-11-25 review over real TLS on loopback: the
// "connected in" record (F25) and what a member's addresses make this
// machine dial (F5, F54).

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
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

// counter is a loopback listener that counts the connections it accepts
// and closes them.
type counter struct {
	ln net.Listener
	n  atomic.Int64
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
			c.n.Add(1)
			conn.Close()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return c
}

// none says that no connection reached c before now: one made now is
// accepted after any earlier one, so once it is counted the count is 1
// exactly when nothing came before it.
func (c *counter) none(t *testing.T) bool {
	t.Helper()
	conn, err := net.Dial("tcp", c.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	within(t, "the control connection", func() bool { return c.n.Load() >= 1 })
	return c.n.Load() == 1
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

// The honest cases: a member that connected over loopback (two machines
// on one computer, as the demo runs) is dialled at the loopback address it
// gave for itself; a member's LAN address is kept and tried. Neither needs
// an invite.
func TestOverTLSAMemberOnThisComputerIsDialledAtItsOwnLoopbackAddress(t *testing.T) {
	a, b := newPM(t, "a", true), newPM(t, "b", true)
	room := group(t, a, b)
	// a learns b's address only from b's own requests, over loopback.
	forget(a.book, b.node())
	b.mustPull(room, a, node.PullOpts{})
	if got := a.book.Addrs(b.node()); len(got) != 1 || got[0] != b.addr {
		t.Fatalf("a kept %v of b's own word", got)
	}
	if given, local := a.book.DialPolicy(b.node(), b.addr); given || !local {
		t.Fatalf("given %v, local %v", given, local)
	}
	a.mustPull(room, b, node.PullOpts{})
	ctx := context.Background()
	if got, err := node.DialTargets(ctx, "192.168.1.5:7423", false, false, nil); err != nil || len(got) != 1 {
		t.Fatalf("a LAN address: %v %v", got, err)
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
