package mcpshim

import (
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/node"
)

// E3 of the second security review (2026-11-25): the text of a tool
// result reached the model as sent, terminal escapes, overrides and tag
// characters (text nobody sees) included. Message text is now escaped by
// node.EscapeDisplayText, so every such character is written \u{...} and
// nothing is dropped; honest text (lines, emoji, Persian) reads as
// written. The first lines inbox peek gives are escaped too.
func TestToolResultsEscapeWhatHidesTextAndKeepHonestText(t *testing.T) {
	c := startDaemon(t)
	a, b := connect(t, c, "alice"), connect(t, c, "bob")
	agentID(t, a)
	agentID(t, b)
	text := "LGTM\U000E0072\U000E006D \x1b[2K\u202eok\nline two ❤\ufe0f تیم\u200cما"
	ok(t, a, "send", map[string]any{"text": text})
	peek := ok(t, b, "inbox", map[string]any{"peek": true})
	if !strings.Contains(peek, `LGTM\\u{e0072}\\u{e006d} \\u{1b}[2K\\u{202e}ok`) || strings.Contains(peek, "\u202e") {
		t.Fatalf("peek = %s", peek)
	}
	out := ok(t, b, "inbox", nil)
	es := parseEntries(t, out)
	if len(es) != 1 || es[0].Text != node.EscapeDisplayText(text) {
		t.Fatalf("entries = %+v", es)
	}
	for _, raw := range []string{"\u202e", "\U000E0072", `\u001b`} {
		if strings.Contains(out, raw) {
			t.Errorf("inbox holds %q: %s", raw, out)
		}
	}
	if !strings.Contains(es[0].Text, "\nline two ❤\ufe0f تیم\u200cما") {
		t.Errorf("honest text changed: %q", es[0].Text)
	}
}
