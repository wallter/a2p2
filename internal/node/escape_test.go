package node

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestEscapeDisplayWritesEveryHidingCharacterAsAnEscape(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain/path.txt", "plain/path.txt"},
		{"日本語.txt", "日本語.txt"},
		{"café 🚀", "café 🚀"},
		{`C:\Users\a\b.txt`, `C:\Users\a\b.txt`},
		// controls, line breaks and terminal escapes
		{"a\nb\tc\x1b[2J", `a\u{a}b\u{9}c\u{1b}[2J`},
		{"\r\x1b[2Kok\x07", `\u{d}\u{1b}[2Kok\u{7}`},
		{"\x1b]52;c;aGk=\x07", `\u{1b}]52;c;aGk=\u{7}`},
		{"x\u0085y\u009b", `x\u{85}y\u{9b}`},
		{"del\x7f", `del\u{7f}`},
		{"a\u2028b\u2029c", `a\u{2028}b\u{2029}c`},
		// bidirectional marks, embeddings, overrides and isolates
		{"\u200e\u200f\u061c", `\u{200e}\u{200f}\u{61c}`},
		{"\u202a\u202e\u2066\u2069", `\u{202a}\u{202e}\u{2066}\u{2069}`},
		{"report\u202etxt.exe", `report\u{202e}txt.exe`},
		// characters that draw as nothing
		{"a\u200bb\u200cc\u200dd", `a\u{200b}b\u{200c}c\u{200d}d`},
		{"\u2060\u2061\u2064\ufeff\u00ad\u034f", `\u{2060}\u{2061}\u{2064}\u{feff}\u{ad}\u{34f}`},
		{"\u115f\u1160\u3164\uffa0\u17b4\u180e", `\u{115f}\u{1160}\u{3164}\u{ffa0}\u{17b4}\u{180e}`},
		{"ok\U000E0068\U000E0069", `ok\u{e0068}\u{e0069}`},
		{"❤\ufe0f", `❤\u{fe0f}`},
		{"x\U000E0100", `x\u{e0100}`},
		// default ignorable code points no class marks: drawn as nothing
		{"go\U000E0080\U000E00FF\U000E01F0\U000E0FFF\ufff0\ufff8", `go\u{e0080}\u{e00ff}\u{e01f0}\u{e0fff}\u{fff0}\u{fff8}`},
		// bytes that are not UTF-8, a surrogate's bytes among them
		{"bad\xffbyte", `bad\x{ff}byte`},
		{"\xed\xa0\x80", `\x{ed}\x{a0}\x{80}`},
	} {
		if got := EscapeDisplay(tc.in); got != tc.want {
			t.Errorf("EscapeDisplay(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if hidden := DisplayHidden(tc.in); hidden != (tc.in != tc.want) {
			t.Errorf("DisplayHidden(%q) = %v", tc.in, hidden)
		}
	}
}

func TestEscapeDisplayLinesKeepsLinesAndEscapesTheRest(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"one\ntwo", "one\ntwo"},
		{"one\r\ntwo\t2", "one\\u{d}\ntwo\\u{9}2"},
		{"a\u202eb\n\u200bc", "a\\u{202e}b\n\\u{200b}c"},
	} {
		if got := EscapeDisplayLines(tc.in); got != tc.want {
			t.Errorf("EscapeDisplayLines(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEscapeDisplayTextKeepsWritingAndShowsWhatHides(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain", "LGTM, thanks", "LGTM, thanks"},
		{"lines and tabs", "func f() {\n\treturn\n}", "func f() {\n\treturn\n}"},
		{"emoji with a selector", "❤\ufe0f ©\ufe0f 1\ufe0f⃣ #\ufe0f⃣", "❤\ufe0f ©\ufe0f 1\ufe0f⃣ #\ufe0f⃣"},
		{"emoji joined", "👨\u200d💻 🏳\ufe0f\u200d🌈 👩\u200d👩\u200d👧", "👨\u200d💻 🏳\ufe0f\u200d🌈 👩\u200d👩\u200d👧"},
		{"Persian non-joiner", "می\u200cخواهم", "می\u200cخواهم"},
		{"Devanagari joiner", "क्\u200dष", "क्\u200dष"},
		{"carriage return", "ok\r\nnext", "ok\\u{d}\nnext"},
		{"terminal escape", "a\x1b[1A\x1b[2Kb", `a\u{1b}[1A\u{1b}[2Kb`},
		{"bidi override", "say \u202eon\u202c", `say \u{202e}on\u{202c}`},
		{"tag characters", "LGTM\U000E0072\U000E006D", `LGTM\u{e0072}\u{e006d}`},
		{"zero width space", "rm\u200b -rf", `rm\u{200b} -rf`},
		{"joiner between ASCII", "a\u200db", `a\u{200d}b`},
		{"joiner at the end", "👨\u200d", `👨\u{200d}`},
		{"two joiners", "👨\u200d\u200d💻", `👨\u{200d}\u{200d}💻`},
		{"selector after a joiner", "👨\u200d\ufe0f💻", `👨\u{200d}\u{fe0f}💻`},
		{"selector run", "😀\ufe0f\ufe0f\ufe0e", `😀` + "\ufe0f" + `\u{fe0f}\u{fe0e}`},
		{"selector after ASCII letter", "ok\ufe0f", `ok\u{fe0f}`},
		{"selector after a line break", "\n\ufe0f", "\n" + `\u{fe0f}`},
		{"other selectors", "😀\ufe01\U000E0101", `😀\u{fe01}\u{e0101}`},
		{"line separator", "a\u2028b", `a\u{2028}b`},
		{"not UTF-8", "a\xffb", `a\x{ff}b`},
		// Joiners and selectors are kept only where emoji or a joining
		// script need them, so they cannot mark Latin, Greek or Cyrillic
		// letters, punctuation or blanks.
		{"joiners and selectors between Latin letters", "é\u200dé\u200cé\ufe0fé", `é\u{200d}é\u{200c}é\u{fe0f}é`},
		{"joiner between Greek letters", "α\u200dβ", `α\u{200d}β`},
		{"non-joiner between Cyrillic letters", "д\u200cж", `д\u{200c}ж`},
		{"selector after punctuation", "ok…\ufe0f", `ok…\u{fe0f}`},
		{"joiner around blank braille", "x\u2800\u200d\u2800", "x\u2800" + `\u{200d}` + "\u2800"},
		{"non-joiner between emoji", "👨\u200c💻", `👨\u{200c}💻`},
		{"joiner from an emoji to Arabic", "😀\u200dم", `😀\u{200d}م`},
		{"joiner before U+FFFD", "😀\u200d\ufffd", `😀\u{200d}` + "\ufffd"},
		{"joiner after a keycap", "1\ufe0f\u200d😀", "1\ufe0f" + `\u{200d}` + "😀"},
		{"skin tone and joiner", "👩🏽\u200d💻", "👩🏽\u200d💻"},
		{"Arabic joiner", "ل\u200dا", "ل\u200dا"},
	} {
		if got := EscapeDisplayText(tc.in); got != tc.want {
			t.Errorf("%s: EscapeDisplayText(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// Every control, format, line separator and paragraph separator
// character of Go's Unicode tables, every other default ignorable code
// point and every variation selector is in the table, so a Go release with a newer
// Unicode that adds one fails here until both tables (Go and app.js) have
// it.
func TestEscapeDisplayCoversEveryControlAndFormatCharacter(t *testing.T) {
	for class, tab := range map[string]*unicode.RangeTable{"Cc": unicode.Cc, "Cf": unicode.Cf, "Zl": unicode.Zl, "Zp": unicode.Zp,
		"Other_Default_Ignorable_Code_Point": unicode.Other_Default_Ignorable_Code_Point,
		"Variation_Selector":                 unicode.Variation_Selector} {
		for r := rune(0); r <= unicode.MaxRune; r++ {
			if unicode.Is(tab, r) && !Hidden(r) {
				t.Errorf("U+%04X (%s) is not in the table", r, class)
			}
		}
	}
	prev := rune(-1)
	for _, rg := range hiddenRanges {
		if rg[0] > rg[1] || rg[0] <= prev {
			t.Errorf("range %x-%x is out of order or overlaps", rg[0], rg[1])
		}
		prev = rg[1]
	}
	if !Hidden(0) || !Hidden(0xe0fff) || Hidden(0xe1000) || !Hidden(0xfff0) || Hidden(0xffef) || Hidden('a') || Hidden(0xfffd) {
		t.Error("table edges")
	}
	for name, tab := range map[string][][2]rune{"emoji": emojiRanges, "joining": joiningRanges} {
		prev := rune(-1)
		for _, rg := range tab {
			if rg[0] > rg[1] || rg[0] <= prev {
				t.Errorf("%s range %x-%x is out of order or overlaps", name, rg[0], rg[1])
			}
			prev = rg[1]
		}
	}
}

// F29 (security review 2026-11-25): a choice is sent as the person's
// reply when its button is clicked, so the button shows exactly what is
// sent. An honest choice with an emoji sequence is shown and sent as it
// is; one holding a hidden character, a line break, a blank that is not
// U+0020 or a space the button would collapse or the reply would trim is
// shown escaped and refused.
func TestAChoiceIsShownExactlyAsItIsSentOrRefused(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Yes", "Yes"},
		{"❤\ufe0f Yes", "❤\ufe0f Yes"},
		{"⚠\ufe0f Stop", "⚠\ufe0f Stop"},
		{"✔\ufe0f Done", "✔\ufe0f Done"},
		{"👨\u200d💻 ok", "👨\u200d💻 ok"},
		{"Ship it to prod", "Ship it to prod"},
		{"Yes\U000E0061\U000E006C", `Yes\u{e0061}\u{e006c}`},
		{"No\u202e", `No\u{202e}`},
		{"Yes\u2800\u2800", `Yes\u{2800}\u{2800}`},
		{"Yes\u00a0no", `Yes\u{a0}no`},
		{"Yes\u3000", `Yes\u{3000}`},
		{" Yes", `\u{20}Yes`},
		{"Yes ", `Yes\u{20}`},
		{"Yes  all", `Yes\u{20}\u{20}all`},
		{"Yes\nNo", `Yes\u{a}No`},
		{"Yes\tNo", `Yes\u{9}No`},
		{"é\u200dé", `é\u{200d}é`},
	} {
		got := EscapeChoice(tc.in)
		if got != tc.want {
			t.Errorf("EscapeChoice(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if ChoiceHidden(tc.in) != (tc.in != tc.want) {
			t.Errorf("ChoiceHidden(%q) = %v", tc.in, ChoiceHidden(tc.in))
		}
	}
}

// What EscapeDisplay writes holds no character it escapes and is valid
// UTF-8, so escaping it again changes nothing; what EscapeDisplayText
// writes holds only the hidden characters it may keep.
func FuzzEscapeDisplay(f *testing.F) {
	for _, s := range []string{"", "ok", "\x1b[2K\u202e\n", "👨\u200d💻\ufe0f\ufe0f", "\xff\xed\xa0\x80", "a\U000E0041\u200d"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		one := EscapeDisplay(s)
		if !utf8.ValidString(one) || EscapeDisplay(one) != one || DisplayHidden(one) {
			t.Fatalf("EscapeDisplay(%q) = %q", s, one)
		}
		for _, r := range one {
			if Hidden(r) {
				t.Fatalf("EscapeDisplay(%q) kept U+%04X", s, r)
			}
		}
		if lines := EscapeDisplayLines(s); strings.Count(lines, "\n") != strings.Count(s, "\n") {
			t.Fatalf("EscapeDisplayLines(%q) = %q", s, lines)
		}
		if c := EscapeChoice(s); !utf8.ValidString(c) || strings.ContainsAny(c, "\n\t") || EscapeChoice(c) != c {
			t.Fatalf("EscapeChoice(%q) = %q", s, c)
		}
		text := EscapeDisplayText(s)
		if !utf8.ValidString(text) {
			t.Fatalf("EscapeDisplayText(%q) is not UTF-8", s)
		}
		for _, r := range text {
			switch {
			case !Hidden(r), r == '\n', r == '\t', r == 0x200c, r == 0x200d, r == 0xfe0e, r == 0xfe0f:
			default:
				t.Fatalf("EscapeDisplayText(%q) kept U+%04X", s, r)
			}
		}
	})
}
