package core

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const testNode = "ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34"

func valid() Envelope {
	now := time.Unix(1_800_000_000, 0).UTC()
	return Envelope{V: 1, ID: NewID(now), Room: LocalRoom, FromNode: testNode,
		From: AgentID("claude-x", testNode), Kind: KindMessage, Seq: 1, CreatedAt: now,
		Body: Body{Text: "hi"}}
}

func TestValidate(t *testing.T) {
	if err := func() error { e := valid(); return e.Validate() }(); err != nil {
		t.Fatalf("valid envelope rejected: %v", err)
	}
	bad := map[string]func(*Envelope){
		"version":        func(e *Envelope) { e.V = 2 },
		"id":             func(e *Envelope) { e.ID = "x" },
		"room":           func(e *Envelope) { e.Room = "secret-room-name" },
		"from_node":      func(e *Envelope) { e.FromNode = "zz" },
		"from spoof":     func(e *Envelope) { e.From = "claude-x@deadbeef" },
		"seq":            func(e *Envelope) { e.Seq = 0 },
		"created":        func(e *Envelope) { e.CreatedAt = time.Time{} },
		"empty text":     func(e *Envelope) { e.Body.Text = "  " },
		"kind":           func(e *Envelope) { e.Kind = "exec" },
		"recipient":      func(e *Envelope) { e.To = []string{"not an agent"} },
		"many to":        func(e *Envelope) { e.To = make([]string, MaxRecipients+1) },
		"big body":       func(e *Envelope) { e.Body.Text = strings.Repeat("a", MaxBodyBytes) },
		"answer orphan":  func(e *Envelope) { e.Kind = KindAnswer },
		"receipt no ref": func(e *Envelope) { e.Kind = KindReceipt; e.Body = Body{State: ReceiptSeen} },
		"receipt state":  func(e *Envelope) { e.Kind = KindReceipt; e.Body = Body{Ref: e.ID, State: "exploded"} },
		"reply_to":       func(e *Envelope) { e.ReplyTo = "nope" },
	}
	for name, mutate := range bad {
		e := valid()
		mutate(&e)
		if err := e.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

func TestNewIDUniqueAndClockIndependent(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	seen := map[string]bool{}
	for i := 0; i < 10_000; i++ {
		id := NewID(now) // same instant every time
		if !ValidID(id) || seen[id] {
			t.Fatalf("bad or duplicate id %q", id)
		}
		seen[id] = true
	}
	if a, b := NewID(now), NewID(now.Add(time.Hour)); !(a < b) {
		t.Errorf("ids should sort by time: %s !< %s", a, b)
	}
}

func TestCleanLabel(t *testing.T) {
	for in, want := range map[string]string{"A2P2": "A2P2", "my project!": "my-project", "///": "agent", "": "agent"} {
		if got := CleanLabel(in); got != want || !ValidLabel(got) {
			t.Errorf("CleanLabel(%q) = %q, want %q", in, got, want)
		}
	}
	if got := CleanLabel(strings.Repeat("x", 200)); !ValidLabel(got + "-99") {
		t.Errorf("long label leaves no room for suffix: %d chars", len(got))
	}
}

func FuzzDecodeEnvelope(f *testing.F) {
	e := valid()
	seed, _ := json.Marshal(e)
	f.Add(seed)
	f.Add([]byte(`{"v":1,"to":[null],"body":{"choices":[""]}}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, b []byte) {
		got, err := DecodeEnvelope(b)
		if err != nil {
			return
		}
		// Anything accepted must survive a round trip and still be valid.
		out, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeEnvelope(out); err != nil {
			t.Fatalf("accepted envelope fails after round trip: %v", err)
		}
	})
}
