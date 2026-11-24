package node

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// joinNet is the in-memory transport of the join exchange: machines serve
// at an address, the joiner's check sees the serving machine's key before
// the body is handed over, and the server is given the joiner's node id.
// It counts the bodies each address received and keeps every request as
// the server got it.
type joinNet struct {
	mu     sync.Mutex
	at     map[string]*lnode
	srv    map[*Rooms]*JoinServer
	bodies map[string]int
	down   map[string]bool
	heard  []JoinRequest
}

func newJoinNet() *joinNet {
	return &joinNet{at: map[string]*lnode{}, srv: map[*Rooms]*JoinServer{}, bodies: map[string]int{}, down: map[string]bool{}}
}

func addrOf(n *lnode) string { return n.name + ".test:7443" }

func (jn *joinNet) serve(n *lnode) { jn.serveAt(addrOf(n), n) }

func (jn *joinNet) serveAt(addr string, n *lnode) {
	jn.mu.Lock()
	defer jn.mu.Unlock()
	jn.at[addr] = n
}

type joinLink struct {
	net  *joinNet
	from *lnode
}

func (l joinLink) Join(ctx context.Context, addrs []string, check func(string) error, body []byte) (JoinReply, error) {
	jn := l.net
	for _, a := range addrs {
		jn.mu.Lock()
		n, down := jn.at[a], jn.down[a]
		jn.mu.Unlock()
		if n == nil || down {
			continue
		}
		if err := check(n.id); err != nil {
			return JoinReply{}, err
		}
		jn.mu.Lock()
		jn.bodies[a]++
		s := jn.srv[n.r]
		if s == nil {
			s = NewJoinServer(n.r)
			jn.srv[n.r] = s
		}
		var q JoinRequest
		_ = json.Unmarshal(body, &q)
		jn.heard = append(jn.heard, q)
		jn.mu.Unlock()
		return s.Handle(ctx, l.from.id, "src-"+l.from.name, slices.Clone(body)), nil
	}
	return JoinReply{}, errUnreachable
}

// joiner is n's Joiner over jn; its sleeps move the shared clock.
func (n *lnode) joiner(jn *joinNet) *Joiner {
	return NewJoiner(n.r, joinLink{jn, n}, JoinerOpts{Name: n.name, Addrs: []string{addrOf(n)},
		Sleep: func(ctx context.Context, d time.Duration) error {
			*n.clock = n.clock.Add(d)
			return ctx.Err()
		}})
}

func (n *lnode) invite(room, role string, history bool) string {
	n.t.Helper()
	inv, err := n.r.Invite(context.Background(), room, role, history, []string{addrOf(n)})
	if err != nil {
		n.t.Fatalf("%s invite: %v", n.name, err)
	}
	return inv.Token
}

func (n *lnode) preview(j *Joiner, token string) Preview {
	n.t.Helper()
	p, err := j.JoinPreview(context.Background(), token)
	if err != nil {
		n.t.Fatalf("%s preview: %v", n.name, err)
	}
	return p
}

func (n *lnode) accept(j *Joiner, token string, wait time.Duration) JoinOutcome {
	n.t.Helper()
	o, err := j.JoinAccept(context.Background(), token, wait)
	if err != nil {
		n.t.Fatalf("%s accept: %v", n.name, err)
	}
	return o
}

func (n *lnode) resume(j *Joiner, wait time.Duration) []JoinOutcome {
	n.t.Helper()
	os, err := j.Resume(context.Background(), wait)
	if err != nil {
		n.t.Fatalf("%s resume: %v", n.name, err)
	}
	return os
}

// onlyRequest is the one pending request on n, for room.
func (n *lnode) onlyRequest(room string) store.Request {
	n.t.Helper()
	qs, err := n.r.Requests(context.Background())
	if err != nil {
		n.t.Fatal(err)
	}
	if len(qs) != 1 || qs[0].Room != room {
		n.t.Fatalf("%s requests: %+v", n.name, qs)
	}
	return qs[0]
}

func wantOutcome(t *testing.T, got []JoinOutcome, state, reason string) {
	t.Helper()
	if len(got) != 1 || got[0].State != state || got[0].Reason != reason {
		t.Fatalf("outcome: %+v, want %s %s", got, state, reason)
	}
}

// joinRoom runs the whole exchange: b previews and accepts a's invite, a's
// person approves, b follows to the end.
func joinRoom(t *testing.T, jn *joinNet, a, b *lnode, room, role string, history bool) {
	t.Helper()
	j := b.joiner(jn)
	tok := a.invite(room, role, history)
	b.preview(j, tok)
	if o := b.accept(j, tok, 0); o.State != JoinStateAccepted {
		t.Fatalf("accept: %+v", o)
	}
	a.confirm(a.onlyRequest(room).ID)
	wantOutcome(t, b.resume(j, time.Minute), JoinStateSigned, "")
}

// The preview shows the room, role, history and admin; the invite binds to
// the joiner's key; nothing is signed and the joiner holds nothing until a
// person on the admin machine confirms the request its accept raised; an
// accept repeated is a no-op; afterwards the joiner holds the room's log.
//
// Requirements: T3, P3
func TestJoinPreviewShowsTheRoomAndNothingIsSignedUntilTheAdminsPersonConfirms(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")
	tok := a.invite(room, RoleMember, true)
	j := b.joiner(jn)
	p := b.preview(j, tok)
	rl := a.r.rooms[room]
	want := Preview{State: JoinStatePreviewed, Room: room, Label: "team", Create: rl.createID, Role: RoleMember,
		History: true, Admin: a.id, Expires: clock.Add(JoinInviteTTL)}
	if p != want {
		t.Fatalf("preview:\n%+v\nwant\n%+v", p, want)
	}
	if qs := a.requests(room); len(qs) != 0 || a.r.signs != 0 {
		t.Fatalf("a preview raised or signed something: %+v", qs)
	}
	if o := b.accept(j, tok, 2*time.Minute); o.State != JoinStateAccepted {
		t.Fatalf("accept: %+v", o)
	}
	q := a.onlyRequest(room)
	if q.Kind != "add" || q.Action != ActionAdmit || q.Node != b.id || q.Name != "b" || !q.History || q.Role != RoleMember {
		t.Fatalf("request: %+v", q)
	}
	if a.r.signs != 0 || b.r.rooms[room] != nil {
		t.Fatal("something was signed or held before the admin's person confirmed")
	}
	// The accept repeated, after a fresh preview, raises nothing more.
	if p := b.preview(j, tok); p.State != JoinStateAccepted {
		t.Fatalf("second preview: %+v", p)
	}
	b.accept(j, tok, 0)
	if qs := a.requests(room); len(qs) != 1 {
		t.Fatalf("requests after a repeated accept: %+v", qs)
	}
	a.confirm(q.ID)
	if a.r.signs != 1 {
		t.Fatalf("signs: %d", a.r.signs)
	}
	wantOutcome(t, b.resume(j, time.Minute), JoinStateSigned, "")
	sameState(t, "after join", a.state(room), b.state(room))
	wantNodes(t, "members", b.state(room).Members, a.id, b.id)
	if js, _ := b.st.Joins(context.Background(), store.JoinSigned); len(js) != 1 || js[0].Room != room {
		t.Fatalf("join record: %+v", js)
	}
	// Nothing of the secret is stored on either machine, only its hash.
	sec := mustToken(t, tok).Secret
	for _, n := range []*lnode{a, b} {
		for _, suf := range []string{"", "-wal"} {
			raw, _ := os.ReadFile(n.path + suf)
			if bytes.Contains(raw, []byte(mustToken(t, tok).SecretText())) || bytes.Contains(raw, sec[:]) ||
				bytes.Contains(raw, []byte(hex.EncodeToString(sec[:]))) {
				t.Fatalf("the secret is stored on %s", n.name)
			}
		}
	}
}

func mustToken(t *testing.T, s string) core.Token {
	t.Helper()
	tok, err := core.DecodeToken(s)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// A person on the admin machine declines: nothing is signed and the joiner
// is told declined.
//
// Requirements: T3, P3
func TestJoinDeclinedByTheAdminsPersonIsRefusedOnTheJoiner(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")
	tok := a.invite(room, "", false)
	j := b.joiner(jn)
	b.preview(j, tok)
	b.accept(j, tok, 0)
	if _, err := a.r.Confirm(context.Background(), a.onlyRequest(room).ID, false); err != nil {
		t.Fatal(err)
	}
	wantOutcome(t, b.resume(j, time.Minute), JoinStateRefused, CodeDeclined)
	if a.r.signs != 0 || b.r.rooms[room] != nil {
		t.Fatal("a declined join signed or pinned something")
	}
	if _, err := b.joiner(jn).JoinPreview(context.Background(), tok); !errors.Is(err, ErrInviteUsed) {
		t.Fatalf("preview after decline: %v", err)
	}
}

func isRefused(err error, code string) bool {
	var r *JoinRefused
	return errors.As(err, &r) && r.Code == code
}

// An invite expires 30 minutes after it was made unless signed: an
// unopened one is refused as expired, and a request raised by an accept
// fails on confirmation and leaves the list; the joiner is told expired.
//
// Requirements: T3
func TestJoinExpiresBeforeSigning(t *testing.T) {
	clock := lclock()
	a, b, c := newLNode(t, "a", clock), newLNode(t, "b", clock), newLNode(t, "c", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")

	late := a.invite(room, "", false)
	tok := a.invite(room, "", false)
	j := b.joiner(jn)
	b.preview(j, tok)
	b.accept(j, tok, 0)
	q := a.onlyRequest(room)
	*clock = clock.Add(JoinInviteTTL)
	c2, err := a.r.Confirm(context.Background(), q.ID, true)
	if err != nil || c2.State != store.RequestFailed || c2.Reason != store.ReasonInviteExpired {
		t.Fatalf("confirm after expiry: %+v %v", c2, err)
	}
	if _, err := c.joiner(jn).JoinPreview(context.Background(), late); !isRefused(err, CodeExpired) {
		t.Fatalf("unopened invite after expiry: %v", err)
	}
	if qs, _ := a.r.Requests(context.Background()); len(qs) != 0 {
		t.Fatalf("requests after expiry: %+v", qs)
	}
	wantOutcome(t, b.resume(j, time.Minute), JoinStateExpired, CodeExpired)
	if a.r.signs != 0 {
		t.Fatal("an expired invite signed something")
	}

	// Expiry with the request still waiting: listing the requests expires it.
	tok = a.invite(room, "", false)
	j = b.joiner(jn)
	b.preview(j, tok)
	b.accept(j, tok, 0)
	*clock = clock.Add(JoinInviteTTL + time.Second)
	if qs, _ := a.r.Requests(context.Background()); len(qs) != 0 {
		t.Fatalf("an expired invite's request is still listed: %+v", qs)
	}
}

// The secret works for one key only: a second key that presents it is
// refused bad_secret, before and after the first key's join is signed.
//
// Requirements: T3
func TestJoinSecretPresentedByASecondKeyIsRefused(t *testing.T) {
	clock := lclock()
	a, b, c := newLNode(t, "a", clock), newLNode(t, "b", clock), newLNode(t, "c", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")
	tok := a.invite(room, RoleMember, false)
	jb, jc := b.joiner(jn), c.joiner(jn)
	b.preview(jb, tok)
	if _, err := jc.JoinPreview(context.Background(), tok); !isRefused(err, CodeBadSecret) {
		t.Fatalf("second key: %v", err)
	}
	b.accept(jb, tok, 0)
	a.confirm(a.onlyRequest(room).ID)
	wantOutcome(t, b.resume(jb, time.Minute), JoinStateSigned, "")
	if _, err := jc.JoinPreview(context.Background(), tok); !isRefused(err, CodeBadSecret) {
		t.Fatalf("second key after signing: %v", err)
	}
	// Nor can it poll by the invite's hash.
	body, _ := json.Marshal(JoinRequest{V: JoinVersion, Op: JoinPoll, Invite: secretHash(mustToken(t, tok).Secret)})
	if rp := NewJoinServer(a.r).Handle(context.Background(), c.id, "x", body); rp.Status != 403 || !strings.Contains(string(rp.Body), CodeBadSecret) {
		t.Fatalf("poll by another key: %d", rp.Status)
	}
	wantNodes(t, "members", a.state(room).Members, a.id, b.id)
}

// signChain signs n entries by a, each adding a machine that does not
// exist, as one chain after a's latest accepted entry.
func signChain(t *testing.T, a *lnode, room string, n int) [][]byte {
	t.Helper()
	rl := a.r.rooms[room]
	prev, seq := rl.createID, int64(1)
	var out [][]byte
	for i := 0; i < n; i++ {
		target := fmt.Sprintf("%064x", i+1)
		e, err := core.NewEntry(a.key, core.EntryFields{Room: room, Seq: seq + 1, Prev: prev,
			Parents: []string{prev}, Kind: core.EntryAdd, Target: target, Name: fmt.Sprintf("m%d", i), Created: *a.clock})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e.Wire())
		prev, seq = e.ID(), seq+1
	}
	return out
}

// Poll pages the accepted log 256 entries at a time, in causal order, and
// the joiner ends with exactly the admin's log.
//
// Requirements: T3
func TestJoinPollPagesALogLongerThan256Entries(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")
	ws := signChain(t, a, room, 300)
	if res, err := a.r.Receive(context.Background(), room, 0, ws...); err != nil || res[len(res)-1].Disposition != "accepted" {
		t.Fatalf("chain: %v %+v", err, res[len(res)-1])
	}
	joinRoom(t, jn, a, b, room, RoleMember, false)
	var pages []int64
	for _, q := range jn.heard {
		if q.Op == JoinPoll {
			pages = append(pages, q.After)
		}
	}
	// one poll while accepted, then pages from 0 and from 256
	if !slices.Equal(pages[len(pages)-2:], []int64{0, 256}) {
		t.Fatalf("poll offsets: %v", pages)
	}
	wa, _ := a.r.Offer(room)
	wb, _ := b.r.Offer(room)
	if len(wa) != 302 || len(wb) != len(wa) {
		t.Fatalf("logs: a %d, b %d", len(wa), len(wb))
	}
	sameState(t, "a and b", a.state(room), b.state(room))
}

// A joiner removed after its join was signed, before it fetched the log,
// is told removed.
//
// Requirements: T3, T5
func TestJoinerRemovedLaterIsToldRemoved(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")
	tok := a.invite(room, RoleMember, false)
	j := b.joiner(jn)
	b.preview(j, tok)
	b.accept(j, tok, 0)
	a.confirm(a.onlyRequest(room).ID)
	a.change(room, Change{Kind: core.EntryRemove, Target: b.id})
	wantOutcome(t, b.resume(j, time.Minute), JoinStateRemoved, CodeRemoved)
	if b.r.rooms[room] != nil {
		t.Fatal("a removed joiner pinned the room")
	}
}

// A wrong server key stops the joiner before the secret leaves the
// machine: the server at the address saw no request body.
//
// Requirements: T3, T7
func TestJoinWrongServerKeyStopsTheJoinerBeforeTheSecretLeaves(t *testing.T) {
	clock := lclock()
	a, b, m := newLNode(t, "a", clock), newLNode(t, "b", clock), newLNode(t, "m", clock)
	jn := newJoinNet()
	jn.serveAt(addrOf(a), m) // another machine answers at the invite's address
	room := a.create("team")
	tok := a.invite(room, "", false)
	_, err := b.joiner(jn).JoinPreview(context.Background(), tok)
	if !errors.Is(err, ErrWrongKey) {
		t.Fatalf("got %v, want wrong_key", err)
	}
	if jn.bodies[addrOf(a)] != 0 || len(jn.heard) != 0 {
		t.Fatalf("the server saw %d bodies", jn.bodies[addrOf(a)])
	}
	if strings.Contains(err.Error(), mustToken(t, tok).SecretText()[:10]) {
		t.Fatal("the error carries the secret")
	}
}

// A restart of either side, in each state, resumes: a preview survives the
// admin's restart; an accepted join survives both; a signed join is
// fetched after both restart.
//
// Requirements: T3, P3
func TestJoinRestartOfEitherSideInEveryStateResumes(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")
	tok := a.invite(room, "", true)
	j := b.joiner(jn)
	b.preview(j, tok)
	b.restart() // the secret was only in memory: the person previews again
	j = b.joiner(jn)
	if _, err := j.JoinAccept(context.Background(), tok, 0); err == nil {
		t.Fatal("accepted without a preview in this run")
	}
	b.preview(j, tok)
	a.restart()
	if p := b.preview(j, tok); p.State != JoinStatePreviewed || p.Role != RoleAdmin {
		t.Fatalf("preview after the admin restarted: %+v", p)
	}
	b.accept(j, tok, 0)
	a.restart()
	b.restart()
	j = b.joiner(jn)
	wantOutcome(t, b.resume(j, 0), JoinStateAccepted, "")
	q := a.onlyRequest(room)
	if q.Kind != "admin" || q.Action != ActionAdmitAdmin {
		t.Fatalf("request for the second machine: %+v", q)
	}
	a.confirm(q.ID)
	a.restart()
	b.restart()
	j = b.joiner(jn)
	wantOutcome(t, b.resume(j, time.Minute), JoinStateSigned, "")
	sameState(t, "after join", a.state(room), b.state(room))
	wantNodes(t, "admins", b.state(room).Admins, a.id, b.id)
	b.restart()
	if os := b.resume(b.joiner(jn), time.Minute); len(os) != 0 {
		t.Fatalf("a finished join resumed: %+v", os)
	}
	sameState(t, "after b restarts", a.state(room), b.state(room))
}

// A machine that left a room locally and is still a member joins again
// with a fresh invite: preview, accept and a person's confirmation, and no
// new grant is signed.
//
// Requirements: T3, P3
func TestJoinAgainAfterLeavingLocallySignsNoNewGrant(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")
	joinRoom(t, jn, a, b, room, RoleMember, false)
	if err := b.r.Leave(context.Background(), room); err != nil {
		t.Fatal(err)
	}
	signs := a.r.signs
	tok := a.invite(room, RoleAdmin, true)
	j := b.joiner(jn)
	if p := b.preview(j, tok); p.Role != RoleMember {
		t.Fatalf("a member's preview shows the role it has: %+v", p)
	}
	b.accept(j, tok, 0)
	q := a.onlyRequest(room)
	if q.Action != ActionRejoin || q.Node != b.id {
		t.Fatalf("request: %+v", q)
	}
	if c := a.confirm(q.ID); len(c.Entries) != 0 || c.Reason != ReasonNoGrant {
		t.Fatalf("confirmation: %+v", c)
	}
	if a.r.signs != signs {
		t.Fatal("a grant was signed for a machine that is still a member")
	}
	wantOutcome(t, b.resume(j, time.Minute), JoinStateSigned, "")
	sameState(t, "after joining again", a.state(room), b.state(room))
}

// A machine restored to a copy in which it knows no current member (the
// admin it knew was removed, the members now were added after the copy)
// catches up by joining again through a member it did not know.
//
// Requirements: T3, T9; REQUIREMENTS section 6 (restore from backup)
func TestJoinRestoredMachineThatKnowsNoCurrentMemberCatchesUp(t *testing.T) {
	clock := lclock()
	a, b, c := newLNode(t, "a", clock), newLNode(t, "b", clock), newLNode(t, "c", clock)
	jn := newJoinNet()
	jn.serve(a)
	jn.serve(c)
	room := a.create("team")
	joinRoom(t, jn, a, b, room, "", false) // second machine: admin
	snap := filepath.Join(t.TempDir(), "b-copy.db")
	b.st.Close()
	copyDB(t, b.path, snap)
	b.open()

	joinRoom(t, jn, a, c, room, "", false) // third: added, and promoted by a
	b.pull(room, a)
	b.confirm(b.onlyRequest(room).ID) // b seconds c's promotion
	a.pull(room, b)
	c.pull(room, b)
	wantNodes(t, "admins", c.state(room).Admins, a.id, b.id, c.id)
	b.change(room, Change{Kind: core.EntryRemove, Target: a.id})
	c.pull(room, b)
	c.confirm(c.onlyRequest(room).ID)
	wantNodes(t, "members now", c.state(room).Members, b.id, c.id)

	b.st.Close()
	copyDB(t, snap, b.path)
	b.open()
	wantNodes(t, "the restored copy's members", b.state(room).Members, a.id, b.id)
	tok := c.invite(room, "", false)
	j := b.joiner(jn)
	b.preview(j, tok)
	b.accept(j, tok, 0)
	if q := c.onlyRequest(room); q.Action != ActionRejoin || q.Node != b.id {
		t.Fatalf("request on c: %+v", q)
	}
	signs := c.r.signs
	c.confirm(c.onlyRequest(room).ID)
	wantOutcome(t, b.resume(j, time.Minute), JoinStateSigned, "")
	if c.r.signs != signs {
		t.Fatal("a grant was signed for a machine that is still a member")
	}
	sameState(t, "restored b caught up", c.state(room), b.state(room))
}

// Bootstrap from nothing over the join exchange: three machines reach
// three admins with exactly the confirmations DESIGN-next section 7 lists
// (the second: its admin and itself; the third: the admitting admin, the
// other admin seconding, and itself) and no others; then fourth machines
// join as plain members, with and without history, and their agents'
// inboxes start at the admitting admin's position.
//
// Requirements: T3, T9, P3, R8; members rules M3, M4
func TestJoinBootstrapThreeMachinesFromNothingThenAFourthAsAMember(t *testing.T) {
	clock := lclock()
	a, b, c := newLNode(t, "a", clock), newLNode(t, "b", clock), newLNode(t, "c", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")
	confirmed := map[string]int{}
	approve := func(n *lnode) Confirmation {
		t.Helper()
		confirmed[n.name]++
		return n.confirm(n.onlyRequest(room).ID)
	}
	accept := func(n *lnode, admin *lnode, tok string) *Joiner {
		t.Helper()
		j := n.joiner(jn)
		n.preview(j, tok)
		confirmed[n.name]++ // the person on the joiner confirms the preview
		n.accept(j, tok, 0)
		return j
	}

	// Second machine: one admin entry, confirmed on the admin and the joiner.
	jb := accept(b, a, a.invite(room, "", false))
	if got := approve(a); len(got.Entries) != 1 || got.Effect != EffectApplied {
		t.Fatalf("second machine: %+v", got)
	}
	wantOutcome(t, b.resume(jb, time.Minute), JoinStateSigned, "")
	wantNodes(t, "admins", b.state(room).Admins, a.id, b.id)

	// Third: the add and a's admin entry under one confirmation; it is a
	// member at once and its promotion waits for b.
	tok := a.invite(room, "", false)
	jc := c.joiner(jn)
	if p := c.preview(jc, tok); p.Role != RoleAdmin {
		t.Fatalf("third machine's preview: %+v", p)
	}
	confirmed["c"]++
	c.accept(jc, tok, 0)
	got := approve(a)
	if len(got.Entries) != 2 || got.Effect != EffectPending {
		t.Fatalf("third machine: %+v", got)
	}
	wantOutcome(t, c.resume(jc, time.Minute), JoinStateSigned, "")
	wantNodes(t, "members", c.state(room).Members, a.id, b.id, c.id)
	if info, _ := c.r.Info(context.Background(), room); len(info.Pending) != 1 || info.Pending[0].Target != c.id {
		t.Fatalf("c sees its promotion waiting: %+v", info.Pending)
	}
	b.pull(room, a)
	if q := b.onlyRequest(room); q.Seconds != got.Entry || q.Node != c.id {
		t.Fatalf("b's request: %+v", q)
	}
	if got := approve(b); got.Effect != EffectApplied {
		t.Fatalf("b seconds: %+v", got)
	}
	a.pull(room, b)
	c.pull(room, b)
	for _, n := range []*lnode{a, b, c} {
		wantNodes(t, n.name+" admins", n.state(room).Admins, a.id, b.id, c.id)
		if qs := n.requests(room); len(qs) != 0 {
			t.Fatalf("%s has requests left: %+v", n.name, qs)
		}
	}
	if want := map[string]int{"a": 2, "b": 2, "c": 1}; fmt.Sprint(confirmed) != fmt.Sprint(want) {
		t.Fatalf("confirmations %v, want %v", confirmed, want)
	}
	if a.r.signs != 3 || b.r.signs != 1 || c.r.signs != 0 {
		t.Fatalf("signs: a %d, b %d, c %d", a.r.signs, b.r.signs, c.r.signs)
	}

	// Fourth machines: plain members by default, with and without history.
	a.register("alice")
	m1 := a.send(room, "alice", "before")
	for _, history := range []bool{true, false} {
		d := newLNode(t, fmt.Sprintf("d-%v", history), clock)
		tok := a.invite(room, "", history)
		jd := d.joiner(jn)
		if p := d.preview(jd, tok); p.Role != RoleMember || p.History != history {
			t.Fatalf("fourth machine's preview: %+v", p)
		}
		d.accept(jd, tok, 0)
		if q := a.onlyRequest(room); q.Kind != "add" || q.Action != ActionAdmit || q.History != history {
			t.Fatalf("fourth machine's request: %+v", q)
		}
		a.confirm(a.onlyRequest(room).ID)
		wantOutcome(t, d.resume(jd, time.Minute), JoinStateSigned, "")
		d.register("dan")
		m2 := a.send(room, "alice", "after")
		for _, r := range a.records(room) {
			d.take(r)
		}
		if es := d.inbox(room, "dan"); len(es) != 1 || es[0].ID != m2 {
			t.Fatalf("history %v: inbox %+v", history, es)
		}
		want := map[bool]string{false: store.ReasonNotAdmitted, true: store.ReasonLive}[history]
		if h, _ := d.held(room, m1); h.Reason != want {
			t.Fatalf("history %v: earlier text %+v", history, h)
		}
		wantNodes(t, "admins", d.state(room).Admins, a.id, b.id, c.id)
	}
}

// Join requests are strict JSON with the fields each operation takes, and
// opens are limited to 10 a minute per source address.
//
// Requirements: T3; PROTOCOL-next sections 0 and 5.3
func TestJoinRequestsAreStrictAndOpensAreRateLimited(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	room := a.create("team")
	tok := mustToken(t, a.invite(room, "", false))
	s := NewJoinServer(a.r)
	ctx := context.Background()
	secret := tok.SecretText()
	for _, c := range []struct{ name, body string }{
		{"duplicate key", `{"v":2,"op":"open","secret":"` + secret + `","name":"b","name":"c"}`},
		{"case variant", `{"v":2,"op":"open","Secret":"` + secret + `","name":"b"}`},
		{"null", `{"v":2,"op":"open","secret":"` + secret + `","name":"b","x":null}`},
		{"after zero on open", `{"v":2,"op":"open","secret":"` + secret + `","name":"b","after":0}`},
		{"after on open", `{"v":2,"op":"open","secret":"` + secret + `","name":"b","after":3}`},
		{"empty addrs", `{"v":2,"op":"open","secret":"` + secret + `","name":"b","addrs":[]}`},
		{"no name", `{"v":2,"op":"open","secret":"` + secret + `"}`},
		{"version", `{"v":1,"op":"open","secret":"` + secret + `","name":"b"}`},
		{"secret and invite", `{"v":2,"op":"poll","secret":"` + secret + `","invite":"` + strings.Repeat("a", 64) + `"}`},
		{"trailing data", `{"v":2,"op":"open","secret":"` + secret + `","name":"b"} x`},
	} {
		// The case's name only: bodies carry the secret and never go in output.
		if rp := s.Handle(ctx, b.id, "src", []byte(c.body)); rp.Status != http.StatusBadRequest {
			t.Fatalf("%s: %d", c.name, rp.Status)
		}
	}
	ok := fmt.Sprintf(`{"v":2,"op":"open","secret":%q,"name":"b","unknown":[1]}`, strings.ToLower(secret))
	for i := 0; i < OpensPerMinute; i++ {
		if rp := s.Handle(ctx, b.id, "src", []byte(ok)); rp.Status != http.StatusOK {
			t.Fatalf("open %d: %d", i, rp.Status)
		}
	}
	rp := s.Handle(ctx, b.id, "src", []byte(ok))
	if rp.Status != http.StatusTooManyRequests || rp.RetryAfter <= 0 || rp.RetryAfter > time.Minute {
		t.Fatalf("eleventh open: %d %v", rp.Status, rp.RetryAfter)
	}
	if rp := s.Handle(ctx, b.id, "other", []byte(ok)); rp.Status != http.StatusOK {
		t.Fatalf("another source: %d", rp.Status)
	}
	*clock = clock.Add(time.Minute)
	if rp := s.Handle(ctx, b.id, "src", []byte(ok)); rp.Status != http.StatusOK {
		t.Fatalf("a minute later: %d", rp.Status)
	}
	if strings.Contains(string(rp.Body), secret) {
		t.Fatal("an answer carries the secret")
	}
}

// A request whose revalidation fails when a person approves it (here the
// joiner was added meanwhile by another path) signs nothing, and the
// joiner is told revoked.
//
// Requirements: T3, P3
func TestJoinWhoseRequestFailsRevalidationIsRevoked(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")
	tok := a.invite(room, RoleMember, false)
	j := b.joiner(jn)
	b.preview(j, tok)
	b.accept(j, tok, 0)
	q := a.onlyRequest(room)
	a.change(room, Change{Kind: core.EntryAdd, Target: b.id, Name: "b"})
	signs := a.r.signs
	c, err := a.r.Confirm(context.Background(), q.ID, true)
	if err != nil || c.State != store.RequestFailed {
		t.Fatalf("confirm: %+v %v", c, err)
	}
	if a.r.signs != signs {
		t.Fatal("a failed revalidation signed something")
	}
	wantOutcome(t, b.resume(j, time.Minute), JoinStateRefused, CodeRevoked)
}
