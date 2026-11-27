package node

// D73 and a room set aside (D57): a label registered while a room is set
// aside starts at the current end of that room's streams, as in 0.3.0;
// only a known frontier whose head is not held yet defers a stream.

import (
	"database/sql"
	"testing"

	"github.com/wallter/a2p2/internal/core"
)

// setAsideWhile sets room aside on n by breaking its create entry's
// signature, runs fn, and puts the signature back, so the room opens
// again at the restart that follows.
func setAsideWhile(t *testing.T, n *lnode, room string, fn func()) {
	t.Helper()
	create := n.r.rooms[room].createID
	var sig []byte
	editLog(t, n, func(db *sql.DB) {
		if err := db.QueryRow(`SELECT sig FROM log WHERE room = ? AND id = ?`, room, create).Scan(&sig); err != nil {
			t.Fatal(err)
		}
		exec(t, db, `UPDATE log SET sig = ? WHERE room = ? AND id = ?`, make([]byte, 64), room, create)
	})
	if !n.r.isQuarantined(room) {
		t.Fatal("room not set aside")
	}
	fn()
	editLog(t, n, func(db *sql.DB) {
		exec(t, db, `UPDATE log SET sig = ? WHERE room = ? AND id = ?`, sig, room, create)
	})
	if n.r.isQuarantined(room) {
		t.Fatal("room still set aside")
	}
}

func entryTexts(es []Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Kind+":"+e.Text)
	}
	return out
}

// [U3, D73] A label registered while its room is set aside, everything
// admitted: when the room opens again, the room-wide history from before
// the registration is not unread for it (as in 0.3.0), and what is written
// after is.
func TestALabelRegisteredWhileItsRoomIsSetAsideDoesNotReadTheOldHistoryWhenItOpens(t *testing.T) {
	a := newLNode(t, "a", lclock())
	a.register("alice")
	room := a.create("bad")
	for _, text := range []string{"old 1", "old 2", "old 3"} {
		a.send(room, "alice", text)
	}
	setAsideWhile(t, a, room, func() { a.register("late") })
	wantKinds(t, a.inbox(room, "late"))
	a.send(room, "alice", "new")
	wantKinds(t, a.inbox(room, "late"), "message:new")
}

// [U3, T3, D73] The same on a machine admitted without history, whose
// frontier is known again when the room opens and does not admit
// everything: the label starts at the current end, so neither the text
// before the frontier nor the room-wide messages after it and before the
// registration are unread; what follows is.
func TestALabelRegisteredWhileARoomAdmittedWithoutHistoryIsSetAsideStartsAtTheEnd(t *testing.T) {
	a, b, room := twoAdmins(t)
	a.register("alice")
	a.send(room, "alice", "before e")
	for _, r := range a.records(room) {
		b.take(r)
	}
	e := newLNode(t, "e", a.clock)
	b.change(room, Change{Kind: core.EntryAdd, Target: e.id, Name: "eee"})
	e.pin(room, b)
	e.pull(room, b)
	take := func() {
		for _, r := range a.records(room) {
			e.take(r)
		}
	}
	take()
	a.send(room, "alice", "after e")
	take()
	setAsideWhile(t, e, room, func() { e.register("late") })
	if es := e.inbox(room, "late"); len(es) != 0 {
		t.Fatalf("old history unread: %q", entryTexts(es))
	}
	a.send(room, "alice", "new")
	take()
	wantKinds(t, e.inbox(room, "late"), "message:new")
}
