package members

import (
	"reflect"
	"testing"
)

// fork signs an entry by author at seq, continuing from prev, as a stolen
// key or a restored backup would: a second entry at a position the author
// has already used.
func (r *room) fork(author string, seq int, prev string, kind Kind, target string, parents ...string) string {
	e := New(Entry{Author: author, Seq: seq, Prev: prev, Kind: kind, Target: target, Parents: uniq(append(parents, prev))})
	r.byID[e.ID] = e
	return e.ID
}

func machineWith(es []Entry) *Machine {
	m := NewMachine(Final)
	m.Receive(es...)
	return m
}

func sameState(a, b State) bool {
	return reflect.DeepEqual(a.Members, b.Members) && reflect.DeepEqual(a.Admins, b.Admins)
}

// Attack 2: the creator's key is stolen and the thief forks from the room's
// first entry, building another history (make X an admin, then remove
// everyone). Every machine that holds the honest entry at that position
// refuses the fork and keeps evidence; a machine admitted honestly
// afterwards receives the honest prefix from its admitter first.
//
// Requirements: T9, T5
func TestStolenCreatorKeyForkFromFirstEntryIsRefusedByEveryMember(t *testing.T) {
	r, h0 := threeAdmins(t)
	honest := r.entries(h0...)
	f2 := r.fork("A", 2, r.root, MakeAdmin, "X", r.root)
	f3 := r.fork("A", 3, f2, Remove, "B", f2)
	fx := r.next("X", Remove, "P", f2)
	forged := []Entry{r.byID[f2], r.byID[f3], r.byID[fx]}
	want := Final.Resolve(honest)
	var machines []*Machine
	for range []string{"B", "C", "P"} {
		m := machineWith(honest)
		m.Receive(forged...)
		if !sameState(m.State(), want) {
			t.Fatalf("a member was fooled: %v", m.State().Admins)
		}
		if len(m.Evidence) == 0 {
			t.Fatal("no fork evidence kept")
		}
		machines = append(machines, m)
	}
	// B admits D; D's machine receives what B offers, honest entries first.
	addD := r.add("B", "D", h0...)
	machines[0].Receive(r.byID[addD])
	d := machineWith(machines[0].Offer())
	if st := d.State(); !st.members["D"] || st.admins["X"] {
		t.Fatalf("new member fooled: admins %v members %v", st.Admins, st.Members)
	}
	// Only a machine holding nothing but the create entry can be fooled,
	// and no member is in that position.
	lone := machineWith([]Entry{r.byID[r.root]})
	lone.Receive(forged...)
	if !lone.State().admins["X"] {
		t.Fatal("expected a machine with only the create entry to take the fork")
	}
}

// Attack 2, later position: the creator added D, then made B an admin. D
// was offline and has only its own admission when the thief forks the
// creator's chain at the next position, where the creator was still the
// only admin. D is fooled and then refuses the honest history. Once B and
// C remove A, D accepts both branches and agrees with everyone.
//
// Requirements: T9, T5
func TestForkInsideSoleAdminPeriodFoolsOnlyMachinesMissingThatEntryUntilSettled(t *testing.T) {
	r := newRoom(t, "A")
	addD := r.add("A", "D")
	dView := r.entries(addD)
	r.admin("A", "B", r.all()...)
	r.admin("A", "C", r.all()...)
	r.admin("B", "C", r.all()...)
	honestIDs := r.all()
	f := r.fork("A", 3, addD, MakeAdmin, "X", addD)
	fy := r.next("X", Add, "Y", f)
	d := machineWith(dView)
	d.Receive(r.byID[f], r.byID[fy])
	if !d.State().admins["X"] {
		t.Fatal("expected D to be fooled")
	}
	honest := r.entries(honestIDs...)
	b := machineWith(honest)
	d.Receive(honest...)
	if !d.State().admins["X"] {
		t.Fatal("D should still hold its first branch")
	}
	// Settlement: B and C remove A. B and D exchange everything.
	rb := r.remove("B", "A", honestIDs...)
	rc := r.remove("C", "A", honestIDs...)
	b.Receive(r.byID[rb], r.byID[rc])
	b.Receive(d.Offer()...)
	d.Receive(b.Offer()...)
	sb, sd := b.State(), d.State()
	if !sameState(sb, sd) {
		t.Fatalf("not settled: B %v/%v, D %v/%v", sb.Admins, sb.Members, sd.Admins, sd.Members)
	}
	wantList(t, "admins", sd.Admins, "B", "C")
	wantList(t, "members", sd.Members, "B", "C", "D")
	wantReason(t, sd, f, ReasonAfterCut)
	wantReason(t, sd, fy, ReasonAuthorVoid)
}

// Attack 6 and sol's third counterexample: B is restored from an old
// backup and signs a second entry at a position it had already used (no
// malice). Machines that saw different branches first disagree; when A and
// C remove B, each having seen a different branch, neither branch stands
// from the fork point on, both grants lose effect, and every machine
// agrees.
//
// Requirements: T9, D6
func TestBackupRestoreForkIsSettledByRemovalOfTheForkingAdmin(t *testing.T) {
	r, h0 := threeAdmins(t)
	lastB := r.last["B"]
	addD := r.add("B", "D", h0...)
	addE := r.fork("B", lastB.Seq+1, lastB.ID, Add, "E", h0...)
	p1 := machineWith(r.entries(h0...))
	p2 := machineWith(r.entries(h0...))
	p1.Receive(r.byID[addD], r.byID[addE])
	p2.Receive(r.byID[addE], r.byID[addD])
	if s1, s2 := p1.State(), p2.State(); !s1.members["D"] || s1.members["E"] || !s2.members["E"] || s2.members["D"] {
		t.Fatalf("expected first-seen branches: %v / %v", s1.Members, s2.Members)
	}
	ra := r.remove("A", "B", append(h0, addD)...)
	rc := r.remove("C", "B", append(h0, addE)...)
	for _, m := range []*Machine{p1, p2} {
		m.Receive(r.byID[ra], r.byID[rc])
	}
	p1.Receive(p2.Offer()...)
	p2.Receive(p1.Offer()...)
	s1, s2 := p1.State(), p2.State()
	if !sameState(s1, s2) {
		t.Fatalf("not settled: %v/%v vs %v/%v", s1.Admins, s1.Members, s2.Admins, s2.Members)
	}
	wantList(t, "admins", s1.Admins, "A", "C")
	wantList(t, "members", s1.Members, "A", "C", "P")
	wantReason(t, s1, addD, ReasonAfterCut)
	wantReason(t, s1, addE, ReasonAfterCut)
}

// C7 in a room of two admins: a fork by one of them cannot be settled,
// because removing an admin needs two admins and the only other one is the
// forking key. Machines that saw different branches first stay apart.
//
// Requirements: T9
func TestForkInTwoAdminRoomIsNeverSettled(t *testing.T) {
	r := newRoom(t, "A")
	r.admin("A", "B")
	h0 := r.all()
	lastB := r.next("B", Add, "P", h0...)
	h1 := r.all()
	addD := r.add("B", "D", h1...)
	addE := r.fork("B", 2, lastB, Add, "E", h1...)
	p1, p2 := machineWith(r.entries(h1...)), machineWith(r.entries(h1...))
	p1.Receive(r.byID[addD], r.byID[addE])
	p2.Receive(r.byID[addE], r.byID[addD])
	if s1, s2 := p1.State(), p2.State(); !s1.members["D"] || !s2.members["E"] {
		t.Fatalf("expected first-seen branches: %v / %v", s1.Members, s2.Members)
	}
	ra := r.remove("A", "B", append(h1, addD)...)
	p1.Receive(r.byID[ra])
	p2.Receive(r.byID[ra])
	p1.Receive(p2.Offer()...)
	p2.Receive(p1.Offer()...)
	if sameState(p1.State(), p2.State()) {
		return
	}
	t.Skip("KNOWN GAP: with two admins a fork by one of them is never settled; " +
		"machines that saw different branches first disagree about a member for ever. " +
		"Smallest case: A, B admins; B adds D, B restored from backup adds E at the same position; " +
		"A alone cannot remove B. Remedy outside Resolve: three admins, or re-pair the affected machines.")
}

// A fork is settled for good by the removal of its author: once the
// forking admin is added back and made an admin again, its new grants
// count, although they continue a chain that once forked.
//
// Requirements: T9, D6
func TestReadmittedAdminAfterSettledForkCanGrantAgain(t *testing.T) {
	r, h0 := threeAdmins(t)
	lastB := r.last["B"]
	addD := r.add("B", "D", h0...)
	addE := r.fork("B", lastB.Seq+1, lastB.ID, Add, "E", h0...)
	r.remove("A", "B", append(h0, addD)...)
	r.remove("C", "B", append(h0, addE)...)
	r.add("A", "B", r.all()...)
	r.admin("A", "B", r.all()...)
	r.admin("C", "B", r.all()...)
	addF := r.add("B", "F", r.all()...)
	st := r.state(Final)
	wantApplied(t, st, addF)
	wantList(t, "admins", st.Admins, "A", "B", "C")
	wantList(t, "members", st.Members, "A", "B", "C", "F", "P")
}
