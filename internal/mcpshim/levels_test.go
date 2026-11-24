package mcpshim

import (
	"encoding/json"
	"testing"
)

type levelsResult struct {
	MarkedSeen bool `json:"marked_seen"`
	Unread     int  `json:"unread"`
	Rooms      []struct {
		Room   string `json:"room_label"`
		Level  string `json:"level"`
		Unread int    `json:"unread"`
	} `json:"rooms"`
	First []struct {
		ID   string `json:"id"`
		Line string `json:"line"`
	} `json:"first_lines"`
	Levels []struct {
		Room  string `json:"room_label"`
		Level string `json:"level"`
	} `json:"levels"`
	Messages []struct {
		Text string `json:"text"`
	} `json:"messages"`
}

func parseLevels(t *testing.T, s string) levelsResult {
	t.Helper()
	var r levelsResult
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatalf("%v: %s", err, s)
	}
	return r
}

// Requirements: U1, U2, U3, U4. Through the tools: an agent sets its own
// level with inbox, whoami shows it with unread counts, peek gives counts
// and first lines marking nothing, and skipped reads what the level kept
// out, marking nothing.
func TestAnAgentControlsItsLevelAndReadsCheaplyThroughTheTools(t *testing.T) {
	c := startDaemon(t)
	a, b := connect(t, c, "alice"), connect(t, c, "bob")
	agentID(t, a)
	bob := agentID(t, b)
	r := parseLevels(t, ok(t, b, "inbox", map[string]any{"level": "addressed"}))
	if len(r.Levels) != 1 || r.Levels[0].Level != "addressed" || r.Levels[0].Room != "local" {
		t.Fatalf("set level: %+v", r)
	}
	ok(t, a, "send", map[string]any{"text": "room-wide"})
	ok(t, a, "send", map[string]any{"text": "for you\nmore", "to": []string{bob}})
	w := parseLevels(t, ok(t, b, "whoami", nil))
	if len(w.Rooms) != 1 || w.Rooms[0].Level != "addressed" || w.Rooms[0].Unread != 1 {
		t.Fatalf("whoami: %+v", w)
	}
	for i := 0; i < 2; i++ {
		p := parseLevels(t, ok(t, b, "inbox", map[string]any{"peek": true}))
		if p.MarkedSeen || p.Unread != 1 || len(p.Rooms) != 1 || p.Rooms[0].Unread != 1 || len(p.First) != 1 || p.First[0].Line != "for you" {
			t.Fatalf("peek: %+v", p)
		}
	}
	s := parseLevels(t, ok(t, b, "inbox", map[string]any{"skipped": true}))
	if s.MarkedSeen || len(s.Messages) != 1 || s.Messages[0].Text != "room-wide" {
		t.Fatalf("skipped: %+v", s)
	}
	in := parseLevels(t, ok(t, b, "inbox", nil))
	if !in.MarkedSeen || len(in.Messages) != 1 || in.Messages[0].Text != "for you\nmore" {
		t.Fatalf("inbox: %+v", in)
	}
	if s := parseLevels(t, ok(t, b, "inbox", map[string]any{"skipped": true})); len(s.Messages) != 1 {
		t.Fatalf("skipped after reading the inbox: %+v", s)
	}
	if out, isErr := call(t, b, "inbox", map[string]any{"level": "some"}); !isErr {
		t.Fatalf("a level that is not one was accepted: %s", out)
	}
}
