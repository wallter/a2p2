package node

import (
	"context"
	"testing"

	"github.com/wallter/a2p2/internal/store"
)

// Requirements: L1. A room-wide message with a file that a thread or
// history view showed an agent may have its file fetched; the view does
// not let the agent report the message handled.
func TestAnAgentShownAFileInAThreadViewMayFetchItButNotReportItHandled(t *testing.T) {
	ctx := context.Background()
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	msg, res := a.sendFiles(room, "alice", "for everyone", writeSource(t, "x.txt", "shown in a thread"))
	b.pullAll(room, a)
	b.tickFiles()
	if _, err := b.m.Register(ctx, "agent-b"); err != nil {
		t.Fatal(err)
	}
	es, err := b.m.Thread(ctx, room, msg)
	if err != nil || len(es) != 1 || len(es[0].Files) != 1 {
		t.Fatalf("thread = %+v %v", es, err)
	}
	if _, err := b.fetch(room, "agent-b", res.Files[0].SHA256); err == nil {
		t.Fatal("fetched before any view showed the message")
	}
	if err := b.m.Showed(ctx, room, "agent-b", es, false); err != nil {
		t.Fatal(err)
	}
	r, err := b.fetch(room, "agent-b", res.Files[0].SHA256)
	if err != nil || r.State != FetchHere {
		t.Fatalf("fetch after the thread view = %+v %v", r, err)
	}
	if _, err := b.m.Handled(ctx, room, "agent-b", msg); err == nil {
		t.Fatal("a thread view let the agent report a room-wide post handled")
	}
}

// Requirements: L1. An agent that read a message in its inbox and
// confirmed it (the receipt is replaced by the read) is told a file of it
// is gone, and why, after the author withdraws it.
func TestAFetchAfterAWithdrawOfAMessageTheAgentConfirmedSaysGone(t *testing.T) {
	ctx := context.Background()
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	msg, res := a.sendFiles(room, "alice", "for everyone", writeSource(t, "x.txt", "then withdrawn"))
	b.pullAll(room, a)
	b.tickFiles()
	es := b.inbox(room, "agent-b")
	if len(es) != 1 {
		t.Fatalf("inbox = %+v", es)
	}
	if r, err := b.m.Seen(ctx, room, "agent-b", []store.Read{{Key: es[0].Key, Version: es[0].Version}}); err != nil || !r[0].Confirmed {
		t.Fatalf("seen = %+v %v", r, err)
	}
	a.tick()
	if _, err := a.m.Withdraw(ctx, room, "alice", msg); err != nil {
		t.Fatal(err)
	}
	b.pullAll(room, a)
	g, err := b.f.Fetch(ctx, FetchOpts{Agent: "agent-b", Room: room, SHA256: res.Files[0].SHA256, Message: msg})
	if err != nil || g.State != FetchGone || g.Reason != "withdrawn" {
		t.Fatalf("after the withdraw: %+v %v", g, err)
	}
}
