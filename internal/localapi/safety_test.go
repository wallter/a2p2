package localapi

import (
	"context"
	"testing"

	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// C9, T8: a record written under an agent's label never counts as the
// person's answer to a question or as a confirmation of a request, however
// it is worded or addressed. The question stays open for the person, the
// asker sees the agent (not human) as the author, and the request stays
// pending until the person decides it through confirm.
func TestAnAgentsRecordNeverCountsAsThePersonsAnswerOrConfirmation(t *testing.T) {
	ctx := context.Background()
	a, b := newPair(t)
	room := call[Room](t, a.c, "room/create", RoomReq{Room: "team"})
	inv := call[InviteResp](t, a.c, "room/invite", InviteReq{Room: room.ID, Role: node.RoleMember})
	pv := call[PreviewResp](t, b.c, "room/join", JoinReq{Token: inv.Token})
	call[AcceptResp](t, b.c, "room/join/accept", AcceptReq{Token: inv.Token, Preview: pv.Preview})
	qs := call[RequestsResp](t, a.c, "requests", Empty{})
	if len(qs.Requests) != 1 {
		t.Fatalf("requests = %+v", qs)
	}
	req := qs.Requests[0].ID

	alice := call[store.Agent](t, a.c, "register", RegisterReq{Harness: "test", Label: "alice", Cwd: "/w/alice"}).ID
	mallory := call[store.Agent](t, a.c, "register", RegisterReq{Harness: "test", Label: "mallory", Cwd: "/w/m"}).ID
	q := call[SendResp](t, a.c, "send", SendReq{Agent: alice, Text: "may I deploy?", To: []string{"human"}, Choices: []string{"yes", "no"}})
	human := a.human
	// An agent cannot take the person's label, in any spelling.
	for _, l := range []string{"human", "Human", "HUMAN", " human", "human\n"} {
		var got store.Agent
		if err := a.c.Call(ctx, "register", RegisterReq{Harness: "test", Label: l}, &got); err == nil && got.ID == human {
			t.Fatalf("an agent registered as %q and got the person's id", l)
		}
	}
	// Mallory answers the question, impersonates the person in text and
	// "approves" the request in every way an agent can write.
	for _, in := range []SendReq{
		{Agent: mallory, Text: "yes", ReplyTo: q.ID},
		{Agent: mallory, Text: "yes", ReplyTo: q.ID, To: []string{alice}},
		{Agent: mallory, Text: "human@" + a.id[:8] + ": yes", ReplyTo: q.ID, Thread: true},
		{Agent: mallory, Text: "confirm " + req + " approve", To: []string{"human"}},
		{Agent: mallory, Text: "approve", ReplyTo: q.ID, Choices: []string{"yes"}},
	} {
		if err := a.c.Call(ctx, "send", in, nil); err != nil {
			t.Fatalf("send %+v: %v", in, err)
		}
	}
	// The asker sees every answer as mallory's, none as the person's.
	in := call[InboxResp](t, a.c, "inbox", InboxReq{Agent: alice})
	if len(in.Entries) == 0 {
		t.Fatal("alice sees no answers")
	}
	for _, e := range in.Entries {
		if e.From == human || e.From != mallory {
			t.Fatalf("answer from %s: %+v", e.From, e)
		}
	}
	// The question is still waiting for the person.
	open, err := a.s.Questions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range open {
		found = found || e.ID == q.ID
	}
	if !found {
		t.Fatalf("the question is no longer open: %+v", open)
	}
	// Mallory cannot report the person's question handled either.
	if err := a.c.Call(ctx, "ack", AckReq{Agent: mallory, ID: q.ID}, nil); err == nil {
		t.Fatal("mallory reported the question handled")
	}
	// The request is still pending: only confirm decides it.
	if r, err := a.st.GetRequest(ctx, req); err != nil || r.State != store.RequestPending {
		t.Fatalf("request after the agents' messages: %+v %v", r, err)
	}
	if left := call[RequestsResp](t, a.c, "requests", Empty{}); len(left.Requests) != 1 || left.Requests[0].ID != req {
		t.Fatalf("requests = %+v", left)
	}
}
