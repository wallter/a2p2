package node

import (
	"context"
	"crypto/ed25519"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/store"
)

type surfEnv struct {
	t    *testing.T
	path string
	key  ed25519.PrivateKey
	st   *store.Store
	s    *Surface
}

func newSurf(t *testing.T) *surfEnv {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = 7
	e := &surfEnv{t: t, path: filepath.Join(t.TempDir(), "a2p2.db"), key: ed25519.NewKeyFromSeed(seed)}
	e.open()
	return e
}

func (e *surfEnv) open() {
	e.t.Helper()
	st, err := store.Open(e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	e.st = st
	e.t.Cleanup(func() { st.Close() })
	m := NewMachine(st, e.key, MachineOpts{})
	r, err := OpenRooms(context.Background(), m)
	if err != nil {
		e.t.Fatal(err)
	}
	e.s, err = OpenSurface(context.Background(), m, r, SurfaceOpts{Name: "solo", Book: NewAddrBook()})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *surfEnv) restart() {
	e.t.Helper()
	e.st.Close()
	e.open()
}

func (e *surfEnv) agent(label string) string {
	e.t.Helper()
	a, err := e.s.Register(context.Background(), "test", label, "/work/"+label)
	if err != nil {
		e.t.Fatal(err)
	}
	return a.ID
}

func (e *surfEnv) send(in SendInput) Sent {
	e.t.Helper()
	s, err := e.s.Send(context.Background(), in)
	if err != nil {
		e.t.Fatalf("send %+v: %v", in, err)
	}
	return s
}

func (e *surfEnv) inbox(agent string) []Entry {
	e.t.Helper()
	v, err := e.s.Inbox(context.Background(), agent, "", 0, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return v.Entries
}

func (e *surfEnv) read(agent string) []Entry {
	e.t.Helper()
	es := e.inbox(agent)
	var reads []store.Read
	for _, x := range es {
		reads = append(reads, store.Read{Key: x.Key, Version: x.Version})
	}
	res, err := e.s.Seen(context.Background(), agent, reads)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, r := range res {
		if !r.Confirmed {
			e.t.Fatalf("seen %s refused: %s", r.Key, r.Reason)
		}
	}
	return es
}

func isUser(err error) bool {
	_, ok := err.(*UserError)
	return ok
}

func TestSurfaceLocalRoomIsCreatedOnceAndOnlyThisMachineIsInIt(t *testing.T) {
	e := newSurf(t)
	ctx := context.Background()
	local := e.s.LocalRoom()
	if got, err := e.s.ResolveRoom(ctx, ""); err != nil || got != local {
		t.Fatalf("default room = %q, %v", got, err)
	}
	if got, err := e.s.ResolveRoom(ctx, LocalLabel); err != nil || got != local {
		t.Fatalf("local by label = %q, %v", got, err)
	}
	e.restart()
	if e.s.LocalRoom() != local {
		t.Fatal("local room changed across a restart")
	}
	rooms, err := e.s.RoomList(ctx, "")
	if err != nil || len(rooms) != 1 || rooms[0].ID != local || len(rooms[0].Members) != 1 || rooms[0].Members[0].Node != e.s.Node() {
		t.Fatalf("rooms = %+v, %v", rooms, err)
	}
	if _, err := e.s.Invite(ctx, "", "", false); !isUser(err) {
		t.Fatalf("invite to local: %v", err)
	}
	if err := e.s.Leave(ctx, ""); !isUser(err) {
		t.Fatalf("leave local: %v", err)
	}
	agents, _ := e.s.Agents(ctx)
	if len(agents) != 1 || agents[0].ID != e.s.HumanID() {
		t.Fatalf("agents = %+v", agents)
	}
}

func TestSurfaceResolvesRoomsByLabelAndRefusesAmbiguousLabels(t *testing.T) {
	e := newSurf(t)
	ctx := context.Background()
	a, err := e.s.CreateRoom(ctx, "team")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := e.s.ResolveRoom(ctx, "team"); err != nil || got != a.ID {
		t.Fatalf("by label: %q %v", got, err)
	}
	b, _ := e.s.CreateRoom(ctx, "team")
	if _, err := e.s.ResolveRoom(ctx, "team"); !isUser(err) || !strings.Contains(err.Error(), a.ID) || !strings.Contains(err.Error(), b.ID) {
		t.Fatalf("ambiguous: %v", err)
	}
	if got, err := e.s.ResolveRoom(ctx, b.ID); err != nil || got != b.ID {
		t.Fatalf("by id: %q %v", got, err)
	}
	if _, err := e.s.ResolveRoom(ctx, "nope"); !isUser(err) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := e.s.CreateRoom(ctx, "bad label"); !isUser(err) {
		t.Fatalf("bad label: %v", err)
	}
}

func TestSurfaceSendInboxSeenReplyAckThreadHistoryTrace(t *testing.T) {
	e := newSurf(t)
	ctx := context.Background()
	alice, bob := e.agent("alice"), e.agent("bob")
	q := e.send(SendInput{Agent: alice, Text: "first line\nsecond", To: []string{bob}})
	if es := e.inbox(alice); len(es) != 0 {
		t.Fatalf("own message unread: %+v", es)
	}
	es := e.read(bob)
	if len(es) != 1 || es[0].ID != q.ID || es[0].From != alice || len(es[0].To) != 1 || AgentID(es[0].To[0]) != bob {
		t.Fatalf("bob's inbox = %+v", es)
	}
	if es := e.inbox(bob); len(es) != 0 {
		t.Fatalf("seen did not confirm: %+v", es)
	}
	// A reply goes to the parent's author, in the parent's room.
	r := e.send(SendInput{Agent: bob, Text: "answer", ReplyTo: q.ID})
	es = e.read(alice)
	if len(es) != 1 || es[0].ID != r.ID || es[0].Parent != q.ID || es[0].Thread != q.ID || es[0].ParentState != ParentHere {
		t.Fatalf("alice's inbox = %+v", es)
	}
	h, err := e.s.Ack(ctx, bob, q.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Ack(ctx, alice, q.ID); !isUser(err) {
		t.Fatalf("author acks own: %v", err)
	}
	th, err := e.s.Thread(ctx, alice, q.ID)
	if err != nil || len(th) != 2 || th[0].ID != q.ID || th[1].ID != r.ID {
		t.Fatalf("thread = %+v, %v", th, err)
	}
	br, err := e.s.Thread(ctx, alice, r.ID)
	if err != nil || len(br) != 1 || br[0].ID != r.ID {
		t.Fatalf("branch = %+v, %v", br, err)
	}
	hist, err := e.s.History(ctx, "", "", 0)
	if err != nil || len(hist) != 2 || hist[0].ID != q.ID {
		t.Fatalf("history = %+v, %v", hist, err)
	}
	if older, err := e.s.History(ctx, "", r.ID, 0); err != nil || len(older) != 1 || older[0].ID != q.ID {
		t.Fatalf("history before = %+v, %v", older, err)
	}
	tr, err := e.s.Trace(ctx, q.ID)
	if err != nil || !tr.Saved || tr.Reason != store.ReasonLive || tr.Room != e.s.LocalRoom() ||
		len(tr.Handled) != 1 || tr.Handled[0].Agent != bob {
		t.Fatalf("trace = %+v, %v (handled %s)", tr, err, h.ID)
	}
	if _, err := e.s.Trace(ctx, strings.Repeat("0", 32)); !isUser(err) {
		t.Fatalf("trace unknown: %v", err)
	}
}

func TestSurfaceReplacesAndWithdraws(t *testing.T) {
	e := newSurf(t)
	alice, bob := e.agent("alice"), e.agent("bob")
	m := e.send(SendInput{Agent: alice, Text: "v1"})
	e.read(bob)
	e.send(SendInput{Agent: alice, Replaces: m.ID, Text: "v2"})
	es := e.read(bob)
	if len(es) != 1 || es[0].Kind != EntryReplaced || es[0].Text != "v2" {
		t.Fatalf("replaced = %+v", es)
	}
	e.send(SendInput{Agent: alice, Replaces: m.ID})
	es = e.read(bob)
	if len(es) != 1 || es[0].Kind != EntryWithdrawn {
		t.Fatalf("withdrawn = %+v", es)
	}
	if _, err := e.s.Send(context.Background(), SendInput{Agent: alice, Replaces: m.ID, Text: "x", To: []string{bob}}); !isUser(err) {
		t.Fatalf("replace with to: %v", err)
	}
}

func TestSurfaceRefusesForeignAgentsAndUnknownAddressees(t *testing.T) {
	e := newSurf(t)
	ctx := context.Background()
	alice := e.agent("alice")
	for _, in := range []SendInput{
		{Agent: "alice@deadbeef", Text: "x"},
		{Agent: alice, Text: " "},
		{Agent: alice, Text: "x", To: []string{"bob@deadbeef"}},
		{Agent: alice, Text: "x", To: []string{Everyone, alice}},
		{Agent: alice, Text: "x", Room: "nowhere"},
	} {
		if _, err := e.s.Send(ctx, in); !isUser(err) {
			t.Errorf("%+v: %v", in, err)
		}
	}
	if _, err := e.s.Register(ctx, "cli", HumanLabel, ""); !isUser(err) {
		t.Fatalf("human label taken by a script: %v", err)
	}
	if _, err := e.s.Inbox(ctx, alice, "", 0, 3*time.Minute); !isUser(err) {
		t.Fatalf("wait bound: %v", err)
	}
	e.send(SendInput{Agent: alice, Text: "to all", To: []string{Everyone}})
}

func TestSurfaceUnreadCountsAndFirstLinesNeverMarkSeen(t *testing.T) {
	e := newSurf(t)
	ctx := context.Background()
	alice, bob := e.agent("alice"), e.agent("bob")
	e.send(SendInput{Agent: alice, Text: "line one\nline two"})
	e.send(SendInput{Agent: alice, Text: strings.Repeat("y", 200)})
	for range 3 {
		u, err := e.s.Unread(ctx, bob, true)
		if err != nil || u.Count != 2 || len(u.From) != 1 || u.From[0] != alice || len(u.Rooms) != 1 || u.Rooms[0].Count != 2 ||
			len(u.First) != 2 || u.First[0].Line != "line one" || !strings.HasSuffix(u.First[1].Line, "...") {
			t.Fatalf("unread = %+v, %v", u, err)
		}
	}
	if u, _ := e.s.Unread(ctx, bob, false); len(u.First) != 0 {
		t.Fatalf("first lines without asking: %+v", u)
	}
	if es := e.inbox(bob); len(es) != 2 {
		t.Fatalf("unread marked something seen: %+v", es)
	}
}

func TestSurfaceInboxWaitReturnsWhenAMessageArrives(t *testing.T) {
	e := newSurf(t)
	alice, bob := e.agent("alice"), e.agent("bob")
	go func() {
		time.Sleep(200 * time.Millisecond)
		e.s.Send(context.Background(), SendInput{Agent: alice, Text: "wake"})
	}()
	start := time.Now()
	v, err := e.s.Inbox(context.Background(), bob, "", 0, 20*time.Second)
	if err != nil || len(v.Entries) != 1 || time.Since(start) > 5*time.Second {
		t.Fatalf("after %v: %+v %v", time.Since(start), v, err)
	}
	start = time.Now()
	v, err = e.s.Inbox(context.Background(), alice, "", 0, 300*time.Millisecond)
	if err != nil || len(v.Entries) != 0 || time.Since(start) < 300*time.Millisecond {
		t.Fatalf("empty wait: %+v %v after %v", v, err, time.Since(start))
	}
}

func TestSurfaceSeenConfirmsOnlyWhatTheInboxShowsNow(t *testing.T) {
	e := newSurf(t)
	ctx := context.Background()
	alice, bob := e.agent("alice"), e.agent("bob")
	m := e.send(SendInput{Agent: alice, Text: "v1"})
	es := e.inbox(bob)
	e.send(SendInput{Agent: alice, Replaces: m.ID, Text: "v2"})
	res, err := e.s.Seen(ctx, bob, []store.Read{{Key: es[0].Key, Version: es[0].Version}, {Key: strings.Repeat("a", 32)}})
	if err != nil || len(res) != 2 || res[0].Confirmed || res[0].Reason != SeenVersionChanged || res[1].Confirmed || res[1].Reason != SeenNotShown {
		t.Fatalf("seen = %+v, %v", res, err)
	}
}

func TestV02NoticeIsShownOnceForADatabaseWithV02Messages(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "a2p2.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	n, err := New(ctx, st, strings.Repeat("ab", 32), "old", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.Send(ctx, SendReq{From: n.HumanID(), Text: "from v0.2"}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	seed := make([]byte, ed25519.SeedSize)
	e := &surfEnv{t: t, path: path, key: ed25519.NewKeyFromSeed(seed)}
	e.open()
	if e.s.Notice() != NoticeV02 {
		t.Fatalf("first start: notice %q", e.s.Notice())
	}
	if hist, err := e.s.History(ctx, "", "", 0); err != nil || len(hist) != 0 {
		t.Fatalf("v0.2 message shown: %+v %v", hist, err)
	}
	e.restart()
	if e.s.Notice() != "" {
		t.Fatalf("second start: notice %q", e.s.Notice())
	}
	fresh := newSurf(t)
	if fresh.s.Notice() != "" {
		t.Fatal("notice on an empty database")
	}
}
