package node

// Group A of the 2026-11-25 security review: requests and replay. One
// hostile admin cannot fill another machine's request queue (F1), a stored
// log loads under stricter receipt rules and reworded reasons (F71), one
// room whose log does not replay is set aside while the others open, and
// leaving a room does not run its file deletions under the rooms lock (F68).

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/members"
	"github.com/wallter/a2p2/internal/store"
)

// threeAdmins is a room with admins a, b and c, as the review's proof
// builds it.
func threeAdmins(t *testing.T) (a, b, c *lnode, room string) {
	t.Helper()
	clock := lclock()
	a, b, c = newLNode(t, "a", clock), newLNode(t, "b", clock), newLNode(t, "c", clock)
	room = a.create("team")
	a.change(room, Change{Kind: core.EntryAdmin, Target: b.id, Name: "bee", History: true})
	b.pin(room, a)
	b.pull(room, a)
	a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "sea", History: true})
	a.change(room, Change{Kind: core.EntryAdmin, Target: c.id})
	b.pull(room, a)
	b.confirm(b.requests(room)[0].ID)
	a.pull(room, b)
	c.pin(room, b)
	c.pull(room, b)
	wantNodes(t, "admins", c.state(room).Admins, a.id, b.id, c.id)
	return a, b, c, room
}

func fakeNode(i int) string { return strings.Repeat("f", 60) + fmt.Sprintf("%04x", i+1) }

// flood has b sign n lone admin entries for made-up machines, each waiting
// for a second admin, and returns them in the order signed.
func flood(t *testing.T, b *lnode, room string, from, n int) []string {
	t.Helper()
	var out []string
	for i := from; i < from+n; i++ {
		got := b.change(room, Change{Kind: core.EntryAdmin, Target: fakeNode(i), Name: "x"})
		if got.Effect != EffectPending || got.Reason != members.ReasonNeedsSecond {
			t.Fatalf("flood entry %d: %+v", i, got)
		}
		out = append(out, got.Entry)
	}
	return out
}

// raisedFor reports whether n holds a request, in any state, that seconds
// entry.
func raisedFor(t *testing.T, n *lnode, room, entry string) bool {
	t.Helper()
	qs, err := n.st.Requests(context.Background(), room, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range qs {
		if q.Seconds == entry {
			return true
		}
	}
	return false
}

// The review's proof (TestAuditHostileAdminFloodBlocksItsRemoval), turned
// round: b signs more waiting admin entries than any queue holds; a still
// proposes b's removal, and the removal is raised on c at once, without
// declines or a restart, so the other two remove b.
//
// Requirements: T9, P3; security review F1
func TestHostileAdminFloodingSecondRequestsCannotBlockItsRemoval(t *testing.T) {
	ctx := context.Background()
	a, b, c, room := threeAdmins(t)
	flood(t, b, room, 0, 40)
	a.pull(room, b)
	c.pull(room, b)
	if na, nc := len(a.requests(room)), len(c.requests(room)); na != soloLimit || nc != soloLimit {
		t.Fatalf("pending after the flood: a=%d c=%d, want %d each", na, nc, soloLimit)
	}

	// a proposes b's removal with b's requests still waiting.
	a.tick()
	q, err := a.r.proposeAny(ctx, room, Change{Kind: core.EntryRemove, Target: b.id})
	if err != nil {
		t.Fatalf("propose remove(b) on flooded a: %v", err)
	}
	rm := a.confirm(q.ID)
	if rm.Effect != EffectPending || rm.Reason != members.ReasonNeedsSecond {
		t.Fatalf("remove(b) on a: %+v", rm)
	}

	// It reaches c, whose queue holds b's requests: it is raised.
	c.pull(room, a)
	if !raisedFor(t, c, room, rm.Entry) {
		t.Fatal("remove(b) was not raised on c")
	}
	var second store.Request
	for _, q := range c.requests(room) {
		if q.Seconds == rm.Entry {
			second = q
		}
	}
	if second.Action != ActionRemove || second.Node != b.id {
		t.Fatalf("c's request: %+v", second)
	}
	c.confirm(second.ID)
	if st := c.state(room); slices.Contains(st.Admins, b.id) || slices.Contains(st.Members, b.id) {
		t.Fatalf("b still in the room on c: %+v", st)
	}
	a.pull(room, c)
	if st := a.state(room); slices.Contains(st.Members, b.id) {
		t.Fatal("b still a member on a")
	}
}

// A flooder's waiting entries are raised a few at a time: as c's person
// declines one, the next is raised at once, and a restart raises none
// twice. b's entries never take more than soloLimit places: a keeps the
// places of its own budget.
//
// Requirements: P3; security review F1 (raise retries)
func TestAFloodersWaitingEntriesAreRaisedAFewAtATimeAsThePersonDecides(t *testing.T) {
	ctx := context.Background()
	_, b, c, room := threeAdmins(t)
	flood(t, b, room, 0, soloLimit+6)
	c.pull(room, b)
	first := c.requests(room)
	if len(first) != soloLimit {
		t.Fatalf("pending: %d", len(first))
	}
	seen := map[string]bool{}
	for _, q := range first {
		seen[q.Seconds] = true
	}
	if _, err := c.r.Confirm(ctx, first[0].ID, false); err != nil {
		t.Fatal(err)
	}
	next := c.requests(room)
	if len(next) != soloLimit {
		t.Fatalf("pending after one decline: %d", len(next))
	}
	fresh := 0
	for _, q := range next {
		if !seen[q.Seconds] {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("entries newly raised after a decline: %d", fresh)
	}
	c.restart()
	if n := len(c.requests(room)); n != soloLimit {
		t.Fatalf("pending after a restart: %d", n)
	}
	all, err := c.st.Requests(ctx, room, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != soloLimit+1 {
		t.Fatalf("requests in all: %d", len(all))
	}
}

// A waiting entry with no request (an older version skipped it while its
// queue was full, or a place was freed without a raise) is raised when the
// daemon starts.
//
// Requirements: P3; security review F1 (raise at load)
func TestAWaitingEntryLeftUnraisedIsRaisedAtStart(t *testing.T) {
	ctx := context.Background()
	_, b, c, room := threeAdmins(t)
	flood(t, b, room, 0, soloLimit+2)
	c.pull(room, b)
	qs := c.requests(room)
	if len(qs) != soloLimit {
		t.Fatalf("pending: %d", len(qs))
	}
	// Decided in the store alone: no raise follows.
	if err := c.st.DecideRequest(ctx, qs[0].ID, store.RequestDeclined, "", *c.clock); err != nil {
		t.Fatal(err)
	}
	if n := len(c.requests(room)); n != soloLimit-1 {
		t.Fatalf("pending after the store decline: %d", n)
	}
	c.restart()
	if n := len(c.requests(room)); n != soloLimit {
		t.Fatalf("pending after a restart: %d, want %d", n, soloLimit)
	}
}

// soloLimit is what one admin's waiting entries can hold on c in
// threeAdmins when the third admin has none: the room-wide budget less
// the places kept for the other admin's own budget.
const soloLimit = MaxRaisedRequests - MaxRaisedPerSigner

// A pending removal of an admin takes no place in the room-wide budget:
// with 31 ordinary requests and b's removal of a pending on c, a's
// ordinary entry still gets the last place, and c's approval seconds it.
//
// Requirements: T9, P3; security review F1 (removals in their own allowance)
func TestAPendingRemovalOfAnAdminTakesNoPlaceFromOtherRequests(t *testing.T) {
	ctx := context.Background()
	a, b, c, room := threeAdmins(t)
	ids := flood(t, b, room, 0, MaxRaisedRequests)
	c.pull(room, b)
	// Fill c's queue to one short of the budget, as an older version
	// could leave it.
	for i, id := range ids {
		if len(c.requests(room)) >= MaxRaisedRequests-1 {
			break
		}
		if raisedFor(t, c, room, id) {
			continue
		}
		q := store.Request{ID: fmt.Sprintf("q_%032x", i+1), Kind: string(core.EntryAdmin), Room: room, Node: fakeNode(i),
			Name: "x", Seconds: id, Action: ActionAdmitAdmin, Role: RoleAdmin, Created: *c.clock, State: store.RequestPending}
		if err := c.st.CreateRequest(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(c.requests(room)); n != MaxRaisedRequests-1 {
		t.Fatalf("pending on c: %d", n)
	}
	b.tick()
	q, err := b.r.proposeAny(ctx, room, Change{Kind: core.EntryRemove, Target: a.id})
	if err != nil {
		t.Fatal(err)
	}
	rm := b.confirm(q.ID)
	c.pull(room, b)
	if _, ok := pendingFor(c, room, rm.Entry); !ok {
		t.Fatal("remove(a) not raised on c")
	}
	honest := flood(t, a, room, 100, 1)[0]
	c.pull(room, a)
	second, ok := pendingFor(c, room, honest)
	if !ok {
		t.Fatalf("a's entry not raised: b's pending removal took the last place (%d pending)", len(c.requests(room)))
	}
	c.confirm(second.ID)
	if !slices.Contains(c.state(room).Admins, fakeNode(100)) {
		t.Fatal("a's entry not seconded on c")
	}
}

// An honest admin's fifth waiting entry is raised when the room has
// spare places, and the approval seconds it.
//
// Requirements: M4, P3; security review F1 (own budget binds only under contention)
func TestAnHonestAdminsFifthEntryIsRaisedWhenTheRoomHasSparePlaces(t *testing.T) {
	_, b, c, room := threeAdmins(t)
	ids := flood(t, b, room, 0, MaxRaisedPerSigner+2)
	c.pull(room, b)
	for i, id := range ids {
		if _, ok := pendingFor(c, room, id); !ok {
			t.Fatalf("entry %d of %d not raised; pending: %d", i+1, len(ids), len(c.requests(room)))
		}
	}
	q, _ := pendingFor(c, room, ids[MaxRaisedPerSigner])
	if q.Action != ActionAdmitAdmin {
		t.Fatalf("c's request: %+v", q)
	}
	c.confirm(q.ID)
	if !slices.Contains(c.state(room).Admins, fakeNode(MaxRaisedPerSigner)) {
		t.Fatal("the fifth entry was not seconded on c")
	}
}

// Under contention each admin keeps its own places: b's flood takes the
// spare places, a's first MaxRaisedPerSigner entries are still raised,
// and its next one waits for a place.
//
// Requirements: T9, P3; security review F1 (own budget under contention)
func TestUnderContentionEachAdminKeepsThePlacesOfItsOwnBudget(t *testing.T) {
	a, b, c, room := threeAdmins(t)
	flood(t, b, room, 0, 40)
	c.pull(room, b)
	if n := len(c.requests(room)); n != soloLimit {
		t.Fatalf("b's entries raised on c: %d, want %d", n, soloLimit)
	}
	ids := flood(t, a, room, 100, MaxRaisedPerSigner+1)
	c.pull(room, a)
	for i, id := range ids {
		if _, ok := pendingFor(c, room, id); ok != (i < MaxRaisedPerSigner) {
			t.Fatalf("a's entry %d raised: %v", i+1, ok)
		}
	}
	if n := len(c.requests(room)); n != MaxRaisedRequests {
		t.Fatalf("pending on c: %d", n)
	}
}

// Requests raised to second another admin's entries do not count against
// the person's own sends with files.
//
// Requirements: P3; security review F1 (sibling: the send budget)
func TestSecondingRequestsDoNotFillTheSendWithFilesBudget(t *testing.T) {
	ctx := context.Background()
	_, b, c, room := threeAdmins(t)
	flood(t, b, room, 0, 4)
	c.pull(room, b)
	for i := 0; i < MaxPendingRequests; i++ {
		q := store.Request{ID: fmt.Sprintf("q_%032x", i+1), Kind: store.RequestSendFiles, Room: room,
			Created: *c.clock, State: store.RequestPending, Draft: "{}", Terms: "t"}
		if err := c.st.CreateSendRequest(ctx, q, nil, MaxPendingRequests); err != nil {
			t.Fatalf("send request %d with %d seconding requests waiting: %v", i, MaxRaisedPerSigner, err)
		}
	}
	q := store.Request{ID: fmt.Sprintf("q_%032x", 999), Kind: store.RequestSendFiles, Room: room,
		Created: *c.clock, State: store.RequestPending, Draft: "{}", Terms: "t"}
	if err := c.st.CreateSendRequest(ctx, q, nil, MaxPendingRequests); err == nil {
		t.Fatal("the own budget is not capped")
	}
}

// editLog runs fn over n's database with n closed, then opens n again.
func editLog(t *testing.T, n *lnode, fn func(db *sql.DB)) {
	t.Helper()
	n.st.Close()
	db, err := sql.Open("sqlite", store.FileURI(n.path))
	if err != nil {
		t.Fatal(err)
	}
	fn(db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	n.open()
}

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	res, err := db.Exec(q, args...)
	if err != nil {
		t.Fatal(err)
	}
	if k, _ := res.RowsAffected(); k != 1 {
		t.Fatalf("%s: %d rows", q, k)
	}
}

// An entry stored when the rules were laxer than today's (here a name
// that today's receipt rule refuses) still loads: stored entries are
// checked for form and signature only.
//
// Requirements: DESIGN section 5 (replay); security review F71
func TestAnEntryStoredBeforeAStricterRuleDoesNotStopTheDaemon(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	room := a.create("team")
	add := a.change(room, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee"})
	other := a.create("other")

	rl := a.r.rooms[room]
	h := rl.ents[add.Entry].Header()
	lax := bytes.Replace(h, []byte(`"name":"bee"`), []byte(`"name":"b!ee"`), 1)
	if bytes.Equal(lax, h) {
		t.Fatalf("header has no name: %s", h)
	}
	sig := ed25519.Sign(a.key, lax)
	if _, err := core.EntryFromParts(lax, sig); err == nil {
		t.Fatal("today's rule accepts the name: the test proves nothing")
	}
	if _, err := core.StoredEntryFromParts(lax, sig); err != nil {
		t.Fatalf("lenient parse: %v", err)
	}
	editLog(t, a, func(db *sql.DB) {
		exec(t, db, `UPDATE log SET id = ?, bytes = ?, sig = ? WHERE room = ? AND id = ?`,
			core.EntryID(lax), lax, sig, room, add.Entry)
	})
	if q := a.r.Quarantined(); len(q) != 0 {
		t.Fatalf("set aside: %+v", q)
	}
	wantNodes(t, "members", a.state(room).Members, a.id, b.id)
	wantNodes(t, "members of the other room", a.state(other).Members, a.id)
}

// A stored reason in other words than this version's (a reworded reason)
// does not stop the start, and the stored text is left as it is, so the
// version that wrote it still opens the database.
//
// Requirements: DESIGN section 5 (replay); security review F71
func TestReplayAfterAReasonIsRewordedStillStarts(t *testing.T) {
	ctx := context.Background()
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	room := a.create("team")
	add := a.change(room, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee"})
	editLog(t, a, func(db *sql.DB) {
		exec(t, db, `UPDATE log SET reason = 'worded by an older version' WHERE room = ? AND id = ?`, room, add.Entry)
	})
	if q := a.r.Quarantined(); len(q) != 0 {
		t.Fatalf("set aside: %+v", q)
	}
	wantNodes(t, "members", a.state(room).Members, a.id, b.id)
	rows, err := a.st.Log(ctx, room)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == add.Entry && row.Reason != "worded by an older version" {
			t.Fatalf("stored reason rewritten: %q", row.Reason)
		}
	}
	// The room still changes as usual (b is gone for good: no catch-up).
	q := a.propose(room, Change{Kind: core.EntryRemove, Target: b.id})
	if c, err := a.r.ConfirmWith(ctx, q.ID, true, ConfirmOpts{NotCaughtUpOK: true}); err != nil || c.State != store.RequestApproved {
		t.Fatalf("remove b: %+v %v", c, err)
	}
	wantNodes(t, "members", a.state(room).Members, a.id)
}

// A room whose stored log replays to another disposition, or whose
// stored entry no longer verifies, is set aside: the daemon starts, the
// other rooms open and change, the set-aside room is not served and
// nothing of it is deleted. Leaving it works.
//
// Requirements: DESIGN section 5 (replay); security review F71
func TestARoomWhoseLogDoesNotReplayIsSetAsideAndTheOthersOpen(t *testing.T) {
	ctx := context.Background()
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	bad := a.create("bad")
	add := a.change(bad, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee"})
	broken := a.create("broken")
	brokenCreate := a.r.rooms[broken].createID
	good := a.create("good")
	before, err := a.st.Log(ctx, bad)
	if err != nil {
		t.Fatal(err)
	}
	editLog(t, a, func(db *sql.DB) {
		exec(t, db, `UPDATE log SET state = 'refused' WHERE room = ? AND id = ?`, bad, add.Entry)
		exec(t, db, `UPDATE log SET sig = ? WHERE room = ? AND id = ?`, make([]byte, 64), broken, brokenCreate)
	})
	q := a.r.Quarantined()
	if len(q) != 2 || q[0].Room > q[1].Room {
		t.Fatalf("set aside: %+v", q)
	}
	for _, x := range q {
		switch x.Room {
		case bad:
			if x.Label != "bad" || !strings.Contains(x.Reason, "replay decided") {
				t.Fatalf("bad: %+v", x)
			}
		case broken:
			if !strings.Contains(x.Reason, "does not verify") {
				t.Fatalf("broken: %+v", x)
			}
		default:
			t.Fatalf("unexpected: %+v", x)
		}
	}
	if _, err := a.r.State(bad); err != ErrNoRoom {
		t.Fatalf("state of a set-aside room: %v", err)
	}
	if _, err := a.r.Offer(bad); err != ErrNoRoom {
		t.Fatalf("offer of a set-aside room: %v", err)
	}
	if _, err := a.r.Receive(ctx, bad, 0); err != ErrNoRoom {
		t.Fatalf("receive into a set-aside room: %v", err)
	}
	after, err := a.st.Log(ctx, bad)
	if err != nil || len(after) != len(before) {
		t.Fatalf("log of a set-aside room: %d rows, was %d (%v)", len(after), len(before), err)
	}
	// The good room opens and changes.
	a.change(good, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee"})
	wantNodes(t, "good members", a.state(good).Members, a.id, b.id)
	// A restart sets it aside again; leaving it clears it.
	a.restart()
	if len(a.r.Quarantined()) != 2 {
		t.Fatal("not set aside after a restart")
	}
	if err := a.r.Leave(ctx, bad); err != nil {
		t.Fatal(err)
	}
	if q := a.r.Quarantined(); len(q) != 1 || q[0].Room != broken {
		t.Fatalf("after leaving: %+v", q)
	}
	a.restart()
	if q := a.r.Quarantined(); len(q) != 1 {
		t.Fatalf("after leaving and a restart: %+v", q)
	}
}

// A local room set aside does not stop the surface opening, and status
// lists the rooms set aside.
//
// Requirements: security review F71 (visible in status)
func TestStatusListsARoomSetAside(t *testing.T) {
	ctx := context.Background()
	e := newSurf(t)
	local := e.s.LocalRoom()
	e.st.Close()
	db, err := sql.Open("sqlite", store.FileURI(e.path))
	if err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`UPDATE log SET sig = ? WHERE room = ? AND batch = 0`, make([]byte, 64), local)
	if err != nil {
		t.Fatal(err)
	}
	if k, _ := res.RowsAffected(); k != 1 {
		t.Fatalf("rows: %d", k)
	}
	db.Close()
	e.open()
	st, err := e.s.Status(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Quarantined) != 1 || st.Quarantined[0].Room != local {
		t.Fatalf("status: %+v", st.Quarantined)
	}
	for _, r := range st.Rooms {
		if r.ID == local {
			t.Fatal("the local room set aside is listed as open")
		}
	}
}

// Leaving a room runs its file deletions only after the rooms lock is
// released, so a room with many files does not stall the other rooms;
// the deletions still run before Leave returns, and none stays queued.
// (Other file work still waits for them: they hold the store's file
// lock.)
//
// Requirements: DESIGN section 7 (leave); security review F68
func TestLeavingARoomDoesNotHoldTheRoomsLockWhileItsFilesAreDeleted(t *testing.T) {
	ctx := context.Background()
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	a.sendFiles(room, "alice", "with files", writeSource(t, "x.txt", "left with its file"),
		writeSource(t, "y.txt", "a second file"), writeSource(t, "z.txt", "and a third"))
	held := make(chan bool, 1)
	testHook = func(p string) error {
		if p == "leave:trash" {
			ok := a.r.mu.TryLock()
			if ok {
				a.r.mu.Unlock()
			}
			held <- !ok
		}
		return nil
	}
	t.Cleanup(func() { testHook = nil })
	if err := a.r.Leave(ctx, room); err != nil {
		t.Fatal(err)
	}
	select {
	case h := <-held:
		if h {
			t.Fatal("the rooms lock was held while the room's files were deleted")
		}
	default:
		t.Fatal("the deletions did not run after the lock was released")
	}
	db, err := sql.Open("sqlite", store.FileURI(a.path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Where a directory cannot be synced (Windows) a removal that was done
	// stays in the queue, marked unlinked, until a start sees the path gone
	// (store/files_windows.go); only a removal not yet done is a failure.
	var n, done int
	if err := db.QueryRow(`SELECT count(*), coalesce(sum(unlinked), 0) FROM file_trash`).Scan(&n, &done); err != nil {
		t.Fatal(err)
	}
	if n-done != 0 || runtime.GOOS != "windows" && n != 0 {
		t.Fatalf("%d deletions still queued after Leave, %d of them done but not yet known durable", n, done)
	}
	q, err := db.Query(`SELECT path FROM file_trash`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	for q.Next() {
		var rel string
		if err := q.Scan(&rel); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(filepath.Dir(a.path), filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("%s is marked removed but is there: %v", rel, err)
		}
	}
	if err := q.Err(); err != nil {
		t.Fatal(err)
	}
}

// pendingFor is n's pending request that seconds entry, if any.
func pendingFor(n *lnode, room, entry string) (store.Request, bool) {
	for _, q := range n.requests(room) {
		if q.Seconds == entry {
			return q, true
		}
	}
	return store.Request{}, false
}

// The honest admin's budgets on the other machine are full (soloLimit
// of its entries wait there): its removal of an admin is still raised. A
// second removal of the same admin by the same signer is not raised past
// the budget.
//
// Requirements: T9, P3; security review F1 (removal past the per-signer budget)
func TestARemovalOfAnAdminIsRaisedPastAFullPerSignerBudget(t *testing.T) {
	ctx := context.Background()
	a, b, c, room := threeAdmins(t)
	flood(t, a, room, 0, soloLimit+1)
	c.pull(room, a)
	if n := len(c.requests(room)); n != soloLimit {
		t.Fatalf("a's entries raised on c: %d", n)
	}
	a.tick()
	q, err := a.r.proposeAny(ctx, room, Change{Kind: core.EntryRemove, Target: b.id})
	if err != nil {
		t.Fatal(err)
	}
	rm := a.confirm(q.ID)
	if rm.Effect != EffectPending {
		t.Fatalf("remove(b) on a: %+v", rm)
	}
	c.pull(room, a)
	second, ok := pendingFor(c, room, rm.Entry)
	if !ok || second.Action != ActionRemove || second.Node != b.id {
		t.Fatalf("remove(b) not raised on c past a's full budget: %+v", c.requests(room))
	}
	c.confirm(second.ID)
	if slices.Contains(c.state(room).Members, b.id) {
		t.Fatal("b still a member on c")
	}
}

// The room-wide budget holds: with 32 seconding requests pending (as 0.2.3
// could leave them, all for one flooding admin), another admin's ordinary
// waiting entry is not raised, but its removal of the flooder is.
//
// Requirements: T9, P3; security review F1 (room-wide budget, inherited rows)
func TestTheRoomWideSecondingBudgetHoldsAndARemovalPassesIt(t *testing.T) {
	ctx := context.Background()
	a, b, c, room := threeAdmins(t)
	ids := flood(t, b, room, 0, MaxRaisedRequests+4)
	c.pull(room, b)
	// Fill c's queue with requests for b's other waiting entries, as an
	// older version raised them.
	for i, id := range ids {
		if raisedFor(t, c, room, id) {
			continue
		}
		if len(c.requests(room)) >= MaxRaisedRequests {
			break
		}
		q := store.Request{ID: fmt.Sprintf("q_%032x", i+1), Kind: string(core.EntryAdmin), Room: room, Node: fakeNode(i),
			Name: "x", Seconds: id, Action: ActionAdmitAdmin, Role: RoleAdmin, Created: *c.clock, State: store.RequestPending}
		if err := c.st.CreateRequest(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(c.requests(room)); n != MaxRaisedRequests {
		t.Fatalf("pending on c: %d", n)
	}
	honest := flood(t, a, room, 100, 1)[0]
	c.pull(room, a)
	if raisedFor(t, c, room, honest) {
		t.Fatal("an ordinary entry was raised past the room-wide budget")
	}
	a.tick()
	q, err := a.r.proposeAny(ctx, room, Change{Kind: core.EntryRemove, Target: b.id})
	if err != nil {
		t.Fatal(err)
	}
	rm := a.confirm(q.ID)
	c.pull(room, a)
	if _, ok := pendingFor(c, room, rm.Entry); !ok {
		t.Fatal("remove(b) not raised past the room-wide budget")
	}
}

// A pending seconding request whose entry no longer waits (another admin
// seconded it) is closed as failed, and its place is free again. Here
// the entry made fakeNode(0) an admin, whose own budget keeps the place,
// so b, past its own budget, gets no more places.
//
// Requirements: P3; security review F1 (stale requests)
func TestASecondingRequestWhoseEntryNoLongerWaitsIsClosed(t *testing.T) {
	a, b, c, room := threeAdmins(t)
	ids := flood(t, b, room, 0, soloLimit+2)
	c.pull(room, b)
	a.pull(room, b)
	q, ok := pendingFor(a, room, ids[0])
	if !ok {
		t.Fatalf("a has no request for b's first entry: %+v", a.requests(room))
	}
	a.confirm(q.ID)
	c.pull(room, a)
	if _, ok := pendingFor(c, room, ids[0]); ok {
		t.Fatal("c's request for an entry already seconded is still pending")
	}
	if n := len(c.requests(room)); n != soloLimit-1 {
		t.Fatalf("pending on c: %d, want %d", n, soloLimit-1)
	}
	if _, ok := pendingFor(c, room, ids[soloLimit]); ok {
		t.Fatal("b's next entry took the place the new admin's budget keeps")
	}
	all, err := c.st.Requests(context.Background(), room, store.RequestFailed)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Seconds != ids[0] || all[0].Reason != ReasonNotWaiting {
		t.Fatalf("failed on c: %+v", all)
	}
}

// Entries that arrive in one batch are raised in the order they arrived,
// not by id.
//
// Requirements: P3; security review F1 (arrival order)
func TestEntriesOfOneBatchAreRaisedInArrivalOrder(t *testing.T) {
	_, b, c, room := threeAdmins(t)
	ids := flood(t, b, room, 0, soloLimit+8)
	c.pull(room, b) // one batch
	for i, id := range ids {
		if _, ok := pendingFor(c, room, id); ok != (i < soloLimit) {
			t.Fatalf("entry %d of the batch raised: %v; pending: %d", i, ok, len(c.requests(room)))
		}
	}
}

// A raise that fails after a decision, or at start, does not stop the
// daemon, and the room's next pull, even one that brings nothing new,
// raises what was left.
//
// Requirements: P3; security review F1 (retry)
func TestAFailedRaiseIsRetriedAtTheNextPull(t *testing.T) {
	ctx := context.Background()
	_, b, c, room := threeAdmins(t)
	flood(t, b, room, 0, soloLimit+3)
	c.pull(room, b)
	fail := func(p string) error {
		if p == "ingest:requests" {
			return errors.New("injected")
		}
		return nil
	}
	testHook = fail
	t.Cleanup(func() { testHook = nil })
	if _, err := c.r.Confirm(ctx, c.requests(room)[0].ID, false); err != nil {
		t.Fatalf("the decline reports the raise's failure: %v", err)
	}
	testHook = nil
	if n := len(c.requests(room)); n != soloLimit-1 {
		t.Fatalf("pending after the failed raise: %d", n)
	}
	c.pull(room, b) // nothing new
	if n := len(c.requests(room)); n != soloLimit {
		t.Fatalf("pending after the next pull: %d", n)
	}
	// At start: the raise fails and the daemon opens all the same.
	if err := c.st.DecideRequest(ctx, c.requests(room)[0].ID, store.RequestDeclined, "", *c.clock); err != nil {
		t.Fatal(err)
	}
	testHook = fail
	c.restart()
	testHook = nil
	if _, err := c.r.State(room); err != nil {
		t.Fatalf("room after a start whose raise failed: %v", err)
	}
	c.pull(room, b)
	if n := len(c.requests(room)); n != soloLimit {
		t.Fatalf("pending after a start and a pull: %d", n)
	}
}

// The person's own budget is checked where the request is stored, so a
// request that passed prepare cannot take the room past it; and a removal
// of an admin passes it once per target, so agents' sends with files
// cannot stop the person proposing one.
//
// Requirements: P3; security review F1 (own budget)
func TestTheOwnBudgetIsCheckedOnInsertAndARemovalOfAnAdminPassesIt(t *testing.T) {
	ctx := context.Background()
	a, b, _, room := threeAdmins(t)
	for i := 0; i < MaxPendingRequests; i++ {
		q := store.Request{ID: fmt.Sprintf("q_%032x", i+1), Kind: store.RequestSendFiles, Room: room,
			Created: *a.clock, State: store.RequestPending, Draft: "{}", Terms: "t"}
		if err := a.st.CreateSendRequest(ctx, q, nil, MaxPendingRequests); err != nil {
			t.Fatal(err)
		}
	}
	late := store.Request{ID: fmt.Sprintf("q_%032x", 999), Kind: string(core.EntryAdd), Room: room, Node: fakeNode(1),
		Name: "x", Action: ActionAdmit, Created: *a.clock, State: store.RequestPending, Cap: MaxPendingRequests}
	if err := a.st.CreateRequest(ctx, late); !errors.Is(err, store.ErrTooManyPending) {
		t.Fatalf("insert past the own budget: %v", err)
	}
	if _, err := a.r.proposeAny(ctx, room, Change{Kind: core.EntryAdd, Target: fakeNode(2), Name: "x"}); err == nil {
		t.Fatal("an add was proposed past the own budget")
	}
	if _, err := a.r.proposeAny(ctx, room, Change{Kind: core.EntryRemove, Target: b.id}); err != nil {
		t.Fatalf("removal of an admin with the own budget full: %v", err)
	}
	if _, err := a.r.proposeAny(ctx, room, Change{Kind: core.EntryRemove, Target: b.id}); err == nil {
		t.Fatal("a second removal of the same admin passed the own budget")
	}
}

// quarantineBad sets room aside on n by zeroing its create entry's
// signature, and reopens n.
func quarantineBad(t *testing.T, n *lnode, room string) {
	t.Helper()
	create := n.r.rooms[room].createID
	editLog(t, n, func(db *sql.DB) {
		exec(t, db, `UPDATE log SET sig = ? WHERE room = ? AND id = ?`, make([]byte, 64), room, create)
	})
	if !n.r.isQuarantined(room) {
		t.Fatal("room not set aside")
	}
}

// Nothing can be written to a room set aside: a send, replace, withdraw
// or handled report is refused with the reason and stores no record, and
// what the room held survives restarts.
//
// Requirements: security review F71 (set aside: not changed)
func TestAWriteToARoomSetAsideIsRefusedAndStoresNothing(t *testing.T) {
	ctx := context.Background()
	clock := lclock()
	a := newLNode(t, "a", clock)
	a.register("alice")
	bad := a.create("bad")
	msg := a.send(bad, "alice", "before")
	quarantineBad(t, a, bad)
	before := len(a.records(bad))
	refused := func(what string, err error) {
		t.Helper()
		var ue *UserError
		if !errors.As(err, &ue) || ue.CodeOf() != ErrorNotAllowed || !strings.Contains(ue.Msg, QuarantineText) {
			t.Fatalf("%s into a room set aside: %v", what, err)
		}
	}
	_, err := a.m.Send(ctx, Draft{Room: bad, Agent: "alice", Text: "x"})
	refused("send", err)
	_, err = a.m.Replace(ctx, bad, "alice", msg, "y")
	refused("replace", err)
	_, err = a.m.Withdraw(ctx, bad, "alice", msg)
	refused("withdraw", err)
	_, err = a.m.Handled(ctx, bad, "alice", msg)
	refused("handled", err)
	if n := len(a.records(bad)); n != before {
		t.Fatalf("records in the room set aside: %d, was %d", n, before)
	}
	a.restart()
	a.restart()
	if n := len(a.records(bad)); n != before || !a.r.isQuarantined(bad) {
		t.Fatalf("after restarts: %d records, set aside %v", n, a.r.isQuarantined(bad))
	}
}

// A send with files to a room set aside is refused before anything is
// staged.
//
// Requirements: security review F71 (set aside: not changed)
func TestASendWithFilesToARoomSetAsideIsRefused(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	quarantineBad(t, a.lnode, room)
	a.attachFiles()
	_, err := a.f.RequestSend(context.Background(), FileSend{Draft: Draft{Room: room, Agent: "alice", Text: "x"},
		Files: []string{writeSource(t, "x.txt", "x")}})
	var ue *UserError
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, QuarantineText) {
		t.Fatalf("send with files into a room set aside: %v", err)
	}
	if n := len(a.requests(room)); n != 0 {
		t.Fatalf("requests stored: %d", n)
	}
}
