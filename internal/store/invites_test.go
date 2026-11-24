package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var memberTerms = Terms{Role: "member"}

// An invite moves issued, previewed (bound to the first key, with the
// terms shown), accepted (with its request, in one transaction, only on
// the terms shown), and is signed by the approval of that request; past
// its expiry an undecided invite expires and its pending request fails,
// while a signed or declined one is never touched. Leaving the room
// deletes its invites.
//
// Requirements: T3; PROTOCOL 5.3, 8
func TestInviteStatesAndExpiry(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	root := testCreate(t)
	if _, err := s.CreateLogRoom(ctx, logRoom, "team", root, nil, t0); err != nil {
		t.Fatal(err)
	}
	h1, h2, h3, h4 := strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64), strings.Repeat("4", 64)
	kb, kc := nodeOf(testKey("b")), nodeOf(testKey("c"))
	exp := t0.Add(30 * time.Minute)
	for _, h := range []string{h1, h2, h3, h4} {
		if err := s.CreateInvite(ctx, Invite{SecretHash: h, Room: logRoom, Role: "member", Created: t0, Expires: exp}); err != nil {
			t.Fatal(err)
		}
	}
	inv, err := s.OpenInvite(ctx, h1, kb, "bee", []string{"b:1"}, memberTerms, t0)
	if err != nil || inv.State != InvitePreviewed || inv.BoundKey != kb || inv.Shown != memberTerms {
		t.Fatalf("open: %+v %v", inv, err)
	}
	if _, err := s.OpenInvite(ctx, h1, kc, "sea", nil, memberTerms, t0); !errors.Is(err, ErrInviteBound) {
		t.Fatalf("second key: %v", err)
	}
	if err := s.AcceptInvite(ctx, h1, kb, "bee", nil, Terms{Role: "admin"}, Request{ID: "q_0", Room: logRoom, Created: t0}, func() time.Time { return t0 }); !errors.Is(err, ErrInviteTerms) {
		t.Fatalf("accept on other terms: %v", err)
	}
	q := Request{ID: "q_1", Kind: "add", Action: "admit", Room: logRoom, Node: kb, Name: "bee", Created: t0}
	if err := s.AcceptInvite(ctx, h1, kb, "bee", nil, memberTerms, q, func() time.Time { return t0 }); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptInvite(ctx, h1, kb, "bee", nil, memberTerms, Request{ID: "q_2", Room: logRoom, Created: t0}, func() time.Time { return t0 }); !errors.Is(err, ErrInviteState) {
		t.Fatalf("second accept: %v", err)
	}
	if qs, _ := s.Requests(ctx, logRoom, RequestPending); len(qs) != 1 {
		t.Fatalf("requests: %+v", qs)
	}
	if _, err := s.OpenInvite(ctx, h2, kc, "sea", nil, memberTerms, exp); !errors.Is(err, ErrInviteExpired) {
		t.Fatalf("open after expiry: %v", err)
	}

	// h3 declined, h4 signed, before the expiry.
	for i, h := range []string{h3, h4} {
		key := nodeOf(testKey(h[:1]))
		if _, err := s.OpenInvite(ctx, h, key, "m", nil, memberTerms, t0); err != nil {
			t.Fatal(err)
		}
		q := Request{ID: "q_" + h[:1], Kind: "add", Action: "admit", Room: logRoom, Node: key, Name: "m", Created: t0}
		if err := s.AcceptInvite(ctx, h, key, "m", nil, memberTerms, q, func() time.Time { return t0 }); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := s.DecideRequest(ctx, q.ID, RequestDeclined, "", t0); err != nil {
				t.Fatal(err)
			}
		} else if _, err := s.ApplyLog(ctx, LogBatch{Room: logRoom, Complete: q.ID, Now: t0}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.ExpireInvites(ctx, exp.Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("expire: %d %v", n, err) // h1 only: h2 expired already
	}
	got, _ := s.GetInvite(ctx, h1)
	if got.State != InviteExpired || got.RequestState != RequestFailed || got.RequestReason != ReasonInviteExpired {
		t.Fatalf("expired: %+v", got)
	}
	if got, _ := s.GetInvite(ctx, h3); got.State != InviteAccepted || got.RequestState != RequestDeclined {
		t.Fatalf("expiry overwrote a decline: %+v", got)
	}
	if got, _ := s.GetInvite(ctx, h4); got.State != InviteAccepted || got.RequestState != RequestApproved {
		t.Fatalf("expiry touched a signed invite: %+v", got)
	}
	if _, err := s.LeaveRoom(ctx, logRoom, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetInvite(ctx, h4); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invite after leaving: %v", err)
	}
}

// An approval written after its invite's expiry fails in the transaction
// that would store it; nothing is approved.
//
// Requirements: T3; review should-fix (expiry checked at mutation time)
func TestApprovalWrittenAfterTheInviteExpiredFails(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if _, err := s.CreateLogRoom(ctx, logRoom, "team", testCreate(t), nil, t0); err != nil {
		t.Fatal(err)
	}
	h, kb := strings.Repeat("5", 64), nodeOf(testKey("b"))
	exp := t0.Add(30 * time.Minute)
	if err := s.CreateInvite(ctx, Invite{SecretHash: h, Room: logRoom, Role: "member", Created: t0, Expires: exp}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenInvite(ctx, h, kb, "bee", nil, memberTerms, t0); err != nil {
		t.Fatal(err)
	}
	q := Request{ID: "q_1", Kind: "add", Action: "admit", Room: logRoom, Node: kb, Name: "bee", Created: t0}
	if err := s.AcceptInvite(ctx, h, kb, "bee", nil, memberTerms, q, func() time.Time { return t0 }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyLog(ctx, LogBatch{Room: logRoom, Complete: q.ID, Now: exp}); !errors.Is(err, ErrInviteExpired) {
		t.Fatalf("approval after expiry: %v", err)
	}
	if got, _ := s.GetRequest(ctx, q.ID); got.State != RequestPending {
		t.Fatalf("request: %+v", got)
	}
}

// The joiner's record of an approved join is written once and never
// overwritten; leaving the room marks a join in progress left, in the
// leave's transaction, and the record stays so the invite is used once.
//
// Requirements: T3; review blockers 1 and 2
func TestJoinRecordIsKeptAndLeavingEndsAJoinInProgress(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if _, err := s.CreateLogRoom(ctx, logRoom, "team", testCreate(t), nil, t0); err != nil {
		t.Fatal(err)
	}
	j := Join{Invite: strings.Repeat("6", 64), Room: logRoom, Role: "member", Admin: nodeOf(testKey("a")), Gen: 1, Created: t0}
	if _, added, err := s.AddJoin(ctx, j); err != nil || !added {
		t.Fatalf("add: %v %v", added, err)
	}
	other := j
	other.Role = "admin"
	if got, added, err := s.AddJoin(ctx, other); err != nil || added || got.Role != "member" {
		t.Fatalf("second add: %+v %v %v", got, added, err)
	}
	if _, err := s.LeaveRoom(ctx, logRoom, t0); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetJoin(ctx, j.Invite); err != nil || got.State != JoinLeft {
		t.Fatalf("after leave: %+v %v", got, err)
	}
	if err := s.EndJoin(ctx, j.Invite, JoinSigned, "", t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ending a left join: %v", err)
	}
}

// Every stored log row gets an arrival position allocated once: a row
// deleted later frees nothing, and a later row's position is larger.
//
// Requirements: T3; second review blocker 4
func TestLogArrivalPositionsAreNeverReused(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	root := testCreate(t)
	gen, err := s.CreateLogRoom(ctx, logRoom, "team", root, nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	row := func(i int) LogRow {
		r := root
		r.ID, r.State = strings.Repeat("0", 31)+string(rune('1'+i)), LogHeld
		return r
	}
	if _, err := s.ApplyLog(ctx, LogBatch{Room: logRoom, Gen: gen, New: []LogRow{row(0), row(1)}, Now: t0}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyLog(ctx, LogBatch{Room: logRoom, Gen: gen, Gone: []string{row(1).ID}, Now: t0}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyLog(ctx, LogBatch{Room: logRoom, Gen: gen, New: []LogRow{row(2)}, Now: t0}); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.Log(ctx, logRoom)
	var got []int64
	for _, r := range rows {
		got = append(got, r.Arrival)
	}
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 4 {
		t.Fatalf("arrivals: %v", got)
	}
}

// A join's pin checks, in its own write, that the join is still accepted
// and the room's generation is the one recorded at the preview; a leave or
// another pin in between refuses it.
//
// Requirements: T3; second review blocker 1
func TestJoinLogRoomChecksTheJoinAndTheGenerationInItsWrite(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	root := testCreate(t)
	j := Join{Invite: strings.Repeat("7", 64), Room: logRoom, Role: "member", Created: t0}
	if _, _, err := s.AddJoin(ctx, j); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateLogRoom(ctx, logRoom, "team", root, nil, t0); err != nil { // pinned by another path
		t.Fatal(err)
	}
	if _, err := s.JoinLogRoom(ctx, j.Invite, 0, "team", root, nil, t0); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("pin while held: %v", err)
	}
	if _, err := s.LeaveRoom(ctx, logRoom, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.JoinLogRoom(ctx, j.Invite, 0, "team", root, nil, t0); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("pin after a leave: %v", err)
	}
	j2 := Join{Invite: strings.Repeat("8", 64), Room: logRoom, Role: "member", Gen: 2, Created: t0} // left: a new generation
	if _, _, err := s.AddJoin(ctx, j2); err != nil {
		t.Fatal(err)
	}
	gen, err := s.JoinLogRoom(ctx, j2.Invite, 2, "team", root, nil, t0)
	if got, _ := s.GetJoin(ctx, j2.Invite); err != nil || gen != 3 || got.PinnedGen != 3 {
		t.Fatalf("pin: %d %v %+v", gen, err, got)
	}
	if _, err := s.JoinLogRoom(ctx, j2.Invite, 2, "team", root, nil, t0); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("second pin: %v", err)
	}
}

// An approval's clock is read inside its transaction.
//
// Requirements: T3; second review should-fix 6
func TestApprovalClockIsReadInsideTheWrite(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if _, err := s.CreateLogRoom(ctx, logRoom, "team", testCreate(t), nil, t0); err != nil {
		t.Fatal(err)
	}
	h, kb := strings.Repeat("9", 64), nodeOf(testKey("b"))
	exp := t0.Add(30 * time.Minute)
	if err := s.CreateInvite(ctx, Invite{SecretHash: h, Room: logRoom, Role: "member", Created: t0, Expires: exp}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenInvite(ctx, h, kb, "bee", nil, memberTerms, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptInvite(ctx, h, kb, "bee", nil, memberTerms, Request{ID: "q_9", Room: logRoom, Created: t0},
		func() time.Time { return t0 }); err != nil {
		t.Fatal(err)
	}
	b := LogBatch{Room: logRoom, Complete: "q_9", Now: t0, Clock: func() time.Time { return exp }}
	if _, err := s.ApplyLog(ctx, b); !errors.Is(err, ErrInviteExpired) {
		t.Fatalf("approval written after the expiry: %v", err)
	}
	if err := s.AcceptInvite(ctx, h, kb, "bee", nil, memberTerms, Request{ID: "q_8", Room: logRoom, Created: t0},
		func() time.Time { return exp }); !errors.Is(err, ErrInviteExpired) && !errors.Is(err, ErrInviteState) {
		t.Fatalf("accept judged by its clock: %v", err)
	}
}
