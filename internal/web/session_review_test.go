package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/node"
)

// pageRun is what the page harness reports after opening a room.
type pageRun struct {
	AppHidden      bool     `json:"appHidden"`
	AuthfailHidden bool     `json:"authfailHidden"`
	Authwhy        string   `json:"authwhy"`
	Stream         string   `json:"stream"`
	StoredSession  string   `json:"storedSession"`
	Errors         []string `json:"errors"`
}

func runPage(t *testing.T, nodeBin, url, code, room string) pageRun {
	t.Helper()
	b, err := exec.Command(nodeBin, "testdata/page_harness.js", "static/index.html", "static/app.js", url, "c="+code, room).Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, b)
	}
	var got pageRun
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%q: %v", b, err)
	}
	return got
}

// F47, second review: between two states the page reads a history, a
// thread or a file without a fresh state first. Another listener on the
// port that answers only those (the daemon gone between two polls) must
// not get its text drawn: every answer carries the session's run, and an
// answer without it signs the page out, which also forgets the session in
// the tab's storage, so a reload does not send it. The honest page draws
// the room and keeps its session.
func TestAPageDrawsNoHistoryFromAnotherListenerBetweenStates(t *testing.T) {
	nodeBin := needNode(t)
	for _, forged := range []bool{false, true} {
		e := setup(t)
		alice := e.agent(t, "alice")
		room, err := e.s.CreateRoom(e.ctx, "team")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.s.Send(e.ctx, node.SendInput{Agent: alice, Room: room.ID, Text: "hello from alice"}); err != nil {
			t.Fatal(err)
		}
		real := Handler(e.s, e.ss)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !forged || r.URL.Path != "/api/history" {
				real.ServeHTTP(w, r)
				return
			}
			// The squatter: the daemon's own answer, rewritten, without its headers.
			rec := httptest.NewRecorder()
			real.ServeHTTP(rec, r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(strings.ReplaceAll(rec.Body.String(), "hello from alice", "forged by another listener")))
		}))
		t.Cleanup(srv.Close)
		got := runPage(t, nodeBin, srv.URL, e.ss.Code(), room.ID)
		switch {
		case !forged && (!strings.Contains(got.Stream, "hello from alice") || got.StoredSession == "" || got.AppHidden):
			t.Fatalf("the honest page: %+v", got)
		case forged && (strings.Contains(got.Stream, "forged") || !got.AppHidden || got.AuthfailHidden || got.StoredSession != ""):
			t.Fatalf("another listener's history: %+v", got)
		}
	}
}

// F14, second review: where the system says which user is at the other
// end of the connection (Linux), a link's code traded by another user is
// refused before it is looked at, so the person's browser still trades it
// afterwards. The person's own trade works, and so do one where the user
// is not known and one by root.
func TestALinkTradedByAnotherUserIsRefusedAndStaysGood(t *testing.T) {
	e := setup(t)
	old := peerOf
	t.Cleanup(func() { peerOf = old })
	peerOf = func(*http.Request) (int, bool) { return os.Getuid() + 1, true }
	code := e.ss.Code()
	if st, out := e.trade(t, code, e.srv.URL); st != http.StatusForbidden || out["session"] != "" || !strings.Contains(out["error"], "another user") {
		t.Fatalf("another user's trade: %d %v", st, out)
	}
	peerOf = func(*http.Request) (int, bool) { return os.Getuid(), true }
	if st, out := e.trade(t, code, e.srv.URL); st != 200 || out["session"] == "" {
		t.Fatalf("the person's trade after it: %d %v", st, out)
	}
	peerOf = func(*http.Request) (int, bool) { return 0, false }
	if st, out := e.trade(t, e.ss.Code(), e.srv.URL); st != 200 || out["session"] == "" {
		t.Fatalf("a trade where the user is not known: %d %v", st, out)
	}
	peerOf = func(*http.Request) (int, bool) { return 0, true }
	if st, out := e.trade(t, e.ss.Code(), e.srv.URL); st != 200 || out["session"] == "" {
		t.Fatalf("a trade by root, which can read the home anyway: %d %v", st, out)
	}
}

// Second review: every status call makes a code, and the CLI's connect,
// the MCP shim and hooks call status, so a code printed with --print is
// not pushed out by a few hundred calls made before it is opened.
func TestALinkCodeOutlivesTheStatusCallsOfABusyMachine(t *testing.T) {
	ss := NewSessions()
	code := ss.Code()
	for i := 0; i < 500; i++ {
		ss.Code()
	}
	if _, _, why := ss.trade(code); why != 0 {
		t.Fatalf("the printed code was pushed out: %d", why)
	}
}
