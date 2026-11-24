package node

// Tests of the step 5a third round (docs/research/decisions/
// step5a-rereview-sol.md and the coordinator's rulings): seen only after a
// clean round, release and expiry, the detail budget, cuts by stream,
// progress, refusal state, validation, permission before expiry, Admitted
// after apply.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// pairWithClocks is pairWithHistory with a clock each.
func pairWithClocks(t *testing.T) (a, b *pnode, net *memNet, room string, aclock, bclock *time.Time) {
	t.Helper()
	aclock, bclock = lclock(), lclock()
	net = newMemNet()
	a = newPNode(t, net, "a", newLNode(t, "a", aclock))
	b = newPNode(t, net, "b", newLNode(t, "b", bclock))
	room = a.create("team")
	a.change(room, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee", History: true})
	b.pin(room, a.lnode)
	b.pull(room, a.lnode)
	a.register("alice")
	b.register("bob")
	return a, b, net, room, aclock, bclock
}

// ---- item 1: seen only after a clean round ----

// [N1, 4.3 step 3] A record dated more than 24 h ahead of the requester is
// refused; the round was not clean, so no seen is kept, and the record is
// taken once the requester's clock allows it.
func TestAFutureRecordLeavesNoSeenAndIsTakenOnceTheClockAllows(t *testing.T) {
	a, b, _, room, aclock, bclock := pairWithClocks(t)
	*aclock = aclock.Add(30 * time.Hour)
	id := a.send(room, "alice", "from the future")
	st := b.mustPull(room, a, PullOpts{})
	if _, ok := b.held(room, id); ok {
		t.Fatalf("a record 30 h ahead was taken: %+v", st)
	}
	gen, _ := b.r.Gen(room)
	if _, ok := b.x.seenOf([2]string{room, a.id}, gen); ok {
		t.Fatalf("seen kept after a refusal: %+v", st)
	}
	b.mustPull(room, a, PullOpts{})
	*bclock = bclock.Add(30 * time.Hour)
	st = b.mustPull(room, a, PullOpts{})
	if _, ok := b.held(room, id); !ok {
		t.Fatalf("the record was not taken once the clock allowed: %+v", st)
	}
}

// [4.3 step 3] seen is kept for the room's generation and at most an hour.
func TestSeenLastsAnHourAndOneGeneration(t *testing.T) {
	a, n, _, room := newcomerPair(t)
	n.mustPull(room, a, PullOpts{})
	n.mustPull(room, a, PullOpts{})
	key := [2]string{room, a.id}
	gen, _ := n.r.Gen(room)
	if _, ok := n.x.seenOf(key, gen); !ok {
		t.Fatal("no seen after a clean fruitless round")
	}
	if _, ok := n.x.seenOf(key, gen+1); ok {
		t.Fatal("seen outlived its generation")
	}
	*n.clock = n.clock.Add(61 * time.Minute)
	if _, ok := n.x.seenOf(key, gen); ok {
		t.Fatal("seen outlived an hour")
	}
}

// ---- item 2: release and expiry ----

// [RM2, 4.4 step 10] Text chosen for a page that passes its retention time
// between build and release, with nothing written meanwhile, does not go
// out: the response is built again.
func TestTextThatExpiresBetweenBuildAndReleaseIsNotSent(t *testing.T) {
	a, b, net, room := pairWithHistory(t)
	id := a.send(room, "alice", "nearly thirty days old")
	*a.clock = a.clock.Add(store.Retention - time.Minute)
	fired := onDetail(a.x, b.id, func(string) { *a.clock = a.clock.Add(2 * time.Minute) })
	b.mustPull(room, a, PullOpts{})
	if *fired != 1 {
		t.Fatal("no data page was built")
	}
	if slices.Contains(net.pageText, id) {
		t.Fatal("text past its retention was released")
	}
}

// ---- item 3: the detail budget ----

// epochAuthors admits, on a, an author per count holding that many
// streams of one record each, gives a their records, and returns their ids
// by author.
func epochAuthors(t *testing.T, a *pnode, room string, counts ...int) [][]string {
	t.Helper()
	var keys []ed25519.PrivateKey
	for i := range counts {
		seed := sha256.Sum256([]byte(fmt.Sprintf("fix3 %d", i)))
		k := ed25519.NewKeyFromSeed(seed[:])
		keys = append(keys, k)
		a.change(room, Change{Kind: core.EntryAdd, Target: hex.EncodeToString(k.Public().(ed25519.PublicKey)),
			Name: fmt.Sprintf("e%02d", i), History: true})
	}
	out := make([][]string, len(counts))
	for i, n := range counts {
		for j := 0; j < n; j++ {
			f := core.RecordFields{Room: room, Epoch: fmt.Sprintf("%016x", 1+j), Seq: 1, Kind: core.KindMessage,
				Agent: "agent", Text: "x", Created: *a.clock}
			r, err := core.NewRecord(keys[i], f)
			if err != nil {
				t.Fatal(err)
			}
			v, err := r.Verified()
			if err != nil {
				t.Fatal(err)
			}
			if out := a.take(v); out != store.OutcomeStored {
				t.Fatalf("take: %s", out)
			}
			out[i] = append(out[i], v.ID())
		}
	}
	return out
}

// [4.3 step 5] Sixteen authors whose server detail together exceeds the
// response limit: the response carries the detail that fits, first, and
// the requester asks for the rest again; an author whose server detail
// alone cannot fit is counted too_large and skipped.
func TestServerDetailThatDoesNotFitIsAskedAgainAndTooLargeIsSkipped(t *testing.T) {
	a, b, net, room := pairWithHistory(t)
	counts := []int{120}
	for i := 0; i < 16; i++ {
		counts = append(counts, 10)
	}
	byAuthor := epochAuthors(t, a, room, counts...)
	big := byAuthor[0]
	var ids []string
	for _, xs := range byAuthor[1:] {
		ids = append(ids, xs...)
	}
	b.pull(room, a.lnode)
	defer func(v int) { maxResponseBytes = v }(maxResponseBytes)
	maxResponseBytes = 4 << 10
	l := capture(net)
	st, err := b.pullFrom(room, a, PullOpts{})
	if err != nil {
		t.Fatalf("pull: %+v %v", st, err)
	}
	for _, rb := range net.respBytes {
		if rb > maxResponseBytes {
			t.Fatalf("a response of %d bytes", rb)
		}
	}
	for _, id := range ids {
		if _, ok := b.held(room, id); !ok {
			t.Fatalf("not all taken: %+v", st)
		}
	}
	if _, ok := b.held(room, big[0]); ok || st.TooLarge == 0 { // counted in each round that skips it
		t.Fatalf("the oversized author: %+v", st)
	}
	short := false
	for i, q := range l.reqs {
		if q.Detail != nil && len(l.resps[i].Detail) < len(q.Detail) {
			short = true
		}
	}
	if !short {
		t.Fatal("no response left detail out")
	}
}

// ---- item 4: cuts ----

// [R4, 4.3 step 4] One author's first stream is blocked by a record dated
// too far ahead; its second stream is fetched to the end, a record a page.
func TestABlockedStreamDoesNotHoldBackTheSameAuthorsOtherStream(t *testing.T) {
	a, b, _, room, aclock, _ := pairWithClocks(t)
	seed := sha256.Sum256([]byte("fix3 two streams"))
	k := ed25519.NewKeyFromSeed(seed[:])
	a.change(room, Change{Kind: core.EntryAdd, Target: hex.EncodeToString(k.Public().(ed25519.PublicKey)), Name: "two", History: true})
	b.pull(room, a.lnode)
	start := *aclock
	*aclock = start.Add(30 * time.Hour)
	write := func(epoch string, seq int64, prev string, at time.Time) string {
		f := core.RecordFields{Room: room, Epoch: epoch, Seq: seq, Prev: prev, Kind: core.KindMessage,
			Agent: "agent", Text: "x", Created: at}
		r, err := core.NewRecord(k, f)
		if err != nil {
			t.Fatal(err)
		}
		v, err := r.Verified()
		if err != nil {
			t.Fatal(err)
		}
		if out := a.take(v); out != store.OutcomeStored {
			t.Fatalf("take: %s", out)
		}
		return v.ID()
	}
	e1 := write("0000000000000001", 1, "", start)
	write("0000000000000001", 2, e1, start.Add(30*time.Hour)) // more than 24 h ahead of b
	var later []string
	prev := ""
	for i := int64(1); i <= 3; i++ {
		prev = write("0000000000000002", i, prev, start)
		later = append(later, prev)
	}
	a.x.pageLimit = 1
	st, err := b.pullFrom(room, a, PullOpts{})
	if err != nil {
		t.Fatalf("pull: %+v %v", st, err)
	}
	if _, ok := b.held(room, e1); !ok {
		t.Fatalf("the first stream's first record: %+v", st)
	}
	for _, id := range later {
		if _, ok := b.held(room, id); !ok {
			t.Fatalf("the second stream was held back by the first: %+v", st)
		}
	}
}

// [4.3 step 4] Fork evidence a peer cannot list in extra goes at most once
// in an exchange, each way, and is not progress.
func TestEvidenceAPeerCannotListGoesOnceAndIsNotProgress(t *testing.T) {
	a, b, net, room := pairWithHistory(t)
	add := a.r.rooms[room].mach.Offer()[1].ID() // b's admission
	var forks [][]byte
	for i := 0; i < 3; i++ {
		forks = append(forks, signFork(t, a.lnode, room, add, junkNode(i)))
	}
	for _, n := range []*pnode{a, b} {
		if _, err := n.r.Receive(context.Background(), room, 0, forks...); err != nil {
			t.Fatal(err)
		}
	}
	if _, ex := a.r.rooms[room].logHeads(); len(ex) != 3 {
		t.Fatalf("evidence: %d", len(ex))
	}
	defer func(v int) { maxLogExtra = v }(maxLogExtra)
	maxLogExtra = 1
	a.register("alice")
	for i := 0; i < 3; i++ {
		if i > 0 {
			a.send(room, "alice", "a continuation") // so the exchange has several requests
		}
		l := capture(net)
		st := b.mustPull(room, a, PullOpts{})
		sent := map[string]int{}
		down := 0
		for j, q := range l.reqs {
			for _, e := range q.Log.Entries {
				sent["up "+string(e)]++
			}
			for _, e := range l.resps[j].Log.Entries {
				sent["down "+string(e)]++
				down++
			}
		}
		for k, n := range sent {
			if n > 1 {
				t.Fatalf("pull %d: sent %d times: %.40s", i, n, k)
			}
		}
		if i == 0 && st.Rounds != 1 {
			t.Fatalf("evidence counted as progress: %+v", st)
		}
		if i == 2 && down > 0 {
			t.Fatalf("pull %d: evidence sent again while the peer's heads did not change", i)
		}
	}
}

// ---- item 5: progress and quiet ----

// [4.3 step 4] An offer the server refuses is not progress: the exchange
// ends after the round it went in.
func TestARefusedOfferIsNotProgress(t *testing.T) {
	a, b, _, room, _, bclock := pairWithClocks(t)
	b.mustPull(room, a, PullOpts{})
	*bclock = bclock.Add(30 * time.Hour) // b's records are more than 24 h ahead of a
	id := b.send(room, "bob", "from the future")
	st := b.mustPull(room, a, PullOpts{Offer: true})
	if _, ok := a.held(room, id); ok || st.Offered != 1 {
		t.Fatalf("the offer: %+v", st)
	}
	if st.Rounds != 1 {
		t.Fatalf("a refused offer counted as progress: %+v", st)
	}
}

// [4.3 step 4] Log entries the server holds already are not progress.
func TestDuplicateLogEntriesAreNotProgress(t *testing.T) {
	a, b, _, room := pairWithHistory(t)
	b.mustPull(room, a, PullOpts{})
	forgetLog(t, b, room, a.id) // b no longer knows what a holds of the log
	st := b.mustPull(room, a, PullOpts{})
	if st.Rounds != 1 {
		t.Fatalf("entries a held already counted as progress: %+v", st)
	}
}

// forgetLog deletes what n observed of peer's log.
func forgetLog(t *testing.T, n *pnode, room, peer string) {
	t.Helper()
	if _, err := n.st.Exchange(context.Background(), room, 0, func(tx *store.Tx) error {
		return tx.ForgetLog(room, peer)
	}); err != nil {
		t.Fatal(err)
	}
}

// [4.3 step 3] A server restored from an old copy lacks a record its
// observation said it held: the round that stored nothing but showed that
// in the detail is followed by one more round carrying the offer.
func TestARoundThatFirstLearnsUsableDetailIsFollowedByOneOffer(t *testing.T) {
	a, b, _, room := pairWithHistory(t)
	b.mustPull(room, a, PullOpts{})
	a.mustPull(room, b, PullOpts{})
	old := snapshot(t, b)
	id := a.send(room, "alice", "lost by b's restore")
	b.mustPull(room, a, PullOpts{})
	a.mustPull(room, b, PullOpts{}) // a observes that b holds it
	restoreFrom(t, b, old)
	st := a.mustPull(room, b, PullOpts{Offer: true})
	if _, ok := b.held(room, id); !ok {
		t.Fatalf("not offered: %+v", st)
	}
}

// ---- item 6: refusal state ----

// [4.4] A 403 to a continuing request is handled as one to an opening:
// the observed log of the peer is forgotten; the one resend carries the
// accepted log from the start with the whole budget and no offer; after
// the second refusal neither entries nor offers go to that peer.
func TestEveryNotMemberRefusalForgetsTheLogAndMutesEntriesAndOffers(t *testing.T) {
	a, b, net, room := pairWithHistory(t)
	for i := 0; i < 24; i++ {
		a.change(room, Change{Kind: core.EntryAdd, Target: junkNode(10 + i), Name: fmt.Sprintf("j%d", i), History: true})
	}
	b.mustPull(room, a, PullOpts{})
	a.mustPull(room, b, PullOpts{})
	b.send(room, "bob", "a backlog")
	a.send(room, "alice", "x")
	removed := false
	net.lose = func(_, _ string, q *PullRequest, _ *PullResponse) bool {
		if q.Sum != "" && !removed {
			removed = true
			a.change(room, Change{Kind: core.EntryRemove, Target: b.id})
		}
		return false
	}
	if _, err := b.pullFrom(room, a, PullOpts{Offer: true}); !isNotMember(err) {
		t.Fatalf("continuation: %v", err)
	}
	if obs, _ := observedOf(t, b, room, a.id); obs != nil {
		t.Fatalf("the observed log survived a refusal: %+v", obs)
	}
	net.lose = nil
	var accepted int
	for _, e := range b.r.rooms[room].mach.Offer() {
		if b.r.rooms[room].state[e.ID()].State == store.LogAccepted {
			accepted++
		}
	}
	whole := 0
	for _, e := range b.r.rooms[room].mach.Offer() {
		whole += len(e.Wire()) + 1
	}
	defer func(v int) { maxRequestBytes = v }(maxRequestBytes)
	maxRequestBytes = whole + 3*wireMargin // the log fits once, not in half the budget
	n0 := len(net.heard)
	if _, err := b.pullFrom(room, a, PullOpts{Offer: true}); !isNotMember(err) {
		t.Fatalf("resend: %v", err)
	}
	if q := net.heard[n0]; len(q.Log.Entries) != accepted || q.Offer != nil {
		t.Fatalf("resend: %d of %d entries, %d offered", len(q.Log.Entries), accepted, len(q.Offer))
	}
	for i := 0; i < 2; i++ {
		n0 = len(net.heard)
		if _, err := b.pullFrom(room, a, PullOpts{Offer: true}); !isNotMember(err) {
			t.Fatalf("muted: %v", err)
		}
		if q := net.heard[n0]; q.Log.Entries != nil || q.Offer != nil {
			t.Fatalf("muted: %d entries, %d offered", len(q.Log.Entries), len(q.Offer))
		}
	}
}

// observedOf is what n observed of peer's log.
func observedOf(t *testing.T, n *pnode, room, peer string) (*store.PeerLog, []store.Observation) {
	t.Helper()
	var obs []store.Observation
	if err := n.st.View(context.Background(), func(rd *store.Reader) (err error) {
		obs, err = rd.Observed(room, peer)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, l := observedMap(obs)
	return l, obs
}

// ---- item 7: validation ----

// [4.3 step 5] A page record whose author is not in the request's detail
// is refused, and counted, before anything of the response is applied.
func TestAPageRecordOfAnAuthorNotAskedIsRefusedBeforeAnythingIsApplied(t *testing.T) {
	a, b, net, room := pairWithHistory(t)
	b.send(room, "bob", "b's own")
	b.mustPull(room, a, PullOpts{})
	a.mustPull(room, b, PullOpts{})
	id := a.send(room, "alice", "asked")
	c := newLNode(t, "c", a.clock)
	a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "sea", History: true})
	c.pin(room, a.lnode)
	c.pull(room, a.lnode)
	b.pull(room, a.lnode)
	c.register("carol")
	other := c.send(room, "carol", "not asked")
	var extra json.RawMessage
	for _, r := range c.records(room) {
		if r.ID() == other {
			extra = r.Wire()
		}
	}
	net.tamper = func(_, _ string, r *PullResponse) {
		if r.Detail != nil {
			if _, asked := r.Detail[c.id]; !asked {
				r.Page = append(r.Page, extra)
			}
		}
	}
	if _, err := b.pullFrom(room, a, PullOpts{}); err == nil {
		t.Fatal("accepted")
	}
	for _, x := range []string{id, other} {
		if _, ok := b.held(room, x); ok {
			t.Fatal("something of it was applied")
		}
	}
	if !refusedBy(t, b, room, a.id, CodeBadResponse) {
		t.Fatal("not counted")
	}
}

// refusedBy says whether n counted a refusal of peer for reason.
func refusedBy(t *testing.T, n *pnode, room, peer, reason string) bool {
	t.Helper()
	rs, err := n.st.Refusals(context.Background(), room)
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(rs, func(r store.Refusal) bool { return r.Peer == peer && r.Reason == reason && r.Count > 0 })
}

// [R4] A record that is not the next of its stream is counted.
func TestARecordThatIsNotNextIsCounted(t *testing.T) {
	a, b, net, room := pairWithHistory(t)
	a.send(room, "alice", "one")
	a.send(room, "alice", "two")
	net.tamper = func(_, _ string, r *PullResponse) {
		if len(r.Page) > 1 {
			r.Page = r.Page[1:]
		}
	}
	_, _ = b.pullFrom(room, a, PullOpts{})
	if !refusedBy(t, b, room, a.id, string(store.OutcomeNotNext)) {
		t.Fatal("not_next not counted")
	}
}

// [R5] A known position a peer reported is a hint: the same peer's later
// report lowers it, and the gap goes.
func TestAKnownPositionIsLoweredByTheSamePeersLaterReport(t *testing.T) {
	a, b, _, room := pairWithHistory(t)
	b.send(room, "bob", "one")
	a.mustPull(room, b, PullOpts{})
	ctx := context.Background()
	var d []store.StreamDetail
	if err := b.m.view(ctx, room, func(v *roomView) error {
		own, err := b.r.summarize(v, b.state(room))
		d = own.detailOf(b.id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	report := func(c int64) {
		dd := []store.StreamDetail{{E: d[0].E, C: c, X: 0}}
		q := &PullRequest{V: PullVersion, Room: room, Snap: strings.Repeat("0", 64), Detail: map[string][]store.StreamDetail{b.id: dd}}
		q.Log.Heads, q.Log.Extra = b.r.rooms[room].logHeads()
		if _, err := a.x.Serve(ctx, b.id, q); err != nil {
			t.Fatal(err)
		}
	}
	gap := func() bool {
		var known int64
		_ = a.st.View(ctx, func(rd *store.Reader) error {
			ss, err := rd.Streams(room)
			for _, s := range ss {
				if s.Author == b.id {
					known = s.Known - s.Contig
				}
			}
			return err
		})
		return known > 0
	}
	report(9) // unsupported: b holds one
	if !gap() {
		t.Fatal("no gap noted")
	}
	report(1)
	if gap() {
		t.Fatal("the gap outlived the peer's retraction")
	}
}

// ---- item 8: permission before expiry ----

// [R8, 4.3] A header past retention received before the frontier that
// covers it resolves: once it resolves and puts it before the reader's
// start, the reader is told "before you joined" (nothing), not "expired".
func TestAnOldHeaderBeforeTheFrontierReadsAsBeforeYouJoined(t *testing.T) {
	clock := lclock()
	a := newLNode(t, "a", clock)
	room := a.create("team")
	a.register("alice")
	old := a.send(room, "alice", "long ago")
	*clock = clock.Add(31 * 24 * time.Hour)
	head := a.send(room, "alice", "the head")
	n := newLNode(t, "n", clock)
	a.change(room, Change{Kind: core.EntryAdd, Target: n.id, Name: "en"})
	n.pin(room, a)
	n.pull(room, a)
	n.register("nina")
	recs := map[string]*core.VerifiedRecord{}
	for _, r := range a.records(room) {
		recs[r.ID()] = r
	}
	n.take(recs[old])
	n.take(recs[head])
	es, err := n.m.Thread(context.Background(), room, old)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		if e.ID == old {
			t.Fatalf("a record before the reader's start reads as %q", e.Kind)
		}
	}
}

// ---- item 9: Admitted after apply ----

// [R5] An author admitted by the opening answer's log is asked for in the
// same round: its record arrives in the first round, the second is same.
func TestAnAuthorAdmittedByTheOpeningAnswerIsAskedInTheSameRound(t *testing.T) {
	a, b, _, room := pairWithHistory(t)
	b.mustPull(room, a, PullOpts{})
	c := newLNode(t, "c", a.clock)
	a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "sea", History: true})
	c.pin(room, a.lnode)
	c.pull(room, a.lnode)
	c.register("carol")
	id := c.send(room, "carol", "hi")
	for _, r := range c.records(room) {
		a.take(r)
	}
	st := b.mustPull(room, a, PullOpts{})
	if _, ok := b.held(room, id); !ok || st.Rounds != 2 {
		t.Fatalf("pull: %+v", st)
	}
}
