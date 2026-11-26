package node

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/wallter/a2p2/internal/store"
)

// The letters the text rules let a right-to-left mark stand between are
// letters of Go's Unicode tables and of the right-to-left scripts, so the
// mark between two of them changes no ordering.
func TestRightToLeftLettersAreLettersOfRightToLeftScripts(t *testing.T) {
	prev := rune(-1)
	for _, rg := range rtlLetterRanges {
		if rg[0] > rg[1] || rg[0] <= prev {
			t.Errorf("range %x-%x is out of order or overlaps", rg[0], rg[1])
		}
		prev = rg[1]
		for r := rg[0]; r <= rg[1]; r++ {
			if !unicode.IsLetter(r) || !inRanges(rtlRanges, r) || Hidden(r) {
				t.Errorf("U+%04X is not a right-to-left letter", r)
			}
		}
	}
}

// unescape reads back what the helpers write: \u{hex} is that code point
// and \x{hh} that byte; anything else is itself.
func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\\' && i+2 < len(s) && (s[i+1] == 'u' || s[i+1] == 'x') && s[i+2] == '{' {
			if end := strings.IndexByte(s[i:], '}'); end > 3 {
				if v, err := strconv.ParseUint(s[i+3:i+end], 16, 32); err == nil {
					if s[i+1] == 'u' {
						b.WriteRune(rune(v))
					} else {
						b.WriteByte(byte(v))
					}
					i += end + 1
					continue
				}
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// keptInvisible reports whether r is a character of the table that the
// text rules may keep where it stands.
func keptInvisible(r rune) bool {
	switch {
	case r == 0x200b, r == 0x200c, r == 0x200d, r == 0x200f, r == 0x061c, r == 0xfe0e, r == 0xfe0f,
		r >= 0xe0100 && r <= 0xe01ef, r >= 0xe0061 && r <= 0xe007a, r == 0xe007f:
		return true
	}
	return false
}

// What the helpers write is valid UTF-8, holds only the hidden
// characters the text rules may keep, and reads back as the string given
// (message text with each CRLF as one line break): no two strings are
// shown alike, a literal escape among them.
func FuzzEscapeDisplay(f *testing.F) {
	seeds := []string{"", "ok", "\x1b[2K\u202e\n", "👨\u200d💻\ufe0f\ufe0f", "\xff\xed\xa0\x80", "a\U000E0041\u200d",
		"\\u{202e}", "\\\\u{", "a\r\nb", "🏴" + tagged("gbeng"), "葛\U000E0100", "של\u200fום"}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		one := EscapeDisplay(s)
		if !utf8.ValidString(one) || strings.ContainsAny(one, "\n\t\r") {
			t.Fatalf("EscapeDisplay(%q) = %q", s, one)
		}
		for _, r := range one {
			if Hidden(r) && !keptInvisible(r) {
				t.Fatalf("EscapeDisplay(%q) kept U+%04X", s, r)
			}
		}
		if unescape(one) != s {
			t.Fatalf("EscapeDisplay(%q) = %q reads back as %q", s, one, unescape(one))
		}
		if DisplayHidden(s) != (one != s) {
			t.Fatalf("DisplayHidden(%q)", s)
		}
		if lines := EscapeDisplayLines(s); strings.Count(lines, "\n") != strings.Count(s, "\n") {
			t.Fatalf("EscapeDisplayLines(%q) = %q", s, lines)
		}
		if c := EscapeChoice(s); !utf8.ValidString(c) || strings.ContainsAny(c, "\n\t") {
			t.Fatalf("EscapeChoice(%q) = %q", s, c)
		}
		text := EscapeDisplayText(s)
		if !utf8.ValidString(text) {
			t.Fatalf("EscapeDisplayText(%q) is not UTF-8", s)
		}
		for _, r := range text {
			if Hidden(r) && r != '\n' && r != '\t' && !keptInvisible(r) {
				t.Fatalf("EscapeDisplayText(%q) kept U+%04X", s, r)
			}
		}
		if unescape(text) != strings.ReplaceAll(s, "\r\n", "\n") {
			t.Fatalf("EscapeDisplayText(%q) = %q reads back as %q", s, text, unescape(text))
		}
	})
}

// escapeCase is one row of testdata/escape_cases.json, the table of test
// strings that the page's tests (internal/web) share: what EscapeDisplay
// (line), EscapeDisplayText (text) and EscapeChoice (choice) write for
// in, and the LabelKey of in (label), where the row says.
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
	b, err := os.ReadFile("testdata/escape_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cs []escapeCase
	if err := json.Unmarshal(b, &cs); err != nil {
		t.Fatal(err)
	}
	return cs
}

// E1 and E2 of the second security review (2026-11-25): honest emoji,
// joining scripts, right-to-left marks and Thai word breaks between
// letters, the flags of England, Scotland and Wales, Japanese variation
// sequences and CRLF line ends are shown as written, in labels and file
// names too; a mark, space or tag anywhere else is escaped; a literal
// escape is shown apart from an escaped character.
func TestEscapeCasesShowHonestTextAsWrittenAndEveryEscapeApart(t *testing.T) {
	cs := escapeCases(t)
	if len(cs) < 30 {
		t.Fatalf("%d cases", len(cs))
	}
	for _, c := range cs {
		for _, x := range []struct {
			fn   string
			want *string
			got  string
		}{{"EscapeDisplay", c.Line, EscapeDisplay(c.In)}, {"EscapeDisplayText", c.Text, EscapeDisplayText(c.In)},
			{"EscapeChoice", c.Choice, EscapeChoice(c.In)}} {
			if x.want != nil && x.got != *x.want {
				t.Errorf("%s: %s(%q) = %q, want %q", c.Name, x.fn, c.In, x.got, *x.want)
			}
		}
	}
}

// E2 of the third review (2026-11-25): two labels compared as drawn
// collapsed only ASCII blanks, so a no-break space, an ideographic space
// or another Unicode space let two labels draw alike and get no ids. Every
// white space character and blank braille now count as a space; a
// character that is escaped is not blank, and an honest label keeps its
// key. The page's drawnLabel gives the table's keys too (internal/web).
func TestLabelKeyCountsEveryUnicodeSpaceAsASpace(t *testing.T) {
	n := 0
	for _, c := range escapeCases(t) {
		if c.Label == nil {
			continue
		}
		n++
		if got := LabelKey(c.In); got != *c.Label {
			t.Errorf("%s: LabelKey(%q) = %q, want %q", c.Name, c.In, got, *c.Label)
		}
	}
	if n < 10 {
		t.Fatalf("%d label cases", n)
	}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.Is(unicode.White_Space, r) && !Hidden(r) && LabelKey("a"+string(r)+"b") != "a b" {
			t.Errorf("U+%04X is not a space in a label: %q", r, LabelKey("a"+string(r)+"b"))
		}
	}
}

// E2: a literal escape typed into a label no longer reads as the
// character it names.
func TestALiteralEscapeIsNotShownAsTheCharacterItNames(t *testing.T) {
	real, typed := EscapeDisplay("ops\u202e"), EscapeDisplay(EscapeDisplay("ops\u202e"))
	if real == typed {
		t.Fatalf("both shown as %q", real)
	}
	if unescape(typed) != EscapeDisplay("ops\u202e") {
		t.Fatalf("%q does not read back as the text typed", typed)
	}
}

// E3: the question alert was built raw from the asker's text, so a
// terminal escape in a question reached a2p2 alerts and the hooks as
// sent. It is escaped as message text now; an honest question reads as
// written.
func TestAQuestionAlertEscapesTheAskersText(t *testing.T) {
	ctx := context.Background()
	e := newSurf(t)
	alice := e.agent("alice")
	e.send(SendInput{Agent: alice, Text: "Ship it?\x1b[2K\u202e ❤\ufe0f\nnow", To: []string{e.s.HumanID()}})
	as, err := e.s.Alerts(ctx, 0)
	if err != nil || len(as) != 1 || as[0].Text != alice+` [this machine] asks: Ship it?\u{1b}[2K\u{202e} ❤`+"\ufe0f\nnow" {
		t.Fatalf("alerts: %+v %v", as, err)
	}
}

// E2: a room label and the names in the text a person approves are
// isolated, so right-to-left text in them cannot reorder the words and
// punctuation around them; the same in a send with files.
func TestARightToLeftLabelIsIsolatedInTheTextAPersonApproves(t *testing.T) {
	label := "שלום (prod)"
	got := requestText(store.Request{Kind: "add", Action: ActionAdmit, Room: "r_1", Node: "0123", Name: "bob"}, label, nil)
	if want := "add machine 0123 (" + Isolate("bob") + ") to room " + Isolate(label) + " (r_1) as a member, without earlier history"; got != want {
		t.Fatalf("request text %q, want %q", got, want)
	}
	sv := &SendView{Agent: "alice@01234567", To: []string{"bob@89abcdef"}, Files: []SendFileView{{}}}
	if got := sendText(sv, label, "r_1", 3); !strings.Contains(got, Isolate("bob@89abcdef")+" in room "+Isolate(label)+" (r_1)") ||
		!strings.Contains(got, "by agent "+Isolate("alice@01234567")) {
		t.Fatalf("send text %q", got)
	}
}

// E2: a remote string in a sentence a person approves is isolated, and a
// terminal finds the pieces again.
func TestIsolatedFindsThePiecesIsolateWrote(t *testing.T) {
	text := "add machine 0123 (" + Isolate("bob") + ") to room " + Isolate("שלום, team") + " (r_1)"
	got := Isolated(text)
	want := []Part{{"add machine 0123 (", false}, {"bob", true}, {") to room ", false}, {"שלום, team", true}, {" (r_1)", false}}
	if !slices.Equal(got, want) {
		t.Fatalf("Isolated(%q) = %+v", text, got)
	}
	if RightToLeft("bob") || !RightToLeft("שלום, team") || !RightToLeft("عربي") {
		t.Error("RightToLeft")
	}
	if got := Isolated("a" + string(isoOpen) + "b"); !slices.Equal(got, []Part{{"a", false}, {"b", true}}) {
		t.Errorf("an open isolate: %+v", got)
	}
	// An escaped string holds no isolate, so the pieces are Isolate's.
	if e := EscapeDisplay("x" + string(isoOpen) + "y" + string(isoClose) + "z"); strings.ContainsAny(e, string(isoOpen)+string(isoClose)) {
		t.Errorf("EscapeDisplay kept an isolate: %q", e)
	}
}
