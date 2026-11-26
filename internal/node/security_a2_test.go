package node

// Group A, second pass of the 2026-11-25 security review: fair seconding
// budgets, a log that replays whatever a peer delivers, and what a room
// set aside still does.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/members"
	"github.com/wallter/a2p2/internal/store"
)

// signAt signs, with key, an add entry in room at seq after prev with
// parents, for a made-up target.
func signAt(t *testing.T, key ed25519.PrivateKey, room string, seq int64, prev string, parents []string, target int, at time.Time) *core.Entry {
	t.Helper()
	e, err := core.NewEntry(key, core.EntryFields{Room: room, Seq: seq, Prev: prev, Parents: parents,
		Kind: core.EntryAdd, Target: fakeNode(target), Name: "x", Created: at})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func keyOf(name string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("security a2 " + name))
	return ed25519.NewKeyFromSeed(seed[:])
}

// A refused entry whose fork is settled becomes held when it also waits
// for a missing parent, and the held bound then drops it, while an entry
// that depends on it stays refused. Delivered as a peer delivers entries,
// then a restart: the room must still replay.
//
// Requirements: DESIGN section 5 (replay); security review F71 (replay from ingress)
func TestASettledForkWhoseDependantIsDroppedStillReplays(t *testing.T) {
	ctx := context.Background()
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	room := a.create("team")
	a.change(room, Change{Kind: core.EntryAdmin, Target: b.id, Name: "bee", History: true})
	b.pin(room, a)
	b.pull(room, a)
	addD := a.change(room, Change{Kind: core.EntryAdd, Target: fakeNode(900), Name: "dee"})
	b.pull(room, a)
	forkWire := signFork(t, a, room, addD.Entry, fakeNode(901))
	fork, err := core.ParseEntry(forkWire)
	if err != nil {
		t.Fatal(err)
	}
	ff := fork.Fields()
	// missing is never delivered.
	missing := signAt(t, keyOf("absent"), room, 1, "", []string{b.r.rooms[room].createID}, 902, *clock)
	p := signAt(t, a.key, room, ff.Seq+1, fork.ID(), []string{fork.ID(), missing.ID()}, 903, *clock)
	d := signAt(t, a.key, room, ff.Seq+2, p.ID(), []string{p.ID()}, 904, *clock)
	res, err := b.r.Receive(ctx, room, 0, forkWire, p.Wire(), d.Wire())
	if err != nil {
		t.Fatal(err)
	}
	for i, x := range res {
		if x.Disposition != members.Refused {
			t.Fatalf("entry %d: %+v", i, x)
		}
	}
	// b removes a: the fork settles, the fork's branch is accepted, p
	// waits for missing, d stays refused.
	if got := b.change(room, Change{Kind: core.EntryRemove, Target: a.id}); got.Effect != EffectApplied {
		t.Fatalf("settling removal: %+v", got)
	}
	// A stranger's held entries push p out of the held bound.
	var wires [][]byte
	for i := 0; i < 1100; i++ {
		wires = append(wires, signAt(t, keyOf(fmt.Sprint("stranger ", i)), room, 1, "", []string{missing.ID()}, 1000+i, *clock).Wire())
	}
	for i := 0; i < len(wires); i += 100 {
		if _, err := b.r.Receive(ctx, room, 0, wires[i:min(i+100, len(wires))]...); err != nil {
			t.Fatal(err)
		}
	}
	stored := func() map[string]string {
		rows, err := b.st.Log(ctx, room)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, row := range rows {
			out[row.ID] = row.State
		}
		return out
	}
	before := stored()
	if before[fork.ID()] != string(members.Accepted) {
		t.Fatalf("the fork's branch after settling: %q", before[fork.ID()])
	}
	// p left the log, and d with it: d's refusal rested on p.
	if x, ok := before[d.ID()]; ok && x == string(members.Refused) {
		t.Fatalf("d kept refused although p was dropped: p %q", before[p.ID()])
	}
	st := b.state(room)
	b.restart()
	if q := b.r.Quarantined(); len(q) != 0 {
		t.Fatalf("set aside after a restart: %+v", q)
	}
	sameState(t, "after a restart", st, b.state(room))
	if after := stored(); len(after) != len(before) {
		t.Fatalf("rows after a restart: %d, were %d", len(after), len(before))
	}
	// Delivered again, p and d are decided as on first receipt.
	if _, err := b.r.Receive(ctx, room, 0, p.Wire(), d.Wire()); err != nil {
		t.Fatal(err)
	}
	b.restart()
	if q := b.r.Quarantined(); len(q) != 0 {
		t.Fatalf("set aside after p and d came again: %+v", q)
	}
}

// The honest case: a fork settled while nothing is dropped keeps its
// dependants as decided, and a held entry dropped over the bound with
// nothing refused on it leaves the rest as they were.
//
// Requirements: DESIGN section 5 (replay); security review F71 (replay from ingress)
func TestAHeldFloodWithoutAForkReplaysAsDecided(t *testing.T) {
	ctx := context.Background()
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	room := a.create("team")
	a.change(room, Change{Kind: core.EntryAdmin, Target: b.id, Name: "bee", History: true})
	b.pin(room, a)
	b.pull(room, a)
	missing := signAt(t, keyOf("absent"), room, 1, "", []string{b.r.rooms[room].createID}, 902, *clock)
	var wires [][]byte
	for i := 0; i < 1100; i++ {
		wires = append(wires, signAt(t, keyOf(fmt.Sprint("stranger ", i)), room, 1, "", []string{missing.ID()}, 1000+i, *clock).Wire())
	}
	for i := 0; i < len(wires); i += 100 {
		if _, err := b.r.Receive(ctx, room, 0, wires[i:min(i+100, len(wires))]...); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := b.st.Log(ctx, room)
	if err != nil {
		t.Fatal(err)
	}
	held := 0
	for _, row := range rows {
		if row.State == string(members.Held) {
			held++
		}
	}
	if held != 1024 {
		t.Fatalf("held: %d", held)
	}
	got := b.change(room, Change{Kind: core.EntryAdd, Target: fakeNode(1), Name: "x"})
	if got.Effect != EffectApplied {
		t.Fatalf("add: %+v", got)
	}
	st := b.state(room)
	b.restart()
	if q := b.r.Quarantined(); len(q) != 0 {
		t.Fatalf("set aside: %+v", q)
	}
	sameState(t, "after a restart", st, b.state(room))
}

// Random entries delivered as a peer delivers them: a forking admin's
// entries on both branches with parents that are missing, refused or
// accepted, strangers' entries waiting past the held bound, and the
// removal that settles the fork at a random point. After every batch a
// restart replays the room to the same state and rows.
//
// Requirements: DESIGN section 5 (replay); security review F71 (replay from ingress)
func TestRandomIngressAlwaysReplays(t *testing.T) {
	// Without settleDrops, seeds 1 and 8 set the room aside, and 6 seeds
	// of the first 40 did.
	for _, seed := range []uint64{1, 8} {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { randomIngress(t, seed) })
	}
}

func randomIngress(t *testing.T, seed uint64) {
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(seed, 7))
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	room := a.create("team")
	a.change(room, Change{Kind: core.EntryAdmin, Target: b.id, Name: "bee", History: true})
	b.pin(room, a)
	b.pull(room, a)
	addD := a.change(room, Change{Kind: core.EntryAdd, Target: fakeNode(900), Name: "dee"})
	b.pull(room, a)
	forkWire := signFork(t, a, room, addD.Entry, fakeNode(901))
	fork, err := core.ParseEntry(forkWire)
	if err != nil {
		t.Fatal(err)
	}
	k := fork.Fields().Seq
	create := b.r.rooms[room].createID
	// a's entries by position, both branches.
	at := map[int64][]string{k: {addD.Entry, fork.ID()}}
	var absent []string
	for i := 0; i < 4; i++ {
		absent = append(absent, signAt(t, keyOf(fmt.Sprint("absent ", seed, i)), room, 1, "", []string{create}, 950+i, *clock).ID())
	}
	known := []string{create, addD.Entry, fork.ID()}
	pending := [][]byte{forkWire}
	target := 2000
	for seq := k + 1; seq < k+12; seq++ {
		for n := 0; n < 1+rng.IntN(2); n++ {
			prev := at[seq-1][rng.IntN(len(at[seq-1]))]
			parents := []string{prev}
			if rng.IntN(2) == 0 {
				parents = append(parents, absent[rng.IntN(len(absent))])
			}
			if rng.IntN(2) == 0 {
				parents = append(parents, known[rng.IntN(len(known))])
			}
			target++
			e := signAt(t, a.key, room, seq, prev, slices.Compact(slices.Sorted(slices.Values(parents))), target, *clock)
			at[seq] = append(at[seq], e.ID())
			known = append(known, e.ID())
			pending = append(pending, e.Wire())
		}
	}
	// a's entries first, in a random order, then the strangers' mixed with
	// a few of a's, so that what a's entries left held is the oldest held.
	rng.Shuffle(len(pending), func(i, j int) { pending[i], pending[j] = pending[j], pending[i] })
	ofA := len(pending) - rng.IntN(4)
	for i := 0; i < 1060; i++ {
		target++
		pending = append(pending, signAt(t, keyOf(fmt.Sprint("stranger ", seed, i)), room, 1, "",
			[]string{absent[rng.IntN(len(absent))]}, target, *clock).Wire())
	}
	rest := pending[ofA:]
	rng.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
	settleAt := rng.IntN(ofA + 20)
	settled := false
	check := func(when string) {
		t.Helper()
		st := b.state(room)
		rows, err := b.st.Log(ctx, room)
		if err != nil {
			t.Fatal(err)
		}
		b.restart()
		if q := b.r.Quarantined(); len(q) != 0 {
			t.Fatalf("%s: set aside: %+v", when, q)
		}
		sameState(t, when, st, b.state(room))
		after, err := b.st.Log(ctx, room)
		if err != nil || len(after) != len(rows) {
			t.Fatalf("%s: %d rows, were %d (%v)", when, len(after), len(rows), err)
		}
	}
	for i := 0; i < len(pending); {
		n := 1 + rng.IntN(150)
		if !settled && i+n > settleAt {
			q := b.propose(room, Change{Kind: core.EntryRemove, Target: a.id})
			// Pending until the fork arrives, if it has not yet.
			if c, err := b.r.ConfirmWith(ctx, q.ID, true, ConfirmOpts{NotCaughtUpOK: true}); err != nil || c.State != store.RequestApproved {
				t.Fatalf("settling removal: %+v %v", c, err)
			}
			settled = true
			check("after the settling removal")
		}
		if _, err := b.r.Receive(ctx, room, 0, pending[i:min(i+n, len(pending))]...); err != nil {
			t.Fatal(err)
		}
		i += n
		check(fmt.Sprint("after ", i, " entries"))
	}
	// Everything again, in another order.
	rng.Shuffle(len(pending), func(i, j int) { pending[i], pending[j] = pending[j], pending[i] })
	for i := 0; i < len(pending); i += 200 {
		if _, err := b.r.Receive(ctx, room, 0, pending[i:min(i+200, len(pending))]...); err != nil {
			t.Fatal(err)
		}
	}
	check("after everything again")
}

// A room set aside has no known reading frontier: text whose readability
// was undecided stays undecided. Past retention a read shows no notice
// for it and a prune does not mark it expired, while this machine's own
// text past retention expires as usual. The honest case, the same room
// not set aside, holds it back the same way.
//
// Requirements: R6; security review F71 (set aside: reads and expiry)
func TestARoomSetAsideKeepsUndecidedTextUndecided(t *testing.T) {
	for _, aside := range []bool{false, true} {
		t.Run(fmt.Sprint("set aside ", aside), func(t *testing.T) {
			ctx := context.Background()
			a, b, room := twoAdmins(t)
			a.register("alice")
			a.send(room, "alice", "u1")
			for _, r := range a.records(room) {
				b.take(r)
			}
			a2 := newLNodeAs(t, "a2", "a", a.clock)
			a2.pin(room, a)
			a2.register("alice")
			x1 := a2.send(room, "alice", "x1")
			e := newLNode(t, "e", a.clock)
			b.change(room, Change{Kind: core.EntryAdd, Target: e.id, Name: "eee"})
			e.pin(room, b)
			e.pull(room, b)
			e.register("erin")
			if out := e.take(recordOf(t, a2, room, x1)); out != store.OutcomeStored {
				t.Fatalf("x1: %s", out)
			}
			mine := e.send(room, "erin", "mine")
			if aside {
				quarantineBad(t, e, room)
			}
			*e.clock = e.clock.Add(store.Retention + 24*time.Hour)
			for _, en := range e.inbox(room, "erin") {
				if en.ID == x1 {
					t.Fatalf("undecided text shown: %+v", en)
				}
			}
			if err := e.m.Prune(ctx); err != nil {
				t.Fatal(err)
			}
			if h, _ := e.held(room, x1); h.Reason != store.ReasonMissing {
				t.Fatalf("undecided text after a prune: %+v", h)
			}
			if h, _ := e.held(room, mine); h.Reason != store.ReasonExpired {
				t.Fatalf("own text past retention after a prune: %q", h.Reason)
			}
		})
	}
}

// Seen and a change of the files setting are refused for a room set
// aside and write nothing; reading the setting, and both in another
// room, work.
//
// Requirements: security review F71 (set aside: not changed)
func TestSeenAndTheFilesSettingOfARoomSetAsideAreRefused(t *testing.T) {
	ctx := context.Background()
	clock := lclock()
	a := newLNode(t, "a", clock)
	a.register("alice")
	bad := a.create("bad")
	good := a.create("good")
	quarantineBad(t, a, bad)
	s, err := OpenSurface(ctx, a.m, a.r, SurfaceOpts{Name: "solo", Book: NewAddrBook()})
	if err != nil {
		t.Fatal(err)
	}
	refused := func(what string, err error) {
		t.Helper()
		var ue *UserError
		if !errors.As(err, &ue) || ue.CodeOf() != ErrorNotAllowed || !strings.Contains(ue.Msg, QuarantineText) {
			t.Fatalf("%s for a room set aside: %v", what, err)
		}
	}
	_, err = a.m.Seen(ctx, bad, "alice", nil)
	refused("seen", err)
	if _, err := a.m.Seen(ctx, good, "alice", nil); err != nil {
		t.Fatalf("seen in another room: %v", err)
	}
	before, err := s.RoomFiles(ctx, bad, "")
	if err != nil {
		t.Fatalf("reading the files setting: %v", err)
	}
	next := store.FilesKeep
	if before.Mode == store.FilesKeep {
		next = store.FilesOff
	}
	_, err = s.RoomFiles(ctx, bad, next)
	refused("files setting", err)
	if after, err := s.RoomFiles(ctx, bad, ""); err != nil || after.Mode != before.Mode {
		t.Fatalf("files setting after a refusal: %+v %v (was %q)", after, err, before.Mode)
	}
	if v, err := s.RoomFiles(ctx, good, next); err != nil || v.Mode != next {
		t.Fatalf("files setting of another room: %+v %v", v, err)
	}
}

// The file sweep of a room set aside drops what retention requires and
// nothing the room's membership would decide: a file whose author the
// stored member cache shows as voided stays until its text passes
// retention. The honest case: in a room not set aside the start rebuilds
// the cache from the log, and the file stays until retention too.
//
// Requirements: L1; security review F71 (set aside: the file sweep)
func TestTheFileSweepOfARoomSetAsideDropsOnlyWhatRetentionRequires(t *testing.T) {
	for _, aside := range []bool{false, true} {
		t.Run(fmt.Sprint("set aside ", aside), func(t *testing.T) {
			ctx := context.Background()
			ns := filesNet(t, "a", "b")
			a, b := ns[0], ns[1]
			room := fileRoom(t, a, b)
			msg, res := a.sendFiles(room, "alice", "see the file", writeSource(t, "x.txt", "the bytes"))
			sha := res.Files[0].SHA256
			b.pullAll(room, a)
			b.tickFiles()
			b.inbox(room, "agent-b")
			if r, err := b.fetch(room, "agent-b", sha); err != nil || heldFor(t, b, room, sha, msg) == "" {
				t.Fatalf("fetch: %+v %v", r, err)
			}
			void := func(db *sql.DB) {
				exec(t, db, `UPDATE room_members SET admitted = 0 WHERE room = ? AND node = ?`, room, a.id)
			}
			if aside {
				create := b.r.rooms[room].createID
				editLog(t, b.lnode, func(db *sql.DB) {
					exec(t, db, `UPDATE log SET sig = ? WHERE room = ? AND id = ?`, make([]byte, 64), room, create)
					void(db)
				})
				if !b.r.isQuarantined(room) {
					t.Fatal("room not set aside")
				}
			} else {
				editLog(t, b.lnode, void)
			}
			b.attachFiles() // a start: the sweep of every room
			if how := heldFor(t, b, room, sha, msg); how == "" {
				t.Fatal("dropped by the start's sweep")
			}
			*b.clock = b.clock.Add(store.Retention + time.Hour)
			if err := b.f.SweepAll(ctx); err != nil {
				t.Fatal(err)
			}
			if how := heldFor(t, b, room, sha, msg); how != "" {
				t.Fatalf("held past retention: %q", how)
			}
		})
	}
}

// Seen across rooms, as the seen tool sends what the all-rooms inbox
// showed, confirms the keys of the other rooms when one key belongs to a
// room set aside; that key is left unconfirmed (not_shown).
//
// Requirements: security review F71 (set aside: not changed)
func TestSeenAcrossRoomsConfirmsTheOthersWhenOneRoomIsSetAside(t *testing.T) {
	ctx := context.Background()
	clock := lclock()
	a := newLNode(t, "a", clock)
	a.register("alice")
	a.register("bob")
	bad := a.create("bad")
	good := a.create("good")
	a.send(bad, "bob", "b1")
	var first []store.Read
	for _, e := range a.inbox(bad, "alice") {
		first = append(first, store.Read{Key: e.Key, Version: e.Version})
	}
	if res, err := a.m.Seen(ctx, bad, "alice", first); err != nil || len(res) != 1 || !res[0].Confirmed {
		t.Fatalf("seen b1: %+v %v", res, err)
	}
	a.send(bad, "bob", "b2")
	a.send(good, "bob", "g1")
	quarantineBad(t, a, bad)
	s, err := OpenSurface(ctx, a.m, a.r, SurfaceOpts{Name: "solo", Book: NewAddrBook()})
	if err != nil {
		t.Fatal(err)
	}
	agent := "alice@" + short(a.m.ID)
	v, err := s.Inbox(ctx, agent, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var rs []store.Read
	for _, room := range []string{bad, good} { // the room set aside first
		for _, e := range v.Entries {
			if e.Room == room {
				rs = append(rs, store.Read{Key: e.Key, Version: e.Version})
			}
		}
	}
	if len(rs) != 2 || v.Entries[0].Room == v.Entries[1].Room {
		t.Fatalf("all-rooms inbox: %+v", v.Entries)
	}
	res, err := s.Seen(ctx, agent, rs)
	if err != nil {
		t.Fatalf("seen across rooms: %v", err)
	}
	if res[0].Confirmed || res[0].Reason != SeenNotShown || !res[1].Confirmed {
		t.Fatalf("seen results: %+v", res)
	}
	v, err = s.Inbox(ctx, agent, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Entries) != 1 || v.Entries[0].Room != bad {
		t.Fatalf("inbox after seen: %+v", v.Entries)
	}
}
