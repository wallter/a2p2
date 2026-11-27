package node

// The review of D71 (group B, second round): admission of non-members
// that carry entries under contention, an invite's addresses only once
// accepted, and net check dialling only what the pulls would.

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

// At most strangerEntriesInFlight such requests are served at a time:
// while the rooms lock is held elsewhere, two wait for it and the rest
// are refused 429 at once, without waiting. The token burst is not a cap
// on requests in flight; this is.
func TestRequestsWithEntriesFromKeysThatAreNotMembersAreCappedInFlight(t *testing.T) {
	ctx := context.Background()
	a, _, _, room := pairWithHistory(t)
	heads, _ := a.r.rooms[room].logHeads()
	ws, err := a.r.Offer(room)
	if err != nil {
		t.Fatal(err)
	}
	a.r.mu.Lock()
	const n = 6
	errs := make(chan error, n)
	for i := range n {
		go func() {
			q := strangerOpening(room, heads)
			q.Log.Entries = []json.RawMessage{json.RawMessage(ws[0])}
			_, err := a.x.Serve(WithSource(ctx, "198.51.100."+string(rune('1'+i))+":5000"), junkNode(i), q)
			errs <- err
		}()
	}
	limited := 0
	for range n - strangerEntriesInFlight {
		select {
		case err := <-errs:
			var pe *PullError
			if !errors.As(err, &pe) || pe.Code != CodeRateLimited {
				a.r.mu.Unlock()
				t.Fatalf("while %d wait for the lock: %v", strangerEntriesInFlight, err)
			}
			limited++
		case <-time.After(30 * time.Second):
			a.r.mu.Unlock()
			t.Fatalf("only %d refused at once; the rest waited for the rooms lock", limited)
		}
	}
	a.r.mu.Unlock()
	for range strangerEntriesInFlight {
		if err := <-errs; !isNotMember(err) {
			t.Fatalf("one served after the lock: %v", err)
		}
	}
}

// Admission under contention: one hostile source floods the listener
// with requests that carry entries. It takes its source's share (2 at
// once, one a second) and no more, so a machine that its own entries
// admit, at another address, is served at once.
func TestAFloodFromOneSourceDoesNotKeepOutAMachineItsOwnEntriesAdmit(t *testing.T) {
	ctx := context.Background()
	a, b, net, room := pairWithHistory(t)
	heads, _ := b.r.rooms[room].logHeads()
	ws, err := b.r.Offer(room)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	served := 0
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q := strangerOpening(room, heads)
			q.Log.Entries = []json.RawMessage{json.RawMessage(ws[0])}
			if _, err := b.x.Serve(WithSource(ctx, "203.0.113.5:5000"), junkNode(i), q); isNotMember(err) {
				mu.Lock()
				served++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if served > strangerSourceBurst {
		t.Fatalf("one source had %d requests with entries checked, its share is %d", served, strangerSourceBurst)
	}
	c := newPNode(t, net, "c", newLNode(t, "c", a.clock))
	a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "cee", History: true})
	c.pin(room, a.lnode)
	c.pull(room, a.lnode)
	if _, err := c.x.Pull(ctx, room, b.id, PullOpts{}); err != nil {
		t.Fatalf("an honest machine at another address, admitted by its own entries: %v", err)
	}
	if _, m := b.r.memberOf(room, c.id); !m {
		t.Fatal("b did not take the entries that admit c")
	}
}

// F5 (the review of D71): previewing an invite dials its addresses (the
// person ran the join with it), but puts nothing in the address book; a
// preview that is left there leaves nothing. Accepting marks them as
// given, to be dialled as written.
func TestAnInvitesAddressesAreDialledAsGivenOnlyOnceTheJoinIsAccepted(t *testing.T) {
	ctx := context.Background()
	clock := lclock()
	jn := newJoinNet()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	room := a.create("team")
	jn.serve(a)
	token := a.invite(room, "member", true)
	book := NewAddrBook()
	j := b.joiner(jn)
	s, err := OpenSurface(ctx, b.m, b.r, SurfaceOpts{Name: "b", Book: book, Joiner: j, Network: Network{State: NetworkOutbound}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.JoinPreview(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if got := book.Addrs(a.id); len(got) != 0 {
		t.Fatalf("a preview put %v in the book", got)
	}
	if given, _ := book.Rule(a.id, addrOf(a)); given {
		t.Fatal("a preview marked the invite's address as given")
	}
	if _, err := s.JoinAccept(ctx, token, p.ID, 0); err != nil {
		t.Fatal(err)
	}
	if given, why := book.Rule(a.id, addrOf(a)); !given || why != "" {
		t.Fatalf("after the accept: given %v, refused %q, book %v", given, why, book.Addrs(a.id))
	}
}

// F5, F54 (the review of D71): net check probes a member's addresses as
// the pulls would dial them. An address the rule refuses (a loopback one
// stored by an earlier version) is not probed and is reported not tried,
// with why; a name is looked up once and probed only at the addresses it
// gives that the rule allows, so "reached" is what the pulls would reach;
// an address from an invite the person accepted is probed as given.
func TestNetCheckProbesMembersOnlyWhereThePullsWouldDial(t *testing.T) {
	a, b, c, _, _ := dialBackRoom(t)
	var room string
	a.r.mu.Lock()
	for id := range a.r.rooms {
		if id != localRoomID(a.id) {
			room = id
		}
	}
	a.r.mu.Unlock()
	a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "cee", History: true})
	book := NewAddrBook()
	book.Listed(b.id, []string{"hub.example:7423"}, *a.clock)
	book.add(b.id, []string{"127.0.0.1:6379"}, "self", *a.clock, false, false) // as an earlier version stored it
	book.Learn(c.id, []string{"127.0.0.1:7424"}, *a.clock)                     // an accepted invite
	lookups := 0
	pr := answerProber{
		b.id + " 192.168.1.9:7423": {Result: ProbeReached},
		c.id + " 127.0.0.1:7424":   {Result: ProbeReached},
	}
	s, err := OpenSurface(context.Background(), a.m, a.r, SurfaceOpts{Name: "a", Book: book, Probe: pr,
		Network: Network{State: NetworkOutbound},
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			lookups++
			return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("192.168.1.9")}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	nc, err := s.NetCheck(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]AddrCheck{}
	for _, m := range nc.Members {
		if !m.Reached {
			t.Errorf("%s not reached", m.Name)
		}
		for _, ac := range m.Addrs {
			got[ac.Addr] = ac
		}
	}
	if r := got["127.0.0.1:6379"]; r.Result != ProbeNotTried || !strings.Contains(r.Detail, "loopback") {
		t.Errorf("a refused stored address: %+v", r)
	}
	if r := got["hub.example:7423"]; r.Result != ProbeReached || !strings.Contains(r.Detail, "192.168.1.9") {
		t.Errorf("a name: %+v", r)
	}
	if r := got["127.0.0.1:7424"]; r.Result != ProbeReached {
		t.Errorf("an invite's address: %+v", r)
	}
	if lookups != 1 {
		t.Errorf("%d lookups, want 1", lookups)
	}
}
