package node

// The two-level summary (PROTOCOL-next 4.3) on the wire, and the sizes of
// requests and responses in a large room.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

// exchangeLog captures every request and response on net.
type exchangeLog struct {
	reqs  []*PullRequest
	resps []*PullResponse
}

func capture(net *memNet) *exchangeLog {
	l := &exchangeLog{}
	net.lose = func(_, _ string, q *PullRequest, resp *PullResponse) bool {
		l.reqs, l.resps = append(l.reqs, q), append(l.resps, resp)
		return false
	}
	return l
}

// twoSynced returns a and b in one room with history, each with an agent,
// after a message from a has reached b and b has pulled once more.
func twoSynced(t *testing.T) (a, b *pnode, net *memNet, room string) {
	clock := lclock()
	net = newMemNet()
	a = newPNode(t, net, "a", newLNode(t, "a", clock))
	b = newPNode(t, net, "b", newLNode(t, "b", clock))
	room = a.create("team")
	a.change(room, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee", History: true})
	b.pin(room, a.lnode)
	a.register("alice")
	b.register("bob")
	a.send(room, "alice", "hello")
	b.mustPull(room, a, PullOpts{})
	b.mustPull(room, a, PullOpts{})
	return a, b, net, room
}

// [R5, N1] Equal summaries cost one small request each way: the opening
// carries the sum only, the answer is same with the log heads.
func TestPullWithEqualSummariesSendsNothing(t *testing.T) {
	a, b, net, room := twoSynced(t)
	l := capture(net)
	st := b.mustPull(room, a, PullOpts{Offer: true})
	if st.Requests != 1 || st.Rounds != 1 || st.Stored != 0 || st.Offered != 0 || st.Same != 1 || !st.Closed {
		t.Fatalf("stats: %+v", st)
	}
	q, r := l.reqs[0], l.resps[0]
	if q.Sum == "" || q.Detail != nil || len(q.Log.Entries) != 0 || len(q.Offer) != 0 {
		t.Fatalf("request: %+v", q)
	}
	if !r.Same || r.Authors != nil || r.Digests != nil || len(r.Page) != 0 || r.Detail != nil || len(r.Log.Entries) != 0 {
		t.Fatalf("response: %+v", r)
	}
	if up, down := net.reqBytes[len(net.reqBytes)-1], net.respBytes[len(net.respBytes)-1]; up > 300 || down > 200 {
		t.Fatalf("not small: %d and %d bytes", up, down)
	}
	t.Logf("idle pull of a two-machine room: request %d bytes, response %d bytes",
		net.reqBytes[len(net.reqBytes)-1], net.respBytes[len(net.respBytes)-1])
}

// [R5] One author differs: the opening is answered with the server's
// digests and sum, the requester finds the author whose digest differs,
// the continuing request carries its detail only (snap: the server's sum)
// and the page answers it.
func TestPullDetailRoundAsksOnlyForTheAuthorsThatDiffer(t *testing.T) {
	a, b, net, room := twoSynced(t)
	m := a.send(room, "alice", "again")
	l := capture(net)
	st := b.mustPull(room, a, PullOpts{})
	if st.Stored != 1 {
		t.Fatalf("stats: %+v", st)
	}
	if len(l.reqs) < 2 || l.reqs[0].Sum == "" || l.resps[0].Same || l.resps[0].Sum == "" || len(l.resps[0].Page) != 0 {
		t.Fatalf("opening: %+v %+v", l.reqs[0], l.resps[0])
	}
	q, r := l.reqs[1], l.resps[1]
	if len(q.Detail) != 1 || q.Detail[a.id] == nil || q.Snap != l.resps[0].Sum || q.Sum != "" {
		t.Fatalf("detail request: %+v", q)
	}
	if len(r.Page) != 1 || r.Detail[a.id] == nil {
		t.Fatalf("detail response: %+v", r)
	}
	if rec, err := core.ParseRecord(r.Page[0]); err != nil || rec.ID() != m {
		t.Fatalf("page: %v %v", rec, err)
	}
}

// [R5] Twenty authors differ: the detail goes in requests of at most 16
// authors, every author asked once, and everything arrives.
func TestPullWithMoreThanSixteenDifferingAuthorsSplitsTheDetail(t *testing.T) {
	clock := lclock()
	net := newMemNet()
	a := newPNode(t, net, "a", newLNode(t, "a", clock))
	b := newPNode(t, net, "b", newLNode(t, "b", clock))
	room := a.create("team")
	a.change(room, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee", History: true})
	b.pin(room, a.lnode)
	b.register("bob")
	var ids, ws []string
	for i := 0; i < 20; i++ {
		w := newLNode(t, fmt.Sprintf("w%02d", i), clock)
		ws = append(ws, w.id)
		a.change(room, Change{Kind: core.EntryAdd, Target: w.id, Name: fmt.Sprintf("w%02d", i), History: true})
		w.pin(room, a.lnode)
		w.pull(room, a.lnode)
		w.register("wes")
		ids = append(ids, w.send(room, "wes", "hi"))
		for _, r := range w.records(room) {
			a.take(r)
		}
	}
	l := capture(net)
	st := b.mustPull(room, a, PullOpts{})
	if st.Stored != 20 {
		t.Fatalf("stats: %+v", st)
	}
	asked := map[string]int{}
	var sizes []int
	for _, q := range l.reqs {
		if q.Detail != nil {
			sizes = append(sizes, len(q.Detail))
			for k := range q.Detail {
				asked[k]++
			}
		}
	}
	// The round that learned of the 20 from the log asks for each once,
	// in two requests.
	for _, w := range ws {
		if asked[w] != 1 {
			t.Fatalf("author %.8s asked %d times; detail requests %v", w, asked[w], sizes)
		}
	}
	if !slices.Equal(sizes, []int{16, 4}) {
		t.Fatalf("detail requests %v", sizes)
	}
	if es := b.inbox(room, "bob"); len(es) != 20 {
		t.Fatalf("bob: %d entries", len(es))
	}
}

// [R5] The server's records are refused by the puller every time (a relay
// altering text): the digests go on differing, and the exchange ends after
// the round that changed nothing instead of running to its bound. A round
// with a refusal keeps no seen, so the next exchange tries again, again
// in one round.
func TestPullRoundThatCannotMakeProgressEnds(t *testing.T) {
	a, b, net, room := twoSynced(t)
	a.send(room, "alice", "again")
	net.tamper = func(_, _ string, resp *PullResponse) {
		for i, w := range resp.Page {
			if r, err := core.ParseRecord(w); err == nil && len(r.Body()) > 0 {
				body := slices.Clone(r.Body())
				body[len(body)-3] ^= 1
				resp.Page[i] = wireOf(r.Header(), r.Sig(), body)
			}
		}
	}
	st, err := b.pullFrom(room, a, PullOpts{})
	if err != nil || st.Rounds != 1 || st.Stored != 0 || st.Same != 0 {
		t.Fatalf("stats: %+v %v", st, err)
	}
	st, err = b.pullFrom(room, a, PullOpts{})
	if err != nil || st.Rounds != 1 || st.SeenSame != 0 || st.Stored != 0 {
		t.Fatalf("again: %+v %v", st, err)
	}
}

// Sizes of a 64-machine room after 30 days in which every machine
// restarted daily and wrote once after each start: 64 authors with 30
// streams each. Printed for an idle pull and for one new message.
func TestPullSizesInASixtyFourMachineRoomAfterThirtyDaysOfDailyRestarts(t *testing.T) {
	if os.Getenv("A2P2_MODEL_DEEP") != "1" {
		t.Skip("measurement (26 s); run with A2P2_MODEL_DEEP=1")
	}
	clock := lclock()
	net := newMemNet()
	a := newPNode(t, net, "a", newLNode(t, "a", clock))
	b := newPNode(t, net, "b", newLNode(t, "b", clock))
	room := a.create("team")
	a.change(room, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee", History: true})
	b.pin(room, a.lnode)
	b.pull(room, a.lnode)
	var authors []ed25519.PrivateKey
	for i := 0; i < 62; i++ {
		seed := sha256.Sum256([]byte(fmt.Sprintf("sizes %d", i)))
		k := ed25519.NewKeyFromSeed(seed[:])
		authors = append(authors, k)
		a.change(room, Change{Kind: core.EntryAdd, Target: hex.EncodeToString(k.Public().(ed25519.PublicKey)),
			Name: fmt.Sprintf("m%02d", i), History: true})
	}
	b.pull(room, a.lnode)
	start := *clock
	*clock = start.Add(30*24*time.Hour + time.Hour)
	write := func(k ed25519.PrivateKey, day int, prev *core.VerifiedRecord) *core.VerifiedRecord {
		f := core.RecordFields{Room: room, Epoch: fmt.Sprintf("%016x", 1+day*1000+int(k[0])), Seq: 1,
			Kind: core.KindMessage, Agent: "agent", Text: "a short message of about sixty bytes, as most are.",
			Created: start.Add(time.Duration(day) * 24 * time.Hour)}
		if prev != nil {
			p := prev.Fields()
			f.Links = []core.Link{{Epoch: p.Epoch, Seq: p.Seq, ID: prev.ID()}}
		}
		r, err := core.NewRecord(k, f)
		if err != nil {
			t.Fatal(err)
		}
		v, err := r.Verified()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	last := map[int]*core.VerifiedRecord{}
	for day := 0; day < 30; day++ {
		for i, k := range authors {
			last[i] = write(k, day, last[i])
			for _, n := range []*pnode{a, b} {
				if out := n.take(last[i]); out != "stored" {
					t.Fatalf("day %d author %d: %s", day, i, out)
				}
			}
		}
	}
	b.mustPull(room, a, PullOpts{})
	report := func(what string, o PullOpts) {
		l := capture(net)
		n := len(net.reqBytes)
		st := b.mustPull(room, a, o)
		total := [2]int{}
		for i := n; i < len(net.reqBytes); i++ {
			total[0] += net.reqBytes[i]
			total[1] += net.respBytes[i]
			t.Logf("%s: request %d: %d bytes (detail for %d authors), response %d bytes (page %d)",
				what, i-n+1, net.reqBytes[i], len(l.reqs[i-n].Detail), net.respBytes[i], len(l.resps[i-n].Page))
		}
		t.Logf("%s: %d requests, %d bytes up, %d bytes down, stored %d", what, st.Requests, total[0], total[1], st.Stored)
	}
	report("64 machines, 1920 streams, idle", PullOpts{Offer: true})
	last[7] = write(authors[7], 30, last[7])
	a.take(last[7])
	report("64 machines, 1920 streams, one new message", PullOpts{Offer: true})
	// Fifteen days later: the text of half the streams has expired.
	*clock = clock.Add(15 * 24 * time.Hour)
	for _, n := range []*pnode{a, b} {
		if err := n.m.Prune(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	report("64 machines, 1920 streams, half fully expired, idle", PullOpts{Offer: true})
	last[7] = write(authors[7], 45, last[7])
	a.take(last[7])
	report("64 machines, 1920 streams, half fully expired, one new message", PullOpts{Offer: true})
}

// [R4] Streams begun by restarts link back to the stream before: a page
// carries each linked record before the first record linking to it, so one
// round stores everything.
func TestPullPageCarriesLinkedStreamsInOrder(t *testing.T) {
	a, b, _, room := twoSynced(t)
	for i := 0; i < 4; i++ {
		a.restartP()
		a.send(room, "alice", fmt.Sprintf("after restart %d", i))
	}
	st := b.mustPull(room, a, PullOpts{})
	if st.Stored != 4 || st.Rounds != 2 {
		t.Fatalf("stats: %+v", st)
	}
	if es := b.inbox(room, "bob"); len(es) != 5 {
		t.Fatalf("bob: %d entries", len(es))
	}
}
