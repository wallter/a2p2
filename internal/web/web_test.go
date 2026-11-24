package web

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
