package node

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/store"
)

// texts lists the entries' texts in alphabetical order: messages written in
// the same millisecond are ordered by id.
func texts(es []Entry) string {
	var out []string
	for _, e := range es {
		out = append(out, e.Text)
	}
	slices.Sort(out)
	return strings.Join(out, ", ")
}

func (e *surfEnv) level(agent, room, level string) []RoomLevel {
	e.t.Helper()
	ls, err := e.s.Levels(context.Background(), agent, room, level)
	if err != nil {
		e.t.Fatalf("level %s %q: %v", agent, level, err)
	}
	return ls
}

func (e *surfEnv) skipped(agent string) []Entry {
	e.t.Helper()
	es, err := e.s.Skipped(context.Background(), agent, "", "", 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return es
}

// Requirements: U1, D10
func TestAgentAtAddressedSeesWhatIsAddressedToItAndRepliesInItsThreadsButNotRoomWidePosts(t *testing.T) {
	e := newSurf(t)
	alice, bob, carol := e.agent("alice"), e.agent("bob"), e.agent("carol")
	e.level(bob, "", store.LevelAddressed)
	e.send(SendInput{Agent: alice, Text: "room-wide"})
	e.send(SendInput{Agent: alice, Text: "asks", AskHandled: true}) // asks for reports, marked for nobody
	e.send(SendInput{Agent: alice, Text: "@everyone for everyone"})
	e.send(SendInput{Agent: alice, Text: "to bob", To: []string{bob}})
	e.send(SendInput{Agent: alice, Text: "to carol", To: []string{carol}})
	root := e.send(SendInput{Agent: bob, Text: "bob asks"})
	e.send(SendInput{Agent: carol, Text: "carol answers", ReplyTo: root.ID, Thread: true})
	e.send(SendInput{Agent: alice, Text: "alice adds", ReplyTo: root.ID, Thread: true})
	e.send(SendInput{Agent: alice, Text: "alice to the room", ReplyTo: root.ID, To: []string{Everyone}})
	if got := texts(e.read(bob)); got != "@everyone for everyone, alice adds, carol answers, to bob" {
		t.Fatalf("bob at addressed was shown %q", got)
	}
	// Another agent on the same machine, at the default level, is unaffected.
	if got := texts(e.read(carol)); got != "@everyone for everyone, alice adds, alice to the room, asks, bob asks, room-wide, to carol" {
		t.Fatalf("carol at the default level was shown %q", got)
	}
	if got := texts(e.skipped(bob)); got != "alice to the room, asks, room-wide" {
		t.Fatalf("bob skipped %q", got)
	}
}

// Requirements: U1, U3
func TestAgentAtNoneIsShownNothingUntilItAsks(t *testing.T) {
	e := newSurf(t)
	alice, bob := e.agent("alice"), e.agent("bob")
	e.level(bob, "", store.LevelNone)
	e.send(SendInput{Agent: alice, Text: "to bob", To: []string{bob}})
	e.send(SendInput{Agent: alice, Text: "@everyone for everyone", AskHandled: true})
	if got := e.read(bob); len(got) != 0 {
		t.Fatalf("bob at none was shown %q", texts(got))
	}
	for i := 0; i < 2; i++ { // asking twice: asking marks nothing
		if got := texts(e.skipped(bob)); got != "@everyone for everyone, to bob" {
			t.Fatalf("bob asked and got %q", got)
		}
	}
	// Skipped showed the text: bob may report it handled (D-reading).
	if _, err := e.s.Ack(context.Background(), bob, e.skipped(bob)[0].ID); err != nil {
		t.Fatalf("handled refused for a message skipped showed: %v", err)
	}
}

// Requirements: U2, U3
func TestRaisingTheLevelShowsWhatWasSkippedWithoutMarkingItSeenAndTheInboxContinuesFromNow(t *testing.T) {
	e := newSurf(t)
	alice, bob := e.agent("alice"), e.agent("bob")
	if ls := e.level(bob, "", store.LevelAddressed); len(ls) != 1 || ls[0].Level != store.LevelAddressed || ls[0].Label != LocalLabel {
		t.Fatalf("levels = %+v", ls)
	}
	e.send(SendInput{Agent: alice, Text: "one"})
	e.read(bob)
	e.send(SendInput{Agent: alice, Text: "two"})
	e.level(bob, "", store.LevelAll)
	e.send(SendInput{Agent: alice, Text: "three"})
	if got := texts(e.read(bob)); got != "three" {
		t.Fatalf("after raising, the inbox showed %q", got)
	}
	if got := texts(e.skipped(bob)); got != "one, two" {
		t.Fatalf("skipped %q", got)
	}
	if got := e.read(bob); len(got) != 0 {
		t.Fatalf("reading what was skipped put %q in the inbox", texts(got))
	}
	// Skipped messages stay readable, the newest limit of them.
	if es, err := e.s.Skipped(context.Background(), bob, LocalLabel, "", 1); err != nil || len(es) != 1 {
		t.Fatalf("skipped limit 1 = %q, %v", texts(es), err)
	}
}

// Requirements: U4
func TestCountsAndFirstLinesFollowTheLevelAndMarkNothing(t *testing.T) {
	e := newSurf(t)
	alice, bob := e.agent("alice"), e.agent("bob")
	e.level(bob, "", store.LevelAddressed)
	e.send(SendInput{Agent: alice, Text: "room-wide"})
	e.send(SendInput{Agent: alice, Text: "to bob\nsecond line", To: []string{bob}})
	for i := 0; i < 2; i++ {
		u, err := e.s.Unread(context.Background(), bob, "", true)
		if err != nil {
			t.Fatal(err)
		}
		if u.Count != 1 || len(u.Rooms) != 1 || u.Rooms[0].Count != 1 || len(u.First) != 1 || u.First[0].Line != "to bob" {
			t.Fatalf("unread = %+v", u)
		}
	}
	if got := texts(e.read(bob)); got != "to bob\nsecond line" {
		t.Fatalf("counts and first lines marked something: inbox %q", got)
	}
}

// Requirements: U1, U2, D1
func TestLevelsAndSkipsSurviveARestart(t *testing.T) {
	e := newSurf(t)
	alice, bob := e.agent("alice"), e.agent("bob")
	e.level(bob, "", store.LevelNone)
	e.send(SendInput{Agent: alice, Text: "one"})
	e.level(bob, "", store.LevelAll)
	e.restart()
	if ls := e.level(bob, "", ""); ls[0].Level != store.LevelAll {
		t.Fatalf("levels = %+v", ls)
	}
	// Right after the first restart, at all: the skip survived.
	if got := texts(e.skipped(bob)); got != "one" {
		t.Fatalf("skipped after the first restart %q", got)
	}
	if got := e.read(bob); len(got) != 0 {
		t.Fatalf("after the first restart the inbox showed %q", texts(got))
	}
	e.level(bob, "", store.LevelAddressed)
	e.restart()
	if ls := e.level(bob, "", ""); ls[0].Level != store.LevelAddressed {
		t.Fatalf("after a restart, levels = %+v", ls)
	}
	e.send(SendInput{Agent: alice, Text: "two"})
	if got := e.read(bob); len(got) != 0 {
		t.Fatalf("shown %q", texts(got))
	}
	if got := texts(e.skipped(bob)); got != "one, two" {
		t.Fatalf("skipped %q", got)
	}
	ls, err := e.s.AgentLevels(context.Background())
	if err != nil || len(ls) != 1 || ls[0].Agent != bob || ls[0].Level != store.LevelAddressed {
		t.Fatalf("agent levels = %+v, %v", ls, err)
	}
}

// Requirements: U1, U2. One agent with opposite levels in two rooms: each
// room follows its own level, and changing one leaves the other as it was.
func TestOneAgentsLevelsInTwoRoomsAreIndependent(t *testing.T) {
	ctx := context.Background()
	e := newSurf(t)
	alice, bob := e.agent("alice"), e.agent("bob")
	if _, err := e.s.CreateRoom(ctx, "team"); err != nil {
		t.Fatal(err)
	}
	e.level(bob, LocalLabel, store.LevelNone)
	e.level(bob, "team", store.LevelAll)
	levels := func() string {
		var out []string
		for _, l := range e.level(bob, "", "") {
			out = append(out, l.Label+"="+l.Level)
		}
		slices.Sort(out)
		return strings.Join(out, " ")
	}
	if got := levels(); got != "local=none team=all" {
		t.Fatalf("levels %s", got)
	}
	e.send(SendInput{Agent: alice, Text: "local post"})
	e.send(SendInput{Agent: alice, Room: "team", Text: "team post"})
	if got := texts(e.read(bob)); got != "team post" {
		t.Fatalf("shown %q", got)
	}
	e.level(bob, "team", store.LevelNone)
	if got := levels(); got != "local=none team=none" {
		t.Fatalf("after changing team: %s", got)
	}
	e.level(bob, LocalLabel, store.LevelAll)
	if got := levels(); got != "local=all team=none" {
		t.Fatalf("after changing local: %s", got)
	}
	e.send(SendInput{Agent: alice, Text: "local again"})
	e.send(SendInput{Agent: alice, Room: "team", Text: "team again"})
	if got := texts(e.read(bob)); got != "local again" {
		t.Fatalf("shown %q", got)
	}
	if got := texts(e.skipped(bob)); got != "local post, team again" {
		t.Fatalf("skipped %q", got)
	}
}

// Requirements: U1, U2, U3. The person is not an agent with a level, and an
// agent registered later has skipped nothing from before its time.
func TestThePersonHasNoLevelAndALaterAgentSkippedNothingBeforeItsTime(t *testing.T) {
	e := newSurf(t)
	alice := e.agent("alice")
	human := e.s.HumanID()
	if _, err := e.s.Levels(context.Background(), human, "", store.LevelNone); !isUser(err) {
		t.Fatalf("the person's level was set: %v", err)
	}
	if _, err := e.s.Levels(context.Background(), alice, "", "some"); !isUser(err) {
		t.Fatalf("a level that is not one: %v", err)
	}
	e.send(SendInput{Agent: alice, Text: "before dave"})
	dave := e.agent("dave")
	e.level(dave, "", store.LevelNone)
	if got := e.skipped(dave); len(got) != 0 {
		t.Fatalf("dave skipped %q from before his time", texts(got))
	}
	e.send(SendInput{Agent: alice, Text: "question", To: []string{human}})
	if got := texts(e.read(human)); got != "before dave, question" {
		t.Fatalf("the person was shown %q", got)
	}
}
