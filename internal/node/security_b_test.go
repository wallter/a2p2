package node

// Group B of the 2026-11-25 review, pull-path state: what a key that is
// not a member makes this machine keep (F3, F18, F20), what a member
// makes this machine dial (F5, F54), how long a mute lasts (F22), and the
// warning before removing the one member reached (F21).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// F18: the listener asks whether a key shares a room for every handshake
// and request; the answer no longer waits for the rooms lock.
func TestSharesRoomDoesNotWaitForTheRoomsLock(t *testing.T) {
	a, b, _, _ := pairWithHistory(t)
	a.r.mu.Lock()
	done := make(chan bool, 2)
	go func() { done <- a.r.SharesRoom(b.id); done <- a.r.SharesRoom(junkNode(1)) }()
	var got []bool
	for range 2 {
		select {
		case v := <-done:
			got = append(got, v)
		case <-time.After(30 * time.Second):
			a.r.mu.Unlock()
			t.Fatal("SharesRoom waited for the rooms lock")
		}
	}
	a.r.mu.Unlock()
	if !got[0] || got[1] {
		t.Fatalf("member %v, stranger %v", got[0], got[1])
	}
}

// exchangeSizes is how many (room, peer) entries the exchange keeps.
func exchangeSizes(x *Exchange) [4]int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return [4]int{len(x.heard), len(x.evidence), len(x.pages), len(x.lastEx)}
}

func strangerOpening(room string, heads []string) *PullRequest {
	return &PullRequest{V: PullVersion, Room: room, Sum: sumOf(nil), Log: PullLog{Heads: heads},
		Addrs: []string{"127.0.0.1:22", "198.51.100.7:7423"}}
}

// F3, F18, F20: a thousand keys that are not members, each new, pull a
// room held here. Before, each left its log heads in Exchange.heard for
// the life of the process, and each took the rooms lock. Now none leaves
// anything, none is answered but not_member, and one is refused while
// the rooms lock is held by someone else, without waiting for it.
//
// Requirements: T4
func TestPullsFromKeysThatAreNotMembersKeepNoStateAndDoNotWaitForTheRoomsLock(t *testing.T) {
	ctx := context.Background()
	a, b, _, room := pairWithHistory(t)
	a.opts.Book = NewAddrBook()
	a.attach()
	b.mustPull(room, a, PullOpts{})
	heads, _ := a.r.rooms[room].logHeads()
	before := exchangeSizes(a.x)
	for i := range 1000 {
		if _, err := a.x.Serve(ctx, junkNode(i), strangerOpening(room, heads)); !isNotMember(err) {
			t.Fatalf("stranger %d: %v", i, err)
		}
	}
	if after := exchangeSizes(a.x); after != before {
		t.Fatalf("a thousand strangers changed what the exchange keeps from %v to %v", before, after)
	}
	if got := a.opts.Book.Addrs(junkNode(0)); got != nil {
		t.Fatalf("a stranger's addresses were kept: %v", got)
	}
	a.r.mu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := a.x.Serve(ctx, junkNode(5000), strangerOpening(room, heads))
		done <- err
	}()
	select {
	case err := <-done:
		a.r.mu.Unlock()
		if !isNotMember(err) {
			t.Fatalf("while the rooms lock was held: %v", err)
		}
	case <-time.After(30 * time.Second):
		a.r.mu.Unlock()
		t.Fatal("a stranger's pull waited for the rooms lock")
	}
	if rs, err := a.st.Refusals(ctx, room); err != nil || len(rs) != 0 {
		t.Fatalf("refusals stored for strangers: %+v %v", rs, err)
	}
}

// F3: a key that is not a member and carries log entries (honestly, only
// a machine its own entries admit) is checked under the rooms lock, so
// such requests share a token bucket for the whole machine (8 after a
// quiet spell, 4 a second); entries that are garbage or do not admit it
// leave nothing, refusal counts included. Requests for rooms not held
// here spend none of it.
func TestRequestsWithEntriesFromKeysThatAreNotMembersShareOneBudgetAndLeaveNothing(t *testing.T) {
	ctx := context.Background()
	a, _, _, room := pairWithHistory(t)
	heads, _ := a.r.rooms[room].logHeads()
	ws, err := a.r.Offer(room)
	if err != nil {
		t.Fatal(err)
	}
	before := exchangeSizes(a.x)
	entries := []json.RawMessage{json.RawMessage(ws[0]), json.RawMessage(`{"not":"an entry"}`)}
	for i := range 100 { // rooms that do not exist here: refused, nothing spent
		q := strangerOpening(fmt.Sprintf("r_%032x", 0xdead00+i), heads)
		q.Log.Entries = entries
		if _, err := a.x.Serve(WithSource(ctx, "198.51.100.1:5000"), junkNode(i), q); !isNotMember(err) {
			t.Fatalf("a room not held: %v", err)
		}
	}
	refused, limited := 0, 0
	for i := range 3 * strangerEntriesBurst {
		q := strangerOpening(room, heads)
		q.Log.Entries = entries
		src := fmt.Sprintf("198.51.100.%d:5000", 10+i) // a source each, so only the machine's bucket binds
		_, err := a.x.Serve(WithSource(ctx, src), junkNode(i), q)
		var pe *PullError
		switch {
		case isNotMember(err):
			refused++
		case errors.As(err, &pe) && pe.Code == CodeRateLimited && pe.RetryAfter > 0:
			limited++
		default:
			t.Fatalf("stranger %d: %v", i, err)
		}
	}
	if refused != strangerEntriesBurst || limited != 2*strangerEntriesBurst {
		t.Fatalf("%d refused, %d rate limited; want %d and %d", refused, limited, strangerEntriesBurst, 2*strangerEntriesBurst)
	}
	if after := exchangeSizes(a.x); after != before {
		t.Fatalf("strangers with entries changed what the exchange keeps from %v to %v", before, after)
	}
	if rs, err := a.st.Refusals(ctx, room); err != nil || len(rs) != 0 {
		t.Fatalf("refusals stored for strangers: %+v %v", rs, err)
	}
	*a.clock = a.clock.Add(time.Second) // the budget refills
	q := strangerOpening(room, heads)
	q.Log.Entries = []json.RawMessage{json.RawMessage(ws[0])}
	if _, err := a.x.Serve(ctx, junkNode(99), q); !isNotMember(err) {
		t.Fatalf("after a second: %v", err)
	}
}

// The honest case of the rule above (T4): a machine whose admission the
// server has not seen yet carries the entries that admit it in its own
// request, and is served as a member at once.
//
// Requirements: T4
func TestAMachineAdmittedByEntriesInItsOwnRequestIsStillServed(t *testing.T) {
	ctx := context.Background()
	a, b, net, room := pairWithHistory(t)
	c := newPNode(t, net, "c", newLNode(t, "c", a.clock))
	a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "cee", History: true})
	c.pin(room, a.lnode)
	c.pull(room, a.lnode)
	if _, m := b.r.memberOf(room, c.id); m {
		t.Fatal("b already knows c")
	}
	if _, err := c.x.Pull(ctx, room, b.id, PullOpts{}); err != nil {
		t.Fatalf("c pulls from b with the entries that admit it: %v", err)
	}
	if _, m := b.r.memberOf(room, c.id); !m {
		t.Fatal("b did not take the entries that admit c")
	}
	b.x.mu.Lock()
	_, heard := b.x.heard[[2]string{room, c.id}]
	b.x.mu.Unlock()
	if !heard {
		t.Fatal("b keeps nothing of c, now a member")
	}
}

// F3, F9: once a member is removed here, the next tidy drops what the
// exchange kept of it in memory, its observed reports and the stream
// hints only its reports kept; a member's own stay.
func TestTidyDropsWhatTheExchangeKeptOfAPeerRemovedHere(t *testing.T) {
	ctx := context.Background()
	a, b, net, room := pairWithHistory(t)
	c := newPNode(t, net, "c", newLNode(t, "c", a.clock))
	a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "cee", History: true})
	c.pin(room, a.lnode)
	c.pull(room, a.lnode)
	b.mustPull(room, a, PullOpts{})
	c.mustPull(room, a, PullOpts{})
	// b reports streams of a that do not exist: hints, on b's word only.
	fake := map[string][]store.StreamDetail{a.id: {{E: "00000000000000f1", C: 3}, {E: "00000000000000f2", C: 3}}}
	if _, err := a.st.Exchange(ctx, room, 0, func(tx *store.Tx) error {
		return observe(tx, room, b.id, a.r.rooms[room].st, detailRows(fake), store.PeerLog{}, a.m.now(), false)
	}); err != nil {
		t.Fatal(err)
	}
	hints := func() int {
		n := 0
		var ss []store.Stream
		if err := a.st.View(ctx, func(rd *store.Reader) (err error) { ss, err = rd.Streams(room); return err }); err != nil {
			t.Fatal(err)
		}
		for _, s := range ss {
			if s.Contig == 0 {
				n++
			}
		}
		return n
	}
	if hints() != 2 {
		t.Fatalf("%d hints, want 2", hints())
	}
	a.x.tidy(ctx)
	if hints() != 2 {
		t.Fatal("a current member's hints were dropped")
	}
	a.change(room, Change{Kind: core.EntryRemove, Target: b.id})
	a.x.tidy(ctx)
	a.x.mu.Lock()
	for _, m := range []any{a.x.heard, a.x.lastEx} {
		switch m := m.(type) {
		case map[[2]string][]string:
			if _, ok := m[[2]string{room, b.id}]; ok {
				t.Error("heard kept for a removed peer")
			}
		case map[[2]string]time.Time:
			if _, ok := m[[2]string{room, b.id}]; ok {
				t.Error("last exchange kept for a removed peer")
			}
			if _, ok := m[[2]string{room, c.id}]; !ok {
				t.Error("a member's last exchange was dropped")
			}
		}
	}
	a.x.mu.Unlock()
	if hints() != 0 {
		t.Fatalf("%d hints of a removed peer kept", hints())
	}
	if _, obs := observedOf(t, a, room, b.id); len(obs) != 0 {
		t.Fatalf("observed reports of a removed peer kept: %+v", obs)
	}
	if _, obs := observedOf(t, a, room, c.id); len(obs) == 0 {
		t.Fatal("a member's observed reports were dropped")
	}
}

// F9: a member that reported hints and then went silent keeps them only
// for hintLife; its next report sets them again.
func TestAHintOfAMemberNotHeardFromForADayAgesOut(t *testing.T) {
	ctx := context.Background()
	a, b, _, room := pairWithHistory(t)
	b.mustPull(room, a, PullOpts{})
	report := func() {
		fake := map[string][]store.StreamDetail{a.id: {{E: "00000000000000f1", C: 3}}}
		if _, err := a.st.Exchange(ctx, room, 0, func(tx *store.Tx) error {
			return observe(tx, room, b.id, a.r.rooms[room].st, detailRows(fake), store.PeerLog{}, a.m.now(), false)
		}); err != nil {
			t.Fatal(err)
		}
	}
	waiting := func() bool {
		var ss []store.Stream
		if err := a.st.View(ctx, func(rd *store.Reader) (err error) { ss, err = rd.Streams(room); return err }); err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(ss, func(s store.Stream) bool { return s.Epoch == "00000000000000f1" })
	}
	report()
	*a.clock = a.clock.Add(hintLife - time.Minute)
	a.x.tidy(ctx)
	if !waiting() {
		t.Fatal("a hint went before hintLife")
	}
	*a.clock = a.clock.Add(2 * time.Minute)
	a.x.tidy(ctx)
	if waiting() {
		t.Fatal("a hint of a member silent for a day is still kept")
	}
	report()
	if !waiting() {
		t.Fatal("its next report did not set the hint again")
	}
}

// F22: a laptop that reaches a room only through its listener is
// refused twice when the listener leaves the room, and muted. When the
// listener holds the room again, nothing on the laptop changes (the
// listener never sends it a request); before, it was muted until its
// daemon restarted. Now the mute runs out after 5 minutes, it asks
// again with the whole log and pulls; a mute after the next refusal
// lasts twice as long.
func TestALaptopMutedByItsOnlyListenerAsksAgainAfterABackOff(t *testing.T) {
	ctx := context.Background()
	a, b, _, room := pairWithHistory(t)
	b.mustPull(room, a, PullOpts{})
	heads, _ := b.r.rooms[room].logHeads()
	if err := a.r.Leave(ctx, room); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := b.pullFrom(room, a, PullOpts{}); !isNotMember(err) {
			t.Fatalf("pull from a listener without the room: %v", err)
		}
	}
	if !b.x.muted(room, a.id, heads) {
		t.Fatal("not muted after two refusals")
	}
	a.pin(room, b.lnode)
	a.pull(room, b.lnode)
	*b.clock = b.clock.Add(muteMin - time.Second)
	if !b.x.muted(room, a.id, heads) {
		t.Fatal("the mute ended early")
	}
	if _, ok := b.x.NotMemberSince(room)[a.id]; !ok {
		t.Fatal("not shown as a peer that may have removed this machine")
	}
	*b.clock = b.clock.Add(2 * time.Second)
	if b.x.muted(room, a.id, heads) {
		t.Fatal("still muted after 5 minutes")
	}
	if _, err := b.pullFrom(room, a, PullOpts{}); err != nil {
		t.Fatalf("after the mute: %v", err)
	}
	if len(b.x.NotMemberSince(room)) != 0 {
		t.Fatal("a success left the refusal shown")
	}
	// The next run of refusals mutes for 5 minutes again; a second mute
	// within one run lasts 10.
	if got := []time.Duration{muteFor(1), muteFor(2), muteFor(3), muteFor(10)}; !slices.Equal(got,
		[]time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, time.Hour}) {
		t.Fatalf("mute lengths %v", got)
	}
}

// F21: removing the one member this machine has exchanged with in the
// last hour is warned of, beside the request's text (which the warning
// does not change); not when another member was reached too, nor once
// none was reached lately.
func TestRemovingTheOnlyMemberReachedLatelyIsWarnedOf(t *testing.T) {
	ctx := context.Background()
	a, b, net, room := pairWithHistory(t)
	c := newPNode(t, net, "c", newLNode(t, "c", a.clock))
	a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "cee", History: true})
	c.pin(room, a.lnode)
	c.pull(room, a.lnode)
	s, err := OpenSurface(ctx, a.m, a.r, SurfaceOpts{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	q, err := a.r.Propose(ctx, room, Change{Kind: core.EntryRemove, Target: b.id})
	if err != nil {
		t.Fatal(err)
	}
	warning := func() string {
		t.Helper()
		v, err := s.Describe(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(v.Text, "only member") {
			t.Fatalf("the warning went into the approved text: %q", v.Text)
		}
		return v.Warning
	}
	if w := warning(); w != "" {
		t.Fatalf("warned before any exchange: %q", w)
	}
	a.mustPull(room, b, PullOpts{})
	w := warning()
	if !strings.Contains(w, Fingerprint(b.id)) || !strings.Contains(w, "bee") || !strings.Contains(w, "only member") {
		t.Fatalf("removing the only member reached: %q", w)
	}
	if !strings.Contains(w, Isolate("bee")) {
		t.Fatalf("the name is not isolated: %q", w)
	}
	a.mustPull(room, c, PullOpts{})
	if w := warning(); w != "" {
		t.Fatalf("warned with another member reached: %q", w)
	}
	*a.clock = a.clock.Add(ReachedFor + time.Minute)
	if w := warning(); w != "" {
		t.Fatalf("warned with no member reached lately: %q", w)
	}
	b.mustPull(room, a, PullOpts{}) // b's request, answered: an exchange too
	if w := warning(); w == "" {
		t.Fatal("not warned after b's request was answered")
	}
}

// F21: the warning names the machine by the room's record of its name,
// escaped once and isolated, as a request's text is, so another machine's
// name cannot rewrite or reorder it; the surfaces show it as they show
// that text, without escaping it again (the review of D71).
func TestTheOnlyReachedWarningEscapesTheNameOnceAndIsolatesIt(t *testing.T) {
	name := "ok\u200f\u202e\x1b[2K"
	w := OnlyReachedWarning(wNode, name)
	if strings.ContainsAny(w, "\x1b\u202e\u200f") || !strings.Contains(w, Fingerprint(wNode)) {
		t.Fatalf("%q", w)
	}
	if !strings.Contains(w, Isolate(EscapeDisplay(name))) || strings.Count(w, "\u2068") != 1 {
		t.Fatalf("the name is not escaped once and isolated: %q", w)
	}
	parts := Isolated(w)
	if len(parts) != 3 || !parts[1].Isolated || parts[1].Text != EscapeDisplay(name) {
		t.Fatalf("isolated parts %+v", parts)
	}
}

// F5, F54: what this machine keeps and dials on another machine's word.
// Private ranges (LAN, VPN and overlay networks) and names stay; never
// loopback, whoever gives it, link-local, unspecified, multicast or
// broadcast addresses, IPv6 forms that embed one of those (NAT64, 6to4)
// or any IPv4 address (IPv4-compatible, IPv4-translated), well-known
// ports other than 443, localhost, or a name that is a number.
func TestTheAddressRuleForAddressesFromOtherMachines(t *testing.T) {
	ok := []string{
		"192.168.1.5:7423", "10.0.0.5:7423", "172.16.4.2:7423", // LAN
		"100.101.102.103:7423", "[fd7a:115c:a1e0::1]:7423", // overlay (CGNAT range, unique-local IPv6)
		"198.51.100.7:40001", "[2001:db8::1]:7423", "203.0.113.9:443",
		"laptop.tail1234.ts.net:7423", "hub.example.com:443", "macbook.local:7423",
		"[64:ff9b::c633:6407]:7423", "[2002:c633:6407::1]:7423", // NAT64 and 6to4 of a public address
	}
	bad := []string{
		"127.0.0.1:7423", "127.9.9.9:7423", "[::1]:7423", "[::ffff:127.0.0.1]:7423",
		"169.254.169.254:80", "169.254.1.1:7423", "[fe80::1]:7423", "[fe80::1%25en0]:7423",
		"0.0.0.0:7423", "0.1.2.3:7423", "[::]:7423", "224.0.0.1:7423", "[ff02::1]:7423", "255.255.255.255:7423",
		"10.0.0.1:445", "192.168.1.1:22", "10.0.0.1:80", "10.0.0.1:0", "10.0.0.1:65536",
		"localhost:7423", "a.localhost:7423", "127.1:7423", "0x7f.1:7423", "2130706433:7423",
		"", "nohost", ":7423", "a b:7423", strings.Repeat("a", 300) + ":7423",
		// IPv6 forms that embed an IPv4 address this rule refuses
		"[64:ff9b::7f00:1]:7423", "[64:ff9b::a9fe:a9fe]:7423", "[64:ff9b::a00:1]:445",
		"[64:ff9b:1::7f00:1]:7423", "[64:ff9b:1::c633:6407]:7423", // the local-use NAT64 prefix: refused whole
		"[2002:7f00:1::1]:7423", "[2002:a9fe:a9fe::1]:7423", "[2002:0:1::1]:7423",
		"[::127.0.0.1]:7423", "[::7f00:1]:7423", "[::c633:6407]:7423", // IPv4-compatible
		"[::ffff:0:7f00:1]:7423", "[::ffff:0:c633:6407]:7423", // IPv4-translated
		"[::ffff:169.254.169.254]:7423", "[::ffff:0.0.0.0]:7423",
	}
	for _, a := range ok {
		if why := AddrRefusal(a); why != "" {
			t.Errorf("%q refused: %s", a, why)
		}
	}
	for _, a := range bad {
		if AddrRefusal(a) == "" {
			t.Errorf("%q accepted", a)
		}
	}
	if !strings.Contains(AddrRefusal("127.0.0.1:7423"), "loopback") || !strings.Contains(AddrRefusal("10.0.0.1:22"), "port") {
		t.Error("the reasons do not say why")
	}
}

// F5, F54: a member's own word and a third party's go through the rule
// as they reach the book; an invite's (a person's) does not, and an empty
// result still replaces what the member said of itself before.
func TestAddressesFromOtherMachinesAreFilteredAsTheyReachTheBook(t *testing.T) {
	b := NewAddrBook()
	b.Heard(wNode, []string{"127.0.0.1:22", "192.168.1.5:7423", "evil.example:22", "laptop.ts.net:7423"}, bookT0)
	if got := b.Addrs(wNode); !slices.Equal(got, []string{"192.168.1.5:7423", "laptop.ts.net:7423"}) {
		t.Fatalf("its own word: %v", got)
	}
	other := wNode[:63] + "e"
	b.Listed(other, []string{"[::1]:7423", "169.254.169.254:80", "10.1.2.3:7423"}, bookT0)
	if got := b.Addrs(other); !slices.Equal(got, []string{"10.1.2.3:7423"}) {
		t.Fatalf("a third party's word: %v", got)
	}
	if given, why := b.Rule(other, "10.1.2.3:7423"); given || why != "" {
		t.Fatalf("a third party's LAN address: given %v, refused %q", given, why)
	}
	b.Learn(other, []string{"127.0.0.1:7424"}, bookT0) // an invite pasted here
	if given, why := b.Rule(other, "127.0.0.1:7424"); !given || why != "" {
		t.Fatal("an invite's address is not given")
	}
	b.Heard(wNode, []string{"127.0.0.1:22"}, bookT0.Add(time.Hour))
	if got := b.Addrs(wNode); len(got) != 0 {
		t.Fatalf("a refused list did not replace its own earlier one: %v", got)
	}
	// Loopback on another machine's word is never kept, however it came:
	// behind a tunnel or proxy on this machine every member's request
	// arrives over loopback.
	b.Heard(wNode, []string{"127.0.0.1:7423", "[::1]:7423"}, bookT0.Add(2*time.Hour))
	third := wNode[:63] + "f"
	b.Listed(third, []string{"127.0.0.1:7425"}, bookT0)
	if len(b.Addrs(wNode)) != 0 || len(b.Addrs(third)) != 0 {
		t.Fatalf("loopback kept: %v %v", b.Addrs(wNode), b.Addrs(third))
	}
	if ps := b.peers([]string{other}); len(ps) != 1 || !slices.Equal(ps[0].Addrs, []string{"10.1.2.3:7423"}) {
		t.Fatalf("listed to others: %+v", ps)
	}
}

// F5, F54 over the exchange: a member's request and a response's peers
// that name loopback, link-local and well-known ports leave only the
// addresses the rule accepts; a LAN address and a name stay.
func TestAMembersAddressesAndThePeersItListsAreFilteredInTheExchange(t *testing.T) {
	clock := lclock()
	net := newMemNet()
	a := newPNode(t, net, "a", newLNode(t, "a", clock))
	b := newPNode(t, net, "b", newLNode(t, "b", clock))
	c := newPNode(t, net, "c", newLNode(t, "c", clock))
	for _, p := range []*pnode{a, b, c} {
		p.opts.Book = NewAddrBook()
		p.attach()
	}
	b.opts.Addrs = []string{"127.0.0.1:22", "169.254.169.254:80", "10.0.0.7:7423", "bee.example:7423"}
	b.attach()
	room := a.create("team")
	a.change(room, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee", History: true})
	a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "cee", History: true})
	for _, p := range []*pnode{b, c} {
		p.pin(room, a.lnode)
		p.pull(room, a.lnode)
	}
	b.mustPull(room, a, PullOpts{})
	if got := a.opts.Book.Addrs(b.id); !slices.Equal(got, []string{"10.0.0.7:7423", "bee.example:7423"}) {
		t.Fatalf("a kept %v of b's own word", got)
	}
	net.tamper = func(from, _ string, resp *PullResponse) { // a lists b at addresses of its choosing
		if from == c.id && resp.Peers != nil {
			resp.Peers = []PullPeer{{Node: b.id, Addrs: []string{"127.0.0.1:7423", "[fe80::1]:7423", "10.9.9.9:139", "10.9.9.9:7423"}}}
		}
	}
	c.mustPull(room, a, PullOpts{})
	if got := c.opts.Book.Addrs(b.id); !slices.Equal(got, []string{"10.9.9.9:7423"}) {
		t.Fatalf("c kept %v of a's word on b", got)
	}
}

// F5, F54: an address another machine gave is dialled only as literal
// addresses; a name is looked up once, every address it gives is checked,
// and only those that pass are dialled, so a name cannot pass the check
// as one address and be dialled as another (DNS rebinding).
func TestANameFromAnotherMachineIsLookedUpOnceAndOnlyAddressesThatPassAreDialled(t *testing.T) {
	ctx := context.Background()
	lookups := 0
	answer := []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("169.254.169.254"),
		netip.MustParseAddr("192.168.1.9"), netip.MustParseAddr("::ffff:10.0.0.3")}
	resolve := func(_ context.Context, host string) ([]netip.Addr, error) {
		lookups++
		return answer, nil
	}
	got, err := DialTargets(ctx, "hub.example:7423", false, resolve)
	if err != nil || lookups != 1 || !slices.Equal(got, []string{"192.168.1.9:7423", "10.0.0.3:7423"}) {
		t.Fatalf("%v %v after %d lookups", got, err, lookups)
	}
	answer = []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("64:ff9b::7f00:1")}
	if _, err := DialTargets(ctx, "hub.example:7423", false, resolve); !errors.Is(err, ErrAddrRefused) {
		t.Fatalf("a name that resolves to loopback: %v", err)
	}
	n := lookups
	if _, err := DialTargets(ctx, "evil.example:22", false, resolve); !errors.Is(err, ErrAddrRefused) || lookups != n {
		t.Fatalf("a refused port was looked up: %v", err)
	}
	if got, err := DialTargets(ctx, "localhost:7423", true, resolve); err != nil || !slices.Equal(got, []string{"localhost:7423"}) || lookups != n {
		t.Fatalf("an invite's address: %v %v", got, err)
	}
}

// The stored book a past version wrote may hold addresses the rule
// refuses. They load (the daemon starts, the room works), count as
// another machine's word, and so are not dialled until a person's invite
// gives them again.
func TestStoredAddressesTheRuleRefusesLoadAndCountAsAnotherMachinesWord(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/a2p2.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetPeerAddrs(ctx, wNode, []store.PeerAddr{
		{Node: wNode, Addr: "127.0.0.1:22", Source: store.AddrSelf, Seen: bookT0},
		{Node: wNode, Addr: "192.168.1.5:7423", Source: store.AddrPeer, Seen: bookT0},
	}); err != nil {
		t.Fatal(err)
	}
	b, err := LoadAddrBook(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Addrs(wNode); len(got) != 2 {
		t.Fatalf("loaded %v", got)
	}
	for _, a := range b.Addrs(wNode) {
		given, why := b.Rule(wNode, a)
		if (why == "") != (a == "192.168.1.5:7423") {
			t.Errorf("%s: refused %q", a, why)
		}
		if _, err := DialTargets(ctx, a, given, nil); (err == nil) != (a == "192.168.1.5:7423") {
			t.Errorf("%s: %v", a, err)
		}
	}
	if h := b.Held(wNode); len(h) != 1 || h[0].Addr != "127.0.0.1:22" || h[0].Why == "" {
		t.Fatalf("held and not dialled: %+v", h)
	}
	// A completed pinned handshake (Good) never overrides the rule: a
	// verified loopback literal is refused and held like any other, and
	// a verified name is still looked up and checked.
	vNode := strings.Repeat("d", 64)
	if err := st.SetPeerAddrs(ctx, vNode, []store.PeerAddr{
		{Node: vNode, Addr: "127.0.0.1:7423", Source: store.AddrSelf, Seen: bookT0, Good: true},
		{Node: vNode, Addr: "hub.example:7423", Source: store.AddrSelf, Seen: bookT0, Good: true},
	}); err != nil {
		t.Fatal(err)
	}
	if b, err = LoadAddrBook(ctx, st); err != nil {
		t.Fatal(err)
	}
	if given, why := b.Rule(vNode, "127.0.0.1:7423"); given || why == "" {
		t.Fatal("a verified loopback address is dialled after a restart")
	}
	if h := b.Held(vNode); len(h) != 1 || h[0].Addr != "127.0.0.1:7423" {
		t.Fatalf("held for the verified rows: %+v", h)
	}
	if given, _ := b.Rule(vNode, "hub.example:7423"); given {
		t.Fatal("a verified name is dialled unchecked")
	}
	b.Learn(wNode, []string{"127.0.0.1:22"}, time.Time{}) // the invite the person accepted names it
	if given, why := b.Rule(wNode, "127.0.0.1:22"); !given || why != "" {
		t.Fatal("given again by an invite, still not dialled")
	}
	if h := b.Held(wNode); len(h) != 0 {
		t.Fatalf("held after the invite: %+v", h)
	}
}

// Many members, all honest, on one LAN: every member's own LAN address is
// kept by the listener and listed to the others.
func TestAListenerOfSixtyFourMembersOnALANKeepsAndListsEveryMembersAddress(t *testing.T) {
	b := NewAddrBook()
	var nodes []string
	for i := range 64 {
		n := fmt.Sprintf("%064x", 0xbeef00+i)
		nodes = append(nodes, n)
		b.Heard(n, []string{fmt.Sprintf("192.168.%d.%d:7423", i/200, 10+i%200)}, bookT0)
	}
	if ps := b.peers(nodes); len(ps) != MaxPeersListed {
		t.Fatalf("%d of 64 members listed", len(ps))
	}
}
