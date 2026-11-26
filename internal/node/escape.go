package node

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Display escaping (PROTOCOL 10.8, DESIGN 12 item 8).
//
// Room labels, message text, choices, file names and paths can come from
// other machines, and those machines may belong to another owner. Text
// like that must never change what a person or an agent believes they are
// reading: it must not move the cursor, erase or recolour a line, reorder
// the words around it, start a new line where one line is shown, or carry
// characters nobody can see.
//
// The helpers here write every such character visibly instead, as
// \u{hex} (a code point) or \x{hh} (a byte that is not UTF-8). Nothing is
// ever dropped, so what is shown still says everything that was sent.
// They escape at display time and key on nothing remote: stored and
// signed data stays as it came, and old logs still load.
//
// Which helper to use depends on where the string is shown:
//
//	position                                   helper
//	one line: a label, name, path, an id,      EscapeDisplay
//	  a clause in approval text
//	lines a person approves (a send's body)    EscapeDisplayLines
//	message text a person or agent reads       EscapeDisplayText
//	a choice: a remote string this machine     EscapeChoice; refuse when
//	  would send as its person's own reply       ChoiceHidden
//	any other remote string to refuse          DisplayHidden
//
// The page (internal/web/static/app.js) has the same tables and the same
// rules, and tests keep the two equal. The test strings both use are in
// testdata/escape_cases.json.
//
// The escape can be read back: a backslash that would start what looks
// like an escape (\u{ or \x{) is itself written \u{5c}, so every \u{
// shown is a character that was escaped. A remote string set inside a
// sentence a person approves is also isolated (Isolate), so that
// right-to-left text in it cannot reorder the words around it.

// hiddenRanges are the code points the helpers write as \u{...} unless
// the text rules keep them where they stand: inclusive, sorted and not overlapping. They are every control (Cc),
// format (Cf), line separator (Zl) and paragraph separator (Zp) character
// of Unicode 17, plus characters that draw as nothing although Unicode
// gives them another class: U+034F, the Hangul fillers, the Khmer
// invisible vowels, the Mongolian variation selectors, the variation
// selectors, the unassigned U+FFF0 to U+FFF8, the whole U+E0000 to
// U+E0FFF block (tags, variation selectors and unassigned code points that
// text shaping draws as nothing) and the surrogate range (which the page
// can meet in a broken string). A test checks the table against Go's
// tables of the classes, Other_Default_Ignorable_Code_Point and
// Variation_Selector.
var hiddenRanges = [][2]rune{
	{0x0000, 0x001f}, // C0 controls: BEL, BS, TAB, LF, CR, ESC
	{0x007f, 0x009f}, // DEL and C1 controls, CSI (U+009B) among them
	{0x00ad, 0x00ad}, // soft hyphen
	{0x034f, 0x034f}, // combining grapheme joiner
	{0x0600, 0x0605}, // Arabic number signs (Cf)
	{0x061c, 0x061c}, // Arabic letter mark
	{0x06dd, 0x06dd}, // Arabic end of ayah (Cf)
	{0x070f, 0x070f}, // Syriac abbreviation mark (Cf)
	{0x0890, 0x0891}, // Arabic pound and piastre marks above (Cf)
	{0x08e2, 0x08e2}, // Arabic disputed end of ayah (Cf)
	{0x115f, 0x1160}, // Hangul choseong and jungseong fillers
	{0x17b4, 0x17b5}, // Khmer inherent vowels, drawn as nothing
	{0x180b, 0x180f}, // Mongolian variation selectors and vowel separator
	{0x200b, 0x200f}, // zero width space, ZWNJ, ZWJ, LRM, RLM
	{0x2028, 0x202e}, // line and paragraph separators, bidi embeddings and overrides
	{0x2060, 0x206f}, // word joiner, invisible operators, bidi isolates, deprecated format characters
	{0x3164, 0x3164}, // Hangul filler
	{0xd800, 0xdfff}, // surrogates: never valid text
	{0xfe00, 0xfe0f}, // variation selectors
	{0xfeff, 0xfeff}, // zero width no-break space (byte order mark)
	{0xffa0, 0xffa0}, // halfwidth Hangul filler
	{0xfff0, 0xfffb}, // unassigned, drawn as nothing; interlinear annotation controls
	{0x110bd, 0x110bd},
	{0x110cd, 0x110cd},
	{0x13430, 0x1343f}, // Egyptian hieroglyph format controls
	{0x1bca0, 0x1bca3}, // shorthand format controls
	{0x1d173, 0x1d17a}, // musical symbol format controls
	{0xe0000, 0xe0fff}, // tag characters, variation selectors supplement, unassigned: all drawn as nothing
}

// emojiRanges are the characters EscapeDisplayText treats as emoji: a
// zero width joiner may stand between two of them, and an emoji or text
// selector (U+FE0E, U+FE0F) after one. They cover the emoji of Unicode 17
// with room to spare; app.js has the same table.
var emojiRanges = [][2]rune{
	{0x00a9, 0x00a9}, {0x00ae, 0x00ae}, {0x203c, 0x203c}, {0x2049, 0x2049},
	{0x2122, 0x2122}, {0x2139, 0x2139}, {0x2194, 0x21aa}, {0x231a, 0x23ff},
	{0x24c2, 0x24c2}, {0x25aa, 0x27bf}, {0x2934, 0x2935}, {0x2b05, 0x2b55},
	{0x3030, 0x3030}, {0x303d, 0x303d}, {0x3297, 0x3297}, {0x3299, 0x3299},
	{0x1f000, 0x1faff},
}

// joiningRanges are the scripts whose writing needs a zero width joiner
// or non-joiner between two letters: Arabic, Syriac, Thaana, NKo and the
// other scripts of U+0600 to U+08FF, the Indic scripts from Devanagari to
// Sinhala, Myanmar, Khmer, Mongolian and the Arabic presentation forms.
// Latin, Greek and Cyrillic are not among them. app.js has the same table.
var joiningRanges = [][2]rune{
	{0x0600, 0x08ff}, {0x0900, 0x0dff}, {0x1000, 0x109f}, {0x1780, 0x18af},
	{0xfb50, 0xfdff}, {0xfe70, 0xfefe},
}

// rtlRanges are the right-to-left scripts: Hebrew, Arabic and the other
// scripts of U+0590 to U+08FF, their presentation forms and the
// right-to-left blocks above U+FFFF (RightToLeft).
var rtlRanges = [][2]rune{
	{0x0590, 0x08ff}, {0xfb1d, 0xfdff}, {0xfe70, 0xfefe}, {0x10800, 0x10fff}, {0x1e800, 0x1efff},
}

// rtlLetterRanges are the letters of Hebrew, Arabic, Syriac, Thaana, NKo,
// Samaritan and Mandaic and of their presentation forms, every one of them
// strong right-to-left. A right-to-left or Arabic letter mark between two
// of them is kept by the text rules: both sides are strong right-to-left
// there, so the mark changes nothing in how the line is ordered. The table
// lists code points, not a Unicode class, so that a browser with an older
// Unicode gives the same answer; a test checks each is a letter. app.js
// has the same table.
var rtlLetterRanges = [][2]rune{
	{0x05d0, 0x05ea}, {0x05ef, 0x05f2}, {0x0620, 0x064a}, {0x066e, 0x066f}, {0x0671, 0x06d3},
	{0x06d5, 0x06d5}, {0x06ee, 0x06ef}, {0x06fa, 0x06fc}, {0x06ff, 0x06ff}, {0x0710, 0x0710},
	{0x0712, 0x072f}, {0x074d, 0x07a5}, {0x07b1, 0x07b1}, {0x07ca, 0x07ea}, {0x0800, 0x0815},
	{0x0840, 0x0858}, {0x0860, 0x086a}, {0x0870, 0x0887}, {0x0889, 0x088e}, {0x08a0, 0x08c9},
	{0xfb1d, 0xfb1d}, {0xfb1f, 0xfb28}, {0xfb2a, 0xfb36}, {0xfb38, 0xfb3c}, {0xfb3e, 0xfb3e},
	{0xfb40, 0xfb41}, {0xfb43, 0xfb44}, {0xfb46, 0xfbb1}, {0xfbd3, 0xfd3d}, {0xfd50, 0xfd8f},
	{0xfd92, 0xfdc7}, {0xfdf0, 0xfdfb}, {0xfe70, 0xfe74}, {0xfe76, 0xfefc},
}

// spacelessRanges are the scripts written without spaces between words,
// where a zero width space marks a word break: Thai, Lao, Tibetan,
// Myanmar and Khmer. A zero width space between two characters of these
// scripts is kept by the text rules. app.js has the same table.
var spacelessRanges = [][2]rune{
	{0x0e00, 0x0fff}, {0x1000, 0x109f}, {0x1780, 0x17ff},
}

// hanRanges are the CJK ideographs. One ideographic variation selector
// (U+E0100 to U+E01EF) straight after one is kept by the text rules: it
// picks the glyph a Japanese name is written with. app.js has the same
// table.
var hanRanges = [][2]rune{
	{0x3400, 0x4dbf}, {0x4e00, 0x9fff}, {0xf900, 0xfaff}, {0x20000, 0x3ffff},
}

// flagTags are the tag sequences after U+1F3F4 (a black flag) that draw
// the flags of England, Scotland and Wales, the only ones Unicode
// recommends. The text rules keep exactly these; any other tag character
// is escaped, since tags draw as nothing.
var flagTags = []string{tagged("gbeng"), tagged("gbsct"), tagged("gbwls")}

// tagged is s written in tag characters, with the cancel tag after it.
func tagged(s string) string {
	var b strings.Builder
	for _, r := range s {
		b.WriteRune(0xe0000 + r)
	}
	b.WriteRune(0xe007f)
	return b.String()
}

// EmojiRanges, JoiningRanges, RTLLetterRanges, SpacelessRanges and
// HanRanges return copies of the tables the text rules use, for another
// renderer to share.
func EmojiRanges() [][2]rune     { return slices.Clone(emojiRanges) }
func JoiningRanges() [][2]rune   { return slices.Clone(joiningRanges) }
func RTLLetterRanges() [][2]rune { return slices.Clone(rtlLetterRanges) }
func SpacelessRanges() [][2]rune { return slices.Clone(spacelessRanges) }
func HanRanges() [][2]rune       { return slices.Clone(hanRanges) }

// HiddenRanges returns a copy of the table of characters the helpers
// escape, as inclusive [low, high] pairs in order, for another renderer
// to share.
func HiddenRanges() [][2]rune { return slices.Clone(hiddenRanges) }

// Hidden reports whether r is in that table: the helpers write it as
// \u{...} unless the text rules keep it where it stands.
func Hidden(r rune) bool { return inRanges(hiddenRanges, r) }

func inRanges(tab [][2]rune, r rune) bool {
	_, found := slices.BinarySearchFunc(tab, r, func(rg [2]rune, r rune) int {
		switch {
		case rg[1] < r:
			return -1
		case rg[0] > r:
			return 1
		}
		return 0
	})
	return found
}

// EscapeDisplay is s made safe to show as one line. It follows the text
// rules of EscapeDisplayText with a line break and a tab written as
// escapes too: every character of the table is written \u{hex} unless
// those rules keep it, a byte that is not UTF-8 is written \x{hh}, and a
// backslash before "u{" or "x{" is written \u{5c}. Every other character
// is written as it is. Use it for anything shown on one line.
func EscapeDisplay(s string) string { return escapeText(s, false) }

// EscapeDisplayLines is text a person approves line by line: each line
// escaped by EscapeDisplay and joined by "\n", so the lines stay lines and
// every other character that could hide or reorder text is shown. A
// carriage return is shown too.
func EscapeDisplayLines(text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = EscapeDisplay(l)
	}
	return strings.Join(lines, "\n")
}

// EscapeDisplayText is message text as a person or an agent reads it. It
// escapes the characters of the table, with exceptions that ordinary
// writing needs (the text rules):
//
//   - a newline, which stays a line break, and a tab; a carriage return
//     straight before a newline is shown as that one line break;
//   - a zero width joiner between two emoji (or after an emoji and its
//     selector), and one U+FE0E or U+FE0F straight after an emoji, a
//     digit, '#' or '*';
//   - a joiner or non-joiner between two letters of a script that needs
//     them (joiningRanges);
//   - a right-to-left mark or Arabic letter mark between two letters of
//     a right-to-left script (rtlLetterRanges), where it changes no
//     ordering;
//   - a zero width space between two characters of a script written
//     without spaces (spacelessRanges);
//   - one ideographic variation selector straight after a CJK ideograph;
//   - the tag sequences of the flags of England, Scotland and Wales.
//
// Every other such character, a run of them, and one at either end of a
// word is escaped.
//
// What is kept can still carry a little information nobody sees: where
// a joiner, mark or space may stand, the drawing can look the same with
// or without it, and a selector after an emoji that has one presentation
// changes nothing. That is at most about one bit per such position (an
// ideographic variation selector, about one byte per ideograph), only
// inside emoji and those scripts, never in Latin, Greek or Cyrillic text
// or after ASCII punctuation. Nothing kept changes the order in which the
// words around it are drawn. It is accepted because escaping these would
// break emoji and the writing of those scripts for every reader.
func EscapeDisplayText(text string) string { return escapeText(text, true) }

// EscapeChoice is a question's choice as the page draws it on a button,
// whose text is sent as the person's reply when clicked: one line, so the
// rules of EscapeDisplay, and with every character that shows as blank or
// would not be sent as shown written as an escape. That is a space other
// than U+0020, U+2800 (blank braille), and a U+0020 at either end or next
// to another, which the button would collapse or the reply would trim
// (\u{20}).
func EscapeChoice(s string) string {
	t := escapeText(s, false)
	if !strings.ContainsFunc(t, blankChoice) && !strings.HasPrefix(t, " ") && !strings.HasSuffix(t, " ") && !strings.Contains(t, "  ") {
		return t
	}
	rs := []rune(t)
	var b strings.Builder
	for i, r := range rs {
		switch {
		case blankChoice(r):
			fmt.Fprintf(&b, `\u{%x}`, r)
		case r == ' ' && (i == 0 || i == len(rs)-1 || rs[i-1] == ' ' || rs[i+1] == ' '):
			b.WriteString(`\u{20}`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ChoiceHidden reports whether EscapeChoice changes s: the page then does
// not send the choice, since the person cannot have read it as it is.
func ChoiceHidden(s string) bool { return EscapeChoice(s) != s }

// blankChoice reports whether r shows as blank space on a choice, other
// than U+0020.
func blankChoice(r rune) bool { return r != ' ' && unicode.Is(unicode.Zs, r) || r == 0x2800 }

// DisplayHidden reports whether EscapeDisplay changes s. A caller about
// to act on a remote string that must read as it is shown refuses it
// then. For a choice, use ChoiceHidden.
func DisplayHidden(s string) bool { return needsEscape(s) && EscapeDisplay(s) != s }

// Isolate marks s, a remote string already escaped, as one piece of a
// sentence a person approves: it is set between U+2068 (first strong
// isolate) and U+2069 (pop directional isolate), so that a renderer that
// follows the Unicode bidirectional algorithm orders it apart from the
// words around it. The escaped s holds neither character, so the pairs
// in a sentence are the ones Isolate wrote. The page draws each part in
// a bdi element; a terminal shows the sentence with Isolated.
func Isolate(s string) string { return string(isoOpen) + s + string(isoClose) }

const (
	isoOpen  = '\u2068'
	isoClose = '\u2069'
)

// Part is a piece of a sentence made with Isolate: Isolated when Isolate
// set it apart.
type Part struct {
	Text     string
	Isolated bool
}

// Isolated splits a sentence made with Isolate into its parts, in order.
// A U+2068 without its U+2069 isolates the rest of the sentence.
func Isolated(text string) []Part {
	var out []Part
	for text != "" {
		before, rest, found := strings.Cut(text, string(isoOpen))
		if before != "" {
			out = append(out, Part{Text: before})
		}
		if !found {
			break
		}
		inner, after, _ := strings.Cut(rest, string(isoClose))
		out = append(out, Part{Text: inner, Isolated: true})
		text = after
	}
	return out
}

// RightToLeft reports whether s holds a character of a right-to-left
// script (rtlRanges), which a terminal that orders text by direction
// could move across the words next to it.
func RightToLeft(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return inRanges(rtlRanges, r) })
}

// What a character is to the text rules (textClass), as bits.
const (
	clsEmoji     = 1 << iota // an emoji
	clsSelector              // a kept U+FE0E or U+FE0F after an emoji
	clsJoining               // a letter of a script in joiningRanges
	clsKeycap                // a digit, '#' or '*', which may take an emoji selector
	clsRTL                   // a letter of rtlLetterRanges
	clsSpaceless             // a character of a script in spacelessRanges
	clsHan                   // a CJK ideograph
)

func escapeText(text string, lines bool) string {
	if !needsEscape(text) {
		return text
	}
	var b strings.Builder
	prev := 0
	for i := 0; i < len(text); {
		r, n := utf8.DecodeRuneInString(text[i:])
		rest := text[i+n:]
		if tail := flagTail(r, rest); tail != "" {
			b.WriteString(text[i : i+n+len(tail)])
			prev = clsEmoji
			i += n + len(tail)
			continue
		}
		if lines && r == '\r' && strings.HasPrefix(rest, "\n") {
			b.WriteByte('\n') // shown as the one line break it ends
			prev = 0
			i += n + 1
			continue
		}
		hide := Hidden(r)
		next := 0
		nr, _ := utf8.DecodeRuneInString(rest)
		nc := textClass(nr)
		switch {
		case r == utf8.RuneError && n == 1:
		case r == '\n' || r == '\t':
			hide = !lines
		case r == '\\' && literalEscape(rest):
			hide = true
		case r == 0x200d && prev&(clsEmoji|clsSelector) != 0 && nc&clsEmoji != 0,
			(r == 0x200c || r == 0x200d) && prev&clsJoining != 0 && nc&clsJoining != 0,
			(r == 0x200f || r == 0x061c) && prev&clsRTL != 0 && nc&clsRTL != 0,
			r == 0x200b && prev&clsSpaceless != 0 && nc&clsSpaceless != 0,
			r >= 0xe0100 && r <= 0xe01ef && prev&clsHan != 0:
			hide = false
		case (r == 0xfe0e || r == 0xfe0f) && prev&clsEmoji != 0:
			hide, next = false, clsSelector
		case (r == 0xfe0e || r == 0xfe0f) && prev&clsKeycap != 0:
			hide = false
		case !hide:
			next = textClass(r)
		}
		writeRune(&b, text[i:i+n], r, n, hide)
		prev = next
		i += n
	}
	return b.String()
}

// literalEscape reports whether a backslash followed by rest would read
// as the start of an escape.
func literalEscape(rest string) bool {
	return strings.HasPrefix(rest, "u{") || strings.HasPrefix(rest, "x{")
}

// flagTail is the tag sequence of flagTags that follows r in rest when r
// is U+1F3F4, or "".
func flagTail(r rune, rest string) string {
	if r != 0x1f3f4 {
		return ""
	}
	for _, t := range flagTags {
		if strings.HasPrefix(rest, t) {
			return t
		}
	}
	return ""
}

// textClass is what r is to the text rules.
func textClass(r rune) int {
	if Hidden(r) {
		return 0
	}
	c := 0
	if r >= '0' && r <= '9' || r == '#' || r == '*' {
		c |= clsKeycap
	}
	if inRanges(emojiRanges, r) {
		c |= clsEmoji
	}
	if inRanges(joiningRanges, r) {
		c |= clsJoining
	}
	if inRanges(rtlLetterRanges, r) {
		c |= clsRTL
	}
	if inRanges(spacelessRanges, r) {
		c |= clsSpaceless
	}
	if inRanges(hanRanges, r) {
		c |= clsHan
	}
	return c
}

func needsEscape(s string) bool {
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n == 1 || Hidden(r) || r == '\\' && literalEscape(s[i+n:]) {
			return true
		}
		i += n
	}
	return false
}

func writeRune(b *strings.Builder, raw string, r rune, n int, hide bool) {
	switch {
	case r == utf8.RuneError && n == 1:
		fmt.Fprintf(b, `\x{%02x}`, raw[0])
	case hide:
		fmt.Fprintf(b, `\u{%x}`, r)
	default:
		b.WriteString(raw)
	}
}

// LabelKey is a room label as a person tells it apart from other labels:
// escaped (EscapeDisplay), without the characters the text rules keep
// that do not show (ZWSP, ZWNJ, ZWJ, RLM, ALM and the variation
// selectors), with each run of blank characters as one space, trimmed,
// and without case. Blank is every Unicode white space character
// (unicode.White_Space: a no-break space, an ideographic space and the
// other spaces among them) and U+2800, blank braille, so each draws as an
// ordinary space does. Two rooms whose labels give the same key are shown
// with their ids. The page's drawnLabel (app.js) gives the same key after
// it normalises the label to NFC, which this does not do.
func LabelKey(l string) string {
	var b strings.Builder
	blank := false
	for _, r := range EscapeDisplay(l) {
		switch {
		case keptUnseen(r):
		case labelBlank(r):
			blank = true
		default:
			if blank && b.Len() > 0 {
				b.WriteByte(' ')
			}
			blank = false
			b.WriteRune(r)
		}
	}
	return strings.ToLower(b.String())
}

// keptUnseen reports whether r is a character the text rules may keep
// raw that does not show (KEPT_INVISIBLE in app.js).
func keptUnseen(r rune) bool {
	switch r {
	case 0x200b, 0x200c, 0x200d, 0x200f, 0x061c, 0xfe0e, 0xfe0f:
		return true
	}
	return r >= 0xe0100 && r <= 0xe01ef
}

// labelBlank reports whether r draws as blank space in a label
// (LABEL_BLANK in app.js).
func labelBlank(r rune) bool { return unicode.Is(unicode.White_Space, r) || r == 0x2800 }
