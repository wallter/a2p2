package node

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/store"
)

// [memo] Only a local message or handled report at the next position, a
// Seen and a receipt are quiet (store.Quiet); a replace, a withdraw and a
// level change are counted by Changes alone, so a memo does not outlive
// them.
func TestOnlyWritesThatChangeNoEarlierPositionAreQuiet(t *testing.T) {
	e := newSurf(t)
	ctx := context.Background()
	alice, bob := e.agent("alice"), e.agent("bob")
	st := e.s.m.st
	step := func(what string, quiet bool, fn func()) {
		t.Helper()
		c0, q0 := st.Changes(), st.Quiet()
		fn()
		dc, dq := st.Changes()-c0, st.Quiet()-q0
		if dc == 0 || (dq == dc) != quiet || (!quiet && dq != 0) {
			t.Fatalf("%s: %d writes, %d quiet; want quiet %v", what, dc, dq, quiet)
		}
	}
	var m Sent
	step("a message", true, func() { m = e.send(SendInput{Agent: alice, Text: "hi", To: []string{bob}}) })
	var es []Entry
	step("an inbox listing (its receipts)", true, func() { es = e.inbox(bob) })
	step("a seen", true, func() {
		if _, err := e.s.Seen(ctx, bob, []store.Read{{Key: es[0].Key, Version: es[0].Version}}); err != nil {
			t.Fatal(err)
		}
	})
	step("a handled report", true, func() {
		if _, err := e.s.Ack(ctx, bob, m.ID); err != nil {
			t.Fatal(err)
		}
	})
	step("a replace", false, func() { e.send(SendInput{Agent: alice, Replaces: m.ID, Text: "edited"}) })
	step("a withdraw", false, func() { e.send(SendInput{Agent: alice, Replaces: m.ID, Text: " "}) })
	step("a level change", false, func() {
		if _, err := e.s.Levels(ctx, bob, "", store.LevelAddressed); err != nil {
			t.Fatal(err)
		}
	})
}

// Tests of the memo of a look that showed nothing (reading.go: lookMemo):
// the next look walks on from where it stopped, or does not read the room
// at all, and shows exactly what a look from the cursors shows.

type walkLog struct {
	mu sync.Mutex
	ws []walkSeen
}

type walkSeen struct {
	room, how string
	n         int
}

func (e *surfEnv) walks() *walkLog {
	l := &walkLog{}
	// a slow run must not see a memo expire between two looks of a test
	e.s.m.lookFresh = time.Minute
	e.s.m.inboxWalked = func(room, how string, n int) {
		l.mu.Lock()
		l.ws = append(l.ws, walkSeen{room, how, n})
		l.mu.Unlock()
	}
	return l
}

// take returns the walks since the last take.
func (l *walkLog) take() []walkSeen {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.ws
	l.ws = nil
	return out
}

// fromCursors is agent's inbox as a look from its cursors shows it (no
// memo): the reference every memo look must equal.
func (e *surfEnv) fromCursors(agent string) []Entry {
	e.t.Helper()
	e.s.m.wakes.mu.Lock()
	e.s.m.wakes.looks = nil
	e.s.m.wakes.mu.Unlock()
	return e.inbox(agent)
}

func entryKeys(es []Entry) []string {
	var out []string
	for _, x := range es {
		out = append(out, x.Key+"@"+x.Version)
	}
	return out
}

// [memo] A look walks on from where the last look that showed nothing
// stopped: traffic for others is passed once, not on every look, and
// without a Seen. The cursor does not move: after a restart (no memo) the
// look walks it all again, and finds what it found before.
func TestALookWalksOnFromWhereTheLastEmptyLookStopped(t *testing.T) {
	e := newSurf(t)
	alice, bob, carol := e.agent("alice"), e.agent("bob"), e.agent("carol")
	for i := 0; i < 100; i++ {
		e.send(SendInput{Agent: alice, Text: fmt.Sprintf("to carol %d", i), To: []string{carol}})
	}
	w := e.walks()
	if es := e.inbox(bob); len(es) != 0 {
		t.Fatalf("bob: %+v", es)
	}
	if got := w.take(); len(got) != 1 || got[0].how != walkFromCursor || got[0].n != 100 {
		t.Fatalf("first look: %+v", got)
	}
	for i := 0; i < 5; i++ {
		e.send(SendInput{Agent: alice, Text: fmt.Sprintf("more %d", i), To: []string{carol}})
	}
	e.inbox(bob) // nothing in them for bob: the room is not read
	if got := w.take(); len(got) != 1 || got[0].how != walkSkipped {
		t.Fatalf("second look: %+v", got)
	}
	m := e.send(SendInput{Agent: alice, Text: "for bob", To: []string{bob}})
	es := e.inbox(bob)
	if len(es) != 1 || es[0].ID != m.ID {
		t.Fatalf("a message addressed after the look: %+v", es)
	}
	if got := w.take(); len(got) != 1 || got[0].how != walkFromMemo || got[0].n != 6 {
		t.Fatalf("third look: %+v", got)
	}
	e.restart()
	w = e.walks()
	if es2 := e.inbox(bob); !slices.Equal(entryKeys(es2), entryKeys(es)) {
		t.Fatalf("after a restart: %+v, want %+v", es2, es)
	}
	if got := w.take(); len(got) != 1 || got[0].how != walkFromCursor || got[0].n != 106 {
		t.Fatalf("after a restart the look walks from the cursor: %+v", got)
	}
}

// [memo] A look that does not read the room, because nothing since the
// last one could show the agent anything, and one that cannot use the
// memo: a replace (not quiet) walks from the cursors.
func TestALookDoesNotReadARoomWithNothingNewForTheAgent(t *testing.T) {
	e := newSurf(t)
	alice, bob, carol := e.agent("alice"), e.agent("bob"), e.agent("carol")
	m := e.send(SendInput{Agent: alice, Text: "to carol", To: []string{carol}})
	w := e.walks()
	e.inbox(bob)
	e.send(SendInput{Agent: alice, Text: "to carol again", To: []string{carol}})
	e.read(carol)
	w.take()
	e.inbox(bob)
	if got := w.take(); len(got) != 1 || got[0].how != walkSkipped {
		t.Fatalf("looks: %+v", got)
	}
	e.send(SendInput{Agent: alice, Replaces: m.ID, Text: "replaced"})
	e.inbox(bob)
	if got := w.take(); len(got) != 1 || got[0].how != walkFromCursor {
		t.Fatalf("after a replace: %+v", got)
	}
}

// [memo] A message passed over by an empty look (kept out by the level)
// and then replaced, and a level changed after the look: each look with a
// memo shows exactly what a look from the cursors shows.
func TestALookWithAMemoShowsWhatALookFromTheCursorsShows(t *testing.T) {
	e := newSurf(t)
	ctx := context.Background()
	alice, bob := e.agent("alice"), e.agent("bob")
	if _, err := e.s.Levels(ctx, bob, "", store.LevelAddressed); err != nil {
		t.Fatal(err)
	}
	post := e.send(SendInput{Agent: alice, Text: "room-wide"})
	check := func(what string) []Entry {
		t.Helper()
		got := e.inbox(bob)
		want := e.fromCursors(bob)
		if !slices.Equal(entryKeys(got), entryKeys(want)) {
			t.Fatalf("%s: with the memo %v, from the cursors %v", what, entryKeys(got), entryKeys(want))
		}
		return got
	}
	if es := check("kept out"); len(es) != 0 {
		t.Fatalf("kept out: %+v", es)
	}
	e.inbox(bob) // the memo's look
	e.send(SendInput{Agent: alice, Replaces: post.ID, Text: "@everyone now marked"})
	check("replaced after the look")
	e.inbox(bob)
	if _, err := e.s.Levels(ctx, bob, "", store.LevelAll); err != nil {
		t.Fatal(err)
	}
	check("level raised after the look")
	e.inbox(bob)
	after := e.send(SendInput{Agent: alice, Text: "room-wide after"})
	if es := check("a post after"); len(es) != 1 || es[0].ID != after.ID {
		t.Fatalf("a post after: %+v", es)
	}
	e.read(bob)
	e.inbox(bob)
	e.send(SendInput{Agent: alice, Replaces: after.ID, Text: "after, edited"})
	if es := check("a replacement of a confirmed message"); len(es) != 1 || es[0].Kind != EntryReplaced {
		t.Fatalf("replaced: %+v", es)
	}
}

// [memo] An inbox of every room reads only the rooms with something new
// for the agent: the others are not read at all.
func TestAnInboxOfEveryRoomReadsOnlyTheRoomsWithSomethingNew(t *testing.T) {
	e := newSurf(t)
	ctx := context.Background()
	alice, bob, carol := e.agent("alice"), e.agent("bob"), e.agent("carol")
	var rooms []string
	for i := 0; i < 8; i++ {
		r, err := e.s.CreateRoom(ctx, fmt.Sprintf("r%d", i))
		if err != nil {
			t.Fatal(err)
		}
		rooms = append(rooms, r.ID)
	}
	w := e.walks()
	e.inbox(bob)
	if got := w.take(); len(got) != 9 { // the local room too
		t.Fatalf("first look: %d rooms read: %+v", len(got), got)
	}
	e.send(SendInput{Agent: alice, Room: rooms[2], Text: "to carol", To: []string{carol}})
	m := e.send(SendInput{Agent: alice, Room: rooms[5], Text: "to bob", To: []string{bob}})
	es := e.inbox(bob)
	if len(es) != 1 || es[0].ID != m.ID {
		t.Fatalf("entries: %+v", es)
	}
	read := map[string]string{}
	for _, x := range w.take() {
		read[x.room] = x.how
	}
	for i, r := range rooms {
		want := walkSkipped
		if i == 5 {
			want = walkFromMemo
		}
		if read[r] != want {
			t.Fatalf("room %d read %q, want %q (%v)", i, read[r], want, read)
		}
	}
}
