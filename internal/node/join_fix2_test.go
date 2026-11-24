package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// A signed answer read before a leave never pins the room afterwards:
// the room is pinned and left between the joiner's reading of the answer
// and its pin, and the pin, which checks the join and the generation in
// its own write, refuses.
//
// Requirements: T3; second review blocker 1
func TestJoinSignedAnswerReadBeforeALeaveNeverPins(t *testing.T) {
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
	testHook = func(p string) error {
		if p == "join:pin" {
			testHook = nil
			b.pin(room, a) // another path pins it, and the person leaves
			if err := b.r.Leave(context.Background(), room); err != nil {
				t.Error(err)
			}
		}
		return nil
	}
	defer func() { testHook = nil }()
	wantOutcome(t, b.resume(j, time.Minute), JoinStateRefused, ReasonJoinLeft)
	if b.r.rooms[room] != nil {
		t.Fatal("a signed answer read before the leave pinned the room after it")
	}
}

// A preview has an identifier over everything it showed; accept carries
// it, and fails unless it is the preview stored here, which a later
// preview of an approved invite does not replace. On the wire, an accept
// whose terms are not what the server last showed that key is revoked,
// and an accept for an invite never opened is refused.
//
// Requirements: T3, P3; second review blocker 2
func TestJoinAcceptNeedsTheApprovedPreviewIdentifier(t *testing.T) {
	clock := lclock()
	a, b, c := newLNode(t, "a", clock), newLNode(t, "b", clock), newLNode(t, "c", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")
	ctx := context.Background()
	joinRoom(t, jn, a, b, room, RoleMember, false)
	if err := b.r.Leave(ctx, room); err != nil {
		t.Fatal(err)
	}
	tok := a.invite(room, RoleMember, false)
	j := b.joiner(jn)
	p1 := b.preview(j, tok)
	if p1.ID == "" || p1.ID != previewID(p1) {
		t.Fatalf("preview id: %+v", p1)
	}
	a.change(room, Change{Kind: core.EntryAdmin, Target: b.id}) // what the next preview shows changes
	p2 := b.preview(j, tok)
	if p2.ID == p1.ID || p2.Role != RoleAdmin {
		t.Fatalf("second preview: %+v", p2)
	}
	if _, err := j.JoinAccept(ctx, tok, p1.ID, 0); !errors.Is(err, ErrPreviewChanged) {
		t.Fatalf("accept of a preview no longer stored: %v", err)
	}
	if o, err := j.JoinAccept(ctx, tok, p2.ID, 0); err != nil || o.State != JoinStateAccepted {
		t.Fatalf("accept: %+v %v", o, err)
	}
	if p := b.preview(j, tok); p.ID != p2.ID || p.State != JoinStateAccepted {
		t.Fatalf("a preview after the accept: %+v", p)
	}

	s := NewJoinServer(a.r)
	t2 := mustToken(t, a.invite(room, RoleMember, false))
	accept := func(terms string) JoinReply {
		return s.Handle(ctx, c.id, "x", []byte(fmt.Sprintf(`{"v":2,"op":"accept","secret":%q,"name":"c","terms":%q}`,
			t2.SecretText(), terms)))
	}
	if rp := accept(fmt.Sprintf("%064x", 1)); rp.Status != http.StatusBadRequest {
		t.Fatalf("accept of an invite never opened: %d", rp.Status)
	}
	if rp := s.Handle(ctx, c.id, "x", []byte(fmt.Sprintf(`{"v":2,"op":"open","secret":%q,"name":"c"}`, t2.SecretText()))); rp.Status != http.StatusOK {
		t.Fatalf("open: %d", rp.Status)
	}
	if rp := accept(fmt.Sprintf("%064x", 1)); rp.Status != http.StatusForbidden || !jsonHas(rp.Body, CodeRevoked) {
		t.Fatalf("accept of terms never shown: %d", rp.Status)
	}
}

// The log is the truth: a promotion that lands between the approval and
// the joiner's fetch is reported as the role the joiner's own state shows,
// with a note, not as a failed join.
//
// Requirements: T3; second review blocker 3
func TestJoinReportsTheRoleItsOwnLogShows(t *testing.T) {
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
	a.change(room, Change{Kind: core.EntryAdmin, Target: b.id})
	os := b.resume(j, time.Minute)
	if len(os) != 1 || os[0].State != JoinStateSigned || os[0].Role != RoleAdmin || os[0].History || os[0].Note == "" {
		t.Fatalf("outcome: %+v", os)
	}
	sameState(t, "a and b", a.state(room), b.state(room))
}

// heldWires signs n entries by a that wait for an entry nobody has.
func heldWires(t *testing.T, a *lnode, room string, from, n int) [][]byte {
	t.Helper()
	var out [][]byte
	for i := from; i < from+n; i++ {
		missing := fmt.Sprintf("%032x", i+1)
		e, err := core.NewEntry(a.key, core.EntryFields{Room: room, Seq: int64(5000 + i), Prev: missing,
			Parents: []string{missing}, Kind: core.EntryAdd, Target: fmt.Sprintf("%064x", 7000+i), Name: "h", Created: *a.clock})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e.Wire())
	}
	return out
}

// Paging carries a position allocated once per stored entry: an earlier
// held row evicted between pages skips nothing, every answer's next is
// the joiner's next after, and the joiner ends with every accepted head.
//
// Requirements: T3; second review blocker 4 and note (paging across an eviction)
func TestJoinPagingContinuesAcrossAnEvictedHeldRow(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")
	ctx := context.Background()
	receive := func(ws [][]byte) {
		for len(ws) > 0 {
			k := min(len(ws), 200)
			if _, err := a.r.Receive(ctx, room, 0, ws[:k]...); err != nil {
				t.Fatal(err)
			}
			ws = ws[k:]
		}
	}
	receive(heldWires(t, a, room, 0, 1)) // the oldest held row, early in the log
	receive(signChain(t, a, room, 300))  // accepted, across the page boundary
	tok := a.invite(room, RoleMember, false)
	var nexts, afters []int64
	var once sync.Once
	j := b.hookedJoiner(jn, hookLink{after: func(q JoinRequest, rp JoinReply) (JoinReply, error) {
		if q.Op == JoinPoll && rp.Status == http.StatusOK {
			afters = append(afters, q.After)
			var a2 JoinAnswer
			_ = jsonDecode(rp.Body, &a2)
			nexts = append(nexts, a2.Next)
			once.Do(func() { receive(heldWires(t, a, room, 1, 1024)) }) // evicts the oldest held row
		}
		return rp, nil
	}})
	b.preview(j, tok)
	b.accept(j, tok, 0)
	a.confirm(a.onlyRequest(room).ID)
	os := b.resume(j, time.Minute)
	if len(os) != 1 || os[0].State != JoinStateSigned {
		t.Fatalf("outcome: %+v", os)
	}
	if _, held := a.r.rooms[room].state[mustID(t, heldWires(t, a, room, 0, 1)[0])]; held {
		t.Fatal("the oldest held row was not evicted")
	}
	for i := 1; i < len(afters); i++ {
		if afters[i] != nexts[i-1] {
			t.Fatalf("after %v, next %v", afters, nexts)
		}
	}
	sameState(t, "a and b", a.state(room), b.state(room))
	if !slices.Equal(acceptedHeads(a.r.rooms[room]), acceptedHeads(b.r.rooms[room])) {
		t.Fatal("heads differ")
	}
}

func mustID(t *testing.T, w []byte) string {
	t.Helper()
	e, err := core.ParseEntry(w)
	if err != nil {
		t.Fatal(err)
	}
	return e.ID()
}

// Propose refuses to admit a machine that is not a member: admission goes
// through the invite path. Promoting and removing members are unchanged.
//
// Requirements: T3, P3; second review blocker 5
func TestProposeRefusesAdmissionWithoutAnInvite(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")
	ctx := context.Background()
	for _, c := range []Change{
		{Kind: core.EntryAdd, Target: b.id, Name: "b"},
		{Kind: core.EntryAdd, Target: b.id, Name: "b", Admin: true},
		{Kind: core.EntryAdmin, Target: b.id, Name: "b"},
	} {
		if _, err := a.r.Propose(ctx, room, c); err == nil || err.Error() != ReasonNeedsInvite {
			t.Fatalf("admission without an invite %+v: %v", c, err)
		}
	}
	joinRoom(t, jn, a, b, room, RoleMember, false)
	if _, err := a.r.Propose(ctx, room, Change{Kind: core.EntryAdmin, Target: b.id}); err != nil {
		t.Fatalf("promotion: %v", err)
	}
}

// The clock that judges an invite's expiry is read inside the write, for
// accept and for approval, not when the handler started.
//
// Requirements: T3; second review should-fix 6
func TestJoinExpiryClockIsReadInsideTheWrite(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	room := a.create("team")
	ctx := context.Background()
	s := NewJoinServer(a.r)
	tok := mustToken(t, a.invite(room, RoleMember, false))
	rp := s.Handle(ctx, b.id, "x", []byte(fmt.Sprintf(`{"v":2,"op":"open","secret":%q,"name":"b"}`, tok.SecretText())))
	var p JoinAnswer
	if err := jsonDecode(rp.Body, &p); err != nil || rp.Status != http.StatusOK {
		t.Fatalf("open: %d", rp.Status)
	}
	terms := previewID(answerToPreview(t, p))
	// The accept waits for the room; meanwhile the invite expires.
	testHook = func(p string) error {
		if p == "join:accept" {
			*clock = clock.Add(JoinInviteTTL)
		}
		return nil
	}
	rp = s.Handle(ctx, b.id, "x", []byte(fmt.Sprintf(`{"v":2,"op":"accept","secret":%q,"name":"b","terms":%q}`,
		tok.SecretText(), terms)))
	testHook = nil
	if rp.Status != http.StatusForbidden || !jsonHas(rp.Body, CodeExpired) {
		t.Fatalf("accept that waited past the expiry: %d", rp.Status)
	}
	if qs := a.requests(room); len(qs) != 0 {
		t.Fatalf("requests: %+v", qs)
	}
}

func answerToPreview(t *testing.T, a JoinAnswer) Preview {
	t.Helper()
	p, err := answerPreview(JoinReply{Status: http.StatusOK}, a, a.Admin)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// While the shared budget for polls naming unknown invites is spent, a
// poll naming an invite is answered before any lookup, known or not.
//
// Requirements: T3; second review should-fix 7
func TestJoinUnknownPollBudgetIsCheckedBeforeTheLookup(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")
	ctx := context.Background()
	tok := a.invite(room, RoleMember, false)
	j := b.joiner(jn)
	b.preview(j, tok)
	b.accept(j, tok, 0)
	s := jn.srv[a.r]
	for i := 0; i < unknownPolls; i++ {
		s.Handle(ctx, b.id, "x", []byte(fmt.Sprintf(`{"v":2,"op":"poll","invite":"%064x"}`, i+1)))
	}
	h := secretHash(mustToken(t, tok).Secret)
	if rp := s.Handle(ctx, b.id, "x", []byte(`{"v":2,"op":"poll","invite":"`+h+`"}`)); rp.Status != http.StatusTooManyRequests {
		t.Fatalf("a poll while the budget is spent: %d", rp.Status)
	}
}

// A poll that comes back after the room was left reports the join ended;
// the request list shows an invite request's deadline; confirming one
// whose invite expired returns failed with the recorded reason.
//
// Requirements: T3; second review should-fix 8
func TestJoinStaleViewsReportWhatIsRecorded(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")
	ctx := context.Background()
	joinRoom(t, jn, a, b, room, RoleMember, false)
	tok := a.invite(room, RoleMember, false)
	var once sync.Once
	j := b.hookedJoiner(jn, hookLink{after: func(q JoinRequest, rp JoinReply) (JoinReply, error) {
		if q.Op == JoinAccept {
			once.Do(func() {
				if err := b.r.Leave(ctx, room); err != nil {
					t.Error(err)
				}
			})
		}
		return rp, nil
	}})
	b.preview(j, tok)
	if o := b.accept(j, tok, 0); o.State != JoinStateRefused || o.Reason != ReasonJoinLeft {
		t.Fatalf("a join whose room was left while it polled: %+v", o)
	}
	qs, err := a.r.Requests(ctx)
	if err != nil || len(qs) != 1 || !qs[0].Deadline.Equal(clock.Add(JoinInviteTTL)) {
		t.Fatalf("requests: %+v %v", qs, err)
	}
	*clock = clock.Add(JoinInviteTTL)
	if _, err := a.r.Requests(ctx); err != nil { // records the expiry
		t.Fatal(err)
	}
	if cf, err := a.r.Confirm(ctx, qs[0].ID, true); err != nil || cf.State != store.RequestFailed || cf.Reason != store.ReasonInviteExpired {
		t.Fatalf("confirming an expired request: %+v %v", cf, err)
	}
}

// Two accepts contend for the last request slot: one is accepted, the
// other is told the queue is full, with Retry-After.
//
// Requirements: T3; second review note (contention for the last slot)
func TestJoinConcurrentAcceptsForTheLastRequestSlot(t *testing.T) {
	clock := lclock()
	a, b, c := newLNode(t, "a", clock), newLNode(t, "b", clock), newLNode(t, "c", clock)
	room := a.create("team")
	ctx := context.Background()
	for i := 0; i < MaxPendingRequests-1; i++ {
		if _, err := a.r.proposeAny(ctx, room, Change{Kind: core.EntryAdd, Target: fmt.Sprintf("%064x", i+1), Name: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	s := NewJoinServer(a.r)
	var codes [2]int
	var wg sync.WaitGroup
	for i, n := range []*lnode{b, c} {
		tok := mustToken(t, a.invite(room, RoleMember, false))
		rp := s.Handle(ctx, n.id, "x", []byte(fmt.Sprintf(`{"v":2,"op":"open","secret":%q,"name":%q}`, tok.SecretText(), n.name)))
		var p JoinAnswer
		if err := jsonDecode(rp.Body, &p); err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`{"v":2,"op":"accept","secret":%q,"name":%q,"terms":%q}`, tok.SecretText(), n.name,
			previewID(answerToPreview(t, p)))
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = s.Handle(ctx, n.id, "x", []byte(body)).Status
		}()
	}
	wg.Wait()
	slices.Sort(codes[:])
	if codes != [2]int{http.StatusAccepted, http.StatusServiceUnavailable} {
		t.Fatalf("accepts: %v", codes)
	}
}

// Bootstrap with default-role invites, in both confirmation orders: with
// one admin, two waiting admin joins race; the first confirmed wins, the
// other fails because the admin count changed, and its fresh invite
// produces an add and a promotion that the second admin's confirmation
// completes, giving three admins.
//
// Requirements: T3, T9; second review note (both orders)
func TestJoinBootstrapRaceInBothConfirmationOrders(t *testing.T) {
	for _, first := range []string{"b", "c"} {
		t.Run(first+" first", func(t *testing.T) {
			clock := lclock()
			a, b, c := newLNode(t, "a", clock), newLNode(t, "b", clock), newLNode(t, "c", clock)
			jn := newJoinNet()
			jn.serve(a)
			room := a.create("team")
			ctx := context.Background()
			win, lose := b, c
			if first == "c" {
				win, lose = c, b
			}
			jw, jl := win.joiner(jn), lose.joiner(jn)
			tw, tl := a.invite(room, "", false), a.invite(room, "", false)
			win.preview(jw, tw)
			lose.preview(jl, tl)
			win.accept(jw, tw, 0)
			lose.accept(jl, tl, 0)
			var qw, ql store.Request
			for _, q := range a.requests(room) {
				if q.Node == win.id {
					qw = q
				} else {
					ql = q
				}
			}
			a.confirm(qw.ID)
			if cf, err := a.r.Confirm(ctx, ql.ID, true); err != nil || cf.State != store.RequestFailed || cf.Reason != ReasonQuorumMoved {
				t.Fatalf("the loser: %+v %v", cf, err)
			}
			wantOutcome(t, win.resume(jw, time.Minute), JoinStateSigned, "")
			wantOutcome(t, lose.resume(jl, time.Minute), JoinStateRefused, CodeRevoked)

			tl = a.invite(room, "", false)
			jl = lose.joiner(jn)
			lose.preview(jl, tl)
			lose.accept(jl, tl, 0)
			q := a.onlyRequest(room)
			if q.Kind != string(core.EntryAdd) || q.Action != ActionAdmitAdmin {
				t.Fatalf("the fresh request: %+v", q)
			}
			a.confirm(q.ID)
			win.pull(room, a)
			win.confirm(win.onlyRequest(room).ID) // the second admin's confirmation
			a.pull(room, win)
			os := lose.resume(jl, time.Minute)
			if len(os) != 1 || os[0].State != JoinStateSigned {
				t.Fatalf("outcome: %+v", os)
			}
			lose.pull(room, a)
			wantNodes(t, "admins", a.state(room).Admins, a.id, b.id, c.id)
			sameState(t, "a and the third", a.state(room), lose.state(room))
		})
	}
}

func jsonDecode(b []byte, v any) error { return json.Unmarshal(b, v) }
