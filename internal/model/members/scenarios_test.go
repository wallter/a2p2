package members

import "testing"

// X1 (q10-claude) and sol's first counterexample: an admin is removed while,
// out of touch, it removes a compromised machine. The removal of the
// compromised machine must stand.
//
// Requirements: T5, T9
func TestRemovedAdminsConcurrentRemovalOfCompromisedMachineStands(t *testing.T) {
	r, h0 := threeAdmins(t)
	k := r.add("A", "K", h0...)
	h1 := r.all()
	// A and B, out of touch with C, remove C.
	ra := r.remove("A", "C", h1...)
	rb := r.remove("B", "C", h1...)
	// C, reaching only P, finds K compromised and removes it.
	rk := r.remove("C", "K", h1...)
	st := r.state(Final)
	wantApplied(t, st, ra)
	wantApplied(t, st, rb)
	wantApplied(t, st, rk)
	wantApplied(t, st, k)
	wantList(t, "admins", st.Admins, "A", "B")
	wantList(t, "members", st.Members, "A", "B", "P")
}

// The same with the lead's rules and two admins: the creator alone removes
// the other admin, who concurrently removes K. K stays removed.
//
// Requirements: T5, T9
func TestLeadRulesAlsoKeepConcurrentRemovalByRemovedAdmin(t *testing.T) {
	r := newRoom(t, "C")
	r.admin("C", "A")
	r.add("C", "K", r.all()...)
	r.add("C", "P", r.all()...)
	h0 := r.all()
	r.remove("C", "A", h0...)
	r.remove("A", "K", h0...)
	st := r.state(Lead)
	wantList(t, "admins", st.Admins, "C")
	wantList(t, "members", st.Members, "C", "P")
}

// X2 (q10-claude), sol's second counterexample: the creator has no lasting
// special power. Two other admins remove it, and its key alone can no
// longer remove anyone or name an admin.
//
// Requirements: T9
func TestCreatorKeyIsRemovableByTwoOtherAdmins(t *testing.T) {
	r, h0 := threeAdmins(t)
	r.remove("B", "A", h0...)
	r.remove("C", "A", h0...)
	h1 := r.all()
	st := r.state(Final)
	wantList(t, "admins", st.Admins, "B", "C")
	wantList(t, "members", st.Members, "B", "C", "P")
	// The stolen creator key, after its removal, claims it never saw it.
	x := r.admin("A", "X", h0...)
	rmB := r.remove("A", "B", h0...)
	rmP := r.remove("A", "P", h0...)
	st = r.state(Final)
	wantReason(t, st, x, ReasonAfterCut)
	wantReason(t, st, rmB, ReasonNeedsSecond)
	wantApplied(t, st, rmP) // the nuisance: removals by a removed key still count
	wantList(t, "admins", st.Admins, "B", "C")
	wantList(t, "members", st.Members, "B", "C")
	_ = h1
}

// X3 (q10-claude): there are no ranks, so nothing can be gained by
// choosing a key. Every pair of admins can remove the third, and no single
// admin can remove another, whatever the key names are.
//
// Requirements: T9
func TestNoRankAnyTwoAdminsRemoveTheThirdNoneAlone(t *testing.T) {
	for _, target := range []string{"A", "B", "C"} {
		var others []string
		for _, k := range []string{"A", "B", "C"} {
			if k != target {
				others = append(others, k)
			}
		}
		r, h0 := threeAdmins(t)
		alone := r.remove(others[0], target, h0...)
		st := r.state(Final)
		wantReason(t, st, alone, ReasonNeedsSecond)
		if !st.admins[target] {
			t.Fatalf("%s removed by one admin", target)
		}
		r.remove(others[1], target, h0...)
		st = r.state(Final)
		if st.members[target] {
			t.Fatalf("%s not removed by two admins", target)
		}
	}
}

// X4 (q10-claude): a machine added and made admin with the help of an
// admin that was being removed at the same moment. Its grants have no
// effect, but a removal it made while it appeared to be an admin stands.
//
// Requirements: T5, T9
func TestCascadeFromVoidAdminVoidsItsGrantsKeepsItsRemovals(t *testing.T) {
	r, _ := threeAdmins(t)
	r.admin("A", "D", r.all()...)
	r.admin("B", "D", r.all()...)
	r.add("A", "Q", r.all()...)
	h0 := r.all()
	wantList(t, "admins", r.state(Final).Admins, "A", "B", "C", "D")
	// A and B remove C, having seen h0.
	r.remove("A", "C", h0...)
	r.remove("B", "C", h0...)
	// Meanwhile C and D make M an admin; M removes Q and adds N.
	gc := r.admin("C", "M", h0...)
	gd := r.admin("D", "M", h0...)
	h1 := append(append([]string(nil), h0...), gc, gd)
	rq := r.remove("M", "Q", h1...)
	an := r.add("M", "N", append(h1, rq)...)
	st := r.state(Final)
	wantReason(t, st, gc, ReasonAfterCut)
	wantReason(t, st, gd, ReasonNeedsSecond)
	wantApplied(t, st, rq)
	wantReason(t, st, an, ReasonAuthorVoid)
	wantList(t, "admins", st.Admins, "A", "B", "D")
	wantList(t, "members", st.Members, "A", "B", "D", "P")
}

// Attack 3 under the lead's rules: with two admins one may promote alone,
// so a stolen idle key signing on an old view takes the room over.
//
// Requirements: T9
func TestLeadRulesLetIdleStolenKeyTakeOverByBackdating(t *testing.T) {
	r := newRoom(t, "A")
	r.admin("A", "B")
	r.add("A", "P", r.all()...)
	old := r.all() // what B had seen: admins A and B
	r.admin("A", "C", r.all()...)
	wantList(t, "admins", r.state(Lead).Admins, "A", "B", "C")
	// B's key is stolen; the thief signs on B's chain as if asleep.
	ra := r.remove("B", "A", old...)
	x := r.admin("B", "X", old...)
	st := r.state(Lead)
	wantApplied(t, st, x)
	wantApplied(t, st, ra)
	// A is out, and A's promotion of C, which B had not seen, is void with
	// it: the thief holds the room.
	wantList(t, "lead admins", st.Admins, "B", "X")
	// The same entries under the final rules: A cannot promote C alone in
	// the first place, and B's backdated entries need a second admin.
	st = r.state(Final)
	wantList(t, "admins", st.Admins, "A", "B")
	wantReason(t, st, x, ReasonNeedsSecond)
	wantReason(t, st, ra, ReasonNeedsSecond)
}

// Attack 3 under the final rules: A and C make D an admin while B is
// asleep. B's key is stolen; the thief signs on B's chain, claiming the old
// view, and tries to remove D (a plain member in that view), remove A,
// and name X. None of it touches the admin set.
//
// Requirements: T9
func TestIdleStolenKeyBackdatingCannotChangeAdminSet(t *testing.T) {
	r, _ := threeAdmins(t)
	r.add("A", "D", r.all()...)
	old := r.all()
	r.admin("A", "D", r.all()...)
	r.admin("C", "D", r.all()...)
	wantList(t, "admins", r.state(Final).Admins, "A", "B", "C", "D")
	rd := r.remove("B", "D", old...)
	ra := r.remove("B", "A", old...)
	x := r.admin("B", "X", old...)
	st := r.state(Final)
	wantReason(t, st, rd, ReasonNeedsSecond)
	wantReason(t, st, ra, ReasonNeedsSecond)
	wantReason(t, st, x, ReasonNeedsSecond)
	wantList(t, "admins", st.Admins, "A", "B", "C", "D")
	// Judging the target only in the author's own past (lead's C4) lets
	// the thief remove the new admin alone.
	st = Rules{SoloLimit: 1, TargetJudgedInOwnPast: true}.Resolve(r.entries())
	if st.members["D"] {
		t.Fatalf("expected own-past judgement to lose D; admins %v", st.Admins)
	}
}

// Attack 4: a removed admin keeps signing. Grants no remover had seen have
// no effect; removals still count. The nuisance is bounded by the key's own
// chain: having removed P once, it can remove P again only from a view in
// which P was re-added, and an admin that re-adds P knowing of the key's
// removal puts that removal into the view.
//
// Requirements: T5, T9
func TestRemovedAdminKeepsSigningGrantsVoidRemovalsCountOncePerTarget(t *testing.T) {
	r, h0 := threeAdmins(t)
	r.remove("A", "C", h0...)
	r.remove("B", "C", h0...)
	gx := r.add("C", "X", h0...)
	rp := r.remove("C", "P", h0...)
	st := r.state(Final)
	wantReason(t, st, gx, ReasonAfterCut)
	wantApplied(t, st, rp)
	wantList(t, "members", st.Members, "A", "B")
	// A re-adds P knowingly. C tries again, on its old view (where its own
	// earlier removal of P is visible) and on the re-add (where its own
	// removal as admin is visible).
	back := r.add("A", "P", r.all()...)
	again := r.remove("C", "P", h0...)
	again2 := r.remove("C", "P", back)
	st = r.state(Final)
	wantApplied(t, st, back)
	wantReason(t, st, again, ReasonNotMember)
	wantReason(t, st, again2, ReasonNotAdmin)
	wantList(t, "members", st.Members, "A", "B", "P")
}

// Attack 7: two admins add the same machine at once; it is a member once.
//
// Requirements: T9
func TestTwoAdminsAddSameMachineAtOnce(t *testing.T) {
	r, h0 := threeAdmins(t)
	a := r.add("A", "D", h0...)
	b := r.add("B", "D", h0...)
	st := r.state(Final)
	wantApplied(t, st, a)
	wantApplied(t, st, b)
	wantList(t, "members", st.Members, "A", "B", "C", "D", "P")
	// Removing it needs to have seen both adds? No: one removal that saw
	// either add removes it, because a removal beats every add it did not
	// see as well as those it did, unless an add saw the removal.
	r.remove("C", "D", a)
	wantList(t, "members", r.state(Final).Members, "A", "B", "C", "P")
}

// Attack 7: an add and a removal of the same machine at once. The removal
// wins; an add made after seeing the removal re-admits.
//
// Requirements: T5, T9
func TestConcurrentAddAndRemoveOfSameMachineRemovalWins(t *testing.T) {
	r, h0 := threeAdmins(t)
	rm := r.remove("A", "P", h0...)
	add := r.add("B", "P", h0...) // already a member in B's view: no effect
	st := r.state(Final)
	wantApplied(t, st, rm)
	wantReason(t, st, add, ReasonAlreadyMember)
	wantList(t, "members", st.Members, "A", "B", "C")
	// D: added by B while A removes it, concurrently.
	r2, g0 := threeAdmins(t)
	addD := r2.add("B", "D", g0...)
	g1 := r2.all()
	rmD := r2.remove("A", "D", g1...)
	readd := r2.add("C", "D", addD) // C saw only the add: D already a member
	st = r2.state(Final)
	wantApplied(t, st, rmD)
	wantReason(t, st, readd, ReasonAlreadyMember)
	if st.members["D"] {
		t.Fatal("removal should win")
	}
	knowing := r2.add("C", "D", r2.all()...)
	st = r2.state(Final)
	wantApplied(t, st, knowing)
	if !st.members["D"] {
		t.Fatal("an add that saw the removal should re-admit")
	}
}

// Attack 7: two admins remove each other at once, with 2, 3 and 4 admins.
//
// Requirements: T9
func TestMutualRemovalWithTwoThreeAndFourAdmins(t *testing.T) {
	// Two admins: neither alone can remove the other. Nothing happens.
	r := newRoom(t, "A")
	r.admin("A", "B")
	h := r.all()
	x, y := r.remove("A", "B", h...), r.remove("B", "A", h...)
	st := r.state(Final)
	wantReason(t, st, x, ReasonNeedsSecond)
	wantReason(t, st, y, ReasonNeedsSecond)
	wantList(t, "admins", st.Admins, "A", "B")
	// Lead's rules: both are removed and the room has no admin.
	wantList(t, "lead admins", r.state(Lead).Admins)

	// Three admins: A and B vote each other out; no vote reaches two.
	r3, h3 := threeAdmins(t)
	r3.remove("A", "B", h3...)
	r3.remove("B", "A", h3...)
	wantList(t, "admins", r3.state(Final).Admins, "A", "B", "C")
	// C joins A's side: B is out. A's removal by B alone still lacks a
	// second vote.
	r3.remove("C", "B", h3...)
	wantList(t, "admins", r3.state(Final).Admins, "A", "C")

	// Four admins: A and B remove C while C and D remove A. Both take effect.
	r4, _ := threeAdmins(t)
	r4.admin("A", "D", r4.all()...)
	r4.admin("B", "D", r4.all()...)
	h4 := r4.all()
	r4.remove("A", "C", h4...)
	r4.remove("B", "C", h4...)
	r4.remove("C", "A", h4...)
	r4.remove("D", "A", h4...)
	st = r4.state(Final)
	wantList(t, "admins", st.Admins, "B", "D")
	wantList(t, "members", st.Members, "B", "D", "P")
}

// Attack 5: one lost admin out of three. The other two remove it and name
// a replacement, ending with three admins again.
//
// Requirements: T9, T10
func TestLostAdminOfThreeIsReplacedByTheOtherTwo(t *testing.T) {
	r, h0 := threeAdmins(t)
	r.remove("A", "C", h0...)
	r.remove("B", "C", r.all()...)
	r.admin("A", "D", r.all()...)
	r.admin("B", "D", r.all()...)
	st := r.state(Final)
	wantList(t, "admins", st.Admins, "A", "B", "D")
	wantList(t, "members", st.Members, "A", "B", "D", "P")
}

// Attack 5: one lost admin out of two. The other still adds and removes
// members, but cannot retire the lost key or name a new admin.
//
// Requirements: T9, T10
func TestLostAdminOfTwoLeavesMembersManageableAdminSetFixed(t *testing.T) {
	r := newRoom(t, "A")
	r.admin("A", "B")
	a := r.add("A", "D", r.all()...)
	rm := r.remove("A", "D", r.all()...)
	x := r.remove("A", "B", r.all()...)
	y := r.admin("A", "E", r.all()...)
	st := r.state(Final)
	wantApplied(t, st, a)
	wantApplied(t, st, rm)
	wantReason(t, st, x, ReasonNeedsSecond)
	wantReason(t, st, y, ReasonNeedsSecond)
	wantList(t, "admins", st.Admins, "A", "B")
}

// Attack 5: every admin lost. Nothing anyone else signs changes the member
// list, and nobody becomes an admin by waiting.
//
// Requirements: T10
func TestAllAdminsLostFreezesMembershipNobodyPromoted(t *testing.T) {
	r, h0 := threeAdmins(t)
	ids := []string{
		r.add("P", "Z", h0...),
		r.admin("P", "P", h0...),
		r.remove("P", "A", h0...),
	}
	st := r.state(Final)
	for _, id := range ids {
		wantReason(t, st, id, ReasonNotAdmin)
	}
	wantList(t, "admins", st.Admins, "A", "B", "C")
}

// A machine's vote on its own removal does not count. Otherwise a stolen
// key that is the target of a pending removal could complete it and widen
// what is kept of its own grants. Here D proposes removing A; A's own vote
// changes nothing.
//
// Requirements: T9
func TestOwnVoteOnOwnRemovalDoesNotCount(t *testing.T) {
	r := newRoom(t, "A")
	r.admin("A", "D")
	r.admin("D", "C", r.all()...)
	pending := r.remove("D", "A", r.all()...)
	vote := r.admin("A", "C", r.all()...)
	self := r.remove("A", "A", r.all()...)
	st := r.state(Final)
	wantReason(t, st, self, ReasonSelfVote)
	wantReason(t, st, pending, ReasonNeedsSecond)
	wantApplied(t, st, vote)
	wantList(t, "admins", st.Admins, "A", "C", "D")
}

// Limit (known gap 3): a removal waiting for a second admin can be
// completed by any other admin, a stolen key included. The removed admin's
// entries that the first signer had not seen stand only if the completing
// key had seen them, so the completing key decides their fate by what it
// claims to have seen. Here D proposes removing C. C then helps A make E
// an admin. An honest B that has seen everything keeps C's vote and E
// stays; a stolen B claiming not to have seen C's vote drops it, and E's
// promotion falls.
//
// Requirements: T9
func TestStolenKeyCompletingAPendingRemovalDecidesTheTargetsUnseenEntries(t *testing.T) {
	build := func() (*room, []string) {
		r, _ := threeAdmins(t)
		r.admin("A", "D", r.all()...)
		r.admin("B", "D", r.all()...)
		h0 := r.all()
		pending := r.remove("D", "C", h0...)
		r.admin("C", "E", h0...)
		r.admin("A", "E", r.all()...)
		return r, append(h0, pending)
	}
	r, _ := build()
	wantList(t, "admins", r.state(Final).Admins, "A", "B", "C", "D", "E")
	r.remove("B", "C", r.all()...)
	wantList(t, "honest completion", r.state(Final).Admins, "A", "B", "D", "E")
	r2, seenByD := build()
	r2.remove("B", "C", seenByD...)
	wantList(t, "stolen completion", r2.state(Final).Admins, "A", "B", "D")
}

// Limit (known gap 3, found by the deep seeded search): B removes member P
// while C votes to make P an admin; the removal wins and C's vote has no
// effect. A stolen A key signs a second vote claiming not to have seen the
// removal. The promotion is now complete and concurrent with the removal,
// so the removal is treated as removing an admin and waits for a second
// admin: P is an admin. The key needed another admin's vote to do it.
//
// Requirements: T9, T5
func TestStolenKeyCanCompleteAPromotionBeatenByAConcurrentRemoval(t *testing.T) {
	r, h0 := threeAdmins(t)
	rm := r.remove("B", "P", h0...)
	c := r.admin("C", "P", h0...)
	st := r.state(Final)
	wantApplied(t, st, rm)
	wantReason(t, st, c, ReasonRemovalWins)
	wantList(t, "members", st.Members, "A", "B", "C")
	r.admin("A", "P", append(h0, c)...)
	st = r.state(Final)
	wantReason(t, st, rm, ReasonNeedsSecond)
	wantList(t, "admins", st.Admins, "A", "B", "C", "P")
}

// Regression (review finding 1): an admin removed, added back, made an
// admin again and removed again. The second removal governs only what it
// signed since it came back; it must not void entries the first removal
// accepted, such as the admin grants of B and the addition of D. (The
// review's reproducer named an empty cut-off; cut-offs are now implied by
// what the removers had seen.)
//
// Requirements: T9, T5
func TestSecondRemovalOfReadmittedAdminDoesNotReopenEarlierPeriod(t *testing.T) {
	r := newRoom(t, "A")
	r.admin("A", "B")
	r.add("A", "D")
	r.admin("A", "C", r.all()...)
	r.admin("B", "C", r.all()...)
	h0 := r.all()
	r.remove("B", "A", h0...)
	r.remove("C", "A", h0...)
	h1 := r.all()
	r.add("B", "A", h1...)
	r.admin("B", "A", r.all()...)
	r.admin("C", "A", r.all()...)
	h2 := r.all()
	r.remove("B", "A", h2...)
	r.remove("C", "A", h2...)
	st := r.state(Final)
	wantList(t, "admins", st.Admins, "B", "C")
	wantList(t, "members", st.Members, "B", "C", "D")
}

// Regression (found by the deep seeded search): A and B remove C; B and D
// add C back and make it an admin again. A's key then signs another
// removal of C on its old view, where its own first vote was still
// waiting. That vote joins the old, completed removal; it must not count as
// a new removal that the re-promotion had not seen.
//
// Requirements: T9, T5
func TestLateVoteOnCompletedRemovalDoesNotUndoReadmission(t *testing.T) {
	r, _ := threeAdmins(t)
	r.admin("A", "D", r.all()...)
	r.admin("B", "D", r.all()...)
	h0 := r.all()
	first := r.remove("A", "C", h0...)
	r.remove("B", "C", h0...)
	r.add("B", "C", r.all()...)
	r.admin("B", "C", r.all()...)
	r.admin("D", "C", r.all()...)
	wantList(t, "admins", r.state(Final).Admins, "A", "B", "C", "D")
	r.remove("A", "C", append(h0, first)...)
	wantList(t, "admins", r.state(Final).Admins, "A", "B", "C", "D")
}

// Second review, finding 1: A and B remove C without having seen C's
// addition of E, so it has no effect. A vote that had seen the removal
// complete changes nothing. D signing a concurrent vote that claims to have
// seen E's addition take effect does bring E back: a removed admin's grant
// stands if some remover had seen it take effect. That gives D nothing it
// lacked, since D could add E itself.
//
// Requirements: T9, T5
func TestExtraRemovalVoteRevivesOnlyWhatItsSignerCouldGrantItself(t *testing.T) {
	build := func() (*room, []string, string) {
		r, _ := threeAdmins(t)
		r.admin("A", "D", r.all()...)
		r.admin("B", "D", r.all()...)
		h0 := r.all()
		addE := r.add("C", "E", h0...)
		r.remove("A", "C", h0...)
		r.remove("B", "C", h0...)
		return r, h0, addE
	}
	r, _, addE := build()
	wantReason(t, r.state(Final), addE, ReasonAfterCut)
	r.remove("D", "C", r.all()...) // late: had seen the removal complete
	wantReason(t, r.state(Final), addE, ReasonAfterCut)

	r2, h0, addE := build()
	r2.remove("D", "C", append(h0, addE)...)
	if !r2.state(Final).members["E"] {
		t.Fatal("expected the concurrent vote to keep E")
	}
}

// Found by the deep seeded search: B and D completed C's removal, D having
// seen C's vote that made D an admin. A stolen A key then signs another
// removal vote claiming not to have seen C's vote. D stays an admin: what a
// remover had seen take effect stands, whatever other votes claim.
//
// Requirements: T9
func TestExtraRemovalVoteCannotUndoACompletedPromotion(t *testing.T) {
	r, _ := threeAdmins(t)
	r.add("A", "D", r.all()...)
	r.admin("A", "D", r.all()...)
	before := r.all()
	vc := r.admin("C", "D", r.all()...)
	rb := r.remove("B", "C", before...)
	r.remove("D", "C", append(before, vc, rb)...)
	wantList(t, "admins", r.state(Final).Admins, "A", "B", "D")
	r.remove("A", "C", before...)
	wantList(t, "admins", r.state(Final).Admins, "A", "B", "D")
}

// One key cannot revive a removed admin's promotion vote and then give the
// second signature itself. C votes to make E an admin, unseen by A and B,
// who remove C. D joins the removal claiming to have seen C's vote, then
// votes for E. E is not an admin.
//
// Requirements: T9
func TestOneKeyCannotReviveAPromotionVoteAndSecondIt(t *testing.T) {
	r, _ := threeAdmins(t)
	r.admin("A", "D", r.all()...)
	r.admin("B", "D", r.all()...)
	h0 := r.all()
	vc := r.admin("C", "E", h0...)
	r.remove("A", "C", h0...)
	r.remove("B", "C", h0...)
	r.remove("D", "C", append(h0, vc)...)
	vd := r.admin("D", "E", append(h0, vc)...)
	st := r.state(Final)
	wantReason(t, st, vc, ReasonAfterCut)
	wantReason(t, st, vd, ReasonNeedsSecond)
	wantList(t, "admins", st.Admins, "A", "B", "D")
}

// Limit (known gap 4): C votes to make E an admin, unseen by A and B, who
// remove C; the vote is void. A stolen D key seconds C's vote, claiming
// not to have seen the removal, then joins the removal having seen its own
// second: E is an admin. No rule over the entries alone can refuse this:
// the same set arises when D seconds C's vote honestly, A and D remove C,
// and a stolen B key then joins the removal without having seen the
// second; there E must stay, or B's key alone would remove an admin. The
// attack needs C's signature from before the removal, like gap 3.
//
// Requirements: T9
func TestStolenKeyCanSecondAVoidedPromotionVoteAndThenJoinTheRemoval(t *testing.T) {
	r, _ := threeAdmins(t)
	r.admin("A", "D", r.all()...)
	r.admin("B", "D", r.all()...)
	h0 := r.all()
	vc := r.admin("C", "E", h0...)
	r.remove("A", "C", h0...)
	r.remove("B", "C", h0...)
	wantReason(t, r.state(Final), vc, ReasonAfterCut)
	vd := r.admin("D", "E", append(h0, vc)...)
	r.remove("D", "C", append(h0, vc, vd)...)
	if !r.state(Final).admins["E"] {
		t.Fatal("the limit no longer holds: update known gap 4 and atRest")
	}
	t.Skip("KNOWN GAP 4: one key can second a voided promotion vote and then revive it by joining the removal")
}

// A remover that had seen a grant take effect keeps it, whatever other
// removers claim: here A saw C's addition of E, B did not, and D joins
// claiming not to have seen it. E stays.
//
// Requirements: T9
func TestExtraRemovalVoteCannotDropWhatARemoverSawTakeEffect(t *testing.T) {
	r, _ := threeAdmins(t)
	r.admin("A", "D", r.all()...)
	r.admin("B", "D", r.all()...)
	h0 := r.all()
	addE := r.add("C", "E", h0...)
	r.remove("A", "C", append(h0, addE)...)
	r.remove("B", "C", h0...)
	r.remove("D", "C", h0...)
	if !r.state(Final).members["E"] {
		t.Fatal("D's vote dropped an entry A had seen take effect")
	}
}

// The converse: a grant voided by the first removal (its removers had not
// seen it) is not revived by a second removal whose removers have seen it.
//
// Requirements: T9, T5
func TestSecondRemovalDoesNotReviveGrantVoidedByFirst(t *testing.T) {
	r, h0 := threeAdmins(t)
	bad := r.add("A", "X", h0...)
	r.remove("B", "A", h0...)
	r.remove("C", "A", h0...)
	if r.state(Final).members["X"] {
		t.Fatal("first removal should void A's addition of X")
	}
	r.add("B", "A", r.all()...)
	r.admin("B", "A", r.all()...)
	r.admin("C", "A", r.all()...)
	again := r.add("A", "Y", r.all()...)
	r.remove("B", "A", r.all()...) // these removers have seen bad
	r.remove("C", "A", r.all()...)
	st := r.state(Final)
	if st.members["X"] {
		t.Fatal("second removal revived a grant the first had voided")
	}
	wantApplied(t, st, again)
	wantReason(t, st, bad, ReasonAfterCut)
	wantList(t, "members", st.Members, "B", "C", "P", "Y")
}

// The default "the first three machines are admins": the creator alone
// makes the second machine an admin, because it is then the only admin.
// The third needs matching entries from both, so a person confirms on two
// admin machines (and on the joining machine, T3).
//
// Requirements: T9, T3
func TestFirstThreeMachinesBecomeAdminsWithTwoAdminConfirmations(t *testing.T) {
	r := newRoom(t, "A")
	b := r.admin("A", "B")
	c1 := r.admin("A", "C", r.all()...)
	st := r.state(Final)
	wantApplied(t, st, b)
	wantReason(t, st, c1, ReasonNeedsSecond)
	c2 := r.admin("B", "C", r.all()...)
	st = r.state(Final)
	wantApplied(t, st, c1)
	wantApplied(t, st, c2)
	wantList(t, "admins", st.Admins, "A", "B", "C")
}
