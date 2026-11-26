package mcpshim

import (
	"strings"
	"testing"
)

// D62: the reply tool names no version, so the daemon binds it to the
// text the agent was last shown. A reply to a message replaced since the
// agent's read is refused as not_shown, and nothing is reported handled;
// once a read shows the new text, the reply goes through and reports it.
func TestTheReplyToolToTextReplacedSinceTheReadIsRefusedUntilTheNewTextIsShown(t *testing.T) {
	c := startDaemon(t)
	a, b := connect(t, c, "alice"), connect(t, c, "bob")
	agentID(t, a)
	bob := agentID(t, b)
	id := idOf(t, ok(t, a, "send", map[string]any{"text": "first text", "to": []string{bob}}))
	if es := parseEntries(t, ok(t, b, "inbox", nil)); len(es) != 1 || es[0].ID != id {
		t.Fatalf("first read: %+v", es)
	}
	ok(t, a, "send", map[string]any{"replaces": id, "text": "second text"})
	s, isErr := call(t, b, "reply", map[string]any{"id": id, "text": "answer to the first text"})
	if !isErr || !strings.Contains(s, "not_shown") {
		t.Fatalf("reply to text replaced since the read: %s", s)
	}
	if out := ok(t, b, "inbox", nil); !strings.Contains(out, "second text") {
		t.Fatalf("the new text was not shown: %s", out)
	}
	s = ok(t, b, "reply", map[string]any{"id": id, "text": "answer to the second text"})
	if !strings.Contains(s, "the original is marked handled") {
		t.Fatalf("reply after the new text was shown: %s", s)
	}
}
