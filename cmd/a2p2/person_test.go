package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
)

// The tests run in whatever shell started them, perhaps an agent's: none
// of its variables decide who the CLI acts as, unless a test sets them.
func TestMain(m *testing.M) {
	getenv = func(string) string { return "" }
	os.Exit(m.Run())
}

// inShell makes the CLI see the environment env for the rest of t.
func inShell(t *testing.T, env map[string]string) {
	t.Helper()
	old := getenv
	getenv = func(k string) string { return env[k] }
	t.Cleanup(func() { getenv = old })
}

// unread is what the person has unread, read at a terminal with --peek.
func (d *daemonForCLI) unread(t *testing.T) string {
	t.Helper()
	out, _, err := d.run(t, true, "", "inbox", "--peek", "--room", "team")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// askThePerson has an agent ask the person a question, addressed to
// human@<this machine> so the daemon lists it among the person's
// questions, and returns its id.
func (d *daemonForCLI) askThePerson(t *testing.T, text string) string {
	t.Helper()
	if _, _, err := d.run(t, false, "", "send", "--as", "bot", "--room", "team", "--to", d.s.HumanID(), text); err != nil {
		t.Fatal(err)
	}
	return lastID(t, d.unread(t))
}

// asked reports whether id is still on the person's list of questions.
func (d *daemonForCLI) asked(t *testing.T, id string) bool {
	t.Helper()
	qs, err := d.s.Questions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(qs, func(e node.Entry) bool { return e.ID == id })
}

// lastID is the id line of the newest entry in out.
func lastID(t *testing.T, out string) string {
	t.Helper()
	i := strings.LastIndex(out, "    id ")
	if i < 0 {
		t.Fatalf("no id in %q", out)
	}
	return strings.TrimSpace(out[i+len("    id "):])
}

// F11: an explicitly empty --as is a mistake (a shell variable that was
// not set), never the person.
func TestCLIRefusesAnEmptyAs(t *testing.T) {
	d := newDaemonForCLI(t)
	for _, args := range [][]string{
		{"send", "--as", "", "--room", "team", "approved"},
		{"send", "--as=", "--room", "team", "approved"},
		{"inbox", "--as", "", "--json"},
		{"history", "--as", ""},
		{"ack", "--as", "", "m1"},
	} {
		_, _, err := d.run(t, true, "", args[0], args[1:]...)
		if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "--as needs a label") {
			t.Errorf("%q: %v", args, err)
		}
	}
	if out, _, _ := d.run(t, true, "", "history", "--room", "team"); strings.Contains(out, "approved") {
		t.Fatalf("sent anyway: %q", out)
	}
	if _, _, err := d.run(t, true, "", "send", "--as", "bot", "--person", "--room", "team", "x"); !errors.Is(err, errUsage) {
		t.Fatalf("--as with --person: %v", err)
	}
}

// F11: away from a terminal, send, ack and a reading inbox refuse to act
// as the person, so a program with a shell does not answer the person's
// questions or take them off the person's list by accident.
func TestCLIWithoutATerminalDoesNotActAsThePerson(t *testing.T) {
	d := newDaemonForCLI(t)
	ask := d.askThePerson(t, "Ship it?")
	if !d.asked(t, ask) {
		t.Fatal("the fixture's question is not on the person's list")
	}
	for _, args := range [][]string{
		{"send", "--room", "team", "--reply-to", ask, "approved"},
		{"inbox", "--room", "team"},
		{"inbox", "--level", "none"},
		{"ack", ask},
	} {
		_, _, err := d.run(t, false, "", args[0], args[1:]...)
		if !errors.Is(err, errNoTerminal) || !strings.Contains(err.Error(), "--person") {
			t.Errorf("%q without a terminal: %v", args, err)
		}
	}
	if out := d.unread(t); !strings.Contains(out, "Ship it?") || strings.Contains(out, "approved") || !d.asked(t, ask) {
		t.Fatalf("the question was answered or marked seen: %q", out)
	}
	// Reading without marking anything needs no terminal.
	for _, args := range [][]string{{"inbox", "--peek", "--room", "team"}, {"history", "--room", "team"}, {"inbox", "--thread", ask}} {
		if out, _, err := d.run(t, false, "", args[0], args[1:]...); err != nil || !strings.Contains(out, "Ship it?") {
			t.Errorf("%q: %q %v", args, out, err)
		}
	}
}

// F11: the honest case: at a terminal the person reads, answers and
// reports a question handled as before.
func TestThePersonAtATerminalStillReadsAndAnswers(t *testing.T) {
	d := newDaemonForCLI(t)
	ask := d.askThePerson(t, "Ship it?")
	out, _, err := d.run(t, true, "", "inbox", "--room", "team")
	if err != nil || !strings.Contains(out, "Ship it?") || d.asked(t, ask) {
		t.Fatalf("inbox: %q %v, still asked %v", out, err, d.asked(t, ask))
	}
	if _, _, err := d.run(t, true, "", "send", "--room", "team", "--reply-to", ask, "yes"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.run(t, true, "", "ack", ask); err != nil {
		t.Fatal(err)
	}
	out, _, err = d.run(t, false, "", "history", "--room", "team")
	if err != nil || !strings.Contains(out, "human@") || !strings.Contains(out, "    yes\n") {
		t.Fatalf("history: %q %v", out, err)
	}
}

// lockedBuffer is a log both the daemon's goroutines and the test use.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
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

// F11: a script that must act as the person says so with --person, and
// the daemon's log records each such use; at a terminal nothing is
// logged.
func TestPersonFlagActsAsThePersonAndTheDaemonLogRecordsIt(t *testing.T) {
	var log lockedBuffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&log, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	d := newDaemonForCLI(t)
	if _, _, err := d.run(t, true, "", "send", "--person", "--room", "team", "at a terminal"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(log.String(), "--person") {
		t.Fatalf("logged at a terminal: %q", log.String())
	}
	if _, _, err := d.run(t, false, "", "send", "--person", "--room", "team", "from a script"); err != nil {
		t.Fatal(err)
	}
	if got := log.String(); !strings.Contains(got, "acting as the person with --person") || !strings.Contains(got, "command=send") {
		t.Fatalf("not recorded: %q", got)
	}
	out, _, _ := d.run(t, false, "", "history", "--room", "team")
	if !strings.Contains(out, "from a script") || strings.Count(out, "human@") != 2 {
		t.Fatalf("history: %q", out)
	}
}

// F11: in a shell a harness says is its agent's, the CLI without --as
// acts as that agent, never as the person, with a terminal or without:
// it neither speaks for the person nor takes a question off the person's
// list. The markers are set by the test as the harnesses are said to set
// them (person.go names which were seen in a real shell).
func TestAgentShellGetsTheAgentsIdentityNotThePersons(t *testing.T) {
	d := newDaemonForCLI(t)
	ask := d.askThePerson(t, "Ship it?")
	dir := filepath.Join(t.TempDir(), "proj")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	for _, atTerminal := range []bool{false, true} {
		for _, sh := range []struct{ env, label string }{
			{"CLAUDECODE", "claude-proj@"}, {"CODEX_THREAD_ID", "codex-proj@"}, {"CODEX_SANDBOX", "codex-proj@"},
			{"GEMINI_CLI", "gemini-proj@"}, {"AI_AGENT", "agent-proj@"},
		} {
			inShell(t, map[string]string{sh.env: "1"})
			_, serr, err := d.run(t, atTerminal, "", "send", "--room", "team", "--reply-to", ask, "approved by "+sh.env)
			if err != nil || !strings.Contains(serr, sh.label) || !strings.Contains(serr, "not as the person") {
				t.Fatalf("%s terminal=%v: %q %v", sh.env, atTerminal, serr, err)
			}
			if _, _, err := d.run(t, atTerminal, "", "inbox", "--room", "team"); err != nil {
				t.Fatalf("%s terminal=%v inbox: %v", sh.env, atTerminal, err)
			}
		}
	}
	inShell(t, nil)
	out, _, _ := d.run(t, false, "", "history", "--room", "team")
	if strings.Contains(out, "  "+d.s.HumanID()+" ") || !strings.Contains(out, "  claude-proj@") {
		t.Fatalf("sent as: %q", out)
	}
	if !d.asked(t, ask) {
		t.Fatal("an agent's reply or inbox took the person's question off their list")
	}
}

// F11: in an agent's shell, even at a terminal, --person acts as the
// person, and the daemon log records it, since no person was seen typing.
func TestPersonFlagInAnAgentShellIsLoggedEvenAtATerminal(t *testing.T) {
	var log lockedBuffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&log, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	d := newDaemonForCLI(t)
	ask := d.askThePerson(t, "Ship it?")
	inShell(t, map[string]string{"CLAUDECODE": "1"})
	if _, _, err := d.run(t, true, "", "send", "--person", "--room", "team", "--reply-to", ask, "yes"); err != nil {
		t.Fatal(err)
	}
	if got := log.String(); !strings.Contains(got, "acting as the person with --person") || !strings.Contains(got, "command=send") {
		t.Fatalf("not recorded: %q", got)
	}
	out, _, _ := d.run(t, false, "", "history", "--room", "team")
	if !strings.Contains(out, "  "+d.s.HumanID()+" ") {
		t.Fatalf("history: %q", out)
	}
}

// F11, F41: the page's address carries the token that lets the page act
// as the person, so a2p2 ui gives it as the person's identity is given:
// not in an agent's shell, and away from a terminal only with --person,
// which the daemon log records.
func TestUIGivesThePagesAddressOnlyWhereThePersonIs(t *testing.T) {
	var log lockedBuffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&log, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	const url = "http://127.0.0.1:7422/#token=0123456789abcdef"
	d := newDaemonForCLIInfo(t, nil, localapi.Info{Version: "t", UIURL: func() string { return url }})
	for _, tc := range []struct {
		env        map[string]string
		atTerminal bool
	}{{map[string]string{"CLAUDECODE": "1"}, false}, {map[string]string{"CODEX_THREAD_ID": "x"}, true}, {nil, false}} {
		inShell(t, tc.env)
		for _, args := range [][]string{{"--print"}, {"--json"}} {
			out, _, err := d.run(t, tc.atTerminal, "", "ui", args...)
			if err == nil || !strings.Contains(err.Error(), "--person") || strings.Contains(out, "token") {
				t.Errorf("%v terminal=%v %q: %q %v", tc.env, tc.atTerminal, args, out, err)
			}
		}
	}
	if _, _, err := d.run(t, true, "", "ui", "--print", "--as", "bot"); !errors.Is(err, errUsage) {
		t.Errorf("--as: %v", err)
	}
	inShell(t, nil)
	if out, _, err := d.run(t, true, "", "ui", "--print"); err != nil || out != url+"\n" {
		t.Fatalf("at a terminal: %q %v", out, err)
	}
	if strings.Contains(log.String(), "--person") {
		t.Fatalf("logged at a terminal: %q", log.String())
	}
	inShell(t, map[string]string{"CLAUDECODE": "1"})
	if out, _, err := d.run(t, false, "", "ui", "--print", "--person"); err != nil || out != url+"\n" {
		t.Fatalf("--person: %q %v", out, err)
	}
	if got := log.String(); !strings.Contains(got, "acting as the person with --person") || !strings.Contains(got, "command=ui") {
		t.Fatalf("not recorded: %q", got)
	}
}

// F38: leaving a room deletes its data here, so it is asked at a terminal
// with a typed yes, as room files and net are.
func TestRoomLeaveNeedsATerminalAndAYes(t *testing.T) {
	d := newDaemonForCLI(t)
	if _, _, err := d.run(t, false, "yes\n", "room", "leave", "team"); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("without a terminal: %v", err)
	}
	if _, serr, err := d.run(t, true, "no\n", "room", "leave", "team"); err == nil || !strings.Contains(serr, "deleted") {
		t.Fatalf("answered no: %q %v", serr, err)
	}
	if out, _, err := d.run(t, false, "", "room", "list", "team"); err != nil || !strings.Contains(out, "Room team") {
		t.Fatalf("left without a yes: %q %v", out, err)
	}
	if out, _, err := d.run(t, true, "yes\n", "room", "leave", "team"); err != nil || !strings.Contains(out, "left") {
		t.Fatalf("at a terminal: %q %v", out, err)
	}
	if _, _, err := d.run(t, false, "", "room", "list", "team"); err == nil {
		t.Fatal("still here after leaving")
	}
	if _, _, err := d.run(t, true, "yes\n", "room", "leave", "local"); err == nil || !strings.Contains(err.Error(), "local room") {
		t.Fatalf("local room: %v", err)
	}
}

// F38: the prompt says when this machine is the room's only member, or
// its only admin, which no one can undo.
func TestLeaveAsTheLastAdminWarns(t *testing.T) {
	self, other := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, tc := range []struct {
		r    localapi.Room
		want string
	}{
		{localapi.Room{ID: "r1", Label: "team", Members: []localapi.Member{{Node: self, Admin: true}}, Admins: []string{self}}, "only member"},
		{localapi.Room{ID: "r1", Label: "team", Members: []localapi.Member{{Node: self, Admin: true}, {Node: other}}, Admins: []string{self}}, "only admin"},
		{localapi.Room{ID: "r1", Label: "team", Members: []localapi.Member{{Node: self, Admin: true}, {Node: other, Admin: true}}, Admins: []string{self, other}}, ""},
		{localapi.Room{ID: "r1", Label: "team", Members: []localapi.Member{{Node: self}, {Node: other, Admin: true}}, Admins: []string{other}}, ""},
	} {
		var out bytes.Buffer
		leaveWarning(&out, tc.r, self)
		got := out.String()
		if !strings.Contains(got, "deleted") || tc.want != "" && !strings.Contains(got, tc.want) ||
			tc.want == "" && (strings.Contains(got, "only admin") || strings.Contains(got, "only member")) {
			t.Errorf("%+v: %q", tc.r, got)
		}
	}
	// A label from another machine is escaped.
	var out bytes.Buffer
	leaveWarning(&out, localapi.Room{ID: "r1", Label: "ops\x1b[2K", Admins: []string{other}}, self)
	if strings.Contains(out.String(), "\x1b") || !strings.Contains(out.String(), node.EscapeDisplay("ops\x1b[2K")) {
		t.Fatalf("label: %q", out.String())
	}
}

// F41: a2p2 status --json, which agents run, leaves out the page's
// address and its token; the other members keep their order and their
// bytes as the daemon's compact answer has them. a2p2 ui --print, at a
// terminal, still gives the address.
func TestStatusJSONLeavesOutThePageAddress(t *testing.T) {
	raw := []byte(`{"node":"ab","name":"m","ui":"http://127.0.0.1:7424/#token=secret","rooms":[{"id":"r","ui":"kept"}],"x":"a<b"}` + "\n")
	got, err := withoutKey(raw, "ui")
	want := `{"node":"ab","name":"m","rooms":[{"id":"r","ui":"kept"}],"x":"a<b"}` + "\n"
	if err != nil || string(got) != want {
		t.Fatalf("got %q %v", got, err)
	}
	const url = "http://127.0.0.1:7422/#token=0123456789abcdef"
	d := newDaemonForCLIInfo(t, nil, localapi.Info{Version: "t", UIURL: func() string { return url }})
	raw, err = d.cl.CallRaw(context.Background(), "status", localapi.StatusReq{}, nil)
	if err != nil || !strings.Contains(string(raw), "0123456789abcdef") {
		t.Fatalf("the daemon's own answer has no token: %q %v", raw, err)
	}
	out, _, err := d.run(t, false, "", "status", "--json")
	if err != nil || !strings.HasPrefix(out, `{"node":"`) || strings.Contains(out, `"ui"`) || strings.Contains(out, "0123456789abcdef") ||
		!strings.Contains(out, `"rooms"`) {
		t.Fatalf("status --json: %q %v", out, err)
	}
	if out, _, err := d.run(t, true, "", "ui", "--print"); err != nil || out != url+"\n" {
		t.Fatalf("ui --print: %q %v", out, err)
	}
	if _, err := withoutKey([]byte(`[1]`), "ui"); err == nil {
		t.Fatal("not an object, yet no error")
	}
}

// F51: before net on, the person is told who gets the addresses it
// advertises.
func TestNetOnSaysWhoGetsTheAdvertisedAddresses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("state directory modes are a Unix rule")
	}
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("A2P2_HOME", home)
	d := newDaemonForCLI(t)
	_, serr, err := d.run(t, true, "no\n", "net", "on", "127.0.0.1:0")
	if err == nil || !strings.Contains(serr, advertisedWarning) {
		t.Fatalf("net on: %q %v", serr, err)
	}
}

// F38: the real prompt for a room this machine alone carries names the
// room by its id and says it is the only member, and the daemon is asked
// to leave that id, not the label typed.
func TestLeaveShowsTheRoomsIDAndLeavesThatID(t *testing.T) {
	var left []string
	d := newDaemonForCLIWith(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/room/leave" {
				var req localapi.RoomReq
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &req)
				left = append(left, req.Room)
				r.Body = io.NopCloser(bytes.NewReader(b))
			}
			h.ServeHTTP(w, r)
		})
	})
	var rs localapi.RoomsResp
	if err := d.cl.Call(context.Background(), "room/list", localapi.RoomListReq{Room: "team"}, &rs); err != nil || len(rs.Rooms) != 1 {
		t.Fatal(err)
	}
	id := rs.Rooms[0].ID
	_, serr, err := d.run(t, true, "yes\n", "room", "leave", "team")
	if err != nil || !strings.Contains(serr, "("+id+")") || !strings.Contains(serr, "only member") {
		t.Fatalf("leave: %q %v", serr, err)
	}
	if len(left) != 1 || left[0] != id {
		t.Fatalf("asked the daemon to leave %q, not %q", left, id)
	}
}

// F38: when the room cannot be looked up, nothing is asked and nothing is
// left: the prompt never falls back to the label typed, which the daemon
// would resolve again after the yes.
func TestLeaveWithoutALookupAsksNothingAndLeavesNothing(t *testing.T) {
	left := false
	d := newDaemonForCLIWith(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/room/list":
				http.Error(w, `{"error":"lookup failed","code":"internal"}`, http.StatusInternalServerError)
				return
			case "/v1/room/leave":
				left = true
			}
			h.ServeHTTP(w, r)
		})
	})
	for _, room := range []string{"team", "nosuch"} {
		_, serr, err := d.run(t, true, "yes\n", "room", "leave", room)
		if err == nil || strings.Contains(serr, "Leave room") || left {
			t.Fatalf("%s: %q %v left=%v", room, serr, err, left)
		}
	}
}

// F38: a room set aside does not list, and leaving is how it is mended:
// it is found by its id or label in status, shown with its id, and left
// by that id. A label a set-aside room shares with another is refused.
func TestLeaveOfASetAsideRoomUsesItsID(t *testing.T) {
	const qid = "r_setaside"
	var left []string
	d := newDaemonForCLIWith(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/status":
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, r)
				var st map[string]any
				_ = json.Unmarshal(rec.Body.Bytes(), &st)
				st["quarantined"] = []localapi.Quarantined{{ID: qid, Label: "old", Reason: "test"}, {ID: "r_two", Label: "team", Reason: "test"}}
				_ = json.NewEncoder(w).Encode(st)
				return
			case "/v1/room/leave":
				var req localapi.RoomReq
				_ = json.NewDecoder(r.Body).Decode(&req)
				left = append(left, req.Room)
				_, _ = w.Write([]byte("{}"))
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	for _, name := range []string{"old", qid} {
		_, serr, err := d.run(t, true, "yes\n", "room", "leave", name)
		if err != nil || !strings.Contains(serr, "("+qid+"), set aside") {
			t.Fatalf("%s: %q %v", name, serr, err)
		}
	}
	if !slices.Equal(left, []string{qid, qid}) {
		t.Fatalf("left %q", left)
	}
	if _, serr, err := d.run(t, true, "yes\n", "room", "leave", "team"); err == nil || !strings.Contains(err.Error(), "by its id") || strings.Contains(serr, "Leave room") {
		t.Fatalf("shared label: %q %v", serr, err)
	}
}

// F38: a right-to-left room label cannot reorder the sentence a person
// answers: it is shown on its own line, as approvals show it.
func TestLeavePromptIsolatesARightToLeftLabel(t *testing.T) {
	var out bytes.Buffer
	leaveWarning(&out, localapi.Room{ID: "r1", Label: "שלום", Admins: []string{"x"}}, "y")
	first, rest, _ := strings.Cut(out.String(), "\n")
	if strings.Contains(first, "ש") || !strings.Contains(first, "[1] (r1)?") || !strings.HasPrefix(rest, "  [1] שלום") {
		t.Fatalf("prompt: %q", out.String())
	}
}

// F51: with advertise set in config.json, net on lists those addresses
// before the yes, as the daemon gives them instead of the detected ones.
func TestNetOnListsTheConfiguredAdvertisedAddressesBeforeTheYes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("state directory modes are a Unix rule")
	}
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"advertise":["203.0.113.5:7423","[2001:db8::5]:7423"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("A2P2_HOME", home)
	d := newDaemonForCLI(t)
	_, serr, err := d.run(t, true, "no\n", "net", "on")
	if err == nil || !strings.Contains(serr, "203.0.113.5:7423, [2001:db8::5]:7423") || !strings.Contains(serr, "instead of the ones it detects") ||
		strings.Contains(serr, "It advertises these IPv6") || strings.Index(serr, "203.0.113.5") > strings.Index(serr, "Apply this?") {
		t.Fatalf("net on: %q %v", serr, err)
	}
}
