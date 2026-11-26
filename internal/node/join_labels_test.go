package node

// Group D of the security review of 2026-11-25: a join answer for a room
// held here (F7) and the label a joined room is held and shown under (F12).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// rewriteLink is a join link to a real server whose answers fn changes
// on the way to the joiner: a hostile admin machine. A preview it
// changes is mapped back in the joiner's accept, so the server takes the
// accept as one for the preview it showed.
type rewriteLink struct {
	joinLink
	fn    func(*JoinAnswer)
	mu    sync.Mutex
	shown map[string]string // the preview id the joiner saw: the one the server showed
}

func newRewriteLink(jn *joinNet, from *lnode, fn func(*JoinAnswer)) *rewriteLink {
	return &rewriteLink{joinLink: joinLink{jn, from}, fn: fn, shown: map[string]string{}}
}

func (l *rewriteLink) Join(ctx context.Context, addrs []string, check func(string) error, body []byte) (JoinReply, error) {
	var q JoinRequest
	if json.Unmarshal(body, &q) == nil && q.Op == JoinAccept {
		l.mu.Lock()
		if real, ok := l.shown[q.Terms]; ok {
			q.Terms = real
			body, _ = json.Marshal(q)
		}
		l.mu.Unlock()
	}
	rp, err := l.joinLink.Join(ctx, addrs, check, body)
	if err != nil {
		return rp, err
	}
	var a JoinAnswer
	if json.Unmarshal(rp.Body, &a) != nil || a.Room == nil {
		return rp, nil
	}
	real, _ := answerPreview(rp, a, a.Admin)
	l.fn(&a)
	if a.State == JoinStatePreviewed {
		if seen, err := answerPreview(rp, a, a.Admin); err == nil {
			l.mu.Lock()
			l.shown[seen.ID] = real.ID
			l.mu.Unlock()
		}
	}
	rp.Body, _ = json.Marshal(a)
	return rp, nil
}

func (n *lnode) linkJoiner(l JoinTransport) *Joiner {
	return NewJoiner(n.r, l, JoinerOpts{Name: n.name, Addrs: []string{addrOf(n)},
		Sleep: func(ctx context.Context, d time.Duration) error {
			*n.clock = n.clock.Add(d)
			return ctx.Err()
		}})
}

func (n *lnode) pulled(room string) bool {
	n.r.mu.Lock()
	defer n.r.mu.Unlock()
	rl := n.r.rooms[room]
	return rl != nil && rl.pulled
}

func (n *lnode) logRows(room string) int {
	n.t.Helper()
	rows, err := n.st.Log(context.Background(), room)
	if err != nil {
		n.t.Fatal(err)
	}
	return len(rows)
}

// F7. A removed admin that has not heard of its removal signs the removal
// of a member, then hands a member that holds the room an invite with its
// own key. The member's person approves what looks like a join. The
// answer's entries are not stored, the member it removed stays, and the
// join does not open the not_caught_up gate.
func TestJoinAnswerFromANonMemberCannotWriteIntoAHeldRoom(t *testing.T) {
	clock := lclock()
	a, b, x := newLNode(t, "a", clock), newLNode(t, "b", clock), newLNode(t, "x", clock)
	m, v := newLNode(t, "m", clock), newLNode(t, "v", clock)
	jn := newJoinNet()
	jn.serve(a)
	jn.serve(x)
	room := a.create("team")
	joinRoom(t, jn, a, b, room, "", false) // second machine: admin
	joinRoom(t, jn, a, x, room, "", false) // third: promoted by a, seconded by b
	b.pull(room, a)
	b.confirm(b.onlyRequest(room).ID)
	a.pull(room, b)
	x.pull(room, a)
	wantNodes(t, "admins", x.state(room).Admins, a.id, b.id, x.id)
	joinRoom(t, jn, a, m, room, RoleMember, false)
	joinRoom(t, jn, a, v, room, RoleMember, false)
	x.pull(room, a)

	// a and b remove x; v learns it. x hears nothing of it.
	a.change(room, Change{Kind: core.EntryRemove, Target: x.id})
	b.pull(room, a)
	b.confirm(b.onlyRequest(room).ID)
	a.pull(room, b)
	v.pull(room, a)
	wantNodes(t, "v's members", v.state(room).Members, a.id, b.id, m.id, v.id)
	x.change(room, Change{Kind: core.EntryRemove, Target: m.id})

	v.restart() // the gate is closed again
	if v.pulled(room) {
		t.Fatal("pulled after a restart")
	}
	rows := v.logRows(room)
	tok := x.invite(room, RoleMember, false)
	j := v.joiner(jn)
	if p := v.preview(j, tok); p.Label != "team" {
		t.Fatalf("preview: %+v", p)
	}
	v.accept(j, tok, 0)
	x.confirm(x.onlyRequest(room).ID)
	wantOutcome(t, v.resume(j, time.Minute), JoinStateRefused, ReasonJoinServerNotMember)
	wantNodes(t, "v's members after", v.state(room).Members, a.id, b.id, m.id, v.id)
	if got := v.logRows(room); got != rows {
		t.Fatalf("v's log has %d rows, had %d", got, rows)
	}
	if v.pulled(room) {
		t.Fatal("a join answer from a removed admin opened the not_caught_up gate")
	}
	// The room still works: a pull from a member catches up.
	v.pull(room, a)
	if !v.pulled(room) {
		t.Fatal("a pull from a member did not catch up")
	}
}

// F7. A signed answer with no heads, even from a member, does not count
// as caught up; an honest one from a member does.
func TestJoinWithEmptyHeadsDoesNotCountAsCaughtUp(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")
	joinRoom(t, jn, a, b, room, RoleMember, false)
	b.restart()

	strip := newRewriteLink(jn, b, func(an *JoinAnswer) { an.Heads = nil })
	j := b.linkJoiner(strip)
	tok := a.invite(room, RoleMember, false)
	b.preview(j, tok)
	b.accept(j, tok, 0)
	a.confirm(a.onlyRequest(room).ID)
	wantOutcome(t, b.resume(j, time.Minute), JoinStateSigned, "")
	if b.pulled(room) {
		t.Fatal("an answer with no heads opened the not_caught_up gate")
	}

	// Honest: the same join with the heads catches up.
	j = b.joiner(jn)
	tok = a.invite(room, RoleMember, false)
	b.preview(j, tok)
	b.accept(j, tok, 0)
	a.confirm(a.onlyRequest(room).ID)
	wantOutcome(t, b.resume(j, time.Minute), JoinStateSigned, "")
	if !b.pulled(room) {
		t.Fatal("a join through a member with its heads did not catch up")
	}
}

// F7. The preview of a room held here must name the label held here; a
// different one is refused as a change before anything is sent.
func TestJoinPreviewOfAHeldRoomWithAnotherLabelIsRefusedAsAChange(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")
	joinRoom(t, jn, a, b, room, RoleMember, false)
	j := b.linkJoiner(newRewriteLink(jn, b, func(an *JoinAnswer) { an.Room.Label = "a-new-project" }))
	_, err := j.JoinPreview(context.Background(), a.invite(room, RoleMember, false))
	if err == nil || !strings.Contains(err.Error(), "held here as") || !strings.Contains(err.Error(), "team") {
		t.Fatalf("preview of a held room under another label: %v", err)
	}
}

// F12. The admin machine's preview names one label and its signed create
// entry another: the join is refused and nothing stays held.
func TestJoinWhoseCreateEntryLabelDiffersFromThePreviewIsRefused(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	jn := newJoinNet()
	jn.serve(a)
	for _, created := range []string{"team", LocalLabel} {
		room := a.create(created)
		j := b.linkJoiner(newRewriteLink(jn, b, func(an *JoinAnswer) { an.Room.Label = "trusted" }))
		tok := a.invite(room, RoleMember, false)
		if p := b.preview(j, tok); p.Label != "trusted" {
			t.Fatalf("preview: %+v", p)
		}
		b.accept(j, tok, 0)
		a.confirm(a.onlyRequest(room).ID)
		got := b.resume(j, time.Minute)
		if len(got) != 1 || got[0].State != JoinStateRefused || !strings.Contains(got[0].Reason, "as the preview showed") {
			t.Fatalf("%s: outcome %+v", created, got)
		}
		if _, held, _ := b.st.RoomGen(context.Background(), room); held {
			t.Fatalf("%s: the room is held after a refused join", created)
		}
	}
}

// F12. A room created on another machine under the label local, or a
// label with control, direction or invisible characters, is refused at
// the preview; a join recorded under an older version is refused when
// its answer comes. An honest label joins.
func TestJoinOfARoomLabelledLocalOrWithControlsIsRefused(t *testing.T) {
	ctx := context.Background()
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	jn := newJoinNet()
	jn.serve(a)
	for _, label := range []string{LocalLabel, "Local", " local", "ops\u202e\x1b[2K", "ops\u200b", "ops\nfine", "op\u200cs"} {
		room := a.create(label)
		_, err := b.joiner(jn).JoinPreview(ctx, a.invite(room, RoleMember, false))
		if err == nil || !strings.Contains(err.Error(), "cannot be joined here") {
			t.Fatalf("%q: preview %v", label, err)
		}
	}

	// Approved under 0.2.3, which had no such rule: the record is there,
	// and the answer is refused when it arrives.
	room := a.create(LocalLabel)
	tok := mustToken(t, a.invite(room, RoleMember, false))
	var server string
	rp, an, err := b.joiner(jn).call(ctx, tok.Addrs, fingerprintCheck(tok, &server), JoinRequest{Op: JoinOpen, Secret: tok.SecretText()})
	if err != nil {
		t.Fatal(err)
	}
	p, err := answerPreview(rp, an, server)
	if err != nil {
		t.Fatal(err)
	}
	h := secretHash(tok.Secret)
	if _, _, err := b.st.AddJoin(ctx, store.Join{Invite: h, Room: p.Room, Label: p.Label, CreateID: p.Create, Role: p.Role,
		History: p.History, Admin: p.Admin, Addrs: tok.Addrs, Expires: p.Expires, Created: *clock}); err != nil {
		t.Fatal(err)
	}
	j := b.joiner(jn)
	if _, _, err := j.call(ctx, tok.Addrs, func(string) error { return nil },
		JoinRequest{Op: JoinAccept, Secret: tok.SecretText(), Terms: p.ID}); err != nil {
		t.Fatal(err)
	}
	a.confirm(a.onlyRequest(room).ID)
	got := b.resume(j, time.Minute)
	if len(got) != 1 || got[0].State != JoinStateRefused || !strings.Contains(got[0].Reason, "reads as local") {
		t.Fatalf("outcome %+v", got)
	}
	if _, held, _ := b.st.RoomGen(ctx, room); held {
		t.Fatal("the room labelled local is held")
	}

	// Honest: accented letters, a space, emoji with a joiner and a
	// selector; a non-joiner and a joiner between letters of scripts
	// that need them (Persian, Devanagari). A non-joiner between Latin
	// letters is refused above: the display escapes it there.
	for _, label := range []string{"équipe \U0001F469\u200d\U0001F4BB \u2764\ufe0f", "\u0645\u06cc\u200c\u062e\u0648\u0627\u0647\u0645", "\u0915\u094d\u200d\u0937"} {
		room = a.create(label)
		joinRoom(t, jn, a, b, room, RoleMember, false)
		if info, err := b.r.Info(ctx, room); err != nil || info.Label != label {
			t.Fatalf("%q: info %+v %v", label, info, err)
		}
	}
}

// F12. A database that holds a room from another machine labelled local,
// stored by an older version, still opens with that room working; the
// room is never shown or resolved as this machine's local room.
func TestStoredRoomLabelledLocalStillOpensAndIsNotShownAsTheLocalRoom(t *testing.T) {
	ctx := context.Background()
	clock := lclock()
	y := newLNode(t, "y", clock)
	room := y.create(LocalLabel)
	e := newSurf(t)
	y.r.mu.Lock()
	rl := y.r.rooms[room]
	create := rl.ents[rl.createID].Wire()
	y.r.mu.Unlock()
	if _, err := e.s.r.PinRoom(ctx, create); err != nil { // as 0.2.3 stored it
		t.Fatal(err)
	}
	e.restart()
	if qs := e.s.r.Quarantined(); len(qs) != 0 {
		t.Fatalf("set aside: %+v", qs)
	}
	if _, err := e.s.r.State(room); err != nil {
		t.Fatalf("the room does not load: %v", err)
	}
	if lr, err := e.st.LogRoom(ctx, room); err != nil || lr.Label != LocalLabel {
		t.Fatalf("stored label %q %v: it must stay as signed", lr.Label, err)
	}
	if id, err := e.s.ResolveRoom(ctx, LocalLabel); err != nil || id != e.s.local {
		t.Fatalf("local resolves to %q %v, want this machine's local room %q", id, err, e.s.local)
	}
	shown, err := e.s.roomLabel(ctx, room)
	if err != nil || shown == LocalLabel || !strings.Contains(shown, room) {
		t.Fatalf("shown label %q %v", shown, err)
	}
	if id, err := e.s.ResolveRoom(ctx, room); err != nil || id != room {
		t.Fatalf("by id: %q %v", id, err)
	}
	info, err := e.s.r.Info(ctx, room)
	if err != nil || info.Label != shown {
		t.Fatalf("info label %q %v", info.Label, err)
	}
	if local, _ := e.s.roomLabel(ctx, e.s.local); local != LocalLabel {
		t.Fatalf("the local room is shown as %q", local)
	}
}

// signChainAfter is n entries a signs at the end of its own stream in
// room, each adding a made-up member, the first on every head of the
// room on a, which must include a's last entry: a valid chain at any
// point of a log,
// unlike signChain, which starts at position 2.
func signChainAfter(t *testing.T, a *lnode, room string, n int) [][]byte {
	t.Helper()
	a.r.mu.Lock()
	rl := a.r.rooms[room]
	seq, prev := int64(0), ""
	for id, v := range rl.ents {
		if rl.state[id].State == store.LogAccepted && v.Author() == a.id && v.Fields().Seq > seq {
			seq, prev = v.Fields().Seq, id
		}
	}
	heads := acceptedHeads(rl)
	a.r.mu.Unlock()
	if prev != "" && !slices.Contains(heads, prev) {
		t.Fatalf("%s's last entry %s is not a head %v", a.name, prev, heads)
	}
	parents := heads
	var out [][]byte
	for i := 0; i < n; i++ {
		e, err := core.NewEntry(a.key, core.EntryFields{Room: room, Seq: seq + 1, Prev: prev, Parents: parents,
			Kind: core.EntryAdd, Target: fmt.Sprintf("c%063x", len(a.name)<<20+i), Name: fmt.Sprintf("n%d", i), Created: *a.clock})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e.Wire())
		prev, seq = e.ID(), seq+1
		parents = []string{prev}
	}
	if res, err := a.r.Receive(context.Background(), room, 0, out...); err != nil || res[len(res)-1].Disposition != "accepted" {
		t.Fatalf("chain on %s: %v %+v", a.name, err, res[len(res)-1])
	}
	return out
}

// pollLink is a join link that counts the signed pages it hands over and
// lets before see each poll first: it may answer it in place of the
// server (ok true).
type pollLink struct {
	joinLink
	mu     sync.Mutex
	pages  int
	before func(q JoinRequest) (JoinReply, bool)
}

func (l *pollLink) Join(ctx context.Context, addrs []string, check func(string) error, body []byte) (JoinReply, error) {
	var q JoinRequest
	_ = json.Unmarshal(body, &q)
	if l.before != nil && q.Op == JoinPoll {
		if rp, ok := l.before(q); ok {
			return rp, nil
		}
	}
	rp, err := l.joinLink.Join(ctx, addrs, check, body)
	var a JoinAnswer
	if err == nil && json.Unmarshal(rp.Body, &a) == nil && a.State == JoinStateSigned {
		l.mu.Lock()
		l.pages++
		l.mu.Unlock()
	}
	return rp, err
}

func (l *pollLink) signedPages() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pages
}

// F7, the honest case: a member that holds the room but has not heard
// of the admin machine yet joins through it. The admin's own admission
// comes on the second page of its log; the first page commits nothing
// and is kept until then, and the join is signed and caught up.
func TestJoinOfAHeldRoomThroughAnAdminItDoesNotKnowYetAcrossPages(t *testing.T) {
	clock := lclock()
	a, b, c := newLNode(t, "a", clock), newLNode(t, "b", clock), newLNode(t, "c", clock)
	jn := newJoinNet()
	jn.serve(a)
	jn.serve(c)
	room := a.create("team")
	joinRoom(t, jn, a, b, room, RoleMember, false)
	signChainAfter(t, a, room, 300)
	joinRoom(t, jn, a, c, room, RoleAdmin, false)
	b.restart()
	if slices.Contains(b.state(room).Members, c.id) {
		t.Fatal("b knows c already")
	}
	rows := b.logRows(room)
	link := &pollLink{joinLink: joinLink{jn, b}}
	var afterFirst []int
	link.before = func(q JoinRequest) (JoinReply, bool) {
		if q.After != 0 {
			afterFirst = append(afterFirst, b.logRows(room))
		}
		return JoinReply{}, false
	}
	tok := c.invite(room, RoleMember, false)
	j := b.linkJoiner(link)
	b.preview(j, tok)
	b.accept(j, tok, 0)
	c.confirm(c.onlyRequest(room).ID)
	wantOutcome(t, b.resume(j, time.Minute), JoinStateSigned, "")
	if len(afterFirst) == 0 || afterFirst[0] != rows {
		t.Fatalf("after page one b's log has %v rows, had %d: page one must commit nothing", afterFirst, rows)
	}
	sameState(t, "b caught up through c", c.state(room), b.state(room))
	if !b.pulled(room) {
		t.Fatal("the join through a member did not catch up")
	}
}

// F7, opus point 1. A join that pinned the room in an earlier run and
// failed after the pin resumes after this machine became a member another
// way. Its admin machine was removed meanwhile, and signed the removal of
// a member it had not heard of its own removal: none of its entries are
// stored, and the member stays.
func TestResumedPinnedJoinFromANonMemberCannotWriteAfterJoiningAnotherWay(t *testing.T) {
	clock := lclock()
	a, b, x := newLNode(t, "a", clock), newLNode(t, "b", clock), newLNode(t, "x", clock)
	m, v := newLNode(t, "m", clock), newLNode(t, "v", clock)
	jn := newJoinNet()
	jn.serve(a)
	jn.serve(x)
	room := a.create("team")
	joinRoom(t, jn, a, b, room, "", false)
	joinRoom(t, jn, a, x, room, "", false)
	b.pull(room, a)
	b.confirm(b.onlyRequest(room).ID)
	a.pull(room, b)
	x.pull(room, a)
	wantNodes(t, "admins", x.state(room).Admins, a.id, b.id, x.id)
	joinRoom(t, jn, a, m, room, RoleMember, false)
	x.pull(room, a)
	a.change(room, Change{Kind: core.EntryRemove, Target: x.id})
	b.pull(room, a)
	b.confirm(b.onlyRequest(room).ID)
	a.pull(room, b)
	x.change(room, Change{Kind: core.EntryRemove, Target: m.id})

	// J1, through x: the first page pins the room, then fails.
	j := v.joiner(jn)
	tok1 := x.invite(room, RoleMember, false)
	v.preview(j, tok1)
	v.accept(j, tok1, 0)
	x.confirm(x.onlyRequest(room).ID)
	testHook = func(p string) error {
		if p == "ingest:apply" {
			return errors.New("interrupted")
		}
		return nil
	}
	_, err := j.Resume(context.Background(), time.Minute)
	testHook = nil
	if err == nil {
		t.Fatal("the interrupted join did not fail")
	}
	if _, held, _ := v.st.RoomGen(context.Background(), room); !held {
		t.Fatal("J1 did not pin the room")
	}

	// J2, through a, while x cannot be reached: v becomes a member and
	// learns that x was removed.
	jn.mu.Lock()
	jn.down[addrOf(x)] = true
	jn.mu.Unlock()
	tok2 := a.invite(room, RoleMember, false)
	v.preview(j, tok2)
	v.accept(j, tok2, 0)
	a.confirm(a.onlyRequest(room).ID)
	got := v.resume(j, time.Minute)
	signed := 0
	for _, o := range got {
		if o.State == JoinStateSigned {
			signed++
		}
	}
	if signed != 1 {
		t.Fatalf("J2: %+v", got)
	}
	wantNodes(t, "v's members", v.state(room).Members, a.id, b.id, m.id, v.id)

	// J1 resumes: nothing from x is stored.
	jn.mu.Lock()
	jn.down[addrOf(x)] = false
	jn.mu.Unlock()
	rows := v.logRows(room)
	wantOutcome(t, v.resume(j, time.Minute), JoinStateRefused, ReasonJoinServerNotMember)
	wantNodes(t, "v's members after", v.state(room).Members, a.id, b.id, m.id, v.id)
	if n := v.logRows(room); n != rows {
		t.Fatalf("v's log has %d rows, had %d", n, rows)
	}
	if _, held, _ := v.st.RoomGen(context.Background(), room); !held {
		t.Fatal("the room was left")
	}
}

// legacyJoin records, as 0.2.3 did, a join of a's room under the label
// the hostile preview showed, and sends the accept with the preview the
// server really showed. pin, as 0.2.3 did after the first page, pins the
// room by its create entry without comparing the labels.
func legacyJoin(t *testing.T, jn *joinNet, a, b *lnode, room, label string, pin bool) {
	t.Helper()
	ctx := context.Background()
	tok := mustToken(t, a.invite(room, RoleMember, false))
	var server string
	j := b.joiner(jn)
	rp, an, err := j.call(ctx, tok.Addrs, fingerprintCheck(tok, &server), JoinRequest{Op: JoinOpen, Secret: tok.SecretText()})
	if err != nil {
		t.Fatal(err)
	}
	p, err := answerPreview(rp, an, server)
	if err != nil {
		t.Fatal(err)
	}
	gen, _, err := b.st.RoomGen(ctx, room)
	if err != nil {
		t.Fatal(err)
	}
	h := secretHash(tok.Secret)
	if _, _, err := b.st.AddJoin(ctx, store.Join{Invite: h, Room: p.Room, Label: label, CreateID: p.Create, Role: p.Role,
		History: p.History, Member: p.Member, Admin: p.Admin, Addrs: tok.Addrs, Gen: gen, Expires: p.Expires, Created: *b.clock}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := j.call(ctx, tok.Addrs, func(string) error { return nil },
		JoinRequest{Op: JoinAccept, Secret: tok.SecretText(), Terms: p.ID}); err != nil {
		t.Fatal(err)
	}
	if pin {
		a.r.mu.Lock()
		rl := a.r.rooms[room]
		create := rl.ents[rl.createID].Wire()
		a.r.mu.Unlock()
		if _, err := b.r.pinJoin(ctx, create, h, gen); err != nil {
			t.Fatal(err)
		}
	}
	a.confirm(a.onlyRequest(room).ID)
}

// F12, gpt point 5. A join recorded by 0.2.3 under a label other than
// the one the room is held under here, for a room held before the join or
// pinned by that join's earlier run, is refused after a restart before any
// entry of it is applied; the room stays held and working.
func TestPendingLegacyJoinUnderAnotherLabelIsRefusedAndTheRoomStays(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		clock := lclock()
		a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
		jn := newJoinNet()
		jn.serve(a)
		room := a.create("team")
		if !pinned {
			joinRoom(t, jn, a, b, room, RoleMember, false)
		}
		legacyJoin(t, jn, a, b, room, "payroll", pinned)
		rows := b.logRows(room)
		b.restart()
		got := b.resume(b.joiner(jn), time.Minute)
		if len(got) != 1 || got[0].State != JoinStateRefused || !strings.Contains(got[0].Reason, "held here as") {
			t.Fatalf("pinned %v: outcome %+v", pinned, got)
		}
		if n := b.logRows(room); n != rows {
			t.Fatalf("pinned %v: b's log has %d rows, had %d", pinned, n, rows)
		}
		if _, held, _ := b.st.RoomGen(context.Background(), room); !held {
			t.Fatalf("pinned %v: the room was left", pinned)
		}
		if !pinned {
			b.pull(room, a)
			if !b.pulled(room) {
				t.Fatal("the room does not work after the refusal")
			}
		}
	}
}

// gpt point 6. A signed page that says more follows but holds nothing, or
// does not move past the last page, ends the join; it does not loop.
func TestJoinPageThatDoesNotMoveOnIsRefused(t *testing.T) {
	for _, mode := range []string{"empty", "same next"} {
		clock := lclock()
		a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
		jn := newJoinNet()
		jn.serve(a)
		room := a.create("team")
		calls := 0
		link := newRewriteLink(jn, b, func(an *JoinAnswer) {
			if an.State != JoinStateSigned {
				return
			}
			if calls++; calls > 20 {
				return // a joiner that loops gets an honest end instead
			}
			an.More = true
			if mode == "same next" {
				an.Next = 0
			}
		})
		j := b.linkJoiner(link)
		tok := a.invite(room, RoleMember, false)
		b.preview(j, tok)
		b.accept(j, tok, 0)
		a.confirm(a.onlyRequest(room).ID)
		wantOutcome(t, b.resume(j, time.Minute), JoinStateRefused, ReasonJoinBadPage)
		if calls > 3 {
			t.Fatalf("%s: %d signed pages read", mode, calls)
		}
		if _, held, _ := b.st.RoomGen(context.Background(), room); held {
			t.Fatalf("%s: the room is held after a refused join", mode)
		}
	}
}

// gpt point 6. A page with more entries than a page holds is refused.
func TestJoinPageOverTheLimitsIsRefused(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")
	link := newRewriteLink(jn, b, func(an *JoinAnswer) {
		if an.State == JoinStateSigned && len(an.Entries) > 0 {
			for len(an.Entries) <= MaxJoinEntries {
				an.Entries = append(an.Entries, an.Entries[len(an.Entries)-1])
			}
		}
	})
	j := b.linkJoiner(link)
	tok := a.invite(room, RoleMember, false)
	b.preview(j, tok)
	b.accept(j, tok, 0)
	a.confirm(a.onlyRequest(room).ID)
	wantOutcome(t, b.resume(j, time.Minute), JoinStateRefused, ReasonJoinBadPage)
}

// gpt point 6. A removed admin machine pages a long log of its own
// entries to a member that holds the room: the join gives up as soon as
// what it keeps passes the limit, in entries or in bytes, not at the end,
// and stores nothing.
func TestJoinOfAHeldRoomGivesUpWhenWhatItKeepsPassesTheLimit(t *testing.T) {
	for _, limit := range []string{"entries", "bytes"} {
		clock := lclock()
		a, b, x, v := newLNode(t, "a", clock), newLNode(t, "b", clock), newLNode(t, "x", clock), newLNode(t, "v", clock)
		jn := newJoinNet()
		jn.serve(a)
		jn.serve(x)
		room := a.create("team")
		joinRoom(t, jn, a, b, room, "", false)
		joinRoom(t, jn, a, x, room, "", false)
		b.pull(room, a)
		b.confirm(b.onlyRequest(room).ID)
		a.pull(room, b)
		joinRoom(t, jn, a, v, room, RoleMember, false)
		x.pull(room, a)
		a.change(room, Change{Kind: core.EntryRemove, Target: x.id})
		b.pull(room, a)
		b.confirm(b.onlyRequest(room).ID)
		a.pull(room, b)
		v.pull(room, a)
		if slices.Contains(v.state(room).Members, x.id) {
			t.Fatal("v does not know x was removed")
		}
		tok := x.invite(room, RoleMember, false)
		link := &pollLink{joinLink: joinLink{jn, v}}
		j := v.linkJoiner(link)
		v.preview(j, tok)
		v.accept(j, tok, 0)
		x.confirm(x.onlyRequest(room).ID)
		signChainAfter(t, x, room, 700)
		old, oldBytes := maxJoinPending, maxJoinPendingBytes
		if limit == "entries" {
			maxJoinPending = 300
		} else {
			maxJoinPendingBytes = 100 << 10
		}
		rows := v.logRows(room)
		got := v.resume(j, time.Minute)
		maxJoinPending, maxJoinPendingBytes = old, oldBytes
		wantOutcome(t, got, JoinStateRefused, ReasonJoinServerNotMember)
		if n := link.signedPages(); n > 2 {
			t.Fatalf("%s: %d signed pages read of 3", limit, n)
		}
		if n := v.logRows(room); n != rows {
			t.Fatalf("%s: v's log has %d rows, had %d", limit, n, rows)
		}
	}
}

// gpt point 6. Entries held here already are not kept: a member that
// holds a long log joins through an admin it has not heard of, whose
// admission comes after a full page of entries the member holds, with a
// limit lower than that page. A poll that fails between the pages is
// tried again.
func TestJoinOfAHeldRoomDoesNotChargeEntriesHeldHere(t *testing.T) {
	clock := lclock()
	a, b, c := newLNode(t, "a", clock), newLNode(t, "b", clock), newLNode(t, "c", clock)
	jn := newJoinNet()
	jn.serve(a)
	jn.serve(c)
	room := a.create("team")
	joinRoom(t, jn, a, b, room, RoleMember, false)
	signChainAfter(t, a, room, 300)
	b.pull(room, a)
	joinRoom(t, jn, a, c, room, RoleAdmin, false)
	link := &pollLink{joinLink: joinLink{jn, b}}
	failed := false
	link.before = func(q JoinRequest) (JoinReply, bool) {
		if q.After != 0 && !failed {
			failed = true
			return JoinReply{Status: 503, RetryAfter: time.Second}, true
		}
		return JoinReply{}, false
	}
	tok := c.invite(room, RoleMember, false)
	j := b.linkJoiner(link)
	b.preview(j, tok)
	b.accept(j, tok, 0)
	c.confirm(c.onlyRequest(room).ID)
	old := maxJoinPending
	maxJoinPending = 100
	got := b.resume(j, time.Minute)
	maxJoinPending = old
	wantOutcome(t, got, JoinStateSigned, "")
	if !failed {
		t.Fatal("no poll failed")
	}
	sameState(t, "b caught up through c", c.state(room), b.state(room))
}

// F12, opus point 2 and gpt point 4. A room from another machine labelled
// local, joined under an older version, with unread mail and a request
// waiting: the unread counts, the levels, the request's text and room
// label all show it with its id, the request's text is the same in
// Describe and Alerts, and approving the text shown works. An invite for
// it is not offered again under that name.
func TestLegacyRoomLabelledLocalIsShownWithItsIdEverywhere(t *testing.T) {
	ctx := context.Background()
	clock := lclock()
	net := newMemNet()
	y := newPNode(t, net, "y", newLNode(t, "y", clock))
	b := newPNode(t, net, "b", newLNode(t, "b", clock))
	room := y.create(LocalLabel)
	y.change(room, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee", History: true})
	b.pin(room, y.lnode) // as 0.2.3 held it
	s, err := OpenSurface(ctx, b.m, b.r, SurfaceOpts{Name: "b", Book: NewAddrBook()})
	if err != nil {
		t.Fatal(err)
	}
	ag, err := s.Register(ctx, "test", "bob", "/work/bob")
	if err != nil {
		t.Fatal(err)
	}
	y.register("yan")
	y.send(room, "yan", "from the other machine")
	b.mustPull(room, y, PullOpts{})
	shown := s.r.ShownLabel(room, LocalLabel)
	if shown == LocalLabel || !strings.Contains(shown, room) {
		t.Fatalf("shown %q", shown)
	}

	u, err := s.Unread(ctx, ag.ID, "", true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rc := range u.Rooms {
		if rc.Room == room {
			found = true
			if rc.Label != shown {
				t.Fatalf("unread labels the room %q", rc.Label)
			}
		} else if rc.Room != s.local && rc.Label == LocalLabel {
			t.Fatalf("unread: %+v", rc)
		}
	}
	if !found || len(u.First) == 0 {
		t.Fatalf("no unread mail from the room: %+v", u)
	}
	ls, err := s.Levels(ctx, ag.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range ls {
		if l.Room != s.local && LabelKey(l.Label) == LocalLabel {
			t.Fatalf("levels: %+v", ls)
		}
	}

	// A request waiting in the room: b is an admin there.
	y.change(room, Change{Kind: core.EntryAdmin, Target: b.id})
	b.mustPull(room, y, PullOpts{})
	q := b.propose(room, Change{Kind: core.EntryAdd, Target: fmt.Sprintf("%064x", 77), Name: "zed"})
	v, err := s.Describe(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if v.RoomLabel != shown || !strings.Contains(v.Text, room) {
		t.Fatalf("describe: %q %q", v.RoomLabel, v.Text)
	}
	as, err := s.r.Alerts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(as, func(a Alert) bool { return a.ID == q.ID }); i < 0 || as[i].Text != v.Text {
		t.Fatalf("alerts %+v, describe %q", as, v.Text)
	}
	if c, err := s.ConfirmShown(ctx, q.ID, true, true, "", v.Text); err != nil || c.State != store.RequestApproved {
		t.Fatalf("approve the text shown: %+v %v", c, err)
	}

	// An invite for the room is refused at the preview.
	jn := newJoinNet()
	jn.serve(y.lnode)
	_, err = b.joiner(jn).JoinPreview(ctx, y.invite(room, RoleMember, false))
	if err == nil || !strings.Contains(err.Error(), "reads as local") {
		t.Fatalf("preview of the held room: %v", err)
	}
}
