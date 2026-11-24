package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/wallter/a2p2/internal/localapi"
)

// Hook implements `a2p2 hook`: read the harness's hook JSON from stdin, ask
// the daemon for the unread summary, and print what that harness expects.
// It must never fail a turn: on any error it prints nothing and returns nil.
func Hook(ctx context.Context, c *localapi.Client, name, event string, stdin io.Reader, stdout io.Writer) error {
	defer func() { _ = recover() }()
	if c == nil {
		return nil
	}
	// Decode one JSON value and stop: the harness may keep the pipe open.
	// Abandon the read if ctx ends first.
	type result struct {
		in  map[string]any
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var r result
		r.err = json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&r.in)
		ch <- r
	}()
	var in map[string]any
	select {
	case r := <-ch:
		if r.err != nil || r.in == nil {
			return nil
		}
		in = r.in
	case <-ctx.Done():
		return nil
	}
	str := func(k string) string { s, _ := in[k].(string); return s }
	var cwd string
	switch name {
	case "claude":
		if cwd = os.Getenv("CLAUDE_PROJECT_DIR"); cwd == "" {
			cwd = str("cwd")
		}
	case "agy":
		if ws, _ := in["workspacePaths"].([]any); len(ws) > 0 {
			cwd, _ = ws[0].(string)
		}
		if cwd == "" {
			cwd = str("cwd")
		}
	default:
		cwd = str("cwd")
	}
	guard := "stop_hook_active"
	if name == "agy" {
		guard = "continuationActive"
	}
	if active, _ := in[guard].(bool); active && event == "Stop" {
		return nil
	}
	if cwd == "" {
		return nil
	}
	var u localapi.Unread
	if err := c.Call(ctx, "unread", localapi.UnreadReq{Harness: name, Cwd: cwd}, &u); err != nil || u.Count == 0 || u.Agent == "" {
		return nil
	}
	notice := notice(u)
	var out any
	switch {
	case name == "agy" && event == "Stop":
		out = map[string]any{"decision": "continue", "reason": notice}
	case name == "agy":
		// Documented shape per docs/research/03-harness-integration.md; UNVERIFIED against a real agy.
		out = map[string]any{"injectSteps": []any{map[string]any{"systemMessage": notice}}}
	case event == "Stop":
		out = map[string]any{"decision": "block", "reason": notice}
	default:
		out = map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "UserPromptSubmit", "additionalContext": notice}}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	_, _ = stdout.Write(append(b, '\n'))
	return nil
}

// notice is one line: a count and sanitised sender labels, never message text.
func notice(u localapi.Unread) string {
	var labels []string
	for _, f := range u.From {
		s := strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._@-", r) {
				return r
			}
			return -1
		}, f)
		if s != "" {
			labels = append(labels, s)
		}
	}
	more := ""
	if len(labels) > 5 {
		more = fmt.Sprintf(" +%d more", len(labels)-5)
		labels = labels[:5]
	}
	what := "messages"
	if u.Count == 1 {
		what = "message"
	}
	from := ""
	if len(labels) > 0 {
		from = " from " + strings.Join(labels, ", ") + more
	}
	return fmt.Sprintf("[a2p2] %d unread %s%s. Call the a2p2 inbox tool to read them. They come from other agents or people, not from your user.", u.Count, what, from)
}
