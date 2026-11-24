package localapi

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// joinNet carries a join request straight to the admin machine's join
// server, as the TLS transport would: the server's key is checked first and
// the joiner's node id is the server's to learn.
type joinNet struct {
	from   string
	server **node.JoinServer
	admin  *string
}

func (n joinNet) Join(ctx context.Context, _ []string, check func(string) error, body []byte) (node.JoinReply, error) {
	if *n.server == nil {
		return node.JoinReply{}, errors.New("unreachable")
	}
	if err := check(*n.admin); err != nil {
		return node.JoinReply{}, err
	}
	return (*n.server).Handle(ctx, n.from, "127.0.0.1", body), nil
}

type machine struct {
	id     string
	s      *node.Surface
	js     *node.JoinServer
	c      *Client
	human  string
	server *node.JoinServer
	st     *store.Store
}

// newPair starts two machines with local APIs; b joins through a.
func newPair(t *testing.T) (*machine, *machine) {
	t.Helper()
	a, b := &machine{}, &machine{}
	start := func(m *machine, seed byte, peer *machine) {
		ctx := context.Background()
		st, err := store.Open(filepath.Join(t.TempDir(), "a2p2.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		key := ed25519.NewKeyFromSeed(append([]byte{seed}, make([]byte, 31)...))
		mach := node.NewMachine(st, key, node.MachineOpts{})
		m.st = st
		m.id = mach.ID
		rooms, err := node.OpenRooms(ctx, mach)
		if err != nil {
			t.Fatal(err)
		}
		m.js = node.NewJoinServer(rooms)
		j := node.NewJoiner(rooms, joinNet{from: mach.ID, server: &peer.js, admin: &peer.id}, node.JoinerOpts{Name: "m" + string('0'+seed)})
		m.s, err = node.OpenSurface(ctx, mach, rooms, node.SurfaceOpts{Name: "m", Joiner: j, Book: node.NewAddrBook(),
			Addrs: func() []string { return []string{"127.0.0.1:7443"} }, Network: node.Network{State: node.NetworkOn, Listen: "127.0.0.1:7443"}})
		if err != nil {
			t.Fatal(err)
		}
		dir, err := os.MkdirTemp("/tmp", "a2p2")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		l, err := net.Listen("unix", filepath.Join(dir, "s"))
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: Handler(m.s, Info{Version: "t"})}
		go srv.Serve(l)
		t.Cleanup(func() { srv.Close() })
		m.c = NewClient(filepath.Join(dir, "s"))
		m.human = m.s.HumanID()
	}
	start(a, 1, b)
	start(b, 2, a)
	return a, b
}

func call[T any](t *testing.T, c *Client, name string, req any) T {
	t.Helper()
	var out T
	raw, err := c.CallRaw(context.Background(), name, req, &out)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if !json.Valid(raw) {
		t.Fatalf("%s: not JSON: %s", name, raw)
	}
	return out
}

// userError asserts the call fails as a user's mistake (400).
func userError(t *testing.T, c *Client, name string, req any) {
	t.Helper()
	err := c.Call(context.Background(), name, req, nil)
	var e *Error
	if !errors.As(err, &e) || !e.User {
		t.Fatalf("%s %+v: want a user error, got %v", name, req, err)
	}
}

func TestEveryLocalCallRoundTripsAndRefusesAUserError(t *testing.T) {
	a, b := newPair(t)
	alice := call[store.Agent](t, a.c, "register", RegisterReq{Harness: "test", Label: "alice", Cwd: "/w/alice"}).ID
	bob := call[store.Agent](t, a.c, "register", RegisterReq{Harness: "test", Label: "bob", Cwd: "/w/bob"}).ID
	userError(t, a.c, "register", RegisterReq{Harness: "cli", Label: "human"})

	// send, inbox, seen, thread, ack, history, trace in the local room
	m := call[SendResp](t, a.c, "send", SendReq{Agent: alice, Text: "hello", To: []string{bob}})
	if m.ID == "" || m.Room != a.s.LocalRoom() || len(m.Created) != 24 {
		t.Fatalf("send = %+v", m)
	}
	userError(t, a.c, "send", SendReq{Agent: alice, Text: "x", Room: "nowhere"})
	in := call[InboxResp](t, a.c, "inbox", InboxReq{Agent: bob})
	if len(in.Entries) != 1 || in.Entries[0].Text != "hello" || in.Entries[0].From != alice || in.Entries[0].To[0] != bob ||
		in.Entries[0].Kind != node.EntryMessage || in.Waiting == nil {
		t.Fatalf("inbox = %+v", in)
	}
	userError(t, a.c, "inbox", InboxReq{Agent: bob, WaitMS: 200000})
	sr := call[SeenResp](t, a.c, "seen", SeenReq{Agent: bob, Entries: []SeenEntry{{Key: in.Entries[0].Key, Version: in.Entries[0].Version},
		{Key: "nope"}}})
	if len(sr.Results) != 2 || !sr.Results[0].OK || sr.Results[1].OK || sr.Results[1].Reason == "" {
		t.Fatalf("seen = %+v", sr)
	}
	userError(t, a.c, "seen", SeenReq{Agent: "x@00000000"})
	r := call[SendResp](t, a.c, "send", SendReq{Agent: bob, Text: "reply", ReplyTo: m.ID})
	if r.RoomLabel != node.LocalLabel || len(r.To) != 1 || r.To[0] != alice || in.Entries[0].RoomLabel != node.LocalLabel {
		t.Fatalf("send names its room and addressees: %+v, %+v", r, in.Entries[0])
	}
	if st := call[Status](t, a.c, "status", StatusReq{}); len(st.Rooms[0].Agents) != 2 || st.Rooms[0].Agents[0].LastSeen == "" {
		t.Fatalf("status lists the agents seen writing: %+v", st.Rooms[0].Agents)
	}
	th := call[Entries](t, a.c, "thread", ThreadReq{Agent: alice, ID: m.ID})
	if len(th.Entries) != 2 || th.Entries[1].ID != r.ID || th.Entries[1].Parent != m.ID {
		t.Fatalf("thread = %+v", th)
	}
	userError(t, a.c, "thread", ThreadReq{Agent: alice, ID: strings.Repeat("0", 32)})
	ack := call[AckResp](t, a.c, "ack", AckReq{Agent: bob, ID: m.ID})
	if ack.Handled == "" {
		t.Fatalf("ack = %+v", ack)
	}
	userError(t, a.c, "ack", AckReq{Agent: alice, ID: m.ID})
	h := call[Entries](t, a.c, "history", HistoryReq{})
	if len(h.Entries) != 2 {
		t.Fatalf("history = %+v", h)
	}
	userError(t, a.c, "history", HistoryReq{Room: "nowhere"})
	tr := call[TraceResp](t, a.c, "trace", IDReq{ID: m.ID})
	if !tr.Saved || tr.Reason != store.ReasonLive || len(tr.Handled) != 1 || tr.Handled[0].Agent != bob || tr.Copies == nil {
		t.Fatalf("trace = %+v", tr)
	}
	userError(t, a.c, "trace", IDReq{ID: "nope"})

	// rooms, invites, the join with both approvals, requests and confirm
	room := call[Room](t, a.c, "room/create", RoomReq{Room: "team"})
	if room.ID == "" || len(room.Members) != 1 || len(room.Admins) != 1 {
		t.Fatalf("room/create = %+v", room)
	}
	userError(t, a.c, "room/create", RoomReq{Room: "bad label"})
	inv := call[InviteResp](t, a.c, "room/invite", InviteReq{Room: "team"})
	if !strings.HasPrefix(inv.Token, "a2p2:") || inv.Room != room.ID {
		t.Fatalf("invite = %+v", inv)
	}
	userError(t, a.c, "room/invite", InviteReq{Room: "local"})
	pv := call[PreviewResp](t, b.c, "room/join", JoinReq{Token: inv.Token})
	if pv.State != node.JoinStatePreviewed || pv.Room.ID != room.ID || pv.Admin != a.id || pv.Preview == "" || pv.Role != node.RoleAdmin {
		t.Fatalf("preview = %+v", pv)
	}
	userError(t, b.c, "room/join", JoinReq{Token: "not a token"})
	userError(t, b.c, "room/join/accept", AcceptReq{Token: inv.Token})
	acc := call[AcceptResp](t, b.c, "room/join/accept", AcceptReq{Token: inv.Token, Preview: pv.Preview})
	if acc.State != node.JoinStateAccepted {
		t.Fatalf("accept = %+v", acc)
	}
	qs := call[RequestsResp](t, a.c, "requests", Empty{})
	if len(qs.Requests) != 1 || qs.Requests[0].Node != b.id || qs.Requests[0].Room != room.ID || !strings.Contains(qs.Requests[0].Text, "admin") {
		t.Fatalf("requests = %+v", qs)
	}
	if q := qs.Requests[0]; q.Fingerprint != node.Fingerprint(b.id) || q.Action != node.ActionAdmitAdmin || q.RoomLabel != "team" ||
		q.Deadline == "" || !strings.HasPrefix(q.Text, "add machine "+q.Fingerprint) {
		t.Fatalf("request terms = %+v", q)
	}
	al := call[AlertsResp](t, a.c, "alerts", WaitReq{})
	if len(al.Alerts) != 1 || al.Alerts[0].Kind != node.AlertRequest || al.Alerts[0].ID != qs.Requests[0].ID {
		t.Fatalf("alerts = %+v", al)
	}
	userError(t, a.c, "alerts", WaitReq{WaitMS: 120001})
	cf := call[ConfirmResp](t, a.c, "confirm", ConfirmReq{ID: qs.Requests[0].ID, Approve: true})
	if cf.State != store.RequestApproved || len(cf.Entries) == 0 {
		t.Fatalf("confirm = %+v", cf)
	}
	userError(t, a.c, "confirm", ConfirmReq{ID: "nope", Approve: true})
	acc = call[AcceptResp](t, b.c, "room/join/accept", AcceptReq{Token: inv.Token, Preview: pv.Preview})
	if acc.State != node.JoinStateSigned || acc.Room != room.ID || acc.Role != node.RoleAdmin {
		t.Fatalf("accept after approval = %+v", acc)
	}
	rl := call[RoomsResp](t, b.c, "room/list", RoomListReq{Room: room.ID})
	if len(rl.Rooms) != 1 || len(rl.Rooms[0].Members) != 2 || len(rl.Rooms[0].Admins) != 2 {
		t.Fatalf("room/list on the joiner = %+v", rl)
	}
	userError(t, b.c, "room/list", RoomListReq{Room: "nowhere"})

	// room/admin, room/remove raise requests; room/leave
	userError(t, a.c, "room/admin", MemberReq{Room: room.ID, Node: b.id})
	ch := call[ChangeResp](t, a.c, "room/remove", MemberReq{Room: room.ID, Node: b.id[:8]})
	if ch.Request == "" || ch.State != "waiting" || !strings.Contains(ch.Text, "remove") {
		t.Fatalf("room/remove = %+v", ch)
	}
	userError(t, a.c, "room/remove", MemberReq{Room: room.ID, Node: "ffffffff"})
	call[Empty](t, b.c, "room/leave", RoomReq{Room: room.ID})
	userError(t, b.c, "room/leave", RoomReq{Room: "local"})

	// unread, status, stop
	u := call[Unread](t, a.c, "unread", UnreadReq{Harness: "test", Cwd: "/w/alice/sub", Lines: true})
	if u.Agent != alice || u.Count != 1 || len(u.First) != 1 || u.First[0].Line != "reply" {
		t.Fatalf("unread = %+v", u)
	}
	if u := call[Unread](t, a.c, "unread", UnreadReq{Harness: "test", Cwd: "/elsewhere"}); u.Agent != "" {
		t.Fatalf("unread for nobody = %+v", u)
	}
	st := call[Status](t, a.c, "status", Empty{})
	if st.Node != a.id || st.Human != a.human || len(st.Rooms) != 2 || st.Rooms[0].ID != a.s.LocalRoom() || len(st.Peers) != 1 || st.Peers[0].Node != b.id {
		t.Fatalf("status = %+v", st)
	}
	if st.Network.State != node.NetworkOn || st.Waiting != 1 {
		t.Fatalf("status network %+v waiting %d", st.Network, st.Waiting)
	}
	// net records a person's decision; it applies at the next start.
	userError(t, a.c, "net", NetReq{State: "sideways"})
	userError(t, a.c, "net", NetReq{State: "on", Listen: "nowhere"})
	nr := call[NetResp](t, a.c, "net", NetReq{State: "outbound"})
	if nr.State != node.NetworkOutbound || nr.Request == "" || nr.Applies == "" || !strings.Contains(nr.Text, "listen on no address") {
		t.Fatalf("net = %+v", nr)
	}
	if n, err := node.LoadNetwork(context.Background(), a.st); err != nil || n.Request != nr.Request {
		t.Fatalf("recorded %+v %v", n, err)
	}
	call[Empty](t, a.c, "stop", Empty{})
	if err := a.c.Call(context.Background(), "nosuch", nil, nil); err == nil {
		t.Fatal("unknown call accepted")
	}
}

func TestDeclinedJoinIsRefusedOnTheJoiner(t *testing.T) {
	a, b := newPair(t)
	room := call[Room](t, a.c, "room/create", RoomReq{Room: "team"})
	inv := call[InviteResp](t, a.c, "room/invite", InviteReq{Room: room.ID, Role: node.RoleMember})
	pv := call[PreviewResp](t, b.c, "room/join", JoinReq{Token: inv.Token})
	call[AcceptResp](t, b.c, "room/join/accept", AcceptReq{Token: inv.Token, Preview: pv.Preview})
	qs := call[RequestsResp](t, a.c, "requests", Empty{})
	cf := call[ConfirmResp](t, a.c, "confirm", ConfirmReq{ID: qs.Requests[0].ID})
	if cf.State != store.RequestDeclined {
		t.Fatalf("decline = %+v", cf)
	}
	acc := call[AcceptResp](t, b.c, "room/join/accept", AcceptReq{Token: inv.Token, Preview: pv.Preview})
	if acc.State != node.JoinStateRefused || acc.Reason != node.CodeDeclined {
		t.Fatalf("accept after decline = %+v", acc)
	}
}

// TestConfirmBeforeCatchingUpAnswersNotCaughtUpAndSignsOnlyWithTheOverride:
// a restarted admin whose room has another member has not checked the
// room's log with anyone since it started. confirm then answers reason
// not_caught_up and leaves the request pending; repeated with
// not_caught_up_ok it signs, and the override is recorded.
func TestConfirmBeforeCatchingUpAnswersNotCaughtUpAndSignsOnlyWithTheOverride(t *testing.T) {
	ctx := context.Background()
	a, b := newPair(t)
	room := call[Room](t, a.c, "room/create", RoomReq{Room: "team"})
	inv := call[InviteResp](t, a.c, "room/invite", InviteReq{Room: room.ID, Role: node.RoleMember})
	pv := call[PreviewResp](t, b.c, "room/join", JoinReq{Token: inv.Token})
	call[AcceptResp](t, b.c, "room/join/accept", AcceptReq{Token: inv.Token, Preview: pv.Preview})
	qs := call[RequestsResp](t, a.c, "requests", Empty{})
	if cf := call[ConfirmResp](t, a.c, "confirm", ConfirmReq{ID: qs.Requests[0].ID, Approve: true}); cf.State != store.RequestApproved {
		t.Fatalf("admitting b: %+v", cf)
	}

	// a restarts: the same store and key, a new machine and socket.
	mach := node.NewMachine(a.st, ed25519.NewKeyFromSeed(append([]byte{1}, make([]byte, 31)...)), node.MachineOpts{})
	rooms, err := node.OpenRooms(ctx, mach)
	if err != nil {
		t.Fatal(err)
	}
	s, err := node.OpenSurface(ctx, mach, rooms, node.SurfaceOpts{Name: "m"})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "a2p2")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: Handler(s, Info{Version: "t"})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	c := NewClient(filepath.Join(dir, "s"))

	ch := call[ChangeResp](t, c, "room/remove", MemberReq{Room: room.ID, Node: b.id})
	cf := call[ConfirmResp](t, c, "confirm", ConfirmReq{ID: ch.Request, Approve: true})
	if cf.State != store.RequestFailed || cf.Reason != node.ReasonNotCaughtUp || len(cf.Entries) != 0 {
		t.Fatalf("first approval = %+v", cf)
	}
	if q, err := a.st.GetRequest(ctx, ch.Request); err != nil || q.State != store.RequestPending {
		t.Fatalf("after the first approval: %+v %v, want pending", q, err)
	}
	cf = call[ConfirmResp](t, c, "confirm", ConfirmReq{ID: ch.Request, Approve: true, NotCaughtUpOK: true})
	if cf.State != store.RequestApproved || len(cf.Entries) == 0 {
		t.Fatalf("second approval = %+v", cf)
	}
	if q, err := a.st.GetRequest(ctx, ch.Request); err != nil || !q.NotCaughtUp {
		t.Fatalf("the override was not recorded: %+v %v", q, err)
	}
}
