package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// machine is a real daemon in a temporary home with networking on,
// listening on loopback.
type machine struct {
	t    *testing.T
	home string
	c    *localapi.Client
	done chan error
	node string
}

func newMachine(t *testing.T, name string) *machine { return newMachineWith(t, name, nil) }

// newMachineWith runs prepare on the home before the first start.
func newMachineWith(t *testing.T, name string, prepare func(home string)) *machine {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "a2p2")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	// A fixed port, as a real machine has, so that a restart is found again
	// at the address its peers know.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := ln.Addr().String()
	ln.Close()
	if err := SaveConfig(home, Config{Name: name, Listen: listen, UI: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	if prepare != nil {
		prepare(home)
	}
	m := &machine{t: t, home: home}
	m.start()
	t.Cleanup(m.stop)
	return m
}

func (m *machine) start() {
	m.t.Helper()
	m.done = make(chan error, 1)
	done := m.done
	go func() { done <- Run(context.Background(), m.home, "test") }()
	m.c = localapi.NewClient(SockPath(m.home))
	for deadline := time.Now().Add(10 * time.Second); ; {
		var st localapi.Status
		if m.c.Call(context.Background(), "status", nil, &st) == nil {
			m.node = st.Node
			return
		}
		if time.Now().After(deadline) {
			m.t.Fatal("daemon did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stop stops the daemon with its own stop call and waits for it to end.
func (m *machine) stop() {
	if m.done == nil {
		return
	}
	_ = m.c.Call(context.Background(), "stop", nil, nil)
	select {
	case err := <-m.done:
		if err != nil {
			m.t.Errorf("daemon: %v", err)
		}
	case <-time.After(15 * time.Second):
		m.t.Error("daemon did not stop")
	}
	m.done = nil
}

func call[T any](m *machine, name string, req any) T {
	m.t.Helper()
	var out T
	if err := m.c.Call(context.Background(), name, req, &out); err != nil {
		m.t.Fatalf("%s: %v", name, err)
	}
	return out
}

func (m *machine) agent(label string) string {
	return call[store.Agent](m, "register", localapi.RegisterReq{Harness: "test", Label: label, Cwd: "/w/" + label}).ID
}

// eventually retries cond until it holds or 60 s pass.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(60 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// join takes machine j into room through admin a: invite, preview, the
// joiner's approval (room/join/accept), the admin's approval (confirm).
// restart, if set, restarts the joiner between its accept and the admin's
// approval, so the join resumes after a restart.
func join(t *testing.T, a, j *machine, room string, inv localapi.InviteReq, restart bool) {
	t.Helper()
	inv.Room = room
	tok := call[localapi.InviteResp](a, "room/invite", inv)
	pv := call[localapi.PreviewResp](j, "room/join", localapi.JoinReq{Token: tok.Token})
	if pv.State != node.JoinStatePreviewed || pv.Room.ID != room || pv.Admin != a.node {
		t.Fatalf("preview = %+v", pv)
	}
	acc := call[localapi.AcceptResp](j, "room/join/accept", localapi.AcceptReq{Token: tok.Token, Preview: pv.Preview})
	if acc.State != node.JoinStateAccepted {
		t.Fatalf("accept = %+v", acc)
	}
	if restart {
		j.stop()
	}
	qs := call[localapi.RequestsResp](a, "requests", localapi.Empty{})
	i := slices.IndexFunc(qs.Requests, func(q localapi.Request) bool { return q.Node == j.node })
	if i < 0 {
		t.Fatalf("requests = %+v", qs)
	}
	cf := call[localapi.ConfirmResp](a, "confirm", localapi.ConfirmReq{ID: qs.Requests[i].ID, Approve: true})
	if cf.State != store.RequestApproved {
		t.Fatalf("confirm = %+v", cf)
	}
	if restart {
		j.start()
	}
	eventually(t, "the joiner to hold the room", func() bool {
		var rl localapi.RoomsResp
		err := j.c.Call(context.Background(), "room/list", localapi.RoomListReq{Room: room}, &rl)
		return err == nil && len(rl.Rooms) == 1 && slices.ContainsFunc(rl.Rooms[0].Members, func(m localapi.Member) bool { return m.Node == j.node })
	})
}

func inboxWait(m *machine, agent string) []localapi.Entry {
	m.t.Helper()
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); {
		in := call[localapi.InboxResp](m, "inbox", localapi.InboxReq{Agent: agent, WaitMS: 5000})
		if len(in.Entries) > 0 {
			seen := localapi.SeenReq{Agent: agent}
			for _, e := range in.Entries {
				seen.Entries = append(seen.Entries, localapi.SeenEntry{Key: e.Key, Version: e.Version})
			}
			res := call[localapi.SeenResp](m, "seen", seen)
			for _, r := range res.Results {
				if !r.OK {
					m.t.Fatalf("seen %s: %s", r.Key, r.Reason)
				}
			}
			return in.Entries
		}
	}
	m.t.Fatalf("no mail for %s", agent)
	return nil
}

func TestTwoDaemonsOnLoopbackPairSendReplyAckAndTrace(t *testing.T) {
	if testing.Short() {
		t.Skip("starts daemons")
	}
	a, b := newMachine(t, "alpha"), newMachine(t, "beta")
	room := call[localapi.Room](a, "room/create", localapi.RoomReq{Room: "team"}).ID
	ann, bea := a.agent("ann"), b.agent("bea")
	// The joiner restarts between its approval and the admin's: the join resumes.
	join(t, a, b, room, localapi.InviteReq{}, true)
	bea = b.agent("bea")

	m := call[localapi.SendResp](a, "send", localapi.SendReq{Agent: ann, Room: "team", Text: "build is green", To: []string{bea}})
	es := inboxWait(b, bea)
	if len(es) != 1 || es[0].ID != m.ID || es[0].Text != "build is green" || es[0].From != ann || es[0].FromNode != a.node {
		t.Fatalf("bea's inbox = %+v", es)
	}
	r := call[localapi.SendResp](b, "send", localapi.SendReq{Agent: bea, Text: "thanks", ReplyTo: m.ID})
	if r.Room != room {
		t.Fatalf("reply went to %s", r.Room)
	}
	call[localapi.AckResp](b, "ack", localapi.AckReq{Agent: bea, ID: m.ID})
	es = inboxWait(a, ann)
	if len(es) != 1 || es[0].ID != r.ID || es[0].Parent != m.ID || es[0].Thread != m.ID || es[0].To[0] != ann {
		t.Fatalf("ann's inbox = %+v", es)
	}
	th := call[localapi.Entries](a, "thread", localapi.ThreadReq{Agent: ann, ID: m.ID})
	if len(th.Entries) != 2 {
		t.Fatalf("thread = %+v", th)
	}
	eventually(t, "a copy report and the handled report on the author's machine", func() bool {
		tr := call[localapi.TraceResp](a, "trace", localapi.IDReq{ID: m.ID})
		return len(tr.Copies) == 1 && tr.Copies[0].Node == b.node && tr.Copies[0].Member && len(tr.Handled) == 1 && tr.Handled[0].Agent == bea
	})

	// The author's daemon restarts in the middle: pulling resumes both ways.
	a.stop()
	m2 := call[localapi.SendResp](b, "send", localapi.SendReq{Agent: bea, Room: room, Text: "while you were away"})
	a.start()
	es = inboxWait(a, ann)
	if len(es) != 1 || es[0].ID != m2.ID {
		t.Fatalf("after restart = %+v", es)
	}
	m3 := call[localapi.SendResp](a, "send", localapi.SendReq{Agent: ann, Room: room, Text: "back"})
	if es = inboxWait(b, bea); len(es) != 1 || es[0].ID != m3.ID {
		t.Fatalf("after restart, other way = %+v", es)
	}
}

func TestMachineAddedWithoutHistorySeesNoEarlierTextThroughAnySurface(t *testing.T) {
	if testing.Short() {
		t.Skip("starts daemons")
	}
	a, b, c := newMachine(t, "alpha"), newMachine(t, "beta"), newMachine(t, "gamma")
	room := call[localapi.Room](a, "room/create", localapi.RoomReq{Room: "team"}).ID
	ann := a.agent("ann")
	join(t, a, b, room, localapi.InviteReq{History: true}, false)
	bea := b.agent("bea")
	before := call[localapi.SendResp](a, "send", localapi.SendReq{Agent: ann, Room: room, Text: "SECRET-BEFORE", To: []string{"everyone"}})
	if es := inboxWait(b, bea); len(es) != 1 || es[0].Text != "SECRET-BEFORE" {
		t.Fatalf("member with history: %+v", es)
	}
	cid := c.agent("cid")
	join(t, a, c, room, localapi.InviteReq{Role: node.RoleMember}, false)
	after := call[localapi.SendResp](a, "send", localapi.SendReq{Agent: ann, Room: room, Text: "after you joined"})
	es := inboxWait(c, cid)
	for _, e := range es {
		if e.ID == before.ID || strings.Contains(e.Text, "SECRET") {
			t.Fatalf("inbox showed earlier text: %+v", e)
		}
	}
	if !slices.ContainsFunc(es, func(e localapi.Entry) bool { return e.ID == after.ID }) {
		t.Fatalf("newcomer's inbox = %+v", es)
	}
	h := call[localapi.Entries](c, "history", localapi.HistoryReq{Room: room})
	for _, e := range h.Entries {
		if e.ID == before.ID || strings.Contains(e.Text, "SECRET") {
			t.Fatalf("history showed earlier text: %+v", e)
		}
	}
	var th localapi.Entries
	if err := c.c.Call(context.Background(), "thread", localapi.ThreadReq{Agent: cid, ID: before.ID}, &th); err == nil {
		for _, e := range th.Entries {
			if strings.Contains(e.Text, "SECRET") {
				t.Fatalf("thread showed earlier text: %+v", e)
			}
		}
	}
	var tr localapi.TraceResp
	err := c.c.Call(context.Background(), "trace", localapi.IDReq{ID: before.ID}, &tr)
	var ue *localapi.Error
	if err == nil && tr.Reason == store.ReasonLive {
		t.Fatalf("trace shows the earlier text as held: %+v", tr)
	} else if err != nil && !(errors.As(err, &ue) && ue.User) {
		t.Fatal(err)
	}
	var u localapi.Unread
	if err := c.c.Call(context.Background(), "unread", localapi.UnreadReq{Harness: "test", Cwd: "/w/cid", Lines: true}, &u); err != nil {
		t.Fatal(err)
	}
	for _, f := range u.First {
		if strings.Contains(f.Line, "SECRET") {
			t.Fatalf("first lines showed earlier text: %+v", f)
		}
	}
}

func TestV02NoticeShownOnceThroughStatus(t *testing.T) {
	if st := call[localapi.Status](newMachine(t, "fresh"), "status", localapi.Empty{}); st.Notice != "" {
		t.Fatalf("notice on a new home: %q", st.Notice)
	}
	// A home whose database holds v0.2 messages.
	m := newMachineWith(t, "old", func(home string) {
		ctx := context.Background()
		st, err := store.Open(filepath.Join(home, "a2p2.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		n, err := node.New(ctx, st, strings.Repeat("ab", 32), "old", time.Now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := n.Send(ctx, node.SendReq{From: n.HumanID(), Text: "v0.2 text"}); err != nil {
			t.Fatal(err)
		}
	})
	st := call[localapi.Status](m, "status", localapi.Empty{})
	if st.Notice != node.NoticeV02 || len(st.Rooms) != 1 || st.Rooms[0].ID == "local" {
		t.Fatalf("first start: notice %q rooms %+v", st.Notice, st.Rooms)
	}
	if h := call[localapi.Entries](m, "history", localapi.HistoryReq{}); len(h.Entries) != 0 {
		t.Fatalf("v0.2 messages shown: %+v", h)
	}
	m.stop()
	m.start()
	if st := call[localapi.Status](m, "status", localapi.Empty{}); st.Notice != "" {
		t.Fatalf("second start: notice %q", st.Notice)
	}
}

func TestDaemonServesOnlyTheNewPath(t *testing.T) {
	m := newMachine(t, "solo")
	st := call[localapi.Status](m, "status", localapi.Empty{})
	if len(st.Rooms) != 1 || st.Rooms[0].Label != "local" || st.Rooms[0].ID == "local" || len(st.Rooms[0].Admins) != 1 || st.Rooms[0].Admins[0] != st.Node {
		t.Fatalf("rooms on an empty home = %+v", st.Rooms)
	}
}
