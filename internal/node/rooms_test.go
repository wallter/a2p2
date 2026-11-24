package node

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/members"
	"github.com/wallter/a2p2/internal/store"
)

// lnode is one in-process machine with the room log: its own key, store
// and Rooms, a clock shared with the other test machines, and
// deterministic randomness. Machines hand each other entries by calling
// the ingress function directly.
type lnode struct {
	t     *testing.T
	name  string
	key   ed25519.PrivateKey
	id    string
	path  string
	st    *store.Store
	m     *Machine
	r     *Rooms
	clock *time.Time
	n     uint64
}

func newLNode(t *testing.T, name string, clock *time.Time) *lnode {
	t.Helper()
	return newLNodeAs(t, name, name, clock)
}

// newLNodeAs is a machine named name holding the key of keyName: a second
// copy of one machine, to compare a run with a restart against one without.
func newLNodeAs(t *testing.T, name, keyName string, clock *time.Time) *lnode {
	t.Helper()
	seed := sha256.Sum256([]byte("rooms test " + keyName))
	key := ed25519.NewKeyFromSeed(seed[:])
	n := &lnode{t: t, name: name, key: key, id: hex.EncodeToString(key.Public().(ed25519.PublicKey)),
		path: filepath.Join(t.TempDir(), "a2p2.db"), clock: clock}
	n.open()
	t.Cleanup(func() { n.st.Close() })
	return n
}

func (n *lnode) open() {
	n.t.Helper()
	st, err := store.Open(n.path)
	if err != nil {
		n.t.Fatal(err)
	}
	n.st = st
	n.m = NewMachine(st, n.key, MachineOpts{
		Now: func() time.Time { return *n.clock },
		Rand: func(b []byte) {
			n.n++
			var c [8]byte
			binary.BigEndian.PutUint64(c[:], n.n)
			s := sha256.Sum256(append([]byte(n.name), c[:]...))
			copy(b, s[:])
		},
	})
	if n.r, err = OpenRooms(context.Background(), n.m); err != nil {
		n.t.Fatal(err)
	}
}

func (n *lnode) restart() {
	n.st.Close()
	n.open()
}

func (n *lnode) tick() { *n.clock = n.clock.Add(time.Second) }

func (n *lnode) create(label string) string {
	n.t.Helper()
	room, err := n.r.CreateRoom(context.Background(), label)
	if err != nil {
		n.t.Fatal(err)
	}
	return room
}

// pin starts holding room's log on n from from's create entry (what the
// join preview will hand over, step 4b).
func (n *lnode) pin(room string, from *lnode) {
	n.t.Helper()
	rl := from.r.rooms[room]
	if _, err := n.r.PinRoom(context.Background(), rl.ents[rl.createID].Wire()); err != nil {
		n.t.Fatal(err)
	}
}

// pull hands n everything from offers about room, in one batch.
func (n *lnode) pull(room string, from *lnode) []EntryResult {
	n.t.Helper()
	ws, err := from.r.Offer(room)
	if err != nil {
		n.t.Fatal(err)
	}
	res, err := n.r.Receive(context.Background(), room, 0, ws...)
	if err != nil {
		n.t.Fatal(err)
	}
	// As an exchange with from would: caught up if every head from has
	// accepted is accepted here (maySign).
	from.r.mu.Lock()
	heads := acceptedHeads(from.r.rooms[room])
	from.r.mu.Unlock()
	n.r.caughtUp(room, from.id, 0, heads)
	return res
}

func (n *lnode) propose(room string, c Change) store.Request {
	n.t.Helper()
	n.tick()
	q, err := n.r.proposeAny(context.Background(), room, c) // admits without the join exchange: test only
	if err != nil {
		n.t.Fatalf("%s propose %+v: %v", n.name, c, err)
	}
	return q
}

func (n *lnode) confirm(id string) Confirmation {
	n.t.Helper()
	n.tick()
	c, err := n.r.Confirm(context.Background(), id, true)
	if err != nil {
		n.t.Fatalf("%s confirm: %v", n.name, err)
	}
	if c.State != store.RequestApproved {
		n.t.Fatalf("%s confirm: %+v", n.name, c)
	}
	return c
}

// change proposes and confirms in one go.
func (n *lnode) change(room string, c Change) Confirmation {
	n.t.Helper()
	return n.confirm(n.propose(room, c).ID)
}

func (n *lnode) state(room string) members.State {
	n.t.Helper()
	st, err := n.r.State(room)
	if err != nil {
		n.t.Fatal(err)
	}
	return st
}

func (n *lnode) register(agent string) {
	n.t.Helper()
	if _, err := n.m.Register(context.Background(), agent); err != nil {
		n.t.Fatal(err)
	}
}

func (n *lnode) send(room, agent, text string) string {
	n.t.Helper()
	n.tick()
	s, err := n.m.Send(context.Background(), Draft{Room: room, Agent: agent, Text: text})
	if err != nil {
		n.t.Fatal(err)
	}
	return s.ID
}

// records returns every record n holds of room, by author, stream and
// position, as a holder would hand them on.
func (n *lnode) records(room string) []*core.VerifiedRecord {
	n.t.Helper()
	var out []*core.VerifiedRecord
	err := n.st.View(context.Background(), func(rd *store.Reader) error {
		ss, err := rd.Streams(room)
		if err != nil {
			return err
		}
		for _, s := range ss {
			hs, err := rd.RecordsFrom(room, s.Author, s.Epoch, 1)
			if err != nil {
				return err
			}
			for _, h := range hs {
				v, err := h.Record.Verified()
				if err != nil {
					return err
				}
				out = append(out, v)
			}
		}
		return nil
	})
	if err != nil {
		n.t.Fatal(err)
	}
	return out
}

func (n *lnode) take(r *core.VerifiedRecord) store.Outcome {
	n.t.Helper()
	out, err := n.r.ReceiveRecord(context.Background(), 0, r)
	if err != nil {
		n.t.Fatal(err)
	}
	return out
}

func (n *lnode) held(room, id string) (store.Held, bool) {
	n.t.Helper()
	var h store.Held
	var ok bool
	err := n.st.View(context.Background(), func(rd *store.Reader) (err error) {
		h, ok, err = rd.Record(room, id)
		return err
	})
	if err != nil {
		n.t.Fatal(err)
	}
	return h, ok
}

func (n *lnode) inbox(room, agent string) []Entry {
	n.t.Helper()
	v, err := n.m.Inbox(context.Background(), room, agent)
	if err != nil {
		n.t.Fatal(err)
	}
	return v.Entries
}

func (n *lnode) requests(room string) []store.Request {
	n.t.Helper()
	qs, err := n.st.Requests(context.Background(), room, store.RequestPending)
	if err != nil {
		n.t.Fatal(err)
	}
	return qs
}

func sameState(t *testing.T, what string, a, b members.State) {
	t.Helper()
	if !reflect.DeepEqual(a.Members, b.Members) || !reflect.DeepEqual(a.Admins, b.Admins) ||
		!reflect.DeepEqual(a.Admitted, b.Admitted) || !reflect.DeepEqual(a.Effects, b.Effects) ||
		!reflect.DeepEqual(a.Permissions, b.Permissions) {
		t.Fatalf("%s: states differ:\n%+v\n%+v", what, a, b)
	}
}

func wantNodes(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	w := slices.Clone(want)
	slices.Sort(w)
	if !slices.Equal(got, w) {
		t.Fatalf("%s: got %v, want %v", what, got, w)
	}
}

func lclock() *time.Time {
	c := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	return &c
}

// permutations calls fn with every order of xs.
func permutations(xs [][]byte, fn func([][]byte)) {
	var rec func(int)
	rec = func(k int) {
		if k == len(xs) {
			fn(slices.Clone(xs))
			return
		}
		for i := k; i < len(xs); i++ {
			xs[k], xs[i] = xs[i], xs[k]
			rec(k + 1)
			xs[k], xs[i] = xs[i], xs[k]
		}
	}
	rec(0)
}

// The first three machines of a room become its admins (DESIGN section
// 7, T9 default): the second by one admin entry confirmed on the creator;
// the third added by one admin, then made admin by matching entries from
// both, the second raised on the other admin by the first's entry. Three
// in-process machines hand each other entries through the ingress
// function; a fourth receives the room's entries one at a time in every
// order and reaches the same state.
//
// Requirements: T9, P3, A4; members rules M3, M4, M11
func TestBootstrapFirstThreeMachinesBecomeAdminsWithBothAdminsConfirmingTheThird(t *testing.T) {
	clock := lclock()
	a, b, c := newLNode(t, "a", clock), newLNode(t, "b", clock), newLNode(t, "c", clock)
	room := a.create("team")
	info, err := a.r.Info(context.Background(), room)
	if err != nil {
		t.Fatal(err)
	}
	wantNodes(t, "admins at create", info.Admins, a.id)
	if !slices.Equal(info.Limits, []string{LimitOneAdmin}) {
		t.Fatalf("limits with one admin: %v", info.Limits)
	}

	// Second machine: one admin entry, confirmed on the only admin.
	if got := a.change(room, Change{Kind: core.EntryAdmin, Target: b.id, Name: "bee", History: true}); got.Effect != EffectApplied {
		t.Fatalf("second admin: %+v", got)
	}
	b.pin(room, a)
	b.pull(room, a)
	sameState(t, "b after joining", a.state(room), b.state(room))
	wantNodes(t, "admins", b.state(room).Admins, a.id, b.id)
	if info, _ := b.r.Info(context.Background(), room); !slices.Equal(info.Limits, []string{LimitTwoAdmins}) {
		t.Fatalf("limits with two admins: %v", info.Limits)
	}

	// Third machine: added at once by one admin...
	if got := a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "sea", History: true}); got.Effect != EffectApplied {
		t.Fatalf("add c: %+v", got)
	}
	// ...then made admin: the first admin's entry waits for a second.
	first := a.change(room, Change{Kind: core.EntryAdmin, Target: c.id})
	if first.Effect != EffectPending || first.Reason != members.ReasonNeedsSecond {
		t.Fatalf("first admin entry for c: %+v", first)
	}
	if info, _ := a.r.Info(context.Background(), room); len(info.Pending) != 1 || info.Pending[0].Entry != first.Entry {
		t.Fatalf("pending on a: %+v", info.Pending)
	}
	b.pull(room, a)
	qs := b.requests(room)
	if len(qs) != 1 || qs[0].Kind != "admin" || qs[0].Node != c.id || qs[0].Seconds != first.Entry {
		t.Fatalf("request raised on b: %+v", qs)
	}
	alerts, err := b.r.Alerts(context.Background())
	if err != nil || len(alerts) != 1 || alerts[0].Kind != AlertRequest {
		t.Fatalf("alerts on b: %+v %v", alerts, err)
	}
	if got := b.confirm(qs[0].ID); got.Effect != EffectApplied {
		t.Fatalf("second admin entry for c: %+v", got)
	}
	wantNodes(t, "admins on b", b.state(room).Admins, a.id, b.id, c.id)
	a.pull(room, b)
	c.pin(room, b)
	c.pull(room, b)
	sameState(t, "a and b", a.state(room), b.state(room))
	sameState(t, "a and c", a.state(room), c.state(room))
	if len(a.requests(room)) != 0 || len(b.requests(room)) != 0 || len(c.requests(room)) != 0 {
		t.Fatal("requests left over")
	}
	if info, _ := c.r.Info(context.Background(), room); len(info.Limits) != 0 || len(info.Pending) != 0 || len(info.Members) != 3 {
		t.Fatalf("c's view: %+v", info)
	}

	// Every order of arrival, one entry at a time, gives the same state.
	ws, _ := a.r.Offer(room)
	want := a.state(room)
	orders := 0
	permutations(ws[1:], func(order [][]byte) {
		orders++
		x := newLNode(t, "x", clock)
		x.pin(room, a)
		for _, w := range order {
			if _, err := x.r.Receive(context.Background(), room, 0, w); err != nil {
				t.Fatal(err)
			}
		}
		sameState(t, "order", want, x.state(room))
	})
	if orders != 24 {
		t.Fatalf("orders: %d", orders)
	}
}

// Declining signs nothing; a request is decided once.
//
// Requirements: P3
func TestDecliningARequestSignsNothing(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	room := a.create("team")
	before, _ := a.r.Offer(room)
	q := a.propose(room, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee"})
	got, err := a.r.Confirm(context.Background(), q.ID, false)
	if err != nil || got.State != store.RequestDeclined || got.Entry != "" {
		t.Fatalf("decline: %+v %v", got, err)
	}
	after, _ := a.r.Offer(room)
	if len(after) != len(before) || a.r.signs != 0 {
		t.Fatalf("declining signed an entry")
	}
	if _, err := a.r.Confirm(context.Background(), q.ID, true); err == nil {
		t.Fatal("a declined request was confirmed")
	}
	if len(a.requests(room)) != 0 {
		t.Fatal("declined request still pending")
	}
}

// One admin removes a member [M3]; the removed machine's records stay and
// are shown marked author_removed [RM1].
//
// Requirements: T5; RM1; members rule M3
func TestRemovalOfAMemberByOneAdminKeepsItsRecordsMarked(t *testing.T) {
	clock := lclock()
	a, c := newLNode(t, "a", clock), newLNode(t, "c", clock)
	room := a.create("team")
	a.register("alice")
	a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "sea", History: true})
	c.pin(room, a)
	c.pull(room, a)
	c.register("carol")
	msg := c.send(room, "carol", "hello from c")
	for _, r := range c.records(room) {
		if out := a.take(r); out != store.OutcomeStored {
			t.Fatalf("record of a member: %s", out)
		}
	}
	if es := a.inbox(room, "alice"); len(es) != 1 || es[0].ID != msg || es[0].AuthorRemoved {
		t.Fatalf("before removal: %+v", es)
	}
	if got := a.change(room, Change{Kind: core.EntryRemove, Target: c.id}); got.Effect != EffectApplied {
		t.Fatalf("remove: %+v", got)
	}
	wantNodes(t, "members", a.state(room).Members, a.id)
	if es := a.inbox(room, "alice"); len(es) != 1 || !es[0].AuthorRemoved || es[0].Text != "hello from c" {
		t.Fatalf("after removal: %+v", es)
	}
	// A record it writes later is still stored (it was admitted), marked.
	later := c.send(room, "carol", "after removal")
	for _, r := range c.records(room) {
		if r.ID() == later {
			if out := a.take(r); out != store.OutcomeStored {
				t.Fatalf("later record: %s", out)
			}
		}
	}
	if es := a.inbox(room, "alice"); len(es) != 2 || !es[1].AuthorRemoved {
		t.Fatalf("later record: %+v", es)
	}
}

// signFork signs, with n's key, a second entry at the position of n's
// entry id, as a restored backup would: same position and parents, another
// target.
func signFork(t *testing.T, n *lnode, room, id, target string) []byte {
	t.Helper()
	f := n.r.rooms[room].ents[id].Fields()
	f.Target, f.Name = target, "fork"
	f.Created = f.Created.Add(time.Millisecond)
	e, err := core.NewEntry(n.key, f)
	if err != nil {
		t.Fatal(err)
	}
	return e.Wire()
}

// An admin's key signs two entries at one position (a restored backup).
// The other admin's machine keeps the first, refuses the second as
// evidence and alerts; one removal by the other admin settles it, and the
// branch it had accepted stands.
//
// Requirements: T9; members rules M11, M12
func TestForkInTwoAdminRoomIsSettledByTheOtherAdminsRemoval(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	d, e := newLNode(t, "d", clock), newLNode(t, "e", clock)
	room := a.create("team")
	a.change(room, Change{Kind: core.EntryAdmin, Target: b.id, Name: "bee", History: true})
	b.pin(room, a)
	b.pull(room, a)
	addD := a.change(room, Change{Kind: core.EntryAdd, Target: d.id, Name: "dee"})
	fork := signFork(t, a, room, addD.Entry, e.id)
	b.pull(room, a)
	res, err := b.r.Receive(context.Background(), room, 0, fork)
	if err != nil || res[0].Disposition != members.Refused || res[0].Reason != members.ReasonForkRefused {
		t.Fatalf("fork: %+v %v", res, err)
	}
	info, _ := b.r.Info(context.Background(), room)
	if len(info.Forks) != 1 || !info.Forks[0].Open || info.Forks[0].Author != a.id || info.Forks[0].Entries[0] != addD.Entry {
		t.Fatalf("fork evidence: %+v", info.Forks)
	}
	if alerts, _ := b.r.Alerts(context.Background()); len(alerts) != 1 || alerts[0].Kind != AlertFork {
		t.Fatalf("fork alert: %+v", alerts)
	}
	// One removal by the other admin completes it: the fork is a's vote.
	if got := b.change(room, Change{Kind: core.EntryRemove, Target: a.id}); got.Effect != EffectApplied {
		t.Fatalf("settling removal: %+v", got)
	}
	st := b.state(room)
	wantNodes(t, "members", st.Members, b.id, d.id)
	wantNodes(t, "admins", st.Admins, b.id)
	info, _ = b.r.Info(context.Background(), room)
	if len(info.Forks) != 1 || info.Forks[0].Open {
		t.Fatalf("fork after settling: %+v", info.Forks)
	}
	if alerts, _ := b.r.Alerts(context.Background()); len(alerts) != 0 {
		t.Fatalf("alerts after settling: %+v", alerts)
	}
	// Machines that hear of it later reach the same state, in either order.
	for _, src := range []*lnode{b, a} {
		x := newLNode(t, "x", clock)
		x.pin(room, a)
		x.pull(room, src)
		x.pull(room, b)
		if _, err := x.r.Receive(context.Background(), room, 0, fork); err != nil {
			t.Fatal(err)
		}
		sameState(t, "late machine", st, x.state(room))
	}
}

// The membership machine is rebuilt on start by replaying the stored log
// in arrival batches: held entries, fork evidence and requests come back
// as they were, and what follows a restart is decided as it would have
// been without one.
//
// Requirements: DESIGN section 5; members rule M11
func TestRestartReplaysTheLogToTheSameStateAndDecidesLaterEntriesTheSame(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	ctl := newLNodeAs(t, "ctl", "b", clock) // b's key, never restarted
	d, e, f := newLNode(t, "d", clock), newLNode(t, "e", clock), newLNode(t, "f", clock)
	room := a.create("team")
	a.change(room, Change{Kind: core.EntryAdmin, Target: b.id, Name: "bee", History: true})
	for _, n := range []*lnode{b, ctl} {
		n.pin(room, a)
		n.pull(room, a)
	}
	addD := a.change(room, Change{Kind: core.EntryAdd, Target: d.id, Name: "dee"})
	addF := a.change(room, Change{Kind: core.EntryAdd, Target: f.id, Name: "eff"})
	wireOf := func(id string) []byte { return a.r.rooms[room].ents[id].Wire() }
	fork := signFork(t, a, room, addD.Entry, e.id)
	// The same arrivals on b and on the control: addF before addD (held),
	// then addD, then the fork, then a change b's person must second.
	steps := [][][]byte{{wireOf(addF.Entry)}, {wireOf(addD.Entry), fork}}
	for _, n := range []*lnode{b, ctl} {
		for _, s := range steps[:1] {
			res, err := n.r.Receive(context.Background(), room, 0, s...)
			if err != nil || res[0].Disposition != members.Held {
				t.Fatalf("held: %+v %v", res, err)
			}
		}
	}
	b.restart()
	sameState(t, "after restart with a held entry", ctl.state(room), b.state(room))
	for _, n := range []*lnode{b, ctl} {
		if _, err := n.r.Receive(context.Background(), room, 0, steps[1]...); err != nil {
			t.Fatal(err)
		}
	}
	promote := a.change(room, Change{Kind: core.EntryAdmin, Target: d.id})
	for _, n := range []*lnode{b, ctl} {
		n.r.Receive(context.Background(), room, 0, wireOf(promote.Entry))
	}
	beforeInfo, _ := b.r.Info(context.Background(), room)
	beforeOffer, _ := b.r.Offer(room)
	beforeEvidence := b.r.rooms[room].mach.Evidence()
	b.restart()
	sameState(t, "after restart", ctl.state(room), b.state(room))
	afterInfo, _ := b.r.Info(context.Background(), room)
	afterOffer, _ := b.r.Offer(room)
	if !reflect.DeepEqual(beforeInfo, afterInfo) || !reflect.DeepEqual(beforeOffer, afterOffer) ||
		!reflect.DeepEqual(beforeEvidence, b.r.rooms[room].mach.Evidence()) {
		t.Fatalf("restart changed what is shown or offered:\n%+v\n%+v", beforeInfo, afterInfo)
	}
	if len(afterInfo.Forks) != 1 || !afterInfo.Forks[0].Open || len(afterInfo.Requests) != 1 {
		t.Fatalf("after restart: %+v", afterInfo)
	}
	// Settling after the restart: same as the control machine. The
	// restarted b has to have caught up with the room's log before it
	// signs (maySign); this stands in for that pull.
	b.r.rooms[room].pulled = true
	for _, n := range []*lnode{b, ctl} {
		n.change(room, Change{Kind: core.EntryRemove, Target: a.id})
	}
	got, want := b.state(room), ctl.state(room)
	if !reflect.DeepEqual(got.Members, want.Members) || !reflect.DeepEqual(got.Admins, want.Admins) {
		t.Fatalf("settled differently after restart: %+v %+v", got, want)
	}
	wantNodes(t, "members", got.Members, b.id, d.id, f.id)
	b.restart()
	sameState(t, "after settling and restart", got, b.state(room))
}

// A request is revalidated when the person confirms it: if the state
// changed underneath it, it fails with the reason and nothing is signed.
//
// Requirements: P3, A4
func TestRequestConfirmedAfterTheStateChangedUnderneathItIsRefusedWithAReason(t *testing.T) {
	clock := lclock()
	a, b, c := newLNode(t, "a", clock), newLNode(t, "b", clock), newLNode(t, "c", clock)
	room := a.create("team")
	a.change(room, Change{Kind: core.EntryAdmin, Target: b.id, Name: "bee", History: true})
	b.pin(room, a)
	b.pull(room, a)
	q := a.propose(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "sea"})
	b.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "sea"})
	a.pull(room, b)
	before, _ := a.r.Offer(room)
	got, err := a.r.Confirm(context.Background(), q.ID, true)
	if err != nil || got.State != store.RequestFailed || got.Reason != members.ReasonAlreadyMember || got.Entry != "" {
		t.Fatalf("stale add: %+v %v", got, err)
	}
	if after, _ := a.r.Offer(room); len(after) != len(before) {
		t.Fatal("a failed request signed an entry")
	}
	stored, err := a.st.GetRequest(context.Background(), q.ID)
	if err != nil || stored.State != store.RequestFailed || stored.Reason != members.ReasonAlreadyMember {
		t.Fatalf("stored request: %+v %v", stored, err)
	}

	// No longer an admin: a's request to remove c fails once b has removed
	// a, whose key signed a fork.
	d := newLNode(t, "d", clock)
	q2 := a.propose(room, Change{Kind: core.EntryRemove, Target: c.id})
	addD := a.change(room, Change{Kind: core.EntryAdd, Target: d.id, Name: "dee"})
	b.pull(room, a)
	if _, err := b.r.Receive(context.Background(), room, 0, signFork(t, a, room, addD.Entry, c.id)); err != nil {
		t.Fatal(err)
	}
	b.change(room, Change{Kind: core.EntryRemove, Target: a.id})
	a.pull(room, b)
	if slices.Contains(a.state(room).Admins, a.id) {
		t.Fatalf("a still an admin: %+v", a.state(room))
	}
	got, err = a.r.Confirm(context.Background(), q2.ID, true)
	if err != nil || got.State != store.RequestFailed || got.Reason != members.ReasonNotAdmin {
		t.Fatalf("not an admin: %+v %v", got, err)
	}
}

// Leaving is local: the room's data is deleted, its generation moves on,
// and a response prepared under the old generation is not applied, even
// after joining again.
//
// Requirements: DESIGN section 7 (leaving), section 4 (requester)
func TestLeaveThenALateResponseIsNotApplied(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	room := a.create("team")
	a.register("alice")
	a.change(room, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee", History: true})
	b.pin(room, a)
	b.pull(room, a)
	a.send(room, "alice", "hello")
	recs := a.records(room)
	for _, r := range recs {
		b.take(r)
	}
	gen, err := b.r.Gen(room)
	if err != nil {
		t.Fatal(err)
	}
	ws, _ := a.r.Offer(room) // the late response
	if err := b.r.Leave(context.Background(), room); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.held(room, recs[0].ID()); ok {
		t.Fatal("records kept after leaving")
	}
	if _, err := b.r.Receive(context.Background(), room, gen, ws...); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("late response after leaving: %v", err)
	}
	if _, err := b.r.ReceiveRecord(context.Background(), gen, recs[0]); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("late record after leaving: %v", err)
	}
	// Joining again: a new generation; the old response still does not apply.
	b.pin(room, a)
	gen2, _ := b.r.Gen(room)
	if gen2 <= gen {
		t.Fatalf("generation did not move: %d then %d", gen, gen2)
	}
	if err := b.r.CheckGen(room, gen); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("CheckGen: %v", err)
	}
	if _, err := b.r.Receive(context.Background(), room, gen, ws...); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("late response after rejoining: %v", err)
	}
	if _, err := b.r.ReceiveRecord(context.Background(), gen, recs[0]); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("late record after rejoining: %v", err)
	}
	if st := b.state(room); len(st.Effects) != 1 {
		t.Fatalf("late response applied: %+v", st)
	}
	// The store refuses it too, inside its own transaction.
	if _, err := b.st.ApplyLog(context.Background(), store.LogBatch{Room: room, Gen: gen}); !errors.Is(err, store.ErrStaleGeneration) {
		t.Fatalf("store: %v", err)
	}
}

// Entries are checked at ingress before the membership machine: the room
// (R1), the pinned create (C1), the signature (G1) and the strict decoding.
//
// Requirements: PROTOCOL section 3 checks R1, C1, G1, S1
func TestIngressRefusesWrongRoomSecondCreateAndBadSignature(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	room := a.create("team")
	other := b.create("other")
	otherWire := b.r.rooms[other].ents[b.r.rooms[other].createID].Wire()
	second, err := core.NewEntry(b.key, core.EntryFields{Room: room, Seq: 1, Kind: core.EntryCreate, Label: "x", Created: *clock})
	if err != nil {
		t.Fatal(err)
	}
	// An add for this room by b, carrying the signature of another entry.
	add, err := core.NewEntry(b.key, core.EntryFields{Room: room, Seq: 1, Kind: core.EntryAdd, Target: b.id, Name: "bee",
		Parents: []string{a.r.rooms[room].createID}, Created: *clock})
	if err != nil {
		t.Fatal(err)
	}
	bad := []byte(`{"h":"` + base64.StdEncoding.EncodeToString(add.Header()) + `","s":"` +
		base64.StdEncoding.EncodeToString(second.Sig()) + `"}`)
	res, err := a.r.Receive(context.Background(), room, 0, otherWire, second.Wire(), []byte("{"), bad)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{CodeWrongRoom, CodeSecondCreate, "", CodeBadSignature}
	for i, r := range res {
		if r.Disposition != members.Invalid || want[i] != "" && r.Reason != want[i] {
			t.Fatalf("wire %d: %+v", i, r)
		}
	}
	if st := a.state(room); len(st.Effects) != 1 {
		t.Fatalf("something applied: %+v", st)
	}
}

// Records follow membership [RM1, RM2]: a record whose author was never
// admitted is not stored; an author that leaves Admitted (it can shrink,
// [M13]) has its records hidden, not deleted, and shown again when it is
// admitted again; a machine admitted without history holds earlier text
// only as headers and its agents start at the admission frontier.
//
// Requirements: T3, T5; RM1, RM2; members rules M13, M14
func TestRecordsFollowMembership(t *testing.T) {
	clock := lclock()
	a, b, c, d := newLNode(t, "a", clock), newLNode(t, "b", clock), newLNode(t, "c", clock), newLNode(t, "d", clock)
	room := a.create("team")
	a.register("alice")

	// Never admitted: not stored.
	d.pin(room, a)
	d.register("dave")
	stray := d.send(room, "dave", "not a member")
	for _, r := range d.records(room) {
		if out := a.take(r); out != store.OutcomeAuthorNotAdmitted {
			t.Fatalf("never-admitted author: %s", out)
		}
	}
	if _, ok := a.held(room, stray); ok {
		t.Fatal("stored a record of a machine never admitted")
	}

	// Admitted shrinks: a adds then removes c while b, having seen
	// neither, adds and removes c (members TestAdmittedCanShrinkWhenMoreEntriesArrive).
	a.change(room, Change{Kind: core.EntryAdmin, Target: b.id, Name: "bee", History: true})
	b.pin(room, a)
	b.pull(room, a)
	a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "sea", History: true})
	c.pin(room, a)
	c.pull(room, a)
	c.register("carol")
	msg := c.send(room, "carol", "from c")
	for _, r := range c.records(room) {
		if out := a.take(r); out != store.OutcomeStored {
			t.Fatalf("record of c: %s", out)
		}
	}
	a.change(room, Change{Kind: core.EntryRemove, Target: c.id})
	b.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "sea"})
	b.change(room, Change{Kind: core.EntryRemove, Target: c.id})
	a.pull(room, b)
	wantNodes(t, "admitted", a.state(room).Admitted, a.id, b.id)
	if es := a.inbox(room, "alice"); len(es) != 0 {
		t.Fatalf("record of an author no longer admitted shown: %+v", es)
	}
	if th, _ := a.m.Thread(context.Background(), room, msg); len(th) != 0 {
		t.Fatalf("thread shows it: %+v", th)
	}
	if h, ok := a.held(room, msg); !ok || h.Reason != store.ReasonLive {
		t.Fatalf("record not kept: %+v %v", h, ok)
	}
	a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "sea", History: true})
	if es := a.inbox(room, "alice"); len(es) != 1 || es[0].ID != msg || es[0].AuthorRemoved {
		t.Fatalf("readmitted author's record: %+v", es)
	}

	// Without history: e is admitted with a's heads as its frontier.
	e := newLNode(t, "e", clock)
	m1 := a.send(room, "alice", "before e one")
	m2 := a.send(room, "alice", "before e two")
	a.change(room, Change{Kind: core.EntryAdd, Target: e.id, Name: "eee"})
	if p := a.state(room).Permissions[e.id]; p.All || len(p.From[a.id]) != 1 {
		t.Fatalf("e's permission: %+v", p)
	}
	e.pin(room, a)
	e.pull(room, a)
	e.register("erin")
	m3 := a.send(room, "alice", "after e")
	for _, r := range a.records(room) {
		if r.Author() == a.id {
			if out := e.take(r); out != store.OutcomeStored {
				t.Fatalf("record for e: %s", out)
			}
		}
	}
	for _, id := range []string{m1, m2} {
		if h, ok := e.held(room, id); !ok || h.Reason != store.ReasonNotAdmitted || h.Record.Body() != nil {
			t.Fatalf("earlier text held without history: %+v %v", h, ok)
		}
	}
	if h, _ := e.held(room, m3); h.Reason != store.ReasonLive {
		t.Fatalf("later text: %+v", h)
	}
	es := e.inbox(room, "erin")
	if len(es) != 1 || es[0].ID != m3 {
		t.Fatalf("erin's inbox: %+v", es)
	}
	var cs []store.Cursor
	e.st.View(context.Background(), func(rd *store.Reader) (err error) {
		cs, err = rd.Cursors("erin", room)
		return err
	})
	if len(cs) != 1 || cs[0].Author != a.id || cs[0].Pos != 2 || cs[0].Start != 2 {
		t.Fatalf("erin's cursors: %+v", cs)
	}

	// With history: earlier text is held.
	g := newLNode(t, "g", clock)
	a.change(room, Change{Kind: core.EntryAdd, Target: g.id, Name: "gee", History: true})
	g.pin(room, a)
	g.pull(room, a)
	for _, r := range a.records(room) {
		g.take(r)
	}
	if h, _ := g.held(room, m1); h.Reason != store.ReasonLive {
		t.Fatalf("history grant: %+v", h)
	}
}
