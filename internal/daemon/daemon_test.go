package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
	_ "modernc.org/sqlite"
)

// stableListener is one real loopback listener held for the whole test,
// handed to each run of a daemon as a view (RunOpts.PeerListener), so that
// a restart is found again at the address its peers know without releasing
// and rebinding a port. It counts the connections other machines made.
type stableListener struct {
	ln       net.Listener
	accepted atomic.Int64
	mu       sync.Mutex
	cur      *listenView
}

type listenView struct {
	s    *stableListener
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func newStableListener(t *testing.T) *stableListener {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &stableListener{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.accepted.Add(1)
			s.mu.Lock()
			v := s.cur
			s.mu.Unlock()
			if v == nil {
				c.Close() // no daemon listens now
				continue
			}
			select {
			case v.ch <- c:
			case <-v.done:
				c.Close()
			}
		}
	}()
	return s
}

func (s *stableListener) view() *listenView {
	v := &listenView{s: s, ch: make(chan net.Conn), done: make(chan struct{})}
	s.mu.Lock()
	s.cur = v
	s.mu.Unlock()
	return v
}

func (v *listenView) Accept() (net.Conn, error) {
	select {
	case c := <-v.ch:
		return c, nil
	case <-v.done:
		return nil, net.ErrClosed
	}
}

func (v *listenView) Close() error {
	v.once.Do(func() {
		close(v.done)
		v.s.mu.Lock()
		if v.s.cur == v {
			v.s.cur = nil
		}
		v.s.mu.Unlock()
	})
	return nil
}

func (v *listenView) Addr() net.Addr { return v.s.ln.Addr() }

// machine is a real daemon in a temporary home, listening on loopback
// through a stable listener.
type machine struct {
	t    *testing.T
	home string
	ln   *stableListener
	c    *localapi.Client
	done chan error
	node string

	realBind bool // listen on the decided address, not through ln

	mu        sync.Mutex
	xx        *node.Exchange // the current run's pull exchange
	onHandler func(path string, enter bool)
}

// newMachine starts a machine with networking on, decided through the
// local API's net call as a2p2 net on does.
func newMachine(t *testing.T, name string) *machine {
	m := newMachineWith(t, name, nil)
	m.setNet(node.NetworkOn)
	return m
}

// newMachineWith runs prepare on the home before the first start; the
// machine starts with networking off, as a new one does.
func newMachineWith(t *testing.T, name string, prepare func(home string)) *machine {
	t.Helper()
	return newMachineOpts(t, name, prepare, false)
}

func newMachineOpts(t *testing.T, name string, prepare func(home string), realBind bool) *machine {
	t.Helper()
	home, err := os.MkdirTemp(sockTemp(), "a2p2")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	if err := SaveConfig(home, Config{Name: name, UI: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	if prepare != nil {
		prepare(home)
	}
	m := &machine{t: t, home: home, ln: newStableListener(t), realBind: realBind}
	m.start()
	t.Cleanup(m.stop)
	return m
}

// start runs the daemon and waits for its readiness barrier.
func (m *machine) start() {
	m.t.Helper()
	m.done = make(chan error, 1)
	done, ready := m.done, make(chan struct{})
	go func() {
		var listen func(string) (net.Listener, error) // a real bind on the decided address
		if !m.realBind {
			listen = func(string) (net.Listener, error) { return m.ln.view(), nil }
		}
		done <- RunWith(context.Background(), m.home, "test", RunOpts{
			Listen: listen, Ready: func() { close(ready) },
			Exchange: func(x *node.Exchange) { m.setX(x) }, Handler: m.hook})
	}()
	select {
	case <-ready:
	case err := <-done:
		m.done = nil
		m.t.Fatalf("daemon did not start: %v", err)
	case <-time.After(10 * time.Second):
		m.t.Fatal("daemon did not start")
	}
	m.c = Client(m.home)
	m.node = call[localapi.Status](m, "status", localapi.Empty{}).Node
}

// setNet records a networking decision through the local API's net call,
// as a2p2 net does, and waits for the daemon to apply it by itself.
func (m *machine) setNet(state string) localapi.Status {
	m.t.Helper()
	req := localapi.NetReq{State: state}
	if state == node.NetworkOn {
		req.Listen = m.ln.ln.Addr().String()
	}
	return m.decide(req)
}

func (m *machine) decide(req localapi.NetReq) localapi.Status {
	m.t.Helper()
	res := call[localapi.NetResp](m, "net", req)
	if res.State != req.State || res.Request == "" {
		m.t.Fatalf("net %s = %+v", req.State, res)
	}
	var st localapi.Status
	eventually(m.t, "the daemon to apply net "+req.State, func() bool {
		st = localapi.Status{}
		err := m.c.Call(context.Background(), "status", localapi.Empty{}, &st)
		return err == nil && st.Network.Request == res.Request && st.Network.Reason != "being applied"
	})
	if st.Network.State != req.State {
		m.t.Fatalf("after net %s: %+v", req.State, st.Network)
	}
	return st
}

func (m *machine) setX(x *node.Exchange) {
	m.mu.Lock()
	m.xx = x
	m.mu.Unlock()
}

// exchange is the pull exchange of the daemon's current run.
func (m *machine) exchange() *node.Exchange {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.xx
}

// hook passes the daemon's handler entries and exits to onHandler.
func (m *machine) hook(path string, enter bool) {
	m.mu.Lock()
	f := m.onHandler
	m.mu.Unlock()
	if f != nil {
		f(path, enter)
	}
}

// ui calls the page's API of m with its token.
func (m *machine) ui(method, path string, body any, out any) int {
	m.t.Helper()
	st := call[localapi.Status](m, "status", localapi.Empty{})
	u, err := url.Parse(st.UI)
	if err != nil {
		m.t.Fatal(err)
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, "http://"+u.Host+path, rd)
	req.Header.Set("Authorization", "Bearer "+u.Fragment)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://"+u.Host)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		m.t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil {
		_ = json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode
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
	ann := a.agent("ann")
	b.agent("bea") // bea registers before the restart, and again after it
	// The joiner restarts between its approval and the admin's: the join resumes.
	join(t, a, b, room, localapi.InviteReq{}, true)
	bea := b.agent("bea")

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
	// The page's API, as the browser fetches it (no browser runs the script).
	for _, path := range []string{"/api/state", "/api/history?room=" + room, "/api/thread?id=" + before.ID, "/api/trace?id=" + before.ID} {
		var raw json.RawMessage
		c.ui("GET", path, nil, &raw)
		if strings.Contains(string(raw), "SECRET") {
			t.Fatalf("page API %s showed earlier text: %s", path, raw)
		}
	}
}

// loadV02 writes the schema-version-1 fixture (a database as v0.2 left
// it) into home.
func loadV02(t *testing.T, home string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "store", "testdata", "v02-schema1.sql"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(home, "a2p2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(b)); err != nil {
		t.Fatal(err)
	}
}

func TestV02NoticeFromASchemaOneFileIsShownOnceEverThroughStatus(t *testing.T) {
	if st := call[localapi.Status](newMachineWith(t, "fresh", nil), "status", localapi.StatusReq{ShowNotice: true}); st.Notice != "" {
		t.Fatalf("notice on a new home: %q", st.Notice)
	}
	logs := captureLog(t)
	m := newMachineWith(t, "old", func(home string) { loadV02(t, home) })
	// An agent's status (whoami, peers) does not carry it. A person's
	// status and the page's state return it, across a restart, until the
	// CLI or the page reports that it printed or drew it.
	if st := call[localapi.Status](m, "status", localapi.Empty{}); st.Notice != "" {
		t.Fatalf("agent status: %q", st.Notice)
	}
	var page struct {
		Status localapi.Status `json:"status"`
	}
	for i := 0; i < 2; i++ {
		st := call[localapi.Status](m, "status", localapi.StatusReq{ShowNotice: true})
		if st.Notice != node.NoticeV02 || len(st.Rooms) != 1 || st.Rooms[0].Label != node.LocalLabel {
			t.Fatalf("not yet shown %d: notice %q rooms %+v", i, st.Notice, st.Rooms)
		}
		if code := m.ui("GET", "/api/state", nil, &page); code != 200 || page.Status.Notice != node.NoticeV02 {
			t.Fatalf("page state %d: %d %q", i, code, page.Status.Notice)
		}
		m.stop()
		m.start()
	}
	if code := m.ui("POST", "/api/notice-shown", map[string]any{}, nil); code != 200 {
		t.Fatalf("notice-shown: %d", code)
	}
	if st := call[localapi.Status](m, "status", localapi.StatusReq{ShowNotice: true}); st.Notice != "" {
		t.Fatalf("shown twice: %q", st.Notice)
	}
	if strings.Contains(logs.String(), "removed the messages") {
		t.Fatal("the notice is written to the daemon's log")
	}
	if h := call[localapi.Entries](m, "history", localapi.HistoryReq{}); len(h.Entries) != 0 {
		t.Fatalf("v0.2 messages shown: %+v", h)
	}
	m.stop()
	m.start()
	if st := call[localapi.Status](m, "status", localapi.StatusReq{ShowNotice: true}); st.Notice != "" {
		t.Fatalf("after a restart: notice %q", st.Notice)
	}
	// The v0.2 data is gone from the file itself, the agent stays.
	m.stop()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(m.home, "a2p2.db"))
	if err != nil {
		t.Fatal(err)
	}
	var v02, agents int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM sqlite_master WHERE name IN ('messages', 'members', 'deliveries', 'marks', 'invites')) +
		(SELECT COUNT(*) FROM rooms WHERE id = 'r_v02shared'), (SELECT COUNT(*) FROM agents WHERE id = 'claude-x@abababab')`).Scan(&v02, &agents); err != nil || v02 != 0 || agents != 1 {
		t.Fatalf("v0.2 tables and rooms left %d, agents %d: %v", v02, agents, err)
	}
	db.Close()
	m.start() // and the daemon starts over the migrated file
}

func TestDaemonServesOnlyTheNewPath(t *testing.T) {
	m := newMachineWith(t, "solo", nil)
	st := call[localapi.Status](m, "status", localapi.Empty{})
	if len(st.Rooms) != 1 || st.Rooms[0].Label != "local" || st.Rooms[0].ID == "local" || len(st.Rooms[0].Admins) != 1 || st.Rooms[0].Admins[0] != st.Node {
		t.Fatalf("rooms on an empty home = %+v", st.Rooms)
	}
	if st.Rooms[0].Members[0].Name != "solo" {
		t.Fatalf("room list without this machine's own name: %+v", st.Rooms[0].Members)
	}
}

func TestNetworkingIsOffOnANewHomeAndAConfigListenDoesNotTurnItOn(t *testing.T) {
	m := newMachineWith(t, "solo", func(home string) {
		if err := SaveConfig(home, Config{Name: "solo", UI: "127.0.0.1:0", Listen: "127.0.0.1:0"}); err != nil {
			t.Fatal(err)
		}
	})
	st := call[localapi.Status](m, "status", localapi.Empty{})
	if st.Network.State != node.NetworkOff || len(st.Listen) != 0 || st.Network.Request != "" {
		t.Fatalf("new home: %+v listen %v", st.Network, st.Listen)
	}
	if m.ln.accepted.Load() != 0 {
		t.Fatal("something reached a machine with networking off")
	}
}

// TestNetworkStatesAfterARestart turns a joined machine's networking off,
// outbound only and on, restarting each time. Off: no listener, and it
// reaches nobody (its admin's listener counts no connection from it) and
// refuses calls that would dial. Outbound: still no listener, but it pulls
// from its admin. On: a listener again.
func TestNetworkStatesAfterARestart(t *testing.T) {
	if testing.Short() {
		t.Skip("starts daemons")
	}
	a, b := newMachine(t, "alpha"), newMachine(t, "beta")
	room := call[localapi.Room](a, "room/create", localapi.RoomReq{Room: "team"}).ID
	ann := a.agent("ann")
	join(t, a, b, room, localapi.InviteReq{}, false)
	bea := b.agent("bea")
	tok := call[localapi.InviteResp](a, "room/invite", localapi.InviteReq{Room: room}).Token
	// A join accepted and waiting for a's person: join polling, had it
	// run while off, would dial a.
	room2 := call[localapi.Room](a, "room/create", localapi.RoomReq{Room: "second"}).ID
	tok2 := call[localapi.InviteResp](a, "room/invite", localapi.InviteReq{Room: room2}).Token
	pv2 := call[localapi.PreviewResp](b, "room/join", localapi.JoinReq{Token: tok2})
	if acc := call[localapi.AcceptResp](b, "room/join/accept", localapi.AcceptReq{Token: tok2, Preview: pv2.Preview}); acc.State != node.JoinStateAccepted {
		t.Fatalf("seeded join: %+v", acc)
	}

	st := b.setNet(node.NetworkOff)
	if len(st.Listen) != 0 {
		t.Fatalf("off listens: %+v", st.Listen)
	}
	heard := a.ln.accepted.Load()
	msg := call[localapi.SendResp](a, "send", localapi.SendReq{Agent: ann, Room: room, Text: "while off", To: []string{bea}})
	var ue *localapi.Error
	for _, c := range []struct {
		name string
		req  any
	}{
		{"room/join", localapi.JoinReq{Token: tok}},
		{"room/join/accept", localapi.AcceptReq{Token: tok, Preview: "pv"}},
	} {
		if err := b.c.Call(context.Background(), c.name, c.req, nil); !errors.As(err, &ue) || !ue.User || !strings.Contains(err.Error(), "networking is off") {
			t.Fatalf("%s with networking off: %v", c.name, err)
		}
	}
	// Long enough for the scheduler's first pull and a join poll, had they run.
	time.Sleep(3 * time.Second)
	if n := a.ln.accepted.Load(); n != heard {
		t.Fatalf("networking off, yet %d connection(s) reached the admin machine", n-heard)
	}
	if in := call[localapi.InboxResp](b, "inbox", localapi.InboxReq{Agent: bea}); len(in.Entries) != 0 {
		t.Fatalf("networking off, yet mail arrived: %+v", in.Entries)
	}

	st = b.setNet(node.NetworkOutbound)
	if len(st.Listen) != 0 {
		t.Fatalf("outbound listens: %+v", st.Listen)
	}
	if es := inboxWait(b, bea); len(es) != 1 || es[0].ID != msg.ID {
		t.Fatalf("outbound pulled: %+v", es)
	}
	if a.ln.accepted.Load() == heard {
		t.Fatal("outbound reached nobody")
	}
	var inv localapi.InviteResp
	if err := b.c.Call(context.Background(), "room/invite", localapi.InviteReq{Room: room}, &inv); !errors.As(err, &ue) || !ue.User {
		t.Fatalf("invite without a listener: %v", err)
	}

	st = b.setNet(node.NetworkOn)
	if len(st.Listen) != 1 || st.Listen[0] != b.ln.ln.Addr().String() {
		t.Fatalf("on: listen %v", st.Listen)
	}
	// Every decision is a decided request of kind network.
	var nets int
	err := b.c.Call(context.Background(), "status", nil, &st)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(b.home, "a2p2.db"))
	if err != nil {
		t.Fatal(err)
	}
	qs, err := db.Requests(context.Background(), "", store.RequestApproved)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range qs {
		if q.Kind == store.RequestNetwork {
			nets++
		}
	}
	if nets != 4 { // on (newMachine), off, outbound, on
		t.Fatalf("%d network decisions recorded: %+v", nets, qs)
	}
}

// TestStopDuringAWaitingCallEndsItBeforeTheStoreCloses stops a daemon while
// a waiting inbox read is held: the read ends (the request's context is
// cancelled), the daemon returns, and no handler outlives it.
func TestStopDuringAWaitingCallEndsItBeforeTheStoreCloses(t *testing.T) {
	m := newMachineWith(t, "solo", nil)
	ann := m.agent("ann")
	entered, left := make(chan struct{}), make(chan struct{})
	m.mu.Lock()
	m.onHandler = func(path string, enter bool) {
		if path != "/v1/inbox" {
			return
		}
		if enter {
			close(entered)
		} else {
			close(left)
		}
	}
	m.mu.Unlock()
	res := make(chan error, 1)
	go func() {
		res <- m.c.Call(context.Background(), "inbox", localapi.InboxReq{Agent: ann, WaitMS: 100000}, &localapi.InboxResp{})
	}()
	select {
	case <-entered: // the read is in its handler
	case <-time.After(10 * time.Second):
		t.Fatal("the read did not reach its handler")
	}
	start := time.Now()
	m.stop() // returns once the daemon has returned, its store closed
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("stop took %v with a waiting read", d)
	}
	select {
	case <-left:
	default:
		t.Fatal("the daemon returned while the waiting read was still in its handler")
	}
	<-res
}

func TestPageDecisionNeedsTheExactOriginAndShowsTheFullFingerprint(t *testing.T) {
	if testing.Short() {
		t.Skip("starts daemons")
	}
	a, b := newMachine(t, "alpha"), newMachine(t, "beta")
	room := call[localapi.Room](a, "room/create", localapi.RoomReq{Room: "team"}).ID
	tok := call[localapi.InviteResp](a, "room/invite", localapi.InviteReq{Room: room, Role: node.RoleAdmin, History: true})
	pv := call[localapi.PreviewResp](b, "room/join", localapi.JoinReq{Token: tok.Token})
	call[localapi.AcceptResp](b, "room/join/accept", localapi.AcceptReq{Token: tok.Token, Preview: pv.Preview})
	var state struct {
		Requests []localapi.Request `json:"requests"`
	}
	if code := a.ui("GET", "/api/state", nil, &state); code != 200 || len(state.Requests) != 1 {
		t.Fatalf("state %d %+v", code, state)
	}
	q := state.Requests[0]
	fp := node.Fingerprint(b.node)
	if q.Node != b.node || q.Fingerprint != fp || q.Action != node.ActionAdmitAdmin || !q.History || q.RoomLabel != "team" ||
		q.Text != "add machine "+fp+" (beta) to room team ("+room+") as an admin, with earlier history" {
		t.Fatalf("request as the page shows it: %+v", q)
	}
	// Without the page's Origin, or with another, nothing is decided.
	st := call[localapi.Status](a, "status", localapi.Empty{})
	u, _ := url.Parse(st.UI)
	for _, origin := range []string{"", "http://evil.example"} {
		body := strings.NewReader(`{"id":"` + q.ID + `","approve":true,"text":` + jsonString(q.Text) + `}`)
		req, _ := http.NewRequest("POST", "http://"+u.Host+"/api/confirm", body)
		req.Header.Set("Authorization", "Bearer "+u.Fragment)
		req.Header.Set("Content-Type", "application/json")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode == 200 {
			t.Fatalf("decided with Origin %q", origin)
		}
	}
	if qs := call[localapi.RequestsResp](a, "requests", localapi.Empty{}); len(qs.Requests) != 1 {
		t.Fatalf("decided without the page: %+v", qs)
	}
	var cf localapi.ConfirmResp
	if code := a.ui("POST", "/api/confirm", map[string]any{"id": q.ID, "approve": true, "text": q.Text}, &cf); code != 200 || cf.State != store.RequestApproved {
		t.Fatalf("page decision: %d %+v", code, cf)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestStopReleasesAHeldPullAtOnce stops a daemon while a peer's waiting
// pull is held at it (up to a minute otherwise): peer.Serve cancels the
// hold as the daemon's context ends, so the daemon exits well inside the
// listener's 5-second grace period.
func TestStopReleasesAHeldPullAtOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("starts daemons")
	}
	a, b := newMachine(t, "alpha"), newMachine(t, "beta")
	room := call[localapi.Room](a, "room/create", localapi.RoomReq{Room: "team"}).ID
	ann := a.agent("ann")
	join(t, a, b, room, localapi.InviteReq{}, false)
	bea := b.agent("bea")
	held := a.exchange().NextHold(room, b.node)
	// The message wakes any hold already there; the pull that follows it
	// is the one held.
	call[localapi.SendResp](a, "send", localapi.SendReq{Agent: ann, Room: room, Text: "ping", To: []string{bea}})
	inboxWait(b, bea)
	select {
	case <-held:
	case <-time.After(90 * time.Second):
		t.Fatal("no waiting pull from the peer was held")
	}
	start := time.Now()
	a.stop()
	d := time.Since(start)
	t.Logf("stopped in %v with a held pull", d)
	if d > 2*time.Second {
		t.Fatalf("stop took %v with a held pull", d)
	}
}

// TestAMachineThatDoesNotListenReachesItsPeerAfterARestartFromStoredAddresses:
// the daemon loads the address book from the database. A machine with
// networking outbound (no listener) restarts with no join record left to
// relearn its admin's address from; it still pulls from the admin, at the
// address the book stored.
func TestAMachineThatDoesNotListenReachesItsPeerAfterARestartFromStoredAddresses(t *testing.T) {
	if testing.Short() {
		t.Skip("starts daemons")
	}
	a, b := newMachine(t, "alpha"), newMachine(t, "beta")
	room := call[localapi.Room](a, "room/create", localapi.RoomReq{Room: "team"}).ID
	ann := a.agent("ann")
	join(t, a, b, room, localapi.InviteReq{}, false)
	bea := b.agent("bea")
	b.setNet(node.NetworkOutbound)
	b.stop()
	db, err := store.Open(filepath.Join(b.home, "a2p2.db"))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := db.PeerAddrs(context.Background())
	db.Close()
	if err != nil || !slices.ContainsFunc(stored, func(p store.PeerAddr) bool { return p.Node == a.node && p.Addr == a.ln.ln.Addr().String() }) {
		t.Fatalf("stored addresses: %+v %v", stored, err)
	}
	raw, err := sql.Open("sqlite", filepath.Join(b.home, "a2p2.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("DELETE FROM joins"); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	b.start()
	msg := call[localapi.SendResp](a, "send", localapi.SendReq{Agent: ann, Room: room, Text: "after the restart", To: []string{bea}})
	if es := inboxWait(b, bea); len(es) != 1 || es[0].ID != msg.ID {
		t.Fatalf("after the restart: %+v", es)
	}
}

// lockedBuffer is a log destination safe for the daemon's goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// captureLog sends the daemon's log to a buffer for the rest of the test.
func captureLog(t *testing.T) *lockedBuffer {
	buf := &lockedBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}
