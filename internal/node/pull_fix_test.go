package node

// Tests of the step 5a fix round (docs/research/decisions/step5a-review-sol.md
// and the coordinator's rulings): release rebuilds, a restored server's
// catch-up, wire budgets, convergence, response validation, observation
// order, the textless floor, forked streams and the short first request.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

func pairWithHistory(t *testing.T) (a, b *pnode, net *memNet, room string) {
	t.Helper()
	clock := lclock()
	net = newMemNet()
	a = newPNode(t, net, "a", newLNode(t, "a", clock))
	b = newPNode(t, net, "b", newLNode(t, "b", clock))
	room = a.create("team")
	a.change(room, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee", History: true})
	b.pin(room, a.lnode)
	b.pull(room, a.lnode)
	a.register("alice")
	b.register("bob")
	return a, b, net, room
}

// onDetail runs fn once, at release, the first time a response carrying a
// page (to a detail request) from x to peer is released.
func onDetail(x *Exchange, peer string, fn func(room string)) *int {
	n := 0
	x.atRelease = func(room, p string, q *PullRequest) {
		if p == peer && q.Detail != nil && n == 0 {
			n++
			fn(room)
		}
	}
	return &n
}

// ---- item 1: release rebuilds ----

// [RM2] A message withdrawn after the page was built and before it is
// released does not go out with text: the response is built again.
func TestAMessageWithdrawnBetweenBuildAndReleaseIsNotSent(t *testing.T) {
	a, b, net, room := pairWithHistory(t)
	id := a.send(room, "alice", "secret")
	fired := onDetail(a.x, b.id, func(room string) {
		if _, err := a.m.Withdraw(context.Background(), room, "alice", id); err != nil {
			t.Fatal(err)
		}
	})
	b.mustPull(room, a, PullOpts{})
	if *fired != 1 {
		t.Fatal("no data page was built")
	}
	if slices.Contains(net.pageText, id) {
		t.Fatal("the withdrawn text was released")
	}
	if h, ok := b.held(room, id); ok && h.Record.Body() != nil {
		t.Fatal("the peer holds the withdrawn text")
	}
}

// [RM1, M13] An author voided (out of Admitted) after the page was built
// and before it is released: none of its records go out.
func TestAnAuthorVoidedBetweenBuildAndReleaseIsNotSent(t *testing.T) {
	al, bl, room := twoAdmins(t)
	net := newMemNet()
	a := newPNode(t, net, "a", al)
	d := newPNode(t, net, "d", newLNode(t, "d", al.clock))
	a.change(room, Change{Kind: core.EntryAdd, Target: d.id, Name: "dee", History: true})
	d.pin(room, al)
	d.pull(room, al)
	bl.pull(room, al)
	c := newLNode(t, "c", al.clock)
	a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "sea", History: true})
	c.pin(room, al)
	c.pull(room, al)
	c.register("carol")
	msg := c.send(room, "carol", "from c")
	for _, r := range c.records(room) {
		a.take(r)
	}
	fired := onDetail(a.x, d.id, func(room string) {
		a.change(room, Change{Kind: core.EntryRemove, Target: c.id})
		bl.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "sea"})
		bl.change(room, Change{Kind: core.EntryRemove, Target: c.id})
		al.pull(room, bl)
		if slices.Contains(a.state(room).Admitted, c.id) {
			t.Fatal("c is still admitted")
		}
	})
	_, _ = d.pullFrom(room, a, PullOpts{})
	if *fired != 1 {
		t.Fatal("no data page was built")
	}
	if _, ok := d.held(room, msg); ok {
		t.Fatal("a record of the voided author was released")
	}
}

// [RM2] The server leaves the room and joins it again between build and
// release: nothing built in the old generation is released.
func TestALeaveAndRejoinBetweenBuildAndReleaseReleasesNothingOfTheOldGeneration(t *testing.T) {
	a, b, _, room := pairWithHistory(t)
	id := a.send(room, "alice", "x")
	create := a.r.rooms[room].ents[a.r.rooms[room].createID].Wire()
	fired := onDetail(a.x, b.id, func(room string) {
		if err := a.r.Leave(context.Background(), room); err != nil {
			t.Fatal(err)
		}
		if _, err := a.r.PinRoom(context.Background(), create); err != nil {
			t.Fatal(err)
		}
	})
	_, err := b.pullFrom(room, a, PullOpts{})
	var pe *PullError
	if *fired != 1 || !errors.As(err, &pe) || pe.Code != CodeUnavailable {
		t.Fatalf("pull: fired %d, %v", *fired, err)
	}
	if _, ok := b.held(room, id); ok {
		t.Fatal("a record built in the old generation was released")
	}
}

// ---- item 2: a restored server and a requester's admission ----

// junkNode is a valid node id that no machine holds.
func junkNode(i int) string { return fmt.Sprintf("%064x", 0xa2b2c3d400+i) }

// [T4; stated limit] A server that knows the room only as it was before a
// peer's admission, more than 256 entries ago (restored from an old copy,
// or asleep), refuses that peer: the peer's entries do not make it a
// member in one batch. The peer is not asked again while the server's log
// heads stay as they were. The server catches up by pulling the log, in
// several log.more pages, from a member it knows, and then serves the
// peer.
func TestARestoredServerPagesTheLogFromAMemberThenServesANewPeer(t *testing.T) {
	clock := lclock()
	net := newMemNet()
	a := newPNode(t, net, "a", newLNode(t, "a", clock))
	s := newPNode(t, net, "s", newLNode(t, "s", clock))
	n := newPNode(t, net, "n", newLNode(t, "n", clock))
	room := a.create("team")
	a.change(room, Change{Kind: core.EntryAdd, Target: s.id, Name: "ess", History: true})
	s.pin(room, a.lnode)
	s.pull(room, a.lnode)
	for i := 0; i < 140; i++ { // 280 entries s does not know
		a.change(room, Change{Kind: core.EntryAdd, Target: junkNode(i), Name: "j", History: true})
		a.change(room, Change{Kind: core.EntryRemove, Target: junkNode(i)})
	}
	a.change(room, Change{Kind: core.EntryAdd, Target: n.id, Name: "en", History: true})
	n.pin(room, a.lnode)
	n.pull(room, a.lnode)

	for i := 0; i < 3; i++ {
		r0 := len(net.reqBytes)
		_, err := n.pullFrom(room, s, PullOpts{})
		var pe *PullError
		if !errors.As(err, &pe) || pe.Code != CodeNotMember {
			t.Fatalf("pull %d from the stale server: %v", i, err)
		}
		if got := len(net.reqBytes) - r0; got > 2 || i > 0 && got != 1 {
			t.Fatalf("pull %d from the stale server: %d requests", i, got)
		}
	}
	l := capture(net)
	s.mustPull(room, a, PullOpts{})
	more := 0
	for _, r := range l.resps {
		if r.Log.More {
			more++
		}
	}
	if more < 1 || !slices.Contains(s.state(room).Members, n.id) {
		t.Fatalf("the server did not page the log: %d cut responses, members %v", more, s.state(room).Members)
	}
	net.lose = nil
	n.mustPull(room, s, PullOpts{})
}

// ---- item 3: wire budgets ----

// [N1] With a small response limit, log entries are cut by bytes as well
// as count and continued with log.more; every response fits; the logs
// converge.
func TestALogBatchIsCutByBytesAndContinued(t *testing.T) {
	a, b, net, room := pairWithHistory(t)
	for i := 0; i < 30; i++ {
		a.change(room, Change{Kind: core.EntryAdd, Target: junkNode(i), Name: "j", History: true})
	}
	defer func(v int) { maxResponseBytes = v }(maxResponseBytes)
	maxResponseBytes = 8 << 10
	l := capture(net)
	b.mustPull(room, a, PullOpts{})
	cut := false
	for _, r := range l.resps {
		raw, _ := json.Marshal(r)
		if len(raw) > maxResponseBytes {
			t.Fatalf("response of %d bytes over the limit", len(raw))
		}
		cut = cut || r.Log.More
	}
	ah, _ := a.r.rooms[room].logHeads()
	bh, _ := b.r.rooms[room].logHeads()
	if !cut || !sameSet(ah, bh) {
		t.Fatalf("cut %v, heads %v and %v", cut, ah, bh)
	}
}

// [N1; 4.3 item 4] An author whose detail alone cannot fit in a request is
// left out of the detail requests and counted as too_large; the exchange
// does not fail.
func TestAnAuthorWhoseDetailCannotFitIsSkippedAsTooLarge(t *testing.T) {
	a, b, _, room := pairWithHistory(t)
	for i := 0; i < 12; i++ { // a stream per start
		a.send(room, "alice", "x")
		a.restartP()
	}
	b.mustPull(room, a, PullOpts{})
	a.send(room, "alice", "y")
	defer func(v int) { maxRequestBytes = v }(maxRequestBytes)
	maxRequestBytes = 4*wireMargin + 900
	st, err := b.pullFrom(room, a, PullOpts{FullForm: true})
	if err != nil || st.TooLarge == 0 {
		t.Fatalf("pull: %+v %v", st, err)
	}
	rs, err := b.st.Refusals(context.Background(), room)
	if err != nil || !slices.ContainsFunc(rs, func(r store.Refusal) bool { return r.Reason == CodeTooLarge && r.Peer == a.id }) {
		t.Fatalf("refusals: %+v %v", rs, err)
	}
}

// ---- item 4: cuts and convergence ----

// [R4, N1] A stream whose next record is refused (dated too far ahead)
// does not hold back the other streams of the same detail chunk: they are
// fetched to the end, a page at a time.
func TestABlockedStreamDoesNotHoldBackTheOthersInItsChunk(t *testing.T) {
	clock, bclock := lclock(), lclock()
	net := newMemNet()
	a := newPNode(t, net, "a", newLNode(t, "a", clock))
	b := newPNode(t, net, "b", newLNode(t, "b", bclock))
	room := a.create("team")
	// The blocked author sorts before a, so its stream comes first in
	// every page.
	name := ""
	for i := 0; name == ""; i++ {
		seed := sha256.Sum256([]byte(fmt.Sprintf("rooms test c%d", i)))
		if hex.EncodeToString(ed25519.NewKeyFromSeed(seed[:]).Public().(ed25519.PublicKey)) < a.id {
			name = fmt.Sprintf("c%d", i)
		}
	}
	ahead := newLNode(t, name, clock)
	for _, n := range []*lnode{b.lnode, ahead} {
		a.change(room, Change{Kind: core.EntryAdd, Target: n.id, Name: "x", History: true})
	}
	for _, n := range []*lnode{b.lnode, ahead} {
		n.pin(room, a.lnode)
		n.pull(room, a.lnode)
	}
	a.register("alice")
	var ids []string
	for i := 0; i < 4; i++ {
		ids = append(ids, a.send(room, "alice", "now"))
	}
	*clock = clock.Add(30 * time.Hour) // more than 24 h ahead of b
	ahead.register("carol")
	ahead.send(room, "carol", "from the future")
	for _, r := range ahead.records(room) {
		if out := a.take(r); out != store.OutcomeStored {
			t.Fatalf("a takes %s: %s", r.ID(), out)
		}
	}
	a.x.pageLimit = 1
	st, err := b.pullFrom(room, a, PullOpts{FullForm: true})
	if err != nil {
		t.Fatalf("pull: %+v %v", st, err)
	}
	for _, id := range ids {
		if _, ok := b.held(room, id); !ok {
			t.Fatalf("a's stream was held back by the refused one: %+v", st)
		}
	}
}

// [N1] A peer that changes on every round reaches the round cap: counted,
// not an error.
func TestReachingTheRoundCapIsCountedNotAnError(t *testing.T) {
	a, b, net, room := pairWithHistory(t)
	a.send(room, "alice", "0")
	net.tamper = func(_, _ string, r *PullResponse) {
		if r.Detail != nil {
			a.send(room, "alice", "more") // after the response was built
		}
	}
	st, err := b.pullFrom(room, a, PullOpts{FullForm: true})
	if err != nil || !st.Capped || st.Rounds != pullRoundsPerPull {
		t.Fatalf("pull: %+v %v", st, err)
	}
}

// [N1] A response that says more was cut, but whose continuation brings
// nothing new, ends the exchange quietly, counted as stalled.
func TestAContinuationThatDoesNotAdvanceEndsQuietly(t *testing.T) {
	a, b, net, room := pairWithHistory(t)
	a.send(room, "alice", "x")
	b.mustPull(room, a, PullOpts{})
	a.send(room, "alice", "y")
	net.tamper = func(_, _ string, r *PullResponse) {
		if r.Detail != nil {
			r.Page, r.More = nil, true
		}
	}
	st, err := b.pullFrom(room, a, PullOpts{FullForm: true})
	if err != nil || st.Stalled == 0 {
		t.Fatalf("pull: %+v %v", st, err)
	}
}

// ---- item 5: response validation ----

// [R5, N1] A response that does not fit its request is refused before
// anything of it is applied.
func TestAResponseThatDoesNotFitItsRequestIsRefusedBeforeAnythingIsApplied(t *testing.T) {
	cases := map[string]func(q *PullRequest, r *PullResponse){
		"page without detail asked": func(q *PullRequest, r *PullResponse) {
			if q.Detail == nil {
				r.Page = append(r.Page, json.RawMessage(`{}`))
			}
		},
		"detail without detail asked": func(q *PullRequest, r *PullResponse) {
			if q.Detail == nil {
				r.Detail = map[string][]store.StreamDetail{}
			}
		},
		"differ not computed from the digests": func(q *PullRequest, r *PullResponse) {
			if q.Detail == nil {
				r.Differ = nil
			}
		},
		"detail of an author not asked": func(q *PullRequest, r *PullResponse) {
			if q.Detail != nil {
				r.Detail[junkNode(1)] = []store.StreamDetail{}
			}
		},
		"detail inconsistent with the digest": func(q *PullRequest, r *PullResponse) {
			for a, d := range r.Detail {
				if len(d) > 0 {
					d[0].C++
					r.Detail[a] = d
				}
			}
		},
		"too large": func(q *PullRequest, r *PullResponse) {
			if q.Detail != nil {
				r.Peers = nil
				r.Log.Extra = nil
				r.Snap = ""
				big := strings.Repeat("a", maxResponseBytes)
				r.Page = append(r.Page, json.RawMessage(`"`+big+`"`))
			}
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			a, b, net, room := pairWithHistory(t)
			id := a.send(room, "alice", "x")
			var q *PullRequest
			net.lose = func(_, _ string, req *PullRequest, _ *PullResponse) bool { q = req; return false }
			net.tamper = func(_, _ string, r *PullResponse) { tamper(q, r) }
			if _, err := b.pullFrom(room, a, PullOpts{FullForm: true}); err == nil {
				t.Fatal("accepted")
			}
			if _, ok := b.held(room, id); ok {
				t.Fatal("something of it was applied")
			}
		})
	}
}

// [R5] Known positions are written to stream rows only for authors this
// machine admits: a requester's detail for an author it invents leaves
// nothing behind.
func TestKnownPositionsAreNotedOnlyForAdmittedAuthors(t *testing.T) {
	a, b, _, room := pairWithHistory(t)
	fake := junkNode(7)
	d := []store.StreamDetail{{E: "0123456789abcdef", C: 9}}
	q := &PullRequest{V: PullVersion, Room: room, Authors: []string{a.id, b.id, fake},
		Digests: map[string]string{fake: digestOf(d)}, Snap: snapOf(map[string]string{fake: digestOf(d)}),
		Detail: map[string][]store.StreamDetail{fake: d}}
	q.Log.Heads, q.Log.Extra = a.r.rooms[room].logHeads()
	if _, err := a.x.Serve(context.Background(), b.id, q); err != nil {
		t.Fatal(err)
	}
	if err := a.st.View(context.Background(), func(rd *store.Reader) error {
		ss, err := rd.Streams(room)
		for _, s := range ss {
			if s.Author == fake {
				t.Fatalf("a stream row for an author not admitted: %+v", s)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// [R5] A record refused is counted against the peer that sent it, in a
// page (on the requester) and in an offer (on the server).
func TestRefusedRecordsAreCountedOnBothPaths(t *testing.T) {
	a, b, net, room := pairWithHistory(t)
	a.send(room, "alice", "x")
	b.send(room, "bob", "y")
	b.mustPull(room, a, PullOpts{})
	a.mustPull(room, b, PullOpts{})
	a.send(room, "alice", "x2")
	b.send(room, "bob", "y2")
	strip := func(ws []json.RawMessage) {
		for i, w := range ws {
			var m map[string]any
			_ = json.Unmarshal(w, &m)
			m["sig"] = strings.Repeat("A", 86)
			ws[i], _ = json.Marshal(m)
		}
	}
	a.change(room, Change{Kind: core.EntryAdd, Target: junkNode(3), Name: "j", History: true})
	net.tamper = func(_, _ string, r *PullResponse) { strip(r.Page); strip(r.Log.Entries) }
	_, _ = b.pullFrom(room, a, PullOpts{FullForm: true})
	if rs, _ := b.st.Refusals(context.Background(), room); !slices.ContainsFunc(rs, func(r store.Refusal) bool {
		return r.Peer == a.id && strings.HasPrefix(r.Reason, "entry_")
	}) {
		t.Fatalf("invalid entries not counted: %+v", rs)
	}
	net.tamper = nil
	a.x.atRelease = nil
	b2 := &tamperLink{PullTransport: b.net.link(b.id), fn: func(q *PullRequest) { strip(q.Offer) }}
	b.x.t = b2
	_, _ = b.pullFrom(room, a, PullOpts{Offer: true})
	for _, c := range []struct {
		n    *pnode
		peer string
	}{{b, a.id}, {a, b.id}} {
		rs, err := c.n.st.Refusals(context.Background(), room)
		if err != nil || !slices.ContainsFunc(rs, func(r store.Refusal) bool { return r.Peer == c.peer && r.Count > 0 }) {
			t.Fatalf("%s's refusals: %+v %v", c.n.name, rs, err)
		}
	}
}

type tamperLink struct {
	PullTransport
	fn func(*PullRequest)
}

func (l *tamperLink) Pull(ctx context.Context, peer string, q *PullRequest) (*PullResponse, error) {
	c := *q
	c.Offer = slices.Clone(q.Offer)
	l.fn(&c)
	return l.PullTransport.Pull(ctx, peer, &c)
}

// ---- item 7: frontier seeding ----

// [U3, R8] An agent's cursor for an author is seeded only once every head
// the admission names for that author is held; until then the author's
// streams are held back.
func TestACursorIsSeededOnlyWhenEveryHeadTheAdmissionNamesIsHeld(t *testing.T) {
	a, b, room := twoAdmins(t)
	c := newLNode(t, "c", a.clock)
	a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "sea", History: true})
	c.pin(room, a)
	c.pull(room, a)
	b.pull(room, a)
	c.register("carol")
	recs := map[string]*core.VerifiedRecord{}
	c1 := c.send(room, "carol", "one")
	c2 := c.send(room, "carol", "two")
	for _, r := range c.records(room) {
		recs[r.ID()] = r
	}
	a.take(recs[c1])
	b.take(recs[c1])
	b.take(recs[c2])
	// Two admissions of n, concurrent: a names c1 as c's head, b names c2.
	n := newLNode(t, "n", a.clock)
	a.change(room, Change{Kind: core.EntryAdd, Target: n.id, Name: "en"})
	b.change(room, Change{Kind: core.EntryAdd, Target: n.id, Name: "en"})
	a.pull(room, b)
	if from := a.state(room).Permissions[n.id].From[c.id]; len(from) != 2 {
		t.Fatalf("the admissions name %v of c", from)
	}
	n.pin(room, a)
	n.pull(room, a)
	n.register("nina")
	n.take(recs[c1])
	for _, cs := range cursors(t, n, "nina", room) {
		if cs.Author == c.id {
			t.Fatalf("seeded from some of the heads: %+v", cs)
		}
	}
	n.take(recs[c2])
	c3 := c.send(room, "carol", "three")
	for _, r := range c.records(room) {
		if r.ID() == c3 {
			n.take(r)
		}
	}
	var ids []string
	for _, e := range n.inbox(room, "nina") {
		ids = append(ids, e.ID)
	}
	if !slices.Equal(ids, []string{c3}) {
		t.Fatalf("inbox once every head is held: %v (c1 %s c2 %s c3 %s)", ids, c1, c2, c3)
	}
}

// ---- item 8: observation order ----

// [R5] A response to a request built earlier, applied after one built
// later, does not replace the later observation.
func TestAnOlderObservationNeverReplacesALaterOne(t *testing.T) {
	a, b, _, room := pairWithHistory(t)
	ctx := context.Background()
	gen, _ := b.r.Gen(room)
	build := func() (prepared, *PullResponse) {
		p, err := b.x.prepare(ctx, room, a.id, gen, formFirst, nil, "", false, nil, map[string]bool{}, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := a.x.Serve(ctx, b.id, p.req)
		if err != nil {
			t.Fatal(err)
		}
		return p, resp
	}
	p1, r1 := build()
	b.tick()
	a.send(room, "alice", "between")
	p2, r2 := build()
	if _, err := b.x.apply(ctx, room, a.id, gen, p2, r2); err != nil {
		t.Fatal(err)
	}
	if _, err := b.x.apply(ctx, room, a.id, gen, p1, r1); err != nil {
		t.Fatal(err)
	}
	var obs []store.Observation
	if err := b.st.View(ctx, func(rd *store.Reader) (err error) {
		obs, err = rd.Observed(room, a.id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, o := range obs {
		if o.Author == a.id && (o.Digest != r2.Digests[a.id] || !o.At.Equal(p2.at)) {
			t.Fatalf("the older observation replaced the later: %+v", o)
		}
	}
}

// ---- item 10: the textless floor ----

// [N1; 4.3] A stream's detail carries the floor x: positions at or below
// it hold no text and are not wanted; nb and w list only what is above.
// The digest line includes x.
func TestStreamDetailCarriesTheTextlessFloor(t *testing.T) {
	d := streamDetail("e", 9, []int64{1, 2, 3, 5, 7}, []int64{7})
	if d.X != 3 || !slices.Equal(d.NB, []int64{5, 7}) || !slices.Equal(d.W, []int64{7}) {
		t.Fatalf("detail: %+v", d)
	}
	if all := streamDetail("e", 4, []int64{1, 2, 3, 4}, nil); all.X != 4 || all.NB != nil {
		t.Fatalf("fully textless: %+v", all)
	}
	if w := streamDetail("e", 4, []int64{1, 2}, []int64{1}); w.X != 0 || !slices.Equal(w.NB, []int64{1, 2}) {
		t.Fatalf("wanted first: %+v", w)
	}
	if got := digestOf([]store.StreamDetail{d}); got != digestOf([]store.StreamDetail{{E: "e", C: 9, X: 3, NB: []int64{5, 7}, W: []int64{7}}}) {
		t.Fatal("digest")
	}
	raw, _ := json.Marshal(store.StreamDetail{E: "e", C: 1})
	if !strings.Contains(string(raw), `"x":0`) {
		t.Fatalf("x not always present: %s", raw)
	}
	for _, bad := range []store.StreamDetail{{E: "e", C: 2, X: 3}, {E: "e", C: 5, X: 2, NB: []int64{2}}, {E: "e", C: 5, X: -1}} {
		if checkDetail([]store.StreamDetail{bad}) == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

// ---- item 11: forked streams (known gap) ----

// Known gap (stated limit, step 5a fix item 11): two records by one
// author at one position, which only a faulty or dishonest author machine
// writes (a restore starts a new stream), look equal in digests: two
// holders of different branches see equal summaries, exchange nothing,
// and each reports the other as holding its own record.
func TestKnownGapForkedStreamsLookEqualAndGiveAWrongCopyReport(t *testing.T) {
	clock := lclock()
	net := newMemNet()
	a1 := newLNode(t, "a", clock)
	room := a1.create("team")
	b := newPNode(t, net, "b", newLNode(t, "b", clock))
	c := newPNode(t, net, "c", newLNode(t, "c", clock))
	for _, n := range []*pnode{b, c} {
		a1.change(room, Change{Kind: core.EntryAdd, Target: n.id, Name: n.name, History: true})
	}
	for _, n := range []*pnode{b, c} {
		n.pin(room, a1)
		n.pull(room, a1)
	}
	a1.register("alice")
	x := a1.send(room, "alice", "branch one")
	for _, r := range a1.records(room) {
		if r.ID() != x {
			continue
		}
		b.take(r)
		f := r.Fields()
		f.Text = "branch two" // the same position, signed again: a fork
		fr, err := core.NewRecord(a1.key, f)
		if err != nil {
			t.Fatal(err)
		}
		v, err := fr.Verified()
		if err != nil {
			t.Fatal(err)
		}
		c.take(v)
	}
	y := ""
	for _, r := range c.records(room) {
		if r.Author() == a1.id {
			y = r.ID()
		}
	}
	if y == "" || y == x {
		t.Fatal("no fork")
	}
	st := b.mustPull(room, c, PullOpts{FullForm: true})
	if _, ok := b.held(room, y); ok || st.Stored != 0 {
		t.Fatalf("the fork was exchanged: %+v", st)
	}
	f := func() core.RecordFields { h, _ := b.held(room, x); return h.Record.Fields() }()
	cs, err := b.st.Copies(context.Background(), room, a1.id, f.Epoch, f.Seq)
	if err != nil || !slices.ContainsFunc(cs, func(cp store.Copy) bool { return cp.Peer == c.id }) {
		t.Fatalf("copies: %+v %v", cs, err)
	}
}

// ---- item 12: the short first request ----

// [N1] Equal summaries cost one short request each way.
func TestEqualSummariesCostOneShortRequestEachWay(t *testing.T) {
	a, b, net, room := twoSynced(t)
	l := capture(net)
	st := b.mustPull(room, a, PullOpts{})
	if len(l.reqs) != 1 || l.reqs[0].Sum == "" || l.reqs[0].Authors != nil || !l.resps[0].Same || st.Same != 1 {
		t.Fatalf("exchange: %+v %+v %+v", st, l.reqs, l.resps)
	}
}

// [N1] A difference costs one extra short round trip (answered full),
// then the full form converges.
func TestADifferenceCostsOneShortRoundTripThenConverges(t *testing.T) {
	a, b, net, room := twoSynced(t)
	id := a.send(room, "alice", "new")
	l := capture(net)
	b.mustPull(room, a, PullOpts{})
	if len(l.reqs) < 2 || l.reqs[0].Sum == "" || !l.resps[0].Full || l.reqs[1].Sum != "" {
		t.Fatalf("opening: %+v %+v", l.reqs, l.resps)
	}
	if _, ok := b.held(room, id); !ok {
		t.Fatal("did not converge")
	}
}

// [N1] A requester that knows there is a difference (it has an offer to
// make) sends the full form directly.
func TestARequesterWithAnOfferSendsTheFullFormDirectly(t *testing.T) {
	a, b, net, room := twoSynced(t)
	b.send(room, "bob", "to offer")
	l := capture(net)
	b.mustPull(room, a, PullOpts{Offer: true})
	if len(l.reqs) == 0 || l.reqs[0].Sum != "" || len(l.reqs[0].Offer) == 0 {
		t.Fatalf("first request: %+v", l.reqs)
	}
}

// [R5] A peer that answers same wrongly cannot make the requester store
// or drop anything.
func TestAWrongSameCannotMakeTheRequesterStoreOrDropAnything(t *testing.T) {
	a, b, net, room := twoSynced(t)
	id := a.send(room, "alice", "new")
	before := len(b.records(room))
	net.tamper = func(_, _ string, r *PullResponse) {
		if r.Full {
			r.Full, r.Same, r.Log.Entries, r.Log.More = false, true, nil, false
		}
	}
	if _, err := b.pullFrom(room, a, PullOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.held(room, id); ok || len(b.records(room)) != before {
		t.Fatal("a wrong same changed what the requester holds")
	}
	a.change(room, Change{Kind: core.EntryAdd, Target: junkNode(3), Name: "j", History: true})
	net.tamper = func(_, _ string, r *PullResponse) {
		if r.Full {
			r.Full, r.Same = false, true
		}
	}
	if _, err := b.pullFrom(room, a, PullOpts{}); err == nil {
		t.Fatal("a same carrying log entries was accepted")
	}
	net.tamper = nil
	b.mustPull(room, a, PullOpts{FullForm: true})
	if _, ok := b.held(room, id); !ok {
		t.Fatal("the full form did not repair it")
	}
}
