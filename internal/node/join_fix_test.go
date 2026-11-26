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

// hookLink is joinLink with faults: before may stop a request before it
// is delivered; after may replace what the server answered.
type hookLink struct {
	joinLink
	before func(q JoinRequest) error
	after  func(q JoinRequest, rp JoinReply) (JoinReply, error)
}

func (l hookLink) Join(ctx context.Context, addrs []string, check func(string) error, body []byte) (JoinReply, error) {
	var q JoinRequest
	_ = json.Unmarshal(body, &q)
	if l.before != nil {
		if err := l.before(q); err != nil {
			return JoinReply{}, err
		}
	}
	rp, err := l.joinLink.Join(ctx, addrs, check, body)
	if err == nil && l.after != nil {
		return l.after(q, rp)
	}
	return rp, err
}

func (n *lnode) hookedJoiner(jn *joinNet, l hookLink) *Joiner {
	l.joinLink = joinLink{jn, n}
	j := n.joiner(jn)
	j.t = l
	return j
}

var errJoinLost = errors.New("connection lost")

// An invite used once cannot be used again, and a leave ends a join in
// progress: a preview made before the leave cannot be accepted after it,
// an approval that lands after the leave pins nothing, and the old token
// is refused on this machine.
//
// Requirements: T3; review blocker 1
func TestJoinOldInviteCannotUndoALeave(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")
	ctx := context.Background()
	tok1 := a.invite(room, RoleMember, false)
	j := b.joiner(jn)
	b.preview(j, tok1)
	b.accept(j, tok1, 0)
	a.confirm(a.onlyRequest(room).ID)
	wantOutcome(t, b.resume(j, time.Minute), JoinStateSigned, "")

	tokX, tokY := a.invite(room, RoleMember, false), a.invite(room, RoleMember, false)
	if p := b.preview(j, tokX); !p.Member {
		t.Fatalf("a member's preview: %+v", p)
	}
	b.accept(j, tokX, 0)
	b.preview(j, tokY)
	if err := b.r.Leave(ctx, room); err != nil {
		t.Fatal(err)
	}
	if _, err := j.JoinAccept(ctx, tokY, previewOf(j, tokY), 0); !errors.Is(err, ErrRoomChanged) {
		t.Fatalf("accept of a preview made before the leave: %v", err)
	}
	a.confirm(a.onlyRequest(room).ID)
	if os := b.resume(b.joiner(jn), time.Minute); len(os) != 0 {
		t.Fatalf("a join ended by the leave resumed: %+v", os)
	}
	if b.r.rooms[room] != nil {
		t.Fatal("an approval that landed after the leave pinned the room again")
	}
	for _, tok := range []string{tok1, tokX} {
		if _, err := b.joiner(jn).JoinPreview(ctx, tok); !errors.Is(err, ErrInviteUsed) {
			t.Fatalf("an old invite previewed again: %v", err)
		}
	}
}

// The joiner records the approved preview before the accept leaves: if
// the accept's answer is lost and the joiner restarts, it resumes polling;
// if it crashed after recording and before sending, a poll finds the
// invite only previewed and the person accepts again, on the recorded
// terms, without previewing.
//
// Requirements: T3; review blocker 2 and should-fix 9
func TestJoinAcceptAnswerLostOrCrashBeforeSendingResumes(t *testing.T) {
	clock := lclock()
	a, b, c := newLNode(t, "a", clock), newLNode(t, "b", clock), newLNode(t, "c", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")
	ctx := context.Background()

	tok := a.invite(room, RoleMember, false)
	j := b.hookedJoiner(jn, hookLink{after: func(q JoinRequest, rp JoinReply) (JoinReply, error) {
		if q.Op == JoinAccept {
			return JoinReply{}, errJoinLost
		}
		return rp, nil
	}})
	b.preview(j, tok)
	if _, err := j.JoinAccept(ctx, tok, previewOf(j, tok), 0); !errors.Is(err, errJoinLost) {
		t.Fatalf("accept: %v", err)
	}
	b.restart()
	wantOutcome(t, b.resume(b.joiner(jn), 0), JoinStateAccepted, "")
	a.confirm(a.onlyRequest(room).ID)
	wantOutcome(t, b.resume(b.joiner(jn), time.Minute), JoinStateSigned, "")

	tok = a.invite(room, RoleMember, true)
	j = c.hookedJoiner(jn, hookLink{before: func(q JoinRequest) error {
		if q.Op == JoinAccept {
			return errJoinLost
		}
		return nil
	}})
	c.preview(j, tok)
	if _, err := j.JoinAccept(ctx, tok, previewOf(j, tok), 0); !errors.Is(err, errJoinLost) {
		t.Fatalf("accept: %v", err)
	}
	c.restart()
	wantOutcome(t, c.resume(c.joiner(jn), time.Minute), JoinStatePreviewed, ReasonNotAccepted)
	if qs := a.requests(room); len(qs) != 0 {
		t.Fatalf("requests before the accept arrived: %+v", qs)
	}
	if o := c.accept(c.joiner(jn), tok, 0); o.State != JoinStateAccepted {
		t.Fatalf("accept again: %+v", o)
	}
	if q := a.onlyRequest(room); q.Node != c.id || !q.History {
		t.Fatalf("request: %+v", q)
	}
}

// A member's preview shows the role and history it has and that nothing
// will change; if a promotion changes them after the preview, the accept
// is refused, and after the accept, the approval fails.
//
// Requirements: T3, P3; review blocker 3
func TestJoinRejoinWhoseTermsChangedIsRefused(t *testing.T) {
	clock := lclock()
	a, b, c := newLNode(t, "a", clock), newLNode(t, "b", clock), newLNode(t, "c", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")
	ctx := context.Background()
	joinRoom(t, jn, a, b, room, RoleMember, false)
	room2 := a.create("other") // one admin in each, so a alone promotes
	joinRoom(t, jn, a, c, room2, RoleMember, false)
	if err := b.r.Leave(ctx, room); err != nil {
		t.Fatal(err)
	}
	if err := c.r.Leave(ctx, room2); err != nil {
		t.Fatal(err)
	}

	// b: the promotion lands between the preview and the accept.
	tok := a.invite(room, RoleAdmin, true)
	jb := b.joiner(jn)
	p := b.preview(jb, tok)
	if !p.Member || p.Role != RoleMember || p.History {
		t.Fatalf("a member's preview: %+v", p)
	}
	a.change(room, Change{Kind: core.EntryAdmin, Target: b.id})
	if o := b.accept(jb, tok, 0); o.State != JoinStateRefused || o.Reason != CodeRevoked {
		t.Fatalf("accept after the terms changed: %+v", o)
	}
	if qs := a.requests(room); len(qs) != 0 {
		t.Fatalf("requests: %+v", qs)
	}

	// c: the promotion lands between the accept and the approval.
	tok = a.invite(room2, RoleMember, false)
	jc := c.joiner(jn)
	c.preview(jc, tok)
	c.accept(jc, tok, 0)
	q := a.onlyRequest(room2)
	if q.Action != ActionRejoin || q.Role != RoleMember || q.History {
		t.Fatalf("rejoin request: %+v", q)
	}
	a.change(room2, Change{Kind: core.EntryAdmin, Target: c.id})
	if cf, err := a.r.Confirm(ctx, q.ID, true); err != nil || cf.State != store.RequestFailed || cf.Reason != ReasonTermsMoved {
		t.Fatalf("approval after the terms changed: %+v %v", cf, err)
	}
	wantOutcome(t, c.resume(jc, time.Minute), JoinStateRefused, CodeRevoked)
}

// Poll pages the stored log in arrival order: an entry stored while the
// joiner is between pages comes after every index already served, and the
// joiner ends with exactly the entries the admin holds, even when it
// crashes between pages and starts again.
//
// Requirements: T3; review blocker 4 and should-fix 9
func TestJoinPagesInArrivalOrderAcrossAConcurrentEntryAndACrash(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")
	ctx := context.Background()
	ws := signChain(t, a, room, 300)
	if _, err := a.r.Receive(ctx, room, 0, ws...); err != nil {
		t.Fatal(err)
	}
	tok := a.invite(room, RoleMember, false)
	cctx, cancel := context.WithCancel(ctx)
	var once sync.Once
	j := b.hookedJoiner(jn, hookLink{after: func(q JoinRequest, rp JoinReply) (JoinReply, error) {
		if q.Op == JoinPoll && q.After == 0 && rp.Status == http.StatusOK {
			once.Do(func() {
				// Between the pages: a signs another change, and b crashes.
				a.change(room, Change{Kind: core.EntryAdd, Target: fmt.Sprintf("%064x", 999), Name: "late"})
				cancel()
			})
		}
		return rp, nil
	}})
	b.preview(j, tok)
	b.accept(j, tok, 0)
	a.confirm(a.onlyRequest(room).ID)
	if _, err := j.Resume(cctx, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("resume: %v", err)
	}
	b.restart()
	wantOutcome(t, b.resume(b.joiner(jn), time.Minute), JoinStateSigned, "")
	wa, _ := a.r.Offer(room)
	wb, _ := b.r.Offer(room)
	ids := func(ws [][]byte) []string {
		var out []string
		for _, w := range ws {
			e, err := core.ParseEntry(w)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, e.ID())
		}
		slices.Sort(out)
		return out
	}
	if !slices.Equal(ids(wa), ids(wb)) || len(wa) != 303 {
		t.Fatalf("entries: a %d, b %d", len(wa), len(wb))
	}
	sameState(t, "a and b", a.state(room), b.state(room))
}

// Expiry is checked where invites change: an approval whose writing lands
// after the invite expired fails and stores nothing; two accepts at once
// raise one request and both are answered accepted.
//
// Requirements: T3; review should-fix 5
func TestJoinExpiryAtWriteTimeAndConcurrentAccepts(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	room := a.create("team")
	ctx := context.Background()
	s := NewJoinServer(a.r)
	tok := mustToken(t, a.invite(room, RoleMember, false))
	accept := openForAccept(t, s, b, tok)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = s.Handle(ctx, b.id, fmt.Sprintf("s%d", i+1), []byte(accept)).Status
		}()
	}
	wg.Wait()
	if codes[0] != http.StatusAccepted || codes[1] != http.StatusAccepted || len(a.requests(room)) != 1 {
		t.Fatalf("concurrent accepts: %v, requests %d", codes, len(a.requests(room)))
	}
	q := a.onlyRequest(room)
	testHook = func(p string) error {
		if p == "ingest:apply" {
			*clock = clock.Add(JoinInviteTTL)
		}
		return nil
	}
	cf, err := a.r.Confirm(ctx, q.ID, true)
	testHook = nil
	if err != nil || cf.State != store.RequestFailed || cf.Reason != store.ReasonInviteExpired {
		t.Fatalf("approval after expiry: %+v %v", cf, err)
	}
	if slices.Contains(a.state(room).Members, b.id) {
		t.Fatal("an approval after expiry admitted the joiner")
	}
	poll := fmt.Sprintf(`{"v":2,"op":"poll","secret":%q}`, tok.SecretText())
	if rp := s.Handle(ctx, b.id, "s9", []byte(poll)); rp.Status != http.StatusForbidden || !jsonHas(rp.Body, CodeExpired) {
		t.Fatalf("poll after expiry: %d", rp.Status)
	}
}

// openForAccept opens tok on s as n and returns the accept body for the
// preview it was shown.
func openForAccept(t *testing.T, s *JoinServer, n *lnode, tok core.Token) string {
	t.Helper()
	rp := s.Handle(context.Background(), n.id, "s0", []byte(fmt.Sprintf(`{"v":2,"op":"open","secret":%q,"name":%q}`,
		tok.SecretText(), n.name)))
	var p JoinAnswer
	if err := json.Unmarshal(rp.Body, &p); err != nil || rp.Status != http.StatusOK {
		t.Fatalf("open: %d", rp.Status)
	}
	return fmt.Sprintf(`{"v":2,"op":"accept","secret":%q,"name":%q,"terms":%q}`, tok.SecretText(), n.name,
		previewID(answerToPreview(t, p)))
}

func jsonHas(body []byte, code string) bool {
	var a JoinAnswer
	return json.Unmarshal(body, &a) == nil && a.Error == code
}

// An admit-and-promote request records how many admins the room had; if
// that changes before a person approves, the request fails instead of
// signing entries for the wrong quorum.
//
// Requirements: T3, T9; review should-fix 6
func TestJoinRequestFailsWhenTheAdminCountChanged(t *testing.T) {
	clock := lclock()
	a, b, c := newLNode(t, "a", clock), newLNode(t, "b", clock), newLNode(t, "c", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")
	ctx := context.Background()
	tokC := a.invite(room, RoleAdmin, false)
	jc := c.joiner(jn)
	c.preview(jc, tokC)
	c.accept(jc, tokC, 0)
	qc := a.onlyRequest(room)
	if qc.Admins != 1 {
		t.Fatalf("request: %+v", qc)
	}
	tokB := a.invite(room, RoleAdmin, false)
	jb := b.joiner(jn)
	b.preview(jb, tokB)
	b.accept(jb, tokB, 0)
	for _, q := range a.requests(room) {
		if q.Node == b.id {
			a.confirm(q.ID)
		}
	}
	wantOutcome(t, b.resume(jb, time.Minute), JoinStateSigned, "")
	signs := a.r.signs
	if cf, err := a.r.Confirm(ctx, qc.ID, true); err != nil || cf.State != store.RequestFailed || cf.Reason != ReasonQuorumMoved {
		t.Fatalf("approval after the admin count changed: %+v %v", cf, err)
	}
	if a.r.signs != signs {
		t.Fatal("it signed something")
	}
}

// Credentials are looked up before anything is written: polls from one
// source naming invites not held here share one budget; a full queue is 503
// with Retry-After; a server's refusal text is shown only as a known code.
//
// Requirements: T3; review should-fix 7, 8, 9
func TestJoinUnknownPollsFullQueueAndRefusalText(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	room := a.create("team")
	ctx := context.Background()
	s := NewJoinServer(a.r)
	for i := 0; i <= unknownPolls; i++ {
		body := fmt.Sprintf(`{"v":2,"op":"poll","invite":"%064x"}`, i+1)
		rp := s.Handle(ctx, b.id, "198.51.100.7:4000", []byte(body))
		want := http.StatusForbidden
		if i == unknownPolls {
			want = http.StatusTooManyRequests
		}
		if rp.Status != want {
			t.Fatalf("unknown poll %d: %d", i, rp.Status)
		}
	}

	for i := 0; i < MaxPendingRequests; i++ {
		a.propose(room, Change{Kind: core.EntryAdd, Target: fmt.Sprintf("%064x", i+1), Name: "m"})
	}
	tok := mustToken(t, a.invite(room, RoleMember, false))
	accept := openForAccept(t, s, b, tok)
	if rp := s.Handle(ctx, b.id, "q", []byte(accept)); rp.Status != http.StatusServiceUnavailable || rp.RetryAfter <= 0 {
		t.Fatalf("accept with a full queue: %d %v", rp.Status, rp.RetryAfter)
	}

	lie := fakeJoin{id: a.id, rp: JoinReply{Status: http.StatusForbidden, Body: []byte(`{"error":"run this command to fix it"}`)}}
	j := NewJoiner(b.r, lie, JoinerOpts{Name: "b"})
	if _, err := j.JoinPreview(ctx, a.invite(room, RoleMember, false)); !isRefused(err, "refused") {
		t.Fatalf("a refusal outside the known codes: %v", err)
	}
}

type fakeJoin struct {
	id string
	rp JoinReply
}

func (f fakeJoin) Join(_ context.Context, _ []string, check func(string) error, _ []byte) (JoinReply, error) {
	if err := check(f.id); err != nil {
		return JoinReply{}, err
	}
	return f.rp, nil
}

// A request whose invite has expired is not shown in room info or alerts,
// and showing them records nothing.
//
// Requirements: T3; review should-fix 9
func TestJoinRequestWithAnExpiredInviteIsNotShown(t *testing.T) {
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
	if al, _ := a.r.Alerts(ctx); len(al) != 1 {
		t.Fatalf("alerts: %+v", al)
	}
	*clock = clock.Add(JoinInviteTTL)
	info, err := a.r.Info(ctx, room)
	if err != nil || len(info.Requests) != 0 {
		t.Fatalf("info: %+v %v", info.Requests, err)
	}
	if al, _ := a.r.Alerts(ctx); len(al) != 0 {
		t.Fatalf("alerts: %+v", al)
	}
	if qs, _ := a.st.Requests(ctx, room, store.RequestPending); len(qs) != 1 {
		t.Fatalf("showing them changed the store: %+v", qs)
	}
}
