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

// machineWith returns a machine for the room of the create entry in es
// (the test room's create if es has none), having received es.
func machineWith(es []Entry) *Machine {
	root := New(Entry{Author: "A", Seq: 1, Kind: Create})
	for _, e := range es {
		if e.Kind == Create {
			root = e
			break
		}
	}
	m := NewMachine(Final, root)
	m.Receive(es...)
	return m
}

func sameState(a, b State) bool {
	return reflect.DeepEqual(a.Members, b.Members) && reflect.DeepEqual(a.Admins, b.Admins) && reflect.DeepEqual(a.Admitted, b.Admitted)
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

// C7 and known gap 1, closed by rule 12: in a room of two admins, B is
// restored from a backup and signs a second entry at a position it had
// used. Machines that saw different branches first disagree until A, holding
// both branches, removes B: the fork counts as B's own vote, so A's vote
// completes the removal and every machine reaches the same state.
//
// Requirements: T9, D6
func TestForkInTwoAdminRoomIsSettledByTheOtherAdminAlone(t *testing.T) {
	r, h1, addD, addE := twoAdminFork(t)
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
	s1, s2 := p1.State(), p2.State()
	if !sameState(s1, s2) {
		t.Fatalf("not settled: %v/%v vs %v/%v", s1.Admins, s1.Members, s2.Admins, s2.Members)
	}
	wantList(t, "admins", s1.Admins, "A")
	wantApplied(t, s1, ra)
}

// twoAdminFork builds A and B as admins, B's add of P, then B's add of D
// and, at the same position from a restored backup, B's add of E.
func twoAdminFork(t *testing.T) (r *room, h1 []string, addD, addE string) {
	r = newRoom(t, "A")
	r.admin("A", "B")
	lastB := r.next("B", Add, "P", r.all()...)
	h1 = r.all()
	addD = r.add("B", "D", h1...)
	addE = r.fork("B", 2, lastB, Add, "E", h1...)
	return r, h1, addD, addE
}

// Rule 12, what stands: when a single remover settles a fork, the branch it
// had seen take effect stands and the other branch does not; a remover
// that had seen both sees neither take effect, so neither stands.
//
// Requirements: T9, D6
func TestForkSettledByOneRemoverKeepsTheBranchItHadSeen(t *testing.T) {
	r, h1, addD, addE := twoAdminFork(t)
	ra := r.remove("A", "B", append(h1, addD)...)
	st := r.state(Final)
	wantApplied(t, st, ra)
	wantApplied(t, st, addD)
	wantReason(t, st, addE, ReasonAfterCut)
	wantList(t, "members", st.Members, "A", "D", "P")

	r, h1, addD, addE = twoAdminFork(t)
	r.remove("A", "B", append(h1, addD, addE)...)
	st = r.state(Final)
	wantReason(t, st, addD, ReasonAfterCut)
	wantReason(t, st, addE, ReasonAfterCut)
	wantList(t, "members", st.Members, "A", "P")
}

// Rule 12 needs two different entries signed by one key at one position.
// Entries by different keys at the same position, or one entry offered
// twice, are not a fork, and A's vote to remove B still needs a second.
//
// Requirements: T9
func TestForkEvidenceNeedsTwoEntriesByOneKeyAtOnePosition(t *testing.T) {
	r := newRoom(t, "A")
	r.admin("A", "B")
	addP := r.add("B", "P", r.all()...) // B#1
	addQ := r.add("A", "Q", r.all()...) // A#3
	r.add("B", "D", r.all()...)         // B#2, as A#2 is
	ra := r.remove("A", "B", r.all()...)
	es := r.entries(r.all()...)
	es = append(es, r.byID[addP], r.byID[addQ], r.byID[ra])
	st := Final.Resolve(es)
	wantReason(t, st, ra, ReasonNeedsSecond)
	wantList(t, "admins", st.Admins, "A", "B")
}

// Rule 12 counts only forks by an admin. A member that is not an admin
// signs no entry with effect, so its fork changes nothing: both of its
// entries are in every machine's set and nobody needs to be removed.
//
// Requirements: T9, D6
func TestForkByMemberWhoIsNotAnAdminChangesNothing(t *testing.T) {
	r := newRoom(t, "A")
	r.admin("A", "B")
	r.add("A", "M", r.all()...)
	h0 := r.all()
	m1 := r.add("M", "X", h0...)
	m2 := r.put(Entry{Author: "M", Seq: 1, Kind: Add, Target: "Y", Parents: h0})
	ab := r.add("A", "Z", append(h0, m2)...)
	p1, p2 := machineWith(r.entries(h0...)), machineWith(r.entries(h0...))
	p1.Receive(r.byID[m1], r.byID[m2], r.byID[ab])
	p2.Receive(r.byID[m2], r.byID[m1], r.byID[ab])
	s1, s2 := p1.State(), p2.State()
	if !sameState(s1, s2) || !s1.members["Z"] {
		t.Fatalf("a member's fork split the machines: %v vs %v", s1.Members, s2.Members)
	}
	wantList(t, "admins", s1.Admins, "A", "B")
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

// Found by the deep fork search: A, the only admin, added B, removed it,
// then made it an admin again; A and B made C an admin. A's restored
// backup re-signs A's second position, making B an admin there, and B
// settles the fork. B's earlier removal, which C's promotion had seen as a
// member's removal, counts here as an admin's (the backup's grant is
// concurrent with it); C's promotion still stands.
//
// Requirements: T9, D6
func TestForkTurningAnEarlierRemovalIntoAnAdminRemovalKeepsLaterGrants(t *testing.T) {
	r := newRoom(t, "A")
	e1 := r.add("A", "B")
	e2 := r.remove("A", "B", r.all()...)
	e3 := r.admin("A", "B", r.all()...)
	e4 := r.admin("A", "C", r.all()...)
	e5 := r.next("B", MakeAdmin, "C", r.root, e1, e2, e3)
	r.fork("A", 2, r.root, MakeAdmin, "B", r.root)
	r.remove("B", "A", r.root, e1, e2, e3, e4, e5)
	st := r.state(Final)
	wantApplied(t, st, e5)
	wantList(t, "admins", st.Admins, "B", "C")
}

// Limit (known gap 4, removal form), found by the deep seeded search: B
// forked and A and C removed it. B, not yet knowing, votes to remove D;
// the vote has no effect (rule 12). A stolen A key seconds it, claiming not
// to have seen C's vote, then votes again to remove B having seen B's vote
// take effect: D is removed. As in gap 4 the same entries arise honestly
// (A seconded B's vote in good faith, then removed B), so no rule can
// refuse it; it needs B's signature from before its removal.
//
// Requirements: T9, D6
func TestStolenKeyCanSecondAForkingKeysVoidedRemovalAndThenJoinTheRemoval(t *testing.T) {
	r, _ := threeAdmins(t)
	r.admin("A", "D", r.all()...)
	r.admin("B", "D", r.all()...)
	h0 := r.all()
	lastB := r.last["B"]
	addD := r.add("B", "D", h0...)
	addE := r.fork("B", lastB.Seq+1, lastB.ID, Add, "E", h0...)
	r.remove("C", "B", append(h0, addE)...)
	ra := r.remove("A", "B", append(h0, addD)...)
	rbD := r.remove("B", "D", append(h0, addD, ra)...)
	wantReason(t, r.state(Final), rbD, ReasonFork)
	r.remove("A", "D", append(h0, addD, ra, rbD)...)
	r.remove("A", "B", append(h0, addD, ra, rbD)...)
	if r.state(Final).admins["D"] {
		t.Fatal("the limit no longer holds: update known gap 4 and atRest")
	}
	t.Skip("KNOWN GAP 4: one key can second a removed admin's voided vote and then revive it by joining the removal")
}

func holds(m *Machine, ids ...string) bool {
	for _, id := range ids {
		if _, ok := m.set[id]; !ok {
			return false
		}
	}
	return true
}

// Two independent forks: B and C, both admins of four, each sign a second
// entry at a position they had used. One open fork does not hold back the
// other's settlement: once a removal of B arrives, every machine takes
// both of B's branches and the removal whatever the state of C's fork, and
// then likewise for C, in either order. Each remover had not yet received
// the other fork. (A removal that names one branch of the other, still
// open fork waits on machines holding that fork's other branch, like every
// entry depending on a refused one, and is taken once that fork settles:
// TestSettlingRemovalThatNamesAnotherOpenForkWaitsForIt.)
//
// Requirements: T9, D6
func TestOneOpenForkDoesNotHoldBackSettlingAnother(t *testing.T) {
	for _, bFirst := range []bool{true, false} {
		r, _ := threeAdmins(t)
		r.admin("A", "D", r.all()...)
		r.admin("B", "D", r.all()...)
		h0 := r.all()
		lastB, lastC := r.last["B"], r.last["C"]
		b1 := r.add("B", "X", h0...)
		b2 := r.fork("B", lastB.Seq+1, lastB.ID, Add, "Y", h0...)
		var c1, c2 string
		if lastC.ID == "" {
			c1 = r.add("C", "Z", h0...)
			c2 = r.put(Entry{Author: "C", Seq: 1, Kind: Add, Target: "W", Parents: h0})
		} else {
			c1 = r.add("C", "Z", h0...)
			c2 = r.fork("C", lastC.Seq+1, lastC.ID, Add, "W", h0...)
		}
		p1, p2 := machineWith(r.entries(h0...)), machineWith(r.entries(h0...))
		p1.Receive(r.byID[b1], r.byID[c1], r.byID[b2], r.byID[c2])
		p2.Receive(r.byID[b2], r.byID[c2], r.byID[b1], r.byID[c1])
		rmB := r.remove("A", "B", append(h0, b1)...)
		rmC := r.remove("D", "C", append(h0, c2)...)
		first, second := rmB, rmC
		firstFork, secondFork := []string{b1, b2}, []string{c1, c2}
		if !bFirst {
			first, second = rmC, rmB
			firstFork, secondFork = secondFork, firstFork
		}
		for _, m := range []*Machine{p1, p2} {
			m.Receive(r.byID[first])
		}
		p1.Receive(p2.Offer()...)
		p2.Receive(p1.Offer()...)
		for i, m := range []*Machine{p1, p2} {
			if !holds(m, firstFork...) || !holds(m, first) {
				t.Fatalf("bFirst=%v machine %d: first fork not settled while the other is open", bFirst, i+1)
			}
			if holds(m, secondFork...) {
				t.Fatalf("bFirst=%v machine %d: second fork accepted before its removal", bFirst, i+1)
			}
		}
		for _, m := range []*Machine{p1, p2} {
			m.Receive(r.byID[second])
		}
		p1.Receive(p2.Offer()...)
		p2.Receive(p1.Offer()...)
		if !holds(p1, secondFork...) || !holds(p2, secondFork...) || !sameState(p1.State(), p2.State()) {
			t.Fatalf("bFirst=%v: not settled: %v / %v", bFirst, p1.State().Members, p2.State().Members)
		}
		wantList(t, "admins", p1.State().Admins, "A", "D")
	}
}

// The rule the design states: an admin signs a settling removal naming as
// parents only entries it has accepted, never the branch it refused. Then
// the branch it had accepted stands and the other does not. A signer that
// names both branches sees neither take effect, so neither stands.
//
// Requirements: T9, D6
func TestSettlingRemovalNamesOnlyAcceptedEntries(t *testing.T) {
	r, h1, addD, addE := twoAdminFork(t)
	a := machineWith(r.entries(h1...))
	a.Receive(r.byID[addD], r.byID[addE])
	if !holds(a, addD) || holds(a, addE) {
		t.Fatal("expected A to accept D's branch and refuse E's")
	}
	rm := r.remove("A", "B", idsOf(a.Entries())...)
	a.Receive(r.byID[rm])
	other := machineWith(r.entries(h1...))
	other.Receive(r.byID[addE], r.byID[addD])
	other.Receive(a.Offer()...)
	for _, m := range []*Machine{a, other} {
		st := m.State()
		wantApplied(t, st, addD)
		wantReason(t, st, addE, ReasonAfterCut)
		wantList(t, "members", st.Members, "A", "D", "P")
	}

	r, h1, addD, addE = twoAdminFork(t)
	r.remove("A", "B", append(h1, addD, addE)...)
	st := r.state(Final)
	wantReason(t, st, addD, ReasonAfterCut)
	wantReason(t, st, addE, ReasonAfterCut)
	wantList(t, "members", st.Members, "A", "P")
}

// A removal of B signed by A, which had accepted B's first branch and C's
// first branch, names C's first branch as a parent. A machine that holds
// C's other branch refuses it until C's fork is settled, then takes both
// forks; every machine ends in the same state.
//
// Requirements: T9, D6
func TestSettlingRemovalThatNamesAnotherOpenForkWaitsForIt(t *testing.T) {
	r, _ := threeAdmins(t)
	r.admin("A", "D", r.all()...)
	r.admin("B", "D", r.all()...)
	h0 := r.all()
	lastB := r.last["B"]
	b1 := r.add("B", "X", h0...)
	b2 := r.fork("B", lastB.Seq+1, lastB.ID, Add, "Y", h0...)
	c1 := r.add("C", "Z", h0...)
	c2 := r.put(Entry{Author: "C", Seq: 1, Kind: Add, Target: "W", Parents: h0})
	p1, p2 := machineWith(r.entries(h0...)), machineWith(r.entries(h0...))
	p1.Receive(r.byID[b1], r.byID[c1], r.byID[b2], r.byID[c2])
	p2.Receive(r.byID[b2], r.byID[c2], r.byID[b1], r.byID[c1])
	rmB := r.remove("A", "B", idsOf(p1.Entries())...)
	p1.Receive(r.byID[rmB])
	p2.Receive(p1.Offer()...)
	if holds(p2, rmB) {
		t.Fatal("expected the removal to wait on a machine holding C's other branch")
	}
	rmC := r.remove("D", "C", idsOf(p2.Entries())...)
	p2.Receive(r.byID[rmC])
	p1.Receive(p2.Offer()...)
	p2.Receive(p1.Offer()...)
	if !holds(p1, b1, b2, c1, c2, rmB, rmC) || !holds(p2, b1, b2, c1, c2, rmB, rmC) || !sameState(p1.State(), p2.State()) {
		t.Fatalf("not settled: %v / %v", p1.State().Members, p2.State().Members)
	}
	wantList(t, "admins", p1.State().Admins, "A", "D")
}
