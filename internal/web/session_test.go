package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/node"
)

// trade posts code to /api/session as the page does, with origin as its
// Origin header ("" for none).
func (e *env) trade(t *testing.T, code, origin string) (int, map[string]string) {
	t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/session", strings.NewReader(`{"code":`+jsonQuote(code)+`}`))
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out := map[string]string{}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func jsonQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

// stateWith is GET /api/state with session as the Bearer token.
func (e *env) stateWith(t *testing.T, session string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/state", nil)
	req.Header.Set("Authorization", "Bearer "+session)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

// F14: the link a2p2 ui opens carries a code that gives one session, once.
// A second use is refused and ends the session the first use made, since
// one of the two was not the person's; the code itself is never a session.
func TestTheUILinkCarriesAOneTimeCodeThatWorksOnce(t *testing.T) {
	e := setup(t)
	origin := e.srv.URL
	link := e.ss.Link("127.0.0.1:7422")
	code, ok := strings.CutPrefix(link, "http://127.0.0.1:7422/#c=")
	if !ok || len(code) < 32 {
		t.Fatalf("link %q", link)
	}
	if got := e.stateWith(t, code); got != 401 {
		t.Fatalf("the code itself was taken as a session: %d", got)
	}
	st, out := e.trade(t, code, origin)
	if st != 200 || out["session"] == "" || len(out["run"]) < 32 || out["session"] == code {
		t.Fatalf("first trade: %d %v", st, out)
	}
	if got := e.stateWith(t, out["session"]); got != 200 {
		t.Fatalf("the session the code gave is refused: %d", got)
	}
	st, again := e.trade(t, code, origin)
	if st != 401 || again["session"] != "" || !strings.Contains(again["error"], "used already") {
		t.Fatalf("second trade: %d %v", st, again)
	}
	if got := e.stateWith(t, out["session"]); got != 401 {
		t.Fatalf("a code used twice left its first session signed in: %d", got)
	}
	// Another code still works: the person runs a2p2 ui again.
	if st, out := e.trade(t, e.ss.Code(), origin); st != 200 || e.stateWith(t, out["session"]) != 200 {
		t.Fatalf("a fresh code: %d %v", st, out)
	}
}

// F47: each session has its own run, and a state carries the run of the
// session that reads it. One process-wide run would let a user who once
// traded a code (their own, or the person's first) answer a tab of the
// person's later as its daemon, after taking the port.
func TestEachSessionHasItsOwnRun(t *testing.T) {
	e := setup(t)
	_, a := e.trade(t, e.ss.Code(), e.srv.URL)
	_, b := e.trade(t, e.ss.Code(), e.srv.URL)
	if a["run"] == "" || a["run"] == b["run"] {
		t.Fatalf("two sessions share a run: %q %q", a["run"], b["run"])
	}
	for _, s := range []map[string]string{a, b} {
		req, _ := http.NewRequest("GET", e.srv.URL+"/api/state", nil)
		req.Header.Set("Authorization", "Bearer "+s["session"])
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var st struct {
			Run string `json:"run"`
		}
		err = json.NewDecoder(res.Body).Decode(&st)
		res.Body.Close()
		if err != nil || res.StatusCode != 200 || st.Run != s["run"] {
			t.Fatalf("state for a session with run %q: %d %q %v", s["run"], res.StatusCode, st.Run, err)
		}
	}
}

// F14: a code read later, from a process list or a browser's history, is
// dead once its 30 seconds have passed.
func TestALinkCodeIsRefusedAfterItExpires(t *testing.T) {
	e := setup(t)
	now := time.Now()
	e.ss.now = func() time.Time { return now }
	code := e.ss.Code()
	now = now.Add(CodeLife)
	if st, out := e.trade(t, code, e.srv.URL); st != 401 || out["session"] != "" || !strings.Contains(out["error"], "expired") {
		t.Fatalf("an expired code: %d %v", st, out)
	}
	fresh := e.ss.Code()
	now = now.Add(CodeLife - time.Second)
	if st, _ := e.trade(t, fresh, e.srv.URL); st != 200 {
		t.Fatalf("a code within its life: %d", st)
	}
}

// F14: another user who read the code from a process list trades it
// first; the person's browser trades it later, after the code's life has
// passed (a slow browser start, a link pasted late) or after many newer
// codes (each CLI call asks for status). The second trade still ends the
// first trade's session, and the daemon says so in its log.
func TestALateSecondTradeStillEndsTheFirstTradesSession(t *testing.T) {
	for _, late := range []string{"after its life", "after newer codes"} {
		t.Run(late, func(t *testing.T) {
			e := setup(t)
			now := time.Now()
			e.ss.now = func() time.Time { return now }
			var logs strings.Builder
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })
			code := e.ss.Code()
			st, first := e.trade(t, code, e.srv.URL)
			if st != 200 {
				t.Fatal(st)
			}
			if late == "after its life" {
				now = now.Add(CodeLife + time.Second)
			} else {
				for i := 0; i < 2*maxCodes; i++ {
					e.ss.Code()
				}
			}
			st, again := e.trade(t, code, e.srv.URL)
			if st != 401 || !strings.Contains(again["error"], "used already") {
				t.Fatalf("second trade: %d %v", st, again)
			}
			if e.stateWith(t, first["session"]) != 401 {
				t.Fatal("the first trade's session survived a late second trade")
			}
			if !strings.Contains(logs.String(), "used twice") {
				t.Fatalf("no warning in the log: %q", logs.String())
			}
		})
	}
}

// F14: many trades of one code at once give at most one session, and it
// does not survive: the second trade ends it, and a trade after that finds
// nothing left to end and is refused as unknown.
func TestConcurrentTradesOfOneCodeLeaveNoSession(t *testing.T) {
	ss := NewSessions()
	code := ss.Code()
	toks := make(chan string, 16)
	var reused atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < cap(toks); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch tok, _, why := ss.trade(code); why {
			case 0:
				toks <- tok
			case tradeReused:
				reused.Add(1)
			}
		}()
	}
	wg.Wait()
	close(toks)
	var got []string
	for tok := range toks {
		got = append(got, tok)
	}
	if len(got) != 1 || reused.Load() != 1 || ss.valid(got[0]) {
		t.Fatalf("sessions given %d; the one given still valid: %v", len(got), len(got) == 1 && ss.valid(got[0]))
	}
	if len(ss.traded) != 0 || len(ss.sessions) != 0 {
		t.Fatalf("kept after the session ended: %d traded, %d sessions", len(ss.traded), len(ss.sessions))
	}
}

// F14: only the page itself can trade a code: a request without the
// page's exact Origin is refused and does not use the code up.
func TestTradingALinkCodeNeedsThePagesExactOrigin(t *testing.T) {
	e := setup(t)
	code := e.ss.Code()
	for _, origin := range []string{"", "http://evil.example", "null"} {
		if st, out := e.trade(t, code, origin); st != 403 || out["session"] != "" {
			t.Fatalf("Origin %q: %d %v", origin, st, out)
		}
	}
	if st, _ := e.trade(t, code, e.srv.URL); st != 200 {
		t.Fatalf("the page's own trade after refused ones: %d", st)
	}
}

// What a status call gives out is bounded: old codes and sessions go
// first, so a program that keeps asking cannot grow the daemon's memory.
func TestCodesAndSessionsAreBounded(t *testing.T) {
	ss := NewSessions()
	first := ss.Code()
	for i := 0; i < 3*maxCodes; i++ {
		ss.Code()
	}
	if len(ss.codes) > maxCodes || len(ss.codeAge) > maxCodes {
		t.Fatalf("%d codes kept", len(ss.codes))
	}
	if _, _, why := ss.trade(first); why != tradeUnknown {
		t.Fatalf("the oldest code was kept past the bound: %d", why)
	}
	var toks []string
	for i := 0; i < 2*maxSessions; i++ {
		tok, _, why := ss.trade(ss.Code())
		if why != 0 {
			t.Fatal(why)
		}
		toks = append(toks, tok)
	}
	if len(ss.sessions) != maxSessions || ss.valid(toks[0]) || !ss.valid(toks[len(toks)-1]) {
		t.Fatalf("%d sessions; oldest valid %v; newest valid %v", len(ss.sessions), ss.valid(toks[0]), ss.valid(toks[len(toks)-1]))
	}
}

// F47: a tab left open after its daemon stopped, now talking to another
// local user's listener on the same port, refuses to draw that listener's
// state and makes no further call. The stand-in listener here answers
// every call as a daemon of another run would.
func TestAPageRefusesStateFromAnotherRun(t *testing.T) {
	nodeBin := needNode(t)
	e := setup(t)
	other := Handler(e.s, testSessions()) // another run: another run name
	mux := http.NewServeMux()
	mux.Handle("/api/session", Handler(e.s, e.ss))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+tok) // a squatter takes any token
		other.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	room, err := e.s.CreateRoom(e.ctx, "team")
	if err != nil {
		t.Fatal(err)
	}
	b, err := exec.Command(nodeBin, "testdata/page_harness.js", "static/index.html", "static/app.js", srv.URL, "c="+e.ss.Code(), room.ID).Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, b)
	}
	var got struct {
		AppHidden      bool           `json:"appHidden"`
		AuthfailHidden bool           `json:"authfailHidden"`
		Authwhy        string         `json:"authwhy"`
		Rooms          []string       `json:"rooms"`
		Scenario       map[string]any `json:"scenario"`
		Errors         []string       `json:"errors"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%q: %v", b, err)
	}
	if !got.AppHidden || got.AuthfailHidden || !strings.Contains(got.Authwhy, "earlier run") || len(got.Rooms) != 0 {
		t.Fatalf("a state from another run was drawn: %+v", got)
	}
}

// F47: a working tab whose listener changes under it (the daemon stops and
// another local user answers on its port) sends nothing there: neither
// what the person types nor a confirmation of what was read. The stand-in
// serves the real page until the room is open and read, then answers every call as
// a daemon between runs does (503) or as another run's daemon would, and
// records each POST it receives from then on.
func TestAPageWhoseListenerChangesMidRunSendsNothingThere(t *testing.T) {
	nodeBin := needNode(t)
	for _, mode := range []string{"unavailable", "another run", "another session"} {
		t.Run(mode, func(t *testing.T) {
			e := setup(t)
			alice := e.agent(t, "alice")
			room, err := e.s.CreateRoom(e.ctx, "team")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.s.Send(e.ctx, node.SendInput{Agent: alice, Room: room.ID, Text: "hello from alice"}); err != nil {
				t.Fatal(err)
			}
			real, other := Handler(e.s, e.ss), Handler(e.s, testSessions())
			squat := tok // a squatter takes any token and answers as another run
			if mode == "another session" {
				// The same process, as a user who traded a code of their own
				// (or the person's first) answers: that session's run.
				other = real
				var why int
				if squat, _, why = e.ss.trade(e.ss.Code()); why != 0 {
					t.Fatal(why)
				}
			}
			var opened, swapped atomic.Bool
			var mu sync.Mutex
			var leaked []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !swapped.Load() {
					real.ServeHTTP(w, r)
					if r.URL.Path == "/api/history" && r.URL.Query().Get("room") == room.ID {
						opened.Store(true)
					}
					if opened.Load() && r.URL.Path == "/api/seen" {
						swapped.Store(true) // the room is drawn and read; the daemon goes away
					}
					return
				}
				if r.Method == "POST" {
					b, _ := io.ReadAll(r.Body)
					mu.Lock()
					leaked = append(leaked, r.URL.Path+" "+string(b))
					mu.Unlock()
				}
				if mode == "unavailable" {
					writeJSON(w, 503, map[string]string{"error": "restarting"})
					return
				}
				r.Header.Set("Authorization", "Bearer "+squat)
				other.ServeHTTP(w, r)
			}))
			t.Cleanup(srv.Close)
			b, err := exec.Command(nodeBin, "testdata/page_harness.js", "static/index.html", "static/app.js", srv.URL, "c="+e.ss.Code(), room.ID).Output()
			if err != nil {
				t.Fatalf("node: %v\n%s", err, b)
			}
			var got struct {
				Stream   string   `json:"stream"`
				Errors   []string `json:"errors"`
				Scenario struct {
					Author  string `json:"author"`
					SendErr string `json:"sendErr"`
				} `json:"scenario"`
			}
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("%q: %v", b, err)
			}
			if !swapped.Load() || got.Scenario.Author == "" || !strings.Contains(got.Stream, "hello from alice") {
				t.Fatalf("the scenario did not reach the send: %s", b)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(leaked) > 0 {
				t.Fatalf("the replaced listener received: %q", leaked)
			}
			want := "nothing was sent"
			if mode != "unavailable" {
				want = "signed out"
			}
			if !strings.Contains(got.Scenario.SendErr, want) {
				t.Fatalf("the page said %q after a refused send", got.Scenario.SendErr)
			}
		})
	}
}

// F14: a link that does not work says why on the page.
func TestAPageOpenedWithAUsedLinkSaysWhy(t *testing.T) {
	nodeBin := needNode(t)
	e := setup(t)
	code := e.ss.Code()
	if st, _ := e.trade(t, code, e.srv.URL); st != 200 {
		t.Fatal(st)
	}
	b, err := exec.Command(nodeBin, "testdata/page_harness.js", "static/index.html", "static/app.js", e.srv.URL, "c="+code, "r_none").Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, b)
	}
	var got struct {
		AppHidden      bool   `json:"appHidden"`
		AuthfailHidden bool   `json:"authfailHidden"`
		Authwhy        string `json:"authwhy"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%q: %v", b, err)
	}
	if !got.AppHidden || got.AuthfailHidden || !strings.Contains(got.Authwhy, "used already") {
		t.Fatalf("page after a used link: %+v", got)
	}
}

func needNode(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the page harness is not run on Windows")
	}
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("A2P2_REQUIRE_NODE") != "" {
			t.Fatal("A2P2_REQUIRE_NODE is set and node is not on PATH")
		}
		t.Skip("node is not on PATH")
	}
	return nodeBin
}
