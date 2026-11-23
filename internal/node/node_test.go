package node

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

func init() {
	RetryBase, RetryMax, Tick, RefreshMinGap = 2*time.Millisecond, 20*time.Millisecond, 3*time.Millisecond, 0
}

// fakeNet connects in-process nodes and injects the failures a real network has.
type fakeNet struct {
	mu        sync.Mutex
	peers     map[string]*peer
	down      map[string]bool
	dropResp  map[string]int  // destination -> responses to lose after the receiver commits
	garble    map[string]bool // destination -> answer with ids that do not match the batch
	afterJoin func()          // runs once on the owner after a join commits, before the joiner hears
	delivered int
}

type peer struct {
	n      *Node
	m      *Mesh
	dir    string
	st     *store.Store
	cancel context.CancelFunc
	done   chan struct{}
}

type fakeT struct {
	net  *fakeNet
	self string
}

func (t fakeT) target(node string) (*peer, error) {
	t.net.mu.Lock()
	defer t.net.mu.Unlock()
	p := t.net.peers[node]
	if p == nil || t.net.down[node] || t.net.down[t.self] {
		return nil, errors.New("connection refused")
	}
	return p, nil
}

func (t fakeT) Deliver(ctx context.Context, to store.Member, req DeliverReq) ([]Result, error) {
	p, err := t.target(to.Node)
	if err != nil {
		return nil, err
	}
	cp := DeliverReq{Envelopes: append([]core.Envelope(nil), req.Envelopes...), Versions: req.Versions}
	res := p.n.Receive(ctx, t.self, cp, func(room string) { go p.m.Refresh(context.Background(), room) })
	t.net.mu.Lock()
	defer t.net.mu.Unlock()
	t.net.delivered++
	if t.net.dropResp[to.Node] > 0 {
		t.net.dropResp[to.Node]--
		return nil, errors.New("connection reset before response")
	}
	if t.net.garble[to.Node] {
		for i := range res {
			res[i].ID = "garbled"
		}
	}
	return res, nil
}

func (t fakeT) Join(ctx context.Context, owner store.Member, req JoinReq) (store.Room, error) {
	p, err := t.target(owner.Node)
	if err != nil {
		return store.Room{}, err
	}
	room, err := p.n.HandleJoin(ctx, t.self, req)
	t.net.mu.Lock()
	hook := t.net.afterJoin
	t.net.afterJoin = nil
	t.net.mu.Unlock()
	if hook != nil && err == nil {
		hook()
	}
	return room, err
}

func (t fakeT) Room(ctx context.Context, owner store.Member, req RoomReq) (store.Room, error) {
	p, err := t.target(owner.Node)
	if err != nil {
		return store.Room{}, err
	}
	return p.n.HandleRoom(ctx, t.self, req)
}

// clockSkew lets a test move every node's clock without touching shared tunables.
var clockSkew atomic.Int64

func testNow() time.Time { return time.Now().Add(time.Duration(clockSkew.Load())) }

func newNet() *fakeNet {
	return &fakeNet{peers: map[string]*peer{}, down: map[string]bool{}, dropResp: map[string]int{}, garble: map[string]bool{}}
}

func randNode() string {
	var b [32]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// start boots a node in dir (reusing its database if present) with identity id.
func (fn *fakeNet) start(t *testing.T, name, id, dir string) *peer {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, "a2p2.db"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := New(context.Background(), st, id, name, testNow)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &peer{n: n, dir: dir, st: st, cancel: cancel, done: make(chan struct{})}
	p.m = NewMesh(n, fakeT{fn, id}, func() []string { return []string{name + ":7"} })
	fn.mu.Lock()
	fn.peers[id] = p
	fn.mu.Unlock()
	go func() { p.m.Run(ctx); close(p.done) }()
	t.Cleanup(func() { p.stop() })
	return p
}

func (p *peer) stop() {
	p.cancel()
	<-p.done
	p.st.Close()
}

func (fn *fakeNet) add(t *testing.T, name string) *peer {
	return fn.start(t, name, randNode(), t.TempDir())
}

func (fn *fakeNet) setDown(node string, down bool) {
	fn.mu.Lock()
	fn.down[node] = down
	fn.mu.Unlock()
}

var bg = context.Background()

func (p *peer) agent(t *testing.T, label string) string {
	t.Helper()
	a, err := p.n.Register(bg, "test", label, "/tmp/"+label)
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
}

func (p *peer) send(t *testing.T, from, room, text string) store.Message {
	t.Helper()
	m, err := p.n.Send(bg, SendReq{From: from, Room: room, Text: text})
	if err != nil {
		t.Fatalf("send %q: %v", text, err)
	}
	return m
}

// join creates a room on owner and joins every other peer to it.
func join(t *testing.T, owner *peer, others ...*peer) store.Room {
	t.Helper()
	room, err := owner.n.CreateRoom(bg, "team", owner.m.addrs())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range others {
		tok, _, err := owner.n.Invite(bg, room.ID, owner.m.addrs())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := o.m.Join(bg, tok); err != nil {
			t.Fatalf("join: %v", err)
		}
	}
	for _, o := range others { // everyone learns the final member list
		o.m.Refresh(bg, room.ID)
	}
	return room
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func (p *peer) state(t *testing.T, id, node string) string {
	t.Helper()
	ds, err := p.st.Deliveries(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		if d.Node == node {
			return d.State
		}
	}
	return ""
}

func (p *peer) texts(t *testing.T, room string) []string {
	t.Helper()
	msgs, err := p.n.History(bg, room, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, m := range msgs {
		out = append(out, m.Body.Text)
	}
	return out
}

// ---- single node ----

func TestLocalRoundTrip(t *testing.T) {
	p := newNet().add(t, "solo")
	claude, codex := p.agent(t, "claude"), p.agent(t, "codex")

	m := p.send(t, claude, "", "review the API contract?")
	if m.Seq < 1 || m.Room != core.LocalRoom {
		t.Fatalf("bad message: %+v", m)
	}
	if own, _ := p.n.Inbox(bg, claude, "", 10, 0); len(own) != 0 {
		t.Fatalf("sender sees its own message as unread")
	}
	in, err := p.n.Inbox(bg, codex, "", 10, 0)
	if err != nil || len(in) != 1 || in[0].ID != m.ID {
		t.Fatalf("inbox = %v, %v", in, err)
	}
	// Not confirmed yet: a lost tool result must be redelivered.
	if again, _ := p.n.Inbox(bg, codex, "", 10, 0); len(again) != 1 {
		t.Fatalf("unconfirmed message vanished from inbox")
	}
	if err := p.n.Seen(bg, codex, m.ID); err != nil {
		t.Fatal(err)
	}
	if after, _ := p.n.Inbox(bg, codex, "", 10, 0); len(after) != 0 {
		t.Fatalf("seen message still unread")
	}

	reply, err := p.n.Send(bg, SendReq{From: codex, ReplyTo: m.ID, Text: "looks fine"})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Thread != m.ID || len(reply.To) != 1 || reply.To[0] != claude || reply.Room != m.Room {
		t.Fatalf("reply not threaded/addressed: %+v", reply)
	}
	tr, _ := p.n.Trace(bg, m.ID)
	handled := false
	for _, k := range tr.Marks {
		handled = handled || (k.Agent == codex && k.State == core.ReceiptHandled)
	}
	if !handled {
		t.Errorf("replying should mark the original handled: %+v", tr.Marks)
	}
}

func TestAddressedMessagesAndNewAgents(t *testing.T) {
	p := newNet().add(t, "solo")
	a, b, c := p.agent(t, "a"), p.agent(t, "b"), p.agent(t, "c")
	if _, err := p.n.Send(bg, SendReq{From: a, To: []string{b}, Text: "for b only"}); err != nil {
		t.Fatal(err)
	}
	if in, _ := p.n.Inbox(bg, b, "", 10, 0); len(in) != 1 {
		t.Errorf("b should have 1 unread, has %d", len(in))
	}
	if in, _ := p.n.Inbox(bg, c, "", 10, 0); len(in) != 0 {
		t.Errorf("c should have 0 unread, has %d", len(in))
	}
	late := p.agent(t, "late")
	p.send(t, a, "", "after late arrived")
	if in, _ := p.n.Inbox(bg, late, "", 10, 0); len(in) != 1 {
		t.Errorf("a new agent should not inherit history as unread: got %d", len(in))
	}
}

func TestHookFindsMailboxByHarnessAndDirectory(t *testing.T) {
	p := newNet().add(t, "solo")
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	sub := filepath.Join(real, "pkg")
	os.Mkdir(sub, 0o755)
	a, err := p.n.Register(bg, "claude", "", real)
	if err != nil {
		t.Fatal(err)
	}
	if want := "claude-" + filepath.Base(real); a.Label != want {
		t.Errorf("label = %q, want %q", a.Label, want)
	}
	again, _ := p.n.Register(bg, "claude", "", link) // a restart, reached through a symlink
	if again.ID != a.ID {
		t.Errorf("same harness and directory got a different mailbox: %s vs %s", again.ID, a.ID)
	}
	for _, dir := range []string{real, link, sub, filepath.Join(link, "pkg")} {
		got, ok, err := p.n.AgentFor(bg, "claude", dir)
		if err != nil || !ok || got.ID != a.ID {
			t.Errorf("AgentFor(%s) = %v, %v, %v", dir, got.ID, ok, err)
		}
	}
	if _, ok, _ := p.n.AgentFor(bg, "codex", real); ok {
		t.Errorf("a different harness matched this mailbox")
	}
	if _, ok, _ := p.n.AgentFor(bg, "claude", filepath.Dir(real)); ok {
		t.Errorf("a parent directory matched this mailbox")
	}
}

func TestSendIsIdempotentByID(t *testing.T) {
	p := newNet().add(t, "solo")
	a := p.agent(t, "a")
	id := core.NewID(time.Now())
	first, err := p.n.Send(bg, SendReq{ID: id, From: a, Text: "once"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.n.Send(bg, SendReq{ID: id, From: a, Text: "once"})
	if err != nil || second.N != first.N || second.Seq != first.Seq {
		t.Fatalf("retry created a second message: %+v vs %+v (%v)", first, second, err)
	}
	if got := p.texts(t, ""); len(got) != 1 {
		t.Fatalf("want 1 message, have %v", got)
	}
}

func TestInboxWaitWakesOnArrival(t *testing.T) {
	p := newNet().add(t, "solo")
	a, b := p.agent(t, "a"), p.agent(t, "b")
	got := make(chan int, 1)
	go func() {
		in, _ := p.n.Inbox(bg, b, "", 10, 3*time.Second)
		got <- len(in)
	}()
	time.Sleep(20 * time.Millisecond)
	p.send(t, a, "", "wake up")
	select {
	case n := <-got:
		if n != 1 {
			t.Fatalf("woke with %d messages", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("long poll did not wake")
	}
}

func TestRejectsBadInput(t *testing.T) {
	p := newNet().add(t, "solo")
	a := p.agent(t, "a")
	var ue *UserError
	for name, req := range map[string]SendReq{
		"foreign agent": {From: "x@00000000", Text: "hi"},
		"empty":         {From: a, Text: " "},
		"unknown room":  {From: a, Room: "nope", Text: "hi"},
		"fake answer":   {From: a, Kind: core.KindAnswer, Text: "yes", ReplyTo: core.NewID(time.Now())},
		"fake receipt":  {From: a, Kind: core.KindReceipt, Text: "x"},
		"reply nowhere": {From: a, ReplyTo: core.NewID(time.Now()), Text: "hi"},
	} {
		if _, err := p.n.Send(bg, req); !errors.As(err, &ue) {
			t.Errorf("%s: want UserError, got %v", name, err)
		}
	}
	if _, err := p.n.Register(bg, "claude", core.HumanName, ""); !errors.As(err, &ue) {
		t.Errorf("agent could claim the human label: %v", err)
	}
}

// ---- several nodes ----

func TestDeliverAndReceipt(t *testing.T) {
	net := newNet()
	a, b := net.add(t, "a"), net.add(t, "b")
	room := join(t, a, b)
	alice, bob := a.agent(t, "alice"), b.agent(t, "bob")

	m := a.send(t, alice, room.ID, "hello bob")
	eventually(t, "stored on b", func() bool { return a.state(t, m.ID, b.n.ID) == store.Stored })
	in, _ := b.n.Inbox(bg, bob, "", 10, 0)
	if len(in) != 1 || in[0].FromNode != a.n.ID || in[0].Body.Text != "hello bob" {
		t.Fatalf("b inbox = %+v", in)
	}
	if err := b.n.Ack(bg, bob, m.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.n.Ack(bg, bob, m.ID); err != nil { // twice: still one receipt
		t.Fatal(err)
	}
	eventually(t, "handled receipt on a", func() bool {
		tr, _ := a.n.Trace(bg, m.ID)
		for _, k := range tr.Marks {
			if k.Agent == bob && k.State == core.ReceiptHandled {
				return true
			}
		}
		return false
	})
	if got := a.texts(t, room.ID); len(got) != 1 {
		t.Errorf("receipts must not appear as messages: %v", got)
	}
}

func TestOfflineRecipientGetsEverythingOnceInOrder(t *testing.T) {
	net := newNet()
	a, b := net.add(t, "a"), net.add(t, "b")
	room := join(t, a, b)
	alice := a.agent(t, "alice")

	net.setDown(b.n.ID, true)
	var ids []string
	for i := 0; i < 20; i++ {
		ids = append(ids, a.send(t, alice, room.ID, fmt.Sprintf("m%02d", i)).ID)
	}
	time.Sleep(30 * time.Millisecond)
	for _, id := range ids {
		if s := a.state(t, id, b.n.ID); s != store.Queued {
			t.Fatalf("receiver is down but delivery is %q", s)
		}
	}
	net.setDown(b.n.ID, false)
	eventually(t, "all stored", func() bool { return a.state(t, ids[19], b.n.ID) == store.Stored })
	got := b.texts(t, room.ID)
	if len(got) != 20 {
		t.Fatalf("b has %d messages, want 20: %v", len(got), got)
	}
	for i, txt := range got {
		if txt != fmt.Sprintf("m%02d", i) {
			t.Fatalf("out of order at %d: %v", i, got)
		}
	}
}

func TestLostResponseDoesNotDuplicate(t *testing.T) {
	net := newNet()
	a, b := net.add(t, "a"), net.add(t, "b")
	room := join(t, a, b)
	alice := a.agent(t, "alice")
	net.mu.Lock()
	net.dropResp[b.n.ID] = 3
	net.mu.Unlock()
	m := a.send(t, alice, room.ID, "exactly one row please")
	eventually(t, "stored after retries", func() bool { return a.state(t, m.ID, b.n.ID) == store.Stored })
	if got := b.texts(t, room.ID); len(got) != 1 {
		t.Fatalf("b holds %d copies: %v", len(got), got)
	}
}

func TestSenderRestartResumesOutbox(t *testing.T) {
	net := newNet()
	a, b := net.add(t, "a"), net.add(t, "b")
	room := join(t, a, b)
	alice := a.agent(t, "alice")

	net.setDown(b.n.ID, true)
	m := a.send(t, alice, room.ID, "survives a restart")
	id, dir := a.n.ID, a.dir
	a.stop() // sender dies with the message committed but unsent
	net.setDown(b.n.ID, false)
	a2 := net.start(t, "a", id, dir)
	eventually(t, "stored after restart", func() bool { return a2.state(t, m.ID, b.n.ID) == store.Stored })
	if got := b.texts(t, room.ID); len(got) != 1 || got[0] != "survives a restart" {
		t.Fatalf("b has %v", got)
	}
}

func TestReceiverRestartKeepsDedup(t *testing.T) {
	net := newNet()
	a, b := net.add(t, "a"), net.add(t, "b")
	room := join(t, a, b)
	alice := a.agent(t, "alice")
	net.mu.Lock()
	net.dropResp[b.n.ID] = 1 // b commits, a never hears
	net.mu.Unlock()
	m := a.send(t, alice, room.ID, "once across a restart")
	eventually(t, "first attempt", func() bool { net.mu.Lock(); defer net.mu.Unlock(); return net.delivered >= 1 })
	id, dir := b.n.ID, b.dir
	net.setDown(id, true)
	b.stop()
	b2 := net.start(t, "b", id, dir)
	net.setDown(id, false)
	eventually(t, "stored", func() bool { return a.state(t, m.ID, id) == store.Stored })
	if got := b2.texts(t, room.ID); len(got) != 1 {
		t.Fatalf("b holds %d copies", len(got))
	}
}

func TestStrangerAndWrongRoomAreRejected(t *testing.T) {
	net := newNet()
	a, b, stranger := net.add(t, "a"), net.add(t, "b"), net.add(t, "x")
	room := join(t, a, b)
	other, _ := a.n.CreateRoom(bg, "private", a.m.addrs())
	now := time.Now()
	mk := func(from *peer, roomID string) core.Envelope {
		return core.Envelope{V: 1, ID: core.NewID(now), Room: roomID, FromNode: from.n.ID,
			From: core.AgentID("evil", from.n.ID), Kind: core.KindMessage, Seq: 99, CreatedAt: now, Body: core.Body{Text: "hi"}}
	}
	cases := map[string]struct {
		from *peer
		env  core.Envelope
	}{
		"stranger":           {stranger, mk(stranger, room.ID)},
		"member, other room": {b, mk(b, other.ID)},
		"local room":         {b, mk(b, core.LocalRoom)},
		"unknown room":       {b, mk(b, core.NewRoomID())},
	}
	for name, c := range cases {
		res := a.n.Receive(bg, c.from.n.ID, DeliverReq{Envelopes: []core.Envelope{c.env}}, nil)
		if len(res) != 1 || res[0].Status != ResRejected {
			t.Errorf("%s: want rejected, got %+v", name, res)
		}
	}
	// A member cannot speak as another node: identity comes from the connection.
	spoof := mk(a, room.ID)
	res := a.n.Receive(bg, b.n.ID, DeliverReq{Envelopes: []core.Envelope{spoof}}, nil)
	if res[0].Status != ResRejected {
		t.Errorf("spoofed from label accepted: %+v", res)
	}
	if got := a.texts(t, other.ID); len(got) != 0 {
		t.Errorf("private room leaked: %v", got)
	}
}

// Ids are scoped to their author. A member that learns another member's
// message id must not be able to block, replace or falsely confirm it.
func TestIDReuseCannotSuppressOrRewrite(t *testing.T) {
	net := newNet()
	a, b, c := net.add(t, "a"), net.add(t, "b"), net.add(t, "c")
	room := join(t, a, b, c)
	alice := a.agent(t, "alice")

	// c is unreachable for a, so a's message is still on its way to c ...
	net.setDown(c.n.ID, true)
	orig := a.send(t, alice, room.ID, "the real message")
	eventually(t, "b has it", func() bool { return a.state(t, orig.ID, b.n.ID) == store.Stored })
	// ... while b, who has seen the id, gets a message with that id to c first.
	now := time.Now()
	fake := core.Envelope{V: 1, ID: orig.ID, Room: room.ID, FromNode: b.n.ID, From: core.AgentID("mallory", b.n.ID),
		Kind: core.KindMessage, Seq: 1_000_000, CreatedAt: now, Body: core.Body{Text: "an impostor"}}
	if res := c.n.Receive(bg, b.n.ID, DeliverReq{Envelopes: []core.Envelope{fake}}, nil); res[0].Status != ResStored {
		t.Fatalf("b's own message (under its own identity) should store: %+v", res)
	}
	net.setDown(c.n.ID, false)
	eventually(t, "a's real message still reaches c", func() bool { return a.state(t, orig.ID, c.n.ID) == store.Stored })
	got := c.texts(t, room.ID)
	if len(got) != 2 || !slices.Contains(got, "the real message") {
		t.Fatalf("c should hold both, attributed separately: %v", got)
	}
	// Replaying a's message with different content is not a duplicate.
	tampered := orig.Envelope
	tampered.Body.Text = "rewritten"
	if res := c.n.Receive(bg, a.n.ID, DeliverReq{Envelopes: []core.Envelope{tampered}}, nil); res[0].Status != ResRejected {
		t.Errorf("changed content under an existing id must be rejected, got %+v", res)
	}
	// An exact replay is.
	if res := c.n.Receive(bg, a.n.ID, DeliverReq{Envelopes: []core.Envelope{orig.Envelope}}, nil); res[0].Status != ResStored {
		t.Errorf("exact replay should report stored, got %+v", res)
	}
	if got := c.texts(t, room.ID); len(got) != 2 {
		t.Errorf("replays changed history: %v", got)
	}
}

func TestReceiptCannotCrossRoomsOrBeForged(t *testing.T) {
	net := newNet()
	a, b, c := net.add(t, "a"), net.add(t, "b"), net.add(t, "c")
	shared := join(t, a, b, c)
	private, _ := a.n.CreateRoom(bg, "private", a.m.addrs())
	tok, _, _ := a.n.Invite(bg, private.ID, a.m.addrs())
	if _, err := c.m.Join(bg, tok); err != nil {
		t.Fatal(err)
	}
	alice := a.agent(t, "alice")
	secret := a.send(t, alice, private.ID, "b never received this")
	now := time.Now()
	receipt := func(roomID, ref string) core.Envelope {
		return core.Envelope{V: 1, ID: core.NewID(now), Room: roomID, FromNode: b.n.ID, From: core.AgentID("bob", b.n.ID),
			Kind: core.KindReceipt, Seq: int64(now.UnixNano()), CreatedAt: now, Body: core.Body{Ref: ref, State: core.ReceiptHandled}}
	}
	for name, env := range map[string]core.Envelope{
		"message from a room b is not in, via a shared room": receipt(shared.ID, secret.ID),
		"message that does not exist":                        receipt(shared.ID, core.NewID(now)),
	} {
		if res := a.n.Receive(bg, b.n.ID, DeliverReq{Envelopes: []core.Envelope{env}}, nil); res[0].Status != ResRejected {
			t.Errorf("%s: want rejected, got %+v", name, res)
		}
	}
	if tr, _ := a.n.Trace(bg, secret.ID); len(tr.Marks) != 0 {
		t.Errorf("forged receipt left a mark: %+v", tr.Marks)
	}
}

func TestUsedInviteDoesNotReadmitARemovedNode(t *testing.T) {
	net := newNet()
	a, b := net.add(t, "a"), net.add(t, "b")
	room, _ := a.n.CreateRoom(bg, "team", a.m.addrs())
	tok, _, _ := a.n.Invite(bg, room.ID, a.m.addrs())
	if _, err := b.m.Join(bg, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := a.n.RemoveMember(bg, room.ID, b.n.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.m.Join(bg, tok); err == nil {
		t.Fatal("a removed node rejoined with its old invite")
	}
	if r, _ := a.st.Room(bg, room.ID); len(r.Members) != 1 {
		t.Fatalf("room has %d members, want 1", len(r.Members))
	}
}

func TestConcurrentMembershipChangesAllApply(t *testing.T) {
	net := newNet()
	a := net.add(t, "a")
	room, _ := a.n.CreateRoom(bg, "team", a.m.addrs())
	const joiners = 8
	var wg sync.WaitGroup
	for i := 0; i < joiners; i++ {
		p := net.add(t, fmt.Sprintf("j%d", i))
		tok, _, err := a.n.Invite(bg, room.ID, a.m.addrs())
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.m.Join(bg, tok); err != nil {
				t.Errorf("join: %v", err)
			}
			p.m.Refresh(bg, room.ID) // address reports race with other joins
		}()
	}
	wg.Wait()
	if r, _ := a.st.Room(bg, room.ID); len(r.Members) != joiners+1 {
		t.Fatalf("room has %d members, want %d: a concurrent change was lost", len(r.Members), joiners+1)
	}
}

// The owner may send to a new member before the member has heard that its
// join succeeded. That first message must wait, not be rejected.
func TestMessageThatBeatsTheJoinResponseIsNotLost(t *testing.T) {
	net := newNet()
	a, b := net.add(t, "a"), net.add(t, "b")
	room, _ := a.n.CreateRoom(bg, "team", a.m.addrs())
	alice := a.agent(t, "alice")
	tok, _, _ := a.n.Invite(bg, room.ID, a.m.addrs())
	var early store.Message
	net.mu.Lock()
	net.afterJoin = func() { // runs on the owner after admitting b, before b gets the answer
		early = a.send(t, alice, room.ID, "welcome!")
		time.Sleep(30 * time.Millisecond) // long enough for a delivery attempt
	}
	net.mu.Unlock()
	if _, err := b.m.Join(bg, tok); err != nil {
		t.Fatal(err)
	}
	eventually(t, "welcome message stored on b", func() bool { return a.state(t, early.ID, b.n.ID) == store.Stored })
	if got := b.texts(t, room.ID); len(got) != 1 || got[0] != "welcome!" {
		t.Fatalf("b has %v", got)
	}
}

func TestLateRefreshAnswerCannotResurrectARoom(t *testing.T) {
	net := newNet()
	a, b := net.add(t, "a"), net.add(t, "b")
	room := join(t, a, b)
	stale, _ := b.st.Room(bg, room.ID)
	stale.Version += 5 // an answer the owner sent before removing b, arriving late
	if _, err := a.n.RemoveMember(bg, room.ID, b.n.ID); err != nil {
		t.Fatal(err)
	}
	b.m.Refresh(bg, room.ID) // owner refuses: b drops the room
	if applied, err := b.st.PutRoom(bg, stale, false, time.Now()); err != nil || applied {
		t.Fatalf("late answer was applied (%v, %v)", applied, err)
	}
	if _, err := b.st.Room(bg, room.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("room came back: %v", err)
	}
}

func TestGarbledAnswerBacksOffAndKeepsMessageQueued(t *testing.T) {
	net := newNet()
	a, b := net.add(t, "a"), net.add(t, "b")
	room := join(t, a, b)
	alice := a.agent(t, "alice")
	net.mu.Lock()
	net.garble[b.n.ID] = true
	net.delivered = 0
	net.mu.Unlock()
	m := a.send(t, alice, room.ID, "answer me properly")
	time.Sleep(150 * time.Millisecond)
	net.mu.Lock()
	attempts := net.delivered
	net.garble[b.n.ID] = false
	net.mu.Unlock()
	if attempts > 40 { // 2ms base doubling to a 20ms cap: about a dozen, never a hot loop
		t.Fatalf("%d attempts in 150ms: delivery is hot-looping", attempts)
	}
	eventually(t, "stored once answers are sane", func() bool { return a.state(t, m.ID, b.n.ID) == store.Stored })
}

func TestInviteIsSingleUseAndExpires(t *testing.T) {
	net := newNet()
	a, b, c := net.add(t, "a"), net.add(t, "b"), net.add(t, "c")
	room, _ := a.n.CreateRoom(bg, "team", a.m.addrs())
	tok, _, _ := a.n.Invite(bg, room.ID, a.m.addrs())
	if _, err := b.m.Join(bg, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := b.m.Join(bg, tok); err != nil {
		t.Errorf("same node retrying a join must succeed (lost response): %v", err)
	}
	if _, err := c.m.Join(bg, tok); err == nil {
		t.Errorf("second node reused a one-time invite")
	}
	old := InviteTTL
	InviteTTL = -time.Second
	defer func() { InviteTTL = old }()
	expired, _, _ := a.n.Invite(bg, room.ID, a.m.addrs())
	if _, err := c.m.Join(bg, expired); err == nil {
		t.Errorf("expired invite accepted")
	}
	if _, err := a.n.HandleJoin(bg, c.n.ID, JoinReq{Secret: "guess"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("guessing a secret: %v", err)
	}
	r, _ := a.st.Room(bg, room.ID)
	if len(r.Members) != 2 {
		t.Errorf("room has %d members, want 2", len(r.Members))
	}
	if _, _, err := b.n.Invite(bg, room.ID, b.m.addrs()); err == nil {
		t.Errorf("non-owner created an invite")
	}
}

func TestRemovedMemberIsCutOff(t *testing.T) {
	net := newNet()
	a, b, c := net.add(t, "a"), net.add(t, "b"), net.add(t, "c")
	room := join(t, a, b, c)
	alice, bob, carol := a.agent(t, "alice"), b.agent(t, "bob"), c.agent(t, "carol")
	_ = carol

	net.setDown(b.n.ID, true)
	pending := a.send(t, alice, room.ID, "queued for b when b is removed")
	if _, err := a.n.RemoveMember(bg, room.ID, b.n.ID); err != nil {
		t.Fatal(err)
	}
	if s := a.state(t, pending.ID, b.n.ID); s != store.Rejected {
		t.Errorf("queued delivery to a removed node should be rejected, is %q", s)
	}
	net.setDown(b.n.ID, false)
	c.m.Refresh(bg, room.ID)

	// b has not heard yet and still tries to post.
	m := b.send(t, bob, room.ID, "am I still here?")
	eventually(t, "rejected by a and c", func() bool {
		return b.state(t, m.ID, a.n.ID) == store.Rejected && b.state(t, m.ID, c.n.ID) == store.Rejected
	})
	for _, p := range []*peer{a, c} {
		for _, txt := range p.texts(t, room.ID) {
			if txt == "am I still here?" {
				t.Errorf("%s stored a message from a removed node", p.n.Name)
			}
		}
	}
	b.m.Refresh(bg, room.ID)
	if _, err := b.st.Room(bg, room.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("b should drop the room once the owner refuses it: %v", err)
	}
	after := a.send(t, alice, room.ID, "after removal")
	if s := a.state(t, after.ID, b.n.ID); s != "" {
		t.Errorf("new message was queued for a removed node (%q)", s)
	}
}

func TestNewMemberCanTalkBeforeOthersHeard(t *testing.T) {
	net := newNet()
	a, b, c := net.add(t, "a"), net.add(t, "b"), net.add(t, "c")
	room, _ := a.n.CreateRoom(bg, "team", a.m.addrs())
	tok, _, _ := a.n.Invite(bg, room.ID, a.m.addrs())
	if _, err := b.m.Join(bg, tok); err != nil {
		t.Fatal(err)
	}
	tok, _, _ = a.n.Invite(bg, room.ID, a.m.addrs())
	if _, err := c.m.Join(bg, tok); err != nil { // b has not refreshed: it does not know c
		t.Fatal(err)
	}
	carol := c.agent(t, "carol")
	m := c.send(t, carol, room.ID, "hi, I just joined")
	eventually(t, "b accepts c after refreshing", func() bool { return c.state(t, m.ID, b.n.ID) == store.Stored })
	if got := b.texts(t, room.ID); len(got) != 1 {
		t.Fatalf("b has %v", got)
	}
}

func TestLateJoinerGetsNoBackfill(t *testing.T) {
	net := newNet()
	a, b := net.add(t, "a"), net.add(t, "b")
	room, _ := a.n.CreateRoom(bg, "team", a.m.addrs())
	alice := a.agent(t, "alice")
	a.send(t, alice, room.ID, "before b joined")
	tok, _, _ := a.n.Invite(bg, room.ID, a.m.addrs())
	if _, err := b.m.Join(bg, tok); err != nil {
		t.Fatal(err)
	}
	m := a.send(t, alice, room.ID, "after b joined")
	eventually(t, "stored", func() bool { return a.state(t, m.ID, b.n.ID) == store.Stored })
	if got := b.texts(t, room.ID); len(got) != 1 || got[0] != "after b joined" {
		t.Fatalf("b has %v", got)
	}
}

func TestConcurrentSenders(t *testing.T) {
	net := newNet()
	a, b, c := net.add(t, "a"), net.add(t, "b"), net.add(t, "c")
	room := join(t, a, b, c)
	const each = 100
	var wg sync.WaitGroup
	for _, p := range []*peer{a, b} {
		agent := p.agent(t, "w")
		for w := 0; w < 4; w++ { // several local sessions per node
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < each/4; i++ {
					if _, err := p.n.Send(bg, SendReq{From: agent, Room: room.ID, Text: "x"}); err != nil {
						t.Error(err)
					}
				}
			}()
		}
	}
	wg.Wait()
	eventually(t, "c holds all 200", func() bool { return len(c.texts(t, room.ID)) == 2*each })
	msgs, _ := c.n.History(bg, room.ID, 0, 200)
	last, ids := map[string]int64{}, map[string]bool{}
	for _, m := range msgs {
		if ids[m.ID] {
			t.Fatalf("duplicate id %s", m.ID)
		}
		ids[m.ID] = true
		if m.Seq <= last[m.FromNode] {
			t.Fatalf("seq went backwards for %s: %d after %d", core.ShortNode(m.FromNode), m.Seq, last[m.FromNode])
		}
		last[m.FromNode] = m.Seq
	}
	time.Sleep(20 * time.Millisecond)
	if n := len(c.texts(t, room.ID)); n != 2*each {
		t.Fatalf("c holds %d, want %d", n, 2*each)
	}
}

func TestExpiryIsVisible(t *testing.T) {
	net := newNet()
	a, b := net.add(t, "a"), net.add(t, "b")
	room := join(t, a, b)
	alice := a.agent(t, "alice")
	net.setDown(b.n.ID, true)
	m := a.send(t, alice, room.ID, "nobody home")
	time.Sleep(10 * time.Millisecond)
	if s := a.state(t, m.ID, b.n.ID); s != store.Queued {
		t.Fatalf("want queued, got %q", s)
	}
	clockSkew.Store(int64(RetentionExpiry + time.Hour)) // a week passes
	defer clockSkew.Store(0)
	eventually(t, "expired", func() bool { return a.state(t, m.ID, b.n.ID) == store.Expired })
}
