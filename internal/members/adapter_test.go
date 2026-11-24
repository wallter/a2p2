package members

import (
	"crypto/ed25519"
	"encoding/hex"
	"reflect"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

const (
	testRoom   = "r_0123456789abcdef0123456789abcdef"
	testRoom2  = "r_fedcba9876543210fedcba9876543210"
	testRecord = "00112233445566778899aabbccddeeff" // a record id, A's head
)

// signer signs real core entries with a test key.
type signer struct {
	t    *testing.T
	key  ed25519.PrivateKey
	id   string
	seq  int64
	last string
}

func newSigner(t *testing.T, n byte) *signer {
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = n
	key := ed25519.NewKeyFromSeed(seed)
	return &signer{t: t, key: key, id: hex.EncodeToString(key.Public().(ed25519.PublicKey))}
}

// sign signs the signer's next entry; at reuses a position (a restored
// backup) when at > 0, continuing from prev.
func (s *signer) sign(kind core.EntryKind, target string, f core.EntryFields, parents ...*core.Entry) *core.Entry {
	if f.Room == "" {
		f.Room = testRoom
	}
	f.Kind, f.Target = kind, target
	f.Created = time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	if f.Seq == 0 {
		f.Seq, f.Prev = s.seq+1, s.last
	}
	seen := map[string]bool{}
	for _, p := range parents {
		seen[p.ID()] = true
	}
	if f.Prev != "" {
		seen[f.Prev] = true
	}
	f.Parents = sortedKeys(seen)
	switch kind {
	case core.EntryCreate:
		f.Label, f.Parents, f.Target = "room", nil, ""
	case core.EntryAdd:
		f.Name = "m"
	}
	e, err := core.NewEntry(s.key, f)
	if err != nil {
		s.t.Fatal(err)
	}
	if f.Seq > s.seq {
		s.seq, s.last = f.Seq, e.ID()
	}
	return e
}

func ids(es ...*core.Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.ID())
	}
	return out
}

func sortedNodes(ss ...*signer) []string {
	m := map[string]bool{}
	for _, s := range ss {
		m[s.id] = true
	}
	return sortedKeys(m)
}

// bootstrap: A creates the room; adds B with history and makes it an
// admin alone (A is the only admin); adds C with a frontier, then A and B
// make C an admin with two votes.
func bootstrap(t *testing.T) (a, b, c *signer, log []*core.Entry, mkC1 *core.Entry) {
	a, b, c = newSigner(t, 1), newSigner(t, 2), newSigner(t, 3)
	add := func(e *core.Entry) *core.Entry { log = append(log, e); return e }
	create := add(a.sign(core.EntryCreate, "", core.EntryFields{}))
	addB := add(a.sign(core.EntryAdd, b.id, core.EntryFields{History: true}, create))
	mkB := add(a.sign(core.EntryAdmin, b.id, core.EntryFields{}, addB))
	addC := add(a.sign(core.EntryAdd, c.id, core.EntryFields{From: map[string][]string{a.id: {testRecord}}}, mkB))
	mkC1 = add(a.sign(core.EntryAdmin, c.id, core.EntryFields{}, addC))
	add(b.sign(core.EntryAdmin, c.id, core.EntryFields{}, mkC1))
	return a, b, c, log, mkC1
}

func machineOf(t *testing.T, log []*core.Entry, order ...int) *Machine {
	m, err := NewMachine(log[0])
	if err != nil {
		t.Fatal(err)
	}
	if order == nil {
		m.Receive(log...)
		return m
	}
	for _, i := range order {
		m.Receive(log[i])
	}
	return m
}

// The bootstrap sequence with real signed entries: the creator makes the
// second machine an admin alone, the third needs both admins. Machine and
// ResolveRoom agree, in any arrival order, and the permissions come from
// the grants' history and from fields.
//
// Requirements: T3, T9, RM2
func TestSignedBootstrapMakesThreeAdmins(t *testing.T) {
	a, b, c, log, mkC1 := bootstrap(t)
	st := mustResolve(t, log[0].ID(), log)
	want := sortedNodes(a, b, c)
	if !reflect.DeepEqual(st.Admins, want) || !reflect.DeepEqual(st.Members, want) {
		t.Fatalf("admins %v members %v", st.Admins, st.Members)
	}
	for _, e := range log {
		if !st.Effects[e.ID()].Applied {
			t.Fatalf("entry %s: %+v", e.ID(), st.Effects[e.ID()])
		}
	}
	if !st.Permissions[a.id].All || !st.Permissions[b.id].All {
		t.Fatalf("permissions of A, B: %+v %+v", st.Permissions[a.id], st.Permissions[b.id])
	}
	if p := st.Permissions[c.id]; p.All || !reflect.DeepEqual(p.From, map[string][]string{a.id: {testRecord}}) {
		t.Fatalf("permission of C: %+v", p)
	}
	// One vote for C is not enough.
	half := mustResolve(t, log[0].ID(), log[:5])
	if got := half.Effects[mkC1.ID()].Reason; got != ReasonNeedsSecond || len(half.Admins) != 2 {
		t.Fatalf("one vote: %q, admins %v", got, half.Admins)
	}
	for _, order := range [][]int{nil, {5, 4, 3, 2, 1, 0}, {2, 5, 1, 4, 3}} {
		m := machineOf(t, log, order...)
		if got := m.State(); !sameState(got, st) {
			t.Fatalf("order %v: machine %v / %v", order, got.Admins, got.Members)
		}
		if off := m.Offer(); !reflect.DeepEqual(ids(off...), ids(machineOf(t, off).Offer()...)) {
			t.Fatal("Offer does not replay")
		}
	}
}

// A removal with real signed entries: A and B remove C.
//
// Requirements: T9
func TestSignedRemovalByTwoAdmins(t *testing.T) {
	a, b, c, log, _ := bootstrap(t)
	head := log[len(log)-1]
	rA := a.sign(core.EntryRemove, c.id, core.EntryFields{}, head)
	rB := b.sign(core.EntryRemove, c.id, core.EntryFields{}, head)
	one := mustResolve(t, log[0].ID(), append(append([]*core.Entry(nil), log...), rA))
	if one.Effects[rA.ID()].Reason != ReasonNeedsSecond {
		t.Fatalf("one vote: %+v", one.Effects[rA.ID()])
	}
	log = append(log, rA, rB)
	m := machineOf(t, log)
	st := m.State()
	want := sortedNodes(a, b)
	if !reflect.DeepEqual(st.Admins, want) || !reflect.DeepEqual(st.Members, want) {
		t.Fatalf("admins %v members %v", st.Admins, st.Members)
	}
	if !sameState(st, mustResolve(t, log[0].ID(), log)) {
		t.Fatal("machine and ResolveRoom disagree")
	}
}

// A fork with real signed entries, settled by the other admin (rule 12):
// A and B are admins; B adds P, then adds D, and B restored from a backup
// adds E at the same position. Machines that saw different branches first
// disagree until A, on its machine, removes B naming only what it
// accepted; then both agree, and the branch A had accepted stands.
//
// Requirements: T9, D6
func TestSignedForkInTwoAdminRoomIsSettledByTheOtherAdmin(t *testing.T) {
	a, b := newSigner(t, 1), newSigner(t, 2)
	p, d, e := newSigner(t, 4), newSigner(t, 5), newSigner(t, 6)
	create := a.sign(core.EntryCreate, "", core.EntryFields{})
	addB := a.sign(core.EntryAdd, b.id, core.EntryFields{}, create)
	mkB := a.sign(core.EntryAdmin, b.id, core.EntryFields{}, addB)
	addP := b.sign(core.EntryAdd, p.id, core.EntryFields{}, mkB)
	addD := b.sign(core.EntryAdd, d.id, core.EntryFields{}, addP)
	addE := b.sign(core.EntryAdd, e.id, core.EntryFields{Seq: 2, Prev: addP.ID()}, addP)
	base := []*core.Entry{create, addB, mkB, addP}
	p1, p2 := machineOf(t, base), machineOf(t, base)
	p1.Receive(addD, addE)
	p2.Receive(addE, addD)
	if s1, s2 := p1.State(), p2.State(); sameState(s1, s2) || len(p1.Evidence()) != 1 || len(p2.Evidence()) != 1 {
		t.Fatalf("expected first-seen branches and evidence: %v / %v", s1.Members, s2.Members)
	}
	var accepted []*core.Entry
	for _, e := range p1.Offer() {
		if e.ID() != addE.ID() {
			accepted = append(accepted, e)
		}
	}
	rm := a.sign(core.EntryRemove, b.id, core.EntryFields{}, accepted...)
	p1.Receive(rm)
	p2.Receive(p1.Offer()...)
	p1.Receive(p2.Offer()...)
	s1, s2 := p1.State(), p2.State()
	if !sameState(s1, s2) {
		t.Fatalf("not settled: %v / %v", s1.Members, s2.Members)
	}
	if !reflect.DeepEqual(s1.Admins, []string{a.id}) || !reflect.DeepEqual(s1.Members, sortedNodes(a, p, d)) {
		t.Fatalf("admins %v members %v", s1.Admins, s1.Members)
	}
	if s1.Effects[addE.ID()].Reason != ReasonAfterCut {
		t.Fatalf("E's add: %+v", s1.Effects[addE.ID()])
	}
}

// A model entry (or any entry not made from a core entry) has no signed
// id, so the daemon's machine refuses it.
//
// Requirements: A4
func TestDaemonMachineRefusesEntriesWithoutASignedID(t *testing.T) {
	_, _, _, log, _ := bootstrap(t)
	m := machineOf(t, log)
	before := m.State()
	fake := newEntry(entry{Author: log[0].Author(), Seq: 9, Kind: kAdd, Target: "x", Parents: []string{log[0].ID()}})
	m.receive(fake)
	if m.invalid[fake.ID] != ReasonBadID || !sameState(m.State(), before) {
		t.Fatalf("got %q", m.invalid[fake.ID])
	}
}

func mustResolve(t *testing.T, root string, es []*core.Entry) State {
	t.Helper()
	st, err := ResolveRoom(root, es)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// The permission consumer: C may read A's records after its head, not the
// head or what precedes it, and a head it cannot relate hides everything
// by that author.
//
// Requirements: RM2
func TestPermissionHidesAtOrBeforeTheHeadAndFailsClosed(t *testing.T) {
	a, _, c, log, _ := bootstrap(t)
	p := mustResolve(t, log[0].ID(), log).Permissions[c.id]
	known := func(before map[string]bool) func(head, rec string) (bool, bool) {
		return func(head, rec string) (bool, bool) {
			if head != testRecord {
				return false, false
			}
			v, ok := before[rec]
			return v, ok
		}
	}
	rel := known(map[string]bool{testRecord: true, "early": true, "later": false})
	if !p.Hidden(a.id, testRecord, rel) || !p.Hidden(a.id, "early", rel) || p.Hidden(a.id, "later", rel) {
		t.Fatal("wrong records hidden")
	}
	if p.Hidden("someone-else", "x", rel) {
		t.Fatal("an author without a head is hidden")
	}
	missing := func(string, string) (bool, bool) { return false, false }
	if !p.Hidden(a.id, "later", missing) {
		t.Fatal("a head the caller does not hold must hide everything by that author")
	}
}

// The package checks what it relies on: an entry signed for another room
// is refused with a reason, an empty pin is an error, a corrupted
// signature is refused at the boundary, and entries parsed from the wire
// resolve as built.
//
// Requirements: A4, D6
func TestSignedIngressChecksRoomPinAndSignature(t *testing.T) {
	a, b, _, log, _ := bootstrap(t)
	create := log[0]
	other := a.sign(core.EntryAdd, newSigner(t, 9).id, core.EntryFields{Room: testRoom2}, log[len(log)-1])
	m := machineOf(t, log)
	res := m.Receive(other)
	if len(res) != 1 || res[0].Disposition != Invalid || res[0].Reason != ReasonOtherRoom {
		t.Fatalf("other room: %+v", res)
	}
	st := mustResolve(t, create.ID(), append(append([]*core.Entry(nil), log...), other))
	if st.Effects[other.ID()].Reason != ReasonOtherRoom {
		t.Fatalf("other room in ResolveRoom: %+v", st.Effects[other.ID()])
	}
	if _, err := ResolveRoom("", log); err == nil {
		t.Fatal("an empty pin must be an error")
	}
	good := b.sign(core.EntryAdd, newSigner(t, 8).id, core.EntryFields{}, log[len(log)-1])
	sig := good.Sig()
	sig[0] ^= 1
	bad, err := core.EntryFromParts(good.Header(), sig)
	if err != nil {
		t.Fatal(err)
	}
	if res := m.Receive(bad); len(res) != 1 || res[0].Reason != ReasonBadSignature {
		t.Fatalf("bad signature: %+v", res)
	}
	if st := mustResolve(t, create.ID(), append(append([]*core.Entry(nil), log...), bad)); st.Effects[bad.ID()].Reason != ReasonBadSignature {
		t.Fatalf("bad signature in ResolveRoom: %+v", st.Effects[bad.ID()])
	}
	// From the wire.
	var parsed []*core.Entry
	for _, e := range log {
		p, err := core.ParseEntry(e.Wire())
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, p)
	}
	if !sameState(machineOf(t, parsed).State(), mustResolve(t, create.ID(), log)) {
		t.Fatal("entries from the wire resolve differently")
	}
}

// Sequence numbers are int64 end to end.
//
// Requirements: A4
func TestSequenceNumbersKeepSixtyFourBits(t *testing.T) {
	a, _, _, log, _ := bootstrap(t)
	big := a.sign(core.EntryAdd, newSigner(t, 7).id, core.EntryFields{Seq: 1 << 31, Prev: testRecord}, log[len(log)-1])
	if e := fromCore(big); e.Seq != 1<<31 {
		t.Fatalf("seq %d", e.Seq)
	}
	m := machineOf(t, log)
	if res := m.Receive(big); len(res) != 1 || res[0].Disposition != Held {
		t.Fatalf("got %+v", res)
	}
}
