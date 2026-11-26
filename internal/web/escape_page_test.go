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
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// The page's HIDDEN, EMOJI, JOINING, RTL, SPACELESS and HAN tables
// (static/app.js) are node's tables, range for range. This runs without
// node; the page test below also compares the functions' output.
func TestThePageEscapesTheSameCharactersAsTheDaemon(t *testing.T) {
	src, err := os.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][][2]rune{"HIDDEN": node.HiddenRanges(), "EMOJI": node.EmojiRanges(), "JOINING": node.JoiningRanges(),
		"RTL_LETTERS": node.RTLLetterRanges(), "SPACELESS": node.SpacelessRanges(), "HAN": node.HanRanges()} {
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
	e.ss = testSessions()
	e.srv = httptest.NewServer(Handler(e.s, e.ss))
	t.Cleanup(e.srv.Close)
	return e, rooms
}

// escapeCase is one row of the table of test strings the daemon's tests
// share (internal/node/testdata/escape_cases.json).
type escapeCase struct {
	Name   string  `json:"name"`
	In     string  `json:"in"`
	Line   *string `json:"line"`
	Text   *string `json:"text"`
	Choice *string `json:"choice"`
	Label  *string `json:"label"`
}

func escapeCases(t *testing.T) []escapeCase {
	t.Helper()
	b, err := os.ReadFile("../node/testdata/escape_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cs []escapeCase
	if err := json.Unmarshal(b, &cs); err != nil {
		t.Fatal(err)
	}
	return cs
}

// escapeProbe is the strings the page's escape functions are compared on:
// the edges of every range of the tables, alone, inside ASCII and after an
// emoji, the sequences the text rules keep or refuse, and the inputs of
// the shared table.
func escapeProbe(cases []escapeCase) []string {
	out := []string{"", "plain", "café 🚀", "a\nb\tc\r\x1b[2K", "\u202eabc\u202c", "LGTM\U000E0072\U000E006D",
		"❤\ufe0f ©\ufe0f 1\ufe0f⃣ #\ufe0f⃣", "👨\u200d💻 🏳\ufe0f\u200d🌈 👩\u200d👩\u200d👧", "می\u200cخواهم", "a\u200db", "👨\u200d", "👨\u200d\u200d💻",
		"😀\ufe0f\ufe0f", "ok\ufe0f", "\n\ufe0f", `already \u{202e}`,
		"é\u200dé\u200cé\ufe0fé", "α\u200dβ", "ok…\ufe0f", "x\u2800\u200d\u2800", "😀\u200d\ufffd", "\ufffd\u200d😀",
		"1\ufe0f\u200d😀", "👩🏽\u200d💻", "ل\u200dا", "क्\u200dष",
		"❤\ufe0f Yes", "👨\u200d💻 ok", " Yes", "Yes ", "Yes  all", "Yes\u2800", "a b", " ", "  "}
	for _, r := range []rune{0xa0, 0x1680, 0x2000, 0x200a, 0x202f, 0x205f, 0x3000, 0x2800, 0x2801, 0x1f, 0x21} {
		out = append(out, "a"+string(r)+"b", string(r))
	}
	// Every white space character and its neighbours, in a label as the
	// page and status compare labels (drawnLabel, node.LabelKey).
	for r := rune(0); r <= 0x3001; r++ {
		if unicode.Is(unicode.White_Space, r) || r == 0x2800 {
			out = append(out, " A"+string(r-1)+string(r)+" \u200b"+string(r)+"b"+string(r+1)+" ")
		}
	}
	for _, c := range cases {
		out = append(out, c.In)
	}
	tables := append(append(node.HiddenRanges(), node.EmojiRanges()...), node.JoiningRanges()...)
	tables = append(append(append(tables, node.RTLLetterRanges()...), node.SpacelessRanges()...), node.HanRanges()...)
	for _, rg := range tables {
		for _, r := range []rune{rg[0] - 1, rg[0], rg[1], rg[1] + 1} {
			if r < 0 || r >= 0xd800 && r <= 0xdfff {
				continue
			}
			s := string(r)
			out = append(out, s, "x"+s+"y", "😀"+s+"é", "é"+s+"\u200d😀", "1"+s+"\ufe0f",
				s+"\u200d"+s, s+"\u200c"+s, s+"\ufe0f\u200d😀", "😀\u200d"+s,
				s+"\u200f"+s, s+"\u061c"+s, s+"\u200b"+s, s+"\U000E0100", s+"\r\n"+s)
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
	// apart from it (E2), so neither needs its id for that.
	typed, err := rooms.CreateRoom(e.ctx, node.EscapeDisplay(label))
	if err != nil {
		t.Fatal(err)
	}
	// Rooms whose labels the page draws alike: "café" composed and
	// decomposed, and labels that differ in a run of spaces or in a
	// no-break or ideographic space. Each is shown with its id.
	alike := map[string]string{}
	for _, l := range []string{"caf\u00e9", "cafe\u0301", "on  call", "on call", "on\u00a0call", "on\u3000call"} {
		id, err := rooms.CreateRoom(e.ctx, l)
		if err != nil {
			t.Fatal(err)
		}
		alike[id] = l
	}
	// A room whose label is right-to-left text, drawn in a bdi element.
	rtl := "\u05e9\u05dc\u05d5\u05dd (prod)"
	rtlID, err := rooms.CreateRoom(e.ctx, rtl)
	if err != nil {
		t.Fatal(err)
	}
	// A machine asks to join it: the request card draws the label and the
	// machine's name each in a bdi element.
	inv, err := rooms.Invite(e.ctx, rtlID, node.RoleMember, false, []string{"127.0.0.1:7443"})
	if err != nil {
		t.Fatal(err)
	}
	joiner := open(t, 2, e)
	p, err := joiner.s.JoinPreview(joiner.ctx, inv.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := joiner.s.JoinAccept(joiner.ctx, inv.Token, p.ID, 0); err != nil {
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
	cases := escapeCases(t)
	probe := escapeProbe(cases)
	// Sentences as the daemon writes a request's text: the page draws each
	// isolated part in a bdi element and escapes a raw control outside one.
	sentences := []string{
		"add machine 0123 (" + node.Isolate("bob") + ") to room " + node.Isolate(node.EscapeDisplay(rtl)) + " (r_1) as a member",
		"plain\x1b[2K",
	}
	b, err := json.Marshal(map[string]any{"strings": probe, "sentences": sentences})
	if err != nil {
		t.Fatal(err)
	}
	probePath := filepath.Join(t.TempDir(), "probe.json")
	if err := os.WriteFile(probePath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(nodeBin, "testdata/page_harness.js", "static/index.html", "static/app.js", e.srv.URL, "c="+e.ss.Code(), id, probePath).Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got struct {
		Errors    []string `json:"errors"`
		Rooms     []string `json:"rooms"`
		NodeOpen  string   `json:"nodeOpen"`
		Roomhead  string   `json:"roomhead"`
		Stream    string   `json:"stream"`
		Probe     [][]any  `json:"probe"`
		Sentences []struct {
			Text string   `json:"text"`
			BDI  []string `json:"bdi"`
		} `json:"sentences"`
		BDI                  []string        `json:"bdi"`
		RequestBDI           [][]string      `json:"requestBDI"`
		QuestionBDI          []string        `json:"questionBDI"`
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
		want := []any{node.EscapeDisplay(s), node.EscapeDisplayText(s), node.DisplayHidden(s), node.EscapeChoice(s), node.ChoiceHidden(s),
			node.LabelKey(s)}
		if len(p) != len(want)+1 {
			t.Fatalf("probe %q: page %v", s, p)
		}
		// drawnLabel normalises to NFC and node.LabelKey does not: they
		// are compared where that changes nothing.
		if p[len(want)] != true {
			want[len(want)-1] = p[len(want)-1]
		}
		for k := range want {
			if p[k] != want[k] {
				t.Errorf("probe %q: page %v, daemon %v", s, p, want)
				break
			}
		}
	}

	// The page gives the shared table's answers.
	page := map[string][]any{}
	for i, s := range probe {
		page[s] = got.Probe[i]
	}
	for _, c := range cases {
		p := page[c.In]
		for k, want := range map[int]*string{0: c.Line, 1: c.Text, 3: c.Choice, 5: c.Label} {
			if want != nil && p[k] != *want {
				t.Errorf("%s: the page gives %q, the table %q", c.Name, p[k], *want)
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
	if strings.Contains(sidebar, " ("+id+")") || strings.Contains(sidebar, " ("+typed+")") ||
		!strings.Contains(sidebar, node.EscapeDisplay(escLabel)) {
		t.Errorf("a label and its escaped form typed out are not drawn apart: %q", sidebar)
	}
	for aid, l := range alike {
		if !strings.Contains(sidebar, node.EscapeDisplay(l)+" ("+aid+")") {
			t.Errorf("label %q drawn like another is not shown with its id: %q", l, sidebar)
		}
	}
	if !slices.Contains(got.BDI, rtl) || !slices.Contains(got.BDI, escLabel) {
		t.Errorf("labels not set apart in bdi elements: %q", got.BDI)
	}
	// The request card draws the daemon's sentence with its parts in bdi
	// elements, escaped; so does the line that says who asked a question.
	if len(got.RequestBDI) != 1 || !slices.Equal(got.RequestBDI[0], []string{"joiner", node.EscapeDisplay(rtl)}) {
		t.Errorf("request card bdi %q, want the machine's name and the room's label", got.RequestBDI)
	}
	if !slices.Equal(got.QuestionBDI, []string{node.EscapeDisplay(alice), "this machine", escLabel}) {
		t.Errorf("question card bdi %q, want the agent, its machine and the room's label", got.QuestionBDI)
	}
	if len(got.Sentences) != 2 || got.Sentences[0].Text != "add machine 0123 (bob) to room "+rtl+" (r_1) as a member" ||
		!slices.Equal(got.Sentences[0].BDI, []string{"bob", rtl}) || got.Sentences[1].Text != `plain\u{1b}[2K` {
		t.Errorf("sentences drawn %+v", got.Sentences)
	}
	if !strings.Contains(sidebar, escLabel) || got.Roomhead != escLabel || !strings.Contains(got.NodeOpen, "Members of "+escLabel) {
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
