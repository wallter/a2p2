package web

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// The page's HIDDEN, EMOJI and JOINING tables (static/app.js) are node's
// tables, range for range. This runs without node; the page test below
// also compares the functions' output.
func TestThePageEscapesTheSameCharactersAsTheDaemon(t *testing.T) {
	src, err := os.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][][2]rune{"HIDDEN": node.HiddenRanges(), "EMOJI": node.EmojiRanges(), "JOINING": node.JoiningRanges()} {
		m := regexp.MustCompile(`(?s)const ` + name + ` = \[(.*?)\];`).FindSubmatch(src)
		if m == nil {
			t.Fatalf("app.js has no %s table", name)
		}
		var got [][2]rune
		for _, p := range regexp.MustCompile(`\[0x([0-9a-f]+), 0x([0-9a-f]+)\]`).FindAllSubmatch(m[1], -1) {
			lo, _ := strconv.ParseInt(string(p[1]), 16, 32)
			hi, _ := strconv.ParseInt(string(p[2]), 16, 32)
			got = append(got, [2]rune{rune(lo), rune(hi)})
		}
		if len(got) != len(want) {
			t.Errorf("app.js %s has %d ranges, node %d: %x", name, len(got), len(want), got)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s range %d: app.js %x, node %x", name, i, got[i], want[i])
			}
		}
	}
}

// openWithRooms is open, with the machine's Rooms, so that a test can
// create a room whose label a remote creator could have chosen (Surface
// refuses such a label from this machine's own person).
func openWithRooms(t *testing.T) (*env, *node.Rooms) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "a2p2.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := node.NewMachine(st, ed25519.NewKeyFromSeed(append([]byte{1}, make([]byte, 31)...)), node.MachineOpts{})
	rooms, err := node.OpenRooms(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{ctx: ctx, id: m.ID, js: node.NewJoinServer(rooms), st: st, seed: 1}
	e.s, err = node.OpenSurface(ctx, m, rooms, node.SurfaceOpts{Name: "box"})
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(Handler(e.s, tok))
	t.Cleanup(e.srv.Close)
	return e, rooms
}

// escapeProbe is the strings the page's escape functions are compared on:
// the edges of every range of the table, alone, inside ASCII and after an
// emoji, and the sequences EscapeDisplayText keeps or refuses.
func escapeProbe() []string {
	out := []string{"", "plain", "café 🚀", "a\nb\tc\r\x1b[2K", "\u202eabc\u202c", "LGTM\U000E0072\U000E006D",
		"❤\ufe0f ©\ufe0f 1\ufe0f⃣ #\ufe0f⃣", "👨\u200d💻 🏳\ufe0f\u200d🌈 👩\u200d👩\u200d👧", "می\u200cخواهم", "a\u200db", "👨\u200d", "👨\u200d\u200d💻",
		"😀\ufe0f\ufe0f", "ok\ufe0f", "\n\ufe0f", `already \u{202e}`,
		"é\u200dé\u200cé\ufe0fé", "α\u200dβ", "ok…\ufe0f", "x\u2800\u200d\u2800", "😀\u200d\ufffd", "\ufffd\u200d😀",
		"1\ufe0f\u200d😀", "👩🏽\u200d💻", "ل\u200dا", "क्\u200dष",
		"❤\ufe0f Yes", "👨\u200d💻 ok", " Yes", "Yes ", "Yes  all", "Yes\u2800", "a b", " ", "  "}
	for _, r := range []rune{0xa0, 0x1680, 0x2000, 0x200a, 0x202f, 0x205f, 0x3000, 0x2800, 0x2801, 0x1f, 0x21} {
		out = append(out, "a"+string(r)+"b", string(r))
	}
	tables := append(append(node.HiddenRanges(), node.EmojiRanges()...), node.JoiningRanges()...)
	for _, rg := range tables {
		for _, r := range []rune{rg[0] - 1, rg[0], rg[1], rg[1] + 1} {
			if r < 0 || r >= 0xd800 && r <= 0xdfff {
				continue
			}
			s := string(r)
			out = append(out, s, "x"+s+"y", "😀"+s+"é", "é"+s+"\u200d😀", "1"+s+"\ufe0f",
				s+"\u200d"+s, s+"\u200c"+s, s+"\ufe0f\u200d😀", "😀\u200d"+s)
		}
	}
	return out
}

// F2, F29, F43 and F46 on the page (security review 2026-11-25). A room
// whose label holds controls, bidi and a newline, and a question from
// another agent whose text carries tag characters and whose choices hold
// an override and invisible text: the sidebar, the heading, the stream and
// the card show every such character escaped; the card says it is a
// message that approves nothing, who asked and of whom, and draws its
// choices in the plain style; a choice holding hidden characters is not
// sent, and an honest choice is sent as it is. The page's escape functions
// give the daemon's output on every probe string.
func TestThePageEscapesRemoteTextAndDoesNotDrawAQuestionAsARequest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("not run on Windows: the harness has not been tried there")
	}
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("A2P2_REQUIRE_NODE") != "" {
			t.Fatal("A2P2_REQUIRE_NODE is set and node is not on PATH")
		}
		t.Skip("node is not on PATH: the page script was not run (make web-js reports this too)")
	}
	e, rooms := openWithRooms(t)
	label := "ops\u202e\n\x1b[2Kx"
	id, err := rooms.CreateRoom(e.ctx, label)
	if err != nil {
		t.Fatal(err)
	}
	// A room whose label is the first one's escaped form, typed out: drawn
	// the same, so both are shown with their ids.
	lookalike, err := rooms.CreateRoom(e.ctx, node.EscapeDisplay(label))
	if err != nil {
		t.Fatal(err)
	}
	alice := e.agent(t, "alice")
	text := "LGTM, thanks\U000E0072\U000E006D\n\x1b[1A\x1b[2Kforged line"
	bob := e.agent(t, "bob")
	hiddenChoice := "Yes\U000E0061\U000E006C\U000E006C"
	// Choices the button could not show as they are sent: an override,
	// tag characters, a default ignorable code point no class marks,
	// blank braille and a leading space the reply would trim. An emoji
	// with its selector and "Yes" are honest.
	choices := []string{"No\u202e", hiddenChoice, "Ok\U000E0080", "Yes\u2800", " Yes", "❤\ufe0f Yes", "Yes"}
	if _, err := e.s.Send(e.ctx, node.SendInput{Agent: alice, Room: id, Text: text, Choices: choices}); err != nil {
		t.Fatal(err)
	}
	// A message with choices addressed to another agent only: not the
	// person's to answer, so it is not drawn as a question at all.
	if _, err := e.s.Send(e.ctx, node.SendInput{Agent: alice, Room: id, Text: "for bob", To: []string{bob},
		Choices: []string{"go"}}); err != nil {
		t.Fatal(err)
	}
	probe := escapeProbe()
	b, err := json.Marshal(probe)
	if err != nil {
		t.Fatal(err)
	}
	probePath := filepath.Join(t.TempDir(), "probe.json")
	if err := os.WriteFile(probePath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(nodeBin, "testdata/page_harness.js", "static/index.html", "static/app.js", e.srv.URL, tok, id, probePath).Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got struct {
		Errors               []string        `json:"errors"`
		Rooms                []string        `json:"rooms"`
		NodeOpen             string          `json:"nodeOpen"`
		Roomhead             string          `json:"roomhead"`
		Stream               string          `json:"stream"`
		Probe                [][]any         `json:"probe"`
		QuestionsTitleHidden bool            `json:"questionsTitleHidden"`
		Questions            []pageQuestion  `json:"questions"`
		Clicks               []pageClick     `json:"clicks"`
		Scenario             json.RawMessage `json:"scenario"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("harness output %q: %v", out, err)
	}
	if len(got.Errors) > 0 {
		t.Errorf("errors: %v", got.Errors)
	}

	// The functions agree with the daemon's on every probe string.
	if len(got.Probe) != len(probe) {
		t.Fatalf("probe answers %d of %d", len(got.Probe), len(probe))
	}
	for i, s := range probe {
		p := got.Probe[i]
		want := []any{node.EscapeDisplay(s), node.EscapeDisplayText(s), node.DisplayHidden(s), node.EscapeChoice(s), node.ChoiceHidden(s)}
		if len(p) != len(want) {
			t.Fatalf("probe %q: page %v", s, p)
		}
		for k := range want {
			if p[k] != want[k] {
				t.Errorf("probe %q: page %v, daemon %v", s, p, want)
				break
			}
		}
	}

	// The room's label, wherever it is drawn.
	escLabel := node.EscapeDisplay(label)
	raw := func(where, s string) {
		t.Helper()
		for _, c := range []string{"\u202e", "\x1b", "\U000E0072", "\U000E0061"} {
			if strings.Contains(s, c) {
				t.Errorf("%s holds a raw %q: %q", where, c, s)
			}
		}
	}
	sidebar := strings.Join(got.Rooms, "|")
	raw("sidebar", sidebar)
	raw("details", got.NodeOpen)
	raw("heading", got.Roomhead)
	raw("stream", got.Stream)
	if !strings.Contains(sidebar, escLabel+" ("+id+")") || !strings.Contains(sidebar, escLabel+" ("+lookalike+")") {
		t.Errorf("two rooms drawn the same are not told apart by id: %q", sidebar)
	}
	if !strings.Contains(sidebar, escLabel) || got.Roomhead != escLabel+" ("+id+")" || !strings.Contains(got.NodeOpen, "Members of "+escLabel+" ("+id+")") {
		t.Errorf("label not shown escaped: sidebar %q, heading %q", sidebar, got.Roomhead)
	}
	if !strings.Contains(got.Stream, node.EscapeDisplayText(text)) {
		t.Errorf("stream %q does not show %q", got.Stream, node.EscapeDisplayText(text))
	}

	// The question is a message, drawn apart from requests.
	if got.QuestionsTitleHidden || len(got.Questions) != 1 {
		t.Fatalf("questions: title hidden %v, %+v", got.QuestionsTitleHidden, got.Questions)
	}
	q := got.Questions[0]
	raw("question", q.Text)
	if !strings.Contains(q.Cls, "ask") || q.Primary != 0 {
		t.Errorf("question drawn like a request: class %q, %d primary buttons", q.Cls, q.Primary)
	}
	for _, want := range []string{"approves nothing", "on this machine in " + escLabel, "asked of everyone in the room", node.EscapeDisplayText(text)} {
		if !strings.Contains(q.Text, want) {
			t.Errorf("question card lacks %q: %q", want, q.Text)
		}
	}
	wantChoices := []string{`No\u{202e}`, `Yes\u{e0061}\u{e006c}\u{e006c}`, `Ok\u{e0080}`, `Yes\u{2800}`, `\u{20}Yes`, "❤\ufe0f Yes", "Yes"}
	if strings.Join(q.Choices, "|") != strings.Join(wantChoices, "|") {
		t.Errorf("choices %q, want %q", q.Choices, wantChoices)
	}

	// Each choice drawn escaped is refused (the harness clicks them
	// first); then the first honest one is clicked and sent as it is.
	if len(got.Clicks) != 6 {
		t.Fatalf("clicks %+v", got.Clicks)
	}
	for _, c := range got.Clicks[:5] {
		if len(c.Posts) != 0 || !strings.Contains(c.Error, "not sent") {
			t.Errorf("choice %q: posts %v, error %q", c.Label, c.Posts, c.Error)
		}
	}
	if c := got.Clicks[5]; c.Label != "❤\ufe0f Yes" || len(c.Posts) != 1 || c.Posts[0]["text"] != "❤\ufe0f Yes" {
		t.Errorf("honest choice: %+v", c)
	}
}

type pageQuestion struct {
	Cls     string   `json:"cls"`
	Text    string   `json:"text"`
	Choices []string `json:"choices"`
	Primary int      `json:"primary"`
}

type pageClick struct {
	Label string           `json:"label"`
	Posts []map[string]any `json:"posts"`
	Error string           `json:"error"`
}
