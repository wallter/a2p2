package web

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

const tok = "test-token-123"

// joinNet carries a join request straight to the admin's join server.
type joinNet struct {
	from  string
	admin **node.JoinServer
	id    *string
}

func (n joinNet) Join(ctx context.Context, _ []string, check func(string) error, body []byte) (node.JoinReply, error) {
	if *n.admin == nil {
		return node.JoinReply{}, errors.New("unreachable")
	}
	if err := check(*n.id); err != nil {
		return node.JoinReply{}, err
	}
	return (*n.admin).Handle(ctx, n.from, "127.0.0.1", body), nil
}

type env struct {
	srv  *httptest.Server
	s    *node.Surface
	js   *node.JoinServer
	id   string
	ctx  context.Context
	peer *env
	st   *store.Store
	seed byte
}

func open(t *testing.T, seed byte, peer *env) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "a2p2.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return openOn(t, st, seed, peer)
}

// restart opens e's machine again over its store, as a restarted daemon
// does: no room has been checked with another member since.
func (e *env) restart(t *testing.T) *env { return openOn(t, e.st, e.seed, e.peer) }

func openOn(t *testing.T, st *store.Store, seed byte, peer *env) *env {
	t.Helper()
	ctx := context.Background()
	m := node.NewMachine(st, ed25519.NewKeyFromSeed(append([]byte{seed}, make([]byte, 31)...)), node.MachineOpts{})
	rooms, err := node.OpenRooms(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{ctx: ctx, id: m.ID, js: node.NewJoinServer(rooms), peer: peer, st: st, seed: seed}
	var j *node.Joiner
	if peer != nil {
		j = node.NewJoiner(rooms, joinNet{from: m.ID, admin: &peer.js, id: &peer.id}, node.JoinerOpts{Name: "joiner"})
	}
	e.s, err = node.OpenSurface(ctx, m, rooms, node.SurfaceOpts{Name: "box", Joiner: j,
		Addrs: func() []string { return []string{"127.0.0.1:7443"} }, Network: node.Network{State: node.NetworkOn, Listen: "127.0.0.1:7443"}})
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(Handler(e.s, tok))
	t.Cleanup(e.srv.Close)
	return e
}

func setup(t *testing.T) *env { return open(t, 1, nil) }

// setupFiles is one machine with attachments, a room "team" and an agent
// alice, as the daemon assembles them (node.NewFiles before OpenSurface).
func setupFiles(t *testing.T) (*env, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "a2p2.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := node.NewMachine(st, ed25519.NewKeyFromSeed(append([]byte{9}, make([]byte, 31)...)), node.MachineOpts{})
	rooms, err := node.OpenRooms(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	files := node.NewFiles(rooms, nil, node.FilesOpts{})
	if err := files.Start(ctx); err != nil {
		t.Fatal(err)
	}
	e := &env{ctx: ctx, id: m.ID, st: st, seed: 9}
	if e.s, err = node.OpenSurface(ctx, m, rooms, node.SurfaceOpts{Name: "box"}); err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(Handler(e.s, tok))
	t.Cleanup(e.srv.Close)
	if _, err := e.s.CreateRoom(ctx, "team"); err != nil {
		t.Fatal(err)
	}
	return e, e.agent(t, "alice")
}

// sendFile has alice send body as a file named name to room team and
// returns the request the person is asked to decide, as the page loads it.
func (e *env) sendFile(t *testing.T, alice, name, body string) localapi.Request {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SendFiles(e.ctx, node.SendInput{Agent: alice, Room: "team", Text: "the log\nmore"}, []string{p}); err != nil {
		t.Fatal(err)
	}
	st := e.state(t)
	if len(st.Requests) != 1 {
		t.Fatalf("requests on the page: %+v", st.Requests)
	}
	return st.Requests[0]
}

// approve decides q from the page and returns the message written.
func (e *env) approve(t *testing.T, q localapi.Request) string {
	t.Helper()
	c, b := e.do(t, "POST", "/api/confirm", map[string]any{"id": q.ID, "approve": true, "text": q.Text, "terms": q.Terms}, origin)
	var cf localapi.ConfirmResp
	if c != 200 || json.Unmarshal(b, &cf) != nil || cf.State != store.RequestApproved || cf.Effect == "" {
		t.Fatalf("approve: %d %s", c, b)
	}
	return cf.Effect
}

// shown loads the room's history as the page does to render it: the
// reading path that records the messages as shown to the person without
// marking them seen. On the author's machine the person's id may fetch a
// file only after that (DESIGN 12, fix-files-b 6).
func (e *env) shown(t *testing.T) {
	t.Helper()
	if c, b := e.do(t, "GET", "/api/history?room=team", nil, nil); c != 200 {
		t.Fatalf("history: %d %s", c, b)
	}
}

func shaOf(body string) string {
	h := sha256.Sum256([]byte(body))
	return hex.EncodeToString(h[:])
}

func (e *env) do(t *testing.T, method, path string, body any, mod func(*http.Request)) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if mod != nil {
		mod(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (e *env) agent(t *testing.T, label string) string {
	t.Helper()
	a, err := e.s.Register(e.ctx, "test", label, "")
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
}

type pageState struct {
	Status   localapi.Status    `json:"status"`
	Unread   []localapi.Entry   `json:"unread"`
	Alerts   []localapi.Alert   `json:"alerts"`
	Requests []localapi.Request `json:"requests"`
}

func (e *env) state(t *testing.T) pageState {
	t.Helper()
	c, b := e.do(t, "GET", "/api/state", nil, nil)
	var st pageState
	if c != 200 || json.Unmarshal(b, &st) != nil {
		t.Fatalf("state: %d %s", c, b)
	}
	return st
}

func TestStaticAssetsAndHeaders(t *testing.T) {
	e := setup(t)
	for _, p := range []string{"/", "/app.js", "/style.css"} {
		resp, err := http.Get(e.srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || len(b) == 0 || strings.Contains(string(b), tok) {
			t.Fatalf("%s: status %d len %d", p, resp.StatusCode, len(b))
		}
		for k, v := range map[string]string{"Content-Security-Policy": "default-src 'self'; frame-ancestors 'none'",
			"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "Cache-Control": "no-store"} {
			if resp.Header.Get(k) != v {
				t.Errorf("%s %s = %q", p, k, resp.Header.Get(k))
			}
		}
		if resp.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Error("CORS header emitted")
		}
	}
}

func TestAuthHostOrigin(t *testing.T) {
	e := setup(t)
	if c, _ := e.do(t, "GET", "/api/state", nil, func(r *http.Request) { r.Header.Del("Authorization") }); c != 401 {
		t.Errorf("no token: %d", c)
	}
	if c, _ := e.do(t, "GET", "/api/state", nil, func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") }); c != 401 {
		t.Errorf("wrong token: %d", c)
	}
	if c, _ := e.do(t, "GET", "/api/state", nil, func(r *http.Request) { r.Host = "evil.example:80" }); c != 403 {
		t.Errorf("bad host: %d", c)
	}
	if c, _ := e.do(t, "GET", "/", nil, func(r *http.Request) { r.Host = "evil.example" }); c != 403 {
		t.Errorf("bad host on static: %d", c)
	}
	if c, _ := e.do(t, "GET", "/api/state", nil, func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }); c != 403 {
		t.Errorf("bad origin: %d", c)
	}
	if c, _ := e.do(t, "GET", "/api/state", nil, func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) }); c != 200 {
		t.Errorf("matching origin: %d", c)
	}
	if c, _ := e.do(t, "GET", "/api/state", nil, func(r *http.Request) { r.Host = "localhost:1234" }); c != 200 {
		t.Errorf("localhost host: %d", c)
	}
	if c, _ := e.do(t, "POST", "/api/confirm", map[string]any{"id": "x", "approve": true}, func(r *http.Request) { r.Header.Del("Authorization") }); c != 401 {
		t.Errorf("confirm without token: %d", c)
	}
}

func TestMutationShape(t *testing.T) {
	e := setup(t)
	body := map[string]string{"text": "x"}
	if c, _ := e.do(t, "POST", "/api/send", body, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }); c < 400 {
		t.Errorf("non-JSON POST accepted: %d", c)
	}
	if c, _ := e.do(t, "GET", "/api/send", nil, nil); c < 400 {
		t.Errorf("GET on mutation route accepted: %d", c)
	}
	if c, _ := e.do(t, "GET", "/api/confirm", nil, nil); c < 400 {
		t.Errorf("GET on confirm accepted: %d", c)
	}
	if c, _ := e.do(t, "POST", "/api/send", map[string]string{"text": strings.Repeat("a", maxBody+10)}, nil); c != 413 {
		t.Errorf("oversized body: %d", c)
	}
}

func TestSendAsHuman(t *testing.T) {
	e := setup(t)
	bob := e.agent(t, "bob")
	c, b := e.do(t, "POST", "/api/send", map[string]any{"room": "local", "text": "hello bob", "to": []string{bob}}, nil)
	if c != 200 {
		t.Fatalf("send: %d %s", c, b)
	}
	var m localapi.SendResp
	json.Unmarshal(b, &m)
	_, hb := e.do(t, "GET", "/api/history?room=local", nil, nil)
	if !strings.Contains(string(hb), "hello bob") || !strings.Contains(string(hb), e.s.HumanID()) {
		t.Errorf("history: %s", hb)
	}
	in, _ := e.s.Inbox(e.ctx, bob, "", 0, 0)
	if len(in.Entries) != 1 || in.Entries[0].Text != "hello bob" || in.Entries[0].From != e.s.HumanID() {
		t.Errorf("bob inbox: %+v", in)
	}
	if c, _ := e.do(t, "GET", "/api/trace?id="+m.ID, nil, nil); c != 200 {
		t.Errorf("trace: %d", c)
	}
	if c, _ := e.do(t, "GET", "/api/thread?id="+m.ID, nil, nil); c != 200 {
		t.Errorf("thread: %d", c)
	}
	if c, _ := e.do(t, "POST", "/api/send", map[string]any{"text": "x", "room": "nowhere"}, nil); c != 400 {
		t.Errorf("unknown room: %d", c)
	}
}

func TestQuestionAnswerFlow(t *testing.T) {
	e := setup(t)
	alice := e.agent(t, "alice")
	q, err := e.s.Send(e.ctx, node.SendInput{Agent: alice, Text: "deploy?", Choices: []string{"yes", "no"}, To: []string{e.s.HumanID()}})
	if err != nil {
		t.Fatal(err)
	}
	st := e.state(t)
	if len(st.Unread) != 1 || st.Unread[0].ID != q.ID || len(st.Unread[0].Choices) != 2 {
		t.Fatalf("unread: %+v", st.Unread)
	}
	if c, b := e.do(t, "POST", "/api/send", map[string]any{"text": "yes", "reply_to": q.ID}, nil); c != 200 {
		t.Fatalf("answer: %d %s", c, b)
	}
	u := st.Unread[0]
	if c, b := e.do(t, "POST", "/api/seen", map[string]any{"entries": []map[string]string{{"key": u.Key, "version": u.Version}}}, nil); c != 200 ||
		!strings.Contains(string(b), `"ok":true`) {
		t.Fatalf("seen: %d %s", c, b)
	}
	if c, b := e.do(t, "POST", "/api/ack", map[string]any{"id": q.ID}, nil); c != 200 {
		t.Fatalf("ack: %d %s", c, b)
	}
	in, _ := e.s.Inbox(e.ctx, alice, "", 0, 0)
	if len(in.Entries) != 1 || in.Entries[0].Parent != q.ID || in.Entries[0].Text != "yes" || in.Entries[0].From != e.s.HumanID() {
		t.Errorf("alice inbox: %+v", in)
	}
	if st := e.state(t); len(st.Unread) != 0 {
		t.Errorf("question still unread: %+v", st.Unread)
	}
}

func TestHostileStringsAreInert(t *testing.T) {
	e := setup(t)
	evil := "<img src=x onerror=alert(1)>"
	_, _ = e.s.Register(e.ctx, "test", evil, "") // cleaned into a label; either way inert
	if c, _ := e.do(t, "POST", "/api/send", map[string]any{"room": "local", "text": evil}, nil); c != 200 {
		t.Fatalf("send: %d", c)
	}
	_, b := e.do(t, "GET", "/api/history?room=local", nil, nil)
	var h struct{ Entries []localapi.Entry }
	if err := json.Unmarshal(b, &h); err != nil || len(h.Entries) != 1 || h.Entries[0].Text != evil {
		t.Fatalf("round trip: %v %s", err, b)
	}
	_, b = e.do(t, "GET", "/api/state", nil, nil)
	if !json.Valid(b) || bytes.Contains(b, []byte("<img")) {
		t.Errorf("state not escaped: %s", b)
	}
}

func TestNoUnsafeDOMSinks(t *testing.T) {
	for _, f := range []string{"static/app.js", "static/index.html"} {
		b, _ := assets.ReadFile(f)
		s := strings.ToLower(string(b))
		for _, bad := range []string{"innerhtml", "outerhtml", "insertadjacenthtml", "document.write", "eval(", "new function", " onclick", " onerror", " onload", "javascript:"} {
			if strings.Contains(s, bad) {
				t.Errorf("%s contains %q", f, bad)
			}
		}
	}
}

// joinRequest makes the joiner accept an invite to room, so that a request
// waits for the admin's person.
func joinRequest(t *testing.T, admin, joiner *env, room, role string) {
	t.Helper()
	inv, err := admin.s.Invite(admin.ctx, room, role, false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := joiner.s.JoinPreview(joiner.ctx, inv.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := joiner.s.JoinAccept(joiner.ctx, inv.Token, p.ID, 0); err != nil {
		t.Fatal(err)
	}
}

// origin sets the page's own Origin, as a fetch from the page does.
func origin(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) }

func TestPageApprovesAndDeclinesRequestsNamingExactlyWhatIsApproved(t *testing.T) {
	admin := setup(t)
	room, err := admin.s.CreateRoom(admin.ctx, "team")
	if err != nil {
		t.Fatal(err)
	}
	joiner := open(t, 2, admin)
	other := open(t, 3, admin)
	joinRequest(t, admin, joiner, room.ID, node.RoleMember)
	st := admin.state(t)
	if len(st.Requests) != 1 || !strings.Contains(st.Requests[0].Text, node.Fingerprint(joiner.id)) ||
		st.Requests[0].Node != joiner.id || st.Requests[0].Fingerprint != node.Fingerprint(joiner.id) || len(st.Alerts) != 1 {
		t.Fatalf("requests = %+v alerts = %+v", st.Requests, st.Alerts)
	}
	q := st.Requests[0]
	// A decision needs the page's exact Origin: none, or another, decides nothing.
	if c, b := admin.do(t, "POST", "/api/confirm", map[string]any{"id": q.ID, "approve": true, "text": q.Text}, nil); c != 400 {
		t.Fatalf("confirm without an Origin: %d %s", c, b)
	}
	if c, b := admin.do(t, "POST", "/api/confirm", map[string]any{"id": q.ID, "approve": true, "text": q.Text},
		func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:1") }); c == 200 {
		t.Fatalf("confirm from another origin: %d %s", c, b)
	}
	if c, b := admin.do(t, "POST", "/api/confirm", map[string]any{"id": q.ID, "approve": true, "text": "add machine ffffffff"}, origin); c != 400 {
		t.Fatalf("confirm naming something else: %d %s", c, b)
	}
	if c, b := admin.do(t, "POST", "/api/confirm", map[string]any{"id": "nope", "approve": true, "text": q.Text}, origin); c != 400 {
		t.Fatalf("confirm of nothing: %d %s", c, b)
	}
	c, b := admin.do(t, "POST", "/api/confirm", map[string]any{"id": q.ID, "approve": true, "text": q.Text}, origin)
	var cf localapi.ConfirmResp
	if c != 200 || json.Unmarshal(b, &cf) != nil || cf.State != store.RequestApproved {
		t.Fatalf("approve: %d %s", c, b)
	}
	joinRequest(t, admin, other, room.ID, node.RoleMember)
	q = admin.state(t).Requests[0]
	c, b = admin.do(t, "POST", "/api/confirm", map[string]any{"id": q.ID, "approve": false, "text": q.Text}, origin)
	if c != 200 || json.Unmarshal(b, &cf) != nil || cf.State != store.RequestDeclined {
		t.Fatalf("decline: %d %s", c, b)
	}
	info, _ := admin.s.RoomList(admin.ctx, room.ID)
	if len(info[0].Members) != 2 {
		t.Fatalf("members after approve and decline: %+v", info[0].Members)
	}
	if st := admin.state(t); len(st.Requests) != 0 || len(st.Status.Rooms) != 2 {
		t.Fatalf("after: %+v", st)
	}
}

// TestPageAsksASecondApprovalBeforeSigningWhenNotCaughtUp: after a restart
// with another member in the room, an approval that would sign answers
// reason not_caught_up and decides nothing; the page shows the warning
// (the same text as node.NotCaughtUpWarning) and only the second, explicit
// approval (not_caught_up_ok) signs, which is recorded with the request.
func TestPageAsksASecondApprovalBeforeSigningWhenNotCaughtUp(t *testing.T) {
	admin := setup(t)
	room, err := admin.s.CreateRoom(admin.ctx, "team")
	if err != nil {
		t.Fatal(err)
	}
	joiner := open(t, 2, admin)
	joinRequest(t, admin, joiner, room.ID, node.RoleMember)
	q := admin.state(t).Requests[0]
	if c, b := admin.do(t, "POST", "/api/confirm", map[string]any{"id": q.ID, "approve": true, "text": q.Text}, origin); c != 200 {
		t.Fatalf("approve the join: %d %s", c, b)
	}
	admin = admin.restart(t)
	if _, err := admin.s.Remove(admin.ctx, room.ID, joiner.id); err != nil {
		t.Fatal(err)
	}
	q = admin.state(t).Requests[0]
	c, b := admin.do(t, "POST", "/api/confirm", map[string]any{"id": q.ID, "approve": true, "text": q.Text}, origin)
	var cf localapi.ConfirmResp
	if c != 200 || json.Unmarshal(b, &cf) != nil || cf.State != store.RequestFailed || cf.Reason != node.ReasonNotCaughtUp || len(cf.Entries) != 0 {
		t.Fatalf("first approval: %d %s", c, b)
	}
	if got, err := admin.st.GetRequest(admin.ctx, q.ID); err != nil || got.State != store.RequestPending {
		t.Fatalf("after the first approval the request is %+v %v, want pending", got, err)
	}
	c, b = admin.do(t, "POST", "/api/confirm", map[string]any{"id": q.ID, "approve": true, "text": q.Text, "not_caught_up_ok": true}, origin)
	if c != 200 || json.Unmarshal(b, &cf) != nil || cf.State != store.RequestApproved || len(cf.Entries) == 0 {
		t.Fatalf("second approval: %d %s", c, b)
	}
	if got, err := admin.st.GetRequest(admin.ctx, q.ID); err != nil || !got.NotCaughtUp {
		t.Fatalf("the override was not recorded: %+v %v", got, err)
	}
	js, _ := assets.ReadFile("static/app.js")
	page := strings.Join(strings.Fields(strings.NewReplacer("\" +", "", "\"", "").Replace(string(js))), " ")
	if !strings.Contains(page, node.NotCaughtUpWarning) || !strings.Contains(string(js), "not_caught_up_ok") {
		t.Fatal("the page does not show node.NotCaughtUpWarning before sending not_caught_up_ok")
	}
}

// The page names a copy report and a text state in the words a2p2 trace
// uses (cmd/a2p2 copyReportWords and textWords, asserted there).
func TestPageTraceUsesTheCLIWords(t *testing.T) {
	b, _ := assets.ReadFile("static/app.js")
	for _, want := range []string{
		"machines that reported holding the author's messages up to this one, as this machine observed",
		"available on this machine", "waiting for a copy", "outside this machine's permitted room history",
		"withdrawn by its author", "expired (text is kept 30 days)",
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
}

// The browser's request for an icon is answered empty, not 404, with the
// page's headers.
func TestTheIconRequestIsAnsweredEmpty(t *testing.T) {
	e := setup(t)
	code, body := e.do(t, "GET", "/favicon.ico", nil, nil)
	if code != http.StatusNoContent || len(body) != 0 {
		t.Fatalf("favicon: %d %q", code, body)
	}
}

// Requirements: U1, U2. The page's level line for an agent, rendered from
// the page's own state endpoint: the raw /api/state JSON is read with the
// fields and the rule of app.js (which the test checks it still uses), for
// one agent with opposite levels in two rooms, before and after one
// changes.
func TestPageRendersOneAgentsOppositeLevelsInTwoRoomsFromItsState(t *testing.T) {
	e := setup(t)
	bob := e.agent(t, "bob")
	if _, err := e.s.CreateRoom(e.ctx, "team"); err != nil {
		t.Fatal(err)
	}
	js, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, use := range []string{"S.st = d.status", "(S.st.levels || []).filter((l) => l.agent === a.id)",
		`roomLabel(l.room) + ": " + l.level`, `"; other rooms: all" : "all in every room"`, "(S.st.rooms || []).find((r) => r.id === id)"} {
		if !strings.Contains(string(js), use) {
			t.Fatalf("app.js no longer renders levels with %q: update this test", use)
		}
	}
	render := func() string {
		t.Helper()
		_, b := e.do(t, "GET", "/api/state", nil, nil)
		var d struct {
			Status struct {
				Human  string `json:"human"`
				Rooms  []struct{ ID, Label string }
				Agents []struct{ ID string }
				Levels []struct{ Agent, Room, Level string }
			} `json:"status"`
		}
		if err := json.Unmarshal(b, &d); err != nil {
			t.Fatal(err)
		}
		label := func(id string) string {
			for _, r := range d.Status.Rooms {
				if r.ID == id {
					return r.Label
				}
			}
			return id
		}
		var words []string
		for _, l := range d.Status.Levels {
			if l.Agent == bob {
				words = append(words, label(l.Room)+": "+l.Level)
			}
		}
		if len(words) == 0 {
			return "level all in every room"
		}
		// The page keeps the state's order, which follows room ids; sort
		// so the expected line does not depend on random ids.
		sort.Strings(words)
		return "level " + strings.Join(words, ", ") + "; other rooms: all"
	}
	if _, err := e.s.Levels(e.ctx, bob, "local", store.LevelNone); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Levels(e.ctx, bob, "team", store.LevelAddressed); err != nil {
		t.Fatal(err)
	}
	if got := render(); got != "level local: none, team: addressed; other rooms: all" {
		t.Fatalf("page shows %q", got)
	}
	if _, err := e.s.Levels(e.ctx, bob, "local", store.LevelAll); err != nil {
		t.Fatal(err)
	}
	if got := render(); got != "level team: addressed; other rooms: all" {
		t.Fatalf("after one change the page shows %q", got)
	}
}

// Requirements: U1, A3. The page shows each local agent's level per room,
// and the person reads the whole room whatever any agent's level is.
func TestPageShowsAgentLevelsAndThePersonReadsEverything(t *testing.T) {
	e := setup(t)
	bob := e.agent(t, "bob")
	alice := e.agent(t, "alice")
	if _, err := e.s.Levels(e.ctx, bob, "local", store.LevelNone); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Send(e.ctx, node.SendInput{Agent: alice, Text: "room-wide"}); err != nil {
		t.Fatal(err)
	}
	st := e.state(t)
	if len(st.Status.Levels) != 1 || st.Status.Levels[0].Agent != bob || st.Status.Levels[0].Level != store.LevelNone {
		t.Fatalf("levels on the page: %+v", st.Status.Levels)
	}
	if len(st.Unread) != 1 || st.Unread[0].Text != "room-wide" {
		t.Fatalf("the person's unread: %+v", st.Unread)
	}
	if _, hb := e.do(t, "GET", "/api/history?room=local", nil, nil); !strings.Contains(string(hb), "room-wide") {
		t.Fatalf("history: %s", hb)
	}
	if in, _ := e.s.Inbox(e.ctx, bob, "", 0, 0); len(in.Entries) != 0 {
		t.Fatalf("bob at none was shown %+v", in.Entries)
	}
}

// get is a GET with the page's token that also returns the headers.
func (e *env) get(t *testing.T, path string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func fileURL(room, msg, sha string) string {
	return "/api/file?" + url.Values{"room": {room}, "msg": {msg}, "sha": {sha}}.Encode()
}

// The new routes keep every check of the others: the token, the Host and
// the Origin.
func TestFileRoutesRefuseWithoutTokenWithForeignOriginAndBadHost(t *testing.T) {
	e, alice := setupFiles(t)
	body := "abc"
	msg := e.approve(t, e.sendFile(t, alice, "a.txt", body))
	routes := []struct {
		method, path string
		body         any
	}{
		{"GET", fileURL("team", msg, shaOf(body)), nil},
		{"POST", "/api/room-files", map[string]string{"room": "team", "mode": "keep"}},
	}
	for _, rt := range routes {
		if c, b := e.do(t, rt.method, rt.path, rt.body, func(r *http.Request) { r.Header.Del("Authorization") }); c != 401 || bytes.Contains(b, []byte(body)) {
			t.Errorf("%s without a token: %d", rt.path, c)
		}
		if c, _ := e.do(t, rt.method, rt.path, rt.body, func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") }); c != 401 {
			t.Errorf("%s with a wrong token: %d", rt.path, c)
		}
		if c, _ := e.do(t, rt.method, rt.path, rt.body, func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }); c != 403 {
			t.Errorf("%s from a foreign origin: %d", rt.path, c)
		}
		if c, _ := e.do(t, rt.method, rt.path, rt.body, func(r *http.Request) { r.Host = "evil.example:80" }); c != 403 {
			t.Errorf("%s with a bad host: %d", rt.path, c)
		}
	}
	// A setting changed with the token but not from the page's exact Origin is refused.
	if c, b := e.do(t, "POST", "/api/room-files", map[string]string{"room": "team", "mode": "keep"}, nil); c != 400 {
		t.Errorf("room-files without the page's Origin: %d %s", c, b)
	}
	if rf, _ := e.s.RoomFiles(e.ctx, "team", ""); rf.Mode != store.FilesCarry {
		t.Errorf("a refused request changed the setting to %s", rf.Mode)
	}
}

// L1, P3: the page lists an agent's send with files with every term the
// CLI shows, and approving it there writes the message with its file.
func TestThePageListsASendWithFilesWithEveryTermAndApprovesIt(t *testing.T) {
	e, alice := setupFiles(t)
	body := "line one\nline two\n"
	q := e.sendFile(t, alice, "build.log", body)
	if q.Kind != store.RequestSendFiles || q.Agent != alice || q.RoomLabel != "team" || q.TextLine != "the log" ||
		len(q.To) != 1 || q.To[0] != node.Everyone || q.Members != 1 || len(q.Terms) != 64 || len(q.Files) != 1 {
		t.Fatalf("request = %+v", q)
	}
	if f := q.Files[0]; !filepath.IsAbs(f.Path) || !strings.HasSuffix(f.Path, "build.log") || f.Name != "build.log" ||
		f.Size != int64(len(body)) || f.SHA256 != shaOf(body) {
		t.Fatalf("request file = %+v", f)
	}
	msg := e.approve(t, q)
	if st := e.state(t); len(st.Requests) != 0 {
		t.Fatalf("a decided request is still listed: %+v", st.Requests)
	}
	_, hb := e.do(t, "GET", "/api/history?room=team", nil, nil)
	var h struct{ Entries []localapi.Entry }
	if err := json.Unmarshal(hb, &h); err != nil || len(h.Entries) != 1 || h.Entries[0].ID != msg || len(h.Entries[0].Files) != 1 {
		t.Fatalf("history: %v %s", err, hb)
	}
	if f := h.Entries[0].Files[0]; f.Name != "build.log" || f.Size != int64(len(body)) || f.SHA256 != shaOf(body) || f.State != node.FileStateHere {
		t.Fatalf("history file = %+v", f)
	}
	// The page shows the same terms as a2p2 confirm (cmd/a2p2 printSendTerms).
	js, _ := assets.ReadFile("static/app.js")
	for _, want := range []string{`r.kind === "send_files"`, "everyone in the room (", "the author of message ",
		"the replacement keeps its recipients and has only the files listed here", "r.body", "reply_author", "terms: r.terms", "kept from the message replaced", "r.terms", "f.path", "f.sha256",
		"declined unless approved"} {
		if !strings.Contains(string(js), want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
}

// F3, PROTOCOL 10.8: a file here is handed to the person as an attachment
// with a safe type and exactly its bytes, never to be rendered.
func TestTheFileRouteServesAHereFileAsAnAttachmentWithItsExactBytes(t *testing.T) {
	e, alice := setupFiles(t)
	body := "<html><script>alert(1)</script>\x00\xff"
	msg := e.approve(t, e.sendFile(t, alice, "résumé page.html", body))
	e.shown(t)
	resp, b := e.get(t, fileURL("team", msg, shaOf(body)))
	if resp.StatusCode != 200 || string(b) != body {
		t.Fatalf("file: %d %q", resp.StatusCode, b)
	}
	for k, v := range map[string]string{
		"Content-Type":            "application/octet-stream",
		"Content-Disposition":     "attachment; filename*=UTF-8''r%C3%A9sum%C3%A9%20page.html",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'; sandbox",
		"Content-Length":          strconv.Itoa(len(body)),
	} {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	// Without the message, the file is found by its hash alone.
	if resp, b := e.get(t, fileURL("", "", shaOf(body))); resp.StatusCode != 200 || string(b) != body {
		t.Fatalf("by hash: %d %q", resp.StatusCode, b)
	}
	// An earlier copy changed on disk is not reused: the fetch writes a
	// fresh copy, verified, and that is what is served.
	res, err := e.s.Fetch(e.ctx, e.s.HumanID(), "team", shaOf(body), msg, 0)
	if err != nil || res.State != node.FetchHere {
		t.Fatalf("fetch: %+v %v", res, err)
	}
	if err := os.WriteFile(res.Path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if resp, b := e.get(t, fileURL("team", msg, shaOf(body))); resp.StatusCode != 200 || string(b) != body {
		t.Fatalf("after an earlier copy was changed: %d %q", resp.StatusCode, b)
	}
	// The copy the fetch hands the page, changed before the page opens
	// it, is not served: the page's own snapshot and hash refuse it.
	fileHook = func(stage, path string) {
		if stage == "fetched" {
			_ = os.WriteFile(path, []byte("tampered"), 0o600)
		}
	}
	t.Cleanup(func() { fileHook = func(string, string) {} })
	if resp, b := e.get(t, fileURL("team", msg, shaOf(body))); resp.StatusCode != 404 || bytes.Contains(b, []byte("tampered")) ||
		resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("a changed copy: %d %s", resp.StatusCode, b)
	}
}

// PROTOCOL 10.8: the bytes served are a snapshot verified before a byte
// is sent. A copy changed in place (same size) after it was opened is
// refused with 404; one changed after the snapshot was verified does not
// change what is served. No snapshot is left behind.
func TestADownloadRacedWithAChangeServesOnlyVerifiedBytes(t *testing.T) {
	e, alice := setupFiles(t)
	t.Cleanup(func() { fileHook = func(string, string) {} })
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	for _, stage := range []string{"opened", "verified"} {
		body := "the verified bytes of " + stage
		evil := strings.ToUpper(body)
		msg := e.approve(t, e.sendFile(t, alice, stage+".txt", body))
		e.shown(t)
		fileHook = func(s, path string) {
			if s != stage {
				return
			}
			f, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				t.Errorf("%s: %v", stage, err)
				return
			}
			_, _ = f.WriteAt([]byte(evil), 0)
			f.Close()
		}
		resp, b := e.get(t, fileURL("team", msg, shaOf(body)))
		switch {
		case bytes.Contains(b, []byte(evil)):
			t.Errorf("changed at %s: served the changed bytes", stage)
		case stage == "opened" && resp.StatusCode != 404:
			t.Errorf("changed after the open: %d %s", resp.StatusCode, b)
		case stage == "verified" && (resp.StatusCode != 200 || string(b) != body):
			t.Errorf("changed after the check: %d %q", resp.StatusCode, b)
		}
		if left, _ := os.ReadDir(tmp); len(left) != 0 {
			t.Errorf("changed at %s: %d files in the system's temporary directory", stage, len(left))
		}
		if left := e.snapshots(t); len(left) != 0 {
			t.Errorf("changed at %s: snapshots left %v", stage, left)
		}
	}
}

// F1, PROTOCOL 10.6: a file whose state is not here is not served; the
// page is told the state. A file no message lists is refused, and so is
// one whose message the page never showed.
func TestTheFileRouteDoesNotServeAFileThatIsNotHere(t *testing.T) {
	e, alice := setupFiles(t)
	body := "old bytes"
	msg := e.approve(t, e.sendFile(t, alice, "a.txt", body))
	e.shown(t)
	// A replacement without files: the file belongs to a version that lost.
	if _, err := e.s.Send(e.ctx, node.SendInput{Agent: alice, Replaces: msg, Text: "no file now"}); err != nil {
		t.Fatal(err)
	}
	resp, b := e.get(t, fileURL("team", msg, shaOf(body)))
	var fr localapi.FetchResp
	if resp.StatusCode != 409 || resp.Header.Get("Content-Type") != "application/json" || json.Unmarshal(b, &fr) != nil ||
		fr.State != node.FetchGone || fr.Path != "" || bytes.Contains(b, []byte(body)) {
		t.Fatalf("a replaced file: %d %s", resp.StatusCode, b)
	}
	if resp, b := e.get(t, fileURL("team", msg, shaOf("never sent"))); resp.StatusCode != 400 || resp.Header.Get("Content-Disposition") != "" {
		t.Fatalf("a file no message lists: %d %s", resp.StatusCode, b)
	}
	if resp, _ := e.get(t, fileURL("team", msg, "nothex")); resp.StatusCode != 400 {
		t.Fatalf("a bad hash: %d", resp.StatusCode)
	}
	// A file here whose message the page has not shown is refused, on the
	// author's machine too; once shown, the same request serves it.
	unseen := "never shown"
	msg2 := e.approve(t, e.sendFile(t, alice, "b.txt", unseen))
	if resp, b := e.get(t, fileURL("team", msg2, shaOf(unseen))); resp.StatusCode != 400 ||
		!strings.Contains(string(b), "read it first") || bytes.Contains(b, []byte(unseen)) {
		t.Fatalf("a file never shown: %d %s", resp.StatusCode, b)
	}
	e.shown(t)
	if resp, b := e.get(t, fileURL("team", msg2, shaOf(unseen))); resp.StatusCode != 200 || string(b) != unseen {
		t.Fatalf("the same file once shown: %d %q", resp.StatusCode, b)
	}
}

// Finding 5: the page's approval of a send with files carries the terms
// the card showed, and the whole text is on the card; without the terms,
// or with others, nothing is sent.
func TestThePageApprovesASendWithFilesOnlyWithTheTermsItShowed(t *testing.T) {
	e, alice := setupFiles(t)
	q := e.sendFile(t, alice, "a.txt", "abc")
	if q.Body != "the log\nmore" {
		t.Fatalf("body = %q", q.Body)
	}
	for _, terms := range []string{"", strings.Repeat("0", 64)} {
		if c, b := e.do(t, "POST", "/api/confirm", map[string]any{"id": q.ID, "approve": true, "text": q.Text, "terms": terms}, origin); c != 400 {
			t.Fatalf("terms %q: %d %s", terms, c, b)
		}
	}
	if st := e.state(t); len(st.Requests) != 1 {
		t.Fatalf("a refused approval decided the request: %+v", st.Requests)
	}
	e.approve(t, q)
}

// The room's files setting is shown from state and changed from the page.
func TestThePageReadsAndSetsTheRoomsFilesSetting(t *testing.T) {
	e, alice := setupFiles(t)
	team := func() *localapi.RoomFiles {
		for _, r := range e.state(t).Status.Rooms {
			if r.Label == "team" {
				return r.Files
			}
		}
		return nil
	}
	if f := team(); f == nil || f.Mode != store.FilesCarry || f.Budget <= 0 || f.Held != 0 {
		t.Fatalf("state files = %+v", f)
	}
	c, b := e.do(t, "POST", "/api/room-files", map[string]string{"room": "team", "mode": "keep"}, origin)
	var rf localapi.RoomFiles
	if c != 200 || json.Unmarshal(b, &rf) != nil || rf.Mode != store.FilesKeep || rf.Budget <= 0 {
		t.Fatalf("set keep: %d %s", c, b)
	}
	if c, b := e.do(t, "POST", "/api/room-files", map[string]string{"room": "team"}, nil); c != 200 || !strings.Contains(string(b), `"mode":"keep"`) {
		t.Fatalf("read: %d %s", c, b)
	}
	// Staged bytes count while the send waits for the person.
	e.sendFile(t, alice, "a.txt", "abc")
	if f := team(); f == nil || f.Mode != store.FilesKeep || f.Held != 3 {
		t.Fatalf("state files after a send = %+v", f)
	}
	if c, _ := e.do(t, "POST", "/api/room-files", map[string]string{"room": "team", "mode": "always"}, origin); c != 400 {
		t.Fatalf("a bad mode: %d", c)
	}
	if c, _ := e.do(t, "POST", "/api/room-files", map[string]string{"room": "nowhere", "mode": "off"}, origin); c != 400 {
		t.Fatalf("an unknown room: %d", c)
	}
}

// PROTOCOL 10.8: the name a download offers has no quotes, controls,
// format characters or slashes, and is percent-encoded for filename*.
func TestADownloadNameIsSanitisedAndPercentEncoded(t *testing.T) {
	for in, want := range map[string]string{
		"report.txt":             "report.txt",
		`a"b'c.txt`:              "a_b_c.txt",
		"../../etc/passwd":       "_.._etc_passwd",
		`dir\file:x`:             "dir_file_x",
		"line\nbreak\x00.txt":    "line_break_.txt",
		"report\u202etxt.exe":    "report_txt.exe",
		" .hidden. ":             "hidden",
		"":                       "file",
		"...":                    "file",
		"bad\xffbyte":            "bad_byte",
		strings.Repeat("é", 150): strings.Repeat("é", 100),
	} {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := percentName("a b;é\"%.txt"); got != "a%20b%3B%C3%A9%22%25.txt" {
		t.Errorf("percentName = %q", got)
	}
}
