package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

const tok = "test-token-123"

type env struct {
	srv *httptest.Server
	n   *node.Node
	ctx context.Context
}

func setup(t *testing.T) env {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "a2p2.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	n, err := node.New(ctx, st, strings.Repeat("ab", 32), "box", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	m := node.NewMesh(n, nil, func() []string { return nil })
	srv := httptest.NewServer(Handler(n, m, tok))
	t.Cleanup(srv.Close)
	return env{srv, n, ctx}
}

func (e env) do(t *testing.T, method, path string, body any, mod func(*http.Request)) (int, []byte) {
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

func (e env) agent(t *testing.T, label string) string {
	t.Helper()
	a, err := e.n.Register(e.ctx, "test", label, "")
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
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
	var m store.Message
	json.Unmarshal(b, &m)
	if m.From != e.n.HumanID() {
		t.Errorf("from = %q", m.From)
	}
	_, hb := e.do(t, "GET", "/api/history?room=local", nil, nil)
	if !strings.Contains(string(hb), "hello bob") {
		t.Errorf("history: %s", hb)
	}
	in, _ := e.n.Inbox(e.ctx, bob, "", 10, 0)
	if len(in) != 1 || in[0].Body.Text != "hello bob" {
		t.Errorf("bob inbox: %+v", in)
	}
	if c, _ := e.do(t, "GET", "/api/trace?id="+m.ID, nil, nil); c != 200 {
		t.Errorf("trace: %d", c)
	}
}

func TestKindsRefused(t *testing.T) {
	e := setup(t)
	for _, k := range []string{"receipt", "question", "bogus"} {
		if c, _ := e.do(t, "POST", "/api/send", map[string]any{"text": "x", "kind": k}, nil); c != 400 {
			t.Errorf("kind %q: %d", k, c)
		}
	}
	if c, _ := e.do(t, "POST", "/api/send", map[string]any{"text": "x", "kind": "answer"}, nil); c != 400 {
		t.Errorf("answer without reply_to: %d", c)
	}
}

func TestQuestionAnswerFlow(t *testing.T) {
	e := setup(t)
	alice := e.agent(t, "alice")
	q, err := e.n.Send(e.ctx, node.SendReq{From: alice, Text: "deploy?", Kind: core.KindQuestion, Choices: []string{"yes", "no"}, To: []string{e.n.HumanID()}})
	if err != nil {
		t.Fatal(err)
	}
	_, b := e.do(t, "GET", "/api/state", nil, nil)
	var st struct {
		Unread []store.Message `json:"unread"`
	}
	json.Unmarshal(b, &st)
	if len(st.Unread) != 1 || st.Unread[0].ID != q.ID {
		t.Fatalf("unread: %s", b)
	}
	if c, b := e.do(t, "POST", "/api/send", map[string]any{"room": q.Room, "text": "yes", "kind": "answer", "reply_to": q.ID}, nil); c != 200 {
		t.Fatalf("answer: %d %s", c, b)
	}
	e.do(t, "POST", "/api/seen", map[string]any{"ids": []string{q.ID}}, nil)
	in, _ := e.n.Inbox(e.ctx, alice, "", 10, 0)
	if len(in) != 1 || in[0].Kind != core.KindAnswer || in[0].ReplyTo != q.ID || in[0].Body.Text != "yes" {
		t.Errorf("alice inbox: %+v", in)
	}
	_, b = e.do(t, "GET", "/api/state", nil, nil)
	if strings.Contains(string(b), `"deploy?"`) {
		t.Errorf("question still unread: %s", b)
	}
}

func TestHostileStringsAreInert(t *testing.T) {
	e := setup(t)
	evil := "<img src=x onerror=alert(1)>"
	_, _ = e.n.Register(e.ctx, "test", evil, "") // may be refused by label validation; either way inert
	c, _ := e.do(t, "POST", "/api/send", map[string]any{"room": "local", "text": evil}, nil)
	if c != 200 {
		t.Fatalf("send: %d", c)
	}
	_, b := e.do(t, "GET", "/api/history?room=local", nil, nil)
	var ms []store.Message
	if err := json.Unmarshal(b, &ms); err != nil || len(ms) != 1 || ms[0].Body.Text != evil {
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
