package node

import (
	"context"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// Wakes for changes other than a record written here: each test asserts on
// the wake channel after the change has returned (wake_inbox_test.go).

// [wake] A receipt that lets a message through the level wakes the other
// session of the same agent. Session A reads at level all and has not yet
// written its receipt; the level goes to none, and session B looks, sees
// nothing and waits; A's receipt then commits, the message is listed, so
// the level no longer keeps it out (keptOut), and B is woken at once.
func TestAReceiptThatRestoresVisibilityWakesTheAgentsOtherSession(t *testing.T) {
	e := newSurf(t)
	ctx := context.Background()
	alice, bob := e.agent("alice"), e.agent("bob")
	label, _ := e.s.label(bob)
	m := e.send(SendInput{Agent: alice, Text: "room-wide"})
	a, err := e.s.m.Inbox(ctx, m.Room, label) // session A's look: no receipt yet
	if err != nil || len(a.Entries) != 1 {
		t.Fatalf("session A: %+v %v", a, err)
	}
	e.level(bob, "", store.LevelNone)
	b := e.arm(bob, "") // session B waits: the level keeps the post out
	if err := e.s.m.Showed(ctx, m.Room, label, a.Entries, true); err != nil {
		t.Fatal(err)
	}
	if !b.woken() {
		t.Fatal("session A's receipt did not wake session B")
	}
	onlyEntry(t, b.result(t), m.ID)
	// A receipt of another agent wakes no one of bob's.
	carol := e.agent("carol")
	clabel, _ := e.s.label(carol)
	n := e.send(SendInput{Agent: alice, Text: "@everyone second"})
	c, err := e.s.m.Inbox(ctx, n.Room, clabel)
	if err != nil {
		t.Fatal(err)
	}
	e.read(bob)
	b = e.arm(bob, "")
	if err := e.s.m.Showed(ctx, n.Room, clabel, c.Entries, true); err != nil {
		t.Fatal(err)
	}
	if b.woken() {
		t.Fatal("carol's receipt woke bob")
	}
}

// fileWaiter registers a waiting inbox of agent in room on n's machine and
// returns it once it knows its level, as Surface.Inbox does.
func fileWaiter(n *fnode, agent, room string) *inboxWaiter {
	x := n.r.m.wakes.waitInbox(agent, room)
	n.r.m.wakes.setLevels(x, map[string]string{room: store.LevelAll})
	return x
}

// [wake] A send with files that the person declines, or whose approval
// comes too late and declines it, wakes its writer's waiting inbox, which
// lists the decision (requestEntries).
func TestADeclinedOrExpiredSendWithFilesWakesTheWaitingInbox(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a := ns[0]
	room := fileRoom(t, a, ns[1])
	ctx := context.Background()
	stage := func(text string) FileSendResult {
		res, err := a.f.RequestSend(ctx, FileSend{Draft: Draft{Room: room, Agent: "alice", Text: text},
			Files: []string{writeSource(t, "x.txt", text)}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := stage("declined")
	x := fileWaiter(a, "alice", room)
	if c, err := a.r.ConfirmWith(ctx, res.Request, false, ConfirmOpts{}); err != nil || c.State != store.RequestDeclined {
		t.Fatalf("decline: %+v %v", c, err)
	}
	if !isClosed(x) {
		t.Fatal("a declined send with files did not wake its writer's waiting inbox")
	}
	a.r.m.wakes.doneInbox(x)
	res = stage("too late")
	*a.clock = a.clock.Add(core.StagingLife + time.Minute) // no sweep has run
	x = fileWaiter(a, "alice", room)
	if c, err := a.f.ApproveSend(ctx, res.Request, res.Terms); err != nil || c.State != store.RequestDeclined {
		t.Fatalf("late approval: %+v %v", c, err)
	}
	if !isClosed(x) {
		t.Fatal("an approval that declined an expired send did not wake its writer's waiting inbox")
	}
	a.r.m.wakes.doneInbox(x)
}

// [wake] A sweep that declines an expired staged send (the pass after a
// pull, a fetch, or the periodic one: all go through Files.sweep) wakes
// the room's waiting inboxes; a sweep that changes nothing wakes none.
func TestASweepThatDecidesAStagedSendWakesTheWaitingInbox(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a := ns[0]
	room := fileRoom(t, a, ns[1])
	ctx := context.Background()
	res, err := a.f.RequestSend(ctx, FileSend{Draft: Draft{Room: room, Agent: "alice", Text: "never confirmed"},
		Files: []string{writeSource(t, "x.txt", "staged")}})
	if err != nil {
		t.Fatal(err)
	}
	x := fileWaiter(a, "alice", room)
	if err := a.f.SweepAll(ctx); err != nil {
		t.Fatal(err)
	}
	if isClosed(x) {
		t.Fatal("a sweep that changed nothing woke the waiting inbox")
	}
	*a.clock = a.clock.Add(core.StagingLife + time.Minute)
	a.f.sweepRoom(ctx, room) // as after a pull (Exchange.Pull)
	if !isClosed(x) {
		t.Fatal("a sweep that declined an expired send did not wake the waiting inbox")
	}
	a.r.m.wakes.doneInbox(x)
	if q, err := a.st.GetRequest(ctx, res.Request); err != nil || q.State != store.RequestDeclined {
		t.Fatalf("request after the sweep: %+v %v", q, err)
	}
}

// [wake] A room set aside while the daemon runs (a replay that no longer
// agrees with what is stored, Rooms.restore) wakes its waiting inboxes,
// which then no longer read it.
func TestARoomSetAsideAtRunTimeWakesItsWaitingInboxes(t *testing.T) {
	e := newSurf(t)
	ctx := context.Background()
	bob := e.agent("bob")
	team, err := e.s.CreateRoom(ctx, "team")
	if err != nil {
		t.Fatal(err)
	}
	b := e.arm(bob, team.ID)
	r := e.s.r
	r.mu.Lock()
	rl := r.rooms[team.ID]
	for id, row := range rl.state { // what is stored no longer matches a replay
		row.State = "not a state"
		rl.state[id] = row
		break
	}
	r.restore(rl)
	r.mu.Unlock()
	if !b.woken() {
		t.Fatal("a room set aside at run time did not wake its waiting inbox")
	}
	if !r.isQuarantined(team.ID) {
		t.Fatal("the room was not set aside")
	}
}
