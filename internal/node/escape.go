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
// rules, and tests keep the two equal.
//
// The escape is not reversible: a literal backslash is written as it is,
// so text that already says \u{202e} reads the same as an escaped U+202E.
// Either way the reader sees exactly the characters shown, and nothing
// hidden.

// hiddenRanges are the code points EscapeDisplay writes as \u{...}:
// inclusive, sorted and not overlapping. They are every control (Cc),
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

// EmojiRanges and JoiningRanges return copies of the tables
// EscapeDisplayText uses, for another renderer to share.
func EmojiRanges() [][2]rune   { return slices.Clone(emojiRanges) }
func JoiningRanges() [][2]rune { return slices.Clone(joiningRanges) }

// HiddenRanges returns a copy of the table EscapeDisplay escapes, as
// inclusive [low, high] pairs in order, for another renderer to share.
func HiddenRanges() [][2]rune { return slices.Clone(hiddenRanges) }

// Hidden reports whether EscapeDisplay writes r as \u{...}.
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

// EscapeDisplay is s made safe to show as one line: every character of
// the table above, line breaks and tabs included, is written \u{hex}, and
// a byte that is not UTF-8 is written \x{hh}. Every other character is
// written as it is. Use it for anything shown on one line.
func EscapeDisplay(s string) string {
	if !needsEscape(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		writeRune(&b, s[i:i+n], r, n, Hidden(r))
		i += n
	}
	return b.String()
}

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
// escapes what EscapeDisplay does, with exceptions that ordinary writing
// needs: a newline, which stays a line break; a tab; a zero width joiner
// between two emoji (or after an emoji and its selector); a joiner or
// non-joiner between two letters of a script that needs them
// (joiningRanges); and one U+FE0E or U+FE0F straight after an emoji, a
// digit, '#' or '*'. Every other such character, a run of them, and one
// at either end of a word is escaped.
//
// What is kept can still carry a little information nobody sees: where
// two emoji may be joined, or a letter pair of those scripts may take a
// joiner, the drawing can look the same with or without it, and a
// selector after an emoji that has one presentation changes nothing.
// That is at most about one bit per such position, only inside emoji and
// those scripts, never in Latin, Greek or Cyrillic text or after ASCII
// punctuation. It is accepted because escaping these would break emoji
// and the writing of those scripts for every reader.
func EscapeDisplayText(text string) string { return escapeText(text, true) }

// EscapeChoice is a question's choice as the page draws it on a button,
// whose text is sent as the person's reply when clicked: one line, so the
// rules of EscapeDisplayText with a newline and a tab escaped too, and
// with every character that shows as blank or would not be sent as shown
// written as an escape. That is a space other than U+0020, U+2800 (blank
// braille), and a U+0020 at either end or next to another, which the
// button would collapse or the reply would trim (\u{20}).
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

// DisplayHidden reports whether s holds anything EscapeDisplay would
// write as an escape. A caller about to act on a remote string that must
// read as it is shown refuses it then. For a choice, use ChoiceHidden,
// which keeps emoji sequences.
func DisplayHidden(s string) bool { return needsEscape(s) }

const (
	keptNone          = iota
	keptEmoji         // an emoji
	keptEmojiSelector // a kept U+FE0E or U+FE0F after an emoji
	keptJoining       // a letter of a script in joiningRanges
	keptKeycap        // a digit, '#' or '*', which may take an emoji selector
)

func escapeText(text string, lines bool) string {
	if !needsEscape(text) {
		return text
	}
	var b strings.Builder
	prev := keptNone
	for i := 0; i < len(text); {
		r, n := utf8.DecodeRuneInString(text[i:])
		hide := Hidden(r)
		next := keptNone
		nr, _ := utf8.DecodeRuneInString(text[i+n:])
		switch {
		case r == utf8.RuneError && n == 1:
		case r == '\n' || r == '\t':
			hide = !lines
		case r == 0x200d && (prev == keptEmoji || prev == keptEmojiSelector) && textClass(nr) == keptEmoji,
			(r == 0x200c || r == 0x200d) && prev == keptJoining && textClass(nr) == keptJoining:
			hide = false
		case (r == 0xfe0e || r == 0xfe0f) && prev == keptEmoji:
			hide, next = false, keptEmojiSelector
		case (r == 0xfe0e || r == 0xfe0f) && prev == keptKeycap:
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

// textClass is what r is to the rules of EscapeDisplayText.
func textClass(r rune) int {
	switch {
	case Hidden(r):
		return keptNone
	case r >= '0' && r <= '9' || r == '#' || r == '*':
		return keptKeycap
	case inRanges(emojiRanges, r):
		return keptEmoji
	case inRanges(joiningRanges, r):
		return keptJoining
	}
	return keptNone
}

func needsEscape(s string) bool {
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n == 1 || Hidden(r) {
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
