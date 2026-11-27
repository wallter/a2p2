package peer

// The third review of D71: the verified mark never overrides the address
// rule, each of the two checks on the pull path is pinned by its own
// test, and a name is charged for every address it gives.

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// A row an earlier version stored as verified (Good: a pinned handshake
// completed there) at a loopback and at a link-local address is held and
// not dialled: the mark says the member's key once answered there, not
// that this machine's person gave the address. The pull goes on to the
// address the accepted invite gave, and the loopback port never sees a
// connection.
func TestAStoredVerifiedAddressOnLoopbackOrLinkLocalIsHeldAndNotDialled(t *testing.T) {
	a, b := newPM(t, "a", true), newPM(t, "b", true)
	room := group(t, a, b)
	probe := newCounter(t)
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/a2p2.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	loop, link := probe.ln.Addr().String(), "169.254.169.254:8080"
	now := time.Now()
	if err := st.SetPeerAddrs(ctx, a.node(), []store.PeerAddr{
		{Node: a.node(), Addr: loop, Source: store.AddrSelf, Seen: now, Good: true},
		{Node: a.node(), Addr: link, Source: store.AddrSelf, Seen: now, Good: true},
		{Node: a.node(), Addr: a.addr, Source: store.AddrPeer, Seen: now},
	}); err != nil {
		t.Fatal(err)
	}
	book, err := node.LoadAddrBook(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	book.Learn(a.node(), []string{a.addr}, time.Time{}) // the invite b's person accepted named a here
	for _, addr := range []string{loop, link} {
		if given, why := book.Rule(a.node(), addr); given || why == "" {
			t.Fatalf("%s: given %v, refused %q", addr, given, why)
		}
	}
	held := book.Held(a.node())
	if len(held) != 2 || !slices.ContainsFunc(held, func(h node.HeldAddr) bool { return h.Addr == loop && h.Why != "" }) ||
		!slices.ContainsFunc(held, func(h node.HeldAddr) bool { return h.Addr == link && h.Why != "" }) {
		t.Fatalf("held: %+v", held)
	}
	cl := NewPullClient(b.id, book)
	t.Cleanup(cl.Close)
	x := node.NewExchange(b.r, cl, node.ExchangeOpts{Book: book})
	if _, err := x.Pull(ctx, room, a.node(), node.PullOpts{}); err != nil {
		t.Fatalf("b pulls from a: %v", err)
	}
	if !probe.none(t) {
		t.Fatal("a verified loopback address from an earlier version was dialled")
	}
}

// rtRec is a transport that records the hosts of the requests it is
// given, and answers none.
type rtRec struct {
	mu    sync.Mutex
	hosts []string
}

func (r *rtRec) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.hosts = append(r.hosts, req.URL.Host)
	r.mu.Unlock()
	return nil, errors.New("recorded")
}

// The first of the two checks: the pull loop (PullClient.dialled) skips
// an address the rule refuses before a request is made for it, so no
// further attempt of the requester's budget is spent and the transport
// never sees it. Only the address the rule allows reaches the transport.
func TestThePullLoopMakesNoRequestForAnAddressTheRuleRefuses(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/a2p2.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	peer := newID(t).NodeID()
	now := time.Now()
	if err := st.SetPeerAddrs(ctx, peer, []store.PeerAddr{
		{Node: peer, Addr: "127.0.0.1:7423", Source: store.AddrSelf, Seen: now, Good: true},
		{Node: peer, Addr: "192.0.2.1:7423", Source: store.AddrPeer, Seen: now},
	}); err != nil {
		t.Fatal(err)
	}
	book, err := node.LoadAddrBook(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	cl := NewPullClient(newID(t), book)
	t.Cleanup(cl.Close)
	rec := &rtRec{}
	cl.mu.Lock()
	cl.clients[peer] = &http.Client{Transport: rec}
	cl.mu.Unlock()
	if _, err := cl.Pull(ctx, peer, &node.PullRequest{V: node.PullVersion}); err == nil {
		t.Fatal("a pull answered by a transport that answers nothing")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !slices.Equal(rec.hosts, []string{"192.0.2.1:7423"}) {
		t.Fatalf("requests made for %v, want only the allowed address", rec.hosts)
	}
}

// The second check: the dial itself (the client's DialContext: the rule,
// then DialTargets) refuses an address the rule refuses, whatever asked for
// it, so a request made past the pull loop still never connects.
func TestTheDialRefusesAnAddressTheRuleRefusesForAnyRequest(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/a2p2.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	probe := newCounter(t)
	peer := newID(t).NodeID()
	addr := probe.ln.Addr().String()
	if err := st.SetPeerAddrs(ctx, peer, []store.PeerAddr{{Node: peer, Addr: addr, Source: store.AddrSelf, Seen: time.Now(), Good: true}}); err != nil {
		t.Fatal(err)
	}
	book, err := node.LoadAddrBook(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	cl := NewPullClient(newID(t), book)
	t.Cleanup(cl.Close)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+addr+pathPullV2, bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.client(peer).Do(req); !errors.Is(err, node.ErrAddrRefused) {
		t.Fatalf("a request to a refused address: %v", err)
	}
	if !probe.none(t) {
		t.Fatal("the dial connected to a refused address")
	}
}

// The dial asks the rule before it charges anything: a name the rule
// refuses (localhost), asked for past the pull loop, is refused without a
// token of the member's share or the machine's budget, and is never
// looked up. (DialTargets would refuse it too, but only after the charge
// a name pays before its lookup.)
func TestTheDialRefusesARefusedNameBeforeItChargesTheBudgets(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/a2p2.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	peer := newID(t).NodeID()
	name := "localhost:7423"
	if err := st.SetPeerAddrs(ctx, peer, []store.PeerAddr{{Node: peer, Addr: name, Source: store.AddrSelf, Seen: time.Now(), Good: true}}); err != nil {
		t.Fatal(err)
	}
	book, err := node.LoadAddrBook(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	cl := NewPullClient(newID(t), book)
	t.Cleanup(cl.Close)
	cl.Resolve = func(context.Context, string) ([]netip.Addr, error) {
		t.Error("a refused name was looked up")
		return nil, errors.New("no lookups in this test")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+name+pathPullV2, bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := cl.client(peer).Do(req); !errors.Is(err, node.ErrAddrRefused) {
			t.Fatalf("a refused name: %v", err)
		}
	}
	for i := range int(pullPeerDialBurst) {
		if ok, _ := cl.peerDials.take(peer); !ok {
			t.Fatalf("the member's share lost tokens to a refused name: only %d left", i)
		}
	}
}

// A host name that once completed a pinned handshake is charged like any
// other: the dial budgets are spent unless the literal address dialled
// itself completed one. With the member's share spent, a dial of the
// verified name is refused before the name is even looked up.
func TestAVerifiedNameIsChargedToTheDialBudgets(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/a2p2.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	peer := newID(t).NodeID()
	name := "peer.example:7423"
	if err := st.SetPeerAddrs(ctx, peer, []store.PeerAddr{{Node: peer, Addr: name, Source: store.AddrSelf, Seen: time.Now(), Good: true}}); err != nil {
		t.Fatal(err)
	}
	book, err := node.LoadAddrBook(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	cl := NewPullClient(newID(t), book)
	t.Cleanup(cl.Close)
	lookups := 0
	cl.Resolve = func(context.Context, string) ([]netip.Addr, error) {
		lookups++
		return nil, errors.New("no lookups in this test")
	}
	for {
		if ok, _ := cl.peerDials.take(peer); !ok {
			break
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+name+pathPullV2, bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.client(peer).Do(req); !errors.Is(err, errDialBudget) {
		t.Fatalf("a verified name with the budget spent: %v", err)
	}
	if lookups != 0 {
		t.Fatalf("the name was looked up %d times with the budget spent", lookups)
	}
}

// The other side: a literal address that completed a pinned handshake
// with the member costs nothing, so a member reached before is still
// reached with every dial budget spent.
func TestAVerifiedLiteralAddressIsDialledWithTheBudgetsSpent(t *testing.T) {
	a, b := newPM(t, "a", true), newPM(t, "b", true)
	room := group(t, a, b)
	b.mustPull(room, a, node.PullOpts{}) // a.addr completes a handshake
	if !slices.ContainsFunc(b.book.Dial(a.node()), func(d node.DialAddr) bool { return d.Addr == a.addr && d.Good }) {
		t.Fatal("a's address is not marked verified")
	}
	b.cl.client(a.node()).CloseIdleConnections()
	for {
		if ok, _ := b.cl.peerDials.take(a.node()); !ok {
			break
		}
	}
	for {
		if ok, _ := b.cl.dials.take(""); !ok {
			break
		}
	}
	b.mustPull(room, a, node.PullOpts{})
}
