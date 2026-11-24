package members

import "testing"

// A malformed entry by B (its previous entry is another author's, or its
// position does not follow its previous entry) arrives before B's valid
// entry at that position. It reserves nothing: the valid entry is accepted
// and no fork evidence is recorded, so it cannot count as B's vote for its
// own removal (rule 12). Only B's key can sign an entry authored by B, so
// nobody else can cause evidence against B; with this check not even a
// malformed entry signed by B does.
//
// Requirements: T9, D6
func TestMalformedChainEntryReservesNoPosition(t *testing.T) {
	r := newRoom(t, "A")
	mkB := r.admin("A", "B")
	b1 := r.add("B", "P", r.all()...)
	h := r.all()
	// Previous entry by another author, at B's next position.
	badPrev := New(Entry{Author: "B", Seq: 2, Prev: mkB, Kind: Add, Target: "X", Parents: h})
	// Position that does not follow the previous entry.
	badSeq := New(Entry{Author: "B", Seq: 3, Prev: b1, Kind: Add, Target: "Y", Parents: h})
	b2 := r.add("B", "D", h...)
	b3 := r.add("B", "Q", r.all()...)
	m := machineWith(r.entries(h...))
	m.Receive(badPrev, badSeq)
	m.Receive(r.byID[b2], r.byID[b3])
	if !holds(m, b2, b3) {
		t.Fatal("a malformed entry kept B's valid entry out")
	}
	if len(m.Evidence) != 0 {
		t.Fatalf("fork evidence recorded from a malformed entry: %v", m.Evidence)
	}
	for _, e := range m.Offer() {
		if e.ID == badPrev.ID || e.ID == badSeq.ID {
			t.Fatal("a malformed entry is passed on")
		}
	}
	if m.Invalid[badPrev.ID] == "" || m.Invalid[badSeq.ID] == "" {
		t.Fatalf("malformed entries refused without a reason: %v", m.Invalid)
	}
	st := Final.ResolveRoom(r.root, append(m.Entries(), badPrev, badSeq))
	wantReason(t, st, badPrev.ID, ReasonBadChain)
	wantReason(t, st, badSeq.ID, ReasonBadChain)
	wantList(t, "members", st.Members, "A", "B", "D", "P", "Q")
}

// A second create entry, by any key, at a position not yet used: the
// machine is pinned to its room's create and refuses it, and Resolve given
// the room's create treats it as having no effect, with a reason, instead
// of returning an empty room.
//
// Requirements: T9, A4
func TestSecondCreateIsRefusedAndDoesNotEmptyTheRoom(t *testing.T) {
	r, _ := threeAdmins(t)
	want := r.state(Final)
	for _, k := range []string{"A", "B", "C", "Z"} {
		extra := New(Entry{Author: k, Seq: 1, Kind: Create})
		m := machineWith(r.entries(r.all()...))
		m.Receive(extra)
		if holds(m, extra.ID) && extra.ID != r.root {
			t.Fatalf("%s: machine accepted a second create", k)
		}
		if len(m.Evidence) != 0 {
			t.Fatalf("%s: a second create recorded fork evidence", k)
		}
		if !sameState(m.State(), want) {
			t.Fatalf("%s: second create changed the machine: %v", k, m.State().Members)
		}
		st := Final.ResolveRoom(r.root, append(r.entries(r.all()...), extra))
		if !sameState(st, want) {
			t.Fatalf("%s: Resolve with a second create gave %v / %v", k, st.Admins, st.Members)
		}
		if extra.ID != r.root {
			wantReason(t, st, extra.ID, ReasonOtherCreate)
		}
	}
}

// Offer returns accepted entries in causal order, deterministically, so a
// fresh machine replaying it never holds an entry waiting for another and
// ends with everything accepted. (The second branch of a settled fork is
// refused on arrival and taken when the settling removal arrives.)
//
// Requirements: D6
func TestOfferReplaysIntoAFreshMachineWithNothingHeld(t *testing.T) {
	r, _ := threeAdmins(t)
	r.admin("A", "D", r.all()...)
	r.admin("B", "D", r.all()...)
	h0 := r.all()
	lastB := r.last["B"]
	addD := r.add("B", "D", h0...)
	addE := r.fork("B", lastB.Seq+1, lastB.ID, Add, "E", h0...)
	r.remove("A", "B", append(h0, addD)...)
	r.add("C", "F", r.all()...)
	m := machineWith(nil)
	for _, id := range sortedKeys(r.byID) {
		m.Receive(r.byID[id])
	}
	m.Receive(r.byID[addE])
	first := m.Offer()
	if again := m.Offer(); len(again) != len(first) {
		t.Fatal("Offer is not deterministic")
	} else {
		for i := range first {
			if first[i].ID != again[i].ID {
				t.Fatal("Offer is not deterministic")
			}
		}
	}
	fresh := NewMachine(Final, r.byID[r.root])
	n := len(m.Entries())
	for i, e := range first[:n] {
		fresh.Receive(e)
		if len(fresh.held) != 0 {
			t.Fatalf("entry %d of Offer arrived before an entry it had seen", i)
		}
	}
	for _, e := range m.Entries() {
		if !holds(fresh, e.ID) {
			t.Fatalf("replay did not accept %s", e.ID)
		}
	}
	if !sameState(fresh.State(), m.State()) {
		t.Fatal("replay gave a different state")
	}
}
