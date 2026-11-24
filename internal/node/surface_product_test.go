package node

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/core"
)

// The product round (what five agents tripped over): ack says why it
// refused, a write says whom it addressed and in which room by label,
// human is an addressee, and status lists the agents seen writing.

func TestSurfaceAckRefusalsSayWhyAndTheRuleIsUnchanged(t *testing.T) {
	e := newSurf(t)
	ctx := context.Background()
	alice, bob := e.agent("alice"), e.agent("bob")
	direct := e.send(SendInput{Agent: alice, Text: "for bob", To: []string{bob}})
	plain := e.send(SendInput{Agent: alice, Text: "for all"})
	asked := e.send(SendInput{Agent: alice, Text: "asked", AskHandled: true})
	for _, c := range []struct{ agent, id, want string }{
		{bob, strings.Repeat("0", 32), "no message"},
		{bob, plain.ID, "not addressed to you and did not ask"},
		{alice, asked.ID, "you wrote this message"},
		{bob, direct.ID, "read it first"},
		{bob, asked.ID, "read it first"},
	} {
		_, err := e.s.Ack(ctx, c.agent, c.id)
		if !isUser(err) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s acks %s: %v, want %q", c.agent, c.id, err, c.want)
		}
	}
	// A thread read showed bob the text: it counts (D-reading).
	if _, err := e.s.Thread(ctx, bob, direct.ID); err != nil {
		t.Fatal(err)
	}
	h, err := e.s.Ack(ctx, bob, direct.ID)
	if err != nil {
		t.Fatalf("after a thread read: %v", err)
	}
	e.read(bob)
	if _, err := e.s.Ack(ctx, bob, asked.ID); err != nil {
		t.Fatalf("a post that asked, once shown: %v", err)
	}
	// A second report by the same agent succeeds and names the first.
	if again, err := e.s.Ack(ctx, bob, direct.ID); err != nil || !again.Already || again.ID != h.ID {
		t.Fatalf("second ack: %+v %v", again, err)
	}
	// The machine-level refusals still count as ErrNotAllowed.
	if _, err := e.s.m.Handled(ctx, e.s.LocalRoom(), "alice", asked.ID); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("machine refusal: %v", err)
	}
}

func TestSurfaceWriteSaysWhomItAddressedAndTheRoomLabel(t *testing.T) {
	e := newSurf(t)
	alice, bob, carol := e.agent("alice"), e.agent("bob"), e.agent("carol")
	q := e.send(SendInput{Agent: alice, Text: "kickoff"})
	if q.Label != LocalLabel || len(q.To) != 0 {
		t.Fatalf("post = %+v", q)
	}
	r := e.send(SendInput{Agent: bob, Text: "finding", ReplyTo: q.ID})
	if len(r.To) != 1 || AgentID(r.To[0]) != alice || r.Label != LocalLabel {
		t.Fatalf("default reply = %+v", r)
	}
	r = e.send(SendInput{Agent: bob, Text: "for both", ReplyTo: q.ID, To: []string{alice, carol}})
	if len(r.To) != 2 {
		t.Fatalf("reply with to = %+v", r)
	}
	es := e.inbox(carol)
	if len(es) != 2 || es[0].RoomLabel != LocalLabel {
		t.Fatalf("carol = %+v", es)
	}
	if _, err := e.s.Send(context.Background(), SendInput{Agent: bob, Text: "x", ReplyTo: "nope"}); err == nil ||
		!strings.Contains(err.Error(), "no message nope") {
		t.Fatalf("reply to unknown: %v", err)
	}
}

func TestSurfaceHumanIsThePersonAtThisMachine(t *testing.T) {
	e := newSurf(t)
	alice := e.agent("alice")
	s := e.send(SendInput{Agent: alice, Text: "look", To: []string{HumanLabel}})
	if len(s.To) != 1 || AgentID(s.To[0]) != e.s.HumanID() {
		t.Fatalf("to = %+v", s.To)
	}
	if es := e.inbox(e.s.HumanID()); len(es) != 1 || es[0].ID != s.ID {
		t.Fatalf("human's inbox = %+v", es)
	}
}

func TestSurfaceStatusListsTheAgentsSeenWritingInEachRoom(t *testing.T) {
	e := newSurf(t)
	ctx := context.Background()
	alice, bob := e.agent("alice"), e.agent("bob")
	e.agent("quiet")
	e.send(SendInput{Agent: alice, Text: "one"})
	e.send(SendInput{Agent: bob, Text: "two"})
	st, err := e.s.Status(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	ws := st.Writers[e.s.LocalRoom()]
	if len(ws) != 2 || ws[0].Agent != bob && ws[1].Agent != bob || ws[0].Last.IsZero() {
		t.Fatalf("writers = %+v", ws)
	}
	for _, w := range ws {
		if w.Agent != alice && w.Agent != bob {
			t.Fatalf("writer %+v", w)
		}
	}
}

// The reply tool's default addressees stop at the addressee limit; the
// parent's author is always among them and the result counts the rest.
func TestSurfaceThreadReplyAddresseesAreCappedWithTheParentsAuthorKept(t *testing.T) {
	e := newSurf(t)
	lead := e.agent("lead")
	kick := e.send(SendInput{Agent: lead, Text: "kickoff"})
	var first Sent
	for i := 0; i < core.MaxTo+3; i++ {
		w := e.agent(fmt.Sprintf("w%02d", i))
		s := e.send(SendInput{Agent: w, Text: "finding", ReplyTo: kick.ID})
		if i == 0 {
			first = s
		}
	}
	me := e.agent("me")
	r := e.send(SendInput{Agent: me, Text: "to all of you", ReplyTo: first.ID, Thread: true})
	if len(r.To) != core.MaxTo || r.ToOmitted != 4 { // 35 writers and the lead, less 32
		t.Fatalf("%d addressed, %d left out", len(r.To), r.ToOmitted)
	}
	if !slices.ContainsFunc(r.To, func(a core.Addressee) bool { return AgentID(a) == e.s.agentID("w00", e.s.Node()) }) {
		t.Fatalf("the parent's author is not addressed: %+v", r.To)
	}
	if slices.ContainsFunc(r.To, func(a core.Addressee) bool { return AgentID(a) == me }) {
		t.Fatal("the sender addressed itself")
	}
}
