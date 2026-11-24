package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// An invite moves issued, previewed (bound to the first key), accepted
// (with its request, in one transaction), and is signed by the approval
// of that request; past its expiry an unsigned invite expires and its
// pending request fails, while a signed one stays signed. Leaving the room
// deletes its invites.
//
// Requirements: T3; PROTOCOL-next 5.3, 8
func TestInviteStatesAndExpiry(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	root := testCreate(t)
	if _, err := s.CreateLogRoom(ctx, logRoom, "team", root, nil, t0); err != nil {
		t.Fatal(err)
	}
	h1, h2 := strings.Repeat("1", 64), strings.Repeat("2", 64)
	kb, kc := nodeOf(testKey("b")), nodeOf(testKey("c"))
	exp := t0.Add(30 * time.Minute)
	for _, h := range []string{h1, h2} {
		if err := s.CreateInvite(ctx, Invite{SecretHash: h, Room: logRoom, Role: "member", Created: t0, Expires: exp}); err != nil {
			t.Fatal(err)
		}
	}
	inv, err := s.BindInvite(ctx, h1, kb, "bee", []string{"b:1"}, t0)
	if err != nil || inv.State != InvitePreviewed || inv.BoundKey != kb {
		t.Fatalf("bind: %+v %v", inv, err)
	}
	if _, err := s.BindInvite(ctx, h1, kc, "sea", nil, t0); !errors.Is(err, ErrInviteBound) {
		t.Fatalf("second key: %v", err)
	}
	q := Request{ID: "q_1", Kind: "add", Action: "admit", Room: logRoom, Node: kb, Name: "bee", Created: t0}
	if err := s.AcceptInvite(ctx, h1, kb, "bee", nil, q, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptInvite(ctx, h1, kb, "bee", nil, Request{ID: "q_2", Room: logRoom, Created: t0}, t0); !errors.Is(err, ErrInviteState) {
		t.Fatalf("second accept: %v", err)
	}
	if qs, _ := s.Requests(ctx, logRoom, RequestPending); len(qs) != 1 {
		t.Fatalf("requests: %+v", qs)
	}
	if _, err := s.BindInvite(ctx, h2, kc, "sea", nil, exp); !errors.Is(err, ErrInviteExpired) {
		t.Fatalf("bind after expiry: %v", err)
	}
	if n, err := s.ExpireInvites(ctx, exp); err != nil || n != 2 {
		t.Fatalf("expire: %d %v", n, err)
	}
	got, _ := s.GetInvite(ctx, h1)
	if got.State != InviteExpired || got.RequestState != RequestFailed || got.RequestReason != ReasonInviteExpired {
		t.Fatalf("expired: %+v", got)
	}

	// A signed invite does not expire.
	h3 := strings.Repeat("3", 64)
	if err := s.CreateInvite(ctx, Invite{SecretHash: h3, Room: logRoom, Role: "member", Created: t0, Expires: exp}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindInvite(ctx, h3, kc, "sea", nil, t0); err != nil {
		t.Fatal(err)
	}
	q3 := Request{ID: "q_3", Kind: "add", Action: "admit", Room: logRoom, Node: kc, Name: "sea", Created: t0}
	if err := s.AcceptInvite(ctx, h3, kc, "sea", nil, q3, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyLog(ctx, LogBatch{Room: logRoom, Complete: q3.ID, Now: t0}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.ExpireInvites(ctx, exp.Add(time.Hour)); n != 0 {
		t.Fatalf("a signed invite expired: %d", n)
	}
	if got, _ := s.InviteByRequest(ctx, q3.ID); got.RequestState != RequestApproved || got.State != InviteAccepted {
		t.Fatalf("signed: %+v", got)
	}
	if _, err := s.LeaveRoom(ctx, logRoom, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetInvite(ctx, h3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invite after leaving: %v", err)
	}
}
