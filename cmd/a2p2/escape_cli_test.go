package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
)

// rawHiding are characters that must never reach the terminal raw from
// another machine: ESC, CR, a right-to-left override and a tag.
var rawHiding = []string{"\x1b", "\r", "\u202e", "\U000E0061"}

func holdsRaw(t *testing.T, what, s string) {
	t.Helper()
	for _, c := range rawHiding {
		if strings.Contains(s, c) {
			t.Errorf("%s holds a raw %q: %q", what, c, s)
		}
	}
}

// E3 of the second security review (2026-11-25): an entry's author,
// addressees, text, choices and reason come from other machines, and
// printEntry wrote them raw to the terminal, where ESC sequences move the
// cursor and erase lines. Each is now escaped; honest text (an emoji
// sequence, Persian, a CRLF line end) is shown as written.
func TestPrintEntryEscapesWhatAnotherMachineWrote(t *testing.T) {
	var out bytes.Buffer
	printEntry(&out, localapi.Entry{ID: "m1", Kind: "message", Created: "2026-11-25T12:00:00Z",
		From: "bot\x1b[2J@1a2b3c4d", To: []string{"you\u202e@1a2b3c4d"},
		Text:    "ok\x1b[1A\x1b[2Kforged\U000E0061\r\nline two ❤\ufe0f تیم\u200cما",
		Choices: []string{"Yes\u202e", "✔\ufe0f Done"}})
	got := out.String()
	holdsRaw(t, "entry", got)
	for _, want := range []string{`bot\u{1b}[2J@1a2b3c4d → you\u{202e}@1a2b3c4d`, `ok\u{1b}[1A\u{1b}[2Kforged\u{e0061}` + "\n    line two ❤\ufe0f تیم\u200cما\n",
		`choices: Yes\u{202e} | ✔` + "\ufe0f Done"} {
		if !strings.Contains(got, want) {
			t.Errorf("entry lacks %q: %q", want, got)
		}
	}
}

// E3: the question alert's text was built raw from the asker's label and
// text (surface.go); the daemon now escapes it, and printAlert keeps each
// of its lines indented; a body's later lines sit deeper than the
// asker's line (D65).
func TestPrintAlertIndentsEveryLineOfAQuestion(t *testing.T) {
	var out bytes.Buffer
	printAlert(&out, localapi.Alert{Kind: node.AlertQuestion, ID: "m1", Room: "r_1", Text: "bot@1a2b3c4d asks: Ship?\nReally"})
	if got := out.String(); !strings.Contains(got, "  bot@1a2b3c4d asks: Ship?\n    Really\n") {
		t.Fatalf("alert %q", got)
	}
}

// E2: a room label of right-to-left text sits in the sentence a person
// approves. A terminal that orders text by direction could move it, and
// the words and punctuation around it, so confirm shows it on its own
// line and a reference in the sentence. An ordinary label stays inline.
func TestApprovalTextShowsARightToLeftLabelOnItsOwnLine(t *testing.T) {
	label := "שלום (prod)"
	q := localapi.Request{ID: "q_1", Text: "add machine 0123 (" + node.Isolate("bob") + ") to room " + node.Isolate(label) + " (r_1) as a member"}
	var out bytes.Buffer
	printTerms(&out, q)
	got := out.String()
	if !strings.Contains(got, "If approved: add machine 0123 (bob) to room [1] (r_1) as a member\n    [1] "+label+"\n") {
		t.Fatalf("terms %q", got)
	}
	if strings.ContainsAny(got, "\u2068\u2069") {
		t.Errorf("isolates printed raw: %q", got)
	}
	out.Reset()
	printTerms(&out, localapi.Request{ID: "q_1", Text: "remove machine 0123 from room " + node.Isolate("ops") + " (r_1)"})
	if got := out.String(); !strings.Contains(got, "If approved: remove machine 0123 from room ops (r_1)\n") || strings.Contains(got, "[1]") {
		t.Fatalf("an ordinary label: %q", got)
	}
}

// E2: the join preview isolates the label the same way.
func TestTheJoinPreviewShowsARightToLeftLabelOnItsOwnLine(t *testing.T) {
	pv := localapi.PreviewResp{Room: localapi.JoinRoom{ID: "r_1", Label: "فريق"}, Role: "member",
		Admin: "0123456789abcdef", Expires: "2026-11-25T12:00:00Z"}
	got := previewText(pv)
	if !strings.HasPrefix(got, `Invite to room "[1]" (r_1), as member`) || !strings.HasSuffix(got, "\n  [1] فريق") {
		t.Fatalf("preview %q", got)
	}
}

// E3: the room list and status name rooms by labels their creators chose,
// perhaps on other machines; both print them escaped.
func TestRoomListAndStatusEscapeARoomLabelFromAnotherMachine(t *testing.T) {
	d := newDaemonForCLI(t)
	hostile := "ops\x1b[2K\u202e\U000E0061"
	if _, err := d.rooms.CreateRoom(context.Background(), hostile); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"room", "list"}, {"status"}} {
		out, _, err := d.run(t, false, "", args[0], args[1:]...)
		if err != nil {
			t.Fatal(err)
		}
		holdsRaw(t, args[0], out)
		if !strings.Contains(out, node.EscapeDisplay(hostile)) || !strings.Contains(out, "team") {
			t.Errorf("%s: %q", args[0], out)
		}
	}
}

// E2 in status: two remote labels that differ only in a character the
// text rules keep raw and that does not show (a ZWSP between Thai
// letters, an RLM between Hebrew letters), in case or in a run of spaces
// print alike, so each is listed with its room's id. So do labels that
// differ in a no-break, ideographic or em space (third review). A label
// unlike the others is listed without one.
func TestStatusShowsTheIdsOfRoomsWhoseLabelsPrintAlike(t *testing.T) {
	d := newDaemonForCLI(t)
	ids := map[string]string{}
	for _, l := range []string{"\u0e17\u0e35\u0e21", "\u0e17\u0e35\u200b\u0e21",
		"\u05e9\u05dc\u05d5\u05dd", "\u05e9\u05dc\u200f\u05d5\u05dd", "On  call", "on call", "on\u00a0call", "on\u3000call",
		"\u3000on\u2003call\u3000", "solo"} {
		id, err := d.rooms.CreateRoom(context.Background(), l)
		if err != nil {
			t.Fatal(err)
		}
		if node.EscapeDisplay(l) != l {
			t.Fatalf("%q is escaped as %q: the test needs a label the rules keep", l, node.EscapeDisplay(l))
		}
		ids[l] = id
	}
	out, _, err := d.run(t, false, "", "status")
	if err != nil {
		t.Fatal(err)
	}
	for l, id := range ids {
		if got := strings.Contains(out, l+" ("+id+")"); got != (l != "solo") {
			t.Errorf("%q listed with its id: %v, want %v\n%s", l, got, l != "solo", out)
		}
	}
}

// E3: --json stays data: the label as stored, escaped by JSON's rules
// only.
func TestRoomListJSONKeepsTheLabelAsStored(t *testing.T) {
	d := newDaemonForCLI(t)
	hostile := "ops\u202e🚀"
	if _, err := d.rooms.CreateRoom(context.Background(), hostile); err != nil {
		t.Fatal(err)
	}
	out, _, err := d.run(t, false, "", "room", "--json", "list")
	if err != nil {
		t.Fatal(err)
	}
	var res localapi.RoomsResp
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("not JSON: %q", out)
	}
	found := false
	for _, r := range res.Rooms {
		found = found || r.Label == hostile
	}
	if !found {
		t.Fatalf("label not kept as stored: %q", out)
	}
}

// confirmBodies records the bodies of confirm calls and, with stale set,
// rewrites the text a decision carries, as if the request had changed
// after it was shown.
type confirmBodies struct {
	mu     sync.Mutex
	bodies []localapi.ConfirmReq
	stale  bool
}

func (cb *confirmBodies) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/confirm") {
			b, _ := io.ReadAll(r.Body)
			var q localapi.ConfirmReq
			_ = json.Unmarshal(b, &q)
			cb.mu.Lock()
			cb.bodies = append(cb.bodies, q)
			if cb.stale {
				q.Text += " (changed)"
				b, _ = json.Marshal(q)
			}
			cb.mu.Unlock()
			r.Body = io.NopCloser(bytes.NewReader(b))
			r.ContentLength = int64(len(b))
		}
		h.ServeHTTP(w, r)
	})
}

// E4 of the second security review (2026-11-25): a2p2 confirm decided by
// id only. It now sends the request's text as it showed it, so the daemon
// refuses a decision on a request that no longer reads that way, as the
// page's confirm does; and with the text unchanged, it decides.
func TestConfirmSendsTheTextItShowedAndDecidesNothingWhenItChanged(t *testing.T) {
	cb := &confirmBodies{stale: true}
	d := newDaemonForCLIWith(t, cb.wrap)
	ctx := context.Background()
	alice, err := d.s.Register(ctx, "test", "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	aliceID := alice.Label + "@" + d.s.HumanID()[len("human@"):]
	p := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(p, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	req, err := d.s.SendFiles(ctx, node.SendInput{Agent: aliceID, Room: "team", Text: "the log"}, []string{p})
	if err != nil {
		t.Fatal(err)
	}
	qs, err := d.s.Waiting(ctx)
	if err != nil || len(qs) != 1 {
		t.Fatalf("waiting %+v %v", qs, err)
	}
	shown := qs[0].Text

	_, _, err = d.run(t, true, "yes\n", "confirm", "--decline", req.Request)
	if err == nil || !strings.Contains(err.Error(), "changed since it was shown; nothing was decided") {
		t.Fatalf("a decision on a changed request: %v", err)
	}
	if qs, _ := d.s.Waiting(ctx); len(qs) != 1 {
		t.Fatalf("decided anyway: %+v", qs)
	}

	cb.mu.Lock()
	cb.stale = false
	cb.mu.Unlock()
	if _, _, err := d.run(t, true, "yes\n", "confirm", "--decline", req.Request); err != nil {
		t.Fatal(err)
	}
	if qs, _ := d.s.Waiting(ctx); len(qs) != 0 {
		t.Fatalf("not decided: %+v", qs)
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if len(cb.bodies) != 2 || cb.bodies[0].Text != shown || cb.bodies[1].Text != shown {
		t.Fatalf("confirm sent %+v, want the text shown %q", cb.bodies, shown)
	}
}

// E3: net check names each member by the room's label and the machine's
// name, which other machines chose, and its advice names rooms by label;
// all are printed escaped.
func TestNetCheckEscapesRoomLabelsAndMachineNames(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	printNetCheck(node.NetCheck{State: node.NetworkOutbound, Members: []node.MemberCheck{{Room: "r_1",
		RoomLabel: "ops\x1b[2K", Node: "0123456789abcdef", Name: "box\x1b]0;x\x07"}},
		Advice: "No member was reached, none in ops\x1b[2K."})
	w.Close()
	os.Stdout = old
	got := <-done
	holdsRaw(t, "net check", got)
	for _, want := range []string{`ops\u{1b}[2K / box\u{1b}]0;x\u{7} (01234567)`, `none in ops\u{1b}[2K.`} {
		if !strings.Contains(got, want) {
			t.Errorf("net check lacks %q: %q", want, got)
		}
	}
}

// stdoutOf runs f with os.Stdout captured and returns what it printed.
func stdoutOf(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	f()
	w.Close()
	os.Stdout = old
	return <-done
}

// noForgedLine fails when a line of out starts with "forged": a line
// break inside a remote string started a line of its own.
func noForgedLine(t *testing.T, what, out string) {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "forged") {
			t.Errorf("%s: a remote string started the line %q:\n%s", what, l, out)
		}
	}
}

// E3 of the third review (2026-11-25): a member's addresses come from
// the address book, which keeps any string of up to 255 bytes another
// member sends, and a probe's detail can quote one; members' dial-back
// errors, what they saw and the router's answers reach the check too.
// net check printed all of them raw. Each is escaped as one line now; an
// honest address reads as written, and --json keeps the data.
func TestNetCheckEscapesMemberAddressesDetailsAndEveryRemoteString(t *testing.T) {
	hostile := "203.0.113.9:1\x1b[2K\u202e\nforged line"
	nc := node.NetCheck{State: node.NetworkOn, Fault: "listen\x1b[1A", Advertised: []string{"198.51.100.1:7423\x1b[2K"},
		Members: []node.MemberCheck{{Room: "r_1", RoomLabel: "ops", Node: "0123456789abcdef", Name: "box",
			Addrs: []node.AddrCheck{{Addr: hostile, Result: node.ProbeUnreachable, MS: 3, Detail: "dial tcp " + hostile},
				{Addr: "203.0.113.10:7423", Result: node.ProbeRefused, MS: 4, Detail: "connect: connection refused"},
				{Addr: "x:1", Result: "odd\x1b[2K", MS: 1}}}},
		Reach: node.Reach{State: node.ReachUnknown, Reached: []string{"a\x1b:1"}, NotTried: []string{"b\u202e:1"},
			Saw: []string{"192.0.2.1\x1b[2J\nforged saw"}, NotChecked: []string{"not checked\x1b"},
			Why: "no member answered the dial-back: " + hostile},
		Mapping:       &node.PortMapping{Error: "router said \x1b]0;x\x07\nforged mapping", NotRemoved: []string{"upnp\x1b"}},
		NotAdvertised: []string{"c\x1b:1"}, Forward: "\"advertise\": [\"d\x1b:1\"]",
		Advice: "No member could be reached, none in ops\nforged advice."}
	got := stdoutOf(t, func() { printNetCheck(nc) })
	holdsRaw(t, "net check", got)
	noForgedLine(t, "net check", got)
	esc := node.EscapeDisplay(hostile)
	for _, want := range []string{"  " + esc + " unreachable in 3 ms: dial tcp " + esc,
		"203.0.113.10:7423            refused in 4 ms: connect: connection refused", `odd\u{1b}[2K`,
		`router said \u{1b}]0;x\u{7}\u{a}forged mapping`, `192.0.2.1\u{1b}[2J\u{a}forged saw`,
		`none in ops\u{a}forged advice.`, "(no member answered the dial-back: " + esc + ")"} {
		if !strings.Contains(got, want) {
			t.Errorf("net check lacks %q:\n%s", want, got)
		}
	}

	// The HOST:PORT form, through the command: the address and the
	// probe's error that quotes it are escaped; --json keeps them.
	arg := "bad\x1b[2K\u202e.invalid:1"
	out, err := cli(t, "net", "check", arg)
	if err != nil {
		t.Fatal(err)
	}
	holdsRaw(t, "net check HOST:PORT", out)
	if !strings.Contains(out, node.EscapeDisplay(arg)) {
		t.Errorf("the address is not shown escaped: %q", out)
	}
	out, err = cli(t, "net", "check", "--json", arg)
	if err != nil {
		t.Fatal(err)
	}
	var res addrCheckOut
	if err := json.Unmarshal([]byte(out), &res); err != nil || len(res.Addrs) != 1 || res.Addrs[0].Addr != arg {
		t.Fatalf("--json: %q %v", out, err)
	}
}

// E3: other print sites that show text from other machines: the terms
// of a send with files (the author of the parent), a fetch's reason and
// the members' answers, and the last line of an error. An honest key and
// time stamp read as before.
func TestOtherPrintSitesEscapeWhatAnotherMachineWrote(t *testing.T) {
	var out bytes.Buffer
	printSendTerms(&out, localapi.Request{Agent: "alice@01234567", RoomLabel: "ops", Room: "r_1",
		ReplyTo: "m1\x1b[2K", ReplyAuthor: "bob\x1b[2J\u202e@89abcdef", Body: "hi", Terms: "t"})
	holdsRaw(t, "send terms", out.String())
	if !strings.Contains(out.String(), `to        bob\u{1b}[2J\u{202e}@89abcdef, the author of message m1\u{1b}[2K`) {
		t.Errorf("send terms %q", out.String())
	}
	out.Reset()
	printFetch(&out, io.Discard, localapi.FetchResp{State: node.FetchUnavailable, Reason: "member said \x1b[2K\nforged",
		Holders: []localapi.Holder{{Node: "\x1b[2J0123456789", Answer: "no\u202e", At: "never\x1b"}}})
	holdsRaw(t, "fetch", out.String())
	noForgedLine(t, "fetch", out.String())
	if short("0123456789abcdef") != "01234567" || clock("2026-11-25T12:00:00Z") == "" {
		t.Error("an honest key or time stamp is changed")
	}
	if got := errorText(errors.New("refused: \x1b[2K\nforged")); got != `refused: \u{1b}[2K\u{a}forged` {
		t.Errorf("error %q", got)
	}
}
