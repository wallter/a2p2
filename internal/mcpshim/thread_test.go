package mcpshim

import (
	"context"
	"net"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wallter/a2p2/internal/daemon"
	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// netDaemon is a daemon with networking on, listening on a loopback port.
type netDaemon struct {
	t    *testing.T
	home string
	c    *localapi.Client
	node string
	done chan error
}

func (d *netDaemon) start() {
	d.t.Helper()
	d.done = make(chan error, 1)
	done, ready := d.done, make(chan struct{})
	go func() {
		done <- daemon.RunWith(context.Background(), d.home, "test", daemon.RunOpts{Ready: func() { close(ready) }})
	}()
	select {
	case <-ready:
	case err := <-done:
		d.t.Fatalf("daemon did not start: %v", err)
	case <-time.After(10 * time.Second):
		d.t.Fatal("daemon did not start")
	}
	d.c = localapi.NewClient(daemon.SockPath(d.home))
	var st localapi.Status
	if err := d.c.Call(context.Background(), "status", nil, &st); err != nil {
		d.t.Fatal(err)
	}
	d.node = st.Node
}

func (d *netDaemon) stop() {
	if d.done == nil {
		return
	}
	_ = d.c.Call(context.Background(), "stop", nil, nil)
	if err := <-d.done; err != nil {
		d.t.Errorf("daemon: %v", err)
	}
	d.done = nil
}

func (d *netDaemon) call(name string, req, resp any) {
	d.t.Helper()
	if err := d.c.Call(context.Background(), name, req, resp); err != nil {
		d.t.Fatalf("%s: %v", name, err)
	}
}

func newNetDaemon(t *testing.T, name string) *netDaemon {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "a2p2")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	if err := daemon.SaveConfig(home, daemon.Config{Name: name, UI: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	d := &netDaemon{t: t, home: home}
	d.start()
	t.Cleanup(d.stop)
	// The daemon applies a networking decision by itself: wait until the
	// decision it reports is this one and it is no longer being applied.
	var res localapi.NetResp
	d.call("net", localapi.NetReq{State: node.NetworkOn, Listen: addr}, &res)
	until(t, "the daemon to apply networking on", func() bool {
		var st localapi.Status
		err := d.c.Call(context.Background(), "status", localapi.Empty{}, &st)
		return err == nil && st.Network.Request == res.Request && st.Network.Reason != "being applied" &&
			st.Network.State == node.NetworkOn
	})
	return d
}

// pairDaemons makes room on a and takes b into it.
func pairDaemons(t *testing.T, room string) (*netDaemon, *netDaemon) {
	t.Helper()
	a, b := newNetDaemon(t, "alpha"), newNetDaemon(t, "beta")
	var r localapi.Room
	a.call("room/create", localapi.RoomReq{Room: room}, &r)
	var tok localapi.InviteResp
	a.call("room/invite", localapi.InviteReq{Room: room}, &tok)
	var pv localapi.PreviewResp
	b.call("room/join", localapi.JoinReq{Token: tok.Token}, &pv)
	var acc localapi.AcceptResp
	b.call("room/join/accept", localapi.AcceptReq{Token: tok.Token, Preview: pv.Preview}, &acc)
	var qs localapi.RequestsResp
	a.call("requests", nil, &qs)
	i := slices.IndexFunc(qs.Requests, func(q localapi.Request) bool { return q.Node == b.node })
	if i < 0 {
		t.Fatalf("requests = %+v", qs)
	}
	var cf localapi.ConfirmResp
	a.call("confirm", localapi.ConfirmReq{ID: qs.Requests[i].ID, Approve: true}, &cf)
	if cf.State != store.RequestApproved {
		t.Fatalf("confirm = %+v", cf)
	}
	until(t, "the joiner to hold the room", func() bool {
		var rl localapi.RoomsResp
		err := b.c.Call(context.Background(), "room/list", localapi.RoomListReq{Room: room}, &rl)
		return err == nil && len(rl.Rooms) == 1 && len(rl.Rooms[0].Members) == 2
	})
	return a, b
}

func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(60 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitFor reads cs's inbox until a message with text arrives.
func waitFor(t *testing.T, cs *mcp.ClientSession, text string) localapi.Entry {
	t.Helper()
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); {
		for _, e := range parseEntries(t, ok(t, cs, "inbox", map[string]any{"wait_seconds": 5})) {
			if e.Text == text {
				return e
			}
		}
	}
	t.Fatalf("%q never arrived", text)
	return localapi.Entry{}
}

// Item 6, second form: by default the reply tool addresses the author
// answered and everyone who has posted in the thread, so three agents on
// two machines each reach the other two; an explicit to narrows it.
func TestReplyReachesEveryoneWhoPostedInTheThreadAcrossMachines(t *testing.T) {
	if testing.Short() {
		t.Skip("starts daemons")
	}
	a, b := pairDaemons(t, "team")
	lead, bob, carol := connect(t, a.c, "lead"), connect(t, b.c, "bob"), connect(t, b.c, "carol")
	leadID, bobID, carolID := agentID(t, lead), agentID(t, bob), agentID(t, carol)

	kick := idOf(t, ok(t, lead, "send", map[string]any{"text": "kickoff", "room": "team"}))
	waitFor(t, bob, "kickoff")
	waitFor(t, carol, "kickoff")

	r := parse(t, ok(t, bob, "reply", map[string]any{"message_id": kick, "text": "bob finds"}))
	if !slices.Equal(r.To, []string{leadID}) {
		t.Fatalf("first reply = %+v", r) // only the lead has posted so far
	}
	waitFor(t, lead, "bob finds")

	r = parse(t, ok(t, carol, "reply", map[string]any{"message_id": kick, "text": "carol answers"}))
	if len(r.To) != 2 || !slices.Contains(r.To, leadID) || !slices.Contains(r.To, bobID) {
		t.Fatalf("carol's reply = %+v", r)
	}
	waitFor(t, lead, "carol answers")
	waitFor(t, bob, "carol answers")

	r = parse(t, ok(t, lead, "reply", map[string]any{"message_id": r.ID, "text": "lead wraps up"}))
	if len(r.To) != 2 || !slices.Contains(r.To, carolID) || !slices.Contains(r.To, bobID) {
		t.Fatalf("lead's reply = %+v", r)
	}
	waitFor(t, bob, "lead wraps up")
	waitFor(t, carol, "lead wraps up")

	// An explicit to narrows it.
	r = parse(t, ok(t, bob, "reply", map[string]any{"message_id": kick, "text": "just lead", "to": []string{leadID}}))
	if !slices.Equal(r.To, []string{leadID}) {
		t.Fatalf("narrowed = %+v", r)
	}
	waitFor(t, lead, "just lead")
	if es := parseEntries(t, ok(t, carol, "inbox", nil)); slices.ContainsFunc(es, func(e localapi.Entry) bool { return e.Text == "just lead" }) {
		t.Fatalf("carol got a reply not addressed to her: %+v", es)
	}
}

// A record written with reply_to and no to (send, or the CLI's send
// --reply-to) keeps its meaning: the parent's author only.
func TestSendWithReplyToAndNoToStillAddressesOnlyTheParentsAuthor(t *testing.T) {
	c := startDaemon(t)
	a, b, k := connect(t, c, "alice"), connect(t, c, "bob"), connect(t, c, "carol")
	alice, _, carol := agentID(t, a), agentID(t, b), agentID(t, k)
	kick := idOf(t, ok(t, a, "send", map[string]any{"text": "kickoff"}))
	ok(t, k, "reply", map[string]any{"message_id": kick, "text": "carol was here"})
	r := parse(t, ok(t, b, "send", map[string]any{"text": "plain reply", "reply_to": kick}))
	if !slices.Equal(r.To, []string{alice}) {
		t.Fatalf("send reply_to = %+v", r)
	}
	r = parse(t, ok(t, b, "reply", map[string]any{"message_id": kick, "text": "tool reply"}))
	if len(r.To) != 2 || !slices.Contains(r.To, alice) || !slices.Contains(r.To, carol) {
		t.Fatalf("reply tool = %+v", r)
	}
	var sr localapi.SendResp
	if err := c.Call(context.Background(), "send", localapi.SendReq{Agent: alice, Text: "cli", ReplyTo: kick}, &sr); err != nil ||
		len(sr.To) != 1 || !strings.HasPrefix(sr.To[0], "alice@") {
		t.Fatalf("local send reply_to = %+v %v", sr, err) // the author answering itself: as before
	}
}
