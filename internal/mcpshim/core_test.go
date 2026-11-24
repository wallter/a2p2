package mcpshim

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
)

// refusal parses a tool error: one JSON object with exactly the three fields.
func refusal(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) localapi.Refusal {
	t.Helper()
	s, isErr := call(t, cs, tool, args)
	var m map[string]any
	var r localapi.Refusal
	if !isErr || json.Unmarshal([]byte(s), &m) != nil || len(m) != 3 || json.Unmarshal([]byte(s), &r) != nil || r.Error == "" {
		t.Fatalf("%s %v: want a refusal object, got %v %s", tool, args, isErr, s)
	}
	return r
}

// C2: a tool error is one JSON object {"error","code","retryable"}, with the
// daemon's code, the shim's own for bad input, and unavailable when the
// daemon cannot be reached.
func TestToolErrorsAreOneObjectWithACodeAndRetryable(t *testing.T) {
	c := startDaemon(t)
	a, b := connect(t, c, "alice"), connect(t, c, "bob")
	bob := agentID(t, b)
	direct := idOf(t, ok(t, a, "send", map[string]any{"text": "for bob", "to": []string{bob}}))
	for _, x := range []struct {
		cs    *mcp.ClientSession
		tool  string
		args  map[string]any
		code  string
		retry bool
	}{
		{a, "send", map[string]any{"text": "x", "room": "nonexistent"}, node.ErrorNotFound, false},
		{a, "send", map[string]any{"text": " "}, node.ErrorInvalid, false},
		{a, "inbox", map[string]any{"wait_seconds": 31}, node.ErrorInvalid, false},
		{b, "ack", map[string]any{"id": direct}, node.ErrorNotShown, false},
		{a, "ack", map[string]any{"id": direct}, node.ErrorNotAllowed, false},
		{b, "ack", map[string]any{"id": strings.Repeat("0", 32)}, node.ErrorNotFound, false},
		{a, "send", map[string]any{"text": strings.Repeat("y", 20000)}, node.ErrorTooLarge, false},
	} {
		if r := refusal(t, x.cs, x.tool, x.args); r.Code != x.code || r.Retryable != x.retry {
			t.Errorf("%s %v: %+v, want %s retryable %v", x.tool, x.args, r, x.code, x.retry)
		}
	}
	dead := NewServer(localapi.NewClient("/nonexistent.sock"), "h", "l", "", "v")
	ct, st := mcp.NewInMemoryTransports()
	go dead.Run(context.Background(), st)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "1"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if r := refusal(t, cs, "inbox", nil); r.Code != node.ErrorUnavailable || !r.Retryable || !strings.Contains(r.Error, "unavailable") {
		t.Fatalf("dead daemon: %+v", r)
	}
}

// Item 4: tool results carry '<', '>' and '&' as written, never escaped, and
// still as JSON strings (the text is never markup and forges no structure).
func TestToolResultsDoNotEscapeHTML(t *testing.T) {
	c := startDaemon(t)
	a, b := connect(t, c, "alice"), connect(t, c, "bob")
	agentID(t, a)
	agentID(t, b)
	text := `a < b && c > d </script>`
	ok(t, a, "send", map[string]any{"text": text})
	out := ok(t, b, "inbox", nil)
	if !strings.Contains(out, `"text":"`+text+`"`) || strings.Contains(out, `\`+"u00") {
		t.Fatalf("inbox = %s", out)
	}
	if es := parseEntries(t, out); len(es) != 1 || es[0].Text != text {
		t.Fatalf("entries = %+v", es)
	}
	if out := ok(t, b, "inbox", map[string]any{"recent": 1}); !strings.Contains(out, text) {
		t.Fatalf("recent = %s", out)
	}
}

// C7: wait_seconds is at most 30 in the tool (and says so in its schema); a
// wait that ends with nothing says it waited; a cancelled wait marks
// nothing, and the next read still shows what arrived.
func TestInboxWaitIsCappedSaysWhenItEndedEmptyAndACancelledWaitMarksNothing(t *testing.T) {
	c := startDaemon(t)
	a, b := connect(t, c, "alice"), connect(t, c, "bob")
	agentID(t, a)
	agentID(t, b)
	res, err := b.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range res.Tools {
		if tl.Name == "inbox" && !strings.Contains(string(mustJSON(t, tl.InputSchema)), `"0-30: `) {
			t.Errorf("inbox schema does not state the cap: %s", mustJSON(t, tl.InputSchema))
		}
	}
	if _, isErr := call(t, b, "inbox", map[string]any{"wait_seconds": 31}); !isErr {
		t.Fatal("wait_seconds 31 accepted")
	}
	start := time.Now()
	out := ok(t, b, "inbox", map[string]any{"wait_seconds": 1})
	var r struct {
		Waited   bool  `json:"waited"`
		Messages []any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil || !r.Waited || r.Messages == nil || len(r.Messages) != 0 ||
		time.Since(start) < 900*time.Millisecond {
		t.Fatalf("empty wait = %s", out)
	}
	if out := ok(t, b, "inbox", nil); strings.Contains(out, "waited") {
		t.Fatalf("no wait asked, yet %s", out)
	}
	// Cancel a waiting call, then let mail arrive: nothing is marked seen.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := b.CallTool(ctx, &mcp.CallToolParams{Name: "inbox", Arguments: map[string]any{"wait_seconds": 30}})
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled call did not end")
	}
	ok(t, a, "send", map[string]any{"text": "after the cancel"})
	time.Sleep(300 * time.Millisecond) // a wait left running would have taken it by now
	if es := parseEntries(t, ok(t, b, "inbox", nil)); len(es) != 1 || es[0].Text != "after the cancel" {
		t.Fatalf("after a cancelled wait: %+v", es)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// C6: each tool's annotations, chosen honestly: whoami and peers only read;
// inbox marks seen, so it writes; send can rewrite or withdraw (replaces),
// so it is destructive; the others only add; a repeated ack changes nothing.
func TestToolAnnotationsSayWhatEachToolChanges(t *testing.T) {
	cs := connect(t, startDaemon(t), "a")
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	type hint struct{ readOnly, destructive, idempotent bool }
	want := map[string]hint{
		"whoami": {true, false, true}, "peers": {true, false, true},
		"inbox": {false, false, false}, "send": {false, true, false}, "reply": {false, false, false},
		"ask_human": {false, false, false}, "ack": {false, false, true},
	}
	for _, tl := range res.Tools {
		an := tl.Annotations
		if an == nil {
			t.Errorf("%s: no annotations", tl.Name)
			continue
		}
		w := want[tl.Name]
		if an.ReadOnlyHint != w.readOnly || an.IdempotentHint != w.idempotent {
			t.Errorf("%s: %+v, want %+v", tl.Name, an, w)
		}
		// A writer states destructiveHint explicitly (its default is true).
		if !w.readOnly && (an.DestructiveHint == nil || *an.DestructiveHint != w.destructive) {
			t.Errorf("%s: destructiveHint %v, want %v", tl.Name, an.DestructiveHint, w.destructive)
		}
	}
}

// C4: inbox with thread and after (a message id in that thread) returns
// only what follows it, so re-reading a thread does not repeat it; it
// marks nothing, and an id outside the thread is refused as not_found.
func TestThreadReadAfterAMessageReturnsOnlyWhatFollows(t *testing.T) {
	c := startDaemon(t)
	a, b := connect(t, c, "alice"), connect(t, c, "bob")
	agentID(t, a)
	agentID(t, b)
	root := idOf(t, ok(t, a, "send", map[string]any{"text": "root"}))
	r1 := idOf(t, ok(t, b, "reply", map[string]any{"id": root, "text": "one"}))
	other := idOf(t, ok(t, a, "send", map[string]any{"text": "elsewhere"}))
	all := parseEntries(t, ok(t, b, "inbox", map[string]any{"thread": root}))
	if len(all) != 2 {
		t.Fatalf("thread = %+v", all)
	}
	if es := parseEntries(t, ok(t, b, "inbox", map[string]any{"thread": root, "after": r1})); len(es) != 0 {
		t.Fatalf("after the last one: %+v", es)
	}
	idOf(t, ok(t, a, "reply", map[string]any{"id": r1, "text": "two"}))
	es := parseEntries(t, ok(t, b, "inbox", map[string]any{"thread": root, "after": r1}))
	if len(es) != 1 || es[0].Text != "two" {
		t.Fatalf("after one: %+v", es)
	}
	if es := parseEntries(t, ok(t, b, "inbox", map[string]any{"thread": root, "after": root})); len(es) != 2 || es[0].Text != "one" {
		t.Fatalf("after the root: %+v", es)
	}
	if r := refusal(t, b, "inbox", map[string]any{"thread": root, "after": other}); r.Code != node.ErrorNotFound {
		t.Fatalf("after outside the thread: %+v", r)
	}
	if r := refusal(t, b, "inbox", map[string]any{"after": r1}); r.Code != node.ErrorInvalid {
		t.Fatalf("after without thread: %+v", r)
	}
	// Nothing was marked seen by any of it: bob still has root, elsewhere
	// and alice's reply unread.
	if es := parseEntries(t, ok(t, b, "inbox", nil)); len(es) != 3 {
		t.Fatalf("unread after thread reads: %+v", es)
	}
}
