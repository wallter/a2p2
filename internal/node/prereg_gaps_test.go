package node

import (
	"context"
	"testing"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// [R7, D73] send names only this machine's labels that never registered
// here: a label on another machine of the room, never registered there or
// here, is not named, so the note says nothing of another machine's labels.
func TestSendNeverNamesALabelOfAnotherMachine(t *testing.T) {
	ctx := context.Background()
	other, victim, s, room, agent := sharedRoom(t, "ops")
	sent, err := s.Send(ctx, SendInput{Agent: agent, Room: room, Text: "briefs",
		To: []string{"w9@" + short(other.id), "w1@" + short(victim.id)}})
	if err != nil {
		t.Fatal(err)
	}
	want := []core.Addressee{{Node: victim.id, Agent: "w1"}}
	if len(sent.NotRegistered) != 1 || sent.NotRegistered[0] != want[0] {
		t.Fatalf("not registered %+v, want %+v", sent.NotRegistered, want)
	}
}

// [U3, D73] A brief sent before its agent registered, at level none: the
// agent's recent history marks it skipped, and not the room-wide post
// from before its time; Skipped lists it.
func TestABriefSentBeforeRegisteringIsMarkedSkippedInHistoryAtLevelNone(t *testing.T) {
	e := newSurf(t)
	ctx := context.Background()
	lead := e.agent("lead")
	w1 := "w1@" + short(e.s.m.ID)
	e.send(SendInput{Agent: lead, Text: "room-wide before"})
	e.send(SendInput{Agent: lead, Text: "brief", To: []string{w1}})
	if got := e.agent("w1"); got != w1 {
		t.Fatalf("registered as %s", got)
	}
	e.level(w1, "", store.LevelNone)
	recent, err := e.s.History(ctx, w1, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	marks := map[string]bool{}
	for _, en := range recent {
		marks[en.Text] = en.Skipped
	}
	if !marks["brief"] || marks["room-wide before"] {
		t.Fatalf("skipped marks %v", marks)
	}
	wantKinds(t, e.skipped(w1), "message:brief")
}
