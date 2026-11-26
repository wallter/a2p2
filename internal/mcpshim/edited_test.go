package mcpshim

import (
	"strings"
	"testing"
)

// D62 follow-up: a message whose author replaced its text after it was
// read is marked edited where an agent reads it again (here its thread),
// and a message never replaced carries no mark.
func TestAMessageReplacedAfterItWasReadIsMarkedEditedInTheThread(t *testing.T) {
	c := startDaemon(t)
	a, b := connect(t, c, "alice"), connect(t, c, "bob")
	agentID(t, a)
	bob := agentID(t, b)
	id := idOf(t, ok(t, a, "send", map[string]any{"text": "first text", "to": []string{bob}}))
	if es := parseEntries(t, ok(t, b, "inbox", nil)); len(es) != 1 || es[0].Edited {
		t.Fatalf("first read: %+v", es)
	}
	ok(t, a, "send", map[string]any{"replaces": id, "text": "second text"})
	out := ok(t, b, "inbox", map[string]any{"thread": id})
	var msg bool
	for _, e := range parseEntries(t, out) {
		if e.ID == id && e.Kind == "" {
			msg = true
			if !e.Edited || e.Text != "second text" {
				t.Fatalf("thread entry %+v: want the new text marked edited", e)
			}
		}
	}
	if !msg || !strings.Contains(out, `"edited":true`) {
		t.Fatalf("no edited message in the thread: %s", out)
	}
}
